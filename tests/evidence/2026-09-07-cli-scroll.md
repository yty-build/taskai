# CLI-only wheel scrolling — acceptance plan and live evidence

Status: **local live checks passed and fixture runtimes cleaned up**. A
different-size reattach defect was found, fixed, and retested before deployment.

## Intended behavior

Wheel input over the selected CLI moves only that CLI's content. TaskAI's masthead,
sidebar, footer, active task, and selected recipient stay in place. There is no
second TaskAI transcript/history store or resident service: native mouse-aware
apps receive their own mouse events, otherwise the existing private tmux runtime
provides history through its own copy mode. Its position indicator belongs inside
the CLI area. While the CLI is focused, Esc or End returns that private pane to
live input. Wheel inspection from navigation does not steal keyboard focus:
Enter focuses the CLI; navigation Esc keeps its normal Home meaning.

## Isolation and fixture

- Local end-to-end owner: journey QA. Fixture root `/tmp/taskai-scroll-qa.kgXulH`;
  private TASKAI_HOME `/tmp/taskai-scroll-qa.kgXulH/state`, outer server
  `taskai-scroll-qa-kgXulH`, and private runtime server `taskai-697a12dc84ecc66d`.
- Fake version/auth-status providers only. One prints 240 numbered lines then
  records raw stdin without mouse reporting; another enables native SGR mouse
  reporting and records exact input. Both clearly say NO MODEL CALLS.
- No calls or keystrokes into existing user providers, tabs, or runtimes. No real
  credentials, installs, or model turns. Fixture commands write only within their
  temporary home. Both launches waited for the parent's completed debug build.
- Runtime agent owns the separate opt-in `cruise4b` private-shell proof. No
  duplicate remote workload will be launched by local journey QA.

## Acceptance checks

1. **Inline fallback.** Record selected task/session, private PID, numbered tail,
   outer pane mode/history, and frame geometry. Wheel up over CLI body must reveal
   older numbered lines only in that body. Sidebar/header/footer positions and
   selected recipient do not change (the live clock may advance).
2. **Return to live.** Wheel down approaches the newest output. Esc and End each
   leave the private tmux copy mode. The next deliberately typed marker reaches
   the same recipient. Wheel/history-control bytes never enter fallback stdin.
   No automatic Enter, draft mutation, or prompt submission occurs.
3. **Native mouse app.** Over a mouse-reporting CLI, forward the actual wheel event
   once with coordinates relative to the CLI body. Do not open tmux copy mode or
   send arrow/Enter substitutes. Wheel over TaskAI chrome is not provider input.
4. **Focus/layouts.** Repeat inline, full-screen (Ctrl+F), and Settings setup-console
   cases. Ctrl+G remains a clean navigation boundary. Scrolling never selects a
   different agent or changes a different runtime's history.
5. **Pending/empty/error.** Wheel during launch, before a viewer exists, and after
   provider exit is safe: no crash, no queued keyboard input, no extra runtime or
   recipient change. Application-level regressions cover deterministic pending
   timing if the live fixture's launch is too short to intercept reliably.
6. **Resize and outer tmux.** At 110×30, 150×42, and a larger viewport, CLI history
   stays inside its resized bounds and chrome remains fixed. Outer test tmux pane
   never enters copy mode; only the selected private pane may do so. Confirm the
   outer frame does not grow into scrolling strips or duplicate mastheads.
7. **Setup console.** An ordinary private setup shell prints numbered lines;
   scrolling and Esc/End preserve shell PID and do not affect team counts or the
   selected provider's saved settings.
8. **SSH (runtime owner).** On `cruise4b`, use an isolated ordinary shell, not a
   model CLI. Scroll private history, leave copy mode, type deliberately, and
   confirm the exact PID and target remain unchanged. Bound failures; no fallback
   to local execution or writes to unrelated remote work.
9. **Persistence/cleanup.** Viewer quit and reopen reuse the same fixture runtime;
   normal exact task destruction or explicit fixture shutdown removes only test
   runtimes. List both isolated tmux servers afterward to prove cleanup.

## Evidence to record after implementation

- Binary hash, timestamps, dimensions, explicit fixture status, exact task/session
  refs, and PIDs before/after each scroll or lifecycle operation.
- Literal tmux captures before/history/live; selected-body coordinate checks;
  outer/private `pane_in_mode` and history observations; raw stdin byte checks.
- Scoped automated test results from the implementing agents and a link to the
  remote proof. Distinguish simulated SGR event delivery from an actual physical
  trackpad gesture in the user's terminal emulator.
- Failures, fixes/retests, and exact cleanup. Do not claim physical-terminal
  visual parity, native-provider model behavior, or remote checks not performed.

## Local live results

Performed 2026-09-07 using the real TaskAI debug binary in an attached outer tmux
client with `mouse on` and `status off`. SGR wheel events were written to that
client's PTY, passing through outer tmux into TaskAI. This is actual terminal-event
delivery, **not a physical trackpad test** or a claim about paid model behavior.

Baseline debug SHA-256:
`b798b82b91c9ed4b5837563c0fff1be3dce661b388bbe1f00ffe910cf867f7ed`.
Capture-cancellation retest debug SHA-256:
`342bfdc9cbac4be705c43cc4136fb45b6846702aadd0fbd57cf571a6c169b7cc`.

