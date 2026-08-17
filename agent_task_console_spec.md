# Agent Task Console — Product, UX, Architecture, and Build Specification

**Status:** V1 implementation spec, revised interaction model<br>
**Primary interface:** CLI with interactive Task and Session attachment<br>
**Optional runtime interface:** lightweight local web UI via `acpctl serve`<br>
**Core model:** Task → many Sessions<br>
**Core execution protocol:** ACP<br>
**Primary design goal:** the smallest reliable control layer over Codex, Claude, Gemini, Grok, OpenCode, and custom ACP-compatible agents without rebuilding capabilities those runtimes already provide.

---

# 1. Product Summary

Agent Task Console is a lightweight local-first control plane for running multiple AI agents across multiple repositories.

The product has only two first-class concepts:

- **Task** — the work that needs to get done.
- **Session** — one agent working on some part of that task.

A Task can contain zero, one, or many Sessions.

Examples:

- A Polymarket research task may contain a Gemini researcher, a Codex analyst, and a Claude reviewer.
- A bug-fix task may contain a Codex implementer and a Claude reviewer.
- A video-generation task may contain a storyboard agent, a custom ACP video agent, and a QA reviewer.
- A deployment task may contain a release agent and a verification agent, while the actual deployment gate remains deterministic code outside the agent.

The system does **not** attempt to become a generic agent framework, workflow engine, chat product, or agent marketplace.

It manages only:

1. Tasks.
2. Sessions.
3. Session lifecycle.
4. Agent invocation through ACP.
5. Repo/worktree association.
6. Live session visibility.
7. A polished Task navigator plus structured ACP views for terminal, web, and mobile clients.
8. The real provider-native harness as the default terminal Session experience.

Everything else remains owned by the systems already good at it:

- Git owns code history, diffs, commits, and worktrees.
- Codex / Claude / Gemini / Grok / OpenCode own their native conversation state and transcripts.
- ACP owns the standardized client ↔ agent interaction.
- Repositories own `AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, skills, and MCP configuration.

---

# 2. Product Philosophy

## 2.1 Core principle

> **Task = what needs to be done.**<br>
> **Session = one agent doing part of it.**<br>
> **ACP = how we control that agent.**<br>
> **Worker = what keeps that session alive.**<br>
> **Repo/worktree = where the work lives.**<br>
> **Attach = how a human returns to the same provider conversation.**<br>
> **CLI/web/mobile = views and controls over the same state.**

## 2.2 What the product should feel like

For a developer, it should feel like:

- a cleaner replacement for many tmux panes,
- with persistent agent sessions,
- explicit task grouping,
- live Task and Session consoles,
- raw execution visibility,
- real provider-native CLI access as the default terminal Session experience,
- no large agent platform.

For an end user, the same Task can later be rendered more simply:

- task status,
- which agents are working,
- what is happening,
- what requires attention,
- final result.

The underlying model remains identical.

## 2.3 Conversation source contract

Every surface presents one provider conversation, but the three available
sources have deliberately different responsibilities:

- The provider-owned conversation store is the committed, authoritative source
  for user-visible conversation history. It is always resolved by the
  Session's canonical provider conversation ID; the gateway never selects a
  transcript by recency. Codex, Claude, and Grok use their verified provider
  JSONL stores; OpenCode uses its verified provider SQLite store. Provider
  adapters normalize those formats; they are not one shared file schema.
- ACP is the live control and provisional streaming path. Structured terminal,
  web, mobile, automation, and agent-to-agent prompts go through the Session
  worker over ACP and appear immediately while the provider is still working.
  ACP is not treated as the durable transcript database.
- Session JSONL is the gateway's append-only lifecycle, correlation,
  permission, audit, and recovery journal. It supplies the live overlay and an
  explicit fallback when the provider store cannot be read, but it does not
  replace the provider transcript as committed conversation history.

Opening a Session therefore means: load a normalized snapshot of the exact
provider conversation, overlay gateway-only lifecycle state plus any ACP
activity not yet represented by that snapshot, and then continue applying live
ACP activity. After an ACP turn settles, and after provider-native activity,
an open structured viewer asks the shared service for a fresh joined projection
and reconciles it with its live overlay in the background.

Activating a write surface has an additional continuity step. At the head of
the ACP FIFO, before every prompt, the worker refreshes its idle adapter on the
exact persisted ACP Session ID, falling back to an exact Resume if refresh
fails; every returned ACP, native, and provider identity must still match
before dispatch. This also covers provider-store changes made outside a
gateway-managed native harness. Starting a new native generation
verifies the provider store and uses `BuildResume` to invoke that provider's
exact ID-based resume command. Neither path selects a transcript by recency or
copies the gateway JSONL into provider memory.

ACP and provider-native continuations target the same canonical provider
conversation, and the gateway keeps those write surfaces exclusive. An open,
attached, or detached native harness owns provider-native mode until its
provider process exits normally. An ACP submission during that interval is
rejected before it is journaled or sent; it is never hidden in a cross-surface
queue. Likewise, native creation or activation fails immediately while ACP
work is accepted, queued, preparing, or running. Neither rejection cancels or
interrupts the surface that already owns the Session.

After native exit, the exact owner releases, provider-history reconciliation
starts, and a durable freshness boundary remains until exact ACP Refresh or
fallback Resume validates the same canonical identity. Only then may the
retried ACP request emit `turn_started` and reach the provider. A fresh native
generation similarly verifies the provider store and uses the exact provider
resume command. The gateway never auto-kills a provider process or silently
changes surfaces.

Reconciliation is an atomic presentation update. It must not blank or visibly
reload the transcript, duplicate a turn, move a reader who has scrolled away
from the live edge, steal keyboard focus, alter a composer draft or cursor,
clear text or row selection, or collapse an expanded reasoning/tool surface.
An ACP item remains visible until an equivalent committed provider item is
present in the replacement snapshot. If a transient refresh fails, an
already-open viewer keeps its current in-memory presentation and gives a
concise delay disclosure. If provider history is unavailable on initial load,
the shared service returns a bounded gateway-history fallback with an explicit
source warning.

Provider snapshots, ACP overlays, reconciliation boundaries, and presentation
revisions are in-memory or transport concepts. They do not add a persistent
Run, Event, Artifact, transcript, or other first-class entity. The persistent
product model remains exactly Task and Session.

---

# 3. Non-Goals

V1 must **not** introduce these unless a concrete feature requires them:

- Run as a first-class object.
- Event table.
- Artifact table.
- Workflow engine.
- Generic memory platform.
- Redis.
- Kafka.
- Celery.
- Temporal.
- Kubernetes.
- LangGraph.
- Compozy.
- Buzz.
- A2A.
- Custom observability database.
- Custom model abstraction layer.
- Custom agent SDK.
- Permanent always-on API server.
- Chat channels or threads.
- Agent marketplace.
- Custom artifact storage system.

If one of these becomes necessary later, add it because of an observed requirement—not because it is fashionable.

---

# 4. Primary Use Cases

## 4.1 Polymarket / quantitative research

Task:

```text
PM-42
Research BTC 15-minute Polymarket pricing edge
```

Sessions:

```text
S101  researcher   Gemini   working
S102  analyst      Codex    working
S103  reviewer     Claude   waiting
```

All sessions may use the same research repo or separate worktrees.

---

## 4.2 Software bug fix

Task:

```text
APP-91
Fix production login regression
```

Sessions:

```text
S201  implementer  Codex    working
S202  reviewer     Claude   waiting
```

Reviewer receives:

- original Task,
- repository/worktree,
- producer Session gateway JSONL audit-log reference,
- current Git diff/commit.

The reviewer independently inspects the work and can rerun tests.

---

## 4.3 Deployment

Task:

```text
APP-94
Deploy release v1.14
```

Sessions:

```text
S211  release-agent     Codex    working
S212  verifier          Claude   waiting
```

Important:

The agent may prepare or recommend deployment actions.

Actual consequential operations should remain behind deterministic code, approvals, scripts, CI/CD, or policy gates.

---

## 4.4 Video creation

Task:

```text
VID-17
Create property walkthrough video
```

Sessions:

```text
S301  storyboard       Gemini
S302  video-renderer   custom-video-agent
S303  qa-reviewer      Claude
```

`custom-video-agent` only needs to be ACP-compatible.

Internally it may use:

- FFmpeg,
- Remotion,
- Kling,
- Seedance,
- image-generation models,
- custom Python,
- asset metadata.

The Task Console does not care how the runtime is implemented.

---

# 5. Domain Model

There are only two persistent first-class entities.

---

## 5.1 Task

A Task is a unit of work that may involve zero, one, or many Sessions.

### Fields

```text
id
title
description
status
created_at
updated_at
```

Optional V1 fields:

```text
external_ref      # GitHub issue / Jira / arbitrary user reference
metadata_json     # escape hatch; avoid schema inflation
```

### Suggested statuses

```text
open
working
review
done
failed
cancelled
```

Task status may be manually set initially.

Later it may be partially derived from Session states.

---

## 5.2 Session

A Session is one persistent agent conversation/context working on exactly one Task.

### Fields

```text
id
task_id

