# Feature specification: tmux-backed durable sessions

**Status:** Proposed for approval  
**Date:** 2026-09-01  
**Primary constraint:** tmux is the only **new** persistence layer. This feature adds no
TaskAI daemon, resident control service, RPC server, queue, or database, and it does not
redesign the approved ACP/display conduit.

## 1. Decision

TaskAI will launch every Manager and Subordinate inside tmux. The TaskAI TUI is only a
viewer and controller that can come and go. Leaving TaskAI disconnects its tmux clients;
it does not terminate provider processes.

TaskAI records the exact tmux locator in the existing JSON registry. Destroy uses that
record, plus ownership markers read back from tmux, to terminate only the selected
TaskAI runtime. The same locator shape supports a future tmux instance reached over SSH.

There is deliberately no persistent TaskAI process. tmux's normal background process is
the only resident infrastructure.

tmux owns each runtime process technically. TaskAI may ask tmux to terminate that native
runtime only after an explicit, scoped, typed Destroy confirmation. Merely leaving,
detaching, navigating, crashing, or losing a terminal never grants that authority.

## 2. Why this feature is necessary

The current process hierarchy is:

```text
taskai TUI
└── portable PTY
    └── Claude / Codex / OpenCode / Grok
```

`Pane::drop()` kills its child. A controlled live test confirmed that a child doing
long-running work survives `Ctrl+G`, but dies when the user then presses `q`. This makes
the current README and UX promise that leaving merely detaches untrue.

The replacement hierarchy is:

```text
taskai TUI or taskai command (short-lived)
             │
             │ tmux CLI locally, or later through SSH
             ▼
TaskAI-private tmux namespace
├── runtime for s1
│   └── native window: Claude
├── runtime for s2
│   ├── native window: Codex
│   └── existing connector process group (IPC unchanged)
└── runtime for s3
    ├── native window: OpenCode attach
    └── existing serve/facade process group (HTTP/SSE unchanged)
```

After `q`, there must be no TaskAI TUI, TaskAI daemon, or TaskAI control-server process.
The tmux server, provider CLIs, and connector processes already required by the approved
mode continue.

This feature does not create a new central `taskai`/`taskaid` persistence controller, RPC
server, watcher, queue worker, or proxy. Existing connector bridge processes are not a new
control plane: Codex shared-live, OpenCode `serve` + `attach`, Claude owned-tui, and Grok
handoff-resume retain their approved mechanics. This feature may reparent those existing
processes inside the agent's tmux runtime so their lifecycle survives detach; it does not
replace their protocol, correlation, safety, or rendering mode.

## 3. Product contract

The following are non-negotiable:

1. `q` means **Leave TaskAI**. It never kills a Manager, Subordinate, provider turn,
   connector companion, provider child, tmux session, or tmux server.
2. Closing the terminal, losing SSH, `SIGHUP`, `SIGTERM`, or a TUI crash has the same
   worker-lifecycle result as `q`: the tmux runtime continues.
3. `Ctrl+G` returns to TaskAI navigation and leaves the selected runtime untouched.
   `Ctrl+C` in navigation leaves TaskAI; `Ctrl+C` while the CLI has focus remains native
   provider input, exactly as it is today.
4. Destroy is the only TaskAI UI operation that terminates an agent runtime. A provider
   may still exit itself, and tmux or the machine may still fail independently.
5. Destroying a Task terminates only that Task's recorded Manager and Subordinate tmux
   runtimes. Other TaskAI Tasks and the user's own tmux sessions are untouched.
6. Destroy never deletes or modifies repositories, worktrees, branches, uncommitted
   files, git history, or provider-native conversation history.
7. A running tmux runtime is reattached. TaskAI must not launch a second provider process
   or invoke provider resume while the recorded runtime is alive.
8. tmux identity and provider identity remain separate. A tmux locator finds the running
   terminal; a provider session ID supports exact recovery after that terminal is gone.
9. TaskAI never silently falls back to today's volatile direct-PTY ownership. If tmux is
   missing or unsupported, stored Tasks remain viewable but new runtimes cannot launch.
10. tmux protects work from TaskAI exit and terminal disconnection. It does not survive a
    machine reboot, a host failure, or the tmux server being killed.

This feature intentionally does **not** introduce automatic agent restart, scheduling,
hibernation, a transcript store, or a provider-neutral message bus.

## 4. Explicitly superseded decisions

Once approved, this specification supersedes these existing statements:

- `backend_communication.md` section 1: "Production has no tmux dependency."
- `backend_communication.md` section 11: tmux is evidence only, not production.
- The build and test plans' "no tmux dependency" and "tmux is a test driver only"
  statements. Those documents recorded the completed M0-M9 build and have been removed
  from the repository; their operational parts now live in `CONTRIBUTING.md`.

Sections 2–9 of `backend_communication.md` remain normative and are explicitly **not**
superseded: connector modes, ACP/display conduit behavior, provider correlation,
completion/cancel safety, context delivery, and handoff semantics stay intact. The only
new lifecycle boundary is that tmux owns the runtime technically and TaskAI may terminate
it only through explicit, scoped, confirmed Destroy. Provider-native history remains
authoritative, TaskAI does not invent work, and repositories/worktrees are preserved.
The implementation milestone updates only the conflicting tmux/lifecycle statements and
README wording.

