package main

import (
	"testing"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/winutil"
)

func TestProcFilter(t *testing.T) {
	f := newProcFilter(`C:\Windows`, config.Processes{Exclude: []string{"panel.exe"}, IncludeUnreadable: []string{"vmmem"}})
	for _, tc := range []struct {
		s    winutil.ProcSample
		want bool
	}{
		{winutil.ProcSample{Name: "chrome.exe", Path: `C:\Program Files\Google\Chrome\chrome.exe`}, true},
		{winutil.ProcSample{Name: "svchost.exe", Path: `C:\Windows\System32\svchost.exe`}, false},
		{winutil.ProcSample{Name: "SearchHost.exe", Path: `c:\WINDOWS\SystemApps\x\SearchHost.exe`}, false},
		{winutil.ProcSample{Name: "setup.exe", Path: `C:\Windowsish\setup.exe`}, true}, // the prefix must end at a separator
		{winutil.ProcSample{Name: "MsMpEng.exe"}, false},                               // unreadable, not listed
		{winutil.ProcSample{Name: "vmmem"}, true},                                      // unreadable, listed
		{winutil.ProcSample{Name: "panel.exe", Path: `C:\Tools\calisto-panel\panel.exe`}, false},
	} {
		if got := f.app(tc.s); got != tc.want {
			t.Errorf("app(%s %q) = %v, want %v", tc.s.Name, tc.s.Path, got, tc.want)
		}
	}
}

func app(pid uint32, name string, cpu time.Duration, mem uint64) winutil.ProcSample {
	return winutil.ProcSample{PID: pid, Name: name, Path: `C:\Apps\` + name, CPU: cpu, Private: mem}
}

func TestProcTrackerNeedsBaseline(t *testing.T) {
	var tr procTracker
	f := newProcFilter(`C:\Windows`, config.Processes{})
	if _, ok := tr.update([]winutil.ProcSample{app(1, "a.exe", 0, 1)}, time.Now(), 4, 5, f); ok {
		t.Fatal("the first update has no baseline and must not report")
	}
}

func TestProcTrackerAggregatesAndRanks(t *testing.T) {
	f := newProcFilter(`C:\Windows`, config.Processes{})
	t0 := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	// 5 s later on 4 CPUs: chrome (2 processes) used 2 s + 1 s = 15% of capacity,
	// game used 4 s = 20%, idle.exe holds the most memory, pid 9 is new (no delta)
	// and svchost is a system process.
	samples := []winutil.ProcSample{
		app(1, "chrome.exe", 2*time.Second, 300),
		app(2, "chrome.exe", 1*time.Second, 200),
		app(3, "game.exe", 4*time.Second, 100),
		app(4, "idle.exe", 0, 900),
		app(9, "new.exe", 7*time.Second, 50),
		{PID: 5, Name: "svchost.exe", Path: `C:\Windows\System32\svchost.exe`, CPU: 9 * time.Second, Private: 999},
	}
	baseline := func() procTracker {
		return procTracker{prev: map[uint32]time.Duration{1: 0, 2: 0, 3: 0, 4: 0, 5: 0}, prevAt: t0}
	}

	tr := baseline()
	got, ok := tr.update(samples, t0.Add(5*time.Second), 4, 1, f)
	if !ok {
		t.Fatal("second update must report")
	}
	// topN=1: CPU leader is game (20%), memory leader is idle (900); union, CPU first.
	if len(got) != 2 || got[0].ID != "game.exe" || got[1].ID != "idle.exe" {
		t.Fatalf("items = %+v, want game.exe then idle.exe", got)
	}
	if got[0].CPU < 19.99 || got[0].CPU > 20.01 {
		t.Errorf("game cpu = %v, want 20", got[0].CPU)
	}

	tr = baseline()
	got, _ = tr.update(samples[:2], t0.Add(5*time.Second), 4, 5, f)
	if len(got) != 1 || got[0].Count != 2 || got[0].Mem != 500 || got[0].CPU < 14.99 || got[0].CPU > 15.01 {
		t.Errorf("chrome group = %+v, want count 2, mem 500, cpu 15", got)
	}
}
