# StashVent dev commands. Run `make help` for the list.
#
# Kept compatible with GNU make 3.81 on Windows: one plain command per recipe.
# Recipes run through sh (Git for Windows provides it), so quote any text
# containing shell characters such as parentheses.

# Load .env (optional) and export its values to every command.
-include .env
export

# On Windows, make runs recipes through Git Bash, which rewrites Unix-looking
# arguments (e.g. docker's `-w /app` becomes `C:/Program Files/Git/app`).
# Turn that off; it has no effect on Linux or macOS.
MSYS_NO_PATHCONV := 1

EXE     := $(shell go env GOEXE)
VERSION ?= dev
# Expanded once: with `export` above, a ?= value would run git again for
# every recipe line.
ifndef COMMIT
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null)
endif
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

GOTOOL             := go tool -modfile=tools/go.mod
GOLANGCI_LINT_IMAGE := golangci/golangci-lint:v2.14.0
# Keep in step with the sqlc version in .github/workflows/ci.yml.
SQLC_IMAGE          := sqlc/sqlc:1.31.1
# Keep in step with the GoReleaser version in .github/workflows/release.yml.
GORELEASER_IMAGE    := goreleaser/goreleaser:v2.18.3

# The server settings the end-to-end tests expect: keys on, small limits the
# tests can reach, and rate limits out of the way (they have unit tests).
# run-server-e2e starts the server with them and test-e2e tells the tests,
# so .env stays as it is. Keep in step with the e2e job in
# .github/workflows/ci.yml.
E2E_SERVER_ENV := REQUIRE_API_KEY=true API_KEY_SHARES_PER_DAY=3 MAX_PULLS_PER_SHARE=2 MAX_STORED_BYTES=0 \
	RATE_LIMIT_CREATE_PER_MINUTE=600 RATE_LIMIT_CREATE_BURST=100 \
	RATE_LIMIT_LOOKUP_PER_MINUTE=600 RATE_LIMIT_LOOKUP_BURST=100 \
	RATE_LIMIT_OTHER_PER_MINUTE=600 RATE_LIMIT_OTHER_BURST=100 \
	RATE_LIMIT_KEY_CREATE_PER_MINUTE=600 RATE_LIMIT_KEY_CREATE_BURST=100
E2E_SERVER ?= http://localhost:8080
# e.g. make test-e2e E2E_FLAGS="-v -run TestQuota"
E2E_FLAGS ?=

# Linux, the race detector, and a newer git (2.47) than the local one, in the
# lint image, which has Go and git. Named volumes keep the Go caches between
# runs. The integration variant reaches the Docker stack on the host.
LINUX_TEST := docker run --rm -v "$(CURDIR):/app" -w /app \
	-v stashvent-gobuild:/root/.cache/go-build -v stashvent-gomod:/go/pkg/mod
LINUX_STACK := --add-host=host.docker.internal:host-gateway \
	-e "DATABASE_URL=postgres://stashvent:stashvent@host.docker.internal:5438/stashvent?sslmode=disable" \
	-e S3_ENDPOINT=http://host.docker.internal:8333 -e S3_PUBLIC_ENDPOINT=http://host.docker.internal:8333

# GoReleaser's image runs goreleaser itself, so there is no shell to trust the
# checkout in; git's environment config does it. Its Go may be older than
# go.mod's, and golang images default to GOTOOLCHAIN=local. The latest CLI tag
# is named for it, so it never takes a server tag (server-v0.1.0, image.yml) as
# the current version; none before the first release.
CLI_TAG := $(shell git describe --tags --abbrev=0 --match "v[0-9]*" 2>/dev/null)
RELEASE_ENV := -e GOTOOLCHAIN=auto \
	-e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/app \
	$(if $(CLI_TAG),-e GORELEASER_CURRENT_TAG=$(CLI_TAG))

.PHONY: help up down logs run-server run-server-e2e build test test-integration test-e2e \
        test-linux test-linux-integration vet fmt lint vuln release-snapshot \
        migrate migrate-down migrate-status migration-new sqlc sqlc-diff

