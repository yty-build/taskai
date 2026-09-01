// ABOUTME: Grok connector — handoff-resume: exclusive ACP turn over `grok agent stdio`
// ABOUTME: against the exact session, then native `grok --resume <id>`; never shown live.

use std::process::Command;
use std::time::Duration;

use crate::claude::parse_version;
use crate::rpc::StdioRpc;
use crate::{Capability, Error, Mode};

pub struct GrokConnector {
    pub bin: String,
}

impl Default for GrokConnector {
    fn default() -> Self {
        GrokConnector { bin: "grok".into() }
    }
}

impl GrokConnector {
    /// Harmless handshake: reads the version, never creates a session. Grok is reported
    /// handoff-resume — never live.
    pub fn probe(&self) -> Capability {
        match Command::new(&self.bin).arg("--version").output() {
            Ok(out) if out.status.success() => {
                let text = String::from_utf8_lossy(&out.stdout).trim().to_string();
                crate::gate_version(
                    Capability { mode: Mode::HandoffResume, version: parse_version(&text), detail: text },
                    "1.0.0",
                )
            }
            Ok(out) => Capability {
                mode: Mode::Unsupported,
                version: None,
                detail: String::from_utf8_lossy(&out.stderr).trim().to_string(),
            },
            Err(e) => Capability { mode: Mode::Unsupported, version: None, detail: e.to_string() },
        }
    }

    /// The exact native TUI the pane spawns for a fresh session.
    pub fn launch_command(&self) -> (String, Vec<String>) {
        (self.bin.clone(), Vec::new())
    }

    /// Exact-ID native resume — how results appear after a handoff.
    pub fn resume_command(&self, session_id: &str) -> (String, Vec<String>) {
        (self.bin.clone(), vec!["--resume".into(), session_id.into()])
    }

    /// The exclusive handoff channel: the ACP agent over stdio.
    pub fn spawn_acp(&self) -> Result<AcpAgent, Error> {
        Ok(AcpAgent { rpc: StdioRpc::spawn(&self.bin, &["agent", "stdio"])? })
    }
}

pub struct AcpAgent {
    rpc: StdioRpc,
}

impl AcpAgent {
    /// ACP initialize — the free proof that the handoff channel exists and can load
    /// existing sessions (loadSession capability).
    pub fn initialize(&mut self, timeout: Duration) -> Result<serde_json::Value, Error> {
        self.rpc.request(
            "initialize",
            serde_json::json!({"protocolVersion": 1, "clientCapabilities": {}}),
            timeout,
        )
    }

    pub fn request(
        &mut self,
        method: &str,
        params: serde_json::Value,
        timeout: Duration,
    ) -> Result<serde_json::Value, Error> {
        self.rpc.request(method, params, timeout)
    }

    pub fn notifications(&self) -> &std::sync::mpsc::Receiver<serde_json::Value> {
        self.rpc.notifications()
    }
}
