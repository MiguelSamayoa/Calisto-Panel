// Package actions owns the closed whitelist of panel actions. A client only ever
// supplies an action id; every argv is built here from config.yaml.
package actions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
)

var ErrUnknown = errors.New("unknown action")

// AlreadyRunningError means the same action id is still executing.
type AlreadyRunningError struct{ Exec string }

func (e *AlreadyRunningError) Error() string { return "action already running: " + e.Exec }

// Started is the body of a 202 response.
type Started struct {
	Exec   string `json:"exec"`
	Action string `json:"action"`
	State  string `json:"state"`
}

// seekStep is how far media.seek_fwd and media.seek_back move the playback position.
const seekStep = 10 * time.Second

// Deps are the side effects, injected so tests never touch Docker, VMware or keys.
type Deps struct {
	Run  func(ctx context.Context, argv []string) error
	Keys func(vk uint8) error
	Seek func(ctx context.Context, d time.Duration) error
	// Volume sets the master volume to a percentage 0-100.
	Volume func(ctx context.Context, pct int) error
	Report func(hub.ActionUpdate)
	Log    *slog.Logger
	// AfterMedia is called after a media action succeeded, so the now-playing state can
	// be re-read at once instead of at the next poll. May be nil.
	AfterMedia func()
	// HasContainer reports whether the server itself listed this container id in its
	// last poll. Nil means no container is known.
	HasContainer func(id string) bool
	// ProjectContainers returns the container ids of a compose project as the server
	// listed them in its last poll (nil when unknown).
	ProjectContainers func(project string) []string
}

// spec is a process (argv), a media key (vk), a relative seek or a volume level; exactly
// one is set.
// Media specs are instant: they never wait on, nor are blocked by, anything.
type spec struct {
	argv   []string
	vk     uint8
	seek   time.Duration
	vol    int // 0-100; only meaningful when setVol
	setVol bool
	media  bool
}

const maxMessage = 200

type Executor struct {
	specs   map[string]spec
	order   []string
	deps    Deps
	base    context.Context
	timeout time.Duration
	now     func() time.Time
	docker  bool // container.* and project.* are accepted
	volume  bool // media.volume.<0-100> is accepted

	mu      sync.Mutex
	running map[string]string // action id -> exec id (non-media actions only)
	lastMs  int64
	wg      sync.WaitGroup
}

// New builds the whitelist from the enabled groups of cfg. base cancels in-flight
// actions on shutdown; Wait blocks until they have reported.
func New(base context.Context, cfg config.Actions, deps Deps) *Executor {
	e := &Executor{
		specs:   map[string]spec{},
		deps:    deps,
		base:    base,
		timeout: time.Duration(cfg.TimeoutMs) * time.Millisecond,
		now:     time.Now,
		running: map[string]string{},
		docker:  cfg.Docker.Enabled,
		volume:  cfg.Media.Enabled,
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
		// "soft" asks the guest OS to shut down cleanly (VMware Tools). "hard" would cut
		// the power and is forbidden: it destroys unsaved work.
		add("vm.stop", spec{argv: []string{v.VmrunPath, "-T", "ws", "stop", v.VMX, "soft"}})
		add("vm.start", spec{argv: []string{v.VmrunPath, "-T", "ws", "start", v.VMX, "nogui"}})
	}
	if cfg.Media.Enabled {
		add("media.playpause", spec{vk: cfg.Media.PlayPauseVK, media: true})
		add("media.prev", spec{vk: cfg.Media.PrevVK, media: true})
		add("media.next", spec{vk: cfg.Media.NextVK, media: true})
		add("media.seek_back", spec{seek: -seekStep, media: true})
		add("media.seek_fwd", spec{seek: seekStep, media: true})
	}
	return e
}

// IDs lists the enabled actions, in contract order, for the hello event.
func (e *Executor) IDs() []string { return append([]string(nil), e.order...) }

// Wait blocks until every started action has reported its final state.
func (e *Executor) Wait() { e.wg.Wait() }

