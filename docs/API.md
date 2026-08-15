# Local API and SSE contract

`acpctl serve` exposes a versioned Task/Session API for the local web UI and future clients. It is a transport over the same application services as the CLI and terminal console.

## Boundary

- Default base URL: `http://127.0.0.1:8080`
- Health: `GET /healthz`
- API root: `/api/v1`
- JSON request bodies: UTF-8, one object, maximum 1 MiB, unknown fields rejected
- Timestamps: UTC RFC 3339 with fractional precision when present
- Streams: `text/event-stream`

V1 accepts only explicit loopback listen IPs and rejects wildcard, hostname, and public binding requests. The web handler also rejects non-loopback Host headers and cross-origin mutations.

This is not an authenticated public service. See [OPERATIONS.md](OPERATIONS.md#public-service-boundary).

## DTOs

Task:

```json
{
  "id": "T-…",
  "title": "Fix login regression",
  "description": "Reproduce and repair",
  "status": "working",
  "external_ref": "APP-91",
  "metadata": {"priority": "high"},
  "session_count": 2,
  "created_at": "2026-08-14T20:00:00Z",
  "updated_at": "2026-08-14T20:02:00Z"
}
```

Session:

```json
{
  "id": "S-…",
  "task_id": "T-…",
  "role": "implementer",
  "agent": "codex",
  "provider": "openai",
  "repo_path": "/work/product",
  "worktree_path": "/home/me/.agent/worktrees/T-…-S-…",
  "branch": "agent/T-…/S-…",
  "status": "waiting",
  "permission": "read-approval",
  "worker_running": true,
  "created_at": "2026-08-14T20:00:00Z",
  "updated_at": "2026-08-14T20:02:00Z",
  "last_activity_at": "2026-08-14T20:02:00Z"
}
```

Activity:

```json
{
  "session_id": "S-…",
  "position": 17,
  "request_id": "REQ-…",
  "kind": "acp",
  "timestamp": "2026-08-14T20:02:00Z",
  "activity": "13:02:00 [REQ-…] ran go test ./...",
  "cursor": "opaque-task-cursor-only"
}
```

For `permission_requested`, Activity may additionally include a sanitized actionable permission object:

```json
{
  "permission": {
    "id": "permission-…:tool-…",
    "request_id": "REQ-…",
    "tool_call_id": "tool-…",
    "title": "Write marker",
    "kind": "edit",
    "state": "waiting",
    "options": [
      {"id":"allow-once","name":"Allow once","kind":"allow_once"},
      {"id":"reject-once","name":"Reject","kind":"reject_once"}
    ]
  }
}
```

Only safe labels and exact opaque IDs needed to decide are exposed. Raw tool input, provider environment/auth, ACP wire data, and worker paths remain private.

Database/runtime-only fields are intentionally absent: worker PID, Unix socket, JSONL path, metadata JSON storage representation, acpx record ID, ACP Session ID, provider-native Session ID, and raw ACP data.

## Routes

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/healthz` | `{status:"ok", api_version:"v1"}` |
| `GET` | `/api/v1/tasks` | Task DTO list |
| `POST` | `/api/v1/tasks` | Create Task |
| `GET` | `/api/v1/tasks/{id}` | Task DTO plus child Session DTOs |
| `GET` | `/api/v1/tasks/{id}/sessions` | Child Session DTO list |
| `POST` | `/api/v1/tasks/{id}/sessions` | Start Session and initial request |
| `GET` | `/api/v1/tasks/{id}/stream` | Aggregate Task SSE |
| `GET` | `/api/v1/sessions/{id}` | Session DTO |
| `POST` | `/api/v1/sessions/{id}/prompt` | Queue prompt; return request ID |
| `GET` | `/api/v1/sessions/{id}/permissions` | Current actionable permission list |
| `POST` | `/api/v1/sessions/{id}/permissions/decision` | Select an exact offered option |
| `POST` | `/api/v1/sessions/{id}/cancel` | Request active-turn cancellation |
| `POST` | `/api/v1/sessions/{id}/resume` | Resume worker |
| `POST` | `/api/v1/sessions/{id}/stop` | Stop worker |
| `POST` | `/api/v1/sessions/{id}/review` | Create independent reviewer Session |
| `GET` | `/api/v1/sessions/{id}/stream` | Session SSE |

## Requests

Create Task:

```bash
curl -sS http://127.0.0.1:8080/api/v1/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"Fix login","description":"Reproduce and repair","external_ref":"APP-91","metadata":{"priority":"high"}}'
```

Start Session:

```json
{
  "agent": "codex",
  "role": "implementer",
  "repo_path": "/absolute/path/to/repo",
  "prompt": "Run the complete test suite",
  "worktree": true,
  "approve_all": false,
  "allow_shared_checkout": false
}
```

The `201` response contains `session`, `request_id`, and worker queue `state`.

Prompt:

```bash
curl -sS http://127.0.0.1:8080/api/v1/sessions/S-…/prompt \
  -H 'Content-Type: application/json' \
  -d '{"message":"Check the one-hour case"}'
```

Successful submission is `202 Accepted` with a new `request_id` and `state`. If another turn is active, the worker queues this request; the API does not inject it into the active turn.

Permission decision:

```json
{"permission_id":"permission-…:tool-…","option_id":"allow-once","cancelled":false}
```

IDs are opaque and must come from the current permission list or stream event. The worker revalidates Session, active request, permission, and offered option; concurrent callers receive one accepted decision and a `409` conflict for later attempts. Detaching or closing the client leaves an undecided permission pending.

Cancel and stop require an empty request body. Resume accepts an empty body or:

```json
{"allow_shared_checkout": true}
```

Review:

```json
{"agent": "claude", "allow_active_producer": false}
```

The safe default rejects a producer whose worker/check-out may still change.

## Session stream

Connect from the beginning:

```bash
curl -N http://127.0.0.1:8080/api/v1/sessions/S-…/stream
```

Example event:

```text
id: 17
event: activity
data: {"session_id":"S-…","position":17,"request_id":"REQ-…","kind":"acp","timestamp":"…","activity":"…"}
```

Reconnect after position 17 with either mechanism:

```bash
curl -N -H 'Last-Event-ID: 17' http://127.0.0.1:8080/api/v1/sessions/S-…/stream
curl -N 'http://127.0.0.1:8080/api/v1/sessions/S-…/stream?cursor=17'
```

If both are supplied they must agree. Positions are monotonically increasing within the Session. `request_id` correlates output with prompt submission.

For a stopped Session, the server sends all stored records after the cursor and closes. For an active Session, worker subscription replays stored/missed output before live output.

## Task stream

Task activity has no synthetic global row or Event entity. It merges each child Session's durable history. Its SSE `id` is an opaque URL-safe cursor representing the last delivered position for each Session:

```text
id: eyJTLTEiOjE3LCJTLTIiOjl9
event: activity
data: {"session_id":"S-2","position":9,"request_id":"REQ-…","kind":"acp","timestamp":"…","activity":"…","cursor":"eyJ…"}
```

Pass the opaque value unchanged in `Last-Event-ID` or `?cursor=`. Do not decode or construct it. A reconnect replays only records not acknowledged for their owning Session. Newly created Sessions are discovered while the Task stream remains connected.

Task cursor limits protect the local HTTP process from unbounded headers. Malformed, oversized, concatenated, or conflicting cursors receive `400`.

## Errors

Errors use a stable envelope:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "Task title is required"
  }
}
```

Common statuses:

| Status | Meaning |
| --- | --- |
| `400` | malformed JSON/body/cursor |
| `404` | Task or Session not found |
| `409` | worker, checkout, active-producer, or lifecycle conflict |
| `422` | valid JSON with invalid domain/profile/repository input |
| `502` | worker operation failed; internal socket detail is suppressed |
| `500` | internal storage/server failure with sanitized message |

## Client rules

- Treat IDs and Task cursor tokens as opaque strings.
- Persist the last successfully processed SSE `id`, not merely the last received bytes.
- Deduplicate activities by `(session_id, position)`.
- Route Task-scoped input only after a user/client selects a Session.
- A disconnected prompt client must reconnect using its cursor to receive completed correlated output.
- Do not infer permission from noninteractive use; set `approve_all` explicitly.
- Never invent a permission option ID; display only the exact current `options`, distinguish persistent `*_always` choices, and handle `409` as another viewer winning the race.
- Do not present the local API as an authenticated remote endpoint.