Implementation is not authorized to delete or replace the Codex ACP/app-server path,
OpenCode authenticated serve/attach path, Claude owned-tui conduit, Grok ACP
handoff-resume path, their tests, or their capability labels. Connector edits are limited
to process placement inside tmux, durable locator/reconnection plumbing, and the lifecycle
fences required for safe Destroy.

## 5. Why one tmux session per TaskAI session

| Topology | Benefit | Cost | Decision |
|---|---|---|---|
| One tmux session per TaskAI Task, one window per agent | Mirrors the visible hierarchy | Window focus, resize, sidecars, and partial destruction become coupled | Reject |
| One tmux session with many panes | Compact | All agents share geometry and lifecycle; unsafe blast radius | Reject |
| One tmux session per Manager/Subordinate | Independent lifetime, sizing, attach, recovery, and exact destruction | More tmux sessions | **Adopt** |

The Task remains a JSON/UX grouping. It does not need its own tmux object. A Task destroy
is an enumerated, journaled operation: it kills each verified registered runtime,
children first. It is intentionally not presented as one atomic tmux operation.

Each agent tmux session has:

- one window named `native`, containing the real provider CLI;
- the same logical connector companion process group already required by that harness's
  approved mode, if any;
- `remain-on-exit` enabled so a naturally exited CLI retains its last screen and exit
  status for diagnosis and explicit resume;
- TaskAI ownership markers on the session, windows, and panes.

Killing that one tmux session therefore closes that agent and its existing connector
companions without affecting peers. “Companion” is a lifecycle member, not necessarily a
tmux window: stdio pipes, sockets, PTYs, and parent/child ownership remain with the
unchanged connector launcher. The tmux feature only gives that approved launcher/process
group a durable process parent.

## 6. Private tmux namespace

TaskAI must not use the user's default tmux server. Each registry gets a stable, generated
server name such as `taskai-a8f14c2e`. Every invocation uses exactly `tmux -L <name>`;
there is no implicit/default-server path and no second socket strategy. TaskAI removes
`TMUX_TMPDIR` from every control and attach-client invocation so the same name always
resolves through tmux's standard owner-only socket directory.

The private namespace is still ordinary tmux. It does not add another service or protocol.
It provides two important guarantees:

- TaskAI cannot accidentally attach to or destroy the user's normal tmux sessions.
- TaskAI controls the small set of tmux options needed for terminal fidelity instead of
  inheriting arbitrary user bindings and hooks.

The private server starts without the user's tmux configuration. TaskAI writes one small,
owner-only `$TASKAI_HOME/run/tmux.conf` containing the registry/schema ownership marker
and fixed options, then always names it explicitly:

```text
tmux -f $TASKAI_HOME/run/tmux.conf -L <server-name> ...
```

The config sets:

- server-scoped `@taskai_server_registry_id` and `@taskai_server_schema` before any
  runtime is created;
- `status off`;
- `prefix None` and `prefix2 None`, so native CLI keys pass through;
- `automatic-rename off` and `allow-rename off`;
- `destroy-unattached off` and `exit-unattached off`, so detach never destroys work;
- global `remain-on-exit on`, so even a fast-failing first provider leaves a diagnosable
  dead pane;
- `exit-empty off` only during first-server bootstrap.

Before any server mutation, TaskAI probes with `-N`. If a server already exists, its
global registry and schema markers must match before TaskAI changes options or sessions.
If no server exists, TaskAI starts it with the config and immediately reads those markers
back. If another same-user process won that race, tmux ignores TaskAI's config for the
already-running server, the markers do not match, and TaskAI returns `OWNERSHIP CONFLICT`
without mutation.

After the first fully tagged session is verified, TaskAI sets `exit-empty on`. A crash
during bootstrap may leave only an empty, correctly marked tmux server; reconciliation
either continues the recorded create or sets `exit-empty on` so that empty server exits.
Thus no cleanup daemon or bootstrap shell is required.

For a verified server, `new-session`, session/window/pane tagging, and per-window sizing
are submitted as one tmux command list. The existing runtime-launcher executable and each
argument are passed as separate `new-session` arguments, which tmux executes directly without
`sh -c`; display names and user text never become a shell command. The underlying
start/retain/kill/exit sequence has been checked against tmux 3.5a, including natural
server exit after its last exact session is killed.

Do not set global `window-size manual` before creating windows. tmux 3.5a can crash in
that sequence. Create each detached native window with explicit dimensions, then set
`window-size manual` on that window only and resize it explicitly. Viewer clients attach
with `-f ignore-size`, so a stale or crashing viewer cannot change provider geometry.
Truecolor, UTF-8, cursor, bracketed paste, and resize fidelity come from terminal-feature
negotiation plus tests, not from a generic tmux option claim.

When TaskAI itself runs inside the user's tmux, it removes inherited outer `TMUX` and
`TMUX_PANE` from the environment of private tmux control and attach-client invocations.
Processes created by the private server receive that server's normal `TMUX` and
`TMUX_PANE`; TaskAI does not use those variables as ownership proof. The native pane also
receives nonsecret `TASKAI_SESSION_REF` and `TASKAI_RUNTIME_GENERATION` bindings so stale
hook events can be rejected.

Native and connector launcher processes must receive the same inherited environment they
receive in the approved direct-launch design, except that inherited outer `TMUX`,
`TMUX_PANE`, and `TMUX_TMPDIR` are replaced by tmux's correct private-runtime values.
This feature must not introduce a provider-variable allowlist or drop arbitrary auth,
proxy, PATH, shell, plugin, or connector configuration.

