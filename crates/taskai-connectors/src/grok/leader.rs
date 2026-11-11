// ABOUTME: Grok leader ACP client — explicit permission mode, yolo_mode gate
// ABOUTME: before session/load, serialized session/prompt, never answers approvals.

use std::collections::HashMap;
use std::fs::{self, File, OpenOptions};
use std::io::{BufRead, BufReader, Read, Seek, SeekFrom, Write};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::mpsc::{self, Receiver, Sender};
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc, Mutex,
};
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use taskai_core::mail::strip_messaging_env;

use crate::Error;

/// Longest unfinished line the registration gate keeps; a longer line is no registration and is
/// dropped.
const MAX_LOG_LINE_BYTES: usize = 16 * 1024;
/// Most bytes the registration gate reads from the leader log at a time.
const LOG_READ_BYTES: u64 = 64 * 1024;
/// Most new leader log bytes the registration gate inspects before it gives up.
const MAX_LOG_TAIL_BYTES: u64 = 4 * 1024 * 1024;
const CANCEL_POLL: Duration = Duration::from_millis(25);
/// Pause between two session/load requests while the native session does not exist yet.
const LOAD_RETRY_PAUSE: Duration = Duration::from_millis(200);

/// How to spawn and join a second leader client. Tests inject a fake process.
pub struct JoinConfig {
    pub bin: String,
    pub args: Vec<String>,
    pub cwd: PathBuf,
    pub leader_socket: PathBuf,
    pub leader_log: PathBuf,
    pub session_id: String,
    pub timeout: Duration,
    pub extra_env: Vec<(String, String)>,
}

/// Status pulse parsed from Grok ACP notifications. Approval contents are dropped.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum StatusEvent {
    Busy,
    Idle,
    WaitingApproval,
    Detail(String),
    Unknown,
}

enum RpcReply {
    Message(Value),
    Disconnected,
    ShutDown,
}

pub struct LeaderClient {
    child: Mutex<Child>,
    stdin: Mutex<ChildStdin>,
    pending: Arc<Mutex<HashMap<String, Sender<RpcReply>>>>,
    notifications: Mutex<Receiver<Value>>,
    next_id: Mutex<u64>,
    session_id: String,
    relay: Mutex<()>,
    uncertain: Arc<Mutex<Option<String>>>,
}

impl LeaderClient {
    /// Initialize, require yolo_mode=false on the joining client, then load.
    pub fn join(config: JoinConfig) -> Result<LeaderClient, Error> {
        Self::join_with_cancel(config, Arc::new(AtomicBool::new(false)))
    }

