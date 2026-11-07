package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
)

// newOperator returns the keys commands on the handler's fake store and
// objects, writing to the returned buffers.
func newOperator(h *handler, objs *fakeObjects) (op *operator, stdout, stderr *bytes.Buffer) {
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	store := h.store.(*fakeStore)
	return &operator{store: store, objects: objs, stdout: stdout, stderr: stderr, now: h.now}, stdout, stderr
}

// Bad arguments are reported before the database is touched: DATABASE_URL
// isn't even set here.
func TestKeysCommandArguments(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	for _, args := range [][]string{
		{"keys", "issue"}, // --label is required
		{"keys", "revoke"},
		{"keys", "revoke", "abc"},
		{"keys", "ban", "0"},
		{"keys", "ban", "1", "2"},
		{"keys", "usage", "--days", "0"},
		{"keys", "usage", "-3"},
		{"keys", "list", "extra"},
		{"bogus"},
	} {
		cmd := serverCommand()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err := cmd.Execute()
		if err == nil || strings.Contains(err.Error(), "DATABASE_URL") {
			t.Errorf("%v: err = %v, want an argument error", args, err)
		}
	}
}

func TestKeysIssue(t *testing.T) {
	h, objs := newKeyHandler()
	op, stdout, stderr := newOperator(h, objs)

	if err := op.issue(t.Context(), "  Alice laptop  "); err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSuffix(stdout.String(), "\n")
	if !apikey.Valid(key) || strings.Contains(key, "\n") {
		t.Fatalf("stdout = %q, want one key", stdout)
	}
	// The key is on stdout only; stderr says what was issued.
	if strings.Contains(stderr.String(), key) || !strings.Contains(stderr.String(), "key 1 (Alice laptop)") {
		t.Errorf("stderr = %q", stderr)
	}
	// The server accepts it.
	createWithKey(t, h, objs, key)

	for _, label := range []string{"", "   ", strings.Repeat("x", maxLabelLength+1), "tab\there", "two\nlines", "esc\x1b[31m"} {
		if err := op.issue(t.Context(), label); err == nil {
			t.Errorf("issue(%q) succeeded", label)
		}
	}
	if err := op.issue(t.Context(), strings.Repeat("é", maxLabelLength)); err != nil {
		t.Errorf("a label of %d characters: %v", maxLabelLength, err)
	}
}

func TestKeysList(t *testing.T) {
	h, objs := newKeyHandler()
	h.now = func() time.Time { return time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC) }
	op, stdout, stderr := newOperator(h, objs)
	ctx := t.Context()

	if err := op.list(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "No keys yet") {
		t.Errorf("empty list: stdout %q, stderr %q", stdout, stderr)
	}

	alice, _ := issueKey(t, h, "alice")
	issueKey(t, h, "bob")
	issueKey(t, h, "carol")
	store := h.store.(*fakeStore)
	_, _ = store.RevokeAPIKey(ctx, 2)
	_, _ = store.BanAPIKey(ctx, 3)

	stdout.Reset()
	if err := op.list(ctx, ""); err != nil {
		t.Fatal(err)
	}
	want := "ID  LABEL  CREATED     STATUS\n" +
		"1   alice  2026-10-17  active\n" +
		"2   bob    2026-10-17  revoked 2026-10-17\n" +
		"3   carol  2026-10-17  banned 2026-10-17\n"
	if stdout.String() != want {
		t.Errorf("list:\n%s\nwant:\n%s", stdout, want)
	}

	// A label written by the hosted service may hold anything; it is printed
	// without control characters.
	t.Run("a label with control characters", func(t *testing.T) {
		issueKey(t, h, "x\tFAKE\x1b[2J\nrow") // straight into the store, as the hosted service would
		stdout.Reset()
		if err := op.list(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(stdout.String(), "\x1b") || !strings.Contains(stdout.String(), "x?FAKE?[2J?row") {
			t.Errorf("list:\n%q", stdout)
		}
	})

	t.Run("by share", func(t *testing.T) {
		created := createWithKey(t, h, objs, alice)
		stdout.Reset()
		if err := op.list(ctx, created.ShareID); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), "1   alice") || strings.Contains(stdout.String(), "bob") {
			t.Errorf("list --share:\n%s", stdout)
		}

		h.abuse.requireAPIKey = false
		keyless := createUploaded(t, h, objs)
		stderr.Reset()
		if err := op.list(ctx, keyless.ShareID); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr.String(), "created without an API key") {
			t.Errorf("stderr = %q", stderr)
		}
		if err := op.list(ctx, unknownID); err == nil {
			t.Error("an unknown share was listed")
		}
	})
}

