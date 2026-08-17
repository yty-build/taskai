"use strict";

const page = document.body.dataset.page;
const textEncoder = new TextEncoder();
const LIMITS = Object.freeze({
  assistant: 128 << 10, thought: 32 << 10, tool: 64 << 10, toolTotal: 256 << 10,
  plan: 128 << 10, diagnostics: 1000, turns: 256, blocksPerTurn: 512,
  toolsPerTurn: 256, permissions: 128, retained: 16 << 20, announcement: 768,
  reconcileEvents: 2048, reconcileBytes: 4 << 20
});

async function apiRequest(path, method = "GET", body) {
  const options = { method, headers: { Accept: "application/json" } };
  if (body !== undefined) {
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  const response = await fetch(path, options);
  const responseText = await response.text();
  let payload = {};
  if (responseText) {
    try { payload = JSON.parse(responseText); } catch (_) { payload = {}; }
  }
  if (!response.ok) {
    const message = payload.error?.message || `Request failed (${response.status})`;
    const error = new Error(message);
    error.status = response.status;
    error.code = payload.error?.code || "";
    error.details = payload.error || {};
    throw error;
  }
  return payload;
}

function setNotice(element, message, isError = false) {
  if (!element) return;
  element.textContent = message || "";
  element.classList.toggle("error", isError);
  element.classList.toggle("has-action", false);
}

function offerRepositorySuggestion(notice, input, error) {
  const path = error?.code === "repository_selection_required" ? error?.details?.suggested_repo_path : "";
  if (!notice || !input || typeof path !== "string" || path.length === 0) return false;
  setNotice(notice, "");
  const explanation = document.createTextNode("That folder contains one Git repository. ");
  const use = document.createElement("button");
  use.type = "button";
  use.className = "repository-suggestion-button";
  use.textContent = `Use ${path}`;
  use.addEventListener("click", () => {
    input.value = path;
    input.focus();
    setNotice(notice, "Repository selected. Review the path, then press Start Session again.");
  });
  notice.classList.toggle("has-action", true);
  notice.replaceChildren(explanation, use);
  return true;
}

const sessionStartWarningMessages = Object.freeze({
  session_start_failed: "This Session was created but could not start. Its saved status and any established provider identity are shown here; inspect them before trying again.",
  session_started_with_warning: "This Session started, but its Task status update did not complete. The conversation remains available.",
});

function rememberSessionStartWarning(result) {
  const sessionID = typeof result?.session?.id === "string" ? result.session.id : "";
  const code = typeof result?.warning?.code === "string" ? result.warning.code : "";
  if (!sessionID || sessionID.length > 256 || !sessionStartWarningMessages[code]) return false;
  try {
    window.sessionStorage?.setItem(`session-start-warning:${sessionID}`, code);
    return true;
  } catch (_) {
    return false;
  }
}

function restoreSessionStartWarning(sessionID, notice = document.querySelector("#action-notice")) {
  if (!sessionID || !notice) return false;
  const key = `session-start-warning:${sessionID}`;
  let code = "";
  try {
    code = window.sessionStorage?.getItem(key) || "";
    window.sessionStorage?.removeItem(key);
  } catch (_) {
    return false;
  }
  const message = sessionStartWarningMessages[code];
  if (!message) return false;
  setNotice(notice, message, code === "session_start_failed");
  return true;
}

function permissionActionErrorMessage(error) {
  if (error?.status === 409 && error?.code !== "worker_upgrade_required") {
    return "That permission was already decided by another viewer.";
  }
  return error?.message || "Permission decision failed.";
}

function statusTone(status) {
  if (status === "working" || status === "starting") return "active";
  if (["waiting", "review", "open", "stopped"].includes(status)) return "waiting";
  if (status === "done") return "done";
  if (status === "failed" || status === "cancelled") return "danger";
  return "neutral";
}

function statusLabel(status) {
  return ({ starting: "Starting", working: "Working", waiting: "Ready", review: "In review", open: "Open", stopped: "Stopped", done: "Complete", failed: "Failed", cancelled: "Cancelled" })[status] || "Unknown";
}

function setStatus(element, status) {
  if (!element) return;
  element.className = `status status-${statusTone(status)}`;
  const dot = document.createElement("i");
  element.replaceChildren(dot, document.createTextNode(statusLabel(status)));
}

function agentInitial(value) {
  return Array.from(String(value || "A").trim())[0]?.toUpperCase() || "A";
}

function findNavSession(sessionID) {
  const target = `/sessions/${encodeURIComponent(sessionID)}`;
  return Array.from(document.querySelectorAll(".nav-session")).find((link) => link.getAttribute("href") === target) || null;
}

function bindShell() {
  const sidebar = document.querySelector("#app-sidebar");
  const main = document.querySelector(".main-pane");
  const mobileNavigation = window.matchMedia("(max-width: 760px)");
  const dialogReturnFocus = new WeakMap();
  let sidebarOpener = null;
  const sidebarTriggers = Array.from(document.querySelectorAll("[data-open-sidebar]"));
  const setSidebarExpanded = (expanded) => sidebarTriggers.forEach((button) => button.setAttribute("aria-expanded", String(expanded)));
  const closeSidebar = () => {
    if (!document.body.classList.contains("sidebar-open")) return;
    document.body.classList.remove("sidebar-open");
    setSidebarExpanded(false);
    if (main) main.inert = false;
    if (sidebar && mobileNavigation.matches) sidebar.inert = true;
    sidebar?.removeAttribute("role");
    sidebar?.removeAttribute("aria-modal");
    if (mobileNavigation.matches) sidebar?.setAttribute("aria-hidden", "true");
    const target = sidebarOpener;
    sidebarOpener = null;
    window.setTimeout(() => target?.focus(), 0);
  };
  const openSidebar = (button) => {
    sidebarOpener = button;
    document.body.classList.add("sidebar-open");
    setSidebarExpanded(true);
    if (main) main.inert = true;
    if (sidebar) sidebar.inert = false;
    sidebar?.setAttribute("aria-hidden", "false");
    sidebar?.setAttribute("role", "dialog");
    sidebar?.setAttribute("aria-modal", "true");
    window.setTimeout(() => sidebar?.querySelector(".sidebar-close, a, button")?.focus(), 0);
  };
  for (const button of sidebarTriggers) button.addEventListener("click", () => openSidebar(button));
  for (const button of document.querySelectorAll("[data-close-sidebar]")) button.addEventListener("click", closeSidebar);
  const syncSidebarMode = () => {
    if (!mobileNavigation.matches) {
      document.body.classList.remove("sidebar-open");
      setSidebarExpanded(false);
      if (main) main.inert = false;
      if (sidebar) sidebar.inert = false;
      sidebar?.removeAttribute("aria-hidden");
      sidebar?.removeAttribute("role");
      sidebar?.removeAttribute("aria-modal");
    } else if (!document.body.classList.contains("sidebar-open")) {
      if (sidebar) sidebar.inert = true;
      sidebar?.setAttribute("aria-hidden", "true");
    }
  };
  syncSidebarMode();
  mobileNavigation.addEventListener?.("change", syncSidebarMode);
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && document.body.classList.contains("sidebar-open")) {
      closeSidebar();
      return;
    }
    if (event.key === "Tab" && document.body.classList.contains("sidebar-open") && sidebar) {
      const focusable = Array.from(sidebar.querySelectorAll('a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'));
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    }
  });

  for (const button of document.querySelectorAll("[data-open-dialog]")) {
    button.addEventListener("click", () => {
      const target = document.getElementById(button.dataset.openDialog);
      if (!(target instanceof HTMLDialogElement)) return;
      const source = button.closest("dialog");
      dialogReturnFocus.set(target, source instanceof HTMLDialogElement ? (dialogReturnFocus.get(source) || document.querySelector(`[data-open-dialog="${source.id}"]`)) : button);
      if (source instanceof HTMLDialogElement && source.open && source !== target) source.close();
      if (!target.open) target.showModal();
      const focusTarget = target.querySelector("input:not([type=checkbox]), textarea, select, button:not(.icon-button)");
      window.setTimeout(() => focusTarget?.focus(), 0);
    });
  }
  for (const button of document.querySelectorAll("[data-close-dialog]")) {
    button.addEventListener("click", () => button.closest("dialog")?.close());
  }
  for (const dialog of document.querySelectorAll("dialog")) {
    dialog.addEventListener("close", () => {
      const target = dialogReturnFocus.get(dialog);
      if (target && !document.querySelector("dialog[open]")) window.setTimeout(() => target.focus(), 0);
    });
    dialog.addEventListener("click", (event) => {
      if (event.target !== dialog) return;
      const bounds = dialog.getBoundingClientRect();
      const inside = event.clientX >= bounds.left && event.clientX <= bounds.right && event.clientY >= bounds.top && event.clientY <= bounds.bottom;
      if (!inside) dialog.close();
    });
  }
}

function bindTaskCreation() {
  const form = document.querySelector("#create-task-form");
  const notice = document.querySelector("#task-form-notice");
  if (!form) return;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = form.querySelector("button[type=submit]");
    submit.disabled = true;
    setNotice(notice, "Creating Task…");
    try {
      const data = new FormData(form);
      const result = await apiRequest("/api/v1/tasks", "POST", {
        title: data.get("title"), description: data.get("description"), external_ref: data.get("external_ref")
      });
      window.location.assign(`/tasks/${encodeURIComponent(result.task.id)}`);
    } catch (error) {
      setNotice(notice, error.message, true);
      submit.disabled = false;
    }
  });
}

function bindSessionCreation(taskID) {
  const form = document.querySelector("#create-session-form");
  const notice = document.querySelector("#session-form-notice");
  if (!form || !taskID) return;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = form.querySelector("button[type=submit]");
    submit.disabled = true;
    setNotice(notice, "Starting Session and connecting the agent…");
    try {
      const data = new FormData(form);
      const result = await apiRequest(`/api/v1/tasks/${encodeURIComponent(taskID)}/sessions`, "POST", {
        agent: data.get("agent"), role: data.get("role"), repo_path: data.get("repo_path"), prompt: data.get("prompt"),
        model: data.get("model"), reasoning_effort: data.get("reasoning_effort"),
        worktree: data.has("worktree"), approve_all: data.has("approve_all"), allow_shared_checkout: data.has("allow_shared_checkout")
      });
      rememberSessionStartWarning(result);
      window.location.assign(`/sessions/${encodeURIComponent(result.session.id)}`);
    } catch (error) {
      const repoInput = form.querySelector('input[name="repo_path"]');
      if (!offerRepositorySuggestion(notice, repoInput, error)) setNotice(notice, error.message, true);
      submit.disabled = false;
    }
  });
}

function makeSessionRow(item) {
  const link = document.createElement("a");
  link.className = item.role === "reviewer" ? "session-row reviewer" : "session-row";
  link.href = `/sessions/${encodeURIComponent(item.id)}`;
  link.dataset.sessionId = item.id;

  const avatar = document.createElement("span");
  avatar.className = "provider-avatar";
  avatar.textContent = agentInitial(item.agent);

  const copy = document.createElement("span");
  copy.className = "session-row-copy";
  const title = document.createElement("strong");
  title.append(document.createTextNode(item.agent), document.createTextNode(" · "));
  const role = document.createElement("small");
  role.textContent = item.role;
  title.append(role);
  const context = document.createElement("span");
  const path = item.repo_path || "No checkout";
  const checkout = path.split("/").filter(Boolean).pop() || path;
  context.textContent = `${checkout}${item.worktree_path ? " · isolated worktree" : ""}`;
  copy.append(title, context);

  const state = document.createElement("span");
  state.className = "session-row-state";
  const status = document.createElement("span");
  setStatus(status, item.status);
  const permission = document.createElement("small");
  permission.textContent = item.permission;
  state.append(status, permission);

  const chevron = document.createElement("span");
  chevron.className = "row-chevron";
  chevron.textContent = "›";
  link.append(avatar, copy, state, chevron);
  return link;
}

function updateSessionRow(link, item) {
  link.className = item.role === "reviewer" ? "session-row reviewer" : "session-row";
  link.href = `/sessions/${encodeURIComponent(item.id)}`;
  link.dataset.sessionId = item.id;
  const avatar = link.querySelector(".provider-avatar");
  if (avatar) avatar.textContent = agentInitial(item.agent);
  const title = link.querySelector(".session-row-copy > strong");
  if (title) {
    title.replaceChildren(document.createTextNode(item.agent), document.createTextNode(" · "));
    const role = document.createElement("small");
    role.textContent = item.role;
    title.append(role);
  }
  const context = link.querySelector(".session-row-copy > span");
  if (context) {
    const path = item.repo_path || "No checkout";
    const checkout = path.split("/").filter(Boolean).pop() || path;
    context.textContent = `${checkout}${item.worktree_path ? " · isolated worktree" : ""}`;
  }
  setStatus(link.querySelector(".session-row-state .status"), item.status);
  const permission = link.querySelector(".session-row-state small");
  if (permission) permission.textContent = item.permission;
}

