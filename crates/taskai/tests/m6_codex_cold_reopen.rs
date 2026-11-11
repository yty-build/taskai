// ABOUTME: Offline cold reopen of a saved zero-turn Codex thread through a second native gate.
// ABOUTME: A fake TUI refuses empty direct resume; no installed provider or model turn is used.

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
use tungstenite::{accept, Message, WebSocket};

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

fn recv(ws: &mut WebSocket<std::os::unix::net::UnixStream>) -> Value {
    serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap()
}

fn send(ws: &mut WebSocket<std::os::unix::net::UnixStream>, value: Value) {
    ws.send(Message::Text(value.to_string())).unwrap();
}

#[test]
fn cold_reopen_uses_saved_thread_and_second_human_gate() {
    if Command::new("tmux").arg("-V").output().is_err() {
        return;
    }
    let root = TempDir::new().unwrap();
    let workspace = root.path().join("workspace");
    fs::create_dir(&workspace).unwrap();
    let codex_home = root.path().join("codex-home");
    fs::create_dir_all(codex_home.join("app-server-control")).unwrap();
    let listener =
        UnixListener::bind(codex_home.join("app-server-control/app-server-control.sock")).unwrap();
    let (reopen_tx, reopen_rx) = mpsc::channel();
    let (turn_tx, turn_rx) = mpsc::channel();
    let (stop_tx, stop_rx) = mpsc::channel();
    let daemon = thread::spawn(move || {
        let handshake = |listener: &UnixListener| {
            let (stream, _) = listener.accept().unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(8)))
                .unwrap();
            let mut ws = accept(stream).unwrap();
            let init = recv(&mut ws);
            assert_eq!(init["method"], "initialize");
            send(
                &mut ws,
                json!({"id":init["id"], "result":{"userAgent":"codex-tui/0.167.0 (fake)"}}),
            );
            assert_eq!(recv(&mut ws)["method"], "initialized");
            ws
        };
        let mut first = handshake(&listener);
        let start = recv(&mut first);
        assert_eq!(start["method"], "thread/start");
        send(
            &mut first,
            json!({"id":start["id"], "result":{
                "thread":{"id":"owned-thread", "status":{"type":"idle"}},
                "approvalPolicy":"on-request"
            }}),
        );
        reopen_rx.recv_timeout(Duration::from_secs(8)).unwrap();
        drop(first);
        let mut second = handshake(&listener);
        let resume = recv(&mut second);
        assert_eq!(
            resume["method"], "thread/resume",
            "cold reopen must not create another thread"
        );
        assert_eq!(resume["params"]["threadId"], "owned-thread");
        send(
            &mut second,
            json!({"id":resume["id"], "result":{
                "thread":{"id":"owned-thread", "status":{"type":"idle"}},
                "approvalPolicy":"on-request"
            }}),
        );
        let turn = recv(&mut second);
        assert_eq!(turn["method"], "turn/start");
        assert_eq!(turn["params"]["threadId"], "owned-thread");
        turn_tx
            .send(turn["params"]["input"][0]["text"] == "PRIVATE_COLD_FIRST")
            .unwrap();
        send(
            &mut second,
            json!({"id":turn["id"], "result":{"turn":{"id":"first-turn"}}}),
        );
        let _ = stop_rx.recv_timeout(Duration::from_secs(8));
    });
    let provider_started = root.path().join("provider-started");
    let early_resume = root.path().join("early-resume");
    let fake_tui = root.path().join("fake-codex");
    fs::write(&fake_tui, format!(
        "#!/bin/sh\n[ \"$1\" = resume ] && [ \"$2\" = owned-thread ] || exit 3\nif ! /usr/bin/grep -q '\"accepted\":true' '{}/run/codex/s1.first-turn.json' 2>/dev/null; then : > '{}'; exit 9; fi\n: > '{}'\nexec sleep 2\n",
        root.path().display(), early_resume.display(), provider_started.display()
    )).unwrap();
    fs::set_permissions(&fake_tui, fs::Permissions::from_mode(0o755)).unwrap();
    let state = root.path().join("state");
    let mut registry = Registry::open(&state).unwrap();
    registry
        .set_server_harness(
            "local",
            "codex",
            HarnessSettings {
                enabled: true,
                executable: Some(fake_tui.display().to_string()),
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
    let server = format!("taskai-codex-cold-test-{}", std::process::id());
    registry.bind_runtime(&session, &server).unwrap();
    let _guard = SessionGuard(server.clone());
    let taskai = env!("CARGO_BIN_EXE_taskai");
    let start_native_gate = || {
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
        )
    };
    let start_bridge = || {
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
        )
    };
    start_native_gate();
    tmux_ok(
        &server,
        &["set-window-option", "-g", "remain-on-exit", "on"],
    );
    start_bridge();
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
    assert!(wait_for(|| tmux_ok(
        &server,
        &["capture-pane", "-p", "-t", "=s1:0.0"]
    )
    .contains("type your first message")));
    let old_creation: Value =
        serde_json::from_slice(&fs::read(root.path().join("run/codex/s1.creation.json")).unwrap())
            .unwrap();
    let old_gate: Value =
        serde_json::from_slice(&fs::read(root.path().join("run/codex/s1.gate.json")).unwrap())
            .unwrap();
    for field in ["sessionRef", "paneId", "pid", "procStart"] {
        assert_eq!(old_creation[field], old_gate[field]);
    }
    tmux_ok(&server, &["send-keys", "-t", "=s1:0.0", "C-d"]);
    assert!(wait_for(|| tmux_ok(
        &server,
        &["list-panes", "-t", "=s1:0.0", "-F", "#{pane_dead}"]
    ) == "1"));
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry.session(&session).unwrap().messaging.is_none()
    }));
    assert!(!root.path().join("run/codex/s1.first-turn.json").exists());
    assert!(!provider_started.exists() && !early_resume.exists());
    // The launch path stages this metadata-only adoption after checking the old exact gate.
    taskai_core::write_private(
        &root.path().join("run/codex/s1.adoption.json"),
        &json!({
            "sessionRef":"s1", "kind":"gate-adoption", "threadId":"owned-thread",
            "source":old_creation,
        })
        .to_string(),
    )
    .unwrap();
    tmux_ok(&server, &["kill-session", "-t", "=s1"]);
    reopen_tx.send(()).unwrap();
    start_native_gate();
    tmux_ok(
        &server,
        &["set-window-option", "-g", "remain-on-exit", "on"],
    );
    start_bridge();
    tmux_ok(&server, &["select-window", "-t", "=s1:0"]);
    assert!(wait_for(|| tmux_ok(
        &server,
        &["capture-pane", "-p", "-t", "=s1:0.0"]
    )
    .contains("type your first message")));
    assert!(
        !early_resume.exists(),
        "a zero-turn direct resume was attempted"
    );
    let adopted: Value =
        serde_json::from_slice(&fs::read(root.path().join("run/codex/s1.creation.json")).unwrap())
            .unwrap();
    assert_eq!(adopted["threadId"], "owned-thread");
    assert_ne!(adopted["pid"], old_gate["pid"]);
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
    viewer.write_bytes(b"PRIVATE_COLD_FIRST\r").unwrap();
    assert!(turn_rx.recv_timeout(Duration::from_secs(8)).unwrap());
    assert!(wait_for(|| provider_started.exists()));
    assert!(!early_resume.exists());
    let first_intent: Value = serde_json::from_slice(
        &fs::read(root.path().join("run/codex/s1.first-turn.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(first_intent["accepted"], true);
    assert_eq!(first_intent["pid"], adopted["pid"]);
    let _ = stop_tx.send(());
    daemon.join().unwrap();
    drop(viewer);
}
