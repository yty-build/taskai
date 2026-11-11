// ABOUTME: Session-bound private tool socket for Grok's MCP client: one JSON
// ABOUTME: line in, one JSON line out; foreign session ids are refused.

use std::collections::HashMap;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Condvar, Mutex, MutexGuard, PoisonError};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use rustix::fs::{flock, FlockOperation, OFlags};
use rustix::io::Errno;
use serde_json::{json, Map, Value};
use taskai_core::listener::Listener;

use crate::Error;

/// Largest JSON line accepted on the tool socket, request or reply.
pub const MAX_TOOL_LINE_BYTES: usize = 1 << 20;

/// Dispatches owned tool calls. The socket never accepts a caller-supplied
/// sender identity, permission mode, or hop chain.
pub trait ToolHandler: Send + Sync + 'static {
    fn list_sessions(&self) -> Result<Value, String>;
    fn send_message(&self, to: &str, text: &str, notify_when_idle: bool) -> Result<Value, String>;
}

/// Private Unix socket the `taskai mcp` process dials. Mode 0600.
///
/// Recovery requires an unlocked claim for the exact socket inode from a cooperating ToolServer,
/// then the core listener's stale-socket checks. A live owner's full backlog can also refuse a
/// connection, so that error alone is never ownership evidence. Unknown/preclaim sockets stay.
/// Closing cancels calls, drains them and removes only this server's socket and claim.
pub struct ToolServer {
    path: PathBuf,
    listener: Option<Listener>,
    claim: SocketClaim,
    calls: Arc<Calls>,
}

impl ToolServer {
    pub fn listen(
        path: &Path,
        session_id: impl Into<String>,
        handler: impl ToolHandler,
    ) -> Result<ToolServer, Error> {
        ensure_owner_parent(path)?;
        let (listener, claim) = {
            let _turn = BindTurn::take(path)?;
            let previous = SocketClaim::for_existing(path)?;
            let listener = Listener::bind(path).map_err(refused)?;
            let claim = SocketClaim::create(path)?;
            if let Some(previous) = previous {
                previous.remove_if_owned();
            }
            (listener, claim)
        };
        let calls = Arc::new(Calls::new());
        let answering = Arc::clone(&calls);
        let session_id = session_id.into();
        let handler = Arc::new(handler);
        listener.serve(move |stream| answering.answer(stream, &session_id, handler.as_ref()));
        Ok(ToolServer {
            path: path.to_path_buf(),
            listener: Some(listener),
            claim,
            calls,
        })
    }
}

impl Drop for ToolServer {
    fn drop(&mut self) {
        self.calls.cancel();
        {
            let _turn = BindTurn::take(&self.path).ok();
            // Drop the listener under the same turn too: its Drop repeats close, and must not
            // race a fresh bind after this server has removed its socket.
            drop(self.listener.take());
            self.claim.remove_if_owned();
        }
        self.calls.wait_until_answered();
    }
}

/// A refusal from the core listener, worded as the tool socket's: an existing path is only ever
/// taken when it is this owner's stale socket.
fn refused(error: taskai_core::Error) -> Error {
    match error {
        taskai_core::Error::Io(error) => Error::Io(error),
        taskai_core::Error::Invalid(reason) => {
            Error::Invalid(format!("tool socket already exists: {reason}"))
        }
        other => Error::Invalid(format!("tool socket refused: {other}")),
    }
}

/// An exclusive turn at binding or closing tool sockets in one directory. Servers for one path
/// take turns, so a later one finds the earlier one's socket live, never takes it for stale, and
/// never unlinks it. The lock is on the directory itself, so no lock file is left behind.
struct BindTurn {
    _dir: File,
}

