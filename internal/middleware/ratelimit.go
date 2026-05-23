package middleware

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/leninboccardo/shortlink/internal/auth"
	"github.com/leninboccardo/shortlink/internal/httpx"
)

// LimitForTier resolves a tier name to its per-window request budget. A value
// <= 0 means unlimited — the limiter is skipped.
type LimitForTier func(tier string) int

// RateLimit enforces the per-key sliding-window limit (SPEC §4.1/§9). On every
// rate-limited request it sets X-RateLimit-Limit/Remaining/Reset; on rejection
// it adds Retry-After and returns 429.
func RateLimit(rl *auth.RateLimiter, limitFor LimitForTier, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := APIKey(r.Context())
			if !ok {
				// Auth didn't run — shouldn't happen if chained right; fail open.
				next.ServeHTTP(w, r)
				return
			}
			limit := limitFor(key.Tier)
			dec, err := rl.Check(r.Context(), key.KeyHash, limit, chimw.GetReqID(r.Context()))
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
				httpx.WriteError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
