package hub

import (
	"regexp"
	"time"
)

// The processes and docker domains have no fixed item list: every poll replaces the
// whole list. Health is therefore per domain (state in the envelope), not per item.

// ProcessUpdate is one app (all processes of one executable, summed).
type ProcessUpdate struct {
	ID    string  // lower-case executable name
	Label string  // executable name as Windows reports it
	CPU   float64 // percent of total CPU capacity, raw
	Mem   float64 // private bytes, raw
	Count int     // processes of this executable
}

// ProcessesUpdate is one poll of the processes domain.
type ProcessesUpdate struct {
	Items    []ProcessUpdate
	MemTotal float64 // installed physical memory in bytes, so clients can compute shares
	Fault    string
}

// Container is one Docker container. Pointer fields are nil when the value is unknown
// (stopped containers have no stats).
type Container struct {
	ID       string // short id, 12 lower-case hex digits
	Project  string // compose project, "" for a standalone container
	Name     string
	Image    string
	Status   string // docker's own text, e.g. "Up 3 hours"
	State    string // running | exited | paused | ...
	Ports    string
	CPU      *float64 // percent, raw
	Mem      *float64 // bytes in use
	MemLimit *float64 // bytes
	PIDs     *int
}

// DockerUpdate is one poll. Engine false means the docker CLI could not reach the
// engine (Docker Desktop stopped); Containers is then empty.
type DockerUpdate struct {
	Engine     bool
	Containers []Container
	Fault      string
}

