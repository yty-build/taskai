# Server Settings release verification — 2026-09-06

## Gates

- `cargo test --workspace`: **208 passed, 0 failed, 6 ignored**. The six are
  two opt-in SSH tests and four billed provider-turn tests. No billed turns run.
- `cargo fmt --all -- --check`: passed.
- `cargo clippy --workspace --all-targets -- -D warnings`: passed.
- `cargo build --release`: passed.
- Final suite output: `/tmp/taskai-server-settings-final-tests.log`.
- Installed `/Users/trinath/.local/bin/taskai` matches `target/release/taskai`:
  SHA-256 `703cb706bbff2cdb18757e0aab3666b1ef4e2c336d717e1d2000495ba5d9b2ba`.

## Parallel QA

[Isolated live journey](2026-09-06-server-profile-journey.md) covers save/edit,
keyboard/mouse server choice, repository browse/create, cancellation, archive,
and a real local setup shell surviving viewer quit/relaunch with the same PID.

Runtime-focused verification: 9 probe tests, 6 launch tests, 25 tmux tests passed
(one SSH test ignored in the focused default run). Separate actual `cruise4b`
configured-executable cache-edit proof passed in 4.08 seconds; the stale-PATH,
same-PID remote lifecycle proof passed in 22.44 seconds. These used isolated
runtime targets, not user task processes. Only shell/version operations, no
model prompts, automatic installation, upgrades, or authentication changes.

Root integration tests additionally cover cancellable remote repository checks,
draft locking and dropped late results, correct server association, visible
Settings controls, return navigation, and default-home setup shell reattachment.
Settings geometry/actions tested at 110×30, 150×42, and 209×51.

## Installed live verification

Stopped only the TaskAI viewer in `acp-build:4.0` from Home using `q`; confirmed
the outer zsh returned before installing. Backed up previous binary and JSON in:
`/Users/trinath/.taskai/server-settings-backup.h9t0VW/` (private directory).
Previous binary SHA-256:
`74525b2ed806f1914e9ec1eb2fe6b7eb9877167bcaa98ea6a197bf024dc06f87`.

Live registry migrated to `local` / This computer and `server-1` / cruise4b.
An identity comparison against the backup confirmed existing task fields
(excluding the additive profile reference), session hierarchy/workspace,
provider mappings, and exact runtime locators remained identical. Session
launch defaults remain native. No sample task/agent was added to user data.

All five existing tmux runtimes remained live with unchanged PIDs:

| Target | Session | PID before and after |
|---|---|---:|
| Local | s1 | 19966 |
| Local | s2 | 41491 |
| cruise4b | s5 | 1381720 |
| cruise4b | s6 | 1390151 |
| cruise4b | t2-terminal | 1358854 |

Installed UI showed four LIVE agents/two tasks, readable `h Settings` footer,
and friendly server names on portfolio cards. Settings→cruise4b reported:
Claude 2.1.263 and Codex 0.153.4 compatible; OpenCode 1.18.10 unavailable through
the current remote helper path; Grok 0.2.117 below the supported connector range.
Every row reports authentication **Not checked**, including compatible providers.

New Task defaulted to This computer. Enter showed both saved profiles; selecting
cruise4b and F2 opened its actual `/home/ubuntu` directory with `github/` visible.
Cancelled the browser and form without creating a task. Final release includes
contextual selector shortcuts (no ineffective Select all/Clear on Server).

## Boundaries

Provider authentication/model access is still checked through native CLIs, not
guessed from versions. Remote OpenCode and an OpenCode attach model override
remain explicitly unsupported. Existing ACP/provider paths are retained.
This is production core/UI code, not a fake-provider prototype.
