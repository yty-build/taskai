BINARY := bin/acpctl
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/yty-build/acp-gateway/internal/buildinfo.Version=$(VERSION) \
	-X github.com/yty-build/acp-gateway/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/yty-build/acp-gateway/internal/buildinfo.Date=$(BUILD_DATE)

.PHONY: all build test race vet fmt fmt-check security-check version-check acceptance verify clean

all: test build

build:
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/acpctl

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

security-check:
	./scripts/check-repository.sh

version-check:
	@$(MAKE) --no-print-directory build VERSION=acceptance COMMIT=acceptance-sha BUILD_DATE=2000-01-01T00:00:00Z
	@test "$$($(BINARY) version)" = "acpctl acceptance (commit acceptance-sha, built 2000-01-01T00:00:00Z)"

acceptance:
	./scripts/acceptance.sh

verify: fmt-check test race vet version-check security-check

clean:
	rm -f $(BINARY)
