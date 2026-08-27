# Agent Task Console

Agent Task Console is a small local control plane for persistent AI-agent work. It groups provider-owned agent conversations under Tasks and exposes each one through a live Session Channel that terminal and browser clients can observe and control.

The product has exactly two persistent entities:

- **Task** — the outcome to produce.
- **Session** — one addressable agent conversation doing part of a Task.

Everything else is runtime composition. A worker, provider Port, stream frame, request ID, permission request, or viewer connection is not another product object.

## The core primitive

Every managed Session has one logical channel:

```text
commands in                  ordered journal frames out
-----------                  --------------------------
prompt                       connection
cancel                       client_command
permission_response          provider_event
configure                    provider_result
                             permission_requested
                             permission_resolved
                             permission_abandoned
                             worker_error
```

`stop` is a Session lifecycle action: it stops the worker and closes the Port. It is not a fifth channel command, and there is no separate `close` command or API.

An accepted prompt is journaled and sent as the exact caller-supplied text. The gateway adds no hidden Task/role envelope and keeps no second display-message version.

One live provider Port owns that channel. The worker calls `Start` only for a Session without a provider reference and calls exact `Resume` only when a worker or provider transport starts again. A healthy Port stays connected across turns; there is no per-prompt refresh or transcript reconstruction.

The channel becomes addressable before that provider connection is ready. The worker opens its private socket and journal, emits `connection` with `data.state == "connecting"`, and accepts subscribers before it invokes the one `Start` or exact `Resume`. Create and Resume therefore return the Session in `starting`, without provider options. After `connection.data.state == "connected"`, the retained Port is ready for provider-dependent commands. Startup is bounded to 30 seconds, and Stop can cancel it.

Provider activity is forwarded immediately as `provider_event` journal frames. Each frame's `data` is the normalized runtime event object verbatim, including event shapes the gateway does not recognize. The outer journal does not translate those events into invented semantic frame types. The gateway does not crawl provider-private transcript stores, determine whether a turn is “committed,” merge two histories, or infer whether work came from an ACP or native surface.

Session JSONL stores the ordered frames that crossed the channel so a viewer can replay missed output and operators can inspect lifecycle evidence. It is not provider memory and is never fed back into an agent as conversation history.

## Requirements

- Go 1.23+
- Node.js 22.13+ for the pinned `acpx/runtime` bridge
- `acpx` 0.13.0
- at least one independently authenticated ACP-compatible provider CLI

The embedded registry includes Codex, Claude, Gemini, Grok, OpenCode, and custom ACP command profiles. Provider authentication remains provider-owned and is never copied into the database or logs.

## Install

```bash
npm install --global --prefix "$HOME/.local" acpx@0.13.0
export PATH="$HOME/.local/bin:$PATH"

git clone https://github.com/yty-build/acp-gateway.git
cd acp-gateway
make build VERSION=1.0.0
install -m 0755 bin/acpctl "$HOME/.local/bin/acpctl"

acpctl version
acpctl agents
```

`acpx` is pre-1.0, so the adapter probes the installed version and rejects an incompatible runtime. `ACPX_BINARY=/absolute/path/to/acpx` may select an explicit installation.

Runtime state defaults to `~/.agent`. Use an absolute private directory to override it:

```bash
export AGENT_HOME=/absolute/private/path/agent-state
```

## Quick start

Create a Task:

```bash
acpctl task new "Fix login regression" \
  --description "Reproduce, repair, and verify the production issue" \
  --external-ref APP-91
```

Create a Task and first Session together:

```bash
acpctl start "Fix login regression" \
  --agent codex \
  --repo /absolute/path/to/product \
  --role manager \
  --worktree
```

Add another Session under the Task:

```bash
acpctl session start <task-id> \
  --agent claude \
  --repo /absolute/path/to/product \
  --role worker \
  --worktree
```