Because a long-lived tmux server can otherwise retain stale values from its first start,
runtime creation copies the complete current launch environment into the new tmux session
through tmux's environment-update mechanism. Only variable **names** may appear in tmux
commands/config; secret values stay in process/session environments and never enter argv,
JSON, config, audit, or logs. Names present in an older tmux environment but absent from
the current launcher are explicitly removed. Nonsecret `TASKAI_HOME`, session ref, and
generation are added. Existing runtimes keep their launch environment; later viewer
attaches use `-E` and cannot refresh it accidentally.

The nested viewer command is equivalent to:

```text
env -u TMUX -u TMUX_PANE -u TMUX_TMPDIR \
  tmux -N -L <name> attach-session -E -f ignore-size -t <exact-session>
```

`-N` prevents attach from creating a replacement server and `-E` prevents the viewer
from replacing the durable runtime environment. Exact flags remain covered by the
minimum-version capability test.

This is an ownership and isolation guard, not a security boundary: another process
running as the same OS user can still discover and control the private tmux server.

The initial supported platform is macOS/Linux with a capability-tested tmux. The current
development machine has tmux 3.5a. The final minimum version is chosen from the required
capabilities and CI matrix, not by accepting an untested version string.

## 7. JSON registry model

The registry remains `~/.taskai/state.json`. It gains a schema version, a stable
registry identity, and an optional runtime binding on each TaskAI session.

Illustrative shape:

```json
{
  "schema_version": 2,
  "registry_id": "r-7b2d4f...",
  "sessions": [
    {
      "session_ref": "s3",
      "task_ref": "t1",
      "parent": "s1",
      "role": "Subordinate",
      "harness": "opencode",
      "workspace": "/repo/worktrees/verification",
      "provider": {
        "provider": "opencode",
        "provider_session_id": "provider-native-id",
        "connector_version": "1.18.25"
      },
      "runtime": {
        "backend": "tmux",
        "phase": "RUNNING",
        "target_id": "local",
        "transport": { "kind": "local" },
        "server_name": "taskai-a8f14c2e",
        "session_name": "rt-s3-31cc0a",
        "session_id": "$4",
        "native_window_id": "@7",
        "native_pane_id": "%12",
        "generation": "31cc0a...",
        "last_observed": {
          "runtime": "RUNNING",
          "at": "2026-08-31T23:30:00Z"
        }
      }
    }
  ]
}
```

Rules:

- `provider_session_id` is never replaced by a tmux ID.
- The current `runtime_id: pid:<taskai-pid>` is deprecated; a TUI PID is not durable.
- Task names, Manager names, roles, and repository names never enter tmux identifiers.
  Names come only from internal stable refs plus a random generation.
- PIDs and pane IDs are observations, not sufficient ownership proof.
- The verified tmux session is the lifecycle boundary for the connector's existing
  process group; JSON does not remodel its internal pipes/endpoints as tmux objects.
- Credentials, prompt content, SSH keys, passwords, and transcript content never enter
  this JSON.
- Writes remain temp-file plus atomic rename, but all read-modify-write mutations first
  take an owner-only advisory file lock and reload the latest revision.
- Create recovery uses the session's small `PREPARED`/`LAUNCHING` runtime phases; it does
  not need a job queue. One scoped `pending_destroy` tombstone records a confirmed
  destruction until it is fully accounted for.

There are exactly two new owner-only lifecycle locks:

- `$TASKAI_HOME/run/tui.lock` is a lifetime lease allowing one interactive TaskAI viewer;
- `$TASKAI_HOME/run/state.lock` is held only for a brief
  reload/read-modify-write/atomic-persist cycle, including the final Destroy/dispatch
  lifecycle fence.

Hooks and context reads never wait on `.tui.lock`. Existing connector writer exclusion,
human-focus rules, busy refusal, and cancel behavior remain exactly in `Conduit`; the tmux
feature does not replace them with an OS writer lock or broaden their critical section.

TaskAI uses distinct user-option names at each scope and reads the exact scope back before
attach, resize, prompt, or Destroy:

```text
server (-s): @taskai_server_registry_id, @taskai_server_schema
session:     @taskai_session_registry_id, @taskai_session_task_ref,
             @taskai_session_ref, @taskai_session_generation
window (-w): @taskai_window_registry_id, @taskai_window_session_ref,
             @taskai_window_generation, @taskai_window_role=<native|connector-role>
pane (-p):   @taskai_pane_registry_id, @taskai_pane_session_ref,
             @taskai_pane_generation, @taskai_pane_role=<native|connector-role>
```

The same identity values are placed into the tmux session environment by `new-session`
and user options are added in that same tmux command list. This leaves enough evidence to
reconcile a crash between session creation and full tagging. Names alone are never proof.

Verification uses `show-options -s`, session `show-options`, `show-options -w`, and
`show-options -p` against exact IDs. It never uses `-A` or a format-expanded inherited
value, so a window marker cannot masquerade as a pane marker.

The JSON locator and all tmux markers must agree. A mismatch is `OWNERSHIP CONFLICT`;
TaskAI refuses to attach, inject input, or kill anything.

## 8. Local now, SSH-shaped from the beginning

tmux operations sit behind a small, stateless transport boundary—not a server:

```text
probe
list runtimes
create runtime
inspect exact runtime
build attach command
resize
kill exact runtime
```

This feature implements **only local tmux**:

