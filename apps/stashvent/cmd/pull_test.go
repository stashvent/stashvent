package cmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stashvent/stashvent/internal/git"
	"github.com/stashvent/stashvent/internal/payload"
)

func TestStashMessage(t *testing.T) {
	tests := []struct {
		name   string
		sender string
		msg    string
		want   string
	}{
		{"sender and message", "Alice", "On main: wip", "Shared from Alice — On main: wip"},
		{"no sender", "", "On main: wip", "Shared via stashvent — On main: wip"},
		{"no message", "Alice", "", "Shared from Alice"},
		{"terminal escapes dropped", "Al\x1b]0;pwned\x07ice", "On main:\x1b[31m red", "Shared from Al]0;pwnedice — On main:[31m red"},
		{"C1 control dropped", "Bob\u009b", "wip", "Shared from Bob — wip"},
		{"bidi override dropped", "Alice" + string(rune(0x202E)) + "gnp.exe", "wip", "Shared from Alicegnp.exe — wip"},
		{"bidi isolates and zero-width chars dropped",
			"Al" + string(rune(0x2066)) + "i" + string(rune(0x2069)) + "ce" + string(rune(0x200B)),
			"w" + string(rune(0x200D)) + "ip" + string(rune(0xFEFF)),
			"Shared from Alice — wip"},
		{"newlines and tabs become one space", "Alice", "line one\n\n\tline two", "Shared from Alice — line one line two"},
		{"only control characters", "\x1b\x07", "\x00", "Shared via stashvent"},
		// A stash imported, then pushed on: only the newest sender is named.
		{"pushed on again", "Bob", "Shared from Alice — On main: wip", "Shared from Bob — On main: wip"},
		{"pushed on twice", "Carol", "Shared from Bob — Shared from Alice — On main: wip", "Shared from Carol — On main: wip"},
		{"pushed on from a sender without a name", "Bob", "Shared via stashvent — On main: wip", "Shared from Bob — On main: wip"},
		{"pushed on, no message", "Bob", "Shared from Alice", "Shared from Bob — Shared from Alice"},
		{"a name holding the separator", "Alice — Ops", "On main: wip", "Shared from Alice - Ops — On main: wip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stashMessage(payload.Manifest{SenderName: tt.sender, StashMessage: tt.msg})
			if got != tt.want {
				t.Errorf("stashMessage = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepoCheck(t *testing.T) {
	tests := []struct {
		name                    string
		manifestHash, localHash string
		baseKnown, shallow      bool
		want                    repoVerdict
	}{
		{"base present", "h1", "h1", true, false, repoOK},
		{"base present, hashes differ (extra roots)", "h1", "h2", true, false, repoOK},
		{"base present, shallow", "h1", "h2", true, true, repoOK},
		{"same repo, base missing", "h1", "h1", false, false, repoBaseMissing},
		{"different repo, base missing", "h1", "h2", false, false, repoForeign},
		{"local repo has no commits", "h1", "", false, false, repoForeign},
		{"shallow, base missing, hashes differ", "h1", "h2", false, true, repoBaseMissingShallow},
		{"no hash in manifest, base missing", "", "h2", false, false, repoBaseMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repoCheck(tt.manifestHash, tt.localHash, tt.baseKnown, tt.shallow); got != tt.want {
				t.Errorf("repoCheck = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		name     string
		terminal bool
		input    string
		want     bool
	}{
		{"y", true, "y\n", true},
		{"YES with spaces", true, "  YES \r\n", true},
		{"empty means no", true, "\n", false},
		{"other text means no", true, "sure\n", false},
		{"EOF means no", true, "", false},
		{"no terminal means no", false, "y\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder
			a := newApp(Env{Stdin: strings.NewReader(tt.input), Stderr: &stderr, IsTerminal: func() bool { return tt.terminal }})
			got, err := a.confirm(t.Context(), "Import anyway?")
			if err != nil || got != tt.want {
				t.Errorf("confirm = %v, %v; want %v", got, err, tt.want)
			}
			if !strings.Contains(stderr.String(), "Import anyway? [y/N]") {
				t.Errorf("prompt not shown: %q", stderr.String())
			}
		})
	}
}

// Two prompts in one run read the same stdin. The first must not take the
// second one's answer with it.
func TestPromptsShareStdin(t *testing.T) {
	a := newApp(Env{Stdin: strings.NewReader("n\ny\n"), Stderr: io.Discard, IsTerminal: func() bool { return true }})
	first, err1 := a.confirm(t.Context(), "First?")
	second, err2 := a.confirm(t.Context(), "Second?")
	if err1 != nil || err2 != nil || first || !second {
		t.Errorf("answers = %v, %v (%v, %v); want false, true", first, second, err1, err2)
	}
}

func TestConfirmReturnsOnCancel(t *testing.T) {
	// stdin that never delivers a line, like a user who hasn't pressed Enter.
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() }) // ends the reader goroutine
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Ctrl+C

	a := newApp(Env{Stdin: r, Stderr: io.Discard, IsTerminal: func() bool { return true }})
	got, err := a.confirm(ctx, "Import anyway?")
	if got || !errors.Is(err, context.Canceled) {
		t.Errorf("confirm = %v, %v; want false, context.Canceled", got, err)
	}
}

func TestShortSHA(t *testing.T) {
	tests := map[string]string{
		"40cfca85e04edd62150362bfd5f8aad144cd52a5": "40cfca8",
		"abc":             "abc",
		"\x1b[31mabcdef0": "[31mabc",
	}
	for in, want := range tests {
		if got := shortSHA(in); got != want {
			t.Errorf("shortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanTextCapsLength(t *testing.T) {
	got := cleanText(strings.Repeat("é", 300), 200)
	if n := len([]rune(got)); n != 201 || !strings.HasSuffix(got, "…") {
		t.Errorf("cleanText kept %d runes (%q…), want 200 plus an ellipsis", n, got[:10])
	}
}

// The branch of a stash that was imported before is still found, so the
// next recipient's "Based on … (branch)" note keeps it.
func TestBranchOfAnImportedStash(t *testing.T) {
	for msg, want := range map[string]string{
		"On main: wip":                       "main",
		"Shared from Alice — On feat/x: wip": "feat/x",
		"Shared from Bob — Shared from Alice — WIP on dev: 1a2b3c4 msg": "dev",
		"Shared from Alice": "",
	} {
		if got := git.StashBranch(withoutSharedPrefix(msg)); got != want {
			t.Errorf("branch of %q = %q, want %q", msg, got, want)
		}
	}
}

// A stash imported, then pushed on to a third repository: the third
// recipient sees the newest sender once and git's own message, and the
// branch survives the hop. The second sender's name holds the separator.
func TestStashPushedOn(t *testing.T) {
	f := newFakeServer(t)
	first := newRepo(t)
	second, third := cloneRepo(t, first), cloneRepo(t, first)
	gitRun(t, second, "config", "user.name", "Bob — Ops")
	addStash(t, first, "wip")

	pullInto := func(dir, link string) result {
		t.Helper()
		c := newCLI(t, dir, f)
		c.stdin = testPassword + "\n"
		res := c.run("pull", "--password-stdin", link)
		if res.code != exitOK {
			t.Fatalf("pull: exit %d\n%s", res.code, res.stderr)
		}
		return res
	}
	if push := newCLI(t, first, f).run("push"); push.code != exitOK {
		t.Fatalf("first push: exit %d\n%s", push.code, push.stderr)
	}
	pullInto(second, f.URL+"/"+fakeIDs[0])
	if push := newCLI(t, second, f).run("push"); push.code != exitOK {
		t.Fatalf("second push: exit %d\n%s", push.code, push.stderr)
	}

	// HEAD moved on, so pull names the stash's branch.
	writeFile(t, third, "b.txt", "local\n")
	gitRun(t, third, "add", "b.txt")
	gitRun(t, third, "commit", "-q", "-m", "local")
	res := pullInto(third, f.URL+"/"+fakeIDs[1])
	if got, want := gitRun(t, third, "stash", "list"), "stash@{0}: Shared from Bob - Ops — On main: wip"; got != want {
		t.Errorf("third recipient's stash list = %q, want %q", got, want)
	}
	if !strings.Contains(res.stderr, " (main); you're on ") {
		t.Errorf("the branch was lost on the way:\n%s", res.stderr)
	}
}
