# Settings status and optional helper release — 2026-09-06

Installed and checked in `acp-build:4.0` (`live-proto-claude`, pane `%113`) at
22:45–22:48 PDT. This is production Rust code and the installed release binary,
not the earlier fake-provider prototype. Viewer PID after update: **59129**.

## Release gates

- `cargo test --workspace`: **236 passed, 0 failed, 6 ignored**. Ignored tests
  remain opt-in SSH/provider-turn proofs; no billed model turns were run.
- `cargo fmt --all -- --check`: passed.
- `cargo clippy --workspace --all-targets -- -D warnings`: passed.
- `cargo build --release`: passed.
- Logs: `/tmp/taskai-helper-tests.log`, `/tmp/taskai-helper-clippy.log`, and
  `/tmp/taskai-helper-build.log`.
- `target/release/taskai` and `/Users/trinath/.local/bin/taskai` have identical
  SHA-256: `7e74a430c03876049e35fd543ae4437b26e043161af7919ecb3a51d9d391ed74`.
- PATH resolves `/Users/trinath/.local/bin/taskai`. No root installation required.

The [isolated live helper journey](2026-09-06-helper-live-journey.md) proves normal
task/Manager creation, zero initial stdin, direct input, same-PID reopen and
quit/relaunch, explicit F8 review, preserved drafts, and scoped destruction.
It uses explicitly fake providers and does not claim a real model turn.

Final regressions additionally cover read-only whitelisted helper status;
ANSI-only/blank first frames accepting buffered input; the selected helper
provider carried into local Settings; correct back navigation; and pending
versus failed helper checks. Geometry tests cover 110×30, 150×42 and 209×51.

The native Settings shell test exposed slow personal zsh/Conda startup under
parallel load. Native tmux captures showed commands already queued inside the
shell while Conda initialized, then executed unchanged. Its eventual bound now
matches the existing 20-second utility-shell test; no fixed sleep or production
shell-startup behavior was added. Three concurrent focused runs passed.

## Real readiness checks

The opt-in sanitized auth test ran native status checks locally and on cruise4b:
Claude local SignedOut; Codex local SignedIn; both remote SignedIn. Only the
derived enums were logged. No login/logout, credential-file inspection, model
prompt, installation, upgrade, or authentication mutation was performed.

Installed Settings independently rendered:

| Target | Claude | Codex | OpenCode | Grok |
|---|---|---|---|---|
| This computer | Sign in needed · 2.1.226 | Ready · 0.153.3 | Couldn't verify · 1.18.25 | Couldn't verify · 1.0.13 |
| cruise4b | Ready · 2.1.263 | Ready · 0.153.4 | Unsupported · 1.18.10 | Update needed · 0.2.117 |

There is one Status column, not separate compatibility/auth columns. Selected
details explain the next action and that saved sign-in does not establish model
access or quota. The OpenAI documentation skill informed the distinction between
[native saved-login status](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
and a successful authenticated model call; a version string alone never means Ready.
Raw auth stdout/stderr is not shown or stored in JSON.

## Existing work preserved

Quit only the old TaskAI viewer from Home with `q`, confirmed the outer zsh
returned, then installed and launched the new binary. Previous binary and state
are backed up in private directory:
`/Users/trinath/.taskai/helper-release-backup.VGEMe2/`.

All exact runtime PIDs remained alive before and after:

| Target | Session | PID |
|---|---|---:|
| Local | s1 | 19966 |
| Local | s2 | 41491 |
| cruise4b | s5 | 1381720 |
| cruise4b | s6 | 1390151 |
| cruise4b | t2-terminal | 1358854 |

A JSON comparison against the backup confirmed tasks, servers, full session
records except observed liveness, provider IDs, launch settings, and tmux
locators are identical. On entry s5 was recorded QUEUED although its native
process was live; relaunch reconciled it to LIVE. The portfolio now shows four
LIVE agents, zero QUEUED, and the same two tasks. No provider was restarted.

Configuration remains outside the repository at
`/Users/trinath/.taskai/state/state.json`. Only the additive empty `helper` settings
were migrated. No helper task, agent, or new setup runtime was created in user
data during this installed smoke test. No new service or provider/ACP replacement
was introduced.

## Installed interaction checks and handoff

- Settings → remote server shows completed real statuses and explanations.
- Settings `?` opens the optional local helper chooser without creating records.
- Choosing Codex, then `h`, opens This computer with **Codex** selected, not Claude.
- Esc returns correctly to Home; F1 reopens the chooser.
- Final screen is the helper chooser with **Codex · Ready** selected. Enter will
  create/open the user's optional normal helper Manager. No prompt is filled or
  submitted automatically. Esc returns Home without creating anything.

Literal tmux captures:
[Local Settings](2026-09-06-helper-release-local.txt),
[cruise4b Settings](2026-09-06-helper-release-remote.txt),
[final helper chooser](2026-09-06-helper-release-chooser.txt).
