// ABOUTME: Local messaging bridge runtime for exact TaskAI-owned native sessions.
// ABOUTME: A single protocol owner translates mailbox deliveries and provider events.

use std::{
    path::Path,
    sync::{atomic::AtomicBool, Arc},
};

mod claude_mirror;
pub(crate) mod codex_intent;
mod codex_relay;
mod codex_runtime;
pub(crate) mod gate_control;
mod grok_runtime;

pub(crate) use claude_mirror::native_sessions_dir;

use serde_json::{json, Value};
use taskai_connectors::codex::daemon::TaskaiTools;
use taskai_core::mail::Mailbox;
use taskai_core::registry::{ConnectionTarget, Registry};

use crate::tmux::{self, RuntimeStatus};

/// This process lives in fixed detached window 1 of one exact local runtime.
pub fn run(home: &Path, session_ref: &str) -> Result<(), String> {
    if std::env::var("TASKAI_SESSION_REF").ok().as_deref() != Some(session_ref) {
        return Err("bridge session does not match its launch identity".into());
    }
    let registry = Registry::open(&home.join("state")).map_err(|error| error.to_string())?;
    let session = registry
        .session(session_ref)
        .ok_or("bridge has no saved session")?
        .clone();
    let runtime = session
        .runtime
        .as_ref()
        .ok_or("bridge has no saved runtime")?;
    if runtime.target != ConnectionTarget::Local || runtime.session != session_ref {
        return Err("bridge requires an exact local native runtime".into());
    }
    match tmux::inspect_sidecar(runtime, "bridge").map_err(|error| error.to_string())? {
        RuntimeStatus::Live { pid, .. } if pid == std::process::id() => {}
        _ => return Err("bridge is not its exact detached sidecar pane".into()),
    }
    let native = tmux::native_identity(runtime)
        .map_err(|error| error.to_string())?
        .ok_or("bridge has no live native pane")?;
    let stop = Arc::new(AtomicBool::new(false));
    // Destroying the runtime closes this pane's terminal, which arrives as SIGHUP. Both signals
    // ask for the same orderly close, so subscribers hear `exited` and nothing owned is left.
    for signal in [signal_hook::consts::SIGTERM, signal_hook::consts::SIGHUP] {
        signal_hook::flag::register(signal, Arc::clone(&stop))
            .map_err(|error| error.to_string())?;
    }
    match session.harness.as_str() {
        "codex" => codex_runtime::run(home, &session, runtime, native, stop),
        "grok" => grok_runtime::run(home, &session, runtime, native, stop),
        _ => Err("bridge has no local messaging adapter for this harness".into()),
    }
}

pub struct MailboxTools {
    mailbox: Arc<Mailbox>,
}

impl MailboxTools {
    pub fn new(mailbox: Arc<Mailbox>) -> Self {
        Self { mailbox }
    }
}

impl TaskaiTools for MailboxTools {
    fn list_sessions(&mut self) -> Result<Value, String> {
        let sessions = self
            .mailbox
            .list_sessions()
            .map_err(|error| error.to_string())?;
        Ok(json!({"sessions": sessions}))
    }

    fn send_message(
        &mut self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
        chain: Option<&[String]>,
    ) -> Result<Value, String> {
        serde_json::to_value(self.mailbox.send(to, text, notify_when_idle, chain))
            .map_err(|error| error.to_string())
    }
}

#[cfg(test)]
mod tests {
    use std::{path::PathBuf, sync::Arc};

    use taskai_connectors::codex::daemon::TaskaiTools;
    use taskai_core::mail::{
        Activity, Directory, Identity, MailPeer, Mailbox, MailboxConfig, PermissionMode,
        SocketTransport,
    };
    use taskai_core::Error;
    use tempfile::TempDir;

    use super::MailboxTools;

    struct UnreadableDirectory;

    impl Directory for UnreadableDirectory {
        fn live_sessions(&self) -> Result<Vec<MailPeer>, Error> {
            Err(Error::Invalid("registry unreadable".into()))
        }

        fn register(&self, _: &MailPeer) -> Result<(), Error> {
            Ok(())
        }

        fn set_status(&self, _: &str, _: Activity, _: Option<&str>) -> Result<(), Error> {
            Ok(())
        }

        fn unregister(&self, _: &str) -> Result<(), Error> {
            Ok(())
        }
    }

    fn mailbox(temp: &TempDir) -> Arc<Mailbox> {
        Arc::new(
            Mailbox::open(
                MailboxConfig {
                    identity: Identity {
                        session_ref: "s1".into(),
                        task_ref: "t1".into(),
                        name: "agent-one".into(),
                        harness: "codex".into(),
                        workspace: PathBuf::from("/tmp"),
                        mode: PermissionMode::Prompting,
                    },
                    mail_dir: temp.path().join("mail"),
                    log_path: temp.path().join("messages.jsonl"),
                },
                Arc::new(UnreadableDirectory),
                Arc::new(SocketTransport),
                |_| {},
            )
            .unwrap(),
        )
    }

    #[test]
    fn codex_tools_preserve_directory_and_send_errors() {
        let temp = TempDir::new().unwrap();
        let mailbox = mailbox(&temp);
        let mut tools = MailboxTools::new(Arc::clone(&mailbox));
        assert!(tools
            .list_sessions()
            .unwrap_err()
            .contains("registry unreadable"));
        let result = tools.send_message("other", "hello", false, None).unwrap();
        assert_eq!(result["status"], "send-failed");
        assert!(result["error"]
            .as_str()
            .unwrap()
            .contains("registry unreadable"));
        mailbox.close();
    }
}
