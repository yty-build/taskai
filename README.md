# Agent Task Console

Agent Task Console is a local-first Go control plane for grouping persistent AI-agent Sessions under Tasks. It uses ACP through a pinned `acpx` adapter, detached per-Session workers, SQLite state, append-only JSONL history, interactive terminal consoles, and an optional loopback-only web interface.

The product deliberately has two first-class entities:

- **Task**: the work to be done.
- **Session**: one agent working on part of a Task.

## Development

Requirements:

- Go 1.23 or newer
- `acpx` 0.13.0 for live agent execution

```bash
make test
make race
make vet
make build
./bin/agent --help
```

Runtime state defaults to `~/.agent`. Set `AGENT_HOME` to use an isolated state directory.

See [the product specification](agent_task_console_spec.md) and [development plan](docs/DEVELOPMENT_PLAN.md) for the complete V1 contract. Installation and operational documentation will be completed with V1 hardening.
