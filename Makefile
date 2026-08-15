# Every build must be distinguishable from every other, or you cannot tell which
# agents are current. git describe gives a tag when one exists and a commit SHA
# otherwise; the build timestamp disambiguates repeated builds of the same
# commit, which is the normal case during development.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-untracked)
BUILDTIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DIST      := dist
# The database lives outside DIST so `make clean` cannot destroy it. Losing
# every device's identity to a routine clean is not a recoverable mistake:
# each agent would have to be reinstalled.
DATA      := data

# CGO is off everywhere: it is what makes both binaries statically linked and
# lets every target cross-compile from one machine with no C toolchain.
GO      := CGO_ENABLED=0 go
LDFLAGS_SERVER := -s -w \
	-X github.com/ollie/monitorrr/internal/server.Version=$(VERSION) \
	-X github.com/ollie/monitorrr/internal/server.BuildTime=$(BUILDTIME)
LDFLAGS_AGENT  := -s -w \
	-X github.com/ollie/monitorrr/internal/agent.Version=$(VERSION) \
	-X github.com/ollie/monitorrr/internal/agent.BuildTime=$(BUILDTIME)

# os/arch pairs built by `make build-all`.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build server agent build-all run clean fmt vet test tidy help

all: build

## build: build server and agent for this machine
build: server agent

server:
	@mkdir -p $(DIST)
	$(GO) build -ldflags "$(LDFLAGS_SERVER)" -o $(DIST)/monitorrr-server ./cmd/server

agent:
	@mkdir -p $(DIST)
	$(GO) build -ldflags "$(LDFLAGS_AGENT)" -o $(DIST)/monitorrr-agent ./cmd/agent

## build-all: cross-compile agents for every supported platform
# Depends on `build` (not just `server`) so the host-architecture agent is
# rebuilt too — otherwise local testing silently runs a stale binary.
build-all: build
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="$(DIST)/monitorrr-agent-$$os-$$arch$$ext"; \
		echo "  building $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO) build -ldflags "$(LDFLAGS_AGENT)" -o "$$out" ./cmd/agent || exit 1; \
	done
	@echo "agents built in $(DIST)/ — they appear on the Deployment page"

## run: build and start the server on :8080
run: build
	@mkdir -p $(DATA)
	./$(DIST)/monitorrr-server -addr :8080 -db $(DATA)/monitorrr.db -dist $(DIST)

## fmt, vet, test: standard checks
fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy

## clean: remove build output. The database in $(DATA) is deliberately kept.
clean:
	rm -rf $(DIST)
	@echo "removed $(DIST)/ — kept $(DATA)/ (database)"

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
