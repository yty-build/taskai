# Agent Task Console — Product and Architecture Specification

**Status:** simplifying V1 contract
**Persistent model:** Task → Sessions
**Runtime primitive:** one Session Channel backed by one provider Port
**Primary interfaces:** CLI, structured terminal, and local web

## 1. Product summary

Agent Task Console is a local-first control plane for agent work across repositories. It intentionally has only two persistent first-class entities:

- **Task** — the outcome that needs to be produced.
- **Session** — one addressable provider-owned agent conversation doing part of that Task.

A Task may contain zero, one, or many Sessions. A Session belongs to exactly one Task. An optional parent Session expresses a manager/child relationship within that Task.

The product manages Task and Session lifecycle, repository association, one live provider Port per active Session, commands sent to that Port, provider activity streamed to viewers, and ordered replay of the mechanical channel journal.

The provider owns agent memory, conversation semantics, tool execution, reasoning, and native subagents. Git owns code history and worktrees. Repositories own their instructions, skills, and tool configuration.

## 2. Governing principles

```text
Task             = group of related agent work
Session          = address of one agent conversation
Provider Port    = live owner of the provider connection
Session Channel  = commands in, mechanical journal frames out
Viewer           = disposable attachment
Repo/worktree    = where work happens
```

These rules are normative:

1. Persist only Task and Session as product entities.
2. One active Session has exactly one live provider Port.
3. Start a provider conversation only when the Session has no provider reference.
4. Exact Resume is a worker/transport startup operation, never a per-prompt operation.
5. Keep a healthy Port alive across turns.
6. Allow one active turn and reject another prompt as busy.
7. Journal and stream provider events as soon as they arrive, without semantic projection.
8. Treat Session JSONL as channel replay and audit only.
9. Never parse or reconcile provider-private transcript stores.
10. Never infer committed history, invocation surface, or hidden provider state.
11. Detaching a viewer never affects the worker, Port, turn, or permission request.
12. Discover model and reasoning controls from the live Port; never hard-code catalogs.
13. Never hand a managed Session to a separate native CLI writer.
14. Keep HTTP, SSE, future WebSocket, CLI, and terminal as transports over shared services.
15. Keep V1 parent/child controls human/operator-facing; provider manager tools and child wait/result routing require an explicit post-V1 integration.

## 3. Non-goals

V1 does not introduce Run, Event, Artifact, Team, Subagent, Message, Mailbox, workflow, broker, or daemon entities. It does not provide provider-facing manager tools, child wait/result routing, peer mail, peer-to-peer messaging, a workflow engine, a transcript database, provider-store discovery, semantic history merging, ACP/native same-conversation handoff, or a gateway prompt queue.

Provider-native subagents may exist inside a provider harness. They remain provider behavior, not gateway entities.

## 4. Domain model

### 4.1 Task

```text
id
title
description
status
external_ref
metadata_json
created_at
updated_at
```

Suggested statuses are `open`, `working`, `review`, `done`, `failed`, and `cancelled`.

### 4.2 Session

```text
id
task_id
parent_session_id       # optional; same Task only

role
agent                   # profile / adapter selection
provider

repo_path
worktree_path
branch

provider_ref            # opaque provider conversation reference
status

worker_pid              # operational locator, not public API state
worker_socket
log_path
metadata_json           # permission policy and bounded escape hatch

created_at
updated_at
last_activity_at
```

Suggested statuses are `starting`, `working`, `waiting`, `done`, `failed`, `cancelled`, and `stopped`.

`provider_ref` is opaque. The gateway stores it only so the same adapter can Resume it. The gateway does not inspect provider storage to prove or enrich it.

Permission policy and worktree intent may live in `metadata_json`. Model and reasoning selection are disposable live-Port state and must not be stored or reapplied by the gateway. Provider advertisements are live observations, not catalog entities.

A Session persists when a viewer or worker exits. Restarting it does not create a Run.

### 4.3 Parent and child Sessions

```text
Task
└── Manager Session
    ├── Child Session A
    └── Child Session B
```

