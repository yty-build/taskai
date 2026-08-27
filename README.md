# TaskAI

TaskAI is a terminal workspace where a user talks to a persistent Manager agent, and that Manager delegates to subordinate agents over the Agent Client Protocol (ACP).

## Current status

The M02 metadata foundation is complete. TaskAI now has validated Task and Session domains, same-Task depth-one ownership, non-secret provider Settings, and a metadata-only SQLite store with restrictive permissions. The command still exposes only help and version while connector/runtime and terminal integration are built; no interactive provider capability is claimed yet.

The approved contracts are [the product vision](docs/UX_PRODUCT_VISION.md), [the build plan](build_plan.md), and [the test plan](docs/TEST_PLAN.md). Connector feasibility evidence is recorded in [the G0 conformance report](docs/G0_CONNECTOR_CONFORMANCE.md).

## Build and inspect

```sh
make build
./bin/taskai --help
./bin/taskai --version
```

Run the required checks with:

```sh
make fmt-check
make test
make race
make vet
make security-check
make acceptance
```

## State and privacy boundary

The application state root uses `TASKAI_STATE_DIR` when that variable is set to an absolute path. A relative override is rejected; when the variable is unset, the root is the platform configuration directory joined with `taskai`. Informational commands do not create it. The metadata store creates it as `0700`, with its SQLite database, WAL, and shared-memory files restricted to `0600`.

TaskAI persists only Task and Session metadata plus non-secret provider configuration. Provider launch configuration is a bounded literal argv selected from registered connector profiles; it is never a shell command. Prompt text, answers, tool traffic, replay items, credentials, and provider-native history remain outside TaskAI persistence. Replay reads the exact provider-owned Session history used by Resume; it does not create a second transcript or cache.

Provider launch and typed delegation belong behind thin, version-conformed ACP connectors. The product has one model-visible delegation capability, no web server, no MCP control plane, no provider-TUI scraping, and no tmux runtime dependency.
