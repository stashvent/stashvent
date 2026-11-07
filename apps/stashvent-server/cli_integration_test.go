//go:build integration

// Run against the Docker stack: make up && make test-integration

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	clicmd "github.com/stashvent/stashvent/apps/stashvent/cmd"
	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/apiclient"
	"github.com/stashvent/stashvent/internal/crypto"
	"github.com/stashvent/stashvent/internal/storage"
)

// cliRun is one run of the stashvent CLI, in-process, against srv. The CLI
// gets its own config directory and an empty environment, so nothing of the
// developer's (history, STASHVENT_SERVER) is read or written.
type cliRun struct {
	code   int
	stdout string
	stderr string
}

func runCLI(t *testing.T, srv *integrationServer, dir, configDir, stdin string, args ...string) cliRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := clicmd.Run(t.Context(), clicmd.Env{
		Version:   "integration-test",
		Args:      append(args, "--server", srv.url),
		Stdin:     strings.NewReader(stdin),
		Stdout:    &stdout,
		Stderr:    &stderr,
		Dir:       dir,
		ConfigDir: configDir,
		Getenv:    func(string) string { return "" },
	})
	return cliRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// gitIn runs git in dir and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: fixed test arguments
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
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

// The happy path of the product through the real CLI commands and the real
// server, Postgres, and object storage: push → list → pull → delete.
func TestCLIHappyPathOnDockerStack(t *testing.T) {
	ctx := t.Context()
	s := newIntegrationServer(t)

	// The sender's repository, and the recipient's clone of it.
	sender := newGitRepo(t, "Test Sender")
	recipient := cloneGitRepo(t, sender)

	// Unfinished work: a changed file and a new one.
	writeTestFile(t, sender, "a.txt", "base\nwork in progress\n")
	writeTestFile(t, sender, "notes.txt", "notes for the handoff\n")
	gitIn(t, sender, "stash", "push", "-q", "--include-untracked", "-m", "e2e handoff")

	senderConfig, recipientConfig := t.TempDir(), t.TempDir()

	// Push.
	push := runCLI(t, s, sender, senderConfig, "", "push", "--expires", "1h")
	if push.code != 0 {
		t.Fatalf("push: exit %d\n%s", push.code, push.stderr)
	}
	link := field(t, push.stdout, "Share link")
	password := field(t, push.stdout, "Password")
	shareID := strings.TrimPrefix(link, s.url+"/")
	if !crypto.ValidShareID(shareID) || len(password) != crypto.PasswordLength {
		t.Fatalf("push printed link %q and a password of %d characters", link, len(password))
	}
	key := "stashes/" + shareID
	s.forget(t, shareID)
	if got := gitIn(t, sender, "stash", "list"); got != "stash@{0}: On main: e2e handoff" {
		t.Errorf("the sender's stash list changed: %q", got)
	}
	// What is stored is the encrypted payload, at the size the server knows.
	share, err := s.client.GetShare(ctx, shareID)
	if err != nil {
		t.Fatalf("the pushed share isn't on the server: %v", err)
	}
	if size, err := s.objects.Size(ctx, key); err != nil || size != share.Size {
		t.Errorf("stored object: size %d, %v; want %d", size, err, share.Size)
	}

	// List.
	list := runCLI(t, s, sender, senderConfig, "", "list")
	if list.code != 0 || !strings.Contains(list.stdout, shareID) || !strings.Contains(list.stdout, "On main: e2e handoff") {
		t.Errorf("list: exit %d\n%s%s", list.code, list.stdout, list.stderr)
	}

	// Pull: a wrong password first, then the right one.
	wrong := runCLI(t, s, recipient, recipientConfig, "not-the-password\n", "pull", "--password-stdin", link)
	if wrong.code != 1 || !strings.Contains(wrong.stderr, "Decryption failed") {
		t.Errorf("pull with a wrong password: exit %d\n%s", wrong.code, wrong.stderr)
	}
	pull := runCLI(t, s, recipient, recipientConfig, password+"\n", "pull", "--password-stdin", link)
	if pull.code != 0 {
		t.Fatalf("pull: exit %d\n%s", pull.code, pull.stderr)
	}
	if got, want := gitIn(t, recipient, "stash", "list"), "stash@{0}: Shared from Test Sender — On main: e2e handoff"; got != want {
		t.Errorf("recipient's stash list = %q, want %q", got, want)
	}
	gitIn(t, recipient, "stash", "apply", "-q")
	for name, want := range map[string]string{"a.txt": "base\nwork in progress\n", "notes.txt": "notes for the handoff\n"} {
		if got := readTestFile(t, recipient, name); got != want {
			t.Errorf("%s after applying the stash = %q, want %q", name, got, want)
		}
	}
	// Only the sender's machine can delete the share.
	if res := runCLI(t, s, recipient, recipientConfig, "", "delete", shareID); res.code != 1 || !strings.Contains(res.stderr, "isn't in this machine's history") {
		t.Errorf("delete from the recipient's machine: exit %d\n%s", res.code, res.stderr)
	}

	// Delete.
	del := runCLI(t, s, sender, senderConfig, "", "delete", shareID)
	if del.code != 0 || !strings.Contains(del.stderr, "✓ Deleted "+shareID) {
		t.Fatalf("delete: exit %d\n%s", del.code, del.stderr)
	}
	var apiErr *apiclient.Error
	if _, err := s.client.GetShare(ctx, shareID); !errors.As(err, &apiErr) || apiErr.Code != api.CodeShareNotFound {
		t.Errorf("get after delete: err = %v, want %s", err, api.CodeShareNotFound)
	}
	if _, err := s.objects.Size(ctx, key); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("object after delete: err = %v, want ErrObjectNotFound", err)
	}
	again := runCLI(t, s, recipient, recipientConfig, password+"\n", "pull", "--password-stdin", link)
	if again.code != 1 || !strings.Contains(again.stderr, "Share not found or expired.") {
		t.Errorf("pull after delete: exit %d\n%s", again.code, again.stderr)
	}
	if list := runCLI(t, s, sender, senderConfig, "", "list"); list.code != 0 || list.stdout != "" {
		t.Errorf("list after delete: exit %d, stdout %q", list.code, list.stdout)
	}
}

// newGitRepo is a repository with one commit, an identity of its own, and
// no signing, so the user's global git config can't change what happens.
func newGitRepo(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	setGitIdentity(t, dir, name)
	writeTestFile(t, dir, "a.txt", "base\n")
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func setGitIdentity(t *testing.T, dir, name string) {
	t.Helper()
	gitIn(t, dir, "config", "user.name", name)
	gitIn(t, dir, "config", "user.email", strings.ToLower(strings.ReplaceAll(name, " ", "."))+"@example.com")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
}

// cloneGitRepo clones src (with extra clone arguments, e.g. --depth 1).
func cloneGitRepo(t *testing.T, src string, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, append(append([]string{"clone", "-q"}, args...), src, ".")...)
	setGitIdentity(t, dir, "Test Recipient")
	return dir
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readTestFile reads a file of the working tree, with the line endings Git
// for Windows may have changed back to LF.
func readTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))) //nolint:gosec // G304: the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
