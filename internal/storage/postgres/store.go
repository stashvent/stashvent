// Package postgres stores share metadata in Postgres.
//
// Queries live in queries/*.sql and are generated into db/ by sqlc (`make sqlc`).
// Callers see storage types and sentinels only, never sqlc or pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stashvent/stashvent/internal/storage"
	"github.com/stashvent/stashvent/internal/storage/postgres/db"
)

// Store implements share metadata storage on a pgx pool.
type Store struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: db.New(pool)}
}

// CreateShare inserts a pending share. A share created with an API key
// (n.APIKeyID set) also counts toward that key's quota for quota.Day, in the
// same transaction, so a failed insert uses up nothing. It returns
// storage.ErrQuotaExceeded, and inserts nothing, if the key has created
// quota.SharesPerDay shares that day already, and storage.ErrAPIKeyNotFound
// if the key was revoked or banned since the caller looked it up.
func (s *Store) CreateShare(ctx context.Context, n storage.NewShare, quota storage.Quota) error {
	if n.APIKeyID == 0 {
		return insertShare(ctx, s.q, n)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	q := s.q.WithTx(tx)

	// Holds the key's row until commit: a ban that starts now ends this share too.
	if _, err := q.LockActiveAPIKey(ctx, n.APIKeyID); errors.Is(err, pgx.ErrNoRows) {
		return storage.ErrAPIKeyNotFound
	} else if err != nil {
		return fmt.Errorf("locking api key: %w", err)
	}

	d := quota.Day.UTC()
	_, err = q.CountShareForKey(ctx, db.CountShareForKeyParams{
		ApiKeyID:   n.APIKeyID,
		Day:        time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC),
		Bytes:      n.Size,
		DailyLimit: int32(min(quota.SharesPerDay, math.MaxInt32)), //nolint:gosec // G115: clamped above
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ErrQuotaExceeded
	}
	if err != nil {
		return fmt.Errorf("counting share for key: %w", err)
	}
	if err := insertShare(ctx, q, n); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing share: %w", err)
	}
	return nil
}

func insertShare(ctx context.Context, q *db.Queries, n storage.NewShare) error {
	var apiKeyID *int64
	if n.APIKeyID != 0 {
		apiKeyID = &n.APIKeyID
	}
	err := q.CreateShare(ctx, db.CreateShareParams{
		ShareID:          n.ShareID,
		StorageKey:       n.StorageKey,
		Size:             n.Size,
		Salt:             n.Salt,
		Iv:               n.IV,
		DeleteTokenHash:  n.DeleteTokenHash,
		PayloadFormat:    n.PayloadFormat,
		PackagingMethod:  n.PackagingMethod,
		BundleVersion:    n.BundleVersion,
		ObjectFormat:     n.ObjectFormat,
		SenderGitVersion: n.SenderGitVersion,
		ExpiresAt:        n.ExpiresAt,
		ApiKeyID:         apiKeyID,
	})
	if err != nil {
		return fmt.Errorf("inserting share: %w", err)
	}
	return nil
}

// LookupShare returns an unexpired, active share and counts the lookup. It
// returns storage.ErrPullLimitReached, and counts nothing, if the share has
// been looked up maxPulls times already (0: no cap), and
// storage.ErrShareNotFound if there is no such share.
func (s *Store) LookupShare(ctx context.Context, shareID string, maxPulls int) (storage.Share, error) {
	share, err := one(s.q.LookupShare(ctx, db.LookupShareParams{
		ShareID:  shareID,
		MaxPulls: int32(min(maxPulls, math.MaxInt32)), //nolint:gosec // G115: clamped above
	}))
	if !errors.Is(err, storage.ErrShareNotFound) {
		return share, err
	}
	// Nothing was counted: the share is missing, or at its cap.
	if _, err := s.GetActiveShare(ctx, shareID); err != nil {
		return storage.Share{}, err
	}
	return storage.Share{}, storage.ErrPullLimitReached
}

// StoredBytes returns the declared size of every unexpired share, pending
// ones included.
func (s *Store) StoredBytes(ctx context.Context) (int64, error) {
	n, err := s.q.StoredBytes(ctx)
	if err != nil {
		return 0, fmt.Errorf("summing stored bytes: %w", err)
	}
	return n, nil
}

// ListLiveSharesByKey returns the unexpired shares, in any status, that a key
// created, with what is needed to end them.
func (s *Store) ListLiveSharesByKey(ctx context.Context, apiKeyID int64) ([]storage.ExpiredShare, error) {
	rows, err := s.q.ListLiveSharesByKey(ctx, apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("listing shares of key %d: %w", apiKeyID, err)
	}
	shares := make([]storage.ExpiredShare, len(rows))
	for i, row := range rows {
		shares[i] = storage.ExpiredShare(row)
	}
	return shares, nil
}

