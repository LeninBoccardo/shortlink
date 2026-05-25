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
//
// APIKeyHash / KeyHash are kept server-side (used as the dedup map key in
// Ingest) but excluded from JSON: they're SHA-256(raw key) — the same value
// stored in the DB as the credential record — and there's no reason to
// broadcast them to anyone with the showcase open. The frontend keys on
// `api_key_hint` / `key_hint` instead.
type LogEntry struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"ts"`
	ExpiresAt  time.Time      `json:"expires_at"`
	Source     string         `json:"source"`
	Level      string         `json:"level"`
	Kind       string         `json:"kind"`
	APIKeyHash string         `json:"-"`
	APIKeyHint string         `json:"api_key_hint,omitempty"`
	Message    string         `json:"message"`
	Meta       map[string]any `json:"meta,omitempty"`
}

// KeyStat is the per-API-key counter row in the showcase table.
type KeyStat struct {
	KeyHash     string    `json:"-"`
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
//
// `logs` is a true circular buffer: `logs` is pre-allocated to LogRingSize
// at NewState and never reallocated; `logHead` points at the next write
// slot; `logCount` is the number of valid entries (0..LogRingSize). This
// makes appendLog O(1) without per-event allocation.
//
// Locking: a sync.RWMutex so multiple broadcaster tick goroutines (one per
// connected client) can read snapshots concurrently. Ingest, Prune, and the
// other mutators take the write lock.
type State struct {
	mu          sync.RWMutex
	startedAt   time.Time
	keyStats    map[string]*KeyStat
	logs        []LogEntry
	logHead     int
	logCount    int
	system      SystemStat
	updatedAt   time.Time
	logsAppendN int64 // monotonic cursor for broadcaster diff (logs since last tick)
}

// Tunables (SPEC §4.3).
const (
	LogRingSize   = 500
	LatencyWindow = 60 * time.Second
	defaultLogTTL = 2 * time.Minute
	UntilPodStops = 24 * time.Hour // pod_started TTL — "until the pod reports stopped"

	// keyStatsIdleEvict is the LastSeen threshold past which Prune drops a
	// KeyStat entry entirely. Keeps the keyStats map from growing without
	// bound for keys that stop being used (e.g. revoked test keys).
	keyStatsIdleEvict = 1 * time.Hour
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
	events.KindAttackStarted:  10 * time.Minute,
	events.KindAttackComplete: 30 * time.Minute,
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
	events.KindAttackStarted:  true,
	events.KindAttackComplete: true,
}

// NewState returns an empty State with start time set to now.
func NewState() *State {
	return &State{
		startedAt: time.Now(),
		keyStats:  make(map[string]*KeyStat),
		logs:      make([]LogEntry, LogRingSize),
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
	// Overwrite the next slot in the ring. The previous entry's Meta map is
	// now unreferenced and eligible for GC.
	s.logs[s.logHead] = LogEntry{
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
	s.logHead = (s.logHead + 1) % LogRingSize
	if s.logCount < LogRingSize {
		s.logCount++
	}
	s.logsAppendN++
}

// logIndex maps the i-th newest entry (0 = newest) to its position in the
// ring. Returns -1 if i is out of range.
func (s *State) logIndex(i int) int {
	if i < 0 || i >= s.logCount {
		return -1
	}
	return (s.logHead - 1 - i + LogRingSize) % LogRingSize
}

// Prune drops expired log entries and old latency samples. Call periodically
// from the aggregator's ticker.
func (s *State) Prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logCount > 0 {
		// Per-kind TTLs mean entries don't expire in chronological order, so
		// we can't early-stop on the first non-expired entry. Compact in-place
		// with two indices anchored at the ring's tail: the read index walks
		// every live entry oldest→newest, the write index advances only when
		// the read entry survives. Since write <= read at every step (both
		// relative to tail), we never overwrite an entry that hasn't been
		// read yet -- including across the ring's wraparound. No per-tick
		// allocation; the previous kept-slice churned ~10 allocs/s at idle.
		tail := (s.logHead - s.logCount + LogRingSize) % LogRingSize
		j := 0
		for i := 0; i < s.logCount; i++ {
			readIdx := (tail + i) % LogRingSize
			if now.After(s.logs[readIdx].ExpiresAt) {
				continue
			}
			writeIdx := (tail + j) % LogRingSize
			if writeIdx != readIdx {
				s.logs[writeIdx] = s.logs[readIdx]
			}
			j++
		}
		// Zero the now-vacant slots so dropped entries' Meta maps can be GC'd.
		for i := j; i < s.logCount; i++ {
			idx := (tail + i) % LogRingSize
			s.logs[idx] = LogEntry{}
		}
		s.logHead = (tail + j) % LogRingSize
		s.logCount = j
	}
	cutoff := now.Add(-LatencyWindow)
	idleCutoff := now.Add(-keyStatsIdleEvict)
	for hash, ks := range s.keyStats {
		// Drop entries for keys we haven't heard from in a while — keeps the
		// map bounded over multi-day uptimes (revoked test keys etc.).
		if !ks.LastSeen.IsZero() && ks.LastSeen.Before(idleCutoff) {
			delete(s.keyStats, hash)
			continue
		}
		if len(ks.latencySamples) == 0 {
			continue
		}
		kept := ks.latencySamples[:0]
		for _, sample := range ks.latencySamples {
			if !sample.at.Before(cutoff) {
				kept = append(kept, sample)
			}
		}
		// If the backing array has grown much larger than the live window,
		// reallocate to release the unused tail back to the GC.
		if cap(kept) > 4*len(kept) && cap(kept) > 64 {
			fresh := make([]latencySample, len(kept))
			copy(fresh, kept)
			kept = fresh
		}
		ks.latencySamples = kept
		// P99 is now recomputed lazily in snapshotKeysLocked — the sort cost
		// only fires on snapshot (broadcaster tick every 500ms, under RLock)
		// instead of every Prune tick (every 100ms, under write Lock). Cuts
		// sort cost 5x and stops blocking concurrent Ingest writers.
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
	// Zero the whole backing array, not [0, logCount): the live ring lives at
	// [logHead-logCount, logHead-1) mod LogRingSize, so the old slice indices
	// almost never lined up with the actual live range. Wiping the full
	// fixed-size array is cheap and lets each entry's Meta map be GC'd.
	for i := range s.logs {
		s.logs[i] = LogEntry{}
	}
	s.logHead = 0
	s.logCount = 0
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

// Snapshot returns deep copies of every part of state — full key stats, the
// entire log ring, and the system block. Used for the one-shot WS frame the
// broadcaster sends each newly-connected client. Hot-path code that ticks
// every 500ms should use StatsSnapshot instead so it doesn't pay the log copy.
func (s *State) Snapshot() (keys []KeyStat, logs []LogEntry, system SystemStat, ts time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys = s.snapshotKeysLocked()
	logs = make([]LogEntry, s.logCount)
	for i := 0; i < s.logCount; i++ {
		logs[i] = s.logs[s.logIndex(i)]
	}
	return keys, logs, s.system, s.updatedAt
}

// StatsSnapshot returns only the key stats + system block — no logs. The
// broadcaster ticks at 500ms × per-connected-client and only needs these
// fields in the periodic stats frame; logs are already shipped diff-only via
// LogsSince, so copying the whole log ring per tick was pure waste.
func (s *State) StatsSnapshot() (keys []KeyStat, system SystemStat, ts time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotKeysLocked(), s.system, s.updatedAt
}

// snapshotKeysLocked deep-copies keyStats into a sorted []KeyStat. Caller
// must hold s.mu (read lock is sufficient — snapshot doesn't mutate state).
//
// P99 is recomputed here rather than in Prune so the O(N log N) sort only
// runs at snapshot frequency (500ms) instead of Prune frequency (100ms),
// and under the read lock rather than the write lock — so concurrent
// Ingest writers aren't blocked. The lazy recompute uses the same samples
// that Prune has already truncated to LatencyWindow.
func (s *State) snapshotKeysLocked() []KeyStat {
	keys := make([]KeyStat, 0, len(s.keyStats))
	for _, ks := range s.keyStats {
		ksCopy := *ks
		ksCopy.P99Latency = computeP99(ks.latencySamples)
		ksCopy.latencySamples = nil // exported snapshot doesn't carry raw samples
		keys = append(keys, ksCopy)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].KeyHash < keys[j].KeyHash })
	return keys
}

// LogsSince returns up to (cursor - lastSeenN) newest log entries, and the
// new cursor. Used by the broadcaster to ship only the delta each tick.
// Returned logs are newest-first, matching Snapshot.
func (s *State) LogsSince(lastSeenN int64) (logs []LogEntry, cursor int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cursor = s.logsAppendN
	if lastSeenN >= cursor {
		return nil, cursor
	}
	delta := int(cursor - lastSeenN)
	if delta > s.logCount {
		delta = s.logCount
	}
	logs = make([]LogEntry, delta)
	for i := 0; i < delta; i++ {
		logs[i] = s.logs[s.logIndex(i)]
	}
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
