# Focus ownership highlight — release evidence

## Scope

One steady header edge identifies keyboard ownership: `▸ NAVIGATION` or
`▸ CLI INPUT`. The selected agent retains a muted fill when the console owns
input; navigation adds its stronger fill and a spaced selection arrow. The old
sidebar `CLI INPUT ACTIVE` message is removed. Harness stripes and state lamps
keep their identity/status meaning. Fullscreen and setup consoles share the CLI
edge. Native cells, cursor visibility rules, input routing, and pane dimensions
are unchanged; no timers, animations, dependencies, or daemons were added.

## Automated gates

- Application: **133 passed, 2 explicit SSH tests ignored**; final full binary
  suite passed after the spacing adjustment.
- TUI: **49 passed**, including four new focus-specific tests and cell-for-cell
  golden parity. Twelve intended goldens changed: task, attach, subordinate,
  destroy at 110×30, 150×42, 209×51. Compared with a pre-change fixture copy and
  reviewed the rendered rows. Historical prototype source was not changed.
- Real-PTY app regression covers repeated Enter / Ctrl+G transitions, visible
  and deliberately hidden native cursors, hover/wheel not taking focus, and
  identical 114×31 inline pane dimensions across focus changes.
- Render regressions check the active header, retained selected agent, unchanged
  harness/status colors, exact native cell text and attributes, fixed geometry,
  stable (nonanimated) output, large teams, and the utility Terminal marker.
- Release build, rustfmt, targeted all-target clippy with warnings denied, and
  `git diff --check` passed.

## Live isolated acceptance

Final binary SHA-256:
`2b253f150001d68ca00cf5c22296ca83ed8de16a8af4e5b6d60bc5cd43d9fb4c`.

Fixture: `/tmp/taskai-focus-qa.etycrY`, outer tmux server
`taskai-focus-qa-etycrY`, TaskAI home isolated beneath the fixture. All four
provider executable overrides pointed to `/usr/bin/false`; no provider/model
was launched. An ordinary local shell was opened through TaskAI's Terminal.

The final driver (`verify-focus.mjs`) passed **27 checks**:

- Inline typing shows the CLI edge and native cursor; Ctrl+G moves the edge to
  navigation and hides the cursor. Both states retained shell PID **40187** and
  identical native dimensions **114×31**.
- Literal SGR hover and wheel inspection do not steal navigation focus. A click
  in the console transfers focus without an extra Enter.
- Shell output explicitly hides its cursor: CLI focus stays indicated, while
  the host cursor remains hidden, including after navigation and re-entry.
  A subsequent native show-cursor request restores it.
- Ctrl+F fullscreen, Ctrl+G navigation, and Enter inline work; repeated inline /
  fullscreen transitions at 110×30, 209×51, and 273×58 retain the same shell PID.
- Leaving TaskAI preserves the shell. The exact fixture-owned shell was then
  stopped; both fixture tmux servers are absent. Repository and test artifacts
  remain. `captures.json` and `result.json` contain final frames and measurements.

An early driver PID lookup produced an empty tmux format result; the driver was
corrected to use `list-panes` with an exact window target and reject nonnumeric
PIDs. The final acceptance above was rerun with valid PID/dimension assertions;
the earlier invalid measurements are not counted as process-identity evidence.

## Installation and preservation

Installed the accepted release at `/Users/trinath/.local/bin/taskai` and restarted
only the TaskAI interface in `acp-build:4.0`. Previous binary and registry copies
are under `/Users/trinath/.taskai/focus-release-backup.zpsnv0/`; no state backup
was restored.

Read-only post-install inspection shows the new navigation edge in the actual
209×51 pane, with the host cursor hidden (`cursor_flag=0`) and the existing
SMB_Deal_flow Codex console visible. No further keys were sent after observing
the task selection had changed, to avoid interfering with user interaction.

Three tasks, seven session records, and two servers remain. Task/server records
and every session field other than live-state reconciliation compare equal to
the immediate pre-restart snapshot, including provider IDs and tmux mappings.
All pre/post runtime PIDs match: local s1=19966, s2=41491, s7=60906, s8=73968,
s9=74123; cruise4b s5=1381720, s6=1390151, t2-terminal=1358854. No agent prompt
was sent and no existing provider runtime was restarted.

This pass validates focus presentation and preserves the existing scroll/input
implementation; it makes no new end-to-end latency or remote model-proof claim.
