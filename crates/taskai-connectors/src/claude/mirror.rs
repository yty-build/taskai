// ABOUTME: A bridge's presence in Claude Code's session folder: its card, its key and its Claude
// ABOUTME: door, bound first and published only once the door's handler can receive.

use std::{
    collections::HashMap,
    ffi::OsString,
    fs,
    io::{BufRead, BufReader, Read},
    net::Shutdown,
    os::unix::{
        ffi::OsStringExt,
        fs::{MetadataExt, PermissionsExt},
        net::UnixStream,
    },
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Arc, Mutex, MutexGuard, PoisonError, RwLock,
    },
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use serde_json::{json, Value};
use taskai_core::{
    listener::Listener,
    mail::{proc_start, Activity, Directory, Frame, FEATURE_NOTIFY_IDLE},
    write_private, Error,
};

use super::{
    drift::{Drift, TESTED_VERSION},
    envelope::uds_address,
    peer::{inbound, key_path, CLAUDE_HARNESS, PEER_PROTOCOL},
};

/// How Claude names the pid space a card's pid belongs to.
#[cfg(target_os = "macos")]
pub const PID_DOMAIN: &str = "darwin";
#[cfg(not(target_os = "macos"))]
pub const PID_DOMAIN: &str = std::env::consts::OS;
/// How long a door connection may sit silent before it is dropped.
const READ_TIMEOUT: Duration = Duration::from_secs(10);
/// Longest line the door reads, as long as a Claude session reads.
const MAX_LINE_BYTES: usize = 1 << 20;

/// Where and as whom a bridge appears in Claude's session folder.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MirrorConfig {
    /// Claude's session folder, `~/.claude/sessions`.
    pub sessions_dir: PathBuf,
    /// Claude's socket folder, `/tmp/cc-socks`: Claude answers only inside it.
    pub socket_dir: PathBuf,
    /// The session's TaskAI address.
    pub name: String,
    pub harness: String,
    /// The provider's own session or thread id.
    pub session_id: String,
    pub cwd: PathBuf,
}

/// What the Claude door hands on. Each call runs inside the door's admission, which
/// `Mirror::close` waits for, so a handler must not close or drop the mirror, nor wait on a thread
/// that does.
pub trait DoorHandler: Send + Sync {
    /// The TaskAI address of the Claude session answering from `from`, if the bridge may hear it.
    fn sender(&self, from: &str) -> Option<String>;
    /// A frame from a known sender, authenticated and translated, for the mailbox.
    fn accept(&self, frame: Frame);
    /// A frame or connection the door did not admit, and why; nothing was answered.
    fn refuse(&self, reason: &str);
}

/// A mirror whose door is bound but whose key and card are not yet published.
pub struct PreparedMirror {
    config: MirrorConfig,
    listener: Listener,
    door: PathBuf,
    address: String,
    token: String,
    pid: u32,
    proc_start: String,
}

/// A bridge's published presence in Claude's session folder: card, key and door.
pub struct Mirror {
    listener: Listener,
    admission: Arc<Admission>,
    address: String,
    card_path: PathBuf,
    key_path: PathBuf,
    key_id: FileId,
    /// The card as last written, and the file that write made.
    card: Mutex<(Value, FileId)>,
    /// Every close caller waits for the same admission drain and owned-file cleanup.
    close_gate: Mutex<()>,
    closed: AtomicBool,
}

/// A file's device and inode, which tell apart two files that held one pathname.
type FileId = (u64, u64);

impl Mirror {
    /// Binds the bridge's Claude door, owner-only, at `<socket_dir>/<pid>.sock`, and publishes
    /// nothing: no Claude session can find or authenticate to it before `publish`. With a failed
    /// `drift`, nothing is bound and its reasons are the error.
    pub fn prepare(config: MirrorConfig, drift: &Drift) -> Result<PreparedMirror, Error> {
        drift.require()?;
        ensure_private_dir(&config.sessions_dir)?;
        ensure_private_dir(&config.socket_dir)?;
        let pid = std::process::id();
        let proc_start = proc_start(pid)
            .ok_or_else(|| Error::Invalid("this process's start time is unreadable".into()))?;
        let door = config.socket_dir.join(format!("{pid}.sock"));
        let listener = Listener::bind(&door)?;
        Ok(PreparedMirror {
            address: uds_address(&door),
            token: random_token()?,
            config,
            listener,
            door,
            pid,
            proc_start,
        })
    }

