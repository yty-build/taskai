// ABOUTME: One owner of Codex mailbox delivery writes and native activity.
// ABOUTME: Confirmed acceptance releases a slot; uncertainty preserves it for shutdown.

use std::{collections::VecDeque, sync::Arc, time::Duration};

use taskai_connectors::{
    codex::daemon::{CodexActivity, DaemonClient, RelayKind, RelayResult},
    Error,
};
use taskai_core::mail::{Activity, Delivery, DeliveryKind, Mailbox};

pub trait RelayPort {
    fn relay(
        &mut self,
        kind: RelayKind,
        text: &str,
        chain: Option<Vec<String>>,
        timeout: Duration,
    ) -> Result<RelayResult, Error>;
}

impl RelayPort for DaemonClient {
    fn relay(
        &mut self,
        kind: RelayKind,
        text: &str,
        chain: Option<Vec<String>>,
        timeout: Duration,
    ) -> Result<RelayResult, Error> {
        DaemonClient::relay(self, kind, text, chain, timeout)
    }
}

pub struct CodexRelay<P> {
    port: P,
    mailbox: Arc<Mailbox>,
    pending: VecDeque<Delivery>,
    human_first: bool,
}

impl<P: RelayPort> CodexRelay<P> {
    pub fn new(port: P, mailbox: Arc<Mailbox>, human_first: bool) -> Self {
        Self {
            port,
            mailbox,
            pending: VecDeque::new(),
            human_first,
        }
    }

    pub fn port_mut(&mut self) -> &mut P {
        &mut self.port
    }

    pub fn delivery(&mut self, delivery: Delivery, timeout: Duration) -> Result<(), Error> {
        if self.human_first {
            self.pending.push_back(delivery);
            return Ok(());
        }
        self.write_delivery(delivery, timeout)
    }

    pub fn human_turn_accepted(&mut self, timeout: Duration) -> Vec<Result<(), Error>> {
        self.human_first = false;
        self.pending
            .drain(..)
            .collect::<Vec<_>>()
            .into_iter()
            .map(|delivery| self.write_delivery(delivery, timeout))
            .collect()
    }

    fn write_delivery(&mut self, delivery: Delivery, timeout: Duration) -> Result<(), Error> {
        let kind = match delivery.kind {
            DeliveryKind::Message => RelayKind::Message,
            DeliveryKind::Notice | DeliveryKind::Receipt => RelayKind::Notice,
        };
        let chain = (kind == RelayKind::Message)
            .then_some(delivery.chain)
            .flatten();
        // The one owner serializes every relay; the in-flight marker is set
        // immediately before the adapter attempts its provider write.
        self.mailbox.delivery_in_flight(&delivery.id);
        match self.port.relay(kind, &delivery.text, chain, timeout) {
            Ok(_) => {
                self.mailbox.turn_started(&delivery.id);
                Ok(())
            }
            Err(error @ Error::Uncertain(_)) => Err(error),
            Err(error) => {
                self.mailbox
                    .delivery_failed(&delivery.id, &error.to_string());
                Err(error)
            }
        }
    }

    pub fn native_activity(&self, status: CodexActivity) -> Activity {
        let activity = match status {
            CodexActivity::Idle => Activity::Idle,
            CodexActivity::Busy => Activity::Busy,
            CodexActivity::WaitingApproval | CodexActivity::WaitingInput => Activity::Waiting,
        };
        self.mailbox.set_activity(activity, status.waiting_for());
        activity
    }

    pub fn native_detail(&self, detail: &str) {
        self.mailbox.set_detail(detail);
    }
}

#[cfg(test)]
mod tests {
    use std::{
        collections::VecDeque,
        path::Path,
        sync::{mpsc, Arc},
        time::Duration,
    };

    use taskai_connectors::{
        codex::daemon::{CodexActivity, RelayKind, RelayResult},
        Error,
    };
    use taskai_core::{
        mail::{
            Activity, Delivery, Identity, Mailbox, MailboxConfig, PermissionMode, SocketTransport,
        },
        registry::{AddressBook, Registry, Source},
    };
    use tempfile::TempDir;

    use super::{CodexRelay, RelayPort};

    struct FakePort {
        results: VecDeque<Result<RelayResult, Error>>,
        writes: Vec<(RelayKind, Option<Vec<String>>)>,
    }

    impl RelayPort for FakePort {
        fn relay(
            &mut self,
            kind: RelayKind,
            _: &str,
            chain: Option<Vec<String>>,
            _: Duration,
        ) -> Result<RelayResult, Error> {
            self.writes.push((kind, chain));
            self.results.pop_front().unwrap_or(Ok(RelayResult {
                turn_id: "turn".into(),
            }))
        }
    }

    struct Fixture {
        root: TempDir,
        state: std::path::PathBuf,
        task: String,
        recipient: String,
        receiver: Arc<Mailbox>,
        sender: Mailbox,
        deliveries: mpsc::Receiver<Delivery>,
    }

