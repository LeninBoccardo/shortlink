package main

import (
	"context"
	"log/slog"
	"sync"
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
}

// newHitRecorder starts the recorder's worker pool.
func newHitRecorder(queries *db.Queries, log *slog.Logger) *hitRecorder {
	hr := &hitRecorder{
		queries: queries,
		log:     log,
		ch:      make(chan hitEvent, hitBuffer),
	}
	for i := 0; i < hitWorkers; i++ {
		hr.wg.Add(1)
		go hr.worker()
	}
	return hr
}

// record queues a hit. It never blocks: if the buffer is full the hit is
// dropped, since analytics is best-effort.
func (hr *hitRecorder) record(slug, device string) {
	select {
	case hr.ch <- hitEvent{slug: slug, device: device}:
	default:
		hr.log.Warn("hit recorder buffer full, dropping hit", "slug", slug)
	}
}

func (hr *hitRecorder) worker() {
	defer hr.wg.Done()
	for ev := range hr.ch {
		hr.write(ev)
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

// shutdown stops intake and drains buffered hits. Call it only after the HTTP
// server has stopped, so no further record calls race the channel close.
func (hr *hitRecorder) shutdown() {
	close(hr.ch)
	hr.wg.Wait()
}
