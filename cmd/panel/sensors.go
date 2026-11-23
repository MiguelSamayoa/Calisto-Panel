package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
	"github.com/MiguelSamayoa/calisto-panel/internal/lhm"
)

// sensorUpdates maps one LHM snapshot onto the configured tiles. A tile whose
// sensor_id is absent is unavailable; its id is returned in missing the first time
// only, so the caller logs it once and not on every tick.
func sensorUpdates(items []config.SensorItem, readings map[string]lhm.Reading, warned map[string]bool) (updates []hub.SensorUpdate, missing []string) {
	for _, it := range items {
		r, ok := readings[it.SensorID]
		if ok {
			updates = append(updates, hub.SensorUpdate{ID: it.ID, Value: r.Value, Unit: r.Unit})
			delete(warned, it.ID)
			continue
		}
		updates = append(updates, hub.SensorUpdate{ID: it.ID, Fault: hub.StateUnavailable})
		if !warned[it.ID] {
			warned[it.ID] = true
			missing = append(missing, it.ID)
		}
	}
	return updates, missing
}

// pollSensors polls LHM until ctx ends. When LHM does not answer it reports nothing
// for the items (they age to stale and then dead) but still emits the event so
// clients see the aging; failures are logged on the way down and on recovery only.
func pollSensors(ctx context.Context, cfg config.Sensors, client *lhm.Client, h *hub.Hub, log *slog.Logger) {
	warned := map[string]bool{}
	down := false
	tick := func() {
		readings, err := client.Fetch(ctx)
		if err != nil {
			if !down {
				log.Warn("lhm unreachable", "err", err)
			}
			down = true
			_ = h.UpdateSensors(nil)
			return
		}
		if down {
			log.Info("lhm recovered")
		}
		down = false
		updates, missing := sensorUpdates(cfg.Items, readings, warned)
		for _, id := range missing {
			log.Error("sensor_id not found in lhm data", "tile", id)
		}
		if err := h.UpdateSensors(updates); err != nil {
			log.Error("updating sensors", "err", err)
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
