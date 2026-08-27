# taskai — G0 provider-native Replay conformance

**Status:** Replay boundary proven; conformed production adapters integrated

**Date:** 2026-08-27

**Scope:** Gate G0 item 8 only

## Decision

The provider-owned Session history used by the provider's own Resume behavior is the canonical source for Replay items.

TaskAI persists only the opaque provider Session reference needed to resolve that history. It must not create another transcript, message/tool table, replay journal, content cache, search index, seal store, or runtime-content store.

An explicit `replay` or `replay N` operation may read the exact named Session in place, group its native records into completed invocations, and stream only the requested final range to the owning manager. The native item stream may contain messages, reasoning summaries, plans, tool calls, tool results, execution output, and completion state. TaskAI preserves native order and does not semantically reinterpret those items.

## Required adapter contract

Every Replay adapter must:

1. Accept the provider identity, installed provider version, exact opaque Session reference used by Resume, and bounded positive `N`.
2. Prefer a supported provider history API. Use native JSON or JSONL only for an explicitly conformed provider version and schema.
3. Resolve exactly one Session. Never enumerate or content-search unrelated histories as a fallback.
4. Decode only mechanical Session, turn, item, parent, ordering, tool, status, timestamp, and completion fields.
5. Exclude an incomplete final invocation and select exactly the last `N` completed invocation item streams in original order.
6. Keep the selected items transient. Do not copy native history or its filesystem path into TaskAI persistence, logs, crash reports, indexes, or caches.
7. Fail closed when the reference is missing, retention removed the range, the schema/version is unknown, a concurrent write cannot be read safely, or exact completion boundaries cannot be proven.

## Installed-provider evidence

All probes used one exact Session reference. They emitted structural counts and item kinds only; no provider conversation content or Session identifier was printed or committed.

| Provider | Installed version | Canonical history boundary | Sanitized result |
| --- | --- | --- | --- |
| Codex | 0.150.1 | App Server stored-thread APIs; exact rollout JSONL only as a version-conformed fallback | PASS — exact current thread, four completed turns, Replay 1/2 selected correctly; native kinds included messages, reasoning, command execution, file changes, and web search |
| Claude Code | 2.1.226 | Exact project/session JSONL; SDK SessionStore/message API when embedded | PASS — two turns created and resumed by exact Session ID; Replay 1/2 selected correctly; native kinds included `tool_use` and `tool_result`; synthetic Session file removed |
| Gemini CLI | 0.25.0-preview.0 | Exact project-scoped native `ConversationRecord` JSON for this installed version | PASS for read-only adapter semantics — exact existing Session, nine completed turns, Replay 1/2 selected correctly; native kinds included `toolCall`; source hash unchanged |
| Grok Build | 1.0.5 | Exact Session `updates.jsonl` | PASS — two turns created and resumed by exact Session ID; Replay 1/2 selected correctly; native kinds included `tool_call` and `tool_call_update`; synthetic Session directory removed |

Gemini's fresh two-turn Resume probe was blocked before Session creation because this account requires `GOOGLE_CLOUD_PROJECT` or `GOOGLE_CLOUD_PROJECT_ID`. No setting was invented or mutated. That limits the live Resume evidence for this installed Gemini configuration; it does not invalidate the read-only exact-Session Replay proof. Production conformance must test each newly supported Gemini storage generation independently because newer Gemini CLI sources use JSONL while the installed version above uses a JSON ConversationRecord.

## Production disposition

The shipped Replay boundary now uses the exact opaque provider Session reference already stored for Resume:

- Codex Replay lazily opens App Server and reads the single named thread through `thread/read`.
- Claude Code Replay reads one exact project-scoped Session JSONL only when the configured native root, project mapping, schema, and path boundary are all safe and conformed.
- Gemini CLI and Grok Build Replay remain unavailable. Their installed ACP surfaces do not return enough information to derive the exact native history key without enumeration or guessing.

Replay adapters live behind the dedicated Replay port. The live Manager connection, runtime, metadata store, and IPC protocol expose no provider-history reader and retain no selected Replay items.

## Provider source anchors

- [Codex App Server](https://developers.openai.com/codex/app-server) documents stored thread reads, resumptions, and paginated turn/item reads.
- [Claude Code sessions](https://code.claude.com/docs/en/sessions) documents project-scoped Session JSONL; [Claude Agent SDK session storage](https://code.claude.com/docs/en/agent-sdk/session-storage) exposes stored Session messages.
- [Gemini CLI session management](https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/session-management.md) documents persisted conversation history; its [recording types](https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/services/chatRecordingTypes.ts) define native message and tool-call records.
- [Grok Build sessions](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/17-sessions.md) document persisted history; the [session export implementation](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-shell/src/session/export.rs) identifies `updates.jsonl` as the replay/export source of truth.

## G0 disposition

Gate G0 item 8 is no longer a blocker: multiple real provider-native paths prove bounded Replay without a TaskAI content store, and the production capability matrix now advertises only the exact paths implemented above.

The complete connector boundary and executable revalidation are tracked in `docs/G0_CONNECTOR_CONFORMANCE.md` and the compiled PTY acceptance suite.
