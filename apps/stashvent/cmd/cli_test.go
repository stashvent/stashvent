package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/apiclient"
	"github.com/stashvent/stashvent/internal/crypto"
	"github.com/stashvent/stashvent/internal/git"
)

// The main outputs, as golden files: a change to any of them has to be made
// on purpose (-update) and shows up in review.
func TestGoldenPushPull(t *testing.T) {
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "payment retry WIP")

	sender := newCLI(t, repo, f)
	push := sender.run("push")
	if push.code != exitOK {
		t.Fatalf("push: exit %d\n%s", push.code, push.stderr)
	}
	golden(t, "push.stdout.golden", push.stdout, f)
	golden(t, "push.stderr.golden", push.stderr, f)

	// The recipient is at a terminal and types the password.
	link := f.URL + "/" + fakeIDs[0]
	receiver := newCLI(t, recipient, nil)
	receiver.terminal = true
	receiver.secrets = []string{testPassword}
	pull := receiver.run("pull", link)
	if pull.code != exitOK {
		t.Fatalf("pull: exit %d\n%s", pull.code, pull.stderr)
	}
	golden(t, "pull.stdout.golden", pull.stdout, f)
	golden(t, "pull.stderr.golden", pull.stderr, f)
	if got, want := gitRun(t, recipient, "stash", "list"), "stash@{0}: Shared from Test Sender — On main: payment retry WIP"; got != want {
		t.Errorf("recipient's stash list = %q, want %q", got, want)
	}
	gitRun(t, recipient, "stash", "apply", "-q")
	got, err := os.ReadFile(filepath.Join(recipient, "notes.txt")) //nolint:gosec // G304: test's own temp dir
	// Git for Windows may check the file out with CRLF.
	if text := strings.ReplaceAll(string(got), "\r\n", "\n"); err != nil || text != "notes for payment retry WIP\n" {
		t.Errorf("the applied stash didn't bring notes.txt: %q, %v", got, err)
	}

	// A second share, twenty minutes later: the sender's list has both,
	// newest first.
	addStash(t, repo, "second idea")
	sender.now = testNow.Add(20 * time.Minute)
	if second := sender.run("push", "--expires", "7d"); second.code != exitOK {
		t.Fatalf("second push: exit %d\n%s", second.code, second.stderr)
	}
	list := sender.run("list")
	lines := strings.Split(strings.TrimSpace(list.stdout), "\n")
	if list.code != exitOK || list.stderr != "" || len(lines) != 3 ||
		!strings.HasPrefix(lines[1], fakeIDs[1]+"  6d 23h ") || !strings.HasSuffix(lines[1], "  On main: second idea") ||
		!strings.HasPrefix(lines[2], fakeIDs[0]+"  5h 40m ") || !strings.HasSuffix(lines[2], "  On main: payment retry WIP") {
		t.Errorf("list: exit %d, stderr %q\n%s", list.code, list.stderr, list.stdout)
	}
}

func TestGoldenSecretScanWarning(t *testing.T) {
	f := newFakeServer(t)
	repo := newRepo(t)
	// Assembled at run time so this file holds no secret-shaped text.
	writeFile(t, repo, ".env", "PORT=8080\n\nAWS_ACCESS_KEY_ID="+"AKIA"+"W5JHNBT4YRC6ZKDS"+"\n")
	// --all, so a global gitignore that lists .env can't keep it out.
	gitRun(t, repo, "stash", "push", "-q", "--all", "-m", "config for staging")

	// No terminal, so nobody can be asked: the push is cancelled.
	res := newCLI(t, repo, f).run("push")
	if res.code != exitCancelled || res.stdout != "" {
		t.Fatalf("push: exit %d, stdout %q; want %d and nothing", res.code, res.stdout, exitCancelled)
	}
	golden(t, "push_secrets.stderr.golden", res.stderr, f)
	if n := f.requests(); n != 0 {
		t.Errorf("%d requests reached the server, want 0", n)
	}
}

func TestGoldenHelp(t *testing.T) {
	tests := []struct {
		golden string
		args   []string
	}{
		{"help.golden", nil}, // bare stashvent
		{"help_push.golden", []string{"push", "--help"}},
		{"help_pull.golden", []string{"pull", "--help"}},
		{"help_list.golden", []string{"list", "--help"}},
		{"help_delete.golden", []string{"delete", "--help"}},
		{"help_login.golden", []string{"login", "--help"}},
		{"help_logout.golden", []string{"logout", "--help"}},
	}
	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			res := newCLI(t, t.TempDir(), nil).run(tt.args...)
			if res.code != exitOK || res.stderr != "" {
				t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
			}
			golden(t, tt.golden, res.stdout, nil)
		})
	}
	// "stashvent help", "--help", and no arguments all show the same page.
	bare := newCLI(t, t.TempDir(), nil).run()
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		if res := newCLI(t, t.TempDir(), nil).run(args...); res.stdout != bare.stdout || res.code != exitOK {
			t.Errorf("stashvent %s differs from bare stashvent (exit %d)", args[0], res.code)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // in stderr
	}{
		{"unknown command", []string{"psh"}, `unknown command "psh". Did you mean "push"?`},
		{"unknown flag", []string{"push", "--nope"}, `Run "stashvent push --help" for usage.`},
		{"too many arguments", []string{"push", "0", "1"}, "accepts at most 1 arg"},
		{"not a stash", []string{"push", "main"}, `"main" is not a stash`},
		{"bad --expires", []string{"push", "--expires", "2h"}, "--expires must be 1h, 6h, 24h, or 7d"},
		{"both password flags", []string{"push", "--password", "--password-stdin"}, "not both"},
		{"pull without a link", []string{"pull"}, `Run "stashvent pull --help" for usage.`},
		{"pull with a bad link", []string{"pull", "not a link"}, "is not a share link or ID"},
		{"delete without an ID", []string{"delete"}, `Run "stashvent delete --help" for usage.`},
		{"delete with a bad ID", []string{"delete", "short"}, "is not a share link or ID"},
		{"list with an argument", []string{"list", "x"}, `Run "stashvent list --help" for usage.`},
		{"insecure server", []string{"push", "--server", "http://example.com"}, "refusing insecure http:// server"},
		{"server with a path", []string{"push", "--server", "https://example.com/api"}, "only a scheme and a host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeServer(t)
			res := newCLI(t, t.TempDir(), f).run(tt.args...)
			if res.code != exitUsage {
				t.Errorf("exit %d, want %d", res.code, exitUsage)
			}
			if !strings.Contains(res.stderr, tt.want) || !strings.HasPrefix(res.stderr, "✗ ") {
				t.Errorf("stderr = %q, want it to hold %q", res.stderr, tt.want)
			}
			if res.stdout != "" || f.requests() != 0 {
				t.Errorf("stdout %q and %d requests, want neither", res.stdout, f.requests())
			}
		})
	}
}

