# ShortLink

A production-grade, observable URL shortening platform built in Go — a
portfolio demonstration of distributed systems, async processing, Kubernetes
orchestration, and real-time observability.

See [docs/SPEC.md](docs/SPEC.md) for the full design, [docs/DECISIONS.md](docs/DECISIONS.md)
for the design-decision log, and [docs/AUDIT.md](docs/AUDIT.md) for deferred
audit findings. The project is built milestone by milestone (SPEC §17).

## Status

**Milestone 2 — Redis-backed async queue.** The shorten pipeline runs over a
Redis/asynq queue: `POST /shorten` reserves a row, enqueues a job, and returns
`202`; a separate worker binary claims the job (lease-based idempotency +
crash recovery), generates the slug + QR code, uploads it, and delivers a
signed, HMAC-signed webhook with retry and a dead-letter queue. A sweeper
reclaims abandoned rows and orphaned QR objects.

## Prerequisites

- Go 1.26+
- Docker (for local Postgres + MinIO + Redis)

## Quickstart

```sh
make dev         # start Postgres + MinIO + Redis (docker compose)
make migrate     # apply the database schema
make keys        # generate test API keys -> config/keys.yaml
make run-worker  # start the worker (in one terminal)
make run-api     # start the gateway on :8080 (in another)
```

Then shorten a URL (use a key printed by `make keys`):

```sh
curl -X POST http://localhost:8080/shorten \
  -H "X-Api-Key: sl_live_..." \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/some/long/path","webhook_url":"https://webhook.site/..."}'
```

The result is delivered asynchronously to the webhook URL. Visiting
`http://localhost:8080/{slug}` redirects to the original URL.

## Layout

| Path | Purpose |
|------|---------|
| `cmd/api` | Gateway — authenticate, reserve, enqueue |
| `cmd/worker` | Queue consumer — shorten + webhook handlers + sweeper |
| `cmd/migrate` | goose migration runner |
| `cmd/keygen` | API key + webhook secret provisioning |
| `internal/` | Domain packages (auth, shortener, qrcode, queue, webhook, sweeper, …) |
| `migrations/` | Postgres schema (goose) |
| `deploy/` | docker-compose stack |
