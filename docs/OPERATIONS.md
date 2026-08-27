# Operations guide

## Installation and upgrade

Requirements:

- Go 1.23+
- Node.js 22.13+
- `acpx` 0.13.0
- independently authenticated provider CLIs

```bash
npm install --global --prefix "$HOME/.local" acpx@0.13.0
export PATH="$HOME/.local/bin:$PATH"
make build VERSION=1.0.0
install -m 0755 bin/acpctl "$HOME/.local/bin/acpctl"
```

Confirm:

```bash
acpctl version
acpctl agents
```

`ACPX_BINARY=/absolute/path/to/acpx` selects an explicit adapter. The binary rejects unsupported `acpx` versions before starting provider work.

### Architecture-changing upgrades

The Session Channel worker protocol is intentionally incompatible with workers implementing provider-history reconciliation, per-prompt Refresh, staged launch, or native ownership.

Before upgrading:

1. List active Sessions.
2. Let important turns settle or cancel them deliberately.
3. Stop old workers cleanly.
4. Confirm no old provider-native/tmux child remains active.
5. Back up `$AGENT_HOME/agent.db` and `$AGENT_HOME/logs`.
6. Install the new binary.
7. Resume only Session Channel Sessions with a saved provider reference. Migration clears the physical ProviderRef from every row carrying pre-Channel identity evidence, even if the old row claimed provider-store verification; create a new Session for those legacy rows.

Never delete old native-owner markers, launch outboxes, runtime directories, or logs while an old process might still use them. New code may ignore obsolete files after safe shutdown. It must not replay an indeterminate old prompt.

## Runtime state

State defaults to `~/.agent`:

```text
$AGENT_HOME/
├── agent.db          # Task and Session rows
├── agents/           # profile overlays
├── logs/             # ordered Session channel frames
├── workers/          # private sockets and runtime metadata
├── acpx-runtime/     # private Port bridge state
└── worktrees/        # persistent optional Git worktrees
```

Use an absolute private alternative when needed:

```bash
export AGENT_HOME=/absolute/private/path/agent-state
```

Directories use `0700`; state, logs, and sockets are user-private. Provider auth remains in provider-owned locations.

## Agent profiles

Built-in and `$AGENT_HOME/agents/*.yaml` profiles select a provider adapter:

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

Only documented fields are accepted. Commands and arguments are literal, not shell fragments. Environment values are hidden from diagnostic output and are not persisted in channel logs.

Profiles do not contain model or reasoning catalogs. Those choices come from the connected Port.

## Task operations

```bash
acpctl task new "Fix login regression" --description "Reproduce and repair"
acpctl tasks
acpctl task <task-id>
acpctl task attach <task-id>
acpctl task logs <task-id> --follow
```

Task attachment is a disposable viewer over child Session channels. Closing it does not stop workers.

## Session operations

```bash
acpctl session start <task-id> \
  --agent codex \
  --role worker \
  --repo /absolute/path/to/repo \
  --worktree

acpctl session <session-id>
acpctl session attach <session-id>
acpctl session prompt <session-id> "Inspect the failing test"
acpctl session cancel <session-id>
acpctl session stop <session-id>
acpctl session resume <session-id>
```

Session creation persists the row, opens its channel socket and journal, emits a replayable `connection` frame with `data.state == "connecting"`, and returns the Session in `starting` without provider options. Subscribers can attach immediately while the worker invokes one provider Start or exact Resume. Wait for `connection.data.state == "connected"` before reading provider options or sending provider-dependent commands. Provider startup is bounded to 30 seconds; timeout or other failure leaves the same Session visible as failed.

One turn may be active. A second prompt returns busy and is not queued. Wait, cancel, or retry explicitly.

`stop` ends the worker and closes its Port while retaining the Session. It is the lifecycle operation for that purpose; there is no separate close command or API. `resume` starts one worker and exact-resumes the saved provider reference once. If the provider cannot resume it, the Session stays stopped or failed; create a new Session rather than forcing replacement continuity.

