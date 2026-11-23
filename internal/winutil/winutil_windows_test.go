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
