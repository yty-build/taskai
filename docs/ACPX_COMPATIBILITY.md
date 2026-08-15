# acpx 0.13.0 compatibility contract

Agent Task Console V1 pins `acpx` 0.13.0. All command grammar and parsing lives in `internal/acp`; no other package may invoke `acpx` directly.

The implementation spike on 2026-08-14 used an authenticated Codex session and verified:

- `sessions ensure --name <name>` returns `acpxRecordId` and `acpxSessionId`.
- The first persistent prompt may replace the provisional ACP session identifier, so metadata is inspected after prompting.
- `status` uses `acpxSessionId`, while `sessions show` uses `acpSessionId`; the adapter accepts both spellings.
- `--format json --json-strict` prompt output is raw ACP JSON-RPC NDJSON in practice. Agent Task Console wraps each raw line in its own stable request-correlated stream envelope while retaining the complete raw object.
- Provider `agentSessionId` is optional. The Codex adapter used in the spike did not expose one, so provider-native attachment must remain unavailable unless later metadata explicitly supplies it.
- `--auth-policy skip` allows the Codex ACP adapter to reuse an existing ChatGPT login. No credential is copied into Agent Task Console state.
- The harmless probe completed with `ACPX_COMPAT_OK`, then its named acpx session was soft-closed.

Adapter fixtures cover malformed and partial NDJSON, nonzero exits, cancellation, version mismatch, missing native IDs, explicit resume, and literal custom command arguments.
