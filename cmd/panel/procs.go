package main

import (
	"context"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
	"github.com/MiguelSamayoa/calisto-panel/internal/winutil"
)

// procFilter decides which processes count as "apps": their image lives outside
// SystemRoot, or they are named in include (processes whose path cannot be read).
// exclude wins over both. Names are compared lower-case.
type procFilter struct {
	sysRoot string // lower-case, no trailing separator
	exclude map[string]bool
	include map[string]bool
}

func newProcFilter(sysRoot string, cfg config.Processes) procFilter {
	set := func(names []string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[strings.ToLower(n)] = true
		}
		return m
	}
	return procFilter{
		sysRoot: strings.ToLower(strings.TrimRight(sysRoot, `\/`)),
		exclude: set(cfg.Exclude),
		include: set(cfg.IncludeUnreadable),
	}
}

func (f procFilter) app(s winutil.ProcSample) bool {
	name := strings.ToLower(s.Name)
	base := strings.TrimSuffix(name, ".exe")
	if f.exclude[name] || f.exclude[base] {
		return false
	}
	if f.include[name] || f.include[base] {
		return true
	}
	return s.Path != "" && !strings.HasPrefix(strings.ToLower(s.Path), f.sysRoot+`\`)
}

// procTracker turns successive snapshots into CPU percentages.
type procTracker struct {
	prev   map[uint32]time.Duration
	prevAt time.Time
}

// update aggregates samples by executable. CPU is the share of total capacity (all
// logical processors) used since the previous call; the first call has no baseline
// and reports ok=false. The result holds the topN apps by CPU plus the topN by memory.
func (t *procTracker) update(samples []winutil.ProcSample, now time.Time, cpus, topN int, f procFilter) (items []hub.ProcessUpdate, ok bool) {
	elapsed := now.Sub(t.prevAt)
	next := make(map[uint32]time.Duration, len(samples))
	groups := map[string]*hub.ProcessUpdate{}
	var order []string
	for _, s := range samples {
		next[s.PID] = s.CPU
		if !f.app(s) {
			continue
		}
		id := strings.ToLower(s.Name)
		g := groups[id]
		if g == nil {
			g = &hub.ProcessUpdate{ID: id, Label: s.Name}
			groups[id] = g
			order = append(order, id)
		}
		g.Count++
		g.Mem += float64(s.Private)
		// A pid seen for the first time has no delta; a smaller counter means the
		// pid was reused by another process.
		if before, seen := t.prev[s.PID]; seen && elapsed > 0 && s.CPU >= before {
			g.CPU += float64(s.CPU-before) / float64(elapsed) / float64(cpus) * 100
		}
	}
	have := t.prev != nil
	t.prev, t.prevAt = next, now
	if !have {
		return nil, false
	}

	all := make([]hub.ProcessUpdate, 0, len(order))
	for _, id := range order {
		all = append(all, *groups[id])
	}
	byCPU := append([]hub.ProcessUpdate(nil), all...)
	sort.SliceStable(byCPU, func(i, j int) bool { return byCPU[i].CPU > byCPU[j].CPU })
	byMem := append([]hub.ProcessUpdate(nil), all...)
	sort.SliceStable(byMem, func(i, j int) bool { return byMem[i].Mem > byMem[j].Mem })

	seen := map[string]bool{}
	for _, list := range [][]hub.ProcessUpdate{byCPU, byMem} {
		for i := 0; i < len(list) && i < topN; i++ {
			if !seen[list[i].ID] {
				seen[list[i].ID] = true
				items = append(items, list[i])
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].CPU > items[j].CPU })
	return items, true
}

// pollProcesses samples until ctx ends. A failed snapshot keeps the last list and
// marks the domain as an error, so clients see it instead of a silently frozen list.
func pollProcesses(ctx context.Context, cfg config.Processes, sysRoot string, h *hub.Hub, log *slog.Logger) {
	var tr procTracker
	filter := newProcFilter(sysRoot, cfg)
	failing := false
	var total float64
	tick := func() {
		samples, err := winutil.Processes()
		if err != nil {
			if !failing {
				log.Warn("process snapshot failed", "err", err)
			}
			failing = true
			h.UpdateProcesses(hub.ProcessesUpdate{Fault: hub.StateError})
			return
		}
		failing = false
		if items, ok := tr.update(samples, time.Now(), runtime.NumCPU(), cfg.TopN, filter); ok {
			if total == 0 { // constant for the life of the process; retry only while unknown
				if t, err := winutil.TotalMemory(); err == nil {
					total = float64(t)
				}
			}
			h.UpdateProcesses(hub.ProcessesUpdate{Items: items, MemTotal: total})
		}
	}

	t := time.NewTicker(time.Duration(cfg.IntervalMs) * time.Millisecond)
	defer t.Stop()
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
