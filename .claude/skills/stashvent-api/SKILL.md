---
name: stashvent-api
description: HTTP API contract between the StashVent CLI and server — endpoints, request/response shapes, status and error codes, validation limits, rate limiting, logging rules, and compatibility. Load whenever writing or reviewing server handlers (apps/stashvent-server), the CLI's HTTP client, or the shared request/response types in internal/api.
---

# StashVent API Contract

The CLI and server are two halves of one contract. Both import the same
request/response types from `internal/api`, so they can't drift apart.
Never define a request or response struct separately on either side.

## Endpoints (all under `/v1`)

| Method & path | Auth | Success | Purpose |
|---|---|---|---|
| `POST /v1/shares` | API key, when `REQUIRE_API_KEY` is on | `201` | Create a `pending` share; returns share ID, delete token, presigned PUT URL |
| `POST /v1/shares/{id}/complete` | delete token | `204` | Verify the uploaded object, mark the share `active` |
| `GET /v1/shares/{id}` | none | `200` | Metadata (salt, iv, format fields) + presigned GET URL; counts toward the pull cap |
| `GET /v1/shares/{id}/summary` | none | `200` | Size, created, expiry only, for a page a share link opens; doesn't count, signs nothing |
| `DELETE /v1/shares/{id}` | delete token, **or** the API key that created the share (keys on) | `204` | Revoke: delete object, end the share |
| `GET /healthz` | none | `200` | Process is alive |
| `GET /readyz` | none | `200`, or `503` | Postgres and object storage are reachable |

- The health endpoints answer in plain text (`ok`, `not ready`), not in the
  JSON error shape: probes read the status only. Which dependency failed is
  in the log, never in the response. The checks run at the same time, with
  2 seconds each. A `/readyz` result is reused for 1 second after the
  checks end, because the endpoint is open and not rate-limited.
- The storage check is a `HeadBucket`, so the access key needs bucket-level
  list permission (`s3:ListBucket` on AWS), not only object permissions.

- `/complete` is a deliberate action endpoint (a state transition), in the
  style of Stripe's `/confirm`. No other verbs in URLs.
- The delete token is sent as `X-Delete-Token: <delete_token>`
  (`api.DeleteTokenHeader`). It also authorizes `/complete`, so only the
  share's creator can activate it.
- `Authorization: Bearer sv_…` carries the sender's API key,
  and is read only when `REQUIRE_API_KEY` is on; with keys off it is
  ignored on every route. It is required on `POST /v1/shares`, and is a
  second way to authorize `DELETE` of a share that key created. `/complete`
  never accepts it, so a key can never activate an upload. User login
  (v1.x) doesn't add another kind of credential: it lives in the closed
  hosted account service and ends in an API key. Don't
  put the delete token there: a request may carry both.
- No endpoint lists the shares a key created: a leaked key would become a
  list of the sender's links.
- The payload itself never goes through the API. Reject any attempt to
  send blob bytes to the server.

### Create share

```json
// POST /v1/shares
{
  "size": 342192,
  "expires_in": "6h",
  "salt": "<base64, 16 bytes>",
  "iv": "<base64, 12 bytes>",
  "payload_format": "stashvent-v1",
  "packaging_method": "stash-ref",
  "bundle_version": 2,
  "object_format": "sha1",
  "sender_git_version": "2.32.0.windows.2"
}

// 201 Created
{
  "share_id": "8xK29qLm4Tz7",
  "delete_token": "<base64url, returned once, never again>",
  "upload_url": "<presigned PUT URL>",
  "upload_expires_at": "2026-09-26T11:05:00Z",
  "expires_at": "2026-09-26T16:50:00Z",
  "upload_headers": { "<name>": "<value>" }   // optional; none today
}
```

- The presigned PUT signs the exact `Content-Length`, so the upload can't
  exceed the declared `size`.
- `upload_headers` lists headers the upload must carry because the server
  signed them into the URL; the CLI sends each one as given. The server
  sends none today. It exists so that a server can start signing headers
  (server-side encryption, a storage class, `Content-Type`) without breaking
  the CLIs already installed, which wouldn't know to send them. It can only
  hold values the server knows: not a checksum of the payload, which the
  server never sees and installed CLIs don't send.
