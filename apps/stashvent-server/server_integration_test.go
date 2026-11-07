//go:build integration

// Run against the Docker stack: make up && make test-integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/apiclient"
	"github.com/stashvent/stashvent/internal/crypto"
	"github.com/stashvent/stashvent/internal/storage"
	"github.com/stashvent/stashvent/internal/storage/postgres"
)

// integrationServer is the real handlers on the Docker stack's Postgres and
// object storage.
type integrationServer struct {
	url     string
	client  *apiclient.Client
	pool    *pgxpool.Pool
	store   *postgres.Store
	objects *storage.Objects
	s3      storage.S3Config
	seen    *recorder // the server's log, and every request it received
	apiKey  string    // sent by create, if set
}

// recorder keeps what reached a test server, for tests that check what the
// server saw: its log lines, and each request's method, URL, headers, and body.
type recorder struct {
	mu       sync.Mutex
	log      bytes.Buffer
	requests []string
}

// Write takes the server's log.
func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log.Write(p)
}

func (r *recorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "reading the body", http.StatusBadRequest)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s\n", req.Method, req.URL)
		_ = req.Header.Write(&b)
		b.Write(body)
		r.mu.Lock()
		r.requests = append(r.requests, b.String())
		r.mu.Unlock()
		next.ServeHTTP(w, req)
	})
}

// requestCount is how many requests reached the server so far.
func (r *recorder) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// all is the log and every request, as one text.
func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log.String() + strings.Join(r.requests, "\n")
}

// newIntegrationServer runs with keys off and the default pull cap.
func newIntegrationServer(t *testing.T) *integrationServer {
	t.Helper()
	return newIntegrationServerWith(t, abuseConfig{sharesPerDay: 10, maxPulls: 50})
}

// newIntegrationServerWith uses the development database itself (the server
// under test migrates it, as on any start), so tests remove what they create.
func newIntegrationServerWith(t *testing.T, abuse abuseConfig) *integrationServer {
	t.Helper()
	return newServerOn(t, abuse, false)
}

// newIsolatedServer has a schema of its own, dropped when the test ends, for
// tests that count or sweep over the whole table: on the development
// database, a share of someone else's expiring would change their result,
// and a sweep would remove other people's shares.
func newIsolatedServer(t *testing.T, abuse abuseConfig) *integrationServer {
	t.Helper()
	return newServerOn(t, abuse, true)
}

func newServerOn(t *testing.T, abuse abuseConfig, isolated bool) *integrationServer {
	t.Helper()
	ctx := t.Context()

	cfg, err := pgxpool.ParseConfig(envOr("DATABASE_URL", "postgres://stashvent:stashvent@localhost:5438/stashvent?sslmode=disable"))
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("test_%d", time.Now().UnixNano())
	schema := pgx.Identifier{name}.Sanitize()
	if isolated {
		cfg.ConnConfig.RuntimeParams["search_path"] = name
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if isolated {
		if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatalf("creating a schema (is the Docker stack up?): %v", err)
		}
		// Runs before pool.Close, and after the tests' own cleanups.
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	}
	if _, err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrating (is the Docker stack up?): %v", err)
	}

	store := postgres.New(pool)
	s3cfg := storage.S3Config{
		Endpoint:       envOr("S3_ENDPOINT", "http://localhost:8333"),
		PublicEndpoint: envOr("S3_PUBLIC_ENDPOINT", "http://localhost:8333"),
		Region:         envOr("S3_REGION", "us-east-1"),
		Bucket:         envOr("S3_BUCKET", "stashvent"),
		UsePathStyle:   true,
		AccessKey:      envOr("S3_ACCESS_KEY", "stashvent-dev"),
		SecretKey:      envOr("S3_SECRET_KEY", "stashvent-dev-secret"),
	}
	objects := storage.NewObjects(s3cfg)

	// The server's own log format, at debug level so nothing is left out.
	seen := &recorder{}
	h := &handler{
		store:      store,
		keys:       store,
		objects:    objects,
		maxPayload: api.DefaultMaxPayloadBytes,
		abuse:      abuse,
		logger:     slog.New(requestIDHandler{slog.NewJSONHandler(seen, &slog.HandlerOptions{Level: slog.LevelDebug})}),
		now:        time.Now,
		limits:     newRateLimits(testLimits, nil, time.Now),
		ready: &readiness{checks: []readyCheck{
			{name: "postgres", ping: pool.Ping},
			{name: "storage", ping: objects.Ping},
		}},
	}
	srv := httptest.NewServer(seen.wrap(newMux(h)))
	t.Cleanup(srv.Close)
	return &integrationServer{
		url:     srv.URL,
		client:  apiclient.New(srv.URL, "integration-test"),
		pool:    pool,
		store:   store,
		objects: objects,
		s3:      s3cfg,
		seen:    seen,
	}
}

