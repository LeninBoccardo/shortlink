package main

import (
	"log/slog"
	"net/http"

	"github.com/leninboccardo/shortlink/internal/keysfile"
)

// sink is the built-in webhook receiver. Piece 3 (next commit) fills in the
// HMAC verification + per-key counters; this stub just lets the skeleton
// build and serves a 200 for /healthz.
type sink struct {
	keys *keysfile.File
	log  *slog.Logger
}

func newSink(keys *keysfile.File, log *slog.Logger) *sink {
	return &sink{keys: keys, log: log}
}

func (s *sink) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func (s *sink) counts() map[string]int { return nil }