    /// The same guarded join, with cancellation throughout startup. A cancelled reply wait
    /// is uncertain after its request was written; no later startup request is issued. The
    /// joining child is killed and reaped before any error returns. The native leader is not
    /// owned here. No watcher or cancellation flag is retained by a successful client.
    pub fn join_with_cancel(
        config: JoinConfig,
        cancelled: Arc<AtomicBool>,
    ) -> Result<LeaderClient, Error> {
        check_join_cancelled(Some(&cancelled))?;
        let mut registrations = LogTail::at_end(&config.leader_log)?;
        let mut command = Command::new(&config.bin);
        command
            .args(&config.args)
            .current_dir(&config.cwd)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null());
        for (key, value) in &config.extra_env {
            command.env(key, value);
        }
        strip_messaging_env(&mut command);
        command.env("GROK_LEADER_SOCKET", &config.leader_socket);
        command.env("GROK_DISABLE_AUTOUPDATER", "1");
        command.env("CMUX_GROK_HOOKS_DISABLED", "1");
        check_join_cancelled(Some(&cancelled))?;
        let mut child = command.spawn()?;
        let stdin = child
            .stdin
            .take()
            .ok_or_else(|| Error::Invalid("no stdin".into()))?;
        let stdout = child
            .stdout
            .take()
            .ok_or_else(|| Error::Invalid("no stdout".into()))?;
        let pending: Arc<Mutex<HashMap<String, Sender<RpcReply>>>> =
            Arc::new(Mutex::new(HashMap::new()));
        let uncertain = Arc::new(Mutex::new(None));
        let (notif_tx, notif_rx) = mpsc::channel();
        let pending_reader = Arc::clone(&pending);
        let uncertain_reader = Arc::clone(&uncertain);
        std::thread::spawn(move || {
            read_replies(stdout, pending_reader, uncertain_reader, notif_tx)
        });
        let client = LeaderClient {
            child: Mutex::new(child),
            stdin: Mutex::new(stdin),
            pending,
            notifications: Mutex::new(notif_rx),
            next_id: Mutex::new(1),
            session_id: config.session_id.clone(),
            relay: Mutex::new(()),
            uncertain,
        };
        let init = match client.request_with_cancel(
            "initialize",
            json!({
                "protocolVersion": 1,
                "clientCapabilities": {
                    "fs": {"readTextFile": false, "writeTextFile": false},
                    "terminal": false
                }
            }),
            config.timeout,
            Some(&cancelled),
        ) {
            Ok(result) => result,
            Err(err) => {
                drop(client);
                return Err(err);
            }
        };
        if init["agentCapabilities"]["loadSession"] != true {
            drop(client);
            return Err(Error::Invalid(
                "leader initialize did not report loadSession".into(),
            ));
        }
        check_join_cancelled(Some(&cancelled))?;
        match registrations.wait_for_registration(
            &config.leader_log,
            &config.leader_socket.display().to_string(),
            config.timeout,
            Some(&cancelled),
        ) {
            Ok(()) => {}
            Err(err) => {
                drop(client);
                return Err(err);
            }
        }
        // The native CLI creates its session a moment after it starts its leader, so a bridge
        // started with it reaches the leader first. Until the session exists the leader refuses
        // the load outright and loads nothing, so asking again is safe. Any other outcome, or a
        // refusal that outlasts the join's time limit, ends the join.
        let give_up = Instant::now() + config.timeout;
        loop {
            let refusal = match client.exchange(
                "session/load",
                json!({
                    "sessionId": config.session_id,
                    "cwd": config.cwd.display().to_string(),
                    "mcpServers": [],
                    // In 1.0.77 reconnect only enables modes whose flags are true.
                    // Explicit false avoids the agent-default fallback for autoMode.
                    "_meta": {"yoloMode": false, "autoMode": false}
                }),
                config.timeout,
                Some(&cancelled),
            ) {
                Ok(Ok(_)) => break,
                Ok(Err(refusal)) => refusal,
                Err(err) => {
                    drop(client);
                    return Err(err);
                }
            };
            if Instant::now() >= give_up {
                drop(client);
                return Err(Error::Invalid(format!("session/load: {refusal}")));
            }
            std::thread::sleep(LOAD_RETRY_PAUSE);
            if let Err(err) = check_join_cancelled(Some(&cancelled)) {
                drop(client);
                return Err(err);
            }
        }
        check_join_cancelled(Some(&cancelled))?;
        Ok(client)
    }

    pub fn prompt(&self, text: &str, timeout: Duration) -> Result<Value, Error> {
        self.prompt_with(text, timeout, || {}, || {})
    }

    /// Serialize session/prompt. `before` runs under the relay lock before
    /// the write. `after` runs under that lock only for a confirmed result
    /// or confirmed rejection, so timeout/disconnect keep the active chain.
    pub fn prompt_with(
        &self,
        text: &str,
        timeout: Duration,
        before: impl FnOnce(),
        after: impl FnOnce(),
    ) -> Result<Value, Error> {
        let _gate = self.relay.lock().unwrap();
        if let Some(reason) = self.uncertain.lock().unwrap().clone() {
            return Err(Error::Invalid(format!("leader client unusable: {reason}")));
        }
        before();
        let result = self.request(
            "session/prompt",
            json!({
                "sessionId": self.session_id,
                "prompt": [{"type": "text", "text": text}]
            }),
            timeout,
        );
        if !is_uncertain_acceptance_result(&result) {
            after();
        }
        result
    }

    pub fn poll_notification(&self) -> Option<Value> {
        self.notifications.lock().unwrap().try_recv().ok()
    }

    pub(super) fn session_id(&self) -> &str {
        &self.session_id
    }

    /// Stops this client's own Grok process and wakes every request waiting on it, taking neither
    /// the relay nor the stdin lock: a request already written ends uncertain, and any later one is
    /// refused before its write. The process is killed and reaped; the leader it joined runs on.
    /// Calling it again does nothing more.
    pub fn shutdown(&self) {
        self.uncertain
            .lock()
            .unwrap()
            .get_or_insert_with(|| "client shut down".into());
        for (_, waiter) in self.pending.lock().unwrap().drain() {
            let _ = waiter.send(RpcReply::ShutDown);
        }
        if let Ok(mut child) = self.child.lock() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }

    fn request(&self, method: &str, params: Value, timeout: Duration) -> Result<Value, Error> {
        self.request_with_cancel(method, params, timeout, None)
    }

    fn request_with_cancel(
        &self,
        method: &str,
        params: Value,
        timeout: Duration,
        cancelled: Option<&AtomicBool>,
    ) -> Result<Value, Error> {
        self.exchange(method, params, timeout, cancelled)?
            .map_err(|refusal| Error::Invalid(format!("{method}: {refusal}")))
    }

    /// One request and its reply. The inner `Err` is the leader's own refusal: a well-formed
    /// JSON-RPC error, after which nothing was done for the request. Every other failure is the
    /// outer error, and one after the write leaves this client unusable.
    fn exchange(
        &self,
        method: &str,
        params: Value,
        timeout: Duration,
        cancelled: Option<&AtomicBool>,
    ) -> Result<Result<Value, String>, Error> {
        check_join_cancelled(cancelled)?;
        if let Some(reason) = self.uncertain.lock().unwrap().clone() {
            return Err(Error::Invalid(format!("leader client unusable: {reason}")));
        }
        let id = {
            let mut next = self.next_id.lock().unwrap();
            let id = *next;
            *next += 1;
            id
        };
        let (tx, rx) = mpsc::channel();
        self.pending.lock().unwrap().insert(id.to_string(), tx);
        {
            let mut stdin = self.stdin.lock().unwrap();
            if let Err(error) = check_join_cancelled(cancelled) {
                self.pending.lock().unwrap().remove(&id.to_string());
                return Err(error);
            }
            // A shutdown since this request was registered must stop it before its write. The
            // flag is read into a local first: holding it while taking the waiters would invert
            // the reader's order.
            let unusable = self.uncertain.lock().unwrap().clone();
            if let Some(reason) = unusable {
                self.pending.lock().unwrap().remove(&id.to_string());
                return Err(Error::Invalid(format!("leader client unusable: {reason}")));
            }
            if let Err(err) = writeln!(
                stdin,
                "{}",
                json!({"jsonrpc": "2.0", "id": id, "method": method, "params": params})
            )
            .and_then(|_| stdin.flush())
            {
                self.pending.lock().unwrap().remove(&id.to_string());
                let reason = format!("{method}: write failed: {err}");
                *self.uncertain.lock().unwrap() = Some(reason.clone());
                return Err(Error::Uncertain(reason));
            }
        }
        let deadline = Instant::now() + timeout;
        let reply = loop {
            if check_join_cancelled(cancelled).is_err() {
                self.shutdown();
                return Err(Error::Uncertain(format!(
                    "{method}: join cancelled after write"
                )));
            }
            let remaining = deadline.saturating_duration_since(Instant::now());
            let wait = if cancelled.is_some() {
                remaining.min(CANCEL_POLL)
            } else {
                remaining
            };
            match rx.recv_timeout(wait) {
                Err(mpsc::RecvTimeoutError::Timeout)
                    if cancelled.is_some() && Instant::now() < deadline => {}
                result => break result,
            }
        };
        let msg = match reply {
            Ok(RpcReply::Message(msg)) => msg,
            Ok(RpcReply::Disconnected) => {
                let reason = format!("{method}: leader stdout closed after write");
                *self.uncertain.lock().unwrap() = Some(reason.clone());
                return Err(Error::Uncertain(reason));
            }
            Ok(RpcReply::ShutDown) => {
                return Err(Error::Uncertain(format!(
                    "{method}: client shut down after write"
                )));
            }
            Err(_) => {
                self.pending.lock().unwrap().remove(&id.to_string());
                *self.uncertain.lock().unwrap() = Some(format!("{method}: no response"));
                return Err(Error::Timeout(format!("{method}: no response")));
            }
        };
        if let Some(error) = msg.get("error") {
            if msg.get("result").is_some()
                || !error["code"].is_i64()
                || !error["message"].is_string()
            {
                return Err(self.malformed_reply(method));
            }
            return Ok(Err(error["message"]
                .as_str()
                .unwrap_or("error")
                .to_string()));
        }
        let result = msg
            .get("result")
            .ok_or_else(|| self.malformed_reply(method))?;
        if method == "session/prompt"
            && !result["stopReason"]
                .as_str()
                .is_some_and(|reason| !reason.is_empty())
        {
            return Err(self.malformed_reply(method));
        }
        Ok(Ok(result.clone()))
    }

    fn malformed_reply(&self, method: &str) -> Error {
        let reason = format!("{method}: malformed acknowledgement");
        *self.uncertain.lock().unwrap() = Some(reason.clone());
        Error::Uncertain(reason)
    }
}