impl BindTurn {
    fn take(socket: &Path) -> Result<Self, Error> {
        let dir = socket
            .parent()
            .filter(|dir| !dir.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        let dir = File::open(dir)?;
        loop {
            match flock(&dir, FlockOperation::LockExclusive) {
                Ok(()) => return Ok(Self { _dir: dir }),
                Err(Errno::INTR) => {}
                Err(error) => return Err(io::Error::from(error).into()),
            }
        }
    }
}

/// A lifetime flock on an owner-only file named for one socket's device/inode. All cooperating
/// binders/closers hold BindTurn while examining or changing either pathname. This is recovery
/// evidence for TaskAI servers, not an ownership boundary against a hostile same-user process.
struct SocketClaim {
    path: PathBuf,
    file: File,
}

impl SocketClaim {
    fn name(socket: &Path, metadata: &fs::Metadata) -> PathBuf {
        let mut name = socket.as_os_str().to_os_string();
        name.push(format!(".{}.{}.lock", metadata.dev(), metadata.ino()));
        name.into()
    }

    fn create(socket: &Path) -> Result<Self, Error> {
        let socket_metadata = fs::symlink_metadata(socket)?;
        let path = Self::name(socket, &socket_metadata);
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&path)?;
        let claim = Self { path, file };
        if let Err(error) = claim.lock() {
            claim.remove_if_owned();
            return Err(error);
        }
        Ok(claim)
    }

    fn for_existing(socket: &Path) -> Result<Option<Self>, Error> {
        let found = match fs::symlink_metadata(socket) {
            Ok(found) => found,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(error) => return Err(error.into()),
        };
        let owner = fs::metadata(socket.parent().unwrap_or(Path::new(".")))?.uid();
        if !found.file_type().is_socket() || found.uid() != owner {
            return Err(Self::refusal("not an owned socket"));
        }
        let path = Self::name(socket, &found);
        let metadata = fs::symlink_metadata(&path)
            .map_err(|_| Self::refusal("no readable ownership claim"))?;
        if !metadata.is_file()
            || metadata.uid() != owner
            || metadata.mode() & 0o777 != 0o600
            || metadata.nlink() != 1
        {
            return Err(Self::refusal("invalid ownership claim"));
        }
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .custom_flags((OFlags::NOFOLLOW | OFlags::NONBLOCK).bits() as i32)
            .open(&path)
            .map_err(|_| Self::refusal("ownership claim cannot be opened"))?;
        let opened = file.metadata()?;
        if !same_file(&metadata, &opened) {
            return Err(Self::refusal("ownership claim changed"));
        }
        let claim = Self { path, file };
        claim.lock()?;
        if !fs::symlink_metadata(&claim.path).is_ok_and(|now| same_file(&opened, &now))
            || !fs::symlink_metadata(socket).is_ok_and(|now| same_file(&found, &now))
        {
            return Err(Self::refusal("socket or claim changed"));
        }
        Ok(Some(claim))
    }

    fn lock(&self) -> Result<(), Error> {
        loop {
            match flock(&self.file, FlockOperation::NonBlockingLockExclusive) {
                Ok(()) => return Ok(()),
                Err(Errno::INTR) => continue,
                Err(_) => {
                    return Err(Self::refusal(
                        "ownership claim is still held or unavailable",
                    ))
                }
            }
        }
    }

    fn refusal(reason: &str) -> Error {
        Error::Invalid(format!("tool socket already exists: {reason}"))
    }

    fn remove_if_owned(&self) {
        if let (Ok(held), Ok(named)) = (self.file.metadata(), fs::symlink_metadata(&self.path)) {
            if same_file(&held, &named) {
                let _ = fs::remove_file(&self.path);
            }
        }
    }
}

fn same_file(left: &fs::Metadata, right: &fs::Metadata) -> bool {
    left.dev() == right.dev()
        && left.ino() == right.ino()
        && left.uid() == right.uid()
        && left.file_type() == right.file_type()
}

/// The calls a tool server is answering, so closing can cancel each one and wait for all.
struct Calls {
    /// Cleared at cancel; a call reads and dispatches only while it is set.
    running: AtomicBool,
    state: Mutex<CallsState>,
    answered: Condvar,
    next_id: AtomicU64,
}

