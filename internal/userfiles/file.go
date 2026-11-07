// Package userfiles reads and writes the files the CLI keeps for one user in
// os.UserConfigDir()/stashvent: history.json, the shares sent from this
// machine, and config.json, the user's API key for each server. It never
// stores a password.
//
// Governed by the stashvent-cli skill ("Config and history files").
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

var (
	// ErrCorrupt means a file exists but can't be read as what it should be.
	// It is never overwritten: the user fixes or removes it.
	ErrCorrupt = errors.New("file is corrupt")
	// ErrNewerVersion means a newer stashvent wrote the file.
	ErrNewerVersion = errors.New("file was written by a newer stashvent")
	// ErrLocked means another stashvent held the lock for the whole wait.
	ErrLocked = errors.New("file is in use by another stashvent")
)

// Dir returns the directory for this user's files: %AppData%\stashvent on
// Windows, ~/Library/Application Support/stashvent on macOS, and
// $XDG_CONFIG_HOME/stashvent or ~/.config/stashvent on Linux.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the user config directory: %w", err)
	}
	return filepath.Join(base, "stashvent"), nil
}

// Permissions on Unix. On Windows they only set the read-only bit, and the
// files rely on the access list inherited from %AppData%, which by default
// admits the user, SYSTEM, and Administrators.
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// How long to wait for the lock, and when one is taken to be left over from
// a stashvent that died. A holder keeps it for one read and one write.
const (
	lockWait  = 2 * time.Second
	lockStale = 10 * time.Second
	lockRetry = 20 * time.Millisecond
)

// lock takes the lock file at path and returns the function that releases
// it. Every read and write of a file happens under its lock: two pushes at
// once would otherwise lose one entry, and on Windows a rename fails while
// another process has the target open.
func lock(path string) (unlock func(), err error) {
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm) //nolint:gosec // G304: a fixed name in our own directory
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		// Windows answers "access denied" for a lock that is being removed
		// this instant, so that is worth another try as well.
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			if errors.Is(err, fs.ErrExist) {
				return nil, fmt.Errorf("%s: %w", path, ErrLocked)
			}
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		time.Sleep(lockRetry)
	}
}

// writeFile replaces path with data in one step: it writes a temp file in
// the same directory, then renames it. A failure leaves the old file as it
// was. os.CreateTemp makes the file user-only (0600 on Unix).
func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // nothing to remove once the rename worked

	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// decodeObject reads a JSON object into v and returns the fields v doesn't
// have. They go back into the file when it is rewritten (encodeObject), so a
// field added by a newer stashvent survives an older one.
func decodeObject(data []byte, v any) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	known, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var knownFields map[string]json.RawMessage
	if err := json.Unmarshal(known, &knownFields); err != nil {
		return nil, err
	}
	for name := range knownFields {
		delete(fields, name)
	}
	return fields, nil
}

// encodeObject writes v as a JSON object, followed by the extra fields in
// name order.
func encodeObject(v any, extra map[string]json.RawMessage) ([]byte, error) {
	out, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return out, err
	}
	var buf bytes.Buffer
	buf.Write(bytes.TrimSuffix(out, []byte("}")))
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		if buf.Len() > 1 { // something follows the opening brace
			buf.WriteByte(',')
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(extra[name])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
