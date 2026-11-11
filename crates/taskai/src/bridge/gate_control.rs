// ABOUTME: Private first-human gate socket with kernel peer and native pane checks.
// ABOUTME: JSON identity fields are consistency checks; only the OS and registry authorize a turn.

use std::{
    io::{Read, Write},
    os::{fd::AsRawFd, unix::net::UnixStream},
    path::{Path, PathBuf},
    sync::mpsc::{self, Receiver},
};

use serde_json::{json, Value};
use taskai_core::{
    listener::Listener,
    mail::proc_start,
    registry::{ConnectionTarget, Registry, TmuxRuntimeLocator},
};

use crate::{
    gate,
    tmux::{self, NativeIdentity},
};

// serde_json may encode one control byte as six ASCII bytes (\\u00XX).
const MAX_GATE_FRAME: usize = 6 * 65_536 + 1024;

pub struct GateControl {
    listener: Listener,
    accepts: Receiver<UnixStream>,
    current: Option<GateConnection>,
    state_dir: PathBuf,
    session_ref: String,
    runtime: TmuxRuntimeLocator,
    expected: NativeIdentity,
    thread_id: String,
}

struct GateConnection {
    stream: UnixStream,
    bytes: Vec<u8>,
    awaiting_ack: bool,
}

pub struct GateRequest {
    pub text: String,
}

#[derive(Clone, Copy)]
pub enum GateAck {
    Accepted,
    Rejected,
    Unknown,
}

impl GateControl {
    pub fn listen(
        home: &Path,
        session_ref: &str,
        runtime: TmuxRuntimeLocator,
        expected: NativeIdentity,
        thread_id: String,
    ) -> Result<Self, String> {
        let path = gate::socket_path(home, session_ref);
        let listener = Listener::bind(&path).map_err(|error| error.to_string())?;
        let (sender, accepts) = mpsc::channel();
        listener.serve(move |stream| {
            let _ = sender.send(stream);
        });
        Ok(Self {
            listener,
            accepts,
            current: None,
            state_dir: home.join("state"),
            session_ref: session_ref.into(),
            runtime,
            expected,
            thread_id,
        })
    }

    pub fn validate_native(&self) -> Result<(), String> {
        let observed = tmux::native_identity(&self.runtime).map_err(|error| error.to_string())?;
        if observed.as_ref() != Some(&self.expected)
            || proc_start(self.expected.pid).as_deref() != Some(self.expected.proc_start.as_str())
        {
            return Err("Codex gate native pane identity changed".into());
        }
        let registry = Registry::open(&self.state_dir).map_err(|error| error.to_string())?;
        let session = registry
            .session(&self.session_ref)
            .ok_or("Codex gate session disappeared")?;
        if session.harness != "codex"
            || session.runtime.as_ref() != Some(&self.runtime)
            || self.runtime.target != ConnectionTarget::Local
            || session.provider.as_ref().is_none_or(|mapping| {
                mapping.provider != "codex" || mapping.provider_session_id != self.thread_id
            })
        {
            return Err("Codex gate registry identity changed".into());
        }
        Ok(())
    }

