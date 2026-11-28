package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/httpapi"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
	"github.com/MiguelSamayoa/calisto-panel/internal/winutil"
)

// ---- VM ----

func TestVMNameAndArgv(t *testing.T) {
	if got := vmName(`C:\Users\x\Virtual Machines\MI_VM\MI_VM.vmx`); got != "MI_VM" {
		t.Errorf("vmName = %q", got)
	}
	want := []string{`C:\VMware\vmrun.exe`, "-T", "ws", "list"}
	if got := vmListArgv(`C:\VMware\vmrun.exe`); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestVMStateFromList(t *testing.T) {
	vmx := `C:\VMs\LAB\LAB.vmx`
	none := func(string) bool { return false }
	vmss := func(p string) bool { return p == `C:\VMs\LAB\LAB.vmss` }

	running := parseVMList([]byte("Total running VMs: 2\r\nc:\\vms\\lab\\lab.VMX\r\nC:\\other\\x.vmx\r\n"))
	if len(running) != 2 {
		t.Fatalf("parsed %v", running)
	}
	for _, tc := range []struct {
		name    string
		running []string
		exists  func(string) bool
		want    string
	}{
		{"listed, case and slashes ignored", running, none, "running"},
		{"not listed, no state file", parseVMList([]byte("Total running VMs: 0\n")), none, "off"},
		{"not listed, suspend file present", nil, vmss, "suspended"},
		{"another VM running", parseVMList([]byte("Total running VMs: 1\nC:\\other\\x.vmx\n")), none, "off"},
	} {
		if got := vmState(tc.running, vmx, tc.exists); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---- logs ----

func TestParseLogsStripsColorAndKeepsContinuations(t *testing.T) {
	out := []byte("2026-11-24T10:00:00.500000000Z \x1b[32mready\x1b[0m\r\n" +
		"stack line without timestamp\n" +
		"\n" +
		"2026-11-24T10:00:01.000000000Z done\n")
	got := parseLogs("aaaaaaaaaaaa", out)
	if len(got) != 3 {
		t.Fatalf("lines = %+v", got)
	}
	if got[0].M != "ready" || got[0].C != "aaaaaaaaaaaa" {
		t.Errorf("first = %+v, want the colour codes stripped", got[0])
	}
	if got[1].T != got[0].T || got[1].M != "stack line without timestamp" {
		t.Errorf("continuation = %+v, want the previous timestamp", got[1])
	}
	if got[2].T-got[0].T != 500 {
		t.Errorf("timestamps not parsed: %+v", got)
	}
}

func TestMergeLogsOrdersByTimeAndKeepsTheNewest(t *testing.T) {
	a := []logLine{{T: 1, C: "a", M: "a1"}, {T: 4, C: "a", M: "a4"}}
	b := []logLine{{T: 2, C: "b", M: "b2"}, {T: 3, C: "b", M: "b3"}}
	got := mergeLogs([][]logLine{a, b}, 3)
	var msgs []string
	for _, l := range got {
		msgs = append(msgs, l.M)
	}
	if !reflect.DeepEqual(msgs, []string{"b2", "b3", "a4"}) {
		t.Errorf("merged = %v, want the newest three in time order", msgs)
	}
}

func logsHub(t *testing.T) *hub.Hub {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server = config.Server{StaleMs: 10000, DeadMs: 60000, SSEKeepaliveMs: 15000}
	cfg.Sensors.IntervalMs = 5000
	cfg.CI.IntervalMs = 60000
	cfg.Actions.Docker.Enabled = true
	h, err := hub.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.UpdateDocker(hub.DockerUpdate{Engine: true, Containers: []hub.Container{
		{ID: "aaaaaaaaaaaa", Project: "forms-dev", Name: "forms-dev-web-1", State: "running"},
		{ID: "bbbbbbbbbbbb", Project: "forms-dev", Name: "forms-dev-db-1", State: "running"},
		{ID: "cccccccccccc", Project: "other", Name: "other-x-1", State: "running"},
	}})
	return h
}

func TestLogsFuncReadsOnlyTheProjectsOwnContainers(t *testing.T) {
	var argvs [][]string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		argvs = append(argvs, argv)
		id := argv[len(argv)-1]
		return []byte("2026-11-24T10:00:0" + map[string]string{"aaaaaaaaaaaa": "2", "bbbbbbbbbbbb": "1"}[id] + ".000000000Z hello from " + id + "\n"), nil
	}
	logs := newLogsFunc(logsHub(t), run)

	body, err := logs(context.Background(), httpapi.LogsProject, "forms-dev")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Name       string      `json:"name"`
		Containers []logMember `json:"containers"`
		Lines      []logLine   `json:"lines"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "forms-dev" || len(got.Containers) != 2 || len(got.Lines) != 2 {
		t.Fatalf("response = %s", body)
	}
	if got.Lines[0].C != "bbbbbbbbbbbb" || got.Lines[1].C != "aaaaaaaaaaaa" {
		t.Errorf("lines not in time order: %+v", got.Lines)
	}
	for _, a := range argvs { // fixed argv; the only variable part is a listed id
		if len(a) != 6 || a[0] != "docker" || a[1] != "logs" || (a[5] != "aaaaaaaaaaaa" && a[5] != "bbbbbbbbbbbb") {
			t.Errorf("unexpected argv %q", a)
		}
	}
	if len(argvs) != 2 {
		t.Errorf("ran %d docker commands, want 2 (nothing from project 'other')", len(argvs))
	}
}

func TestLogsFuncRejectsUnknownAndMalformedProjects(t *testing.T) {
	ran := false
	logs := newLogsFunc(logsHub(t), func(context.Context, []string) ([]byte, error) { ran = true; return nil, nil })
	for _, p := range []string{"nope", "", "../x", "forms dev", "FORMS-DEV"} {
		if _, err := logs(context.Background(), httpapi.LogsProject, p); !errors.Is(err, httpapi.ErrNotFound) {
			t.Errorf("project %q: err = %v, want ErrNotFound", p, err)
		}
	}
	if ran {
		t.Error("docker ran for a project the server does not know")
	}
}

func TestLogsFuncSurvivesAContainerThatFails(t *testing.T) {
	run := func(_ context.Context, argv []string) ([]byte, error) {
		if argv[len(argv)-1] == "aaaaaaaaaaaa" {
			return nil, errors.New("no such container")
		}
		return []byte("2026-11-24T10:00:01.000000000Z alive\n"), nil
	}
	body, err := newLogsFunc(logsHub(t), run)(context.Background(), httpapi.LogsProject, "forms-dev")
	if err != nil || !strings.Contains(string(body), "alive") {
		t.Errorf("body = %s, err = %v; one failing container must not sink the view", body, err)
	}
}

func TestLogsFuncOfOneContainer(t *testing.T) {
	var argvs [][]string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		argvs = append(argvs, argv)
		return []byte("2026-11-24T10:00:01.000000000Z only mine\n"), nil
	}
	logs := newLogsFunc(logsHub(t), run)

	body, err := logs(context.Background(), httpapi.LogsContainer, "bbbbbbbbbbbb")
	if err != nil || !strings.Contains(string(body), "only mine") {
		t.Fatalf("body = %s, err = %v", body, err)
	}
	if len(argvs) != 1 || argvs[0][len(argvs[0])-1] != "bbbbbbbbbbbb" {
		t.Errorf("argvs = %v, want one docker logs for bbbbbbbbbbbb", argvs)
	}

	argvs = nil
	for _, id := range []string{"ffffffffffff", "", "--rm", "BBBBBBBBBBBB", "bbbbbbbbbbb", "bbbbbbbbbbbb --since 1", "forms-dev"} {
		if _, err := logs(context.Background(), httpapi.LogsContainer, id); !errors.Is(err, httpapi.ErrNotFound) {
			t.Errorf("container %q: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := logs(context.Background(), "nonsense", "bbbbbbbbbbbb"); !errors.Is(err, httpapi.ErrNotFound) {
		t.Errorf("an unknown kind must be ErrNotFound, got %v", err)
	}
	if len(argvs) != 0 {
		t.Errorf("docker ran for targets the server does not know: %v", argvs)
	}
}

// ---- media ----

func TestMediaUpdateMapping(t *testing.T) {
	u := mediaUpdate(winutil.MediaInfo{Active: true, Status: "playing", App: "Chrome", Title: "T", Artist: "A", Album: "L", Position: 90 * time.Second, Duration: 5 * time.Minute})
	want := hub.MediaUpdate{Active: true, Status: "playing", App: "Chrome", SID: winutil.SessionID("Chrome"), Title: "T", Artist: "A", Album: "L", Position: 90, Duration: 300}
	if !reflect.DeepEqual(u, want) {
		t.Errorf("mediaUpdate = %+v, want %+v", u, want)
	}
	if got := mediaUpdate(winutil.MediaInfo{}); got.Active || got.Title != "" {
		t.Errorf("no session = %+v", got)
	}
}

func TestPollMediaRereadsAfterAPoke(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server = config.Server{StaleMs: 10000, DeadMs: 60000, SSEKeepaliveMs: 15000}
	cfg.Sensors.IntervalMs = 5000
	cfg.CI.IntervalMs = 60000
	h, err := hub.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	reads := make(chan struct{}, 16)
	read := func(context.Context) (winutil.MediaInfo, error) {
		reads <- struct{}{}
		return winutil.MediaInfo{Active: true, Status: "playing"}, nil
	}
	poke := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A long interval: any read after the first one can only come from the poke.
		pollMedia(ctx, config.Media{IntervalMs: 60000}, read, nil, nil, nil, poke, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() { cancel(); <-done }()

	waitRead := func(what string) {
		t.Helper()
		select {
		case <-reads:
		case <-time.After(3 * time.Second):
			t.Fatalf("no read: %s", what)
		}
	}
	waitRead("initial")
	poke <- struct{}{}
	waitRead("after the poke")
	waitRead("second read after the poke")
}

func TestPollMediaReportsAFailureAsErrorState(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server = config.Server{StaleMs: 10000, DeadMs: 60000, SSEKeepaliveMs: 15000}
	cfg.Sensors.IntervalMs = 5000
	cfg.CI.IntervalMs = 60000
	h, err := hub.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 4)
	read := func(context.Context) (winutil.MediaInfo, error) {
		called <- struct{}{}
		return winutil.MediaInfo{}, errors.New("winrt down")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollMedia(ctx, config.Media{IntervalMs: 60000}, read, nil, nil, nil, make(chan struct{}), h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("never read")
	}
	cancel()
	<-done
}
