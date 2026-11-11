# Feature specification: session messaging across harnesses

**Status:** Built. Two live proofs with a prototype passed (§10) with Claude's peer protocol
carrying every hop; the build moves the bridge-to-bridge hops onto TaskAI's own mailbox protocol
(§3) and makes Claude's protocol one adapter (§4). §11 states what is proven on the build.
**Date:** 2026-11-05
**Primary constraint:** One address book and one message shape for every agent, owned by TaskAI.
No harness is the floor: any harness can be absent, and the rest keep messaging. No TaskAI daemon;
each bridge lives inside its agent's tmux runtime.

## 1. Decision

Agents find and message each other through TaskAI's session registry. Every session TaskAI
launches gets a mailbox. Each harness attaches through one adapter with two halves: a way in
(deliver a letter as user input to the live session) and a way out (the tools its agent calls).

- Codex and Grok sessions get a TaskAI bridge process that owns the mailbox.
- Claude sessions keep their native `ListAgents` and `SendMessage`. TaskAI mirrors its sessions
  into Claude's local peer registry, and speaks Claude's peer protocol to and from Claude
  sessions. That protocol is the Claude adapter and nothing else depends on it.
- Bridge-to-bridge traffic uses TaskAI's mailbox protocol (§3).

This is the explicit product decision that backend_communication.md §10 requires for
agent-to-agent messaging. It supersedes the per-harness submission paths in
backend_communication.md §3 and §6–7: typed Claude PTY injection, the Codex stdio sidecar and Grok
handoff-resume. Context (§5), session references and provider-native history stay as specified.
OpenCode keeps its existing path until an adapter is specified for it.

## 2. The primitive

A session is an addressable mailbox in TaskAI's registry. A delivered message is a delegation:
the agents decide and do the work.

| Part | Definition |
|---|---|
| Session | Registry entry `{session_ref, name, harness, task_ref, workspace, pid, procStart, status, waitingFor, mailbox, features}`. The name is the address. |
| Message | `{msg_id, from, from_mode, hop_chain, to, priority: next \| now, body}` |
| Verbs | list · send · receive · status, plus a one-shot idle notice |
| Receipt | held · denied · expired · delivered · refused · dropped |
| Idle notice | idle · exited · unavailable, one per subscription |

Rules:

1. An address resolves only to a live entry: the pid is alive and its start time matches.
2. A letter carries the sender's real name and permission mode.
3. A bridge never changes a session's permission mode and never answers an approval for the user.
4. Each relay appends its hop token. TaskAI carries the chain and puts no limit on it or on how
   many letters a session receives; a harness with a limit of its own applies it, as Claude does
   (§4).
5. A refused or dropped message gets a receipt, so silence never stands in for delivery. A normal
   delivery has no receipt; the reply or the idle notice confirms it.
6. A subscriber gets exactly one idle notice: when the target next finishes a turn with nothing
   queued, or exits.
7. Message bodies stay in the sessions' own histories; TaskAI's log records correlation.

## 3. TaskAI mailbox protocol (v1)

- **Entry** in `~/.taskai/state/state.json`, on the existing session record: `name`, `status` ∈
  {`busy`, `idle`, `waiting`}, `waitingFor` (free text such as "approval"), `pid`, `procStart`
  (`ps -o lstart`, UTC), `mailbox` (socket path), `features` (`["notify_idle"]`).
- **Key** `~/.taskai/run/mail/<session_ref>.key`, mode 0600 in a 0700 directory: 32 hex.
- **Socket** `~/.taskai/run/mail/<session_ref>.sock`, mode 0600. Newline-delimited JSON. The
  first line authenticates: `{"taskai":1,"type":"auth","key":"<recipient key>"}`. Then frames:

  | Frame | Fields |
  |---|---|
  | `message` | `msg_id`, `from` (name), `from_mode` ∈ {`prompting`, `bypass`}, `hop_chain`, `priority`, `body` |
  | `receipt` | `orig_msg_id`, `status`, `reason`; when dropped, `drop_reason` and `dropped_msg_ids` |
  | `subscribe` | `msg_id`, `from`, `from_mode` |
  | `idle_notice` | `orig_msg_id`, `state`, optional `finished_at` (ms) and `detail` (≤100 chars) |

  No acknowledgement line is returned. A receipt or notice counts only when its `orig_msg_id`
  matches a send or subscription the recipient made. A subscription expires unheard after 12 hours;
  a mailbox holds at most 32. Idle fires after 0.75 s idle with nothing queued.
