---
name: stashvent-postgres
description: Postgres conventions for StashVent — versioned migrations with goose, type-safe queries with sqlc + pgx, schema naming and type conventions, and safe schema changes. Load whenever creating or editing migrations, writing SQL queries, changing the schema, or touching internal/storage/postgres.
---

# StashVent Postgres Rules

Stack: **goose** (versioned migrations, like Alembic) + **sqlc** (type-safe
Go generated from SQL) + **pgx/v5** (driver, `pgxpool`). No ORM.
Storage boundaries (what may and may not be stored) are defined in the
`stashvent-crypto` skill. Follow them for every column.

## Layout

```
internal/storage/postgres/
  migrations/        # goose SQL files, embedded into the server binary
    00001_create_shares.sql
  queries/           # hand-written SQL, input to sqlc
    shares.sql
  db/                # sqlc output — generated, never edit by hand
  store.go           # ShareStore implementation wrapping db/
sqlc.yaml            # repo root
```

## Migrations (goose)

- Create with sequential numbering: `make migration-new NAME=<name>` (runs
  `goose -s create <name> sql` via `go tool -modfile=tools/go.mod`; goose is not
  installed globally). Apply with `make migrate`, roll back with `make migrate-down`.
  Names are short and imperative: `create_shares`, `add_shares_status_index`.
- Each file has both sections, and every Up has a working Down:

```sql
-- +goose Up
...
-- +goose Down
...
```

- **Never edit a migration that has been applied** anywhere, even locally
  shared. Write a new migration instead. The one exception is a change to
  `--` comments only: goose records only version numbers and sqlc ignores
  comments, so the applied schema stays the same. Say so in the commit.
- One logical change per migration.
- Goose wraps each migration in a transaction. Statements that can't run
  in one (e.g. `CREATE INDEX CONCURRENTLY`) need `-- +goose NO TRANSACTION`
  and must be the only statement in that file.
- Migrations are embedded with `embed.FS` and applied by the server on
  startup via goose's `Provider` with a Postgres session locker (advisory
  lock), so multiple server replicas can't migrate at the same time.
- Local commands use env vars: `GOOSE_DRIVER=postgres`,
  `GOOSE_DBSTRING=<dsn>`, `GOOSE_MIGRATION_DIR=internal/storage/postgres/migrations`.
- CI runs: up to latest → down to 0 → up to latest, against a real Postgres.
  This proves every Down works.

## Schema conventions

**Naming**
- `snake_case` everywhere. Tables are plural (`shares`), columns singular.
- Name every constraint and index explicitly; never rely on generated names:
  - primary key: `pk_<table>`
  - unique: `uq_<table>_<columns>`
  - foreign key: `fk_<table>_<ref_table>`
  - check: `ck_<table>_<column>`
  - index: `idx_<table>_<columns>`

**Keys**
- Primary key: `id bigint GENERATED ALWAYS AS IDENTITY`. Internal only,
  never exposed in APIs or URLs.
- Public identifiers (e.g. `share_id`) are separate `text` columns with a
  unique constraint.

**Types**
- Timestamps: always `timestamptz`, never `timestamp`. Store UTC.
- Strings: `text`, never `varchar(n)`. Enforce lengths with `CHECK` if needed.
- Binary values (salt, iv, hashes): `bytea`, never hex/base64 text.
- Status-like values: `text` + `CHECK (col IN (...))`, not Postgres `ENUM`
  types (enums are awkward to change in migrations).
- Sizes/counts that can exceed 2^31: `bigint`.

**Defaults**
- Columns are `NOT NULL` unless null has a real meaning.
- `created_at timestamptz NOT NULL DEFAULT now()` on every table;
  `updated_at` only on tables whose rows are updated.
- Add an index for every column used in a frequent `WHERE`, e.g.
  `expires_at` for the expiration sweep.

### Example: first migration