    /// The door's address, where Claude sessions answer this bridge.
    pub fn address(&self) -> &str {
        &self.address
    }

    /// Shows the bridge's status on its card: waiting keeps what it waits for. Refused when the
    /// card at the path is no longer the one this mirror wrote.
    pub fn set_status(&self, status: Activity, waiting_for: Option<&str>) -> Result<(), Error> {
        let mut card = locked(&self.card);
        if self.closed.load(Ordering::SeqCst) || file_at(&self.card_path) != Some(card.1) {
            return Err(Error::Invalid(format!(
                "{} is no longer this bridge's Claude card",
                self.card_path.display()
            )));
        }
        let now = now_ms();
        let entry = &mut card.0;
        entry["status"] = json!(status);
        entry["statusUpdatedAt"] = json!(now);
        entry["updatedAt"] = json!(now);
        match waiting_for.filter(|_| status == Activity::Waiting) {
            Some(what) => entry["waitingFor"] = json!(what),
            None => {
                entry
                    .as_object_mut()
                    .map(|fields| fields.remove("waitingFor"));
            }
        }
        card.1 = write_owned(&self.card_path, &card.0)?;
        Ok(())
    }

    /// Shuts the door's admission first, so no frame reaches the handler once this returns, and
    /// ends the connections it accepted; then removes the card and the key while they are still
    /// this mirror's, and closes the door, whatever its pathname now holds. Closing again does
    /// nothing once any in-progress close has completed.
    pub fn close(&self) {
        let _closing = locked(&self.close_gate);
        if self.closed.swap(true, Ordering::SeqCst) {
            return;
        }
        self.admission.shut();
        remove_if_owned(&self.card_path, locked(&self.card).1);
        remove_if_owned(&self.key_path, self.key_id);
        self.listener.close();
    }
}

impl Drop for Mirror {
    fn drop(&mut self) {
        self.close();
    }
}

impl PreparedMirror {
    /// The door's address, for the transport that sends with this bridge as the reply address.
    pub fn address(&self) -> &str {
        &self.address
    }

    /// Starts the door with `handler`, then writes the key and the card, in that order, so a
    /// Claude session can only reach the door once it is served. A peer that connected while the
    /// mirror was only prepared waits in the backlog and is served now. When publishing fails, the
    /// door is shut as at close and nothing stays behind.
    pub fn publish(self, handler: Arc<dyn DoorHandler>) -> Result<Mirror, Error> {
        let PreparedMirror {
            config,
            listener,
            door,
            address,
            token,
            pid,
            proc_start,
        } = self;
        let admission = Arc::new(Admission::new());
        let door_admission = Arc::clone(&admission);
        let door_token = token.clone();
        listener.serve(move |stream| {
            serve_connection(stream, &door_token, handler.as_ref(), &door_admission);
        });
        let key_path = key_path(&config.sessions_dir, pid, &door);
        let key = json!({"peerToken": token, "procStart": proc_start, "pidDomain": PID_DOMAIN});
        let key_id = match write_owned(&key_path, &key) {
            Ok(key_id) => key_id,
            Err(error) => {
                admission.shut();
                return Err(error);
            }
        };
        let now = now_ms();
        let card = json!({
            "pid": pid, "sessionId": config.session_id, "cwd": config.cwd, "startedAt": now,
            "procStart": proc_start, "version": TESTED_VERSION, "peerProtocol": PEER_PROTOCOL,
            "peerFeatures": [FEATURE_NOTIFY_IDLE], "kind": "interactive",
            "entrypoint": format!("taskai-bridge:{}", config.harness), "pidDomain": PID_DOMAIN,
            "messagingSocketPath": door, "name": config.name, "nameSince": now,
            "status": json!(Activity::Idle), "updatedAt": now, "statusUpdatedAt": now
        });
        let card_path = config.sessions_dir.join(format!("{pid}.json"));
        let card_id = match write_owned(&card_path, &card) {
            Ok(card_id) => card_id,
            Err(error) => {
                admission.shut();
                remove_if_owned(&key_path, key_id);
                return Err(error);
            }
        };
        Ok(Mirror {
            listener,
            admission,
            address,
            card_path,
            key_path,
            key_id,
            card: Mutex::new((card, card_id)),
            close_gate: Mutex::new(()),
            closed: AtomicBool::new(false),
        })
    }
}

