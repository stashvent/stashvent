//go:build integration

// Run against the Docker stack: make up && make test-integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stashvent/stashvent/internal/storage"
)

const defaultDatabaseURL = "postgres://stashvent:stashvent@localhost:5438/stashvent?sslmode=disable" //nolint:gosec // G101: the Docker stack's local dev login (docker-compose.yml), not a secret

// newTestPool returns a pool whose connections use a fresh, migrated schema,
// so tests never touch the shares in the public schema. The schema is dropped
// when the test ends.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDatabaseURL
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		pool.Close()
		t.Fatalf("creating schema (is the Docker stack up?): %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
		pool.Close()
	})

	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newShare(id string, expiresIn time.Duration) storage.NewShare {
	return storage.NewShare{
		ShareID:          id,
		StorageKey:       "stashes/" + id,
		Size:             100,
		Salt:             []byte("0123456789abcdef"),
		IV:               []byte("0123456789ab"),
		DeleteTokenHash:  []byte("delete-token-hash"),
		PayloadFormat:    "stashvent-v1",
		PackagingMethod:  "stash-ref",
		BundleVersion:    2,
		ObjectFormat:     "sha1",
		SenderGitVersion: "2.32.0",
		ExpiresAt:        time.Now().Add(expiresIn).UTC().Truncate(time.Microsecond),
	}
}

// insertAged creates a share with an explicit status, age, and time left
// (negative: already expired), for states the store can't produce directly.
// Both are applied to the database's now(), the clock the queries compare
// against, so the tests don't depend on this machine's clock matching it.
func insertAged(t *testing.T, pool *pgxpool.Pool, id, status string, age, expiresIn time.Duration) {
	t.Helper()
	n := newShare(id, time.Hour)
	_, err := pool.Exec(t.Context(), `
		INSERT INTO shares (
			share_id, storage_key, status, size, salt, iv, delete_token_hash,
			payload_format, packaging_method, bundle_version, object_format,
			sender_git_version, created_at, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			now() - $13::interval, now() + $14::interval)`,
		n.ShareID, n.StorageKey, status, n.Size, n.Salt, n.IV, n.DeleteTokenHash,
		n.PayloadFormat, n.PackagingMethod, n.BundleVersion, n.ObjectFormat,
		n.SenderGitVersion, age, expiresIn)
	if err != nil {
		t.Fatalf("inserting %s: %v", id, err)
	}
}

