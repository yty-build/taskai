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

   Confirm that the initial nonce response appears as committed provider
   history, with any not-yet-committed ACP activity identified as provisional.
   Then press `q`. Detaching this viewer must not stop the worker, cancel work,
   or stop the provider process.

5. Make the ACP turn deterministically wait at a write permission. In a second
   terminal, follow the raw Session journal:

   ```bash
   bin/acpctl session logs <session-id> --follow --raw
   ```

   In a third terminal, submit this prompt:

   ```bash
   bin/acpctl session prompt <session-id> \
     "Use your write tool to create .agent-native-overlap-smoke containing overlap-check. Do not choose a permission for me."
   ```

   Wait until the raw follower shows `permission_requested` for that request.
   Do not approve it. While that correlated ACP turn is still active, attempt
   to open a new real Codex harness:

   ```bash
   bin/acpctl session attach <session-id>
   ```

   The gateway must reject this new generation immediately with
   wait-then-open guidance. The attempt is not queued, launches no Codex
   process, and does not cancel the ACP turn. Cancel it explicitly and wait for
   its terminal `cancelled` record:

   ```bash
   bin/acpctl session cancel <session-id>
   ```

   Then run the same attach command again. This time it must verify
   the canonical ID in Codex's local Session store and run
   `codex resume <canonical-id>` in the persisted checkout. Ask Codex:
   `What nonce did I ask you to remember? Reply only with it.` The answer must
   be `ACP-NATIVE-42`.

6. Add a harmless native-only nonce, `NATIVE-ACP-73`, and wait for Codex to
   finish that turn. If tmux is installed, press `Ctrl-B d`: this detaches the
   viewer but deliberately leaves the now-idle provider process and its
   exclusive native ownership running.

7. While that Codex process is still open but idle, submit an ACP prompt from
   another terminal:

   ```bash
   bin/acpctl session prompt <session-id> \
     "What native-only nonce did I just ask you to remember? Include it, then reply with marker ACP-AFTER-NATIVE-51. Do not use tools."
   ```

   The gateway must reject the submission immediately with exit-and-retry
   guidance. It must not return an accepted request ID, append prompt lifecycle
   records, enter the ACP FIFO, or send anything through the bridge. This proves
   that tmux detach preserves native ownership rather than silently changing
   write surfaces.

8. Reattach to that exact native harness and exit Codex normally without
   sending another provider prompt:

   ```bash
   bin/acpctl session attach <session-id>
   ```

   The gateway must not kill the process automatically. A second tmux detach is
   still not an exit. After normal exit, inspect raw Session activity:

   ```bash
   bin/acpctl session logs <session-id> --raw
   ```

   Expect `native_released`, followed asynchronously by
   `provider_transcript_reconciled` or
   `provider_transcript_reconcile_warning`. A failed provider read must not emit
   `provider_transcript_reconciled`; it retains the last validated structured
   view. The read-only transcript reconciliation result is not the ACP write
   gate: exact native release clears ownership, and the later exact ACP
   rehydration gates dispatch.

9. Retry the ACP prompt from step 7. After it reaches the ACP FIFO head, the
   gateway must refresh the idle ACP adapter on the exact persisted ACP Session,
   falling back to exact Resume if necessary. It must validate the same
   canonical identity, emit correlated `acp_rehydrated` before `turn_started`,
   and only then forward the prompt. The response must include `NATIVE-ACP-73`
   and `ACP-AFTER-NATIVE-51`. A delayed provider-transcript reconciliation may
   finish later and must not delay this post-release ACP dispatch.

10. Reopen the structured ACP console:

   ```bash
   bin/acpctl session attach <session-id> --acp
   ```

   This load must read the committed Codex provider JSONL snapshot and join the
   gateway Session-JSONL lifecycle/provisional overlay; it must not treat the
   gateway journal as the committed transcript. The view must show one copy of
   each turn without a visible reload or duplicate, retaining the prior
   validated view if a snapshot refresh is temporarily unavailable.

11. Open a fresh native process and ask for `ACP-AFTER-NATIVE-51`. It must
    resume the same canonical ID and see the committed ACP turn. This creates a
    fresh native generation and uses a fresh exact `BuildResume` invocation;
    no old process was reused or killed.

Native and ACP are distinct transports to one canonical provider conversation.
The ACP runtime permits one active ACP turn, so later ACP prompts use an
ACP-to-ACP FIFO. A native owner or reservation never joins that queue: the
second cross-surface activation is rejected immediately. Viewer detach changes
neither provider-process lifetime nor exclusive native ownership. Normal
provider exit releases ownership; every later ACP prompt still performs exact
Refresh/fallback Resume and identity validation before dispatch.
