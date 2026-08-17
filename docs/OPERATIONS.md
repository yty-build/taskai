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
| `logs/<session>.jsonl` | lifecycle/audit journal, provisional overlay, and disclosed fallback | `0600` |
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

Optional Session provider controls use `--model` and `--reasoning-effort` in
the CLI and matching fields in Web/API. Leave either blank for Provider
default. Codex supports both advertised controls; Claude supports both but its
default effort must be represented by a blank field; OpenCode supports model
only. The pinned Grok adapter exposes no verifiable ACP configuration control,
so Grok/Grok Build require both fields blank. Gemini and custom profiles also
remain provider-default-only until their profiles declare proven native parity.
Requested values are stored on the Session and shown by every transport.

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
| `acpctl session stop <id>` | Stop worker without deleting provider history, gateway journal, or worktree |
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
- `resume` starts a new detached worker that resumes the persisted ACP Session ID
  for the same canonical provider conversation.
- a worker accepts one ACP turn at a time and keeps later ACP prompts in its
  transport-local FIFO.
- an open or detached provider-native process retains exclusive ownership;
  ACP submissions are rejected before admission and never enter that FIFO.
- multiple subscribers receive the same lines.
- a worker stop/crash does not delete JSONL.

Every normal CLI command checks persisted active Sessions. If a PID is dead or its private socket is unreachable, it clears stale ownership, marks the Session stopped, and removes stale runtime files. It does not auto-resume work.

New Sessions record `acp_transport=acpx-runtime-v1` in `metadata_json`. A
Session with an existing ACP ID but no runtime marker is treated as a legacy
CLI-backed conversation and is not blindly resumed: its old detached queue
owner may still be alive. Its provider history and gateway journal remain
readable when their sources are available. Create a fresh Session for normal
use; do not run both transports against the same provider conversation.

## Terminal attachment

Task attachment is a modern developer cockpit over the Task's Sessions. Direct
Session attachment opens the provider's real native terminal by default; use
`--acp` for the structured gateway console:

```bash
acpctl task attach <task-id>
acpctl session attach <session-id>
acpctl session attach <session-id> --acp
```

Use `q`/`Ctrl-C` to detach a gateway view. In Structured ACP, `n` opens that
Session's exact provider-native CLI. When the console itself is inside tmux,
native attachment switches to the one stable `agent-<session-id>` tmux
Session: use `Ctrl-B L` to return to the console or `Ctrl-B d` to detach the
client while the provider keeps running. `acpctl session attach <id>`
reconnects to it while that generation remains current. Detaching either view
never stops the worker, cancels an ACP turn, or stops the still-running native
process. A detached native process retains exclusive ownership until normal
provider exit; do not use `stop` merely to close a UI.

Prompt, permission decision, provider-native attachment, and Resume against a
live worker all negotiate worker protocol v3 and
`exclusive-native-owner-v2` before sending provider input or spawning anything.
That capability proves generation-scoped duplicate-native protection and
cross-surface writer exclusion. If a worker does not satisfy that contract,
those operations fail closed; HTTP provider-write calls return `409` with code
`worker_upgrade_required`, and no prompt, permission decision, native process,
or replacement worker is sent or started. Cancel, Stop, status/read,
conversation/history, permission-list, and stream operations remain available
for inspection and safe migration. Then run, in order:

```bash
acpctl session stop <session-id>
acpctl session resume <session-id>
```

Use the exact Session ID and commands printed by the error. Replacing
`bin/acpctl` on disk does not upgrade an already-running worker process.

Task attach targets one selected Session for every prompt or control action;
there is no broadcast. At wide widths it uses one cohesive two-pane cockpit
with a slim Task status/title header, scannable Session rows, and
selected-Session preview. It stacks cleanly at narrow widths and keeps a useful Session navigator
with the core actions at its supported 40x16 minimum. Smaller terminals show an
actionable size warning while Session work continues. It does not require a
mouse, Nerd Font, or truecolor; status and focus use words/glyphs as well as
color.

From the Task screen, press `n` to add a Session without leaving the console. The wizard chooses the agent, role, repository, isolated worktree setting, permission mode, and optional initial prompt; `Ctrl+S` starts it and opens the new Session automatically.

