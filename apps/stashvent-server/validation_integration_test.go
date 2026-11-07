//go:build integration

// Run against the Docker stack: make up && make test-integration
//
// Checks of the whole system: what the server can see, who may
// delete, the expiry sweep, the storage cap, and the CLI's repository and
// secret checks on a real push and pull.

package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/git"
	"github.com/stashvent/stashvent/internal/storage"
)

// pushed pushes the newest stash of repo and returns its link and password.
// The share is removed when the test ends.
func (s *integrationServer) pushed(t *testing.T, repo, configDir string, args ...string) (link, password string) {
	t.Helper()
	res := runCLI(t, s, repo, configDir, "", append([]string{"push", "--expires", "1h"}, args...)...)
	if res.code != 0 {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}
	link = field(t, res.stdout, "Share link")
	s.forget(t, strings.TrimPrefix(link, s.url+"/"))
	return link, field(t, res.stdout, "Password")
}

// pull runs `stashvent pull` in dir with a config directory of its own.
func (s *integrationServer) pull(t *testing.T, dir, password, link string) cliRun {
	t.Helper()
	return runCLI(t, s, dir, t.TempDir(), password+"\n", "pull", "--password-stdin", link)
}

// login stores key for s in configDir.
func (s *integrationServer) login(t *testing.T, configDir, key string) {
	t.Helper()
	if res := runCLI(t, s, t.TempDir(), configDir, key+"\n", "login", "--with-token"); res.code != 0 {
		t.Fatalf("login: exit %d\n%s", res.code, res.stderr)
	}
}

// keyID is the ID of the key issued with label.
func (s *integrationServer) keyID(t *testing.T, label string) int64 {
	t.Helper()
	keys, err := s.store.ListAPIKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Label == label {
			return k.ID
		}
	}
	t.Fatalf("no key labelled %q", label)
	return 0
}

// deleteWith sends DELETE /v1/shares/{id} with a delete token, an API key,
// both, or neither.
func (s *integrationServer) deleteWith(t *testing.T, shareID, token, key string) (int, string) {
	t.Helper()
	return s.send(t, http.MethodDelete, "/v1/shares/"+shareID, token, key)
}

// backdate makes a share created age ago, expiring expiresIn from now
// (negative: already expired). Both are measured from the database's clock,
// which the sweep's query compares against.
func (s *integrationServer) backdate(t *testing.T, shareID string, age, expiresIn time.Duration) {
	t.Helper()
	tag, err := s.pool.Exec(t.Context(),
		`UPDATE shares SET created_at = now() - $2::interval, expires_at = now() + $3::interval WHERE share_id = $1`,
		shareID, age, expiresIn)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("backdating %s: %v (%d rows)", shareID, err, tag.RowsAffected())
	}
}

