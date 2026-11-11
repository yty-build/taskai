// ABOUTME: One Codex daemon stream owner for the exact saved TaskAI thread.
// ABOUTME: Starts one thread with durable intent, serves tools, and closes on native death.

use std::{
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicBool, Ordering},
        mpsc, Arc,
    },
    thread,
    time::Duration,
};

use taskai_connectors::{
    codex::daemon::{DaemonClient, DaemonEvent, DaemonShutdown, EffectivePermissionMode},
    Error,
};
use taskai_core::{
    mail::{is_live, Delivery, Identity, MailboxConfig, PermissionMode},
    registry::{AddressBook, ProviderMapping, Registry, Session, TmuxRuntimeLocator},
};

use super::{
    claude_mirror::{prepare_native, BridgeMail},
    codex_intent::{CodexIntents, CreationState, FirstTurnState, SavedThreadOrigin},
    codex_relay::CodexRelay,
    gate_control::{GateAck, GateControl},
    MailboxTools,
};
use crate::{
    gate,
    tmux::{self, NativeIdentity},
};

const RPC_TIMEOUT: Duration = Duration::from_secs(10);
const EVENT_WAIT: Duration = Duration::from_millis(100);
const WATCH_INTERVAL: Duration = Duration::from_millis(200);

/// Joins the owned connection watcher on every exit, including startup errors before a
/// mailbox exists. It never shuts down the daemon itself, only this bridge connection.
struct WatcherGuard {
    done: Arc<AtomicBool>,
    join: Option<thread::JoinHandle<()>>,
}

struct WatchTarget {
    home: PathBuf,
    session_ref: String,
    native: NativeIdentity,
}

impl WatcherGuard {
    fn finish(&mut self) {
        self.done.store(true, Ordering::SeqCst);
        if let Some(join) = self.join.take() {
            let _ = join.join();
        }
    }
}

