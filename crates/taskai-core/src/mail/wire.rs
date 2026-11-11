// ABOUTME: TaskAI mailbox protocol v1 on the wire — the frames one mailbox writes to another
// ABOUTME: as newline-delimited JSON after the authenticating first line.

use std::{
    io::{BufRead, Read, Write},
    net::Shutdown,
    os::unix::net::UnixStream,
    time::Duration,
};

use serde::{Deserialize, Serialize};

use super::{MailPeer, Transport};
use crate::Error;

/// The protocol version every connection declares on its authenticating first line.
pub const PROTOCOL_VERSION: u64 = 1;
/// Longest line a mailbox reads; a longer one closes the connection.
const MAX_LINE_BYTES: usize = 1 << 20;
const WRITE_TIMEOUT: Duration = Duration::from_secs(5);

/// The first line of every connection: the recipient's own key.
#[derive(Serialize, Deserialize)]
struct Auth {
    taskai: u64,
    #[serde(rename = "type")]
    kind: String,
    key: String,
}

/// True when `line` is a version-1 auth line carrying exactly `key`, compared in constant time.
pub(super) fn authenticates(line: &str, key: &str) -> bool {
    serde_json::from_str::<Auth>(line).is_ok_and(|auth| {
        auth.taskai == PROTOCOL_VERSION
            && auth.kind == "auth"
            && auth.key.len() == key.len()
            && auth
                .key
                .bytes()
                .zip(key.bytes())
                .fold(0, |differs, (a, b)| differs | (a ^ b))
                == 0
    })
}

/// Reads one line of at most 1 MiB; `None` at the end of the stream.
pub(super) fn read_line(reader: &mut impl BufRead) -> Result<Option<String>, Error> {
    let mut line = Vec::new();
    let read = reader
        .by_ref()
        .take(MAX_LINE_BYTES as u64 + 1)
        .read_until(b'\n', &mut line)?;
    if read == 0 {
        return Ok(None);
    }
    if line.len() > MAX_LINE_BYTES {
        return Err(Error::Invalid("line longer than 1 MiB".into()));
    }
    String::from_utf8(line)
        .map(Some)
        .map_err(|_| Error::Invalid("line is not UTF-8".into()))
}

/// Writes to a TaskAI mailbox: the auth line with the key kept beside its socket, then the frame.
#[derive(Clone, Copy, Debug, Default)]
pub struct SocketTransport;

impl Transport for SocketTransport {
    fn send(&self, to: &MailPeer, frame: &Frame) -> Result<(), Error> {
        let key = std::fs::read_to_string(to.mailbox.with_extension("key"))?;
        let auth = Auth {
            taskai: PROTOCOL_VERSION,
            kind: "auth".into(),
            key: key.trim().into(),
        };
        let mut payload = String::new();
        for line in [serde_json::to_string(&auth), serde_json::to_string(frame)] {
            let line = line.map_err(|error| Error::Invalid(format!("frame: {error}")))?;
            // The recipient closes a longer line unread, so it must fail here, not vanish there.
            if line.len() + 1 > MAX_LINE_BYTES {
                return Err(Error::Invalid("letter frame longer than 1 MiB".into()));
            }
            payload.push_str(&line);
            payload.push('\n');
        }
        let mut stream = UnixStream::connect(&to.mailbox)?;
        stream.set_write_timeout(Some(WRITE_TIMEOUT))?;
        stream.write_all(payload.as_bytes())?;
        stream.shutdown(Shutdown::Write)?;
        Ok(())
    }
}

/// Whether a session asks before acting (`prompting`) or never asks (`bypass`).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PermissionMode {
    Prompting,
    Bypass,
}

/// `next` waits for the recipient's current turn; `now` asks to interrupt it.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Priority {
    #[default]
    Next,
    Now,
}

/// One frame after authentication. The `type` field names the variant.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Frame {
    Message(Letter),
    Receipt(Receipt),
    Subscribe(Subscription),
    IdleNotice(IdleNotice),
}

/// A message from one session to another, with the sender's real name and permission mode.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Letter {
    pub msg_id: String,
    pub from: String,
    pub from_mode: PermissionMode,
    #[serde(default)]
    pub hop_chain: Vec<String>,
    #[serde(default)]
    pub priority: Priority,
    pub body: String,
}

/// What happened to a letter, sent back to its sender. A normal delivery sends none.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Receipt {
    pub orig_msg_id: String,
    pub status: ReceiptStatus,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub drop_reason: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub dropped_msg_ids: Vec<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ReceiptStatus {
    Held,
    Denied,
    Expired,
    Delivered,
    Refused,
    Dropped,
}

/// A request for one idle notice when the recipient next goes idle or exits.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Subscription {
    pub msg_id: String,
    pub from: String,
    pub from_mode: PermissionMode,
}

/// The one answer to a subscription. `finished_at` is in milliseconds since the epoch.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct IdleNotice {
    pub orig_msg_id: String,
    pub state: NoticeState,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub finished_at: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum NoticeState {
    Idle,
    Exited,
    Unavailable,
}
