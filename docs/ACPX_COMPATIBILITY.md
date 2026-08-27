# acpx 0.13.0 compatibility contract

Agent Task Console V1 pins `acpx` 0.13.0. All adapter behavior stays in `internal/acp`; worker, service, CLI, terminal, API, and web packages do not invoke `acpx` directly.

## Runtime integration

Each active Session worker owns one long-lived private bridge using the public `acpx/runtime` export. The bridge resolves the runtime from the package associated with the configured `acpx` binary and verifies the exact supported version before provider startup.

Runtime and bridge files live under private `$AGENT_HOME/acpx-runtime` directories with user-only permissions. Provider authentication remains provider-managed. Environment values are used only to launch the adapter and are not persisted in Session metadata or channel logs.

Go and Node exchange bounded structured frames over a private control pipe. Child stdin is reserved for any pinned runtime host-operation response that requires it. Malformed, oversized, or unexpected control traffic fails closed.

## One Port per Session

The bridge implements the Session provider Port:

- Start a new provider conversation when the Session has no provider reference.
- Persist the returned opaque reference before the Session becomes ready.
- Exact-resume that saved reference once when a worker/provider transport starts again.
- Keep the connection alive across turns.
- Send prompt, cancel, permission response, and configure commands through that connection.
- Emit normalized runtime event objects immediately.

Stopping a Session worker closes the Port as lifecycle cleanup. The bridge does not expose a separate channel close command or HTTP close endpoint.

The bridge does not Refresh or Resume before each prompt. It does not reset `acpx` records to manufacture a new retained connection for every turn. A healthy Port is reused.

Pinned `acpx` 0.13.0 normally falls back to creating a new client from inside
`startTurn` and idle control calls when its pending persistent client is lost.
That fallback is incompatible with the Session Port contract because it would
hide an exact Resume inside Prompt, provider-options, or configure. The bridge
therefore shape-checks the pinned manager's `createClient` and
`readPendingPersistentClient` hooks before the one allowed Start/Resume,
disables subsequent client creation on that manager instance, and proves the
same retained client immediately before and after every provider-dependent
operation. Loss at either boundary fails and closes the Port. Only an explicit
Session Resume may create a fresh bridge and reconnect the saved provider
reference. This compatibility shim is intentionally contained in
`internal/acp` and must be re-audited before changing the pinned acpx version.

If exact Resume fails, the bridge closes and reports `provider_resume_failed`. It never falls back to Start under the same Task Console Session.

## Configuration discovery

Model and reasoning controls come from the connected provider Session. An observational provider-options call returns a bounded catalog containing sanitized labels and exact opaque values; it does not append a journal frame.

The gateway never embeds a cross-provider model catalog. Blank in a `configure` field means leave the current live value unchanged; on a fresh Port that value is the provider default. Every nonblank value must exactly match a current advertisement. Configure completes synchronously after the Port applies it or returns an error. The gateway does not persist or reapply model/effort selections, and it emits no `config_changed` journal frame. After Resume, the Port's advertised current values are authoritative.

There is no prepared Session, provider-options reconnect, launch phase, or initial-prompt outbox. The ordinary flow is attach after `connection.data.state == "connecting"`, wait for `connection.data.state == "connected"`, observe options, configure, and prompt. The channel is addressable before the bridge completes its one Start or exact Resume.

## Turn behavior

The public runtime supports one active turn per provider Session. Agent Task Console mirrors that constraint: when a turn is active, another prompt is rejected as busy. The bridge does not maintain a prompt FIFO.

Provider activity is forwarded while the turn runs as `provider_event` records. The terminal outcome becomes `provider_result`; it is not the first time clients receive content.

Cancellation targets the active request. It does not cancel the gateway's Prompt context first: the bridge sends the provider cancellation, drains the provider's terminal turn result, and proves the same retained Port remains reusable. Only then does the worker report a healthy cancelled turn. A cancellation error, timeout, or failed retained-Port proof fails and closes the channel, requiring explicit exact Resume. The gateway does not claim cancellation paused an exact token or tool position.

## Permission behavior

The gateway passes its coarse permission policy to the ACP runtime but does not select provider-specific modes, rewrite provider configuration, or infer policy from a provider name.

For the typed `session/request_permission` callback, the bridge emits the exact provider-offered choices. The worker validates opaque IDs and uses first-valid-decision-wins. Detaching a viewer leaves the callback pending. Cancel, stop, bridge EOF, or Port close abandons it safely.

If an adapter exposes a separate human terminal prompt instead of the typed callback, the managed noninteractive Port fails closed. The gateway does not parse status text, synthesize permission cards, or push invented yes/no answers into provider stdin.

## Frame fidelity

`acpx/runtime` exposes normalized event objects for text, reasoning, tools, plans, status, usage, configuration, permissions, and extensions. The worker places each object verbatim in `provider_event.data`, including unknown event types and fields.

The result is not guaranteed to be an exact ACP JSON-RPC packet capture. The adapter bounds the normalized object, but the gateway does not project it into invented semantic outer frame types. The outer journal type remains `provider_event`.

The other journal types are exactly `connection`, `client_command`, `provider_result`, `permission_requested`, `permission_resolved`, `permission_abandoned`, and `worker_error`.

Session JSONL records the frames that crossed this integration. It supports replay and audit only. It is not a provider transcript and is never replayed into provider memory.

## What the adapter must not do

- read Codex, Claude, Grok, Gemini, or OpenCode private transcript stores;
- select a conversation by recency;
- merge provider history with Session JSONL;
- infer committed/provisional state;
- launch a provider-native CLI as a second writer;
- Refresh before prompt;
- queue prompts;
- replace a failed exact Resume with Start.

## Custom agents

Friendly built-in profiles may use the pinned runtime registry. Custom profiles install literal command/argument overrides without shell parsing. Gemini-specific ACP flags or similar compatibility details stay inside `internal/acp` fixtures.

Unknown profiles may still function through a custom ACP command if they satisfy the Port contract. Unsupported model/effort controls remain disabled and unchanged.

## Upgrade behavior

Worker protocol changes require a version bump. A responsive old worker receives no provider write from a new client. Stop it cleanly, confirm its provider process is gone, and resume the Session with the new worker.

Do not delete legacy `acpx` state, Session logs, native-owner markers, or launch files while an older worker/provider process may be alive. New code may ignore obsolete files after safe shutdown, but it must not reinterpret them as current channel state.

## Required compatibility tests

- exact package/version resolution;
- private runtime permissions;
- literal profile argv and redacted environment;
- Start once for an empty reference;
- Resume once for a saved reference;
- two turns reuse one Port;
- retained-client loss before or during prompt, provider-options, or configure
  fails the Port and performs no hidden client creation;
- Resume failure never calls Start;
- immediate frame delivery before turn completion;
- busy rejection without queueing;
- observational live configuration advertisements and synchronous configure completion;
- permission detach/decision/cancel behavior;
- malformed/oversized bridge frame failure;
- bridge/process-group shutdown without leaked provider children.
