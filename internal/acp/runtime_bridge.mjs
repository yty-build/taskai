import crypto from "node:crypto";
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

process.umask(0o077);

const packageRoot = process.argv[2];
const stateDir = process.argv[3];
if (!packageRoot || !stateDir) {
  throw new Error("acpx runtime bridge requires package root and state directory");
}

// Bridge commands use fd 3. The gateway relies only on acpx's typed
// onPermissionRequest callback; it never parses human terminal prompts or
// pretends a provider status string is a permission protocol.
const commandInput = fs.createReadStream(null, { fd: 3, autoClose: false });

const maxCommandFrameBytes = 4 * 1024 * 1024;
const maxOutputFrameBytes = 1 * 1024 * 1024;
const maxQueuedOutputBytes = 4 * 1024 * 1024;
const maxPendingPermissions = 128;
const permissionKinds = new Set([
  "allow_once",
  "allow_always",
  "reject_once",
  "reject_always",
]);
const automaticallyResolvedReadKinds = new Set(["read", "search", "fetch", "think"]);

const require = createRequire(path.join(packageRoot, "package.json"));
const runtimeURL = pathToFileURL(require.resolve("acpx/runtime")).href;
const { createAcpRuntime, createAgentRegistry, createRuntimeStore } = await import(runtimeURL);

// Adapter-backed agents are launched through npx. Keep its cache under the
// gateway's private runtime root so an unrelated or incorrectly owned global
// npm cache cannot prevent a Session from starting.
const npmCacheDir = path.join(path.dirname(stateDir), "npm-cache");
fs.mkdirSync(npmCacheDir, { recursive: true, mode: 0o700 });
fs.chmodSync(npmCacheDir, 0o700);
process.env.npm_config_cache = npmCacheDir;

let runtime;
let runtimeStore;
let handle;
let configuredPermissionMode = "approve-reads";
let activeTurn;
let activatingTurn;
let activeTurnCommandId = "";
let activeRequestId = "";
let shuttingDown = false;
let retainedPortManager;
const pendingPermissions = new Map();
const outputQueue = [];
let queuedOutputBytes = 0;
let outputBlocked = false;
let outputExitCode;
let outputOverflowed = false;

const strictRetainedPortMarker = Symbol("acpGatewayStrictRetainedPort");

function retainedPortUnavailable(operation) {
  const error = new Error(`the retained provider Port is unavailable during ${operation}; explicitly resume the Session before sending provider commands`);
  error.code = "retained_port_unavailable";
  error.retryable = false;
  return error;
}

function encode(message) {
  return JSON.stringify(message);
}

function requestBridgeExit(code) {
  outputExitCode = code;
  flushOutputQueue();
}

function failOutputBackpressure() {
  if (outputOverflowed) return;
  outputOverflowed = true;
  process.stderr.write("acpx runtime bridge output exceeded its bounded backpressure queue\n");
  for (const permissionId of [...pendingPermissions.keys()]) {
    settlePermission(permissionId, { outcome: "cancel" });
  }
  void activeTurn?.cancel({ reason: "bridge output backpressure exceeded" }).catch(() => {});
  commandInput.destroy();
  setTimeout(() => process.exit(1), 0);
}

function flushOutputQueue() {
  if (outputBlocked || outputOverflowed) return;
  while (outputQueue.length > 0) {
    const next = outputQueue.shift();
    queuedOutputBytes -= next.bytes;
    if (!process.stdout.write(next.line)) {
      outputBlocked = true;
      process.stdout.once("drain", () => {
        outputBlocked = false;
        flushOutputQueue();
      });
      return;
    }
  }
  if (outputExitCode !== undefined) {
    const code = outputExitCode;
    outputExitCode = undefined;
    process.stdout.write("", () => process.exit(code));
  }
}

function emit(message) {
  let line = encode(message);
  if (Buffer.byteLength(line, "utf8") > maxOutputFrameBytes) {
    line = encode({
      type: "error",
      commandId: message.commandId || "",
      requestId: message.requestId || "",
      code: "bridge_frame_too_large",
      message: "acpx runtime event exceeded the gateway bridge frame limit",
    });
  }
  const framed = `${line}\n`;
  const bytes = Buffer.byteLength(framed, "utf8");
  if (queuedOutputBytes + process.stdout.writableLength + bytes > maxQueuedOutputBytes) {
    failOutputBackpressure();
    return false;
  }
  outputQueue.push({ line: framed, bytes });
  queuedOutputBytes += bytes;
  flushOutputQueue();
  return true;
}

