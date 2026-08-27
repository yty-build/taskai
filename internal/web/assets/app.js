"use strict";

const page = document.body.dataset.page;
const textEncoder = new TextEncoder();
const LIMITS = Object.freeze({
  assistant: 128 << 10, thought: 32 << 10, tool: 64 << 10, toolTotal: 256 << 10,
  diagnostics: 1000, diagnosticBytes: 8 << 20, diagnosticSample: 512 << 10,
  turns: 256, blocksPerTurn: 512,
  toolsPerTurn: 256, permissions: 128, permissionOptions: 16, retained: 16 << 20, announcement: 768,
  identifier: 256, permissionIdentifier: 512, title: 1024, metadata: 512,
  kind: 128, status: 128, errorProvenance: 256, stopReason: 512, permissionDecision: 512,
  replayReady: 1024
});

async function apiRequest(path, method = "GET", body, requestOptions = {}) {
  const options = { method, headers: { Accept: "application/json" } };
  if (requestOptions.signal) options.signal = requestOptions.signal;
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
    setNotice(notice, "Repository selected. Review the path, then connect the Session.");
  });
  notice.classList.toggle("has-action", true);
  notice.replaceChildren(explanation, use);
  return true;
}

const providerOptionSpecs = Object.freeze({
  model: Object.freeze({ label: "Model", valueLimit: 256 }),
  reasoning_effort: Object.freeze({ label: "Reasoning effort", valueLimit: 128 }),
});

function boundedProviderOptionText(value, limit) {
  if (typeof value !== "string") return "";
  return truncateUTF8(value.replace(/[\u0000-\u001f\u007f]/g, ""), limit).text;
}

function normalizedProviderChoices(control, valueLimit) {
  if (control?.supported !== true || !Array.isArray(control.choices)) return [];
  const choices = [];
  const seen = new Set();
  for (const candidate of control.choices.slice(0, 256)) {
    const value = typeof candidate?.value === "string" ? candidate.value : "";
    if (!value || textEncoder.encode(value).length > valueLimit || seen.has(value)) continue;
    const label = boundedProviderOptionText(candidate.label, 256) || value;
    const group = boundedProviderOptionText(candidate.group, 80);
    seen.add(value);
    choices.push({ value, label, group });
  }
  return choices;
}

function renderProviderOptionControl(select, badge, control, spec) {
  if (!select || !spec) return { choices: [], reason: "not_advertised" };
  const defaultOption = document.createElement("option");
  defaultOption.value = "";
  const currentValue = boundedProviderOptionText(control?.current_value, 96);
  defaultOption.textContent = currentValue ? `Keep current (${currentValue})` : "Keep current provider default";
  const choices = normalizedProviderChoices(control, spec.valueLimit);
  const children = [defaultOption];
  const groups = new Map();
  for (const choice of choices) {
    const option = document.createElement("option");
    option.value = choice.value;
    option.textContent = choice.label;
    if (!choice.group) {
      children.push(option);
      continue;
    }
    let group = groups.get(choice.group);
    if (!group) {
      group = document.createElement("optgroup");
      group.label = choice.group;
      groups.set(choice.group, group);
      children.push(group);
    }
    group.append(option);
  }
  select.replaceChildren(...children);
  select.value = "";
  select.disabled = choices.length === 0;
  select.dataset.providerSupported = String(choices.length > 0);
  const reason = ["not_advertised", "ambiguous_or_invalid"].includes(control?.unavailable_reason)
    ? control.unavailable_reason : (choices.length > 0 ? "" : "not_advertised");
  select.dataset.unavailableReason = reason;
  if (badge) {
    badge.textContent = choices.length > 0 ? `${choices.length} available` : (reason === "ambiguous_or_invalid" ? "Unavailable" : "Provider default only");
    badge.classList.toggle("available", choices.length > 0);
    badge.classList.toggle("unavailable", choices.length === 0);
  }
  return { choices, reason };
}

function renderProviderOptions(form, result) {
  const catalog = result?.provider_options || {};
  let availableControls = 0;
  const unavailable = [];
  for (const [name, spec] of Object.entries(providerOptionSpecs)) {
    const select = form?.querySelector(`[data-provider-option="${name}"]`);
    const badge = form?.querySelector(`[data-provider-option-badge="${name}"]`);
    const rendered = renderProviderOptionControl(select, badge, catalog[name], spec);
    if (rendered.choices.length > 0) availableControls += 1;
    else unavailable.push({ label: spec.label, reason: rendered.reason });
  }
  const panel = form?.querySelector("[data-provider-options]");
  panel?.classList.toggle("provider-options-ready", true);
  panel?.setAttribute("aria-busy", "false");
  const status = form?.querySelector("[data-provider-options-status]");
  if (status) {
    const invalid = unavailable.filter((item) => item.reason === "ambiguous_or_invalid").map((item) => item.label);
    const messages = [];
    if (invalid.length > 0) {
      messages.push(`${invalid.join(" and ")} choices were ambiguous or invalid, so those controls are unavailable.`);
    }
    if (availableControls > 0) messages.push("Keep the current values or choose exact advertised values, then apply changes.");
    else if (messages.length === 0) messages.push("This agent does not advertise configurable model or reasoning effort.");
    if (unavailable.length > 0) messages.push("Unavailable controls keep their current values.");
    status.textContent = messages.join(" ");
  }
  return availableControls;
}

function providerSelection(form, name) {
  const select = form?.querySelector(`[data-provider-option="${name}"]`);
  if (!select || select.disabled) return "";
  const value = typeof select.value === "string" ? select.value : "";
  if (!value) return "";
  const choices = Array.from(select.querySelectorAll?.("option") || []);
  if (!choices.some((option) => option.value === value)) {
    throw new Error("Provider choices changed. Reload the available options before applying them.");
  }
  return value;
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

function setSessionDisplayStatus(element, item) {
  setStatus(element, item?.status);
}

function sessionDisplayStatus(item) {
  return { tone: statusTone(item?.status), label: statusLabel(item?.status) };
}

function agentInitial(value) {
  return Array.from(String(value || "A").trim())[0]?.toUpperCase() || "A";
}

function sessionRoleLabel(item) {
  const role = String(item?.role || "Session");
  return item?.parent_session_id ? `Child · ${role}` : role;
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
    button.addEventListener("click", () => {
      const dialog = button.closest("dialog");
      if (dialog?.querySelector('[data-provider-options-form][aria-busy="true"]')) return;
      dialog?.close();
    });
  }
  for (const dialog of document.querySelectorAll("dialog")) {
    dialog.addEventListener("cancel", (event) => {
      if (dialog.querySelector('[data-provider-options-form][aria-busy="true"]')) event.preventDefault();
    });
    dialog.addEventListener("close", () => {
      const target = dialogReturnFocus.get(dialog);
      if (target && !document.querySelector("dialog[open]")) window.setTimeout(() => target.focus(), 0);
    });
    dialog.addEventListener("click", (event) => {
      if (event.target !== dialog) return;
      if (dialog.querySelector('[data-provider-options-form][aria-busy="true"]')) return;
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

function resultSessionID(result) {
  const sessionID = typeof result?.session?.id === "string" ? result.session.id : "";
  return sessionID && sessionID.length <= 256 ? sessionID : "";
}

function sessionIdentityPayload(form) {
  const data = new FormData(form);
  return {
    agent: data.get("agent"),
    role: data.get("role"),
    repo_path: data.get("repo_path"),
    worktree: data.has("worktree"),
    approve_all: data.has("approve_all"),
    allow_shared_checkout: data.has("allow_shared_checkout")
  };
}

async function submitSessionCreation(form, path, notice) {
  if (!beginFormSubmission(form)) return;
  const submit = form.querySelector("button[type=submit]");
  submit.disabled = true;
  setNotice(notice, "Connecting Session…");
  try {
    const result = await apiRequest(path, "POST", sessionIdentityPayload(form));
    const sessionID = resultSessionID(result);
    if (!sessionID) throw new Error("The server did not return a Session.");
    window.location.assign(`/sessions/${encodeURIComponent(sessionID)}`);
  } catch (error) {
    const repoInput = form.querySelector('input[name="repo_path"]');
    if (!offerRepositorySuggestion(notice, repoInput, error)) setNotice(notice, error.message, true);
    endFormSubmission(form);
    submit.disabled = false;
  }
}

function bindSessionCreation(taskID) {
  const form = document.querySelector("#create-session-form");
  if (!form || !taskID) return;
  const notice = document.querySelector("#session-form-notice");
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    submitSessionCreation(form, `/api/v1/tasks/${encodeURIComponent(taskID)}/sessions`, notice);
  });
}

function bindChildSessionCreation(sessionID) {
  const form = document.querySelector("#create-child-session-form");
  if (!form || !sessionID) return;
  const notice = document.querySelector("#child-session-form-notice");
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    submitSessionCreation(form, `/api/v1/sessions/${encodeURIComponent(sessionID)}/children`, notice);
  });
}

