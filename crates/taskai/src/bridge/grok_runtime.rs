// ABOUTME: One Grok leader client and private tool/mode socket owner for an exact native pane.
// ABOUTME: It relays deliveries off the status loop and closes admission before mailbox teardown.

use std::{
    fs,
    io::Write,
    os::unix::{fs::FileTypeExt, net::UnixStream},
    path::Path,
    sync::{
        atomic::{AtomicBool, Ordering},
        mpsc, Arc,
    },
    thread,
    time::{Duration, Instant},
};

use taskai_connectors::grok::{
    self, GrokBridge, GrokConnector, JoinConfig, LeaderClient, ModeEvidenceServer, StatusEvent,
    ToolServer,
};
use taskai_core::{
    mail::{is_live, proc_start, Activity, Delivery, Identity, MailboxConfig, PermissionMode},
    registry::{AddressBook, Session, TmuxRuntimeLocator},
};

use super::{
    claude_mirror::{prepare_native, BridgeMail},
    gate_control::{current_uid, peer_identity},
};
use crate::{
    doctor::{self, Paths},
    launch,
    tmux::{self, NativeIdentity},
};

const LEADER_WAIT: Duration = Duration::from_secs(10);
const JOIN_TIMEOUT: Duration = Duration::from_secs(10);
const RELAY_TIMEOUT: Duration = Duration::from_secs(300);
const POLL_INTERVAL: Duration = Duration::from_millis(100);
const LEADER_STOP_WAIT: Duration = Duration::from_secs(5);

/// Runs during the whole blocking join, including initialize and session/load. The connector's
/// cancellation-aware join observes `cancel`; finishing joins this watcher on every path.
struct JoinWatch {
    done: Arc<AtomicBool>,
    cancel: Arc<AtomicBool>,
    join: Option<thread::JoinHandle<()>>,
}

impl JoinWatch {
    fn start(stop: Arc<AtomicBool>, native_alive: impl Fn() -> bool + Send + 'static) -> Self {
        let done = Arc::new(AtomicBool::new(false));
        let cancel = Arc::new(AtomicBool::new(false));
        let watch_done = Arc::clone(&done);
        let watch_cancel = Arc::clone(&cancel);
        let join = thread::spawn(move || {
            while !watch_done.load(Ordering::SeqCst) {
                if stop.load(Ordering::SeqCst) || !native_alive() {
                    watch_cancel.store(true, Ordering::SeqCst);
                    break;
                }
                thread::sleep(POLL_INTERVAL);
            }
        });
        Self {
            done,
            cancel,
            join: Some(join),
        }
    }

    fn finish(&mut self) {
        self.done.store(true, Ordering::SeqCst);
        if let Some(join) = self.join.take() {
            let _ = join.join();
        }
    }
}

impl Drop for JoinWatch {
    fn drop(&mut self) {
        self.finish();
    }
}

pub fn run(
    home: &Path,
    session: &Session,
    runtime: &TmuxRuntimeLocator,
    native: NativeIdentity,
    stop: Arc<AtomicBool>,
) -> Result<(), String> {
    let result = serve(home, session, runtime, native.clone(), stop);
    // Grok's leader is a process of its own and outlives the pane that started it. With the pane
    // gone nothing else ends it, on any way out of this bridge.
    let pane_present = is_live(native.pid, &native.proc_start)
        && tmux::native_identity(runtime).is_ok_and(|identity| identity.as_ref() == Some(&native));
    if !pane_present {
        let leader_socket = grok::leader_socket_path(&home.join("run"), &session.session_ref);
        if let Err(reason) = stop_leader(&leader_socket) {
            // The pane's terminal may be gone with it; a failed write must not stop the exit.
            let _ = writeln!(std::io::stderr(), "{reason}");
        }
    }
    result
}

/// Asks the one process that listens on this session's own leader socket to stop, and waits for
/// it. The kernel names that process; no file or process listing is trusted for it.
fn stop_leader(leader_socket: &Path) -> Result<(), String> {
    let Ok(stream) = UnixStream::connect(leader_socket) else {
        return Ok(());
    };
    let (uid, pid) = peer_identity(&stream)?;
    drop(stream);
    if uid != current_uid() || pid <= 1 || pid == std::process::id() {
        return Err("Grok leader socket is not held by a leader this user owns".into());
    }
    let Some(started) = proc_start(pid) else {
        return Ok(());
    };
    // SAFETY: kill takes a pid and a signal number and touches no memory.
    if unsafe { libc::kill(pid as libc::pid_t, libc::SIGTERM) } != 0 {
        return Err("Grok leader could not be asked to stop".into());
    }
    let deadline = Instant::now() + LEADER_STOP_WAIT;
    while is_live(pid, &started) {
        if Instant::now() >= deadline {
            return Err("Grok leader did not stop when asked".into());
        }
        thread::sleep(POLL_INTERVAL);
    }
    Ok(())
}

