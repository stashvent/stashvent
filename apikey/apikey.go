// Package apikey generates, checks, and hashes StashVent API keys.
//
// It lives outside internal/ on purpose: the closed-source hosted account
// service is another Go module, and it must issue keys in exactly
// the format and hash this server checks. The format is a contract with that
// service and with every key already issued: change it like the /v1 API.
//
// A key is "sv_", then 43 random base62 characters (256 bits), then a
// 6-character base62 CRC32 of those 43. The checksum catches a mistyped or
// truncated key before any request, and lets secret scanners tell a real key
// from a string that merely starts with sv_.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"hash/crc32"
	"math/big"
	"strings"
)

const (
	// Prefix starts every key, so a leaked one can be recognized.
	Prefix = "sv_"
	// randomLength base62 characters carry 256 bits (43 × log2 62 ≈ 256.03).
	randomLength   = 43
	checksumLength = 6 // the largest CRC32, 4294967295, needs 6 base62 digits
	// Length is the length of a whole key.
	Length = len(Prefix) + randomLength + checksumLength
)

// Pattern is a regular expression that matches a key, for secret scanners.
const Pattern = `\bsv_[0-9A-Za-z]{49}\b`

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// New returns a fresh random key. Show it once; store only Hash(key).
func New() (string, error) {
	n := big.NewInt(int64(len(alphabet)))
	random := make([]byte, randomLength)
	for i := range random {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", fmt.Errorf("generating API key: %w", err)
		}
		random[i] = alphabet[idx.Int64()]
	}
	return Prefix + string(random) + checksum(string(random)), nil
}

// Valid reports whether s has the shape of a key and its checksum matches.
// It says nothing about whether a server knows the key.
func Valid(s string) bool {
	if len(s) != Length || !strings.HasPrefix(s, Prefix) {
		return false
	}
	body := s[len(Prefix):]
	for i := range len(body) {
		if strings.IndexByte(alphabet, body[i]) < 0 {
			return false
		}
	}
	random, sum := body[:randomLength], body[randomLength:]
	return checksum(random) == sum
}

// Hash returns the SHA-256 of a key, the only form a server stores. Keys are
// 256-bit random secrets, so they are looked up by their hash directly.
func Hash(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// checksum is the CRC32 (IEEE) of random in checksumLength base62 digits.
func checksum(random string) string {
	v := crc32.ChecksumIEEE([]byte(random))
	out := make([]byte, checksumLength)
	for i := checksumLength - 1; i >= 0; i-- {
		out[i] = alphabet[v%uint32(len(alphabet))]
		v /= uint32(len(alphabet))
	}
	return string(out)
}
