# Optional TaskAI setup helper

Implemented September 6, 2026. A user-chosen local native CLI Manager helps explain
server setup and requests reviewable UI destinations. It is not a new service,
background installer, authentication proxy, or privileged automation engine.

## Design and lifecycle

- Before a helper exists, `F1` from Home/task navigation or Settings opens a local
  provider chooser; `?` opens shortcut Help instead. Opening/canceling it creates nothing. The user
  chooses an available provider; known signed-out providers require native setup.
- First confirmed open creates an ordinary visible `taskai-helper` task and one
  local Manager. Standard task/team counts, CLI focus, launch defaults, tmux
  persistence, and typed destroy confirmation all apply. Helper model turns use
  the chosen provider's ordinary account and billing.
- There is no automatic prompt, prefill, or Enter. Complete native trust/login
  prompts before asking a question. Ctrl+F toggles inline/full-screen CLI;
  Ctrl+G returns to task navigation.
- Reopening uses the stored task/Manager refs and existing process, not display
  names. A user's identically named task is never adopted. Existing helper
  harness, launch snapshot, provider ID, and tmux locator remain unchanged.
- Normal destroy clears stale helper refs; reopening can create new never-reused
  refs. Destroying only its Manager retains the ordinary task for a new Manager.
  Workspaces, guide files, and native provider history are preserved.

The same `state/state.json` stores optional `helper.preferred_harness`,
`helper.task_ref`, and `helper.manager_ref`, alongside normal tasks, sessions, and
server profiles. Legacy state defaults to no helper. New helper records are
staged and saved atomically: disabled-provider or persistence failure leaves no
orphan task and consumes no task/session refs. The local server profile supplies
new-agent executable/model defaults; existing sessions are not rewritten.

The private `$TASKAI_HOME/helper` directory contains versioned bundled README.md
and AGENTS.md guidance, plus managed copies of `gauntlet-loop` and
`taskai-staff-objective` under `.claude/skills/` and `.grok/skills/`. Only
TaskAI's marked blocks are updated; unrelated text/files are preserved.
Skill metadata stays at the start of each file; its managed marker is a YAML
comment. Existing skills without a managed block, malformed blocks, and
symlinked guide or skill targets are rejected. Ordinary
local context bootstrap makes the guide discoverable to the native agent. The
newest injected TaskAI Context remains the source of current session/assignment
information, not an old transcript or the static guide.

When the user states an objective, the helper asks probing questions first,
offers named/fetchable/comparable gauntlet bars, writes a paste-ready brief
(`The task`, `The build method (How)`, `The bar to hit (When)`), and recommends
Manager/Subordinate harnesses from helper-status readiness and work type. It
does not know remaining usage or quota. It still does not create tasks or
agents: after the user accepts, it queues one existing `taskai helper request`
at a time for Ctrl+G then F8 review.

## Reviewable UI handoff

Read-only commands work without a helper session:

```sh
taskai helper guide
taskai helper status
```

Status reads existing JSON without opening the mutable Registry; it reports
recorded state and does not claim a fresh runtime or login check.

The registered helper Manager can use its actual TASKAI_SESSION_REF with the
existing TaskAI control channel:

```sh
taskai helper request open-settings [server-id]
taskai helper request add-server
taskai helper request new-task
taskai helper request add-manager <task-ref>
taskai helper request add-subordinate <manager-ref>
```

These are an allowlist of UI destinations, not arbitrary commands or writes.
The live TaskAI UI must be running. The request checks the registered caller and
target and queues one pending review; a second pending request is refused.
No server/task/agent is created and no form is saved by requesting it.

The user presses Ctrl+G to leave CLI focus, then F8 in Home/task navigation to
review/open the ordinary destination. Esc in navigation dismisses the request.
An open form keeps its input/draft and does not accept F8 as a replacement;
finish or cancel it and return to navigation first. The normal form still needs
explicit submission. Targets are revalidated before review.

## Authority and safety

No new daemon, credential store, or tracker is introduced. Saved-login checks
and the single Settings Status follow [feature-server-settings.md](feature-server-settings.md).
Native-reported saved sign-in does not prove model access or quota. Authentication
output is transient, never persisted or exposed as a helper diagnostic.

The guide prohibits reading/copying credentials or hand-editing live tracker
JSON. Native authentication stays in Configure CLI on the chosen target. The
helper must obtain explicit user approval before installations, upgrades,
configuration/model changes, model calls, or destruction. A UI navigation
request does not authorize those actions. This instruction-based native helper
does not gain a new sandbox or elevated OS privileges.

## Test plan and current evidence

1. Legacy/no-helper defaults; duplicate display names; exact ref/native identity
   reuse; local launch snapshots; disabled/persistence failure rollback; normal
   destroy and stale-ref recovery without ID reuse or unrelated-task changes.
2. Guide and skill installation is idempotent, preserves unrelated bytes, and
   refuses malformed blocks/symlinks before changing the other managed files.
   Bundled skills include gauntlet-loop (attributed, CC BY 4.0) and
   taskai-staff-objective (probe first; recommend; do not create).
3. Isolated real UI with explicitly fake providers: chooser cancellation, ordinary
   task/Manager counts, zero initial stdin, direct typing, Ctrl+F/Ctrl+G, and
   F1/Settings helper entry reuse. Check layout at supported terminal sizes.
4. Read-only guide/status and rejected/accepted requests leave JSON unchanged;
   only the exact registered helper caller can queue valid destination refs.
   Test pending-request refusal, explicit F8 review, Esc dismissal, and form-draft
   preservation. No form submission or config mutation occurs from a request.
5. Quit/relaunch reattaches the same tmux PID and output; exact normal destroy
   ends only fixture work. No real model calls, login, installs, or user data changes.

[Live evidence and literal tmux captures](tests/evidence/2026-09-06-helper-live-journey.md)
cover the integrated journey with version/status fixtures. They prove UI routing
and persistence, not real provider login or billed model behavior. Core tests are
in `crates/taskai-core/tests/helper.rs`; guide tests are in `crates/taskai/src/helper.rs`;
UI/request regressions live beside the implementation in `app.rs` and control tests.
