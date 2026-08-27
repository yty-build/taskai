# taskai — terminal UX, UI, and product verification plan

**Status:** Worker-ready verification contract v1.0
**Date:** 2026-08-26
**Product contract:** `docs/UX_PRODUCT_VISION.md`
**Build contract:** `build_plan.md`
**Surface under test:** The `taskai` terminal application and its ACP-facing runtime boundaries

## 1. Purpose

This plan makes the approved user experience executable. A worker cannot call a screen complete because it renders once on one machine. Every TaskAI surface must be proven through deterministic state tests, terminal rendering tests, keyboard and mouse interaction tests, pseudo-terminal acceptance, connector contracts, and storage audits.

The quality bar is:

> A user can always understand where they are, what is running, which manager owns each subordinate, and what action is available—without TaskAI inventing provider behavior or retaining conversation content.

This document specifies verification, not a new product layer. If it conflicts with `docs/UX_PRODUCT_VISION.md`, the vision wins. If a test would require MCP, tmux, provider-native terminal capture, a transcript database, or semantic interpretation of agent output, reject the test and the implementation it implies.

## 2. Verification principles

1. Test user-observable behavior before implementation detail.
2. Pair every visual snapshot with semantic assertions so a plausible-looking but unusable screen cannot pass.
3. Exercise every displayed action through the keyboard and, where the terminal supports pointer input, through the mouse.
4. Test TaskAI chrome exactly; treat provider-owned authentication UI and arbitrary ACP content as opaque boundaries.
5. Use deterministic fake ACP agents for CI. Real provider checks are explicit, local, opt-in smoke tests.
6. Test absence as a first-class requirement: no surprise login, no direct subordinate prompt, no sticky Follow, no automatic replay, and no persisted content.
7. Do not normalize provider meaning for easier tests. Unknown public ACP content must remain unknown and render through the bounded generic path.
8. Do not make timing tests depend on internet or model latency. Measure only the time between a local fake ACP frame becoming available and TaskAI rendering it.
9. Never commit credentials, real provider references, conversation content, terminal captures containing sensitive data, or generated TaskAI state.

## 3. Test layers

| Layer | Purpose | Required on every change |
| --- | --- | --- |
| State/model tests | Prove navigation, focus, selection, hierarchy, mode, and error transitions | Yes for affected behavior |
| Renderer tests | Prove TaskAI regions, labels, clipping, and responsive behavior from deterministic models | Yes for affected surface |
| Golden terminal tests | Catch visual regressions in representative states and sizes | Yes for affected surface |
| Input tests | Prove keyboard, mouse, cancel, back, and focus behavior | Yes for affected action |
| ACP contract tests | Prove manager, subordinate, Answer, Follow, Replay, Resume, and capability truth | Yes for affected connector/runtime |
| Fake-provider integration | Prove multiple components and concurrent Sessions without external services | Yes |
| PTY acceptance | Prove the compiled `taskai` binary as a user experiences it | Yes |
| Storage/security audit | Prove metadata-only persistence and safe terminal/process handling | Yes |
| Real-provider smoke | Confirm claims against authenticated connectors | Before advertising a capability |

Snapshots alone are insufficient. A golden can pass while focus is trapped, a button is unreachable, or the selected Session targets the wrong manager.

## 4. Required deterministic test harness

Build the harness under `internal/testkit` and keep it out of production composition.

### 4.1 Fake ACP fixtures

Provide programmable fixtures for:

- a dual-role manager connector;
- a subordinate-only connector;
- a connector with and without exact Resume;
- a connector with and without bounded Replay;
- connected, disconnected, needs-repair, missing-executable, and authentication-expired states;
- successful, refused, cancelled, token-exhausted, malformed, oversized, and transport-failed turns;
- delayed and concurrent completions;
- known and unknown public ACP frame shapes.

The fake manager—not TaskAI production code—decides when to create a subordinate, what assignment to send, and whether to use Answer, Follow, or Replay. Tests must fail if TaskAI parses manager prose to cause delegation.

### 4.2 Deterministic environment

Tests use:

