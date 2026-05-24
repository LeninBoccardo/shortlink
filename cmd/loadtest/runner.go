// In-process test runner powering the showcase page's "Test console" panel.
// Each testCase is a one-shot probe against the live local stack -- POST
// /shorten with edge-case inputs, GET Prometheus/Grafana healthchecks,
// curl-equivalents that match the docs/MANUAL_TESTING.md catalog. The
// runner is mounted on the loadtest binary's HTTP server (same-origin with
// the page, so no CORS) and exposes:
//
//	GET  /tests/list         -> the catalog (categories + auto/manual flags)
//	POST /tests/run/{id}     -> execute one case, return structured result
//
// The catalog is deliberately data-driven so the frontend renders labels +
// categories + manual hints straight from the server's source of truth.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/leninboccardo/shortlink/internal/keysfile"
)

// testCase is one entry in the catalog. `run` is nil for manual-only entries;
// those render as instructional cards on the frontend with no Run button.
type testCase struct {
	ID          string   `json:"id"`
	Category    string   `json:"category"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Manual      bool     `json:"manual"`
	Steps       []string `json:"steps,omitempty"`
	run         func(context.Context) *testResult
}

// testResult is the JSON shape returned by POST /tests/run/{id}. Headers and
// Body are populated for the curl-equivalent tests so the frontend can show
// the raw response when the user expands a card.
type testResult struct {
	Passed     bool              `json:"passed"`
	StatusCode int               `json:"status_code,omitempty"`
	Expected   string            `json:"expected"`
	Actual     string            `json:"actual"`
	Details    string            `json:"details,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	DurationMs int64             `json:"duration_ms"`
	ErrorClass string            `json:"error_class,omitempty"`
}

type runner struct {
	apiBase       string // http://localhost:8080
	pageBase      string // http://localhost:8090 (for self-introspection)
	observerURL   string // http://localhost:9090
	grafanaURL    string // http://localhost:3000
	prometheusURL string // http://localhost:9091
	sinkURL       string // http://localhost:8091/sink
	keys          *keysfile.File
	client        *http.Client
	cases         []*testCase
	byID          map[string]*testCase
}

// newRunner builds the catalog and wires the per-case runtime info. The
// catalog order here is what the frontend renders top-to-bottom.
func newRunner(cfg runConfig, keys *keysfile.File, prometheusURL, pageBase string) *runner {
	if prometheusURL == "" {
		prometheusURL = "http://localhost:9091"
	}
	if pageBase == "" {
		pageBase = fmt.Sprintf("http://localhost:%d", cfg.pagePort)
	}
	r := &runner{
		apiBase:       strings.TrimRight(cfg.target, "/"),
		pageBase:      strings.TrimRight(pageBase, "/"),
		observerURL:   strings.TrimRight(cfg.observerURL, "/"),
		grafanaURL:    strings.TrimRight(cfg.grafanaURL, "/"),
		prometheusURL: strings.TrimRight(prometheusURL, "/"),
		sinkURL:       cfg.sinkURL,
		keys:          keys,
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
	r.cases = r.buildCatalog()
	r.byID = make(map[string]*testCase, len(r.cases))
	for _, c := range r.cases {
		r.byID[c.ID] = c
	}
	return r
}

func (r *runner) attachRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/tests/list", r.handleList)
	mux.HandleFunc("/tests/run/", r.handleRun)
}

func (r *runner) handleList(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(r.cases)
}

func (r *runner) handleRun(w http.ResponseWriter, req *http.Request) {
	id := strings.TrimPrefix(req.URL.Path, "/tests/run/")
	if id == "" {
		http.Error(w, "missing test id", http.StatusBadRequest)
		return
	}
	c, ok := r.byID[id]
	if !ok {
		http.Error(w, "unknown test id", http.StatusNotFound)
		return
	}
	if c.Manual || c.run == nil {
		http.Error(w, "this case is manual-only and cannot be auto-run", http.StatusMethodNotAllowed)
		return
	}
	// 5m upper bound so the integration-test card can finish; per-request
	// transport calls also carry the 20s client timeout above.
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	res := c.run(ctx)
	if res == nil {
		res = &testResult{Passed: false, Actual: "(no result)", ErrorClass: "internal"}
	}
	res.DurationMs = time.Since(start).Milliseconds()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(res)
}

