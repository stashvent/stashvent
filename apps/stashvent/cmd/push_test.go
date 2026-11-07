package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stashvent/stashvent/internal/git"
	"github.com/stashvent/stashvent/internal/security"
)

func TestSecretsGate(t *testing.T) {
	stash := git.Stash{Ref: "stash@{2}", Message: "On main: wip"}
	found := []security.Finding{
		{File: ".env", Line: 3, RuleID: "aws-access-token"},
		{File: "config/app.yml", Line: 12, RuleID: "stripe-access-token"},
	}
	const warning = "⚠ Potential secrets detected in stash@{2}: On main: wip\n" +
		"\n" +
		"  .env:3             aws-access-token\n" +
		"  config/app.yml:12  stripe-access-token\n" +
		"\n" +
		"Secret values are not shown. To review: git stash show -p --include-untracked 2\n"

	// `git stash show` doesn't print what is only staged, so those findings
	// are marked and get their own command.
	foundStaged := []security.Finding{
		{File: ".env", Line: 3, RuleID: "aws-access-token"},
		{File: "app.conf", Line: 2, RuleID: "jwt", Staged: true},
	}
	const warningStaged = "⚠ Potential secrets detected in stash@{2}: On main: wip\n" +
		"\n" +
		"  .env:3      aws-access-token\n" +
		"  app.conf:2  jwt  (staged)\n" +
		"\n" +
		"Secret values are not shown. To review: git stash show -p --include-untracked 2\n" +
		"The ones marked (staged) are only in the index: git show \"stash@{2}^2\"\n"

	tests := []struct {
		name     string
		findings []security.Finding
		ignore   bool
		terminal bool
		input    string
		wantErr  error
		want     string // stderr
	}{
		{"clean stash", nil, false, true, "", nil, "✓ Secret scan passed\n"},
		{"clean stash without a terminal", nil, false, false, "", nil, "✓ Secret scan passed\n"},
		{"answer yes", found, false, true, "y\n", nil, warning + "Share anyway? [y/N] "},
		{"answer no", found, false, true, "n\n", errCancelled, warning + "Share anyway? [y/N] "},
		{"Enter means no", found, false, true, "\n", errCancelled, warning + "Share anyway? [y/N] "},
		{"no terminal cancels", found, false, false, "y\n", errCancelled,
			warning + "Remove them, or pass --ignore-secrets to share anyway.\n"},
		{"--ignore-secrets on a terminal", found, true, true, "", nil, warning + "Continuing (--ignore-secrets).\n"},
		{"--ignore-secrets without a terminal", found, true, false, "", nil, warning + "Continuing (--ignore-secrets).\n"},
		{"staged-only finding", foundStaged, false, true, "n\n", errCancelled, warningStaged + "Share anyway? [y/N] "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder
			a := newApp(Env{Stdin: strings.NewReader(tt.input), Stderr: &stderr, IsTerminal: func() bool { return tt.terminal }})
			err := a.secretsGate(t.Context(), stash, tt.findings, tt.ignore)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("secretsGate error = %v, want %v", err, tt.wantErr)
			}
			if got := stderr.String(); got != tt.want {
				t.Errorf("stderr:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestSecretsGateCapsTheList(t *testing.T) {
	var findings []security.Finding
	for i := range maxFindingsShown + 5 {
		findings = append(findings, security.Finding{File: "keys.txt", Line: i + 1, RuleID: "generic-api-key"})
	}
	var stderr strings.Builder
	a := newApp(Env{Stdin: strings.NewReader(""), Stderr: &stderr})
	_ = a.secretsGate(t.Context(), git.Stash{Ref: "stash@{0}"}, findings, true)

	got := stderr.String()
	if n := strings.Count(got, "generic-api-key"); n != maxFindingsShown {
		t.Errorf("listed %d findings, want %d", n, maxFindingsShown)
	}
	if !strings.Contains(got, "  …and 5 more\n") {
		t.Errorf("no count of the remaining findings:\n%s", got)
	}
}

func TestCleanPath(t *testing.T) {
	deep := strings.Repeat("dir/", 60) // 240 runes
	tests := []struct{ name, path, want string }{
		{"short path", "config/app.yml", "config/app.yml"},
		// File names come from the working tree and reach the terminal.
		{"escape sequence dropped", "a\x1b[2Jb.env", "a[2Jb.env"},
		// The end of a path is what tells two files apart.
		{"long path keeps its end", deep + "one.env", "…" + (deep + "one.env")[len(deep+"one.env")-maxPathLen:]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanPath(tt.path); got != tt.want {
				t.Errorf("cleanPath = %q, want %q", got, tt.want)
			}
		})
	}
	if a, b := cleanPath(deep+"one.env"), cleanPath(deep+"two.env"); a == b {
		t.Errorf("two long paths are shown the same: %q", a)
	}
}

func TestScanFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"too large", fmt.Errorf("wrapped: %w", git.ErrPatchTooLarge), "the stash has more than 32 MB of changes"},
		{"unreadable", security.ErrUnreadablePatch, "git's diff of the stash could not be read"},
		{"git error keeps git's message", errors.New("git stash: exit status 128: fatal: bad object."),
			"git stash: exit status 128: fatal: bad object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scanFailure(tt.err); got != tt.want {
				t.Errorf("scanFailure = %q, want %q", got, tt.want)
			}
		})
	}
}