role
agent
provider

repo_path
worktree_path
branch

acpx_record_id
acp_session_id                 # adapter evidence / resume transport
native_session_id              # optional adapter evidence / compatibility
provider_conversation_id       # canonical provider identity used by native attach
conversation_id_source         # acp, native, or provider-store
conversation_id_verified_at

status

worker_pid
worker_socket
log_path

created_at
updated_at
last_activity_at
```

### Suggested statuses

```text
starting
working
waiting
done
failed
cancelled
stopped
```

### Important semantics

A Session is persistent even if:

- the CLI exits,
- an attached terminal console exits,
- the web UI exits,
- the user disconnects,
- the worker is temporarily stopped.

The same Session may later be resumed.

Do **not** model each restart as a separate Run in V1.

Optional requested provider model and reasoning-effort values are immutable
Session configuration stored in `metadata_json`; they are not a model catalog
or another entity. Blank means Provider default. V1 accepts an explicit value
only when the pinned ACP adapter advertises it and the exact provider-native
resume path can preserve it. Public transports expose these as requested
values, not as a guessed effective-provider state.

---

# 6. Relationship Model

```text
Task
 │
 ├── Session
 │     ├── Agent/runtime
 │     ├── ACP session
 │     └── Repo/worktree
 │
 ├── Session
 │     ├── Agent/runtime
 │     ├── ACP session
 │     └── Repo/worktree
 │
 └── Session
       ├── Agent/runtime
       ├── ACP session
       └── Repo/worktree
```

One Task may span many repositories.

Each Session has exactly one primary repo/worktree context.

Each Session belongs to exactly one Task. Do not share one Session across multiple Tasks.

A convenience command may create a Task and its first Session atomically, but this does not change the underlying relationship:

```text
acpctl start "Fix login regression" --agent codex --repo ./product
    │
    └── creates Task
            └── creates one Session
```

---

# 7. Repository Model

Each repo defines its own agent behavior.

Example:

```text
repos/
├── polymarket/
│   ├── AGENTS.md
│   ├── CLAUDE.md
│   ├── GEMINI.md
│   └── .agents/
│       └── skills/
│           ├── research/
│           ├── backtesting/
│           └── skeptical-review/
│
├── product/
│   ├── AGENTS.md
│   └── .agents/
│       └── skills/
│           ├── bug-triage/
│           ├── implementation/
│           └── review/
│
└── video/
    ├── AGENTS.md
    └── .agents/
        └── skills/
            ├── storyboard/
            ├── render/
            └── qa/
```

The Task Console does not own these files.

The agent runtime reads them from the repo.

---

# 8. Agent Registry

Agents should be declarative.

Suggested layout:

```text
~/.agent/agents/
├── codex.yaml
├── claude.yaml
├── gemini.yaml
├── grok.yaml
├── opencode.yaml
└── video-agent.yaml
```

Example:

```yaml
name: codex
provider: openai
command: codex
```

Example custom agent:

```yaml
name: video-agent
provider: custom
command: video-agent
args:
  - --acp
```

V1 only needs:

```text
name
provider
command
args
environment
```

Repo selection belongs to the Session, not the Agent definition.

For built-in `acpx` profiles, `command` may be the friendly profile name such as `codex`, `claude`, `gemini`, `grok-build`, or `opencode`. For a custom agent, `command` and `args` describe the raw ACP-compatible server command. The adapter is responsible for translating either form safely without shell interpolation.

---

# 9. ACP Strategy

## 9.1 V1

Do not implement ACP protocol plumbing directly.

Use one long-lived bridge process per Session worker around the public `acpx/runtime` API. The bridge is an implementation detail inside `internal/acp`; it is not another product entity or a shared daemon. Do not drive `acpx prompt` through a PTY: `acpx` delegates persistent prompts to a detached queue owner whose permission input is not attached to the foreground terminal.

Pin the supported `acpx` version for V1 and verify it at startup. `acpx` is pre-1.0, so its command grammar and output contract must be covered by adapter-level fixtures and integration tests.

The product uses the public runtime to:

- start ACP sessions,
- prompt sessions,
- resume sessions,
- stream structured output,
- cancel sessions,
- invoke custom ACP-compatible agents.

When a Session requests supported provider controls, ACP must first ensure or
exactly resume the conversation and durably persist its acpx/ACP/native
identity. Only then may it set and jointly verify the requested model and
reasoning effort. The worker repeats that configuration after every exact
Refresh or fallback Resume and before a prompt. Configuration failure sends no
prompt, discards the partially configured bridge, marks the saved Session
failed, and exposes only a bounded gateway-owned explanation. Codex supports
advertised model and effort values; Claude supports both but uses blank for its
default effort; OpenCode supports advertised model only; the pinned Grok,
Gemini, and custom paths remain Provider-default-only until both ACP and native
parity can be proved.

Resolve `acpx/runtime` from the installed, pinned package associated with the configured `acpx` binary. Runtime state and the materialized bridge script live under private `AGENT_HOME` state with `0700`/`0600` permissions. Provider authentication remains provider-managed and is never copied into Session options, SQLite, or logs.

The product must hide `acpx` behind one internal package so the rest of the code does not depend on its CLI syntax.

The public runtime emits rich normalized messages, reasoning, tool, status, usage, and permission activity. It does not promise the exact underlying JSON-RPC wire object. The advanced raw/debug view therefore preserves the complete gateway event envelope and normalized runtime payload available to this integration; it must not be described as an exact packet capture. Exact wire access requires a future stable upstream hook or a direct ACP implementation.

Suggested internal interface:

```go
type ACPClient interface {
    Start(ctx context.Context, spec StartSpec) (ACPResult, error)
    Send(ctx context.Context, sessionID string, message string) (PromptReceipt, error)
    Cancel(ctx context.Context, sessionID string, requestID string) error
    Resume(ctx context.Context, sessionID string) error
    Status(ctx context.Context, sessionID string) (ACPStatus, error)
}
```

Do not expose this abstraction to users.

### Prompt and response semantics

Each submitted prompt receives a request ID used for correlation in the live stream and JSONL log. A request ID is operational metadata, not a new `Run` or `Event` entity.

The public ACP runtime supports one active turn per Session. The worker keeps a
FIFO only between ACP prompts: when an ACP turn is already active, later ACP
messages wait for that transport-local limitation. A provider-native owner is
not part of that FIFO: an ACP submission while native mode is active is rejected
before durable admission and must be retried after normal provider exit. At the
head of the ACP FIFO, the worker performs exact ACP rehydration for that prompt.
Successful rehydration records correlated `acp_rehydrated` with state
`refreshed` or `resumed` before `turn_started`. Failure produces a correlated
turn error and sends no prompt through an uncertain adapter.

Pressing `Esc` in the structured ACP console requests ACP cancellation of the active turn when its composer is not active. This is cancellation, not suspension at an exact token or tool-execution position. The Session and its conversation remain resumable, and the steering instruction begins as a new turn. The real provider-native harness owns its own keys.

If the submitting client remains connected, it receives the correlated response live. If it disconnects, work continues, the result is persisted, and the client can replay it from its last stream position after reconnecting.

### Permission semantics

Under `read-approval`, read/search/fetch/thought work may be resolved automatically by the pinned runtime. Write, execute, and unknown ACP `session/request_permission` calls remain pending until a viewer selects one of the exact options offered by the provider. The worker exposes pending choices in status, records requested/resolved/abandoned transitions in Session JSONL, validates Session/request/option identity, and applies first-valid-decision-wins when viewers race.

The adapter must override permissive provider/user defaults when necessary to preserve this contract. In the pinned provider matrix, a read-approval Codex Session uses Codex `read-only` mode, a read-approval Claude Session uses Claude `default` (manual) mode, and OpenCode receives an inline permission override that makes `edit` and `bash` ask while preserving stricter deny rules. These controls prevent provider defaults from executing writes silently.

Detaching a viewer does not resolve, cancel, or time out a permission request. A later viewer sees the same pending card and can decide it. Cancelling the turn, stopping the worker, bridge failure, or stdin/process shutdown cancels pending callbacks and fails closed. `allow_always` and `reject_always` must be visibly persistent choices and are selectable only when the provider offered them.

The public runtime callback covers ACP `session/request_permission`. For the pinned runtime's client-side `fs/write_text_file` and `terminal/create` prompts, the bridge reserves child stdin for the host answer, observes the structured `client_operation` running event, and exposes only gateway-owned `Allow Once` and `Reject` choices. It does not parse provider terminal output or claim a persistent choice the host did not offer. Unknown host permission paths remain fail-closed and must be called out in provider capability tests.

---

## 9.2 Future

If `acpx` becomes limiting:

- replace the adapter with a direct ACP implementation,
- use an official ACP SDK or transport library,
- preserve every higher-level Task and Session API.

No migration of product concepts should be required.

---

# 10. Session Worker

The Session Worker is the only special runtime primitive.

Each active Session owns one tiny detached worker process.

Example:

```text
Task PM-42
│
├── Session S101
│      worker PID 1021
│      └── ACP → Gemini
│
├── Session S102
│      worker PID 1044
│      └── ACP → Codex
│
└── Session S103
       worker PID 1092
       └── ACP → Claude