```json
"transport": { "kind": "local" }
```

The tagged transport type reserves a future SSH form:

```json
"transport": {
  "kind": "ssh",
  "host": "buildbox"
}
```

`host` would be an SSH-config alias; credentials and keys would remain in the user's SSH
configuration, never TaskAI JSON. Runtime identity therefore avoids a local PID and is
shaped as `{target, server_name, session_name, generation}`.

The local release may deserialize and round-trip that future variant, but every operation
on it returns `UNSUPPORTED TRANSPORT` without probing, attaching, polling, prompting, or
destroying anything. There is no remote state in the current TUI and no SSH check in the
current release gate.

Remote tmux is a separate feature specification. It must resolve safe remote argv
transport (ordinary SSH commands pass through a remote shell), target-local locking,
multiple controllers, remote workspace canonicalization, context delivery inside the
provider, hook/event storage, provider-owned child processes, unreachable-host truth,
and whether a short-lived remote TaskAI helper is required. Repository/workspace paths
will be interpreted on their recorded target, not assumed to exist locally.

Reserving the tagged transport and `target_id` now only minimizes lifecycle-layer churn
later; it is not a claim that SSH behavior has already been designed or implemented.

## 9. Lifecycle behavior

| Event | Required behavior |
|---|---|
| Create Manager/Subordinate | Persist `PREPARED`, then `LAUNCHING`; create/tag tmux runtime, verify, then mark running |
| `Enter` on running session | Attach to the existing `native` pane; never launch/resume another provider |
| `Ctrl+G` | Close that viewer client and return to navigation; the native runtime continues |
| `q` | Close TaskAI's attach clients and TUI only; worker tmux sessions continue |
| Terminal closes or TUI crashes | tmux clients disconnect; worker tmux sessions continue |
| Provider exits naturally | Keep dead pane/exit status; show `STOPPED`; do not restart automatically |
| Relaunch TaskAI | Reconcile JSON against tmux, then show and reattach existing runtimes |
| Dead pane or tmux session missing, provider ID present | Show stopped; `Enter` explicitly exact-resumes into a new tmux generation |
| tmux session missing, no provider ID | Show recovery required; do not silently create a replacement conversation |
| Machine reboot or tmux-server loss | Mark interrupted on next reconciliation; preserve provider ID/history for explicit recovery |
| `taskai session cancel` | Through the existing open-TUI conduit, cancel the current provider turn; never kill the tmux runtime |
| Destroy | Execute the verified, scoped termination transaction in section 12 |

Create is a small recoverable state machine, not a background job:

1. Under `.state.lock`, allocate the generation and deterministic exact session name,
   persist the complete locator as `PREPARED`, then release the lock. `PREPARED` means no
   launch submission has been attempted and is the only automatically retryable phase.
2. Briefly reacquire `.state.lock`, reload and verify that binding, persist `LAUNCHING`,
   and release it **before** invoking tmux. A conservative crash window is preferable to
   a duplicate provider conversation.
3. Submit `new-session` with the registry/session/generation environment plus the user
   option tagging commands as one tmux command list. The unchanged connector launcher
   starts its existing children, pipes, sockets, and optional additional tmux objects;
   TaskAI does not translate stdio/ACP endpoints into pane input.
4. Inspect the exact native pane, the connector launcher's existing ready proof, and every
   tmux object that recipe actually creates, then briefly reacquire `.state.lock` to
   commit `RUNNING` and the observed tmux object IDs.
5. On recovery, `PREPARED` plus no session may retry the same generation. `LAUNCHING`
   plus a fully marked and connector-ready runtime group finalizes to `RUNNING`.
   `LAUNCHING` plus a partial connector startup is `RECOVERY`, never a reason to start a
   duplicate server/facade. `LAUNCHING` plus no session is
   `RECOVERY` and requires an explicit user decision because tmux may have accepted a
   launch whose runtime later vanished. A name-only, unmarked, or mismatched session is
   `OWNERSHIP CONFLICT` and is never attached or killed.
6. Never allocate another generation while an unresolved `PREPARED` or `LAUNCHING`
   binding exists.

Runtime truth keys off the exact marked `native` pane, not merely the containing tmux
session. On explicit Resume from a retained dead pane, TaskAI verifies it is dead, removes
that inert tmux artifact, and creates a new generation using the preserved exact provider
session ID. It never respawns a pane in place or resumes automatically.

After a normal leave, TaskAI prints after restoring the terminal:

```text
TaskAI detached — 4 runtimes continue. Run taskai to return.
```

If no runtime is running, it prints `TaskAI closed — no runtimes are running.`

## 10. Rendering and input

`taskai-pane` continues to own the local portable PTY and vt100 grid, but its child becomes
a tmux **attach client**, not the provider CLI:

```text
TaskAI pane → portable PTY → tmux attach → existing native provider pane
```

`Pane::drop()` may continue killing its immediate child because that child is only the
viewer. Killing the attach client must be proven not to kill the tmux session or provider.

Human input continues through the attached tmux client. `Ctrl+G` remains TaskAI's only
reserved chord. Disabling tmux's prefix is essential so `Ctrl+B` and every other native
CLI key remain unchanged.

Human focus and TaskAI-initiated writer exclusion retain their current connector-specific
rules. tmux adds no new focus-duration writer lease.

Before spawning an input-capable attach client, the TUI briefly takes `.state.lock`,
reloads the session, rejects a covering Destroy tombstone, and revalidates its generation
and pane markers. Read-only inspection of an unfinished Destroy uses captured pane output,
not an input-capable attach client.

