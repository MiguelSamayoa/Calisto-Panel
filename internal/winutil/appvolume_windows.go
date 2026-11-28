package winutil

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Per-application volume, like EarTrumpet: Core Audio sessions of the default playback
// device (IAudioSessionManager2). Sessions are grouped by executable name, so every
// Chrome tab moves together; the group id is a hash of that name, never a PID.

var (
	iidAudioSessionManager2 = windows.GUID{Data1: 0x77aa99a0, Data2: 0x1bd6, Data3: 0x484f, Data4: [8]byte{0x8b, 0xc7, 0x2c, 0x65, 0x4c, 0x9a, 0x9b, 0x6f}}
	iidAudioSessionControl2 = windows.GUID{Data1: 0xbfb7ff88, Data2: 0x7239, Data3: 0x4fc9, Data4: [8]byte{0x8f, 0xa2, 0x07, 0xc9, 0x50, 0xbe, 0x9c, 0x6d}}
	iidSimpleAudioVolume    = windows.GUID{Data1: 0x87ce5498, Data2: 0x68d6, Data3: 0x44e5, Data4: [8]byte{0x92, 0x15, 0x6d, 0xa4, 0x7e, 0xf8, 0x83, 0xd8}}
)

const (
	slotSessMgrEnum  = 5  // IAudioSessionManager2.GetSessionEnumerator
	slotEnumCount    = 3  // IAudioSessionEnumerator.GetCount
	slotEnumGet      = 4  // IAudioSessionEnumerator.GetSession
	slotCtlGetState  = 3  // IAudioSessionControl.GetState
	slotCtl2GetPID   = 14 // IAudioSessionControl2.GetProcessId
	slotCtl2IsSystem = 15 // IAudioSessionControl2.IsSystemSoundsSession
	slotSimpleSetVol = 3  // ISimpleAudioVolume.SetMasterVolume
	slotSimpleGetVol = 4  // ISimpleAudioVolume.GetMasterVolume

	audioSessionExpired = 2
	processQueryLimited = 0x1000
)

// AudioApp is one application that has an audio session. ID names it in SetAppVolume.
type AudioApp struct {
	ID     string
	Name   string  // executable name without .exe
	Volume float64 // percent 0-100
}

// Apps lists the applications with an audio session on the default playback device.
func (m *Media) Apps(ctx context.Context) ([]AudioApp, error) {
	var out []AudioApp
	err := m.run(ctx, func() error {
		return eachAudioSession(func(name, label string, vol unsafe.Pointer) bool {
			var v float32
			if vcall(vol, slotSimpleGetVol, uintptr(unsafe.Pointer(&v))) != 0 {
				return true
			}
			id := shortHash(name)
			for _, o := range out {
				if o.ID == id {
					return true
				}
			}
			out = append(out, AudioApp{ID: id, Name: label, Volume: float64(v) * 100})
			return true
		})
	})
	return out, err
}

// SetAppVolume sets every session of the app with this id to pct (0-100).
func (m *Media) SetAppVolume(ctx context.Context, id string, pct int) error {
	if pct < 0 || pct > 100 {
		return errors.New("volume out of range")
	}
	return m.run(ctx, func() error {
		found := false
		bits := uintptr(math.Float32bits(float32(pct) / 100)) // see SetVolume for the float ABI note
		err := eachAudioSession(func(name, label string, vol unsafe.Pointer) bool {
			if shortHash(name) == id {
				found = true
				vcall(vol, slotSimpleSetVol, bits, 0)
			}
			return true
		})
		if err == nil && !found {
			err = errors.New("no audio session for that app")
		}
		return err
	})
}

// eachAudioSession calls f with the executable name and ISimpleAudioVolume of every live,
// non-system audio session. Runs on the WinRT thread.
func eachAudioSession(f func(name, label string, vol unsafe.Pointer) bool) error {
	dev, err := defaultDevice()
	if err != nil {
		return err
	}
	defer release(dev)
	var mgr unsafe.Pointer
	if hr := vcall(dev, slotDeviceActivate, uintptr(unsafe.Pointer(&iidAudioSessionManager2)), clsctxAll, 0, uintptr(unsafe.Pointer(&mgr))); hr != 0 {
		return hrErr("IMMDevice.Activate(IAudioSessionManager2)", hr)
	}
	defer release(mgr)
	en := comOut(mgr, slotSessMgrEnum)
	if en == nil {
		return errors.New("GetSessionEnumerator failed")
	}
	defer release(en)
	parents := parentMap()
	var n int32
	if hr := vcall(en, slotEnumCount, uintptr(unsafe.Pointer(&n))); hr != 0 {
		return hrErr("GetCount", hr)
	}
	for i := int32(0); i < n; i++ {
		ctl := comOut(en, slotEnumGet, uintptr(i))
		if ctl == nil {
			continue
		}
		cont := visitAudioSession(ctl, parents, f)
		release(ctl)
		if !cont {
			break
		}
	}
	return nil
}

