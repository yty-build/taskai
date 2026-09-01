// ABOUTME: Control socket — the TaskAI runtime serves prompt requests from the session
// ABOUTME: CLI over an owner-only unix socket; one JSON line request, one line response.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use taskai_connectors::claude::ClaudeConnector;
use taskai_connectors::events::wait_for_event;
use taskai_connectors::Conduit;
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

/// Everything the server needs about the one attached session.
pub struct Served {
    pub session_ref: String,
    pub pane: Arc<Mutex<Pane>>,
    pub events: PathBuf,
    pub session_id: Arc<Mutex<Option<String>>>,
    pub conduit: Arc<Conduit>,
}

pub fn serve(sock_path: PathBuf, served: Served) -> std::io::Result<()> {
    let _ = std::fs::remove_file(&sock_path);
    let listener = UnixListener::bind(&sock_path)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&sock_path, std::fs::Permissions::from_mode(0o600))?;
    }
    let served = Arc::new(served);
    std::thread::spawn(move || {
        for conn in listener.incoming() {
            if let Ok(stream) = conn {
                let s = Arc::clone(&served);
                std::thread::spawn(move || handle(stream, &s));
            }
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

fn handle(mut stream: UnixStream, served: &Served) {
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
    if req["cmd"] != "prompt" {
        return respond(&mut stream, err(format!("unknown cmd {}", req["cmd"])));
    }
    let target = req["ref"].as_str().unwrap_or("");
    if target != served.session_ref {
        return respond(
            &mut stream,
            err(format!("unknown session {target}; attached: {}", served.session_ref)),
        );
    }
    let text = req["text"].as_str().unwrap_or("");
    let wait = req["wait"].as_bool().unwrap_or(true);

    let _guard = match served.conduit.begin_turn(target) {
        Ok(g) => g,
        Err(e) => return respond(&mut stream, err(e.to_string())),
    };
    let offset = std::fs::metadata(&served.events).map(|m| m.len()).unwrap_or(0);
    {
        let mut pane = served.pane.lock().unwrap();
        if let Err(e) = pane.write_bytes(&ClaudeConnector::prompt_bytes(text)) {
            return respond(&mut stream, err(format!("submit failed: {e}")));
        }
    }
    if !wait {
        return respond(&mut stream, serde_json::json!({"ok": true, "dispatched": true}));
    }
    let sid = served.session_id.lock().unwrap().clone();
    match wait_for_event(&served.events, "Stop", sid.as_deref(), offset, Duration::from_secs(600)) {
        Ok(ev) => respond(
            &mut stream,
            serde_json::json!({"ok": true, "event": "stop", "session_id": ev.session_id}),
        ),
        Err(e) => respond(&mut stream, err(e.to_string())),
    }
}

pub fn client_prompt(
    sock: &Path,
    session_ref: &str,
    text: &str,
    wait: bool,
) -> Result<serde_json::Value, String> {
    let mut stream = UnixStream::connect(sock)
        .map_err(|e| format!("TaskAI runtime not reachable at {}: {e}", sock.display()))?;
    writeln!(
        stream,
        "{}",
        serde_json::json!({"cmd": "prompt", "ref": session_ref, "text": text, "wait": wait})
    )
    .map_err(|e| e.to_string())?;
    let mut line = String::new();
    BufReader::new(stream)
        .read_line(&mut line)
        .map_err(|e| e.to_string())?;
    serde_json::from_str(&line).map_err(|e| format!("bad response {line:?}: {e}"))
}
