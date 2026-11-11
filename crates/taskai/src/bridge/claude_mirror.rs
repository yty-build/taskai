// ABOUTME: Prepares a Claude mirror before the bridge mailbox and publishes it after admission is ready.
// ABOUTME: Mirror handlers borrow the mailbox weakly, and teardown drains the door before closing core.

use std::{
    path::{Path, PathBuf},
    sync::{Arc, PoisonError, RwLock, Weak},
};

use taskai_connectors::claude::{
    drift::{drift_check, Drift},
    mirror::{sender_from_book, DoorHandler, Mirror, MirrorConfig, PreparedMirror},
    peer::ClaudeTransport,
};
use taskai_core::{
    mail::{
        Activity, Delivery, Frame, MailPeer, Mailbox, MailboxConfig, PermissionMode,
        SocketTransport, Transport,
    },
    registry::{AddressBook, Registry, Session},
    Error,
};

use crate::{doctor, remote_probes::HarnessAvailability};

/// Passed in only after the caller's Claude readiness check and M3 drift gate succeed.
pub struct PreparedClaudeMirror {
    pub prepared: PreparedMirror,
    pub sessions_dir: PathBuf,
    pub drift: Drift,
}

/// Prepare a bridge's native Claude door only after the shared M7 check has established the
/// configured executable and tested grammar. The caller supplies private paths in tests.
pub fn prepare_for_session(
    session: &Session,
    provider_session_id: &str,
    availability: &HarnessAvailability,
    sessions_dir: PathBuf,
    socket_dir: PathBuf,
) -> Result<PreparedClaudeMirror, String> {
    if let Some(reason) = availability.unavailable.as_ref() {
        return Err(format!("Claude mirror unavailable: {reason}"));
    }
    let check = availability
        .messaging
        .as_ref()
        .ok_or("Claude mirror readiness was not checked")?;
    if !check.is_ready() {
        return Err(format!("Claude mirror unavailable: {}", check.label()));
    }
    let drift = drift_check(availability.version.as_deref(), &[]);
    drift.require().map_err(|error| error.to_string())?;
    let prepared = Mirror::prepare(
        MirrorConfig {
            sessions_dir: sessions_dir.clone(),
            socket_dir,
            name: session.address.clone(),
            harness: session.harness.clone(),
            session_id: provider_session_id.into(),
            cwd: session.workspace.clone(),
        },
        &drift,
    )
    .map_err(|error| error.to_string())?;
    Ok(PreparedClaudeMirror {
        prepared,
        sessions_dir,
        drift,
    })
}

/// Resolve the same native sessions folder the Claude observer uses. A relative override cannot
/// identify the native folder consistently from a detached bridge, so it fails closed.
pub fn native_sessions_dir() -> Result<PathBuf, String> {
    let config = match std::env::var_os("CLAUDE_CONFIG_DIR") {
        Some(value) => PathBuf::from(value),
        None => PathBuf::from(std::env::var_os("HOME").ok_or("HOME is unset")?).join(".claude"),
    };
    if !config.is_absolute() {
        return Err("Claude config directory is not absolute".into());
    }
    Ok(config.join("sessions"))
}

pub fn prepare_native(
    home: &Path,
    session: &Session,
    provider_session_id: &str,
) -> Result<PreparedClaudeMirror, String> {
    let executable = configured_claude_executable(home)?;
    let available = doctor::check_harness("claude", &executable, &doctor::Paths::from_env());
    prepare_for_session(
        session,
        provider_session_id,
        &available,
        native_sessions_dir()?,
        Path::new("/tmp/cc-socks").to_path_buf(),
    )
}

fn configured_claude_executable(home: &Path) -> Result<String, String> {
    let registry = Registry::open(&home.join("state")).map_err(|error| error.to_string())?;
    let configured = registry
        .server("local")
        .and_then(|profile| profile.harnesses.get("claude"))
        .and_then(|settings| settings.executable.as_deref());
    Ok(doctor::configured_executable("claude", configured))
}

