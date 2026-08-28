# TaskAI

TaskAI is a terminal workspace where a user talks to a persistent Manager agent, and that Manager delegates to subordinate agents over the Agent Client Protocol (ACP).

## Current status

TaskAI now includes the interactive terminal workspace, a viewer-independent runtime worker, direct ACP Manager-to-subordinate delegation, exact provider Session Resume, and bounded provider-native Replay. Tasks is always the launch screen; provider enablement, authentication, and status recheck are available only from Settings.

The approved contracts are [the product vision](docs/UX_PRODUCT_VISION.md), [the build plan](build_plan.md), and [the test plan](docs/TEST_PLAN.md). Connector feasibility evidence is recorded in [the G0 conformance report](docs/G0_CONNECTOR_CONFORMANCE.md).

## Provider capabilities

Capabilities fail closed unless the compiled connector, native Session boundary, and production ports match the conformance evidence.

| Provider | Manager | Subordinate | Resume | Replay | Authentication |
| --- | --- | --- | --- | --- | --- |
| Codex | Yes | Yes | Yes | Yes, through exact App Server `thread/read` | Settings |
| Claude Code | No | Yes | Yes | Conditional on an exact safe native root and project scope | Settings |
| Gemini CLI | No | No | No | No | No |
| Grok Build | No | No | No | No | Settings (`grok models` recheck) |

Gemini 0.25 includes a timestamp in its native record key that ACP does not return, and Grok 1.0.5's exact workspace-key derivation is not proven. TaskAI does not enumerate provider history or guess either mapping. Grok authentication and its output-discarded `grok models` recheck are available independently of those ACP capability claims. Claude scoped capabilities also fail closed for a non-BMP project path, and Claude Replay remains unavailable when its configured native root is absent or unsafe. Claude is intentionally absent from Add Manager: its pinned ACP connector exposes subordinate Sessions, but no conforming non-MCP carrier for TaskAI's required `taskai_delegate` Manager capability.

## Build and inspect

```sh
make build
./bin/taskai --help
./bin/taskai --version
./bin/taskai
```

Run `taskai` from the project directory whose provider-native instructions and tools should apply. The viewer automatically attaches to the persistent local worker. Closing or leaving the viewer detaches it without stopping active Managers; stopping a Manager is a separate explicit action. After a worker restart, an interrupted Manager can Resume only through its recorded opaque provider reference and advertised exact-Resume capability.

Run the required checks with:

```sh
make fmt-check
make test
make race
make vet
make security-check
make acceptance
```

`make acceptance` launches the compiled binary in real pseudo-terminals against test-owned synthetic Codex App Server and ACP processes. It exercises the 80×24, 60×18, and 40×16 layouts, authentication terminal ownership, delegation, native Replay, viewer detach, worker restart, and exact Resume without reading any user provider state.

## State and privacy boundary

The application state root uses `TASKAI_STATE_DIR` when that variable is set to an absolute path. A relative override is rejected; when the variable is unset, the root is the platform configuration directory joined with `taskai`. Informational commands do not create it. On first worker launch, the root is permanently bound to the current project by a versioned one-way digest; opening that same root from another project fails closed. Use a distinct absolute `TASKAI_STATE_DIR` for each additional project. The metadata store creates the root as `0700`, with its binding, SQLite database, WAL, socket, and lock files restricted to `0600`.

TaskAI persists only Task and Session metadata plus non-secret provider configuration. Provider launch configuration is a bounded literal argv selected from registered connector profiles; it is never a shell command. Prompt text, answers, tool traffic, replay items, credentials, and provider-native history remain outside TaskAI persistence. Replay reads the exact provider-owned Session history used by Resume; it does not create a second transcript or cache.

On a new Manager Start, the manager-capable connector supplies the fixed TaskAI instructions once through the provider's native instruction field and exposes exactly one model-visible capability, `taskai_delegate`. The provider continues its normal `AGENTS.md`, `CLAUDE.md`, or equivalent project-instruction discovery; TaskAI does not edit those files. Exact Resume sends only the same opaque native Session reference and restores supported runtime capability wiring. It never reinjects the prefix, user context, messages, or Replay items and never falls back to a replacement Start.

The Manager model alone decides whether and how to delegate. The connector executes its typed request through direct downstream ACP and reports only content-free child lifecycle metadata to TaskAI core. There is no web server, MCP control plane, provider-TUI scraping, semantic coordinator, TaskAI transcript store, or tmux runtime dependency.
