# TaskAI build plan

Builds the production TaskAI: one terminal command center for Task → Manager → Subordinate teams of native CLI agents. Derived from three sources: `docs/UX_PRODUCT_VISION.md` (surface and journey), `backend_communication.md` (responsibilities, connector mechanics, safety, release gate), and the golden frames in `tests/fixtures/` (the approved UI contract). Companion: `test_plan.md` — identical milestone spine, tests written first at every step.

## Architecture

Rust workspace producing one installable binary, `taskai`, plus connector sidecar processes it owns.

| Crate | Owns |
|---|---|
| `taskai-core` | Domain model, Registry (tasks, sessions, stable refs, topology, states), persistence, workspace provisioning (repos, git worktrees), destroy semantics |
| `taskai-context` | Context snapshots (`self`/`session`/`tree`, text+json, revisioned), bootstrap installer (`AGENTS.md` managed block), per-harness fresh-context delivery |
| `taskai-connectors` | Connector contract (`probe, resolve_session, attach_native_tui, install_bootstrap, prompt, subscribe, cancel_transport, snapshot, await_terminal, detach`), the four connectors, the Conduit turn pipeline, writer exclusion, dispatch correlation |
| `taskai-pane` | portable-pty spawn/resize/io, vt100 grid, blit into the TUI working pane |
| `taskai-tui` | FILL design tokens, screens, overlays, overlay compositor, focus model, key routing |
| `taskai` (bin) | CLI surface: TUI launch, `context self|session <ref>|tree`, `session prompt <ref> --stdin --wait --format json` |

Process model: the `taskai` binary spawns and owns its sidecars — `codex app-server` over stdio against the shared Codex thread store, one authenticated loopback `opencode serve`, the displayed Claude PTY itself (owned-tui: the display *is* the session), and `grok agent stdio` only during a handoff window. Runtime state lives in an owner-only (0700) `~/.taskai/` directory: registry state, dispatch-correlation log, sockets, logs. Credentials never appear in argv, prompts, transcripts, or logs. No tmux dependency; no local model server.

Design contract: the approved terminal design is pinned by the golden frames in `tests/fixtures/*.ans`, captured from the interactive prototype that preceded this build. Mapping:

| Prototype | Production |
|---|---|
| The palette, slab/soft-edge primitives, keycap chips, masthead, keybar | `taskai-tui` fill module — same token names, same chroma budget (ION live/focus, AMB attention, RED destroy) |
| Renderers: boot, home, harness bay, rail, pane, attach, attention, drawers, destroy, help | `taskai-tui` screens, one module per screen |
| The state machine, Ctrl+G boundary, native/ACP view toggle `c` | `taskai-tui` input router and focus model |
| Overlay compositing | `taskai-tui` overlay compositor over the rendered buffer |
| The demo world | No production counterpart — data comes from the `taskai-core` registry and live panes |

## Milestones

### M0 — Walking skeleton

Goal: `taskai` opens the TUI shell and the rendering pipeline is testable headlessly.

Scope in: cargo workspace, all crates stubbed; masthead, empty home, keycap keybar; `q` quits; FILL tokens ported; frame-dump harness. Scope out: any data, any connector.

```text
Component: fill tokens + row engine
Why: every screen is built from width-exact painted rows; the prototype proved the grammar.
Expectation: segments (text, fg, bg, bold) compose into rows padded to exact width on the
  VOID ground; slabs are background fills with ▄/▀ soft edges; keycaps render on KCAP fill.
Value: one rendering grammar shared by every screen; pixel-stable parity with the prototype.
Pseudo:
  seg = (text, fg, bg, bold)
  row(segs, width, base_bg): pad/clip so painted cells == width exactly
  slab(top | body | bottom, glass_color), keycap(label), masthead(crumb, right, lamp), keybar(pairs)
```

```text
Component: frame-dump harness
Why: parity and TDD need frames as data, not eyeballs; ANSI is ground truth.
Expectation: render(state, cols, rows) -> ANSI string with no terminal attached; a hidden
  `taskai --dump-frame <screen> --cols N --rows N` entry prints it.
Value: every screen test asserts on captured frames; prototype frames become fixtures.
Pseudo:
  render is a pure function of (state, cols, rows)
  dump-frame: build canned state for the named screen, print render() to stdout, exit
```

