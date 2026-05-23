// Package observer holds the observer hub's in-memory state and goroutines:
// the aggregator that consumes events from /ingest, the Redis poller that
// derives system-wide stats, and the WebSocket broadcaster.
//
// All state is in-memory only (SPEC §4.3) — on restart the observer starts
// empty and rebuilds from incoming events. This is acceptable for a demo /
// operability tool and lets us skip persistence entirely.
package observer

import (
	"sort"
	"sync"
	"time"

	"github.com/leninboccardo/shortlink/internal/events"
)

// LogEntry mirrors SPEC §4.3 — what the browser ring-buffer renders.
type LogEntry struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"ts"`
	ExpiresAt  time.Time      `json:"expires_at"`
	Source     string         `json:"source"`
	Level      string         `json:"level"`
	Kind       string         `json:"kind"`
	APIKeyHash string         `json:"api_key_hash,omitempty"`
	APIKeyHint string         `json:"api_key_hint,omitempty"`
	Message    string         `json:"message"`
	Meta       map[string]any `json:"meta,omitempty"`
}

// KeyStat is the per-API-key counter row in the showcase table.
type KeyStat struct {
	KeyHash     string    `json:"key_hash"`
	KeyHint     string    `json:"key_hint"`
	Tier        string    `json:"tier"`
	RateLimit   int       `json:"rate_limit"`
	TotalReqs   int64     `json:"total_reqs"`
	Webhooks    int64     `json:"webhooks"`
	LimitErrors int64     `json:"limit_errors"`
	JobErrors   int64     `json:"job_errors"`
	P99Latency  int64     `json:"p99_latency_ms"`
	LastSeen    time.Time `json:"last_seen"`

	// latencySamples holds raw duration_ms values from request_completed events
	// within the rolling p99 window. Pruned in-place by recomputeP99.
	latencySamples []latencySample `json:"-"`
}

type latencySample struct {
	at time.Time
	ms int64
}

// SystemStat is the aggregate the showcase header renders.
type SystemStat struct {
	ActivePods    int     `json:"active_pods"`
	QueueDepth    int64   `json:"queue_depth"`
	TotalJobs     int64   `json:"total_jobs"`
	ErrorRate     float64 `json:"error_rate"`
	UptimeSeconds int64   `json:"uptime_s"`

	// totalErrors is the cumulative (job_error + job_dlq) used as the numerator
	// for ErrorRate. Held here so it survives between recomputes without a
	// global counter map.
	totalErrors int64 `json:"-"`
}

// State is the observer's mutable shared state. Read it via methods only
// (snapshot helpers below) — they take the mutex.
type State struct {
	mu          sync.Mutex
	startedAt   time.Time
	keyStats    map[string]*KeyStat
	logs        []LogEntry // newest-first ring buffer, max LogRingSize
	system      SystemStat
	updatedAt   time.Time
	logsAppendN int64 // tail-cursor for broadcaster diff (logs since last tick)
}

// Tunables (SPEC §4.3).
const (
	LogRingSize   = 500
	LatencyWindow = 60 * time.Second
	defaultLogTTL = 2 * time.Minute
	UntilPodStops = 24 * time.Hour // pod_started TTL — "until the pod reports stopped"
)

// logTTLs maps an event kind to its retention in the log ring (SPEC §4.3).
var logTTLs = map[string]time.Duration{
	events.KindAuthFailure:    15 * time.Minute,
	events.KindRateLimitHit:   5 * time.Minute,
	events.KindJobEnqueued:    2 * time.Minute,
	events.KindJobComplete:    2 * time.Minute,
	events.KindJobError:       10 * time.Minute,
	events.KindJobDLQ:         15 * time.Minute,
	events.KindWebhookSent:    2 * time.Minute,
	events.KindWebhookFailed:  10 * time.Minute,
	events.KindPodStarted:     UntilPodStops,
	events.KindPodStopped:     5 * time.Minute,
	events.KindQueueDepthHigh: 1 * time.Minute,
	events.KindDLQNonempty:    15 * time.Minute,
	// loadtest kinds default to 2m via defaultLogTTL until M5 wires them.
}

// loggedKinds is the set of kinds that are appended to the log ring; the
// stat-only kinds (request_completed) update counters but never log.
var loggedKinds = map[string]bool{
	events.KindAuthFailure:    true,
	events.KindRateLimitHit:   true,
	events.KindJobEnqueued:    true,
	events.KindJobComplete:    true,
	events.KindJobError:       true,
	events.KindJobDLQ:         true,
	events.KindWebhookSent:    true,
	events.KindWebhookFailed:  true,
	events.KindPodStarted:     true,
	events.KindPodStopped:     true,
	events.KindQueueDepthHigh: true,
	events.KindDLQNonempty:    true,
}

// NewState returns an empty State with start time set to now.
func NewState() *State {
	return &State{
		startedAt: time.Now(),
		keyStats:  make(map[string]*KeyStat),
		logs:      make([]LogEntry, 0, LogRingSize),
		updatedAt: time.Now(),
	}
}

// Ingest applies one event to the state — updating per-key counters, the log
// ring buffer, and system-wide counters as appropriate.
func (s *State) Ingest(ev events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.updatedAt = now

	if ev.APIKeyHash != "" {
		ks := s.keyStats[ev.APIKeyHash]
		if ks == nil {
			ks = &KeyStat{KeyHash: ev.APIKeyHash, KeyHint: ev.APIKeyHint}
			s.keyStats[ev.APIKeyHash] = ks
		}
		if ks.KeyHint == "" && ev.APIKeyHint != "" {
			ks.KeyHint = ev.APIKeyHint
		}
		if tier, ok := ev.Meta["tier"].(string); ok && tier != "" {
			ks.Tier = tier
		}
		if rl, ok := metaInt(ev.Meta, "rate_limit"); ok {
			ks.RateLimit = rl
		}
		ks.LastSeen = ev.Timestamp

		switch ev.Kind {
		case events.KindRequestCompleted:
			ks.TotalReqs++
			if ms, ok := metaInt64(ev.Meta, "duration_ms"); ok {
				ks.latencySamples = append(ks.latencySamples, latencySample{at: now, ms: ms})
			}
		case events.KindWebhookSent:
			ks.Webhooks++
		case events.KindRateLimitHit:
			ks.LimitErrors++
		case events.KindJobError, events.KindJobDLQ:
			ks.JobErrors++
		}
	}

	switch ev.Kind {
	case events.KindJobComplete:
		s.system.TotalJobs++
	case events.KindJobError, events.KindJobDLQ:
		s.system.totalErrors++
	}

	if loggedKinds[ev.Kind] {
		s.appendLog(ev, now)
	}
}

func (s *State) appendLog(ev events.Event, now time.Time) {
	ttl := logTTLs[ev.Kind]
	if ttl == 0 {
		ttl = defaultLogTTL
	}
	entry := LogEntry{
		ID:         ev.ID,
		Timestamp:  ev.Timestamp,
		ExpiresAt:  now.Add(ttl),
		Source:     ev.Source,
		Level:      ev.Level,
		Kind:       ev.Kind,
		APIKeyHash: ev.APIKeyHash,
		APIKeyHint: ev.APIKeyHint,
		Message:    ev.Message,
		Meta:       ev.Meta,
	}
	// Prepend (newest-first); cap at LogRingSize.
	s.logs = append([]LogEntry{entry}, s.logs...)
	if len(s.logs) > LogRingSize {
		s.logs = s.logs[:LogRingSize]
	}
	s.logsAppendN++
}

// Prune drops expired log entries and old latency samples. Call periodically
// from the aggregator's ticker.
func (s *State) Prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.logs) > 0 {
		live := s.logs[:0]
		for _, l := range s.logs {
			if !now.After(l.ExpiresAt) {
				live = append(live, l)
			}
		}
		s.logs = live
	}
	cutoff := now.Add(-LatencyWindow)
	for _, ks := range s.keyStats {
		if len(ks.latencySamples) == 0 {
			continue
		}
		kept := ks.latencySamples[:0]
		for _, sample := range ks.latencySamples {
			if !sample.at.Before(cutoff) {
				kept = append(kept, sample)
			}
		}
		ks.latencySamples = kept
		ks.P99Latency = computeP99(ks.latencySamples)
	}
	// Update derived system fields.
	s.system.UptimeSeconds = int64(now.Sub(s.startedAt).Seconds())
	if s.system.TotalJobs > 0 {
		s.system.ErrorRate = float64(s.system.totalErrors) / float64(s.system.TotalJobs)
	} else {
		s.system.ErrorRate = 0
	}
}

// SetPodCount + SetQueueDepth are written by the Redis poller. They snap into
// place under the same lock as Ingest.
func (s *State) SetPodCount(n int) {
	s.mu.Lock()
	s.system.ActivePods = n
	s.mu.Unlock()
}

func (s *State) SetQueueDepth(n int64) {
	s.mu.Lock()
	s.system.QueueDepth = n
	s.mu.Unlock()
}

// clearLogs wipes the server-side log ring (responds to a browser clear_logs
// command). The broadcaster sends every connected client a reset frame.
func (s *State) clearLogs() {
	s.mu.Lock()
	s.logs = s.logs[:0]
	s.mu.Unlock()
}

// resetStats wipes per-key counters and the system-wide totals. Pod count and
// queue depth are re-derived on the next poller tick.
func (s *State) resetStats() {
	s.mu.Lock()
	s.keyStats = make(map[string]*KeyStat)
	s.system.TotalJobs = 0
	s.system.totalErrors = 0
	s.system.ErrorRate = 0
	s.mu.Unlock()
}

// Snapshot returns deep copies safe to ship over the wire without holding the
// lock. Callers must not mutate the result.
func (s *State) Snapshot() (keys []KeyStat, logs []LogEntry, system SystemStat, ts time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys = make([]KeyStat, 0, len(s.keyStats))
	for _, ks := range s.keyStats {
		ksCopy := *ks
		ksCopy.latencySamples = nil // exported snapshot doesn't carry raw samples
		keys = append(keys, ksCopy)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].KeyHash < keys[j].KeyHash })
	logs = make([]LogEntry, len(s.logs))
	copy(logs, s.logs)
	return keys, logs, s.system, s.updatedAt
}

// LogsSince returns all log entries appended after lastSeenN, and the new
// cursor value. Used by the broadcaster to send only the delta each tick.
func (s *State) LogsSince(lastSeenN int64) (logs []LogEntry, cursor int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor = s.logsAppendN
	if lastSeenN >= cursor {
		return nil, cursor
	}
	delta := cursor - lastSeenN
	if delta > int64(len(s.logs)) {
		delta = int64(len(s.logs))
	}
	logs = make([]LogEntry, delta)
	copy(logs, s.logs[:delta])
	return logs, cursor
}

func computeP99(samples []latencySample) int64 {
	if len(samples) == 0 {
		return 0
	}
	xs := make([]int64, len(samples))
	for i, s := range samples {
		xs[i] = s.ms
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	// nearest-rank p99: ceil(0.99 * n) — never out of bounds.
	idx := (99*len(xs) + 99) / 100
	if idx > len(xs) {
		idx = len(xs)
	}
	if idx < 1 {
		idx = 1
	}
	return xs[idx-1]
}

func metaInt(m map[string]any, key string) (int, bool) {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		}
	}
	return 0, false
}

func metaInt64(m map[string]any, key string) (int64, bool) {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		}
	}
	return 0, false
}