#[derive(Default)]
struct CallsState {
    cancelled: bool,
    /// A handle on each call's connection, by id, until the call is answered.
    answering: HashMap<u64, UnixStream>,
}

impl Calls {
    fn new() -> Self {
        Self {
            running: AtomicBool::new(true),
            state: Mutex::new(CallsState::default()),
            answered: Condvar::new(),
            next_id: AtomicU64::new(0),
        }
    }

    /// Answers one connection unless the server is closing.
    fn answer(&self, stream: UnixStream, session_id: &str, handler: &dyn ToolHandler) {
        let Some(id) = self.enter(&stream) else {
            return;
        };
        handle_conn(stream, session_id, handler, &self.running);
        let mut state = self.state();
        state.answering.remove(&id);
        self.answered.notify_all();
    }

    fn enter(&self, stream: &UnixStream) -> Option<u64> {
        let handle = stream.try_clone().ok()?;
        let mut state = self.state();
        if state.cancelled {
            return None;
        }
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        state.answering.insert(id, handle);
        Some(id)
    }

    /// Stops every call: none starts after this, and each one being answered is cut off.
    fn cancel(&self) {
        let mut state = self.state();
        state.cancelled = true;
        self.running.store(false, Ordering::SeqCst);
        for stream in state.answering.values() {
            let _ = stream.shutdown(std::net::Shutdown::Both);
        }
    }

    fn wait_until_answered(&self) {
        let mut state = self.state();
        while !state.answering.is_empty() {
            state = self
                .answered
                .wait(state)
                .unwrap_or_else(PoisonError::into_inner);
        }
    }

