// ABOUTME: Grok connector tests — probe the real CLI, ACP handshake over stdio with
// ABOUTME: loadSession capability, exact-ID resume command, truthful mode label. All free.

use std::time::Duration;

use taskai_connectors::grok::GrokConnector;
use taskai_connectors::Mode;

// Expectation: probing the installed Grok CLI reports handoff-resume — never live —
// with a version. Why: backend §7 — Grok proves persistence and exact resume, not
// multi-client live rendering; truth over theater. Value: the one hard case never lies.
#[test]
fn probe_real_grok_is_handoff_resume() {
    let cap = GrokConnector::default().probe();
    assert_eq!(cap.mode, Mode::HandoffResume, "detail: {}", cap.detail);
    assert_eq!(cap.mode.label(), "handoff-resume");
    assert!(cap.version.is_some(), "detail: {}", cap.detail);
}

// Expectation: a missing binary reports Unsupported, never a weaker claim.
// Why: failed identity is `unsupported` (backend §8). Value: honest degradation.
#[test]
fn probe_missing_binary_is_unsupported() {
    let cap = GrokConnector { bin: "taskai-no-such-cli".into() }.probe();
    assert_eq!(cap.mode, Mode::Unsupported);
}

// Expectation: `grok agent stdio` answers ACP initialize with protocolVersion and the
// loadSession capability — the free proof the handoff channel can address an existing
// session. Why: backend §7's submission conduit is this exact channel. Value: handoff
// plumbing verified without model spend.
#[test]
fn acp_handshake_supports_load_session() {
    let c = GrokConnector::default();
    let mut agent = c.spawn_acp().unwrap();
    let result = agent.initialize(Duration::from_secs(20)).unwrap();
    assert_eq!(result["protocolVersion"], 1, "{result}");
    assert_eq!(result["agentCapabilities"]["loadSession"], true, "{result}");
}

// Expectation: the resume command is the exact native CLI with the exact session id.
// Why: results appear after resume, in the real Grok. Value: no simulated rendering.
#[test]
fn resume_command_is_exact_id_native() {
    let c = GrokConnector::default();
    let (prog, args) = c.resume_command("0199aaaa-bbbb-cccc-dddd-eeeeffff0000");
    assert_eq!(prog, "grok");
    assert_eq!(args, vec!["--resume", "0199aaaa-bbbb-cccc-dddd-eeeeffff0000"]);
}

// Expectation: one full billed handoff — ACP session/new + prompt returns a stop reason,
// and the exact-ID native resume shows the reply in the real Grok TUI. Why: the M6 live
// proof (approval protocol). Value: handoff-resume proven end to end.
// First approved run calibrates exact ACP param shapes against grok 1.0.13.
#[test]
#[ignore = "billed: one live Grok turn; requires T approval"]
fn live_grok_handoff_and_resume() {
    let ws = tempfile::tempdir().unwrap();
    let c = GrokConnector::default();
    let mut agent = c.spawn_acp().unwrap();
    agent.initialize(Duration::from_secs(20)).unwrap();
    let new = agent
        .request(
            "session/new",
            serde_json::json!({"cwd": ws.path(), "mcpServers": []}),
            Duration::from_secs(30),
        )
        .unwrap();
    let sid = new["sessionId"].as_str().expect("session id").to_string();
    let done = agent
        .request(
            "session/prompt",
            serde_json::json!({"sessionId": sid, "prompt": [{"type": "text", "text": "Reply with exactly TASKAI-M6-OK and nothing else."}]}),
            Duration::from_secs(300),
        )
        .unwrap();
    assert!(done["stopReason"].is_string(), "{done}");
    drop(agent);

    let (prog, args) = c.resume_command(&sid);
    let argv: Vec<&str> = args.iter().map(String::as_str).collect();
    let pane = taskai_pane::Pane::spawn(&prog, &argv, ws.path(), 40, 120, &[]).unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(60);
    loop {
        if pane.text().contains("TASKAI-M6-OK") {
            break;
        }
        assert!(std::time::Instant::now() < deadline, "resume never showed the reply:\n{}", pane.text());
        std::thread::sleep(Duration::from_millis(300));
    }
}
