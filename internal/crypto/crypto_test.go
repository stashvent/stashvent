package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Expected values below are published vectors, independently recomputed with
// Python's hashlib (PBKDF2) and Node's crypto (AES-GCM) before being added.

func TestDeriveKeyKnownAnswers(t *testing.T) {
	tests := []struct {
		name       string
		password   string
		salt       []byte
		iterations int
		size       int
		want       string
	}{
		{"RFC 7914 §11 c=1", "passwd", []byte("salt"), 1, 64,
			"55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"},
		{"RFC 7914 §11 c=80000", "Password", []byte("NaCl"), 80000, 64,
			"4ddcd8f60b98be21830cee5ef22701f9641a4418d04c0414aeff08876b34ab56a1d425a1225833549adb841b51c9b3176a272bdebba1d078478f62b397f33c8d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deriveKey(tt.password, tt.salt, tt.iterations, tt.size)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != tt.want {
				t.Errorf("deriveKey = %x, want %s", got, tt.want)
			}
		})
	}

	// The production parameters (600k iterations, 32-byte key) through the
	// public function, so a change to either constant fails here.
	salt := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	got, err := DeriveKey("correct horse battery staple", salt)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ef177144eec9420cbc1093d2a8b344a92bc506d0d4ec9c028dd19f8324d8c1e6"; hex.EncodeToString(got) != want {
		t.Errorf("DeriveKey = %x, want %s", got, want)
	}
}