let taskSessionSignature = "";
async function refreshTask(taskID) {
  const result = await apiRequest(`/api/v1/tasks/${encodeURIComponent(taskID)}`);
  setStatus(document.querySelector("#task-status"), result.task.status);
  const count = document.querySelector("#session-count");
  if (count) count.textContent = String(result.sessions.length);
  const signature = JSON.stringify(result.sessions.map((item) => [item.id, item.status, item.agent, item.role, item.permission, item.updated_at]));
  if (signature === taskSessionSignature) return;
  taskSessionSignature = signature;
  for (const item of result.sessions) {
    const navLink = findNavSession(item.id);
    const dot = navLink?.querySelector(".status-dot");
    if (dot) {
      dot.className = `status-dot status-${statusTone(item.status)}`;
      dot.setAttribute("aria-label", statusLabel(item.status));
    }
  }
  const list = document.querySelector("#session-list");
  if (!list) return;
  if (result.sessions.length === 0) {
    if (list.querySelector(".welcome-empty")) return;
    const fragment = document.createDocumentFragment();
    const empty = document.createElement("div");
    empty.className = "welcome-empty compact";
    const icon = document.createElement("span");
    icon.className = "empty-orbit";
    icon.textContent = "◎";
    const heading = document.createElement("h2");
    heading.textContent = "No Sessions yet";
    const detail = document.createElement("p");
    detail.textContent = "Start one agent conversation under this Task.";
    const action = document.createElement("button");
    action.type = "button";
    action.className = "primary";
    action.textContent = "Start the first Session";
    action.addEventListener("click", () => document.querySelector("#create-session-dialog")?.showModal());
    empty.append(icon, heading, detail, action);
    fragment.append(empty);
    list.replaceChildren(fragment);
  } else {
    list.querySelector(".welcome-empty")?.remove();
    const existing = new Map(Array.from(list.querySelectorAll(".session-row[data-session-id]")).map((row) => [row.dataset.sessionId, row]));
    const desired = [];
    for (const item of result.sessions) {
      const row = existing.get(item.id) || makeSessionRow(item);
      updateSessionRow(row, item);
      desired.push(row);
      existing.delete(item.id);
    }
    for (const row of existing.values()) row.remove();
    for (let index = 0; index < desired.length; index += 1) {
      if (list.children[index] !== desired[index]) list.insertBefore(desired[index], list.children[index] || null);
    }
  }
  setNotice(document.querySelector("#task-live-status"), "Session list updated.");
}

function truncateUTF8(value, limit) {
  if (!value || limit <= 0) return { text: "", bytes: 0, clipped: Boolean(value) };
  const encoded = textEncoder.encode(value);
  if (encoded.length <= limit) return { text: value, bytes: encoded.length, clipped: false };
  let text = "";
  let bytes = 0;
  for (const character of value) {
    const size = textEncoder.encode(character).length;
    if (bytes + size > limit) break;
    text += character;
    bytes += size;
  }
  return { text, bytes, clipped: true };
}

function dropUTF8Prefix(value, minimumBytes) {
  if (!value || minimumBytes <= 0) return { text: value || "", dropped: 0 };
  let dropped = 0;
  let units = 0;
  for (const character of value) {
    if (dropped >= minimumBytes) break;
    dropped += textEncoder.encode(character).length;
    units += character.length;
  }
  return { text: value.slice(units), dropped };
}

function truncateUTF8Tail(value, limit) {
  if (!value || limit <= 0) return { text: "", bytes: 0, clipped: Boolean(value) };
  const bytes = textEncoder.encode(value).length;
  if (bytes <= limit) return { text: value, bytes, clipped: false };
  const result = dropUTF8Prefix(value, bytes - limit);
  return { text: result.text, bytes: bytes - result.dropped, clipped: true };
}

function makeRollingTextRoom(turn, type, byteField, incomingBytes, limit) {
  let needed = Math.max(0, turn[byteField] + incomingBytes - limit);
  while (needed > 0) {
    const index = turn.blocks.findIndex((block) => block.type === type && block.bytes > 0);
    if (index < 0) break;
    const block = turn.blocks[index];
    const trimmed = dropUTF8Prefix(block.text, Math.min(needed, block.bytes));
    block.text = trimmed.text;
    block.bytes = Math.max(0, block.bytes - trimmed.dropped);
    block.leadingClipped = true;
    turn[byteField] = Math.max(0, turn[byteField] - trimmed.dropped);
    needed = Math.max(0, turn[byteField] + incomingBytes - limit);
    turn.contentClipped = true;
    touchBlock(block);
    if (block.bytes === 0) turn.blocks.splice(index, 1);
  }
}

function beginFormSubmission(form) {
  if (form.dataset.submitting === "true") return false;
  form.dataset.submitting = "true";
  return true;
}

function endFormSubmission(form) {
  form.dataset.submitting = "false";
}

function applyOptimisticPromptState(turn, acceptedState) {
  if (turn.lifecycleSeen) return false;
  turn.state = acceptedState === "queued" ? "queued" : acceptedState === "ready" ? "ready" : "working";
  recordTurnLifecycle(turn, turn.state, {});
  return true;
}

function applyOptimisticPromptReceipt(turn, acceptedState) {
  const applied = applyOptimisticPromptState(turn, acceptedState);
  if (applied && turn.state === "working") setRuntimeStatus("working");
  else reconcileRuntimeStatusSoon();
  if (applied) updateActionVisibility(currentSessionRuntimeStatus);
  return applied;
}

function newTranscriptState() {
  return {
    turns: [], byRequest: new Map(), permissions: new Map(), diagnostics: [],
    renderTimer: 0, shouldStick: true, newUpdates: 0, lastPosition: 0, historyClipped: false,
    permissionOverflow: false, retainedBytes: 0, source: "gateway-log", checkpoint: "",
    sourceWarning: "", snapshotTruncated: false
  };
}

let transcriptState = newTranscriptState();

let streamAnnouncementsReady = false;
let conversationAnnouncementTimer = 0;
let conversationAnnouncementScheduled = false;
let pendingConversationAnnouncement = "";
let pendingConversationStatus = "";

function queueConversationAnnouncement(text, status = "") {
  if (text) {
    const used = textEncoder.encode(pendingConversationAnnouncement).length;
    const remaining = Math.max(0, LIMITS.announcement - used);
    if (remaining > 0) pendingConversationAnnouncement += truncateUTF8(text, remaining).text;
  }
  if (status) {
    const terminal = status.includes("complete") || status.includes("stopped");
    const pendingTerminal = pendingConversationStatus.includes("complete") || pendingConversationStatus.includes("stopped");
    if (terminal) {
      // Terminal state supersedes the earlier working state for this turn.
      pendingConversationStatus = status;
    } else if (pendingTerminal && !pendingConversationStatus.includes(status.trim())) {
      // A queued turn may start before this batch is announced. Preserve both
      // the completed boundary and the next turn's working state.
      pendingConversationStatus += status;
    } else if (!pendingTerminal) {
      pendingConversationStatus = status;
    }
  }
  if ((!pendingConversationAnnouncement && !pendingConversationStatus) || conversationAnnouncementScheduled) return;
  conversationAnnouncementScheduled = true;
  conversationAnnouncementTimer = window.setTimeout(() => {
    conversationAnnouncementScheduled = false;
    conversationAnnouncementTimer = 0;
    const announcer = document.querySelector("#conversation-announcer");
    const statusText = pendingConversationStatus;
    const statusBytes = textEncoder.encode(statusText).length;
    const body = truncateUTF8(pendingConversationAnnouncement, Math.max(0, LIMITS.announcement - statusBytes)).text;
    const announcement = `${body}${statusText}`;
    pendingConversationAnnouncement = "";
    pendingConversationStatus = "";
    if (!announcer || !announcement) return;
    announcer.textContent = "";
    window.setTimeout(() => { announcer.textContent = announcement; }, 0);
  }, 700);
}

function noteStreamActivity(activity) {
  if (!streamAnnouncementsReady) return;
  const display = activity.display;
  if (!display) return;
  if (display.type === "assistant_chunk") queueConversationAnnouncement(display.text || "");
  if (display.type === "turn_state" && display.state === "working") queueConversationAnnouncement("", " Agent is working.");
  if (display.type === "turn_state" && ["complete", "cancelled"].includes(display.state)) {
    queueConversationAnnouncement("", display.state === "cancelled" ? " Response stopped." : " Response complete.");
  }
}

function activityPosition(activity) {
  const position = Number(activity?.position);
  return Number.isFinite(position) && position > 0 ? position : Number.POSITIVE_INFINITY;
}

function orderedTurns() {
  return [...transcriptState.turns].sort((left, right) => left.firstPosition - right.firstPosition);
}

function activityTimestamp(activity) {
  const value = Date.parse(activity?.timestamp || "");
  return Number.isFinite(value) ? value : 0;
}

function recordTurnActivityTime(turn, activity) {
  const value = activityTimestamp(activity);
  if (!value) return;
  if (!turn.firstTimestamp || value < turn.firstTimestamp) turn.firstTimestamp = value;
  if (!turn.lastTimestamp || value > turn.lastTimestamp) turn.lastTimestamp = value;
}

function recordTurnLifecycle(turn, state, activity) {
  state = String(state || "").trim().toLowerCase();
  if (!state) return;
  const timestamp = activityTimestamp(activity);
  const previous = turn.lifecycle[turn.lifecycle.length - 1];
  if (previous?.state === state) {
    if (timestamp) previous.timestamp = timestamp;
    return;
  }
  if (turn.lifecycle.length >= 16) turn.lifecycle.shift();
  turn.lifecycle.push({ state, timestamp });
}

function turnRetainedBytes(turn) {
  let total = 0;
  for (const block of turn.blocks) {
    if (["user", "assistant", "thought"].includes(block.type)) total += block.bytes || 0;
    if (block.type === "error") total += textEncoder.encode(block.text || "").length;
    if (block.type === "plan") {
      for (const entry of block.entries || []) total += textEncoder.encode(`${entry.content || ""}${entry.status || ""}`).length;
    }
    if (block.type === "tool") {
      total += block.detailBytes || 0;
      total += textEncoder.encode(`${block.title || ""}${block.kind || ""}${block.status || ""}${block.metadata || ""}${block.preview || ""}${block.detailLabel || ""}`).length;
    }
  }
  return total;
}

function removeTurn(turn) {
  transcriptState.turns = transcriptState.turns.filter((item) => item !== turn);
  transcriptState.byRequest.delete(turn.id);
  transcriptState.retainedBytes = Math.max(0, transcriptState.retainedBytes - (turn.retainedBytes || 0));
  turn.element?.remove();
  for (const permissionID of turn.permissionIDs) {
    if (transcriptState.permissions.get(permissionID)?.state !== "waiting") transcriptState.permissions.delete(permissionID);
  }
  transcriptState.historyClipped = true;
}

function updateTurnRetention(turn) {
  const next = turnRetainedBytes(turn);
  transcriptState.retainedBytes += next - (turn.retainedBytes || 0);
  turn.retainedBytes = next;
  while (transcriptState.retainedBytes > LIMITS.retained) {
    const candidate = orderedTurns().find((item) => item !== turn);
    if (!candidate) break;
    removeTurn(candidate);
  }
}

function trimTurns() {
  while (transcriptState.turns.length > LIMITS.turns) {
    removeTurn(orderedTurns()[0]);
  }
}

function appendTurnBlock(turn, block) {
  if (turn.blocks.length >= LIMITS.blocksPerTurn) {
    turn.contentClipped = true;
    return false;
  }
  if (!block.revision) block.revision = 1;
  turn.blocks.push(block);
  turn.dirty = true;
  return true;
}

function makeTextBlockSlot(turn) {
  if (turn.blocks.length < LIMITS.blocksPerTurn) return true;
  const index = turn.blocks.findIndex((block) => block.type === "assistant" || block.type === "thought");
  if (index < 0) {
    turn.contentClipped = true;
    return false;
  }
  const [removed] = turn.blocks.splice(index, 1);
  if (removed.type === "assistant") turn.assistantBytes = Math.max(0, turn.assistantBytes - (removed.bytes || 0));
  if (removed.type === "thought") turn.thoughtBytes = Math.max(0, turn.thoughtBytes - (removed.bytes || 0));
  turn.contentClipped = true;
  turn.dirty = true;
  return true;
}

function touchBlock(block) {
  block.revision = (block.revision || 0) + 1;
}

