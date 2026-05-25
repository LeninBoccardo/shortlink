// Package events is the best-effort emitter used by every ShortLink binary
// to push structured domain events to the observer hub (SPEC §10).
//
// Emit is non-blocking. Events are queued in a small bounded buffer and
// drained by a background goroutine that POSTs them to {OBSERVER_URL}/ingest
// with a short timeout. If the buffer is full or the observer is unreachable
// the event is dropped — emission must never block or fail a request or job.
package events

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Level constants for Event.Level.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Source constants for Event.Source.
const (
	SourceAPI      = "api"
	SourceWorker   = "worker"
	SourceObserver = "observer"
	SourceLoadtest = "loadtest"
)

// Event kinds (SPEC §10).
const (
	KindRequestCompleted = "request_completed"
	KindAuthFailure      = "auth_failure"
	KindRateLimitHit     = "rate_limit_hit"
	KindJobEnqueued      = "job_enqueued"
	KindJobComplete      = "job_complete"
	KindJobError         = "job_error"
	KindJobDLQ           = "job_dlq"
	KindWebhookSent      = "webhook_sent"
	KindWebhookFailed    = "webhook_failed"
	KindPodStarted       = "pod_started"
	KindPodStopped       = "pod_stopped"
	KindQueueDepthHigh   = "queue_depth_high"
	KindDLQNonempty      = "dlq_nonempty"
	KindAttackStarted    = "attack_started"
	KindAttackComplete   = "attack_complete"
)

// Event is the wire envelope POSTed to /ingest (SPEC §10).
type Event struct {
	ID         string         `json:"id"`
	Source     string         `json:"source"`
	Level      string         `json:"level"`
	Kind       string         `json:"kind"`
	APIKeyHash string         `json:"api_key_hash,omitempty"`
	APIKeyHint string         `json:"api_key_hint,omitempty"`
	Message    string         `json:"message"`
	Meta       map[string]any `json:"meta,omitempty"`
	Timestamp  time.Time      `json:"ts"`
}

// Emitter ships events to the observer hub asynchronously.
type Emitter struct {
	url    string
	token  string
	source string
	client *http.Client
	log    *slog.Logger
	ch     chan Event
	wg     sync.WaitGroup
	stop   chan struct{}
	once   sync.Once
}

// Config tunes the emitter buffer + HTTP timeout. Sensible defaults are applied.
type Config struct {
	URL        string        // base URL of the observer (e.g. http://localhost:9000)
	Token      string        // OBSERVER_INGEST_TOKEN; sent as Authorization: Bearer
	Source     string        // SourceAPI / SourceWorker / ...
	BufferSize int           // bounded channel size; default 256
	Timeout    time.Duration // per-POST timeout; default 500ms
	Logger     *slog.Logger
}

// NewEmitter constructs an Emitter and starts its background worker. Call
// Close on shutdown to drain the buffer with a bounded grace period.
func NewEmitter(cfg Config) *Emitter {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 256
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 500 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	e := &Emitter{
		url:    cfg.URL,
		token:  cfg.Token,
		source: cfg.Source,
		client: &http.Client{Timeout: cfg.Timeout},
		log:    cfg.Logger,
		ch:     make(chan Event, cfg.BufferSize),
		stop:   make(chan struct{}),
	}
	e.wg.Add(1)
	go e.run()
	return e
}

// Emit enqueues ev for asynchronous delivery. Missing ID / Timestamp / Source
// are filled in. If the buffer is full the event is dropped silently — the
// caller is never blocked. After Close, further Emits are dropped (the
// background goroutine has exited and nothing would send them).
func (e *Emitter) Emit(ev Event) {
	if ev.ID == "" {
		ev.ID = NewEventID()
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	if ev.Source == "" {
		ev.Source = e.source
	}
	select {
	case <-e.stop:
		return
	default:
	}
	select {
	case e.ch <- ev:
	default:
		// Buffer full — drop. Logged at debug to avoid log spam under load.
		e.log.Debug("event dropped: emitter buffer full", "kind", ev.Kind)
	}
}

// Close stops the background worker. It drains the channel for up to ~grace
// before returning; events still in flight after that are abandoned.
func (e *Emitter) Close(grace time.Duration) {
	e.once.Do(func() {
		close(e.stop)
	})
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

func (e *Emitter) run() {
	defer e.wg.Done()
	for {
		select {
		case ev := <-e.ch:
			e.send(ev)
		case <-e.stop:
			// Drain whatever is already buffered, then exit. New emissions
			// after Close still go into the channel but won't be sent.
			for {
				select {
				case ev := <-e.ch:
					e.send(ev)
				default:
					return
				}
			}
		}
	}
}

func (e *Emitter) send(ev Event) {
	if e.url == "" {
		return
	}
	body, err := json.Marshal(ev)
	if err != nil {
		e.log.Debug("event marshal failed", "error", err, "kind", ev.Kind)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/ingest", bytes.NewReader(body))
	if err != nil {
		e.log.Debug("event request build failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		// Observer unreachable. Expected when the observer isn't running yet.
		return
	}
	// Drain the body so the underlying TCP conn returns to the keep-alive
	// pool. Without this, Go's http.Transport refuses to reuse the conn and
	// each event burns a fresh socket+TLS handshake.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// NewEventID returns a fresh ULID-based event ID with the canonical "evt_"
// prefix. Exported so other packages (e.g. observer.Hub.Enqueue) can stamp
// IDs in the same format as Emit produces.
func NewEventID() string {
	id, err := ulid.New(ulid.Timestamp(time.Now()), rand.Reader)
	if err != nil {
		return ""
	}
	return "evt_" + id.String()
}
