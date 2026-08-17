# Agent Task Console V1 — Verification Plan

The primary Codex session runs this as a distinct verification pass after implementation. No tmux developer/tester workflow is required. Read `agent_task_console_spec.md` and `docs/DEVELOPMENT_PLAN.md`, record exact evidence for every defect, fix it in a focused commit, then rerun the affected checks and the complete final suite.

## T01 — Source, history, and privacy

- Verify the exact SHA and clean worktree.
- Verify `origin` is `yty-build/acp-gateway` and GitHub reports `PRIVATE`.
- Review every milestone commit.
- Search tracked files and history for credentials, auth state, databases, sockets, logs, binaries, coverage, or live worktrees.
- Confirm there are only Task and Session domain entities and no Run, Event, Artifact, workflow, broker, or daemon model.

## T02 — Static and automated verification

Run independently:

```bash
make acceptance
gofmt -l .
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/acpctl
make security-check
```

Inspect the quality and failure sensitivity of tests rather than accepting green output alone.

## T03 — Persistence and CLI behavior

With an isolated `AGENT_HOME`:

- Create/list/show/update Tasks.
- Create a Task with zero Sessions.
- Use `acpctl start` for a one-Session Task.
- Start multiple Sessions under one Task.
- Verify invalid IDs, statuses, profiles, and metadata.
- Restart CLI processes and verify persistence.
- Exercise concurrent creation and migration reopening.

## T04 — Deterministic ACP and worker lifecycle

Using the fake ACP/acpx fixtures:

- Prove worker survival after parent CLI exit.
- Connect multiple viewers.
- Prove one active ACP turn and ordered ACP-to-ACP FIFO prompts.
- Against a responsive mixed-version worker, prove Prompt, permission
  decision, provider-native attach, and live Resume negotiate worker protocol
  v3 plus `exclusive-native-owner-v2`, then fail before any provider command or
  replacement spawn. Verify HTTP writes return sanitized `409` responses with
  code `worker_upgrade_required`, while Cancel, Stop, reads, conversation/history,
  permission listing, and streams remain usable for migration. Cover
  `TestPromptNegotiatesCurrentWorkerBeforeSending`,
  `TestStaleWorkerPermissionDecisionIsNotSent`,
  `TestPendingPermissionSnapshotDisablesStaleWorkerDecisions`,
  `TestResumeLiveWorkerNegotiatesCurrentOrRequiresUpgradeWithoutSpawn`,
  `TestNativeAttachRejectsStaleWorkerBeforeAcquireOrProviderStart`, and
  `TestStaleWorkerProviderWritesReturnConflictWithoutSendingProviderCommand`.
- Verify request IDs and response routing.
- Verify JSONL validity, monotonic positions, replay, and its role as
  lifecycle/audit/provisional-overlay/fallback data rather than committed
  provider conversation history.
- Verify pre-prompt exact ACP rehydration for every request at the FIFO head:
  Refresh or fallback Resume must validate the same identity and emit
  correlated `acp_rehydrated` before `turn_started`; failure sends no prompt.
  Include a provider-store change made outside a gateway-managed native marker.
- Cancel active/idle turns, stop, resume, crash, and recover.
- Exercise malformed/partial adapter output and missing native IDs.
- Verify socket, state, and log permissions.
- Start a custom ACP-compatible command.
- With the public runtime fixture, prove pending permission, exact offered decision, malformed duplicate-kind fail-closed behavior, cancellation, and exact resume identity.

## T05 — Interactive attachment

For `acpctl task attach`:

- Every Task Session appears with correct state and activity.
- A new Session appears without restarting.
- Keyboard and mouse selection work, with a keyboard-complete path that does
  not require a mouse or special font.
- The cohesive two-pane cockpit works at 80x24 and 120x40, collapses to a
  stacked layout at 60x16, and keeps the Session navigator plus core actions at
  the supported 40x16 minimum. Smaller terminals show an actionable warning.
- Status, selection, attention, and focus use words/glyphs as well as color.
- Contextual help and the searchable substring command palette expose the
  shared action registry; unavailable native continuation and invalid current
  actions produce explicit notices.
- Session drill-down and return work.
- Task-level input cannot silently broadcast.
- Detaching does not affect workers.
- Enter opens the selected provider-native harness and `a` opens the structured
  conversation view; neither action silently substitutes the other.
- From the structured conversation view, `n` opens that Session's exact native harness and is
  present in both the contextual footer and help.
- When Task/Session attachment already runs inside tmux, native handoff
  switches to the single stable `agent-<session-id>` harness; it never launches
  a second provider process. `Ctrl-b L` returns to the preserved console and
  `Ctrl-b d` leaves the provider running.
- A responsive worker outside the required protocol/capability contract fails closed before native
  acquisition or provider launch and reports exact stop/resume migration
  commands.
- Once a native harness already exists, missing tmux, noninteractive I/O, or a
  missing stable harness all fail closed; a second outer viewer cannot use
  direct execution, create a replacement provider, or release the live harness.
  An active owner or reservation must also reject ACP admission rather than
  placing the prompt in the ACP FIFO.
