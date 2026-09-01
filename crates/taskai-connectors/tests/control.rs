// ABOUTME: Conduit control-server tests — real socket, real pty, real event files:
// ABOUTME: dispatch, writer collision, cancel transport, attribution, honest refusals.

use std::collections::HashMap;
use std::io::Write as _;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use taskai_connectors::control::{client_cancel, client_prompt, serve, Shared};
use taskai_connectors::Conduit;
use taskai_pane::Pane;

fn shared_with_cat(tmp: &std::path::Path) -> (Shared, std::path::PathBuf) {
    let cwd = std::env::current_dir().unwrap();
    let pane = Arc::new(Mutex::new(Pane::spawn("/bin/sh", &["-c", "cat -v"], &cwd, 24, 120, &[]).unwrap()));
    let events = tmp.join("events.jsonl");
    std::fs::write(&events, "").unwrap();
    let shared = Shared {
        panes: Arc::new(Mutex::new(HashMap::from([("s1".to_string(), pane)]))),
        session_ids: Arc::new(Mutex::new(HashMap::from([("s1".to_string(), "sid1".to_string())]))),
        harnesses: Arc::new(Mutex::new(HashMap::from([
            ("s1".to_string(), "claude".to_string()),
            ("s9".to_string(), "codex".to_string()),
        ]))),
        events: events.clone(),
        log: tmp.join("dispatch.jsonl"),
        conduit: Arc::new(Conduit::new()),
    };
    let sock = tmp.join("ctl.sock");
    serve(sock.clone(), shared.clone()).unwrap();
    (shared, sock)
}

fn append_stop(events: &std::path::Path, sid: &str) {
    let mut f = std::fs::OpenOptions::new().append(true).open(events).unwrap();
    writeln!(f, "{{\"hook_event_name\":\"Stop\",\"session_id\":\"{sid}\"}}").unwrap();
}

// Expectation: a prompt dispatches into the real pty (bracketed paste visible via cat),
// a concurrent second writer is refused with a clean structured error, and the turn
// completes when the Stop event lands. Why: backend §6 steps 2-5. Value: the whole
// conduit pipeline proven with zero model spend.
#[test]
fn dispatch_collision_and_completion() {
    let tmp = tempfile::tempdir().unwrap();
    let (shared, sock) = shared_with_cat(tmp.path());

    let sock2 = sock.clone();
    let a = std::thread::spawn(move || client_prompt(&sock2, "s1", "hello-turn", true, Some("s7")));
    std::thread::sleep(Duration::from_millis(400));

    let b = client_prompt(&sock, "s1", "second-writer", true, Some("s8")).unwrap();
    assert_eq!(b["ok"], false);
    assert!(b["error"].as_str().unwrap().contains("busy"), "{b}");

    append_stop(&shared.events, "sid1");
    let ra = a.join().unwrap().unwrap();
    assert_eq!(ra["ok"], true, "{ra}");
    assert_eq!(ra["session_id"], "sid1");

    let pane = shared.panes.lock().unwrap()["s1"].clone();
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if pane.lock().unwrap().text().contains("hello-turn") {
            break;
        }
        assert!(Instant::now() < deadline, "prompt never reached the pty");
        std::thread::sleep(Duration::from_millis(100));
    }
}

// Expectation: cancel transports the native interrupt (ESC) into the pty and reports.
// Why: backend §6 step 6 — cancel is a transport, not a decision. Value: provable.
#[test]
fn cancel_transports_native_escape() {
    let tmp = tempfile::tempdir().unwrap();
    let (shared, sock) = shared_with_cat(tmp.path());
    let r = client_cancel(&sock, "s1").unwrap();
    assert_eq!(r["ok"], true, "{r}");
    let pane = shared.panes.lock().unwrap()["s1"].clone();
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if pane.lock().unwrap().text().contains("^[") {
            break;
        }
        assert!(Instant::now() < deadline, "escape never reached the pty");
        std::thread::sleep(Duration::from_millis(100));
    }
}

// Expectation: unattached targets, sessions whose provider identity is not yet bound,
// and harnesses without a prompt transport are refused with specific errors — never a
// blind submit. Why: §6 step 2 and §8 truthfulness. Value: no misrouted turns, ever.
#[test]
fn refusals_are_specific() {
    let tmp = tempfile::tempdir().unwrap();
    let (shared, sock) = shared_with_cat(tmp.path());

    let r = client_prompt(&sock, "s404", "x", false, None).unwrap();
    assert_eq!(r["ok"], false);
    assert!(r["error"].as_str().unwrap().contains("not attached"), "{r}");

    shared.session_ids.lock().unwrap().remove("s1");
    let r = client_prompt(&sock, "s1", "x", true, None).unwrap();
    assert_eq!(r["ok"], false);
    assert!(r["error"].as_str().unwrap().contains("not yet bound"), "{r}");
    shared.session_ids.lock().unwrap().insert("s1".into(), "sid1".into());

    shared.harnesses.lock().unwrap().insert("s1".into(), "codex".into());
    let r = client_prompt(&sock, "s1", "x", false, None).unwrap();
    assert_eq!(r["ok"], false);
    assert!(r["error"].as_str().unwrap().contains("owned-tui"), "{r}");
}

// Expectation: every dispatch is attributed to its source (human UI or a registered
// session) in the dispatch log, which stores correlation only — never prompt content.
// Why: backend §2 attribution + §9 no-content safety state. Value: auditable, safe.
#[test]
fn dispatch_log_attributes_without_content() {
    let tmp = tempfile::tempdir().unwrap();
    let (shared, sock) = shared_with_cat(tmp.path());

    let sock2 = sock.clone();
    let a = std::thread::spawn(move || {
        client_prompt(&sock2, "s1", "secret-prompt-text", true, Some("s7"))
    });
    std::thread::sleep(Duration::from_millis(300));
    append_stop(&shared.events, "sid1");
    a.join().unwrap().unwrap();
    let _ = client_prompt(&sock, "s1", "human-secret", false, None).unwrap();

    let log = std::fs::read_to_string(&shared.log).unwrap();
    assert!(log.contains("\"source\":\"s7\""), "{log}");
    assert!(log.contains("\"source\":\"human\""), "{log}");
    assert!(log.contains("\"target\":\"s1\""), "{log}");
    assert!(!log.contains("secret"), "log must never carry prompt content: {log}");
}
