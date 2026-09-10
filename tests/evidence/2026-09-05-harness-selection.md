# Harness selection QA — 2026-09-05

## Reproduced cause

Before the selector correction, Manager/Subordinate forms stored only the ready
harnesses. Clicking the Harness row set focus but did not change the selected
provider. Left/right silently clamped at the first/last item. With one available
CLI, both displayed arrows therefore appeared ineffective, while the hidden
providers had no visible reason for being unavailable.

The approved interaction is a focused selector, not a dropdown: Tab reaches the
Harness field; left/right and the visible mouse arrows cycle all four entries;
clicking the label focuses it. Disabled entries explain their state and cannot
create a session. Esc cancels the form. F5 rechecks CLIs while retaining entries
such as the Manager name, Subordinate role, and workspace choice.

## Independent regression checklist

- Manager and Subordinate forms each reach Harness through Tab/BackTab.
- Left/right cycle Claude, Codex, OpenCode, and Grok in both directions.
- Left/right arrow clicks change the entry; label clicks only focus.
- Missing or below-minimum CLIs remain visible with actionable reasons.
- Enter on a disabled provider preserves the form and creates no tracker/runtime.
- F5 preserves form values and selection during/after refreshed discovery.
- All-disabled discovery still allows inspecting the form and its reasons.
- Enter on a ready provider launches that exact provider with immediate CLI focus.
- Rendering and click hit targets match at 110 × 30, 150 × 42, and 209 × 51.

## Live results — completed 2026-09-06

The rebuilt `target/debug/taskai` ran in the private outer tmux server
`taskai-harness-qa-FcBxyx` with a fresh tracker and temporary PATH fixtures.
Claude 2.1.0 and Codex 0.150.0 were ready; OpenCode returned an explicit probe
failure; Grok reported 0.2.117 and therefore remained below the configured minimum.

All tested selector checks passed:

- Manager keyboard navigation reached every provider and wrapped both directions.
- Label clicks focused without changing providers; subsequent arrow keys changed
  the focused selector.
- Actual SGR mouse clicks on both chevrons worked in **both** Manager and
  Subordinate forms at 110 × 30, 150 × 42, and 209 × 51.
- Enter on disabled OpenCode and Grok preserved the form and displayed the cause.
  Before any successful creation the tracker still had `next_session: 0` and an
  empty session list, proving no hidden session or runtime creation.
- A test-only switch made every provider unavailable. F5 refreshed both Manager
  and Subordinate forms without losing their typed name/role, workspace, or
  selected provider. Enter on disabled Codex remained blocked.
- Restoring the ready fixtures and pressing F5 kept Codex selected. Creating the
  Manager and Subordinate launched exactly the fake Codex executable, not the
  first/default CLI. The resulting terminal identified test PIDs 3708 and 4098;
  both accepted immediate typed input while TaskAI showed CLI HAS FOCUS.
- Esc canceled the whole Manager form and returned to task navigation without
  creating another record.

Cleanup used TaskAI's exact `t1` task-destroy confirmation. It closed the two test
provider sessions and left an empty tracker. Both the private runtime server
`taskai-7dcd114853ce1945` and outer test server reported no server running after
the viewer exited. The temporary fixture files remain available under
`/tmp/taskai-harness-qa.FcBxyx`; no test process remains.

The fixtures print QA ONLY / NO MODEL CALLS and run ordinary `cat`; no user
provider executable, user task, or user tmux tab was mutated. No installs or model
calls occurred. A conversation interruption separated initial setup from the
final bounded proof; these results describe the resumed test, not an unattended
two-hour run.

## Primary Grok evidence

Read-only commands executed on local and `ssh cruise4b`:

- Local `grok --version`: `grok 1.0.13 (5e9a58528b76) [stable]`.
- Remote `grok --version`: `grok 0.2.117 (f1c0609308) [stable]`.
- Remote `/usr/bin/grok` resolves to
  `/usr/lib/node_modules/@xai-official/grok/bin/grok`.
- Both `grok --help` describe Grok Build TUI and expose `--resume` by session ID.
- Both `grok agent stdio --help` succeed with matching command usage/options.

Thus the remote binary is the official Grok package with the expected native
command surface, not an absent executable or unrelated tool. This does **not**
prove ACP initialize/loadSession compatibility or exact result correlation for
version 0.2.117. The current connector minimum is 1.0.0 and prior integration
documentation identifies tested version 1.0.13. The selector should truthfully
show installed 0.2.117 and the configured minimum; the minimum should not be
lowered merely because a version/help command succeeded.

No Grok session, prompt, update, or installation was performed for this review.

## Release acceptance — 2026-09-06

- Final default-parallel workspace suite: **175 passed, 0 failed, 6 ignored**.
- `cargo fmt --all --check`, `cargo clippy --workspace --all-targets -- -D warnings`,
  `cargo build --release`, and `git diff --check`: pass.
- `TASKAI_REMOTE_TEST_HOST=cruise4b cargo test -p taskai configured_ssh_provider
  -- --nocapture --test-threads=1`: **2 passed**, 11.36 s. Discovery returned Claude
  2.1.263, Codex 0.153.4 and Grok 0.2.117. An isolated stale-PATH tmux runtime
  preserved exact argv/cwd/context, one launch, same PID and unsent draft across
  reattachment; exact cleanup preserved its peer until separate fixture cleanup.
- Local wrapper regressions cover path/argv quoting, empty arguments, startup
  output suppression, cwd/context restoration, and rejection of shell-only
  function/alias providers. Provider arguments are never interpreted as shell code.
- Installed release SHA-256:
  `74525b2ed806f1914e9ec1eb2fe6b7eb9877167bcaa98ea6a197bf024dc06f87`.
  `/Users/trinath/.local/bin/taskai` matches `target/release/taskai`; prior binary
  retained at `/Users/trinath/.local/bin/taskai.before-harness-fix-20260905`.

Actual user tab `acp-build:4.0` / `%113`, 209×51, now runs that release. The existing
SMB_Deal_flow Subordinate form showed Claude **1/4 READY**, Codex **2/4 READY**,
OpenCode **3/4 UNAVAILABLE** with the remote shared-live limitation, and Grok
**4/4 UNAVAILABLE** with installed/minimum versions. Real keyboard arrows and an
SGR mouse click on the visible left chevron changed selections. F5 preserved the
Grok selection across recheck; the original Codex choice, empty role, and same-repo
setting were then restored. The form was not submitted and no new user agent was
created.

All existing process IDs were unchanged across installation/relaunch: local
Claude `19966`, local Codex `41491`, remote Codex `1381720`, and remote Terminal
`1358854`. No software upgrade or billed model turn was performed.
