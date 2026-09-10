# Simplified tmux persistence: test plan

**Status:** Passed on 2026-09-03; implementation evidence is recorded in
[`tests/evidence/2026-09-03-tmux-persistence.md`](tests/evidence/2026-09-03-tmux-persistence.md).
**Cost rule:** All automated and manual tests in this plan use a deterministic fake or
ordinary shell process. They must not submit a turn to Claude, Codex, OpenCode, Grok, or
any other model provider. Live-provider proofs require separate approval.

## 1. Scope being tested

TaskAI replaces direct ownership of each Manager or Subordinate process with one durable
tmux session behind the scenes:

```text
TaskAI viewer -> local or SSH tmux attachment -> one agent runtime
```

The Task remains a tracker grouping; it does not get a tmux session of its own. The
existing JSON tracker maps every Manager and Subordinate separately to:

- its TaskAI session ref;
- its repository/workspace path;
- a local or SSH connection target;
- its tmux server and tmux session names; and
- its provider session ID, when one exists, independently of the tmux identity.

The required behavior is:

- `q` closes TaskAI's viewer but leaves every mapped tmux session running;
- reopening TaskAI reattaches to the same tmux session and process;
- destroying a Subordinate stops only that Subordinate's mapped session;
- destroying a Manager stops that Manager and its mapped Subordinates;
- destroying a Task stops all and only the sessions mapped to that Task;
- repositories, worktrees, branches, files, and provider history are not destroyed;
- the existing Claude, Codex, OpenCode, Grok, ACP, hook, and conduit paths retain their
  current behavior; and
- no TaskAI daemon, resident server, queue, or database is added.

This plan intentionally does **not** require hostile ownership-collision protection or
multiple simultaneous TaskAI interfaces. It does require ordinary missing-session and
unreachable-host reconciliation, without automatic destructive recovery.

## 2. Repository and connection semantics

Repository location and connection target are separate values.

| Case | Repository value | Connection entry | Tracker/UI display |
|---|---|---|---|
| Local | Local absolute repository path | Empty | `<local>` |
| Remote | Absolute path, `~`, or `~/path` as seen on the remote host | `ssh cruise2` | `ssh cruise2` |

The empty value must mean local; users should not have to type a local connection command.
For an SSH target, the tracker stores a structured target (for example,
`{"kind":"ssh","host":"cruise2"}`), derives process arguments, and renders the readable
command `ssh cruise2`. It must not store
passwords, private-key contents, or expanded secrets. The runtime locator is stored on
each Manager/Subordinate record rather than inferred from its display name.

The remote repository need not exist locally. TaskAI expands a remote `~` on the target,
validates the directory there, and stores the resolved absolute path. Repository display
must continue to show the repository path;
connection display must show `<local>` or the SSH command so the user can tell where the
runtime lives.

## 3. Test harness and isolation

All tests use:

- a temporary `TASKAI_HOME`;
- a unique TaskAI tmux server/socket name per test run;
- TaskAI refs rather than task, manager, or role names for tmux session names;
- a deterministic fake CLI that prints its PID and working directory, accepts input,
  preserves an unsubmitted draft in the terminal, and writes lifecycle events to a
  temporary log;
- bounded polling or tmux synchronization, never timing assumptions alone; and
- cleanup limited to the exact temporary local/remote tmux server and directories created
  by the test.

Provider executables in the test `PATH` should be non-billing stubs that fail loudly if a
test accidentally invokes a real provider. Before and after each integration run, capture
the user's default tmux session list; it must be unchanged.

Remote integration tests use a unique directory and tmux namespace on `cruise2`. They must
not use or kill its default tmux server.

## 4. Automated acceptance tests

### AT-01 — Local target parsing and display

Create a Task with an empty connection value and a valid local repository.

Pass conditions:

- the tracker records a local target;
- the UI renders the connection as `<local>`; and
- the repository path remains independently visible.

### AT-02 — SSH target parsing and display

Create a Task with repository `~/example-repo` and connection `ssh cruise2`.

Pass conditions:

- the tracker records an SSH target and parsed SSH arguments;
- the UI renders `ssh cruise2` rather than `<local>`;
- the tracker stores the remote host's resolved absolute repository path; and
- no credential material is serialized.

### AT-03 — One tmux session per agent

Create one fake Manager and two fake Subordinates.

Pass conditions:

- exactly three TaskAI tmux sessions exist;
- every record has its own locator;
- each fake process reports the intended working directory; and
- no tmux session exists for the Task grouping itself.

### AT-04 — Leave detaches without stopping work

Start a fake CLI that increments a counter, record its PID, and leave with `q`.

Pass conditions:

- TaskAI and its attach client exit;
- the tmux session and original fake PID remain alive; and
- the counter continues advancing after TaskAI exits.

