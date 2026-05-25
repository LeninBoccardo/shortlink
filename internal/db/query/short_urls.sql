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
-- Atomically transition a row to processing. Matches any pending or processing
-- row: asynq never delivers a task to two workers at once, so a processing row
-- is always this job's own prior attempt or a crashed worker — either way this
-- attempt takes over. The returned updated_at is the lease token guarding
-- finalize/fail; a stalled worker preempted by a re-claim fails that guard.
UPDATE short_urls
SET status = 'processing', updated_at = NOW()
WHERE job_id = $1 AND status IN ('pending', 'processing')
RETURNING *;

-- name: GetShortURLByJobID :one
SELECT * FROM short_urls WHERE job_id = $1;

-- name: FinalizeShortURL :execrows
-- Lease-guarded: zero rows means the lease was lost to a re-claim, so a stalled
-- worker's late finalize is harmlessly discarded. A unique violation on slug
-- surfaces as an error so the worker can regenerate.
UPDATE short_urls
SET slug = @slug, qr_object = @qr_object, status = 'done', updated_at = NOW()
WHERE job_id = @job_id AND updated_at = @lease;

-- name: FailShortURL :execrows
-- Lease-guarded, same as finalize: a preempted worker cannot stamp 'failed'
-- over a row another worker is actively re-processing.
UPDATE short_urls
SET status = 'failed', updated_at = NOW()
WHERE job_id = @job_id AND updated_at = @lease;

-- name: GetActiveShortURLBySlug :one
-- The redirect path: only rows that are finalized and unexpired resolve;
-- anything else yields pgx.ErrNoRows, which the handler maps to 404.
SELECT * FROM short_urls
WHERE slug = $1
  AND status = 'done'
  AND (expires_at IS NULL OR expires_at > NOW());

-- name: DeleteStaleReservations :execrows
-- Abandoned pending/processing rows past SWEEP_STALE_AGE. Deleting a row frees
-- any custom slug it had reserved.
DELETE FROM short_urls
WHERE status IN ('pending', 'processing') AND updated_at < @cutoff;

-- name: DeleteOldFailedShortURLs :execrows
DELETE FROM short_urls
WHERE status = 'failed' AND updated_at < @cutoff;

-- name: ListExpiredQRObjects :many
-- done rows whose QR object has outlived QR_OBJECT_TTL and is still present.
SELECT job_id, qr_object FROM short_urls
WHERE status = 'done' AND qr_object IS NOT NULL AND updated_at < @cutoff
LIMIT @max_rows;

-- name: ClearQRObjects :exec
-- Bulk variant used by the sweeper: NULLs qr_object for many job_ids in one
-- statement instead of N round-trips. Order vs storage delete is unchanged:
-- the column is cleared first so a concurrent webhook handler can't Stat
-- a key we're about to delete.
UPDATE short_urls SET qr_object = NULL WHERE job_id = ANY(@job_ids::text[]);
