# ShortLink — developer tasks (SPEC §13). Targets grow as milestones land.
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: dev dev-down stack-up stack-logs migrate keys run-api run-worker run-observer loadtest build test sqlc tidy

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

test: ## run all tests
	go test ./...

sqlc: ## regenerate type-safe query code from internal/db/query
	sqlc generate

tidy: ## tidy and verify go module dependencies
	go mod tidy
