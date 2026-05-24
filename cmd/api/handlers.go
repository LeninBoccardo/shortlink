package main

import (
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/oklog/ulid/v2"

	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/httpx"
	"github.com/leninboccardo/shortlink/internal/metrics"
	"github.com/leninboccardo/shortlink/internal/middleware"
	"github.com/leninboccardo/shortlink/internal/queue"
	"github.com/leninboccardo/shortlink/internal/shortener"
)

func (a *app) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(middleware.Logger(a.log))
	r.Use(chimw.Recoverer)

	r.Get("/healthz", a.handleHealth)
	r.Method(http.MethodGet, "/metrics", metrics.Handler())

	limitFor := func(tier string) int {
		switch tier {
		case "pro":
			return a.cfg.RateLimitPro
		case "unlimited":
			return 0
		default:
			return a.cfg.RateLimitFree
		}
	}
	r.With(
		middleware.Auth(a.validator, a.toucher, a.emitter, a.log),
		middleware.RateLimit(a.limiter, limitFor, a.emitter, a.log),
		middleware.Stat(a.emitter),
	).Post("/shorten", a.handleShorten)

	// The redirect path is public — no API key required.
	r.Get("/{slug}", a.handleRedirect)
	return r
}

func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type shortenRequest struct {
	URL        string `json:"url"`
	WebhookURL string `json:"webhook_url"`
	CustomSlug string `json:"custom_slug"`
	ExpiresIn  int64  `json:"expires_in"`
}

type shortenResponse struct {
	JobID   string `json:"job_id"`
	Message string `json:"message"`
}

