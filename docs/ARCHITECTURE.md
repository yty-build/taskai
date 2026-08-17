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
- serializes later ACP prompts in a visible transport-local FIFO;
- rejects ACP admission while an exclusive native owner or pre-launch
  reservation exists; cross-surface work never enters the ACP FIFO;
- gives every prompt a request ID;
- appends every visible transition/output with a monotonic position;
- broadcasts the same correlated lines to multiple subscribers;
- supports prompt, status/ping, exact permission decision, cancel, stop, and subscribe commands;
- before every prompt dispatch at the head of the ACP FIFO, refreshes the idle
  adapter on the exact ACP Session and falls back to exact Resume without
  changing canonical identity;
- clears worker ownership on orderly stop.

Pending permissions are runtime state plus append-only Session journal records,
not another entity or table. A viewer detach has no permission or turn side
effect. The first valid viewer decision wins; cancel/stop/bridge failure
abandons the request safely.

The runtime bridge uses a bounded NDJSON control pipe on child fd 3. Child stdin is reserved for the pinned runtime's own one-shot filesystem/terminal host prompt, allowing structured `client_operation` events to become normal permission cards without mixing provider terminal text into the protocol. Provider-specific restrictive modes/configuration prevent silent internal writes before this boundary.

The spawning CLI is not the worker's parent lifecycle owner. Terminal and web connections may exit without affecting it. Every normal CLI invocation reconciles stale persisted PID/socket state and removes stale runtime files.

Operations against a live worker first negotiate worker protocol v3 and
`exclusive-native-owner-v2`. That capability proves generation-scoped
duplicate-native-process protection and cross-surface writer exclusion. The
compatibility check covers prompts, permission decisions, provider-native
attachment, and Resume when a worker socket is already live. A responsive
incompatible worker receives no provider command and returns a typed upgrade
conflict. Cancel, Stop, status/read, conversation/history, and stream operations
remain available so the user can inspect and migrate safely by stopping and
resuming that Session with the current binary.

## Conversation sources, projection, and reconnect

One canonical provider conversation has three distinct data paths:

| Source | Role |
| --- | --- |
| Verified provider-owned store (Codex/Claude/Grok JSONL; OpenCode SQLite) | Committed, authoritative user-visible conversation history |
| Live ACP | Provisional reasoning, output, tools, permissions, and turn state while work is happening |
| Session JSONL | Gateway lifecycle/audit record, missed-stream recovery, provisional overlay, and explicit fallback |

The provider store may itself be JSONL, SQLite, or another provider format; it
is accessed only through verified provider adapters in `internal/native`. V1
reads Codex, Claude, and Grok provider JSONL plus OpenCode provider SQLite.
Gateway JSONL is not promoted into a canonical transcript merely because a
provider snapshot is temporarily unavailable.

After reconciliation, provider-exposed user, reasoning, and assistant records
are authoritative for the committed turn. Distinct ACP-only reasoning may
remain as explicitly ACP-sourced supplemental context; when the provider has
no readable reasoning, the ACP reasoning remains the visible fallback. During
a gateway-history fallback, all reasoning remains provisional gateway evidence
and is never labelled provider-committed.

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
  projection, then follows ACP from the captured gateway cursor. Every prompt
  additionally performs exact ACP rehydration after reaching the FIFO head and
  before dispatch.
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
          └────────── ACP bridge ───────────────────────────┘
                    same conversation; exclusive writers
                                                           │ process exits/stops
                                                           ▼
                    owner release + provider-store reconciliation
```

When tmux is available, the native CLI runs in a stable per-Session tmux session; detaching the viewer leaves it running. The gateway tags that tmux session with a hash of the canonical state root plus Session identity, explicitly passes the selected `AGENT_HOME` and current `PATH` to defeat stale tmux-server environment, and refuses an unowned name collision. From a gateway console already inside tmux, attachment switches that client to the same stable harness; `Ctrl-B L` returns to the previous console client view. Direct execution is allowed only outside tmux when the terminal is non-interactive or the tmux binary is unavailable; a nested tmux handoff always fails closed rather than launching a second provider.

Native handoff uses a private, generation-scoped lifecycle inside the worker:

```text
none -> reserved(g) -> active(g) -> refresh_pending -> none
                   \-> recovered -> refresh_pending -> none
