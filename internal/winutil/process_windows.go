// Package winutil wraps the Windows-only calls the panel needs: running a fixed
// argv without a shell, sending media keys to the interactive session, and sampling
// per-process CPU and memory.
package winutil

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"os/exec"
)

const createNoWindow = 0x08000000

func command(ctx context.Context, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	cmd.WaitDelay = 2 * time.Second
	return cmd, nil
}

// Output executes argv directly (no shell, no string concatenation), waits for it and
// returns its stdout. The process is killed when ctx ends.
func Output(ctx context.Context, argv []string) ([]byte, error) {
	cmd, err := command(ctx, argv)
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running %s: %w", filepath.Base(argv[0]), err)
	}
	return out, nil
}

// CombinedOutput is Output with stderr interleaved into the result, for tools such as
// `docker logs` that write a container's stderr to their own stderr.
func CombinedOutput(ctx context.Context, argv []string) ([]byte, error) {
	cmd, err := command(ctx, argv)
	if err != nil {
		return nil, err
	}
	w := &tailBuffer{max: maxCombinedOutput}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running %s: %w", filepath.Base(argv[0]), err)
	}
	return w.tail(), nil
}

// maxCombinedOutput caps what CombinedOutput keeps. `docker logs --tail` limits lines,
// not their length, so a container printing huge lines must not exhaust the panel's RAM.
const maxCombinedOutput = 1 << 20

// tailBuffer keeps only the last max bytes written to it: the newest log lines matter.
// It always accepts the whole write so the child process never blocks or fails.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) tail() []byte {
	if len(t.buf) > t.max {
		return t.buf[len(t.buf)-t.max:]
	}
	return t.buf
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max { // trim in bulk, not on every write
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// Run is Output with the stdout discarded.
func Run(ctx context.Context, argv []string) error {
	_, err := Output(ctx, argv)
	return err
}