/// The mailbox stays usable when Claude readiness or mirror publication fails. A Claude peer
/// gets an explicit refusal instead of TaskAI's socket frames being sent to its native door.
struct BridgeTransport {
    claude: RwLock<Option<Arc<ClaudeTransport>>>,
    direct: SocketTransport,
}

impl BridgeTransport {
    fn new() -> Self {
        Self {
            claude: RwLock::new(None),
            direct: SocketTransport,
        }
    }

    fn enable(&self, transport: ClaudeTransport) {
        *self.claude.write().unwrap_or_else(PoisonError::into_inner) = Some(Arc::new(transport));
    }

    fn disable(&self) {
        *self.claude.write().unwrap_or_else(PoisonError::into_inner) = None;
    }
}

impl Transport for BridgeTransport {
    fn send(&self, to: &MailPeer, frame: &Frame) -> Result<(), Error> {
        if to.harness != "claude" {
            return self.direct.send(to, frame);
        }
        let claude = self
            .claude
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
            .ok_or_else(|| Error::Invalid("Claude mirror unavailable for this bridge".into()))?;
        claude.send(to, frame)
    }
}

struct MailboxDoor {
    book: Arc<AddressBook>,
    mailbox: Weak<Mailbox>,
}

impl DoorHandler for MailboxDoor {
    fn sender(&self, from: &str) -> Option<String> {
        match sender_from_book(self.book.as_ref(), from) {
            Ok(sender) => sender,
            Err(_) => {
                eprintln!("Claude mirror sender lookup unavailable");
                None
            }
        }
    }

    fn accept(&self, frame: Frame) {
        if let Some(mailbox) = self.mailbox.upgrade() {
            mailbox.accept_authenticated(frame);
        }
    }

    fn refuse(&self, _reason: &str) {
        eprintln!("Claude mirror refused an incoming frame");
    }
}

/// The bridge owner sends final core frames before draining and closing the mirror. The handler
/// holds no mirror reference, so admission can drain in-hand frames without a lifetime cycle.
pub struct BridgeMail {
    mailbox: Arc<Mailbox>,
    transport: Arc<BridgeTransport>,
    mirror: Option<Mirror>,
}

impl BridgeMail {
    pub fn open(
        config: MailboxConfig,
        book: Arc<AddressBook>,
        deliver: impl Fn(Delivery) + Send + Sync + 'static,
        initial_mode: Option<PermissionMode>,
        prepared: Option<PreparedClaudeMirror>,
    ) -> Result<(Self, Option<String>), Error> {
        let transport = Arc::new(BridgeTransport::new());
        let mailbox = Arc::new(match initial_mode {
            Some(mode) if mode == config.identity.mode => {
                Mailbox::open(config, book.clone(), transport.clone(), deliver)?
            }
            Some(_) => {
                return Err(Error::Invalid(
                    "bridge initial mode differs from observed native mode".into(),
                ));
            }
            None => Mailbox::open_unknown_mode(config, book.clone(), transport.clone(), deliver)?,
        });
        let (mirror, issue) = if let Some(plan) = prepared {
            let address = plan.prepared.address().to_owned();
            transport.enable(ClaudeTransport::new(
                plan.sessions_dir,
                address,
                Arc::new(SocketTransport),
                plan.drift,
            ));
            let handler = Arc::new(MailboxDoor {
                book,
                mailbox: Arc::downgrade(&mailbox),
            });
            match plan.prepared.publish(handler) {
                Ok(mirror) => (Some(mirror), None),
                Err(error) => {
                    transport.disable();
                    (
                        None,
                        Some(format!("Claude mirror publication failed: {error}")),
                    )
                }
            }
        } else {
            (None, None)
        };
        Ok((
            Self {
                mailbox,
                transport,
                mirror,
            },
            issue,
        ))
    }

    pub fn mailbox(&self) -> &Arc<Mailbox> {
        &self.mailbox
    }

    pub fn mirror_status(
        &mut self,
        activity: Activity,
        waiting_for: Option<&str>,
    ) -> Result<(), String> {
        let Some(mirror) = self.mirror.as_ref() else {
            return Ok(());
        };
        if let Err(error) = mirror.set_status(activity, waiting_for) {
            self.transport.disable();
            if let Some(mirror) = self.mirror.take() {
                mirror.close();
            }
            return Err(format!("Claude mirror status publication failed: {error}"));
        }
        Ok(())
    }

