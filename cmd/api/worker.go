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
	"github.com/leninboccardo/shortlink/internal/qrcode"
	"github.com/leninboccardo/shortlink/internal/queue"
	"github.com/leninboccardo/shortlink/internal/shortener"
	"github.com/leninboccardo/shortlink/internal/webhook"
)

// handleShortenJob runs the shorten pipeline: claim -> slug -> QR -> upload ->
// finalize -> enqueue webhook (SPEC §4.2).
func (a *app) handleShortenJob(ctx context.Context, payload []byte) error {
	var p queue.ShortenJobPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("unmarshal shorten payload: %w", err)
	}

	if _, err := a.queries.ClaimShortURL(ctx, p.JobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return a.handleUnclaimable(ctx, p.JobID)
		}
		return fmt.Errorf("claim %s: %w", p.JobID, err)
	}

	slug := p.CustomSlug
	generated := slug == ""
	qrKey := qrcode.ObjectKey(p.JobID, time.Now())

	// Generate the QR and finalize, regenerating the slug on the (astronomically
	// rare) unique-constraint collision.
	for attempt := 0; ; attempt++ {
		if generated {
			s, err := shortener.Generate(a.cfg.SlugLength)
			if err != nil {
				return a.failJob(ctx, p.JobID, fmt.Errorf("generate slug: %w", err))
			}
			slug = s
		}

		png, err := qrcode.Generate(a.cfg.ShortURLBase+"/"+slug, a.cfg.QRSize)
		if err != nil {
			return a.failJob(ctx, p.JobID, fmt.Errorf("generate qr: %w", err))
		}
		if err := a.store.Upload(ctx, qrKey, png, "image/png"); err != nil {
			return a.failJob(ctx, p.JobID, fmt.Errorf("upload qr: %w", err))
		}

		rows, err := a.queries.FinalizeShortURL(ctx, db.FinalizeShortURLParams{
			JobID:    p.JobID,
			Slug:     pgtype.Text{String: slug, Valid: true},
			QrObject: pgtype.Text{String: qrKey, Valid: true},
		})
		if err != nil {
			if generated && isUniqueViolation(err) && attempt < a.cfg.SlugMaxRetries {
				a.log.Warn("slug collision, regenerating", "job_id", p.JobID, "attempt", attempt+1)
				continue
			}
			return a.failJob(ctx, p.JobID, fmt.Errorf("finalize %s: %w", p.JobID, err))
		}
		if rows == 0 {
			a.log.Warn("finalize affected no rows; short_urls row missing", "job_id", p.JobID)
			return nil
		}
		break
	}

	a.log.Info("shorten job complete", "job_id", p.JobID, "slug", slug)

	wp, err := json.Marshal(queue.WebhookJobPayload{JobID: p.JobID, EnqueuedAt: time.Now().Unix()})
	if err != nil {
		return fmt.Errorf("marshal webhook job: %w", err)
	}
	if err := a.queue.Enqueue(ctx, queue.Job{Type: queue.TypeWebhook, Payload: wp}); err != nil {
		a.log.Error("enqueue webhook job", "error", err, "job_id", p.JobID)
	}
	return nil
}

// handleUnclaimable runs when the claim UPDATE matched no row — the job was
// already processed, or its row is gone. The in-process M1 queue delivers each
// job exactly once, so this is defensive; the redelivery cases it guards
// against become live with the Redis queue in M2.
func (a *app) handleUnclaimable(ctx context.Context, jobID string) error {
	row, err := a.queries.GetShortURLByJobID(ctx, jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.log.Warn("shorten job for unknown row, skipping", "job_id", jobID)
			return nil
		}
		return fmt.Errorf("load %s: %w", jobID, err)
	}
	if row.Status == "done" {
		wp, _ := json.Marshal(queue.WebhookJobPayload{JobID: jobID, EnqueuedAt: time.Now().Unix()})
		if err := a.queue.Enqueue(ctx, queue.Job{Type: queue.TypeWebhook, Payload: wp}); err != nil {
			a.log.Error("re-enqueue webhook job", "error", err, "job_id", jobID)
		}
		return nil
	}
	a.log.Info("shorten job not claimable, skipping", "job_id", jobID, "status", row.Status)
	return nil
}

// handleWebhookJob re-presigns the QR download URL, signs the payload, and
// POSTs it to the client's webhook (SPEC §4.2/§8).
func (a *app) handleWebhookJob(ctx context.Context, payload []byte) error {
	var p queue.WebhookJobPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("unmarshal webhook payload: %w", err)
	}

	row, err := a.queries.GetShortURLByJobID(ctx, p.JobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.log.Warn("webhook job for unknown row, skipping", "job_id", p.JobID)
			return nil
		}
		return fmt.Errorf("load %s: %w", p.JobID, err)
	}
	if row.Status != "done" || !row.Slug.Valid || !row.QrObject.Valid {
		a.log.Warn("webhook job for non-finalized row, skipping",
			"job_id", p.JobID, "status", row.Status)
		return nil
	}

	apiKey, err := a.queries.GetAPIKeyByID(ctx, row.ApiKeyID)
	if err != nil {
		return fmt.Errorf("load api key for %s: %w", p.JobID, err)
	}

	// A fresh signed URL on every attempt — the client always gets the full TTL.
	downloadURL, err := a.store.PresignGet(ctx, row.QrObject.String, a.cfg.SignedURLTTL)
	if err != nil {
		return fmt.Errorf("presign qr for %s: %w", p.JobID, err)
	}
	size, err := a.store.Stat(ctx, row.QrObject.String)
	if err != nil {
		a.log.Warn("stat qr object", "error", err, "job_id", p.JobID)
	}

	// Re-validate the webhook URL — DNS may have changed since enqueue.
	if err := a.ssrf.ValidateURL(ctx, row.WebhookUrl); err != nil {
		a.log.Error("webhook url failed SSRF re-validation, skipping delivery",
			"job_id", p.JobID, "error", err)
		return nil
	}

	body := webhook.Payload{
		JobID:    p.JobID,
		Status:   "success",
		ShortURL: a.cfg.ShortURLBase + "/" + row.Slug.String,
		QRCode: webhook.QRCode{
			DownloadURL: downloadURL,
			ExpiresAt:   time.Now().Add(a.cfg.SignedURLTTL).UTC().Format(time.RFC3339),
			SizeBytes:   size,
		},
		OriginalURL: row.OriginalUrl,
		CreatedAt:   row.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
	if err := a.dispatcher.Deliver(ctx, row.WebhookUrl, apiKey.WebhookSecret, apiKey.KeyHint, body); err != nil {
		return fmt.Errorf("deliver webhook for %s: %w", p.JobID, err)
	}
	a.log.Info("webhook delivered", "job_id", p.JobID, "target", row.WebhookUrl)
	return nil
}

// failJob logs the cause, marks the short_urls row failed, and returns the
// cause so the queue logs it too.
func (a *app) failJob(ctx context.Context, jobID string, cause error) error {
	a.log.Error("shorten job failed", "job_id", jobID, "error", cause)
	if _, err := a.queries.FailShortURL(ctx, jobID); err != nil {
		a.log.Error("mark job failed", "job_id", jobID, "error", err)
	}
	return cause
}

// isUniqueViolation reports whether err is a Postgres unique-constraint error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
