# Agent Task Console V1 — Developer Execution Plan

This plan is executed directly by the primary Codex implementation session. It does not depend on a tmux developer or tester handoff.

Read `agent_task_console_spec.md` completely before editing. The specification is authoritative. Implement the full V1 locally in Go, preserve the Task → Sessions model, and do not add Run, Event, Artifact, workflow, broker, or daemon abstractions.

## Execution rules

1. Work through D01–D14 in order.
2. Add feature tests in the same commit as each feature.
3. Run the relevant tests before every commit.
4. Keep every commit focused and buildable; do not squash or amend completed milestones.
5. Push every green commit to the already-verified private `origin`.
6. Never commit credentials, provider auth state, databases, sockets, logs, binaries, coverage, or live worktrees.
7. Preserve unrelated work and never use destructive Git commands.
8. Do not start web work until the CLI and interactive attachment flow are pleasant and green.
9. After implementation, run a separate evidence-driven verification pass against the test plan before declaring completion.
10. If a dependency or adapter behavior differs from the specification, investigate it, preserve the product contract behind an internal adapter, document the decision, and continue when safely possible.
11. Treat the provider store as committed conversation history, ACP as the live
    provisional stream, and Session JSONL as lifecycle/audit/overlay/fallback.
    No transport may silently promote JSONL into the canonical provider
    transcript.
12. Keep exactly Task and Session as persistent product entities. Provider
    snapshots, ACP overlays, provider-write ownership, reconciliation
    boundaries, and projection revisions remain operational/in-memory state.

## Ordered conversation-continuity and UX work

The D01–D14 milestone order remains authoritative. Within it, implement the
approved source and seamless-view contract in this dependency order:

1. **D05 — ACP live contract:** normalize request-correlated ACP activity as a
   provisional stream and keep every `acpx` assumption inside `internal/acp`.
   Do not treat ACP replay as the provider's committed transcript.
2. **D06 — journal and write serialization:** make Session JSONL the monotonic
   lifecycle/audit/overlay/fallback journal and add one exclusive provider-write
   gate per Session. ACP turns and a provider-native process never write the
   same conversation concurrently.
3. **D08 — source-agnostic cockpit:** build the Go/Bubble Tea Task cockpit and
   Structured ACP console against a semantic conversation projection. Exercise
   provider-snapshot and JSONL-fallback fixtures even before live provider-store
   adapters arrive.
4. **D10 — committed history and reconciliation:** add verified read-only
   provider-store adapters for Codex, Claude, Grok, and OpenCode; load the exact
   canonical conversation; join it with the gateway overlay; and reconcile
   after ACP and native turns before the other writer continues.
5. **D12 — shared projection service:** make CLI, TUI, API, and web consume the
   same initial snapshot, fallback state, live cursor, and atomic replacement
   contract. Clients never open provider files or merge JSONL themselves.
6. **D13 — seamless browser/mobile projection:** apply live provisional frames
   and background replacements with stable keys while preserving reader and
   composer state.
7. **D14 — cross-provider acceptance:** run the full source/fallback,
   exclusivity, reconciliation, state-preservation, terminal-compatibility, and
   no-flicker matrix before declaring the build complete.

## D01 — Private Go project bootstrap

- Verify `origin` is `yty-build/acp-gateway` and is private.
- Install Go with Homebrew if it is unavailable.
- Install `acpx@0.13.0` beneath `~/.local` without `sudo` if it is unavailable. Use a user-owned npm cache if the default cache has ownership problems.
- Create the Go module `github.com/yty-build/acp-gateway`.
- Add `.gitignore`, `AGENTS.md`, a README scaffold, and build/test/format/vet/race targets.
- Add `cmd/acpctl` with version, help, stable error output, and exit-code handling.
- Add `AGENT_HOME`, defaulting to `~/.agent`, and create state directories with user-only permissions.
- Prefer standard-library CLI dispatch and avoid an unnecessary command framework.

