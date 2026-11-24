package hub

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProcessesEvent(t *testing.T) {
	h, now := fakeClock(t, testConfig())
	s := h.subscribe()
	for range 3 { // hello, sensors, ci
		<-s.ch
	}
	if p := string(<-s.ch); !strings.Contains(p, `"state":"unavailable"`) || !strings.Contains(p, `"items":[]`) {
		t.Errorf("before the first poll: %s", p)
	}

	h.UpdateProcesses(ProcessesUpdate{Items: []ProcessUpdate{{ID: "chrome.exe", Label: "chrome.exe", CPU: 12.5, Mem: 2e9, Count: 30}}, MemTotal: 16e9})
	var got struct {
		State    string  `json:"state"`
		MemTotal float64 `json:"memTotal"`
		Items    []struct {
			ID    string  `json:"id"`
			CPU   float64 `json:"cpu"`
			Mem   float64 `json:"mem"`
			Count int     `json:"count"`
		} `json:"items"`
	}
	mustDecodeFrame(t, <-s.ch, "processes", &got)
	if got.State != StateOK || got.MemTotal != 16e9 || len(got.Items) != 1 || got.Items[0].CPU != 12.5 || got.Items[0].Mem != 2e9 || got.Items[0].Count != 30 {
		t.Errorf("processes = %+v", got)
	}

	*now = now.Add(15 * time.Second)
	h.UpdateProcesses(ProcessesUpdate{Fault: StateError}) // snapshot failed: list kept, state says why
	mustDecodeFrame(t, <-s.ch, "processes", &got)
	if got.State != StateError || len(got.Items) != 1 {
		t.Errorf("after failure = %+v, want error state with the last list", got)
	}
}

func TestDockerEventAndHasContainer(t *testing.T) {
	cfg := testConfig()
	cfg.Actions.Docker.Enabled = true
	h, _ := fakeClock(t, cfg)

	if h.HasContainer("aaaaaaaaaaaa") {
		t.Error("no container is known before the first poll")
	}
	cpu := 1.5
	h.UpdateDocker(DockerUpdate{Engine: true, Containers: []Container{{ID: "aaaaaaaaaaaa", Name: "web", State: "running", CPU: &cpu}, {ID: "bbbbbbbbbbbb", Name: "db", State: "exited"}}})
	if !h.HasContainer("aaaaaaaaaaaa") || !h.HasContainer("bbbbbbbbbbbb") || h.HasContainer("cccccccccccc") {
		t.Error("HasContainer must match exactly the ids of the last poll")
	}

	s := h.subscribe()
	var hello map[string]any
	mustDecodeFrame(t, <-s.ch, "hello", &hello)
	if d, _ := hello["dynamicActions"].([]any); len(d) != 4 {
		t.Errorf("hello dynamicActions = %v, want the container and project families", hello["dynamicActions"])
	}
	// testConfig lists only docker.stop; the dynamic destructive ones are always added.
	conf := fmt.Sprint(hello["confirmRequired"])
	if !strings.Contains(conf, "container.stop") || !strings.Contains(conf, "project.stop") || !strings.Contains(conf, "docker.stop") {
		t.Errorf("hello confirmRequired = %s", conf)
	}
	<-s.ch
	<-s.ch
	<-s.ch // sensors, ci, processes
	var got struct {
		Engine bool   `json:"engine"`
		State  string `json:"state"`
		Items  []struct {
			ID  string   `json:"id"`
			CPU *float64 `json:"cpu"`
		} `json:"items"`
	}
	mustDecodeFrame(t, <-s.ch, "docker", &got)
	if !got.Engine || got.State != StateOK || len(got.Items) != 2 || got.Items[0].CPU == nil || got.Items[1].CPU != nil {
		t.Errorf("docker = %+v", got)
	}

	h.UpdateDocker(DockerUpdate{Engine: false}) // Docker Desktop stopped
	if h.HasContainer("aaaaaaaaaaaa") {
		t.Error("with the engine down no container may be actionable")
	}
}

func TestDockerOffEmitsNoDockerEvent(t *testing.T) {
	h, _ := fakeClock(t, testConfig())
	s := h.subscribe()
	for i := 0; i < 4; i++ {
		<-s.ch
	}
	select {
	case f := <-s.ch:
		t.Errorf("unexpected frame %s", f)
	default:
	}
}

// mustDecodeFrame checks an SSE frame is the wanted event and decodes its data.
func mustDecodeFrame(t *testing.T, f []byte, event string, into any) {
	t.Helper()
	var name, data string
	for _, line := range strings.Split(string(f), "\n") {
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			name = v
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			data = v
		}
	}
	if name != event {
		t.Fatalf("event = %q, want %q", name, event)
	}
	if err := json.Unmarshal([]byte(data), into); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
}

