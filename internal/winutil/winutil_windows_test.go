package winutil

import (
	"context"
	"os"
	"testing"
	"time"
	"unsafe"
)

func TestKeyInputMatchesWin32Layout(t *testing.T) {
	if got := unsafe.Sizeof(keyInput{}); got != 40 {
		t.Errorf("sizeof(INPUT) = %d, want 40 on amd64", got)
	}
	if got := unsafe.Offsetof(keyInput{}.vk); got != 8 {
		t.Errorf("offset of wVk = %d, want 8", got)
	}
	if got := unsafe.Offsetof(keyInput{}.extraInfo); got != 24 {
		t.Errorf("offset of dwExtraInfo = %d, want 24", got)
	}
}

func TestRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The test binary itself is a harmless executable to launch.
	if err := Run(ctx, []string{os.Args[0], "-test.run=^$"}); err != nil {
		t.Errorf("Run(self) = %v", err)
	}
	if err := Run(ctx, []string{`C:\definitely\not\here.exe`}); err == nil {
		t.Error("Run of a missing executable succeeded")
	}
	if err := Run(ctx, nil); err == nil {
		t.Error("Run with empty argv succeeded")
	}
}

func TestOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := Output(ctx, []string{os.Args[0], "-test.list=TestOutput"})
	if err != nil || len(out) == 0 {
		t.Errorf("Output(self) = %q, %v; want the listed test name", out, err)
	}
}

func TestProcessesSeesSelf(t *testing.T) {
	ps, err := Processes()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.PID == uint32(os.Getpid()) {
			if p.Path == "" || p.Private == 0 {
				t.Errorf("own process incomplete: %+v", p)
			}
			return
		}
	}
	t.Error("own pid not in snapshot")
}

func TestTotalMemory(t *testing.T) {
	got, err := TotalMemory()
	if err != nil || got < 1<<30 {
		t.Errorf("TotalMemory = %d, %v; want at least 1 GiB", got, err)
	}
}

func TestMediaReadOnThisMachine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m := StartMedia(ctx)
	info, err := m.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	t.Logf("media: %+v", info)
	if info.Active && info.Status == "" {
		t.Errorf("an active session must have a status: %+v", info)
	}
	if !info.Active {
		if err := m.SeekBy(ctx, 10*time.Second); err == nil {
			t.Error("seeking without a session must fail")
		}
	}
}

func TestTailBufferKeepsNewestBytes(t *testing.T) {
	w := &tailBuffer{max: 10}
	for i := 0; i < 100; i++ {
		if n, err := w.Write([]byte("0123456789")); n != 10 || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	w.Write([]byte("tail"))
	got := w.tail()
	if len(got) != 10 || string(got[6:]) != "tail" {
		t.Errorf("tail = %q, want the last 10 bytes ending in tail", got)
	}
	if len(w.buf) > 2*w.max+10 {
		t.Errorf("buffer grew to %d", len(w.buf))
	}
}