// ----------------------------------------------------------------------------
// catalog builder

func (r *runner) buildCatalog() []*testCase {
	return []*testCase{
		// happy paths -----------------------------------------------------
		{
			ID: "shorten-happy-path", Category: "happy", Title: "Shorten — 202 Accepted",
			Description: "POST /shorten with a pro-tier key + a valid public URL. Expect 202 and a job_id in the body. The webhook delivery follows asynchronously and lands on the loadtest sink.",
			run:         r.runShortenHappy,
		},
		{
			ID: "custom-slug-and-redirect", Category: "happy", Title: "Custom slug + redirect",
			Description: "POST /shorten with custom_slug=<random>, wait for the job to finalize, then GET /<slug> and assert 302 with the original URL in Location.",
			run:         r.runCustomSlugAndRedirect,
		},

		// edge cases ------------------------------------------------------
		{
			ID: "bad-api-key", Category: "edge", Title: "Bad API key — 401",
			Description: "POST /shorten with a bogus X-Api-Key header. Expect 401 + 'missing or invalid API key' body.",
			run:         r.runBadAPIKey,
		},
		{
			ID: "ssrf-rfc1918", Category: "edge", Title: "SSRF webhook — 422",
			Description: "POST /shorten with webhook_url=http://10.0.0.99:8080/sink (RFC1918). The api's SSRF guard should reject with 422.",
			run:         r.runSSRF,
		},
		{
			ID: "url-blocklist", Category: "edge", Title: "URL blocklist — 422 (audit COV-4)",
			Description: "POST /shorten with a URL whose host matches URL_BLOCKLIST. Pre-audit-fix this returned 400; SPEC §9 mandates 422. REQUIRES: api restarted with URL_BLOCKLIST=blocked-domain.example.",
			run:         r.runURLBlocklist,
		},
		{
			ID: "rate-limit-burst", Category: "edge", Title: "Rate limit burst — 429s",
			Description: "Burst 12× POST /shorten with a free-tier key (default 10 req/min). Expect at least one 429 in the response codes.",
			run:         r.runRateLimitBurst,
		},
		{
			ID: "rate-limit-headers", Category: "edge", Title: "Rate limit headers present",
			Description: "Trigger a 429 (assumes burst test ran first or budget already exhausted), then assert X-RateLimit-Limit / X-RateLimit-Remaining / X-RateLimit-Reset / Retry-After are all present.",
			run:         r.runRateLimitHeaders,
		},
		{
			ID: "custom-slug-conflict", Category: "edge", Title: "Custom slug conflict — 409",
			Description: "POST /shorten twice with the same random custom_slug. Expect 202 then 409 + 'custom slug already taken'.",
			run:         r.runSlugConflict,
		},

		// observability ---------------------------------------------------
		{
			ID: "prometheus-targets", Category: "observability", Title: "Prometheus targets — all UP",
			Description: "Query Prometheus' /api/v1/targets. Pass if every active target is UP (api:8080, worker:8081, observer:9090).",
			run:         r.runPrometheusTargets,
		},
		{
			ID: "grafana-healthz", Category: "observability", Title: "Grafana healthz — 200",
			Description: "GET <grafana>/api/health. Pass if 200 OK + body contains \"ok\".",
			run:         r.runGrafanaHealthz,
		},

		// audit-fix verifications ----------------------------------------
		{
			ID: "observer-origin-guard", Category: "audit", Title: "Observer Origin guard (COV-5)",
			Description: "Open a WS upgrade against the observer /stream with Origin: http://evil.example. Pass if the server refuses the upgrade (non-101 response).",
			run:         r.runOriginGuard,
		},
		{
			ID: "page-csp-headers", Category: "audit", Title: "Page CSP / nosniff / frame-deny (S4)",
			Description: "GET / on this very page server, assert Content-Security-Policy, X-Content-Type-Options, X-Frame-Options, Referrer-Policy are all present.",
			run:         r.runCSPHeaders,
		},
		{
			ID: "integration-test", Category: "audit", Title: "End-to-end integration test",
			Description: "Run `go test -tags integration -timeout 5m ./tests/...`. Spins up testcontainers Postgres+Redis+MinIO, builds api+worker fresh, asserts the four subtests. Takes ~8s after first container pull.",
			run:         r.runIntegrationTest,
		},

		// manual-only cards ----------------------------------------------
		{
			ID: "manual-b6-reset-broadcasts", Category: "manual", Title: "B6 — reset broadcasts to all clients",
			Description: "Open this page in two browser tabs, click 'Reset stats' in tab 1. Tab 2's API key metrics table should clear within ~500ms. Pre-fix only the issuing tab reset.",
			Manual:      true,
			Steps: []string{
				"Open http://localhost:8090 in a second browser tab",
				"In tab 1, click the 'Reset stats' button at the top right",
				"Look at tab 2 — its 'API KEY METRICS' table should clear too",
			},
		},
		{
			ID: "manual-b1-redis-resilience", Category: "manual", Title: "B1 — webhook survives Redis blip",
			Description: "Pause Redis briefly while a shorten job is in flight, then resume. The shorten task should re-deliver and the webhook should fire, not be silently lost (pre-fix the swallowed enqueue error meant the webhook vanished).",
			Manual:      true,
			Steps: []string{
				"docker compose -f deploy/docker-compose.yml pause redis",
				"POST /shorten via this page's 'Shorten — 202 Accepted' test (it'll appear to succeed)",
				"docker compose -f deploy/docker-compose.yml unpause redis",
				"Watch the log audit panel: a webhook_sent event for that job_id should appear within ~30s",
			},
		},
		{
			ID: "manual-b8-minio-failure", Category: "manual", Title: "B8 — Stat failure surfaces, not size=0",
			Description: "Pause MinIO before a webhook delivery is attempted. The handler should fail the attempt (webhook_failed event) instead of silently delivering with size_bytes=0.",
			Manual:      true,
			Steps: []string{
				"docker compose -f deploy/docker-compose.yml pause minio",
				"Click 'Shorten — 202 Accepted' here, wait ~5s",
				"Watch the log audit panel: a webhook_failed event with error_class set should appear",
				"docker compose -f deploy/docker-compose.yml unpause minio",
			},
		},
		{
			ID: "manual-b9-worker-graceful-shutdown", Category: "manual", Title: "B9 — health-server shutdown logged",
			Description: "Stop the worker process. If health.Shutdown returns an error during the 5s grace window, the worker logs 'health server shutdown'. Pre-fix this was silently discarded.",
			Manual:      true,
			Steps: []string{
				"In the worker terminal, Ctrl-C",
				"Look at the last log lines: 'shutdown complete' should appear; if any health.Shutdown error happened it now shows alongside",
			},
		},
		{
			ID: "manual-b5-sweeper-race", Category: "manual", Title: "B5 — sweeper nulls column before MinIO delete",
			Description: "Wait QR_OBJECT_TTL (15m default) after a successful shorten. The sweeper now nulls qr_object FIRST, then deletes the MinIO object — so a concurrent webhook handler can't Stat a missing key.",
			Manual:      true,
			Steps: []string{
				"Run 'Shorten — 202 Accepted', note the time",
				"Wait > 15m (or shorten QR_OBJECT_TTL in worker env)",
				"Watch the log audit: 'reclaimed expired qr objects' from worker",
			},
		},
		{
			ID: "manual-k8s-walkthrough", Category: "manual", Title: "K8s walkthrough (S1, S2, S3, migrate Job)",
			Description: "Optional: bring up the kind cluster and verify Pod Security restricted contexts, NetworkPolicy SSRF egress block, Helm required-token gate, migration Job. See docs/MANUAL_TESTING.md §11.",
			Manual:      true,
			Steps: []string{
				"kind version && helm version  (skip if not installed)",
				"make k8s-up  (will fail without observerIngestToken — that IS S3 working)",
				"helm upgrade --install shortlink deploy/k8s --set image.tag=dev --set secrets.observerIngestToken=$(openssl rand -hex 16) --wait",
				"kubectl get deploy -o yaml | grep -A8 securityContext",
				"kubectl get networkpolicy",
			},
		},
		{
			ID: "manual-spec-drift", Category: "manual", Title: "SPEC-only COV-3/6/7/8/9/10/11/12/15/17/19",
			Description: "Pure SPEC reconciliations from the M9 audit. No code change — read docs/SPEC.md sections §3, §5, §10, §12, §13.",
			Manual:      true,
		},
	}
}