When `parent_session_id` is set, parent and child must share a Task, self-parenting and cycles are invalid, and reassignment must be explicit. Older Sessions remain valid with no parent. The gateway never guesses a manager from role text.

Task membership is team membership. No Team or membership model is needed.

## 5. Session Channel

```text
client command
     │
     ▼
Session service ──► worker ──► provider Port
     ▲                              │
     └──── ordered journal frames ◄┘
```

The Session Channel is a logical bidirectional runtime primitive, not a database entity. HTTP commands plus SSE may implement it today. A future WebSocket may carry the same envelopes.

### 5.1 Commands

The channel command vocabulary is:

```text
prompt
cancel
permission_response
configure
```

Connect, subscribe, replay-after, provider-options, status, ping, Resume, and stop are transport, observation, or lifecycle operations. `stop` stops the worker and closes its Port. It is not a provider command, and there is no separate channel `close` command or HTTP close endpoint. Request IDs are operational correlation fields, not entities.

An accepted `prompt` is journaled in `client_command.data` and sent to the Port as the exact caller-supplied text. The service does not trim, decorate, or wrap it in Task/role instructions, and it does not keep a different display prompt.

### 5.2 Frames

Every outward frame receives a monotonically increasing Session sequence:

```json
{
  "session_id": "S-123",
  "seq": 42,
  "request_id": "REQ-9",
  "timestamp": "2026-08-25T20:00:00Z",
  "type": "provider_event",
  "data": {"type":"text_delta","text":"Checking now."}
}
```

The complete outer journal type vocabulary is:

```text
connection
client_command
provider_event
provider_result
permission_requested
permission_resolved
permission_abandoned
worker_error
```

For `provider_event`, `data` is the normalized runtime event object verbatim, including unknown event types and fields. The gateway bounds the object at the adapter boundary but does not translate it into semantic outer frame types. The other journal types record only gateway commands, connection/result state, permission bookkeeping, or worker failure. Browser DTOs omit credentials, environment, sockets, and provider references.

### 5.3 Immediate streaming

Interactive clients follow this order:

```text
1. create or Resume Session; receive status starting with no provider options
2. attach Session stream and replay connection.data.state == "connecting"
3. wait for connection.data.state == "connected"
4. read current provider options from the live Port
5. optionally send configure and receive its synchronous result
6. send prompt
7. render every journal frame immediately
```

The worker makes its socket, journal, and subscribe/replay path available before invoking the one provider `Start` or exact `Resume`. While `connection.data.state == "connecting"`, provider-options, configure, prompt, cancel, and permission-response operations return typed `session_starting`; they are neither dispatched nor journaled as accepted. Status, ping, subscribe/replay, and Stop remain available. No transcript load, store check, provider connection, or projection may block attachment or that initial `connection` frame.

### 5.4 One active turn

If a prompt arrives while a turn is active, the gateway returns a typed busy result without accepting or queueing it. The client may wait, cancel, or retry.

### 5.5 Configuration

Clients make an observational provider-options read against the live Port, render the returned controls as dropdowns or arrow selectors, and send `configure` with advertised opaque values.

- Before any selection, blank leaves the provider default in place.
- In a later configuration request, a blank field leaves that control unchanged.
- Unsupported controls remain disabled with a reason.
- No profile or transport contains a model catalog.
- Values are exact, bounded, opaque provider values; clients must not normalize or guess them.
- Configuration returns success only after the worker applies it to the live Port.
- The `200` response includes the refreshed live provider options; no synthetic `config_changed` journal frame exists.
- Selections are not Session metadata and are not reapplied on Resume; the resumed Port's advertised current values are authoritative.

There is no prepared, launching, or launched phase. Creating an idle Session, connecting, configuring, and prompting are ordinary operations.

## 6. Provider Port

The conceptual adapter operations are:

