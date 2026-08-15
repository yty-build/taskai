import fs from "node:fs";
import readline from "node:readline/promises";

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
  return { ...options, registryKind: options.overrides ? "override" : "builtin" };
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
      if (
        process.env.FAKE_ACPX_EXPECT_REGISTRY &&
        options.agentRegistry?.registryKind !== process.env.FAKE_ACPX_EXPECT_REGISTRY
      ) {
        throw new Error(
          `fixture registry was ${options.agentRegistry?.registryKind}, expected ${process.env.FAKE_ACPX_EXPECT_REGISTRY}`,
        );
      }
      if (process.env.FAKE_ACPX_EXPECT_OPENCODE_PERMISSION) {
        const config = JSON.parse(process.env.OPENCODE_CONFIG_CONTENT || "{}");
        const expected = process.env.FAKE_ACPX_EXPECT_OPENCODE_PERMISSION;
        if (expected === "read-approval") {
          const expectsDenyRules = process.env.FAKE_ACPX_EXPECT_OPENCODE_DENY_RULES === "1";
          if (
            config.permission?.read !== "allow" ||
            (!expectsDenyRules && (config.permission?.edit !== "ask" || config.permission?.bash !== "ask"))
          ) {
            throw new Error(`unexpected OpenCode read-approval config: ${JSON.stringify(config.permission)}`);
          }
        } else if (expected === "approve-all" && config.permission !== "allow") {
          throw new Error(`unexpected OpenCode approve-all config: ${JSON.stringify(config.permission)}`);
        }
        if (
          process.env.FAKE_ACPX_EXPECT_OPENCODE_DENY_RULES === "1" &&
          (config.permission?.edit?.["*"] !== "ask" ||
            config.permission?.edit?.["*.secret"] !== "deny" ||
            config.permission?.bash !== "deny" ||
            config.model !== "keep-me")
        ) {
          throw new Error(`OpenCode deny rules were not preserved: ${JSON.stringify(config)}`);
        }
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
          if (
            scenario === "host-permission" ||
            scenario === "host-permission-terminal" ||
            scenario === "host-permission-unknown"
          ) {
            const method =
              scenario === "host-permission-terminal"
                ? "terminal/create"
                : scenario === "host-permission-unknown"
                  ? "fs/delete"
                  : "fs/write_text_file";
            const detail =
              scenario === "host-permission-terminal"
                ? "terminal/create running terminal: printf safe"
                : scenario === "host-permission-unknown"
                  ? "fs/delete running delete: /private/worktree/marker"
                : "fs/write_text_file running write_text_file: /private/worktree/marker";
            yield {
              type: "status",
              tag: "client_operation",
              text: detail,
            };
            process.stderr.write(`\n[permission] Allow ${method}?\n`);
            const prompt = readline.createInterface({ input: process.stdin, output: process.stderr });
            const answer = await prompt.question("Allow write? (y/N) ");
            prompt.close();
            appendPermissionDecision({ host_answer: answer });
            if (!cancelled) {
              yield { type: "text_delta", stream: "output", text: `host:${answer}`, tag: "agent_message_chunk" };
            }
            return;
          }
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

    async setMode({ mode }) {
      if (process.env.FAKE_ACPX_REJECT_SET_MODE === "1") {
        throw new Error(`fixture did not expect setMode(${mode})`);
      }
      if (process.env.FAKE_ACPX_EXPECT_MODE && mode !== process.env.FAKE_ACPX_EXPECT_MODE) {
        throw new Error(`fixture mode was ${mode}, expected ${process.env.FAKE_ACPX_EXPECT_MODE}`);
      }
      appendArguments(["--set-mode", mode]);
    },

    async cancel() {
      await active?.cancel();
    },

    async close() {
      await active?.cancel();
    },
  };
}
