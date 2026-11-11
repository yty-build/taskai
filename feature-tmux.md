# Feature specification: simple tmux-backed sessions

**Status:** Implemented and acceptance-tested
**Date:** 2026-09-03
**Primary constraint:** tmux is the only new runtime-persistence mechanism. This feature
adds no TaskAI daemon, resident TaskAI server, queue, or database.

## 1. Decision

Every TaskAI Manager and Subordinate runs in its own tmux session. A Task groups
those agent records without an agent tmux session of its own. It may additionally
own one lazily opened utility Terminal, stored separately on the task in JSON.

TaskAI records each agent's exact tmux locator in the existing JSON registry. The TaskAI
TUI connects to that tmux session as a disposable viewer:

~~~text
TaskAI viewer -> portable PTY -> local or SSH tmux attach -> native provider CLI
~~~

Leaving TaskAI closes the viewer. It does not close tmux or the provider CLI. Opening
TaskAI again inspects the saved locator and attaches to the same process and terminal.
Only an explicitly confirmed Destroy operation asks tmux to stop a recorded runtime.

tmux's normal background server is the durable process owner. There is no persistent
TaskAI controller behind it.

This specification supersedes only the two lifecycle statements in
backend_communication.md that call tmux a non-production dependency and demo evidence
only. Its provider modes, ACP/display conduit, correlation, completion, cancel, context,
and handoff contracts remain normative.

## 2. Product contract

The required behavior is:

1. One tmux session exists for each Manager or Subordinate that has been launched.
2. A Task groups its Manager and Subordinate records in JSON. Its optional utility
   Terminal is a separate tmux session, not an agent or team member.
3. q means Leave TaskAI. Running tmux sessions and provider processes continue.
4. Closing the terminal or losing the SSH viewer also leaves the tmux runtime running,
   subject to the normal limitations of the host and tmux itself.
5. Reopening TaskAI attaches the recorded live runtime. It must not launch a duplicate
   provider while that runtime is alive.
6. Destroying a Subordinate stops only that Subordinate's recorded tmux session.
7. Destroying a Manager stops that Manager and the Subordinates that report to it.
8. Destroying a Task stops all and only its recorded Manager, Subordinate, and
   optional utility Terminal runtimes. Manager destruction never stops Terminal.
9. Destroy never deletes or modifies repositories, worktrees, branches, working files,
   git history, or provider-native conversation history.
10. Provider identity remains separate from tmux identity. A tmux locator finds a live
    terminal; a provider session ID is retained for provider-native resume when needed.
11. TaskAI never falls back from an unavailable SSH target to a local runtime.
12. Existing provider and ACP/conduit behavior remains in place. tmux changes process
    lifetime and attachment, not the approved provider protocols.

tmux protects a running process from TaskAI exit and connection loss. It does not survive
a machine reboot, host failure, or an explicitly killed tmux server.

## 3. Runtime topology

One Task with two Managers and three Subordinates is represented as:

~~~text
Task t1                                      JSON/UI grouping only
├── Manager s1  -> tmux session s1
│   ├── Subordinate s2 -> tmux session s2
│   └── Subordinate s3 -> tmux session s3
└── Manager s4  -> tmux session s4
    └── Subordinate s5 -> tmux session s5
~~~

Each session contains one native window and one provider-facing pane. Provider-specific
launch mechanics remain unchanged. For local OpenCode, the existing `serve` and
`attach` pair lives together inside that one tmux runtime; this feature does not expose
either process as another TaskAI session.

TaskAI uses a private tmux server name derived from TASKAI_HOME and invokes tmux with an
explicit server name and no user tmux configuration. This keeps TaskAI sessions separate
from the user's normal tmux server. The exact TaskAI session ref, such as s1, is also the
tmux session name.

The private namespace and exact internal refs protect against accidental broad
operations. They are not a hostile same-user security or ownership system.

## 4. Repository and connection are separate

A Task stores two independent values:

| Field | Local Task | Remote Task |
|---|---|---|
| Repository entry | Absolute path on the local machine | Absolute path, ~, or ~/path on the remote host |
| Repository stored | Canonical absolute local path | Canonical absolute remote path |
| Connection entry | Empty | ssh cruise2 |
| Connection display | <local> | ssh cruise2 |