Commit: `chore: bootstrap the private Go project`

## D02 — Task and Session persistence

- Use a pure-Go SQLite driver and embedded transactional migrations.
- Implement exactly the `tasks` and `sessions` tables from the spec.
- Enable foreign keys, busy timeout, and safe local concurrency.
- Implement repositories, UTC timestamps, status validation, metadata JSON validation, and readable collision-resistant Task/Session IDs.
- Cover fresh database, reopening, idempotent migration, foreign key, invalid state, and concurrent creation behavior.

Commit: `feat: add task and session persistence`

## D03 — Task management

- Implement `acpctl task new`, `acpctl tasks`, `acpctl task <id>`, and `acpctl task status`.
- Implement `acpctl start` as atomic convenience for creating a Task and its first Session without changing the persisted relationship.
- Include descriptions, external references, Session summaries, useful errors, and stable human-readable output.
- Add isolated CLI integration and output tests.

Commit: `feat: add task management commands`

## D04 — Declarative agent registry

- Load embedded default YAML profiles and overlay `~/.agent/agents/*.yaml`.
- Support only name, provider, command, args, and environment in V1.
- Provide profiles for Codex, Claude, Gemini, Grok (`grok`, with `grok-build` compatibility), and OpenCode.
- Support raw custom ACP-compatible commands.
- Validate names and commands and never use shell interpolation.
- Provide a small list/diagnostic command.

Commit: `feat: add declarative agent registry`

## D05 — ACP adapter

- Put every `acpx` invocation and output assumption in `internal/acp`.
- Pin and probe `acpx@0.13.0` compatibility.
- Run one long-lived Node bridge per Session worker through the public `acpx/runtime` export; do not use the detached `acpx prompt` queue owner or a PTY.
- Implement session create/ensure, prompt, status, cancel, reconnect/resume, metadata inspection, and interactive `session/request_permission` decisions.
- Stream normalized runtime events and preserve the complete gateway event payload. Document that the public runtime does not expose an exact raw JSON-RPC packet stream.
- Mark ACP conversation activity as the live provisional source in the internal
  projection contract. It may render immediately but is not the canonical
  committed transcript once an equivalent provider-store turn exists.
- Persist Task Console and acpx record IDs distinctly from one canonical provider conversation ID. Retain ACP/native source IDs only as adapter evidence and migration compatibility.
- Assign a request ID to every prompt and preserve it through streamed output.
- Keep runtime state/bridge source private, keep provider auth out of persisted options, bound bridge frames and diagnostics, and fail closed on malformed permission options.
- Build a deterministic fake-`acpx/runtime` fixture for success, cancellation,
  exact resume, permissions, duplicate option kinds, reconnection, live chunks
  that precede provider commit, and settled-turn refresh signals. Retain
  CLI-adapter fixtures for the legacy compatibility unit tests.
- Run a focused live compatibility spike before locking the subprocess/queue behavior.

Commit: `feat: add the acpx execution adapter`

## D06 — Durable Session workers

- Implement hidden `acpctl _worker --session <id>`.
- Detach it so it survives the parent CLI.
- Enforce one worker per active Session.
- Implement the NDJSON Unix socket protocol from the spec with user-only permissions.
- Support multiple simultaneous subscribers.
- Permit one active ACP turn and a serialized prompt queue.
- Broadcast request-correlated activity to all viewers.
- Append monotonically positioned JSONL lifecycle/audit history and retain the
  complete ACP material needed for provisional overlay, missed-stream recovery,
  and explicit provider-history fallback.
- Handle signals, stale PID/socket state, duplicate workers, and crash recovery.
- Keep work independent of disposable viewers, but serialize provider writers.
  Add one per-Session operational write gate: an ACP turn owns it while sending;
  native attach waits/fails without cancelling that turn; a running native
  process owns it until process exit/stop; ACP prompts submitted meanwhile are
  visibly queued and never reach the provider concurrently.
