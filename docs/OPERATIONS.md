# Operations guide

## Installation and upgrade

Agent Task Console is one Go binary plus the pinned external `acpx` executable.

```bash
npm install --global --prefix "$HOME/.local" acpx@0.13.0
export PATH="$HOME/.local/bin:$PATH"

make verify
make build VERSION=1.0.0
install -m 0755 bin/acpctl "$HOME/.local/bin/acpctl"
acpctl version
```

Build metadata is injected through `VERSION`, `COMMIT`, and `BUILD_DATE`. `make version-check` builds with deterministic values and verifies the exact output.

Before upgrading:

1. commit or preserve any Session worktree changes;
2. stop active Sessions when practical;
3. copy the private `AGENT_HOME` if rollback is required;
4. build and run `make acceptance` at the target SHA;
5. replace only the `acpctl` binary;
6. run `acpctl tasks` to reopen/migrate SQLite and reconcile stale workers.

Migrations are embedded, transactional, and idempotent. Downgrade migration is not supported; restore the matching binary and backup together.

## Runtime state

Default state root: `~/.agent`.

| Path | Purpose | Expected access |
| --- | --- | --- |
| `agent.db` plus SQLite sidecars | Task/Session persistence | current user only |
| `agents/*.yaml` | profile overlays | private configuration |
| `logs/<session>.jsonl` | correlated ACP-visible history | `0600` |
| `workers/<session>.sock` | local worker command/subscription transport | `0600` |
| `worktrees/<task>-<session>` | optional persistent Git checkout | normal user-owned Git tree |

The root and state subdirectories are forced to `0700`. JSONL and sockets are forced to `0600`. Provider authentication stays wherever each provider CLI owns it; it is not copied into this tree or SQLite.

For isolated testing:

```bash
export AGENT_HOME="$(mktemp -d)/agent-home"
```

Do not point two unrelated users or hosts at one state root. V1 assumes one OS user and local filesystem semantics.

## Agent profiles

Embedded profiles:

| Name | Provider | Literal command |
| --- | --- | --- |
| `codex` | `openai` | `codex` |
| `claude` | `anthropic` | `claude` |
| `gemini` | `google` | `gemini --experimental-acp` |
| `grok-build` | `xai` | `grok-build` |
| `opencode` | `opencode` | `npx -y opencode-ai@1.18.18 acp` |

Place overrides or custom profiles in `$AGENT_HOME/agents/*.yaml`:

```yaml
name: custom-acp
provider: internal
command: /opt/company/bin/custom-agent
args:
  - --acp
  - --profile
  - safe
environment:
  COMPANY_REGION: us-west
```

Schema rules:

- only `name`, `provider`, `command`, `args`, and `environment` are allowed;
- name uses letters, numbers, `.`, `_`, or `-` and begins alphanumerically;
- environment names use conventional shell-variable syntax;
- YAML has one document and no unknown fields;
- user profiles load lexically and replace an embedded profile with the same name;
- two user files may not define the same name;
- command and arguments are passed directly to `exec`, never through shell expansion.

Inspect resolution without exposing environment values:

```bash
acpctl agents
acpctl agents custom-acp
```

Prefer provider-native credential stores or the launching process environment over literal secrets in YAML. If an environment value is necessary, keep the profile user-readable only and never commit it.

`acpx` receives a custom command as one literal agent argument. Adapter fixtures verify spaces/shell metacharacters are not interpolated.

## CLI reference

