// ABOUTME: Native Grok hook evidence on private real sockets, matched once to exact send arguments.
// ABOUTME: Rejects guessed, stale, duplicate and ambiguous modes without contacting any provider.

use chrono::{SecondsFormat, Utc};
use serde_json::{json, Value};
use std::{
    fs,
    io::Write,
    os::unix::{fs::PermissionsExt, net::UnixStream},
    time::{Duration, Instant},
};
use taskai_connectors::grok::{forward_mode_hook, install_mode_hook, ModeEvidenceServer};
use taskai_core::mail::PermissionMode;

fn hook(id: &str, mode: &str, text: &str) -> Value {
    json!({
        "hookEventName": "pre_tool_use", "hook_event_name": "PreToolUse",
        "sessionId": "native-owned", "permissionMode": mode,
        "toolName": "taskai__send_message", "toolUseId": id,
        "toolInput": {"to": "codex-peer", "text": text, "notify_when_idle": true},
        "toolInputTruncated": false,
        "timestamp": Utc::now().to_rfc3339_opts(SecondsFormat::Millis, true)
    })
}

fn forward(socket: &std::path::Path, value: &Value) -> Result<(), taskai_connectors::Error> {
    let bytes = serde_json::to_vec(value).unwrap();
    forward_mode_hook(bytes.as_slice(), socket, "native-owned")
}

// A hook can authorize one exact call; it cannot authorize another argument or a later replay.
#[test]
fn native_hook_proves_exactly_one_matching_send() {
    let root = tempfile::tempdir().unwrap();
    let socket = root.path().join("mode.sock");
    let server = ModeEvidenceServer::listen(&socket, "native-owned").unwrap();
    let evidence = server.evidence();
    assert_eq!(
        fs::metadata(&socket).unwrap().permissions().mode() & 0o777,
        0o600
    );
    assert!(evidence.take_for("codex-peer", "letter", true).is_err());
    forward(&socket, &hook("call-1", "bypassPermissions", "letter")).unwrap();
    assert_eq!(
        evidence.take_for("codex-peer", "letter", true).unwrap(),
        PermissionMode::Bypass
    );
    assert!(evidence.take_for("codex-peer", "letter", true).is_err());
    assert!(forward(&socket, &hook("call-1", "default", "letter")).is_err());
    assert!(evidence.take_for("codex-peer", "letter", true).is_err());
    for (i, mode) in ["default", "auto", "plan"].iter().enumerate() {
        let text = format!("distinct {i}");
        forward(&socket, &hook(&format!("prompting-{i}"), mode, &text)).unwrap();
        assert_eq!(
            evidence.take_for("codex-peer", &text, true).unwrap(),
            PermissionMode::Prompting
        );
    }
    server.close();
    assert!(!socket.exists());
    assert!(evidence.take_for("codex-peer", "letter", true).is_err());
}

// Missing, malformed or foreign native evidence must never manufacture a default mode.
#[test]
fn malformed_foreign_truncated_and_stale_hooks_are_refused() {
    let root = tempfile::tempdir().unwrap();
    let socket = root.path().join("mode.sock");
    let server = ModeEvidenceServer::listen(&socket, "native-owned").unwrap();
    let cases = [
        ("sessionId", json!("foreign")),
        ("permissionMode", json!("unknown")),
        ("permissionMode", Value::Null),
        ("toolName", json!("other__send_message")),
        ("toolUseId", json!("")),
        ("toolInputTruncated", json!(true)),
        ("toolInputTruncated", Value::Null),
        ("hookEventName", json!("post_tool_use")),
        ("hook_event_name", json!("PostToolUse")),
        ("timestamp", json!("2000-01-01T00:00:00Z")),
        ("timestamp", json!("2999-01-01T00:00:00Z")),
        ("timestamp", Value::Null),
        (
            "toolInput",
            json!({"to":"codex-peer", "text":"PRIVATE-BODY", "from_mode":"bypass"}),
        ),
    ];
    for (field, value) in cases {
        let mut event = hook("bad", "default", "PRIVATE-BODY");
        event[field] = value;
        let error = forward(&socket, &event).unwrap_err().to_string();
        assert!(!error.contains("PRIVATE-BODY"));
        assert!(server
            .evidence()
            .take_for("codex-peer", "PRIVATE-BODY", true)
            .is_err());
    }
    assert!(forward_mode_hook(&b"not json"[..], &socket, "native-owned").is_err());
    assert!(forward_mode_hook(&vec![b' '; (1 << 20) + 1][..], &socket, "native-owned").is_err());
}

