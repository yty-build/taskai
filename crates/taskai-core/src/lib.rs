// ABOUTME: Domain model and Registry — tasks, sessions, stable refs, topology, states,
// ABOUTME: persistence, workspace provisioning, and destroy semantics.

pub mod registry;
pub mod workspace;

use std::fmt;

#[derive(Debug)]
pub enum Error {
    Io(std::io::Error),
    Git(String),
    NotFound(String),
    Invalid(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Io(e) => write!(f, "io: {e}"),
            Error::Git(m) => write!(f, "git: {m}"),
            Error::NotFound(m) => write!(f, "not found: {m}"),
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
