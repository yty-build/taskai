// ABOUTME: Offline lost-ack first-human Codex bridge test with private tmux and fake daemon.
// ABOUTME: Verifies one written turn stays pending and never launches an unproven native resume.

use std::{
    fs,
    os::unix::{fs::PermissionsExt, net::UnixListener},
    process::{Command, Output},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

use serde_json::{json, Value};
use taskai_core::registry::{HarnessSettings, Registry, Source};
use taskai_pane::Pane;
use tempfile::TempDir;
use tungstenite::{accept, Message};

fn tmux(server: &str, args: &[&str]) -> Output {
    let mut command = Command::new("tmux");
    command.args(["-f", "/dev/null", "-L", server]).args(args);
    taskai_core::mail::strip_messaging_env(&mut command);
    command.output().unwrap()
}

fn tmux_ok(server: &str, args: &[&str]) -> String {
    let output = tmux(server, args);
    assert!(output.status.success(), "private tmux command failed");
    String::from_utf8(output.stdout).unwrap().trim().into()
}

fn wait_for(mut ready: impl FnMut() -> bool) -> bool {
    let deadline = Instant::now() + Duration::from_secs(8);
    while Instant::now() < deadline {
        if ready() {
            return true;
        }
        thread::sleep(Duration::from_millis(50));
    }
    false
}

struct SessionGuard(String);
impl Drop for SessionGuard {
    fn drop(&mut self) {
        let _ = tmux(&self.0, &["kill-session", "-t", "=s1"]);
    }
}

#[test]
fn lost_first_turn_ack_keeps_exact_thread_and_never_resumes_native_child() {
    if Command::new("tmux").arg("-V").output().is_err() {
        return;
    }
    let root = TempDir::new().unwrap();
    let workspace = root.path().join("workspace");
    fs::create_dir(&workspace).unwrap();
    let codex_home = root.path().join("codex-home");
    fs::create_dir_all(codex_home.join("app-server-control")).unwrap();
    let socket = codex_home.join("app-server-control/app-server-control.sock");
    let listener = UnixListener::bind(&socket).unwrap();
    let (written_tx, written_rx) = mpsc::channel();
    let daemon = thread::spawn(move || {
        let (stream, _) = listener.accept().unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(8)))
            .unwrap();
        let mut ws = accept(stream).unwrap();
        let init: Value = serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap();
        assert_eq!(init["method"], "initialize");
        ws.send(Message::Text(
            json!({
                "id": init["id"], "result": {"userAgent": "codex-tui/0.167.0 (fake)"}
            })
            .to_string(),
        ))
        .unwrap();
        let initialized: Value =
            serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap();
        assert_eq!(initialized["method"], "initialized");
        let start: Value = serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap();
        assert_eq!(start["method"], "thread/start");
        ws.send(Message::Text(
            json!({
                "id":start["id"], "result":{
                    "thread":{"id":"owned-thread", "status":{"type":"idle"}},
                    "approvalPolicy":"on-request"
                }
            })
            .to_string(),
        ))
        .unwrap();
        let turn: Value = serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap();
        assert_eq!(turn["method"], "turn/start");
        assert_eq!(turn["params"]["threadId"], "owned-thread");
        written_tx
            .send(turn["params"]["input"][0]["text"] == "PRIVATE_UNCERTAIN_FIRST")
            .unwrap();
        // A lost acknowledgment has no result frame. Closing this connection proves only that
        // the write happened; it cannot prove whether Codex accepted the turn.
    });
    let fake_codex = root.path().join("fake-codex");
    let provider_started = root.path().join("provider-started");
    fs::write(
        &fake_codex,
        format!(
            "#!/bin/sh\n: > '{}'\nexec sleep 30\n",
            provider_started.display()
        ),
    )
    .unwrap();
    fs::set_permissions(&fake_codex, fs::Permissions::from_mode(0o755)).unwrap();
    let state = root.path().join("state");
    let mut registry = Registry::open(&state).unwrap();
    registry
        .set_server_harness(
            "local",
            "codex",
            HarnessSettings {
                enabled: true,
                executable: Some(fake_codex.display().to_string()),
                model: None,
            },
            Source::HumanUi,
        )
        .unwrap();
    let task = registry
        .create_task("Task", &workspace, Source::HumanUi)
        .unwrap();
    let session = registry
        .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
        .unwrap();
    assert_eq!(session, "s1");
    let server = format!("taskai-codex-uncertain-test-{}", std::process::id());
    registry.bind_runtime(&session, &server).unwrap();
    let _guard = SessionGuard(server.clone());
    let taskai = env!("CARGO_BIN_EXE_taskai");
    tmux_ok(
        &server,
        &[
            "new-session",
            "-d",
            "-s",
            "s1",
            "-n",
            "native",
            "-c",
            workspace.to_str().unwrap(),
            "-e",
            &format!("TASKAI_HOME={}", root.path().display()),
            "-e",
            "TASKAI_SESSION_REF=s1",
            "--",
            taskai,
            "gate",
            "s1",
        ],
    );
    tmux_ok(
        &server,
        &["set-window-option", "-g", "remain-on-exit", "on"],
    );
    tmux_ok(
        &server,
        &[
            "new-window",
            "-d",
            "-t",
            "=s1:1",
            "-n",
            "bridge",
            "-c",
            workspace.to_str().unwrap(),
            "-e",
            &format!("TASKAI_HOME={}", root.path().display()),
            "-e",
            "TASKAI_SESSION_REF=s1",
            "-e",
            &format!("CODEX_HOME={}", codex_home.display()),
            "-e",
            &format!("GROK_HOME={}", root.path().join("grok-home").display()),
            "-e",
            &format!(
                "TASKAI_CLAUDE_BIN={}",
                root.path().join("absent-claude").display()
            ),
            "-e",
            &format!(
                "CLAUDE_CONFIG_DIR={}",
                root.path().join("claude-home").display()
            ),
            "--",
            taskai,
            "bridge",
            "s1",
        ],
    );
    tmux_ok(&server, &["select-window", "-t", "=s1:0"]);
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry
            .session(&session)
            .unwrap()
            .provider
            .as_ref()
            .is_some_and(|mapping| mapping.provider_session_id == "owned-thread")
    }));
    assert!(wait_for(|| {
        tmux_ok(&server, &["capture-pane", "-p", "-t", "=s1:0.0"])
            .contains("type your first message")
    }));
    let mut viewer = Pane::spawn(
        "tmux",
        &[
            "-f",
            "/dev/null",
            "-L",
            &server,
            "attach-session",
            "-t",
            "=s1:0",
        ],
        &workspace,
        24,
        80,
        &[],
    )
    .unwrap();
    viewer.write_bytes(b"PRIVATE_UNCERTAIN_FIRST\r").unwrap();
    assert!(written_rx.recv_timeout(Duration::from_secs(8)).unwrap());
    daemon.join().unwrap();
    assert!(wait_for(|| {
        let first = root.path().join("run/codex/s1.first-turn.json");
        fs::read(&first)
            .ok()
            .and_then(|bytes| serde_json::from_slice::<Value>(&bytes).ok())
            .is_some_and(|intent| {
                intent["threadId"] == "owned-thread" && intent["accepted"] == false
            })
    }));
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry.session(&session).unwrap().messaging.is_none()
    }));
    assert!(wait_for(|| {
        tmux_ok(
            &server,
            &["list-panes", "-t", "=s1:0.0", "-F", "#{pane_dead}"],
        ) == "1"
    }));
    registry.reload().unwrap();
    assert_eq!(
        registry
            .session(&session)
            .unwrap()
            .provider
            .as_ref()
            .unwrap()
            .provider_session_id,
        "owned-thread"
    );
    assert!(
        !provider_started.exists(),
        "uncertain first turn launched a native child"
    );
    assert!(!root.path().join("run/codex/s1.provider.json").exists());
    let first_intent = fs::read(root.path().join("run/codex/s1.first-turn.json")).unwrap();
    assert!(!first_intent
        .windows(b"PRIVATE_UNCERTAIN_FIRST".len())
        .any(|part| part == b"PRIVATE_UNCERTAIN_FIRST"));
    drop(viewer);
}
