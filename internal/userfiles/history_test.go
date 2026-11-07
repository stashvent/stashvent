package userfiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)

func testShare(id string, expiresIn time.Duration) Share {
	return Share{
		ShareID:      id,
		Server:       "https://stash.example.com",
		Link:         "https://stash.example.com/" + id,
		StashMessage: "On main: wip",
		CreatedAt:    testNow,
		ExpiresAt:    testNow.Add(expiresIn),
		DeleteToken:  "token-" + id,
	}
}

func ids(shares []Share) string {
	var out []string
	for _, s := range shares {
		out = append(out, s.ShareID)
	}
	return strings.Join(out, ",")
}

func writeHistory(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, historyName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readHistory(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, historyName)) //nolint:gosec // G304: test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHistoryRoundTrip(t *testing.T) {
	// A directory that doesn't exist yet, as on a first run.
	dir := filepath.Join(t.TempDir(), "stashvent")
	h := NewHistory(dir)

	got, err := h.Load(testNow)
	if err != nil || len(got) != 0 {
		t.Fatalf("Load on a first run = %v, %v; want nothing", got, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a read created %s", dir)
	}

	for _, id := range []string{"AAAAAAAAAAAA", "BBBBBBBBBBBB", "CCCCCCCCCCCC"} {
		if err := h.Add(testNow, testShare(id, time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Remove(testNow, "BBBBBBBBBBBB"); err != nil {
		t.Fatal(err)
	}
	if err := h.Remove(testNow, "no-such-share"); err != nil {
		t.Errorf("Remove of an unknown share = %v, want nil", err)
	}
	got, err = h.Load(testNow)
	if err != nil || ids(got) != "AAAAAAAAAAAA,CCCCCCCCCCCC" {
		t.Fatalf("Load = %s, %v; want AAAAAAAAAAAA,CCCCCCCCCCCC", ids(got), err)
	}
	want := testShare("AAAAAAAAAAAA", time.Hour)
	if s := got[0]; s.Server != want.Server || s.Link != want.Link || s.StashMessage != want.StashMessage ||
		!s.CreatedAt.Equal(want.CreatedAt) || !s.ExpiresAt.Equal(want.ExpiresAt) || s.DeleteToken != want.DeleteToken {
		t.Errorf("share came back as %+v, want %+v", s, want)
	}

	// Adding a share again replaces its entry.
	again := testShare("AAAAAAAAAAAA", 2*time.Hour)
	if err := h.Add(testNow, again); err != nil {
		t.Fatal(err)
	}
	got, _ = h.Load(testNow)
	if ids(got) != "CCCCCCCCCCCC,AAAAAAAAAAAA" {
		t.Errorf("after adding a share twice: %s, want CCCCCCCCCCCC,AAAAAAAAAAAA", ids(got))
	}

	// No temp or lock files are left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != historyName {
		t.Errorf("directory holds %v, want only %s", entries, historyName)
	}
}

// The layout is frozen at v0.1, so this text is the contract.
func TestHistoryLayout(t *testing.T) {
	dir := t.TempDir()
	if err := NewHistory(dir).Add(testNow, testShare("8xK29qLm4Tz7", 6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	const want = `{
  "version": 1,
  "shares": [
    {
      "share_id": "8xK29qLm4Tz7",
      "server": "https://stash.example.com",
      "link": "https://stash.example.com/8xK29qLm4Tz7",
      "stash_message": "On main: wip",
      "created_at": "2026-10-13T10:00:00Z",
      "expires_at": "2026-10-13T16:00:00Z",
      "delete_token": "token-8xK29qLm4Tz7"
    }
  ]
}
`
	if got := readHistory(t, dir); got != want {
		t.Errorf("history.json:\n%s\nwant:\n%s", got, want)
	}
}

func TestHistoryEmptyFiles(t *testing.T) {
	for name, content := range map[string]string{
		"empty":           "",
		"only whitespace": " \r\n\t",
		"byte order mark": "\xef\xbb\xbf" + `{"version":1,"shares":[]}`,
		"no shares field": `{"version":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeHistory(t, dir, content)
			h := NewHistory(dir)
			if got, err := h.Load(testNow); err != nil || len(got) != 0 {
				t.Fatalf("Load = %v, %v; want nothing", got, err)
			}
			if err := h.Add(testNow, testShare("AAAAAAAAAAAA", time.Hour)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		})
	}
}

// A file that can't be read as a history is an error, and is left as it is.
func TestHistoryDamagedFiles(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    error
	}{
		{"not JSON", "not json", ErrCorrupt},
		{"cut off", `{"version":1,"shares":[{"share_id":"AAAA`, ErrCorrupt},
		{"an array", `[]`, ErrCorrupt},
		{"shares is not a list", `{"version":1,"shares":"x"}`, ErrCorrupt},
		{"no version", `{"shares":[]}`, ErrCorrupt},
		{"newer version", `{"version":2,"shares":[]}`, ErrNewerVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeHistory(t, dir, tt.content)
			h := NewHistory(dir)

			_, loadErr := h.Load(testNow)
			addErr := h.Add(testNow, testShare("AAAAAAAAAAAA", time.Hour))
			removeErr := h.Remove(testNow, "AAAAAAAAAAAA")
			for op, err := range map[string]error{"Load": loadErr, "Add": addErr, "Remove": removeErr} {
				if !errors.Is(err, tt.want) {
					t.Errorf("%s = %v, want %v", op, err, tt.want)
				}
				if err != nil && !strings.Contains(err.Error(), h.Path()) {
					t.Errorf("%s: the error doesn't name the file: %v", op, err)
				}
			}
			if got := readHistory(t, dir); got != tt.content {
				t.Errorf("the file was changed to %q", got)
			}
		})
	}
}

func TestHistoryDropsExpired(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory(dir)
	for id, expiresIn := range map[string]time.Duration{
		"AAAAAAAAAAAA": time.Hour,
		"BBBBBBBBBBBB": 6 * time.Hour,
	} {
		if err := h.Add(testNow, testShare(id, expiresIn)); err != nil {
			t.Fatal(err)
		}
	}

	// At the moment of expiry a share is already gone.
	later := testNow.Add(time.Hour)
	got, err := h.Load(later)
	if err != nil || ids(got) != "BBBBBBBBBBBB" {
		t.Fatalf("Load after one expired = %s, %v; want BBBBBBBBBBBB", ids(got), err)
	}
	// Its delete token is gone from the file too, after a plain read.
	if file := readHistory(t, dir); strings.Contains(file, "AAAAAAAAAAAA") {
		t.Errorf("the expired share is still in the file:\n%s", file)
	}
}

// A field this version doesn't know was written by a newer stashvent, and
// must still be there after this one rewrites the file.
func TestHistoryKeepsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	writeHistory(t, dir, `{
  "version": 1,
  "shares": [
    {
      "share_id": "AAAAAAAAAAAA",
      "server": "https://stash.example.com",
      "link": "https://stash.example.com/AAAAAAAAAAAA",
      "stash_message": "On main: wip",
      "created_at": "2026-10-13T10:00:00Z",
      "expires_at": "2026-10-13T16:00:00Z",
      "delete_token": "token-AAAAAAAAAAAA",
      "via": {"kind": "api_key"}
    }
  ],
  "settings": {"keep": true}
}`)
	h := NewHistory(dir)
	if err := h.Add(testNow, testShare("BBBBBBBBBBBB", time.Hour)); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Version  int             `json:"version"`
		Settings json.RawMessage `json:"settings"`
		Shares   []struct {
			ShareID     string          `json:"share_id"`
			DeleteToken string          `json:"delete_token"`
			Via         json.RawMessage `json:"via"`
		} `json:"shares"`
	}
	file := readHistory(t, dir)
	if err := json.Unmarshal([]byte(file), &doc); err != nil {
		t.Fatalf("the rewritten file isn't JSON: %v\n%s", err, file)
	}
	if doc.Version != 1 || len(doc.Shares) != 2 {
		t.Fatalf("rewritten file:\n%s", file)
	}
	if string(doc.Settings) == "" || !strings.Contains(string(doc.Settings), "keep") {
		t.Errorf("the top-level field was dropped:\n%s", file)
	}
	if s := doc.Shares[0]; s.ShareID != "AAAAAAAAAAAA" || s.DeleteToken != "token-AAAAAAAAAAAA" || !strings.Contains(string(s.Via), "api_key") {
		t.Errorf("the share's unknown field was dropped:\n%s", file)
	}
	if s := doc.Shares[1]; s.ShareID != "BBBBBBBBBBBB" || s.Via != nil {
		t.Errorf("the new share is wrong:\n%s", file)
	}
}

func TestHistoryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no permission bits; the file relies on the access list of %AppData%")
	}
	dir := filepath.Join(t.TempDir(), "stashvent")
	if err := NewHistory(dir).Add(testNow, testShare("AAAAAAAAAAAA", time.Hour)); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, historyName): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", path, got, want)
		}
	}
}