- Persist enough ownership metadata for crash recovery without introducing a
  new product entity or table. A tmux viewer detach must not falsely release a
  still-running native owner.
- Test FIFO acquisition, wait/fail behavior, prompt queueing behind native,
  native reattach, stale pre-launch reservation recovery, fail-closed
  active-owner crash preservation, and proof that two surfaces never dispatch
  provider input concurrently. Missing tmux must never clear active ownership.

Commit: `feat: add durable session workers`

## D07 — Session lifecycle controls

- Implement `acpctl session start`, detail, prompt, cancel, resume, stop, and logs.
- Implement `acpctl task logs` aggregation.
- Starting a Session must send an initial prompt containing Task title, description, role, and repo/worktree context; `--prompt` appends Session-specific instructions.
- Default to read approval. Require an explicit choice such as `--approve-all` for unattended writes and persist it in `metadata_json`.
- `logs --follow` must support normalized and `--raw` gateway output and fall
  back to Session JSONL when the worker is unavailable. Label this as gateway
  lifecycle/audit history rather than the canonical provider transcript.
- Every normal CLI invocation must reconcile stale workers.

Commit: `feat: add session lifecycle controls`

## D08 — Interactive Task and Session attachment

- Implement `acpctl task attach <task-id>` and `acpctl session attach <session-id>`.
- Keep the terminal implementation in Go with Bubble Tea and centralize
  ANSI-aware layout, semantic styles, pointer geometry, help, and composer
  behavior. Do not combine a Go baseline or Bubble Tea major-version migration
  with this redesign.
- Build visual-contract fixtures at 40x16, 60x16, 80x24, and 120x40. Assert
  ANSI-aware width, keyboard-only operation, and no click-through overlays.
- Make Task attachment a polished developer cockpit. At normal widths use one
  cohesive two-pane surface with a scannable Session navigator, single shared
  seam, larger selected-Session preview, slim Task status/title header, and
  contextual status/footer. Use a high-contrast focus marker and semantic
  status glyph+word; prioritize Task title, agent/role, latest activity,
  checkout, and canonical-conversation readiness over opaque IDs.
- Use a stacked navigator/preview on narrow terminals and keep the Session
  navigator plus core actions at the supported 40x16 minimum. Smaller terminals
  receive an actionable warning while Session work continues. Mouse is optional
  convenience, never the only path. Do not require truecolor or Nerd Fonts.
- Keep one action registry for direct keys, footer, help, filtering, and a
  searchable substring command palette. Label unavailable native continuation
  and return an actionable notice for an invalid current action. `Enter` opens
  the exact real provider-native harness; `a` opens Structured ACP; `p` targets
  an ACP message.
- Make Structured ACP conversation-first: compact identity/status line,
  transcript on the terminal background, distinct user/agent gutters, inline
  reasoning/tools, compact tool status/previews with `d` detail disclosure,
  latest-plan replacement without duplicate updates, prominent permission
  shelf, and a wrapping two-line composer. Do not imitate provider-native
  harness features.
- New Sessions must appear without restarting the console.
- Keyboard and mouse selection drill into a Session and allow return to the
  Task view with selection/filter/scroll state restored after native handoff.
- Consume a semantic Session projection: provider snapshot as committed base,
  gateway lifecycle/uncommitted ACP as overlay, then live provisional ACP from
  a captured cursor. Before D10 adapters exist, use deterministic provider-base
  fixtures and explicit JSONL fallback rather than hard-wiring the UI to log
  files.
- Follow live output only while at the bottom. Scrolling away freezes the
  viewport and shows `N new`/return-to-live status instead of moving content
  under the reader. Coalesce chunk redraws without delaying meaningful state.
- Render messages, tools, commands, results, diffs, permissions, errors, and provider-exposed reasoning in event order; identify the completed response without claiming a protocol-level final-answer channel, and provide a raw toggle.
- Number exact provider-offered permission choices, distinguish persistent decisions, and keep a permission actionable across detach/reattach.
- A prompt submitted while an ACP turn or native process owns the provider-write
  gate is visibly queued and must not dispatch concurrently.