```go
type Port interface {
    Start(context.Context, StartSpec) (SessionInfo, error)
    Resume(context.Context, StartSpec) (SessionInfo, error)
    ProviderOptions(context.Context, StartSpec) (ProviderOptionCatalog, error)
    Configure(context.Context, StartSpec) (SessionInfo, error)
    Prompt(context.Context, PromptSpec, func(Event) error) (PromptReceipt, error)
    Cancel(context.Context, Target) error
    Close() error
}
```

The concrete Go shape may differ, but the semantics may not:

- `Start` creates one provider conversation and returns its opaque reference.
- `Resume` reconnects exactly the saved reference.
- `ProviderOptions` is observational live state.
- `Configure`, `Prompt`, and `Cancel` use the already-live Port.
- User cancellation is a provider command, not an optimistic local context cancellation. The worker waits boundedly for the active prompt to drain its provider terminal result and for the Port adapter to prove that the same retained connection survived turn finalization. A cancellation error, timeout, or failed retained-Port proof fails and closes the Session Channel; only explicit Session Resume may reconnect it.
- `Close` releases provider resources only as worker lifecycle cleanup.
- `Prompt` emits normalized event objects incrementally; the worker journals them without semantic projection.

Start and Resume happen only when a worker establishes its provider transport. A viewer reconnect never causes Resume. If Resume fails, the Session stays stopped or failed; the gateway never replacement-Starts under the same Session ID or reconstructs memory from JSONL.

The Port implementation must also disable adapter/runtime fallback reconnects
inside prompt, configuration, or observational calls. If the retained provider
connection disappears, that Port fails; only an explicit Session Resume may
establish another connection to the saved `provider_ref`.

Adapters may use `acpx/runtime`, a provider structured app-server, another ACP command, or a future remote transport. Provider behavior remains behind the adapter boundary. All `acpx` behavior remains in `internal/acp`.

## 7. Worker lifecycle

One detached worker owns one active Session and one Port.

```text
create Session row
      │
validate/prepare checkout
      │
spawn one worker
      │
open socket + journal
      │
emit connection with data.state=connecting + accept subscribe/replay
      │
      ├── provider_ref empty ─► Port.Start once ─► save provider_ref
      └── provider_ref set   ─► Port.Resume exact reference once
      │                         (30-second startup bound)
      ▼
emit connection with data.state=connected
or worker_error + connection with data.state=failed
```

Create and explicit Resume return once the channel is addressable, with the Session in `starting` and without provider options. The Session is visible as soon as its row exists, and a viewer can observe `connection.data.state == "connecting"` while the provider starts. Provider-dependent commands are rejected with `session_starting` until `connection.data.state == "connected"`. Stop during startup cancels the provider connection attempt and closes the worker. A provider attempt that does not connect within 30 seconds fails the same Session; startup failure never creates or hides a replacement Session.

A healthy worker keeps the Port alive, accepts commands, enforces one turn, appends and broadcasts frames, tracks pending permissions, supports multiple viewers, and updates coarse status. It does not parse provider files, refresh before prompts, maintain a FIFO, or launch a second native writer.

Viewer disconnect has no runtime side effect. A slow viewer may be disconnected and later replay from its last sequence; it may not block provider work.

Startup probes stale PID/socket ownership but never clears it merely because the worker is unresponsive: worker death does not prove that provider descendants exited. Such a Session is marked `failed` with its locator retained as quarantine. Exact Resume and checkout reuse remain blocked until provider-process teardown is independently proven or handled manually. Explicit Resume is available only after orderly ownership release and calls exact `Resume(provider_ref)` once. The gateway never replays an uncertain prompt, and duplicate workers are rejected.

## 8. Ordered replay and audit

Each Session has private append-only JSONL containing the frames that crossed the channel. The canonical per-Session `channel-v1` path and the validated Session resource bind the file to its Session, so the private line does not duplicate `session_id`:

```text
seq
timestamp
request_id (optional)
type
data
```

Public Session and Task stream envelopes add `session_id` from that validated resource context so aggregate clients can address and deduplicate frames without trusting a duplicated on-disk identifier.

V1 replay treats invalid or unterminated JSONL and zero or non-contiguous sequence positions as corruption. The gateway writer owns the remaining line grammar; stricter semantic revalidation of manually modified, syntactically valid private lines is post-V1 hardening.