    /// Nonblocking drain. A request is returned at most once; a subsequent
    /// frame cannot get to the daemon even if an acknowledgment is lost.
    pub fn poll(&mut self) -> Result<Option<GateRequest>, String> {
        while let Ok(mut stream) = self.accepts.try_recv() {
            if self.current.is_some() || !self.authenticate(&stream) {
                continue;
            }
            self.validate_native()?;
            stream
                .set_nonblocking(true)
                .map_err(|error| error.to_string())?;
            stream
                .write_all(
                    json!({"status":"ready", "threadId":self.thread_id})
                        .to_string()
                        .as_bytes(),
                )
                .and_then(|()| stream.write_all(b"\n"))
                .map_err(|_| "Codex gate readiness write failed".to_string())?;
            self.current = Some(GateConnection {
                stream,
                bytes: Vec::new(),
                awaiting_ack: false,
            });
        }
        let Some(current) = self.current.as_mut() else {
            return Ok(None);
        };
        if current.awaiting_ack {
            return Ok(None);
        }
        let mut buf = [0u8; 4096];
        loop {
            match current.stream.read(&mut buf) {
                Ok(0) => {
                    self.current = None;
                    return Ok(None);
                }
                Ok(count) => {
                    if current.bytes.len() + count > MAX_GATE_FRAME {
                        self.current = None;
                        return Err("Codex gate request is too large".into());
                    }
                    current.bytes.extend_from_slice(&buf[..count]);
                    if let Some(end) = current.bytes.iter().position(|byte| *byte == b'\n') {
                        if end + 1 != current.bytes.len() {
                            self.current = None;
                            return Err("Codex gate sent duplicate frames".into());
                        }
                        let text = parse_request(
                            &current.bytes[..end],
                            &self.session_ref,
                            self.expected.pid,
                            &self.thread_id,
                        )?;
                        current.awaiting_ack = true;
                        return Ok(Some(GateRequest { text }));
                    }
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => return Ok(None),
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(_) => {
                    self.current = None;
                    return Err("Codex gate connection read failed".into());
                }
            }
        }
    }

    pub fn ack(&mut self, ack: GateAck) {
        if let Some(mut current) = self.current.take() {
            let status = match ack {
                GateAck::Accepted => "accepted",
                GateAck::Rejected => "rejected",
                GateAck::Unknown => "unknown",
            };
            let _ = current
                .stream
                .write_all(json!({"status": status}).to_string().as_bytes());
            let _ = current.stream.write_all(b"\n");
        }
    }

    pub fn close(&self) {
        self.listener.close();
    }

    fn authenticate(&self, stream: &UnixStream) -> bool {
        peer_identity(stream).is_ok_and(|(uid, pid)| {
            uid == current_uid()
                && pid == self.expected.pid
                && proc_start(pid).as_deref() == Some(self.expected.proc_start.as_str())
        })
    }
}

fn parse_request(
    bytes: &[u8],
    session_ref: &str,
    gate_pid: u32,
    thread_id: &str,
) -> Result<String, String> {
    let value: Value =
        serde_json::from_slice(bytes).map_err(|_| "Codex gate request is invalid".to_string())?;
    if value["sessionRef"] != session_ref
        || value["pid"] != gate_pid
        || value["threadId"] != thread_id
    {
        return Err("Codex gate request identity differs".into());
    }
    value["text"]
        .as_str()
        .filter(|text| !text.trim().is_empty() && text.len() <= 65_536)
        .map(str::to_string)
        .ok_or_else(|| "Codex gate request has no valid text".into())
}

pub(crate) fn current_uid() -> u32 {
    // SAFETY: geteuid has no pointer arguments or side effects.
    unsafe { libc::geteuid() }
}

#[cfg(target_os = "macos")]
pub(crate) fn peer_identity(stream: &UnixStream) -> Result<(u32, u32), String> {
    let fd = stream.as_raw_fd();
    let mut uid: libc::uid_t = 0;
    let mut gid: libc::gid_t = 0;
    // SAFETY: the two output pointers are valid for the duration of the call.
    if unsafe { libc::getpeereid(fd, &mut uid, &mut gid) } != 0 {
        return Err("Codex gate kernel peer UID is unavailable".into());
    }
    let mut pid: libc::pid_t = 0;
    let mut len = std::mem::size_of::<libc::pid_t>() as libc::socklen_t;
    // SAFETY: pid and len point to initialized writable storage of their declared sizes.
    if unsafe {
        libc::getsockopt(
            fd,
            libc::SOL_LOCAL,
            libc::LOCAL_PEERPID,
            (&mut pid as *mut libc::pid_t).cast(),
            &mut len,
        )
    } != 0
        || len as usize != std::mem::size_of::<libc::pid_t>()
        || pid <= 0
    {
        return Err("Codex gate kernel peer PID is unavailable".into());
    }
    Ok((uid, pid as u32))
}

#[cfg(target_os = "linux")]
pub(crate) fn peer_identity(stream: &UnixStream) -> Result<(u32, u32), String> {
    let fd = stream.as_raw_fd();
    let mut cred = libc::ucred {
        pid: 0,
        uid: 0,
        gid: 0,
    };
    let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
    // SAFETY: cred and len point to initialized writable storage of their declared sizes.
    if unsafe {
        libc::getsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            (&mut cred as *mut libc::ucred).cast(),
            &mut len,
        )
    } != 0
        || len as usize != std::mem::size_of::<libc::ucred>()
        || cred.pid <= 0
    {
        return Err("Codex gate kernel peer identity is unavailable".into());
    }
    Ok((cred.uid, cred.pid as u32))
}

#[cfg(not(any(target_os = "macos", target_os = "linux")))]
pub(crate) fn peer_identity(_: &UnixStream) -> Result<(u32, u32), String> {
    Err("Codex gate kernel peer identity is unsupported".into())
}

#[cfg(test)]
mod tests {
    use std::{os::unix::net::UnixStream, process};

    use super::{current_uid, parse_request, peer_identity, MAX_GATE_FRAME};

    #[test]
    fn unix_peer_identity_comes_from_kernel_not_a_json_claim() {
        let (left, _right) = UnixStream::pair().unwrap();
        let (uid, pid) = peer_identity(&left).unwrap();
        assert_eq!(uid, current_uid());
        assert_eq!(pid, process::id());
    }

    #[test]
    fn near_limit_escaped_human_text_fits_private_gate_frame() {
        let text = "\u{0001}".repeat(65_535);
        let frame = serde_json::json!({
            "sessionRef": "s1", "pid": 42, "threadId": "owned-thread", "text": text
        })
        .to_string();
        assert!(frame.len() <= MAX_GATE_FRAME);
        assert_eq!(
            parse_request(frame.as_bytes(), "s1", 42, "owned-thread").unwrap(),
            text
        );
        let quoted = "\\\"".repeat(32_767);
        let frame = serde_json::json!({
            "sessionRef": "s1", "pid": 42, "threadId": "owned-thread", "text": quoted
        })
        .to_string();
        assert_eq!(
            parse_request(frame.as_bytes(), "s1", 42, "owned-thread").unwrap(),
            quoted
        );
    }
}