// End to end through real git and the real rules. Each file hides its
// credential from a plain `git stash show -p` in a different way.
func TestScanStash(t *testing.T) {
	repo := t.TempDir()
	gitRun := func(args ...string) {
		t.Helper()
		args = append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: fixed test arguments
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", args[len(args)-1], err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Assembled at run time so this file holds no secret-shaped text.
	key := func(body string) string { return "AKIA" + body }
	utf16 := func(s string) string {
		b := []byte{0xff, 0xfe} // byte order mark
		for _, c := range []byte(s) {
			b = append(b, c, 0)
		}
		return string(b)
	}

	gitRun("init", "-q")
	write(".gitattributes", "*.lock -diff\n")
	write("app.conf", "name = app\n")
	write("deps.lock", "dep 1\n")
	gitRun("add", ".")
	gitRun("commit", "-q", "-m", "base")

	// Staged, then removed from the working tree: only the index has it.
	write("app.conf", "name = app\nkey_id = "+key("Q3EGUTZ7VLKM2XPD")+"\n")
	gitRun("add", "app.conf")
	write("app.conf", "name = app\n")
	// Untracked.
	write(".env", "PORT=8080\n\nAWS_ACCESS_KEY_ID="+key("W5JHNBT4YRC6ZKDS")+"\n")
	// Binary as far as `git diff` is concerned, because of .gitattributes.
	write("deps.lock", "dep 1\nkey_id = "+key("M4PXD7RNZ2QKHB6T")+"\n")
	// UTF-16, as Windows PowerShell's `>` writes it: binary by git's own guess.
	write("out.txt", utf16("PORT=8080\r\nkey_id = "+key("T6ZC3KWJ5HNYR2DL")+"\r\n"))
	// --all, so a global gitignore that lists .env can't keep it out.
	gitRun("stash", "push", "-q", "--all")

	stash, err := git.FindStash(t.Context(), repo, "stash@{0}")
	if err != nil {
		t.Fatal(err)
	}
	got, err := scanStash(t.Context(), repo, stash)
	if err != nil {
		t.Fatalf("scanStash: %v", err)
	}
	want := []security.Finding{
		{File: ".env", Line: 3, RuleID: "aws-access-token"},
		{File: "app.conf", Line: 2, RuleID: "aws-access-token", Staged: true},
		{File: "deps.lock", Line: 2, RuleID: "aws-access-token"},
		{File: "out.txt", Line: 2, RuleID: "aws-access-token"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %+v, want %+v", got, want)
	}
}