// ----------------------------------------------------------------------------
// per-case implementations

func (r *runner) runShortenHappy(ctx context.Context) *testResult {
	key, miss := r.keyByTier("pro")
	if miss != nil {
		return miss
	}
	resp, body, hdrs, err := r.postShorten(ctx, key, "https://example.com/test-console-happy", r.sinkURL, "")
	if err != nil {
		return &testResult{Expected: "202 Accepted", Actual: "request error", Details: err.Error(), ErrorClass: "transport"}
	}
	res := &testResult{
		StatusCode: resp.StatusCode, Headers: hdrs, Body: truncateBytes(body, 512),
		Expected: "202 Accepted with job_id",
	}
	if resp.StatusCode != http.StatusAccepted {
		res.Actual = fmt.Sprintf("%d %s", resp.StatusCode, resp.Status)
		return res
	}
	var parsed struct {
		JobID string `json:"job_id"`
	}
	if jerr := json.Unmarshal(body, &parsed); jerr != nil || parsed.JobID == "" {
		res.Actual = "202 but body missing job_id"
		return res
	}
	res.Passed = true
	res.Actual = "202 + job_id=" + parsed.JobID
	return res
}

func (r *runner) runCustomSlugAndRedirect(ctx context.Context) *testResult {
	key, miss := r.keyByTier("pro")
	if miss != nil {
		return miss
	}
	slug := "tc-" + randHex(4)
	target := "https://example.com/test-console-redirect"
	resp, body, _, err := r.postShorten(ctx, key, target, r.sinkURL, slug)
	if err != nil {
		return &testResult{Expected: "202 then 302 redirect", Actual: "shorten request error", Details: err.Error(), ErrorClass: "transport"}
	}
	if resp.StatusCode != http.StatusAccepted {
		return &testResult{Expected: "202 then 302 redirect", Actual: fmt.Sprintf("shorten returned %d", resp.StatusCode), Body: truncateBytes(body, 256), StatusCode: resp.StatusCode}
	}
	// Wait for the worker to claim+finalize. ~1s is comfortable; bail at 5s.
	deadline := time.Now().Add(5 * time.Second)
	var redirResp *http.Response
	var redirBody []byte
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return &testResult{Expected: "302 redirect", Actual: "context cancelled while polling", ErrorClass: "timeout"}
		case <-time.After(250 * time.Millisecond):
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.apiBase+"/"+slug, nil)
		// Don't follow the redirect -- we want to inspect 302 itself.
		noRedir := *r.client
		noRedir.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		rresp, rerr := noRedir.Do(req)
		if rerr != nil {
			continue
		}
		redirBody, _ = io.ReadAll(rresp.Body)
		rresp.Body.Close()
		redirResp = rresp
		if rresp.StatusCode == http.StatusFound {
			break
		}
	}
	if redirResp == nil {
		return &testResult{Expected: "302 redirect", Actual: "no GET succeeded before timeout", ErrorClass: "timeout"}
	}
	loc := redirResp.Header.Get("Location")
	res := &testResult{
		StatusCode: redirResp.StatusCode, Headers: map[string]string{"Location": loc},
		Body:     truncateBytes(redirBody, 256),
		Expected: fmt.Sprintf("302 with Location: %s", target),
		Actual:   fmt.Sprintf("%d  Location=%q", redirResp.StatusCode, loc),
	}
	if redirResp.StatusCode == http.StatusFound && loc == target {
		res.Passed = true
	}
	return res
}

