# Product hardening — 2026-09-05

Three parallel development/QA agents reviewed rendering, runtime reliability, and
the complete interactive journey. The main agent integrated application changes
and release acceptance. Existing user tasks, repositories, provider sessions,
ACP integrations, and the private tmux runtime architecture remain in scope and
preserved. No model turn or software upgrade was requested for verification.

## Acceptance gates and fixes

| Area | Required behavior and regression coverage |
| --- | --- |
| Navigation | Click task/team/Terminal, Enter inline input, Ctrl+F fullscreen, Ctrl+G navigation, Esc/G Home; remember selected task member. |
| Long lists | Sweep every selection of 40 tasks and 30 team entries at 110×30, 150×42, 209×51; selected entry, shortcuts and hit-testing stay aligned. |
| Search | `/` filters real task refs by name/path/connection; typing, paste, Ctrl+U, clickable Clear, Esc, and filtered selection work. |
| Forms | Broad centered fields; mouse caret/Clear, normal and word editing, Unicode physical widths, visible clipping, Tab navigation, explicit F2 browsing. |
| Subordinates | Actual branch/HEAD and unused worktree names; non-Git/unborn tasks default to Same repository; Other repository editable and re-browsable; validation precedes filesystem mutation. |
| CLI fidelity | Cursor visibility/position, truecolor, inverse/underline/italic, wide/combining cells, paste and terminal resize survive nesting. |
| Responsiveness | CLI launch, remote readiness, disconnect inspection and tmux resize run off the input/render thread. Exact mappings persist before launch. Input queued during launch is delivered to that session. |
| Destruction | Stable task/session ref confirmation; pending launches guard their exact blast scope; wrong confirmation/Esc do not mutate. Manager destruction leaves Terminal; task destruction closes it. |
| Persistence | Shell/provider PID and draft survive layout changes and viewer quit/relaunch. Native shell exit differs from viewer disconnect. Missing paths never fall back to a different repository. |
| Honest UI | No generated activity sparklines in live portfolio; no advertised ACP visualization for native cells; runtime labels distinguish running, connecting, exited and unavailable. |
| Terminal recovery | Compact view ignores unsafe click/mutation paths; q/Ctrl+G remain safe. RAII restores raw mode, cursor, mouse, alternate screen, wrap and synchronized-update modes on exit/error. |

## Test reliability findings

The old default-parallel suite was not merely slow:

- Two tests mutated `TASKAI_CLAUDE_BIN` concurrently, allowing the durable-runtime
  test to launch a different test's executable. A narrow environment mutex now
  protects only those fixtures; the whole suite is not serialized.
- Every app unit fixture spawned four real provider version checks, even for
  form-only tests. The injected unit constructor now omits those probes. Normal
  application boot and real connector/binary integration probes are unchanged.
- Fixed-sleep provider-exit coverage became a readiness/exit handshake.
- The startup settle deadline wrongly included SSH inspection time; it now
  requires two confirmed-live observations separated by 250 ms instead.
- Control commands have bounded time/output and distinguish process timeout
  from unclosed output pipes. Exact isolated fixture cleanup is registered before
  launch; four leaked fixture sessions from failing test runs were closed without
  touching user runtimes or repository files.

Following environment/probe isolation, the app suite passed in default-parallel
mode: 58 passed, 0 failed, 2 explicitly ignored. The ignored tests are separately
selected SSH acceptance checks, not billed model turns.

See [interactive journey evidence](2026-09-05-journey-hardening.md) for the
independent live keyboard/mouse/destroy/persistence walkthrough and exact fixture
cleanup. Billed provider proofs remain separate and were not run.

## Installed release acceptance

- `cargo test --workspace --no-fail-fast`: 165 passed, 0 failed, 6 ignored in
  the default parallel configuration. The six opt-ins are two SSH checks and four
  billed provider proofs; SSH checks are selected separately below.
- `cargo clippy --workspace --all-targets -- -D warnings`, `cargo fmt --all --check`,
  `git diff --check`, and `cargo build --release`: pass.
- `cargo test -p taskai cruise2_directory_picker_reads_real_folders -- --ignored
  --nocapture`: pass, 10.06 s, actual remote directories, read-only.
- `cargo test -p taskai --bin taskai app::tests::remote_utility_terminal_lifecycle
  -- --ignored --nocapture --test-threads=1`: two consecutive final passes,
  42.79 s and 60.88 s, against `ssh cruise4b`. Real shell command delivery,
  cursor/focus/layout, draft/PID survival, manager-only destroy isolation, shell
  exit/reopen, and task destroy with a peer runtime preserved are covered.
  The earlier final-run attempt failed at the shell-exit assertion after 206.74 s.
  Its test loop used 100 potentially slow SSH polls and did not pump pending input
  like production. The corrected test pumps `poll_launches`, uses a 20 s wait
  deadline (individual SSH operations remain separately bounded), and captures
  native tmux output and focus/input state on failure before fixture cleanup.
  No production exit logic was changed to make these final two passes succeed.
- Release SHA-256:
  `5c3468773d70d23f73a2560c3bb987eb06a77879cf241c7e6e319a3ac9af97f1`.
  Installed at `/Users/trinath/.local/bin/taskai` and launched by absolute path in
  `acp-build:4.0` / pane `%113`, 209×51. Prior binary retained as
  `/Users/trinath/.local/bin/taskai.before-smoothness-20260905`.

Actual user-state acceptance, without creating agents or submitting model turns:

- Quit the old viewer, relaunch, search `SMB`, and open `SMB_Deal_flow` directly
  into its existing SSH Terminal. The prompt and cursor render immediately after
  the background attach completes; the old blank disconnected view is gone.
- `Ctrl+F`: inline → fullscreen → inline, native cursor visible in both layouts.
  Captured outer cursor positions were `(39,7)` inline and `(3,5)` fullscreen.
- `Ctrl+G`: navigation, cursor hidden; `Esc`: portfolio; `Enter`: same Terminal
  inline with focus restored. Existing terminal input was not submitted or cleared.
- Local Claude PID `19966`, local Codex PID `41491`, and remote shell PID
  `1358854` are unchanged before/after viewer replacement. Their private server
  remains `taskai-17ace2a775c719e3`; registry still contains exactly tasks `t1`,
  `t2`, agent refs `s1`, `s2`, and the existing `t2-terminal` mapping.
- The user had an unsubmitted Add Manager form (`aaa`, Codex). That form was
  restored after acceptance and left unsubmitted; no extra manager was launched.

Saved-runtime inspections now also run in the background at application startup;
the portfolio does not wait for SSH. The durable-runtime reopen regression waits
for this reconciliation instead of asserting a synchronous constructor result.
