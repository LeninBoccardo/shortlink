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
| 2026-05-26 | external-review follow-up (auth + keygen) | commits `f425c4f` + `97398cb` — `ValidKeyFormat` pre-check in `internal/auth/validator.go` rejects malformed keys before cache/DB (closes the invalid-key DB-pressure gap); `keygen --replace` flag bulk-revokes active keys before reinserting (early form of old-S1 resolution); P4 (auth cache) marked resolved post-hoc as the cache had already landed without the audit doc being updated; P1 (hit-row `hit_count`) tightened with an explicit v2 trigger condition so future reviewers don't keep re-opening the deferral | none |
| 2026-05-27 | post-v1 deep audit (operator-panel security focus) | commits `ce520d7..7b9b762` — Grafana dlq legend disambig (`ce520d7`); SSRF allowlist now supports `host:port` matching + compose/helm pin `loadtest:8091` (closes S1-NEW: webhook → control-plane pivot); `sameOriginGuard` middleware on all `/api/*` mutating endpoints (S2 CSRF); `tierRateMax` caps per-tier `attack_rate_per_min` at 10× the default (S3); `handleAttackStart` rejects malformed JSON instead of silently using defaults (D3/B17); `keygen --replace` now requires interactive `yes` or `--yes` flag with DSN+count echo (old-S1 + new-S4); loadtest Ingress NetworkPolicy added (S5); validator cache sweeper (B12); `dockerStats` subprocess isolated from caller ctx (B13); dead CLI summary deleted, `HasTier` deleted, `24*time.Hour` hoisted to `maxAttackDuration`, local `hintOf` consolidated to `auth.Hint` (D1+D2+D7+D8) | B14 (rejected after re-review: registry lock-across-IO is real but harmless — vegeta measures HTTP roundtrip to api, sink HMAC-verify latency is a different plane); B15/B16/B18, S6/S7, D4–D6/D9/D10 (rejected — minor/speculative, no concrete failure case) |

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

### P1 — Hot-row `hit_count` counter (score 6, confidence 95% — v2)

Every redirect runs `UPDATE short_urls SET hit_count = hit_count+1`. For a
viral link every hit contends on one row's lock and creates a dead tuple
(MVCC bloat → autovacuum pressure). Redirects are the highest-volume operation
*in principle*.
*Suggested fix:* drop the denormalized counter and derive counts from `hits`,
or batch increments in Redis and flush periodically. This is a SPEC §5 design
choice — fixing it means revisiting the spec.

**Why not promoted to v1 (re-reviewed 2026-05-27).** The v1 workload doesn't
exercise the redirect path under load: the vegeta attacker
(`cmd/loadtest/attack.go`) only targets `POST /shorten`; the integration test
does exactly one redirect for functional verification; demo browser traffic is
<1 req/sec. SPEC §16 commits to redirect *latency* (`< 5 ms p99`) but no
throughput SLO, so a single UPDATE per redirect stays inside budget and
autovacuum keeps up trivially. The finding remains correct *in principle* —
the score and confidence describe what would happen if the workload changed.
**Trigger condition for promoting to v1 work:** either (a) the loadtest grows
a redirect-path attack mode, or (b) SPEC §16 adds a redirect-throughput SLO.

### P4 — Per-request auth query, no cache (resolved 2026-05-26)

`internal/auth/validator.go` now caches resolved keys in a `sync.Map` keyed
by hash with a 60-second TTL — the redirect / shorten hot path skips
Postgres entirely on a cache hit. Failed lookups are deliberately *not*
cached so a brute-force flood can't pollute the cache with junk entries;
that gap is closed by a `ValidKeyFormat` pre-check (added 2026-05-26 in the
same package) that rejects malformed keys before touching cache or DB, so
attacker probes with random `X-Api-Key` values never reach `api_keys`.
Cache hits/misses are exported via the `auth_key_cache_total` counter.

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

### S1 — `cmd/keygen` re-runs accumulate valid, untracked keys (resolved 2026-05-27)

`cmd/keygen` gained a `--replace` flag (`f425c4f`) that bulk-revokes every
still-active row in `api_keys` before inserting the fresh tier batch, plus a
follow-up safety pass (`633a879`) that requires either interactive `yes` or
an explicit `--yes` flag *and* prints the target `DATABASE_URL` host plus
the active-key count before any destructive call. The latter exists because
the bulk-revoke is a foot-gun when `DATABASE_URL` is exported from a stale
shell pointed at the wrong cluster — the echo lets the operator notice
"that's not the database I meant" before confirming. The original suggested
fix was "revoke on every re-run or require --force"; the chosen shape splits
that into two opt-ins (`--replace` to wipe, `--yes` to skip the prompt) so
the common `make keys` path stays unchanged.

> SSRF / URL validation, log hygiene, and the (documented) plaintext webhook
> secret were reviewed and are acceptable as-is — no action needed.

## Deferred — operator panel (post-v1 audit)

### B14 — Registry write lock held across `keysfile.Write` (re-reviewed 2026-05-27, REJECTED)

Original concern: `keyRegistry.Append` / `RemoveByHint` hold the write lock
across atomic tempfile + rename in `keysfile.Write`. During a sustained
attack this could inflate sink-side HMAC-verify latency by tens of ms while
the operator clicks Generate.

Rejected after verification: vegeta measures the HTTP roundtrip to the api
gateway; the sink's HMAC verify is a separate measurement plane and does
not feed back into the vegeta `attackResult` numbers. The realistic mutation
cadence is human-pace (a few clicks per minute), and the current "lock
across IO" pattern guarantees readers either see the pre-mutation state OR
the post-mutation-and-on-disk state — dropping the lock between mutation
and write would open a window where a reader sees in-memory state that
hasn't reached disk, then a write failure rolls it back. The proposed fix
trades real consistency for theoretical perf, with no measured impact.

Trigger to revisit: if benchmarking ever shows sink HMAC-verify p99
spiking measurably during operator actions in a way that affects what the
panel reports back to the user.

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
