// Package metrics centralises Prometheus collectors used across ShortLink
// binaries (SPEC §17 Milestone 7). Each binary registers a /metrics endpoint
// via Handler(); Prometheus scrapes them and Grafana renders the dashboards
// provisioned in deploy/grafana/.
//
// Labels are deliberately low-cardinality (SPEC §17 Milestone 7): tier, status,
// queue, decision, source. Never api_key / key_hash / URL — those go through
// the observer hub instead.
//
// Collectors are registered via promauto on the default registry. A binary
// that never increments a given series will still expose it as zero — that's
// fine; Prometheus stores zero, dashboards usually rate() it away. Splitting
// per-binary would buy little.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Status / decision / queue label values. Exported so call sites use the same
// strings — typos in label values produce silent extra series.
const (
	JobStatusComplete = "complete"
	JobStatusError    = "error"
	JobStatusDLQ      = "dlq"

	QueueShorten = "shorten"
	QueueWebhook = "webhook"

	RateDecisionAllowed = "allowed"
	RateDecisionLimited = "limited"

	WebhookStatusSuccess = "success"
	WebhookStatusFailure = "failure"

	ShortenStatusAccepted           = "accepted"
	ShortenStatusRejectedAuth       = "rejected_auth"
	ShortenStatusRejectedRateLimit  = "rejected_rate_limit"
	ShortenStatusRejectedValidation = "rejected_validation"
	ShortenStatusRejectedConflict   = "rejected_conflict"
	ShortenStatusInternalError      = "internal_error"
	ShortenStatusUnknown            = "unknown"

	SourceAPI      = "api"
	SourceWorker   = "worker"
	SourceObserver = "observer"
	SourceLoadtest = "loadtest"
)

var (
	JobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_jobs_total",
		Help: "Asynq job outcomes by queue and terminal status.",
	}, []string{"queue", "status"})

	JobDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "shortlink_job_duration_seconds",
		Help:    "Asynq job execution duration per queue.",
		Buckets: prometheus.DefBuckets,
	}, []string{"queue"})

	QRGenerateDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "shortlink_qr_generate_duration_seconds",
		Help:    "QR code generation duration.",
		Buckets: prometheus.DefBuckets,
	})

	RateLimitHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_rate_limit_hits_total",
		Help: "Per-tier rate-limit decisions.",
	}, []string{"tier", "decision"})

	WebhookAttemptsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_webhook_attempts_total",
		Help: "Outbound webhook attempts by status.",
	}, []string{"status"})

	WebhookDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "shortlink_webhook_duration_seconds",
		Help:    "Outbound webhook delivery duration.",
		Buckets: prometheus.DefBuckets,
	})

	ShortenRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_shorten_requests_total",
		Help: "POST /shorten request outcomes.",
	}, []string{"status"})

	// Active worker pods, set by the observer's Redis poller from the
	// pod:*:alive heartbeat scan. Other binaries leave this at 0.
	ActivePods = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "shortlink_active_pods",
		Help: "Number of live worker pods (Redis heartbeats).",
	})

	// Events --- mostly observer-side. EventsDroppedTotal is also bumped by
	// the observer when its ingest buffer overflows (emitter-side drops are
	// fire-and-forget and not counted here).
	EventsReceivedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shortlink_events_received_total",
		Help: "Events accepted by the observer ingest endpoint.",
	})

	EventsRejectedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shortlink_events_rejected_total",
		Help: "Ingest requests rejected for missing/bad auth.",
	})

	EventsDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "shortlink_events_dropped_total",
		Help: "Events dropped due to ingest buffer overflow.",
	}, []string{"source"})

	// Queue / DLQ depth, set by the observer's Redis poller.
	QueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "shortlink_queue_depth",
		Help: "Current asynq queue depth by queue.",
	}, []string{"queue"})

	DLQDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "shortlink_dlq_depth",
		Help: "Current asynq archived (dead-letter) set depth.",
	})
)

// Handler returns the /metrics http.Handler serving collectors registered on
// the Prometheus default registry.
func Handler() http.Handler {
	return promhttp.Handler()
}
