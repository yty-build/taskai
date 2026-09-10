# Backend communication

TaskAI provides context, stable session references, and a proven ACP-to-native-display conduit. Native provider sessions own reasoning, conversations, approvals, and work. Managers, other native sessions, and humans may initiate work; TaskAI transports their intent but never invents it.

## 1. Product boundary

- Manager and Subordinate are roles attached to native provider sessions; no harness is intrinsically either role.
- A Manager normally plans and coordinates delegated work, but roles describe team function rather than API permissions.
- TaskAI describes registered topology and state. It does not choose work, delegation, targets, permissions, or next actions.
- A human may focus and type directly into any displayed native Manager or Subordinate CLI, or explicitly send through the conduit to a selected session; any registered native session may also address another. TaskAI never autonomously originates a turn.
- Provider-native history and UI remain authoritative. TaskAI never creates a transcript, semantic message model, or shadow conversation.
- Provider sessions own their prompts, tools, questions, approvals, cancellations, and
  normal lifecycle. TaskAI may observe or relay structured events without deciding them;
  only an explicitly confirmed, scoped Destroy may terminate a recorded tmux runtime.
- TaskAI owns only its context service, session-reference registry, connector processes, and display conduit.
- Minimal connector guards may reject a technically unsafe concurrent submission; this is collision protection, not delegation policy.
- Detaching or removing TaskAI metadata never deletes a repository, worktree, or provider-native history.
- Production uses tmux only to preserve each native process and terminal across TaskAI
  viewer detach/reconnect. tmux does not change provider submission, correlation,
  completion, cancellation, or handoff semantics. No local model server is required.

## 2. Complete TaskAI responsibility classification

Every v1 responsibility must appear below. Anything else is an expansion governed by Section 10.

| Responsibility | Classification |
|---|---|
| Register Task, role, assignment, and relevant topology metadata | Context |
| Give each native session fresh identity, purpose, assignment, team-chain, state, and capability context | Context |
| Install the static context-discovery bootstrap and supported refresh hooks | Context |
| Expose context as stable text and JSON | Context |
| Map one TaskAI reference to one exact provider session, workspace, runtime, and connector version | Session reference |
| Resolve and exact-resume or attach that native provider session | Session reference |
| Attribute an invocation to its human UI or registered native-session source | Session reference |
| Translate a caller-initiated request into the target connector's ACP/native call | ACP/display conduit |
| Render that work in the target's real native CLI using its proven integration mode | ACP/display conduit |
| Relay structured progress and terminal outcome without interpreting agent semantics | ACP/display conduit |
| Leave provider-native history untouched and use it only for exact recovery/replay verification | ACP/display conduit |
| Package and locally run the context, connector, and bridge dependencies without modifying provider CLIs | ACP/display conduit |
| Probe harness/version support and context-delivery behavior | Capability reporting |
| Publish truthful session state and `shared-live`, `owned-tui`, `handoff-resume`, or `unsupported` | Capability reporting |
| Prevent concurrent writers or submission into a provider-declared unsafe state | Minimum connector safety |
| Correlate completion, transport caller-requested cancellation, and reconcile uncertain dispatch | Minimum connector safety |
| Protect local connector endpoints and credentials; clean up TaskAI-owned bridge resources | Minimum connector safety |
| Detach the conduit without deleting provider history, repositories, or worktrees | Minimum connector safety |

## 3. Current support and primary proof boundary

| Harness | Proven mode | Submission conduit | Native target experience |
|---|---|---|---|
| Codex | `shared-live` | ACP → bundled `codex-acp` → shared app-server | Same session updates live |
| OpenCode | `shared-live` | ACP facade → shared authenticated HTTP/SSE server | Same session updates live |
| Claude | `owned-tui` | ACP facade → the one TaskAI-connected native PTY | Work renders live in that process |
| Grok | `handoff-resume` | Exclusive ACP turn → exact-ID native resume | Result appears after resume, not live |

The retained tmux proof establishes text prompt, same-session native rendering, and end-of-turn for Codex, OpenCode, and Claude. Grok proves shared native persistence and exact-ID resume, not multi-client live rendering. Tools, questions/approvals, steering, cancellation, and failure recovery remain release-gated integration proofs; TaskAI must transport or observe them without assuming ownership.

## 4. Proposed v1 decisions for human approval

