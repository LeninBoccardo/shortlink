# ShortLink — developer tasks (SPEC §13).
# Milestone 1 targets; more are added as later milestones land.
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: dev dev-down migrate keys run-api build test sqlc tidy

dev: ## start local infrastructure (Postgres + MinIO)
	$(COMPOSE) up -d

dev-down: ## stop local infrastructure
	$(COMPOSE) down

migrate: ## apply database migrations
	go run ./cmd/migrate up

keys: ## generate test API keys + webhook secrets into config/keys.yaml
	go run ./cmd/keygen

run-api: ## run the API gateway (in-process queue + worker pool)
	go run ./cmd/api

build: ## build all binaries into ./bin
	go build -o bin/ ./cmd/...

test: ## run all tests
	go test ./...

sqlc: ## regenerate type-safe query code from internal/db/query
	sqlc generate

tidy: ## tidy and verify go module dependencies
	go mod tidy
