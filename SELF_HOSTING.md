# Running your own server

Most people don't need this: the CLI uses the hosted service,
`https://stashvent.dev`, by default, and pushing to it only needs an API key
from signing in with GitHub. Run your own
server when your code may not leave your network, or you want full control
over where shares are stored.

The `stashvent` CLI works with any StashVent server. This guide covers running
one: for your team, your company, or just yourself. For using the CLI, see
[DOCS.md](DOCS.md); for developing StashVent, [SETUP.md](SETUP.md).

- [What you need](#what-you-need)
- [Run it](#run-it)
- [Settings](#settings)
- [API keys](#api-keys)
- [Behind a reverse proxy](#behind-a-reverse-proxy)
- [Storage](#storage)
- [Operating it](#operating-it)
- [Common problems](#common-problems)

## What you need

- **The server image**, `ghcr.io/stashvent/stashvent-server` (linux/amd64).
  Pin a version tag such as `server-v0.1.1`; `:latest` follows the newest
  final version. [RELEASES.md](RELEASES.md#releasing-the-server) lists the
  tags.
- **Postgres.** The server applies its own migrations at startup. Tested with
  Postgres 18.
- **An S3-compatible bucket**, private, created beforehand. The server only
  hands out short-lived presigned URLs: payloads go from the CLI straight to
  storage and back, never through the server.
- **HTTPS.** The CLI refuses a server on plain `http://`, except `localhost`
  and loopback addresses. Put the server behind a reverse proxy that
  terminates TLS.
- **Its own origin.** The server must answer at the root of a host, such as
  `https://stash.example.com`, not under a path: a share link is the server
  plus `/<id>`.

## Run it

Put the settings in an env file, here `stashvent.env`:

```
DATABASE_URL=postgres://stashvent:<password>@db.internal:5432/stashvent?sslmode=require
S3_ENDPOINT=https://s3.us-east-1.amazonaws.com
S3_PUBLIC_ENDPOINT=https://s3.us-east-1.amazonaws.com
S3_REGION=us-east-1
S3_BUCKET=stashvent-shares
S3_ACCESS_KEY=<access key>
S3_SECRET_KEY=<secret key>
REQUIRE_API_KEY=true
```

Then start it:

```
docker run -d --name stashvent-server --restart unless-stopped \
  --env-file stashvent.env -p 8080:8080 \
  ghcr.io/stashvent/stashvent-server:server-v0.1.1
```

It listens on `:8080`. Check it, then point the CLI at it:

```
curl https://stash.example.com/readyz           # "ok" once Postgres and the bucket answer
stashvent push --server https://stash.example.com
```

To make it the default for a machine, set
`STASHVENT_SERVER=https://stash.example.com` (see
[Choosing a server](DOCS.md#choosing-a-server)).

The `docker-compose.yml` in this repository is for development: it builds the
image from source and uses development credentials. Don't use it as is.

## Settings

The server reads environment variables only; it never reads an env file
itself. A value it can't use stops it at startup, with a message naming the
variable.

**Required**

| Variable | What it is |
|---|---|
| `DATABASE_URL` | Postgres connection URL |
| `S3_ENDPOINT` | how the server reaches storage |
| `S3_PUBLIC_ENDPOINT` | the storage URL the CLI uses; presigned URLs are signed for it (see [Storage](#storage)) |
| `S3_BUCKET` | the bucket; it must exist |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | the storage credentials |

**Optional**

| Variable | Default | What it does |
|---|---|---|
| `S3_REGION` | `us-east-1` | the bucket's region |
| `S3_USE_PATH_STYLE` | `false` | `true` for `https://host/bucket/key` URLs (most self-hosted stores), `false` for `https://bucket.host/key` |
| `SERVER_ADDR` | `:8080` | the address to listen on |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `MAX_PAYLOAD_BYTES` | `5242880` (5 MB) | the largest encrypted stash; can be lowered, not raised: the CLI never sends more |
| `REQUIRE_API_KEY` | `false` | `true`: pushing needs an API key (see [API keys](#api-keys)) |
| `API_KEY_SHARES_PER_DAY` | `10` | shares one key may create per UTC day; `0` turns the quota off |
| `MAX_STORED_BYTES` | `0` | cap on the declared size of all unexpired shares; `0` is no cap. Size it to your bucket |
| `MAX_PULLS_PER_SHARE` | `50` | lookups one share answers before it stops; `0` is no cap |
| `TRUSTED_PROXIES` | empty | addresses or CIDR ranges of your reverse proxies, comma-separated (see [Behind a reverse proxy](#behind-a-reverse-proxy)) |
| `RATE_LIMIT_CREATE_PER_MINUTE`, `RATE_LIMIT_CREATE_BURST` | `10`, `5` | creating shares, per client address |
| `RATE_LIMIT_LOOKUP_PER_MINUTE`, `RATE_LIMIT_LOOKUP_BURST` | `30`, `10` | looking shares up, per client address |
| `RATE_LIMIT_OTHER_PER_MINUTE`, `RATE_LIMIT_OTHER_BURST` | `60`, `20` | completing and deleting shares, per client address |
| `RATE_LIMIT_KEY_CREATE_PER_MINUTE`, `RATE_LIMIT_KEY_CREATE_BURST` | `10`, `5` | creating shares, per API key, on top of the per-address limit |

The rate limits are counted in each replica's memory. The startup log shows
the limits, the abuse controls, and the trusted proxies in force.

## API keys

With `REQUIRE_API_KEY=false`, anyone who can reach the server can push. That's
fine on a private network; turn keys on before exposing a server to the
internet. Pulling never needs a key.

Keys are managed with the server binary itself, which reads `DATABASE_URL` like
the server does. In the running container:

```
docker exec stashvent-server /stashvent-server keys issue --label "Alice laptop"
docker exec stashvent-server /stashvent-server keys list
docker exec stashvent-server /stashvent-server keys list --share 8xK29qLm4Tz7
docker exec stashvent-server /stashvent-server keys usage
docker exec stashvent-server /stashvent-server keys revoke <key-id>
docker exec stashvent-server /stashvent-server keys ban <key-id>
```

- `issue` prints the key once; the server keeps only its hash. Hand out one key
  per person, and send it privately.
- `revoke` refuses a key from now on; the shares it created stay until they
  expire. Use it for a leaked or replaced key.
- `ban` revokes a key and ends every share it created at once. Use it for
  abuse.
- `list --share` finds the key that created a share, for example when someone
  reports one.

Each person saves their key with
`stashvent login --server https://stash.example.com`.

## Behind a reverse proxy

- **TLS.** The proxy terminates HTTPS and forwards to the server's `:8080`.
- **Client addresses.** Rate limits are per client address. Set
  `TRUSTED_PROXIES` to the proxy's addresses, so the server believes the
  `X-Forwarded-For` header they send. Leave it empty without a proxy: then the
  header is ignored, so clients can't fake their address. With a proxy and an
  empty list, every client shares the proxy's address and one limit.
- **Logs.** If the proxy logs request headers, have it redact
  `Authorization` (API keys) and `X-Delete-Token` (delete tokens). The server
  itself never logs them.
- **Caching.** Don't cache anything under `/v1/`: a share's response holds a
  presigned download URL.
- **Health checks.** `/healthz` answers as long as the process runs;
  `/readyz` also checks Postgres and the bucket. Neither is rate-limited.

## Storage

- **Two endpoints.** `S3_ENDPOINT` is how the server reaches storage, which can
  be an internal address. `S3_PUBLIC_ENDPOINT` is the URL every CLI uses, and
  presigned URLs are signed for exactly that scheme, host, and port. If
  anything between the CLI and storage changes the host, uploads and downloads
  fail their signature check. A proxy in front of storage must forward the
  original `Host`.
- **Permissions.** The access key needs to put, get, head, and delete objects,
  **and** permission on the bucket itself (`s3:ListBucket` on AWS). `/readyz`
  checks the bucket with it, and without it storage answers 403 instead of
  404 for a missing object, so the server can't tell "not uploaded yet" from a
  real failure.
- **Keep the bucket private.** No public reads, no CORS: only the CLI uses the
  presigned URLs.
- **A lifecycle rule as a backstop.** The server deletes expired shares itself,
  but a rule that deletes objects older than 8 days (the longest expiry, plus
  margin) cleans up anything a sweep missed.
- **Tested with:** AWS S3, and SeaweedFS (in CI). Other S3-compatible stores
  (Cloudflare R2, Google Cloud Storage with HMAC keys, Backblaze B2) should
  work but aren't verified yet. Azure Blob Storage isn't S3-compatible.

### AWS S3

Both endpoints are the regional one, with virtual-hosted URLs:

```
S3_ENDPOINT=https://s3.us-east-1.amazonaws.com
S3_PUBLIC_ENDPOINT=https://s3.us-east-1.amazonaws.com
S3_REGION=us-east-1
S3_USE_PATH_STYLE=false
```

Give the server its own IAM user (or role) with only this bucket, and keep
the bucket's Block Public Access on:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::stashvent-shares/*"
    },
    {
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::stashvent-shares"
    }
  ]
}
```

`s3:GetObject` also covers reading an object's size, and `s3:ListBucket`
covers checking the bucket. For the lifecycle rule, add one to the bucket
that expires objects 8 days after creation.

## Operating it

- **Expiry.** Every 5 minutes the server deletes expired shares, and shares
  that were created but never uploaded: the object first, then the row.
- **Upgrades.** Pull a newer version tag and restart; migrations run at
  startup. To roll back, use the previous tag, but check the release notes for
  a schema change first.
- **More than one replica** works: they share Postgres and storage. Each
  replica's sweep does the same work, which is safe, and the rate limits are
  counted per replica.
- **Logs** are JSON on stdout, one line per request: request ID, method,
  route, status, latency, a shortened User-Agent, and the share ID when there
  is one. Never tokens, keys, presigned URLs, full request URLs, bodies, or
  client addresses.
- **What the server stores:** sizes, times, the encryption parameters (salt
  and IV, which aren't secret), pull counts, and with keys on, which key
  created which share. Never the password, the code, or repository details
  ([What the server can and can't see](DOCS.md#what-the-server-can-and-cant-see)).

## Common problems

| Symptom | Cause |
|---|---|
| The server exits at startup naming a variable | that value is missing or invalid; the message says which |
| `/readyz` answers 503 | Postgres or the bucket can't be reached with the configured settings; the server's log names the failing check |
| Uploads or downloads fail with HTTP 403 | `S3_PUBLIC_ENDPOINT` doesn't match the URL clients reach storage at, or a proxy changed the `Host` |
| `/readyz` answers 503 while the bucket exists, or pushes fail with an unexpected server error (500) | the storage key may lack permission on the bucket itself, see [Storage](#storage) |
| `refusing insecure http:// server` | the CLI needs `https://`; put the server behind TLS |
| `the server URL must be only a scheme and a host` | the server can't live under a path; give it its own host |
| Every client hits the rate limit at once | a proxy in front and `TRUSTED_PROXIES` empty: all clients share the proxy's address |
