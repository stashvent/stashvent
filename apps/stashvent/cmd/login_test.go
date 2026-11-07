package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/apiclient"
)

func newAPIKey(t *testing.T) string {
	t.Helper()
	key, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// configFile returns config.json as text, "" if there is none.
func (c *cli) configFile() string {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.configDir, "config.json")) //nolint:gosec // G304: test's own temp dir
	if err != nil && !os.IsNotExist(err) {
		c.t.Fatal(err)
	}
	return string(data)
}

// login stores key for the CLI's current server, as a user would.
func (c *cli) login(key string) {
	c.t.Helper()
	stdin := c.stdin
	c.stdin = key + "\n"
	res := c.run("login", "--with-token")
	c.stdin = stdin
	if res.code != exitOK {
		c.t.Fatalf("login: exit %d, stderr %q", res.code, res.stderr)
	}
}

// wantNoSecret fails if a run printed the key anywhere.
func wantNoSecret(t *testing.T, res result, secret string) {
	t.Helper()
	if strings.Contains(res.stdout, secret) || strings.Contains(res.stderr, secret) {
		t.Errorf("the key was printed: stdout %q, stderr %q", res.stdout, res.stderr)
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()
	key, other := newAPIKey(t), newAPIKey(t)
	c := newCLI(t, t.TempDir(), nil)
	c.env[serverEnv] = "https://Stashvent.example"

	// At a hidden prompt, for the server in its normal form.
	c.terminal = true
	c.secrets = []string{key}
	res := c.run("login")
	if res.code != exitOK || res.stdout != "" ||
		res.stderr != "API key for https://stashvent.example: \n✓ Saved the API key for https://stashvent.example\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	wantNoSecret(t, res, key)

	// From stdin, for another server; spaces and the line ending are dropped.
	c.terminal = false
	c.stdin = "  " + other + "\r\n"
	if res := c.run("login", "--with-token", "--server", "https://other.example"); res.code != exitOK {
		t.Fatalf("--with-token: exit %d, stderr %q", res.code, res.stderr)
	}
	config := c.configFile()
	for _, want := range []string{
		`"https://stashvent.example": {` + "\n      \"api_key\": \"" + key + `"`,
		`"https://other.example": {` + "\n      \"api_key\": \"" + other + `"`,
		`"source": "pasted"`,
	} {
		if !strings.Contains(config, want) {
			t.Errorf("config.json lacks %q:\n%s", want, config)
		}
	}

	// With STASHVENT_API_KEY set, login says it wins.
	c.env[apiKeyEnv] = other
	c.stdin = key + "\n"
	res = c.run("login", "--with-token")
	delete(c.env, apiKeyEnv)
	if res.code != exitOK || !strings.Contains(res.stderr, "STASHVENT_API_KEY is set, so it is used instead") {
		t.Errorf("exit %d, stderr %q", res.code, res.stderr)
	}

	// Logout forgets one server's key only.
	res = c.run("logout")
	if res.code != exitOK || res.stderr != "✓ Removed the API key for https://stashvent.example\n" {
		t.Errorf("logout: exit %d, stderr %q", res.code, res.stderr)
	}
	res = c.run("logout")
	if res.code != exitOK || res.stderr != "No API key was saved for https://stashvent.example.\n" {
		t.Errorf("second logout: exit %d, stderr %q", res.code, res.stderr)
	}
	if config := c.configFile(); strings.Contains(config, key) || !strings.Contains(config, other) {
		t.Errorf("after logout:\n%s", config)
	}
}

func TestLoginRefuses(t *testing.T) {
	t.Parallel()
	key := newAPIKey(t)
	mistyped := key[:len(key)-1] + "0"
	if mistyped == key {
		mistyped = key[:len(key)-1] + "1"
	}
	tests := []struct {
		name     string
		args     []string
		stdin    string
		terminal bool
		secrets  []string
		code     int
		want     string
		secret   string // must not be printed
	}{
		{"no terminal and no --with-token", []string{"login"}, "", false, nil, exitUsage, "use --with-token", ""},
		{"the key as an argument", []string{"login", key}, "", true, []string{key}, exitUsage, "accepts 0 arg(s)", key},
		{"nothing on stdin", []string{"login", "--with-token"}, "", false, nil, exitError, "No API key on stdin", ""},
		{"another kind of token", []string{"login", "--with-token"}, "ghp_notastashventkey\n", false, nil, exitError, "isn't a StashVent API key", "ghp_notastashventkey"},
		{"a mistyped key", []string{"login", "--with-token"}, mistyped + "\n", false, nil, exitError, "check that it was copied whole", mistyped},
		{"a password typed at the prompt", []string{"login"}, "", true, []string{"hunter2-password"}, exitError, "isn't a StashVent API key", "hunter2-password"},
		{"a bad server", []string{"login", "--with-token", "--server", "http://plain.example"}, key + "\n", false, nil, exitUsage, "refusing insecure", key},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLI(t, t.TempDir(), nil)
			c.stdin, c.terminal, c.secrets = tt.stdin, tt.terminal, tt.secrets
			res := c.run(tt.args...)
			if res.code != tt.code || !strings.Contains(res.stderr, tt.want) {
				t.Errorf("exit %d, stderr %q; want %d and %q", res.code, res.stderr, tt.code, tt.want)
			}
			if tt.secret != "" {
				wantNoSecret(t, res, tt.secret)
			}
			if c.configFile() != "" {
				t.Errorf("config.json was written:\n%s", c.configFile())
			}
		})
	}
}

