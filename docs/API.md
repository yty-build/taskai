# Local API and Session Channel contract

`acpctl serve` exposes the same Task and Session services used by CLI and the structured terminal.

## Boundary

- Default URL: `http://127.0.0.1:8080`
- Health: `GET /healthz`
- API root: `/api/v1`
- JSON bodies: UTF-8 object, at most 1 MiB, unknown fields rejected
- Times: UTC RFC 3339
- Current stream transport: Server-Sent Events

V1 accepts explicit loopback IPs only. It rejects wildcard, hostname, and public binds. Mutations require a loopback Host and same-origin browser request.

HTTP commands plus the SSE stream form one logical bidirectional Session Channel. A future WebSocket may carry the same command and frame envelopes; it is not required for V1.

## DTOs

### Task

```json
{
  "id": "T-123",
  "title": "Fix login regression",
  "description": "Reproduce and repair",
  "status": "working",
  "external_ref": "APP-91",
  "metadata": {"priority": "high"},
  "session_count": 2,
  "created_at": "2026-08-25T20:00:00Z",
  "updated_at": "2026-08-25T20:02:00Z"
}
```

### Session

```json
{
  "id": "S-123",
  "task_id": "T-123",
  "parent_session_id": "S-manager",
  "role": "worker",
  "agent": "codex",
  "provider": "openai",
  "repo_path": "/work/product",
  "worktree_path": "/private/state/worktrees/T-123-S-123",
  "branch": "agent/T-123/S-123",
  "status": "waiting",
  "permission": "read-approval",
  "worker_running": true,
  "resumable": false,
  "created_at": "2026-08-25T20:00:00Z",
  "updated_at": "2026-08-25T20:02:00Z",
  "last_activity_at": "2026-08-25T20:02:00Z"
}
```

`resumable` is true only when an exact provider reference exists and no worker ownership remains. The API never exposes that reference, PID, socket, log path, raw metadata representation, credentials, or environment.

### Channel frame

```json
{
  "session_id": "S-123",
  "seq": 42,
  "request_id": "REQ-9",
  "timestamp": "2026-08-25T20:02:01Z",
  "type": "provider_event",
  "data": {"type":"text_delta","text":"Checking the repository now."}
}
```

The complete journal frame type vocabulary is:

- `connection`
- `client_command`
- `provider_event`
- `provider_result`
- `permission_requested`
- `permission_resolved`
- `permission_abandoned`
- `worker_error`

For `provider_event`, `data` is the normalized runtime event object verbatim. This includes unknown runtime event types and fields; the outer frame remains `provider_event`. Clients must retain or render unknown event objects safely instead of inventing a new outer frame type.

The public envelope fields are `session_id`, `seq`, optional `request_id`, `timestamp`, `type`, and `data`. Frames do not claim that output is committed/provisional or came from an ACP/native surface.

### Configuration options

`GET /api/v1/sessions/{id}/provider-options` makes an observational read from the currently connected Port. Its `provider_options` object has this shape:

```json
{
  "model": {
    "supported": true,
    "current_value": "opaque-current",
    "choices": [
      {"value": "opaque-provider-value", "label": "Provider label"}
    ]
  },
  "reasoning_effort": {
    "supported": false,
    "unavailable_reason": "not_advertised",
    "choices": []
  }
}
```

Catalog text is printable, sanitized, and bounded. Values are exact opaque provider values. Clients never supply a hard-coded list. Blank in a configuration field means leave that live control unchanged; on a fresh Port its current value is the provider default.

