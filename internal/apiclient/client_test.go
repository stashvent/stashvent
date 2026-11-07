package apiclient

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/crypto"
)

// The delete token is in a header net/http doesn't strip on a redirect, so
// the API client must not follow one to another host.
func TestDeleteTokenStaysOnTheServer(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		leaked.Store(true)
	}))
	defer other.Close()

	sent := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent <- r.Header.Get(api.DeleteTokenHeader)
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	err := New(server.URL, "test").CompleteShare(t.Context(), "8xK29qLm4Tz7", "token")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTemporaryRedirect {
		t.Fatalf("CompleteShare = %v, want a %d error", err, http.StatusTemporaryRedirect)
	}
	if got := <-sent; got != "token" {
		t.Errorf("%s = %q, want %q", api.DeleteTokenHeader, got, "token")
	}
	if leaked.Load() {
		t.Error("the redirect was followed to another host")
	}
}

func TestDeleteShare(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode string // "" means no error
	}{
		{"deleted", http.StatusNoContent, "", ""},
		{"already gone", http.StatusNotFound, `{"error":{"code":"share_not_found","message":"x"}}`, api.CodeShareNotFound},
		{"wrong token", http.StatusForbidden, `{"error":{"code":"invalid_delete_token","message":"x"}}`, api.CodeInvalidDeleteToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *http.Request
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			err := New(server.URL, "test").DeleteShare(t.Context(), "8xK29qLm4Tz7", "token", "")
			var apiErr *Error
			switch {
			case tt.wantCode == "" && err != nil:
				t.Fatalf("DeleteShare = %v, want nil", err)
			case tt.wantCode != "" && (!errors.As(err, &apiErr) || apiErr.Code != tt.wantCode || apiErr.Status != tt.status):
				t.Fatalf("DeleteShare = %v, want %d %s", err, tt.status, tt.wantCode)
			}
			if got.Method != http.MethodDelete || got.URL.Path != "/v1/shares/8xK29qLm4Tz7" {
				t.Errorf("request = %s %s, want DELETE /v1/shares/8xK29qLm4Tz7", got.Method, got.URL.Path)
			}
			if h := got.Header.Get(api.DeleteTokenHeader); h != "token" {
				t.Errorf("%s = %q, want %q", api.DeleteTokenHeader, h, "token")
			}
			// No key was given, so none is sent.
			if h := got.Header.Get("Authorization"); h != "" {
				t.Errorf("Authorization = %q, want it unset", h)
			}
		})
	}
}

// The API key goes only on create and delete, as Authorization: Bearer, and
// only when the caller passes one.
func TestAPIKeyHeader(t *testing.T) {
	const key = "sv_test-key"
	got := map[string]string{} // "METHOD path" → Authorization
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()

	c := New(server.URL, "test")
	ctx := t.Context()
	if _, err := c.CreateShare(ctx, api.CreateShareRequest{}, key); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteShare(ctx, "WithKey00001", "", key); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteShare(ctx, "NoKey0000001", "token", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.CompleteShare(ctx, "Complete0001", "token"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetShare(ctx, "GetShare0001"); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"POST /v1/shares":                       "Bearer " + key,
		"DELETE /v1/shares/WithKey00001":        "Bearer " + key,
		"DELETE /v1/shares/NoKey0000001":        "",
		"POST /v1/shares/Complete0001/complete": "",
		"GET /v1/shares/GetShare0001":           "",
	}
	for route, auth := range want {
		if got[route] != auth {
			t.Errorf("%s: Authorization = %q, want %q", route, got[route], auth)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", r.URL.Query().Get("retry"))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"quota_exceeded","message":"x"}}`))
	}))
	defer server.Close()

	for value, want := range map[string]time.Duration{
		"14400":                         4 * time.Hour,
		"":                              0,
		"soon":                          0,
		"-5":                            0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 0, // the server only sends seconds
	} {
		c := New(server.URL+"?retry="+url.QueryEscape(value), "test")
		err := c.do(t.Context(), http.MethodGet, "", credentials{}, nil, nil)
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.CodeQuotaExceeded || apiErr.RetryAfter != want {
			t.Errorf("Retry-After %q: err = %#v, want RetryAfter %v", value, err, want)
		}
	}
}

