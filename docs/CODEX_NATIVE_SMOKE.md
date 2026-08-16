# Real Codex native-continuity smoke procedure

Use this only in a disposable Git repository with authenticated Codex and acpx 0.13.0 installations. The test proves that the structured ACP console and the real Codex terminal continue one canonical provider conversation; it does not depend on a gateway-owned terminal emulation.

1. Build the current commit and select an isolated private state directory:

   ```bash
   make build
   export AGENT_HOME="$(mktemp -d)/agent-home"
   ```

2. Start a harmless Codex Session in a disposable repository and wait for it to return to `waiting`:

   ```bash
   bin/acpctl start "Native continuity smoke" \
     --agent codex \
     --repo /absolute/path/to/disposable-repo \
     --role smoke \
     --worktree \
     --prompt "Remember the nonce ACP-NATIVE-42 and reply with it. Do not use tools or modify files."
   bin/acpctl session <session-id>
   ```

3. Session detail must show a non-empty `Canonical provider conversation`. If it is `—`, continue through `--acp` and record identity establishment as the blocker. Never substitute an acpx record, guess a recent provider Session, or start a replacement conversation.

4. Inspect the structured gateway view:

   ```bash
   bin/acpctl session attach <session-id> --acp
   ```

   Confirm that the initial nonce response appears, then press `q`. Detaching this viewer must not stop the worker or cancel work.

5. Open the real Codex harness for that exact conversation:

   ```bash
   bin/acpctl session attach <session-id>
   ```

   The command must verify the canonical ID in Codex's local Session store and run `codex resume <canonical-id>` in the persisted checkout. Ask Codex: `What nonce did I ask you to remember? Reply only with it.` The answer must be `ACP-NATIVE-42`.

6. If tmux is installed, press `Ctrl-B d`. Codex must keep running. Re-run `bin/acpctl session attach <session-id>` and confirm that it reconnects to the same native screen instead of starting another process.

7. Add a second harmless native-only nonce, `NATIVE-ACP-73`, then exit Codex normally. Detaching tmux is not an exit; close the Codex process itself for this step.

8. Inspect raw Session activity:

   ```bash
   bin/acpctl session logs <session-id> --raw
   ```

   Expect `native_refresh_started` followed by `native_refreshed`. If ACP was busy, refresh may occur only after active and queued turns become idle. A `native_refresh_warning` is actionable but does not hold a lease or pause the worker.

9. Reopen the structured ACP console and ask for the native-only nonce:

   ```bash
   bin/acpctl session attach <session-id> --acp
   ```

   Submit: `What native-only nonce did I just ask you to remember? Reply only with it.` The answer must be `NATIVE-ACP-73`.

Native and ACP transports are intentionally advisory. Opening native mode never pauses or cancels ACP work, and opening ACP never seizes the native process. Do not submit simultaneous turns through both transports during this continuity test.