func (r *runner) runBadAPIKey(ctx context.Context) *testResult {
	resp, body, hdrs, err := r.postShorten(ctx, "sl_live_bogus_xxxxxxxxxxxxxxxxxxxx", "https://example.com/x", r.sinkURL, "")
	if err != nil {
		return &testResult{Expected: "401 Unauthorized", Actual: "request error", Details: err.Error(), ErrorClass: "transport"}
	}
	return passWhenStatus(resp.StatusCode, http.StatusUnauthorized, "401 Unauthorized", body, hdrs)
}

func (r *runner) runSSRF(ctx context.Context) *testResult {
	key, miss := r.keyByTier("pro")
	if miss != nil {
		return miss
	}
	resp, body, hdrs, err := r.postShorten(ctx, key, "https://example.com/ssrf", "http://10.0.0.99:8080/sink", "")
	if err != nil {
		return &testResult{Expected: "422 Unprocessable Entity", Actual: "request error", Details: err.Error(), ErrorClass: "transport"}
	}
	res := passWhenStatus(resp.StatusCode, http.StatusUnprocessableEntity, "422 (RFC1918 webhook blocked)", body, hdrs)
	if !res.Passed && resp.StatusCode == http.StatusAccepted {
		res.Details = "api accepted the request — is SSRF_ALLOWLIST too permissive (e.g. includes 10.0.0.0/8)?"
	}
	return res
}

