package winutil

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Master volume of the default playback device (Core Audio, IAudioEndpointVolume),
// called by hand like the media session, on the same thread. Slots come from
// mmdeviceapi.h and endpointvolume.h.

var (
	ole32                   = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance    = ole32.NewProc("CoCreateInstance")
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xbcde0395, Data2: 0xe52f, Data3: 0x467c, Data4: [8]byte{0x8e, 0x3d, 0xc4, 0x57, 0x92, 0x91, 0x69, 0x2e}}
	iidMMDeviceEnumerator   = windows.GUID{Data1: 0xa95664d2, Data2: 0x9614, Data3: 0x4f35, Data4: [8]byte{0xa7, 0x46, 0xde, 0x8d, 0xb6, 0x36, 0x17, 0xe6}}
	iidAudioEndpointVolume  = windows.GUID{Data1: 0x5cdf2c82, Data2: 0x841e, Data3: 0x4546, Data4: [8]byte{0x97, 0x22, 0x0c, 0xf7, 0x40, 0x78, 0x22, 0x9a}}
)

const (
	clsctxAll = 0x17

	slotEnumDefaultEndpoint = 4 // IMMDeviceEnumerator.GetDefaultAudioEndpoint
	slotDeviceActivate      = 3 // IMMDevice.Activate
	slotVolSetScalar        = 7 // IAudioEndpointVolume.SetMasterVolumeLevelScalar
	slotVolGetScalar        = 9 // IAudioEndpointVolume.GetMasterVolumeLevelScalar

	eRender     = 0
	eMultimedia = 1
)

// Volume returns the master volume as a percentage 0-100.
func (m *Media) Volume(ctx context.Context) (float64, error) {
	var pct float64
	err := m.run(ctx, func() error {
		ep, err := endpointVolume()
		if err != nil {
			return err
		}
		defer release(ep)
		var v float32
		if hr := vcall(ep, slotVolGetScalar, uintptr(unsafe.Pointer(&v))); hr != 0 {
			return hrErr("GetMasterVolumeLevelScalar", hr)
		}
		pct = float64(v) * 100
		return nil
	})
	return pct, err
}

// SetVolume sets the master volume to pct (0-100).
func (m *Media) SetVolume(ctx context.Context, pct int) error {
	if pct < 0 || pct > 100 {
		return errors.New("volume out of range")
	}
	return m.run(ctx, func() error {
		ep, err := endpointVolume()
		if err != nil {
			return err
		}
		defer release(ep)
		// The float travels in the low 32 bits of the argument slot: the Go Windows
		// call stub copies the first four slots to both the integer and the XMM
		// registers, which is where the x64 ABI expects a float.
		bits := uintptr(math.Float32bits(float32(pct) / 100))
		return hrErr("SetMasterVolumeLevelScalar", vcall(ep, slotVolSetScalar, bits, 0))
	})
}

// endpointVolume returns the IAudioEndpointVolume of the default playback device.
func endpointVolume() (unsafe.Pointer, error) {
	var enum unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum)))
	if err := hrErr("CoCreateInstance(MMDeviceEnumerator)", hr); err != nil {
		return nil, err
	}
	defer release(enum)
	dev := comOut(enum, slotEnumDefaultEndpoint, eRender, eMultimedia)
	if dev == nil {
		return nil, errors.New("no default playback device")
	}
	defer release(dev)
	var ep unsafe.Pointer
	if hr := vcall(dev, slotDeviceActivate, uintptr(unsafe.Pointer(&iidAudioEndpointVolume)), clsctxAll, 0, uintptr(unsafe.Pointer(&ep))); hr != 0 {
		return nil, hrErr("IMMDevice.Activate", hr)
	}
	return ep, nil
}
