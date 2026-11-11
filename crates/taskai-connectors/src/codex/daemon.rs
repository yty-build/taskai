// ABOUTME: Codex shared daemon WebSocket client for one exact TaskAI-owned thread.
// ABOUTME: Creates dynamic tools, routes native events, relays turns, and never answers approvals.

use std::collections::{HashMap, HashSet, VecDeque};
use std::net::Shutdown;
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use tungstenite::client::IntoClientRequest;
use tungstenite::{client, Message, WebSocket};

use crate::Error;

/// A successful handshake reports the daemon's User-Agent, which is the version source.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Initialization {
    pub user_agent: String,
    pub daemon_version: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum EffectivePermissionMode {
    Prompting,
    Bypass,
    Unknown,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct EffectivePermission {
    pub native_approval_policy: Option<String>,
    pub mode: EffectivePermissionMode,
}

impl EffectivePermission {
    fn from_thread_result(result: &Value) -> Self {
        let native_approval_policy = result["approvalPolicy"].as_str().map(str::to_string);
        let mode = match native_approval_policy.as_deref() {
            Some("on-request") => EffectivePermissionMode::Prompting,
            Some("never") => EffectivePermissionMode::Bypass,
            _ => EffectivePermissionMode::Unknown,
        };
        Self {
            native_approval_policy,
            mode,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CodexActivity {
    Idle,
    Busy,
    WaitingApproval,
    WaitingInput,
}

impl CodexActivity {
    pub fn waiting_for(self) -> Option<&'static str> {
        match self {
            Self::WaitingApproval => Some("approval"),
            Self::WaitingInput => Some("input needed"),
            Self::Idle | Self::Busy => None,
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
pub struct ToolCall {
    request_id: Value,
    tool: String,
    arguments: Value,
    turn_id: Option<String>,
    /// Some(None) means a proven human/no-chain origin; None means unresolved.
    origin_chain: Option<Option<Vec<String>>>,
}

impl ToolCall {
    pub fn name(&self) -> &str {
        &self.tool
    }
}

#[derive(Clone, Debug, PartialEq)]
pub enum DaemonEvent {
    ToolCall(ToolCall),
    /// Passive current-policy evidence for the exact bound thread. Unknown revokes old evidence.
    EffectivePermission(EffectivePermission),
    Activity(CodexActivity),
    Detail(String),
    TurnStarted(String),
    TurnCompleted(String),
    /// Only the method name is exposed; request parameters may contain private approval data.
    UnansweredRequest {
        method: String,
    },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RelayKind {
    Message,
    Notice,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RelayResult {
    pub turn_id: String,
}

enum RequestFailure {
    Rejected(Error),
    Uncertain(Error),
}

/// M6 supplies the mailbox implementation. The daemon cannot supply identity or hop overrides.
pub trait TaskaiTools {
    fn list_sessions(&mut self) -> Result<Value, String>;
    fn send_message(
        &mut self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
        chain: Option<&[String]>,
    ) -> Result<Value, String>;
}

/// Ends one `DaemonClient`'s connection from any thread, waking an RPC blocked on it: a request
/// already written then reports `Error::Uncertain`, and once `shutdown` has returned the client
/// refuses every request before writing it, as a definite `Error::Invalid`. It holds a duplicate
/// of that connection's socket, so it can never end another, and it never reads, writes or asks
/// the daemon anything. The daemon is certain to see the connection end only once the client and
/// every handle are dropped, closing the socket; a shutdown usually shows it sooner.
pub struct DaemonShutdown {
    connection: UnixStream,
    closed: Arc<AtomicBool>,
}

impl DaemonShutdown {
    /// Shuts the connection down both ways; calling it again does nothing more.
    pub fn shutdown(&self) {
        self.closed.store(true, Ordering::SeqCst);
        let _ = self.connection.shutdown(Shutdown::Both);
    }
}

pub struct DaemonClient {
    socket: WebSocket<UnixStream>,
    /// Set by a `DaemonShutdown` before it ends the connection.
    closed: Arc<AtomicBool>,
    next_id: u64,
    initialized: bool,
    thread_id: Option<String>,
    effective_permission: Option<EffectivePermission>,
    active_turn: Option<String>,
    turn_chains: HashMap<String, Option<Vec<String>>>,
    completed_turns: HashSet<String>,
    acceptance_uncertain: bool,
    events: VecDeque<DaemonEvent>,
    early_frames: Vec<Value>,
}

impl DaemonClient {
    /// Opens only `/rpc` on an injected Unix socket. No process is spawned.
    pub fn connect(path: &Path, timeout: Duration) -> Result<Self, Error> {
        let stream = UnixStream::connect(path)?;
        stream.set_read_timeout(Some(timeout))?;
        stream.set_write_timeout(Some(timeout))?;
        let mut request = "ws://localhost/rpc"
            .into_client_request()
            .map_err(|error| Error::Invalid(format!("Codex WebSocket request: {error}")))?;
        request.headers_mut().insert(
            "User-Agent",
            tungstenite::http::HeaderValue::from_str(&format!(
                "taskai-bridge/{}",
                env!("CARGO_PKG_VERSION")
            ))
            .map_err(|error| Error::Invalid(format!("Codex User-Agent: {error}")))?,
        );
        let (socket, _) = client(request, stream)
            .map_err(|error| Error::Invalid(format!("codex WebSocket handshake: {error}")))?;
        Ok(Self {
            socket,
            closed: Arc::new(AtomicBool::new(false)),
            next_id: 1,
            initialized: false,
            thread_id: None,
            effective_permission: None,
            active_turn: None,
            turn_chains: HashMap::new(),
            completed_turns: HashSet::new(),
            acceptance_uncertain: false,
            events: VecDeque::new(),
            early_frames: Vec::new(),
        })
    }

    pub fn initialize(&mut self, timeout: Duration) -> Result<Initialization, Error> {
        let reply = self.request(
            "initialize",
            json!({"clientInfo": {
                "name": "taskai-bridge",
                "title": "TaskAI bridge",
                "version": env!("CARGO_PKG_VERSION"),
            }, "capabilities": {"experimentalApi": true}}),
            timeout,
        )?;
        let user_agent = reply["userAgent"]
            .as_str()
            .ok_or_else(|| Error::Invalid("initialize did not return a User-Agent".into()))?
            .to_string();
        self.write_json(&json!({"method":"initialized"}))?;
        self.initialized = true;
        let daemon_version = user_agent
            .split_whitespace()
            .next()
            .and_then(|product| product.split_once('/'))
            .map(|(_, version)| version.to_string());
        Ok(Initialization {
            user_agent,
            daemon_version,
        })
    }

    /// Creates one thread with both dynamic tools. This does not start a model turn.
    pub fn start_thread(&mut self, cwd: &Path, timeout: Duration) -> Result<String, Error> {
        if !self.initialized || self.thread_id.is_some() {
            return Err(Error::Invalid(
                "Codex client must initialize once before thread/start".into(),
            ));
        }
        let reply = self.request(
            "thread/start",
            json!({
                "cwd": cwd,
                "dynamicTools": dynamic_tools(),
            }),
            timeout,
        )?;
        self.bind_thread_result(&reply, None)
    }

    /// Rejoins a registry-validated TaskAI thread and resubscribes this connection to events.
    /// The caller must validate the saved ID against TaskAI's own registry before invoking this.
    pub fn attach_owned_thread(
        &mut self,
        registry_verified_thread_id: &str,
        timeout: Duration,
    ) -> Result<String, Error> {
        if !self.initialized || self.thread_id.is_some() || registry_verified_thread_id.is_empty() {
            return Err(Error::Invalid(
                "Codex client needs an initialized, unbound connection and a verified thread ID"
                    .into(),
            ));
        }
        let reply = self.request(
            "thread/resume",
            json!({
                "threadId": registry_verified_thread_id,
                "excludeTurns": true,
            }),
            timeout,
        )?;
        self.bind_thread_result(&reply, Some(registry_verified_thread_id))
    }

    fn bind_thread_result(
        &mut self,
        reply: &Value,
        expected_id: Option<&str>,
    ) -> Result<String, Error> {
        let thread_id = reply["thread"]["id"]
            .as_str()
            .filter(|id| !id.is_empty())
            .ok_or_else(|| Error::Invalid("Codex thread response returned no thread.id".into()))?
            .to_string();
        if expected_id.is_some_and(|expected| expected != thread_id) {
            let _ = self.socket.get_mut().shutdown(Shutdown::Both);
            return Err(Error::Invalid("Codex thread.id mismatch on resume".into()));
        }
        self.effective_permission = Some(EffectivePermission::from_thread_result(reply));
        self.thread_id = Some(thread_id.clone());
        for frame in std::mem::take(&mut self.early_frames) {
            self.handle_frame(frame);
        }
        if let Some(activity) = status_activity(&reply["thread"]["status"]) {
            self.events.push_back(DaemonEvent::Activity(activity));
        }
        Ok(thread_id)
    }

    /// A handle that ends this client's connection from another thread. A thread that owns the
    /// client while a relay is blocked cannot drop it, so a bridge stops that thread with the
    /// handle's `shutdown`, joins it, and then drops the handle so the daemon sees the connection
    /// close.
    pub fn shutdown_handle(&self) -> Result<DaemonShutdown, Error> {
        Ok(DaemonShutdown {
            connection: self.socket.get_ref().try_clone()?,
            closed: Arc::clone(&self.closed),
        })
    }

    pub fn thread_id(&self) -> Option<&str> {
        self.thread_id.as_deref()
    }

    pub fn effective_permission(&self) -> Option<&EffectivePermission> {
        self.effective_permission.as_ref()
    }

    /// The visible TUI joins the TaskAI-created thread with no config/profile overrides.
    pub fn resume_command(&self, bin: &str) -> Result<(String, Vec<String>), Error> {
        let thread_id = self.owned_thread()?;
        Ok((bin.into(), vec!["resume".into(), thread_id.into()]))
    }

    /// M6 shows this line if the installed TUI cannot resume an empty thread.
    pub fn first_message_prompt() -> &'static str {
        "TaskAI: type your first message to start this Codex thread."
    }

    /// Called only after a person submits the first message through the TaskAI pane.
    pub fn first_human_message(
        &mut self,
        text: &str,
        timeout: Duration,
    ) -> Result<RelayResult, Error> {
        if text.trim().is_empty() {
            return Err(Error::Invalid("first message is empty".into()));
        }
        self.start_turn(text, None, timeout)
    }

    /// Relays M1's already-prepared delivery text. A successful return means one mailbox
    /// delivery reached `turn/start`, including steering.
    pub fn relay(
        &mut self,
        kind: RelayKind,
        text: &str,
        chain: Option<Vec<String>>,
        timeout: Duration,
    ) -> Result<RelayResult, Error> {
        let chain = if kind == RelayKind::Message {
            chain
        } else {
            None
        };
        self.start_turn(text, chain, timeout)
    }

    fn start_turn(
        &mut self,
        text: &str,
        chain: Option<Vec<String>>,
        timeout: Duration,
    ) -> Result<RelayResult, Error> {
        if self.acceptance_uncertain {
            return Err(Error::Invalid(
                "Codex prior turn/start acceptance is unknown; this prompt was not written".into(),
            ));
        }
        let thread_id = self.owned_thread()?.to_string();
        let previous_active = self.active_turn.clone();
        let previous_known_chain = previous_active.as_deref().and_then(|id| {
            self.turn_chains
                .get(id)
                .cloned()
                .map(|chain| (id.to_string(), chain))
        });
        let queued_before_request = self.events.len();
        let result = self.request_classified(
            "turn/start",
            json!({
                "threadId": thread_id,
                "input": [{"type":"text", "text":text, "text_elements":[]}],
            }),
            timeout,
        );
        let result = match result {
            Ok(result) => result,
            Err(RequestFailure::Rejected(error)) => return Err(error),
            Err(RequestFailure::Uncertain(error)) => {
                self.mark_acceptance_uncertain(previous_known_chain);
                return Err(Error::Uncertain(format!("turn/start: {error}")));
            }
        };
        let Some(turn_id) = result["turn"]["id"].as_str().filter(|id| !id.is_empty()) else {
            self.mark_acceptance_uncertain(previous_known_chain);
            return Err(Error::Uncertain("turn/start returned no turn.id".into()));
        };
        let turn_id = turn_id.to_string();
        let turn_was_active = previous_active.as_deref() == Some(turn_id.as_str());
        let mut origin_chain = if turn_was_active {
            previous_known_chain
                .as_ref()
                .map(|(_, chain)| chain.clone())
                .unwrap_or(None)
        } else {
            chain
        };
        if !self.completed_turns.contains(&turn_id) {
            origin_chain = self
                .turn_chains
                .entry(turn_id.clone())
                .or_insert(origin_chain)
                .clone();
            self.active_turn = Some(turn_id.clone());
        }
        // A tool request may precede this result, even if completion also arrived first.
        // Correlate only calls queued during this request; completed turns need no history map.
        for event in self.events.iter_mut().skip(queued_before_request) {
            if let DaemonEvent::ToolCall(call) = event {
                if call.turn_id.as_deref() == Some(turn_id.as_str()) {
                    call.origin_chain = Some(origin_chain.clone());
                }
            }
        }
        Ok(RelayResult { turn_id })
    }

    fn mark_acceptance_uncertain(&mut self, previous: Option<(String, Option<Vec<String>>)>) {
        self.acceptance_uncertain = true;
        if let Some((turn_id, chain)) = previous {
            self.turn_chains.insert(turn_id, chain);
        }
    }

    pub fn chain_for_turn(&self, turn_id: &str) -> Option<&[String]> {
        self.turn_chains.get(turn_id).and_then(Option::as_deref)
    }

    /// Returns only events for the exact thread created or reattached by this client.
    pub fn next_event(&mut self, timeout: Duration) -> Result<Option<DaemonEvent>, Error> {
        if let Some(event) = self.events.pop_front() {
            return Ok(Some(event));
        }
        let deadline = Instant::now() + timeout;
        loop {
            if Instant::now() >= deadline {
                return Ok(None);
            }
            match self.read_json(deadline.saturating_duration_since(Instant::now())) {
                Ok(value) => self.handle_frame(value),
                Err(Error::Timeout(_)) => return Ok(None),
                Err(error) => return Err(error),
            }
            if let Some(event) = self.events.pop_front() {
                return Ok(Some(event));
            }
        }
    }

    /// Dispatches only `taskai.list_sessions` and `taskai.send_message` to the injected mailbox.
    pub fn answer_tool_call<T: TaskaiTools>(
        &mut self,
        call: ToolCall,
        tools: &mut T,
    ) -> Result<(), Error> {
        let result = match call.tool.as_str() {
            "list_sessions"
                if call
                    .arguments
                    .as_object()
                    .is_some_and(serde_json::Map::is_empty) =>
            {
                tools.list_sessions()
            }
            "send_message" => {
                if let Some((to, text, notify)) = parse_send_arguments(&call.arguments) {
                    let known_chain = call.origin_chain.as_ref().or_else(|| {
                        call.turn_id
                            .as_deref()
                            .and_then(|id| self.turn_chains.get(id))
                    });
                    let chain = if let Some(chain) = known_chain {
                        Ok(chain.as_deref())
                    } else if self.acceptance_uncertain
                        || call
                            .turn_id
                            .as_deref()
                            .is_some_and(|id| self.completed_turns.contains(id))
                    {
                        Err(
                            "Codex turn chain unknown after completion or uncertain acceptance"
                                .to_string(),
                        )
                    } else {
                        Ok(None)
                    };
                    match chain {
                        Ok(chain) => tools.send_message(to, text, notify, chain),
                        Err(error) => Err(error),
                    }
                } else {
                    Err("invalid send_message arguments".into())
                }
            }
            "list_sessions" => Err("list_sessions takes an empty object".into()),
            _ => Err("unknown TaskAI tool".into()),
        };
        let success = result
            .as_ref()
            .is_ok_and(|value| matches!(value["status"].as_str(), None | Some("ok" | "sent")));
        let output = match result {
            Ok(value) => value,
            Err(error) => json!({"status":"error", "reason":error}),
        };
        self.write_json(&json!({"id": call.request_id, "result": {
            "contentItems":[{"type":"inputText", "text": output.to_string()}],
            "success":success,
        }}))
    }

    pub fn interrupt(&mut self, turn_id: &str, timeout: Duration) -> Result<(), Error> {
        let thread_id = self.owned_thread()?.to_string();
        self.request(
            "turn/interrupt",
            json!({"threadId":thread_id,"turnId":turn_id}),
            timeout,
        )?;
        Ok(())
    }

    pub fn unarchive(&mut self, timeout: Duration) -> Result<(), Error> {
        let thread_id = self.owned_thread()?.to_string();
        self.request("thread/unarchive", json!({"threadId":thread_id}), timeout)?;
        Ok(())
    }

    fn owned_thread(&self) -> Result<&str, Error> {
        self.thread_id
            .as_deref()
            .ok_or_else(|| Error::NotAttached("Codex thread not started".into()))
    }

    fn request(&mut self, method: &str, params: Value, timeout: Duration) -> Result<Value, Error> {
        self.request_classified(method, params, timeout)
            .map_err(|failure| match failure {
                RequestFailure::Rejected(error) | RequestFailure::Uncertain(error) => error,
            })
    }

    fn request_classified(
        &mut self,
        method: &str,
        params: Value,
        timeout: Duration,
    ) -> Result<Value, RequestFailure> {
        if self.closed.load(Ordering::SeqCst) {
            return Err(RequestFailure::Rejected(Error::Invalid(format!(
                "{method}: the Codex connection was shut down; nothing was written"
            ))));
        }
        let id = self.next_id;
        self.next_id += 1;
        // A socket write error can follow a partial send, so it cannot prove nondelivery.
        self.write_json(&json!({"id":id,"method":method,"params":params}))
            .map_err(RequestFailure::Uncertain)?;
        let deadline = Instant::now() + timeout;
        loop {
            if Instant::now() >= deadline {
                return Err(RequestFailure::Uncertain(Error::Timeout(format!(
                    "{method}: no response"
                ))));
            }
            let frame = self
                .read_json(deadline.saturating_duration_since(Instant::now()))
                .map_err(RequestFailure::Uncertain)?;
            if frame.get("id").and_then(Value::as_u64) == Some(id) && frame.get("method").is_none()
            {
                if let Some(error) = frame.get("error") {
                    if frame.get("result").is_some()
                        || !error["code"].is_i64()
                        || !error["message"].is_string()
                    {
                        return Err(RequestFailure::Uncertain(Error::Invalid(format!(
                            "{method}: malformed RPC error"
                        ))));
                    }
                    return Err(RequestFailure::Rejected(Error::Invalid(format!(
                        "{method}: {}",
                        error["message"].as_str().unwrap_or("daemon error")
                    ))));
                }
                return frame.get("result").cloned().ok_or_else(|| {
                    RequestFailure::Uncertain(Error::Invalid(format!("{method}: missing result")))
                });
            }
            self.handle_frame(frame);
        }
    }

    fn write_json(&mut self, value: &Value) -> Result<(), Error> {
        self.socket
            .send(Message::Text(value.to_string()))
            .map_err(|error| Error::Invalid(format!("Codex daemon write: {error}")))
    }

    fn read_json(&mut self, timeout: Duration) -> Result<Value, Error> {
        let deadline = Instant::now() + timeout;
        loop {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(Error::Timeout("Codex daemon read timed out".into()));
            }
            // macOS rejects a sub-microsecond SO_RCVTIMEO after rounding it to zero. It also refuses
            // any timeout (EINVAL) once the connection is closed both ways; frames the daemon sent
            // first are still buffered then and a read cannot block, so that refusal is harmless.
            let timed = self
                .socket
                .get_mut()
                .set_read_timeout(Some(remaining.max(Duration::from_millis(1))));
            if let Err(error) = timed {
                if error.kind() != std::io::ErrorKind::InvalidInput {
                    return Err(Error::Invalid(format!(
                        "Codex read timeout {remaining:?}: {error}"
                    )));
                }
            }
            let message = match self.socket.read() {
                Ok(message) => message,
                Err(tungstenite::Error::Io(error))
                    if matches!(
                        error.kind(),
                        std::io::ErrorKind::TimedOut | std::io::ErrorKind::WouldBlock
                    ) =>
                {
                    return Err(Error::Timeout("Codex daemon read timed out".into()));
                }
                Err(error) => return Err(Error::Invalid(format!("Codex daemon read: {error}"))),
            };
            match message {
                Message::Text(text) => {
                    return serde_json::from_str(&text)
                        .map_err(|_| Error::Invalid("Codex daemon sent invalid JSON".into()))
                }
                Message::Ping(_) | Message::Pong(_) | Message::Binary(_) | Message::Frame(_) => {
                    continue
                }
                Message::Close(_) => {
                    return Err(Error::NotAttached("Codex daemon closed the socket".into()))
                }
            }
        }
    }

    fn handle_frame(&mut self, frame: Value) {
        let Some(method) = frame["method"].as_str() else {
            return;
        };
        let params = &frame["params"];
        let Some(owned) = self.thread_id.as_deref() else {
            if thread_of(params).is_some() && self.early_frames.len() < 500 {
                self.early_frames.push(frame);
            }
            return;
        };
        if thread_of(params) != Some(owned) {
            return;
        }
        if let Some(id) = frame.get("id") {
            if method == "item/tool/call" && params["namespace"] == "taskai" {
                let turn_id = params["turnId"].as_str().map(str::to_string);
                let origin_chain = turn_id
                    .as_deref()
                    .and_then(|id| self.turn_chains.get(id).cloned());
                self.events.push_back(DaemonEvent::ToolCall(ToolCall {
                    request_id: id.clone(),
                    tool: params["tool"].as_str().unwrap_or_default().into(),
                    arguments: params["arguments"].clone(),
                    turn_id,
                    origin_chain,
                }));
            } else {
                self.events.push_back(DaemonEvent::UnansweredRequest {
                    method: method.into(),
                });
            }
            return;
        }
        match method {
            "thread/settings/updated" => {
                let permission = EffectivePermission::from_thread_result(&params["threadSettings"]);
                self.effective_permission = Some(permission.clone());
                self.events
                    .push_back(DaemonEvent::EffectivePermission(permission));
            }
            "thread/status/changed" => {
                if let Some(activity) = status_activity(&params["status"]) {
                    self.events.push_back(DaemonEvent::Activity(activity));
                }
            }
            "turn/started" => {
                if let Some(turn_id) = params["turn"]["id"].as_str() {
                    self.active_turn = Some(turn_id.into());
                    self.completed_turns.remove(turn_id);
                    self.events
                        .push_back(DaemonEvent::TurnStarted(turn_id.into()));
                }
            }
            "turn/completed" => {
                if let Some(turn_id) = params["turn"]["id"].as_str() {
                    if !self.acceptance_uncertain {
                        self.turn_chains.remove(turn_id);
                    }
                    self.completed_turns.insert(turn_id.into());
                    if self.active_turn.as_deref() == Some(turn_id) {
                        self.active_turn = None;
                    }
                    self.events
                        .push_back(DaemonEvent::TurnCompleted(turn_id.into()));
                }
            }
            "item/completed" if params["item"]["type"] == "agentMessage" => {
                if let Some(text) = params["item"]["text"].as_str() {
                    self.events.push_back(DaemonEvent::Detail(text.into()));
                }
            }
            _ => {}
        }
    }
}

impl Drop for DaemonClient {
    fn drop(&mut self) {
        let _ = self.socket.get_mut().shutdown(Shutdown::Both);
    }
}

fn thread_of(params: &Value) -> Option<&str> {
    params["threadId"]
        .as_str()
        .or_else(|| params["conversationId"].as_str())
        .or_else(|| params["thread"]["id"].as_str())
}

fn status_activity(status: &Value) -> Option<CodexActivity> {
    match status["type"].as_str() {
        Some("active")
            if status["activeFlags"]
                .as_array()
                .is_some_and(|flags| flags.iter().any(|flag| flag == "waitingOnApproval")) =>
        {
            Some(CodexActivity::WaitingApproval)
        }
        Some("active")
            if status["activeFlags"]
                .as_array()
                .is_some_and(|flags| flags.iter().any(|flag| flag == "waitingOnUserInput")) =>
        {
            Some(CodexActivity::WaitingInput)
        }
        Some("active") => Some(CodexActivity::Busy),
        Some("idle" | "notLoaded") => Some(CodexActivity::Idle),
        _ => None,
    }
}

fn parse_send_arguments(args: &Value) -> Option<(&str, &str, bool)> {
    let object = args.as_object()?;
    if object
        .keys()
        .any(|key| !matches!(key.as_str(), "to" | "text" | "notify_when_idle"))
    {
        return None;
    }
    let to = object
        .get("to")?
        .as_str()
        .filter(|value| !value.is_empty())?;
    let text = object.get("text")?.as_str()?;
    let notify = match object.get("notify_when_idle") {
        Some(value) => value.as_bool()?,
        None => false,
    };
    Some((to, text, notify))
}

fn dynamic_tools() -> Value {
    json!([{
        "type":"namespace", "name":"taskai",
        "description":"TaskAI cross-session messaging: find other sessions and message them.",
        "tools":[
            {"type":"function", "name":"list_sessions",
             "description":"List live sessions you can message: name, harness, status and waiting reason.",
             "inputSchema":{"type":"object","properties":{},"additionalProperties":false}},
            {"type":"function", "name":"send_message",
             "description":"Send a message to a live session by name; optionally receive one idle notice.",
             "inputSchema":{"type":"object",
               "properties":{
                 "to":{"type":"string","description":"Recipient session name."},
                 "text":{"type":"string","description":"Message text."},
                 "notify_when_idle":{"type":"boolean","description":"Receive one idle notice."}},
               "required":["to","text"],"additionalProperties":false}}
        ]
    }])
}