/// The address of the live Claude session of this task whose mailbox is the socket `from` names,
/// decoding Claude's percent-encoding; any other reply address belongs to no one the bridge hears.
pub fn sender_from_book(book: &dyn Directory, from: &str) -> Result<Option<String>, Error> {
    let Some(socket) = from.strip_prefix("uds:").and_then(percent_decoded) else {
        return Ok(None);
    };
    Ok(book
        .live_sessions()?
        .into_iter()
        .find(|peer| peer.harness == CLAUDE_HARNESS && peer.mailbox == socket)
        .map(|peer| peer.name))
}

/// Whether the door still hands frames on, and the connections it accepted. Each frame is handed
/// on under the read lock that `shut` takes for writing, so once `shut` returns no frame is being
/// handed on, nor will be.
struct Admission {
    open: RwLock<bool>,
    /// Accepted connections being served, by id; `None` once shut, when no more are taken.
    connections: Mutex<Option<HashMap<u64, UnixStream>>>,
    next_id: AtomicU64,
}

/// A connection the door tracks while serving it, until this drops.
struct Tracked<'a> {
    admission: &'a Admission,
    id: u64,
}

impl Admission {
    fn new() -> Self {
        Self {
            open: RwLock::new(true),
            connections: Mutex::new(Some(HashMap::new())),
            next_id: AtomicU64::new(0),
        }
    }

    /// Tracks an accepted connection so that `shut` can end it; `None`, and the connection goes
    /// unserved, once the door is shut or when the connection cannot be tracked.
    fn track(&self, stream: &UnixStream) -> Option<Tracked<'_>> {
        let tracked = stream.try_clone().ok()?;
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        locked(&self.connections).as_mut()?.insert(id, tracked);
        Some(Tracked {
            admission: self,
            id,
        })
    }

    /// Runs `handle` unless the door is shut, and tells whether it ran.
    fn admit(&self, handle: impl FnOnce()) -> bool {
        let open = self.open.read().unwrap_or_else(PoisonError::into_inner);
        if *open {
            handle();
        }
        *open
    }

    /// Ends every accepted connection, so no more frames arrive, and turns later ones away; then
    /// stops admission once any frame being handed on is done.
    fn shut(&self) {
        let connections = locked(&self.connections).take();
        for stream in connections.into_iter().flat_map(HashMap::into_values) {
            let _ = stream.shutdown(Shutdown::Both);
        }
        *self.open.write().unwrap_or_else(PoisonError::into_inner) = false;
    }
}

impl Drop for Tracked<'_> {
    fn drop(&mut self) {
        if let Some(connections) = locked(&self.admission.connections).as_mut() {
            connections.remove(&self.id);
        }
    }
}

/// Serves one door connection while the door admits frames: the auth line first, then frames
/// until the connection ends or the door shuts.
fn serve_connection(
    stream: UnixStream,
    token: &str,
    handler: &dyn DoorHandler,
    admission: &Admission,
) {
    let Some(_tracked) = admission.track(&stream) else {
        return;
    };
    // macOS refuses a timeout on a socket whose writer already finished; its frames are still
    // buffered and reading cannot block, so the refusal is harmless.
    let _ = stream.set_read_timeout(Some(READ_TIMEOUT));
    let mut reader = BufReader::new(stream);
    match read_line(&mut reader) {
        Ok(None) => return,
        Ok(Some(line)) if authenticates(&line, token) => {}
        _ => {
            admission.admit(|| handler.refuse("bad or missing auth line"));
            return;
        }
    }
    loop {
        let line = match read_line(&mut reader) {
            Ok(Some(line)) => line,
            Ok(None) => return,
            Err(reason) => {
                admission.admit(|| handler.refuse(&reason));
                return;
            }
        };
        if line.trim().is_empty() {
            continue;
        }
        if !admission.admit(|| hand_on(&line, handler)) {
            return;
        }
    }
}

