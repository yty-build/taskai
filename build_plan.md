# taskai — clean-slate ACP manager workspace build plan

**Status:** Worker-ready plan v1.0  
**Date:** 2026-08-26  
**Product contract:** docs/UX_PRODUCT_VISION.md  
**Verification contract:** docs/TEST_PLAN.md  
**Embedded vision SHA-256:** bd36020b73a1184c958d7d098cd7f51f0683fbc00dd62da0424990a857070a51  
**Execution rule:** Workers execute this plan in milestone order after the repository safety checkpoint. A failed gate stops dependent work; it never authorizes a workaround outside the product contract.

## 1. Outcome

Replace the current Agent Task Console / ACP gateway implementation with a clean terminal application named taskai.

The finished product has this loop:

~~~text
taskai
  -> Tasks opens immediately
  -> Settings enables and authenticates providers once
  -> Add Session creates an opinionated Manager Session
  -> the manager delegates to provider-specific subordinate Sessions over ACP
  -> Answer returns only the current answers the manager requested
  -> Follow exposes one current subordinate invocation live
  -> replay means replay 1; replay N is explicit and bounded
  -> the manager synthesizes one response for the user
~~~

TaskAI supplies lifecycle, walls, metadata, and terminal presentation. Agents supply decomposition, execution, inspection decisions, and synthesis.

## 2. Authority and conflict resolution

Workers must use the following precedence:

1. The latest explicit product-owner instruction.
2. docs/UX_PRODUCT_VISION.md.
3. The embedded byte-for-byte copy in section 18 of this plan.
4. This plan's architecture, milestones, gates, and work-packet boundaries.
5. docs/TEST_PLAN.md for verification details that do not alter the product or architecture.
6. Replacement AGENTS.md contributor instructions created in M01.
7. Implementation details selected inside an approved milestone.

If the source vision and embedded copy differ, stop. Refresh the plan and hash before implementation continues.

The existing agent_task_console_spec.md, docs/DEVELOPMENT_PLAN.md, and current production code are legacy evidence, not product authority.

## 3. Non-negotiable product invariants

1. Keep exactly two persistent first-class entities: Task and Session.
2. A Session belongs to exactly one Task.
3. A user-created Session is a Manager Session.
4. A subordinate Session names exactly one same-Task Manager Session.
5. Subordinate depth is one in V1.
6. Every provider interaction uses ACP.
7. The manager owns downstream subordinate ACP connections.
8. TaskAI never semantically coordinates manager/subordinate work.
9. Answer is the default.
10. Answer supplies only current requested subordinate answers.
11. Follow is explicit, applies to one active invocation, and resets on completion.
12. Replay is explicit and post-answer only.
13. Replay without N means replay 1.
14. Replay N applies independently to every explicitly named subordinate Session.
15. There is no Recall operation, automatic history insertion, or whole-Session replay workflow.
16. Replay is connector-capability-gated; TaskAI never emulates it with a transcript database.
17. Provider authentication is initiated only from Settings and reused afterward.
18. Tasks is always the first screen, including first launch.
19. Detaching the terminal viewer does not stop runtime work.
20. TaskAI persists no prompt, answer, tool, plan, replay, transcript, token stream, or provider credential content.
21. No MCP, tmux, provider-native embedded terminal, web UI, or HTTP API exists in this build.
22. No custom TaskAI manager protocol, semantic message envelope, workflow engine, broker, or mailbox may be introduced.
23. No provider-private transcript store may be crawled or inferred.
24. No automatic repository initialization, retargeting, worktree creation, merge, or cleanup is product behavior.

## 4. Gate G0 — prove the architecture before cleanup

The central risk is not the terminal UI. It is whether a real manager connector can satisfy the required dual ACP role without TaskAI becoming the orchestrator.

No legacy deletion or feature implementation begins until G0 passes.

### 4.1 Required proof

Using a disposable external fixture or temporary spike that is not retained as production architecture, prove:

1. A manager runtime can act as an ACP agent toward a client.
2. The same manager runtime can act as an ACP client toward at least one real subordinate connector.
3. The manager can create or own the subordinate ACP Session without TaskAI parsing a manager answer into an instruction.
4. The manager can send a subordinate prompt through ACP.
5. Answer can withhold intermediate subordinate activity from manager context and expose only the current completed public answer plus native completion state.
6. Two concurrent Answer subordinates can return two independently correlated current answers.
7. Follow can expose one invocation's ACP frames live and automatically reset afterward.
8. At least one connector can satisfy replay 1 and replay N through provider-owned ACP replay or opaque completed-turn cursors, without TaskAI storing content.
9. The terminal/client side can detach and reattach without becoming the runtime owner.
10. Provider authentication can be completed once and reused.

### 4.2 Forbidden proof shortcuts

G0 fails if the proof depends on:

- MCP;
- a TaskAI tool presented to the manager;
- parsing manager prose or JSON to discover delegation;
- TaskAI-generated follow-up prompts;
- a TaskAI transcript or tool-call database;
- tmux, terminal scraping, or provider-native TUI automation;
- provider-private transcript files;
- acpx flow or another workflow engine acting as the hidden manager;
- a custom agent-to-agent protocol presented as ACP;
- hard-coded capability claims that were not exercised.

### 4.3 Dependency selection

G0 must record the supported ACP implementation boundary:

- selected ACP version and SDK or connector runtime;
- public API used;
- process and transport ownership;
- manager-capable connector requirements;
- subordinate connector requirements;
- exact resume behavior;
- Answer completion/message-boundary behavior;
- Follow event behavior;
- replay cursor/range behavior;
- authentication probe behavior;
- unsupported capabilities.

acpx may be selected only through its supported public surface and only after conformance. Do not reproduce the current private runtime shape checks, hidden-client interception, or version-specific compatibility shims.

### 4.4 G0 exit

Produce a short, evidence-backed connector conformance note and executable contract tests. At least one real connector must be manager-capable and at least one real connector must be subordinate-capable. Replay may be available only on a subset, but at least one real replay-capable path is required to ship Replay.

If G0 fails, mark the build blocked and return the evidence to the product owner. Do not proceed with a different architecture.

## 5. Target architecture