- injected clocks;
- stable Task and Session IDs;
- fixed locale and timezone;
- controlled terminal capability flags;
- deterministic provider names, labels, and frame sequences;
- isolated temporary state roots with restrictive permissions;
- no network and no real provider credentials.

Normalize only unstable test metadata such as temporary paths, generated IDs, and timestamps. Never normalize away selected rows, lifecycle state, provider identity, Answer/Follow state, error text, or ordering.

### 4.3 Terminal driver

The PTY driver must be able to:

- launch the compiled `taskai` binary;
- send normal keys, navigation keys, Enter, Space, Escape, Backspace, and the implemented TaskAI shortcuts;
- send supported mouse click and scroll events;
- resize while idle and while ACP frames are arriving;
- capture the visible cell grid and plain-text semantic regions;
- wait on application state or a rendered marker instead of arbitrary sleeps;
- terminate only the viewer in detach tests while leaving runtime processes observable.

### 4.4 Storage auditor

Use unique canary values for prompts, answers, tool updates, replay frames, credentials, and auth state. After each privacy scenario, recursively inspect the TaskAI state root and database values. A canary match outside an explicitly test-owned transient fixture fails the suite.

The test harness may retain fixture content in memory to make assertions. That does not authorize production code to persist it.

## 5. UI element contract

These are the TaskAI-owned elements that require both rendering and interaction coverage.

| Surface | Required elements | Required behavior |
| --- | --- | --- |
| Tasks | title, Add Task, Settings, no-provider notice, Task rows, Created column, Active count, Dormant count, Active/All switch, sort label | Always launches first; newest-created ordering; counts include managers and subordinates; empty and historical states remain navigable |
| Settings · Providers | back path, enabled control, provider name, connection status, capability labels, configure/auth action, recheck action | Auth starts only here; status and capabilities are truthful; disabling affects new creation only |
| Add Task | Task title input, create action, cancel/back action, validation/error region | Creates one Task and returns to/selects it without provider setup or checkout behavior |
| Add Manager Session | enabled manager-provider selector, optional Session name, create action, cancel/back action, unavailable-provider guidance | Offers only enabled manager-capable providers; reuses auth; opens the new manager conversation |
| Task workspace | Task title, Sessions region, Add Session, manager rows, indented subordinate rows, provider, role/mode/state text, selected-manager ACP pane, manager input, footer/status, back/help/lifecycle actions | Preserves ownership hierarchy; user input targets only the selected manager; manager output streams immediately |
| Empty/error state | direct explanation, exact affected target where known, safe next action | Never launches auth implicitly, manufactures an answer, or hides whether work is still running |

The visual design may refine spacing and glyphs, but it may not remove these elements or change their approved meaning without updating the product vision.

## 6. Terminal size and rendering matrix

Run renderer and PTY coverage at:

| Size | Role in the suite | Acceptance |
| --- | --- | --- |
| `40x16` | constrained stress case | No panic, escape leakage, overlap, or unreachable exit/back path. A bounded compact or resize-required view is acceptable. |
| `60x18` | narrow supported workflow | Tasks, Settings, creation, hierarchy navigation, manager input, and streamed output remain usable. Labels may truncate deliberately but targets remain distinguishable. |
| `80x24` | baseline terminal | Canonical screens and two-region Task workspace match approved goldens. |
| `120x40` | wide terminal | Extra space expands useful content rather than introducing different behavior. |

Also test live transitions `120x40 -> 60x18 -> 40x16 -> 80x24` while:

- a list row is selected;
- a manager input draft exists;
- output is streaming;
- the user has scrolled away from the newest content;
- an error or disconnected-provider notice is visible.

Across resize, TaskAI must preserve the current Task, selected manager, input draft and cursor, lifecycle state, and user scroll intent. Content may reflow; actions may not silently retarget.

### 6.1 Golden policy

Maintain focused goldens for TaskAI-owned chrome at `60x18`, `80x24`, and `120x40`. The `40x16` case may use semantic assertions plus one constrained-state golden.

Each golden change requires:

1. the triggering product or UX decision;
2. a semantic assertion update when behavior changed;
3. review of all sizes for that surface;
4. confirmation that provider text was deterministic fixture text;
5. no bulk accept/update without inspecting the diff.