function failure(commandId, error, requestId = "") {
  const boundedCode = (value) => {
    if (typeof value !== "string") return undefined;
    const trimmed = value.trim();
    return trimmed && Buffer.byteLength(trimmed, "utf8") <= 128 ? trimmed : undefined;
  };
  emit({
    type: "error",
    commandId,
    requestId,
    message: error instanceof Error ? error.message : String(error),
    code: boundedCode(error?.code),
    detailCode: boundedCode(error?.detailCode),
    retryable: error?.retryable === true,
  });
}

function settlePermission(permissionId, decision) {
  const pending = pendingPermissions.get(permissionId);
  if (!pending || pending.source !== "acp") {
    return false;
  }
  pendingPermissions.delete(permissionId);
  pending.cleanup();
  pending.resolve(decision);
  return true;
}

function cancelPendingPermissions(reason = "cancelled") {
  for (const permissionId of [...pendingPermissions.keys()]) {
    const pending = pendingPermissions.get(permissionId);
    if (pending) {
      emit({
        type: "permission_abandoned",
        commandId: pending.turnCommandId,
        requestId: pending.requestId,
        permissionId,
        reason,
      });
    }
	settlePermission(permissionId, { outcome: "cancel" });
  }
}

function validatedPermission(raw) {
  if (
    !raw ||
    !raw.toolCall ||
    typeof raw.toolCall.toolCallId !== "string" ||
    raw.toolCall.toolCallId.length === 0 ||
    raw.toolCall.toolCallId.length > 512 ||
    (raw.toolCall.title !== undefined && (typeof raw.toolCall.title !== "string" || raw.toolCall.title.length > 4096)) ||
    (raw.toolCall.kind !== undefined && (typeof raw.toolCall.kind !== "string" || raw.toolCall.kind.length > 64)) ||
    (raw.sessionId !== undefined && (typeof raw.sessionId !== "string" || raw.sessionId.length > 512)) ||
    !Array.isArray(raw.options) ||
    raw.options.length === 0 ||
    raw.options.length > 16
  ) {
    return undefined;
  }
  const byId = new Map();
  const kinds = new Set();
  const safeOptions = [];
  for (const option of raw.options) {
    if (
      !option ||
      typeof option.optionId !== "string" ||
      option.optionId.length === 0 ||
      option.optionId.length > 256 ||
      typeof option.name !== "string" ||
      option.name.length === 0 ||
      option.name.length > 1024 ||
      !permissionKinds.has(option.kind) ||
      byId.has(option.optionId) ||
      kinds.has(option.kind)
    ) {
      return undefined;
    }
    const safeOption = { optionId: option.optionId, name: option.name, kind: option.kind };
    byId.set(option.optionId, safeOption);
    safeOptions.push(safeOption);
    kinds.add(option.kind);
  }
  return {
    byId,
    request: {
      sessionId: typeof raw.sessionId === "string" ? raw.sessionId : "",
      toolCall: {
        toolCallId: raw.toolCall.toolCallId,
        title: typeof raw.toolCall.title === "string" ? raw.toolCall.title : "",
        kind: typeof raw.toolCall.kind === "string" ? raw.toolCall.kind : "",
      },
      options: safeOptions,
    },
  };
}

function waitForPermissionDecision(validated, inferredKind, signal, commandId, requestId) {
  const permissionId = `permission-${crypto.randomUUID()}`;
  return new Promise((resolve) => {
    const onAbort = () => settlePermission(permissionId, { outcome: "cancel" });
    signal.addEventListener("abort", onAbort, { once: true });
    pendingPermissions.set(permissionId, {
      source: "acp",
      resolve,
      options: validated.byId,
      requestId,
      turnCommandId: commandId,
      cleanup: () => signal.removeEventListener("abort", onAbort),
    });
    emit({
      type: "permission_request",
      commandId,
      requestId,
      permissionId,
      inferredKind,
      request: validated.request,
    });
  });
}

