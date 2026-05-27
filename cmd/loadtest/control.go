// Control-plane endpoints for the operator panel (SPEC §4.4 — UI-driven
// keygen + attack lifecycle). Mounted under /api/* on the same loopback
// server that hosts the showcase page; the inherited 127.0.0.1 bind is the
// only thing standing between an attacker on the same network and a free
// keygen oracle, so KEEP THESE ENDPOINTS LOCAL-ONLY.
//
//   GET  /api/keys                    → list registered keys (hint, tier, name, rate)
//   POST /api/keys/generate           → {name, tier} → returns raw key + webhook secret
//   POST /api/keys/revoke             → {key_hint} → soft-deletes in DB + drops from registry
//   GET  /api/attack/status           → current vegeta state (idle | running)
//   POST /api/attack/start            → optional {duration_seconds, key_hints} → starts attack
//   POST /api/attack/stop             → cancels current attack, waits up to 5s to drain
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leninboccardo/shortlink/internal/auth"
	"github.com/leninboccardo/shortlink/internal/db"
	"github.com/leninboccardo/shortlink/internal/events"
	"github.com/leninboccardo/shortlink/internal/keysfile"
)

// tierDefaults maps a tier label to the default vegeta attack rate per
// minute used when the operator UI generates a key without specifying one.
// Matches cmd/keygen's hard-coded profiles so the UI and the CLI converge
// on the same shape of generated key.
var tierDefaults = map[string]int{
	"free":      10,
	"pro":       60,
	"unlimited": 200,
}

// tierRateMax caps the operator-supplied attack_rate_per_min per tier.
// Without a cap, a single key minted via the UI can drive vegeta at an
// arbitrarily high rate and effectively DoS the api / Postgres pool / Redis
// rate-limit budget. The values are 10× the tier default — comfortably high
// enough for legitimate "what does this look like under burst" exploration,
// low enough that an accidental copy-paste of `999999999` is rejected at
// the door rather than ten minutes later when the operator notices.
var tierRateMax = map[string]int{
	"free":      100,
	"pro":       600,
	"unlimited": 2000,
}

// validTier reports whether tier is one we recognise. Any other label would
// pass through to the DB (which has no CHECK on tier — tier is just text in
// the schema), so we gate at the UI layer.
func validTier(tier string) bool {
	_, ok := tierDefaults[tier]
	return ok
}

// attackState is a tiny lifecycle machine: idle ↔ running. A single struct +
// mutex is plenty here — Status() is called from the polling UI every second
// or so, mutations come from explicit user clicks. No need for atomic.Pointer
// theatrics.
type attackState struct {
	mu       sync.Mutex
	running  bool
	started  time.Time
	duration time.Duration
	keyHints []string // empty slice = "all keys in registry"
	cancel   context.CancelFunc
	done     chan struct{} // closed by the attack goroutine on exit
}

// snapshot returns a copy of the current state for the status endpoint.
func (s *attackState) snapshot() attackStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return attackStatus{State: "idle"}
	}
	elapsed := time.Since(s.started)
	remaining := s.duration - elapsed
	if remaining < 0 {
		remaining = 0
	}
	return attackStatus{
		State:            "running",
		StartedAt:        s.started.UTC().Format(time.RFC3339),
		DurationSeconds:  int(s.duration.Seconds()),
		ElapsedSeconds:   int(elapsed.Seconds()),
		RemainingSeconds: int(remaining.Seconds()),
		KeyHints:         append([]string{}, s.keyHints...),
	}
}

// claim moves idle→running atomically. Returns ok=false if already running
// so the handler can answer 409 without racing.
func (s *attackState) claim(duration time.Duration, keyHints []string, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	s.started = time.Now()
	s.duration = duration
	s.keyHints = append([]string{}, keyHints...)
	s.cancel = cancel
	s.done = make(chan struct{})
	return true
}

// release moves running→idle. Called by the attack goroutine on exit.
func (s *attackState) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	s.running = false
	s.cancel = nil
	close(s.done)
}

// requestStop calls cancel() and returns the done channel so the caller
// can wait (with a timeout) for the attack goroutine to actually exit.
// Returns nil, nil if no attack was running.
func (s *attackState) requestStop() (<-chan struct{}, context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return nil, nil
	}
	return s.done, s.cancel
}

// attackStatus is the JSON shape returned by GET /api/attack/status. State
// is always set; everything else is populated only when running.
type attackStatus struct {
	State            string   `json:"state"`
	StartedAt        string   `json:"started_at,omitempty"`
	DurationSeconds  int      `json:"duration_seconds,omitempty"`
	ElapsedSeconds   int      `json:"elapsed_seconds,omitempty"`
	RemainingSeconds int      `json:"remaining_seconds,omitempty"`
	KeyHints         []string `json:"key_hints,omitempty"`
}

