package observer

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/httpx"
	"github.com/leninboccardo/shortlink/internal/metrics"
)

// IngestBuffer is the size of the channel between the ingest handler and the
// aggregator goroutine. SPEC §4.3 calls for 1000. If it's full /ingest drops
// the event (favouring liveness over completeness) and bumps the drop counter.
const IngestBuffer = 1000

// Hub owns the observer's runtime: the State, the event channel, and the
// goroutines that consume/prune it. Routes are registered on an http.ServeMux
// returned by Routes().
type Hub struct {
	state       *State
	ch          chan events.Event
	log         *slog.Logger
	ingestToken string
	stop        chan struct{}
	done        chan struct{}
}

// NewHub returns a Hub with a fresh State. ingestToken is the required value
// of the Authorization: Bearer header on /ingest; empty string keeps /ingest
// open (the local-dev default).
func NewHub(ingestToken string, log *slog.Logger) *Hub {
	return &Hub{
		state:       NewState(),
		ch:          make(chan events.Event, IngestBuffer),
		log:         log,
		ingestToken: ingestToken,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// State exposes the underlying State so other components (poller, broadcaster)
// can read snapshots and inject system-wide values.
func (h *Hub) State() *State { return h.state }

// Enqueue submits an event for aggregation. Non-blocking — dropped on overflow.
// Used by the Redis poller to inject queue_depth_high / dlq_nonempty. After
// Shutdown returns, further Enqueues are dropped (the aggregator goroutine
// has exited and nothing would consume them).
func (h *Hub) Enqueue(ev events.Event) {
	if ev.Source == "" {
		ev.Source = events.SourceObserver
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	if ev.ID == "" {
		ev.ID = events.NewEventID()
	}
	select {
	case <-h.stop:
		metrics.EventsDroppedTotal.WithLabelValues(metrics.SourceObserver).Inc()
		return
	default:
	}
	select {
	case h.ch <- ev:
	default:
		metrics.EventsDroppedTotal.WithLabelValues(metrics.SourceObserver).Inc()
	}
}

// Start launches the aggregator goroutine.
func (h *Hub) Start() {
	go h.run()
}

// Shutdown stops the aggregator and waits for it to drain.
func (h *Hub) Shutdown(ctx context.Context) error {
	close(h.stop)
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the aggregator: it ingests events as they arrive and prunes the log
// ring + latency window every 100 ms (SPEC §4.3).
func (h *Hub) run() {
	defer close(h.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case ev := <-h.ch:
			h.state.Ingest(ev)
		case <-ticker.C:
			h.state.Prune(time.Now())
		case <-h.stop:
			// Drain any buffered events before exiting.
			for {
				select {
				case ev := <-h.ch:
					h.state.Ingest(ev)
				default:
					return
				}
			}
		}
	}
}

// Routes returns an http.Handler with /ingest, /healthz, and /metrics.
// /stream is registered by the WebSocket broadcaster in a later commit.
func (h *Hub) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest", h.handleIngest)
	mux.HandleFunc("/healthz", h.handleHealth)
	mux.Handle("/metrics", metrics.Handler())
	return mux
}

func (h *Hub) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		metrics.EventsRejectedTotal.WithLabelValues(metrics.EventRejectReasonMethod).Inc()
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.checkIngestAuth(r) {
		metrics.EventsRejectedTotal.WithLabelValues(metrics.EventRejectReasonAuth).Inc()
		httpx.WriteError(w, http.StatusUnauthorized, "missing or invalid ingest token")
		return
	}
	var ev events.Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&ev); err != nil {
		metrics.EventsRejectedTotal.WithLabelValues(metrics.EventRejectReasonBadJSON).Inc()
		httpx.WriteError(w, http.StatusBadRequest, "malformed JSON event")
		return
	}
	if ev.Kind == "" {
		metrics.EventsRejectedTotal.WithLabelValues(metrics.EventRejectReasonMissingKind).Inc()
		httpx.WriteError(w, http.StatusBadRequest, "event missing kind")
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	metrics.EventsReceivedTotal.Inc()
	select {
	case h.ch <- ev:
	default:
		metrics.EventsDroppedTotal.WithLabelValues(sourceLabel(ev.Source)).Inc()
		h.log.Warn("observer ingest buffer full, dropping event", "kind", ev.Kind)
	}
	w.WriteHeader(http.StatusAccepted)
}

// sourceLabel returns a stable, low-cardinality source label for metrics.
// Events with unknown/missing Source land in "other" rather than spawning
// an unbounded label space if a misconfigured emitter sends garbage.
func sourceLabel(s string) string {
	switch s {
	case metrics.SourceAPI, metrics.SourceWorker, metrics.SourceObserver, metrics.SourceLoadtest:
		return s
	default:
		return "other"
	}
}

func (h *Hub) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// checkIngestAuth returns true iff the request carries the expected bearer
// token, or if no token was configured (local-dev default — /ingest open).
// Constant-time compare so we don't leak token-byte info via timing.
func (h *Hub) checkIngestAuth(r *http.Request) bool {
	if h.ingestToken == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.ingestToken)) == 1
}