- **Hop token:** 24 hex, HMAC-SHA256 of the session's own address under a random per-process key,
  so only that session recognizes it. A letter sent in a turn that a peer letter started carries
  that letter's chain plus the sender's token; any other letter carries none. A chain keeps its
  newest 32 tokens. The shape matches the Claude adapter's, so Claude counts one chain correctly
  across both protocols.
- **Admission:** key, then deliver. A mailbox counts no deliveries and judges no chain. It sends a
  `dropped` receipt for a letter its session exits without reading.
- **Log** `~/.taskai/state/messages/<task_ref>.jsonl`: `ts, msg_id, from, to, direction, outcome`
  plus body text (owner-only; development use). Never keys.

## 4. Adapters

| | Claude Code | Codex | Grok CLI |
|---|---|---|---|
| Way in | the session's peer socket, Claude envelope | daemon `turn/start` on the TaskAI-created thread; steers a running turn | `session/prompt` from TaskAI's second client on the session's leader |
| Way out | native `SendMessage`, `ListAgents` | `taskai.list_sessions` / `send_message` dynamic tools attached at `thread/start`; the bridge answers `item/tool/call` | `taskai` MCP server (`taskai mcp`) declared in the workspace; a thin client of the bridge over a private socket, identified by `GROK_SESSION_ID` |
| Observe | card status; receipts and notices Claude sends | `thread/status/changed`: idle → `idle`, active → `busy`, `waitingOnApproval` → `waiting` "approval", `waitingOnUserInput` → `waiting` "input needed"; `item/completed` text as detail | `_x.ai/sessions/changed` activity; `prompt_complete`; `session/request_permission` → `waiting` "approval"; message chunks as detail |
| Lifecycle | none | bridge creates the thread; pane runs `codex resume <thread>` | pane runs `grok --leader --leader-socket <per-session> -s <uuid> --permission-mode <mode>`; bridge joins |

Both bridge tools take `notify_when_idle`. Notices and receipts for the agent's own sends arrive
as new turns, labelled as TaskAI notices.

### Claude adapter (peer protocol v1, Claude Code 2.1.322)

Enabled only while the drift check (§7) passes. It has three jobs:

1. **Mirror.** For each bridged session, write a card `~/.claude/sessions/<bridge pid>.json`
   (`pid`, `sessionId`, `startedAt`, `kind: "interactive"`, `name`, `status`, `cwd`, `version`,
   `peerProtocol: 1`, `peerFeatures: ["notify_idle"]`, `messagingSocketPath`, `procStart`,
   `pidDomain`, `entrypoint: "taskai-bridge:<harness>"`) and a key
   `<pid>.<sha256(socket)>.key` = `{peerToken, procStart, pidDomain}`, mode 0600. The card points
   at the bridge's **Claude door**: a second listener at `/tmp/cc-socks/<bridge pid>.sock`, which
   feeds the same mailbox. Claude accepts reply targets only inside its own socket directory, so
   the door lives there and TaskAI's own socket does not. Remove both on exit.
2. **Deliver into Claude.** Resolve the Claude session's card by its pane pid, read its key, write
   `{"type":"auth","token":…}` then
   `{"msgV":1,"type":"user","msg_id","priority","from":"uds:<door>","message":{"role":"user","content":<envelope>}}`.
   Envelope grammar, in this order: `from`, `from-session`, `hop-chain`, `from-name`,
   `from-mode` ∈ {`prompting`, `bypass`}, `from-plugin`; Claude re-serializes a parsed envelope
   and drops it on any difference. Control frames `peer_message_status`, `notify_when_idle` and
   `peer_idle_notice` carry the same fields as §3's `receipt`, `subscribe` and `idle_notice`.