- The CLI takes any 2xx from the upload as success: S3-compatible stores
  answer a PUT with 200, 201, or 204.
- `/complete` checks the stored object's size against `size`. On mismatch it
  deletes the object and returns `422 size_mismatch`. It returns the same
  when no object was uploaded at all.
- A share can be completed for 1 hour after it was created
  (`storage.PendingTTL`). After that `/complete` returns `404
  share_not_found`, and the expiry sweep removes the share.
- With keys on, the checks run in this order, before the body is read:
  no `Authorization` (`401 missing_api_key`); not `Bearer <key>`, or a key
  that fails `apikey.Valid` (`401 invalid_api_key`, no database lookup);
  unknown, revoked, or banned (the same `401`); the per-key rate limit
  (`429 rate_limited`). Then the body is validated as above, then the
  storage cap (`503 storage_full`), then the daily quota, counted in the
  same transaction as the insert (`429 quota_exceeded`).
- The share records the key that created it (`shares.api_key_id`).
- The insert's transaction checks the key once more and holds its row
  (`LockActiveAPIKey`, `FOR SHARE`). So a key revoked or banned while the
  request ran gets `401 invalid_api_key`, and a ban that starts during the
  insert waits for it and then ends that share too. Never drop that lock:
  without it a ban can miss a share created at the same moment.

### Delete share

- Works on an unexpired share in any status, so a sender can also cancel a
  `pending` share. Unknown and expired shares are `404 share_not_found`,
  and so is a second DELETE of the same share.
- Deletes the object, then ends the share by moving its expiry to now
  (`RevokeShare`), so every endpoint answers `404` from then on. If the
  object can't be deleted the answer is `500` and the share is unchanged, so
  the client can retry. Once the object is gone the share is ended even if
  the client disconnects.
- **DELETE does not delete the row.** Deleting an object doesn't cancel the
  presigned PUT URL that wrote it, so the sender could put the object back
  for as long as that URL lives, and without a row nothing would ever remove
  it. The expiry sweep removes the row, and the object once more, when the
  share is 30 minutes old (upload URL TTL plus `uploadGrace`) or at its next
  run if it is older already. Never remove a row while its upload URL can
  still be used.
- No request body, no response body.
- With keys on, the API key that created the share works instead of the
  delete token. The checks run in this order (`authorizedShare` with
  `allowKey`): malformed ID (`404`); neither token nor key
  (`401 missing_delete_token`, whose message names both); a key-only
  request with a key that can't be used (`401 invalid_api_key`, before the
  lookup); the share (`404`); a matching token is let in, **whatever key
  came with it**; then the key: unusable (`401 invalid_api_key`), the one
  that created the share (let in), another (`403 share_not_owned`); last,
  a wrong token (`403 invalid_delete_token`). A revoked key can't delete;
  the token still can, also when the revoked key is sent along. With keys
  off, `Authorization` is ignored and DELETE works as before.
- DELETE and the operator's `keys ban` end a share the same way
  (`endShare` in `apps/stashvent-server/shares.go`).

### Get share

- Returns `404 share_not_found` for unknown, expired, **and pending** shares,
  with no difference in response. Don't give an attacker a way to tell them apart.
- `download_url` is a presigned GET with a short TTL (60 seconds): it can be
  reused until it expires, and the server never sees those downloads.
- Each lookup of an active share counts toward the pull cap
  (`MAX_PULLS_PER_SHARE`, default 50, `0` = no cap), counted before the
  download URL is signed (signing is local, so it doesn't fail on a storage
  outage): check and count are one `UPDATE`. Past the cap
  the answer is `410 pull_limit_reached` (permanent, no `Retry-After`);
  unknown, expired, and pending shares stay `404`, and don't count.
  Anyone with the link can use up the pulls, even without the password;
  that trade-off is accepted. A page that only shows a share must not use
  this endpoint; it uses the summary.

### Share summary

```json
// GET /v1/shares/{id}/summary → 200
{ "share_id": "8xK29qLm4Tz7", "size": 342192,
  "created_at": "2026-09-26T10:50:00Z", "expires_at": "2026-09-26T16:50:00Z" }
```