// hasRow reports whether the share's row is still in the table, whatever
// its state.
func (s *integrationServer) hasRow(t *testing.T, shareID string) bool {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM shares WHERE share_id = $1`, shareID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// hasObject reports whether the share's object is in the bucket.
func (s *integrationServer) hasObject(t *testing.T, shareID string) bool {
	t.Helper()
	_, err := s.objects.Size(t.Context(), "stashes/"+shareID)
	if err != nil && !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// bucket is a plain S3 client for the test's own looks into the bucket, which
// storage.Objects has no methods for.
func (s *integrationServer) bucket() *s3.Client {
	return s3.New(s3.Options{
		Region:                     s.s3.Region,
		BaseEndpoint:               aws.String(s.s3.Endpoint),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider(s.s3.AccessKey, s.s3.SecretKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

// The server must never learn a repository detail: a push with a
// distinctive repository name, branch, file, message, sender, and commits
// leaves none of them in any table, object name, object, log line, or
// request. TestSharesColumnsAreAllowlisted checks the columns by name; this
// checks the values.
func TestNoRepoDetailsOnServer(t *testing.T) {
	ctx := t.Context()
	s := newIntegrationServerWith(t, abuseConfig{requireAPIKey: true, sharesPerDay: 10, maxPulls: 50})
	op := &operator{store: s.store, objects: s.objects, stdout: io.Discard, stderr: io.Discard, now: time.Now}
	key := s.issueKey(t, op, "integration-"+rand.Text())

	// Every detail carries "zqcanary", which nothing else would contain.
	sender := t.TempDir()
	gitIn(t, sender, "init", "-q", "-b", "zqcanary-branch")
	setGitIdentity(t, sender, "Zqcanary Sender")
	gitIn(t, sender, "remote", "add", "origin", "https://example.com/zqcanaryorg/zqcanaryrepo.git")
	writeTestFile(t, sender, "zqcanary-base.txt", "zqcanary base\n")
	gitIn(t, sender, "add", ".")
	gitIn(t, sender, "commit", "-q", "-m", "zqcanary base commit")
	recipient := cloneGitRepo(t, sender)
	writeTestFile(t, sender, "zqcanary-base.txt", "zqcanary base\nzqcanary change\n")
	writeTestFile(t, sender, "zqcanary-file.txt", "zqcanary content\n")
	gitIn(t, sender, "stash", "push", "-q", "--include-untracked", "-m", "zqcanary handoff")

	// The commit IDs: base, stash, index, untracked files; and the repo hash.
	commits := strings.Fields(gitIn(t, sender, "rev-parse", "stash@{0}", "stash@{0}^1", "stash@{0}^2", "stash@{0}^3"))
	repoHash, err := git.RepoHash(ctx, sender, commits[1])
	if err != nil {
		t.Fatal(err)
	}
	needles := append([]string{"zqcanary", repoHash}, commits...)
	leaks := func(where, text string) {
		t.Helper()
		lower := strings.ToLower(text)
		for _, n := range needles {
			if strings.Contains(lower, strings.ToLower(n)) {
				t.Errorf("%s holds %q", where, n)
			}
		}
	}

	senderConfig := t.TempDir()
	s.login(t, senderConfig, key)
	link, password := s.pushed(t, sender, senderConfig)
	shareID := strings.TrimPrefix(link, s.url+"/")

	// Every row of every table, as JSON: bytea columns come out as hex, so a
	// commit ID stored as bytes would show too. Tables are listed, not named,
	// so a new table is covered without changing this test.
	rows, err := s.pool.Query(ctx, `SELECT table_name::text FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"shares", "api_keys", "api_key_usage"} {
		if !slices.Contains(tables, table) {
			t.Fatalf("table %s not found among %v", table, tables)
		}
	}
	for _, table := range tables {
		rows, err := s.pool.Query(ctx, `SELECT row_to_json(t)::text FROM `+pgx.Identifier{table}.Sanitize()+` t`)
		if err != nil {
			t.Fatal(err)
		}
		values, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		leaks("table "+table, strings.Join(values, "\n"))
	}
	var share string
	if err := s.pool.QueryRow(ctx, `SELECT row_to_json(t)::text FROM shares t WHERE share_id = $1`, shareID).Scan(&share); err != nil {
		t.Fatalf("the pushed share has no row: %v", err)
	}

	// Every object name in the bucket, and the share's object itself.
	bucket := s.bucket()
	var names []string
	pages := s3.NewListObjectsV2Paginator(bucket, &s3.ListObjectsV2Input{Bucket: aws.String(s.s3.Bucket)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Contents {
			names = append(names, aws.ToString(o.Key))
		}
	}
	if !strings.Contains(strings.Join(names, "\n"), "stashes/"+shareID) {
		t.Fatalf("the share's object is not in the bucket listing")
	}
	leaks("object names", strings.Join(names, "\n"))
	obj, err := bucket.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.s3.Bucket), Key: aws.String("stashes/" + shareID)})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	leaks("the stored object", string(ciphertext))

	// What reached the server: its log, and each request with its headers
	// and body.
	leaks("the server's log and requests", s.seen.all())

	// The details do exist, inside the encrypted payload: the recipient gets
	// them back.
	if res := s.pull(t, recipient, password, link); res.code != 0 {
		t.Fatalf("pull: exit %d\n%s", res.code, res.stderr)
	}
	if got, want := gitIn(t, recipient, "stash", "list"), "stash@{0}: Shared from Zqcanary Sender — On zqcanary-branch: zqcanary handoff"; got != want {
		t.Errorf("recipient's stash list = %q, want %q", got, want)
	}
}