~~~text
┌─────────────────────────────────────────────────────────────────────────────┐
│ taskai terminal client                                                      │
│ Tasks · Settings · Task workspace · manager ACP rendering                  │
└───────────────────────────────┬─────────────────────────────────────────────┘
                                │ ACP
┌───────────────────────────────▼─────────────────────────────────────────────┐
│ Manager Session runtime                                                     │
│ provider manager conversation · subordinate ownership · Answer/Follow      │
│ bounded replay operation · no TaskAI semantic coordinator                  │
└───────────────┬────────────────────┬────────────────────┬───────────────────┘
                │ ACP                │ ACP                │ ACP
┌───────────────▼──────┐ ┌───────────▼──────────┐ ┌──────▼──────────────────┐
│ Subordinate Session A │ │ Subordinate Session B │ │ Subordinate Session C   │
│ provider-owned state  │ │ provider-owned state  │ │ provider-owned state    │
└───────────────────────┘ └───────────────────────┘ └─────────────────────────┘
~~~

### 5.1 Responsibility split

TaskAI core owns:

- Task and Session metadata;
- provider Settings and enabled selection;
- lifecycle supervision;
- exact parent/child Session placement;
- safe process launch and stop;
- attachment and detachment;
- mechanical status;
- terminal presentation of the manager's public ACP surface.

The manager-capable connector owns:

- making the selected provider behave as the manager;
- downstream ACP client behavior;
- deciding delegation;
- choosing subordinate providers;
- Answer and Follow consumption;
- explicit replay 1/replay N;
- giving subordinate output to the manager model;
- synthesis.

The subordinate connector owns:

- provider process and conversation;
- tools and execution;
- public ACP updates;
- native completion;
- exact resume and replay only when advertised.

### 5.2 Content boundary

Core code may inspect only ACP and runtime envelope fields required for operation:

- JSON-RPC ID and method;
- Session and message IDs;
- connection and lifecycle state;
- stop reason;
- opaque provider reference;
- byte/frame sizes needed for safety.

Core code must not:

- classify tool meaning;
- determine whether work succeeded;
- select or summarize subordinate content;
- merge answers;
- create result objects;
- index tools;
- retain content for later search;
- infer provider memory.

Unknown ACP content crosses the connector boundary without a semantic projection layer.

## 6. Persistent model and storage

### 6.1 Task

Required fields:

- ID;
- title;
- created time;
- updated time only when mechanically needed.

Task ordering uses created time descending.

### 6.2 Session

Required fields:

- ID;
- Task ID;
- nullable parent Session ID;
- role: manager or subordinate;
- label;
- provider ID;
- opaque provider Session reference when available;
- mechanical state;
- created time;
- last-active time.

Constraints:

- parent and child belong to the same Task;
- a manager has no parent;
- a subordinate must have a manager parent;
- a subordinate cannot be a parent;
- provider reference is opaque;
- role cannot be silently changed;
- deletion never deletes provider-owned state implicitly.

### 6.3 Provider Settings

Provider Settings are configuration, not a third first-class entity. Store:

- provider ID;
- enabled flag;
- non-secret launch/configuration values;
- last mechanical probe status;
- conformance-derived capability flags.

Never store credentials, tokens, cookies, provider auth databases, prompts, outputs, or transcript paths.

### 6.4 Prohibited schemas

Reject migrations or storage code introducing tables or durable records for:

- runs or invocations;
- events or frames;
- messages or answers;
- tools or tool results;
- transcripts;
- replay content;
- artifacts;
- workflows;
- teams;
- mailboxes;
- permissions;
- token streams.

## 7. Target repository shape

The exact package names may be refined during M01, but responsibility boundaries must remain:

~~~text
.
├── AGENTS.md
├── README.md
├── build_plan.md
├── docs/
│   ├── UX_PRODUCT_VISION.md
│   └── TEST_PLAN.md
├── cmd/
│   └── taskai/
│       └── main.go
├── internal/
│   ├── app/             # composition only
│   ├── acp/             # thin standard ACP transport/types
│   ├── connector/       # provider launch, auth probe, capability conformance
│   ├── manager/         # manager-capable runtime boundary
│   ├── runtime/         # process, attach/detach, exact lifecycle supervision
│   ├── settings/        # enabled providers and non-secret config
│   ├── store/           # Task and Session metadata only
│   ├── task/            # Task rules
│   ├── session/         # Session hierarchy and lifecycle rules
│   ├── tui/             # Tasks, Settings, Task workspace
│   └── testkit/         # fake ACP agents and deterministic fixtures
├── scripts/
│   ├── acceptance.sh
│   └── check-repository.sh
├── Makefile
├── go.mod
└── go.sum
~~~

Forbidden production package concepts include web, api, broker, workflow, mailbox, transcript, sessionlog, native, tmux, presentation projection, semantic render, and MCP.

## 8. Clean-slate removal manifest

The existing worktree contains extensive modified and untracked work. Cleanup must be recoverable and exact.

### 8.1 Preserve

- .git and repository history;
- docs/UX_PRODUCT_VISION.md;
- docs/TEST_PLAN.md;
- build_plan.md;
- any explicit product-owner files outside the approved manifest;
- external provider state and authentication;
- linked worktrees, branches, tags, stashes, and remotes.

### 8.2 Replace from scratch

- AGENTS.md;
- README.md;
- .gitignore;
- Makefile;
- go.mod and go.sum as selected dependencies require;
- scripts/acceptance.sh;
- scripts/check-repository.sh.

The repository module path may remain github.com/yty-build/acp-gateway until a separate rename is approved. The binary and product name are taskai.

### 8.3 Delete after the safety checkpoint

- agent_task_console_spec.md;
- legacy docs other than docs/UX_PRODUCT_VISION.md and docs/TEST_PLAN.md;
- cmd/acpctl and obsolete command trees;
- the current internal tree in full;
- old SQLite migrations and fixtures;
- current web, API, worker, channel, native, tmux, transcript, sessionlog, render, and compatibility code;
- committed or generated legacy binaries and test binaries inside the checkout;
- obsolete acceptance fixtures and scripts after their replacements exist.

