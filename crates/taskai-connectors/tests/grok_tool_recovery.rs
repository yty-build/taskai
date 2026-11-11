// ABOUTME: Proves Grok tool-socket recovery with killed and stopped private child servers.
// ABOUTME: Checks lifetime ownership, concurrent binding and exact cleanup without native providers.

use std::{
    fs,
    os::unix::{fs::MetadataExt, net::UnixListener},
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    sync::{Arc, Barrier},
    thread,
    time::{Duration, Instant},
};

use serde_json::{json, Value};
use taskai_connectors::grok::{call_tool, ToolHandler, ToolServer};
use taskai_core::mail::strip_messaging_env;

struct Tools;
impl ToolHandler for Tools {
    fn list_sessions(&self) -> Result<Value, String> {
        Ok(json!([]))
    }
    fn send_message(&self, _: &str, _: &str, _: bool) -> Result<Value, String> {
        Ok(json!({"status":"sent"}))
    }
}

struct OwnedChild(Child);
impl Drop for OwnedChild {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn wait_for(mut predicate: impl FnMut() -> bool) -> bool {
    let until = Instant::now() + Duration::from_secs(4);
    while Instant::now() < until {
        if predicate() {
            return true;
        }
        thread::sleep(Duration::from_millis(10));
    }
    predicate()
}

fn spawn_copy(test: &str, path: &Path, ready: &Path) -> OwnedChild {
    let mut command = Command::new(std::env::current_exe().unwrap());
    command
        .args(["--exact", test, "--nocapture"])
        .env("TASKAI_RECOVERY_SOCKET", path)
        .env("TASKAI_RECOVERY_READY", ready)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    strip_messaging_env(&mut command);
    OwnedChild(command.spawn().unwrap())
}

#[test]
fn recovery_child_server() {
    let Some(path) = std::env::var_os("TASKAI_RECOVERY_SOCKET") else {
        return;
    };
    let _server = ToolServer::listen(Path::new(&path), "owned", Tools).unwrap();
    fs::write(std::env::var_os("TASKAI_RECOVERY_READY").unwrap(), "ready").unwrap();
    loop {
        thread::sleep(Duration::from_millis(100));
    }
}

#[test]
fn recovery_child_attempt() {
    let Some(path) = std::env::var_os("TASKAI_RECOVERY_SOCKET") else {
        return;
    };
    let result = ToolServer::listen(Path::new(&path), "owned", Tools);
    fs::write(
        std::env::var_os("TASKAI_RECOVERY_READY").unwrap(),
        if result.is_ok() {
            "accepted"
        } else {
            "refused"
        },
    )
    .unwrap();
}

fn start_server(root: &Path) -> (PathBuf, OwnedChild) {
    let path = root.join("tools.sock");
    let ready = root.join("ready");
    let child = spawn_copy("recovery_child_server", &path, &ready);
    assert!(
        wait_for(|| ready.exists()),
        "private server failed to start"
    );
    fs::remove_file(ready).unwrap();
    assert_eq!(
        call_tool(&path, "owned", "list_sessions", json!({}))["status"],
        "ok"
    );
    (path, child)
}

fn signal(child: &OwnedChild, signal: &str) {
    let mut command = Command::new("/bin/kill");
    command.args([signal, &child.0.id().to_string()]);
    strip_messaging_env(&mut command);
    assert!(command.status().unwrap().success());
}

fn kill_and_reap(child: &mut OwnedChild) {
    child.0.kill().unwrap();
    child.0.wait().unwrap();
}

fn claim(root: &Path) -> PathBuf {
    let claims: Vec<_> = fs::read_dir(root)
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .filter(|path| {
            path.extension()
                .is_some_and(|extension| extension == "lock")
        })
        .collect();
    assert_eq!(claims.len(), 1, "one exact-socket ownership claim");
    claims[0].clone()
}

// A killed bridge cannot run Drop. Its successor must recover its exact socket and remove
// the previous ownership claim, then serve calls and leave no socket/claim on normal close.
#[test]
fn killed_tool_server_is_recovered_and_its_claim_is_cleaned() {
    let root = tempfile::tempdir().unwrap();
    let (path, mut child) = start_server(root.path());
    kill_and_reap(&mut child);
    let server = ToolServer::listen(&path, "owned", Tools).expect("recover killed owner");
    assert_eq!(
        call_tool(&path, "owned", "list_sessions", json!({}))["status"],
        "ok"
    );
    let claim = fs::symlink_metadata(claim(root.path())).unwrap();
    assert!(claim.is_file());
    assert_eq!(claim.mode() & 0o777, 0o600);
    assert_eq!(fs::metadata(&path).unwrap().mode() & 0o777, 0o600);
    drop(server);
    assert_eq!(fs::read_dir(root.path()).unwrap().count(), 0);
}

// ECONNREFUSED is also returned by macOS for a stopped live server with a full backlog.
// An actual lifetime lock must refuse replacement before the probe; every child is bounded
// and reaped even when the old implementation hangs or accepts the live socket.
#[test]
fn stopped_owner_with_full_backlog_keeps_its_socket() {
    let root = tempfile::tempdir().unwrap();
    let (path, child) = start_server(root.path());
    let original = fs::symlink_metadata(&path).unwrap().ino();
    signal(&child, "-STOP");
    let ready = root.path().join("full");
    let mut filler = Command::new("python3");
    filler.args(["-c", "import socket,sys,time,pathlib\nsockets=[]\nfor i in range(512):\n s=socket.socket(socket.AF_UNIX); s.settimeout(.02)\n try: s.connect(sys.argv[1]); sockets.append(s)\n except OSError: s.close(); break\npathlib.Path(sys.argv[2]).write_text(str(len(sockets)))\ntime.sleep(30)"])
        .arg(&path).arg(&ready).stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null());
    strip_messaging_env(&mut filler);
    let _filler = OwnedChild(filler.spawn().unwrap());
    assert!(
        wait_for(|| ready.exists()),
        "private backlog filler timed out"
    );
    let count: usize = fs::read_to_string(&ready).unwrap().parse().unwrap();
    assert!(count > 0 && count < 512, "backlog was not filled: {count}");
    let result = root.path().join("attempt");
    let _attempt = spawn_copy("recovery_child_attempt", &path, &result);
    assert!(
        wait_for(|| result.exists()),
        "rebind blocked against a live owner"
    );
    assert_eq!(
        fs::read_to_string(&result).unwrap(),
        "refused",
        "live owner replaced"
    );
    assert_eq!(fs::symlink_metadata(&path).unwrap().ino(), original);
    signal(&child, "-CONT");
}

