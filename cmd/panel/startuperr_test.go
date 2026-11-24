package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteStartupErrorAppends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs") // does not exist yet
	writeStartupError(dir, errors.New("invalid config: port out of range"))
	writeStartupError(dir, errors.New("second failure"))
	b, err := os.ReadFile(filepath.Join(dir, "panel-startup.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Count(got, "\n") != 2 || !strings.Contains(got, "port out of range") || !strings.Contains(got, "second failure") {
		t.Errorf("log = %q", got)
	}
}

func TestWriteStartupErrorIsBoundedAndQuiet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel-startup.log")
	if err := os.WriteFile(path, make([]byte, maxStartupLog+1), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStartupError(dir, errors.New("fresh"))
	if st, _ := os.Stat(path); st.Size() > 1024 {
		t.Errorf("an oversized log must be restarted, size = %d", st.Size())
	}
	writeStartupError("", errors.New("no dir")) // must not panic or write anywhere
	writeStartupError(dir, nil)                 // nothing to record
	if err := os.WriteFile(filepath.Join(dir, "blocker"), nil, 0o644); err == nil {
		writeStartupError(filepath.Join(dir, "blocker", "sub"), errors.New("cannot create dir")) // quiet failure
	}
}
