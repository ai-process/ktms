.PHONY: build test proto proto-lint migrate-up clients fmt

GO=go
GOFLAGS=-trimpath -ldflags="-s -w"

build:
	$(GO) build $(GOFLAGS) -o bin/llm-proxy-server ./cmd/server
	$(GO) build $(GOFLAGS) -o bin/llm-proxy-node ./cmd/node-agent

test:
	$(GO) test -race ./...

proto:
	buf generate

proto-lint:
	buf lint

# Regenerate vendored stubs of services this one consumes.
clients:
	buf generate usage-proto --template buf.gen.usage.yaml

fmt:
	$(GO) fmt ./...