impl Drop for WatcherGuard {
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
    let state_dir = home.join("state");
    let socket = daemon_socket_path();
    let mut client =
        DaemonClient::connect(&socket, RPC_TIMEOUT).map_err(|error| error.to_string())?;
    let shutdown = client
        .shutdown_handle()
        .map_err(|error| error.to_string())?;
    let watcher_done = Arc::new(AtomicBool::new(false));
    let fresh_gate_watch = Arc::new(AtomicBool::new(false));
    let watch_runtime = runtime.clone();
    let watch_native = native.clone();
    let join = spawn_watcher(
        WatchTarget {
            home: home.to_path_buf(),
            session_ref: session.session_ref.clone(),
            native: native.clone(),
        },
        Arc::clone(&fresh_gate_watch),
        Arc::clone(&stop),
        Arc::clone(&watcher_done),
        shutdown,
        move || {
            tmux::native_identity(&watch_runtime)
                .is_ok_and(|identity| identity.as_ref() == Some(&watch_native))
        },
    );
    let mut watcher = WatcherGuard {
        done: watcher_done,
        join: Some(join),
    };
    let initialization = client
        .initialize(RPC_TIMEOUT)
        .map_err(|error| error.to_string())?;
    require_native(runtime, &native, &stop)?;
    let mut registry = Registry::open(&state_dir).map_err(|error| error.to_string())?;
    let saved = registry
        .session(&session.session_ref)
        .ok_or("Codex session disappeared")?;
    if saved.runtime.as_ref() != Some(runtime)
        || saved.task_ref != session.task_ref
        || saved.workspace != session.workspace
    {
        return Err("Codex bridge registry mapping changed".into());
    }
    let intents = CodexIntents::new(home, &session.session_ref, native.clone())?;
    let runtime_id = format!("tmux:{}:{}", runtime.private_server, runtime.session);
    let (thread_id, origin) = match saved.provider.as_ref() {
        Some(mapping) => {
            if mapping.provider != "codex"
                || mapping.runtime_id != runtime_id
                || mapping.provider_session_id.is_empty()
            {
                return Err("Codex saved provider mapping is not its exact runtime".into());
            }
            let id = mapping.provider_session_id.clone();
            let origin = match intents.saved_thread_origin(&id) {
                Ok(origin) => origin,
                Err(error) => {
                    if !intents.has_gate_adoption(&id)? {
                        return Err(error);
                    }
                    gate::wait_for_identity(
                        home,
                        &session.session_ref,
                        &native,
                        Duration::from_secs(5),
                    )?;
                    intents.adopt_saved_creation(&id)?;
                    SavedThreadOrigin::FreshGate
                }
            };
            client
                .attach_owned_thread(&id, RPC_TIMEOUT)
                .map_err(|error| error.to_string())?;
            require_native(runtime, &native, &stop)?;
            (id, origin)
        }
        None => {
            gate::wait_for_identity(home, &session.session_ref, &native, Duration::from_secs(5))?;
            let id = match intents.creation_state()? {
                CreationState::Missing => {
                    intents.begin_creation()?;
                    let id = client
                        .start_thread(&session.workspace, RPC_TIMEOUT)
                        .map_err(|error| error.to_string())?;
                    intents.record_created(&id)?;
                    require_native(runtime, &native, &stop)?;
                    id
                }
                CreationState::Pending => {
                    return Err(
                        "Codex thread/start outcome is unknown; no replacement thread was created"
                            .into(),
                    );
                }
                CreationState::Created(id) => {
                    client
                        .attach_owned_thread(&id, RPC_TIMEOUT)
                        .map_err(|error| error.to_string())?;
                    require_native(runtime, &native, &stop)?;
                    id
                }
            };
            registry
                .bind_provider(
                    &session.session_ref,
                    ProviderMapping {
                        provider: "codex".into(),
                        provider_session_id: id.clone(),
                        runtime_id: runtime_id.clone(),
                        connector_version: initialization.daemon_version.unwrap_or_default(),
                    },
                )
                .map_err(|error| error.to_string())?;
            (id, SavedThreadOrigin::FreshGate)
        }
    };
    let effective = client
        .effective_permission()
        .ok_or("Codex daemon returned no effective policy")?;
    let mode =
        permission_mode(effective.mode).ok_or("Codex effective approval policy is unknown")?;
    let fresh_gate = origin == SavedThreadOrigin::FreshGate;
    fresh_gate_watch.store(fresh_gate, Ordering::SeqCst);
    let human_first = if fresh_gate {
        let first_state = intents.first_turn_state(&thread_id)?;
        if first_state == FirstTurnState::Pending {
            return Err("Codex first turn acceptance is unknown; no retry was sent".into());
        }
        first_state == FirstTurnState::Missing
    } else {
        !intents.human_established(&thread_id)?
    };
    let (sender, deliveries) = mpsc::channel::<Delivery>();
    require_native(runtime, &native, &stop)?;
    let prepared_mirror = match prepare_native(home, session, &thread_id) {
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
                harness: "codex".into(),
                workspace: session.workspace.clone(),
                mode,
            },
            mail_dir: home.join("run/mail"),
            log_path: state_dir
                .join("messages")
                .join(format!("{}.jsonl", session.task_ref)),
        },
        Arc::new(AddressBook::new(&state_dir, &session.task_ref)),
        move |delivery| {
            let _ = sender.send(delivery);
        },
        Some(mode),
        prepared_mirror,
    )
    .map_err(|error| error.to_string())?;
    if let Some(reason) = mirror_issue {
        eprintln!("{reason}");
    }
    let mailbox = Arc::clone(bridge_mail.mailbox());
    let mut gate = if human_first {
        Some(GateControl::listen(
            home,
            &session.session_ref,
            runtime.clone(),
            native.clone(),
            thread_id.clone(),
        )?)
    } else {
        None
    };
    if let Err(error) = require_native(runtime, &native, &stop) {
        bridge_mail.close();
        return Err(error);
    }
    let mut relay = CodexRelay::new(client, Arc::clone(&mailbox), human_first);
    let mut human_proven = !human_first;
    let mut tools = MailboxTools::new(Arc::clone(&mailbox));
    let result = (|| {
        loop {
            if stop.load(Ordering::SeqCst)
                || tmux::native_identity(runtime)
                    .map_err(|error| error.to_string())?
                    .as_ref()
                    != Some(&native)
            {
                break;
            }
            if let Some(control) = gate.as_mut() {
                if let Some(request) = control.poll()? {
                    control.validate_native()?;
                    if intents.begin_first_turn(&thread_id).is_err() {
                        control.ack(GateAck::Unknown);
                        continue;
                    }
                    control.validate_native()?;
                    let result = relay
                        .port_mut()
                        .first_human_message(&request.text, RPC_TIMEOUT);
                    match result {
                        Ok(_) => {
                            if intents.record_first_turn_accepted(&thread_id).is_err() {
                                control.ack(GateAck::Unknown);
                                return Err(
                                    "Codex accepted first turn but its receipt could not be saved"
                                        .into(),
                                );
                            }
                            control.ack(GateAck::Accepted);
                            human_proven = true;
                            for result in relay.human_turn_accepted(RPC_TIMEOUT) {
                                if let Err(Error::Uncertain(_)) = result {
                                    // The mailbox keeps that delivery in flight.
                                }
                            }
                            control.close();
                            gate = None;
                        }
                        Err(Error::Uncertain(_)) => {
                            control.ack(GateAck::Unknown);
                            return Err(
                                "Codex first turn acceptance is unknown; no retry was sent".into(),
                            );
                        }
                        Err(_) => {
                            control.ack(GateAck::Rejected);
                            return Err(
                                "Codex refused the first human turn; no retry was sent".into()
                            );
                        }
                    }
                }
            }
            // Process native frames before relaying another queued delivery. An exact-thread
            // policy update preceding a tool call must update the mailbox before that tool sends.
            match relay.port_mut().next_event(EVENT_WAIT) {
                Ok(Some(DaemonEvent::EffectivePermission(permission))) => mailbox
                    .set_effective_mode(permission_mode(permission.mode))
                    .map_err(|error| {
                        format!(
                            "Codex effective policy update could not be published; outgoing mail disabled: {error}"
                        )
                    })?,
                Ok(Some(DaemonEvent::ToolCall(call))) => relay
                    .port_mut()
                    .answer_tool_call(call, &mut tools)
                    .map_err(|error| error.to_string())?,
                Ok(Some(DaemonEvent::Activity(activity))) => {
                    let reported = relay.native_activity(activity);
                    if let Err(reason) = bridge_mail.mirror_status(reported, activity.waiting_for()) {
                        eprintln!("{reason}");
                    }
                }
                Ok(Some(DaemonEvent::Detail(detail))) => relay.native_detail(&detail),
                Ok(Some(DaemonEvent::TurnStarted(_))) => {
                    // While the relay is gated, only the exact native TUI can start a turn.
                    // An observed turn establishes human origin without TaskAI writing one.
                    if origin == SavedThreadOrigin::DirectResume && !human_proven {
                        intents.record_native_human_turn(&thread_id)?;
                        human_proven = true;
                        for result in relay.human_turn_accepted(RPC_TIMEOUT) {
                            if let Err(Error::Uncertain(_)) = result {
                                // Keep the core delivery in flight; no automatic retry.
                            }
                        }
                    }
                }
                Ok(Some(DaemonEvent::TurnCompleted(_) | DaemonEvent::UnansweredRequest { .. }))
                | Ok(None) => {}
                Err(_error)
                    if stop.load(Ordering::SeqCst) || !is_live(native.pid, &native.proc_start) =>
                {
                    break
                }
                Err(error) => return Err(error.to_string()),
            }
            while let Ok(delivery) = deliveries.try_recv() {
                let _ = relay.delivery(delivery, RPC_TIMEOUT);
            }
        }
        Ok(())
    })();
    watcher.finish();
    if let Some(control) = gate {
        control.close();
    }
    bridge_mail.close();
    result
}

