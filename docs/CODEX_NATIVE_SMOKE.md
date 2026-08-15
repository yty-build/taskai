# Real Codex native attachment smoke procedure

Use this only in a disposable Git repository with an authenticated Codex CLI and the pinned `acpx` 0.13.0. Native mode is intentionally unavailable unless `acpctl session <id>` shows both a non-empty `ACP Session ID` and a non-empty `Native Session ID`.

1. Build the current commit and point an isolated state directory at it:

   ```bash
   make build
   export AGENT_HOME="$(mktemp -d)/agent-home"
   ```

2. In a disposable Git repository, start a harmless Codex Session and wait for it to become `waiting`:

   ```bash
   bin/acpctl start "Native attachment smoke" --agent codex --repo /absolute/path/to/disposable-repo --role smoke --worktree
   bin/acpctl session <session-id>
   ```

3. If the native ID is `—`, stop. Record native attachment as unsupported by the current Codex ACP adapter. Do not substitute the ACP ID or create a new Codex conversation.

4. Attach through the provider-native CLI:

   ```bash
   bin/acpctl session attach <session-id> --native
   ```

   The command must open `codex resume <native-session-id>` in the persisted worktree. While it is open, `bin/acpctl session <session-id>` remains `waiting` and its worker PID remains live.

5. From a second terminal, submit a harmless prompt. It must report `queued` and must not begin while Codex owns the native lease:

   ```bash
   bin/acpctl session prompt <session-id> "Reply with NATIVE_QUEUE_OK only"
   ```

6. Exit Codex normally. The attach command must report successful reconciliation. Then verify the held prompt runs and the lease transitions are in durable history:

   ```bash
   bin/acpctl session logs <session-id> --follow
   bin/acpctl session logs <session-id> --raw
   ```

   Expect `native_acquired`, `native_released`, and the queued prompt's correlated turn only after `native_released`.

7. To exercise active-turn cancellation explicitly, start another harmless turn and run:

   ```bash
   bin/acpctl session attach <session-id> --native --cancel-active
   ```

   Without `--cancel-active`, attachment waits for the turn to finish. It never silently cancels.

8. If reconciliation fails, the worker deliberately holds the lease and queued prompts. After confirming the native CLI has exited and correcting the provider or `acpx` problem, retry reconciliation without launching a second native process:

   ```bash
   bin/acpctl session attach <session-id> --native --reconcile-only
   ```

Never use `--reconcile-only` while a provider-native CLI for that Session is still running.
