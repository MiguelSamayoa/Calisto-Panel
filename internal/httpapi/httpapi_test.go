package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MiguelSamayoa/calisto-panel/internal/actions"
)

const token = "s3cret-token-value"

type rig struct {
	h        http.Handler
	calls    []string
	err      error
	logs     *bytes.Buffer
	logCalls []string // "<kind>/<name>"
	logsBody string
	logsErr  error
	cover    []byte
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
		func(_ context.Context, kind, name string) ([]byte, error) {
			r.logCalls = append(r.logCalls, kind+"/"+name)
			return []byte(r.logsBody), r.logsErr
		},
		func() []byte { return r.cover },
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
		{"internal", errors.New("boom"), 500, `{"error":"internal"}`},
	}
	for _, c := range cases {
		r := newRig()
		r.err = c.err
		w := r.do("POST", "/api/do/docker.stop", auth)
		if w.Code != c.wantStatus || strings.TrimSpace(w.Body.String()) != c.wantBody {
			t.Errorf("%s: %d %s; want %d %s", c.name, w.Code, w.Body, c.wantStatus, c.wantBody)
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

func TestLogsNeedTheBearerToken(t *testing.T) {
	for _, c := range []struct {
		name, auth string
		want       int
		wantCalls  int
	}{
		{"no token", "", 401, 0},
		{"wrong token", "Bearer nope", 401, 0},
		{"correct token", "Bearer " + token, 200, 1},
	} {
		r := newRig()
		r.logsBody = `{"lines":[]}`
		w := r.do("GET", "/api/logs/forms-dev", c.auth)
		if w.Code != c.want || len(r.logCalls) != c.wantCalls {
			t.Errorf("%s: status %d, calls %d; want %d, %d", c.name, w.Code, len(r.logCalls), c.want, c.wantCalls)
		}
	}
	r := newRig()
	if w := r.do("GET", "/api/logs/forms-dev?token="+token, ""); w.Code != 401 {
		t.Errorf("a token in the query string must not authorize: %d", w.Code)
	}
}

func TestLogsResponses(t *testing.T) {
	auth := "Bearer " + token
	r := newRig()
	r.logsBody = `{"project":"forms-dev","lines":[]}`
	w := r.do("GET", "/api/logs/forms-dev", auth)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != r.logsBody || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("ok: %d %s %q", w.Code, w.Body, w.Header().Get("Cache-Control"))
	}
	r = newRig()
	r.logsErr = ErrNotFound
	if w := r.do("GET", "/api/logs/nope", auth); w.Code != 404 || !strings.Contains(w.Body.String(), "not_found") {
		t.Errorf("unknown: %d %s", w.Code, w.Body)
	}
	r = newRig()
	r.logsErr = errors.New("boom")
	if w := r.do("GET", "/api/logs/x", auth); w.Code != 500 || strings.Contains(w.Body.String(), "boom") {
		t.Errorf("internal: %d %s (the error text must not leak)", w.Code, w.Body)
	}
}

func TestLogsRoutesByKind(t *testing.T) {
	auth := "Bearer " + token
	for target, want := range map[string]string{
		"/api/logs/forms-dev":              "project/forms-dev",
		"/api/logs/container/aaaaaaaaaaaa": "container/aaaaaaaaaaaa",
	} {
		r := newRig()
		r.logsBody = "{}"
		if w := r.do("GET", target, auth); w.Code != 200 || len(r.logCalls) != 1 || r.logCalls[0] != want {
			t.Errorf("%s: %d, calls %v; want %s", target, w.Code, r.logCalls, want)
		}
		r = newRig()
		if w := r.do("GET", target, ""); w.Code != 401 || len(r.logCalls) != 0 {
			t.Errorf("%s without a token: %d, calls %v", target, w.Code, r.logCalls)
		}
	}
}

func TestStreamOpenAndCloseAreLogged(t *testing.T) {
	r := newRig()
	w := r.do("GET", "/api/stream", "")
	if w.Code != 200 {
		t.Fatalf("stream status %d", w.Code)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "stream opened") || !strings.Contains(logs, "stream closed") || !strings.Contains(logs, "duration") {
		t.Errorf("connection lifecycle not logged: %s", logs)
	}
}

func TestCover(t *testing.T) {
	r := newRig()
	if w := r.do("GET", "/api/media/cover", ""); w.Code != 404 {
		t.Fatalf("no cover: status %d, want 404", w.Code)
	}
	r.cover = []byte("GIF89a....")
	w := r.do("GET", "/api/media/cover", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/gif" || !bytes.Equal(w.Body.Bytes(), r.cover) {
		t.Fatalf("cover: status %d type %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestCoverRejectsNonImage(t *testing.T) {
	r := newRig()
	r.cover = []byte("<html><script>alert(1)</script></html>")
	if w := r.do("GET", "/api/media/cover", ""); w.Code != 404 {
		t.Fatalf("html cover: status %d, want 404", w.Code)
	}
}

func TestClipBoundsLoggedInput(t *testing.T) {
	if got := clip(strings.Repeat("a", 5000)); len(got) > 70 {
		t.Errorf("clip left %d bytes", len(got))
	}
	r := newRig()
	r.do("POST", "/api/do/"+strings.Repeat("a", 5000), "")
	if r.logs.Len() > 1000 {
		t.Errorf("an unauthenticated request wrote %d bytes of log", r.logs.Len())
	}
}
