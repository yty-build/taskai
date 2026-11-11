// ABOUTME: Native Codex pane gate for exactly one human-origin first message.
// ABOUTME: Text crosses only an owner-only Unix socket body after kernel peer checks.

use std::{
    fs,
    io::{self, BufRead, Read, Write},
    os::unix::net::UnixStream,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use serde_json::{json, Value};
use taskai_connectors::codex::CodexConnector;
use taskai_core::{
    mail::{proc_start, strip_messaging_env},
    registry::{ConnectionTarget, Registry, Session},
    write_private,
};

use crate::{
    bridge::gate_control::{current_uid, peer_identity},
    tmux::{self, NativeIdentity, RuntimeStatus},
};

const MAX_FIRST_MESSAGE_BYTES: usize = 64 * 1024;
const MAX_CONTROL_BYTES: usize = 4096;
const CONNECT_RETRY: Duration = Duration::from_millis(250);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(30);
const CONTROL_TIMEOUT: Duration = Duration::from_secs(30);

pub fn socket_path(home: &Path, session_ref: &str) -> PathBuf {
    home.join("run")
        .join("codex")
        .join(format!("{session_ref}.gate.sock"))
}

fn identity_path(home: &Path, session_ref: &str) -> PathBuf {
    home.join("run")
        .join("codex")
        .join(format!("{session_ref}.gate.json"))
}

pub fn wait_for_identity(
    home: &Path,
    session_ref: &str,
    native: &NativeIdentity,
    timeout: Duration,
) -> Result<(), String> {
    let path = identity_path(home, session_ref);
    let deadline = std::time::Instant::now() + timeout;
    loop {
        match fs::read(&path) {
            Ok(bytes) if bytes.len() <= MAX_CONTROL_BYTES => {
                let value: Value = serde_json::from_slice(&bytes)
                    .map_err(|_| "Codex gate identity record is invalid".to_string())?;
                if value["sessionRef"] == session_ref
                    && value["paneId"] == native.pane_id
                    && value["pid"] == native.pid
                    && value["procStart"] == native.proc_start
                {
                    return Ok(());
                }
            }
            Ok(_) => return Err("Codex gate identity record is too large".into()),
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.to_string()),
        }
        if std::time::Instant::now() >= deadline {
            return Err("Codex native gate did not publish its exact identity".into());
        }
        thread::sleep(Duration::from_millis(50));
    }
}

fn child_path(home: &Path, session_ref: &str) -> PathBuf {
    home.join("run")
        .join("codex")
        .join(format!("{session_ref}.provider.json"))
}

pub fn run(home: &Path, session_ref: &str) -> Result<(), String> {
    if std::env::var("TASKAI_SESSION_REF").ok().as_deref() != Some(session_ref) {
        return Err("Codex gate session does not match launch identity".into());
    }
    let state = home.join("state");
    let registry = Registry::open(&state).map_err(|error| error.to_string())?;
    let session = registry
        .session(session_ref)
        .ok_or("Codex gate has no saved session")?;
    let runtime = session
        .runtime
        .as_ref()
        .ok_or("Codex gate has no saved runtime")?;
    if session.harness != "codex"
        || runtime.target != ConnectionTarget::Local
        || runtime.session != session_ref
    {
        return Err("Codex gate requires its exact local native session".into());
    }
    let native = tmux::native_identity(runtime)
        .map_err(|error| error.to_string())?
        .ok_or("Codex gate has no native pane")?;
    if native.pid != std::process::id()
        || proc_start(native.pid).as_deref() != Some(native.proc_start.as_str())
    {
        return Err("Codex gate is not the exact native pane process".into());
    }
    let identity = identity_path(home, session_ref);
    fs::create_dir_all(identity.parent().unwrap()).map_err(|error| error.to_string())?;
    write_private(
        &identity,
        &json!({
            "sessionRef": session_ref,
            "paneId": native.pane_id,
            "pid": native.pid,
            "procStart": native.proc_start,
        })
        .to_string(),
    )
    .map_err(|error| error.to_string())?;
    let path = socket_path(home, session_ref);
    println!("TaskAI: waiting for this Codex thread to be ready.");
    let mut stream = connect_owned_bridge(&path, runtime)?;
    let mut input = io::stdin().lock();
    let Some(thread_id) = exchange_first_message(
        &mut stream,
        &mut input,
        session_ref,
        native.pid,
        || {
            println!(
                "{}",
                taskai_connectors::codex::daemon::DaemonClient::first_message_prompt()
            );
            let _ = io::stdout().flush();
        },
        || connect_owned_bridge(&path, runtime),
    )?
    else {
        return Ok(());
    };
    let saved = Registry::open(&state).map_err(|error| error.to_string())?;
    let session = saved
        .session(session_ref)
        .ok_or("Codex gate session disappeared")?;
    if session.provider.as_ref().is_none_or(|mapping| {
        mapping.provider != "codex" || mapping.provider_session_id != thread_id
    }) || tmux::native_identity(runtime)
        .map_err(|error| error.to_string())?
        .as_ref()
        != Some(&native)
    {
        return Err("Codex gate identity changed before native resume".into());
    }
    launch_owned_tui(home, session, &native, &thread_id)
}