| Command | Purpose / options |
| --- | --- |
| `acpctl version` | Print injected version, commit, and build date |
| `acpctl agents [name]` | List profiles or diagnose one profile |
| `acpctl task new <title>` | Create zero-Session Task; optional `--description`, `--external-ref`, `--metadata-json` |
| `acpctl tasks` | List Tasks |
| `acpctl task <id>` | Show Task and child Session summaries |
| `acpctl task status <id> <status>` | Set `open`, `working`, `review`, `done`, `failed`, or `cancelled` |
| `acpctl start <title>` | Atomically create Task + first Session; requires `--agent`, `--repo`; supports `--role`, Task description/reference/metadata, `--prompt`, `--approve-all`, `--worktree`, and shared-checkout override |
| `acpctl session start <task-id>` | Add Session; requires `--agent`, `--repo`; supports `--role`, `--prompt`, `--approve-all`, `--worktree`, and shared-checkout override |
| `acpctl session <id>` | Show full local Session/runtime detail |
| `acpctl session prompt <id> <message>` | Submit/queue a correlated prompt |
| `acpctl session cancel <id>` | Request cancellation of active turn |
| `acpctl session stop <id>` | Stop worker without deleting history/worktree |
| `acpctl session resume <id>` | Resume persisted ACP conversation; optional shared-checkout override |
| `acpctl session logs <id>` | Replay/follow; supports `--follow`, `--raw`, `--from <position>` |
| `acpctl task logs <id>` | Aggregate child histories; supports `--follow`, `--raw` |
| `acpctl task attach <id>` | Interactive aggregate Task viewer/controller |
| `acpctl session attach <id>` | Interactive Session viewer/controller |
| `acpctl session attach <id> --native` | Exclusive exact provider-native resume; optional `--cancel-active` or `--reconcile-only` |
| `acpctl session review <producer-id>` | Requires `--agent`; optional `--allow-active-producer` |
| `acpctl serve` | Local HTML/JSON/SSE server; optional `--listen <explicit-loopback-ip:port>` |

Options accept either `--name value` or `--name=value`. Boolean options take no value. Duplicate and unknown options fail with exit code 2. General failures use exit code 1 and stable `acpctl: <message>` stderr output.

## Permissions

The default permission is `read-approval`: read-like work may be resolved automatically, while ACP write/execute/unknown `session/request_permission` calls appear as actionable cards in an attached Session console.

Use `--approve-all` only for an explicitly trusted unattended Session:

```bash
acpctl session start <task-id> --agent codex --repo /repo --role implementer --approve-all
```

The choice is persisted in Session `metadata_json` and surfaced as `Permission` in CLI, terminal, API, and web detail. Noninteractive execution never implicitly enables writes.

When an approval card appears, press its displayed number and confirm the exact provider-offered choice. `allow_always` and `reject_always` are marked `PERSISTENT`. Pressing `q`, closing the terminal, or losing the viewer connection leaves the request pending; attach again to decide it. If two viewers decide concurrently, exactly one wins. Cancelling the turn or stopping the worker cancels the pending request.

