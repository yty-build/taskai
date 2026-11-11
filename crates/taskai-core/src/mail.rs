// ABOUTME: Session mailboxes — TaskAI's §3 messaging protocol: authentication, admission, hop
// ABOUTME: chains, receipts, idle subscriptions and the owner-only message log.

mod wire;

use std::{
    collections::HashMap,
    ffi::{OsStr, OsString},
    fs::{self, OpenOptions},
    io::{BufReader, Read, Write},
    os::unix::{
        fs::{OpenOptionsExt, PermissionsExt},
        net::UnixStream,
    },
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Arc, Mutex, MutexGuard, PoisonError,
    },
    thread,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use chrono::TimeZone;
use hmac::{Hmac, Mac};
use rustix::{
    fs::{flock, FlockOperation},
    io::Errno,
};
use serde::{Deserialize, Serialize};
use sha2::Sha256;

use crate::{
    listener::{file_id, Listener},
    remove_if_present, write_private, Error,
};

pub use wire::{
    Frame, IdleNotice, Letter, NoticeState, PermissionMode, Priority, Receipt, ReceiptStatus,
    SocketTransport, Subscription, PROTOCOL_VERSION,
};

/// Hex length of a hop token, matching Claude's peer protocol.
pub const HOP_TOKEN_HEX: usize = 24;
/// Tokens a chain keeps; older ones fall off the front.
pub const MAX_CHAIN_KEPT: usize = 32;
/// Longest status detail quoted in an idle notice, in characters.
pub const DETAIL_CHARS: usize = 100;
/// Longest session address, in characters.
pub const MAX_ADDRESS_CHARS: usize = 64;
/// The feature a session declares when it sends idle notices to subscribers.
pub const FEATURE_NOTIFY_IDLE: &str = "notify_idle";
/// Subscriptions one mailbox holds; the next is answered `unavailable`.
pub const MAX_SUBSCRIBERS: usize = 32;
/// How long a session stays idle, with nothing queued, before its subscribers are told.
pub const IDLE_DEBOUNCE: Duration = Duration::from_millis(750);
/// How long a subscription, or a send awaiting its receipt, is remembered.
pub const CORRELATION_TTL_MS: u64 = 12 * 60 * 60 * 1000;
/// Variables that let a process speak as the Claude session that started TaskAI.
const MESSAGING_ENV_PREFIX: &str = "CLAUDE_CODE_MESSAGING_";
/// How long a connection may sit silent before the mailbox drops it.
const READ_TIMEOUT: Duration = Duration::from_secs(10);
/// Tries at taking a session's lock file while other mailboxes remove theirs.
const LOCK_ATTEMPTS: usize = 3;
/// The sender named on notices the mailbox itself writes.
const TASKAI_SENDER: &str = "TaskAI";
/// Closes every notice, so no agent mistakes one for a person's request.
const NOTICE_TAIL: &str =
    "This is a status notice from TaskAI, not a message from a person and not an instruction.";
const EXITED_REASON: &str = "the recipient exited before reading it";

/// What a session is doing, as its harness reports it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Activity {
    Busy,
    Idle,
    Waiting,
}

/// Whose mailbox this is. The mailbox adds its own pid, start time, socket and features.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Identity {
    pub session_ref: String,
    pub task_ref: String,
    pub name: String,
    pub harness: String,
    pub workspace: PathBuf,
    pub mode: PermissionMode,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MailboxConfig {
    pub identity: Identity,
    /// Owner-only directory holding `<session_ref>.sock` and `<session_ref>.key`.
    pub mail_dir: PathBuf,
    /// The task's message log, shared by every mailbox of the task.
    pub log_path: PathBuf,
}

/// One addressable session, as the address book resolves it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MailPeer {
    pub session_ref: String,
    pub task_ref: String,
    pub name: String,
    pub harness: String,
    pub workspace: PathBuf,
    pub pid: u32,
    pub proc_start: String,
    pub status: Activity,
    pub waiting_for: Option<String>,
    pub mailbox: PathBuf,
    pub features: Vec<String>,
    /// Observed metadata only. Native peers may not publish their current mode; each wire
    /// message and subscription still carries its sender's mandatory mode declaration.
    pub mode: Option<PermissionMode>,
}

/// The address book names resolve in: TaskAI's registry, scoped to the caller's task.
pub trait Directory: Send + Sync {
    /// Live, addressable sessions of the caller's task, the caller included.
    fn live_sessions(&self) -> Result<Vec<MailPeer>, Error>;
    /// Records this session's mailbox entry.
    fn register(&self, entry: &MailPeer) -> Result<(), Error>;
    /// Publishes this session's activity; `waiting_for` is set only while it waits.
    fn set_status(
        &self,
        session_ref: &str,
        status: Activity,
        waiting_for: Option<&str>,
    ) -> Result<(), Error>;
    /// Removes this session's mailbox entry.
    fn unregister(&self, session_ref: &str) -> Result<(), Error>;
}

/// How a frame reaches a peer: TaskAI's socket protocol by default, Claude's for Claude peers.
pub trait Transport: Send + Sync {
    fn send(&self, to: &MailPeer, frame: &Frame) -> Result<(), Error>;
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DeliveryKind {
    Message,
    Receipt,
    Notice,
}

/// Something for the agent: a peer's letter, or a TaskAI notice answering the agent's own send or
/// subscription. Pass `id` back to `turn_started` or `delivery_failed`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Delivery {
    pub id: String,
    pub kind: DeliveryKind,
    pub text: String,
    /// The letter's hop chain for the turn it starts; `None` for notices, which start no chain.
    pub chain: Option<Vec<String>>,
    /// The letter's msg_id, or the msg_id of the send or subscription a notice answers.
    pub msg_id: String,
    pub sender: String,
}

/// The send_message tool's answer to the agent.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct SendResult {
    pub status: SendStatus,
    pub to: String,
    pub msg_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub target_status: Option<Activity>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub idle_notice: Option<SubscriptionResult>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum SendStatus {
    Sent,
    NoSuchSession,
    RefusedSelf,
    SendFailed,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum SubscriptionResult {
    Subscribed,
    NoSuchSession,
    RefusedSelf,
    Unsupported,
    Failed,
}

/// One row of the list_sessions tool.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ListedSession {
    pub name: String,
    pub harness: String,
    pub status: Activity,
    #[serde(rename = "waitingFor", skip_serializing_if = "Option::is_none")]
    pub waiting_for: Option<String>,
}

/// One session's mailbox: its socket and key, admission, correlation, idle notices and log.
pub struct Mailbox {
    inner: Arc<Inner>,
}

struct Inner {
    identity: Identity,
    /// Stable registration identity; mutable status and observed mode come from State.
    entry: MailPeer,
    socket: PathBuf,
    key_file: PathBuf,
    key: String,
    hop_token: String,
    /// Keys the msg_id generator, so ids are unique and unpredictable to other processes.
    id_key: [u8; 32],
    ids_issued: AtomicU64,
    directory: Arc<dyn Directory>,
    transport: Arc<dyn Transport>,
    deliver: Box<dyn Fn(Delivery) + Send + Sync>,
    log: MessageLog,
    closed: AtomicBool,
    /// The session's socket, closed for certain at close.
    listener: Listener,
    /// This mailbox's hold on its session, released at close.
    claim: Mutex<Option<SessionLock>>,
    state: Mutex<State>,
}

