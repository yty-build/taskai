// ABOUTME: Grok adapter's Mailbox types and trait, matching accepted core exports
// ABOUTME: so tools and relay pass Transport at open and complete by Delivery.id.

use std::path::PathBuf;

use serde::{Deserialize, Serialize};

use super::PermissionMode as CliMode;
use crate::Error;

/// Wire permission mode: `prompting` or `bypass`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PermissionMode {
    Prompting,
    Bypass,
}

impl From<CliMode> for PermissionMode {
    fn from(mode: CliMode) -> Self {
        match mode {
            CliMode::Prompting => PermissionMode::Prompting,
            CliMode::Bypass => PermissionMode::Bypass,
        }
    }
}

/// Registry pulse: `busy`, `idle`, or `waiting`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Activity {
    Busy,
    Idle,
    Waiting,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DeliveryKind {
    Message,
    Receipt,
    Notice,
}

/// One item the mailbox hands the adapter. `id` is unique per delivery.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Delivery {
    pub id: String,
    pub kind: DeliveryKind,
    pub text: String,
    pub chain: Option<Vec<String>>,
    pub msg_id: String,
    pub sender: String,
}

/// Caller-known session identity. The mailbox fills pid, procStart, socket,
/// features and status when it registers.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Identity {
    pub session_ref: String,
    pub task_ref: String,
    pub name: String,
    pub harness: String,
    pub workspace: PathBuf,
    pub mode: PermissionMode,
}

impl Identity {
    pub fn grok(
        session_ref: impl Into<String>,
        task_ref: impl Into<String>,
        name: impl Into<String>,
        workspace: PathBuf,
        mode: CliMode,
    ) -> Self {
        Self {
            session_ref: session_ref.into(),
            task_ref: task_ref.into(),
            name: name.into(),
            harness: "grok".into(),
            workspace,
            mode: PermissionMode::from(mode),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MailboxConfig {
    pub identity: Identity,
    pub mail_dir: PathBuf,
    pub log_path: PathBuf,
    pub max_deliveries: usize,
}

/// A directory entry: what M2 resolves from the registry.
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
    pub mode: PermissionMode,
}

/// Injected address book. M2 implements it over TaskAI's registry.
pub trait Directory: Send + Sync {
    fn live_sessions(&self) -> Result<Vec<MailPeer>, Error>;
    fn register(&self, entry: &MailPeer) -> Result<(), Error>;
    fn set_status(
        &self,
        session_ref: &str,
        status: Activity,
        waiting_for: Option<&str>,
    ) -> Result<(), Error>;
    fn unregister(&self, session_ref: &str) -> Result<(), Error>;
}

/// Injected outbound boundary. Default: TaskAI auth line then one NDJSON frame.
pub trait Transport: Send + Sync {
    fn send(&self, to: &MailPeer, frame: &Frame) -> Result<(), Error>;
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Priority {
    #[default]
    Next,
    Now,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Frame {
    Message(Letter),
    Receipt(Receipt),
    Subscribe(Subscription),
    IdleNotice(IdleNotice),
}

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

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Subscription {
    pub msg_id: String,
    pub from: String,
    pub from_mode: PermissionMode,
}

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

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ListedSession {
    pub name: String,
    pub harness: String,
    pub status: Activity,
    pub waiting_for: Option<String>,
}

/// Methods Grok calls on a mailbox. M1 owns the concrete type; Grok does not
/// admit, authenticate, or write frames.
pub trait Mailbox: Send + Sync {
    fn send(
        &self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
        chain: Option<&[String]>,
    ) -> SendResult;
    fn subscribe(&self, to: &str) -> SubscriptionResult;
    fn list_sessions(&self) -> Result<Vec<ListedSession>, Error>;
    fn set_activity(&self, activity: Activity, waiting_for: Option<&str>);
    fn set_detail(&self, text: &str);
    fn turn_started(&self, delivery_id: &str);
    fn delivery_failed(&self, delivery_id: &str, reason: &str);
    fn close(&self);
}
