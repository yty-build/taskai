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