Do not snapshot provider-native authentication UI or any provider-native terminal. Those are not TaskAI surfaces.

## 7. Navigation, input, and accessibility contract

### UI-001 — complete keyboard path

Starting from process launch, a keyboard-only user can:

1. open Settings and return;
2. add and open a Task;
3. add a Manager Session;
4. move among managers in the Session tree;
5. enter and submit a manager prompt;
6. scroll manager output without changing the selected Session;
7. open help;
8. leave the workspace without stopping work;
9. invoke an explicit lifecycle action on the exact selected target.

### UI-002 — mouse parity

Where mouse support is enabled, every visible pointer action produces the same state transition and target as its keyboard equivalent. Mouse-disabled terminals remain fully usable.

### UI-003 — focus and selection

- Exactly one interactive region owns focus.
- Focus and selected Session are visually distinguishable without relying only on color.
- Opening and closing overlays restores focus to the invoking control.
- List scrolling never changes selection merely because rows move.
- Streaming never steals focus from the manager input or navigation.

### UI-004 — text-first status

Working, Answer, Follow, answered, dormant, interrupted, closed, disconnected, and needs-repair states have text or glyph-plus-text representations. Color may reinforce status but cannot be its only carrier. `NO_COLOR` and a monochrome terminal must remain understandable.

### UI-005 — help and key hints

Every displayed key hint works in the current context. Help lists TaskAI navigation and lifecycle actions only. It contains no invented provider commands, MCP actions, tmux controls, Recall command, or direct-subordinate prompt action.

### UI-006 — safe terminal text

ACP text, Task titles, Session labels, provider names, and errors are tested with:

- ASCII;
- Unicode wide characters;
- combining characters;
- right-to-left markers;
- newlines and tabs;
- long unbroken text;
- ANSI/OSC escape sequences;
- control characters.

Untrusted text must not execute terminal controls, rewrite TaskAI chrome, alter the window title, inject fake key hints, or escape its bounded region.

### UI-007 — back is not provider input

Back/Escape returns to the prior TaskAI surface according to the active UI context. It never sends a control character to a provider process. Leaving the Task workspace detaches the viewer; it does not stop or cancel work.

## 8. Tasks screen tests

### UX-101 — first and every launch

- With empty state and no enabled provider, Tasks is the first rendered product screen.
- With configured providers and historical Sessions, Tasks is still first.
- No setup wizard, auth process, checkout dialog, permission-preparation screen, manager Session, or provider process starts merely from launch.

### UX-102 — empty Tasks

- Add Task and Settings remain reachable.
- The no-provider notice appears only when appropriate and is non-blocking.
- Creating a Task is allowed without configuring a provider.
- Attempting Add Session without an enabled manager-capable provider gives a direct Settings path and does not authenticate automatically.

### UX-103 — ordering and filters

- Active Tasks is the default view.
- Tasks sort by Task creation time descending, not title, updated time, Session activity, or provider state.
- Equal timestamps use a deterministic tie-breaker without changing the primary rule.
- All Tasks reveals dormant and historical Tasks and preserves newest-created ordering.
- Returning from a Task workspace preserves a valid filter and selection where possible.

### UX-104 — counts

- Active and Dormant counts use mechanical Session state.
- Counts include top-level managers and subordinate Sessions.
- State changes update counts without changing Task creation ordering.
- Zero, large, and concurrently changing counts render without overlap or negative/intermediate values.

### UX-105 — large and unusual lists

Prove empty, one-row, many-row, duplicate-title, long-title, Unicode-title, and historical-only lists. Scrolling preserves header context, selected Task, and action access.

## 9. Provider Settings tests

### UX-201 — explicit provider setup

- Settings is reached from Tasks or an explicit no-provider path.
- Enabling does not pretend the provider is authenticated.
- Configure/auth invokes only the selected provider's connector-owned auth action.
- Returning from successful auth shows Connected after a mechanical probe.
- No credential, token, cookie, or auth database is copied into TaskAI state.