async function loadProviderOptions(sessionID, form = document.querySelector("#provider-config-form")) {
  if (!form || !sessionID) return false;
  if (!providerOptionsCanLoad()) {
    syncProviderDependentControls();
    return false;
  }
  const streamEpoch = sessionStreamBoundaryEpoch;
  const connectionEpoch = providerConnectionEpoch;
  const runtimeEpoch = runtimeStatusEpoch;
  const requestEpoch = ++providerOptionsRequestEpoch;
  const requestIsCurrent = () => requestEpoch === providerOptionsRequestEpoch &&
    streamEpoch === sessionStreamBoundaryEpoch && connectionEpoch === providerConnectionEpoch &&
    runtimeEpoch === runtimeStatusEpoch && providerOptionsCanLoad();
  const panel = form.querySelector("[data-provider-options]");
  const submit = form.querySelector("button[type=submit]");
  const status = form.querySelector("[data-provider-options-status]");
  panel?.setAttribute("aria-busy", "true");
  if (submit) submit.disabled = true;
  setNotice(status, "Loading the controls advertised by this agent…");
  try {
    const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/provider-options`);
    if (!requestIsCurrent()) return false;
    renderProviderOptions(form, result);
    syncProviderDependentControls();
    return true;
  } catch (error) {
    if (!requestIsCurrent()) return false;
    if (error?.code === "session_starting") {
      setNotice(status, "Waiting for the provider connection…");
    } else {
      setNotice(status, error.message, true);
    }
    return false;
  } finally {
    if (requestEpoch === providerOptionsRequestEpoch) panel?.setAttribute("aria-busy", "false");
    syncProviderDependentControls();
  }
}

function bindProviderConfig(sessionID) {
  const form = document.querySelector("#provider-config-form");
  if (!form || !sessionID) return;
  const notice = document.querySelector("#provider-config-notice");
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!providerOptionsCanLoad()) {
      setNotice(notice, "Wait until the Session is connected and idle before changing provider options.", true);
      syncProviderDependentControls();
      return;
    }
    if (!beginFormSubmission(form)) return;
    const submit = form.querySelector("button[type=submit]");
    submit.disabled = true;
    try {
      const model = providerSelection(form, "model");
      const reasoningEffort = providerSelection(form, "reasoning_effort");
      if (!model && !reasoningEffort) {
        setNotice(notice, "Choose at least one new value to apply.");
        return;
      }
      const config = {};
      if (model) config.model = model;
      if (reasoningEffort) config.reasoning_effort = reasoningEffort;
      setNotice(notice, "Applying provider configuration…");
      const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/config`, "POST", config);
      if (result?.provider_options) renderProviderOptions(form, result);
      else await loadProviderOptions(sessionID, form);
      setNotice(notice, "Provider configuration applied.");
    } catch (error) {
      setNotice(notice, error.message, true);
    } finally {
      endFormSubmission(form);
      syncProviderDependentControls();
    }
  });
}

function makeSessionRow(item) {
  const link = document.createElement("a");
  link.className = `session-row${item.parent_session_id ? " child-session" : ""}`;
  link.href = `/sessions/${encodeURIComponent(item.id)}`;
  link.dataset.sessionId = item.id;
  if (item.parent_session_id) link.dataset.parentSessionId = item.parent_session_id;

  const avatar = document.createElement("span");
  avatar.className = "provider-avatar";
  avatar.textContent = agentInitial(item.agent);

  const copy = document.createElement("span");
  copy.className = "session-row-copy";
  const title = document.createElement("strong");
  title.append(document.createTextNode(item.agent), document.createTextNode(" · "));
  const role = document.createElement("small");
  role.textContent = sessionRoleLabel(item);
  title.append(role);
  const context = document.createElement("span");
  const path = item.repo_path || "No checkout";
  const checkout = path.split("/").filter(Boolean).pop() || path;
  context.textContent = `${checkout}${item.worktree_path ? " · isolated worktree" : ""}`;
  copy.append(title, context);

  const state = document.createElement("span");
  state.className = "session-row-state";
  const status = document.createElement("span");
  setSessionDisplayStatus(status, item);
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
  link.className = `session-row${item.parent_session_id ? " child-session" : ""}`;
  link.href = `/sessions/${encodeURIComponent(item.id)}`;
  link.dataset.sessionId = item.id;
  if (item.parent_session_id) link.dataset.parentSessionId = item.parent_session_id;
  else delete link.dataset.parentSessionId;
  const avatar = link.querySelector(".provider-avatar");
  if (avatar) avatar.textContent = agentInitial(item.agent);
  const title = link.querySelector(".session-row-copy > strong");
  if (title) {
    title.replaceChildren(document.createTextNode(item.agent), document.createTextNode(" · "));
    const role = document.createElement("small");
    role.textContent = sessionRoleLabel(item);
    title.append(role);
  }
  const context = link.querySelector(".session-row-copy > span");
  if (context) {
    const path = item.repo_path || "No checkout";
    const checkout = path.split("/").filter(Boolean).pop() || path;
    context.textContent = `${checkout}${item.worktree_path ? " · isolated worktree" : ""}`;
  }
  setSessionDisplayStatus(link.querySelector(".session-row-state .status"), item);
  const permission = link.querySelector(".session-row-state small");
  if (permission) permission.textContent = item.permission;
}

