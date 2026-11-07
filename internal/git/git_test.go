package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitT runs git for test setup with a fixed identity, independent of the
// machine's git config.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	args = append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)
	out, err := run(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // G304: test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(got), "\r\n", "\n")
}

// newRepo creates a repository with one commit of a.txt.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	writeFile(t, filepath.Join(dir, "a.txt"), "base\n")
	gitT(t, dir, "add", "a.txt")
	gitT(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func clone(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "clone", "-q", src, ".")
	return dir
}

// bundleStash bundles ref from dir into a temp file.
func bundleStash(t *testing.T, dir, ref string) (string, BundleInfo) {
	t.Helper()
	s, err := FindStash(t.Context(), dir, ref)
	if err != nil {
		t.Fatalf("FindStash(%s): %v", ref, err)
	}
	path := filepath.Join(t.TempDir(), "stash.bundle")
	info, err := CreateStashBundle(t.Context(), dir, s, path)
	if err != nil {
		t.Fatalf("CreateStashBundle: %v", err)
	}
	return path, info
}

func stashCount(t *testing.T, dir string) int {
	t.Helper()
	stashes, err := ListStashes(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(stashes)
}

func TestStashBundleRoundTrip(t *testing.T) {
	ctx := t.Context()
	sender := newRepo(t)
	recipient := clone(t, sender)

	writeFile(t, filepath.Join(sender, "a.txt"), "changed\n")
	writeFile(t, filepath.Join(sender, "new.txt"), "untracked\n")
	gitT(t, sender, "stash", "push", "-u", "-m", "wip")

	s, err := FindStash(ctx, sender, "stash@{0}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(s.Message, ": wip") {
		t.Errorf("Message = %q, want suffix %q", s.Message, ": wip")
	}
	if n, err := FileCount(ctx, sender, s.SHA); err != nil || n != 2 {
		t.Errorf("FileCount = %d, %v; want 2 (one tracked, one untracked)", n, err)
	}

	bundle, info := bundleStash(t, sender, "stash@{0}")
	if !strings.HasPrefix(info.Ref, "refs/share/") {
		t.Errorf("info.Ref = %q, want refs/share/…", info.Ref)
	}
	if head := gitT(t, sender, "rev-parse", "HEAD"); info.Base != head {
		t.Errorf("info.Base = %q, want HEAD %q", info.Base, head)
	}
	if v, err := BundleVersion(bundle); err != nil || v != 2 {
		t.Errorf("BundleVersion = %d, %v; want 2 for a sha1 repo", v, err)
	}
	if refs := gitT(t, sender, "for-each-ref", "refs/share/"); refs != "" {
		t.Errorf("temp refs left in sender: %q", refs)
	}

	// The ref comes from the (untrusted) manifest; anything but the bundle's
	// own refs/share/ head is refused before fetching.
	for _, bad := range []string{"refs/heads/main", "refs/share/other", "refs/share/a..b", "-refs/share/x"} {
		if _, err := ImportBundle(ctx, recipient, bundle, bad, "x"); err == nil {
			t.Errorf("ImportBundle accepted ref %q", bad)
		}
	}

	res, err := ImportBundle(ctx, recipient, bundle, info.Ref, "Shared via stashvent test")
	if err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	if res != (ImportResult{Index: 0}) {
		t.Errorf("ImportResult = %+v, want a new entry at 0", res)
	}

	list := gitT(t, recipient, "stash", "list")
	if n := len(strings.Split(list, "\n")); list == "" || n != 1 {
		t.Fatalf("recipient stash list = %q, want one entry", list)
	}
	if !strings.Contains(list, "Shared via stashvent test") {
		t.Errorf("stash message missing: %q", list)
	}

	// Importing the same share again adds nothing and says where it is.
	res, err = ImportBundle(ctx, recipient, bundle, info.Ref, "again")
	if err != nil {
		t.Fatalf("second ImportBundle: %v", err)
	}
	if res != (ImportResult{AlreadyStashed: true, Index: 0}) || stashCount(t, recipient) != 1 {
		t.Errorf("second import = %+v with %d entries, want AlreadyStashed at 0 and one entry", res, stashCount(t, recipient))
	}

	gitT(t, recipient, "stash", "apply", "-q")
	for file, want := range map[string]string{"a.txt": "changed\n", "new.txt": "untracked\n"} {
		if got := readFile(t, filepath.Join(recipient, file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
}

func TestStagedOnlyStash(t *testing.T) {
	ctx := t.Context()
	sender := newRepo(t)
	recipient := clone(t, sender)

	writeFile(t, filepath.Join(sender, "staged.txt"), "staged\n")
	gitT(t, sender, "add", "staged.txt")
	gitT(t, sender, "stash", "push", "-q")

	bundle, info := bundleStash(t, sender, "stash@{0}")
	if _, err := ImportBundle(ctx, recipient, bundle, info.Ref, "staged"); err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	gitT(t, recipient, "stash", "apply", "-q", "--index")
	if status := gitT(t, recipient, "status", "--porcelain"); status != "A  staged.txt" {
		t.Errorf("status after apply --index = %q, want the file staged", status)
	}
}

// A stash keeps the index and the working tree separately; both must survive
// the bundle, even for one file with different staged and unstaged content.
func TestStagedAndUnstagedSameFile(t *testing.T) {
	ctx := t.Context()
	sender := newRepo(t)
	recipient := clone(t, sender)

	writeFile(t, filepath.Join(sender, "a.txt"), "staged\n")
	gitT(t, sender, "add", "a.txt")
	writeFile(t, filepath.Join(sender, "a.txt"), "unstaged\n")
	writeFile(t, filepath.Join(sender, "new.txt"), "untracked\n")
	gitT(t, sender, "stash", "push", "-q", "-u")

	bundle, info := bundleStash(t, sender, "stash@{0}")
	if _, err := ImportBundle(ctx, recipient, bundle, info.Ref, "mixed"); err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	gitT(t, recipient, "stash", "apply", "-q", "--index")

	if got := strings.ReplaceAll(gitT(t, recipient, "show", ":a.txt"), "\r\n", "\n"); got != "staged" {
		t.Errorf("staged a.txt = %q, want %q", got, "staged")
	}
	if got := readFile(t, filepath.Join(recipient, "a.txt")); got != "unstaged\n" {
		t.Errorf("working tree a.txt = %q, want %q", got, "unstaged\n")
	}
	if got := readFile(t, filepath.Join(recipient, "new.txt")); got != "untracked\n" {
		t.Errorf("new.txt = %q, want %q", got, "untracked\n")
	}
	if status := gitT(t, recipient, "status", "--porcelain"); status != "MM a.txt\n?? new.txt" {
		t.Errorf("status = %q, want a.txt staged and modified, new.txt untracked", status)
	}
}

// The patch for secret scanning must show everything the bundle carries
// (working tree, untracked files, index), in the same format whatever the
// user's diff config and .gitattributes say.
func TestStashPatch(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, ".gitattributes"), "*.lock -diff\n") // "binary" to git diff
	writeFile(t, filepath.Join(repo, "deps.lock"), "dep 1\n")
	writeFile(t, filepath.Join(repo, "lines.txt"), "l1\nl2\nl3\nl4\nl5\nl6\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "more files")

	gitT(t, repo, "config", "diff.noprefix", "true")
	gitT(t, repo, "config", "color.ui", "always")
	gitT(t, repo, "config", "diff.external", "echo")      // would replace the patch with echo's arguments
	gitT(t, repo, "config", "diff.interHunkContext", "3") // would join the two hunks of lines.txt

	writeFile(t, filepath.Join(repo, "a.txt"), "index-only\n")
	gitT(t, repo, "add", "a.txt")
	writeFile(t, filepath.Join(repo, "a.txt"), "worktree\n")
	writeFile(t, filepath.Join(repo, "new.txt"), "untracked\n")
	writeFile(t, filepath.Join(repo, "deps.lock"), "dep 1\nadded-to-lock\n")
	writeFile(t, filepath.Join(repo, "lines.txt"), "l1\nNEW2\nl3\nl4\nNEW5\nl6\n")
	writeFile(t, filepath.Join(repo, "utf16.txt"), "\xff\xfeh\x00i\x00\n\x00") // binary by git's own guess
	gitT(t, repo, "stash", "push", "-q", "-u")

	s, err := FindStash(ctx, repo, "stash@{0}")
	if err != nil {
		t.Fatal(err)
	}
	patch, err := StashPatch(ctx, repo, s.SHA)
	if err != nil {
		t.Fatalf("StashPatch: %v", err)
	}
	worktree := strings.ReplaceAll(patch.Worktree, "\r\n", "\n")
	index := strings.ReplaceAll(patch.Index, "\r\n", "\n")

	for _, want := range []string{
		"+++ b/a.txt\n", "+worktree\n", // working tree
		"+++ b/new.txt\n", "+untracked\n", // untracked
		"+added-to-lock\n",                           // marked -diff in .gitattributes
		"+++ b/utf16.txt\n", "+\xff\xfeh\x00i\x00\n", // UTF-16
		"\n@@ -2 +2 @@", "\n@@ -5 +5 @@", // one hunk per change
	} {
		if !strings.Contains(worktree, want) {
			t.Errorf("working tree patch is missing %q:\n%s", want, worktree)
		}
	}
	// Staged, then changed again: only the index has it.
	if !strings.Contains(index, "+++ b/a.txt\n") {
		t.Errorf("index patch is missing %q:\n%s", "+++ b/a.txt\n", index)
	}
	if !strings.Contains(index, "+index-only\n") || strings.Contains(worktree, "index-only") {
		t.Errorf("want +index-only in the index patch only.\nindex:\n%s\nworking tree:\n%s", index, worktree)
	}
	for name, p := range map[string]string{"working tree": worktree, "index": index} {
		if strings.Contains(p, "\x1b[") {
			t.Errorf("%s patch contains colour codes:\n%q", name, p)
		}
		if strings.Contains(p, "Binary files") {
			t.Errorf("%s patch leaves a file out as binary:\n%s", name, p)
		}
		if strings.HasPrefix(p, " ") || strings.Contains(p, "\n ") {
			t.Errorf("%s patch contains context lines:\n%s", name, p)
		}
	}

	// The other ways config can change the a/ and b/ prefixes.
	gitT(t, repo, "config", "diff.noprefix", "false")
	gitT(t, repo, "config", "diff.mnemonicPrefix", "true")
	gitT(t, repo, "config", "diff.dstPrefix", "new/") // Git 2.45+; older Git ignores it
	patch, err = StashPatch(ctx, repo, s.SHA)
	if err != nil {
		t.Fatalf("StashPatch: %v", err)
	}
	for name, p := range map[string]string{"working tree": patch.Worktree, "index": patch.Index} {
		if !strings.Contains(strings.ReplaceAll(p, "\r\n", "\n"), "\n--- a/a.txt\n+++ b/a.txt\n") {
			t.Errorf("%s patch doesn't use the a/ and b/ prefixes:\n%s", name, p)
		}
	}

	if _, err := StashPatch(ctx, repo, "stash@{0}"); err == nil {
		t.Error("StashPatch accepted a ref; want an error for anything but a full SHA")
	}
}

// A stash with a huge file must not be read into memory to the end.
func TestStashPatchTooLarge(t *testing.T) {
	ctx := t.Context()
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "big.txt"), strings.Repeat("0123456789abcdef\n", 4096)) // 68 KB
	gitT(t, repo, "stash", "push", "-q", "-u")
	s, err := FindStash(ctx, repo, "stash@{0}")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := stashPatch(ctx, repo, s.SHA, 1024); !errors.Is(err, ErrPatchTooLarge) {
		t.Errorf("stashPatch with a 1 KB limit: error = %v, want ErrPatchTooLarge", err)
	}
	if _, err := stashPatch(ctx, repo, s.SHA, 1<<20); err != nil {
		t.Errorf("stashPatch with a 1 MB limit: %v", err)
	}
}

func TestSelectOlderStash(t *testing.T) {
	ctx := t.Context()
	sender := newRepo(t)
	recipient := clone(t, sender)

	for _, content := range []string{"first\n", "second\n", "third\n"} {
		writeFile(t, filepath.Join(sender, "a.txt"), content)
		gitT(t, sender, "stash", "push", "-q", "-m", strings.TrimSpace(content))
	}
	if n := stashCount(t, sender); n != 3 {
		t.Fatalf("sender has %d stashes, want 3", n)
	}

	s, err := FindStash(ctx, sender, "stash@{1}")
	if err != nil {
		t.Fatal(err)
	}
	if want := gitT(t, sender, "rev-parse", "stash@{1}"); s.SHA != want || !strings.HasSuffix(s.Message, ": second") {
		t.Errorf("FindStash(stash@{1}) = %+v, want SHA %s and message \"…: second\"", s, want)
	}
	if _, err := FindStash(ctx, sender, "stash@{9}"); !errors.Is(err, ErrStashNotFound) {
		t.Errorf("FindStash(stash@{9}) error = %v, want ErrStashNotFound", err)
	}

	bundle, info := bundleStash(t, sender, "stash@{1}")
	if _, err := ImportBundle(ctx, recipient, bundle, info.Ref, "second"); err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	gitT(t, recipient, "stash", "apply", "-q")
	if got := readFile(t, filepath.Join(recipient, "a.txt")); got != "second\n" {
		t.Errorf("a.txt = %q, want the second stash's content", got)
	}
}

func TestMissingBaseCommit(t *testing.T) {
	ctx := t.Context()
	sender := newRepo(t)
	recipient := clone(t, sender) // cloned before the sender's next commit

	writeFile(t, filepath.Join(sender, "a.txt"), "newer base\n")
	gitT(t, sender, "commit", "-q", "-am", "unpushed")
	writeFile(t, filepath.Join(sender, "a.txt"), "wip\n")
	gitT(t, sender, "stash", "push", "-q")

	bundle, info := bundleStash(t, sender, "stash@{0}")
	if HasCommit(ctx, recipient, info.Base) {
		t.Error("HasCommit = true for a commit the recipient doesn't have")
	}
	if !HasCommit(ctx, sender, info.Base) {
		t.Error("HasCommit = false for the sender's own base commit")
	}
	if _, err := ImportBundle(ctx, recipient, bundle, info.Ref, "x"); err == nil {
		t.Error("ImportBundle succeeded without the base commit")
	}
	if n := stashCount(t, recipient); n != 0 {
		t.Errorf("recipient has %d stashes after a failed import, want 0", n)
	}
}

func TestIsShallow(t *testing.T) {
	src := newRepo(t)
	writeFile(t, filepath.Join(src, "a.txt"), "second\n")
	gitT(t, src, "commit", "-q", "-am", "second")

	shallow := t.TempDir()
	// A local path normally ignores --depth; --no-local makes it a real
	// shallow clone. Not a file:// URL: under `make test` on Windows
	// (MSYS_NO_PATHCONV=1) git can't open file:///C:/….
	gitT(t, shallow, "clone", "-q", "--depth", "1", "--no-local", src, ".")
	if !IsShallow(t.Context(), shallow) {
		t.Error("IsShallow = false for a --depth 1 clone")
	}
	if IsShallow(t.Context(), src) {
		t.Error("IsShallow = true for a full repository")
	}
	// Its "root" is the cut-off commit, so the hash differs from the real one.
	full, _ := RepoHash(t.Context(), src, "HEAD")
	cut, _ := RepoHash(t.Context(), shallow, "HEAD")
	if full == cut {
		t.Error("RepoHash of a shallow clone matches the full repository's; the shallow check is pointless")
	}
}

func TestHasCommitRejectsNonSHA(t *testing.T) {
	dir := newRepo(t)
	for _, s := range []string{"", "HEAD", "--all", gitT(t, dir, "rev-parse", "--short", "HEAD")} {
		if HasCommit(t.Context(), dir, s) {
			t.Errorf("HasCommit(%q) = true, want false for anything but a full SHA", s)
		}
	}
}

func TestRepoHash(t *testing.T) {
	ctx := t.Context()
	a := newRepo(t)
	aClone := clone(t, a)
	other := newRepo(t)

	hash := func(dir string) string {
		t.Helper()
		h, err := RepoHash(ctx, dir, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if len(hash(a)) != 64 {
		t.Errorf("RepoHash = %q, want 64 hex characters", hash(a))
	}
	if hash(a) != hash(aClone) {
		t.Error("RepoHash differs between a repository and its clone")
	}
	// Unrelated repos with identical first commits can share a root, so
	// give this one a different first commit.
	gitT(t, other, "commit", "-q", "--amend", "-m", "different root")
	if hash(a) == hash(other) {
		t.Error("RepoHash is the same for unrelated repositories")
	}
	if _, err := RepoHash(ctx, t.TempDir(), "HEAD"); err == nil {
		t.Error("RepoHash succeeded outside a repository")
	}
}

func TestCreateStashBundleCleansUpOnFailure(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "wip\n")
	gitT(t, dir, "stash", "push", "-q")
	s, err := FindStash(t.Context(), dir, "stash@{0}")
	if err != nil {
		t.Fatal(err)
	}

	// The temp ref exists by the time `git bundle create` fails here.
	bad := filepath.Join(t.TempDir(), "missing", "s.bundle")
	if _, err := CreateStashBundle(t.Context(), dir, s, bad); err == nil {
		t.Fatal("CreateStashBundle succeeded writing into a missing directory")
	}
	if refs := gitT(t, dir, "for-each-ref", "refs/share/"); refs != "" {
		t.Errorf("temp refs left after a failure: %q", refs)
	}
}

func TestFindStashNoStash(t *testing.T) {
	_, err := FindStash(t.Context(), newRepo(t), "stash@{0}")
	if !errors.Is(err, ErrNoStash) {
		t.Errorf("err = %v, want ErrNoStash", err)
	}
}

func TestParseStashRef(t *testing.T) {
	valid := map[string]string{
		"stash@{0}":  "stash@{0}",
		"stash@{12}": "stash@{12}",
		"3":          "stash@{3}",
		"007":        "stash@{7}",
	}
	for in, want := range valid {
		if got, err := ParseStashRef(in); err != nil || got != want {
			t.Errorf("ParseStashRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "stash@{-1}", "stash@{0}x", "HEAD", "stash", "stash@{}", "--all", "99999999999999999999"} {
		if got, err := ParseStashRef(in); err == nil {
			t.Errorf("ParseStashRef(%q) = %q, want an error", in, got)
		}
	}
}

func TestStashBranch(t *testing.T) {
	tests := map[string]string{
		"WIP on main: 80ff2c1 base":        "main",
		"On feature/payment-retry: wip":    "feature/payment-retry",
		"WIP on (no branch): 80ff2c1 base": "",
		"Shared from Alice — On main: wip": "",
		"On main":                          "",
		"":                                 "",
	}
	for msg, want := range tests {
		if got := StashBranch(msg); got != want {
			t.Errorf("StashBranch(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestRepoNameFromURL(t *testing.T) {
	tests := map[string]string{
		"https://github.com/company/payments-api.git":           "company/payments-api",
		"https://user:token@github.com/a/b.git":                 "a/b",
		"https://gitlab.com/group/sub/project":                  "sub/project",
		"ssh://git@github.com:22/company/payments-api.git":      "company/payments-api",
		"git@github.com:company/payments-api.git":               "company/payments-api",
		"git@github.com:payments-api.git":                       "payments-api",
		`E:\projects\stash-vent`:                                "projects/stash-vent",
		"/home/alice/src/repo/":                                 "src/repo",
		"https://github.com/company/payments-api.git?token=abc": "company/payments-api",
		"": "",
	}
	for in, want := range tests {
		if got := repoNameFromURL(in); got != want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		have string
		want bool
	}{
		{"2.32.0.windows.2", true},
		{"2.39.3 (Apple Git-145)", true},
		{"2.45.0.rc1", true},
		{"3.0.0", true},
		{"2.31.8", false},
		{"2.25.1", false},
		{"1.99.0", false},
		{"unknown build", true}, // unparseable passes
	}
	for _, tt := range tests {
		if got := versionAtLeast(tt.have, MinVersion); got != tt.want {
			t.Errorf("versionAtLeast(%q, %s) = %v, want %v", tt.have, MinVersion, got, tt.want)
		}
	}
}