- `Esc` requests cancellation of the current turn only in the specified
  non-composer focus; it is not true suspension. Composer/overlay Escape keeps
  its contextual dismiss/back behavior.
- Detach closes only the viewer and never affects the worker or turn.
- Reconnect from the last stream position and recover missed correlated output.
  Provider reconciliation must preserve selected Session/tool, scroll anchor
  and live-follow state, focus, composer draft/cursor, selection, pending
  approval focus, and expanded reasoning/tool state.
- Multiple viewers must work safely.

Commit: `feat: add interactive task and session consoles`

## D09 — Git and worktree isolation

- Validate Git repositories.
- Implement optional `--worktree` using `~/.agent/worktrees/<task>-<session>` and `agent/<task>/<session>` naming.
- Reject accidental concurrent writers in the same checkout unless explicitly requested.
- Persist repo/worktree/branch and never automatically delete user worktrees or branches.
- Test with disposable Git repositories.

Commit: `feat: add isolated git worktrees`

## D10 — Canonical provider-native attachment

- Make `acpctl session attach <id>` open the real provider-native harness by default; use `--acp` for the structured gateway console and keep `--native` only as a compatibility alias.
- Resolve the exact canonical provider conversation ID from the Session. Never select a conversation by recency or create a replacement conversation as an attach fallback.
- Implement read-only provider-store adapters and deterministic fixtures for
  Codex, Claude, Grok, and OpenCode. Verify the exact canonical ID and
  repo/worktree before reading or launching; treat each format as
  provider-specific and project only allowlisted user-visible history.
- Define the shared conversation reducer: normalized provider snapshot is the
  committed base; gateway-only lifecycle/permission/audit state and ACP
  activity not yet represented in that snapshot form a keyed overlay. Never
  expose provider auth, hidden internal state, unrelated conversations, store
  paths, or canonical provider IDs through the browser projection.
- Implement initial load as one consistency boundary: load/validate a bounded
  provider snapshot, derive the JSONL overlay and returned gateway cursor,
  reduce off-screen, publish once, then follow from that cursor without gaps or
  duplicates.
- Implement explicit nonfatal source states: live ACP before provider commit
  and bounded gateway-history fallback when provider snapshot is unavailable.
  On transient refresh failure, an already-open viewer keeps its current
  in-memory presentation and shows a delayed-refresh warning. A Session with no
  conversation activity uses a specific empty state. Gemini and custom ACP
  commands use disclosed gateway fallback until they have a verified
  provider-store adapter; never guess a store path or reuse another provider's
  parser.
- Launch the documented provider resume command in the correct repo/worktree, preferably in a tmux-backed native harness so a terminal detach does not end the provider process.
- Acquire the exclusive per-Session provider-write gate before native launch.
  Wait in serialization order or fail visibly while ACP owns it; never cancel
  the turn. Hold ownership for the native process lifetime, including viewer
  detach/reattach, and through post-exit exact refresh plus reconciliation.
  Keep later ACP prompts visibly queued without dispatch; release native
  ownership only after the reconciled handoff is safe.
- After every settled ACP turn and after native process exit/stop, load a fresh
  provider snapshot in the background, merge still-uncommitted/gateway-only
  overlay records, validate the complete candidate, and atomically publish it.
  Same-transport ACP work may continue while a display refresh retries, but an
  ACP-to-native or native-to-ACP ownership handoff must not cross an
  unreconciled provider-write boundary.
- Never remove a live ACP item until its equivalent provider item exists in the
  candidate snapshot. On malformed/partial/wrong-conversation/wrong-checkout
  input, leave an already-open viewer's current projection plus live overlay in
  place and surface a bounded delayed-refresh diagnostic; never blank or
  partially swap the view.