A Session is created in `starting` as soon as its channel can be attached. A client first subscribes and observes the replayable `connection` frame whose `data.state` is `connecting`, then waits for the one whose state is `connected`. Provider-options, configure, prompt, cancel, and permission-response operations return typed `session_starting` until that boundary; they are not queued. Terminal and web clients then read the provider's currently advertised model and reasoning choices from the live Port and render them as selectors; they never embed a provider catalog. This provider-options read is observational and is not a journal frame. `configure` completes only after the worker applies the selection to the live Port. Blank leaves that control's current value unchanged; on a fresh Port that value is the provider default. The gateway does not persist or reapply model/effort selections, so a resumed Port's advertised current values are authoritative.

Inspect and control work:

```bash
acpctl tasks
acpctl task <task-id>
acpctl session <session-id>
acpctl session prompt <session-id> "Check the failing edge case"
acpctl session cancel <session-id>
acpctl session stop <session-id>
acpctl session resume <session-id>
```

Only one turn may be active in a Session. A second prompt receives a busy response and can be retried after the active turn settles; the gateway does not hide it in a FIFO.

## Attachment

```bash
acpctl task attach <task-id>
acpctl session attach <session-id>
```

Task attachment shows every child Session. Structured Session attachment subscribes to the Session Channel, replays frames after the requested sequence, and follows new journal frames. Multiple viewers may attach safely.

A fresh viewer gets a declared, byte-bounded tail of at most 256 frames and then live output; it does not scan or download the whole audit first. Reconnecting with a saved nonzero sequence resumes exactly.

Detaching a terminal, closing a browser, losing a network connection, or stopping `acpctl serve` affects only that viewer. It does not stop the worker, close the provider Port, cancel the active turn, or decide a permission request.

A provider’s native CLI remains useful in a normal terminal, but it is provider-owned and unmanaged by Agent Task Console. It is not opened as a second writer to an existing managed Session. The structured terminal uses the same Session Channel as web and automation.

## Manager and children

A Task is the team boundary. An optional `parent_session_id` on Session expresses the complete persisted manager/child relationship:

```text
Task
└── Manager Session
    ├── Child Session A
    └── Child Session B
```

In V1, this relationship is controlled by a human/operator through the API, web, or terminal surfaces. An operator can create and list direct child Sessions, then prompt or cancel one exact same-Task child through its ordinary Session Channel. Each child has its own stream; the gateway does not wait for a child or route its result into the parent provider.

V1 does not inject `create_child`, `send_child`, `wait_child`, `cancel_child`, or `list_children` tools into a provider harness. A provider-facing manager adapter, including any wait/result mapping, is an explicit post-V1 integration over the same Session services. Mailboxes, peer-to-peer messaging, offline delivery, broadcast, and peer discovery are likewise post-V1 external integrations, such as Buzz or Agent Mail, and do not change the Task/Session model.

## Web and API

```bash
acpctl serve
# Agent Task Console listening at http://127.0.0.1:8080
```

The browser attaches the Session stream immediately, including while provider startup is still in progress. It waits for the `connection` frame whose `data.state` is `connected` before reading provider options or enabling provider-dependent controls. It then renders each `provider_event.data` object as soon as the provider emits it and derives any assistant text, reasoning, tools, permissions, or turn presentation locally.

The current browser transport uses HTTP commands plus an SSE response stream. Together they implement one logical bidirectional Session Channel. A future WebSocket can carry the same command and frame envelopes without changing services or domain objects.

The API is rooted at `/api/v1`; `/healthz` provides local health. Public frame envelopes contain `session_id`, `seq`, optional `request_id`, `timestamp`, `type`, and `data`; sequence is monotonic within a Session. See [docs/API.md](docs/API.md).

## Logs

```bash
acpctl session logs <session-id>
acpctl session logs <session-id> --follow
acpctl session logs <session-id> --raw --from 42
acpctl task logs <task-id> --follow
```

