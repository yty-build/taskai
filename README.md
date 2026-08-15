# Agent Task Console

Agent Task Console is a local-first control plane for persistent AI-agent work. It groups independent agent conversations under durable Tasks, runs each Session through ACP with a detached worker, and lets a human reconnect from the CLI, terminal console, local web UI, or versioned API without owning the worker process.

The V1 product has exactly two first-class entities:

- **Task** — the outcome to produce. A Task may have zero or more Sessions.
- **Session** — one agent, role, provider conversation, and checkout working under exactly one Task.

Request IDs and append-only stream records are operational metadata. Reviews are ordinary reviewer Sessions. Git and the filesystem own artifacts. There is no Run, Event, Artifact, workflow, broker, or daemon domain model.

## Requirements

- macOS or Linux
- Go 1.23 or newer
- Git
- Node/npm only to install the pinned `acpx` executable
- `acpx` exactly 0.13.0
- at least one ACP-compatible agent CLI, authenticated independently

The embedded registry includes Codex, Claude, Gemini, Grok (`grok-build`), and OpenCode profiles. Agent Task Console never copies provider credentials into its database.

## Install

Clone the private repository, install the pinned adapter without `sudo`, then build:

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

`acpx` is pre-1.0, so V1 probes its version before use and refuses an incompatible binary. `ACPX_BINARY=/absolute/path/to/acpx` may select an explicit installation. See [the compatibility contract](docs/ACPX_COMPATIBILITY.md).

Runtime state defaults to `~/.agent`. Set `AGENT_HOME` before invoking `acpctl` to use another private state directory:

```bash
export AGENT_HOME=/absolute/private/path/agent-state
```

## Quick start

Create a Task with no Sessions:

```bash
acpctl task new "Fix login regression" \
  --description "Reproduce, repair, and independently verify the production issue" \
  --external-ref APP-91
```

Create a Task and its first Session in one convenience operation:

```bash
acpctl start "Fix login regression" \
  --agent codex \
  --repo /absolute/path/to/product \
  --role implementer \
  --worktree \
  --prompt "Run the complete authentication test suite"
```

`acpctl start` still persists one Task and one child Session; it does not introduce another product object.

Add independent Sessions under the same Task:

```bash
acpctl session start <task-id> \
  --agent claude \
  --repo /absolute/path/to/product \
  --role reviewer \
  --worktree
```

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

## Attachment semantics

Normal attachment is a disposable viewer/controller connection through the gateway and ACP:

```bash
acpctl task attach <task-id>
acpctl session attach <session-id>
```

The terminal console replays complete stored ACP-visible history, deduplicates it against live output, and reconnects from the last Session position. A Task console aggregates every child Session and discovers new Sessions without restart. The default Session screen is a native conversation harness with user/assistant turns, collapsed reasoning, merged tool cards, a multiline composer, and actionable permission cards.

Controls:

| Key | Effect |
| --- | --- |
| `↑`/`↓`, `j`/`k`, mouse | Select or scroll |
| `Enter` or click | Open selected Session |
| `n` (Task screen) | Open the in-console New Session wizard; choose agent, role, repository, worktree, permission mode, and optional prompt |
| `p` | Prompt the selected/current Session only |
| `Esc` or `c` | Request cancellation of the active turn |
| `u` | Resume a stopped Session |
| `x` or `s` | Stop the worker |
| `r` | Toggle normalized/raw debug rendering |
| `1`–`9` | Choose an exact offered permission option, then confirm |
| `b` or `Backspace` | Return to Task view |
| `q` or `Ctrl-C` | Detach the viewer |

Detaching never cancels a turn, stops a worker, resolves a permission, or releases another viewer. `Esc` requests cancellation when the composer is not active; inside the composer it closes editing and preserves the draft. A prompt submitted while busy is queued as the next ACP turn.

Provider-native attachment is an explicit, exclusive escape hatch:

```bash
acpctl session attach <session-id> --native
```