```

The CLI is **not** the parent that must remain alive.

The worker:

1. starts or resumes the ACP agent,
2. owns the live process/connection,
3. writes session output to JSONL,
4. listens on a Unix domain socket,
5. accepts commands from CLI, terminal consoles, or web server,
6. supports multiple simultaneous viewers,
7. serializes ACP-to-ACP prompts as required by the runtime, rejects ACP
   admission while native ownership exists, and records runtime/provider
   lifecycle state,
8. broadcasts correlated activity to connected viewers,
9. updates Session status in SQLite,
10. survives every viewing client closing.

---

## 10.1 Worker filesystem

```text
~/.agent/
├── agent.db
│
├── workers/
│   ├── S101.json
│   ├── S101.sock
│   ├── S102.json
│   └── S102.sock
│
└── logs/
    ├── S101.jsonl
    └── S102.jsonl
```

Example worker metadata:

```json
{
  "session_id": "S102",
  "pid": 1044,
  "socket": "/home/user/.agent/workers/S102.sock",
  "acp_session_id": "abc123",
  "provider_conversation_id": "abc123"
}
```

The JSON file is worker metadata, not a new first-class entity.

---

## 10.2 Worker socket protocol

Keep it tiny.

Commands:

```json
{"type":"ping"}
{"type":"subscribe","from":100}
{"type":"prompt","request_id":"req-42","message":"Check the one-hour horizon too"}
{"type":"cancel","request_id":"req-42"}
{"type":"status"}
{"type":"tail","from":100}
```

Responses:

```json
{"type":"pong","protocol_version":3,"capabilities":["exclusive-native-owner-v2"]}
{"type":"accepted","request_id":"req-42","state":"ready"}
{"type":"status","status":"working"}
{"type":"line","seq":101,"request_id":"req-42","data":{...}}
{"type":"error","message":"..."}
```

Before Prompt, permission decision, provider-native attachment, or Resume
against an already-live worker can write or spawn, the current client sends
`ping` and requires the current socket protocol version plus
`exclusive-native-owner-v2`. That capability proves generation-scoped
duplicate-native protection and cross-surface writer exclusion. A responsive
incompatible worker fails closed with a
typed upgrade-required result and exact stop/resume instructions; none of the
requested provider input is sent. Cancel, Stop, status/read,
conversation/history, permission-list, and stream operations remain available
so the operator can inspect and migrate the Session safely.
The permission-list response includes a sanitized decision-readiness result.
When a pending request belongs to an older or unverifiable worker, Web/mobile
must keep the request visible, disable Allow/Reject controls, and show the exact
safe migration message rather than presenting a decision that the server will
refuse.

Closing a subscriber connection only detaches that viewer. It must not cancel a turn, stop the worker, or close the ACP Session.

Provider-native continuation and ACP continuation share a canonical provider
conversation identity and an exclusive writer boundary. Native attach does not
cancel an ACP turn, and an ACP submission does not stop or take over a native
process. Instead, the second cross-surface activation is rejected immediately:
ACP is rejected before admission while a native owner or reservation exists,
and native creation or activation is rejected while ACP is accepted, queued,
preparing, or active. Only ACP-to-ACP work uses the FIFO.

The worker represents native-harness startup as a generation-scoped private lifecycle:
`reserved(g) -> active(g) -> none`. The outer viewer obtains
a fresh opaque generation before creating or inspecting the stable tmux
harness. The hidden native process, or direct foreground runner, must atomically
activate that exact generation immediately before provider start and present it
again on release. Old delayed processes cannot activate or release a newer
generation. This lifecycle prevents duplicate native harness creation and
blocks ACP admission until exact native release. An unactivated reservation may
be explicitly recovered only after exact `=agent-<session-id>` tmux absence is
verified. Recovery that wins the reservation race releases the exact generation
and schedules the same freshness and presentation-reconciliation work as normal
native release. If a concurrent safe release wins first, the command reports
`already_released`. An active native-harness marker is never
cleared from tmux absence alone, because that does not prove provider-process
exit and could create a duplicate native process. Legacy ownership state is
treated conservatively and continues to exclude ACP until safely released.
This is worker/runtime state, not a new persistent product
entity.

Before every ACP dispatch, after that request reaches the head of the
ACP-to-ACP FIFO, and only when no native owner or reservation exists, the worker
refreshes the idle ACP adapter on the exact persisted ACP Session and falls
back to exact Resume. The native release freshness marker remains until the
same canonical ACP identity is verified. Read-only provider-transcript
reconciliation is scheduled separately and does not delay dispatch.

Native exit/stop triggers the provider-snapshot reconciliation in Section 11;
the projection is prepared off-screen and published atomically without
resetting any viewer. A transient presentation-history failure retains the
current validated projection. It does not authorize a second native writer or
bypass the independent exact ACP identity check required before dispatch.

Use newline-delimited JSON over Unix sockets.

No gRPC.

No message broker.

---

# 11. Session Logs and Conversation Projection

Each Session gets an append-only JSONL log.

Example:

```text
~/.agent/logs/S102.jsonl
```

This is **not** an Event database or the canonical conversation transcript.

It is the gateway lifecycle/audit journal, live projection overlay, and
provider-history fallback.

Store the raw structured output received from ACP/acpx with minimal normalization.

Purposes:

- request/response correlation,
- queued/active/cancelled/failed turn lifecycle,
- pending and resolved permission state,
- the provisional ACP overlay used by every structured viewer,
- explicit fallback history if the provider snapshot is unavailable,
- `acpctl session logs --follow`
- reviewer inspection
- debugging
- finding suspicious behavior
- auditing commands/tool output
- recovering recent UI history

Each stored record should carry a monotonic stream position and, when
applicable, a request ID. These are fields in the append-only gateway Session
journal, not separate database entities.

Native Codex / Claude / Gemini / Grok / OpenCode transcript storage remains
authoritative for committed provider conversation history. A provider store may
use JSONL, SQLite, or another provider-specific format; no UI may assume that
all providers share one file schema. Each provider adapter reads only the exact
canonical conversation and projects allowlisted user-visible content. It must
not expose provider authentication, hidden internal state, or unrelated
conversations.

The V1 verified implementations read Codex, Claude, and Grok provider JSONL
and OpenCode provider SQLite. Gemini remains on the disclosed gateway fallback
until it has an exact provider-store verifier and reader.

The Task Console should not attempt to replace it.

## 11.1 Source roles

| Source | Role | Viewer behavior |
|---|---|---|
| Provider conversation store (Codex/Claude/Grok JSONL; OpenCode SQLite) | Committed user-visible conversation history | Load first as the base snapshot for the exact canonical provider conversation. |
| ACP worker stream | Live provisional messages, reasoning, tools, plans, permissions, and turn state | Apply immediately as a keyed overlay while work is active. |
| Session JSONL | Gateway lifecycle/audit record, missed-stream recovery, and fallback | Restore gateway-only state, recover provisional activity, and disclose when it is being used instead of a provider snapshot. |
| SQLite | Task/Session identity, status, checkout, and canonical provider identity | Never use it as a conversation body store. |

"Provisional" describes source lifecycle, not inferior UI treatment. Live ACP
content must be readable immediately and may include the best information
available while the provider has not committed its transcript. Once a refreshed
provider snapshot contains the equivalent turn, the projection replaces the
duplicate ACP conversation fragments while retaining gateway-only lifecycle,
permission, and diagnostic facts.

## 11.2 Initial load

A structured Session load uses the shared service to:

1. Load the Session and verify its canonical provider conversation identity.
2. Read and normalize a bounded snapshot of that exact provider-owned
   conversation.
3. Capture the gateway stream high-water position while reading Session JSONL
   and derive gateway-only lifecycle state plus ACP activity not represented in
   the provider snapshot.
4. Reduce the two sources into one semantic conversation projection off the
   render path.
5. Publish the projection once and subscribe after the returned gateway stream
   position so no activity is lost or duplicated across the load boundary.

If the Session has not yet acquired a provider conversation identity, the
provider has not committed its first turn, or a verified provider snapshot is
temporarily unavailable, loading must still succeed from gateway JSONL/live ACP
when possible. The viewer labels this state, for example:

- `Live ACP · provider history not committed yet`,
- `Gateway history fallback · provider transcript unavailable`, or
- `Provider history refresh delayed · current view retained`.

A provider without a verified store adapter, including a V1 custom ACP command
and Gemini until its store contract is implemented, uses the disclosed gateway
fallback. The gateway must not guess a provider-store location or apply another
provider's parser.

The disclosure is visible but does not replace the transcript with an error
screen. A new Session with no provider or gateway conversation activity shows a
specific empty state rather than implying that a completed transcript exists.

## 11.3 Background reconciliation

Reconcile after every settled ACP turn (completed, cancelled, or failed when
the provider may have persisted output) and after provider-native activity.
Native process exit/stop requests reconciliation. Detaching a viewer alone does
not stop a still-running native process or release its exclusive ownership.
After exact native release, reconciliation is a presentation and history
operation, not the ACP write-continuity gate; ACP dispatch waits for its own
exact Refresh/fallback Resume and identity validation, not for the read-only
snapshot result.

Reconciliation must:

1. read and normalize a fresh provider snapshot without mutating the active
   presentation,
2. combine it with gateway-only records and still-uncommitted ACP activity,
3. reject a partial, malformed, wrong-conversation, or wrong-checkout snapshot,
4. atomically publish the bounded replacement projection only after it passes
   validation, and
5. leave an already-open viewer's current in-memory projection in place and
   surface a bounded diagnostic if any step fails.

The replacement uses stable presentation keys rather than array position so
clients can preserve the selected Session/tool, transcript anchor and live-tail
mode, keyboard focus, composer value and cursor, text selection, pending
approval focus, and expanded/collapsed reasoning and tool state. A reader at
the live edge continues following; a reader inspecting older content remains
anchored and receives a new-activity indicator. Reconciliation must never show
an intermediate empty transcript or force a page/console reload.

---

# 12. Git / Worktrees

Git owns work products.

The product must not create a separate Artifact model.

For coding Sessions:

```text
Session
  ├── repo_path
  ├── worktree_path
  └── branch
