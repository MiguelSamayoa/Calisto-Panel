package httpapi

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MiguelSamayoa/calisto-panel/internal/actions"
)

const token = "s3cret-token-value"

type rig struct {
	h     http.Handler
	calls []string
	err   error
	logs  *bytes.Buffer
}

func newRig() *rig {
	r := &rig{logs: &bytes.Buffer{}}
	r.h = New(token, []byte("<html>panel</html>"),
		func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "stream") },
		func(id string) (actions.Started, error) {
			r.calls = append(r.calls, id)
			if r.err != nil {
				return actions.Started{}, r.err
			}
			return actions.Started{Exec: "01JF3K8S2M", Action: id, State: "running"}, nil
		},
		slog.New(slog.NewJSONHandler(r.logs, nil)))
	return r
}

func (r *rig) do(method, target, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

func TestPostAuthorization(t *testing.T) {
	cases := []struct {
		name, target, auth string
		wantStatus         int
		wantCalls          int
	}{
		{"no token", "/api/do/docker.start", "", 401, 0},
		{"wrong token", "/api/do/docker.start", "Bearer nope", 401, 0},
		{"token prefix only", "/api/do/docker.start", "Bearer s3cret", 401, 0},
		{"token plus suffix", "/api/do/docker.start", "Bearer " + token + "x", 401, 0},
		{"wrong scheme", "/api/do/docker.start", "Basic " + token, 401, 0},
		{"bare token", "/api/do/docker.start", token, 401, 0},
		{"token only in query string", "/api/do/docker.start?token=" + token, "", 401, 0},
		{"correct token", "/api/do/docker.start", "Bearer " + token, 202, 1},
	}
	for _, c := range cases {
		r := newRig()
		w := r.do("POST", c.target, c.auth)
		if w.Code != c.wantStatus || len(r.calls) != c.wantCalls {
			t.Errorf("%s: status %d, calls %d; want %d, %d", c.name, w.Code, len(r.calls), c.wantStatus, c.wantCalls)
		}
	}
}

func TestPostResponses(t *testing.T) {
	auth := "Bearer " + token
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{"accepted", nil, 202, `{"exec":"01JF3K8S2M","action":"docker.stop","state":"running"}`},
		{"unknown", actions.ErrUnknown, 404, `{"error":"unknown_action"}`},
		{"busy", &actions.AlreadyRunningError{Exec: "EXEC1"}, 409, `{"error":"already_running","exec":"EXEC1"}`},
		{"limited", &actions.RateLimitedError{RetryAfter: 1500 * time.Millisecond}, 429, `{"error":"rate_limited","retryAfterMs":1500}`},
		{"internal", errors.New("boom"), 500, `{"error":"internal"}`},
	}
	for _, c := range cases {
		r := newRig()
		r.err = c.err
		w := r.do("POST", "/api/do/docker.stop", auth)
		if w.Code != c.wantStatus || strings.TrimSpace(w.Body.String()) != c.wantBody {
			t.Errorf("%s: %d %s; want %d %s", c.name, w.Code, w.Body, c.wantStatus, c.wantBody)
		}
		if c.name == "limited" && w.Header().Get("Retry-After") != "2" {
			t.Errorf("Retry-After = %q, want 2", w.Header().Get("Retry-After"))
		}
	}
}

func TestUnauthorizedNeverReachesTheExecutorNorLeaksToken(t *testing.T) {
	r := newRig()
	r.do("POST", "/api/do/not.an.action", "Bearer wrong-but-secret-looking")
	r.do("POST", "/api/do/docker.start", "Bearer "+token)
	if len(r.calls) != 1 {
		t.Errorf("calls = %v", r.calls)
	}
	if strings.Contains(r.logs.String(), token) || strings.Contains(r.logs.String(), "wrong-but-secret-looking") {
		t.Errorf("a token reached the log: %s", r.logs)
	}
}

func TestPublicGETsNeedNoAuth(t *testing.T) {
	r := newRig()
	for target, want := range map[string]string{
		"/":           "<html>panel</html>",
		"/healthz":    `{"ok":true}`,
		"/api/stream": "stream",
	} {
		w := r.do("GET", target, "")
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
			t.Errorf("GET %s = %d %q, want 200 %q", target, w.Code, w.Body, want)
		}
	}
}

func TestMethodsAndPaths(t *testing.T) {
	r := newRig()
	auth := "Bearer " + token
	if w := r.do("GET", "/api/do/docker.start", auth); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on action = %d, want 405", w.Code)
	}
	if w := r.do("POST", "/healthz", auth); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", w.Code)
	}
	if w := r.do("GET", "/nothing", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET /nothing = %d, want 404", w.Code)
	}
	if len(r.calls) != 0 {
		t.Errorf("executor called: %v", r.calls)
	}
}
