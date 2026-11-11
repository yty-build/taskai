// ABOUTME: Domain model and Registry — tasks, sessions, stable refs, topology, states,
// ABOUTME: persistence, workspace provisioning, destroy semantics, and session mailboxes.

pub mod listener;
pub mod mail;
pub mod registry;
pub mod workspace;

use std::{
    fmt,
    fs::{self, OpenOptions},
    io::Write,
    os::unix::fs::OpenOptionsExt,
    path::Path,
};

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

/// Writes a file only its owner can read, atomically: through a staging file named for this
/// process, renamed over the target, so no reader sees it half-written.
pub fn write_private(path: &Path, text: &str) -> Result<(), Error> {
    let name = path
        .file_name()
        .map(|name| name.to_string_lossy().into_owned())
        .unwrap_or_default();
    let staged = path.with_file_name(format!(".{name}.{}.tmp", std::process::id()));
    remove_if_present(&staged)?;
    let written = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&staged)
        .and_then(|mut file| file.write_all(text.as_bytes()))
        .and_then(|()| fs::rename(&staged, path));
    if let Err(error) = written {
        let _ = remove_if_present(&staged);
        return Err(error.into());
    }
    Ok(())
}

pub(crate) fn remove_if_present(path: &Path) -> Result<(), Error> {
    match fs::remove_file(path) {
        Err(error) if error.kind() != std::io::ErrorKind::NotFound => Err(error.into()),
        _ => Ok(()),
    }
}

/// The SHA-256 digest of `data` as 64 lowercase hex digits.
pub fn sha256_hex(data: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    Sha256::digest(data)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}
