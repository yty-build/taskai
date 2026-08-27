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

for internal_dir in internal/*; do
	[ -d "$internal_dir" ] || continue
	case "$internal_dir" in
		internal/acp|internal/app|internal/buildinfo|internal/connector|internal/manager|internal/replay|\
		internal/runtime|internal/session|internal/settings|internal/store|internal/task|internal/testkit|internal/tui)
			;;
		*) fail "unexpected internal package: $internal_dir" ;;
	esac
done

GOWORK=off go mod verify >/dev/null || fail "module checksums failed verification"
GOWORK=off go mod tidy -diff >/dev/null || fail "go.mod or go.sum is not tidy"

if GOWORK=off go list -m -f '{{.Path}}' all | rg -i '(^|/)(mcp|tmux|websocket|gorilla/websocket)(/|$)' >/dev/null; then
	fail "forbidden architecture dependency is present"
fi

if rg -n '"(net/http|net/rpc|github\.com/.*/mcp|nhooyr\.io/websocket|github\.com/gorilla/websocket)"' \
	cmd internal --glob '*.go' --glob '!**/*_test.go' --glob '!internal/testkit/**' >/dev/null; then
	fail "forbidden server, MCP, or WebSocket import is present"
fi

if rg -n 'strings\.Fields\(' cmd internal --glob '*.go' --glob '!**/*_test.go' --glob '!internal/testkit/**' >/dev/null; then
	fail "shell-like argument splitting is present"
fi

if rg -n '(^|[^A-Za-z])(sh|bash|zsh)[[:space:]]+-c([^A-Za-z]|$)' \
	internal/connector internal/runtime internal/app --glob '*.go' --glob '!**/*_test.go' >/dev/null; then
	fail "shell command execution is present"
fi

sql_importers=$(rg -l '"database/sql"' cmd internal --glob '*.go' --glob '!**/*_test.go' --glob '!internal/testkit/**' || true)
for sql_file in $sql_importers; do
	case "$sql_file" in internal/store/*) ;; *) fail "SQL import outside internal/store: $sql_file" ;; esac
done

exec_importers=$(rg -l '"os/exec"' cmd internal --glob '*.go' --glob '!**/*_test.go' --glob '!internal/testkit/**' || true)
for exec_file in $exec_importers; do
	case "$exec_file" in internal/connector/*|internal/runtime/*|internal/app/*) ;; *) fail "process launch outside approved boundary: $exec_file" ;; esac
done

if rg -n 'github\.com/yty-build/acp-gateway/internal/testkit' cmd internal/app \
	--glob '*.go' --glob '!**/*_test.go' >/dev/null; then
	fail "production composition imports internal/testkit"
fi

for responsibility in acp connector replay manager runtime settings store task session tui testkit; do
	directory="internal/$responsibility"
	[ -f "$directory/doc.go" ] || fail "missing package documentation: $directory/doc.go"
done

schema_files=$(rg -l 'CREATE[[:space:]]+TABLE' internal --glob '*.go' --glob '!**/*_test.go' || true)
[ "$schema_files" = "internal/store/migrations.go" ] || fail "persistent schema exists outside the metadata migration boundary"

schema_tables=$(rg --no-filename -o 'CREATE[[:space:]]+TABLE[[:space:]]+[a-z_]+' internal/store/migrations.go |
	awk '{print $3}' | LC_ALL=C sort)
expected_tables=$(printf '%s\n' provider_settings sessions tasks)
[ "$schema_tables" = "$expected_tables" ] || fail "metadata schema must contain only tasks, sessions, and provider_settings"

if rg -n 'CREATE[[:space:]]+TABLE[[:space:]]+(run|runs|invocation|invocations|event|events|frame|frames|message|messages|answer|answers|tool|tools|result|results|transcript|transcripts|replay|replays|artifact|artifacts|workflow|workflows|team|teams|mailbox|mailboxes|token|tokens)([[:space:](]|$)' \
	internal/store --glob '*.go' >/dev/null; then
	fail "prohibited durable product entity is present"
fi

if rg -n '(prompt|answer|thought|plan|tool_(input|output|call|result)|replay_(item|content)|transcript|token_stream|user_context)[[:space:]_]*(TEXT|BLOB)' \
	internal/store --glob '*.go' --glob '!**/*_test.go' -i >/dev/null; then
	fail "content-bearing metadata column is present"
fi

if rg -n '(ConversationRecord|updates\.jsonl|project-session-jsonl|conversation-record-json|app-server-thread-read)' \
	cmd internal --glob '*.go' --glob '!internal/replay/**' --glob '!**/*_test.go' >/dev/null; then
	fail "provider-native history reader escaped internal/replay"
fi

if rg -n '\b(ReadNativeSession|NativeSessionReader)\b' \
	internal/connector internal/manager internal/runtime internal/app \
	--glob '*.go' --glob '!**/*_test.go' >/dev/null; then
	fail "provider-native history leaked into the live Manager/runtime boundary"
fi

manager_connection_shape=$(awk '
	/^type ManagerConnection interface/ { inside=1 }
	inside { print }
	inside && /^}/ { exit }
' internal/connector/manager.go)
case "$manager_connection_shape" in
	*Replay*\(*|*History*\(*|*Read*Session*\(*)
		fail "live ManagerConnection exposes a provider-history port"
		;;
esac

tool_name_definitions=$(rg --no-filename '^(const|var)[[:space:]]+[A-Za-z0-9_]*ToolName[[:space:]]*=' \
	cmd internal --glob '*.go' --glob '!**/*_test.go' | wc -l | tr -d ' ')
if [ -f internal/connector/delegate_schema.go ]; then
	[ "$tool_name_definitions" -eq 1 ] || fail "production must define exactly one model-visible tool name"
	rg --no-filename '^const[[:space:]]+DelegateToolName[[:space:]]*=[[:space:]]*"taskai_delegate"' \
		internal/connector --glob '*.go' >/dev/null || fail "the sole typed capability is not taskai_delegate"
else
	[ "$tool_name_definitions" -eq 0 ] || fail "model-visible tool exists outside the connector boundary"
fi

prefix_definitions=$(rg --no-filename '^const[[:space:]]+ManagerPrefix[[:space:]]*=' \
	cmd internal --glob '*.go' --glob '!**/*_test.go' | wc -l | tr -d ' ')
if [ -f internal/connector/manager.go ]; then
	[ "$prefix_definitions" -eq 1 ] || fail "fixed manager prefix must have one shipped definition"
	resume_shape=$(awk '
		/^type ThreadResumeParams struct/ { inside=1 }
		inside { print }
		inside && /^}/ { exit }
	' internal/connector/manager.go)
	case "$resume_shape" in
		*DynamicTools*|*DeveloperInstructions*|*Prompt*|*Context*)
			fail "exact Resume shape contains reinjection state"
			;;
	esac
else
	[ "$prefix_definitions" -eq 0 ] || fail "manager prefix exists outside the connector boundary"
fi

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

printf '%s\n' 'security-check: repository and persistence boundaries verified'
