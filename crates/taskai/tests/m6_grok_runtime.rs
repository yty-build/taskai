// ABOUTME: Private fake-Grok and tmux lifecycle checks for the exact M6 bridge process.
// ABOUTME: No installed provider, user home, real leader, permission answer, or model call is used.

use std::{
    fs,
    os::unix::fs::{FileTypeExt, PermissionsExt},
    os::unix::net::UnixStream,
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
    sync::{
        atomic::{AtomicUsize, Ordering},
        mpsc, Arc,
    },
    thread,
    time::{Duration, Instant},
};

use serde_json::json;
use taskai_connectors::grok::{call_tool, mode_socket_path, tool_socket_path};
use taskai_core::{
    mail::{
        is_live, key_path, proc_start, socket_path, strip_messaging_env, Delivery, Identity,
        Mailbox, MailboxConfig, PermissionMode, SocketTransport,
    },
    registry::{AddressBook, HarnessSettings, ProviderMapping, Registry, Source},
};

static NEXT_SERVER: AtomicUsize = AtomicUsize::new(1);
const GROK_ID: &str = "01234567-89ab-4cde-8fab-0123456789ab";

const FAKE_GROK: &str = r#"#!/usr/bin/env python3
import json, os, socket, sys, threading, time
root = os.environ['FAKE_GROK_ROOT']
log = os.path.join(root, 'grok-home', 'leader.log')
if '--version' in sys.argv:
    open(os.path.join(root, 'version.received'), 'w').close()
    print('grok 1.0.77')
    sys.exit(0)
if 'agent' not in sys.argv:
    sock = os.path.join(root, 'run', 'grok', 's1.sock')
    os.makedirs(os.path.dirname(sock), exist_ok=True)
    os.makedirs(os.path.dirname(log), exist_ok=True)
    open(log, 'a').close()
    if os.environ.get('FAKE_SEPARATE_LEADER') == '1':
        # Like Grok: the leader is its own process in its own session, records its pid beside
        # the socket, ignores the pane's hangup and removes the socket when asked to stop.
        if os.fork() == 0:
            import signal
            os.setsid()
            leader = socket.socket(socket.AF_UNIX)
            leader.bind(sock)
            leader.listen(2)
            for path in (sock[:-len('.sock')] + '.lock', os.path.join(root, 'leader.pid')):
                with open(path, 'w') as out:
                    out.write(str(os.getpid()))
            def stop(*_):
                try:
                    os.unlink(sock)
                except OSError:
                    pass
                os._exit(0)
            signal.signal(signal.SIGTERM, stop)
            signal.signal(signal.SIGHUP, signal.SIG_IGN)
            while True:
                time.sleep(.1)
        while not os.path.exists(os.path.join(root, 'leader.pid')):
            time.sleep(.02)
    else:
        leader = socket.socket(socket.AF_UNIX)
        leader.bind(sock)
        leader.listen(2)
    with open(os.path.join(root, 'native.pid'), 'w') as out:
        out.write(str(os.getpid()))
    while True:
        time.sleep(.1)
with open(os.path.join(root, 'joined.pid'), 'w') as out:
    out.write(str(os.getpid()))
def reply(msg_id, value):
    sys.stdout.write(json.dumps({'jsonrpc':'2.0','id':msg_id,'result':value}) + '\n')
    sys.stdout.flush()
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get('method')
    with open(os.path.join(root, 'methods.jsonl'), 'a') as out:
        out.write(json.dumps({'method':method,'id':msg.get('id')}) + '\n')
    if method == 'initialize':
        open(os.path.join(root, 'initialize.received'), 'w').close()
        if os.environ.get('FAKE_HOLD_INIT') == '1':
            while True: time.sleep(.1)
        sock = os.environ['GROK_LEADER_SOCKET']
        with open(log, 'a') as out:
            out.write('2026-11-11T03:13:03.960303Z  INFO Client registered client_id=1 client_type=python3 --permission-mode default agent --leader --leader-socket %s stdio mode=Stdio yolo_mode=false client_version=Some("1.0.77")\n' % sock)
        reply(msg['id'], {'protocolVersion':1,'agentCapabilities':{'loadSession':True}})
    elif method == 'session/load':
        with open(os.path.join(root, 'loaded.json'), 'w') as out:
            json.dump(msg.get('params'), out)
        reply(msg['id'], {})
        sys.stdout.write(json.dumps({'jsonrpc':'2.0','method':'_x.ai/sessions/changed',
            'params':{'upserted':[{'sessionId':msg['params']['sessionId'],'activity':'working'}]}}) + '\n')
        sys.stdout.flush()
        def permission_notice():
            while not os.path.exists(os.path.join(root, 'permission.go')):
                time.sleep(.02)
            sys.stdout.write(json.dumps({'jsonrpc':'2.0','id':77,'method':'session/request_permission',
                'params':{'sessionId':msg['params']['sessionId'],'toolCall':{'secret':'fixture'}}}) + '\n')
            sys.stdout.flush()
        threading.Thread(target=permission_notice, daemon=True).start()
    elif method == 'session/prompt':
        open(os.path.join(root, 'prompt.received'), 'w').close()
        if os.environ.get('FAKE_HOLD_PROMPT') == '1':
            while True: time.sleep(.1)
        reply(msg['id'], {'stopReason':'end_turn'})
