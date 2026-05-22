-- name: InsertPendingShortURL :one
-- Generated-slug path: slug stays NULL until the worker assigns one.
INSERT INTO short_urls (job_id, original_url, api_key_id, webhook_url, expires_at, status)
VALUES ($1, $2, $3, $4, $5, 'pending')
RETURNING id;

-- name: InsertPendingShortURLWithSlug :one
-- Custom-slug path: the slug is reserved at insert time. ON CONFLICT DO NOTHING
-- makes the reservation race-free — zero rows returned means the slug is taken
-- and the gateway responds 409.
INSERT INTO short_urls (job_id, slug, original_url, api_key_id, webhook_url, expires_at, status)
VALUES ($1, $2, $3, $4, $5, $6, 'pending')
ON CONFLICT (slug) DO NOTHING
RETURNING id;

-- name: ClaimShortURL :one
-- Atomically transition pending -> processing. Zero rows means another worker
-- already claimed it (or it is no longer pending); the caller branches on the
-- row's current status. The lease-based re-claim of stale rows arrives in M2.
UPDATE short_urls
SET status = 'processing', updated_at = NOW()
WHERE job_id = $1 AND status = 'pending'
RETURNING *;

-- name: GetShortURLByJobID :one
SELECT * FROM short_urls WHERE job_id = $1;

-- name: FinalizeShortURL :execrows
-- Write the assigned slug + QR object key and mark the job done. A unique
-- violation on slug surfaces as an error so the worker can regenerate.
UPDATE short_urls
SET slug = $2, qr_object = $3, status = 'done', updated_at = NOW()
WHERE job_id = $1;

-- name: FailShortURL :execrows
UPDATE short_urls
SET status = 'failed', updated_at = NOW()
WHERE job_id = $1;

-- name: GetActiveShortURLBySlug :one
-- The redirect path: only rows that are finalized and unexpired resolve;
-- anything else yields pgx.ErrNoRows, which the handler maps to 404.
SELECT * FROM short_urls
WHERE slug = $1
  AND status = 'done'
  AND (expires_at IS NULL OR expires_at > NOW());

-- name: IncrementHitCount :exec
UPDATE short_urls SET hit_count = hit_count + 1 WHERE slug = $1;
