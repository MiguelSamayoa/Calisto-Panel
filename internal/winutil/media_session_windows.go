package winutil

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file reads and seeks the current Windows media session (SMTC, the same source
// the volume flyout uses) with raw WinRT calls: no cgo and no extra module. The IIDs
// and vtable slots below come from Windows.Media.Control.winmd; every interface starts
// with the six IInspectable slots, so the first interface method is slot 6.

var (
	combase                  = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize         = combase.NewProc("RoInitialize")
	procRoGetActivationFact  = combase.NewProc("RoGetActivationFactory")
	procWindowsCreateString  = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString  = combase.NewProc("WindowsDeleteString")
	procWindowsGetStringBuf  = combase.NewProc("WindowsGetStringRawBuffer")
	iidSessionManagerStatics = windows.GUID{Data1: 0x2050c4ee, Data2: 0x11a0, Data3: 0x57de, Data4: [8]byte{0xae, 0xd7, 0xc9, 0x7c, 0x70, 0x33, 0x82, 0x45}}
	iidBufferFactory         = windows.GUID{Data1: 0x71af914d, Data2: 0xc10f, Data3: 0x484b, Data4: [8]byte{0xbc, 0x50, 0x14, 0xbc, 0x62, 0x3b, 0x3a, 0x27}}
	iidRandomAccessStream    = windows.GUID{Data1: 0x905a0fe1, Data2: 0xbc53, Data3: 0x11df, Data4: [8]byte{0x8c, 0x49, 0x00, 0x1e, 0x4f, 0xc6, 0x86, 0xda}}
	iidInputStream           = windows.GUID{Data1: 0x905a0fe2, Data2: 0xbc53, Data3: 0x11df, Data4: [8]byte{0x8c, 0x49, 0x00, 0x1e, 0x4f, 0xc6, 0x86, 0xda}}
	iidBufferByteAccess      = windows.GUID{Data1: 0x905a0fef, Data2: 0xbc53, Data3: 0x11df, Data4: [8]byte{0x8c, 0x49, 0x00, 0x1e, 0x4f, 0xc6, 0x86, 0xda}}
	iidAsyncInfo             = windows.GUID{Data1: 0x00000036, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

const (
	managerClass = "Windows.Media.Control.GlobalSystemMediaTransportControlsSessionManager"
	bufferClass  = "Windows.Storage.Streams.Buffer"

	// Largest cover accepted; a bigger thumbnail is treated as "no cover".
	maxCoverBytes = 2 << 20

	roInitMultithreaded = 1
	asyncStarted        = 0
	asyncCompleted      = 1
	asyncWait           = 3 * time.Second

	// Slots of the interfaces we call.
	slotQueryInterface = 0
	slotRelease        = 2

	slotStaticsRequestAsync = 6
	slotMgrCurrentSession   = 6

	slotSessApp          = 6
	slotSessProps        = 7
	slotSessTimeline     = 8
	slotSessPlayback     = 9
	slotSessChangePos    = 24
	slotPropsTitle       = 6
	slotPropsArtist      = 9
	slotPropsAlbum       = 10
	slotPropsThumbnail   = 15
	slotRefOpenRead      = 6 // IRandomAccessStreamReference.OpenReadAsync
	slotStreamSize       = 6 // IRandomAccessStream.Size
	slotInputReadAsync   = 6 // IInputStream.ReadAsync
	slotBufferFactory    = 6 // IBufferFactory.Create
	slotBufferLength     = 7 // IBuffer.Length
	slotByteAccessBuffer = 3 // IBufferByteAccess.Buffer (plain COM, after IUnknown)
	slotPlaybackStatus   = 7
	slotTimelineStart    = 6
	slotTimelineEnd      = 7
	slotTimelinePosition = 10
	slotTimelineUpdated  = 11

	slotAsyncInfoStatus = 7
	slotAsyncResults    = 8  // IAsyncOperation<T>.GetResults
	slotProgressResults = 10 // IAsyncOperationWithProgress<T,P>.GetResults

	// Windows.Media.Control.GlobalSystemMediaTransportControlsSessionPlaybackStatus
	statusStopped = 3
	statusPlaying = 4
	statusPaused  = 5

	ticksPerSecond   = 10_000_000
	filetimeUnixDiff = 116444736000000000 // 100 ns ticks between 1601-01-01 and 1970-01-01
)

// MediaInfo is one reading of the current media session. A zero value (Active false)
// means nothing is registered with Windows.
type MediaInfo struct {
	Active   bool
	Status   string // playing | paused | stopped | other
	App      string // AppUserModelId of the source, e.g. "Spotify.exe"
	Title    string
	Artist   string
	Album    string
	Position time.Duration // extrapolated to the moment of the reading
	Duration time.Duration // 0 when the source does not report one (live streams)
}

// Media owns a dedicated OS thread where every WinRT call runs: the apartment is set
// up once for it and never torn down while the process lives.
type Media struct {
	reqs chan func()
	done chan struct{}
}

// StartMedia starts the thread; it stops when ctx ends.
func StartMedia(ctx context.Context) *Media {
	m := &Media{reqs: make(chan func()), done: make(chan struct{})}
	go m.loop(ctx)
	return m
}

func (m *Media) loop(ctx context.Context) {
	defer close(m.done)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// S_FALSE (1) means "already initialised on this thread"; a failure here surfaces
	// as an error from the first call that needs WinRT.
	procRoInitialize.Call(roInitMultithreaded)
	defer func() { release(sessionMgr); sessionMgr = nil }()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-m.reqs:
			f()
		}
	}
}

// run executes f on the WinRT thread and waits for it.
func (m *Media) run(ctx context.Context, f func() error) error {
	res := make(chan error, 1)
	task := func() { res <- f() }
	select {
	case m.reqs <- task:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return errors.New("media session thread stopped")
	}
	select {
	case err := <-res:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Read returns the current session, or Active=false when there is none.
func (m *Media) Read(ctx context.Context) (MediaInfo, error) {
	var info MediaInfo
	err := m.run(ctx, func() error {
		var err error
		info, err = readSession()
		return err
	})
	return info, err
}

// Cover returns the encoded thumbnail (PNG or JPEG) of the current session, or nil
// when there is no session or the source published no image.
func (m *Media) Cover(ctx context.Context) ([]byte, error) {
	var img []byte
	err := m.run(ctx, func() error {
		var err error
		img, err = readCover()
		return err
	})
	return img, err
}

// SeekBy moves the playback position by d (negative goes back), clamped to the track.
func (m *Media) SeekBy(ctx context.Context, d time.Duration) error {
	return m.run(ctx, func() error { return seekSession(d) })
}

// ---- everything below runs on the WinRT thread ----

var errNoSession = errors.New("no active media session")

func readSession() (MediaInfo, error) {
	sess, err := currentSession()
	if err != nil || sess == nil {
		return MediaInfo{}, err
	}
	defer release(sess)

	info := MediaInfo{Active: true, Status: "other"}
	info.App, _ = hstringGetter(sess, slotSessApp)

	if pb := comOut(sess, slotSessPlayback); pb != nil {
		var st int32
		if hr := vcall(pb, slotPlaybackStatus, uintptr(unsafe.Pointer(&st))); hr == 0 {
			switch st {
			case statusPlaying:
				info.Status = "playing"
			case statusPaused:
				info.Status = "paused"
			case statusStopped:
				info.Status = "stopped"
			}
		}
		release(pb)
	}

	if op := comOut(sess, slotSessProps); op != nil {
		if props, err := awaitPointer(op); err == nil && props != nil {
			info.Title, _ = hstringGetter(props, slotPropsTitle)
			info.Artist, _ = hstringGetter(props, slotPropsArtist)
			info.Album, _ = hstringGetter(props, slotPropsAlbum)
			release(props)
		}
		release(op)
	}

	if tl := comOut(sess, slotSessTimeline); tl != nil {
		start, end, pos, ok := readTimeline(tl, info.Status == "playing")
		if ok {
			info.Position, info.Duration = pos-start, end-start
			if info.Duration < 0 {
				info.Duration = 0
			}
		}
		release(tl)
	}
	return info, nil
}

// readCover reads the whole thumbnail stream of the current session into memory.
func readCover() ([]byte, error) {
	sess, err := currentSession()
	if err != nil || sess == nil {
		return nil, err
	}
	defer release(sess)
	op := comOut(sess, slotSessProps)
	if op == nil {
		return nil, nil
	}
	defer release(op)
	props, err := awaitPointer(op)
	if err != nil || props == nil {
		return nil, err
	}
	defer release(props)
	ref := comOut(props, slotPropsThumbnail)
	if ref == nil {
		return nil, nil // the source published no image
	}
	defer release(ref)
	open := comOut(ref, slotRefOpenRead)
	if open == nil {
		return nil, nil
	}
	defer release(open)
	stream, err := awaitPointer(open)
	if err != nil || stream == nil {
		return nil, err
	}
	defer release(stream)

	var ras, in unsafe.Pointer
	if hr := vcall(stream, slotQueryInterface, uintptr(unsafe.Pointer(&iidRandomAccessStream)), uintptr(unsafe.Pointer(&ras))); hr != 0 {
		return nil, hrErr("QueryInterface(IRandomAccessStream)", hr)
	}
	defer release(ras)
	if hr := vcall(stream, slotQueryInterface, uintptr(unsafe.Pointer(&iidInputStream)), uintptr(unsafe.Pointer(&in))); hr != 0 {
		return nil, hrErr("QueryInterface(IInputStream)", hr)
	}
	defer release(in)
	var size uint64
	if hr := vcall(ras, slotStreamSize, uintptr(unsafe.Pointer(&size))); hr != 0 {
		return nil, hrErr("IRandomAccessStream.Size", hr)
	}
	if size == 0 || size > maxCoverBytes {
		return nil, nil
	}

	cls, err := newHString(bufferClass)
	if err != nil {
		return nil, err
	}
	defer deleteHString(cls)
	var factory unsafe.Pointer
	hr, _, _ := procRoGetActivationFact.Call(uintptr(cls), uintptr(unsafe.Pointer(&iidBufferFactory)), uintptr(unsafe.Pointer(&factory)))
	if err := hrErr("RoGetActivationFactory(Buffer)", hr); err != nil {
		return nil, err
	}
	defer release(factory)
	buf := comOut(factory, slotBufferFactory, uintptr(size))
	if buf == nil {
		return nil, errors.New("creating cover buffer")
	}
	defer release(buf)

	read := comOut(in, slotInputReadAsync, uintptr(buf), uintptr(size), 0)
	if read == nil {
		return nil, errors.New("starting cover read")
	}
	defer release(read)
	var filled unsafe.Pointer
	if err := awaitResultAt(read, unsafe.Pointer(&filled), slotProgressResults); err != nil {
		return nil, err
	}
	defer release(filled)

	var n uint32
	if hr := vcall(filled, slotBufferLength, uintptr(unsafe.Pointer(&n))); hr != 0 {
		return nil, hrErr("IBuffer.Length", hr)
	}
	if n == 0 {
		return nil, nil
	}
	var access unsafe.Pointer
	if hr := vcall(filled, slotQueryInterface, uintptr(unsafe.Pointer(&iidBufferByteAccess)), uintptr(unsafe.Pointer(&access))); hr != 0 {
		return nil, hrErr("QueryInterface(IBufferByteAccess)", hr)
	}
	defer release(access)
	var raw uintptr
	if hr := vcall(access, slotByteAccessBuffer, uintptr(unsafe.Pointer(&raw))); hr != 0 || raw == 0 {
		return nil, hrErr("IBufferByteAccess.Buffer", hr)
	}
	return append([]byte(nil), unsafe.Slice((*byte)(ptrOf(raw)), n)...), nil
}

// readTimeline returns start, end and the position extrapolated to now. Sources
// publish the position only when it changes (play, pause, seek), so while playing the
// elapsed time since LastUpdatedTime is added, capped at the end of the track.
func readTimeline(tl unsafe.Pointer, playing bool) (start, end, pos time.Duration, ok bool) {
	var s, e, p, updated int64
	if vcall(tl, slotTimelineStart, uintptr(unsafe.Pointer(&s))) != 0 ||
		vcall(tl, slotTimelineEnd, uintptr(unsafe.Pointer(&e))) != 0 ||
		vcall(tl, slotTimelinePosition, uintptr(unsafe.Pointer(&p))) != 0 {
		return 0, 0, 0, false
	}
	start, end, pos = ticks(s), ticks(e), ticks(p)
	if playing && vcall(tl, slotTimelineUpdated, uintptr(unsafe.Pointer(&updated))) == 0 && updated > 0 {
		last := time.Unix(0, (updated-filetimeUnixDiff)*100)
		if el := time.Since(last); el > 0 {
			pos += el
		}
		if end > start && pos > end {
			pos = end
		}
	}
	return start, end, pos, true
}

func seekSession(d time.Duration) error {
	sess, err := currentSession()
	if err != nil {
		return err
	}
	if sess == nil {
		return errNoSession
	}
	defer release(sess)

	tl := comOut(sess, slotSessTimeline)
	if tl == nil {
		return errors.New("media session has no timeline")
	}
	defer release(tl)
	playing := false
	if pb := comOut(sess, slotSessPlayback); pb != nil {
		var st int32
		playing = vcall(pb, slotPlaybackStatus, uintptr(unsafe.Pointer(&st))) == 0 && st == statusPlaying
		release(pb)
	}
	start, end, pos, ok := readTimeline(tl, playing)
	if !ok {
		return errors.New("reading media timeline")
	}
	target := pos + d
	if target < start {
		target = start
	}
	if end > start && target > end {
		target = end
	}

	op := comOut(sess, slotSessChangePos, uintptr(target.Nanoseconds()/100))
	if op == nil {
		return errors.New("media source refused the seek")
	}
	defer release(op)
	var okByte uint8
	if err := awaitResult(op, unsafe.Pointer(&okByte)); err != nil {
		return err
	}
	if okByte == 0 {
		return errors.New("media source does not support seeking")
	}
	return nil
}

// sessionManager activates the SMTC session manager and waits for it.
func sessionManager() (unsafe.Pointer, error) {
	cls, err := newHString(managerClass)
	if err != nil {
		return nil, err
	}
	defer deleteHString(cls)
	var statics unsafe.Pointer
	hr, _, _ := procRoGetActivationFact.Call(uintptr(cls), uintptr(unsafe.Pointer(&iidSessionManagerStatics)), uintptr(unsafe.Pointer(&statics)))
	if err := hrErr("RoGetActivationFactory", hr); err != nil {
		return nil, err
	}
	defer release(statics)
	op := comOut(statics, slotStaticsRequestAsync)
	if op == nil {
		return nil, errors.New("RequestAsync returned nothing")
	}
	defer release(op)
	return awaitPointer(op)
}

// sessionMgr is touched only from the WinRT thread.
var sessionMgr unsafe.Pointer

func currentSession() (unsafe.Pointer, error) {
	if sessionMgr == nil {
		mgr, err := sessionManager()
		if err != nil {
			return nil, fmt.Errorf("media session manager: %w", err)
		}
		sessionMgr = mgr
	}
	return comOut(sessionMgr, slotMgrCurrentSession), nil
}

// ---- tiny COM / WinRT helpers ----

// ptrOf reinterprets a raw address returned by the OS as a pointer. The memory is
// owned by COM, not the Go heap, so the garbage collector never sees it.
func ptrOf(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

// vcall calls slot idx of obj's vtable and returns the HRESULT.
func vcall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, idx*int(unsafe.Sizeof(uintptr(0)))))
	hr, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return hr
}

// comOut calls a method whose last parameter is an out interface pointer and returns
// that pointer, or nil on failure or when the method yielded nothing.
func comOut(obj unsafe.Pointer, idx int, args ...uintptr) unsafe.Pointer {
	var out unsafe.Pointer
	if vcall(obj, idx, append(args, uintptr(unsafe.Pointer(&out)))...) != 0 {
		return nil
	}
	return out
}

func release(obj unsafe.Pointer) {
	if obj != nil {
		vcall(obj, slotRelease)
	}
}

// awaitResult waits for an IAsyncOperation and copies its result into out.
func awaitResult(op, out unsafe.Pointer) error { return awaitResultAt(op, out, slotAsyncResults) }

// awaitResultAt is awaitResult for an operation whose GetResults sits in another slot
// (IAsyncOperationWithProgress has two more members before it).
func awaitResultAt(op, out unsafe.Pointer, resultsSlot int) error {
	var info unsafe.Pointer
	if hr := vcall(op, slotQueryInterface, uintptr(unsafe.Pointer(&iidAsyncInfo)), uintptr(unsafe.Pointer(&info))); hr != 0 {
		return hrErr("QueryInterface(IAsyncInfo)", hr)
	}
	defer release(info)
	deadline := time.Now().Add(asyncWait)
	for {
		var st int32
		if hr := vcall(info, slotAsyncInfoStatus, uintptr(unsafe.Pointer(&st))); hr != 0 {
			return hrErr("IAsyncInfo.Status", hr)
		}
		if st == asyncCompleted {
			break
		}
		if st != asyncStarted {
			return fmt.Errorf("async operation ended with status %d", st)
		}
		if time.Now().After(deadline) {
			return errors.New("async operation timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return hrErr("GetResults", vcall(op, resultsSlot, uintptr(out)))
}

// awaitPointer waits for an IAsyncOperation<T> whose result is an interface pointer.
func awaitPointer(op unsafe.Pointer) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	err := awaitResult(op, unsafe.Pointer(&out))
	return out, err
}

func hrErr(what string, hr uintptr) error {
	if int32(hr) < 0 {
		return fmt.Errorf("%s: HRESULT 0x%08x", what, uint32(hr))
	}
	return nil
}

func ticks(t int64) time.Duration { return time.Duration(t) * 100 }

func newHString(s string) (uintptr, error) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	var h uintptr
	hr, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&h)))
	return h, hrErr("WindowsCreateString", hr)
}

func deleteHString(h uintptr) { procWindowsDeleteString.Call(h) }

// hstringGetter calls a getter with an HSTRING out parameter and returns it as a string.
func hstringGetter(obj unsafe.Pointer, idx int) (string, error) {
	var h uintptr
	if hr := vcall(obj, idx, uintptr(unsafe.Pointer(&h))); hr != 0 {
		return "", hrErr("getter", hr)
	}
	if h == 0 {
		return "", nil
	}
	defer deleteHString(h)
	var n uint32
	raw, _, _ := procWindowsGetStringBuf.Call(h, uintptr(unsafe.Pointer(&n)))
	if raw == 0 || n == 0 {
		return "", nil
	}
	return windows.UTF16ToString(unsafe.Slice((*uint16)(ptrOf(raw)), n)), nil
}