func wantConstraint(t *testing.T, err error, name string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != name {
		t.Errorf("err = %v, want a violation of %s", err, name)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	first, err := Migrate(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Migrate(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 || first != second {
		t.Errorf("schema versions = %d then %d, want the same non-zero version", first, second)
	}
}

// The storage boundary (stashvent-crypto skill): Postgres holds operational
// metadata only. A new column fails this test until it is added here on
// purpose, which is the moment to check it isn't plaintext, ciphertext, a
// password, a raw delete token or API key, a repo detail (name, branch, base
// commit), or a user identity (api_keys has no owner: accounts belong to the
// hosted service).
func TestSharesColumnsAreAllowlisted(t *testing.T) {
	pool := newTestPool(t)
	tables := map[string][]string{
		"shares": {
			"api_key_id", "bundle_version", "created_at", "delete_token_hash", "expires_at", "id", "iv",
			"lookups", "object_format", "packaging_method", "payload_format", "salt", "sender_git_version",
			"share_id", "size", "status", "storage_key", "updated_at",
		},
		"api_keys": {
			"banned_at", "created_at", "id", "key_hash", "label", "revoked_at", "updated_at",
		},
		"api_key_usage": {
			"api_key_id", "bytes", "created_at", "day", "id", "shares", "updated_at",
		},
	}
	for table, want := range tables {
		rows, err := pool.Query(t.Context(), `
			SELECT column_name::text FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1
			ORDER BY column_name`, table)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s columns:\n got %v\nwant %v", table, got, want)
		}
	}
}

func TestCreateAndGetShare(t *testing.T) {
	ctx := t.Context()
	store := New(newTestPool(t))
	n := newShare("share-one", time.Hour)
	if err := store.CreateShare(ctx, n, storage.Quota{}); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetShare(ctx, n.ShareID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != storage.StatusPending || got.CreatedAt.IsZero() {
		t.Errorf("status = %q, created_at = %v, want pending and a timestamp", got.Status, got.CreatedAt)
	}
	if !got.ExpiresAt.Equal(n.ExpiresAt) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, n.ExpiresAt)
	}
	got.ExpiresAt = n.ExpiresAt // same instant, possibly another location
	if got.ShareID != n.ShareID || got.StorageKey != n.StorageKey || got.Size != n.Size ||
		string(got.Salt) != string(n.Salt) || string(got.IV) != string(n.IV) ||
		string(got.DeleteTokenHash) != string(n.DeleteTokenHash) ||
		got.PayloadFormat != n.PayloadFormat || got.PackagingMethod != n.PackagingMethod ||
		got.BundleVersion != n.BundleVersion || got.ObjectFormat != n.ObjectFormat ||
		got.SenderGitVersion != n.SenderGitVersion {
		t.Errorf("round trip:\n got %+v\nwant %+v", got.NewShare, n)
	}

	t.Run("pending share is not active", func(t *testing.T) {
		if _, err := store.GetActiveShare(ctx, n.ShareID); !errors.Is(err, storage.ErrShareNotFound) {
			t.Errorf("err = %v, want ErrShareNotFound", err)
		}
	})
	t.Run("unknown share", func(t *testing.T) {
		if _, err := store.GetShare(ctx, "missing"); !errors.Is(err, storage.ErrShareNotFound) {
			t.Errorf("err = %v, want ErrShareNotFound", err)
		}
	})
}

func TestConstraints(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)
	if err := store.CreateShare(ctx, newShare("dup", time.Hour), storage.Quota{}); err != nil {
		t.Fatal(err)
	}

	t.Run("duplicate share_id", func(t *testing.T) {
		n := newShare("dup", time.Hour)
		n.StorageKey = "stashes/other"
		wantConstraint(t, store.CreateShare(ctx, n, storage.Quota{}), "uq_shares_share_id")
	})
	t.Run("duplicate storage_key", func(t *testing.T) {
		n := newShare("other", time.Hour)
		n.StorageKey = "stashes/dup"
		wantConstraint(t, store.CreateShare(ctx, n, storage.Quota{}), "uq_shares_storage_key")
	})
	t.Run("negative size", func(t *testing.T) {
		n := newShare("negative", time.Hour)
		n.Size = -1
		wantConstraint(t, store.CreateShare(ctx, n, storage.Quota{}), "ck_shares_size")
	})
	t.Run("expiry before creation", func(t *testing.T) {
		wantConstraint(t, store.CreateShare(ctx, newShare("backwards", -time.Hour), storage.Quota{}), "ck_shares_expires_after_created")
	})
	t.Run("invalid status", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE shares SET status = 'deleted' WHERE share_id = 'dup'`)
		wantConstraint(t, err, "ck_shares_status")
	})
}

func TestActivateShare(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)

	if err := store.CreateShare(ctx, newShare("fresh", time.Hour), storage.Quota{}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateShare(ctx, "fresh", storage.PendingTTL); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetActiveShare(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != storage.StatusActive {
		t.Errorf("status = %q, want active", got.Status)
	}

	insertAged(t, pool, "almost-stale", storage.StatusPending, storage.PendingTTL-time.Minute, time.Hour)
	if err := store.ActivateShare(ctx, "almost-stale", storage.PendingTTL); err != nil {
		t.Errorf("share pending for just under PendingTTL: %v", err)
	}

	insertAged(t, pool, "stale", storage.StatusPending, storage.PendingTTL, time.Hour)
	insertAged(t, pool, "expired", storage.StatusPending, time.Minute, -time.Second)

	for name, id := range map[string]string{
		"already active":    "fresh",
		"abandoned pending": "stale",
		"expired pending":   "expired",
		"unknown":           "missing",
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.ActivateShare(ctx, id, storage.PendingTTL); !errors.Is(err, storage.ErrShareNotFound) {
				t.Errorf("err = %v, want ErrShareNotFound", err)
			}
		})
	}
}

func TestExpiredShareIsNotReturned(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)
	insertAged(t, pool, "expired", storage.StatusActive, 2*time.Hour, -time.Second)

	if _, err := store.GetShare(ctx, "expired"); !errors.Is(err, storage.ErrShareNotFound) {
		t.Errorf("GetShare err = %v, want ErrShareNotFound", err)
	}
	if _, err := store.GetActiveShare(ctx, "expired"); !errors.Is(err, storage.ErrShareNotFound) {
		t.Errorf("GetActiveShare err = %v, want ErrShareNotFound", err)
	}
}

func TestListExpiredAndDelete(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)
	const pendingAge = 90 * time.Minute

	insertAged(t, pool, "expired-active", storage.StatusActive, 3*time.Hour, -time.Second)
	insertAged(t, pool, "live-active", storage.StatusActive, 3*time.Hour, time.Hour)
	insertAged(t, pool, "stale-pending", storage.StatusPending, pendingAge, time.Hour)
	insertAged(t, pool, "younger-pending", storage.StatusPending, pendingAge-time.Minute, time.Hour)
	insertAged(t, pool, "expired-pending", storage.StatusPending, time.Minute, -time.Second)

	list := func(afterID int64, limit int) []storage.ExpiredShare {
		t.Helper()
		shares, err := store.ListExpired(ctx, 0, pendingAge, afterID, limit)
		if err != nil {
			t.Fatal(err)
		}
		return shares
	}
	ids := func(shares []storage.ExpiredShare) []string {
		out := make([]string, len(shares))
		for i, s := range shares {
			out[i] = s.ShareID
		}
		return out
	}

	// Oldest row first.
	all := list(0, 10)
	if got, want := ids(all), []string{"expired-active", "stale-pending", "expired-pending"}; !slices.Equal(got, want) {
		t.Fatalf("ListExpired = %v, want %v", got, want)
	}
	if all[1].StorageKey != "stashes/stale-pending" {
		t.Errorf("storage key = %q, want %q", all[1].StorageKey, "stashes/stale-pending")
	}

	// Paging: a limit, then the rest after the last ID of the first page.
	first := list(0, 2)
	if got, want := ids(first), []string{"expired-active", "stale-pending"}; !slices.Equal(got, want) {
		t.Fatalf("first page = %v, want %v", got, want)
	}
	if got, want := ids(list(first[1].ID, 2)), []string{"expired-pending"}; !slices.Equal(got, want) {
		t.Errorf("second page = %v, want %v", got, want)
	}

	// DeleteShare removes shares that GetShare no longer returns.
	if err := store.DeleteShare(ctx, "expired-active"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteShare(ctx, "expired-active"); !errors.Is(err, storage.ErrShareNotFound) {
		t.Errorf("second delete err = %v, want ErrShareNotFound", err)
	}
	if got, want := ids(list(0, 10)), []string{"stale-pending", "expired-pending"}; !slices.Equal(got, want) {
		t.Errorf("ListExpired after delete = %v, want %v", got, want)
	}
}

func TestRevokeShare(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)
	const pendingAge = 90 * time.Minute

	if err := store.CreateShare(ctx, newShare("pending", time.Hour), storage.Quota{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateShare(ctx, newShare("active", time.Hour), storage.Quota{}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateShare(ctx, "active", storage.PendingTTL); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateShare(ctx, newShare("untouched", time.Hour), storage.Quota{}); err != nil {
		t.Fatal(err)
	}
	insertAged(t, pool, "expired", storage.StatusActive, 2*time.Hour, -time.Second)

	for _, id := range []string{"pending", "active"} {
		if err := store.RevokeShare(ctx, id); err != nil {
			t.Fatalf("revoking %s: %v", id, err)
		}
		// Gone for every lookup at once.
		if _, err := store.GetShare(ctx, id); !errors.Is(err, storage.ErrShareNotFound) {
			t.Errorf("GetShare(%s) after revoke: err = %v, want ErrShareNotFound", id, err)
		}
		if _, err := store.GetActiveShare(ctx, id); !errors.Is(err, storage.ErrShareNotFound) {
			t.Errorf("GetActiveShare(%s) after revoke: err = %v, want ErrShareNotFound", id, err)
		}
		if err := store.ActivateShare(ctx, id, storage.PendingTTL); !errors.Is(err, storage.ErrShareNotFound) {
			t.Errorf("ActivateShare(%s) after revoke: err = %v, want ErrShareNotFound", id, err)
		}
	}

	for name, id := range map[string]string{
		"already revoked": "active",
		"expired":         "expired",
		"unknown":         "missing",
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.RevokeShare(ctx, id); !errors.Is(err, storage.ErrShareNotFound) {
				t.Errorf("err = %v, want ErrShareNotFound", err)
			}
		})
	}

	if _, err := store.GetShare(ctx, "untouched"); err != nil {
		t.Errorf("another share was affected: %v", err)
	}

	// The rows are still there, for the sweep to find once they are old enough.
	listed := func(minAge time.Duration) []string {
		t.Helper()
		shares, err := store.ListExpired(ctx, minAge, pendingAge, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(shares))
		for i, s := range shares {
			out[i] = s.ShareID
		}
		return out
	}
	if got, want := listed(0), []string{"pending", "active", "expired"}; !slices.Equal(got, want) {
		t.Errorf("ListExpired with no minimum age = %v, want %v", got, want)
	}
	// Just revoked, so their upload URLs may still work: only the old one is due.
	if got, want := listed(30*time.Minute), []string{"expired"}; !slices.Equal(got, want) {
		t.Errorf("ListExpired with a 30 minute minimum age = %v, want %v", got, want)
	}
}

// The minimum age applies whatever made a share due.
func TestListExpiredMinAge(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)
	const minAge, pendingAge = 30 * time.Minute, 20 * time.Minute

	insertAged(t, pool, "expired-old", storage.StatusActive, minAge, -time.Second)
	insertAged(t, pool, "expired-young", storage.StatusActive, minAge-time.Minute, -time.Second)
	insertAged(t, pool, "pending-old", storage.StatusPending, minAge, time.Hour)
	insertAged(t, pool, "pending-young", storage.StatusPending, minAge-time.Minute, time.Hour)

	shares, err := store.ListExpired(ctx, minAge, pendingAge, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range shares {
		got = append(got, s.ShareID)
	}
	if want := []string{"expired-old", "pending-old"}; !slices.Equal(got, want) {
		t.Errorf("ListExpired = %v, want %v", got, want)
	}
}
