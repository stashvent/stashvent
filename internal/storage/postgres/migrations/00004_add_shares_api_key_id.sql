-- +goose Up
-- The key that created a share, when the server requires keys. It lets the
-- key delete the share, and a ban find every share the key created. shares
-- holds at most 7 days of rows, so the index is built in place.
ALTER TABLE shares
    ADD COLUMN api_key_id bigint,
    ADD CONSTRAINT fk_shares_api_keys FOREIGN KEY (api_key_id)
        REFERENCES api_keys (id) ON DELETE SET NULL;

CREATE INDEX idx_shares_api_key_id ON shares (api_key_id) WHERE api_key_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_shares_api_key_id;
ALTER TABLE shares DROP COLUMN api_key_id;