| Decision | Proposed default |
|---|---|
| Allowed initiators | A human through TaskAI UI or any registered native session; TaskAI itself never autonomously creates a turn |
| Native-session interface | `taskai session prompt <taskai-session-ref> --stdin --wait --format json`, available through the session's normal shell capability |
| UI interface | Focused keystrokes go directly to the displayed native CLI; an explicit send action targets that exact native session through the conduit |
| Context interface | `taskai context self`, `taskai context session <ref>`, and `taskai context tree` |
| Context meaning | Descriptive current facts, never orchestration decisions or delegation policy |
| Native interaction | Questions, approvals, tools, prompt semantics, and conversation stay provider-native; TaskAI only relays when the connector requires it |
| Grok experience | Show `Updating via ACP`, suspend competing input, then exact-ID resume; report `handoff-resume`, never `live` |
| Capability testing | Harmless handshake automatically; any session-creating or model-billed proof requires explicit user approval |

## 5. Context and native self-awareness

Every form accepts `--format text|json`:

```text
taskai context self
taskai context session <taskai-session-ref>
taskai context tree
```

Context must answer: **Who am I? Why does this session exist? What am I responsible for? How do I fit into this team? Who is around me, and how can I reach them?** It contains `schema_version`, monotonic `context_revision`, `generated_at`; the Task's purpose, goal, and current phase; self identity, role, reason-for-creation, assignment, expected contribution, workspace, and state; the reporting/delegation chain from Task through Managers to Subordinates; relevant parents, children, peers, their assignments, availability, stable TaskAI references, and declared capabilities. It describes the environment and relationships without deciding anyone's next action. Provider credentials, sockets, PIDs, and transcript paths are omitted.

```text
task:       {ref, purpose, goal, phase}
self:       {session_ref, role, reason_for_creation, assignment, expected_contribution, workspace, state}
team_chain: {parent, managers, subordinates, peers}
sessions[]: {session_ref, role, assignment, workspace, state, capabilities}
```

The human/model text view must explain those facts, not dump identifiers. For example:

```text
You are Subordinate S2, created to evaluate the repository's authentication flow.
You contribute evidence to Task T1: "Assess release readiness."
Your team chain is Task T1 → Manager M1 → you; M1 is active in /repo/main.
Peer S1 is testing the API in /repo/api and can be reached as session S1.
Your workspace is /repo/auth-worktree; report findings to M1.
```

TaskAI binds `self` to the registered native process/session rather than trusting a model-supplied ID, cwd, tmux pane, or role flag. This makes every Manager and Subordinate self-aware; it is not a delegation ACL. `context tree` explains the complete relevant team chain, and `context session` explains another session's place, assignment, state, workspace, and capabilities. The native agent or human—not TaskAI—decides how to use that knowledge.

The canonical shared bootstrap is a small managed block in `AGENTS.md`:

```text
When TaskAI-managed, treat the newest injected TaskAI Context as current environment information.
At each user turn without newly injected context, run `taskai context self`; before delegating, also run `taskai context tree`.
Use `taskai context session <ref>` when more detail about a registered session is needed.
Use TaskAI Context to understand who you are, why this session exists, your assignment, and your place in the team chain.
TaskAI Context informs your decisions; it does not make them for you.
```

On Unix, `CLAUDE.md` may symlink to `AGENTS.md`; use Claude's `@AGENTS.md` import when Claude-specific additions or portable Windows behavior are needed. TaskAI never silently overwrites an existing instruction file, and a shared repo file never contains volatile per-session identity or topology.

| Harness | Static bootstrap | Fresh-context mechanism |
|---|---|---|
| Codex | `AGENTS.md` | `UserPromptSubmit.additionalContext`; refresh on resume/compact with `SessionStart` |
| Claude | `CLAUDE.md` symlink/import | `UserPromptSubmit.additionalContext`; refresh on resume/compact with `SessionStart` |
| OpenCode 1.18.25 | `AGENTS.md` | Version-gated `experimental.chat.system.transform` on each model dispatch |
| Grok 1.0.13 | `AGENTS.md`/`CLAUDE.md` or creation rules | No usable model-visible pre-prompt hook; use the bootstrap command and a TaskAI-routed envelope |

The snapshot is compact and revisioned; the newest revision supersedes copies retained in native history. A connector uses exactly one delivery path per turn. If refresh fails, TaskAI reports context as unavailable or stale; it does not invent topology or make an orchestration decision.

## 6. Session reference and caller-initiated turn

The stable mapping is `{taskai_session_ref, provider, provider_session_id, runtime_id, workspace, connector_version}`. Provider-native history remains the only conversation store.

```text
human UI or registered native session → TaskAI session reference lookup → connector safety check
→ ACP/native prompt to exact target provider session
→ native target CLI live update or declared handoff/resume
→ structured outcome returned to the initiating UI/session
```

TaskAI records whether the source is the human UI or a registered native session and resolves the selected target. It does not use Manager/Subordinate roles as invocation permissions, rank targets, approve delegation, inspect prompt meaning, or apply a social permission graph. UI and native callers use the same underlying conduit.

For each turn TaskAI must:

