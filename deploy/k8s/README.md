# ShortLink on Kubernetes

The chart in this directory deploys ShortLink (api + worker + observer +
PgBouncer + migrate Job) into any Kubernetes cluster with a CNI that
enforces NetworkPolicy. The default workflow uses [kind](https://kind.sigs.k8s.io/)
for a local demo cluster; ten lines of `kubectl apply` change between
"kind" and a real cluster.

## Prerequisites

Install these once:

- **Docker** — builds the images locally.
- **kubectl** — `kubectl version --client` should report v1.28+.
- **helm** — v3.12+.
- **kind** — `kind version` should report v0.20+.

Two cluster add-ons land on a fresh `kind` cluster:

- **Calico** for NetworkPolicy enforcement (kind's default `kindnet` does
  not enforce, so the SSRF egress policy becomes a no-op without it).
- **KEDA** for worker autoscaling on asynq queue depth.

## Bring it up

```sh
make k8s-up          # builds images, kind-loads them, helm-installs the chart
```

This runs `kind create cluster`, `make images`, `kind load docker-image`,
and `helm upgrade --install --wait`. On first run it takes ~2 minutes; on
subsequent runs it reuses the cluster and just rolls the new images.

**Calico** (NetworkPolicy enforcement) needs a one-time install in the kind
cluster. Without it the SSRF egress NetworkPolicy is a no-op:

```sh
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.27.0/manifests/calico.yaml
kubectl -n kube-system rollout status deploy/calico-kube-controllers
```

**KEDA** (queue-depth autoscaling for the worker):

```sh
helm repo add kedacore https://kedacore.github.io/charts
helm install keda kedacore/keda --namespace keda --create-namespace
```

You'll also need Postgres + Redis + MinIO somewhere reachable by name. For
the demo, install community Helm charts (Bitnami / minio.io); the
`postgres.host`, `redis.url`, `minio.endpoint` values in `values.yaml`
already use the conventional Service names.

## Day-to-day

```sh
make k8s-logs        # tail logs across api + worker + observer
make k8s-status      # deploy/pod/svc/job/scaledobject/networkpolicy summary
make k8s-down        # uninstall the release (cluster stays up)
```

## Before exposing the API publicly

The chart's default `values.yaml` ships **production-safe** for SSRF
(`config.ssrfAllowlist: ""`) and **demo-permissive** for the open-redirect
surface (`config.urlBlocklist: ""`). The latter is fine for an internal demo
on a private network — but anyone with any tier key can shorten arbitrary
`http(s)` URLs, so a public deployment turns your domain into a redirect-
laundering surface for phishing.

Before the API is reachable from the internet, curate
`config.urlBlocklist` (comma-separated domains), or wire a reputation
service such as Google Safe Browsing at the API layer. See SPEC §9 *URL
validation* for the rationale.

## TLS on the api→observer ingest hop

Event POSTs from api/worker to the observer carry `api_key_hash`
(SHA-256 of the customer's raw key — the same value stored as the
credential record). `OBSERVER_INGEST_TOKEN` authenticates the emitter
but doesn't encrypt the wire. Run this hop over TLS in production —
either a service mesh (Linkerd/Istio mTLS) or an in-cluster HTTPS
Service. Without it, a passive in-cluster sniffer can collect hashes
that become useful whenever DB access is later compromised. The chart
itself doesn't terminate TLS; that's the cluster operator's call.
See SPEC §10 *Event envelope* for the rationale.

## Demos

To exercise the rollout demo for SPEC §16 (graceful shutdown):

```sh
kubectl rollout restart deploy/shortlink-shortlink-api
kubectl rollout status deploy/shortlink-shortlink-api
# In another terminal:
kubectl logs -l app.kubernetes.io/component=api -f
```

You should see SIGTERM logged, the server drain in-flight requests, and the
pod terminate before the 30s `terminationGracePeriodSeconds` is up.

## Troubleshooting

**Migrate Job hangs** — `kubectl describe job/shortlink-shortlink-migrate-1`.
Most likely Postgres isn't reachable on `postgres:5432` (the migration goes
direct, not through PgBouncer). Verify the Postgres Service exists and is
ready.

**Worker pods stuck `CrashLoopBackoff`** — `kubectl logs` will show the
asynq connection error. Redis is the usual culprit; verify `redis.url` in
values matches your in-cluster Service.

**KEDA never scales** — check `kubectl describe scaledobject shortlink-shortlink-worker`.
The Redis trigger needs the list `asynq:{shorten}:pending` to exist; it
only appears after the first /shorten request, so don't expect KEDA to
react to an empty queue.

**NetworkPolicy blocks legitimate traffic** — if Calico isn't installed the
policy is a no-op; if it is, your in-cluster Postgres/Redis/MinIO need
labels matching the egress rules in `templates/networkpolicy.yaml` (the
defaults expect `app: postgres / redis / minio`).
