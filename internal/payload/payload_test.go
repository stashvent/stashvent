package payload

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func testManifest() Manifest {
	return Manifest{
		StashMessage: "On main: wip",
		BaseCommit:   "80ff2c19df9e75a14e4e407c72111e91418e49a4",
		BundleRef:    "refs/share/0123456789abcdef",
		SenderName:   "Test",
		CreatedAt:    time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
		RepoName:     "stashvent/stashvent",
		RepoHash:     "0b2d6ef5b1f6e0a8c3d6b7f6b2b8a1a4c0b1e4f2a6d3c9e8f7a6b5c4d3e2f1a0",
		Branch:       "main",
		FileCount:    3,
	}
}

// rawPayload builds a payload by hand, bypassing Encode's checks.
func rawPayload(manifest, bundle string) []byte {
	d := binary.BigEndian.AppendUint32([]byte("SVP1"), uint32(len(manifest))) //nolint:gosec // G115: test manifests are tiny
	return append(append(d, manifest...), bundle...)
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	bundle := []byte("# v2 git bundle\n...")
	data, err := Encode(testManifest(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("SVP1")) {
		t.Errorf("payload starts with %q, want SVP1", data[:4])
	}

	m, got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m != testManifest() {
		t.Errorf("manifest = %+v, want %+v", m, testManifest())
	}
	if !bytes.Equal(got, bundle) {
		t.Errorf("bundle = %q, want %q", got, bundle)
	}
}

func TestDecodeIgnoresUnknownManifestFields(t *testing.T) {
	data := rawPayload(`{"bundle_ref":"refs/share/x","field_from_a_newer_cli":"x"}`, "bundle")

	m, _, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m.BundleRef != "refs/share/x" {
		t.Errorf("BundleRef = %q", m.BundleRef)
	}
}

func TestDecodeRejects(t *testing.T) {
	valid, err := Encode(testManifest(), []byte("bundle"))
	if err != nil {
		t.Fatal(err)
	}
	withLength := func(n uint32) []byte {
		d := bytes.Clone(valid)
		binary.BigEndian.PutUint32(d[4:8], n)
		return d
	}
	rawManifest := func(manifest string) []byte { return rawPayload(manifest, "bundle") }

	tests := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short header", []byte("SVP1")},
		{"wrong magic", append([]byte("SVP2"), valid[4:]...)},
		{"manifest length beyond data", withLength(1 << 10)}, // valid is far shorter than 1 KB
		{"manifest length over limit", withLength(MaxManifestSize + 1)},
		{"empty bundle", valid[:len(valid)-len("bundle")]},
		{"invalid JSON", rawManifest(`{"bundle_ref":`)},
		{"ref outside refs/share", rawManifest(`{"bundle_ref":"refs/heads/main"}`)},
		{"missing ref", rawManifest(`{}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := Decode(tt.data); !errors.Is(err, ErrMalformed) {
				t.Errorf("Decode error = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestEncodeRejectsEmptyBundle(t *testing.T) {
	if _, err := Encode(testManifest(), nil); err == nil {
		t.Error("Encode accepted an empty bundle")
	}
}

func TestAssociatedData(t *testing.T) {
	base, err := AssociatedData("stashvent-v1", "stash-ref", 2, "sha1")
	if err != nil {
		t.Fatal(err)
	}
	want := "payload_format=stashvent-v1\npackaging_method=stash-ref\nbundle_version=2\nobject_format=sha1"
	if string(base) != want {
		t.Errorf("AssociatedData = %q, want %q", base, want)
	}

	// Changing any one field must change the bytes.
	variants := [][]any{
		{"stashvent-v2", "stash-ref", 2, "sha1"},
		{"stashvent-v1", "stash-export", 2, "sha1"},
		{"stashvent-v1", "stash-ref", 3, "sha1"},
		{"stashvent-v1", "stash-ref", 2, "sha256"},
	}
	for _, v := range variants {
		got, err := AssociatedData(v[0].(string), v[1].(string), v[2].(int), v[3].(string))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got, base) {
			t.Errorf("AssociatedData%v equals the base value", v)
		}
	}

	if _, err := AssociatedData("stashvent-v1\nbundle_version=3", "stash-ref", 2, "sha1"); err == nil {
		t.Error("AssociatedData accepted a field containing a newline")
	}
}

// Pull reads only the formats it knows; anything else means "upgrade".
func TestKnownFormat(t *testing.T) {
	tests := []struct {
		payloadFormat, packagingMethod string
		bundleVersion                  int
		objectFormat                   string
		want                           bool
	}{
		{"stashvent-v1", "stash-ref", 2, "sha1", true},
		{"stashvent-v1", "stash-ref", 3, "sha256", true},
		{"stashvent-v2", "stash-ref", 2, "sha1", false},
		{"stashvent-v1", "stash-export", 2, "sha1", false},
		{"stashvent-v1", "stash-ref", 4, "sha1", false},
		{"stashvent-v1", "stash-ref", 0, "sha1", false},
		{"stashvent-v1", "stash-ref", 2, "sha512", false},
		{"stashvent-v1", "stash-ref", 2, "", false},
	}
	for _, tt := range tests {
		if got := KnownFormat(tt.payloadFormat, tt.packagingMethod, tt.bundleVersion, tt.objectFormat); got != tt.want {
			t.Errorf("KnownFormat(%q, %q, %d, %q) = %v, want %v",
				tt.payloadFormat, tt.packagingMethod, tt.bundleVersion, tt.objectFormat, got, tt.want)
		}
	}
}
