# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

StashVent (`stashvent` CLI) shares unfinished Git work: a stash is packaged as a Git
bundle, encrypted client-side with a password, uploaded to S3-compatible storage, and
imported into the recipient's `git stash list`. The server never sees plaintext or the
password.

- `DOCS.md` (using the CLI), `SETUP.md` (developing), `CONTRIBUTING.md` (the checks
  and rules for a pull request), and `RELEASES.md` (downloads and cutting a release)
  are the human-facing docs; update them when commands, flags, limits, make targets,
  or the release pipeline change.

## Commands

All tooling goes through the Makefile (`make help`). Dev tools are **not** installed
globally: goose and govulncheck run via `go tool -modfile=tools/go.mod`, golangci-lint
runs via Docker. go-licenses (also in `tools/go.mod`) collects the dependencies'
licenses for the release archives and the server image; both builds compile it with
`go build -modfile=tools/go.mod` first, because with `GOOS` set `go tool` would
cross-compile the tool itself.

```
make up / make down          # Postgres + SeaweedFS in Docker
make run-server              # API server on the host (loads .env)
make build                   # bin/stashvent, bin/stashvent-server
make test / vet / lint / vuln
make test-integration        # -tags integration, against the Docker stack (make up first)
make run-server-e2e          # the server with keys on and the e2e limits (E2E_SERVER_ENV)
make test-e2e                # built binaries as processes, against make run-server-e2e
make test-linux              # -race and git 2.47 in the golangci-lint container
make test-linux-integration  # the same with -tags integration, stack via host.docker.internal
make release-snapshot        # GoReleaser (Docker) builds the CLI release archives into dist/, no publish
make migrate / migrate-down / migrate-status
make migration-new NAME=create_shares
make sqlc / sqlc-diff        # regenerate internal/storage/postgres/db (Docker); diff fails if stale
```

The server applies embedded migrations itself on startup (`postgres.Migrate`), so
`make migrate` is only needed for rolling back or checking status. After changing a
migration or `queries/*.sql`, run `make sqlc` and commit the generated `db/` code.

Single test: `go test ./apps/stashvent-server -run TestHealthz` (subtest: `-run 'TestHealthz/GET'`).

Windows specifics: `make` runs recipes through Git's `sh` even from PowerShell, so quote
Makefile text containing shell characters, and keep `MSYS_NO_PATHCONV=1` (otherwise Git
Bash rewrites container paths like `-w /app`).

## Layout and architecture

- `apps/stashvent` → `stashvent` binary; `apps/stashvent-server` → API server. Each
  folder is named after its binary, because `go install` names the binary after the
  last folder of its path. The website (landing page, share page) is hosted-only and
  lives in the closed repo, not here.
- `internal/` is shared by CLI and server. `internal/api` holds the request/response
  types both sides import, so the `/v1` contract can't drift.
- `apikey/` (repo root, deliberately outside `internal/`) generates, checks, and hashes
  senders' API keys (`sv_…`). The closed hosted service imports it to issue
  keys the server accepts, so its format and hash are a contract like `/v1`.
- `internal/userfiles` owns the CLI's per-user files in `os.UserConfigDir()/stashvent/`:
  `history.json` (shares sent from this machine, with their delete tokens) and
  `config.json` (one API key per server, from `stashvent login`); both layouts are frozen
  at v0.1. The CLI's server is `--server`, then `STASHVENT_SERVER`, then the hosted
  `https://stashvent.dev`; for local development set `STASHVENT_SERVER=http://localhost:8080`.
- `install.sh` and `install.ps1` (repo root, ASCII only) install the latest CLI release
  from GitHub, served from the raw `main` URL. They build URLs from `.goreleaser.yaml`'s
  archive names and `checksums.txt`, so change them together, and install the release
  GitHub marks latest, which `release.yml` gives only to the highest final version.
- `stashvent-server` with no arguments serves; `stashvent-server keys …` are the operator
  commands (issue, list, revoke, ban, usage) for `REQUIRE_API_KEY` instances.
- CLI output is pinned by golden files in `apps/stashvent/cmd/testdata/`; after a
  deliberate change, `go test ./apps/stashvent/cmd -update` rewrites them.
- `e2e/` (build tag `e2e`) runs the built binaries against a running server with keys
  on; each "machine" is a temp dir set as its home, config, and temp directory.
  Lint covers the `integration` and `e2e` tags (`.golangci.yml`).
- Two Go modules: `go.mod` (only what our code imports) and `tools/go.mod` (dev tools,
  kept separate because goose's CLI pulls in every database driver).
- `.dockerignore` is an allowlist: the server image's build context holds only the
  paths listed there. A new top-level path the server needs (a package outside
  `internal/`, a file it embeds) must be added, or the image build fails.

Push/pull flow: CLI scans the stash diff for secrets, bundles and encrypts it
with a manifest → `POST /v1/shares` creates a `pending` row and returns a presigned PUT
URL + delete token → CLI uploads directly to object storage → `POST /v1/shares/{id}/complete`.
Pull: `GET /v1/shares/{id}` returns salt/iv + presigned GET URL → CLI downloads, decrypts,
verifies the repo from the manifest, `git bundle verify`, `git stash store`.
Share summary: `GET /v1/shares/{id}/summary` (size, created, expiry; doesn't count a pull,
signs nothing) is what the hosted share page reads, never `GET /v1/shares/{id}`.

Invariants that span packages:
- Postgres holds operational metadata only (salt, iv, size, expiry, format fields, delete
  token hash, API key hashes, per-key usage, pull counts). Repo name, branch, and base
  commit live in the encrypted manifest. No user columns: accounts are the hosted
  service's.
- An API key is sent only on push and delete, never to a link's host: a key from
  `config.json` only to the server it is stored for, `STASHVENT_API_KEY` (which wins) to
  the server chosen for the command. With `REQUIRE_API_KEY` off the server
  ignores `Authorization`.
- Blobs never pass through the API server; it only issues presigned URLs.
- Storage has two endpoints: internal (server → storage, `seaweedfs:8333` in Docker) and
  public (`S3_PUBLIC_ENDPOINT`, used to sign presigned URLs the CLI can reach).
- The server reads configuration from environment variables only; it never reads `.env`.

## Local infrastructure

`docker-compose.yml`: Postgres 18 (host port 5438), SeaweedFS S3 on 8333 (MinIO's
open-source repo is unmaintained, so it's not used), a one-shot `createbucket`
container, and the containerized server behind `--profile server`. In dev the server
runs on the host. `.env` is optional except for goose, which needs the `GOOSE_*` values.

## Project skills

Each area has a skill under `.claude/skills/` with binding rules; follow it when working
there: `stashvent-crypto` (keys, encryption, what may be stored where), `stashvent-git`
(running git, bundles, temp refs/files), `stashvent-postgres` (goose + sqlc + pgx,
schema conventions), `stashvent-api` (`/v1` contract, error codes, logging rules),
`stashvent-cli` (commands, output, prompts, config files), `golang-idioms` (general Go).
The `golang-developer` agent preloads all of them.