Exit demo: `taskai` shows masthead + empty board + keybar full-screen, `q` quits cleanly; `taskai --dump-frame home` emits a width-exact frame. Needs: nothing.

### M1 — Registry and persistence

Goal: the domain exists and survives restart; destroy is safe by construction.

Scope in: Task/Session model (roles Manager/Subordinate, harness, workspace, states LIVE/QUEUED/HANDOFF/ATTENTION/READY/SETUP/WORKING/COMPLETE), stable session refs, the `{taskai_session_ref, provider, provider_session_id, runtime_id, workspace, connector_version}` mapping, atomic persistence, workspace provisioning, destroy semantics, source attribution records. Scope out: live processes.

```text
Component: Registry
Why: backend §2 — TaskAI's identity is its session-reference registry; everything else
  resolves through it.
Expectation: create/read/update tasks and sessions; refs never recycled; topology is
  parent-linked; state transitions validated; every mutation attributed (human UI or
  registered native session) and persisted atomically.
Value: one truth for the board, the rail, context, and the conduit target lookup.
Pseudo:
  create_task(name, repo) -> task_ref
  create_session(task, parent?, role, name, harness, workspace_choice) -> session_ref
  resolve(session_ref) -> exact provider mapping
  persist: serialize to ~/.taskai/state, write temp file, atomic rename
```

```text
Component: workspace provisioning
Why: journey step 5 — a Subordinate runs in the Manager's repo, another repo, or a git worktree.
Expectation: worktree choice takes base repo, base branch, name; previews the resulting path;
  creates worktree before launch. Destroy never removes it.
Value: isolation between agents without ever risking the user's repository.
Pseudo:
  provision(choice): same_repo -> manager.repo | other_repo -> chosen path
    | worktree -> git worktree add <base>-worktrees/<name> from base_branch; return path
```

```text
Component: destroy semantics
Why: UX vision §6 — exact blast radius shown, repositories and history never touched.
Expectation: destroying a Subordinate removes it; a Manager removes it and its children;
  a Task removes all sessions then the entry. Repos, worktrees, provider history retained.
Value: destroy is trustworthy; users stop fearing the red key.
Pseudo:
  blast(item) -> ordered session list (for the confirmation slab)
  destroy(item): stop TaskAI-owned conduits, delete registry entries only
```

Exit demo: scripted CLI run creates a task tree, kills the process, relaunches, tree intact; destroy leaves a fixture repo and worktree byte-identical. Needs: M0.

### M2 — Pane conduit

Goal: a real CLI lives inside the working pane; the boundary chord works.

Scope in: portable-pty spawn/resize, vt100 grid maintained from pty output, blit into the pane region, Ctrl+G boundary, focus model, pane header strip (session · harness · state). Scope out: connector semantics.

```text
Component: pty pane
Why: the non-negotiable surface — the pane is the CLI's exact native rendering,
  never a TaskAI re-implementation.
Expectation: spawn command in a pty sized to the pane; feed output to a vt100 grid;
  blit grid cells (glyph, fg, bg, attrs) into the frame each tick; resize propagates.
Value: Claude Code, Codex, OpenCode look exactly like themselves inside TaskAI.
Pseudo:
  pane = spawn(cmd, cols, rows); reader thread -> vt100 parser -> grid
  blit(frame, region): copy grid cells; cursor drawn only when CLI has focus
```

```text
Component: focus model
Why: UX vision — while the CLI is focused every keystroke passes through unchanged;
  Ctrl+G is the one reserved chord; single-letter commands only in navigation.
Expectation: two focus states; CLI focus forwards all bytes except Ctrl+G to the pty;
  navigation focus interprets TaskAI keys; keybar reflects the current state.
Value: typing prose in a CLI can never trigger TaskAI actions.
Pseudo:
  on key: if cli_focused: key == C-g ? focus=nav : pty.write(bytes)
          else: route to screen handler
```

