// ABOUTME: Claude Code's peer protocol v1: owned-pane card lookup, the frames on a session's socket,
// ABOUTME: their translation to and from TaskAI's mailbox frames, and the transport to Claude.

use std::{
    fs,
    io::Write,
    net::Shutdown,
    os::unix::net::UnixStream,
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicU64, Ordering},
        Arc,
    },
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use serde::Deserialize;
use serde_json::{json, Value};
use taskai_core::{
    mail::{
        is_live, Activity, Frame, IdleNotice, Letter, MailPeer, NoticeState, PermissionMode,
        Priority, Receipt, ReceiptStatus, Subscription, Transport,
    },
    registry::Session,
    sha256_hex, Error,
};

use super::{drift::Drift, envelope::Envelope};

/// The peer protocol version this adapter speaks.
pub const PEER_PROTOCOL: u32 = 1;
/// The harness name of Claude Code sessions in TaskAI's address book.
pub const CLAUDE_HARNESS: &str = "claude";
/// Longest line a Claude session reads.
const MAX_LINE_BYTES: usize = 1 << 20;
const WRITE_TIMEOUT: Duration = Duration::from_secs(5);

/// A Claude session's card, `<sessions>/<pid>.json`, as far as TaskAI reads it.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Card {
    pub pid: u32,
    pub session_id: String,
    pub started_at: u64,
    pub kind: String,
    #[serde(default)]
    pub proc_start: Option<String>,
    #[serde(default)]
    pub peer_protocol: Option<u32>,
    #[serde(default)]
    pub peer_features: Vec<String>,
    #[serde(default)]
    pub messaging_socket_path: Option<PathBuf>,
    #[serde(default)]
    pub name: Option<String>,
    #[serde(default)]
    pub status: Option<String>,
    #[serde(default)]
    pub waiting_for: Option<String>,
}

impl Card {
    /// The socket this session reads letters on, when it speaks protocol 1 at an absolute path.
    pub fn socket(&self) -> Option<&Path> {
        self.messaging_socket_path
            .as_deref()
            .filter(|path| self.peer_protocol == Some(PEER_PROTOCOL) && path.is_absolute())
    }
}

/// The card of the Claude session in process `pid` started at `proc_start`, read from
/// `<sessions_dir>/<pid>.json` alone: only when the card names that exact process and it still
/// runs. No card, another process's card or an exited process gives `None`; a card that is not
/// valid JSON of Claude's schema is an error.
pub fn card_for(sessions_dir: &Path, pid: u32, proc_start: &str) -> Result<Option<Card>, Error> {
    let text = match fs::read_to_string(sessions_dir.join(format!("{pid}.json"))) {
        Ok(text) => text,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error.into()),
    };
    let card: Card = serde_json::from_str(&text)
        .map_err(|error| Error::Invalid(format!("Claude card {pid}.json: {error}")))?;
    let owned = card.pid == pid
        && card.proc_start.as_deref() == Some(proc_start)
        && is_live(pid, proc_start);
    Ok(owned.then_some(card))
}

/// The address-book entry for a native Claude session TaskAI launched, from its validated card:
/// the card's own pid and start time, its protocol-1 socket as the mailbox, its features and its
/// status (an unknown status shows as busy), under the TaskAI session's address. Mode metadata
/// is optional: Claude's native card does not report it. Every native letter still declares its
/// sender's mode in the canonical envelope. `None` when there is no protocol-1 socket/start time.
/// Re-register it whenever the card changes; the address book lets the same pid refresh its entry.
pub fn native_entry(
    session: &Session,
    card: &Card,
    mode: impl Into<Option<PermissionMode>>,
) -> Option<MailPeer> {
    let socket = card.socket()?;
    let proc_start = card.proc_start.clone()?;
    let status = match card.status.as_deref() {
        Some("idle") => Activity::Idle,
        Some("waiting") => Activity::Waiting,
        _ => Activity::Busy,
    };
    Some(MailPeer {
        session_ref: session.session_ref.clone(),
        task_ref: session.task_ref.clone(),
        name: session.address.clone(),
        harness: CLAUDE_HARNESS.into(),
        workspace: session.workspace.clone(),
        pid: card.pid,
        proc_start,
        status,
        waiting_for: card
            .waiting_for
            .clone()
            .filter(|_| status == Activity::Waiting),
        mailbox: socket.into(),
        features: card.peer_features.clone(),
        mode: mode.into(),
    })
}