The provider-owned auth screen itself is outside visual snapshot scope. Test the TaskAI screen before invocation, the exact connector target, cancellation/failure return, and the resulting status.

### UX-202 — one-time reuse

- After one successful provider auth, opening Tasks, creating Tasks, creating or reopening Managers, and reattaching never invokes auth again while the connector reports valid credentials.
- A connector-reported expiry changes the Settings state to Disconnected or Needs repair.
- Reauthentication is available only through Settings.

### UX-203 — provider enablement

- Only explicitly enabled providers may be selected for new Sessions.
- Only enabled manager-capable providers appear in Add Manager Session.
- Disabling a provider prevents new Sessions but preserves existing Task and Session metadata and provider-owned state.
- An existing Session reports its truthful unavailable/disconnected state instead of being silently moved to another provider.

### UX-204 — capability truth

Manager, Subordinate, Resume, and Replay labels come from passed connector conformance. Unsupported capability controls are absent or unavailable with a concise explanation. Recheck updates mechanical status without changing capability claims that were not reconformed.

### UX-205 — Settings visual states

Render and interact with mixed rows covering Connected, Not connected, Disconnected, Needs repair, missing connector, disabled, and long provider names. Selection, enablement, status, and capability columns must remain attributable to the same row at every supported width.

## 10. Task and Manager creation tests

### UX-301 — Add Task

- Create accepts the displayed title exactly according to the input contract.
- Cancel/back creates nothing.
- One submit creates exactly one Task even if Enter or mouse input repeats.
- Success opens or selects the created Task without starting provider auth, repository initialization, checkout preparation, or worktree creation.

### UX-302 — Add Manager Session

- Add Session always means Add Manager Session.
- The selector contains only enabled manager-capable providers.
- The optional Session label is presented separately from provider identity.
- Submit creates exactly one top-level Manager Session in the current Task.
- Existing valid provider authentication is reused.
- The selected manager conversation opens immediately through ACP.
- Cancel/back creates nothing and does not launch a connector.

### UX-303 — unavailable manager path

With zero enabled manager-capable providers, Add Session remains safe and gives an explicit Settings path. It does not offer a subordinate-only provider, invent manager capability, or start an auth flow.

### UX-304 — creation failure

Start, connector, auth-validity, and exact-resume failures leave an understandable mechanical state on the intended Session. No replacement conversation, hidden retry, duplicate row, or unrelated Session mutation occurs.

## 11. Task workspace tests

### UX-401 — hierarchy

- Every user-created Session is a top-level manager.
- Every subordinate is indented exactly one level beneath its same-Task manager.
- Multiple managers and their children remain visually separable.
- A child never appears under another Task or another manager.
- A subordinate can never render as a parent of another subordinate.

### UX-402 — row information

Manager and subordinate rows show enough text to identify label, provider, role or current behavior, and mechanical lifecycle state as defined in the vision. Truncation cannot make two visible targets indistinguishable without another accessible detail path.

### UX-403 — selected manager pane

- Selecting a manager binds the right pane and input to that exact manager ACP Session.
- Switching managers changes both the heading and target atomically.
- Late frames from a previously selected manager do not appear as content from the current manager.
- Frames continue to be handled for running, non-selected managers without focus theft or provider backpressure.

### UX-404 — subordinate is not a user target

A subordinate row may be inspected or highlighted for hierarchy/status, but the user cannot submit a direct prompt, cancel a subordinate turn, select Follow, or request Replay from TaskAI UI. Those actions belong to its manager through the conformed manager connector. No composer or send action may silently retarget to a subordinate.

### UX-405 — manager input

- Submitted text reaches the selected manager ACP connection exactly once.
- Whitespace and Unicode follow the input contract and are not wrapped in a TaskAI delegation envelope.
- Input remains usable while other Sessions stream.
- Busy, disconnected, interrupted, and closed states prevent an invalid submit and explain the mechanical reason.

### UX-406 — workspace chrome

The Task title, Sessions region, Add Session, selected manager identity, provider, lifecycle state, subordinate count, back action, help action, and explicit stop/lifecycle target remain internally consistent as state changes.

### UX-407 — historical Sessions

