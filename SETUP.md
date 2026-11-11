# Developing StashVent

How to build, run, and test StashVent on your own machine. For using the CLI,
see [DOCS.md](DOCS.md); for cutting a release, [RELEASES.md](RELEASES.md).

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | the one in `go.mod` (1.27.4) | An older Go 1.21+ downloads it by itself (`GOTOOLCHAIN=auto`, the default) |
| Git | 2.32 or newer | CI uses a much newer one; `make test-linux` runs Git 2.47 |
| Docker | with Compose v2 | Postgres, SeaweedFS, and the Docker-based tools |
| GNU make | 3.81 or newer | The Makefile sticks to 3.81, the version macOS ships |

Nothing else needs installing. goose, govulncheck, and go-licenses (used by
the release build and the server image, which compile it from the same module)
run through `go tool -modfile=tools/go.mod`; golangci-lint, sqlc, and GoReleaser run in
Docker at the versions pinned in the `Makefile`. How to get these on each
system: [Windows](#windows), [macOS](#macos), [Linux](#linux).

## First run

```
cp .env.example .env     # make loads it; the values match docker-compose.yml
make up                  # Postgres on :5438, SeaweedFS S3 on :8333, the bucket
make run-server          # the API server on :8080; applies migrations on start
```

The server reads its settings from environment variables only and never
reads `.env` itself. `make` loads `.env` and exports it, which is why the
server is started through `make run-server`. Check it's up:

```
curl http://localhost:8080/readyz
```

Then build the binaries and share a stash between two clones. The CLI's default
server is the hosted service (`https://stashvent.dev`), so point it at yours
first; `push` then says `Server: http://localhost:8080 (from STASHVENT_SERVER)`:

```
export STASHVENT_SERVER=http://localhost:8080      # macOS, Linux, Git Bash
$env:STASHVENT_SERVER = "http://localhost:8080"    # PowerShell

make build               # bin/stashvent, bin/stashvent-server
cd /path/to/some-repo && git stash push -m "try it" && /path/to/bin/stashvent push
cd /path/to/another-clone && /path/to/bin/stashvent pull <link>
```

Forgetting it sends your test pushes to the hosted service, which refuses them
without a key.

`make down` stops the containers; the data stays in Docker volumes.

### API keys locally

Keys are off by default (`REQUIRE_API_KEY=false`). To try them, set
`REQUIRE_API_KEY=true` in `.env`, restart the server, and issue a key with the
operator commands. They read `DATABASE_URL`, and `keys ban` the `S3_*` settings
too:

```
set -a; . ./.env; set +a                      # export .env's values (bash, Git Bash)
bin/stashvent-server keys issue --label me    # prints the key once
bin/stashvent login                           # paste it
bin/stashvent-server keys list
```

### The server in a container

The image CI tests and `image.yml` publishes is built from
`deploy/docker/server.Dockerfile`:

```
docker compose --profile server up -d --build server
```

## Checks

| Command | What it runs |
|---|---|
| `make test` | Unit tests, the CLI golden files included |
| `make vet` / `make fmt` | `go vet`, `go fmt` |
| `make lint` | golangci-lint in Docker, with the `integration` and `e2e` tags |
| `make vuln` | govulncheck over the dependencies and the Go version |
| `make test-integration` | `-tags integration` against the Docker stack; needs `make up` |
| `make test-e2e` | The built binaries as separate processes against `make run-server-e2e` |
| `make test-linux` | Linux, `-race`, and Git 2.47, in Docker |
| `make test-linux-integration` | The same with the integration tests |
| `make release-snapshot` | The release build into `dist/`, without publishing (see [RELEASES.md](RELEASES.md)) |

Run a single test with `go test`:

```
go test ./apps/stashvent-server -run TestHealthz
go test ./apps/stashvent-server -run 'TestHealthz/GET'
```

### End-to-end tests

They need the server running with keys on and small limits, which
`make run-server-e2e` sets without touching `.env`. Use two terminals:

```
make run-server-e2e                 # terminal 1
make test-e2e                       # terminal 2
make test-e2e E2E_FLAGS="-v -run TestQuota"
```

Each test "machine" is a temp folder used as its home, config, and temp
directory, so your own config and history are never touched.

### CLI output

What the CLI prints is pinned by golden files in `apps/stashvent/cmd/testdata/`.
After a deliberate change, rewrite them and read the diff:

```
go test ./apps/stashvent/cmd -update
git diff apps/stashvent/cmd/testdata
```

## Database

The server applies the embedded migrations itself on startup, so
`make migrate` is only for checking or rolling back. Changing the schema:

```
make migration-new NAME=add_something    # internal/storage/postgres/migrations/
make sqlc                                # regenerate internal/storage/postgres/db/
make sqlc-diff                           # fails if the generated code is stale
make migrate-status / migrate-down
```

Commit the generated `db/` code with the migration or the `queries/*.sql`
change. goose needs the `GOOSE_*` values from `.env`.

## Platform notes

### Windows

- Install [Git for Windows](https://git-scm.com/download/win), Go, and
  [Docker Desktop](https://www.docker.com/products/docker-desktop/) (WSL 2
  backend). GNU make comes from a package manager, e.g. `choco install make`
  or `scoop install make`.
- `make` runs recipes through Git for Windows' `sh`, even from PowerShell, so
  Git's `bin` folder must be on `PATH`. Quote Makefile text that contains shell
  characters.
- The Makefile sets `MSYS_NO_PATHCONV=1`; without it Git Bash rewrites container
  paths like `-w /app`.
- Commands in this file are written for a POSIX shell. In PowerShell, set
  variables with `$env:NAME = "value"` on a line of its own, and run the
  `set -a; . ./.env` line in Git Bash.
- Ctrl+C is tested automatically on Unix only; on Windows it's a manual check.

### macOS

- `xcode-select --install` gives Git and GNU make 3.81, enough for the
  Makefile. Install Go from [go.dev/dl](https://go.dev/dl/) or with
  `brew install go`.
- Docker: [Docker Desktop](https://www.docker.com/products/docker-desktop/), or
  a drop-in such as OrbStack or Colima, as long as `docker compose` works.
- Apple's Git is older than Git's own releases, but above 2.32. For the newest
  Git, `brew install git`; `make test-linux` runs Git 2.47 regardless.

### Linux

- Install Go from [go.dev/dl](https://go.dev/dl/) (distribution packages are
  often too old; `GOTOOLCHAIN=auto` then fetches the right one), plus `git` and
  `make` from the distribution (`build-essential` on Debian and Ubuntu).
- Docker Engine with the Compose plugin (`docker compose`, not `docker-compose`).
  To run it without `sudo`, add yourself to the `docker` group and log in again.
- Containers run as root, so files they write into the checkout are owned by
  root: `make sqlc` (the generated `db/` code) and `make release-snapshot`
  (`dist/`). Take them back with `sudo chown -R "$USER" internal/storage/postgres/db dist`.
- `make test-linux-integration` reaches the Docker stack through
  `host.docker.internal`, which needs Docker 20.10 or newer.

## CI

`.github/workflows/ci.yml` runs on every pull request and push to `main`:

| Job | What it checks |
|---|---|
| Test | build, vet, and tests on Linux (with `-race`), Windows, and macOS |
| lint | golangci-lint |
| Database | migrations up, down, and up again; `sqlc diff`; the store tests |
| Integration | the server and storage tests against the Docker Compose stack |
| End to end | the built binaries against the server image |
| vuln | govulncheck |

`release.yml` publishes the CLI on its version tags (`v…`), and `image.yml` the server
image on its own tags (`server-v…`) or by hand between releases; both are
described in [RELEASES.md](RELEASES.md).

Dependabot (`.github/dependabot.yml`) opens grouped update PRs once a month for
both Go modules, the GitHub Actions, the server image's base images, and the
Compose images; security updates arrive as soon as an advisory is published. The
images pinned in the `Makefile` (golangci-lint, sqlc, GoReleaser) are updated by
hand.

## Where things are decided

- `.claude/skills/` holds the binding rules per area: crypto, Git, Postgres,
  the `/v1` API, the CLI, and general Go.