```sql
-- +goose Up
CREATE TABLE shares (
    id                 bigint      GENERATED ALWAYS AS IDENTITY,
    share_id           text        NOT NULL,
    storage_key        text        NOT NULL,
    status             text        NOT NULL DEFAULT 'pending',
    size               bigint      NOT NULL,
    salt               bytea       NOT NULL,
    iv                 bytea       NOT NULL,
    delete_token_hash  bytea       NOT NULL,
    payload_format     text        NOT NULL,
    packaging_method   text        NOT NULL,
    bundle_version     integer     NOT NULL,
    object_format      text        NOT NULL,
    sender_git_version text        NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    CONSTRAINT pk_shares PRIMARY KEY (id),
    CONSTRAINT uq_shares_share_id UNIQUE (share_id),
    CONSTRAINT uq_shares_storage_key UNIQUE (storage_key),
    CONSTRAINT ck_shares_status CHECK (status IN ('pending', 'active')),
    CONSTRAINT ck_shares_size CHECK (size >= 0),
    CONSTRAINT ck_shares_expires_after_created CHECK (expires_at > created_at)
);

CREATE INDEX idx_shares_expires_at ON shares (expires_at);

-- +goose Down
DROP TABLE shares;
```

## Changing the schema safely

Assume old and new server versions may run at the same time during a deploy.

- **Add a column:** nullable, or `NOT NULL` with a `DEFAULT`. Never add a
  `NOT NULL` column without a default to a table that has rows.
- **Rename or drop a column:** use expand/contract across releases. Add the
  new column, write to both, backfill, switch reads, then drop the old one
  in a later migration. Never rename or drop in the same release that stops
  using it.
- **Add an index** to a large table: `CREATE INDEX CONCURRENTLY` in its own
  `NO TRANSACTION` migration.
- Data backfills go in their own migration, separate from schema changes.
- `api_keys` is a contract with the closed hosted service, which writes rows
  into it: change it like the `/v1` API, additively. It never gets
  a user or owner column; the service keeps those in its own schema.

## Queries (sqlc + pgx)

- All SQL lives in `queries/*.sql` with sqlc annotations:

```sql
-- name: GetActiveShare :one
SELECT * FROM shares
WHERE share_id = $1 AND status = 'active' AND expires_at > now();
```

- Parameters only (`$1`). Never build SQL with string concatenation or
  `fmt.Sprintf`.
- Run `sqlc generate` after changing queries or migrations; commit the
  generated `db/` code. CI fails if `sqlc diff` shows it's out of date.
- Every query takes a `context.Context`. Use `pgxpool.Pool`, never a single
  connection shared across goroutines.
- Multi-statement operations use a transaction (`pool.BeginTx`), with
  `defer tx.Rollback(ctx)` followed by an explicit `tx.Commit(ctx)`.
- Map `pgx.ErrNoRows` to a domain sentinel (e.g. `ErrShareNotFound`) in the
  store layer. Callers never see pgx errors.

## Keep the database behind an interface

The rest of the server depends on a small interface, not on sqlc or pgx:

```go
type ShareStore interface {
    CreateShare(ctx context.Context, s NewShare) error
    CompleteShare(ctx context.Context, shareID string, size int64) error
    GetActiveShare(ctx context.Context, shareID string) (Share, error)
    DeleteShare(ctx context.Context, shareID string) error
    ListExpired(ctx context.Context, before time.Time, limit int) ([]Share, error)
}
```

Swapping sqlc for another query layer (e.g. GORM) later then only touches
`store.go`. If GORM is ever adopted, goose still owns the schema. Never
enable GORM `AutoMigrate`.

## Testing

- Store tests run against a real Postgres (the Docker Compose instance or
  testcontainers), never mocks or SQLite.
- Each test gets a clean state: its own schema or a truncate in `t.Cleanup`.
- Cover the constraints: duplicate `share_id` fails, invalid `status` fails,
  expired shares are not returned by `GetActiveShare`.
- The server's tests on the Docker stack use the development database
  itself and remove what they create (`newIntegrationServer`). A test that
  counts or sweeps over a whole table uses `newIsolatedServer`, which has a
  schema of its own, dropped when the test ends: on the shared database
  someone else's share expiring would change its result, and a sweep would
  remove other people's shares (`TestSweepOnDockerStack`,
  `TestStorageCapOnDockerStack`). `TestNoRepoDetailsOnServer` only reads, so
  it reads the real tables, every row of every one.