Historical metadata remains visible without implying transcript ownership. Selecting it either exactly resumes the same provider Session when advertised or displays Resume unavailable. TaskAI never reconstructs a conversation from rendered output, replay frames, or provider-private files.

## 12. Answer, Follow, and Replay tests

These are connector/runtime contracts reflected by Session rows and the manager's public ACP surface. They are not new TaskAI keyboard commands, UI buttons, semantic messages, or provider-facing TaskAI tools.

### ACP-501 — Answer default

- Every subordinate invocation begins in Answer unless the manager explicitly requests Follow for that invocation.
- Intermediate subordinate messages, plans, tool updates, and execution output are not delivered into manager model context in Answer.
- On success, only that invocation's final public ACP answer and native completion state are delivered to its manager.
- No prior answer, unrelated Session, or automatic replay is included.

### ACP-502 — multiple current answers

Run at least three subordinate Sessions concurrently with deliberately reordered completion. The manager receives exactly one correlated current answer from each requested invocation, in actual completion order, with no batching, merging, ranking, or TaskAI summary.

Also cover two invocations with identical answer text to prove correlation does not depend on content parsing.

### ACP-503 — Answer non-success

Cancellation, refusal, token exhaustion, malformed completion, and transport failure preserve their native non-success state. TaskAI does not manufacture a final answer or mark the row answered.

### ACP-504 — explicit Follow

- Follow applies only when explicitly selected by the manager for one current invocation.
- Exposed ACP frames reach the manager live and in order.
- Unknown frames are forwarded without semantic projection.
- The Task workspace can reflect Follow mechanically without interpreting frame meaning.

### ACP-505 — Follow reset

On success, cancellation, refusal, token exhaustion, or transport failure, Follow ends. The next invocation on that subordinate starts in Answer. Follow never carries to another subordinate or survives exact resume as a sticky preference.

### ACP-506 — Replay eligibility

- Replay is unavailable before a target invocation completes.
- Replay is available only when the target connector advertises and passes bounded Replay conformance.
- Unsupported Replay remains unavailable; TaskAI creates no transcript, rolling buffer, whole-Session load, or private-history fallback.

### ACP-507 — Replay 1

After at least two completed invocations, an explicit replay request with no N returns only the exposed ACP frames for the most recent completed invocation of the named subordinate Session.

### ACP-508 — Replay N

For a named subordinate with more than N completed invocations, replay N returns exactly the last N completed invocation streams, in their original invocation/frame ordering. Validate N boundaries, invalid values, and a request larger than the connector's available bounded history without silently widening to whole-Session replay.

### ACP-509 — multi-target Replay

For two or more named subordinate Sessions with different history lengths, replay N applies independently to each target. No unnamed Session and no older range appears. Correlation uses protocol identity, not answer text.

### ACP-510 — no automatic history

Complete several invocations, switch managers, detach/reattach, and submit a new prompt. No previous answer or replay stream is inserted unless the manager explicitly requested the bounded Replay operation.

## 13. Streaming and content rendering tests

### UI-601 — push-driven first content

Static/code review and an instrumented fake-provider test must prove the path from available manager ACP frame to UI invalidation is push-driven, not polling-driven.

In the controlled local PTY harness, the first visible content update must render within 250 ms of the fake frame becoming available. Record the observed timing as diagnostic evidence; fail only on the bound or missing output, not on relative microbenchmarks.

### UI-602 — order and completeness

For uniquely numbered frames, rendered public content preserves ACP order. Backpressure may slow the producer; it may not drop, duplicate, or silently reorder frames. Test burst, slow, oversized-within-limit, and concurrent non-selected Session traffic.

### UI-603 — bounded unknown content

Known public content uses its TaskAI renderer. Unknown content uses a bounded generic representation that preserves mechanical identity needed for display and debugging without inventing tool/result meaning. Deeply nested, large, and malformed shapes fail safely.

### UI-604 — scrolling while live

- At the live edge, new manager output remains visible.
- After the user scrolls away, streaming does not force the viewport back to the bottom.
- Returning to the live edge resumes follow behavior.
- Streaming preserves selected text where the terminal library supports selection, focused control, manager input draft/cursor, selected manager, and Session-tree scroll position.

