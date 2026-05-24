package main

import (
	"net/http"

	"github.com/leninboccardo/shortlink/internal/metrics"
)

// shortenStatusRecorder captures the response status so the recorder
// middleware can map it to a shortlink_shorten_requests_total label.
type shortenStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *shortenStatusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// shortenMetricRecorder is the OUTERMOST middleware on POST /shorten. It
// observes the final HTTP status (set by any later middleware or the handler)
// and bumps shortlink_shorten_requests_total{status} exactly once per request.
// Centralising the increment here keeps the metric coherent: every request
// ends in exactly one label, no branch can forget to count, and the path-
// agnostic Auth/RateLimit middlewares stop hard-coding /shorten knowledge.
func shortenMetricRecorder(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default 200 covers handlers that hand back a body without ever
		// calling WriteHeader (net/http does the implicit write on Write).
		rec := &shortenStatusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		metrics.ShortenRequestsTotal.WithLabelValues(shortenStatusForCode(rec.status)).Inc()
	})
}

// shortenStatusForCode maps an HTTP status to the canonical low-cardinality
// label. Unmapped codes bucket into "unknown" so a future handler returning
// 418 cannot grow the label set without an explicit decision.
func shortenStatusForCode(code int) string {
	switch code {
	case http.StatusAccepted:
		return metrics.ShortenStatusAccepted
	case http.StatusUnauthorized:
		return metrics.ShortenStatusRejectedAuth
	case http.StatusTooManyRequests:
		return metrics.ShortenStatusRejectedRateLimit
	case http.StatusConflict:
		return metrics.ShortenStatusRejectedConflict
	}
	switch {
	case code >= 500:
		return metrics.ShortenStatusInternalError
	case code >= 400:
		// 400 (bad body), 422 (SSRF rejected), etc.
		return metrics.ShortenStatusRejectedValidation
	}
	return metrics.ShortenStatusUnknown
}
