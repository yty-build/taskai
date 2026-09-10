// ABOUTME: Conduit control server — serves caller-initiated turns over an owner-only
// ABOUTME: unix socket: attributed dispatch, writer exclusion, cancel transport, refusals.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::sync::mpsc::{self, SyncSender};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use crate::claude::ClaudeConnector;
use crate::events::wait_for_event;
use crate::Conduit;
use taskai_pane::Pane;

/// Unix sockets cap paths near 104 bytes; a long TASKAI_HOME falls back to a short
/// hashed /tmp path both the runtime and the client derive identically.
pub fn sock_path(home: &Path) -> PathBuf {
    let direct = home.join("run/control.sock");
    if direct.as_os_str().len() < 100 {
        return direct;
    }
    let mut hash: u64 = 0xcbf2_9ce4_8422_2325;
    for b in home.as_os_str().as_encoded_bytes() {
        hash ^= *b as u64;
        hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
    }
    std::env::temp_dir().join(format!("taskai-{hash:016x}.sock"))
}

/// The attached-session registry the server routes by: one entry per displayed session.
#[derive(Clone)]
pub struct Shared {
    pub panes: Arc<Mutex<HashMap<String, Arc<Mutex<Pane>>>>>,
    pub session_ids: Arc<Mutex<HashMap<String, String>>>,
    pub harnesses: Arc<Mutex<HashMap<String, String>>>,
    pub events: PathBuf,
    pub log: PathBuf,
    pub conduit: Arc<Conduit>,
}

impl Shared {
    /// Correlation-only dispatch log: source, target, event — never prompt content.
    fn note(&self, source: &str, target: &str, event: &str) {
        let ts = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0);
        if let Ok(mut f) = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.log)
        {
            let _ = writeln!(
                f,
                "{}",
                serde_json::json!({"ts": ts, "source": source, "target": target, "event": event})
            );
        }
    }
}

/// A helper may request a reviewable UI form, never mutate the registry here.
pub struct UiRequest {
    pub source: String,
    pub action: String,
    pub argument: Option<String>,
    pub reply: mpsc::Sender<Result<(), String>>,
}

pub fn serve(sock_path: PathBuf, shared: Shared) -> std::io::Result<()> {
    serve_inner(sock_path, shared, None)
}

pub fn serve_with_ui(
    sock_path: PathBuf,
    shared: Shared,
    ui: SyncSender<UiRequest>,
) -> std::io::Result<()> {
    serve_inner(sock_path, shared, Some(ui))
}

fn serve_inner(
    sock_path: PathBuf,
    shared: Shared,
    ui: Option<SyncSender<UiRequest>>,
) -> std::io::Result<()> {
    let _ = std::fs::remove_file(&sock_path);
    let listener = UnixListener::bind(&sock_path)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&sock_path, std::fs::Permissions::from_mode(0o600))?;
    }
    std::thread::spawn(move || {
        for stream in listener.incoming().flatten() {
            let s = shared.clone();
            let ui = ui.clone();
            std::thread::spawn(move || handle(stream, &s, ui.as_ref()));
        }
    });
    Ok(())
}

fn respond(stream: &mut UnixStream, v: serde_json::Value) {
    let _ = writeln!(stream, "{v}");
}

fn err(m: impl Into<String>) -> serde_json::Value {
    serde_json::json!({"ok": false, "error": m.into()})
}