func visitAudioSession(ctl unsafe.Pointer, parents map[uint32]uint32, f func(string, string, unsafe.Pointer) bool) bool {
	var st int32
	if vcall(ctl, slotCtlGetState, uintptr(unsafe.Pointer(&st))) != 0 || st == audioSessionExpired {
		return true
	}
	var c2, vol unsafe.Pointer
	if vcall(ctl, slotQueryInterface, uintptr(unsafe.Pointer(&iidAudioSessionControl2)), uintptr(unsafe.Pointer(&c2))) != 0 {
		return true
	}
	defer release(c2)
	if vcall(c2, slotCtl2IsSystem) == 0 { // S_OK means it IS the system sounds session
		return true
	}
	var pid uint32
	if vcall(c2, slotCtl2GetPID, uintptr(unsafe.Pointer(&pid))) != 0 || pid == 0 {
		return true
	}
	name, label := owner(pid, parents)
	if name == "" {
		return true
	}
	if vcall(ctl, slotQueryInterface, uintptr(unsafe.Pointer(&iidSimpleAudioVolume)), uintptr(unsafe.Pointer(&vol))) != 0 {
		return true
	}
	defer release(vol)
	return f(name, label, vol)
}

// hostExes are helper hosts that render for another app (WhatsApp, Teams and others embed
// WebView2): the volume belongs to whoever launched them, so walk up to that parent.
var hostExes = map[string]bool{"msedgewebview2.exe": true}

// owner returns the grouping key (executable name) and a human label of the app that
// owns pid, skipping helper hosts. The label is the exe's FileDescription ("Google
// Chrome"), or the cleaned exe name when it has none.
func owner(pid uint32, parents map[uint32]uint32) (key, label string) {
	var path string
	for i := 0; i < 8; i++ {
		p := exePath(pid)
		if p == "" {
			break
		}
		path = p
		if !hostExes[strings.ToLower(filepath.Base(p))] {
			break
		}
		next, ok := parents[pid]
		if !ok || next == 0 {
			break
		}
		pid = next
	}
	if path == "" {
		return "", ""
	}
	base := strings.ToLower(filepath.Base(path))
	return base, describe(path, filepath.Base(path))
}

// descCache is touched only from the WinRT thread.
var descCache = map[string]string{}

func describe(path, base string) string {
	if d, ok := descCache[path]; ok {
		return d
	}
	d := fileDescription(path)
	if d == "" || strings.EqualFold(d, strings.TrimSuffix(base, filepath.Ext(base))) {
		d = strings.TrimSuffix(strings.TrimSuffix(base, filepath.Ext(base)), ".Root")
	}
	descCache[path] = d
	return d
}

// fileDescription reads the FileDescription string of an executable ("" if none or
// unreadable, e.g. a packaged app whose folder the user cannot open).
func fileDescription(path string) string {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return ""
	}
	buf := make([]byte, size)
	if windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])) != nil {
		return ""
	}
	var tr unsafe.Pointer
	var n uint32
	if windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\VarFileInfo\Translation`, unsafe.Pointer(&tr), &n) != nil || n < 4 {
		return ""
	}
	t := *(*[2]uint16)(tr)
	var s unsafe.Pointer
	sub := fmt.Sprintf(`\StringFileInfo\%04x%04x\FileDescription`, t[0], t[1])
	if windows.VerQueryValue(unsafe.Pointer(&buf[0]), sub, unsafe.Pointer(&s), &n) != nil || n == 0 {
		return ""
	}
	return strings.TrimSpace(windows.UTF16PtrToString((*uint16)(s)))
}

// parentMap returns pid -> parent pid for every process.
func parentMap() map[uint32]uint32 {
	m := map[uint32]uint32{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return m
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		m[e.ProcessID] = e.ParentProcessID
	}
	return m
}

func exePath(pid uint32) string {
	h, err := windows.OpenProcess(processQueryLimited, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
