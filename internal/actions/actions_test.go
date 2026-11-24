package actions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
)

func testCfg() config.Actions {
	return config.Actions{
		TimeoutMs:   60000,
		RateLimitMs: 2000,
		Docker:      config.Docker{Enabled: true},
		VMware:      config.VMware{Enabled: true, VmrunPath: `C:\VMware\vmrun.exe`, VMX: `C:\VMs\LAB\LAB.vmx`},
		Media:       config.Media{Enabled: true, PlayPauseVK: 0xB3, NextVK: 0xB0},
	}
}

// harness records what the executor does instead of doing it.
type harness struct {
	mu      sync.Mutex
	argvs   [][]string
	keys    []uint8
	reports []hub.ActionUpdate
	block   chan struct{} // when non-nil, Run waits for it or ctx
	runErr  error
	now     time.Time
	known   map[string]bool     // container ids the fake hub has listed
	project map[string][]string // compose project -> container ids
	ex      *Executor
	done    chan struct{}
}

func newHarness(t *testing.T, cfg config.Actions) *harness {
	t.Helper()
	h := &harness{now: time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC), done: make(chan struct{}, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.ex.Wait() })
	h.ex = New(ctx, cfg, Deps{
		Run: func(ctx context.Context, argv []string) error {
			h.mu.Lock()
			h.argvs = append(h.argvs, argv)
			block, err := h.block, h.runErr
			h.mu.Unlock()
			if block != nil {
				select {
				case <-block:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return err
		},
		Keys: func(vk uint8) error {
			h.mu.Lock()
			h.keys = append(h.keys, vk)
			h.mu.Unlock()
			return nil
		},
		Report: func(u hub.ActionUpdate) {
			h.mu.Lock()
			h.reports = append(h.reports, u)
			h.mu.Unlock()
			if u.State != hub.ActionRunning {
				h.done <- struct{}{}
			}
		},
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		HasContainer:      func(id string) bool { return h.known[id] },
		ProjectContainers: func(name string) []string { return h.project[name] },
	})
	h.ex.now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
	return h
}

func (h *harness) advance(d time.Duration) { h.mu.Lock(); h.now = h.now.Add(d); h.mu.Unlock() }

func (h *harness) waitDone(t *testing.T) hub.ActionUpdate {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("action did not finish")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reports[len(h.reports)-1]
}

func TestWhitelistAndArgvBuiltFromConfig(t *testing.T) {
	h := newHarness(t, testCfg())
	wantIDs := []string{"docker.start", "docker.stop", "vm.suspend", "vm.start", "media.playpause", "media.next"}
	if got := h.ex.IDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("IDs = %v, want %v", got, wantIDs)
	}

	wantArgv := map[string][]string{
		"docker.start": {"docker", "desktop", "start"},
		"docker.stop":  {"docker", "desktop", "stop"},
		"vm.suspend":   {`C:\VMware\vmrun.exe`, "-T", "ws", "suspend", `C:\VMs\LAB\LAB.vmx`, "soft"},
		"vm.start":     {`C:\VMware\vmrun.exe`, "-T", "ws", "start", `C:\VMs\LAB\LAB.vmx`, "nogui"},
	}
	for id, want := range wantArgv {
		if got := h.ex.specs[id].argv; !reflect.DeepEqual(got, want) {
			t.Errorf("%s argv = %q, want %q", id, got, want)
		}
	}
	if h.ex.specs["media.playpause"].vk != 0xB3 || h.ex.specs["media.next"].vk != 0xB0 {
		t.Errorf("media keys = %+v %+v", h.ex.specs["media.playpause"], h.ex.specs["media.next"])
	}
}

func TestDisabledGroupsAreNotRegistered(t *testing.T) {
	cfg := testCfg()
	cfg.VMware.Enabled = false
	h := newHarness(t, cfg)
	if _, err := h.ex.Do("vm.suspend"); !errors.Is(err, ErrUnknown) {
		t.Errorf("vm.suspend with vmware disabled: err = %v, want ErrUnknown", err)
	}
}

func TestUnknownIDsRejectedWithoutRunningAnything(t *testing.T) {
	h := newHarness(t, testCfg())
	for _, id := range []string{"", "docker.restart", "DOCKER.START", " docker.start", "docker.start && calc", "../vm.start", "vm.stop", "stop hard"} {
		if _, err := h.ex.Do(id); !errors.Is(err, ErrUnknown) {
			t.Errorf("Do(%q) err = %v, want ErrUnknown", id, err)
		}
	}
	if len(h.argvs) != 0 || len(h.keys) != 0 || len(h.reports) != 0 {
		t.Errorf("rejected ids had side effects: %v %v %v", h.argvs, h.keys, h.reports)
	}
}

