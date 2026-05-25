// Package storage provides the Postgres connection pool and the object-storage
// client used across the ShortLink binaries.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens a pgx connection pool against dsn, caps it at maxConns, and
// verifies connectivity before returning.
//
// Health/lifetime knobs override pgx defaults so a stale connection left over
// from a Postgres failover (or a long-idle PgBouncer pause) gets recycled
// instead of returning a hard error on first reuse:
//   - MaxConnIdleTime: 5m so idle conns turn over predictably
//   - MaxConnLifetime: 30m so even hot conns get refreshed against the
//     current PG instance
//   - HealthCheckPeriod: 30s so a wedged conn is removed proactively rather
//     than only on the next acquire
func NewPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}
