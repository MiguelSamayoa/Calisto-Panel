// Package httpapi wires the public contract: GET routes are open, every POST needs
// the bearer token, and so does the container log view (logs can hold secrets).
// Per-action concurrency lives in actions.Executor, which answers 409.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/actions"
)

const maxBody = 1 << 10 // POST has no meaningful body

// coverTypes are the only content types the cover endpoint will serve.
var coverTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true,
}

// ErrNotFound is what a LogsFunc returns for a project or container the server does
// not know.
var ErrNotFound = errors.New("not found")

// Kinds of log target.
const (
	LogsProject   = "project"
	LogsContainer = "container"
)

// LogsFunc returns the JSON body of the log view of one compose project or one
// container (kind is LogsProject or LogsContainer; name is its name or short id).
type LogsFunc func(ctx context.Context, kind, name string) ([]byte, error)

type Server struct {
	token  [sha256.Size]byte
	index  []byte
	stream http.Handler
	do     func(id string) (actions.Started, error)
	logs   LogsFunc
	cover  func() []byte // encoded cover of the current track, nil when none
	log    *slog.Logger
}

// New returns the router. token is the api_token; only its hash is kept. do is
// normally (*actions.Executor).Do and stream is (*hub.Hub).ServeStream.
func New(token string, index []byte, stream http.HandlerFunc, do func(string) (actions.Started, error), logs LogsFunc, cover func() []byte, log *slog.Logger) http.Handler {
	s := &Server{token: sha256.Sum256([]byte(token)), index: index, stream: stream, do: do, logs: logs, cover: cover, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.Handle("GET /api/stream", s.logStream(s.stream))
	mux.HandleFunc("GET /api/logs/container/{name}", s.handleLogs(LogsContainer))
	mux.HandleFunc("GET /api/logs/{name}", s.handleLogs(LogsProject))
	mux.HandleFunc("GET /api/media/cover", s.handleCover)
	mux.HandleFunc("POST /api/do/{id}", s.handleDo)
	return mux
}

// handleCover serves the cover of the current track. The page asks for it with
// ?v=<cover id from the media event>, so a cached copy never outlives its track. No
// auth, like the other read-only GETs.
func (s *Server) handleCover(w http.ResponseWriter, _ *http.Request) {
	var img []byte
	if s.cover != nil {
		img = s.cover()
	}
	// The bytes come from whatever app publishes the thumbnail, so only a sniffed image
	// type is served; anything else (HTML, say) would run in the panel's origin.
	ct := http.DetectContentType(img)
	if len(img) == 0 || !coverTypes[ct] {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(img)
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(s.index)
}

// authorized checks the Authorization header only: a token in the URL is never read.
// Both sides are hashed first so the comparison is constant-time even across lengths.
func (s *Server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(sum[:], s.token[:]) == 1
}

func (s *Server) handleDo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	id := r.PathValue("id")

	if !s.authorized(r) {
		s.log.Warn("unauthorized action request", "remote", r.RemoteAddr, "action", id)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	started, err := s.do(id)
	var busy *actions.AlreadyRunningError
	switch {
	case err == nil:
		s.log.Info("action accepted", "remote", r.RemoteAddr, "action", id, "exec", started.Exec)
		writeJSON(w, http.StatusAccepted, started)
	case errors.Is(err, actions.ErrUnknown):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown_action"})
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already_running", "exec": busy.Exec})
	default:
		s.log.Error("action dispatch failed", "action", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
}

// handleLogs serves the recent logs of a compose project (merged by time) or of one
// container. Unlike the other GETs it needs the bearer token: container logs routinely
// contain secrets.
func (s *Server) handleLogs(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !s.authorized(r) {
			s.log.Warn("unauthorized logs request", "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		body, err := s.logs(r.Context(), kind, name)
		switch {
		case err == nil:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(body)
		case errors.Is(err, ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		default:
			s.log.Error("reading logs", "kind", kind, "name", name, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		}
	}
}

// logStream records when each phone connection opens and closes and how long it lasted.
// It is the evidence for telling a phone that dropped the connection (short, frequent
// closes) from a server that never saw it again.
func (s *Server) logStream(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s.log.Info("stream opened", "remote", r.RemoteAddr)
		next.ServeHTTP(w, r)
		s.log.Info("stream closed", "remote", r.RemoteAddr, "duration", time.Since(start).Round(time.Second).String())
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
