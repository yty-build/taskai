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
  const records = new Map();
  return {
    ...options,
    async load(sessionId) {
      const record = records.get(sessionId);
      return record ? structuredClone(record) : undefined;
    },
    async save(record) {
      const saved = structuredClone(record);
      for (const [key, candidate] of records) {
        if (key !== record.acpxRecordId && candidate.acpxRecordId === record.acpxRecordId) {
          records.set(key, structuredClone(saved));
        }
      }
      records.set(record.acpxRecordId, structuredClone(saved));
      if (record.name) records.set(record.name, structuredClone(saved));
      const destination = process.env.FAKE_ACPX_STORE_LOG;
      if (destination) {
        fs.appendFileSync(destination, `${JSON.stringify(saved)}\n`, { mode: 0o600 });
      }
    },
  };
}

export function createAcpRuntime(options = {}) {
  let handle;
  let active;
  let ensureCount = 0;
  let statusCount = 0;
  let configuredModel = "provider-default";
  let configuredEffort = "medium";
  let livePersistentConnection = false;
  let retainedPortAvailable = false;
  let retainedDropCountdown = -1;
  let retainedReadCount = 0;
  let connectionGeneration = 0;
  const manager = {
    createClient() {
      appendArguments(["--hidden-resume"]);
      throw new Error("fixture attempted a hidden provider reconnect");
    },
    async readPendingPersistentClient(record) {
      retainedReadCount += 1;
      const activationGate = process.env.FAKE_ACPX_PROMPT_ACTIVATION_GATE;
      if (activationGate && retainedReadCount === 2) {
        fs.writeFileSync(`${activationGate}.entered`, "entered\n", { mode: 0o600 });
        while (!fs.existsSync(activationGate)) {
          await new Promise((resolve) => setTimeout(resolve, 5));
        }
      }
      if (retainedDropCountdown === 0) {
        retainedPortAvailable = false;
        retainedDropCountdown = -1;
      } else if (retainedDropCountdown > 0) {
        retainedDropCountdown -= 1;
      }
      if (!retainedPortAvailable || record?.acpSessionId !== handle?.backendSessionId) return undefined;
      return {
        hasReusableSession(sessionId) {
          return retainedPortAvailable && sessionId === handle?.backendSessionId;
        },
      };
    },
  };

  function configOptions() {
    const driftAfter = Number.parseInt(process.env.FAKE_ACPX_CONFIG_OPTIONS_DRIFT_AFTER_STATUS || "", 10);
    const shape = Number.isFinite(driftAfter) && statusCount >= driftAfter
      ? process.env.FAKE_ACPX_CONFIG_OPTIONS_DRIFT_SHAPE
      : process.env.FAKE_ACPX_CONFIG_OPTIONS;
    if (!shape || shape === "none") return undefined;
    const grouped = shape === "grouped";
    const wrap = (values) => grouped ? [{ group: "available", name: "Available", options: values }] : values;
    const modelChoices = shape === "opaque" ? [
      { value: " model @ 2 ", name: "Opaque model" },
    ] : [
      { value: "provider-default", name: "Provider default" },
      { value: "model-selected", name: "Selected model" },
    ];
    if (shape === "duplicate") modelChoices.push({ value: "model-selected", name: "Duplicate model" });
    if (shape === "overflow") {
      for (let index = 0; index < 129; index += 1) modelChoices.push({ value: `model-${index}`, name: `Model ${index}` });
    }
    const result = [
      {
        id: "model",
        name: "Model",
        ...(grouped ? {} : { category: "model" }),
        type: "select",
		currentValue: shape === "invalid-current" ? "\u0000invalid" : configuredModel,
		options: wrap(modelChoices),
      },
      {
        id: "effort",
        name: "Reasoning effort",
        ...(grouped ? {} : { category: "thought_level" }),
        type: "select",
        currentValue: configuredEffort,
        options: wrap(shape === "opaque" ? [
          { value: " effort + 2 ", name: "Opaque effort" },
        ] : [
          { value: "low", name: "Low" },
          { value: "medium", name: "Medium" },
          { value: "high", name: "High" },
        ]),
      },
    ];
    if (shape === "conflicting-category") {
      result[0].category = "thought_level";
    }
    if (shape === "dual-semantic") {
      return [{
        id: "effort",
        name: "Ambiguous provider control",
        category: "model",
        type: "select",
        currentValue: configuredModel,
        options: [
          ...modelChoices,
          { value: "low", name: "Low" },
          { value: "medium", name: "Medium" },
          { value: "high", name: "High" },
        ],
      }];
    }
    if (shape === "ambiguous") {
      result.push({
        id: "reasoning_effort", name: "Other effort", type: "select", currentValue: configuredEffort,
        options: [{ value: "high", name: "High" }],
      });
    }
    return result;
  }

  return {
    async getManager() {
      return manager;
    },

    async ensureSession(session) {
      if (process.env.FAKE_ACPX_SCENARIO === "nonzero") {
        throw new Error("fixture ensure failed");
      }
      if (
        process.env.FAKE_ACPX_REJECT_INITIAL_SESSION_MODEL === "1" &&
        !session.resumeSessionId &&
        session.sessionOptions?.model
      ) {
        throw new Error("fixture received model before initial identity persistence");
      }
      if (
        process.env.FAKE_ACPX_EXPECT_REGISTRY &&
        options.agentRegistry?.registryKind !== process.env.FAKE_ACPX_EXPECT_REGISTRY
      ) {
        throw new Error(
          `fixture registry was ${options.agentRegistry?.registryKind}, expected ${process.env.FAKE_ACPX_EXPECT_REGISTRY}`,
        );
      }
      const existing = await options.sessionStore?.load(session.sessionKey);
      const reusedClosedRecord = existing && existing.acpx?.reset_on_next_ensure !== true;
      if (
        process.env.FAKE_ACPX_CONNECTION_SCOPED_CONFIG === "1" &&
        session.resumeSessionId &&
        livePersistentConnection
      ) {
        throw new Error("fixture observed overlapping exact-resume provider connections");
      }
      ensureCount += 1;
      if (ensureCount > 1 && process.env.FAKE_ACPX_RESET_CONFIG_ON_ENSURE === "1") {
        configuredModel = "provider-default";
        configuredEffort = "medium";
      }
      if (process.env.FAKE_ACPX_CONNECTION_SCOPED_CONFIG === "1") {
        livePersistentConnection = !reusedClosedRecord;
        if (livePersistentConnection) {
          connectionGeneration += 1;
          configuredModel = session.sessionOptions?.model || "provider-default";
          configuredEffort = "medium";
        }
      }
      if (process.env.FAKE_ACPX_SCENARIO === "activation-conflict") {
        const error = new Error("SECRET provider activation conflict detail");
        error.code = "turn_failed";
        error.detailCode = "conversation_conflict";
        error.retryable = true;
        throw error;
      }
      const ids = identifiers(session.resumeSessionId);
      handle = {
        sessionKey: session.sessionKey,
        runtimeSessionName: session.sessionKey,
        cwd: session.cwd,
        ...ids,
      };
	  retainedPortAvailable = true;
      if (!reusedClosedRecord) {
        await options.sessionStore?.save({
          acpxRecordId: handle.acpxRecordId,
          acpSessionId: handle.backendSessionId,
          name: session.sessionKey,
          cwd: handle.cwd,
          agentCommand: session.agent,
          createdAt: `created-${ensureCount}`,
          lastSeq: 0,
          eventLog: { activePath: `stream-${ensureCount}` },
          pid: ensureCount,
          messages: [],
          updated_at: `empty-${ensureCount}`,
          cumulative_token_usage: { input_tokens: 0 },
          request_token_usage: {},
          acpx: { current_model_id: configuredModel },
        });
      }
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
      const normalizeHiddenReconnectFailure = process.env.FAKE_ACPX_DROP_RETAINED_DURING_TURN === "1";
      if (!normalizeHiddenReconnectFailure) {
        appendArguments(process.env.FAKE_ACPX_CONNECTION_SCOPED_CONFIG === "1"
          ? ["--connection", String(connectionGeneration), "--prompt", turn.text]
          : [turn.text]);
      }
      if (
        process.env.FAKE_ACPX_REQUIRE_CONFIGURED_TUPLE === "1" &&
        (configuredModel !== "model-selected" || configuredEffort !== "high")
      ) {
        throw new Error(`fixture prompt received provider tuple ${configuredModel}/${configuredEffort}`);
      }
      const historySaved = (async () => {
        if (normalizeHiddenReconnectFailure) return;
        if (process.env.FAKE_ACPX_RECORD_HISTORY !== "1") return;
        const record = await options.sessionStore.load(handle.sessionKey);
        const expectedMessages = turn.text === "second prompt" ? 2 : turn.text === "third prompt" ? 4 : 0;
        if (!record || !Array.isArray(record.messages) || record.messages.length !== expectedMessages) {
          throw new Error(`fixture record history had ${record?.messages?.length ?? "no"} messages, expected ${expectedMessages}`);
        }
        record.messages.push({ User: { id: `${turn.requestId}-user`, content: [{ Text: turn.text }] } });
        record.messages.push({ Agent: { content: [{ Text: "ok" }], tool_results: {} } });
        record.lastSeq = expectedMessages + 2;
        record.lastRequestId = turn.requestId;
        record.lastPromptAt = `prompt-${expectedMessages + 2}`;
        record.updated_at = `conversation-${expectedMessages + 2}`;
        record.cumulative_token_usage = { input_tokens: 17 + expectedMessages };
        record.request_token_usage = { ...record.request_token_usage, [turn.requestId]: { input_tokens: 17 } };
        await options.sessionStore.save(record);
      })();
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
      let finishCancellation;
      const eventsFinished = new Promise((resolve) => {
        finishEvents = resolve;
      });
      const cancellationFinished = new Promise((resolve) => {
        finishCancellation = resolve;
      });
      completion.events = (async function* () {
        try {
          if (normalizeHiddenReconnectFailure) {
            // Model pinned acpx 0.13's real failure boundary. The runtime
            // catches the strict createClient rejection inside its async turn
            // task, drains/closes the event queue, and normalizes the original
            // coded error to an ordinary RUNTIME failed result.
            retainedPortAvailable = false;
            try {
              await manager.createClient();
            } catch {
              return;
            }
          }
          await historySaved;
          if (process.env.FAKE_ACPX_DROP_RETAINED_ON_CANCEL === "1") {
            yield { type: "status", status: "working" };
            await cancellationFinished;
            return;
          }
          await new Promise((resolve) => setTimeout(resolve, 10));
          if (cancelled) {
            return;
          }
          const scenario = process.env.FAKE_ACPX_SCENARIO;
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
        if (normalizeHiddenReconnectFailure) {
          return { status: "failed", error: { code: "RUNTIME", message: "normalized runtime failure" } };
        }
		if (process.env.FAKE_ACPX_SCENARIO === "prompt-no-session") {
		  return { status: "failed", error: { code: "NO_SESSION", message: "fixture conversation disappeared" } };
		}
        if (process.env.FAKE_ACPX_DROP_RETAINED_AFTER_TURN === "1") {
          // Allow exactly the bridge's healthy completion postcondition, then
          // deterministically lose the Port on the next command preflight.
          retainedDropCountdown = 1;
        }
        return cancelled
          ? { status: "cancelled", stopReason: "cancelled" }
          : { status: "completed", stopReason: "end_turn" };
      })();
      completion.cancel = async () => {
        cancelled = true;
        if (process.env.FAKE_ACPX_DROP_RETAINED_ON_CANCEL === "1") {
          retainedPortAvailable = false;
        }
        finishCancellation();
        abort.abort();
      };
      active = completion;
      return completion;
    },

    async getStatus() {
	  statusCount += 1;
      return {
        summary: active ? "working" : "ready",
        acpxRecordId: handle?.acpxRecordId,
        backendSessionId: handle?.backendSessionId,
        ...(process.env.FAKE_ACPX_OMIT_MODEL_STATUS === "1" ? {} : {
          models: { currentModelId: configuredModel, availableModelIds: ["provider-default", "model-selected"] },
        }),
        details: { configOptions: configOptions() },
      };
    },

    async setConfigOption({ key, value }) {
	  if (process.env.FAKE_ACPX_DROP_RETAINED_DURING_CONFIG === "1") {
		retainedPortAvailable = false;
		manager.createClient();
	  }
      if (process.env.FAKE_ACPX_CONNECTION_SCOPED_CONFIG === "1" && !livePersistentConnection) {
        // Model the pinned runtime's idle-control path: every call creates a
        // separate exact-resume client and the Claude-like adapter reconnects
        // with its default model on each client.
        configuredModel = "provider-default";
        configuredEffort = "medium";
        connectionGeneration += 1;
      }
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
      appendArguments(process.env.FAKE_ACPX_CONNECTION_SCOPED_CONFIG === "1"
        ? ["--connection", String(connectionGeneration), "--set-config", key, value]
        : ["--set-config", key, value]);
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
	  retainedPortAvailable = false;
      if (process.env.FAKE_ACPX_CONNECTION_SCOPED_CONFIG === "1" && livePersistentConnection) {
        appendArguments(["--close-connection", String(connectionGeneration)]);
      }
      livePersistentConnection = false;
    },
  };
}
