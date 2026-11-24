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
		TimeoutMs: 60000,
		Docker:    config.Docker{Enabled: true},
		VMware:    config.VMware{Enabled: true, VmrunPath: `C:\VMware\vmrun.exe`, VMX: `C:\VMs\LAB\LAB.vmx`},
		Media:     config.Media{Enabled: true, PlayPauseVK: 0xB3, NextVK: 0xB0, PrevVK: 0xB1},
	}
}

// harness records what the executor does instead of doing it.
type harness struct {
	mu         sync.Mutex
	argvs      [][]string
	keys       []uint8
	seeks      []time.Duration
	seekErr    error
	vols       []int
	afterMedia int
	reports    []hub.ActionUpdate
	block      chan struct{} // when non-nil, Run waits for it or ctx
	runErr     error
	now        time.Time
	known      map[string]bool     // container ids the fake hub has listed
	project    map[string][]string // compose project -> container ids
	ex         *Executor
	done       chan struct{}
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
		Seek: func(_ context.Context, d time.Duration) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.seeks = append(h.seeks, d)
			return h.seekErr
		},
		Volume: func(_ context.Context, pct int) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.vols = append(h.vols, pct)
			return nil
		},
		AfterMedia: func() { h.mu.Lock(); h.afterMedia++; h.mu.Unlock() },
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
	wantIDs := []string{"docker.start", "docker.stop", "vm.stop", "vm.start", "media.playpause", "media.prev", "media.next", "media.seek_back", "media.seek_fwd"}
	if got := h.ex.IDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("IDs = %v, want %v", got, wantIDs)
	}

	wantArgv := map[string][]string{
		"docker.start": {"docker", "desktop", "start"},
		"docker.stop":  {"docker", "desktop", "stop"},
		"vm.stop":      {`C:\VMware\vmrun.exe`, "-T", "ws", "stop", `C:\VMs\LAB\LAB.vmx`, "soft"},
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
	if _, err := h.ex.Do("vm.stop"); !errors.Is(err, ErrUnknown) {
		t.Errorf("vm.stop with vmware disabled: err = %v, want ErrUnknown", err)
	}
}