Nothing from the old production tree is copied into the new tree. A behavior may be reimplemented only from this contract and must receive a new test.

### 8.4 Never delete or mutate automatically

- .claude/settings.local.json or other user-local configuration;
- provider credential stores;
- provider auth state;
- databases, logs, sockets, or workspaces outside this repository;
- live provider processes;
- Git history, branches, remotes, stashes, or linked worktrees;
- untracked user files not explicitly classified at the safety checkpoint.

The new app uses a distinct taskai state namespace and never imports, rewrites, or deletes legacy state automatically.

## 9. Cleanup safety procedure

Before M01:

1. Save the exact git status inventory.
2. Classify every modified, deleted, and untracked path.
3. Create a named recoverable checkpoint through a commit/branch or another product-owner-approved method.
4. Confirm every deletion target resolves inside this repository.
5. Confirm no live external provider process or state is part of source cleanup.
6. Confirm the source and embedded vision hashes match.

During M01:

1. Apply only the approved manifest.
2. Use exact paths; do not use a broad unresolved recursive target.
3. Separate deletion/scaffold from feature implementation.
4. Confirm all removed source remains recoverable from Git.
5. Report what was removed and how it can be recovered.

## 10. Ordered milestones

### M00 — contract freeze and connector feasibility

**Depends on:** nothing  
**Owner:** integration/architecture owner  
**Produces:** G0 evidence, connector conformance tests, dependency decision

Work:

- verify the embedded vision hash;
- run the dual-role manager proof;
- prove Answer, two concurrent answers, Follow reset, replay 1/N, auth reuse, detach;
- identify manager-capable and subordinate-capable connectors truthfully;
- select only public supported ACP surfaces;
- document unsupported cases.

Exit:

- G0 passes;
- no forbidden shortcut was used;
- the product owner can see which real providers qualify for each capability.

### M01 — recoverable clean baseline

**Depends on:** M00  
**Owner:** integration owner only  
**Produces:** minimal compiling taskai scaffold

Work:

- complete the repository safety checkpoint;
- apply the removal manifest;
- create cmd/taskai and empty responsibility packages;
- replace AGENTS.md and README.md;
- replace Makefile and repository checks;
- set the new taskai state namespace;
- retain no legacy compatibility layer.

Exit:

- taskai builds and prints a minimal version/help surface;
- old commands and packages are absent;
- no external state changed;
- all required Make targets exist, even if feature acceptance is initially skeletal.

### M02 — Task, Session, Settings, and metadata store

**Depends on:** M01  
**Primary paths:** internal/task, internal/session, internal/settings, internal/store

Work:

- implement Task and Session structs and validation;
- implement same-Task parent and role constraints;
- implement newest-created Task queries;
- implement active/dormant mechanical aggregation;
- implement provider enabled selection and non-secret config;
- implement metadata-only SQLite schema;
- use restrictive file permissions;
- prove prohibited content never reaches persistence.

Exit:

- unit and migration tests pass;
- schema contains only Task, Session, and configuration mechanics;
- storage audit finds no content fields.

### M03 — thin ACP transport and connector conformance

**Depends on:** M01 and G0 dependency selection  
**Primary paths:** internal/acp, internal/connector, internal/testkit

Work:

- implement the smallest selected public ACP client/agent surface;
- preserve standard ACP ordering and unknown shapes;
- implement literal provider command launch without shell interpolation;
- implement auth action and status probe used only by Settings;
- expose conformance-derived Manager, Subordinate, Resume, and Replay flags;
- keep provider-specific behavior inside connector packages;
- implement fake deterministic ACP agents.

Exit:

- transport contract tests pass under race detection;
- no normalized semantic event model exists;
- no private provider transcript or private acpx hook is used;
- unknown ACP updates survive the boundary.

### M04 — Manager Session runtime

**Depends on:** M02 and M03  
**Primary paths:** internal/manager, internal/runtime

Work:

- implement one live Manager runtime per active Manager Session;
- keep the manager provider conversation connected across turns;
- allow the manager-capable connector to own subordinate ACP ports;
- record subordinate Session metadata as a mechanical consequence of port creation;
- enforce one parent, same Task, and depth one;
- prevent peer or user-directed subordinate output;
- keep TaskAI out of manager delegation decisions.

Exit:

- one real manager can own at least two differently identified subordinate Sessions;
- TaskAI does not parse manager prose or issue subordinate prompts;
- stopping the manager stops or interrupts its exact owned runtime tree;
- detaching the viewer does not.

### M05 — Answer, Follow, and Replay

**Depends on:** M04  
**Primary paths:** internal/manager, internal/connector, internal/testkit

Work:

- make Answer the default for every subordinate invocation;
- deliver only each current requested completed answer to the manager;
- correlate simultaneous answers by subordinate Session and prompt turn;
- implement Follow for one active invocation;
- reset Follow to Answer on every terminal completion path;
- implement one explicit replay operation with default N equals 1;
- support one or multiple explicitly named subordinate Sessions;
- apply N per target Session;
- forbid replay before completion and whole-Session replay;
- expose Replay unsupported rather than building a fallback;
- retain no TaskAI answer or transcript buffer.

Exit:

- deterministic tests cover concurrent answers, failure, cancellation, Follow reset, replay 1, replay N, multi-target replay, and unsupported replay;
- ACP-501 through ACP-510 in docs/TEST_PLAN.md pass;
- no prior answer is inserted unless explicitly replayed;
- no Recall operation exists.

### M06 — persistent runtime ownership, detach, and exact resume

**Depends on:** M04  
**Primary paths:** internal/runtime, internal/app

Work:

- make runtime lifetime independent from terminal attachment;
- implement local attach/detach transport with restrictive permissions;
- preserve one live provider port per active Session;
- stop only through exact explicit lifecycle actions;
- exact-resume only when advertised;
- mark interrupted state honestly after process failure;
- never reconstruct provider memory from UI output.

Exit:

- closing the terminal viewer leaves work running;
- reattachment observes the same live Manager runtime;
- stop targets exactly one Manager runtime tree;
- UX-701 through UX-703 and ACP-704 in docs/TEST_PLAN.md pass;
- failed resume never falls back to a new conversation.

### M07 — terminal Tasks and Provider Settings

**Depends on:** M02 and M03  
**Primary paths:** internal/tui

Work:

- make Tasks the unconditional launch screen;
- implement Active Tasks and All Tasks;
- sort by Task creation time descending;
- show active/dormant Session counts;
- implement Settings provider enable/disable;
- implement explicit provider auth and recheck actions;
- show truthful connector capability labels;
- route Add Session to Settings when no manager provider is enabled;
- never open login implicitly.

Exit:

- visual/model tests cover empty first launch, Settings, enabled providers, and task ordering;
- UX-101 through UX-205, UX-301, UX-303, and applicable UI-001 through UI-007 in docs/TEST_PLAN.md pass;
- provider secrets never render;
- first launch has no wizard.

### M08 — Task workspace and manager ACP rendering

**Depends on:** M04, M05, M06, and M07  
**Primary paths:** internal/tui

Work:

- render Manager Sessions with indented subordinates;
- show provider, label, role, mode, and mechanical state;
- render the selected manager's public ACP content as it arrives;
- keep TaskAI navigation separate from provider content;
- provide bounded fallback rendering for unknown public content;
- prohibit direct user prompts to subordinates;
- ensure keyboard and mouse parity where supported;
- preserve backpressure and ordering.

Exit:

- the agreed Task workspace is usable at narrow and standard terminal sizes;
- UX-302, UX-304, UX-401 through UX-407, and UI-601 through UI-606 in docs/TEST_PLAN.md pass;
- no tmux, ANSI scrape, native provider TUI, or second writer exists;
- subordinate selection cannot bypass its manager.

### M09 — failure, privacy, and security hardening

**Depends on:** M02 through M08  
**Primary paths:** all new production packages and scripts

Work:

- handle auth expiry, connector absence, non-success stop reasons, process crash, replay unsupported, and malformed/oversized traffic;
- use literal argv and bounded frames;
- redact environment and diagnostics;
- lock state directories, sockets, and databases to the user;
- ensure no credential or content persistence;
- ensure unknown frames cannot escape bounds;
- ensure cancellation and stop cannot target another Task or manager tree.

Exit:

- race, vet, security, fault-injection, and storage audits pass;
- UX-705, the failure-state matrix, and SEC-801 through SEC-804 in docs/TEST_PLAN.md pass;
- external state is never deleted;
- errors remain useful without leaking secrets or provider-private data.

### M10 — end-to-end acceptance and release handoff

**Depends on:** M09  
**Owner:** integration owner  
**Produces:** releasable taskai binary and final evidence

Work:

- run every applicable test ID and acceptance scenario from docs/TEST_PLAN.md;
- review the TaskAI chrome goldens and terminal-size matrix required by the test plan;
- run fake-provider acceptance in CI;
- run opt-in real-provider smoke for every advertised connector capability;
- refresh README and AGENTS.md from implemented behavior;
- verify build plan/vision hash;
- verify forbidden code and dependency scan;
- verify clean repository artifacts.

Exit:

- all gates G1 through G4 pass;
- taskai is the only shipped command;
- no legacy product path remains;
- product owner receives capability matrix, test evidence, and known unsupported providers.

## 11. Worker packets and ownership

Workers may execute independent packets only after M01. Use explicit isolated Git worktrees for contributor coordination if desired; this is a development practice, not automatic TaskAI product behavior.

| Packet | Milestones | Owned paths | Must not edit |
| --- | --- | --- | --- |
| Integration | M00, M01, M10 | root files, docs contracts, cmd/taskai, app composition | feature internals without owner review |
| Domain | M02 | internal/task, internal/session, internal/store | ACP or TUI |
| Settings | M02, M07 | internal/settings and Settings TUI components | manager runtime |
| ACP connectors | M03 | internal/acp, internal/connector, connector fixtures | Task/Session schema |
| Manager runtime | M04–M06 | internal/manager, internal/runtime | TUI presentation |
| Terminal UX | M07–M08 | internal/tui | connector internals |
| Verification | M02–M10 | internal/testkit, acceptance/security scripts by coordination | product and verification contracts |

Coordination rules:

1. One integration owner controls go.mod, go.sum, Makefile, AGENTS.md, README.md, shared app wiring, and milestone merges.
2. Every worker receives this entire plan, the embedded vision, and docs/TEST_PLAN.md.
3. Workers modify only owned paths unless the integration owner explicitly reassigns a shared file.
4. A packet cannot weaken a gate to make its tests pass.
5. A connector worker cannot claim a capability without its conformance suite.
6. A TUI worker uses fake ports and does not invent backend behavior.
7. A runtime worker exposes mechanical state only and does not create a semantic event model.
8. Product contract edits stop all dependent packets until reapproved and re-embedded.
9. Test-contract edits require integration-owner review and may not weaken an approved product invariant or release gate.

## 12. Dependency and integration order

~~~text
M00 feasibility
   |
M01 clean scaffold
   |
   +----------+-----------+
   |          |           |
 M02        M03       M07 shell
 domain     ACP       Tasks/Settings
   |          |
   +-----> M04 manager runtime
               |
          +----+----+
          |         |
        M05       M06
 Answer/Follow   detach/resume
          |         |
          +----+----+
               |
             M08 workspace
               |
             M09 hardening
               |
             M10 acceptance
~~~

Integration owner merges only when the incoming packet's own tests and all previously completed gates pass.

## 13. Verification strategy

docs/TEST_PLAN.md is the executable verification contract. It defines the required UI elements, semantic assertions, golden terminal states, size matrix, keyboard/mouse coverage, ACP behavior IDs, deterministic PTY scenarios, privacy audits, and release evidence. The sections below are its build-level summary; if they differ, use the precedence in section 2 and repair the documents before merging.

### 13.1 Unit tests

Cover:

- Task ordering;
- Session role and same-Task parent validation;
- no subordinate nesting;
- provider Settings;
- capability derivation;
- Answer default;
- Follow reset;
- replay target/range validation;
- process state transitions;
- literal argv and bounds;
- rendering models.

### 13.2 Contract tests

Every connector capability has an executable contract:

- Manager dual role;
- Subordinate ACP prompt lifecycle;
- Resume exactness;
- Replay 1;
- Replay N;
- multi-target replay;
- auth action and status;
- unknown ACP frame fidelity;
- no hidden new conversation on resume failure.

### 13.3 Integration tests

Use deterministic fake ACP agents to prove:

- two and three concurrent subordinate answers;
- no intermediate Answer leakage;
- Follow live stream and reset;
- cancellation and failure;
- replay only after completion;
- N applied per Session;
- detach/reattach;
- manager stop ownership;
- capability-disabled paths.

### 13.4 Persistence audit

After realistic acceptance flows, inspect all TaskAI files and database columns. Fail if any stored value contains:

- user prompts;
- manager or subordinate answers;
- tool names, inputs, or outputs;
- agent thoughts or plans;
- replay frames;
- credentials or auth-state material;
- provider-private transcript paths.

### 13.5 Repository scans

Production code fails the repository check when it contains or imports:

- MCP dependencies;
- tmux or terminal-capture integration;
- HTTP/web server packages;
- transcript or message persistence;
- workflow/broker/mailbox systems;
- provider-private history readers;
- old acpctl command paths;
- legacy compatibility shims.

Allow product documents to name forbidden concepts in non-goal and test assertions.

### 13.6 Required commands

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

Real-provider smoke tests remain opt-in and must never expose credentials in CI logs.

## 14. Release gates

### G1 — clean architecture

- only Task and Session are persistent entities;
- old production implementation is absent;
- no forbidden dependencies or packages;
- source/embedded UX hashes match.

### G2 — connector truth

- every visible capability is backed by conformance tests;
- at least one Manager connector and one Subordinate connector work end to end;
- at least one real Replay-capable connector passes replay 1 and N;
- unsupported connectors are labeled honestly.

### G3 — product behavior

- Tasks-first and Settings flows pass;
- required TaskAI screens pass semantic, golden, keyboard, mouse, resize, and PTY coverage from docs/TEST_PLAN.md;
- Manager Session creation passes;
- multiple requested Answer responses pass;
- Follow and automatic reset pass;
- replay 1, replay N, and multi-target replay pass;
- detach and exact stop/resume behavior pass.

### G4 — privacy and operations

- persistence audit is clean;
- auth reuse works without implicit login;
- no provider-private history is read;
- no external state is deleted;
- all required commands pass;
- release binary is taskai only.

## 15. Anti-debt review checklist

Reject a change when any answer is yes:

- Did TaskAI interpret manager prose to trigger a child?
- Did TaskAI send a manager-generated subordinate prompt?
- Did core code classify or summarize tool output?
- Did a new durable entity or content table appear?
- Did Answer expose an older or unrequested response?
- Did Follow survive into the next invocation?
- Did replay run automatically or before completion?
- Did replay use whole-Session history?
- Did replay unsupported silently fall back?
- Did a provider capability get hard-coded without conformance?
- Did a connector read a provider-private transcript?
- Did a terminal or tmux surface become a protocol?
- Did MCP, a custom message envelope, or a workflow engine enter the design?
- Did viewer detach change runtime ownership?
- Did a failed resume start a different conversation?
- Did product code initialize, retarget, merge, or delete a repository automatically?

## 16. Definition of done

The build is complete only when:

1. Every applicable test ID and vision acceptance scenario in docs/TEST_PLAN.md passes.
2. G0 through G4 pass.
3. All required Make commands pass from a clean checkout.
4. The source vision and embedded copy are byte-for-byte identical.
5. TaskAI persistence contains metadata only.
6. At least one real Manager/Subordinate path works through ACP only.
7. Answer, Follow, replay 1, and replay N work as specified.
8. Settings is the only authentication surface.
9. Tasks is always the launch screen.
10. No legacy command, web surface, MCP, tmux, transcript service, or semantic coordinator remains.
11. All removed source remains recoverable from Git history.
12. The product owner receives a concise capability and limitation report.
13. TaskAI-owned chrome has reviewed goldens and semantic interaction evidence at the required terminal sizes.

## 17. Worker handoff template

Every worker reports:

~~~text
Packet:
Milestone:
Owned paths changed:
Behavior completed:
Contract decisions used:
Tests added:
Commands run:
Results:
Known unsupported capabilities:
Security/privacy observations:
Potential contract conflict:
Commit or patch reference:
~~~

Workers stop and escalate instead of making a local product decision when:

- a real connector cannot satisfy the advertised capability;
- ACP cannot provide a required behavior without a forbidden layer;
- a migration would persist content;
- another packet must change a shared contract;
- source and embedded vision differ;
- cleanup safety preconditions are not satisfied.

## 18. Embedded UX product vision

The following block must remain byte-for-byte identical to docs/UX_PRODUCT_VISION.md. Its SHA-256 is recorded in this plan header.

<!-- UX_PRODUCT_VISION_EMBED_START -->
# taskai — ACP manager workspace product vision

**Status:** Approved product vision v1.0  
**Date:** 2026-08-26  
**Current product surface:** Terminal application  
**Portable boundary:** The same ACP-facing manager surface may support a future web or desktop client. Those clients are not part of this build.

## 1. Product in one sentence

TaskAI is a persistent terminal workspace in which every user-created Session is an opinionated manager that delegates work to provider-specific subordinate Sessions over ACP, receives only the answers it requested by default, and may explicitly follow or replay bounded subordinate work without TaskAI interpreting the conversation.

## 2. North star

TaskAI rents the space and builds the walls.

The agents do the work:

~~~text
User
  |
  | ACP
  v
Manager Session
  |
  | ACP only
  +----> Subordinate Session · Claude
  +----> Subordinate Session · Codex
  +----> Subordinate Session · Gemini
~~~

TaskAI owns placement, connection lifecycle, provider configuration, Session metadata, and terminal presentation. It does not decide how work is decomposed, interpret tool calls, summarize subordinate output, invent result messages, or coordinate agents semantically.

## 3. Product model

There are exactly two persistent first-class entities:

| Entity | Meaning |
| --- | --- |
| Task | A durable body of work containing zero or more Sessions |
| Session | One provider-owned conversation belonging to exactly one Task |

A Session has one of two roles:

| Role | Creation | Responsibility |
| --- | --- | --- |
| Manager | Created directly by the user through Add Session | Delegates, optionally follows or replays, synthesizes, and answers the user |
| Subordinate | Created by a Manager Session | Performs one or more manager-assigned ACP invocations and responds only to that manager |

A subordinate names exactly one parent Manager Session in the same Task. A subordinate cannot create another subordinate in V1. There is no Team, Run, Invocation, Event, Tool, Answer, Result, Transcript, Artifact, Workflow, Broker, or Mailbox entity.

One ACP prompt turn may be described as an invocation in this document, but an invocation is only a transient protocol boundary. It is not persisted as a product entity.

## 4. Provider roles

TaskAI distinguishes connector capabilities without inventing a cross-provider behavior layer:

| Capability | Meaning |
| --- | --- |
| Manager | The connector's manager runtime can act as an ACP agent toward TaskAI and as an ACP client toward subordinate agents |
| Subordinate | The connector can run as an ACP agent and accept manager-owned ACP Sessions |
| Resume | The connector can exactly resume its provider-owned Session |
| Replay | The manager runtime can request bounded completed-invocation replay from this connector without a TaskAI transcript database |

A provider may be subordinate-capable without being manager-capable. TaskAI never adds provider-specific semantic workarounds to make a subordinate-only connector appear manager-capable.

## 5. Launch behavior

Every launch, including the first launch, opens the Tasks screen. There is no forced setup wizard.

If no provider is enabled, Tasks remains usable and shows a non-blocking path to Settings:

~~~text
┌─ taskai · Tasks ────────────────────────────────────────────────────────────┐
│                                                                            │
│ + Add Task                                                   Settings      │
│                                                                            │
│ No providers enabled. Open Settings to select providers.                   │
│                                                                            │
└────────────────────────────────────────────────────────────────────────────┘
~~~

Attempting to add a Manager Session without an enabled manager-capable provider opens or points to Provider Settings. It never starts a surprise login flow inside the Task workspace.

## 6. Provider Settings

Provider selection and authentication live under Settings, beyond the first screen.

~~~text
┌─ taskai · Settings · Providers ─────────────────────────────────────────────┐
│                                                                            │
│ ENABLED   PROVIDER       STATUS          CAPABILITIES                       │
│ [✓]       Codex          Connected       Manager · Subordinate · Resume    │
│ [✓]       Claude Code    Connected       Subordinate · Resume · Replay     │
│ [ ]       OpenCode       Not connected   Subordinate                       │
│ [✓]       Gemini         Connected       Subordinate · Resume · Replay     │
│ [ ]       Grok           Not connected   Subordinate                       │
│                                                                            │
│ Enter configure     Space enable/disable     r recheck     Esc back         │
└────────────────────────────────────────────────────────────────────────────┘
~~~

Settings behavior:

1. The user explicitly enables the providers TaskAI may use.
2. Authentication is started only from Settings.
3. Authentication remains provider-owned.
4. TaskAI stores only non-secret connector configuration and opaque provider references required to launch or resume.
5. Once connected, ordinary Task and Session navigation never requests login again.
6. If a provider invalidates its credentials, Settings marks it Disconnected or Needs repair.
7. Capability labels are derived from connector conformance, not a hard-coded marketing claim.
8. Disabling a provider prevents new Sessions but does not delete historical Session metadata or provider state.

Credentials, provider authentication databases, tokens, cookies, and private provider transcripts never enter TaskAI storage.

## 7. Tasks screen

The Tasks screen is the normal home screen.

~~~text
┌─ taskai · Active Tasks ─────────────────────────────────────────────────────┐
│                                                                            │
│ + Add Task                                                   Settings      │
│                                                                            │
│ TASK                                      CREATED       ACTIVE   DORMANT   │
│ Fix authentication regression            Aug 26 09:42       4         2   │
│ Prepare release candidate                 Aug 25 16:18       2         1   │
│ Investigate database latency              Aug 24 11:03       1         4   │
│                                                                            │
│ sort: Created · newest first                  a All Tasks                  │
└────────────────────────────────────────────────────────────────────────────┘
~~~

Rules:

- The default view is Active Tasks.
- Tasks are sorted by Task creation time, newest first.
- All Tasks reveals dormant and historical Tasks without changing the sort rule.
- Active and Dormant counts are derived from Session runtime state.
- Counts include Manager and subordinate Sessions.
- A Task is not a repository checkout manager. Repository and workspace behavior must be explicit connector/runtime configuration.

## 8. Task workspace

Opening a Task shows Manager Sessions and their subordinate Sessions.

~~~text
┌─ taskai · Fix authentication regression ────────────────────────────────────┐
│ SESSIONS                         │ MANAGER · CODEX                          │
│                                  │                                          │
│ + Add Session                    │ › Fix the login regression               │
│                                  │                                          │
│ ● implement · Codex    manager   │ I am delegating diagnosis, test review,  │
│   ◐ diagnose · Claude  answer    │ and implementation review.               │
│   ● tests · OpenCode   follow    │                                          │
│   ○ review · Gemini    answered  │                                          │
│                                  │                                          │
│ ○ prior attempt · Claude manager │                                          │
│                                  │                                          │
├──────────────────────────────────┴──────────────────────────────────────────┤
│ implement · working                 3 subordinate Sessions                  │
└────────────────────────────────────────────────────────────────────────────┘
~~~

Rules:

- Add Session creates a Manager Session.
- Subordinate Sessions are indented below their manager.
- The right pane shows the selected manager's ACP conversation.
- Subordinates are manager-owned. The user does not prompt a subordinate directly in V1.
- A subordinate row may show provider, assignment label, current Answer or Follow behavior, and mechanical lifecycle state.
- TaskAI renders ACP content for the user but does not reinterpret it into a provider-native terminal.
- There is no tmux pane, provider-native embedded TUI, second writer, or terminal scrape.

## 9. Creating a Manager Session

Add Session follows this flow:

1. Show enabled manager-capable providers.
2. Let the user select the manager provider and optionally name the Session.
3. Start or exactly resume the provider-owned manager conversation through ACP.
4. Open the manager conversation immediately.
5. Reuse the provider authentication completed in Settings.

