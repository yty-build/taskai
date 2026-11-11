// ABOUTME: Offline Codex gate and bridge over a private tmux server and fake daemon.
// ABOUTME: Verifies one tool-enabled thread, one human turn, exact native resume and cleanup.

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
fn fresh_codex_gate_persists_one_thread_before_one_human_turn() {
    if Command::new("tmux").arg("-V").output().is_err() {
        return;
    }
    // The daemon's fixed socket suffix exceeds macOS SUN_LEN under its long TMPDIR.
    let root = TempDir::new_in("/tmp").unwrap();
    let workspace = root.path().join("workspace");
    fs::create_dir(&workspace).unwrap();
    let codex_home = root.path().join("codex-home");
    fs::create_dir_all(codex_home.join("app-server-control")).unwrap();
    let socket = codex_home.join("app-server-control/app-server-control.sock");
    let listener = UnixListener::bind(&socket).unwrap();
    let (turn_tx, turn_rx) = mpsc::channel();
    let (bypass_tx, bypass_rx) = mpsc::channel();
    let (continue_tx, continue_rx) = mpsc::channel();
    let (policy_tx, policy_rx) = mpsc::channel();
    let (restart_tx, restart_rx) = mpsc::channel();
    let (stop_tx, stop_rx) = mpsc::channel();
    let daemon = thread::spawn(move || {
        let (stream, _) = listener.accept().unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(8)))
            .unwrap();
        let mut ws = accept(stream).unwrap();
        let init = recv(&mut ws);
        assert_eq!(init["method"], "initialize");
        send(
            &mut ws,
            json!({"id": init["id"], "result":{"userAgent":"codex-tui/0.167.0 (fake)"}}),
        );
        assert_eq!(recv(&mut ws)["method"], "initialized");
        let start = recv(&mut ws);
        assert_eq!(start["method"], "thread/start");
        assert_eq!(start["params"]["dynamicTools"][0]["name"], "taskai");
        send(
            &mut ws,
            json!({"id":start["id"], "result":{
                "thread":{"id":"owned-thread", "status":{"type":"idle"}},
                "approvalPolicy":"on-request"
            }}),
        );
        restart_rx.recv_timeout(Duration::from_secs(8)).unwrap();
        drop(ws);
        let (stream, _) = listener.accept().unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(8)))
            .unwrap();
        let mut ws = accept(stream).unwrap();
        let init = recv(&mut ws);
        assert_eq!(init["method"], "initialize");
        send(
            &mut ws,
            json!({"id": init["id"], "result":{"userAgent":"codex-tui/0.167.0 (fake)"}}),
        );
        assert_eq!(recv(&mut ws)["method"], "initialized");
        let resume = recv(&mut ws);
        assert_eq!(
            resume["method"], "thread/resume",
            "the owned thread must not be recreated"
        );
        assert_eq!(resume["params"]["threadId"], "owned-thread");
        send(
            &mut ws,
            json!({"id":resume["id"], "result":{
                "thread":{"id":"owned-thread", "status":{"type":"idle"}},
                "approvalPolicy":"on-request"
            }}),
        );
        let turn = recv(&mut ws);
        assert_eq!(turn["method"], "turn/start");
        assert_eq!(turn["params"]["threadId"], "owned-thread");
        let text_is_exact = turn["params"]["input"][0]["text"] == "PRIVATE_FIRST_TURN";
        send(
            &mut ws,
            json!({"id":turn["id"], "result":{"turn":{"id":"first-turn"}}}),
        );
        turn_tx.send(text_is_exact).unwrap();
        send(
            &mut ws,
            json!({"method":"thread/settings/updated","params":{
                "threadId":"foreign-thread","threadSettings":{}
            }}),
        );
        send(
            &mut ws,
            json!({"id":91,"method":"item/tool/call","params":{
                "threadId":"owned-thread","turnId":"first-turn","namespace":"taskai",
                "tool":"send_message","arguments":{"to":"missing","text":"before mode loss"}
            }}),
        );
        let known = recv(&mut ws);
        let known_body: Value =
            serde_json::from_str(known["result"]["contentItems"][0]["text"].as_str().unwrap())
                .unwrap();
        assert_eq!(known["id"], 91);
        assert_eq!(known_body["status"], "no-such-session");
        send(
            &mut ws,
            json!({"method":"thread/settings/updated","params":{
                "threadId":"owned-thread","threadSettings":{"approvalPolicy":"never"}
            }}),
        );
        send(
            &mut ws,
            json!({"id":92,"method":"item/tool/call","params":{
                "threadId":"owned-thread","turnId":"first-turn","namespace":"taskai",
                "tool":"send_message","arguments":{"to":"missing","text":"after bypass"}
            }}),
        );
        let bypass = recv(&mut ws);
        let bypass_body: Value = serde_json::from_str(
            bypass["result"]["contentItems"][0]["text"]
                .as_str()
                .unwrap(),
        )
        .unwrap();
        assert_eq!(bypass["id"], 92);
        assert_eq!(bypass_body["status"], "no-such-session");
        bypass_tx.send(()).unwrap();
        continue_rx.recv_timeout(Duration::from_secs(8)).unwrap();
        send(
            &mut ws,
            json!({"method":"thread/settings/updated","params":{
                "threadId":"owned-thread","threadSettings":{}
            }}),
        );
        send(
            &mut ws,
            json!({"id":93,"method":"item/tool/call","params":{
                "threadId":"owned-thread","turnId":"first-turn","namespace":"taskai",
                "tool":"send_message","arguments":{"to":"missing","text":"after mode loss"}
            }}),
        );
        let unknown = recv(&mut ws);
        let unknown_body: Value = serde_json::from_str(
            unknown["result"]["contentItems"][0]["text"]
                .as_str()
                .unwrap(),
        )
        .unwrap();
        assert_eq!(unknown["id"], 93);
        assert_eq!(unknown_body["status"], "send-failed");
        assert!(unknown_body["error"]
            .as_str()
            .unwrap()
            .contains("permission mode is unknown"));
        policy_tx.send(()).unwrap();
        let _ = stop_rx.recv_timeout(Duration::from_secs(8));
    });
    let fake_codex = root.path().join("fake-codex");
    let provider_started = root.path().join("provider-started");
    fs::write(
        &fake_codex,
        format!(
            "#!/bin/sh\n[ \"$1\" = resume ] && [ \"$2\" = owned-thread ] || exit 3\n: > '{}'\nsleep 4\n",
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
    let server = format!("taskai-codex-bridge-test-{}", std::process::id());
    registry.bind_runtime(&session, &server).unwrap();
    let _guard = SessionGuard(server.clone());
    let taskai = env!("CARGO_BIN_EXE_taskai");
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
        );
    };
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
    start_bridge();
    tmux_ok(&server, &["select-window", "-t", "=s1:0"]);
    assert!(
        wait_for(|| {
            registry.reload().unwrap();
            registry
                .session(&session)
                .unwrap()
                .provider
                .as_ref()
                .is_some_and(|mapping| mapping.provider_session_id == "owned-thread")
        }),
        "bridge did not save its exact thread"
    );
    assert!(wait_for(|| root
        .path()
        .join("run/codex/s1.gate.sock")
        .exists()));
    assert!(
        wait_for(|| {
            registry.reload().unwrap();
            registry.session(&session).unwrap().messaging.is_some()
        }),
        "bridge did not publish its mailbox"
    );
    let native_command = tmux_ok(
        &server,
        &["list-panes", "-t", "=s1:0.0", "-F", "#{pane_start_command}"],
    );
    assert!(!native_command.contains("PRIVATE_FIRST_TURN"));
    let viewer = Pane::spawn(
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
    drop(viewer);
    assert_eq!(
        tmux_ok(
            &server,
            &["list-panes", "-t", "=s1:0.0", "-F", "#{pane_dead}"]
        ),
        "0"
    );
    assert_eq!(
        tmux_ok(
            &server,
            &["list-panes", "-t", "=s1:1.0", "-F", "#{pane_dead}"]
        ),
        "0"
    );
    assert!(
        wait_for(|| {
            tmux_ok(&server, &["capture-pane", "-p", "-t", "=s1:0.0"])
                .contains("type your first message")
        }),
        "the gate never reached its first-human input wait"
    );
    let first_bridge_pid: u32 = tmux_ok(
        &server,
        &["list-panes", "-t", "=s1:1.0", "-F", "#{pane_pid}"],
    )
    .parse()
    .unwrap();
    tmux_ok(&server, &["kill-window", "-t", "=s1:1"]);
    assert!(wait_for(
        || taskai_core::mail::proc_start(first_bridge_pid).is_none()
    ));
    restart_tx.send(()).unwrap();
    start_bridge();
    tmux_ok(&server, &["select-window", "-t", "=s1:0"]);
    assert!(
        wait_for(|| {
            registry.reload().unwrap();
            registry
                .session(&session)
                .unwrap()
                .messaging
                .as_ref()
                .is_some_and(|entry| entry.pid != first_bridge_pid)
        }),
        "replacement bridge did not republish its exact mailbox"
    );
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
    viewer.write_bytes(b"PRIVATE_FIRST_TURN\r").unwrap();
    assert!(
        turn_rx.recv_timeout(Duration::from_secs(8)).unwrap(),
        "daemon did not receive exact first text"
    );
    bypass_rx.recv_timeout(Duration::from_secs(8)).unwrap();
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry
            .session(&session)
            .unwrap()
            .messaging
            .as_ref()
            .is_some_and(|entry| entry.mode == Some(taskai_core::mail::PermissionMode::Bypass))
    }));
    continue_tx.send(()).unwrap();
    policy_rx.recv_timeout(Duration::from_secs(8)).unwrap();
    registry.reload().unwrap();
    assert_eq!(
        registry
            .session(&session)
            .unwrap()
            .messaging
            .as_ref()
            .unwrap()
            .mode,
        None
    );
    assert!(
        wait_for(|| provider_started.exists()),
        "gate did not resume the exact native thread"
    );
    assert!(wait_for(|| root
        .path()
        .join("run/codex/s1.provider.json")
        .exists()));
    let child: Value =
        serde_json::from_slice(&fs::read(root.path().join("run/codex/s1.provider.json")).unwrap())
            .unwrap();
    let pane_pid: u32 = tmux_ok(
        &server,
        &["list-panes", "-t", "=s1:0.0", "-F", "#{pane_pid}"],
    )
    .parse()
    .unwrap();
    assert_ne!(child["providerPid"], pane_pid);
    let creation: Value =
        serde_json::from_slice(&fs::read(root.path().join("run/codex/s1.creation.json")).unwrap())
            .unwrap();
    let first: Value = serde_json::from_slice(
        &fs::read(root.path().join("run/codex/s1.first-turn.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(creation["threadId"], "owned-thread");
    assert_eq!(first["accepted"], true);
    assert!(!fs::read(root.path().join("run/codex/s1.first-turn.json"))
        .unwrap()
        .windows(b"PRIVATE_FIRST_TURN".len())
        .any(|part| part == b"PRIVATE_FIRST_TURN"));
    let message_log = state.join("messages").join(format!("{task}.jsonl"));
    if let Ok(log) = fs::read(&message_log) {
        assert!(!log
            .windows(b"PRIVATE_FIRST_TURN".len())
            .any(|part| part == b"PRIVATE_FIRST_TURN"));
    }
    assert!(
        wait_for(|| {
            registry.reload().unwrap();
            registry.session(&session).unwrap().messaging.is_none()
        }),
        "native child exit did not close the mailbox"
    );
    let _ = stop_tx.send(());
    daemon.join().unwrap();
    drop(viewer);
}