function permissionRequest(request, context) {
  if (configuredPermissionMode === "approve-all") {
    return undefined;
  }
  if (automaticallyResolvedReadKinds.has(request.inferredKind)) {
    return undefined;
  }
  if (!activeTurn || !activeTurnCommandId || !activeRequestId) {
    return { outcome: "cancel" };
  }

  const validated = validatedPermission(request.raw);
  if (!validated || pendingPermissions.size >= maxPendingPermissions) {
    return { outcome: "cancel" };
  }
  const inferredKind = typeof request.inferredKind === "string" ? request.inferredKind.slice(0, 64) : "";
  return waitForPermissionDecision(validated, inferredKind, context.signal, activeTurnCommandId, activeRequestId);
}

function applyEnvironment(environment) {
  if (!environment) {
    return;
  }
  for (const [name, value] of Object.entries(environment)) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || typeof value !== "string" || value.includes("\0")) {
      throw new Error("invalid provider environment override");
    }
    process.env[name] = value;
  }
}

function configure(command) {
  if (runtime) {
    return;
  }
  applyEnvironment(command.environment);
  configuredPermissionMode = command.permissionMode === "approve-all" ? "approve-all" : "approve-reads";
  const agentRegistry = command.useBuiltin
    ? createAgentRegistry()
    : createAgentRegistry({ overrides: { [command.agent]: command.argv } });
  runtimeStore = createRuntimeStore({ stateDir });
  runtime = createAcpRuntime({
    cwd: command.cwd,
    sessionStore: runtimeStore,
    agentRegistry,
    permissionMode: configuredPermissionMode,
    nonInteractivePermissions: "fail",
    onPermissionRequest: permissionRequest,
  });
}

async function prepareStrictRetainedPortMode() {
  if (retainedPortManager) return retainedPortManager;
  if (!runtime || typeof runtime.getManager !== "function") {
    throw new Error("pinned acpx runtime does not expose its retained-Port manager");
  }
  const manager = await runtime.getManager();
  if (
    !manager || typeof manager.createClient !== "function" ||
    typeof manager.readPendingPersistentClient !== "function"
  ) {
    throw new Error("pinned acpx runtime retained-Port manager shape is incompatible");
  }
  retainedPortManager = manager;
  return manager;
}

function activateStrictRetainedPortMode(manager) {
  if (manager[strictRetainedPortMarker] === true) return;
  // acpx 0.13 normally reconnects from Prompt/configure when its pending
  // persistent client disappears. That is useful for acpx as a standalone
  // runtime but violates this gateway's one-Port contract: only an explicit
  // worker Resume may reconnect. All hidden fallback paths create a client
  // through this pinned manager method, so disabling it after the one allowed
  // ensure makes transport loss fail closed.
  manager.createClient = () => {
    throw retainedPortUnavailable("provider command");
  };
  manager[strictRetainedPortMarker] = true;
}

async function requireRetainedPort(operation) {
  if (!runtime || !runtimeStore || !handle) {
    throw retainedPortUnavailable(operation);
  }
  const manager = await prepareStrictRetainedPortMode();
  const record = await runtimeStore.load(handle.acpxRecordId);
  if (
    !record || record.acpxRecordId !== handle.acpxRecordId ||
    record.acpSessionId !== handle.backendSessionId
  ) {
    throw retainedPortUnavailable(operation);
  }
  const client = await manager.readPendingPersistentClient(record, { consume: false });
  if (!client) throw retainedPortUnavailable(operation);
}

async function requireExactResumeConnection(command) {
  if (!command.resumeSessionId) return;
  if (!runtimeStore) {
    throw new Error("acpx runtime store is unavailable for exact resume activation");
  }
  const record = await runtimeStore.load(command.sessionKey);
  if (!record) return;
  if (record.acpSessionId !== command.resumeSessionId) {
    throw new Error("persisted acpx record does not match the requested exact ACP conversation");
  }
  // acpx 0.13's public ensureSession reuses an existing persistent record
  // without reconnecting it. Its public idle control methods then each open
  // and close a separate exact-resume connection, so interdependent provider
  // controls cannot be applied and verified as one tuple. The public session
  // store exposes the pinned record's reset_on_next_ensure lifecycle flag.
  // Setting it makes ensureSession resume this same ACP ID on one retained
  // persistent connection; it does not close or replace the canonical
  // provider conversation, alter gateway JSONL, or permit a new identity.
  record.acpx = { ...record.acpx, reset_on_next_ensure: true };
  await runtimeStore.save(record);
}

function advertisedConfigOptions(status) {
  const options = status?.details?.configOptions;
  return Array.isArray(options) ? options : [];
}