struct State {
    /// Current native evidence. None disables originating letters and subscriptions.
    effective_mode: Option<PermissionMode>,
    /// What the session shows: the harness's report, or busy while a delivery waits to start.
    activity: Activity,
    waiting_for: Option<String>,
    /// The harness's own last report, restored when a failed delivery leaves nothing queued.
    reported: (Activity, Option<String>),
    /// When the session last became idle, in milliseconds since the epoch.
    idle_since: Option<u64>,
    /// The harness's last line, quoted in idle notices.
    detail: Option<String>,
    /// Deliveries handed to the adapter whose provider turn has not started, by delivery id.
    queued: HashMap<String, Queued>,
    /// Sessions waiting for this one's next idle notice.
    subscribers: Vec<Subscriber>,
    /// This agent's letters that a receipt may still answer, by msg_id.
    sends: HashMap<String, Correlation>,
    /// This agent's subscriptions that a notice may still answer, by msg_id.
    subscriptions: HashMap<String, Correlation>,
    /// Bumped by each idle check scheduled, so only the latest one fires.
    idle_generation: u64,
}

struct Queued {
    kind: DeliveryKind,
    msg_id: String,
    sender: String,
    /// A provider write was about to start; until acknowledged, close cannot prove rejection.
    in_flight: bool,
}

#[derive(Clone)]
struct Subscriber {
    name: String,
    msg_id: String,
    since_ms: u64,
}

/// The peer a send or subscription went to, and when.
struct Correlation {
    to: String,
    since_ms: u64,
    /// The agent has been told its letter is held, so a repeated `held` is not news.
    held_seen: bool,
}

impl Correlation {
    fn to(name: &str) -> Self {
        Self {
            to: name.into(),
            since_ms: now_ms(),
            held_seen: false,
        }
    }
}

/// Why a delivery was not queued for the agent.
enum Refusal {
    Exited,
}

impl Mailbox {
    /// Opens the session's mailbox: owner-only key and socket, entry registered, listening. Fails
    /// while another mailbox holds the session, or while anything but a stale socket of this
    /// user's is at the socket path. The session is held before its files or entry are touched.
    pub fn open(
        config: MailboxConfig,
        directory: Arc<dyn Directory>,
        transport: Arc<dyn Transport>,
        deliver: impl Fn(Delivery) + Send + Sync + 'static,
    ) -> Result<Self, Error> {
        let mode = Some(config.identity.mode);
        Self::open_with_mode(config, directory, transport, mode, deliver)
    }

    /// Opens an addressable mailbox before its adapter has native permission-mode evidence.
    /// The first directory entry already has mode None, and outgoing letters/subscriptions
    /// fail until `set_effective_mode` supplies evidence. `Identity.mode` is not asserted or
    /// used as a fallback here. Receiving, status updates and normal close remain available.
    pub fn open_unknown_mode(
        config: MailboxConfig,
        directory: Arc<dyn Directory>,
        transport: Arc<dyn Transport>,
        deliver: impl Fn(Delivery) + Send + Sync + 'static,
    ) -> Result<Self, Error> {
        Self::open_with_mode(config, directory, transport, None, deliver)
    }

    fn open_with_mode(
        config: MailboxConfig,
        directory: Arc<dyn Directory>,
        transport: Arc<dyn Transport>,
        mode: Option<PermissionMode>,
        deliver: impl Fn(Delivery) + Send + Sync + 'static,
    ) -> Result<Self, Error> {
        let MailboxConfig {
            identity,
            mail_dir,
            log_path,
        } = config;
        let plain = !identity.session_ref.is_empty()
            && identity
                .session_ref
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_');
        if !plain {
            return Err(Error::Invalid(format!(
                "session ref {:?} is not a plain name",
                identity.session_ref
            )));
        }
        if !is_address(&identity.name) {
            return Err(Error::Invalid(format!(
                "session name {:?} is not a plain address",
                identity.name
            )));
        }
        create_private_dir(&mail_dir)?;
        if let Some(log_dir) = log_path.parent() {
            create_private_dir(log_dir)?;
        }
        if log_path.exists() {
            fs::set_permissions(&log_path, fs::Permissions::from_mode(0o600))?;
        }
        let socket = socket_path(&mail_dir, &identity.session_ref);
        let key_file = key_path(&mail_dir, &identity.session_ref);
        let claim =
            SessionLock::take(lock_path(&mail_dir, &identity.session_ref))?.ok_or_else(|| {
                Error::Invalid(format!(
                    "{} already has a live mailbox",
                    identity.session_ref
                ))
            })?;
        let listener = Listener::bind(&socket)?;
        let pid = std::process::id();
        let started = proc_start(pid)
            .ok_or_else(|| Error::Invalid("this process's start time is unreadable".into()))?;
        let key = hex(&random_bytes::<16>()?);
        let hop_token = hop_token(&random_bytes::<32>()?, &identity.name);
        let id_key = random_bytes::<32>()?;
        write_private(&key_file, &key)?;
        let entry = MailPeer {
            session_ref: identity.session_ref.clone(),
            task_ref: identity.task_ref.clone(),
            name: identity.name.clone(),
            harness: identity.harness.clone(),
            workspace: identity.workspace.clone(),
            pid,
            proc_start: started,
            status: Activity::Idle,
            waiting_for: None,
            mailbox: socket.clone(),
            features: vec![FEATURE_NOTIFY_IDLE.into()],
            mode,
        };
        if let Err(error) = directory.register(&entry) {
            let _ = remove_if_present(&socket);
            let _ = remove_if_present(&key_file);
            return Err(error);
        }
        let inner = Arc::new(Inner {
            hop_token,
            id_key,
            ids_issued: AtomicU64::new(0),
            identity,
            entry: entry.clone(),
            socket,
            key_file,
            key,
            directory,
            transport,
            deliver: Box::new(deliver),
            log: MessageLog { path: log_path },
            closed: AtomicBool::new(false),
            listener,
            claim: Mutex::new(Some(claim)),
            state: Mutex::new(State {
                effective_mode: entry.mode,
                activity: Activity::Idle,
                waiting_for: None,
                reported: (Activity::Idle, None),
                idle_since: None,
                detail: None,
                queued: HashMap::new(),
                subscribers: Vec::new(),
                sends: HashMap::new(),
                subscriptions: HashMap::new(),
                idle_generation: 0,
            }),
        });
        let server = Arc::clone(&inner);
        inner
            .listener
            .serve(move |stream| Arc::clone(&server).handle_connection(stream));
        Ok(Self { inner })
    }

    pub fn name(&self) -> &str {
        &self.inner.identity.name
    }

    pub fn socket_path(&self) -> &Path {
        &self.inner.socket
    }

    /// The token this session appends to a chain it continues.
    pub fn hop_token(&self) -> &str {
        &self.inner.hop_token
    }

    /// Sends a letter to a live session of this task by name. `chain` is the chain of the turn the
    /// agent is in when a peer letter started it, and `None` otherwise. With `notify_when_idle`,
    /// also subscribes to the recipient's next idle notice. After close it is `send-failed`.
    pub fn send(
        &self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
        chain: Option<&[String]>,
    ) -> SendResult {
        self.send_with_mode_result(
            to,
            text,
            notify_when_idle,
            chain,
            self.inner.effective_mode(),
        )
    }