/// `<sessions_dir>/<pid>.<sha256 of the socket path>.key`, the key beside a session's card.
pub(crate) fn key_path(sessions_dir: &Path, pid: u32, socket: &Path) -> PathBuf {
    let digest = sha256_hex(socket.as_os_str().as_encoded_bytes());
    sessions_dir.join(format!("{pid}.{digest}.key"))
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Key {
    peer_token: String,
    #[serde(default)]
    proc_start: Option<String>,
}

/// The token a session admits writers with, from a key of the same process start as its card.
/// The token never appears in an error.
fn peer_token(sessions_dir: &Path, card: &Card, socket: &Path) -> Result<String, Error> {
    let text = fs::read_to_string(key_path(sessions_dir, card.pid, socket))?;
    let key: Key = serde_json::from_str(&text)
        .map_err(|_| Error::Invalid(format!("the Claude key of {} is malformed", card.pid)))?;
    if key.proc_start != card.proc_start {
        return Err(Error::Invalid(format!(
            "the Claude key of {} belongs to another process start",
            card.pid
        )));
    }
    Ok(key.peer_token)
}

#[derive(Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum Wire {
    User(UserFrame),
    Control(ControlFrame),
}

#[derive(Deserialize)]
struct UserFrame {
    msg_id: String,
    #[serde(default)]
    priority: Priority,
    from: String,
    message: UserMessage,
}

#[derive(Deserialize)]
struct UserMessage {
    content: String,
}

#[derive(Deserialize)]
struct ControlFrame {
    msg_id: String,
    #[serde(flatten)]
    action: Action,
}

#[derive(Deserialize)]
#[serde(tag = "action", rename_all = "snake_case")]
enum Action {
    PeerMessageStatus {
        status: String,
        #[serde(default)]
        status_detail: Option<String>,
        #[serde(default)]
        reason: Option<String>,
        #[serde(default)]
        orig_msg_id: Option<String>,
        #[serde(default)]
        drop_reason: Option<String>,
        #[serde(default)]
        dropped_msg_ids: Vec<String>,
    },
    NotifyWhenIdle {
        #[serde(default)]
        from_mode: Option<PermissionMode>,
    },
    PeerIdleNotice {
        orig_msg_id: String,
        state: NoticeState,
        #[serde(default)]
        finished_at: Option<u64>,
        #[serde(default)]
        detail: Option<String>,
    },
}

