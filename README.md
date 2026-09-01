# TaskAI

One terminal. Many tasks. Teams of native CLI agents working in parallel.

TaskAI is the command center around agent CLIs. It owns the task board, hierarchy,
status, creation flows, and navigation chrome; the entire working pane belongs to the
selected session's actual native CLI — Claude Code, Codex, OpenCode, or Grok CLI.
`Ctrl+G` is the one reserved boundary chord; every other keystroke passes through to
the CLI unchanged.

## Install

```sh
cargo build --release
sudo install -m 0755 target/release/taskai /usr/local/bin/taskai
```

Only the install step needs `sudo`. To run the build without installing it, use
`./target/release/taskai`.

A clean machine needs only this binary plus the provider CLIs you use, user-installed:
`claude` (≥ 2.0), `codex` (≥ 0.100), `opencode` (≥ 1.18), `grok` (≥ 1.0). TaskAI never
modifies or upgrades them; missing or outdated CLIs show as SETUP in the harness bay.

## Run

```sh
taskai
```

Boot probes the harnesses, then the portfolio opens. Keys while navigation has focus:

| Key | Does |
|---|---|
| `Enter` | Open task / attach the selected session's CLI |
| `n` | New task (name + repository, with an explorer) |
| `m` | Add Manager · `a` Add Subordinate under the selected Manager |
| `c` | Toggle native / ACP conduit view |
| `d` | Destroy (shows the exact blast radius; typed confirmation) |
| `h` | Harness bay · `T` Home · `?` Help · `q` Quit |
| `Ctrl+G` | Focus the CLI — and the only chord that returns |

Destroy removes TaskAI sessions and records only: repositories, worktrees, and
provider-native history are always preserved. Quitting TaskAI detaches the display;
sessions reopen from the board, and bound provider sessions resume by exact ID.

## For agents and scripts

Sessions receive `TASKAI_SESSION_REF`, so inside any managed CLI:

```sh
taskai context self            # who am I, why do I exist, my team chain
taskai context tree            # the whole task's chain
taskai context session <ref>   # another session's place
echo "prompt" | taskai session prompt <ref> --stdin --wait
taskai session cancel <ref>
```

State lives in `~/.taskai` (owner-only). The dispatch log records correlation only,
never prompt content. `TASKAI_HOME` overrides the location.

## Development

`cargo test` runs the free tier: unit, real-CLI integration, and cell-exact parity
against `prototype/taskai_tui.py` (the approved design). Billed live proofs are
`#[ignore]`d and run only under the approval protocol in `test_plan.md`; results land
in `tests/evidence/`.