### UI-605 — no cross-Session visual contamination

Interleave frames from two managers and several subordinates. The visible conversation shows only the selected manager's public ACP surface, while Session rows update only for their exact Sessions.

### UI-606 — diagnostics do not leak private state

Errors and generic frames must not print provider environment variables, credentials, auth database paths, private transcript paths, raw process command lines containing secrets, or content from another Session.

## 14. Detach, lifecycle, and recovery tests

### UX-701 — leave workspace

Navigate back to Tasks during active manager and subordinate work. The viewer detaches; no turn is cancelled, no ACP Port is closed, and no runtime is stopped.

### UX-702 — close and reopen viewer

Terminate the TaskAI viewer during active fake-provider work, relaunch `taskai`, and reopen the same Task and manager. Work continued while detached. TaskAI reconnects to the existing runtime rather than starting another provider conversation.

The test does not require TaskAI to reconstruct old display content. Only provider-supported current output or exact resume behavior may repopulate it.

### UX-703 — explicit stop

Stop is an explicit lifecycle action tied to the exact selected Manager Session. Stopping it stops or marks interrupted only its owned subordinate tree. Another manager, another Task, and provider-owned external state remain untouched.

### ACP-704 — exact Resume

When Resume is advertised, restart the worker and use the same opaque provider reference exactly once. Resume failure never falls back to Start and never creates a replacement conversation. When Resume is unsupported, the UI says so rather than inferring context from output.

### UX-705 — runtime failure

A manager crash marks that manager and its subordinate runtime tree interrupted. The screen remains navigable, preserves historical metadata, and offers only actions supported by connector capability. Manager-local replay loss is not hidden by a TaskAI transcript fallback.

## 15. Failure-state matrix

Every row requires model, renderer, keyboard, and PTY coverage where applicable.

| Failure/state | Required user-facing outcome |
| --- | --- |
| No providers enabled | Tasks remains primary; explicit Settings path; no auth starts |
| No enabled Manager provider | Add Session explains the requirement and points to Settings |
| Provider executable missing | Exact provider identified; safe configure/recheck path |
| Credentials expired | Disconnected or Needs repair; reauth only in Settings |
| Manager start fails | Intended Session remains identifiable and failed/interrupted; no replacement Start |
| Subordinate turn fails | Native failure reaches only its manager; no synthetic answer |
| Replay unsupported | Unavailable for exact subordinate; no fallback or whole-Session option |
| Resume unsupported | Historical metadata remains; no reconstructed conversation |
| Resume fails | Same Session shows failure; no new provider reference |
| Malformed/oversized ACP | Bounded error; affected runtime isolated; terminal remains usable |
| Runtime crashes while detached | Metadata shows interrupted on return; no hidden recovery claim |
| Terminal becomes constrained | Bounded compact/resize state; no panic or garbled terminal |
| State store unavailable/corrupt | Bounded startup error with no automatic destructive reset |

Failure wording must say what TaskAI mechanically knows. It must not claim that agent work is correct, complete, lost, or recoverable unless the provider/runtime state proves it.

## 16. Persistence, privacy, and architecture tests

### SEC-801 — only two persistent entities

Fresh and upgraded TaskAI state contains only Task and Session as first-class product entities. Provider Settings remains configuration. Fail on durable Run, Invocation, Event, Message, Answer, Tool, Result, Transcript, Replay, Artifact, Workflow, Team, Broker, Mailbox, or token-stream records.

### SEC-802 — content canary audit

Run normal, Answer, Follow, Replay, error, detach, and exact-resume flows with distinct canaries. Inspect databases, journals, logs, cache files, sockets, crash files, and configuration. Find no prompt, answer, plan, tool, replay frame, token stream, credential, cookie, auth-state, or provider-private transcript content.

### SEC-803 — filesystem and process safety

- User state directories and files use restrictive permissions.
- Connector arguments execute literally without a shell.
- Logs and errors redact non-content secrets without semantically parsing agent output.
- TaskAI never initializes, retargets, shares, creates, merges, or deletes a repository/worktree automatically.
- Stopping or removing TaskAI metadata never deletes provider-owned state implicitly.