```

The outer viewer first reserves a fresh opaque generation. The hidden tmux
process, or a direct foreground runner, must atomically activate that exact
generation immediately before provider start and present it again on release.
A delayed process from an older generation cannot activate or release a newer
lifecycle. A collision that never activated remains `reserved` and can be
recovered explicitly only after exact `=agent-<session-id>` tmux absence is
verified. Recovery that revokes the reservation releases that exact generation,
schedules the same provider-store reconciliation as normal native exit, and
retains the freshness marker that forces exact ACP rehydration before the next
dispatch. This is required because the colliding harness may have changed
provider history. If a concurrent safe release wins first, recovery reports
`already_released`. Once
`active`, tmux disappearance does not prove that the provider tree exited, so
the native-harness lifecycle stays fail-closed against creating a replacement
native process or admitting ACP work. Legacy ownership markers are
conservatively treated as exclusive native-harness markers.

Before every ACP prompt, after it reaches the head of the ACP FIFO, the worker
closes and refreshes its idle ACP adapter on the exact persisted ACP Session
ID. If that fails, it performs exact Resume. This also covers provider-store
changes made outside a gateway-managed native harness.
Both paths revalidate ACP, native, and provider identity; successful continuity
emits correlated `acp_rehydrated` (`refreshed` or `resumed`) before
`turn_started`. A failed rehydration sends no prompt through an uncertain
adapter.

Viewer connections are non-exclusive, but native and ACP provider writes are
exclusive. While a native owner or reservation exists, ACP admission returns an
actionable conflict before journal or queue mutation. While ACP is accepted,
queued, preparing, or active, native creation or activation is rejected without
cancelling ACP. Only ACP-to-ACP prompts use the transport FIFO.

Native process exit/stop establishes a durable release barrier and starts exact
provider-store reload and presentation reconciliation. Only after the exact
owner has released may a retried ACP request be admitted; that request still
must complete pre-prompt exact ACP rehydration before dispatch. Presentation
reconciliation may finish independently after release and never weakens writer
exclusion. Pre-prompt ACP rehydration,
prompt-transport failure recovery, and post-turn identity inspection reload
persisted verification state and are time-bounded. An uncertain ACP bridge and its provider process
group may still be terminated and awaited before exact ACP resume; affected
requests receive correlated transport errors. Recovery never substitutes a
different conversation identity.

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
- **New native generation requested while ACP is accepted, queued, preparing, or active:** reject
  the native start with wait-then-open guidance; do not queue it, start a
  provider process, or cancel ACP.
- **Provider snapshot is temporarily unreadable:** an already-open viewer keeps
  its current in-memory presentation and discloses the delayed refresh; an
  initial load uses the bounded gateway-history fallback. Never swap in a
  partial candidate.
- **Post-native provider reload or reconciliation fails:** after exact native
  release, the current validated view is retained and a delayed-history warning
  is recorded. A retried ACP request still performs exact Refresh/fallback
  Resume before dispatch; snapshot presentation failure never permits a second
  native writer.
- **Pre-prompt ACP rehydration fails:** the correlated request receives an
  error and no prompt is sent through the uncertain adapter. If native ownership
  still exists, the request is rejected before admission rather than queued.
- **ACP transport identity recovery fails:** affected requests receive
  correlated errors, the suspect ACP process tree is terminated, and the
  worker stops for explicit exact resume.
- **SQLite contention:** busy timeout and per-repository locking protect local concurrency.
- **Existing checkout collision:** fail before a second writer unless explicitly overridden.

## Deliberate V1 boundaries

V1 does not include Kubernetes, a message broker, a permanent API daemon, remote worker routing, public authentication, multi-tenancy, an observability database, a model abstraction, an artifact service, or a provider credential store. A future deployment may replace local transports/persistence while preserving Task, Session, and the versioned API contract.
