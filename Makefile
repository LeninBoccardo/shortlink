# ShortLink — developer tasks (SPEC §13). Targets grow as milestones land.
COMPOSE := docker compose -f deploy/docker-compose.yml
COMPOSE_FULL := docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.full.yml

# k8s targets (M8). KIND_CLUSTER stays overridable from the env.
KIND_CLUSTER ?= shortlink
HELM_RELEASE ?= shortlink
HELM_NAMESPACE ?= default
IMAGE_TAG ?= dev

.PHONY: dev dev-down stack-up stack-logs migrate keys run-api run-worker run-observer loadtest build test test-integration sqlc tidy \
        full full-down full-logs \
        images kind-up kind-load k8s-up k8s-down k8s-logs k8s-status

dev: ## start local infrastructure (Postgres + MinIO + Redis + Prometheus + Grafana)
	$(COMPOSE) up -d

dev-down: ## stop local infrastructure
	$(COMPOSE) down

stack-up: ## start (or restart) just the M7 observability services
	$(COMPOSE) up -d prometheus grafana

stack-logs: ## tail Prometheus + Grafana logs
	$(COMPOSE) logs -f prometheus grafana

migrate: ## apply database migrations
	go run ./cmd/migrate up

keys: ## generate test API keys + webhook secrets into config/keys.yaml
	go run ./cmd/keygen

run-api: ## run the API gateway
	go run ./cmd/api

run-worker: ## run the worker (asynq consumer + sweeper)
	go run ./cmd/worker

run-observer: ## run the observer hub (events ingest + WebSocket broadcaster)
	go run ./cmd/observer

loadtest: ## run the multi-key vegeta attack (default --duration=60s)
	go run ./cmd/loadtest

build: ## build all binaries into ./bin
	go build -o bin/ ./cmd/...

test: ## run unit tests (skips integration; no Docker needed)
	go test ./...

test-integration: ## run end-to-end test against testcontainers (needs Docker)
	go test -tags integration -timeout 5m -v ./tests/...

sqlc: ## regenerate type-safe query code from internal/db/query
	sqlc generate

tidy: ## tidy and verify go module dependencies
	go mod tidy

# ---------------------------------------------------------------------------
# Kubernetes (M8). Assumes kind, kubectl, helm are on PATH. See
# deploy/k8s/README.md for the one-time cluster setup (Calico + KEDA).

images: ## build api/worker/observer/migrate/loadtest docker images locally
	docker build --build-arg BINARY=api      -t shortlink-api:$(IMAGE_TAG)      .
	docker build --build-arg BINARY=worker   -t shortlink-worker:$(IMAGE_TAG)   .
	docker build --build-arg BINARY=observer -t shortlink-observer:$(IMAGE_TAG) .
	docker build --build-arg BINARY=migrate  -t shortlink-migrate:$(IMAGE_TAG)  .
	docker build --build-arg BINARY=loadtest -t shortlink-loadtest:$(IMAGE_TAG) .

# ---------------------------------------------------------------------------
# Compose.full — one-command showcase mode (SPEC §13). Brings up the full
# stack INCLUDING api/worker/observer/loadtest as containers, so the user
# doesn't need a Go toolchain. `make dev` stays the iteration-friendly mode
# (infra only; binaries on the host).

full: ## bring up everything in containers (infra + api/worker/observer/loadtest)
	# Build images up front so the migrate step doesn't lag on a cold cache.
	# Profile gate keeps the migrate service out of the default `up`; we
	# run it in the foreground so api/worker only start once schema is at
	# head. `restart: no` (set in the base file) means it exits cleanly
	# after applying.
	$(COMPOSE_FULL) build
	$(COMPOSE) --profile migrate run --rm migrate
	$(COMPOSE_FULL) up -d

full-down: ## tear the full stack down (preserves named volumes)
	$(COMPOSE_FULL) down

full-logs: ## tail api + worker + observer + loadtest logs
	$(COMPOSE_FULL) logs -f api worker observer loadtest

kind-up: ## create the local kind cluster (idempotent)
	kind get clusters | grep -q "^$(KIND_CLUSTER)$$" || kind create cluster --name $(KIND_CLUSTER)

kind-load: images ## push the locally built images into the kind node
	kind load docker-image --name $(KIND_CLUSTER) \
	  shortlink-api:$(IMAGE_TAG) shortlink-worker:$(IMAGE_TAG) \
	  shortlink-observer:$(IMAGE_TAG) shortlink-migrate:$(IMAGE_TAG)

k8s-up: kind-up kind-load ## install/upgrade the Helm release into the kind cluster
	helm upgrade --install $(HELM_RELEASE) deploy/k8s \
	  --namespace $(HELM_NAMESPACE) --create-namespace \
	  --set image.tag=$(IMAGE_TAG) --wait --timeout 3m

k8s-down: ## uninstall the Helm release (keeps the cluster)
	helm uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

k8s-logs: ## tail logs from api + worker + observer pods
	kubectl -n $(HELM_NAMESPACE) logs -l app.kubernetes.io/instance=$(HELM_RELEASE) --all-containers --tail=50 -f

k8s-status: ## summarise rollout state across the chart's workloads
	kubectl -n $(HELM_NAMESPACE) get deploy,pod,svc,job,scaledobject,networkpolicy \
	  -l app.kubernetes.io/instance=$(HELM_RELEASE)