// A password typed where it doesn't belong is in the shell history already.
// It must not also be printed.
func TestPasswordOnCommandLineIsNotRepeated(t *testing.T) {
	const typed = "hunter2-hunter2"
	for _, args := range [][]string{
		{"push", "--password=" + typed},
		{"push", "--password", typed},
		{"push", "--password-stdin", typed},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			f := newFakeServer(t)
			c := newCLI(t, t.TempDir(), f)
			c.stdin = "a-password-from-stdin\n"
			res := c.run(args...)
			if res.code != exitUsage || !strings.Contains(res.stderr, "never taken from the command line") {
				t.Errorf("exit %d, stderr %q; want a usage error about the command line", res.code, res.stderr)
			}
			if strings.Contains(res.stderr+res.stdout, typed) {
				t.Errorf("the password was repeated: %q", res.stderr)
			}
			if f.requests() != 0 {
				t.Errorf("%d requests reached the server, want 0", f.requests())
			}
		})
	}
}

func TestVersion(t *testing.T) {
	tests := []struct {
		commit string
		want   string
	}{
		{"", "stashvent test\n"},
		{"1a2b3c4", "stashvent test (commit 1a2b3c4)\n"},
	}
	for _, tt := range tests {
		for _, arg := range []string{"version", "--version"} {
			c := newCLI(t, t.TempDir(), nil)
			c.commit = tt.commit
			res := c.run(arg)
			if res.code != exitOK || res.stdout != tt.want {
				t.Errorf("commit %q: stashvent %s = exit %d, %q; want %q", tt.commit, arg, res.code, res.stdout, tt.want)
			}
		}
	}
}

func TestServerURL(t *testing.T) {
	tests := []struct {
		name     string
		flag     string
		env      string
		want     string
		wantFrom serverSource
		wantErr  string
	}{
		{"default", "", "", defaultServer, serverFromDefault, ""},
		{"environment", "", "https://env.example.com", "https://env.example.com", serverFromEnv, ""},
		{"flag wins over the environment", "https://flag.example.com", "https://env.example.com", "https://flag.example.com", serverFromFlag, ""},
		{"normalised", "HTTPS://Flag.Example.com/", "", "https://flag.example.com", serverFromFlag, ""},
		{"bad environment value names its source", "", "nonsense", "", 0, "(from STASHVENT_SERVER)"},
		{"bad flag names its source", "http://example.com", "", "", 0, "(from --server)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newApp(Env{Stdin: strings.NewReader(""), Getenv: func(key string) string {
				if key == serverEnv {
					return tt.env
				}
				return ""
			}})
			a.server = tt.flag
			got, err := a.serverURL()
			if tt.wantErr != "" {
				if !errors.As(err, new(usageError)) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("serverURL = %q, %v; want a usage error holding %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want || a.serverFrom != tt.wantFrom {
				t.Errorf("serverURL = %q from %v, %v; want %q from %v", got, a.serverFrom, err, tt.want, tt.wantFrom)
			}
		})
	}
}

// The built-in server is the hosted instance, over HTTPS, written in the one
// form servers are compared and stored in: otherwise push would say "Server:
// … (the default)" for the default itself, and config.json keys saved for it
// would sit under another spelling.
func TestDefaultServerIsTheHostedInstance(t *testing.T) {
	if defaultServer != "https://stashvent.dev" {
		t.Errorf("defaultServer = %q, want https://stashvent.dev", defaultServer)
	}
	normalized, err := apiclient.NormalizeServer(defaultServer)
	if err != nil || normalized != defaultServer {
		t.Errorf("NormalizeServer(%q) = %q, %v; want it unchanged", defaultServer, normalized, err)
	}
}

func TestPushSaysWhichServer(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")

	c := newCLI(t, repo, f)
	if res := c.run("push"); !strings.HasPrefix(res.stderr, "Server: "+f.URL+" (from STASHVENT_SERVER)\n") {
		t.Errorf("push with the server from the environment starts with %q", firstLine(res.stderr))
	}
	c.env = map[string]string{}
	if res := c.run("push", "--server", f.URL); !strings.HasPrefix(res.stderr, "Server: "+f.URL+" (from --server)\n") {
		t.Errorf("push with --server starts with %q", firstLine(res.stderr))
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// A server that can't be reached gets a plain sentence: no Go error text,
// and no advice to send the work somewhere else.
func TestUnreachableServer(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	dead := f.URL
	f.Close() // nothing listens there any more
	repo := newRepo(t)
	addStash(t, repo, "wip")

	c := newCLI(t, repo, nil)
	res := c.run("push", "--server", dead)
	want := "✗ Could not reach " + dead + ". Check that it is running and that you are connected, then try again.\n" +
		"That server was set with --server.\n"
	if res.code != exitError || !strings.HasSuffix(res.stderr, want) {
		t.Errorf("exit %d, stderr:\n%s\nwant it to end with:\n%s", res.code, res.stderr, want)
	}
	for _, raw := range []string{"dial tcp", "connectex", "connection refused", "Post \""} {
		if strings.Contains(res.stderr, raw) {
			t.Errorf("stderr holds raw error text %q:\n%s", raw, res.stderr)
		}
	}
	if c.historyFile() != "" {
		t.Errorf("a failed push left a history:\n%s", c.historyFile())
	}
}

func TestPushExpires(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")

	res := newCLI(t, repo, f).run("push", "--expires", "24h")
	if res.code != exitOK {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	if got := f.share(fakeIDs[0]).req.ExpiresIn; got != "24h" {
		t.Errorf("the server was asked for %q, want 24h", got)
	}
	if want := "Expires:    2026-10-14 10:50 UTC (in 24 hours)\n"; !strings.HasSuffix(res.stdout, want) {
		t.Errorf("stdout = %q, want it to end with %q", res.stdout, want)
	}
}

// Every --expires value the server accepts has its wording.
func TestExpiresLabels(t *testing.T) {
	for value := range api.ExpiresIn {
		if expiresLabel[value] == "" {
			t.Errorf("--expires %s has no label", value)
		}
	}
	if len(expiresLabel) != len(api.ExpiresIn) {
		t.Errorf("%d labels for %d values", len(expiresLabel), len(api.ExpiresIn))
	}
}

func TestPushOwnPassword(t *testing.T) {
	t.Parallel()
	const own = "correct horse battery"
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "wip")

	c := newCLI(t, repo, f)
	c.terminal = true
	// A password that is too short, then one repeated wrongly, then right.
	c.secrets = []string{"short", own, own + "x", own, own}
	res := c.run("push", "--password")
	if res.code != exitOK {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	for _, want := range []string{
		"Password: \n✗ The password must be at least 12 characters. Try again (2 of 3).\n",
		"Password: \nRepeat password: \n✗ The passwords don't match. Try again (3 of 3).\n",
		"The password is the one you chose.",
	} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, res.stderr)
		}
	}
	// A typed password is never shown, and the generated one wasn't used.
	if out := res.stdout + res.stderr; strings.Contains(out, own) || strings.Contains(out, testPassword) || strings.Contains(res.stdout, "Password") {
		t.Errorf("a password is on screen:\n%s\n%s", res.stdout, res.stderr)
	}
	if h := c.historyFile(); strings.Contains(h, own) {
		t.Errorf("the password is in the history:\n%s", h)
	}

	// It is the password that decrypts the share.
	receiver := newCLI(t, recipient, nil)
	receiver.stdin = own + "\n"
	if pull := receiver.run("pull", "--password-stdin", f.URL+"/"+fakeIDs[0]); pull.code != exitOK {
		t.Errorf("pull with the chosen password: exit %d\n%s", pull.code, pull.stderr)
	}
}

