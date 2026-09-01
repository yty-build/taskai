// ABOUTME: Claude Code connector — owned-tui: the one TaskAI-connected native PTY is the
// ABOUTME: session; probe, launch command, hook installation, atomic prompt bytes.

use std::path::{Path, PathBuf};
use std::process::Command;

use crate::{Capability, Error, Mode};

pub struct ClaudeConnector {
    pub bin: String,
}

impl Default for ClaudeConnector {
    fn default() -> Self {
        ClaudeConnector { bin: "claude".into() }
    }
}

impl ClaudeConnector {
    /// Harmless handshake: reads the version, never creates a session.
    pub fn probe(&self) -> Capability {
        match Command::new(&self.bin).arg("--version").output() {
            Ok(out) if out.status.success() => {
                let text = String::from_utf8_lossy(&out.stdout).trim().to_string();
                Capability { mode: Mode::OwnedTui, version: parse_version(&text), detail: text }
            }
            Ok(out) => Capability {
                mode: Mode::Unsupported,
                version: None,
                detail: String::from_utf8_lossy(&out.stderr).trim().to_string(),
            },
            Err(e) => Capability { mode: Mode::Unsupported, version: None, detail: e.to_string() },
        }
    }

    /// The exact native CLI the pane spawns — no flags, no wrapping.
    pub fn launch_command(&self) -> (String, Vec<String>) {
        (self.bin.clone(), Vec::new())
    }

    /// Bracketed-paste bytes that submit one prompt atomically into the idle PTY.
    pub fn prompt_bytes(text: &str) -> Vec<u8> {
        let mut b = Vec::new();
        b.extend_from_slice(b"\x1b[200~");
        b.extend_from_slice(text.as_bytes());
        b.extend_from_slice(b"\x1b[201~");
        b.push(b'\r');
        b
    }
}

pub fn parse_version(s: &str) -> Option<String> {
    for token in s.split_whitespace() {
        let t = token.trim_matches(|c: char| !c.is_ascii_digit() && c != '.');
        if !t.is_empty() && t.chars().next().is_some_and(|c| c.is_ascii_digit()) && t.contains('.') {
            return Some(t.to_string());
        }
    }
    None
}

/// Installs the SessionStart and Stop hooks that append their payloads to the TaskAI
/// events file. Merges into existing settings, never clobbers user content, idempotent.
pub fn install_hooks(workspace: &Path, runtime_dir: &Path) -> Result<PathBuf, Error> {
    std::fs::create_dir_all(runtime_dir)?;
    let events = runtime_dir.join("claude-events.jsonl");
    let cmd = format!("cat >> '{0}' && printf '\\n' >> '{0}'", events.display());
    let dir = workspace.join(".claude");
    std::fs::create_dir_all(&dir)?;
    let path = dir.join("settings.local.json");
    let mut cfg: serde_json::Value = if path.exists() {
        serde_json::from_str(&std::fs::read_to_string(&path)?)
            .map_err(|e| Error::Invalid(format!("{}: {e}", path.display())))?
    } else {
        serde_json::json!({})
    };
    let marker = events.display().to_string();
    for event in ["SessionStart", "Stop"] {
        let mut arr = cfg["hooks"][event].as_array().cloned().unwrap_or_default();
        let already = serde_json::to_string(&arr).unwrap_or_default().contains(&marker);
        if !already {
            arr.push(serde_json::json!({"hooks": [{"type": "command", "command": cmd}]}));
        }
        cfg["hooks"][event] = serde_json::Value::Array(arr);
    }
    let body = serde_json::to_string_pretty(&cfg).map_err(|e| Error::Invalid(e.to_string()))?;
    std::fs::write(&path, body)?;
    Ok(events)
}
