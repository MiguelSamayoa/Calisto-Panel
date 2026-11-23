package main

import (
	"reflect"
	"testing"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
	"github.com/MiguelSamayoa/calisto-panel/internal/lhm"
)

func TestSensorUpdates(t *testing.T) {
	items := []config.SensorItem{
		{ID: "cpu.temp", SensorID: "/cpu/t"},
		{ID: "gpu.temp", SensorID: "/gpu/t"},
	}
	warned := map[string]bool{}

	got, missing := sensorUpdates(items, map[string]lhm.Reading{"/cpu/t": {Value: 55, Unit: "°C"}}, warned)
	want := []hub.SensorUpdate{
		{ID: "cpu.temp", Value: 55, Unit: "°C"},
		{ID: "gpu.temp", Fault: hub.StateUnavailable},
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(missing, []string{"gpu.temp"}) {
		t.Fatalf("got %+v missing %v", got, missing)
	}

	// Still missing on the next tick: reported unavailable again, but not logged again.
	_, missing = sensorUpdates(items, map[string]lhm.Reading{}, warned)
	if len(missing) != 1 || missing[0] != "cpu.temp" {
		t.Errorf("second tick missing = %v, want only cpu.temp (gpu.temp already warned)", missing)
	}

	// A tile that comes back can be reported missing again later.
	sensorUpdates(items, map[string]lhm.Reading{"/gpu/t": {Value: 1}}, warned)
	if warned["gpu.temp"] {
		t.Error("recovered tile still marked as warned")
	}
}