TaskAI must preserve the cursor bridge, wide-cell widths, truecolor, bracketed paste,
alternate-screen behavior, scroll regions, and resize fidelity through the extra tmux
layer. TaskAI renders only the `native` window; connector companion processes and IPC
remain hidden implementation details exactly as they are in the approved backend design.

For the first release, one interactive TaskAI TUI holds `.tui.lock`. A second TUI reports
that another interactive client is open instead of creating two writers. It does not hold
`.state.lock` for its lifetime: context, hooks, cancel, and brief registry mutations still
work while the TUI is open. Multi-viewer/input-leasing can be a later feature.

## 11. Preserve the ACP/display conduit

`backend_communication.md` remains the source of truth for communication. tmux changes
where the existing processes live, not how a turn is transported:

| Harness | Preserved mode | tmux placement |
|---|---|---|
| Codex | `shared-live`: native CLI + bundled `codex-acp` on the same app-server/thread | tmux parents the unchanged launcher group; app-server/ACP pipes and shim IPC remain connector-owned |
| OpenCode | `shared-live`: authenticated `opencode serve` + native `opencode attach` + facade | The runtime contains the same serve/attach/facade group; HTTP/SSE and credentials remain unchanged |
| Claude Code | `owned-tui`: atomic prompt into the exact displayed Claude PTY + Stop/JSONL correlation | The displayed PTY now reaches the same Claude process through tmux attach |
| Grok | `handoff-resume`: exclusive `grok agent stdio` turn followed by exact-ID native resume | tmux parents the unchanged launcher/handoff group; its stdio remains a connector pipe, never pane dispatch |

This feature must not replace those paths with generic tmux keystroke injection, disable
their automation, change their correlation IDs, or downgrade their capability labels. It
also does not activate connector paths that are not currently active; it preserves the
approved implementation and release plan as-is.

The existing owner-only `control.sock` remains the TUI's conduit endpoint while the TUI
is open. It is not replaced or promoted into a resident daemon by this feature. Therefore
`q` removes that endpoint while the tmux-hosted native runtimes and connector companions
continue. Until the TUI is reopened and reconnects the same recorded runtimes,
`taskai session prompt/cancel` calls that require the TUI endpoint must fail clearly as
`TaskAI UI unavailable — runtime still running`; they must not claim the target stopped
or silently use a different transport.

That is the honest consequence of keeping this feature persistence-only. Making the
conduit callable while no TUI exists would require a separately approved lifecycle design
for the **existing** conduit; it is not smuggled into tmux persistence as a new server or
an alternate protocol.

While the TUI is open, it continues the existing connector event/correlation flow and
periodically reconciles tmux runtime truth. `taskai context ...` may read the last JSON
snapshot with its observation time. No UI means no continuous status poll, so stale or
uncorrelated activity is `UNKNOWN`, never guessed.

The brief state-lock check and Destroy tombstone form a lifecycle fence around the
existing `Conduit` turn guard. They prevent a confirmed Destroy from racing a new
dispatch but do not alter provider-specific submission, completion, cancellation, or
recovery mechanics.

## 12. Destroy semantics

Destroy is contextual and must match the UX vision:

| Selected scope | tmux runtimes terminated | JSON removed after verification |
|---|---|---|
| Subordinate | That Subordinate only | That session record |
| Manager | Manager plus all its Subordinates | Those session records |
| Task | Every Manager and Subordinate in that Task | All session records, then Task record |

Destroying one Task therefore kills all and only the tmux sessions enumerated from that
Task's JSON membership. If other Tasks remain, their sessions and the private server
remain. If the destroyed Task contained the registry's final runtime, `exit-empty on`
lets tmux end its now-empty private server naturally; TaskAI still never uses
`kill-server`.

The current TUI always builds a whole-Task destroy from Task view even when a session is
selected. This must be corrected; the confirmation screen must state the exact scope.

The confirmation includes:

- Task/Manager/Subordinate names and stable refs;
- every affected runtime, harness, target, and current runtime/activity state;
- how many native terminals and existing connector companions will stop;
- a warning when work is active or state is unknown;
- the repository and worktree paths that will be preserved;
- the fact that tmux scrollback is ephemeral and will be discarded;
- a collision-safe token such as `destroy t1` or `destroy s3`.

Execution is ordered and recoverable:

1. Briefly acquire `.state.lock`, compute and freeze the exact refs, generations, and
   previously observed ownership markers, then release it before showing confirmation.
2. If work is `WORKING` or `UNKNOWN`, offer `Esc` or a typed **Destroy now — interrupt
   work** confirmation. There is no scheduled or wait-until-idle action in this release.
3. After the typed confirmation, reacquire `.state.lock`, reload JSON, compare the frozen
   target refs, generations, correlated turn IDs, and activity snapshot. Abort for a new
   confirmation if the blast radius changed. If a target changed from idle to an active
   or unknown turn, require the explicit **Destroy now — interrupt work** confirmation.
   Unrelated Tasks may change without invalidating it. The existing conduit holds this
   same lock across its brief final lifecycle check and dispatch-start record, so a new
   provider-specific turn cannot slip between this check and tombstoning.
4. Persist the scoped `pending_destroy` tombstone, then release `.state.lock` before any
   tmux operation. While it exists, block input attach, prompt, Resume, new
   Manager/Subordinate creation, and another Destroy beneath that scope. Read-only
   inspection and Cancel of a still-running recorded turn remain available.