    fn state(&self) -> MutexGuard<'_, CallsState> {
        self.state.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

pub(super) fn ensure_owner_parent(path: &Path) -> Result<(), Error> {
    let Some(parent) = path.parent() else {
        return Ok(());
    };
    if parent.as_os_str().is_empty() {
        return Ok(());
    }
    fs::create_dir_all(parent)?;
    if is_system_tmpdir(parent) {
        return Ok(());
    }
    fs::set_permissions(parent, fs::Permissions::from_mode(0o700))?;
    Ok(())
}

fn is_system_tmpdir(path: &Path) -> bool {
    matches!(
        path.to_str(),
        Some("/tmp" | "/var/tmp" | "/private/tmp" | "/")
    )
}

fn handle_conn(
    mut stream: UnixStream,
    session_id: &str,
    handler: &dyn ToolHandler,
    running: &AtomicBool,
) {
    let _ = stream.set_read_timeout(Some(Duration::from_secs(10)));
    let _ = stream.set_write_timeout(Some(Duration::from_secs(10)));
    let Ok(buf) = read_delimited_line(&mut stream, MAX_TOOL_LINE_BYTES, running) else {
        return;
    };
    if !running.load(Ordering::SeqCst) {
        return;
    }
    let call: Value = serde_json::from_str(buf.trim_end()).unwrap_or_else(|_| json!({}));
    let reply = dispatch(session_id, handler, &call);
    let _ = writeln!(stream, "{reply}");
}

fn dispatch(session_id: &str, handler: &dyn ToolHandler, call: &Value) -> Value {
    if call["session_id"].as_str() != Some(session_id) {
        return json!({"status": "refused-unknown-session"});
    }
    let args = call.get("args").cloned().unwrap_or_else(|| json!({}));
    match call["tool"].as_str() {
        Some("list_sessions") => {
            if !is_empty_object(&args) {
                return json!({
                    "status": "error",
                    "error": "list_sessions takes an empty object"
                });
            }
            match handler.list_sessions() {
                Ok(sessions) => json!({"status": "ok", "sessions": sessions}),
                Err(error) => json!({"status": "error", "error": error}),
            }
        }
        Some("send_message") => match parse_send_arguments(&args) {
            Some((to, text, notify)) => match handler.send_message(&to, &text, notify) {
                Ok(value) => value,
                Err(error) => json!({"status": "error", "error": error}),
            },
            None => json!({
                "status": "error",
                "error": "invalid send_message arguments"
            }),
        },
        _ => json!({"status": "unknown-tool"}),
    }
}

fn is_empty_object(value: &Value) -> bool {
    value.as_object().is_some_and(Map::is_empty)
}

pub(super) fn parse_send_arguments(args: &Value) -> Option<(String, String, bool)> {
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
        .filter(|value| !value.is_empty())?
        .to_string();
    let text = object.get("text")?.as_str()?.to_string();
    let notify = match object.get("notify_when_idle") {
        Some(value) => value.as_bool()?,
        None => false,
    };
    Some((to, text, notify))
}

/// MCP-side client of ToolServer. A missing bridge is reported, never guessed.
pub fn call_tool(path: &Path, session_id: &str, tool: &str, args: Value) -> Value {
    match call_tool_inner(path, session_id, tool, args) {
        Ok(value) => value,
        Err(_) => json!({"status": "bridge-unavailable"}),
    }
}

fn call_tool_inner(path: &Path, session_id: &str, tool: &str, args: Value) -> Result<Value, Error> {
    let mut stream = UnixStream::connect(path)?;
    stream.set_read_timeout(Some(Duration::from_secs(10)))?;
    stream.set_write_timeout(Some(Duration::from_secs(10)))?;
    writeln!(
        stream,
        "{}",
        json!({"session_id": session_id, "tool": tool, "args": args})
    )?;
    stream.shutdown(std::net::Shutdown::Write)?;
    let buf = read_delimited_line(&mut stream, MAX_TOOL_LINE_BYTES, &AtomicBool::new(true))?;
    Ok(serde_json::from_str(buf.trim_end())
        .unwrap_or_else(|_| json!({"status": "bridge-unavailable"})))
}

fn read_delimited_line(
    read: &mut impl Read,
    max: usize,
    running: &AtomicBool,
) -> Result<String, Error> {
    let mut buf = Vec::new();
    let mut one = [0u8; 1];
    loop {
        if !running.load(Ordering::SeqCst) {
            return Err(Error::Invalid("tool server closed".into()));
        }
        match read.read(&mut one) {
            Ok(0) => {
                return Err(Error::Invalid("unterminated tool line".into()));
            }
            Ok(_) => {
                if one[0] == b'\n' {
                    return Ok(String::from_utf8_lossy(&buf).into_owned());
                }
                if buf.len() >= max {
                    return Err(Error::Invalid("tool line too long".into()));
                }
                buf.push(one[0]);
            }
            Err(err)
                if err.kind() == io::ErrorKind::TimedOut
                    || err.kind() == io::ErrorKind::WouldBlock =>
            {
                return Err(Error::Timeout("tool line".into()));
            }
            Err(err) => return Err(err.into()),
        }
    }
}

/// Owner-only atomic replace of `path`. Used by workspace MCP install.
pub fn atomic_write(path: &Path, contents: &str) -> Result<(), Error> {
    let dir = path.parent().unwrap_or_else(|| Path::new("."));
    let name = path
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("config.toml");
    let (tmp, mut file) = create_unique_staging(dir, name)?;
    let result = (|| {
        file.write_all(contents.as_bytes())?;
        file.set_permissions(fs::Permissions::from_mode(0o600))?;
        file.sync_all()?;
        drop(file);
        fs::set_permissions(&tmp, fs::Permissions::from_mode(0o600))?;
        fs::rename(&tmp, path)?;
        fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    result
}

fn create_unique_staging(dir: &Path, name: &str) -> Result<(PathBuf, File), Error> {
    static SEQ: AtomicU64 = AtomicU64::new(0);
    loop {
        let seq = SEQ.fetch_add(1, Ordering::Relaxed);
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let tmp = dir.join(format!(".{name}.{}.{seq}.{nanos}.tmp", std::process::id()));
        match OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)
        {
            Ok(file) => return Ok((tmp, file)),
            Err(err) if err.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(err) => return Err(err.into()),
        }
    }
}
