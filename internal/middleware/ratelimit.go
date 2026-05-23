package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/leninboccardo/shortlink/internal/auth"
	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/httpx"
)

// LimitForTier resolves a tier name to its per-window request budget. A value
// <= 0 means unlimited — the limiter is skipped.
type LimitForTier func(tier string) int

// RateLimitInfo is the per-request context value the limiter publishes so that
// downstream middleware (Logger) can include tier + limit in stat events
// without having to re-resolve them.
type RateLimitInfo struct {
	Tier  string
	Limit int
}

// RateLimitFromContext returns the resolved tier + limit if the RateLimit
// middleware ran for this request.
func RateLimitFromContext(ctx context.Context) (RateLimitInfo, bool) {
	v, ok := ctx.Value(rateLimitCtxKey).(RateLimitInfo)
	return v, ok
}

// RateLimit enforces the per-key sliding-window limit (SPEC §4.1/§9). On every
// rate-limited request it sets X-RateLimit-Limit/Remaining/Reset; on rejection
// it adds Retry-After, emits a rate_limit_hit event, and returns 429.
func RateLimit(rl *auth.RateLimiter, limitFor LimitForTier, em *events.Emitter, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := APIKey(r.Context())
			if !ok {
				// Auth didn't run — shouldn't happen if chained right; fail open.
				next.ServeHTTP(w, r)
				return
			}
			limit := limitFor(key.Tier)
			ctx := context.WithValue(r.Context(), rateLimitCtxKey, RateLimitInfo{Tier: key.Tier, Limit: limit})
			r = r.WithContext(ctx)
			dec, err := rl.Check(ctx, key.KeyHash, limit, chimw.GetReqID(ctx))
			if err != nil {
				// Fail open on Redis errors — do not block traffic on a Redis blip.
				log.Error("rate limit check", "error", err)
				next.ServeHTTP(w, r)
				return
			}
			if dec.Limit > 0 {
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(dec.Limit))
				w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(dec.Remaining))
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(dec.ResetAt.Unix(), 10))
			}
			if !dec.Allowed {
				retryAfter := int(time.Until(dec.ResetAt).Seconds() + 0.5)
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				em.Emit(events.Event{
					Level:      events.LevelWarn,
					Kind:       events.KindRateLimitHit,
					APIKeyHash: key.KeyHash,
					APIKeyHint: key.KeyHint,
					Message:    "rate limit exceeded",
					Meta: map[string]any{
						"tier":        key.Tier,
						"rate_limit":  limit,
						"retry_after": retryAfter,
					},
				})
				httpx.WriteError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