/// Hands one authenticated frame on. A frame needs a reply address the handler knows and a
/// translation the core can admit; otherwise it is refused with the reason.
fn hand_on(line: &str, handler: &dyn DoorHandler) {
    let from = serde_json::from_str::<Value>(line).ok().and_then(|frame| {
        frame
            .get("from")
            .and_then(Value::as_str)
            .map(str::to_string)
    });
    let Some(from) = from else {
        handler.refuse("a frame without a reply address");
        return;
    };
    let Some(sender) = handler.sender(&from) else {
        handler.refuse(&format!("{from} is not a live Claude session of this task"));
        return;
    };
    match inbound(line, &sender) {
        Ok(frames) => frames.into_iter().for_each(|frame| handler.accept(frame)),
        Err(reason) => handler.refuse(&reason),
    }
}

/// True when `line` is Claude's auth frame carrying exactly `token`, compared in constant time.
fn authenticates(line: &str, token: &str) -> bool {
    let Ok(frame) = serde_json::from_str::<Value>(line) else {
        return false;
    };
    let offered = frame
        .get("token")
        .and_then(Value::as_str)
        .unwrap_or_default();
    frame.get("type").and_then(Value::as_str) == Some("auth")
        && offered.len() == token.len()
        && offered
            .bytes()
            .zip(token.bytes())
            .fold(0, |differs, (a, b)| differs | (a ^ b))
            == 0
}

/// One line of at most 1 MiB; `None` when the connection ends or goes quiet.
fn read_line(reader: &mut impl BufRead) -> Result<Option<String>, String> {
    let mut line = Vec::new();
    let read = match reader
        .by_ref()
        .take(MAX_LINE_BYTES as u64 + 1)
        .read_until(b'\n', &mut line)
    {
        Ok(read) => read,
        Err(_) => return Ok(None),
    };
    if read == 0 {
        return Ok(None);
    }
    if line.len() > MAX_LINE_BYTES {
        return Err("line longer than 1 MiB".into());
    }
    String::from_utf8(line)
        .map(Some)
        .map_err(|_| "line is not UTF-8".into())
}

/// Bytes of `text` with each `%XX` decoded, as a path; `None` for a malformed escape.
fn percent_decoded(text: &str) -> Option<PathBuf> {
    let raw = text.as_bytes();
    let mut bytes = Vec::with_capacity(raw.len());
    let mut at = 0;
    while at < raw.len() {
        if raw[at] == b'%' {
            let hex = text.get(at + 1..at + 3)?;
            bytes.push(u8::from_str_radix(hex, 16).ok()?);
            at += 3;
        } else {
            bytes.push(raw[at]);
            at += 1;
        }
    }
    Some(PathBuf::from(OsString::from_vec(bytes)))
}

/// Creates `dir` owner-only when it is missing; an existing directory is left as it is.
fn ensure_private_dir(dir: &Path) -> Result<(), Error> {
    if !dir.exists() {
        fs::create_dir_all(dir)?;
        fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}

/// Writes `value` owner-only and atomically, returning the file that write made.
fn write_owned(path: &Path, value: &Value) -> Result<FileId, Error> {
    write_private(path, &value.to_string())?;
    file_at(path).ok_or_else(|| Error::NotFound(path.display().to_string()))
}

fn file_at(path: &Path) -> Option<FileId> {
    fs::symlink_metadata(path)
        .ok()
        .map(|found| (found.dev(), found.ino()))
}

fn remove_if_owned(path: &Path, owned: FileId) {
    if file_at(path) == Some(owned) {
        let _ = fs::remove_file(path);
    }
}

fn random_token() -> Result<String, Error> {
    let mut bytes = [0u8; 16];
    fs::File::open("/dev/urandom")?.read_exact(&mut bytes)?;
    Ok(bytes.iter().map(|byte| format!("{byte:02x}")).collect())
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| {
            u64::try_from(since.as_millis()).unwrap_or(u64::MAX)
        })
}

fn locked<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}