```

Recommended behavior:

If `--worktree` is requested:

```bash
acpctl session start APP-91 \
  --agent codex \
  --repo ~/repos/product \
  --role implementer \
  --worktree
```

create something like:

```text
~/.agent/worktrees/APP-91-S201/
```

with branch:

```text
agent/APP-91/S201
```

Two Sessions should not modify the same checkout concurrently unless explicitly requested.

---

# 13. CLI Product UX

Binary name:

```bash
agent
```

The CLI is the primary V1 interface.

---

## 13.1 Task commands

Create:

```bash
acpctl task new "Research Polymarket BTC edge"
```

Output:

```text
Created PM-42
```

List:

```bash
acpctl tasks
```

Output:

```text
ID       STATUS    TITLE
PM-42    working   Research Polymarket BTC edge
APP-91   review    Fix login regression
VID-17   done      Create property walkthrough
```

Show:

```bash
acpctl task PM-42
```

Output:

```text
PM-42  Research Polymarket BTC edge
Status: working

SESSION   ROLE         AGENT    STATUS
S101      researcher   gemini   working
S102      analyst      codex    working
S103      reviewer     claude   waiting
```

Set status:

```bash
acpctl task status PM-42 review
```

Attach to the live Task console:

```bash
acpctl task attach PM-42
```

The Task console shows every Session, current status, latest activity, and an
aggregate activity stream. Keyboard navigation or a mouse click selects a
Session; Enter opens its real provider-native harness. The explicit ACP action
opens the reconciled provider snapshot plus gateway overlay and continues with
live provisional ACP output.

Follow Task activity without an interactive terminal UI:

```bash
acpctl task logs PM-42 --follow
```

Convenience creation for a one-Session Task:

```bash
acpctl start "Fix login regression" \
  --agent codex \
  --repo ~/repos/product \
  --role implementer
```

This is shorthand for creating a Task and then its first Session. The persisted model remains Task → Session.

---

## 13.2 Session commands

Start:

```bash
acpctl session start PM-42 \
  --agent codex \
  --repo ~/repos/polymarket \
  --role researcher \
  --model gpt-5.6-sol \
  --reasoning-effort high
```

Starting a Session sends an initial prompt constructed from the Task title, Task description, Session role, and repo/worktree context. `--prompt` may append Session-specific instructions.

`--model` and `--reasoning-effort` are optional exact provider values. Leaving
either blank preserves that provider's default. The Web creation and reviewer
forms and the terminal New Session wizard expose the same bounded controls;
unsupported combinations fail before Session creation. Because provider
catalogs are account- and adapter-dependent, V1 does not invent a hard-coded
cross-provider model abstraction.

ACP permissions default to read approval. A write-capable unattended Session must be started with an explicit permission choice such as `--approve-all`; persist that choice in `metadata_json` without adding a new table or first-class object.

Start with worktree:

```bash
acpctl session start APP-91 \
  --agent codex \
  --repo ~/repos/product \
  --role implementer \
  --worktree
