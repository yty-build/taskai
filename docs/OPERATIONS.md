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
| `acpx-runtime/<session>/` | private public-runtime bridge state | current user only |
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
| `grok` | `xai` | `grok-build` (acpx friendly profile; native executable is `grok`) |
| `grok-build` | `xai` | `grok-build` (compatibility profile name) |
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
| `acpctl session attach <id>` | Resume the exact canonical conversation in the real provider-native terminal |
| `acpctl session attach <id> --acp` | Open the structured gateway transcript/permission console; `--native` remains a compatibility alias for the default |
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

Task attachment is a Session navigator. Direct Session attachment opens the provider's real native terminal by default; use `--acp` for the structured gateway console:

```bash
acpctl task attach <task-id>
acpctl session attach <session-id>
acpctl session attach <session-id> --acp
```

Use `q`/`Ctrl-C` to detach a gateway view. With a tmux-backed native terminal, use `Ctrl-B d`; the provider keeps running and `acpctl session attach <id>` reconnects to it. Detaching either view never stops the worker or cancels an ACP turn. Do not use `stop` merely to close a UI.

Task attach targets one selected Session for every prompt or control action; there is no broadcast. Stored history appears before live output, and positions prevent duplicate replay.

From the Task screen, press `n` to add a Session without leaving the console. The wizard chooses the agent, role, repository, isolated worktree setting, permission mode, and optional initial prompt; `Ctrl+S` starts it and opens the new Session automatically.

Enter or a mouse click on the Task screen opens the selected real native harness. Press `a` to open the selected structured ACP view, `p` to compose an ACP prompt, `/` to filter Sessions, `:` for the searchable action palette, and `?` for contextual help.

The `--acp` view renders user/assistant turns, collapsed reasoning, merged tool cards, plans, errors, and permission cards. Press `r` for the bounded gateway raw/debug envelope view; the public runtime does not provide an exact JSON-RPC packet capture.

## Provider-native mode

Supported exact resume mappings:

| Profile | Invocation suffix |
| --- | --- |
| `codex` | `resume <provider-conversation-id>` |
| `claude` | `--resume <provider-conversation-id>` |
| `grok` / `grok-build` | `grok --resume <provider-conversation-id>` |
| `opencode` | pinned `npx -y opencode-ai@1.18.18 --session <provider-conversation-id>` |

Gemini and custom profiles remain usable through ACP but do not have V1 provider-store verification for native attachment.

`acpctl session <id>` shows the Task Console ID, immutable canonical provider conversation, identity source, and adapter evidence. Native attach requires a canonical ID and verifies that exact ID plus checkout in the provider's local store. It never picks the most recent conversation and never starts a replacement when verification fails.

```bash
acpctl session attach <id>
```

With tmux installed, the command creates or reconnects to a stable `agent-<session-id>` tmux session. Detach it without stopping the provider:

```bash
Ctrl-B d
```

Native and ACP are two transports to the same provider conversation:

- native attachment does not pause, cancel, or block the ACP worker;
- ACP activity does not terminate or seize the native terminal;
- native attachment emits an advisory when it detects active, queued, or refreshing ACP work because concurrent turns may interleave unpredictably;
- the user is responsible for not sending simultaneous turns through both transports.

Detaching tmux is not provider exit, so no refresh is requested. When the provider process actually exits, the hidden native owner asks the worker to refresh its exact persisted ACP Session. The tmux Session receives the invoking `AGENT_HOME` and `PATH`, rather than inheriting stale values from an older tmux server. If an ACP turn is active or queued, refresh waits for the next idle boundary. Refresh is time-bounded; on an uncertain result the complete bridge/provider process group is terminated and awaited before bounded exact resume recovery. If recovery fails, queued prompts receive correlated `turn_error` records and the worker stops instead of using an uncertain conversation. Warnings use `native_refresh_warning` or `native_refresh_recovery_warning`; no path creates a lock.

```bash
acpctl session logs <id> --raw
```

Look for `native_refresh_started`, then `native_refreshed` or an actionable warning. Continue through `--acp` if native verification is unavailable; never substitute another identifier.

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

**Prompt says queued:** another ACP turn owns execution. Native attachment does not participate in the ACP queue. Follow logs for its request ID.

**Cancel says no active turn:** the Session is idle; send the next prompt normally.

### Git

**Not a repository:** pass any path inside a real Git worktree; the service resolves its root.

**Checkout conflict:** choose `--worktree`, stop the owner, or knowingly use `--allow-shared-checkout`.

**Worktree path/branch exists:** preserve or remove the existing user data manually, then start a new Session. V1 does not overwrite it.

### Native mode

**Canonical provider conversation missing:** continue with `--acp` until the provider establishes an exact identity. Do not invent one.

**Unsupported profile:** use ACP; do not invent provider flags.

**Provider-store verification fails:** the gateway found no exact canonical-ID/check-out match and intentionally did not launch native mode. Diagnose the provider's local state or continue with `--acp`.

**tmux name collision:** an existing `agent-<session-id>` tmux Session lacks the private owner tag for this `AGENT_HOME`. The gateway refuses to attach. Inspect that tmux Session and close or rename it deliberately; never attach by name alone.

**Refresh/recovery warning after native exit:** no lease exists. Inspect raw logs. If exact recovery failed, the worker drains queued work with correlated errors and stops; run `acpctl session resume <id>` to establish the exact persisted identity before sending another prompt.

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
