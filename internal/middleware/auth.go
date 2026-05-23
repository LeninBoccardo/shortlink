// Package middleware holds the gateway's HTTP middleware (SPEC §4.1).
package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/leninboccardo/shortlink/internal/auth"
	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/httpx"
)

type ctxKey int

const apiKeyCtxKey ctxKey = iota

// Auth validates the X-Api-Key header, records the touch (throttled), and
// injects the resolved api_keys row into the request context. Apply it only
// to authenticated routes — the redirect path is public.
func Auth(v *auth.Validator, t *auth.LastUsedToucher, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("X-Api-Key")
			key, err := v.Validate(r.Context(), raw)
			if err != nil {
				if !errors.Is(err, auth.ErrInvalidKey) {
					log.Error("api key validation error", "error", err)
					httpx.WriteError(w, http.StatusInternalServerError, "internal error")
					return
				}
				httpx.WriteError(w, http.StatusUnauthorized, "missing or invalid API key")
				return
			}
			ctx := context.WithValue(r.Context(), apiKeyCtxKey, key)
			if t != nil {
				t.Touch(ctx, key.KeyHash, key.ID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// APIKey returns the authenticated api_keys row from the request context.
func APIKey(ctx context.Context) (db.ApiKey, bool) {
	key, ok := ctx.Value(apiKeyCtxKey).(db.ApiKey)
	return key, ok
}