func TestUnknownIDsRejectedWithoutRunningAnything(t *testing.T) {
	h := newHarness(t, testCfg())
	for _, id := range []string{"", "docker.restart", "DOCKER.START", " docker.start", "docker.start && calc", "../vm.start", "vm.suspend", "vm.stop.hard", "stop hard"} {
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
	st, err := h.ex.Do("vm.stop")
	if err != nil {
		t.Fatal(err)
	}
	if st.Action != "vm.stop" || st.State != hub.ActionRunning || len(st.Exec) != 10 {
		t.Errorf("Started = %+v", st)
	}
	final := h.waitDone(t)
	if final.State != hub.ActionOK || final.Exec != st.Exec {
		t.Errorf("final = %+v", final)
	}
	want := []string{`C:\VMware\vmrun.exe`, "-T", "ws", "stop", `C:\VMs\LAB\LAB.vmx`, "soft"}
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
	h.ex.Wait() // a media action that works reports nothing, so there is no final report to wait for
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

func TestAlreadyRunningButNoRateLimit(t *testing.T) {
	h := newHarness(t, testCfg())
	h.block = make(chan struct{})
	first, err := h.ex.Do("docker.start")
	if err != nil {
		t.Fatal(err)
	}

	// The same id while it runs: 409 with the running exec.
	var busy *AlreadyRunningError
	if _, err := h.ex.Do("docker.start"); !errors.As(err, &busy) || busy.Exec != first.Exec {
		t.Errorf("err = %v, want AlreadyRunning(%s)", err, first.Exec)
	}

	// The opposite action is available at once, even while the first still runs, and
	// so is any other: there is no global rate limit.
	for _, id := range []string{"docker.stop", "vm.start", "vm.stop"} {
		if _, err := h.ex.Do(id); err != nil {
			t.Errorf("%s right after another action: %v", id, err)
		}
	}

	close(h.block)
	for range 4 {
		h.waitDone(t)
	}
	// Once finished, the same id may run again immediately.
	if _, err := h.ex.Do("docker.start"); err != nil {
		t.Errorf("same id after it finished: %v", err)
	}
	h.waitDone(t)
}

func TestMediaActionsAreInstantAndQuiet(t *testing.T) {
	h := newHarness(t, testCfg())
	for range 5 { // hammering must never be refused
		if _, err := h.ex.Do("media.next"); err != nil {
			t.Fatalf("media.next: %v", err)
		}
	}
	h.ex.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.keys) != 5 {
		t.Errorf("keys sent = %v, want 5 presses", h.keys)
	}
	if len(h.reports) != 0 {
		t.Errorf("a media action that works must not report, got %+v", h.reports)
	}
	if h.afterMedia != 5 {
		t.Errorf("AfterMedia calls = %d, want 5", h.afterMedia)
	}
}

func TestMediaKeysAndSeek(t *testing.T) {
	h := newHarness(t, testCfg())
	for id, wantVK := range map[string]uint8{"media.playpause": 0xB3, "media.prev": 0xB1, "media.next": 0xB0} {
		if _, err := h.ex.Do(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		h.ex.Wait()
		h.mu.Lock()
		got := h.keys[len(h.keys)-1]
		h.mu.Unlock()
		if got != wantVK {
			t.Errorf("%s sent vk 0x%X, want 0x%X", id, got, wantVK)
		}
	}
	for id, want := range map[string]time.Duration{"media.seek_fwd": 10 * time.Second, "media.seek_back": -10 * time.Second} {
		if _, err := h.ex.Do(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		h.ex.Wait()
		h.mu.Lock()
		got := h.seeks[len(h.seeks)-1]
		h.mu.Unlock()
		if got != want {
			t.Errorf("%s seeked %v, want %v", id, got, want)
		}
	}
}

func TestMediaFailureIsReported(t *testing.T) {
	h := newHarness(t, testCfg())
	h.seekErr = errors.New("no active media session")
	if _, err := h.ex.Do("media.seek_fwd"); err != nil {
		t.Fatal(err)
	}
	if f := h.waitDone(t); f.State != hub.ActionFailed || f.Message == "" {
		t.Errorf("failure = %+v", f)
	}
	if h.afterMedia != 0 {
		t.Error("AfterMedia must not run after a failure")
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
	a := newExecID(1_700_000_000_000)
	b := newExecID(1_700_000_002_000)
	if len(a) != 10 || a >= b {
		t.Errorf("ids %q %q: want 10 chars and increasing", a, b)
	}
}

func TestExecIDsAreUniqueWithinOneMillisecond(t *testing.T) {
	h := newHarness(t, testCfg()) // the fake clock never advances
	h.block = make(chan struct{})
	seen := map[string]bool{}
	for _, id := range []string{"docker.start", "docker.stop", "vm.start", "vm.stop"} {
		st, err := h.ex.Do(id)
		if err != nil {
			t.Fatal(err)
		}
		if seen[st.Exec] {
			t.Fatalf("duplicate exec id %s", st.Exec)
		}
		seen[st.Exec] = true
	}
	close(h.block)
	for range 4 {
		h.waitDone(t)
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

// The VM is only ever stopped gracefully: a hard stop or a power-off is never offered.
func TestVMNeverHardStops(t *testing.T) {
	h := newHarness(t, testCfg())
	for id, sp := range h.ex.specs {
		for _, a := range sp.argv {
			if a == "hard" {
				t.Errorf("%s uses a hard mode: %q", id, sp.argv)
			}
		}
	}
	for _, id := range []string{"vm.suspend", "vm.kill", "vm.reset", "vm.stop.hard", "vm.poweroff"} {
		if _, err := h.ex.Do(id); !errors.Is(err, ErrUnknown) {
			t.Errorf("Do(%q) err = %v, want ErrUnknown", id, err)
		}
	}
}

func TestVolumeActionAcceptsOnlyCanonicalLevels(t *testing.T) {
	h := newHarness(t, testCfg())
	for id, want := range map[string]int{"media.volume.0": 0, "media.volume.7": 7, "media.volume.50": 50, "media.volume.100": 100} {
		if _, err := h.ex.Do(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		h.ex.Wait()
		h.mu.Lock()
		got := h.vols[len(h.vols)-1]
		h.mu.Unlock()
		if got != want {
			t.Errorf("%s set %d, want %d", id, got, want)
		}
	}
	n := len(h.vols)
	for _, id := range []string{"media.volume.", "media.volume.101", "media.volume.007", "media.volume.-1", "media.volume.5x", "media.volume.1000", "media.volume. 5", "media.volume.5.0"} {
		if _, err := h.ex.Do(id); !errors.Is(err, ErrUnknown) {
			t.Errorf("%q: err = %v, want ErrUnknown", id, err)
		}
	}
	if len(h.vols) != n {
		t.Error("an invalid volume id reached the volume setter")
	}
}