"#;

fn tmux(server: &str, args: &[&str]) -> Output {
    let mut command = Command::new("tmux");
    command.args(["-f", "/dev/null", "-L", server]).args(args);
    strip_messaging_env(&mut command);
    command.output().unwrap()
}

fn tmux_ok(server: &str, args: &[&str]) -> String {
    let output = tmux(server, args);
    assert!(output.status.success(), "private tmux command failed");
    String::from_utf8(output.stdout).unwrap().trim().into()
}

fn wait_for(mut condition: impl FnMut() -> bool) -> bool {
    let deadline = Instant::now() + Duration::from_secs(6);
    while Instant::now() < deadline {
        if condition() {
            return true;
        }
        thread::sleep(Duration::from_millis(25));
    }
    false
}

fn pid_file(path: &Path) -> Option<u32> {
    fs::read_to_string(path).ok()?.parse().ok()
}

struct OwnedJoinedClient {
    pid: u32,
    start: String,
}

impl OwnedJoinedClient {
    fn from_pid_file(path: &Path) -> Self {
        let pid = pid_file(path).unwrap();
        let start = proc_start(pid).unwrap();
        Self { pid, start }
    }
}

impl Drop for OwnedJoinedClient {
    fn drop(&mut self) {
        if is_live(self.pid, &self.start) {
            unsafe { libc::kill(self.pid as i32, libc::SIGTERM) };
            if !wait_for(|| !is_live(self.pid, &self.start)) {
                unsafe { libc::kill(self.pid as i32, libc::SIGKILL) };
                let _ = wait_for(|| !is_live(self.pid, &self.start));
            }
        }
    }
}

struct Fixture {
    root: tempfile::TempDir,
    server: String,
    grok_address: String,
    peer_address: String,
    native_pid: u32,
    sender: Arc<Mailbox>,
    received: mpsc::Receiver<Delivery>,
}

impl Fixture {
    fn new(hold_init: bool, hold_prompt: bool) -> Self {
        Self::build(hold_init, hold_prompt, false, false)
    }

    fn new_with_claude_probe(hold_init: bool, hold_prompt: bool, slow_claude: bool) -> Self {
        Self::build(hold_init, hold_prompt, slow_claude, false)
    }

    /// A pane whose leader is a separate process that outlives it, as Grok's is.
    fn with_separate_leader() -> Self {
        Self::build(false, false, false, true)
    }

    fn build(hold_init: bool, hold_prompt: bool, slow_claude: bool, separate_leader: bool) -> Self {
        let mut version = Command::new("tmux");
        version.arg("-V");
        strip_messaging_env(&mut version);
        assert!(version.output().is_ok());
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        let grok_home = root.path().join("grok-home");
        fs::create_dir_all(&workspace).unwrap();
        fs::create_dir_all(&grok_home).unwrap();
        fs::write(grok_home.join("auth.json"), "").unwrap();
        fs::write(
            grok_home.join("trusted_folders.toml"),
            format!("[folders.\"{}\"]\ntrusted = true\n", workspace.display()),
        )
        .unwrap();
        let fake = root.path().join("fake-grok");
        fs::write(&fake, FAKE_GROK).unwrap();
        fs::set_permissions(&fake, fs::Permissions::from_mode(0o700)).unwrap();
        let server = format!(
            "w2g{}x{}",
            std::process::id(),
            NEXT_SERVER.fetch_add(1, Ordering::SeqCst)
        );
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        registry
            .set_server_harness(
                "local",
                "grok",
                HarnessSettings {
                    enabled: true,
                    executable: Some(fake.display().to_string()),
                    model: None,
                },
                Source::HumanUi,
            )
            .unwrap();
        if slow_claude {
            let claude = root.path().join("fake-slow-claude");
            fs::write(&claude, "#!/bin/sh\n: > \"$FAKE_GROK_ROOT/claude-probe.received\"\nsleep 1\nprintf 'claude 0.0.0\\n'\n").unwrap();
            fs::set_permissions(&claude, fs::Permissions::from_mode(0o700)).unwrap();
            registry
                .set_server_harness(
                    "local",
                    "claude",
                    HarnessSettings {
                        enabled: true,
                        executable: Some(claude.display().to_string()),
                        model: None,
                    },
                    Source::HumanUi,
                )
                .unwrap();
        }
        let task_ref = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let grok_ref = registry
            .create_session(&task_ref, None, "Grok", "grok", &workspace, Source::HumanUi)
            .unwrap();
        let peer_ref = registry
            .create_session(
                &task_ref,
                None,
                "Peer",
                "codex",
                &workspace,
                Source::HumanUi,
            )
            .unwrap();
        assert_eq!(grok_ref, "s1");
        registry.bind_runtime(&grok_ref, &server).unwrap();
        let runtime = registry
            .session(&grok_ref)
            .unwrap()
            .runtime
            .as_ref()
            .unwrap();
        registry
            .bind_provider_if_absent(
                &grok_ref,
                ProviderMapping {
                    provider: "grok".into(),
                    provider_session_id: GROK_ID.into(),
                    runtime_id: format!("tmux:{}:{}", runtime.private_server, runtime.session),
                    connector_version: "fake-grok-1.0.77".into(),
                },
            )
            .unwrap();
        let grok_address = registry.session(&grok_ref).unwrap().address.clone();
        let peer = registry.session(&peer_ref).unwrap();
        let peer_address = peer.address.clone();
        let (tx, received) = mpsc::channel();
        let sender = Arc::new(
            Mailbox::open(
                MailboxConfig {
                    identity: Identity {
                        session_ref: peer_ref.clone(),
                        task_ref: task_ref.clone(),
                        name: peer_address.clone(),
                        harness: "codex".into(),
                        workspace: workspace.clone(),
                        mode: PermissionMode::Prompting,
                    },
                    mail_dir: root.path().join("run/mail"),
                    log_path: root
                        .path()
                        .join("state/messages")
                        .join(format!("{task_ref}.jsonl")),
                },
                Arc::new(AddressBook::new(root.path().join("state"), &task_ref)),
                Arc::new(SocketTransport),
                move |delivery| {
                    let _ = tx.send(delivery);
                },
            )
            .unwrap(),
        );
        let root_env = format!("FAKE_GROK_ROOT={}", root.path().display());
        let grok_env = format!("GROK_HOME={}", grok_home.display());
        let home_env = format!("TASKAI_HOME={}", root.path().display());
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
                &root_env,
                "-e",
                &grok_env,
                "-e",
                &home_env,
                "-e",
                "TASKAI_SESSION_REF=s1",
                "-e",
                &format!("FAKE_HOLD_INIT={}", u8::from(hold_init)),
                "-e",
                &format!("FAKE_HOLD_PROMPT={}", u8::from(hold_prompt)),
                "-e",
                &format!("FAKE_SEPARATE_LEADER={}", u8::from(separate_leader)),
                "--",
                fake.to_str().unwrap(),
            ],
        );
        tmux_ok(
            &server,
            &["set-window-option", "-g", "remain-on-exit", "on"],
        );
        // The file exists a moment before its content does; wait for the pid itself.
        assert!(wait_for(
            || pid_file(&root.path().join("native.pid")).is_some()
        ));
        let native_pid = pid_file(&root.path().join("native.pid")).unwrap();
        let fixture = Self {
            root,
            server,
            grok_address,
            peer_address,
            native_pid,
            sender,
            received,
        };
        fixture.start_bridge(hold_init, hold_prompt);
        fixture
    }

    fn start_bridge(&self, hold_init: bool, hold_prompt: bool) {
        let taskai = env!("CARGO_BIN_EXE_taskai");
        let root_env = format!("FAKE_GROK_ROOT={}", self.root.path().display());
        let grok_env = format!("GROK_HOME={}", self.root.path().join("grok-home").display());
        let home_env = format!("TASKAI_HOME={}", self.root.path().display());
        tmux_ok(
            &self.server,
            &[
                "new-window",
                "-d",
                "-t",
                "=s1:1",
                "-n",
                "bridge",
                "-c",
                self.path("workspace").to_str().unwrap(),
                "-e",
                &root_env,
                "-e",
                &grok_env,
                "-e",
                &home_env,
                "-e",
                "TASKAI_SESSION_REF=s1",
                "-e",
                &format!("TASKAI_CLAUDE_BIN={}", self.path("absent-claude").display()),
                "-e",
                &format!("CLAUDE_CONFIG_DIR={}", self.path("claude-home").display()),
                "-e",
                &format!("CODEX_HOME={}", self.path("codex-home").display()),
                "-e",
                &format!("FAKE_HOLD_INIT={}", u8::from(hold_init)),
                "-e",
                &format!("FAKE_HOLD_PROMPT={}", u8::from(hold_prompt)),
                "--",
                taskai,
                "bridge",
                "s1",
            ],
        );
    }

    fn restart_bridge(&self) {
        assert!(self.sidecar_dead(), "old bridge is still live");
        tmux_ok(&self.server, &["kill-window", "-t", "=s1:1"]);
        self.start_bridge(false, false);
    }

    fn path(&self, name: &str) -> PathBuf {
        self.root.path().join(name)
    }
    fn sidecar_pid(&self) -> u32 {
        tmux_ok(
            &self.server,
            &["list-panes", "-t", "=s1:1.0", "-F", "#{pane_pid}"],
        )
        .parse()
        .unwrap()
    }
    fn sidecar_dead(&self) -> bool {
        tmux_ok(
            &self.server,
            &["list-panes", "-t", "=s1:1.0", "-F", "#{pane_dead}"],
        ) == "1"
    }
    fn stop_bridge(&self) {
        let pid = self.sidecar_pid();
        let start = proc_start(pid).unwrap();
        assert!(is_live(pid, &start));
        assert_eq!(unsafe { libc::kill(pid as i32, libc::SIGTERM) }, 0);
    }
    fn bridge_pane(&self) -> String {
        tmux_ok(&self.server, &["capture-pane", "-p", "-t", "=s1:1.0"])
    }

    fn native_identity(&self) -> (String, u32, String) {
        let line = tmux_ok(
            &self.server,
            &[
                "list-panes",
                "-t",
                "=s1:0.0",
                "-F",
                "#{pane_id}\t#{pane_pid}\t#{pane_dead}",
            ],
        );
        let fields: Vec<_> = line.split('\t').collect();
        assert_eq!(fields.len(), 3);
        assert_eq!(fields[2], "0", "native pane is dead");
        let pid = fields[1].parse().unwrap();
        (fields[0].into(), pid, proc_start(pid).unwrap())
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        self.sender.close();
        let _ = tmux(&self.server, &["kill-session", "-t", "=s1"]);
        // A separate fake leader ignores its pane's hangup; end this fixture's own one.
        if let Some(leader) = pid_file(&self.path("leader.pid")) {
            unsafe { libc::kill(leader as i32, libc::SIGTERM) };
        }
    }
}

#[test]
fn grok_bridge_startup_join_cancels_when_exact_native_dies() {
    let fixture = Fixture::new(true, false);
    assert!(
        wait_for(|| fixture.path("initialize.received").exists()),
        "bridge_dead={} pid={} command={} version={} joined={} log={} pane={}",
        fixture.sidecar_dead(),
        fixture.sidecar_pid(),
        tmux_ok(
            &fixture.server,
            &[
                "list-panes",
                "-t",
                "=s1:1.0",
                "-F",
                "#{pane_current_command}"
            ]
        ),
        fixture.path("version.received").exists(),
        fixture.path("joined.pid").exists(),
        fixture.path("grok-home/leader.log").exists(),
        fixture.bridge_pane()
    );
    let joined = pid_file(&fixture.path("joined.pid")).unwrap();
    let joined_start = proc_start(joined).unwrap();
    assert_eq!(
        unsafe { libc::kill(fixture.native_pid as i32, libc::SIGTERM) },
        0
    );
    assert!(
        wait_for(|| fixture.sidecar_dead()),
        "{}",
        fixture.bridge_pane()
    );
    assert!(!is_live(joined, &joined_start));
    let registry = Registry::open(&fixture.path("state")).unwrap();
    let grok = registry.session("s1").unwrap();
    assert_eq!(grok.provider.as_ref().unwrap().provider_session_id, GROK_ID);
    assert!(grok.messaging.is_none());
    assert!(!tool_socket_path(&fixture.path("run"), "s1").exists());
    assert!(!mode_socket_path(&fixture.path("run"), "s1").exists());
}

#[test]
fn grok_bridge_startup_join_cancels_on_sigterm_without_stopping_native() {
    let fixture = Fixture::new(true, false);
    assert!(
        wait_for(|| fixture.path("initialize.received").exists()),
        "bridge_dead={} version={} joined={} log={} pane={}",
        fixture.sidecar_dead(),
        fixture.path("version.received").exists(),
        fixture.path("joined.pid").exists(),
        fixture.path("grok-home/leader.log").exists(),
        fixture.bridge_pane()
    );
    let joined = pid_file(&fixture.path("joined.pid")).unwrap();
    let joined_start = proc_start(joined).unwrap();
    fixture.stop_bridge();
    assert!(
        wait_for(|| fixture.sidecar_dead()),
        "{}",
        fixture.bridge_pane()
    );
    assert!(!is_live(joined, &joined_start));
    assert!(proc_start(fixture.native_pid).is_some());
    assert!(!tool_socket_path(&fixture.path("run"), "s1").exists());
}

#[test]
fn grok_native_loss_during_slow_claude_readiness_never_publishes_mailbox() {
    let fixture = Fixture::new_with_claude_probe(false, false, true);
    assert!(
        wait_for(|| fixture.path("claude-probe.received").exists()),
        "{}",
        fixture.bridge_pane()
    );
    let joined = pid_file(&fixture.path("joined.pid")).unwrap();
    let joined_start = proc_start(joined).unwrap();
    assert_eq!(
        unsafe { libc::kill(fixture.native_pid as i32, libc::SIGTERM) },
        0
    );
    assert!(
        wait_for(|| fixture.sidecar_dead()),
        "{}",
        fixture.bridge_pane()
    );
    let registry = Registry::open(&fixture.path("state")).unwrap();
    assert!(registry.session("s1").unwrap().messaging.is_none());
    assert!(!is_live(joined, &joined_start));
    assert!(!tool_socket_path(&fixture.path("run"), "s1").exists());
    assert!(!mode_socket_path(&fixture.path("run"), "s1").exists());
}