/// What a Claude frame read at a bridge's door means for its mailbox, attributed to `sender`, the
/// TaskAI address the door resolved. A letter needs one canonical envelope that states a mode and
/// names the frame's own reply address. A status receipt yields one receipt per message it
/// answers, and Claude's expired-refused is read back as refused. Anything else is refused with
/// the reason.
pub fn inbound(line: &str, sender: &str) -> Result<Vec<Frame>, String> {
    let named = |field: &str| -> String {
        serde_json::from_str::<Value>(line)
            .ok()
            .and_then(|frame| frame.get(field).and_then(Value::as_str).map(str::to_string))
            .unwrap_or_else(|| "none".into())
    };
    let wire: Wire = serde_json::from_str(line).map_err(|_| {
        format!(
            "not a Claude frame this door takes (type {}, action {})",
            named("type"),
            named("action")
        )
    })?;
    match wire {
        Wire::User(user) => {
            let envelope = Envelope::parse(&user.message.content)
                .ok_or("the letter is not one canonical envelope")?;
            if envelope.from.as_deref() != Some(user.from.as_str()) {
                return Err("the envelope names another reply address than its frame".into());
            }
            let from_mode = envelope
                .from_mode
                .ok_or("the envelope states no permission mode")?;
            Ok(vec![Frame::Message(Letter {
                msg_id: user.msg_id,
                from: sender.into(),
                from_mode,
                hop_chain: envelope.hop_chain.unwrap_or_default(),
                priority: user.priority,
                body: envelope.body,
            })])
        }
        Wire::Control(control) => match control.action {
            Action::PeerMessageStatus {
                status,
                status_detail,
                reason,
                orig_msg_id,
                drop_reason,
                dropped_msg_ids,
            } => {
                let status = match (status.as_str(), status_detail.as_deref()) {
                    ("expired", Some("refused")) => ReceiptStatus::Refused,
                    ("held", _) => ReceiptStatus::Held,
                    ("delivered", _) => ReceiptStatus::Delivered,
                    ("denied", _) => ReceiptStatus::Denied,
                    ("expired", _) => ReceiptStatus::Expired,
                    ("dropped", _) => ReceiptStatus::Dropped,
                    (other, _) => return Err(format!("unknown receipt status {other:?}")),
                };
                let orig_msg_id = orig_msg_id.ok_or("the receipt names no message")?;
                let mut answered = vec![orig_msg_id];
                if status == ReceiptStatus::Dropped {
                    for id in dropped_msg_ids {
                        if !answered.contains(&id) {
                            answered.push(id);
                        }
                    }
                }
                Ok(answered
                    .into_iter()
                    .map(|id| {
                        Frame::Receipt(Receipt {
                            dropped_msg_ids: if status == ReceiptStatus::Dropped {
                                vec![id.clone()]
                            } else {
                                Vec::new()
                            },
                            orig_msg_id: id,
                            status,
                            reason: reason.clone(),
                            drop_reason: drop_reason.clone(),
                        })
                    })
                    .collect())
            }
            Action::NotifyWhenIdle { from_mode } => Ok(vec![Frame::Subscribe(Subscription {
                msg_id: control.msg_id,
                from: sender.into(),
                from_mode: from_mode.unwrap_or(PermissionMode::Prompting),
            })]),
            Action::PeerIdleNotice {
                orig_msg_id,
                state,
                finished_at,
                detail,
            } => Ok(vec![Frame::IdleNotice(IdleNotice {
                orig_msg_id,
                state,
                finished_at,
                detail,
            })]),
        },
    }
}

/// The Claude frame for a mailbox frame, answering to `reply_to`, the bridge's door address: a
/// letter as a user frame carrying its canonical envelope, the rest as control frames.
pub fn outbound(frame: &Frame, reply_to: &str) -> Value {
    match frame {
        Frame::Message(letter) => {
            let envelope = Envelope {
                from: Some(reply_to.into()),
                hop_chain: Some(letter.hop_chain.clone()),
                from_name: Some(letter.from.clone()),
                from_mode: Some(letter.from_mode),
                body: letter.body.clone(),
                ..Envelope::default()
            };
            json!({"msgV": 1, "type": "user", "msg_id": letter.msg_id, "priority": letter.priority,
                   "from": reply_to, "message": {"role": "user", "content": envelope.render()}})
        }
        Frame::Receipt(receipt) => {
            let mut frame = control("peer_message_status", new_msg_id(), reply_to);
            if receipt.status == ReceiptStatus::Refused {
                frame["status"] = json!("expired");
                frame["status_detail"] = json!("refused");
            } else {
                frame["status"] = json!(receipt.status);
            }
            frame["orig_msg_id"] = json!(receipt.orig_msg_id);
            if let Some(reason) = &receipt.reason {
                frame["reason"] = json!(reason);
            }
            if let Some(drop_reason) = &receipt.drop_reason {
                frame["drop_reason"] = json!(drop_reason);
                frame["dropped_msg_ids"] = json!(receipt.dropped_msg_ids);
            }
            frame
        }
        Frame::Subscribe(subscription) => {
            let mut frame = control("notify_when_idle", subscription.msg_id.clone(), reply_to);
            frame["from_mode"] = json!(subscription.from_mode);
            frame
        }
        Frame::IdleNotice(notice) => {
            let mut frame = control("peer_idle_notice", new_msg_id(), reply_to);
            frame["orig_msg_id"] = json!(notice.orig_msg_id);
            frame["state"] = json!(notice.state);
            if let Some(finished_at) = notice.finished_at {
                frame["finished_at"] = json!(finished_at);
            }
            if let Some(detail) = &notice.detail {
                frame["detail"] = json!(detail);
            }
            frame
        }
    }
}