function turnFor(activity) {
  const requestID = activity.request_id || `local:${activity.position}`;
  let turn = transcriptState.byRequest.get(requestID);
  if (turn) {
    turn.firstPosition = Math.min(turn.firstPosition, activityPosition(activity));
    recordTurnActivityTime(turn, activity);
    return turn;
  }
  turn = {
    id: requestID, firstPosition: activityPosition(activity), state: "", stopReason: "", blocks: [],
    tools: new Map(), plan: null, error: null, permissionIDs: new Set(),
    assistantBytes: 0, thoughtBytes: 0, toolBytes: 0, retainedBytes: 0, assistantSeen: false, reasoningSeen: false,
    lifecycle: [], firstTimestamp: 0, lastTimestamp: 0, surface: "acp",
    lifecycleSeen: false, committed: false, contentClipped: false, dirty: true, element: null, overviewElement: null
  };
  recordTurnActivityTime(turn, activity);
  transcriptState.byRequest.set(requestID, turn);
  transcriptState.turns.push(turn);
  return turn;
}

function applyDisplay(activity, turn) {
  const display = activity.display;
  if (!display) return;
  switch (display.type) {
  case "legacy_user_message": {
    if (turn.blocks.some((item) => item.type === "user")) break;
    const value = truncateUTF8(display.text || "", LIMITS.assistant);
    if (turn.blocks.length >= LIMITS.blocksPerTurn) {
      turn.contentClipped = true;
      break;
    }
    turn.blocks.unshift({ type: "user", text: value.text, bytes: value.bytes, clipped: value.clipped || Boolean(display.truncated), legacy: true, revision: 1 });
    turn.dirty = true;
    break;
  }
  case "user_message": {
    let block = turn.blocks.find((item) => item.type === "user");
    if (!block) {
      block = { type: "user", text: "", bytes: 0, clipped: false };
      if (turn.blocks.length >= LIMITS.blocksPerTurn) {
        turn.contentClipped = true;
        break;
      }
      turn.blocks.unshift(block);
    }
    const value = truncateUTF8(display.text || "", LIMITS.assistant);
    block.text = value.text;
    block.bytes = value.bytes;
    block.clipped = value.clipped || Boolean(display.truncated);
    block.legacy = false;
    touchBlock(block);
    turn.dirty = true;
    break;
  }
  case "assistant_chunk": {
    if (String(display.text || "").trim()) turn.assistantSeen = true;
    const incoming = truncateUTF8Tail(display.text || "", LIMITS.assistant);
    makeRollingTextRoom(turn, "assistant", "assistantBytes", incoming.bytes, LIMITS.assistant);
    const previous = turn.blocks[turn.blocks.length - 1];
    const source = activity.kind === "provider_transcript" ? "provider" : "acp";
    const compatible = previous?.type === "assistant" && previous.source === source && (previous.messageID === display.message_id || !previous.messageID || !display.message_id);
    const block = compatible ? previous : { type: "assistant", source, text: "", bytes: 0, clipped: false, leadingClipped: false, trailingClipped: false, messageID: display.message_id || "", key: activity.position };
    if (!compatible && (!makeTextBlockSlot(turn) || !appendTurnBlock(turn, block))) break;
    block.text += incoming.text;
    block.bytes += incoming.bytes;
    turn.assistantBytes += incoming.bytes;
    block.leadingClipped = block.leadingClipped || incoming.clipped;
    block.trailingClipped = block.trailingClipped || Boolean(display.truncated);
    turn.contentClipped = turn.contentClipped || incoming.clipped || Boolean(display.truncated);
    touchBlock(block);
    turn.dirty = true;
    break;
  }
  case "thought_chunk": {
    if (String(display.text || "").trim()) turn.reasoningSeen = true;
    const incoming = truncateUTF8Tail(display.text || "", LIMITS.thought);
    makeRollingTextRoom(turn, "thought", "thoughtBytes", incoming.bytes, LIMITS.thought);
    const source = activity.kind === "provider_transcript" ? "provider" : "acp";
    const previous = turn.blocks[turn.blocks.length - 1];
    const block = previous?.type === "thought" && previous.source === source ? previous : { type: "thought", source, text: "", bytes: 0, clipped: false, leadingClipped: false, trailingClipped: false, key: activity.position };
    if (block !== previous && (!makeTextBlockSlot(turn) || !appendTurnBlock(turn, block))) break;
    block.text += incoming.text;
    block.bytes += incoming.bytes;
    turn.thoughtBytes += incoming.bytes;
    block.leadingClipped = block.leadingClipped || incoming.clipped;
    block.trailingClipped = block.trailingClipped || Boolean(display.truncated);
    turn.contentClipped = turn.contentClipped || incoming.clipped || Boolean(display.truncated);
    touchBlock(block);
    turn.dirty = true;
    break;
  }
  case "tool_upsert": {
    const patch = display.tool;
    if (!patch?.id) break;
    let block = turn.tools.get(patch.id);
    if (!block) {
      if (turn.tools.size >= LIMITS.toolsPerTurn) {
        turn.contentClipped = true;
        turn.dirty = true;
        break;
      }
      block = { type: "tool", id: patch.id, title: "", kind: "", status: "", metadata: "", preview: "", detail: "", detailBytes: 0, clipped: false, sensitive: false };
      if (!appendTurnBlock(turn, block)) break;
      turn.tools.set(patch.id, block);
    }
    for (const field of ["title", "kind", "status", "metadata", "detail_label"]) {
      if (patch[field]) block[field === "detail_label" ? "detailLabel" : field] = patch[field];
    }
    if (patch.detail) {
      const available = Math.min(LIMITS.tool, Math.max(0, LIMITS.toolTotal - turn.toolBytes + block.detailBytes));
      const detail = truncateUTF8(patch.detail, available);
      turn.toolBytes = turn.toolBytes - block.detailBytes + detail.bytes;
      block.detail = detail.text;
      block.detailBytes = detail.bytes;
      block.clipped = detail.clipped || Boolean(patch.detail_clipped);
    }
    block.sensitive = block.sensitive || Boolean(patch.sensitive);
    if (block.sensitive) {
      block.preview = "";
    } else if (patch.preview) {
      block.preview = truncateUTF8(patch.preview, 2 << 10).text.split("\n").slice(0, 3).join("\n");
    }
    touchBlock(block);
    turn.dirty = true;
    break;
  }
  case "plan_replace": {
    if (!turn.plan) {
      turn.plan = { type: "plan", entries: [] };
      if (!appendTurnBlock(turn, turn.plan)) {
        turn.plan = null;
        break;
      }
    }
    let remaining = LIMITS.plan;
    turn.plan.entries = [];
    for (const entry of (display.plan || []).slice(0, 128)) {
      const content = truncateUTF8(entry.content || "", remaining);
      if (!content.text) continue;
      turn.plan.entries.push({ content: content.text, status: truncateUTF8(entry.status || "", 64).text });
      remaining -= content.bytes;
      if (remaining <= 0) break;
    }
    touchBlock(turn.plan);
    turn.dirty = true;
    break;
  }
  case "error_upsert": {
    if (!turn.error) {
	  turn.error = { type: "error", text: "", errorSource: "", errorCode: "", retryable: false };
      if (!appendTurnBlock(turn, turn.error)) {
        turn.error = null;
        break;
      }
    }
    turn.error.text = truncateUTF8(display.text || "The provider turn failed.", LIMITS.assistant).text;
	turn.error.errorSource = display.error_source || "";
	turn.error.errorCode = display.error_code || "";
	turn.error.retryable = Boolean(display.retryable);
    touchBlock(turn.error);
    turn.state = "error";
    recordTurnLifecycle(turn, "error", activity);
    turn.lifecycleSeen = true;
    turn.dirty = true;
    updateRuntimeStatusFromDisplay(display);
    break;
  }
  case "turn_state":
    turn.state = display.state || turn.state;
    turn.stopReason = display.stop_reason || turn.stopReason;
    recordTurnLifecycle(turn, turn.state, activity);
    turn.lifecycleSeen = true;
    turn.dirty = true;
    updateRuntimeStatusFromDisplay(display);
    break;
  }
}

function rememberPermission(activity, turn) {
  const permission = activity.permission;
  if (!permission?.id) return;
  const existing = transcriptState.permissions.get(permission.id) || {};
  const merged = { ...existing, ...permission };
  if ((!permission.options || permission.options.length === 0) && existing.options) merged.options = existing.options;
  if (!ensurePermissionCapacity(permission.id)) return;
  transcriptState.permissions.set(permission.id, merged);
  turn.permissionIDs.add(permission.id);
  turn.dirty = true;
  enforcePermissionBound();
}

function flagPermissionOverflow() {
  transcriptState.permissionOverflow = true;
  activeEventSource?.close();
  activeEventSource = null;
  const state = document.querySelector("#stream-state");
  state?.classList.remove("connected");
  const label = state?.querySelector("span");
  if (label) label.textContent = "Approval limit";
}

function removePermissionRecord(id) {
  transcriptState.permissions.delete(id);
  for (const item of transcriptState.turns) item.permissionIDs.delete(id);
  transcriptState.historyClipped = true;
}

function ensurePermissionCapacity(permissionID) {
  if (transcriptState.permissions.has(permissionID)) return true;
  while (transcriptState.permissions.size >= LIMITS.permissions) {
    const removable = Array.from(transcriptState.permissions).find(([, item]) => item.state !== "waiting");
    if (!removable) {
      flagPermissionOverflow();
      return false;
    }
    removePermissionRecord(removable[0]);
  }
  transcriptState.permissionOverflow = false;
  return true;
}

function enforcePermissionBound() {
  while (transcriptState.permissions.size > LIMITS.permissions || (transcriptState.permissionOverflow && transcriptState.permissions.size >= LIMITS.permissions)) {
    const removable = Array.from(transcriptState.permissions).find(([, item]) => item.state !== "waiting");
    if (!removable) {
      flagPermissionOverflow();
      break;
    }
    removePermissionRecord(removable[0]);
  }
  if (transcriptState.permissions.size < LIMITS.permissions) transcriptState.permissionOverflow = false;
}

function applyActivity(activity) {
  const position = Number(activity.position) || 0;
  if (position > 0 && position <= transcriptState.lastPosition) return false;
  transcriptState.lastPosition = Math.max(transcriptState.lastPosition, position);
  transcriptState.diagnostics.push({
    position: activity.position, request_id: activity.request_id, kind: activity.kind,
    timestamp: activity.timestamp, activity: activity.activity
  });
  if (transcriptState.diagnostics.length > LIMITS.diagnostics) transcriptState.diagnostics.shift();
  if (!activity.display && !activity.permission) return true;
  const turn = turnFor(activity);
  rememberPermission(activity, turn);
  applyDisplay(activity, turn);
  updateActionVisibility(currentSessionRuntimeStatus);
  updateTurnRetention(turn);
  trimTurns();
  return true;
}

function applySnapshotActivity(activity) {
  const turn = turnFor(activity);
  rememberPermission(activity, turn);
  applyDisplay(activity, turn);
  updateTurnRetention(turn);
  trimTurns();
  return turn;
}

function buildConversationSnapshot(snapshot, buffered = []) {
  const candidate = newTranscriptState();
  candidate.source = snapshot.source || "gateway-log";
  candidate.checkpoint = snapshot.checkpoint || "";
  candidate.sourceWarning = snapshot.warning || "";
  candidate.snapshotTruncated = Boolean(snapshot.truncated);
  candidate.historyClipped = candidate.snapshotTruncated;
  const previous = transcriptState;
  transcriptState = candidate;
  try {
    let snapshotPosition = 0;
    for (const [turnIndex, snapshotTurn] of (snapshot.turns || []).entries()) {
      const requestID = snapshotTurn.key || `snapshot:${turnIndex + 1}`;
      for (const activity of (snapshotTurn.activities || [])) {
        snapshotPosition += 1;
        applySnapshotActivity({ ...activity, position: snapshotPosition, request_id: requestID });
      }
      const turn = candidate.byRequest.get(requestID);
      if (turn) {
        turn.committed = Boolean(snapshotTurn.committed);
        turn.surface = ["acp", "native", "unknown"].includes(snapshotTurn.surface) ? snapshotTurn.surface : "unknown";
      }
      if (turn && snapshotTurn.committed && !turn.lifecycleSeen) {
        turn.state = "complete";
        recordTurnLifecycle(turn, "complete", {});
        turn.lifecycleSeen = true;
        turn.dirty = true;
      }
    }
    candidate.diagnostics = Array.isArray(snapshot.diagnostics) ? snapshot.diagnostics.slice(-LIMITS.diagnostics) : candidate.diagnostics;
    candidate.lastPosition = Math.max(candidate.lastPosition, Number(snapshot.gateway_cursor) || 0);
    for (const activity of buffered) {
      if ((Number(activity.position) || 0) > candidate.lastPosition) applyActivity(activity);
    }
    return candidate;
  } finally {
    transcriptState = previous;
  }
}

function turnUserText(turn) {
  return turn.blocks.find((block) => block.type === "user")?.text?.trim() || "";
}

function turnAssistantText(turn) {
  return turn.blocks.filter((block) => block.type === "assistant").map((block) => block.text || "").join("\n").trim();
}

function normalizedPresentationText(value) {
  return String(value || "").replace(/\s+/g, " ").trim();
}

function matchingOldTurn(turn, oldTurns, used) {
  const exact = oldTurns.find((item) => !used.has(item) && item.id === turn.id);
  if (exact) return exact;
  const user = normalizedPresentationText(turnUserText(turn));
  const assistant = normalizedPresentationText(turnAssistantText(turn));
  const matches = oldTurns.filter((item) => {
    if (used.has(item)) return false;
    const oldUser = normalizedPresentationText(turnUserText(item));
    const oldAssistant = normalizedPresentationText(turnAssistantText(item));
    if (user && oldUser && user !== oldUser) return false;
    if (!assistant || !oldAssistant) return Boolean(user && oldUser && user === oldUser);
    return assistant === oldAssistant || assistant.endsWith(oldAssistant) || oldAssistant.endsWith(assistant);
  });
  // Fuzzy ACP-to-provider matching only preserves presentation state. If
  // repeated turns make it ambiguous, replace the nodes atomically instead
  // of attaching focus or selection to the wrong turn.
  return matches.length === 1 ? matches[0] : null;
}

function matchingOldBlock(block, oldBlocks, used) {
  const matches = oldBlocks.filter((item) => {
    if (used.has(item) || item.type !== block.type) return false;
    if (block.type === "tool") return item.id === block.id;
    if (["assistant", "thought", "user"].includes(block.type)) {
      const nextText = normalizedPresentationText(block.text);
      const oldText = normalizedPresentationText(item.text);
      return Boolean(nextText && oldText) && (nextText === oldText || nextText.endsWith(oldText) || oldText.endsWith(nextText));
    }
    return true;
  });
  return matches.length === 1 ? matches[0] : null;
}

function blockPresentationMatches(left, right) {
  if (!left || !right || left.type !== right.type) return false;
  if (["assistant", "thought", "user", "error"].includes(left.type)) {
    return left.text === right.text && blockWasClipped(left) === blockWasClipped(right);
  }
  if (left.type === "tool") {
    return ["id", "title", "kind", "status", "metadata", "preview", "detail", "detailLabel", "clipped", "sensitive"]
      .every((field) => left[field] === right[field]);
  }
  if (left.type === "plan") return JSON.stringify(left.entries || []) === JSON.stringify(right.entries || []);
  return false;
}

function adoptConversationPresentation(previous, candidate) {
  candidate.shouldStick = previous.shouldStick;
  candidate.newUpdates = previous.newUpdates;
  const usedTurns = new Set();
  for (const turn of candidate.turns) {
    const oldTurn = matchingOldTurn(turn, previous.turns, usedTurns);
    if (!oldTurn) continue;
    usedTurns.add(oldTurn);
    turn.element = oldTurn.element;
    turn.overviewElement = oldTurn.overviewElement;
    const usedBlocks = new Set();
    for (const block of turn.blocks) {
      const oldBlock = matchingOldBlock(block, oldTurn.blocks, usedBlocks);
      if (!oldBlock) continue;
      usedBlocks.add(oldBlock);
      block.element = oldBlock.element;
      // Reuse the stable DOM node so open/focus/selection state survives, but
      // force a patch when the provider-committed text differs from the
      // provisional ACP block. Equal revision counters are not content IDs.
      block.renderedRevision = blockPresentationMatches(block, oldBlock) ? oldBlock.renderedRevision : -1;
      block.renderedContext = oldBlock.renderedContext;
    }
  }
  for (const [id, permission] of previous.permissions) {
    if (permission.state !== "waiting" || candidate.permissions.has(id)) continue;
    if (candidate.permissions.size >= LIMITS.permissions) {
      candidate.permissionOverflow = true;
      candidate.historyClipped = true;
      continue;
    }
    candidate.permissions.set(id, permission);
  }
}

let conversationSnapshotSignature = "";
let conversationReconcileInFlight = false;
let conversationReconcilePending = false;
let conversationReconcileBuffer = [];
let conversationReconcileBufferBytes = 0;
let conversationReconcileOverflow = false;
let conversationReconcileTimer = 0;
let conversationReconcileAttempts = 0;
let providerAuthorityRetryTimer = 0;

function hasUncommittedTerminalTurns(state = transcriptState) {
  // Cancelled and failed turns are intentionally never committed as provider
  // conversation history, so only successful completed turns need polling.
  return state.turns.some((turn) => !turn.committed && turn.state === "complete");
}

function scheduleProviderCommitRetry(sessionID) {
  if (!hasUncommittedTerminalTurns()) return;
  // Provider stores normally commit within the first few attempts. Keep a
  // low-frequency tail retry while the viewer remains open so a slow fsync,
  // WAL checkpoint, or provider-side compaction cannot leave a completed ACP
  // turn permanently provisional.
  const delay = conversationReconcileAttempts < 6
    ? Math.min(3000, 220 * (2 ** conversationReconcileAttempts))
    : 10000;
  conversationReconcileAttempts = Math.min(conversationReconcileAttempts + 1, 6);
  scheduleConversationReconcile(sessionID, delay);
}

function clearProviderAuthorityRetry() {
  if (providerAuthorityRetryTimer) window.clearTimeout?.(providerAuthorityRetryTimer);
  providerAuthorityRetryTimer = 0;
}

function scheduleProviderAuthorityRetry(sessionID) {
  if (providerAuthorityRetryTimer) return;
  // This retry is intentionally independent from completed-turn commit
  // polling. An idle or empty gateway fallback still needs to discover later
  // native/provider history without requiring another ACP event.
  providerAuthorityRetryTimer = window.setTimeout(() => {
    providerAuthorityRetryTimer = 0;
    reconcileConversation(sessionID).catch(() => {});
  }, 10000);
}

function conversationHistorySummary(state = transcriptState) {
  const turns = state?.turns || [];
  const saved = turns.filter((turn) => turn.committed).length;
  const unsaved = turns.filter((turn) => !turn.committed);
  const active = unsaved.filter((turn) => !["complete", "cancelled", "incomplete", "interrupted", "error", "failed"].includes(turn.state)).length;
  const saving = unsaved.filter((turn) => turn.state === "complete").length;
  const stopped = unsaved.filter((turn) => ["cancelled", "incomplete", "interrupted", "error", "failed"].includes(turn.state)).length;
  const tools = turns.reduce((count, turn) => count + (turn.blocks || []).filter((block) => block.type === "tool").length, 0);
  const bounded = Boolean(state?.snapshotTruncated || state?.historyClipped);
  const shortParts = [];
  if (saved > 0) shortParts.push(`${saved} saved${saved === 1 ? " turn" : " turns"}`);
  if (active > 0) shortParts.push(`${active} live${active === 1 ? " turn" : " turns"}`);
  if (saving > 0) shortParts.push(`${saving} saving`);
  if (stopped > 0) shortParts.push(`${stopped} unsaved`);
  let short = shortParts.join(" · ") || "No history yet";
  if (saved === 0 && active > 0 && tools > 0) short += ` · ${tools} steps`;
  if (bounded) short += " · partial";
  const savedCopy = `${saved} saved provider ${saved === 1 ? "turn" : "turns"}`;
  let detail = saved > 0 ? savedCopy : "No saved provider turns yet";
  if (active > 0) detail += ` · ${active} live ${active === 1 ? "turn" : "turns"} can be saved after successful completion`;
  if (saving > 0) detail += ` · ${saving} completed ${saving === 1 ? "turn is" : "turns are"} waiting for provider commit`;
  if (stopped > 0) detail += ` · ${stopped} failed, interrupted, or cancelled ${stopped === 1 ? "turn remains" : "turns remain"} unsaved history`;
  if (tools > 0) detail += ` · ${tools} tool ${tools === 1 ? "step" : "steps"} loaded`;
  detail += bounded ? " · older content is outside this bounded view" : " · all available history is loaded";
  return { short, detail, saved, active, saving, stopped, tools, bounded };
}

function updateHistorySummary(state = transcriptState) {
  const summary = conversationHistorySummary(state);
  const label = document.querySelector("#history-summary");
  if (label) {
    label.textContent = summary.short;
    label.className = `history-summary${summary.active > 0 || summary.saving > 0 ? " live" : ""}${summary.bounded ? " bounded" : ""}`;
    label.setAttribute("title", summary.detail);
    label.setAttribute("aria-label", summary.detail);
  }
  return summary;
}

function updateHistorySource(source, warning = "") {
  const label = document.querySelector("#history-source");
  if (!label) return;
  const summary = conversationHistorySummary();
  label.textContent = source === "provider-store"
    ? summary.detail
    : warning || "Gateway JSONL fallback + live ACP stream";
}

function snapshotSignature(snapshot) {
  return JSON.stringify([
    snapshot.source, snapshot.checkpoint, snapshot.gateway_cursor, snapshot.warning, snapshot.truncated,
    (snapshot.turns || []).map((turn) => [turn.key, turn.committed, turn.surface, (turn.activities || []).map((item) => [item.position, item.kind, item.request_id])]),
  ]);
}

function applySuccessfulConversationMetadata(snapshot) {
  const nextWarning = snapshot.warning || "";
  const changed = transcriptState.source !== snapshot.source || transcriptState.sourceWarning !== nextWarning;
  transcriptState.source = snapshot.source;
  transcriptState.sourceWarning = nextWarning;
  updateHistorySource(snapshot.source, nextWarning);
  return changed;
}

