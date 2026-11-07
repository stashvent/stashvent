package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/git"
	"github.com/stashvent/stashvent/internal/security"
)

// update rewrites the golden files: go test ./apps/stashvent/cmd -update
var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// What every test run sees instead of the clock, the password generator, and
// the server's address, so its output is the same everywhere.
var testNow = time.Date(2026, 10, 13, 10, 50, 0, 0, time.UTC)

const (
	testPassword = "Kj9nM2xP7qL5vW3bG8dFt4Rw" //nolint:gosec // G101: a sample password, used only by the fake generator
	// goldenServer stands in for the test server's address in golden files.
	// The .example domain is reserved and can never resolve.
	goldenServer = "https://stashvent.example"
)

// fakeIDs are the share IDs the fake server hands out, in order.
var fakeIDs = []string{"8xK29qLm4Tz7", "Qw3rTy7uIo9p", "Zx5cVb8nMa2s", "Lk4jHg6fDs1a"}

// fakeID is the ID of the nth share (from 0) the fake server creates.
func fakeID(n int) string {
	if n < len(fakeIDs) {
		return fakeIDs[n]
	}
	return fmt.Sprintf("Share%07d", n)
}

// fakeShare is one share in the fake server.
type fakeShare struct {
	req       api.CreateShareRequest
	token     string
	createdAt time.Time
	expiresAt time.Time
	active    bool
	blob      []byte
	key       string // the API key that created it, if the server requires keys
}

// fakeFailure is an error a route answers with instead of doing its work.
type fakeFailure struct {
	status     int
	code       string
	retryAfter string
	message    string // "test" if empty
	requestID  string // "TESTREQUESTID" if empty
}

// fakeServer is the /v1 API and the object storage behind it, in memory. It
// follows the contract closely enough for the CLI: the same status codes and
// error codes, presigned-looking URLs, a delete token per share.
type fakeServer struct {
	*httptest.Server

	mu     sync.Mutex
	shares map[string]*fakeShare
	made   int
	calls  []string               // route names, in the order they were called
	fail   map[string]fakeFailure // route name → error to answer instead
	tokens []string               // the X-Delete-Token of every request that had one
	// auths is the Authorization header of every call, "" for none, by route.
	auths map[string][]string
	// requireKey is REQUIRE_API_KEY: creating a share needs one of keys.
	requireKey bool
	keys       map[string]bool
	// maxPayload is this server's MAX_PAYLOAD_BYTES.
	maxPayload int64
	// onUpload runs when an upload arrives, before it is stored.
	onUpload func(r *http.Request)
	// uploadHeaders go out as CreateShareResponse.UploadHeaders.
	uploadHeaders map[string]string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{
		shares: map[string]*fakeShare{}, fail: map[string]fakeFailure{}, maxPayload: api.DefaultMaxPayloadBytes,
		auths: map[string][]string{}, keys: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/shares", f.route("create", f.create))
	mux.HandleFunc("POST /v1/shares/{id}/complete", f.route("complete", f.complete))
	mux.HandleFunc("GET /v1/shares/{id}", f.route("get", f.get))
	mux.HandleFunc("DELETE /v1/shares/{id}", f.route("delete", f.delete))
	mux.HandleFunc("PUT /blobs/{id}", f.route("upload", f.upload))
	mux.HandleFunc("GET /blobs/{id}", f.route("download", f.download))
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// route records the call and applies an injected failure.
func (f *fakeServer) route(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, name)
		if token := r.Header.Get(api.DeleteTokenHeader); token != "" {
			f.tokens = append(f.tokens, token)
		}
		f.auths[name] = append(f.auths[name], r.Header.Get("Authorization"))
		failure, failing := f.fail[name]
		f.mu.Unlock()
		if failing {
			if failure.retryAfter != "" {
				w.Header().Set("Retry-After", failure.retryAfter)
			}
			if failure.requestID != "" {
				w.Header().Set("X-Request-Id", failure.requestID)
			}
			message := failure.message
			if message == "" {
				message = "test"
			}
			fakeErrorMessage(w, failure.status, failure.code, message)
			return
		}
		h(w, r)
	}
}

func fakeError(w http.ResponseWriter, status int, code string) {
	fakeErrorMessage(w, status, code, "test")
}

func fakeErrorMessage(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("X-Request-Id") == "" { // a fakeFailure may set its own
		w.Header().Set("X-Request-Id", "TESTREQUESTID")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.ErrorBody{Code: code, Message: message}})
}