3. **Receive from Claude** at the door: parse the envelope into a §3 message; map Claude's control
   frames the other way. Only answers to this agent's own sends and subscriptions reach the agent.

Claude's behaviour to rely on: a letter wakes an idle session ("Message from @name"); a differing
`from-mode` is held for the user, with `held` then `denied` or `delivered` receipts; the setting
`crossSessionInbound: "accept"` skips the hold; its inbound guard drops, by its defaults,
`hop-runaway` (over 28), `hop-loop` (own token 10 times), `duplicate` (30 s), `rate-limited`
(30-message bucket, 0.5/s) and `queue-full` (50), each with a `dropped` receipt; `SendMessage notify_when_idle: true` subscribes
only to cards declaring `notify_idle`; notices and receipts reach its model as
"[Cross-session idle notice]" and "[Cross-session delivery notice]". Claude launched with
`-n <name>` registers under TaskAI's name.

### Codex adapter

- Connect to `~/.codex/app-server-control/app-server-control.sock` (WebSocket at `/rpc`,
  `initialize` with `experimentalApi`), never a stdio sidecar: only the shared daemon steers the
  thread the TUI shows.
- TaskAI creates the thread, because dynamic tools exist only at `thread/start`. A 0.160 TUI cannot
  `codex resume` a thread with no turn.
- Never launch the TUI with `-c` or `--profile`; either one moves it off the daemon.
- The bridge stays subscribed, or tool calls stall. It never answers approvals.
- A `turn/start` during an active turn steers it and emits no `turn/started`; count each delivery
  when `turn/start` returns. A steered letter shares the running turn's hop chain.
- An approval in the thread needs the pane that runs `codex resume`; TaskAI shows it.
- The client name appears in the daemon's User-Agent until another client initializes.
- A thread started with `/new` inside the TUI has no `taskai` tools: it can receive, not send.

### Grok adapter

- The bridge's client passes the session's mode through the top-level flag:
  `grok --permission-mode <mode> agent --leader --leader-socket … stdio`. Omitted, the client
  applies the user's global mode to the shared session (observed: always-approve). Before
  `session/load`, confirm the leader registered the client with `yolo_mode=false`.
- Grok shows the envelope as raw text and reads it correctly. Grok loads MCP tools lazily
  (`search_tool`, then `use_tool`).
- The MCP server holds no address, key or state; the bridge serves only calls carrying the
  session's own `GROK_SESSION_ID`. Without a bridge the tool returns `bridge-unavailable`.
- `default` mode ran `sleep 45` without a prompt; the `waiting` mapping is not yet exercised live.

## 5. The bridge

One process per bridged session. It runs inside that agent's tmux runtime and lives and dies with
the agent. This is process placement, like OpenCode's serve/attach pair, not a new service.

```
bridge(session):                                   # pseudocode
  book.register(name, harness, pid, status, features = [notify_idle], mailbox)
  if claude_adapter.enabled: claude.mirror(session, door = /tmp/cc-socks/<pid>.sock)
  on frame at mailbox or door:                     # TaskAI frames, or Claude frames at the door
      letter = parse(frame)                        # one record either way
      message  -> deliver_into(session)            # no delivery count, no chain limit
      subscribe -> subscribers += (from, msg_id); notify_if_idle()
      receipt / idle_notice -> if it answers my send or subscription: deliver_into(session)
  on tool list_sessions():  book.live(task)        # name, harness, status, waitingFor
  on tool send_message(to, text, notify_when_idle):
      target = book.live(to) or fail("no live session named <to>")
      chain = (turn started by a letter) ? its chain + my token : none
      if target is a Claude session: claude.send(target, letter) else mail.send(target, letter)
      if notify_when_idle and target declares notify_idle: subscribe(target)
  on harness status change: book.update(status, waitingFor); claude.mirror_status()
      idle with nothing queued for 0.75 s -> one idle_notice per subscriber
  on exit: idle_notice(exited) to every subscriber; book.unregister(); claude.unmirror()
```

