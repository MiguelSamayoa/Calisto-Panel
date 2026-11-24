package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
	"github.com/MiguelSamayoa/calisto-panel/internal/winutil"
)

// vmrun list is read-only. Its argv is built from config only.
func vmListArgv(vmrun string) []string { return []string{vmrun, "-T", "ws", "list"} }

const vmProcess = "vmware-vmx.exe"

// vmName is the .vmx file name without its extension.
func vmName(vmx string) string {
	b := filepath.Base(vmx)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// parseVMList reads `vmrun list`: a "Total running VMs: N" line, then one .vmx path per
// line. Paths are returned lower-cased and cleaned for comparison.
func parseVMList(out []byte) []string {
	var vms []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Total running VMs") {
			continue
		}
		vms = append(vms, strings.ToLower(filepath.Clean(line)))
	}
	return vms
}

// vmState is running when vmrun lists the VM, suspended when VMware left a .vmss
// state file next to it, and off otherwise.
func vmState(running []string, vmx string, exists func(string) bool) string {
	want := strings.ToLower(filepath.Clean(vmx))
	for _, r := range running {
		if r == want {
			return "running"
		}
	}
	if exists(strings.TrimSuffix(vmx, filepath.Ext(vmx)) + ".vmss") {
		return "suspended"
	}
	return "off"
}

// pollVM reports the configured VM: its state from vmrun and its host-side CPU and
// memory from the vmware-vmx process. With several VMs running the process numbers
// cover all of them; Running tells the client.
func pollVM(ctx context.Context, cfg config.VM, vmw config.VMware, run outputFunc, sample func() ([]winutil.ProcSample, error), exists func(string) bool, h *hub.Hub, log *slog.Logger) {
	var tr procTracker
	filter := newProcFilter("", config.Processes{})
	name := vmName(vmw.VMX)
	failing := false
	tick := func() {
		cctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond)
		defer cancel()
		out, err := run(cctx, vmListArgv(vmw.VmrunPath))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !failing {
				log.Warn("vmrun list failed", "err", err)
			}
			failing = true
			h.UpdateVM(hub.VMUpdate{Fault: hub.StateError})
			return
		}
		failing = false
		running := parseVMList(out)
		u := hub.VMUpdate{Name: name, State: vmState(running, vmw.VMX, exists), Running: len(running)}

		// The tracker needs a baseline every tick, so feed it even while the VM is off.
		if samples, err := sample(); err == nil {
			var vmx []winutil.ProcSample
			for _, s := range samples {
				if strings.EqualFold(s.Name, vmProcess) {
					vmx = append(vmx, s)
				}
			}
			if items, ok := tr.update(vmx, time.Now(), runtime.NumCPU(), 1, filter); ok && len(items) > 0 && u.State == "running" {
				u.CPU, u.Mem = items[0].CPU, items[0].Mem
			}
		}
		h.UpdateVM(u)
	}

	t := time.NewTicker(time.Duration(cfg.IntervalMs) * time.Millisecond)
	defer t.Stop()
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
