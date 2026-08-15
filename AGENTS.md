# Agent Task Console contributor instructions

The product contract is defined by `agent_task_console_spec.md`; the ordered build milestones are in `docs/DEVELOPMENT_PLAN.md`.

## Architectural boundaries

- Keep exactly two persistent first-class entities: Task and Session.
- A Task owns zero or more Sessions; a Session belongs to exactly one Task.
- Do not add Run, Event, Artifact, workflow, broker, or daemon domain models.
- Keep all `acpx` behavior behind `internal/acp`.
- Treat attachment as a disposable viewer connection. Detaching must never stop a worker or cancel a turn.
- Treat provider-native attachment as exclusive; ACP and a native CLI must never write concurrently.
- Keep HTTP, CLI, and terminal UIs as transports over shared application services.

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
