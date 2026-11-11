// ABOUTME: Private CLI proof that Grok's native mode hook forwards evidence without a decision.
// ABOUTME: The fixture uses only a private socket and never starts a provider or model turn.

use std::{
    io::Write,
    process::{Command, Stdio},
};

use taskai_connectors::grok::ModeEvidenceServer;
use taskai_core::mail::{strip_messaging_env, PermissionMode};

#[test]
fn grok_hook_cli_has_empty_stdout_and_one_use_exact_evidence() {
    let root = tempfile::tempdir().unwrap();
    let socket = root.path().join("mode.sock");
    let server = ModeEvidenceServer::listen(&socket, "owned-session").unwrap();
    let envelope = serde_json::json!({
        "sessionId": "owned-session",
        "hookEventName": "pre_tool_use",
        "hook_event_name": "PreToolUse",
        "toolName": "taskai__send_message",
        "toolUseId": "tool-one",
        "toolInputTruncated": false,
        "permissionMode": "default",
        "timestamp": chrono::Utc::now().to_rfc3339(),
        "toolInput": {"to":"agent-two", "text":"private hello", "notify_when_idle":false}
    });
    let mut command = Command::new(env!("CARGO_BIN_EXE_taskai"));
    command
        .arg("grok-mode-hook")
        .env("TASKAI_MODE_SOCKET", &socket)
        .env("GROK_SESSION_ID", "owned-session")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    strip_messaging_env(&mut command);
    let mut child = command.spawn().unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(envelope.to_string().as_bytes())
        .unwrap();
    let result = child.wait_with_output().unwrap();
    assert!(result.status.success());
    assert!(
        result.stdout.is_empty(),
        "hook cannot decide native permissions"
    );
    assert!(result.stderr.is_empty());
    assert_eq!(
        server
            .evidence()
            .take_for("agent-two", "private hello", false),
        Ok(PermissionMode::Prompting)
    );
    assert!(server
        .evidence()
        .take_for("agent-two", "private hello", false)
        .is_err());
    server.close();
}

#[test]
fn grok_hook_cli_failure_also_has_no_native_decision() {
    let mut command = Command::new(env!("CARGO_BIN_EXE_taskai"));
    command
        .arg("grok-mode-hook")
        .env_remove("TASKAI_MODE_SOCKET")
        .env_remove("GROK_SESSION_ID")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    strip_messaging_env(&mut command);
    let child = command.output().unwrap();
    assert!(child.status.success());
    assert!(child.stdout.is_empty());
    assert!(String::from_utf8_lossy(&child.stderr).contains("missing TASKAI_MODE_SOCKET"));
}