An empty connection always means local. The user does not type a local shell command.

For an SSH connection, TaskAI accepts the form ssh HOST, where HOST is a safe SSH-config
alias such as cruise2. The registry stores the target structurally as an SSH host alias
and reconstructs the display command. It does not store passwords, private-key contents,
or expanded credentials. User names, ports, jump hosts, keys, and other connection
options belong in the user's SSH configuration.

The repository path is interpreted on its recorded target. A remote path does not need
to exist locally. Before creating a remote Task, TaskAI checks that tmux is available,
expands `~` against the remote account when present, and verifies that the directory
exists on that host. The resolved absolute remote path is stored in the tracker.

Remote Subordinates currently use the Task repository. Creating a new remote git worktree
or choosing a different remote repository is outside this small lifecycle feature.

## 5. Existing JSON registry

State continues to live in TASKAI_HOME/state/state.json, normally
~/.taskai/state/state.json. The existing atomic JSON persistence remains the only TaskAI
state store.

The model adds:

- connection on each Task; and
- an optional tmux runtime locator on each Manager or Subordinate session.

An illustrative remote record is:

~~~json
{
  "tasks": [
    {
      "task_ref": "t1",
      "name": "Remote evaluation",
      "repo": "/srv/taskai",
      "connection": {
        "kind": "ssh",
        "host": "cruise2"
      }
    }
  ],
  "sessions": [
    {
      "session_ref": "s1",
      "task_ref": "t1",
      "parent": null,
      "role": "Manager",
      "harness": "claude",
      "workspace": "/srv/taskai",
      "provider": {
        "provider": "claude",
        "provider_session_id": "provider-native-id",
        "runtime_id": "tmux:taskai-0f83d9af:s1",
        "connector_version": "2.1.0"
      },
      "runtime": {
        "target": {
          "kind": "ssh",
          "host": "cruise2"
        },
        "private_server": "taskai-0f83d9af",
        "session": "s1"
      }
    }
  ]
}
~~~

The full serialized records also contain the existing source, state, connector version,
audit, and counter fields. The example omits those unrelated fields for readability.

Old unversioned state remains readable. A missing connection field defaults to local and
a missing runtime field means no durable runtime has been bound yet. This design adds no
schema migration engine, registry identity, generation counter, lifecycle phase,
ownership marker, lock service, or Destroy tombstone.

## 6. Small local/SSH tmux boundary

The implementation is a stateless module inside the existing TaskAI binary:

~~~text
probe target
validate repository on target
create exact session
inspect exact session
build disposable attach command
resize exact session
kill exact session
~~~

Local operations run tmux directly. Remote operations run the corresponding tmux command
through the recorded SSH alias. Interactive remote attachment uses an SSH TTY; control
operations use non-interactive SSH with a bounded connection timeout.

Provider version checks and new provider launches are the narrow exception to the
plain control environment: both enter the remote account's interactive-login shell
to find user-installed executables. The launcher restores the requested repository
after shell startup, suppresses startup banners, then execs the exact provider argv.
This also works when an existing private tmux server inherited an older PATH. It
adds no resident shell wrapper or service and does not change provider/ACP arguments.

Manager/Subordinate Harness selectors retain all four providers, including versions
and explicit unavailability reasons. Left/right keys and clickable chevrons cycle
the catalogue; labels focus without changing selection. Unavailable choices cannot
create registry records or runtimes. F5 performs a bounded background recheck while
retaining the form draft and selected provider. Compatibility gates remain enforced;
TaskAI does not install or upgrade CLIs automatically.

Each command:

- removes inherited TMUX, TMUX_PANE, and TMUX_TMPDIR before addressing the private
  runtime;
- validates internal server/session names and SSH aliases;
- quotes remote arguments rather than interpolating untrusted shell fragments;
- targets the exact session, never a prefix or wildcard; and
- never invokes tmux kill-server.

