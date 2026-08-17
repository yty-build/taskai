import fs from "node:fs";
import readline from "node:readline/promises";

function appendArguments(values) {
  const destination = process.env.FAKE_ACPX_ARGS_LOG;
  if (!destination) {
    return;
  }
  fs.appendFileSync(destination, `---\n${values.join("\n")}\n`, { mode: 0o600 });
}

function identifiers(resumeSessionId, nativeSessionId) {
  const acpSessionId = resumeSessionId || "acp-reconnected";
  return {
    acpxRecordId: "acpx-record",
    backendSessionId: acpSessionId,
    agentSessionId: nativeSessionId || process.env.FAKE_ACPX_NATIVE_ID || "native-reconnected",
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
  let ensureCount = 0;
  let statusCount = 0;
  let configuredModel = "provider-default";
  let configuredEffort = "medium";

  function configOptions() {
    const shape = process.env.FAKE_ACPX_CONFIG_OPTIONS;
    if (!shape || shape === "none") return undefined;
    const grouped = shape === "grouped";
    const wrap = (values) => grouped ? [{ group: "available", name: "Available", options: values }] : values;
    const result = [
      {
        id: "model",
        name: "Model",
        ...(grouped ? {} : { category: "model" }),
        type: "select",
        currentValue: configuredModel,
        options: wrap([
          { value: "provider-default", name: "Provider default" },
          { value: "model-selected", name: "Selected model" },
        ]),
      },
      {
        id: "effort",
        name: "Reasoning effort",
        ...(grouped ? {} : { category: "thought_level" }),
        type: "select",
        currentValue: configuredEffort,
        options: wrap([
          { value: "low", name: "Low" },
          { value: "medium", name: "Medium" },
          { value: "high", name: "High" },
        ]),
      },
    ];
    if (shape === "ambiguous") {
      result.push({
        id: "reasoning_effort", name: "Other effort", type: "select", currentValue: configuredEffort,
        options: [{ value: "high", name: "High" }],
      });
    }
    return result;
  }

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
      ensureCount += 1;
      if (ensureCount > 1 && process.env.FAKE_ACPX_RESET_CONFIG_ON_ENSURE === "1") {
        configuredModel = "provider-default";
        configuredEffort = "medium";
      }
      if (
        process.env.FAKE_ACPX_SCENARIO === "activation-conflict" ||
        (process.env.FAKE_ACPX_SCENARIO === "activation-conflict-on-refresh" && ensureCount > 1)
      ) {
        const error = new Error("SECRET provider activation conflict detail");
        error.code = "turn_failed";
        error.detailCode = "conversation_conflict";
        error.retryable = true;
        throw error;
      }
      const nativeSessionId =
        process.env.FAKE_ACPX_SCENARIO === "identity-drift-on-refresh" && ensureCount > 1
          ? "native-replacement"
          : undefined;
      const ids = identifiers(session.resumeSessionId, nativeSessionId);
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
      if (process.env.FAKE_ACPX_SCENARIO === "prompt-provider-conflict") {
        const error = new Error("SECRET provider prompt conflict detail");
        error.code = "turn_failed";
        error.detailCode = "provider_busy";
        error.retryable = true;
        throw error;
      }
      if (process.env.FAKE_ACPX_SCENARIO === "prompt-bridge-frame-error") {
        const error = new Error("invalid bridge frame");
        error.code = "bridge_frame_too_large";
        throw error;
      }
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
          if (scenario === "permission-overflow") {
            const decisions = await Promise.all(Array.from({ length: 129 }, (_, index) => options.onPermissionRequest?.(
              {
                sessionId: handle.backendSessionId,
                raw: {
                  sessionId: handle.backendSessionId,
                  toolCall: { toolCallId: `tool-${index}`, title: `Write ${index}`, kind: "edit", ignored: "x".repeat(10000) },
                  options: [{ optionId: "reject-once", name: "Reject", kind: "reject_once", ignored: "z".repeat(10000) }],
                  ignored: "y".repeat(10000),
                },
                inferredKind: "edit",
              },
              { signal: abort.signal },
            )));
            appendPermissionDecision({ overflow_outcomes: decisions.map((decision) => decision?.outcome ?? "automatic") });
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
	  statusCount += 1;
	  const inspectedNativeId =
	    process.env.FAKE_ACPX_SCENARIO === "identity-drift-on-inspect" && statusCount > 1
	      ? "native-replacement"
	      : handle?.agentSessionId;
      return {
        summary: active ? "working" : "ready",
        acpxRecordId: handle?.acpxRecordId,
        backendSessionId: handle?.backendSessionId,
	    agentSessionId: inspectedNativeId,
        ...(process.env.FAKE_ACPX_OMIT_MODEL_STATUS === "1" ? {} : {
          models: { currentModelId: configuredModel, availableModelIds: ["provider-default", "model-selected"] },
        }),
        details: { configOptions: configOptions() },
      };
    },

    async setConfigOption({ key, value }) {
      const option = configOptions()?.find((candidate) => candidate.id === key);
      const values = option?.options?.flatMap((candidate) => Array.isArray(candidate?.options) ? candidate.options : [candidate])
        .map((candidate) => candidate?.value);
      if (!option || !values?.includes(value)) {
        throw new Error(`fixture rejected config ${key}=${value}`);
      }
      if (key === "model") configuredModel = value;
      else {
        configuredEffort = value;
        if (process.env.FAKE_ACPX_EFFORT_RESETS_MODEL === "1") configuredModel = "provider-default";
      }
      appendArguments(["--set-config", key, value]);
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
      if (process.env.FAKE_ACPX_SCENARIO === "refresh-hang") {
        await new Promise(() => {});
      }
    },
  };
}
