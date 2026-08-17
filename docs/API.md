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
  "conversation_ready": true,
  "conversation_verified": true,
  "conversation_verified_at": "2026-08-14T20:01:30Z",
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
  "activity": "Run tests · completed",
  "display": {
    "type": "tool_upsert",
    "tool": {
      "id": "opaque-display-key",
      "title": "Run tests",
      "kind": "execute",
      "status": "completed",
      "detail": "PASS",
      "detail_label": "result"
    }
  },
  "cursor": "opaque-task-cursor-only"
}
```

`activity` is a short, backward-compatible sanitized summary. `display` is an optional semantic delta for structured clients. Its `type` is one of `user_message`, `legacy_user_message`, `assistant_chunk`, `thought_chunk`, `tool_upsert`, `plan_replace`, `permission`, `error_upsert`, or `turn_state`. Clients merge deltas by request ID and monotonic position; tool IDs and assistant message IDs are opaque display-grouping keys, not provider identifiers.

Conversation projection:

```json
{
  "source": "provider-store",
  "checkpoint": "opaque-provider-generation",
  "gateway_cursor": 17,
  "turns": [
    {
      "key": "opaque-presentation-key",
      "committed": true,
      "surface": "native",
      "activities": []
    },
    {
      "key": "REQ-…",
      "committed": false,
      "surface": "acp",
      "activities": []
    }
  ],
  "diagnostics": [],
  "truncated": false
}
```

`source: "provider-store"` means the committed base came from the exact
canonical provider conversation after identity and checkout verification:
provider JSONL for Codex, Claude, and Grok, or provider SQLite for OpenCode.
`source: "gateway-log"` is an explicit fallback and includes a safe `warning`;
it never claims that gateway JSONL is the provider's committed transcript.
`committed: false` identifies a provisional ACP turn that has not yet matched a
provider-store commit. `checkpoint`, turn `key`, and activity IDs are opaque
presentation/reconnect values, not new domain entities or provider IDs.
Turn `surface` is transient presentation attribution: `acp` is backed by a
gateway invocation, `native` is a provider-store turn with no gateway match
only when matched ACP turns on both sides and complete gateway history prove
that distinction, and `unknown` is used for leading, trailing, repeated,
ambiguous, or bounded history that cannot support a safe attribution.

The service reads one bounded provider snapshot, joins gateway activity through
the returned `gateway_cursor`, and returns the result as one bounded object. If
provider history is unavailable on initial load, the same endpoint returns a
bounded gateway-history projection with `source: "gateway-log"` and a warning.
Clients fetch it before opening the Session stream after `gateway_cursor`. A
successful post-turn or post-native reconciliation produces a replacement
projection that clients publish atomically; it does not require a page clear or
provider-file access.

The projection is deliberately bounded. Each provider-store transcript record,
including reasoning, is at most 128 KiB; gateway assistant text is at most
128 KiB and gateway thought text at most 32 KiB. Tool detail is at most 64 KiB,
plan text 128 KiB/128 entries, and preview text 2 KiB/three lines. The browser
applies its own 32 KiB per-turn reasoning bound when reducing either source.
Tool output uses an allowlist of normalized text/diff/terminal fields. Paths
become basenames; commands, queries, URLs, raw input/output objects, arbitrary
metadata, provider/worker error detail, and structured credential/environment
fields are omitted. Allowlisted text, diffs, and terminal output are content
rather than a secret-scanning boundary and may contain sensitive repository or
tool-produced values. `truncated: true` reports that provider history or the
gateway overlay/fallback was omitted or clipped by the shared-service bounds;
activity-level `truncated` flags identify clipped display records where one is
still present. Truncation does not change the provider/gateway source role.

For `permission_requested`, Activity may additionally include a sanitized actionable permission object:

```json
{
  "permission": {
    "id": "permission-…:tool-…",
    "request_id": "REQ-…",
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

Only safe labels and the exact permission/option IDs needed to decide are exposed. Raw tool input, provider environment/auth, provider/worker errors, ACP wire data, and worker paths remain private.

The three `conversation_*` fields expose only safe continuity state. Database/runtime-only values remain intentionally absent: worker PID, Unix socket, JSONL path, metadata JSON storage representation, the canonical provider conversation ID, acpx record ID, ACP Session ID, native adapter ID, and raw ACP data.

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
| `GET` | `/api/v1/sessions/{id}/conversation` | Bounded committed provider snapshot + gateway overlay and cursor |
| `POST` | `/api/v1/sessions/{id}/prompt` | Submit ACP prompt; return request ID and lifecycle state |
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
  "model": "gpt-5.6-sol",
  "reasoning_effort": "high",
  "repo_path": "/absolute/path/to/repo",
  "prompt": "Run the complete test suite",
  "worktree": true,
  "approve_all": false,
  "allow_shared_checkout": false
}
```

The `201` response contains `session`, `request_id`, and worker queue `state`.
`model` and `reasoning_effort` are optional, bounded provider values. Blank or
omitted fields mean Provider default. Explicit values are immutable Session
metadata, returned as `requested_model` and `requested_reasoning_effort`; these
names describe requested controls and do not claim an inferred effective value.
The same optional request fields are accepted when creating a reviewer Session.

Supported explicit controls for the pinned adapters are:

| Agent | Model | Reasoning effort |
| --- | --- | --- |
| Codex | yes, provider-advertised | yes, provider-advertised |
| Claude | yes, provider-advertised | yes; leave blank for Claude's default sentinel |
| OpenCode | yes, provider-advertised | no |
| Grok / Grok Build | no; Provider default only | no; Provider default only |
| Gemini / custom profiles | no declared native-parity contract | no declared native-parity contract |

Unsupported combinations fail validation before a Session is created. Values
that require ACP advertisement are checked after the exact provider identity is
persisted but before the initial prompt. If that post-identity configuration
fails, the API still returns `201` with the failed `session` and a bounded
`warning` (`session_start_failed`) so clients open that saved resource instead
of accidentally creating a duplicate. Provider/bridge detail remains redacted.
A Session that did start but encountered a later Task-status write error uses
`session_started_with_warning` instead.

Repository selection is explicit. If `repo_path` names a non-repository folder
with exactly one immediate Git checkout, the API does not start a Session or
silently retarget it. It returns `422` with
`code: "repository_selection_required"` and the canonical
`suggested_repo_path`; the client may offer that path for a deliberate second
submission. Folders with no checkout, multiple checkouts, or more than the
bounded discovery limit require an exact repository path. The gateway never
initializes the supplied directory.

Prompt:

```bash
curl -sS http://127.0.0.1:8080/api/v1/sessions/S-…/prompt \
  -H 'Content-Type: application/json' \
  -d '{"message":"Check the one-hour case"}'
```

Successful submission is `202 Accepted` with a new `request_id` and `state`. If
another ACP turn is active, the worker keeps this request in its ACP-to-ACP FIFO
because the public runtime supports one active turn per Session; the API does
not inject it into that active turn. If an exclusive provider-native owner or
pre-launch reservation exists, submission fails with `409` before a request is
journaled or queued; the client must exit the provider normally, wait for exact
release, and retry. At the head of the ACP FIFO, every accepted request first
refreshes the idle adapter on the exact persisted ACP Session, falling back to
exact Resume, even when no managed native marker is present. Successful
continuity emits correlated `acp_rehydrated` before `turn_started`; failure
sends no prompt.

Permission decision:

```json
{"permission_id":"permission-…:tool-…","option_id":"allow-once","cancelled":false}
```

The permission-list response also reports whether the live worker can safely
accept a browser decision:

```json
{
  "permissions": [],
  "decisions_ready": false,
  "blocked_code": "worker_upgrade_required",
  "blocked_message": "safe recovery instructions"
}
```

When `decisions_ready` is false, clients must keep every pending request visible
but disable its decision controls. `blocked_message` is a sanitized local-user
recovery message; the decision has not been sent to the provider.

IDs are opaque and must come from the current permission list or stream event. The worker revalidates Session, active request, permission, and offered option; concurrent callers receive one accepted decision and a `409` conflict for later attempts. Detaching or closing the client leaves an undecided permission pending.

Cancel and stop require an empty request body. Resume accepts an empty body or:

```json
{"allow_shared_checkout": true}
```

Prompt, permission decision, and Resume against an already-live worker first
negotiate worker protocol v3 and `exclusive-native-owner-v2`. The capability
proves generation-scoped duplicate-native protection and cross-surface writer
exclusion. A responsive incompatible worker returns `409` with error code
`worker_upgrade_required`; the requested provider write is not sent and Resume
does not spawn a replacement worker. Read endpoints, conversation/history and
SSE, permission listing, Cancel, and Stop remain available for safe migration.
Run the exact `acpctl session stop <session-id>` and
`acpctl session resume <session-id>`
commands carried in the sanitized error.

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

Browser EventSource clients that need a one-time resume point may use `?seed=17`. The seed applies only when `Last-Event-ID` is absent; after the first response, automatic reconnect uses the browser's advancing `Last-Event-ID`. Do not combine `seed` with `cursor`.

After the connection's stored replay, the Session stream emits an unnumbered `event: ready` with `data: {}`. Activity received after that marker belongs to the live side of this connection; clients may use the boundary to suppress assistive announcements during history hydration. Every reconnect receives its own marker, and the marker does not advance the activity cursor.

For a stopped Session, the server sends all gateway journal records after the
cursor, emits `ready`, and closes. For an active Session, records appended after
the replay snapshot are delivered after `ready` through the worker subscription
without a cursor gap. This stream-append meaning is distinct from a turn being
committed in the provider-owned conversation store.

## Task stream

Task activity has no synthetic global row or Event entity. It merges each child
Session's durable gateway journal as an operational attention feed. It is not
the committed conversation projection. Its SSE `id` is an opaque URL-safe
cursor representing the last delivered position for each Session:

```text
id: eyJTLTEiOjE3LCJTLTIiOjl9
event: activity
data: {"session_id":"S-2","position":9,"request_id":"REQ-…","kind":"acp","timestamp":"…","activity":"…","cursor":"eyJ…"}
```

Pass the opaque value unchanged in `Last-Event-ID` or `?cursor=`. Do not decode or construct it. A reconnect replays only records not acknowledged for their owning Session. Newly created Sessions are discovered while the Task stream remains connected.

Task cursor limits protect the local HTTP process from unbounded headers. Malformed, oversized, concatenated, or conflicting cursors receive `400`. If the Task's own Session set cannot fit a valid bounded cursor, opening its aggregate stream fails closed with `422 stream_limit`; individual Session streams remain available.

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
| `409` | worker upgrade required, checkout, active-producer, permission race, native/ACP writer ownership, or lifecycle conflict |
| `422` | valid JSON with invalid domain/profile/repository input, including an explicit nested-repository selection request |
| `502` | worker operation failed; internal socket detail is suppressed |
| `500` | internal storage/server failure with sanitized message |

## Client rules

- Treat IDs and Task cursor tokens as opaque strings.
- Persist the last successfully processed SSE `id`, not merely the last received bytes.
- Deduplicate activities by `(session_id, position)`.
- Hydrate structured conversation from `/conversation`, then follow the Session
  stream after its returned `gateway_cursor`. Treat the provider-store snapshot
  as committed and unmatched gateway-JSONL/ACP deltas as provisional. Clients
  do not open provider files or perform ACP Resume themselves.
- Publish a refreshed bounded conversation projection atomically while
  preserving local scroll/focus/draft/selection/disclosure state. Never clear
  the visible transcript while fetching it.
- Display `source`, `warning`, delayed-refresh/fallback state, and `committed` honestly;
  never promote gateway fallback to canonical provider history.
- Route Task-scoped input only after a user/client selects a Session.
- Treat a native-owner/reservation `409` as a rejected ACP submission, not a
  queued request; show exit-and-retry guidance and do not synthesize an
  optimistic turn.
- A disconnected prompt client must reconnect using its cursor to receive completed correlated output.
- Do not infer permission from noninteractive use; set `approve_all` explicitly.
- Never invent a permission option ID; display only the exact current `options`, distinguish persistent `*_always` choices, and handle `409` as another viewer winning the race.
- Do not present the local API as an authenticated remote endpoint.
