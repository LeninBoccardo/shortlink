// Package sweeper reclaims abandoned rows and orphaned QR objects (SPEC §6).
// It runs as a background loop inside the worker.
package sweeper

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/storage"
)

const (
	// failedGrace is how long a failed row lingers before deletion (SPEC §6,
	// "a short grace period").
	failedGrace = 5 * time.Minute
	// qrBatchSize caps the QR objects reclaimed per sweep.
	qrBatchSize = 200
)

// Sweeper periodically deletes stale reservations and reclaims expired QR
// objects.
type Sweeper struct {
	queries     *db.Queries
	store       *storage.ObjectStore
	staleAge    time.Duration
	qrObjectTTL time.Duration
	interval    time.Duration
	log         *slog.Logger
}

// New builds a Sweeper. staleAge and qrObjectTTL come from config; interval is
// how often the loop runs.
func New(queries *db.Queries, store *storage.ObjectStore, staleAge, qrObjectTTL, interval time.Duration, log *slog.Logger) *Sweeper {
	return &Sweeper{
		queries:     queries,
		store:       store,
		staleAge:    staleAge,
		qrObjectTTL: qrObjectTTL,
		interval:    interval,
		log:         log,
	}
}

// Run sweeps once immediately, then every interval, until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *Sweeper) sweep(ctx context.Context) {
	now := time.Now()
	s.sweepStaleReservations(ctx, now)
	s.sweepFailed(ctx, now)
	s.sweepQRObjects(ctx, now)
}

// sweepStaleReservations deletes abandoned pending/processing rows, freeing any
// custom slug they reserved.
func (s *Sweeper) sweepStaleReservations(ctx context.Context, now time.Time) {
	n, err := s.queries.DeleteStaleReservations(ctx, ts(now.Add(-s.staleAge)))
	if err != nil {
		s.log.Error("sweep stale reservations", "error", err)
		return
	}
	if n > 0 {
		s.log.Info("swept stale reservations", "rows", n)
	}
}

// sweepFailed deletes failed rows past the grace period.
func (s *Sweeper) sweepFailed(ctx context.Context, now time.Time) {
	n, err := s.queries.DeleteOldFailedShortURLs(ctx, ts(now.Add(-failedGrace)))
	if err != nil {
		s.log.Error("sweep failed rows", "error", err)
		return
	}
	if n > 0 {
		s.log.Info("swept failed rows", "rows", n)
	}
}

// sweepQRObjects deletes QR objects past QR_OBJECT_TTL and NULLs the column.
// Both the column clear and the MinIO delete are bulked: one SQL statement
// and one S3 multi-object-delete request, replacing the prior per-row loop
// that issued up to 2*qrBatchSize sequential round-trips per tick.
func (s *Sweeper) sweepQRObjects(ctx context.Context, now time.Time) {
	rows, err := s.queries.ListExpiredQRObjects(ctx, db.ListExpiredQRObjectsParams{
		Cutoff:  ts(now.Add(-s.qrObjectTTL)),
		MaxRows: qrBatchSize,
	})
	if err != nil {
		s.log.Error("list expired qr objects", "error", err)
		return
	}
	jobIDs := make([]string, 0, len(rows))
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		if !r.QrObject.Valid {
			continue
		}
		jobIDs = append(jobIDs, r.JobID)
		keys = append(keys, r.QrObject.String)
	}
	if len(jobIDs) == 0 {
		return
	}
	// Null the qr_object column BEFORE deleting the storage object so a
	// concurrent webhook handler that loaded the row a moment earlier can't
	// Stat a key the sweeper is about to delete (M9a-B5). The handler's
	// QrObject.Valid guard means any later load drops the delivery cleanly.
	// Per-key Delete failures leave MinIO orphans; the SPEC §6 1-day
	// lifecycle rule is the documented backstop.
	if err := s.queries.ClearQRObjects(ctx, jobIDs); err != nil {
		s.log.Error("clear qr_object columns (bulk)", "error", err, "count", len(jobIDs))
		return
	}
	delErrs := s.store.DeleteMany(ctx, keys)
	for _, derr := range delErrs {
		s.log.Warn("delete qr object (orphan)", "error", derr)
	}
	cleared := len(keys) - len(delErrs)
	if cleared > 0 {
		s.log.Info("reclaimed expired qr objects", "count", cleared, "orphans", len(delErrs))
	}
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