// requestKey returns the API key of a request, "" if it has none.
func requestKey(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (f *fakeServer) create(w http.ResponseWriter, r *http.Request) {
	var key string
	if f.requireKey {
		key = requestKey(r)
		f.mu.Lock()
		known := f.keys[key]
		f.mu.Unlock()
		switch {
		case key == "":
			fakeError(w, http.StatusUnauthorized, api.CodeMissingAPIKey)
			return
		case !known:
			fakeError(w, http.StatusUnauthorized, api.CodeInvalidAPIKey)
			return
		}
	}
	var req api.CreateShareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fakeError(w, http.StatusBadRequest, api.CodeInvalidRequest)
		return
	}
	lifetime, ok := api.ExpiresIn[req.ExpiresIn]
	switch {
	case !ok || req.Size <= 0:
		fakeError(w, http.StatusBadRequest, api.CodeInvalidRequest)
		return
	case req.Size > f.maxPayload:
		fakeError(w, http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
		return
	}

	f.mu.Lock()
	id := fakeID(f.made)
	f.made++
	share := &fakeShare{req: req, token: "delete-token-" + id, createdAt: testNow, expiresAt: testNow.Add(lifetime), key: key}
	f.shares[id] = share
	uploadHeaders := f.uploadHeaders
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(api.CreateShareResponse{
		ShareID:         id,
		DeleteToken:     share.token,
		UploadURL:       f.URL + "/blobs/" + id + "?X-Amz-Signature=upload-signature",
		UploadExpiresAt: testNow.Add(15 * time.Minute),
		ExpiresAt:       share.expiresAt,
		UploadHeaders:   uploadHeaders,
	})
}

// authorized finds the share and checks the delete token, in the order the
// real server does.
func (f *fakeServer) authorized(w http.ResponseWriter, r *http.Request) *fakeShare {
	token := r.Header.Get(api.DeleteTokenHeader)
	share := f.shares[r.PathValue("id")]
	switch {
	case token == "":
		fakeError(w, http.StatusUnauthorized, api.CodeMissingDeleteToken)
	case share == nil:
		fakeError(w, http.StatusNotFound, api.CodeShareNotFound)
	case token != share.token:
		fakeError(w, http.StatusForbidden, api.CodeInvalidDeleteToken)
	default:
		return share
	}
	return nil
}

func (f *fakeServer) complete(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	share := f.authorized(w, r)
	switch {
	case share == nil:
	case share.active:
		fakeError(w, http.StatusConflict, api.CodeShareNotPending)
	case int64(len(share.blob)) != share.req.Size:
		fakeError(w, http.StatusUnprocessableEntity, api.CodeSizeMismatch)
	default:
		share.active = true
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeServer) get(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := r.PathValue("id")
	share := f.shares[id]
	if share == nil || !share.active {
		fakeError(w, http.StatusNotFound, api.CodeShareNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.GetShareResponse{
		ShareID:         id,
		Size:            share.req.Size,
		Salt:            share.req.Salt,
		IV:              share.req.IV,
		PayloadFormat:   share.req.PayloadFormat,
		PackagingMethod: share.req.PackagingMethod,
		BundleVersion:   share.req.BundleVersion,
		ObjectFormat:    share.req.ObjectFormat,
		DownloadURL:     f.URL + "/blobs/" + id + "?X-Amz-Signature=download-signature",
		CreatedAt:       share.createdAt,
		ExpiresAt:       share.expiresAt,
	})
}

// delete also accepts the API key that created the share, in the order the
// real server checks.
func (f *fakeServer) delete(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := requestKey(r)
	if f.requireKey && key != "" && r.Header.Get(api.DeleteTokenHeader) == "" {
		share := f.shares[r.PathValue("id")]
		switch {
		case !f.keys[key]:
			fakeError(w, http.StatusUnauthorized, api.CodeInvalidAPIKey)
			return
		case share == nil:
			fakeError(w, http.StatusNotFound, api.CodeShareNotFound)
			return
		case share.key != key:
			fakeError(w, http.StatusForbidden, api.CodeShareNotOwned)
			return
		}
	} else if f.authorized(w, r) == nil {
		return
	}
	delete(f.shares, r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeServer) upload(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hook := f.onUpload
	f.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	blob, err := io.ReadAll(r.Body)
	if err != nil {
		return // the client went away
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	share := f.shares[r.PathValue("id")]
	if share == nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	share.blob = blob
}

func (f *fakeServer) download(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	share := f.shares[r.PathValue("id")]
	if share == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = w.Write(share.blob)
}

// failWith makes a route answer with status from now on; 0 turns it off.
func (f *fakeServer) failWith(route string, status int) {
	if status == 0 {
		f.failWithCode(route, fakeFailure{})
		return
	}
	f.failWithCode(route, fakeFailure{status: status, code: api.CodeInternal})
}

// failWithCode makes a route answer with an error code from now on; a zero
// failure turns it off.
func (f *fakeServer) failWithCode(route string, failure fakeFailure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if failure.status == 0 {
		delete(f.fail, route)
		return
	}
	f.fail[route] = failure
}

// requireKeys turns REQUIRE_API_KEY on, accepting keys.
func (f *fakeServer) requireKeys(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requireKey = true
	for _, k := range keys {
		f.keys[k] = true
	}
}

// authorization returns the Authorization headers a route received, "" for
// a call without one.
func (f *fakeServer) authorization(route string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.auths[route])
}

// called returns how often a route was called.
func (f *fakeServer) called(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == route {
			n++
		}
	}
	return n
}

// requests returns how many requests arrived at all.
func (f *fakeServer) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// share returns a copy of the share with that ID, or nil.
func (f *fakeServer) share(id string) *fakeShare {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.shares[id]; s != nil {
		c := *s
		return &c
	}
	return nil
}

// forget drops a share without a request, as the server's expiry sweep would.
func (f *fakeServer) forget(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.shares, id)
}

// cli runs the CLI the way a user would, with everything around it faked.
type cli struct {
	t          *testing.T
	dir        string            // working directory
	configDir  string            // where history.json goes; never the real one
	env        map[string]string // the environment
	terminal   bool              // whether stdin is a terminal
	stdin      string
	secrets    []string // what is typed at each hidden prompt, in order
	now        time.Time
	commit     string // the build's commit; none by default
	maxPayload int64  // the size limit; api.DefaultMaxPayloadBytes if 0
	scanErr    error  // if set, the secret scan fails with it
}

// newCLI returns a CLI in dir whose server, through STASHVENT_SERVER, is f.
func newCLI(t *testing.T, dir string, f *fakeServer) *cli {
	t.Helper()
	c := &cli{t: t, dir: dir, configDir: filepath.Join(t.TempDir(), "stashvent"), env: map[string]string{}, now: testNow}
	if f != nil {
		c.env[serverEnv] = f.URL
	}
	return c
}

type result struct {
	code   int
	stdout string
	stderr string
}

func (c *cli) run(args ...string) result {
	c.t.Helper()
	return c.runCtx(c.t.Context(), args...)
}

func (c *cli) runCtx(ctx context.Context, args ...string) result {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	secrets := slices.Clone(c.secrets)
	a := newApp(Env{
		Version: "test", Commit: c.commit,
		Stdin: strings.NewReader(c.stdin), Stdout: &stdout, Stderr: &stderr,
		Dir: c.dir, ConfigDir: c.configDir,
		Getenv:     func(key string) string { return c.env[key] },
		Now:        func() time.Time { return c.now },
		IsTerminal: func() bool { return c.terminal },
		ReadSecret: func(context.Context) (string, error) {
			if len(secrets) == 0 {
				return "", io.ErrUnexpectedEOF
			}
			secret := secrets[0]
			secrets = secrets[1:]
			return secret, nil
		},
	})
	a.loc = time.UTC
	a.newPassword = func() (string, error) { return testPassword, nil }
	if c.maxPayload != 0 {
		a.maxPayload = c.maxPayload
	}
	if c.scanErr != nil {
		a.scan = func(context.Context, string, git.Stash) ([]security.Finding, error) { return nil, c.scanErr }
	}
	code := a.run(ctx, args)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// historyFile returns history.json as text, "" if there is none.
func (c *cli) historyFile() string {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.configDir, "history.json")) //nolint:gosec // G304: test's own temp dir
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.t.Fatal(err)
	}
	return string(data)
}