- Test exact provider argv and store verification; provider snapshot + overlay
  deduplication; turn/tool/reasoning ordering; first-turn-before-commit;
  malformed/partial stores; fallback disclosures; native detach/reattach,
  recoverable pre-launch collision reservations, and fail-closed active-owner
  loss; ACP → native → ACP continuity; and atomic no-blank/no-duplicate
  replacement for Codex, Claude, Grok, and OpenCode.

Commit: `feat: make native harness the default session attachment`

## D11 — Independent review Sessions

- Implement `acpctl session review <producer> --agent <reviewer>`.
- Create a new reviewer Session under the same Task.
- Include original Task, producer ID, JSONL path as read-only gateway audit
  evidence, repo/worktree, branch/diff, and independent verification
  instructions. Do not describe JSONL as the provider's committed transcript.
- Include the suspicious-behavior checklist from the spec.
- Do not import the producer conversation into the reviewer conversation.
- Do not begin against an actively changing producer checkout without an explicit safe state.

Commit: `feat: add independent agent review sessions`

## D12 — Shared service and versioned API boundary

- Define shared TaskService, SessionService, and SessionStream behavior used by CLI, TUI, and HTTP.
- Add one shared Session conversation-projection read contract returning the
  validated provider snapshot + derived gateway overlay, source/fallback state,
  stable presentation keys, captured live cursor, and opaque projection
  revision. Projection revisions are transport metadata, not persistent Events.
- Keep provider-file parsing, JSONL joining, deduplication, and reconciliation
  below the service boundary. CLI, TUI, HTTP, web, and future mobile clients
  must not independently merge sources.
- Implement all `/api/v1` routes and `/healthz` from the spec.
- Use stable API DTOs rather than SQLite rows.
- Prompt submission returns a request ID.
- Task streams aggregate every Session.
- Session and Task SSE support cursor/`Last-Event-ID` replay.
- Stream provisional ACP activity and publish successful reconciliation as one
  atomic semantic replacement or stable-keyed equivalent. A failed refresh
  leaves an already-open viewer's current in-memory presentation intact and
  updates its bounded warning state.
- Keep raw ACP and Unix socket details below the boundary.
- Bind only to loopback in V1 and reject unsupported public exposure.

Commit: `feat: expose the versioned task and session API`

## D13 — Local web UI

- Implement `acpctl serve` with Go `net/http`, server-rendered HTML, minimal JavaScript, and SSE.
- Add Tasks, Task detail, clickable Session detail, reconciled provider-history
  snapshot + gateway overlay/fallback + live provisional ACP conversation,
  prompt/cancel/resume/stop, review, and bounded sanitized diagnostics.
- Make desktop and mobile layouts responsive structured ACP views of the same canonical provider conversation: persistent navigation on desktop; an accessible drawer, sticky composer, pinned approvals, and retained response controls on mobile.
- Coalesce streamed assistant output, merge tool updates, and key semantic
  blocks independently of array position. Apply a validated background
  reconciliation without clearing/reloading the page or duplicating a turn.
- Preserve stable-block scroll anchor and live-follow mode, navigation and tool
  selection, keyboard focus, composer draft/cursor, DOM text selection,
  pending-approval focus, and expanded reasoning/tool state across live frames
  and atomic projection replacement.
- Show concise `live before provider commit`, `gateway history fallback`, and
  `provider history refresh delayed` disclosures while leaving the transcript usable.
  Enforce cumulative browser-state limits for provider and gateway sources.
- Expose only safe linked/verified continuity indicators; never send provider conversation identifiers to the browser.
- Use `/api/v1` and shared services; do not duplicate business logic.
- Prove stopping the web server does not affect workers.

Commit: `feat: add the local task console web UI`

## D14 — Hardening and documentation

- Complete README, installation, agent profiles, architecture, API, security, permissions, troubleshooting, and public-service boundary documentation.
- Document the three conversation sources, exclusive provider-write ownership,
  fallback disclosures, and the difference between native committed history,
  live provisional ACP, and raw gateway JSONL.
