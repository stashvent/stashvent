//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/storage"
)

// cfg is what TestMain found: the running server, the binaries, the limits
// the server was started with (make passes the same values to both), and
// direct access to Postgres and object storage for checks and cleanup.
var cfg struct {
	server   string
	sv, svs  string
	quota    int
	maxPulls int
	pool     *pgxpool.Pool
	objects  *storage.Objects
}

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	code := m.Run()
	cfg.pool.Close()
	os.Exit(code)
}

func setup() error {
	cfg.server = strings.TrimSuffix(os.Getenv("STASHVENT_E2E_SERVER"), "/")
	if cfg.server == "" {
		return errors.New("STASHVENT_E2E_SERVER is not set; run the tests with make test-e2e")
	}

	binDir, err := filepath.Abs(envOr("STASHVENT_E2E_BIN", filepath.Join("..", "bin")))
	if err != nil {
		return err
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	cfg.sv = filepath.Join(binDir, "stashvent"+exe)
	cfg.svs = filepath.Join(binDir, "stashvent-server"+exe)
	for _, bin := range []string{cfg.sv, cfg.svs} {
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("%s is missing; make test-e2e builds it: %w", bin, err)
		}
	}

	for name, v := range map[string]*int{"API_KEY_SHARES_PER_DAY": &cfg.quota, "MAX_PULLS_PER_SHARE": &cfg.maxPulls} {
		n, err := strconv.Atoi(os.Getenv(name))
		if err != nil || n <= 0 {
			return fmt.Errorf("%s must be the running server's value, above 0; run the tests with make test-e2e", name)
		}
		*v = n
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is not set; it comes from .env through make test-e2e")
	}
	if cfg.pool, err = pgxpool.New(ctx, databaseURL); err != nil {
		return err
	}
	if err := cfg.pool.Ping(ctx); err != nil {
		cfg.pool.Close()
		return fmt.Errorf("connecting to postgres (is the Docker stack up?): %w", err)
	}
	// No defaults: `stashvent-server keys ban` reads the same variables and
	// stops without them, so the tests and the operator must see one storage.
	for _, name := range []string{"S3_ENDPOINT", "S3_PUBLIC_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		if os.Getenv(name) == "" {
			cfg.pool.Close()
			return fmt.Errorf("%s is not set; it comes from .env through make test-e2e", name)
		}
	}
	cfg.objects = storage.NewObjects(storage.S3Config{
		Endpoint:       os.Getenv("S3_ENDPOINT"),
		PublicEndpoint: os.Getenv("S3_PUBLIC_ENDPOINT"),
		Region:         envOr("S3_REGION", "us-east-1"),
		Bucket:         os.Getenv("S3_BUCKET"),
		UsePathStyle:   envOr("S3_USE_PATH_STYLE", "false") == "true",
		AccessKey:      os.Getenv("S3_ACCESS_KEY"),
		SecretKey:      os.Getenv("S3_SECRET_KEY"),
	})

	if err := checkServer(ctx); err != nil {
		cfg.pool.Close()
		return err
	}
	return nil
}

