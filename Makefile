SHELL := /bin/sh

GO ?= go
VERSION ?= dev
BINARY := bin/taskai
MODULE := github.com/yty-build/acp-gateway

.PHONY: fmt fmt-check test race vet build security-check acceptance clean

fmt:
	@files="$$(find cmd internal -type f -name '*.go' -print)"; \
	if [ -n "$$files" ]; then gofmt -w $$files; fi

fmt-check:
	@files="$$(find cmd internal -type f -name '*.go' -print)"; \
	unformatted="$$(gofmt -l $$files)"; \
	if [ -n "$$unformatted" ]; then printf '%s\n' "$$unformatted"; exit 1; fi

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "-X $(MODULE)/internal/buildinfo.Version=$(VERSION)" -o $(BINARY) ./cmd/taskai

security-check:
	./scripts/check-repository.sh

acceptance: build
	./scripts/acceptance.sh ./$(BINARY)

clean:
	@find bin -mindepth 1 -maxdepth 1 -type f -name 'taskai' -delete 2>/dev/null || true