- Use `acpctl` as the only executable/usage name; never install or overwrite a global `agent` command.
- Add deterministic end-to-end acceptance automation and map it to the spec checklist.
- Run the mandatory projection/UX matrix below with fixtures first and fresh
  authenticated provider Sessions second. Capture source counts, conversation
  IDs only in private test evidence, screen/API assertions, and terminal/web
  state-preservation evidence without committing live stores or logs.
- Add build/version metadata and secret/runtime-file checks.
- Run `make fmt-check`, `make test`, `make race`, `make vet`, `make build`,
  `make security-check`, and `make acceptance`.
- Ensure `git status --short` is empty, push the final commit, and report all SHAs, test output, and limitations.

Commit: `docs: complete V1 operations and acceptance guide`

## Mandatory projection and terminal test matrix

Run these in milestone order and repeat the cross-surface cases during D14:

1. **Reducer unit tests:** provider-only history; ACP-only first turn; provider
   base + nonoverlapping overlay; the same turn in both sources; gateway-only
   permission/lifecycle facts; reasoning → assistant → tool → reasoning →
   assistant ordering; and large bounded histories whose recent/final tail is
   retained.
2. **Provider adapter fixtures:** exact canonical ID and checkout for Codex,
   Claude, Grok, and OpenCode; reject newest/wrong conversation, wrong checkout,
   truncated/partially-written file, unknown record shapes, and hidden/auth
   fields. A failed refresh leaves an already-open viewer's current in-memory
   presentation intact.
3. **Load-boundary tests:** inject an ACP record between provider/JSONL reads
   and subscription; assert captured-cursor replay yields neither gap nor
   duplicate. Assert source/fallback labels for pre-commit, provider unavailable,
   delayed refresh, and an empty new Session.
4. **Writer exclusivity tests:** active ACP makes native wait/fail without
   cancellation; native process makes ACP prompts visibly queue; tmux viewer
   detach retains native ownership; process exit/stop reconciles before queued
   ACP dispatch; crash recovery cannot create two writers.
5. **Atomic reconciliation tests:** show live ACP chunks, commit the equivalent
   provider turn, refresh in the background, and assert one continuous visible
   turn with no intermediate empty frame, duplicate block, lost final response,
   or reordered tool/reasoning content.
6. **Viewer-state tests:** during live update and reconciliation preserve Task
   Session selection/filter, transcript stable-block anchor, follow/not-follow
   mode, selected tool/row, focus, composer draft/cursor, text selection,
   pending approval focus, and each reasoning/tool disclosure state in terminal
   and browser.
7. **Terminal cockpit visual contracts:** empty, many Sessions,
   active/waiting/error, pending approval, long transcript, fallback, and the
   searchable action palette at 40x16, 60x16, 80x24, and 120x40; ANSI-aware
   width, keyboard-only operation, and mouse selection paths.
8. **Compatibility smoke:** Terminal.app, iTerm2, VS Code terminal, tmux, and an
   SSH session; resize, paste/multiline input, native handoff/return, copy/search
   escape hatch, viewer detach, and no worker/turn loss.
9. **Fresh live continuity:** for each authenticated Codex, Claude, Grok, and
   OpenCode Session, prove ACP prompt/stream → provider-store commit → seamless
   refresh → native continuation → native exit/reconcile → queued ACP
   continuation, always on the exact canonical conversation.

## Post-V1 terminal refinements

These are useful follow-ups, not V1 release claims or acceptance gates:

- explicit `NO_COLOR`, 16-color, truecolor, and ASCII-fallback golden suites;
- fuzzy-ranked command search and state-filtered palette contents;
- a compact list/detail toggle below the current 40x16 minimum;
- transcript export to native scrollback or a configured pager/editor; and
- a direct reviewer-creation action inside the Task cockpit.