fn connect_owned_bridge(
    path: &Path,
    runtime: &taskai_core::registry::TmuxRuntimeLocator,
) -> Result<UnixStream, String> {
    let stream = connect_bridge(path, CONNECT_TIMEOUT)?;
    let (peer_uid, peer_pid) = peer_identity(&stream)?;
    let bridge_pid =
        match tmux::inspect_sidecar(runtime, "bridge").map_err(|error| error.to_string())? {
            RuntimeStatus::Live { pid, .. } => pid,
            _ => return Err("Codex gate has no live owned bridge".into()),
        };
    if peer_uid != current_uid() || peer_pid != bridge_pid {
        return Err("Codex gate socket peer is not its owned bridge".into());
    }
    stream
        .set_read_timeout(Some(CONTROL_TIMEOUT))
        .map_err(|error| error.to_string())?;
    stream
        .set_write_timeout(Some(CONTROL_TIMEOUT))
        .map_err(|error| error.to_string())?;
    Ok(stream)
}

fn connect_bridge(path: &Path, timeout: Duration) -> Result<UnixStream, String> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Ok(stream) = UnixStream::connect(path) {
            return Ok(stream);
        }
        if Instant::now() >= deadline {
            return Err("Codex bridge unavailable; no first turn was submitted".into());
        }
        thread::sleep(CONNECT_RETRY.min(deadline.saturating_duration_since(Instant::now())));
    }
}

fn exchange_first_message(
    stream: &mut UnixStream,
    reader: &mut impl BufRead,
    session_ref: &str,
    gate_pid: u32,
    on_ready: impl FnOnce(),
    mut reconnect: impl FnMut() -> Result<UnixStream, String>,
) -> Result<Option<String>, String> {
    let ready = read_control(stream)?;
    let thread_id = ready["threadId"]
        .as_str()
        .filter(|id| !id.is_empty())
        .ok_or("Codex bridge sent no thread ID")?
        .to_string();
    if ready["status"] != "ready" {
        return Err("Codex bridge did not authorize first input".into());
    }
    on_ready();
    let Some(text) = first_message(reader).map_err(|error| error.to_string())? else {
        return Ok(None);
    };
    // An old bridge may die while the person is still typing. Reconnect only after a proven
    // pre-write close; a write attempt or unknown acknowledgment is never retried.
    if bridge_closed_before_write(stream)? {
        let mut replacement = reconnect()?;
        let next = read_control(&mut replacement)?;
        if next["status"] != "ready" || next["threadId"] != thread_id {
            return Err("Codex replacement bridge did not retain the exact thread".into());
        }
        *stream = replacement;
    }
    // The person's text is confined to this private socket body. It is never
    // placed in process arguments, environment, tmux commands or diagnostics.
    let frame =
        json!({"sessionRef": session_ref, "pid": gate_pid, "threadId": thread_id, "text": text});
    stream
        .write_all(frame.to_string().as_bytes())
        .and_then(|()| stream.write_all(b"\n"))
        .map_err(|_| "Codex first input outcome is unknown".to_string())?;
    let reply =
        read_control(stream).map_err(|_| "Codex first input outcome is unknown".to_string())?;
    match reply["status"].as_str() {
        Some("accepted") => Ok(Some(thread_id)),
        Some("rejected") => Err("Codex refused the first input; no retry was sent".into()),
        _ => Err("Codex first input outcome is unknown; no retry was sent".into()),
    }
}

