-- name: CreateAPIKey :one
INSERT INTO api_keys (key_hash, key_hint, name, tier, webhook_secret, webhook_url)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAPIKeyByHash :one
-- Returns the key only if it exists and has not been revoked; a missing or
-- revoked key both yield pgx.ErrNoRows, which the gateway maps to 401.
SELECT * FROM api_keys
WHERE key_hash = $1 AND revoked_at IS NULL;

-- name: GetAPIKeyByID :one
-- Used by the webhook-delivery job to recover the per-key signing secret and
-- key hint from the short_urls row's api_key_id.
SELECT * FROM api_keys WHERE id = $1;

-- name: UpdateLastUsedAt :exec
-- Bumps last_used_at; the gateway throttles how often this runs via a Redis
-- marker (SPEC §9 / LAST_USED_THROTTLE). The revoked_at IS NULL guard
-- prevents the async toucher from bumping a key that was revoked between
-- the request's auth check and the touch goroutine actually firing —
-- otherwise a revoked key would show a fresh last_used_at in admin views,
-- confusing audit trails.
UPDATE api_keys SET last_used_at = NOW() WHERE id = $1 AND revoked_at IS NULL;