func TestPushOwnPasswordGivesUp(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")

	c := newCLI(t, repo, f)
	c.terminal = true
	c.secrets = []string{"one", "two", "three"}
	res := c.run("push", "--password")
	if res.code != exitError || !strings.HasSuffix(res.stderr, "✗ The password must be at least 12 characters.\n") {
		t.Errorf("exit %d, stderr:\n%s", res.code, res.stderr)
	}
	if f.requests() != 0 {
		t.Errorf("%d requests reached the server, want 0", f.requests())
	}

	// Without a terminal there is nobody to ask.
	c.terminal = false
	if res := c.run("push", "--password"); res.code != exitUsage || !strings.Contains(res.stderr, "use --password-stdin") {
		t.Errorf("--password without a terminal: exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestPushPasswordStdin(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "wip")
	c := newCLI(t, repo, f)

	for stdin, want := range map[string]string{
		"":          "✗ No password on stdin.\n",
		"\n":        "✗ No password on stdin.\n",
		"too short": "✗ The password must be at least 12 characters.\n",
	} {
		c.stdin = stdin
		if res := c.run("push", "--password-stdin"); res.code != exitError || !strings.HasSuffix(res.stderr, want) {
			t.Errorf("stdin %q: exit %d, stderr %q; want %q", stdin, res.code, res.stderr, want)
		}
	}
	if f.requests() != 0 {
		t.Fatalf("%d requests reached the server before a usable password, want 0", f.requests())
	}

	// Spaces are part of the password; only the line ending is dropped.
	const own = "  spaces matter here "
	c.stdin = own + "\r\n"
	res := c.run("push", "--password-stdin")
	if res.code != exitOK || strings.Contains(res.stdout+res.stderr, own) {
		t.Fatalf("exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	link := f.URL + "/" + fakeIDs[0]
	receiver := newCLI(t, recipient, nil)
	receiver.stdin = strings.TrimSpace(own) + "\n"
	if pull := receiver.run("pull", "--password-stdin", link); pull.code != exitError || !strings.Contains(pull.stderr, "Decryption failed") {
		t.Errorf("pull with the spaces trimmed: exit %d, stderr %q; want a decryption failure", pull.code, pull.stderr)
	}
	receiver.stdin = own + "\n"
	if pull := receiver.run("pull", "--password-stdin", link); pull.code != exitOK {
		t.Errorf("pull with the exact password: exit %d\n%s", pull.code, pull.stderr)
	}
}

func TestPickStash(t *testing.T) {
	stashes := []git.Stash{
		{Ref: "stash@{0}", SHA: "a", Message: "On main: newest"},
		{Ref: "stash@{1}", SHA: "b", Message: "WIP on main: 1a2b3c4 older\x1b[31m"},
		{Ref: "stash@{2}", SHA: "c", Message: "On feature/x: oldest"},
	}
	const list = "Stashes in this repository:\n\n" +
		"  0  On main: newest\n" +
		"  1  WIP on main: 1a2b3c4 older[31m\n" + // the escape character is gone
		"  2  On feature/x: oldest\n\n"
	const ask = "Stash to share [0]: "
	const again = "✗ Enter a number from 0 to 2.\n"

	tests := []struct {
		name    string
		input   string
		want    string // the chosen ref
		wantErr error
		stderr  string
	}{
		{"Enter picks the newest", "\n", "stash@{0}", nil, list + ask},
		{"a number", "2\n", "stash@{2}", nil, list + ask},
		{"git's own name", " stash@{1} \n", "stash@{1}", nil, list + ask},
		{"a wrong answer is asked again", "9\nx\n1\n", "stash@{1}", nil, list + ask + again + ask + again + ask},
		{"the end of input cancels", "", "", errCancelled, list + ask + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder
			a := newApp(Env{Stdin: strings.NewReader(tt.input), Stderr: &stderr, IsTerminal: func() bool { return true }})
			got, err := a.pickStash(t.Context(), stashes)
			if !errors.Is(err, tt.wantErr) || got.Ref != tt.want {
				t.Errorf("pickStash = %q, %v; want %q, %v", got.Ref, err, tt.want, tt.wantErr)
			}
			if stderr.String() != tt.stderr {
				t.Errorf("stderr:\n%q\nwant:\n%q", stderr.String(), tt.stderr)
			}
		})
	}

	t.Run("three wrong answers", func(t *testing.T) {
		a := newApp(Env{Stdin: strings.NewReader("7\n8\n9\n0\n"), Stderr: io.Discard, IsTerminal: func() bool { return true }})
		_, err := a.pickStash(t.Context(), stashes)
		if !errors.As(err, new(userError)) {
			t.Errorf("pickStash = %v, want a user error", err)
		}
	})

	t.Run("a long list is cut", func(t *testing.T) {
		var many []git.Stash
		for i := range maxStashesShown + 3 {
			many = append(many, git.Stash{Ref: fmt.Sprintf("stash@{%d}", i), Message: "m"})
		}
		var stderr strings.Builder
		a := newApp(Env{Stdin: strings.NewReader("\n"), Stderr: &stderr, IsTerminal: func() bool { return true }})
		if _, err := a.pickStash(t.Context(), many); err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(stderr.String(), "  m\n"); got != maxStashesShown || !strings.Contains(stderr.String(), "  …and 3 older\n") {
			t.Errorf("listed %d stashes:\n%s", got, stderr.String())
		}
	})
}

// Which stash a push takes, through real git.
func TestChooseStash(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	ctx := t.Context()
	with := func(terminal bool, input string) *app {
		return newApp(Env{Stdin: strings.NewReader(input), Stderr: io.Discard, IsTerminal: func() bool { return terminal }})
	}

	if _, err := with(true, "").chooseStash(ctx, repo, ""); !strings.Contains(errText(err), "No stash to share") {
		t.Errorf("no stashes: %v", err)
	}

	addStash(t, repo, "older")
	// One stash: nothing to ask, even on a terminal with no input.
	if got, err := with(true, "").chooseStash(ctx, repo, ""); err != nil || got.Message != "On main: older" {
		t.Errorf("one stash = %q, %v", got.Message, err)
	}

	addStash(t, repo, "newer")
	tests := []struct {
		name     string
		terminal bool
		input    string
		ref      string
		want     string
		wantErr  string
	}{
		{"named", true, "", "stash@{1}", "On main: older", ""},
		{"named, beyond the list", true, "", "stash@{5}", "", "stash@{5} doesn't exist"},
		{"picked at the prompt", true, "1\n", "", "On main: older", ""},
		{"no terminal takes the newest", false, "1\n", "", "On main: newer", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := with(tt.terminal, tt.input).chooseStash(ctx, repo, tt.ref)
			if got.Message != tt.want || !strings.Contains(errText(err), tt.wantErr) || (tt.wantErr == "" && err != nil) {
				t.Errorf("chooseStash = %q, %v; want %q, error %q", got.Message, err, tt.want, tt.wantErr)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sizeHint ends every message about a stash that is too large.
const sizeHint = "Large or binary files are the usual cause. To see what it holds: git stash show --stat --include-untracked 0\n"

// A stash over the limit stops before a password is asked for and before
// anything is sent. The test lowers the limit to 1 MB, so a 2 MB file is
// enough; the bundle check catches it before the bundle is read.
func TestPushTooLarge(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	// Bytes that don't compress, so the bundle is as large as the file. They
	// are the same on every run: a chain of SHA-256 hashes.
	var big []byte
	block := sha256.Sum256([]byte("stashvent"))
	for len(big) < 2<<20 {
		big = append(big, block[:]...)
		block = sha256.Sum256(block[:])
	}
	if err := os.WriteFile(filepath.Join(repo, "video.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "stash", "push", "-q", "--include-untracked", "-m", "with a video")

	c := newCLI(t, repo, f)
	c.maxPayload = 1 << 20
	c.terminal = true // --password would prompt; no answers are queued
	// --ignore-secrets keeps the scanner's opinion of 2 MB of noise out of
	// a test about size.
	res := c.run("push", "--password", "--ignore-secrets")
	want := " MB once packaged, and the limit is 1 MB.\n" + sizeHint
	if res.code != exitError || !strings.HasSuffix(res.stderr, want) {
		t.Errorf("exit %d, stderr:\n%s\nwant it to end with:\n%s", res.code, res.stderr, want)
	}
	if strings.Contains(res.stderr, "Password") || f.requests() != 0 {
		t.Errorf("a password was asked for or %d requests were sent:\n%s", f.requests(), res.stderr)
	}
}

// The exact check: the bundle fits, but the manifest and the GCM tag take the
// payload over the limit. It still stops before the password.
func TestPushTooLargeOnceEncrypted(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")
	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}
	f.mu.Lock()
	size := f.shares[fakeIDs[0]].req.Size // the same stash encrypts to the same size
	f.mu.Unlock()
	requests := f.requests()

	c := newCLI(t, repo, f)
	c.maxPayload = size - 1
	c.terminal = true
	res := c.run("push", "--password")
	want := " once packaged, and the limit is " + formatSize(size-1) + ".\n" + sizeHint
	if res.code != exitError || !strings.HasSuffix(res.stderr, want) {
		t.Errorf("exit %d, stderr:\n%s\nwant it to end with:\n%s", res.code, res.stderr, want)
	}
	if strings.Contains(res.stderr, "Password") || f.requests() != requests {
		t.Errorf("a password was asked for or %d requests were sent:\n%s", f.requests()-requests, res.stderr)
	}
}

// Packaging and the size check come before the scan, so a stash too large to
// send says so, instead of failing the scan on its size and suggesting
// --ignore-secrets for a push that would fail anyway. One that fits and is too
// large to scan gets the scan's failure.
func TestPushTooLargeToScan(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")
	scanErr := fmt.Errorf("reading the diff: %w", git.ErrPatchTooLarge)

	tooBig := newCLI(t, repo, f)
	tooBig.scanErr = scanErr
	tooBig.maxPayload = 100 // bytes; the bundle is larger
	res := tooBig.run("push")
	if res.code != exitError || !strings.HasSuffix(res.stderr, " once packaged, and the limit is 100 bytes.\n"+sizeHint) ||
		strings.Contains(res.stderr, "--ignore-secrets") {
		t.Errorf("too large: exit %d, stderr:\n%s", res.code, res.stderr)
	}

	fits := newCLI(t, repo, f)
	fits.scanErr = scanErr
	res = fits.run("push")
	want := "✗ The secret scan failed, so nothing was shared: the stash has more than 32 MB of changes.\n" +
		"Pass --ignore-secrets to share without the scan.\n"
	// Packaging ran first, but quietly: no "✓ Packaged" before the failure.
	if res.code != exitError || !strings.HasSuffix(res.stderr, want) || strings.Contains(res.stderr, "Packaged") {
		t.Errorf("fits: exit %d, stderr:\n%s", res.code, res.stderr)
	}
	if n := f.requests(); n != 0 {
		t.Errorf("%d requests sent, want none", n)
	}
}

// Rounded, a size just over the limit reads like the limit itself.
func TestSizeOver(t *testing.T) {
	tests := []struct {
		size, limit int64
		want        string
	}{
		{5<<20 + 1, 5 << 20, "just over 5 MB"},
		{5<<20 + 40_000, 5 << 20, "just over 5 MB"},
		{5<<20 + 200_000, 5 << 20, "5.2 MB"},
		{6 << 20, 5 << 20, "6 MB"},
		{101, 100, "101 bytes"},
	}
	for _, tt := range tests {
		if got := sizeOver(tt.size, tt.limit); got != tt.want {
			t.Errorf("sizeOver(%d, %d) = %q, want %q", tt.size, tt.limit, got, tt.want)
		}
	}
}

// Outside tests the limit is the shared constant, the server's default.
func TestTheLimitIsTheDefault(t *testing.T) {
	if got := newApp(Env{}).maxPayload; got != api.DefaultMaxPayloadBytes {
		t.Errorf("maxPayload = %d, want api.DefaultMaxPayloadBytes (%d)", got, api.DefaultMaxPayloadBytes)
	}
}

func TestFormatSize(t *testing.T) {
	tests := map[int64]string{
		0:               "0 bytes",
		812:             "812 bytes",
		1 << 10:         "1 KB",
		342192:          "334 KB",
		5 << 20:         "5 MB",
		5<<20 + 1:       "5 MB",
		1887436:         "1.8 MB",
		7<<20 + 300_000: "7.3 MB",
	}
	for n, want := range tests {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// The server's own limit is the authority. A server set lower than the CLI's
// number answers 413, and that is reported with the size.
func TestPushRefusedByServer(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.maxPayload = 100 // bytes; any real stash is larger
	repo := newRepo(t)
	addStash(t, repo, "wip")

	res := newCLI(t, repo, f).run("push")
	if res.code != exitError || !strings.Contains(res.stderr, " once encrypted, which is more than this server accepts.\n") {
		t.Errorf("exit %d, stderr:\n%s", res.code, res.stderr)
	}
	// Nothing was created, so there is nothing to clean up.
	if f.called("delete") != 0 || strings.Contains(res.stderr, "unfinished share") {
		t.Errorf("cleanup ran for a share that was never created:\n%s", res.stderr)
	}

	// Any other failure to create a share is the generic message, with the
	// request ID for support.
	f.failWith("create", http.StatusInternalServerError)
	res = newCLI(t, repo, f).run("push")
	if res.code != exitError || !strings.HasSuffix(res.stderr, "✗ The server returned an unexpected error (500). Request ID: TESTREQUESTID\n") {
		t.Errorf("exit %d, stderr:\n%s", res.code, res.stderr)
	}
}

// For invalid_request and for a code this version has no wording for, the
// server's message is the only explanation there is: it's how a newer server
// tells this CLI what's wrong (an upgrade, a new limit). It is cleaned like
// any outside text and capped; internal stays the generic message.
func TestServerMessageForUnknownCodes(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")

	const id = " Request ID: TESTREQUESTID\n"
	server := "✗ The server at " + f.URL + " returned an error"
	tests := []struct {
		name    string
		failure fakeFailure
		want    string
	}{
		{"a code from a newer server",
			fakeFailure{status: http.StatusUpgradeRequired, code: "upgrade_required", message: "This server needs stashvent v0.3 or newer."},
			server + ` (HTTP 426): "This server needs stashvent v0.3 or newer.".` + id},
		{"invalid_request",
			fakeFailure{status: http.StatusBadRequest, code: api.CodeInvalidRequest, message: "expires_in must be one of 1h, 6h, 24h"},
			server + ` (HTTP 400): "expires_in must be one of 1h, 6h, 24h".` + id},
		{"terminal escapes and newlines cleaned",
			fakeFailure{status: http.StatusServiceUnavailable, code: "maintenance", message: "Back at\x1b[31m 14:00\nUTC"},
			server + ` (HTTP 503): "Back at[31m 14:00 UTC".` + id},
		{"capped",
			fakeFailure{status: http.StatusBadRequest, code: "too_long", message: strings.Repeat("x", 300)},
			server + ` (HTTP 400): "` + strings.Repeat("x", 200) + `…".` + id},
		{"nothing to show",
			fakeFailure{status: http.StatusTeapot, code: "teapot", message: " "},
			"✗ The server returned an unexpected error (418)." + id},
		{"internal stays generic",
			fakeFailure{status: http.StatusInternalServerError, code: api.CodeInternal, message: "pq: relation shares does not exist"},
			"✗ The server returned an unexpected error (500)." + id},
		// \xe2\x80\xae is U+202E (right-to-left override), written as bytes so
		// the source holds no bidi character (gosec G116).
		{"the request ID cleaned and capped",
			fakeFailure{status: http.StatusInternalServerError, code: api.CodeInternal, requestID: "A\xe2\x80\xaeB\u009b31mC" + strings.Repeat("x", 100)},
			"✗ The server returned an unexpected error (500). Request ID: AB31mC" + strings.Repeat("x", 58) + "…\n"},
	}
	for _, tt := range tests {
		f.failWithCode("create", tt.failure)
		res := newCLI(t, repo, f).run("push")
		if res.code != exitError || !strings.HasSuffix(res.stderr, tt.want) {
			t.Errorf("%s: exit %d, stderr:\n%s\nwant it to end with:\n%s", tt.name, res.code, res.stderr, tt.want)
		}
	}
}

// Headers the server signed into the upload URL go out with the upload.
func TestPushSendsTheUploadHeaders(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.uploadHeaders = map[string]string{"X-Amz-Server-Side-Encryption": "AES256"}
	sent := make(chan string, 1)
	f.onUpload = func(r *http.Request) { sent <- r.Header.Get("X-Amz-Server-Side-Encryption") }
	repo := newRepo(t)
	addStash(t, repo, "wip")

	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}
	if got := <-sent; got != "AES256" {
		t.Errorf("X-Amz-Server-Side-Encryption = %q, want %q", got, "AES256")
	}
}

func TestPushRecordsTheShare(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "payment retry WIP")

	c := newCLI(t, repo, f)
	if res := c.run("push"); res.code != exitOK {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	var doc struct {
		Version int `json:"version"`
		Shares  []struct {
			ShareID      string    `json:"share_id"`
			Server       string    `json:"server"`
			Link         string    `json:"link"`
			StashMessage string    `json:"stash_message"`
			CreatedAt    time.Time `json:"created_at"`
			ExpiresAt    time.Time `json:"expires_at"`
			DeleteToken  string    `json:"delete_token"`
		} `json:"shares"`
	}
	file := c.historyFile()
	if err := json.Unmarshal([]byte(file), &doc); err != nil || doc.Version != 1 || len(doc.Shares) != 1 {
		t.Fatalf("history.json: %v\n%s", err, file)
	}
	s := doc.Shares[0]
	if s.ShareID != fakeIDs[0] || s.Server != f.URL || s.Link != f.URL+"/"+fakeIDs[0] ||
		s.StashMessage != "On main: payment retry WIP" || s.DeleteToken != "delete-token-"+fakeIDs[0] ||
		!s.CreatedAt.Equal(testNow) || !s.ExpiresAt.Equal(testNow.Add(6*time.Hour)) {
		t.Errorf("history entry: %+v", s)
	}
	if strings.Contains(file, testPassword) {
		t.Errorf("the password is in the history:\n%s", file)
	}
	// The delete token is for the history file only.
	if res := c.run("list"); strings.Contains(res.stdout+res.stderr, s.DeleteToken) {
		t.Errorf("list shows the delete token:\n%s", res.stdout)
	}
}

// A history that can't be used stops the push before anything leaves the
// machine: a share that can't be recorded couldn't be deleted later.
func TestPushStopsOnDamagedHistory(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")
	c := newCLI(t, repo, f)
	if err := os.MkdirAll(c.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.configDir, "history.json")

	for content, want := range map[string]string{
		"{ not json":                "✗ The share history can't be read: " + path + "\n",
		`{"version":2,"shares":[]}`: "✗ The share history was written by a newer stashvent: " + path + "\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"push"}, {"list"}, {"delete", fakeIDs[0]}} {
			res := c.run(args...)
			if res.code != exitError || !strings.Contains(res.stderr, want) {
				t.Errorf("%s: exit %d, stderr:\n%s\nwant it to hold:\n%s", args[0], res.code, res.stderr, want)
			}
		}
		if got := c.historyFile(); got != content {
			t.Errorf("the damaged file was rewritten as %q", got)
		}
	}
	if f.requests() != 0 {
		t.Errorf("%d requests reached the server, want 0", f.requests())
	}
}

// Once the share exists on the server, a push that doesn't finish takes it
// away again.
func TestPushCleansUpAfterFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		sabotage  func(f *fakeServer, cancel context.CancelFunc)
		wantCode  int
		wantLines string // the end of stderr
	}{
		{"the upload fails",
			func(f *fakeServer, _ context.CancelFunc) { f.failWith("upload", http.StatusInternalServerError) },
			exitError, "The unfinished share was removed from the server.\n✗ Storage didn't accept the upload (HTTP 500). Try again in a moment.\n"},
		{"/complete fails",
			func(f *fakeServer, _ context.CancelFunc) { f.failWith("complete", http.StatusInternalServerError) },
			exitError, "The unfinished share was removed from the server.\n✗ The server returned an unexpected error (500). Request ID: TESTREQUESTID\n"},
		{"Ctrl+C during the upload",
			func(f *fakeServer, cancel context.CancelFunc) {
				f.onUpload = func(*http.Request) { cancel() }
			},
			exitCancelled, "The unfinished share was removed from the server.\n✗ Cancelled\n"},
		{"the cleanup fails too",
			func(f *fakeServer, _ context.CancelFunc) {
				f.failWith("upload", http.StatusInternalServerError)
				f.failWith("delete", http.StatusInternalServerError)
			},
			exitError, "⚠ The unfinished share could not be removed from the server. Its link was never shown,\n" +
				"  and the server deletes it on its own.\n✗ Storage didn't accept the upload (HTTP 500). Try again in a moment.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeServer(t)
			repo := newRepo(t)
			addStash(t, repo, "wip")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tt.sabotage(f, cancel)

			c := newCLI(t, repo, f)
			res := c.runCtx(ctx, "push")
			if res.code != tt.wantCode || !strings.HasSuffix(res.stderr, tt.wantLines) {
				t.Errorf("exit %d, stderr:\n%s\nwant exit %d and the end:\n%s", res.code, res.stderr, tt.wantCode, tt.wantLines)
			}
			if res.stdout != "" {
				t.Errorf("a link was printed for a share that didn't finish: %q", res.stdout)
			}
			// The DELETE carried this share's token.
			if f.called("delete") != 1 || f.tokens[len(f.tokens)-1] != "delete-token-"+fakeIDs[0] {
				t.Errorf("DELETE calls = %d, tokens = %v", f.called("delete"), f.tokens)
			}
			if tt.name != "the cleanup fails too" && f.share(fakeIDs[0]) != nil {
				t.Error("the share is still on the server")
			}
			if h := c.historyFile(); strings.Contains(h, fakeIDs[0]) {
				t.Errorf("the unfinished share is in the history:\n%s", h)
			}
		})
	}
}

// The link is shown only after the share is in the history. If the result
// can't be printed, the share is of no use to anyone and is removed, from
// the server and from the history.
func TestPushCleansUpWhenTheResultCantBePrinted(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")

	c := newCLI(t, repo, f)
	var stderr strings.Builder
	a := newApp(Env{
		Version: "test", Stdin: strings.NewReader(""), Stdout: brokenWriter{}, Stderr: &stderr,
		Dir: c.dir, ConfigDir: c.configDir,
		Getenv: func(key string) string { return c.env[key] },
		Now:    func() time.Time { return testNow },
	})
	if code := a.run(t.Context(), []string{"push"}); code != exitError {
		t.Errorf("exit %d, want %d\n%s", code, exitError, stderr.String())
	}
	if f.share(fakeIDs[0]) != nil || f.called("delete") != 1 {
		t.Error("the share is still on the server")
	}
	if h := c.historyFile(); strings.Contains(h, fakeIDs[0]) {
		t.Errorf("the share is still in the history:\n%s", h)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("stdout is closed") }

func TestDelete(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	c := newCLI(t, repo, f)
	push := func() string {
		t.Helper()
		addStash(t, repo, "wip")
		before := f.called("create")
		if res := c.run("push"); res.code != exitOK {
			t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
		}
		return fakeIDs[before]
	}
	inHistory := func(id string) bool { return strings.Contains(c.historyFile(), id) }

	t.Run("deleted", func(t *testing.T) {
		id := push()
		// --server and the environment say something else; the request must
		// still go to the server the share was pushed to.
		c.env[serverEnv] = "https://elsewhere.example"
		res := c.run("delete", id, "--server", "https://another.example")
		c.env[serverEnv] = f.URL
		if res.code != exitOK || res.stderr != "✓ Deleted "+id+"\n" || res.stdout != "" {
			t.Fatalf("exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
		}
		if f.share(id) != nil || inHistory(id) {
			t.Error("the share is still on the server or in the history")
		}

		// A second delete has nothing to go on.
		res = c.run("delete", id)
		if res.code != exitError || !strings.Contains(res.stderr, id+" isn't in this machine's history") {
			t.Errorf("second delete: exit %d, stderr %q", res.code, res.stderr)
		}
	})

	t.Run("by link", func(t *testing.T) {
		id := push()
		if res := c.run("delete", f.URL+"/"+id); res.code != exitOK || f.share(id) != nil {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
	})

	t.Run("already gone from the server", func(t *testing.T) {
		id := push()
		f.forget(id)
		res := c.run("delete", id)
		if res.code != exitOK || res.stderr != "✓ "+id+" was already gone from the server. Removed it from the history.\n" {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
		if inHistory(id) {
			t.Error("the entry is still in the history")
		}
	})

	t.Run("the server refuses", func(t *testing.T) {
		id := push()
		f.failWith("delete", http.StatusInternalServerError)
		res := c.run("delete", id)
		f.failWith("delete", 0)
		if res.code != exitError || !strings.Contains(res.stderr, "unexpected error (500)") {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
		// The entry stays, so the delete can be tried again.
		if !inHistory(id) {
			t.Fatal("the entry left the history although the share wasn't deleted")
		}
		if res := c.run("delete", id); res.code != exitOK {
			t.Errorf("retry: exit %d, stderr %q", res.code, res.stderr)
		}
	})

	t.Run("a share this machine never pushed", func(t *testing.T) {
		before := f.requests()
		res := c.run("delete", "AAAAAAAAAAAA")
		if res.code != exitError || !strings.Contains(res.stderr, "AAAAAAAAAAAA isn't in this machine's history") {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
		if f.requests() != before {
			t.Error("a request was sent without a delete token to send")
		}
	})
}

// The history file can be edited. The delete token must still only go to a
// server the CLI would accept on the command line.
func TestDeleteChecksTheStoredServer(t *testing.T) {
	c := newCLI(t, t.TempDir(), nil)
	if err := os.MkdirAll(c.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	history := `{"version":1,"shares":[{"share_id":"8xK29qLm4Tz7","server":"http://evil.example",` +
		`"link":"http://evil.example/8xK29qLm4Tz7","stash_message":"m","created_at":"2026-10-13T10:00:00Z",` +
		`"expires_at":"2026-10-14T10:00:00Z","delete_token":"secret-token"}]}`
	if err := os.WriteFile(filepath.Join(c.configDir, "history.json"), []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	res := c.run("delete", "8xK29qLm4Tz7")
	if res.code != exitError || !strings.Contains(res.stderr, "no usable server") || strings.Contains(res.stderr, "secret-token") {
		t.Errorf("exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestList(t *testing.T) {
	c := newCLI(t, t.TempDir(), nil)
	res := c.run("list")
	if res.code != exitOK || res.stdout != "" || res.stderr != "No active shares were sent from this machine.\n" {
		t.Errorf("empty list: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if _, err := os.Stat(c.configDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("list created %s", c.configDir)
	}

	if err := os.MkdirAll(c.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The golden file is made from a history written here, not from pushes:
	// the columns are aligned to the real links, so the test server's
	// address can't be swapped for a placeholder afterwards.
	history := `{"version":1,"shares":[
		{"share_id":"8xK29qLm4Tz7","server":"https://stashvent.example","link":"https://stashvent.example/8xK29qLm4Tz7",
		 "stash_message":"On main: payment retry WIP","created_at":"2026-10-13T10:30:00Z","expires_at":"2026-10-13T16:30:00Z","delete_token":"token-1"},
		{"share_id":"BBBBBBBBBBBB","server":"https://stashvent.example","link":"https://stashvent.example/BBBBBBBBBBBB",
		 "stash_message":"gone","created_at":"2026-10-13T04:00:00Z","expires_at":"2026-10-13T10:50:00Z","delete_token":"token-2"},
		{"share_id":"Qw3rTy7uIo9p","server":"https://stashvent.example","link":"https://stashvent.example/Qw3rTy7uIo9p",
		 "stash_message":"On feature/login: \u001b[2Jsecond idea","created_at":"2026-10-13T10:45:00Z","expires_at":"2026-10-20T10:45:00Z","delete_token":"token-3"},
		{"share_id":"Zx5cVb8nMa2s","server":"http://localhost:8080","link":"http://localhost:8080/Zx5cVb8nMa2s",
		 "stash_message":"WIP on main: 1a2b3c4 fix typo","created_at":"2026-10-13T09:55:00Z","expires_at":"2026-10-13T10:55:30Z","delete_token":"token-4"}]}`
	if err := os.WriteFile(filepath.Join(c.configDir, "history.json"), []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	res = c.run("list")
	if res.code != exitOK || res.stderr != "" {
		t.Fatalf("list: exit %d, stderr %q", res.code, res.stderr)
	}
	golden(t, "list.stdout.golden", res.stdout, nil)
	// Newest first; the share at its expiry is not listed; the escape
	// character in a message doesn't reach the terminal; no delete token.
	for _, bad := range []string{"BBBBBBBBBBBB", "\x1b", "token-"} {
		if strings.Contains(res.stdout, bad) {
			t.Errorf("list shows %q:\n%s", bad, res.stdout)
		}
	}
	if h := c.historyFile(); strings.Contains(h, "BBBBBBBBBBBB") {
		t.Errorf("the expired share is still in the file:\n%s", h)
	}
}

func TestExpiresIn(t *testing.T) {
	tests := map[time.Duration]string{
		7 * 24 * time.Hour:              "7d 0h",
		6*24*time.Hour + 23*time.Hour:   "6d 23h",
		5*time.Hour + 40*time.Minute:    "5h 40m",
		time.Hour:                       "1h 0m",
		12*time.Minute + 59*time.Second: "12m",
		30 * time.Second:                "<1m",
	}
	for d, want := range tests {
		if got := expiresIn(d); got != want {
			t.Errorf("expiresIn(%v) = %q, want %q", d, got, want)
		}
	}
}

// A link names its own server, so a broken server setting must not get in
// the way; a bare ID does need it.
func TestPullUsesTheLinksServer(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "wip")
	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}

	receiver := newCLI(t, recipient, nil)
	receiver.env[serverEnv] = "nonsense"
	receiver.stdin = testPassword + "\n"
	if res := receiver.run("pull", "--password-stdin", fakeIDs[0]); res.code != exitUsage || !strings.Contains(res.stderr, "(from STASHVENT_SERVER)") {
		t.Errorf("bare ID with a broken server setting: exit %d, stderr %q", res.code, res.stderr)
	}
	if res := receiver.run("pull", "--password-stdin", f.URL+"/"+fakeIDs[0]); res.code != exitOK {
		t.Errorf("link with a broken server setting: exit %d\n%s", res.code, res.stderr)
	}
	// Pulling the same share again finds it already there.
	receiver.env[serverEnv] = f.URL
	if res := receiver.run("pull", "--password-stdin", fakeIDs[0]); res.code != exitOK || !strings.Contains(res.stderr, "Already in your stash list") {
		t.Errorf("bare ID: exit %d\n%s", res.code, res.stderr)
	}
}

// A share with a format field this version doesn't know (a newer stashvent's
// packaging, bundle version, or object format) is refused before the
// download, with "upgrade", instead of being misread.
func TestPullRefusesNewerFormats(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "wip")
	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}

	for name, change := range map[string]func(*api.CreateShareRequest){
		"payload format":   func(r *api.CreateShareRequest) { r.PayloadFormat = "stashvent-v2" },
		"packaging method": func(r *api.CreateShareRequest) { r.PackagingMethod = "stash-export" },
		"bundle version":   func(r *api.CreateShareRequest) { r.BundleVersion = 4 },
		"object format":    func(r *api.CreateShareRequest) { r.ObjectFormat = "sha512" },
	} {
		f.mu.Lock()
		share := f.shares[fakeIDs[0]]
		saved := share.req
		change(&share.req)
		f.mu.Unlock()

		receiver := newCLI(t, recipient, nil)
		receiver.stdin = testPassword + "\n"
		res := receiver.run("pull", "--password-stdin", f.URL+"/"+fakeIDs[0])
		if res.code != exitError || !strings.HasSuffix(res.stderr, "✗ This share uses a newer format. Upgrade stashvent and try again.\n") {
			t.Errorf("%s: exit %d, stderr:\n%s", name, res.code, res.stderr)
		}

		f.mu.Lock()
		share.req = saved
		f.mu.Unlock()
	}
	if n := f.called("download"); n != 0 {
		t.Errorf("%d downloads, want none", n)
	}
}

// The download is held in memory and its size comes from whichever server the
// link names, so a size over the limit, or one too small to hold anything, is
// refused before downloading.
func TestPullRefusesBadSizes(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "wip")
	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}
	link := f.URL + "/" + fakeIDs[0]

	receiver := newCLI(t, recipient, nil)
	receiver.maxPayload = 100 // bytes; the pushed share is larger
	receiver.stdin = testPassword + "\n"
	res := receiver.run("pull", "--password-stdin", link)
	want := ", more than this version of stashvent downloads (100 bytes). Upgrade stashvent and try again.\n"
	if res.code != exitError || !strings.HasSuffix(res.stderr, want) {
		t.Errorf("over the limit: exit %d, stderr:\n%s", res.code, res.stderr)
	}

	for _, size := range []int64{0, -1, 16} {
		f.mu.Lock()
		share := f.shares[fakeIDs[0]]
		saved := share.req.Size
		share.req.Size = size
		f.mu.Unlock()

		receiver := newCLI(t, recipient, nil)
		receiver.stdin = testPassword + "\n"
		res := receiver.run("pull", "--password-stdin", link)
		want := fmt.Sprintf("✗ The server reported an invalid size for this share (%d bytes). Ask the sender to push it again.\n", size)
		if res.code != exitError || !strings.HasSuffix(res.stderr, want) {
			t.Errorf("size %d: exit %d, stderr:\n%s", size, res.code, res.stderr)
		}

		f.mu.Lock()
		share.req.Size = saved
		f.mu.Unlock()
	}
	if n := f.called("download"); n != 0 {
		t.Errorf("%d downloads, want none", n)
	}
}

// The bounds are exclusive on the refusing side: a share of exactly the limit
// is pulled, and one byte more than the GCM tag is downloaded (and here fails
// as a short download, not as an invalid size).
func TestPullSizeBounds(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "wip")
	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}
	link := f.URL + "/" + fakeIDs[0]
	f.mu.Lock()
	share := f.shares[fakeIDs[0]]
	size := share.req.Size
	f.mu.Unlock()

	atLimit := newCLI(t, recipient, nil)
	atLimit.maxPayload = size
	atLimit.stdin = testPassword + "\n"
	if res := atLimit.run("pull", "--password-stdin", link); res.code != exitOK {
		t.Errorf("a share of exactly the limit: exit %d, stderr:\n%s", res.code, res.stderr)
	}

	f.mu.Lock()
	share.req.Size = crypto.TagSize + 1
	f.mu.Unlock()
	downloads := f.called("download")
	justOver := newCLI(t, recipient, nil)
	justOver.stdin = testPassword + "\n"
	res := justOver.run("pull", "--password-stdin", link)
	if res.code != exitError || strings.Contains(res.stderr, "invalid size") || f.called("download") != downloads+1 {
		t.Errorf("size %d: exit %d, %d downloads, stderr:\n%s", crypto.TagSize+1, res.code, f.called("download")-downloads, res.stderr)
	}
}

// The format fields are bound to the ciphertext, so a server that swaps one
// known value for another makes decryption fail.
func TestPullDetectsSwappedFormats(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "wip")
	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}

	for name, change := range map[string]func(*api.CreateShareRequest){
		"bundle version": func(r *api.CreateShareRequest) { r.BundleVersion = 5 - r.BundleVersion }, // 2 ↔ 3
		"object format": func(r *api.CreateShareRequest) {
			r.ObjectFormat = map[string]string{"sha1": "sha256", "sha256": "sha1"}[r.ObjectFormat]
		},
	} {
		f.mu.Lock()
		share := f.shares[fakeIDs[0]]
		saved := share.req
		change(&share.req)
		f.mu.Unlock()

		receiver := newCLI(t, recipient, nil)
		receiver.stdin = testPassword + "\n"
		res := receiver.run("pull", "--password-stdin", f.URL+"/"+fakeIDs[0])
		if res.code != exitError || !strings.Contains(res.stderr, "Decryption failed") {
			t.Errorf("%s: exit %d, stderr:\n%s", name, res.code, res.stderr)
		}

		f.mu.Lock()
		share.req = saved
		f.mu.Unlock()
	}
}

func TestPullWrongPassword(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	repo := newRepo(t)
	recipient := cloneRepo(t, repo)
	addStash(t, repo, "wip")
	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}
	link := f.URL + "/" + fakeIDs[0]

	// At a terminal: two wrong tries, then the right one, with one download.
	receiver := newCLI(t, recipient, nil)
	receiver.terminal = true
	receiver.secrets = []string{"wrong-password-1", "wrong-password-2", testPassword}
	res := receiver.run("pull", link)
	if res.code != exitOK || strings.Count(res.stderr, "✗ Wrong password. Try again") != 2 || f.called("download") != 1 {
		t.Errorf("exit %d, %d downloads\n%s", res.code, f.called("download"), res.stderr)
	}
	if out := res.stdout + res.stderr; strings.Contains(out, testPassword) || strings.Contains(out, "wrong-password") {
		t.Errorf("a typed password is on screen:\n%s", out)
	}

	// A share that isn't there.
	receiver.secrets = []string{testPassword}
	if res := receiver.run("pull", f.URL+"/AAAAAAAAAAAA"); res.code != exitError || !strings.HasSuffix(res.stderr, "✗ Share not found or expired.\n") {
		t.Errorf("unknown share: exit %d, stderr %q", res.code, res.stderr)
	}
}