fn serve(
    home: &Path,
    session: &Session,
    runtime: &TmuxRuntimeLocator,
    native: NativeIdentity,
    stop: Arc<AtomicBool>,
) -> Result<(), String> {
    let session_id = saved_session_id(session, runtime)?;
    launch::grok_messaging_gate(session, &session.workspace, &Paths::from_env())?;
    require_native(runtime, &native, &stop)?;

    let run_dir = home.join("run");
    let state_dir = home.join("state");
    let leader_socket = grok::leader_socket_path(&run_dir, &session.session_ref);
    let tool_socket = grok::tool_socket_path(&run_dir, &session.session_ref);
    let mode_socket = grok::mode_socket_path(&run_dir, &session.session_ref);
    let leader_log = grok::leader_log_path().map_err(|error| error.to_string())?;
    if leader_socket.to_str().is_none() || leader_log.to_str().is_none() {
        return Err("Grok leader socket or log path is not UTF-8".into());
    }
    wait_for_leader(runtime, &native, &stop, &leader_socket, &leader_log)?;
    let executable = doctor::configured_executable("grok", session.launch.executable.as_deref());
    let connector = GrokConnector { bin: executable };
    let (bin, args) = connector.client_command(&leader_socket, grok::PermissionMode::Prompting);
    let watch_runtime = runtime.clone();
    let watch_native = native.clone();
    let mut join_watch = JoinWatch::start(Arc::clone(&stop), move || {
        is_live(watch_native.pid, &watch_native.proc_start)
            && tmux::native_identity(&watch_runtime)
                .is_ok_and(|identity| identity.as_ref() == Some(&watch_native))
    });
    let client = LeaderClient::join_with_cancel(
        JoinConfig {
            bin,
            args,
            cwd: session.workspace.clone(),
            leader_socket,
            leader_log,
            session_id: session_id.into(),
            timeout: JOIN_TIMEOUT,
            extra_env: Vec::new(),
        },
        Arc::clone(&join_watch.cancel),
    )
    .map_err(|error| error.to_string())?;
    join_watch.finish();
    if join_watch.cancel.load(Ordering::SeqCst) {
        client.shutdown();
        return Err("Grok native pane exited or bridge stopped during leader join".into());
    }
    require_native(runtime, &native, &stop)?;
    let evidence_server =
        ModeEvidenceServer::listen(&mode_socket, session_id).map_err(|error| error.to_string())?;
    let evidence = evidence_server.evidence();
    let (sender, deliveries) = mpsc::channel::<Delivery>();
    let prepared_mirror = match prepare_native(home, session, session_id) {
        Ok(prepared) => Some(prepared),
        Err(reason) => {
            eprintln!("{reason}");
            None
        }
    };
    require_native(runtime, &native, &stop)?;
    let (mut bridge_mail, mirror_issue) = BridgeMail::open(
        MailboxConfig {
            identity: Identity {
                session_ref: session.session_ref.clone(),
                task_ref: session.task_ref.clone(),
                name: session.address.clone(),
                harness: "grok".into(),
                workspace: session.workspace.clone(),
                // open_unknown_mode ignores this required Identity field. Only one-use native
                // evidence can authorize an outgoing send; no mode is published for the session.
                mode: PermissionMode::Prompting,
            },
            mail_dir: run_dir.join("mail"),
            log_path: state_dir
                .join("messages")
                .join(format!("{}.jsonl", session.task_ref)),
        },
        Arc::new(AddressBook::new(&state_dir, &session.task_ref)),
        move |delivery| {
            let _ = sender.send(delivery);
        },
        None,
        prepared_mirror,
    )
    .map_err(|error| error.to_string())?;
    if let Some(issue) = mirror_issue {
        eprintln!("{issue}");
    }
    let bridge = Arc::new(GrokBridge::with_mode_evidence(
        Arc::clone(bridge_mail.mailbox()),
        client,
        evidence,
    ));
    let tool_server = ToolServer::listen(&tool_socket, session_id, bridge.tools())
        .map_err(|error| error.to_string())?;
    require_native(runtime, &native, &stop)?;

    let worker_done = Arc::new(AtomicBool::new(false));
    let worker = {
        let bridge = Arc::clone(&bridge);
        let done = Arc::clone(&worker_done);
        thread::spawn(move || {
            while !done.load(Ordering::SeqCst) {
                match deliveries.recv_timeout(POLL_INTERVAL) {
                    Ok(delivery) => {
                        // The adapter records definite failure or retains uncertain delivery.
                        // Status remains native-observed, never inferred from the RPC reply.
                        let _ = bridge.deliver(delivery, RELAY_TIMEOUT);
                    }
                    Err(mpsc::RecvTimeoutError::Timeout) => {}
                    Err(mpsc::RecvTimeoutError::Disconnected) => break,
                }
            }
        })
    };
    let result = (|| {
        loop {
            if stop.load(Ordering::SeqCst)
                || tmux::native_identity(runtime)
                    .map_err(|error| error.to_string())?
                    .as_ref()
                    != Some(&native)
                || !is_live(native.pid, &native.proc_start)
            {
                break;
            }
            for _ in 0..64 {
                let Some(status) = bridge.poll_status() else {
                    break;
                };
                match status {
                    StatusEvent::Busy => {
                        bridge.tools().apply_status(StatusEvent::Busy);
                        if let Err(reason) = bridge_mail.mirror_status(Activity::Busy, None) {
                            eprintln!("{reason}");
                        }
                    }
                    StatusEvent::Idle => {
                        bridge.tools().apply_status(StatusEvent::Idle);
                        if let Err(reason) = bridge_mail.mirror_status(Activity::Idle, None) {
                            eprintln!("{reason}");
                        }
                    }
                    StatusEvent::WaitingApproval => {
                        bridge.tools().apply_status(StatusEvent::WaitingApproval);
                        if let Err(reason) =
                            bridge_mail.mirror_status(Activity::Waiting, Some("approval"))
                        {
                            eprintln!("{reason}");
                        }
                    }
                    StatusEvent::Detail(detail) => {
                        bridge.tools().apply_status(StatusEvent::Detail(detail));
                    }
                    StatusEvent::Unknown => {}
                }
            }
            thread::sleep(POLL_INTERVAL);
        }
        Ok(())
    })();
    // Stop tool admission first, then revoke unspent native facts. Shutdown wakes any prompt
    // blocked on the joined client without touching the native pane or its leader.
    drop(tool_server);
    evidence_server.close();
    worker_done.store(true, Ordering::SeqCst);
    bridge.shutdown();
    let _ = worker.join();
    bridge_mail.close();
    result
}