// keyView is the public projection of a registry entry. Crucially it does
// NOT include the raw key — the only time a raw key is returned is the
// one-shot response from /api/keys/generate, and that's gone from the
// process the moment the response flushes.
type keyView struct {
	Name             string `json:"name"`
	Hint             string `json:"key_hint"`
	Tier             string `json:"tier"`
	AttackRatePerMin int    `json:"attack_rate_per_min"`
}

// generateResponse is the one-shot body for POST /api/keys/generate. The
// UI surfaces `key` once and never asks the server for it again — any
// later view goes through /api/keys which omits the raw material.
type generateResponse struct {
	Name             string `json:"name"`
	Key              string `json:"key"`
	WebhookSecret    string `json:"webhook_secret"`
	KeyHint          string `json:"key_hint"`
	Tier             string `json:"tier"`
	AttackRatePerMin int    `json:"attack_rate_per_min"`
}

// controlServer owns the shared state for the operator panel.
type controlServer struct {
	keys    *keyRegistry
	queries *db.Queries
	cfg     runConfig
	log     *slog.Logger
	emitter *events.Emitter
	sink    *sink // for delivery counts in the attack-complete summary
	attack  attackState
}

func newControlServer(keys *keyRegistry, queries *db.Queries, cfg runConfig, log *slog.Logger, emitter *events.Emitter, sink *sink) *controlServer {
	return &controlServer{
		keys:    keys,
		queries: queries,
		cfg:     cfg,
		log:     log,
		emitter: emitter,
		sink:    sink,
	}
}

func (s *controlServer) attachRoutes(mux *http.ServeMux) {
	// Mutating endpoints get an extra same-origin gate (see sameOriginGuard
	// for the reasoning); read-only ones don't need it because they can't
	// change state. The dev-mode 127.0.0.1 bind is NOT a sufficient defence
	// on its own — any malicious page the operator visits can fetch() into
	// localhost from JavaScript.
	mux.HandleFunc("/api/keys", s.handleKeys)
	mux.HandleFunc("/api/keys/generate", sameOriginGuard(s.handleGenerate))
	mux.HandleFunc("/api/keys/revoke", sameOriginGuard(s.handleRevoke))
	mux.HandleFunc("/api/attack/status", s.handleAttackStatus)
	mux.HandleFunc("/api/attack/start", sameOriginGuard(s.handleAttackStart))
	mux.HandleFunc("/api/attack/stop", sameOriginGuard(s.handleAttackStop))
}