    fn identity(registry: &Registry, session_ref: &str) -> Identity {
        let session = registry.session(session_ref).unwrap();
        Identity {
            session_ref: session_ref.into(),
            task_ref: session.task_ref.clone(),
            name: session.address.clone(),
            harness: "codex".into(),
            workspace: session.workspace.clone(),
            mode: PermissionMode::Prompting,
        }
    }

    fn mailbox(
        home: &Path,
        state: &Path,
        task: &str,
        identity: Identity,
        deliver: impl Fn(Delivery) + Send + Sync + 'static,
    ) -> Mailbox {
        Mailbox::open(
            MailboxConfig {
                identity,
                mail_dir: home.join("run/mail"),
                log_path: state.join("messages").join(format!("{task}.jsonl")),
            },
            Arc::new(AddressBook::new(state, task)),
            Arc::new(SocketTransport),
            deliver,
        )
        .unwrap()
    }

    impl Fixture {
        fn new() -> Self {
            let root = TempDir::new().unwrap();
            let workspace = root.path().join("workspace");
            std::fs::create_dir(&workspace).unwrap();
            let state = root.path().join("state");
            let mut registry = Registry::open(&state).unwrap();
            let task = registry
                .create_task("Task", &workspace, Source::HumanUi)
                .unwrap();
            let receiver_ref = registry
                .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
                .unwrap();
            let sender_ref = registry
                .create_session(
                    &task,
                    Some(&receiver_ref),
                    "Sender",
                    "codex",
                    &workspace,
                    Source::HumanUi,
                )
                .unwrap();
            let recipient = registry.session(&receiver_ref).unwrap().address.clone();
            let (tx, deliveries) = mpsc::channel();
            let receiver = Arc::new(mailbox(
                root.path(),
                &state,
                &task,
                identity(&registry, &receiver_ref),
                move |delivery| {
                    let _ = tx.send(delivery);
                },
            ));
            let sender = mailbox(
                root.path(),
                &state,
                &task,
                identity(&registry, &sender_ref),
                |_| {},
            );
            Self {
                root,
                state,
                task,
                recipient,
                receiver,
                sender,
                deliveries,
            }
        }

        fn send(&self) -> Delivery {
            let result = self
                .sender
                .send(&self.recipient, "private-letter", false, None);
            assert_eq!(result.status, taskai_core::mail::SendStatus::Sent);
            self.deliveries
                .recv_timeout(Duration::from_secs(2))
                .unwrap()
        }

        fn activity(&self) -> Activity {
            let registry = Registry::open(&self.state).unwrap();
            registry
                .live_sessions(&self.task)
                .into_iter()
                .find(|peer| peer.name == self.recipient)
                .unwrap()
                .status
        }
    }

    #[test]
    fn human_first_queues_peer_delivery_and_late_ack_does_not_pin_busy() {
        let fixture = Fixture::new();
        let delivery = fixture.send();
        let port = FakePort {
            results: VecDeque::new(),
            writes: Vec::new(),
        };
        let mut relay = CodexRelay::new(port, Arc::clone(&fixture.receiver), true);
        relay.delivery(delivery, Duration::from_secs(1)).unwrap();
        assert!(relay.port_mut().writes.is_empty());
        relay.native_activity(CodexActivity::Idle);
        assert!(relay
            .human_turn_accepted(Duration::from_secs(1))
            .into_iter()
            .all(|result| result.is_ok()));
        assert_eq!(relay.port_mut().writes.len(), 1);
        assert_eq!(fixture.activity(), Activity::Idle);
        fixture.receiver.close();
        fixture.sender.close();
        let _ = &fixture.root;
    }

    #[test]
    fn uncertain_write_keeps_slot_and_definite_later_refusal_releases_only_new_delivery() {
        let fixture = Fixture::new();
        let first = fixture.send();
        let second = fixture.send();
        let port = FakePort {
            results: VecDeque::from([
                Err(Error::Uncertain("lost acknowledgment".into())),
                Err(Error::Invalid("prior acceptance unknown; no write".into())),
            ]),
            writes: Vec::new(),
        };
        let mut relay = CodexRelay::new(port, Arc::clone(&fixture.receiver), false);
        assert!(matches!(
            relay.delivery(first, Duration::from_secs(1)),
            Err(Error::Uncertain(_))
        ));
        assert!(matches!(
            relay.delivery(second, Duration::from_secs(1)),
            Err(Error::Invalid(_))
        ));
        fixture.receiver.close();
        fixture.sender.close();
        let log = std::fs::read_to_string(
            fixture
                .state
                .join("messages")
                .join(format!("{}.jsonl", fixture.task)),
        )
        .unwrap();
        assert!(log.contains("uncertain"));
        assert!(log.contains("delivery failed"));
        assert!(!log.contains("dropped receipt"));
    }
}
