-- +goose Up
CREATE TABLE api_keys (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash       TEXT NOT NULL UNIQUE,          -- SHA-256 of the raw key
    key_hint       TEXT NOT NULL,                 -- last 6 chars, for display only
    name           TEXT NOT NULL,
    tier           TEXT NOT NULL DEFAULT 'free',  -- free | pro | unlimited
    webhook_secret TEXT NOT NULL,                 -- per-key HMAC signing secret (SPEC §8/§9)
    webhook_url    TEXT,                          -- default webhook per key (fallback)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at     TIMESTAMPTZ,
    last_used_at   TIMESTAMPTZ
);

-- +goose Down
DROP TABLE api_keys;