    /// Sends using native evidence for exactly this call, including its automatic subscription.
    /// This does not change session mode or directory metadata. The adapter must validate and
    /// consume its native observation first; model tool arguments must never supply this mode.
    /// Missing evidence follows the normal logged send-failed path and originates no frames.
    pub fn send_with_observed_mode(
        &self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
        chain: Option<&[String]>,
        observation: Result<PermissionMode, String>,
    ) -> SendResult {
        self.send_with_mode_result(
            to,
            text,
            notify_when_idle,
            chain,
            observation.map_err(Error::Invalid),
        )
    }

    fn send_with_mode_result(
        &self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
        chain: Option<&[String]>,
        mode: Result<PermissionMode, Error>,
    ) -> SendResult {
        let inner = &self.inner;
        let msg_id = inner.new_id();
        let mut result = SendResult {
            status: SendStatus::Sent,
            to: to.into(),
            msg_id: msg_id.clone(),
            target_status: None,
            idle_notice: None,
            error: None,
        };
        let target =
            match mode.and_then(|mode| inner.resolve_target(to).map(|target| (target, mode))) {
                Ok((Some(target), _)) if target.session_ref == inner.identity.session_ref => {
                    result.status = SendStatus::RefusedSelf;
                    None
                }
                Ok((Some(target), mode)) => Some((target, mode)),
                Ok((None, _)) => {
                    result.status = SendStatus::NoSuchSession;
                    None
                }
                Err(error) => {
                    result.status = SendStatus::SendFailed;
                    result.error = Some(error.to_string());
                    None
                }
            };
        if let Some((target, mode)) = &target {
            let letter = Letter {
                msg_id: msg_id.clone(),
                from: inner.identity.name.clone(),
                from_mode: *mode,
                hop_chain: extend_chain(chain, &inner.hop_token).unwrap_or_default(),
                priority: Priority::Next,
                body: text.into(),
            };
            // A receipt can arrive before the write returns, so the send is recorded first.
            remember(
                &mut inner.state().sends,
                &msg_id,
                Correlation::to(to),
                now_ms(),
            );
            match inner.transport.send(target, &Frame::Message(letter)) {
                Ok(()) => result.target_status = Some(target.status),
                Err(error) => {
                    inner.state().sends.remove(&msg_id);
                    result.status = SendStatus::SendFailed;
                    result.error = Some(error.to_string());
                }
            }
        }
        let outcome = wire_name(&result.status);
        inner.log.append(LogLine {
            note: result.error.as_deref(),
            body: Some(text),
            ..inner.outbound(Some(&msg_id), to, &outcome)
        });
        if let (true, SendStatus::Sent, Some((target, mode))) =
            (notify_when_idle, result.status, &target)
        {
            result.idle_notice = Some(inner.subscribe_to(target, *mode));
        }
        result
    }

    /// Asks a live session of this task for one idle notice when it next goes idle or exits.
    /// After close it is `failed`.
    pub fn subscribe(&self, to: &str) -> SubscriptionResult {
        let inner = &self.inner;
        match inner
            .effective_mode()
            .and_then(|mode| inner.resolve_target(to).map(|target| (target, mode)))
        {
            Ok((Some(target), mode)) => inner.subscribe_to(&target, mode),
            Ok((None, _)) => {
                inner
                    .log
                    .append(inner.outbound(None, to, "subscribe no-such-session"));
                SubscriptionResult::NoSuchSession
            }
            Err(error) => {
                inner.log_problem("subscribe failed", &error);
                SubscriptionResult::Failed
            }
        }
    }

    /// Live sessions of this task other than this one, by name; only plain addresses are listed.
    pub fn list_sessions(&self) -> Result<Vec<ListedSession>, Error> {
        let mut listed: Vec<ListedSession> = self
            .inner
            .directory
            .live_sessions()?
            .into_iter()
            .filter(|peer| {
                peer.session_ref != self.inner.identity.session_ref && is_address(&peer.name)
            })
            .map(|peer| ListedSession {
                name: peer.name,
                harness: peer.harness,
                status: peer.status,
                waiting_for: peer.waiting_for,
            })
            .collect();
        listed.sort_by(|a, b| a.name.cmp(&b.name));
        Ok(listed)
    }

    /// Reports current native permission evidence without changing provider permissions.
    /// Updates the own directory entry and future send snapshots under the state lock.
    /// A letter and its optional subscription share one snapshot. A send already admitted
    /// may finish with its previous snapshot. None, or a failed metadata write, disables
    /// future outgoing letters/subscriptions; a write failure is also returned to the caller.
    /// Activity, correlation, queued deliveries and existing subscriptions are preserved.
    pub fn set_effective_mode(&self, mode: Option<PermissionMode>) -> Result<(), Error> {
        let inner = &self.inner;
        let mut state = inner.state();
        if inner.is_closed() {
            return Err(Error::Invalid("mailbox closed".into()));
        }
        state.effective_mode = None;
        let mut entry = inner.entry.clone();
        entry.mode = mode;
        entry.status = state.activity;
        entry.waiting_for = state.waiting_for.clone();
        inner.directory.register(&entry)?;
        state.effective_mode = mode;
        Ok(())
    }

    /// Reports what the harness is doing. `waiting_for` is kept only while waiting. Idle arms
    /// the idle check for subscribers.
    pub fn set_activity(&self, activity: Activity, waiting_for: Option<&str>) {
        let waiting_for = waiting_for
            .filter(|_| activity == Activity::Waiting)
            .map(str::to_string);
        {
            let mut state = self.inner.state();
            state.reported = (activity, waiting_for.clone());
            self.inner.apply_activity(&mut state, activity, waiting_for);
        }
        if activity == Activity::Idle {
            self.inner.schedule_idle_check();
        }
    }

    /// Records the harness's latest line for idle notices: one plain line, at most 100 chars.
    pub fn set_detail(&self, text: &str) {
        if let Some(line) = one_line(text) {
            self.inner.state().detail = Some(line);
        }
    }

    /// Marks delivery `id` immediately before its provider write, under the adapter's relay lock.
    /// Its slot and correlation stay queued until confirmed acceptance or rejection. If the
    /// mailbox closes meanwhile, this attempt is logged as uncertain, never as definitely dropped.
    /// Unknown and already completed ids are no-ops; an attempt is never retried by the mailbox.
    pub fn delivery_in_flight(&self, delivery_id: &str) {
        if let Some(queued) = self.inner.state().queued.get_mut(delivery_id) {
            queued.in_flight = true;
        }
    }

    /// The provider accepted delivery `id`, as a new turn or steered into the current one, which
    /// releases its slot; repeating it does nothing. Activity is left to the harness's reports,
    /// so a turn_started that arrives after the harness already went idle cannot pin it busy.
    /// Releasing the last queued delivery of an idle session checks for idleness again, because
    /// the check its idle report armed may have run while the delivery was still queued.
    pub fn turn_started(&self, delivery_id: &str) {
        let recheck = {
            let mut state = self.inner.state();
            state.queued.remove(delivery_id).is_some()
                && state.queued.is_empty()
                && state.activity == Activity::Idle
        };
        if recheck {
            self.inner.schedule_idle_check();
        }
    }

