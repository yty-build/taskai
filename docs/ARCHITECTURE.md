# V1 architecture

## Product model

Agent Task Console persists exactly two first-class entities:

```text
Task 1 ─────────────── 0..n Session
```

A Task stores title, description, status, optional external reference, and metadata. A Session stores its parent Task, role, agent/provider, repository/worktree/branch, one immutable canonical provider conversation identity plus adapter evidence, permission metadata, worker location, status, and timestamps.

Everything else is infrastructure or operational data:

- request ID — correlation field for one submitted prompt;
- JSONL record — append-only Session lifecycle/audit and provisional-overlay
  journal with a monotonic position;
- review — another Session with role `reviewer`;
- output — owned by Git and the filesystem;
- Task activity — a computed aggregate over child Session gateway journals.

SQLite migrations are guarded to create only `tasks` and `sessions`. There is no Run, Event, Artifact, workflow, broker, or daemon persistence model.

## Component boundary

```text
CLI commands       terminal console       local web / JSON+SSE
     │                    │                       │
     └────────────────────┼───────────────────────┘
                          ▼
              TaskService / SessionService
           SessionStream + conversation projection
              ┌───────────┼─────────────┐
              ▼           ▼             ▼
         SQLite store     │       internal/native
                          │        ┌─────┴───────────┐
                    worker runtime │                 │
                          │        ▼                 ▼
                   private socket  exact provider   provider-native CLI
                          │        store reader      │
                 detached worker   │                 │
                          │        │                 │
             internal/acp → acpx   │                 │
                          └────────┴────────┬────────┘
                                          ▼
                           canonical provider conversation
```

Transport handlers do not reimplement Task or Session behavior:

- `internal/task` and `internal/service` are shared application services;
- `internal/service.SessionStream` defines Session journal follow and Task
  aggregate follow; the shared conversation projection joins verified provider
  history with gateway-only overlay state for structured clients;
- `internal/store` owns SQLite and embedded migrations;
- `internal/worker` is the only long-lived runtime primitive;
- `internal/acp` is the only package that knows `acpx` grammar or output;
- `internal/native` is the only provider-native resume mapping;
- `internal/console`, `internal/api`, and `internal/web` are transports/views.

The HTTP API emits stable DTOs, not database rows. Worker PID/socket/log paths and gateway-level raw/debug payloads remain below that boundary.

## Session creation

1. Resolve the declarative agent profile.
2. Validate the Git repository and optional existing worktree.
3. Create a readable collision-resistant Session ID.
4. Acquire a per-repository filesystem lock.
5. Reject an active writer in the same checkout unless isolation or an explicit override was requested.
6. Persist the Session under an existing Task. `acpctl start` atomically persists the Task and first Session together.
7. Create an optional worktree at `$AGENT_HOME/worktrees/<task>-<session>` on branch `agent/<task>/<session>`.
8. Spawn a detached hidden `acpctl _worker --session <id>` process.
9. Submit a request-correlated initial prompt containing Task title/description, Session role, repository, worktree/branch, and optional Session instructions.

If atomic Task/Session persistence fails, neither row survives. If later provisioning fails, the persisted state is marked failed rather than silently disappearing. User worktrees and branches are never automatically deleted.

## Detached worker

One worker owns one active Session. It opens a user-only Unix socket and append-only JSONL log, persists its PID/socket/status, and rejects duplicate ownership.

The worker:

- probes `acpx` 0.13.0 before operations;
- owns one long-lived, private Node child using the public `acpx/runtime` API;
- creates or resumes the same ACP Session;
- allows at most one active ACP turn;
- serializes later prompts in a visible queue;
- participates in the per-Session provider-writer gate, so an ACP turn and the
  provider-native process can never send input concurrently;
- gives every prompt a request ID;
- appends every visible transition/output with a monotonic position;
- broadcasts the same correlated lines to multiple subscribers;
- supports prompt, status/ping, exact permission decision, cancel, stop, subscribe, and idle ACP refresh commands;
- clears worker ownership on orderly stop.

Pending permissions are runtime state plus append-only Session journal records,
not another entity or table. A viewer detach has no permission or turn side
effect. The first valid viewer decision wins; cancel/stop/bridge failure
abandons the request safely.