- `api.ShareSummaryResponse`. It never carries anything that helps
  download the share: no `download_url`, `salt`, `iv`, or format fields.
  Adding a field means asking whether a page may show it.
- A read only (`GetActiveShare`): it doesn't count toward the pull cap and
  signs no URL. `TestShareSummary` checks both.
- Otherwise it answers like GET: `404 share_not_found` for unknown,
  expired, deleted, and pending shares, and `410 pull_limit_reached` once
  the share's pulls are used up, so the page doesn't offer a pull that
  would fail.
- In the lookup rate-limit class. A page that calls it from its own server
  has one address for all its visitors, so that server must be in
  `TRUSTED_PROXIES` and forward each visitor's address in
  `X-Forwarded-For` (only from behind a proxy that appends it); otherwise
  every visitor shares the page's bucket.
- The share page that uses it is hosted-only (decided 2026-10-27); this
  repo has the endpoint.
- The cap check is `storage.Share.PullsUsedUp`, the same rule the
  `LookupShare` query applies in SQL; change both together.

## Validation limits

- `expires_in`: one of `1h`, `6h`, `24h`, `7d`. Default `6h`
  is applied by the CLI when the user passes no `--expires`.
- `size`: more than `crypto.TagSize` (an encrypted payload is more than its
  GCM tag) and `<=` the server's configured maximum payload size
  (`MAX_PAYLOAD_BYTES`, default 5 MB); otherwise `400 invalid_request` or
  `413 payload_too_large`. A server can set less, never more: the CLI
  never sends or downloads more than `api.DefaultMaxPayloadBytes`, so the
  server refuses to start above it rather than accept shares no CLI can
  pull.
- `salt`: exactly 16 bytes; `iv`: exactly 12 bytes after base64 decoding.
- `payload_format`: known values only (`stashvent-v1`).
- `share_id` path values: 12 URL-safe base64 characters; anything else is
  `404` without a database lookup.
- JSON bodies are limited with `http.MaxBytesReader` (e.g. 16 KB); they
  only carry metadata.

## JSON conventions

- Field names in `snake_case`.
- Binary values (salt, iv) as standard base64 strings.
- Timestamps as RFC 3339 in UTC (`2026-09-27T10:50:00Z`).
- Sizes in bytes, as integers.

## Errors

Every non-2xx response from a `/v1` handler has this shape:

```json
{ "error": { "code": "share_not_found", "message": "Share not found or expired" } }
```

