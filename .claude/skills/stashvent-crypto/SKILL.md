---
name: stashvent-crypto
description: Password-based encryption rules for StashVent (PBKDF2-SHA256 + AES-256-GCM) — key derivation, salt/IV/nonce handling, storage boundaries, and common cryptographic mistakes to avoid. Load whenever writing or reviewing code that generates passwords, derives keys, encrypts/decrypts stash payloads, or touches metadata storage (Postgres/S3) for shares.
---

# StashVent Crypto Rules

This project's threat model: the server must never see plaintext source code
or the password. Only encrypted blobs + non-secret metadata are stored.

## Parameters

Define these once as constants in `internal/crypto` and reference them
everywhere. Never inline the numbers.

```go
const (
    PBKDF2Iterations = 600_000 // OWASP recommendation for PBKDF2-HMAC-SHA256
    SaltSize         = 16      // bytes
    KeySize          = 32      // bytes, AES-256
    PasswordLength   = 24      // characters, alphanumeric
    ShareIDLength    = 12      // characters of URL-safe base64 (72 bits)
)
```

## Key Derivation

- Algorithm: **PBKDF2-SHA256** via the standard library `crypto/pbkdf2`
  (Go 1.24+), not `golang.org/x/crypto/pbkdf2`.
- Salt: `SaltSize` random bytes, generated fresh per share with `crypto/rand`
  (never `math/rand`). Never reuse a salt across shares.
- Salt is NOT secret. It is stored in Postgres metadata.

```go
func DeriveKey(password string, salt []byte) ([]byte, error) {
    key, err := pbkdf2.Key(sha256.New, password, salt, PBKDF2Iterations, KeySize)
    if err != nil {
        return nil, fmt.Errorf("deriving key: %w", err)
    }
    return key, nil
}
```

## Encryption (AES-256-GCM)

- Generate a fresh random **nonce/IV per encryption operation**
  (`gcm.NonceSize()`, 12 bytes) via `crypto/rand`. **Never reuse a nonce with
  the same key.** Nonce reuse under GCM breaks both confidentiality and
  authenticity.
- The nonce is NOT secret. It is stored in Postgres metadata as
  `iv`, and the blob in object storage is the raw ciphertext + GCM tag only.
  Never derive the nonce from content, and never reuse it across pushes of
  the same stash.
- Always use the authenticated `Seal`/`Open` API (`cipher.AEAD`). Never
  implement CTR mode manually with a separate MAC.
- Treat GCM tag verification failure as a hard failure (wrong password or
  tampered blob). Never attempt partial decryption or ignore the error.

```go
func Encrypt(key, plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
    nonce = make([]byte, NonceSize)
    if _, err := rand.Read(nonce); err != nil {
        return nil, nil, fmt.Errorf("generating nonce: %w", err)
    }
    ciphertext, err = seal(key, nonce, plaintext, aad) // gcm.Seal(nil, nonce, plaintext, aad)
    if err != nil {
        return nil, nil, err
    }
    return ciphertext, nonce, nil
}

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
```

- `seal` (caller-chosen nonce) is unexported and exists only for `Encrypt` and
  known-answer tests. Production code never picks a nonce.

## Payload and associated data (`internal/payload`)

The plaintext that `Encrypt` seals is the `stashvent-v1` payload:

```
"SVP1" (4 bytes) | manifest length (uint32 big-endian) | manifest JSON | bundle bytes
```

- The manifest (≤ 64 KB) holds the descriptive details. The bundle is the rest
  and must be non-empty. There's no compression: the bundle is already a packfile.
- `Decode` treats the payload as untrusted (it comes from another user): check
  every length before slicing, and reject a `bundle_ref` outside `refs/share/`.
- Manifest fields may be added within v1. Readers ignore unknown fields and
  tolerate missing ones. Changing the layout itself means a new `payload_format`.
- The key derivation and the cipher are part of `stashvent-v1` too:
  PBKDF2-SHA256 with `PBKDF2Iterations` and a `SaltSize` salt, AES-256-GCM
  with a `NonceSize` nonce. Changing any of them (more iterations, Argon2id)
  means a new `payload_format`, never a new API field: a CLI that doesn't
  know the field would derive the key the old way and report a wrong
  password instead of asking for an upgrade.
- Pull refuses a share whose four format fields (below) hold a value it
  doesn't know, before downloading (`payload.KnownFormat`). Supporting a new
  value means adding it there, with a reader for it.

**Associated data (AAD):** exactly these four plain-text fields are bound to
the ciphertext, built by `payload.AssociatedData`:
`payload_format`, `packaging_method`, `bundle_version`, `object_format`.
Push builds the AAD from the values it sends; pull builds it from the values the
server returns. If anyone changes one, decryption fails.

- Not bound: `salt` and `iv` (changing them already breaks decryption),
  `share_id` (assigned after encryption), `size` (the download checks the exact
  length), timestamps and `sender_git_version` (informational).
- Adding a field to the AAD changes the encryption format: it needs a new
  `payload_format`.

## Password Generation

