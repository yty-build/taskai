#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$repository_root"

printf 'Running deterministic Task/Session acceptance suite\n'
go test -count=1 ./...

printf 'Running concurrency and detached-worker acceptance suite\n'
go test -count=1 -race ./...

printf 'Running static analysis\n'
test -z "$(gofmt -l .)"
go vet ./...

printf 'Verifying injected build metadata\n'
make --no-print-directory build VERSION=acceptance COMMIT=acceptance-sha BUILD_DATE=2000-01-01T00:00:00Z
actual_version=$(bin/acpctl version)
expected_version='acpctl acceptance (commit acceptance-sha, built 2000-01-01T00:00:00Z)'
if [ "$actual_version" != "$expected_version" ]; then
  printf 'version output mismatch: %s\n' "$actual_version" >&2
  exit 1
fi

printf 'Checking tracked files, ignore rules, fixtures, and entity boundary\n'
./scripts/check-repository.sh

printf 'Deterministic V1 acceptance passed\n'
