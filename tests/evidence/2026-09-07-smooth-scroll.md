# Smooth CLI scrolling — performance acceptance plan

Status: baseline/reversal, candidate boundary checks, and final 8 ms/16 ms tuning
comparison completed. These are observations under shared machine load, not hard
CI latency promises or a physical trackpad/display certification.

## Scope and isolation

Use the actual TaskAI release binary inside an attached outer tmux client, with
SGR wheel events entering through that client's PTY. A unique temporary TaskAI
home and private tmux servers contain explicit NO MODEL CALLS fixtures, numbered
history, and raw stdin recording. No user tabs, providers, saved credentials,
model prompts, SSH, package installation, or configuration outside the fixture.

Baseline installed release SHA-256:
`d7a0d2fbfe7e85b374b5a67d56f8890dcbcdfcef4cab5aece0f5275feee63f50`,
preserved at `/tmp/taskai-smooth-scroll.kEVkzI/taskai-baseline` before new builds.
Compare the eventual release candidate at identical dimensions and fixture state.

## Measurements and acceptance

1. **Last-event latency:** after settling, send exactly one wheel event and no
   subsequent input. Measure monotonic time until the client receives a content
   update (not the unrelated clock). Repeat at multiple idle phases; report
   median/p95/range and timeouts. The final wheel must update without a second
   user action.
2. **Sustained wheel:** emit a bounded sequence at a recorded rate, then stop.
   Report content-update cadence, output volume, and final-event completion.
   Rendering should remain responsive while input is arriving, not wait for the
   input queue to empty indefinitely. Do not infer display refresh rate from raw
   read packet boundaries.
3. **Idle cost:** sample viewer process CPU time and actual terminal output bytes
   over a fixed idle interval. Record load and CPU measurement granularity; no
   claim of zero CPU/output, since TaskAI has a clock and asynchronous status.
4. **Correctness:** compare fixed header/rail/footer around scroll, ignoring only
   the live clock. Verify recipient/PID unchanged and outer tmux mode/history
   remain zero. History-control input must not leak into fallback stdin; a
   deliberate numbered keyboard sequence must arrive without drops/reordering.
5. **Layouts and boundaries:** repeat last-event behavior inline, full-screen,
   and setup shell; resize and Ctrl+G must remain responsive. Keep native mouse
   forwarding semantics and the earlier exact-coordinate/drag tests intact.
6. **Cleanup:** leave/reopen preserves only fixture runtime PIDs; exact setup exit
   and task destruction remove them. List the named servers/PIDs afterward.

The benchmark includes scheduler, outer tmux, TaskAI, and private tmux latency;
it is not a physical trackpad/display measurement. Sample size, concurrent load,
version/hash, viewport, and timing limitations must accompany conclusions.

## Results

Candidate SHA-256:
`c72b02e0abf8007e7d2e44f0483223ee7a29876b0f82e6fa3ebfb008c9a95d59`.
Preserved as `/tmp/taskai-smooth-scroll.kEVkzI/taskai-candidate`.

Actual attached outer tmux client at **150×42**, mouse enabled and status hidden.
The benchmark timestamps raw client-PTY reads containing numbered content, not
clock-only output. Every single-wheel trial first returns to live and settles,
then sends exactly one event with no further input until a content update arrives.
There were **zero detector misses** in these runs. A raw read can split/combine
terminal updates: the sustained read-burst counts below are **not display FPS**.

| First content latency | Baseline median / p95 | Candidate median / p95 | Samples per build |
|---|---:|---:|---:|
| Inline single wheel | 104.50 / 106.61 ms | 2.12 / 11.35 ms | 16 |
| Full-screen single wheel | 111.77 / 291.36 ms | 4.08 / 12.59 ms | 8 |
| Setup single wheel | 9.55 / 145.08 ms | 3.72 / 12.43 ms | 8 |

At 120 wheel events/second for one second, final content arrived **111.29 ms**
after the baseline's last input versus **13.53 ms** for the candidate. The largest
observed content-read gap was **287.45 → 25.51 ms**. Content was delivered during
the input stream in both builds (189 → 537 raw content-read bursts); neither
required another user input to finish scrolling.

In the first pair, the candidate delivered more intermediate scrolling updates:
outer-client output for that burst was **267,592 → 523,635 bytes**. The baseline
reversal later emitted **972,645 bytes** under different scheduling. Active output
is therefore sensitive to update coalescing/load, not a fixed per-event cost or a
claim that every workload emits fewer bytes.
Over six seconds idle, output was effectively unchanged (**1,842 → 1,836 bytes**).
Viewer CPU time was **0.08 → 0.05 seconds**, approximately **1.33% → 0.83% of one
core**. `ps` CPU-time precision and these short windows limit conclusions; no
idle-cost increase was observed.

### Correctness and environment

- Both builds: zero changed chrome rows after excluding only CLI content and the
  live clock; deliberate ordered keyboard markers arrived byte-for-byte; outer
  tmux `pane_in_mode/history_size` remained `0/0`.
- Same fixture agent `s1`, PID **38424**, survived viewer changes between builds;
  private server `taskai-b0661a86c09e16fc`, outer `taskai-smooth-qa-kEVkzI`.
