package auth

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/leninboccardo/shortlink/internal/db"
)

// LastUsedToucher records that an API key was just used, but bumps
// api_keys.last_used_at at most once per `throttle` per key — gated by a Redis
// SETNX marker (SPEC §9 / LAST_USED_THROTTLE).
type LastUsedToucher struct {
	queries  *db.Queries
	redis    *redis.Client
	throttle time.Duration
	log      *slog.Logger
}

// NewLastUsedToucher returns a toucher; pass nil rc to disable the touch.
func NewLastUsedToucher(q *db.Queries, rc *redis.Client, throttle time.Duration, log *slog.Logger) *LastUsedToucher {
	return &LastUsedToucher{queries: q, redis: rc, throttle: throttle, log: log}
}

// Touch is best-effort: Redis or DB errors are logged but never block the
// request, and the DB write only runs when the throttle window is fresh.
func (t *LastUsedToucher) Touch(ctx context.Context, keyHash string, keyID pgtype.UUID) {
	if t == nil || t.redis == nil {
		return
	}
	ok, err := t.redis.SetNX(ctx, "lu:"+keyHash, "1", t.throttle).Result()
	if err != nil {
		t.log.Warn("last-used throttle check", "error", err)
		return
	}
	if !ok {
		return // already touched within the throttle window
	}
	if err := t.queries.UpdateLastUsedAt(ctx, keyID); err != nil {
		t.log.Warn("update last_used_at", "error", err)
	}
}