### AT-05 — Reopen reattaches instead of relaunching

Type a unique but unsubmitted draft into the fake CLI, leave TaskAI, then reopen it and
select the same agent.

Pass conditions:

- the exact original PID remains;
- the draft and terminal history are still visible;
- no second fake CLI process is launched; and
- inline/full-screen focus and `Ctrl+G` navigation still work.

### AT-06 — TaskAI process loss

Terminate the TaskAI viewer unexpectedly after the fake CLI is running.

Pass conditions:

- the fake CLI and tmux session survive;
- a new TaskAI process reconciles the recorded locator; and
- reattachment reaches the same PID without automatic replacement.

### AT-07 — Missing-session reconciliation

Remove a test tmux session externally while its tracker record remains.

Pass conditions:

- reopening TaskAI reports that runtime as stopped/missing;
- it does not claim `LIVE` merely because the JSON record exists; and
- it does not automatically launch or destroy another process.

### AT-08 — Local scoped destruction

Create two Tasks with a Manager and Subordinates, then exercise Subordinate, Manager, and
Task destruction.

Pass conditions:

- Subordinate destroy removes only its session;
- Manager destroy removes that Manager and its Subordinates but preserves peer Managers;
- Task destroy removes every mapped session for that Task and preserves the other Task;
- tracker records are removed only for the selected scope; and
- repository sentinel files remain byte-identical.

### AT-09 — SSH command construction without a remote host

Use an injected command runner to capture remote operations.

Pass conditions:

- create, inspect, attach, and destroy are routed through the recorded SSH arguments;
- the recorded remote repository becomes the runtime working directory;
- local targets never invoke SSH; and
- a failed SSH command produces a truthful unreachable/error state rather than `LIVE`.

### AT-10 — Remote tmux lifecycle on `cruise2`

Run the fake CLI in a unique private tmux namespace on `cruise2`.

Pass conditions:

- TaskAI creates exactly one remote tmux session for the agent;
- `q` disconnects only the local SSH/tmux viewer;
- the remote tmux session, PID, and continuing work survive;
- reopening TaskAI reattaches to the same remote PID and draft;
- Task destroy kills the exact recorded remote sessions; and
- the remote repository sentinel remains intact after Task destroy.

This test may be an explicitly selected integration test rather than part of the default
offline suite, but it is required for this feature's final acceptance.

### AT-11 — Remote failure reconciliation

Simulate an unreachable `cruise2` connection and a missing remote tmux session.

Pass conditions:

- unreachable and missing are distinguishable from running;
- TaskAI retains the tracker mapping for later retry;
- TaskAI does not silently create a replacement session; and
- no local tmux session is created as a fallback.

### AT-12 — Existing connector regression

Run all existing connector, context, core, pane, TUI, and CLI tests.

Pass conditions:

- existing connector mode/capability tests remain green;
- provider identity remains distinct from tmux identity;
- prompt/cancel/hook semantics are not rerouted through generic tmux keystrokes; and
- no existing ACP or provider implementation is deleted or disabled.

### AT-13 — Terminal fidelity through tmux

Drive the fake CLI through both local and SSH attachments using alternate screen, cursor
movement, colors, Unicode/wide glyphs, resize, paste, and a visible input cursor.

Pass conditions:

- rendering matches the direct fake-CLI fixture at supported terminal sizes;
- the cursor remains visible and stable while the CLI is focused;
- input is accepted in both inline and full-screen focus; and
- repeated attach/detach cycles do not accumulate viewer processes.

### AT-14 — User tmux isolation

Run the suite while an unrelated session exists on the user's default local tmux server
and, for the remote test, on `cruise2`'s default tmux server.

Pass conditions:

- those sessions are unchanged before and after leave, reconciliation, and destruction;
- TaskAI always addresses its explicitly recorded private server; and
- normal Task destroy never invokes an unscoped `kill-server`.

### AT-15 — Nested-tmux shortcuts and modal ownership

Run the release binary inside an ordinary user tmux pane. From Task navigation, enter
full-screen CLI focus with `Ctrl+F`, toggle to inline CLI focus with `Ctrl+F`, and return
to navigation with `Ctrl+G`. Open each form from navigation.

Pass conditions:

- the outer tmux does not consume either reserved chord;
- both focused layouts accept native CLI input;
- a form replaces the background keybar with only its active controls;
- modified letters such as `Ctrl+F` and `Ctrl+G` never leak into form fields; and
- `Esc` cancels the form to its originating navigation screen.

## 5. Manual live proof in `acp-build:4`

The live proof runs in tmux window/tab `acp-build:4` and uses the release build plus an
isolated tracker. It must display an unmistakable `FAKE CLI — NO MODEL CALLS` banner.

