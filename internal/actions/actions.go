// Package actions owns the closed whitelist of panel actions. A client only ever
// supplies an action id; every argv is built here from config.yaml.
package actions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
)

var ErrUnknown = errors.New("unknown action")

// AlreadyRunningError means the same action id is still executing.
type AlreadyRunningError struct{ Exec string }

func (e *AlreadyRunningError) Error() string { return "action already running: " + e.Exec }

// RateLimitedError means another action started less than rate_limit_ms ago.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return "rate limited" }

// Started is the body of a 202 response.
type Started struct {
	Exec   string `json:"exec"`
	Action string `json:"action"`
	State  string `json:"state"`
}

// Deps are the side effects, injected so tests never touch Docker, VMware or keys.
type Deps struct {
	Run    func(ctx context.Context, argv []string) error
	Keys   func(vk uint8) error
	Report func(hub.ActionUpdate)
	Log    *slog.Logger
}

// spec is either a process (argv) or a media key (vk); never both.
type spec struct {
	argv []string
	vk   uint8
}

const maxMessage = 200

type Executor struct {
	specs     map[string]spec
	order     []string
	deps      Deps
	base      context.Context
	timeout   time.Duration
	rateLimit time.Duration
	now       func() time.Time

	mu      sync.Mutex
	running map[string]string // action id -> exec id
	last    time.Time
	wg      sync.WaitGroup
}

// New builds the whitelist from the enabled groups of cfg. base cancels in-flight
// actions on shutdown; Wait blocks until they have reported.
func New(base context.Context, cfg config.Actions, deps Deps) *Executor {
	e := &Executor{
		specs:     map[string]spec{},
		deps:      deps,
		base:      base,
		timeout:   time.Duration(cfg.TimeoutMs) * time.Millisecond,
		rateLimit: time.Duration(cfg.RateLimitMs) * time.Millisecond,
		now:       time.Now,
		running:   map[string]string{},
	}
	add := func(id string, s spec) {
		e.specs[id] = s
		e.order = append(e.order, id)
	}
	if cfg.Docker.Enabled {
		add("docker.start", spec{argv: []string{"docker", "desktop", "start"}})
		add("docker.stop", spec{argv: []string{"docker", "desktop", "stop"}})
	}
	if cfg.VMware.Enabled {
		v := cfg.VMware
		add("vm.suspend", spec{argv: []string{v.VmrunPath, "-T", "ws", "suspend", v.VMX, "soft"}})
		add("vm.start", spec{argv: []string{v.VmrunPath, "-T", "ws", "start", v.VMX, "nogui"}})
	}
	if cfg.Media.Enabled {
		add("media.playpause", spec{vk: cfg.Media.PlayPauseVK})
		add("media.next", spec{vk: cfg.Media.NextVK})
	}
	return e
}

// IDs lists the enabled actions, in contract order, for the hello event.
func (e *Executor) IDs() []string { return append([]string(nil), e.order...) }

// Wait blocks until every started action has reported its final state.
func (e *Executor) Wait() { e.wg.Wait() }

// Do validates in the contract order (404, 409, 429; 401 is the caller's), starts
// the action in the background and returns immediately.
func (e *Executor) Do(id string) (Started, error) {
	s, ok := e.specs[id]
	if !ok {
		return Started{}, ErrUnknown
	}

	e.mu.Lock()
	if exec, busy := e.running[id]; busy {
		e.mu.Unlock()
		return Started{}, &AlreadyRunningError{Exec: exec}
	}
	now := e.now()
	if wait := e.rateLimit - now.Sub(e.last); !e.last.IsZero() && wait > 0 {
		e.mu.Unlock()
		return Started{}, &RateLimitedError{RetryAfter: wait}
	}
	exec := newExecID(now)
	e.last = now
	e.running[id] = exec
	e.wg.Add(1)
	e.mu.Unlock()

	e.deps.Report(hub.ActionUpdate{ID: id, Exec: exec, State: hub.ActionRunning})
	go e.execute(id, exec, s, now)
	return Started{Exec: exec, Action: id, State: hub.ActionRunning}, nil
}

func (e *Executor) execute(id, exec string, s spec, began time.Time) {
	defer e.wg.Done()
	ctx, cancel := context.WithTimeout(e.base, e.timeout)
	defer cancel()

	var err error
	if s.argv != nil {
		err = e.deps.Run(ctx, s.argv)
	} else {
		err = e.deps.Keys(s.vk)
	}

	u := hub.ActionUpdate{ID: id, Exec: exec, State: hub.ActionOK}
	switch {
	case err == nil:
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		u.State, u.Message = hub.ActionTimeout, fmt.Sprintf("timed out after %s", e.timeout)
	default:
		u.State, u.Message = hub.ActionFailed, truncate(err.Error())
	}

	e.mu.Lock()
	delete(e.running, id)
	e.mu.Unlock()
	e.deps.Report(u)

	e.deps.Log.Info("action finished",
		"action", id, "exec", exec, "state", u.State, "message", u.Message,
		"duration", e.now().Sub(began).String())
}

func truncate(s string) string {
	if len(s) > maxMessage {
		return s[:maxMessage]
	}
	return s
}

// newExecID encodes the start time in milliseconds as 10 Crockford base32 characters
// (the timestamp half of a ULID): sortable, and unique because starts are rate limited.
func newExecID(t time.Time) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	ms := uint64(t.UnixMilli())
	var b [10]byte
	for i := len(b) - 1; i >= 0; i-- {
		b[i] = alphabet[ms&31]
		ms >>= 5
	}
	return string(b[:])
}
