package userfiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	configName    = "config.json"
	configVersion = 1
)

// How an API key got into config.json.
const (
	SourcePasted = "pasted" // stashvent login, from the prompt or stdin
	// A browser login, if one is added, will add its own value.
)

// ServerEntry is what config.json holds for one server. The layout is frozen
// at v0.1 like history.json's: fields may be added, never renamed or removed.
type ServerEntry struct {
	APIKey  string    `json:"api_key"`
	Source  string    `json:"source"` // how the key was obtained: SourcePasted
	SavedAt time.Time `json:"saved_at"`

	extra map[string]json.RawMessage // fields this version doesn't know
}

// entryFields is ServerEntry without its JSON methods.
type entryFields ServerEntry

// UnmarshalJSON keeps the fields it doesn't know.
func (e *ServerEntry) UnmarshalJSON(data []byte) error {
	var f entryFields
	extra, err := decodeObject(data, &f)
	if err != nil {
		return err
	}
	*e = ServerEntry(f)
	e.extra = extra
	return nil
}

// MarshalJSON writes the unknown fields back after the known ones.
func (e ServerEntry) MarshalJSON() ([]byte, error) {
	return encodeObject(entryFields(e), e.extra)
}

// configDoc is config.json. It holds API keys only, one per server, keyed by
// the server in apiclient.NormalizeServer's form: a key is sent to the
// server it is stored under and nowhere else. There is no default server.
type configDoc struct {
	Version int                    `json:"version"`
	Servers map[string]ServerEntry `json:"servers"`
}

// Config is the config.json in one directory.
type Config struct {
	dir string
}

// NewConfig returns the config kept in dir (see Dir). Nothing is read or
// created until a method is called.
func NewConfig(dir string) *Config {
	return &Config{dir: dir}
}

// Path returns the file's location, for messages.
func (c *Config) Path() string {
	return filepath.Join(c.dir, configName)
}

// APIKey returns the key stored for server, and whether there is one. A
// missing file has none; a read never creates the file or the directory.
func (c *Config) APIKey(server string) (string, bool, error) {
	if _, err := os.Stat(c.Path()); errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	var entry ServerEntry
	var ok bool
	err := c.edit(func(doc *configDoc) bool {
		entry, ok = doc.Servers[server]
		return false
	})
	if err != nil || !ok || entry.APIKey == "" {
		return "", false, err
	}
	return entry.APIKey, true, nil
}

// SetAPIKey stores key for server, replacing the one there was. Fields of
// the entry this version doesn't know are dropped with the old key: they
// described it.
func (c *Config) SetAPIKey(now time.Time, server, key, source string) error {
	return c.edit(func(doc *configDoc) bool {
		doc.Servers[server] = ServerEntry{APIKey: key, Source: source, SavedAt: now.UTC()}
		return true
	})
}

// RemoveAPIKey drops the key stored for server and reports whether there was
// one.
func (c *Config) RemoveAPIKey(server string) (bool, error) {
	if _, err := os.Stat(c.Path()); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	var removed bool
	err := c.edit(func(doc *configDoc) bool {
		_, removed = doc.Servers[server]
		delete(doc.Servers, server)
		return removed
	})
	return removed, err
}

// edit reads the file under its lock, applies change, and writes the file
// back if change reports that it changed something.
func (c *Config) edit(change func(*configDoc) bool) error {
	if err := os.MkdirAll(c.dir, dirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", c.dir, err)
	}
	unlock, err := lock(filepath.Join(c.dir, configName+".lock"))
	if err != nil {
		return err
	}
	defer unlock()

	doc, extra, err := c.read()
	if err != nil {
		return err
	}
	if !change(&doc) {
		return nil
	}
	return c.write(doc, extra)
}

func (c *Config) read() (configDoc, map[string]json.RawMessage, error) {
	empty := configDoc{Version: configVersion, Servers: map[string]ServerEntry{}}
	data, err := os.ReadFile(c.Path()) //nolint:gosec // G304: a fixed name in the user's config directory
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil, nil
	}
	if err != nil {
		return configDoc{}, nil, fmt.Errorf("reading %s: %w", c.Path(), err)
	}
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))) // a byte order mark from an editor
	if len(data) == 0 {
		return empty, nil, nil
	}

	var doc configDoc
	extra, err := decodeObject(data, &doc)
	switch {
	case err != nil:
		return configDoc{}, nil, fmt.Errorf("%s: %w: %v", c.Path(), ErrCorrupt, err) //nolint:errorlint // only ErrCorrupt is meant to be matched
	case doc.Version > configVersion:
		return configDoc{}, nil, fmt.Errorf("%s: %w", c.Path(), ErrNewerVersion)
	case doc.Version != configVersion:
		return configDoc{}, nil, fmt.Errorf("%s: %w: version %d", c.Path(), ErrCorrupt, doc.Version)
	}
	if doc.Servers == nil {
		doc.Servers = map[string]ServerEntry{}
	}
	return doc, extra, nil
}

func (c *Config) write(doc configDoc, extra map[string]json.RawMessage) error {
	compact, err := encodeObject(doc, extra)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", c.Path(), err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return fmt.Errorf("encoding %s: %w", c.Path(), err)
	}
	out.WriteByte('\n')
	return writeFile(c.Path(), out.Bytes())
}
