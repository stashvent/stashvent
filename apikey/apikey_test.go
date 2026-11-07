package apikey

import (
	"crypto/sha256"
	"regexp"
	"strings"
	"testing"
)

// knownKey's checksum (the last 6 characters) was computed with Python
// (zlib.crc32 and the same base62 alphabet), independently of this package.
// It is put together from parts so that no key-shaped text is in the file,
// for secret scanners (StashVent's own included).
const knownKey = Prefix + "abcdefghijklmnopqrstuvwxyz" + "ABCDEFGHIJKLMNOPQ" + "4FLuWK"

func TestNew(t *testing.T) {
	pattern := regexp.MustCompile(Pattern)
	seen := map[string]bool{}
	for range 100 {
		key, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != Length || !strings.HasPrefix(key, Prefix) {
			t.Fatalf("key %q: length %d, want %d with prefix %q", key, len(key), Length, Prefix)
		}
		if !Valid(key) {
			t.Fatalf("New returned a key Valid refuses: %q", key)
		}
		if got := pattern.FindString("API_KEY=" + key + "\n"); got != key {
			t.Fatalf("Pattern found %q in a line holding %q", got, key)
		}
		if seen[key] {
			t.Fatalf("New returned %q twice", key)
		}
		seen[key] = true
	}
}

func TestKnownKeyChecksum(t *testing.T) {
	if !Valid(knownKey) {
		t.Errorf("Valid(%q) = false; the checksum differs from an independent implementation", knownKey)
	}
}

func TestValid(t *testing.T) {
	random := knownKey[len(Prefix):]
	swapped := []byte(knownKey)
	swapped[10] = 'Z' // was 'h'
	tests := map[string]string{
		"empty":                    "",
		"no prefix":                random,
		"other prefix":             "gh_" + random,
		"upper-case prefix":        "SV_" + random,
		"one character changed":    string(swapped),
		"checksum changed":         knownKey[:Length-1] + "X",
		"too short":                knownKey[:Length-1],
		"too long":                 knownKey + "0",
		"base64url character":      Prefix + "-" + random[1:],
		"underscore in the random": Prefix + "_" + random[1:],
		"trailing newline":         knownKey[:Length-1] + "\n",
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			if Valid(s) {
				t.Errorf("Valid(%q) = true, want false", s)
			}
		})
	}
}

func TestHash(t *testing.T) {
	want := sha256.Sum256([]byte(knownKey))
	if got := Hash(knownKey); string(got) != string(want[:]) {
		t.Errorf("Hash = %x, want the SHA-256 of the whole key, %x", got, want)
	}
}