### SEC-804 — forbidden architecture scan

Fail production code or dependencies that introduce:

- MCP;
- tmux, terminal scraping, or native provider TUI embedding;
- web UI, HTTP API, or WebSocket product surfaces;
- TaskAI delegation/inspection tools;
- semantic event/result normalization;
- transcript/message/tool persistence or search;
- workflow, broker, mailbox, or peer messaging;
- provider-private history crawling;
- nested subordinate hierarchies;
- old `acpctl` product paths or legacy compatibility shims.

Documentation and negative test assertions may name forbidden concepts.

## 17. End-to-end acceptance suite

Run these through the compiled binary and deterministic ACP fixtures. Each scenario captures terminal size, input trace, semantic screen assertions, runtime call trace, storage audit result, and final process status.

### E2E-901 — empty first launch

1. Start with an empty TaskAI state root.
2. Launch at `80x24`.
3. Assert Tasks is visible, Add Task and Settings work, and no connector/auth process started.
4. Repeat at `60x18` and `40x16` constrained mode.

### E2E-902 — one-time provider setup

1. Open Settings from Tasks.
2. Enable and authenticate a fake Manager-capable provider.
3. Return to Tasks, create a Task, add two Manager Sessions, leave, and reopen both.
4. Assert the auth action ran exactly once and no other surface requested login.

### E2E-903 — Task history and counts

1. Seed Tasks with controlled creation times and manager/subordinate states.
2. Prove Active default, newest-created ordering, counts, All Tasks, scrolling, and historical metadata.
3. Change runtime state and prove counts update without re-sorting by activity.

### E2E-904 — create and use a Manager

1. Add Session within a Task.
2. Select an enabled Manager-capable provider and optional label.
3. Submit a user prompt.
4. Assert one top-level manager was created, the exact selected ACP connection received the prompt, and public manager output streamed into its pane.

### E2E-905 — multiple requested answers

1. The fake manager opens three subordinate Sessions in Answer.
2. Subordinates emit intermediate frames and complete out of order.
3. Assert the manager receives exactly three current final answers and completion states, no intermediate frames, and no old answer.
4. Assert the tree shows three correctly owned children and TaskAI performs no synthesis.

### E2E-906 — Follow and reset

1. The fake manager explicitly follows one current subordinate invocation.
2. Assert exposed frames arrive live and in order.
3. Complete the invocation with each terminal condition in parameterized runs.
4. Start another invocation and assert Answer is restored.

### E2E-907 — replay 1

1. Complete two invocations for one Replay-capable subordinate.
2. Explicitly request Replay with no N.
3. Assert only the most recent completed invocation stream is returned.

### E2E-908 — replay N across Sessions

1. Complete at least three invocations in each of two Replay-capable subordinates.
2. Request replay 2 for both.
3. Assert two bounded streams per target, correct order/correlation, no unnamed Session, and no older invocation.

### E2E-909 — unsupported Replay

1. Use a subordinate without Replay.
2. Assert Replay is unavailable through the manager connector and represented truthfully in TaskAI state.
3. Run the storage and process trace audits to prove no fallback, whole-Session load, or private-store crawl occurred.

### E2E-910 — detach and return

1. Start manager and subordinate work.
2. Leave the workspace, then terminate the viewer.
3. Let fake providers produce more work while no viewer exists.
4. Relaunch, reopen the same manager, and prove the original runtime continued with no duplicate Start or auth.

### E2E-911 — resize and input stability

1. Draft manager input and scroll away from the live edge while output streams.
2. Resize through the complete matrix.
3. Assert target Session, draft/cursor, scroll intent, focus, ordering, and lifecycle state remain correct.

### E2E-912 — failure recovery

Parameterize missing provider, auth expiry, start failure, subordinate failure, unsupported Resume, failed Resume, malformed frame, oversized frame, and manager crash. Assert the failure matrix and that another Task/manager remains usable.

### E2E-913 — final storage audit

Run all normal workflows with unique content/auth canaries, close TaskAI cleanly, and inspect every file and database value under its state root. Only approved metadata may remain.

