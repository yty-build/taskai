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
- producer session transcript/log reference,
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

A Session has at most one active ACP turn. If a human or another client submits a message while a turn is active, the worker queues it as the next prompt. ACP does not guarantee injection into an already-running turn.

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
7. serializes active and queued prompts,
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
{"type":"refresh"}
```

Responses:

```json
{"type":"accepted","request_id":"req-42","state":"queued"}
{"type":"status","status":"working"}
{"type":"line","seq":101,"request_id":"req-42","data":{...}}
{"type":"error","message":"..."}
```

Closing a subscriber connection only detaches that viewer. It must not cancel a turn, stop the worker, or close the ACP Session.

Provider-native attachment does not acquire a worker lease. It must not pause or cancel an ACP turn, change Session state, or hold the prompt queue. A `refresh` request made after native activity reconnects immediately when the worker is idle, or defers until work already queued at that boundary completes. Prompts submitted after the boundary remain behind refresh. Refresh and post-turn inspection reload persisted identity verification; uncertain transport is terminated and awaited before bounded exact recovery. If recovery fails, later queued prompts are rejected with correlated errors and the worker stops rather than using an unproven conversation. Cross-surface concurrency warnings remain advisory because the user owns coordination.

Use newline-delimited JSON over Unix sockets.

No gRPC.

No message broker.

---

# 11. Session Logs

Each Session gets an append-only JSONL log.

Example:

```text
~/.agent/logs/S102.jsonl
```

This is **not** an Event database.

It is session scrollback / forensic history.

Store the raw structured output received from ACP/acpx with minimal normalization.

Purposes:

- `acpctl task attach`
- `acpctl session attach --acp`
- `acpctl session logs --follow`
- reviewer inspection
- debugging
- finding suspicious behavior
- auditing commands/tool output
- recovering recent UI history

Each stored record should carry a monotonic stream position and, when applicable, a request ID. These are fields in append-only session history, not separate database entities.

Native Codex / Claude / Gemini / Grok / OpenCode transcript storage remains authoritative for provider-specific conversation history.

The Task Console should not attempt to replace it.

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

The Task console shows every Session, current status, latest activity, and an aggregate activity stream. Selecting a Session with Enter or the mouse opens its real provider-native harness; the explicit ACP action opens its complete ACP-visible history and live output.

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
  --agent gemini \
  --repo ~/repos/polymarket \
  --role researcher
```

Starting a Session sends an initial prompt constructed from the Task title, Task description, Session role, and repo/worktree context. `--prompt` may append Session-specific instructions.

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
6. provides an explicit ACP view plus controls for prompt, cancel, resume, stop, and review,
7. returns from Session detail to the Task overview.

The terminal Task console uses a two-pane Session navigator and selected-Session preview at normal widths, with a stacked layout for narrow terminals. A single action registry drives keyboard shortcuts, contextual footer hints, help, and command-palette entries. Filtering and a command palette make larger Tasks quick to navigate. Returning from a native harness restores the Task console.

A Task is not itself an agent. A message entered at Task scope must target an explicitly selected Session. V1 must not silently broadcast a prompt to every Session.

## 14.2 Session attachment

`acpctl session attach <session-id> --acp` opens the structured ACP Session console directly. Web and mobile Session selection always opens this same structured ACP view because those clients cannot embed a local provider terminal.

The console first replays stored ACP-visible history and then subscribes to live output. It should render agent messages, tool calls, command results, diffs, permission requests, errors, and reasoning information when the provider exposes them through ACP. A raw/debug gateway-envelope view is available as an advanced toggle; with the public runtime bridge it is not an exact JSON-RPC packet capture.

While an ACP turn is active:

- a submitted steering message is queued as the next prompt,
- `Esc` requests cancellation of the active turn,
- after cancellation, steering starts a new turn in the same Session,
- detaching does not cancel or stop anything.

When a permission card is pending, numbered choices map directly to the exact offered option IDs. Persistent `allow_always`/`reject_always` choices are visually distinct and require confirmation. Closing the console leaves the agent waiting; attaching again restores the actionable card.

The complete ACP-visible history is not necessarily the provider's complete hidden internal state. The native runtime transcript remains authoritative for provider-specific details that ACP does not emit.

## 14.3 Detach and response routing

Detaching a Task or Session console closes only that viewer connection.

The active turn continues, JSONL continues to append, and all remaining viewers continue to receive activity. The client that submitted a prompt receives its correlated response while connected. A disconnected client can reconnect using its last stream position and replay the missed response.

This behavior applies equally to terminal, web, mobile, automation, and agent-to-agent callers.

## 14.4 Provider-native mode

`acpctl session attach <session-id>` opens the provider's real native CLI and resumes the Session's canonical provider conversation. `--native` is accepted as a compatibility alias. This is the default terminal experience; the gateway does not attempt to reproduce Codex, Claude, Grok, or OpenCode's native harness.

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
3. launch the documented native resume command in that repo/worktree,
4. preferably host it in a tmux session derived from the Task Console Session ID so terminal detach/reattach preserves the native process,
5. leave any active ACP turn and queued prompts untouched,
6. after native exit, request a best-effort ACP refresh at the next idle boundary.

