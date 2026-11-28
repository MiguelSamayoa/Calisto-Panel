package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
	"github.com/MiguelSamayoa/calisto-panel/internal/winutil"
)

const (
	mediaReadTimeout = 3 * time.Second
	// After a media action the source app needs a moment to publish its new state.
	mediaPokeFirst  = 250 * time.Millisecond
	mediaPokeSecond = 700 * time.Millisecond
	// Chrome can take over a second to publish a pause or play.
	mediaPokeThird = 1500 * time.Millisecond
	// A source often publishes the cover a moment after the title; ask again on the next
	// polls, but give up after this many tries so a track without cover costs nothing.
	coverTries = 5
)

func mediaUpdate(i winutil.MediaInfo) hub.MediaUpdate {
	return hub.MediaUpdate{
		Active: i.Active, Status: i.Status, App: i.App, SID: sessionID(i),
		Title: i.Title, Artist: i.Artist, Album: i.Album,
		Position: i.Position.Seconds(), Duration: i.Duration.Seconds(),
	}
}

// mediaMore reads the full session list and the per-app volumes. A failure of either
// leaves that list empty: the primary card keeps working.
func mediaMore(m *winutil.Media) func(context.Context) ([]hub.MediaSession, []hub.AudioApp) {
	return func(ctx context.Context) ([]hub.MediaSession, []hub.AudioApp) {
		var ss []hub.MediaSession
		if list, err := m.Sessions(ctx); err == nil {
			for _, s := range list {
				ss = append(ss, hub.MediaSession{ID: s.ID, App: s.App, Status: s.Status, Title: s.Title, Artist: s.Artist,
					Position: s.Position.Seconds(), Duration: s.Duration.Seconds()})
			}
		}
		var as []hub.AudioApp
		if list, err := m.Apps(ctx); err == nil {
			for _, a := range list {
				as = append(as, hub.AudioApp{ID: a.ID, Name: a.Name, Volume: a.Volume})
			}
		}
		return ss, as
	}
}

// pollMedia reads the current media session every cfg.IntervalMs, and again shortly
// after poke fires (a media action just ran), so a button press shows at once.
func pollMedia(ctx context.Context, cfg config.Media, read func(context.Context) (winutil.MediaInfo, error), readCover func(context.Context) ([]byte, error), readVolume func(context.Context) (float64, error), readMore func(context.Context) ([]hub.MediaSession, []hub.AudioApp), poke <-chan struct{}, h *hub.Hub, log *slog.Logger) {
	failing := false
	var trackKey, coverID string
	var coverImg []byte
	var tries int
	tick := func() {
		rctx, cancel := context.WithTimeout(ctx, mediaReadTimeout)
		defer cancel()
		info, err := read(rctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !failing {
				log.Warn("reading media session", "err", err)
			}
			failing = true
			h.UpdateMedia(hub.MediaUpdate{Fault: hub.StateError})
			return
		}
		failing = false
		u := mediaUpdate(info)
		if key := info.App + "\x00" + info.Title + "\x00" + info.Artist + "\x00" + info.Album; key != trackKey || !info.Active {
			trackKey, coverID, coverImg, tries = key, "", nil, 0
		}
		if info.Active && readCover != nil && coverImg == nil && tries < coverTries {
			tries++
			if img, err := readCover(rctx); err != nil {
				log.Warn("reading media cover", "err", err)
			} else if len(img) > 0 {
				sum := sha256.Sum256(img)
				coverImg, coverID = img, hex.EncodeToString(sum[:6])
			}
		}
		u.Cover, u.CoverImg = coverID, coverImg
		u.Volume = -1
		if readVolume != nil {
			if v, err := readVolume(rctx); err == nil {
				u.Volume = v
			}
		}
		if readMore != nil {
			u.Sessions, u.Apps = readMore(rctx)
		}
		h.UpdateMedia(u)
	}
	wait := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
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
		case <-poke:
			if wait(mediaPokeFirst) {
				tick()
			}
			if wait(mediaPokeSecond - mediaPokeFirst) {
				tick()
			}
			if wait(mediaPokeThird - mediaPokeSecond) {
				tick()
			}
		}
	}
}

// sessionID is the id the session list uses for the current session ("" when none).
func sessionID(i winutil.MediaInfo) string {
	if !i.Active || i.App == "" {
		return ""
	}
	return winutil.SessionID(i.App)
}
