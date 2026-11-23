package winutil

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	inputKeyboard  = 1
	keyEventKeyUp  = 0x0002
	keyInputLength = 2
)

// keyInput mirrors the Win32 INPUT structure for a keyboard event on amd64:
// 4-byte type, 4 bytes padding, then the union (sized by MOUSEINPUT, 32 bytes).
type keyInput struct {
	typ       uint32
	_         uint32
	vk        uint16
	scan      uint16
	flags     uint32
	time      uint32
	_         uint32
	extraInfo uintptr
	_         [8]byte
}

// x/sys/windows has no SendInput wrapper, so it is declared here.
var procSendInput = windows.NewLazySystemDLL("user32.dll").NewProc("SendInput")

// SendMediaKey presses and releases one virtual key (VK_MEDIA_PLAY_PAUSE, etc.).
// It only reaches applications when the process runs in the interactive session.
func SendMediaKey(vk uint8) error {
	inputs := [keyInputLength]keyInput{
		{typ: inputKeyboard, vk: uint16(vk)},
		{typ: inputKeyboard, vk: uint16(vk), flags: keyEventKeyUp},
	}
	n, _, err := procSendInput.Call(
		uintptr(keyInputLength),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if n != keyInputLength {
		return fmt.Errorf("SendInput sent %d of %d events: %w", n, keyInputLength, err)
	}
	return nil
}