Creation leaves tmux `remain-on-exit` off, disables the tmux status bar and prefix keys,
and starts the provider in the recorded repository/workspace. A live provider survives
viewer detach; when the provider itself exits, tmux removes that session and the attach
client exits rather than retaining a dead pane that could appear live. The TaskAI viewer
attaches with no tmux prefix, so provider input remains native apart from TaskAI's two
documented focus chords.

## 7. Lifecycle and reconciliation

| Event | Required behavior |
|---|---|
| Create Task | Persist repository and connection; create no tmux session |
| t in task navigation | Open/reuse the task's utility Terminal inline |
| s in Home/task navigation | Open Settings; h and comma remain legacy aliases |
| ? in navigation | Open shortcut Help |
| F1 in Home/task navigation or Settings | Open/reuse the optional setup helper |
| Add Manager/Subordinate | Persist the session and launch one tmux runtime for that agent |
| Ctrl+F in navigation | Open the selected CLI in full-screen focus, reusing its live runtime |
| Enter in navigation | Focus the selected CLI inline, keeping the sidebar visible |
| Ctrl+F while focused | Toggle full-screen and inline CLI layouts without relaunching |
| Ctrl+G while focused | Return to TaskAI navigation; runtime and viewer state continue |
| G in Task navigation | Return Home without changing any runtime |
| q | Drop TaskAI's attach clients and exit; tmux/provider processes continue |
| Reopen TaskAI | Inspect each saved locator and show live only when tmux reports it live |
| Provider exits / recorded runtime is missing | Its tmux session and attach client exit; show it as not live |
| Explicitly open a missing runtime | Recreate it, using the saved provider ID for exact resume where supported |
| SSH target is unreachable | Report the operation error, retain the mapping, and never create a local fallback |
| Destroy | Stop the exact mapped sessions, then remove the selected tracker records |

Reconciliation is intentionally small. It distinguishes a live tmux pane from an exited
or missing session and updates the displayed session state. It does not automatically
restart, kill, or replace an uncertain runtime.

There is no crash transaction. If TaskAI is interrupted during a multi-session Destroy,
the JSON records are retained unless the whole requested kill set completed. On the next
open, ordinary inspection shows which recorded runtimes are no longer live. Recovery is
manual and non-destructive.

After a normal leave, TaskAI reports how many tmux runtimes continue. If none are live, it
reports that no runtimes are running.

## 8. Rendering and input

TaskAI's portable PTY now owns only the local tmux attach client or SSH/tmux attach
client. Dropping that client must not terminate the recorded tmux session.

Opening a task with a session, creating a Manager/Subordinate, or launching a
repository with an initial Lead enters inline CLI focus immediately after a
successful launch/attach. The native cursor is shown when the child reports it
visible. Empty tasks and launch failures stay in navigation. Returning explicitly
with Ctrl+G leaves navigation active until the user chooses a CLI again.

Terminal uses these same focus keys. Its default login shell runs on the Task's
connection target in the repository. It has no provider binding or context hooks.
The lazy `Task.terminal` locator names `tN-terminal` on the private server; it is
persisted before creation, reused after detach, and removed with the task record.
`exit` ends that shell; a later explicit open starts a new shell at the same
locator. No shell is automatically relaunched on viewer startup.

The hardened application launches/attaches in a disposable background worker after
persisting the exact locator. Input during connection is buffered for that session;
Ctrl+G returns to navigation without stopping the runtime. Destruction of a scope
with a pending launch waits for that launch to finish, preventing creation after
tracker removal. Remote provider checks are cached and asynchronous, resize does
not run SSH on the render thread, and q performs no fresh network liveness check.
These are in-process UI workers, not a new daemon or resident TaskAI service.

The focus keys are:

| Context | Key | Action |
|---|---|---|
| Task navigation | Ctrl+F | Focus selected CLI full-screen |
| Task navigation | Enter | Focus selected CLI inline |
| Task navigation | t | Open/reuse Terminal inline |
| Home/task navigation | s | Open Settings (legacy aliases: h and comma) |
| Navigation | ? | Open shortcut Help |
| Home/task navigation or Settings | F1 | Open/reuse the setup helper |
| Focused CLI | Ctrl+F | Toggle full-screen and inline layouts |
| Focused CLI | Ctrl+G | Return to navigation |
| Task navigation | G | Return Home |
| Navigation | q | Leave TaskAI; agents continue |

