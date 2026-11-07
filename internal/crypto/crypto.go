package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
)

const (
	PBKDF2Iterations = 600_000 // OWASP recommendation for PBKDF2-HMAC-SHA256
	SaltSize         = 16      // bytes
	KeySize          = 32      // bytes, AES-256
	NonceSize        = 12      // bytes, GCM standard nonce
	TagSize          = 16      // bytes, GCM tag: a ciphertext is this much longer than its plaintext
	PasswordLength   = 24      // characters, alphanumeric
	ShareIDLength    = 12      // characters of URL-safe base64 (72 bits)
	DeleteTokenSize  = 32      // random bytes before base64url encoding
)

// MinPasswordLength is the shortest custom password the CLI accepts, in
// characters. Anyone with the link can download the ciphertext and guess
// passwords offline, so a short one would make the link alone enough.
const MinPasswordLength = 12

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// GeneratePassword returns a PasswordLength-character alphanumeric password.
// Each character is drawn uniformly with crypto/rand (no modulo bias).
func GeneratePassword() (string, error) {
	n := big.NewInt(int64(len(passwordAlphabet)))
	b := make([]byte, PasswordLength)
	for i := range b {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", fmt.Errorf("generating password: %w", err)
		}
		b[i] = passwordAlphabet[idx.Int64()]
	}
	return string(b), nil
}

// NewSalt returns SaltSize fresh random bytes. Never reuse a salt across shares.
func NewSalt() ([]byte, error) {
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}
	return salt, nil
}

// DeriveKey derives an AES-256 key from password and salt with PBKDF2-SHA256.
func DeriveKey(password string, salt []byte) ([]byte, error) {
	return deriveKey(password, salt, PBKDF2Iterations, KeySize)
}

// deriveKey takes explicit parameters so tests can run published vectors
// without ever changing PBKDF2Iterations.
func deriveKey(password string, salt []byte, iterations, size int) ([]byte, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, size)
	if err != nil {
		return nil, fmt.Errorf("deriving key: %w", err)
	}
	return key, nil
}

// Encrypt seals plaintext with AES-256-GCM under a fresh random nonce, which
// is returned alongside the ciphertext (ciphertext includes the GCM tag).
// aad (associated data) is authenticated but not encrypted: Decrypt fails
// unless it gets the same aad.
func Encrypt(key, plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generating nonce: %w", err)
	}
	ciphertext, err = seal(key, nonce, plaintext, aad)
	if err != nil {
		return nil, nil, err
	}
	return ciphertext, nonce, nil
}

// seal encrypts under a caller-chosen nonce. Only Encrypt (with a fresh random
// nonce) and known-answer tests call it; a nonce must never repeat for a key.
func seal(key, nonce, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce length %d", len(nonce))
	}
	return gcm.Seal(nil, nonce, plaintext, aad), nil
}

// Decrypt opens an AES-256-GCM ciphertext. Any authentication failure (wrong
// password, tampered data, or different aad) is a hard error; no partial
// plaintext is returned.
func Decrypt(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce length %d", len(nonce))
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (wrong password or corrupted data): %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}
	return gcm, nil
}

// NewShareID returns a ShareIDLength-character URL-safe base64 share ID that
// never starts with '-', so a bare ID on the command line (`stashvent pull
// <id>`) can't be mistaken for a flag. IDs starting with '-' are redrawn,
// which keeps the rest uniformly random at a cost of well under 1 bit.
func NewShareID() (string, error) {
	b := make([]byte, ShareIDLength*6/8) // 72 bits encode to exactly 12 characters
	for {
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generating share ID: %w", err)
		}
		if id := base64.RawURLEncoding.EncodeToString(b); id[0] != '-' {
			return id, nil
		}
	}
}

// ValidShareID reports whether id has the shape of a share ID, so malformed
// values can be rejected without a database lookup.
func ValidShareID(id string) bool {
	return len(id) == ShareIDLength && shareIDShape(id)
}

// MaxLinkShareIDLength is the longest share ID a client accepts in a link.
const MaxLinkShareIDLength = 64

// ValidLinkShareID is ValidShareID for clients: it also accepts IDs longer
// than ShareIDLength, up to MaxLinkShareIDLength, so a server can lengthen
// its IDs without breaking the clients already installed.
func ValidLinkShareID(id string) bool {
	return len(id) >= ShareIDLength && len(id) <= MaxLinkShareIDLength && shareIDShape(id)
}

// shareIDShape reports whether id is URL-safe base64 that doesn't start with
// '-' (which a command line would take for a flag).
func shareIDShape(id string) bool {
	if id == "" || id[0] == '-' {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// NewDeleteToken returns a random base64url delete token. Only its HashToken
// value is ever stored server-side.
func NewDeleteToken() (string, error) {
	b := make([]byte, DeleteTokenSize)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating delete token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the SHA-256 of a delete token. Compare hashes with
// crypto/subtle.ConstantTimeCompare, never ==.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