func TestPushSendsTheAPIKey(t *testing.T) {
	t.Parallel()
	key, envKey := newAPIKey(t), newAPIKey(t)
	f := newFakeServer(t)
	f.requireKeys(key, envKey)
	repo := newRepo(t)
	c := newCLI(t, repo, f)
	addStash(t, repo, "wip")

	// Without a key the server refuses, and the CLI says what to do.
	res := c.run("push")
	if res.code != exitError || !strings.Contains(res.stderr, `This server needs an API key to push. Run "stashvent login"`) {
		t.Fatalf("push without a key: exit %d, stderr %q", res.code, res.stderr)
	}

	c.login(key)
	res = c.run("push")
	if res.code != exitOK {
		t.Fatalf("push: exit %d, stderr %q", res.code, res.stderr)
	}
	wantNoSecret(t, res, key)
	if got := f.authorization("create"); got[len(got)-1] != "Bearer "+key {
		t.Errorf("create sent Authorization %q, want the stored key", got[len(got)-1])
	}
	// Only the create carries it: not the upload, and not /complete.
	for _, route := range []string{"upload", "complete"} {
		for _, auth := range f.authorization(route) {
			if auth != "" {
				t.Errorf("%s sent Authorization %q", route, auth)
			}
		}
	}

	// STASHVENT_API_KEY wins, and push says it is used, never what it is.
	c.env[apiKeyEnv] = envKey
	res = c.run("push")
	if res.code != exitOK || !strings.Contains(res.stderr, "API key: from STASHVENT_API_KEY\n") {
		t.Fatalf("push with the env key: exit %d, stderr %q", res.code, res.stderr)
	}
	wantNoSecret(t, res, envKey)
	if got := f.authorization("create"); got[len(got)-1] != "Bearer "+envKey {
		t.Errorf("create sent Authorization %q, want the env key", got[len(got)-1])
	}

	// A malformed one stops the push before anything is sent.
	c.env[apiKeyEnv] = "not-a-key-but-maybe-a-secret"
	before := f.requests()
	res = c.run("push")
	if res.code != exitError || !strings.Contains(res.stderr, "STASHVENT_API_KEY doesn't hold a StashVent API key") {
		t.Errorf("malformed env key: exit %d, stderr %q", res.code, res.stderr)
	}
	wantNoSecret(t, res, "not-a-key-but-maybe-a-secret")
	if f.requests() != before {
		t.Error("a request was sent")
	}
}

// A key is only ever sent on push and delete: pull and list never send it,
// also not to the very server it is stored for.
func TestPullAndListNeverSendTheAPIKey(t *testing.T) {
	t.Parallel()
	key := newAPIKey(t)
	f := newFakeServer(t)
	repo := newRepo(t)
	addStash(t, repo, "wip")
	if res := newCLI(t, repo, f).run("push"); res.code != exitOK {
		t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
	}

	receiver := newCLI(t, cloneRepo(t, repo), f)
	receiver.login(key)
	receiver.env[apiKeyEnv] = key
	receiver.stdin = testPassword + "\n"
	if res := receiver.run("pull", "--password-stdin", f.URL+"/"+fakeIDs[0]); res.code != exitOK {
		t.Fatalf("pull: exit %d\n%s", res.code, res.stderr)
	}
	for _, route := range []string{"get", "download"} {
		for _, auth := range f.authorization(route) {
			if auth != "" {
				t.Errorf("%s sent Authorization %q", route, auth)
			}
		}
	}
	before := f.requests()
	if res := receiver.run("list"); res.code != exitOK || f.requests() != before {
		t.Errorf("list: exit %d, %d requests", res.code, f.requests()-before)
	}
}