A mouse click selects a Session without leaving the Task navigator. Press Enter
to open the selected real native harness, `a` to open its structured ACP view,
`n` to add a Session, `p` to compose an ACP prompt, `/` to filter Sessions, `:`
for the searchable substring action palette, and `?` for contextual help. The
help, palette, footer, and actions share one registry. Unavailable native
continuation is labelled, and invoking an action that is not valid for the
current Session produces an actionable notice.

The `--acp` view renders the reconciled conversation projection described in
the next section: committed provider turns plus provisional ACP reasoning,
output, tools, plans, errors, and permission cards. A completed turn marks its
last assistant segment as the response; a turn with no exposed reasoning says
so. Successful tools collapse by default while running/failed tools and
actionable approvals remain prominent. Live-follow occurs only while already at
the bottom; scrolling away freezes the reader and shows a new-activity count.
Press `r` for the bounded gateway raw/debug envelope view; the public runtime
does not provide an exact JSON-RPC packet capture.

## Conversation history and live stream

Structured terminal, web, API, and future mobile clients use one shared
server-side projection. They do not read provider files or merge JSONL locally.

| Source | Operational meaning |
| --- | --- |
| Provider-owned store | Committed user-visible history: verified provider JSONL for Codex/Claude/Grok and verified provider SQLite for OpenCode |
| ACP | Live provisional chunks, reasoning, tools, approvals, and turn state |
| Gateway Session JSONL | Lifecycle/audit evidence, missed-stream recovery, provisional overlay, and fallback |

Initial load reads and verifies a bounded provider snapshot, joins unmatched
gateway activity through the returned journal cursor, and then follows ACP
after it. After every settled ACP turn and provider-native process exit/stop, a
fresh bounded provider snapshot is prepared in the background and atomically
replaces the old projection. There is no visible
clear/reload step. Scroll anchor/live-follow, focus, composer draft/cursor,
row/text selection, pending-approval focus, selected Session/tool, and expanded
reasoning/tool state survive the swap.

The UI always discloses when it is showing live provisional content or the
bounded gateway-history fallback. If a transient refresh fails, an already-open
viewer keeps its current in-memory presentation and marks the refresh as
delayed. A new Session with no conversation activity shows an explicit empty
state rather than a partial or invented conversation.

Viewing and prompting have separate activation boundaries. Opening Web or the
structured terminal only loads the provider snapshot plus gateway JSONL
overlay. Before every ACP prompt after it reaches the FIFO head, the worker
refreshes its idle adapter on the exact persisted ACP Session; if refresh fails
it performs exact Resume. A successful
continuity step records correlated `acp_rehydrated` (`refreshed` or `resumed`)
before `turn_started`. A failed rehydration sends no provider prompt.

## Provider-native mode

Supported exact resume mappings:

| Profile | Invocation suffix |
| --- | --- |
| `codex` | `resume <provider-conversation-id>` |
| `claude` | `--resume <provider-conversation-id>` |
| `grok` / `grok-build` | `grok --resume <provider-conversation-id>` |
| `opencode` | pinned `npx -y opencode-ai@1.18.18 --session <provider-conversation-id>` |

When a Session has explicit supported controls, the native invocation carries
the same values while resuming this exact ID. ACP first persists the exact
provider identity, applies/validates the controls before the first prompt, and
reapplies them after refresh or exact resume. If configuration fails, the
Session remains saved as failed with its provider identity; Web/API navigate to
that resource and show a fixed actionable message without provider stderr,
credentials, paths, or raw configuration payloads.

Gemini and custom profiles remain usable through ACP but do not have V1 provider-store verification for native attachment.

`acpctl session <id>` shows the Task Console ID, immutable canonical provider conversation, identity source, and adapter evidence. Native attach requires a canonical ID and verifies that exact ID plus checkout in the provider's local store. It never picks the most recent conversation and never starts a replacement when verification fails.

```bash
acpctl session attach <id>
```

With tmux installed, the command creates or reconnects to a stable `agent-<session-id>` tmux session. Detach it without stopping the provider:

```bash
Ctrl-B d
```

Native and ACP are two transports to the same canonical provider conversation.
Multiple viewers may coexist, but provider writes are exclusive per Session:

- later ACP prompts still wait behind an active ACP turn because the public ACP
  runtime supports one active turn per Session;
- while a native owner or pre-launch reservation exists, ACP submission is
  rejected before journal or queue mutation with exit-and-retry guidance;