If the canonical ID is missing, cannot be verified, or the provider cannot resume it, fail with an actionable explanation. Never choose the newest provider transcript, guess that two IDs represent the same conversation, or silently open a replacement conversation.

The gateway may display that ACP and native clients could both be active, but it does not enforce a lock. Concurrent writes are a provider/user concern: the user is responsible for avoiding conflicting conversations when the provider does not serialize them safely.

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
3. producer session log path,
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

Do not import the producer's conversation into the reviewer as the reviewer's own Session.

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
TASK: APP-91
Fix login regression

SESSIONS

Codex · Implementer              ● Working
product repo · S201

Claude · Reviewer                ○ Waiting
product repo · S202

CURRENT ACTIVITY

17:41  searched auth middleware
17:42  ran pytest
       3 failed
17:44  modified session.py
17:45  running tests...

[ Message ] [ Cancel ] [ Resume ] [ Review ]

[ Show raw execution ]
```

This is the same Task/Session data as CLI.

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
3. One focused terminal UI library for keyboard, mouse, and streaming console behavior.
4. Optional small CLI helper library if useful.

Avoid adding a framework merely for aesthetics.

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
select S201
       ├── Enter/click ──► verify canonical provider ID ──► real native harness
       │
       └── ACP action ──► replay stored history, then receive live ACP output
```

If worker is not available:

```text
show stored JSONL history
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
reconnecting viewer resumes from its last stream position
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
4. Raw ACP details should live under an advanced/debug view.
5. Repo/worktree should always be visible on Session details.
6. Task attachment is a gateway navigator; selecting a Session opens the provider-native harness by default.
7. Direct terminal Session attachment is native by default; `acpctl session attach <id> --acp` opens the structured ACP console used by web/mobile.
8. Detaching a viewer must never stop a worker or active turn.
9. Reviewer Sessions should be visually distinct from producer Sessions.
10. A user should be able to understand the current state without reading raw logs.
11. A developer should be able to inspect the raw execution when suspicious.
12. Never require a terminal or web UI to keep Sessions alive.
13. A Task-scoped message must target a selected Session; never silently broadcast.
14. Keyboard interaction must always work; mouse selection is an additional convenience.

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
Session JSONL log
+
native runtime transcript
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
- [ ] Unattended write access requires an explicit permission choice.
- [ ] A write/execute permission remains pending across viewer detach and can be decided after reattach.
- [ ] Concurrent permission decisions produce exactly one accepted provider response.
- [ ] Cancel/stop/bridge failure abandons pending permission safely and executes no denied write.
- [ ] Attach to a Task and see all of its Sessions update live.
- [ ] Select a Session from the Task console with keyboard or mouse.
- [ ] Attach directly to a Session and resume the exact provider conversation in its native harness.
- [ ] Use `session attach --acp` and see stored history followed by live ACP output.
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
- [ ] Codex, Claude, Grok, and OpenCode resume the exact verified conversation ID; no recency fallback is used.
- [ ] Native attach never pauses/cancels ACP or holds queued prompts, and any concurrency warning is advisory.
- [ ] Exiting native mode requests a nonblocking ACP refresh at the next idle boundary.

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
- [ ] Selecting a web Session shows the same stored and live history as terminal attachment.
- [ ] Web streams live Session output.
- [ ] Web can send a prompt.
- [ ] Web can cancel/resume/stop.
- [ ] Stopping web server does not stop Sessions.

## API and future clients

- [ ] `/api/v1` exposes Task and Session DTOs rather than database rows.
- [ ] Task streams aggregate activity from all Sessions.
- [ ] Session streams expose monotonic replay positions and request IDs.
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
13. serialized prompt queue.
14. correlated JSONL session logs.
15. stale worker detection.

## Phase 4 — interactive attachment

16. `acpctl task attach` Task console.
17. `acpctl session attach --acp` structured Session console.
18. keyboard/mouse Session selection.
19. history replay followed by live stream.
20. prompt/cancel/resume/stop controls.
21. noninteractive `logs --follow` commands.

## Phase 5 — Git/worktrees and native mode

22. repo validation.
23. optional worktree creation.
24. branch naming.
25. canonical provider identity verification and native-by-default attachment.

## Phase 6 — review helper

26. `acpctl session review`.
27. transcript + repo evidence prompt.

## Phase 7 — versioned API and optional local web

28. shared Task/Session/SessionStream service boundary.
29. `/api/v1` Task and Session endpoints.
30. reconnectable Task and Session event streams.
31. `acpctl serve`.
32. Tasks page.
33. Task detail and clickable Session drill-down.
34. stored history plus live Session stream.
35. prompt/cancel/resume/stop.

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

To observe stored ACP-visible history, continue receiving live ACP work, or submit an ACP steering prompt explicitly:

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
        └── detached worker
              └── ACP
                    └── Codex / Claude / Gemini / Grok / OpenCode / custom agent
                          └── repo/worktree
```

Everything else is optional.
