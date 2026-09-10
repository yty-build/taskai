# Simplified tmux persistence acceptance evidence

**Date:** 2026-09-03 (America/Los_Angeles)

**Visible driver:** `acp-build:4` / pane `%113` / `209x51`

**Remote target:** `ssh cruise2`
**Provider cost:** none. Every interactive runtime was
`tests/fixtures/fake-provider.sh`, whose screen banner is
`FAKE CLI — NO MODEL CALLS`.

## Isolation

- Tracker: `/tmp/taskai-tab4-home.sie1bW`
- Local repository: `/tmp/taskai-tab4-local.aJ1z7y`
- Remote repository: `/tmp/taskai-tab4-remote.V6cU9S`
- Private tmux server: `taskai-63e67bc5cb4a37f0`
- The private server name was used independently on the local machine and `cruise2`.
- Default local tmux sessions before the journey were `acp-build` (5 windows),
  `buzz-build` (3), `comfyui-indian` (6), and `poly-hedge-fund` (3).
- The default remote tmux server contained only `fizz-history` (2 windows).

The visible build was started with:

```sh
TASKAI_HOME=/tmp/taskai-tab4-home.sie1bW \
TASKAI_CLAUDE_BIN=/tmp/taskai-tab4-fake-provider \
target/release/taskai
```

## Local journey

The New Task dialog accepted a blank Connection and the task rail rendered:

```text
Local persistence
/private/tmp/taskai-tab4-local.aJ1z7y
CONNECTION  <local>
```

TaskAI created one private tmux session for each agent and none for the Task:

```text
s1 0 1
s2 0 1

s1 pid=76130 dead=0 start=/tmp/taskai-tab4-fake-provider
s2 pid=76351 dead=0 start=/tmp/taskai-tab4-fake-provider
```

The selected Subordinate rendered in full-screen and inline focus as:

```text
FAKE CLI — NO MODEL CALLS · s2 · PID 76351
TAB4-LOCAL-DRAFT
```

After `Ctrl+G`, then `q`, TaskAI returned to the shell with:

```text
TaskAI detached — 2 confirmed runtimes continue in tmux. Run taskai to return.
```

While TaskAI was closed, both heartbeat files grew by ten bytes during the
one-second observation window (`575 -> 585` and `452 -> 462`). Both launch
files remained exactly one byte, proving there was one process launch per
agent.

Relaunching with the same tracker showed the same `s2` PID, terminal history,
and unsubmitted `TAB4-LOCAL-DRAFT`; it did not launch a replacement process.
Destroying `s2` then produced:

```text
s1 alive 76130
s2 destroyed 76351
private sessions: s1
```

Destroying the Task removed `s1` and the tracker records. The local private
tmux server then no longer existed. The repository sentinel remained:

```text
b797f3ee18d85ad207f0cad6f4286064d2bd295d935bcf550a7ef687665b204a
```

## `cruise2` SSH journey

The New Task dialog accepted the remote repository and connection as separate
values, and the task rail rendered:

```text
Remote persistence
/tmp/taskai-tab4-remote.V6cU9S
CONNECTION  ssh cruise2
```

The persisted JSON mapping was:

```json
{
  "task_ref": "t2",
  "repo": "/tmp/taskai-tab4-remote.V6cU9S",
  "connection": { "kind": "ssh", "host": "cruise2" }
}
```

The two agent runtime locators independently recorded SSH target `cruise2`,
private server `taskai-63e67bc5cb4a37f0`, and tmux sessions `s3` and `s4`.
Direct inspection on `cruise2` after `q` showed:

```text
s3 pid=2680993 dead=0 start=/tmp/taskai-tab4-fake-provider
s4 pid=2681528 dead=0 start=/tmp/taskai-tab4-fake-provider

FAKE CLI — NO MODEL CALLS · s4 · PID 2681528
TAB4-REMOTE-DRAFT
```

Both remote heartbeat files grew by ten bytes while the local TaskAI process
was closed (`333 -> 343` and `269 -> 279`), while each launch file remained
exactly one byte. Relaunching TaskAI reattached to PID `2681528` with the same
draft visible.

Destroying only `s4` left the Manager alive and mapped:

```text
s3-alive=2680993
s4-destroyed=2681528
remaining private session: s3
```

Destroying the remote Task removed `s3`; the private tmux server then no
longer existed on `cruise2`. The remote repository sentinel had the same hash
as the local sentinel:

```text
b797f3ee18d85ad207f0cad6f4286064d2bd295d935bcf550a7ef687665b204a
```

The final tracker state retained its monotonic counters but no live records:

```json
{"tasks":0,"sessions":0,"next_task":2,"next_session":4}
```

TaskAI's final exit message was:

```text
TaskAI closed — no runtimes are running.
```

## Isolation result

After all scoped destroys, the default local tmux server still contained the
same four sessions with the same window counts, and `cruise2`'s default tmux
server still contained only `fizz-history` with two windows. Neither private
TaskAI tmux server remained. No `kill-server` was used.