let taskSessionSignature = "";
async function refreshTask(taskID) {
  const result = await apiRequest(`/api/v1/tasks/${encodeURIComponent(taskID)}`);
  setStatus(document.querySelector("#task-status"), result.task.status);
  const count = document.querySelector("#session-count");
  if (count) count.textContent = String(result.sessions.length);
  const signature = JSON.stringify(result.sessions.map((item) => [item.id, item.parent_session_id, item.status, item.agent, item.role, item.permission, item.updated_at]));
  if (signature === taskSessionSignature) return;
  taskSessionSignature = signature;
  for (const item of result.sessions) {
    const navLink = findNavSession(item.id);
    if (navLink) {
      navLink.classList.toggle("child-session", Boolean(item.parent_session_id));
      if (item.parent_session_id) navLink.dataset.parentSessionId = item.parent_session_id;
      else delete navLink.dataset.parentSessionId;
      const role = navLink.querySelector("small");
      if (role) role.textContent = sessionRoleLabel(item);
    }
    const dot = navLink?.querySelector(".status-dot");
    if (dot) {
      const display = sessionDisplayStatus(item);
      dot.className = `status-dot status-${display.tone}`;
      dot.setAttribute("aria-label", display.label);
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

function retainedTextBytes(...values) {
  let total = 0;
  for (const value of values) total += textEncoder.encode(String(value || "")).length;
  return total;
}

function boundedTranscriptText(value, limit) {
  if (value === undefined || value === null) return "";
  return boundedProviderOptionText(String(value), limit);
}

function transcriptIdentifierDigest(value) {
  let first = 0x811c9dc5;
  let second = 0x9e3779b9;
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index);
    first = Math.imul(first ^ unit, 0x01000193) >>> 0;
    second = Math.imul(second ^ unit, 0x85ebca6b) >>> 0;
  }
  return `${value.length.toString(36)}-${first.toString(36)}-${second.toString(36)}`;
}

function boundedTranscriptIdentifier(value, fallback = "") {
  let source = value === undefined || value === null ? "" : String(value);
  if (!source) source = String(fallback || "");
  source = source.replace(/[\u0000-\u001f\u007f]/g, "");
  if (!source) source = String(fallback || "").replace(/[\u0000-\u001f\u007f]/g, "");
  const bounded = truncateUTF8(source, LIMITS.identifier);
  if (!bounded.clipped) return bounded.text;
  const suffix = `…#${transcriptIdentifierDigest(source)}`;
  const suffixBytes = retainedTextBytes(suffix);
  return `${truncateUTF8(source, Math.max(0, LIMITS.identifier - suffixBytes)).text}${suffix}`;
}

function exactPermissionIdentifier(value, limit) {
  if (value === undefined || value === null) return "";
  const identifier = String(value);
  if (!identifier || /[\u0000-\u001f\u007f-\u009f]/.test(identifier)) return "";
  return textEncoder.encode(identifier).length <= limit ? identifier : "";
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
  turn.state = acceptedState === "ready" ? "ready" : "working";
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
    turns: [], byRequest: new Map(), permissions: new Map(), permissionSizes: new Map(), diagnostics: [], diagnosticSizes: [],
    renderTimer: 0, shouldStick: true, newUpdates: 0, lastSequence: 0, framesClipped: false,
    permissionAuthoritySequence: 0, pendingPermissionSnapshot: null,
    replayReady: false, replayCursor: 0, replayEarliestSequence: 0, replayClipped: false,
    replayGapCursor: 0, replayGapEarliestSequence: 0,
    streamErrorCode: "", streamErrorMessage: "",
    diagnosticsClipped: false, diagnosticBytes: 0, permissionOverflow: false, retainedBytes: 0
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
      // A later turn may start before this batch is announced. Preserve both
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

function noteStreamFrame(frame) {
  if (!streamAnnouncementsReady) return;
  const data = frame?.data || {};
  if (frame.type === "provider_event" && data.type === "text_delta" && !providerTextIsThought(data)) {
    queueConversationAnnouncement(data.text || "");
  }
  if (frame.type === "client_command" && data.command === "prompt") queueConversationAnnouncement("", " Agent is working.");
  if (frame.type === "provider_result" && ["completed", "cancelled"].includes(data.status)) {
    queueConversationAnnouncement("", data.status === "cancelled" ? " Response stopped." : " Response complete.");
  }
}

function frameSequence(frame) {
  const sequence = Number(frame?.seq);
  return Number.isFinite(sequence) && sequence > 0 ? sequence : Number.POSITIVE_INFINITY;
}

function rememberReplayReady(encoded) {
  if (typeof encoded !== "string" || encoded.length > LIMITS.replayReady || retainedTextBytes(encoded) > LIMITS.replayReady) return false;
  let detail;
  try { detail = JSON.parse(encoded); } catch (_) { return false; }
  const cursor = detail?.cursor;
  const earliest = detail?.earliest_sequence;
  if (!Number.isSafeInteger(cursor) || cursor < 0 || cursor >= Number.MAX_SAFE_INTEGER ||
    !Number.isSafeInteger(earliest) || earliest !== cursor + 1 || typeof detail?.clipped !== "boolean") return false;
  if ((!detail.clipped && cursor !== transcriptState.lastSequence) ||
    (detail.clipped && (cursor === 0 || cursor < transcriptState.lastSequence))) return false;
  transcriptState.replayReady = true;
  transcriptState.replayCursor = cursor;
  transcriptState.replayEarliestSequence = earliest;
  transcriptState.replayClipped = detail.clipped;
  transcriptState.lastSequence = Math.max(transcriptState.lastSequence, cursor);
  // A declared replay cursor is also the minimum safe permission-snapshot
  // watermark. A clipped prefix may contain a resolution that this viewer
  // never saw, so an older GET must not resurrect it.
  transcriptState.permissionAuthoritySequence = Math.max(transcriptState.permissionAuthoritySequence, cursor);
  if (detail.clipped && transcriptState.replayGapEarliestSequence === 0) {
    transcriptState.replayGapCursor = cursor;
    transcriptState.replayGapEarliestSequence = earliest;
  }
  if (detail.clipped) transcriptState.framesClipped = true;
  return true;
}

function activityLimitNoticeText() {
  if (transcriptState.replayGapEarliestSequence > 0) {
    return `Initial bounded replay began at sequence ${transcriptState.replayGapEarliestSequence} after effective cursor ${transcriptState.replayGapCursor}. Earlier Session activity is outside this browser view.`;
  }
  return transcriptState.framesClipped ? "Earlier activity is outside this bounded browser view." : "";
}

function activityLimitNotice(container) {
  const text = activityLimitNoticeText();
  if (!text) return null;
  let notice = container.querySelector("#activity-limit-note");
  if (!notice) {
    notice = document.createElement("div");
    notice.id = "activity-limit-note";
    notice.className = "activity-limit-note";
  }
  notice.textContent = text;
  return notice;
}

function streamErrorNotice(container) {
  if (!transcriptState.streamErrorMessage) return null;
  let notice = container.querySelector("#stream-error-notice");
  if (!notice) {
    notice = document.createElement("div");
    notice.id = "stream-error-notice";
    notice.className = "error-card stream-error-notice";
  }
  notice.textContent = transcriptState.streamErrorMessage;
  return notice;
}

function syncConversationChildren(container, desired) {
  for (let index = 0; index < desired.length; index += 1) {
    if (container.children[index] !== desired[index]) container.insertBefore(desired[index], container.children[index] || null);
  }
  while (container.children.length > desired.length) container.lastElementChild.remove();
}

function orderedTurns() {
  return [...transcriptState.turns].sort((left, right) => left.firstSequence - right.firstSequence);
}

function frameTimestamp(frame) {
  const value = Date.parse(frame?.timestamp || "");
  return Number.isFinite(value) ? value : 0;
}

function recordTurnFrameTime(turn, frame) {
  const value = frameTimestamp(frame);
  if (!value) return;
  if (!turn.firstTimestamp || value < turn.firstTimestamp) turn.firstTimestamp = value;
  if (!turn.lastTimestamp || value > turn.lastTimestamp) turn.lastTimestamp = value;
}

function recordTurnLifecycle(turn, state, frame) {
  state = boundedTranscriptText(state, LIMITS.status).trim().toLowerCase();
  if (!state) return;
  const timestamp = frameTimestamp(frame);
  const previous = turn.lifecycle[turn.lifecycle.length - 1];
  if (previous?.state === state) {
    if (timestamp) previous.timestamp = timestamp;
    return;
  }
  if (turn.lifecycle.length >= 16) turn.lifecycle.shift();
  turn.lifecycle.push({ state, timestamp });
}

function turnRetainedBytes(turn) {
  let total = retainedTextBytes(turn.id, turn.state, turn.stopReason);
  for (const item of turn.lifecycle) total += retainedTextBytes(item.state);
  for (const permissionID of turn.permissionIDs) total += retainedTextBytes(permissionID);
  for (const block of turn.blocks) {
    if (["user", "assistant", "thought"].includes(block.type)) {
      total += block.bytes || 0;
      total += retainedTextBytes(block.messageID);
    }
    if (block.type === "error") {
      total += retainedTextBytes(block.text, block.errorSource, block.errorCode);
    }
    if (block.type === "status") total += retainedTextBytes(block.text, block.tag);
    if (block.type === "tool") {
      total += block.detailBytes || 0;
      total += retainedTextBytes(block.id, block.title, block.kind, block.status, block.metadata, block.preview, block.detailLabel);
    }
  }
  return total;
}

function permissionRetainedBytes(permission) {
  let total = retainedTextBytes(
    permission?.id, permission?.request_id, permission?.title, permission?.kind,
    permission?.state, permission?.decision
  );
  for (const option of permission?.options || []) {
    total += retainedTextBytes(option.id, option.name, option.kind);
  }
  return total;
}

function refreshTurnRetention(turn) {
  const next = turnRetainedBytes(turn);
  transcriptState.retainedBytes += next - (turn.retainedBytes || 0);
  turn.retainedBytes = next;
}

function storePermissionRecord(permission) {
  const previous = transcriptState.permissionSizes.get(permission.id) || 0;
  const next = permissionRetainedBytes(permission);
  transcriptState.permissions.set(permission.id, permission);
  transcriptState.permissionSizes.set(permission.id, next);
  transcriptState.retainedBytes += next - previous;
}

function removeTurn(turn) {
  transcriptState.turns = transcriptState.turns.filter((item) => item !== turn);
  transcriptState.byRequest.delete(turn.id);
  transcriptState.retainedBytes = Math.max(0, transcriptState.retainedBytes - (turn.retainedBytes || 0));
  turn.element?.remove();
  for (const permissionID of [...turn.permissionIDs]) {
    if (transcriptState.permissions.get(permissionID)?.state !== "waiting") removePermissionRecord(permissionID);
  }
  transcriptState.framesClipped = true;
}

function enforceTranscriptRetention(protectedTurn = null) {
  while (transcriptState.retainedBytes > LIMITS.retained) {
    const candidate = orderedTurns().find((item) => item !== protectedTurn);
    if (!candidate) break;
    removeTurn(candidate);
  }
}

function updateTurnRetention(turn) {
  refreshTurnRetention(turn);
  enforceTranscriptRetention(turn);
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

function turnFor(frame) {
  const requestID = boundedTranscriptIdentifier(frame.request_id || frame.data?.request_id, `local:${frame.seq}`);
  let turn = transcriptState.byRequest.get(requestID);
  if (turn) {
    turn.firstSequence = Math.min(turn.firstSequence, frameSequence(frame));
    recordTurnFrameTime(turn, frame);
    return turn;
  }
  turn = {
    id: requestID, firstSequence: frameSequence(frame), state: "", stopReason: "", blocks: [],
    tools: new Map(), error: null, permissionIDs: new Set(),
    assistantBytes: 0, thoughtBytes: 0, toolBytes: 0, retainedBytes: 0, assistantSeen: false, reasoningSeen: false,
    lifecycle: [], firstTimestamp: 0, lastTimestamp: 0,
    lifecycleSeen: false, contentClipped: false, dirty: true, element: null, overviewElement: null
  };
  recordTurnFrameTime(turn, frame);
  transcriptState.byRequest.set(requestID, turn);
  transcriptState.turns.push(turn);
  return turn;
}

function providerTextIsThought(data) {
  const stream = boundedTranscriptText(data?.stream, LIMITS.status).toLowerCase();
  const tag = boundedTranscriptText(data?.tag, LIMITS.status).toLowerCase();
  return ["thought", "reasoning", "analysis"].some((value) => stream.includes(value) || tag.includes(value));
}

function boundedJSON(value, limit = LIMITS.tool) {
  if (value === undefined || value === null) return "";
  let encoded;
  try { encoded = typeof value === "string" ? value : JSON.stringify(value, null, 2); } catch (_) { encoded = String(value); }
  return truncateUTF8(encoded, limit).text;
}

function rememberDiagnosticFrame(frame) {
  let encoded;
  try { encoded = JSON.stringify(frame); } catch (_) { encoded = ""; }
  let value = frame;
  let bytes = textEncoder.encode(encoded).length;
  if (!encoded || bytes > LIMITS.diagnosticBytes) {
    const sample = truncateUTF8(encoded || String(frame), LIMITS.diagnosticSample);
    value = {
      session_id: frame?.session_id, seq: frame?.seq, request_id: frame?.request_id,
      type: frame?.type, timestamp: frame?.timestamp, cursor: frame?.cursor,
      data: { diagnostic_clipped: true, raw_prefix: sample.text }
    };
    bytes = textEncoder.encode(JSON.stringify(value)).length;
    transcriptState.diagnosticsClipped = true;
  }
  while (transcriptState.diagnostics.length >= LIMITS.diagnostics ||
    (transcriptState.diagnostics.length > 0 && transcriptState.diagnosticBytes + bytes > LIMITS.diagnosticBytes)) {
    transcriptState.diagnostics.shift();
    transcriptState.diagnosticBytes = Math.max(0, transcriptState.diagnosticBytes - (transcriptState.diagnosticSizes.shift() || 0));
    transcriptState.diagnosticsClipped = true;
  }
  transcriptState.diagnostics.push(value);
  transcriptState.diagnosticSizes.push(bytes);
  transcriptState.diagnosticBytes += bytes;
}

function upsertUserFrame(turn, data) {
  let block = turn.blocks.find((item) => item.type === "user");
  if (!block) {
    block = { type: "user", text: "", bytes: 0, clipped: false };
    if (turn.blocks.length >= LIMITS.blocksPerTurn) {
      turn.contentClipped = true;
      return;
    }
    turn.blocks.unshift(block);
  }
  const value = truncateUTF8(data.text || "", LIMITS.assistant);
  block.text = value.text;
  block.bytes = value.bytes;
  block.clipped = value.clipped;
  touchBlock(block);
  turn.dirty = true;
}

function appendProviderText(turn, frame, data) {
  const thought = providerTextIsThought(data);
  const type = thought ? "thought" : "assistant";
  const byteField = thought ? "thoughtBytes" : "assistantBytes";
  const limit = thought ? LIMITS.thought : LIMITS.assistant;
  if (String(data.text || "").trim()) {
    if (thought) turn.reasoningSeen = true;
    else turn.assistantSeen = true;
  }
  const incoming = truncateUTF8Tail(data.text || "", limit);
  makeRollingTextRoom(turn, type, byteField, incoming.bytes, limit);
  const previous = turn.blocks[turn.blocks.length - 1];
  const messageID = boundedTranscriptIdentifier(data.messageId || data.message_id || data.tag);
  const compatible = previous?.type === type && (type === "thought" || previous.messageID === messageID || !previous.messageID || !messageID);
  const block = compatible ? previous : {
    type, text: "", bytes: 0, clipped: false, leadingClipped: false, trailingClipped: false,
    messageID, key: frame.seq
  };
  if (!compatible && (!makeTextBlockSlot(turn) || !appendTurnBlock(turn, block))) return;
  block.text += incoming.text;
  block.bytes += incoming.bytes;
  turn[byteField] += incoming.bytes;
  block.leadingClipped = block.leadingClipped || incoming.clipped;
  block.trailingClipped = block.trailingClipped || Boolean(data.truncated);
  turn.contentClipped = turn.contentClipped || incoming.clipped || Boolean(data.truncated);
  touchBlock(block);
  turn.dirty = true;
}

function upsertToolFrame(turn, frame, data) {
  const id = boundedTranscriptIdentifier(data.toolCallId || data.tool_call_id || data.id, `tool:${frame.seq}`);
  let block = turn.tools.get(id);
  if (!block) {
    if (turn.tools.size >= LIMITS.toolsPerTurn) {
      turn.contentClipped = true;
      turn.dirty = true;
      return;
    }
    block = { type: "tool", id, title: "", kind: "", status: "", metadata: "", preview: "", detail: "", detailBytes: 0, clipped: false, sensitive: false };
    if (!appendTurnBlock(turn, block)) return;
    turn.tools.set(id, block);
  }
  block.title = boundedTranscriptText(data.title || data.name, LIMITS.title) || block.title || "Tool call";
  block.kind = boundedTranscriptText(data.kind, LIMITS.kind) || block.kind;
  block.status = boundedTranscriptText(data.status, LIMITS.status) || block.status || "updated";
  const metadata = data.tag ?? data.metadata;
  if (metadata !== undefined && metadata !== null) {
    block.metadata = typeof metadata === "string"
      ? boundedTranscriptText(metadata, LIMITS.metadata)
      : boundedJSON(metadata, LIMITS.metadata);
  }
  const rawDetail = data.content ?? data.rawOutput ?? data.output ?? data.result;
  if (rawDetail !== undefined) {
    const available = Math.min(LIMITS.tool, Math.max(0, LIMITS.toolTotal - turn.toolBytes + block.detailBytes));
    const detail = truncateUTF8(boundedJSON(rawDetail, available), available);
    turn.toolBytes = turn.toolBytes - block.detailBytes + detail.bytes;
    block.detail = detail.text;
    block.detailBytes = detail.bytes;
    block.clipped = detail.clipped;
  }
  block.sensitive = block.sensitive || Boolean(data.sensitive);
  touchBlock(block);
  turn.dirty = true;
}

function appendProviderStatus(turn, frame, data) {
  const text = boundedProviderOptionText(data.text || data.status || data.state, 2048);
  if (!text) return;
  appendTurnBlock(turn, { type: "status", text, tag: boundedTranscriptText(data.tag, LIMITS.status), key: frame.seq });
}

function upsertTurnError(turn, frame, data, fallback) {
  if (!turn.error) {
    turn.error = { type: "error", text: "", errorSource: "", errorCode: "", retryable: null };
    if (!appendTurnBlock(turn, turn.error)) {
      turn.error = null;
      return;
    }
  }
  turn.error.text = boundedTranscriptText(data.message || fallback, LIMITS.assistant);
  turn.error.errorSource = boundedTranscriptText(data.source, LIMITS.errorProvenance);
  turn.error.errorCode = boundedTranscriptText(data.code, LIMITS.errorProvenance);
  turn.error.retryable = typeof data.retryable === "boolean" ? data.retryable : null;
  touchBlock(turn.error);
  turn.state = "error";
  recordTurnLifecycle(turn, "error", frame);
  turn.lifecycleSeen = true;
  turn.dirty = true;
  reconcileRuntimeStatusSoon();
}

function normalizePermissionOptions(options) {
  if (!Array.isArray(options)) return [];
  if (options.length > LIMITS.permissionOptions) transcriptState.framesClipped = true;
  const normalized = [];
  const seen = new Set();
  for (const option of options.slice(0, LIMITS.permissionOptions)) {
    const id = exactPermissionIdentifier(option?.id, LIMITS.identifier);
    if (!id || seen.has(id)) {
      transcriptState.framesClipped = true;
      continue;
    }
    seen.add(id);
    normalized.push({
      id,
      name: boundedTranscriptText(option?.name, LIMITS.title),
      kind: boundedTranscriptText(option?.kind, LIMITS.kind)
    });
  }
  return normalized;
}

function normalizePermissionRecord(value, forcedState = "") {
  const id = exactPermissionIdentifier(value?.id, LIMITS.permissionIdentifier);
  if (!id) {
    transcriptState.framesClipped = true;
    return null;
  }
  const result = {
    id,
    request_id: boundedTranscriptIdentifier(value?.request_id || value?.requestID),
    title: boundedTranscriptText(value?.title, LIMITS.title),
    kind: boundedTranscriptText(value?.kind, LIMITS.kind),
    state: boundedTranscriptText(forcedState || value?.state, LIMITS.status) || "waiting",
    decision: boundedTranscriptText(value?.decision ?? value?.reason, LIMITS.permissionDecision)
  };
  if (Array.isArray(value?.options)) result.options = normalizePermissionOptions(value.options);
  return result;
}

function setTurnStateFromFrame(turn, frame, state) {
  state = boundedTranscriptText(state, LIMITS.status).trim().toLowerCase();
  const normalized = state === "completed" ? "complete" : state;
  turn.state = normalized || turn.state;
  turn.stopReason = boundedTranscriptText(
    frame.data?.stop_reason || frame.data?.stopReason || turn.stopReason,
    LIMITS.stopReason
  );
  recordTurnLifecycle(turn, turn.state, frame);
  turn.lifecycleSeen = true;
  turn.dirty = true;
  reconcileRuntimeStatusSoon();
}

function applyFrameData(frame, turn) {
  const data = frame.data || {};
  if (frame.type === "client_command") {
    if (data.command === "prompt") {
      upsertUserFrame(turn, data);
      setTurnStateFromFrame(turn, frame, "working");
    } else if (data.command === "cancel") {
      setTurnStateFromFrame(turn, frame, "cancelling");
    }
    return;
  }
  if (frame.type === "provider_event") {
    if (data.type === "text_delta") appendProviderText(turn, frame, data);
    else if (data.type === "tool_call") upsertToolFrame(turn, frame, data);
    else if (data.type === "status") appendProviderStatus(turn, frame, data);
    else if (data.type === "error") upsertTurnError(turn, frame, data, "The provider reported an error.");
    return;
  }
  if (frame.type === "provider_result") {
    if (data.status === "failed") upsertTurnError(turn, frame, data, "The provider turn failed.");
    else setTurnStateFromFrame(turn, frame, data.status || "complete");
    return;
  }
  if (frame.type === "worker_error") upsertTurnError(turn, frame, data, "The Session worker failed.");
}

function rememberPermission(frame, turn) {
  const data = frame.data || {};
  if (!["permission_requested", "permission_resolved", "permission_abandoned"].includes(frame.type)) return;
  transcriptState.permissionAuthoritySequence = Math.max(transcriptState.permissionAuthoritySequence, Number(frame.seq) || 0);
  let permission;
  if (frame.type === "permission_requested") permission = normalizePermissionRecord(data, "waiting");
  else if (frame.type === "permission_resolved") {
    permission = normalizePermissionRecord({ id: data.permission_id, decision: data.decision }, "resolved");
  } else if (frame.type === "permission_abandoned") {
    permission = normalizePermissionRecord({ id: data.permission_id, reason: data.reason }, "abandoned");
  }
  if (!permission) return;
  const existing = transcriptState.permissions.get(permission.id) || {};
  const merged = { ...existing };
  for (const [key, value] of Object.entries(permission)) {
    if (value !== "" || ["id", "state", "options"].includes(key)) merged[key] = value;
  }
  if ((!permission.options || permission.options.length === 0) && existing.options) merged.options = existing.options;
  if (!ensurePermissionCapacity(permission.id)) return;
  storePermissionRecord(merged);
  turn.permissionIDs.add(permission.id);
  turn.dirty = true;
  enforcePermissionBound();
}

function flagPermissionOverflow() {
  transcriptState.permissionOverflow = true;
  invalidateActiveSessionSource();
  const state = document.querySelector("#stream-state");
  state?.classList.remove("connected");
  const label = state?.querySelector("span");
  if (label) label.textContent = "Approval limit";
}

function removePermissionRecord(id) {
  const bytes = transcriptState.permissionSizes.get(id) || 0;
  transcriptState.permissions.delete(id);
  transcriptState.permissionSizes.delete(id);
  transcriptState.retainedBytes = Math.max(0, transcriptState.retainedBytes - bytes);
  for (const item of transcriptState.turns) {
    if (!item.permissionIDs.delete(id)) continue;
    refreshTurnRetention(item);
    item.dirty = true;
  }
  transcriptState.framesClipped = true;
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

function applyFrame(frame) {
  const sequence = Number(frame.seq) || 0;
  if (sequence > 0 && sequence <= transcriptState.lastSequence) return false;
  if (transcriptState.lastSequence === 0 && sequence > 1) transcriptState.framesClipped = true;
  transcriptState.lastSequence = Math.max(transcriptState.lastSequence, sequence);
  rememberDiagnosticFrame(frame);
  if (frame.type === "connection") {
    setProviderConnectionState(frame.data?.state || "");
    applyQueuedPermissionSnapshot();
    return true;
  }
  const turnTypes = ["client_command", "provider_event", "provider_result", "permission_requested", "permission_resolved", "permission_abandoned", "worker_error"];
  if (!turnTypes.includes(frame.type)) {
    applyQueuedPermissionSnapshot();
    return true;
  }
  const dataType = frame.data?.type;
  if (frame.type === "provider_event" && !["text_delta", "tool_call", "status", "error"].includes(dataType)) {
    applyQueuedPermissionSnapshot();
    return true;
  }
  if (frame.type === "client_command" && !["prompt", "cancel"].includes(frame.data?.command)) {
    applyQueuedPermissionSnapshot();
    return true;
  }
  const turn = turnFor(frame);
  rememberPermission(frame, turn);
  applyFrameData(frame, turn);
  updateActionVisibility(currentSessionRuntimeStatus);
  updateTurnRetention(turn);
  trimTurns();
  applyQueuedPermissionSnapshot();
  return true;
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
  phase.textContent = context.final ? "Final response" : context.streaming ? "Working" : "Earlier update";
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
    note.textContent = "Earlier output was omitted to keep this view responsive; the newest output is preserved.";
  } else if (block.trailingClipped) {
    note.textContent = "The agent marked this output as truncated.";
  } else {
    note.textContent = "Display clipped to keep this browser view responsive.";
  }
  return note;
}

function thoughtElement(block) {
  const details = document.createElement("details");
  details.className = "thought-card";
  details.dataset.detailKey = "reasoning";
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
  state.textContent = "Agent";
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

function providerStatusElement(block) {
  const status = document.createElement("div");
  status.className = "provider-status";
  const label = document.createElement("strong");
  label.textContent = block.tag ? block.tag.replaceAll("_", " ") : "Provider status";
  const text = document.createElement("span");
  text.textContent = block.text;
  status.append(label, text);
  return status;
}

function renderBlockElement(block, assistantName, context = {}) {
  if (block.type === "user" || block.type === "assistant") return messageElement(block, assistantName, context);
  if (block.type === "thought") return thoughtElement(block);
  if (block.type === "tool") return toolElement(block);
  if (block.type === "status") return providerStatusElement(block);
  if (block.type === "error") {
    const error = document.createElement("div");
    error.className = "error-card";
    error.textContent = block.text;
    if (block.errorSource || block.errorCode || block.retryable !== null) {
      const detail = document.createElement("small");
      detail.className = "error-provenance";
      const fields = [block.errorSource, block.errorCode].filter(Boolean).map((value) => String(value).replaceAll("_", " "));
      if (block.retryable !== null) fields.push(block.retryable ? "retryable" : "not retryable");
      detail.textContent = fields.join(" · ");
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

function turnStatusLabel(turn) {
  if (turnHasWaitingPermission(turn)) return "Approval required";
  return ({
    ready: "Accepted", working: "Working", cancelling: "Stopping", cancelled: "Stopped",
    incomplete: "Incomplete", interrupted: "Interrupted", error: "Failed", failed: "Failed", complete: "Complete"
  })[turn.state] || "Pending";
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
    ready: "Accepted", working: "Working", cancelling: "Stopping", cancelled: "Stopped",
    incomplete: "Incomplete", interrupted: "Interrupted", error: "Failed", failed: "Failed", complete: "Complete"
  });
  const states = (turn.lifecycle || []).map((item) => labels[item.state] || "").filter(Boolean);
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
    overview._turnLabel = document.createElement("strong");
    overview._statusLabel = document.createElement("span");
    overview._durationLabel = document.createElement("span");
    overview._toolsLabel = document.createElement("span");
    overview._reasoningLabel = document.createElement("span");
    main.append(overview._turnLabel, overview._statusLabel, overview._durationLabel, overview._toolsLabel, overview._reasoningLabel);
    overview._lifecycleLabel = document.createElement("div");
    overview._lifecycleLabel.className = "turn-overview-lifecycle";
    overview.append(main, overview._lifecycleLabel);
    turn.overviewElement = overview;
  }
  overview.className = "turn-overview";
  setTextIfChanged(overview._turnLabel, "Agent turn");
  overview._statusLabel.className = `turn-overview-status status-${turnStatusTone(turn)}`;
  setTextIfChanged(overview._statusLabel, turnStatusLabel(turn));
  const duration = turnDurationLabel(turn);
  overview._durationLabel.hidden = !duration;
  setTextIfChanged(overview._durationLabel, duration);
  const toolCount = turn.tools?.size || 0;
  setTextIfChanged(overview._toolsLabel, toolCount ? `${toolCount} ${toolCount === 1 ? "tool" : "tools"}` : "No tools");
  setTextIfChanged(overview._reasoningLabel, turnHasReasoning(turn) ? "Reasoning available" : "Reasoning not provided");
  setTextIfChanged(overview._lifecycleLabel, turnLifecycleLabel(turn));
  overview.setAttribute("aria-label", `Agent turn. ${turnStatusLabel(turn)}. ${accessibleDurationLabel(duration)}${overview._toolsLabel.textContent}. ${overview._reasoningLabel.textContent}. ${overview._lifecycleLabel.textContent}.`);
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
  if (["working", "cancelling", "cancelled"].includes(turn.state)) {
    const state = document.createElement("div");
    state.className = "turn-state-line";
    const dot = document.createElement("i");
    const copy = ({ working: "Working…", cancelling: "Stopping response…", cancelled: "Response stopped" })[turn.state];
    state.append(dot, document.createTextNode(copy));
    tail.append(state);
  }
  if (turn.state === "complete") {
    const retainedResponse = completedResponseBlock(turn);
    const retainedReasoning = turn.blocks.some((block) => block.type === "thought" && block.text.trim());
    if (turnHasReasoning(turn) && !retainedReasoning) {
      const unavailable = document.createElement("div");
      unavailable.className = "reasoning-unavailable";
      unavailable.textContent = "Earlier reasoning is outside this bounded browser view.";
      tail.append(unavailable);
    }
    if (!turn.assistantSeen) {
      const missing = document.createElement("div");
      missing.className = "response-missing";
      missing.textContent = "The turn completed without an assistant response in this activity stream. Check Diagnostics for more detail.";
      tail.append(missing);
    } else if (!retainedResponse) {
      const missing = document.createElement("div");
      missing.className = "response-missing";
      missing.textContent = "Assistant output is outside this bounded view; check Diagnostics for more detail.";
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

const selectionScopeSelectors = [".message-body", ".thought-content", ".tool-detail", ".tool-inline-preview", ".error-card"];

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
  const contextKey = `${context.streaming ? "streaming" : "recorded"}:${context.final ? "final" : "update"}`;
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
  // Prefer a content block over its containing turn. Incoming frames can
  // insert or replace content in the same turn; anchoring only the article
  // would let a visible block jump. Keeping the block object lets the anchor
  // follow its replacement element.
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
  const priorTop = scroller.scrollTop;
  const scrollAnchor = captureConversationScrollAnchor(scroller, container);
  const assistantName = document.querySelector(".session-title h1")?.firstChild?.textContent?.trim() || "Agent";
  if (transcriptState.turns.length === 0) {
    let empty = container.querySelector(".conversation-empty");
    if (!empty) {
      empty = document.createElement("div");
      empty.className = "conversation-empty";
      const icon = document.createElement("span");
      icon.className = "empty-orbit";
      icon.textContent = "◎";
      const heading = document.createElement("h2");
      heading.textContent = "Start the conversation";
      const copy = document.createElement("p");
      copy.textContent = "Send a message below. Agent activity appears here as it arrives.";
      empty.append(icon, heading, copy);
    }
    const desired = [];
    const streamError = streamErrorNotice(container);
    const notice = activityLimitNotice(container);
    if (streamError) desired.push(streamError);
    if (notice) desired.push(notice);
    desired.push(empty);
    syncConversationChildren(container, desired);
  } else {
    const desired = [];
    const streamError = streamErrorNotice(container);
    const notice = activityLimitNotice(container);
    if (streamError) desired.push(streamError);
    if (notice) desired.push(notice);
    for (const turn of orderedTurns()) {
      if (!turn.element || turn.dirty) patchTurnElement(turn, assistantName);
      desired.push(turn.element);
    }
    syncConversationChildren(container, desired);
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
  if (copy) copy.textContent = `${option.name || option.kind} may apply to later tool requests in this Session.`;
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
    await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/permissions/response`, "POST", {
      permission_id: permission.id, option_id: option.id, cancelled: false
    });
    storePermissionRecord({ ...permission, state: "resolved", decision: boundedTranscriptText(option.kind, LIMITS.permissionDecision) });
    markPermissionDirty(permission.id);
    enforcePermissionBound();
    enforceTranscriptRetention();
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
  const connectionEpoch = providerConnectionEpoch;
  const streamEpoch = sessionStreamBoundaryEpoch;
  const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/permissions`);
  if (connectionEpoch !== providerConnectionEpoch || streamEpoch !== sessionStreamBoundaryEpoch) return false;
  return receivePermissionSnapshot(result);
}

function receivePermissionSnapshot(result) {
  result = result && typeof result === "object" ? result : {};
  const sequence = Number(result.sequence);
  if (!Number.isSafeInteger(sequence) || sequence < 0) return false;
  const permissions = Array.isArray(result.permissions) ? result.permissions.slice(0, LIMITS.permissions + 1) : [];
  const snapshot = {
    sequence,
    permissions,
    ready: Boolean(result.decisions_ready),
    blockedCode: boundedTranscriptText(result.blocked_code, LIMITS.errorProvenance),
    blockedMessage: boundedTranscriptText(result.blocked_message, 2048)
  };
  // Sequence zero is an offline/upgrade/readiness observation, not an
  // authoritative permission-list watermark. Preserve any durable cards and
  // only disable their controls until a current v7 worker can answer.
  if (sequence === 0) return applyPermissionSnapshotReadiness(snapshot);
  if (sequence < transcriptState.permissionAuthoritySequence) return false;
  if (sequence > transcriptState.lastSequence) {
    const pending = transcriptState.pendingPermissionSnapshot;
    if (!pending || sequence >= pending.sequence) transcriptState.pendingPermissionSnapshot = snapshot;
    return false;
  }
  transcriptState.pendingPermissionSnapshot = null;
  return applyPermissionSnapshot(snapshot);
}

function applyPermissionSnapshotReadiness(snapshot) {
  const changed = snapshot.ready !== permissionDecisionsReady ||
    snapshot.blockedCode !== permissionDecisionBlockedCode ||
    snapshot.blockedMessage !== permissionDecisionBlockedMessage;
  permissionDecisionsReady = snapshot.ready;
  permissionDecisionBlockedCode = snapshot.blockedCode;
  permissionDecisionBlockedMessage = snapshot.blockedMessage;
  if (changed) renderedPermissionSignature = "";
  return changed;
}

function applyPermissionSnapshot(snapshot) {
  if (snapshot.sequence < transcriptState.permissionAuthoritySequence) return false;
  transcriptState.permissionAuthoritySequence = Math.max(transcriptState.permissionAuthoritySequence, snapshot.sequence);
  const readinessChanged = applyPermissionSnapshotReadiness(snapshot);
  return reconcilePermissionSnapshot(snapshot.permissions) || readinessChanged;
}

function applyQueuedPermissionSnapshot() {
  const snapshot = transcriptState.pendingPermissionSnapshot;
  if (!snapshot || transcriptState.lastSequence < snapshot.sequence) return false;
  transcriptState.pendingPermissionSnapshot = null;
  return applyPermissionSnapshot(snapshot);
}

function reconcilePermissionSnapshot(received) {
  if (!Array.isArray(received)) received = [];
  if (received.length > LIMITS.permissions) {
    flagPermissionOverflow();
    return true;
  }
  const normalized = received.map((permission) => normalizePermissionRecord(permission, "waiting")).filter(Boolean);
  if (normalized.length !== received.length) return true;
  const waiting = new Set(normalized.map((permission) => permission.id));
  let changed = false;
  // The GET is authoritative. Retire disappeared waiting records before
  // admitting replacements so an exact-cap snapshot can swap IDs safely.
  for (const [id, permission] of transcriptState.permissions) {
    if (permission.state === "waiting" && !waiting.has(id)) {
      storePermissionRecord({ ...permission, state: "resolved" });
      markPermissionDirty(id);
      changed = true;
    }
  }
  for (const permission of normalized) {
    if (!ensurePermissionCapacity(permission.id)) continue;
    const permissionChanged = JSON.stringify(transcriptState.permissions.get(permission.id)) !== JSON.stringify(permission);
    if (permissionChanged) changed = true;
    storePermissionRecord(permission);
    if (permissionChanged) markPermissionDirty(permission.id);
  }
  enforcePermissionBound();
  enforceTranscriptRetention();
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
  if (transcriptState.diagnosticsClipped) {
    const clipped = document.createElement("p");
    clipped.className = "activity-limit-note";
    clipped.textContent = "Earlier or oversized diagnostic detail is outside this bounded browser view.";
    fragment.append(clipped);
  } else if (transcriptState.diagnostics.length === 0) {
    const empty = document.createElement("p");
    empty.className = "muted";
    empty.textContent = "Waiting for activity…";
    fragment.append(empty);
  }
  for (const frame of transcriptState.diagnostics) {
    const row = document.createElement("article");
    row.className = "diagnostic-row";
    const meta = document.createElement("div");
    meta.className = "diagnostic-meta";
    meta.textContent = `#${frame.seq}\n${frame.type}`;
    const data = document.createElement("pre");
    data.textContent = JSON.stringify(frame, null, 2);
    row.append(meta, data);
    fragment.append(row);
  }
  feed.replaceChildren(fragment);
}

let activeEventSource = null;
let sessionReplayResetAttempted = false;
let sessionStreamEpoch = 0;
let sessionStreamBoundaryEpoch = 0;
let sessionStreamReady = false;

function isActiveSessionSource(source, epoch) {
  return activeEventSource === source && sessionStreamEpoch === epoch;
}

function setSessionStreamReady(source, epoch, ready) {
  if (!isActiveSessionSource(source, epoch)) return false;
  if (sessionStreamReady !== ready) {
    sessionStreamReady = ready;
    sessionStreamBoundaryEpoch += 1;
  }
  if (!ready) streamAnnouncementsReady = false;
  syncProviderDependentControls();
  return true;
}

function closeActiveSessionSource(source, epoch) {
  if (!isActiveSessionSource(source, epoch)) return false;
  source.close();
  activeEventSource = null;
  sessionStreamEpoch += 1;
  sessionStreamReady = false;
  sessionStreamBoundaryEpoch += 1;
  streamAnnouncementsReady = false;
  syncProviderDependentControls();
  return true;
}

function invalidateActiveSessionSource() {
  activeEventSource?.close();
  activeEventSource = null;
  sessionStreamEpoch += 1;
  sessionStreamReady = false;
  sessionStreamBoundaryEpoch += 1;
  streamAnnouncementsReady = false;
  syncProviderDependentControls();
}

function attachSessionChannel(path) {
  invalidateActiveSessionSource();
  if (!window.EventSource) return;
  const state = document.querySelector("#stream-state");
  // `seed` applies only to this EventSource's first request. On automatic
  // reconnect the server prefers EventSource's advancing Last-Event-ID, so a
  // stale URL seed can never conflict with the live cursor.
  const separator = path.includes("?") ? "&" : "?";
  const streamPath = transcriptState.lastSequence > 0 ? `${path}${separator}seed=${transcriptState.lastSequence}` : path;
  const source = new EventSource(streamPath);
  activeEventSource = source;
  const sourceEpoch = sessionStreamEpoch;
  source.onopen = () => {
    if (!isActiveSessionSource(source, sourceEpoch)) return;
    state?.classList.add("connected");
    const label = state?.querySelector("span");
    if (label) label.textContent = sessionStreamReady ? "Live" : "Synchronizing";
  };
  source.addEventListener("ready", (event) => {
    if (!isActiveSessionSource(source, sourceEpoch)) return;
    const accepted = rememberReplayReady(event?.data || "");
    if (!accepted) {
      setSessionStreamReady(source, sourceEpoch, false);
      return;
    }
    setSessionStreamReady(source, sourceEpoch, true);
    transcriptState.streamErrorCode = "";
    transcriptState.streamErrorMessage = "";
    const label = state?.querySelector("span");
    if (label) label.textContent = "Live";
    const sessionID = document.body.dataset.sessionId;
    if (sessionID) refreshPermissions().then((changed) => { if (changed) scheduleTranscriptRender(); }).catch(() => {});
    applyQueuedPermissionSnapshot();
    streamAnnouncementsReady = true;
    renderConversation();
    scheduleProviderOptionsAfterConnection();
  });
  source.addEventListener("frame", (event) => {
    if (!isActiveSessionSource(source, sourceEpoch) || !sessionStreamReady) return;
    try {
      const frame = JSON.parse(event.data);
      noteStreamFrame(frame);
      sessionReplayResetAttempted = false;
      if (applyFrame(frame)) scheduleTranscriptRender();
    } catch (_) {
      state?.classList.remove("connected");
      const label = state?.querySelector("span");
      if (label) label.textContent = "Invalid update";
    }
  });
  source.addEventListener("channel_error", (event) => {
    if (!isActiveSessionSource(source, sourceEpoch)) return;
    let detail = {};
    try { detail = JSON.parse(event.data || "{}"); } catch (_) {}
    closeActiveSessionSource(source, sourceEpoch);
    state?.classList.remove("connected");
    const label = state?.querySelector("span");
    if (detail.code === "replay_cursor_reset" && detail.reset_cursor && !sessionReplayResetAttempted) {
      sessionReplayResetAttempted = true;
      transcriptState = newTranscriptState();
      transcriptState.framesClipped = true;
      if (label) label.textContent = "Resetting replay";
      renderConversation();
      const resetEpoch = sessionStreamEpoch;
      window.setTimeout(() => {
        if (activeEventSource !== null || sessionStreamEpoch !== resetEpoch) return;
        attachSessionChannel(path);
      }, 50);
      return;
    }
    transcriptState.streamErrorCode = boundedTranscriptText(detail.code, LIMITS.errorProvenance);
    transcriptState.streamErrorMessage = boundedTranscriptText(detail.message, 2048) ||
      (detail.code === "replay_unavailable"
        ? "Saved Session activity is unavailable. Resume this Session to establish a new channel."
        : "The Session activity stream is unavailable.");
    renderConversation();
    if (label) {
      label.textContent = detail.code === "replay_cursor_reset" ? "History gap" :
        (detail.code === "replay_unavailable" ? "Replay unavailable" : "Stream unavailable");
    }
  });
  source.onerror = () => {
    if (!isActiveSessionSource(source, sourceEpoch)) return;
    setSessionStreamReady(source, sourceEpoch, false);
    state?.classList.remove("connected");
    const label = state?.querySelector("span");
    if (label) label.textContent = "Reconnecting";
  };
  window.setTimeout(() => {
    if (isActiveSessionSource(source, sourceEpoch) && transcriptState.turns.length === 0) renderConversation();
  }, 450);
  window.addEventListener("beforeunload", () => {
    if (isActiveSessionSource(source, sourceEpoch)) closeActiveSessionSource(source, sourceEpoch);
  }, { once: true });
}

function hasLocallyPreparingTurn() {
  return transcriptState.turns.some((turn) => ["ready", "working", "cancelling"].includes(turn.state));
}

function sessionActionVisible(action, visibleStatuses, status) {
	if (action === "resume" && !currentSessionResumable) return false;
  return visibleStatuses.includes(status) || (action === "cancel" && hasLocallyPreparingTurn());
}

function updateActionVisibility(status) {
  document.body.dataset.sessionStatus = status;
  for (const action of document.querySelectorAll("[data-visible-status]")) {
    const visible = sessionActionVisible(action.dataset.action || "", action.dataset.visibleStatus.split(","), status);
    action.dataset.statusHidden = visible ? "false" : "true";
  }
}

let currentSessionRuntimeStatus = document.body.dataset.sessionStatus || "";
let currentSessionResumable = document.body.dataset.sessionResumable === "true";
let currentProviderConnectionState = currentSessionRuntimeStatus === "starting"
  ? "connecting"
  : (["waiting", "working"].includes(currentSessionRuntimeStatus) ? "connected" : "disconnected");
let providerConnectionEpoch = 0;
let runtimeStatusEpoch = 0;
let providerOptionsLoadTimer = 0;
let providerOptionsRequestEpoch = 0;

function providerCommandBoundaryReady() {
  return Boolean(activeEventSource) && sessionStreamReady && currentProviderConnectionState === "connected";
}

function providerOptionsCanLoad() {
  return providerCommandBoundaryReady() && currentSessionRuntimeStatus === "waiting";
}

function setComposerConnectionReady(ready) {
  const form = document.querySelector("#prompt-form");
  const textarea = form?.querySelector("textarea");
  const submit = form?.querySelector('button[type="submit"]');
  if (textarea) textarea.disabled = !ready;
  if (submit) submit.disabled = !ready || form?.dataset.submitting === "true";
  const hint = document.querySelector("#composer-hint");
  if (hint) hint.textContent = ready ? "Enter to send · Shift+Enter for a new line" : "Synchronizing the Session channel…";
}

function setProviderOptionsConnectionReady(ready) {
  const form = document.querySelector("#provider-config-form");
  if (!form) return;
  const panel = form.querySelector("[data-provider-options]");
  ready = ready && panel?.getAttribute("aria-busy") !== "true";
  let available = false;
  for (const name of Object.keys(providerOptionSpecs)) {
    const select = form.querySelector(`[data-provider-option="${name}"]`);
    const supported = select?.dataset.providerSupported === "true";
    if (select) select.disabled = !ready || !supported;
    available ||= supported;
  }
  const submit = form.querySelector("button[type=submit]");
  if (submit) submit.disabled = !ready || !available || form.dataset.submitting === "true";
}

function syncProviderDependentControls() {
  setComposerConnectionReady(providerCommandBoundaryReady());
  setProviderOptionsConnectionReady(providerOptionsCanLoad());
}

function scheduleProviderOptionsAfterConnection() {
  window.clearTimeout?.(providerOptionsLoadTimer);
  providerOptionsLoadTimer = window.setTimeout(() => {
    if (!providerOptionsCanLoad()) return;
    const sessionID = document.body.dataset.sessionId;
    if (sessionID) loadProviderOptions(sessionID).catch(() => {});
  }, 25);
}

function setProviderConnectionState(state) {
  state = boundedTranscriptText(state, LIMITS.status).trim().toLowerCase();
  if (!state) return;
  const changed = state !== currentProviderConnectionState;
  if (changed) providerConnectionEpoch += 1;
  currentProviderConnectionState = state;
  if (state === "connecting") {
    window.clearTimeout?.(providerOptionsLoadTimer);
    syncProviderDependentControls();
    setRuntimeStatus("starting");
    return;
  }
  if (state === "connected") {
    if (currentSessionRuntimeStatus === "starting") setRuntimeStatus("waiting");
    syncProviderDependentControls();
    scheduleProviderOptionsAfterConnection();
    const sessionID = document.body.dataset.sessionId;
    if (changed && sessionID) refreshPermissions().then((updated) => { if (updated) scheduleTranscriptRender(); }).catch(() => {});
    return;
  }
  window.clearTimeout?.(providerOptionsLoadTimer);
  syncProviderDependentControls();
  if (["failed", "stopped", "cancelled"].includes(state)) {
    invalidateActiveSessionSource();
    setRuntimeStatus(state);
    const streamState = document.querySelector("#stream-state");
    streamState?.classList.remove("connected");
    const label = streamState?.querySelector("span");
    if (label) label.textContent = statusLabel(state);
  }
  const sessionID = document.body.dataset.sessionId;
  if (changed && sessionID) refreshPermissions().then((updated) => { if (updated) scheduleTranscriptRender(); }).catch(() => {});
}

function setRuntimeStatus(status) {
  status = boundedTranscriptText(status, LIMITS.status).trim().toLowerCase();
  if (!status) return;
  const changed = status !== currentSessionRuntimeStatus;
  if (changed) runtimeStatusEpoch += 1;
  currentSessionRuntimeStatus = status;
  renderSessionStatus();
  updateActionVisibility(status);
  syncProviderDependentControls();
  if (changed && providerOptionsCanLoad()) scheduleProviderOptionsAfterConnection();
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
	currentSessionResumable = current.resumable === true;
  setRuntimeStatus(current.status);
  if (current.status === "starting") setProviderConnectionState("connecting");
  else if (["failed", "stopped", "cancelled"].includes(current.status)) setProviderConnectionState(current.status);
  const navDot = findNavSession(sessionID)?.querySelector(".status-dot");
  if (navDot) {
    const display = sessionDisplayStatus(current);
    navDot.className = `status-dot status-${display.tone}`;
    navDot.setAttribute("aria-label", display.label);
  }
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
    if (!providerCommandBoundaryReady()) {
      setNotice(notice, "Wait for the Session channel to finish synchronizing.", true);
      syncProviderDependentControls();
      return;
    }
    if (!beginFormSubmission(form)) return;
    const message = textarea.value;
    if (!message.trim()) {
      endFormSubmission(form);
      return;
    }
    const submit = form.querySelector("button[type=submit]");
    submit.disabled = true;
    setNotice(notice, "Sending…");
    try {
      const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/prompt`, "POST", { message });
      const requestID = boundedTranscriptIdentifier(result.request_id, "local:pending");
      const turn = transcriptState.byRequest.get(requestID) || turnFor({ request_id: result.request_id, seq: Number.POSITIVE_INFINITY, data: {} });
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
      setNotice(notice, "Message accepted.");
      transcriptState.shouldStick = true;
      scheduleTranscriptRender();
    } catch (error) {
      setNotice(notice, error.message, true);
    } finally {
      endFormSubmission(form);
      syncProviderDependentControls();
      if (!textarea.disabled) textarea.focus();
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
        if (action === "resume") {
          setProviderConnectionState("connecting");
          attachSessionChannel(`/api/v1/sessions/${encodeURIComponent(sessionID)}/stream`);
        }
      } catch (error) {
        setNotice(notice, error.message, true);
      } finally {
        button.disabled = false;
      }
    });
  }

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
  attachSessionChannel(`/api/v1/sessions/${encodeURIComponent(sessionID)}/stream`);
  bindComposer(sessionID);
  bindSessionControls(sessionID);
  bindProviderConfig(sessionID);
  bindChildSessionCreation(sessionID);
  updateActionVisibility(document.body.dataset.sessionStatus);
  syncProviderDependentControls();
  refreshPermissions().then(scheduleTranscriptRender).catch(() => {});
  window.setInterval(() => {
    if (document.hidden) return;
    refreshSession(sessionID).catch(() => {});
    refreshPermissions().then((changed) => { if (changed) scheduleTranscriptRender(); }).catch(() => {});
  }, 4000);
}
