// Package httpapi wires the public contract: GET routes are open, every POST needs
// the bearer token. Rate limiting and per-action concurrency live in actions.Executor,
// which already answers 409 and 429.
package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/actions"
)

const maxBody = 1 << 10 // POST has no meaningful body

type Server struct {
	token  [sha256.Size]byte
	index  []byte
	stream http.Handler
	do     func(id string) (actions.Started, error)
	log    *slog.Logger
}

// New returns the router. token is the api_token; only its hash is kept. do is
// normally (*actions.Executor).Do and stream is (*hub.Hub).ServeStream.
func New(token string, index []byte, stream http.HandlerFunc, do func(string) (actions.Started, error), log *slog.Logger) http.Handler {
	s := &Server{token: sha256.Sum256([]byte(token)), index: index, stream: stream, do: do, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.Handle("GET /api/stream", s.stream)
	mux.HandleFunc("POST /api/do/{id}", s.handleDo)
	return mux
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
	var limited *actions.RateLimitedError
	switch {
	case err == nil:
		s.log.Info("action accepted", "remote", r.RemoteAddr, "action", id, "exec", started.Exec)
		writeJSON(w, http.StatusAccepted, started)
	case errors.Is(err, actions.ErrUnknown):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown_action"})
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already_running", "exec": busy.Exec})
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int((limited.RetryAfter+time.Second-1)/time.Second)))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate_limited", "retryAfterMs": limited.RetryAfter.Milliseconds()})
	default:
		s.log.Error("action dispatch failed", "action", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