async function reconcileConversation(sessionID, { initial = false } = {}) {
  if (conversationReconcileInFlight) {
    conversationReconcilePending = true;
    return false;
  }
  conversationReconcileInFlight = true;
  conversationReconcileBuffer = [];
  conversationReconcileBufferBytes = 0;
  conversationReconcileOverflow = false;
  try {
    const snapshot = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/conversation`);
    if (conversationReconcileOverflow) {
      transcriptState.sourceWarning = "Provider history refresh was deferred while live activity was busy; live output remains current.";
      updateHistorySource(transcriptState.source, transcriptState.sourceWarning);
      scheduleTranscriptRender();
      conversationReconcilePending = true;
      return false;
    }
    const signature = snapshotSignature(snapshot);
    if (snapshot.source === "provider-store") clearProviderAuthorityRetry();
    else scheduleProviderAuthorityRetry(sessionID);
    // A gateway snapshot is authoritative enough to bootstrap one atomic,
    // bounded view and seed SSE from its captured cursor. On later refreshes,
    // however, it represents a transient loss of provider-store authority;
    // keep the already-rendered provider/live state instead of replacing it
    // with an older or differently-clipped fallback projection.
    if (snapshot.source !== "provider-store" && !initial) {
      transcriptState.sourceWarning = snapshot.warning || "Provider history is unavailable; showing gateway history.";
      updateHistorySource(snapshot.source, transcriptState.sourceWarning);
      scheduleTranscriptRender();
      scheduleProviderCommitRetry(sessionID);
      return false;
    }
    if (!initial && signature === conversationSnapshotSignature && conversationReconcileBuffer.length === 0) {
      if (applySuccessfulConversationMetadata(snapshot)) scheduleTranscriptRender();
      scheduleProviderCommitRetry(sessionID);
      return false;
    }
    const previous = transcriptState;
    const candidate = buildConversationSnapshot(snapshot, conversationReconcileBuffer);
    adoptConversationPresentation(previous, candidate);
    if (previous.renderTimer) window.clearTimeout?.(previous.renderTimer);
    transcriptState = candidate;
    conversationSnapshotSignature = signature;
    updateHistorySource(candidate.source, candidate.sourceWarning);
    renderConversation();
    if (hasUncommittedTerminalTurns(candidate)) scheduleProviderCommitRetry(sessionID);
    else conversationReconcileAttempts = 0;
    return true;
  } catch (_) {
    transcriptState.sourceWarning = "Provider history refresh is delayed; live ACP output remains current.";
    updateHistorySource(transcriptState.source, transcriptState.sourceWarning);
    if (!initial) {
      scheduleTranscriptRender();
    }
    scheduleProviderCommitRetry(sessionID);
    scheduleProviderAuthorityRetry(sessionID);
    return false;
  } finally {
    conversationReconcileBuffer = [];
    conversationReconcileBufferBytes = 0;
    conversationReconcileOverflow = false;
    conversationReconcileInFlight = false;
    if (conversationReconcilePending) {
      conversationReconcilePending = false;
      scheduleConversationReconcile(sessionID, 120);
    }
  }
}

function bufferConversationActivity(activity) {
  if (!conversationReconcileInFlight || conversationReconcileOverflow) return;
  let bytes = 0;
  try {
    bytes = textEncoder.encode(JSON.stringify(activity)).length;
  } catch (_) {
    conversationReconcileOverflow = true;
  }
  if (conversationReconcileOverflow || conversationReconcileBuffer.length >= LIMITS.reconcileEvents || conversationReconcileBufferBytes + bytes > LIMITS.reconcileBytes) {
    conversationReconcileBuffer = [];
    conversationReconcileBufferBytes = 0;
    conversationReconcileOverflow = true;
    return;
  }
  conversationReconcileBuffer.push(activity);
  conversationReconcileBufferBytes += bytes;
}

function scheduleConversationReconcile(sessionID, delay = 140) {
  window.clearTimeout?.(conversationReconcileTimer);
  conversationReconcileTimer = window.setTimeout(() => {
    conversationReconcileTimer = 0;
    reconcileConversation(sessionID).catch(() => {});
  }, delay);
}

function nearBottom(element) {
  return !element || element.scrollHeight - element.scrollTop - element.clientHeight < 90;
}

function scheduleTranscriptRender() {
  const scroller = document.querySelector("#conversation-scroll");
  const atBottom = nearBottom(scroller);
  if (!atBottom) transcriptState.newUpdates += 1;
  transcriptState.shouldStick = transcriptState.shouldStick && atBottom;
  if (transcriptState.renderTimer) return;
  transcriptState.renderTimer = window.setTimeout(() => {
    transcriptState.renderTimer = 0;
    renderConversation();
  }, 55);
}

function appendInline(parent, text) {
  const pattern = /(\[[^\]]+\]\(https?:\/\/[^\s)]+\)|`[^`]+`|\*\*[^*]+\*\*)/g;
  let offset = 0;
  for (const match of text.matchAll(pattern)) {
    if (match.index > offset) parent.append(document.createTextNode(text.slice(offset, match.index)));
    const token = match[0];
    let node;
    const link = token.match(/^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/);
    if (link) {
      node = document.createElement("a");
      node.textContent = link[1];
      node.href = link[2];
      node.target = "_blank";
      node.rel = "noreferrer noopener";
    } else {
      node = token.startsWith("`") ? document.createElement("code") : document.createElement("strong");
      node.textContent = token.startsWith("`") ? token.slice(1, -1) : token.slice(2, -2);
    }
    parent.append(node);
    offset = match.index + token.length;
  }
  if (offset < text.length) parent.append(document.createTextNode(text.slice(offset)));
}

function appendRichText(container, text) {
  const lines = String(text || "").split("\n");
  let index = 0;
  while (index < lines.length) {
    const line = lines[index];
    if (line.startsWith("```")) {
      const language = line.slice(3).trim();
      const codeLines = [];
      index += 1;
      while (index < lines.length && !lines[index].startsWith("```")) codeLines.push(lines[index++]);
      if (index < lines.length) index += 1;
      const pre = document.createElement("pre");
      const code = document.createElement("code");
      if (language) code.dataset.language = language;
      code.textContent = codeLines.join("\n");
      pre.append(code);
      container.append(pre);
      continue;
    }
    if (!line.trim()) { index += 1; continue; }
    const heading = line.match(/^(#{1,3})\s+(.+)$/);
    if (heading) {
      const node = document.createElement(`h${heading[1].length + 2}`);
      appendInline(node, heading[2]);
      container.append(node);
      index += 1;
      continue;
    }
    if (/^[-*]\s+/.test(line)) {
      const list = document.createElement("ul");
      while (index < lines.length && /^[-*]\s+/.test(lines[index])) {
        const item = document.createElement("li");
        appendInline(item, lines[index].replace(/^[-*]\s+/, ""));
        list.append(item);
        index += 1;
      }
      container.append(list);
      continue;
    }
    if (/^\d+\.\s+/.test(line)) {
      const list = document.createElement("ol");
      while (index < lines.length && /^\d+\.\s+/.test(lines[index])) {
        const item = document.createElement("li");
        appendInline(item, lines[index].replace(/^\d+\.\s+/, ""));
        list.append(item);
        index += 1;
      }
      container.append(list);
      continue;
    }
    const paragraph = [];
    while (index < lines.length && lines[index].trim() && !lines[index].startsWith("```") && !/^(#{1,3})\s+/.test(lines[index]) && !/^[-*]\s+/.test(lines[index]) && !/^\d+\.\s+/.test(lines[index])) {
      paragraph.push(lines[index++]);
    }
    const node = document.createElement("p");
    appendInline(node, paragraph.join("\n"));
    container.append(node);
  }
}

function messageElement(block, assistantName, context = {}) {
  if (block.type === "user") {
    const wrapper = document.createElement("div");
    wrapper.className = "user-message";
    wrapper.setAttribute("role", "group");
    wrapper.setAttribute("aria-label", "You");
    const body = document.createElement("div");
    body.className = "message-body";
    body.textContent = block.text;
    if (blockWasClipped(block)) body.append(clippedNote(block));
    wrapper.append(body);
    return wrapper;
  }
  const wrapper = document.createElement("div");
  wrapper.className = "message assistant-message";
  wrapper.setAttribute("role", "group");
  wrapper.setAttribute("aria-label", assistantName);
  const avatar = document.createElement("span");
  avatar.className = "message-avatar";
  avatar.textContent = agentInitial(assistantName);
  const content = document.createElement("div");
  content.className = "assistant-content";
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const author = document.createElement("strong");
  author.textContent = assistantName;
  const phase = document.createElement("span");
  phase.textContent = context.final
    ? (block.source === "provider" ? "Final response · saved" : "Final response")
    : block.source === "provider" ? "Earlier response" : context.streaming ? "Working" : "Earlier update";
  meta.append(author, phase);
  const body = document.createElement("div");
  body.className = "message-body";
  appendRichText(body, block.text);
  if (blockWasClipped(block)) body.append(clippedNote(block));
  content.append(meta, body);
  wrapper.classList.toggle("final-response", Boolean(context.final));
  wrapper.append(avatar, content);
  return wrapper;
}

function blockWasClipped(block) {
  return Boolean(block?.clipped || block?.leadingClipped || block?.trailingClipped);
}

function clippedNote(block = {}) {
  const note = document.createElement("div");
  note.className = "message-clipped";
  if (block.leadingClipped) {
    note.textContent = "Earlier output was omitted to keep this view responsive; the newest ACP output is preserved.";
  } else if (block.trailingClipped) {
    note.textContent = "The provider marked this ACP output as truncated.";
  } else {
    note.textContent = "Display clipped for safety; the provider transcript remains authoritative.";
  }
  return note;
}

function thoughtElement(block, context = {}) {
  const details = document.createElement("details");
  const source = block.source === "provider" ? "provider-history" : context.streaming ? "live-acp" : "captured-acp";
  details.className = `thought-card ${source}`;
  details.dataset.detailKey = "reasoning";
  details.dataset.source = source;
  details.open = true;
  const summary = document.createElement("summary");
  summary.className = "thought-summary";
  const heading = document.createElement("span");
  heading.className = "thought-heading";
  const title = document.createElement("strong");
  title.textContent = "Reasoning";
  const readable = String(block.text || "").trim();
  const lineCount = readable ? readable.split(/\r?\n/).filter((line) => line.trim()).length : 0;
  const wordCount = readable ? readable.split(/\s+/u).filter(Boolean).length : 0;
  const metrics = document.createElement("small");
  metrics.textContent = `${lineCount} ${lineCount === 1 ? "line" : "lines"} · ${wordCount} ${wordCount === 1 ? "word" : "words"}`;
  heading.append(title, metrics);
  const state = document.createElement("span");
  state.className = "thought-source";
  state.textContent = block.source === "provider" ? "Saved" : context.streaming ? "Live" : "Captured";
  summary.append(heading, state);
  const content = document.createElement("div");
  content.className = "thought-content";
  appendRichText(content, block.text);
  if (blockWasClipped(block)) content.append(clippedNote(block));
  details.append(summary, content);
  summary.addEventListener("click", () => { details.dataset.userToggled = "true"; });
  summary.addEventListener("keydown", (event) => { if (["Enter", " "].includes(event.key)) details.dataset.userToggled = "true"; });
  return details;
}

function toolElement(tool) {
  const details = document.createElement("details");
  details.dataset.detailKey = `tool:${tool.id}`;
  const normalizedStatus = String(tool.status || "").toLowerCase();
  const running = ["in_progress", "running", "pending"].includes(normalizedStatus);
  const failed = ["failed", "error", "cancelled"].includes(normalizedStatus);
  details.className = `tool-card${running ? " running" : ""}${failed ? " failed" : ""}`;
  details.open = running || failed;
  const summary = document.createElement("summary");
  summary.className = "tool-summary";
  const icon = document.createElement("span");
  icon.className = "tool-icon";
  icon.textContent = failed ? "!" : running ? "•" : "✓";
  const copy = document.createElement("span");
  copy.className = "tool-copy";
  const title = document.createElement("strong");
  title.textContent = tool.title || tool.metadata || (tool.kind ? tool.kind.replaceAll("_", " ") : "Tool");
  const metadata = document.createElement("small");
  metadata.textContent = tool.metadata || tool.kind?.replaceAll("_", " ") || "";
  copy.append(title, metadata);
  if (tool.preview) {
    const preview = document.createElement("span");
    preview.className = "tool-inline-preview";
    preview.textContent = tool.preview;
    copy.append(preview);
  }
  const state = document.createElement("span");
  state.className = "tool-status";
  state.textContent = normalizedStatus.replaceAll("_", " ") || "updated";
  summary.append(icon, copy, state);
  details.append(summary);
  if (tool.detail) {
    const detail = document.createElement("pre");
    detail.className = "tool-detail";
    detail.textContent = tool.detail;
    if (tool.clipped) detail.textContent += "\n\n… output clipped";
    details.append(detail);
  }
  summary.addEventListener("click", () => { details.dataset.userToggled = "true"; });
  summary.addEventListener("keydown", (event) => { if (["Enter", " "].includes(event.key)) details.dataset.userToggled = "true"; });
  return details;
}

function planElement(block) {
  const card = document.createElement("section");
  card.className = "plan-card";
  const heading = document.createElement("h3");
  heading.textContent = "Current plan";
  const list = document.createElement("ul");
  list.className = "plan-list";
  for (const entry of block.entries) {
    const item = document.createElement("li");
    const status = String(entry.status || "").replaceAll("_", "-");
    if (["completed", "done"].includes(status)) item.className = "completed";
    if (["in-progress", "in_progress", "working"].includes(status)) item.className = "in-progress";
    const mark = document.createElement("span");
    mark.className = "plan-mark";
    mark.textContent = item.className === "completed" ? "✓" : item.className === "in-progress" ? "●" : "○";
    const text = document.createElement("span");
    text.textContent = entry.content;
    item.append(mark, text);
    list.append(item);
  }
  card.append(heading, list);
  return card;
}

function renderBlockElement(block, assistantName, context = {}) {
  if (block.type === "user" || block.type === "assistant") return messageElement(block, assistantName, context);
  if (block.type === "thought") return thoughtElement(block, context);
  if (block.type === "tool") return toolElement(block);
  if (block.type === "plan") return block.entries.length > 0 ? planElement(block) : null;
  if (block.type === "error") {
    const error = document.createElement("div");
    error.className = "error-card";
    error.textContent = block.text;
	if (block.errorSource === "provider" && block.errorCode) {
	  const detail = document.createElement("small");
	  const label = ({busy:"Provider busy", conflict:"Provider conflict", rate_limited:"Rate limited", rejected:"Provider rejected", provider_error:"Provider error"})[block.errorCode] || "Provider error";
	  detail.textContent = `${label} · ${block.retryable ? "Retryable" : "Not marked retryable"}`;
	  error.append(detail);
	}
    return error;
  }
  return null;
}

function completedResponseBlock(turn) {
  if (turn.state !== "complete") return null;
  const assistant = turn.blocks.filter((block) => block.type === "assistant" && block.text.trim());
  return assistant[assistant.length - 1] || null;
}

function turnHasReasoning(turn) {
  return turn.reasoningSeen;
}

function turnHasWaitingPermission(turn) {
  for (const permissionID of turn.permissionIDs || []) {
    if (transcriptState.permissions.get(permissionID)?.state === "waiting") return true;
  }
  return false;
}

function turnSurfaceLabel(turn) {
  if (turn.surface === "native") return "Native CLI turn";
  if (turn.surface === "unknown") return "Provider turn";
  return "ACP turn";
}

function turnStatusLabel(turn) {
  if (turnHasWaitingPermission(turn)) return "Approval required";
  return ({
    ready: "Accepted", queued: "Queued", working: "Working", cancelling: "Stopping", cancelled: "Stopped",
    incomplete: "Incomplete", interrupted: "Interrupted", error: "Failed", failed: "Failed", complete: "Complete"
  })[turn.state] || (turn.committed ? "Complete" : "Pending");
}

function turnStatusTone(turn) {
  if (turnHasWaitingPermission(turn)) return "approval";
  return turn.state || "pending";
}

function turnDurationLabel(turn) {
  if (!["complete", "cancelled", "error", "failed"].includes(turn.state)) return "";
  const elapsed = Math.max(0, (turn.lastTimestamp || 0) - (turn.firstTimestamp || 0));
  if (!elapsed) return "";
  if (elapsed < 1000) return `${elapsed}ms`;
  if (elapsed < 10000) return `${(elapsed / 1000).toFixed(1)}s`;
  if (elapsed < 60000) return `${Math.round(elapsed / 1000)}s`;
  const totalSeconds = Math.round(elapsed / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}m ${seconds}s`;
}

function turnLifecycleLabel(turn) {
  const labels = ({
    ready: "Accepted", queued: "Queued", working: "Working", cancelling: "Stopping", cancelled: "Stopped",
    incomplete: "Incomplete", interrupted: "Interrupted", error: "Failed", failed: "Failed", complete: "Complete"
  });
  const states = (turn.lifecycle || []).map((item) => labels[item.state] || "").filter(Boolean);
  if (turn.surface !== "acp" && states.length === 1 && states[0] === "Complete") {
    return "Provider history → Complete";
  }
  return states.join(" → ") || turnStatusLabel(turn);
}

function setTextIfChanged(element, value) {
  if (element.textContent !== value) element.textContent = value;
}

function accessibleDurationLabel(duration) {
  if (!duration) return "";
  const milliseconds = duration.match(/^(\d+)ms$/);
  if (milliseconds) return `Duration ${milliseconds[1]} milliseconds. `;
  const seconds = duration.match(/^(\d+(?:\.\d+)?)s$/);
  if (seconds) return `Duration ${seconds[1]} seconds. `;
  const minutes = duration.match(/^(\d+)m (\d+)s$/);
  if (minutes) return `Duration ${minutes[1]} minutes ${minutes[2]} seconds. `;
  return "";
}

function turnOverviewElement(turn) {
  const overview = turn.overviewElement || document.createElement("div");
  if (!turn.overviewElement) {
    overview.setAttribute("role", "group");
    const main = document.createElement("div");
    main.className = "turn-overview-main";
    overview._surfaceLabel = document.createElement("strong");
    overview._statusLabel = document.createElement("span");
    overview._durationLabel = document.createElement("span");
    overview._toolsLabel = document.createElement("span");
    overview._reasoningLabel = document.createElement("span");
    main.append(overview._surfaceLabel, overview._statusLabel, overview._durationLabel, overview._toolsLabel, overview._reasoningLabel);
    overview._lifecycleLabel = document.createElement("div");
    overview._lifecycleLabel.className = "turn-overview-lifecycle";
    overview.append(main, overview._lifecycleLabel);
    turn.overviewElement = overview;
  }
  overview.className = `turn-overview surface-${turn.surface || "unknown"}`;
  setTextIfChanged(overview._surfaceLabel, turnSurfaceLabel(turn));
  overview._statusLabel.className = `turn-overview-status status-${turnStatusTone(turn)}`;
  setTextIfChanged(overview._statusLabel, turnStatusLabel(turn));
  const duration = turnDurationLabel(turn);
  overview._durationLabel.hidden = !duration;
  setTextIfChanged(overview._durationLabel, duration);
  const toolCount = turn.tools?.size || 0;
  setTextIfChanged(overview._toolsLabel, toolCount ? `${toolCount} ${toolCount === 1 ? "tool" : "tools"}` : "No tools");
  setTextIfChanged(overview._reasoningLabel, turnHasReasoning(turn) ? "Reasoning available" : "Reasoning not provided");
  setTextIfChanged(overview._lifecycleLabel, turnLifecycleLabel(turn));
  overview.setAttribute("aria-label", `${turnSurfaceLabel(turn)}. ${turnStatusLabel(turn)}. ${accessibleDurationLabel(duration)}${overview._toolsLabel.textContent}. ${overview._reasoningLabel.textContent}. ${overview._lifecycleLabel.textContent}.`);
  return overview;
}

function renderTurnTail(turn) {
  const tail = document.createElement("div");
  tail.className = "turn-tail";
  for (const permissionID of turn.permissionIDs) {
    const permission = transcriptState.permissions.get(permissionID);
    if (!permission || permission.state === "waiting") continue;
    const resolved = document.createElement("div");
    resolved.className = "resolved-permission";
    resolved.textContent = permission.state === "abandoned" ? "Permission request closed" : `Permission resolved${permission.decision ? ` · ${permission.decision.replaceAll("_", " ")}` : ""}`;
    tail.append(resolved);
  }
  if (["queued", "working", "cancelling", "cancelled"].includes(turn.state)) {
    const state = document.createElement("div");
    state.className = "turn-state-line";
    const dot = document.createElement("i");
    const copy = ({ queued: "Queued for the next turn", working: "Working…", cancelling: "Stopping response…", cancelled: "Response stopped" })[turn.state];
    state.append(dot, document.createTextNode(copy));
    tail.append(state);
  }
  if (turn.state === "complete") {
    const retainedResponse = completedResponseBlock(turn);
    const retainedReasoning = turn.blocks.some((block) => block.type === "thought" && block.text.trim());
    if (turnHasReasoning(turn) && !retainedReasoning) {
      const unavailable = document.createElement("div");
      unavailable.className = "reasoning-unavailable";
      unavailable.textContent = "Earlier reasoning is outside this bounded view; committed provider history remains unchanged.";
      tail.append(unavailable);
    }
    if (!turn.assistantSeen) {
      const missing = document.createElement("div");
      missing.className = "response-missing";
      missing.textContent = "The turn completed without a retained assistant response. Check Diagnostics or open the native provider CLI.";
      tail.append(missing);
    } else if (!retainedResponse) {
      const missing = document.createElement("div");
      missing.className = "response-missing";
      missing.textContent = "Assistant output is outside this bounded view; open the native provider CLI or local diagnostics for more detail.";
      tail.append(missing);
    }
  }
  if (turn.contentClipped) {
    const clipped = document.createElement("div");
    clipped.className = "turn-limit-note";
    clipped.textContent = "Older detail in this turn is hidden to keep the browser responsive.";
    tail.append(clipped);
  }
  return tail;
}

const selectionScopeSelectors = [".message-body", ".thought-content", ".tool-detail", ".tool-inline-preview", ".plan-list", ".error-card"];

function selectionOffsets(root) {
  const selection = window.getSelection?.();
  if (!selection || selection.rangeCount === 0) return null;
  const range = selection.getRangeAt(0);
  if (!root.contains(range.startContainer) || !root.contains(range.endContainer)) return null;
  let scope = root;
  let selector = "";
  for (const candidateSelector of selectionScopeSelectors) {
    const candidate = root.querySelector?.(candidateSelector);
    if (candidate?.contains(range.startContainer) && candidate.contains(range.endContainer)) {
      scope = candidate;
      selector = candidateSelector;
      break;
    }
  }
  return {
    start: selectionPoint(scope, range.startContainer, range.startOffset),
    end: selectionPoint(scope, range.endContainer, range.endOffset),
    selector,
    text: range.toString()
  };
}

function textNodes(root) {
  if (!document.createTreeWalker || typeof NodeFilter === "undefined") return [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const nodes = [];
  let node = walker.nextNode();
  while (node) {
    nodes.push(node);
    node = walker.nextNode();
  }
  return nodes;
}

function selectionPoint(root, container, offset) {
  const nodes = textNodes(root);
  let absolute = 0;
  for (let index = 0; index < nodes.length; index += 1) {
    const node = nodes[index];
    if (node === container) {
      const localOffset = Math.max(0, Math.min(offset, node.data.length));
      return {
        index, offset: localOffset, absolute: absolute + localOffset,
        before: node.data.slice(Math.max(0, localOffset - 128), localOffset),
        after: node.data.slice(localOffset, localOffset + 128)
      };
    }
    absolute += node.data.length;
  }
  const before = document.createRange?.();
  if (!before) return { index: -1, offset: 0, absolute: 0 };
  before.selectNodeContents(root);
  before.setEnd(container, offset);
  return { index: -1, offset: 0, absolute: before.toString().length };
}

function textPoint(root, offset) {
  if (!document.createTreeWalker || typeof NodeFilter === "undefined") return null;
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  let remaining = Math.max(0, offset);
  let node = walker.nextNode();
  while (node) {
    if (remaining <= node.data.length) return { node, offset: remaining };
    remaining -= node.data.length;
    node = walker.nextNode();
  }
  return root.lastChild ? { node: root.lastChild, offset: root.lastChild.childNodes?.length || 0 } : null;
}

function pointFromSnapshot(root, snapshot) {
  if (!snapshot) return null;
  const nodes = textNodes(root);
  if (snapshot.index >= 0 && snapshot.index < nodes.length) {
    const node = nodes[snapshot.index];
    const localOffset = Math.max(0, Math.min(snapshot.offset, node.data.length));
    const before = node.data.slice(Math.max(0, localOffset - 128), localOffset);
    const after = node.data.slice(localOffset, localOffset + 128);
    if (snapshot.offset <= node.data.length && before === snapshot.before && (after.startsWith(snapshot.after) || snapshot.after.startsWith(after))) {
      return { node, offset: localOffset };
    }
  }
  return textPoint(root, snapshot.absolute);
}

function restoreSelection(root, offsets) {
  if (!offsets || !document.createRange || !window.getSelection) return;
  const scope = offsets.selector ? root.querySelector?.(offsets.selector) : root;
  if (!scope) return;
  let start = pointFromSnapshot(scope, offsets.start);
  let end = pointFromSnapshot(scope, offsets.end);
  if (!start || !end) return;
  try {
    let range = document.createRange();
    range.setStart(start.node, start.offset);
    range.setEnd(end.node, end.offset);
    if (offsets.text && range.toString() !== offsets.text) {
      const flat = textNodes(scope).map((node) => node.data).join("");
      let match = flat.indexOf(offsets.text);
      let nearest = -1;
      while (match >= 0) {
        if (nearest < 0 || Math.abs(match - offsets.start.absolute) < Math.abs(nearest - offsets.start.absolute)) nearest = match;
        match = flat.indexOf(offsets.text, match + 1);
      }
      if (nearest >= 0) {
        const matchedStart = textPoint(scope, nearest);
        const matchedEnd = textPoint(scope, nearest + offsets.text.length);
        if (matchedStart && matchedEnd) {
          range = document.createRange();
          range.setStart(matchedStart.node, matchedStart.offset);
          range.setEnd(matchedEnd.node, matchedEnd.offset);
        }
      }
    }
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
  } catch (_) {
    // A concurrently removed text node is harmless; the next update remains live.
  }
}

function focusIndex(root) {
  const active = document.activeElement;
  if (!active || !root.contains(active)) return -1;
  return Array.from(root.querySelectorAll("a[href], button, summary, [tabindex]")).indexOf(active);
}

function patchBlockElement(block, assistantName, context = {}) {
  const previous = block.element;
  const contextKey = `${block.source || "acp"}:${context.streaming ? "streaming" : "stored"}:${context.final ? "final" : "update"}`;
  if (previous && block.renderedRevision === block.revision && block.renderedContext === contextKey) return previous;
  const selection = previous ? selectionOffsets(previous) : null;
  const focusedIndex = previous ? focusIndex(previous) : -1;
  const details = previous?.matches?.("details[data-detail-key]") ? previous : null;
  const userOpen = details?.dataset.userToggled === "true" ? details.open : null;
  const next = renderBlockElement(block, assistantName, context);
  if (next?.matches?.("details[data-detail-key]") && userOpen !== null) {
    next.open = userOpen;
    next.dataset.userToggled = "true";
  }
  if (previous?.isConnected) {
    if (next) previous.replaceWith(next);
    else previous.remove();
  }
  block.element = next;
  block.renderedRevision = block.revision;
  block.renderedContext = contextKey;
  if (next && focusedIndex >= 0) {
    const focusable = next.querySelectorAll("a[href], button, summary, [tabindex]")[focusedIndex];
    focusable?.focus({ preventScroll: true });
  }
  if (next) restoreSelection(next, selection);
  return next;
}

function patchTurnElement(turn, assistantName) {
  let article = turn.element;
  if (!article) {
    article = document.createElement("article");
    article.className = "turn";
    article.setAttribute("aria-label", "Conversation turn");
    const blocks = document.createElement("div");
    blocks.className = "turn-blocks";
    article.append(blocks);
    turn.element = article;
  }
  article.dataset.state = turn.state || "";
  const blocks = article.querySelector(".turn-blocks");
  const desired = [];
  const overview = turnOverviewElement(turn);
  const completedResponse = completedResponseBlock(turn);
  let overviewInserted = false;
  for (const block of turn.blocks) {
    const element = patchBlockElement(block, assistantName, {
      streaming: turn.state === "working",
      final: block === completedResponse
    });
    if (element) {
      desired.push(element);
      if (block.type === "user") {
        desired.push(overview);
        overviewInserted = true;
      }
    }
  }
  if (!overviewInserted) desired.unshift(overview);
  for (let index = 0; index < desired.length; index += 1) {
    if (blocks.children[index] !== desired[index]) blocks.insertBefore(desired[index], blocks.children[index] || null);
  }
  while (blocks.children.length > desired.length) blocks.lastElementChild.remove();
  const previousTail = article.querySelector(".turn-tail");
  const tail = renderTurnTail(turn);
  if (previousTail) previousTail.replaceWith(tail);
  else article.append(tail);
  turn.dirty = false;
  return article;
}

function captureConversationScrollAnchor(scroller, container) {
  if (!scroller || !container || transcriptState.shouldStick || typeof scroller.getBoundingClientRect !== "function") return null;
  const scrollerTop = scroller.getBoundingClientRect().top;
  // Prefer a semantic block over its containing turn. Reconciliation can add
  // stored reasoning above a reused response inside the same turn; anchoring
  // only the article would let the response jump even though its own viewer
  // block survives. Keeping the block object lets patchBlockElement replace
  // its DOM node while this anchor follows block.element to the replacement.
  for (const turn of orderedTurns()) {
    for (const block of turn.blocks) {
      const element = block.element;
      if (!element?.isConnected || typeof element.getBoundingClientRect !== "function") continue;
      const rect = element.getBoundingClientRect();
      if (rect.bottom > scrollerTop + 1) return { owner: block, element, offset: rect.top - scrollerTop };
    }
  }
  for (const element of Array.from(container.children || [])) {
    if (typeof element.getBoundingClientRect !== "function") continue;
    const rect = element.getBoundingClientRect();
    if (rect.bottom > scrollerTop + 1) return { element, offset: rect.top - scrollerTop };
  }
  return null;
}

function restoreConversationScrollAnchor(scroller, anchor, fallbackTop) {
  if (!scroller) return;
  const previousBehavior = scroller.style?.scrollBehavior || "";
  if (scroller.style) scroller.style.scrollBehavior = "auto";
  const element = anchor?.owner?.element || anchor?.element;
  if (element?.isConnected && typeof element.getBoundingClientRect === "function" && typeof scroller.getBoundingClientRect === "function") {
    const currentOffset = element.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
    scroller.scrollTop += currentOffset - anchor.offset;
  } else {
    scroller.scrollTop = fallbackTop;
  }
  if (scroller.style) scroller.style.scrollBehavior = previousBehavior;
}

function renderConversation() {
  const container = document.querySelector("#conversation");
  const scroller = document.querySelector("#conversation-scroll");
  if (!container || !scroller) return;
  updateHistorySummary();
  const priorTop = scroller.scrollTop;
  const scrollAnchor = captureConversationScrollAnchor(scroller, container);
  const assistantName = document.querySelector(".session-title h1")?.firstChild?.textContent?.trim() || "Agent";
  if (transcriptState.turns.length === 0) {
    if (!container.querySelector(".conversation-empty")) {
      const empty = document.createElement("div");
      empty.className = "conversation-empty";
      const icon = document.createElement("span");
      icon.className = "empty-orbit";
      icon.textContent = "◎";
      const heading = document.createElement("h2");
      heading.textContent = "Start the conversation";
      const copy = document.createElement("p");
      copy.textContent = "Send a message below. This structured ACP view remains linked to the provider's canonical conversation.";
      empty.append(icon, heading, copy);
      if (transcriptState.sourceWarning) {
        const source = document.createElement("p");
        source.className = "muted";
        source.textContent = transcriptState.sourceWarning;
        empty.append(source);
      }
      container.replaceChildren(empty);
    }
  } else {
    const desired = [];
    if (transcriptState.sourceWarning || transcriptState.snapshotTruncated) {
      let sourceNotice = container.querySelector("#conversation-source-note");
      if (!sourceNotice) {
        sourceNotice = document.createElement("div");
        sourceNotice.id = "conversation-source-note";
        sourceNotice.className = "history-limit-note";
      }
      sourceNotice.textContent = transcriptState.sourceWarning || (transcriptState.source === "provider-store"
        ? "Older provider history is outside this bounded view."
        : "Earlier gateway history is outside this bounded view.");
      desired.push(sourceNotice);
    }
    if (transcriptState.historyClipped) {
      let notice = container.querySelector("#history-limit-note");
      if (!notice) {
        notice = document.createElement("div");
        notice.id = "history-limit-note";
        notice.className = "history-limit-note";
      }
      notice.textContent = transcriptState.source === "provider-store"
        ? "Earlier conversation is outside this bounded browser view; committed provider history remains unchanged."
        : "Earlier conversation is outside this bounded browser view; the private gateway JSONL remains unchanged.";
      desired.push(notice);
    }
    for (const turn of orderedTurns()) {
      if (!turn.element || turn.dirty) patchTurnElement(turn, assistantName);
      desired.push(turn.element);
    }
    for (let index = 0; index < desired.length; index += 1) {
      if (container.children[index] !== desired[index]) container.insertBefore(desired[index], container.children[index] || null);
    }
    while (container.children.length > desired.length) container.lastElementChild.remove();
  }
  renderApprovalShelf();
  renderDiagnostics();
  const jump = document.querySelector("#jump-latest");
  if (transcriptState.shouldStick) {
    scroller.scrollTop = scroller.scrollHeight;
    transcriptState.newUpdates = 0;
    if (jump) jump.hidden = true;
  } else {
    restoreConversationScrollAnchor(scroller, scrollAnchor, priorTop);
    if (jump) {
      jump.hidden = false;
      jump.textContent = `${transcriptState.newUpdates} new update${transcriptState.newUpdates === 1 ? "" : "s"} ↓`;
    }
  }
  transcriptState.shouldStick = nearBottom(scroller);
}

function latestWaitingPermission() {
  const permissions = Array.from(transcriptState.permissions.values());
  for (let index = permissions.length - 1; index >= 0; index -= 1) {
    if (permissions[index].state === "waiting") return permissions[index];
  }
  return null;
}

let renderedPermissionSignature = "";
let permissionDecisionsReady = false;
let permissionDecisionBlockedCode = "";
let permissionDecisionBlockedMessage = "";
function renderApprovalShelf() {
  const shelf = document.querySelector("#approval-shelf");
  if (!shelf) return;
  const announcer = document.querySelector("#approval-announcer");
  const permission = latestWaitingPermission();
  const signature = permission ? JSON.stringify([permission.id, permission.state, permission.title, permission.options, transcriptState.permissionOverflow, permissionDecisionsReady, permissionDecisionBlockedCode, permissionDecisionBlockedMessage]) : `none:${transcriptState.permissionOverflow}`;
  if (signature === renderedPermissionSignature) {
    renderSessionStatus();
    return;
  }
  renderedPermissionSignature = signature;
  if (!permission) {
    shelf.hidden = true;
    shelf.replaceChildren();
    if (announcer) announcer.textContent = "";
    renderSessionStatus();
    return;
  }
  const heading = document.createElement("div");
  heading.className = "approval-heading";
  const icon = document.createElement("span");
  icon.className = "approval-icon";
  icon.textContent = "!";
  const copy = document.createElement("div");
  const title = document.createElement("strong");
  title.textContent = `Approval required · ${permission.title || "Tool request"}`;
  const detail = document.createElement("p");
  detail.textContent = !permissionDecisionsReady
    ? (permissionDecisionBlockedCode === "worker_upgrade_required" ? "This request is preserved, but the older worker must be upgraded before a browser decision is safe." : "Checking whether this worker can safely accept a decision…")
    : transcriptState.permissionOverflow ? "Many approvals are pending; none have been discarded. Close this page safely or resolve them one at a time." : "The Session remains safely waiting if you close this page.";
  copy.append(title, detail);
  heading.append(icon, copy);
  const options = document.createElement("div");
  options.className = "permission-options";
  if (!permissionDecisionsReady) {
    const blocked = document.createElement("div");
    blocked.className = "approval-blocked";
    blocked.textContent = permissionDecisionBlockedMessage || "Permission controls remain disabled until the live worker capability check succeeds.";
    options.append(blocked);
  } else {
    for (const option of permission.options || []) {
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = option.name || option.kind.replaceAll("_", " ");
      if (String(option.kind).startsWith("reject")) button.classList.add("reject");
      if (["allow_always", "reject_always"].includes(option.kind)) button.classList.add("persistent");
      button.addEventListener("click", () => submitPermission(permission, option));
      options.append(button);
    }
  }
  shelf.replaceChildren(heading, options);
  shelf.hidden = false;
  if (announcer) {
    announcer.textContent = "";
    const announcement = permissionDecisionBlockedCode === "worker_upgrade_required"
      ? `Approval blocked. ${permission.title || "Tool request"}. The older worker must be upgraded before a decision is safe.`
      : `Approval required. ${permission.title || "Tool request"}.`;
    window.setTimeout(() => { announcer.textContent = announcement; }, 0);
  }
  renderSessionStatus();
}

function confirmPersistentChoice(option) {
  const dialog = document.querySelector("#permission-confirm-dialog");
  if (!(dialog instanceof HTMLDialogElement)) return Promise.resolve(false);
  const copy = dialog.querySelector("#permission-confirm-copy");
  if (copy) copy.textContent = `${option.name || option.kind} may apply to later tool requests in this provider conversation.`;
  return new Promise((resolve) => {
    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      accept.removeEventListener("click", onAccept);
      cancel.removeEventListener("click", onCancel);
      dialog.removeEventListener("close", onClose);
      if (dialog.open) dialog.close();
      resolve(value);
    };
    const accept = dialog.querySelector('[data-permission-confirm="accept"]');
    const cancel = dialog.querySelector('[data-permission-confirm="cancel"]');
    const onAccept = () => finish(true);
    const onCancel = () => finish(false);
    const onClose = () => finish(false);
    accept.addEventListener("click", onAccept);
    cancel.addEventListener("click", onCancel);
    dialog.addEventListener("close", onClose);
    dialog.showModal();
  });
}

async function submitPermission(permission, option) {
  const persistent = ["allow_always", "reject_always"].includes(option.kind);
  if (persistent && !(await confirmPersistentChoice(option))) return;
  const shelf = document.querySelector("#approval-shelf");
  for (const button of shelf?.querySelectorAll("button") || []) button.disabled = true;
  const notice = document.querySelector("#action-notice");
  setNotice(notice, "Sending permission decision…");
  try {
    const sessionID = document.body.dataset.sessionId;
    await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/permissions/decision`, "POST", {
      permission_id: permission.id, option_id: option.id, cancelled: false
    });
    transcriptState.permissions.set(permission.id, { ...permission, state: "resolved", decision: option.kind });
    markPermissionDirty(permission.id);
    enforcePermissionBound();
    const rejected = String(option.kind).startsWith("reject");
    setNotice(notice, rejected ? "Permission rejected. The agent will continue without that action." : "Permission allowed. The agent can continue.");
    scheduleTranscriptRender();
    refreshSession(sessionID).catch(() => {});
  } catch (error) {
    setNotice(notice, permissionActionErrorMessage(error), true);
    await refreshPermissions().catch(() => {});
    renderedPermissionSignature = "";
    for (const button of shelf?.querySelectorAll("button") || []) button.disabled = false;
    scheduleTranscriptRender();
  }
}

async function refreshPermissions() {
  const sessionID = document.body.dataset.sessionId;
  const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/permissions`);
  const ready = Boolean(result.decisions_ready);
  const blockedCode = String(result.blocked_code || "");
  const blockedMessage = String(result.blocked_message || "");
  const readinessChanged = ready !== permissionDecisionsReady || blockedCode !== permissionDecisionBlockedCode || blockedMessage !== permissionDecisionBlockedMessage;
  permissionDecisionsReady = ready;
  permissionDecisionBlockedCode = blockedCode;
  permissionDecisionBlockedMessage = blockedMessage;
  if (readinessChanged) renderedPermissionSignature = "";
  return reconcilePermissionSnapshot(result.permissions || []) || readinessChanged;
}

function reconcilePermissionSnapshot(received) {
  if (!Array.isArray(received)) received = [];
  if (received.length > LIMITS.permissions) {
    flagPermissionOverflow();
    return true;
  }
  const waiting = new Set(received.map((permission) => permission.id));
  let changed = false;
  // The GET is authoritative. Retire disappeared waiting records before
  // admitting replacements so an exact-cap snapshot can swap IDs safely.
  for (const [id, permission] of transcriptState.permissions) {
    if (permission.state === "waiting" && !waiting.has(id)) {
      transcriptState.permissions.set(id, { ...permission, state: "resolved" });
      markPermissionDirty(id);
      changed = true;
    }
  }
  for (const permission of received) {
    if (!permission?.id || !ensurePermissionCapacity(permission.id)) continue;
    const permissionChanged = JSON.stringify(transcriptState.permissions.get(permission.id)) !== JSON.stringify(permission);
    if (permissionChanged) changed = true;
    transcriptState.permissions.set(permission.id, permission);
    if (permissionChanged) markPermissionDirty(permission.id);
  }
  enforcePermissionBound();
  return changed;
}

function markPermissionDirty(permissionID) {
  for (const turn of transcriptState.turns) {
    if (turn.permissionIDs.has(permissionID)) turn.dirty = true;
  }
}

function renderDiagnostics() {
  const feed = document.querySelector("#diagnostics-feed");
  if (!feed || !document.querySelector("#diagnostics-dialog")?.open) return;
  const fragment = document.createDocumentFragment();
  if (transcriptState.diagnostics.length === 0) {
    const empty = document.createElement("p");
    empty.className = "muted";
    empty.textContent = "Waiting for activity…";
    fragment.append(empty);
  }
  for (const activity of transcriptState.diagnostics) {
    const row = document.createElement("article");
    row.className = "diagnostic-row";
    const meta = document.createElement("div");
    meta.className = "diagnostic-meta";
    meta.textContent = `#${activity.position}\n${activity.kind}`;
    const data = document.createElement("pre");
    data.textContent = JSON.stringify(activity, null, 2);
    row.append(meta, data);
    fragment.append(row);
  }
  feed.replaceChildren(fragment);
}

let activeEventSource = null;
function shouldReconcileConversationKind(kind) {
  return ["turn_completed", "turn_error", "native_refreshed", "native_refresh_recovered", "provider_transcript_reconciled", "acp_rehydrated"].includes(kind);
}

function noteProviderTranscriptReconcileWarning(kind, sessionID = document.body.dataset.sessionId) {
  if (kind !== "provider_transcript_reconcile_warning") return false;
  transcriptState.sourceWarning = "Provider transcript reload is delayed; live ACP output remains current.";
  updateHistorySource(transcriptState.source, transcriptState.sourceWarning);
  if (sessionID) scheduleProviderAuthorityRetry(sessionID);
  return true;
}

function attachActivityStream(path) {
  if (!window.EventSource) return;
  activeEventSource?.close();
  const state = document.querySelector("#stream-state");
  // `seed` applies only to this EventSource's first request. On automatic
  // reconnect the server prefers EventSource's advancing Last-Event-ID, so a
  // stale URL seed can never conflict with the live cursor.
  const separator = path.includes("?") ? "&" : "?";
  const streamPath = transcriptState.lastPosition > 0 ? `${path}${separator}seed=${transcriptState.lastPosition}` : path;
  const source = new EventSource(streamPath);
  activeEventSource = source;
  source.onopen = () => {
    state?.classList.add("connected");
    const label = state?.querySelector("span");
    if (label) label.textContent = "Live";
  };
  source.addEventListener("ready", () => { streamAnnouncementsReady = true; });
  source.addEventListener("activity", (event) => {
    try {
      const activity = JSON.parse(event.data);
      bufferConversationActivity(activity);
      noteStreamActivity(activity);
      if (applyActivity(activity)) scheduleTranscriptRender();
      if (noteProviderTranscriptReconcileWarning(activity.kind)) scheduleTranscriptRender();
      if (shouldReconcileConversationKind(activity.kind)) {
        conversationReconcileAttempts = 0;
        scheduleConversationReconcile(document.body.dataset.sessionId, activity.kind === "turn_completed" ? 100 : 180);
      }
    } catch (_) {
      state?.classList.remove("connected");
      const label = state?.querySelector("span");
      if (label) label.textContent = "Invalid update";
    }
  });
  source.onerror = () => {
    state?.classList.remove("connected");
    const label = state?.querySelector("span");
    if (label) label.textContent = "Reconnecting";
  };
  window.setTimeout(() => { if (transcriptState.turns.length === 0) renderConversation(); }, 450);
  window.addEventListener("beforeunload", () => source.close(), { once: true });
}

function reconcileStreamAvailability(sessionID, running) {
  const state = document.querySelector("#stream-state");
  if (!running && activeEventSource) {
    activeEventSource.close();
    activeEventSource = null;
    state?.classList.remove("connected");
    const label = state?.querySelector("span");
    if (label) label.textContent = "Stored history";
  } else if (running && !activeEventSource && !transcriptState.permissionOverflow) {
    attachActivityStream(`/api/v1/sessions/${encodeURIComponent(sessionID)}/stream`);
  }
}

function hasLocallyPreparingTurn() {
  return transcriptState.turns.some((turn) => !turn.committed && ["ready", "working", "cancelling"].includes(turn.state));
}

function sessionActionVisible(action, visibleStatuses, status) {
  return visibleStatuses.includes(status) || (action === "cancel" && hasLocallyPreparingTurn());
}

function updateActionVisibility(status) {
  document.body.dataset.sessionStatus = status;
  for (const action of document.querySelectorAll("[data-visible-status]")) {
    const visible = sessionActionVisible(action.dataset.action || "", action.dataset.visibleStatus.split(","), status);
    action.dataset.statusHidden = visible ? "false" : "true";
  }
  const submitCopy = document.querySelector(".send-button span");
  const submit = document.querySelector(".send-button");
  const queueing = ["working", "starting"].includes(status);
  if (submitCopy) submitCopy.textContent = queueing ? "Queue" : "Send";
  if (submit) submit.setAttribute("aria-label", queueing ? "Queue message" : "Send message");
}

let currentSessionRuntimeStatus = document.body.dataset.sessionStatus || "";
function setRuntimeStatus(status) {
  if (!status) return;
  currentSessionRuntimeStatus = status;
  renderSessionStatus();
  updateActionVisibility(status);
}

function updateRuntimeStatusFromDisplay(display) {
  if (page !== "session" || !display) return;
  if (display.type === "error_upsert" || display.type === "turn_state") reconcileRuntimeStatusSoon();
}

let runtimeReconcileTimer = 0;
function reconcileRuntimeStatusSoon() {
  window.clearTimeout?.(runtimeReconcileTimer);
  runtimeReconcileTimer = window.setTimeout(() => {
    const sessionID = document.body.dataset.sessionId;
    if (sessionID) refreshSession(sessionID).catch(() => {});
  }, 160);
}

function renderSessionStatus() {
  const element = document.querySelector("#session-status");
  if (!element) return;
  if (latestWaitingPermission()) {
    element.className = "status status-waiting";
    const label = permissionDecisionBlockedCode === "worker_upgrade_required" ? "Upgrade required" : "Needs approval";
    element.replaceChildren(document.createElement("i"), document.createTextNode(label));
    return;
  }
  setStatus(element, currentSessionRuntimeStatus);
}

async function refreshSession(sessionID) {
  const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}`);
  const current = result.session;
  setRuntimeStatus(current.status);
  const conversation = document.querySelector("#conversation-state");
  if (conversation) conversation.textContent = current.conversation_verified ? "Verified" : (current.conversation_ready ? "Linked" : "Pending provider identity");
  const continuity = document.querySelector("#continuity-chip");
  if (continuity) {
    continuity.className = `continuity-chip${current.conversation_verified ? " verified" : (current.conversation_ready ? " linked" : "")}`;
    continuity.textContent = current.conversation_verified ? "✓ Verified" : (current.conversation_ready ? "↔ Linked" : "○ Linking");
  }
  const navDot = findNavSession(sessionID)?.querySelector(".status-dot");
  if (navDot) {
    navDot.className = `status-dot status-${statusTone(current.status)}`;
    navDot.setAttribute("aria-label", statusLabel(current.status));
  }
  reconcileStreamAvailability(sessionID, current.worker_running);
  return current;
}

function bindComposer(sessionID) {
  const form = document.querySelector("#prompt-form");
  const textarea = form?.querySelector("textarea");
  const notice = document.querySelector("#action-notice");
  if (!form || !textarea) return;
  const resize = () => {
    const visualLines = textarea.value.split("\n").reduce((count, line) => count + Math.max(1, Math.ceil(Array.from(line).length / 76)), 0);
    textarea.rows = Math.min(8, Math.max(1, visualLines));
  };
  textarea.addEventListener("input", resize);
  textarea.addEventListener("keydown", (event) => {
    const coarse = window.matchMedia("(pointer: coarse)").matches;
    if (event.key === "Enter" && !event.shiftKey && !event.isComposing && !coarse) {
      event.preventDefault();
      form.requestSubmit();
    }
  });
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!beginFormSubmission(form)) return;
    const message = textarea.value.trim();
    if (!message) {
      endFormSubmission(form);
      return;
    }
    const submit = form.querySelector("button[type=submit]");
    submit.disabled = true;
    setNotice(notice, "Sending…");
    try {
      const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/prompt`, "POST", { message });
      const turn = transcriptState.byRequest.get(result.request_id) || turnFor({ request_id: result.request_id, position: Number.POSITIVE_INFINITY });
      if (!turn.blocks.some((block) => block.type === "user")) {
        const value = truncateUTF8(message, LIMITS.assistant);
        if (turn.blocks.length < LIMITS.blocksPerTurn) turn.blocks.unshift({ type: "user", text: value.text, bytes: value.bytes, clipped: value.clipped });
      }
      applyOptimisticPromptReceipt(turn, result.state);
      turn.dirty = true;
      updateTurnRetention(turn);
      trimTurns();
      textarea.value = "";
      resize();
      setNotice(notice, result.state === "queued" ? "Message queued for the next ACP turn." : "Message accepted; loading the exact conversation before dispatch.");
      transcriptState.shouldStick = true;
      scheduleTranscriptRender();
    } catch (error) {
      setNotice(notice, error.message, true);
    } finally {
      endFormSubmission(form);
      submit.disabled = false;
      textarea.focus();
    }
  });
  if (!window.matchMedia("(pointer: coarse)").matches) window.setTimeout(() => textarea.focus(), 0);
}