// sameOriginGuard rejects state-changing requests that aren't either
// (a) browser requests with `Sec-Fetch-Site: same-origin` (or `none`, for
// address-bar navigations), or (b) non-browser requests with no Origin
// header at all (curl, integration tests, server-to-server calls).
//
// The middleware closes the CSRF gap on the operator panel: a cross-origin
// page can fetch() into localhost:8090 because the dev-mode loopback bind
// isn't a browser boundary; without this guard a visited webpage could
// silently POST /api/keys/generate or /api/attack/start in the
// background. The guard also blunts the SSRF-pivot vector (S1): even if
// a webhook URL slips past the allowlist, the worker's outbound POST
// won't carry a Sec-Fetch-Site header, but it WILL carry the worker's
// own Origin/Referer chain depending on how the http.Client is set up
// — explicitly rejecting "browser but cross-origin" closes the easy
// case; the port-pinned allowlist closes the rest.
func sameOriginGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Read-only methods don't change state; never gate them.
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next(w, r)
			return
		}
		site := r.Header.Get("Sec-Fetch-Site")
		if site != "" {
			// Modern browser: trust Sec-Fetch-Site, accept only first-party.
			if site == "same-origin" || site == "none" {
				next(w, r)
				return
			}
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		// No Sec-Fetch-Site header. Two cases: an old browser (rare in the
		// operator-panel target audience) or a non-browser caller. We use
		// the presence of Origin as the discriminator — browsers always
		// send it on a non-GET request, server-to-server callers don't.
		if r.Header.Get("Origin") != "" {
			http.Error(w, "cross-origin request rejected (no Sec-Fetch-Site)", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// keys

func (s *controlServer) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries := s.keys.Snapshot()
	out := make([]keyView, 0, len(entries))
	for _, e := range entries {
		out = append(out, keyView{
			Name:             e.Name,
			Hint:             hintOf(e.Key),
			Tier:             e.Tier,
			AttackRatePerMin: e.AttackRatePerMin,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *controlServer) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name             string `json:"name"`
		Tier             string `json:"tier"`
		AttackRatePerMin int    `json:"attack_rate_per_min"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "malformed JSON body", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Tier = strings.ToLower(strings.TrimSpace(req.Tier))
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if !validTier(req.Tier) {
		http.Error(w, "tier must be one of: free, pro, unlimited", http.StatusBadRequest)
		return
	}
	rate := req.AttackRatePerMin
	if rate <= 0 {
		rate = tierDefaults[req.Tier]
	}
	if max := tierRateMax[req.Tier]; rate > max {
		http.Error(w, fmt.Sprintf("attack_rate_per_min must be <= %d for tier %s", max, req.Tier), http.StatusBadRequest)
		return
	}

	rawKey, err := auth.NewAPIKey()
	if err != nil {
		s.log.Error("generate api key", "error", err)
		http.Error(w, "generate key failed", http.StatusInternalServerError)
		return
	}
	secret, err := auth.NewWebhookSecret()
	if err != nil {
		s.log.Error("generate webhook secret", "error", err)
		http.Error(w, "generate secret failed", http.StatusInternalServerError)
		return
	}
	// DB first — if this fails, nothing landed and the registry is unchanged.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := s.queries.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:       auth.HashKey(rawKey),
		KeyHint:       auth.Hint(rawKey),
		Name:          req.Name,
		Tier:          req.Tier,
		WebhookSecret: secret,
		WebhookUrl:    pgtype.Text{Valid: false},
	}); err != nil {
		s.log.Error("create api key in db", "error", err)
		http.Error(w, "create key in db failed", http.StatusInternalServerError)
		return
	}
	entry := keysfile.Entry{
		Name:             req.Name,
		Key:              rawKey,
		WebhookSecret:    secret,
		AttackRatePerMin: rate,
		Tier:             req.Tier,
	}
	if err := s.keys.Append(entry); err != nil {
		// DB succeeded but keys.yaml write failed. Surface the inconsistency
		// to the operator — keygen via CLI can recover by rewriting the file
		// against the DB. We deliberately don't auto-revoke the DB row: a
		// transient disk error shouldn't lose a valid key.
		s.log.Error("append key to registry", "error", err, "hint", auth.Hint(rawKey))
		http.Error(w, fmt.Sprintf("key created in DB but keys.yaml write failed: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Info("operator generated key", "hint", auth.Hint(rawKey), "tier", req.Tier, "name", req.Name)
	writeJSON(w, http.StatusCreated, generateResponse{
		Name:             req.Name,
		Key:              rawKey,
		WebhookSecret:    secret,
		KeyHint:          auth.Hint(rawKey),
		Tier:             req.Tier,
		AttackRatePerMin: rate,
	})
}

func (s *controlServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		KeyHint string `json:"key_hint"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "malformed JSON body", http.StatusBadRequest)
		return
	}
	hint := strings.TrimSpace(req.KeyHint)
	if hint == "" {
		http.Error(w, "key_hint is required", http.StatusBadRequest)
		return
	}
	// DB first again — if revoke matches 0 rows the key was already revoked
	// (or never existed), but the registry might still have a stale row from
	// a manually-edited keys.yaml, so we run the registry removal regardless
	// and let it report whether anything was in memory.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.queries.RevokeAPIKeyByHint(ctx, hint)
	if err != nil {
		s.log.Error("revoke api key in db", "error", err, "hint", hint)
		http.Error(w, "revoke key in db failed", http.StatusInternalServerError)
		return
	}
	removed, regErr := s.keys.RemoveByHint(hint)
	if regErr != nil {
		s.log.Error("remove key from registry", "error", regErr, "hint", hint)
		http.Error(w, fmt.Sprintf("key revoked in DB but keys.yaml write failed: %v", regErr), http.StatusInternalServerError)
		return
	}
	if rows == 0 && !removed {
		http.Error(w, "no such key", http.StatusNotFound)
		return
	}
	s.log.Info("operator revoked key", "hint", hint, "db_rows", rows, "from_registry", removed)
	writeJSON(w, http.StatusOK, map[string]any{
		"key_hint":      hint,
		"db_rows":       rows,
		"from_registry": removed,
	})
}

// ---------------------------------------------------------------------------
// attack lifecycle

func (s *controlServer) handleAttackStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.attack.snapshot())
}