fn saved_session_id<'a>(
    session: &'a Session,
    runtime: &TmuxRuntimeLocator,
) -> Result<&'a str, String> {
    let mapping = session
        .provider
        .as_ref()
        .ok_or("Grok has no saved native session ID")?;
    let runtime_id = format!("tmux:{}:{}", runtime.private_server, runtime.session);
    if session.harness != "grok"
        || mapping.provider != "grok"
        || mapping.runtime_id != runtime_id
        || !launch::is_grok_session_id(&mapping.provider_session_id)
    {
        return Err("Grok provider mapping is not for this exact runtime".into());
    }
    Ok(&mapping.provider_session_id)
}

fn require_native(
    runtime: &TmuxRuntimeLocator,
    native: &NativeIdentity,
    stop: &AtomicBool,
) -> Result<(), String> {
    if stop.load(Ordering::SeqCst)
        || !is_live(native.pid, &native.proc_start)
        || tmux::native_identity(runtime)
            .map_err(|error| error.to_string())?
            .as_ref()
            != Some(native)
    {
        return Err("Grok native pane exited or changed before bridge startup completed".into());
    }
    Ok(())
}

fn wait_for_leader(
    runtime: &TmuxRuntimeLocator,
    native: &NativeIdentity,
    stop: &AtomicBool,
    socket: &Path,
    log: &Path,
) -> Result<(), String> {
    let deadline = Instant::now() + LEADER_WAIT;
    loop {
        require_native(runtime, native, stop)?;
        if fs::metadata(socket).is_ok_and(|meta| meta.file_type().is_socket())
            && fs::metadata(log).is_ok_and(|meta| meta.is_file())
        {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(
                "Grok native leader socket or shared registration log did not become ready".into(),
            );
        }
        thread::sleep(POLL_INTERVAL);
    }
}

#[cfg(test)]
mod tests {
    use std::{
        sync::{
            atomic::{AtomicBool, Ordering},
            Arc,
        },
        thread,
        time::{Duration, Instant},
    };

    use taskai_core::registry::{ProviderMapping, Registry, Source};

    use super::{saved_session_id, JoinWatch};

    #[test]
    fn grok_join_watcher_cancels_on_native_loss_and_joins() {
        let stop = Arc::new(AtomicBool::new(false));
        let alive = Arc::new(AtomicBool::new(true));
        let check = Arc::clone(&alive);
        let mut watch = JoinWatch::start(stop, move || check.load(Ordering::SeqCst));
        alive.store(false, Ordering::SeqCst);
        let deadline = Instant::now() + Duration::from_secs(2);
        while !watch.cancel.load(Ordering::SeqCst) && Instant::now() < deadline {
            thread::sleep(Duration::from_millis(10));
        }
        assert!(watch.cancel.load(Ordering::SeqCst));
        watch.finish();
        assert!(watch.join.is_none());
    }

    #[test]
    fn grok_bridge_rejects_a_foreign_runtime_mapping_before_any_socket() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        let task = registry
            .create_task("Task", root.path(), Source::HumanUi)
            .unwrap();
        let key = registry
            .create_session(&task, None, "Grok", "grok", root.path(), Source::HumanUi)
            .unwrap();
        registry.bind_runtime(&key, "taskai-grok-test").unwrap();
        let session = registry.session(&key).unwrap().clone();
        let runtime = session.runtime.as_ref().unwrap();
        assert!(saved_session_id(&session, runtime).is_err());
        registry
            .bind_provider(
                &key,
                ProviderMapping {
                    provider: "grok".into(),
                    provider_session_id: "owned-id".into(),
                    runtime_id: "another-runtime".into(),
                    connector_version: "1.0.77".into(),
                },
            )
            .unwrap();
        let session = registry.session(&key).unwrap().clone();
        assert!(saved_session_id(&session, runtime).is_err());
    }
}
