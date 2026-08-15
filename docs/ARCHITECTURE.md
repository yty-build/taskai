# V1 architecture

## Product model

Agent Task Console persists exactly two first-class entities:

```text
Task 1 ─────────────── 0..n Session
```

A Task stores title, description, status, optional external reference, and metadata. A Session stores its parent Task, role, agent/provider, repository/worktree/branch, distinct Task Console/acpx/ACP/provider-native identifiers, permission metadata, worker location, status, and timestamps.

Everything else is infrastructure or operational data:

- request ID — correlation field for one submitted prompt;
- JSONL record — append-only Session history with a monotonic position;
- review — another Session with role `reviewer`;
- output — owned by Git and the filesystem;
- Task activity — a computed aggregate over child Session history.

SQLite migrations are guarded to create only `tasks` and `sessions`. There is no Run, Event, Artifact, workflow, broker, or daemon persistence model.

## Component boundary

```text
CLI commands       terminal console       local web / JSON+SSE
     │                    │                       │
     └────────────────────┼───────────────────────┘
                          ▼
              TaskService / SessionService
                       SessionStream
                    ┌─────┴─────┐
                    ▼           ▼
               SQLite store   worker runtime
                                  │
                           private Unix socket
                                  │
                          detached Session worker
                                  │
                    internal/acp → acpx 0.13.0
                                  │
                           ACP-compatible agent
```

Transport handlers do not reimplement Task or Session behavior:

- `internal/task` and `internal/service` are shared application services;
- `internal/service.SessionStream` defines Session history/follow and Task aggregate history/follow;
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
- gives every prompt a request ID;
- appends every visible transition/output with a monotonic position;
- broadcasts the same correlated lines to multiple subscribers;
- supports prompt, status/ping, exact permission decision, cancel, stop, subscribe, and native-lease commands;
- clears worker ownership on orderly stop.

Pending permissions are runtime state plus append-only Session history, not another entity or table. A viewer detach has no permission or turn side effect. The first valid viewer decision wins; cancel/stop/bridge failure abandons the request safely.

The runtime bridge uses a bounded NDJSON control pipe on child fd 3. Child stdin is reserved for the pinned runtime's own one-shot filesystem/terminal host prompt, allowing structured `client_operation` events to become normal permission cards without mixing provider terminal text into the protocol. Provider-specific restrictive modes/configuration prevent silent internal writes before this boundary.

The spawning CLI is not the worker's parent lifecycle owner. Terminal and web connections may exit without affecting it. Every normal CLI invocation reconciles stale persisted PID/socket state and removes stale runtime files.

## History and reconnect

Session JSONL is the recovery boundary for ACP-visible activity. Each line contains:

```text
seq, timestamp, optional request_id, type, raw internal data
```

Session replay reads positions greater than a numeric cursor. An active worker subscription replays missed positions before live delivery. Clients deduplicate by Session ID and position.

A Task stream is a computed merge of every child Session's durable history. Its cursor is a component-wise map:

```text
Session A → last position
Session B → last position
...
```

The HTTP transport encodes that map as an opaque URL-safe token. This preserves lossless reconnect without introducing a global Event table or synthetic Task stream entity. `FollowTask` polls durable history, so it also discovers Sessions added after attachment.

Native provider transcripts remain authoritative for details ACP does not emit. Session JSONL is authoritative for Task Console replay and operational correlation.

## Attachment

Attachment always means a viewer/controller connection unless `--native` is explicit.

- Task attachment loads the Task and all Sessions, replays every history, then subscribes.
- Session attachment replays that Session, then subscribes from its last position.
- Multiple viewers may subscribe safely.
- A Task prompt is never broadcast; the user must select its target Session.
- `q`, `Ctrl-C`, browser close, network loss, or `acpctl serve` exit detaches only the viewer.
- `Esc` requests cancellation of the current ACP turn. It does not suspend an exact token/tool state.
- numbered permission choices select the exact option offered by the provider; persistent choices require confirmation;
- detaching while permission is pending leaves the agent waiting so another attachment can decide it.

## Provider-native ownership

Native mode requires both an ACP Session ID for reconciliation and a provider-native Session ID for exact CLI resume. Unknown providers and missing IDs fail closed.

```text
ACP worker active
      │ wait for turn or explicitly cancel
      ▼
worker acquires exclusive native lease; prompt queue pauses
      │
      ▼
native CLI resumes exact provider conversation in persisted checkout
      │ native process exits
      ▼
worker reconciles same conversation through acpx
      │ success only
      ▼
lease releases; queued ACP prompts continue
```

If native execution or reconciliation fails, the worker remains alive and holds the lease. The operator must ensure the native CLI exited, fix the cause, and run `--reconcile-only`. The system never guesses an identifier or starts a replacement conversation.

## Review

Review is an independent Session under the producer's Task. By default, the producer must be stopped or terminal so evidence is not changing. The review prompt includes:

- original Task context;
- producer Session ID and read-only JSONL path;
- repository/worktree and branch;
- captured Git status, staged diff, and unstaged diff;
- instructions to rerun critical verification;
- the specification's suspicious-behavior checklist.

The producer transcript is evidence, not imported conversational context. Review creation moves the Task to `review` status.

## HTTP and web process

`acpctl serve` opens a disposable `net/http` server on an explicit loopback IP. Server-rendered HTML and embedded CSS/vanilla JavaScript use the same `/api/v1` handlers for mutations and SSE.

The process has no worker shutdown hook. Its context cancellation gracefully closes HTTP connections and the database handle opened by that CLI process; detached workers retain their own processes and database connections.

Browser hardening includes loopback Host enforcement, cross-origin mutation rejection, CSP, frame denial, no-store responses, and output escaping. This is defense in depth for a trusted-user local tool, not a public authentication boundary.

## Failure and recovery rules

- **Viewer exits:** no worker change.
- **Worker exits:** later CLI reconciliation clears stale ownership; JSONL remains replayable.
- **CLI process exits after spawn:** worker continues.
- **Malformed/non-monotonic JSONL:** replay fails closed with an actionable error.
- **Malformed/partial/nonzero acpx output:** the adapter returns a typed protocol/command error; valid prefixes already emitted remain in history.
- **Prompt client disconnects:** worker continues and records the request-correlated response for cursor replay.
- **Native reconciliation fails:** lease and queue remain held.
- **SQLite contention:** busy timeout and per-repository locking protect local concurrency.
- **Existing checkout collision:** fail before a second writer unless explicitly overridden.

## Deliberate V1 boundaries

V1 does not include Kubernetes, a message broker, a permanent API daemon, remote worker routing, public authentication, multi-tenancy, an observability database, a model abstraction, an artifact service, or a provider credential store. A future deployment may replace local transports/persistence while preserving Task, Session, and the versioned API contract.