    pub fn close(&mut self) {
        // Core's final exited notices and definite dropped receipts still use Claude's door.
        // Keep its outgoing transport live until those writes have finished. The weak incoming
        // handler can safely hand a late frame to the closed-but-still-owned mailbox while the
        // mirror then drains admission.
        self.mailbox.close();
        if let Some(mirror) = self.mirror.take() {
            mirror.close();
        }
        self.transport.disable();
    }
}

impl Drop for BridgeMail {
    fn drop(&mut self) {
        self.close();
    }
}

#[cfg(test)]
mod tests {
    use std::{
        fs,
        io::{Read, Write},
        os::unix::net::{UnixListener, UnixStream},
        path::Path,
        process::{Child, Command},
        sync::{mpsc, Arc},
        time::Duration,
    };

    use serde_json::{json, Value};
    use taskai_connectors::claude::{
        drift::Drift,
        mirror::{Mirror, MirrorConfig},
        peer::outbound,
    };
    use taskai_core::{
        mail::{
            proc_start, Activity, Directory, Frame, Identity, Letter, MailPeer, MailboxConfig,
            PermissionMode, Priority, SendStatus, Subscription, FEATURE_NOTIFY_IDLE,
        },
        registry::{AddressBook, HarnessSettings, Registry, Source},
        sha256_hex, write_private,
    };
    use tempfile::TempDir;

    use crate::{
        doctor::Check,
        remote_probes::{AuthState, HarnessAvailability},
    };

    use super::{
        configured_claude_executable, prepare_for_session, BridgeMail, PreparedClaudeMirror,
    };

    #[test]
    fn mirror_uses_local_claude_profile_not_sender_executable() {
        let root = TempDir::new().unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        registry
            .set_server_harness(
                "local",
                "claude",
                HarnessSettings {
                    enabled: true,
                    executable: Some("/private/fake-claude".into()),
                    model: None,
                },
                Source::HumanUi,
            )
            .unwrap();
        registry
            .set_server_harness(
                "local",
                "codex",
                HarnessSettings {
                    enabled: true,
                    executable: Some("/private/fake-codex".into()),
                    model: None,
                },
                Source::HumanUi,
            )
            .unwrap();
        let task = registry
            .create_task("Task", root.path(), Source::HumanUi)
            .unwrap();
        let key = registry
            .create_session(&task, None, "Codex", "codex", root.path(), Source::HumanUi)
            .unwrap();
        assert_eq!(
            registry.session(&key).unwrap().launch.executable.as_deref(),
            Some("/private/fake-codex")
        );
        assert_eq!(
            configured_claude_executable(root.path()).unwrap(),
            "/private/fake-claude"
        );
    }

    #[test]
    fn mirror_preparation_requires_shared_readiness_before_private_bind() {
        let root = TempDir::new().unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        let task = registry
            .create_task("Task", root.path(), Source::HumanUi)
            .unwrap();
        let key = registry
            .create_session(&task, None, "Codex", "codex", root.path(), Source::HumanUi)
            .unwrap();
        let session = registry.session(&key).unwrap();
        let sessions = root.path().join("claude-sessions");
        let sockets = root.path().join("cc-socks");
        let mut available = HarnessAvailability {
            hkey: "claude".into(),
            version: Some("2.1.322".into()),
            unavailable: None,
            auth: AuthState::Unknown,
            messaging: Some(Check::NotReady {
                reason: "fixture drift".into(),
                remedy: "fixture remedy".into(),
            }),
        };
        let error = prepare_for_session(
            session,
            "owned-thread",
            &available,
            sessions.clone(),
            sockets.clone(),
        )
        .err()
        .unwrap();
        assert!(error.contains("fixture drift"));
        assert!(!sessions.exists());
        assert!(!sockets.exists());
        available.messaging = Some(Check::Ready("private fixture".into()));
        let prepared = prepare_for_session(
            session,
            "owned-thread",
            &available,
            sessions.clone(),
            sockets.clone(),
        )
        .unwrap();
        assert!(prepared.prepared.address().starts_with("uds:"));
        drop(prepared);
    }