The public `acpx/runtime` hook covers ACP `session/request_permission`. The pinned bridge also brokers the runtime's client-side `fs/write_text_file` and `terminal/create` y/N host prompts as one-shot approval cards over a separate private control/input channel. Unknown host permission paths fail closed. Codex/Claude restrictive modes and the OpenCode inline `edit`/`bash: ask` policy prevent their permissive defaults from bypassing this contract. OpenCode's permission configuration is described in its [official permission documentation](https://opencode.ai/docs/permissions).

Agent Task Console does not replace provider sandboxing. Provider command behavior and local OS permissions still apply.

## Repository and worktree safety

Every Session records a validated Git repository root, optional worktree, and branch. The service takes a per-repository file lock during checkout claims and worktree creation.

Preferred isolation:

```bash
acpctl session start <task-id> --agent codex --repo /repo --role implementer --worktree
```

This creates:

```text
$AGENT_HOME/worktrees/<task-id>-<session-id>
agent/<task-id>/<session-id>
```

The preflight rejects an existing path, existing branch, or an active Session already using the checkout. `--allow-shared-checkout` bypasses only the active-checkout claim and should be rare. It does not merge or serialize concurrent Git/file writes.

Stopping/completing a Session never removes its worktree or branch. Inspect and remove them manually with normal Git tools after preserving work.

## Worker lifecycle

Useful commands:

```bash
acpctl session <id>
acpctl session logs <id> --follow
acpctl session cancel <id>
acpctl session stop <id>
acpctl session resume <id>
```

- `cancel` affects the active ACP turn and leaves the conversation resumable.
- `stop` asks the worker to stop and waits for persisted ownership to clear.
- `resume` starts a new detached worker that resumes the persisted ACP Session ID.
- a worker accepts one turn at a time and queues later prompts.
- multiple subscribers receive the same lines.
- a worker stop/crash does not delete JSONL.

Every normal CLI command checks persisted active Sessions. If a PID is dead or its private socket is unreachable, it clears stale ownership, marks the Session stopped, and removes stale runtime files. It does not auto-resume work.

New Sessions record `acp_transport=acpx-runtime-v1` in `metadata_json`. A Session with an existing ACP ID but no runtime marker is treated as a legacy CLI-backed conversation and is not blindly resumed: its old detached queue owner may still be alive. Stored history remains readable. Create a fresh Session for normal use; do not run both transports against the same provider conversation.

## Terminal attachment

Normal attach is through ACP:

```bash
acpctl task attach <task-id>
acpctl session attach <session-id>
```

Use `q`/`Ctrl-C` to detach. This cancels only viewer subscriptions. Do not use `stop` merely to close a UI.

Task attach targets one selected Session for every prompt or control action; there is no broadcast. Stored history appears before live output, and positions prevent duplicate replay.

From the Task screen, press `n` to add a Session without leaving the console. The wizard chooses the agent, role, repository, isolated worktree setting, permission mode, and optional initial prompt; `Ctrl+S` starts it and opens the new Session automatically.

Session attach renders a native conversation harness by default: user/assistant turns, collapsed reasoning, merged tool cards, plans, errors, and permission cards. Press `r` for the bounded gateway raw/debug envelope view; the public runtime does not provide an exact JSON-RPC packet capture.

## Provider-native mode

Supported exact resume mappings:

| Profile | Invocation suffix |
| --- | --- |
| `codex` | `resume <native-id>` |
| `claude` | `--resume <native-id>` |
| `gemini` | `--resume <native-id>` |
| `opencode` | `--session <native-id>` |

`grok-build` and custom profiles have no V1 native mapping. They remain usable through ACP.

Before native attach, `acpctl session <id>` must show both ACP and Native Session IDs. If either is absent, native mode fails without starting another conversation.

```bash
acpctl session attach <id> --native
```

Default behavior waits for an active turn. To request cancellation explicitly:

```bash
acpctl session attach <id> --native --cancel-active
```

While leased:

- the worker remains live and Session status is waiting;
- new prompts are accepted into the queue but not executed;
- the native CLI runs in the persisted repo/worktree;
- no ACP prompt touches the provider conversation.

On native exit the worker reconciles the exact ACP/provider conversation. It releases the lease only after successful reconciliation. If reconciliation fails, confirm the native process is closed, fix the external problem, then run:

```bash
acpctl session attach <id> --native --reconcile-only
```

Never run reconciliation while a native CLI still owns the Session.

## Review operations

Stop or complete the producer, then create an independent reviewer:

```bash
acpctl session stop <producer-id>
acpctl session review <producer-id> --agent claude
```

`--allow-active-producer` is an explicit unsafe-state override when concurrent review is intentional. It also permits the shared checkout needed for that case. The review snapshot may become stale immediately, so the reviewer prompt tells the agent to rerun Git inspection.

Review evidence includes the producer's JSONL path. Treat it as read-only; repair work belongs in another implementer Session.

## Local web server

```bash
acpctl serve
acpctl serve --listen 127.0.0.1:0
```

Use the exact URL printed. The handler rejects arbitrary Host headers; `localhost` may therefore be rejected even though it resolves locally. Browser mutations must be same-origin. Closing the browser or sending SIGINT/SIGTERM to `acpctl serve` has no worker lifecycle effect.

The browser's “Show raw execution” option shows the sanitized API Activity DTO, including kind/request/position. It intentionally does not reveal ACP JSON-RPC, worker sockets, native identifiers, or JSONL paths. Use the local terminal `--raw` log view for trusted-user forensic inspection.

## Public-service boundary

V1 must not be put behind a public DNS name, reverse proxy, port forward, tunnel, container ingress, or changed bind address. Loopback restriction is enforced, but it is not the only missing production control.

A public profile requires, at minimum:

- authenticated HTTPS;
- per-client and per-Task authorization;
- tenant/process/repository isolation;
- encrypted secret storage and rotation;
- rate limits, quotas, and abuse controls;
- structured audit retention;
- production database topology, backups, and recovery;
- remote worker identity/routing;
- safe customer-facing output filtering;
- operational monitoring and incident response.

Those controls may wrap the versioned Task/Session API later. They are intentionally not approximated by V1.

## Backup and cleanup

For a coherent cold backup, stop active Sessions and `acpctl serve`, then copy the entire private `AGENT_HOME`. Worktrees may contain uncommitted user work and must be backed up or committed separately.

To remove disposable state, first verify the exact `AGENT_HOME`, stop its Sessions, preserve required repositories/worktrees/logs, and then use normal user-controlled filesystem cleanup. Agent Task Console never auto-deletes that data.

## Troubleshooting

### Adapter

**Not found:** install `acpx@0.13.0`, correct `PATH`, or set `ACPX_BINARY` to an absolute executable.

**Version mismatch:** `acpx --version` must print exactly `0.13.0`. V1 refuses to guess compatibility.

**Protocol error/malformed output:** capture the operation and provider versions. The adapter accepts a final non-newline JSON record but rejects malformed JSON, remote JSON-RPC errors, and missing required control fields.

**Authentication prompt:** authenticate with the provider's own CLI outside Agent Task Console. Do not paste credentials into Task text, logs, profile files, or bug reports.

### Sessions and workers

**No live worker:** `acpctl session resume <id>` after confirming its checkout is safe.

**Already running:** another healthy worker owns the Session. Attach to it; do not start a duplicate.

**Stop timeout:** inspect Session detail and process state. A later normal CLI command will reconcile a dead PID/socket; do not delete a live socket blindly.

**Prompt says queued:** another turn or native lease owns execution. Follow logs for its request ID.

**Cancel says no active turn:** the Session is idle; send the next prompt normally.

### Git

**Not a repository:** pass any path inside a real Git worktree; the service resolves its root.

**Checkout conflict:** choose `--worktree`, stop the owner, or knowingly use `--allow-shared-checkout`.

**Worktree path/branch exists:** preserve or remove the existing user data manually, then start a new Session. V1 does not overwrite it.

### Native mode

**Native ID missing:** continue with ACP. Current provider metadata did not support exact resume.

**Unsupported profile:** use ACP; do not invent provider flags.

**Lease remains after failure:** ensure the native process exited, then use `--reconcile-only`. Held prompts are a safety mechanism.

### Logs and streams

**Replay fails on corrupt JSONL:** copy the file for diagnosis. Records are not skipped because doing so could mis-correlate later positions.

**SSE reconnect is rejected:** send either the exact prior `Last-Event-ID` or the identical `cursor` query, not conflicting values. Task cursors are opaque.

**Web shows reconnecting for a stopped Session:** its finite history stream closed normally; reconnect carries the last position and produces no duplicates.

### Repository checks

Run:

```bash
make security-check
git status --short
git ls-files
```

The safety check rejects those files in the current tree or reachable history, common credential formats in current/history content, missing ignore rules, non-executable automation fixtures, or persistence tables beyond Task/Session.
