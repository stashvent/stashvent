package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
)

// newTestLogger returns a logger set up like the server's, writing to w.
func newTestLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(requestIDHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})})
}

// logLines decodes the JSON log written so far.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q is not JSON: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// requestLines returns only the per-request lines.
func requestLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range logLines(t, logs) {
		if line["msg"] == "request" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestRequestLine(t *testing.T) {
	longAgent := "stashvent/1.2.3 " + strings.Repeat("x", 200)

	tests := []struct {
		name       string
		method     string
		path       string
		agent      string
		wantStatus float64 // JSON numbers decode as float64
		wantRoute  string
		wantShare  any
		wantAgent  any
	}{
		{"share route logs the pattern and the ID", http.MethodGet, "/v1/shares/" + unknownID, "stashvent/1.2.3",
			http.StatusNotFound, "GET /v1/shares/{id}", unknownID, "stashvent/1.2.3"},
		{"a malformed ID is not logged", http.MethodGet, "/v1/shares/nope", "",
			http.StatusNotFound, "GET /v1/shares/{id}", nil, nil},
		{"a long User-Agent is cut", http.MethodPost, "/v1/shares", longAgent,
			http.StatusBadRequest, "POST /v1/shares", nil, longAgent[:maxLoggedAgent]},
		{"an unknown path has no route", http.MethodGet, "/wp-login.php?user=admin", "",
			http.StatusNotFound, "", nil, nil},
		{"a wrong method has no route", http.MethodPut, "/v1/shares", "",
			http.StatusMethodNotAllowed, "", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newTestHandler()
			var logs bytes.Buffer
			h.logger = newTestLogger(&logs, slog.LevelInfo)

			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			req.Header.Set("User-Agent", tt.agent)
			rec := serve(newMux(h), req)

			lines := requestLines(t, &logs)
			if len(lines) != 1 {
				t.Fatalf("%d request lines, want 1:\n%s", len(lines), &logs)
			}
			line := lines[0]
			want := map[string]any{
				"level":      "INFO",
				"method":     tt.method,
				"route":      tt.wantRoute,
				"status":     tt.wantStatus,
				"share_id":   tt.wantShare,
				"user_agent": tt.wantAgent,
				"request_id": rec.Header().Get("X-Request-Id"),
			}
			for key, value := range want {
				if line[key] != value {
					t.Errorf("%s = %v, want %v", key, line[key], value)
				}
			}
			if _, ok := line["latency_ms"].(float64); !ok {
				t.Errorf("latency_ms = %v, want a number", line["latency_ms"])
			}
			// Nothing else: no URL, no address, no headers.
			for key := range line {
				switch key {
				case "time", "level", "msg", "method", "route", "status", "latency_ms", "share_id", "user_agent", "request_id":
				default:
					t.Errorf("unexpected field %q in the request line", key)
				}
			}
		})
	}
}

func TestRequestID(t *testing.T) {
	h, _ := newTestHandler()
	mux := newMux(h)

	seen := map[string]bool{}
	for range 3 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/shares/"+unknownID, nil)
		// A client can't choose its ID: it would end up in our logs.
		req.Header.Set("X-Request-Id", "chosen-by-the-client")
		id := serve(mux, req).Header().Get("X-Request-Id")
		if len(id) < 16 || id == "chosen-by-the-client" {
			t.Fatalf("X-Request-Id = %q, want a random ID of our own", id)
		}
		if seen[id] {
			t.Fatalf("X-Request-Id %q used twice", id)
		}
		seen[id] = true
	}
}

// What a handler logs while serving a request carries that request's ID.
func TestErrorLineCarriesRequestID(t *testing.T) {
	h, _ := newTestHandler()
	var logs bytes.Buffer
	h.logger = newTestLogger(&logs, slog.LevelInfo)
	h.store.(*fakeStore).err = errors.New("connection refused")

	rec := do(t, newMux(h), http.MethodGet, "/v1/shares/"+unknownID, "", nil)

	id := rec.Header().Get("X-Request-Id")
	lines := logLines(t, &logs)
	if len(lines) != 2 {
		t.Fatalf("%d log lines, want the error and the request line:\n%s", len(lines), &logs)
	}
	for _, line := range lines {
		if line["request_id"] != id {
			t.Errorf("%q line has request_id %v, want %q", line["msg"], line["request_id"], id)
		}
	}
	if lines[0]["msg"] != "request failed" || lines[0]["error"] != "connection refused" {
		t.Errorf("error line = %v", lines[0])
	}
}

