package userfiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	historyName    = "history.json"
	historyVersion = 1
)

// Share is one share sent from this machine, as history.json holds it. The
// layout is frozen at v0.1: fields may be added, never renamed or removed.
type Share struct {
	ShareID string `json:"share_id"`
	// Server is where the share was created, in apiclient.NormalizeServer's
	// form. It is the only host DeleteToken is ever sent to.
	Server       string    `json:"server"`
	Link         string    `json:"link"`
	StashMessage string    `json:"stash_message"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	DeleteToken  string    `json:"delete_token"`

	extra map[string]json.RawMessage // fields this version doesn't know
}

// shareFields is Share without its JSON methods.
type shareFields Share

// UnmarshalJSON keeps the fields it doesn't know.
func (s *Share) UnmarshalJSON(data []byte) error {
	var f shareFields
	extra, err := decodeObject(data, &f)
	if err != nil {
		return err
	}
	*s = Share(f)
	s.extra = extra
	return nil
}

// MarshalJSON writes the unknown fields back after the known ones.
func (s Share) MarshalJSON() ([]byte, error) {
	return encodeObject(shareFields(s), s.extra)
}

// historyDoc is history.json: an object, so fields can be added next to the
// list of shares.
type historyDoc struct {
	Version int     `json:"version"`
	Shares  []Share `json:"shares"`
}

// History is the history.json in one directory.
type History struct {
	dir string
}

// NewHistory returns the history kept in dir (see Dir). Nothing is read or
// created until a method is called.
func NewHistory(dir string) *History {
	return &History{dir: dir}
}

// Path returns the file's location, for messages.
func (h *History) Path() string {
	return filepath.Join(h.dir, historyName)
}

// Load returns the shares that haven't expired at now, oldest first. A
// missing or empty file is an empty history. Expired entries are also
// removed from the file, so their delete tokens don't stay on disk.
func (h *History) Load(now time.Time) ([]Share, error) {
	// First run: nothing to read, and a read shouldn't create the directory.
	if _, err := os.Stat(h.Path()); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return h.edit(now, nil)
}

// Add records a share, replacing an entry with the same ID.
func (h *History) Add(now time.Time, share Share) error {
	_, err := h.edit(now, func(shares []Share) []Share {
		shares = slices.DeleteFunc(shares, func(s Share) bool { return s.ShareID == share.ShareID })
		return append(shares, share)
	})
	return err
}

// Remove drops the share with that ID. It is not an error if there is none.
func (h *History) Remove(now time.Time, shareID string) error {
	_, err := h.edit(now, func(shares []Share) []Share {
		return slices.DeleteFunc(shares, func(s Share) bool { return s.ShareID == shareID })
	})
	return err
}

// edit reads the file under its lock, drops the expired shares, applies
// change (nil for a plain read), and writes the file back if it differs.
func (h *History) edit(now time.Time, change func([]Share) []Share) ([]Share, error) {
	if err := os.MkdirAll(h.dir, dirPerm); err != nil {
		return nil, fmt.Errorf("creating %s: %w", h.dir, err)
	}
	unlock, err := lock(filepath.Join(h.dir, historyName+".lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()

	doc, extra, err := h.read()
	if err != nil {
		return nil, err
	}
	stored := len(doc.Shares)
	doc.Shares = slices.DeleteFunc(doc.Shares, func(s Share) bool { return !s.ExpiresAt.After(now) })
	if change == nil {
		if len(doc.Shares) != stored {
			// Tidying up after a read; if it fails, the next write does it.
			_ = h.write(doc, extra)
		}
		return doc.Shares, nil
	}
	doc.Shares = change(doc.Shares)
	if err := h.write(doc, extra); err != nil {
		return nil, err
	}
	return doc.Shares, nil
}

func (h *History) read() (historyDoc, map[string]json.RawMessage, error) {
	empty := historyDoc{Version: historyVersion}
	data, err := os.ReadFile(h.Path()) //nolint:gosec // G304: a fixed name in the user's config directory
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil, nil
	}
	if err != nil {
		return historyDoc{}, nil, fmt.Errorf("reading %s: %w", h.Path(), err)
	}
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))) // a byte order mark from an editor
	if len(data) == 0 {
		return empty, nil, nil
	}

	var doc historyDoc
	extra, err := decodeObject(data, &doc)
	switch {
	case err != nil:
		return historyDoc{}, nil, fmt.Errorf("%s: %w: %v", h.Path(), ErrCorrupt, err) //nolint:errorlint // only ErrCorrupt is meant to be matched
	case doc.Version > historyVersion:
		return historyDoc{}, nil, fmt.Errorf("%s: %w", h.Path(), ErrNewerVersion)
	case doc.Version != historyVersion:
		return historyDoc{}, nil, fmt.Errorf("%s: %w: version %d", h.Path(), ErrCorrupt, doc.Version)
	}
	return doc, extra, nil
}

func (h *History) write(doc historyDoc, extra map[string]json.RawMessage) error {
	if doc.Shares == nil {
		doc.Shares = []Share{} // "shares": [] rather than null
	}
	compact, err := encodeObject(doc, extra)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", h.Path(), err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return fmt.Errorf("encoding %s: %w", h.Path(), err)
	}
	out.WriteByte('\n')
	return writeFile(h.Path(), out.Bytes())
}
