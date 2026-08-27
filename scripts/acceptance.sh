#!/bin/sh
set -eu

binary=${1:-./bin/taskai}

fail() {
	printf 'acceptance: %s\n' "$1" >&2
	exit 1
}

[ -x "$binary" ] || fail "taskai binary is not executable"

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/taskai-m01.XXXXXX")
trap 'find "$temporary_root" -depth -delete 2>/dev/null || true' EXIT HUP INT TERM
state_dir="$temporary_root/state-must-remain-absent"

expected_help='Usage:
  taskai --help
  taskai --version

Options:
  -h, --help  Show this help.
  --version   Show the TaskAI version.'

help_output=$(TASKAI_STATE_DIR="$state_dir" "$binary" --help)
[ "$help_output" = "$expected_help" ] || fail "help output changed"

version_output=$(TASKAI_STATE_DIR="$state_dir" "$binary" --version)
[ "$version_output" = 'taskai dev' ] || fail "version output changed"

stdout_file="$temporary_root/stdout"
stderr_file="$temporary_root/stderr"

set +e
TASKAI_STATE_DIR="$state_dir" "$binary" >"$stdout_file" 2>"$stderr_file"
status=$?
set -e
[ "$status" -eq 1 ] || fail "no-argument status must be 1"
[ ! -s "$stdout_file" ] || fail "no-argument failure wrote stdout"
[ "$(cat "$stderr_file")" = 'taskai: interactive workspace is not implemented in this baseline' ] || fail "no-argument error changed"

set +e
TASKAI_STATE_DIR="$state_dir" "$binary" --unknown >"$stdout_file" 2>"$stderr_file"
status=$?
set -e
[ "$status" -eq 2 ] || fail "invalid-argument status must be 2"
[ ! -s "$stdout_file" ] || fail "invalid argument wrote stdout"
[ "$(cat "$stderr_file")" = 'taskai: invalid arguments; use --help' ] || fail "invalid-argument error changed"

[ ! -e "$state_dir" ] || fail "M01 informational commands created TaskAI state"

for built_file in bin/*; do
	[ -e "$built_file" ] || continue
	[ "$built_file" = "bin/taskai" ] || fail "a command other than taskai was built"
done

printf '%s\n' 'M01 scaffold acceptance only; no product E2E capability claimed'