func TestDeleteWithTheAPIKey(t *testing.T) {
	t.Parallel()
	key, otherKey := newAPIKey(t), newAPIKey(t)
	f := newFakeServer(t)
	f.requireKeys(key, otherKey)
	repo := newRepo(t)
	pusher := newCLI(t, repo, f)
	pusher.login(key)
	push := func() string {
		t.Helper()
		addStash(t, repo, "wip")
		before := f.called("create")
		if res := pusher.run("push"); res.code != exitOK {
			t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
		}
		return fakeID(before)
	}
	lastDelete := func() string {
		t.Helper()
		auths := f.authorization("delete")
		return auths[len(auths)-1]
	}

	// A machine whose history doesn't have the share, but has the key.
	elsewhere := newCLI(t, t.TempDir(), f)
	elsewhere.login(key)

	t.Run("in this machine's history: the token, not the key", func(t *testing.T) {
		id := push()
		tokens := len(f.tokens)
		if res := pusher.run("delete", id); res.code != exitOK {
			t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
		}
		if got := lastDelete(); got != "" {
			t.Errorf("the key was sent along with the token: %q", got)
		}
		if len(f.tokens) != tokens+1 {
			t.Error("the delete token wasn't sent")
		}
	})

	t.Run("from another machine, by ID", func(t *testing.T) {
		id := push()
		tokens := len(f.tokens)
		res := elsewhere.run("delete", id)
		if res.code != exitOK || res.stderr != "Server: "+f.URL+" (from STASHVENT_SERVER)\n✓ Deleted "+id+"\n" {
			t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
		}
		if f.share(id) != nil || lastDelete() != "Bearer "+key || len(f.tokens) != tokens {
			t.Errorf("share left %v, Authorization %q, tokens sent %d", f.share(id) != nil, lastDelete(), len(f.tokens)-tokens)
		}
		wantNoSecret(t, res, key)

		// Deleted already: nothing to do, but the ID was typed, so it may be wrong.
		res = elsewhere.run("delete", id)
		want := id + " isn't on the server: it expired, was already deleted, or the ID is mistyped.\n"
		if res.code != exitOK || !strings.HasSuffix(res.stderr, want) || strings.Contains(res.stderr, "✓") {
			t.Errorf("second delete: exit %d, stderr %q", res.code, res.stderr)
		}
	})

	t.Run("from another machine, by a link to the chosen server", func(t *testing.T) {
		id := push()
		if res := elsewhere.run("delete", f.URL+"/"+id); res.code != exitOK || f.share(id) != nil {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
	})

	t.Run("a key that didn't create the share", func(t *testing.T) {
		id := push()
		other := newCLI(t, t.TempDir(), f)
		other.login(otherKey)
		res := other.run("delete", id)
		if res.code != exitError || !strings.Contains(res.stderr, "Your API key didn't create this share") {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
		if f.share(id) == nil {
			t.Error("the share was deleted")
		}
	})

	// A link names a host anyone could have written: the key never goes
	// there, with or without one stored.
	t.Run("a link to another host", func(t *testing.T) {
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the link's host got a request: %s %s, Authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}))
		t.Cleanup(trap.Close)
		id := push()
		withoutKey := newCLI(t, t.TempDir(), f)
		for name, c := range map[string]*cli{"with a key": elsewhere, "without a key": withoutKey} {
			before := f.requests()
			res := c.run("delete", trap.URL+"/"+id)
			if res.code != exitError || !strings.Contains(res.stderr, "the link points to "+trap.URL+", not to "+f.URL) ||
				!strings.Contains(res.stderr, "stashvent delete --server "+trap.URL+" "+id) {
				t.Errorf("%s: exit %d, stderr %q", name, res.code, res.stderr)
			}
			if f.requests() != before {
				t.Errorf("%s: a request went to the chosen server", name)
			}
		}
		if f.share(id) == nil {
			t.Error("the share was deleted")
		}
	})

	t.Run("a server without keys", func(t *testing.T) {
		keyless := newFakeServer(t) // REQUIRE_API_KEY off: the key is ignored
		repo := newRepo(t)
		addStash(t, repo, "wip")
		if res := newCLI(t, repo, keyless).run("push"); res.code != exitOK {
			t.Fatalf("push: exit %d\n%s", res.code, res.stderr)
		}
		c := newCLI(t, t.TempDir(), keyless)
		c.login(key)
		res := c.run("delete", fakeIDs[0])
		if res.code != exitError || !strings.Contains(res.stderr, "doesn't use API keys,\nso the share can only be deleted from the machine that pushed it.") {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
	})

	t.Run("no key for the chosen server", func(t *testing.T) {
		id := push()
		c := newCLI(t, t.TempDir(), f)
		c.login(key)
		c.env[serverEnv] = "https://other.example" // the key is stored for f, not here
		before := f.requests()
		res := c.run("delete", id)
		if res.code != exitError || !strings.Contains(res.stderr, id+" isn't in this machine's history") ||
			!strings.Contains(res.stderr, `run "stashvent login" here`) {
			t.Errorf("exit %d, stderr %q", res.code, res.stderr)
		}
		if f.requests() != before {
			t.Error("a request was sent")
		}
	})
}

func TestAPIKeyErrorMessages(t *testing.T) {
	a := newApp(Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
	tests := []struct {
		code       string
		retryAfter time.Duration
		want       string
	}{
		{api.CodeMissingAPIKey, 0, `This server needs an API key to push. Run "stashvent login" with the key its operator gave you.`},
		{api.CodeInvalidAPIKey, 0, "The server didn't accept your API key: it was revoked or mistyped.\n" +
			`Ask the server's operator for a new one, then run "stashvent login".`},
		{api.CodeQuotaExceeded, 5*time.Hour + 11*time.Minute + 30*time.Second, "Your API key has used its daily share limit on this server. Try again in 5h 12m."},
		{api.CodeQuotaExceeded, 0, "Your API key has used its daily share limit on this server. Try again tomorrow (UTC)."},
		{api.CodeStorageFull, 0, "The server is out of storage space. Try again later."},
		{api.CodeShareNotOwned, 0, "Your API key didn't create this share, so it can't delete it."},
		{api.CodePullLimitReached, 0, "This share has been pulled the maximum number of times. Ask the sender to push it again."},
	}
	for _, tt := range tests {
		got := a.friendly(&apiclient.Error{Status: 400, Code: tt.code, RetryAfter: tt.retryAfter})
		if got != tt.want {
			t.Errorf("%s:\n got %q\nwant %q", tt.code, got, tt.want)
		}
	}
}

// The quota's Retry-After reaches the message end to end.
func TestPushQuotaExceeded(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.failWithCode("create", fakeFailure{status: http.StatusTooManyRequests, code: api.CodeQuotaExceeded, retryAfter: "2400"})
	repo := newRepo(t)
	addStash(t, repo, "wip")
	res := newCLI(t, repo, f).run("push")
	if res.code != exitError || !strings.HasSuffix(res.stderr, "✗ Your API key has used its daily share limit on this server. Try again in 40m.\n") {
		t.Errorf("exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestPullLimitReached(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.failWithCode("get", fakeFailure{status: http.StatusGone, code: api.CodePullLimitReached})
	c := newCLI(t, newRepo(t), f)
	c.stdin = testPassword + "\n"
	res := c.run("pull", "--password-stdin", f.URL+"/"+fakeIDs[0])
	if res.code != exitError || !strings.Contains(res.stderr, "pulled the maximum number of times. Ask the sender to push it again.") {
		t.Errorf("exit %d, stderr %q", res.code, res.stderr)
	}
}

func TestFormatWait(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Second:                   "1m",
		59 * time.Second:              "1m",
		40 * time.Minute:              "40m",
		time.Hour:                     "1h",
		5*time.Hour + 11*time.Minute:  "5h 11m",
		23*time.Hour + 59*time.Minute: "23h 59m",
		23*time.Hour + 59*time.Minute + time.Second: "24h",
	} {
		if got := formatWait(d); got != want {
			t.Errorf("formatWait(%v) = %q, want %q", d, got, want)
		}
	}
}
