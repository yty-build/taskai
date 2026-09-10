# Task Terminal utility acceptance — 2026-09-05

## Scope and checks

One lazy, reusable login shell per task, separate from agent records. Verify:

1. Open with `s` from navigation or select Terminal and press Enter; type inline immediately.
2. Run in the task repository on its recorded local/SSH target, without agent context injection.
3. Preserve one runtime and unsubmitted input across leaving/reopening TaskAI.
4. Toggle fullscreen with Ctrl+F; return to navigation with Ctrl+G; bridge the native cursor.
5. Exclude Terminal from team/HUD counts and manager destruction.
6. Shell `exit` closes its runtime; explicit reopen starts a new shell at the saved locator.
7. Task destruction closes its shell and agents, preserving peer tasks and repository files.
8. Reject a missing repository instead of falling back to a different working directory.
9. Keep the Terminal entry visible with a large team at 110×30, 150×42, and 209×51.

## Automated results

- `cargo test --workspace -- --test-threads=1`: passed; billed live-provider tests remain ignored.
- `cargo test -p taskai --bin taskai utility_terminal_rejects_a_missing_repository_without_starting_a_shell -- --test-threads=1`: passed (added after the full suite began).
- `cargo test -p taskai --bin taskai remote_utility_terminal_lifecycle -- --ignored --nocapture --test-threads=1`: passed against `cruise4b`, including the final repository validation guard.
- `cargo clippy --workspace --all-targets -- -D warnings`: passed.
- `git diff --check`: passed.
- `cargo build --release`: passed.

The initial parallel workspace run failed four time-sensitive PTY tests: startup
confirmation, viewer reattachment, truecolor startup, and utility shell startup.
All passed sequentially. The login-shell lifecycle test allows 20 seconds for
user shell startup files; it checks executed output rather than echoed input.
Parallel-load timing sensitivity remains in the test harness.

Lifecycle tests use isolated tracker homes/private tmux namespaces, a real login
shell, and exact-runtime cleanup. They do not submit model prompts. Remote tests
use `/tmp` as their working directory; local tests verify a repository sentinel.

## Live acceptance in `acp-build:4.0`

Installed release at `/Users/trinath/.local/bin/taskai`:

`ac371e02296bc8c178c979aeee61cb06b482b2da4bfc11478b34408d82d9c3ca`

Previous executable retained as `taskai.before-terminal-utility` beside it.

- Opened the existing `SMB_Deal_flow` task, then `s`.
- Actual remote shell: `zsh`, PID `1358854`, tmux session `t2-terminal` on private server `taskai-17ace2a775c719e3`, SSH target `cruise4b`.
- Executed `pwd` through TaskAI: `/home/ubuntu/github/smb-deal-flow`.
- Inline render showed `Terminal · Shell`, `RUNNING`, `CLI HAS FOCUS`, and `0 sessions · 0 harnesses` for this currently agent-empty task.
- Outer tmux reported visible host cursor at `(38,12)` inline and `(2,10)` fullscreen; Ctrl+F switched to the shell-only view and back.
- Existing local agent PIDs remained unchanged: `s1` Claude `19966`, `s2` Codex `41491`.
- No provider was upgraded, no model prompt was sent, and no real user task or agent was destroyed for acceptance testing.

The task's JSON now holds `terminal: {target, private_server, session}` separately
from the `sessions` array. Provider IDs and ACP/provider integrations are unchanged
by this utility feature.
