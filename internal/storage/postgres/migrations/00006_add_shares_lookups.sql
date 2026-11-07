-- +goose Up
-- How many times a share was looked up (GET /v1/shares/{id}), for the pull
-- cap (MAX_PULLS_PER_SHARE). A count, not a repository detail.
ALTER TABLE shares
    ADD COLUMN lookups integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT ck_shares_lookups CHECK (lookups >= 0);

-- +goose Down
ALTER TABLE shares DROP COLUMN lookups;
