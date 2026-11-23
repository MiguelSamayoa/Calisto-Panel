// Package hub keeps the in-memory state of the three data domains (sensors, ci,
// actions) and fans it out to SSE subscribers. Domains are independent: updating one
// never touches another. stale/dead are computed when emitting, never stored.
package hub

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
)

// Data health states ("can I trust this value?").
const (
	StateOK          = "ok"
	StateStale       = "stale"
	StateDead        = "dead"
	StateUnavailable = "unavailable"
	StateAuth        = "auth"
	StateError       = "error"
)

// Action execution states.
const (
	ActionRunning = "running"
	ActionOK      = "ok"
	ActionFailed  = "failed"
	ActionTimeout = "timeout"
)

const (
	serverName = "calisto-panel/0.1.0"
	// actionRetention is how long a finished action stays in the actions event.
	actionRetention = 60 * time.Second
	subBuffer       = 32
)

type Hub struct {
	mu   sync.Mutex
	now  func() time.Time
	subs map[*subscriber]struct{}
	done chan struct{}
	once sync.Once

	hello           []byte
	keepalive       time.Duration
	sensorsStale    time.Duration
	sensorsDead     time.Duration
	ciStale, ciDead time.Duration

	sensors   []*sensorEntry
	sensorIdx map[string]*sensorEntry
	ci        []*ciEntry
	ciIdx     map[string]*ciEntry
	actions   []*actionEntry
}

type sensorEntry struct {
	id, label string
	value     float64
	unit      string
	updatedAt time.Time
	fault     string
}

type ciEntry struct {
	id, label               string
	result, build, finished string
	url                     string
	updatedAt               time.Time
	fault                   string
}

type actionEntry struct {
	id, exec, state, message string
	finishedAt               time.Time
}

// SensorUpdate reports one sensor after a poll. A non-empty Fault (StateUnavailable,
// StateAuth, StateError) replaces the value; otherwise the value is fresh.
type SensorUpdate struct {
	ID    string
	Value float64
	Unit  string
	Fault string
}

// CIUpdate reports one pipeline after a poll. Empty strings are emitted as null.
type CIUpdate struct {
	ID       string // "ci.<definition id>"
	Result   string
	Build    string
	Finished string
	URL      string
	Fault    string
}

// ActionUpdate reports an action execution changing state.
type ActionUpdate struct {
	ID      string
	Exec    string
	State   string
	Message string
}

// New registers every configured tile as unavailable so none disappears from the
// layout before the first successful poll. actionIDs is the enabled whitelist.
func New(cfg *config.Config, actionIDs []string) (*Hub, error) {
	h := &Hub{
		now:       time.Now,
		subs:      map[*subscriber]struct{}{},
		done:      make(chan struct{}),
		keepalive: time.Duration(cfg.Server.SSEKeepaliveMs) * time.Millisecond,
		sensorIdx: map[string]*sensorEntry{},
		ciIdx:     map[string]*ciEntry{},
	}
	stale := time.Duration(cfg.Server.StaleMs) * time.Millisecond
	dead := time.Duration(cfg.Server.DeadMs) * time.Millisecond
	h.sensorsStale, h.sensorsDead = stale, dead
	// CI polls far less often than sensors, so its thresholds scale with the interval.
	ciEvery := time.Duration(cfg.CI.IntervalMs) * time.Millisecond
	h.ciStale, h.ciDead = max(stale, 2*ciEvery), max(dead, 4*ciEvery)

	for _, it := range cfg.Sensors.Items {
		e := &sensorEntry{id: it.ID, label: it.Label}
		h.sensors = append(h.sensors, e)
		h.sensorIdx[it.ID] = e
	}
	for _, p := range cfg.CI.AzureDevOps.Pipelines {
		e := &ciEntry{id: fmt.Sprintf("ci.%d", p.ID), label: p.Label}
		h.ci = append(h.ci, e)
		h.ciIdx[e.id] = e
	}

	if actionIDs == nil {
		actionIDs = []string{}
	}
	confirm := cfg.Actions.ConfirmRequired
	if confirm == nil {
		confirm = []string{}
	}
	hello, err := json.Marshal(map[string]any{
		"server":            serverName,
		"sensorsIntervalMs": cfg.Sensors.IntervalMs,
		"ciIntervalMs":      cfg.CI.IntervalMs,
		"staleMs":           cfg.Server.StaleMs,
		"deadMs":            cfg.Server.DeadMs,
		"actions":           actionIDs,
		"confirmRequired":   confirm,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding hello: %w", err)
	}
	h.hello = hello
	return h, nil
}

// Close ends every open stream. Safe to call more than once.
func (h *Hub) Close() { h.once.Do(func() { close(h.done) }) }

// UpdateSensors applies a poll result and emits one sensors event. Unknown ids are
// skipped and reported in the returned error; the rest are still applied.
func (h *Hub) UpdateSensors(updates []SensorUpdate) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var unknown []string
	for _, u := range updates {
		e, ok := h.sensorIdx[u.ID]
		if !ok {
			unknown = append(unknown, u.ID)
			continue
		}
		e.fault = u.Fault
		if u.Fault == "" {
			e.value, e.unit, e.updatedAt = u.Value, u.Unit, h.now()
		}
	}
	h.publishLocked("sensors", h.sensorsPayloadLocked())
	if unknown != nil {
		return fmt.Errorf("unknown sensor ids: %v", unknown)
	}
	return nil
}

