# TaskAI

One terminal. Many tasks. Teams of native CLI agents working in parallel.

TaskAI is the command center around agent CLIs. It owns the task board, hierarchy,
status, creation flows, and navigation chrome; the entire working pane belongs to the
selected session's actual native CLI — Claude Code, Codex, OpenCode, or Grok CLI.
The CLI can be focused either beside the session rail or full-screen. While it has
focus, TaskAI reserves `Ctrl+F` to change that layout and `Ctrl+G` to return to
navigation; every other keystroke passes through to the CLI unchanged.

The project is TaskAI; the repository is named `acp-gateway` after the agent-conduit work
it grew out of.

## Install

```sh
cargo build --release
install -m 0755 target/release/taskai ~/.local/bin/taskai   # no sudo needed
```

Use any directory on your `PATH`. `/usr/local/bin` is root-owned on macOS, so installing
there needs `sudo install -m 0755 target/release/taskai /usr/local/bin/taskai`. To run the
build without installing it at all, use `./target/release/taskai`.

A clean machine needs this binary, `tmux`, and the provider CLIs you use,
user-installed: `claude` (≥ 2.0), `codex` (≥ 0.100), `opencode` (≥ 1.18), `grok`
(≥ 1.0). TaskAI never modifies or upgrades them; Settings shows an actionable status
and reason for missing or outdated CLIs. Remote Tasks also require `ssh` locally and `tmux` plus the chosen
provider CLI on the remote host.

## Run

```sh
taskai
```

Boot probes the harnesses, then the portfolio opens. Keys while navigation has focus:

| Key | Does |
|---|---|
| `Enter` | Open a task directly into its CLI / type in the selected session from navigation / confirm a form |
| `n` | New task (saved server → repository → task name) |
| `m` | Add Manager · `a` Add Subordinate under the selected Manager |
| `t` | Open/reuse the task's ordinary Terminal shell |
| `/` | Find tasks by name, repository, ref, or connection |
| `Esc` | Return from task navigation to the task list; cancel a form/search |
| `d` / `D` | Destroy selected agent / whole task (exact scope and typed ref confirmation) |
| `s` | Settings → Servers (`h` / `,` remain legacy aliases from Home/task navigation) |
| `G` / `?` / `q` | Home / shortcut Help / Leave |
| `F1` | Open/reuse the optional setup helper from Home, task navigation, or Settings |
| `Ctrl+F` | Enter full-screen CLI focus / toggle full-screen and inline focus |
| `Ctrl+G` | Return either focused CLI view to Task navigation |

Keyboard focus has one steady header accent: `▸ NAVIGATION` in the sidebar or
`▸ CLI INPUT` above the console. The selected agent stays softly marked while
typing, but only navigation shows its bright row/arrow. Hover and scrolling do
not move focus. The console's native colors remain untouched; its cursor appears
only with CLI focus and only when the CLI itself requests it. Switching focus
does not animate or resize the console.

In navigation, click a task or team entry to open it, click the working pane to
type, and use the wheel or arrow keys to move through lists. The selected row and
shortcuts remain visible in long lists. Returning Home remembers the selected
agent or Terminal for each task during that run. Wheel input over the CLI scrolls
only its content: the header, sidebar, and shortcuts stay fixed. Mouse-aware CLIs
receive their native events; other CLIs use their private tmux history. While the
CLI has focus, `Esc` or `End` leaves that history and returns to live input.
Scrolling the CLI from navigation does not change the selected agent or steal
keyboard focus—press `Enter` first to focus it. `Ctrl+G` returns to navigation.

CLI output wakes rendering immediately instead of waiting for the background UI
tick. Rapid updates are combined into a 16 ms redraw cadence without delaying
input forwarding; adjacent cells share colour codes to keep terminal traffic
small. This adds no service or second history store. See the measured results
and shared-machine limitations in the [scroll performance evidence](tests/evidence/2026-09-07-smooth-scroll.md).

