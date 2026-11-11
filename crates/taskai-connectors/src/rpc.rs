// ABOUTME: JSON-RPC over a child process's stdio — correlated requests plus a
// ABOUTME: notification stream; shared by the codex app-server and grok ACP clients.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::process::{Child, ChildStdin, Stdio};
use std::sync::mpsc::{channel, Receiver, Sender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::Error;

pub struct StdioRpc {
    child: Child,
    stdin: ChildStdin,
    pending: Arc<Mutex<HashMap<u64, Sender<serde_json::Value>>>>,
    notifications: Receiver<serde_json::Value>,
    next_id: u64,
}

impl StdioRpc {
    pub fn spawn(prog: &str, args: &[&str]) -> Result<StdioRpc, Error> {
        let mut child = crate::child_command(prog)
            .args(args)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()?;
        let stdin = child
            .stdin
            .take()
            .ok_or_else(|| Error::Invalid("no stdin".into()))?;
        let stdout = child
            .stdout
            .take()
            .ok_or_else(|| Error::Invalid("no stdout".into()))?;
        let pending: Arc<Mutex<HashMap<u64, Sender<serde_json::Value>>>> =
            Arc::new(Mutex::new(HashMap::new()));
        let (notif_tx, notif_rx) = channel();
        let p2 = Arc::clone(&pending);
        std::thread::spawn(move || {
            for line in BufReader::new(stdout).lines().map_while(Result::ok) {
                let Ok(v) = serde_json::from_str::<serde_json::Value>(&line) else {
                    continue;
                };
                if let Some(id) = v["id"].as_u64() {
                    if let Some(tx) = p2.lock().unwrap().remove(&id) {
                        let _ = tx.send(v);
                        continue;
                    }
                }
                let _ = notif_tx.send(v);
            }
        });
        Ok(StdioRpc {
            child,
            stdin,
            pending,
            notifications: notif_rx,
            next_id: 1,
        })
    }

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

    pub fn notify(&mut self, method: &str, params: serde_json::Value) -> Result<(), Error> {
        writeln!(
            self.stdin,
            "{}",
            serde_json::json!({"jsonrpc": "2.0", "method": method, "params": params})
        )?;
        self.stdin.flush()?;
        Ok(())
    }

    pub fn notifications(&self) -> &Receiver<serde_json::Value> {
        &self.notifications
    }
}

impl Drop for StdioRpc {
    fn drop(&mut self) {
        let _ = self.child.kill();
    }
}