func TestKeysRevoke(t *testing.T) {
	h, objs := newKeyHandler()
	op, _, stderr := newOperator(h, objs)
	mux := newMux(h)
	key, k := issueKey(t, h, "leaked")
	created := createWithKey(t, h, objs, key)
	path := "/v1/shares/" + created.ShareID
	if rec := do(t, mux, http.MethodPost, path+"/complete", created.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
	}

	if err := op.revoke(t.Context(), k.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Revoked key 1 (leaked)") {
		t.Errorf("stderr = %q", stderr)
	}
	// Pushes and deletes with the key are refused...
	wantError(t, doKey(t, mux, http.MethodPost, "/v1/shares", "", key, validCreate()), http.StatusUnauthorized, api.CodeInvalidAPIKey)
	wantError(t, doKey(t, mux, http.MethodDelete, path, "", key, nil), http.StatusUnauthorized, api.CodeInvalidAPIKey)
	// ...but its share still pulls.
	if rec := do(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusOK {
		t.Errorf("a revoked key's share: get = %d (%s)", rec.Code, rec.Body)
	}

	stderr.Reset()
	if err := op.revoke(t.Context(), k.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "already revoked") {
		t.Errorf("second revoke: stderr = %q", stderr)
	}
	if err := op.revoke(t.Context(), 99); err == nil {
		t.Error("revoking an unknown key succeeded")
	}
}

func TestKeysBan(t *testing.T) {
	h, objs := newKeyHandler()
	op, _, stderr := newOperator(h, objs)
	mux := newMux(h)
	abuser, k := issueKey(t, h, "abuser")
	other, _ := issueKey(t, h, "other")

	active := createWithKey(t, h, objs, abuser)
	if rec := do(t, mux, http.MethodPost, "/v1/shares/"+active.ShareID+"/complete", active.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
	}
	pending := createWithKey(t, h, objs, abuser)
	failing := createWithKey(t, h, objs, abuser)
	untouched := createWithKey(t, h, objs, other)
	if rec := do(t, mux, http.MethodPost, "/v1/shares/"+untouched.ShareID+"/complete", untouched.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
	}

	// The first run can't delete one object.
	failingKey := "stashes/" + failing.ShareID
	objs.deleteErr[failingKey] = errors.New("storage down")
	err := op.ban(t.Context(), k.ID)
	if err == nil || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("ban with a failure: err = %v", err)
	}
	if !strings.Contains(stderr.String(), "Ended 2 shares") || !strings.Contains(stderr.String(), failing.ShareID) {
		t.Errorf("stderr = %q", stderr)
	}
	if !isLive(h, failing.ShareID) {
		t.Error("a share whose object couldn't be deleted was ended")
	}
	// The key is banned already, so the key can't push meanwhile.
	wantError(t, doKey(t, mux, http.MethodPost, "/v1/shares", "", abuser, validCreate()), http.StatusUnauthorized, api.CodeInvalidAPIKey)

	// Running it again finishes the job.
	delete(objs.deleteErr, failingKey)
	stderr.Reset()
	if err := op.ban(t.Context(), k.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Ended 1 share.") {
		t.Errorf("second run: stderr = %q", stderr)
	}

	for _, s := range []api.CreateShareResponse{active, pending, failing} {
		if _, ok := objs.sizes["stashes/"+s.ShareID]; ok || isLive(h, s.ShareID) {
			t.Errorf("banned key's share %s: object kept %v, live %v", s.ShareID, ok, isLive(h, s.ShareID))
		}
	}
	wantError(t, do(t, mux, http.MethodGet, "/v1/shares/"+active.ShareID, "", nil), http.StatusNotFound, api.CodeShareNotFound)
	if rec := do(t, mux, http.MethodGet, "/v1/shares/"+untouched.ShareID, "", nil); rec.Code != http.StatusOK {
		t.Errorf("another key's share: get = %d (%s)", rec.Code, rec.Body)
	}
	banned, _ := h.store.(*fakeStore).GetAPIKey(t.Context(), k.ID)
	if banned.BannedAt == nil || banned.RevokedAt == nil {
		t.Errorf("key after ban = %+v, want banned and revoked", banned)
	}
	if err := op.ban(t.Context(), 99); err == nil {
		t.Error("banning an unknown key succeeded")
	}
}

func TestKeysUsage(t *testing.T) {
	h, objs := newKeyHandler()
	day := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return day }
	op, stdout, _ := newOperator(h, objs)
	alice, _ := issueKey(t, h, "alice")
	issueKey(t, h, "bob")

	h.now = func() time.Time { return day.AddDate(0, 0, -1) }
	createWithKey(t, h, objs, alice)
	h.now = func() time.Time { return day }
	createWithKey(t, h, objs, alice)
	createWithKey(t, h, objs, alice)

	if err := op.usage(t.Context(), 0, 30); err != nil {
		t.Fatal(err)
	}
	want := "ID  LABEL  TODAY  SHARES (30d)  BYTES (30d)\n" +
		"1   alice  2      3             300\n" +
		"2   bob    0      0             0\n"
	if stdout.String() != want {
		t.Errorf("usage:\n%s\nwant:\n%s", stdout, want)
	}

	stdout.Reset()
	if err := op.usage(t.Context(), 1, 30); err != nil {
		t.Fatal(err)
	}
	want = "DAY         SHARES  BYTES\n" +
		"2026-10-16  1       100\n" +
		"2026-10-17  2       200\n"
	if stdout.String() != want {
		t.Errorf("usage of one key:\n%s\nwant:\n%s", stdout, want)
	}

	// --days 1 is today only.
	stdout.Reset()
	if err := op.usage(t.Context(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "2026-10-16") {
		t.Errorf("usage --days 1:\n%s", stdout)
	}
	if err := op.usage(t.Context(), 99, 30); err == nil {
		t.Error("usage of an unknown key succeeded")
	}
}
