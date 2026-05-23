package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/qrcode"
	"github.com/leninboccardo/shortlink/internal/queue"
	"github.com/leninboccardo/shortlink/internal/shortener"
	"github.com/leninboccardo/shortlink/internal/webhook"
)

// handleShortenJob runs the shorten pipeline: claim -> slug -> QR -> upload ->
// finalize -> enqueue webhook (SPEC §4.2).
func (w *worker) handleShortenJob(ctx context.Context, payload []byte) error {
	started := time.Now()
	var p queue.ShortenJobPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("unmarshal shorten payload: %w", err)
	}

	// Claim the row. With asynq's single-delivery guarantee a processing row is
	// this job's own prior attempt or a crashed worker — either way we take it
	// over. The returned updated_at is the lease token guarding finalize/fail.
	row, err := w.queries.ClaimShortURL(ctx, p.JobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return w.handleUnclaimable(ctx, p.JobID)
		}
		return fmt.Errorf("claim %s: %w", p.JobID, err)
	}
	lease := row.UpdatedAt

	slug := p.CustomSlug
	generated := slug == ""
	qrKey := qrcode.ObjectKey(p.JobID, time.Now())

	// Generate the QR and finalize, regenerating the slug on a unique collision.
	for attempt := 0; ; attempt++ {
		if generated {
			s, err := shortener.Generate(w.cfg.SlugLength)
			if err != nil {
				return w.shortenFailed(ctx, p, lease, fmt.Errorf("generate slug: %w", err))
			}
			slug = s
		}
		png, err := qrcode.Generate(w.cfg.ShortURLBase+"/"+slug, w.cfg.QRSize)
		if err != nil {
			return w.shortenFailed(ctx, p, lease, fmt.Errorf("generate qr: %w", err))
		}
		if err := w.store.Upload(ctx, qrKey, png, "image/png"); err != nil {
			return w.shortenFailed(ctx, p, lease, fmt.Errorf("upload qr: %w", err))
		}
		rows, err := w.queries.FinalizeShortURL(ctx, db.FinalizeShortURLParams{
			Slug:     pgtype.Text{String: slug, Valid: true},
			QrObject: pgtype.Text{String: qrKey, Valid: true},
			JobID:    p.JobID,
			Lease:    lease,
		})
		if err != nil {
			if generated && isUniqueViolation(err) && attempt < w.cfg.SlugMaxRetries {
				w.log.Warn("slug collision, regenerating", "job_id", p.JobID, "attempt", attempt+1)
				continue
			}
			return w.shortenFailed(ctx, p, lease, fmt.Errorf("finalize %s: %w", p.JobID, err))
		}
		if rows == 0 {
			w.log.Warn("finalize matched no rows; lease lost to a re-claim", "job_id", p.JobID)
			return nil
		}
		break
	}

	w.log.Info("shorten job complete", "job_id", p.JobID, "slug", slug)
	w.emitter.Emit(events.Event{
		Level:      events.LevelInfo,
		Kind:       events.KindJobComplete,
		APIKeyHash: p.APIKeyHash,
		APIKeyHint: p.APIKeyHint,
		Message:    "shorten job completed: slug=" + slug,
		Meta: map[string]any{
			"job_id":      p.JobID,
			"slug":        slug,
			"duration_ms": time.Since(started).Milliseconds(),
		},
	})
	w.enqueueWebhook(ctx, p.JobID)
	return nil
}

// handleUnclaimable runs when the claim matched no row — the job is already
// done, already failed, or its row is gone.
func (w *worker) handleUnclaimable(ctx context.Context, jobID string) error {
	row, err := w.queries.GetShortURLByJobID(ctx, jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.log.Warn("shorten job for unknown row, skipping", "job_id", jobID)
			return nil
		}
		return fmt.Errorf("load %s: %w", jobID, err)
	}
	if row.Status == "done" {
		w.enqueueWebhook(ctx, jobID) // idempotent — asynq dedups by job_id
		return nil
	}
	w.log.Info("shorten job not claimable, skipping", "job_id", jobID, "status", row.Status)
	return nil
}

