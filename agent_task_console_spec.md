# Agent Task Console — Product, UX, Architecture, and Build Specification

**Status:** V1 implementation spec, revised interaction model<br>
**Primary interface:** CLI with interactive Task and Session attachment<br>
**Optional runtime interface:** lightweight local web UI via `agent serve`<br>
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
7. Interactive Task and Session attachment through ACP.
8. Optional exclusive access through a provider's native CLI.

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
> **Attach = how a human observes and steers through ACP.**<br>
> **CLI/web/mobile = views and controls over the same state.**

## 2.2 What the product should feel like

For a developer, it should feel like:

- a cleaner replacement for many tmux panes,
- with persistent agent sessions,
- explicit task grouping,
- live Task and Session consoles,
- raw execution visibility,
- optional native CLI access,
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

acp_session_id
native_session_id

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
agent start "Fix login regression" --agent codex --repo ./product
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

Use a thin wrapper around `acpx`.

Pin the supported `acpx` version for V1 and verify it at startup. `acpx` is pre-1.0, so its command grammar and output contract must be covered by adapter-level fixtures and integration tests.

The product calls `acpx` to:

- start ACP sessions,
- prompt sessions,
- resume sessions,
- stream structured output,
- cancel sessions,
- invoke custom ACP-compatible agents.

The product must hide `acpx` behind one internal package so the rest of the code does not depend on its CLI syntax.

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

Pressing `Esc` in an attached console requests ACP cancellation of the active turn. This is cancellation, not suspension at an exact token or tool-execution position. The Session and its conversation remain resumable, and the steering instruction begins as a new turn.

If the submitting client remains connected, it receives the correlated response live. If it disconnects, work continues, the result is persisted, and the client can replay it from its last stream position after reconnecting.

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
  "native_session_id": "abc123"
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
{"type":"native_acquire"}
{"type":"native_release"}
```

Responses:

```json
{"type":"accepted","request_id":"req-42","state":"queued"}
{"type":"status","status":"working"}
{"type":"line","seq":101,"request_id":"req-42","data":{...}}
{"type":"error","message":"..."}
```

Closing a subscriber connection only detaches that viewer. It must not cancel a turn, stop the worker, or close the ACP Session.

The worker may grant an exclusive native lease only when the active ACP turn has completed or been cancelled. While that lease exists, the worker stays alive and may retain queued prompts, but it must not issue prompts to the same provider-native Session.

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

- `agent task attach`
- `agent session attach`
- `agent session logs --follow`
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
agent session start APP-91 \
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
agent task new "Research Polymarket BTC edge"
```

Output:

```text
Created PM-42
```

List:

```bash
agent tasks
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
agent task PM-42
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
agent task status PM-42 review
```

Attach to the live Task console:

```bash
agent task attach PM-42
```

The Task console shows every Session, current status, latest activity, and an aggregate activity stream. A user may select a Session with the keyboard or mouse and drill into its complete ACP-visible history and live output.

Follow Task activity without an interactive terminal UI:

```bash
agent task logs PM-42 --follow
```

Convenience creation for a one-Session Task:

```bash
agent start "Fix login regression" \
  --agent codex \
  --repo ~/repos/product \
  --role implementer
```

This is shorthand for creating a Task and then its first Session. The persisted model remains Task → Session.

---

## 13.2 Session commands

Start:

```bash
agent session start PM-42 \
  --agent gemini \
  --repo ~/repos/polymarket \
  --role researcher
```

Starting a Session sends an initial prompt constructed from the Task title, Task description, Session role, and repo/worktree context. `--prompt` may append Session-specific instructions.

ACP permissions default to read approval. A write-capable unattended Session must be started with an explicit permission choice such as `--approve-all`; persist that choice in `metadata_json` without adding a new table or first-class object.

Start with worktree:

```bash
agent session start APP-91 \
  --agent codex \
  --repo ~/repos/product \
  --role implementer \
  --worktree
```

Prompt:

```bash
agent session prompt S102 "Check the one-hour horizon too"
```

Attach to the interactive ACP console:

```bash
agent session attach S102
```

Follow logs without an interactive console:

```bash
agent session logs S102 --follow
agent session logs S102 --follow --raw
```

Status:

```bash
agent session S102
```

Cancel:

```bash
agent session cancel S102
```

Resume:

```bash
agent session resume S102
```

Stop worker:

```bash
agent session stop S102
```

Open the provider-native CLI as an explicit exclusive mode:

```bash
agent session attach S102 --native
```

---

# 14. Interactive Attachment and Native Mode

Attachment is the primary way a human observes and steers work. It is a client connection to existing Task/Session state; it is not a new persistent entity and it does not own worker lifetime.

