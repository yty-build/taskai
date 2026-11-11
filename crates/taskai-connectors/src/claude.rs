// ABOUTME: Claude Code connector — owned-tui: the one TaskAI-connected native PTY is the
// ABOUTME: session; probe, launch command, hook installation, atomic prompt bytes.

pub mod drift;
pub mod envelope;
pub mod mirror;
pub mod peer;

use std::path::{Path, PathBuf};

use crate::{Capability, Error, Mode};

pub struct ClaudeConnector {
    pub bin: String,
}

impl Default for ClaudeConnector {
    fn default() -> Self {
        ClaudeConnector {
            bin: std::env::var("TASKAI_CLAUDE_BIN").unwrap_or_else(|_| "claude".into()),
        }
    }
}

impl ClaudeConnector {
    /// Harmless handshake: reads the version, never creates a session.
    pub fn probe(&self) -> Capability {
        match crate::child_command(&self.bin).arg("--version").output() {
            Ok(out) if out.status.success() => {
                let text = String::from_utf8_lossy(&out.stdout).trim().to_string();
                crate::gate_version(
                    Capability {
                        mode: Mode::OwnedTui,
                        version: parse_version(&text),
                        detail: text,
                    },
                    "2.0.0",
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
        if !t.is_empty() && t.chars().next().is_some_and(|c| c.is_ascii_digit()) && t.contains('.')
        {
            return Some(t.to_string());
        }
    }
    None
}

fn load_settings(workspace: &Path) -> Result<(PathBuf, serde_json::Value), Error> {
    let dir = workspace.join(".claude");
    std::fs::create_dir_all(&dir)?;
    let path = dir.join("settings.local.json");
    let cfg = if path.exists() {
        serde_json::from_str(&std::fs::read_to_string(&path)?)
            .map_err(|e| Error::Invalid(format!("{}: {e}", path.display())))?
    } else {
        serde_json::json!({})
    };
    Ok((path, cfg))
}

fn save_settings(path: &Path, cfg: &serde_json::Value) -> Result<(), Error> {
    let body = serde_json::to_string_pretty(cfg).map_err(|e| Error::Invalid(e.to_string()))?;
    std::fs::write(path, body)?;
    Ok(())
}

/// Appends a hook entry for `event` unless one carrying `marker` is already present.
fn upsert_hook(cfg: &mut serde_json::Value, event: &str, cmd: &str, marker: &str) {
    let mut arr = cfg["hooks"][event].as_array().cloned().unwrap_or_default();
    let already = serde_json::to_string(&arr)
        .unwrap_or_default()
        .contains(marker);
    if !already {
        arr.push(serde_json::json!({"hooks": [{"type": "command", "command": cmd}]}));
    }
    cfg["hooks"][event] = serde_json::Value::Array(arr);
}

fn replace_exact_hook(cfg: &mut serde_json::Value, event: &str, old: &str, new: &str) {
    let mut arr = cfg["hooks"][event].as_array().cloned().unwrap_or_default();
    arr.retain(|entry| {
        !entry["hooks"].as_array().is_some_and(|hooks| {
            hooks
                .iter()
                .any(|hook| hook["command"].as_str() == Some(old))
        })
    });
    let already = arr.iter().any(|entry| {
        entry["hooks"].as_array().is_some_and(|hooks| {
            hooks
                .iter()
                .any(|hook| hook["command"].as_str() == Some(new))
        })
    });
    if !already {
        arr.push(serde_json::json!({"hooks": [{"type": "command", "command": new}]}));
    }
    cfg["hooks"][event] = serde_json::Value::Array(arr);
}

fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

/// Session-specific copy of Claude hook events. The shared stream remains the conduit
/// completion source; this copy lets a restarted TaskAI recover a SessionStart that
/// arrived while its viewer was detached.
pub fn session_events_path(runtime_dir: &Path, session_ref: &str) -> PathBuf {
    runtime_dir.join(format!("claude-session-{session_ref}.jsonl"))
}

/// Installs the SessionStart and Stop hooks that append their payloads to the TaskAI
/// events file. Merges into existing settings, never clobbers user content, idempotent.
pub fn install_hooks(workspace: &Path, runtime_dir: &Path) -> Result<PathBuf, Error> {
    std::fs::create_dir_all(runtime_dir)?;
    let events = runtime_dir.join("claude-events.jsonl");
    let legacy = format!("cat >> '{0}' && printf '\\n' >> '{0}'", events.display());
    let events_path = shell_quote(&events.display().to_string());
    let runtime_path = shell_quote(&runtime_dir.display().to_string());
    let cmd = format!(
        "payload=$(cat); printf '%s\\n' \"$payload\" >> {events_path}; if [ -n \"${{TASKAI_SESSION_REF:-}}\" ]; then printf '%s\\n' \"$payload\" >> {runtime_path}/claude-session-\"$TASKAI_SESSION_REF\".jsonl; fi"
    );
    let (path, mut cfg) = load_settings(workspace)?;
    for event in ["SessionStart", "Stop"] {
        replace_exact_hook(&mut cfg, event, &legacy, &cmd);
    }
    save_settings(&path, &cfg)?;
    Ok(events)
}

/// Installs the UserPromptSubmit hook that injects fresh TaskAI Context each user turn —
/// exactly one delivery path per turn (backend §5).
pub fn install_context_hook(workspace: &Path, taskai_bin: &Path) -> Result<(), Error> {
    let cmd = format!("'{}' context inject", taskai_bin.display());
    let (path, mut cfg) = load_settings(workspace)?;
    upsert_hook(&mut cfg, "UserPromptSubmit", &cmd, "context inject");
    save_settings(&path, &cfg)
}
