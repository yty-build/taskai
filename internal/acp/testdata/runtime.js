import fs from "node:fs";

function appendArguments(values) {
  const destination = process.env.FAKE_ACPX_ARGS_LOG;
  if (!destination) {
    return;
  }
  fs.appendFileSync(destination, `---\n${values.join("\n")}\n`, { mode: 0o600 });
}

function identifiers(resumeSessionId) {
  const acpSessionId = resumeSessionId || "acp-reconnected";
  return {
    acpxRecordId: "acpx-record",
    backendSessionId: acpSessionId,
    agentSessionId: "native-reconnected",
  };
}

function appendPermissionDecision(value) {
  const destination = process.env.FAKE_ACPX_PERMISSION_LOG;
  if (destination) {
    fs.appendFileSync(destination, `${JSON.stringify(value)}\n`, { mode: 0o600 });
  }
}

export function createAgentRegistry(options = {}) {
  return options;
}

export function createRuntimeStore(options = {}) {
  return options;
}

export function createAcpRuntime(options = {}) {
  let handle;
  let active;

  return {
    async ensureSession(session) {
      if (process.env.FAKE_ACPX_SCENARIO === "nonzero") {
        throw new Error("fixture ensure failed");
      }
      const ids = identifiers(session.resumeSessionId);
      handle = {
        sessionKey: session.sessionKey,
        runtimeSessionName: session.sessionKey,
        cwd: session.cwd,
        ...ids,
      };
      const configured = options.agentRegistry?.overrides?.[session.agent] || [];
      appendArguments([
        ...configured,
        ...(session.resumeSessionId ? ["--resume-session", session.resumeSessionId] : []),
      ]);
      return handle;
    },

    startTurn(turn) {
      let cancelled = false;
      const abort = new AbortController();
      appendArguments([turn.text]);
      const completion = {};
      let finishEvents;
      const eventsFinished = new Promise((resolve) => {
        finishEvents = resolve;
      });
      completion.events = (async function* () {
        try {
          await new Promise((resolve) => setTimeout(resolve, 10));
          if (cancelled) {
            return;
          }
          const scenario = process.env.FAKE_ACPX_SCENARIO;
          if (scenario === "permission" || scenario === "permission-duplicate" || scenario === "permission-read") {
            const duplicate = scenario === "permission-duplicate";
            const read = scenario === "permission-read";
            const raw = {
              sessionId: handle.backendSessionId,
              toolCall: { toolCallId: "tool-write", title: "Write harmless marker", kind: read ? "read" : "edit" },
              options: duplicate
                ? [
                    { optionId: "allow-a", name: "Allow A", kind: "allow_once" },
                    { optionId: "allow-b", name: "Allow B", kind: "allow_once" },
                  ]
                : [
                    { optionId: "allow-once", name: "Allow once", kind: "allow_once" },
                    { optionId: "allow-always", name: "Always allow", kind: "allow_always" },
                    { optionId: "reject-once", name: "Reject once", kind: "reject_once" },
                    { optionId: "reject-always", name: "Always reject", kind: "reject_always" },
                  ],
            };
            const decision = await options.onPermissionRequest?.(
              { sessionId: handle.backendSessionId, raw, inferredKind: read ? "read" : "edit" },
              { signal: abort.signal },
            );
            appendPermissionDecision(decision ?? { outcome: "automatic" });
            if (!cancelled) {
              yield { type: "text_delta", stream: "output", text: `permission:${decision?.outcome ?? "automatic"}`, tag: "agent_message_chunk" };
            }
            return;
          }
          yield { type: "text_delta", stream: "output", text: "ok", tag: "agent_message_chunk" };
        } finally {
          finishEvents();
        }
      })();
      completion.result = (async () => {
        await eventsFinished;
        return cancelled
          ? { status: "cancelled", stopReason: "cancelled" }
          : { status: "completed", stopReason: "end_turn" };
      })();
      completion.cancel = async () => {
        cancelled = true;
        abort.abort();
      };
      active = completion;
      return completion;
    },

    async getStatus() {
      return { summary: active ? "working" : "ready", ...identifiers(handle?.backendSessionId) };
    },

    async cancel() {
      await active?.cancel();
    },

    async close() {
      await active?.cancel();
    },
  };
}
