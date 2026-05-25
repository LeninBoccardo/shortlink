package auth

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/leninboccardo/shortlink/internal/db"
)

const (
	touchWorkers  = 4
	touchBuffer   = 2048
	touchCallTTL  = 2 * time.Second
)

// touchJob is one queued touch — the auth middleware drops these in
// non-blockingly; the worker pool drains them against Redis/PG.
type touchJob struct {
	keyHash string
	keyID   pgtype.UUID
}

// LastUsedToucher records that an API key was just used, but bumps
// api_keys.last_used_at at most once per `throttle` per key — gated by a Redis
// SETNX marker (SPEC §9 / LAST_USED_THROTTLE).
//
// Touch is fire-and-forget: it enqueues into a bounded buffer that a fixed
// worker pool drains, so a burst of authenticated requests can never spawn
// unbounded goroutines (the prior implementation did `go t.Touch(...)` per
// request). On a wedged Redis the buffer fills and excess touches are
// dropped — last_used_at staleness is the acceptable failure mode.
type LastUsedToucher struct {
	queries  *db.Queries
	redis    *redis.Client
	throttle time.Duration
	log      *slog.Logger

	ch      chan touchJob
	done    chan struct{}
	wg      sync.WaitGroup
	stopped atomic.Bool
}

// NewLastUsedToucher returns a toucher; pass nil rc to disable the touch.
// The worker pool launches immediately.
func NewLastUsedToucher(q *db.Queries, rc *redis.Client, throttle time.Duration, log *slog.Logger) *LastUsedToucher {
	t := &LastUsedToucher{
		queries:  q,
		redis:    rc,
		throttle: throttle,
		log:      log,
		ch:       make(chan touchJob, touchBuffer),
		done:     make(chan struct{}),
	}
	if rc != nil {
		for i := 0; i < touchWorkers; i++ {
			t.wg.Add(1)
			go t.worker()
		}
	}
	return t
}

// Touch enqueues a touch for the given key. Never blocks: if the recorder
// has been shut down, or the buffer is full, the touch is dropped. The
// caller can fire this on the request path with zero latency cost — the
// actual Redis SETNX + PG UPDATE happens on a pool goroutine.
func (t *LastUsedToucher) Touch(keyHash string, keyID pgtype.UUID) {
	if t == nil || t.redis == nil || t.stopped.Load() {
		return
	}
	select {
	case t.ch <- touchJob{keyHash: keyHash, keyID: keyID}:
	default:
		// Buffer full — drop. Logged at debug so a wedged Redis doesn't
		// flood the log; the staleness is observable via last_used_at.
		t.log.Debug("last-used toucher buffer full, dropping", "key_hint", keyHash)
	}
}

// worker drains the queue. Same select-on-done pattern as cmd/api/hitRecorder
// — `ch` is never closed (avoids send-on-closed-channel races with Touch
// callers); workers exit when `done` is closed and the buffer is drained.
func (t *LastUsedToucher) worker() {
	defer t.wg.Done()
	for {
		select {
		case job := <-t.ch:
			t.do(job)
		case <-t.done:
			for {
				select {
				case job := <-t.ch:
					t.do(job)
				default:
					return
				}
			}
		}
	}
}

// do performs the actual SETNX + (on miss) UPDATE under a fresh, bounded
// context — never the request's. Errors are logged at warn (best-effort).
func (t *LastUsedToucher) do(job touchJob) {
	ctx, cancel := context.WithTimeout(context.Background(), touchCallTTL)
	defer cancel()
	ok, err := t.redis.SetNX(ctx, "shortlink:lu:"+job.keyHash, "1", t.throttle).Result()
	if err != nil {
		t.log.Warn("last-used throttle check", "error", err)
		return
	}
	if !ok {
		return // already touched within the throttle window
	}
	if err := t.queries.UpdateLastUsedAt(ctx, job.keyID); err != nil {
		t.log.Warn("update last_used_at", "error", err)
	}
}

// Shutdown stops intake and drains buffered touches. Safe to call from the
// graceful-shutdown sequence; uses the same race-free pattern as the api's
// hitRecorder (atomic stop flag + done channel, no close on data channel).
func (t *LastUsedToucher) Shutdown() {
	if t == nil || t.redis == nil {
		return
	}
	t.stopped.Store(true)
	close(t.done)
	t.wg.Wait()
}
