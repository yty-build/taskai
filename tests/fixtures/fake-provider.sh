#!/bin/sh
# Deterministic interactive provider used by tmux lifecycle acceptance tests.
# It never connects to a model or network service.

if [ "${1-}" = "--version" ]; then
    printf '2.1.0 (TaskAI Fake Provider)\n'
    exit 0
fi

session_ref=${TASKAI_SESSION_REF:-unknown}
printf '%s\n' "$$" > ".taskai-fake-${session_ref}-pid"
pwd > ".taskai-fake-${session_ref}-cwd"
printf x >> ".taskai-fake-${session_ref}-launches"
(
    while :; do
        printf x >> ".taskai-fake-${session_ref}-heartbeat"
        sleep 0.1
    done
) &
heartbeat_pid=$!
trap 'kill "$heartbeat_pid" 2>/dev/null || true' EXIT HUP INT TERM

printf 'FAKE CLI — NO MODEL CALLS · %s · PID %s\n' "$session_ref" "$$"
printf '\033[?2004h'
cat