The `type` is one of `connection`, `client_command`, `provider_event`, `provider_result`, `permission_requested`, `permission_resolved`, `permission_abandoned`, or `worker_error`.

It supports reconnect, scrollback, Task activity aggregation, lifecycle diagnosis, and permission display. It is not the provider transcript, provider memory, an Event table, or evidence that the provider committed a turn.

A stream establishes a live subscription and replay boundary, replays through that boundary, then releases buffered later frames in order. Deduplication uses only Session ID and sequence—never text, time, fingerprints, or provider file positions.

Replay, frame data, subscriber buffers, browser state, and Task aggregation are bounded. A fresh Session viewer receives at most the last 256 frames and an explicit earliest-sequence/clipped transport notice; a saved nonzero cursor resumes exactly. A fresh Task viewer receives the same notice and effective cursor for every Session present at its initial boundary. Every server-issued aggregate cursor also marks that Task-membership boundary: a current Session absent from a saved aggregate cursor was created later and resumes at exact zero, so reconnect cannot silently replace its sequence-one boundary with a fresh tail. If an exact child cursor is invalid, the Task stream identifies that Session and directs the client to establish a fresh bounded aggregate cursor. If older frames are unavailable, report the earliest retained sequence or a replay gap; do not imply completeness.

After a stream resource and cursor validate, every failure while establishing its replay window is delivered as a bounded SSE `channel_error`, including a failure detected before `ready`; the response does not switch back to a JSON action-error shape. The browser treats `channel_error` as terminal for that connection, closes it, and displays the bounded guidance.

## 9. Task coordination

A Task groups Sessions but does not orchestrate them implicitly. In V1, `parent_session_id` is grouping and addressing metadata for human/operator clients. The API, web, and terminal surfaces may create and list direct child Sessions, then prompt or cancel one exact child after same-Task validation. These operations use the child's ordinary Session Channel.

Each child streams independently. The gateway does not wait for a child, synthesize a child result, or deliver child output into the parent provider. V1 does not expose `create_child`, `send_child`, `wait_child`, `cancel_child`, or `list_children` as provider tools.

A provider-facing manager adapter may be added after V1 as an explicit integration over the same Task and Session services. That integration owns any tool schema and wait/result mapping. Mailboxes, peer-to-peer messaging, offline delivery, broadcast, and peer discovery are also post-V1 external integrations; none requires another gateway domain entity.

## 10. Repository safety

Each Session has one primary checkout.

1. The requested path must be an existing Git repository or worktree.
2. Never run `git init` implicitly.
3. Never silently retarget to a nested repository; a bounded suggestion requires explicit confirmation.
4. Canonicalize symlinks before ownership checks.
5. Reject another active Session in the same checkout by default.
6. Sharing requires an explicit override.
7. Create an isolated worktree only when requested.
8. Never delete worktrees or branches automatically.

Suggested layout:

```text
$AGENT_HOME/worktrees/<task-id>-<session-id>
branch agent/<task-id>/<session-id>
```

Git and the filesystem own outputs. There is no Artifact entity.

## 11. Agent registry and ACP

Profiles declare `name`, `provider`, literal `command`, literal `args`, and optional environment. They contain no model catalog or credentials.

V1 pins `acpx` and uses its public runtime through one private bridge per Session worker. ACP grammar, process setup, event translation, permission callbacks, and compatibility behavior remain inside `internal/acp`.

The bridge normalizes runtime activity into an event object. The worker places that object verbatim in `provider_event.data`, including unknown event shapes. Provider-option discovery is an observational call on the live Port, not an invented stream event. The bridge never reads provider transcript files or forces a new connection before every prompt.

## 12. Permissions

Default policy is read approval. Write, execute, and unknown requests remain pending unless unattended approval was explicit.

- Display only exact provider-offered choices.
- Revalidate opaque permission and option IDs.
- First valid concurrent decision wins.
- Viewer detach leaves a request pending.
- Cancel, stop, Port failure, or worker shutdown abandons callbacks safely.
- Persistent choices are visibly persistent and require confirmation.

