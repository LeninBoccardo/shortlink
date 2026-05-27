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

### Per-binary pool sizing: `API_PG_POOL_SIZE` / `WORKER_PG_POOL_SIZE` (M1 → resolved post-v1-audit)
Started life as one `PG_POOL_SIZE` shared by both binaries; SPEC §14 always
wanted 8 for api / 4 for worker but the M1 cut-over kept it simple. Split out
once the helm chart was real and each binary had its own ConfigMap surface.

### `hit_count` denormalized counter on `short_urls` (M1, deferred — see AUDIT.md P1)
Every redirect runs `UPDATE ... SET hit_count = hit_count+1`. Known to cause
row-lock contention + MVCC bloat on viral links. **Why kept:** SPEC §5 calls
for the field, and v1's workload doesn't exercise the redirect path under
load (loadtest's vegeta only targets `POST /shorten`; SPEC §16 commits to
redirect *latency* not throughput) — at the project's actual demo scale, the
per-redirect UPDATE is invisible to Postgres. **v2 trigger condition** (so
this isn't a forever-deferral): promote when either the loadtest grows a
redirect-path attack mode or §16 adds a redirect-throughput SLO. Likely
landing shape when triggered: per-replica in-process aggregation + periodic
flush (one UPDATE per slug per ~1s interval), keeping `hits` as the source
of truth so the counter is a recoverable cache.

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

### docker-compose holds **infra only**; api/worker/observer run on the host (M1+, dev mode)
Compose runs Postgres + MinIO + Redis (M2), plus Prometheus + Grafana (M7).
`make run-api` / `run-worker` / `run-observer` run on the host with `go run`
for fast iteration. **Why:** in-loop edits don't require image rebuilds.
**Consequence:** `SSRF_ALLOWLIST=host.docker.internal,localhost,127.0.0.1`
needed locally so workers can reach the loadtest sink. **Post-M9:** this is
now the **dev mode** half of a two-mode local-dev story; the **full mode**
puts every binary in a container — see §13 "Two compose files instead of
one" for the rationale and the showcase-vs-iteration split.

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

### `config/keys.yaml` is gitignored — never committed (M1, post-M9 extended)
Contains real API keys + per-key webhook HMAC secrets. **Two writers now:**
the original `cmd/keygen` CLI (`make keys`, batch-overwrites with three
default-tier keys) AND the operator panel's `POST /api/keys/generate`
endpoint (single-key append-and-write per click — see §13 "keys.yaml is
canonical"). Both go through `internal/keysfile.Write`, which writes
atomically via tempfile + rename, so a crashed Generate doesn't leave a
truncated file. **Known issue:** `make keys` still overwrites the whole
file, so it nukes any keys the operator generated via the UI between
runs (AUDIT.md S1 covers the broader "re-runs accumulate untracked DB
keys" angle).

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

---

## 12. Kubernetes & deployment (M8)

### One multi-stage Dockerfile, not five (M8, deviates from SPEC §6)
SPEC §6 listed `Dockerfile.api / .worker / .observer / .loadtest / .migrate`
at the repo root. The chart ships one `Dockerfile` with
`--build-arg BINARY=name`. **Why:** the build steps were going to be
identical (`go mod download`, `go build ./cmd/<name>`); five files would
have been five identical copies. **Tradeoff:** loses the at-a-glance
"there's a Dockerfile per binary" grep target. SPEC §6 was updated to
match.

### Helm chart under `deploy/k8s/templates/`, not flat manifests (M8)
SPEC §6 listed flat YAML files under `deploy/k8s/`. The chart is now a
standard Helm structure (`Chart.yaml`, `values.yaml`, `templates/`).
**Why:** SPEC §12 already requires a Helm pre-install / pre-upgrade hook
for the migrate Job; once Helm is in the loop, the templates/ layout is
the convention. **Tradeoff:** one more level of nesting in the path; the
README in `deploy/k8s/` makes the entry point obvious.

### Distroless `static-debian12:nonroot` runtime image (M8)
Runtime stage is `gcr.io/distroless/static-debian12:nonroot`. **Why:**
zero shell, zero package manager, runs as uid 65532, ships with CA roots
already trusted. Future PodSecurity restricted policies pass automatically.
**Rejected:** scratch (no DNS resolver bundled), alpine (musl quirks +
shell increases attack surface).

### Migrations bypass PgBouncer; apps go through it (M8)
The migrate Job uses a separate `shortlink.databaseURLDirect` DSN that
points straight at Postgres on 5432. **Why:** goose runs DDL inside
transactions, and DDL is unsafe under transaction-mode pooling — a
prepared-statement sweep mid-migration would corrupt the run. Apps still
use the pooler via the standard `shortlink.databaseURL` helper.

### KEDA ScaledObject per logical queue (shorten + webhook) (M8)
One Redis trigger per asynq queue name, both pointed at the same worker
Deployment. **Why:** the worker handles both queues; KEDA needs to react
when either is hot. **Rejected:** a single combined `pending` trigger —
asynq's pending lists are partitioned per queue, there is no aggregate
key.

### NetworkPolicy at the worker pod with `0.0.0.0/0` minus RFC1918 (M8)
The egress rule allows public-internet HTTP/HTTPS (customer webhooks)
but explicitly `except`s `10/8 + 172.16/12 + 192.168/16 + 169.254/16 +
127/8`. **Why:** defense in depth for the in-code SSRF guard. **Tradeoff:**
the `except` list covers kind's default Pod CIDR (10.244/16) and Service
CIDR (10.96/12) because both fall under `10.0.0.0/8`; if your cluster
uses non-default CIDRs outside RFC1918, broaden `except`. **Requires:** a
NetworkPolicy-enforcing CNI (Calico, Cilium). kindnet does not enforce.

### Migrate Job uses `helm.sh/hook-delete-policy: before-hook-creation` (M8)
The previous Job stays around until the next install starts; on success the
new Job becomes the only one, with `ttlSecondsAfterFinished: 300` to clean
up. **Why:** a failed migration is the worst time to lose `kubectl logs`
of the failing pod. **Rejected:** `hook-succeeded` (deletes immediately,
no diagnostic window) or no policy (Job objects pile up forever).

### Observer stays at 1 replica even in k8s (M8)
Observer state (key stats + log ring + active pods gauge) lives in memory.
A second replica would split the aggregation; the live UI would show two
disjoint snapshots depending on which pod a viewer hit through the Service.
**Tradeoff:** observer is now a SPOF. Acceptable — its data is recoverable
on restart (Redis poll gives queue/DLQ depth + pod count; only the log ring
and per-key stats are truly transient). Horizontal scaling is a v2 concern.

### Worker `terminationGracePeriodSeconds: 60` (M8)
30s default isn't enough — `DRAIN_TIMEOUT=30s` covers asynq Shutdown but
in-flight HTTP webhook delivery (up to 10s per attempt) can run inside
that window. 60s gives the worker a clean tail.

### Single-replica PgBouncer in k8s (M8)
SPEC §12 mentions PgBouncer; we run one replica with a bounded upstream
pool (`DEFAULT_POOL_SIZE=20`). **Why:** PgBouncer is stateless but a
second replica doubles the upstream Postgres connections, defeating its
purpose. Scale the pool, not the replica count. **Tradeoff:** PgBouncer
is the path's SPOF; an upgrade or pod restart drops the entire connection
fleet briefly. Acceptable; production-grade HA would front it with a
TCP load balancer over 2+ replicas with `DEFAULT_POOL_SIZE` halved.

---

## 13. Operator panel (post-M9)

### Loadtest becomes the UI canonical entry point (post-M9)
Original M5 design: vegeta attack auto-starts when the loadtest binary
boots, runs for `--duration`, then the showcase page stays up for
inspection. Operator key management lived in `cmd/keygen` (CLI only),
keys.yaml was static. **Why changed:** users wanted "open the page,
generate a key, click Start" as the canonical demo flow without needing
a Go toolchain or a terminal walkthrough. **What it cost:** loadtest
binary now opens a DB connection (was read-only against keys.yaml),
the operator panel surfaces three new unauth endpoints, and the
binary lives in `127.0.0.1:8090` for dev mode and `0.0.0.0:8090` for
container mode — split bind addresses based on `CONTAINER_MODE` env.
**Rejected:** a separate microservice for the control plane (no
deployment / scaling story justifies the split; loadtest is already
the natural home).

### Two compose files instead of one (post-M9)
`docker-compose.yml` keeps infra-only; `docker-compose.full.yml` adds
the four app binaries as containers. `make dev` + `make run-*` is dev
mode; `make full` is showcase mode. **Why:** the SPEC §13 "iterate
fast via host binaries" model and the SPEC §4.4 "operator panel as
the one-command demo" model are both valuable and serve different
users (developer vs evaluator). One unified compose either bakes in
container rebuilds on every code change (slow) or forces host-only
(can't demo without `go install`). **Rejected:** single `--profile`-
gated compose, because compose profile expansion is opaque to
`docker compose up` users and adds friction to the "I just want to
see it work" flow.

### keys.yaml is canonical, in-memory + DB are derived (post-M9)
The operator panel writes keys.yaml on every Generate / Revoke
under the same mutex that guards the in-memory registry. **Why:**
keys.yaml is gitignored but lives across pod restarts (via the PVC
in k8s or the bind mount in compose.full). DB hashes survive volume
wipes only inasmuch as the user keeps the Postgres volume; in-memory
state dies with the process. Anchoring on the file means a `helm
upgrade` (which recreates the pod) doesn't lose keys. **Rejected:**
in-memory-only with a "Download keys.yaml" button — relies on the
operator remembering to download before reopening, error-prone.

### `--observer-public` / `--grafana-public` split (post-M9)
Two flag pairs because the loadtest binary needs different URLs for
server-side and browser-side use of the same logical service. In
dev mode they're identical (`localhost:N`). In compose.full / k8s
the server-side reaches `observer:9090` via service DNS but the
browser must use the published host port `localhost:9090`. **Why
explicit flags:** alternative would be to have the page server
introspect compose names and rewrite them for the browser — opaque
and fragile. Explicit flags keep the two endpoints in plain sight in
`docker-compose.full.yml` and `values.yaml`. **Rejected:** auto-
rewriting via a `host.docker.internal`-style alias inside the
container — would only work on Docker Desktop, not Linux Docker or
k8s.

### Loadtest single-replica in k8s with PVC for `/config` (post-M9)
Mirrors the observer-stays-at-1-replica decision above for the same
reason: the operator panel maintains state. A second replica would
split keys.yaml writes across the RWO volume (neither would bind)
and fork the in-memory key registry. Tiny PVC (10Mi, plenty for ~10k
keys at ~1KB each) gives `helm upgrade` a place to round-trip
keys.yaml. **Why not emptyDir:** UI-generated keys would die on
every rollout, surprising the operator. The PVC adds 10Mi to the
chart's storage footprint — negligible.

### `CONTAINER_MODE` env var instead of auto-detect (post-M9)
The loadtest binary changes two behaviors when running in a
container: bind address (`0.0.0.0` vs `127.0.0.1`) and integration-
test card filtering. **Why explicit env var:** runtime detection
(check for `/.dockerenv`, etc.) is platform-specific and breaks
inside non-Docker runtimes (Podman, k8s, CI). Explicit flag is set
by the chart / compose.full file at the source of truth. **Tradeoff:**
host-mode users who happen to be inside a container (e.g. WSL +
Docker Desktop) need to set it themselves — none have asked yet.