// forget removes a share created through the CLI, object and row, when the
// test ends. The row outlives a DELETE; see TestUploadAfterDeleteOnDockerStack.
func (s *integrationServer) forget(t *testing.T, shareID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_ = s.objects.Delete(ctx, "stashes/"+shareID)
		_ = s.store.DeleteShare(ctx, shareID)
	})
}

// create registers a share for payload and, when the test ends, removes its
// object and its row, however far the test got.
func (s *integrationServer) create(t *testing.T, payload []byte) api.CreateShareResponse {
	t.Helper()
	salt := make([]byte, crypto.SaltSize)
	iv := make([]byte, crypto.NonceSize)
	for _, b := range [][]byte{salt, iv} {
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
	}
	created, err := s.client.CreateShare(t.Context(), api.CreateShareRequest{
		Size:             int64(len(payload)),
		ExpiresIn:        "1h",
		Salt:             salt,
		IV:               iv,
		PayloadFormat:    api.PayloadFormatV1,
		PackagingMethod:  api.PackagingStashRef,
		BundleVersion:    2,
		ObjectFormat:     "sha1",
		SenderGitVersion: "2.32.0",
	}, s.apiKey)
	if err != nil {
		t.Fatal(err)
	}
	s.forget(t, created.ShareID)
	return created
}

// request sends one request without the API client, for the endpoints the
// client doesn't cover yet. It returns the status.
func (s *integrationServer) request(t *testing.T, method, path, token string) int {
	t.Helper()
	status, _ := s.send(t, method, path, token, "")
	return status
}

// send sends one request with a delete token, an API key, both, or neither,
// and returns the status and the error code, if any.
func (s *integrationServer) send(t *testing.T, method, path, token, key string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set(api.DeleteTokenHeader, token)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body api.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&body) // a 2xx has no error body
	return resp.StatusCode, body.Error.Code
}

