# ShortLink

A production-grade, observable URL shortening platform built in Go — a
portfolio demonstration of distributed systems, async processing, Kubernetes
orchestration, and real-time observability.

See [SPEC.md](SPEC.md) for the full design. The project is built milestone by
milestone (SPEC §17).

## Status

**Milestone 1 — core pipeline, async via an in-process queue** (no Redis, no
Kubernetes). The shorten pipeline is fully asynchronous: `POST /shorten`
reserves a row, enqueues a job, and returns `202`; a worker pool generates the
slug + QR code, uploads it, and delivers a signed, HMAC-signed webhook.

## Prerequisites

- Go 1.26+
- Docker (for local Postgres + MinIO)

## Quickstart

```sh
make dev        # start Postgres + MinIO (docker compose)
make migrate    # apply the database schema
make keys       # generate test API keys -> config/keys.yaml
make run-api    # start the gateway on :8080
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
| `cmd/api` | Gateway + in-process queue + worker pool (M1) |
| `cmd/migrate` | goose migration runner |
| `cmd/keygen` | API key + webhook secret provisioning |
| `internal/` | Domain packages (auth, shortener, qrcode, queue, webhook, …) |
| `migrations/` | Postgres schema (goose) |
| `deploy/` | docker-compose stack |