## Additional automated SSH proof

The opt-in real-host integration test also passed independently:

```sh
TASKAI_REMOTE_TEST_HOST=cruise2 \
  cargo test -p taskai \
  tmux::tests::configured_ssh_runtime_survives_detach_and_is_destroyed_exactly \
  -- --exact --nocapture
```

It uses a unique remote directory and socket, verifies the original PID and
draft after detach/reattach, destroys the exact mapped session, preserves a
sentinel, and confirms the default remote tmux server is unchanged.

The remote prerequisites also passed directly:

```text
ssh -o BatchMode=yes cruise2 true       -> ok
ssh -o BatchMode=yes cruise2 tmux -V    -> tmux 3.2a
ssh -o BatchMode=yes cruise2 command -v sh -> /usr/bin/sh
```

## Release gate

All checks were rerun after the implementation and manual journey:

```text
cargo fmt --check                                  PASS
cargo clippy --workspace --all-targets -- -D warnings  PASS
cargo test --workspace --locked                    PASS
cargo build --release --locked                     PASS
```

The workspace test run completed 109 non-billing tests successfully. Four
explicitly billed provider-turn proofs remained ignored, as required; they are
not part of this tmux acceptance run.

A final release-binary smoke run in `acp-build:4` rendered `q  Leave` in the
Home keybar, exited normally, and left the tab at its `zsh` prompt. The default
local/remote tmux snapshots still matched the baselines and no uniquely named
remote test directory remained.

Additional deterministic acceptance tests in that total prove:

- one Manager plus two Subordinates create exactly three tmux sessions and no
  Task-level session;
- live Subordinate, Manager-family, and two-Task Destroy scopes kill only their
  resolved runtimes while preserving peer runtimes and both repositories;
- a real TaskAI binary can be `SIGKILL`ed and reopened onto the same tmux PID,
  heartbeat, and unsubmitted draft with no orphan attach client or relaunch;
- injected SSH failures remain errors distinct from missing tmux sessions,
  retain the recorded JSON locator, and never create a local fallback; and
- the explicit `cruise2` run preserves alternate-screen state, exact RGB and
  bold cells, a width-2 Unicode glyph, cursor position/visibility, bracketed
  paste bytes, and resize through three attach/drop cycles that each return to
  zero attached viewers.

## 2026-09-04 installed-build and nested-shortcut regression

The final correction was exercised directly in `acp-build:4` at `364x58`, using the
binary resolved by a plain `taskai` command. `lsof` proved the running executable was
`/Users/trinath/.local/bin/taskai`; the release and installed copies had the same SHA-256:

```text
d35a64415011d7fa08c664a169a3d16b11e671012a8befcb650d4488da467774
```

The stale New Task attempt was recreated exactly:

```text
Name        Common repo task
Repository  ~/github
Connection  ssh cruise2
```

The corrected build resolved that path through the real SSH target and persisted:

```json
{
  "name": "Common repo task",
  "repo": "/home/ubuntu/github",
  "connection": { "kind": "ssh", "host": "cruise2" }
}
```

While the form owned input, its bottom bar showed only `Enter Create / Browse local`,
`Tab Next field`, and `Esc Cancel`; the old Home shortcuts were absent. Injecting physical
`C-f` and `C-g` through the outer tmux left the active field unchanged. The Manager and
Subordinate forms likewise exposed one undimmed, modal-specific bottom keybar.

A local non-billing Task then launched fake Manager `s1` (PID `25311`) and fake
Subordinate `s2` (PID `25951`) as two independent tmux sessions. In the outer tmux pane:

```text
Ctrl+F  navigation -> full-screen CLI
Ctrl+F  full-screen -> inline CLI focus
Ctrl+G  inline focus -> Task navigation
```

The inline CLI accepted the unsubmitted text `nested shortcut verified`. After `q`, PID
`25311` and PID `25951` remained live. Relaunching the installed binary reattached PID
`25311` and rendered the same draft without a replacement launch. That first smoke Task
was then destroyed through TaskAI, proving both processes stopped; its exact temporary
marker files in the working repository were removed.

The immediately inspectable demo used isolated repository
`/private/tmp/taskai-tab4-shortcut-repo.gqJ2Sx`: Manager `s3` was PID `42125` and
Subordinate `s4` initially was PID `42231`. It was left open in `acp-build:4`, inline with
CLI focus and the same visible draft, without writing runtime markers into the source
repository. After visual confirmation, the fake Task was destroyed through TaskAI, the
private tmux server was confirmed absent, and the isolated tracker/repository were moved
to Trash. Tab 4 was then relaunched with plain `taskai`, the real default tracker, and no
fake-provider environment override.

The post-review release gate passed `fmt`, strict workspace Clippy, all 115 non-billing
workspace tests, the release build, and the opt-in `cruise2` lifecycle test. Four billed
provider-turn tests remained intentionally ignored.