During `starting`, provider-options, configure, prompt, cancel, and permission response return typed `session_starting` without provider dispatch or queueing. Status, ping, subscribe/replay, permission observation, and Stop remain available. Stop cancels an in-progress provider startup and leaves the Session stopped.

## Live channel and replay

The structured terminal and browser show the ordered Session Channel. The outer journal types are:

- `connection`;
- `client_command`;
- `provider_event`;
- `provider_result`;
- `permission_requested`;
- `permission_resolved`;
- `permission_abandoned`;
- `worker_error`.

Every normalized runtime object, including unknown event shapes, appears verbatim in `provider_event.data` immediately. Clients may render known event contents locally, but the journal does not invent outer assistant/reasoning/tool/plan types. No provider transcript load happens first.

```bash
acpctl session logs <session-id>
acpctl session logs <session-id> --follow
acpctl session logs <session-id> --from 42
acpctl session logs <session-id> --raw
```

Session JSONL is ordered replay and audit. It is not provider memory or a complete transcript. If retention or corruption prevents replay, the viewer reports a gap and continues live when possible. It never reads provider-private history as a fallback.

Fresh Web/TUI attachment uses the last 256 frames from a byte-bounded tail so current output is never delayed by years of audit. A saved nonzero cursor resumes exactly; `acpctl session logs` remains the streaming full-audit path.

Logs can contain sensitive repository and tool output. Protect and rotate them according to local policy; do not commit them.

## Model and reasoning configuration

After the Port connects, clients make an observational provider-options read. Web renders dropdowns and terminal renders arrow selectors from that response.

- Blank in a configure field leaves the live Port's current value unchanged; on a fresh Port that value is the provider default.
- Unsupported controls remain disabled.
- Do not paste or guess values not advertised by the current Port.
- Configuration returns success only after the worker applies it to the live Port; failure is returned directly.
- No `config_changed` journal frame exists.
- Model and effort selections are not persisted or reapplied. After Resume, refresh provider options and treat the Port's advertised current values as authoritative.

There is no prepared/launching Session recovery. Configuration and prompt are ordinary channel commands.

## Permissions

Default policy is read approval. Write, execute, and unknown requests remain pending unless unattended approval was explicitly chosen.

A pending permission survives:

- terminal detach;
- browser close;
- another viewer disconnecting.

Attach any viewer to the same Session and choose one exact offered option. The first valid concurrent decision wins. Cancel, stop, provider disconnect, and worker failure abandon unresolved callbacks safely.

Use unattended write approval only for trusted work in an appropriate isolated checkout.

## Repository and worktree safety

The repository path must already be a Git repository or worktree. The gateway does not initialize directories or silently choose a child checkout.

If a non-repository parent contains exactly one immediate Git checkout, the UI may suggest it. Confirm the exact canonical path in a second action.

Another active managed Session in the same checkout is rejected by default. Prefer `--worktree`; use shared checkout only when concurrent writers are intentional.

Worktrees use:

```text
$AGENT_HOME/worktrees/<task-id>-<session-id>
branch agent/<task-id>/<session-id>
```

Worktrees and branches are not removed by Session stop, failure, or cleanup. Inspect and delete them manually with normal Git tools when safe.

## Manager and child Sessions

A human/operator creates normal Sessions under the same Task with a parent `parent_session_id`. V1 operator surfaces support create, list, prompt, and cancel.

Operational rules:

- exact child IDs are required;
- parent and child must share a Task;
- each child has an independent worker, Port, checkout, status, and stream;
- the gateway only validates membership and routes operator requests through ordinary Session services;
- child output remains on the child stream and is not delivered automatically to the parent provider.

V1 exposes no provider-facing manager tools and has no child wait/result-routing primitive. A manager adapter, mailbox, peer-to-peer or sibling messaging, broadcast, and offline delivery require explicit post-V1 integrations.