```

Prompt:

```bash
acpctl session prompt S102 "Check the one-hour horizon too"
```

Attach to the provider's native harness for the same conversation:

```bash
acpctl session attach S102
```

Open the structured ACP console explicitly:

```bash
acpctl session attach S102 --acp
```

Follow logs without an interactive console:

```bash
acpctl session logs S102 --follow
acpctl session logs S102 --follow --raw
```

Status:

```bash
acpctl session S102
```

Cancel:

```bash
acpctl session cancel S102
```

Resume:

```bash
acpctl session resume S102
```

Stop worker:

```bash
acpctl session stop S102
```

---

# 14. Interactive Attachment and Native/ACP Views

Attachment is the primary way a human observes and steers work. It is a client connection to existing Task/Session state; it is not a new persistent entity and it does not own worker lifetime.

## 14.1 Task attachment

`acpctl task attach <task-id>` opens an interactive terminal console that:

1. shows all Sessions belonging to the Task,
2. shows agent, role, repo/worktree, status, and latest activity,
3. updates when Sessions are added or change state,
4. accepts keyboard and mouse selection,
5. opens the selected Session in its real provider-native harness on Enter,
6. provides an explicit ACP view plus controls for prompt, cancel, resume, and stop,
7. returns from Session detail to the Task overview.

The terminal Task console is a compact developer cockpit, not another agent
harness. At normal widths it uses one cohesive two-pane surface: a scannable
Session navigator and a larger selected-Session preview separated by a single
shared seam. A slim header shows Task identity and status; attention is visible
in Session status and selected-Session context. Each Session row pairs its
status color with a glyph and word, gives agent/role priority over opaque IDs,
and keeps status/activity context aligned for scanning. The preview emphasizes
latest activity, checkout, canonical-conversation readiness, and the primary
actions: `Enter` opens the real provider CLI, `a` opens the structured
conversation view, and `p` targets a message through ACP. Normal conversation
rows use human labels; protocol/source provenance belongs in details or the
explicit raw view, not a repeated third pane or every transcript line.

Narrow terminals use a stacked navigator/preview. At the supported minimum of
40 columns by 16 rows, the console keeps a useful Session navigator and its
core Native, ACP, New Session, and Detach actions instead of requiring a desktop
viewport. Smaller terminals receive an actionable size warning while Session
work continues. Keyboard interaction is complete; mouse support is additive.
Status is never conveyed by color alone, and the core UI does not require a
Nerd Font or truecolor.

A single action registry drives keyboard shortcuts, the footer, help, filter,
and a searchable substring command palette. Unavailable native continuation is
labelled explicitly, and invoking an action that is not valid for the current
Session produces an actionable notice. Returning from a native harness restores
the Task console, its selected Session, filter, scroll position, and other
disposable viewer state.

A Task is not itself an agent. A message entered at Task scope must target an explicitly selected Session. V1 must not silently broadcast a prompt to every Session.

## 14.2 Session attachment

`acpctl session attach <session-id> --acp` opens the structured ACP Session console directly. Pressing `n` from this console hands the terminal to the same Session's exact provider-native CLI; the contextual footer and help always expose this action, or label it unavailable with a reason. Web and mobile Session selection always opens this same structured ACP view because those clients cannot embed a local provider terminal.

The console loads the reconciled projection defined in Section 11: committed
provider history first, gateway lifecycle/uncommitted ACP activity as an
overlay, and then the live ACP stream from the captured position. It renders
agent messages, tool calls, command results, diffs, permission requests,
errors, and reasoning information when the provider exposes them. Reasoning
segments stay in event order and are visible while streaming. When a turn
completes, the last assistant segment may be labeled as the completed response;
this is a presentation inference because ACP does not promise a separate
final-answer channel. If no reasoning segment was exposed, the UI states that
explicitly. A raw/debug gateway-envelope view is available as an advanced
toggle; with the public runtime bridge it is not an exact JSON-RPC packet
capture and it does not replace the committed provider snapshot.

The Structured ACP console is conversation-first. A compact identity/status
line sits above a transcript rendered directly on the terminal background.
User and agent turns use distinct gutters. Successful tool calls collapse to a
one-line summary; running and failed calls retain their status and bounded
preview; detailed tool output and reasoning use progressive disclosure through
the `d` and `o` controls. The latest plan summary may stay pinned without
duplicating every plan update. Only modal decisions, especially a pending
permission, receive a prominent bordered shelf above the multi-line composer.

Live streaming follows only while the reader is at the bottom. Scrolling away
freezes the viewport and shows a new-activity/return-to-live control instead of
moving text underneath the reader. Background reconciliation uses the same
keyed update path, so it preserves scroll anchor, focus, draft/cursor, text or
row selection, selected tool, and expanded reasoning/tool cards. An initial
gateway-history fallback or delayed refresh uses the disclosures in Section 11
rather than a blank screen or full console reload.

Export to native scrollback or a configured pager/editor is a post-V1 terminal
enhancement. V1 retains trusted raw gateway inspection through
`acpctl session logs <id> --raw` and the provider-native harness.

While an ACP turn is active:

- another submitted ACP message is queued as the next ACP prompt in the
  worker's transport-local FIFO; this queue is unrelated to native-process
  presence,
- `Esc` requests cancellation of the active turn,
- after cancellation, steering starts a new turn in the same Session,
- detaching does not cancel or stop anything.

Messages submitted from structured terminal, web, or mobile surfaces always go
through the Session worker over ACP and stream provisionally. Input typed in a
provider-native harness goes directly through that provider. Both paths target
the same canonical provider conversation, but only one may own provider writes.
An ACP submission while native mode is active is rejected before admission;
the operator exits native normally and retries. Native creation or activation
while ACP is accepted, queued, preparing, or running is likewise rejected
without cancelling ACP. Once the active surface releases, the next surface
loads the latest exact provider conversation before accepting input.

While a native process is running, structured viewers keep the last validated
provider snapshot plus their live ACP overlay. They do not invent live ACP
frames for native-only work. The provider's real harness is the live native
view; structured viewers receive committed native turns through background
reconciliation. Native ownership must release before an ACP request can be
accepted. After release, presentation reconciliation is atomic but distinct
from the exact ACP rehydration required before dispatch.

When a permission card is pending, numbered choices map directly to the exact offered option IDs. Persistent `allow_always`/`reject_always` choices are visually distinct and require confirmation. Closing the console leaves the agent waiting; attaching again restores the actionable card.

The normalized provider snapshot is limited to user-visible committed history
and may still omit hidden provider state. ACP can expose provisional reasoning,
tool, or lifecycle detail that the provider transcript does not persist, which
is why the gateway overlay remains part of the projection. Neither structured
surface attempts to expose hidden provider internals.

## 14.3 Detach and response routing

Detaching a Task or Session console closes only that viewer connection.

The active turn continues, JSONL continues to append, and all remaining viewers continue to receive activity. The client that submitted a prompt receives its correlated response while connected. A disconnected client can reconnect using its last stream position and replay the missed response.

Detaching from a provider-native viewer likewise leaves a tmux-backed native
process running and preserves its exclusive native ownership. ACP submissions
are rejected until that provider process exits normally. Exit releases the
exact owner and starts provider-snapshot reconciliation; the retried ACP
request must then complete exact adapter Refresh or fallback Resume before it
can dispatch.

When the gateway console itself runs inside tmux, native attachment switches
the current tmux client to that same stable `agent-<session-id>` harness rather
than starting a second provider process. `Ctrl-b L` returns to the console;
`Ctrl-b d` detaches the viewer while the provider continues.

This behavior applies equally to terminal, web, mobile, automation, and agent-to-agent callers.

## 14.4 Provider-native mode

`acpctl session attach <session-id>` opens the provider's real native CLI and resumes the Session's canonical provider conversation. `--native` is accepted as a compatibility alias. This is the default terminal experience; the gateway does not attempt to reproduce Codex, Claude, Grok, or OpenCode's native harness.

Before any native process starts, attachment requires a compatible worker
protocol and exact canonical provider identity verification. A worker from an
older incompatible binary fails closed with exact stop/resume migration
commands; it must never be treated as compatible merely because its socket
still answers ping. After the native harness already exists, every additional
outer viewer is attach-only: it may attach or switch to the verified stable
tmux harness and must never fall back to direct execution or create a
replacement provider process.

If a tmux name collision strands only the pre-launch reservation, the local
operator may run
`acpctl session recover-native <session-id>`. The command has no force mode: it
requires a current worker, a still-reserved exact generation, and verified
absence of `=agent-<session-id>`, then releases that exact reservation and
starts provider reconciliation. Recovery refuses an
active or uncertain native-harness marker
even when tmux is gone, because that condition could still represent a live
provider process. This native-harness safety check also blocks ACP admission.
An owner-tag mismatch alone must never instruct the operator to kill the tmux
Session because a stale viewer may be observing a legitimate newer generation;
the operator returns to the console, inspects status, and retries. Recovery is
appropriate only after the worker reports a pre-launch reservation and the
exact harness is independently known absent.

Examples:

Codex:

```bash
codex resume <provider_conversation_id>
```

Claude:

```bash
claude --resume <provider_conversation_id>
```

Grok:

```bash
grok --resume <provider_conversation_id>
```

OpenCode:

```bash
npx -y opencode-ai@1.18.18 --session <provider_conversation_id>
```

Gemini remains available through structured ACP in V1. Native attachment stays disabled until an exact Gemini provider-store verifier is implemented.

Behavior:

1. resolve the canonical provider conversation ID persisted for the Session,
2. verify the exact ID exists in the provider's local store and belongs to the same repo/worktree,
3. attach to the existing verified native harness or reserve creation of one
   stable native harness while acquiring exclusive provider-native ownership,
4. launch the documented native resume command in that repo/worktree when no
   harness already exists,
5. preferably host it in a tmux session derived from the Task Console Session
   ID so terminal detach/reattach preserves the native process and its
   conversation,
6. reject ACP admission while the provider-native process remains active,
7. on native process exit/stop, release the exact owner and start provider
   snapshot reconciliation; before the retried ACP request dispatches, exactly
   Refresh or resume the persisted ACP Session and verify identity.

If the canonical ID is missing, cannot be verified, or the provider cannot resume it, fail with an actionable explanation. Never choose the newest provider transcript, guess that two IDs represent the same conversation, or silently open a replacement conversation.

Several viewers may observe the Session at once, but only one write surface may
own provider input. Closing a Task, ACP, web, or mobile viewer never stops a
worker or cancels a turn. Detaching a native viewer also leaves its provider
process and exclusive ownership active until normal provider exit.

Native start while ACP is accepted, queued, preparing, or running fails
immediately with wait-then-open guidance. This creates no native queue, sends no
provider command, and never cancels ACP. ACP admission while native ownership
exists likewise fails immediately with exit-and-retry guidance and sends no
provider input.

---

# 15. Review Workflow

Review is not a special persistent object.

A review is simply another Session under the same Task.

Convenience command:

```bash
acpctl session review S201 --agent claude
```

Equivalent to creating a new Session:

```text
Task: APP-91
Role: reviewer
Agent: claude
Repo/worktree: same as S201
```

The generated reviewer prompt should include:

1. original Task title/description,
2. producer Session ID,
3. producer Session JSONL path as read-only gateway lifecycle/audit evidence,
4. repo/worktree,
5. current Git branch/diff,
6. instruction to independently rerun critical verification.

The reviewer must inspect both:

- **what the agent produced**, and
- **how the producing agent behaved**.

The reviewer should explicitly look for:

- ignored test failures,
- selective tests presented as complete tests,
- failed commands that were ignored,
- claims unsupported by tool output,
- suspicious shortcuts,
- unrelated file changes,
- repeated broken approaches,
- security regressions.

Do not import the producer's conversation into the reviewer as the reviewer's
own Session. It is read-only evidence and does not change either Session's
provider conversation identity.

The reviewer gets read-only access to evidence.

---

# 16. Web UX

Web is optional and should not change the architecture.

Command:

```bash
acpctl serve
```

This starts a disposable local web server.

The server:

- uses the same Task, Session, and SessionStream application services as the CLI,
- reads persisted state through the store layer,
- streams Session output through the worker/log abstraction,
- sends prompts/cancel/resume/stop commands through the Session service.

Web handlers must not contain a second implementation of Task or Session behavior and must not expose Unix socket or raw ACP internals as the client contract.

If `acpctl serve` exits, Sessions keep running.

---

## 16.1 Web technology

Keep V1 minimal:

- Go `net/http`
- server-rendered HTML
- minimal vanilla JavaScript
- Server-Sent Events for reconnectable Task and Session output
- no React required for V1
- no Node build required for V1

Later a React, remote web, or mobile client may use the same versioned JSON endpoints and event-stream semantics.

---

## 16.2 Web screens

### Tasks

```text
TASKS

PM-42   Research BTC/Polymarket edge      ● Working
        3 sessions

APP-91  Fix login regression              ● Review
        2 sessions

VID-17  Create property walkthrough       ✓ Done
        4 sessions