- Prove the outer handoff persists `reserved(generation)` before provider start,
  and the hidden/direct provider atomically activates that exact generation.
  A failed or delayed activation must launch no provider process.
- While an ACP request is accepted/queued/preparing/running, prove new native reservation or
  activation is rejected with wait-then-open guidance, not queued; it launches
  no provider process and does not cancel ACP.
- Reproduce an unowned tmux collision, remove the exact colliding harness,
  prove the stale reservation rejects ACP, recover that exact reservation, and
  retry only after the recovery command reports release. Observe transcript
  reconciliation asynchronously. Existing/uncertain tmux, active
  native-harness state, stale generation activation/release, and legacy markers
  all fail closed against both replacement native creation and ACP admission.
  Missing tmux never clears an active native harness. Race a
  reserved-status probe against a safe concurrent release and prove CLI output
  reports `already_released` without instructing the operator to wait for a
  nonexistent ownership release.

For `acpctl session attach --acp`:

- The exact verified provider snapshot loads as committed history; gateway
  JSONL contributes only lifecycle/audit and unmatched provisional overlay;
  live ACP starts after the captured gateway cursor with no gap or duplicate.
- Missing/unreadable provider history on initial load is returned as a bounded,
  disclosed gateway fallback. A transient refresh failure leaves an already-open
  viewer's current in-memory presentation visible with a delayed-refresh notice.
  JSONL is never labelled canonical provider history.
- After reconciliation, provider-store user/reasoning/assistant records are the
  committed semantic authority. Retain distinct ACP-only reasoning as labelled
  supplemental context; if the provider exposes no readable reasoning, retain
  ACP reasoning as the fallback. In a gateway-only snapshot, reasoning remains
  explicitly provisional. Cover this with
  `TestConversationMergeUsesProviderSemanticHistoryWhenBothSourcesContainReasoning`,
  `TestConversationMergeKeepsOnlyGenuinelySupplementalACPReasoning`, and
  `TestConversationMergePreservesACPReasoningWhenProviderHasNoReadableReasoning`.
- Prompts reach the correct Session.
- Busy steering is queued visibly.
- `Esc` cancels the current turn and a follow-up continues the same Session.
- Detach leaves work running.
- A connected original caller receives its correlated response.
- A disconnected caller recovers missed output from its cursor.
- Two viewers can observe safely.
- Raw and normalized views are faithful.
- A permission remains pending across detach; a reattached viewer can decide it.
- Two viewers racing a permission yield one accepted decision and one deterministic conflict.
- `allow_always`/`reject_always` are visibly persistent and require confirmation.
- Settled ACP turns reconcile against the provider store off-screen and publish
  atomically without a blank frame, visible reload, duplicate turn, or content
  jump.
- Live updates and reconciliation preserve scroll anchor/live-follow, focus,
  composer draft/cursor, row/text selection, pending-approval focus, selected
  Session/tool, and expanded reasoning/tool state.
- When scrolled away from the bottom, streaming does not steal the reader's
  position and exposes a deterministic new-activity indicator.

## T06 — Git, review, and native mode

- Create multiple isolated worktrees and branches.
- Verify shared-checkout protection and non-destructive cleanup behavior.
- Verify reviewer Session independence and its producer JSONL, checkout, branch,
  status, and Git-diff evidence.
- Prove the first exact provider identity becomes immutable canonical Session state while acpx/ACP/native IDs remain evidence.
- Test exact Codex/Claude/Grok provider-JSONL and OpenCode-provider-SQLite
  verification/readers plus ambiguous, missing, and checkout-mismatch
  rejection.
- Prove `session attach <id>` and Task Enter invoke the exact real provider-native resume command; `--acp` and Task `a` open the structured gateway console.
- Prove tmux detach/reattach leaves the native process running and does not
  request an early provider-snapshot reload.
- Prove an accepted/queued/preparing/active ACP request rejects creation or activation of a new
  native generation with wait-then-open guidance. It must not queue native
  startup, launch a provider process, or cancel ACP.
- Prove a running or detached provider-native owner rejects ACP before journal
  or queue mutation with exit-and-retry guidance. It must return no accepted
  request ID, send no bridge Prompt, and leave the native process untouched.
- Prove tmux viewer detach leaves both the native process and exclusive owner
  active; reattachment reaches that same process and conversation. Only normal
  provider exit with exact generation/token release permits a cross-surface
  retry.
- After native exit, prove a retried ACP prompt performs exact
  Refresh/fallback Resume and identity validation before `turn_started`, then
  reaches the bridge. A subsequent fresh native generation must use
  `BuildResume` on the same canonical ID and observe the committed ACP turn.
- Prove actual native exit/stop requests exact provider-store reload and
  reconciliation in the background. Require the sanitized
  `provider_transcript_reconciled` lifecycle line after the exact provider read,
  but prove a delayed or failed presentation read does not delay ACP after
  ownership release and exact rehydration; a failed read emits no success line
  and keeps the last validated projection.
