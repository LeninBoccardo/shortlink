# Manual Testing Guide

A runnable walkthrough that exercises every feature shipped through M1–M9
plus the post-M9 audit fixes AND the operator-panel work (UI-driven
keygen + attack lifecycle, both compose modes, loadtest in k8s). Work
top-to-bottom or jump to a specific section — each block notes what it
depends on.

## 0. Prerequisites

This repo has **two local-dev modes**. Pick one before continuing:

- **Dev mode** (the rest of this guide assumes this): `make dev` brings
  up infra in compose, you run api/worker/observer/loadtest on the host
  via `make run-*`. Fast code iteration. Needs Go 1.26.
- **Full mode** (`make full`): everything runs in containers. No Go
  toolchain required. Trades iteration speed for "one command and a
  browser tab". See §6.5 for the full-mode equivalent of §3–§6.

The §7 onward sections (showcase tour, API edge cases, observability
checks, integration test) work identically against either mode — the
loadtest binary's HTTP surface is the same in both.

Common requirements:

- Docker Desktop (Windows/Mac) or Docker Engine (Linux), running
- Go **1.26** on PATH (`go version`) — dev mode only
- `make`, `curl`, `git` available
- Four terminals minimum if you're in dev mode (one each for
  **api**, **worker**, **observer**, **loadtest**, plus one free for
  `curl` / inspection). Full mode needs only one terminal.
- (Optional, only for §11) `kind` and `helm` for the k8s walkthrough

If you just want a single one-shot smoke test, jump to **§10 Integration
test** — it spins up everything ephemerally and asserts the pipeline end
to end in ~8s.

---

## 1. Build the binaries

```bash
go build -o bin/ ./cmd/...
ls bin/
# expected: api  worker  observer  loadtest  keygen  migrate
```

`go build` doubles as the type-check; if this fails nothing else will.

---

## 2. Bring up the infrastructure

`make dev` starts Postgres, PgBouncer, Redis, MinIO, Prometheus, and
Grafana via docker compose. Nothing in here is the shortlink code itself
— the binaries always run on the host during local dev.

```bash
make dev
docker compose -f deploy/docker-compose.yml ps
```

Expected: 6 services running, all healthy. Host ports you'll touch:

| Service     | Host port | What                                      |
|-------------|-----------|-------------------------------------------|
| Postgres    | 55432     | Direct DB access (psql)                   |
| PgBouncer   | 16432     | What the api/worker actually connect to   |
| Redis       | 6379      | asynq, rate limiter, heartbeats           |
| MinIO API   | 9000      | S3-compatible storage                     |
| MinIO Web   | 9001      | Console (login `minioadmin`/`minioadmin`) |
| Prometheus  | 9091      | Scrape target list + queries              |
| Grafana     | 3000      | Anon viewer; dashboards under `Browse`    |

Sanity probe:

```bash
curl -sf http://localhost:9001/                    > /dev/null && echo "minio ok"
curl -sf http://localhost:9091/-/ready             > /dev/null && echo "prometheus ok"
curl -sf http://localhost:3000/api/health          > /dev/null && echo "grafana ok"
```

---

## 3. Apply migrations

```bash
make migrate
```

Verify the tables exist:

```bash
docker exec -it deploy-postgres-1 psql -U shortlink -d shortlink -c "\dt"
# expected: api_keys, short_urls, hits, goose_db_version
```