fn control(action: &str, msg_id: String, reply_to: &str) -> Value {
    json!({"type": "control", "action": action, "msgV": 1, "msg_id": msg_id, "from": reply_to})
}

/// A UUID v4 unique to this process and moment, for frames TaskAI originates. Claude matches
/// answers by the ids TaskAI's mailbox issued, so these need only be unique.
fn new_msg_id() -> String {
    static ISSUED: AtomicU64 = AtomicU64::new(0);
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| since.as_nanos());
    let seed = format!(
        "{}-{nanos}-{}",
        std::process::id(),
        ISSUED.fetch_add(1, Ordering::Relaxed)
    );
    let digest = sha256_hex(seed.as_bytes());
    let variant = ['8', '9', 'a', 'b'][usize::from(digest.as_bytes()[16] % 4)];
    format!(
        "{}-{}-4{}-{variant}{}-{}",
        &digest[..8],
        &digest[8..12],
        &digest[13..16],
        &digest[17..20],
        &digest[20..32]
    )
}

/// Writes to Claude sessions through Claude's peer protocol, while the drift check passes, and
/// hands every other peer to `others`, normally TaskAI's own socket transport.
pub struct ClaudeTransport {
    sessions_dir: PathBuf,
    reply_to: String,
    others: Arc<dyn Transport>,
    drift: Drift,
}

impl ClaudeTransport {
    /// `reply_to` is the bridge's door address, where Claude sends its answers. With a failed
    /// `drift`, every Claude peer is refused with its reasons; other peers are unaffected.
    pub fn new(
        sessions_dir: PathBuf,
        reply_to: String,
        others: Arc<dyn Transport>,
        drift: Drift,
    ) -> Self {
        Self {
            sessions_dir,
            reply_to,
            others,
            drift,
        }
    }
}

impl Transport for ClaudeTransport {
    /// For a Claude peer: the card of exactly its process, whose socket must be the peer's
    /// mailbox, authenticated with that card's key; then one Claude frame.
    fn send(&self, to: &MailPeer, frame: &Frame) -> Result<(), Error> {
        if to.harness != CLAUDE_HARNESS {
            return self.others.send(to, frame);
        }
        self.drift.require()?;
        let line = outbound(frame, &self.reply_to).to_string();
        if line.len() + 1 > MAX_LINE_BYTES {
            return Err(Error::Invalid("letter frame longer than 1 MiB".into()));
        }
        let card = card_for(&self.sessions_dir, to.pid, &to.proc_start)?
            .ok_or_else(|| Error::NotFound(format!("no live Claude card for {}", to.name)))?;
        let socket = card.socket().ok_or_else(|| {
            Error::Invalid(format!(
                "{}'s Claude card offers no protocol-1 socket",
                to.name
            ))
        })?;
        if socket != to.mailbox {
            return Err(Error::Invalid(format!(
                "{}'s Claude card names another socket than the address book",
                to.name
            )));
        }
        let auth = json!({"type": "auth", "token": peer_token(&self.sessions_dir, &card, socket)?});
        let mut stream = UnixStream::connect(socket)?;
        stream.set_write_timeout(Some(WRITE_TIMEOUT))?;
        stream.write_all(format!("{auth}\n{line}\n").as_bytes())?;
        stream.shutdown(Shutdown::Write)?;
        Ok(())
    }
}
