# Agent Task Console V1 — Developer Execution Plan

This plan is executed directly by the primary Codex implementation session. It does not depend on a tmux developer or tester handoff.

Read `agent_task_console_spec.md` completely before editing. The specification is authoritative. Implement the full V1 locally in Go, preserve the Task → Sessions model, and do not add Run, Event, Artifact, workflow, broker, or daemon abstractions.

## Execution rules

1. Work through D01–D14 in order.
2. Add feature tests in the same commit as each feature.
3. Run the relevant tests before every commit.
4. Keep every commit focused and buildable; do not squash or amend completed milestones.
5. Push every green commit to the already-verified private `origin`.
6. Never commit credentials, provider auth state, databases, sockets, logs, binaries, coverage, or live worktrees.
7. Preserve unrelated work and never use destructive Git commands.
8. Do not start web work until the CLI and interactive attachment flow are pleasant and green.
9. After implementation, run a separate evidence-driven verification pass against the test plan before declaring completion.
10. If a dependency or adapter behavior differs from the specification, investigate it, preserve the product contract behind an internal adapter, document the decision, and continue when safely possible.

## D01 — Private Go project bootstrap

- Verify `origin` is `yty-build/acp-gateway` and is private.
- Install Go with Homebrew if it is unavailable.
- Install `acpx@0.13.0` beneath `~/.local` without `sudo` if it is unavailable. Use a user-owned npm cache if the default cache has ownership problems.
- Create the Go module `github.com/yty-build/acp-gateway`.
- Add `.gitignore`, `AGENTS.md`, a README scaffold, and build/test/format/vet/race targets.
- Add `cmd/acpctl` with version, help, stable error output, and exit-code handling.
- Add `AGENT_HOME`, defaulting to `~/.agent`, and create state directories with user-only permissions.
- Prefer standard-library CLI dispatch and avoid an unnecessary command framework.

Commit: `chore: bootstrap the private Go project`

## D02 — Task and Session persistence

- Use a pure-Go SQLite driver and embedded transactional migrations.
- Implement exactly the `tasks` and `sessions` tables from the spec.
- Enable foreign keys, busy timeout, and safe local concurrency.
- Implement repositories, UTC timestamps, status validation, metadata JSON validation, and readable collision-resistant Task/Session IDs.
- Cover fresh database, reopening, idempotent migration, foreign key, invalid state, and concurrent creation behavior.

Commit: `feat: add task and session persistence`

## D03 — Task management

- Implement `acpctl task new`, `acpctl tasks`, `acpctl task <id>`, and `acpctl task status`.
- Implement `acpctl start` as atomic convenience for creating a Task and its first Session without changing the persisted relationship.
- Include descriptions, external references, Session summaries, useful errors, and stable human-readable output.
- Add isolated CLI integration and output tests.

Commit: `feat: add task management commands`

## D04 — Declarative agent registry

- Load embedded default YAML profiles and overlay `~/.agent/agents/*.yaml`.
- Support only name, provider, command, args, and environment in V1.
- Provide profiles for Codex, Claude, Gemini, Grok (`grok`, with `grok-build` compatibility), and OpenCode.
- Support raw custom ACP-compatible commands.
- Validate names and commands and never use shell interpolation.
- Provide a small list/diagnostic command.

Commit: `feat: add declarative agent registry`

## D05 — ACP adapter

- Put every `acpx` invocation and output assumption in `internal/acp`.
- Pin and probe `acpx@0.13.0` compatibility.
- Run one long-lived Node bridge per Session worker through the public `acpx/runtime` export; do not use the detached `acpx prompt` queue owner or a PTY.
- Implement session create/ensure, prompt, status, cancel, reconnect/resume, metadata inspection, and interactive `session/request_permission` decisions.
- Stream normalized runtime events and preserve the complete gateway event payload. Document that the public runtime does not expose an exact raw JSON-RPC packet stream.
- Persist Task Console and acpx record IDs distinctly from one canonical provider conversation ID. Retain ACP/native source IDs only as adapter evidence and migration compatibility.
- Assign a request ID to every prompt and preserve it through streamed output.
- Keep runtime state/bridge source private, keep provider auth out of persisted options, bound bridge frames and diagnostics, and fail closed on malformed permission options.
- Build a deterministic fake-`acpx/runtime` fixture for success, cancellation, exact resume, permissions, duplicate option kinds, and reconnection. Retain CLI-adapter fixtures for the legacy compatibility unit tests.
- Run a focused live compatibility spike before locking the subprocess/queue behavior.

Commit: `feat: add the acpx execution adapter`

## D06 — Durable Session workers

- Implement hidden `acpctl _worker --session <id>`.
- Detach it so it survives the parent CLI.
- Enforce one worker per active Session.
- Implement the NDJSON Unix socket protocol from the spec with user-only permissions.
- Support multiple simultaneous subscribers.
- Permit one active ACP turn and a serialized prompt queue.
- Broadcast request-correlated activity to all viewers.
- Append monotonically positioned JSONL history.
- Handle signals, stale PID/socket state, duplicate workers, and crash recovery.
- Keep ACP work independent of disposable viewers. Native attachment must not acquire a worker lease, pause ACP work, cancel a turn, or hold queued prompts.

Commit: `feat: add durable session workers`

## D07 — Session lifecycle controls