Opening a CLI and checking remote providers run in background workers, not on
the input/render loop. Typing during launch is buffered for that session; pasted
text waits for the CLI's first output so its bracketed-paste mode can be honored.
`Ctrl+G` lets you navigate while connecting. A failed launch retains its mapping
and buffered input for retry. Destroy waits until launches in its exact scope
finish, preventing untracked processes. `q` never waits for a fresh SSH check.

Live task cards show recorded runtime status, not synthetic activity graphs.
The native CLI view stays native; TaskAI does not advertise an ACP visualization
when none is available. This does not remove the underlying ACP integrations.

While a form or confirmation popup is open, it owns input: the bottom bar switches to
only that popup's active controls, ordinary letters edit the selected field, and `Esc`
cancels back to navigation. Control/Option/Command-modified letters are never inserted
into form text.

Manager and Subordinate Harness fields show all four providers, including unavailable
ones. Click the **Harness** label or press Tab to focus it; use left/right keys or
click its chevrons to cycle choices. The selected provider shows its version or the
reason it cannot launch. `Enter` cannot create an unavailable provider. `F5` rechecks
CLIs without losing the form's name, role, repository choices, or selected harness.
Local and remote version checks are bounded and run outside the input loop.

SSH provider discovery and actual launches use the remote account's interactive-login
environment, so user-installed executables are found even when an existing tmux server
has an older PATH. Shell startup banners are suppressed; the provider still renders
natively. Compatibility checks remain enforced: an older Grok or unsupported remote
OpenCode setup is explained in the selector, not silently hidden or automatically
upgraded.

Every Manager and Subordinate runs in its own private tmux session; a Task is only their
tracker grouping. `q` closes TaskAI's disposable viewers and leaves those runtimes
working. Relaunching TaskAI inspects the saved tmux locators and reattaches the same live
processes instead of launching duplicates. If a provider itself exits, its tmux session
and attach client exit too, preventing a retained dead pane from appearing `LIVE`.

Destroy stops only the tmux sessions in the confirmed Subordinate, Manager, or Task
scope, then removes those TaskAI records. Repositories, worktrees, branches, files, and
provider-native history are always preserved.

## Local and SSH tasks

### Settings → Servers

Press `s` from Home or task navigation (`h` and `,` remain legacy aliases). **This computer** is always
available. Add an SSH server once with a display name, SSH-config alias (for
example `cruise4b` or `ssh cruise4b`), and an optional default repository folder.
Select that saved server when creating a task—no repeated SSH entry.

Each server has its own enabled harnesses, executable overrides, and default
model names. Blank executable/model fields keep native defaults. The provider
table has one **Status** beside its version: **Ready**, **Sign in needed**,
**Couldn't verify**, **Disabled**, **Update needed**, or **Unsupported**. Select a
provider for the reason and next step.

Adding/saving a server, saving harness defaults, pressing `F5`, or returning from
Configure CLI/setup Terminal starts background readiness checks. These check
SSH/tmux, versions, and safe native saved-login status where supported: Claude's
`auth status --json` and Codex's `login status`. OpenCode/Grok login remains
**Couldn't verify**; TaskAI does not inspect their credential stores. No model
prompt is sent. **Ready means compatible CLI plus native-reported saved sign-in,
not verified model access, quota, entitlement, or a successful model turn.** Raw
auth output is neither displayed nor persisted. Unknown login does not prevent
opening an otherwise compatible native CLI to inspect or complete setup.

Select a harness and press `Enter` to edit its defaults (`e` edits the server), or `c` to open the
native CLI for configuration/login. `t` opens an ordinary setup terminal on that
server for installation or troubleshooting; TaskAI never installs, upgrades, or
authenticates automatically. Setup consoles accept typing immediately, use
`Ctrl+G` to return to Settings, and persist in tmux across leaving TaskAI. Use
the shell's `exit` or the CLI's own quit command to close them. They have saved
locators but are not agents, do not affect team counts, and are not destroyed
with a task.