// The whole life of a share through the real server, Postgres, and object
// storage: create → upload → complete → get → download → delete.
func TestShareLifecycleOnDockerStack(t *testing.T) {
	ctx := t.Context()
	s := newIntegrationServer(t)

	if status := s.request(t, http.MethodGet, "/readyz", ""); status != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want 200", status)
	}

	payload := make([]byte, 4096) // stands in for an encrypted payload
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	created := s.create(t, payload)
	path := "/v1/shares/" + created.ShareID
	key := "stashes/" + created.ShareID

	if err := s.client.Upload(ctx, created.UploadURL, created.UploadHeaders, payload); err != nil {
		t.Fatal(err)
	}
	if err := s.client.CompleteShare(ctx, created.ShareID, created.DeleteToken); err != nil {
		t.Fatal(err)
	}

	// The summary doesn't count as a pull.
	for range 2 {
		if status := s.request(t, http.MethodGet, path+"/summary", ""); status != http.StatusOK {
			t.Fatalf("GET %s/summary = %d, want 200", path, status)
		}
	}
	if row, err := s.store.GetActiveShare(ctx, created.ShareID); err != nil || row.Lookups != 0 {
		t.Fatalf("after two summaries: lookups = %d, err = %v; want 0", row.Lookups, err)
	}

	got, err := s.client.GetShare(ctx, created.ShareID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != int64(len(payload)) || len(got.Salt) != crypto.SaltSize || len(got.IV) != crypto.NonceSize {
		t.Errorf("get returned other metadata than was created: %+v", got)
	}
	downloaded, err := s.client.Download(ctx, got.DownloadURL, got.Size)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded, payload) {
		t.Error("downloaded bytes differ from the uploaded ones")
	}

	steps := []struct {
		name       string
		method     string
		token      string
		wantStatus int
	}{
		{"delete without a token", http.MethodDelete, "", http.StatusUnauthorized},
		{"delete with a wrong token", http.MethodDelete, "wrong", http.StatusForbidden},
		{"still there", http.MethodGet, "", http.StatusOK},
		{"delete", http.MethodDelete, created.DeleteToken, http.StatusNoContent},
		{"get after delete", http.MethodGet, "", http.StatusNotFound},
		{"delete again", http.MethodDelete, created.DeleteToken, http.StatusNotFound},
	}
	for _, step := range steps {
		if status := s.request(t, step.method, path, step.token); status != step.wantStatus {
			t.Fatalf("%s: status = %d, want %d", step.name, status, step.wantStatus)
		}
	}

	// The object is gone too, so the download URL handed out before the
	// delete no longer works although it hasn't expired.
	if _, err := s.objects.Size(ctx, key); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("object after delete: err = %v, want ErrObjectNotFound", err)
	}
	if _, err := s.client.Download(ctx, got.DownloadURL, got.Size); err == nil {
		t.Error("the old download URL still returns the payload after the delete")
	}
}

// Deleting the object doesn't cancel the upload URL, so the sender can put the
// object back after a DELETE. The share must stay gone, and its row must stay
// for the sweep, which is the only thing that can still remove that object.
func TestUploadAfterDeleteOnDockerStack(t *testing.T) {
	ctx := t.Context()
	s := newIntegrationServer(t)
	payload := []byte("encrypted-payload-stand-in")
	created := s.create(t, payload)
	path := "/v1/shares/" + created.ShareID
	key := "stashes/" + created.ShareID

	// A pending share, with its object uploaded, can be deleted by its creator.
	if err := s.client.Upload(ctx, created.UploadURL, created.UploadHeaders, payload); err != nil {
		t.Fatal(err)
	}
	if status := s.request(t, http.MethodDelete, path, created.DeleteToken); status != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", status)
	}
	if _, err := s.objects.Size(ctx, key); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object after delete: err = %v, want ErrObjectNotFound", err)
	}

	// The upload URL still works.
	if err := s.client.Upload(ctx, created.UploadURL, created.UploadHeaders, payload); err != nil {
		t.Fatalf("upload after delete: %v (if storage now refuses this, the sweep's wait is no longer needed)", err)
	}
	var apiErr *apiclient.Error
	if err := s.client.CompleteShare(ctx, created.ShareID, created.DeleteToken); !errors.As(err, &apiErr) || apiErr.Code != api.CodeShareNotFound {
		t.Errorf("complete after delete: err = %v, want %s", err, api.CodeShareNotFound)
	}
	if status := s.request(t, http.MethodGet, path, ""); status != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", status)
	}

	// What the sweep does once the share is old enough: it finds the row,
	// and through it the object.
	sw := &sweeper{store: s.store, objects: s.objects, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := sw.remove(ctx, storage.ExpiredShare{ShareID: created.ShareID, StorageKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.objects.Size(ctx, key); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("object after the sweep: err = %v, want ErrObjectNotFound", err)
	}
	// The row was there until the sweep removed it.
	if err := s.store.DeleteShare(ctx, created.ShareID); !errors.Is(err, storage.ErrShareNotFound) {
		t.Errorf("row after the sweep: err = %v, want ErrShareNotFound", err)
	}
}
