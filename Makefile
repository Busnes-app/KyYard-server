.PHONY: all build build-web test test-race test-postgres test-web tidy-check lint smoke ci run clean docker-build

all: build-web build

build-web:
	@echo "==> Building frontend web assets..."
	@cd web && npm ci && npm run build

build:
	@echo "==> Compiling ky_server_base binary..."
	@go build -o ky_server_base ./cmd/server

test:
	@echo "==> Running test suite..."
	@go test -v ./...

test-race:
	@echo "==> Running test suite with race detector..."
	@go test -race -covermode=atomic -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

# Runs the same suite against PostgreSQL; needs a reachable server.
test-postgres:
	@echo "==> Running test suite against PostgreSQL..."
	@KY_TEST_POSTGRES_DSN="$${KY_TEST_POSTGRES_DSN:-postgres://postgres:postgrespassword@127.0.0.1:5432/ky_server?sslmode=disable}" go test -count=1 ./...

test-web:
	@echo "==> Running frontend test suite..."
	@cd web && npm ci && npm test

# The same gate CI runs: a stale go.sum fails the build there, so fail here first.
tidy-check:
	@echo "==> Checking go.mod is tidy..."
	@go mod tidy
	@git diff --exit-code go.mod go.sum

lint:
	@echo "==> Checking formatting and vet..."
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || { echo "gofmt needed:"; gofmt -l $$(git ls-files '*.go'); exit 1; }
	@go vet ./...

smoke: build
	@./scripts/smoke-test.sh

ci: tidy-check lint test-race test-web smoke
	@echo "==> Local CI checks passed"

run: build
	@./ky_server_base

docker-build:
	@docker build -t ky_server_base .

clean:
	@rm -rf ky_server_base web/dist web/node_modules data/ backups/ coverage.out