1. Record the existing contents of `acp-build:4` and the default local tmux session list.
2. Build TaskAI and start it in that tab with a temporary `TASKAI_HOME` and fake-provider
   `PATH`.
3. Create a local Task. Leave the connection entry empty and verify `<local>` is shown.
4. Add one Manager and at least one Subordinate. Record their tmux locators and fake PIDs.
5. Focus one CLI, type a unique unsubmitted draft, toggle inline/full-screen, and return to
   navigation with `Ctrl+G`.
6. Press `q`. Outside TaskAI, verify both fake PIDs and their continuing counters remain.
7. Relaunch TaskAI using the same tracker, reattach, and verify the same PIDs, history, and
   draft.
8. Destroy the local Task and verify its tmux sessions are gone while its repository
   sentinel remains.
9. Create a remote Task with a remote repository path and connection `ssh cruise2`; verify
   the UI shows `ssh cruise2`.
10. Add a fake remote Manager and Subordinate. Confirm their PIDs and sessions directly on
    `cruise2` in TaskAI's unique private tmux namespace.
11. Type a unique draft, press `q`, and prove the remote PIDs keep running after the local
    SSH attachment closes.
12. Relaunch TaskAI, reattach to the same remote PID and draft, then destroy the remote
    Task.
13. Verify only the mapped remote sessions stopped and the remote repository sentinel was
    preserved. Remove only the uniquely named temporary test artifacts after recording
    that proof.
14. Confirm the default local and remote tmux session lists match their initial snapshots.

Capture the commands, relevant PID/session evidence, and before/after checks under
`tests/evidence/`. Screenshots may supplement this record but do not replace process and
tmux assertions.

## 6. `cruise2` prerequisites

Before implementation is declared testable, all of these must succeed without an
interactive prompt:

```sh
ssh -o BatchMode=yes cruise2 true
ssh -o BatchMode=yes cruise2 tmux -V
ssh -o BatchMode=yes cruise2 'command -v sh'
```

The test also requires:

- the `cruise2` alias to resolve through the user's existing SSH configuration;
- permission to create a uniquely named directory below the remote temporary directory;
- permission to start a private, explicitly named tmux server;
- a compatible terminal and UTF-8 locale; and
- no requirement for TaskAI to copy SSH credentials or install a resident remote helper.

If a prerequisite fails, report the exact failing prerequisite. Do not fall back to a
local runtime and do not claim remote acceptance passed.

## 7. Execution order and release gate

The required order is:

1. Add these automated tests and confirm new behavior tests fail for the expected reason.
2. Implement the local tracker mapping, tmux runtime, detach, reattach, and destroy path.
3. Pass AT-01 through AT-08 and AT-12 through AT-14 locally.
4. Implement SSH command/target handling without altering provider protocols.
5. Pass AT-09 and AT-11 with the injected runner.
6. Check `cruise2` prerequisites and pass AT-10.
7. Complete the manual journey in `acp-build:4`.
8. Run the repository-wide release checks:

```sh
cargo fmt --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace --locked
cargo build --release --locked
```

The feature is accepted only when local and `cruise2` runtimes survive `q`, reopen to the
same process, stop on the correct scoped Destroy, preserve repositories, leave default
tmux servers untouched, and all regression/release checks pass. No live provider turn is
part of this gate.

## 8. Final result

All acceptance areas passed with non-billing processes:

| Area | Result | Primary evidence |
|---|---|---|
| AT-01–02 | Pass | Local/SSH parsing, display, serialization, and visible tab-4 creation |
| AT-03 | Pass | One Manager plus two Subordinates map to exactly three tmux sessions; the Task maps to none |
| AT-04–05 | Pass | `q`, advancing heartbeats, one launch, and same-PID/draft reattachment locally and on `cruise2` |
| AT-06 | Pass | Real TaskAI binary `SIGKILL`, zero orphan attach clients, same-PID/draft recovery |
| AT-07 | Pass | Missing-runtime startup reconciliation retains the locator and never claims `LIVE` |
| AT-08 | Pass | Live Subordinate, Manager-family, and cross-Task scoped-destroy isolation with preserved sentinels |
| AT-09 | Pass | Injected runner proves complete SSH lifecycle routing, quoting, remote cwd, and no local SSH path |
| AT-10–11 | Pass | Real `cruise2` lifecycle plus deterministic unreachable-vs-missing/no-fallback coverage |
| AT-12 | Pass | Complete workspace connector/context/core/pane/TUI/CLI regression suite |
| AT-13 | Pass | Local and real-SSH alternate screen, RGB, bold, wide cells, cursor, paste, resize, and viewer cleanup |
| AT-14 | Pass | Default local and remote tmux servers unchanged; private exact targeting; no `kill-server` |

The final workspace run passed 109 non-billing tests. Four explicitly billed provider-turn
proofs remained ignored under the separate approval protocol.
