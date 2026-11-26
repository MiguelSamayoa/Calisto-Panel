package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]float64{
		"0B":      0,
		"512B":    512,
		"1.5kB":   1500,
		"10.5MiB": 10.5 * (1 << 20),
		"7.6GiB":  7.6 * (1 << 30),
		"2MB":     2e6,
		" 3KiB ":  3 * 1024,
	} {
		if got, ok := parseSize(in); !ok || got != want {
			t.Errorf("parseSize(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "--", "MiB", "abc"} {
		if _, ok := parseSize(in); ok {
			t.Errorf("parseSize(%q) accepted", in)
		}
	}
}

const psOut = `{"ID":"aaaaaaaaaaaa","Names":"web","Image":"nginx:1","Status":"Up 3 hours","State":"running","Ports":"0.0.0.0:80->80/tcp","Project":"forms-dev"}
{"ID":"bbbbbbbbbbbb","Names":"db","Image":"postgres","Status":"Exited (0) 2 days ago","State":"exited","Ports":""}
{"ID":"--rm","Names":"evil","Image":"x","Status":"","State":"running","Ports":""}
`

const statsOut = `{"ID":"aaaaaaaaaaaa","CPUPerc":"1.25%","MemUsage":"10.5MiB / 7.6GiB","PIDs":"4"}
`

func TestFetchContainersMergesStatsAndDropsMalformedIDs(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		calls = append(calls, argv)
		if argv[1] == "ps" {
			return []byte(psOut), nil
		}
		return []byte(statsOut), nil
	}
	u, err := fetchContainers(context.Background(), run)
	if err != nil || !u.Engine {
		t.Fatalf("fetch = %+v, %v", u, err)
	}
	if len(u.Containers) != 2 {
		t.Fatalf("containers = %+v, want web and db only (the malformed id is dropped)", u.Containers)
	}
	web, db := u.Containers[0], u.Containers[1]
	if web.Project != "forms-dev" || db.Project != "" {
		t.Errorf("projects = %q, %q; want forms-dev and none", web.Project, db.Project)
	}
	if web.Name != "web" || web.CPU == nil || *web.CPU != 1.25 || *web.Mem != 10.5*(1<<20) || *web.MemLimit != 7.6*(1<<30) || *web.PIDs != 4 {
		t.Errorf("web = %+v", web)
	}
	if db.CPU != nil || db.Mem != nil || db.PIDs != nil {
		t.Errorf("a stopped container has no stats, got %+v", db)
	}
	want := [][]string{psArgv, statsArgv}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("argv = %v, want %v", calls, want)
	}
}

func TestFetchContainersSkipsStatsWhenNothingRuns(t *testing.T) {
	n := 0
	run := func(_ context.Context, argv []string) ([]byte, error) {
		n++
		return []byte(`{"ID":"bbbbbbbbbbbb","Names":"db","State":"exited"}`), nil
	}
	if _, err := fetchContainers(context.Background(), run); err != nil || n != 1 {
		t.Errorf("err=%v calls=%d, want a single docker ps", err, n)
	}
}

func TestFetchContainersEngineDown(t *testing.T) {
	run := func(context.Context, []string) ([]byte, error) { return nil, errors.New("pipe not found") }
	if _, err := fetchContainers(context.Background(), run); err == nil {
		t.Error("an unreachable engine must be an error")
	}
}

func TestComposeProject(t *testing.T) {
	for name, want := range map[string]string{
		"":          "",
		"forms-dev": "forms-dev",
		"app_1":     "app_1",
		"Has Space": "", // not a valid project name
		"a;b":       "",
		"a,b=c":     "",
	} {
		if got := composeProject(name); got != want {
			t.Errorf("composeProject(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestForgedLabelCannotSetProject(t *testing.T) {
	// The project comes from its own field, so a label value that merely looks like the
	// compose label has no effect: an odd project name is dropped.
	rows, err := jsonLines[psRow]([]byte(`{"ID":"cccccccccccc","Names":"x","State":"running","Project":"a,com.docker.compose.project=b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := buildContainers(rows, nil)[0].Project; got != "" {
		t.Errorf("project = %q, want none", got)
	}
}
