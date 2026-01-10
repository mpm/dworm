.PHONY: build build-host build-endpoint clean test test-unit test-e2e test-race test-cover fmt tidy

GO := /home/malte/.local/share/mise/installs/go/1.25.5/bin/go

build: build-host build-endpoint

build-host:
	$(GO) build -o bin/dworm ./cmd/dworm

build-endpoint:
	GOOS=linux GOARCH=amd64 $(GO) build -o bin/dworm_endpoint ./cmd/dworm_endpoint

clean:
	rm -rf bin/

# Run all tests (unit + e2e)
test: test-unit test-e2e

# Unit tests only (fast, no Docker required)
test-unit:
	$(GO) test -v ./...

# E2E tests (requires Docker, skips gracefully if unavailable)
test-e2e: build
	./test/e2e/run-e2e.sh

# Run tests with race detector
test-race:
	$(GO) test -race ./...

# Generate test coverage report
test-cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy
