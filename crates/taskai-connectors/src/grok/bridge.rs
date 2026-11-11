// ABOUTME: Relays Mailbox deliveries through a Grok leader and dispatches
// ABOUTME: list_sessions and send_message onto Mailbox, chaining by native turn.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::Value;
use taskai_core::mail::{Activity, Delivery, DeliveryKind, Mailbox};

use super::leader::{interpret_notification, LeaderClient, StatusEvent};
use super::mode_evidence::ModeEvidence;
use super::tools::ToolHandler;
use crate::Error;

/// Tool side of a Grok session: Mailbox methods plus the active turn's chain.
pub struct GrokTools {
    mailbox: Arc<Mailbox>,
    chain: Arc<Mutex<Option<Vec<String>>>>,
    mode_evidence: Option<Arc<ModeEvidence>>,
}

impl Clone for GrokTools {
    fn clone(&self) -> Self {
        Self {
            mailbox: Arc::clone(&self.mailbox),
            chain: Arc::clone(&self.chain),
            mode_evidence: self.mode_evidence.clone(),
        }
    }
}

impl GrokTools {
    pub fn new(mailbox: Arc<Mailbox>) -> Self {
        Self {
            mailbox,
            chain: Arc::new(Mutex::new(None)),
            mode_evidence: None,
        }
    }

    /// Production Grok tools use a mailbox opened with unknown mode and consume a native
    /// PreToolUse observation for each send. Nothing publishes a lasting mode afterward.
    pub fn with_mode_evidence(mailbox: Arc<Mailbox>, evidence: Arc<ModeEvidence>) -> Self {
        Self {
            mode_evidence: Some(evidence),
            ..Self::new(mailbox)
        }
    }

    pub fn apply_status(&self, event: StatusEvent) {
        match event {
            StatusEvent::Busy => self.mailbox.set_activity(Activity::Busy, None),
            StatusEvent::Idle => self.mailbox.set_activity(Activity::Idle, None),
            StatusEvent::WaitingApproval => self
                .mailbox
                .set_activity(Activity::Waiting, Some("approval")),
            StatusEvent::Detail(text) => self.mailbox.set_detail(&text),
            StatusEvent::Unknown => {}
        }
    }
}

impl ToolHandler for GrokTools {
    fn list_sessions(&self) -> Result<Value, String> {
        match self.mailbox.list_sessions() {
            Ok(sessions) => serde_json::to_value(sessions).map_err(|error| error.to_string()),
            Err(error) => Err(error.to_string()),
        }
    }

    fn send_message(&self, to: &str, text: &str, notify_when_idle: bool) -> Result<Value, String> {
        let chain = self.chain.lock().unwrap();
        let result = match &self.mode_evidence {
            Some(evidence) => self.mailbox.send_with_observed_mode(
                to,
                text,
                notify_when_idle,
                chain.as_deref(),
                evidence.take_for(to, text, notify_when_idle),
            ),
            None => self
                .mailbox
                .send(to, text, notify_when_idle, chain.as_deref()),
        };
        serde_json::to_value(&result).map_err(|error| error.to_string())
    }
}

/// Leader relay plus the tools that share the current delivery's chain.
pub struct GrokBridge {
    tools: GrokTools,
    client: LeaderClient,
}

impl GrokBridge {
    pub fn new(mailbox: Arc<Mailbox>, client: LeaderClient) -> Self {
        Self {
            tools: GrokTools::new(mailbox),
            client,
        }
    }

    /// Uses one-use native hook evidence for outgoing tools; relay/status behavior is unchanged.
    pub fn with_mode_evidence(
        mailbox: Arc<Mailbox>,
        client: LeaderClient,
        evidence: Arc<ModeEvidence>,
    ) -> Self {
        Self {
            tools: GrokTools::with_mode_evidence(mailbox, evidence),
            client,
        }
    }

    pub fn tools(&self) -> GrokTools {
        self.tools.clone()
    }

    /// The next native status of this bridge's own session, reading past other sessions'
    /// notifications and anything that maps to no status; `None` once nothing is waiting. It never
    /// waits on a prompt in flight, and a permission request yields only `WaitingApproval`, never
    /// an answer.
    pub fn poll_status(&self) -> Option<StatusEvent> {
        while let Some(notification) = self.client.poll_notification() {
            if let Some(status) = interpret_notification(self.client.session_id(), &notification) {
                return Some(status);
            }
        }
        None
    }

    /// Stops this bridge's joined Grok client and wakes a prompt in flight at once, without its
    /// relay lock: a written prompt stays uncertain, keeping its delivery's slot and chain, and a
    /// later prompt fails before any write. The native leader and its pane run on. Calling it
    /// again does nothing more.
    pub fn shutdown(&self) {
        self.client.shutdown();
    }

    /// Prompt the leader with `delivery.text`. Completes or fails by `delivery.id`; activity is
    /// left to native status.
    pub fn deliver(&self, delivery: Delivery, timeout: Duration) -> Result<(), Error> {
        let chain = match delivery.kind {
            DeliveryKind::Message => delivery.chain.clone(),
            DeliveryKind::Receipt | DeliveryKind::Notice => None,
        };
        let result = self.client.prompt_with(
            &delivery.text,
            timeout,
            || {
                self.tools.mailbox.delivery_in_flight(&delivery.id);
                *self.tools.chain.lock().unwrap() = chain.clone();
            },
            || {
                *self.tools.chain.lock().unwrap() = None;
            },
        );
        match result {
            Ok(_) => {
                self.tools.mailbox.turn_started(&delivery.id);
                Ok(())
            }
            Err(err) if super::leader::is_uncertain_acceptance(&err) => Err(err),
            Err(err) => {
                self.tools
                    .mailbox
                    .delivery_failed(&delivery.id, &err.to_string());
                Err(err)
            }
        }
    }
}