// UpdateCI applies a poll result and emits one ci event.
func (h *Hub) UpdateCI(updates []CIUpdate) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var unknown []string
	for _, u := range updates {
		e, ok := h.ciIdx[u.ID]
		if !ok {
			unknown = append(unknown, u.ID)
			continue
		}
		e.fault = u.Fault
		if u.Fault == "" {
			e.result, e.build, e.finished, e.url, e.updatedAt = u.Result, u.Build, u.Finished, u.URL, h.now()
		}
	}
	h.publishLocked("ci", h.ciPayloadLocked())
	if unknown != nil {
		return fmt.Errorf("unknown ci ids: %v", unknown)
	}
	return nil
}

// UpdateAction inserts or updates an execution and emits one actions event. Finished
// executions are dropped from the event actionRetention after they end.
func (h *Hub) UpdateAction(u ActionUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var e *actionEntry
	for _, x := range h.actions {
		if x.exec == u.Exec {
			e = x
			break
		}
	}
	if e == nil {
		e = &actionEntry{id: u.ID, exec: u.Exec}
		h.actions = append(h.actions, e)
	}
	e.state, e.message = u.State, u.Message
	finished := u.State != ActionRunning
	if finished {
		e.finishedAt = h.now()
	}
	h.publishLocked("actions", h.actionsPayloadLocked())
	if finished {
		// Emit again once the retention window passes so clients drop the item.
		time.AfterFunc(actionRetention, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.publishLocked("actions", h.actionsPayloadLocked())
		})
	}
}

// health is the state machine behind every item: faults win, then age decides.
func health(updatedAt time.Time, fault string, now time.Time, stale, dead time.Duration) (string, *int64) {
	if fault != "" {
		return fault, nil
	}
	if updatedAt.IsZero() {
		return StateUnavailable, nil
	}
	age := now.Sub(updatedAt)
	ms := age.Milliseconds()
	switch {
	case age >= dead:
		return StateDead, &ms
	case age >= stale:
		return StateStale, &ms
	}
	return StateOK, nil
}

type envelope[T any] struct {
	TS    string `json:"ts"`
	Items []T    `json:"items"`
}

type sensorOut struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
	State string  `json:"state"`
	AgeMs *int64  `json:"ageMs,omitempty"`
}

type ciOut struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	State    string  `json:"state"`
	Result   *string `json:"result"`
	Build    *string `json:"build"`
	Finished *string `json:"finished"`
	URL      *string `json:"url"`
	AgeMs    *int64  `json:"ageMs,omitempty"`
}

type actionOut struct {
	ID      string  `json:"id"`
	Exec    string  `json:"exec"`
	State   string  `json:"state"`
	Message *string `json:"message"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (h *Hub) sensorsPayloadLocked() []byte {
	now := h.now()
	items := make([]sensorOut, 0, len(h.sensors))
	for _, e := range h.sensors {
		state, age := health(e.updatedAt, e.fault, now, h.sensorsStale, h.sensorsDead)
		o := sensorOut{ID: e.id, Label: e.label, Unit: e.unit, State: state, AgeMs: age}
		if state != StateUnavailable && e.fault == "" {
			o.Value = e.value
		}
		items = append(items, o)
	}
	return marshal(envelope[sensorOut]{TS: now.Format(time.RFC3339), Items: items})
}

func (h *Hub) ciPayloadLocked() []byte {
	now := h.now()
	items := make([]ciOut, 0, len(h.ci))
	for _, e := range h.ci {
		state, age := health(e.updatedAt, e.fault, now, h.ciStale, h.ciDead)
		o := ciOut{ID: e.id, Label: e.label, State: state, AgeMs: age}
		if e.fault == "" && !e.updatedAt.IsZero() {
			o.Result, o.Build, o.Finished, o.URL = nullable(e.result), nullable(e.build), nullable(e.finished), nullable(e.url)
		}
		items = append(items, o)
	}
	return marshal(envelope[ciOut]{TS: now.Format(time.RFC3339), Items: items})
}

func (h *Hub) actionsPayloadLocked() []byte {
	now := h.now()
	kept := h.actions[:0]
	for _, e := range h.actions {
		if e.finishedAt.IsZero() || now.Sub(e.finishedAt) < actionRetention {
			kept = append(kept, e)
		}
	}
	h.actions = kept
	items := make([]actionOut, 0, len(kept))
	for _, e := range kept {
		items = append(items, actionOut{ID: e.id, Exec: e.exec, State: e.state, Message: nullable(e.message)})
	}
	return marshal(envelope[actionOut]{TS: now.Format(time.RFC3339), Items: items})
}

// marshal cannot realistically fail: the payloads are plain structs of strings and
// finite numbers (the LHM parser rejects Inf/NaN). Fall back to an empty event.
func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"ts":"","items":[]}`)
	}
	return b
}