    fn read_json(path: &Path) -> Value {
        serde_json::from_slice(&fs::read(path).unwrap()).unwrap()
    }

    struct OwnedChild(Child);

    impl Drop for OwnedChild {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }

    #[test]
    fn published_door_admits_only_owned_claude_and_closes_before_mailbox() {
        let root = TempDir::new().unwrap();
        let workspace = root.path().join("workspace");
        fs::create_dir(&workspace).unwrap();
        let state = root.path().join("state");
        let mut registry = Registry::open(&state).unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let codex_ref = registry
            .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
            .unwrap();
        let claude_ref = registry
            .create_session(&task, None, "Claude", "claude", &workspace, Source::HumanUi)
            .unwrap();
        let codex_name = registry.session(&codex_ref).unwrap().address.clone();
        let claude_name = registry.session(&claude_ref).unwrap().address.clone();
        let book = Arc::new(AddressBook::new(&state, &task));
        let native_socket = root.path().join("native-claude.sock");
        let pid = std::process::id();
        book.register(&MailPeer {
            session_ref: claude_ref,
            task_ref: task.clone(),
            name: claude_name.clone(),
            harness: "claude".into(),
            workspace: workspace.clone(),
            pid,
            proc_start: proc_start(pid).unwrap(),
            status: Activity::Idle,
            waiting_for: None,
            mailbox: native_socket.clone(),
            features: vec![FEATURE_NOTIFY_IDLE.into()],
            mode: None,
        })
        .unwrap();
        let sessions = root.path().join("sessions");
        let sockets = root.path().join("cc-socks");
        let drift = Drift {
            enabled: true,
            reasons: Vec::new(),
        };
        let prepared = Mirror::prepare(
            MirrorConfig {
                sessions_dir: sessions.clone(),
                socket_dir: sockets.clone(),
                name: codex_name.clone(),
                harness: "codex".into(),
                session_id: "owned-thread".into(),
                cwd: workspace.clone(),
            },
            &drift,
        )
        .unwrap();
        let (sent, received) = mpsc::channel();
        let (mut bridge, issue) = BridgeMail::open(
            MailboxConfig {
                identity: Identity {
                    session_ref: codex_ref.clone(),
                    task_ref: task.clone(),
                    name: codex_name,
                    harness: "codex".into(),
                    workspace: workspace.clone(),
                    mode: PermissionMode::Prompting,
                },
                mail_dir: root.path().join("run/mail"),
                log_path: state.join("messages").join(format!("{task}.jsonl")),
            },
            book.clone(),
            move |delivery| {
                sent.send(delivery).unwrap();
            },
            Some(PermissionMode::Prompting),
            Some(PreparedClaudeMirror {
                prepared,
                sessions_dir: sessions.clone(),
                drift,
            }),
        )
        .unwrap();
        assert!(issue.is_none());
        let card_path = sessions.join(format!("{pid}.json"));
        let card = read_json(&card_path);
        let door = card["messagingSocketPath"].as_str().unwrap();
        let key_path = fs::read_dir(&sessions)
            .unwrap()
            .map(|entry| entry.unwrap().path())
            .find(|path| path.extension().is_some_and(|ext| ext == "key"))
            .unwrap();
        let token = read_json(&key_path)["peerToken"]
            .as_str()
            .unwrap()
            .to_string();
        let frame = outbound(
            &Frame::Message(Letter {
                msg_id: "m1".into(),
                from: claude_name.clone(),
                from_mode: PermissionMode::Prompting,
                hop_chain: Vec::new(),
                priority: Priority::Next,
                body: "private fixture".into(),
            }),
            &format!("uds:{}", native_socket.display()),
        );
        let mut stream = UnixStream::connect(door).unwrap();
        stream
            .write_all(
                format!("{}\n{}\n", json!({"type":"auth", "token": token}), frame).as_bytes(),
            )
            .unwrap();
        drop(stream);
        let delivery = received.recv_timeout(Duration::from_secs(2)).unwrap();
        assert_eq!(delivery.sender, claude_name);
        assert!(delivery.text.contains("private fixture"));
        bridge
            .mirror_status(Activity::Waiting, Some("approval"))
            .unwrap();
        let card = read_json(&card_path);
        assert_eq!(card["status"], "waiting");
        assert_eq!(card["waitingFor"], "approval");
        bridge.close();
        assert!(!card_path.exists() && !key_path.exists() && !Path::new(door).exists());
        assert!(!bridge.mailbox().socket_path().exists());
    }

