# Code Audit — deferred findings

This file is the running log of audit findings that were intentionally left
unresolved. Each finding has a **fix-now score (0–10)** at the time it was
recorded, plus a rough confidence.

## Audit history

| Date       | Scope          | Resolved in                                    | Deferred (this file)               |
|------------|----------------|------------------------------------------------|------------------------------------|
| 2026-05-22 | M1+M2 (pre-M3) | `2c5f266` — P2, P3, P5, B2, B5, B6             | P1, P4, P6, P7, B1, B3, B4, S1, D1, D2, D3 |
| 2026-05-23 | M3+M4+M5       | commits `6941fc3..1307dc7` — P8, P9, P10, B6, B8, B10, B13, B14, D2, D4, D5, D8, S2, S3, S5 | B11 (re-reviewed and rejected — analysis was wrong) |
| 2026-05-24 | M7             | commits `2a66607..1935b4e` — B1+B7, B2+B4, B3, B5+D1, B6, S1, S2, D2 (all 11 in-scope findings) | none |
| 2026-05-24 | M8+M9          | not separately audited — M8 chart was written by inspection (helm/kind not installed locally to render-verify), M9 is text + the integration test under tests/ which is its own form of validation | M8 pod-eviction smoke (needs a real cluster) |
| 2026-05-24 | post-M9 full repo + SPEC coverage | commits `ae172a0..d2c72d4` — B1, B2, B3, B5, B6, B7, B8, B9, COV-4, COV-5, COV-18, S1, S2, S3, S4, P5, P1+B4 (bundled), D1, D5, D6, D7, D9; SPEC drift in `d2c72d4` (COV-1, 2, 3-drop, 6, 7, 8, 9, 10, 11, 15, 17, 19) | B10 (verified false positive — vegeta.Stop is sync.Once-guarded), B4 (verified benign — ObjectKey deterministic from jobID) |
| 2026-05-25 | v1-readiness sweep (spec-vs-code) | this pass — pgx `QueryExecModeExec` for PgBouncer transaction-mode safety; `PG_POOL_SIZE` split into `API_PG_POOL_SIZE`/`WORKER_PG_POOL_SIZE` (P7); `DATABASE_URL` default re-pointed at PgBouncer `:16432` to match §13; MinIO 1-day lifecycle backstop installed at worker boot (B1); pending-row rollback on enqueue failure (B3); webhook silent-drop on swept QR replaced with `webhook_failed` emit (B4-prev-M1); SPEC drift in §4.2 grace period and §7 `asynq.Unique → asynq.TaskID` | none |

> M3+M4+M5 audit findings B7, B9, B12, P12, P13, P15, P17, S4, S6, S7, S8,
> S9, S10, D6, D7, D9 were investigated and rejected as not real issues —
> see the curated audit report in conversation history for the reasoning.
>
> M7 audit finding S3 (`template.JS` injection via JSON value containing
> `</script>`) was rejected after verification: Go's `encoding/json` HTML-
> escapes `<`, `>`, and `&` to `<`, `>`, `&` by default, so
> a `</script>` byte sequence in any string value renders as
> `</script>` and cannot break out of the script tag. No other
> deferred items from M7.

---

## Deferred — performance

### P1 — Hot-row `hit_count` counter (score 6, confidence 95%)
Every redirect runs `UPDATE short_urls SET hit_count = hit_count+1`. For a
viral link every hit contends on one row's lock and creates a dead tuple
(MVCC bloat → autovacuum pressure). Redirects are the highest-volume operation.
*Suggested fix:* drop the denormalized counter and derive counts from `hits`,
or batch increments in Redis and flush periodically. This is a SPEC §5 design
choice — fixing it means revisiting the spec.

### P4 — Per-request auth query, no cache (score 5, confidence 95%)
Every `POST /shorten` does `GetAPIKeyByHash` + insert (2 queries) against a
pool of 8. Auth results are highly cacheable.
*Suggested fix:* a small in-memory LRU (or Redis) cache of key-hash → resolved
key, short TTL. Pairs naturally with M3's per-request Redis rate-limiting.