- while ACP is accepted, queued, preparing, or running, native creation or
  activation is rejected immediately with wait-then-open guidance;
- neither rejection pauses, cancels, or takes over the active surface;
- detaching a tmux viewer leaves the provider process and its native ownership
  intact.

Starting or activating a new native generation while an ACP request is
accepted, queued, preparing, or running is rejected immediately, not queued.
No native provider process starts and the ACP request is not cancelled. Wait
for the correlated ACP request to settle, then retry `session attach` so the
new process resumes a current provider snapshot.

ACP submission while native owns the Session fails immediately with
exit-and-retry guidance. Return to the original provider terminal—or attach to
its verified `agent-<session-id>` tmux harness—then exit the provider normally.
Viewer detach is not release, and the gateway never auto-kills the provider.
After exact owner release, provider-store reconciliation begins. Retry ACP only
after release; at the FIFO head it performs exact Refresh/fallback Resume and
identity validation before `turn_started`. A later native launch verifies the
provider store and runs a fresh `BuildResume` invocation on the same canonical
ID.

If an owner-tag mismatch prevents attachment, do not kill the tmux Session from
that error alone: it may be a legitimate newer generation observed by a stale
viewer. Return to the console, inspect Session/worker status, and retry. If the
worker reports a generation-scoped pre-launch reservation and the exact
`agent-<session-id>` harness is independently known to be absent (for example,
after an unrelated name collision was deliberately closed or renamed), recover
only that reservation:

```bash
acpctl session recover-native <session-id>
acpctl session logs <session-id> --raw
acpctl session attach <session-id>
```

Recovery refuses while the exact tmux harness exists, tmux inspection is
unavailable or uncertain, or the worker says the generation became active. A
successful recovery revokes the exact reservation and starts provider-store
reconciliation. Do not retry another surface until the recovery command reports
release; a later ACP request still performs exact Refresh/fallback Resume before
dispatch. If a concurrent safe release already won, the command explicitly
reports that state. Missing tmux is never evidence that an active provider
process exited: an active or legacy native-harness marker remains blocked
against replacement native creation and ACP admission until the real provider
process completes its normal release. There is intentionally no force flag.

The tmux Session receives the invoking `AGENT_HOME` and `PATH`, rather than
inheriting stale values from an older tmux server. Reload and reconciliation
are time-bounded. Snapshot failure keeps the current validated presentation and
emits `provider_transcript_reconcile_warning`. After native ownership has
released, presentation reconciliation does not replace the independent exact
ACP rehydration gate. An uncertain ACP transport fails its affected requests
with correlated `turn_error` records and stops for exact resume rather than
substituting a conversation.

```bash
acpctl session logs <id> --raw
```

On provider exit, look for `native_released`, followed asynchronously by
`provider_transcript_reconciled` or
`provider_transcript_reconcile_warning`. Reservation recovery similarly emits
`native_reservation_recovered` before its asynchronous reconciliation result.
The release record is the ownership boundary; the reconciliation result is the
display-history result. Pre-prompt ACP continuity is represented separately by
correlated `acp_rehydrated`, followed by `turn_started`. Continue through
`--acp` if native verification is unavailable; never substitute another
identifier.

## Review operations

Stop or complete the producer, then create an independent reviewer:

```bash
acpctl session stop <producer-id>
acpctl session review <producer-id> --agent claude
```

`--allow-active-producer` is an explicit unsafe-state override when concurrent review is intentional. It also permits the shared checkout needed for that case. The review snapshot may become stale immediately, so the reviewer prompt tells the agent to rerun Git inspection.

Review evidence includes the producer's JSONL audit path plus its repository,
worktree, branch, status, and captured Git diff. Treat the evidence as read-only;
repair work belongs in another implementer Session.

## Local web server

```bash
acpctl serve
acpctl serve --listen 127.0.0.1:0
```

Use the exact URL printed. The handler rejects arbitrary Host headers; `localhost` may therefore be rejected even though it resolves locally. Browser mutations must be same-origin. Closing the browser or sending SIGINT/SIGTERM to `acpctl serve` has no worker lifecycle effect.