- Implement `acpctl session start`, detail, prompt, cancel, resume, stop, and logs.
- Implement `acpctl task logs` aggregation.
- Starting a Session must send an initial prompt containing Task title, description, role, and repo/worktree context; `--prompt` appends Session-specific instructions.
- Default to read approval. Require an explicit choice such as `--approve-all` for unattended writes and persist it in `metadata_json`.
- `logs --follow` must support normalized and `--raw` output and fall back to JSONL when the worker is unavailable.
- Every normal CLI invocation must reconcile stale workers.

Commit: `feat: add session lifecycle controls`

## D08 — Interactive Task and Session attachment

- Implement `acpctl task attach <task-id>` and `acpctl session attach <session-id>`.
- Use one focused Go terminal UI dependency for keyboard, mouse, streaming, and text input.
- Task attachment uses a polished two-pane developer console: Session navigator, selected-Session preview, contextual footer, help, filter, and command palette, with a stacked narrow-terminal layout.
- Pressing Enter on a Session opens the real provider-native harness for that canonical conversation. An explicit ACP action opens the gateway transcript/permission console.
- New Sessions must appear without restarting the console.
- Keyboard and mouse selection drill into a Session and allow return to the Task view.
- Session detail replays complete stored ACP-visible history, then continues live without duplication.
- Render messages, tools, commands, results, diffs, permissions, errors, and provider-exposed reasoning; provide a raw toggle.
- Number exact provider-offered permission choices, distinguish persistent decisions, and keep a permission actionable across detach/reattach.
- A prompt submitted while busy is visibly queued.
- `Esc` requests cancellation of the current turn; it is not true suspension.
- Detach closes only the viewer and never affects the worker or turn.
- Reconnect from the last stream position and recover missed correlated output.
- Multiple viewers must work safely.

Commit: `feat: add interactive task and session consoles`

## D09 — Git and worktree isolation

- Validate Git repositories.
- Implement optional `--worktree` using `~/.agent/worktrees/<task>-<session>` and `agent/<task>/<session>` naming.
- Reject accidental concurrent writers in the same checkout unless explicitly requested.
- Persist repo/worktree/branch and never automatically delete user worktrees or branches.
- Test with disposable Git repositories.

Commit: `feat: add isolated git worktrees`

## D10 — Canonical provider-native attachment

- Make `acpctl session attach <id>` open the real provider-native harness by default; use `--acp` for the structured gateway console and keep `--native` only as a compatibility alias.
- Resolve the exact canonical provider conversation ID from the Session. Never select a conversation by recency or create a replacement conversation as an attach fallback.
- Verify that ID against the provider's local conversation store and the Session repo/worktree before launching Codex, Claude, Grok, or OpenCode.
- Launch the documented provider resume command in the correct repo/worktree, preferably in a tmux-backed native harness so a terminal detach does not end the provider process.
- Do not acquire a lock or lease, cancel/pause ACP, change Session status, or hold queued ACP prompts. Show an advisory warning when both surfaces may be active; the user owns concurrency.
- After native exit, request a best-effort ACP refresh at the next idle boundary. Refresh failure must be visible but must not strand or replace the canonical conversation.
- Test exact provider argv, provider-store verification, native detach/reattach, and ACP → native → ACP continuity for Codex, Claude, Grok, and OpenCode.

Commit: `feat: make native harness the default session attachment`

## D11 — Independent review Sessions

- Implement `acpctl session review <producer> --agent <reviewer>`.
- Create a new reviewer Session under the same Task.
- Include original Task, producer ID, JSONL path, repo/worktree, branch/diff, and independent verification instructions.
- Include the suspicious-behavior checklist from the spec.
- Do not import the producer conversation into the reviewer conversation.
- Do not begin against an actively changing producer checkout without an explicit safe state.

Commit: `feat: add independent agent review sessions`

## D12 — Shared service and versioned API boundary

- Define shared TaskService, SessionService, and SessionStream behavior used by CLI, TUI, and HTTP.
- Implement all `/api/v1` routes and `/healthz` from the spec.
- Use stable API DTOs rather than SQLite rows.
- Prompt submission returns a request ID.
- Task streams aggregate every Session.
- Session and Task SSE support cursor/`Last-Event-ID` replay.
- Keep raw ACP and Unix socket details below the boundary.
- Bind only to loopback in V1 and reject unsupported public exposure.

Commit: `feat: expose the versioned task and session API`

## D13 — Local web UI

- Implement `acpctl serve` with Go `net/http`, server-rendered HTML, minimal JavaScript, and SSE.
- Add Tasks, Task detail, clickable Session detail, stored+live history, prompt/cancel/resume/stop, review, and raw execution controls.
- Make desktop and mobile layouts responsive structured ACP views of the same canonical provider conversation.
- Expose only safe linked/verified continuity indicators; never send provider conversation identifiers to the browser.
- Use `/api/v1` and shared services; do not duplicate business logic.
- Prove stopping the web server does not affect workers.

Commit: `feat: add the local task console web UI`

## D14 — Hardening and documentation

- Complete README, installation, agent profiles, architecture, API, security, permissions, troubleshooting, and public-service boundary documentation.
- Use `acpctl` as the only executable/usage name; never install or overwrite a global `agent` command.
- Add deterministic end-to-end acceptance automation and map it to the spec checklist.
- Add build/version metadata and secret/runtime-file checks.
- Run `gofmt -l .`, `go test ./...`, `go test -race ./...`, `go vet ./...`, and `go build ./cmd/acpctl`.
- Ensure `git status --short` is empty, push the final commit, and report all SHAs, test output, and limitations.

Commit: `docs: complete V1 operations and acceptance guide`
