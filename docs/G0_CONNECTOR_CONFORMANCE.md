# taskai — G0 connector conformance

**Status:** REOPENED — owner-approved boundary authorized; production waits for revalidation

**Date:** 2026-08-26

**Scope:** Gate G0 manager, subordinate, Answer, Follow, Replay, detach, Resume, and authentication feasibility

## Original disposition under product vision v1.0

The installed public ACP stack can prove the individual transport mechanics for downstream prompts, bounded successful Answer delivery, concurrent correlation, invocation-scoped Follow/reset, exact Resume, provider-native Replay, and viewer-independent runtime ownership.

It cannot prove the complete product path without a forbidden boundary. A manager provider can launch nested ACP work through its native terminal tool, but the nested subordinate Session and prompt identities exist only inside that tool's content. Standard ACP does not expose a subordinate-created or parent-Session notification on the manager's outer connection.

TaskAI therefore could not mechanically create the required subordinate Session row and exact parent relationship without doing at least one action prohibited by product vision v1.0:

- parsing manager tool input or output to discover delegation;
- presenting a TaskAI or connector-owned delegation tool to the manager model;
- defining a custom ACP extension or agent-to-agent protocol;
- inspecting a duplicate acpx transcript/session store; or
- depending on private provider metadata or version-specific connector internals.

Gate G0 is conjunctive. This observability failure, plus incomplete native non-success delivery to the manager model, blocked M01 scaffolding and every dependent production milestone under that contract.

## Product-owner amendment

On 2026-08-26, the product owner authorized exactly one typed, connector-owned delegation capability presented to the manager model. Product vision v1.1 permits that capability to:

1. accept the manager's mechanical operation, target provider or existing subordinate Session, opaque prompt payload, and Answer or Follow mode;
2. establish and own direct downstream ACP connections;
3. report the structural parent/child and prompt-turn correlation without parsing manager prose or tool-output content;
4. return mechanically correlated public ACP content and native completion without semantic rewriting;
5. serve explicit bounded Replay from the exact provider-native Session history used by Resume; and
6. persist no prompt, answer, tool, event, replay, or user-context content in TaskAI storage.

The owner also clarified manager context behavior. A new Manager Session receives the fixed TaskAI prefix once through the provider's supported native instruction/session mechanism, layered ahead of normal `CLAUDE.md`, `AGENTS.md`, or provider-equivalent discovery. TaskAI does not modify those files. Exact Resume uses the same opaque native Session reference, does not reinject the prefix or user context as a new turn, and must retain or restore the typed capability through a supported runtime configuration path. Unsupported combinations fail closed.

This amendment resolves the architectural-policy blocker but does not retroactively pass G0. The authorized path must now pass the executable revalidation below before M01 begins.

## Installed boundary

| Component | Version |
| --- | --- |
| ACP SDK | 1.3.0 |
| acpx | 0.13.0 |
| claude-agent-acp | 0.64.0 |
| Claude Agent SDK | 0.3.220 |
| codex-acp | 1.1.7 |
| Bundled Codex CLI used by codex-acp | 0.145.0 |
| Negotiated ACP protocol | 1 |

The exercised public wire boundary was newline-delimited ACP JSON-RPC over stdio. No MCP server, TaskAI tool, tmux product dependency, provider TUI automation, terminal scrape, workflow engine, or custom agent protocol was used.

## G0 results

