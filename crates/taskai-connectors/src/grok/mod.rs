// ABOUTME: Grok connector — handoff-resume plus leader argv, workspace MCP,
// ABOUTME: a session-bound tool socket, and core Mailbox relay and tools.

use std::path::{Path, PathBuf};
use std::time::Duration;

use crate::claude::parse_version;
use crate::rpc::StdioRpc;
use crate::{Capability, Error, Mode};

mod bridge;
mod leader;
mod mcp;
mod mode_evidence;
mod tools;

pub use bridge::{GrokBridge, GrokTools};
pub use leader::{interpret_notification, JoinConfig, LeaderClient, StatusEvent};
pub use mcp::{handle_mcp_message, install_mcp, run_mcp_stdio};
pub use mode_evidence::{
    forward_mode_hook, install_mode_hook, mode_socket_path, ModeEvidence, ModeEvidenceServer,
};
pub use tools::{atomic_write, call_tool, ToolHandler, ToolServer, MAX_TOOL_LINE_BYTES};

/// Mail permission mode as Grok's `--permission-mode` flag. Prompting is Grok
/// `default` (ask). Bypass is `bypassPermissions`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PermissionMode {
    Prompting,
    Bypass,
}

impl PermissionMode {
    pub fn as_cli(self) -> &'static str {
        match self {
            PermissionMode::Prompting => "default",
            PermissionMode::Bypass => "bypassPermissions",
        }
    }
}

/// The log every Grok leader of this user's Grok home writes its client registrations to:
/// `leader.log` in `GROK_HOME`, which must be absolute, else in `.grok` under the home directory,
/// as Grok resolves its home. The log is shared and is only ever read. A relative or empty
/// `GROK_HOME`, or a home directory that is not absolute, is an error, never a guess.
pub fn leader_log_path() -> Result<PathBuf, Error> {
    let grok_home = match std::env::var_os("GROK_HOME") {
        Some(grok_home) => PathBuf::from(grok_home),
        None => std::env::home_dir()
            .ok_or_else(|| Error::Invalid("no home directory for the Grok home".into()))?
            .join(".grok"),
    };
    if !grok_home.is_absolute() {
        return Err(Error::Invalid(
            "the Grok home is not an absolute directory".into(),
        ));
    }
    Ok(grok_home.join("leader.log"))
}

/// Per-session leader socket under TaskAI's run directory.
pub fn leader_socket_path(run_dir: &Path, session_ref: &str) -> PathBuf {
    run_dir.join("grok").join(format!("{session_ref}.sock"))
}

/// Makes the folder a session's leader socket goes in, for its owner only. Grok binds the socket
/// where it is told to and does not create the folder; without it Grok runs with no leader.
pub fn ensure_leader_socket_dir(leader_socket: &Path) -> Result<(), Error> {
    tools::ensure_owner_parent(leader_socket)
}

/// Per-session private tool socket the MCP client dials.
pub fn tool_socket_path(run_dir: &Path, session_ref: &str) -> PathBuf {
    run_dir
        .join("grok")
        .join(format!("{session_ref}.tools.sock"))
}

/// Pane launch environment for this Grok agent. Workspace MCP config is
/// static; the MCP child inherits these values from the pane process.
pub fn pane_env(session_id: &str, tool_socket: &Path) -> Vec<(String, String)> {
    vec![
        ("GROK_SESSION_ID".into(), session_id.into()),
        (
            "TASKAI_TOOL_SOCKET".into(),
            tool_socket.display().to_string(),
        ),
    ]
}

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
        match crate::child_command(&self.bin).arg("--version").output() {
            Ok(out) if out.status.success() => {
                let text = String::from_utf8_lossy(&out.stdout).trim().to_string();
                crate::gate_version(
                    Capability {
                        mode: Mode::HandoffResume,
                        version: parse_version(&text),
                        detail: text,
                    },
                    "1.0.0",
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

    /// The exact native TUI the pane spawns for a fresh session.
    pub fn launch_command(&self) -> (String, Vec<String>) {
        (self.bin.clone(), Vec::new())
    }

    /// Pane argv for a Grok session that owns an isolated leader.
    pub fn pane_command(
        &self,
        leader_socket: &Path,
        session_id: &str,
        mode: PermissionMode,
        model: Option<&str>,
    ) -> (String, Vec<String>) {
        let mut args = vec![
            "--leader".into(),
            "--leader-socket".into(),
            leader_socket.display().to_string(),
            "-s".into(),
            session_id.into(),
            "--permission-mode".into(),
            mode.as_cli().into(),
        ];
        if let Some(model) = model {
            args.extend(["-m".into(), model.into()]);
        }
        (self.bin.clone(), args)
    }

    /// Second-client argv that joins the pane's leader with the same mode.
    pub fn client_command(
        &self,
        leader_socket: &Path,
        mode: PermissionMode,
    ) -> (String, Vec<String>) {
        (
            self.bin.clone(),
            vec![
                "--permission-mode".into(),
                mode.as_cli().into(),
                "agent".into(),
                "--leader".into(),
                "--leader-socket".into(),
                leader_socket.display().to_string(),
                "stdio".into(),
            ],
        )
    }

    /// Exact-ID native resume — how results appear after a handoff.
    pub fn resume_command(&self, session_id: &str) -> (String, Vec<String>) {
        (self.bin.clone(), vec!["--resume".into(), session_id.into()])
    }

    /// The exclusive handoff channel: the ACP agent over stdio.
    pub fn spawn_acp(&self) -> Result<AcpAgent, Error> {
        Ok(AcpAgent {
            rpc: StdioRpc::spawn(&self.bin, &["agent", "stdio"])?,
        })
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