    /// The provider did not accept delivery `id`. Its slot is released, a letter's sender gets a
    /// `dropped` receipt with `reason`, and nothing is retried. Only the first call for an id counts.
    pub fn delivery_failed(&self, delivery_id: &str, reason: &str) {
        let inner = &self.inner;
        let (failed, idle) = {
            let mut state = inner.state();
            let Some(failed) = state.queued.remove(delivery_id) else {
                return;
            };
            let mut idle = false;
            if state.queued.is_empty() {
                let (activity, waiting_for) = state.reported.clone();
                inner.apply_activity(&mut state, activity, waiting_for);
                idle = activity == Activity::Idle;
            }
            (failed, idle)
        };
        inner.log.append(LogLine {
            note: Some(reason),
            ..inner.inbound(Some(&failed.msg_id), &failed.sender, "delivery failed")
        });
        if failed.kind == DeliveryKind::Message {
            let reason = one_line(reason).unwrap_or_default();
            inner.send_receipt(
                &failed.sender,
                Receipt {
                    orig_msg_id: failed.msg_id.clone(),
                    status: ReceiptStatus::Dropped,
                    reason: Some(format!(
                        "the recipient's harness did not accept it: {reason}"
                    )),
                    drop_reason: Some("delivery-failed".into()),
                    dropped_msg_ids: vec![failed.msg_id],
                },
            );
        }
        if idle {
            inner.schedule_idle_check();
        }
    }

    /// Admits a frame the Claude adapter received and authenticated at this session's Claude door.
    pub fn accept_authenticated(&self, frame: Frame) {
        self.inner.accept(frame);
    }

    /// Closes the listener, then unregisters and removes the key, and the socket it bound, unless
    /// another mailbox has since taken the session; then releases the session. Letters still queued
    /// that were never attempted get a `dropped` receipt; in-flight attempts are logged as uncertain.
    /// Every subscriber hears `exited`. Closing twice does nothing.
    pub fn close(&self) {
        let inner = &self.inner;
        if inner.closed.swap(true, Ordering::SeqCst) {
            return;
        }
        let (subscribers, queued) = {
            let mut state = inner.state();
            (
                std::mem::take(&mut state.subscribers),
                std::mem::take(&mut state.queued),
            )
        };
        inner.listener.close();
        if inner.owns_session() {
            if let Err(error) = inner.directory.unregister(&inner.identity.session_ref) {
                inner.log_problem("entry not removed", &error);
            }
            if let Err(error) = remove_if_present(&inner.key_file) {
                inner.log_problem("file not removed", &error);
            }
        } else {
            inner.log.append(LogLine {
                note: Some("another mailbox holds this session; its files and entry are kept"),
                ..inner.inbound(None, &inner.identity.name, "closed after takeover")
            });
        }
        drop(locked(&inner.claim).take());
        for letter in queued
            .into_values()
            .filter(|queued| queued.kind == DeliveryKind::Message)
        {
            if letter.in_flight {
                inner.log.append(LogLine {
                    note: Some(
                        "the provider acknowledgement was not confirmed before the mailbox closed",
                    ),
                    ..inner.inbound(Some(&letter.msg_id), &letter.sender, "delivery uncertain")
                });
            } else {
                inner.send_exited_receipt(&letter.sender, letter.msg_id);
            }
        }
        let now = now_ms();
        for subscriber in subscribers
            .iter()
            .filter(|subscriber| !expired(subscriber.since_ms, now))
        {
            inner.send_notice(subscriber, NoticeState::Exited, Some(now), None);
        }
    }
}

impl Drop for Mailbox {
    fn drop(&mut self) {
        self.close();
    }
}

impl Inner {
    fn effective_mode(&self) -> Result<PermissionMode, Error> {
        if self.is_closed() {
            return Err(Error::Invalid("mailbox closed".into()));
        }
        self.state()
            .effective_mode
            .ok_or_else(|| Error::Invalid("session permission mode is unknown".into()))
    }

