.PHONY: build test proto proto-lint proto-py proto-ts migrate-up clients fmt

GO=go
GOFLAGS=-trimpath -ldflags="-s -w"

build:
	$(GO) build $(GOFLAGS) -o bin/ktms-server ./cmd/server
	$(GO) build $(GOFLAGS) -o bin/ktms-node ./cmd/node-agent
	$(GO) build $(GOFLAGS) -o bin/ktms-node-tray ./cmd/node-tray

test:
	$(GO) test -race ./...

proto:
	buf generate

proto-py:
	buf generate --template buf.gen.python.yaml

proto-ts:
	buf generate --template buf.gen.ts.yaml

proto-lint:
	buf lint

# Regenerate vendored stubs of services this one consumes.
clients:
	buf generate usage-proto --template buf.gen.usage.yaml

fmt:
	$(GO) fmt ./...
