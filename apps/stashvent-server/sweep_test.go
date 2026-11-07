package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/storage"
)

func testShare(id, status string, created, expires time.Time) storage.Share {
	return storage.Share{
		NewShare:  storage.NewShare{ShareID: id, StorageKey: "stashes/" + id, ExpiresAt: expires},
		Status:    status,
		CreatedAt: created,
	}
}

// expiredShares returns n active shares that expired a minute before now.
func expiredShares(now time.Time, n int) []storage.Share {
	shares := make([]storage.Share, n)
	for i := range shares {
		shares[i] = testShare(fmt.Sprintf("share-%d", i), storage.StatusActive, now.Add(-2*time.Hour), now.Add(-time.Minute))
	}
	return shares
}

// newTestSweeper returns a sweeper over shares, each with a stored object.
// now is the store's clock.
func newTestSweeper(now time.Time, shares []storage.Share) (*sweeper, *fakeStore, *fakeObjects) {
	store := newFakeStore(func() time.Time { return now })
	objs := &fakeObjects{sizes: map[string]int64{}, deleteErr: map[string]error{}}
	for _, s := range shares {
		store.add(s)
		objs.sizes[s.StorageKey] = 100
	}
	return &sweeper{
		store:   store,
		objects: objs,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, store, objs
}

func TestSweep(t *testing.T) {
	now := time.Now()
	sweptAge := storage.PendingTTL + pendingGrace
	shares := []storage.Share{
		testShare("active", storage.StatusActive, now.Add(-2*time.Hour), now.Add(time.Hour)),
		testShare("expired", storage.StatusActive, now.Add(-2*time.Hour), now.Add(-time.Minute)),
		testShare("expired-stuck", storage.StatusActive, now.Add(-2*time.Hour), now.Add(-time.Minute)),
		testShare("pending-new", storage.StatusPending, now.Add(-time.Minute), now.Add(time.Hour)),
		// Too old to complete, but still inside the grace period.
		testShare("pending-grace", storage.StatusPending, now.Add(-sweptAge+time.Minute), now.Add(6*time.Hour)),
		testShare("pending-stale", storage.StatusPending, now.Add(-sweptAge), now.Add(6*time.Hour)),
		// Revoked by a DELETE: expired, but its upload URL may still work.
		testShare("revoked-young", storage.StatusActive, now.Add(-uploadURLTTL-uploadGrace+time.Minute), now.Add(-time.Minute)),
		testShare("revoked-old", storage.StatusActive, now.Add(-uploadURLTTL-uploadGrace), now.Add(-time.Minute)),
	}
	sw, store, objs := newTestSweeper(now, shares)
	objs.deleteErr["stashes/expired-stuck"] = errors.New("storage unavailable")

	sw.sweep(t.Context())

	// expired-stuck keeps its row because its object couldn't be deleted.
	wantKept := map[string]bool{"active": true, "expired-stuck": true, "pending-new": true, "pending-grace": true, "revoked-young": true}
	for _, s := range shares {
		_, hasRow := store.shares[s.ShareID]
		_, hasObject := objs.sizes[s.StorageKey]
		if want := wantKept[s.ShareID]; hasRow != want || hasObject != want {
			t.Errorf("%s: row kept = %v, object kept = %v, want both %v", s.ShareID, hasRow, hasObject, want)
		}
	}
}

// Shares that can't be deleted are skipped, not fetched again: the sweep
// still reaches every other share, across more than one page.
func TestSweepGetsPastFailingShares(t *testing.T) {
	now := time.Now()
	shares := expiredShares(now, 3*sweepBatch)
	sw, store, objs := newTestSweeper(now, shares)
	stuck := 0
	for i, s := range shares {
		if i%3 == 0 {
			objs.deleteErr[s.StorageKey] = errors.New("storage unavailable")
			stuck++
		}
	}

	sw.sweep(t.Context())

	if got := len(store.shares); got != stuck {
		t.Fatalf("%d rows left, want only the %d stuck ones", got, stuck)
	}
	if got, want := objs.deletes, len(shares); got != want {
		t.Errorf("%d delete attempts, want %d: one per share", got, want)
	}
}

// When every removal fails, storage is down: the sweep gives up instead of
// working through the whole backlog.
func TestSweepStopsAfterRepeatedFailures(t *testing.T) {
	now := time.Now()
	shares := expiredShares(now, 2*sweepBatch)
	sw, store, objs := newTestSweeper(now, shares)
	for _, s := range shares {
		objs.deleteErr[s.StorageKey] = errors.New("storage unavailable")
	}

	sw.sweep(t.Context())

	if got := objs.deletes; got != sweepMaxFailures {
		t.Errorf("%d delete attempts, want %d", got, sweepMaxFailures)
	}
	if got, want := len(store.shares), len(shares); got != want {
		t.Errorf("%d rows left, want all %d", got, want)
	}
}

// cancelAfter cancels a context once Delete has been called n times.
type cancelAfter struct {
	objectDeleter
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfter) Delete(ctx context.Context, key string) error {
	err := c.objectDeleter.Delete(ctx, key)
	if c.n--; c.n == 0 {
		c.cancel()
	}
	return err
}

func TestSweepStopsOnShutdown(t *testing.T) {
	now := time.Now()
	sw, store, objs := newTestSweeper(now, expiredShares(now, 10))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sw.objects = &cancelAfter{objectDeleter: objs, n: 3, cancel: cancel}

	sw.sweep(ctx)

	if got, want := len(store.shares), 7; got != want {
		t.Errorf("%d rows left, want %d: the sweep should stop once the context is cancelled", got, want)
	}
}
