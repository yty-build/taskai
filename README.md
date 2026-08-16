# Agent Task Console

Agent Task Console is a local-first control plane for persistent AI-agent work. It groups independent agent conversations under durable Tasks, runs each Session through ACP with a detached worker, and lets a human reconnect from the CLI, terminal console, local web UI, or versioned API without owning the worker process. The provider's own conversation store supplies committed history; ACP supplies the live provisional stream; and the gateway JSONL journal supplies lifecycle/audit evidence, provisional overlay, and an explicitly labelled fallback.

The V1 product has exactly two first-class entities:

- **Task** — the outcome to produce. A Task may have zero or more Sessions.
- **Session** — one agent, role, provider conversation, and checkout working under exactly one Task.

Request IDs and append-only stream records are operational metadata. Reviews are ordinary reviewer Sessions. Git and the filesystem own artifacts. There is no Run, Event, Artifact, workflow, broker, or daemon domain model.

## Requirements

- macOS or Linux
- Go 1.23 or newer
- Git
- tmux (recommended for detachable provider-native terminal sessions)
- Node/npm only to install the pinned `acpx` executable
- `acpx` exactly 0.13.0
- at least one ACP-compatible agent CLI, authenticated independently

The embedded registry includes Codex, Claude, Gemini, Grok (`grok`, with `grok-build` compatibility), and OpenCode profiles. Grok uses acpx's `grok-build` profile over ACP and the installed `grok` executable for its native terminal. Agent Task Console never copies provider credentials into its database.

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

Task attachment is a disposable developer cockpit over every child Session.
Direct Session attachment opens the real provider-native terminal by default;
`--acp` opens the gateway's structured transcript and permission console:

```bash
acpctl task attach <task-id>
acpctl session attach <session-id>
acpctl session attach <session-id> --acp
```

The Task console is a responsive Go/Bubble Tea developer cockpit over every child Session. It discovers new Sessions without restart, keeps attention states scannable, uses a two-pane layout at wide widths and stacked/single-pane layouts as space narrows, and offers contextual help plus a searchable action palette. Enter opens the selected provider's real native harness; `a` opens the structured ACP view.

The structured view loads a verified, bounded snapshot of the exact provider-owned conversation, overlays gateway-only provisional and lifecycle state through one captured JSONL cursor, then follows live ACP from that boundary without duplication. It renders user/assistant turns, provider-exposed reasoning in event order, merged tool cards, a multiline composer, and actionable permission cards. After settled ACP turns and provider-native process exit, an open structured viewer fetches a fresh joined projection in the background and publishes it atomically, without a visible reload or flicker, while preserving scroll/follow state, focus, composer draft/cursor, selection, pending-approval focus, and expanded reasoning/tool state. If the provider store is unavailable on initial load, the service returns a bounded gateway-history fallback with an explicit warning. If a transient refresh fails in an already-open viewer, that viewer keeps its existing in-memory presentation and discloses the delay; it never presents JSONL as the provider's committed transcript.

Task and structured ACP console controls (provider-native keys remain owned by that provider):

| Key | Effect |
| --- | --- |
| `↑`/`↓`, `j`/`k`, click | Select a Session |
| `Enter` | Open the selected Session in its real provider-native harness |
| `a` (Task screen) | Open the selected Session's structured ACP console |
| `n` (Task screen) | Open the in-console New Session wizard; choose agent, role, repository, worktree, permission mode, and optional prompt |
| `p` | Prompt the selected/current Session only |
| `/` | Filter the Session list |
| `:` | Open the searchable command palette |
| `?` | Show contextual help |
| `Esc` or `c` | Request cancellation of the active turn |
| `u` | Resume a stopped Session |
| `x` or `s` | Stop the worker |
| `r` | Toggle normalized/raw debug rendering |
| `1`–`9` | Choose an exact offered permission option, then confirm |
| `b` or `Backspace` | Return to Task view when opened from Task attachment |
| `q` or `Ctrl-C` | Detach the viewer |

Detaching either view never cancels a turn, stops a worker, resolves a permission, or changes provider-write ownership. `Esc` requests cancellation only in the structured ACP console when the composer is not active; inside the composer it closes editing and preserves the draft. A prompt submitted while an ACP turn is busy is queued as the next ACP turn.

Native attachment resumes one immutable canonical provider conversation ID after verifying that exact ID in the provider's local store:

```bash
acpctl session attach <session-id>
```

