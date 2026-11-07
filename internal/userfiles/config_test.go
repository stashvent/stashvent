package userfiles

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const (
	hosted     = "https://stash.example.com"
	selfHosted = "https://stash.company.example"
	// Not key-shaped: the store doesn't check the format, the CLI does.
	testKey  = "sv_test-key-one"
	otherKey = "sv_test-key-two"
)

func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, configName)) //nolint:gosec // G304: test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func wantKey(t *testing.T, c *Config, server, want string) {
	t.Helper()
	got, ok, err := c.APIKey(server)
	if err != nil || got != want || ok != (want != "") {
		t.Errorf("APIKey(%s) = %q, %v, %v; want %q", server, got, ok, err, want)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	// A directory that doesn't exist yet, as on a first run.
	dir := filepath.Join(t.TempDir(), "stashvent")
	c := NewConfig(dir)

	wantKey(t, c, hosted, "")
	if removed, err := c.RemoveAPIKey(hosted); removed || err != nil {
		t.Errorf("RemoveAPIKey on a first run = %v, %v; want false, nil", removed, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a read created %s", dir)
	}

	// Keys are per server.
	if err := c.SetAPIKey(testNow, hosted, testKey, SourcePasted); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAPIKey(testNow, selfHosted, otherKey, SourcePasted); err != nil {
		t.Fatal(err)
	}
	wantKey(t, c, hosted, testKey)
	wantKey(t, c, selfHosted, otherKey)
	wantKey(t, c, "https://elsewhere.example", "")

	// Logging in again replaces the key.
	if err := c.SetAPIKey(testNow, hosted, otherKey, SourcePasted); err != nil {
		t.Fatal(err)
	}
	wantKey(t, c, hosted, otherKey)

	removed, err := c.RemoveAPIKey(hosted)
	if !removed || err != nil {
		t.Fatalf("RemoveAPIKey = %v, %v; want true, nil", removed, err)
	}
	wantKey(t, c, hosted, "")
	wantKey(t, c, selfHosted, otherKey)
	if removed, err := c.RemoveAPIKey(hosted); removed || err != nil {
		t.Errorf("second RemoveAPIKey = %v, %v; want false, nil", removed, err)
	}

	// No temp or lock files are left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != configName {
		t.Errorf("directory holds %v, want only %s", entries, configName)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, configName))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("config.json permissions = %o, want 600", perm)
		}
	}
}

// The layout is frozen at v0.1, so this text is the contract.
func TestConfigLayout(t *testing.T) {
	dir := t.TempDir()
	if err := NewConfig(dir).SetAPIKey(testNow, hosted, testKey, SourcePasted); err != nil {
		t.Fatal(err)
	}
	const want = `{
  "version": 1,
  "servers": {
    "https://stash.example.com": {
      "api_key": "sv_test-key-one",
      "source": "pasted",
      "saved_at": "2026-10-13T10:00:00Z"
    }
  }
}
`
	if got := readConfig(t, dir); got != want {
		t.Errorf("config.json:\n%s\nwant:\n%s", got, want)
	}
}

// An older stashvent must not drop what a newer one wrote, at the top level,
// in another server's entry, or in the entry it doesn't touch.
func TestConfigKeepsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{
  "version": 1,
  "default_server": "https://stash.company.example",
  "servers": {
    "https://stash.company.example": {
      "api_key": "sv_test-key-two",
      "source": "browser",
      "saved_at": "2026-10-12T09:00:00Z",
      "label": "work laptop"
    }
  }
}`)
	c := NewConfig(dir)
	if err := c.SetAPIKey(testNow, hosted, testKey, SourcePasted); err != nil {
		t.Fatal(err)
	}
	const want = `{
  "version": 1,
  "servers": {
    "https://stash.company.example": {
      "api_key": "sv_test-key-two",
      "source": "browser",
      "saved_at": "2026-10-12T09:00:00Z",
      "label": "work laptop"
    },
    "https://stash.example.com": {
      "api_key": "sv_test-key-one",
      "source": "pasted",
      "saved_at": "2026-10-13T10:00:00Z"
    }
  },
  "default_server": "https://stash.company.example"
}
`
	if got := readConfig(t, dir); got != want {
		t.Errorf("config.json:\n%s\nwant:\n%s", got, want)
	}
}

func TestConfigRefusesBadFiles(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    error
	}{
		{"not JSON", "{", ErrCorrupt},
		{"servers not an object", `{"version":1,"servers":[]}`, ErrCorrupt},
		{"no version", `{"servers":{}}`, ErrCorrupt},
		{"newer version", `{"version":2,"servers":{}}`, ErrNewerVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, tt.content)
			c := NewConfig(dir)
			if _, _, err := c.APIKey(hosted); !errors.Is(err, tt.want) {
				t.Errorf("APIKey err = %v, want %v", err, tt.want)
			}
			if err := c.SetAPIKey(testNow, hosted, testKey, SourcePasted); !errors.Is(err, tt.want) {
				t.Errorf("SetAPIKey err = %v, want %v", err, tt.want)
			}
			if _, err := c.RemoveAPIKey(hosted); !errors.Is(err, tt.want) {
				t.Errorf("RemoveAPIKey err = %v, want %v", err, tt.want)
			}
			// It holds the user's keys: never overwritten.
			if got := readConfig(t, dir); got != tt.content {
				t.Errorf("the file was changed to %q", got)
			}
		})
	}
}

func TestConfigToleratesEmptyAndBOM(t *testing.T) {
	for name, content := range map[string]string{
		"empty":             "",
		"whitespace":        "\n  \n",
		"byte order mark":   "\xef\xbb\xbf{\"version\":1,\"servers\":{}}",
		"no servers object": `{"version":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, content)
			c := NewConfig(dir)
			wantKey(t, c, hosted, "")
			if err := c.SetAPIKey(testNow, hosted, testKey, SourcePasted); err != nil {
				t.Fatal(err)
			}
			wantKey(t, c, hosted, testKey)
		})
	}
}