// A server that can't be reached is a NetworkError naming the server, for
// every kind of request, and it never carries the URL that was asked for.
func TestNetworkError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // nothing listens there any more

	c := New(url, "test")
	const presigned = "/stashes/8xK29qLm4Tz7?X-Amz-Signature=secret"
	_, downloadErr := c.Download(t.Context(), url+presigned, 100)
	tests := []struct {
		op  string
		err error
	}{
		{OpRequest, c.DeleteShare(t.Context(), "8xK29qLm4Tz7", "token", "")},
		{OpUpload, c.Upload(t.Context(), url+presigned, nil, []byte("x"))},
		{OpDownload, downloadErr},
	}
	for _, tt := range tests {
		var netErr *NetworkError
		if !errors.As(tt.err, &netErr) || netErr.Op != tt.op || netErr.Server != url {
			t.Errorf("%s: err = %v, want a NetworkError for %s", tt.op, tt.err, url)
			continue
		}
		if strings.Contains(tt.err.Error(), "X-Amz-Signature") {
			t.Errorf("%s: the error holds the presigned URL: %v", tt.op, tt.err)
		}
	}
}

// The size comes from whichever server a link names, and the body is held in
// memory: a size no share can have is refused before any request.
func TestDownloadRefusesImpossibleSizes(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()

	for _, size := range []int64{-1, 0, crypto.TagSize, api.DefaultMaxPayloadBytes + 1, 1 << 40} {
		if _, err := New(server.URL, "test").Download(t.Context(), server.URL+"/blob", size); err == nil {
			t.Errorf("Download(%d) succeeded, want an error", size)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests sent, want none", n)
	}
}

// An API response is read up to maxResponseBytes, so a server can't stream an
// endless one into memory, on success or on error.
func TestResponsesAreCapped(t *testing.T) {
	huge := `{"share_id":"` + strings.Repeat("x", 2*maxResponseBytes) + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer server.Close()

	if _, err := New(server.URL, "test").GetShare(t.Context(), "8xK29qLm4Tz7"); err == nil {
		t.Error("GetShare decoded a response larger than maxResponseBytes")
	}
}

// The upload carries the headers the server signed into the URL, and any 2xx
// is success: S3-compatible stores answer a PUT with 200, 201, or 204.
func TestUpload(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusForbidden, http.StatusMultipleChoices} {
		var got *http.Request
		var body []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r
			body, _ = io.ReadAll(r.Body)
			w.WriteHeader(status)
		}))
		headers := map[string]string{"X-Amz-Server-Side-Encryption": "AES256", "Content-Type": "application/octet-stream"}
		err := New(server.URL, "test").Upload(t.Context(), server.URL+"/blobs/8xK29qLm4Tz7?X-Amz-Signature=s", headers, []byte("payload"))
		server.Close()

		var storageErr *StorageError
		switch {
		case status < 300 && err != nil:
			t.Errorf("%d: Upload = %v, want success", status, err)
		case status >= 300 && (!errors.As(err, &storageErr) || storageErr.Status != status):
			t.Errorf("%d: Upload = %v, want a StorageError with %d", status, err, status)
		}
		if got == nil {
			t.Errorf("%d: the upload never reached the server", status)
			continue
		}
		if got.Method != http.MethodPut || string(body) != "payload" {
			t.Errorf("%d: request = %s with %q, want PUT with the payload", status, got.Method, body)
		}
		for name, value := range headers {
			if h := got.Header.Get(name); h != value {
				t.Errorf("%d: %s = %q, want %q", status, name, h, value)
			}
		}
	}
}