## 18. Real connector conformance and smoke

Real-provider tests are local and opt-in. They are required before the UI advertises the corresponding capability.

For every supported connector, record:

- connector/version and ACP version;
- Manager, Subordinate, Resume, and Replay claims;
- auth initiation and reuse behavior;
- exact Start versus Resume calls;
- Answer completion/public-message boundary;
- Follow frame behavior and reset;
- Replay 1, Replay N, and range-limit behavior when claimed;
- detach/reattach behavior;
- known unsupported behavior.

At least one real connector must pass the dual-role Manager path, at least one must pass Subordinate, and at least one must pass bounded Replay before those product paths ship. Evidence is sanitized and never committed if it contains provider state or conversation content.

Do not compare provider answer quality or snapshot provider-native UI. The smoke test proves protocol and lifecycle claims only.

## 19. Decision traceability

| Vision decision | Required tests |
| --- | --- |
| D1 Tasks always launches first | UX-101, E2E-901 |
| D2 auth only in Settings | UX-201, UX-202, E2E-902 |
| D3 one-time auth reuse | UX-202, E2E-902, E2E-910 |
| D4 only Task and Session | SEC-801, SEC-802 |
| D5 Add Session creates Manager | UX-302, E2E-904 |
| D6 one same-Task parent manager | UX-401, SEC-801 |
| D7 manager owns downstream ACP | ACP-502, UX-404, real conformance |
| D8 all provider communication is ACP | ACP-501–ACP-510, SEC-804 |
| D9 Answer default | ACP-501, ACP-505, E2E-905 |
| D10 only current requested answers | ACP-502, ACP-510, E2E-905 |
| D11 invocation-scoped Follow/reset | ACP-504, ACP-505, E2E-906 |
| D12 post-answer Replay defaults to 1 | ACP-506, ACP-507, E2E-907 |
| D13 N applies per named Session | ACP-508, ACP-509, E2E-908 |
| D14 Replay is exposed ACP, not reconstruction | ACP-507–ACP-509, SEC-802 |
| D15 no Recall/automatic history | ACP-510, UI-005, SEC-804 |
| D16 Replay is capability-gated | UX-204, ACP-506, E2E-909 |
| D17 no content persistence | SEC-801, SEC-802, E2E-913 |
| D18 detach never stops work | UX-701, UX-702, E2E-910 |
| D19 no MCP or tmux | UI-005, SEC-804 |
| D20 terminal-only V1 | SEC-804, all PTY acceptance |

## 20. Required commands and release evidence

Every milestone submission runs:

~~~text
make fmt-check
make test
make race
make vet
make build
make security-check
make acceptance
~~~

`make acceptance` must run the deterministic PTY scenarios. Real-provider smoke remains separate and opt-in.

The release packet includes:

- command results, platform, terminal library/version, and Go version;
- unit, renderer, golden, input, contract, integration, PTY, and storage-audit counts;
- reviewed golden diffs;
- terminal size matrix results;
- sanitized timing evidence for UI-601;
- connector capability/conformance matrix;
- known unsupported connectors or capabilities;
- forbidden-architecture scan result;
- clean artifact and credential scan result.

No flaky test is muted, retried until green, or replaced by an arbitrary sleep. Fix its synchronization boundary or document a product/runtime defect.

## 21. Definition of verification complete

Verification is complete only when:

1. Every applicable test ID in this plan passes.
2. Every approved decision D1–D20 has passing traceability evidence.
3. All TaskAI-owned screens have semantic assertions and reviewed goldens at the required sizes.
4. Every visible action has keyboard coverage and mouse parity where supported.
5. Empty, loading/starting, working, dormant, interrupted, closed, disconnected, needs-repair, unsupported, and constrained-terminal states are covered.
6. Answer, Follow/reset, Replay 1, Replay N, and multi-target Replay pass without a TaskAI content store.
7. Detach and exact Resume tests prove no replacement conversation or viewer-owned runtime.
8. Storage canary and forbidden-architecture scans are clean.
9. Advertised connector capabilities have real conformance evidence.
10. All required commands pass from a clean checkout containing only intentional source changes.