// DELETE is refused for everything but the share's own delete token and the
// key that created it, on the real stack: and a refused DELETE changes
// nothing.
func TestDeleteRefusalsOnDockerStack(t *testing.T) {
	ctx := t.Context()
	s := newIntegrationServerWith(t, abuseConfig{requireAPIKey: true})
	op := &operator{store: s.store, objects: s.objects, stdout: io.Discard, stderr: io.Discard, now: time.Now}
	labelA, labelB := "integration-"+rand.Text(), "integration-"+rand.Text()
	keyA, keyB := s.issueKey(t, op, labelA), s.issueKey(t, op, labelB)
	payload := []byte("encrypted-payload-stand-in")
	uploaded := func() api.CreateShareResponse {
		t.Helper()
		created := s.create(t, payload)
		if err := s.client.Upload(ctx, created.UploadURL, created.UploadHeaders, payload); err != nil {
			t.Fatal(err)
		}
		if err := s.client.CompleteShare(ctx, created.ShareID, created.DeleteToken); err != nil {
			t.Fatal(err)
		}
		return created
	}
	s.apiKey = keyA
	first, second := uploaded(), uploaded()
	s.apiKey = keyB
	third := uploaded()

	alive := func(step, shareID string) {
		t.Helper()
		if _, err := s.store.GetActiveShare(ctx, shareID); err != nil || !s.hasObject(t, shareID) {
			t.Fatalf("%s: the share is no longer live (%v)", step, err)
		}
	}
	unissued, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}

	refusals := []struct {
		name, token, key string
		status           int
		code             string
	}{
		{"neither token nor key", "", "", http.StatusUnauthorized, api.CodeMissingDeleteToken},
		{"a wrong token", "not-the-token", "", http.StatusForbidden, api.CodeInvalidDeleteToken},
		{"another share's token", second.DeleteToken, "", http.StatusForbidden, api.CodeInvalidDeleteToken},
		{"a key that didn't create it", "", keyB, http.StatusForbidden, api.CodeShareNotOwned},
		{"a wrong token and another key", "not-the-token", keyB, http.StatusForbidden, api.CodeShareNotOwned},
		{"a key never issued", "", unissued, http.StatusUnauthorized, api.CodeInvalidAPIKey},
		{"a malformed key", "", "sv_not-a-key", http.StatusUnauthorized, api.CodeInvalidAPIKey},
	}
	for _, r := range refusals {
		status, code := s.deleteWith(t, first.ShareID, r.token, r.key)
		if status != r.status || code != r.code {
			t.Errorf("%s: %d %s, want %d %s", r.name, status, code, r.status, r.code)
		}
		alive(r.name, first.ShareID)
	}

	// A revoked key can't delete any more; the delete token still can, also
	// with the revoked key sent along.
	if err := op.revoke(ctx, s.keyID(t, labelA)); err != nil {
		t.Fatal(err)
	}
	if status, code := s.deleteWith(t, first.ShareID, "", keyA); status != http.StatusUnauthorized || code != api.CodeInvalidAPIKey {
		t.Errorf("the revoked creator's key: %d %s, want 401 %s", status, code, api.CodeInvalidAPIKey)
	}
	alive("the revoked creator's key", first.ShareID)
	if status, code := s.deleteWith(t, first.ShareID, first.DeleteToken, keyA); status != http.StatusNoContent {
		t.Errorf("the token with the revoked key: %d %s, want 204", status, code)
	}
	if _, err := s.store.GetActiveShare(ctx, first.ShareID); !errors.Is(err, storage.ErrShareNotFound) || s.hasObject(t, first.ShareID) {
		t.Errorf("after the delete: %v, object still there: %v", err, s.hasObject(t, first.ShareID))
	}

	// With keys off, Authorization is ignored: not even the key that created
	// a share can delete it.
	keysOff := newIntegrationServer(t)
	if status, code := keysOff.deleteWith(t, third.ShareID, "", keyB); status != http.StatusUnauthorized || code != api.CodeMissingDeleteToken {
		t.Errorf("keys off, the creator's key: %d %s, want 401 %s", status, code, api.CodeMissingDeleteToken)
	}
	alive("keys off, the creator's key", third.ShareID)
}

