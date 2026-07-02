.PHONY: build test acceptance integration browser install-test clean install fmt lint

GO := CGO_ENABLED=0 go
BINARY := shiftlog
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X github.com/re-cinq/shift-log/cmd.version=$(VERSION)"

# Lint runs before build and test to catch issues early
lint:
	golangci-lint run

build: lint
	$(GO) build $(LDFLAGS) -o $(BINARY) .

test: lint
	$(GO) test ./internal/...

acceptance: build
	$(GO) test ./tests/acceptance/... -v

browser: build
	$(GO) test ./tests/browser/... -v -timeout 60s

# Integration test with real Claude Code CLI
# Requires: ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN environment variable
# Requires: Claude Code CLI in PATH
integration: build
	SHIFTLOG_BINARY=$(PWD)/$(BINARY) $(GO) test ./tests/integration/... -v -timeout 120s

install-test:
	$(GO) test ./tests/install/... -v -timeout 120s

all: test acceptance

clean:
	rm -f $(BINARY)

install: build
	cp $(BINARY) $(GOPATH)/bin/

fmt:
	go fmt ./...
