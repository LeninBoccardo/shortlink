package observer

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/httpx"
)

// IngestBuffer is the size of the channel between the ingest handler and the
// aggregator goroutine. SPEC §4.3 calls for 1000. If it's full /ingest drops
// the event (favouring liveness over completeness) and bumps the drop counter.
const IngestBuffer = 1000

// Hub owns the observer's runtime: the State, the event channel, and the
// goroutines that consume/prune it. Routes are registered on an http.ServeMux
// returned by Routes().
type Hub struct {
	state    *State
	ch       chan events.Event
	log      *slog.Logger
	stop     chan struct{}
	done     chan struct{}
	dropped  atomic.Int64
	received atomic.Int64
}

// NewHub returns a Hub with a fresh State. Call Start to launch the aggregator.
func NewHub(log *slog.Logger) *Hub {
	return &Hub{
		state: NewState(),
		ch:    make(chan events.Event, IngestBuffer),
		log:   log,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// State exposes the underlying State so other components (poller, broadcaster)
// can read snapshots and inject system-wide values.
func (h *Hub) State() *State { return h.state }

// Enqueue submits an event for aggregation. Non-blocking — dropped on overflow.
// Used by the Redis poller to inject queue_depth_high / dlq_nonempty.
func (h *Hub) Enqueue(ev events.Event) {
	if ev.Source == "" {
		ev.Source = events.SourceObserver
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	if ev.ID == "" {
		ev.ID = "evt_obs_" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	select {
	case h.ch <- ev:
	default:
		h.dropped.Add(1)
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
	mux.HandleFunc("/metrics", h.handleMetrics)
	return mux
}

func (h *Hub) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var ev events.Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&ev); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "malformed JSON event")
		return
	}
	if ev.Kind == "" {
		httpx.WriteError(w, http.StatusBadRequest, "event missing kind")
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	h.received.Add(1)
	select {
	case h.ch <- ev:
	default:
		h.dropped.Add(1)
		h.log.Warn("observer ingest buffer full, dropping event", "kind", ev.Kind)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Hub) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMetrics is the Prometheus stub for M4. Full collectors land in M7 —
// for now we only expose the one counter SPEC §4.3 explicitly names.
func (h *Hub) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	body := "" +
		"# HELP observer_events_dropped_total Events dropped by the observer ingest buffer.\n" +
		"# TYPE observer_events_dropped_total counter\n" +
		"observer_events_dropped_total " + strconv.FormatInt(h.dropped.Load(), 10) + "\n" +
		"# HELP observer_events_received_total Events accepted at /ingest.\n" +
		"# TYPE observer_events_received_total counter\n" +
		"observer_events_received_total " + strconv.FormatInt(h.received.Load(), 10) + "\n"
	_, _ = w.Write([]byte(body))
}
