package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stashvent/stashvent/internal/storage"
	"github.com/stashvent/stashvent/internal/storage/postgres/db"
)

// CreateAPIKey stores a new key by its hash (apikey.Hash), never the key.
func (s *Store) CreateAPIKey(ctx context.Context, keyHash []byte, label string) (storage.APIKey, error) {
	row, err := s.q.CreateAPIKey(ctx, db.CreateAPIKeyParams{KeyHash: keyHash, Label: label})
	if err != nil {
		return storage.APIKey{}, fmt.Errorf("inserting api key: %w", err)
	}
	return toAPIKey(row), nil
}

// GetActiveAPIKey returns the key with that hash. It returns
// storage.ErrAPIKeyNotFound for an unknown, revoked, or banned key.
func (s *Store) GetActiveAPIKey(ctx context.Context, keyHash []byte) (storage.APIKey, error) {
	return oneKey(s.q.GetActiveAPIKeyByHash(ctx, keyHash))
}

// GetAPIKey returns a key by its internal ID, whatever its state.
func (s *Store) GetAPIKey(ctx context.Context, id int64) (storage.APIKey, error) {
	return oneKey(s.q.GetAPIKey(ctx, id))
}

// ListAPIKeys returns every key, oldest first.
func (s *Store) ListAPIKeys(ctx context.Context) ([]storage.APIKey, error) {
	rows, err := s.q.ListAPIKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	keys := make([]storage.APIKey, len(rows))
	for i, row := range rows {
		keys[i] = toAPIKey(row)
	}
	return keys, nil
}

// RevokeAPIKey refuses the key from now on. Revoking it again keeps the
// first time. It returns storage.ErrAPIKeyNotFound if no key has that ID.
func (s *Store) RevokeAPIKey(ctx context.Context, id int64) (storage.APIKey, error) {
	return oneKey(s.q.RevokeAPIKey(ctx, id))
}

// BanAPIKey revokes the key and marks it banned. Banning it again keeps the
// first times. Ending its shares is the caller's job. It returns
// storage.ErrAPIKeyNotFound if no key has that ID.
func (s *Store) BanAPIKey(ctx context.Context, id int64) (storage.APIKey, error) {
	return oneKey(s.q.BanAPIKey(ctx, id))
}

// ListKeyUsage returns the shares each key created per UTC day from since
// on, by key, then day. apiKeyID 0 means every key.
func (s *Store) ListKeyUsage(ctx context.Context, since time.Time, apiKeyID int64) ([]storage.KeyUsage, error) {
	d := since.UTC()
	rows, err := s.q.ListKeyUsage(ctx, db.ListKeyUsageParams{
		Since:    time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC),
		ApiKeyID: apiKeyID,
	})
	if err != nil {
		return nil, fmt.Errorf("listing key usage: %w", err)
	}
	usage := make([]storage.KeyUsage, len(rows))
	for i, row := range rows {
		usage[i] = storage.KeyUsage{APIKeyID: row.ApiKeyID, Day: row.Day, Shares: int(row.Shares), Bytes: row.Bytes}
	}
	return usage, nil
}

func oneKey(row db.ApiKey, err error) (storage.APIKey, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.APIKey{}, storage.ErrAPIKeyNotFound
	}
	if err != nil {
		return storage.APIKey{}, fmt.Errorf("querying api key: %w", err)
	}
	return toAPIKey(row), nil
}

func toAPIKey(row db.ApiKey) storage.APIKey {
	return storage.APIKey{
		ID:        row.ID,
		Label:     row.Label,
		CreatedAt: row.CreatedAt,
		RevokedAt: row.RevokedAt,
		BannedAt:  row.BannedAt,
	}
}