func TestHealthRequestsLogAtDebug(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
			h, _ := newTestHandler()
			var logs bytes.Buffer
			h.logger = newTestLogger(&logs, level)

			do(t, newMux(h), http.MethodGet, path, "", nil)

			want := 0
			if level == slog.LevelDebug {
				want = 1
			}
			if got := len(requestLines(t, &logs)); got != want {
				t.Errorf("%s at %v: %d request lines, want %d", path, level, got, want)
			}
		}
	}
}

// panicObjects stands in for a bug in a handler.
type panicObjects struct {
	objectStore
}

func (panicObjects) PresignGet(context.Context, string, time.Duration) (string, error) {
	panic("presign went wrong")
}

func TestPanicBecomesInternalError(t *testing.T) {
	h, objs := newTestHandler()
	var logs bytes.Buffer
	h.logger = newTestLogger(&logs, slog.LevelInfo)
	created := createUploaded(t, h, objs)
	path := "/v1/shares/" + created.ShareID
	if rec := do(t, newMux(h), http.MethodPost, path+"/complete", created.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
	}
	h.objects = panicObjects{objs}
	logs.Reset()

	rec := do(t, newMux(h), http.MethodGet, path, "", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rec.Code, rec.Body)
	}
	if code := errorCode(t, rec); code != api.CodeInternal {
		t.Errorf("code = %q, want %q", code, api.CodeInternal)
	}
	if strings.Contains(rec.Body.String(), "presign went wrong") {
		t.Errorf("response carries the panic: %s", rec.Body)
	}

	lines := logLines(t, &logs)
	if len(lines) != 2 {
		t.Fatalf("%d log lines, want the panic and the request line:\n%s", len(lines), &logs)
	}
	if lines[0]["msg"] != "handler panicked" || lines[0]["panic"] != "presign went wrong" || lines[0]["stack"] == nil {
		t.Errorf("panic line = %v", lines[0])
	}
	if lines[1]["msg"] != "request" || lines[1]["status"] != float64(http.StatusInternalServerError) {
		t.Errorf("request line = %v", lines[1])
	}
}

// A body over the limit must also end the connection, so the server doesn't
// go on reading what the client keeps sending. net/http only does that when
// MaxBytesReader is given the server's own writer, not the log wrapper.
func TestOversizedBodyClosesConnection(t *testing.T) {
	h, _ := newTestHandler()
	srv := httptest.NewServer(newMux(h))
	defer srv.Close()

	tests := []struct {
		name      string
		body      string
		wantClose bool
	}{
		{"over the limit", `{"sender_git_version":"` + strings.Repeat("9", 4*maxRequestBody) + `"}`, true},
		{"malformed but small", "{", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/shares", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if resp.Close != tt.wantClose {
				t.Errorf("connection closed by the server = %v, want %v", resp.Close, tt.wantClose)
			}
		})
	}
}