The runtime bridge uses a bounded NDJSON control pipe on child fd 3. Child stdin is reserved for the pinned runtime's own one-shot filesystem/terminal host prompt, allowing structured `client_operation` events to become normal permission cards without mixing provider terminal text into the protocol. Provider-specific restrictive modes/configuration prevent silent internal writes before this boundary.

The spawning CLI is not the worker's parent lifecycle owner. Terminal and web connections may exit without affecting it. Every normal CLI invocation reconciles stale persisted PID/socket state and removes stale runtime files.

## Conversation sources, projection, and reconnect

One canonical provider conversation has three distinct data paths:

| Source | Role |
| --- | --- |
| Verified provider-owned store | Committed, authoritative user-visible conversation history |
| Live ACP | Provisional reasoning, output, tools, permissions, and turn state while work is happening |
| Session JSONL | Gateway lifecycle/audit record, missed-stream recovery, provisional overlay, and explicit fallback |

The provider store may itself be JSONL, SQLite, or another provider format; it
is accessed only through verified provider adapters in `internal/native`.
Gateway JSONL is not promoted into a canonical transcript merely because a
provider snapshot is temporarily unavailable.

Session JSONL is the recovery boundary for gateway-visible activity. Each line contains:

```text
seq, timestamp, optional request_id, type, raw internal data
```

Session replay reads positions greater than a numeric cursor. An active worker
subscription replays missed positions before live delivery. Clients deduplicate
gateway deltas by Session ID and position.

A structured load verifies the immutable provider conversation ID and checkout,
reads a bounded provider snapshot, joins the gateway-only overlay through the
returned gateway cursor, and only then follows ACP after that boundary. If the
provider snapshot cannot be read on initial load, the service returns a bounded
gateway-history projection with an explicit fallback warning. On a transient
refresh failure, an already-open viewer retains its current in-memory
presentation and marks the refresh as delayed. HTTP, terminal, web, and future
mobile clients consume this shared projection; they do not parse provider
stores or join JSONL themselves.

After each settled ACP turn and after provider-native process exit/stop, a
structured viewer requests a fresh projection. The service prepares and
validates that bounded snapshot; the viewer then replaces its local
presentation atomically. Reconciliation does not blank or visibly reload the
transcript and cannot duplicate the provisional turn it commits. Presentation
keys remain stable so clients preserve scroll anchor/live-follow, focus,
composer draft/cursor, row/text selection, pending-approval focus, selected
Session/tool, and expanded reasoning/tool state.

A Task stream is a computed merge of every child Session's durable gateway
journal. Its cursor is a component-wise map:

```text
Session A → last position
Session B → last position
...
```

The HTTP transport encodes that map as an opaque URL-safe token. This preserves
lossless reconnect without introducing a global Event table or synthetic Task
stream entity. `FollowTask` polls durable gateway journals, so it also discovers
Sessions added after attachment.

Task aggregate activity remains a computed merge over child Session gateway
journals. It is an operational/attention feed, not the committed conversation
projection and not another persistent entity.

## Attachment

Task attachment is a responsive gateway cockpit. Direct Session attachment is
provider-native by default; `--acp` selects the structured gateway console.

- Task attachment loads the Task and all Sessions, replays their operational
  activity, then subscribes. Enter opens the selected native provider harness
  and `a` opens its reconciled structured ACP view.
- Structured ACP attachment loads the shared provider-snapshot-plus-overlay
  projection, then follows ACP from the captured gateway cursor.
- Multiple viewers may subscribe safely.
- A Task prompt is never broadcast; the user must select its target Session.
- In Task/structured ACP views, `q`, `Ctrl-C`, browser close, network loss, or `acpctl serve` exit detaches only the viewer. Native keys belong to the provider; tmux uses `Ctrl-B d` to detach while leaving it running.
- In the structured ACP view, `Esc` requests cancellation of the current ACP turn. It does not suspend an exact token/tool state.
- numbered permission choices select the exact option offered by the provider; persistent choices require confirmation;
- detaching while permission is pending leaves the agent waiting so another attachment can decide it.

## Canonical provider conversation and native attachment

The first exact identity returned by the provider adapter becomes the Session's immutable canonical provider conversation ID. Adapter-specific acpx, ACP, and native IDs remain evidence fields. A later observation may verify but may not silently replace the canonical value.

Before native attachment, `internal/native` verifies that exact canonical ID against the provider's local conversation store and the Session checkout. Unknown providers, missing IDs, ambiguous records, checkout mismatches, and unsupported mappings fail closed without opening a replacement conversation.

