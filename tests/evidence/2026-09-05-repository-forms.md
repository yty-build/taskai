# Repository-first forms — 2026-09-05

## Follow-up: explicit browsing, not Tab-triggered (current behavior)

Supersedes the initial iteration's automatic Tab-to-browse behavior below.

- Tab/Shift+Tab and Up/Down only change fields, including incomplete or invalid
  SSH entries. Enter on Connection opens the local/SSH picker; F2 is an explicit
  browse action from any field. Enter on Task name with no repo reports validation
  instead of unexpectedly opening a picker.
- Empty Connection visibly displays Local. It is a default, not text that must
  be deleted before typing SSH. Clearing the field restores Local; explicitly
  typing Local also parses as a local connection.
- Footer Enter label follows focus: Next field / Browse folders / Create task.
- Binary tests: 37 passed, one gated SSH test not invoked in this run. Clippy
  with `-D warnings`, release build, and `git diff --check` passed.
- Live tab 4: entered the intended path and `ssh cruise4b`; Tab left the form
  on Task name with no browser. Shift+Tab returned to Connection; Enter opened
  the actual `/home/ubuntu/github/smb-deal-flow` folder on cruise4b. This is the
  correct host, unlike the earlier incomplete/mistyped cruise4 value.
- Selected that folder and restored the unsubmitted name `smb deal flow`.
  No Task was created or model prompt sent. Existing providers stayed at
  s1 PID 19966 and s2 PID 41491.
- Current release/installed SHA-256:
  `831a029543ec0251f61e431aca1409edc6d70a6c6594b3c4f39fc7971a69cf25`.

The remaining sections document the initial implementation and its test history.

## Scope and acceptance checks

Improve the production TaskAI forms, not a fake-provider demo. Keep all provider,
ACP, and tmux lifetime behavior unchanged. No new services or remote installations.

- Repository first, connection second, task name last; choosing a folder suggests
  an editable name. No task is created until Enter on the task-name field.
- Local/SSH folder browser, asynchronous remote reads, cancel while loading,
  explicit host/path errors, and no local fallback for remote failures.
- Pasted absolute paths replace existing values and remove copied row padding.
  Preserve spaces within a path; handle LF, CRLF, and tmux's bare CR separators.
- Click-to-focus/caret positioning and Clear; Ctrl+A, Ctrl+U, arrows, Home/End,
  Backspace/Delete, Tab/Shift+Tab. Native CLI input remains separate.
- Broad centered dialogs and visible modal-only keybars at 110×30, 150×42,
  and 209×51. Long fields keep the editing cursor visible.
- Reopen the release in acp-build:4 without restarting either user's provider.

## Results

- `cargo test --workspace -- --test-threads=1`: passed on final code. Billed tests
  remain ignored. New binary suite: 34 passed, one explicitly gated SSH check.
- `cargo clippy -p taskai --all-targets -- -D warnings`: passed.
- `cargo test -p taskai --bin taskai cruise2_directory_picker_reads_real_folders
  -- --ignored --exact tmux::tests::cruise2_directory_picker_reads_real_folders`:
  passed. Reads home and ~/github; missing paths and shell-metacharacter paths
  fail safely. No writes to cruise2 or model turns.
- One earlier run of the existing reattach/draft test timed out. It passed on
  subsequent binary and full workspace runs; this form change does not claim to
  fix that intermittent timing issue.

## Live tab 4

Rebuilt and installed `/Users/trinath/.local/bin/taskai`. The installed and release
SHA-256 match: `ea97b780cb56c86d6b0ed8c5e111f10ef12e86cba62984ae3de61b495d30fee8`.
Previous installed binary retained at `~/.local/bin/taskai.before-repo-forms`.

Verified through the real terminal stream:

- New form has Repository / Connection / Task name and visible editing shortcuts.
- A terminal paste of a padded, two-row path renders exactly as
  `/home/ubuntu/github/smb-deal-flow`, without inserted whitespace. The initial
  live test exposed bare CR handling; fixed and repeated on the final release.
- SGR mouse down/up on Clear emptied the field. Unit tests also cover click hit
  geometry at three sizes. Physical pointer/pixel verification via iTerm's UI
  was unavailable; no claim of a screenshot-based visual review.
- Entering `ssh cruise2` then Tab displayed actual `/home/ubuntu/github` folders.
- The old draft's `ssh cruise4` does not resolve on this machine. The new picker
  showed the error and Esc returned to the editable form.
- `/home/ubuntu/github/smb-deal-flow` was not found on cruise2. No substitute
  repository or Task was created. Left the user's intended path and original
  `ssh cruise4` connection in the new, unsubmitted form.
- Both providers survived viewer upgrades: private server
  `taskai-17ace2a775c719e3`, s1 Claude PID 19966, s2 Codex PID 41491, both live.
  No real Task/session was destroyed and no prompt was submitted.

## Boundaries

The browser lists one folder level, not a recursive repository index. It uses
ordinary SSH with existing credentials/configuration and a 15-second browse
deadline. It neither creates repositories nor configures SSH authentication.
Cancelling discards the result; an in-flight read terminates by its deadline.
Form values remain single-line, with horizontal scrolling. Terminal font size
is controlled by the terminal application; TaskAI improves width and contrast.