// handleShorten validates the request, reserves a pending short_urls row, and
// enqueues a shorten job — returning 202 immediately (SPEC §4.1).
func (a *app) handleShorten(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := middleware.APIKey(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}

	var req shortenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		metrics.ShortenRequestsTotal.WithLabelValues(metrics.ShortenStatusRejectedValidation).Inc()
		httpx.WriteError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}

	if err := a.validateSubmittedURL(req.URL); err != nil {
		metrics.ShortenRequestsTotal.WithLabelValues(metrics.ShortenStatusRejectedValidation).Inc()
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve the effective webhook URL: request value, else the key default.
	webhookURL := strings.TrimSpace(req.WebhookURL)
	if webhookURL == "" && apiKey.WebhookUrl.Valid {
		webhookURL = apiKey.WebhookUrl.String
	}
	if webhookURL == "" {
		metrics.ShortenRequestsTotal.WithLabelValues(metrics.ShortenStatusRejectedValidation).Inc()
		httpx.WriteError(w, http.StatusBadRequest, "no webhook URL: none in request and no key default")
		return
	}
	if err := a.ssrf.ValidateURL(r.Context(), webhookURL); err != nil {
		a.log.Warn("webhook url rejected", "error", err)
		metrics.ShortenRequestsTotal.WithLabelValues(metrics.ShortenStatusRejectedValidation).Inc()
		httpx.WriteError(w, http.StatusUnprocessableEntity, "webhook URL failed SSRF validation")
		return
	}

	customSlug := strings.TrimSpace(req.CustomSlug)
	if customSlug != "" {
		if err := shortener.ValidateCustomSlug(customSlug); err != nil {
			metrics.ShortenRequestsTotal.WithLabelValues(metrics.ShortenStatusRejectedValidation).Inc()
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if req.ExpiresIn < 0 || req.ExpiresIn > maxExpiresIn {
		metrics.ShortenRequestsTotal.WithLabelValues(metrics.ShortenStatusRejectedValidation).Inc()
		httpx.WriteError(w, http.StatusBadRequest, "expires_in out of range")
		return
	}
	var expiresAt pgtype.Timestamptz
	if req.ExpiresIn > 0 {
		expiresAt = pgtype.Timestamptz{
			Time:  time.Now().Add(time.Duration(req.ExpiresIn) * time.Second),
			Valid: true,
		}
	}

	jobID, err := newJobID()
	if err != nil {
		a.log.Error("generate job id", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Reserve the row. For a custom slug this also reserves the slug; a
	// conflict (zero rows) means the slug is already taken.
	if customSlug != "" {
		_, err = a.queries.InsertPendingShortURLWithSlug(r.Context(), db.InsertPendingShortURLWithSlugParams{
			JobID:       jobID,
			Slug:        pgtype.Text{String: customSlug, Valid: true},
			OriginalUrl: req.URL,
			ApiKeyID:    apiKey.ID,
			WebhookUrl:  webhookURL,
			ExpiresAt:   expiresAt,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusConflict, "custom slug already taken")
			return
		}
	} else {
		_, err = a.queries.InsertPendingShortURL(r.Context(), db.InsertPendingShortURLParams{
			JobID:       jobID,
			OriginalUrl: req.URL,
			ApiKeyID:    apiKey.ID,
			WebhookUrl:  webhookURL,
			ExpiresAt:   expiresAt,
		})
	}
	if err != nil {
		a.log.Error("reserve short_urls row", "error", err, "job_id", jobID)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	payload, err := json.Marshal(queue.ShortenJobPayload{
		JobID:       jobID,
		OriginalURL: req.URL,
		WebhookURL:  webhookURL,
		APIKeyHash:  apiKey.KeyHash,
		APIKeyHint:  apiKey.KeyHint,
		CustomSlug:  customSlug,
		EnqueuedAt:  time.Now().Unix(),
	})
	if err != nil {
		a.log.Error("marshal shorten job", "error", err, "job_id", jobID)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := a.queue.Enqueue(r.Context(), queue.Job{Type: queue.TypeShorten, Key: jobID, Payload: payload}); err != nil {
		a.log.Error("enqueue shorten job", "error", err, "job_id", jobID)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	a.emitter.Emit(events.Event{
		Level:      events.LevelInfo,
		Kind:       events.KindJobEnqueued,
		APIKeyHash: apiKey.KeyHash,
		APIKeyHint: apiKey.KeyHint,
		Message:    "shorten job accepted",
		Meta: map[string]any{
			"job_id":      jobID,
			"custom_slug": customSlug != "",
		},
	})
	metrics.ShortenRequestsTotal.WithLabelValues(metrics.ShortenStatusAccepted).Inc()

	httpx.WriteJSON(w, http.StatusAccepted, shortenResponse{
		JobID:   jobID,
		Message: "Job accepted. Result will be delivered to your webhook.",
	})
}

// handleRedirect resolves a slug and 302-redirects to the original URL,
// recording an analytics hit asynchronously (SPEC §4.1).
func (a *app) handleRedirect(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpx.WriteError(w, http.StatusNotFound, "short link not found")
		return
	}
	row, err := a.queries.GetActiveShortURLBySlug(r.Context(), pgtype.Text{String: slug, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "short link not found")
			return
		}
		a.log.Error("resolve slug", "error", err, "slug", slug)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	a.hits.record(slug, deviceFromUA(r.UserAgent()))
	http.Redirect(w, r, row.OriginalUrl, http.StatusFound)
}

// validateSubmittedURL checks the URL being shortened (SPEC §9, URL validation).
func (a *app) validateSubmittedURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	if len(raw) > maxURLLength {
		return fmt.Errorf("url exceeds %d characters", maxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url must be http or https")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return errors.New("url must have a host")
	}
	for _, blocked := range a.cfg.URLBlocklist {
		b := strings.ToLower(strings.TrimSpace(blocked))
		if b != "" && (host == b || strings.HasSuffix(host, "."+b)) {
			return errors.New("url domain is blocked")
		}
	}
	return nil
}

func deviceFromUA(ua string) string {
	switch {
	case ua == "":
		return ""
	case strings.Contains(ua, "Mobile"):
		return "mobile"
	default:
		return "desktop"
	}
}

// newJobID returns a fresh ULID-based job ID. crypto/rand entropy makes it
// safe for concurrent callers.
func newJobID() (string, error) {
	id, err := ulid.New(ulid.Timestamp(time.Now()), cryptorand.Reader)
	if err != nil {
		return "", fmt.Errorf("new ulid: %w", err)
	}
	return "job_" + id.String(), nil
}
