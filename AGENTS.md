# Agent Task Console contributor instructions

The product contract is defined by `agent_task_console_spec.md`; the ordered build milestones are in `docs/DEVELOPMENT_PLAN.md`.

## Architectural boundaries

- Keep exactly two persistent first-class entities: Task and Session.
- A Task owns zero or more Sessions; a Session belongs to exactly one Task.
- A Session may name one parent Session in the same Task. This is the complete manager/child relationship; do not add Team, Subagent, Run, Event, Artifact, workflow, broker, mailbox, or daemon domain models.
- In V1, parent/child operations are human/operator-facing: transports may create or list children and prompt or cancel an exact same-Task child through shared Session services. Do not expose manager tools to the provider harness, add a wait/result-routing primitive, or imply that child output is delivered to the parent provider. Provider-facing manager tools, wait/result routing, mailboxes, and peer-to-peer messaging require explicit post-V1 integrations.
- Treat one live provider Port as the runtime owner of one Session. Start a new provider conversation only when the Session has no provider reference; otherwise exactly Resume it when the worker or transport starts.
- Make the Session Channel addressable and journal `connection` with `data.state == "connecting"` before the one provider Start or exact Resume. Create and Resume return the Session in `starting` without provider options; subscribe/replay remain available while provider-dependent operations return typed `session_starting`. Provider startup is bounded to 30 seconds, and stop cancels it.
- Keep a healthy Port connected across turns. Never Refresh, re-resume, or reconstruct provider state before each prompt.
- Send `prompt`, `cancel`, `permission_response`, and `configure` commands through the Session Channel. Send and journal accepted prompt text exactly as supplied; do not wrap it in a Task envelope or maintain a separate display prompt. `stop` is a Session lifecycle operation that stops the worker and closes its Port; there is no separate channel `close` command or API. Allow one active turn and reject another prompt as busy rather than queueing it in the gateway.
- Stream mechanical journal frames immediately. The only journal types are `connection`, `client_command`, `provider_event`, `provider_result`, `permission_requested`, `permission_resolved`, `permission_abandoned`, and `worker_error`. A `provider_event` carries the normalized runtime event object verbatim in `data`, including unknown event shapes; do not invent semantic outer frame types.
- Session JSONL is ordered channel replay and audit only; it is not provider memory, a transcript database, or a source to merge with provider-owned history.
- Never crawl, parse, select, reconcile, or infer committed history from provider-private transcript stores.
- Discover model and reasoning controls with the observational provider-options read from the live provider Port. Configuration is synchronous: return success only after the worker applies it to that Port. Model/effort selections are disposable Port state; do not persist or reapply them, hard-code catalogs, emit a synthetic `config_changed` frame, or add prepared/launching/outbox phases.
- Treat attachment as a disposable viewer connection. Detaching must never stop a worker, close its Port, cancel a turn, or resolve a permission request.
- Structured terminal, HTTP, and web clients are transports over shared Session Channel services. HTTP commands plus SSE may implement the logical channel; WebSocket is optional, not a separate architecture.
- A provider-native terminal is provider-owned and unmanaged. Do not attach it as a second writer to a managed Session or attempt native/ACP same-Session handoff.
- Keep all `acpx` behavior behind `internal/acp`.
- Preserve explicit repository validation, checkout exclusion, and opt-in worktree creation. Never initialize, retarget, share, or delete a checkout implicitly.

## Development commands

Run these before submitting changes:

```bash
make fmt-check
make test
make race
make vet
make build
make security-check
make acceptance
```

Never commit credentials, provider authentication state, SQLite databases, sockets, logs, binaries, coverage files, or live worktrees.