fn handle(mut stream: UnixStream, shared: &Shared, ui: Option<&SyncSender<UiRequest>>) {
    let reader = match stream.try_clone() {
        Ok(s) => s,
        Err(_) => return,
    };
    let mut line = String::new();
    if BufReader::new(reader).read_line(&mut line).is_err() {
        return;
    }
    let req: serde_json::Value = match serde_json::from_str(&line) {
        Ok(v) => v,
        Err(e) => return respond(&mut stream, err(format!("bad request: {e}"))),
    };
    let cmd = req["cmd"].as_str().unwrap_or("");
    let target = req["ref"].as_str().unwrap_or("").to_string();
    let source = req["source"].as_str().unwrap_or("human").to_string();

    if cmd == "helper_ui" {
        let Some(ui) = ui else {
            return respond(
                &mut stream,
                err("this TaskAI viewer does not support helper actions"),
            );
        };
        let action = req["action"].as_str().unwrap_or("");
        if !matches!(
            action,
            "open-settings" | "add-server" | "new-task" | "add-manager" | "add-subordinate"
        ) {
            return respond(
                &mut stream,
                err("unknown helper action; only reviewable setup/team forms are supported"),
            );
        }
        let argument = req["argument"].as_str().map(str::to_owned);
        if source.len() > 64
            || argument
                .as_ref()
                .is_some_and(|s| s.len() > 64 || s.chars().any(char::is_control))
        {
            return respond(&mut stream, err("invalid helper action target"));
        }
        let (reply, receive) = mpsc::channel();
        if ui
            .try_send(UiRequest {
                source,
                action: action.into(),
                argument,
                reply,
            })
            .is_err()
        {
            return respond(
                &mut stream,
                err("TaskAI is busy; retry after reviewing the pending action"),
            );
        }
        match receive.recv_timeout(Duration::from_secs(3)) {
            Ok(Ok(())) => respond(
                &mut stream,
                serde_json::json!({"ok":true,"message":"Request queued, not applied. Ask the user to press Ctrl+G, then F8 to review. No task, agent, or server has been created."}),
            ),
            Ok(Err(error)) => respond(&mut stream, err(error)),
            Err(_) => respond(
                &mut stream,
                err("TaskAI did not accept the request in time; no setup change was applied"),
            ),
        }
        return;
    }

    let Some(pane) = shared.panes.lock().unwrap().get(&target).cloned() else {
        let attached: Vec<String> = shared.panes.lock().unwrap().keys().cloned().collect();
        return respond(
            &mut stream,
            err(format!(
                "session {target} is not attached; attached: {attached:?}"
            )),
        );
    };

    match cmd {
        "cancel" => {
            shared.note(&source, &target, "cancel");
            let mut pane = pane.lock().unwrap();
            match pane.write_bytes(b"\x1b") {
                Ok(()) => respond(
                    &mut stream,
                    serde_json::json!({"ok": true, "event": "cancel"}),
                ),
                Err(e) => respond(&mut stream, err(format!("cancel failed: {e}"))),
            }
        }
        "prompt" => {
            let harness = shared
                .harnesses
                .lock()
                .unwrap()
                .get(&target)
                .cloned()
                .unwrap_or_default();
            if harness != "claude" {
                return respond(
                    &mut stream,
                    err(format!(
                        "conduit prompt currently supports owned-tui (claude) sessions; {target} is {harness}"
                    )),
                );
            }
            let text = req["text"].as_str().unwrap_or("");
            let wait = req["wait"].as_bool().unwrap_or(true);
            let sid = shared.session_ids.lock().unwrap().get(&target).cloned();
            if wait && sid.is_none() {
                return respond(
                    &mut stream,
                    err(format!(
                        "session {target} is starting; provider identity not yet bound"
                    )),
                );
            }
            let _guard = match shared.conduit.begin_turn(&target) {
                Ok(g) => g,
                Err(e) => {
                    shared.note(&source, &target, "busy");
                    return respond(&mut stream, err(e.to_string()));
                }
            };
            let offset = std::fs::metadata(&shared.events)
                .map(|m| m.len())
                .unwrap_or(0);
            shared.note(&source, &target, "dispatch");
            {
                let mut pane = pane.lock().unwrap();
                if let Err(e) = pane.write_bytes(&ClaudeConnector::prompt_bytes(text)) {
                    return respond(&mut stream, err(format!("submit failed: {e}")));
                }
            }
            if !wait {
                return respond(
                    &mut stream,
                    serde_json::json!({"ok": true, "dispatched": true}),
                );
            }
            match wait_for_event(
                &shared.events,
                "Stop",
                sid.as_deref(),
                offset,
                Duration::from_secs(600),
            ) {
                Ok(ev) => {
                    shared.note(&source, &target, "stop");
                    respond(
                        &mut stream,
                        serde_json::json!({"ok": true, "event": "stop", "session_id": ev.session_id}),
                    )
                }
                Err(e) => respond(
                    &mut stream,
                    err(format!(
                        "{e}; dispatch was submitted — reconcile from native history before any resend"
                    )),
                ),
            }
        }
        other => respond(&mut stream, err(format!("unknown cmd {other:?}"))),
    }
}

fn client_call(sock: &Path, req: serde_json::Value) -> Result<serde_json::Value, String> {
    let mut stream = UnixStream::connect(sock)
        .map_err(|e| format!("TaskAI runtime not reachable at {}: {e}", sock.display()))?;
    if req["cmd"] == "helper_ui" {
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .map_err(|e| e.to_string())?;
        stream
            .set_write_timeout(Some(Duration::from_secs(5)))
            .map_err(|e| e.to_string())?;
    }
    writeln!(stream, "{req}").map_err(|e| e.to_string())?;
    let mut line = String::new();
    BufReader::new(stream)
        .read_line(&mut line)
        .map_err(|e| e.to_string())?;
    serde_json::from_str(&line).map_err(|e| format!("bad response {line:?}: {e}"))
}

pub fn client_prompt(
    sock: &Path,
    session_ref: &str,
    text: &str,
    wait: bool,
    source: Option<&str>,
) -> Result<serde_json::Value, String> {
    client_call(
        sock,
        serde_json::json!({
            "cmd": "prompt",
            "ref": session_ref,
            "text": text,
            "wait": wait,
            "source": source.unwrap_or("human"),
        }),
    )
}

pub fn client_cancel(sock: &Path, session_ref: &str) -> Result<serde_json::Value, String> {
    client_call(
        sock,
        serde_json::json!({"cmd": "cancel", "ref": session_ref, "source": "human"}),
    )
}

pub fn client_helper_ui(
    sock: &Path,
    source: &str,
    action: &str,
    argument: Option<&str>,
) -> Result<serde_json::Value, String> {
    client_call(
        sock,
        serde_json::json!({"cmd":"helper_ui", "source":source, "action":action, "argument":argument}),
    )
}