    fn state(&self) -> MutexGuard<'_, State> {
        locked(&self.state)
    }

    fn is_closed(&self) -> bool {
        self.closed.load(Ordering::SeqCst)
    }

    /// True while the key beside the socket is still this mailbox's own: no other mailbox has
    /// taken the session since it opened.
    fn owns_session(&self) -> bool {
        fs::read_to_string(&self.key_file).is_ok_and(|key| key == self.key)
    }

    /// A UUID v4 from a keyed hash of a counter: unique, and unguessable without this mailbox's key.
    fn new_id(&self) -> String {
        let count = self.ids_issued.fetch_add(1, Ordering::Relaxed);
        let mut mac =
            Hmac::<Sha256>::new_from_slice(&self.id_key).expect("HMAC accepts keys of any length");
        mac.update(&count.to_be_bytes());
        let mut bytes = [0; 16];
        bytes.copy_from_slice(&mac.finalize().into_bytes()[..16]);
        bytes[6] = (bytes[6] & 0x0f) | 0x40;
        bytes[8] = (bytes[8] & 0x3f) | 0x80;
        let hex = hex(&bytes);
        format!(
            "{}-{}-{}-{}-{}",
            &hex[..8],
            &hex[8..12],
            &hex[12..16],
            &hex[16..20],
            &hex[20..]
        )
    }

    /// The live session of this task with this address. A name that is not a plain address is
    /// nobody's, whatever the address book holds.
    fn resolve(&self, name: &str) -> Result<Option<MailPeer>, Error> {
        if !is_address(name) {
            return Ok(None);
        }
        Ok(self
            .directory
            .live_sessions()?
            .into_iter()
            .find(|peer| peer.name == name))
    }

    /// Why `name` cannot be heard from, if it cannot: the address book is the allowlist, so a
    /// letter or subscription must come from a live session of this task.
    fn sender_refusal(&self, name: &str) -> Option<String> {
        if !is_address(name) {
            return Some("not a plain session address".into());
        }
        match self.resolve(name) {
            Ok(Some(_)) => None,
            Ok(None) => Some("not a live session of this task".into()),
            Err(error) => Some(format!("address book unreadable: {error}")),
        }
    }

    /// Resolves the target of a send or subscription the agent starts. A closed mailbox has left
    /// the address book, so it starts none; its close-time answers go through `route` instead.
    fn resolve_target(&self, to: &str) -> Result<Option<MailPeer>, Error> {
        if self.is_closed() {
            return Err(Error::Invalid("this session's mailbox is closed".into()));
        }
        self.resolve(to)
    }

    /// Hands a frame to the live session named `to`, through this mailbox's transport.
    fn route(&self, to: &str, frame: &Frame) -> Result<(), Error> {
        let peer = self
            .resolve(to)?
            .ok_or_else(|| Error::NotFound(format!("no live session named {to}")))?;
        self.transport.send(&peer, frame)
    }

    /// Queues one delivery until its provider turn starts. An idle session shows busy meanwhile; a
    /// waiting one stays waiting, since only the harness knows when that ends. Checked under the
    /// same lock `close` drains the queue with, so nothing is queued after the drain.
    fn queue_delivery(
        &self,
        kind: DeliveryKind,
        text: String,
        chain: Option<Vec<String>>,
        msg_id: &str,
        sender: &str,
    ) -> Result<Delivery, Refusal> {
        let id = self.new_id();
        let mut state = self.state();
        if self.is_closed() {
            return Err(Refusal::Exited);
        }
        state.queued.insert(
            id.clone(),
            Queued {
                kind,
                msg_id: msg_id.into(),
                sender: sender.into(),
                in_flight: false,
            },
        );
        if state.activity == Activity::Idle {
            self.apply_activity(&mut state, Activity::Busy, None);
        }
        Ok(Delivery {
            id,
            kind,
            text,
            chain,
            msg_id: msg_id.into(),
            sender: sender.into(),
        })
    }

    /// Hands a queued delivery to the adapter, unless `close` has drained it meanwhile.
    fn hand_over(&self, delivery: Delivery) {
        if !self.is_closed() {
            (self.deliver)(delivery);
        }
    }

    fn apply_activity(&self, state: &mut State, activity: Activity, waiting_for: Option<String>) {
        if (state.activity, &state.waiting_for) == (activity, &waiting_for) {
            return;
        }
        if activity == Activity::Idle {
            state.idle_since = Some(now_ms());
        }
        state.activity = activity;
        state.waiting_for = waiting_for;
        let published = self.directory.set_status(
            &self.identity.session_ref,
            activity,
            state.waiting_for.as_deref(),
        );
        if let Err(error) = published {
            self.log_problem("status not published", &error);
        }
    }

    /// Reads one connection: the auth line first, then frames until it ends. A bad first line or an
    /// unreadable frame closes it.
    fn handle_connection(self: Arc<Self>, stream: UnixStream) {
        // macOS refuses a timeout on a socket whose writer already finished; its frames are still
        // buffered and reading cannot block, so the refusal is harmless.
        let _ = stream.set_read_timeout(Some(READ_TIMEOUT));
        let mut reader = BufReader::new(stream);
        let mut authenticated = false;
        loop {
            let line = match wire::read_line(&mut reader) {
                Ok(Some(line)) => line,
                Err(Error::Invalid(reason)) => {
                    self.log.append(LogLine {
                        note: Some(&reason),
                        ..self.inbound(None, "unknown", "rejected-frame")
                    });
                    return;
                }
                Ok(None) | Err(_) => return,
            };
            if line.trim().is_empty() {
                continue;
            }
            if !authenticated {
                authenticated = wire::authenticates(&line, &self.key);
                if !authenticated {
                    self.log.append(LogLine {
                        note: Some("bad or missing auth line"),
                        ..self.inbound(None, "unknown", "rejected-auth")
                    });
                    return;
                }
                continue;
            }
            match serde_json::from_str::<Frame>(&line) {
                Ok(frame) => self.accept(frame),
                Err(_) => {
                    self.log
                        .append(self.inbound(None, "unknown", "rejected-frame"));
                    return;
                }
            }
        }
    }

    /// Routes one frame. After close, letters and subscriptions from live sessions of this task are
    /// still answered, as dropped and exited, so a sender racing the exit is never left with silence.
    fn accept(self: &Arc<Self>, frame: Frame) {
        match frame {
            Frame::Message(letter) => self.admit(letter),
            Frame::Receipt(receipt) => self.on_receipt(receipt),
            Frame::Subscribe(subscription) => self.on_subscribe(subscription),
            Frame::IdleNotice(notice) => self.on_notice(notice),
        }
    }

    /// Admission: the sender, then delivery. A letter from anyone but a live session of this task
    /// is only logged, since its name is no address to answer; one that arrives after close is
    /// answered with a receipt. The hop chain is carried, never judged: a harness with a limit of
    /// its own applies it.
    fn admit(&self, letter: Letter) {
        if let Some(why) = self.sender_refusal(&letter.from) {
            self.log.append(LogLine {
                note: Some(&why),
                ..self.inbound(Some(&letter.msg_id), &letter.from, "rejected-sender")
            });
            return;
        }
        let queued = self.queue_delivery(
            DeliveryKind::Message,
            letter_text(&letter),
            Some(letter.hop_chain.clone()),
            &letter.msg_id,
            &letter.from,
        );
        let delivery = match queued {
            Ok(delivery) => delivery,
            Err(Refusal::Exited) => {
                self.log.append(LogLine {
                    body: Some(&letter.body),
                    ..self.inbound(
                        Some(&letter.msg_id),
                        &letter.from,
                        "dropped recipient-exited",
                    )
                });
                self.send_exited_receipt(&letter.from, letter.msg_id);
                return;
            }
        };
        let mut note = format!("hop chain {}", letter.hop_chain.len());
        match self.state().effective_mode {
            Some(mode) if letter.from_mode != mode => {
                note.push_str(&format!(
                    "; mode mismatch: from-mode {}, recipient {}",
                    wire_name(&letter.from_mode),
                    wire_name(&mode)
                ));
            }
            None => note.push_str("; recipient mode unknown"),
            _ => {}
        }
        self.log.append(LogLine {
            note: Some(&note),
            body: Some(&letter.body),
            ..self.inbound(Some(&letter.msg_id), &letter.from, "admitted")
        });
        self.hand_over(delivery);
    }

    fn send_exited_receipt(&self, to: &str, msg_id: String) {
        self.send_receipt(
            to,
            Receipt {
                orig_msg_id: msg_id.clone(),
                status: ReceiptStatus::Dropped,
                reason: Some(EXITED_REASON.into()),
                drop_reason: Some("recipient-exited".into()),
                dropped_msg_ids: vec![msg_id],
            },
        );
    }

    /// A receipt reaches the agent only when its orig_msg_id answers one of the agent's own sends.
    /// `held` keeps the send open for its final outcome and is told once; anything else closes it.
    fn on_receipt(&self, receipt: Receipt) {
        let answered = {
            let mut state = self.state();
            let now = now_ms();
            state.sends.retain(|_, sent| !expired(sent.since_ms, now));
            match state.sends.get_mut(&receipt.orig_msg_id) {
                None => None,
                Some(sent) if receipt.status == ReceiptStatus::Held => {
                    let news = !sent.held_seen;
                    sent.held_seen = true;
                    Some((sent.to.clone(), news))
                }
                Some(_) => state
                    .sends
                    .remove(&receipt.orig_msg_id)
                    .map(|sent| (sent.to, true)),
            }
        };
        let mut outcome = format!("receipt {}", wire_name(&receipt.status));
        if let Some(drop_reason) = &receipt.drop_reason {
            outcome = format!("{outcome} {drop_reason}");
        }
        let Some((to, news)) = answered else {
            self.log.append(LogLine {
                note: Some("matches no send by this agent"),
                ..self.inbound(Some(&receipt.orig_msg_id), "unknown", &outcome)
            });
            return;
        };
        let note = if news {
            "answers this agent's send"
        } else {
            "repeats a held receipt already told"
        };
        self.log.append(LogLine {
            note: Some(note),
            ..self.inbound(Some(&receipt.orig_msg_id), &to, &outcome)
        });
        if !news {
            return;
        }
        let reason = match receipt.drop_reason.as_deref().and_then(loop_limit_cause) {
            Some(cause) => Some(cause.to_string()),
            None => receipt
                .reason
                .as_deref()
                .or(receipt.drop_reason.as_deref())
                .and_then(one_line),
        };
        let text = receipt_text(&to, &receipt.orig_msg_id, receipt.status, reason.as_deref());
        self.deliver_status(DeliveryKind::Receipt, text, &receipt.orig_msg_id);
    }

    /// Hands the agent a TaskAI notice about its own send or subscription.
    fn deliver_status(&self, kind: DeliveryKind, text: String, msg_id: &str) {
        let refusal = match self.queue_delivery(kind, text, None, msg_id, TASKAI_SENDER) {
            Ok(delivery) => return self.hand_over(delivery),
            Err(Refusal::Exited) => "mailbox closed",
        };
        let kind = match kind {
            DeliveryKind::Receipt => "receipt",
            _ => "notice",
        };
        let outcome = format!("{kind} not delivered: {refusal}");
        self.log
            .append(self.inbound(Some(msg_id), TASKAI_SENDER, &outcome));
    }

    /// Subscribes to `target`'s next idle notice. The subscription is recorded before the write,
    /// since the notice can arrive before the write returns.
    fn subscribe_to(&self, target: &MailPeer, mode: PermissionMode) -> SubscriptionResult {
        if target.session_ref == self.identity.session_ref {
            return SubscriptionResult::RefusedSelf;
        }
        if !target
            .features
            .iter()
            .any(|feature| feature == FEATURE_NOTIFY_IDLE)
        {
            self.log
                .append(self.outbound(None, &target.name, "subscribe unsupported"));
            return SubscriptionResult::Unsupported;
        }
        let msg_id = self.new_id();
        remember(
            &mut self.state().subscriptions,
            &msg_id,
            Correlation::to(&target.name),
            now_ms(),
        );
        let frame = Frame::Subscribe(Subscription {
            msg_id: msg_id.clone(),
            from: self.identity.name.clone(),
            from_mode: mode,
        });
        let (result, outcome) = match self.transport.send(target, &frame) {
            Ok(()) => (SubscriptionResult::Subscribed, "subscribed"),
            Err(_) => {
                self.state().subscriptions.remove(&msg_id);
                (SubscriptionResult::Failed, "subscribe failed")
            }
        };
        self.log
            .append(self.outbound(Some(&msg_id), &target.name, outcome));
        result
    }

    /// Records a live peer's subscription, or answers at once: `unavailable` when 32 are already
    /// held, `exited` once the mailbox is closing. Both are decided under the lock `close` drains
    /// subscribers with, so no subscriber is recorded after the drain.
    fn on_subscribe(self: &Arc<Self>, subscription: Subscription) {
        let Subscription { msg_id, from, .. } = subscription;
        let refusal = if msg_id.is_empty() {
            Some("the subscription has no msg_id".into())
        } else {
            self.sender_refusal(&from)
        };
        if let Some(why) = refusal {
            self.log.append(LogLine {
                note: Some(&why),
                ..self.inbound(Some(&msg_id), &from, "subscription rejected")
            });
            return;
        }
        let subscriber = Subscriber {
            name: from,
            msg_id,
            since_ms: now_ms(),
        };
        let answer = {
            let mut state = self.state();
            let now = subscriber.since_ms;
            state
                .subscribers
                .retain(|held| !expired(held.since_ms, now));
            if self.is_closed() {
                Some((NoticeState::Exited, Some(now), "subscription after exit"))
            } else if state.subscribers.len() >= MAX_SUBSCRIBERS {
                Some((NoticeState::Unavailable, None, "subscription full"))
            } else {
                state.subscribers.push(subscriber.clone());
                None
            }
        };
        let outcome = answer.map_or("subscription recorded", |(_, _, outcome)| outcome);
        self.log
            .append(self.inbound(Some(&subscriber.msg_id), &subscriber.name, outcome));
        match answer {
            Some((state, finished_at, _)) => {
                self.send_notice(&subscriber, state, finished_at, None);
            }
            None => self.schedule_idle_check(),
        }
    }

    /// An idle notice reaches the agent only when it answers one of its own subscriptions.
    fn on_notice(&self, notice: IdleNotice) {
        let label = {
            let mut state = self.state();
            let now = now_ms();
            state
                .subscriptions
                .retain(|_, subscribed| !expired(subscribed.since_ms, now));
            state
                .subscriptions
                .remove(&notice.orig_msg_id)
                .map(|subscribed| subscribed.to)
        };
        let outcome = format!("idle notice {}", wire_name(&notice.state));
        let Some(label) = label else {
            self.log.append(LogLine {
                note: Some("matches no subscription by this agent"),
                ..self.inbound(Some(&notice.orig_msg_id), "unknown", &outcome)
            });
            return;
        };
        self.log.append(LogLine {
            note: Some("answers this agent's subscription"),
            ..self.inbound(Some(&notice.orig_msg_id), &label, &outcome)
        });
        let detail = notice.detail.as_deref().and_then(one_line);
        let text = notice_text(&label, notice.state, notice.finished_at, detail.as_deref());
        self.deliver_status(DeliveryKind::Notice, text, &notice.orig_msg_id);
    }

    /// Checks for idleness once the debounce has passed; a later schedule supersedes this one.
    fn schedule_idle_check(self: &Arc<Self>) {
        let generation = {
            let mut state = self.state();
            state.idle_generation += 1;
            state.idle_generation
        };
        let inner = Arc::clone(self);
        thread::spawn(move || {
            thread::sleep(IDLE_DEBOUNCE);
            inner.notify_if_idle(generation);
        });
    }

    /// Tells every subscriber `idle`, once, when the session is idle with nothing queued.
    fn notify_if_idle(&self, generation: u64) {
        let (subscribers, finished_at, detail) = {
            let mut state = self.state();
            let settled = !self.is_closed()
                && state.idle_generation == generation
                && state.activity == Activity::Idle
                && state.queued.is_empty();
            if !settled {
                return;
            }
            let now = now_ms();
            state
                .subscribers
                .retain(|held| !expired(held.since_ms, now));
            (
                std::mem::take(&mut state.subscribers),
                state.idle_since,
                state.detail.clone(),
            )
        };
        for subscriber in &subscribers {
            self.send_notice(subscriber, NoticeState::Idle, finished_at, detail.clone());
        }
    }

    fn send_notice(
        &self,
        subscriber: &Subscriber,
        state: NoticeState,
        finished_at: Option<u64>,
        detail: Option<String>,
    ) {
        let frame = Frame::IdleNotice(IdleNotice {
            orig_msg_id: subscriber.msg_id.clone(),
            state,
            finished_at,
            detail,
        });
        let routed = self.route(&subscriber.name, &frame);
        let state = wire_name(&state);
        let outcome = match &routed {
            Ok(()) => format!("idle notice {state} sent"),
            Err(_) => format!("idle notice {state} undeliverable"),
        };
        let note = routed.err().map(|error| error.to_string());
        self.log.append(LogLine {
            note: note.as_deref(),
            ..self.outbound(Some(&subscriber.msg_id), &subscriber.name, &outcome)
        });
    }

    fn send_receipt(&self, to: &str, receipt: Receipt) {
        let orig_msg_id = receipt.orig_msg_id.clone();
        let status = wire_name(&receipt.status);
        let routed = self.route(to, &Frame::Receipt(receipt));
        let outcome = match &routed {
            Ok(()) => format!("receipt {status} sent"),
            Err(_) => format!("receipt {status} undeliverable"),
        };
        let note = routed.err().map(|error| error.to_string());
        self.log.append(LogLine {
            note: note.as_deref(),
            ..self.outbound(Some(&orig_msg_id), to, &outcome)
        });
    }

    fn inbound<'a>(
        &'a self,
        msg_id: Option<&'a str>,
        from: &'a str,
        outcome: &'a str,
    ) -> LogLine<'a> {
        LogLine {
            msg_id,
            from,
            to: &self.identity.name,
            direction: "in",
            outcome,
            ..LogLine::default()
        }
    }

    fn outbound<'a>(
        &'a self,
        msg_id: Option<&'a str>,
        to: &'a str,
        outcome: &'a str,
    ) -> LogLine<'a> {
        LogLine {
            msg_id,
            from: &self.identity.name,
            to,
            direction: "out",
            outcome,
            ..LogLine::default()
        }
    }

    /// Records a local failure that leaves messaging working but should be visible.
    fn log_problem(&self, outcome: &str, error: &Error) {
        let note = error.to_string();
        self.log.append(LogLine {
            msg_id: None,
            from: &self.identity.name,
            to: &self.identity.name,
            direction: "local",
            outcome,
            note: Some(&note),
            ..LogLine::default()
        });
    }
}

