-- +goose Up
CREATE TABLE shares (
    id                 bigint      GENERATED ALWAYS AS IDENTITY,
    share_id           text        NOT NULL,
    storage_key        text        NOT NULL,
    status             text        NOT NULL DEFAULT 'pending',
    size               bigint      NOT NULL,
    salt               bytea       NOT NULL,
    iv                 bytea       NOT NULL,
    delete_token_hash  bytea       NOT NULL,
    payload_format     text        NOT NULL,
    packaging_method   text        NOT NULL,
    bundle_version     integer     NOT NULL,
    object_format      text        NOT NULL,
    sender_git_version text        NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    CONSTRAINT pk_shares PRIMARY KEY (id),
    CONSTRAINT uq_shares_share_id UNIQUE (share_id),
    CONSTRAINT uq_shares_storage_key UNIQUE (storage_key),
    CONSTRAINT ck_shares_status CHECK (status IN ('pending', 'active')),
    CONSTRAINT ck_shares_size CHECK (size >= 0),
    CONSTRAINT ck_shares_expires_after_created CHECK (expires_at > created_at)
);

CREATE INDEX idx_shares_expires_at ON shares (expires_at);

-- +goose Down
DROP TABLE shares;