// checkServer makes sure a server is running, and with keys on: these tests
// are about the binaries with keys on, and would fail confusingly otherwise.
func checkServer(ctx context.Context) error {
	hint := "start it with make run-server-e2e"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.server+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("no server at %s (%w); %s", cfg.server, err, hint)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server at %s isn't ready (GET /readyz: %d)", cfg.server, resp.StatusCode)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodPost, cfg.server+"/v1/shares", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var body api.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Code != api.CodeMissingAPIKey {
		return fmt.Errorf("the server at %s doesn't require API keys (a push without one got %d %s); %s, not make run-server",
			cfg.server, resp.StatusCode, body.Error.Code, hint)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// machine is one user's computer: a home, config, and temp directory of its
// own, and the server chosen with STASHVENT_SERVER.
type machine struct {
	home string
	tmp  string
	env  []string
}

// newMachine starts from this process's environment (PATH, and what Windows
// needs to run anything), minus every STASHVENT_ and GIT_ variable and the
// directories it replaces. GIT_CONFIG_GLOBAL, GIT_DIR, GIT_AUTHOR_NAME and the
// like would bring the developer's git setup in. The system git config is
// kept: it belongs to the machine's git install (core.autocrlf on Windows).
// extra adds NAME=value entries.
func newMachine(t *testing.T, extra ...string) *machine {
	t.Helper()
	home := t.TempDir()
	m := &machine{home: home, tmp: filepath.Join(home, "tmp")}
	if err := os.Mkdir(m.tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	own := map[string]string{
		// os.UserConfigDir: %AppData% on Windows, $XDG_CONFIG_HOME on Linux,
		// $HOME/Library/Application Support on macOS. HOME also keeps the
		// developer's global git config out.
		"APPDATA": home, "XDG_CONFIG_HOME": home, "HOME": home,
		"TMP": m.tmp, "TEMP": m.tmp, "TMPDIR": m.tmp,
		"STASHVENT_SERVER": cfg.server,
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name) // Windows names ignore case
		if _, replaced := own[upper]; replaced || strings.HasPrefix(upper, "STASHVENT_") || strings.HasPrefix(upper, "GIT_") {
			continue
		}
		m.env = append(m.env, kv)
	}
	for name, value := range own {
		m.env = append(m.env, name+"="+value)
	}
	m.env = append(m.env, extra...)
	return m
}

// configDir is where the CLI keeps history.json and config.json here.
func (m *machine) configDir() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(m.home, "Library", "Application Support", "stashvent")
	}
	return filepath.Join(m.home, "stashvent")
}

type result struct {
	code           int
	stdout, stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

// exitCode is the code of a finished process; a process that didn't run
// fails the test.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatal(err)
		return 0
	}
}