// ShareAPIKeyID returns the ID of the key that created a share, 0 if none
// did. Expired shares are found until the sweep removes them; otherwise it
// returns storage.ErrShareNotFound.
func (s *Store) ShareAPIKeyID(ctx context.Context, shareID string) (int64, error) {
	id, err := s.q.GetShareAPIKeyID(ctx, shareID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, storage.ErrShareNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("querying share key: %w", err)
	}
	if id == nil {
		return 0, nil
	}
	return *id, nil
}

// GetShare returns an unexpired share in any status.
func (s *Store) GetShare(ctx context.Context, shareID string) (storage.Share, error) {
	return one(s.q.GetShare(ctx, shareID))
}

// GetActiveShare returns an unexpired, active share.
func (s *Store) GetActiveShare(ctx context.Context, shareID string) (storage.Share, error) {
	return one(s.q.GetActiveShare(ctx, shareID))
}

// ActivateShare moves a pending share to active. It returns
// storage.ErrShareNotFound if no unexpired share with that ID has been pending
// for less than pendingTTL, measured with the database clock.
func (s *Store) ActivateShare(ctx context.Context, shareID string, pendingTTL time.Duration) error {
	n, err := s.q.ActivateShare(ctx, db.ActivateShareParams{ShareID: shareID, PendingTtl: pendingTTL})
	if err != nil {
		return fmt.Errorf("activating share: %w", err)
	}
	if n == 0 {
		return storage.ErrShareNotFound
	}
	return nil
}

// RevokeShare ends an unexpired share now, in any status: every lookup treats
// it as expired from here on. The row stays until the expiry sweep removes it
// (see ListExpired). It returns storage.ErrShareNotFound if no unexpired
// share has that ID.
func (s *Store) RevokeShare(ctx context.Context, shareID string) error {
	n, err := s.q.RevokeShare(ctx, shareID)
	if err != nil {
		return fmt.Errorf("revoking share: %w", err)
	}
	if n == 0 {
		return storage.ErrShareNotFound
	}
	return nil
}

// DeleteShare removes a share row in any status. It returns
// storage.ErrShareNotFound if no share has that ID.
func (s *Store) DeleteShare(ctx context.Context, shareID string) error {
	n, err := s.q.DeleteShare(ctx, shareID)
	if err != nil {
		return fmt.Errorf("deleting share: %w", err)
	}
	if n == 0 {
		return storage.ErrShareNotFound
	}
	return nil
}

// ListExpired returns up to limit shares to clean up, oldest first: those that
// have expired, and those that have been pending for pendingAge or longer,
// both measured with the database clock. A share younger than minAge is never
// returned: its upload URL may still work, so its row has to stay. Pass the
// last returned ID as afterID to get the next page (0 for the first).
func (s *Store) ListExpired(ctx context.Context, minAge, pendingAge time.Duration, afterID int64, limit int) ([]storage.ExpiredShare, error) {
	rows, err := s.q.ListExpired(ctx, db.ListExpiredParams{
		AfterID:    afterID,
		MinAge:     minAge,
		PendingAge: pendingAge,
		MaxRows:    int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing expired shares: %w", err)
	}
	shares := make([]storage.ExpiredShare, len(rows))
	for i, row := range rows {
		shares[i] = storage.ExpiredShare(row)
	}
	return shares, nil
}

func one(row db.Share, err error) (storage.Share, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.Share{}, storage.ErrShareNotFound
	}
	if err != nil {
		return storage.Share{}, fmt.Errorf("querying share: %w", err)
	}
	return toShare(row), nil
}

func toShare(row db.Share) storage.Share {
	var apiKeyID int64
	if row.ApiKeyID != nil {
		apiKeyID = *row.ApiKeyID
	}
	return storage.Share{
		NewShare: storage.NewShare{
			ShareID:          row.ShareID,
			StorageKey:       row.StorageKey,
			Size:             row.Size,
			Salt:             row.Salt,
			IV:               row.Iv,
			DeleteTokenHash:  row.DeleteTokenHash,
			PayloadFormat:    row.PayloadFormat,
			PackagingMethod:  row.PackagingMethod,
			BundleVersion:    row.BundleVersion,
			ObjectFormat:     row.ObjectFormat,
			SenderGitVersion: row.SenderGitVersion,
			ExpiresAt:        row.ExpiresAt,
			APIKeyID:         apiKeyID,
		},
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
		Lookups:   int(row.Lookups),
	}
}