(Container name varies — use `docker compose ps postgres` to grab the
exact one if `deploy-postgres-1` doesn't match.)

---

## 4. Generate test API keys

```bash
make keys
cat config/keys.yaml
```

Expected: three keys (free, pro, abuser). The raw key material is in
`config/keys.yaml` (gitignored). Hashes are in `api_keys` — confirm:

```bash
docker exec -it deploy-postgres-1 psql -U shortlink -d shortlink \
  -c "SELECT name, tier, left(key_hint, 12) FROM api_keys;"
```

---

## 5. Start the three host binaries

Open three terminals (or background each). Each prints a `listening`
log line when ready.

```bash
# terminal A
make run-observer
# expected: "observer hub listening" on :9090, "websocket broadcaster started"

# terminal B
SSRF_ALLOWLIST=127.0.0.1,localhost,host.docker.internal make run-worker
# expected: "asynq worker started", "sweeper started", "pod_started" event

# terminal C
SSRF_ALLOWLIST=127.0.0.1,localhost,host.docker.internal make run-api
# expected: "api listening" on :8080
```

Health checks (in a fourth terminal):

```bash
curl -sf http://localhost:8080/healthz && echo "api ok"
curl -sf http://localhost:8081/healthz && echo "worker ok"
curl -sf http://localhost:9090/healthz && echo "observer ok"
```

If any of these errors, look at the relevant terminal — config or
connection problems surface there immediately.

---

## 6. Start the load-test runner

The runner does triple duty: it serves the showcase page + operator
panel at `:8090`, hosts the webhook sink at `:8091`, and runs vegeta
attacks **on demand** (no auto-start at boot since the operator panel
work landed).

```bash
# terminal D — pick one of:
make loadtest                                                # default --duration 60s
go run ./cmd/loadtest --duration=10m --grafana=http://localhost:3000
```

What this gives you:

- **Showcase page + operator panel** at <http://localhost:8090> (open
  it manually — auto-open is off so the script works in CI/SSH too).
- **Webhook sink** at `http://localhost:8091/sink` — POST deliveries
  land here, HMAC is verified, the count is reflected in the page's
  "WH" column and the post-attack summary.
- **No attack runs yet.** The binary boots into idle state. To start
  one: open the page, generate at least one key in the **Operator
  panel** (see §7), then click **Start attack**. Or via curl:
  `curl -X POST http://localhost:8090/api/attack/start -d '{"duration_seconds":60}'`.

---

## 6.5 Alternative: full mode (`make full`) — one command, no Go toolchain

Equivalent of §2–§6 above but every binary runs in a container. Useful
when you want to demo the project to someone without setting up Go, or
when you want to exercise the production-shaped network topology
locally (everything reaches everything by compose DNS).

```bash
make full       # builds shortlink-{api,worker,observer,loadtest}:dev,
                # runs migrate, brings the whole stack up
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.full.yml ps
```

Expected: 12 services running (the 8 infra services from §2 plus api /
worker / observer / loadtest). Same host ports for the infra (§2
table), plus:

| Service  | Host port | What                                           |
|----------|-----------|------------------------------------------------|
| api      | 8080      | API gateway, just like dev mode                |
| observer | 9090      | Observer hub + WebSocket `/stream`             |
| loadtest | 8090,8091 | Operator panel + showcase page + webhook sink  |

Differences from dev mode:

- **No `make keys` / `make migrate` steps needed** — `make full` runs
  the migrate job up front, and keys are created via the operator
  panel's Generate button (§7.1).
- **`SSRF_ALLOWLIST` is preset** to include `loadtest` (the in-compose
  hostname of the sink). You don't need to set anything per-terminal.
- **The integration-test card is filtered out** of the test console
  in full mode (no Go toolchain inside the container).
- **`make loadtest` doesn't apply** — the loadtest binary is already
  running in its container.

Once `make full` is up, skip ahead to **§7 Showcase page tour** —
everything from there works identically against either mode.

To tear down: `make full-down` (preserves volumes) or `make dev-down`
(also wipes them — same compose project name).

---

## 7. Showcase page tour (M6 + operator panel)

Open <http://localhost:8090> in your browser. You should see:

- **Top bar** — green `●` "Live" indicator (the WebSocket is open),
  "Reset stats" button.
- **Operator panel** *(post-M9)* — two cards:
  - **Keys** (left): table of registered keys with `name`, color-
    coded tier, 6-char hint, attack rate/min, per-row `revoke` link.
    Below: a Generate form (name + tier select + Generate). Generated
    raw key + webhook secret are shown ONCE in a green callout with
    per-row copy buttons; dismiss to clear.
  - **Attack** (right): live status dot (idle/running/stopping),
    duration input (defaults to 60s), multi-select key picker
    (Ctrl-click for multiple, or none = all), Start/Stop buttons.
  - **Tier gating**: test cards whose `required_tier` isn't covered
    by the current key set get a yellow "needs &lt;tier&gt;" badge,
    a dimmed look, and a disabled Run button. Generate a key of the
    right tier → they snap back to "idle" + enabled.
- **Test console** — manual_testing.md cases runnable from here. Run
  one by clicking its **Run** button; "Run all auto" iterates every
  non-manual card sequentially. Manual cards expose **Show steps**.