func (r *runner) runURLBlocklist(ctx context.Context) *testResult {
	key, miss := r.keyByTier("pro")
	if miss != nil {
		return miss
	}
	resp, body, hdrs, err := r.postShorten(ctx, key, "https://blocked-domain.example/spam", r.sinkURL, "")
	if err != nil {
		return &testResult{Expected: "422 Unprocessable Entity", Actual: "request error", Details: err.Error(), ErrorClass: "transport"}
	}
	res := passWhenStatus(resp.StatusCode, http.StatusUnprocessableEntity, "422 (host in URL_BLOCKLIST)", body, hdrs)
	if !res.Passed {
		switch resp.StatusCode {
		case http.StatusAccepted:
			res.Details = "api returned 202 — URL_BLOCKLIST isn't configured. Restart the api with `URL_BLOCKLIST=blocked-domain.example`."
		case http.StatusBadRequest:
			res.Details = "api returned 400 — that's the pre-audit (COV-4) behaviour. Make sure you're on the fixed binary."
		}
	}
	return res
}

func (r *runner) runRateLimitBurst(ctx context.Context) *testResult {
	key, miss := r.keyByTier("free")
	if miss != nil {
		return miss
	}
	const n = 12
	var n429, n202, nOther int
	codes := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		resp, _, _, err := r.postShorten(ctx, key, fmt.Sprintf("https://example.com/burst-%d", i), r.sinkURL, "")
		if err != nil {
			nOther++
			continue
		}
		codes = append(codes, resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			n429++
		case http.StatusAccepted:
			n202++
		default:
			nOther++
		}
	}
	res := &testResult{
		Expected: "at least one 429 across 12 requests",
		Actual:   fmt.Sprintf("%d × 202, %d × 429, %d × other", n202, n429, nOther),
		Details:  fmt.Sprintf("status codes: %v", codes),
	}
	if n429 >= 1 {
		res.Passed = true
	}
	return res
}