// Argument changes and two matching native calls cannot select whichever permission is easiest.
#[test]
fn rewritten_and_ambiguous_arguments_cannot_reuse_evidence() {
    let root = tempfile::tempdir().unwrap();
    let socket = root.path().join("mode.sock");
    let server = ModeEvidenceServer::listen(&socket, "native-owned").unwrap();
    let evidence = server.evidence();
    forward(&socket, &hook("rewritten", "bypassPermissions", "original")).unwrap();
    assert!(evidence.take_for("codex-peer", "rewritten", true).is_err());
    assert!(evidence.take_for("codex-peer", "original", true).is_err());
    forward(&socket, &hook("notify", "default", "notify arg")).unwrap();
    assert!(evidence
        .take_for("codex-peer", "notify arg", false)
        .is_err());
    forward(&socket, &hook("a", "default", "same")).unwrap();
    assert!(forward(&socket, &hook("b", "bypassPermissions", "same")).is_err());
    assert!(evidence.take_for("codex-peer", "same", true).is_err());
    assert!(evidence.take_for("codex-peer", "same", true).is_err());
}

// Closing must wake an accepted partial request and revoke evidence held by a tools clone.
#[test]
fn closing_evidence_socket_revokes_facts_and_wakes_partial_connections() {
    let root = tempfile::tempdir().unwrap();
    let socket = root.path().join("mode.sock");
    let server = ModeEvidenceServer::listen(&socket, "native-owned").unwrap();
    forward(&socket, &hook("waiting", "default", "unsent")).unwrap();
    let evidence = server.evidence();
    let mut partial = UnixStream::connect(&socket).unwrap();
    partial.write_all(b"{").unwrap();
    std::thread::sleep(Duration::from_millis(30));
    let start = Instant::now();
    server.close();
    server.close();
    assert!(start.elapsed() < Duration::from_secs(1));
    assert!(evidence.take_for("codex-peer", "unsent", true).is_err());
    assert!(!socket.exists());
}

// Static shared-workspace installation must preserve unrelated hooks and quote the executable.
#[test]
fn workspace_hook_install_is_static_merged_and_idempotent() {
    let root = tempfile::tempdir().unwrap();
    let hooks = root.path().join(".grok/hooks");
    fs::create_dir_all(&hooks).unwrap();
    let file = hooks.join("taskai-mail-mode.json");
    let unrelated = json!({"matcher":"Bash", "hooks":[{"type":"command","command":"other-hook"}]});
    fs::write(
        &file,
        json!({"hooks":{"PreToolUse":[unrelated.clone()],"SessionStart":[]},"extra":"keep"})
            .to_string(),
    )
    .unwrap();
    let bin = root.path().join("a 'quoted' taskai");
    install_mode_hook(root.path(), &bin).unwrap();
    let first = fs::read(&file).unwrap();
    install_mode_hook(root.path(), &bin).unwrap();
    assert_eq!(fs::read(&file).unwrap(), first);
    let value: Value = serde_json::from_slice(&first).unwrap();
    let entries = value["hooks"]["PreToolUse"].as_array().unwrap();
    assert_eq!(entries.len(), 2);
    assert_eq!(entries[0], unrelated);
    assert_eq!(entries[1]["matcher"], "^taskai__send_message$");
    assert_eq!(
        entries[1]["hooks"][0]["command"],
        format!(
            "'{}' grok-mode-hook",
            bin.display().to_string().replace('\'', "'\\''")
        )
    );
    assert_eq!(entries[1]["hooks"][0]["type"], "command");
    assert_eq!(value["extra"], "keep");
    assert!(value["hooks"]["SessionStart"].is_array());
    assert!(!String::from_utf8(first)
        .unwrap()
        .contains("TASKAI_MODE_SOCKET"));
    assert_eq!(
        fs::metadata(file).unwrap().permissions().mode() & 0o777,
        0o600
    );
}

