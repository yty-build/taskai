// ABOUTME: Claude connector tests — probe against the real CLI, hook installation that
// ABOUTME: never clobbers user config, event watching, writer lock, and pane launch.

mod harness;

use std::time::Duration;

use taskai_connectors::claude::{install_hooks, parse_version, ClaudeConnector};
use taskai_connectors::events::wait_for_event;
use taskai_connectors::{Conduit, Mode};

// Expectation: the version parser extracts the semver from real `claude --version`
// output shapes. Why: probe truthfulness starts with reading the version right.
// Value: version gating never guesses.
#[test]
fn parses_version_output() {
    assert_eq!(parse_version("2.1.226 (Claude Code)"), Some("2.1.226".into()));
    assert_eq!(parse_version("claude 3.0.0\n"), Some("3.0.0".into()));
    assert_eq!(parse_version("no digits here"), None);
}

// Expectation: probing the installed Claude CLI reports owned-tui with a version.
// Why: capability reporting must reflect the real machine (backend §2). This machine
// pins Claude Code in the environment matrix. Value: READY in the bay is earned.
#[test]
fn probe_real_claude() {
    if !harness::present("claude") {
        return;
    }
    let cap = ClaudeConnector::default().probe();
    assert_eq!(cap.mode, Mode::OwnedTui, "detail: {}", cap.detail);
    assert!(cap.version.is_some(), "detail: {}", cap.detail);
}

// Expectation: probing a missing binary reports Unsupported, never a weaker claim or a
// panic. Why: failed identity is `unsupported` (backend §8). Value: honest degradation.
#[test]
fn probe_missing_binary_is_unsupported() {
    let cap = ClaudeConnector { bin: "taskai-no-such-cli".into() }.probe();
    assert_eq!(cap.mode, Mode::Unsupported);
}

// Expectation: hook installation merges into existing .claude/settings.local.json,
// preserving user content, and is idempotent. Why: TaskAI never silently overwrites an
// existing instruction or config file (backend §5). Value: safe re-runs.
#[test]
fn hooks_install_merges_and_is_idempotent() {
    let tmp = tempfile::tempdir().unwrap();
    let ws = tmp.path().join("ws");
    std::fs::create_dir_all(ws.join(".claude")).unwrap();
    std::fs::write(
        ws.join(".claude/settings.local.json"),
        r#"{"permissions":{"allow":["Bash(ls:*)"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo user-hook"}]}]}}"#,
    )
    .unwrap();
    let events = install_hooks(&ws, tmp.path()).unwrap();
    install_hooks(&ws, tmp.path()).unwrap();

    let cfg: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(ws.join(".claude/settings.local.json")).unwrap()).unwrap();
    assert_eq!(cfg["permissions"]["allow"][0], "Bash(ls:*)");
    let stops = cfg["hooks"]["Stop"].as_array().unwrap();
    let all = serde_json::to_string(&stops).unwrap();
    assert!(all.contains("echo user-hook"), "user hook preserved: {all}");
    let ours = stops
        .iter()
        .filter(|e| e.to_string().contains(events.to_str().unwrap()))
        .count();
    assert_eq!(ours, 1, "taskai stop hook installed exactly once: {all}");
    assert!(cfg["hooks"]["SessionStart"].is_array());
}

// Expectation: the event watcher returns the first matching hook event appended after
// the given offset, and times out cleanly otherwise. Why: completion is observed from
// the Stop hook's real file output. Value: terminal detection is a provable read.
#[test]
fn event_watch_matches_kind_session_and_offset() {
    let tmp = tempfile::tempdir().unwrap();
    let f = tmp.path().join("events.jsonl");
    std::fs::write(&f, "{\"hook_event_name\":\"Stop\",\"session_id\":\"old\"}\n").unwrap();
    let offset = std::fs::metadata(&f).unwrap().len();

    let f2 = f.clone();
    let writer = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(200));
        use std::io::Write;
        let mut fh = std::fs::OpenOptions::new().append(true).open(&f2).unwrap();
        writeln!(fh, "{{\"hook_event_name\":\"SessionStart\",\"session_id\":\"abc\"}}").unwrap();
        writeln!(fh, "{{\"hook_event_name\":\"Stop\",\"session_id\":\"abc\"}}").unwrap();
    });

    let ev = wait_for_event(&f, "Stop", None, offset, Duration::from_secs(5)).unwrap();
    assert_eq!(ev.session_id.as_deref(), Some("abc"));
    writer.join().unwrap();

    let end = std::fs::metadata(&f).unwrap().len();
    assert!(wait_for_event(&f, "Stop", Some("abc"), end, Duration::from_millis(300)).is_err());
}

