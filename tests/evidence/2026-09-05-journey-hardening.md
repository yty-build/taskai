# Interactive journey hardening — 2026-09-05

This is isolated QA, not the user's live prototype or model sessions. The real
TaskAI binary ran at 150 × 42 in tmux server `taskai-journey-qa-kpAnBq`, against
`TASKAI_HOME=/tmp/taskai-journey-qa.kpAnBq/state`. Provider processes were explicitly
the repository's non-billing fake fixture. The utility Terminal was a real local
login shell. No model prompts, upgrades, SSH writes, or user task mutations were
performed by this test.

## Reproduced issues and fixes

- A valid task in a plain directory defaulted its first subordinate to a git
  worktree on `main`; submitting a role failed with `fatal: not a git repository`.
  The form now uses Same repository when there is no valid git HEAD. A committed
  repo uses its actual current branch (or HEAD when detached) and an available
  `agent-N` name.
- Worktree names previously flowed directly into a filesystem path. Validation
  now rejects absolute paths, traversal, multi-component names, invalid branches,
  existing folders, and git reference collisions before creating any directory.
- The Other repository field could not be edited or re-browsed after choosing a
  directory. The application now has an editable field and explicit F2 re-browse.
- Mouse clicks on portfolio rows and session navigation were ignored. The
  application now routes them using the renderer's row layout.
- Long input text did not mark clipping on the right. It now shows `›` / `‹`,
  retains the visible caret, and supports conventional word editing.
- Esc from task navigation did not return to the portfolio. It now does.
- `/ Find` was advertised but inactive. Filtering now works; a follow-up QA check
  caught the search field's displayed Clear button not handling clicks, reported
  to the main implementation agent for correction.

## Completed live checks

1. Created a task through the repository-first form in an isolated plain directory.
2. Created a fake Manager and a fake Subordinate; input went directly to the
   selected inline CLI. Typed text survived Ctrl+F fullscreen/inline transitions.
3. Entering a wrong destroy reference kept all three test agent runtimes alive;
   Esc canceled the dialog without mutation.
4. Quit and restarted TaskAI; the original Manager remained PID 32506. A raw SGR
   mouse click on its portfolio row reopened it directly into input focus.
5. After the fix, a second Subordinate created from the plain-directory task
   defaulted to Same repository and launched successfully without extra choices.
6. `s` launched the real task Terminal. `pwd` returned the exact selected temporary
   repository. tmux reported the host cursor visible and native mouse capture off
   while the shell had focus.
7. Ctrl+F round trip and Ctrl+G → s preserved the shell PID 44661 and the unsubmitted
   draft `draft-preserved-qa`.
8. Destroying the Manager closed exactly its three fake agent sessions, while the
   utility shell retained PID 44661 and its draft.
9. Ctrl+G → Esc returned to the portfolio. Find with `NoMatch` showed a truthful
   empty result; Ctrl+U then `Journey` filtered correctly. Enter reopened the
   remembered Terminal selection and its draft.
10. Ctrl+G → q detached the viewer; its utility shell remained alive for the final
    rebuild/reopen check.

## Automated coverage added

`cargo test -p taskai-core --test workspace -- --test-threads=1`: 7 passed.

This covers worktree preview/provision parity, destroy preservation, actual
`trunk` and detached-HEAD defaults, unavailable names (existing branch, branch
prefix, preserved directory), non-git/unborn repositories, no-mutation invalid
names/bases, and preservation of an existing folder sentinel.

`cargo test -p taskai --bin taskai forms::tests -- --test-threads=1`: 6 passed.

This covers padded paste normalization, Unicode-safe insertion/deletion,
select-all replacement, conventional word editing, and exact cell-width rendering
with visible Clear and clipping indicators at widths 30, 98, and 138.

Core clippy with `-D warnings` passed.

## Async-launch follow-up and cleanup

- The missing-repository async error test passed (0.08 s).
- Manager and Subordinate tests now type immediately while their launch workers
  are pending, then poll the actual event-loop delivery path. Both reached the
  native test CLI without an extra focus key (0.98 s combined test).
- The full local utility lifecycle passed with async launch (3.65 s).
- The live rebuilt viewer reopened utility PID 44661 with its saved draft. An
  immediate keystroke during connection was delivered once to that same shell.
- Native `exit` closed the shell and restored navigation. Explicit `s` reopened
  the same mapping as a new shell, PID 54257.
- Task destroy showed the utility in its blast-radius explanation. Typing the
  exact temporary task reference closed that shell; the temporary tracker then
  contained no tasks or sessions, the repository directory remained, and the
  private tmux server had no surviving sessions. The outer viewer exited with q.

SSH async utility testing found an intermittent control-transport failure and did
**not** pass in this run: one attempt returned no viewer after a live runtime was
created; the diagnostic rerun reached real shell input and then failed after
viewer reopen with `Timeout { operation: "inspect", after: 15s }` (32.43 s test).
This evidence was sent to the independent runtime agent for investigation. No
remote provider was started; test cleanup targets only its temporary locator.
