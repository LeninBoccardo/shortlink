package middleware

import (
	"net/http"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/leninboccardo/shortlink/internal/events"
)

// Stat emits a request_completed event after each request the chain reaches
// (SPEC §4.3/§10). Scope it under Auth+RateLimit so the request context
// already carries the api key and resolved tier/limit.
func Stat(em *events.Emitter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			key, ok := APIKey(r.Context())
			if !ok {
				return
			}
			meta := map[string]any{
				"duration_ms": time.Since(start).Milliseconds(),
				"status":      ww.Status(),
			}
			if rl, ok := RateLimitFromContext(r.Context()); ok {
				meta["tier"] = rl.Tier
				meta["rate_limit"] = rl.Limit
			}
			em.Emit(events.Event{
				Level:      events.LevelInfo,
				Kind:       events.KindRequestCompleted,
				APIKeyHash: key.KeyHash,
				APIKeyHint: key.KeyHint,
				Message:    "request completed",
				Meta:       meta,
			})
		})
	}
}