    #[test]
    fn absent_mirror_refuses_claude_only_and_keeps_core_mailbox() {
        let root = TempDir::new().unwrap();
        let workspace = root.path().join("workspace");
        fs::create_dir(&workspace).unwrap();
        let state = root.path().join("state");
        let mut registry = Registry::open(&state).unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let codex_ref = registry
            .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
            .unwrap();
        let claude_ref = registry
            .create_session(&task, None, "Claude", "claude", &workspace, Source::HumanUi)
            .unwrap();
        let name = registry.session(&codex_ref).unwrap().address.clone();
        let claude_name = registry.session(&claude_ref).unwrap().address.clone();
        let book = Arc::new(AddressBook::new(&state, &task));
        let pid = std::process::id();
        book.register(&MailPeer {
            session_ref: claude_ref,
            task_ref: task.clone(),
            name: claude_name.clone(),
            harness: "claude".into(),
            workspace: workspace.clone(),
            pid,
            proc_start: proc_start(pid).unwrap(),
            status: Activity::Idle,
            waiting_for: None,
            mailbox: root.path().join("native.sock"),
            features: vec![],
            mode: None,
        })
        .unwrap();
        let (mut bridge, issue) = BridgeMail::open(
            MailboxConfig {
                identity: Identity {
                    session_ref: codex_ref,
                    task_ref: task.clone(),
                    name,
                    harness: "codex".into(),
                    workspace,
                    mode: PermissionMode::Prompting,
                },
                mail_dir: root.path().join("run/mail"),
                log_path: state.join("messages").join(format!("{task}.jsonl")),
            },
            book,
            |_| {},
            Some(PermissionMode::Prompting),
            None,
        )
        .unwrap();
        assert!(issue.is_none());
        let result = bridge
            .mailbox()
            .send(&claude_name, "private fixture", false, None);
        assert_eq!(result.status, SendStatus::SendFailed);
        assert!(result.error.unwrap().contains("Claude mirror unavailable"));
        assert!(bridge.mailbox().socket_path().exists());
        bridge.close();
    }

    #[test]
    fn publication_failure_keeps_the_core_mailbox_and_cleans_the_unpublished_door() {
        let root = TempDir::new().unwrap();
        let workspace = root.path().join("workspace");
        fs::create_dir(&workspace).unwrap();
        let state = root.path().join("state");
        let mut registry = Registry::open(&state).unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let session = registry
            .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
            .unwrap();
        let name = registry.session(&session).unwrap().address.clone();
        let sessions = root.path().join("sessions");
        let sockets = root.path().join("cc-socks");
        let drift = Drift {
            enabled: true,
            reasons: Vec::new(),
        };
        let prepared = Mirror::prepare(
            MirrorConfig {
                sessions_dir: sessions.clone(),
                socket_dir: sockets.clone(),
                name: name.clone(),
                harness: "codex".into(),
                session_id: "owned-thread".into(),
                cwd: workspace.clone(),
            },
            &drift,
        )
        .unwrap();
        fs::remove_dir(&sessions).unwrap();
        fs::write(&sessions, b"occupied").unwrap();
        let (mut bridge, issue) = BridgeMail::open(
            MailboxConfig {
                identity: Identity {
                    session_ref: session,
                    task_ref: task.clone(),
                    name,
                    harness: "codex".into(),
                    workspace,
                    mode: PermissionMode::Prompting,
                },
                mail_dir: root.path().join("run/mail"),
                log_path: state.join("messages").join(format!("{task}.jsonl")),
            },
            Arc::new(AddressBook::new(&state, &task)),
            |_| {},
            None,
            Some(PreparedClaudeMirror {
                prepared,
                sessions_dir: sessions.clone(),
                drift,
            }),
        )
        .unwrap();
        assert!(issue.unwrap().contains("Claude mirror publication failed"));
        assert!(bridge.mailbox().socket_path().exists());
        registry.reload().unwrap();
        assert_eq!(
            registry
                .session("s1")
                .unwrap()
                .messaging
                .as_ref()
                .unwrap()
                .mode,
            None
        );
        let send = bridge
            .mailbox()
            .send("unregistered", "private fixture", false, None);
        assert_eq!(send.status, SendStatus::SendFailed);
        assert!(send.error.unwrap().contains("permission mode is unknown"));
        assert!(!sockets
            .join(format!("{}.sock", std::process::id()))
            .exists());
        assert_eq!(fs::read(&sessions).unwrap(), b"occupied");
        bridge.close();
    }

