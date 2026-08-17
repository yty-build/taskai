import crypto from "node:crypto";
import fs from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { Writable } from "node:stream";
import { pathToFileURL } from "node:url";

process.umask(0o077);

const packageRoot = process.argv[2];
const stateDir = process.argv[3];
if (!packageRoot || !stateDir) {
  throw new Error("acpx runtime bridge requires package root and state directory");
}

// Bridge commands use fd 3, leaving stdin exclusively for acpx's built-in
// host-operation prompts. acpx only offers those prompts when stdin/stderr are
// TTY-like; the synthetic input stays private and its prompt output is redacted.
const commandInput = fs.createReadStream(null, { fd: 3, autoClose: false });
const diagnosticStderr = process.stderr;
let permissionPromptOutput = false;
const promptStderr = new Writable({
  write(chunk, encoding, callback) {
    const text = Buffer.isBuffer(chunk) ? chunk.toString("utf8") : String(chunk);
    if (text.includes("[permission]")) {
      permissionPromptOutput = true;
    }
    if (!permissionPromptOutput) {
      diagnosticStderr.write(chunk, encoding);
    }
    callback();
  },
});
Object.defineProperties(promptStderr, {
  isTTY: { value: true },
  columns: { value: 80 },
  rows: { value: 24 },
});
Object.defineProperty(process.stdin, "isTTY", { value: true, configurable: true });
Object.defineProperty(process, "stderr", { value: promptStderr, configurable: true });

const maxCommandFrameBytes = 4 * 1024 * 1024;
const maxOutputFrameBytes = 8 * 1024 * 1024;
const maxPendingPermissions = 128;
const permissionKinds = new Set([
  "allow_once",
  "allow_always",
  "reject_once",
  "reject_always",
]);
const automaticallyResolvedReadKinds = new Set(["read", "search", "fetch", "think"]);
const readApprovalProviderModes = new Map([
  ["codex", "read-only"],
  ["claude", "default"],
]);

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
let handle;
let configuredPermissionMode = "approve-reads";
let activeTurn;
let activeTurnCommandId = "";
let activeRequestId = "";
let shuttingDown = false;
const pendingPermissions = new Map();

