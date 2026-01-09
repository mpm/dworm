.PHONY: build build-host build-endpoint clean test

GO := /home/malte/.local/share/mise/installs/go/1.25.5/bin/go

build: build-host build-endpoint

build-host:
	$(GO) build -o bin/dworm ./cmd/dworm

build-endpoint:
	GOOS=linux GOARCH=amd64 $(GO) build -o bin/dworm_endpoint ./cmd/dworm_endpoint

clean:
	rm -rf bin/

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy
