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

const maxCommandFrameBytes = 4 * 1024 * 1024;
const maxOutputFrameBytes = 8 * 1024 * 1024;
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
  emit({
    type: "error",
    commandId,
    requestId,
    message: error instanceof Error ? error.message : String(error),
    code: typeof error?.code === "string" ? error.code : undefined,
  });
}

function settlePermission(permissionId, decision) {
  const pending = pendingPermissions.get(permissionId);
  if (!pending) {
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

function validatedPermissionOptions(raw) {
  if (
    !raw ||
    !raw.toolCall ||
    typeof raw.toolCall.toolCallId !== "string" ||
    raw.toolCall.toolCallId.length === 0 ||
    raw.toolCall.toolCallId.length > 512 ||
    (raw.toolCall.title !== undefined && (typeof raw.toolCall.title !== "string" || raw.toolCall.title.length > 4096)) ||
    !Array.isArray(raw.options) ||
    raw.options.length === 0 ||
    raw.options.length > 16
  ) {
    return undefined;
  }
  const byId = new Map();
  const kinds = new Set();
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
    byId.set(option.optionId, option);
    kinds.add(option.kind);
  }
  return byId;
}

async function permissionRequest(request, context) {
  if (configuredPermissionMode === "approve-all") {
    return undefined;
  }
  if (automaticallyResolvedReadKinds.has(request.inferredKind)) {
    return undefined;
  }
  if (!activeTurn || !activeTurnCommandId || !activeRequestId) {
    return { outcome: "cancel" };
  }

  const raw = request.raw;
  const options = validatedPermissionOptions(raw);
  if (!options) {
    return { outcome: "cancel" };
  }

  const permissionId = `permission-${crypto.randomUUID()}`;
  return await new Promise((resolve) => {
    const onAbort = () => settlePermission(permissionId, { outcome: "cancel" });
    context.signal.addEventListener("abort", onAbort, { once: true });
    pendingPermissions.set(permissionId, {
      resolve,
      options,
      raw,
      requestId: activeRequestId,
      turnCommandId: activeTurnCommandId,
      cleanup: () => context.signal.removeEventListener("abort", onAbort),
    });
    emit({
      type: "permission_request",
      commandId: activeTurnCommandId,
      requestId: activeRequestId,
      permissionId,
      inferredKind: request.inferredKind,
      request: raw,
    });
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

function configure(command) {
  if (runtime) {
    return;
  }
  applyEnvironment(command.environment);
  configuredPermissionMode = command.permissionMode === "approve-all" ? "approve-all" : "approve-reads";
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
  const status = await runtime.getStatus({ handle });
  emit({ type: "response", commandId: command.commandId, data: { handle, status } });
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
  if (activeTurn) {
    await activeTurn.cancel({ reason: command.reason || "cancelled by user" });
  } else if (runtime && handle) {
    await runtime.cancel({ handle, reason: command.reason || "cancelled by user" });
  }
  cancelPendingPermissions(command.reason || "turn cancelled");
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
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
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

process.stdin.on("end", () => {
  cancelPendingPermissions("bridge input closed");
  process.exit(0);
});

process.on("SIGTERM", () => {
  cancelPendingPermissions("bridge terminated");
  process.exit(0);
});

emit({ type: "ready" });