function advertisedSelectValues(option) {
  if (!option || option.type !== "select" || !Array.isArray(option.options)) {
    return [];
  }
  const values = [];
  for (const candidate of option.options) {
    if (typeof candidate?.value === "string" && candidate.value !== "") {
      values.push(candidate.value);
      continue;
    }
    if (Array.isArray(candidate?.options)) {
      for (const nested of candidate.options) {
        if (typeof nested?.value === "string" && nested.value !== "") values.push(nested.value);
      }
    }
  }
  return values;
}

const maxProviderCatalogChoices = 128;

function boundedProviderValue(value, limit) {
  if (typeof value !== "string") return "";
  if (value === "" || Buffer.byteLength(value, "utf8") > limit || /[\p{C}]/u.test(value)) return "";
  return value;
}

function boundedProviderDisplay(value, limit, fallback = "") {
  if (value === undefined || value === null || value === "") return fallback;
  if (typeof value !== "string") return null;
  const clean = value.trim();
  if (!clean) return fallback;
  if (Buffer.byteLength(clean, "utf8") > limit || /[\p{C}]/u.test(clean)) return null;
  return clean;
}

function catalogForConfigOption(status, category, fallbackIds, reasoningEffort) {
  let option;
  try {
    option = uniqueConfigOption(status, category, fallbackIds, reasoningEffort ? "reasoning effort" : "model");
  } catch {
    return { supported: false, unavailable_reason: "ambiguous_or_invalid", choices: [] };
  }
  if (!option || option.type !== "select" || !Array.isArray(option.options)) {
    return { supported: false, unavailable_reason: "not_advertised", choices: [] };
  }
  const choices = [];
  const seen = new Set();
  let invalid = false;
  let overflow = false;
  const addChoice = (candidate, group) => {
    if (choices.length >= maxProviderCatalogChoices) {
      overflow = true;
      return;
    }
    const value = boundedProviderValue(candidate?.value, reasoningEffort ? 128 : 256);
    if (!value || seen.has(value)) {
      invalid = true;
      return;
    }
    const label = boundedProviderDisplay(candidate?.name, 256, value);
    if (label === null) {
      invalid = true;
      return;
    }
    seen.add(value);
    choices.push({
      value,
      label,
      ...(group ? { group } : {}),
    });
  };
  for (const candidate of option.options) {
    if (Array.isArray(candidate?.options)) {
      const group = boundedProviderDisplay(candidate?.name ?? candidate?.group, 128);
      if (group === null) {
        invalid = true;
        continue;
      }
      for (const nested of candidate.options) addChoice(nested, group);
    } else {
      addChoice(candidate, "");
    }
  }
  if (invalid || overflow || choices.length === 0) {
    return { supported: false, unavailable_reason: "ambiguous_or_invalid", choices: [] };
  }
  const currentValue = boundedProviderValue(option.currentValue, reasoningEffort ? 128 : 256);
  if (option.currentValue !== undefined && option.currentValue !== null && option.currentValue !== "" && !currentValue) {
    return { supported: false, unavailable_reason: "ambiguous_or_invalid", choices: [] };
  }
  return { supported: true, ...(currentValue ? { current_value: currentValue } : {}), choices };
}

function providerOptionCatalog(status) {
  try {
    validateProviderConfigSemantics(status);
  } catch {
    const unavailable = { supported: false, unavailable_reason: "ambiguous_or_invalid", choices: [] };
    return { model: unavailable, reasoning_effort: unavailable };
  }
  return {
    model: catalogForConfigOption(status, "model", ["model"], false),
    reasoning_effort: catalogForConfigOption(
      status,
      "thought_level",
      ["reasoning_effort", "effort", "thinking", "thought_level"],
      true,
    ),
  };
}

const recognizedProviderConfigCategories = new Set(["model", "thought_level"]);

function matchingConfigOptions(status, category, fallbackIds) {
  const options = advertisedConfigOptions(status);
  const ids = new Set(fallbackIds);
  const matches = [];
  for (const option of options) {
    if (!option || typeof option.id !== "string" || option.id.trim().length === 0) continue;
    const id = option.id.trim();
    const optionCategory = typeof option.category === "string" ? option.category.trim() : "";
    const fallbackMatch = ids.has(id);
    if (fallbackMatch && recognizedProviderConfigCategories.has(optionCategory) && optionCategory !== category) {
      throw new Error("the provider advertised contradictory model and reasoning option semantics");
    }
    if (optionCategory === category || fallbackMatch) matches.push(option);
  }
  return matches;
}