// An unclaimed stale socket may have an owner whose backlog is full. Recovery requires
// proof from a cooperating ToolServer; an unknown/preclaim socket stays untouched.
#[test]
fn an_unclaimed_stale_socket_is_refused() {
    let root = tempfile::tempdir().unwrap();
    let path = root.path().join("unknown.sock");
    drop(UnixListener::bind(&path).unwrap());
    let inode = fs::symlink_metadata(&path).unwrap().ino();
    assert!(ToolServer::listen(&path, "owned", Tools).is_err());
    assert_eq!(fs::symlink_metadata(&path).unwrap().ino(), inode);
}

// Eight cooperating constructors racing after the same crash must leave exactly one
// listener; the winner's endpoint and claim remain usable until that winner closes.
#[test]
fn concurrent_recovery_keeps_one_server() {
    let root = tempfile::tempdir().unwrap();
    let (path, mut child) = start_server(root.path());
    kill_and_reap(&mut child);
    let barrier = Arc::new(Barrier::new(8));
    let threads: Vec<_> = (0..8)
        .map(|_| {
            let path = path.clone();
            let barrier = Arc::clone(&barrier);
            thread::spawn(move || {
                barrier.wait();
                ToolServer::listen(&path, "owned", Tools)
            })
        })
        .collect();
    let results: Vec<_> = threads
        .into_iter()
        .map(|thread| thread.join().unwrap())
        .collect();
    assert_eq!(results.iter().filter(|result| result.is_ok()).count(), 1);
    assert_eq!(
        call_tool(&path, "owned", "list_sessions", json!({}))["status"],
        "ok"
    );
    drop(results);
    assert_eq!(fs::read_dir(root.path()).unwrap().count(), 0);
}

// A replaced claim is no authority to remove an endpoint. Do not follow a symlink or
// modify its target, even when it lives beside a known killed ToolServer's socket.
#[test]
fn a_replaced_claim_cannot_authorize_recovery() {
    let root = tempfile::tempdir().unwrap();
    let (path, mut child) = start_server(root.path());
    kill_and_reap(&mut child);
    let inode = fs::symlink_metadata(&path).unwrap().ino();
    let claim = claim(root.path());
    fs::remove_file(&claim).unwrap();
    let target = root.path().join("sentinel");
    fs::write(&target, "unchanged").unwrap();
    std::os::unix::fs::symlink(&target, &claim).unwrap();
    assert!(ToolServer::listen(&path, "owned", Tools).is_err());
    assert_eq!(fs::symlink_metadata(&path).unwrap().ino(), inode);
    assert!(fs::symlink_metadata(&claim)
        .unwrap()
        .file_type()
        .is_symlink());
    assert_eq!(fs::read_to_string(target).unwrap(), "unchanged");
}

// Close and bind must share the same turn, so a check-and-unlink cannot interleave
// with another cooperating constructor replacing the socket.
#[test]
fn close_waits_for_the_directory_bind_turn() {
    let root = tempfile::tempdir().unwrap();
    let path = root.path().join("tools.sock");
    let server = ToolServer::listen(&path, "owned", Tools).unwrap();
    let directory = fs::File::open(root.path()).unwrap();
    rustix::fs::flock(&directory, rustix::fs::FlockOperation::LockExclusive).unwrap();
    let closing = thread::spawn(move || drop(server));
    thread::sleep(Duration::from_millis(150));
    let kept = path.exists();
    drop(directory);
    closing.join().unwrap();
    assert!(kept, "close removed socket during another binder's turn");
    assert_eq!(fs::read_dir(root.path()).unwrap().count(), 0);
}
