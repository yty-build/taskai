// ABOUTME: OpenCode connector tests — probe the real CLI, run a real authenticated
// ABOUTME: loopback server, create a session over HTTP, attach the native TUI. All free.

mod harness;

use std::time::{Duration, Instant};

use taskai_connectors::opencode::OpenCodeConnector;
use taskai_connectors::Mode;

// Expectation: probing the installed OpenCode CLI reports shared-live with a version.
// Why: capability truth; 1.18.25 is the version-gated environment pin. Value: earned READY.
#[test]
fn probe_real_opencode() {
    if !harness::present("opencode") {
        return;
    }
    let cap = OpenCodeConnector::default().probe();
    assert_eq!(cap.mode, Mode::SharedLive, "detail: {}", cap.detail);
    assert!(cap.version.is_some(), "detail: {}", cap.detail);
}

// Expectation: the loopback server starts with a generated password; unauthenticated
// requests are refused (401) and authenticated ones succeed; a session created over
// HTTP is bound to the workspace directory. Why: backend §7 — one authenticated
// loopback serve shared by the native CLI and the facade. Value: the shared-live
// substrate is proven free of model spend.
#[test]
fn server_is_authenticated_and_creates_sessions() {
    if !harness::present("opencode") {
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let c = OpenCodeConnector::default();
    let server = c.start_server(tmp.path()).unwrap();

    let (code, _) = server.http_unauthenticated("GET", "/session", None).unwrap();
    assert_eq!(code, 401, "unauthenticated must be refused");

    let (code, body) = server.http("GET", "/session", None).unwrap();
    assert_eq!(code, 200, "{body}");

    let id = server.create_session().unwrap();
    assert!(id.starts_with("ses_"), "{id}");
    let (_, listing) = server.http("GET", "/session", None).unwrap();
    assert!(listing.contains(&id), "created session listed");
}

// Expectation: the native OpenCode TUI attaches to the same server inside a real pane
// and renders. Why: shared-live — the visible CLI and the facade share one server.
// Value: the pane shows the exact native client, not a re-implementation.
#[test]
fn native_tui_attaches_to_shared_server() {
    if !harness::present("opencode") {
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let c = OpenCodeConnector::default();
    let server = c.start_server(tmp.path()).unwrap();
    let (prog, args, envs) = server.attach_command();
    let argv: Vec<&str> = args.iter().map(String::as_str).collect();
    let envrefs: Vec<(&str, &str)> = envs.iter().map(|(k, v)| (k.as_str(), v.as_str())).collect();
    let pane = taskai_pane::Pane::spawn(&prog, &argv, tmp.path(), 30, 100, &envrefs).unwrap();
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let text = pane.text();
        if !text.trim().is_empty() {
            break;
        }
        assert!(Instant::now() < deadline, "opencode TUI never rendered");
        std::thread::sleep(Duration::from_millis(300));
    }
}

// Expectation: a prompt posted through the facade renders in the shared session and the
// assistant reply correlates to it; completion arrives via the event stream's idle edge.
// Why: the M5 shared-live proof (backend §7). Value: same-session live rendering proven.
// First approved run calibrates exact event names against opencode 1.18.25.
#[test]
#[ignore = "billed: one live OpenCode turn; requires T approval"]
fn live_opencode_turn_correlates() {
    let tmp = tempfile::tempdir().unwrap();
    let c = OpenCodeConnector::default();
    let server = c.start_server(tmp.path()).unwrap();
    let id = server.create_session().unwrap();
    let events = server.subscribe().unwrap();

    let body = serde_json::json!({"parts": [{"type": "text", "text": "Reply with exactly TASKAI-M5-OK and nothing else."}]});
    let (code, resp) = server
        .http("POST", &format!("/session/{id}/message"), Some(&body.to_string()))
        .unwrap();
    assert_eq!(code, 200, "{resp}");
    assert!(resp.contains("TASKAI-M5-OK") || resp.contains(&id), "response correlates: {resp}");

    let deadline = Instant::now() + Duration::from_secs(120);
    let mut idle_seen = false;
    while Instant::now() < deadline {
        if let Ok(ev) = events.recv_timeout(Duration::from_secs(5)) {
            let s = ev.to_string();
            if s.contains("session.idle") || s.contains("idle") {
                idle_seen = true;
                break;
            }
        }
    }
    assert!(idle_seen, "session idle edge observed");
    let (_, msgs) = server.http("GET", &format!("/session/{id}/message"), None).unwrap();
    assert!(msgs.contains("TASKAI-M5-OK"), "history integrity: {msgs}");
}