func (r *runner) runRateLimitHeaders(ctx context.Context) *testResult {
	key, miss := r.keyByTier("free")
	if miss != nil {
		return miss
	}
	// Try a few times in case the previous burst hasn't filled the window.
	var last *http.Response
	var body []byte
	var hdrs map[string]string
	for i := 0; i < 15; i++ {
		if ctx.Err() != nil {
			return &testResult{Expected: "429 with rate-limit headers", Actual: "context cancelled before any 429", ErrorClass: "timeout"}
		}
		resp, b, h, err := r.postShorten(ctx, key, "https://example.com/over", r.sinkURL, "")
		if err != nil {
			continue
		}
		last, body, hdrs = resp, b, h
		if resp.StatusCode == http.StatusTooManyRequests {
			break
		}
	}
	if last == nil {
		return &testResult{Expected: "429 with rate-limit headers", Actual: "no response", ErrorClass: "transport"}
	}
	res := &testResult{StatusCode: last.StatusCode, Headers: hdrs, Body: truncateBytes(body, 256), Expected: "429 + X-RateLimit-* + Retry-After"}
	if last.StatusCode != http.StatusTooManyRequests {
		res.Actual = fmt.Sprintf("%d %s — never hit 429 within 15 attempts (window already drained?)", last.StatusCode, last.Status)
		return res
	}
	want := []string{"X-Ratelimit-Limit", "X-Ratelimit-Remaining", "X-Ratelimit-Reset", "Retry-After"}
	var missing []string
	for _, h := range want {
		if hdrs[h] == "" {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		res.Passed = true
		res.Actual = "429 with all 4 headers present"
	} else {
		res.Actual = "429 but missing: " + strings.Join(missing, ", ")
	}
	return res
}

func (r *runner) runSlugConflict(ctx context.Context) *testResult {
	key, miss := r.keyByTier("pro")
	if miss != nil {
		return miss
	}
	slug := "tc-conflict-" + randHex(4)
	r1, _, _, err := r.postShorten(ctx, key, "https://example.com/a", r.sinkURL, slug)
	if err != nil {
		return &testResult{Expected: "202 then 409", Actual: "first POST errored: " + err.Error(), ErrorClass: "transport"}
	}
	if r1.StatusCode != http.StatusAccepted {
		return &testResult{Expected: "202 then 409", Actual: fmt.Sprintf("first POST returned %d", r1.StatusCode), StatusCode: r1.StatusCode}
	}
	r2, body2, hdrs2, err := r.postShorten(ctx, key, "https://example.com/b", r.sinkURL, slug)
	if err != nil {
		return &testResult{Expected: "202 then 409", Actual: "second POST errored: " + err.Error(), ErrorClass: "transport"}
	}
	res := &testResult{StatusCode: r2.StatusCode, Headers: hdrs2, Body: truncateBytes(body2, 256), Expected: "second POST -> 409 Conflict"}
	if r2.StatusCode == http.StatusConflict {
		res.Passed = true
		res.Actual = "409 as expected"
	} else {
		res.Actual = fmt.Sprintf("second POST returned %d", r2.StatusCode)
	}
	return res
}

func (r *runner) runPrometheusTargets(ctx context.Context) *testResult {
	u := r.prometheusURL + "/api/v1/targets?state=active"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return &testResult{Expected: "Prometheus reachable, all targets UP", Actual: "request error", Details: err.Error(), ErrorClass: "transport"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &testResult{Expected: "200 OK from /api/v1/targets", Actual: fmt.Sprintf("%d %s", resp.StatusCode, resp.Status), StatusCode: resp.StatusCode, Body: truncateBytes(body, 256)}
	}
	var parsed struct {
		Data struct {
			ActiveTargets []struct {
				Health     string `json:"health"`
				ScrapeURL  string `json:"scrapeUrl"`
				LastError  string `json:"lastError"`
				ScrapePool string `json:"scrapePool"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal(body, &parsed); jerr != nil {
		return &testResult{Expected: "JSON targets list", Actual: "malformed response", Details: jerr.Error(), Body: truncateBytes(body, 256)}
	}
	if len(parsed.Data.ActiveTargets) == 0 {
		return &testResult{Expected: "3 active targets, all UP", Actual: "0 active targets returned", Body: truncateBytes(body, 256)}
	}
	var down []string
	for _, t := range parsed.Data.ActiveTargets {
		if t.Health != "up" {
			msg := t.ScrapeURL
			if t.LastError != "" {
				msg += " (" + t.LastError + ")"
			}
			down = append(down, msg)
		}
	}
	res := &testResult{Expected: fmt.Sprintf("all %d targets UP", len(parsed.Data.ActiveTargets))}
	if len(down) == 0 {
		res.Passed = true
		res.Actual = fmt.Sprintf("%d targets, all UP", len(parsed.Data.ActiveTargets))
	} else {
		res.Actual = fmt.Sprintf("%d targets, %d DOWN", len(parsed.Data.ActiveTargets), len(down))
		res.Details = "DOWN targets:\n  - " + strings.Join(down, "\n  - ")
	}
	return res
}

func (r *runner) runGrafanaHealthz(ctx context.Context) *testResult {
	u := r.grafanaURL + "/api/health"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return &testResult{Expected: "200 OK + body contains 'ok'", Actual: "request error", Details: err.Error(), ErrorClass: "transport"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	res := &testResult{StatusCode: resp.StatusCode, Body: truncateBytes(body, 256), Expected: "200 OK + database \"ok\""}
	if resp.StatusCode == http.StatusOK && strings.Contains(strings.ToLower(string(body)), "\"ok\"") {
		res.Passed = true
		res.Actual = "200 OK + body has \"ok\""
	} else {
		res.Actual = fmt.Sprintf("%d %s", resp.StatusCode, resp.Status)
	}
	return res
}

func (r *runner) runOriginGuard(ctx context.Context) *testResult {
	wsURL, err := url.Parse(r.observerURL + "/stream")
	if err != nil {
		return &testResult{Expected: "non-101 to bad-Origin upgrade", Actual: "invalid observer URL: " + err.Error(), ErrorClass: "config"}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, wsURL.String(), nil)
	// Forge an Upgrade attempt -- the broadcaster's CheckOrigin rejects with
	// 403 before completing the upgrade, so we can probe without a real
	// websocket client.
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGVzdC1jb25zb2xlLWtleS09")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := r.client.Do(req)
	if err != nil {
		return &testResult{Expected: "non-101 to bad-Origin upgrade", Actual: "request error: " + err.Error(), ErrorClass: "transport"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	res := &testResult{StatusCode: resp.StatusCode, Body: truncateBytes(body, 256), Expected: "403 (or any non-101) from bad-Origin WS upgrade"}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		res.Passed = true
		res.Actual = fmt.Sprintf("%d %s — Origin rejected", resp.StatusCode, resp.Status)
	} else {
		res.Actual = "101 Switching Protocols — upgrade succeeded despite bad Origin (regression)"
	}
	return res
}

func (r *runner) runCSPHeaders(ctx context.Context) *testResult {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.pageBase+"/", nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return &testResult{Expected: "CSP + nosniff + frame-deny + Referrer-Policy", Actual: "request error", Details: err.Error(), ErrorClass: "transport"}
	}
	defer resp.Body.Close()
	hdrs := headerMap(resp.Header)
	want := []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"}
	var missing, present []string
	for _, h := range want {
		if hdrs[h] == "" {
			missing = append(missing, h)
		} else {
			present = append(present, h)
		}
	}
	res := &testResult{StatusCode: resp.StatusCode, Headers: hdrs, Expected: strings.Join(want, ", ")}
	if len(missing) == 0 {
		res.Passed = true
		res.Actual = "all 4 present (" + strings.Join(present, ", ") + ")"
	} else {
		res.Actual = "missing: " + strings.Join(missing, ", ")
	}
	return res
}

// runIntegrationTest shells out to `go test -tags integration` against the
// tests/ directory. Long-running (~8s after first container pull). Output is
// captured wholesale and returned as Details; the page renders it in a <pre>.
func (r *runner) runIntegrationTest(ctx context.Context) *testResult {
	root, err := repoRoot()
	if err != nil {
		return &testResult{Expected: "PASS from integration suite", Actual: "could not locate repo root", Details: err.Error(), ErrorClass: "config"}
	}
	args := []string{"test", "-tags", "integration", "-timeout", "5m", "./tests/..."}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	res := &testResult{Expected: "PASS", Details: strings.TrimSpace(string(out))}
	if err == nil {
		res.Passed = true
		res.Actual = "PASS"
	} else {
		res.Actual = "go test exited non-zero"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.ErrorClass = "timeout"
		}
	}
	return res
}

// ----------------------------------------------------------------------------
// helpers

// postShorten is the workhorse for every POST /shorten case. Returns the
// response (already consumed body), the body bytes, a compact header map for
// the page to display, and a transport error if the request never landed.
func (r *runner) postShorten(ctx context.Context, apiKey, target, webhookURL, customSlug string) (*http.Response, []byte, map[string]string, error) {
	payload := map[string]string{
		"url":         target,
		"webhook_url": webhookURL,
	}
	if customSlug != "" {
		payload["custom_slug"] = customSlug
	}
	buf, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.apiBase+"/shorten", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body, headerMap(resp.Header), nil
}

// keyByTier returns the first keysfile entry of the given tier and an error
// result if none are configured -- the test then short-circuits with a clear
// "no key of tier X" message instead of failing for an unrelated reason.
func (r *runner) keyByTier(tier string) (string, *testResult) {
	if r.keys == nil {
		return "", &testResult{Expected: "tier=" + tier + " key from keys.yaml", Actual: "keys file not loaded", ErrorClass: "config"}
	}
	for _, k := range r.keys.Keys {
		if strings.EqualFold(k.Tier, tier) {
			return k.Key, nil
		}
	}
	return "", &testResult{Expected: "tier=" + tier + " key from keys.yaml", Actual: "no " + tier + "-tier key configured — run `make keys`", ErrorClass: "config"}
}

func passWhenStatus(got, want int, expected string, body []byte, hdrs map[string]string) *testResult {
	res := &testResult{
		StatusCode: got, Headers: hdrs, Body: truncateBytes(body, 256),
		Expected: expected,
		Actual:   fmt.Sprintf("HTTP %d", got),
	}
	if got == want {
		res.Passed = true
	}
	return res
}

// headerMap flattens single-value headers into a string map so the frontend
// can render them in a fixed table without dealing with multi-values. Keeps
// only the first value per header -- adequate for everything we surface.
func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) == 0 {
			continue
		}
		out[k] = v[0]
	}
	return out
}

// truncateBytes converts the response body to a display string, capping at n
// bytes so we don't blow up the page with a multi-MB blob. Named distinctly
// from attack.go's string-flavoured `truncate` to avoid a package collision.
func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "… (+" + fmt.Sprint(len(b)-n) + " bytes)"
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// repoRoot walks up from the binary's working dir until it finds go.mod.
// Used by the integration-test case so `go run ./cmd/loadtest` from anywhere
// in the tree still locates the right test target.
func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(wd)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", wd)
		}
		dir = parent
	}
}