- **Expectation:** any agent finds another with `list_sessions`, hands it work with one letter,
  and learns the outcome from the reply, the idle notice or a receipt. The letter appears in the
  recipient's native CLI as user input it can answer the same way.
- **Why:** each harness exposes a way in and a tool mechanism, and nothing more. The bridge turns
  those into membership of one address book.
- **Value:** one address book, one message shape, one pulse and one log for every harness, with
  no per-pair integrations and no harness that can take the others down.

## 6. Safety rules

1. Never log or copy keys. Strip `CLAUDE_CODE_MESSAGING_*` from every process TaskAI launches.
2. Never answer approvals, and never change a session's permission mode.
3. Declare the real name and mode. Claude holds letters whose declared mode differs, which
   protects against honest mistakes, not a sender that lies. A session that asks before acting
   declares `prompting`; one that never asks declares `bypass`.
4. Act only on sessions TaskAI launched. The address book is the allowlist.
5. Carry the hop chain truthfully, so a harness's own loop limit keeps working. TaskAI limits
   neither chain length nor deliveries.
6. Codex: never request the daemon's shutdown path.
7. Deliver a notice or receipt into an agent only when it answers that agent's own send or
   subscription. Each one states that it is not an instruction.
8. Sockets and keys are owner-only; everything binds locally.

## 7. Version gate and drift checks

Each adapter is gated on its own, and a failed check disables that adapter alone, with the reason
shown in Settings. The others keep working. Checks make no model calls:

- **Claude:** installed version within the tested range; any live card reports `peerProtocol 1`
  and the required fields; a sample envelope round-trips through the strict grammar.
  Disabled means: no mirror, Claude sessions not addressable, other harnesses unaffected.
- **Codex:** the daemon accepts `initialize`; `thread/start` with `dynamicTools`, `turn/start`,
  `turn/interrupt`, `thread/status/changed` and `thread/unarchive` exist; daemon version read
  from the `initialize` User-Agent, not from `codex --version`.
- **Grok:** the leader's `initialize` reports `loadSession`; the client registers with
  `yolo_mode=false` under `--permission-mode`.

## 8. Scope and non-goals

- Local machine only. SSH tasks are out of scope.
- Only sessions TaskAI launched take part. Claude sessions started by hand are not imported.
- Development use only; no hosted or always-on agents.
- No transcript store, scheduler or delegation policy; backend_communication.md §10 still
  excludes those.

## 9. Decisions for T

| Decision | Recommendation |
|---|---|
| Bridge lifetime | A second window in the agent's tmux session, started after the provider, exited with it; recreated on reopen when the runtime is live |
| Messages across permission modes | Claude's hold applies at the Claude door. For bridge-to-bridge letters, TaskAI delivers, records the mode mismatch in the log, and shows it in the UI; no hold. Approvals for actions stay in each harness. |
| Message log content | Correlation plus text, in an owner-only file per task |
| Naming | `<harness>-<agent name>`, for example `codex-lead`; Claude is launched with `-n` so its native name matches |

## 10. Evidence

Both proofs ran with Claude's peer protocol carrying every hop, including bridge to bridge. The
mailbox protocol in §3 is specified from them and is not yet run.