func TestAESGCMKnownAnswers(t *testing.T) {
	// Test cases 13–16 from McGrew & Viega, "The Galois/Counter Mode of
	// Operation (GCM)": AES-256 with a 96-bit IV; 16 includes associated data.
	const (
		k15 = "feffe9928665731c6d6a8f9467308308feffe9928665731c6d6a8f9467308308"
		v15 = "cafebabefacedbaddecaf888"
		p15 = "d9313225f88406e5a55909c5aff5269a86a7a9531534f7da2e4c303d8a318a721c3c0c95956809532fcf0e2449a6b525b16aedf5aa0de657ba637b391aafd255"
		c15 = "522dc1f099567d07f47f37a32a84427d643a8cdcbfe5c0c97598a2bd2555d1aa8cb08e48590dbb3da7b08b1056828838c5f61e6393ba7a0abcc9f662898015ad"
	)
	zeroKey, zeroIV := strings.Repeat("00", 32), strings.Repeat("00", 12)
	tests := []struct {
		name                      string
		key, iv, pt, aad, ct, tag string
	}{
		{"case 13", zeroKey, zeroIV, "", "", "", "530f8afbc74536b9a963b4f1c4cb738b"},
		{"case 14", zeroKey, zeroIV, strings.Repeat("00", 16), "", "cea7403d4d606b6e074ec5d3baf39d18", "d0d1c8a799996bf0265b98b5d48ab919"},
		{"case 15", k15, v15, p15, "", c15, "b094dac5d93471bdec1a502270e3cc6c"},
		{"case 16", k15, v15, p15[:120], "feedfacedeadbeeffeedfacedeadbeefabaddad2", c15[:120], "76fc6ece0f4e1768cddf8853bb2d551b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, iv, pt, aad := mustHex(t, tt.key), mustHex(t, tt.iv), mustHex(t, tt.pt), mustHex(t, tt.aad)
			want := tt.ct + tt.tag

			got, err := seal(key, iv, pt, aad)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != want {
				t.Errorf("seal = %x, want %s", got, want)
			}

			opened, err := Decrypt(key, iv, mustHex(t, want), aad)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(opened, pt) {
				t.Errorf("Decrypt = %x, want %x", opened, pt)
			}
		})
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := randomKey(t)
	plaintext := []byte("stash bundle bytes")
	aad := []byte("payload_format=stashvent-v1")

	ct, nonce, err := Encrypt(key, plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(nonce) != NonceSize {
		t.Errorf("nonce length = %d, want %d", len(nonce), NonceSize)
	}
	// The CLI works out the encrypted size from this before it encrypts.
	if len(ct) != len(plaintext)+TagSize {
		t.Errorf("ciphertext length = %d, want %d (plaintext + TagSize)", len(ct), len(plaintext)+TagSize)
	}
	if bytes.Contains(ct, plaintext) {
		t.Error("ciphertext contains the plaintext")
	}

	got, err := Decrypt(key, nonce, ct, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt = %q, want %q", got, plaintext)
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	key := randomKey(t)
	_, n1, err := Encrypt(key, []byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, n2, err := Encrypt(key, []byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(n1, n2) {
		t.Error("two encryptions reused the same nonce")
	}
}

func TestDecryptRejects(t *testing.T) {
	key := randomKey(t)
	aad := []byte("bundle_version=2")
	ct, nonce, err := Encrypt(key, []byte("secret source code"), aad)
	if err != nil {
		t.Fatal(err)
	}

	tampered := bytes.Clone(ct)
	tampered[0] ^= 0x01

	tests := []struct {
		name  string
		key   []byte
		nonce []byte
		ct    []byte
		aad   []byte
	}{
		{"wrong key", randomKey(t), nonce, ct, aad},
		{"tampered ciphertext", key, nonce, tampered, aad},
		{"short nonce", key, nonce[:8], ct, aad},
		{"different associated data", key, nonce, ct, []byte("bundle_version=3")},
		{"missing associated data", key, nonce, ct, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decrypt(tt.key, tt.nonce, tt.ct, tt.aad)
			if err == nil {
				t.Fatal("Decrypt succeeded, want error")
			}
			if got != nil {
				t.Error("Decrypt returned partial plaintext on failure")
			}
		})
	}
}

func TestGeneratePassword(t *testing.T) {
	pw, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != PasswordLength {
		t.Errorf("length = %d, want %d", len(pw), PasswordLength)
	}
	for _, c := range pw {
		if !strings.ContainsRune(passwordAlphabet, c) {
			t.Errorf("password contains %q, outside the alphabet", c)
		}
	}
}

func TestShareID(t *testing.T) {
	// About 1 in 64 raw draws starts with '-'; 1000 IDs make hitting the
	// redraw path near certain.
	for range 1000 {
		id, err := NewShareID()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidShareID(id) {
			t.Fatalf("NewShareID() = %q, not a valid share ID", id)
		}
	}

	for _, bad := range []string{"", "short", "8xK29qLm4Tz7x", "8xK29qLm4T/7", "../../etc/pa", "-xK29qLm4Tz7"} {
		if ValidShareID(bad) {
			t.Errorf("ValidShareID(%q) = true, want false", bad)
		}
	}
}

// Clients accept longer IDs than the server issues today, up to a limit, so
// a server can lengthen its IDs; the shape rules stay the same.
func TestValidLinkShareID(t *testing.T) {
	for _, ok := range []string{"8xK29qLm4Tz7", "8xK29qLm4Tz7x", "Qw3rTy7uIo9pAb_-", strings.Repeat("a", MaxLinkShareIDLength)} {
		if !ValidLinkShareID(ok) {
			t.Errorf("ValidLinkShareID(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "short", "8xK29qLm4Tz", strings.Repeat("a", MaxLinkShareIDLength+1),
		"8xK29qLm4T/7", "../../etc/passwd", "-xK29qLm4Tz7x", "8xK29qLm4Tz7.x"} {
		if ValidLinkShareID(bad) {
			t.Errorf("ValidLinkShareID(%q) = true, want false", bad)
		}
	}
}

func TestDeleteTokenHash(t *testing.T) {
	tok, err := NewDeleteToken()
	if err != nil {
		t.Fatal(err)
	}
	if h := HashToken(tok); len(h) != 32 {
		t.Errorf("hash length = %d, want 32", len(h))
	}
	if bytes.Equal(HashToken(tok), HashToken(tok+"x")) {
		t.Error("different tokens hash equal")
	}
}