fn check_join_cancelled(cancelled: Option<&AtomicBool>) -> Result<(), Error> {
    if cancelled.is_some_and(|flag| flag.load(Ordering::SeqCst)) {
        Err(Error::Invalid("Grok leader join cancelled".into()))
    } else {
        Ok(())
    }
}

/// Routes the client's output: notifications and server requests to `notifications`, replies to
/// their waiting requests. When the output ends, the client is marked unusable and every waiter is
/// woken as disconnected.
fn read_replies(
    stdout: impl Read,
    pending: Arc<Mutex<HashMap<String, Sender<RpcReply>>>>,
    uncertain: Arc<Mutex<Option<String>>>,
    notifications: Sender<Value>,
) {
    for line in BufReader::new(stdout).lines().map_while(Result::ok) {
        let Ok(msg) = serde_json::from_str::<Value>(&line) else {
            continue;
        };
        if msg.get("method").is_some() {
            let _ = notifications.send(msg);
            continue;
        }
        if let Some(id) = msg.get("id") {
            if let Some(tx) = pending.lock().unwrap().remove(&id.to_string()) {
                let _ = tx.send(RpcReply::Message(msg));
            }
        }
    }
    // Marking first, never under the waiter lock, means a request registered after the drain
    // still sees the mark before its write.
    *uncertain.lock().unwrap() = Some("leader stdout closed".into());
    for (_, tx) in pending.lock().unwrap().drain() {
        let _ = tx.send(RpcReply::Disconnected);
    }
}

