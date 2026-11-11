// ABOUTME: Offline native Claude observer lifecycle with a fake pane and private tmux server.
// ABOUTME: No installed provider, model turn, native key, or TaskAI viewer is started.

use std::{
    fs,
    os::unix::{fs::PermissionsExt, net::UnixListener},
    path::Path,
    process::{Command, Output},
    thread,
    time::{Duration, Instant},
};

use taskai_core::{
    mail::{proc_start, Activity, Directory, MailPeer},
    registry::{AddressBook, HarnessSettings, Registry, Source},
};
use tempfile::TempDir;

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

fn write_card(path: &Path, pid: u32, start: &str, socket: &Path, status: &str, mode: Option<&str>) {
    fs::write(
        path,
        serde_json::json!({
            "pid": pid,
            "sessionId": "fake-claude",
            "startedAt": 1,
            "kind": "interactive",
            "procStart": start,
            "peerProtocol": 1,
            "peerFeatures": ["notify_idle"],
            "messagingSocketPath": socket,
            "status": status,
            "waitingFor": if status == "waiting" {Some("approval")} else {None},
            "permissionMode": mode,
        })
        .to_string(),
    )
    .unwrap();
}

#[test]
fn detached_claude_observer_registers_refreshes_and_clears_owned_metadata() {
    if Command::new("tmux").arg("-V").output().is_err() {
        return;
    }
    let root = TempDir::new().unwrap();
    let workspace = root.path().join("workspace");
    let claude = root.path().join("claude");
    let cards = claude.join("sessions");
    fs::create_dir_all(&workspace).unwrap();
    fs::create_dir_all(&cards).unwrap();
    let fake_bin = root.path().join("fake-claude");
    let probed = root.path().join("configured-probed");
    fs::write(
        &fake_bin,
        format!(
            "#!/bin/sh\n: > '{}'\nprintf 'claude 2.1.322\\n'\n",
            probed.display()
        ),
    )
    .unwrap();
    fs::set_permissions(&fake_bin, fs::Permissions::from_mode(0o755)).unwrap();
    let wrong_bin = root.path().join("wrong-default-claude");
    let wrong_probed = root.path().join("wrong-default-probed");
    fs::write(
        &wrong_bin,
        format!(
            "#!/bin/sh\n: > '{}'\nprintf 'claude 0.0.0\\n'\n",
            wrong_probed.display()
        ),
    )
    .unwrap();
    fs::set_permissions(&wrong_bin, fs::Permissions::from_mode(0o755)).unwrap();
    let real_tmux = std::env::split_paths(&std::env::var_os("PATH").unwrap())
        .map(|dir| dir.join("tmux"))
        .find(|path| path.is_file())
        .unwrap();
    let shim_dir = root.path().join("shim");
    fs::create_dir(&shim_dir).unwrap();
    let fail_marker = root.path().join("fail-observation");
    let shim_calls = root.path().join("shim-calls");
    let shim = shim_dir.join("tmux");
    fs::write(
        &shim,
        format!(
            "#!/bin/sh\nprintf x >> '{}'\nif [ -e '{}' ]; then exit 9; fi\nexec '{}' \"$@\"\n",
            shim_calls.display(),
            fail_marker.display(),
            real_tmux.display()
        ),
    )
    .unwrap();
    fs::set_permissions(&shim, fs::Permissions::from_mode(0o755)).unwrap();
    let server = format!("taskai-observer-test-{}", std::process::id());
    let _guard = SessionGuard(server.clone());
    let state = root.path().join("state");
    let mut registry = Registry::open(&state).unwrap();
    registry
        .set_server_harness(
            "local",
            "claude",
            HarnessSettings {
                enabled: true,
                executable: Some(fake_bin.display().to_string()),
                model: None,
            },
            Source::HumanUi,
        )
        .unwrap();
    let task = registry
        .create_task("Task", &workspace, Source::HumanUi)
        .unwrap();
    let session = registry
        .create_session(&task, None, "Claude", "claude", &workspace, Source::HumanUi)
        .unwrap();
    assert_eq!(session, "s1");
    registry.bind_runtime(&session, &server).unwrap();
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
            "--",
            "/bin/sh",
            "-c",
            "while :; do sleep 1; done",
        ],
    );
    tmux_ok(
        &server,
        &["set-window-option", "-g", "remain-on-exit", "on"],
    );
    let pid: u32 = tmux_ok(
        &server,
        &["list-panes", "-t", "=s1:0.0", "-F", "#{pane_pid}"],
    )
    .parse()
    .unwrap();
    let start = proc_start(pid).unwrap();
    let socket = root.path().join("fake-claude.sock");
    let _listener = UnixListener::bind(&socket).unwrap();
    let card_path = cards.join(format!("{pid}.json"));
    let taskai = env!("CARGO_BIN_EXE_taskai");
    let mut observer = Command::new("tmux");
    observer.args([
        "-f",
        "/dev/null",
        "-L",
        &server,
        "new-window",
        "-d",
        "-t",
        "=s1:1",
        "-n",
        "observer",
        "-c",
        workspace.to_str().unwrap(),
    ]);
    let codex_home = root.path().join("codex-home");
    let grok_home = root.path().join("grok-home");
    for (key, value) in [
        ("TASKAI_HOME", root.path().to_str().unwrap()),
        ("TASKAI_SESSION_REF", "s1"),
        ("CLAUDE_CONFIG_DIR", claude.to_str().unwrap()),
        ("TASKAI_CLAUDE_BIN", wrong_bin.to_str().unwrap()),
        ("CODEX_HOME", codex_home.to_str().unwrap()),
        ("GROK_HOME", grok_home.to_str().unwrap()),
    ] {
        observer.args(["-e", &format!("{key}={value}")]);
    }
    observer.args([
        "--",
        "/usr/bin/env",
        &format!(
            "PATH={}:{}",
            shim_dir.display(),
            std::env::var("PATH").unwrap()
        ),
        taskai,
        "observer",
        "s1",
    ]);
    taskai_core::mail::strip_messaging_env(&mut observer);
    assert!(observer.output().unwrap().status.success());
    thread::sleep(Duration::from_millis(150));
    registry.reload().unwrap();
    assert!(registry.live_sessions(&task).is_empty(), "no card yet");
    write_card(&card_path, pid, &start, &socket, "idle", None);
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry
            .live_sessions(&task)
            .first()
            .is_some_and(|peer| peer.status == Activity::Idle && peer.mode.is_none())
    }));
    assert!(
        wait_for(|| probed.exists()),
        "the saved executable was probed; observer: {}; pane: {}",
        tmux_ok(&server, &["capture-pane", "-p", "-t", "=s1:1.0"]),
        tmux_ok(
            &server,
            &[
                "list-panes",
                "-t",
                "=s1:1.0",
                "-F",
                "#{pane_dead} #{pane_current_command}"
            ]
        )
    );
    assert!(
        !wrong_probed.exists(),
        "the PATH/default executable was never probed"
    );
    assert!(
        shim_calls.exists(),
        "observer tmux control uses its private PATH"
    );
    // A field outside the native card contract cannot assert current mode.
    write_card(
        &card_path,
        pid,
        &start,
        &socket,
        "idle",
        Some("bypassPermissions"),
    );
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry
            .live_sessions(&task)
            .first()
            .is_some_and(|peer| peer.status == Activity::Idle && peer.mode.is_none())
    }));
    write_card(&card_path, pid, &start, &socket, "waiting", Some("default"));
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry.live_sessions(&task).first().is_some_and(|peer| {
            peer.waiting_for.as_deref() == Some("approval") && peer.mode.is_none()
        })
    }));
    fs::remove_file(&card_path).unwrap();
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry.session(&session).unwrap().messaging.is_none()
    }));
    write_card(&card_path, pid, &start, &socket, "idle", Some("default"));
    assert!(wait_for(|| {
        registry.reload().unwrap();
        !registry.live_sessions(&task).is_empty()
    }));
    // A tmux observation error is reported by the child; the native shell and
    // card remain, while its exact TaskAI metadata is retracted.
    fs::write(&fail_marker, b"fail").unwrap();
    let cleared = wait_for(|| {
        registry.reload().unwrap();
        registry.session(&session).unwrap().messaging.is_none()
    });
    assert!(
        cleared,
        "shim calls after injected error: {}",
        fs::read(&shim_calls).map(|bytes| bytes.len()).unwrap_or(0)
    );
    assert!(taskai_core::mail::is_live(pid, &start));
    assert!(card_path.exists());
    assert!(wait_for(|| tmux_ok(
        &server,
        &["list-panes", "-t", "=s1:1.0", "-F", "#{pane_dead}"]
    ) == "1"));
    fs::remove_file(&fail_marker).unwrap();
    tmux_ok(&server, &["kill-window", "-t", "=s1:1"]);
    assert!(observer.output().unwrap().status.success());
    assert!(wait_for(|| {
        registry.reload().unwrap();
        !registry.live_sessions(&task).is_empty()
    }));
    let observer_pid: u32 = tmux_ok(
        &server,
        &["list-panes", "-t", "=s1:1.0", "-F", "#{pane_pid}"],
    )
    .parse()
    .unwrap();
    assert!(Command::new("/bin/kill")
        .args(["-TERM", &observer_pid.to_string()])
        .status()
        .unwrap()
        .success());
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry.session(&session).unwrap().messaging.is_none()
    }));
    assert!(
        taskai_core::mail::is_live(pid, &start),
        "native pane outlives observer SIGTERM"
    );
    assert!(
        card_path.exists(),
        "observer leaves the native card untouched"
    );
    // Startup observation can fail before NativeObserver is constructed. An
    // earlier exact registration must still be retracted, with the pane alive.
    tmux_ok(&server, &["kill-window", "-t", "=s1:1"]);
    let saved = registry.session(&session).unwrap().clone();
    AddressBook::new(&state, &task)
        .register(&MailPeer {
            session_ref: session.clone(),
            task_ref: task.clone(),
            name: saved.address,
            harness: "claude".into(),
            workspace: saved.workspace,
            pid,
            proc_start: start.clone(),
            status: Activity::Idle,
            waiting_for: None,
            mailbox: socket.clone(),
            features: vec!["notify_idle".into()],
            mode: None,
        })
        .unwrap();
    fs::write(&fail_marker, b"fail on startup").unwrap();
    assert!(observer.output().unwrap().status.success());
    assert!(wait_for(|| {
        registry.reload().unwrap();
        registry.session(&session).unwrap().messaging.is_none()
    }));
    assert!(taskai_core::mail::is_live(pid, &start));
    assert!(card_path.exists());
}