## Structured and native terminals

`acpctl session attach` is the managed structured terminal. It consumes the same Session Channel as web.

A provider-native CLI can still be launched directly in an ordinary terminal. That process is provider-owned and unmanaged:

- it is not attached to the Task Console Session;
- its output is not imported;
- the gateway does not claim shared memory or history;
- the user is responsible for avoiding checkout conflicts.

Do not try to use a native CLI as a concurrent second writer to a managed Session.

## Web server

```bash
acpctl serve
acpctl serve --listen 127.0.0.1:0
```

Only explicit loopback IPs are accepted. Stopping the web process detaches viewers only.

The browser opens the Session stream before enabling Send. The current implementation may use SSE for frames and HTTP for commands. This is one logical Session Channel; WebSocket is optional later.

## Worker lifecycle

Every active Session has one detached worker and one provider Port. Viewer processes are not lifecycle owners.

On normal command startup, stale PID/socket rows are probed but never cleared from worker unresponsiveness alone. A dead worker does not prove that provider descendants exited. The Session is marked `failed` while PID/socket ownership remains as quarantine; Resume and checkout reuse stay blocked until teardown is independently proven or completed manually. Workers are never all restarted automatically, and duplicate ownership fails closed.

If a worker dies during a turn, inspect retained frames and provider state through the provider itself. The gateway does not replay the prompt or reconstruct memory.

## Backup and cleanup

For a consistent backup, stop active workers or use SQLite's supported backup mechanism. Back up `agent.db`, relevant Session logs, and any desired worktrees. Provider conversation/auth state is outside Task Console backup responsibility.

Safe cleanup may remove stale sockets and temporary bridge files only after proving no worker owns them. Never automatically delete:

- provider files or authentication;
- Session logs without an explicit retention action;
- live process markers;
- Git worktrees or branches.

## Troubleshooting

### `acpx` unavailable or incompatible

Confirm:

```bash
command -v acpx
acpx --version
node --version
```

Install the pinned version or set `ACPX_BINARY` to its absolute path.

### Repository rejected

Run `git -C /exact/path rev-parse --is-inside-work-tree`. Select the actual checkout, not its non-Git parent. Accept a UI suggestion only after verifying it.

### Session busy

The active turn owns the Port. Wait for it, cancel it, or retry later. No queued prompt exists.

### Worker unavailable

Inspect the Session, then resume it. An old responsive worker may require a clean stop before the new protocol can take ownership.

### Resume failed

The saved provider reference could not be resumed. The gateway deliberately did not start a replacement conversation. Use provider diagnostics, then create a new Session if continuity is unavailable.

### Provider teardown could not be proven

The worker marks the Session failed and retains its worker ownership when closing the provider process cannot be proven. This is a quarantine: Resume is disabled, stale-worker recovery does not clear it, and the checkout remains exclusively claimed because an old provider process may still be able to write. The current release has no automatic force-recovery path. Inspect and terminate the provider process tree manually, preserve the Session as audit evidence, and use a new checkout/Session; do not clear the SQLite ownership fields or opt into shared-checkout overlap merely to bypass the guard.

### No model or effort choices

The connected Port did not advertise a safe complete control. Leave the control unchanged. Do not hard-code or guess a model ID.

### Output appears only after completion

Verify the client attached `/stream` before prompting and that the adapter forwards delta frames rather than buffering until terminal result.

### Replay gap

Continue live if appropriate and inspect raw local logs. A replay gap is not repaired from provider transcripts.

### Permission appears stuck

Attach another viewer and inspect the watermarked current-permission snapshot. If the Port stopped orderly and released ownership, exact Resume is available. If worker ownership is quarantined, follow the manual teardown guidance above; do not fabricate a provider decision or clear ownership merely to force Resume.

## Release checks

```bash
make fmt-check
make test
make race
make vet
make build
make security-check
make acceptance
```