### P6 — MinIO `Stat` round-trip per webhook attempt (score 3, confidence 95%)
`handleWebhookJob` calls `store.Stat` on every delivery attempt solely to fill
`qr_code.size_bytes`.
*Suggested fix:* store the QR size in a new `short_urls` column at finalize
time, or drop `size_bytes` from the webhook payload.

### P7 — Pool sizing (resolved 2026-05-25)
Split into `API_PG_POOL_SIZE` (8) and `WORKER_PG_POOL_SIZE` (4) — matches SPEC
§14, distinct values per binary. `MinConns` warmup was rejected: pgx defaults
already cover this for the request path (first query opens a conn that stays
hot under load), and forcing min-conns would just create idle PgBouncer
sessions at startup. See `internal/config/config.go`.

## Deferred — bugs / correctness

### B1 — Orphaned QR objects leak in MinIO (resolved 2026-05-25)
The SPEC §6 backstop — a 1-day MinIO/S3 lifecycle rule on the bucket — is now
installed by `ensureLifecycleBackstop` in `internal/storage/minio.go`, called
on every worker boot. SetBucketLifecycle is idempotent (replaces the config),
fail-soft (a misconfigured rule logs a WARN but does not block startup), and
works against any S3-compatible store, so the same Go code handles local
MinIO and prod. List-and-compare against live rows was considered and
rejected — adds bucket-scan cost on every sweeper tick for a backstop that
the lifecycle rule covers structurally.

### B3 — Custom slug locked 30 min on a transient enqueue failure (resolved 2026-05-25)
`cmd/api/handlers.go` now calls a new `DeletePendingReservation` query on
enqueue failure. The query is pending-only-guarded so it can't race a worker
that somehow already claimed the row (defense in depth — Enqueue failed, so
no worker should have it). Best-effort: the rollback is logged but doesn't
mask the original enqueue error to the client.

### B4 — Webhook silently dropped if it runs after the QR is swept (resolved 2026-05-25)
`handleWebhookJob` now treats the `!row.QrObject.Valid` branch as a terminal
failure: emits `webhook_failed` with `error_class=qr_object_swept`, bumps
`shortlink_webhook_attempts_total{status=failure}` and the DLQ counter, and
returns `nil` so asynq doesn't retry (the QR is gone — retrying can't recover
it). Degraded delivery (payload without `qr_code`) was rejected as a worse
outcome than explicit failure: the customer's webhook contract promises a
QR, and silently shipping a partial payload would be harder to debug than a
visible failure event.

### B11 — Heartbeat refresh/Del race (re-reviewed 2026-05-23, REJECTED)
Original concern: `refresh()` in the ticker case could overlap with the
final `Del` in the ctx.Done case. **Re-review found the loop is structurally
safe**: `select` in `runHeartbeat` fires exactly one case per iteration,
`refresh()` is synchronous (no goroutine), so by the time the ctx.Done
branch executes, the previous refresh has returned. Confidence dropped
from 50% to ~20%; finding closed without code change.

## Deferred — security

### S1 — `cmd/keygen` re-runs accumulate valid, untracked keys (score 3, confidence 90%)
Each `make keys` inserts 3 new keys and overwrites `config/keys.yaml`; previous
keys stay *valid* in the DB with no record of them.
*Suggested fix:* on re-run, revoke (set `revoked_at`) existing non-revoked keys,
or refuse to run when keys already exist unless `--force` is given.

> SSRF / URL validation, log hygiene, and the (documented) plaintext webhook
> secret were reviewed and are acceptable as-is — no action needed.

## Deferred — dead code

### D1 — `internal/queue/inproc.go` (resolved post-audit Tier 3)

Deleted. No binary constructed `InProc`, the "showcase of the abstraction"
framing wasn't load-bearing, and SPEC §7's directory listing was updated to
match. Git history preserves the implementation if it's ever wanted back.

### D3 — generated `db.Querier` interface unused (score 1, confidence 100%)
Emitted by `emit_interface: true` in `sqlc.yaml`. Harmless; useful later for
mocking in tests. Leave as-is.

> D2 (`ShortenJobPayload.APIKeyID`) was resolved in the M5 audit pass —
> commit `3b8ccbf`.