Settings apply to **new** agents. Existing agents retain their saved launch
defaults, provider session IDs, and exact tmux locators. A server's SSH target
cannot be changed after creation; add another server instead. Archiving hides
it from new-task choices without deleting existing tasks or setup terminals.
Existing state files migrate automatically to This computer plus saved profiles
for their SSH aliases. No new daemon, credential store, or configuration service
is introduced. See [feature-server-settings.md](feature-server-settings.md).

### Optional setup helper

Press `F1` from Home, task navigation, or Settings; `?` opens shortcut Help. Choose a
local provider to create an ordinary `taskai-helper` task with one local Manager;
later opens reuse its saved identity and live tmux process. It appears in normal
task/team counts, and normal task destruction removes it. A similarly named user
task is never adopted. No prompt is automatically filled or submitted: complete
any native trust/login step, then type your question. Helper prompts use your
normal provider account and billing.

Its private `$TASKAI_HOME/helper` workspace (default `~/.taskai/helper`) contains
the bundled versioned setup guide in README.md and AGENTS.md, plus managed
`gauntlet-loop` and `taskai-staff-objective` skills. Give it an objective and it
asks probing questions, writes a Task / How / Bar brief, and recommends which
Ready harnesses to use as Manager and Subordinates. It does not create them:
queue the ordinary forms with `Ctrl+G` then `F8`. No new daemon or credential
store is added. Existing drafts are not replaced; saving/creating still
requires you to submit the ordinary form. A request is not permission to install,
change configuration, authenticate, destroy work, or make model calls.

`taskai helper guide` and `taskai helper status` are read-only. See
[feature-taskai-helper.md](feature-taskai-helper.md) for the request commands,
safety boundaries, and live test evidence.

### Task Terminal

Each task has a **Terminal** entry under **Utilities**, separate from its team.
From navigation, press `t`, or select Terminal and press `Enter`. It opens one
ordinary login shell in the task repository, locally or on its configured SSH
host. Use it for Git, upgrades, and troubleshooting; TaskAI does not run upgrades
for you or grant extra privileges. Your normal shell startup files still apply.

Type immediately; `Ctrl+G` returns to navigation and `Ctrl+F` toggles fullscreen.
Reopening Terminal reuses its running process and draft. `exit` closes the shell;
opening it again starts a new shell using the same saved location. Leaving TaskAI
keeps it running. Destroying a manager leaves it alone; destroying the task closes
it along with that task's agents. A terminal is not counted as a team member and
has no provider session ID or agent context injection. It is created lazily, so
tasks that never open Terminal create no utility runtime.

### Agent focus and repository selection

Opening a task or creating a Manager/Subordinate focuses its native CLI immediately,
with the sidebar still visible. Type directly; the cursor follows the CLI's own
position and visibility. `Ctrl+G` returns to navigation for selecting sessions or
adding team members; `Enter` returns to the selected CLI inline. `Ctrl+F` toggles
fullscreen. Empty tasks and failed launches remain in navigation.

Server, Repository, and Task name are separate fields in the New Task form.
Server defaults to **This computer**. Press `Enter` on Server to choose a saved
profile; left/right cycles profiles. Changing servers replaces the repository
with that server's default folder, avoiding accidental reuse of another host's
path. Choosing the same server or cancelling preserves your draft.

`Tab`/`Shift+Tab` only move fields. Paste a repository path or press `F2` to browse
the selected server. With no default folder, browsing starts at its home directory.
Choose with `Space` (or enter folders with `Enter`), then edit the suggested task
name and press `Enter` to create. Browsing is read-only and never starts an agent.
Remote validation runs in the background; `Esc` cancels it and preserves the draft.

Click any text field to position the caret, or click its `[Clear]` action.
`Ctrl+A` selects the whole value; typing/pasting replaces the selection. `Ctrl+U`
clears it. Arrow keys, `Home`, `End`, `Delete`, and `Backspace` edit normally;
`Tab`/`Shift+Tab` move between fields. Long values scroll to keep the caret visible.
Pasted absolute repository paths replace the old path and strip surrounding
terminal padding. These editing controls apply to TaskAI forms, not native CLIs.
`Ctrl+W` removes the previous word; Ctrl/Option arrows move by word. Subordinate
worktrees default to the repository's current branch with an unused worktree name.
Non-Git and not-yet-committed repositories default to the same repository instead.
The Other repository field is editable and `F2` reopens its browser.

