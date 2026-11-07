-- name: CreateAPIKey :one
INSERT INTO api_keys (key_hash, label) VALUES ($1, $2)
RETURNING *;

-- name: GetActiveAPIKeyByHash :one
-- Revoked keys (banned ones included) are refused like unknown ones.
SELECT * FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL;

-- name: LockActiveAPIKey :one
-- Run in the transaction that creates a share with the key. It refuses a key
-- revoked since the request was checked, and holds the key's row until the
-- share is committed: a revoke or ban (an UPDATE of the row) waits for it, so
-- a ban's list of the key's shares can't miss one created at that moment.
SELECT id FROM api_keys WHERE id = $1 AND revoked_at IS NULL FOR SHARE;

-- name: GetAPIKey :one
SELECT * FROM api_keys WHERE id = $1;

-- name: ListAPIKeys :many
SELECT * FROM api_keys ORDER BY id;

-- name: RevokeAPIKey :one
-- Keeps the first revoke time when run again.
UPDATE api_keys SET revoked_at = coalesce(revoked_at, now()), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: BanAPIKey :one
-- A ban also revokes. Keeps the first times when run again, so a ban that
-- failed half-way can simply be repeated.
UPDATE api_keys
SET revoked_at = coalesce(revoked_at, now()),
    banned_at  = coalesce(banned_at, now()),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: CountShareForKey :one
-- Counts one share for a key on a UTC day, unless the key has created
-- daily_limit shares that day already (0: no limit). Check and count are one
-- statement, so two pushes at the same moment can't both pass at the limit
-- minus one. No row back means the quota is used up. day is computed by the
-- caller, never with current_date, which follows the session's TimeZone.
INSERT INTO api_key_usage (api_key_id, day, shares, bytes)
VALUES (@api_key_id, @day, 1, @bytes)
ON CONFLICT ON CONSTRAINT uq_api_key_usage_api_key_id_day
DO UPDATE SET shares     = api_key_usage.shares + 1,
              bytes      = api_key_usage.bytes + EXCLUDED.bytes,
              updated_at = now()
WHERE @daily_limit::int = 0 OR api_key_usage.shares < @daily_limit::int
RETURNING shares;

-- name: ListKeyUsage :many
-- Usage per key per day from since on; api_key_id 0 means every key.
SELECT api_key_id, day, shares, bytes FROM api_key_usage
WHERE day >= @since AND (@api_key_id::bigint = 0 OR api_key_id = @api_key_id::bigint)
ORDER BY api_key_id, day;