- Auto-generated passwords: `PasswordLength` alphanumeric characters, each
  picked with `crypto/rand` (e.g. `rand.Int(rand.Reader, big.NewInt(len(alphabet)))`).
  Never `math/rand`, and never `byte % len(alphabet)` (modulo bias).
- Never log the generated password or send it to the server/API. It must
  never leave the sender's machine except via the user's own separate channel.
- Custom passwords go through the same PBKDF2 path. Never special-case them
  into a weaker code path.
- Custom passwords are only read from a hidden prompt (`--password`) or
  stdin (`--password-stdin`), never from a command-line value or env var
  that ends up in shell history or the process list.
- The CLI never persists the password (not in the share history file,
  not in config). The sender stores it themselves.

## Storage Boundaries (what goes where)

- **Postgres (operational metadata only):** `share_id`, `storage_key`,
  `size`, `created_at`, `expires_at`, `status`, `salt`, `iv`,
  `delete_token_hash`, plus the bundle format fields. Never
  the password, the derived key, the raw delete token, plaintext, or
  ciphertext.
- **API keys:** `api_keys` holds `key_hash` (SHA-256 of the
  whole key), a `label` the operator chose, and timestamps (created,
  revoked, banned); never the key, and no owner or user column (accounts
  belong to the closed hosted service).
  `shares.api_key_id` records which key created a share, `shares.lookups`
  counts pulls, and `api_key_usage` counts shares and declared bytes per
  key per UTC day. None of it is a repository detail.
  `TestSharesColumnsAreAllowlisted` covers all three tables.
- **Encrypted manifest (inside the payload):** repository name, repository
  hash, branch, base commit, stash message, file count, sender name. These
  are descriptive details, and the server must never see them.
- **Object storage (S3-compatible):** the encrypted payload (bundle + manifest) only.
- Before adding a field to a server-side struct, ask: "could this leak repo
  names, usernames, branch names, file names, or source code?" If yes, it
  goes in the encrypted manifest. The only exception is a future,
  explicit sender opt-in (e.g. `--show-repo`), stored as a separate public field.
- Two tests hold these boundaries. `TestSharesColumnsAreAllowlisted` checks
  the column **names** (a new column fails until it is reviewed).
  `TestNoRepoDetailsOnServer` (`apps/stashvent-server/validation_integration_test.go`)
  checks the **values**: after a push whose repository name, branch, file,
  stash message, sender, and commit IDs all carry a marker, none of them is
  in any row of any table, any object name, the stored object, the server's
  log, or any request the server received. A new table is covered without
  changing it: the tables are listed, not named.

## Delete Tokens

- Generated server-side with `crypto/rand` (at least 32 bytes, base64url),
  returned to the sender once at share creation.
- The server stores only `sha256(token)`; verify with
  `subtle.ConstantTimeCompare` on the hashes.
- The CLI stores the raw token in the local share history file (user-only
  permissions), never the password alongside it.

## API Keys

- Format, fixed before the first key was issued: `sv_` + 43 base62
  characters, each drawn uniformly with `crypto/rand` (256 bits), + a
  6-character base62 CRC32 (IEEE) of those 43. The checksum only catches
  typos and lets scanners tell a key from look-alikes; it adds no secrecy.
- Only the `apikey` package (repo root) generates, checks, and hashes keys.
  The closed hosted service imports it, so never duplicate its logic.
- The server stores `apikey.Hash(key)` and finds a key by that hash in a
  unique index. That lookup needs no `ConstantTimeCompare`: the attacker
  would have to know the SHA-256 preimage of a 256-bit random secret. Keys
  are not passwords, so no slow KDF either.
- The CLI keeps keys in `config.json` (user-only), sends them only as
  `Authorization: Bearer`, and never prints or logs them.

## Comparisons & Side Channels

- Any comparison of secret material (e.g. a delete token, or allowlist
  checks in future versions) must use `crypto/subtle.ConstantTimeCompare`,
  never `==` or `bytes.Equal`. API keys are the one exception, by design:
  they are looked up by their hash (see "API Keys").
- Share IDs aren't secret (the password is the actual protection), but they
  must resist enumeration: `ShareIDLength` characters from `crypto/rand`,
  plus server-side rate limiting on lookups.

## Common Mistakes to Flag in Review

- Reusing a nonce/IV across multiple encryptions with the same key.
- Using `math/rand` anywhere in salt, nonce, password, or share-ID generation.
- Ignoring errors from `aes.NewCipher`, `cipher.NewGCM`, `rand.Read`, or `pbkdf2.Key`.
- Storing the password or derived key anywhere server-side, even temporarily
  (logs, error messages, metrics, request tracing).
- Lowering `PBKDF2Iterations` "for tests" in a code path shared with prod.
  Tests that need speed should inject parameters explicitly, never change the constant.
- Silently continuing after a GCM auth-tag failure instead of erroring out.
- Encrypting or decrypting a payload without the associated data from
  `payload.AssociatedData`, or building it from different values on each side.
- Known-answer test values copied from memory. Recompute them with an
  independent implementation (e.g. Python `hashlib`, Node `crypto`) first.
- Reusing the same salt for multiple independent shares.