Exit demo: `taskai` attaches a real shell/CLI in the pane; typing reaches it verbatim; Ctrl+G flips to navigation and back. Needs: M0.

### M3 — Claude connector (owned-tui)

Goal: first full vertical: create a Manager, its real Claude Code renders in the pane, one prompted turn goes through the Conduit into that exact PTY.

Scope in: connector contract trait; Claude connector: probe (`claude --version`), launch in workspace, attach, atomic prompt into the idle displayed PTY, completion via Stop hook or native JSONL record, in-process writer exclusion, ordered progress relay, structured outcome. Scope out: second writer hardening, cancel (M8).

```text
Component: connector contract
Why: backend §8 — four harnesses, one verb set; capability truth over simulation.
Expectation: trait with probe, resolve_session, attach_native_tui, install_bootstrap,
  prompt, subscribe, cancel_transport, snapshot, await_terminal, detach; each connector
  declares version range, rendering mode, context-delivery mode, collision constraints.
Value: adding a harness is implementing a trait, not rewiring the product.
Pseudo:
  trait Connector { probe() -> Capability; prompt(ref, text) -> TurnHandle; ... }
  Capability = { mode: shared-live | owned-tui | handoff-resume | unsupported, ... }
```

```text
Component: Claude owned-tui connector
Why: backend §7 — the one TaskAI-connected native PTY is the session; work must render there.
Expectation: prompt submits atomically only when the PTY is idle; the same process renders
  the work; completion observed from Stop hook or matching native JSONL; questions and
  approvals stay in the Claude terminal untouched; never a second writer.
Value: proves the whole chain — registry, pane, conduit, connector — on the primary harness.
Pseudo:
  prompt: assert idle; acquire writer lock(ref); write text + submit into pty
  await_terminal: watch Stop hook / session JSONL for the correlated turn; release lock
```