Provider-specific restrictive settings may ensure provider defaults do not bypass this boundary.

## 13. Terminal and web

The structured terminal is a Session Channel client. Task attachment navigates child Sessions; Session attachment shows replay then live frames, a composer, status, and permissions.

Provider-native CLIs are provider-owned and unmanaged. The gateway does not open one as a second writer, claim it is the same managed Session, or merge its output. A future native structured Port must be the Session's sole Port.

The browser must attach the stream before prompting, render frames immediately, populate config selectors only from the live provider-options response, keep permissions actionable, preserve draft/focus/selection/disclosure/scroll state, provide accessible mobile navigation, and leave workers running when the web process exits.

The UI may coalesce adjacent deltas for readability. That is local rendering, not history reconciliation.

## 14. Local API

V1 may use HTTP commands plus SSE:

```text
GET    /healthz

GET    /api/v1/tasks
POST   /api/v1/tasks
GET    /api/v1/tasks/{id}
GET    /api/v1/tasks/{id}/sessions
POST   /api/v1/tasks/{id}/sessions
GET    /api/v1/tasks/{id}/stream

GET    /api/v1/sessions/{id}
GET    /api/v1/sessions/{id}/stream
GET    /api/v1/sessions/{id}/provider-options
GET    /api/v1/sessions/{id}/permissions
POST   /api/v1/sessions/{id}/config
POST   /api/v1/sessions/{id}/prompt
POST   /api/v1/sessions/{id}/permissions/response
POST   /api/v1/sessions/{id}/cancel
POST   /api/v1/sessions/{id}/resume
POST   /api/v1/sessions/{id}/stop

POST   /api/v1/sessions/{id}/children
GET    /api/v1/sessions/{id}/children
POST   /api/v1/sessions/{id}/children/{child}/prompt
POST   /api/v1/sessions/{id}/children/{child}/cancel
```

The child routes are human/operator-facing transports over shared Session services. Their existence does not expose tools to the parent provider or create an implicit child wait/result path.

A future `/api/v1/sessions/{id}/connect` WebSocket may carry the same commands and frames. It is a transport replacement, not a second architecture.

There is no provider-history or merged-conversation endpoint. Session reads expose metadata/runtime state; streams expose ordered channel frames. V1 binds only to explicit loopback addresses.

## 15. Persistence

Target schema:

```sql
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT,
    status TEXT NOT NULL,
    external_ref TEXT,
    metadata_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    parent_session_id TEXT REFERENCES sessions(id),
    role TEXT,
    agent TEXT NOT NULL,
    provider TEXT,
    repo_path TEXT,
    worktree_path TEXT,
    branch TEXT,
    provider_ref TEXT,
    status TEXT NOT NULL,
    worker_pid INTEGER,
    worker_socket TEXT,
    log_path TEXT,
    metadata_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_activity_at TEXT
);
```

Only these two tables are allowed. Same-Task parent validation and cycle prevention happen transactionally in the Session service.

Legacy columns may remain physically present during a safe additive migration, but new code must not use them to parse stores, infer sources, or revive staged launch.

## 16. Failure and recovery

- Start failure leaves a visible failed Session.
- Resume failure leaves the Session stopped/failed; no replacement conversation is created.
- Worker death quarantines retained runtime ownership as failed, never silently clears it, and never replays an uncertain prompt.
- Viewer disconnect has no provider side effect.
- A lagging viewer reconnects from sequence.
- A second prompt returns busy.
- Permission callbacks fail closed when their Port closes.
- Missing/corrupt replay logs produce a visible bounded `channel_error`, including when corruption is detected before `ready`; they never trigger provider-file fallback.
- Unknown provider activity is preserved inside `provider_event.data` without changing the outer journal type.
- Repository drift fails validation before starting or resuming a writer.

During upgrade, do not delete legacy logs, native-owner markers, or launch files while an old worker or provider process may still be active. Stop old workers first and preserve user data.