With tmux installed, `Ctrl-B d` detaches the terminal while the provider continues running; attaching again reopens the same tmux session. Viewers may coexist, but writes to one provider conversation are exclusive. Native attach waits or fails visibly while an ACP turn owns the writer gate and never cancels that turn. While the provider-native process owns the gate, ACP prompts remain visibly queued and do not reach the provider. Detaching tmux leaves both the native process and its ownership intact. Process exit/stop releases ownership only after the provider snapshot is reconciled, before queued ACP work crosses the boundary. See [native operations](docs/OPERATIONS.md#provider-native-mode) and the [real Codex smoke procedure](docs/CODEX_NATIVE_SMOKE.md).

## Logs and review

The gateway lifecycle/audit journal is append-only JSONL with monotonic per-Session positions and request IDs:

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

The reviewer receives the producer's gateway audit-log path as read-only
evidence, plus its repository/worktree, branch, status and diff snapshot, and
explicit skeptical verification instructions. The producer conversation is not
imported as the reviewer's own provider conversation.

## Local web UI and API

Start the disposable browser viewer:

```bash
acpctl serve
# Agent Task Console listening at http://127.0.0.1:8080
```

Use `--listen 127.0.0.1:0` for an automatically selected port. Only explicit loopback IPs are accepted; wildcard, hostname, and public bindings are rejected. Stopping `acpctl serve` does not stop Session workers.

The responsive web UI is a conversation-first structured ACP client: a persistent Task/Session rail, committed user and assistant turns from the provider store, live provisional ACP reasoning/output, bounded tool and plan cards, a pinned approval shelf, and a sticky multiline composer. It supports prompt/cancel/resume/stop and reviewer creation, preserves all viewer state during live updates and atomic reconciliation, and becomes a single-pane drawer layout on mobile. Session IDs, checkout details, source/fallback state, continuity state, and bounded sanitized diagnostics live behind Details instead of competing with the conversation.

The browser shows whether the same canonical provider conversation is linked and provider-store verified without exposing that identifier. Raw worker/provider errors, sockets, JSONL paths, provider identifiers, structured environment/auth fields, commands derived from raw input, raw tool objects, and ACP wire payloads do not cross the browser API boundary. Allowlisted provider-visible text, diffs, and terminal output are intentionally rendered and can contain sensitive repository or tool-produced content; this loopback console is for the same trusted OS user and is not a secret scanner. Trusted local forensic inspection remains available through terminal `--raw` logs.

The JSON/SSE API is rooted at `/api/v1`, with `/healthz` for local health checks. Session streams use numeric per-Session positions. Task streams use opaque cursors containing independent positions for every Session. Both accept `Last-Event-ID` or `?cursor=` for lossless replay. See [API.md](docs/API.md).

## Permissions and checkout safety

Sessions default to `read-approval`. Read-like work may resolve automatically; write/execute/unknown ACP permission requests stay pending and are shown in the Session harness. Codex, Claude, and OpenCode are launched with restrictive provider settings so their defaults cannot silently write, while acpx host filesystem/terminal prompts are brokered as one-shot cards. A pending request survives detach, and concurrent viewers use first-valid-decision-wins. Persistent `allow_always`/`reject_always` choices are visibly marked and require confirmation. Use `--approve-all` only when unattended writes are intentional; the choice is persisted and visible in Session detail.

The pinned public runtime exposes interactive ACP `session/request_permission` requests. The bridge additionally brokers its known `fs/write_text_file` and `terminal/create` host prompts with one-shot choices; unknown host operations fail closed. The `r` view shows the bounded gateway envelopes retained by that viewer and their normalized runtime payloads, not an exact underlying JSON-RPC packet capture. Use `acpctl session logs <id> --raw` for the full persisted gateway journal.

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
├── logs/             # Session lifecycle/audit/overlay/fallback JSONL
├── workers/          # private Unix sockets and runtime state
├── acpx-runtime/     # private per-Session public-runtime bridge state
└── worktrees/        # optional persistent Git worktrees
```

Every normal CLI invocation reconciles stale worker PID/socket state. Workers are detached per Session, serialize prompts, support multiple subscribers, and outlive CLI, terminal, and web viewers. One verified provider-owned conversation is the committed history for each Session. ACP activity remains provisional until provider-store reconciliation; gateway JSONL remains lifecycle/audit evidence and fallback rather than the canonical transcript.

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
- **Legacy CLI transport Session** — available provider history and gateway
  journal remain readable, but the new binary refuses a blind resume while an
  old detached acpx queue owner may exist. Create a fresh runtime-backed Session
  unless the old owner is independently proven gone and a deliberate migration
  path is added.
- **Canonical provider conversation unavailable** — wait for the first ACP turn to establish one exact identity. Continue through `--acp`; never guess or substitute another ID.
- **Native verification failed** — the exact canonical ID was not found in that provider's local store for the Session checkout. Continue through `--acp` and diagnose provider state; the gateway will not open a replacement conversation.
- **Native attach is waiting/busy** — an ACP turn currently owns the Session's provider-writer gate. Let it settle or cancel it explicitly; native attach never cancels it on your behalf.
- **ACP prompt says queued behind native** — the provider-native process still owns the writer gate. Detaching its tmux viewer is not process exit. Exit/stop the native process, then allow exact provider reconciliation to finish before the queued prompt dispatches.
- **ACP refresh/recovery warning after native exit** — the ownership boundary remains closed to queued writes until exact reconciliation/recovery succeeds. If bounded exact recovery failed, queued prompts receive correlated errors and the worker stops; run `acpctl session resume <id>` before sending more work.
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

`make acceptance` uses isolated temporary state plus deterministic fake `acpx` and native CLIs. It exercises the built CLI, detached worker lifecycle, Task/Session persistence, worktrees, review, canonical identity and provider-history verification, native attachment, exclusive writer ownership, atomic reconciliation, API/SSE reconnect, web lifecycle, build metadata, and repository safety without provider credentials or network access.

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