## 14.1 Task attachment

`agent task attach <task-id>` opens an interactive terminal console that:

1. shows all Sessions belonging to the Task,
2. shows agent, role, repo/worktree, status, and latest activity,
3. updates when Sessions are added or change state,
4. accepts keyboard and mouse selection,
5. drills into a selected Session without launching another process,
6. provides controls for prompt, cancel, resume, stop, review, and native mode,
7. returns from Session detail to the Task overview.

A Task is not itself an agent. A message entered at Task scope must target an explicitly selected Session. V1 must not silently broadcast a prompt to every Session.

## 14.2 Session attachment

`agent session attach <session-id>` opens the same Session detail console directly.

The console first replays stored ACP-visible history and then subscribes to live output. It should render agent messages, tool calls, command results, diffs, permission requests, errors, and reasoning information when the provider exposes them through ACP. Raw ACP is available as an advanced toggle.

While an ACP turn is active:

- a submitted steering message is queued as the next prompt,
- `Esc` requests cancellation of the active turn,
- after cancellation, steering starts a new turn in the same Session,
- detaching does not cancel or stop anything.

The complete ACP-visible history is not necessarily the provider's complete hidden internal state. The native runtime transcript remains authoritative for provider-specific details that ACP does not emit.

## 14.3 Detach and response routing

Detaching a Task or Session console closes only that viewer connection.

The active turn continues, JSONL continues to append, and all remaining viewers continue to receive activity. The client that submitted a prompt receives its correlated response while connected. A disconnected client can reconnect using its last stream position and replay the missed response.

This behavior applies equally to terminal, web, mobile, automation, and agent-to-agent callers.

## 14.4 Provider-native mode

`agent session attach <session-id> --native` is an escape hatch for interacting through the provider's native CLI rather than through ACP.

Native mode is exclusive because an ACP worker and a provider-native CLI must not both write to the same native Session concurrently.

Examples:

Codex:

```bash
codex resume <native_session_id>
```

Claude:

```bash
claude --resume <native_session_id>
```

Gemini:

```bash
gemini --resume <native_session_id>
```

Behavior:

1. let the active ACP turn finish or allow the human to cancel it with `Esc`,
2. acquire an exclusive native lease from the worker,
3. mark the Session `waiting`, keep the worker alive, and prevent it from issuing ACP prompts,
4. hold queued ACP prompts without running them,
5. launch the native CLI in the repo/worktree,
6. when the human exits, reconcile/resume the same native Session through the adapter,
7. release the native lease and continue queued ACP prompts only after reconciliation succeeds.

If the provider does not expose a resumable native Session ID or reconciliation fails, leave the Session in `waiting` with an actionable explanation. Never guess that two different provider conversations are the same Session.

Do not allow the automated worker and human CLI to actively write to the same native Session at the same time.

---

# 15. Review Workflow

Review is not a special persistent object.

A review is simply another Session under the same Task.

Convenience command:

```bash
agent session review S201 --agent claude
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
agent serve
```

This starts a disposable local web server.

The server:

- uses the same Task, Session, and SessionStream application services as the CLI,
- reads persisted state through the store layer,
- streams Session output through the worker/log abstraction,
- sends prompts/cancel/resume/stop commands through the Session service.

Web handlers must not contain a second implementation of Task or Session behavior and must not expose Unix socket or raw ACP internals as the client contract.

If `agent serve` exits, Sessions keep running.

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

`agent serve` exposes a small, versioned JSON API used by the local web UI.

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

Raw ACP should remain below this boundary.

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
- no long-lived Python or Node runtime required,
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

    acp_session_id TEXT,
    native_session_id TEXT,

    status TEXT NOT NULL,

    worker_pid INTEGER,
    worker_socket TEXT,
    log_path TEXT,

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
agent session start
       │
       ▼
create Session row
       │
       ▼
create worktree if requested
       │
       ▼
spawn detached:
agent _worker --session S201
       │
       ▼
worker starts acpx / ACP agent
       │
       ▼
persist ACP/native session IDs
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
agent task attach APP-91
       │
       ▼
load Task and every Session
       │
       ▼
subscribe to live Session streams
       │
       ▼
select S201
       │
       ▼
replay stored history, then receive live ACP output
```

If worker is not available:

```text
show stored JSONL history
+
offer agent session resume S201
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
agent session resume S201
       │
       ▼
spawn new detached worker
       │
       ▼
worker asks acpx/runtime to resume native Session
```

Still the same Task Console Session.

No new Run entity.

---

# 22. Startup / Crash Recovery

No permanent daemon is required.

When any `agent` command starts:

1. open SQLite,
2. inspect Sessions marked active,
3. validate worker PIDs/sockets,
4. mark stale Sessions as `stopped` if worker is gone.

Do not automatically restart every agent on boot in V1.

User may run:

```bash
agent session resume S201
```

Later an optional recovery command may be added:

```bash
agent resume --all
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

