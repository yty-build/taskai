# TaskAI

One terminal. Many tasks. Teams of native CLI agents working in parallel.

TaskAI is the command center around agent CLIs. It owns the task board, hierarchy,
status, creation flows, and navigation chrome; the entire working pane belongs to the
selected session's actual native CLI — Claude Code, Codex, OpenCode, or Grok CLI.
`Ctrl+G` is the one reserved boundary chord; every other keystroke passes through to
the CLI unchanged.

The project is TaskAI; the repository is named `acp-gateway` after the agent-conduit work
it grew out of.

## Install

```sh
cargo build --release
install -m 0755 target/release/taskai ~/.local/bin/taskai   # no sudo needed
```

Use any directory on your `PATH`. `/usr/local/bin` is root-owned on macOS, so installing
there needs `sudo install -m 0755 target/release/taskai /usr/local/bin/taskai`. To run the
build without installing it at all, use `./target/release/taskai`.

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
| `c` | Toggle the pane label between the native and ACP conduit view |
| `d` | Destroy (shows the exact blast radius; typed confirmation) |
| `h` | Harness bay · `T` Home · `?` Help · `q` Quit |
| `Ctrl+G` | Focus the CLI — and the only chord that returns |

Destroy removes TaskAI sessions and records only: repositories, worktrees, and
provider-native history are always preserved.

**Sessions do not currently outlive TaskAI.** Quitting closes the panes TaskAI owns, so
the provider processes in them stop. The board, the topology, and the provider session IDs
all persist, so relaunching shows the same tree and `Enter` resumes a bound session by its
exact ID — but in-flight work is interrupted, not continued. Making agents survive a quit
is the subject of `feature-tmux.md`, which is a proposal, not shipped behavior.

## For agents and scripts

Sessions receive `TASKAI_SESSION_REF`, so inside any managed CLI:

```sh
taskai context self            # who am I, why do I exist, my team chain
taskai context tree            # the whole task's chain
taskai context session <ref>   # another session's place
echo "prompt" | taskai session prompt <ref> --stdin --wait
taskai session cancel <ref>
```

`session prompt` needs the TaskAI TUI running, and currently accepts only `owned-tui`
(Claude Code) targets; a shared-live or handoff-resume target is refused with a message
naming the reason rather than misrouted. The context commands work any time.

State lives in `~/.taskai` (owner-only), overridable with `TASKAI_HOME`. The dispatch log
records correlation only, never prompt content. TaskAI also writes into each session's
workspace: a managed `AGENTS.md` block (with a `CLAUDE.md` symlink) so agents can discover
themselves, and Claude Code hook entries in `.claude/settings.local.json`. Both merge into
existing files and never overwrite your content.

## Development

`cargo test` runs the free tier: unit tests, integration against whichever provider CLIs
are installed, and cell-exact parity against `tests/fixtures/*.ans` — captured terminal
frames that are the approved design's golden reference. No test calls a model.

A proof whose harness is absent — or whose version is below the connector's supported
range — prints `unproven: <cli> is absent or unsupported (…)` and skips, so a clone
without all four CLIs is still green. Run with `TASKAI_REQUIRE_HARNESSES=1` to turn that
skip into a failure; that is how the release gate refuses to pass on unproven ground.
Billed live proofs are `#[ignore]`d and run only under the approval protocol in
[CONTRIBUTING.md](CONTRIBUTING.md); results land in `tests/evidence/`.

[CONTRIBUTING.md](CONTRIBUTING.md) covers the crate map and how to change the terminal
design. `backend_communication.md` is the architecture contract, `docs/UX_PRODUCT_VISION.md`
the product definition, and `feature-tmux.md` the proposed durable-session specification.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