fn bridge_closed_before_write(stream: &mut UnixStream) -> Result<bool, String> {
    stream
        .set_nonblocking(true)
        .map_err(|_| "Codex gate could not check its bridge before input".to_string())?;
    let mut byte = [0u8; 1];
    let read = stream.read(&mut byte);
    stream
        .set_nonblocking(false)
        .map_err(|_| "Codex gate could not restore its bridge socket".to_string())?;
    match read {
        Ok(0) => Ok(true),
        Err(error) if error.kind() == io::ErrorKind::WouldBlock => Ok(false),
        Err(error) if error.kind() == io::ErrorKind::Interrupted => Ok(false),
        _ => Err("Codex bridge sent unexpected data before first input".into()),
    }
}

fn read_control(stream: &mut UnixStream) -> Result<Value, String> {
    let mut line = Vec::new();
    loop {
        if line.len() >= MAX_CONTROL_BYTES {
            return Err("Codex gate control frame is too large".into());
        }
        let mut byte = [0u8; 1];
        match stream.read(&mut byte) {
            Ok(0) => return Err("Codex bridge closed the gate connection".into()),
            Ok(_) if byte[0] == b'\n' => break,
            Ok(_) => line.push(byte[0]),
            Err(error) if error.kind() == io::ErrorKind::Interrupted => continue,
            Err(_) => return Err("Codex gate control read failed".into()),
        }
    }
    serde_json::from_slice(&line).map_err(|_| "Codex gate control frame is invalid".into())
}

fn launch_owned_tui(
    home: &Path,
    session: &Session,
    native: &NativeIdentity,
    thread_id: &str,
) -> Result<(), String> {
    launch_owned_tui_with(home, session, native, thread_id, |_| {})
}

fn launch_owned_tui_with(
    home: &Path,
    session: &Session,
    native: &NativeIdentity,
    thread_id: &str,
    on_spawn: impl FnOnce(u32),
) -> Result<(), String> {
    let executable = session
        .launch
        .executable
        .clone()
        .unwrap_or_else(|| CodexConnector::default().bin);
    let mut args = Vec::new();
    if let Some(model) = session.launch.model.as_ref() {
        args.extend(["--model".into(), model.clone()]);
    }
    args.extend(["resume".into(), thread_id.into()]);
    let command = tmux::provider_launch_command(&ConnectionTarget::Local, &executable, &args)
        .map_err(|error| error.to_string())?;
    let mut child_command = Command::new(&command.program);
    child_command
        .args(&command.args)
        .current_dir(&session.workspace)
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    strip_messaging_env(&mut child_command);
    let mut child = child_command.spawn().map_err(|error| error.to_string())?;
    on_spawn(child.id());
    let prepared = (|| {
        let started = (0..20)
            .find_map(|_| {
                let found = proc_start(child.id());
                if found.is_none() {
                    thread::sleep(Duration::from_millis(25));
                }
                found
            })
            .ok_or("Codex provider child start time is unavailable")?;
        let path = child_path(home, &session.session_ref);
        fs::create_dir_all(path.parent().unwrap()).map_err(|error| error.to_string())?;
        write_private(
            &path,
            &json!({
                "threadId": thread_id,
                "paneId": native.pane_id,
                "gatePid": native.pid,
                "gateProcStart": native.proc_start,
                "providerPid": child.id(),
                "providerProcStart": started,
            })
            .to_string(),
        )
        .map_err(|error| error.to_string())?;
        Ok::<(), String>(())
    })();
    if let Err(error) = prepared {
        let _ = child.kill();
        let _ = child.wait();
        return Err(error);
    }
    let status = match child.wait() {
        Ok(status) => status,
        Err(error) => {
            let _ = child.kill();
            let _ = child.wait();
            return Err(error.to_string());
        }
    };
    if status.success() {
        Ok(())
    } else {
        Err(format!("Codex native TUI exited with {status}"))
    }
}

/// Read a person's first nonblank line from the gate PTY. The runtime keeps
/// these bytes out of argv, env, tmux commands and logs.
fn first_message(reader: &mut impl BufRead) -> io::Result<Option<String>> {
    loop {
        let mut line = Vec::new();
        loop {
            let available = reader.fill_buf()?;
            if available.is_empty() {
                if line.is_empty() {
                    return Ok(None);
                }
                break;
            }
            let count = available
                .iter()
                .position(|byte| *byte == b'\n')
                .map_or(available.len(), |at| at + 1);
            if line.len() + count > MAX_FIRST_MESSAGE_BYTES {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "first message is too long",
                ));
            }
            line.extend_from_slice(&available[..count]);
            reader.consume(count);
            if line.last() == Some(&b'\n') {
                break;
            }
        }
        if line.last() == Some(&b'\n') {
            line.pop();
        }
        if line.last() == Some(&b'\r') {
            line.pop();
        }
        let text = String::from_utf8(line).map_err(|_| {
            io::Error::new(io::ErrorKind::InvalidData, "first message is not UTF-8")
        })?;
        if !text.trim().is_empty() {
            return Ok(Some(text));
        }
    }
}

