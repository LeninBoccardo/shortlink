# ShortLink — Project Specification

> A production-grade, observable URL shortening platform built in Go.  
> Designed for portfolio demonstration of distributed systems, async processing, Kubernetes orchestration, and real-time observability.

---

## Table of Contents

1. [Project Overview](#1-project-overview)
2. [System Architecture](#2-system-architecture)
3. [Repository Structure](#3-repository-structure)
4. [Components](#4-components)
   - 4.1 [API Gateway](#41-api-gateway)
   - 4.2 [Worker Pod](#42-worker-pod)
   - 4.3 [Observer Hub](#43-observer-hub)
   - 4.4 [Load Test Runner](#44-load-test-runner)
5. [Data Models](#5-data-models)
6. [Storage Design](#6-storage-design)
7. [Queue Design](#7-queue-design)
8. [Webhook Contract](#8-webhook-contract)
9. [Security](#9-security)
10. [Observability Events](#10-observability-events)
11. [Frontend Dashboard](#11-frontend-dashboard)
12. [Kubernetes Deployment](#12-kubernetes-deployment)
13. [Local Development](#13-local-development)
14. [Configuration](#14-configuration)
15. [Tech Stack](#15-tech-stack)
16. [Non-Functional Requirements](#16-non-functional-requirements)
17. [Implementation Milestones](#17-implementation-milestones)

---

## 1. Project Overview

**ShortLink** is a URL shortening service with QR code generation, built around an async task queue and a real-time observability layer. It is intentionally over-engineered relative to its feature surface — the goal is to demonstrate a realistic production architecture, not to solve URL shortening as efficiently as possible.

### Core user flow

1. Client sends a `POST /shorten` request with a long URL and a valid API key.
2. The API gateway validates the key, checks rate limits, SSRF-validates the webhook URL, **writes a `pending` record to Postgres** (this single row is both the idempotency anchor and — for custom slugs — the slug reservation), and enqueues a shorten job.
3. The API gateway returns `202 Accepted` immediately.
4. A worker pod picks up the shorten job, atomically claims the `pending` record, generates a short slug and a QR code PNG, uploads the QR to object storage, finalizes the Postgres record, and **enqueues a separate webhook-delivery job**.
5. A webhook-delivery job (separate queue) generates a fresh signed download URL for the QR code, SSRF-validates the client's webhook URL, and POSTs the result with an HMAC signature.
6. The QR image's signed URL is short-lived (default 60 s, regenerated on every webhook retry); the QR object itself is deleted from storage by a sweeper after a longer TTL (~15 minutes) that comfortably outlives the webhook retry window.

The pipeline is **fully asynchronous from the first milestone** — shortening and QR generation never happen inside the request handler. Milestone 1 uses an in-process channel queue; Milestone 2 swaps in Redis without changing the API contract.

### What this project demonstrates

- Async job processing with retries, idempotency, crash recovery, and a dead-letter queue
- A decoupled webhook-delivery tier — slow client endpoints never block job throughput
- Kubernetes pod orchestration with tight resource constraints and queue-driven autoscaling (KEDA)
- API key authentication with per-key rate-limiting tiers
- Webhook payload integrity via per-key HMAC signing secrets
- SSRF-hardened outbound HTTP (the server fetches user-supplied webhook URLs)
- Signed URL generation and object storage lifecycle management
- Real-time observability via a WebSocket-broadcasting aggregator
- Standard metrics observability via Prometheus + Grafana
- Connection multiplexing at pod scale via PgBouncer
- Load testing with per-key concurrency and a live showcase dashboard

---

## 2. System Architecture

```text
┌──────────────────────────────────────────────────────────────────────┐
│  Client                                                              │
│  POST /shorten  {url, webhook_url?, custom_slug?, expires_in?}        │
│                 +  X-Api-Key header                                  │
└──────────────────────────┬───────────────────────────────────────────┘
                           │ HTTP
                           ▼
┌──────────────────────────────────────────────────────────────────────┐
│  API Gateway  :8080   (stateless — 2+ replicas behind ClusterIP)     │
│  • API key validation (SHA-256 lookup via PgBouncer → Postgres)      │
│  • Per-key sliding-window rate limiting (Redis + Lua)               │
│  • SSRF validation of webhook_url                                   │
│  • Writes a `pending` short_urls row (idempotency + slug reserve)   │
│  • Enqueues a shorten job                                           │
│  • Returns 202 Accepted immediately                                 │
└──────────────────────────┬───────────────────────────────────────────┘
                           │ enqueue
                           ▼
┌──────────────────────────────────────────────────────────────────────┐
│  Queue  (in-process channel in M1 → Redis/asynq from M2)             │
│  • shorten queue   • webhook queue   • dead-letter (archived) set   │
│  • per-job retry count, backoff   • enqueue dedup (asynq Unique)    │
└────────────┬─────────────────────────────────────────────────────────┘
             │ dequeue
             ▼
┌──────────────────────────────────────────────────────────────────────┐
│  Worker Deployment   0.5 CPU / 256 MB each   KEDA: 2–12 replicas     │
│                                                                      │
│  shorten handler:  claim row (lease) → slug (retry on collision) →  │
│                    QR PNG → upload → finalize row → enqueue webhook │
│  webhook handler:  re-presign QR URL → SSRF re-validate → POST +    │
│                    HMAC → retry w/ backoff → archive on exhaustion  │
│  sweeper:          delete stale rows + orphaned QR objects          │
└────┬───────────────────────────┬─────────────────────────────────────┘
     │                           │
     ▼                           ▼
┌─────────────────┐      ┌───────────────────┐
│  Postgres       │      │  Object Storage   │
│  (via PgBouncer)│      │  QR PNG           │
│  short_urls     │      │  object TTL ~15m  │
│  api_keys       │      │  signed URL 60s   │
│  hits           │      └─────────┬─────────┘
└─────────────────┘                │ webhook HTTP POST
                                   ▼  (signed URL + HMAC signature)
                          ┌─────────────────┐
                          │  Client webhook │
                          └─────────────────┘

  All services ─POST /ingest─►  Observer Hub :9000  (backend only)
                                      │ WebSocket: snapshot / stats / log_append
                                      ▼
                          Showcase Page  (served by loadtest :8090)
                          ├─ per-key test table   (live via WS)
                          ├─ live log audit       (live via WS)
                          └─ embedded Grafana panels (iframe → :3000)

  All services ─expose /metrics─►  Prometheus :9090 ──► Grafana :3000
```

The API gateway and worker tier scale horizontally. The observer hub is deliberately single-instance — see [§16](#16-non-functional-requirements).

---

## 3. Repository Structure

```text
shortlink/
├── cmd/
│   ├── api/                  # API gateway binary
│   │   └── main.go
│   ├── worker/               # Worker binary (shorten + webhook + sweeper)
│   │   └── main.go
│   ├── observer/             # Observer hub binary (aggregator + WS, backend only)
│   │   └── main.go
│   ├── loadtest/             # Load test runner + showcase page + webhook sink
│   │   ├── main.go
│   │   └── web/              # Single-page showcase UI, embedded via go:embed
│   │       ├── index.html
│   │       ├── dashboard.js  # WebSocket client, DOM updates, TTL countdown
│   │       └── style.css
│   ├── keygen/               # API key + webhook-secret provisioning (make keys)
│   │   └── main.go
│   └── migrate/              # Goose migration runner (used by Job / one-shot service)
│       └── main.go
│
├── internal/
│   ├── auth/
│   │   ├── keygen.go         # API key + webhook signing secret generation
│   │   ├── validator.go      # Key lookup, tier resolution, throttled last_used_at
│   │   └── ratelimit.go      # Sliding window rate limiter (Redis Lua)
│   ├── shortener/
│   │   ├── slug.go           # base62 slug generation (crypto/rand)
│   │   └── shortener.go      # Slug assignment + collision retry loop
│   ├── qrcode/
│   │   ├── generator.go      # QR PNG generation (skip2/go-qrcode)
│   │   └── uploader.go       # Upload to object storage, presign signed URL
│   ├── queue/
│   │   ├── queue.go          # Queue interface (Enqueue / handler registration)
│   │   ├── inproc.go         # In-process channel implementation (M1)
│   │   ├── asynq.go          # Redis/asynq implementation (M2+)
│   │   └── jobs.go           # Job type definitions and payload structs
│   ├── webhook/
│   │   ├── dispatcher.go     # HTTP POST to client webhook URL
│   │   ├── signer.go         # HMAC-SHA256 signing with per-key webhook secret
│   │   └── retry.go          # Backoff schedule, max attempts
│   ├── sweeper/
│   │   └── sweeper.go        # Stale-record + orphaned-object cleanup loop
│   ├── storage/
│   │   ├── postgres.go       # pgx pool setup (sized; PgBouncer-aware exec mode)
│   │   ├── queries.go        # sqlc-generated query wrappers
│   │   └── minio.go          # MinIO client, upload, presign, delete
│   ├── security/
│   │   └── ssrf.go           # Webhook URL validation + safe-dial HTTP client
│   ├── metrics/
│   │   └── metrics.go        # Prometheus collectors, shared across binaries
│   ├── events/
│   │   ├── event.go          # Event envelope type (shared by emitter and observer)
│   │   └── emitter.go        # Best-effort fire-and-forget event emission to observer
│   ├── middleware/
│   │   ├── auth.go           # API key extraction and injection into ctx
│   │   ├── ratelimit.go      # Rate limit middleware, emits 429
│   │   └── logger.go         # Structured request logging → observer
│   ├── observer/
│   │   ├── ingest.go         # POST /ingest handler
│   │   ├── store.go          # In-memory event store with TTL (State/KeyStat/LogEntry)
│   │   ├── aggregator.go     # Aggregation loop, prunes expired entries
│   │   ├── poller.go         # Polls Redis for queue depth + pod heartbeats
│   │   └── broadcaster.go    # WebSocket hub: snapshot / stats / log_append
│   └── config/
│       └── config.go         # Env-based config with defaults (see §14)
│
├── migrations/
│   ├── 001_create_api_keys.sql
│   ├── 002_create_short_urls.sql
│   └── 003_create_hits.sql
│
├── deploy/
│   ├── docker-compose.yml    # Local stack (built up across milestones — see §13)
│   ├── k8s/
│   │   ├── api-deployment.yaml
│   │   ├── api-service.yaml
│   │   ├── worker-deployment.yaml
│   │   ├── worker-scaledobject.yaml   # KEDA queue-depth autoscaler
│   │   ├── pgbouncer.yaml
│   │   ├── migrate-job.yaml           # Helm pre-upgrade hook
│   │   ├── configmap.yaml
│   │   ├── secrets.yaml
│   │   └── networkpolicy.yaml         # Worker egress restriction (SSRF defense)
│   ├── prometheus/           # prometheus.yml
│   └── grafana/              # provisioned dashboards + datasource JSON
│
├── config/
│   └── keys.yaml             # Load test key profiles (gitignored — see §13)
│
├── sqlc.yaml                 # sqlc codegen config
├── Makefile                  # make dev, make migrate, make keys, make loadtest, make build
├── Dockerfile.api
├── Dockerfile.worker
├── Dockerfile.observer
├── Dockerfile.loadtest
├── Dockerfile.migrate
├── go.mod
├── go.sum
└── README.md
```

> **Module path:** `github.com/leninboccardo/shortlink` — **Go toolchain:** 1.26 (`go 1.26` in `go.mod`).
>
> The showcase UI lives under `cmd/loadtest/web/` (not at the repo root) because `//go:embed` can only embed files within the embedding package's own directory tree — `..` is not permitted in embed patterns.

---

## 4. Components

### 4.1 API Gateway

**Binary:** `cmd/api`  
**Port:** `8080`  
**Replicas:** 2+ in Kubernetes; 1 locally (stateless — see [§16](#16-non-functional-requirements))  
**Responsibility:** Accept client requests, authenticate, rate-limit, reserve the record, enqueue, and respond immediately.

#### Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/shorten` | Submit a URL for shortening |
| `GET` | `/:slug` | Redirect to original URL |
| `GET` | `/healthz` | Health check |
| `GET` | `/metrics` | Prometheus metrics |

#### POST /shorten

**Request:**
```json
{
  "url": "https://very-long-url.example.com/path?query=value",
  "webhook_url": "https://client.example.com/webhook",   // optional — see below
  "custom_slug": "my-slug",                              // optional
  "expires_in": 86400                                    // optional — seconds until the short URL expires
}
```

**Headers:**
```text
X-Api-Key: sl_live_abc123...
Content-Type: application/json
```

**`webhook_url` resolution:** the effective webhook URL is the request's `webhook_url` if present, otherwise the API key's default `api_keys.webhook_url`. If neither is set, the request fails with `400`. The resolved value is what gets stored in `short_urls.webhook_url` (which is `NOT NULL`).

**`expires_in`:** if provided, the gateway sets `short_urls.expires_at = now() + expires_in`. If omitted, `expires_at` stays `NULL` (the short URL never expires).

**Response — 202 Accepted:**
```json
{
  "job_id": "job_01J8XYZ...",
  "message": "Job accepted. Result will be delivered to your webhook."
}
```

**Error responses:**

| Status | Condition |
|--------|-----------|
| `400` | Malformed request, invalid URL, or no webhook URL (none in request and no key default) |
| `401` | Missing or invalid API key |
| `409` | Custom slug already taken |
| `422` | URL fails validation (blocked domain, SSRF-unsafe webhook URL, etc.) |
| `429` | Rate limit exceeded for this key |
| `500` | Internal error |

#### Record reservation (idempotency + slug reservation)

Before enqueuing, the gateway generates a ULID `job_id` and writes a `short_urls` row in status `pending`. This single row is the anchor for both idempotency ([§4.2](#42-worker-pod)) and custom-slug uniqueness:

- **Custom slug provided:**
  ```sql
  INSERT INTO short_urls (job_id, slug, original_url, api_key_id, webhook_url, expires_at, status)
  VALUES ($1, $2, $3, $4, $5, $6, 'pending')
  ON CONFLICT (slug) DO NOTHING
  RETURNING id;
  ```
  Zero rows returned → the slug is taken → respond `409` synchronously. The `UNIQUE` constraint on `slug` makes this race-free; there is no time-of-check/time-of-use gap.

- **No custom slug:** insert with `slug = NULL`. The worker assigns and writes the generated slug later. (`UNIQUE` on a nullable column permits many `NULL` rows in Postgres.)

Only after the row is committed does the gateway enqueue the shorten job and return `202`.

#### GET /:slug

Performs a `302 Found` redirect to the original URL. Records an analytics hit (async, fire-and-forget). Returns `404` if the slug is not found, not yet finalized (`status != 'done'`), or expired (`expires_at` in the past).

#### Middleware chain (in order)

```text
request
  → RequestID        (attach UUID to context)
  → Logger           (structured log; emits request_completed to observer)
  → Auth             (extract + validate API key, attach tier to context)
  → RateLimit        (sliding window per key in Redis, emits event on 429)
  → Handler
```

#### Rate limiting implementation

Uses a Redis sorted set per API key (sliding window algorithm):

```text
Key pattern:   rl:{api_key_hash}
Window:        60 seconds
Tier limits:
  free:        10 req/min   (RATE_LIMIT_FREE)
  pro:         60 req/min   (RATE_LIMIT_PRO)
  unlimited:   no limit (internal/admin keys)
```

On limit exceeded: emits `rate_limit_hit` event to observer, returns 429 with headers:
```text
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1735000060
Retry-After: 43
```

The `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and `X-RateLimit-Reset` headers are returned on **every** `/shorten` response, success included; `Retry-After` is sent only on a 429.

> **v1 scope note:** rate limiting is keyed on the API key, so only *authenticated* traffic is throttled. A request with a missing or invalid key still reaches the gateway and triggers one Postgres lookup. Per-IP rate limiting for unauthenticated/invalid-key traffic is a documented **v2** item — v1 assumes authenticated clients.

#### Graceful shutdown

On `SIGTERM` the gateway stops accepting new connections, lets in-flight HTTP requests finish (timeout 10 s), and exits.

In **Milestone 1** the in-process queue and its worker goroutines run *inside the API binary*, so shutdown additionally drains the in-process channel — queued jobs are processed to completion before exit. The in-process queue has no durability: a hard crash (not a clean `SIGTERM`) loses whatever is still in the channel. This is an accepted M1 limitation; durability arrives in M2 when the queue moves to Redis.

---

### 4.2 Worker Pod

**Binary:** `cmd/worker`  
**Port:** `8081` (health + metrics)  
**Concurrency:** shorten handler 2–3 goroutines per pod; webhook handler runs on its own queue with its own concurrency budget  
**Resources:** 0.5 CPU, 256 MB RAM

#### Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Health check |
| `GET` | `/metrics` | Prometheus metrics |

The worker registers **two queue handlers** plus a **sweeper loop**. The shorten job never performs webhook delivery itself — it enqueues a separate webhook job and completes. A slow or failing client webhook therefore can never hold a shorten worker slot or block queue throughput.

#### Shorten job pipeline

```text
dequeue shorten job
  → claim record:  UPDATE short_urls SET status='processing', updated_at=now()
                   WHERE job_id=$1 AND status IN ('pending','processing')
                   RETURNING updated_at AS lease, ...
       └─ zero rows → read the row's status and branch:
            done             → re-enqueue webhook job, ack
            failed / no row  → ack, skip
  → generate slug (if not custom)        — base62, SLUG_LENGTH chars, crypto/rand
  → generate QR code PNG                 — skip2/go-qrcode, QR_SIZE px
  → upload PNG to object storage
  → finalize:  UPDATE short_urls SET slug=$1, qr_object=$2, status='done',
               updated_at=now() WHERE job_id=$3 AND updated_at=$lease
       └─ zero rows → lease lost to a re-claim → discard work, ack
       └─ slug UNIQUE violation → regenerate slug, retry (see §5, Slug policy)
  → enqueue webhook-delivery job
  → emit job_complete event to observer
```

#### Webhook delivery job pipeline

```text
dequeue webhook job
  → load short_urls record by job_id
  → re-presign QR signed URL            — fresh TTL on every attempt
  → SSRF-validate webhook_url           — re-resolved (DNS may have changed)
  → HMAC-sign the request body          — per-key webhook secret
  → POST to client webhook_url
       └─ failure → retry with backoff (§8); after max attempts → archive,
                    emit webhook_failed
  → emit webhook_sent event to observer
```

#### Idempotency & crash recovery

The `short_urls` row is created **once**, by the gateway, before the job is even enqueued. Workers only ever *transition* it. The claim is a single atomic `UPDATE`; Postgres row-locking serializes any two workers that hold the same `job_id`. The claim unconditionally re-acquires any row in `pending` or `processing` — there is no lease-cutoff in the `WHERE` clause. Two distinct cases:

- **Concurrent duplicate delivery** — two workers receive the same `job_id` at once. Both run the claim `UPDATE`; Postgres serializes them, so one writes first and bumps `updated_at`, while the second matches the row (still `processing`) and bumps `updated_at` again. Each gets its own fresh `updated_at` back as its **lease token**.
- **Crash recovery** — a worker claims a row (`pending`→`processing`) and then dies before finalizing. asynq's recoverer redelivers the task. Because the claim matches `processing` unconditionally, the redelivered job re-claims and completes the abandoned work — there's no `CLAIM_LEASE` window to wait out. This deliberately diverges from a lease-cutoff claim: asynq retries can fire within seconds of a failure, and a cutoff `WHERE updated_at < now()-CLAIM_LEASE` would simply lose those retries.

Safety still rests on the lease token: the finalizing `UPDATE` carries `AND updated_at=$lease`, so if a stalled worker is preempted by a re-claim, its late finalize matches zero rows and is harmlessly discarded. Combined with `asynq.Unique` on enqueue and an asynq task timeout aligned to `CLAIM_LEASE` ([§7](#7-queue-design), [§14](#14-configuration)), duplicate **execution** is possible — two workers can run the same job in parallel — but duplicate **writes** are structurally impossible, so the row only ever transitions once.

#### Permanent failure

If a shorten job exhausts all retries, the worker detects the final attempt (`asynq.GetRetryCount(ctx) == asynq.GetMaxRetry(ctx)`) and marks the record failed with the **same lease guard** as `finalize`:

```sql
UPDATE short_urls SET status='failed', updated_at=now()
WHERE job_id=$1 AND updated_at=$lease;
```

Zero rows means this worker was preempted by a re-claim — the failure write is skipped, so a stalled worker can never stamp `failed` over a row another worker is actively re-processing. The job is then archived ([§7](#7-queue-design)), and the sweeper ([§6](#6-storage-design)) later deletes the row — freeing any reserved custom slug. A permanently failed *webhook* job does **not** change `status`: the short URL was created successfully, only delivery failed.

#### QR code spec

- Library: `github.com/skip2/go-qrcode`
- Content: the full short URL (e.g. `https://sl.example.com/abc1234`), built from `SHORT_URL_BASE` + slug
- Size: `QR_SIZE` × `QR_SIZE` px (default 256×256)
- Error correction: Medium (recovers from ~15% damage)
- Format: PNG
- Typical generation time: 1–4 ms
- Typical memory allocation: 1–3 MB (freed after upload)

#### Sweeper loop

The worker runs a sweeper every ~60 s — see [§6, Stale record & object sweeper](#stale-record--object-sweeper).

#### Pod heartbeat

While running, the worker refreshes a Redis key `pod:{POD_ID}:alive` (15 s TTL) so the observer can count live pods ([§6](#6-storage-design)). It emits `pod_started` on boot and `pod_stopped` during drain.

#### Graceful shutdown

On `SIGTERM` (Kubernetes drain):
1. Stop accepting new jobs from both queues.
2. Allow in-flight jobs to complete. Shorten jobs are millisecond-scale (no webhook in the path); webhook jobs are a single bounded HTTP POST.
3. Drain timeout: `DRAIN_TIMEOUT` (default 30 s).
4. Emit `pod_stopped`, delete the heartbeat key, exit cleanly.

`terminationGracePeriodSeconds` is set to 40 s, comfortably above the 30 s drain plus the `preStop` delay.

---

### 4.3 Observer Hub

**Binary:** `cmd/observer`  
**Port:** `9090` (locally — kept off `9000` because docker-compose publishes MinIO's S3 API there; production Kubernetes can use whatever the Service exposes)  
**Responsibility:** Receive structured events from all services, aggregate them in memory with TTL, and broadcast state to connected browser clients via WebSocket. **Backend only — it serves no static files.** The showcase frontend is served by the load test runner ([§4.4](#44-load-test-runner), [§11](#11-frontend-dashboard)).

#### Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/ingest` | Receive an event from any service |
| `GET` | `/stream` | WebSocket upgrade — browser connects here |
| `GET` | `/healthz` | Health check |
| `GET` | `/metrics` | Prometheus metrics |

> The `/stream` WebSocket upgrader must set a custom `CheckOrigin` allowing the showcase page's origin: the page is served from `:8090` while the observer listens on `:9090`, so the connection is cross-origin and `gorilla/websocket`'s default `CheckOrigin` would reject it.

#### Internal concurrency model

```text
ingestHandler()   →  eventChannel (buffered, 1000)
                         │
                   aggregator()    ← ticker: every 100ms
                         │         prunes TTL, updates keyStats map, updates logRing
                         │
                   poller()        ← ticker: every 5s
                         │         reads Redis: asynq queue depth + live pod heart-
                         │         beats; emits queue_depth_high / dlq_nonempty
                         │
                   broadcaster()  ← ticker: every 500ms
                         │         emits a `stats` message + a `log_append` message
                         └──────── fan-out to all connected WS clients
```

The observer opens a **read-only Redis connection** for the poller (queue depth + pod-heartbeat keys). The poller emits `queue_depth_high` when the pending-job count exceeds `QUEUE_DEPTH_THRESHOLD`, and `dlq_nonempty` when the archived set is non-empty. If the `eventChannel` buffer is full, the ingest handler **drops the event** (favouring liveness over completeness) and increments an `observer_events_dropped_total` metric.

#### In-memory state

```go
type State struct {
    KeyStats  map[string]*KeyStat  // keyed by api_key_hash
    Logs      []LogEntry           // ring buffer, newest first, max 500
    System    SystemStat
    UpdatedAt time.Time
}

type KeyStat struct {
    KeyHash     string    // full SHA-256 hash — the map key
    KeyHint     string    // last 6 chars, for display
    Tier        string
    RateLimit   int       // req/min
    TotalReqs   int64
    Webhooks    int64
    LimitErrors int64     // 429s
    JobErrors   int64
    P99Latency  int64     // ms, rolling 60s window
    LastSeen    time.Time
}

type LogEntry struct {
    ID         string
    Timestamp  time.Time
    ExpiresAt  time.Time   // TTL per event kind
    Source     string      // api | worker | loadtest | observer
    Level      string      // info | warn | error
    Kind       string
    APIKeyHash string
    APIKeyHint string
    Message    string
    Meta       map[string]any
}

type SystemStat struct {
    ActivePods    int
    QueueDepth    int64
    TotalJobs     int64
    ErrorRate     float64
    UptimeSeconds int64
}
```

> All observer state is **in-memory only** — there is no persistence. On restart the observer starts empty and rebuilds from incoming events. This is acceptable for an operability/demo tool ([§16](#16-non-functional-requirements)).

#### How aggregated fields are derived

`KeyStat` (per `api_key_hash`):

| Field | Source |
|-------|--------|
| `KeyHash` / `KeyHint` | First event seen carrying the key |
| `Tier` / `RateLimit` | `meta.tier` / `meta.rate_limit` on API-emitted events (`request_completed`, `rate_limit_hit`) — the gateway resolves them at auth time |
| `TotalReqs` | Running count of `request_completed` events |
| `Webhooks` | Running count of `webhook_sent` events |
| `LimitErrors` | Running count of `rate_limit_hit` events |
| `JobErrors` | Running count of `job_error` + `job_dlq` events |
| `P99Latency` | Rolling 60 s p99 of `request_completed.meta.duration_ms` |
| `LastSeen` | Timestamp of the most recent event for the key |

`SystemStat`:

| Field | Source |
|-------|--------|
| `ActivePods` | Count of live `pod:*:alive` keys in Redis (poller) |
| `QueueDepth` | asynq pending-list length in Redis (poller) |
| `TotalJobs` | Running count of `job_complete` events |
| `ErrorRate` | `(job_error + job_dlq) / job_complete` over a rolling window (0 when no jobs) |
| `UptimeSeconds` | Observer process uptime |

#### WebSocket message protocol

The broadcaster does **not** resend the full state every tick. The previous design — a complete snapshot (up to 500 log lines + all key stats) marshalled and sent to every client twice per second — wastes bandwidth and CPU re-sending unchanged data, and overflows under load-test log churn. Instead, four message types:

**1. `snapshot`** — sent **once**, immediately on each new connection. Full key stats + the full log buffer + system stats. Gives a freshly connected browser its starting state.

```json
{ "type": "snapshot", "ts": "...", "key_stats": [...], "logs": [...], "system": {...} }
```

**2. `stats`** — sent every 500 ms. Key stats + system stats only. These are small and aggregate (each value replaces the previous one), so sending them in full every tick is cheap.

```json
{
  "type": "stats",
  "ts": "2025-05-18T12:00:00Z",
  "key_stats": [
    {
      "key_hash": "9f2c1a...e7",
      "key_hint": "abc123",
      "tier": "pro",
      "rate_limit": 60,
      "total_reqs": 4291,
      "webhooks": 4180,
      "limit_errors": 11,
      "job_errors": 2,
      "p99_latency_ms": 38
    }
  ],
  "system": {
    "active_pods": 4,
    "queue_depth": 23,
    "total_jobs": 8821,
    "error_rate": 0.003,
    "uptime_s": 3621
  }
}
```

**3. `log_append`** — sent every 500 ms. **Only the log entries created since the last tick** (typically 0–5). The browser maintains its own 500-entry ring buffer: it appends new entries, drops the oldest past 500, prunes expired entries by `expires_at`, and counts down TTL badges locally. Each log line is therefore transmitted **exactly once per client**, not ~500× per second.

```json
{
  "type": "log_append",
  "ts": "2025-05-18T12:00:00Z",
  "logs": [
    {
      "id": "evt_01J8...",
      "ts": "2025-05-18T12:00:00Z",
      "expires_in_s": 47,
      "source": "worker",
      "level": "info",
      "kind": "job_complete",
      "api_key_hash": "9f2c1a...e7",
      "api_key_hint": "abc123",
      "message": "Job completed: slug=xK9p2aT, qr_upload=12ms"
    }
  ]
}
```

**4. `reset`** — sent in response to a `clear_logs` or `reset_stats` command; tells the browser to wipe its local log list and/or stats.

The browser sends commands back over the same WebSocket:

```json
{ "type": "cmd", "action": "clear_logs" }
{ "type": "cmd", "action": "reset_stats" }
```

#### Event TTLs by kind

The TTL governs how long a **logged** event stays in the log ring buffer. Counters in `KeyStat` / `SystemStat` are cumulative and independent of TTL. (`request_completed` is not logged — it is retained ~60 s only for the rolling p99 window.)

| Kind | TTL |
|------|-----|
| `auth_failure` | 15 minutes |
| `rate_limit_hit` | 5 minutes |
| `job_enqueued` | 2 minutes |
| `job_complete` | 2 minutes |
| `job_error` | 10 minutes |
| `job_dlq` | 15 minutes |
| `webhook_sent` | 2 minutes |
| `webhook_failed` | 10 minutes |
| `pod_started` | until the pod reports stopped |
| `pod_stopped` | 5 minutes |
| `queue_depth_high` | 1 minute |
| `dlq_nonempty` | 15 minutes |
| `attack_started` | 10 minutes |
| `attack_complete` | 30 minutes |

> Any logged event kind not listed defaults to a **2-minute** TTL.

---

### 4.4 Load Test Runner

**Binary:** `cmd/loadtest`  
**Ports:** `8090` (showcase page) · `8091` (built-in webhook sink)  
**Library:** `github.com/tsenart/vegeta` (embedded as a library, not CLI)

The load test runner has three jobs: run multi-key attacks against the API, **serve the single-page showcase UI** ([§11](#11-frontend-dashboard)) — embedded from `cmd/loadtest/web/` via `go:embed` — and host a **webhook sink** that receives the resulting callbacks. The showcase page is naturally tied to a test's lifecycle: you run `make loadtest`, the attack starts, and the page is available at `:8090` for the duration.

#### CLI flags

```text
--keys      path to keys.yaml config file       (default: config/keys.yaml)
--target    base URL of API gateway             (default: http://localhost:8080)
--duration  attack duration                     (default: 60s)
--observer  observer hub URL for live stats     (default: http://localhost:9000)
--grafana   Grafana base URL for embedded panels(default: http://localhost:3000)
--port      showcase page port                  (default: 8090)
--sink-url  webhook sink URL advertised to the API (default: http://localhost:8091/sink)
```

`--sink-url` is the address the **API/worker** must use to reach the sink. The load test runner always runs on the host ([§13](#13-local-development)); the value differs by where the API runs: `http://localhost:8091/sink` when the API is also on the host, `http://host.docker.internal:8091/sink` when the API runs in docker-compose. Whatever host it names must be present in `SSRF_ALLOWLIST` ([§9](#9-security)).

#### keys.yaml format

```yaml
keys:
  - name: "Free tier"
    key: "sl_live_abc..."
    webhook_secret: "whsec_abc..."
    attack_rate_per_min: 10
    tier: free

  - name: "Pro tier"
    key: "sl_live_def..."
    webhook_secret: "whsec_def..."
    attack_rate_per_min: 60
    tier: pro

  - name: "Abuser (over-limit)"
    key: "sl_live_ghi..."
    webhook_secret: "whsec_ghi..."
    attack_rate_per_min: 200   # intentionally exceeds the server-side limit
    tier: pro
```

> `attack_rate_per_min` controls how fast *this client* fires requests. It is **not** a server setting — the server enforces the limit bound to the key's tier. The "Abuser" profile attacks at 200/min against a 60/min tier to exercise rejection.

#### Attack model

Each key profile runs its own `vegeta.Attacker` in a separate goroutine. Attackers are started simultaneously. Each attacker:

1. Generates randomised URLs from a seed list.
2. POSTs to `POST /shorten` with its key in the header and `webhook_url` set to `--sink-url`.
3. Collects `vegeta.Metrics` independently.

```go
type AttackResult struct {
    Profile  KeyProfile
    Metrics  vegeta.Metrics
    Started  time.Time
    Finished time.Time
}
```

The runner emits `attack_started` when the attack begins and `attack_complete` when it ends (the latter carrying a vegeta summary in `meta`). The per-key showcase table is **derived server-side** by the observer from API and worker events ([§4.3](#43-observer-hub)) — the runner does not push the table's data itself.

#### Webhook sink

A load test has no real client endpoint to receive callbacks, so the runner hosts a **built-in webhook sink** at `:8091/sink`. Every attack request sets its `webhook_url` to the sink, closing the pipeline loop end-to-end: gateway → queue → worker → QR → webhook delivery → sink.

The sink:
- Responds `200 OK` and counts deliveries per key.
- Reads the `X-ShortLink-Key-Hint` header to select the matching `webhook_secret` from `keys.yaml`, then **verifies the `X-ShortLink-Signature` HMAC** — a genuine end-to-end check that signing works.
- Lets the showcase "WH" column reflect real delivered callbacks.

> Because the sink lives on a loopback or private address, its host **must** be added to `SSRF_ALLOWLIST` — otherwise the gateway's SSRF check rejects every attack request with `422`. The local stack wires this automatically ([§13](#13-local-development)).

---

## 5. Data Models

### API Key

```sql
CREATE TABLE api_keys (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash       TEXT NOT NULL UNIQUE,   -- SHA-256 of the raw key
    key_hint       TEXT NOT NULL,          -- last 6 chars, for display only
    name           TEXT NOT NULL,
    tier           TEXT NOT NULL DEFAULT 'free',  -- free | pro | unlimited
    webhook_secret TEXT NOT NULL,          -- per-key HMAC signing secret (see §8/§9)
    webhook_url    TEXT,                   -- default webhook per key (fallback — see §4.1)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at     TIMESTAMPTZ,
    last_used_at   TIMESTAMPTZ
);
```

The raw API key (`sl_live_<random>`) is shown once on creation and never stored — only its SHA-256 hash is persisted. The `webhook_secret` (`whsec_<random>`) is also shown once. Unlike the API key it **must** be stored in recoverable form, because the server is the *signer* and has to reproduce the HMAC — see [§8](#8-webhook-contract) / [§9](#9-security).

### Short URL

```sql
CREATE TABLE short_urls (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id       TEXT NOT NULL UNIQUE,   -- idempotency anchor, generated by the gateway
    slug         TEXT UNIQUE,            -- NULL until a generated slug is assigned;
                                         -- set immediately for custom slugs
    original_url TEXT NOT NULL,
    api_key_id   UUID NOT NULL REFERENCES api_keys(id),
    webhook_url  TEXT NOT NULL,          -- resolved at the gateway (request or key default)
    qr_object    TEXT,                   -- object storage key; NULLed by the sweeper after cleanup
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending | processing | done | failed
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),  -- bumped on every status change
    expires_at   TIMESTAMPTZ,            -- NULL means never expires
    hit_count    BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX idx_short_urls_slug    ON short_urls(slug);
CREATE INDEX idx_short_urls_api_key ON short_urls(api_key_id);
CREATE INDEX idx_short_urls_status  ON short_urls(status);  -- sweeper + claim scan by status
```

The row is created by the gateway in status `pending`, claimed by a worker as `processing`, and finalized as `done` (or `failed`). `updated_at` is bumped on every status change; the worker's lease-based claim ([§4.2](#42-worker-pod)) and the sweeper ([§6](#6-storage-design)) both rely on it. The redirect handler only serves rows in status `done` whose `expires_at` is null or in the future.

### Slug generation & collision policy

- **Length:** `SLUG_LENGTH` characters (default **7**), base62 (`a–z A–Z 0–9`). 62⁷ ≈ 3.5 trillion — at 100 M stored URLs the per-insert collision probability is ≈ 0.003%.
- **Randomness:** `crypto/rand`, never `math/rand`.
- **Generated-slug collision:** the finalize `UPDATE` may hit the `slug` `UNIQUE` constraint. On violation the worker generates a **fresh** random slug and retries, up to `SLUG_MAX_RETRIES` (default 5). Five consecutive collisions ⇒ keyspace saturation ⇒ emit `job_error` and bump `SLUG_LENGTH` by 1 for subsequent jobs.
- **Custom-slug collision:** never retried — the gateway already returned `409` at reservation time ([§4.1](#41-api-gateway)).

### Analytics (append-only)

```sql
CREATE TABLE hits (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    country     TEXT,
    device      TEXT
);

CREATE INDEX idx_hits_slug ON hits(slug);
```

`hits` is the per-event analytics log; `short_urls.hit_count` is a denormalized running counter on the parent row. A redirect does both writes asynchronously (fire-and-forget) so it never blocks the `302`. `country` and `device` are best-effort and may be `NULL` — `device` is parsed from the User-Agent; `country` requires an optional GeoIP database and is left `NULL` in v1 if none is configured.

---

## 6. Storage Design

### Postgres

- Connection pool: `pgx/v5/pgxpool`, **sized to expected concurrency** via `PG_POOL_SIZE` — workers ~4, API gateway ~8. Pools are kept small deliberately so total connection count stays bounded as the worker tier scales.
- At pod scale, all services connect through **PgBouncer** in transaction-pooling mode. PgBouncer multiplexes a small upstream pool (~20 connections) to Postgres, so the number of real Postgres connections is independent of pod count. (Transaction-mode pooling requires care with prepared statements — use pgx's `QueryExecModeExec`, or PgBouncer ≥ 1.21 prepared-statement support.)
- Query layer: `sqlc` for type-safe generated query code.
- Migrations: `pressly/goose`, run by `cmd/migrate` — as a one-shot service locally and a Kubernetes Job (Helm pre-upgrade hook) in production ([§12](#12-kubernetes-deployment)).
- Index strategy: slug lookups are the hot path; keep `idx_short_urls_slug` warm.

### Redis

Used for several independent purposes:

| Purpose | Key pattern | TTL |
|---------|-------------|-----|
| Task queues (via asynq) | `asynq:{queue}:*` | Managed by asynq |
| Enqueue dedup (asynq Unique) | `asynq:unique:*` | Job retry lifetime |
| Rate limit sliding window | `rl:{key_hash}` | 60 seconds (auto-expire) |
| `last_used_at` write throttle | `lu:{key_hash}` | `LAST_USED_THROTTLE` (auto-expire) |
| Pod heartbeat | `pod:{pod_id}:alive` | 15 seconds (worker refreshes; observer polls) |

### Object Storage (MinIO in local, S3-compatible in prod)

- Bucket: `MINIO_BUCKET` (default `shortlink-qr`)
- Object key: `{year}/{month}/{day}/{job_id}.png`
- Upload: immediate after QR generation.
- **Signed URL TTL:** `SIGNED_URL_TTL` (default 60 s). A fresh signed URL is generated on *every* webhook delivery attempt, so the client always receives a URL valid for the full TTL regardless of retry timing.
- **Object cleanup:** done by the sweeper, **not** an S3 lifecycle rule. S3/MinIO lifecycle expiration is day-granular and cannot express a ~15-minute deadline. A 1-day MinIO lifecycle rule is still configured as a **backstop** for any object the sweeper misses.

### Stale record & object sweeper

The gateway writes a `short_urls` row in status `pending` *before* the job runs ([§4.1](#41-api-gateway)). If a job is abandoned — enqueue fails, the pod dies before the claim, or the job is archived to the dead-letter set — that row would otherwise linger forever, and for a **custom slug** it would permanently reserve that slug. Separately, the QR object needs deleting once the webhook retry window has closed.

A sweeper loop (`internal/sweeper`, runs in the worker every ~60 s) handles both:

| Target | Action |
|--------|--------|
| `pending` / `processing` rows whose `updated_at` is older than `SWEEP_STALE_AGE` (30 min) | Delete the row (treated as abandoned). 30 min comfortably exceeds the asynq retry envelope, so the sweeper never races a job still being retried or re-claimed under the lease ([§4.2](#42-worker-pod)) |
| `failed` rows whose `updated_at` is older than a short grace period | Delete the row |
| `done` rows whose QR object is older than `QR_OBJECT_TTL` | Delete the QR object from storage, set `qr_object = NULL` (the row itself is permanent) |

Deleting a row releases its `slug` from the `UNIQUE` constraint, so a custom slug tied to a failed or abandoned job becomes claimable again. `done` rows are never deleted — only their QR object is reclaimed.

---

## 7. Queue Design

The queue is accessed through a `Queue` interface with two implementations:

- **`inproc`** — a buffered Go channel plus a goroutine worker pool, used in Milestone 1. No external dependency; lets the full async pipeline (including the `202` contract) be built and tested before the queue infrastructure (Redis) exists. Not durable — see [§4.1 graceful shutdown](#41-api-gateway).
- **`asynq`** — `github.com/hibiken/asynq`, Redis-backed, used from Milestone 2 onward.

Swapping implementations does not change the API contract or the job payloads.

### Job payloads

```go
type ShortenJobPayload struct {
    JobID       string `json:"job_id"`
    OriginalURL string `json:"original_url"`
    WebhookURL  string `json:"webhook_url"`
    APIKeyID    string `json:"api_key_id"`
    CustomSlug  string `json:"custom_slug,omitempty"`
    EnqueuedAt  int64  `json:"enqueued_at"`
}

type WebhookJobPayload struct {
    JobID      string `json:"job_id"`
    EnqueuedAt int64  `json:"enqueued_at"`
}
```

### Queues and configuration

Two functional queues, processed by the worker server:

```go
asynq.Config{
    Queues: map[string]int{
        "shorten": 3,   // weight
        "webhook": 2,
    },
    RetryDelayFunc: func(n int, err error, t *asynq.Task) time.Duration {
        // webhook tasks follow the §8 schedule; shorten tasks use a
        // capped exponential: 10s, 30s, 2m, 5m, 10m
        ...
    },
    MaxRetry: 5,  // WEBHOOK_MAX_ATTEMPTS for the webhook queue
}
```

- **Shorten jobs** are enqueued with `asynq.Unique(ttl)` keyed on `job_id` (deduplicates *enqueue*) and a short `asynq.Timeout` (~2 min, aligned with `CLAIM_LEASE`) so a crashed worker's task is bounded and asynq can redeliver it. The redelivered task unconditionally re-claims the abandoned `processing` row ([§4.2](#42-worker-pod)); the lease token in the finalize/fail `UPDATE` ensures only one writer ever lands the terminal transition.
- **Webhook jobs** retry on the [§8](#8-webhook-contract) schedule.

### Dead-letter queue

Jobs that exhaust all retries are moved to asynq's **archived set** (`asynq:{queue}:archived`), which serves as the dead-letter queue. The observer hub monitors archived-set size and emits a `dlq_nonempty` warning event when it is non-empty.

---

## 8. Webhook Contract

After a shorten job completes, a separate webhook-delivery job POSTs to the client's `webhook_url`.

### Request

```text
POST {webhook_url}
Content-Type: application/json
X-ShortLink-Signature: sha256={hmac}
X-ShortLink-Job-ID: job_01J8XYZ...
X-ShortLink-Key-Hint: a8sdG1
```

```json
{
  "job_id": "job_01J8XYZ...",
  "status": "success",
  "short_url": "https://sl.example.com/xK9p2aT",
  "qr_code": {
    "download_url": "https://storage.example.com/shortlink-qr/2025/05/18/job_01J8XYZ.png?X-Amz-Signature=...",
    "expires_at": "2025-05-18T12:01:00Z",
    "size_bytes": 3241
  },
  "original_url": "https://very-long-url.example.com/path",
  "created_at": "2025-05-18T12:00:00Z"
}
```

The `download_url` is **re-presigned on every delivery attempt**, so `expires_at` is always ~60 s after the attempt that actually reached the client.

### Headers

- **`X-ShortLink-Signature`** — HMAC-SHA256 of the raw request body, keyed with the API key's **`webhook_secret`** (not the API key itself — see [§9](#9-security)). Clients verify this before trusting the payload.
- **`X-ShortLink-Job-ID`** — the job ID, for client-side deduplication (webhook delivery is at-least-once).
- **`X-ShortLink-Key-Hint`** — the last 6 characters of the API key. A receiver that serves multiple keys (such as the load-test sink) uses this to pick the correct verification secret; a single-key client can ignore it.

### Retry behaviour

| Attempt | Delay before attempt |
|---------|----------------------|
| 1 | Immediate |
| 2 | 5 seconds |
| 3 | 30 seconds |
| 4 | 2 minutes |
| 5 | 5 minutes |

Total window from the first attempt is ~7m35s — well within the ~15-minute QR object lifetime. After `WEBHOOK_MAX_ATTEMPTS` failures the job is archived (dead-letter). The observer emits a `webhook_failed` error event for each failed attempt.

### Webhook failure payload

If a delivery attempt's body must report failure (e.g. final notification semantics), the shape is:

```json
{
  "job_id": "job_01J8XYZ...",
  "status": "failed",
  "error": "webhook delivery failed after 5 attempts",
  "short_url": "https://sl.example.com/xK9p2aT"
}
```

The short URL and Postgres record are always created — the failure is delivery-only.

---

## 9. Security

### API key format

```text
sl_live_<base62-random-32-chars>
```

Example: `sl_live_4xKpZ9mQwRvLjB2nHcYtFoUeA8sdG1iN`

Keys are generated once, shown once, never stored in plaintext. Only a SHA-256 hash is persisted in Postgres.

### Authentication flow

1. Client sends `X-Api-Key: sl_live_...` in the request header.
2. Gateway computes `SHA-256(key)`.
3. Looks up the hash in Postgres; if not found or `revoked_at IS NOT NULL`, return `401`.
4. Injects resolved `api_key_id` and `tier` into the request context.
5. Updates `last_used_at` — **throttled**: the gateway writes Postgres only if the Redis key `lu:{key_hash}` is absent, then sets it with a `LAST_USED_THROTTLE` TTL (default 5 min). This bounds `last_used_at` writes to at most one per key per window regardless of traffic; the column is eventually consistent within that window, by design.

### Webhook signing secret

Webhook payloads are signed with a **dedicated per-key secret** (`whsec_<base62-random>`), stored in the `api_keys` table and shown once at key creation.

Why a separate secret, and why it is stored (not hashed) — the asymmetry is the key point:

- The **API key** is only ever *verified* (an equality check), so storing a SHA-256 hash is sufficient and ideal.
- The **webhook secret** must be *reproduced* — the server is the signer and the client is the verifier, and HMAC requires both sides to hold the same secret. It therefore cannot be hash-only.

This mirrors how Stripe and GitHub separate inbound API credentials from outbound webhook signing secrets, and it allows per-key rotation. For this project the secret is stored in plaintext in Postgres; **in production it should be encrypted at rest** (KMS / a secret manager). Deriving it from a single server-wide master key was rejected — one master-key leak would compromise every client's webhook integrity at once, with no per-key rotation.

### Rate limiting

Per-key sliding window implemented with a Redis sorted set:

```text
ZADD  rl:{key_hash}  {now_unix_ms}  {request_id}
ZREMRANGEBYSCORE  rl:{key_hash}  -inf  {window_start_ms}
ZCARD  rl:{key_hash}   → if > limit, reject 429
EXPIRE rl:{key_hash}  60
```

All four commands execute atomically in a Lua script to prevent race conditions. See the v1-scope note in [§4.1](#41-api-gateway) regarding unauthenticated traffic.

### Webhook signature verification (client side)

```python
import hmac, hashlib
expected = "sha256=" + hmac.new(webhook_secret.encode(), body, hashlib.sha256).hexdigest()
assert hmac.compare_digest(expected, request.headers["X-ShortLink-Signature"])
```

### SSRF protection

The server makes outbound HTTP requests to a **user-supplied** `webhook_url`, which is a classic SSRF surface (OWASP A10). `internal/security/ssrf.go` defends it:

1. **Scheme allow-list** — `http` / `https` only.
2. **IP validation** — resolve the hostname via `net.LookupIP` and reject if any resolved address is loopback, private (RFC 1918), link-local (`169.254.0.0/16`, `fe80::/10`), unique-local (`fc00::/7`), unspecified, or multicast. Go's `net.IP` exposes `IsLoopback()`, `IsPrivate()`, `IsLinkLocalUnicast()`, etc.
3. **Allow-list exception** — `SSRF_ALLOWLIST` (comma-separated hosts and/or CIDRs) exempts specific targets from the internal-IP rejection. It is **empty by default** (production). Local development adds the load-test webhook sink's host so the end-to-end pipeline can run ([§4.4](#44-load-test-runner), [§13](#13-local-development)). Allow-listed targets still pass the scheme check and are still subject to the timeout / redirect / body-size limits below.
4. **Validate twice** — at the gateway (`POST /shorten` → `422` on failure) and again at delivery time, because DNS can change between enqueue and delivery (DNS rebinding).
5. **Safe dial** — webhook delivery uses a custom `http.Client` whose `DialContext` re-validates the resolved IP at connect time (closing the validate-then-connect TOCTOU window), with an enforced timeout, a capped response body, and a `CheckRedirect` that re-validates every redirect hop.
6. **Defense in depth (k8s)** — a `NetworkPolicy` (`deploy/k8s/networkpolicy.yaml`) restricts worker-pod egress away from internal cluster ranges.

### URL validation

Submitted `url` values (the link being shortened) are validated before enqueuing:
- Must be HTTP or HTTPS.
- Must parse as a valid URL.
- Domain must not be on `URL_BLOCKLIST` (configurable list of self-referential and abused domains).
- Maximum length: 2048 characters.

---

## 10. Observability Events

All services emit structured events to the observer hub via `POST {OBSERVER_URL}/ingest`. This is a deliberate **event-push** model — services emit typed domain events at the source. The observer is **not** a log-file reader or scraper; there is no log parsing. (If raw log aggregation is ever wanted, the future-proof path is Grafana **Loki**, which slots into the existing Grafana — see [§15](#15-tech-stack). That is explicitly a **v2** consideration; v1 builds no log aggregator.)

> **Emission is best-effort.** `internal/events` POSTs asynchronously with a short timeout and a small bounded buffer. If the observer is unreachable — including Milestones 1–3, before the observer exists — the event is dropped and the caller continues. Emitting an event never blocks or fails a request or job.

### Event envelope

```go
type Event struct {
    ID         string         `json:"id"`             // ULID
    Source     string         `json:"source"`         // api | worker | loadtest | observer
    Level      string         `json:"level"`          // info | warn | error
    Kind       string         `json:"kind"`
    APIKeyHash string         `json:"api_key_hash,omitempty"`  // SHA-256 hash — safe to send; the stats key
    APIKeyHint string         `json:"api_key_hint,omitempty"`  // last 6 chars — for display
    Message    string         `json:"message"`
    Meta       map[string]any `json:"meta,omitempty"`
    Timestamp  time.Time      `json:"ts"`
}
```

The observer keys `KeyStat` by `APIKeyHash` (a hash, not the secret — safe to transmit) and shows `APIKeyHint` in the UI. This avoids the collision a 6-char hint alone would cause.

### Event kinds catalogue

One kind is **stat-only**: `request_completed` updates the per-key counters and the rolling p99 window but is not written to the log ring buffer (it is the highest-volume event and would flood the audit log). Every other kind is logged.

| Kind | Source | Level | Logged | Description |
|------|--------|-------|--------|-------------|
| `request_completed` | api | info | stat-only | Request finished; `meta` carries `duration_ms`, `status`, `tier`, `rate_limit` |
| `auth_failure` | api | warn | yes | Invalid or missing key |
| `rate_limit_hit` | api | warn | yes | Request rejected, 429 returned; `meta` carries `tier`, `rate_limit` |
| `job_enqueued` | api | info | yes | Shorten job accepted into queue |
| `job_complete` | worker | info | yes | Shorten job done, webhook job enqueued; `meta` carries QR timing |
| `job_error` | worker | error | yes | Shorten job failed, will retry |
| `job_dlq` | worker | error | yes | Shorten job permanently failed (archived) |
| `webhook_sent` | worker | info | yes | Webhook delivered successfully |
| `webhook_failed` | worker | error | yes | Webhook attempt failed |
| `pod_started` | worker | info | yes | Pod came online |
| `pod_stopped` | worker | info | yes | Pod draining / shutting down |
| `queue_depth_high` | observer | warn | yes | Pending-job count exceeded `QUEUE_DEPTH_THRESHOLD` |
| `dlq_nonempty` | observer | warn | yes | Dead-letter (archived) set has items |
| `attack_started` | loadtest | info | yes | Load test begun |
| `attack_complete` | loadtest | info | yes | Load test finished; `meta` carries the vegeta summary |

---

## 11. Frontend Dashboard

**Served by:** the load test runner (`cmd/loadtest`) at `GET /` on `:8090`; `cmd/loadtest/web/` is embedded into the binary via `go:embed`.  
**Technology:** Vanilla JS + WebSocket (no framework).  
**Role:** a **single-page showcase of load tests and analysis**. It does not control infrastructure — it visualises test runs.

### Data sources

- **WebSocket to the observer** — drives the live per-key table and the log audit panel, using the `snapshot` / `stats` / `log_append` / `reset` protocol from [§4.3](#43-observer-hub).
- **Embedded Grafana panels** (`iframe`) — the metrics monitoring section. Grafana runs independently and is always available; the page simply embeds its panels. (Grafana must be configured with `allow_embedding = true` and anonymous auth for the iframes to render.) Each iframe slot carries a `data-panel` attribute (`jobs-error-rate`, `qr-queue-depth`) that matches the **uid** of a dashboard provisioned in `deploy/grafana/dashboards/`, so a fresh stack renders immediately with no manual Grafana setup. The iframe URL appends `?kiosk=tv&theme=dark&refresh=5s` for chrome-free embedding.

The observer WebSocket URL and the Grafana base URL are **not hard-coded**. The load test runner templates them into the served `index.html` at request time from its `--observer` and `--grafana` flags, so the page works regardless of host or port.

### Layout

```text
┌────────────────────────────────────────────────────────────┐
│  ShortLink — Load Test Showcase            ● Live  [Reset] │
├────────────────────────────────────────────────────────────┤
│  API KEY METRICS  (live via observer WebSocket)            │
│  ┌────────┬─────┬─────────┬──────┬─────┬────┬───┬───────┐  │
│  │ Key    │Tier │RateLimit│ Reqs │ WH  │429 │Err│  p99  │  │
│  ├────────┼─────┼─────────┼──────┼─────┼────┼───┼───────┤  │
│  │..abc123│ pro │ 60/min  │ 4291 │4180 │ 11 │ 2 │  38ms │  │
│  │..def456│ free│ 10/min  │  847 │ 644 │203 │ 0 │ 142ms │  │
│  │..ghi789│ pro │200/min ⚠│ 1203 │   0 │1203│ 0 │   —   │  │
│  └────────┴─────┴─────────┴──────┴─────┴────┴───┴───────┘  │
├────────────────────────────────────────────────────────────┤
│  MONITORING  (embedded Grafana panels)                     │
│  ┌────────────────────────┐  ┌──────────────────────────┐ │
│  │ jobs/sec, error rate   │  │ QR gen p99, queue depth  │ │
│  │ (iframe → Grafana)     │  │ (iframe → Grafana)       │ │
│  └────────────────────────┘  └──────────────────────────┘ │
├────────────────────────────────────────────────────────────┤
│  LOG AUDIT  (live via observer WebSocket)  [Filter ▾][Clear]│
│  12:00:01 [worker] [info]  job_complete  ...abc123         │
│            slug=xK9p2aT, qr=12ms             TTL: 118s     │
│  12:00:00 [api]    [warn]  rate_limit_hit ...def456        │
│            limit=10/min, current=11/min      TTL: 294s     │
│  11:59:59 [worker] [error] webhook_failed ...abc123        │
│            attempt=2/5, next_retry=30s       TTL: 594s     │
└────────────────────────────────────────────────────────────┘
```

The `Reqs`, `WH`, `429`, `Err`, and `p99` columns map to `KeyStat.TotalReqs`, `Webhooks`, `LimitErrors`, `JobErrors`, and `P99Latency` ([§4.3](#43-observer-hub)).

### Behaviours

- **Table rows are keyed by `key_hash`** (not the hint) so two keys sharing the last 6 chars never collide in the UI.
- **Key row highlighted red** when `limit_errors / total_reqs > 0.5` (majority rejected).
- **TTL badge** counts down live in JS, recalculated from `expires_at`; the browser owns its log ring buffer and prunes expired entries locally (see `log_append`, [§4.3](#43-observer-hub)).
- **Log filter** dropdown: by source (api / worker / loadtest), by level (warn/error only), by API key hint.
- **Connection indicator** turns grey and shows "Reconnecting…" if the WS drops; auto-reconnects with exponential backoff. On reconnect the server re-sends a `snapshot`.
- **Reset / Clear** controls send `cmd` messages over the WebSocket.

---

## 12. Kubernetes Deployment

### API Gateway Deployment

The API gateway is stateless — all state lives in Redis and Postgres — so it runs multiple replicas behind a ClusterIP Service for zero-downtime rolling deploys.

```yaml
# deploy/k8s/api-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shortlink-api
spec:
  replicas: 2
  selector:
    matchLabels:
      app: shortlink-api
  template:
    spec:
      containers:
        - name: api
          image: shortlink-api:latest
          ports:
            - containerPort: 8080
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
```

### Migration Job

Migrations run as a **Kubernetes Job**, triggered by a Helm `pre-install` / `pre-upgrade` hook — once per release, before new application pods roll out. This is preferred over an init container, which would run the migration tooling once *per pod, per rollout* (N pods → N concurrent runs), couple pod readiness to migration tooling, and crash-loop pods on a bad migration instead of failing one Job cleanly.

```yaml
# deploy/k8s/migrate-job.yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: shortlink-migrate
  annotations:
    "helm.sh/hook": pre-install,pre-upgrade
    "helm.sh/hook-weight": "-5"
    "helm.sh/hook-delete-policy": before-hook-creation
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: shortlink-migrate:latest
```

### Worker Deployment

```yaml
# deploy/k8s/worker-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shortlink-worker
spec:
  replicas: 3
  selector:
    matchLabels:
      app: shortlink-worker
  template:
    spec:
      terminationGracePeriodSeconds: 40   # > job drain timeout (30s)
      containers:
        - name: worker
          image: shortlink-worker:latest
          resources:
            requests: { cpu: "500m", memory: "256Mi" }
            limits:   { cpu: "500m", memory: "256Mi" }
          env:
            - name: POD_ID
              valueFrom:
                fieldRef: { fieldPath: metadata.name }
            - name: REDIS_URL
              valueFrom:
                secretKeyRef: { name: shortlink-secrets, key: redis-url }
          livenessProbe:
            httpGet: { path: /healthz, port: 8081 }
            initialDelaySeconds: 5
            periodSeconds: 10
          lifecycle:
            preStop:
              exec:
                command: ["/bin/sh", "-c", "sleep 5"]  # wait for LB drain
```

### Worker autoscaling — KEDA

Worker autoscaling is driven by **KEDA**, a CNCF event-driven autoscaler purpose-built for queue-depth scaling. KEDA's Redis scaler watches the asynq pending-task list directly — no Prometheus or metrics-adapter wiring is needed for scaling.

```yaml
# deploy/k8s/worker-scaledobject.yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: shortlink-worker-scaler
spec:
  scaleTargetRef:
    name: shortlink-worker
  minReplicaCount: 2
  maxReplicaCount: 12
  triggers:
    - type: redis
      metadata:
        address: redis:6379
        listName: "asynq:{shorten}:pending"
        listLength: "10"     # scale up when > 10 pending jobs per pod
```

### PgBouncer

A PgBouncer Deployment + Service sits between application pods and Postgres ([§6](#6-storage-design)). All services point `DATABASE_URL` at PgBouncer; PgBouncer holds a small upstream pool, keeping real Postgres connections bounded regardless of how far KEDA scales the worker tier.

### Scope note

KEDA, PgBouncer, the migration Job, the egress `NetworkPolicy`, and the autoscaling story apply to the Kubernetes deployment. `docker-compose` mirrors the same topology with simpler equivalents ([§13](#13-local-development)).

---

## 13. Local Development

### docker-compose.yml services

| Service | Image | Port(s) | Notes |
|---------|-------|---------|-------|
| `api` | `shortlink-api` | `8080` | Single instance locally (k8s runs 2 replicas — §12) |
| `worker` | `shortlink-worker` | `8081` (not published) | 3 replicas; shorten + webhook + sweeper |
| `observer` | `shortlink-observer` | `9000` | Backend only (ingest + WebSocket) |
| `migrate` | `shortlink-migrate` | — | One-shot; `depends_on: postgres` (healthy) |
| `postgres` | `postgres:16-alpine` | `55432` → `5432` | Host port `55432` avoids colliding with a native Postgres install |
| `pgbouncer` | `edoburu/pgbouncer` | `6432` | Transaction-pooling in front of Postgres |
| `redis` | `redis:7-alpine` | `6379` | No persistence needed locally |
| `minio` | `minio/minio` | `9000` API / `9001` console | S3-compatible object storage |
| `prometheus` | `prom/prometheus` | `9091` → `9090` | Scrapes `/metrics` from all services |
| `grafana` | `grafana/grafana` | `3000` | Provisioned dashboards + datasource |

Notes:
- `api` runs a **single instance** locally — a published host port (`8080`) cannot be shared by multiple replicas. Kubernetes runs 2 replicas behind a Service.
- `worker`'s port `8081` is **not published to the host**; Prometheus scrapes it over the internal compose network.
- `api` and `worker` declare `depends_on: { migrate: { condition: service_completed_successfully } }` so they start only after migrations finish.
- The host-port overlap between MinIO API and the observer (both internal `9000`) is resolved by mapping MinIO to a different host port; inside the compose network services use service names.
- Postgres is published on host port **`55432`** (not `5432`) so the stack coexists with a native Postgres install on the developer's machine; inside the compose network it still listens on `5432`. The container uses an explicit `POSTGRES_PASSWORD` — `POSTGRES_HOST_AUTH_METHOD=trust` is unreliable because initdb's default scram rules for `127.0.0.1`/`::1` shadow it.
- The **load test runner is not a compose service** — it runs on the host on demand via `make loadtest` (a one-shot, lasting the attack duration). Its webhook sink listens on the host at `:8091`; the compose `worker` reaches it through `host.docker.internal`, so `SSRF_ALLOWLIST` for `api` and `worker` is set to `host.docker.internal` (resolves natively on Docker Desktop; on Linux add `extra_hosts: ["host.docker.internal:host-gateway"]`).
- The compose file is **built up across milestones** — Postgres + MinIO from M1, Redis from M2, Prometheus + Grafana from M7, PgBouncer at M8 ([§17](#17-implementation-milestones)).
- **Prometheus host port is `9091`** (not the canonical `9090`) because the observer already owns host `9090` during local dev. Grafana still reaches Prometheus on the internal compose name `http://prometheus:9090`.
- **Scrape model differs by environment.** Locally `api`/`worker`/`observer` run on the host (`make run-*`), so the Prometheus container reaches them via `host.docker.internal:8080/8081/9090` (compose declares `host.docker.internal:host-gateway` as an `extra_host` so Linux works too). In Kubernetes (M8) they become pod targets discovered by labels — the dashboards and metric names do not change.

### Makefile targets

```makefile
make dev          # docker-compose up --build
make migrate      # run goose migrations (one-shot)
make keys         # run cmd/keygen: generate 3 test API keys (free, pro,
                  # abuser) + webhook secrets, insert hashes, write keys.yaml
make loadtest     # go run ./cmd/loadtest with defaults
make build        # build all binaries
make test         # go test ./...
make lint         # golangci-lint run
```

### First-run sequence

```bash
git clone https://github.com/leninboccardo/shortlink
cd shortlink
make dev          # starts all services (migrations run as the one-shot service)
make keys         # generates test keys + webhook secrets
make loadtest     # starts the attack + serves the showcase page at :8090
                  # Grafana available at :3000, Prometheus at :9090
```

> `config/keys.yaml` contains real key material and **must be gitignored**. `make keys` regenerates it locally; it is never committed.

---

## 14. Configuration

All binaries are configured through environment variables, parsed by `internal/config` (`caarlos0/env`) with the defaults below. The load test runner is the exception — it takes CLI flags ([§4.4](#44-load-test-runner)).

| Variable | Default | Used by | Description |
|----------|---------|---------|-------------|
| `LOG_LEVEL` | `info` | all | `slog` level (`debug`/`info`/`warn`/`error`) |
| `SHORT_URL_BASE` | `http://localhost:8080` | api, worker | Base URL for building short links and QR content |
| `DATABASE_URL` | `postgres://shortlink:shortlink@localhost:55432/shortlink?sslmode=disable` | api, worker, observer, keygen, migrate | Postgres DSN. The local default targets the docker-compose Postgres on host port `55432`; in k8s this points at **PgBouncer** (`:6432`), not Postgres directly |
| `PG_POOL_SIZE` | `8` (api) / `4` (worker) | api, worker | `pgxpool` max connections per process |
| `REDIS_URL` | `redis://localhost:6379` | api, worker, observer | Queue, rate-limit windows, pod heartbeats |
| `API_PORT` | `8080` | api | HTTP listen port |
| `WORKER_PORT` | `8081` | worker | Health + metrics port |
| `OBSERVER_PORT` | `9090` | observer | HTTP + WebSocket port. Local default is `9090` (not `9000`) to leave the conventional MinIO host port free |
| `OBSERVER_URL` | `http://localhost:9090` | api, worker, loadtest | Target for event emission (`POST /ingest`) |
| `OBSERVER_INGEST_TOKEN` | (empty) | api, worker, loadtest, observer | Shared bearer token gating `POST /ingest`. Empty (the local-dev default) leaves `/ingest` open — the observer logs a WARN at startup. When set, the observer requires `Authorization: Bearer <token>` and every emitter must be started with the same value so it can attach the header. `/stream` is unaffected (read-only; gated by Origin allowlist) |
| `QUEUE_DEPTH_THRESHOLD` | `100` | observer | Pending-job count above which `queue_depth_high` is emitted |
| `MINIO_ENDPOINT` | `localhost:9000` | worker | Object storage endpoint |
| `MINIO_ACCESS_KEY` | `minioadmin` (local) | worker | Object storage credential; a real secret in production |
| `MINIO_SECRET_KEY` | `minioadmin` (local) | worker | Object storage credential; a real secret in production |
| `MINIO_BUCKET` | `shortlink-qr` | worker | QR bucket |
| `MINIO_USE_SSL` | `false` | worker | TLS to object storage |
| `SIGNED_URL_TTL` | `60s` | worker | Presigned QR download-URL lifetime |
| `QR_OBJECT_TTL` | `15m` | worker | Age at which the sweeper deletes a QR object ([§6](#6-storage-design)) |
| `QR_SIZE` | `256` | worker | QR PNG side length in px |
| `SLUG_LENGTH` | `7` | worker | Generated slug length (base62) |
| `SLUG_MAX_RETRIES` | `5` | worker | Collision retries before the length is bumped |
| `WORKER_CONCURRENCY` | `3` | worker | Shorten-handler goroutines per pod |
| `CLAIM_LEASE` | `2m` | worker | asynq task timeout for the shorten job; also the freshness window for the `updated_at` lease token that guards finalize/fail. The claim itself is unconditional and does **not** key off this value ([§4.2](#42-worker-pod)) |
| `SWEEP_STALE_AGE` | `30m` | worker | Age at which the sweeper deletes abandoned `pending`/`processing` rows |
| `DRAIN_TIMEOUT` | `30s` | worker | Graceful-shutdown drain window |
| `RATE_LIMIT_FREE` | `10` | api | Free-tier requests/minute |
| `RATE_LIMIT_PRO` | `60` | api | Pro-tier requests/minute |
| `LAST_USED_THROTTLE` | `5m` | api | `last_used_at` write-throttle window |
| `WEBHOOK_MAX_ATTEMPTS` | `5` | worker | Webhook delivery attempts before archiving |
| `SSRF_ALLOWLIST` | (empty) | api, worker | Comma-separated hosts/CIDRs exempt from the internal-IP block ([§9](#9-security)). Empty in production; the local stack sets it to `host.docker.internal` so the API/worker can reach the host-run webhook sink |
| `URL_BLOCKLIST` | (empty) | api | Comma-separated domains rejected for submitted URLs |
| `POD_ID` | hostname | worker | Identity for the Redis heartbeat key (k8s: downward API `metadata.name`) |

Secrets (`DATABASE_URL`, `REDIS_URL`, `MINIO_ACCESS_KEY`, `MINIO_SECRET_KEY`) come from a Kubernetes `Secret` in production and from `docker-compose.yml` / a local `.env` file in development.

---

## 15. Tech Stack

| Layer | Library / Tool | Rationale |
|-------|---------------|-----------|
| Language | Go 1.26 | Module `github.com/leninboccardo/shortlink` |
| HTTP router | `net/http` stdlib + `go-chi/chi` | Lightweight, idiomatic |
| Task queue | `hibiken/asynq` | Redis-backed, battle-tested |
| QR generation | `skip2/go-qrcode` | Pure Go, no CGO, minimal allocations |
| Database driver | `jackc/pgx/v5` | Fastest Postgres driver for Go |
| Query codegen | `sqlc` | Type-safe SQL, no ORM magic |
| Migrations | `pressly/goose` | Simple, embeddable |
| Connection pooling | PgBouncer | Bounds Postgres connections at pod scale |
| Object storage | MinIO Go SDK | S3-compatible, works locally and in prod |
| Load testing | `tsenart/vegeta` | Embeddable library, rich metrics |
| WebSocket | `gorilla/websocket` | Stable, well-documented |
| ID generation | `oklog/ulid/v2` | Lexicographically sortable job and event IDs |
| Config | `caarlos0/env` | Struct-based env parsing |
| Logging | `log/slog` (stdlib) | Structured, no external dependency |
| Metrics | `prometheus/client_golang` | `/metrics` endpoint on every service |
| Metrics viz | Grafana | Provisioned dashboards, embedded in showcase page |
| Autoscaling | KEDA | Queue-depth-driven worker autoscaling |
| Containerisation | Docker + docker-compose | Standard |
| Orchestration | Kubernetes + Helm | Standard |
| Integration testing | `testcontainers-go` | Ephemeral Redis/Postgres/MinIO for tests |
| Log aggregation | Grafana Loki | **v2 only** — not built in v1 |

---

## 16. Non-Functional Requirements

### Performance targets (single worker pod, 0.5 CPU / 256 MB)

| Operation | Target |
|-----------|--------|
| QR code generation | < 10 ms p99 |
| Shorten job (claim → finalize, webhook enqueued) | < 100 ms p99 under normal load |
| Webhook delivery (enqueue → client 2xx) | < 500 ms p99, network-bound |
| Slug redirect (`GET /:slug`) | < 5 ms p99 (Postgres index lookup) |
| Observer broadcast latency | < 600 ms (500 ms tick + network) |

### Reliability targets

| Concern | Approach |
|---------|----------|
| Worker crash mid-job | asynq redelivers the task on pod death; the redelivered task unconditionally re-claims the abandoned `processing` row, and the lease-token finalize guard prevents a recovered stale worker from overwriting it ([§4.2](#42-worker-pod)) |
| Redis restart | Jobs survive if asynq runs with AOF persistence |
| Webhook failure | Decoupled queue; 5-attempt retry with backoff |
| Slow client webhook | Cannot block shorten throughput — separate queue/tier |
| Pod eviction | Graceful drain; in-flight jobs finish before SIGTERM kills the pod |
| QR expiry race | Short URL always created; signed URL re-presigned per retry; object outlives retry window |
| Abandoned `pending` row | Sweeper deletes it, freeing the reserved slug |
| Orphaned QR object | Sweeper deletes objects past `QR_OBJECT_TTL`; a 1-day MinIO lifecycle rule is a backstop |
| Observer unavailable | Event emission is best-effort fire-and-forget; requests and jobs are unaffected |

### Scalability

The system scales horizontally on **two tiers**:

- **API gateway** — stateless (all state in Redis/Postgres), runs 2+ replicas behind a ClusterIP Service. Demonstrates the payoff of externalised state: zero-downtime rolling deploys, no sticky sessions.
- **Worker tier** — autoscales 2–12 replicas via KEDA on queue depth.

The **observer hub is deliberately single-instance**. It is a stateful operability tool (in-memory aggregation + live WebSocket connections), not a request-path component; making it multi-instance would require shared state and sticky WS routing for no real gain. The contrast — a stateless, horizontally scaled serving path next to a deliberately single-instance observability tool — is itself part of what the project demonstrates.

---

## 17. Implementation Milestones

### Milestone 1 — Core pipeline, async via in-process queue (no Redis, no k8s)
- [ ] Minimal `docker-compose.yml` (Postgres + MinIO) for local infra
- [ ] `internal/config` — env-based configuration with defaults ([§14](#14-configuration))
- [ ] Postgres schema + migrations (goose); `cmd/migrate`
- [ ] `cmd/keygen` — generate API keys + webhook secrets, insert hashes, write `keys.yaml` (`make keys`)
- [ ] API key + webhook-secret validation
- [ ] `POST /shorten` → resolve webhook URL → SSRF-validate → write `pending` row → enqueue (in-process) → `202`
- [ ] Optional `expires_in` → gateway sets `expires_at`
- [ ] In-process queue + worker pool: claim → slug → QR → MinIO upload → finalize row
- [ ] Webhook delivery: HMAC signing, `X-ShortLink-Key-Hint` header, re-presigned signed URL
- [ ] SSRF validation module (`internal/security`) incl. `SSRF_ALLOWLIST`
- [ ] `GET /:slug` redirect (honours `status` and `expires_at`)
- [ ] Graceful shutdown — drain the in-process queue on `SIGTERM`

### Milestone 2 — Redis-backed async queue
- [ ] Redis + asynq setup (extend `docker-compose.yml`)
- [ ] Swap the `inproc` queue for the `asynq` implementation behind the same `Queue` interface
- [ ] Split the worker into its own binary
- [ ] Separate `shorten` and `webhook` queues
- [ ] Lease-based idempotency claim (status machine + crash recovery) + `asynq.Unique` and task timeout
- [ ] Retry + dead-letter (archived set)
- [ ] Permanent-failure handling (`status='failed'`)
- [ ] Stale-record + orphaned-object sweeper (`internal/sweeper`)

### Milestone 3 — Rate limiting + auth tiers
- [ ] Sliding-window rate limiter in Redis (Lua script)
- [ ] Tier system (free / pro / unlimited)
- [ ] 429 responses with correct headers (and `X-RateLimit-*` on success)
- [ ] Throttled `last_used_at` updates

### Milestone 4 — Observer hub
- [ ] `/ingest` endpoint + best-effort emitter (`internal/events`)
- [ ] In-memory aggregator with TTL; `KeyStat` / `SystemStat` derivation
- [ ] Redis poller — queue depth + pod heartbeats; `queue_depth_high` / `dlq_nonempty`
- [ ] WebSocket broadcaster — `snapshot` / `stats` / `log_append` / `reset` protocol; custom `CheckOrigin`
- [ ] Event emission wired into API gateway and worker

### Milestone 5 — Load test runner
- [ ] `cmd/loadtest` binary — CLI flags, multi-key vegeta attack runner
- [ ] Per-key metrics collection
- [ ] `keys.yaml` config (`attack_rate_per_min`)
- [ ] Built-in HMAC-verifying webhook sink as the attack webhook target
- [ ] `attack_started` / `attack_complete` events emitted to the observer

### Milestone 6 — Showcase frontend
- [ ] Single-page UI under `cmd/loadtest/web/`, embedded and served by `cmd/loadtest`
- [ ] Observer + Grafana URLs templated into the page
- [ ] WebSocket client, live key-stats table (keyed by `key_hash`)
- [ ] Log audit panel with client-side TTL countdown and ring buffer
- [ ] Filter controls + reset command
- [ ] Embedded Grafana panels (iframe)

### Milestone 7 — Observability stack
- [ ] Prometheus `/metrics` endpoints on all services (extend `docker-compose.yml`: Prometheus + Grafana)
- [ ] `internal/metrics` collectors (jobs, QR duration, rate-limit hits, webhook deliveries, active pods, dropped events)
- [ ] Metric labels limited to low-cardinality dimensions (`tier`, `status`) — never `api_key`
- [ ] Prometheus config + provisioned Grafana dashboards committed to the repo

### Milestone 8 — Kubernetes + deployment
- [ ] Dockerfiles for all binaries
- [ ] Finalize `docker-compose.yml` (add PgBouncer; one-shot migrate wired)
- [ ] K8s manifests: API Deployment (2 replicas) + Service, Worker Deployment
- [ ] KEDA ScaledObject for worker autoscaling
- [ ] PgBouncer Deployment + Service
- [ ] Migration Job (Helm pre-upgrade hook)
- [ ] ConfigMap + Secrets
- [ ] SSRF egress NetworkPolicy
- [ ] Graceful shutdown handling verified under pod eviction

### Milestone 9 — Polish
- [ ] README with architecture diagram and quickstart
- [ ] End-to-end integration test (`testcontainers-go`: Redis/Postgres/MinIO)

---

*Specification version: 2.6 — revised during Milestone 1 implementation (2026-05-22).*  
*All library versions should be pinned in `go.mod` before implementation begins.*