## Routes

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/healthz` | local health |
| `GET` | `/api/v1/tasks` | Task list |
| `POST` | `/api/v1/tasks` | create Task |
| `GET` | `/api/v1/tasks/{id}` | Task plus child Sessions |
| `GET` | `/api/v1/tasks/{id}/sessions` | Session list |
| `POST` | `/api/v1/tasks/{id}/sessions` | create Session; return when channel is attachable |
| `GET` | `/api/v1/tasks/{id}/stream` | aggregate child frame stream |
| `GET` | `/api/v1/sessions/{id}` | Session metadata/runtime state |
| `GET` | `/api/v1/sessions/{id}/stream` | ordered Session frames |
| `GET` | `/api/v1/sessions/{id}/provider-options` | live provider-advertised controls |
| `POST` | `/api/v1/sessions/{id}/config` | set advertised provider config |
| `POST` | `/api/v1/sessions/{id}/prompt` | submit one prompt |
| `GET` | `/api/v1/sessions/{id}/permissions` | current actionable permissions plus atomic channel-sequence watermark |
| `POST` | `/api/v1/sessions/{id}/permissions/response` | choose an exact option |
| `POST` | `/api/v1/sessions/{id}/cancel` | cancel active turn |
| `POST` | `/api/v1/sessions/{id}/resume` | start worker; begin exact Port Resume behind channel |
| `POST` | `/api/v1/sessions/{id}/stop` | stop worker, retain Session |
| `GET` | `/api/v1/sessions/{id}/children` | direct child Sessions |
| `POST` | `/api/v1/sessions/{id}/children` | create child under same Task |
| `POST` | `/api/v1/sessions/{id}/children/{child}/prompt` | address one exact child |
| `POST` | `/api/v1/sessions/{id}/children/{child}/cancel` | cancel one exact child |

The child routes are human/operator-facing API controls. They create or address ordinary Sessions after same-Task validation; they are not tools injected into the parent provider. V1 has no child wait endpoint and does not route child output or a synthesized result into the parent Session.

There is no merged conversation, provider-history, prepare, launch, provider-options reconnect, native attach, or special review endpoint.

The permissions response contains `permissions`, `sequence`, `decisions_ready`, `blocked_code`, and `blocked_message`. For a live current worker, `sequence` is the journal high-water captured atomically with its permission callbacks. A viewer applies the list only after its Session stream reaches that sequence and rejects a snapshot older than an already-applied permission frame or snapshot. Sequence zero is readiness-only for an offline/incompatible worker and never authoritatively retires durable permission cards.

## Requests

### Create Task

```json
{
  "title": "Fix login regression",
  "description": "Reproduce and repair",
  "external_ref": "APP-91",
  "metadata": {"priority": "high"}
}
```

### Create Session

```json
{
  "agent": "codex",
  "role": "worker",
  "parent_session_id": "S-manager",
  "repo_path": "/absolute/path/to/repo",
  "worktree": true,
  "approve_all": false,
  "allow_shared_checkout": false
}
```

Creation never submits a user turn and does not accept `prompt`, `model`, or `reasoning_effort` fields. It persists the Session, starts its worker, and returns `201` as soon as the Session Channel is addressable. The top-level response contains only the complete `session` DTO, whose status is `starting`; it does not contain `provider_options`.

The worker has already journaled `connection` with `data.state == "connecting"`, so the client can attach `/stream` immediately. The client waits for `connection.data.state == "connected"`, reads provider options, optionally configures, and sends a separate prompt command.

If provider startup fails after creation returns, the same Session emits failure frames and becomes failed. A client must inspect that resource rather than blindly create another.

`parent_session_id`, when supplied, must name a Session in the URL Task and must not introduce a cycle.

Repository selection is exact. If one immediate child repository can be suggested, the server returns a typed `repository_selection_required` error with the canonical suggestion and performs no mutation.

### Configure

```json
{
  "model": "opaque-provider-value",
  "reasoning_effort": "high"
}
```

Each nonblank field must exactly match a currently advertised opaque value. A blank field leaves that live control unchanged. Configuration is synchronous: `200` is returned only after the worker applies the selection. The response contains the `session` and refreshed observational `provider_options`. Failure is returned as an HTTP error; no `config_changed` journal frame is emitted. Selections are not Session metadata and are not reapplied by the gateway. After Resume, the provider's advertised current values are authoritative.

### Prompt

```json
{"message":"Check the one-hour case"}
```

An accepted prompt returns `202` with its `request_id`. The stream carries a `client_command` record with the exact submitted text, immediate `provider_event` records, and one terminal `provider_result`.

If another turn is active, the server returns `409`:

```json
{
  "error": {
    "code": "session_busy",
    "message": "This Session already has an active turn. Wait, cancel it, or retry later."
  }
}
```

The rejected prompt is not queued and is not recorded as accepted.

### Permission response

```json
{
  "permission_id": "opaque-permission-id",
  "option_id": "opaque-option-id",
  "cancelled": false
}
```

The worker validates Session, active request, permission, and exact offered option. Concurrent responses accept the first valid decision and return conflict for later attempts.

### Cancel, stop, and resume

Cancel and stop require an empty request body. Cancel targets the active turn. Stop is a lifecycle operation: it ends the worker and closes that worker's Port. The Session is resumable only if a provider identity was established and teardown cleared worker ownership; otherwise its DTO says `resumable: false`. There is no separate close command or HTTP close endpoint.

Resume accepts an empty object or an explicit shared-checkout override. It starts one worker and invokes exact provider Resume once. It never starts a replacement conversation when the saved reference cannot resume.

Create and Resume use the same immediate-attach startup contract: the worker socket and stream are available first, the response Session is `starting` and contains no provider options, and one provider Start or exact Resume proceeds behind the channel. Provider startup is bounded to 30 seconds. Stop remains available during `starting`; it cancels startup, closes the worker, and leaves the Session stopped.

Until the stream reports `connection.data.state == "connected"`, these provider-dependent operations return HTTP `409` with `error.code == "session_starting"`: provider-options, configure, prompt, cancel, and permission response. They do not reach the provider and are not queued or journaled as accepted. Metadata/status, stream subscription and replay, current permission observation, and Stop remain available.

## Session stream

Connect before sending a prompt:

```bash
curl -N http://127.0.0.1:8080/api/v1/sessions/S-123/stream
```

Example:

```text
id: 42
event: frame
data: {"session_id":"S-123","seq":42,"request_id":"REQ-9","timestamp":"2026-08-25T20:02:01Z","type":"provider_event","data":{"type":"text_delta","text":"Checking now."}}
```

Reconnect after sequence 42 using one mechanism:

```bash
curl -N -H 'Last-Event-ID: 42' http://127.0.0.1:8080/api/v1/sessions/S-123/stream
curl -N 'http://127.0.0.1:8080/api/v1/sessions/S-123/stream?cursor=42'
```

If both are supplied, they must agree. A fresh stream (no cursor, or caller-supplied numeric cursor `0`) establishes a live boundary and replays at most the last 256 retained frames before releasing later frames in order. Its transport-only `ready` event contains `cursor`, `earliest_sequence`, and `clipped`; `clipped: true` explicitly means older channel audit is outside this viewer window. The event also sets its SSE ID to the effective replay boundary. At boundary zero that ID is the opaque reconnect-only token `exact:0`, allowing EventSource to disconnect after `ready` but before frame one and still resume exactly; clients should retain and return the ID rather than reinterpret it as a numeric cursor. A nonzero `Last-Event-ID` or `cursor` resumes exactly from that saved sequence. Full audit export uses the streaming CLI log command rather than an unbounded browser replay.

If the requested sequence is older than retention, the server reports the earliest available sequence. It does not load provider history as a fallback.

If a persisted owned or terminal Session has lost its channel journal, the stream ends with `channel_error.code == "replay_unavailable"`, `reset_cursor: false`, and bounded Resume guidance. Clearing the cursor cannot recreate missing gateway audit; Resume must establish a new live channel, and earlier audit remains unavailable.

Once the Session or Task resource and supplied cursor have validated, any failure while computing the initial replay window is returned on the SSE connection as a bounded terminal `channel_error`, even when it occurs before `ready`. Clients must close that connection and display the bounded message; they must not expect the response to change back to a JSON action-error body.

## Task stream

Task activity is a computed merge of child Session frames. Its cursor is an opaque encoding of independent per-Session positions plus a Task-membership boundary. On a fresh request, each Session present at that boundary selects the bounded 256-frame tail policy. On resume, every explicitly present child position is exact, including zero (the boundary immediately before sequence one). A current child absent from a server-issued saved cursor was created after its membership boundary and therefore also resumes at exact zero; this remains lossless even when the saved cursor came from an empty Task and the new child accumulated more than one fresh replay window while disconnected. Before frames, the transport-only `ready` event sets its SSE ID and returns the same effective opaque `cursor` plus a `sessions` map whose entries contain `cursor`, `earliest_sequence`, and `clipped`. If one saved child cursor is invalid, `channel_error` identifies that `session_id` and sets `reset_cursor: true`; reconnect without the old aggregate cursor to establish fresh bounded windows.

A Session created after the Task stream's initial boundary is read from sequence one, so its activity is not silently clipped before a replay-window notice. The stream discovers such Sessions and remains bounded. It is not a global Event stream entity.

## Optional WebSocket

A later WebSocket endpoint may use:

```text
/api/v1/sessions/{id}/connect
```

Client messages carry `prompt`, `cancel`, `permission_response`, and `configure`; server messages carry the same mechanical journal frames. Stop remains a lifecycle action. Adding WebSocket transport must not change services, persistence, Port ownership, or replay semantics.

## Errors

Errors use:

```json
{"error":{"code":"stable_code","message":"safe actionable text"}}
```

Expected codes include `not_found`, `invalid_request`, `repository_selection_required`, `checkout_conflict`, `validation_failed`, `session_starting`, `session_busy`, `session_conflict`, `worker_upgrade_required`, `provider_setup_unavailable`, `replay_unavailable`, `session_operation_failed`, `stream_limit`, `stream_unsupported`, `origin_forbidden`, and `internal_error`. A persisted Session may also be returned with a bounded startup warning rather than being hidden behind an error-only response.

Provider errors, paths, environment, credentials, commands derived from raw tool input, sockets, log paths, and provider references are not copied into browser errors.

## Client rules

- Attach stream before prompt.
- Treat `connection.data.state == "connecting"` as an observable startup state. Wait for `connected` before provider-options, configure, prompt, cancel, or permission response; do not spin-retry `session_starting`.
- Treat Session ID and sequence as the only replay identity.
- Do not invent provider history from frames.
- Render unknown `provider_event.data` objects safely.
- Do not retry a busy prompt automatically.
- Do not retry an ambiguous failed submission without first inspecting stream state.
- Keep permission cards actionable across viewer reconnect.
- Use the synchronous configure response or a fresh observational provider-options read for the connected Port's current values. Session metadata does not carry requested or effective model/effort state.
- Do not present the loopback API as a public authenticated service.
