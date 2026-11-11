# Contributing to TaskAI

Thanks for looking. This file covers how to build it, how the tests are organized, and
the one workflow that isn't obvious — changing the terminal design.

## Build and run

```sh
cargo build --release
install -m 0755 target/release/taskai ~/.local/bin/taskai   # any directory on PATH
taskai
```

TaskAI drives provider CLIs you install yourself: `claude` (≥ 2.0), `codex` (≥ 0.100),
`opencode` (≥ 1.18), `grok` (≥ 1.0). None are required to build or to run the test suite.
Missing or out-of-range CLIs show as SETUP in the harness bay.

## Where things live

| Crate | Owns |
|---|---|
| `taskai-core` | Domain model, Registry, and session mailboxes: tasks, sessions, stable refs, topology, states, persistence, workspace provisioning, destroy semantics, mail protocol and delivery accounting |
| `taskai-context` | Context snapshots (`self`/`session`/`tree`), the `AGENTS.md` bootstrap installer, per-harness context delivery |
| `taskai-connectors` | The connector contract, the four harness connectors, and the conduit: writer exclusion, dispatch correlation, cancel transport |
| `taskai-pane` | portable-pty spawn/resize/io and the vt100 grid the working pane blits |
| `taskai-tui` | The FILL design system, screen renderers, overlays, focus and key routing |
| `taskai` | The binary: TUI, `context`, and `session` subcommands |

`backend_communication.md` is the architecture contract — read it before touching a
connector. It defines each harness's mode (`shared-live`, `owned-tui`, `handoff-resume`)
and what TaskAI is and isn't allowed to do on a provider's behalf.

## Tests

```sh
cargo test
```

No test calls a model, so the suite is free to run. It covers real logic, real ptys, and
real provider CLIs where they're installed.

**Absent or outdated harnesses skip.** A proof whose CLI is missing — or whose version is
below the connector's supported range — prints `unproven: <cli> …` and skips, so a clone
with none of the four CLIs is still green. That's deliberate: a skipped proof must never
read as a passing one.

```sh
TASKAI_REQUIRE_HARNESSES=1 cargo test    # absent harness becomes a failure
```

Use that before claiming a release is verified.

**Billed live proofs** are `#[ignore]`d and marked `billed:` in their ignore reason. They
run one real model turn against a real provider. The protocol:

1. Propose the batch: which tests, which harnesses, expected turn count.
2. Get explicit approval from the maintainer. No billed call runs without it.
3. Record the result — pass/fail, provider session refs, date — under `tests/evidence/`.
4. Re-run a failed proof only after a fix, under fresh approval.

## Changing the terminal design

`tests/fixtures/*.ans` are **golden frames**: recordings of every screen at three window
sizes, storing each cell's glyph, foreground, background, and bold. `parity.rs` renders
the same screens and compares every cell, so an accidental color or column change fails
immediately and names the exact cell.

That means an *intentional* design change fails too. When the change is what you meant:

```sh
TASKAI_BLESS=1 cargo test -p taskai-tui --test parity
git diff tests/fixtures        # review what actually moved
```

Blessing re-records the frames from the current renderer. It produces no diff when nothing
visually changed, so a blessed diff is exactly the set of cells you altered — review it
like any other code change. Never bless to make a red suite go green without reading the
diff; that is the one way to silently lose the design.

## Style

```sh
cargo fmt
cargo clippy --all-targets -- -D warnings
```

Both are enforced in CI. `clippy.toml` pins the MSRV so lints don't suggest APIs newer
than the supported Rust version.

Source files open with two `ABOUTME:` comment lines saying what the file does. Tests state
what they expect, why it matters, and what the check is worth — follow the surrounding
style.