// The production tools must consume the hook observation rather than a guessed startup mode.
#[test]
fn guarded_tools_send_with_native_snapshot_and_leave_directory_mode_unknown() {
    use std::sync::{mpsc, Arc, Mutex};
    use taskai_connectors::grok::{GrokTools, ToolHandler};
    use taskai_core::{
        mail::{Frame, Identity, MailPeer, Mailbox, MailboxConfig, SocketTransport, Transport},
        registry::{AddressBook, Registry, Source},
    };
    #[derive(Default)]
    struct Recorded(Mutex<Vec<Frame>>);
    impl Transport for Recorded {
        fn send(&self, to: &MailPeer, frame: &Frame) -> Result<(), taskai_core::Error> {
            self.0.lock().unwrap().push(frame.clone());
            SocketTransport.send(to, frame)
        }
    }
    let root = tempfile::tempdir().unwrap();
    let state = root.path().join("state");
    let mut registry = Registry::open(&state).unwrap();
    let task = registry
        .create_task("private", root.path(), Source::HumanUi)
        .unwrap();
    let owner = registry
        .create_session(&task, None, "Owner", "grok", root.path(), Source::HumanUi)
        .unwrap();
    let peer = registry
        .create_session(&task, None, "Peer", "codex", root.path(), Source::HumanUi)
        .unwrap();
    let config = |session_ref: &str| {
        let session = registry.session(session_ref).unwrap();
        MailboxConfig {
            identity: Identity {
                session_ref: session_ref.into(),
                task_ref: task.clone(),
                name: session.address.clone(),
                harness: session.harness.clone(),
                workspace: root.path().into(),
                mode: PermissionMode::Prompting,
            },
            mail_dir: root.path().join("mail"),
            log_path: state.join("messages/log.jsonl"),
        }
    };
    let book = Arc::new(AddressBook::new(&state, &task));
    let (tx, rx) = mpsc::channel();
    let peer_mail = Mailbox::open(
        config(&peer),
        book.clone(),
        Arc::new(SocketTransport),
        move |delivery| {
            let _ = tx.send(delivery);
        },
    )
    .unwrap();
    let route = Arc::new(Recorded::default());
    let owner_mail =
        Arc::new(Mailbox::open_unknown_mode(config(&owner), book, route.clone(), |_| {}).unwrap());
    let socket = root.path().join("mode.sock");
    let server = ModeEvidenceServer::listen(&socket, "native-owned").unwrap();
    let tools = GrokTools::with_mode_evidence(owner_mail.clone(), server.evidence());
    let result = tools
        .send_message(peer_mail.name(), "private letter", true)
        .unwrap();
    assert_eq!(result["status"], "send-failed");
    assert!(result["error"].as_str().unwrap().contains("mode evidence"));
    assert!(route.0.lock().unwrap().is_empty());
    let mut event = hook("guarded", "bypassPermissions", "private letter");
    event["toolInput"]["to"] = json!(peer_mail.name());
    forward(&socket, &event).unwrap();
    assert_eq!(
        tools
            .send_message(peer_mail.name(), "private letter", true)
            .unwrap()["status"],
        "sent"
    );
    assert!(rx
        .recv_timeout(Duration::from_secs(2))
        .unwrap()
        .text
        .contains("private letter"));
    let frames = route.0.lock().unwrap();
    assert!(
        matches!(&frames[0], Frame::Message(letter) if letter.from_mode == PermissionMode::Bypass)
    );
    assert!(matches!(&frames[1], Frame::Subscribe(sub) if sub.from_mode == PermissionMode::Bypass));
    drop(frames);
    registry.reload().unwrap();
    assert_eq!(
        registry
            .session(&owner)
            .unwrap()
            .messaging
            .as_ref()
            .unwrap()
            .mode,
        None
    );
    assert_eq!(
        tools
            .send_message(peer_mail.name(), "private letter", true)
            .unwrap()["status"],
        "send-failed"
    );
    assert_eq!(route.0.lock().unwrap().len(), 2);
    server.close();
    owner_mail.close();
    peer_mail.close();
}