## 17. Security boundary

V1 assumes one trusted OS user. Database, sockets, logs, bridge state, and worktrees use private permissions. Provider auth stays provider-managed. Commands use literal argv. Browser DTOs omit provider references and runtime secrets. Web mutation is same-origin and loopback only.

A public service would additionally require authentication, TLS, authorization, tenant/worker isolation, quotas, durable audit, secret management, and explicit recovery.

## 18. Acceptance criteria

### Model

- Create/list/view Tasks and multiple Sessions.
- Every Session belongs to one Task.
- Optional parent is same-Task, acyclic, and not self.
- SQLite contains only Task and Session product tables.

### Port and worker

- New Session calls Start once and saves its opaque reference.
- Restarted worker calls exact Resume once.
- Channel subscribe/replay and `connection` with `data.state == "connecting"` are available before Start/Resume completes.
- Provider startup is bounded to 30 seconds, and Stop cancels an in-progress startup.
- Healthy Port handles multiple turns without Refresh/Resume.
- Resume failure never replacement-Starts.
- Duplicate workers are rejected.
- Viewer exit leaves worker and turn alive.

### Channel

- Prompt, cancel, permission response, and configure work; stop closes the worker's Port as a lifecycle action.
- Create and Resume return `starting` without provider options; provider-dependent operations return `session_starting` until `connection.data.state == "connected"`.
- A second prompt receives busy and is not queued.
- First provider output is visible before turn completion.
- Frames have increasing Session sequences.
- Reconnect within retention has no gap or duplicate.
- Unknown activity remains visible inside a `provider_event` frame.

### Configuration

- Web and terminal selectors use live provider advertisements.
- No production code/profile/transport contains a model catalog.
- Unsupported controls stay disabled and leave the live Port unchanged.
- Configuration returns `200` only after the live Port applies it; selections are not persisted or reapplied, and no `config_changed` frame is required.

### History boundary

- No code reads provider-private transcript stores.
- No service joins provider history and Session JSONL.
- No UI claims committed/provisional or ACP/native attribution.
- JSONL is replay/audit only and never provider memory.

### Safety and UI

- Permissions survive detach and concurrent decisions accept one response.
- Repository validation, checkout exclusion, and worktree persistence hold.
- Web attaches before prompt and streams immediately.
- Terminal and web consume shared Session Channel services.
- HTTP+SSE is acceptable as the current logical channel; WebSocket is optional.
- Stopping web does not stop Sessions.

### Manager and child

- Human/operator transports create and list ordinary child Sessions in the same Task.
- Prompt and cancel target exact child IDs; children stream independently.
- No provider manager tools, child wait/result routing, mailbox, peer messaging, or third entity is introduced in V1.

Required gates:

```bash
make fmt-check
make test
make race
make vet
make build
make security-check
make acceptance
```

## 19. Implementation order

1. Replace the old history/continuity contract.
2. Add the simplified Session migration and optional parent.
3. Define provider Port and Session Channel interfaces.
4. Simplify worker to one Port, immediate frames, busy rejection, permissions, and replay.
5. Remove transcript readers/merge, staged launch/outbox, per-prompt Refresh, FIFO, and managed native handoff.
6. Put CLI, terminal, API, and web on shared channel services.
7. Add human/operator create, list, prompt, and cancel controls for same-Task child Sessions.
8. Complete race, security, recovery, and acceptance verification.

See `docs/DEVELOPMENT_PLAN.md` for ordered milestones.

## 20. Final rule

Before adding a primitive, ask:

> Can this be represented as a Task, a Session, a parent Session reference, the Session's provider Port, its ordered channel frames, or its repo/worktree?

If yes, do not add another abstraction.

```text
Task
└── Sessions
    ├── optional manager → child relationship
    ├── one worker → one provider Port
    ├── commands in / ordered journal frames out
    ├── JSONL replay and audit only
    └── repo/worktree
```

The gateway groups, addresses, streams, and safeguards provider-owned agents. It does not become their harness.