There is no checkout confirmation, permission-preparation wizard, tmux setup, web channel setup, or repeated provider login.

Every user-created Manager Session starts with the product's opinionated manager behavior:

- delegate separable work to subordinate Sessions;
- use ACP for every manager/subordinate interaction;
- default every subordinate invocation to Answer;
- use Follow only when live detail is genuinely needed;
- request Replay only after completion and only when the prior ACP work is needed;
- synthesize subordinate answers into one manager response to the user;
- never ask TaskAI to interpret subordinate tools or results.

## 10. ACP wall and ownership

The manager runtime is dual-role:

~~~text
TaskAI terminal
    |
    | ACP client connection
    v
Manager runtime
    |-- ACP agent toward TaskAI
    |
    |-- ACP client toward subordinate connector A
    |-- ACP client toward subordinate connector B
    +-- ACP client toward subordinate connector C
~~~

The Manager Session owns its subordinate ACP connections. TaskAI may establish, supervise, and record the mechanical Session relationship, but it does not read the meaning of the content that crosses those connections.

Wall invariants:

1. A subordinate has exactly one parent manager.
2. Only that manager may prompt, cancel, follow, or replay it.
3. Subordinate output returns only to that manager.
4. A subordinate cannot address the user or a peer subordinate.
5. TaskAI never injects a synthesized message into either agent.
6. TaskAI never turns tool updates into a result object.
7. Unknown ACP frames pass through the connector boundary without semantic projection.
8. The provider remains the owner of conversation memory.

## 11. Answer

Answer is the default consumption behavior for every subordinate invocation.

For each assignment:

1. The manager sends session/prompt to the subordinate through ACP.
2. The subordinate performs its work and emits its normal ACP updates.
3. The manager-side runtime does not expose intermediate subordinate activity to the manager model.
4. When the ACP turn completes, the manager receives the final public ACP answer it requested and the native completion state.
5. No earlier answer or unrelated history is inserted.

If a manager prompts several subordinates, it receives several independent requested answers:

~~~text
Manager -> Claude A: diagnose
Manager -> Codex B: inspect implementation
Manager -> Gemini C: analyze tests

Claude A completes -> Manager receives Claude A's current answer
Codex B completes  -> Manager receives Codex B's current answer
Gemini C completes -> Manager receives Gemini C's current answer
~~~

Answers remain associated with their originating subordinate Session and prompt turn. TaskAI does not batch, summarize, rank, or merge them.

Non-success ACP completion such as cancellation, refusal, token exhaustion, or transport failure remains a non-success. TaskAI never manufactures an answer.

## 12. Follow

Follow is explicit and applies to one current subordinate invocation.

When Follow is selected:

- the manager consumes the subordinate's exposed ACP stream live;
- the stream may contain messages, plans, tool-call updates, execution output, and completion state;
- TaskAI does not normalize or summarize those frames;
- Follow ends automatically when that invocation completes;
- the subordinate's next invocation returns to Answer by default.

Follow is never a sticky Session-wide preference. It cannot silently expose future subordinate work.

## 13. Replay

Replay is one explicit post-answer operation. There is no separate Recall command.

~~~text
replay       means replay 1
replay N     means replay the last N completed invocations
~~~

Replay rules:

1. Replay is unavailable until the target invocation has completed.
2. The manager names one or more subordinate Sessions.
3. N applies independently to each named subordinate Session.
4. Replay returns the exposed native ACP frames for the requested completed invocation ranges.
5. Replay never automatically includes another Session or an older range.
6. Whole-Session replay is not exposed as the manager workflow.
7. No replay is inserted automatically.

Examples:

~~~text
replay Claude-A
  -> most recent completed invocation from Claude-A

replay Claude-A 3
  -> last three completed invocations from Claude-A

replay [Claude-A, Codex-B, Gemini-C]
  -> most recent completed invocation from each named Session

replay [Claude-A, Codex-B] 2
  -> last two completed invocations from each named Session
~~~

Replay may include the messages, plans, tool-call updates, execution output, and completion state that the provider exposed through ACP. It never exposes provider-private state.

TaskAI does not maintain a rolling answer buffer or transcript database. A replay-capable connector may retain opaque completed-turn cursors or use provider-owned ACP replay. If bounded replay cannot be supplied without TaskAI reconstructing history, Settings reports Replay as unsupported for that connector.

## 14. Complete manager flow

For a user request such as “Fix the authentication regression and verify the tests”:

1. TaskAI sends the user's text to the selected Manager Session through ACP.
2. The manager decides whether and how to delegate.
3. The manager creates subordinate Sessions through manager-owned ACP connections.
4. The manager sends each subordinate its exact assignment through ACP.
5. Answer subordinates work privately and return only their requested completed answers.
6. Follow subordinates expose their current ACP activity live to the manager.
7. The manager may request replay 1 or replay N from named completed subordinate Sessions.
8. The manager integrates the information and emits its answer through ACP.
9. TaskAI streams the manager's public answer to the terminal immediately.

TaskAI never decides steps 2 through 8.

## 15. Streaming and rendering

The terminal renders the manager's public ACP output as it arrives.

Requirements:

- no polling delay after a manager output frame is available;
- preserve ACP ordering;
- apply backpressure rather than dropping or silently reordering frames;
- keep unknown public content displayable through a bounded fallback;
- never expose provider-private credential material;
- never infer a hidden transcript from provider files;
- never use terminal screenshots, ANSI scraping, or tmux capture as conversation state.

TaskAI may decode ACP envelope fields required for connection operation and rendering, including Session ID, request ID, method/update discriminator, message ID, lifecycle state, and stop reason. It must not interpret whether the work is correct or what the manager should do next.

## 16. Detach, continue, and resume

Leaving the Task workspace or closing the terminal viewer does not stop an active Manager Session or subordinate Session.

~~~text
detach terminal viewer
    -> runtimes continue
    -> ACP connections remain owned by their runtimes
    -> reopen taskai
    -> reattach to the manager
~~~

Stopping a Manager Session is an explicit lifecycle action. In V1, its subordinate Sessions are stopped or marked interrupted as one owned runtime tree; they are never silently reassigned to another manager.

