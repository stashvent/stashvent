package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/stashvent/stashvent/internal/storage"
)

const (
	sweepInterval = 5 * time.Minute
	sweepBatch    = 100
	// sweepCallTimeout bounds one list query, and one share's removal, so a
	// hung storage or database call can't stall the sweep for good.
	sweepCallTimeout = 30 * time.Second
	// sweepMaxFailures is how many removals may fail in a row before the sweep
	// gives up until the next tick. That many in a row means storage or the
	// database is down, not that single shares are stuck.
	sweepMaxFailures = 5
	// pendingGrace is how long after storage.PendingTTL the sweep waits before
	// removing a pending share. /complete stops activating a share at
	// PendingTTL, so a /complete that is still in flight then has finished
	// long before the sweep deletes the object.
	pendingGrace = 15 * time.Minute
	// uploadGrace is how long after a share's upload URL expires the sweep
	// waits before removing the share. Only a revoked share can be due that
	// soon (the shortest expiry is an hour). Deleting an object doesn't cancel
	// the URL that wrote it, so the row stays until the URL has expired and
	// an upload started with it has finished; then the sweep removes the row
	// together with whatever was uploaded after the DELETE.
	uploadGrace = 15 * time.Minute
)

// expiredStore is the metadata store the sweeper needs (internal/storage/postgres).
type expiredStore interface {
	ListExpired(ctx context.Context, minAge, pendingAge time.Duration, afterID int64, limit int) ([]storage.ExpiredShare, error)
	DeleteShare(ctx context.Context, shareID string) error
}

// objectDeleter is the payload storage the sweeper needs (storage.Objects).
type objectDeleter interface {
	Delete(ctx context.Context, key string) error
}

// sweeper deletes expired shares (revoked ones included) and pending shares
// that were never completed: the object first, then the row.
type sweeper struct {
	store   expiredStore
	objects objectDeleter
	logger  *slog.Logger
}

// run sweeps once, then every sweepInterval until ctx is done.
func (s *sweeper) run(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		s.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweep removes everything that is due, one page at a time. A share whose
// object can't be deleted keeps its row, so the next sweep tries again and no
// object is left without a row pointing at it. It stops quietly when ctx is
// done: a shutdown is not a failed deletion.
func (s *sweeper) sweep(ctx context.Context) {
	var deleted, failed, failedInARow int
	var afterID int64
	for ctx.Err() == nil && failedInARow < sweepMaxFailures {
		shares, err := s.list(ctx, afterID)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			s.logger.ErrorContext(ctx, "listing expired shares", slog.Any("error", err))
			break
		}

		for _, share := range shares {
			if ctx.Err() != nil || failedInARow == sweepMaxFailures {
				break
			}
			err := s.remove(ctx, share)
			switch {
			case err == nil:
				deleted++
				failedInARow = 0
			case ctx.Err() != nil: // interrupted by shutdown
			default:
				failed++
				failedInARow++
				s.logger.ErrorContext(ctx, "deleting expired share", slog.String("share_id", share.ShareID), slog.Any("error", err))
			}
		}

		if len(shares) < sweepBatch {
			break
		}
		afterID = shares[len(shares)-1].ID
	}
	if failedInARow == sweepMaxFailures {
		s.logger.WarnContext(ctx, "expiry sweep stopped after repeated failures; retrying on the next sweep")
	}
	if deleted > 0 || failed > 0 {
		s.logger.InfoContext(ctx, "expiry sweep finished", slog.Int("deleted", deleted), slog.Int("failed", failed))
	}
}

func (s *sweeper) list(ctx context.Context, afterID int64) ([]storage.ExpiredShare, error) {
	ctx, cancel := context.WithTimeout(ctx, sweepCallTimeout)
	defer cancel()
	return s.store.ListExpired(ctx, uploadURLTTL+uploadGrace, storage.PendingTTL+pendingGrace, afterID, sweepBatch)
}

func (s *sweeper) remove(ctx context.Context, share storage.ExpiredShare) error {
	ctx, cancel := context.WithTimeout(ctx, sweepCallTimeout)
	defer cancel()

	if err := s.objects.Delete(ctx, share.StorageKey); err != nil {
		return err
	}
	// Another replica's sweep may have removed the row already.
	if err := s.store.DeleteShare(ctx, share.ShareID); err != nil && !errors.Is(err, storage.ErrShareNotFound) {
		return err
	}
	return nil
}