5. Re-query every target and verify server name, exact tmux IDs, registry ID, Task ref,
   Session ref, and generation markers. Also snapshot every verified non-target runtime
   in the same private server.
6. Terminate verified tmux sessions children-first. Never target names by prefix or glob,
   and never type a generic `q`, `exit`, or slash command into a provider.
7. Verify each exact target session/native pane is absent and apply the connector's
   unchanged cleanup proof to its logical companion process group. Verify every
   snapshotted non-target runtime still exists with matching markers. A missing server is clean
   success only if the confirmed set contained every registered runtime in it; otherwise
   report `COLLATERAL RUNTIME LOSS` and retain the full JSON topology.
8. Briefly reacquire `.state.lock`. Only when **every** frozen target is verified absent
   remove the corresponding registry records, append the audit result, and clear the
   tombstone. On partial failure, retain the entire frozen JSON topology and record
   completed refs only inside the tombstone; never leave a Subordinate whose Manager
   record was removed.

Because there is no TaskAI daemon, Destroy does not continue after the invoking process
is killed. On the next invocation, already-absent targets recorded with their prior
verified marker snapshot may be finalized. Any remaining live target is shown as
**Unfinished Destroy** and requires a fresh typed Continue confirmation after a new
preflight; startup never resumes killing automatically. The user may instead explicitly
abandon the tombstone: TaskAI performs no process action, keeps the full topology, marks
already-absent runtimes `STOPPED`, and restores normal actions for the surviving scope.

Failure rules:

- A missing target counts as already stopped only when its persisted tombstone contains
  the exact previously verified ownership marker snapshot.
- A marker mismatch is `OWNERSHIP CONFLICT`; never kill it.
- On partial failure, keep unresolved records and the pending operation. Do not pretend a
  rollback restored runtimes already stopped. The retained absent records render as
  stopped/pending-destroy until the transaction is completed or explicitly abandoned.
- Normal Task/Manager/Subordinate destroy never calls `tmux kill-server`.
- TaskAI never kills by a stale PID and never touches the user's default tmux namespace.

`kill-session` proves that the tmux runtime disappeared; it cannot prove that an arbitrary
program which deliberately daemonized has exited. The unchanged connector launch recipes
must keep their native and companion processes within the recorded runtime group's pane
lifecycles or perform their already-approved exact cleanup. The acceptance suite proves
the complete existing connector group disappears; this feature does not redefine it.

There is no separate Stop/Hibernate lifecycle in this feature. That can be designed later
if needed; keeping Destroy as the only terminating UI operation follows the chosen rule.

## 13. Runtime truth

Process lifetime and model activity are distinct facts:

```text
Runtime:  PREPARED | LAUNCHING | RUNNING | STOPPED | RECOVERY
Activity: IDLE | WORKING | ATTENTION | COMPLETE | UNKNOWN
Display:  ATTACHED | BACKGROUND
```

tmux determines runtime truth. Provider events determine activity. The TUI must not call
a process `LIVE` merely because stale JSON says so, and it must not call an agent idle
merely because tmux is running.

Examples:

```text
● RUNNING · WORKING · BACKGROUND
● RUNNING · IDLE · ATTACHED
○ STOPPED · exact resume available
```

Color never carries meaning alone. Running uses the existing live/ion color, Attention
uses amber, stopped/background is dim, and red remains reserved for destructive actions.

`last_observed` is a cache, never runtime authority. `IDLE` requires a provider signal;
otherwise activity is `UNKNOWN`. `ATTACHED/BACKGROUND` is process-local display state and
is not serialized.

While the TUI is open it polls the private local server in one bounded background worker
and renders the last complete snapshot. The worker dies with the TUI and is not resident
infrastructure. It does not run one subprocess per row on every frame.

## 14. User-visible key changes

Home:

```text
Enter Open   n New task   d Destroy task   h Harnesses   ? Help   q Leave
```

Task navigation:

```text
Enter Attach/Resume   a Subordinate   m Manager   d Destroy   T Tasks   q Leave
```

Attached native CLI:

```text
Ctrl+G Return to TaskAI
```

Help must say:

```text
q       Leave TaskAI — agents continue in tmux
d       Destroy selected scope — stops its verified tmux runtimes
Ctrl+G  Return from the native CLI to TaskAI navigation
```

The Harness/diagnostic view shows the runtime backend separately from provider harnesses:

```text
Runtime  tmux 3.5a · READY · private namespace taskai-a8f14c2e
```

## 15. Migration

Treat the current unversioned state as schema version 1.

On first schema-2 open:

1. If the pre-migration TUI's existing `control.sock` has a live listener, refuse
   migration and ask the user to close that TUI; the old binary does not honor the new
   lock. This is concurrency protection, not removal of the conduit. A stale path may be
   removed only after `lstat` proves it is an owner-owned socket, not a symlink or
   arbitrary file; the reopened TUI recreates the same endpoint.
2. Take `.state.lock` and create owner-only `state.json.v1.bak` once. Never overwrite that
   backup on a later schema-2 open.
3. Preserve every Task/session ref, topology edge, workspace, harness, provider mapping,
   and audit record.
4. Add the stable registry identity and empty runtime bindings.
5. Convert legacy volatile `LIVE`, `WORKING`, and `ATTENTION` process claims to queued or
   unknown because no pre-feature direct PTY can have survived its owning TUI.
