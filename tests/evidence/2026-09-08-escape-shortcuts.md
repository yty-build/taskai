# Escape and shortcut consistency — isolated acceptance plan

## Installed release and application gates

The production binary at `/Users/trinath/.local/bin/taskai` and the interface in
`acp-build:4.0` were updated to the accepted candidate:
`333239f9e32d50ef92a84c79a796ba2a45e5adcb2b33b7e5940ce6d8456a07db`.
Only the TaskAI viewer was closed and restarted. A fresh recovery copy of the
previous binary and registry was saved under
`/Users/trinath/.taskai/escape-shortcuts-backup.evxebK/`; no backup was restored.

Post-install checks in the actual 273×58 pane passed:

- `s` opens Settings; its footer displays the canonical keys.
- Add Server → empty-name validation error → one keyboard Escape cancels the
  dialog; the next Escape returns to Home.
- `?` opens shortcut Help, not a helper launch; Escape returns to Settings.
- The rendered Cancel button also cancels after a validation error, using a
  real SGR mouse press/release at column 173, row 57 (one-based).
- The interface was left at Settings → This computer, with no open draft.

Three tasks, seven agent records, and two server profiles remain. All task and
server records, plus all session fields other than live-state reconciliation,
compare equal to the immediate pre-restart snapshot. Provider IDs, launch
settings, workspaces, and tmux locators were preserved.

Read-only local/remote tmux PID checks before and after restart also match:
local s1=19966, s2=41491, s7=60906, s8=73968, s9=74123;
cruise4b s5=1381720, s6=1390151, task Terminal=1358854. No provider was restarted
or prompted. The actual full-size blank Cancel button worked before this patch;
the reproducible failure established below is the compact-resize path, not an
assertion that every earlier reported Escape symptom had the same cause.

Automated gates: 132 application tests passed (two explicit SSH tests skipped),
69 core/context/pane tests passed, and 45 TUI tests passed including parity.
Release build, formatting, targeted all-target clippy with warnings denied, and
`git diff --check` passed. The application suite passed twice, including the
existing local tmux mouse fixture. This is not a claim that all workspace live
provider proofs or the earlier intermittent scrolling investigation were rerun.

The TUI's 21 intentional shortcut/help golden changes were reviewed; historical
prototype source was not rewritten. Focused regressions cover stale provider
requests not swallowing a Settings Escape, resize recovery, native CLI key
forwarding, Help return state, literal form letters, and footer width limits.

Status: **candidate live acceptance passed; fixture cleaned up**. Baseline
reproduced the compact-resize Escape defect and the candidate fixed that exact
path. This is separate from the closed scroll audit.

## Approved key map

- Navigation: `s` Settings (`h` and comma retained as legacy aliases), `t` ordinary
  Terminal, `?` shortcut Help, `F1` setup helper, `F5` Check, `G` Home, `q` Leave.
- Forms keep text input; navigation letter shortcuts do not fire while editing.
- Esc closes the nearest dialog, then Settings, then returns to its original
  Home/task context. Native CLI Escape remains provider input.

## Isolation

Use a new temporary TaskAI home and uniquely named outer tmux server. All four
provider executables are explicit unavailable fixtures (exit 127); an SSH stub
records any attempted invocation and exits without connecting. No real provider,
authentication, model, remote connection, user tab, or saved user state is used.
An empty local task may be created inside the fixture solely to check return to
the previous task screen; no Manager/Subordinate runtime is needed.

## Acceptance checks

1. Home → Settings → Add Server. The modal owns all keyboard and mouse input;
   visible shortcuts describe the current form, not hidden navigation actions.
2. Enter with an empty display name keeps the form open with a clear error.
   One Esc closes only the form; a second Esc returns from Settings to Home.
3. Enter a name and syntactically invalid SSH text (for example a semicolon or
   extra shell command). Save shows validation feedback, preserves the draft,
   creates no profile, and runs no SSH. Esc has the same two-level behavior.
4. Use a valid-shaped alias plus a relative repository root to trigger the core
   save-error path before persistence/probes. Error remains visible, all fields
   remain editable, registry bytes/server count are unchanged, and Esc still
   returns to the server list before the previous screen.
5. Repeat form cancellation by clicking the actual rendered Cancel shortcut;
   derive coordinates from the candidate's current frame rather than assuming
   old geometry. The button and Esc must have identical scope and outcome.
6. Repeat from an empty task. Cancelling Add Server returns to Settings; backing
   out of Settings returns to that task, not unexpectedly Home or a CLI.