Logs are channel replay and audit records. They may contain sensitive provider-visible repository content. They are not canonical transcripts, are not parsed together with provider files, and are never used to rebuild an agent’s memory.

## Permissions

Sessions default to `read-approval`. Typed ACP write, execute, and unknown permission requests remain pending until a viewer selects an exact provider-offered option. A pending request survives viewer detach, and concurrent decisions use first-valid-decision-wins. Untyped terminal prompts fail closed; the gateway never parses provider status text into a permission card.

Use unattended write approval only when intentional. Cancellation, worker stop, provider disconnect, and bridge failure abandon unresolved permission callbacks safely.

## Repository safety

The requested repository must already be a valid Git checkout. Agent Task Console never initializes a directory or silently chooses a nested repository.

Without an isolated worktree, another active Session in the same checkout is rejected. Prefer:

```text
~/.agent/worktrees/<task-id>-<session-id>
branch: agent/<task-id>/<session-id>
```

Sharing a checkout requires an explicit override. Worktrees and branches are never deleted automatically.

## Agent profiles

Embedded profiles may be replaced or extended with `$AGENT_HOME/agents/*.yaml`:

```yaml
name: private-agent
provider: custom
command: /absolute/path/to/acp-agent
args:
  - --mode
  - acp
environment:
  FEATURE_FLAG: enabled
```

Commands and arguments execute literally without a shell. `acpctl agents [name]` hides environment values.

## Runtime state

```text
$AGENT_HOME/
├── agent.db          # Task and Session rows only
├── agents/           # profile overlays
├── logs/             # ordered Session channel replay/audit
├── workers/          # private sockets and worker state
├── acpx-runtime/     # private provider bridge state
└── worktrees/        # optional persistent Git worktrees
```

Workers are detached per Session and outlive viewers. Startup probes stale PID/socket ownership but never clears it from liveness alone: unresponsive ownership becomes a failed quarantine because provider descendants may remain. The gateway never reconstructs provider state from logs or automatically creates a replacement provider conversation.

## Security boundary

V1 assumes one trusted OS user on one machine. State directories, logs, the database, sockets, and provider output are private to that user. The web server accepts explicit loopback addresses only and enforces same-origin mutation controls.

This is not a public multi-tenant service. A public deployment would additionally require authentication, TLS, authorization, tenant and worker isolation, quotas, audit controls, secret management, and production recovery.

Never commit provider credentials, authentication state, SQLite files, sockets, logs, binaries, coverage output, or live worktrees. `make security-check` enforces the repository boundary.

## Troubleshooting

- **Repository is not Git** — select the exact existing checkout. The console may suggest one immediate child but never selects it automatically.
- **Session is busy** — wait for the active turn to complete or cancel it, then retry the prompt.
- **Session is starting** — keep the stream attached and wait for `connection.data.state == "connected"`. Provider-dependent commands return `session_starting`; Stop remains available and cancels startup.
- **Session has no live worker** — Resume only when it has a saved provider reference and no retained worker ownership. A Session with no saved reference cannot be recovered safely; create a new one. Retained failed ownership is quarantine and requires manual provider-process teardown rather than ordinary Resume.
- **Model or effort selector is unavailable** — the provider Port did not advertise that control. Leave it blank, which keeps the live Port's current value unchanged. Do not enter a guessed value.
- **Provider resume failed** — the Session remains stopped or failed. Create a new Session if the provider cannot resume its saved reference.
- **Permission remains pending after closing the browser** — this is expected. Attach another viewer and decide the exact request, or cancel the turn.

## Development

```bash
make fmt-check
make test
make race
make vet
make build
make security-check
make acceptance
```

The authoritative architecture and acceptance contract are in [agent_task_console_spec.md](agent_task_console_spec.md). Ordered implementation milestones are in [docs/DEVELOPMENT_PLAN.md](docs/DEVELOPMENT_PLAN.md).
