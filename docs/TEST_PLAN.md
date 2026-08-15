# Agent Task Console V1 — Verification Plan

The primary Codex session runs this as a distinct verification pass after implementation. No tmux developer/tester workflow is required. Read `agent_task_console_spec.md` and `docs/DEVELOPMENT_PLAN.md`, record exact evidence for every defect, fix it in a focused commit, then rerun the affected checks and the complete final suite.

## T01 — Source, history, and privacy

- Verify the exact SHA and clean worktree.
- Verify `origin` is `yty-build/acp-gateway` and GitHub reports `PRIVATE`.
- Review every milestone commit.
- Search tracked files and history for credentials, auth state, databases, sockets, logs, binaries, coverage, or live worktrees.
- Confirm there are only Task and Session domain entities and no Run, Event, Artifact, workflow, broker, or daemon model.

## T02 — Static and automated verification

Run independently:

```bash
make acceptance
gofmt -l .
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/acpctl
make security-check
```

Inspect the quality and failure sensitivity of tests rather than accepting green output alone.

## T03 — Persistence and CLI behavior

With an isolated `AGENT_HOME`:

- Create/list/show/update Tasks.
- Create a Task with zero Sessions.
- Use `acpctl start` for a one-Session Task.
- Start multiple Sessions under one Task.
- Verify invalid IDs, statuses, profiles, and metadata.
- Restart CLI processes and verify persistence.
- Exercise concurrent creation and migration reopening.

## T04 — Deterministic ACP and worker lifecycle

Using the fake ACP/acpx fixtures:

- Prove worker survival after parent CLI exit.
- Connect multiple viewers.
- Prove one active turn and ordered queued prompts.
- Verify request IDs and response routing.
- Verify JSONL validity, monotonic positions, and replay.
- Cancel active/idle turns, stop, resume, crash, and recover.
- Exercise malformed/partial adapter output and missing native IDs.
- Verify socket, state, and log permissions.
- Start a custom ACP-compatible command.
- With the public runtime fixture, prove pending permission, exact offered decision, malformed duplicate-kind fail-closed behavior, cancellation, and exact resume identity.

## T05 — Interactive attachment

For `acpctl task attach`:

- Every Task Session appears with correct state and activity.
- A new Session appears without restarting.
- Keyboard and mouse selection work.
- Session drill-down and return work.
- Task-level input cannot silently broadcast.
- Detaching does not affect workers.

For `acpctl session attach`:

- Stored history loads before live output and does not duplicate.
- Prompts reach the correct Session.
- Busy steering is queued visibly.
- `Esc` cancels the current turn and a follow-up continues the same Session.
- Detach leaves work running.
- A connected original caller receives its correlated response.
- A disconnected caller recovers missed output from its cursor.
- Two viewers can observe safely.
- Raw and normalized views are faithful.
- A permission remains pending across detach; a reattached viewer can decide it.
- Two viewers racing a permission yield one accepted decision and one deterministic conflict.
- `allow_always`/`reject_always` are visibly persistent and require confirmation.

## T06 — Git, review, and native mode

- Create multiple isolated worktrees and branches.
- Verify shared-checkout protection and non-destructive cleanup behavior.
- Verify reviewer Session independence and complete producer evidence.
- Test native attachment with a fake provider CLI.
- Smoke-test real Codex native attachment when a native ID is available.
- Prove ACP/native mutual exclusion, held prompt queue, and reconciliation before ACP resumes.

## T07 — API and web

- Exercise every `/api/v1` endpoint and `/healthz`.
- Verify CLI, TUI, API, and web show consistent state.
- Verify Task aggregate SSE and Session replay/reconnect.
- Verify request/response correlation and invalid cursor handling.
- Verify loopback-only binding and no socket/ACP leakage.
- Verify web prompt/cancel/resume/stop and Session drill-down.
- Stop the web server and confirm workers continue.

## T08 — Authenticated live provider matrix

Use real `acpx` sessions for `codex`, `claude`, `grok-build`, `opencode`, and `gemini`.

For every provider:

- Create a Session under one acceptance Task with its own worktree.
- Send the Task-derived initial prompt.
- Require a provider-specific proof file and `go test ./...`.
- Require the provider to commit only its proof change on its Session branch.
- Confirm structured ACP history, IDs, status, and response routing.
- Detach the UI while work continues.
- Send a short follow-up prompt.
- Stop and resume the same Session.
- Verify it appears and drills down correctly in `acpctl task attach`.
- Exercise one harmless write permission in `read-approval`: the file must not exist before allow, must appear after allow, and must remain absent after reject.
- Record whether each provider uses `session/request_permission` or the one-shot host broker for `fs/write_text_file`/`terminal/create`; unknown host paths must fail closed.

Do not edit authentication configuration. Report an unexpected login prompt as a blocker for that provider.

## T09 — Defect loop and sign-off

For each failure, record the exact command, expected result, actual result, and relevant log. Add a regression test and a focused pushed fix commit. Rerun the affected tests and the complete final suite.

Sign off only with:

- tested SHA,
- commit audit,
- automated test output,
- live provider matrix,
- explicit acceptance checklist,
- remaining limitations,
- clean Git status.