fn permission_mode(mode: EffectivePermissionMode) -> Option<PermissionMode> {
    match mode {
        EffectivePermissionMode::Prompting => Some(PermissionMode::Prompting),
        EffectivePermissionMode::Bypass => Some(PermissionMode::Bypass),
        EffectivePermissionMode::Unknown => None,
    }
}

fn daemon_socket_path() -> PathBuf {
    let home = std::env::var_os("CODEX_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            PathBuf::from(std::env::var_os("HOME").unwrap_or_default()).join(".codex")
        });
    home.join("app-server-control/app-server-control.sock")
}

fn require_native(
    runtime: &TmuxRuntimeLocator,
    native: &NativeIdentity,
    stop: &AtomicBool,
) -> Result<(), String> {
    if stop.load(Ordering::SeqCst)
        || tmux::native_identity(runtime)
            .map_err(|error| error.to_string())?
            .as_ref()
            != Some(native)
    {
        return Err("Codex native pane exited or changed before bridge startup completed".into());
    }
    Ok(())
}

fn spawn_watcher(
    target: WatchTarget,
    fresh_gate: Arc<AtomicBool>,
    stop: Arc<AtomicBool>,
    done: Arc<AtomicBool>,
    shutdown: DaemonShutdown,
    native_owned: impl Fn() -> bool + Send + 'static,
) -> thread::JoinHandle<()> {
    thread::spawn(move || {
        while !done.load(Ordering::SeqCst) {
            if stop.load(Ordering::SeqCst)
                || !is_live(target.native.pid, &target.native.proc_start)
                || !native_owned()
                || (fresh_gate.load(Ordering::SeqCst)
                    && !provider_child_live(&target.home, &target.session_ref, &target.native))
            {
                shutdown.shutdown();
                return;
            }
            thread::sleep(WATCH_INTERVAL);
        }
    })
}

