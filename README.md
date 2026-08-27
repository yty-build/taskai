# TaskAI

TaskAI is a terminal workspace where a user talks to a persistent Manager agent, and that Manager delegates to subordinate agents over the Agent Client Protocol (ACP).

## Current status

This repository is at the M01 clean scaffold. It builds the `taskai` command and exposes only help and version output; the interactive workspace is not yet usable. No connector or provider capability is claimed by this scaffold.

The approved contracts are [the product vision](docs/UX_PRODUCT_VISION.md), [the build plan](build_plan.md), and [the test plan](docs/TEST_PLAN.md). Connector feasibility evidence is recorded in [the G0 conformance report](docs/G0_CONNECTOR_CONFORMANCE.md).

## Build and inspect

```sh
make build
./bin/taskai --help
./bin/taskai --version
```

Run the M01 checks with:

```sh
make fmt-check
make test
make race
make vet
make security-check
make acceptance
```

## State and privacy boundary

The future application state root uses `TASKAI_STATE_DIR` when that variable is set to an absolute path. A relative override is rejected; when the variable is unset, the root is the platform configuration directory joined with `taskai`. M01 only resolves this name in tested library code and never creates the directory.

TaskAI will persist only Task and Session metadata plus non-secret provider configuration. Prompt text, answers, tool traffic, replay items, credentials, and provider-native history remain outside TaskAI persistence. Replay reads the exact provider-owned Session history used by Resume; it does not create a second transcript or cache.

Provider launch and typed delegation belong behind thin, version-conformed ACP connectors. The product has one model-visible delegation capability, no web server, no MCP control plane, no provider-TUI scraping, and no tmux runtime dependency.
