package winutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// Every SMTC session at once (GetSessions) and per-session transport controls. A
// session is addressed by SessionID(app): stable across polls, unlike a list index.

const (
	slotMgrSessions  = 7  // ISessionManager.GetSessions -> IVectorView<Session>
	slotVecGetAt     = 6  // IVectorView.GetAt
	slotVecSize      = 7  // IVectorView.get_Size
	slotSessSkipNext = 16 // TrySkipNextAsync
	slotSessSkipPrev = 17 // TrySkipPreviousAsync
	slotSessToggle   = 20 // TryTogglePlayPauseAsync
)

// SessionInfo is one entry of the session list; ID names it in SessionDo.
type SessionInfo struct {
	ID string
	MediaInfo
}

// SessionID is the short stable id of a session, derived from its source app.
func SessionID(app string) string { return shortHash(app) }

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// Sessions returns every media session Windows knows, deduplicated by source app.
func (m *Media) Sessions(ctx context.Context) ([]SessionInfo, error) {
	var out []SessionInfo
	err := m.run(ctx, func() error {
		return eachSession(func(sess unsafe.Pointer) bool {
			info := readInfo(sess)
			id := SessionID(info.App)
			for _, o := range out {
				if o.ID == id {
					return true
				}
			}
			out = append(out, SessionInfo{ID: id, MediaInfo: info})
			return true
		})
	})
	return out, err
}

// SessionDo runs a transport verb (playpause, prev, next, seek_back, seek_fwd) on the
// session with this id.
func (m *Media) SessionDo(ctx context.Context, id, verb string) error {
	return m.run(ctx, func() error {
		found := false
		var doErr error
		err := eachSession(func(sess unsafe.Pointer) bool {
			app, _ := hstringGetter(sess, slotSessApp)
			if SessionID(app) != id {
				return true
			}
			found = true
			doErr = sessionVerb(sess, verb)
			return false
		})
		if err != nil {
			return err
		}
		if !found {
			return errNoSession
		}
		return doErr
	})
}

func sessionVerb(sess unsafe.Pointer, verb string) error {
	slot := 0
	switch verb {
	case "playpause":
		slot = slotSessToggle
	case "prev":
		slot = slotSessSkipPrev
	case "next":
		slot = slotSessSkipNext
	case "seek_back":
		return seekOn(sess, -10*time.Second)
	case "seek_fwd":
		return seekOn(sess, 10*time.Second)
	default:
		return fmt.Errorf("unknown session verb %q", verb)
	}
	op := comOut(sess, slot)
	if op == nil {
		return errors.New("media source refused the command")
	}
	defer release(op)
	var ok uint8
	if err := awaitResult(op, unsafe.Pointer(&ok)); err != nil {
		return err
	}
	if ok == 0 {
		return errors.New("media source does not support that command")
	}
	return nil
}

// eachSession calls f for every session until f returns false. Runs on the WinRT thread.
func eachSession(f func(sess unsafe.Pointer) bool) error {
	if sessionMgr == nil {
		mgr, err := sessionManager()
		if err != nil {
			return fmt.Errorf("media session manager: %w", err)
		}
		sessionMgr = mgr
	}
	vec := comOut(sessionMgr, slotMgrSessions)
	if vec == nil {
		return nil
	}
	defer release(vec)
	var n uint32
	if hr := vcall(vec, slotVecSize, uintptr(unsafe.Pointer(&n))); hr != 0 {
		return hrErr("IVectorView.Size", hr)
	}
	for i := uint32(0); i < n; i++ {
		sess := comOut(vec, slotVecGetAt, uintptr(i))
		if sess == nil {
			continue
		}
		cont := f(sess)
		release(sess)
		if !cont {
			break
		}
	}
	return nil
}