function validateProviderConfigSemantics(status) {
  const modelOptions = matchingConfigOptions(status, "model", ["model"]);
  const effortOptions = matchingConfigOptions(
    status,
    "thought_level",
    ["reasoning_effort", "effort", "thinking", "thought_level"],
  );
  const effortSet = new Set(effortOptions);
  if (modelOptions.some((option) => effortSet.has(option))) {
    throw new Error("the provider advertised one control as both model and reasoning effort");
  }
}

function uniqueConfigOption(status, category, fallbackIds, label) {
  validateProviderConfigSemantics(status);
  const matches = matchingConfigOptions(status, category, fallbackIds);
  if (matches.length === 1) return matches[0];
  throw new Error(
    `the provider did not advertise exactly one ${label} option; leave ${label} on Provider default or update the provider adapter`,
  );
}

function validateRequestedProviderCatalog(status, requestedModel, requestedEffort) {
  const catalog = providerOptionCatalog(status);
  const includes = (selector, requested) =>
    selector?.supported === true && Array.isArray(selector.choices) &&
    selector.choices.some((choice) => choice?.value === requested);
  if (requestedModel && !includes(catalog.model, requestedModel)) {
    throw new Error("the provider's current model catalog does not contain the requested selection");
  }
  if (requestedEffort && !includes(catalog.reasoning_effort, requestedEffort)) {
    throw new Error("the provider's current reasoning catalog does not contain the requested selection");
  }
  return catalog;
}

async function applyRequestedProviderOptions(command) {
  let status = await runtime.getStatus({ handle });
  const requestedModel = typeof command.model === "string" ? command.model : "";
  const requestedEffort = typeof command.reasoningEffort === "string" ? command.reasoningEffort : "";
  validateRequestedProviderCatalog(status, requestedModel, requestedEffort);
  if (requestedModel) {
    const modelOption = uniqueConfigOption(status, "model", ["model"], "model");
    if (!advertisedSelectValues(modelOption).includes(requestedModel)) {
      throw new Error(`the provider did not advertise requested model ${JSON.stringify(requestedModel)}; choose Provider default or another advertised model`);
    }
    // Apply the exact advertised token explicitly. Configuration belongs to
    // this live provider Port; the gateway does not invent aliases or rewrite
    // provider-owned values.
    await runtime.setConfigOption({ handle, key: modelOption.id, value: requestedModel });
    status = await runtime.getStatus({ handle });
    validateRequestedProviderCatalog(status, requestedModel, requestedEffort);
    const appliedModelOption = uniqueConfigOption(status, "model", ["model"], "model");
    const appliedByOption = appliedModelOption.currentValue === requestedModel;
    const appliedByStatus = status?.models?.currentModelId === requestedModel;
    if (!appliedByOption && !appliedByStatus) {
      throw new Error(`the provider did not apply requested model ${JSON.stringify(requestedModel)}`);
    }
  }

  if (requestedEffort) {
    const effortOption = uniqueConfigOption(status, "thought_level", ["reasoning_effort", "effort", "thinking", "thought_level"], "reasoning effort");
    if (!advertisedSelectValues(effortOption).includes(requestedEffort)) {
      throw new Error(`the provider did not advertise requested reasoning effort ${JSON.stringify(requestedEffort)}; choose Provider default or another advertised effort`);
    }
    // See the model branch above: pass the provider-owned token unchanged.
    await runtime.setConfigOption({ handle, key: effortOption.id, value: requestedEffort });
    status = await runtime.getStatus({ handle });
    validateRequestedProviderCatalog(status, requestedModel, requestedEffort);
    const applied = uniqueConfigOption(status, "thought_level", ["reasoning_effort", "effort", "thinking", "thought_level"], "reasoning effort");
    if (applied.currentValue !== requestedEffort) {
      throw new Error(`the provider did not apply requested reasoning effort ${JSON.stringify(requestedEffort)}`);
    }
  }

  // Some adapters expose interdependent controls: applying effort may reset
  // the model (or vice versa). Verify the complete requested tuple only after
  // every mutation, immediately before the worker is allowed to send a turn.
  status = await runtime.getStatus({ handle });
  validateRequestedProviderCatalog(status, requestedModel, requestedEffort);
  if (requestedModel) {
    const finalModelOption = uniqueConfigOption(status, "model", ["model"], "model");
    const finalModelByOption = finalModelOption.currentValue === requestedModel;
    const finalModelByStatus = status?.models?.currentModelId === requestedModel;
    if (!finalModelByOption && !finalModelByStatus) {
      throw new Error(`the provider did not retain requested model ${JSON.stringify(requestedModel)}`);
    }
  }
  if (requestedEffort) {
    const finalEffortOption = uniqueConfigOption(status, "thought_level", ["reasoning_effort", "effort", "thinking", "thought_level"], "reasoning effort");
    if (finalEffortOption.currentValue !== requestedEffort) {
      throw new Error(`the provider did not retain requested reasoning effort ${JSON.stringify(requestedEffort)}`);
    }
  }
  return status;
}