All other focused input is sent to the native CLI. TaskAI must preserve the existing
cursor bridge, colors, wide cells, terminal size, alternate screen, paste, and input
behavior through the tmux attachment layer.

Focus and selection have separate visual meanings. Exactly one input region has
a steady `▸ NAVIGATION` or `▸ CLI INPUT` header edge. Navigation adds the selected
row's arrow and stronger fill; console focus retains a muted selection without
lighting the sidebar. Harness stripes and state lamps retain their existing
identity/status meaning. The focus edge uses existing chrome cells: no extra
rows, native-grid tint, focus-triggered resize, animation, or polling timer.
The child cursor remains hidden in navigation and follows the child's visibility
in CLI focus. Fullscreen and setup consoles use the same CLI input edge.

### CLI-only wheel history

TaskAI captures terminal mouse events and routes wheel input only to the viewport
under the pointer. Over the CLI, native mouse-aware programs receive the event
once, in their own coordinates. Otherwise the selected private tmux pane enters
its own copy mode to browse history. Its counter stays inside the CLI viewport;
TaskAI's masthead, rail, footer, and selected recipient do not move. An outer tmux
session must not enter copy mode for this interaction.

This applies inline, full-screen, and in Settings setup consoles, locally and over
SSH. While a CLI is focused, Esc or End exits private history; outside history,
ordinary provider input keeps its normal meaning. Wheel inspection from navigation
does not take keyboard focus: Enter focuses the CLI, while navigation Esc continues
to mean Home. Ctrl+G remains the focus boundary. Mouse presses are released when
focus or viewport geometry changes, preventing a stuck drag in the child program.

History belongs to tmux: no second transcript store, polling service, or daemon is
introduced. History bindings are applied when attaching existing runtimes too.
Acceptance checks and limitations are recorded in
[CLI scroll evidence](tests/evidence/2026-09-07-cli-scroll.md).

Redraws are driven by coalesced output notifications from the visible PTY, not a
100 ms input-poll timeout. Host input is forwarded before rendering, with bounded
batches so a sustained wheel stream cannot starve display updates. The first
frame after idle is immediate; bursts are combined on a 16 ms render cadence to
reduce redundant frames and tmux output congestion. Animation and housekeeping
keep a separate 100 ms tick; hidden CLIs do not wake the renderer. Style runs
share ANSI colour/attribute sequences without changing terminal cell semantics.
No daemon, transport, dependency, or additional history database is introduced.
Measurements are observations, not a zero-latency guarantee; see
[scroll performance evidence](tests/evidence/2026-09-07-smooth-scroll.md).

## 9. Preserve provider and ACP behavior

backend_communication.md remains the authority for provider communication. This feature
does not delete, replace, newly activate, or silently disable any existing path:

- Claude Code remains owned-tui.
- Codex remains shared-live through its existing app-server/ACP path.
- OpenCode retains its existing connector behavior; locally, its `serve` and `attach`
  pair lives together inside the agent's tmux runtime.
- Grok retains its existing ACP handoff and exact native resume path.

Human keystrokes reach the provider through the tmux attachment. Automated prompt and
cancel behavior must continue through the existing provider-specific conduit; it is not
replaced with generic tmux send-keys.

The existing TUI-owned generic control socket currently supports Claude only while
TaskAI is open. It is not a new daemon and it is removed on a normal leave. Commands that
require that open-TUI conduit continue to require TaskAI to be open; q does not turn the
control socket into a resident service.

Local OpenCode's existing `serve` process lives alongside its `attach` process inside
the agent's tmux lifecycle so both survive viewer detach. That is process placement, not
a new TaskAI service, a parent-held facade, or a provider-protocol redesign.

SSH support in this feature proves remote terminal creation, attachment, persistence,
reconnection, and scoped destruction. It does not claim new remote context/bootstrap,
hook delivery, or ACP behavior beyond what the existing provider path already supports.
Remote OpenCode is currently refused because its existing TaskAI launch helper is not
installed on the target; this feature does not replace that helper with new infrastructure.
Any real-provider remote proof remains separately approved and billed.