| Claim | Status | Where |
|---|---|---|
| A non-Claude peer is listed natively, `SendMessage` reaches it, and its reply lands in Claude | Proven live | journal.md 2026-11-04, scratchpad `ccdemo/` |
| Codex: daemon delivery shows in a live TUI; a dynamic tool call reaches TaskAI | Proven live | scratchpad `codex-live/` |
| Grok: a second leader client delivers into a live TUI; an MCP tool call reaches TaskAI | Proven live | scratchpad `grok-live/` |
| Claude → Codex → Claude and Claude → Grok → Claude round trips, replies waking an idle Claude | Proven live | scratchpad `e2e/run1/` |
| Codex → Grok with no Claude in the path; one msg_id in both bridges' logs | Proven live | scratchpad `e2e/run1/messages.jsonl` |
| A mismatched `from-mode` is held; the sender gets `held` then `denied` | Proven live | scratchpad `e2e/run1/codex_bridge.log` |
| Idle notices, receipts, hop chains, `peerFeatures` and status values as in §4 | Read in Claude Code 2.1.289's source | journal.md 2026-11-04 |
| Claude drops a 29-hop chain, and its `dropped hop-runaway` receipt is relayed to the sending agent | Proven live | scratchpad `e2e/messages.jsonl`, `codex_bridge.first.log` |
| Codex, told only a goal, finds Grok with `list_sessions`, delegates with an idle subscription, gets the answer and the notice, reports back; hop chain 0 → 1 → 2 | Proven live | scratchpad `e2e/` |
| Grok does the same toward Codex through its MCP tools | Proven live | scratchpad `e2e/grok_mcp_calls.jsonl` |
| Claude subscribes to a bridge because it declares `notify_idle`, and renders its `idle` and `exited` notices natively | Proven live | journal.md 2026-11-04 |
| A receipt from a bridge renders in Claude as a delivery notice | Proven live | journal.md 2026-11-04 |
| A Codex thread waiting on an approval shows `waiting` in the registry and in Claude's `ListAgents`; `turn/interrupt` releases it | Proven live | scratchpad `e2e/codex_wire.jsonl` |
| Grok's tool reports `bridge-unavailable` once its bridge is gone | Proven live | scratchpad `e2e/grok_mcp_calls.jsonl` |
| Grok's `waiting` mapping on a permission request | Implemented, not exercised | scratchpad `e2e/grok_bridge.py` |

## 11. Acceptance

Evidence for each criterion: **P** met by the prototype proofs (§10); **O** covered by this
build's offline tests; **Z** met by this build on the installed CLIs with no model call
(`tests/evidence/session-messaging-m9-dry-run-*.json`); **B** needs the billed release proof
(`crates/taskai/tests/release_messaging.rs`), which has not run.

| # | Criterion | Evidence |
|---|---|---|
| 1 | Claude → Codex → Claude and Claude → Grok → Claude round trips complete, with Claude idle when each reply arrives | P · O · B |
| 2 | A message declaring a different permission mode is held, not delivered | P · O for the relayed receipts; not in the billed proof |
| 3 | The drift checks pass on the installed versions and fail closed per adapter | O · Z |
| 4 | Cleanup leaves no registry entries, sockets or leaders behind | P · O · Z |
| 5 | A bridge delivers a letter whatever its chain holds and carries the chain on; Claude applies its own limits to it, and its `dropped` receipt reaches the sender | O for the bridge; P for Claude's drop |
| 6 | A Codex agent told only a goal finds a Grok delegate with `list_sessions`, delegates with `notify_when_idle`, and gets the idle notice | P · O for the tools · B |
| 7 | Grok delegates to Codex the same way | P · O for the tools · B |
| 8 | A bridged session waiting on an approval shows `waiting` in the registry and in Claude's `ListAgents` | P · O · B |
| 9 | A delegate stopped mid-task sends `exited`, and the Claude subscriber is told | P · O · B |
| 10 | A letter a bridge cannot deliver comes back to the Claude sender as a delivery notice | P · O · B |
| 11 | Criteria 6 and 7 pass on the mailbox protocol with the Claude adapter disabled and no Claude session running | O · B |
| 12 | With the Claude adapter enabled, Claude's `ListAgents` lists TaskAI's sessions, and criteria 1, 8, 9 and 10 pass against the mirror | Z for the mirror card, which a real Claude session's `ListAgents` showed · B for the rest |
| 13 | A failed Claude drift check disables only the Claude adapter; criterion 11 still passes | Z for the disabling · B for criterion 11 |
| 14 | A bridge starts with its agent's tmux runtime, exits with it, and is recreated on reopen | O · Z for Grok · B for Codex after its first turn |
