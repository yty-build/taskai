# Server profile and new-task journey QA — 2026-09-06

## Scope and isolation

Live debug UI driven through a private tmux server, not the user's tab or data.
Fixture root: `/tmp/taskai-server-qa.C4lfbG`; `TASKAI_HOME` was its `state` folder.
Outer tmux server: `taskai-server-qa-C4lfbG`; runtime server:
`taskai-0b5571fd50dcd614`. No model prompts, credentials, installs, or real SSH
connections. PATH fixtures returned versions only; the SSH fixture explicitly
failed with `SSH intentionally disabled, no network connection`.

The shared debug executable was rebuilt during parallel development. Initial
journey used the build available at 01:28 PT; quit/relaunch used the later build
available at 01:33 PT. SHA-256 of that final executable:
`28fcd4a48d27789c18b1018dca5046f9d22a59731663e1edb806eda86a0667c4`.

## Automated regression results

Scoped new-task tests and adjacent selected filters: 17 passed, zero failed.
Five `new_task` tests, one `tab_never` test, seven `local_` tests, and one each
for `repository_first`, `name_enter`, `cancelled_browse`, `modified_letters`.
The `local_` filter includes five adjacent tests; it did not run remote tests.

Coverage includes all forward/backward field orders; no implicit browser or
server changes on Tab; server selector is not a text field; local visible default;
contextual shortcuts; paste cleanup and mouse clear; explicit F2 repository
browse; cancellation of asynchronous browse; exact task/profile association;
modified keys never inserting text; picker Esc retaining the full draft; same
server selection/arrows preserving a custom repository; defaults not leaking
between hosts; archived profiles excluded; mouse label/value opening and row
selection; frame dimensions at 110×30, 150×42, and 209×51.

Two production defects caught while adding regressions were corrected by the
parent agent before the passing run: configured local repository root omitted
from the initial form, and same-server arrow cycling overwriting a typed path.

## Live observations

At 150×42:

- Settings opened with built-in `This computer`, four provider rows, separate
  installed/version and compatibility columns, and `AUTH Not checked`.
- Edited local root to the fixture repository. Edited Claude executable and
  default model. Exact values persisted in JSON; no session was created.
- Added `QA remote` with `ssh qa-unreachable` and `/home/ubuntu/github`. Failed
  fixture SSH returned an explicit reason while navigation remained available.
  Editing the profile showed its connection as read-only, with Tab skipping it.
- New Task opened on Server with the configured local root. Enter opened saved
  servers; selecting remote replaced the root and focused Repository. Custom
  remote path survived opening the chooser, highlighting Local, and Esc.
- An actual SGR mouse click selected Local. F2 opened the local repository
  browser. Space chose the folder; typed task name and Enter created only `t1`,
  with `server_profile_id: local` and the canonical local repository path.
- Settings Terminal opened a real native shell in `setup-local-terminal`, PID
  13872. Typing a shell printf displayed `QA_SETUP_INPUT_OK`. Ctrl+G returned to
  Settings without ending that shell or adding an agent/session record.
- Configure CLI used the explicitly refusing fixture, returned to Settings with
  a startup error, and retained the setup mapping. It did not affect the live
  setup shell. This is a failure-path test, not a provider authentication proof.
- Archive confirmation described its scope. Esc kept the remote profile active;
  confirmed archive set only its archived flag. The task, local settings and
  setup shell PID remained unchanged.

At 110×30:

- Settings provider information and shortcuts remained visible, wrapping the
  keybar to two rows. Opening Terminal reused PID 13872 and its previous output.
- Quit TaskAI left that shell alive. Relaunch, Settings, Terminal reattached to
  the same PID and output; profile defaults and archive state persisted.
- `exit` in the shell produced an explicit exited/disconnected view. Ctrl+G
  restored Settings. The new-task server picker now showed only Local, excluding
  the archived remote profile.

## Cleanup and limits

The test shell exited normally and TaskAI was quit normally. Both exact private
tmux servers subsequently reported `no server running`. Fixture files and JSON
remain for audit; there are no active test runtimes. No user runtime was touched.

Actual remote provider login/model access is not established by these tests.
SSH runtime and connector tests are owned separately. This evidence does not
claim full-workspace test results or pixel parity outside the stated checks.