help:
	@echo "Infrastructure:"
	@echo "  make up              start postgres + seaweedfs in Docker"
	@echo "  make down            stop them; data is kept in Docker volumes"
	@echo "  make logs            follow container logs"
	@echo "Server:"
	@echo "  make run-server      run the API server on this machine"
	@echo "  make run-server-e2e  run it with the settings make test-e2e expects"
	@echo "Build and checks:"
	@echo "  make build           build bin/stashvent and bin/stashvent-server"
	@echo "  make test            run tests"
	@echo "  make test-integration  run tests against the Docker stack; needs make up"
	@echo "  make test-e2e        run the built binaries against make run-server-e2e"
	@echo "  make test-linux      run tests with -race and a newer git, in Docker"
	@echo "  make test-linux-integration  the same with the Docker stack tests"
	@echo "  make vet             run go vet"
	@echo "  make fmt             format code"
	@echo "  make lint            run golangci-lint via Docker"
	@echo "  make vuln            scan dependencies for known vulnerabilities"
	@echo "  make release-snapshot  build the release archives into dist/, without publishing"
	@echo "Migrations:"
	@echo "  make migrate         apply all migrations"
	@echo "  make migrate-down    roll back the last migration"
	@echo "  make migrate-status  show migration status"
	@echo "  make migration-new NAME=create_shares   create a new migration"
	@echo "Queries:"
	@echo "  make sqlc            regenerate internal/storage/postgres/db via Docker"
	@echo "  make sqlc-diff       fail if the generated code is out of date"

up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f

run-server:
	go run ./apps/stashvent-server

# The server for make test-e2e; .env supplies the rest of its settings.
run-server-e2e:
	$(E2E_SERVER_ENV) go run ./apps/stashvent-server

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/stashvent$(EXE) ./apps/stashvent
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/stashvent-server$(EXE) ./apps/stashvent-server

test:
	go test ./...

test-integration:
	go test -tags integration ./...

# Needs the server running with the same settings: make run-server-e2e.
test-e2e: build
	$(E2E_SERVER_ENV) STASHVENT_E2E_SERVER=$(E2E_SERVER) go test -tags e2e -count=1 -timeout 10m $(E2E_FLAGS) ./e2e

test-linux:
	$(LINUX_TEST) $(GOLANGCI_LINT_IMAGE) sh -c 'git config --global --add safe.directory /app && git --version && go test -race -timeout 20m ./...'

test-linux-integration:
	$(LINUX_TEST) $(LINUX_STACK) $(GOLANGCI_LINT_IMAGE) sh -c 'git config --global --add safe.directory /app && git --version && go test -race -tags integration -timeout 20m ./...'

vet:
	go vet ./...

fmt:
	go fmt ./...

lint:
	docker run --rm -v "$(CURDIR):/app" -w /app $(GOLANGCI_LINT_IMAGE) golangci-lint run

vuln:
	$(GOTOOL) govulncheck ./...

# The release workflow's build (.goreleaser.yaml), in Docker, without
# publishing anything.
release-snapshot:
	$(LINUX_TEST) $(RELEASE_ENV) $(GORELEASER_IMAGE) release --snapshot --clean

# goose reads GOOSE_DRIVER, GOOSE_DBSTRING and GOOSE_MIGRATION_DIR from .env.
migrate:
	$(GOTOOL) goose up

migrate-down:
	$(GOTOOL) goose down

migrate-status:
	$(GOTOOL) goose status

migration-new:
	$(if $(NAME),,$(error NAME is required, e.g. make migration-new NAME=create_shares))
	$(GOTOOL) goose -s create $(NAME) sql

# sqlc reads sqlc.yaml: the migrations are the schema, queries/*.sql the input.
sqlc:
	docker run --rm -v "$(CURDIR):/src" -w /src $(SQLC_IMAGE) generate

sqlc-diff:
	docker run --rm -v "$(CURDIR):/src" -w /src $(SQLC_IMAGE) diff