| Task location | Repository | Server |
|---|---|---|
| Local | Absolute local path | This computer |
| Remote | Absolute path, `~`, or `~/…` on the remote host | Select a saved SSH server |

TaskAI validates a remote repository and runs that Task's agent tmux sessions through the
recorded SSH alias. A `~` path is resolved on that host and the resulting absolute path
is stored, so `~/github` on `cruise2` becomes that machine's real home-directory path.
SSH credentials and options stay in your normal SSH configuration; TaskAI stores only
the host alias. Connections use non-interactive `BatchMode`, so keys/agents must already
be configured in SSH. TaskAI never falls back to local execution when a remote host is
unavailable.

Remote Subordinates currently share the Task repository rather than creating remote git
worktrees. Local OpenCode keeps its existing `serve` and `attach` pair together inside
its tmux runtime. Remote OpenCode is currently unavailable because its existing TaskAI
launch helper is not installed on the target; the other provider and ACP paths are not
redesigned by this feature.

## For agents and scripts

Local sessions receive `TASKAI_SESSION_REF`, so inside a locally managed CLI:

```sh
taskai context self            # who am I, why do I exist, my team chain
taskai context tree            # the whole task's chain
taskai context session <ref>   # another session's place
echo "prompt" | taskai session prompt <ref> --stdin --wait
taskai session cancel <ref>
```

`session prompt` needs the TaskAI TUI running, and currently accepts only local
`owned-tui` (Claude Code) targets; a shared-live, handoff-resume, or remote target is
refused with a message naming the reason rather than misrouted. Local context commands
work any time. This tmux feature does not add remote TaskAI installation, context
bootstrap, or hook delivery.

State lives in `~/.taskai/state/state.json` (owner-only), with the root directory
overridable through `TASKAI_HOME`.
It stores server profiles and per-server harness defaults, Tasks, hierarchy, repository
paths, local/SSH targets, provider session IDs, agent/utility tmux locators, and
the optional helper's preferred harness and exact task/Manager refs. Saved-login
probe results and raw authentication output are not persisted in this tracker.
Provider identity and tmux identity remain
separate. The dispatch log records correlation only, never prompt content.

TaskAI adds no daemon, resident TaskAI server, queue, or database. The normal tmux server
keeps provider processes alive after `q`; the existing TUI control-socket listener runs
only while TaskAI is open. TaskAI also writes into each local session's workspace: a
managed `AGENTS.md` block (with a `CLAUDE.md` symlink) so agents can discover
themselves, and Claude Code hook entries in `.claude/settings.local.json`. Both merge
into existing files and never overwrite your content.

## Development

`cargo test` runs the free tier: unit tests, integration against whichever provider CLIs
are installed, and cell-exact parity against `tests/fixtures/*.ans` — captured terminal
frames that are the approved design's golden reference. No test calls a model.

A proof whose harness is absent — or whose version is below the connector's supported
range — prints `unproven: <cli> is absent or unsupported (…)` and skips, so a clone
without all four CLIs is still green. Run with `TASKAI_REQUIRE_HARNESSES=1` to turn that
skip into a failure; that is how the release gate refuses to pass on unproven ground.
Billed live proofs are `#[ignore]`d and run only under the approval protocol in
[CONTRIBUTING.md](CONTRIBUTING.md); results land in `tests/evidence/`.

[CONTRIBUTING.md](CONTRIBUTING.md) covers the crate map and how to change the terminal
design. `backend_communication.md` is the architecture contract,
`docs/UX_PRODUCT_VISION.md` the product definition, and `feature-tmux.md` the approved
simple durable-session specification. `TEST_TMUX.md` is its non-billing acceptance plan.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
