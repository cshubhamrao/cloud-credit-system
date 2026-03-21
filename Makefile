.PHONY: all build test run run-worker docker-up docker-down proto-gen sqlc-gen generate setup simulate clean lint

MODULE        := github.com/cshubhamrao/cloud-credit-system
SERVER_BIN    := bin/server
WORKER_BIN    := bin/worker
SIMULATOR_BIN := bin/simulator

all: generate build

# ─── Code Generation ────────────────────────────────────────────────────────

TOOLS_DIR := .tools

proto-gen: $(TOOLS_DIR)/protoc-gen-go $(TOOLS_DIR)/protoc-gen-connect-go
	PATH="$(PWD)/$(TOOLS_DIR):$$PATH" go tool buf generate

$(TOOLS_DIR)/protoc-gen-go:
	@mkdir -p $(TOOLS_DIR)
	go build -o $@ google.golang.org/protobuf/cmd/protoc-gen-go

$(TOOLS_DIR)/protoc-gen-connect-go:
	@mkdir -p $(TOOLS_DIR)
	go build -o $@ connectrpc.com/connect/cmd/protoc-gen-connect-go

sqlc-gen:
	go tool sqlc generate -f sql/sqlc.yaml

generate: proto-gen sqlc-gen

# ─── Build ───────────────────────────────────────────────────────────────────

build: build-server build-worker build-simulator

build-server:
	@mkdir -p bin
	go build -o $(SERVER_BIN) ./cmd/server

build-worker:
	@mkdir -p bin
	go build -o $(WORKER_BIN) ./cmd/worker

build-simulator:
	@mkdir -p bin
	go build -o $(SIMULATOR_BIN) ./cmd/simulator

# ─── Run ─────────────────────────────────────────────────────────────────────

run: build-server
	./$(SERVER_BIN)

run-worker: build-worker
	./$(WORKER_BIN)

simulate: build-simulator
	./$(SIMULATOR_BIN)

# ─── Infrastructure ──────────────────────────────────────────────────────────

docker-up:
	@mkdir -p config/temporal
	@cp -n config/temporal/development-sql.yaml.example config/temporal/development-sql.yaml 2>/dev/null || true
	docker compose up -d
	@echo "Waiting for services to be ready..."
	@sleep 5
	@$(MAKE) db-migrate

docker-down:
	docker compose down -v

db-migrate:
	@echo "Running database migrations..."
	@for f in $$(ls sql/migrations/*.sql | sort); do \
		echo "  applying $$f..."; \
		PGPASSWORD=postgres psql -h localhost -U postgres -d creditdb -f $$f; \
	done
	@echo "Migrations complete."

# ─── Test ────────────────────────────────────────────────────────────────────

test: test-unit test-integration

test-unit:
	go test ./internal/... -v -timeout 60s

test-integration:
	go test ./test/... -v -timeout 120s

# ─── Demo ────────────────────────────────────────────────────────────────────

demo: docker-up build
	@echo "Starting demo..."
	./scripts/demo.sh

# ─── Tools ───────────────────────────────────────────────────────────────────

setup:
	@echo "Adding tool dependencies to go.mod..."
	go mod edit -tool=github.com/bufbuild/buf/cmd/buf
	go mod edit -tool=github.com/sqlc-dev/sqlc/cmd/sqlc
	go mod tidy
	@echo "Done. Run 'make generate' to regenerate code."

lint:
	go tool buf lint
	go vet ./...

clean:
	rm -rf bin/ gen/ .tools/
	find internal/db/sqlcgen -name '*.go' -delete 2>/dev/null || true

# ─── Helpers ─────────────────────────────────────────────────────────────────

.DEFAULT_GOAL := all