func TestProjectContainers(t *testing.T) {
	cfg := testConfig()
	cfg.Actions.Docker.Enabled = true
	h, _ := fakeClock(t, cfg)
	h.UpdateDocker(DockerUpdate{Engine: true, Containers: []Container{
		{ID: "aaaaaaaaaaaa", Project: "forms-dev", State: "running"},
		{ID: "bbbbbbbbbbbb", Project: "forms-dev", State: "exited"},
		{ID: "cccccccccccc", Project: "other"},
		{ID: "dddddddddddd"}, // standalone
	}})
	if got := h.ProjectContainers("forms-dev"); len(got) != 2 || got[0] != "aaaaaaaaaaaa" || got[1] != "bbbbbbbbbbbb" {
		t.Errorf("forms-dev = %v", got)
	}
	if h.ProjectContainers("") != nil || h.ProjectContainers("nope") != nil {
		t.Error("an empty or unknown project has no containers; standalone ones never match")
	}
	h.UpdateDocker(DockerUpdate{Engine: false})
	if h.ProjectContainers("forms-dev") != nil {
		t.Error("with the engine down no project is actionable")
	}
}

func TestValidProject(t *testing.T) {
	for _, ok := range []string{"forms-dev", "a", "my_project2"} {
		if !ValidProject(ok) {
			t.Errorf("ValidProject(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "-x", "Forms", "a b", "a/b", "a..b", "a;rm", strings.Repeat("a", 64)} {
		if ValidProject(bad) {
			t.Errorf("ValidProject(%q) = true", bad)
		}
	}
}

func TestMediaAndVMEvents(t *testing.T) {
	cfg := testConfig()
	cfg.Actions.Media.Enabled = true
	cfg.Actions.VMware.Enabled = true
	h, _ := fakeClock(t, cfg)

	s := h.subscribe()
	for range 4 { // hello, sensors, ci, processes
		<-s.ch
	}
	var med struct {
		State  string `json:"state"`
		Active bool   `json:"active"`
	}
	mustDecodeFrame(t, <-s.ch, "media", &med)
	if med.State != StateUnavailable || med.Active {
		t.Errorf("before the first poll: %+v", med)
	}
	var vm struct {
		State string `json:"state"`
	}
	mustDecodeFrame(t, <-s.ch, "vm", &vm)
	if vm.State != StateUnavailable {
		t.Errorf("vm before the first poll: %+v", vm)
	}

	h.UpdateMedia(MediaUpdate{Active: true, Status: "playing", App: "Spotify", Title: "Song", Artist: "Band", Position: 12.5, Duration: 200})
	var m struct {
		State    string  `json:"state"`
		Active   bool    `json:"active"`
		Status   string  `json:"status"`
		Title    string  `json:"title"`
		Artist   string  `json:"artist"`
		Position float64 `json:"position"`
		Duration float64 `json:"duration"`
	}
	mustDecodeFrame(t, <-s.ch, "media", &m)
	if m.State != StateOK || !m.Active || m.Status != "playing" || m.Title != "Song" || m.Artist != "Band" || m.Position != 12.5 || m.Duration != 200 {
		t.Errorf("media = %+v", m)
	}

	h.UpdateVM(VMUpdate{Name: "MI_VM", State: "running", CPU: 7.5, Mem: 4e9, Running: 1})
	var v struct {
		State   string  `json:"state"`
		Name    string  `json:"name"`
		VM      string  `json:"vm"`
		CPU     float64 `json:"cpu"`
		Mem     float64 `json:"mem"`
		Running int     `json:"running"`
	}
	mustDecodeFrame(t, <-s.ch, "vm", &v)
	if v.State != StateOK || v.Name != "MI_VM" || v.VM != "running" || v.CPU != 7.5 || v.Mem != 4e9 || v.Running != 1 {
		t.Errorf("vm = %+v", v)
	}

	h.UpdateMedia(MediaUpdate{Fault: StateError}) // reading failed: keep the last data, say why
	mustDecodeFrame(t, <-s.ch, "media", &m)
	if m.State != StateError {
		t.Errorf("after a failed read: %+v", m)
	}
}

func TestMediaAndVMOffEmitNothing(t *testing.T) {
	h, _ := fakeClock(t, testConfig())
	s := h.subscribe()
	for range 4 {
		<-s.ch
	}
	select {
	case f := <-s.ch:
		t.Errorf("unexpected frame %s", f)
	default:
	}
}

func TestVMStopAlwaysNeedsConfirmation(t *testing.T) {
	cfg := testConfig() // confirmRequired lists only docker.stop
	cfg.Actions.VMware.Enabled = true
	h, _ := fakeClock(t, cfg)
	s := h.subscribe()
	var hello struct {
		Confirm []string `json:"confirmRequired"`
	}
	mustDecodeFrame(t, <-s.ch, "hello", &hello)
	if !slices.Contains(hello.Confirm, "vm.stop") {
		t.Errorf("confirmRequired = %v, want vm.stop added", hello.Confirm)
	}
	if len(cfg.Actions.ConfirmRequired) != 1 {
		t.Errorf("the config slice was modified: %v", cfg.Actions.ConfirmRequired)
	}
}

func TestContainerInfo(t *testing.T) {
	cfg := testConfig()
	cfg.Actions.Docker.Enabled = true
	h, _ := fakeClock(t, cfg)
	h.UpdateDocker(DockerUpdate{Engine: true, Containers: []Container{{ID: "aaaaaaaaaaaa", Name: "web", Project: "p"}}})
	if c, ok := h.ContainerInfo("aaaaaaaaaaaa"); !ok || c.Name != "web" {
		t.Errorf("ContainerInfo = %+v, %v", c, ok)
	}
	if _, ok := h.ContainerInfo("bbbbbbbbbbbb"); ok {
		t.Error("an unlisted container must not be found")
	}
}
