package main

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/httpapi"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
)

const (
	logsTail       = "200" // lines asked of each container
	logsKeep       = 400   // lines returned in total
	logsMaxLine    = 2000
	logsMaxMembers = 12
	logsTimeout    = 6 * time.Second
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

type logLine struct {
	T int64  `json:"t"` // unix milliseconds
	C string `json:"c"` // container id
	M string `json:"m"`
}

type logMember struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// State lets the client dim the entry of a stopped container.
	State string `json:"state"`
}

// logsArgv is fixed apart from the container id, which comes from the hub's own list.
func logsArgv(id string) []string {
	return []string{"docker", "logs", "--tail", logsTail, "--timestamps", id}
}

// parseLogs splits `docker logs --timestamps` output into lines. A line with no
// parsable timestamp (a continuation) inherits the previous one's.
func parseLogs(id string, out []byte) []logLine {
	var lines []logLine
	var last int64
	for _, raw := range strings.Split(string(out), "\n") {
		raw = strings.TrimRight(raw, "\r")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		raw = ansiEscape.ReplaceAllString(raw, "")
		ts, msg := last, raw
		if head, rest, ok := strings.Cut(raw, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, head); err == nil {
				ts, msg = t.UnixMilli(), rest
				last = ts
			}
		}
		if len(msg) > logsMaxLine {
			msg = msg[:logsMaxLine] + "…"
		}
		lines = append(lines, logLine{T: ts, C: id, M: msg})
	}
	return lines
}

// mergeLogs interleaves the per-container lines by time and keeps the newest keep.
func mergeLogs(per [][]logLine, keep int) []logLine {
	var all []logLine
	for _, l := range per {
		all = append(all, l...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].T < all[j].T })
	if len(all) > keep {
		all = all[len(all)-keep:]
	}
	return all
}

// newLogsFunc returns the handler behind GET /api/logs/{project} and
// GET /api/logs/container/{id}. Only containers the hub itself listed are read;
// concurrent calls are serialised so a busy client cannot pile up docker processes.
func newLogsFunc(h *hub.Hub, run outputFunc) httpapi.LogsFunc {
	var mu sync.Mutex
	return func(ctx context.Context, kind, name string) ([]byte, error) {
		var members []hub.Container
		switch kind {
		case httpapi.LogsProject:
			if hub.ValidProject(name) {
				members = h.ProjectInfo(name)
			}
		case httpapi.LogsContainer:
			if hub.ValidContainerID(name) {
				if c, ok := h.ContainerInfo(name); ok {
					members = []hub.Container{c}
				}
			}
		}
		if len(members) == 0 {
			return nil, httpapi.ErrNotFound
		}
		if len(members) > logsMaxMembers {
			members = members[:logsMaxMembers]
		}

		mu.Lock()
		defer mu.Unlock()
		ctx, cancel := context.WithTimeout(ctx, logsTimeout)
		defer cancel()

		per := make([][]logLine, len(members))
		var wg sync.WaitGroup
		for i, c := range members {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A container with no logs, or one that failed, just contributes nothing.
				if out, err := run(ctx, logsArgv(c.ID)); err == nil {
					per[i] = parseLogs(c.ID, out)
				}
			}()
		}
		wg.Wait()

		info := make([]logMember, len(members))
		for i, c := range members {
			info[i] = logMember{ID: c.ID, Name: c.Name, State: c.State}
		}
		return json.Marshal(map[string]any{
			"kind":       kind,
			"name":       name,
			"containers": info,
			"lines":      mergeLogs(per, logsKeep),
		})
	}
}