```text
canonical provider ID ── exact local-store verification ── native CLI resume
          │                                                │
          └────────── per-Session exclusive writer gate ───┘
                                                           │ process exits/stops
                                                           ▼
                                      refresh + provider-store reconciliation
                                                           │
                                                           ▼
                                              release queued ACP write
```

When tmux is available, the native CLI runs in a stable per-Session tmux session; detaching the viewer leaves it running. The gateway tags that tmux session with a hash of the canonical state root plus Session identity, explicitly passes the selected `AGENT_HOME` and current `PATH` to defeat stale tmux-server environment, and refuses an unowned name collision. A direct execution fallback is used outside a TTY, inside tmux, or when tmux is unavailable.

Viewer connections are non-exclusive, but provider writes are exclusive per
Session. If an ACP turn owns the writer gate, native attach waits or fails
visibly without cancelling that turn. While a provider-native process owns the
gate, ACP prompts remain visibly queued and do not reach the provider. A tmux
viewer detach is not process exit and does not release native ownership.

Native process exit/stop requests an exact ACP refresh plus provider-store
reconciliation while native ownership and the boundary remain closed to queued
writes. Ownership releases only after that reconciliation succeeds. Refresh,
prompt-transport failure, and post-turn identity inspection reload persisted
verification state and are time-bounded. An uncertain bridge and its provider
process group are terminated and awaited before exact ACP resume is attempted.
Queued work crosses the boundary only after identity recovery and atomic
reconciliation succeed; otherwise requests receive correlated errors and the
worker stops for explicit resume. Recovery never substitutes a different
conversation identity.

## Review

Review is an independent Session under the producer's Task. By default, the producer must be stopped or terminal so evidence is not changing. The review prompt includes:

- original Task context;
- producer Session ID and read-only JSONL audit path;
- repository/worktree and branch;
- captured Git status, staged diff, and unstaged diff;
- instructions to rerun critical verification;
- the specification's suspicious-behavior checklist.

The producer journal is evidence, not imported conversational context. Review
creation moves the Task to `review` status.

## HTTP and web process

`acpctl serve` opens a disposable `net/http` server on an explicit loopback IP. Server-rendered HTML and embedded CSS/vanilla JavaScript use the same `/api/v1` handlers for mutations and SSE.

The process has no worker shutdown hook. Its context cancellation gracefully closes HTTP connections and the database handle opened by that CLI process; detached workers retain their own processes and database connections.

Browser hardening includes loopback Host enforcement, cross-origin mutation rejection, CSP, frame denial, no-store responses, and output escaping. This is defense in depth for a trusted-user local tool, not a public authentication boundary.

## Failure and recovery rules

- **Viewer exits:** no worker change.
- **Worker exits:** later CLI reconciliation clears stale worker ownership;
  provider history remains committed and JSONL remains usable as disclosed
  gateway fallback.
- **CLI process exits after spawn:** worker continues.
- **Malformed/non-monotonic JSONL:** replay fails closed with an actionable error.
- **Malformed/partial/nonzero acpx output:** the adapter returns a typed
  protocol/command error; valid prefixes already emitted remain in the gateway
  journal/overlay.
- **Prompt client disconnects:** worker continues and records the request-correlated response for cursor replay.
- **Native verification fails:** no native process starts; the ACP worker is unchanged.
- **Provider snapshot is temporarily unreadable:** an already-open viewer keeps
  its current in-memory presentation and discloses the delayed refresh; an
  initial load uses the bounded gateway-history fallback. Never swap in a
  partial candidate.
- **Post-native refresh, reconciliation, or post-turn identity recovery fails:**
  warnings and correlated queued-request errors are recorded, the suspect
  process tree is terminated, and the worker stops for explicit exact resume;
  queued writes do not cross the unresolved ownership boundary.
- **SQLite contention:** busy timeout and per-repository locking protect local concurrency.
- **Existing checkout collision:** fail before a second writer unless explicitly overridden.

## Deliberate V1 boundaries

V1 does not include Kubernetes, a message broker, a permanent API daemon, remote worker routing, public authentication, multi-tenancy, an observability database, a model abstraction, an artifact service, or a provider credential store. A future deployment may replace local transports/persistence while preserving Task, Session, and the versioned API contract.
