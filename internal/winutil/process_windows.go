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
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("running %s: %w", filepath.Base(argv[0]), err)
	}
	return out, nil
}

// Run is Output with the stdout discarded.
func Run(ctx context.Context, argv []string) error {
	_, err := Output(ctx, argv)
	return err
}