    #[test]
    fn final_claude_frames_reach_owned_peer_before_mirror_cleanup() {
        let root = TempDir::new().unwrap();
        let workspace = root.path().join("workspace");
        fs::create_dir(&workspace).unwrap();
        let state = root.path().join("state");
        let mut registry = Registry::open(&state).unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let bridge_ref = registry
            .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
            .unwrap();
        let claude_ref = registry
            .create_session(&task, None, "Claude", "claude", &workspace, Source::HumanUi)
            .unwrap();
        let bridge_name = registry.session(&bridge_ref).unwrap().address.clone();
        let claude_name = registry.session(&claude_ref).unwrap().address.clone();
        let sessions = root.path().join("sessions");
        let sockets = root.path().join("cc-socks");
        fs::create_dir(&sessions).unwrap();
        fs::create_dir(&sockets).unwrap();
        let native_socket = sockets.join("native.sock");
        let listener = UnixListener::bind(&native_socket).unwrap();
        let mut child = Command::new("sleep");
        child.arg("20");
        taskai_core::mail::strip_messaging_env(&mut child);
        let owned = OwnedChild(child.spawn().unwrap());
        let native_pid = owned.0.id();
        let native_start = proc_start(native_pid).unwrap();
        let native_card = sessions.join(format!("{native_pid}.json"));
        let native_key = sessions.join(format!(
            "{native_pid}.{}.key",
            sha256_hex(native_socket.as_os_str().as_encoded_bytes())
        ));
        write_private(
            &native_card,
            &json!({
                "pid":native_pid, "sessionId":"fixture-claude", "startedAt":1,
                "kind":"interactive", "procStart":native_start, "peerProtocol":1,
                "peerFeatures":["notify_idle"], "messagingSocketPath":native_socket,
                "status":"busy"
            })
            .to_string(),
        )
        .unwrap();
        write_private(
            &native_key,
            &json!({"peerToken":"5".repeat(32), "procStart":native_start,
                "pidDomain":"darwin"})
            .to_string(),
        )
        .unwrap();
        let book = Arc::new(AddressBook::new(&state, &task));
        book.register(&MailPeer {
            session_ref: claude_ref,
            task_ref: task.clone(),
            name: claude_name.clone(),
            harness: "claude".into(),
            workspace: workspace.clone(),
            pid: native_pid,
            proc_start: native_start,
            status: Activity::Busy,
            waiting_for: None,
            mailbox: native_socket.clone(),
            features: vec![FEATURE_NOTIFY_IDLE.into()],
            mode: None,
        })
        .unwrap();
        let drift = Drift {
            enabled: true,
            reasons: Vec::new(),
        };
        let prepared = Mirror::prepare(
            MirrorConfig {
                sessions_dir: sessions.clone(),
                socket_dir: sockets.clone(),
                name: bridge_name.clone(),
                harness: "codex".into(),
                session_id: "owned-thread".into(),
                cwd: workspace.clone(),
            },
            &drift,
        )
        .unwrap();
        let (sent, received) = mpsc::channel();
        let log_path = state.join("messages").join(format!("{task}.jsonl"));
        let (mut bridge, issue) = BridgeMail::open(
            MailboxConfig {
                identity: Identity {
                    session_ref: bridge_ref,
                    task_ref: task,
                    name: bridge_name,
                    harness: "codex".into(),
                    workspace,
                    mode: PermissionMode::Prompting,
                },
                mail_dir: root.path().join("run/mail"),
                log_path: log_path.clone(),
            },
            book,
            move |delivery| {
                sent.send(delivery).unwrap();
            },
            Some(PermissionMode::Prompting),
            Some(PreparedClaudeMirror {
                prepared,
                sessions_dir: sessions.clone(),
                drift,
            }),
        )
        .unwrap();
        assert!(issue.is_none());
        bridge.mailbox().set_activity(Activity::Busy, None);
        let bridge_card = sessions.join(format!("{}.json", std::process::id()));
        let card = read_json(&bridge_card);
        let door = card["messagingSocketPath"].as_str().unwrap().to_string();
        let bridge_key = fs::read_dir(&sessions)
            .unwrap()
            .map(|entry| entry.unwrap().path())
            .find(|path| path.extension().is_some_and(|ext| ext == "key") && *path != native_key)
            .unwrap();
        let token = read_json(&bridge_key)["peerToken"]
            .as_str()
            .unwrap()
            .to_string();
        let reply = format!("uds:{}", native_socket.display());
        let frames = [
            Frame::Subscribe(Subscription {
                msg_id: "s-exit".into(),
                from: claude_name.clone(),
                from_mode: PermissionMode::Prompting,
            }),
            Frame::Message(Letter {
                msg_id: "m-drop".into(),
                from: claude_name.clone(),
                from_mode: PermissionMode::Prompting,
                hop_chain: Vec::new(),
                priority: Priority::Next,
                body: "drop fixture".into(),
            }),
            Frame::Message(Letter {
                msg_id: "m-uncertain".into(),
                from: claude_name,
                from_mode: PermissionMode::Prompting,
                hop_chain: Vec::new(),
                priority: Priority::Next,
                body: "uncertain fixture".into(),
            }),
        ];
        let mut incoming = UnixStream::connect(&door).unwrap();
        writeln!(incoming, "{}", json!({"type":"auth", "token":token})).unwrap();
        for frame in frames {
            writeln!(incoming, "{}", outbound(&frame, &reply)).unwrap();
        }
        drop(incoming);
        let dropped = received.recv_timeout(Duration::from_secs(2)).unwrap();
        let uncertain = received.recv_timeout(Duration::from_secs(2)).unwrap();
        assert_eq!(dropped.msg_id, "m-drop");
        assert_eq!(uncertain.msg_id, "m-uncertain");
        bridge.mailbox().delivery_in_flight(&uncertain.id);
        bridge.close();
        listener.set_nonblocking(true).unwrap();
        let mut output = Vec::new();
        loop {
            let mut stream = match listener.accept() {
                Ok((stream, _)) => stream,
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => break,
                Err(error) => panic!("private Claude listener failed: {error}"),
            };
            let mut body = String::new();
            stream.read_to_string(&mut body).unwrap();
            let frame: Value = serde_json::from_str(body.lines().nth(1).unwrap()).unwrap();
            output.push(frame);
        }
        assert_eq!(output.len(), 2, "one dropped receipt and one exited notice");
        assert!(output.iter().any(|frame| {
            frame["action"] == "peer_message_status"
                && frame["orig_msg_id"] == "m-drop"
                && frame["status"] == "dropped"
        }));
        assert!(output.iter().any(|frame| {
            frame["action"] == "peer_idle_notice"
                && frame["orig_msg_id"] == "s-exit"
                && frame["state"] == "exited"
        }));
        assert!(!output
            .iter()
            .any(|frame| frame["orig_msg_id"] == "m-uncertain"));
        assert!(fs::read_to_string(log_path)
            .unwrap()
            .contains("delivery uncertain"));
        assert!(!bridge_card.exists() && !bridge_key.exists() && !Path::new(&door).exists());
        assert!(!bridge.mailbox().socket_path().exists());
        assert!(native_card.exists() && native_key.exists() && native_socket.exists());
    }
}
