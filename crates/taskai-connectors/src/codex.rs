// ABOUTME: Codex capability probe and existing stdio app-server helper.
// ABOUTME: The daemon module owns TaskAI session messaging over the shared Unix WebSocket.

use std::time::Duration;

use crate::claude::parse_version;
use crate::rpc::StdioRpc;
use crate::{Capability, Error, Mode};

pub mod daemon;

pub struct CodexConnector {
    pub bin: String,
}

impl Default for CodexConnector {
    fn default() -> Self {
        CodexConnector {
            bin: "codex".into(),
        }
    }
}

impl CodexConnector {
    /// Harmless handshake: reads the version, never creates a thread.
    pub fn probe(&self) -> Capability {
        match crate::child_command(&self.bin).arg("--version").output() {
            Ok(out) if out.status.success() => {
                let text = String::from_utf8_lossy(&out.stdout).trim().to_string();
                crate::gate_version(
                    Capability {
                        mode: Mode::SharedLive,
                        version: parse_version(&text),
                        detail: text,
                    },
                    "0.100.0",
                )
            }
            Ok(out) => Capability {
                mode: Mode::Unsupported,
                version: None,
                detail: String::from_utf8_lossy(&out.stderr).trim().to_string(),
            },
            Err(e) => Capability {
                mode: Mode::Unsupported,
                version: None,
                detail: e.to_string(),
            },
        }
    }

    /// The exact native CLI the pane spawns.
    pub fn launch_command(&self) -> (String, Vec<String>) {
        (self.bin.clone(), Vec::new())
    }

    /// The TaskAI sidecar: `codex app-server` speaking JSON-RPC on stdio against the
    /// same thread store the visible TUI uses.
    pub fn spawn_app_server(&self) -> Result<AppServer, Error> {
        Ok(AppServer {
            rpc: StdioRpc::spawn(&self.bin, &["app-server"])?,
        })
    }
}

pub struct AppServer {
    rpc: StdioRpc,
}

impl AppServer {
    pub fn request(
        &mut self,
        method: &str,
        params: serde_json::Value,
        timeout: Duration,
    ) -> Result<serde_json::Value, Error> {
        self.rpc.request(method, params, timeout)
    }

    pub fn initialize(&mut self, timeout: Duration) -> Result<serde_json::Value, Error> {
        let result = self.rpc.request(
            "initialize",
            serde_json::json!({"clientInfo": {
                "name": "taskai",
                "title": "TaskAI",
                "version": env!("CARGO_PKG_VERSION"),
            }}),
            timeout,
        )?;
        self.rpc.notify("initialized", serde_json::json!({}))?;
        Ok(result)
    }

    pub fn notifications(&self) -> &std::sync::mpsc::Receiver<serde_json::Value> {
        self.rpc.notifications()
    }
}