```

### Task detail

```text
┌ Tasks / Sessions ───────┬────────────────────────────────────────┐
│ APP-91                  │ Codex · Implementer       ● Working    │
│ Fix login regression    │                          ✓ Verified     │
│                         ├────────────────────────────────────────┤
│ Sessions                │ You                                    │
│ ● Codex · implementer   │ Fix the login regression and test it.  │
│ ○ Claude · reviewer     │                                        │
│                         │ C  I found the regression in the       │
│ + New Session           │    session refresh path.               │
│                         │                                        │
│                         │    › Reasoning                          │
│                         │    ✓ Read auth middleware               │
│                         │    ● Run tests · working                │
│                         │                                        │
│                         ├────────────────────────────────────────┤
│                         │ Approval required                      │
│                         │ [Allow once] [Reject]                  │
│                         │                                        │
│                         │ Message Codex…                  [Send] │
└─────────────────────────┴────────────────────────────────────────┘
```

The browser is a conversation-first structured ACP client over the same Task/Session state as CLI. The selected Session owns the main canvas. User and assistant turns are primary. A compact per-turn invocation strip identifies ACP, provider-native, or safely unknown origin and shows lifecycle, completed elapsed time when known, logical tool count, reasoning availability, and approval state without rendering raw transport chunks. Provider-exposed reasoning streams in event order inside a collapsible surface that is open by default; successful tools, plan detail, IDs, checkout paths, and diagnostics use progressive disclosure. Actionable permissions are pinned above the sticky composer and must not disappear when the transcript scrolls.

Desktop keeps Task/Session navigation visible. Mobile uses a focus-managed navigation drawer and one full-height conversation pane, but retains response cancel/resume controls and an explicit Send action. Streamed updates preserve reader position, expanded tool/reasoning state, focus, and text selection. The transcript and approval shelf expose live-region/author semantics for assistive technology.

Initial browser load receives the same provider-snapshot-plus-gateway-overlay
projection as the terminal console, followed by live provisional ACP updates.
After a turn or native process finishes, the browser fetches one validated
semantic replacement and swaps it into its local presentation rather than
reloading the page. The browser preserves its stable-block scroll anchor,
live-follow mode, navigation and tool selection, keyboard focus, composer
draft/cursor, DOM text selection, and disclosure state across that replacement. If provider history
cannot be loaded initially, the conversation remains usable from the bounded
gateway fallback. A transient refresh failure keeps the current in-memory view
and shows the delayed-refresh disclosure from Section 11.

The public browser projection is semantic, sanitized, and cumulatively bounded. It coalesces assistant chunks, merges tool updates, replaces plans, and exposes only safe labels plus opaque IDs needed for display or a permission decision. Provider/worker errors, commands derived from raw input, raw tool objects, structured environment/auth fields, ACP envelopes, sockets, native identifiers, and JSONL paths stay below the browser boundary. Allowlisted provider-visible text, diffs, and terminal output remain visible and can contain sensitive repository or tool-produced content; the local browser is a same-trusted-user surface, not a secret scanner. Trusted forensic inspection remains a terminal `--raw` operation.

No channels.

No threads.

No separate chat ontology.

---

# 17. Local HTTP API and Future Client Boundary

`acpctl serve` exposes a small, versioned JSON API used by the local web UI.

Do not create a separate API service.

The API is a transport over the same application services used by the CLI and terminal console. Response objects are stable API DTOs, not serialized SQLite rows.

Suggested endpoints:

```text
GET    /api/v1/tasks
POST   /api/v1/tasks
GET    /api/v1/tasks/{id}
GET    /api/v1/tasks/{id}/sessions
GET    /api/v1/tasks/{id}/stream

GET    /api/v1/sessions/{id}
GET    /api/v1/sessions/{id}/conversation
POST   /api/v1/tasks/{id}/sessions

POST   /api/v1/sessions/{id}/prompt
GET    /api/v1/sessions/{id}/permissions
POST   /api/v1/sessions/{id}/permissions/decision
POST   /api/v1/sessions/{id}/cancel
POST   /api/v1/sessions/{id}/resume
POST   /api/v1/sessions/{id}/stop

GET    /api/v1/sessions/{id}/stream
GET    /healthz
```

Prompt submission returns a request ID. Task and Session streams carry monotonic positions and request IDs so a client can correlate responses and reconnect without losing output. Server-Sent Events should support `Last-Event-ID` or an equivalent cursor.

Session and reviewer creation accept optional `model` and
`reasoning_effort`. Session DTOs return the non-secret requested values as
`requested_model` and `requested_reasoning_effort`. If provider configuration
fails only after the exact conversation identity has been persisted, creation
still returns the created failed Session with an allowlisted
`session_start_failed` warning; clients must open that resource rather than
creating a duplicate. Raw provider configuration errors never cross the API.

`GET /api/v1/sessions/{id}/conversation` exposes the shared semantic
conversation projection rather than making HTTP clients join provider files
and gateway JSONL themselves. The
initial projection contains the normalized provider snapshot plus its derived
gateway overlay and a captured live-stream cursor. Live stream messages add
provisional ACP activity. A successful reconciliation publishes an atomic
projection replacement or stable-keyed equivalent; its opaque revision is
transport metadata, not a persistent Event or transcript entity. Failed initial
provider loading publishes a bounded gateway fallback; a transient refresh
failure updates an already-open viewer's warning without exposing a provider
path or canonical provider conversation ID.

This allows future:

- browser UI,
- mobile client,
- automation,
- external client portals.

The product contract is Task/Session.

Raw ACP and worker transport details should remain below this boundary. A permission DTO may expose only the sanitized title/kind plus exact opaque permission and option IDs required for an authorized decision.

V1 binds this API to loopback and does not claim to be safe for public exposure. A future public gateway may expose the same versioned contract over authenticated HTTPS without changing the Task/Session model.

---

# 18. Technology Stack

## V1 language

**Go**

Reasons:

- single binary,
- easy subprocess management,
- easy Unix sockets,
- easy SQLite,
- easy optional HTTP server,
- easy cross-platform distribution,
- no shared application daemon; each worker may own the pinned Node runtime bridge required by `acpx`,
- good fit for local systems tooling.

---

## V1 dependencies

Keep dependencies minimal.

Required categories:

1. SQLite driver.
2. YAML parser for agent registry.
3. Bubble Tea as the focused Go terminal UI framework, with compatible Charm
   components/styling where they reduce hand-written viewport, composer, help,
   and ANSI behavior.
4. Optional small CLI helper library if useful.

Avoid adding a framework merely for aesthetics.

The terminal redesign stays in Go and does not require a Rust/ratatui rewrite.
Its quality comes from information architecture, semantic style tokens,
component focus, responsive layout, and stable streaming behavior. Preserve the
repository's declared Go compatibility; treat any future Bubble Tea major
upgrade that raises the Go baseline as a separate platform decision rather than
mixing it into the cockpit redesign.

Prefer standard library for:

- HTTP,
- sockets,
- process management,
- JSON,
- logging,
- templates.

---

# 19. SQLite Schema

Suggested initial schema:

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

    role TEXT,
    agent TEXT NOT NULL,
    provider TEXT,

    repo_path TEXT,
    worktree_path TEXT,
    branch TEXT,

    acpx_record_id TEXT,
    acp_session_id TEXT,
    native_session_id TEXT,
    provider_conversation_id TEXT,
    conversation_id_source TEXT,
    conversation_id_verified_at TEXT,

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

No other tables in V1.

---

# 20. Suggested Source Layout

```text
agent/
├── cmd/
│   └── agent/
│       └── main.go
│
├── internal/
│   ├── task/
│   │   ├── model.go
│   │   └── service.go
│   │
│   ├── session/
│   │   ├── model.go
│   │   ├── service.go
│   │   ├── worker.go
│   │   └── socket.go
│   │
│   ├── acp/
│   │   └── acpx.go
│   │
│   ├── agents/
│   │   └── registry.go
│   │
│   ├── git/
│   │   └── worktree.go
│   │
│   ├── store/
│   │   └── sqlite.go
│   │
│   ├── console/
│   │   ├── task.go
│   │   └── session.go
│   │
│   ├── api/
│   │   ├── server.go
│   │   └── stream.go
│   │
│   └── web/
│       └── templates/
│
├── migrations/
│   └── 001_init.sql
│
└── README.md
```

Do not split into microservices.

---

# 21. Worker Lifecycle

## Start

```text
acpctl session start
       │
       ▼
create Session row
       │
       ▼
create worktree if requested
       │
       ▼
spawn detached:
acpctl _worker --session S201
       │
       ▼
worker starts acpx / ACP agent
       │
       ▼
persist acpx/ACP evidence and canonical provider conversation ID
       │
       ▼
status = working
```

---

## CLI exits

```text
CLI exits
   │
   └── no effect

worker stays alive
agent stays alive
session continues
```

---

## Attach

```text
acpctl task attach APP-91
       │
       ▼
load Task and every Session
       │
       ▼
subscribe to live Session streams
       │
       ▼
select S201 (keyboard or single click)
       ├── Enter ──► verify canonical provider ID
       │             └── attach/create stable real native harness
       │
       └── `a` / ACP action ──► provider snapshot + gateway overlay
                                └── receive live provisional ACP output
```

If the provider snapshot is not available:

```text
show gateway JSONL fallback with a visible disclosure
+
continue live ACP when the worker is available
```

If the worker is not available:

```text
load a fresh bounded provider snapshot plus gateway JSONL overlay when available
+
otherwise show the bounded gateway JSONL fallback with a disclosure
+
offer acpctl session resume S201
```

If the user detaches while work is active:

```text
viewer disconnects
       │
       └── no effect on worker or ACP turn

