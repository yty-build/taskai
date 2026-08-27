#!/bin/sh
set -eu

binary=${1:-./bin/taskai}

fail() {
	printf 'acceptance: %s\n' "$1" >&2
	exit 1
}

[ -x "$binary" ] || fail "taskai binary is not executable"
case $(uname -s) in
	Darwin|Linux) ;;
	*) fail "compiled PTY acceptance requires Darwin or Linux" ;;
esac

binary_dir=$(CDPATH= cd -- "$(dirname "$binary")" && pwd)
binary_path="$binary_dir/$(basename "$binary")"

TASKAI_ACCEPTANCE_BINARY="$binary_path" \
	GOWORK=off go test -count=1 -tags=acceptance ./internal/testkit/ptyacceptance

printf '%s\n' 'acceptance: compiled PTY product flow passed'