Exact resume uses the opaque provider Session reference and only when the connector advertises Resume. TaskAI does not reconstruct provider context from UI output or replay frames.

Manager-local bounded replay state may be unavailable after a manager runtime crash unless the subordinate connector can reproduce it through its advertised ACP Replay capability. TaskAI does not add a hidden persistent transcript fallback.

## 17. Historical Tasks and Sessions

TaskAI keeps historical Task and Session metadata so the user can see what existed and its last mechanical state.

History may show:

- Task title and creation time;
- Manager and subordinate Session labels;
- provider;
- parent manager;
- created and last-active times;
- active, dormant, interrupted, or closed state;
- opaque resume availability.

History does not mean TaskAI owns the conversation transcript. Selecting a historical Session either exactly resumes through its provider connector or shows that resume is unavailable.

## 18. Failure behavior

### Provider not configured

The user remains on Tasks or the Task workspace and receives a direct path to Settings. No login flow is opened implicitly.

### Subordinate turn fails

The native ACP failure or stop reason returns to its manager. TaskAI does not synthesize an answer. The manager may retry, choose another subordinate, or continue without it.

### Manager runtime fails

TaskAI marks the Manager Session interrupted and treats its subordinate runtime tree as interrupted. It may exactly resume only through advertised connector capability.

### Replay unsupported

The manager sees Replay unavailable for that subordinate. TaskAI does not emulate it through a transcript database or whole-session crawl.

### Authentication expires

The connector becomes Disconnected or Needs repair in Settings. Existing metadata remains. Reauthentication happens only in Settings.

## 19. Terminal interaction principles

- The Tasks screen is always the launch screen.
- Provider work is configured in Settings, not a first-run wizard.
- The Task workspace keeps the Session hierarchy visible beside the manager conversation.
- Keyboard and mouse actions must have equivalent behavior where the terminal supports them.
- Destructive lifecycle actions require an explicit user action and exact target.
- Detach is not stop.
- Back returns to the previous TaskAI surface; it never sends control characters to a provider process.
- Help lists only TaskAI navigation and lifecycle actions, not provider-specific invented commands.

## 20. Storage and privacy

TaskAI persists:

- Tasks;
- Session metadata and same-Task parent relationship;
- non-secret provider configuration;
- enabled/disabled provider selection;
- opaque provider references required for exact resume;
- mechanical lifecycle timestamps and state.

TaskAI does not persist:

- credentials or authentication state;
- manager or subordinate prompts;
- answers;
- agent thoughts;
- plans;
- tool calls or tool outputs;
- replay content;
- provider-private transcript data;
- terminal captures.

Provider-owned state remains provider-owned.

## 21. Explicit non-goals

V1 does not include:

- MCP;
- tmux;
- provider-native terminal embedding;
- web UI or HTTP API;
- TaskAI delegation or inspection tools;
- TaskAI-generated manager control messages;
- custom agent-to-agent message envelopes;
- semantic event normalization;
- transcript storage or search;
- tool-call lookup;
- automatic result delivery outside the parent manager;
- peer-to-peer subordinate messaging;
- nested subordinate hierarchies;
- whole-Session replay;
- automatic replay;
- automatic repository initialization, retargeting, worktree creation, merging, or cleanup.

## 22. Approved decisions

| ID | Decision |
| --- | --- |
| D1 | Terminal Tasks is always the launch screen, including first launch |
| D2 | Provider selection and authentication live only in Settings |
| D3 | Provider authentication is completed once and reused |
| D4 | Task and Session are the only persistent first-class entities |
| D5 | Add Session creates an opinionated Manager Session |
| D6 | A subordinate Session belongs to one same-Task manager |
| D7 | The manager owns downstream ACP communication |
| D8 | All provider communication is ACP |
| D9 | Answer is the default subordinate consumption behavior |
| D10 | A manager receives only the current answers it requested |
| D11 | Follow is explicit, invocation-scoped, and automatically resets |
| D12 | Replay is post-answer only and defaults to replay 1 |
| D13 | Replay N applies per explicitly named subordinate Session |
| D14 | Replay returns exposed ACP invocation frames, not a TaskAI reconstruction |
| D15 | There is no Recall operation or automatic historical answer insertion |
| D16 | Replay support is connector-capability-gated |
| D17 | TaskAI stores no messages, tools, answers, or transcripts |
| D18 | Detaching the viewer never stops work |
| D19 | No MCP or tmux exists in the product architecture |
| D20 | Future clients may reuse the manager ACP surface, but V1 is terminal-only |

## 23. Acceptance scenarios

### Empty first launch

- Launch taskai with no enabled providers.
- Tasks appears immediately.
- Settings is reachable.
- No provider login begins automatically.

### One-time provider setup

- Enable and authenticate a provider in Settings.
- Return to Tasks.
- Create and reopen Manager Sessions without another login prompt.

### Manager creation

- Add Session inside a Task.
- Only enabled manager-capable providers are offered.
- The new Session appears as a top-level manager.

### Multiple requested answers

- A manager prompts three subordinate Sessions in Answer behavior.
- The manager receives exactly the three current answers as they complete.
- No older answer or intermediate subordinate activity is inserted.

### Follow reset

- A manager follows one subordinate invocation.
- Its exposed ACP activity is available live.
- The next invocation defaults to Answer.

### Replay 1

- Complete a subordinate invocation.
- Request replay without N.
- Only the most recent completed invocation from the named Session is replayed.

### Replay N across Sessions

- Complete several invocations in two subordinate Sessions.
- Request replay 2 for both.
- The last two completed invocation streams from each named Session are replayed.
- No other Session or older invocation appears.

### Unsupported replay

- Use a subordinate connector without Replay capability.
- The manager cannot request replay from it.
- No TaskAI transcript fallback is created.

### Detach

- Leave the Task workspace during active manager/subordinate work.
- Reopen TaskAI.
- Work has not been stopped merely because the viewer detached.

### Storage audit

- Inspect TaskAI persistence after normal use.
- Find Task, Session, provider configuration, and opaque lifecycle metadata only.
- Find no prompt, answer, tool, transcript, replay, token, or credential content.
<!-- UX_PRODUCT_VISION_EMBED_END -->
