// Package payload defines the stashvent-v1 payload: the plaintext that the
// CLI encrypts, holding the share manifest and the Git bundle.
//
// Layout (all of it inside AES-256-GCM):
//
//	"SVP1" (4 bytes) | manifest length (uint32 big-endian) | manifest JSON | bundle bytes
//
// The four format fields sent to the server in plain text are bound to the
// ciphertext as AES-GCM associated data (see AssociatedData), so changing any
// of them makes decryption fail.
//
// Governed by the stashvent-crypto skill.
package payload

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stashvent/stashvent/internal/api"
)

var magic = []byte("SVP1")

const (
	headerSize      = 8        // magic + uint32 manifest length
	MaxManifestSize = 64 << 10 // descriptive details only; far below this in practice
)

// ErrMalformed is returned by Decode for anything that isn't a valid payload.
var ErrMalformed = errors.New("malformed payload")

// Manifest holds the stash's descriptive details. It travels only inside the
// encrypted payload; the server never sees it. Fields may be added within
// stashvent-v1: readers ignore unknown fields and tolerate missing ones.
type Manifest struct {
	StashMessage string    `json:"stash_message"`
	BaseCommit   string    `json:"base_commit"`
	BundleRef    string    `json:"bundle_ref"`  // the refs/share/<random> ref inside the bundle
	SenderName   string    `json:"sender_name"` // git config user.name; may be empty
	CreatedAt    time.Time `json:"created_at"`
	RepoName     string    `json:"repo_name"` // "owner/repo" or folder name; display only
	RepoHash     string    `json:"repo_hash"` // SHA-256 of the root commits; empty in shares from before it existed
	Branch       string    `json:"branch"`    // branch at stash time; empty when detached
	FileCount    int       `json:"file_count"`
}

// Encode builds the payload plaintext from m and bundle.
func Encode(m Manifest, bundle []byte) ([]byte, error) {
	if len(bundle) == 0 {
		return nil, errors.New("encoding payload: empty bundle")
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding manifest: %w", err)
	}
	if len(manifest) > MaxManifestSize {
		return nil, fmt.Errorf("encoding payload: manifest is %d bytes, limit %d", len(manifest), MaxManifestSize)
	}

	out := make([]byte, 0, headerSize+len(manifest)+len(bundle))
	out = append(out, magic...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(manifest))) //nolint:gosec // G115: bounded by MaxManifestSize above
	out = append(out, manifest...)
	out = append(out, bundle...)
	return out, nil
}

// Decode splits a decrypted payload into its manifest and bundle. The data
// comes from another user, so every length is checked before use. The
// returned bundle shares data's backing array.
func Decode(data []byte) (Manifest, []byte, error) {
	if len(data) < headerSize || !bytes.Equal(data[:4], magic) {
		return Manifest{}, nil, fmt.Errorf("%w: missing header", ErrMalformed)
	}
	n := binary.BigEndian.Uint32(data[4:headerSize])
	if n > MaxManifestSize || int64(n) > int64(len(data)-headerSize) {
		return Manifest{}, nil, fmt.Errorf("%w: bad manifest length %d", ErrMalformed, n)
	}
	manifestEnd := headerSize + int(n)

	var m Manifest
	if err := json.Unmarshal(data[headerSize:manifestEnd], &m); err != nil {
		return Manifest{}, nil, fmt.Errorf("%w: manifest: %v", ErrMalformed, err) //nolint:errorlint // only ErrMalformed is meant to be matched
	}
	if !strings.HasPrefix(m.BundleRef, "refs/share/") {
		return Manifest{}, nil, fmt.Errorf("%w: unexpected bundle ref %q", ErrMalformed, m.BundleRef)
	}

	bundle := data[manifestEnd:]
	if len(bundle) == 0 {
		return Manifest{}, nil, fmt.Errorf("%w: empty bundle", ErrMalformed)
	}
	return m, bundle, nil
}

// KnownFormat reports whether this version can read a share with these four
// format fields. Pull checks it before downloading, so a share made by a
// newer stashvent (a new packaging method, bundle version, or object format)
// gets "upgrade stashvent" instead of failing halfway through the import.
// Adding a value here is how a new one becomes readable.
func KnownFormat(payloadFormat, packagingMethod string, bundleVersion int, objectFormat string) bool {
	return payloadFormat == api.PayloadFormatV1 &&
		packagingMethod == api.PackagingStashRef &&
		(bundleVersion == 2 || bundleVersion == 3) && // git writes v3 for sha256 repositories
		(objectFormat == "sha1" || objectFormat == "sha256")
}

// AssociatedData returns the bytes bound to the ciphertext as AES-GCM
// associated data: the four plain-text format fields the CLI relies on to
// read the payload. Push builds it from the values it sends to the server,
// pull from the values the server returns; any difference fails decryption.
func AssociatedData(payloadFormat, packagingMethod string, bundleVersion int, objectFormat string) ([]byte, error) {
	for _, f := range []string{payloadFormat, packagingMethod, objectFormat} {
		if strings.ContainsAny(f, "\n") {
			return nil, fmt.Errorf("format field %q contains a newline", f)
		}
	}
	return []byte("payload_format=" + payloadFormat +
		"\npackaging_method=" + packagingMethod +
		"\nbundle_version=" + strconv.Itoa(bundleVersion) +
		"\nobject_format=" + objectFormat), nil
}
