// ABOUTME: One-use native Grok PreToolUse permission evidence matched to exact send arguments.
// ABOUTME: A private socket carries only digests and metadata; the workspace hook never decides approvals.

use std::{
    collections::HashMap,
    fs::{self, OpenOptions},
    io::{BufRead, BufReader, Read, Write},
    net::Shutdown,
    os::unix::{fs::OpenOptionsExt, net::UnixStream},
    path::{Path, PathBuf},
    sync::{Arc, Condvar, Mutex, MutexGuard, PoisonError},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use taskai_core::{listener::Listener, mail::PermissionMode, sha256_hex};

use super::tools::{atomic_write, ensure_owner_parent, parse_send_arguments};
use crate::Error;

const MAX_HOOK_BYTES: u64 = 1 << 20;
const MAX_FACT_BYTES: usize = 4096;
const MAX_FACTS: usize = 4096;
const MAX_CONNECTIONS: usize = 64;
const FRESH_MS: i64 = 5000;
const IO_TIMEOUT: Duration = Duration::from_millis(500);
const MATCHER: &str = "^taskai__send_message$";
const HOOK_SUFFIX: &str = " grok-mode-hook";

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Fact {
    session_id: String,
    tool_use_id: String,
    arguments_digest: String,
    mode: PermissionMode,
    observed_ms: i64,
}

struct StoredFact {
    digest: String,
    mode: Option<PermissionMode>,
    expires: Instant,
    spent: bool,
}

#[derive(Default)]
struct EvidenceState {
    closed: bool,
    facts: HashMap<String, StoredFact>,
}

/// Evidence for one owned native session. No message bodies or persistent session mode are kept.
/// Replay IDs stay bounded in memory for this bridge lifetime; saturation refuses new evidence.
pub struct ModeEvidence {
    session_id: String,
    state: Mutex<EvidenceState>,
}

impl ModeEvidence {
    fn record(&self, fact: Fact) -> Result<(), String> {
        let age = now_ms().saturating_sub(fact.observed_ms);
        if fact.session_id != self.session_id
            || fact.tool_use_id.is_empty()
            || fact.tool_use_id.len() > 256
            || !(0..FRESH_MS).contains(&age)
            || fact.arguments_digest.len() != 64
            || !fact.arguments_digest.bytes().all(|b| b.is_ascii_hexdigit())
        {
            return Err("Grok native mode evidence is invalid or stale".into());
        }
        let mut state = lock(&self.state);
        if state.closed {
            return Err("Grok mode evidence server is closed".into());
        }
        if let Some(old) = state.facts.get_mut(&fact.tool_use_id) {
            old.spent = true;
            old.mode = None;
            return Err("Grok native mode evidence was repeated".into());
        }
        if state.facts.len() >= MAX_FACTS {
            return Err("Grok mode evidence capacity reached".into());
        }
        let now = Instant::now();
        let mut ambiguous = false;
        for old in state.facts.values_mut() {
            if old.digest == fact.arguments_digest && old.expires > now {
                old.mode = None;
                ambiguous = true;
            }
        }
        state.facts.insert(
            fact.tool_use_id,
            StoredFact {
                digest: fact.arguments_digest,
                mode: (!ambiguous).then_some(fact.mode),
                expires: now + Duration::from_millis((FRESH_MS - age) as u64),
                spent: false,
            },
        );
        if ambiguous {
            Err("Grok native mode evidence is ambiguous".into())
        } else {
            Ok(())
        }
    }

    /// Consumes one fresh exact-argument observation. A mismatch retires pending observations,
    /// so a rewritten call cannot leave its original fact usable by a later call. Repeated
    /// identical arguments inside the freshness window fail conservatively as ambiguous.
    pub fn take_for(
        &self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
    ) -> Result<PermissionMode, String> {
        let digest = arguments_digest(to, text, notify_when_idle);
        let mut state = lock(&self.state);
        if state.closed {
            return Err("Grok mode evidence server is closed".into());
        }
        let now = Instant::now();
        let candidates: Vec<_> = state
            .facts
            .iter()
            .filter(|(_, fact)| fact.digest == digest && !fact.spent && fact.expires > now)
            .map(|(id, _)| id.clone())
            .collect();
        if candidates.len() == 1 {
            let fact = state.facts.get_mut(&candidates[0]).expect("selected fact");
            fact.spent = true;
            if let Some(mode) = fact.mode {
                return Ok(mode);
            }
        }
        for fact in state.facts.values_mut() {
            fact.spent = true;
        }
        Err("Grok native mode evidence is missing, expired or ambiguous for this send".into())
    }

    fn close(&self) {
        let mut state = lock(&self.state);
        state.closed = true;
        state.facts.clear();
    }
}

#[derive(Default)]
struct Connections {
    closed: bool,
    active: usize,
    sockets: Vec<Option<UnixStream>>,
}

#[derive(Default)]
struct Admission {
    state: Mutex<Connections>,
    drained: Condvar,
}

struct Admitted {
    owner: Arc<Admission>,
    index: usize,
}

impl Admission {
    fn enter(self: &Arc<Self>, stream: &UnixStream) -> Option<Admitted> {
        let mut state = lock(&self.state);
        if state.closed || state.active >= MAX_CONNECTIONS {
            return None;
        }
        let socket = stream.try_clone().ok()?;
        let index = state
            .sockets
            .iter()
            .position(Option::is_none)
            .unwrap_or(state.sockets.len());
        if index == state.sockets.len() {
            state.sockets.push(Some(socket));
        } else {
            state.sockets[index] = Some(socket);
        }
        state.active += 1;
        Some(Admitted {
            owner: self.clone(),
            index,
        })
    }

    fn shut(&self) {
        let mut state = lock(&self.state);
        state.closed = true;
        for stream in state.sockets.iter().flatten() {
            let _ = stream.shutdown(Shutdown::Both);
        }
        while state.active != 0 {
            state = self
                .drained
                .wait(state)
                .unwrap_or_else(PoisonError::into_inner);
        }
    }
}

impl Drop for Admitted {
    fn drop(&mut self) {
        let mut state = lock(&self.owner.state);
        state.sockets[self.index] = None;
        state.active -= 1;
        self.owner.drained.notify_all();
    }
}

/// Keep this owner beside the tool server; close it to revoke all outstanding facts and drain
/// accepted connections. It does not own or stop any native Grok process.
pub struct ModeEvidenceServer {
    listener: Listener,
    evidence: Arc<ModeEvidence>,
    admission: Arc<Admission>,
    closing: Mutex<()>,
}

impl ModeEvidenceServer {
    pub fn listen(path: &Path, session_id: impl Into<String>) -> Result<Self, Error> {
        let session_id = session_id.into();
        if session_id.is_empty() || session_id.len() > 256 {
            return Err(Error::Invalid(
                "Grok mode evidence needs an exact session ID".into(),
            ));
        }
        ensure_owner_parent(path)?;
        let listener = Listener::bind(path).map_err(|error| Error::Invalid(error.to_string()))?;
        let evidence = Arc::new(ModeEvidence {
            session_id,
            state: Mutex::default(),
        });
        let admission = Arc::new(Admission::default());
        let calls = admission.clone();
        let facts = evidence.clone();
        listener.serve(move |mut stream| {
            let Some(_admitted) = calls.enter(&stream) else {
                return;
            };
            let _ = stream.set_read_timeout(Some(IO_TIMEOUT));
            let _ = stream.set_write_timeout(Some(IO_TIMEOUT));
            let accepted = read_line(&mut stream, MAX_FACT_BYTES)
                .and_then(|bytes| {
                    serde_json::from_slice::<Fact>(&bytes)
                        .map_err(|_| invalid("invalid mode evidence frame"))
                })
                .and_then(|fact| facts.record(fact).map_err(Error::Invalid))
                .is_ok();
            let _ = writeln!(stream, "{}", json!({"ok": accepted}));
        });
        Ok(Self {
            listener,
            evidence,
            admission,
            closing: Mutex::new(()),
        })
    }

    pub fn evidence(&self) -> Arc<ModeEvidence> {
        self.evidence.clone()
    }

    pub fn close(&self) {
        let _closing = lock(&self.closing);
        self.evidence.close();
        self.admission.shut();
        self.listener.close();
    }
}

impl Drop for ModeEvidenceServer {
    fn drop(&mut self) {
        self.close();
    }
}

pub fn mode_socket_path(run_dir: &Path, session_ref: &str) -> PathBuf {
    run_dir
        .join("grok")
        .join(format!("{session_ref}.mode.sock"))
}

/// Reads a bounded native hook envelope and forwards only its digest/mode metadata. This helper
/// writes no stdout and returns no hook decision. The app must not translate an error into a
/// permission answer; the eventual send fails without fresh evidence.
pub fn forward_mode_hook(reader: impl Read, socket: &Path, session_id: &str) -> Result<(), Error> {
    let mut bytes = Vec::new();
    reader.take(MAX_HOOK_BYTES + 1).read_to_end(&mut bytes)?;
    if bytes.len() as u64 > MAX_HOOK_BYTES {
        return Err(invalid("native hook input too large"));
    }
    let hook: Value =
        serde_json::from_slice(&bytes).map_err(|_| invalid("invalid native hook input"))?;
    if session_id.is_empty()
        || hook["sessionId"].as_str() != Some(session_id)
        || hook["hookEventName"] != "pre_tool_use"
        || hook
            .get("hook_event_name")
            .is_some_and(|event| event != "PreToolUse")
        || hook["toolName"] != "taskai__send_message"
        || hook["toolInputTruncated"] != false
    {
        return Err(invalid("native hook does not prove this TaskAI send"));
    }
    let id = hook["toolUseId"]
        .as_str()
        .filter(|id| !id.is_empty() && id.len() <= 256)
        .ok_or_else(|| invalid("native hook has no exact tool ID"))?;
    let mode = match hook["permissionMode"].as_str() {
        Some("bypassPermissions") => PermissionMode::Bypass,
        Some("default" | "auto" | "plan") => PermissionMode::Prompting,
        _ => return Err(invalid("native hook permission mode is unknown")),
    };
    let observed_ms = hook["timestamp"]
        .as_str()
        .and_then(|value| chrono::DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.timestamp_millis())
        .ok_or_else(|| invalid("native hook timestamp is missing or invalid"))?;
    if !(0..FRESH_MS).contains(&now_ms().saturating_sub(observed_ms)) {
        return Err(invalid("native hook timestamp is stale or in the future"));
    }
    let (to, text, notify) = parse_send_arguments(&hook["toolInput"])
        .ok_or_else(|| invalid("native hook send arguments are not recognized"))?;
    let fact = Fact {
        session_id: session_id.into(),
        tool_use_id: id.into(),
        arguments_digest: arguments_digest(&to, &text, notify),
        mode,
        observed_ms,
    };
    let mut stream = UnixStream::connect(socket)?;
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.set_write_timeout(Some(IO_TIMEOUT))?;
    writeln!(
        stream,
        "{}",
        serde_json::to_string(&fact).expect("fact serialization")
    )?;
    let reply: Value = serde_json::from_slice(&read_line(&mut stream, MAX_FACT_BYTES)?)
        .map_err(|_| invalid("invalid mode evidence acknowledgment"))?;
    if reply == json!({"ok": true}) {
        Ok(())
    } else {
        Err(invalid("native mode evidence was refused"))
    }
}

/// Installs one static workspace hook; per-pane socket/session routing stays in the environment.
/// Unrelated entries survive and repeating installation does not append another TaskAI hook.
pub fn install_mode_hook(workspace: &Path, taskai_bin: &Path) -> Result<(), Error> {
    let mut dir = workspace.to_path_buf();
    for component in [".grok", "hooks"] {
        dir.push(component);
        match fs::symlink_metadata(&dir) {
            Ok(meta) if meta.is_dir() && !meta.file_type().is_symlink() => {}
            Ok(_) => {
                return Err(invalid(
                    "workspace hook directory is not a regular directory",
                ))
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => fs::create_dir(&dir)?,
            Err(error) => return Err(error.into()),
        }
    }
    let path = dir.join("taskai-mail-mode.json");
    let mut doc = match fs::symlink_metadata(&path) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => json!({"hooks": {}}),
        Err(error) => return Err(error.into()),
        Ok(meta) if !meta.is_file() || meta.file_type().is_symlink() => {
            return Err(invalid("workspace hook file is not regular"))
        }
        Ok(_) => {
            let file = OpenOptions::new()
                .read(true)
                .custom_flags(rustix::fs::OFlags::NONBLOCK.bits() as i32)
                .open(&path)?;
            if !file.metadata()?.is_file() {
                return Err(invalid("workspace hook file is not regular"));
            }
            let mut bytes = Vec::new();
            file.take(MAX_HOOK_BYTES + 1).read_to_end(&mut bytes)?;
            if bytes.len() as u64 > MAX_HOOK_BYTES {
                return Err(invalid("workspace hook file too large"));
            }
            serde_json::from_slice(&bytes)
                .map_err(|_| invalid("workspace hook file is not valid JSON"))?
        }
    };
    let object = doc
        .as_object_mut()
        .ok_or_else(|| invalid("workspace hook config is not an object"))?;
    let hooks = object
        .entry("hooks")
        .or_insert_with(|| json!({}))
        .as_object_mut()
        .ok_or_else(|| invalid("workspace hooks is not an object"))?;
    let entries = hooks
        .entry("PreToolUse")
        .or_insert_with(|| json!([]))
        .as_array_mut()
        .ok_or_else(|| invalid("workspace PreToolUse hooks is not an array"))?;
    let executable = taskai_bin
        .to_str()
        .filter(|value| !value.is_empty())
        .ok_or_else(|| invalid("TaskAI hook executable is not a UTF-8 path"))?;
    let command = format!("'{}'{HOOK_SUFFIX}", executable.replace('\'', "'\\''"));
    entries.retain(|entry| {
        !(entry["matcher"] == MATCHER
            && entry["hooks"].as_array().is_some_and(|hooks| {
                hooks.len() == 1
                    && hooks[0]["type"] == "command"
                    && hooks[0]["command"].as_str().is_some_and(|command| {
                        command.starts_with('\'') && command.ends_with(HOOK_SUFFIX)
                    })
            }))
    });
    entries.push(json!({"matcher": MATCHER, "hooks": [{"type": "command", "command": command, "timeout": 1}]}));
    atomic_write(
        &path,
        &(serde_json::to_string_pretty(&doc).expect("hook serialization") + "\n"),
    )
}

fn arguments_digest(to: &str, text: &str, notify: bool) -> String {
    sha256_hex(&serde_json::to_vec(&(to, text, notify)).expect("send arguments serialization"))
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .min(i64::MAX as u128) as i64
}

fn invalid(reason: &str) -> Error {
    Error::Invalid(reason.into())
}
fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

fn read_line(reader: &mut impl Read, max: usize) -> Result<Vec<u8>, Error> {
    let mut bytes = Vec::new();
    BufReader::new(reader.take((max + 1) as u64)).read_until(b'\n', &mut bytes)?;
    if bytes.len() > max || bytes.last() != Some(&b'\n') {
        return Err(invalid("invalid bounded mode evidence line"));
    }
    Ok(bytes)
}
