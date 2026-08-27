# V1 acceptance evidence map

## One-command deterministic acceptance

```bash
make acceptance
```

The deterministic suite uses fake provider Ports and temporary private state. It must not require provider credentials or mutate a developer checkout.

## Core checklist

- [ ] Create, list, view, and update Tasks.
- [ ] Create multiple Sessions under one Task.
- [ ] Persist exactly Task and Session product tables.
- [ ] Validate optional same-Task parent Session and reject cycles/self/cross-Task parents.
- [ ] Validate exact Git repositories without initialization or silent retargeting.
- [ ] Create isolated worktrees only on request and never remove them automatically.
- [ ] Reject shared checkout unless explicitly approved.

## Provider Port checklist

- [ ] New Session calls Start once and persists one opaque provider reference.
- [ ] Worker restart calls exact Resume once.
- [ ] Resume failure does not call Start or replace the provider reference.
- [ ] Healthy Port handles multiple turns without Refresh or Resume.
- [ ] Provider options are live advertisements rather than hard-coded catalogs.
- [ ] Configure returns success only after the live Port applies the selection.
- [ ] Model/effort selections are not persisted or reapplied; Resume trusts the Port's advertised current values.
- [ ] Unknown provider activity is forwarded verbatim inside bounded `provider_event.data`.
- [ ] Bridge shutdown joins provider descendants.

## Session Channel checklist

- [ ] Create and Resume return a `starting` Session without provider options as soon as its channel is addressable.
- [ ] Subscribe/replay observes `connection` with `data.state == "connecting"` before the one provider Start or exact Resume completes.
- [ ] Provider-options, configure, prompt, cancel, and permission response return typed `session_starting` before `connection.data.state == "connected"` and reach neither provider nor accepted-command journal.
- [ ] Provider startup has a 30-second bound, and Stop during startup cancels it and leaves the Session stopped.
- [ ] Prompt, cancel, permission response, and configure commands work.
- [ ] Cancel reports a reusable cancelled turn only after the provider result and retained-Port completion proof; cancellation error, timeout, or continuity loss fails the channel without hidden reconnect.
- [ ] Accepted prompt text is journaled and sent exactly as supplied, with no hidden envelope or display copy.
- [ ] Stop closes the worker's Port as a lifecycle action; no separate close command or API exists.
- [ ] First provider output is observable before turn completion.
- [ ] One active turn is enforced.
- [ ] A second prompt returns busy and is not queued or recorded as accepted.
- [ ] Frames receive increasing Session sequences.
- [ ] Journal frame types are exactly `connection`, `client_command`, `provider_event`, `provider_result`, `permission_requested`, `permission_resolved`, `permission_abandoned`, and `worker_error`.
- [ ] Replay followed by live delivery is gap-free and duplicate-free within retention.
- [ ] Replay gaps are disclosed and are not filled from provider files.
- [ ] Slow/detached viewers do not block or stop provider work.
- [ ] Worker crash never replays an uncertain prompt.

## History-boundary checklist

- [ ] Production code does not read provider-private transcript JSONL or SQLite.
- [ ] No service merges provider history and Session JSONL.
- [ ] Session JSONL is ordered replay/audit only.
- [ ] No UI claims committed/provisional state.
- [ ] No UI infers ACP/native invocation surface.
- [ ] Legacy logs are never used as provider memory.
- [ ] No managed provider-native second writer exists for a Session.

## Permission checklist

- [ ] Write/execute/unknown requests remain pending under read approval.
- [ ] Detaching every viewer leaves the request pending.
- [ ] Reattach can decide the exact offered option.
- [ ] Concurrent decisions accept one response.
- [ ] Wrong identities fail closed.
- [ ] Cancel, stop, Port failure, and bridge shutdown abandon callbacks safely.

## Web and terminal checklist

