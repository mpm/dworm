.PHONY: build build-host build-endpoint clean test test-unit test-e2e test-race test-cover fmt tidy

GO ?= go

# Version info - auto-detected from git
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# ldflags for version injection
LDFLAGS := -X github.com/mpm/dworm/internal/version.Version=$(VERSION) \
           -X github.com/mpm/dworm/internal/version.Commit=$(COMMIT) \
           -X github.com/mpm/dworm/internal/version.Date=$(DATE)

build: build-host build-endpoint

build-host: prepare-endpoints
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/dworm ./cmd/dworm

build-endpoint:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o bin/dworm_endpoint ./cmd/dworm_endpoint

.PHONY: prepare-endpoints
prepare-endpoints:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "-s -w" -o internal/host/endpointbundle/assets/linux_amd64/dworm_endpoint ./cmd/dworm_endpoint
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -ldflags "-s -w" -o internal/host/endpointbundle/assets/linux_arm64/dworm_endpoint ./cmd/dworm_endpoint

clean:
	rm -rf bin/ internal/host/endpointbundle/assets/

# Run all tests (unit + e2e)
test: test-unit test-e2e

# Unit tests only (fast, no Docker required)
test-unit: prepare-endpoints
	$(GO) test -v ./...

# E2E tests (requires Docker, skips gracefully if unavailable)
test-e2e: build
	./test/e2e/run-e2e.sh

# Run tests with race detector
test-race: prepare-endpoints
	$(GO) test -race ./...

# Generate test coverage report
test-cover: prepare-endpoints
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy
