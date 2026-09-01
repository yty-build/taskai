# TaskAI test plan

Verifies the build in `build_plan.md`, same milestone spine M0–M9. Ground truth: `backend_communication.md` §11 release gate, the UX vision journey, and the golden frames in `tests/fixtures/`.

## Philosophy

- TDD: every feature and bugfix starts with a failing test; only enough code to pass; refactor green. A milestone's tests exist before its components.
- Never test mocked behavior. A test that asserts what a stub was told to return proves nothing and is forbidden.
- Never mock in end-to-end tests. E2E means the real `taskai` binary, real provider CLIs, real sessions, real repositories.
- Pristine output. A passing run prints no stray errors. When a test intentionally triggers an error, it captures and asserts that exact output.
- Truthful skips. A test needing an absent harness reports `unproven: <harness> absent` — it never silently passes, and the release gate cannot pass with unproven entries.

## Tiers

| Tier | What | Spend |
|---|---|---|
| T1 unit/property | Real logic in-process: row width invariants, registry semantics, vt100 grid math, SGR handling, state transitions, snapshot construction | none |
| T2 connector integration | Real installed CLIs, free operations only: probe, version handshake, resolve, attach, serve/app-server attachment, bootstrap install | none |
| T3 live proof | Model-billed turns end to end through the conduit | billed — explicit T approval per batch |
| T4 TUI parity | `taskai --dump-frame` output compared against frames captured from the Python prototype; ANSI is ground truth (screenshots lie about leading) | none |
| T5 journey e2e | The real TUI driven via tmux send-keys/capture-pane as a test driver (production has no tmux dependency); full UX-vision journeys | billed only where a turn runs |

Illustrative test (pseudo code rule — expectation, why, value stated per test in the suite):

```text
Test: every rendered line is exactly the frame width
Why: the FILL grammar breaks visibly if one row over/underpaints; the prototype's verify()
  caught every such bug.
Expectation: for each screen and a matrix of (cols, rows), dump-frame yields lines whose
  printed cell count == cols, no trailing spill, no missing paint.
Value: layout regressions surface as a failing number, not a shredded screen.
Pseudo:
  for screen in screens, for (cols, rows) in sizes:
    frame = dump(screen, cols, rows)
    assert all(cell_width(line) == cols for line in frame)
```

## Per-milestone matrix

Tests are written first; the milestone is done when its gate is green.

### M0
- T1: row/seg composition (pad, clip, ellipsis), token values match `fill_system.py`, keycap/masthead/keybar cell layout.
- T4: `dump-frame home|boot` vs prototype fixtures at 209×51 and 110×30.
- Gate: width invariant green across the size matrix; `q` exits with terminal restored (T5 smoke).

### M1
- T1: ref stability (never recycled), topology (Manager children, peer Managers), state-transition validity, attribution recorded on every mutation, atomic persistence (kill mid-write → last good state), blast-radius computation.
- T2: worktree provisioning against a real fixture git repo — path preview matches created path; destroy leaves repo, worktrees, and `.git` byte-identical (hash trees before/after).
- Gate: create → kill → relaunch → identical tree; destroy-preservation green.

### M2
- T1: vt100 grid from recorded pty byte streams (colors, wraps, scroll regions, cursor); blit region math; key routing table (CLI focus forwards everything except Ctrl+G; navigation interprets single letters).
- T5: real shell in the pane — typed bytes arrive verbatim (including `a`, `m`, `d`, prose); Ctrl+G flips focus both ways; resize propagates.
- Gate: passthrough fidelity and boundary chord green.

### M3
- T1: connector contract conformance suite (every verb callable, capability declaration shape) — runs against each connector as it lands.
- T2: Claude probe (`claude --version` parsed), launch in fixture workspace, attach renders the real Claude TUI, idle detection, writer lock held during dispatch.
- T3 (approval): one prompted turn via conduit into the displayed PTY — work renders in that same visible Claude; completion from Stop hook / native JSONL; provider history contains exactly one new turn (integrity check, no duplicates).
- Gate: T2 green; T3 proof recorded.

### M4
- T1: snapshot completeness (task/self/team_chain/sessions), revision monotonicity, secrets/PIDs/sockets omitted, text view explains rather than dumps, caller binding rejects a model-claimed identity that mismatches the registered process.
- T2: bootstrap installer idempotent; never overwrites non-TaskAI `AGENTS.md` content; `CLAUDE.md` symlink honored; per-harness hooks installed only where the version supports them; failed refresh reports stale, never invented topology.
- T3 (approval): fresh session answers "who am I / why / team chain" correctly from `context self` before any other work.
- Gate: T1+T2 green; self-awareness proof recorded.

### M5
- T2: Codex — app-server + `codex-acp` dual attachment to one `threadId` verified before prompt is allowed; shim reconnect after forced close. OpenCode — loopback serve authenticated, facade + CLI share session ID, subscribe precedes prompt.
- T3 (approval): per harness — conduit turn renders live in the same visible session; completion only on `turn/completed` + idle edge (Codex) / `parentID` match + session idle (OpenCode); event-loss recovery deduplicates (kill the subscription mid-turn, recover from native snapshot, assert single result).
- Gate: dual-attachment refusal and live-render proofs recorded.

### M6
- T2: handoff-safe gating — dispatch refused unless the connector proves the safe state; unprovable resume reports `unsupported`, never a weaker claim.
- T3 (approval): full handoff — banner shown, `grok agent stdio` turn, exact-ID resume shows the result in the native Grok; label is `handoff-resume` everywhere; no UI surface ever says live.
- Gate: truthful-label sweep green (grep every state surface for a Grok LIVE claim — must be empty).

### M7
- T1: HUD aggregation derives from the one session list (a HANDOFF session never counts LIVE; ATTENTION rolls up Subordinate → Manager → Task); keybar shows only valid keys per screen/focus; drawers list only READY harnesses; unsupported model/effort/permission fields omitted.
- T4: every screen and overlay vs prototype fixtures (home, harness bay, new task + explorer, drawers, rail `└─` connectors, native/ACP toggle, attach, attention, destroy, help) across the size matrix.
- T5: journey steps 1–10 scripted end to end: launch → board → `h` bay → `n` task + explorer → `m` Manager → attach → Ctrl+G → `a` Subordinate with worktree preview → `d` blast radius + typed confirmation → second Manager as peer → attention attach → `T` home → quit → relaunch → resume exact CLIs. Workspace telemetry follows rail selection.
- Gate: parity suite + full journey green.

### M8
- T1: pipeline ordering (resolve → safety → single submit → correlate → outcome), writer-lock expiry only at proven terminal boundary, reconcile-before-resend decision from snapshot data.
- T2: `session prompt` source attribution — a registered native session initiator recorded as such, human UI as such; concurrent second writer refused with a clean structured error (captured and asserted).
- T3 (approval): Manager CLI drives a Subordinate via `taskai session prompt` — renders in the Subordinate's visible pane; cancel mid-turn transports the provider's native cancel and reports its result; kill the bridge mid-turn — native session intact, TaskAI reports honestly, reconciliation finds the turn without resending.
- Gate: collision, cancel, and reconciliation proofs recorded.

### M9
- T2: clean-account install — only TaskAI + provider CLIs; runtime dir 0700; sockets loopback/owner-only; grep argv/logs/state for credentials (must be empty); provider CLI binaries untouched (hash before/after); detach removes conduit processes only.
- T2: provider upgrade drill — bump a CLI version out of the declared range; probe reports the truthful downgraded capability, no silent breakage.
- Gate: full release-gate table below entirely green/recorded.

## Release-gate traceability (backend §11)

| Gate item | Test | Tier |
|---|---|---|
| Fresh native self-awareness | M4 `context_self_fresh_session` | T3 |
| Full team chain | M4 `context_tree_full_chain` | T1/T3 |
| Exact session references | M1 `ref_resolves_exact_mapping`, M3/M5/M6 per-connector resolve | T1/T2 |
| Attributed UI/native initiation | M8 `turn_source_attribution` | T2 |
| Same-session native rendering | M3/M5 `native_render_same_session` | T3 |
| Truthful handoff | M6 `handoff_truthful_label` | T2/T3 |
| Tools | per-connector `native_tool_flow_undisturbed` | T3 |
| Native questions/approvals | per-connector `approval_stays_native` (approval prompt appears in the CLI, TaskAI relays only where required) | T3 |
| Cancel transport | M8 `cancel_transported_native_result` | T3 |
| Writer collision | M8 `second_writer_refused` | T2 |
| Terminal detection | M3/M5/M6 `terminal_signal_correlated` | T3 |
| Replay/reconnect dedup | M5 `event_loss_recovery_dedup`, M8 `reconcile_no_resend` | T3 |
| Bridge failure | M8 `bridge_death_native_intact` | T3 |
| Provider upgrades | M9 `version_probe_gates_support` | T2 |
| Exact resume | M6 `grok_exact_resume`, M7 journey relaunch step | T3/T5 |
| Repository/provider-history preservation | M1 `destroy_preserves_everything`, M3 `history_integrity` | T2/T3 |

## Environments

macOS (darwin) primary. Installed and version-pinned for T2/T3/T5:

| Harness | Version | Mode under test |
|---|---|---|
| Claude Code | 2.1.226 | owned-tui |
| Codex | codex-cli 0.150.1 | shared-live |
| OpenCode | 1.18.25 (matches the version-gated context hook) | shared-live |
| Grok CLI | 1.0.13 | handoff-resume |

Rust: cargo ≥ 1.97. tmux present as the T5 test driver only. An absent or out-of-range harness marks its tests `unproven` and blocks the gate.

## Live-proof approval protocol

1. Before a T3 batch, the runner presents: milestone, tests, harnesses, expected turn count.
2. T approves the batch explicitly; no billed call runs without it.
3. Results (pass/fail, provider session refs, date) are recorded under `tests/evidence/` and referenced from the release-gate table.
4. A failed proof is re-run only after a fix, under a fresh approval.