function bindSessionControls(sessionID) {
  const notice = document.querySelector("#action-notice");
  for (const button of document.querySelectorAll("[data-action]")) {
    button.addEventListener("click", async () => {
      const action = button.dataset.action;
      button.disabled = true;
      setNotice(notice, action === "cancel" ? "Stopping the active response…" : `${action[0].toUpperCase()}${action.slice(1)} requested…`);
      try {
        await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/${action}`, "POST", action === "resume" ? {} : undefined);
        setNotice(notice, action === "cancel" ? "Cancellation requested. The Session remains resumable." : `${action[0].toUpperCase()}${action.slice(1)} completed.`);
        button.closest("dialog")?.close();
        await refreshSession(sessionID);
      } catch (error) {
        setNotice(notice, error.message, true);
      } finally {
        button.disabled = false;
      }
    });
  }

  const reviewForm = document.querySelector("#review-form");
  const reviewNotice = document.querySelector("#review-notice");
  reviewForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = reviewForm.querySelector("button[type=submit]");
    submit.disabled = true;
    setNotice(reviewNotice, "Creating independent reviewer Session…");
    try {
      const data = new FormData(reviewForm);
      const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/review`, "POST", {
        agent: data.get("agent"), model: data.get("model"), reasoning_effort: data.get("reasoning_effort"),
        allow_active_producer: data.has("allow_active_producer")
      });
      rememberSessionStartWarning(result);
      window.location.assign(`/sessions/${encodeURIComponent(result.session.id)}`);
    } catch (error) {
      setNotice(reviewNotice, error.message, true);
      submit.disabled = false;
    }
  });

  const scroller = document.querySelector("#conversation-scroll");
  const jump = document.querySelector("#jump-latest");
  scroller?.addEventListener("scroll", () => {
    if (nearBottom(scroller)) {
      transcriptState.shouldStick = true;
      transcriptState.newUpdates = 0;
      if (jump) jump.hidden = true;
    } else {
      transcriptState.shouldStick = false;
    }
  }, { passive: true });
  jump?.addEventListener("click", () => {
    transcriptState.shouldStick = true;
    transcriptState.newUpdates = 0;
    scroller.scrollTop = scroller.scrollHeight;
    jump.hidden = true;
  });
  document.querySelector("#diagnostics-dialog")?.addEventListener("toggle", renderDiagnostics);
  document.querySelector("#diagnostics-dialog")?.addEventListener("close", renderDiagnostics);
}

bindShell();
bindTaskCreation();

if (page === "task") {
  const taskID = document.body.dataset.taskId;
  bindSessionCreation(taskID);
  refreshTask(taskID).catch(() => {});
  window.setInterval(() => { if (!document.hidden) refreshTask(taskID).catch(() => {}); }, 4000);
}

if (page === "session") {
  const sessionID = document.body.dataset.sessionId;
  const taskID = document.body.dataset.taskId;
  bindSessionCreation(taskID);
  bindComposer(sessionID);
  bindSessionControls(sessionID);
  restoreSessionStartWarning(sessionID);
  updateActionVisibility(document.body.dataset.sessionStatus);
  (async () => {
    await reconcileConversation(sessionID, { initial: true });
    attachActivityStream(`/api/v1/sessions/${encodeURIComponent(sessionID)}/stream`);
    refreshPermissions().then(scheduleTranscriptRender).catch(() => {});
  })();
  window.setInterval(() => {
    if (document.hidden) return;
    refreshSession(sessionID).catch(() => {});
    refreshPermissions().then((changed) => { if (changed) scheduleTranscriptRender(); }).catch(() => {});
  }, 4000);
}