Two kinds of response are outside it, both plain text: a path or method no
route matches (`http.ServeMux`'s own `404` and `405`), and the health
endpoints. Clients must not expect an error code from those.

- `code` is stable and machine-readable; `message` is for humans and may
  change. **The CLI switches on `code`, never on `message`.**
- Never include stack traces, SQL errors, or internal details in `message`.
- For `invalid_request` and for a code it has no wording for, the CLI shows
  `message` (cleaned of control characters, at most 200 characters, with
  the request ID). So write it for the person at the terminal: it is all an
  older CLI can show for a code added later ("This server needs stashvent
  v0.3 or newer", a new limit). `internal` keeps the CLI's generic wording.

| Status | `code` | When |
|---|---|---|
| 400 | `invalid_request` | Malformed JSON, missing or invalid fields |
| 401 | `missing_delete_token` | No `X-Delete-Token` header where required (DELETE with keys on: neither token nor key) |
| 401 | `missing_api_key` | `REQUIRE_API_KEY` is on and `POST /v1/shares` has no key |
| 401 | `invalid_api_key` | Malformed, unknown, revoked, or banned key: all one answer, so a probe can't tell them apart |
| 403 | `invalid_delete_token` | Token doesn't match |
| 403 | `share_not_owned` | DELETE with a valid key that didn't create this share (and no matching delete token) |
| 404 | `share_not_found` | Unknown or expired share; for GET and the summary also a pending one; `/complete` after 1 hour pending |
| 409 | `share_not_pending` | `/complete` on a share that's already active |
| 410 | `pull_limit_reached` | `GET /v1/shares/{id}` (or its summary) after `MAX_PULLS_PER_SHARE` lookups |
| 413 | `payload_too_large` | `size` above `MAX_PAYLOAD_BYTES` |
| 422 | `size_mismatch` | Uploaded object size differs from declared `size`, or nothing was uploaded |
| 429 | `rate_limited` | Rate limit hit (with `Retry-After` header, in whole seconds) |
| 429 | `quota_exceeded` | The key's shares for today are used up; `Retry-After` until UTC midnight. Apart from `rate_limited`, so the CLI can say "daily limit" |
| 500 | `internal` | Anything unexpected, a panic included (details go to logs only) |
| 503 | `storage_full` | The total storage cap (`MAX_STORED_BYTES`) is reached |

`/complete` and DELETE check in this order: malformed ID (`404`), missing
token (`401`), share lookup (`404`), token (`403`). So a wrong token tells an
existing share from an unknown one; GET never does. DELETE with keys on adds
the key checks (see "Delete share").

## Server implementation

- Routing with the standard library `http.ServeMux` (Go 1.22+ patterns):
  `mux.HandleFunc("POST /v1/shares/{id}/complete", h.completeShare)`.
- Always set `http.Server` timeouts: `ReadHeaderTimeout`, `ReadTimeout`,
  `WriteTimeout`, `IdleTimeout`. Never use a zero-timeout server.
- Those don't stop a handler that waits on Postgres or storage, so every
  request context also has a 10 second deadline (`requestTimeout`). Pass
  `r.Context()` to every store and storage call. `WriteTimeout` is derived
  from the slowest handler (the request deadline plus DELETE's detached
  step), so a handler that finishes in time is never cut off.
- Call `http.MaxBytesReader` with `baseWriter(w)`, not `w`: handlers get a
  wrapped writer, and through a wrapper an oversized body no longer closes
  the connection.
- Handlers are thin: decode, validate, call the service/store, encode.
  Business rules live outside handlers.
- Compare delete tokens by hashing (SHA-256) and using
  `subtle.ConstantTimeCompare` (see `stashvent-crypto`).
- Presigned URL TTLs: PUT 15 minutes, GET 60 seconds (`downloadURLTTL`),
  signed with the public object-storage endpoint. The CLI
  downloads right after the lookup, and S3 checks expiry when a request
  starts, so a slow download still finishes.
- No CORS headers. The CLI doesn't need them, and the hosted share page
  calls the API server-side, never from browser JavaScript. It only
  renders size, created, and expiry, from the summary; it never calls
  `GET /v1/shares/{id}`.

## Rate limiting

- Per client IP, token bucket (`golang.org/x/time/rate`), with stricter
  limits on `POST /v1/shares` and `GET /v1/shares/{id}`. A client has a
  separate bucket in each of three classes:

  | Class | Routes | Variables | Default |
  |---|---|---|---|
  | create | `POST /v1/shares` | `RATE_LIMIT_CREATE_PER_MINUTE`, `RATE_LIMIT_CREATE_BURST` | 10 per minute, burst 5 |
  | lookup | `GET /v1/shares/{id}`, `GET /v1/shares/{id}/summary` | `RATE_LIMIT_LOOKUP_PER_MINUTE`, `RATE_LIMIT_LOOKUP_BURST` | 30 per minute, burst 10 |
  | other | `/complete`, `DELETE` | `RATE_LIMIT_OTHER_PER_MINUTE`, `RATE_LIMIT_OTHER_BURST` | 60 per minute, burst 20 |

  The defaults are development values; each instance chooses its own for
  production. A value that isn't a whole number above zero stops the server
  at startup. There is no off switch.
- With keys on, `POST /v1/shares` also has a bucket per API key
  (`RATE_LIMIT_KEY_CREATE_PER_MINUTE`, `RATE_LIMIT_KEY_CREATE_BURST`,
  default 10 per minute, burst 5), on top of the per-address one: a key
  used from many addresses is still one sender. It is checked in the
  handler once the key is known (`limiter[int64]`, keyed by the key's ID).
- A new route under `/v1` must be wrapped with one of the three limiters in
  `newMux`. Limits are applied per route, before the handler validates
  anything.
- Over the limit: `429 rate_limited` with `Retry-After` in whole seconds. A
  refused request doesn't use a token, so retrying early doesn't make the
  wait longer.
- Only trust `X-Forwarded-For` from configured proxy addresses
  (`TRUSTED_PROXIES`, comma-separated IPs or CIDR ranges, empty by default);
  otherwise use the connection's remote address. From a trusted proxy, the
  client is the rightmost entry that isn't a trusted proxy itself. Never use
  the leftmost entry: the client chooses it.
- An IPv6 client is counted by its /64.
- Health endpoints are not rate-limited.
- The buckets are in memory, per replica: with N replicas a client gets up
  to N times the limit. A client is forgotten once its bucket has refilled
  (20 to 30 seconds with the defaults), at a sweep that runs once a minute.
  One limiter tracks at most 100 000 clients; beyond that a new client gets
  `429` until the next sweep. Don't keep buckets longer than their refill
  time: it adds nothing and makes that cap cheap to hold.
- `TRUSTED_PROXIES` entries and request addresses are compared as plain
  IPv4 or IPv6: IPv4-mapped forms (`::ffff:10.0.0.0/104`) are stored as
  IPv4, and zones (`%eth0`) are dropped.

## Logging

- `log/slog` JSON, one line per request (`msg` is `request`):
  `request_id`, `method`, `route` (the mux pattern, e.g.
  `GET /v1/shares/{id}`; empty when no route matched), `status`,
  `latency_ms`, plus `share_id` when the path holds a well-formed one and
  `user_agent` cut to 64 characters. Health requests log at debug level.
- The request ID is generated by the server (a client-sent `X-Request-Id`
  is ignored) and returned in `X-Request-Id` on every response. It is in
  the request's context, and the log handler adds it to every line logged
  with that context, so always log with `logger.…Context(r.Context(), …)`.
- **Never log:** delete tokens, API keys (valid, wrong, or malformed),
  request headers (`X-Delete-Token`, `Authorization`), presigned URLs
  (their query string is a credential), the request URL or its query
  string, or request/response bodies. Client IP addresses aren't logged
  either. `TestLogsHoldNoSecrets` checks this across every endpoint; extend
  it when adding one.
- Share IDs may be logged; they aren't secret on their own.

## API keys and abuse controls

Everything here is in the open-source server; the hosted
instance turns it on. All values are read at startup; one that can't be
used (negative, not a number, `REQUIRE_API_KEY` not a boolean) stops the
server.

| Variable | Default | Meaning |
|---|---|---|
| `REQUIRE_API_KEY` | `false` | `POST /v1/shares` needs a key; DELETE also accepts one |
| `API_KEY_SHARES_PER_DAY` | `10` | Shares one key may create per UTC day; `0` = no quota |
| `MAX_STORED_BYTES` | `0` | Cap on the declared size of all unexpired shares, pending ones included; `0` = no cap |
| `MAX_PULLS_PER_SHARE` | `50` | Lookups one share answers; `0` = no cap |

- Keys: `sv_` + 43 base62 characters (256 bits) + a 6-character base62
  CRC32 of those 43. Generated, checked, and hashed only by the `apikey`
  package at the repo root (outside `internal/`, so the closed hosted
  service can import it). The server stores `apikey.Hash(key)` (SHA-256)
  in `api_keys.key_hash` and looks keys up by it.
- The quota counts a share when it is created, completed or not, in
  `api_key_usage` (not by counting `shares`, whose rows the sweep deletes).
  The day is the UTC date of the server's clock, passed in, never
  `current_date`. It applies only with keys on.
- The storage cap is checked before the insert, so concurrent creates can
  overshoot it by a few shares. It applies whether keys are on or not.
- Startup logs the rate limits and an `abuse controls` line (keys on or
  off, quota, storage cap, pull cap). With keys off it also logs one
  `warn` line: anyone who can reach the server can create shares.
- Operator commands (`stashvent-server keys …`, in
  `apps/stashvent-server/keys.go`): `issue --label <l>` (prints the key
  once on stdout), `list [--share <id>]`, `revoke <key-id>` (the key's
  shares stay pullable), `ban <key-id>` (also revokes, then ends every live
  share of the key; safe to run again), `usage [<key-id>] [--days N]`.
  They read `DATABASE_URL` (and, for `ban`, the S3 settings) and apply
  migrations. Keys are named by `api_keys.id`, which never appears in the
  API.

## Compatibility

- Within `/v1`, changes are additive only: new optional request fields,
  new response fields. Removing or renaming a field, or making a new field
  required, means `/v2`.
- The server ignores unknown request fields (so newer CLIs work with older
  servers). The CLI ignores unknown response fields.
- The CLI sends `User-Agent: stashvent/<version>` so the server can log
  and, if ever needed, reject very old clients with a clear error. A server
  that stores shares in a newer format can use it to refuse those shares
  to a CLI too old to read them, before counting the pull.
- Installed CLIs stay in use for years, so what v0.1 does with something it
  doesn't know is part of the contract too (decided 2026-11-04, before the
  freeze). A server may rely on all of it:
  - an unknown error code shows the server's `message` (see "Errors");
  - a share whose `payload_format`, `packaging_method`, `bundle_version`, or
    `object_format` holds a value it doesn't know gets "upgrade stashvent",
    before any download (`payload.KnownFormat`). A new value needs a CLI
    that reads it, and for `payload_format` and `packaging_method` a server
    that accepts it: today's server refuses unknown values of those two, and
    a salt or IV of another size, with `invalid_request`. So a CLI that
    writes a new format must handle older servers refusing it;
  - a link's share ID may be 12 to 64 URL-safe characters
    (`crypto.ValidLinkShareID`), so a server can lengthen its IDs; servers
    issue `crypto.ShareIDLength` and check their own exactly
    (`crypto.ValidShareID`);
  - the upload carries `upload_headers` and takes any 2xx as success;
  - a share whose `size` is above `api.DefaultMaxPayloadBytes` gets
    "upgrade stashvent" on pull, before any download, and one at or below
    `crypto.TagSize` is refused as invalid (`apiclient.Download` refuses
    both too). So allowing larger payloads needs a CLI release that raises
    the constant;
  - API response bodies are read up to 64 KB (`maxResponseBytes`), on
    success and on error, so responses must stay small.

## Testing

- Handler tests with `httptest`, table-driven over every row of the error
  table above (`TestErrorTable` in
  `apps/stashvent-server/errortable_test.go`, which fails if an error code
  has no case).
- One end-to-end test runs the real server against the Docker Compose
  Postgres and SeaweedFS and does create → upload → complete → get → delete
  (`apps/stashvent-server/server_integration_test.go`, `make test-integration`).
  `TestAPIKeysOnDockerStack` (`keys_integration_test.go`) does the same with
  keys on: issue, push, the pull cap, delete with the key, ban.
- Key, quota, cap, and DELETE-with-key behaviour: `abuse_test.go`; the
  operator commands: `keys_test.go`, on the same fakes as the handlers.
- On the Docker stack (`validation_integration_test.go`):
  `TestDeleteRefusalsOnDockerStack` (every way a DELETE is refused, and that a
  refusal changes nothing), `TestSweepOnDockerStack` (the real `sweep()` on
  the real table and bucket: what goes, what stays), `TestStorageCapOnDockerStack`,
  and `TestNoRepoDetailsOnServer` (no repository detail in any table value,
  object, log line, or request). The integration server's `recorder`
  (`s.seen`) keeps its log and every request it received, for tests that
  check what the server saw. `s.send` sends a raw request with a delete
  token, a key, both, or neither; `newIsolatedServer` gives a test a schema
  of its own (see the `stashvent-postgres` skill).
- With the built binaries (`e2e/`, `make test-e2e`): the server
  runs on its own with keys on (`make run-server-e2e` locally, the server
  image in CI), and the tests run `stashvent` and `stashvent-server keys …`
  as processes: login, the pull cap, the quota, revoke, ban, `keys list
  --share`, `STASHVENT_API_KEY`, Ctrl+C. A change to these settings goes in
  `E2E_SERVER_ENV` in the Makefile and in the CI `e2e` job's env.
- `-race` and a newer git on Linux: `make test-linux` and `make
  test-linux-integration` (Docker; CI's `test` job runs `-race` on unit tests
  only).
