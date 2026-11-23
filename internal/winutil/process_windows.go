// Package winutil wraps the Windows-only calls the panel needs: running a fixed
// argv without a shell, and sending media keys to the interactive session.
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

// Run executes argv directly (no shell, no string concatenation) and waits for it.
// The process is killed when ctx ends. Output is discarded.
func Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", filepath.Base(argv[0]), err)
	}
	return nil
}
