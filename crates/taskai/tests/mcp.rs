// ABOUTME: Offline proofs that `taskai mcp` is a stateless stdio MCP server
// ABOUTME: relaying to TASKAI_TOOL_SOCKET under GROK_SESSION_ID.

use std::io::Write;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;
use std::process::{Command, Stdio};
use std::thread;
use std::time::Duration;

use taskai_connectors::grok::{install_mcp, pane_env};

fn taskai() -> Command {
    let mut c = Command::new(env!("CARGO_BIN_EXE_taskai"));
    c.env_remove("TASKAI_SESSION_REF")
        .env_remove("TASKAI_HOME")
        .env_remove("TASKAI_TOOL_SOCKET")
        .env_remove("GROK_SESSION_ID");
    c
}

fn tmp() -> tempfile::TempDir {
    tempfile::Builder::new()
        .prefix("t5")
        .tempdir_in("/tmp")
        .unwrap()
}

fn rpc(child: &mut std::process::Child, msg: serde_json::Value) -> serde_json::Value {
    let stdin = child.stdin.as_mut().unwrap();
    writeln!(stdin, "{msg}").unwrap();
    stdin.flush().unwrap();
    let mut line = String::new();
    let stdout = child.stdout.as_mut().unwrap();
    use std::io::BufRead;
    std::io::BufReader::new(stdout)
        .read_line(&mut line)
        .unwrap();
    serde_json::from_str(line.trim_end()).unwrap()
}

// Expectation: `taskai mcp` answers tools/list with list_sessions and
// send_message, and a tools/call against a missing socket is
// bridge-unavailable. Why: Grok launches this binary as a stdio MCP server.
// Value: the subcommand is wired, not just a library function.
#[test]
fn taskai_mcp_lists_tools_and_reports_missing_bridge() {
    let mut child = taskai()
        .arg("mcp")
        .env("GROK_SESSION_ID", "sess-1")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();

    let listed = rpc(
        &mut child,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"tools/list"}),
    );
    let names: Vec<_> = listed["result"]["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, ["list_sessions", "send_message"]);

    let call = rpc(
        &mut child,
        serde_json::json!({"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}),
    );
    let payload: serde_json::Value =
        serde_json::from_str(call["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(payload["status"], "bridge-unavailable");
    assert_eq!(call["result"]["isError"], true);

    drop(child.stdin.take());
    let status = child.wait().unwrap();
    assert!(status.success());
}

// Expectation: a live private socket plus GROK_SESSION_ID lets tools/call
// reach the bridge; a mismatched id is refused. Why: the MCP process is
// identified by Grok's session env, not by a stored address. Value: one
// binary serves every Grok session.
#[test]
fn taskai_mcp_relays_owned_session_only() {
    let tmp = tmp();
    let path = tmp.path().join("t.sock");
    let listener = UnixListener::bind(&path).unwrap();
    let server = thread::spawn(move || {
        let Ok((mut stream, _)) = listener.accept() else {
            return;
        };
        let mut buf = String::new();
        use std::io::BufRead;
        let _ = std::io::BufReader::new(&stream).read_line(&mut buf);
        let call: serde_json::Value =
            serde_json::from_str(buf.trim_end()).unwrap_or_else(|_| serde_json::json!({}));
        let reply = if call["session_id"] == "sess-1" && call["tool"] == "list_sessions" {
            serde_json::json!({"status":"ok","sessions":[{"name":"codex-lead"}]})
        } else {
            serde_json::json!({"status":"refused-unknown-session"})
        };
        let _ = writeln!(stream, "{reply}");
    });
    thread::sleep(Duration::from_millis(20));

    let mut child = taskai()
        .arg("mcp")
        .env("TASKAI_TOOL_SOCKET", &path)
        .env("GROK_SESSION_ID", "sess-1")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let call = rpc(
        &mut child,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions"}}),
    );
    let payload: serde_json::Value =
        serde_json::from_str(call["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(payload["status"], "ok");
    assert_eq!(payload["sessions"][0]["name"], "codex-lead");
    drop(child.stdin.take());
    let _ = child.wait();
    let _ = UnixStream::connect(&path);
    let _ = server.join();
}

fn fake_bridge(path: &Path, session_id: &str) -> thread::JoinHandle<()> {
    let path = path.to_path_buf();
    let session_id = session_id.to_string();
    let listener = UnixListener::bind(&path).unwrap();
    thread::spawn(move || {
        let Ok((mut stream, _)) = listener.accept() else {
            return;
        };
        let mut buf = String::new();
        use std::io::BufRead;
        let _ = std::io::BufReader::new(&stream).read_line(&mut buf);
        let call: serde_json::Value =
            serde_json::from_str(buf.trim_end()).unwrap_or_else(|_| serde_json::json!({}));
        let reply = if call["session_id"] == session_id && call["tool"] == "list_sessions" {
            serde_json::json!({"status":"ok","sessions":[{"name": session_id}]})
        } else {
            serde_json::json!({"status":"refused-unknown-session"})
        };
        let _ = writeln!(stream, "{reply}");
    })
}

fn mcp_child(socket: &Path, session_id: &str) -> std::process::Child {
    let mut command = taskai();
    command.arg("mcp");
    for (key, value) in pane_env(session_id, socket) {
        command.env(key, value);
    }
    command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap()
}

// Expectation: `taskai mcp` children inherit distinct pane TASKAI_TOOL_SOCKET
// values while sharing one static workspace MCP file. Why: Grok must not
// read a last-install-wins socket from config. Value: two agents in one
// workspace keep their own bridges. Live Grok inheritance is the later slot.
#[test]
fn mcp_child_inherits_pane_socket_not_workspace_config() {
    let tmp = tmp();
    let ws = tmp.path().join("ws");
    install_mcp(&ws, std::path::Path::new(env!("CARGO_BIN_EXE_taskai"))).unwrap();
    let config = std::fs::read_to_string(ws.join(".grok/config.toml")).unwrap();
    assert!(
        !config.contains("TASKAI_TOOL_SOCKET"),
        "workspace config must stay static: {config}"
    );

    let sock_a = tmp.path().join("a.tools.sock");
    let sock_b = tmp.path().join("b.tools.sock");
    let server_a = fake_bridge(&sock_a, "sess-a");
    let server_b = fake_bridge(&sock_b, "sess-b");
    thread::sleep(Duration::from_millis(20));

    let mut child_a = mcp_child(&sock_a, "sess-a");
    let mut child_b = mcp_child(&sock_b, "sess-b");
    let call_a = rpc(
        &mut child_a,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions"}}),
    );
    let call_b = rpc(
        &mut child_b,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions"}}),
    );
    let payload_a: serde_json::Value =
        serde_json::from_str(call_a["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    let payload_b: serde_json::Value =
        serde_json::from_str(call_b["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(payload_a["status"], "ok");
    assert_eq!(payload_a["sessions"][0]["name"], "sess-a");
    assert_eq!(payload_b["status"], "ok");
    assert_eq!(payload_b["sessions"][0]["name"], "sess-b");
    drop(child_a.stdin.take());
    drop(child_b.stdin.take());
    let _ = child_a.wait();
    let _ = child_b.wait();
    let _ = UnixStream::connect(&sock_a);
    let _ = UnixStream::connect(&sock_b);
    let _ = server_a.join();
    let _ = server_b.join();
    assert_eq!(
        std::fs::read_to_string(ws.join(".grok/config.toml")).unwrap(),
        config
    );
}
