# Code Audit — deferred findings

Audit of the Milestone 1 + Milestone 2 code, performed 2026-05-22 before
starting M3. Each finding has a **fix-now score (0–10)** — how worth fixing it
was judged to be *before M3* — and a rough confidence.

## Resolved (commit `fix: address pre-M3 audit findings`)

`P2` SSRF DNS-lookup timeout · `P3` HTTP server timeouts · `P5` bounded
hit-recorder · `B2` MinIO bucket-creation race · `B5` `expires_in` overflow ·
`B6` startup connection timeout.

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

### P7 — Pool sizing (score 2, confidence 90%)
One `PG_POOL_SIZE` (8) is used for both binaries; SPEC §14 wants 8 (api) /
4 (worker). `MinConns` is 0, so the first queries pay connection-setup latency.
*Suggested fix:* per-binary pool sizing; set a small `MinConns` to keep
connections warm.

## Deferred — bugs / correctness

### B1 — Orphaned QR objects leak in MinIO (score 4, confidence 85%)
The sweeper only reclaims QR objects referenced by `done` rows. A job that
uploaded its QR then failed at the finalize step leaves the row `failed`;
`DeleteOldFailedShortURLs` deletes the row but never the MinIO object →
permanent orphan. Same for swept stale `processing` rows whose worker had
already uploaded. The SPEC §6 backstop (a 1-day MinIO lifecycle rule) is not
configured.
*Suggested fix:* configure the MinIO lifecycle rule in `deploy/`, and/or have
the sweeper list-and-compare bucket objects against live rows.

### B3 — Custom slug locked 30 min on a transient enqueue failure (score 3, confidence 90%)
If the row insert succeeds but `Enqueue` then fails (Redis blip), the orphan
`pending` row holds the custom slug until the sweeper runs (`SWEEP_STALE_AGE`,
30 min); client retries get `409` in the meantime.
*Suggested fix:* on enqueue failure, delete the just-inserted row before
returning the error.

### B4 — Webhook silently dropped if it runs after the QR is swept (score 2, confidence 70%)
`handleWebhookJob` returns `nil` (no delivery, no error) when
`!row.QrObject.Valid`. The retry window (~7.5 min) is normally inside
`QR_OBJECT_TTL` (15 min), but a webhook delayed by a long queue backlog or
worker downtime could run after the sweep → silent non-delivery.
*Suggested fix:* deliver a degraded payload (no `qr_code`) or emit an explicit
failure event instead of silently skipping.

## Deferred — security

### S1 — `cmd/keygen` re-runs accumulate valid, untracked keys (score 3, confidence 90%)
Each `make keys` inserts 3 new keys and overwrites `config/keys.yaml`; previous
keys stay *valid* in the DB with no record of them.
*Suggested fix:* on re-run, revoke (set `revoked_at`) existing non-revoked keys,
or refuse to run when keys already exist unless `--force` is given.

> SSRF / URL validation, log hygiene, and the (documented) plaintext webhook
> secret were reviewed and are acceptable as-is — no action needed.

## Deferred — dead code

### D1 — `internal/queue/inproc.go` unused since M2 (score 2, confidence 100%)
~110 lines; no binary constructs `InProc`. Kept intentionally per SPEC §7
(two implementations behind the `Queue` interface). *Decision needed:* keep it
as a deliberate showcase of the abstraction, or delete it.

### D2 — `ShortenJobPayload.APIKeyID` populated but never read (score 1, confidence 100%)
The gateway fills it; the worker re-derives the key from the row. It is a
SPEC §7-defined field. *Suggested fix:* drop the field, or leave it documented.

### D3 — generated `db.Querier` interface unused (score 1, confidence 100%)
Emitted by `emit_interface: true` in `sqlc.yaml`. Harmless; useful later for
mocking in tests. Leave as-is.
