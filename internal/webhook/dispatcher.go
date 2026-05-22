package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DeliveryTimeout bounds a single webhook POST attempt.
const DeliveryTimeout = 10 * time.Second

// maxResponseBody caps how much of a client's response we read.
const maxResponseBody = 64 << 10

// QRCode is the nested qr_code object of the webhook payload (SPEC §8).
type QRCode struct {
	DownloadURL string `json:"download_url"`
	ExpiresAt   string `json:"expires_at"`
	SizeBytes   int64  `json:"size_bytes"`
}

// Payload is the success webhook body (SPEC §8).
type Payload struct {
	JobID       string `json:"job_id"`
	Status      string `json:"status"`
	ShortURL    string `json:"short_url"`
	QRCode      QRCode `json:"qr_code"`
	OriginalURL string `json:"original_url"`
	CreatedAt   string `json:"created_at"`
}

// Dispatcher POSTs signed webhook payloads through an SSRF-safe HTTP client.
type Dispatcher struct {
	client *http.Client
}

// NewDispatcher wraps an (SSRF-safe) HTTP client for webhook delivery.
func NewDispatcher(client *http.Client) *Dispatcher {
	return &Dispatcher{client: client}
}

// Deliver signs and POSTs payload to target. Milestone 1 makes a single
// attempt; the retry schedule and dead-letter queue arrive in M2.
func (d *Dispatcher) Deliver(ctx context.Context, target, secret, keyHint string, payload Payload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ShortLink-Signature", Sign(secret, body))
	req.Header.Set("X-ShortLink-Job-ID", payload.JobID)
	req.Header.Set("X-ShortLink-Key-Hint", keyHint)

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}
