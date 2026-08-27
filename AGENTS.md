# TaskAI contributor instructions

## Contract authority

Read `docs/UX_PRODUCT_VISION.md`, `build_plan.md`, and `docs/TEST_PLAN.md` before changing product behavior. They are authoritative in that order for product intent, implementation sequencing, and verification. Do not weaken a gate or test to make an implementation pass.

Gate dependencies and milestone order in `build_plan.md` are mandatory. A capability may be advertised only after executable conformance proves it on the selected public provider boundary.

## Clean-slate boundary

The old implementation remains recoverable at commit `fad1f8f`. Do not restore, copy, reuse, port, or adapt production code from that checkpoint. Implement behavior from the approved contracts with new tests. Do not add a legacy command, compatibility route, migration, or state import.

The product and binary are named `taskai`; `cmd/taskai` is the only command. The integration owner controls root files, documentation contracts, `go.mod`, `go.sum`, `cmd/taskai`, and `internal/app` composition.

## Ownership and architecture

- `internal/acp`: thin standard ACP transport and opaque public shapes.
- `internal/connector`: provider launch, auth probes, typed delegation, and capability conformance.
- `internal/replay`: bounded reads from the exact provider-native Resume history.
- `internal/manager` and `internal/runtime`: Manager behavior, lifecycle, attach/detach, and exact Resume.
- `internal/task`, `internal/session`, `internal/settings`, and `internal/store`: domain rules and metadata-only persistence.
- `internal/tui`: the terminal interface.
- `internal/testkit`: deterministic fakes and test-owned fixtures.

TaskAI may persist only Task and Session metadata and non-secret provider settings. Never persist prompts, answers, messages, tool traffic, events, replay items, credentials, provider transcript paths, or content-derived indexes. Store only the opaque provider Session reference needed by exact Resume and Replay.

The Manager model owns delegation decisions. Present exactly one typed model-visible connector capability. Its singular operations may mechanically start one target, retrieve one native completion, continue one Follow stream, replay explicit bounded ranges, or cancel an owned turn. Do not parse manager prose or tool-output content, batch starts or answers, invent a semantic event model, or create durable invocation/queue entities.

Do not add web/API servers, MCP, brokers, workflows, mailboxes, provider TUI automation, terminal scraping, tmux integration, or a TaskAI transcript/session-log store.

## Verification

Every change needs tests at the appropriate layer. Before handoff, run:

```sh
make fmt-check
make test
make race
make vet
make build
make security-check
make acceptance
```

Keep fixtures synthetic and content-canary aware. Never inspect, print, copy, modify, or delete user provider Sessions, credentials, auth state, or external workspaces.
