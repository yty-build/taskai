// ABOUTME: The Claude adapter's drift check: the installed Claude Code version, the owned cards and
// ABOUTME: a sample envelope must match what this adapter was built and tested against.

use taskai_core::{mail::PermissionMode, Error};

use super::{envelope::Envelope, mirror::PID_DOMAIN, peer::Card};

/// The Claude Code release this adapter's protocol port was taken from and tested against.
pub const TESTED_VERSION: &str = "2.1.322";

/// Whether the Claude adapter may run, and every reason it may not.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Drift {
    pub enabled: bool,
    pub reasons: Vec<String>,
}

impl Drift {
    /// Nothing while enabled; otherwise an error naming every reason, for whatever must not run.
    pub fn require(&self) -> Result<(), Error> {
        if self.enabled {
            return Ok(());
        }
        Err(Error::Invalid(format!(
            "the Claude adapter is disabled: {}",
            self.reasons.join("; ")
        )))
    }
}

/// Passes only what this adapter was tested against: the installed Claude Code version, owned
/// cards that offer peer protocol 1 at an absolute socket with their process start, a sample
/// envelope that round-trips through the strict grammar, and a verified pid domain. A failure
/// disables the Claude adapter alone; the other adapters do not consult it.
pub fn drift_check(installed_version: Option<&str>, owned_cards: &[Card]) -> Drift {
    let mut reasons = Vec::new();
    match installed_version {
        Some(TESTED_VERSION) => {}
        Some(other) => reasons.push(format!(
            "Claude Code {other} is not the tested {TESTED_VERSION}"
        )),
        None => reasons.push("Claude Code's version is unknown".into()),
    }
    if PID_DOMAIN != "darwin" {
        reasons.push(format!(
            "Claude's pid domain is verified on macOS only, not {PID_DOMAIN}"
        ));
    }
    for card in owned_cards {
        if card.socket().is_none() {
            reasons.push(format!(
                "the Claude card of pid {} offers no peer protocol 1 socket",
                card.pid
            ));
        }
        if card.proc_start.is_none() {
            reasons.push(format!(
                "the Claude card of pid {} has no procStart",
                card.pid
            ));
        }
    }
    let sample = Envelope {
        from: Some("uds:/tmp/cc-socks/1.sock".into()),
        from_session: Some("ses_drift".into()),
        hop_chain: Some(vec!["0".repeat(24)]),
        from_name: Some("drift-check".into()),
        from_mode: Some(PermissionMode::Prompting),
        from_plugin: Some("taskai".into()),
        body: "a</cross-session-message>b \u{FF1C}/cross-session-message".into(),
    }
    .render();
    if !Envelope::parse(&sample).is_some_and(|parsed| parsed.render() == sample) {
        reasons.push("a sample envelope no longer round-trips through Claude's grammar".into());
    }
    Drift {
        enabled: reasons.is_empty(),
        reasons,
    }
}