It is available only when the provider exposed a native Session ID and V1 has an exact resume mapping. The worker holds queued prompts while the native CLI owns the conversation. ACP resumes only after native exit and successful reconciliation. See [native operations](docs/OPERATIONS.md#provider-native-mode) and the [real Codex smoke procedure](docs/CODEX_NATIVE_SMOKE.md).

## Logs and review

Session history is append-only JSONL with monotonic per-Session positions and request IDs:

```bash
acpctl session logs <session-id>
acpctl session logs <session-id> --follow
acpctl session logs <session-id> --raw --from 42
acpctl task logs <task-id> --follow
```

Create an independent reviewer Session after stopping or completing the producer:

```bash
acpctl session review <producer-session-id> --agent claude
```

The reviewer receives a read-only producer log path, repository/worktree, branch, status and diff snapshot, plus explicit skeptical verification instructions. The producer conversation is not imported as the reviewer's conversation.

## Local web UI and API

Start the disposable browser viewer:

```bash
acpctl serve
# Agent Task Console listening at http://127.0.0.1:8080
```

Use `--listen 127.0.0.1:0` for an automatically selected port. Only explicit loopback IPs are accepted; wildcard, hostname, and public bindings are rejected. Stopping `acpctl serve` does not stop Session workers.

The web UI provides Task creation/list/detail, Session creation/detail, stored and live activity, prompt/cancel/resume/stop, review creation, and a sanitized raw execution DTO view. Raw worker sockets, JSONL paths, and ACP wire payloads do not cross the browser API boundary.

The JSON/SSE API is rooted at `/api/v1`, with `/healthz` for local health checks. Session streams use numeric per-Session positions. Task streams use opaque cursors containing independent positions for every Session. Both accept `Last-Event-ID` or `?cursor=` for lossless replay. See [API.md](docs/API.md).

## Permissions and checkout safety

Sessions default to `read-approval`. Read-like work may resolve automatically; write/execute/unknown ACP permission requests stay pending and are shown in the Session harness. Codex, Claude, and OpenCode are launched with restrictive provider settings so their defaults cannot silently write, while acpx host filesystem/terminal prompts are brokered as one-shot cards. A pending request survives detach, and concurrent viewers use first-valid-decision-wins. Persistent `allow_always`/`reject_always` choices are visibly marked and require confirmation. Use `--approve-all` only when unattended writes are intentional; the choice is persisted and visible in Session detail.

The pinned public runtime exposes interactive ACP `session/request_permission` requests. The bridge additionally brokers its known `fs/write_text_file` and `terminal/create` host prompts with one-shot choices; unknown host operations fail closed. The `r` view shows complete gateway envelopes and normalized runtime payloads, not an exact underlying JSON-RPC packet capture.

Without `--worktree`, Agent Task Console refuses another active Session in the same checkout. Prefer isolated worktrees:

```text
~/.agent/worktrees/<task-id>-<session-id>
branch: agent/<task-id>/<session-id>
```

`--allow-shared-checkout` is an explicit override for intentional concurrent access. V1 never automatically deletes a worktree or branch.

## Agent profiles

Embedded defaults may be replaced or extended with `~/.agent/agents/*.yaml` (or `$AGENT_HOME/agents/*.yaml`):

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

Only `name`, `provider`, `command`, `args`, and `environment` are accepted. Commands run as literal argv without a shell. `acpctl agents [name]` diagnoses the loaded profile and hides environment values. See [OPERATIONS.md](docs/OPERATIONS.md#agent-profiles).

## State and architecture

```text
$AGENT_HOME/
├── agent.db          # SQLite Task and Session rows only
├── agents/           # profile overlays
├── logs/             # correlated Session JSONL
├── workers/          # private Unix sockets and runtime state
├── acpx-runtime/     # private per-Session public-runtime bridge state
└── worktrees/        # optional persistent Git worktrees
```

Every normal CLI invocation reconciles stale worker PID/socket state. Workers are detached per Session, serialize prompts, support multiple subscribers, and outlive CLI, terminal, and web viewers. Provider-native transcripts remain authoritative for provider-specific state ACP does not expose.

See [ARCHITECTURE.md](docs/ARCHITECTURE.md) for component boundaries and failure behavior.

## Security boundary

V1 assumes one trusted OS user on one machine. State directories, logs, database files, and Unix sockets are private to that user. The web server enforces loopback binding, loopback Host headers, same-origin mutations, CSP, and frame denial.

V1 is **not a public service**. Changing the bind address is deliberately rejected and would not provide authentication, TLS, authorization, tenant isolation, worker isolation, quotas, audit controls, secret management, or production recovery. A future gateway may preserve the Task/Session API contract while adding all of those controls.

Do not place provider authentication files, credentials, databases, sockets, logs, binaries, or live worktrees in this repository. `make security-check` inspects current tracked files and reachable Git history, then enforces credential patterns, ignore rules, executable fixtures, and the two-table boundary.

## Troubleshooting

- **`acpx 0.13.0 was not found`** — install the pinned version under `~/.local`, add `~/.local/bin` to `PATH`, or set `ACPX_BINARY`.
- **Unsupported acpx version** — remove the conflicting executable or point `ACPX_BINARY` at 0.13.0.
- **Agent profile not found** — run `acpctl agents`; ensure the YAML filename ends in `.yaml` and its schema has no unknown fields.
- **Checkout already used** — use `--worktree`, stop the existing Session, or use `--allow-shared-checkout` only when concurrent writers are intentional.
- **Session has no live worker** — inspect `acpctl session <id>`, then run `acpctl session resume <id>`.
- **Legacy CLI transport Session** — stored history remains attachable, but the new binary refuses a blind resume while an old detached acpx queue owner may exist. Create a fresh runtime-backed Session unless the old owner is independently proven gone and a deliberate migration path is added.
- **Native ID unavailable** — the provider/acpx adapter did not expose an exact native conversation ID. Continue through ACP; never substitute a different ID.
- **Native reconciliation failed** — keep the native CLI closed, correct the provider/acpx issue, then use `--native --reconcile-only`. Queued prompts remain held until reconciliation succeeds.
- **Web server refuses address/Host** — use the exact `http://127.0.0.1:<port>` URL printed by `acpctl serve`.
- **Corrupt/non-monotonic JSONL** — preserve the file for diagnosis. The reader fails closed instead of skipping records.

More operational detail is in [OPERATIONS.md](docs/OPERATIONS.md#troubleshooting).

## Development and acceptance

```bash
make fmt-check
make test
make race
make vet
make build
make security-check
make acceptance
```

`make acceptance` uses isolated temporary state plus deterministic fake `acpx` and native CLIs. It exercises the built CLI, detached worker lifecycle, Task/Session persistence, worktrees, review, native lease/reconciliation, API/SSE reconnect, web lifecycle, concurrency, build metadata, and repository safety without provider credentials or network access.

Authenticated live-provider verification is intentionally separate because it depends on local provider accounts. See [ACCEPTANCE.md](docs/ACCEPTANCE.md) for the exact specification mapping and [TEST_PLAN.md](docs/TEST_PLAN.md) for independent tester instructions.

## Documentation

- [Architecture](docs/ARCHITECTURE.md)
- [Local API and SSE contract](docs/API.md)
- [Operations, profiles, permissions, security, and troubleshooting](docs/OPERATIONS.md)
- [Acceptance evidence map](docs/ACCEPTANCE.md)
- [acpx 0.13.0 compatibility](docs/ACPX_COMPATIBILITY.md)
- [Codex native smoke procedure](docs/CODEX_NATIVE_SMOKE.md)
- [Product specification](agent_task_console_spec.md)
- [Ordered development plan](docs/DEVELOPMENT_PLAN.md)