func (s *controlServer) handleAttackStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DurationSeconds int      `json:"duration_seconds"`
		KeyHints        []string `json:"key_hints"`
	}
	// Body is optional — empty body means "use the CLI defaults" and
	// decodeJSON returns nil for that case. A malformed body should be a
	// hard 400 rather than silently falling through to defaults: prior to
	// this, a webhook payload (`{job_id:..., qr_code:...}`) sent here via
	// the SSRF pivot would unmarshal cleanly into the zero-valued request
	// struct and start a 60-second attack on all keys.
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "malformed JSON body", http.StatusBadRequest)
		return
	}

	duration := s.cfg.duration
	if req.DurationSeconds > 0 {
		duration = time.Duration(req.DurationSeconds) * time.Second
	}
	// Cap at 24h to keep accidental "duration: 999999999" requests from
	// pinning the system; if you legitimately want a week-long soak test,
	// edit this constant — but the loadtest UI is for short demos.
	if duration > 24*time.Hour {
		http.Error(w, "duration must be <= 24h", http.StatusBadRequest)
		return
	}

	// Resolve the key set NOW, not inside the goroutine, so the user gets
	// an immediate 400 if their hint list is bogus.
	entries := s.keys.Snapshot()
	if len(entries) == 0 {
		http.Error(w, "no keys registered — generate at least one via POST /api/keys/generate", http.StatusPreconditionFailed)
		return
	}
	selected := entries
	if len(req.KeyHints) > 0 {
		filterSet := make(map[string]struct{}, len(req.KeyHints))
		for _, h := range req.KeyHints {
			filterSet[strings.TrimSpace(h)] = struct{}{}
		}
		selected = selected[:0]
		for _, e := range entries {
			if _, want := filterSet[hintOf(e.Key)]; want {
				selected = append(selected, e)
			}
		}
		if len(selected) == 0 {
			http.Error(w, "no keys matched the provided key_hints", http.StatusBadRequest)
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	if !s.attack.claim(duration, hintsOf(selected), cancel) {
		cancel()
		http.Error(w, "an attack is already running — stop it first", http.StatusConflict)
		return
	}

	// Snapshot the keys we'll attack with so a concurrent revoke can't
	// remove an entry mid-flight. attack.go's runAttacks takes a
	// *keysfile.File; build a synthetic one from the selected slice.
	keysForAttack := &keysfile.File{Keys: selected}
	cfgForAttack := s.cfg
	cfgForAttack.duration = duration

	s.emitter.Emit(events.Event{
		Level:   events.LevelInfo,
		Kind:    events.KindAttackStarted,
		Message: fmt.Sprintf("operator-triggered attack: %d profiles, duration=%s", len(selected), duration),
		Meta: map[string]any{
			"duration_s": int(duration.Seconds()),
			"profiles":   len(selected),
			"target":     cfgForAttack.target,
			"trigger":    "operator_ui",
		},
	})
	go func() {
		defer s.attack.release()
		results := runAttacks(ctx, keysForAttack, cfgForAttack, s.log)
		var delivered, rejected map[string]int
		if s.sink != nil {
			delivered = s.sink.counts()
			rejected = s.sink.rejectedCounts()
		}
		s.emitter.Emit(events.Event{
			Level:   events.LevelInfo,
			Kind:    events.KindAttackComplete,
			Message: fmt.Sprintf("operator attack complete: %d profiles", len(results)),
			Meta:    summaryMeta(results, delivered, rejected),
		})
	}()

	writeJSON(w, http.StatusAccepted, s.attack.snapshot())
}

func (s *controlServer) handleAttackStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	done, cancel := s.attack.requestStop()
	if done == nil {
		http.Error(w, "no attack is running", http.StatusConflict)
		return
	}
	cancel()
	// Wait briefly so the next /start sees state=idle without racing. Vegeta's
	// per-request 10s timeout bounds the drain in the worst case.
	select {
	case <-done:
		writeJSON(w, http.StatusOK, s.attack.snapshot())
	case <-time.After(5 * time.Second):
		// Goroutine is still draining; client should poll /status. Return
		// 202 so the caller knows the cancel landed but state hasn't settled.
		writeJSON(w, http.StatusAccepted, map[string]string{
			"state": "stopping",
			"note":  "cancel requested; goroutine still draining, poll /api/attack/status",
		})
	}
}

// ---------------------------------------------------------------------------
// shared helpers

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func decodeJSON(r *http.Request, into any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, into)
}

// hintsOf extracts the 6-char hints for the selected key set so the status
// endpoint can report which keys an attack is using without exposing the
// raw key material.
func hintsOf(entries []keysfile.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, hintOf(e.Key))
	}
	return out
}

