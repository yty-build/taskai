#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$repository_root"

fail() {
	printf 'security-check: %s\n' "$1" >&2
	exit 1
}

for command_dir in cmd/*; do
	[ -d "$command_dir" ] || continue
	[ "$command_dir" = "cmd/taskai" ] || fail "unexpected command directory: $command_dir"
done
[ ! -e cmd/acpctl ] || fail "legacy acpctl command is present"

for forbidden_dir in \
	internal/api internal/broker internal/mailbox internal/mcp internal/presentation \
	internal/render internal/sessionlog internal/tmux internal/transcript internal/web \
	internal/worker internal/workflow; do
	[ ! -e "$forbidden_dir" ] || fail "forbidden production package is present: $forbidden_dir"
done

module_count=$(go list -m all | wc -l | tr -d ' ')
[ "$module_count" -eq 1 ] || fail "M01 must remain stdlib-only"

if rg -n '"(net/http|net/rpc|github\.com/.*/mcp|nhooyr\.io/websocket|github\.com/gorilla/websocket)"' cmd internal --glob '*.go' >/dev/null; then
	fail "forbidden server, MCP, or WebSocket import is present"
fi

for responsibility in acp connector replay manager runtime settings store task session tui testkit; do
	directory="internal/$responsibility"
	[ -f "$directory/doc.go" ] || fail "missing package documentation: $directory/doc.go"
	extra=$(find "$directory" -type f ! -name doc.go -print)
	[ -z "$extra" ] || fail "M01 responsibility package contains implementation: $directory"
done

hash_file() {
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		sha256sum "$1" | awk '{print $1}'
	fi
}

hash_stdin() {
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 | awk '{print $1}'
	else
		sha256sum | awk '{print $1}'
	fi
}

vision_hash=$(hash_file docs/UX_PRODUCT_VISION.md)
embedded_hash=$(awk '/EMBED_START/{inside=1;next}/EMBED_END/{inside=0}inside' build_plan.md | hash_stdin)
recorded_hash=$(awk '/^\*\*Embedded vision SHA-256:\*\*/ {print $4; exit}' build_plan.md)
[ "$vision_hash" = "$embedded_hash" ] || fail "embedded product vision does not match its source"
[ -n "$recorded_hash" ] || fail "build plan does not record the embedded product vision hash"
[ "$vision_hash" = "$recorded_hash" ] || fail "recorded product vision hash does not match its source"

if git ls-files | rg '(^|/)(acpctl|taskai)(\.test)?$|(^|/)bin/|\.(out|prof)$' >/dev/null; then
	fail "tracked binary or test artifact is present"
fi

printf '%s\n' 'security-check: repository boundaries verified for M01'
