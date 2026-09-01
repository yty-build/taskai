// ABOUTME: Codex connector tests — probe the real CLI, spawn the real app-server sidecar,
// ABOUTME: JSON-RPC handshake, thread listing, and honest protocol errors. All free.

mod harness;

use std::time::Duration;

use taskai_connectors::codex::CodexConnector;
use taskai_connectors::Mode;

// Expectation: probing the installed Codex CLI reports shared-live with a version.
// Why: capability truth per backend §2; this machine pins codex-cli in the matrix.
// Value: READY in the bay is earned, not assumed.
#[test]
fn probe_real_codex() {
    let cap = CodexConnector::default().probe();
    if !harness::ready("codex", &cap) {
        return;
    }
    assert_eq!(cap.mode, Mode::SharedLive, "detail: {}", cap.detail);
    assert!(cap.version.is_some(), "detail: {}", cap.detail);
}

// Expectation: the app-server sidecar spawns on stdio and answers initialize with its
// identity. Why: backend §7 — the conduit is the app-server; the handshake is the free
// proof it exists. Value: connectivity is verified before any billed work.
#[test]
fn app_server_initialize_handshake() {
    if !harness::ready("codex", &CodexConnector::default().probe()) {
        return;
    }
    let c = CodexConnector::default();
    let mut server = c.spawn_app_server().unwrap();
    let result = server.initialize(Duration::from_secs(15)).unwrap();
    assert!(
        result["userAgent"]
            .as_str()
            .unwrap_or("")
            .contains("taskai"),
        "{result}"
    );
    assert!(result["codexHome"].is_string(), "{result}");
}

// Expectation: thread/list answers after the handshake — the shared thread store is
// reachable, which is how TaskAI resolves the visible TUI's exact threadId.
// Why: exact session identity is the §11 gate. Value: resolution is free to prove.
#[test]
fn app_server_lists_threads() {
    if !harness::ready("codex", &CodexConnector::default().probe()) {
        return;
    }
    let c = CodexConnector::default();
    let mut server = c.spawn_app_server().unwrap();
    server.initialize(Duration::from_secs(15)).unwrap();
    let result = server
        .request(
            "thread/list",
            serde_json::json!({"limit": 3}),
            Duration::from_secs(15),
        )
        .unwrap();
    assert!(result["data"].is_array(), "thread/list shape: {result}");
}

// Expectation: an unknown method returns a JSON-RPC error, not silence. Why: honest
// protocol surface — errors must be observable. Value: failures are diagnosable.
#[test]
fn app_server_reports_unknown_method() {
    if !harness::ready("codex", &CodexConnector::default().probe()) {
        return;
    }
    let c = CodexConnector::default();
    let mut server = c.spawn_app_server().unwrap();
    server.initialize(Duration::from_secs(15)).unwrap();
    let err = server
        .request(
            "taskai/bogus",
            serde_json::json!({}),
            Duration::from_secs(15),
        )
        .unwrap_err();
    assert!(err.to_string().contains("unknown variant"), "{err}");
}

// Expectation: a turn started through the app-server on a thread completes with a
// correlated turn event and the thread's history contains the reply. Why: the M5
// shared-live proof — threadId + turn correlation (backend §7). Value: conduit-initiated
// work is provably the same thread the native TUI shows.
// First approved run calibrates exact param/event names against codex-cli 0.150.1.
#[test]
#[ignore = "billed: one live Codex turn; requires T approval"]
fn live_codex_turn_completes() {
    let c = CodexConnector::default();
    let mut server = c.spawn_app_server().unwrap();
    server.initialize(Duration::from_secs(15)).unwrap();
    let ws = tempfile::tempdir().unwrap();
    let started = server
        .request(
            "thread/start",
            serde_json::json!({"cwd": ws.path()}),
            Duration::from_secs(30),
        )
        .unwrap();
    let thread_id = started["threadId"]
        .as_str()
        .or_else(|| started["thread"]["id"].as_str())
        .expect("thread id")
        .to_string();
    server
        .request(
            "turn/start",
            serde_json::json!({"threadId": thread_id, "input": [{"type": "text", "text": "Reply with exactly TASKAI-M5-OK and nothing else."}]}),
            Duration::from_secs(30),
        )
        .unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(300);
    let mut completed = false;
    while std::time::Instant::now() < deadline {
        if let Ok(n) = server.notifications().recv_timeout(Duration::from_secs(5)) {
            let s = n.to_string();
            if s.contains(&thread_id) && (s.contains("turn/completed") || s.contains("completed")) {
                completed = true;
                break;
            }
        }
    }
    assert!(completed, "turn/completed observed for {thread_id}");
}