// The expiry sweep, as the server runs it, on the real table and bucket:
// expired, deleted, banned, and abandoned shares lose row and object; shares
// that aren't due yet keep both.
func TestSweepOnDockerStack(t *testing.T) {
	ctx := t.Context()
	s := newIsolatedServer(t, abuseConfig{requireAPIKey: true}) // the sweep sees only this test's shares
	op := &operator{store: s.store, objects: s.objects, stdout: io.Discard, stderr: io.Discard, now: time.Now}
	// The banned share has a key of its own: a ban ends every live share of
	// its key.
	banLabel := "integration-" + rand.Text()
	key, banKey := s.issueKey(t, op, "integration-"+rand.Text()), s.issueKey(t, op, banLabel)
	s.apiKey = key
	payload := []byte("encrypted-payload-stand-in")
	upload := func(created api.CreateShareResponse) {
		t.Helper()
		if err := s.client.Upload(ctx, created.UploadURL, created.UploadHeaders, payload); err != nil {
			t.Fatal(err)
		}
	}
	active := func() api.CreateShareResponse {
		t.Helper()
		created := s.create(t, payload)
		upload(created)
		if err := s.client.CompleteShare(ctx, created.ShareID, created.DeleteToken); err != nil {
			t.Fatal(err)
		}
		return created
	}
	// deleted is a share ended by DELETE, whose object was then put back
	// with the upload URL, which a DELETE doesn't cancel.
	deleted := func() api.CreateShareResponse {
		t.Helper()
		created := active()
		if status, code := s.deleteWith(t, created.ShareID, created.DeleteToken, ""); status != http.StatusNoContent {
			t.Fatalf("delete: %d %s", status, code)
		}
		upload(created)
		return created
	}

	expired := active()
	s.backdate(t, expired.ShareID, 2*time.Hour, -time.Hour)
	oldDeleted := deleted()
	s.backdate(t, oldDeleted.ShareID, uploadURLTTL+uploadGrace+time.Minute, -time.Second)
	abandoned := s.create(t, payload) // uploaded, never completed
	upload(abandoned)
	s.backdate(t, abandoned.ShareID, storage.PendingTTL+pendingGrace+5*time.Minute, 4*time.Hour)
	live := active()
	young := s.create(t, payload) // pending, but not for long enough
	upload(young)
	s.backdate(t, young.ShareID, 30*time.Minute, 30*time.Minute)
	recentDeleted := deleted() // its upload URL still works
	s.backdate(t, recentDeleted.ShareID, 5*time.Minute, -time.Second)
	s.apiKey = banKey
	banned := active()
	if err := op.ban(ctx, s.keyID(t, banLabel)); err != nil {
		t.Fatal(err)
	}
	upload(banned) // put back after the ban, like a late upload after a DELETE
	s.backdate(t, banned.ShareID, uploadURLTTL+uploadGrace+time.Minute, -time.Second)

	sw := &sweeper{store: s.store, objects: s.objects, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sw.sweep(ctx)

	for name, id := range map[string]string{
		"expired": expired.ShareID, "deleted 31 minutes ago": oldDeleted.ShareID,
		"abandoned while pending": abandoned.ShareID, "banned": banned.ShareID,
	} {
		if s.hasRow(t, id) || s.hasObject(t, id) {
			t.Errorf("%s share: row %v, object %v after the sweep; want both gone", name, s.hasRow(t, id), s.hasObject(t, id))
		}
	}
	for name, id := range map[string]string{
		"live": live.ShareID, "pending for 30 minutes": young.ShareID, "deleted 5 minutes ago": recentDeleted.ShareID,
	} {
		if !s.hasRow(t, id) || !s.hasObject(t, id) {
			t.Errorf("%s share: row %v, object %v after the sweep; want both kept", name, s.hasRow(t, id), s.hasObject(t, id))
		}
	}
}

// MAX_STORED_BYTES through the real CLI: a push that would go over it is
// refused with the CLI's message, and deleting a share makes room again.
func TestStorageCapOnDockerStack(t *testing.T) {
	ctx := t.Context()
	repo := newGitRepo(t, "Test Sender")
	writeTestFile(t, repo, "a.txt", "base\nwork in progress\n")
	gitIn(t, repo, "stash", "push", "-q", "-m", "cap")
	configDir := t.TempDir()

	// The size of this stash's payload, from a server without a cap. Both
	// servers have a schema of their own: on the development database,
	// another share expiring during the test would change the total.
	uncapped := newIsolatedServer(t, abuseConfig{maxPulls: 50})
	link, _ := uncapped.pushed(t, repo, configDir)
	share, err := uncapped.store.GetShare(ctx, strings.TrimPrefix(link, uncapped.url+"/"))
	if err != nil {
		t.Fatal(err)
	}

	// Room for one share of that size, not two.
	s := newIsolatedServer(t, abuseConfig{maxStored: share.Size + share.Size/2, maxPulls: 50})
	first, _ := s.pushed(t, repo, configDir)
	before, err := s.store.StoredBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	requests := s.seen.requestCount()
	full := runCLI(t, s, repo, configDir, "", "push", "--expires", "1h")
	if full.code != 1 || !strings.Contains(full.stderr, "✗ The server is out of storage space. Try again later.") {
		t.Fatalf("push over the cap: exit %d\n%s", full.code, full.stderr)
	}
	// Refused before the insert: nothing was added, and nothing uploaded.
	if after, err := s.store.StoredBytes(ctx); err != nil || after != before {
		t.Errorf("stored bytes after the refused push = %d, %v; want %d", after, err, before)
	}
	if n := s.seen.requestCount() - requests; n != 1 {
		t.Errorf("the refused push made %d requests, want only the create", n)
	}

	if res := runCLI(t, s, repo, configDir, "", "delete", strings.TrimPrefix(first, s.url+"/")); res.code != 0 {
		t.Fatalf("delete: exit %d\n%s", res.code, res.stderr)
	}
	s.pushed(t, repo, configDir) // a deleted share stops counting at once
}

// Repository verification through a real pull: each way the recipient's
// repository can differ ends in its own message, imports nothing, and leaves
// no temp ref; doing what the message says makes the same pull work.
func TestRepoMismatchOnDockerStack(t *testing.T) {
	s := newIntegrationServer(t)
	sender := newGitRepo(t, "Test Sender")
	gitIn(t, sender, "remote", "add", "origin", "https://example.com/acme/payments-api.git")
	writeTestFile(t, sender, "b.txt", "second\n")
	gitIn(t, sender, "add", "b.txt")
	gitIn(t, sender, "commit", "-q", "-m", "second")

	// Made before the stash's base commit exists.
	behind := cloneGitRepo(t, sender)
	// --no-local: a local clone ignores --depth.
	shallow := cloneGitRepo(t, sender, "--no-local", "--depth", "1")
	if gitIn(t, shallow, "rev-parse", "--is-shallow-repository") != "true" {
		t.Fatal("the --depth 1 clone isn't shallow")
	}

	writeTestFile(t, sender, "c.txt", "base of the stash\n")
	gitIn(t, sender, "add", "c.txt")
	gitIn(t, sender, "commit", "-q", "-m", "third")
	base := gitIn(t, sender, "rev-parse", "HEAD")
	ahead := cloneGitRepo(t, sender)
	writeTestFile(t, sender, "a.txt", "base\nwork in progress\n")
	gitIn(t, sender, "stash", "push", "-q", "-m", "mismatch")
	link, password := s.pushed(t, sender, t.TempDir())

	refused := func(name, dir, want string) {
		t.Helper()
		res := s.pull(t, dir, password, link)
		if res.code != 1 || !strings.Contains(res.stderr, "✗ "+want) {
			t.Errorf("%s: exit %d\n%s\nwant %q", name, res.code, res.stderr, want)
		}
		if list := gitIn(t, dir, "stash", "list"); list != "" {
			t.Errorf("%s: the stash list is %q, want it empty", name, list)
		}
		if refs := gitIn(t, dir, "for-each-ref", "refs/share/"); refs != "" {
			t.Errorf("%s: temp refs left: %s", name, refs)
		}
	}
	imported := func(name, dir string) cliRun {
		t.Helper()
		res := s.pull(t, dir, password, link)
		if res.code != 0 || !strings.Contains(res.stderr, "✓ Imported into stash@{0}") {
			t.Errorf("%s: exit %d\n%s", name, res.code, res.stderr)
		}
		return res
	}

	refused("unrelated repository", newGitRepo(t, "Someone Else"),
		"This stash comes from a different repository (acme/payments-api). Pull it inside a clone of that repository.")

	refused("base commit not fetched", behind, `Base commit `+base[:7]+` is missing. Run "git fetch" and try again.`)
	gitIn(t, behind, "fetch", "-q")
	imported("after git fetch", behind)

	refused("shallow clone", shallow, `Base commit `+base[:7]+` is missing from this shallow clone. Run "git fetch --unshallow" and try again.`)
	gitIn(t, shallow, "fetch", "-q", "--unshallow")
	imported("after git fetch --unshallow", shallow)

	// HEAD moved on since the stash: only a note.
	writeTestFile(t, ahead, "d.txt", "local work\n")
	gitIn(t, ahead, "add", "d.txt")
	gitIn(t, ahead, "commit", "-q", "-m", "local")
	head := gitIn(t, ahead, "rev-parse", "HEAD")
	res := imported("HEAD elsewhere", ahead)
	if want := "ℹ Based on " + base[:7] + " (main); you're on " + head[:7] + "."; !strings.Contains(res.stderr, want) {
		t.Errorf("HEAD elsewhere: no %q in\n%s", want, res.stderr)
	}
}

// Fake credentials, assembled at run time so this file holds no
// secret-shaped text. Well-known sample keys can't be used: gitleaks
// allowlists them.
const (
	fill32 = "x8Kq2ZpL9vTn4WbR7sYd3FhJ6mNc1GaE"
	fill16 = "Q3EGUTZ7VLKM2XPD"
)

// The secret scan on a real push: one stash with every kind of secret
// TestScanFindsEachKindOfSecret covers, plus a StashVent key and a password
// in a URL. Without a terminal the push stops before anything is sent, naming
// each finding but no value; with --ignore-secrets the files arrive unchanged.
func TestSecretScanOnDockerStack(t *testing.T) {
	s := newIntegrationServer(t)
	b64 := base64.RawURLEncoding.EncodeToString
	svKey, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		"aws":      "AKIA" + fill16,
		"stripe":   "sk_" + "live_" + fill32,
		"session":  fill32,
		"db":       strings.ToLower(fill32),
		"sv":       svKey,
		"url":      fill16 + "u",
		"rsa":      fill32 + fill32,
		"constant": strings.ToUpper(fill32),
		"jwt":      b64([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + b64([]byte(`{"sub":"1234567890","name":"dev"}`)) + "." + fill32,
		"github":   "ghp_" + fill32 + "aB3d",
	}
	files := map[string]string{
		".env": "PORT=8080\n" +
			"AWS_ACCESS_KEY_ID=" + secrets["aws"] + "\n" +
			"STRIPE_SECRET_KEY=" + secrets["stripe"] + "\n" +
			"SESSION_SECRET=" + secrets["session"] + "\n" +
			"DB_PASSWORD=" + secrets["db"] + "\n" +
			"STASHVENT_API_KEY=" + secrets["sv"] + "\n" +
			"DATABASE_URL=postgres://app:" + secrets["url"] + "@db.internal:5432/app\n",
		"deploy/id_rsa": "-----BEGIN RSA PRIVATE " + "KEY-----\n" + secrets["rsa"] + "\n" + secrets["rsa"] + "\n-----END RSA PRIVATE " + "KEY-----\n",
		"src/client.js": `const apiKey = "` + secrets["constant"] + `";` + "\n" +
			`headers.Authorization = "Bearer ` + secrets["jwt"] + `";` + "\n",
		"ci/token.txt": "GITHUB_TOKEN=" + secrets["github"] + "\n",
		// A placeholder, not a password: not reported.
		"config/db.yml": "url: postgres://app:${DB_PASSWORD}@db.internal/app\n",
	}
	want := map[string]string{
		".env:2":          "aws-access-token",
		".env:3":          "stripe-access-token",
		".env:4":          "generic-api-key",
		".env:5":          "generic-api-key",
		".env:6":          "stashvent-api-key",
		".env:7":          "stashvent-url-password",
		"deploy/id_rsa:1": "private-key",
		"src/client.js:1": "generic-api-key",
		"src/client.js:2": "jwt",
		"ci/token.txt:1":  "github-pat",
	}

	sender := newGitRepo(t, "Test Sender")
	recipient := cloneGitRepo(t, sender)
	for name, content := range files {
		writeTestFile(t, sender, name, content)
	}
	// --all, so a global gitignore that lists .env can't keep it out.
	gitIn(t, sender, "stash", "push", "-q", "--all", "-m", "secrets")
	configDir := t.TempDir()

	cancelled := runCLI(t, s, sender, configDir, "", "push")
	if cancelled.code != 130 || cancelled.stdout != "" {
		t.Fatalf("push: exit %d, stdout %q; want 130 and nothing\n%s", cancelled.code, cancelled.stdout, cancelled.stderr)
	}
	for place, rule := range want {
		if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(place) + ` +` + regexp.QuoteMeta(rule) + `$`).MatchString(cancelled.stderr) {
			t.Errorf("no %s %s in the warning", place, rule)
		}
	}
	if strings.Contains(cancelled.stderr, "config/db.yml") {
		t.Error("a placeholder password was reported")
	}
	for kind, value := range secrets {
		if strings.Contains(cancelled.stderr, value) {
			t.Errorf("the %s secret is in the output", kind)
		}
	}
	if n := s.seen.requestCount(); n != 0 {
		t.Errorf("%d requests reached the server, want 0", n)
	}

	// Shared anyway: the scan warns and never strips.
	link, password := s.pushed(t, sender, configDir, "--ignore-secrets")
	if res := s.pull(t, recipient, password, link); res.code != 0 {
		t.Fatalf("pull: exit %d\n%s", res.code, res.stderr)
	}
	gitIn(t, recipient, "stash", "apply", "-q")
	for name, content := range files {
		if got := readTestFile(t, recipient, name); got != content {
			t.Errorf("%s arrived changed", name)
		}
	}
	if refs := gitIn(t, recipient, "for-each-ref", "refs/share/"); refs != "" {
		t.Errorf("temp refs left in the recipient's repository: %s", refs)
	}
}
