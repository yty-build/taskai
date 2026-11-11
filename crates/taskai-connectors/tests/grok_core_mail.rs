// ABOUTME: Grok tools operate the production core mailbox through private sockets.
// ABOUTME: These offline proofs protect the adapter/core boundary without provider contact.

use std::sync::{Arc, Mutex};

use taskai_connectors::grok::{GrokTools, ToolHandler};
use taskai_core::mail::{
    Activity, Directory, Identity, MailPeer, Mailbox, MailboxConfig, PermissionMode,
    SocketTransport,
};

#[derive(Default)]
struct AddressBook(Mutex<Vec<MailPeer>>);

impl Directory for AddressBook {
    fn live_sessions(&self) -> Result<Vec<MailPeer>, taskai_core::Error> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn register(&self, entry: &MailPeer) -> Result<(), taskai_core::Error> {
        self.0.lock().unwrap().push(entry.clone());
        Ok(())
    }
    fn set_status(
        &self,
        session_ref: &str,
        status: Activity,
        waiting_for: Option<&str>,
    ) -> Result<(), taskai_core::Error> {
        if let Some(entry) = self
            .0
            .lock()
            .unwrap()
            .iter_mut()
            .find(|e| e.session_ref == session_ref)
        {
            entry.status = status;
            entry.waiting_for = waiting_for.map(str::to_owned);
        }
        Ok(())
    }
    fn unregister(&self, session_ref: &str) -> Result<(), taskai_core::Error> {
        self.0
            .lock()
            .unwrap()
            .retain(|e| e.session_ref != session_ref);
        Ok(())
    }
}

// Expectation: GrokTools accepts the actual core mailbox and reports its address book.
// Why: duplicate adapter types compiled while preventing production wiring.
// Value: a missing peer follows core send semantics rather than a mock's success.
#[test]
fn grok_tools_use_the_core_mailbox() {
    let temp = tempfile::Builder::new()
        .prefix("gcore")
        .tempdir_in("/tmp")
        .unwrap();
    let mailbox = Arc::new(
        Mailbox::open(
            MailboxConfig {
                identity: Identity {
                    session_ref: "s3".into(),
                    task_ref: "t1".into(),
                    name: "grok-lead".into(),
                    harness: "grok".into(),
                    workspace: temp.path().to_owned(),
                    mode: PermissionMode::Prompting,
                },
                mail_dir: temp.path().join("mail"),
                log_path: temp.path().join("messages.jsonl"),
            },
            Arc::new(AddressBook::default()),
            Arc::new(SocketTransport),
            |_| {},
        )
        .unwrap(),
    );
    let tools = GrokTools::new(mailbox);
    assert_eq!(tools.list_sessions().unwrap(), serde_json::json!([]));
    let sent = tools.send_message("codex-missing", "hello", false).unwrap();
    assert_eq!(sent["status"], "no-such-session");
}