7. At 110×30 and 150×42, labels and shortcut bars are readable/unclipped and match
   the approved key map. Test forward/backward field navigation and clear/select
   actions that are advertised; modifier keys do not insert stray characters.
8. Record separate key events for sequential Escape presses, with enough time
   for the terminal's Escape decoding. Also cover a bounded fast sequence so the
   UI does not require an undocumented extra key. Do not confuse an incomplete
   terminal escape sequence with an application cancellation result.

## Evidence and cleanup

Record exact candidate hash, dimensions, literal frames before/error/after,
visible footer actions, task/server refs, registry equality checks, and stub SSH
invocation count. No passing claim for checks not actually performed. Preserve
existing user runtime PIDs by not operating their server or tabs at all. Close the
fixture viewer and verify its exact server has exited; any fixture task must be
removed using its exact ref, preserving temporary repository files for audit.

## Results

### Installed baseline

SHA-256 `0e404186384ea0ccb58013bae50b48c46b23e31f4591621a866a503763d430b3`.
Isolated root `/tmp/taskai-escape-qa.mMawcg`; outer server
`taskai-escape-qa-mMawcg`. A continuously drained, attached outer tmux client
received actual Escape and SGR click bytes. Its Escape decoding timeout was
10 ms; sequential Escape presses were separated by observed screen transitions.

At **110×30, 150×42, and 273×58**, empty-name, invalid-SSH, and core relative-root
save errors kept drafts editable and left registry bytes unchanged. Esc correctly
closed the form to Settings, then returned to Home. Rendered Cancel clicks also
worked at one-based coordinates **92,29 / 112,41 / 174,57**. Form keybar:
`Tab Next · Ctrl+A Select all · Ctrl+U Clear · Enter Save · Esc Cancel`.

**Reproduced failure:** error in Add Server at 150×42 → resize to 80×20 → one
Escape → restore 150×42 left the form open. The compact resize screen consumed
the Escape without cancelling the underlying modal. Normal-width cancellation
was not misreported as broken. No SSH invocation occurred in any baseline case.

### Candidate

Testing SHA-256 `333239f9e32d50ef92a84c79a796ba2a45e5adcb2b33b7e5940ce6d8456a07db`.

**22 recorded checks completed** (including binary/fixture identification):

| Check | 110×30 | 150×42 | 273×58 |
|---|---|---|---|
| Empty name → error → Esc Settings → Esc Home | Pass | Pass | Pass |
| Invalid SSH draft preserved; no registry changes | Pass | Pass | Pass |
| Core relative-root save error; draft/registry preserved | Pass | Pass | Pass |
| Click rendered Esc Cancel → Settings → Home | Pass | Pass | Pass |
| Navigation letters stay text; Ctrl+A replacement / Ctrl+U clear / BackTab | Pass | Pass | Pass |
| s Settings / ? Help / F1 helper / F5 Check / G Home | Pass | Pass | Pass |

Help showed `s Settings`, `t Terminal`, and `F1 Setup helper` consistently at all
sizes. `?` opened shortcut Help, not the helper chooser, and Esc returned to
Settings. F1 opened the helper chooser without creating a task or session; Esc
returned to Settings. Form fields containing `sq?Ght` remained text, so navigation
letters did not trigger actions while editing. Visible form keybars matched the
working clear/select/save/cancel controls.

The reproduced compact path now passes: errored Add Server → 80×20 resize screen
→ single Escape → restore 150×42 displays the server list, **not the old form**.
An additional empty local task verified Settings → Add Server → error → Esc
Settings → Esc returns to the **same task**, with `sessions=[]`.

Legacy `h` and comma still opened Settings. Clicking the actual `q Leave` footer
at one-based **111,41** exited the candidate from Settings. This was a real SGR
click routed through the attached outer tmux client, not a direct call into a
button handler. Shifted uppercase G worked as the Home shortcut.

Scope: ordinary Terminal launch and native CLI Escape are covered by root-owned
route/unit regressions, not by this no-provider/no-runtime live fixture. Physical
keyboard/trackpad hardware and the user's terminal tab were not operated.

### Exact cleanup

The empty fixture task `t1` was destroyed through its normal typed-ref confirmation;
its repository directory remains intact. The clicked Leave action ended the only
fixture viewer. `tmux -L taskai-escape-qa-mMawcg list-sessions` reported no server
running. Fixture registry has empty tasks/sessions, no helper refs, and only the
built-in local server with an empty setup-runtime map. The SSH log does not exist:
**zero SSH invocations**. No provider, authentication, model, or remote call was
made; no user runtime or state was touched. Temporary scripts/binaries/registry
remain under `/tmp/taskai-escape-qa.mMawcg` for audit.