#[cfg(test)]
mod tests {
    use std::{
        cell::Cell,
        fs,
        io::{Cursor, Write},
        os::unix::net::UnixStream,
        time::Duration,
    };

    use taskai_core::registry::{HarnessSettings, Registry, Source};
    use tempfile::TempDir;

    use super::{
        connect_bridge, exchange_first_message, first_message, launch_owned_tui_with, read_control,
    };
    use crate::tmux::NativeIdentity;

    #[test]
    fn blank_or_eof_never_submits_the_first_turn() {
        let mut empty = Cursor::new(b"\n\r\n".as_slice());
        assert_eq!(first_message(&mut empty).unwrap(), None);
        let mut line = Cursor::new(b"\n  first human text  \r\n".as_slice());
        assert_eq!(
            first_message(&mut line).unwrap().as_deref(),
            Some("  first human text  ")
        );
    }

    #[test]
    fn oversized_or_invalid_first_input_is_never_a_message() {
        let mut oversized = Cursor::new(vec![b'x'; super::MAX_FIRST_MESSAGE_BYTES + 1]);
        assert_eq!(
            first_message(&mut oversized).unwrap_err().kind(),
            std::io::ErrorKind::InvalidData
        );
        let mut invalid = Cursor::new([0xff, b'\n']);
        assert_eq!(
            first_message(&mut invalid).unwrap_err().kind(),
            std::io::ErrorKind::InvalidData
        );
    }

    #[test]
    fn first_text_crosses_only_the_private_socket_body_once() {
        let (mut gate, mut bridge) = UnixStream::pair().unwrap();
        let server = std::thread::spawn(move || {
            bridge
                .write_all(b"{\"status\":\"ready\",\"threadId\":\"owned-thread\"}\n")
                .unwrap();
            let frame = read_control(&mut bridge).unwrap();
            bridge.write_all(b"{\"status\":\"accepted\"}\n").unwrap();
            frame
        });
        let mut input = Cursor::new(b"private-first-text\n".as_slice());
        let result = exchange_first_message(
            &mut gate,
            &mut input,
            "s1",
            42,
            || {},
            || panic!("the live bridge must not be replaced"),
        )
        .unwrap();
        assert_eq!(result.as_deref(), Some("owned-thread"));
        let frame = server.join().unwrap();
        assert_eq!(frame["sessionRef"], "s1");
        assert_eq!(frame["pid"], 42);
        assert_eq!(frame["threadId"], "owned-thread");
        assert_eq!(frame["text"], "private-first-text");
    }

    #[test]
    fn eof_and_unknown_ack_cannot_submit_a_second_turn() {
        let (mut gate, mut bridge) = UnixStream::pair().unwrap();
        let server = std::thread::spawn(move || {
            bridge
                .write_all(b"{\"status\":\"ready\",\"threadId\":\"owned-thread\"}\n")
                .unwrap();
            bridge
                .set_read_timeout(Some(std::time::Duration::from_millis(250)))
                .unwrap();
            read_control(&mut bridge).is_err()
        });
        let mut empty = Cursor::new(b"\n".as_slice());
        assert_eq!(
            exchange_first_message(
                &mut gate,
                &mut empty,
                "s1",
                42,
                || {},
                || { panic!("EOF must not reconnect") }
            )
            .unwrap(),
            None
        );
        drop(gate);
        assert!(server.join().unwrap());

        let (mut gate, mut bridge) = UnixStream::pair().unwrap();
        let server = std::thread::spawn(move || {
            bridge
                .write_all(b"{\"status\":\"ready\",\"threadId\":\"owned-thread\"}\n")
                .unwrap();
            let first = read_control(&mut bridge).unwrap();
            bridge.write_all(b"{\"status\":\"unknown\"}\n").unwrap();
            first
        });
        let mut input = Cursor::new(b"only-once\n".as_slice());
        assert!(exchange_first_message(
            &mut gate,
            &mut input,
            "s1",
            42,
            || {},
            || { panic!("an unknown acknowledgment must not retry") }
        )
        .unwrap_err()
        .contains("unknown"));
        assert_eq!(server.join().unwrap()["text"], "only-once");
    }

