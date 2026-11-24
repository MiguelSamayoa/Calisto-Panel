package winutil

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProcSample is one process at one instant. Path is empty when the process cannot be
// opened without elevation (protected and other-session system processes).
type ProcSample struct {
	PID  uint32
	Name string
	Path string
	// CPU is cumulative kernel+user time; callers diff two samples.
	CPU time.Duration
	// Private is the private commit in bytes (PrivateUsage), which does not double
	// count pages shared between the many processes of a browser.
	Private uint64
}

var procGetProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// processMemoryCountersEx is PROCESS_MEMORY_COUNTERS_EX.
type processMemoryCountersEx struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

// Processes snapshots every process on the machine. One the unelevated user cannot
// open is still returned, with Path empty and no CPU or memory.
func Processes() ([]ProcSample, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	var out []ProcSample
	for {
		if entry.ProcessID != 0 {
			s := ProcSample{PID: entry.ProcessID, Name: windows.UTF16ToString(entry.ExeFile[:])}
			fill(&s)
			out = append(out, s)
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break // ERROR_NO_MORE_FILES ends the walk
		}
	}
	return out, nil
}

func fill(s *ProcSample) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, s.PID)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_PATH*2)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil {
		s.Path = windows.UTF16ToString(buf[:n])
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
		s.CPU = time.Duration(filetime(kernel)+filetime(user)) * 100 // 100 ns ticks
	}
	var c processMemoryCountersEx
	c.cb = uint32(unsafe.Sizeof(c))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r != 0 {
		s.Private = uint64(c.PrivateUsage)
	}
}

func filetime(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// TotalMemory returns the installed physical memory in bytes.
func TotalMemory() (uint64, error) {
	m := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, fmt.Errorf("reading total memory: %w", err)
	}
	return m.TotalPhys, nil
}
