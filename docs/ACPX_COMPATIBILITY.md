# acpx 0.13.0 compatibility contract

Agent Task Console V1 pins `acpx` 0.13.0. All adapter behavior lives in `internal/acp`; no worker, service, CLI, TUI, API, or web package invokes `acpx` directly.

## Runtime integration

Every new Session worker owns one long-lived Node child using the public `acpx/runtime` export:

- `createAgentRegistry`
- `createRuntimeStore`
- `createAcpRuntime`
- `ensureSession`
- `startTurn` with separate event/result streams
- `getStatus`, `cancel`, and `close`
- `onPermissionRequest`

The configured `acpx` executable is resolved through symlinks, its ancestor `package.json` must identify exact version `0.13.0` and export `./runtime`, and Node must be at least 22.13. The bridge resolves the runtime with `createRequire(package.json).resolve("acpx/runtime")`; a plain package import from the repository is not assumed to work for a user-global npm installation.

The bridge source is embedded in the Go binary and materialized `0600` under a per-Session `0700` directory in `$AGENT_HOME/acpx-runtime`. It sets umask `077`. Friendly built-in profile commands are resolved by the pinned public agent registry; custom profile argv is installed as a literal registry override. Adapter downloads use a shared `0700` npm cache under the private runtime root, avoiding dependence on a global npm cache with incompatible ownership. Provider command/environment configuration crosses the private worker-to-bridge pipe before runtime creation; environment values are not placed in persisted `sessionOptions`, SQLite, or JSONL.

Go and Node exchange bounded NDJSON frames over private stdio. Commands cover ensure, turn, inspect, cancel, permission decision, and shutdown. Events cover normalized runtime activity, pending permission requests, terminal turn results, and errors. Go serializes writes and demultiplexes responses by command ID; bridge exit, overflow, malformed messages, and consumer failure fail closed and cancel the active turn.

## Why V1 does not use a PTY

`acpx prompt` submits persistent work to a detached `__queue-owner` process whose stdin/stdout are not connected to the foreground command. A PTY around the foreground process therefore cannot answer that owner's permission prompt. The queue protocol also has no pending-permission reply operation. Parsing terminal `y/N` text would be both ineffective and structurally unsafe.

## Permission behavior

For `approve-all`, the pinned runtime resolves permission requests using its normal policy. For `read-approval`, inferred read/search/fetch/thought requests may resolve automatically; write/execute/unknown `session/request_permission` calls remain pending in the bridge.

The bridge emits the exact options offered by the provider. The worker validates Session, active request, permission ID, option ID, and unique semantic option kinds. Multiple viewers use first-valid-decision-wins semantics. Detach does nothing to the pending callback. Cancel, stop, bridge EOF, or process shutdown resolves it as cancel and records the abandonment.

`allow_always` and `reject_always` are shown as persistent choices and are usable only if offered. A malformed request with duplicate semantic kinds fails closed rather than guessing which provider option to select.

The public runtime currently exposes interactive callbacks for ACP `session/request_permission`, but not every client-side host callback such as `fs/write_text_file` or `terminal/create`. Those unsupported paths remain fail-closed and must be reported by provider capability tests.

## Event fidelity

The public runtime provides rich normalized output/thought text, tool calls and updates, status, usage, and permission data. It does not expose the exact underlying JSON-RPC packet stream. Session JSONL and the advanced raw/debug view preserve the complete gateway envelope and normalized payload available to this integration; they are not described as a wire packet capture.

The legacy CLI adapter remains covered by focused compatibility tests, but new Session workers use the public runtime bridge.

## Identity, auth, and migration

Task Console Session ID, acpx record ID, ACP/backend Session ID, and provider-native Session ID remain distinct. Resume must return the exact requested ACP ID or fail closed. Cached/provider-managed login state is inherited; Agent Task Console never modifies or copies provider auth files.

Do not run a new runtime bridge concurrently with an old CLI queue owner for the same provider conversation. For demonstrations and provider acceptance, create fresh runtime-backed Sessions. A legacy Session may be resumed only after the prior worker/queue owner is known to be gone and exact identity can be proven.

Provider-native mappings remain isolated in `internal/native`: Codex uses `resume <id>`, Claude and Gemini use `--resume <id>`, and OpenCode uses `--session <id>`. Unknown profiles and missing identifiers fail closed. See [CODEX_NATIVE_SMOKE.md](CODEX_NATIVE_SMOKE.md).