fn provider_child_live(home: &Path, session_ref: &str, native: &NativeIdentity) -> bool {
    let path = home
        .join("run/codex")
        .join(format!("{session_ref}.provider.json"));
    let Ok(bytes) = std::fs::read(path) else {
        return true;
    };
    if bytes.len() > 4096 {
        return false;
    }
    let Ok(value) = serde_json::from_slice::<serde_json::Value>(&bytes) else {
        return false;
    };
    if value["paneId"] != native.pane_id
        || value["gatePid"] != native.pid
        || value["gateProcStart"] != native.proc_start
    {
        // An older gate may leave its metadata file after its provider exits.
        return true;
    }
    let (Some(pid), Some(start)) = (
        value["providerPid"]
            .as_u64()
            .and_then(|pid| u32::try_from(pid).ok()),
        value["providerProcStart"].as_str(),
    ) else {
        return false;
    };
    is_live(pid, start)
}

#[cfg(test)]
mod tests {
    use std::{
        os::unix::net::UnixListener,
        process,
        sync::{
            atomic::{AtomicBool, Ordering},
            mpsc, Arc,
        },
        thread,
        time::{Duration, Instant},
    };

    use serde_json::{json, Value};
    use taskai_connectors::codex::daemon::DaemonClient;
    use taskai_core::mail::proc_start;
    use tempfile::TempDir;
    use tungstenite::{accept, Message};

    use super::{spawn_watcher, NativeIdentity, WatchTarget};

    #[test]
    fn bridge_stop_wakes_a_written_codex_rpc_without_waiting_for_its_deadline() {
        let root = TempDir::new().unwrap();
        let socket = root.path().join("daemon.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        let (written_tx, written_rx) = mpsc::channel();
        let server = thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(5)))
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
            written_tx.send(()).unwrap();
            let _ = ws.read();
        });
        let mut client = DaemonClient::connect(&socket, Duration::from_secs(10)).unwrap();
        client.initialize(Duration::from_secs(10)).unwrap();
        let shutdown = client.shutdown_handle().unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let done = Arc::new(AtomicBool::new(false));
        let pid = process::id();
        let native = NativeIdentity {
            pane_id: "%fixture".into(),
            pid,
            proc_start: proc_start(pid).unwrap(),
        };
        let watcher = spawn_watcher(
            WatchTarget {
                home: root.path().into(),
                session_ref: "s1".into(),
                native,
            },
            Arc::new(AtomicBool::new(false)),
            Arc::clone(&stop),
            Arc::clone(&done),
            shutdown,
            || true,
        );
        let cwd = root.path().to_path_buf();
        let rpc = thread::spawn(move || client.start_thread(&cwd, Duration::from_secs(10)));
        written_rx.recv_timeout(Duration::from_secs(2)).unwrap();
        let stopped_at = Instant::now();
        stop.store(true, Ordering::SeqCst);
        assert!(rpc.join().unwrap().is_err());
        assert!(stopped_at.elapsed() < Duration::from_secs(2));
        done.store(true, Ordering::SeqCst);
        watcher.join().unwrap();
        server.join().unwrap();
    }
}
