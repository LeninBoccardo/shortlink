# Design Decisions

Running log of the non-obvious judgment calls made while building ShortLink.
SPEC.md is the *what*; this file is the *why* — especially the trade-offs, the
spec deviations, and the audit-driven changes. New entries go at the bottom of
their section, with the milestone in which the call was made.

The format is intentionally compact: heading + 2-5 lines of context + the
tradeoff or what was rejected. ADR-lite, not full IETF.

---

## 1. Process model & binaries

### One repo, separate binaries per role (M1 → M5)
M1 shipped `cmd/api` (gateway) and `cmd/worker` as a single combined binary.
M2 split them into two binaries sharing `internal/`. M4 added `cmd/observer`;
M5 added `cmd/loadtest`. Today: api / worker / observer / loadtest / keygen /
migrate. **Why split:** worker scaling is queue-depth-driven (KEDA in M8);
api scaling is request-rate-driven. Different lifecycles, different probes,
different resource shapes. **Tradeoff:** more boilerplate per binary
(`main.go`, config, signal handling); accepted because shared code lives in
`internal/`.

### Worker is the only thing that touches the queue *and* the DB *and* MinIO (M2)
The shorten job claims the row, generates QR, uploads, finalizes, and enqueues
the webhook job. **Why:** lease-token (`updated_at`) row-level idempotency
needs single-writer guarantees per job; coalescing the chain into one handler
avoids cross-process lease handoff. **Rejected:** SPEC-style per-step jobs
(claim → qr → upload → finalize as 4 enqueues) because the lease semantics
get worse.

### Webhook delivery is a separate job, not a step of the shorten job (M2)
A slow customer endpoint must never hold a shorten worker slot. Separate
`webhook` queue, separate handler, separate asynq retry schedule. Worker
shorten-job ends with `enqueueWebhook(jobID)`. **Tradeoff:** Two queues to
observe; accepted as the only way to give webhooks honest SLOs.

---

## 2. Async pipeline

### asynq for the queue (M2)
`hibiken/asynq` over Redis. Reasons: built-in retry + dead-letter (archived
set), at-most-once semantics with explicit single-delivery guarantee, queue
isolation, mature Inspector API for the observer poller. **Rejected:**
`river` (newer, less battle-tested then), homegrown over LPUSH/BRPOP (would
have rebuilt retry + DLQ ourselves).

### Unconditional re-claim model (deviates from SPEC §4.2) (M2)
SPEC originally proposed a lease-cutoff: a re-claim only happens if the prior
attempt's lease has expired. The code claims the row unconditionally on every
delivery (relying on asynq's single-delivery guarantee — only one worker
attempt is live at a time). **Why:** the lease check added correctness surface
without buying anything asynq doesn't already enforce; the unconditional path
is simpler and easier to reason about. SPEC §4.2/§7/§14/§16 were reconciled
to match the code in commit `2314c76`. **Tradeoff:** if asynq's
single-delivery guarantee ever broke, two workers could race; we accept that
risk because asynq's queue semantics are foundational anyway.