6. Do not start or resume a provider during migration.
7. On explicit `Enter`, create the first tmux generation and exact-resume when a proven
   provider session ID exists.
8. Never touch repositories, worktrees, or provider-native history.

New code must read schema 1 and 2 during rollout. Running an older binary against a
schema-2 registry is unsupported because it could discard runtime fields and launch a
duplicate provider process; the release notes must say so plainly.

## 16. Implementation milestones

No milestone adds a new central daemon, service mode, control socket, or persistence
controller. Existing connector bridge processes may be reparented into tmux, and the
TUI-owned conduit endpoint remains in scope exactly as defined by
`backend_communication.md`.

### TM0 — Contract and failing lifecycle tests

- Approve this specification.
- Add a deterministic, non-billing fake CLI.
- First failing proof: work continues after `q` and TUI `SIGKILL`.
- Amend only the conflicting tmux/lifecycle statements in backend/build/test/README.

### TM1 — Registry binding and locks

- Add schema/version migration, registry ID, runtime phase/generation, one scoped Destroy
  tombstone, and correlated turn state to `taskai-core`.
- Add the two explicit lifecycle lock classes and reload-before-mutate semantics around
  atomic JSON writes.
- Keep provider identity separate from runtime identity.

### TM2 — Small tmux module

- Add a focused tmux module to the existing pane/runtime code; do not add a service crate.
- Implement probe, create, tag, inspect, attach, resize, kill, and enumerate using
  argv-based local commands.
- Pass provider launch recipes as tmux's multi-argument direct-exec form; do not compose a
  shell-command string.
- Use a TaskAI-private namespace and verify all markers.
- Add the transport enum now; implement `local`, reserve `ssh`.

### TM3 — Durable launch and native rendering

- Place each native CLI and its existing connector-required companion processes in one
  tmux runtime without changing the connector recipe or capability mode.
- Make the rendered portable PTY own only `tmux attach`.
- Remove the unconditional startup downgrade; reconcile against tmux instead.
- Preserve all current rendering/cursor/size behavior through nested tmux.

### TM4 — Conduit parity and reattachment

- Keep the existing TUI-owned `control.sock` API and every ACP/provider-specific transport.
- Rebuild the TUI's pane/session/harness bindings from verified JSON + tmux state after
  reopen, then reconnect through each connector's existing mechanics.
- Add lifecycle state/tombstone fencing around the existing conduit without changing its
  correlation, completion, cancel, hook, or recovery behavior.
- While no TUI endpoint exists, make prompt/cancel refuse truthfully as UI unavailable;
  never reroute through generic tmux keys.
- Re-prove `shared-live`, `owned-tui`, and `handoff-resume` parity after leave/reopen.

### TM5 — Scoped destroy and recovery

- Make `d` context-sensitive for Subordinate, Manager, and Task.
- Persist intent, verify ownership, kill exact sessions, verify absence, then remove JSON.
- Reconcile partial/interrupted operations without auto-killing conflicts.

### TM6 — Local release gate and SSH seam

- Prove that local runtime identity contains no local PID/socket assumption and that the
  reserved SSH locator round-trips but every attempted operation returns
  `UNSUPPORTED TRANSPORT` without side effects.
- Document the separate SSH follow-up blockers: target-local locking, context delivery,
  event handling, workspace paths, and remote command safety.
- Run the full free fake-CLI suite locally.
- Run real-provider billed proofs only under the existing approval protocol.

## 17. Free acceptance tests

Lifecycle and isolation tests use real tmux plus deterministic fake CLIs. Probe, parser,
and error-path tests may use controlled `PATH` shims. No test calls a model.