// run runs bin/stashvent in dir (the machine's home if empty).
func (m *machine) run(t *testing.T, dir, stdin string, args ...string) result {
	t.Helper()
	if dir == "" {
		dir = m.home
	}
	cmd := exec.CommandContext(t.Context(), cfg.sv, args...) //nolint:gosec // G204: the binary under test
	cmd.Dir = dir
	cmd.Env = m.env
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := exitCode(t, cmd.Run())
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// git runs git in dir with the machine's environment and returns its
// trimmed output.
func (m *machine) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: fixed test arguments
	cmd.Dir = dir
	cmd.Env = m.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (m *machine) identity(t *testing.T, dir, name string) {
	t.Helper()
	m.git(t, dir, "config", "user.name", name)
	m.git(t, dir, "config", "user.email", "e2e@example.com")
	m.git(t, dir, "config", "commit.gpgsign", "false")
}

// newRepo is a repository with one commit.
func (m *machine) newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	m.git(t, dir, "init", "-q", "-b", "main")
	m.identity(t, dir, "Test Sender")
	writeFile(t, dir, "a.txt", "base\n")
	m.git(t, dir, "add", "a.txt")
	m.git(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func (m *machine) clone(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	m.git(t, dir, "clone", "-q", src, ".")
	m.identity(t, dir, "Test Recipient")
	return dir
}

// stash stashes a changed file and a new one.
func (m *machine) stash(t *testing.T, repo, message string) {
	t.Helper()
	writeFile(t, repo, "a.txt", "base\nwork in progress\n")
	writeFile(t, repo, "notes.txt", "notes for the handoff\n")
	m.git(t, repo, "stash", "push", "-q", "--include-untracked", "-m", message)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readFile reads a file of a working tree, with the line endings Git for
// Windows may have changed back to LF.
func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// login stores key on the machine, through the hidden prompt's stdin twin.
func (m *machine) login(t *testing.T, key string) {
	t.Helper()
	if res := m.run(t, "", key+"\n", "login", "--with-token"); res.code != 0 {
		t.Fatalf("login: %s", res)
	}
}

// push pushes the newest stash of repo. The share is removed when the test
// ends.
func (m *machine) push(t *testing.T, repo string) (shareID, link, password string) {
	t.Helper()
	res := m.run(t, repo, "", "push", "--expires", "1h")
	if res.code != 0 {
		t.Fatalf("push: %s", res)
	}
	link = field(t, res.stdout, "Share link")
	shareID = link[strings.LastIndex(link, "/")+1:]
	forget(t, shareID)
	return shareID, link, field(t, res.stdout, "Password")
}

func (m *machine) pull(t *testing.T, dir, password, link string) result {
	t.Helper()
	return m.run(t, dir, password+"\n", "pull", "--password-stdin", link)
}

// field returns the value of a "Name: value" line of the CLI's output.
func field(t *testing.T, output, name string) string {
	t.Helper()
	for line := range strings.Lines(output) {
		if value, ok := strings.CutPrefix(line, name+":"); ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("no %q line in:\n%s", name, output)
	return ""
}

// operator runs bin/stashvent-server with this process's environment, which
// has DATABASE_URL and the S3 settings from .env.
func operator(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), cfg.svs, args...) //nolint:gosec // G204: the binary under test
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := exitCode(t, cmd.Run())
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

var issuedKey = regexp.MustCompile(`^Issued key (\d+) `)

// issueKey issues a key with the operator command and removes its row (and
// with it its usage) when the test ends.
func issueKey(t *testing.T) (key string, id int64) {
	t.Helper()
	res := operator(t, "keys", "issue", "--label", "e2e "+t.Name()+" "+rand.Text()[:8])
	m := issuedKey.FindStringSubmatch(res.stderr)
	key = strings.TrimSpace(res.stdout)
	if res.code != 0 || m == nil || !apikey.Valid(key) {
		t.Fatalf("keys issue: exit %d\n%s", res.code, res.stderr) // never the key
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = cfg.pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	return key, id
}

// forget removes a share's object and row when the test ends.
func forget(t *testing.T, shareID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_ = cfg.objects.Delete(ctx, "stashes/"+shareID)
		_, _ = cfg.pool.Exec(ctx, `DELETE FROM shares WHERE share_id = $1`, shareID)
	})
}

// live reports whether the share is still unexpired on the server.
func live(t *testing.T, shareID string) bool {
	t.Helper()
	var n int
	err := cfg.pool.QueryRow(t.Context(), `SELECT count(*) FROM shares WHERE share_id = $1 AND expires_at > now()`, shareID).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func hasObject(t *testing.T, shareID string) bool {
	t.Helper()
	_, err := cfg.objects.Size(t.Context(), "stashes/"+shareID)
	if err != nil && !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func wantStderr(t *testing.T, step string, res result, code int, want string) {
	t.Helper()
	if res.code != code || !strings.Contains(res.stderr, want) {
		t.Errorf("%s: want exit %d and %q, got %s", step, code, want, res)
	}
}

// noLeftovers checks that a run left no decrypted bundle in the machine's
// temp dir and no temp ref in the repository (stashvent-git skill).
func (m *machine) noLeftovers(t *testing.T, repo string) {
	t.Helper()
	entries, err := os.ReadDir(m.tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "stashvent-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if refs := m.git(t, repo, "for-each-ref", "refs/share/"); refs != "" {
		t.Errorf("temp refs left behind: %s", refs)
	}
}

// The product's round trip with the real binaries: push, list, pull into a
// clone on another machine, apply, delete.
func TestDryRun(t *testing.T) {
	key, _ := issueKey(t)
	sender, recipient := newMachine(t), newMachine(t)
	sender.login(t, key)
	repo := sender.newRepo(t)
	clone := recipient.clone(t, repo)
	sender.stash(t, repo, "e2e handoff")

	shareID, link, password := sender.push(t, repo)
	sender.noLeftovers(t, repo)
	if !live(t, shareID) || !hasObject(t, shareID) {
		t.Fatal("the pushed share isn't on the server")
	}
	if list := sender.run(t, "", "", "list"); list.code != 0 || !strings.Contains(list.stdout, shareID) || !strings.Contains(list.stdout, "On main: e2e handoff") {
		t.Errorf("list: %s", list)
	}

	pull := recipient.pull(t, clone, password, link)
	wantStderr(t, "pull", pull, 0, "✓ Imported into stash@{0}")
	if got, want := recipient.git(t, clone, "stash", "list"), "stash@{0}: Shared from Test Sender — On main: e2e handoff"; got != want {
		t.Errorf("recipient's stash list = %q, want %q", got, want)
	}
	recipient.git(t, clone, "stash", "apply", "-q")
	if readFile(t, clone, "a.txt") != "base\nwork in progress\n" || readFile(t, clone, "notes.txt") != "notes for the handoff\n" {
		t.Error("the applied stash differs from the pushed one")
	}
	recipient.noLeftovers(t, clone)

	// The user files are on the sender's machine only, private on Unix.
	for _, name := range []string{"config.json", "history.json"} {
		info, err := os.Stat(filepath.Join(sender.configDir(), name))
		if err != nil {
			t.Errorf("sender's %s: %v", name, err)
			continue
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("sender's %s has mode %v, want 0600", name, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(recipient.configDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pull created the recipient's config dir (%v)", err)
	}

	wantStderr(t, "delete", sender.run(t, "", "", "delete", shareID), 0, "✓ Deleted "+shareID)
	if live(t, shareID) || hasObject(t, shareID) {
		t.Error("the share is still on the server after delete")
	}
	wantStderr(t, "pull after delete", recipient.pull(t, clone, password, link), 1, "Share not found or expired.")
	if list := sender.run(t, "", "", "list"); list.code != 0 || list.stdout != "" {
		t.Errorf("list after delete: %s", list)
	}
}

// login, the pull cap, deleting from a second machine with only the key,
// and logout.
func TestKeysLifecycle(t *testing.T) {
	key, _ := issueKey(t)
	sender := newMachine(t)
	repo := sender.newRepo(t)
	sender.stash(t, repo, "keys")

	wantStderr(t, "push before login", sender.run(t, repo, "", "push"), 1, `Run "stashvent login"`)
	sender.login(t, key)
	shareID, link, password := sender.push(t, repo)

	// The pull cap: every lookup counts, also one that finds the stash
	// already imported.
	recipient := newMachine(t)
	clone := recipient.clone(t, repo)
	for i := range cfg.maxPulls {
		if res := recipient.pull(t, clone, password, link); res.code != 0 {
			t.Fatalf("pull %d of %d: %s", i+1, cfg.maxPulls, res)
		}
	}
	wantStderr(t, "pull over the cap", recipient.pull(t, clone, password, link), 1,
		"This share has been pulled the maximum number of times. Ask the sender to push it again.")

	// Another machine with only the key: never sent to a link's host, and
	// deletes by ID without a history entry.
	other := newMachine(t)
	other.login(t, key)
	wantStderr(t, "delete through another host's link", other.run(t, "", "", "delete", "https://evil.invalid/"+shareID), 1,
		"isn't in this machine's history, and the link points to https://evil.invalid")
	if !live(t, shareID) {
		t.Fatal("the refused delete ended the share")
	}
	wantStderr(t, "delete with the key", other.run(t, "", "", "delete", shareID), 0, "✓ Deleted "+shareID)
	if live(t, shareID) || hasObject(t, shareID) {
		t.Error("the share is still on the server after the key deleted it")
	}

	if res := sender.run(t, "", "", "logout"); res.code != 0 {
		t.Fatalf("logout: %s", res)
	}
	wantStderr(t, "push after logout", sender.run(t, repo, "", "push"), 1, `Run "stashvent login"`)
}

// The daily quota, and keys usage counting it.
func TestQuota(t *testing.T) {
	key, id := issueKey(t)
	sender := newMachine(t)
	sender.login(t, key)
	repo := sender.newRepo(t)
	sender.stash(t, repo, "quota")
	for range cfg.quota {
		sender.push(t, repo)
	}
	res := sender.run(t, repo, "", "push")
	wantStderr(t, "push over the quota", res, 1, "Your API key has used its daily share limit on this server. Try again in ")

	usage := operator(t, "keys", "usage", strconv.FormatInt(id, 10))
	today := regexp.MustCompile(`(?m)^` + time.Now().UTC().Format("2006-01-02") + ` +` + strconv.Itoa(cfg.quota) + ` +\d+\s*$`)
	if usage.code != 0 || !today.MatchString(usage.stdout) {
		t.Errorf("keys usage: want %d shares today, got %s", cfg.quota, usage)
	}
}

// Revoke keeps a key's shares and stops the key; ban also ends the shares.
// keys list --share is how an abuse report finds the key to ban.
func TestRevokeAndBan(t *testing.T) {
	keyA, idA := issueKey(t)
	keyB, idB := issueKey(t)
	recipient := newMachine(t)

	a := newMachine(t)
	a.login(t, keyA)
	repoA := a.newRepo(t)
	a.stash(t, repoA, "revoke")
	shareA, linkA, passwordA := a.push(t, repoA)

	if res := operator(t, "keys", "revoke", strconv.FormatInt(idA, 10)); res.code != 0 || !strings.Contains(res.stderr, "Revoked key") {
		t.Fatalf("keys revoke: %s", res)
	}
	wantStderr(t, "pull of a revoked key's share", recipient.pull(t, recipient.clone(t, repoA), passwordA, linkA), 0, "✓ Imported")
	wantStderr(t, "push with a revoked key", a.run(t, repoA, "", "push"), 1, "The server didn't accept your API key")
	other := newMachine(t)
	other.login(t, keyA)
	wantStderr(t, "delete with a revoked key", other.run(t, "", "", "delete", shareA), 1, "The server didn't accept your API key")
	if !live(t, shareA) {
		t.Fatal("the revoked key's delete ended the share")
	}
	wantStderr(t, "delete with the history's token", a.run(t, "", "", "delete", shareA), 0, "✓ Deleted "+shareA)

	b := newMachine(t)
	b.login(t, keyB)
	repoB := b.newRepo(t)
	b.stash(t, repoB, "ban")
	shareB1, linkB1, passwordB1 := b.push(t, repoB)
	shareB2, _, _ := b.push(t, repoB)

	list := operator(t, "keys", "list", "--share", shareB1)
	if list.code != 0 || !regexp.MustCompile(`(?m)^`+strconv.FormatInt(idB, 10)+` .* active\s*$`).MatchString(list.stdout) {
		t.Errorf("keys list --share: want key %d, got %s", idB, list)
	}
	ban := operator(t, "keys", "ban", strconv.FormatInt(idB, 10))
	if ban.code != 0 || !strings.Contains(ban.stderr, "Ended 2 shares.") {
		t.Fatalf("keys ban: %s", ban)
	}
	for _, id := range []string{shareB1, shareB2} {
		if live(t, id) || hasObject(t, id) {
			t.Errorf("share %s is still on the server after the ban", id)
		}
	}
	wantStderr(t, "pull of a banned key's share", recipient.pull(t, recipient.clone(t, repoB), passwordB1, linkB1), 1, "Share not found or expired.")
	wantStderr(t, "push with a banned key", b.run(t, repoB, "", "push"), 1, "The server didn't accept your API key")

	all := operator(t, "keys", "list")
	for id, status := range map[int64]string{idA: "revoked", idB: "banned"} {
		if !regexp.MustCompile(`(?m)^` + strconv.FormatInt(id, 10) + ` .* ` + status + ` \d{4}-\d{2}-\d{2}\s*$`).MatchString(all.stdout) {
			t.Errorf("keys list: key %d isn't %s:\n%s", id, status, all.stdout)
		}
	}
	if again := operator(t, "keys", "ban", strconv.FormatInt(idB, 10)); again.code != 0 || !strings.Contains(again.stderr, "Ended 0 shares.") {
		t.Errorf("a second ban: %s", again)
	}
}

// STASHVENT_API_KEY on a machine without config.json: used, said, never
// printed; and a key can't delete another key's share.
func TestEnvAPIKey(t *testing.T) {
	keyC, _ := issueKey(t)
	keyD, _ := issueKey(t)

	d := newMachine(t)
	d.login(t, keyD)
	repoD := d.newRepo(t)
	d.stash(t, repoD, "someone else's")
	shareD, _, _ := d.push(t, repoD)

	c := newMachine(t, "STASHVENT_API_KEY="+keyC)
	repoC := c.newRepo(t)
	c.stash(t, repoC, "from the environment")
	res := c.run(t, repoC, "", "push", "--expires", "1h")
	if res.code != 0 || !strings.Contains(res.stderr, "API key: from STASHVENT_API_KEY") {
		t.Fatalf("push with STASHVENT_API_KEY: %s", res)
	}
	link := field(t, res.stdout, "Share link")
	forget(t, link[strings.LastIndex(link, "/")+1:])
	if strings.Contains(res.stdout+res.stderr, keyC) {
		t.Error("push printed the API key")
	}
	if _, err := os.Stat(filepath.Join(c.configDir(), "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("push wrote a config.json (%v)", err)
	}

	wantStderr(t, "delete another key's share", c.run(t, "", "", "delete", shareD), 1,
		"Your API key didn't create this share, so it can't delete it.")
	if !live(t, shareD) {
		t.Error("another key's delete ended the share")
	}
}
