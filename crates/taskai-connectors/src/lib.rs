// ABOUTME: Connector contract, the four harness connectors, and the Conduit turn pipeline —
// ABOUTME: writer exclusion, dispatch correlation, cancel transport, reconciliation.

pub mod claude;
pub mod codex;
pub mod events;
pub mod grok;
pub mod opencode;
pub mod rpc;

use std::collections::HashSet;
use std::fmt;
use std::sync::{Arc, Mutex};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Mode {
    SharedLive,
    OwnedTui,
    HandoffResume,
    Unsupported,
}

impl Mode {
    pub fn label(&self) -> &'static str {
        match self {
            Mode::SharedLive => "shared-live",
            Mode::OwnedTui => "owned-tui",
            Mode::HandoffResume => "handoff-resume",
            Mode::Unsupported => "unsupported",
        }
    }
}

#[derive(Clone, Debug)]
pub struct Capability {
    pub mode: Mode,
    pub version: Option<String>,
    pub detail: String,
}

#[derive(Debug)]
pub enum Error {
    Busy(String),
    Unsupported(String),
    NotAttached(String),
    Timeout(String),
    Io(std::io::Error),
    Invalid(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Busy(m) => write!(f, "busy: another writer is active on {m}"),
            Error::Unsupported(m) => write!(f, "unsupported: {m}"),
            Error::NotAttached(m) => write!(f, "not attached: {m}"),
            Error::Timeout(m) => write!(f, "timeout: {m}"),
            Error::Io(e) => write!(f, "io: {e}"),
            Error::Invalid(m) => write!(f, "invalid: {m}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Self {
        Error::Io(e)
    }
}

/// Writer exclusion: one TaskAI-initiated turn per provider session at a time.
/// The slot frees when the TurnGuard drops — the proven terminal boundary.
pub struct Conduit {
    active: Arc<Mutex<HashSet<String>>>,
}

pub struct TurnGuard {
    active: Arc<Mutex<HashSet<String>>>,
    session_ref: String,
}

impl Conduit {
    pub fn new() -> Conduit {
        Conduit { active: Arc::new(Mutex::new(HashSet::new())) }
    }

    pub fn begin_turn(&self, session_ref: &str) -> Result<TurnGuard, Error> {
        let mut active = self.active.lock().unwrap();
        if !active.insert(session_ref.to_string()) {
            return Err(Error::Busy(session_ref.to_string()));
        }
        Ok(TurnGuard { active: Arc::clone(&self.active), session_ref: session_ref.to_string() })
    }
}

impl Default for Conduit {
    fn default() -> Self {
        Self::new()
    }
}

impl Drop for TurnGuard {
    fn drop(&mut self) {
        self.active.lock().unwrap().remove(&self.session_ref);
    }
}