Fixture identities throughout: task `t1`; fallback Manager `s1`, PID **10352**;
native-mouse Manager `s2`, PID **10700**; setup shell `setup-local-terminal`, PID
**11185**. Both providers explicitly print NO MODEL CALLS and record raw stdin.

| Check | Observed result |
|---|---|
| Inline, 150×42 | Fallback tail 211–240 moved to 201–231 after wheel up, with private `[10/211]` counter; wheel down moved forward. Header, rail, footer, and recipient unchanged. |
| Chrome invariant | Full capture comparison excluding only CLI body and live clock found **zero differences**. Outer pane stayed `pane_in_mode=0`, `history_size=0`. |
| Esc / End | Each left private copy mode. Fallback raw stdin stayed empty until deliberately typing `SCROLL_LIVE_OK`, then contained exactly those 14 bytes: no wheel, history-control, or Enter leakage. |
| Full-screen | Wheel revealed older lines inside its full-screen body; fixed header/footer unchanged. End returned to live. Repeated on capture-cancellation build. |
| Resize | Actual attached PTY resized from 150×42 to 110×30 in full-screen history; child became 110×25, private history stayed scoped, outer mode/history stayed zero. At 209×51 native inline child became 173×40. |
| Native mouse | Outer wheel at column 60, row 15 arrived exactly once as `ESC[<64;24;9M` / `ESC[<65;24;9M`, matching child coordinates. Native private pane stayed out of copy mode. |
| Chrome wheel | Header/rail wheel emitted no provider bytes and did not change the selected recipient. |
| Navigation wheel | Scrolling the selected fallback body inspected its history without stealing navigation focus or affecting the other runtime. Enter then End returned to live. |
| Drag focus boundary | On capture-cancellation build, native MouseDown followed by Ctrl+G generated one matching `ESC[<0;24;9m` release. No held button remained. |
| Drag resize boundary | Native MouseDown followed by actual PTY resize generated the same matching release; selected recipient and PID stayed unchanged. |
| Setup console | Ordinary shell printed 180 numbered lines. At 209×51, wheel changed first visible line 138→128, private mode became 1, End restored 0. Zero header/footer differences; outer mode/history stayed zero; shell PID unchanged. |
| Viewer leave | Normal q from navigation closed only the outer viewer. All three private PIDs remained unchanged and were reused after relaunch. |

Captures: [inline before](cli-scroll-inline-before.txt),
[inline history](cli-scroll-inline-history.txt),
[full-screen history](cli-scroll-fullscreen-history.txt),
[setup history](cli-scroll-setup-history.txt). Captures are literal terminal text;
they do not preserve colors or prove physical terminal-emulator visual parity.

### Defect found before deployment

Reopening at 150×42 after the previous viewer had exited at 110×30 retained the
old private pane size 74×19 instead of the expected inline 114×31. TaskAI's outer
frame was correct, but the native viewport displayed tmux filler dots beside the
undersized old window. PID stayed 10352. Toggling full-screen resized it correctly.
Root confirmed that the new viewer parser already reported the desired size, so
normal render-time resize skipped the pre-existing private window. Launch/setup
reattach must explicitly apply the desired size before publishing the viewer.

### Fix and final retest

Final debug SHA-256:
`67e3221a6d7f9bce5f0a66b2bf3e7dc1b7f6615a89b22be7b8090623c01694af`.
Reopened the existing fixture at 150×42, without resizing or toggling after
attachment. Existing `s1` changed from 150×37 to the correct inline **114×31**
before display, still PID 10352. The filler defect was gone. Wheel/Esc passed,
zero chrome differences, outer mode/history both zero. Existing setup shell
changed **209×46 → 150×37**, still PID 11185, with wheel/End working. See the
[fixed reattach capture](cli-scroll-fixed-reattach-history.txt).

Root reports final integrated workspace gates: **250 passed, 6 ignored**, format
check and Clippy passed. This includes deterministic mouse rectangle/pending-input,
viewport invalidation, drag-release, and existing-runtime resize regressions.
Local live QA does not claim to intercept every brief pending/exit transition;
the deterministic application tests cover those boundaries. Final release built:
`d7a0d2fbfe7e85b374b5a67d56f8890dcbcdfcef4cab5aece0f5275feee63f50`.

### Exact cleanup

The fixture setup shell received its deliberate `exit`; only PID 11185 ended.
The normal Destroy Task form displayed exactly `t1`, two sessions, and preservation
of repository/worktrees/history. Esc cancelled it and both agent PIDs remained.
Reopening and typing `t1` confirmed destruction; only fixture s1/s2 ended. The
fixture registry now has empty tasks/sessions and no helper task/manager refs.
Final stdin totals were 14 bytes (fallback marker only) and 84 bytes (native wheel
and matched press/release packets only).

Normal q closed the outer viewer. Both exact tmux servers reported `no server
running`, and `ps -p 10352,10700,11185` returned no processes. Temporary fixture
files remain for audit; no runtime is left running. No user runtime was touched.

### Scope limitations

SSH proof belongs to the runtime implementing agent and must be reported
separately from this local journey. No physical trackpad, paid model prompt,
provider upgrade, credential inspection, or user terminal interaction was used.
