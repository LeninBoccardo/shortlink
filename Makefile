# ShortLink — developer tasks (SPEC §13). Targets grow as milestones land.
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: dev dev-down migrate keys run-api run-worker build test sqlc tidy

dev: ## start local infrastructure (Postgres + MinIO + Redis)
	$(COMPOSE) up -d

dev-down: ## stop local infrastructure
	$(COMPOSE) down

migrate: ## apply database migrations
	go run ./cmd/migrate up

keys: ## generate test API keys + webhook secrets into config/keys.yaml
	go run ./cmd/keygen

run-api: ## run the API gateway
	go run ./cmd/api

run-worker: ## run the worker (asynq consumer + sweeper)
	go run ./cmd/worker

build: ## build all binaries into ./bin
	go build -o bin/ ./cmd/...

test: ## run all tests
	go test ./...

sqlc: ## regenerate type-safe query code from internal/db/query
	sqlc generate

tidy: ## tidy and verify go module dependencies
	go mod tidy
