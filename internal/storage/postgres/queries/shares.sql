-- name: CreateShare :exec
INSERT INTO shares (
    share_id, storage_key, size, salt, iv, delete_token_hash,
    payload_format, packaging_method, bundle_version, object_format,
    sender_git_version, expires_at, api_key_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: GetShare :one
SELECT * FROM shares
WHERE share_id = $1 AND expires_at > now();

-- name: GetActiveShare :one
SELECT * FROM shares
WHERE share_id = $1 AND status = 'active' AND expires_at > now();

-- name: LookupShare :one
-- Counts a lookup of an active share and returns it, unless it has been looked
-- up max_pulls times already (0: no cap). Check and count are one statement,
-- so concurrent lookups stop exactly at the cap. The summary reads the same
-- rule from storage.Share.PullsUsedUp; change both together.
UPDATE shares SET lookups = lookups + 1
WHERE share_id = sqlc.arg(share_id)
  AND status = 'active'
  AND expires_at > now()
  AND (sqlc.arg(max_pulls)::int = 0 OR lookups < sqlc.arg(max_pulls)::int)
RETURNING *;

-- name: StoredBytes :one
-- The declared size of every share that hasn't expired, pending ones
-- included: counting only completed shares would let many pushes started at
-- once each pass the storage cap before any upload finishes.
SELECT coalesce(sum(size), 0)::bigint FROM shares WHERE expires_at > now();

-- name: ListLiveSharesByKey :many
-- The unexpired shares, in any status, that a key created: what a ban ends.
SELECT id, share_id, storage_key FROM shares
WHERE api_key_id = sqlc.arg(api_key_id)::bigint AND expires_at > now()
ORDER BY id;

-- name: GetShareAPIKeyID :one
-- Which key created a share, for an abuse report. Expired shares are found
-- too, until the sweep removes their rows.
SELECT api_key_id FROM shares WHERE share_id = $1;

-- name: ActivateShare :execrows
-- A share that has been pending for pending_ttl or longer can no longer be
-- activated (storage.PendingTTL). The expiry sweep removes it a little later.
-- Ages are measured with the database clock, which also stamped created_at.
UPDATE shares SET status = 'active', updated_at = now()
WHERE share_id = sqlc.arg(share_id)
  AND status = 'pending'
  AND expires_at > now()
  AND created_at > now() - sqlc.arg(pending_ttl)::interval;

-- name: RevokeShare :execrows
-- Ends a share now, whatever its status, by moving its expiry to now. The row
-- stays, so the expiry sweep still knows the storage key and can remove
-- anything uploaded after the revoke (see min_age in ListExpired).
-- greatest() keeps ck_shares_expires_after_created satisfied if the clock has
-- stepped back since the row was inserted.
UPDATE shares
SET expires_at = greatest(now(), created_at + interval '1 microsecond'), updated_at = now()
WHERE share_id = $1 AND expires_at > now();

-- name: DeleteShare :execrows
DELETE FROM shares WHERE share_id = $1;

-- name: ListExpired :many
-- Shares the expiry sweep removes: expired ones, and ones that have been
-- pending for pending_age or longer. No share is listed before it is min_age
-- old: a share revoked soon after it was created still has a working upload
-- URL, and its row must outlive that URL. Pages by id (after_id), so a share
-- that fails to delete doesn't hold up the ones behind it. Returns only what
-- the sweep needs.
SELECT id, share_id, storage_key FROM shares
WHERE id > sqlc.arg(after_id)
  AND created_at <= now() - sqlc.arg(min_age)::interval
  AND (expires_at <= now()
       OR (status = 'pending' AND created_at <= now() - sqlc.arg(pending_age)::interval))
ORDER BY id
LIMIT sqlc.arg(max_rows)::bigint;
