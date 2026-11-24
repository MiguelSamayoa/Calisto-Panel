package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// maxStartupLog keeps the startup error file from growing without bound.
const maxStartupLog = 64 << 10

// startupLogDir is where startup failures are recorded: inside %ProgramData%\CalistoPanel,
// the only place the panel may write (CLAUDE.md rule 11).
func startupLogDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		return ""
	}
	return filepath.Join(pd, "CalistoPanel", "logs")
}

// writeStartupError appends err to panel-startup.log in dir. panel.exe is built without a
// console, so a failure before the regular log exists (a bad config.yaml, say) would
// otherwise vanish. Best effort: it can only fail quietly.
func writeStartupError(dir string, err error) {
	if dir == "" || err == nil {
		return
	}
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	path := filepath.Join(dir, "panel-startup.log")
	if st, e := os.Stat(path); e == nil && st.Size() > maxStartupLog {
		_ = os.Remove(path)
	}
	f, e := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if e != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s panel: %v\n", time.Now().Format(time.RFC3339), err)
}
