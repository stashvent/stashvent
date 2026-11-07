//go:build e2e && !windows

// Windows can't send Ctrl+C to a child process from Go, so on Windows this is
// a manual check before each release (RELEASES.md).

package e2e

import (
	"bytes"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Ctrl+C while the CLI waits for a password on stdin: exit 130, "Cancelled",
// and nothing reached the server.
func TestCtrlC(t *testing.T) {
	key, id := issueKey(t)
	sender := newMachine(t)
	sender.login(t, key)
	repo := sender.newRepo(t)
	sender.stash(t, repo, "ctrl-c")
	_, link, _ := sender.push(t, repo)
	recipient := newMachine(t)
	clone := recipient.clone(t, repo)

	for name, run := range map[string]struct {
		m    *machine
		dir  string
		args []string
	}{
		"push": {sender, repo, []string{"push", "--password-stdin"}},
		"pull": {recipient, clone, []string{"pull", "--password-stdin", link}},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), cfg.sv, run.args...) //nolint:gosec // G204: the binary under test
			cmd.Dir, cmd.Env = run.dir, run.m.env
			stdin, err := cmd.StdinPipe() // left open: the CLI waits on it
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stdin.Close() }()
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Long enough for the process to install its signal handler,
			// which is the first thing main does.
			time.Sleep(2 * time.Second)
			if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
			if code := exitCode(t, cmd.Wait()); code != 130 || !strings.Contains(stderr.String(), "✗ Cancelled") {
				t.Errorf("exit %d, want 130 and \"✗ Cancelled\"\n%s", code, stderr.String())
			}
			run.m.noLeftovers(t, run.dir)
		})
	}

	// The cancelled push never created a share: the key made only the first.
	var shares int
	if err := cfg.pool.QueryRow(t.Context(), `SELECT coalesce(sum(shares), 0) FROM api_key_usage WHERE api_key_id = $1`, id).Scan(&shares); err != nil {
		t.Fatal(err)
	}
	if shares != 1 {
		t.Errorf("the key created %d shares, want 1", shares)
	}
}
