# V1 architecture

## Product model

Agent Task Console persists exactly two first-class entities:

```text
Task 1 ─────────────── 0..n Session
                              │
                              └── optional parent Session in the same Task
```

A Task groups work. A Session addresses one provider-owned conversation and one checkout. `parent_session_id` is the complete manager/child relationship; it does not create a Team or Subagent object.

Workers, provider Ports, commands, frames, sequence numbers, request IDs, permissions, and viewer connections are runtime or operational concepts. They are not database entities.

## Component boundary

```text
CLI        structured terminal        web / JSON + SSE
 │                 │                         │
 └─────────────────┴─────────────────────────┘
                         │
                         ▼
              TaskService / SessionService
                         │
              logical Session Channel
              commands in / frames out
                         │
                         ▼
                 detached Session worker
                  │                 │
                  ▼                 ▼
           ordered JSONL       provider Port
           replay/audit        (one per Session)
                                    │
                                    ▼
                           provider-owned harness
```

Package boundaries:

- `internal/task` and `internal/session` define the two entities.
- `internal/service` owns shared application behavior.
- `internal/store` owns SQLite and migrations.
- `internal/worker` owns the detached Session runtime and live fan-out.
- `internal/acp` owns all `acpx` integration.
- provider-specific Ports stay behind a narrow adapter interface.
- `internal/console`, `internal/api`, and `internal/web` are transports/views.
- `internal/git` validates repositories and manages explicitly requested worktrees.

No transport implements its own conversation continuity logic.

## The provider Port

An active Session owns one live provider Port. The Port has four responsibilities:

1. `Start` one provider conversation when no saved provider reference exists.
2. `Resume` exactly the saved reference when a worker/provider transport starts again.
3. Accept commands on the live connection.
4. Emit ordered provider activity until closed.

Start and Resume are connection-startup operations. A healthy Port remains connected across turns. Prompt dispatch does not refresh, resume, inspect provider files, or create a replacement conversation.

The adapter boundary also blocks any runtime-internal reconnect fallback. A
lost retained connection fails the Port before or after the affected command;
the command path cannot silently reconnect. Reconnection is an explicit
Session Resume operation only.

If exact Resume fails, the Session remains stopped or failed. The gateway does not replay JSONL into the provider or call Start under the old Session ID.

Ports may wrap `acpx/runtime`, a provider structured app-server, a custom ACP command, or a future remote transport. That choice does not change Task, Session, or channel semantics.

## The Session Channel

The Session Channel is a shared application interface:

```text
commands: prompt, cancel, permission_response, configure
frames:   connection, client_command, provider_event, provider_result,
          permission_requested, permission_resolved,
          permission_abandoned, worker_error
```

`provider_event.data` is the normalized runtime event object verbatim, including unknown event shapes. The journal does not project it into semantic outer frame types. Stop is a Session lifecycle action that stops the worker and closes its Port; there is no separate channel close command or HTTP close endpoint.

The current public transport can remain HTTP mutations plus SSE. Together they are one logical bidirectional channel. A WebSocket may later carry the same envelopes without creating a parallel service.

Only one turn can be active. A second prompt receives a typed busy result and is not placed in a gateway FIFO.

The Session Channel is addressable before the provider Port is connected. It first journals `connection` with `data.state == "connecting"` and accepts subscribe/replay. Create and Resume return the Session in `starting` without provider options. Until the channel journals `connection` with `data.state == "connected"`, provider-options, configure, prompt, cancel, and permission response return typed `session_starting` without provider dispatch. Configuration options then come from an observational read of the live Port; the client optionally sends `configure`, receives the synchronous result, and sends `prompt`. There are no prepared/launching phases or initial-turn outbox.

## Session creation

1. Resolve the declarative agent profile.
2. Validate the exact repository path.
3. Canonicalize the checkout and enforce active-writer exclusion.
4. Create an optional isolated worktree when explicitly requested.
5. Persist the Session under its Task.
6. Validate an optional parent belongs to the same Task and introduces no cycle.
7. Spawn one detached worker.
8. Open the private socket and journal, emit `connection` with `data.state == "connecting"`, and accept subscribe/replay.
9. Return the Session in `starting`, without provider options, once that channel is addressable.
10. Invoke one provider Start or exact Resume behind the live channel, bounded to 30 seconds.
11. Persist the opaque reference from Start, then emit `connection` with `data.state == "connected"` when the Port is ready.
12. Expose provider options observationally and let the client configure/prompt through the ordinary channel.

The Session exists and is attachable before provider startup succeeds. Stop cancels an in-progress startup. Timeout or other startup failure emits bounded failure frames and updates that same resource; it never disappears and does not justify a duplicate Session.

## Worker lifecycle

One worker owns one active Session and one provider Port. It opens a private socket, uses a private runtime directory, and records its operational locator on the Session.

The worker:

- rejects duplicate ownership;
- exposes its socket, journal, starting status, and `connection` frame with `data.state == "connecting"` before provider startup;
- starts or exactly resumes the Port once;
- bounds provider startup to 30 seconds and lets Stop cancel it;
- retains it across turns;
- accepts channel commands;
- rejects prompts while busy;
- assigns increasing frame sequences;
- appends mechanical journal frames to Session JSONL and broadcasts them;
- keeps permission callbacks pending across viewer detach;
- supports many viewers without letting one slow viewer block the provider;
- closes the Port only on explicit lifecycle action or failure;
- clears its runtime locator on orderly stop.

