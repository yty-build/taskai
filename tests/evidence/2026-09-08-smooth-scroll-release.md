# Smooth scrolling — installed release

Installed in `acp-build:4.0` (`live-proto-claude`, 209×51).
`/Users/trinath/.local/bin/taskai` matches the built release SHA-256:
`0e404186384ea0ccb58013bae50b48c46b23e31f4591621a866a503763d430b3`.

## Changes

- Host input and displayed-PTY output wake the same in-process event loop.
  Output no longer waits for a 100 ms host-input poll. A bounded input queue
  preserves order; bounded processing batches prevent continuous wheel input
  from starving redraws. The input reader is joined before terminal restoration.
- The first frame after idle can render immediately. During bursts, redundant
  intermediate frames are combined on a 16 ms cadence; input forwarding is not
  delayed for that deadline. Animation and background bookkeeping retain their
  separate 100 ms cadence, and hidden panes do not wake the renderer.
- Adjacent terminal cells share style sequences. Parsed-cell equivalence tests
  cover defaults, attribute changes, truecolor/256-color, wide/combining glyphs,
  clipping, padding, and row boundaries.
- The live parser lock now covers a temporary immutable vt100 screen clone,
  not per-cell String allocation. Cells, cursor, mouse protocol, and paste mode
  are resolved from that same copy after unlocking. This is not a new history
  store or retained cache. Mouse and size queries remain no-clone operations.

There is no new daemon, transport, dependency, or history database. Provider and
ACP behavior was not changed.

## Measurements and limits

The [live performance evidence](2026-09-07-smooth-scroll.md) records actual
attached outer-tmux client/PTY delivery, baseline reversal, rapid input, exact
keyboard order, resize, setup console, and cleanup. The earlier candidate removed
the repeated roughly 104 ms inline delay; its ordinary-wheel medians were about
2–4 ms in those runs. Those figures are identified by candidate hash, not claimed
as a physical-trackpad or final-release latency guarantee.

The final 16 ms pacing reduced observed private-tmux congestion from three of
three bursts to one of three. Every burst reached the correct private and settled
outer history position. One shared-host stall remained. More frames are not
automatically smoother: tmux's own congestion backoff can discard intermediate
output. The detailed report includes discarded-byte counters and the primary
tmux source reference.

Renderer benchmark, 209×46 uniform-style body, release-linked actual functions:

| Metric | Previous | Optimized |
|---|---:|---:|
| Default-colour changed frame | 192,793 bytes | 11,001 bytes |
| RGB-colour changed frame | 385,073 bytes | 11,921 bytes |
| Default-colour row serialization median | 2.625 ms | 0.112 ms |
| RGB-colour row serialization median | 8.758 ms | 0.233 ms |

These are 94–97% smaller frame payloads for the specified fixture, not a claim
that every active scrolling stream emits fewer total bytes. The faster viewer
can display more intermediate frames. Idle output did not increase in the live
comparison; see that report for load and CPU-measurement caveats.

The final snapshot change was tested and profiled separately after the live
candidate benchmark:

```sh
TASKAI_SNAPSHOT_PROFILE=1 cargo test --release -p taskai-pane snapshot -- --nocapture
```

Parser critical-section median: **445 → 49 µs** at 173×40 and **842 → 84 µs** at
220×60. The corresponding p95 values were 651→97 and 1471→190 µs. Export still
occurs, but outside the reader lock. Tests verified immutable cells/metadata
after subsequent live parser mutations, including alternate-screen and resize.

## Installed real-session verification

Only the TaskAI viewer was replaced. F1 reopened the existing helper Manager s7;
Ctrl+G restored its previous navigation focus. Its private tmux history remained
at position **60**, not reset to live input.

Three wheel-up packets through the installed outer TaskAI moved private history
**60 → 75**; three wheel-down packets restored **75 → 60**. Comparing captures in
memory found **40 changed CLI rows and zero changed outer-frame cells**, excluding
only the live clock row. No private conversation text was saved to this report.
The outer pane stayed out of copy mode, with mouse/SGR capture enabled and its
pre-existing history size unchanged at 125 during the check.

All original runtime PIDs remained unchanged:

- Local: s1=19966, s2=41491, s7=60906.
- Read-only SSH verification on cruise4b: s5=1381720, s6=1390151,
  t2-terminal=1358854.

Registry comparison found tasks, servers, helper references, and session metadata
unchanged (excluding reconciled state): three tasks and five agents. No model
prompt was submitted. Previous executable and private registry backup:
`/Users/trinath/.taskai/smooth-release-backup.L9KyEt/`.

## Regression gate status

Release build, formatting, and all-target Clippy passed. An earlier integrated
candidate passed 263 tests with six ignored under `SHELL=/bin/sh`. Native user-zsh
startup had caused intermittent lifecycle-test timeouts, so this was explicitly
qualified as a plain-shell suite.

The final broad reruns were **not uniformly green** under shared-machine load:
the main binary run passed 123 tests but one standalone tmux mouse fixture failed;
an isolated retry passed. A later diagnostic run and protocol-readiness checks
are recorded separately; readiness alone was not proven to explain the failure.
An unchanged real Codex `thread/list` integration timed out, and the process-loss
fixture timed out waiting for its repository screen. These failures are not
silently counted as passes or claimed fixed by the scrolling implementation.
The final focused core/context/pane/TUI run passed **111 tests**, including the
new snapshot, notification, and style-equivalence coverage. The six CLI/release
integration checks passed; the separate process-loss check above did not.

A single diagnostic rerun of the standalone tmux mouse fixture still failed its
viewer-text assertion, but confirmed native `in_mode=1`, `scroll_position=35`,
and SGR negotiation: all eight wheel events reached tmux. Client diagnostics
reported 3,390 written bytes and zero discarded bytes while the viewer retained
the old live text. This remaining fixture failure concerns a stale/missing
viewer repaint, not dropped wheel input. Protocol-readiness gates alone did not
resolve it. No production code was changed to hide the failure.
Source review also ruled out the proposed five-second terminal-query timeout as
an established cause: the assertion waits ten seconds, and tmux does not gate
redraw on those query-completion flags. `client_written` counts queued bytes,
not confirmed delivery. Classify this as an unresolved standalone PTY repaint/
transport failure, not a proven harmless fixture artifact or established
production regression. It remains an explicit limit on a fully-green claim.
Sources: [tmux redraw path](https://github.com/tmux/tmux/blob/3.5a/server-client.c#L2534-L2581)
and [terminal query/output handling](https://github.com/tmux/tmux/blob/3.5a/tty.c).

The installed check above proves the real selected CLI route and state
preservation. It does not prove zero latency under arbitrary CPU, terminal,
network, or output pressure. All isolated live QA runtimes were cleaned up;
temporary benchmark/evidence files were retained. No user runtime was destroyed.
