package hub

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
)

func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Server = config.Server{StaleMs: 10000, DeadMs: 60000, SSEKeepaliveMs: 15000}
	cfg.Sensors = config.Sensors{IntervalMs: 5000, Items: []config.SensorItem{
		{ID: "cpu.temp", Label: "CPU temp"},
		{ID: "gpu.temp", Label: "GPU temp"},
	}}
	cfg.CI = config.CI{IntervalMs: 60000}
	cfg.CI.AzureDevOps.Pipelines = []config.Pipeline{{ID: 42, Label: "main"}}
	cfg.Actions.ConfirmRequired = []string{"docker.stop"}
	return cfg
}

// fakeClock returns a hub whose time is controlled by the returned pointer.
func fakeClock(t *testing.T, cfg *config.Config) (*Hub, *time.Time) {
	t.Helper()
	h, err := New(cfg, []string{"docker.start", "docker.stop"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	return h, &now
}

func decode[T any](t *testing.T, b []byte) envelope[T] {
	t.Helper()
	var e envelope[T]
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
	return e
}

func TestHealthStateMachine(t *testing.T) {
	base := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	st, dd := 10*time.Second, 60*time.Second
	cases := []struct {
		name      string
		updated   time.Time
		fault     string
		age       time.Duration
		want      string
		wantAgeMs bool
	}{
		{"never updated", time.Time{}, "", 0, StateUnavailable, false},
		{"fresh", base, "", 9 * time.Second, StateOK, false},
		{"stale", base, "", 10 * time.Second, StateStale, true},
		{"dead", base, "", 60 * time.Second, StateDead, true},
		{"fault beats freshness", base, StateAuth, time.Second, StateAuth, false},
		{"fault without data", time.Time{}, StateError, 0, StateError, false},
	}
	for _, c := range cases {
		got, age := health(c.updated, c.fault, base.Add(c.age), st, dd)
		if got != c.want || (age != nil) != c.wantAgeMs {
			t.Errorf("%s: health = %s, age=%v; want %s, ageMs=%v", c.name, got, age, c.want, c.wantAgeMs)
		}
	}
}

func TestSensorsEvent(t *testing.T) {
	h, now := fakeClock(t, testConfig())

	// Before any poll every tile exists and is unavailable.
	e := decode[sensorOut](t, h.sensorsPayloadLocked())
	if len(e.Items) != 2 || e.Items[0].State != StateUnavailable || e.Items[0].Label != "CPU temp" {
		t.Fatalf("initial items = %+v", e.Items)
	}

	if err := h.UpdateSensors([]SensorUpdate{
		{ID: "cpu.temp", Value: 55, Unit: "°C"},
		{ID: "gpu.temp", Fault: StateUnavailable},
	}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(14200 * time.Millisecond)
	e = decode[sensorOut](t, h.sensorsPayloadLocked())
	cpu, gpu := e.Items[0], e.Items[1]
	if cpu.State != StateStale || cpu.Value != 55 || cpu.Unit != "°C" || cpu.AgeMs == nil || *cpu.AgeMs != 14200 {
		t.Errorf("cpu = %+v", cpu)
	}
	if gpu.State != StateUnavailable || gpu.Value != 0 {
		t.Errorf("gpu = %+v", gpu)
	}

	// A failed poll that reports nothing keeps aging the value toward dead.
	*now = now.Add(time.Minute)
	if got := decode[sensorOut](t, h.sensorsPayloadLocked()).Items[0].State; got != StateDead {
		t.Errorf("cpu after a minute without updates = %s, want dead", got)
	}

	if err := h.UpdateSensors([]SensorUpdate{{ID: "nope"}}); err == nil {
		t.Error("unknown sensor id accepted")
	}
}

func TestCIEvent(t *testing.T) {
	h, now := fakeClock(t, testConfig())
	raw := string(h.ciPayloadLocked())
	if !strings.Contains(raw, `"result":null`) || !strings.Contains(raw, `"state":"unavailable"`) {
		t.Errorf("initial ci event = %s", raw)
	}

	_ = h.UpdateCI([]CIUpdate{{ID: "ci.42", Result: "failed", Build: "20261123.3", Finished: "2026-11-23T07:51:12Z", URL: "https://x"}})
	it := decode[ciOut](t, h.ciPayloadLocked()).Items[0]
	if it.State != StateOK || it.Result == nil || *it.Result != "failed" {
		t.Errorf("failed build must be state ok / result failed, got %+v", it)
	}

	// 60 s old would be dead for sensors, but CI thresholds scale with its interval.
	*now = now.Add(61 * time.Second)
	if got := decode[ciOut](t, h.ciPayloadLocked()).Items[0].State; got != StateOK {
		t.Errorf("ci state at 61s = %s, want ok", got)
	}
	*now = now.Add(60 * time.Second) // 121 s > 2 × 60 s
	if got := decode[ciOut](t, h.ciPayloadLocked()).Items[0].State; got != StateStale {
		t.Errorf("ci state at 121s = %s, want stale", got)
	}

	_ = h.UpdateCI([]CIUpdate{{ID: "ci.42", Fault: StateAuth}})
	it = decode[ciOut](t, h.ciPayloadLocked()).Items[0]
	if it.State != StateAuth || it.Result != nil || it.Build != nil || it.URL != nil {
		t.Errorf("auth fault must null the data, got %+v", it)
	}
}

func TestCIEmptyWhenNoPipelines(t *testing.T) {
	cfg := testConfig()
	cfg.CI.AzureDevOps.Pipelines = nil
	h, _ := fakeClock(t, cfg)
	if got := decode[ciOut](t, h.ciPayloadLocked()); got.Items == nil || len(got.Items) != 0 {
		t.Errorf("items = %#v, want empty non-nil", got.Items)
	}
	if !strings.Contains(string(h.ciPayloadLocked()), `"items":[]`) {
		t.Errorf("ci payload = %s", h.ciPayloadLocked())
	}
}

func TestActionsRetention(t *testing.T) {
	h, now := fakeClock(t, testConfig())
	h.UpdateAction(ActionUpdate{ID: "docker.stop", Exec: "E1", State: ActionRunning})
	h.UpdateAction(ActionUpdate{ID: "vm.start", Exec: "E2", State: ActionRunning})
	h.UpdateAction(ActionUpdate{ID: "docker.stop", Exec: "E1", State: ActionFailed, Message: "exit status 1"})

	items := decode[actionOut](t, h.actionsPayloadLocked()).Items
	if len(items) != 2 || items[0].State != ActionFailed || items[0].Message == nil || items[1].Message != nil {
		t.Fatalf("items = %+v", items)
	}
	*now = now.Add(59 * time.Second)
	if n := len(decode[actionOut](t, h.actionsPayloadLocked()).Items); n != 2 {
		t.Errorf("after 59s: %d items, want 2", n)
	}
	*now = now.Add(time.Second)
	items = decode[actionOut](t, h.actionsPayloadLocked()).Items
	if len(items) != 1 || items[0].Exec != "E2" {
		t.Errorf("after 60s want only the running E2, got %+v", items)
	}
}

// readEvent returns the next "event:" name and its data line, skipping comments.
func readEvent(t *testing.T, sc *bufio.Scanner) (event, data string) {
	t.Helper()
	for sc.Scan() {
		switch line := sc.Text(); {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			return event, strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("stream ended: %v", sc.Err())
	return
}

func TestServeStream(t *testing.T) {
	cfg := testConfig()
	cfg.Server.SSEKeepaliveMs = 30
	h, err := New(cfg, []string{"docker.start"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(h.ServeStream))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || sc.Text() != "retry: 3000" {
		t.Fatalf("first line = %q, want retry: 3000", sc.Text())
	}

	// hello, then a snapshot of sensors and ci (no actions yet).
	for _, want := range []string{"hello", "sensors", "ci"} {
		if ev, _ := readEvent(t, sc); ev != want {
			t.Fatalf("event = %s, want %s", ev, want)
		}
	}

	if err := h.UpdateSensors([]SensorUpdate{{ID: "cpu.temp", Value: 61.5, Unit: "°C"}}); err != nil {
		t.Fatal(err)
	}
	ev, data := readEvent(t, sc)
	if ev != "sensors" || !strings.Contains(data, `"value":61.5`) {
		t.Errorf("live event = %s %s", ev, data)
	}

	seen := false
	deadline := time.Now().Add(2 * time.Second)
	for !seen && time.Now().Before(deadline) && sc.Scan() {
		seen = sc.Text() == ": keepalive"
	}
	if !seen {
		t.Error("no keepalive comment received")
	}

	h.Close()
	for sc.Scan() { // stream must end once the hub closes
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	h, _ := fakeClock(t, testConfig())
	s := h.subscribe()
	for i := 0; i < subBuffer+1; i++ {
		_ = h.UpdateSensors([]SensorUpdate{{ID: "cpu.temp", Value: float64(i)}})
	}
	h.mu.Lock()
	_, still := h.subs[s]
	h.mu.Unlock()
	if still {
		t.Error("subscriber with a full buffer was not dropped")
	}
}
