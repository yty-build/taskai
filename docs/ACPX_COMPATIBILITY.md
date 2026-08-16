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

The bridge source is embedded in the Go binary and materialized `0600` under a per-Session `0700` directory in `$AGENT_HOME/acpx-runtime`. It sets umask `077`. Friendly built-in profile commands are resolved by the pinned public agent registry; custom profile argv is installed as a literal registry override. Gemini is deliberately an override using `gemini --experimental-acp`, because the installed Gemini 0.25 CLI rejects the pinned runtime registry's `--acp` spelling. Adapter downloads use a shared `0700` npm cache under the private runtime root, avoiding dependence on a global npm cache with incompatible ownership. Provider command/environment configuration crosses the private worker-to-bridge pipe before runtime creation; environment values are not placed in persisted `sessionOptions`, SQLite, or JSONL.

Go and Node exchange bounded NDJSON frames over a private control pipe on file descriptor 3, leaving the bridge child's stdin reserved for acpx host-operation answers. Commands cover ensure, turn, inspect, exact refresh, cancel, permission decision, and shutdown. Events cover normalized runtime activity, pending permission requests, terminal turn results, and errors. Go serializes writes and demultiplexes responses by command ID; bridge exit, overflow, malformed messages, and consumer failure fail closed and cancel the active turn.

## Why V1 does not use a PTY

`acpx prompt` submits persistent work to a detached `__queue-owner` process whose stdin/stdout are not connected to the foreground command. A PTY around the foreground process therefore cannot answer that owner's permission prompt. The queue protocol also has no pending-permission reply operation. The public runtime bridge instead owns the live ACP client in-process. For its two known host prompts, it observes structured `client_operation` activity and supplies a private one-shot y/N answer through reserved stdin; it never parses mixed provider terminal output.

## Permission behavior

For `approve-all`, the pinned runtime resolves permission requests using its normal policy. For `read-approval`, inferred read/search/fetch/thought requests may resolve automatically; write/execute/unknown `session/request_permission` calls remain pending in the bridge.

Provider/user defaults can allow workspace writes without a permission request. For every read-approval Session, the bridge therefore selects Codex's provider-native `read-only` mode and Claude's provider-native `default` (manual) mode; that makes file-change and command escalation cross `session/request_permission`. OpenCode receives an inline runtime override that preserves its other configuration while forcing `edit` and `bash` from permissive defaults to `ask` and retaining stricter deny rules. Approve-all OpenCode Sessions explicitly receive `permission: "allow"`; other approve-all providers retain their normal mode and the runtime approves their requests.

For `session/request_permission`, the bridge emits the exact options offered by the provider. The worker validates Session, active request, permission ID, option ID, and unique semantic option kinds. Multiple viewers use first-valid-decision-wins semantics. Detach does nothing to the pending callback. Cancel, stop, bridge EOF, or process shutdown resolves it as cancel and records the abandonment.

`allow_always` and `reject_always` are shown as persistent choices and are usable only if offered. A malformed request with duplicate semantic kinds fails closed rather than guessing which provider option to select.

The public runtime does not expose callbacks for client-side `fs/write_text_file` or `terminal/create`. In read-approval mode the pinned bridge converts their structured running events into gateway-owned `Allow Once`/`Reject` cards, keeps acpx's private y/N prompt pending, and returns the viewer's answer to that exact operation. It does not invent an always-allow scope that acpx did not offer. Unknown host-operation prompts remain fail-closed. Prompt text, write previews, commands, and inline configuration are not copied into bridge diagnostics.

## Event fidelity

The public runtime provides rich normalized output/thought text, tool calls and updates, status, usage, and permission data. It does not expose the exact underlying JSON-RPC packet stream. Session JSONL and the advanced raw/debug view preserve the complete gateway envelope and normalized payload available to this integration; they are not described as a wire packet capture.

The legacy CLI adapter remains covered by focused compatibility tests, but new Session workers use the public runtime bridge.

## Identity, auth, and migration

Task Console Session ID, acpx record ID, ACP/backend Session ID, and native adapter ID remain distinct evidence. The first exact provider identity becomes the immutable canonical provider conversation ID; later observations may verify it but may not replace it implicitly. ACP resume must return the exact requested ACP ID or fail closed. Cached/provider-managed login state is inherited; Agent Task Console never modifies or copies provider auth files.

Do not run a new runtime bridge concurrently with an old CLI queue owner for the same provider conversation. For demonstrations and provider acceptance, create fresh runtime-backed Sessions. A legacy Session may be resumed only after the prior worker/queue owner is known to be gone and exact identity can be proven.

Provider-native mappings and exact local-store verification remain isolated in `internal/native`: Codex uses `resume <id>`, Claude uses `--resume <id>`, Grok's ACP-friendly `grok-build` profile maps to `grok --resume <id>`, and OpenCode reuses the same pinned npm CLI without its `acp` subcommand and adds `--session <id>`. Unknown profiles, missing identities, ambiguous records, and checkout mismatches fail closed. Native attachment never pauses ACP; when the provider process exits, the worker refreshes the exact ACP identity at its next idle boundary. See [CODEX_NATIVE_SMOKE.md](CODEX_NATIVE_SMOKE.md).
