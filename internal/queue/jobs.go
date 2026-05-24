package queue

// Job type tags. The same tags are reused by the Redis/asynq queue in M2.
const (
	TypeShorten = "shorten"
	TypeWebhook = "webhook"
)

// ShortenJobPayload is the work item for the shorten pipeline (SPEC §7).
// APIKeyHash + APIKeyHint ride along so the worker can attribute
// job_complete / job_error / job_dlq events without an extra DB round-trip.
type ShortenJobPayload struct {
	JobID       string `json:"job_id"`
	OriginalURL string `json:"original_url"`
	WebhookURL  string `json:"webhook_url"`
	APIKeyHash  string `json:"api_key_hash"`
	APIKeyHint  string `json:"api_key_hint"`
	CustomSlug  string `json:"custom_slug,omitempty"`
	EnqueuedAt  int64  `json:"enqueued_at"`
}

// WebhookJobPayload is the work item for webhook delivery. It carries only the
// job ID; the worker reloads everything else from Postgres so the delivery
// always uses current data (SPEC §7).
type WebhookJobPayload struct {
	JobID      string `json:"job_id"`
	EnqueuedAt int64  `json:"enqueued_at"`
}
