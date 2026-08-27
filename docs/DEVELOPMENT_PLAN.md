# Agent Task Console — ordered development plan

This plan replaces the provider-history reconciliation and cross-surface continuity design with one provider Port and one Session Channel per Session.

## Execution rules

- Keep exactly Task and Session as persistent entities.
- Make each milestone leave the tree buildable.
- Prefer deletion over compatibility layers that revive the old architecture.
- Preserve existing user databases, logs, worktrees, and provider state during migration.
- Never edit or renumber a migration that may already have been applied; add a new migration.
- Stop old workers before switching worker protocol. Never delete native-owner or launch files while an old provider process may still exist.
- Keep `acpx` behind `internal/acp`.
- Run focused tests during each milestone and the complete gates at the end.

## M01 — Contract and vocabulary

- Replace the product specification, architecture, API, operations, test, and acceptance documents.
- Define Task, Session, optional parent Session, provider Port, the four channel commands, and the eight mechanical journal frame types.
- Remove committed/provisional, provider-store, history merge, per-prompt refresh, prepared launch, outbox, FIFO, native owner, and review-workflow requirements.
- State that HTTP commands plus SSE are the current logical channel and WebSocket is optional.

Exit criteria:

- documentation contains one coherent architecture;
- no normative document instructs the gateway to parse provider transcripts;
- no normative document requires native/ACP same-Session handoff.

## M02 — Safe Session migration

- Add nullable `parent_session_id` with an index and same-Task service validation.
- Add or designate one opaque `provider_ref` used by the selected adapter.
- Preserve Tasks and Sessions during upgrade.
- Leave old physical columns in place if removing them would require an unsafe table rewrite; stop reading them in new code.
- Treat every nonempty legacy provider conversation field as audit evidence only and clear its physical ProviderRef, regardless of its old source or verification timestamp.
- Preserve a ProviderRef only for Session Channel rows that have no legacy provider conversation field; never guess from ACP/native evidence fields.
- Move ownerless legacy rows in `starting`, `waiting`, or `working` to `stopped`, and never replay their old prepared prompt or outbox.

Tests:

- fresh database still has only Task and Session tables;
- version upgrade preserves rows;
- same-Task parent succeeds;
- cross-Task parent, self-parent, and cycles fail atomically;
- every legacy provider identity is retained only as audit evidence and cannot drive Resume;
- a fresh Session Channel ProviderRef survives migration.

## M03 — Provider Port interface

- Define Start, exact Resume, Send, event stream, and lifecycle Close semantics.
- Adapt `acpx/runtime` behind `internal/acp`.
- Start only when `provider_ref` is empty.
- Resume only on worker/provider transport startup.
- Keep one healthy Port across turns.
- Remove Refresh from prompt dispatch and delete provider-store verification.
- Put every bounded normalized runtime event object verbatim in `provider_event.data`, preserving unknown provider extensions without inventing outer frame types.
- Expose live model/reasoning controls without a hard-coded catalog.
- Keep model/effort selection as disposable live-Port state; do not persist or reapply it.

Tests:

- Start returns and persists one reference;
- Resume receives the exact saved reference once;
- two turns reuse one Port;
- Resume failure never calls Start;
- provider options come from the Port;
- malformed/oversized normalized runtime event objects fail safely.

## M04 — Simplified worker and replay

- Reduce the worker to one Session, one Port, one active turn, commands, permission callbacks, ordered log append, and fan-out.
- Replace the prompt FIFO with a typed busy response.
- Remove fresh-handle eligibility, `acp_rehydrated`, per-prompt identity checks, provider transcript reconciliation, native ownership generations, and launch outbox recovery.
- Store private per-Session JSONL lines as `seq`, optional `request_id`, `timestamp`, `type`, and `data`; bind identity through the canonical `channel-v1` path and validated Session resource. Public API envelopes add that resource's `session_id`.
- Limit `type` to `connection`, `client_command`, `provider_event`, `provider_result`, `permission_requested`, `permission_resolved`, `permission_abandoned`, and `worker_error`.
- Preserve raw legacy logs without treating them as current frames.
- Keep V1 replay corruption checks focused on JSONL framing and sequence continuity; semantic revalidation of manually modified, syntactically valid private lines is post-V1 hardening.
- Keep stale PID/socket detection as retained-ownership quarantine and keep duplicate-worker protection; never clear ownership without proven provider-tree teardown.
- Open the socket/journal, emit `connection` with `data.state == "connecting"`, and accept subscribers before the one provider Start or exact Resume.
- Bound provider startup to 30 seconds; keep Stop available to cancel it and reject provider-dependent commands with typed `session_starting` until `connection.data.state == "connected"`.

Tests:

- prompt streams before completion;
- second prompt is busy and not recorded as accepted;
- cancel targets the active request;
- pending permission survives viewer detach;
- first valid permission response wins;
- a slow subscriber does not block provider work;
- replay plus live fan-out is gap-free and ordered;
- worker restart calls Resume and never replays an uncertain prompt.