1. Resolve the exact source, target, provider session, workspace, runtime, and connector version.
2. Decline if another writer is active or the connector cannot safely submit without disturbing native input, tools, questions, or approvals.
3. Submit once, correlate provider session/message/turn identifiers, and expose ordered structured progress.
4. Treat provider-native questions and approvals as native interaction; relay them only as required, without answering or deciding.
5. Report completion only from correlated provider events/history and the connector's proven terminal signal.
6. On a caller-requested cancel, transport the provider's native cancel and report its result.
7. If acknowledgement is uncertain, reconcile native history and never blindly resend.

## 7. Proven connector mechanics

### Codex: shared-live

- Connect the visible Codex CLI and bundled `codex-acp` to the same app-server and exact `threadId`.
- Let the native TUI create the visible thread, capture `thread/started`, ACP-load it, and wait until both clients are attached before prompting.
- Correlate `threadId + turnId`; require matching `turn/completed` and the proven later idle edge.
- The wrapper's Unix-socket/WebSocket/NDJSON shim handles backpressure, close propagation, and explicit reconnect; it never creates another transcript.

### OpenCode: shared-live

- Run one authenticated loopback `opencode serve`; attach the native CLI and TaskAI facade to the same server, directory, and session ID.
- Subscribe before prompting, generate a `messageID`, use `prompt_async`, and match the assistant whose `parentID` equals that ID.
- Require matching completion/error plus the proven session-idle edge; recover event loss from native message snapshots and deduplicate.

### Claude: owned-tui

- Send the caller's ACP-driven prompt atomically into the exact idle native Claude PTY already displayed by TaskAI.
- That same Claude process renders the work. Observe a `Stop` hook or matching native JSONL record for completion.
- Questions and approvals remain in the Claude terminal. Never start a second Claude writer for the visible session.

### Grok: handoff-resume

- Require the native session to be in a connector-proven handoff-safe state, suspend competing input, and run `grok agent stdio` against the same session ID.
- After its structured terminal result, exact-resume the native Grok CLI and label the result `handoff-resume`.
- If safe handoff or exact resume cannot be proven, report `unsupported` rather than simulating live behavior.

## 8. Capability and connector contract

A connector implements `probe, resolve_session, attach_native_tui, install_bootstrap, prompt, subscribe, cancel_transport, snapshot, await_terminal, detach`. It declares its version range, rendering mode, context-delivery mode, native state signals, and collision constraints. Provider CLIs remain user-installed and are never silently modified or upgraded.

TaskAI reports manifest support, handshake verification, and live-test status separately. A live test must prove exact session identity, context visibility before model work, declared native rendering, completion/error, cancel transport, collision refusal, reconnect deduplication, and provider-history integrity. Failed identity or safety is `unsupported`, not a weaker unproven claim.

## 9. Minimum connector safety and packaging

- TaskAI packages its CLI/context service, built-in connectors, and required bridge dependencies; users do not manually operate sidecars.
- Local sockets and runtime directories are owner-only; endpoints bind locally and credentials stay out of argv, prompts, transcripts, and logs.
- Safety state records only dispatch correlation, exact session references, connector/runtime identity, and native recovery locators—not conversation content.
- Writer exclusion exists solely to prevent two clients from corrupting one provider session. It expires at the provider's proven terminal boundary.
- TaskAI may stop or clean up its connector/shim processes. It does not automatically stop, restart, hibernate, or destroy native agent work.
- A user-requested detach removes the viewer and conduit, not the tmux-hosted provider,
  provider history, or repositories. Scoped termination is defined only by the approved
  lifecycle contract in `feature-tmux.md`; further lifecycle expansion still requires
  separate approval under Section 10.

## 10. Explicitly excluded expansions

The following are not v1 responsibilities: autonomous TaskAI-originated turns; delegation policies or agent ACLs; task planning or target selection; schedulers/workflows; central approval or question handling; prompt interpretation; semantic result aggregation; a provider-neutral transcript/message bus; automatic agent restart, hibernation, or destruction; autonomous retries; and portfolio-wide governance.

Adding any of these requires an explicit product decision, a new responsibility classification, and a separate threat/UX analysis. It must not be smuggled in as connector safety or context delivery.

## 11. Release gate

A clean local install requires TaskAI, tmux, and configured provider CLIs. Tests cover fresh native self-awareness, the full team chain, exact session references, correctly attributed UI/native initiation, same-session native rendering or truthful handoff, tools, native questions/approvals, cancel transport, writer collision, terminal detection, replay/reconnect deduplication, bridge failure, provider upgrades, exact resume, and repository/provider-history preservation.

tmux is a production runtime dependency only for process and terminal lifetime. The
retained tmux demo remains rendering evidence, not a provider communication protocol or
an orchestration model.