fn is_uncertain_acceptance_result(result: &Result<Value, Error>) -> bool {
    result.as_ref().err().is_some_and(is_uncertain_acceptance)
}

pub(super) fn is_uncertain_acceptance(err: &Error) -> bool {
    matches!(err, Error::Timeout(_) | Error::Uncertain(_))
}

impl Drop for LeaderClient {
    fn drop(&mut self) {
        if let Ok(mut child) = self.child.lock() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

/// The shared leader log from a starting point on, read forward only and in bounded pieces: the
/// file its path named at the start, how far it has been read, and an unfinished line.
struct LogTail {
    file: File,
    identity: (u64, u64),
    position: u64,
    partial: Vec<u8>,
    /// Inside an older or overlong line, dropped up to its end.
    discard_line: bool,
}

impl LogTail {
    /// Starts at the current end of the log at `path`, which must be a readable regular file.
    fn at_end(path: &Path) -> Result<Self, Error> {
        // A substituted FIFO must not block the open before we can reject non-files.
        let mut file = OpenOptions::new()
            .read(true)
            .custom_flags(rustix::fs::OFlags::NONBLOCK.bits() as i32)
            .open(path)
            .map_err(|_| log_refusal("is missing or unreadable"))?;
        let found = file.metadata().map_err(|_| log_refusal("is unreadable"))?;
        if !found.is_file() {
            return Err(log_refusal("is not a file"));
        }
        // A fresh byte offset may be inside an old record. Its later continuation is
        // not a new registration, even if those appended bytes contain a full proof.
        let mut last = *b"\n";
        if found.len() > 0 {
            file.seek(SeekFrom::Start(found.len() - 1))
                .and_then(|_| file.read_exact(&mut last))
                .map_err(|_| log_refusal("is unreadable"))?;
        }
        Ok(Self {
            identity: (found.dev(), found.ino()),
            position: found.len(),
            file,
            partial: Vec::new(),
            discard_line: last[0] != b'\n',
        })
    }

    /// Waits until a complete new line shows a client registered on exactly `socket` with
    /// yolo_mode=false. Fails closed when that line says yolo_mode=true, when `path` stops naming
    /// the file read from or the file shrinks below what was read, when the log cannot be read,
    /// or when no such line arrives within `timeout` or `MAX_LOG_TAIL_BYTES`. Other lines are
    /// dropped as they are read, and no error repeats any log content.
    fn wait_for_registration(
        &mut self,
        path: &Path,
        socket: &str,
        timeout: Duration,
        cancelled: Option<&AtomicBool>,
    ) -> Result<(), Error> {
        let deadline = Instant::now() + timeout;
        let mut inspected = 0;
        loop {
            check_join_cancelled(cancelled)?;
            let now = fs::metadata(path).map_err(|_| log_refusal("disappeared"))?;
            if (now.dev(), now.ino()) != self.identity {
                return Err(log_refusal("was replaced"));
            }
            if now.len() < self.position {
                return Err(log_refusal("was truncated"));
            }
            let pending = now.len() - self.position;
            if pending > 0 {
                let mut piece = Vec::new();
                self.file
                    .seek(SeekFrom::Start(self.position))
                    .and_then(|_| {
                        (&mut self.file)
                            .take(
                                pending
                                    .min(LOG_READ_BYTES)
                                    .min(MAX_LOG_TAIL_BYTES - inspected),
                            )
                            .read_to_end(&mut piece)
                    })
                    .map_err(|_| log_refusal("is unreadable"))?;
                self.position += piece.len() as u64;
                inspected += piece.len() as u64;
                match self.registration_in(&piece, socket) {
                    Some(false) => return Ok(()),
                    Some(true) => return Err(not_registered()),
                    None if inspected >= MAX_LOG_TAIL_BYTES => return Err(not_registered()),
                    None => {}
                }
            }
            if Instant::now() >= deadline {
                return Err(not_registered());
            }
            if pending <= LOG_READ_BYTES {
                std::thread::sleep(Duration::from_millis(50));
            }
        }
    }

    /// Adds `bytes` to the unfinished line and judges each line they complete: the first
    /// registration on `socket` gives its yolo_mode. Every other line is dropped, and an unfinished
    /// line past `MAX_LOG_LINE_BYTES` is dropped up to its end.
    fn registration_in(&mut self, bytes: &[u8], socket: &str) -> Option<bool> {
        let mut rest = bytes;
        while let Some(end) = rest.iter().position(|&byte| byte == b'\n') {
            let fits = !self.discard_line && self.partial.len() + end <= MAX_LOG_LINE_BYTES;
            let verdict = if fits {
                self.partial.extend_from_slice(&rest[..end]);
                registration_yolo(&String::from_utf8_lossy(&self.partial), socket)
            } else {
                None
            };
            self.partial.clear();
            self.discard_line = false;
            if verdict.is_some() {
                return verdict;
            }
            rest = &rest[end + 1..];
        }
        if self.discard_line || self.partial.len() + rest.len() > MAX_LOG_LINE_BYTES {
            self.partial.clear();
            self.discard_line = true;
        } else {
            self.partial.extend_from_slice(rest);
        }
        None
    }
}

/// The gate's refusal when the leader log itself cannot serve as evidence; it names no content.
fn log_refusal(what: &str) -> Error {
    Error::Invalid(format!("the Grok leader log {what}; skipping session/load"))
}

fn not_registered() -> Error {
    Error::Invalid("client is not registered with yolo_mode=false; skipping session/load".into())
}

/// The yolo_mode a leader log line records for the client registered on exactly `socket`; `None`
/// for every other line. Only the leader's own record counts: "Client registered" must open the
/// message, right after the timestamp and the INFO level. The same words inside another message
/// prove nothing, since a client chooses the identifier the leader logs.
fn registration_yolo(line: &str, socket: &str) -> Option<bool> {
    // leader.log, Grok 1.0.70 and 1.0.77:
    // 2026-11-11T03:13:03.960303Z  INFO Client registered client_id=2 client_type=grok --permission-mode default agent --leader --leader-socket /tmp/t5/s3.sock stdio mode=Stdio yolo_mode=false client_version=Some("1.0.77")
    let (_timestamp, rest) = line.trim_end().split_once(char::is_whitespace)?;
    let (level, message) = rest.trim_start().split_once(' ')?;
    if level != "INFO" {
        return None;
    }
    let rest = message.strip_prefix("Client registered ")?;
    let after_id = rest.split_once(" client_type=")?.1;
    let (before_yolo, after_yolo) = after_id.rsplit_once(" yolo_mode=")?;
    let yolo = match after_yolo.split(' ').next()? {
        "false" => false,
        "true" => true,
        _ => return None,
    };
    let client_type = before_yolo.rsplit_once(" mode=")?.0;
    let tokens: Vec<&str> = client_type.split_whitespace().collect();
    let socket_flag = tokens
        .windows(2)
        .any(|pair| pair[0] == "--leader-socket" && pair[1] == socket);
    let mode_flag = tokens.windows(2).any(|pair| pair[0] == "--permission-mode");
    if socket_flag && mode_flag {
        Some(yolo)
    } else {
        None
    }
}

/// Map a Grok ACP notification or server request onto a registry pulse.
/// Permission payloads are ignored; only the waiting state is returned.
pub fn interpret_notification(session_id: &str, msg: &Value) -> Option<StatusEvent> {
    let method = msg["method"].as_str()?;
    let params = msg.get("params").cloned().unwrap_or_else(|| json!({}));
    match method {
        "_x.ai/sessions/changed" => {
            for session in params["upserted"].as_array()? {
                if session["sessionId"].as_str() == Some(session_id) {
                    return Some(match session["activity"].as_str() {
                        Some("working") => StatusEvent::Busy,
                        Some("idle") => StatusEvent::Idle,
                        _ => StatusEvent::Unknown,
                    });
                }
            }
            None
        }
        "_x.ai/session/prompt_complete" if params["sessionId"].as_str() == Some(session_id) => {
            Some(StatusEvent::Idle)
        }
        "session/request_permission" if params["sessionId"].as_str() == Some(session_id) => {
            Some(StatusEvent::WaitingApproval)
        }
        "session/update" if params["sessionId"].as_str() == Some(session_id) => {
            let update = &params["update"];
            if update["sessionUpdate"] == "agent_message_chunk"
                && update["content"]["type"] == "text"
            {
                Some(StatusEvent::Detail(
                    update["content"]["text"].as_str().unwrap_or("").to_string(),
                ))
            } else {
                None
            }
        }
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A client over `cat`, with no reader running, so a test can hold its locks by hand.
    fn idle_client() -> LeaderClient {
        let mut cat = Command::new("cat");
        cat.stdin(Stdio::piped()).stdout(Stdio::null());
        strip_messaging_env(&mut cat);
        let mut child = cat.spawn().unwrap();
        let stdin = child.stdin.take().unwrap();
        let (_, notifications) = mpsc::channel();
        LeaderClient {
            child: Mutex::new(child),
            stdin: Mutex::new(stdin),
            pending: Arc::new(Mutex::new(HashMap::new())),
            notifications: Mutex::new(notifications),
            next_id: Mutex::new(1),
            session_id: "sess-1".into(),
            relay: Mutex::new(()),
            uncertain: Arc::new(Mutex::new(None)),
        }
    }

    // Expectation: a request refused just before its write, because shutdown marked the client
    // unusable after it registered, never holds the unusable flag while it takes the waiter table,
    // and still refuses without writing. Why: the output reader takes the table, then the flag, at
    // end of output; a request taking them the other way round deadlocked with it. Value: shutdown
    // racing a prompt can never wedge the bridge. The request is held at the stdin lock until the
    // flag is set and the table is held, as the reader holds it.
    #[test]
    fn a_refused_request_never_holds_the_unusable_flag_while_taking_the_waiters() {
        let client = Arc::new(idle_client());
        let stdin = client.stdin.lock().unwrap();
        let request = {
            let client = Arc::clone(&client);
            std::thread::spawn(move || {
                client.request("session/prompt", json!({}), Duration::from_secs(5))
            })
        };
        let deadline = Instant::now() + Duration::from_secs(3);
        while client.pending.lock().unwrap().is_empty() {
            assert!(Instant::now() < deadline, "the request never registered");
            std::thread::sleep(Duration::from_millis(5));
        }
        *client.uncertain.lock().unwrap() = Some("client shut down".into());
        let waiters = client.pending.lock().unwrap();
        drop(stdin);
        while client.stdin.try_lock().is_ok() {
            assert!(
                Instant::now() < deadline,
                "the request never reached its write"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        std::thread::sleep(Duration::from_millis(100));
        let flag_free = client.uncertain.try_lock().is_ok();
        drop(waiters);
        let result = request.join().unwrap();
        assert!(
            flag_free,
            "the refused request held the unusable flag while taking the waiters"
        );
        assert!(matches!(result, Err(Error::Invalid(_))), "{result:?}");
        assert!(client.pending.lock().unwrap().is_empty());
    }

    // Expectation: at the end of the client's output the reader never holds the waiter table
    // while it takes the unusable flag, and it still wakes every waiter and marks the client
    // unusable. Why: a request holding the flag while it takes the table, and the reader the
    // other way round, deadlocked. Value: the end of output always wakes every waiter. The flag is
    // held by the test, as a refusing request would hold it.
    #[test]
    fn the_reader_never_holds_the_waiters_while_taking_the_unusable_flag() {
        let pending = Arc::new(Mutex::new(HashMap::new()));
        let uncertain = Arc::new(Mutex::new(None));
        let (waiter, woken) = mpsc::channel();
        pending.lock().unwrap().insert("1".to_string(), waiter);
        let (notifications, _) = mpsc::channel();
        let flag = uncertain.lock().unwrap();
        let reader = {
            let pending = Arc::clone(&pending);
            let uncertain = Arc::clone(&uncertain);
            std::thread::spawn(move || {
                read_replies(std::io::empty(), pending, uncertain, notifications)
            })
        };
        std::thread::sleep(Duration::from_millis(100));
        let waiters_free = pending.try_lock().is_ok();
        drop(flag);
        reader.join().unwrap();
        assert!(
            waiters_free,
            "the reader held the waiters while taking the unusable flag"
        );
        assert!(matches!(
            woken.recv_timeout(Duration::from_secs(1)),
            Ok(RpcReply::Disconnected)
        ));
        assert!(uncertain.lock().unwrap().is_some());
    }

    /// A registration line as Grok's leader writes it to leader.log.
    fn proof_line(socket: &str, yolo: &str) -> String {
        format!(
            "2026-11-11T03:13:03.960303Z  INFO Client registered client_id=2 client_type=grok --permission-mode default agent --leader --leader-socket {socket} stdio mode=Stdio yolo_mode={yolo} client_version=Some(\"1.0.77\")"
        )
    }

    const SOCKET: &str = "/tmp/t5/s3.sock";

    // Cancellation at the registration boundary must return without waiting
    // for the full log deadline, independently of the initialize reply race.
    #[test]
    fn the_registration_gate_observes_startup_cancellation() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("leader.log");
        fs::write(&log, "").unwrap();
        let mut tail = LogTail::at_end(&log).unwrap();
        let cancelled = Arc::new(AtomicBool::new(false));
        let flag = Arc::clone(&cancelled);
        let worker = std::thread::spawn(move || {
            tail.wait_for_registration(&log, SOCKET, Duration::from_secs(5), Some(&flag))
        });
        cancelled.store(true, Ordering::SeqCst);
        let start = Instant::now();
        let error = worker.join().unwrap().unwrap_err();
        assert!(start.elapsed() < Duration::from_secs(1));
        assert!(matches!(error, Error::Invalid(_)), "{error}");
        assert!(error.to_string().contains("cancel"), "{error}");
        assert_eq!(Arc::strong_count(&cancelled), 1);
    }

    fn registration(socket: &str, yolo: &str) -> String {
        format!("{}\n", proof_line(socket, yolo))
    }

    fn append(path: &Path, text: &str) {
        std::fs::OpenOptions::new()
            .append(true)
            .open(path)
            .unwrap()
            .write_all(text.as_bytes())
            .unwrap();
    }

    // Expectation: the gate accepts this client's exact registration written after its starting
    // point, past foreign lines, another socket's line and a line that arrives in two writes, and
    // never a matching line from before its start. Why: the shared log holds every leader's
    // history; only this spawn's fresh line may authorize session/load. Value: the gate trusts
    // only new, exact evidence.
    #[test]
    fn the_gate_accepts_only_this_clients_fresh_registration() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("leader.log");
        std::fs::write(&log, registration(SOCKET, "false")).unwrap();
        let mut tail = LogTail::at_end(&log).unwrap();
        assert!(
            tail.wait_for_registration(&log, SOCKET, Duration::from_millis(200), None)
                .is_err(),
            "a line from before the start authorized load"
        );
        append(&log, "foreign session: token=secret-token-abc\n");
        append(&log, &registration(&format!("{SOCKET}-other"), "false"));
        let line = registration(SOCKET, "false");
        let (head, rest) = line.split_at(40);
        append(&log, head);
        let writer = {
            let log = log.clone();
            let rest = rest.to_string();
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(150));
                append(&log, &rest);
            })
        };
        let accepted = tail.wait_for_registration(&log, SOCKET, Duration::from_secs(3), None);
        writer.join().unwrap();
        assert!(accepted.is_ok(), "{accepted:?}");
    }

