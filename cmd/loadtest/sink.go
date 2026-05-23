package main

import (
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/leninboccardo/shortlink/internal/keysfile"
	"github.com/leninboccardo/shortlink/internal/webhook"
)

// sink is the built-in webhook receiver (SPEC §4.4). For each delivery it
// looks up the key by the X-ShortLink-Key-Hint header, verifies the
// X-ShortLink-Signature HMAC against the request body, and counts
// success/rejection per key hint. The counters feed the post-attack summary.
type sink struct {
	log     *slog.Logger
	hintMap map[string]string // key_hint -> webhook_secret

	mu        sync.Mutex
	delivered map[string]int // key_hint -> count of verified deliveries
	rejected  map[string]int // key_hint -> count of bad-sig / unknown-key rejects
}

const (
	headerHint = "X-ShortLink-Key-Hint"
	headerSig  = "X-ShortLink-Signature"
	maxBody    = 64 << 10
)

func newSink(keys *keysfile.File, log *slog.Logger) *sink {
	hintMap := make(map[string]string, len(keys.Keys))
	for _, e := range keys.Keys {
		hintMap[hintOf(e.Key)] = e.WebhookSecret
	}
	return &sink{
		log:       log,
		hintMap:   hintMap,
		delivered: make(map[string]int),
		rejected:  make(map[string]int),
	}
}

// hintOf returns the last 6 chars of the raw key — mirrors auth.Hint without
// pulling the whole auth package (which depends on the DB).
func hintOf(raw string) string {
	const n = 6
	if len(raw) <= n {
		return raw
	}
	return raw[len(raw)-n:]
}

func (s *sink) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/sink", s.handleSink)
	return mux
}

func (s *sink) handleSink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	hint := r.Header.Get(headerHint)
	sig := r.Header.Get(headerSig)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		s.bump(&s.rejected, hint)
		s.log.Warn("sink: read body", "error", err, "hint", hint)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := s.verify(hint, sig, body); err != nil {
		s.bump(&s.rejected, hint)
		s.log.Warn("sink: hmac verify failed", "error", err, "hint", hint)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	s.bump(&s.delivered, hint)
	w.WriteHeader(http.StatusOK)
}

func (s *sink) verify(hint, sig string, body []byte) error {
	if hint == "" {
		return errors.New("missing key hint header")
	}
	secret, ok := s.hintMap[hint]
	if !ok {
		return errors.New("unknown key hint")
	}
	if !strings.HasPrefix(sig, "sha256=") {
		return errors.New("malformed signature header")
	}
	want := []byte(webhook.Sign(secret, body))
	got := []byte(sig)
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return errors.New("signature mismatch")
	}
	return nil
}

func (s *sink) bump(m *map[string]int, hint string) {
	s.mu.Lock()
	(*m)[hint]++
	s.mu.Unlock()
}

// counts returns the per-hint delivered count for the run summary.
func (s *sink) counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.delivered))
	for k, v := range s.delivered {
		out[k] = v
	}
	return out
}

// rejectedCounts mirrors counts for the rejected map. Used in the summary.
func (s *sink) rejectedCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.rejected))
	for k, v := range s.rejected {
		out[k] = v
	}
	return out
}