- Host load averages were very high and changing: baseline one-minute load
  **154.88**, candidate **75.20**. Unrelated OpenCode/Spotlight work was observed
  read-only and left untouched. The reverse-order baseline below separates the
  repeated 100 ms inline delay from the falling one-minute load average.
- An early baseline setup shell was delayed in user zsh startup for several
  minutes. It eventually completed and yielded the baseline setup samples.
  Subsequently only the isolated setup runtime was stopped and its private-server
  `SHELL` set to `/bin/sh` in an attempt to bound further startup; no user or
  product setting changed. A later `pane_current_command` check still reported
  zsh, so this did not establish a controlled change of the actual setup shell.
  The same second setup PID 43639 served subsequent comparisons. The agent
  fixture, viewport, and input stream were unchanged.

### Final confirmation and cleanup

Reverse-order baseline (after the candidate), same agent PID, one-minute load
**27.49**: inline median **103.92 ms**, p95 **105.28 ms** (n=16); final sustained
content **126.35 ms** after last input. The private counter ended `[595/1172]`,
matching one copy-mode entry then 119 five-line scroll increments. This retains
the roughly 100 ms inline delay despite much lower measured load.

The reversal also passed exact keyboard order, static chrome, and outer `0/0`.
Full-screen latency was highly variable (median 589.14 ms, p95 665.11 ms); setup
48.12/140.01 ms (n=8 each), illustrating why these are observations under shared
machine load, not service-level promises. Actual client resizing 150×42→110×30→
150×42 correctly produced full-screen child dimensions 110×25→150×37.

The key-release fairness candidate `d0969a364d439364d88b523bbd7ac02507f8a09b2174ad083584396aa8d799c7`
passed single-wheel checks, exact **512-byte** and **513-byte** ordered keyboard
bursts, static chrome, setup input, and resize. One initial sustained measurement
looked incomplete: the rendered counter was only 11 after 120 wheel events. This
was investigated instead of being treated as a pass.

Focused repeats queried **actual private tmux `scroll_position`**, not just the
displayed counter. All three were **595**, proving delivery of the expected
events. Outer displayed positions at roughly 350 ms were 595, 40, 595; after an
additional second **all three displayed 595**, with no further user input. The
Python client reader continuously drained output independently of the injector.
This demonstrated intermittent output delay, not permanently lost mouse events or
a final repaint requiring another key.

The rendering reviewer identified tmux output congestion as a concrete candidate
cause. The final bounded comparison records outer/private `client_discarded`
deltas, testing an 8 ms versus 16 ms render interval. Input remains immediate in
both.

## Final congestion comparison

16 ms release tested:
`89d379bbbadc33528393768f4de962d4d818a16a5ccc3f838bd49f1fcc11b691`.

| Three bursts, private client discarded-byte deltas | 8 ms | 16 ms |
|---|---:|---:|
| Burst 1 | 87,669 | 94,813 |
| Burst 2 | 57,736 | 0 |
| Burst 3 | 92,745 | 0 |

The **outer client discarded zero bytes in every burst**. The private tmux client
did discard intermediate output, so the delay was not caused by the benchmark's
outer reader failing to drain. All six bursts reached actual scroll position 595
and all six outer displays showed 595 after the additional one-second settle,
without further input. The two noncongested 16 ms bursts delivered final content
27.31/15.18 ms after the last event; maximum content-read gaps were 38.00/21.43 ms.

These counters match tmux 3.5a's documented-in-source behavior: when a client's
output queue exceeds its screen-sized threshold, tmux discards intermediate
output and schedules redraw/backoff in 100 ms intervals. Source:
[tmux 3.5a tty.c](https://raw.githubusercontent.com/tmux/tmux/3.5a/tty.c),
`TTY_BLOCK_*`, `tty_block_maybe`, and `tty_timer_callback`.

The 16 ms cap reduced observed congestion from three bursts to one, but this
small, changing-load experiment does **not** prove congestion impossible. Its
eight single-wheel samples ranged 2.99–109.04 ms (median 38.49 ms) during a period
when independent tmux queries also stalled. Earlier repeatable approximately
104 ms baseline idle-poll delay was removed; these measurements do not promise
every event will always render within 16 ms.

Root reports 263 tests passed / 6 ignored with `SHELL=/bin/sh`, plus formatting
and Clippy checks. Native user-zsh startup caused intermittent lifecycle-test
timeouts under load and is not relabeled as a clean native-zsh suite. A later
pane-snapshot critical-section optimization is **outside this live benchmark's
hash scope**; its implementation tests and final installed check belong to the
implementing/root agents.

## Cleanup

The isolated setup shell received deliberate `exit`. The normal TaskAI Destroy
Task form displayed exactly `t1`, one session, and repository/worktree/history
preservation; typed `t1` confirmed scoped destruction. Normal q closed the viewer.
Both `taskai-smooth-qa-kEVkzI` and `taskai-b0661a86c09e16fc` reported no server
running. PIDs 38424, 39046, and 43639 were absent. Fixture tasks/sessions are empty;
helper task/manager refs were cleared. Temporary binary/script/log files are
retained for audit, including `/tmp/taskai-smooth-scroll.kEVkzI/benchmark.py`.
No fixture runtime or test SSH work remains. No model calls or user-state
mutations were performed.