// Several pushes finishing at once must each keep their entry.
func TestHistoryConcurrentAdds(t *testing.T) {
	h := NewHistory(t.TempDir())
	const writers = 8
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			id := strings.Repeat("ABCDEFGH"[i:i+1], 12)
			errs[i] = h.Add(testNow, testShare(id, time.Hour))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	got, err := h.Load(testNow)
	if err != nil || len(got) != writers {
		t.Errorf("Load = %d shares, %v; want %d", len(got), err, writers)
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json.lock")

	t.Run("held by another stashvent", func(t *testing.T) {
		unlock, err := lock(path)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if _, err := lock(path); !errors.Is(err, ErrLocked) {
			t.Errorf("second lock = %v, want ErrLocked", err)
		}
		if waited := time.Since(start); waited < lockWait {
			t.Errorf("gave up after %v, want at least %v", waited, lockWait)
		}
		unlock()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unlock left the lock file: %v", err)
		}
	})

	// A stashvent that was killed leaves its lock; it must not block the next one for good.
	t.Run("left over", func(t *testing.T) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * lockStale)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		unlock, err := lock(path)
		if err != nil {
			t.Fatalf("lock over a stale lock: %v", err)
		}
		unlock()
	})
}

// A write that fails leaves the previous file whole.
func TestWriteFileKeepsOldFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	if err := writeFile(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	// A directory in the way makes the rename fail on every OS.
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(blocked, []byte("new")); err == nil {
		t.Error("writeFile over a directory = nil, want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file %s was left behind", e.Name())
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "old" { //nolint:gosec // G304: test's own temp dir
		t.Errorf("the old file now holds %q", got)
	}
}
