package lhm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestParseValue(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		unit string
	}{
		{"41,0 °C", 41.0, "°C"},
		{"41.0 °C", 41.0, "°C"},
		{"4.593,0 MHz", 4593.0, "MHz"},
		{"1,000 V", 1.0, "V"},
		{"11,8 GB", 11.8, "GB"},
		{"11.8 GB", 11.8, "GB"},
		{"0.0 KB/s", 0, "KB/s"},
		{"45000 mWh", 45000, "mWh"},
	}
	for _, c := range cases {
		got, unit, err := ParseValue(c.in)
		if err != nil || got != c.want || unit != c.unit {
			t.Errorf("ParseValue(%q) = %v, %q, %v; want %v, %q", c.in, got, unit, err, c.want, c.unit)
		}
	}
	for _, in := range []string{"-", "", "   ", "abc", "Value"} {
		if _, _, err := ParseValue(in); err == nil {
			t.Errorf("ParseValue(%q) succeeded, want error", in)
		}
	}
}

func TestParseFixture(t *testing.T) {
	data, err := os.ReadFile("../../testdata/lhm/data.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ // sensor ids configured for this machine
		"/intelcpu/0/load/0":          "%",
		"/intelcpu/0/temperature/6":   "°C",
		"/gpu-nvidia/0/temperature/0": "°C",
		"/ram/data/0":                 "GB",
		"/nic/%7BFFE353B5-F83D-4849-96C4-EBC254BB1619%7D/throughput/8": "B/s", // RawValue is in the base unit; Value says KB/s
	}
	for id, unit := range want {
		r, ok := got[id]
		if !ok {
			t.Errorf("sensor %s missing from fixture", id)
			continue
		}
		if r.Unit != unit {
			t.Errorf("sensor %s unit = %q, want %q", id, r.Unit, unit)
		}
	}
	if r := got["/intelcpu/0/temperature/6"]; r.Value <= 0 || r.Value > 120 {
		t.Errorf("cpu package temperature out of range: %v", r.Value)
	}
}

func TestParseRawValuePreferredAndGroupsIgnored(t *testing.T) {
	doc := `{"Text":"Sensor","Value":"Value","Children":[
		{"Text":"PC","Value":"","Children":[
			{"SensorId":"/a","Value":"41,0 °C","RawValue":"41.5 °C","Children":[]},
			{"SensorId":"/b","Value":"-","Children":[]},
			{"SensorId":"/a","Value":"99 °C","Children":[]}
		]}]}`
	got, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["/a"].Value != 41.5 {
		t.Errorf("got %v, want only /a = 41.5", got)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"Children":[{"SensorId":"/x","Value":"1,5 V","Children":[]}]}`))
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, time.Second).Fetch(context.Background())
	if err != nil || got["/x"].Value != 1.5 {
		t.Fatalf("Fetch = %v, %v", got, err)
	}
	if _, err := NewClient(srv.URL+"/bad", time.Second).Fetch(context.Background()); err == nil {
		t.Error("Fetch on 500 succeeded, want error")
	}
}