- **API key metrics panel** — one row per key seen by the observer.
  Same data as the operator panel's table but enriched with live
  counters (Reqs / WH / 429 / Err / p99). Rows where
  `429/total > 0.5` go red.
- **Monitoring panel** — two Grafana iframes (`jobs/sec · error rate`
  and `QR p99 · queue depth`). If they say "Grafana not configured",
  you forgot `--grafana=...` in step 6.
- **Log audit panel** — newest-first stream of events, per-entry TTL
  badges, source/level/key filters.

### 7.1 — Generate your first key from the UI

If you came in via dev mode and skipped `make keys`, the Keys table
shows "No keys yet — generate one below." Click Generate with the
defaults (name = whatever, tier = pro). The green callout reveals the
raw `sl_live_…` key and the `whsec_…` webhook secret. Copy both
somewhere safe — they disappear on dismiss and will never be shown
again by the server (only the SHA-256 hash is persisted).

The table now lists your key. The attack picker now lists it. The
test cards' "Run" buttons that needed `pro` become enabled.

### 7.2 — Start an attack from the UI

In the Attack card: leave duration at 60s, leave keys empty
(= attack with all registered keys), click **Start attack**. The
status dot turns green and pulses; the test console's `429`-related
cards may trip during the attack as the free-tier limit is exceeded.
Click **Stop** to cancel early — status moves to `stopping` for ≤5s
while vegeta drains, then back to `idle`.

### B6 verification — reset broadcasts to all clients

1. Open **two** browser tabs at <http://localhost:8090>.
2. In tab 1, click **Reset stats**.
3. Tab 2's stats table should also clear within ~500ms.

Pre-audit-fix only the issuing tab would reset; the other would keep
showing stale numbers until the next tick.

### S4 verification — security headers on the page

```bash
curl -sI http://localhost:8090/ | grep -iE "content-security|nosniff|frame-options|referrer"
# expected: Content-Security-Policy: default-src 'none'; script-src 'self' 'sha256-...'
#           X-Content-Type-Options: nosniff
#           X-Frame-Options: DENY
#           Referrer-Policy: no-referrer
```

### Operator panel — control-plane endpoints (post-M9)

The Generate / Start / Stop / Revoke buttons in §7.1–§7.2 are thin
clients over six HTTP endpoints on `:8090`. Useful to curl directly
when scripting or debugging.

```bash
# list keys
curl -s http://localhost:8090/api/keys | jq

# generate a key (name + tier required)
curl -s -X POST http://localhost:8090/api/keys/generate \
  -H "Content-Type: application/json" \
  -d '{"name":"smoke","tier":"free"}' | jq
# expected: {"name":"smoke","key":"sl_live_...","webhook_secret":"whsec_...",
#            "key_hint":"...","tier":"free","attack_rate_per_min":10}

# revoke (use the key_hint from above or from /api/keys)
curl -s -X POST http://localhost:8090/api/keys/revoke \
  -H "Content-Type: application/json" \
  -d '{"key_hint":"abc123"}'
# expected: {"key_hint":"abc123","db_rows":1,"from_registry":true}

# attack lifecycle
curl -s http://localhost:8090/api/attack/status                              # {"state":"idle"}
curl -s -X POST http://localhost:8090/api/attack/start \
  -H "Content-Type: application/json" -d '{"duration_seconds":10}' | jq
curl -s http://localhost:8090/api/attack/status | jq                          # {"state":"running",…}
curl -s -X POST http://localhost:8090/api/attack/start \
  -H "Content-Type: application/json" -d '{"duration_seconds":5}'
# expected: 409 — "an attack is already running"
curl -s -X POST http://localhost:8090/api/attack/stop                         # cancels; status → stopping/idle
```

In dev mode the page server binds `127.0.0.1` (loopback only). In full
mode (`CONTAINER_MODE=true`) it binds `0.0.0.0` because the container
itself is the isolation boundary. Confirm:

```bash
# dev: this should reach the listener
curl -sf http://127.0.0.1:8090/healthz && echo "dev-mode ok"

# full mode: the host port mapping does the iso work; check from the host
curl -sf http://localhost:8090/healthz && echo "full-mode ok"
```

### Tier-gating verification

Reproduces what §7's "rate-limit-*" cards do visually:

