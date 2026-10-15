CREATE TABLE upload_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    key_hash     bytea NOT NULL UNIQUE,
    uploads      bigint NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