Every normal process startup may detect stale worker PID/socket ownership, but it cannot infer that the provider process tree exited. It marks that retained ownership failed/quarantined and does not clear it or automatically restart the Session. Only orderly or independently proven teardown permits exact Resume and checkout reuse.

## Replay boundary

Session JSONL is an ordered record of channel frames:

```text
seq, optional request_id, timestamp, type, data
```

The canonical per-Session `channel-v1` path and validated Session resource supply identity; private lines do not duplicate it. Public Session and Task stream envelopes add that resource's `session_id`.

The exact types are `connection`, `client_command`, `provider_event`, `provider_result`, `permission_requested`, `permission_resolved`, `permission_abandoned`, and `worker_error`.

It supports reconnect, local scrollback, Task activity aggregation, and audit. It is not the provider transcript or memory.

A viewer attach establishes a live subscription and a high-water boundary, replays retained records through that boundary, then releases newer buffered frames in order. Clients deduplicate with Session ID and sequence only.

The gateway does not:

- read provider transcript JSONL or SQLite;
- match turns by text, time, or fingerprints;
- label frames committed or provisional;
- infer ACP versus native origin;
- replace a live projection from a second history source.

A fresh viewer starts from a byte-bounded tail and receives at most 256 frames plus an earliest-sequence/clipped notice. A saved nonzero cursor resumes exactly. A gap is disclosed as a replay limitation, not filled from provider-private files.

## Provider configuration

Model and reasoning controls are disposable live-Port capabilities, not gateway catalogs or Session metadata. The client observes bounded choices and current values from the connected Port, shows native selectors, and sends back exact opaque advertised values.

Blank in a configure request means leave that control's current live value unchanged. On a fresh Port that current value is the provider's default. Unsupported or malformed controls are disabled. Configure returns success only after the live Port applies the request; no selection is persisted or reapplied by the gateway, and no synthetic `config_changed` frame exists. After Resume, the resumed Port's advertised current values are authoritative.

## Manager and children

Task is the team boundary. A human/operator may create ordinary child Sessions with `parent_session_id` pointing to a parent in the same Task.

```text
web / terminal / API ─► SessionService ─► child Session Channel
```

The V1 operator surface creates and lists direct children, then prompts or cancels one exact child. The service validates membership and routes those operations through the child's ordinary channel. Each child exposes its own independent stream.

V1 does not install manager tools in the provider harness, wait for children on a parent's behalf, or route child results into the parent provider. A provider-facing manager adapter and its wait/result mapping require an explicit post-V1 integration. Mailboxes, peer routing, offline delivery, and child-to-child messaging are likewise post-V1 external integrations.

## Permissions

Pending permissions live in worker memory plus ordered channel frames. Detaching does nothing. A live snapshot includes the exact journal high-water captured under the same permission serialization lock; viewers reconcile it only after replay reaches that sequence and reject older or prior-connection snapshots. Exact option IDs are validated, and the first valid concurrent decision wins. Cancellation, stop, provider disconnect, and worker failure abandon callbacks safely.

An ordinary Cancel is forwarded through the live Port while the Prompt remains attached. The worker reports cancellation only after the prompt drains its terminal result and the adapter proves the retained Port is still reusable. Cancellation failure, timeout, or lost Port continuity fails and closes the channel instead of advertising a reusable Session.

Browser-facing frames expose safe labels and opaque control IDs, not raw environment, credentials, runtime paths, or provider references.

## Repository and worktree safety

The gateway requires an existing Git checkout. It never initializes a directory or silently switches to a nested repository. Symlinks are resolved before checkout ownership checks.

An active managed writer blocks another Session in the same checkout unless the user requests an isolated worktree or explicitly approves sharing. Worktrees and branches are never removed automatically.

## Attachment

Viewer attachment is disposable. CLI exit, terminal detach, browser close, network loss, or web-server shutdown cannot stop the worker or active turn.

Structured terminal and web attach to the same Session Channel. A provider-native CLI may be invoked independently by the user, but it is unmanaged and is not a second writer for the managed Session. The gateway does not claim continuity or import its history.

## Failure rules

- Start failure keeps a failed Session row.
- Resume failure never replacement-Starts.
- Worker death never replays an uncertain prompt.
- Busy prompt submission mutates nothing.
- Lagging viewers are disconnected, not allowed to block provider output.
- Missing/corrupt replay produces a bounded warning and does not trigger provider-store reads.
- Unknown provider activity remains bounded and visible inside `provider_event.data`.
- Checkout drift blocks provider startup/resume.

## Deliberate V1 boundaries

- exactly two persistent entities;
- local trusted-user operation;
- explicit loopback web binding;
- one live Port and one active turn per Session;
- HTTP+SSE accepted as the current logical channel;
- no provider-history ingestion;
- no managed native/ACP handoff;
- no provider-facing manager tools or child wait/result routing;
- no peer mailbox;
- no automatic worktree deletion;
- no public-service security claim.