### `updated_at` as the lease token (M2)
`FinalizeShortURL` / `FailShortURL` use `WHERE updated_at = $lease` so a worker
that lost its lease to a re-claim writes zero rows and logs+returns. **Why:**
no extra lease column to manage, no clock skew (it's a Postgres-side
timestamp), naturally monotonic per row. **Rejected:** explicit
`lease_token` column (extra schema), in-process mutex (doesn't survive crash).

---

## 3. Data layer

### `pgx/v5` + `sqlc` (M1)
Typed query layer with no ORM magic. `sqlc.yaml` emits `db.Queries` + a
generated `db.Querier` interface (kept for future mock-based tests — see
[AUDIT.md](AUDIT.md) D3). **Rejected:** `gorm` (runtime SQL surprises, no
sqlc-style compile-time checking), `database/sql` raw (would have written the
type plumbing ourselves).

### `goose` for migrations (M1)
Single-binary migration runner (`cmd/migrate`), schema lives in `migrations/`
as `NNNN_*.sql`. **Why:** simpler than embedded migration libraries, plays
well with a future Kubernetes pre-upgrade Job (M8). **Tradeoff:** no
SQL-as-Go migrations; that hasn't bitten yet.

### One `PG_POOL_SIZE` env var for both binaries (M1, deferred — see AUDIT.md P7)
SPEC §14 wants 8 for api / 4 for worker; today both pull from one var.
Acceptable for local; revisit during M8 when both get distinct ConfigMaps.

### `hit_count` denormalized counter on `short_urls` (M1, deferred — see AUDIT.md P1)
Every redirect runs `UPDATE ... SET hit_count = hit_count+1`. Known to cause
row-lock contention + MVCC bloat on viral links. **Why kept:** SPEC §5 calls
for the field; fixing it means a spec revisit. Tracked for v2.

---

## 4. Auth & rate limiting

### API keys stored only as SHA-256 hashes (M1)
Raw key is shown to the user once at `make keys` time and never persisted.
Validation does `SHA-256(raw)` and looks up by hash. **Why:** a database
dump of `api_keys` can't be used to make requests. **Tradeoff:** lost keys
require reissuance (no recovery flow); fine for a portfolio project.

### SETNX-throttled `last_used_at` updates (M3)
On a 5s sliding window the toucher does `SET key NX EX 5` — if the key
existed, skip the DB write. Without this, every authenticated request would
write to `api_keys`, drowning the workload in a useless update storm. **Why
not a queue:** Redis SETNX is both cheaper and gives the throttle for free.

### Per-key sliding-window via Redis sorted set + Lua (M3)
The limiter atomically: trims old entries, counts current, adds the new entry
(if allowed), returns count + reset-at. Atomicity is critical — three round
trips would let two requests race past the window edge. **Why Lua over a
distributed-lock dance:** the EVAL is server-side atomic, no locks needed.

### Free / Pro / Unlimited tiers, env-driven limits (M3)
`Tier` lives on `api_keys`. The middleware reads the tier and picks
`RATE_LIMIT_FREE` / `RATE_LIMIT_PRO`; tier `unlimited` skips the limiter
entirely (Redis call elided). **Why no DB-driven limits:** changing a limit
shouldn't require a migration.

### Fail-open on Redis errors (M3)
If the limiter's Redis call errors, the middleware logs and forwards — we
don't 503 the whole API on a Redis blip. **Tradeoff:** during a Redis outage
a key could exceed its limit briefly; that's better than a global outage.

### No auth cache (M3, deferred — see AUDIT.md P4)
Every `POST /shorten` does two DB queries (hash lookup + touch). An LRU
cache keyed by hash would cut this to ~zero, but the limiter is the real
hot path anyway. Tracked.

---

## 5. Webhook delivery

### HMAC-SHA256 signing with per-key secret (M1)
`X-ShortLink-Signature: sha256=<hex>` on every webhook POST; the body is the
HMAC payload. Clients verify with their secret. **Why per-key:** rotating
one customer's secret doesn't affect others.

### SSRF validation runs twice — gateway *and* worker (M1)
Gateway validates on `POST /shorten`; worker re-validates before delivery
because DNS records can change between enqueue and execution. **Why:**
without re-validation, an attacker could submit a public URL whose DNS
flips to RFC1918 mid-retry-cycle. **Tradeoff:** small latency cost per
delivery attempt; accepted.

### Per-attempt re-presign of QR download URL (M1)
Every webhook attempt generates a fresh signed URL with the full TTL. **Why:**
asynq retry schedule can push attempt 5 hours past attempt 1; a stale signed
URL would 403. **Tradeoff:** an extra MinIO call per attempt; cheap.

### `webhook` queue silently skips post-sweep deliveries (M2, deferred — see AUDIT.md B4)
If a webhook job runs after the QR object has been swept (worker downtime
+ retry backoff), the handler returns nil without delivering. Real but rare
in normal operation; tracked.

---

## 6. Object storage

### MinIO (S3-compatible) with signed URLs (M1)
QR PNGs are private MinIO objects; the webhook payload carries a presigned
URL with `SIGNED_URL_TTL`. **Why not public:** customer QRs shouldn't be
guessable by bucket-scanning.

### Object keys: `YYYY/MM/DD/{jobID}.png` (M1)
Date-partitioned for cheap "delete everything older than N days" lifecycle
rules. **Tradeoff:** can't easily list by api_key; not needed for current
features.

### `Stat()` round-trip per webhook attempt to fill `size_bytes` (M1, deferred — see AUDIT.md P6)
Could be cached at finalize; not worth a schema change yet.

---

## 7. Events & observer hub

### Event-push model, not log-scrape (M4, per SPEC §10)
Services emit typed events to `POST /ingest`. The observer is **not** a log
reader. **Why:** structured at the source means no parsing, no log-format
coupling, no Loki/Promtail to operate in v1. **Rejected:** Grafana Loki —
explicitly v2 (SPEC §15).

### Best-effort emitter (events package never blocks) (M4)
`internal/events` has a small bounded channel; on overflow the event is
dropped and logged at debug. POST timeout is 500ms; failures are silent.
**Why:** an emit must never fail or slow a request/job. **Tradeoff:** under
sustained pressure events get dropped — the observer drop counter surfaces
this in the UI.

### Observer aggregator tick = 100ms; broadcaster tick = 500ms; poller tick = 5s (M4)
- 100ms: prunes expired log entries + latency samples (perceived snappiness)
- 500ms: pushes the stats snapshot to WebSocket clients (eyeball-friendly
  refresh without flooding)
- 5s: Redis poll for queue depth + pod heartbeats (cheap, low blast radius)

### Custom CheckOrigin on the observer WebSocket (M4)
The showcase page is served from `:8090` while the observer listens on
`:9090`, so the WS connection is cross-origin and `gorilla/websocket`'s
default `CheckOrigin` would reject it. CheckOrigin reads
`OBSERVER_ALLOWED_ORIGINS` and (post-audit S3) requires the Origin header
to be present — empty Origin was a wscat/curl bypass.

### Bearer token on `/ingest`, defaults empty (M5 audit S2)
`OBSERVER_INGEST_TOKEN` gates `POST /ingest` via `Authorization: Bearer ...`
with constant-time compare. **Default empty** keeps local dev frictionless
(observer logs WARN at startup so it's visible). Production sets the same
value across api/worker/loadtest/observer.

### Circular log buffer (M5 audit P8)
The log ring is a pre-allocated `[]LogEntry` with `head` + `count`; append is
O(1). **Why:** the previous prepend-slice was O(N) per event and reallocated
under load. Walks remain newest→oldest via a small `logIndex(i)` helper.

### Pod heartbeats in Redis (`pod:{POD_ID}:alive`, 15s TTL, 5s refresh) (M4)
Workers self-register; observer counts live keys via SCAN. **Why not query
the k8s API:** the observer would need cluster-RBAC to do that; SCAN works
the same locally and in k8s.

---

## 8. Metrics (M7)

### One `internal/metrics` package, default Prometheus registry (M7)
All collectors live in one file, register via `promauto` on the default
registry. **Tradeoff:** binaries that never increment a series still expose
it as zero. **Rejected:** per-binary registries with conditional imports —
the dashboards stay flexible (`sum without (job)`) and the metric noise is
negligible.

### Low-cardinality labels only — never `api_key` or URL (M7, per SPEC §17)
Labels are `tier`, `status`, `queue`, `decision`, `source`. Per-key
breakdowns live in the observer hub (in-memory, TTL-pruned). **Why:**
unbounded labels would crater Prometheus; the showcase already has the
per-key view it needs.

### `source` label has an "other" fallback (M7)
`sourceLabel()` in the observer collapses unknown values into `other` so
a misconfigured emitter can't grow the label set without bound. **Why:**
defense in depth — labels go in by HTTP-decoded JSON; we trust no input.

### Default Prometheus histogram buckets (`prometheus.DefBuckets`) (M7)
Covers 5ms–10s in 11 buckets, which fits every duration we measure (job,
QR, webhook). **Tradeoff:** webhook delivery has a 10s timeout, so the
largest bucket *is* the deadline; observed p99 will pile up at +Inf if
many requests timeout. Acceptable for v1; revisit if alerting needs more
resolution.

### Job-completion buckets: `complete` / `error` / `dlq` (M7)
`error` = retryable failure (asynq will retry). `dlq` = final attempt
failed (asynq archived). **Why three buckets:** dashboards can show
"is this a transient blip or a real outage" — `error` flapping is normal,
sustained `dlq` is not. The handler uses `queue.IsLastAttempt(ctx)` to
pick the right bucket.

### Worker `/metrics` not published to host (M7, per SPEC §13)
Compose `worker` service exposes 8081 only on the compose network;
Prometheus scrapes via host.docker.internal (locally) or pod IPs (k8s).

### Observer's M4 `/metrics` stub replaced wholesale (M7)
Dropped `observer_events_{received,rejected,dropped}_total` text output and
the three `atomic.Int64` fields backing it. The same signals now flow as
`shortlink_events_{received,rejected}_total` + `_dropped_total{source}`.
**Why:** one set of names, one collector, no stub to maintain.

### Two single-panel-uid dashboards, not one multi-panel + d-solo (M7)
`jobs-error-rate` and `qr-queue-depth` are separate dashboard JSONs whose
uids match the `data-panel` attrs in the showcase HTML. **Why:** iframe
src is `/d/<uid>?kiosk=tv` — a clean 1:1 mapping. **Rejected:** one
dashboard with N panels embedded via `/d-solo/<uid>?panelId=N` — would
have coupled panel IDs into HTML.

### Iframe URL: `?kiosk=tv&theme=dark&refresh=5s` (M7)
- `kiosk=tv` strips Grafana chrome
- `theme=dark` matches the showcase page
- `refresh=5s` matches the WebSocket cadence

### Prometheus retention: 2h (M7)
`--storage.tsdb.retention.time=2h`. **Why:** local dev produces no
long-term data worth keeping; restarts are frequent. Production would set
14d+.

### Grafana anonymous Viewer + `allow_embedding = true` (M7, per SPEC §11)
Showcase page must render iframes without a login. Anonymous role is
Viewer (read-only). Admin login still works for editing. **Why this is
acceptable in this repo:** local-dev only; production deployment would
add an auth proxy.

---

## 9. Frontend (M6)

### Vanilla HTML + CSS + JS, embedded via `go:embed` (M6)
No Node toolchain, no framework, no build step. **Why considered SolidJS
and rejected:** SPEC §11 explicitly mandates "Vanilla JS + WebSocket (no
framework)"; the page is ~10 rows × 500ms updates; adding a JS toolchain
to a Go portfolio project would be over-engineering. **Tradeoff:** more
manual DOM code (`rowByHash` cache, filter-class toggling) instead of
reactive bindings; the code is still ~400 lines.

### Page lifecycle: stay up after attack until Ctrl-C (M6)
M5 shut everything down when the attack ended; M6 keeps the page and
sink alive so the user can study the final dashboard. A `shutdown` channel
closed by a watcher goroutine handles both in-attack and post-attack
signal paths. **Why:** the showcase value is *seeing* what happened, not
*running* the attack.

### Templated config injection: `window.SHORTLINK_CONFIG = {{ .Config }};` (M6)
Observer URL + Grafana URL + sink URL come from `--observer`/`--grafana`/
`--sink-url` flags and are JSON-encoded into a single global at page-serve
time. **Why:** the page works regardless of host/port without rebuilding.

### Filter dropdowns toggle a CSS class (`.filtered`), don't re-render (M6)
On filter change we add/remove `.filtered` on each `<li>`; CSS hides them.
**Why:** the log list can hold 500 entries — re-rendering on every
keystroke is wasteful.

### Iframes have a `.no-grafana` fallback (M7)
If `--grafana` flag is empty, JS adds `no-grafana` to the slot; CSS hides
the iframe and shows a fallback hint. **Why:** the page stays useful
without the M7 stack up.

---

## 10. Local stack & ports

### docker-compose holds **infra only**; api/worker/observer run on the host (M1+)
Compose runs Postgres + MinIO + Redis (M2), plus Prometheus + Grafana (M7).
`make run-api` / `run-worker` / `run-observer` run on the host with `go run`
for fast iteration. **Why:** in-loop edits don't require image rebuilds.
**Consequence:** `SSRF_ALLOWLIST=host.docker.internal,localhost,127.0.0.1`
needed locally so workers can reach the loadtest sink.

### Postgres on host **55432** (M1)
The dev machine runs native Postgres 16/17/18 on 5432–5434.
`POSTGRES_HOST_AUTH_METHOD=trust` is unreliable because initdb's default
scram rules for `127.0.0.1`/`::1` shadow it — so we set
`POSTGRES_PASSWORD=shortlink` explicitly.

### Observer on host **9090** (M4, changed from SPEC-canonical 9000)
MinIO claims host 9000 (S3 API). SPEC §4.3 and §14 reconciled in `9c392cf`.

### Prometheus on host **9091** (M7, changed from canonical 9090)
Observer already owns host 9090. Inside the compose network Prometheus
still listens on 9090; Grafana reaches it as `http://prometheus:9090`.

### Prometheus scrapes via `host.docker.internal:8080/8081/9090` (M7)
api/worker/observer run on the host in local dev. The compose service
declares `extra_hosts: ["host.docker.internal:host-gateway"]` so Linux
works too. **In k8s (M8):** these become pod targets discovered by labels;
metric names don't change.

### Showcase page on host **8090**, sink on host **8091** (M5/M6)
Loadtest binary serves both. Webhook delivery from the (host-bound) worker
reaches the sink via `localhost:8091` (allowed by SSRF).

---

## 11. Operational decisions worth recording

### `config/keys.yaml` is gitignored — never committed (M1)
Contains real API keys + per-key webhook HMAC secrets. `make keys`
regenerates it. **Known issue:** re-runs accumulate valid keys in the DB
without tracking the previous file's keys (AUDIT.md S1).

### No log aggregator in v1 (per SPEC §10/§15)
The observer hub *replaces* a log aggregator for the showcase. If raw log
aggregation is ever wanted, Loki slots in next to Grafana — but that's v2.

### Events package stays metrics-free (M7)
`internal/events` does not import `internal/metrics`. **Why:** the events
emitter is the lowest-level shared primitive; pulling in prometheus
collectors would force every importer (including future tooling) to
register them too. Drop counters at the observer side instead.

### Re-claim sweeper / dead-letter / DLQ: one mechanism per failure mode (M2)
- Crashed worker mid-attempt → asynq redelivery + unconditional re-claim
- Final attempt failed → asynq archive set + `FailShortURL` row mark
- Pending row whose worker never ran → sweeper periodically resets it
- Stale `processing` row → sweeper retires after `SWEEP_STALE_AGE`

These are deliberately separate; collapsing them would couple the failure
modes and make incidents harder to diagnose.

### Audit cadence: between milestones, not at the end (post-M2, post-M5)
Pattern proven on the M3+M4+M5 audit (15 findings, all fixed pre-M6 in
commits `6941fc3..1307dc7 + 3a9072f`). One big end-of-project audit
would dilute focus; small ones keep code fresh.