async function ensure(command) {
  configure(command);
  if (handle) {
    const sameSession =
      handle.sessionKey === command.sessionKey &&
      (!command.cwd || !handle.cwd || path.resolve(handle.cwd) === path.resolve(command.cwd));
    if (!sameSession) {
      throw new Error("runtime bridge is already bound to a different Task Console Session");
    }
    if (command.resumeSessionId && handle.backendSessionId !== command.resumeSessionId) {
      throw new Error("runtime bridge is already bound to a different ACP conversation");
    }
    await requireRetainedPort("Session reuse");
    const status = await runtime.getStatus({ handle });
    emit({ type: "response", commandId: command.commandId, data: { handle, status } });
    return;
  }
  const manager = await prepareStrictRetainedPortMode();
  await requireExactResumeConnection(command);
  handle = await runtime.ensureSession({
    sessionKey: command.sessionKey,
    agent: command.agent,
    mode: "persistent",
    cwd: command.cwd,
    resumeSessionId: command.resumeSessionId || undefined,
  });
  activateStrictRetainedPortMode(manager);
  await requireRetainedPort("Session startup");
  const status = await runtime.getStatus({ handle });
  emit({ type: "response", commandId: command.commandId, data: { handle, status } });
}

async function configureProviderOptions(command) {
  if (!runtime || !handle) {
    throw new Error("acpx runtime Session is not ensured");
  }
  if (activeTurn) {
    throw new Error("cannot configure provider options while an acpx runtime turn is active");
  }
  await requireRetainedPort("provider configuration");
  const status = await applyRequestedProviderOptions(command);
  await requireRetainedPort("provider configuration completion");
  emit({ type: "response", commandId: command.commandId, data: { handle, status } });
}

async function providerOptions(command) {
  if (!runtime || !handle) {
    throw new Error("acpx runtime Session is not ensured");
  }
  if (activeTurn) {
    throw new Error("cannot inspect provider options while an acpx runtime turn is active");
  }
  await requireRetainedPort("provider option discovery");
  const status = await runtime.getStatus({ handle });
  await requireRetainedPort("provider option discovery completion");
  emit({
    type: "response",
    commandId: command.commandId,
    data: { handle, status, providerOptions: providerOptionCatalog(status) },
  });
}

async function runTurn(command) {
  if (!runtime || !handle) {
    throw new Error("acpx runtime Session is not ensured");
  }
  if (activeTurn || activatingTurn) {
    throw new Error("an acpx runtime turn is already active");
  }
  // Reserve the turn synchronously before the first asynchronous preflight.
  // Command lines are handled concurrently so Cancel can otherwise observe no
  // active turn, acknowledge a no-op, and let this prompt start afterward.
  const activation = { cancelled: false, reason: "" };
  activatingTurn = activation;
  activeTurnCommandId = command.commandId;
  activeRequestId = command.requestId;
  try {
    await requireRetainedPort("prompt dispatch");
    if (activation.cancelled) {
      emit({
        type: "turn_result",
        commandId: command.commandId,
        requestId: command.requestId,
        result: { status: "cancelled", stopReason: "cancelled" },
      });
      return;
    }
    const turn = runtime.startTurn({
      handle,
      text: command.text,
      mode: "prompt",
      requestId: command.requestId,
    });
    activeTurn = turn;
    emit({ type: "turn_started", commandId: command.commandId, requestId: command.requestId });
    for await (const event of turn.events) {
      emit({
        type: "runtime_event",
        commandId: command.commandId,
        requestId: command.requestId,
        event,
      });
    }
    const result = await turn.result;
    await requireRetainedPort("prompt completion");
    emit({
      type: "turn_result",
      commandId: command.commandId,
      requestId: command.requestId,
      result,
    });
  } finally {
    cancelPendingPermissions("turn ended");
    if (activatingTurn === activation) activatingTurn = undefined;
    activeTurn = undefined;
    activeTurnCommandId = "";
    activeRequestId = "";
  }
}

