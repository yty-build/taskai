# Direct CLI focus — 2026-09-05

## Contract

Opening a task, successfully creating a Manager/Subordinate, or bootstrapping a
Lead with a repository argument immediately focuses the inline native CLI.
No additional fullscreen/focus key is required. Ctrl+G explicitly returns to
navigation; Enter there focuses the selected CLI inline. Ctrl+F continues to
toggle fullscreen. Empty tasks and failed launches stay safely in navigation.

## Tests

- Binary suite: 39 passed, one gated SSH directory test not invoked this run.
- TUI suite, including input, focus, geometry, and golden parity: passed.
- Clippy with `-D warnings`, release build, and `git diff --check`: passed.
- New integration test creates a Manager and Subordinate using a deterministic
  provider in a temporary repository/private tmux namespace. Both accept draft
  input immediately and expose a visible mapped cursor, without a focus key.
- Opening an existing task reuses its pane, accepts draft input immediately, and
  maps the cursor; Ctrl+G hides it in navigation. Empty/failure cases are tested.
- Updated navigation-footer golden fixtures intentionally to advertise Enter Type.

## Live tab 4

The reported problem was reproduced: the remote Claude child reported cursor
visible=1 at column 2, row 15, while TaskAI remained in navigation with host
cursor visible=0.

Installed/release SHA-256:
`569f7b8abcae99878fbde0e5f61ce54dc5336acd06cacde6cf465f7baacc6219`.

After reopening TaskAI, selected SMB_Deal_flow from Portfolio and pressed Enter.
The task opened inline with `CLI HAS FOCUS`, no fullscreen action. Host cursor was
visible=1 at column 38, row 21: child (2,15) plus the correct pane offset (36,6).
Ctrl+G restored navigation, hid the host cursor, and displayed Enter Type in the
footer. Enter returned to inline focus.

The existing remote process on cruise4b was preserved: private tmux server
taskai-17ace2a775c719e3, session s3, PID 1340039. No user task or provider was
destroyed. No prompt was submitted to a real model; input-forwarding tests used
only the isolated deterministic provider. Tab 4 remains on the user's real Claude.