func TestDoRunsExactArgvAndReports(t *testing.T) {
	h := newHarness(t, testCfg())
	st, err := h.ex.Do("vm.suspend")
	if err != nil {
		t.Fatal(err)
	}
	if st.Action != "vm.suspend" || st.State != hub.ActionRunning || len(st.Exec) != 10 {
		t.Errorf("Started = %+v", st)
	}
	final := h.waitDone(t)
	if final.State != hub.ActionOK || final.Exec != st.Exec {
		t.Errorf("final = %+v", final)
	}
	want := []string{`C:\VMware\vmrun.exe`, "-T", "ws", "suspend", `C:\VMs\LAB\LAB.vmx`, "soft"}
	if !reflect.DeepEqual(h.argvs, [][]string{want}) {
		t.Errorf("argv = %q, want %q", h.argvs, want)
	}
	if h.reports[0].State != hub.ActionRunning {
		t.Errorf("first report = %+v, want running", h.reports[0])
	}
}

func TestMediaKeySendsConfiguredVK(t *testing.T) {
	h := newHarness(t, testCfg())
	if _, err := h.ex.Do("media.next"); err != nil {
		t.Fatal(err)
	}
	h.waitDone(t)
	if !reflect.DeepEqual(h.keys, []uint8{0xB0}) || len(h.argvs) != 0 {
		t.Errorf("keys = %v argvs = %v", h.keys, h.argvs)
	}
}

func TestFailureAndTimeout(t *testing.T) {
	h := newHarness(t, testCfg())
	h.runErr = errors.New("running docker: exit status 1")
	if _, err := h.ex.Do("docker.stop"); err != nil {
		t.Fatal(err)
	}
	if f := h.waitDone(t); f.State != hub.ActionFailed || f.Message != "running docker: exit status 1" {
		t.Errorf("failure = %+v", f)
	}

	cfg := testCfg()
	cfg.TimeoutMs = 20
	h = newHarness(t, cfg)
	h.block = make(chan struct{}) // never released: only the timeout ends it
	if _, err := h.ex.Do("docker.stop"); err != nil {
		t.Fatal(err)
	}
	if f := h.waitDone(t); f.State != hub.ActionTimeout {
		t.Errorf("timeout = %+v", f)
	}
}

func TestAlreadyRunningThenRateLimited(t *testing.T) {
	h := newHarness(t, testCfg())
	h.block = make(chan struct{})
	first, err := h.ex.Do("docker.stop")
	if err != nil {
		t.Fatal(err)
	}

	// Same id while running: 409 with the running exec, even inside the rate window.
	var busy *AlreadyRunningError
	if _, err := h.ex.Do("docker.stop"); !errors.As(err, &busy) || busy.Exec != first.Exec {
		t.Errorf("err = %v, want AlreadyRunning(%s)", err, first.Exec)
	}

	// A different id within 2 s: 429 with the remaining wait.
	h.advance(500 * time.Millisecond)
	var limited *RateLimitedError
	if _, err := h.ex.Do("media.next"); !errors.As(err, &limited) || limited.RetryAfter != 1500*time.Millisecond {
		t.Errorf("err = %v, want RateLimited(1.5s)", err)
	}

	// Rejected calls must not have consumed the window.
	h.advance(1500 * time.Millisecond)
	if _, err := h.ex.Do("media.next"); err != nil {
		t.Errorf("after the window: %v", err)
	}

	close(h.block)
	h.waitDone(t)
	h.waitDone(t)
	h.advance(2 * time.Second)
	if _, err := h.ex.Do("docker.stop"); err != nil {
		t.Errorf("same id after it finished: %v", err)
	}
}

func TestShutdownCancelsAndWaits(t *testing.T) {
	h := newHarness(t, testCfg())
	h.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	h.ex.base = ctx
	if _, err := h.ex.Do("docker.start"); err != nil {
		t.Fatal(err)
	}
	cancel()
	h.ex.Wait() // returns only once the goroutine reported
	if f := h.waitDone(t); f.State != hub.ActionFailed {
		t.Errorf("canceled action = %+v, want failed", f)
	}
}

func TestExecID(t *testing.T) {
	a := newExecID(time.UnixMilli(1_700_000_000_000))
	b := newExecID(time.UnixMilli(1_700_000_002_000))
	if len(a) != 10 || a >= b {
		t.Errorf("ids %q %q: want 10 chars and increasing", a, b)
	}
}