worker completes the turn
JSONL stores the correlated result
provider store commits the conversation
reconnecting viewer loads a fresh joined projection and follows after its returned cursor
```

---

## Resume

If worker is stopped:

```text
acpctl session resume S201
       │
       ▼
spawn new detached worker
       │
       ▼
worker asks acpx/runtime to resume the exact persisted ACP Session
```

Still the same Task Console Session.

No new Run entity.

---

# 22. Startup / Crash Recovery

No permanent daemon is required.

When any `acpctl` command starts:

1. open SQLite,
2. inspect Sessions marked active,
3. validate worker PIDs/sockets,
4. mark stale Sessions as `stopped` if worker is gone.

Do not automatically restart every agent on boot in V1.

User may run:

```bash
acpctl session resume S201
```

Later an optional recovery command may be added:

```bash
acpctl resume --all
```

---

# 23. IDs

Human-readable IDs are preferred.

Example prefixes:

```text
Task:
PM-42
APP-91
VID-17

Session:
S201
S202
```

Implementation may use ULIDs internally while exposing short prefixes.

Avoid making provider-native session IDs the product IDs.

Persist these identities:

```text
our Session ID
acpx record ID
ACP session ID
canonical provider conversation ID
identity source and last verified time
```

The canonical provider conversation ID is the only ID used for subsequent provider-native continuation. ACP/native adapter IDs remain evidence and migration fields because they are different protocol concepts even when their values happen to match. Once a Session has a canonical identity, no attach path may replace it implicitly.

---

# 24. Product UX Rules

1. **Task is always the top-level screen/object.**
2. Never present an agent Session as if it is the Task itself.
3. Do not expose implementation vocabulary unless useful.
4. Bounded sanitized diagnostics may live under an advanced browser view; exact raw ACP/runtime detail remains a trusted local terminal-log operation.
5. Repo/worktree should always be visible on Session details.
6. Task attachment is a gateway navigator; selecting a Session opens the provider-native harness by default.
7. Direct terminal Session attachment is native by default; `acpctl session attach <id> --acp` opens the structured ACP console used by web/mobile.
8. Detaching a viewer must never stop a worker or active turn.
9. Reviewer Sessions should be visually distinct from producer Sessions.
10. A user should be able to understand the current state without reading raw logs.
11. A developer should be able to inspect bounded sanitized browser diagnostics and opt into trusted local raw terminal logs when suspicious.
12. Never require a terminal or web UI to keep Sessions alive.
13. A Task-scoped message must target a selected Session; never silently broadcast.
14. Keyboard interaction must always work; mouse selection is an additional convenience.
15. Structured Session load starts from the exact provider-owned committed
    snapshot, overlays gateway lifecycle/uncommitted ACP activity, and then
    streams live ACP; never present gateway JSONL as the provider's canonical
    transcript without a fallback disclosure.
16. Post-turn and post-native reconciliation is background and atomic. It must
    preserve scroll/follow state, focus, draft/cursor, selection, and expanded
    disclosure state without a visible blank/reload.
17. ACP and provider-native writes are exclusive. The second cross-surface
    activation is rejected rather than queued; after release, the selected
    surface loads the latest exact canonical conversation before accepting
    input.
18. The terminal cockpit remains Go/Bubble Tea, is usable without mouse,
    truecolor, or Nerd Fonts, and remains actionable at 40x16 or larger.

---

# 25. Security / Permissions

V1 is local-first.

Default assumptions:

- current OS user owns the processes,
- Unix sockets are user-readable only,
- SQLite and logs live under `~/.agent`,
- no public network exposure by default.

ACP permissions should default to read approval. Unattended write-capable execution requires an explicit user choice such as `--approve-all`. Permission choices must be visible on Session details and must never be inferred merely because the UI is noninteractive.

Interactive permission decisions must be authorized against the current Session, active request, pending permission ID, and exact offered option. Multiple viewers use first-valid-decision-wins semantics. Raw tool input is escaped and bounded in terminal views; provider environment and authentication values are never logged.

`acpctl serve` should bind to:

```text
127.0.0.1
```

by default.

Remote exposure is a later opt-in feature.

If later exposed remotely:

- require authentication,
- use TLS,
- add per-client and per-Task authorization,
- isolate worker processes and repositories,
- encrypt credentials and secrets,
- add rate limits, quotas, audit controls, and backups,
- use production persistence appropriate to the deployment topology,
- do not expose raw shell access to normal customers.

Changing the bind address of the V1 local server is not sufficient to create a safe public service.

---

# 26. Client Exposure Later

The same architecture can support:

```text
CLI
Web
Mobile
Customer portal
Automation
```

All interact with:

```text
Task
Session
```

They must use the same application-service behavior and versioned API semantics:

```text
CLI / terminal console / web / mobile / automation
                       │
                       ▼
             TaskService / SessionService / SessionStream
                       │
             ┌─────────┴─────────┐
             ▼                   ▼
      persistent store      worker transport
                                  │
                                  ▼
                                 ACP
```

In local V1 the CLI may call these services in-process, SQLite is the store, and Unix sockets are the worker transport. A future public deployment may use authenticated HTTPS, a server database, and remote worker routing without changing the Task/Session product contract.

Do not expose the internal worker or ACP protocol directly as the public product API.

For customer-facing use:

```text
Customer
   │
   ▼
Task API
   │
   ▼
Task / Sessions
   │
   ▼
ACP agents
```

Customers may receive simplified visibility while developers retain raw trace/terminal access.

A public gateway is a later deployment profile. It requires authentication, TLS, authorization, tenant isolation, secret management, worker isolation, rate limiting, and operational recovery. The local `acpctl serve` process must not be presented as that production boundary.

---

# 27. Observability Philosophy

Do not build a tracing platform in V1.

Use:

```text
native provider transcript for committed conversation history
+
Session JSONL for lifecycle/audit/fallback
+
Git history
```

This provides enough forensic evidence for early use.

Later, if requirements appear such as:

- cross-task cost analysis,
- anomaly detection,
- agent performance metrics,
- failed-command queries,
- automated supervision,

then normalize logs into tracing/observability infrastructure.

Until then, do not add it.

---

# 28. Why There Is No `Run`

A Session may stop and resume multiple times.

Example:

```text
S201
  starts
  machine restarts
  resumes