The browser Session page is a conversation-first structured ACP view over the
same server-joined projection as the terminal: committed provider history,
gateway-only overlay, then live ACP. It coalesces assistant chunks, shows
provider-exposed reasoning live and in event order, marks provisional versus
committed turns, merges bounded tool cards, pins actionable permissions above
the composer, and keeps IDs/checkouts, source/fallback state, and bounded
sanitized activity summaries in Details and Diagnostics. Reconciliation is an
atomic background swap and preserves scroll/follow position, focus, draft,
selection, approval focus, and expanded disclosures. Reasoning is collapsible
but open by default; if an adapter emits none, the page says so. It
intentionally does not reveal provider errors, ACP JSON-RPC, worker sockets,
native identifiers, provider/JSONL paths, commands derived from raw input, raw
tool objects, or structured environment/auth fields. Allowlisted
provider-visible text, diffs, and terminal output may still contain sensitive
repository or tool-produced content, so the loopback browser remains a
same-trusted-user surface. Use the local terminal `--raw` log view for forensic
inspection.

On mobile, Task/Session navigation becomes a focus-managed drawer while
response cancel/resume controls remain next to the composer. Closing the page
only detaches the viewer. The browser hydrates the conversation endpoint,
follows ACP from its returned gateway cursor, and deduplicates gateway deltas by
monotonically increasing Session position; native EventSource reconnects
advance with `Last-Event-ID`.

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

**Prompt says queued:** another ACP turn occupies the public runtime's
one-active-turn slot, or the provider/runtime itself returned a queued state.
Native ownership is never represented as a queued ACP request: it returns an
immediate conflict before admission. Follow an accepted request ID and do not
resend it while its recorded state is unresolved.

**Cancel says no active turn:** the Session is idle; send the next prompt normally.

### Git

**Not a repository:** pass the repository folder or any path inside a real Git
worktree; the service resolves its root. When a selected parent contains
exactly one immediate repository, Web offers its canonical path but requires
you to choose it and submit again. Multiple or deeply nested repositories need
an exact path. Agent Task Console never runs `git init` for this field.

**Checkout conflict:** choose `--worktree`, stop the owner, or knowingly use `--allow-shared-checkout`.

**Worktree path/branch exists:** preserve or remove the existing user data manually, then start a new Session. V1 does not overwrite it.

### Native mode

**Canonical provider conversation missing:** continue with `--acp` until the provider establishes an exact identity. Do not invent one.

**Unsupported profile:** use ACP; do not invent provider flags.

**Provider-store verification fails:** the gateway found no exact canonical-ID/check-out match and intentionally did not launch native mode. Diagnose the provider's local state or continue with `--acp`.

**Native start rejected while ACP is accepted/queued/preparing/running:** let
the correlated ACP request settle, then retry `session attach`. The native
request was not queued, no provider process started, and ACP was not cancelled.

**ACP prompt is rejected while native is open or detached:** return to the
original provider terminal, or its verified `agent-<session-id>` tmux harness,
and exit the provider normally. The rejected prompt was neither journaled nor
queued. After exact release, retry; the prompt performs exact ACP rehydration
before dispatch.

**tmux owner mismatch/name collision:** an existing `agent-<session-id>` tmux Session has a different protocol, state-root, or generation tag. The gateway refuses to attach. Return to the console, inspect Session status, and retry; do not kill it from the mismatch alone because it may be a legitimate newer harness. Close or rename only a separately verified unrelated collision. Run `recover-native` only when the worker reports a pre-launch reservation and the exact harness is known absent.

**Reconciliation/recovery warning after native exit:** inspect raw logs and the
conversation source state. The current validated transcript remains visible
after the background reconciliation warning, and a later page/Session reload
retries the provider-store read. Confirm that native ownership released before
retrying ACP; the exact Refresh/fallback Resume boundary remains mandatory. If
the ACP transport itself reports identity failure, run
`acpctl session resume <id>` to establish the exact persisted identity before
sending another prompt.

### Logs and streams

**Replay fails on corrupt JSONL:** copy the file for diagnosis. Records are not skipped because doing so could mis-correlate later positions.

**Gateway-history fallback is shown:** provider-store verification or reading
failed, so the UI is intentionally using the gateway lifecycle/provisional
journal. Preserve the provider store and inspect diagnostics; do not describe
the fallback as the canonical provider transcript.

**Provider history refresh is delayed:** an already-open viewer keeps its
current in-memory presentation while refresh is retried. Continue reading
without reloading; do not expect provisional turns to become committed until
reconciliation succeeds.

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