| ID | Scenario | Required assertion |
|---|---|---|
| TM-A01 | Create fake Manager | One tagged private tmux session; correct ref, cwd, env, generation |
| TM-A02 | TaskAI inside user's outer tmux | Inner private namespace works; outer window/session unchanged |
| TM-A03 | `Ctrl+G` | Navigation returns; provider PID and heartbeat continue |
| TM-A04 | `q` during delayed work | Recorded TUI PID/descendants exit after bounded quiescence; provider survives and finishes |
| TM-A05 | TUI `SIGHUP`/`SIGTERM`/`SIGKILL`/PTY close | Provider and existing companions survive; poll until viewer leaves `tmux list-clients` |
| TM-A06 | Relaunch | Same runtime group reattaches and connector bindings rebuild; no second launch/resume |
| TM-A07 | Terminal fidelity | Parsed cells, cursor, wide glyphs, colors, paste, and input match after reattach |
| TM-A08 | Exact provider ID | Provider mapping remains independent of tmux binding |
| TM-A09 | Missing runtime with ID | One explicit exact resume into a new tmux generation |
| TM-A10 | Missing runtime without ID | Recovery shown; no silent replacement session |
| TM-A11 | Natural provider exit | Dead pane and exit status retained; no automatic restart |
| TM-A12 | Conduit boundary after `q` | Prompt/cancel says UI unavailable without mutation; after reopen the same existing conduit/mode works |
| TM-A13 | Existing conduit parity | Prompt secrecy, correlation, completion, cancel, and uncertain-dispatch recovery remain unchanged |
| TM-A14 | Connector runtime group | Native CLI and approved companions survive leave and all exit on scoped Destroy |
| TM-A15 | Subordinate destroy | Only that runtime disappears |
| TM-A16 | Manager destroy | Manager and its children disappear; peers survive |
| TM-A17 | Task destroy | All and only that Task's runtimes disappear |
| TM-A18 | Destroy failure/interrupt | Every child/parent failpoint keeps valid full topology; completed refs stay in tombstone; next run never auto-kills |
| TM-A19 | Ownership collision | Precreated same-name server with other markers has byte-identical options/sessions/panes after every refused operation |
| TM-A20 | User tmux isolation | Default-server sessions are unchanged before/after every operation |
| TM-A21 | Repository preservation | Repo, worktrees, `.git`, and fake provider history hashes are unchanged |
| TM-A22 | Missing/old tmux | Stored tasks view; launch refused; no false running claim |
| TM-A23 | Concurrent registry processes | Separate processes retain unique refs/revisions and no JSON update is lost |
| TM-A24 | Second interactive TUI | Refused clearly; it cannot become a second input writer |
| TM-A25 | Runtime state staleness | Cached observation is labeled; explicit refresh reports stopped/unknown truthfully |
| TM-A26 | Nested sizes | 80×20 resize warning and 110×30, 209×51, 364×58 frames stay correct |
| TM-A27 | Hostile display names/paths | No shell injection or wrong-target tmux operation |
| TM-A28 | Schema-1 migration | Topology/IDs preserved; one-time backup never overwritten; no runtime starts |
| TM-A29 | Locator serialization | Local locator round-trips without PID/socket assumptions; SSH variant deserializes as reserved |
| TM-A30 | Reserved SSH target | Every operation returns `UNSUPPORTED TRANSPORT` without probing or mutating JSON |
| TM-A31 | No new persistence service | After leave no TUI `control.sock` or resident central TaskAI process; approved connector endpoints may remain |
| TM-A32 | Hook event identity | Duplicate event applies once; wrong/old generation and operation IDs are rejected |
| TM-A33 | Writer crash | Death after submission leaves `UNCERTAIN`; resend blocked until native reconciliation |
| TM-A34 | Cancel during wait | Existing connector cancel reaches the correlated turn with unchanged Conduit semantics |
| TM-A35 | Create crash phases | Kill before/after submission; only `PREPARED` retries, `LAUNCHING`+missing requires recovery, marked runtime finalizes once |
| TM-A36 | Lock separation | Second TUI refused while context, hook, cancel, and brief state writes still work |
| TM-A37 | Outer tmux environment | Different inherited `TMUX_TMPDIR` values resolve the same private server safely |
| TM-A38 | TUI socket migration guard | Live direct/long-path endpoint blocks before writes; only stale owner socket is removed; symlink/file/foreign/incompatible listener untouched |
| TM-A39 | Prompt/Destroy race | Either send commits before re-confirmed interrupt Destroy or tombstone blocks send; never both unchecked |
| TM-A40 | Launch environment | Native and companion processes receive fresh required PATH/auth values; secret values stay out of argv/state/logs |
| TM-A41 | Marker scope | Missing/inherited/wrong server, session, window, or pane marker is rejected at that exact scope |
| TM-A42 | Server loss during scoped Destroy | Unrelated runtime loss is reported as collateral; JSON is not finalized as clean success |

Use unique temporary namespaces. Snapshot the user's default tmux server before and after;
it must be unchanged. Use events or `tmux wait-for`, not fixed sleeps. The release gate must
exercise the oldest supported and current tmux versions.

Real Claude/Codex/OpenCode/Grok proofs remain billed T3 tests and require explicit approval.

## 18. Definition of done

The feature is complete only when:

- leaving or crashing TaskAI cannot terminate a running native CLI or its existing
  connector-required runtime group;
- after bounded quiescence, the departed TUI has no resident central TaskAI process or
  TUI `control.sock`; approved connector endpoints may remain inside their tmux runtime;
- relaunch attaches the same live runtime rather than creating a duplicate;
- context remains readable without a TUI; prompt/cancel that require the existing TUI
  conduit refuse as UI unavailable until reopen, without mutating the provider;
- after reopen, Codex shared-live, OpenCode shared-live, Claude owned-tui, and Grok
  handoff-resume retain their approved protocol and capability behavior;
- Subordinate, Manager, and Task destroy stop exactly their documented runtimes and only
  remove JSON after verified termination;
- default/user tmux sessions, repos, worktrees, and provider history remain unchanged;
- missing tmux, missing runtime, stale state, ownership conflict, and a reserved SSH
  target are reported truthfully;
- terminal rendering remains visually and behaviorally identical through tmux;
- only the conflicting tmux/lifecycle portions of README, backend, build plan, UX wording,
  and test plan are updated; ACP/conduit decisions remain intact;
- the complete non-billed test matrix is green before requesting live-provider approval.

## 19. Proposed defaults to confirm

These choices keep the design small and safe:

1. tmux persistence is required; there is no silent direct-PTY fallback.
2. One private tmux server name per TaskAI registry and target.
3. One tmux session per Manager/Subordinate, containing its native window and parenting
   the existing connector-required process group without changing its IPC.
4. `q` is displayed as `Leave`; Destroy is the only terminating UI action.
5. Confirmed Destroy runs immediately; the warning and `Esc` are how a user chooses to wait.
6. One interactive TUI per registry for the initial release.
7. Dead panes are retained until explicit resume or Destroy.
8. Only local transport ships here; the tagged JSON locator reserves SSH, whose behavior
   requires a separate feature specification.