## M05 — Shared Session Channel services

- Expose create/open/configure/prompt/cancel/permission/resume/stop through `SessionService`; stop closes the Port as lifecycle cleanup.
- Remove prepare, reconnect-options, launch, merged conversation, native attach, native recovery, and privileged review flows.
- Make a reviewer role an ordinary Session; no server-generated review workflow.
- Keep repository validation and checkout exclusion shared by CLI/API/web.
- Record provider startup failures on the already-created Session and its channel.
- Return Create/Resume once the channel is addressable, with Session status `starting` and no provider options.

Tests:

- every transport calls the same service methods;
- no failed startup creates a duplicate Session;
- subscribe/replay works while provider startup is blocked, and early provider commands reach neither provider nor accepted-command journal;
- busy, stopped, failed, and permission errors are typed and bounded;
- repo suggestions never retarget or mutate;
- shared checkout remains explicit.

## M06 — API transport

- Keep Task and Session metadata endpoints.
- Make Session SSE the ordered mechanical journal stream.
- Add config, prompt, permission response, cancel, resume, and stop operations over HTTP; do not add a separate close endpoint.
- Remove `/conversation`, prepare, provider-options reconnect, launch, native, and review-prepare routes.
- Support replay after numeric Session sequence.
- Keep Task activity as a computed merge of child Session frames.
- Preserve loopback, Host, origin, size, and output-sanitization controls.
- Optionally add a WebSocket later using the same commands and frames.

Tests:

- attach stream before prompt and observe the first delta immediately;
- replay cursor has no gap/duplicate within retention;
- HTTP busy maps to conflict without provider dispatch;
- browser DTOs omit provider references and runtime secrets;
- stopping the API server leaves workers untouched.

## M07 — Structured terminal and web

- Render one channel replay plus live frames; remove provider snapshot replacement and source badges.
- Coalesce adjacent deltas only as local presentation.
- Populate model and effort selectors from the observational provider-options response.
- Connect before enabling Send.
- Preserve scroll/follow, focus, draft/cursor, text selection, selected Session, and expanded tool/reasoning state.
- Keep permissions pinned and actionable.
- Make structured terminal the managed Session attachment.
- Remove managed provider-native handoff. Document direct provider CLI use as unmanaged.
- Retain responsive desktop/mobile layouts and accessible semantics.

Tests:

- no initial history fetch delays the stream;
- streamed text appears before terminal result;
- config choices are never hard-coded;
- detach does not mutate the worker;
- live updates preserve viewer state;
- terminal and browser reducers consume the same frame contract.

## M08 — Manager and child composition

- Implement nullable parent Session creation through the shared service.
- Expose human/operator API, web, and terminal controls to create and list direct children and to prompt or cancel an exact child.
- Require exact same-Task child IDs.
- Keep each child's frames on its own Session Channel; do not add parent wait or result routing.
- Keep provider-facing manager tools, wait/result mapping, peer messaging, and offline mail as explicit post-V1 integrations.

Tests:

- operator creates and lists multiple children under a parent in the same Task;
- operator child routes cannot address a Session from another Task;
- prompt and cancel use exact child IDs;
- parallel children stream independently;
- no provider manager tool or implicit child wait/result route exists;
- schema still contains only Task and Session tables.

## M09 — Legacy code removal

Delete or retire:

- provider transcript readers and verifiers;
- semantic conversation merge/projection services;
- committed/provisional and surface inference;
- provider-store reconciliation events and retry loops;
- per-prompt Refresh/Resume and identity drift machinery;
- staged prepare/launch metadata, endpoints, outboxes, and recovery;
- gateway ACP FIFO;
- managed native attach, tmux ownership generations, and recovery;
- privileged review preparation and evidence injection.

Do not delete user provider stores, auth, Session logs, worktrees, branches, or live processes.

## M10 — Hardening and release

- Bound frame data, replay, Task aggregation, browser state, and subscriber buffers.
- Verify private permissions on state, logs, sockets, and bridge files.
- Verify literal argv and environment redaction.
- Exercise worker crash, provider disconnect, replay gap, permission abandonment, and repo drift.
- Update README and operations examples to match implemented commands.
- Confirm no obsolete vocabulary remains in normative docs or production UI.

Run:

```bash
make fmt-check
make test
make race
make vet
make build
make security-check
make acceptance
```

## Definition of complete

A release candidate is complete when:

1. A user creates a Task and Session in an existing repository.
2. The Session connects one provider Port and exposes its advertised controls through an observational read.
3. Web attaches before prompt and renders provider output immediately.
4. A second prompt receives busy rather than entering a queue.
5. Closing every viewer leaves work running.
6. Reconnecting replays ordered channel frames and resumes live output.
7. Restarting the worker exactly resumes the saved provider reference once.
8. A human/operator can create and list child Sessions, then prompt or cancel an exact same-Task child.
9. No provider transcript parser, merge layer, staged launch, per-prompt refresh, or managed native handoff remains.
10. SQLite has only Task and Session product tables and all verification gates pass.
