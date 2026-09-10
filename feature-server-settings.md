# Saved servers and harness settings

Implemented September 6, 2026. Replaces repeated task-level SSH entry with saved
servers while preserving the existing tmux, provider, ACP, and control-socket paths.

## Product flow

1. Home/task navigation → `s` → Settings → Servers (`h` and `,` remain legacy aliases).
2. This computer is pinned. Add a display name, SSH-config alias, and optional
   repository root for a remote server. Credentials/options remain in OpenSSH.
3. Saving a server or changing its harness defaults immediately starts background
   checks of its connection/tmux and configured CLI versions; selecting an unchecked
   server also starts checks. Results account for enabled state, executable override,
   and supported model flags. Safe native saved-login checks run where supported.
   Checks are asynchronous and bounded; `F5` retries. These are runtime readiness
   checks, not a run of the application's development test suite or a model turn.
4. `t` opens that server's setup shell; select a harness and `c` opens its native
   configuration CLI. Users perform login/install/configuration themselves.
   `Ctrl+G` returns to Settings and refreshes the checks automatically.
5. New task → Server → Repository → Name. `Enter` on Server opens the chooser;
   Tab only moves fields. `F2` browses the selected server. Escape cancels a
   pending remote repository check without losing the draft.

Settings uses a server rail and a **Provider / Status / Version** table, not
separate enabled/compatibility/auth columns. Status is actionable:

| Status | Meaning |
|---|---|
| Ready | Supported CLI plus native-reported saved sign-in; no model-access proof |
| Sign in needed | Native status command explicitly reports signed out |
| Couldn't verify | Discovery failed or native saved-login status is unknown |
| Disabled | This profile disables new agents using that provider |
| Update needed | Installed CLI is below the supported version |
| Unsupported | Current integration cannot support the requested target/default |

Pending work shows Checking. Selected-provider details retain executable/model
defaults, sign-in detail, the reason/next step, and the model-access caveat.
Unknown login does not block an otherwise compatible native CLI from opening.
`F1 Setup helper` opens the optional local helper described in
[feature-taskai-helper.md](feature-taskai-helper.md).
`?` opens shortcut Help; `G` goes Home and `q` leaves TaskAI. On the Settings list,
`s` is a no-op, `e` edits the server, `Enter` edits harness defaults, and `d` archives
the server. These navigation letters remain literal text inside edit fields.

Edit dialogs are centered, wide,
and isolated from background table content. Fields support mouse focus/clear,
selection, paste, normal editing, explicit save/cancel, and visible shortcuts.

## Persistence contract

Existing `state/state.json` remains the only tracker. Additive fields:

- `servers`: stable ID, name, immutable Local/SSH target, optional repository root,
  archived flag, per-harness settings, and setup runtime locators.
- `Task.server_profile_id`: selected server; existing `connection` is retained as
  its exact runtime routing snapshot.
- `Session.launch`: executable and model snapshots captured for a new agent.
  Missing legacy values mean native defaults, not retroactive profile settings.
- `helper`: optional preferred harness and exact task/Manager refs for the normal
  local helper. No second tracker or name-based task association is introduced.

Version/login check results are transient. Native authentication stdout/stderr,
account details, credentials, and tokens are not persisted in the state JSON.

Migration creates This computer and deduplicates existing SSH task targets into
profiles. It does not alter provider IDs, runtime locators, task paths, hierarchy,
or existing launch defaults. Reopening migrated state is idempotent. IDs are never
recycled. A server target cannot be edited into a different host.

Disabling a harness prevents new agents, not reattaching existing ones. Changing
defaults affects only new agents. Archive excludes a profile from new-task choices;
it does not destroy tasks, agents, repositories, or setup processes.

## Runtime contract

No new service, daemon, database, credential manager, or provider backend. Existing
SSH transport and private tmux sessions remain the persistence mechanism.

Saved-login discovery invokes only the allowlisted native read-only commands
`claude auth status --json` and `codex login status`, using the configured
executable on the selected target. Output is reduced to SignedIn/SignedOut/Unknown
in memory; raw stdout/stderr is never echoed into UI errors or logs. Timeouts,
unrecognized responses, and transport failures remain unknown. OpenCode has no
supported safe login proof here, and Grok exposes no dedicated status command;
their login status remains Couldn't verify. TaskAI does not parse credential
files or run login, model prompts, token validation, or paid access tests.

**Saved sign-in is not proof of current model access, quota, entitlement, or a
successful model turn.** A version alone can never produce Ready. Save/F5 and
returning from native setup refresh these facts; they never authenticate for the
user or submit a prompt.

Setup terminals are lazily created in the profile's repository root or home folder.
Their exact target/server/session locators are persisted before launch. They are
separate from task agents and HUD counts; `Ctrl+G` returns to Settings. Closing
TaskAI disconnects the viewer only. Reopening reuses the live process even if the
profile's defaults changed. Native `exit`/quit ends the process. Archived profile
records retain their locators for explicit native tmux access/cleanup.

Setup consoles strip inherited `TASKAI_*` agent identity and do not bootstrap
agent files. Ordinary authentication environment remains intact. Task destruction
does not destroy server-wide setup consoles because those are not task members.

Configured executable paths are passed as an executable, not evaluated as shell
commands. Model text is a separate native argument. Claude, Codex, and Grok use
their supported model flag. OpenCode's current attach path cannot honor a model
override and reports that limitation; leave it blank and use native configuration.
Remote OpenCode remains unavailable pending its existing launch-helper support.
No unsupported path falls back silently to a different server/provider/model.

## Acceptance plan

- Migrate legacy local and SSH tasks; reopen byte-identically; preserve exact
  provider/runtime identities; never recycle server IDs.
- Snapshot enabled/executable/model defaults on creation; preserve old sessions.
- Check cache isolation by host and effective settings, including stale in-flight
  results and explicit refresh. Add must schedule checks before any UI frame or
  polling pass; a failed Add must schedule nothing.
- Exercise native signed-in/signed-out/unknown responses, malformed output,
  timeout and transport failures, status precedence, and unsupported providers.
  Assert raw auth output never reaches labels/errors/state; version success
  alone never means Ready; no model or login command is executed.
- Return from Configure CLI or setup Terminal via Ctrl+G and verify automatic
  recheck without changing saved defaults or launching a second setup process.
- Exercise add/edit/archive, invalid input, clear/paste, keyboard/mouse selection,
  cancel, visible controls, and exact geometry at 110×30, 150×42, and 209×51.
- New task uses the chosen server and root; same-server/cancel preserve draft;
  Tab never starts browsing; remote validation cancellation creates nothing.
- Real local/SSH setup shell: type, return, close viewer, reattach same PID;
  keep out of team counts; exact cleanup leaves peer runtimes untouched.
- Full workspace tests, formatting, warning-free clippy, release build, and live
  installed-binary verification in `acp-build:4`, preserving existing runtimes.
- No billed model prompts, automated login, upgrades, or changes to remote auth.

Current live evidence: [server-profile journey](tests/evidence/2026-09-06-server-profile-journey.md)
(initial version-only implementation) and
[single-status/helper journey](tests/evidence/2026-09-06-helper-live-journey.md)
(current UI with explicit native-status fixtures; not a real login/model proof).