async function respondPermission(command) {
  const pending = pendingPermissions.get(command.permissionId);
  if (!pending) {
    throw new Error(`permission ${command.permissionId} is no longer pending`);
  }
  let decision = { outcome: "cancel" };
  if (!command.cancelled) {
    const option = pending.options.get(command.optionId);
    if (!option) {
      throw new Error(`permission option ${JSON.stringify(command.optionId)} was not offered`);
    }
    decision = { outcome: option.kind };
  }
  if (!settlePermission(command.permissionId, decision)) {
    throw new Error(`permission ${command.permissionId} is no longer pending`);
  }
  emit({
    type: "response",
    commandId: command.commandId,
    data: { accepted: true, permissionId: command.permissionId, optionId: command.optionId || "", outcome: decision.outcome },
  });
}

async function cancelTurn(command) {
  cancelPendingPermissions(command.reason || "turn cancelled");
  if (activeTurn) {
    await activeTurn.cancel({ reason: command.reason || "cancelled by user" });
  } else if (activatingTurn) {
    activatingTurn.cancelled = true;
    activatingTurn.reason = command.reason || "cancelled by user";
  } else if (runtime && handle) {
    await runtime.cancel({ handle, reason: command.reason || "cancelled by user" });
  }
  emit({ type: "response", commandId: command.commandId, data: { cancelled: true } });
}

async function shutdown(command) {
  if (shuttingDown) {
    return;
  }
  shuttingDown = true;
  cancelPendingPermissions("Session worker stopped");
  if (activeTurn) {
    await activeTurn.cancel({ reason: "Session worker stopped" });
  } else if (activatingTurn) {
    activatingTurn.cancelled = true;
    activatingTurn.reason = "Session worker stopped";
  }
  if (runtime && handle) {
    await runtime.close({ handle, reason: "Session worker stopped" });
  }
  emit({ type: "response", commandId: command.commandId, data: { closed: true } });
  requestBridgeExit(0);
}

async function handleCommand(command) {
  try {
    if (!command || typeof command !== "object" || typeof command.type !== "string" || typeof command.commandId !== "string") {
      throw new Error("invalid bridge command envelope");
    }
    switch (command.type) {
      case "ensure":
        await ensure(command);
        break;
      case "configure":
        await configureProviderOptions(command);
        break;
      case "provider_options":
        await providerOptions(command);
        break;
      case "turn":
        await runTurn(command);
        break;
      case "permission_decision":
        await respondPermission(command);
        break;
      case "cancel":
        await cancelTurn(command);
        break;
      case "shutdown":
        await shutdown(command);
        break;
      default:
        throw new Error(`unknown bridge command ${JSON.stringify(command.type)}`);
    }
  } catch (error) {
    failure(command?.commandId || "", error, command?.requestId || "");
  }
}

function handleLine(line) {
  if (Buffer.byteLength(line, "utf8") > maxCommandFrameBytes) {
    failure("", new Error("gateway bridge command exceeded the frame limit"));
    void shutdown({ commandId: "" });
    return;
  }
  try {
    void handleCommand(JSON.parse(line));
  } catch (error) {
    failure("", error);
  }
}

let inputBuffer = "";
commandInput.setEncoding("utf8");
commandInput.on("data", (chunk) => {
  inputBuffer += chunk;
  if (Buffer.byteLength(inputBuffer, "utf8") > maxCommandFrameBytes && !inputBuffer.includes("\n")) {
    failure("", new Error("gateway bridge command exceeded the frame limit"));
    void shutdown({ commandId: "" });
    return;
  }
  for (;;) {
    const newline = inputBuffer.indexOf("\n");
    if (newline < 0) {
      break;
    }
    const line = inputBuffer.slice(0, newline);
    inputBuffer = inputBuffer.slice(newline + 1);
    if (line.trim() !== "") {
      handleLine(line);
    }
  }
});

commandInput.on("end", () => {
  cancelPendingPermissions("bridge input closed");
  process.exit(0);
});

process.on("SIGTERM", () => {
  cancelPendingPermissions("bridge terminated");
  process.exit(0);
});

emit({ type: "ready" });
