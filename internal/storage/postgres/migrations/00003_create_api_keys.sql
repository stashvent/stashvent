-- +goose Up
-- Senders' API keys. Only the SHA-256 of a key is stored, never the key. No
-- owner or user columns: accounts live in the closed hosted service's own
-- tables. The hosted service writes rows here, so the table is
-- a contract with it: change it like the /v1 API.
-- A ban also revokes (ck_api_keys_banned_revoked); both keep the row, and with
-- it the key's usage history.
CREATE TABLE api_keys (
    id          bigint      GENERATED ALWAYS AS IDENTITY,
    key_hash    bytea       NOT NULL,
    label       text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    banned_at   timestamptz,
    CONSTRAINT pk_api_keys PRIMARY KEY (id),
    CONSTRAINT uq_api_keys_key_hash UNIQUE (key_hash),
    CONSTRAINT ck_api_keys_key_hash CHECK (length(key_hash) = 32),
    CONSTRAINT ck_api_keys_label CHECK (length(label) BETWEEN 1 AND 200),
    CONSTRAINT ck_api_keys_banned_revoked CHECK (banned_at IS NULL OR revoked_at IS NOT NULL)
);

-- +goose Down
DROP TABLE api_keys;