## 10. Destroy semantics

TaskAI first resolves the selected scope through the JSON topology:

| Selected scope | Exact kill set |
|---|---|
| Subordinate | That Subordinate's runtime |
| Manager | That Manager plus its reporting Subordinates |
| Task | Every Manager and Subordinate in that Task |

For every mapped runtime, TaskAI drops its disposable viewer and invokes kill-session
against the exact private server and exact session ref. An already missing runtime is
treated as stopped. TaskAI removes the selected registry records only after every kill
operation returns successfully.

If an SSH connection or tmux operation fails, TaskAI reports the failure and preserves
the tracker records. Some earlier exact sessions in a multi-session request may already
have stopped; the next startup reconciles those mappings as not live. TaskAI does not add
a tombstone, rollback engine, or background recovery worker.

Destroy never:

- calls kill-server;
- targets the user's default tmux server;
- kills by task, manager, role, or repository display name;
- types q, exit, or a provider command into the pane; or
- deletes repository/worktree/history data.

When the final session on a private server is killed, tmux may exit that empty server
naturally.

## 11. Local and SSH support boundary

For a local Task, tmux and the selected provider CLI must be installed locally.

For a remote Task:

- ssh HOST must work non-interactively through the user's SSH configuration;
- tmux and the selected provider CLI must exist on that host;
- the repository is an absolute directory path on that host;
- TaskAI stores only the safe host alias;
- loss of the local viewer or SSH connection leaves remote tmux running; and
- TaskAI performs no software installation and copies no credentials.

The non-billing cruise2 acceptance proof uses a deterministic fake CLI. It proves the
tmux/SSH lifecycle without spending a model turn. It does not by itself prove every
provider's billed remote workflow.

## 12. Deliberate non-goals

To keep this change small, the following are explicitly outside scope:

- simultaneous TaskAI interfaces controlling the same TASKAI_HOME;
- hostile same-user tmux ownership collisions or cryptographic ownership markers;
- registry locks or multi-controller synchronization;
- lifecycle generations, PREPARED/LAUNCHING phases, transaction logs, or tombstones;
- a TaskAI daemon, remote TaskAI daemon, resident RPC server, queue, or database;
- automatic restart, scheduling, hibernation, or reboot recovery;
- a tmux session for a Task grouping;
- arbitrary SSH command strings or credentials in JSON; and
- redesigning, removing, or opportunistically activating existing ACP/provider paths.

Running multiple TaskAI UI processes against one registry is unsupported. A same-user
process that deliberately controls the known private tmux server is also outside the
threat model.

## 13. Acceptance

TEST_TMUX.md is the pre-implementation acceptance contract. It covers:

- local and ssh cruise2 target parsing and display;
- one tmux session per Manager/Subordinate;
- q, process loss, reattach, and draft/PID continuity;
- exited/missing/unreachable reconciliation;
- Subordinate, Manager, and Task destruction;
- repository preservation and exact tmux isolation;
- rendering, cursor, resize, and input behavior;
- regression coverage for all existing connectors and context paths; and
- proof that no model is called by the feature tests.

The release gate is:

~~~sh
cargo fmt --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace --locked
cargo build --release --locked
~~~

Real Claude, Codex, OpenCode, or Grok turns are not part of the free gate and still
require the existing explicit approval protocol.

## 14. Definition of done

The approved feature is complete when:

- local and cruise2 fake runtimes survive q and loss of the viewer;
- reopening reaches the same tmux session and process without a duplicate launch;
- repository and connection render separately as an absolute path plus <local> or
  ssh HOST;
- each Manager/Subordinate has one independently addressable locator in JSON;
- scoped Destroy stops the exact recorded sessions and preserves repository data;
- missing sessions and unreachable hosts never produce a false live claim or local
  fallback;
- the user's default local and remote tmux sessions remain untouched;
- terminal rendering and focus behavior remain usable through tmux;
- existing provider identity, ACP/conduit logic, hooks, and capability labels remain
  intact; and
- all non-billing acceptance and repository-wide checks pass.