// Do validates (404, then 409; 401 is the caller's), starts the action in the
// background and returns immediately. There is no global rate limit: a second
// concurrent run of the same id is refused with 409, and the client locks the
// opposite button of a pair itself. Media actions are never refused.
func (e *Executor) Do(id string) (Started, error) {
	s, ok := e.resolve(id)
	if !ok {
		return Started{}, ErrUnknown
	}

	e.mu.Lock()
	if !s.media {
		if exec, busy := e.running[id]; busy {
			e.mu.Unlock()
			return Started{}, &AlreadyRunningError{Exec: exec}
		}
	}
	now := e.now()
	ms := max(now.UnixMilli(), e.lastMs+1) // unique even for starts within one millisecond
	e.lastMs = ms
	exec := newExecID(ms)
	if !s.media {
		e.running[id] = exec
	}
	e.wg.Add(1)
	e.mu.Unlock()

	if !s.media {
		e.deps.Report(hub.ActionUpdate{ID: id, Exec: exec, State: hub.ActionRunning})
	}
	go e.execute(id, exec, s, now)
	return Started{Exec: exec, Action: id, State: hub.ActionRunning}, nil
}

// resolve maps an action id to what to run. Besides the fixed ids it accepts
// container.start.<cid> and container.stop.<cid>, where cid must be a 12-digit hex
// short id that the server itself listed in its last docker poll, and
// project.start.<name> and project.stop.<name>, where name must be a compose project
// the server listed. The argv is built here; its only variable parts are container ids
// the server itself obtained and validated. The client supplies nothing else.
func (e *Executor) resolve(id string) (spec, bool) {
	if s, ok := e.specs[id]; ok {
		return s, true
	}
	if lvl, ok := strings.CutPrefix(id, "media.volume."); ok {
		n, valid := parseVolume(lvl)
		if !valid || !e.volume || e.deps.Volume == nil {
			return spec{}, false
		}
		return spec{vol: n, setVol: true, media: true}, true
	}
	if !e.docker {
		return spec{}, false
	}
	for _, verb := range []string{"start", "stop"} {
		name, ok := strings.CutPrefix(id, "project."+verb+".")
		if !ok {
			continue
		}
		if !hub.ValidProject(name) || e.deps.ProjectContainers == nil {
			return spec{}, false
		}
		ids := e.deps.ProjectContainers(name)
		if len(ids) == 0 {
			return spec{}, false
		}
		return spec{argv: append([]string{"docker", verb}, ids...)}, true
	}
	if e.deps.HasContainer == nil {
		return spec{}, false
	}
	for _, verb := range []string{"start", "stop"} {
		cid, ok := strings.CutPrefix(id, "container."+verb+".")
		if !ok {
			continue
		}
		if !hub.ValidContainerID(cid) || !e.deps.HasContainer(cid) {
			return spec{}, false
		}
		return spec{argv: []string{"docker", verb, cid}}, true
	}
	return spec{}, false
}

// parseVolume accepts exactly the canonical decimal form of 0..100: one to three digits,
// no sign, no leading zero, no spaces. Anything else is not a volume action.
func parseVolume(s string) (int, bool) {
	if s == "" || len(s) > 3 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, n <= 100
}

func (e *Executor) execute(id, exec string, s spec, began time.Time) {
	defer e.wg.Done()
	ctx, cancel := context.WithTimeout(e.base, e.timeout)
	defer cancel()

	var err error
	switch {
	case s.argv != nil:
		err = e.deps.Run(ctx, s.argv)
	case s.setVol:
		err = e.deps.Volume(ctx, s.vol)
	case s.seek != 0:
		err = e.deps.Seek(ctx, s.seek)
	default:
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

	if !s.media {
		e.mu.Lock()
		delete(e.running, id)
		e.mu.Unlock()
	}
	// A media action that worked has nothing to show: the now-playing card changing is
	// the feedback. A failure is reported like any other.
	if !s.media || u.State != hub.ActionOK {
		e.deps.Report(u)
	}
	if s.media && u.State == hub.ActionOK && e.deps.AfterMedia != nil {
		e.deps.AfterMedia()
	}

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

// newExecID encodes a millisecond timestamp as 10 Crockford base32 characters (the
// timestamp half of a ULID): sortable, and unique because Do hands out strictly
// increasing milliseconds.
func newExecID(ms int64) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	v := uint64(ms)
	var b [10]byte
	for i := len(b) - 1; i >= 0; i-- {
		b[i] = alphabet[v&31]
		v >>= 5
	}
	return string(b[:])
}