/// The task's message log: one JSON line per event, owner-only, never a key.
struct MessageLog {
    path: PathBuf,
}

#[derive(Default, Serialize)]
struct LogLine<'a> {
    ts: String,
    msg_id: Option<&'a str>,
    from: &'a str,
    to: &'a str,
    direction: &'a str,
    outcome: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    note: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    body: Option<&'a str>,
}

impl MessageLog {
    /// Appends one line in a single write. The log is development telemetry: a failed write is
    /// dropped rather than allowed to stop a letter.
    fn append(&self, mut line: LogLine<'_>) {
        line.ts = chrono::Utc::now().format("%Y-%m-%dT%H:%M:%SZ").to_string();
        let Ok(mut text) = serde_json::to_string(&line) else {
            return;
        };
        text.push('\n');
        let file = OpenOptions::new()
            .create(true)
            .append(true)
            .mode(0o600)
            .open(&self.path);
        if let Ok(mut file) = file {
            let _ = file.write_all(text.as_bytes());
        }
    }
}

/// What the agent reads for a letter: who sent it in which mode, the body, and how to answer. The
/// sender was admitted, so its name is a plain address and is shown verbatim.
fn letter_text(letter: &Letter) -> String {
    let from = &letter.from;
    format!(
        "<taskai-message from=\"{from}\" from-mode=\"{mode}\">\n{body}\n</taskai-message>\n\
         To reply, call your TaskAI send_message tool with to=\"{from}\". \
         Reply only if the message asks for a reply, then stop.",
        mode = wire_name(&letter.from_mode),
        body = defuse_envelope_tags(&letter.body),
    )
}

