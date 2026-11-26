package winutil

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole        = kernel32.NewProc("FreeConsole")
)

// HideOwnConsole closes the console window when this process is its only user. It is a
// safety net for a panel.exe built without -H=windowsgui: a console shared with a
// terminal (go run, a shell) is left alone, so debugging output still shows.
func HideOwnConsole() {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		procFreeConsole.Call()
	}
}
