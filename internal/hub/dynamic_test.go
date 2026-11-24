package hub

import (
	"encoding/json"
	"fmt"
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
