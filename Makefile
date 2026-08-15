BINARY := bin/agent
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/yty-build/acp-gateway/internal/buildinfo.Version=$(VERSION) \
	-X github.com/yty-build/acp-gateway/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/yty-build/acp-gateway/internal/buildinfo.Date=$(BUILD_DATE)

.PHONY: all build test race vet fmt fmt-check clean

all: test build

build:
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/agent

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

clean:
	rm -f $(BINARY)
