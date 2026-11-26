package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
)

// outputFunc runs a fixed argv and returns stdout; winutil.Output in production.
type outputFunc func(ctx context.Context, argv []string) ([]byte, error)

// The argv are constants: nothing the network or a container controls reaches them.
var (
	psArgv    = []string{"docker", "ps", "-a", "--format", psFormat}
	statsArgv = []string{"docker", "stats", "--no-stream", "--format", "{{json .}}"}
)

// psFormat asks docker for the compose project label by name. Parsing the comma-joined
// `.Labels` string instead would let any label value containing
// ",com.docker.compose.project=x" forge the project of a container.
const psFormat = `{"ID":{{json .ID}},"Names":{{json .Names}},"Image":{{json .Image}},"Status":{{json .Status}},"State":{{json .State}},"Ports":{{json .Ports}},"Project":{{json (.Label "com.docker.compose.project")}}}`

type psRow struct {
	ID      string `json:"ID"`
	Names   string `json:"Names"`
	Image   string `json:"Image"`
	Status  string `json:"Status"`
	State   string `json:"State"`
	Ports   string `json:"Ports"`
	Project string `json:"Project"`
}

// composeProject keeps a project name only when it is well formed, so a container can
// never be grouped under (or addressed as) an odd-looking name.
func composeProject(name string) string {
	if hub.ValidProject(name) {
		return name
	}
	return ""
}

type statsRow struct {
	ID        string `json:"ID"`
	Container string `json:"Container"` // older docker versions name the id this way
	CPUPerc   string `json:"CPUPerc"`
	MemUsage  string `json:"MemUsage"`
	PIDs      string `json:"PIDs"`
}

// jsonLines decodes the one-JSON-object-per-line output of `docker ... --format {{json .}}`.
func jsonLines[T any](out []byte) ([]T, error) {
	var rows []T
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r T
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("decoding docker output: %w", err)
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}

// parseSize reads docker's byte strings: "10.5MiB", "1.2kB", "0B", "7.6GiB".
func parseSize(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{ // longest suffixes first so "MiB" is not read as "B"
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1},
	}
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			return v * u.mult, err == nil
		}
	}
	return 0, false
}

func parsePercent(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	return v, err == nil
}

// buildContainers merges `docker ps -a` with `docker stats` (running containers only).
// Rows whose id is not 12 lower-case hex digits are dropped: that id is what a client
// may later send back, so only well-formed ones are ever offered.
func buildContainers(ps []psRow, stats []statsRow) []hub.Container {
	byID := map[string]statsRow{}
	for _, s := range stats {
		id := s.ID
		if id == "" {
			id = s.Container
		}
		byID[id] = s
	}
	out := make([]hub.Container, 0, len(ps))
	for _, p := range ps {
		if !hub.ValidContainerID(p.ID) {
			continue
		}
		c := hub.Container{ID: p.ID, Project: composeProject(p.Project), Name: p.Names, Image: p.Image, Status: p.Status, State: p.State, Ports: p.Ports}
		if s, ok := byID[p.ID]; ok {
			if v, ok := parsePercent(s.CPUPerc); ok {
				c.CPU = &v
			}
			if used, limit, ok := strings.Cut(s.MemUsage, "/"); ok {
				if v, ok := parseSize(used); ok {
					c.Mem = &v
				}
				if v, ok := parseSize(limit); ok {
					c.MemLimit = &v
				}
			}
			if n, err := strconv.Atoi(strings.TrimSpace(s.PIDs)); err == nil {
				c.PIDs = &n
			}
		}
		out = append(out, c)
	}
	return out
}

// fetchContainers asks the docker CLI. An error from `docker ps` means the engine is
// not reachable (Docker Desktop stopped, or the CLI is missing); a stats failure only
// loses the numbers for this round.
func fetchContainers(ctx context.Context, run outputFunc) (hub.DockerUpdate, error) {
	out, err := run(ctx, psArgv)
	if err != nil {
		return hub.DockerUpdate{}, err
	}
	ps, err := jsonLines[psRow](out)
	if err != nil {
		return hub.DockerUpdate{}, err
	}
	var stats []statsRow
	for _, p := range ps {
		if p.State == "running" {
			if sout, serr := run(ctx, statsArgv); serr == nil {
				stats, _ = jsonLines[statsRow](sout)
			}
			break
		}
	}
	return hub.DockerUpdate{Engine: true, Containers: buildContainers(ps, stats)}, nil
}

// pollDocker polls until ctx ends. The engine being down is a normal state, reported
// as Engine=false; it is logged on the way down and on recovery only.
func pollDocker(ctx context.Context, cfg config.Containers, run outputFunc, h *hub.Hub, log *slog.Logger) {
	down := false
	tick := func() {
		cctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond)
		defer cancel()
		u, err := fetchContainers(cctx, run)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !down {
				log.Info("docker engine unreachable", "err", err)
			}
			down = true
			h.UpdateDocker(hub.DockerUpdate{Engine: false})
			return
		}
		if down {
			log.Info("docker engine reachable")
		}
		down = false
		h.UpdateDocker(u)
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