```

V1 treats all of this as the same Session.

A separate Run entity only becomes necessary if the product later needs:

- restart-level billing,
- attempt-level metrics,
- failure accounting,
- precise execution-attempt analytics.

Do not model it before then.

---

# 29. Why There Is No `Event`

ACP emits events/activity.

V1 writes them to:

```text
Session JSONL
```

They are Session activity, not a business entity.

Only create an Event table if the product later needs cross-session querying and analytics.

---

# 30. Why There Is No `Artifact`

For code work:

```text
Git + filesystem
```

already own outputs.

For video/data/report work:

the Session may record output paths in `metadata_json` or the Task description/result initially.

Only introduce a first-class Artifact model when files themselves require:

- ownership,
- versioning,
- permissions,
- lifecycle,
- cross-task reuse.

---

# 31. Acceptance Criteria

V1 is complete when all of these work.

## Core

- [ ] Create a Task.
- [ ] List Tasks.
- [ ] View one Task.
- [ ] Start multiple Sessions under one Task.
- [ ] Associate each Session with an agent and repo.
- [ ] Start Codex through ACP/acpx.
- [ ] Start Claude through ACP/acpx.
- [ ] Start Gemini through ACP/acpx.
- [ ] Start Grok through ACP/acpx.
- [ ] Start OpenCode through ACP/acpx.
- [ ] Start a custom ACP-compatible command.
- [ ] Session keeps running after CLI exits.
- [ ] Prompt an existing Session.
- [ ] Starting a Session sends Task, role, and repo/worktree context as its initial prompt.
- [ ] Optional model/reasoning controls are persisted only on the Session,
      applied after exact identity persistence, jointly verified before the
      first prompt, and reapplied after exact Refresh/Resume.
- [ ] Unsupported provider controls fail before Session creation; a
      post-identity configuration failure preserves and returns the failed
      Session, sends no prompt, and exposes no raw provider detail.
- [ ] Supported provider-native resume carries the same requested controls;
      Provider-default-only agents reject explicit overrides.
- [ ] Unattended write access requires an explicit permission choice.
- [ ] A write/execute permission remains pending across viewer detach and can be decided after reattach.
- [ ] Concurrent permission decisions produce exactly one accepted provider response.
- [ ] Cancel/stop/bridge failure abandons pending permission safely and executes no denied write.
- [ ] Attach to a Task and see all of its Sessions update live.
- [ ] Select a Session from the Task console with keyboard or mouse.
- [ ] Attach directly to a Session and resume the exact provider conversation in its native harness.
- [ ] Use `session attach --acp` and see the exact provider snapshot plus
      gateway overlay followed by live provisional ACP output.
- [ ] When the provider store is unavailable, see usable gateway JSONL fallback
      history with an explicit disclosure rather than a blank or falsely
      complete transcript.
- [ ] Queue a steering prompt while a turn is active.
- [ ] Cancel an active turn with `Esc` and continue in the same Session.
- [ ] Detach without interrupting the active turn.
- [ ] Reconnect from a prior stream position and recover missed output.
- [ ] Correlate a response to the client/request that submitted its prompt.
- [ ] Follow normalized or raw Session logs without an interactive console.
- [ ] Follow aggregate Task logs without an interactive console.
- [ ] Cancel a Session.
- [ ] Resume a stopped Session.
- [ ] Persist a canonical provider conversation ID plus ACP/acpx identity evidence.
- [ ] Associate Session with Git worktree.
- [ ] Create isolated worktrees on request.
- [ ] Enter on a Task Session and `session attach <id>` both open the real provider-native harness.
- [ ] Codex, Claude, and Grok load committed history from verified provider
      JSONL; OpenCode loads it from verified provider SQLite; all resume the
      exact verified conversation ID with no recency fallback.
- [ ] Native attach does not cancel an ACP turn; while native ownership exists,
      an ACP prompt is rejected before admission with exit-and-retry guidance.
- [ ] New native creation/activation while ACP is accepted, queued, preparing,
      or running fails with
      wait-then-open guidance, launches nothing, is not queued, and does not
      cancel ACP.
- [ ] ACP Refresh or fallback Resume rehydrates the exact persisted ACP Session
      before every dispatch at the head of the ACP FIFO, and records
      `acp_rehydrated` before `turn_started`.
- [ ] Detaching a native viewer preserves a still-running native process and
      its exclusive ownership; process exit/stop releases the exact owner and
      starts provider-history reconciliation, and the retried ACP request must
      complete exact ACP Refresh/fallback Resume before dispatch.
- [ ] ACP/native simultaneous writes are impossible through gateway transports;
      the second surface receives an actionable conflict and sends nothing.
- [ ] Post-turn and post-native reconciliation loads committed provider history and
      atomically reconciles it with the ACP/JSONL overlay without duplication,
      blanking, or visible reload.
- [ ] Terminal reconciliation preserves selected Session/tool, scroll anchor
      and follow state, focus, composer draft/cursor, selection, and expanded
      reasoning/tool state.

## Review

- [ ] Create reviewer Session from producer Session.
- [ ] Reviewer can read producer JSONL.
- [ ] Reviewer can inspect same repo/worktree.
- [ ] Reviewer can inspect Git diff.
- [ ] Reviewer is instructed to independently verify claims.

## Web

- [ ] `acpctl serve` starts local web UI.
- [ ] Web lists Tasks.
- [ ] Web shows Task Sessions.
- [ ] Web Session and reviewer creation expose optional bounded model and
      reasoning-effort controls, with blank meaning Provider default.
- [ ] Selecting a web Session shows the same provider snapshot, gateway
      overlay/fallback, and live ACP activity as terminal attachment.
- [ ] Web streams live Session output.
- [ ] Web can send a prompt.
- [ ] Web can cancel/resume/stop.
- [ ] Web renders a conversation-first semantic transcript with bounded assistant, reasoning, tool, plan, permission, and diagnostic state.
- [ ] Streaming preserves scroll anchor, expanded disclosure state, focus, and text selection.
- [ ] Background projection replacement preserves live-follow mode, composer
      draft/cursor, navigation/tool selection, and DOM text selection without
      flicker or a page reload.
- [ ] Mobile retains named cancel/resume controls, a focus-managed Task/Session drawer, and explicit Send behavior.
- [ ] Transcript authors and approval changes are announced through accessible semantics.
- [ ] Browser projection omits provider/worker errors, raw tool objects, commands derived from raw input, structured environment/auth fields, ACP envelopes, and runtime paths; allowlisted tool text/output is explicitly treated as trusted-user sensitive content.
- [ ] Stopping web server does not stop Sessions.

## API and future clients

- [ ] `/api/v1` exposes Task and Session DTOs rather than database rows.
- [ ] Session creation accepts optional provider controls and DTOs expose only
      their requested values; partial-created failure responses are explicit
      and safe to follow without duplicate creation.
- [ ] Task streams aggregate activity from all Sessions.
- [ ] Session streams expose monotonic replay positions and request IDs.
- [ ] Session reads expose one semantic provider-snapshot-plus-overlay
      projection, and reconciliation publishes an atomic replacement without
      exposing provider paths or identities.
- [ ] A disconnected client can reconnect without losing completed output.
- [ ] CLI, terminal console, API, and web use the same application services.
- [ ] Raw worker sockets and ACP protocol details remain below the client boundary.
- [ ] V1 binds only to loopback and clearly rejects unsupported public exposure.

---

# 32. Implementation Order

Build in this order.

## Phase 1 — CLI + Task/Session persistence

1. Go binary.
2. SQLite migrations.
3. Task commands.
4. Session persistence and basic commands.
5. one-Session Task convenience command.
6. Agent registry.

## Phase 2 — ACP

7. pinned `acpx` adapter and compatibility fixtures.
8. Start ACP Session.
9. Prompt queue, cancel, and resume.
10. request correlation and canonical provider conversation identity.

## Phase 3 — durable workers

11. Detached `_worker` subcommand.
12. multi-subscriber Unix socket protocol.
13. serialized ACP-to-ACP prompt queue plus exclusive native-owner admission;
    the second cross-surface activation is rejected, never queued.
14. correlated JSONL lifecycle/audit/overlay/fallback journal.
15. stale worker and stale native-harness lifecycle detection.

## Phase 4 — interactive attachment

16. Go/Bubble Tea `acpctl task attach` developer cockpit with responsive
    navigator/preview, contextual actions, and native-default handoff.
17. conversation-first `acpctl session attach --acp` structured Session
    console.
18. keyboard/mouse Session selection.
19. source-agnostic semantic projection fixtures: provider snapshot + gateway
    overlay/fallback followed by live provisional ACP.
20. prompt/cancel/resume/stop controls.
21. noninteractive `logs --follow` commands.

## Phase 5 — Git/worktrees and native mode

22. repo validation.
23. optional worktree creation.
24. branch naming.
25. verified exact-conversation provider-store snapshot adapters for Codex,
    Claude, Grok, and OpenCode.
26. canonical provider identity verification and stable native-by-default
    attachment with exclusive cross-surface ownership and safe reservation
    recovery.
27. exact pre-prompt ACP Refresh/fallback Resume plus atomic post-turn and
    post-native snapshot reconciliation initiated at the native release boundary.

## Phase 6 — review helper

28. `acpctl session review`.
29. gateway JSONL audit-log + repo evidence prompt.

## Phase 7 — versioned API and optional local web

30. shared Task/Session/SessionStream and conversation-projection service
    boundary.
31. `/api/v1` Task and Session endpoints.
32. reconnectable Task and Session activity streams plus atomic projection
    replacement.
33. `acpctl serve`.
34. Tasks page.
35. Task detail and clickable Session drill-down.
36. provider snapshot plus gateway overlay/fallback and live Session stream.
37. no-flicker reconciliation preserving scroll/focus/draft/selection/disclosure
    state.
38. prompt/cancel/resume/stop with visible queued request state.

Do not start Phase 7 before the CLI and terminal attachment flow is pleasant.

---

# 33. Definition of Done for the Product

The product succeeds if one developer can do this:

```bash
acpctl task new "Investigate BTC Polymarket edge"
```

Then:

```bash
acpctl session start PM-42 --agent gemini --repo ~/repos/polymarket --role researcher
acpctl session start PM-42 --agent codex  --repo ~/repos/polymarket --role analyst
```

Close the terminal.

Come back later and attach to the Task:

```bash
acpctl task attach PM-42
```

See:

```text
PM-42  Investigate BTC Polymarket edge

S101  researcher  Gemini  done
S102  analyst     Codex   working
```

Select `S102` from the Task console or attach directly to its real provider harness:

```bash
acpctl session attach S102
```

To observe the committed provider snapshot with its gateway overlay/fallback,
continue receiving live provisional ACP work, or submit an ACP steering prompt
explicitly:

```bash
acpctl session attach S102 --acp
```

Start review:

```bash
acpctl session review S102 --agent claude
```

And optionally:

```bash
acpctl serve
```

to inspect/control the same Tasks and Sessions through the versioned local API in a browser.

If this works cleanly, the V1 architecture is sufficient.

---

# 34. Final Architectural Rule

When tempted to add another primitive, first ask:

> **Can this be represented as a Task, a Session, the Session's repo/worktree, or the Session's existing runtime history?**

If yes, do not add another abstraction.

The product should stay deliberately small.

```text
Task
  └── Sessions
        ├── detached worker ──► gateway JSONL lifecycle/audit/overlay/fallback
        │        │
        │        └── ACP FIFO ──► ACP live provisional path
        │                        └── provider runtime
        ├── native harness ──► same canonical provider conversation
        ├── provider-owned committed conversation store
        └── repo/worktree
```

Everything else is optional.
