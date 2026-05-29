SHELL := /bin/bash

# Defaults can be overridden via the environment, e.g.:
#   DATABASE_URL=... make migrate-up
DATABASE_URL ?= postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable
# Integration tests spin up their own Kafka + Postgres via testcontainers, so
# they need a working Docker daemon (set DOCKER_HOST if you use colima/podman).
KAFKA_BROKERS ?= localhost:9094
PORT ?= 8080

.PHONY: all
all: build ## Default target — build the binary.

.PHONY: clean
clean: ## Remove build artifacts and coverage output.
	rm -rf bin/ coverage/

.PHONY: build
build: ## Build the single wallet binary (all roles, selected by --mode).
	go build -trimpath -o bin/wallet ./cmd/wallet

.PHONY: run-all
run-all: ## Run every role in one process (local dev). Requires Kafka + Postgres.
	DATABASE_URL=$(DATABASE_URL) KAFKA_BROKERS=$(KAFKA_BROKERS) PORT=$(PORT) go run ./cmd/wallet --mode=all

.PHONY: run-gateway
run-gateway: ## Run only the HTTP gateway.
	DATABASE_URL=$(DATABASE_URL) KAFKA_BROKERS=$(KAFKA_BROKERS) PORT=$(PORT) go run ./cmd/wallet --mode=gateway

.PHONY: run-command-processor
run-command-processor: ## Run only the write-side state machine.
	KAFKA_BROKERS=$(KAFKA_BROKERS) go run ./cmd/wallet --mode=command-processor

.PHONY: run-projector
run-projector: ## Run only the CQRS read-side projector.
	DATABASE_URL=$(DATABASE_URL) KAFKA_BROKERS=$(KAFKA_BROKERS) go run ./cmd/wallet --mode=projector

.PHONY: run-saga
run-saga: ## Run only the Saga coordinator.
	DATABASE_URL=$(DATABASE_URL) KAFKA_BROKERS=$(KAFKA_BROKERS) go run ./cmd/wallet --mode=saga

.PHONY: test
test: ## Run unit tests with the race detector and coverage.
	go test -race -count=1 -cover ./...

.PHONY: test-int
test-int: ## Run integration tests (testcontainers Kafka + Postgres; needs Docker).
	go test -count=1 -tags=integration -timeout 900s ./internal/wallet/test/integration/...

.PHONY: test-all
test-all: test test-int ## Run unit + integration tests.

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format the code.
	gofmt -s -w .

.PHONY: vet
vet: ## go vet.
	go vet ./...

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations.
	migrate -path migration -database "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the last migration.
	migrate -path migration -database "$(DATABASE_URL)" down 1

.PHONY: compose-up
compose-up: ## Start the full stack (Kafka + Postgres + migrate + 4 roles).
	docker compose up --build -d

.PHONY: compose-down
compose-down: ## Stop the stack and remove volumes.
	docker compose down -v

.PHONY: compose-seed
compose-seed: ## Seed two demo accounts via the running stack.
	docker compose run --rm --no-deps -e KAFKA_BROKERS=kafka:9092 gateway --mode=seed --account=alice --amount=100.00 --currency=USD
	docker compose run --rm --no-deps -e KAFKA_BROKERS=kafka:9092 gateway --mode=seed --account=bob --amount=10.00 --currency=USD

.PHONY: infra-up
infra-up: ## Start only Kafka + Postgres + migrate (for local `make run-*`).
	docker compose up -d postgres kafka migrate

.PHONY: seed
seed: ## Seed two demo accounts from the host (uses KAFKA_BROKERS).
	KAFKA_BROKERS=$(KAFKA_BROKERS) go run ./cmd/wallet --mode=seed --account=alice --amount=100.00 --currency=USD
	KAFKA_BROKERS=$(KAFKA_BROKERS) go run ./cmd/wallet --mode=seed --account=bob --amount=10.00 --currency=USD

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