- [ ] Web attaches stream before enabling Send.
- [ ] Terminal and web use the same Session Channel service.
- [ ] HTTP commands plus SSE are treated as one logical channel.
- [ ] WebSocket is not required for V1.
- [ ] Journal frames render immediately, including provider events before terminal result.
- [ ] Model and effort dropdowns use provider advertisements.
- [ ] Viewer state survives live updates.
- [ ] Mobile navigation and controls remain accessible.
- [ ] Stopping web leaves Sessions running.
- [ ] Browser output omits provider references and private runtime data.

## Manager/child checklist

- [ ] A human/operator creates and lists ordinary child Sessions under a parent in the same Task.
- [ ] Prompt and cancel target an exact same-Task child ID through its ordinary Session Channel.
- [ ] Children stream independently.
- [ ] Operator child routes cannot address a Session in another Task.
- [ ] Parent detach does not stop children.
- [ ] V1 exposes no provider manager tools and performs no implicit child wait or result routing.
- [ ] No mailbox, peer messaging, or third persistent entity exists; those capabilities require a post-V1 explicit integration.

## API checklist

- [ ] `/api/v1` exposes stable Task, Session, and mechanical channel-frame DTOs.
- [ ] Idle Session creation returns `201` with only a `starting` Session; live provider options require a later observational read after `connection.data.state == "connected"`.
- [ ] Provider-dependent startup conflicts map to `409 session_starting`.
- [ ] Session DTOs contain no synthetic Port-connected/busy state and no requested/effective model or effort fields.
- [ ] Session creation does not submit a hidden initial turn.
- [ ] Session stream supports numeric replay positions.
- [ ] Task stream declares bounded per-Session cursor state and identifies any child cursor that must be reset.
- [ ] No provider-history, merged-conversation, prepare, launch, native, or special review endpoint remains.
- [ ] Busy maps to typed conflict with no provider dispatch.
- [ ] Resume failure is actionable and does not replacement-Start.
- [ ] Loopback, Host, origin, body, timeout, and sanitization controls hold.

## Persistence and upgrade checklist

- [ ] Applied migrations remain immutable.
- [ ] Additive migration preserves all Task and Session rows.
- [ ] Ambiguous legacy identities remain stopped rather than guessed.
- [ ] Legacy launching/outbox state is never replayed.
- [ ] Old workers are stopped before worker-protocol cutover.
- [ ] Upgrade does not delete provider stores, auth, logs, worktrees, branches, or live process markers.
- [ ] Private JSONL lines contain `seq`, optional `request_id`, `timestamp`, `type`, and `data`; Session identity comes from the canonical per-Session `channel-v1` resource rather than a duplicated line field.
- [ ] Public Session and Task stream envelopes add the validated resource's `session_id` to those line fields.

## Authenticated live matrix

Fresh private smoke evidence should cover each supported provider adapter:

| Check | Codex | Claude | Gemini | Grok | OpenCode | custom ACP |
| --- | --- | --- | --- | --- | --- | --- |
| Start returns provider reference |  |  |  |  |  |  |
| Stream attached before prompt |  |  |  |  |  |  |
| First frame before completion |  |  |  |  |  |  |
| Live config options |  |  |  |  |  |  |
| Two turns reuse Port |  |  |  |  |  |  |
| Busy rejection |  |  |  |  |  |  |
| Detach/reattach |  |  |  |  |  |  |
| Exact worker Resume |  |  |  |  |  |  |
| No provider-store read required |  |  |  |  |  |  |

Unsupported controls are recorded as unavailable and left unchanged, not guessed by the gateway. Do not commit live provider references, credentials, stores, databases, logs, or sensitive output.

## Release sign-off

Record:

```text
commit:
version:
go version:
node version:
acpx version:
platform:
make fmt-check:
make test:
make race:
make vet:
make build:
make security-check:
make acceptance:
known adapter limits:
```

Acceptance is complete only when the implementation and documentation agree on the Occam rule: Task groups work, Session addresses one provider Port, and the Session Channel carries provider truth immediately.