| Item | Result | Evidence |
| --- | --- | --- |
| 1. Manager is an ACP agent upstream | PASS | Claude manager served `initialize`, `session/new`, `session/prompt`, updates, and native `end_turn` |
| 2. Same manager acts as downstream ACP client | PASS, bounded | The manager's native Bash capability launched public acpx to a real Codex ACP connector |
| 3. Manager creates/owns subordinate without TaskAI prose parsing | FAIL end to end | The manager process owned the nested ACP calls, but the outer ACP connection exposed no structural subordinate Session notification TaskAI could persist |
| 4. Manager sends subordinate ACP prompt | PASS | Real downstream `initialize` → `session/new` → `session/prompt` → updates → PromptResponse was observed |
| 5. Answer returns only current final answer plus completion | PASS for bounded success; FAIL overall | In-memory filtering returned only current public answer chunks plus `stopReason=end_turn`; cooperative cancellation exposed no native cancelled completion to the manager path |
| 6. Concurrent independent correlation | PASS for two; full contract incomplete | Two overlapping calls used distinct outer tool IDs and subordinate Sessions while returning byte-identical answers; the required three-Session/reordered suite remains unrun |
| 7. Follow live and automatic reset | PASS mechanically; FAIL end to end | Direct runtime consumption delivered ordered frames and reset after success, cancellation, and transport loss, but ACP has no standard way to inject that live stream into the already-running manager model |
| 8. Replay 1/N from native Resume history | PASS | See `docs/G0_REPLAY_CONFORMANCE.md`; no TaskAI content store was used |
| 9. Viewer detach/reattach without runtime ownership | PASS | A `0600` Unix socket viewer detached while a separate supervisor retained the same real ACP child and Session; 220 additional native updates arrived with no viewer and no replacement conversation |
| 10. Authentication once and reused | PASS, bounded | Existing provider authentication was reused without a new login flow |

Exact Resume also passed in the transport-reset probe: a fresh connector used the same opaque provider reference once, emitted no replacement `session/new`, and received no replayed history during Resume.

## Public API limitations

ACP `PromptResponse` carries completion state but not the final answer. Public answer text arrives through `agent_message_chunk` updates without a required final-answer discriminator. acpx quiet output concatenates message chunks and does not pair the text with `stopReason`.

The public acpx runtime can keep turn events and completion results separate and can correlate isolated requests. Its persistent CLI/runtime store, however, retains messages and event history; using that as TaskAI state would duplicate the provider-native Session history and violate the approved storage boundary.

Codex currently exposes a private phase hint in connector metadata, but production conformance may not depend on undocumented provider metadata or a bundled connector implementation detail.

## Authorized boundary requiring revalidation

The now-approved typed, connector-owned delegation capability must:

1. carry the exact target provider/Session, prompt, and Answer or Follow mode;
2. use direct downstream ACP and report the mechanical parent/child relationship;
3. buffer Answer content only in memory until native completion;
4. stream Follow content only for the selected invocation;
5. preserve native non-success completion;
6. persist no prompt, answer, tool, event, or replay content; and
7. continue using provider-native Session history as Replay's only source.

An equivalent future standard ACP manager/delegation extension may replace this boundary only through another explicit product-owner amendment.

## Revalidation exit checklist

G0 passes only after disposable real-provider probes demonstrate all of the following together:

- the manager model invokes the typed capability without TaskAI prose or tool-output parsing;
- a structural notification identifies the exact parent Manager Session, subordinate Session, provider, and prompt turn before downstream content is accepted;
- three concurrent Answer calls with reordered completion remain independently correlated, including identical answer text;
- success and every exercised non-success path return current public ACP content plus native completion without manufacturing an answer;
- Follow streams one invocation live and resets after success, cancellation, refusal, exhaustion, malformed completion, and transport failure;
- Replay 1 and Replay N use only the exact provider-native Session history used by Resume;
- Start supplies the fixed manager prefix once and exposes the capability;
- exact Resume uses the same opaque provider Session, retains the prefix/capability, and injects no duplicate context turn;
- existing project instruction files are byte-for-byte unchanged;
- viewer detach/reattach, authentication reuse, and metadata-only TaskAI persistence still pass; and
- every disposable fixture and owned process is removed after evidence capture.

## Cleanup and repository audit

- All disposable fixtures and owned probe processes were removed.
- No provider credential, raw Session reference, or conversation content was printed or committed.
- Provider-owned synthetic Sessions created by the real probes were left under provider ownership and were neither inspected nor deleted.
- The source and embedded product vision hashes remain identical.
- Commit `78b8463` contains the provider-native Replay correction and evidence.
- No production scaffold or feature code was created after the failed gate.
