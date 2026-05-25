# Manual Testing Guide

A runnable walkthrough that exercises every feature shipped through M1–M9
plus the post-M9 audit fixes. Work top-to-bottom or jump to a specific
section — each block notes what it depends on.

## 0. Prerequisites

- Docker Desktop (Windows/Mac) or Docker Engine (Linux), running
- Go **1.26** on PATH (`go version`)
- `make`, `curl`, `git` available
- Four terminals minimum (or use a tmux/zellij/Windows Terminal tab set):
  one each for **api**, **worker**, **observer**, and **loadtest**, plus
  one free for `curl` / inspection
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

The runner does double duty as your webhook sink. Use a long duration so
it stays up for manual curling.

```bash
# terminal D — pick one of:
make loadtest                                                # default 60s attack
go run ./cmd/loadtest --duration=10m --grafana=http://localhost:3000
```

What this gives you:

- **Showcase page** at <http://localhost:8090> (auto-opens? no — open it manually)
- **Webhook sink** at `http://localhost:8091/sink` — POST deliveries land here,
  HMAC is verified, the count is reflected in the page's "WH" column
- An attack starts immediately against the api using `config/keys.yaml`

---

## 7. Showcase page tour (M6)

Open <http://localhost:8090> in your browser. You should see:

- **Top bar** — green `●` "Connected" indicator, "Reset stats" button
- **API key metrics panel** — one row per key from `config/keys.yaml`,
  with live counters. Rows where `429/total > 0.5` go red.
- **Monitoring panel** — two Grafana iframes (`jobs/sec · error rate`
  and `QR p99 · queue depth`). If they say "Grafana not configured",
  you forgot `--grafana=...` in step 6.
- **Log audit panel** — newest-first stream of events, per-entry TTL
  badges, source/level/key filters

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
| D6       | `make images` should build exactly 4 images (api/worker/observer/migrate), not 6 |
| D7       | `go vet -tags integration ./tests/...` should report no unused imports         |
| COV-3    | SPEC-only — read §5 in `docs/SPEC.md`                                          |
| COV-1/2/14 | `request_completed` shows only on POST /shorten — confirm in the log audit panel that GET /healthz doesn't generate one |
| COV-6/7/8/9/10/11/12/15/17/19 | SPEC-only — read `docs/SPEC.md`                  |

---

## 13. Teardown

```bash
# stop the host binaries: Ctrl-C in each terminal
make dev-down                       # tears the docker compose stack down
make k8s-down                       # (if you ran §11)
kind delete cluster --name shortlink  # nuke the cluster entirely
```

`docker compose down -v` also wipes the volumes (Postgres data,
MinIO objects) — useful when you want a clean migrate run.