```bash
# 1. List tests, note required_tier for each
curl -s http://localhost:8090/tests/list | jq '.[] | {id, required_tier, manual}' | head -30

# 2. Find your free-tier key's hint
HINT=$(curl -s http://localhost:8090/api/keys | jq -r '.[] | select(.tier=="free") | .key_hint' | head -1)
echo "Revoking free-tier key: $HINT"

# 3. Revoke it
curl -s -X POST http://localhost:8090/api/keys/revoke \
  -H "Content-Type: application/json" -d "{\"key_hint\":\"$HINT\"}"

# 4. In the browser, the rate-limit-* cards now show a yellow
#    "NEEDS FREE" badge, are dimmed, and their Run button is disabled
#    with a "Generate a free-tier key first" tooltip.

# 5. Generate one back via the form (or via curl) — the cards re-enable.
```

### keys.yaml durability check

The operator panel writes `config/keys.yaml` on every Generate /
Revoke under the same mutex that guards the in-memory registry; the
file write is atomic (tempfile + rename). Verify both flows persist:

```bash
# dev mode — the host's ./config/keys.yaml IS what loadtest reads
head config/keys.yaml

# full mode — bind-mounted from the host; same file, same content
head config/keys.yaml      # should match what the page shows in /api/keys
```

A `Ctrl-C` on the host loadtest (dev mode) or `docker compose restart
loadtest` (full mode) restarts the process; on boot it re-loads
keys.yaml and the same keys are still there. In k8s the PVC at
`/config` plays the same role.

---

## 8. API edge cases (curl, one at a time)

These exercise the audit-fixed status codes plus the existing happy /
sad paths. Pre-load the key into a variable:

```bash
KEY_PRO=$(grep -A1 'tier: pro$' config/keys.yaml | grep '  key:' | head -1 | sed 's/.*"\(.*\)"/\1/')
KEY_FREE=$(grep -A1 'tier: free$' config/keys.yaml | grep '  key:' | head -1 | sed 's/.*"\(.*\)"/\1/')
echo "$KEY_PRO" "$KEY_FREE"
```

(On Windows PowerShell, open `config/keys.yaml` and copy the values manually.)

### 8a. Happy path — 202 + webhook

```bash
curl -i -X POST http://localhost:8080/shorten \
  -H "X-Api-Key: $KEY_PRO" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/manual-test-1","webhook_url":"http://localhost:8091/sink"}'
```

