"use strict";

const page = document.body.dataset.page;

async function apiRequest(path, method = "GET", body) {
  const options = { method, headers: { Accept: "application/json" } };
  if (body !== undefined) {
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  const response = await fetch(path, options);
  const text = await response.text();
  let payload = {};
  if (text) {
    try { payload = JSON.parse(text); } catch (_) { payload = {}; }
  }
  if (!response.ok) {
    const message = payload.error && payload.error.message ? payload.error.message : `Request failed (${response.status})`;
    throw new Error(message);
  }
  return payload;
}

function setNotice(element, message, isError = false) {
  if (!element) return;
  element.textContent = message;
  element.classList.toggle("error", isError);
}

function statusTone(status) {
  if (status === "working" || status === "starting") return "active";
  if (["waiting", "review", "open", "stopped"].includes(status)) return "waiting";
  if (status === "done") return "done";
  if (status === "failed" || status === "cancelled") return "danger";
  return "neutral";
}

function setStatus(element, status) {
  if (!element) return;
  element.className = `status status-${statusTone(status)}`;
  element.replaceChildren();
  element.append(document.createElement("i"), document.createTextNode(status));
}

function bindTaskCreation() {
  const form = document.querySelector("#create-task-form");
  const notice = document.querySelector("#form-notice");
  if (!form) return;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = form.querySelector("button[type=submit]");
    submit.disabled = true;
    setNotice(notice, "Creating Task…");
    try {
      const data = new FormData(form);
      const result = await apiRequest("/api/v1/tasks", "POST", {
        title: data.get("title"),
        description: data.get("description"),
        external_ref: data.get("external_ref")
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
  const notice = document.querySelector("#form-notice");
  if (!form) return;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = form.querySelector("button[type=submit]");
    submit.disabled = true;
    setNotice(notice, "Starting detached Session worker…");
    try {
      const data = new FormData(form);
      const result = await apiRequest(`/api/v1/tasks/${encodeURIComponent(taskID)}/sessions`, "POST", {
        agent: data.get("agent"),
        role: data.get("role"),
        repo_path: data.get("repo_path"),
        prompt: data.get("prompt"),
        worktree: data.has("worktree"),
        approve_all: data.has("approve_all"),
        allow_shared_checkout: data.has("allow_shared_checkout")
      });
      window.location.assign(`/sessions/${encodeURIComponent(result.session.id)}`);
    } catch (error) {
      setNotice(notice, error.message, true);
      submit.disabled = false;
    }
  });
}

function makeSessionCard(item) {
  const link = document.createElement("a");
  link.className = item.role === "reviewer" ? "session-card reviewer" : "session-card";
  link.href = `/sessions/${encodeURIComponent(item.id)}`;

  const identity = document.createElement("div");
  const title = document.createElement("strong");
  title.textContent = `${item.agent} · ${item.role}`;
  const id = document.createElement("span");
  id.className = "mono";
  id.textContent = item.id;
  identity.append(title, id);

  const meta = document.createElement("div");
  meta.className = "session-meta";
  const checkout = document.createElement("span");
  const path = item.worktree_path || item.repo_path || "No checkout";
  checkout.textContent = path.split("/").filter(Boolean).pop() || path;
  meta.append(checkout);
  if (item.branch) {
    const branch = document.createElement("span");
    branch.textContent = item.branch;
    meta.append(branch);
  }

  const state = document.createElement("span");
  setStatus(state, item.status);
  link.append(identity, meta, state);
  return link;
}

async function refreshTask(taskID) {
  const result = await apiRequest(`/api/v1/tasks/${encodeURIComponent(taskID)}`);
  setStatus(document.querySelector("#task-status"), result.task.status);
  const count = document.querySelector("#session-count");
  if (count) count.textContent = String(result.sessions.length);
  const list = document.querySelector("#session-list");
  if (!list) return;
  list.replaceChildren();
  if (result.sessions.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-state small";
    const title = document.createElement("h3");
    title.textContent = "No Sessions";
    const detail = document.createElement("p");
    detail.textContent = "Start the first agent Session below.";
    empty.append(title, detail);
    list.append(empty);
    return;
  }
  for (const item of result.sessions) list.append(makeSessionCard(item));
}

const activityState = { entries: [], seen: new Set(), permissions: new Map() };

function rememberPermission(activity) {
  const permission = activity.permission;
  if (!permission || !permission.id) return false;
  activityState.permissions.set(permission.id, permission);
  return true;
}

async function submitPermission(sessionID, permission, option, card) {
  const persistent = option.kind === "allow_always" || option.kind === "reject_always";
  const warning = persistent ? " This is a persistent provider choice." : "";
  if (!window.confirm(`${option.name || option.kind} for ${permission.title || "this tool request"}?${warning}`)) return;
  for (const button of card.querySelectorAll("button")) button.disabled = true;
  const notice = document.querySelector("#action-notice");
  setNotice(notice, "Sending exact permission decision…");
  try {
    await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/permissions/decision`, "POST", {
      permission_id: permission.id,
      option_id: option.id,
      cancelled: false
    });
    activityState.permissions.set(permission.id, { ...permission, state: "resolved", decision: option.kind });
    renderActivities();
    setNotice(notice, "Permission decision accepted; the agent turn continues.");
  } catch (error) {
    for (const button of card.querySelectorAll("button")) button.disabled = false;
    setNotice(notice, error.message, true);
  }
}

function makePermissionCard(permission, sessionID) {
  const card = document.createElement("section");
  card.className = "permission-card";
  const title = document.createElement("strong");
  title.textContent = `Approval required · ${permission.title || "Tool request"}`;
  const detail = document.createElement("p");
  detail.textContent = "The agent remains waiting if this browser detaches.";
  const options = document.createElement("div");
  options.className = "permission-options";
  for (const option of permission.options || []) {
    const button = document.createElement("button");
    const persistent = option.kind === "allow_always" || option.kind === "reject_always";
    button.type = "button";
    button.textContent = `${option.name || option.kind}${persistent ? " · PERSISTENT" : ""}`;
    if (option.kind.startsWith("reject")) button.classList.add("permission-reject");
    button.addEventListener("click", () => submitPermission(sessionID, permission, option, card));
    options.append(button);
  }
  card.append(title, detail, options);
  return card;
}

function makeActivityElement(activity, showRaw) {
  const row = document.createElement("article");
  row.className = "activity-item";
  const label = document.createElement("div");
  label.className = "activity-label";
  const owner = document.createElement("strong");
  owner.textContent = activity.session_id || "Session";
  const position = document.createElement("span");
  position.textContent = `#${activity.position}${activity.request_id ? ` · ${activity.request_id}` : ""}`;
  label.append(owner, position);

  const message = document.createElement(showRaw ? "pre" : "p");
  message.className = showRaw ? "activity-message raw" : "activity-message";
  // "Raw" intentionally means the sanitized API DTO. The API never includes
  // ACP wire payloads, worker sockets, or JSONL storage fields.
  message.textContent = showRaw ? JSON.stringify(activity, null, 2) : activity.activity;
  row.append(label, message);
  if (page === "session" && activity.permission?.state === "waiting") {
    const current = activityState.permissions.get(activity.permission.id);
    if (current?.state === "waiting") row.append(makePermissionCard(current, activity.session_id));
  }
  return row;
}

function renderActivities() {
  const feed = document.querySelector("#activity");
  if (!feed) return;
  const showRaw = Boolean(document.querySelector("#raw-toggle")?.checked);
  feed.replaceChildren();
  if (activityState.entries.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-state small activity-placeholder";
    const message = document.createElement("p");
    message.textContent = "Waiting for Session activity…";
    empty.append(message);
    feed.append(empty);
    return;
  }
  for (const activity of activityState.entries) feed.append(makeActivityElement(activity, showRaw));
  feed.scrollTop = feed.scrollHeight;
}

function attachActivityStream(path) {
  const feed = document.querySelector("#activity");
  if (!feed || !window.EventSource) return;
  document.querySelector("#raw-toggle")?.addEventListener("change", renderActivities);
  const state = document.querySelector("#stream-state");
  const source = new EventSource(path);
  source.onopen = () => {
    if (state) {
      state.className = "stream-state connected";
      state.replaceChildren(document.createElement("i"), document.createTextNode("Connected"));
    }
  };
  source.addEventListener("activity", (event) => {
    try {
      const activity = JSON.parse(event.data);
      const key = `${activity.session_id}:${activity.position}`;
      if (activityState.seen.has(key)) return;
      activityState.seen.add(key);
      activityState.entries.push(activity);
      const permissionChanged = rememberPermission(activity);
      const placeholder = feed.querySelector(".activity-placeholder");
      if (placeholder) placeholder.remove();
      if (permissionChanged) {
        renderActivities();
      } else {
        const showRaw = Boolean(document.querySelector("#raw-toggle")?.checked);
        feed.append(makeActivityElement(activity, showRaw));
      }
      feed.scrollTop = feed.scrollHeight;
    } catch (_) {
      if (state) state.textContent = "Invalid activity";
    }
  });
  source.onerror = () => {
    if (state) {
      state.className = "stream-state";
      state.replaceChildren(document.createElement("i"), document.createTextNode("Reconnecting"));
    }
  };
  window.addEventListener("beforeunload", () => source.close(), { once: true });
}

async function refreshSession(sessionID) {
  const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}`);
  setStatus(document.querySelector("#session-status"), result.session.status);
  return result.session;
}

function bindSessionControls(sessionID) {
  const notice = document.querySelector("#action-notice");
  const promptForm = document.querySelector("#prompt-form");
  promptForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = promptForm.querySelector("button[type=submit]");
    submit.disabled = true;
    try {
      const data = new FormData(promptForm);
      const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/prompt`, "POST", { message: data.get("message") });
      setNotice(notice, `Accepted request ${result.request_id} (${result.state}).`);
      promptForm.reset();
    } catch (error) {
      setNotice(notice, error.message, true);
    } finally {
      submit.disabled = false;
    }
  });

  for (const button of document.querySelectorAll("[data-action]")) {
    button.addEventListener("click", async () => {
      const action = button.dataset.action;
      button.disabled = true;
      setNotice(notice, `${action[0].toUpperCase()}${action.slice(1)} requested…`);
      try {
        const body = action === "resume" ? {} : undefined;
        await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/${action}`, "POST", body);
        setNotice(notice, `${action[0].toUpperCase()}${action.slice(1)} completed.`);
        await refreshSession(sessionID);
      } catch (error) {
        setNotice(notice, error.message, true);
      } finally {
        button.disabled = false;
      }
    });
  }

  const reviewForm = document.querySelector("#review-form");
  reviewForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = reviewForm.querySelector("button[type=submit]");
    submit.disabled = true;
    setNotice(notice, "Creating independent reviewer Session…");
    try {
      const data = new FormData(reviewForm);
      const result = await apiRequest(`/api/v1/sessions/${encodeURIComponent(sessionID)}/review`, "POST", {
        agent: data.get("agent"),
        allow_active_producer: data.has("allow_active_producer")
      });
      window.location.assign(`/sessions/${encodeURIComponent(result.session.id)}`);
    } catch (error) {
      setNotice(notice, error.message, true);
      submit.disabled = false;
    }
  });
}

if (page === "tasks") {
  bindTaskCreation();
}

if (page === "task") {
  const taskID = document.body.dataset.taskId;
  bindSessionCreation(taskID);
  attachActivityStream(`/api/v1/tasks/${encodeURIComponent(taskID)}/stream`);
  window.setInterval(() => {
    if (!document.hidden) refreshTask(taskID).catch(() => {});
  }, 3000);
}

if (page === "session") {
  const sessionID = document.body.dataset.sessionId;
  bindSessionControls(sessionID);
  attachActivityStream(`/api/v1/sessions/${encodeURIComponent(sessionID)}/stream`);
  window.setInterval(() => {
    if (!document.hidden) refreshSession(sessionID).catch(() => {});
  }, 3000);
}
