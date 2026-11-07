-- +goose Up
-- Shares created per key per UTC day: the daily quota, and the usage report
-- of the operator commands. Counted here rather than from shares, whose rows
-- the expiry sweep deletes. Rows are kept as the usage history.
CREATE TABLE api_key_usage (
    id          bigint      GENERATED ALWAYS AS IDENTITY,
    api_key_id  bigint      NOT NULL,
    day         date        NOT NULL,            -- UTC calendar day
    shares      integer     NOT NULL DEFAULT 0,  -- shares created that day
    bytes       bigint      NOT NULL DEFAULT 0,  -- sum of their declared sizes
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_api_key_usage PRIMARY KEY (id),
    CONSTRAINT uq_api_key_usage_api_key_id_day UNIQUE (api_key_id, day),
    CONSTRAINT fk_api_key_usage_api_keys FOREIGN KEY (api_key_id)
        REFERENCES api_keys (id) ON DELETE CASCADE,
    CONSTRAINT ck_api_key_usage_shares CHECK (shares >= 0),
    CONSTRAINT ck_api_key_usage_bytes  CHECK (bytes >= 0)
);

-- +goose Down
DROP TABLE api_key_usage;