- Inject post-native snapshot drift and prove it warns without discarding the
  current projection. Separately inject ACP transport identity drift and prove
  affected requests receive correlated errors, the suspect bridge process
  group exits, and the worker stops for exact resume.
- Start tmux with stale `AGENT_HOME`/`PATH`, then prove native attach passes the invoking state root/environment and rejects an unowned name collision.
- Run the same-conversation ACP → native → rejected ACP while native owns →
  normal native exit → retried ACP → fresh native nonce smoke against all four
  authenticated providers. A separate negative case proves new native start
  during ACP is rejected without queueing or cancellation.

## T07 — API and web

- Exercise every `/api/v1` endpoint and `/healthz`.
- Verify CLI, TUI, API, and web show consistent state.
- Verify Task aggregate SSE and Session replay/reconnect, including the Session `ready` replay/live boundary without gaps or duplicate positions.
- Verify `GET /api/v1/sessions/{id}/conversation` is a bounded, sanitized,
  server-joined provider snapshot plus gateway overlay with an explicit source,
  checkpoint, gateway cursor, committed/provisional flags, and fallback state.
- Prove HTTP/web/mobile clients never receive provider file paths or implement
  provider-file/JSONL joining themselves.
- Verify request/response correlation and invalid cursor handling.
- Verify loopback-only binding and no socket/ACP leakage.
- Verify web prompt/cancel/resume/stop and Session drill-down.
- Verify responsive desktop/mobile ordering and the safe linked/verified conversation indicators without exposing any provider ID.
- Verify semantic user/assistant reconstruction from committed provider history
  plus provisional ACP, cumulative transcript bounds, merged tool/plan updates,
  and diagnostic-only records creating no blank turns.
- Verify each turn shows a compact, sanitized invocation overview: conservative
  ACP/native/unknown surface attribution, lifecycle, completed elapsed time,
  deduplicated
  logical tool count, reasoning availability, and pending approval. Confirm
  output chunks remain coalesced and raw ACP envelopes/opaque IDs remain only in
  bounded local diagnostics.
- Verify reconnect after stop/resume does not pin a stale query cursor, rapid Enter cannot duplicate a prompt, and late replay remains ordered before an optimistic prompt.
- Verify streaming preserves expanded tool/reasoning state, focus, text selection, and a reader's scroll anchor; verify mobile keeps named cancel/resume actions and announces conversation/approval updates.
- Stop the web server and confirm workers continue.

## T08 — Authenticated live provider matrix

Use real `acpx` sessions for `codex`, `claude`, `grok` (resolved through acpx's `grok-build` built-in), `opencode`, and `gemini`.

For every provider:

- Create a Session under one acceptance Task with its own worktree.
- Send the Task-derived initial prompt.
- Require a provider-specific proof file and `go test ./...`.
- Require the provider to commit only its proof change on its Session branch.
- Confirm committed provider history, provisional ACP overlay/stream, explicit
  source state, IDs, status, and response routing.
- Detach the UI while work continues.
- Send a short follow-up prompt.
- Stop and resume the same Session.
- Verify it appears and drills down correctly in `acpctl task attach`.
- Exercise one harmless write permission in `read-approval`: the file must not exist before allow, must appear after allow, and must remain absent after reject.
- Record whether each provider uses `session/request_permission` or the one-shot host broker for `fs/write_text_file`/`terminal/create`; unknown host paths must fail closed.

For Codex, Claude, Grok, and OpenCode, additionally verify the canonical ID in
the provider's exact local store and prove this sequence:

1. During an active ACP turn, attempt to start a new native generation. It must
   fail immediately with wait-then-open guidance, start no provider process,
   and leave ACP running.
2. After ACP settles, prove a fresh real native harness remembers the ACP-only
   nonce; add a native-only nonce, then detach while leaving it idle.
3. Submit an ACP prompt. It must fail immediately with exit-and-retry guidance,
   return no accepted request ID or journal record, and send nothing through ACP
   while the detached native process still owns the Session.
4. Reattach to that exact native harness and exit the provider normally without
   another prompt. Observe exact `native_released` and asynchronous provider
   snapshot reconciliation; viewer detach alone must never produce release.
5. Retry the ACP prompt. Prove exact Refresh/fallback Resume loads the native
   advance, emits `acp_rehydrated` before `turn_started`, and remembers the
   native-only nonce. Then attach again and prove a fresh generation/exact
   provider resume sees the ACP turn. The structured projection eventually
   contains one committed copy of each turn even if display reconciliation
   finishes after ACP dispatch.

Gemini is ACP-only in V1 until an exact provider-store verifier is implemented.

Do not edit authentication configuration. Report an unexpected login prompt as a blocker for that provider.

## T09 — Defect loop and sign-off

For each failure, record the exact command, expected result, actual result, and relevant log. Add a regression test and a focused pushed fix commit. Rerun the affected tests and the complete final suite.

Sign off only with:

- tested SHA,
- commit audit,
- automated test output,
- live provider matrix,
- explicit acceptance checklist,
- remaining limitations,
- clean Git status.