Exit demo: create task + Claude Manager in a fixture repo, its live TUI renders in the pane; `taskai session prompt <ref>` makes work appear in that same visible Claude; completion reported from the native signal. (Billed turn — runs under the test plan's live-proof approval protocol.) Needs: M1, M2.

### M4 — Context service

Goal: every session can answer who am I, why do I exist, what am I responsible for, who is around me.

Scope in: `taskai context self|session <ref>|tree` with `--format text|json`; snapshot with `schema_version`, monotonic `context_revision`, `generated_at`, task purpose/goal/phase, self identity/role/assignment/workspace/state, team chain, peers with refs and capabilities; explanatory text view; `AGENTS.md` managed-block installer (never overwriting non-TaskAI content; `CLAUDE.md` symlink/import on Unix); per-harness fresh delivery (Codex/Claude `UserPromptSubmit.additionalContext` + `SessionStart` refresh; OpenCode version-gated `experimental.chat.system.transform`; Grok bootstrap command + TaskAI-routed envelope); stale/unavailable reported honestly. Scope out: any orchestration decision.

```text
Component: context snapshot + views
Why: backend §5 — context is descriptive facts, bound to the registered process,
  never a model-supplied identity and never a decision.
Expectation: build snapshot from Registry only; text view explains ("You are Subordinate S2,
  created to…"), json view is stable; secrets/PIDs/socket paths omitted; caller identity
  bound to the registered native process, not a claimed ID.
Value: self-aware Managers and Subordinates without TaskAI making a single choice for them.
Pseudo:
  snapshot(ref) = { task, self, team_chain, sessions[] } @ context_revision++
  context self: resolve caller binding -> snapshot -> render text|json
```

```text
Component: bootstrap installer + fresh delivery
Why: backend §5 — a static discovery block plus exactly one delivery path per turn.
Expectation: managed block written idempotently into AGENTS.md; per-harness refresh hook
  installed where supported; newest revision supersedes copies in native history;
  failed refresh -> "context unavailable/stale", never invented topology.
Value: agents wake up knowing the team without any human pasting context.
Pseudo:
  install(repo): upsert managed block between markers; harness-specific hook files
  deliver(ref, turn): exactly one path per capability; on failure mark stale
```

Exit demo: `taskai context self` from inside a registered session prints the explanatory view; `context tree` shows the full chain; revision increments on topology change. Needs: M1 (M3 for in-CLI proof).

### M5 — Codex and OpenCode connectors (shared-live)

Goal: two harnesses where the same visible session updates live.

Scope in — Codex: visible CLI and bundled `codex-acp` on the same app-server and exact `threadId`; native TUI creates the thread, capture `thread/started`, ACP-load, both clients attached before prompting; correlate `threadId + turnId`; completion requires `turn/completed` plus the proven idle edge; the socket shim handles backpressure, close propagation, explicit reconnect. Scope in — OpenCode: one authenticated loopback `opencode serve`; native CLI and facade share server, directory, session ID; subscribe before prompting; generated `messageID`, `prompt_async`, match assistant by `parentID`; completion/error plus session-idle edge; recover event loss from native snapshots and deduplicate. Scope out: cancel hardening (M8).

```text
Component: shared-live connectors
Why: backend §7 — both clients attach to one provider session; the native CLI renders
  work initiated through the conduit, live.
Expectation: prompting is refused until dual attachment is verified; completion is claimed
  only from correlated provider events plus the proven idle edge; provider history is the
  only conversation store.
Value: a Manager's Codex/OpenCode pane behaves as if the user typed there — because it
  is the same session.
Pseudo:
  codex: ensure app-server; attach tui + acp to threadId; prompt -> watch turnId lifecycle
  opencode: ensure serve; subscribe; prompt_async(messageID); match parentID; idle edge
```

Exit demo: prompt a Codex Manager and an OpenCode Subordinate through the conduit; the work streams in their own visible CLIs; completion correlated. (Billed — approval protocol.) Needs: M3 (contract), M2.

### M6 — Grok connector (handoff-resume)

Goal: the honest fourth mode — never simulated live.

Scope in: handoff-safe state gating; suspend competing input; `grok agent stdio` against the same session ID; structured terminal result; exact-ID native resume; pane shows `Updating via ACP` during handoff; result labeled `handoff-resume`; unprovable handoff or resume reports `unsupported`. Scope out: everything live-looking.

```text
Component: Grok handoff connector
Why: backend §7 — Grok proves shared persistence and exact-ID resume, not multi-client
  live rendering; truth over theater.
Expectation: the native session must be connector-proven handoff-safe before dispatch;
  after the structured result the exact native CLI resumes; the UI never says live.
Value: users trust every state label because the one hard case never lies.
Pseudo:
  prompt: assert handoff_safe(ref); suspend input; run grok agent stdio(session_id)
  await result -> exact resume native CLI -> label handoff-resume | unsupported
```

Exit demo: prompt a Grok session; pane shows the handoff banner, then the resumed native Grok containing the result. (Billed — approval protocol.) Needs: M3 (contract), M2.

### M7 — Full TUI journey

Goal: every screen, overlay, and key of the prototype, on live data.

Scope in: boot probes; home board (task slabs, HUD aggregates, attention lamp, harness strip); harness bay (`h`: ready/setup, live counts, configure command, recheck, add custom CLI); new task (`n`: name field + repository explorer with canonical path, recents); Manager drawer (`m`: arrow-selected ready harnesses only; model/effort/permissions shown only when supported); Subordinate drawer (`a` on a highlighted Manager: parent shown, three workspace choices, worktree preview); rail with `└─` manager/subordinate connectors and per-selection workspace telemetry; pane with native chrome default and ACP conduit view on `c`; attach (Ctrl+G in/out); attention escalation Subordinate → Manager → Task with direct attach; destroy overlays with typed confirmation; help; `T` home; leave and resume. Scope out: nothing visible in the prototype.

```text
Component: screens on live registry
Why: the prototype is the approved contract; M7 replaces its demo world with M1-M6 truth.
Expectation: every renderer reads Registry + pane state; HUD counts derive from one session
  list (a handoff session is never counted LIVE); keybar shows only currently valid keys;
  drawers create real sessions through real connectors.
Value: the product T approved in tmux:4, now real.
Pseudo:
  home(state) -> slabs from registry aggregates
  rail(selection) -> tree rows + workspace/conduit telemetry of the selected session
  input router: navigation keys per screen; overlays composite over dimmed backdrop
```

Exit demo: the complete UX vision journey (steps 1–10) performed live in one sitting: launch → board → harnesses → new task → first Manager → attach → Subordinate with worktree → destroy preview → second Manager → attention → leave → resume. Needs: M1–M6.

### M8 — Caller-initiated turns at scale

Goal: humans and registered native sessions share one safe conduit.

Scope in: `taskai session prompt <ref> --stdin --wait --format json` for native initiators; source attribution (human UI vs registered session) on every dispatch; cross-process writer exclusion expiring at the provider's proven terminal boundary; decline when a writer is active or the connector reports an unsafe state; single submission with correlation; caller-requested cancel transported natively with the provider's result reported; uncertain acknowledgement reconciled from native history, never blindly resent. Scope out: any permission graph — roles are never ACLs.

```text
Component: Conduit turn pipeline
Why: backend §6 — one pipeline for every initiator; TaskAI transports intent,
  never invents it.
Expectation: resolve source and exact target; safety check; submit once; ordered progress;
  completion only from correlated provider signals; cancel and reconcile are transports.
Value: a Manager can drive its Subordinates over ACP while the human watches the same
  panes — with zero double-submission risk.
Pseudo:
  turn(source, target_ref, text):
    mapping = resolve(target_ref); refuse if writer_active or unsafe
    lock; dispatch_log.append(correlation); connector.prompt; stream progress
    outcome from await_terminal; unlock at proven boundary
  cancel(handle): connector.cancel_transport -> report provider result
  uncertain: reconcile via connector.snapshot before any resend decision
```

Exit demo: from inside a live Manager CLI, `taskai session prompt <subordinate-ref>` runs a turn that renders in the Subordinate's visible pane; a second concurrent submit is refused; a cancel lands natively. (Billed — approval protocol.) Needs: M3, M5.

### M9 — Packaging and release gate

Goal: clean install, safe runtime, gate green.

Scope in: single distributable bundling the CLI, context service, connectors, and bridge dependencies (`codex-acp`); provider CLIs remain user-installed, never modified or upgraded; owner-only sockets and runtime dirs; credentials out of argv/prompts/transcripts/logs; safety state stores correlation and locators only, never conversation content; TaskAI stops only its own connector/shim processes; detach removes the conduit and nothing else; harness probe reports manifest, handshake, and live-test status separately; full release-gate suite (test plan §Release gate) green. Scope out: §10 exclusions, forever until separately approved.

```text
Component: packaging + probe truthfulness
Why: backend §9 and §11 — a clean machine needs only TaskAI plus configured provider CLIs.
Expectation: install produces a working `taskai`; first run probes harnesses harmlessly;
  any session-creating or billed proof requires explicit user approval; failed identity or
  safety reports unsupported, never a weaker claim.
Value: the product is shippable and its claims are auditable.
Pseudo:
  bundle: taskai + sidecar binaries + hook assets; no provider CLI touched
  first-run: probe each harness -> READY | SETUP; deep proofs only behind approval
```

Exit demo: install on a clean account, run the journey, run the gate suite. Needs: M1–M8.

## Decisions assumed (need T's sign-off)

From `backend_communication.md` §4, this plan builds on: initiators are the human UI or any registered native session (TaskAI never originates); native interface `taskai session prompt <ref> --stdin --wait --format json`; focused keystrokes go straight to the CLI, explicit send targets the exact session; context interface `taskai context self|session|tree`; context is descriptive only; questions/approvals/tools stay provider-native; Grok shows `Updating via ACP` then exact resume, reported `handoff-resume` never live; harmless handshakes are automatic, billed proofs need explicit approval.

## Out of scope (backend §10)

No autonomous TaskAI-originated turns, delegation policies or ACLs, task planning or target selection, schedulers/workflows, central approval or question handling, prompt interpretation, semantic result aggregation, provider-neutral transcript/message bus, automatic agent restart/hibernation/destruction, autonomous retries, portfolio governance. Any of these requires a product decision, a responsibility classification, and threat/UX analysis first.
