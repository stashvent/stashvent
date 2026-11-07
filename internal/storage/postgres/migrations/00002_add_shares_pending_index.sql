-- +goose Up
-- The expiry sweep looks up pending shares that were never completed.
CREATE INDEX idx_shares_pending_created_at ON shares (created_at) WHERE status = 'pending';

-- +goose Down
DROP INDEX idx_shares_pending_created_at;
