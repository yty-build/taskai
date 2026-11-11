// ABOUTME: Offline startup-RPC lifetime regression with an exact private tmux pane and fake daemon.
// ABOUTME: Native death after one written thread/start must wake the bridge before its RPC deadline.

use std::{
    fs,
    os::unix::net::UnixListener,
    process::{Command, Output},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

use serde_json::{json, Value};
use taskai_core::registry::{Registry, Source};
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

fn wait_for(limit: Duration, mut ready: impl FnMut() -> bool) -> bool {
    let deadline = Instant::now() + limit;
    while Instant::now() < deadline {
        if ready() {
            return true;
        }
        thread::sleep(Duration::from_millis(40));
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
fn native_death_wakes_written_startup_rpc_without_advertising_a_mailbox() {
    assert_startup_stop(false, false, false);
}

#[test]
fn sigterm_wakes_written_startup_rpc_without_advertising_a_mailbox() {
    assert_startup_stop(true, false, false);
}

#[test]
fn delayed_start_reply_after_native_death_never_advertises_a_mailbox() {
    assert_startup_stop(false, true, false);
}

#[test]
fn native_death_wakes_stalled_initialize_before_any_creation_intent() {
    assert_startup_stop(false, false, true);
}

fn assert_startup_stop(signal_bridge: bool, delayed_reply: bool, hold_initialize: bool) {
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
    let (written_tx, written_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let daemon = thread::spawn(move || {
        let (stream, _) = listener.accept().unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(8)))
            .unwrap();
        let mut ws = accept(stream).unwrap();
        let init: Value = serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap();
        assert_eq!(init["method"], "initialize");
        if hold_initialize {
            written_tx.send(()).unwrap();
            let _ = release_rx.recv_timeout(Duration::from_secs(8));
            return;
        }
        ws.send(Message::Text(
            json!({"id":init["id"], "result":{"userAgent":"codex-tui/0.167.0 (fake)"}}).to_string(),
        ))
        .unwrap();
        assert!(ws
            .read()
            .unwrap()
            .to_text()
            .unwrap()
            .contains("initialized"));
        let start: Value = serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap();
        assert_eq!(start["method"], "thread/start");
        written_tx.send(()).unwrap();
        let _ = release_rx.recv_timeout(Duration::from_secs(8));
        if delayed_reply {
            let _ = ws.send(Message::Text(
                json!({"id":start["id"], "result":{
                    "thread":{"id":"owned-thread", "status":{"type":"idle"}},
                    "approvalPolicy":"on-request"
                }})
                .to_string(),
            ));
        }
        drop(ws);
    });
    let state = root.path().join("state");
    let mut registry = Registry::open(&state).unwrap();
    let task = registry
        .create_task("Task", &workspace, Source::HumanUi)
        .unwrap();
    let session = registry
        .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
        .unwrap();
    assert_eq!(session, "s1");
    let server = format!(
        "taskai-codex-startup-{}-{}",
        std::process::id(),
        if signal_bridge {
            "term"
        } else if hold_initialize {
            "init"
        } else if delayed_reply {
            "late"
        } else {
            "death"
        }
    );
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
    written_rx.recv_timeout(Duration::from_secs(5)).unwrap();
    if !hold_initialize {
        let pending: Value = serde_json::from_slice(
            &fs::read(root.path().join("run/codex/s1.creation.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(pending["kind"], "creation");
        assert!(pending["threadId"].is_null());
    }
    let killed_at = Instant::now();
    if signal_bridge {
        let pid = tmux_ok(
            &server,
            &["list-panes", "-t", "=s1:1.0", "-F", "#{pane_pid}"],
        );
        let mut kill = Command::new("kill");
        kill.args(["-TERM", &pid]);
        taskai_core::mail::strip_messaging_env(&mut kill);
        assert!(kill.status().unwrap().success());
    } else {
        tmux_ok(&server, &["kill-window", "-t", "=s1:0"]);
    }
    if delayed_reply {
        release_tx.send(()).unwrap();
    }
    let exited = wait_for(Duration::from_secs(2), || {
        tmux_ok(
            &server,
            &["list-panes", "-t", "=s1:1.0", "-F", "#{pane_dead}"],
        ) == "1"
    });
    let elapsed = killed_at.elapsed();
    if !delayed_reply {
        release_tx.send(()).unwrap();
    }
    daemon.join().unwrap();
    assert!(exited, "the bridge waited for the startup RPC deadline");
    assert!(elapsed < Duration::from_secs(2));
    registry.reload().unwrap();
    let saved = registry.session(&session).unwrap();
    assert!(saved.provider.is_none() && saved.messaging.is_none());
    if hold_initialize {
        assert!(!root.path().join("run/codex/s1.creation.json").exists());
    } else {
        let after: Value = serde_json::from_slice(
            &fs::read(root.path().join("run/codex/s1.creation.json")).unwrap(),
        )
        .unwrap();
        assert!(
            after["threadId"].is_null() || after["threadId"] == "owned-thread",
            "the sole creation intent cannot name a replacement thread"
        );
    }
}