Persist both:

```text
our Session ID
ACP session ID
native runtime session ID
```

because they are different concepts even when values happen to match.

---

# 24. Product UX Rules

1. **Task is always the top-level screen/object.**
2. Never present an agent Session as if it is the Task itself.
3. Do not expose implementation vocabulary unless useful.
4. Raw ACP details should live under an advanced/debug view.
5. Repo/worktree should always be visible on Session details.
6. `attach` always means an interactive Task or Session console through the gateway and ACP.
7. Provider-native access is an explicit `agent session attach <id> --native` mode.
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

`agent serve` should bind to:

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

A public gateway is a later deployment profile. It requires authentication, TLS, authorization, tenant isolation, secret management, worker isolation, rate limiting, and operational recovery. The local `agent serve` process must not be presented as that production boundary.

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
- [ ] Attach to a Task and see all of its Sessions update live.
- [ ] Select a Session from the Task console with keyboard or mouse.
- [ ] Attach directly to a Session and see stored history followed by live ACP output.
- [ ] Queue a steering prompt while a turn is active.
- [ ] Cancel an active turn with `Esc` and continue in the same Session.
- [ ] Detach without interrupting the active turn.
- [ ] Reconnect from a prior stream position and recover missed output.
- [ ] Correlate a response to the client/request that submitted its prompt.
- [ ] Follow normalized or raw Session logs without an interactive console.
- [ ] Follow aggregate Task logs without an interactive console.
- [ ] Cancel a Session.
- [ ] Resume a stopped Session.
- [ ] Persist ACP/native session IDs.
- [ ] Associate Session with Git worktree.
- [ ] Create isolated worktrees on request.
- [ ] Human can enter exclusive provider-native mode with `agent session attach <id> --native`.
- [ ] ACP and native clients never write to the same native Session concurrently.
- [ ] Exiting native mode reconciles ACP state before queued prompts continue.

## Review

- [ ] Create reviewer Session from producer Session.
- [ ] Reviewer can read producer JSONL.
- [ ] Reviewer can inspect same repo/worktree.
- [ ] Reviewer can inspect Git diff.
- [ ] Reviewer is instructed to independently verify claims.

## Web

- [ ] `agent serve` starts local web UI.
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
10. request correlation and native/ACP IDs.

## Phase 3 — durable workers

11. Detached `_worker` subcommand.
12. multi-subscriber Unix socket protocol.
13. serialized prompt queue.
14. correlated JSONL session logs.
15. stale worker detection.

## Phase 4 — interactive attachment

16. `agent task attach` Task console.
17. `agent session attach` Session console.
18. keyboard/mouse Session selection.
19. history replay followed by live stream.
20. prompt/cancel/resume/stop controls.
21. noninteractive `logs --follow` commands.

## Phase 5 — Git/worktrees and native mode

22. repo validation.
23. optional worktree creation.
24. branch naming.
25. `agent session attach --native` exclusive lease and reconciliation.

## Phase 6 — review helper

26. `agent session review`.
27. transcript + repo evidence prompt.

## Phase 7 — versioned API and optional local web

28. shared Task/Session/SessionStream service boundary.
29. `/api/v1` Task and Session endpoints.
30. reconnectable Task and Session event streams.
31. `agent serve`.
32. Tasks page.
33. Task detail and clickable Session drill-down.
34. stored history plus live Session stream.
35. prompt/cancel/resume/stop.

Do not start Phase 7 before the CLI and terminal attachment flow is pleasant.

---

# 33. Definition of Done for the Product

The product succeeds if one developer can do this:

```bash
agent task new "Investigate BTC Polymarket edge"
```

Then:

```bash
agent session start PM-42 --agent gemini --repo ~/repos/polymarket --role researcher
agent session start PM-42 --agent codex  --repo ~/repos/polymarket --role analyst
```

Close the terminal.

Come back later and attach to the Task:

```bash
agent task attach PM-42
```

See:

```text
PM-42  Investigate BTC Polymarket edge

S101  researcher  Gemini  done
S102  analyst     Codex   working
```

Select `S102` from the Task console or attach directly:

```bash
agent session attach S102
```

Observe the stored ACP-visible history, continue receiving live work, submit a steering prompt, and detach without stopping the Session.

Start review:

```bash
agent session review S102 --agent claude
```

Open the native provider CLI only if needed:

```bash
agent session attach S102 --native
```

And optionally:

```bash
agent serve
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
