// ABOUTME: Codex connector — shared-live: the native TUI and the TaskAI app-server
// ABOUTME: sidecar share one thread store; JSON-RPC over stdio, threadId + turn correlation.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::mpsc::{channel, Receiver, Sender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::claude::parse_version;
use crate::{Capability, Error, Mode};

pub struct CodexConnector {
    pub bin: String,
}

impl Default for CodexConnector {
    fn default() -> Self {
        CodexConnector { bin: "codex".into() }
    }
}

impl CodexConnector {
    /// Harmless handshake: reads the version, never creates a thread.
    pub fn probe(&self) -> Capability {
        match Command::new(&self.bin).arg("--version").output() {
            Ok(out) if out.status.success() => {
                let text = String::from_utf8_lossy(&out.stdout).trim().to_string();
                Capability { mode: Mode::SharedLive, version: parse_version(&text), detail: text }
            }
            Ok(out) => Capability {
                mode: Mode::Unsupported,
                version: None,
                detail: String::from_utf8_lossy(&out.stderr).trim().to_string(),
            },
            Err(e) => Capability { mode: Mode::Unsupported, version: None, detail: e.to_string() },
        }
    }

    /// The exact native CLI the pane spawns.
    pub fn launch_command(&self) -> (String, Vec<String>) {
        (self.bin.clone(), Vec::new())
    }

    /// The TaskAI sidecar: `codex app-server` speaking JSON-RPC on stdio against the
    /// same thread store the visible TUI uses.
    pub fn spawn_app_server(&self) -> Result<AppServer, Error> {
        let mut child = Command::new(&self.bin)
            .arg("app-server")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()?;
        let stdin = child.stdin.take().ok_or_else(|| Error::Invalid("no stdin".into()))?;
        let stdout = child.stdout.take().ok_or_else(|| Error::Invalid("no stdout".into()))?;
        let pending: Arc<Mutex<HashMap<u64, Sender<serde_json::Value>>>> =
            Arc::new(Mutex::new(HashMap::new()));
        let (notif_tx, notif_rx) = channel();
        let p2 = Arc::clone(&pending);
        std::thread::spawn(move || {
            for line in BufReader::new(stdout).lines().map_while(Result::ok) {
                let Ok(v) = serde_json::from_str::<serde_json::Value>(&line) else { continue };
                if let Some(id) = v["id"].as_u64() {
                    if let Some(tx) = p2.lock().unwrap().remove(&id) {
                        let _ = tx.send(v);
                        continue;
                    }
                }
                let _ = notif_tx.send(v);
            }
        });
        Ok(AppServer { child, stdin, pending, notifications: notif_rx, next_id: 1 })
    }
}

/// JSON-RPC client over the sidecar's stdio: correlated requests plus a notification
/// stream (thread/turn events arrive here).
pub struct AppServer {
    child: Child,
    stdin: ChildStdin,
    pending: Arc<Mutex<HashMap<u64, Sender<serde_json::Value>>>>,
    pub notifications: Receiver<serde_json::Value>,
    next_id: u64,
}

impl AppServer {
    pub fn request(
        &mut self,
        method: &str,
        params: serde_json::Value,
        timeout: Duration,
    ) -> Result<serde_json::Value, Error> {
        let id = self.next_id;
        self.next_id += 1;
        let (tx, rx) = channel();
        self.pending.lock().unwrap().insert(id, tx);
        writeln!(
            self.stdin,
            "{}",
            serde_json::json!({"jsonrpc": "2.0", "id": id, "method": method, "params": params})
        )?;
        self.stdin.flush()?;
        let v = rx
            .recv_timeout(timeout)
            .map_err(|_| Error::Timeout(format!("{method}: no response")))?;
        if let Some(err) = v.get("error") {
            return Err(Error::Invalid(format!(
                "{method}: {}",
                err["message"].as_str().unwrap_or("error")
            )));
        }
        Ok(v["result"].clone())
    }

    pub fn initialize(&mut self, timeout: Duration) -> Result<serde_json::Value, Error> {
        let result = self.request(
            "initialize",
            serde_json::json!({"clientInfo": {
                "name": "taskai",
                "title": "TaskAI",
                "version": env!("CARGO_PKG_VERSION"),
            }}),
            timeout,
        )?;
        writeln!(self.stdin, "{}", serde_json::json!({"jsonrpc": "2.0", "method": "initialized"}))?;
        self.stdin.flush()?;
        Ok(result)
    }
}

impl Drop for AppServer {
    fn drop(&mut self) {
        let _ = self.child.kill();
    }
}