// golden compares got with testdata/<name>, or rewrites the file with
// -update. The test server's address is replaced first.
func golden(t *testing.T, name, got string, f *fakeServer) {
	t.Helper()
	if f != nil {
		got = strings.ReplaceAll(got, f.URL, goldenServer)
	}
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a fixed directory of test files
	if err != nil {
		t.Fatalf("%v (run the test with -update to create it)", err)
	}
	// The files are checked out with LF everywhere (.gitattributes); this
	// only guards against a checkout that ignored it.
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); got != w {
		t.Errorf("%s differs (run the test with -update if the change is meant):\n--- got\n%s\n--- want\n%s", name, got, w)
	}
}

// gitRun runs git in dir and returns its trimmed output.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: fixed test arguments
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setIdentity gives a repository its own user, so the output doesn't depend
// on the machine's git config (a CI runner has none).
func setIdentity(t *testing.T, repo string) {
	t.Helper()
	gitRun(t, repo, "config", "user.name", "Test Sender")
	gitRun(t, repo, "config", "user.email", "sender@example.com")
	gitRun(t, repo, "config", "commit.gpgsign", "false")
}

// newRepo creates a repository on branch main with one commit of a.txt.
func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	setIdentity(t, repo)
	writeFile(t, repo, "a.txt", "base\n")
	gitRun(t, repo, "add", "a.txt")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	return repo
}

// cloneRepo clones src, as the recipient's copy.
func cloneRepo(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "clone", "-q", src, ".")
	setIdentity(t, dir)
	return dir
}

// addStash stashes a change to a.txt and a new file, under message. The
// stash list then shows it as "On main: <message>".
func addStash(t *testing.T, repo, message string) {
	t.Helper()
	writeFile(t, repo, "a.txt", "base\n"+message+"\n")
	writeFile(t, repo, "notes.txt", "notes for "+message+"\n")
	gitRun(t, repo, "stash", "push", "-q", "--include-untracked", "-m", message)
}