// Expectation: one writer per session — a second concurrent turn is refused and the
// slot frees when the guard drops. Why: writer exclusion prevents two clients from
// corrupting one provider session (backend §9). Value: collision safety is structural.
#[test]
fn conduit_refuses_second_writer() {
    let conduit = Conduit::new();
    let guard = conduit.begin_turn("s1").unwrap();
    assert!(conduit.begin_turn("s1").is_err());
    assert!(conduit.begin_turn("s2").is_ok());
    drop(guard);
    assert!(conduit.begin_turn("s1").is_ok());
}

// Expectation: the real Claude CLI launches inside a real pane and renders its own
// chrome. Why: owned-tui — the displayed PTY is the session; TaskAI re-implements
// nothing. Value: the primary harness provably renders as itself. (Free: no prompt runs.)
#[test]
fn real_claude_renders_in_pane() {
    if !harness::present("claude") {
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let ws = tmp.path().join("ws");
    std::fs::create_dir_all(&ws).unwrap();
    let c = ClaudeConnector::default();
    let (prog, args) = c.launch_command();
    let argv: Vec<&str> = args.iter().map(String::as_str).collect();
    let pane = taskai_pane::Pane::spawn(&prog, &argv, &ws, 30, 100, &[]).unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(30);
    loop {
        let text = pane.text();
        if ["Claude", "claude", "trust", "Welcome"].iter().any(|m| text.contains(m)) {
            break;
        }
        assert!(std::time::Instant::now() < deadline, "claude never rendered; grid:\n{text}");
        std::thread::sleep(Duration::from_millis(200));
    }
}

// Expectation: one full billed turn — prompt submitted through the conduit renders in
// the same displayed Claude PTY and terminates via the Stop hook with a bound session
// id. Why: the M3 live proof (docs/test_plan.md, approval protocol). Value: the owned-tui
// chain is proven end to end on real infrastructure.
// Run only with T approval: TASKAI_LIVE_WS=<trusted workspace> cargo test -p taskai-connectors -- --ignored
#[test]
#[ignore = "billed: one live Claude turn; requires T approval and TASKAI_LIVE_WS"]
fn live_claude_turn_renders_and_stops() {
    let ws = std::env::var("TASKAI_LIVE_WS").expect("set TASKAI_LIVE_WS to a trusted workspace");
    let ws = std::path::PathBuf::from(ws);
    let runtime = tempfile::tempdir().unwrap();
    let events = install_hooks(&ws, runtime.path()).unwrap();
    let offset = std::fs::metadata(&events).map(|m| m.len()).unwrap_or(0);

    let c = ClaudeConnector::default();
    let (prog, args) = c.launch_command();
    let argv: Vec<&str> = args.iter().map(String::as_str).collect();
    let mut pane = taskai_pane::Pane::spawn(&prog, &argv, &ws, 40, 120, &[]).unwrap();

    let start = wait_for_event(&events, "SessionStart", None, offset, Duration::from_secs(120))
        .expect("SessionStart hook");
    let sid = start.session_id.clone().expect("session id");

    pane.write_bytes(&ClaudeConnector::prompt_bytes(
        "Reply with exactly TASKAI-M3-OK and nothing else. Do not use any tools.",
    ))
    .unwrap();

    let stop = wait_for_event(&events, "Stop", Some(&sid), start.offset, Duration::from_secs(300))
        .expect("Stop hook");
    assert_eq!(stop.session_id.as_deref(), Some(sid.as_str()));
    assert!(pane.text().contains("TASKAI-M3-OK"), "grid:\n{}", pane.text());
}

// Expectation: versions below the connector's declared minimum downgrade the capability
// to Unsupported with a truthful detail — never a silent break. Why: backend §8 declares
// version ranges; §11 gates on provider upgrades. Value: upgrades fail loudly.
#[test]
fn version_gate_downgrades_truthfully() {
    use taskai_connectors::version_ok;
    assert!(version_ok("2.1.226", "2.0.0"));
    assert!(!version_ok("1.9.9", "2.0.0"));
    assert!(version_ok("1.18.25", "1.18.0"));
    assert!(!version_ok("1.17.9", "1.18.0"));
    assert!(version_ok("0.150.1", "0.100.0"));
    assert!(!version_ok("0.99.0", "0.100.0"));
    assert!(version_ok("1.0.13", "1.0.0"));
}
