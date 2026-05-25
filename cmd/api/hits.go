package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leninboccardo/shortlink/internal/db"
)

const (
	hitWorkers  = 4
	hitBuffer   = 2048
	hitWriteTTL = 5 * time.Second
)

// hitEvent is one redirect to be recorded for analytics.
type hitEvent struct {
	slug   string
	device string
}

// hitRecorder writes redirect analytics off the request path through a fixed
// worker pool, so a redirect flood cannot spawn unbounded goroutines.
type hitRecorder struct {
	queries *db.Queries
	log     *slog.Logger
	ch      chan hitEvent
	wg      sync.WaitGroup
	// done is closed by shutdown() to signal workers to drain and exit.
	// We never close `ch` because a handler racing past srv.Shutdown's drain
	// could call record() and panic on a send-to-closed-channel.
	done    chan struct{}
	stopped atomic.Bool
}

// newHitRecorder starts the recorder's worker pool.
func newHitRecorder(queries *db.Queries, log *slog.Logger) *hitRecorder {
	hr := &hitRecorder{
		queries: queries,
		log:     log,
		ch:      make(chan hitEvent, hitBuffer),
		done:    make(chan struct{}),
	}
	for i := 0; i < hitWorkers; i++ {
		hr.wg.Add(1)
		go hr.worker()
	}
	return hr
}

// record queues a hit. It never blocks: if the buffer is full or the recorder
// has already been shut down (handler outlasted srv.Shutdown's drain), the
// hit is dropped — analytics is best-effort.
//
// Two-step check + send rather than one select: a single select picks
// randomly among ready cases, so we could enqueue an event after shutdown
// fired. The fast-path Load also avoids the channel send entirely once the
// recorder is stopped.
func (hr *hitRecorder) record(slug, device string) {
	if hr.stopped.Load() {
		return
	}
	select {
	case hr.ch <- hitEvent{slug: slug, device: device}:
	default:
		hr.log.Warn("hit recorder buffer full, dropping hit", "slug", slug)
	}
}

// worker drains hr.ch until shutdown(); on shutdown it processes whatever is
// still buffered and then exits. We never close hr.ch (see record's comment),
// so the loop is select-driven rather than range-driven.
func (hr *hitRecorder) worker() {
	defer hr.wg.Done()
	for {
		select {
		case ev := <-hr.ch:
			hr.write(ev)
		case <-hr.done:
			// Drain anything left in the buffer (record() stopped accepting
			// new events before done was closed) and exit.
			for {
				select {
				case ev := <-hr.ch:
					hr.write(ev)
				default:
					return
				}
			}
		}
	}
}

func (hr *hitRecorder) write(ev hitEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), hitWriteTTL)
	defer cancel()
	// One round-trip: the CTE inserts the hits row and the outer UPDATE bumps
	// the short_urls counter in the same statement.
	if err := hr.queries.RecordHit(ctx, db.RecordHitParams{
		Slug:    pgtype.Text{String: ev.slug, Valid: true},
		Country: pgtype.Text{Valid: false}, // GeoIP is a v2 item
		Device:  pgtype.Text{String: ev.device, Valid: ev.device != ""},
	}); err != nil {
		hr.log.Warn("record hit", "error", err, "slug", ev.slug)
	}
}

// shutdown stops intake and drains buffered hits. Safe to call even while
// in-flight handlers may still call record(): the atomic stop flag short-
// circuits any racing record(), and `ch` is never closed so a racing send
// that snuck past the flag check just adds one more event for the drainer.
func (hr *hitRecorder) shutdown() {
	hr.stopped.Store(true)
	close(hr.done)
	hr.wg.Wait()
}