#[test]
fn grok_bridge_reports_native_status_and_requires_one_use_hook_for_send() {
    let fixture = Fixture::new(false, false);
    let tool = tool_socket_path(&fixture.path("run"), "s1");
    let mode = mode_socket_path(&fixture.path("run"), "s1");
    assert!(
        wait_for(|| tool.exists() && mode.exists()),
        "{}",
        fixture.bridge_pane()
    );
    let load: serde_json::Value =
        serde_json::from_slice(&fs::read(fixture.path("loaded.json")).unwrap()).unwrap();
    assert_eq!(load["sessionId"], GROK_ID);
    assert_eq!(load["_meta"]["yoloMode"], false);
    assert_eq!(load["_meta"]["autoMode"], false);
    assert_eq!(
        tmux_ok(
            &fixture.server,
            &["list-panes", "-t", "=s1:0.0", "-F", "#{pane_pid}"]
        )
        .parse::<u32>()
        .unwrap(),
        fixture.native_pid
    );
    assert!(
        wait_for(|| {
            let registry = Registry::open(&fixture.path("state")).unwrap();
            registry
                .session("s1")
                .unwrap()
                .messaging
                .as_ref()
                .is_some_and(|entry| {
                    entry.status == taskai_core::mail::Activity::Busy && entry.mode.is_none()
                })
        }),
        "{}",
        fixture.bridge_pane()
    );
    let no_evidence = call_tool(
        &tool,
        GROK_ID,
        "send_message",
        json!({"to":fixture.peer_address,"text":"private hello","notify_when_idle":false}),
    );
    assert_eq!(no_evidence["status"], "send-failed", "{no_evidence}");
    let envelope = json!({
        "sessionId": GROK_ID,
        "hookEventName": "pre_tool_use",
        "hook_event_name": "PreToolUse",
        "toolName": "taskai__send_message",
        "toolUseId": "tool-one",
        "toolInputTruncated": false,
        "permissionMode": "default",
        "timestamp": chrono::Utc::now().to_rfc3339(),
        "toolInput": {"to":fixture.peer_address,"text":"private hello","notify_when_idle":false}
    });
    let mut hook = Command::new(env!("CARGO_BIN_EXE_taskai"));
    hook.arg("grok-mode-hook")
        .env("TASKAI_MODE_SOCKET", &mode)
        .env("GROK_SESSION_ID", GROK_ID)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    strip_messaging_env(&mut hook);
    let mut hook = hook.spawn().unwrap();
    use std::io::Write;
    hook.stdin
        .take()
        .unwrap()
        .write_all(envelope.to_string().as_bytes())
        .unwrap();
    let hook = hook.wait_with_output().unwrap();
    assert!(hook.status.success());
    assert!(
        hook.stdout.is_empty(),
        "hook must never decide a native permission"
    );
    assert!(hook.stderr.is_empty());
    let sent = call_tool(
        &tool,
        GROK_ID,
        "send_message",
        json!({"to":fixture.peer_address,"text":"private hello","notify_when_idle":false}),
    );
    assert_eq!(sent["status"], "sent", "{sent}");
    let delivery = fixture
        .received
        .recv_timeout(Duration::from_secs(2))
        .unwrap();
    assert!(delivery.text.contains("private hello"));
    let registry = Registry::open(&fixture.path("state")).unwrap();
    assert!(
        registry
            .session("s1")
            .unwrap()
            .messaging
            .as_ref()
            .unwrap()
            .mode
            .is_none(),
        "one tool fact cannot publish a session mode"
    );
    assert_eq!(
        call_tool(
            &tool,
            GROK_ID,
            "send_message",
            json!({"to":fixture.peer_address,"text":"private hello","notify_when_idle":false})
        )["status"],
        "send-failed",
        "the hook fact is one-use"
    );
    fs::write(fixture.path("permission.go"), "").unwrap();
    assert!(
        wait_for(|| {
            let registry = Registry::open(&fixture.path("state")).unwrap();
            registry
                .session("s1")
                .unwrap()
                .messaging
                .as_ref()
                .is_some_and(|entry| entry.status == taskai_core::mail::Activity::Waiting)
        }),
        "native permission request should only set waiting status"
    );
    let methods = fs::read_to_string(fixture.path("methods.jsonl")).unwrap();
    assert!(
        !methods.lines().any(|line| {
            serde_json::from_str::<serde_json::Value>(line)
                .ok()
                .is_some_and(|entry| entry["id"] == 77)
        }),
        "bridge must not answer native permission request"
    );
    let joined = pid_file(&fixture.path("joined.pid")).unwrap();
    let joined_start = proc_start(joined).unwrap();
    fixture.stop_bridge();
    assert!(
        wait_for(|| fixture.sidecar_dead()),
        "{}",
        fixture.bridge_pane()
    );
    assert!(!is_live(joined, &joined_start));
    assert!(proc_start(fixture.native_pid).is_some());
    assert!(!tool.exists() && !mode.exists());
}

#[test]
fn grok_bridge_shutdown_wakes_blocked_prompt_without_false_drop() {
    let fixture = Fixture::new(false, true);
    let tool = tool_socket_path(&fixture.path("run"), "s1");
    assert!(wait_for(|| tool.exists()), "{}", fixture.bridge_pane());
    let sent = fixture
        .sender
        .send(&fixture.grok_address, "private incoming", false, None);
    assert_eq!(sent.status, taskai_core::mail::SendStatus::Sent);
    assert!(
        wait_for(|| fixture.path("prompt.received").exists()),
        "{}",
        fixture.bridge_pane()
    );
    let joined = pid_file(&fixture.path("joined.pid")).unwrap();
    let joined_start = proc_start(joined).unwrap();
    fixture.stop_bridge();
    assert!(
        wait_for(|| fixture.sidecar_dead()),
        "{}",
        fixture.bridge_pane()
    );
    assert!(!is_live(joined, &joined_start));
    assert!(proc_start(fixture.native_pid).is_some());
    while let Ok(delivery) = fixture.received.try_recv() {
        assert!(
            !delivery.text.contains("was dropped"),
            "uncertain prompt cannot be called dropped"
        );
    }
    let registry = Registry::open(&fixture.path("state")).unwrap();
    assert!(registry.session("s1").unwrap().messaging.is_none());
    assert!(!tool.exists());
}

#[test]
fn grok_bridge_recreates_beside_exact_native_after_abrupt_death() {
    let fixture = Fixture::new(false, false);
    let tool = tool_socket_path(&fixture.path("run"), "s1");
    let mode = mode_socket_path(&fixture.path("run"), "s1");
    let mail = socket_path(&fixture.path("run/mail"), "s1");
    let key = key_path(&fixture.path("run/mail"), "s1");
    let unrelated = fixture.path("run/grok/unrelated.keep");
    fs::write(&unrelated, "private sentinel").unwrap();
    assert!(
        wait_for(|| {
            tool.exists()
                && mode.exists()
                && call_tool(&tool, GROK_ID, "list_sessions", json!({}))["status"] == "ok"
        }),
        "first bridge did not publish tools: {}",
        fixture.bridge_pane()
    );
    let native = fixture.native_identity();
    assert_eq!(native.1, fixture.native_pid);
    let first_bridge = fixture.sidecar_pid();
    let first_bridge_start = proc_start(first_bridge).unwrap();
    let first_joined = OwnedJoinedClient::from_pid_file(&fixture.path("joined.pid"));
    assert_ne!(first_joined.pid, native.1);
    let registry = Registry::open(&fixture.path("state")).unwrap();
    let saved = registry.session("s1").unwrap();
    assert_eq!(
        saved.provider.as_ref().unwrap().provider_session_id,
        GROK_ID
    );
    assert_eq!(saved.messaging.as_ref().unwrap().pid, first_bridge);
    assert_eq!(saved.messaging.as_ref().unwrap().mode, None);

    assert_eq!(unsafe { libc::kill(first_bridge as i32, libc::SIGKILL) }, 0);
    assert!(wait_for(|| fixture.sidecar_dead()), "bridge did not die");
    assert!(!is_live(first_bridge, &first_bridge_start));
    assert_eq!(fixture.native_identity(), native);
    assert!(tool.exists() && mode.exists() && mail.exists() && key.exists());
    assert!(fs::metadata(&tool).unwrap().file_type().is_socket());
    assert!(
        UnixStream::connect(&tool).is_err(),
        "old tool listener survived"
    );
    // The joined ACP process is the bridge's child, never the native pane. A crash can orphan
    // it, so terminate only this exact recorded PID/start before the new bridge joins.
    if is_live(first_joined.pid, &first_joined.start) {
        let sent = unsafe { libc::kill(first_joined.pid as i32, libc::SIGTERM) };
        assert!(sent == 0 || !is_live(first_joined.pid, &first_joined.start));
    }
    assert!(wait_for(|| !is_live(first_joined.pid, &first_joined.start)));

    fixture.restart_bridge();
    assert!(
        wait_for(|| {
            pid_file(&fixture.path("joined.pid")).is_some_and(|pid| pid != first_joined.pid)
        }),
        "recreated bridge did not join saved session: {}",
        fixture.bridge_pane()
    );
    let second_pid = pid_file(&fixture.path("joined.pid")).unwrap();
    assert_ne!(second_pid, first_joined.pid);
    // The old ToolServer exits the bridge immediately after join. Keep an exact-PID guard
    // when the short-lived child is still present, without depending on that race for red.
    let second_joined = proc_start(second_pid).map(|start| OwnedJoinedClient {
        pid: second_pid,
        start,
    });
    assert_eq!(fixture.native_identity(), native);
    assert_eq!(pid_file(&fixture.path("native.pid")), Some(native.1));
    assert_eq!(
        tmux_ok(&fixture.server, &["list-panes", "-a", "-F", "#{pane_id}"])
            .lines()
            .count(),
        2,
        "one native pane and one bridge pane"
    );
    assert_eq!(
        Registry::open(&fixture.path("state"))
            .unwrap()
            .session("s1")
            .unwrap()
            .provider
            .as_ref(),
        saved.provider.as_ref(),
        "bridge recreation must retain the one saved native ID"
    );
    assert!(
        wait_for(|| {
            fs::read_to_string(fixture.path("methods.jsonl"))
                .ok()
                .is_some_and(|lines| {
                    lines
                        .lines()
                        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
                        .filter(|entry| entry["method"] == "session/load")
                        .count()
                        == 2
                })
        }),
        "recreated bridge did not load the saved session: {}",
        fixture.bridge_pane()
    );
    let methods = fs::read_to_string(fixture.path("methods.jsonl")).unwrap();
    assert!(
        methods.lines().all(|line| {
            let entry: serde_json::Value = serde_json::from_str(line).unwrap();
            matches!(
                entry["method"].as_str(),
                Some("initialize" | "session/load")
            )
        }),
        "no new native session or model prompt is allowed"
    );
    let reloaded: serde_json::Value =
        serde_json::from_slice(&fs::read(fixture.path("loaded.json")).unwrap()).unwrap();
    assert_eq!(reloaded["sessionId"], GROK_ID);
    assert!(
        wait_for(|| { call_tool(&tool, GROK_ID, "list_sessions", json!({}))["status"] == "ok" }),
        "recreated bridge did not recover stale tool socket; dead={} pane={}",
        fixture.sidecar_dead(),
        fixture.bridge_pane()
    );
    let second_joined = second_joined.expect("restored bridge lost its joined client");

    let second_bridge = fixture.sidecar_pid();
    assert_ne!(second_bridge, first_bridge);
    assert_eq!(fixture.native_identity(), native);
    let registry = Registry::open(&fixture.path("state")).unwrap();
    let restored = registry.session("s1").unwrap();
    assert_eq!(
        restored.provider.as_ref().unwrap(),
        saved.provider.as_ref().unwrap()
    );
    let messaging = restored.messaging.as_ref().unwrap();
    assert_eq!(messaging.pid, second_bridge);
    assert_eq!(messaging.mode, None);
    assert!(mode.exists() && mail.exists() && key.exists());
    let load: serde_json::Value =
        serde_json::from_slice(&fs::read(fixture.path("loaded.json")).unwrap()).unwrap();
    assert_eq!(load["sessionId"], GROK_ID);
    assert_eq!(load["_meta"]["yoloMode"], false);
    assert_eq!(load["_meta"]["autoMode"], false);

    let text = "private recreated bridge letter";
    let args = json!({"to":fixture.peer_address,"text":text,"notify_when_idle":false});
    assert_eq!(
        call_tool(&tool, GROK_ID, "send_message", args.clone())["status"],
        "send-failed",
        "recreation must begin without permission evidence"
    );
    let envelope = json!({
        "sessionId": GROK_ID,
        "hookEventName": "pre_tool_use",
        "hook_event_name": "PreToolUse",
        "toolName": "taskai__send_message",
        "toolUseId": "recreated-tool",
        "toolInputTruncated": false,
        "permissionMode": "default",
        "timestamp": chrono::Utc::now().to_rfc3339(),
        "toolInput": args
    });
    let mut hook = Command::new(env!("CARGO_BIN_EXE_taskai"));
    hook.arg("grok-mode-hook")
        .env("TASKAI_MODE_SOCKET", &mode)
        .env("GROK_SESSION_ID", GROK_ID)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    strip_messaging_env(&mut hook);
    let mut hook = hook.spawn().unwrap();
    use std::io::Write;
    hook.stdin
        .take()
        .unwrap()
        .write_all(envelope.to_string().as_bytes())
        .unwrap();
    let hook = hook.wait_with_output().unwrap();
    assert!(hook.status.success(), "hook fact was not accepted");
    assert!(hook.stdout.is_empty() && hook.stderr.is_empty());
    assert_eq!(
        call_tool(
            &tool,
            GROK_ID,
            "send_message",
            json!({"to":fixture.peer_address,"text":text,"notify_when_idle":false})
        )["status"],
        "sent"
    );
    assert!(fixture
        .received
        .recv_timeout(Duration::from_secs(2))
        .unwrap()
        .text
        .contains(text));
    assert_eq!(
        call_tool(
            &tool,
            GROK_ID,
            "send_message",
            json!({"to":fixture.peer_address,"text":text,"notify_when_idle":false})
        )["status"],
        "send-failed",
        "the restored hook fact is one-use"
    );
    let methods = fs::read_to_string(fixture.path("methods.jsonl")).unwrap();
    let methods: Vec<serde_json::Value> = methods
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect();
    assert_eq!(
        methods
            .iter()
            .filter(|entry| entry["method"] == "session/load")
            .count(),
        2,
        "each bridge may load only the one saved session"
    );
    assert!(
        methods.iter().all(|entry| {
            matches!(
                entry["method"].as_str(),
                Some("initialize" | "session/load")
            )
        }),
        "no new session or model prompt is allowed"
    );
    assert_eq!(
        tmux_ok(&fixture.server, &["list-panes", "-a", "-F", "#{pane_id}"])
            .lines()
            .count(),
        2,
        "one native pane and one bridge pane"
    );
    fixture.stop_bridge();
    assert!(
        wait_for(|| fixture.sidecar_dead()),
        "{}",
        fixture.bridge_pane()
    );
    assert!(!is_live(second_joined.pid, &second_joined.start));
    assert_eq!(fixture.native_identity(), native);
    assert!(Registry::open(&fixture.path("state"))
        .unwrap()
        .session("s1")
        .unwrap()
        .messaging
        .is_none());
    assert!(!tool.exists() && !mode.exists() && !mail.exists() && !key.exists());
    assert_eq!(fs::read_to_string(unrelated).unwrap(), "private sentinel");
}

#[test]
fn grok_bridge_tells_subscribers_and_cleans_up_when_its_runtime_is_destroyed() {
    let fixture = Fixture::new(false, false);
    let tool = tool_socket_path(&fixture.path("run"), "s1");
    let mode = mode_socket_path(&fixture.path("run"), "s1");
    let mail = socket_path(&fixture.path("run/mail"), "s1");
    let key = key_path(&fixture.path("run/mail"), "s1");
    assert!(
        wait_for(|| {
            tool.exists()
                && mode.exists()
                && call_tool(&tool, GROK_ID, "list_sessions", json!({}))["status"] == "ok"
        }),
        "bridge did not publish tools: {}",
        fixture.bridge_pane()
    );
    let bridge = fixture.sidecar_pid();
    let bridge_start = proc_start(bridge).unwrap();
    let joined = OwnedJoinedClient::from_pid_file(&fixture.path("joined.pid"));
    // The fake session never reports idle, so this subscription is still open when the
    // runtime goes away.
    let sent = fixture
        .sender
        .send(&fixture.grok_address, "private incoming", true, None);
    assert_eq!(sent.status, taskai_core::mail::SendStatus::Sent);
    assert!(
        wait_for(|| fixture.path("prompt.received").exists()),
        "{}",
        fixture.bridge_pane()
    );
    // Destroying a session kills its exact tmux session: the native pane and the bridge pane
    // lose their terminals at the same moment.
    tmux_ok(&fixture.server, &["kill-session", "-t", "=s1"]);
    assert!(
        wait_for(|| !is_live(bridge, &bridge_start)),
        "bridge outlived its destroyed runtime"
    );
    let mut told = false;
    while let Ok(delivery) = fixture.received.recv_timeout(Duration::from_secs(2)) {
        if delivery.text.contains("exited") {
            told = true;
            break;
        }
    }
    assert!(told, "the subscriber never heard that the session exited");
    assert!(!is_live(joined.pid, &joined.start));
    assert!(Registry::open(&fixture.path("state"))
        .unwrap()
        .session("s1")
        .unwrap()
        .messaging
        .is_none());
    assert!(!tool.exists() && !mode.exists() && !mail.exists() && !key.exists());
}

// Expectation: when a Grok session's pane is gone, its bridge ends the one leader process that
// still listens on that session's own socket; while the pane lives, a bridge that stops leaves
// the leader alone. Why: Grok's leader is a separate process that outlives its pane, so a
// destroyed session would otherwise keep a process and a socket. Value: destroying a Grok agent
// leaves no leader behind, and restarting only its bridge never takes the leader from its CLI.
#[test]
fn grok_bridge_ends_the_leader_of_a_destroyed_pane() {
    let fixture = Fixture::with_separate_leader();
    let tool = tool_socket_path(&fixture.path("run"), "s1");
    let leader_socket = fixture.path("run/grok/s1.sock");
    assert!(
        wait_for(|| {
            tool.exists() && call_tool(&tool, GROK_ID, "list_sessions", json!({}))["status"] == "ok"
        }),
        "bridge did not publish tools: {}",
        fixture.bridge_pane()
    );
    let leader = pid_file(&fixture.path("leader.pid")).unwrap();
    let leader_start = proc_start(leader).unwrap();
    assert_ne!(leader, fixture.native_pid);
    tmux_ok(&fixture.server, &["kill-session", "-t", "=s1"]);
    assert!(
        wait_for(|| !is_live(leader, &leader_start)),
        "the leader outlived its destroyed session"
    );
    assert!(!leader_socket.exists(), "the leader socket was left behind");
}

#[test]
fn grok_bridge_leaves_the_leader_of_a_live_pane_alone() {
    let fixture = Fixture::with_separate_leader();
    let tool = tool_socket_path(&fixture.path("run"), "s1");
    let leader_socket = fixture.path("run/grok/s1.sock");
    assert!(
        wait_for(|| {
            tool.exists() && call_tool(&tool, GROK_ID, "list_sessions", json!({}))["status"] == "ok"
        }),
        "bridge did not publish tools: {}",
        fixture.bridge_pane()
    );
    let leader = pid_file(&fixture.path("leader.pid")).unwrap();
    let leader_start = proc_start(leader).unwrap();
    fixture.stop_bridge();
    assert!(
        wait_for(|| fixture.sidecar_dead()),
        "{}",
        fixture.bridge_pane()
    );
    assert!(proc_start(fixture.native_pid).is_some());
    assert!(
        is_live(leader, &leader_start) && leader_socket.exists(),
        "a stopped bridge took the leader from its live pane"
    );
}