var (
	containerID = regexp.MustCompile(`^[0-9a-f]{12}$`)
	projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// ValidContainerID reports whether s has the shape of a short container id.
func ValidContainerID(s string) bool { return containerID.MatchString(s) }

// ValidProject reports whether s has the shape of a compose project name.
func ValidProject(s string) bool { return projectName.MatchString(s) }

type domain struct {
	updatedAt time.Time
	fault     string
}

// UpdateProcesses replaces the app list and emits one processes event. A non-empty
// fault keeps the previous list but reports it with that state.
func (h *Hub) UpdateProcesses(u ProcessesUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.procs.fault = u.Fault
	if u.Fault == "" {
		h.procItems, h.procMemTotal, h.procs.updatedAt = u.Items, u.MemTotal, h.now()
	}
	h.publishLocked("processes", h.processesPayloadLocked())
}

// UpdateDocker replaces the container list and emits one docker event.
func (h *Hub) UpdateDocker(u DockerUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dock.fault = u.Fault
	if u.Fault == "" {
		h.dockEngine, h.dockItems, h.dock.updatedAt = u.Engine, u.Containers, h.now()
	}
	h.publishLocked("docker", h.dockerPayloadLocked())
}

// ProjectContainers returns the ids of the listed containers of a compose project, in
// list order, or nil if the last poll listed none. Project actions run on exactly these
// ids: the client names a project the server observed, never a container.
func (h *Hub) ProjectContainers(project string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for _, c := range h.dockItems {
		if c.Project != "" && c.Project == project {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// ContainerInfo returns the listed container with this id.
func (h *Hub) ContainerInfo(id string) (Container, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.dockItems {
		if c.ID == id {
			return c, true
		}
	}
	return Container{}, false
}

// HasContainer reports whether id is a container the last successful poll listed.
// Container actions are accepted only for those: the client never names a target
// the server did not itself observe.
func (h *Hub) HasContainer(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.dockItems {
		if c.ID == id {
			return true
		}
	}
	return false
}

type processesEnvelope struct {
	domainEnvelope[processOut]
	MemTotal float64 `json:"memTotal"`
}

type domainEnvelope[T any] struct {
	TS    string `json:"ts"`
	State string `json:"state"`
	AgeMs *int64 `json:"ageMs,omitempty"`
	Items []T    `json:"items"`
}

type processOut struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	CPU   float64 `json:"cpu"`
	Mem   float64 `json:"mem"`
	Count int     `json:"count"`
}

type containerOut struct {
	ID       string   `json:"id"`
	Project  string   `json:"project"`
	Name     string   `json:"name"`
	Image    string   `json:"image"`
	Status   string   `json:"status"`
	State    string   `json:"state"`
	Ports    string   `json:"ports"`
	CPU      *float64 `json:"cpu"`
	Mem      *float64 `json:"mem"`
	MemLimit *float64 `json:"memLimit"`
	PIDs     *int     `json:"pids"`
}

type dockerEnvelope struct {
	domainEnvelope[containerOut]
	Engine bool `json:"engine"`
}

func (h *Hub) processesPayloadLocked() []byte {
	now := h.now()
	state, age := health(h.procs.updatedAt, h.procs.fault, now, h.sensorsStale, h.sensorsDead)
	items := make([]processOut, 0, len(h.procItems))
	for _, p := range h.procItems {
		items = append(items, processOut{ID: p.ID, Label: p.Label, CPU: p.CPU, Mem: p.Mem, Count: p.Count})
	}
	return marshal(processesEnvelope{
		domainEnvelope: domainEnvelope[processOut]{TS: now.Format(time.RFC3339), State: state, AgeMs: age, Items: items},
		MemTotal:       h.procMemTotal,
	})
}

func (h *Hub) dockerPayloadLocked() []byte {
	now := h.now()
	state, age := health(h.dock.updatedAt, h.dock.fault, now, h.sensorsStale, h.sensorsDead)
	items := make([]containerOut, 0, len(h.dockItems))
	for _, c := range h.dockItems {
		items = append(items, containerOut(c))
	}
	return marshal(dockerEnvelope{
		domainEnvelope: domainEnvelope[containerOut]{TS: now.Format(time.RFC3339), State: state, AgeMs: age, Items: items},
		Engine:         h.dockEngine,
	})
}

// MediaUpdate is one reading of the current Windows media session (SMTC).
type MediaUpdate struct {
	Active   bool
	Status   string // playing | paused | stopped | other
	App      string // source app, as Windows names it
	Title    string
	Artist   string
	Album    string
	Position float64 // seconds, extrapolated to the poll instant
	Duration float64 // seconds; 0 when the source reports none
	Volume   float64 // master volume, percent 0-100; -1 when unreadable
	Cover    string  // short id of the cover image ("" = none); changes when the image does
	CoverImg []byte  // the encoded image behind Cover; served at /api/media/cover
	Fault    string
}

// VMUpdate is one poll of the configured VMware VM.
type VMUpdate struct {
	Name    string  // .vmx file name without extension
	State   string  // running | suspended | off
	CPU     float64 // percent of host CPU used by vmware-vmx, raw
	Mem     float64 // private bytes of vmware-vmx, raw
	Running int     // VMs VMware reports running; >1 means CPU and Mem cover all of them
	Fault   string
}

// UpdateMedia applies a poll result and emits one media event.
func (h *Hub) UpdateMedia(u MediaUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.med.fault = u.Fault
	if u.Fault == "" {
		h.mediaNow, h.med.updatedAt = u, h.now()
	}
	h.publishLocked("media", h.mediaPayloadLocked())
}

// UpdateVM applies a poll result and emits one vm event.
func (h *Hub) UpdateVM(u VMUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.vmd.fault = u.Fault
	if u.Fault == "" {
		h.vmNow, h.vmd.updatedAt = u, h.now()
	}
	h.publishLocked("vm", h.vmPayloadLocked())
}

// MediaCover returns the cover image of the current track, or nil.
func (h *Hub) MediaCover() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mediaNow.CoverImg
}

// ProjectInfo returns the listed containers of a compose project (nil if none).
func (h *Hub) ProjectInfo(project string) []Container {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Container
	for _, c := range h.dockItems {
		if c.Project != "" && c.Project == project {
			out = append(out, c)
		}
	}
	return out
}

type mediaEnvelope struct {
	TS       string  `json:"ts"`
	State    string  `json:"state"`
	AgeMs    *int64  `json:"ageMs,omitempty"`
	Active   bool    `json:"active"`
	Status   string  `json:"status"`
	App      string  `json:"app"`
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	Position float64 `json:"position"`
	Duration float64 `json:"duration"`
	Volume   float64 `json:"volume"`
	Cover    string  `json:"cover"`
}

type vmEnvelope struct {
	TS      string  `json:"ts"`
	State   string  `json:"state"`
	AgeMs   *int64  `json:"ageMs,omitempty"`
	Name    string  `json:"name"`
	VM      string  `json:"vm"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	Running int     `json:"running"`
}

func (h *Hub) mediaPayloadLocked() []byte {
	now := h.now()
	state, age := health(h.med.updatedAt, h.med.fault, now, h.sensorsStale, h.sensorsDead)
	m := h.mediaNow
	return marshal(mediaEnvelope{
		TS: now.Format(time.RFC3339), State: state, AgeMs: age,
		Active: m.Active, Status: m.Status, App: m.App, Title: m.Title, Artist: m.Artist, Album: m.Album,
		Position: m.Position, Duration: m.Duration, Volume: m.Volume, Cover: m.Cover,
	})
}

func (h *Hub) vmPayloadLocked() []byte {
	now := h.now()
	state, age := health(h.vmd.updatedAt, h.vmd.fault, now, h.sensorsStale, h.sensorsDead)
	v := h.vmNow
	return marshal(vmEnvelope{
		TS: now.Format(time.RFC3339), State: state, AgeMs: age,
		Name: v.Name, VM: v.State, CPU: v.CPU, Mem: v.Mem, Running: v.Running,
	})
}
