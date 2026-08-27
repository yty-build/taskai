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

The process boundary is viewer/worker, not client/server product architecture. The viewer owns Bubble Tea and any interactive provider authentication. The persistent worker owns the metadata store, Manager service, lifecycle Supervisor, and restrictive same-UID local socket; it must never inherit or retain a viewer terminal. Viewer detach or disconnect unregisters only that viewer and never stops a provider. Worker teardown stops and reconciles Manager trees before releasing the socket lock.

One TaskAI state root is permanently bound to one canonical project directory by a versioned one-way digest. Validate that binding before and after viewer acquisition, and before worker bootstrap. Never place the raw project path in the binding file or permit a second project to reuse an established root.

TaskAI may persist only Task and Session metadata and non-secret provider settings. Never persist prompts, answers, messages, tool traffic, events, replay items, credentials, provider transcript paths, or content-derived indexes. Store only the opaque provider Session reference needed by exact Resume and Replay.

The Manager model owns delegation decisions. Present exactly one typed model-visible connector capability. Its singular operations may mechanically start one target, retrieve one native completion, continue one Follow stream, replay explicit bounded ranges, or cancel an owned turn. Do not parse manager prose or tool-output content, batch starts or answers, invent a semantic event model, or create durable invocation/queue entities.

Apply the fixed TaskAI Manager prefix exactly once on a new provider Session through the conformed native instruction mechanism. Let the provider discover repository instruction files normally; never create, prepend, rewrite, or delete `CLAUDE.md`, `AGENTS.md`, or provider-equivalent files. Exact Resume accepts the stored opaque reference only. It must not accept a prefix, context, prompt, tool definition, history, cwd, or fallback Start path.

Replay is an explicit connector-owned operation over the same exact provider-native Session used by Resume. Prefer the supported native history API; a direct JSON/JSONL reader must pin one conformed root and resolve one exact reference without enumeration. Never add TaskAI replay storage, seals, copied messages, or a transcript cache.

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

End-to-end fixtures belong under `internal/testkit`, write provider-native content only beneath a test-owned provider root outside `TASKAI_STATE_DIR`, and must launch the compiled binary through a real pseudo-terminal. Production packages must not import a fixture or expose a fake-provider switch.