Expected: `HTTP/1.1 202 Accepted`, body has `job_id`. Within a second
or two the loadtest sink should record the delivery (visible in the
showcase page's "WH" column for that key).

### 8b. Bad key — 401

```bash
curl -i -X POST http://localhost:8080/shorten \
  -H "X-Api-Key: sl_live_bogus_xxxxxxxxxxxxxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/x","webhook_url":"http://localhost:8091/sink"}'
# expected: 401 + "missing or invalid API key"
```

### 8c. SSRF rejection — 422 (existing)

```bash
curl -i -X POST http://localhost:8080/shorten \
  -H "X-Api-Key: $KEY_PRO" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/x","webhook_url":"http://10.0.0.99:8080/sink"}'
# expected: 422 + "webhook URL failed SSRF validation"
```

### 8d. URL blocklist — 422 (audit fix COV-4)

This needs the api restarted with `URL_BLOCKLIST` set:

```bash
# stop the api (Ctrl-C in terminal C), then:
SSRF_ALLOWLIST=127.0.0.1,localhost,host.docker.internal \
URL_BLOCKLIST=blocked-domain.example \
make run-api
```

```bash
curl -i -X POST http://localhost:8080/shorten \
  -H "X-Api-Key: $KEY_PRO" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://blocked-domain.example/spam","webhook_url":"http://localhost:8091/sink"}'
# expected: 422 + "url domain is blocked"
# (pre-audit-fix this returned 400)
```

Restore the api without `URL_BLOCKLIST` before continuing.

### 8e. Rate limit — 429 + headers

The "free" tier defaults to 10 req/min. Burst more than that:

```bash
for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -X POST http://localhost:8080/shorten \
    -H "X-Api-Key: $KEY_FREE" \
    -H "Content-Type: application/json" \
    -d "{\"url\":\"https://example.com/burst-$i\",\"webhook_url\":\"http://localhost:8091/sink\"}"
done
# expected: first ~10 return 202, the rest return 429
```

Check headers on a 429:

```bash
curl -i -X POST http://localhost:8080/shorten \
  -H "X-Api-Key: $KEY_FREE" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/over","webhook_url":"http://localhost:8091/sink"}' \
  | grep -iE "X-RateLimit|Retry-After"
# expected: X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, Retry-After
```

### 8f. Custom slug conflict — 409

```bash
# first request reserves the slug
curl -s -X POST http://localhost:8080/shorten \
  -H "X-Api-Key: $KEY_PRO" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/a","custom_slug":"myslug","webhook_url":"http://localhost:8091/sink"}' | head

# second request with same slug
curl -i -X POST http://localhost:8080/shorten \
  -H "X-Api-Key: $KEY_PRO" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/b","custom_slug":"myslug","webhook_url":"http://localhost:8091/sink"}'
# expected: 409 + "custom slug already taken"
```

### 8g. Redirect

Take a slug from any successful response above and:

```bash
curl -i http://localhost:8080/myslug
# expected: 302 Location: https://example.com/a
```

---

## 9. Observability checks

### 9a. Prometheus targets

Open <http://localhost:9091/targets>. All three targets
(`host.docker.internal:8080/8081/9090`) should be **UP**. If any are
DOWN, the host-firewall or the relevant binary isn't running.

Quick metric query:

```bash
curl -s 'http://localhost:9091/api/v1/query?query=shortlink_jobs_total' | head
```

### 9b. Grafana dashboards

Open <http://localhost:3000>. You're auto-logged-in as Viewer (anon).
**Browse → ShortLink** folder:

- `Jobs · Error rate` — should show jobs/sec by status + a 5-minute error rate stat
- `QR latency · Queue depth` — QR p50/p99 + queue depth timeseries

If they're empty, the loadtest attack hasn't run yet — start one
(`make loadtest`) and the dashboards populate within a few seconds.

### 9c. Observer event stream

Watch raw events from the observer:

```bash
curl -sN http://localhost:9090/stream    # this is HTTP; for WS use wscat
```

(The `/stream` endpoint is WebSocket — `curl` will hang. The showcase
page in §7 is the intended viewer.)

---

## 10. Run the integration test

This is the single biggest "did anything regress?" check. It spins up
its own Postgres + Redis + MinIO via testcontainers, builds api+worker
fresh, and asserts the happy path + three failure modes end to end:

```bash
make test-integration
# expected: PASS in ~8s after first run (first run pulls container images)
```

The four subtests:

- `TestShortenHappyPath` — verifies HMAC, payload shape, signed-URL PNG
- `TestShortenRejectsBadAPIKey` — 401
- `TestShortenRateLimited` — burst → 429 with all the headers
- `TestShortenRejectsSSRFBlockedWebhook` — 422 on RFC1918

If you only have time for one test, this is the one.

---

## 11. (Optional) Kubernetes walkthrough

Skip this section unless `kind` and `helm` are installed:

```bash
kind version
helm version
```

### 11a. Bring up the cluster + release

```bash
make k8s-up                                                # creates kind cluster + helm install
# NOTE: the chart now requires observerIngestToken. If install fails with
#   "secrets.observerIngestToken must be set..." that's audit fix S3 working
#   as intended; pass it through:
helm upgrade --install shortlink deploy/k8s \
  --set image.tag=dev \
  --set secrets.observerIngestToken=$(openssl rand -hex 16) \
  --wait
```

Verify the migration Job ran:

```bash
kubectl get jobs
# expected: shortlink-migrate-1 (or -<revision>) succeeded
kubectl logs job/shortlink-migrate-1
```

### 11b. S1 verification — PSS securityContext on pods

```bash
kubectl get deploy -o yaml | grep -A8 securityContext | head -40
# expected (for api/worker/observer):
#   runAsNonRoot: true
#   runAsUser: 65532
#   capabilities: drop: [ALL]
#   readOnlyRootFilesystem: true
#   seccompProfile: type: RuntimeDefault
```

### 11c. S2 verification — NetworkPolicy

```bash
kubectl get networkpolicy
kubectl get networkpolicy <release>-worker-egress -o yaml | grep -A5 ipBlock
# expected: cidr 0.0.0.0/0 with except: 10.0.0.0/8, 172.16.0.0/12,
#           192.168.0.0/16, 169.254.0.0/16, 127.0.0.0/8
```

(NetworkPolicy enforcement needs Calico or Cilium — kind's default
kindnet does **not** enforce. See `deploy/k8s/README.md` for installing
Calico on the kind cluster.)

### 11d. make k8s-status

```bash
make k8s-status
# expected: Deployments (api, worker, observer, pgbouncer) + their Pods all
#           Ready, ScaledObject for worker, NetworkPolicy
```

---

## 12. Audit-fix specific spot checks

Items covered by other sections are noted with the back-reference.

| Audit ID | Manual verification                                                            |
|----------|--------------------------------------------------------------------------------|
| B1       | Stop Redis briefly with `docker compose pause redis`, POST /shorten, resume. The shorten job should retry not silently lose the webhook |
| B2       | Open showcase, repeatedly refresh — pre-fix would panic on closed channel under load |
| B3       | §10 integration test exercises the fixed sink-channel race                     |
| COV-4    | §8d above                                                                      |
| COV-5    | `curl -sI -H "Origin: http://evil.example" http://localhost:9090/stream` should get 403 |
| S1       | §11b above                                                                     |
| COV-18   | Trigger by setting a webhook_url whose DNS resolves to RFC1918 mid-flight (hard manual; the integration test covers this via SSRF reject at submit) |
| S3       | §11a — install without `observerIngestToken` should fail                       |
| D5       | §10 integration test — confirms migrations.FS is what ships                    |
| P1+B4    | Restart worker, run `make loadtest`, query Prometheus: `histogram_quantile(0.99, rate(shortlink_qr_generate_duration_seconds_bucket[1m]))` — should reflect actual QR cost only, not collide-retry waste |
| B5       | Wait > `QR_OBJECT_TTL` (15m default) after a successful job, the sweeper nulls the column before deleting MinIO object — worker can't Stat a missing key |
| B6       | §7 above (two-tab reset test)                                                  |
| B8       | `docker compose pause minio`, trigger a webhook, observer should show `webhook_failed` with retry, not silent size_bytes=0 |
| S2       | §11c above                                                                     |
| P5       | Hard to observe without a profiler; `go test -run=- -bench=. -benchmem` against `internal/observer` would show no per-tick allocation |
| S4       | §7 above                                                                       |
| B9       | Send SIGTERM to the worker (`kill <pid>` from the terminal). Logs should include `health server shutdown` if it errors during the 5s grace window |
| B7       | Pause the api during a loadtest, the `Err` column in the summary should reflect transport failures |
| D1       | Pure code cleanup; verified by `go build ./...`                                |
| D9       | Click "Clear logs" in the showcase, observer log ring should fully empty       |
| D6       | `make images` should build exactly 5 images: api / worker / observer / migrate / loadtest (loadtest joined post-M9 when it became part of the chart). `keygen` stays CLI-only |
| D7       | `go vet -tags integration ./tests/...` should report no unused imports         |
| COV-3    | SPEC-only — read §5 in `docs/SPEC.md`                                          |
| COV-1/2/14 | `request_completed` shows only on POST /shorten — confirm in the log audit panel that GET /healthz doesn't generate one |
| COV-6/7/8/9/10/11/12/15/17/19 | SPEC-only — read `docs/SPEC.md`                  |

---

## 13. Teardown

Dev mode:

```bash
# stop the host binaries: Ctrl-C in each terminal
make dev-down                       # tears the docker compose stack down + wipes volumes
```

Full mode:

```bash
make full-down                      # tears all containers down, PRESERVES volumes
make dev-down                       # same as above but ALSO wipes volumes (clean slate)
```

K8s (only if you ran §11):

```bash
make k8s-down                       # helm uninstall the release
kind delete cluster --name shortlink  # nuke the cluster entirely
```

Notes:

- `docker compose down -v` (under either of the dev-down paths above)
  wipes the volumes — Postgres data, MinIO objects. The teardown
  script also deletes `config/keys.yaml` when the volume is wiped,
  since the on-disk hashes would no longer match anything in the DB.
- Use `-KeepData -KeepKeys` flags on `scripts/local-teardown.ps1` if
  you want to preserve both the volumes AND the keys file (e.g. to
  restart the host binaries against existing data).