// Handlers get a deadline, so one that waits on Postgres or storage ends.
func TestRequestHasDeadline(t *testing.T) {
	h, _ := newTestHandler()
	var left time.Duration
	handler := h.withRequestLog(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if deadline, ok := r.Context().Deadline(); ok {
			left = time.Until(deadline)
		}
	}))

	rec := do(t, handler, http.MethodGet, "/", "", nil)

	if left <= 0 || left > requestTimeout {
		t.Errorf("handler had %v left, want up to %v", left, requestTimeout)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// The logs must never hold a delete token, an API key, a presigned URL, a
// request body, or a query string, at any level and on any path through the
// handlers.
func TestLogsHoldNoSecrets(t *testing.T) {
	const (
		wrongToken  = "wrong-token-sent-by-a-client"
		queryMarker = "query-string-marker"
		bodyMarker  = "9.99.body-marker"
	)
	h, objs := newTestHandler()
	var logs bytes.Buffer
	h.logger = newTestLogger(&logs, slog.LevelDebug)
	mux := newMux(h)

	create := validCreate()
	create.SenderGitVersion = bodyMarker
	rec := do(t, mux, http.MethodPost, "/v1/shares?note="+queryMarker, "", create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body)
	}
	var created api.CreateShareResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	key := "stashes/" + created.ShareID
	path := "/v1/shares/" + created.ShareID
	down := errors.New("connection refused")

	steps := []struct {
		name       string
		method     string
		path       string
		token      string
		setup      func()
		wantStatus int
	}{
		{"complete with a wrong token", http.MethodPost, path + "/complete", wrongToken, nil, http.StatusForbidden},
		{"complete with the wrong size, which can't be deleted", http.MethodPost, path + "/complete", created.DeleteToken,
			func() { objs.sizes[key] = 99; objs.deleteErr[key] = down }, http.StatusUnprocessableEntity},
		{"complete", http.MethodPost, path + "/complete?note=" + queryMarker, created.DeleteToken,
			func() { objs.sizes[key] = create.Size; delete(objs.deleteErr, key) }, http.StatusNoContent},
		{"summary", http.MethodGet, path + "/summary?note=" + queryMarker, "", nil, http.StatusOK},
		{"get", http.MethodGet, path + "?note=" + queryMarker, "", nil, http.StatusOK},
		{"get with the database down", http.MethodGet, path, "",
			func() { h.store.(*fakeStore).err = down }, http.StatusInternalServerError},
		{"delete with a wrong token", http.MethodDelete, path, wrongToken,
			func() { h.store.(*fakeStore).err = nil }, http.StatusForbidden},
		{"delete while storage is down", http.MethodDelete, path, created.DeleteToken,
			func() { objs.deleteErr[key] = down }, http.StatusInternalServerError},
		{"delete", http.MethodDelete, path + "?note=" + queryMarker, created.DeleteToken,
			func() { delete(objs.deleteErr, key) }, http.StatusNoContent},
		{"unknown path", http.MethodGet, "/" + created.DeleteToken + "?note=" + queryMarker, created.DeleteToken, nil, http.StatusNotFound},
		{"readyz", http.MethodGet, "/readyz", created.DeleteToken, nil, http.StatusOK},
	}
	for _, s := range steps {
		if s.setup != nil {
			s.setup()
		}
		if rec := do(t, mux, s.method, s.path, s.token, nil); rec.Code != s.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", s.name, rec.Code, s.wantStatus, rec.Body)
		}
	}

	// With keys on: a create and a delete with the key, and refused ones
	// with a key the server doesn't know and one that is malformed.
	h.abuse.requireAPIKey = true
	apiKey, _ := issueKey(t, h, "alice")
	unknownKey, keyErr := apikey.New()
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	const malformedKey = "malformed-key-marker"
	h.store.(*fakeStore).err = nil
	keyed := createWithKey(t, h, objs, apiKey)
	keySteps := []struct {
		name       string
		method     string
		path       string
		key        string
		wantStatus int
	}{
		{"create with an unknown key", http.MethodPost, "/v1/shares", unknownKey, http.StatusUnauthorized},
		{"create with a malformed key", http.MethodPost, "/v1/shares", malformedKey, http.StatusUnauthorized},
		{"delete with an unknown key", http.MethodDelete, "/v1/shares/" + keyed.ShareID, unknownKey, http.StatusUnauthorized},
		{"delete with the key", http.MethodDelete, "/v1/shares/" + keyed.ShareID, apiKey, http.StatusNoContent},
	}
	for _, s := range keySteps {
		var body any
		if s.method == http.MethodPost {
			body = validCreate()
		}
		if rec := doKey(t, mux, s.method, s.path, "", s.key, body); rec.Code != s.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", s.name, rec.Code, s.wantStatus, rec.Body)
		}
	}

	// Every request was logged, the creates included, so there was something to leak.
	if got, want := len(requestLines(t, &logs)), len(steps)+1+1+len(keySteps); got != want {
		t.Errorf("%d request lines, want %d", got, want)
	}
	secrets := map[string]string{
		"the delete token":      created.DeleteToken,
		"a token a client sent": wrongToken,
		"a request header":      api.DeleteTokenHeader,
		"a presigned URL":       presignedMarker,
		"the storage host":      "storage.test",
		"a query string":        queryMarker,
		"a request body":        bodyMarker,
		"the API key":           apiKey,
		"a key a client sent":   unknownKey,
		"a malformed key":       malformedKey,
		"the key's header":      "Authorization",
		"the key's scheme":      "Bearer",
	}
	for what, secret := range secrets {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log holds %s (%q):\n%s", what, secret, &logs)
		}
	}
}