/// Shows every taskai-message tag in a body, in any case or spacing, with `‹` for its `<`, so the
/// body can neither close its envelope nor open a forged one.
fn defuse_envelope_tags(body: &str) -> String {
    let mut defused = String::with_capacity(body.len());
    let mut rest = body;
    while let Some(at) = rest.find('<') {
        defused.push_str(&rest[..at]);
        rest = &rest[at + 1..];
        let opened = rest.trim_start();
        let tag = opened.strip_prefix('/').unwrap_or(opened).trim_start();
        let is_envelope = tag
            .get(.."taskai-message".len())
            .is_some_and(|head| head.eq_ignore_ascii_case("taskai-message"));
        defused.push(if is_envelope { '\u{2039}' } else { '<' });
    }
    defused.push_str(rest);
    defused
}

/// What the agent reads when a receipt answers one of its sends; `to` is the address it sent to.
/// What a sender's agent is told when the recipient's own loop limit dropped its letter. The
/// receipt's code alone would not say that the exchange can go on.
fn loop_limit_cause(drop_reason: &str) -> Option<&'static str> {
    matches!(drop_reason, "hop-loop" | "hop-runaway").then_some(
        "the recipient's loop limit stopped it; a message sent in a later turn starts a new chain",
    )
}

fn receipt_text(to: &str, msg_id: &str, status: ReceiptStatus, reason: Option<&str>) -> String {
    let why = reason
        .map(|reason| format!(": {reason}"))
        .unwrap_or_default();
    format!(
        "[TaskAI delivery notice] Your message to \"{to}\" (msg_id {msg_id}) was {status}{why}. \
         {NOTICE_TAIL}",
        status = wire_name(&status)
    )
}

/// What the agent reads when an idle notice answers one of its subscriptions; `label` is the address
/// it subscribed to.
fn notice_text(
    label: &str,
    state: NoticeState,
    finished_at: Option<u64>,
    detail: Option<&str>,
) -> String {
    let clock = |ms: u64| {
        let at = chrono::Local
            .timestamp_millis_opt(i64::try_from(ms).ok()?)
            .single()?;
        Some(at.format("%H:%M").to_string())
    };
    match state {
        NoticeState::Idle => {
            let at = finished_at
                .and_then(clock)
                .map(|at| format!(": it finished a turn at {at}"))
                .unwrap_or_default();
            let last = detail
                .map(|line| format!(" Its last line: «{line}»."))
                .unwrap_or_default();
            format!(
                "[TaskAI idle notice] \"{label}\", which you asked to hear from, is idle{at}.{last} \
                 {NOTICE_TAIL}"
            )
        }
        NoticeState::Exited => {
            let at = finished_at
                .and_then(clock)
                .map(|at| format!(" at {at}"))
                .unwrap_or_default();
            format!(
                "[TaskAI idle notice] \"{label}\", which you asked to hear from, exited{at} before \
                 going idle; it will not process further messages at that address. {NOTICE_TAIL}"
            )
        }
        NoticeState::Unavailable => format!(
            "[TaskAI idle notice] \"{label}\" is not holding your idle subscription, so no idle \
             notice will come from it. {NOTICE_TAIL}"
        ),
    }
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| {
            u64::try_from(since.as_millis()).unwrap_or(u64::MAX)
        })
}

/// True once a correlation made at `since_ms` is 12 hours old.
fn expired(since_ms: u64, now_ms: u64) -> bool {
    now_ms.saturating_sub(since_ms) >= CORRELATION_TTL_MS
}

/// Records a send or subscription, forgetting the expired ones so the map stays bounded even when
/// no receipt or notice ever arrives to prune it.
fn remember(
    correlations: &mut HashMap<String, Correlation>,
    msg_id: &str,
    correlation: Correlation,
    now_ms: u64,
) {
    correlations.retain(|_, held| !expired(held.since_ms, now_ms));
    correlations.insert(msg_id.into(), correlation);
}

/// The string a wire value serializes to, such as `prompting` or `no-such-session`.
fn wire_name(value: &impl Serialize) -> String {
    serde_json::to_value(value)
        .ok()
        .and_then(|value| value.as_str().map(str::to_string))
        .unwrap_or_default()
}

/// `<mail_dir>/<session_ref>.sock`, the session's mailbox socket.
pub fn socket_path(mail_dir: &Path, session_ref: &str) -> PathBuf {
    mail_dir.join(format!("{session_ref}.sock"))
}

/// `<mail_dir>/<session_ref>.key`, the key a writer presents on its first line.
pub fn key_path(mail_dir: &Path, session_ref: &str) -> PathBuf {
    mail_dir.join(format!("{session_ref}.key"))
}

fn create_private_dir(dir: &Path) -> Result<(), Error> {
    fs::create_dir_all(dir)?;
    fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
    Ok(())
}

/// `<mail_dir>/<session_ref>.lock`, which the session's live mailbox holds locked.
fn lock_path(mail_dir: &Path, session_ref: &str) -> PathBuf {
    mail_dir.join(format!("{session_ref}.lock"))
}