function encode(message) {
  return JSON.stringify(message);
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
  process.stdout.write(`${line}\n`);
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

function answerHostPrompt(approved) {
  permissionPromptOutput = false;
  if (!process.stdin.destroyed && !process.stdin.readableEnded) {
    process.stdin.push(approved ? "y\n" : "n\n");
  }
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
    if (pending?.source === "host") {
      pendingPermissions.delete(permissionId);
      pending.cleanup();
      answerHostPrompt(false);
    } else {
      settlePermission(permissionId, { outcome: "cancel" });
    }
  }
  // An unrecognized pinned-runtime host operation deliberately has no
  // approval card. If acpx nevertheless opened its private prompt, answer no
  // during cancellation so the turn cannot strand the worker.
  if (permissionPromptOutput) {
    answerHostPrompt(false);
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

function hostPermissionForRuntimeEvent(event) {
  if (
    configuredPermissionMode !== "approve-reads" ||
    !activeTurnCommandId ||
    !activeRequestId ||
    event?.type !== "status" ||
    typeof event.text !== "string"
  ) {
    return;
  }
  let method;
  let kind;
  let title;
  if (event.text.startsWith("fs/write_text_file running ")) {
    method = "fs/write_text_file";
    kind = "edit";
    title = "Write file";
  } else if (event.text.startsWith("terminal/create running ")) {
    method = "terminal/create";
    kind = "execute";
    title = "Run terminal command";
  } else {
    return;
  }
  // The structured running event precedes acpx's human-readable y/N prompt.
  // Suppress that prompt before it is written so paths, commands, and file
  // previews never enter the bounded diagnostic stderr capture.
  permissionPromptOutput = true;
  if (pendingPermissions.size >= maxPendingPermissions) {
    answerHostPrompt(false);
    return;
  }
  for (const pending of pendingPermissions.values()) {
    if (pending.source === "host") {
      throw new Error("multiple concurrent acpx host permission prompts are unsupported");
    }
  }
  const permissionId = `permission-${crypto.randomUUID()}`;
  const toolCallId = `host-${crypto.randomUUID()}`;
  const raw = {
    sessionId: handle?.backendSessionId || handle?.sessionKey || "",
    toolCall: { toolCallId, title, kind, status: "pending" },
    options: [
      { optionId: "host-allow-once", name: "Allow Once", kind: "allow_once" },
      { optionId: "host-reject-once", name: "Reject", kind: "reject_once" },
    ],
    _meta: { gateway: { hostMethod: method } },
  };
  pendingPermissions.set(permissionId, {
    source: "host",
    options: new Map(raw.options.map((option) => [option.optionId, option])),
    requestId: activeRequestId,
    turnCommandId: activeTurnCommandId,
    cleanup: () => {},
  });
  emit({
    type: "permission_request",
    commandId: activeTurnCommandId,
    requestId: activeRequestId,
    permissionId,
    inferredKind: kind,
    request: raw,
  });
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

function restrictiveOpenCodeAction(value) {
  if (value === "deny") {
    return "deny";
  }
  if (value && typeof value === "object" && !Array.isArray(value)) {
    const result = { "*": "ask" };
    for (const [pattern, action] of Object.entries(value)) {
      if (action === "deny") {
        result[pattern] = "deny";
      }
    }
    return result;
  }
  return "ask";
}

function configureOpenCodePermissions(command) {
  if (command.agent !== "opencode") {
    return;
  }
  let config = {};
  const inline = process.env.OPENCODE_CONFIG_CONTENT;
  if (inline) {
    try {
      const parsed = JSON.parse(inline);
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
        throw new Error("inline config must be a JSON object");
      }
      config = parsed;
    } catch {
      throw new Error("OPENCODE_CONFIG_CONTENT must contain a valid JSON object");
    }
  }
  if (configuredPermissionMode === "approve-all") {
    config.permission = "allow";
  } else if (config.permission !== "deny" && config.permission !== "ask") {
    const permission =
      config.permission && typeof config.permission === "object" && !Array.isArray(config.permission)
        ? { ...config.permission }
        : config.permission === "allow"
          ? { "*": "allow" }
          : {};
    permission.edit = restrictiveOpenCodeAction(permission.edit);
    permission.bash = restrictiveOpenCodeAction(permission.bash);
    config.permission = permission;
  }
  process.env.OPENCODE_CONFIG_CONTENT = JSON.stringify(config);
}

function configure(command) {
  if (runtime) {
    return;
  }
  applyEnvironment(command.environment);
  configuredPermissionMode = command.permissionMode === "approve-all" ? "approve-all" : "approve-reads";
  configureOpenCodePermissions(command);
  const agentRegistry = command.useBuiltin
    ? createAgentRegistry()
    : createAgentRegistry({ overrides: { [command.agent]: command.argv } });
  runtime = createAcpRuntime({
    cwd: command.cwd,
    sessionStore: createRuntimeStore({ stateDir }),
    agentRegistry,
    permissionMode: configuredPermissionMode,
    nonInteractivePermissions: "fail",
    onPermissionRequest: permissionRequest,
  });
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
  }
  handle = await runtime.ensureSession({
    sessionKey: command.sessionKey,
    agent: command.agent,
    mode: "persistent",
    cwd: command.cwd,
    resumeSessionId: command.resumeSessionId || undefined,
  });
  // Provider/user defaults can silently allow edits. Select each adapter's
  // restrictive mode so writes and commands cross session/request_permission.
  const readApprovalMode = command.useBuiltin ? readApprovalProviderModes.get(command.agent) : undefined;
  if (configuredPermissionMode === "approve-reads" && readApprovalMode) {
    await runtime.setMode({ handle, mode: readApprovalMode });
  }
  const status = await runtime.getStatus({ handle });
  emit({ type: "response", commandId: command.commandId, data: { handle, status } });
}

async function refresh(command) {
  if (!runtime || !handle) {
    throw new Error("acpx runtime Session is not ensured");
  }
  if (activeTurn) {
    throw new Error("cannot refresh while an acpx runtime turn is active");
  }
  cancelPendingPermissions("Session refreshed after native activity");
  await runtime.close({ handle, reason: "refresh after provider-native activity" });
  handle = undefined;
  await ensure(command);
}

async function runTurn(command) {
  if (!runtime || !handle) {
    throw new Error("acpx runtime Session is not ensured");
  }
  if (activeTurn) {
    throw new Error("an acpx runtime turn is already active");
  }
  activeTurnCommandId = command.commandId;
  activeRequestId = command.requestId;
  const turn = runtime.startTurn({
    handle,
    text: command.text,
    mode: "prompt",
    requestId: command.requestId,
  });
  activeTurn = turn;
  emit({ type: "turn_started", commandId: command.commandId, requestId: command.requestId });
  try {
    for await (const event of turn.events) {
      hostPermissionForRuntimeEvent(event);
      emit({
        type: "runtime_event",
        commandId: command.commandId,
        requestId: command.requestId,
        event,
      });
    }
    const result = await turn.result;
    emit({
      type: "turn_result",
      commandId: command.commandId,
      requestId: command.requestId,
      result,
    });
  } finally {
    cancelPendingPermissions("turn ended");
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
  if (pending.source === "host") {
    pendingPermissions.delete(command.permissionId);
    pending.cleanup();
    answerHostPrompt(decision.outcome === "allow_once" || decision.outcome === "allow_always");
    emit({
      type: "response",
      commandId: command.commandId,
      data: { accepted: true, permissionId: command.permissionId, optionId: command.optionId || "", outcome: decision.outcome },
    });
    return;
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
  } else if (runtime && handle) {
    await runtime.cancel({ handle, reason: command.reason || "cancelled by user" });
  }
  emit({ type: "response", commandId: command.commandId, data: { cancelled: true } });
}

async function inspect(command) {
  if (!runtime || !handle) {
    throw new Error("acpx runtime Session is not ensured");
  }
  const status = await runtime.getStatus({ handle });
  emit({ type: "response", commandId: command.commandId, data: { handle, status } });
}

async function shutdown(command) {
  if (shuttingDown) {
    return;
  }
  shuttingDown = true;
  cancelPendingPermissions("Session worker stopped");
  if (activeTurn) {
    await activeTurn.cancel({ reason: "Session worker stopped" });
  }
  if (runtime && handle) {
    await runtime.close({ handle, reason: "Session worker stopped" });
  }
  emit({ type: "response", commandId: command.commandId, data: { closed: true } });
  setTimeout(() => process.exit(0), 0);
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
      case "refresh":
        await refresh(command);
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
      case "inspect":
        await inspect(command);
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