// handleWebhookJob re-presigns the QR URL, signs the payload, and POSTs it to
// the client's webhook (SPEC §4.2/§8). On failure asynq retries per the §8
// schedule and archives to the dead-letter set; the short_urls status is never
// changed — the short URL was created, only delivery failed.
func (w *worker) handleWebhookJob(ctx context.Context, payload []byte) error {
	var p queue.WebhookJobPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("unmarshal webhook payload: %w", err)
	}

	row, err := w.queries.GetShortURLByJobID(ctx, p.JobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.log.Warn("webhook job for unknown row, skipping", "job_id", p.JobID)
			return nil
		}
		return fmt.Errorf("load %s: %w", p.JobID, err)
	}
	if row.Status != "done" || !row.Slug.Valid || !row.QrObject.Valid {
		w.log.Warn("webhook job for non-finalized row, skipping",
			"job_id", p.JobID, "status", row.Status)
		return nil
	}

	apiKey, err := w.queries.GetAPIKeyByID(ctx, row.ApiKeyID)
	if err != nil {
		return fmt.Errorf("load api key for %s: %w", p.JobID, err)
	}

	// A fresh signed URL on every attempt — the client always gets the full TTL.
	downloadURL, err := w.store.PresignGet(ctx, row.QrObject.String, w.cfg.SignedURLTTL)
	if err != nil {
		return fmt.Errorf("presign qr for %s: %w", p.JobID, err)
	}
	size, err := w.store.Stat(ctx, row.QrObject.String)
	if err != nil {
		w.log.Warn("stat qr object", "error", err, "job_id", p.JobID)
	}

	// Re-validate the webhook URL — DNS may have changed since enqueue.
	if err := w.ssrf.ValidateURL(ctx, row.WebhookUrl); err != nil {
		w.log.Error("webhook url failed SSRF re-validation, skipping delivery",
			"job_id", p.JobID, "error", err)
		return nil
	}

	body := webhook.Payload{
		JobID:    p.JobID,
		Status:   "success",
		ShortURL: w.cfg.ShortURLBase + "/" + row.Slug.String,
		QRCode: webhook.QRCode{
			DownloadURL: downloadURL,
			ExpiresAt:   time.Now().Add(w.cfg.SignedURLTTL).UTC().Format(time.RFC3339),
			SizeBytes:   size,
		},
		OriginalURL: row.OriginalUrl,
		CreatedAt:   row.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
	if err := w.dispatcher.Deliver(ctx, row.WebhookUrl, apiKey.WebhookSecret, apiKey.KeyHint, body); err != nil {
		final := queue.IsLastAttempt(ctx)
		if final {
			w.log.Error("webhook delivery permanently failed (archived)", "job_id", p.JobID, "error", err)
		}
		w.emitter.Emit(events.Event{
			Level:      events.LevelError,
			Kind:       events.KindWebhookFailed,
			APIKeyHash: apiKey.KeyHash,
			APIKeyHint: apiKey.KeyHint,
			Message:    "webhook delivery failed: " + err.Error(),
			Meta: map[string]any{
				"job_id":        p.JobID,
				"target":        row.WebhookUrl,
				"final_attempt": final,
			},
		})
		return fmt.Errorf("deliver webhook for %s: %w", p.JobID, err)
	}
	w.log.Info("webhook delivered", "job_id", p.JobID, "target", row.WebhookUrl)
	w.emitter.Emit(events.Event{
		Level:      events.LevelInfo,
		Kind:       events.KindWebhookSent,
		APIKeyHash: apiKey.KeyHash,
		APIKeyHint: apiKey.KeyHint,
		Message:    "webhook delivered",
		Meta: map[string]any{
			"job_id": p.JobID,
			"target": row.WebhookUrl,
		},
	})
	return nil
}

// enqueueWebhook submits the webhook-delivery job. The job_id key deduplicates
// it, so a re-enqueue (e.g. from a redelivered shorten job) is harmless.
func (w *worker) enqueueWebhook(ctx context.Context, jobID string) {
	wp, err := json.Marshal(queue.WebhookJobPayload{JobID: jobID, EnqueuedAt: time.Now().Unix()})
	if err != nil {
		w.log.Error("marshal webhook job", "error", err, "job_id", jobID)
		return
	}
	if err := w.queue.Enqueue(ctx, queue.Job{Type: queue.TypeWebhook, Key: jobID, Payload: wp}); err != nil {
		w.log.Error("enqueue webhook job", "error", err, "job_id", jobID)
	}
}

// shortenFailed records a failed attempt. On the final attempt it marks the row
// 'failed' (lease-guarded) so the sweeper later frees any reserved custom slug;
// asynq then archives the task. The cause is returned so asynq retries or
// archives the job.
func (w *worker) shortenFailed(ctx context.Context, p queue.ShortenJobPayload, lease pgtype.Timestamptz, cause error) error {
	jobID := p.JobID
	if !queue.IsLastAttempt(ctx) {
		w.log.Warn("shorten attempt failed, will retry", "job_id", jobID, "error", cause)
		w.emitter.Emit(events.Event{
			Level:      events.LevelError,
			Kind:       events.KindJobError,
			APIKeyHash: p.APIKeyHash,
			APIKeyHint: p.APIKeyHint,
			Message:    "shorten attempt failed, will retry: " + cause.Error(),
			Meta:       map[string]any{"job_id": jobID},
		})
		return cause
	}
	rows, err := w.queries.FailShortURL(ctx, db.FailShortURLParams{JobID: jobID, Lease: lease})
	switch {
	case err != nil:
		w.log.Error("mark job failed", "job_id", jobID, "error", err)
	case rows == 0:
		w.log.Warn("fail matched no rows; lease lost to a re-claim", "job_id", jobID)
	default:
		w.log.Error("shorten job permanently failed (archived)", "job_id", jobID, "error", cause)
	}
	w.emitter.Emit(events.Event{
		Level:      events.LevelError,
		Kind:       events.KindJobDLQ,
		APIKeyHash: p.APIKeyHash,
		APIKeyHint: p.APIKeyHint,
		Message:    "shorten job permanently failed (archived): " + cause.Error(),
		Meta:       map[string]any{"job_id": jobID},
	})
	return cause
}

// isUniqueViolation reports whether err is a Postgres unique-constraint error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
