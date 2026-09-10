# CLI-only scrolling — installed release

Installed and exercised on 2026-09-07 in `acp-build:4.0`
(`live-proto-claude`, pane `%113`, 209×51).

## Build and regression gates

- `cargo fmt --all -- --check`: passed.
- `cargo test --workspace`: **250 passed, 0 failed, 6 ignored**. Ignored
  opt-in proofs are not included in the passing count.
- `cargo clippy --workspace --all-targets -- -D warnings`: passed.
- `cargo build --release`: passed.
- Installed `/Users/trinath/.local/bin/taskai` matches the release SHA-256:
  `d7a0d2fbfe7e85b374b5a67d56f8890dcbcdfcef4cab5aece0f5275feee63f50`.
- Previous executable and registry backed up in the private directory
  `/Users/trinath/.taskai/scroll-release-backup.TiyEsM/`.

The implementation keeps host mouse capture active, hit-tests and translates
events into the selected CLI viewport, and uses native app mouse handling or
the existing private tmux history. No additional transcript store or service was
introduced. Mouse capture is cancelled safely across focus, modal, recipient,
viewer, and geometry changes. Reattachment also resizes existing private panes
before display, fixing a different-size reopen defect discovered during QA.

## Installed real-session smoke test

Only the TaskAI viewer was quit and replaced. F1 reopened the existing helper
Manager `s7`; no task, agent, provider process, or model turn was created.

Three SGR wheel-up events at host column 91, row 21 were delivered to the actual
installed TaskAI in the outer pane. Captures before and during history were
compared in memory; private conversation contents were not added to this report.

| View | Child size | Changed CLI rows | Changed outer-frame cells | Private mode |
|---|---|---|---|---|
| Inline | 173×40 | 35 | **0**, excluding only the live clock row | 0 → 1 → End → 0 |
| Full-screen | 209×46 | 42 | **0**, all header/footer cells compared | 0 → 1 → End → 0 |

Outer pane remained out of copy mode (`pane_in_mode=0`), with mouse and SGR
reporting enabled. Its pre-existing history size stayed 121 across the recorded
history/return/full-screen checks. The private helper PID remained **60906**.
The viewer was left in inline CLI focus at live input, not in history.

The three local runtime PIDs remained `s1=19966`, `s2=41491`, `s7=60906`.
Read-only SSH inspection confirmed the three existing cruise4b runtime PIDs
remained `s5=1381720`, `s6=1390151`, `t2-terminal=1358854`.
Registry comparison against the backup found tasks, servers, helper references,
and session metadata unchanged (excluding reconciled session state): three
tasks and five agents. No provider prompt was submitted.

The old viewer had an unsubmitted helper `new-task` notification. That ephemeral
notification cleared on viewer restart; the helper conversation and runtime
were retained. It was **not** reissued by forging the helper caller identity,
and no new-task form was submitted. This limitation was reported to the user.

## Independent local and SSH proofs

The [local acceptance evidence](2026-09-07-cli-scroll.md) covers attached outer
tmux client delivery, ordinary-shell history, exact native mouse bytes,
inline/full-screen/setup layouts, resize, focus transitions, and verified exact
fixture cleanup. Its captures contain test fixtures only.

The runtime agent separately ran:

```sh
TASKAI_REMOTE_TEST_HOST=cruise4b cargo test -p taskai configured_ssh_mouse_routes_native_input_and_history -- --nocapture
```

`tmux::tests::configured_ssh_mouse_routes_native_input_and_history` passed:
**1 passed, 0 failed**, 18.69 seconds. It verified history/counter behavior,
End and vi-mode Escape returning to live, byte-exact SGR wheel delivery to a
mouse-aware shell fixture, repair of saved mouse-off settings, unchanged PID,
and an unchanged default-server session snapshot. Fresh private test namespaces
were used, with no model/provider call. Exact-session cleanup guards ran for
both fixtures; post-cleanup absence was not independently inspected.

## User behavior and test limits

- Wheel over the CLI scrolls its content, leaving TaskAI's outer layout fixed.
- With CLI focus, Esc or End leaves private tmux history. Outside tmux history,
  native applications retain their own key behavior.
- Ctrl+F still toggles full-screen/inline; Ctrl+G returns to navigation.
- Wheel inspection from navigation does not steal keyboard focus. Enter focuses
  the CLI before using its history keys; navigation Esc retains its Home action.

These were terminal-event/PTY tests, not a physical trackpad gesture in the
user's terminal emulator. No paid model behavior, operating-system scroll
override, or unlimited-history claim is made.