    #[test]
    fn dead_bridge_before_text_reconnects_same_thread_without_a_second_write() {
        let (mut gate, mut old_bridge) = UnixStream::pair().unwrap();
        old_bridge
            .write_all(b"{\"status\":\"ready\",\"threadId\":\"owned-thread\"}\n")
            .unwrap();
        drop(old_bridge);
        let (new_gate, mut new_bridge) = UnixStream::pair().unwrap();
        let server = std::thread::spawn(move || {
            new_bridge
                .write_all(b"{\"status\":\"ready\",\"threadId\":\"owned-thread\"}\n")
                .unwrap();
            let frame = read_control(&mut new_bridge).unwrap();
            new_bridge
                .write_all(b"{\"status\":\"accepted\"}\n")
                .unwrap();
            frame
        });
        let mut replacement = Some(new_gate);
        let mut input = Cursor::new(b"first human text\n".as_slice());
        let result = exchange_first_message(
            &mut gate,
            &mut input,
            "s1",
            42,
            || {},
            || replacement.take().ok_or("reconnect already used".into()),
        )
        .unwrap();
        assert_eq!(result.as_deref(), Some("owned-thread"));
        assert_eq!(server.join().unwrap()["text"], "first human text");
        assert!(replacement.is_none());
    }

    #[test]
    fn replacement_bridge_with_a_different_thread_never_receives_text() {
        let (mut gate, mut old_bridge) = UnixStream::pair().unwrap();
        old_bridge
            .write_all(b"{\"status\":\"ready\",\"threadId\":\"owned-thread\"}\n")
            .unwrap();
        drop(old_bridge);
        let (new_gate, mut new_bridge) = UnixStream::pair().unwrap();
        let server = std::thread::spawn(move || {
            new_bridge
                .write_all(b"{\"status\":\"ready\",\"threadId\":\"other-thread\"}\n")
                .unwrap();
            new_bridge
                .set_read_timeout(Some(Duration::from_millis(100)))
                .unwrap();
            read_control(&mut new_bridge).is_err()
        });
        let mut input = Cursor::new(b"first human text\n".as_slice());
        let mut replacement = Some(new_gate);
        let error = exchange_first_message(
            &mut gate,
            &mut input,
            "s1",
            42,
            || {},
            || replacement.take().ok_or("reconnect already used".into()),
        )
        .unwrap_err();
        assert!(error.contains("thread"));
        assert!(server.join().unwrap());
    }

    #[test]
    fn unavailable_bridge_gives_a_bounded_visible_reason() {
        let root = TempDir::new().unwrap();
        let error = connect_bridge(&root.path().join("absent.sock"), Duration::from_millis(80))
            .unwrap_err();
        assert!(error.contains("bridge"));
        assert!(error.contains("unavailable"));
    }

    #[test]
    fn failed_provider_metadata_kills_and_reaps_only_spawned_child() {
        let root = TempDir::new().unwrap();
        let workspace = root.path().join("workspace");
        fs::create_dir(&workspace).unwrap();
        let fake = root.path().join("fake-codex");
        fs::write(&fake, "#!/bin/sh\nexec sleep 30\n").unwrap();
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&fake, fs::Permissions::from_mode(0o755)).unwrap();
        let state = root.path().join("state");
        let mut registry = Registry::open(&state).unwrap();
        registry
            .set_server_harness(
                "local",
                "codex",
                HarnessSettings {
                    enabled: true,
                    executable: Some(fake.display().to_string()),
                    model: None,
                },
                Source::HumanUi,
            )
            .unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let session_ref = registry
            .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
            .unwrap();
        let session = registry.session(&session_ref).unwrap();
        // A directory at the exact metadata path makes the post-spawn write fail.
        fs::create_dir_all(root.path().join("run/codex/s1.provider.json")).unwrap();
        let native = NativeIdentity {
            pane_id: "%1".into(),
            pid: std::process::id(),
            proc_start: "fixture".into(),
        };
        let spawned = Cell::new(0);
        let error = launch_owned_tui_with(root.path(), session, &native, "owned-thread", |pid| {
            spawned.set(pid)
        })
        .unwrap_err();
        assert!(!error.is_empty());
        assert_ne!(spawned.get(), 0);
        assert!(
            taskai_core::mail::proc_start(spawned.get()).is_none(),
            "the exact spawned child survived the write failure"
        );
    }
}