func TestContainerActionsOnlyForObservedIDs(t *testing.T) {
	h := newHarness(t, testCfg())
	h.known = map[string]bool{"abcdef012345": true}

	for _, id := range []string{
		"container.start.000000000000",     // well-formed but never listed
		"container.stop.abcdef01234",       // 11 digits
		"container.stop.abcdef0123456",     // 13 digits
		"container.stop.ABCDEF012345",      // upper case
		"container.stop.--rm",              // would look like a flag
		"container.stop.abcdef012345 --rm", // argument smuggling
		"container.restart.abcdef012345",   // verb outside the whitelist
		"container.rm.abcdef012345",
		"container.start.",
	} {
		if _, err := h.ex.Do(id); !errors.Is(err, ErrUnknown) {
			t.Errorf("Do(%q) err = %v, want ErrUnknown", id, err)
		}
	}
	if len(h.argvs) != 0 || len(h.reports) != 0 {
		t.Fatalf("rejected ids had side effects: %v %v", h.argvs, h.reports)
	}

	for verb, wantArgv := range map[string][]string{
		"start": {"docker", "start", "abcdef012345"},
		"stop":  {"docker", "stop", "abcdef012345"},
	} {
		h.advance(3 * time.Second) // stay clear of the global rate limit
		st, err := h.ex.Do("container." + verb + ".abcdef012345")
		if err != nil || st.Action != "container."+verb+".abcdef012345" {
			t.Fatalf("%s: %+v, %v", verb, st, err)
		}
		h.waitDone(t)
		h.mu.Lock()
		got := h.argvs[len(h.argvs)-1]
		h.mu.Unlock()
		if !reflect.DeepEqual(got, wantArgv) {
			t.Errorf("%s argv = %q, want %q", verb, got, wantArgv)
		}
	}
}

func TestContainerActionsNeedDockerEnabled(t *testing.T) {
	cfg := testCfg()
	cfg.Docker.Enabled = false
	h := newHarness(t, cfg)
	h.known = map[string]bool{"abcdef012345": true}
	if _, err := h.ex.Do("container.stop.abcdef012345"); !errors.Is(err, ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown with docker disabled", err)
	}
}

func TestContainerActionIsExclusivePerContainer(t *testing.T) {
	h := newHarness(t, testCfg())
	h.known = map[string]bool{"abcdef012345": true, "fedcba543210": true}
	h.block = make(chan struct{})
	if _, err := h.ex.Do("container.stop.abcdef012345"); err != nil {
		t.Fatal(err)
	}
	h.advance(3 * time.Second)
	var busy *AlreadyRunningError
	if _, err := h.ex.Do("container.stop.abcdef012345"); !errors.As(err, &busy) {
		t.Errorf("same container twice: err = %v, want AlreadyRunningError", err)
	}
	if _, err := h.ex.Do("container.stop.fedcba543210"); err != nil {
		t.Errorf("another container must not be blocked: %v", err)
	}
	close(h.block)
	h.waitDone(t)
	h.waitDone(t)
}

func TestProjectActionsUseObservedContainerIDs(t *testing.T) {
	h := newHarness(t, testCfg())
	h.project = map[string][]string{"forms-dev": {"aaaaaaaaaaaa", "bbbbbbbbbbbb"}}

	for _, id := range []string{
		"project.start.unknown",
		"project.stop.",
		"project.restart.forms-dev",
		"project.stop.Forms-Dev",
		"project.stop.forms-dev --rm",
		"project.stop.forms-dev;calc",
		"project.stop.../x",
	} {
		if _, err := h.ex.Do(id); !errors.Is(err, ErrUnknown) {
			t.Errorf("Do(%q) err = %v, want ErrUnknown", id, err)
		}
	}
	if len(h.argvs) != 0 || len(h.reports) != 0 {
		t.Fatalf("rejected ids had side effects: %v %v", h.argvs, h.reports)
	}

	for verb, want := range map[string][]string{
		"start": {"docker", "start", "aaaaaaaaaaaa", "bbbbbbbbbbbb"},
		"stop":  {"docker", "stop", "aaaaaaaaaaaa", "bbbbbbbbbbbb"},
	} {
		h.advance(3 * time.Second)
		if _, err := h.ex.Do("project." + verb + ".forms-dev"); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		h.waitDone(t)
		h.mu.Lock()
		got := h.argvs[len(h.argvs)-1]
		h.mu.Unlock()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s argv = %q, want %q", verb, got, want)
		}
	}
}

func TestProjectActionsNeedDockerEnabled(t *testing.T) {
	cfg := testCfg()
	cfg.Docker.Enabled = false
	h := newHarness(t, cfg)
	h.project = map[string][]string{"forms-dev": {"aaaaaaaaaaaa"}}
	if _, err := h.ex.Do("project.stop.forms-dev"); !errors.Is(err, ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown with docker disabled", err)
	}
}