// An error response keeps the server's message next to its code, for codes
// the caller has no wording of its own for.
func TestErrorKeepsTheMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "REQ1")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = w.Write([]byte(`{"error":{"code":"upgrade_required","message":"This server needs stashvent v0.3 or newer."}}`))
	}))
	defer server.Close()

	_, err := New(server.URL, "test").GetShare(t.Context(), "8xK29qLm4Tz7")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUpgradeRequired || apiErr.Code != "upgrade_required" ||
		apiErr.Message != "This server needs stashvent v0.3 or newer." || apiErr.RequestID != "REQ1" || apiErr.Server != server.URL {
		t.Errorf("GetShare = %#v, want the status, code, message, request ID, and server", err)
	}
}

func TestNormalizeServer(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://stash.example.com", "https://stash.example.com"},
		{"https://stash.example.com/", "https://stash.example.com"},
		{"HTTPS://Stash.Example.COM:8443", "https://stash.example.com:8443"},
		{"http://localhost:8080/", "http://localhost:8080"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, tt := range tests {
		if got, err := NormalizeServer(tt.in); err != nil || got != tt.want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	bad := []string{
		"", "stash.example.com", "http://stash.example.com", "ftp://stash.example.com",
		"https://stash.example.com/api", "https://stash.example.com?x=1", "https://stash.example.com#top",
	}
	for _, in := range bad {
		if got, err := NormalizeServer(in); err == nil {
			t.Errorf("NormalizeServer(%q) = %q, want an error", in, got)
		}
	}
	// A URL with credentials is refused without repeating them.
	_, err := NormalizeServer("https://alice:hunter2@stash.example.com")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("NormalizeServer with credentials: err = %v, want an error that doesn't repeat them", err)
	}
}

func TestParseLink(t *testing.T) {
	const def = "http://localhost:8080"
	tests := []struct {
		in, server, id string
		wantErr        bool
	}{
		{"8xK29qLm4Tz7", def, "8xK29qLm4Tz7", false},
		{"https://stashvent.dev/8xK29qLm4Tz7", "https://stashvent.dev", "8xK29qLm4Tz7", false},
		{"http://localhost:8080/8xK29qLm4Tz7/", "http://localhost:8080", "8xK29qLm4Tz7", false},
		{"https://stashvent.dev/short", "", "", true},
		{"not a link", "", "", true},
		// Longer IDs than today's 12 characters are accepted, so a server can
		// lengthen them without breaking this CLI; within a limit.
		{"Qw3rTy7uIo9pAb_-", def, "Qw3rTy7uIo9pAb_-", false},
		{"https://stashvent.dev/" + strings.Repeat("a", 64), "https://stashvent.dev", strings.Repeat("a", 64), false},
		{"https://stashvent.dev/" + strings.Repeat("a", 65), "", "", true},
		{"https://stashvent.dev/8xK29qLm4Tz", "", "", true},    // 11 characters
		{"-xK29qLm4Tz7", "", "", true},                         // would read as a flag
		{"https://stashvent.dev/8xK29qLm4Tz7.x", "", "", true}, // outside the alphabet
	}
	for _, tt := range tests {
		server, id, err := ParseLink(tt.in, def)
		if (err != nil) != tt.wantErr || server != tt.server || id != tt.id {
			t.Errorf("ParseLink(%q) = %q, %q, %v; want %q, %q, err=%v",
				tt.in, server, id, err, tt.server, tt.id, tt.wantErr)
		}
	}
}

func TestCheckServerURL(t *testing.T) {
	for _, ok := range []string{"https://stashvent.dev", "http://localhost:8080", "http://127.0.0.1:8080"} {
		if err := CheckServerURL(ok); err != nil {
			t.Errorf("CheckServerURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"http://example.com", "ftp://x", "localhost:8080", ""} {
		if err := CheckServerURL(bad); err == nil {
			t.Errorf("CheckServerURL(%q) = nil, want error", bad)
		}
	}
}
