//go:build integration

// Run against the Docker stack: make up && make test-integration

package postgres

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/storage"
)

func newKey(t *testing.T, store *Store, label string) storage.APIKey {
	t.Helper()
	key, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	k, err := store.CreateAPIKey(t.Context(), apikey.Hash(key), label)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// keyShare is a share created with a key.
func keyShare(id string, keyID int64) storage.NewShare {
	n := newShare(id, time.Hour)
	n.APIKeyID = keyID
	return n
}

var testDay = time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

func TestAPIKeys(t *testing.T) {
	ctx := t.Context()
	store := New(newTestPool(t))

	key, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateAPIKey(ctx, apikey.Hash(key), "Alice laptop")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.Label != "Alice laptop" || created.CreatedAt.IsZero() ||
		created.RevokedAt != nil || created.BannedAt != nil {
		t.Errorf("created = %+v", created)
	}
	got, err := store.GetActiveAPIKey(ctx, apikey.Hash(key))
	if err != nil || got.ID != created.ID {
		t.Fatalf("GetActiveAPIKey = %+v, %v; want key %d", got, err, created.ID)
	}

	t.Run("constraints", func(t *testing.T) {
		_, err := store.CreateAPIKey(ctx, apikey.Hash(key), "again")
		wantConstraint(t, err, "uq_api_keys_key_hash")
		_, err = store.CreateAPIKey(ctx, []byte("short"), "short hash")
		wantConstraint(t, err, "ck_api_keys_key_hash")
		_, err = store.CreateAPIKey(ctx, apikey.Hash("other"), "")
		wantConstraint(t, err, "ck_api_keys_label")
		_, err = store.pool.Exec(ctx, `UPDATE api_keys SET banned_at = now() WHERE id = $1`, created.ID)
		wantConstraint(t, err, "ck_api_keys_banned_revoked")
	})

	t.Run("unknown", func(t *testing.T) {
		if _, err := store.GetActiveAPIKey(ctx, apikey.Hash("unknown")); !errors.Is(err, storage.ErrAPIKeyNotFound) {
			t.Errorf("GetActiveAPIKey err = %v, want ErrAPIKeyNotFound", err)
		}
		for name, call := range map[string]func() error{
			"GetAPIKey":    func() error { _, err := store.GetAPIKey(ctx, 999); return err },
			"RevokeAPIKey": func() error { _, err := store.RevokeAPIKey(ctx, 999); return err },
			"BanAPIKey":    func() error { _, err := store.BanAPIKey(ctx, 999); return err },
		} {
			if err := call(); !errors.Is(err, storage.ErrAPIKeyNotFound) {
				t.Errorf("%s err = %v, want ErrAPIKeyNotFound", name, err)
			}
		}
	})

	t.Run("revoke", func(t *testing.T) {
		k := newKey(t, store, "revoked")
		hash := func() []byte {
			row := store.pool.QueryRow(ctx, `SELECT key_hash FROM api_keys WHERE id = $1`, k.ID)
			var h []byte
			if err := row.Scan(&h); err != nil {
				t.Fatal(err)
			}
			return h
		}()
		first, err := store.RevokeAPIKey(ctx, k.ID)
		if err != nil || first.RevokedAt == nil || first.BannedAt != nil {
			t.Fatalf("RevokeAPIKey = %+v, %v; want revoked, not banned", first, err)
		}
		if _, err := store.GetActiveAPIKey(ctx, hash); !errors.Is(err, storage.ErrAPIKeyNotFound) {
			t.Errorf("revoked key: err = %v, want ErrAPIKeyNotFound", err)
		}
		again, err := store.RevokeAPIKey(ctx, k.ID)
		if err != nil || !again.RevokedAt.Equal(*first.RevokedAt) {
			t.Errorf("second revoke = %+v, %v; want the first time kept", again, err)
		}
	})

	t.Run("ban", func(t *testing.T) {
		k := newKey(t, store, "banned")
		first, err := store.BanAPIKey(ctx, k.ID)
		if err != nil || first.RevokedAt == nil || first.BannedAt == nil {
			t.Fatalf("BanAPIKey = %+v, %v; want revoked and banned", first, err)
		}
		again, err := store.BanAPIKey(ctx, k.ID)
		if err != nil || !again.BannedAt.Equal(*first.BannedAt) || !again.RevokedAt.Equal(*first.RevokedAt) {
			t.Errorf("second ban = %+v, %v; want the first times kept", again, err)
		}
	})

	keys, err := store.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, k := range keys {
		labels = append(labels, k.Label)
	}
	if want := []string{"Alice laptop", "revoked", "banned"}; !slices.Equal(labels, want) {
		t.Errorf("ListAPIKeys labels = %v, want %v", labels, want)
	}
}

// usage returns a key's counters for a day, zero if it has no row.
func usage(t *testing.T, store *Store, keyID int64, day time.Time) (shares int, bytes int64) {
	t.Helper()
	rows, err := store.ListKeyUsage(t.Context(), day, keyID)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range rows {
		if u.Day.Equal(day) {
			return u.Shares, u.Bytes
		}
	}
	return 0, 0
}

func TestQuota(t *testing.T) {
	ctx := t.Context()
	store := New(newTestPool(t))
	quota := storage.Quota{Day: testDay, SharesPerDay: 10}

	t.Run("concurrent creates stop exactly at the limit", func(t *testing.T) {
		k := newKey(t, store, "busy")
		const attempts = 30
		errs := make([]error, attempts)
		var wg sync.WaitGroup
		for i := range attempts {
			wg.Go(func() {
				errs[i] = store.CreateShare(ctx, keyShare(fmt.Sprintf("busy-%02d", i), k.ID), quota)
			})
		}
		wg.Wait()
		var created, refused int
		for _, err := range errs {
			switch {
			case err == nil:
				created++
			case errors.Is(err, storage.ErrQuotaExceeded):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if created != 10 || refused != attempts-10 {
			t.Errorf("%d created, %d refused; want 10 and %d", created, refused, attempts-10)
		}
		if shares, bytes := usage(t, store, k.ID, testDay); shares != 10 || bytes != 10*100 {
			t.Errorf("usage = %d shares, %d bytes; want 10 and 1000", shares, bytes)
		}
		var rows int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM shares WHERE api_key_id = $1`, k.ID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 10 {
			t.Errorf("%d share rows, want 10: a refused create must insert nothing", rows)
		}

		// A new UTC day starts at zero.
		next := storage.Quota{Day: testDay.AddDate(0, 0, 1), SharesPerDay: 10}
		if err := store.CreateShare(ctx, keyShare("busy-next-day", k.ID), next); err != nil {
			t.Errorf("first share of the next day: %v", err)
		}
		// Another key has its own count.
		if err := store.CreateShare(ctx, keyShare("other-key", newKey(t, store, "other").ID), quota); err != nil {
			t.Errorf("another key's first share: %v", err)
		}
	})

	t.Run("zero never stops", func(t *testing.T) {
		k := newKey(t, store, "unlimited")
		for i := range 15 {
			if err := store.CreateShare(ctx, keyShare(fmt.Sprintf("free-%02d", i), k.ID), storage.Quota{Day: testDay}); err != nil {
				t.Fatalf("share %d: %v", i, err)
			}
		}
		if shares, _ := usage(t, store, k.ID, testDay); shares != 15 {
			t.Errorf("usage = %d shares, want 15", shares)
		}
	})

	t.Run("a failed insert rolls the count back", func(t *testing.T) {
		k := newKey(t, store, "unlucky")
		if err := store.CreateShare(ctx, keyShare("taken", k.ID), quota); err != nil {
			t.Fatal(err)
		}
		wantConstraint(t, store.CreateShare(ctx, keyShare("taken", k.ID), quota), "uq_shares_share_id")
		if shares, bytes := usage(t, store, k.ID, testDay); shares != 1 || bytes != 100 {
			t.Errorf("usage = %d shares, %d bytes; want 1 and 100", shares, bytes)
		}
	})

	t.Run("the day is the UTC date of the time passed", func(t *testing.T) {
		k := newKey(t, store, "zone")
		// 23:30 on the 17th in New York is the 18th in UTC.
		ny := time.FixedZone("EDT", -4*60*60)
		late := time.Date(2026, 10, 17, 23, 30, 0, 0, ny)
		if err := store.CreateShare(ctx, keyShare("zone-1", k.ID), storage.Quota{Day: late}); err != nil {
			t.Fatal(err)
		}
		if shares, _ := usage(t, store, k.ID, testDay.AddDate(0, 0, 1)); shares != 1 {
			t.Errorf("the share was not counted on the UTC day 2026-10-18")
		}
	})

	t.Run("without a key nothing is counted", func(t *testing.T) {
		if err := store.CreateShare(ctx, newShare("no-key", time.Hour), quota); err != nil {
			t.Fatal(err)
		}
		if id, err := store.ShareAPIKeyID(ctx, "no-key"); err != nil || id != 0 {
			t.Errorf("ShareAPIKeyID = %d, %v; want 0", id, err)
		}
	})
}

// A create re-checks its key in its own transaction and holds the key's row,
// so a revoke or ban can't slip in between the check and the insert.
func TestCreateShareRechecksTheKey(t *testing.T) {
	ctx := t.Context()
	store := New(newTestPool(t))
	quota := storage.Quota{Day: testDay, SharesPerDay: 10}

	t.Run("a key revoked since it was looked up", func(t *testing.T) {
		k := newKey(t, store, "revoked")
		if _, err := store.RevokeAPIKey(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateShare(ctx, keyShare("too-late", k.ID), quota); !errors.Is(err, storage.ErrAPIKeyNotFound) {
			t.Errorf("err = %v, want ErrAPIKeyNotFound", err)
		}
		if shares, _ := usage(t, store, k.ID, testDay); shares != 0 {
			t.Errorf("counted %d shares for a revoked key", shares)
		}
		if _, err := store.GetShare(ctx, "too-late"); !errors.Is(err, storage.ErrShareNotFound) {
			t.Errorf("the share was inserted: %v", err)
		}
	})

	t.Run("a ban waits for a create in flight, then sees its share", func(t *testing.T) {
		k := newKey(t, store, "racing")
		// A create's transaction, stopped after it took the lock.
		tx, err := store.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		q := store.q.WithTx(tx)
		if _, err := q.LockActiveAPIKey(ctx, k.ID); err != nil {
			t.Fatal(err)
		}

		banned := make(chan error, 1)
		go func() {
			_, err := store.BanAPIKey(ctx, k.ID)
			banned <- err
		}()
		select {
		case err := <-banned:
			t.Fatalf("the ban finished while a create held the key: %v", err)
		case <-time.After(300 * time.Millisecond):
		}

		if err := insertShare(ctx, q, keyShare("in-flight", k.ID)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-banned; err != nil {
			t.Fatal(err)
		}
		live, err := store.ListLiveSharesByKey(ctx, k.ID)
		if err != nil || len(live) != 1 || live[0].ShareID != "in-flight" {
			t.Errorf("after the ban, the key's live shares = %+v, %v; want the one created meanwhile", live, err)
		}
	})
}

func TestLookupShare(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)
	active := func(id string) {
		t.Helper()
		if err := store.CreateShare(ctx, newShare(id, time.Hour), storage.Quota{}); err != nil {
			t.Fatal(err)
		}
		if err := store.ActivateShare(ctx, id, storage.PendingTTL); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("concurrent lookups stop exactly at the cap", func(t *testing.T) {
		active("popular")
		const attempts, maxPulls = 20, 5
		errs := make([]error, attempts)
		var wg sync.WaitGroup
		for i := range attempts {
			wg.Go(func() {
				_, errs[i] = store.LookupShare(ctx, "popular", maxPulls)
			})
		}
		wg.Wait()
		var served, refused int
		for _, err := range errs {
			switch {
			case err == nil:
				served++
			case errors.Is(err, storage.ErrPullLimitReached):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if served != maxPulls || refused != attempts-maxPulls {
			t.Errorf("%d served, %d refused; want %d and %d", served, refused, maxPulls, attempts-maxPulls)
		}
		got, err := store.GetActiveShare(ctx, "popular")
		if err != nil || got.Lookups != maxPulls {
			t.Errorf("lookups = %d, %v; want %d: a refused lookup counts nothing", got.Lookups, err, maxPulls)
		}
		// The summary's rule agrees with the query's: used up at the cap,
		// not with one pull left.
		if !got.PullsUsedUp(maxPulls) || got.PullsUsedUp(maxPulls+1) || got.PullsUsedUp(0) {
			t.Errorf("PullsUsedUp disagrees with LookupShare at %d lookups", got.Lookups)
		}
	})

	t.Run("zero never stops", func(t *testing.T) {
		active("unlimited")
		for i := range 60 {
			if _, err := store.LookupShare(ctx, "unlimited", 0); err != nil {
				t.Fatalf("lookup %d: %v", i, err)
			}
		}
	})

	t.Run("pending, expired, and unknown are not found and not counted", func(t *testing.T) {
		if err := store.CreateShare(ctx, newShare("pending", time.Hour), storage.Quota{}); err != nil {
			t.Fatal(err)
		}
		insertAged(t, pool, "expired", storage.StatusActive, 2*time.Hour, -time.Second)
		for _, id := range []string{"pending", "expired", "missing"} {
			if _, err := store.LookupShare(ctx, id, 5); !errors.Is(err, storage.ErrShareNotFound) {
				t.Errorf("LookupShare(%s) err = %v, want ErrShareNotFound", id, err)
			}
		}
		if err := store.ActivateShare(ctx, "pending", storage.PendingTTL); err != nil {
			t.Fatal(err)
		}
		got, err := store.LookupShare(ctx, "pending", 5)
		if err != nil || got.Lookups != 1 {
			t.Errorf("first lookup after completion: lookups = %d, %v; want 1", got.Lookups, err)
		}
	})
}

func TestStoredBytes(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)

	if n, err := store.StoredBytes(ctx); err != nil || n != 0 {
		t.Fatalf("empty: StoredBytes = %d, %v; want 0", n, err)
	}
	for _, id := range []string{"pending", "active", "revoked"} {
		if err := store.CreateShare(ctx, newShare(id, time.Hour), storage.Quota{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ActivateShare(ctx, "active", storage.PendingTTL); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeShare(ctx, "revoked"); err != nil {
		t.Fatal(err)
	}
	insertAged(t, pool, "expired", storage.StatusActive, 2*time.Hour, -time.Second)

	// Pending and active count; revoked and expired don't.
	if n, err := store.StoredBytes(ctx); err != nil || n != 200 {
		t.Errorf("StoredBytes = %d, %v; want 200", n, err)
	}
}

func TestSharesOfAKey(t *testing.T) {
	ctx := t.Context()
	pool := newTestPool(t)
	store := New(pool)
	mine, other := newKey(t, store, "mine"), newKey(t, store, "other")

	for _, n := range []storage.NewShare{
		keyShare("mine-pending", mine.ID), keyShare("mine-active", mine.ID), keyShare("mine-revoked", mine.ID),
		keyShare("other-share", other.ID), newShare("no-key", time.Hour),
	} {
		if err := store.CreateShare(ctx, n, storage.Quota{Day: testDay}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ActivateShare(ctx, "mine-active", storage.PendingTTL); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeShare(ctx, "mine-revoked"); err != nil {
		t.Fatal(err)
	}

	live, err := store.ListLiveSharesByKey(ctx, mine.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range live {
		ids = append(ids, s.ShareID)
		if s.StorageKey != "stashes/"+s.ShareID {
			t.Errorf("storage key of %s = %q", s.ShareID, s.StorageKey)
		}
	}
	if want := []string{"mine-pending", "mine-active"}; !slices.Equal(ids, want) {
		t.Errorf("ListLiveSharesByKey = %v, want %v", ids, want)
	}

	// The key is found for an ended share too, while its row is there.
	if id, err := store.ShareAPIKeyID(ctx, "mine-revoked"); err != nil || id != mine.ID {
		t.Errorf("ShareAPIKeyID(mine-revoked) = %d, %v; want %d", id, err, mine.ID)
	}
	if _, err := store.ShareAPIKeyID(ctx, "missing"); !errors.Is(err, storage.ErrShareNotFound) {
		t.Errorf("ShareAPIKeyID(missing) err = %v, want ErrShareNotFound", err)
	}
	got, err := store.GetShare(ctx, "mine-active")
	if err != nil || got.APIKeyID != mine.ID {
		t.Errorf("GetShare APIKeyID = %d, %v; want %d", got.APIKeyID, err, mine.ID)
	}

	usageRows, err := store.ListKeyUsage(ctx, testDay, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []storage.KeyUsage{
		{APIKeyID: mine.ID, Day: testDay, Shares: 3, Bytes: 300},
		{APIKeyID: other.ID, Day: testDay, Shares: 1, Bytes: 100},
	}
	if len(usageRows) != len(want) {
		t.Fatalf("ListKeyUsage = %+v, want %+v", usageRows, want)
	}
	for i := range want {
		g := usageRows[i]
		if g.APIKeyID != want[i].APIKeyID || !g.Day.Equal(want[i].Day) || g.Shares != want[i].Shares || g.Bytes != want[i].Bytes {
			t.Errorf("usage row %d = %+v, want %+v", i, g, want[i])
		}
	}
	if later, err := store.ListKeyUsage(ctx, testDay.AddDate(0, 0, 1), 0); err != nil || len(later) != 0 {
		t.Errorf("usage from the next day = %+v, %v; want none", later, err)
	}
}