    // Expectation: the gate refuses this client's registration that says yolo_mode=true, and
    // fails closed without echoing log content when no registration arrives, when the log is
    // replaced or truncated (noticed at once), and when the log is missing or not a file at the
    // start. Why: only exact fresh evidence of yolo_mode=false may open the session, and the
    // shared log may hold other sessions' secrets. Value: the gate fails closed and quiet.
    #[test]
    fn the_gate_fails_closed_and_quiet() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("leader.log");
        std::fs::write(&log, "").unwrap();
        let mut tail = LogTail::at_end(&log).unwrap();
        append(&log, &registration(SOCKET, "true"));
        assert!(tail
            .wait_for_registration(&log, SOCKET, Duration::from_secs(3), None)
            .is_err());
        let mut tail = LogTail::at_end(&log).unwrap();
        append(&log, "foreign session: token=secret-token-abc\n");
        let quiet = tail
            .wait_for_registration(&log, SOCKET, Duration::from_millis(200), None)
            .unwrap_err()
            .to_string();
        assert!(!quiet.contains("secret-token-abc"), "{quiet}");
        let mut tail = LogTail::at_end(&log).unwrap();
        let replacement = dir.path().join("replacement.log");
        std::fs::write(&replacement, registration(SOCKET, "false")).unwrap();
        std::fs::rename(&replacement, &log).unwrap();
        let started = Instant::now();
        assert!(
            tail.wait_for_registration(&log, SOCKET, Duration::from_secs(3), None)
                .is_err(),
            "a replaced log authorized load"
        );
        assert!(
            started.elapsed() < Duration::from_secs(1),
            "replacement went unnoticed"
        );
        let mut tail = LogTail::at_end(&log).unwrap();
        std::fs::OpenOptions::new()
            .write(true)
            .open(&log)
            .unwrap()
            .set_len(0)
            .unwrap();
        let started = Instant::now();
        assert!(tail
            .wait_for_registration(&log, SOCKET, Duration::from_secs(3), None)
            .is_err());
        assert!(
            started.elapsed() < Duration::from_secs(1),
            "truncation went unnoticed"
        );
        assert!(LogTail::at_end(&dir.path().join("absent.log")).is_err());
        assert!(
            LogTail::at_end(dir.path()).is_err(),
            "a directory is no log"
        );
    }

    // Expectation: the gate keeps at most a bounded unfinished line: a runaway line is dropped
    // as no registration, and a registration after it is still found. Why: the shared log is not
    // ours, so a line without an end must not grow memory. Value: reading stays bounded.
    #[test]
    fn the_gate_bounds_an_unfinished_line() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("leader.log");
        std::fs::write(&log, "").unwrap();
        let mut tail = LogTail::at_end(&log).unwrap();
        let runaway = "y".repeat(3 * MAX_LOG_LINE_BYTES);
        assert_eq!(tail.registration_in(runaway.as_bytes(), SOCKET), None);
        assert!(tail.partial.len() <= MAX_LOG_LINE_BYTES);
        let rest = format!(
            "tail of the runaway line\n{}",
            registration(SOCKET, "false")
        );
        assert_eq!(tail.registration_in(rest.as_bytes(), SOCKET), Some(false));
    }

    // Expectation: the gate gives up, closed, once it has inspected its bound of new log bytes
    // without this client's registration, well before its timeout. Why: a busy shared log must
    // not be read without end. Value: the gate inspects bounded data only.
    #[test]
    fn the_gate_inspects_a_bounded_tail() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("leader.log");
        std::fs::write(&log, "").unwrap();
        let mut tail = LogTail::at_end(&log).unwrap();
        let foreign = "foreign session line\n".repeat(1 + MAX_LOG_TAIL_BYTES as usize / 20);
        append(&log, &foreign);
        append(&log, &registration(SOCKET, "false"));
        let started = Instant::now();
        assert!(tail
            .wait_for_registration(&log, SOCKET, Duration::from_secs(30), None)
            .is_err());
        assert!(started.elapsed() < Duration::from_secs(5));
    }

    // A fresh offset inside an existing record must discard its continuation. Otherwise
    // a partial older record could authorize a new join merely by receiving its final bytes.
    #[test]
    fn the_gate_discards_the_record_already_in_progress_at_its_start() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("leader.log");
        fs::write(&log, "older unfinished record ").unwrap();
        let mut tail = LogTail::at_end(&log).unwrap();
        append(&log, &registration(SOCKET, "false"));
        assert!(
            tail.wait_for_registration(&log, SOCKET, Duration::from_millis(100), None)
                .is_err(),
            "a continuation of an older record authorized load"
        );
        append(&log, &registration(SOCKET, "false"));
        tail.wait_for_registration(&log, SOCKET, Duration::from_secs(1), None)
            .unwrap();
    }

    // The inspection budget is a hard byte boundary, including the proof's newline.
    // Accept a complete proof ending at that boundary, but never read/accept a byte after it.
    #[test]
    fn the_gate_never_accepts_a_registration_past_its_byte_budget() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("leader.log");
        let proof = registration(SOCKET, "false");
        for beyond in [false, true] {
            fs::write(&log, "").unwrap();
            let mut tail = LogTail::at_end(&log).unwrap();
            let padding = MAX_LOG_TAIL_BYTES as usize - proof.len() + usize::from(beyond);
            append(&log, &"\n".repeat(padding));
            append(&log, &proof);
            let result = tail.wait_for_registration(&log, SOCKET, Duration::from_secs(5), None);
            assert_eq!(result.is_ok(), !beyond, "proof past budget: {beyond}");
            assert!(tail.position <= MAX_LOG_TAIL_BYTES, "read past the budget");
        }
    }

    // Opening an arbitrary non-file path must not block before the join timeout starts.
    // Isolate the FIFO opener in a child so the old blocking implementation fails boundedly.
    #[test]
    fn the_gate_refuses_a_fifo_without_waiting_for_a_writer() {
        const COPY: &str = "TASKAI_LOG_FIFO_TEST_PATH";
        if let Some(path) = std::env::var_os(COPY) {
            assert!(LogTail::at_end(Path::new(&path)).is_err());
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        let fifo = dir.path().join("leader.log");
        let mut make_fifo = Command::new("mkfifo");
        make_fifo.arg(&fifo);
        strip_messaging_env(&mut make_fifo);
        assert!(make_fifo.status().unwrap().success());
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args([
                "--exact",
                "grok::leader::tests::the_gate_refuses_a_fifo_without_waiting_for_a_writer",
            ])
            .env(COPY, &fifo)
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        strip_messaging_env(&mut command);
        let mut child = command.spawn().unwrap();
        let deadline = Instant::now() + Duration::from_secs(2);
        let status = loop {
            if let Some(status) = child.try_wait().unwrap() {
                break Some(status);
            }
            if Instant::now() >= deadline {
                child.kill().unwrap();
                child.wait().unwrap();
                break None;
            }
            std::thread::sleep(Duration::from_millis(10));
        };
        assert!(
            status.is_some_and(|status| status.success()),
            "opening a FIFO blocked before the registration gate could refuse it"
        );
    }

    #[test]
    fn registration_matches_proof_line_exact_socket_token() {
        let socket = "/Users/trinath/.grok/leader-taskai-e2e.sock";
        assert_eq!(
            registration_yolo(&proof_line(socket, "false"), socket),
            Some(false)
        );
        assert_eq!(
            registration_yolo(&proof_line(socket, "true"), socket),
            Some(true)
        );
    }

    #[test]
    fn registration_rejects_socket_path_substring() {
        let socket = "/tmp/t5/s3.sock";
        assert_eq!(
            registration_yolo(&proof_line(&format!("{socket}-forged"), "false"), socket),
            None
        );
        assert_eq!(
            registration_yolo(&proof_line(&format!("{socket}extra"), "false"), socket),
            None
        );
        let no_flag = "2026-11-11T03:13:03.960303Z  INFO Client registered client_id=1 client_type=python3 --permission-mode default agent --leader stdio mode=Stdio yolo_mode=false client_version=Some(\"1.0.77\")";
        assert_eq!(registration_yolo(no_flag, socket), None);
    }

    // Expectation: only the leader's own record counts: "Client registered" must open the message,
    // right after the timestamp and the INFO level. The same words inside another message, which a
    // client can cause by choosing its identifier, or on a line with no leader prefix, prove
    // nothing. Why: the gate decides whether a session may be joined without bypassing approvals.
    // Value: the registration is read from what the leader wrote, not from what a client said.
    #[test]
    fn registration_is_read_only_from_the_leaders_own_record() {
        let socket = "/tmp/t5/s3.sock";
        let record = proof_line(socket, "false");
        let message = record.split_once("INFO ").unwrap().1;
        assert_eq!(registration_yolo(&record, socket), Some(false));
        assert_eq!(
            registration_yolo(
                &format!("2026-11-11T03:13:03.962702Z  INFO agent.acp_initialize: Client identifier set to: {message}"),
                socket
            ),
            None
        );
        assert_eq!(registration_yolo(message, socket), None);
        assert_eq!(
            registration_yolo(&record.replace("  INFO ", "  WARN "), socket),
            None
        );
        let unknown = record.replace("yolo_mode=false", "yolo_mode=maybe");
        assert_eq!(registration_yolo(&unknown, socket), None);
    }
}