/// A mailbox's hold on its session for as long as it lives: an exclusive `flock` on the session's
/// lock file, which the kernel also releases if the process dies.
struct SessionLock {
    file: fs::File,
    path: PathBuf,
}

impl SessionLock {
    /// Takes the session, or `None` while another mailbox holds it.
    fn take(path: PathBuf) -> Result<Option<Self>, Error> {
        // A mailbox removes its lock file before releasing it, so a lock won on a file that has
        // since left the path guards nothing; the next try opens the file now there.
        for _ in 0..LOCK_ATTEMPTS {
            let file = OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(false)
                .mode(0o600)
                .open(&path)?;
            match flock(&file, FlockOperation::NonBlockingLockExclusive) {
                Ok(()) => {}
                Err(Errno::WOULDBLOCK) => return Ok(None),
                Err(error) => return Err(std::io::Error::from(error).into()),
            }
            if names_file(&path, &file) {
                return Ok(Some(Self { file, path }));
            }
        }
        Err(Error::Invalid(format!(
            "{} kept changing while it was being locked",
            path.display()
        )))
    }
}

impl Drop for SessionLock {
    /// Removes the lock file while still holding it, so the next mailbox locks a fresh file and
    /// the mail directory keeps nothing of this one.
    fn drop(&mut self) {
        if names_file(&self.path, &self.file) {
            let _ = fs::remove_file(&self.path);
        }
    }
}

/// True while `path` leads to the very file `file` has open.
fn names_file(path: &Path, file: &fs::File) -> bool {
    match (fs::metadata(path), file.metadata()) {
        (Ok(named), Ok(held)) => file_id(&named) == file_id(&held),
        _ => false,
    }
}

fn locked<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

fn random_bytes<const N: usize>() -> Result<[u8; N], Error> {
    let mut bytes = [0; N];
    fs::File::open("/dev/urandom")?.read_exact(&mut bytes)?;
    Ok(bytes)
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

/// True for a `CLAUDE_CODE_MESSAGING_*` variable name, judged by its bytes, so a name that is not
/// UTF-8 is judged too.
pub fn is_messaging_env_name(name: &OsStr) -> bool {
    name.as_encoded_bytes()
        .starts_with(MESSAGING_ENV_PREFIX.as_bytes())
}

/// The names of this process's `CLAUDE_CODE_MESSAGING_*` variables; their values are dropped
/// unread.
pub fn inherited_messaging_env_names() -> Vec<OsString> {
    std::env::vars_os()
        .map(|(name, _)| name)
        .filter(|name| is_messaging_env_name(name))
        .collect()
}

/// Removes every `CLAUDE_CODE_MESSAGING_*` variable from `command`, inherited or set on it.
pub fn strip_messaging_env(command: &mut Command) {
    let explicit: Vec<OsString> = command
        .get_envs()
        .map(|(name, _)| name.to_os_string())
        .filter(|name| is_messaging_env_name(name))
        .collect();
    for name in inherited_messaging_env_names().into_iter().chain(explicit) {
        command.env_remove(name);
    }
}

/// A process's start time as `ps -o lstart` prints it in UTC, the form Claude records too.
pub fn proc_start(pid: u32) -> Option<String> {
    let mut ps = Command::new("ps");
    ps.args(["-o", "lstart=", "-p", &pid.to_string()])
        .env("TZ", "UTC")
        .stdin(Stdio::null())
        .stderr(Stdio::null());
    strip_messaging_env(&mut ps);
    let output = ps.output().ok()?;
    let started = String::from_utf8(output.stdout).ok()?.trim().to_string();
    (output.status.success() && !started.is_empty()).then_some(started)
}

/// True while `pid` runs and started at `started`; a recycled pid has another start time.
pub fn is_live(pid: u32, started: &str) -> bool {
    proc_start(pid).as_deref() == Some(started)
}

/// This session's hop token: only the holder of `key` can recognize it in a chain.
pub fn hop_token(key: &[u8], address: &str) -> String {
    let mut mac = Hmac::<Sha256>::new_from_slice(key).expect("HMAC accepts keys of any length");
    mac.update(address.as_bytes());
    hex(&mac.finalize().into_bytes())[..HOP_TOKEN_HEX].to_string()
}

/// A send continues a chain only in a turn a peer letter started, keeping the last 32 tokens.
pub fn extend_chain(chain: Option<&[String]>, token: &str) -> Option<Vec<String>> {
    let mut extended = chain?.to_vec();
    extended.push(token.to_string());
    let excess = extended.len().saturating_sub(MAX_CHAIN_KEPT);
    extended.drain(..excess);
    Some(extended)
}

/// True for a name every harness and envelope can carry verbatim, which a session's address must
/// be: 1 to 64 ASCII letters, digits, `.`, `_` or `-`, starting with a letter or digit.
pub fn is_address(name: &str) -> bool {
    name.len() <= MAX_ADDRESS_CHARS
        && name.starts_with(|c: char| c.is_ascii_alphanumeric())
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
}

/// First non-blank line without markup or control characters, capped for a notice's detail.
pub fn one_line(text: &str) -> Option<String> {
    text.split(['\n', '\r'])
        .map(|line| {
            let plain: String = line
                .chars()
                .map(|c| {
                    if c.is_control() || "<>\"«»[]".contains(c) {
                        ' '
                    } else {
                        c
                    }
                })
                .collect();
            plain.split_whitespace().collect::<Vec<_>>().join(" ")
        })
        .find(|line| !line.is_empty())
        .map(|line| line.chars().take(DETAIL_CHARS).collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    // Expectation: a correlation or subscription expires 12 hours after it was made, not before.
    // Why: the spec lets a subscription expire unheard after 12 hours, and a send without a receipt
    // (the normal case) must not be remembered forever. Value: a long-lived bridge's memory is bounded.
    #[test]
    fn correlations_expire_after_twelve_hours() {
        let made = 1_000;
        assert!(!expired(made, made));
        assert!(!expired(made, made + CORRELATION_TTL_MS - 1));
        assert!(expired(made, made + CORRELATION_TTL_MS));
        assert_eq!(CORRELATION_TTL_MS, 12 * 60 * 60 * 1000);
    }

    // Expectation: remembering a correlation drops the ones that have expired. Why: most sends never
    // get a receipt, so pruning only when one arrives would keep them for the bridge's lifetime.
    // Value: a long-lived bridge's memory stays bounded by twelve hours of traffic.
    #[test]
    fn remembering_a_correlation_forgets_the_expired_ones() {
        let now = 50 * CORRELATION_TTL_MS;
        let mut sends = HashMap::new();
        remember(
            &mut sends,
            "old",
            Correlation {
                to: "a".into(),
                since_ms: now + 1 - CORRELATION_TTL_MS,
                held_seen: false,
            },
            now,
        );
        remember(
            &mut sends,
            "recent",
            Correlation {
                to: "b".into(),
                since_ms: now,
                held_seen: false,
            },
            now,
        );
        assert_eq!(sends.len(), 2, "both were fresh when remembered");
        remember(
            &mut sends,
            "new",
            Correlation {
                to: "c".into(),
                since_ms: now,
                held_seen: false,
            },
            now + 1,
        );
        let mut kept: Vec<&str> = sends.keys().map(String::as_str).collect();
        kept.sort_unstable();
        assert_eq!(kept, ["new", "recent"]);
    }
}
