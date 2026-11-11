// ABOUTME: Offline Grok mail adapter proofs — leader/client argv, workspace MCP merge,
// ABOUTME: session-bound tool socket, and fake-leader join. Never contacts a live leader.

use std::collections::HashMap;
use std::fs::OpenOptions;
use std::io::Write;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::{mpsc, Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};
use taskai_connectors::grok::{
    atomic_write, call_tool, handle_mcp_message, install_mcp, interpret_notification,
    leader_log_path, leader_socket_path, pane_env, tool_socket_path, GrokBridge, GrokConnector,
    GrokTools, JoinConfig, LeaderClient, PermissionMode, StatusEvent, ToolHandler, ToolServer,
    MAX_TOOL_LINE_BYTES,
};
use taskai_core::mail::{
    strip_messaging_env, Activity, Delivery, DeliveryKind, Directory, Frame, Identity, IdleNotice,
    Letter, MailPeer, Mailbox, MailboxConfig, NoticeState, PermissionMode as MailMode, Priority,
    Receipt, ReceiptStatus, SocketTransport, Subscription, SubscriptionResult, Transport,
};

fn tmp() -> tempfile::TempDir {
    tempfile::Builder::new()
        .prefix("t5")
        .tempdir_in("/tmp")
        .unwrap()
}

// Expectation: the pane argv is the isolated leader TUI with a pre-assigned session
// id and the session's permission mode, never a global socket. Why: Grok's live
// session is only reachable through its leader, and omitting the mode lets a client
// inherit always-approve. Value: launch argv is exact and safe to review.
#[test]
fn pane_command_pins_leader_socket_session_and_mode() {
    let c = GrokConnector::default();
    let sock = PathBuf::from("/tmp/t5/s3.sock");
    let (prog, args) = c.pane_command(
        &sock,
        "136c5f06-a61e-4133-b682-940a4207f3d9",
        PermissionMode::Prompting,
        None,
    );
    assert_eq!(prog, "grok");
    assert_eq!(
        args,
        vec![
            "--leader",
            "--leader-socket",
            "/tmp/t5/s3.sock",
            "-s",
            "136c5f06-a61e-4133-b682-940a4207f3d9",
            "--permission-mode",
            "default",
        ]
    );
}

// Expectation: an optional model is appended as `-m`, never spliced into the
// session id or socket path. Why: launch.rs already treats --model as a verified
// native flag. Value: model override stays a flag.
#[test]
fn pane_command_appends_model_flag() {
    let c = GrokConnector::default();
    let sock = PathBuf::from("/tmp/t5/s3.sock");
    let (_prog, args) = c.pane_command(
        &sock,
        "aaaaaaaa-bbbb-cccc-dddd-eeeeffff0000",
        PermissionMode::Prompting,
        Some("grok-4.6"),
    );
    assert_eq!(args[args.len() - 2], "-m");
    assert_eq!(args[args.len() - 1], "grok-4.6");
}

// Expectation: pane launch env carries this agent's GROK_SESSION_ID and
// TASKAI_TOOL_SOCKET. Why: workspace MCP config is static; Grok must inherit
// the socket from the pane, not from a last-install-wins file. Value: two
// Grok agents in one workspace keep distinct tool sockets.
#[test]
fn pane_env_exports_session_and_tool_socket() {
    let sock = PathBuf::from("/tmp/t5/s3.tools.sock");
    let env = pane_env("136c5f06-a61e-4133-b682-940a4207f3d9", &sock);
    assert_eq!(
        env,
        vec![
            (
                "GROK_SESSION_ID".into(),
                "136c5f06-a61e-4133-b682-940a4207f3d9".into()
            ),
            ("TASKAI_TOOL_SOCKET".into(), "/tmp/t5/s3.tools.sock".into()),
        ]
    );
}

// Expectation: the joining client puts `--permission-mode` before `agent` and
// points at the same per-session leader socket. Why: grok agent has no permission
// flag of its own; the top-level flag is the explicit mode for this client.
// Value: the shared session cannot inherit the user's global always-approve.
#[test]
fn client_command_passes_mode_before_agent() {
    let c = GrokConnector::default();
    let sock = PathBuf::from("/tmp/t5/s3.sock");
    let (prog, args) = c.client_command(&sock, PermissionMode::Prompting);
    assert_eq!(prog, "grok");
    assert_eq!(
        args,
        vec![
            "--permission-mode",
            "default",
            "agent",
            "--leader",
            "--leader-socket",
            "/tmp/t5/s3.sock",
            "stdio",
        ]
    );
    assert!(!args
        .iter()
        .any(|a| a == "--always-approve" || a == "--yolo"));
}

// Expectation: bypass maps to Grok's bypassPermissions CLI value. Why: mail
// PermissionMode is prompting|bypass; Grok's flag names differ. Value: one
// mapping, no silent default.
#[test]
fn bypass_mode_uses_cli_bypass_permissions() {
    let c = GrokConnector::default();
    let sock = PathBuf::from("/tmp/t5/s3.sock");
    let (_, pane) = c.pane_command(&sock, "id", PermissionMode::Bypass, None);
    let (_, client) = c.client_command(&sock, PermissionMode::Bypass);
    assert_eq!(pane[6], "bypassPermissions");
    assert_eq!(client[1], "bypassPermissions");
}

// Expectation: leader and tool sockets live under the TaskAI run dir, named by
// session ref, never under ~/.grok. Why: per-session isolation; tests must not
// touch the live leader. Value: paths are deterministic for M6.
#[test]
fn sockets_live_under_run_dir() {
    let run = tmp();
    let leader = leader_socket_path(run.path(), "s3");
    let tools = tool_socket_path(run.path(), "s3");
    assert_eq!(leader, run.path().join("grok/s3.sock"));
    assert_eq!(tools, run.path().join("grok/s3.tools.sock"));
    assert!(!leader.starts_with(std::env::var("HOME").unwrap()));
}

// Expectation: workspace `.grok/config.toml` gains a taskai MCP block and
// `MCPTool(taskai__*)` allow rule, preserving unrelated servers and rules, and a
// second install does not duplicate. Why: TaskAI never clobbers project config
// (backend §5) and never writes `~/.grok/config.toml`. Value: one merged file.
#[test]
fn mcp_install_merges_and_is_idempotent() {
    let tmp = tmp();
    let ws = tmp.path().join("ws");
    std::fs::create_dir_all(ws.join(".grok")).unwrap();
    std::fs::write(
        ws.join(".grok/config.toml"),
        "[mcp_servers.user-tools]\ncommand = \"uvx\"\nargs = [\"my-tools\"]\n\n[permission]\nallow = [\"Bash(git *)\"]\n",
    )
    .unwrap();

    let bin = PathBuf::from("/tmp/taskai-bin");
    install_mcp(&ws, &bin).unwrap();
    install_mcp(&ws, &bin).unwrap();

    let body = std::fs::read_to_string(ws.join(".grok/config.toml")).unwrap();
    let mode = std::fs::metadata(ws.join(".grok/config.toml"))
        .unwrap()
        .permissions()
        .mode()
        & 0o777;
    assert_eq!(mode, 0o600, "installed config is owner-only");
    assert!(
        body.contains("command = \"uvx\""),
        "user server preserved: {body}"
    );
    assert_eq!(
        body.matches("[mcp_servers.taskai]").count(),
        1,
        "one taskai server: {body}"
    );
    assert!(body.contains("/tmp/taskai-bin"), "{body}");
    assert!(body.contains("\"mcp\""), "{body}");
    assert!(
        !body.contains("TASKAI_TOOL_SOCKET"),
        "socket must not be stored: {body}"
    );
    assert!(
        !body.contains(".tools.sock"),
        "socket path must not be stored: {body}"
    );
    assert_eq!(
        body.matches("MCPTool(taskai__*)").count(),
        1,
        "allow once: {body}"
    );
    assert!(body.contains("Bash(git *)"), "user allow preserved: {body}");
}

// Global HOME/GROK_HOME install assertions live in tests/mcp_install_home.rs
// so this parallel binary never mutates process environment.

struct StubTools {
    listed: Mutex<u32>,
    sent: Mutex<Vec<(String, String, bool)>>,
}

impl StubTools {
    fn new() -> Self {
        Self {
            listed: Mutex::new(0),
            sent: Mutex::new(Vec::new()),
        }
    }
}

impl ToolHandler for StubTools {
    fn list_sessions(&self) -> Result<serde_json::Value, String> {
        *self.listed.lock().unwrap() += 1;
        Ok(json!([{"name": "codex-lead", "harness": "codex", "status": "idle"}]))
    }

    fn send_message(
        &self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
    ) -> Result<serde_json::Value, String> {
        self.sent
            .lock()
            .unwrap()
            .push((to.into(), text.into(), notify_when_idle));
        Ok(json!({"status": "sent", "to": to, "msg_id": "m1"}))
    }
}

struct DropFlag {
    dropped: Arc<AtomicBool>,
    inner: StubTools,
}

impl Drop for DropFlag {
    fn drop(&mut self) {
        self.dropped.store(true, Ordering::SeqCst);
    }
}

impl ToolHandler for DropFlag {
    fn list_sessions(&self) -> Result<serde_json::Value, String> {
        self.inner.list_sessions()
    }
    fn send_message(
        &self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
    ) -> Result<serde_json::Value, String> {
        self.inner.send_message(to, text, notify_when_idle)
    }
}

struct CountOnDrop {
    dropped: Arc<AtomicBool>,
    listed: Arc<AtomicU32>,
}

impl Drop for CountOnDrop {
    fn drop(&mut self) {
        self.dropped.store(true, Ordering::SeqCst);
    }
}

impl ToolHandler for CountOnDrop {
    fn list_sessions(&self) -> Result<serde_json::Value, String> {
        self.listed.fetch_add(1, Ordering::SeqCst);
        Ok(json!([]))
    }
    fn send_message(
        &self,
        _to: &str,
        _text: &str,
        _notify_when_idle: bool,
    ) -> Result<serde_json::Value, String> {
        Ok(json!({"status": "sent"}))
    }
}

struct FailList;

impl ToolHandler for FailList {
    fn list_sessions(&self) -> Result<serde_json::Value, String> {
        Err("registry locked".into())
    }
    fn send_message(
        &self,
        _to: &str,
        _text: &str,
        _notify_when_idle: bool,
    ) -> Result<serde_json::Value, String> {
        Ok(json!({"status": "sent"}))
    }
}

// Expectation: a Unix socket in a private temp dir serves list_sessions and
// send_message only when the caller carries this session's id. Why: the MCP
// process holds no address book; the bridge is the allowlist. Value: a foreign
// GROK_SESSION_ID cannot send as this agent.
#[test]
fn tool_socket_refuses_foreign_session_id() {
    let tmp = tmp();
    let path = tmp.path().join("t.sock");
    let server = ToolServer::listen(&path, "owned-session", StubTools::new()).unwrap();
    let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600, "tool socket is owner-only");
    let parent_mode = std::fs::metadata(path.parent().unwrap())
        .unwrap()
        .permissions()
        .mode()
        & 0o777;
    assert_eq!(parent_mode, 0o700, "runtime parent is owner-only");

    let refused = call_tool(&path, "other-session", "list_sessions", json!({}));
    assert_eq!(refused["status"], "refused-unknown-session");

    let listed = call_tool(&path, "owned-session", "list_sessions", json!({}));
    assert_eq!(listed["status"], "ok");
    assert_eq!(listed["sessions"][0]["name"], "codex-lead");

    let sent = call_tool(
        &path,
        "owned-session",
        "send_message",
        json!({"to": "codex-lead", "text": "hello", "notify_when_idle": true}),
    );
    assert_eq!(sent["status"], "sent");
    assert_eq!(sent["to"], "codex-lead");

    let unknown = call_tool(&path, "owned-session", "explode", json!({}));
    assert_eq!(unknown["status"], "unknown-tool");
    drop(server);
}

// Expectation: a missing or dead tool socket is reported as bridge-unavailable,
// never guessed around. Why: Grok's MCP server holds no mailbox of its own.
// Value: the agent sees the truth when the bridge is gone.
#[test]
fn call_tool_reports_bridge_unavailable() {
    let tmp = tmp();
    let path = tmp.path().join("missing.sock");
    let result = call_tool(&path, "owned-session", "list_sessions", json!({}));
    assert_eq!(result["status"], "bridge-unavailable");
}

// Expectation: the MCP server speaks initialize, tools/list (exactly two tools),
// tools/call, and ping, and holds no address of its own. Why: Grok only loads
// tools through MCP; the stdio process is a thin client of the tool socket.
// Value: list and send work from the first Grok turn without global config.
#[test]
fn mcp_stdio_lists_two_tools_and_relays_calls() {
    let tmp = tmp();
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", StubTools::new()).unwrap();

    let init = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}),
        &path,
        "sess-1",
    )
    .unwrap();
    assert_eq!(init["result"]["protocolVersion"], "2025-06-18");
    assert_eq!(init["result"]["serverInfo"]["name"], "taskai");
    assert_eq!(
        init["result"]["capabilities"]["tools"]["listChanged"],
        false
    );

    let listed = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}),
        &path,
        "sess-1",
    )
    .unwrap();
    let tools = listed["result"]["tools"].as_array().unwrap();
    assert_eq!(tools.len(), 2);
    assert_eq!(tools[0]["name"], "list_sessions");
    assert_eq!(tools[1]["name"], "send_message");
    assert!(tools[1]["inputSchema"]["required"]
        .as_array()
        .unwrap()
        .iter()
        .any(|v| v == "to"));

    let call = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"codex-lead","text":"hi","notify_when_idle":true}}}),
        &path,
        "sess-1",
    )
    .unwrap();
    let payload: serde_json::Value =
        serde_json::from_str(call["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(payload["status"], "sent");
    assert_eq!(call["result"]["isError"], false);

    let ping = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":4,"method":"ping"}),
        &path,
        "sess-1",
    )
    .unwrap();
    assert_eq!(ping["result"], json!({}));

    assert!(handle_mcp_message(
        &json!({"jsonrpc":"2.0","method":"notifications/initialized"}),
        &path,
        "sess-1",
    )
    .is_none());
}

// Expectation: a dead tool socket or a foreign session id surfaces as an MCP
// error result, never as a silent ok. Why: the proof's Grok tool reported
// bridge-unavailable once its bridge was gone. Value: the agent can retry later.
#[test]
fn mcp_stdio_reports_bridge_and_foreign_session() {
    let tmp = tmp();
    let missing = tmp.path().join("gone.sock");
    let dead = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}),
        &missing,
        "sess-1",
    )
    .unwrap();
    let payload: serde_json::Value =
        serde_json::from_str(dead["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(payload["status"], "bridge-unavailable");
    assert_eq!(dead["result"]["isError"], true);

    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", StubTools::new()).unwrap();
    let foreign = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_sessions"}}),
        &path,
        "other",
    )
    .unwrap();
    let payload: serde_json::Value =
        serde_json::from_str(foreign["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(payload["status"], "refused-unknown-session");
    assert_eq!(foreign["result"]["isError"], true);

    let unknown = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":3,"method":"nope"}),
        &path,
        "sess-1",
    )
    .unwrap();
    assert_eq!(unknown["error"]["code"], -32601);
}

const FAKE_ACP: &str = r#"
import json, os, sys, time
log = os.environ["FAKE_LEADER_LOG"]
seen = os.environ["FAKE_SEEN"]
yolo = os.environ.get("FAKE_YOLO", "false")
sock = os.environ.get("FAKE_SOCK", "")
delay = float(os.environ.get("FAKE_PROMPT_DELAY", "0"))
perm = os.environ.get("FAKE_PERMISSION", "")
open(os.environ["FAKE_ENV_FLAG"], "w").write(
    "present" if "CLAUDE_CODE_MESSAGING_TOKEN" in os.environ else "absent"
)
if os.environ.get("FAKE_PID"):
    open(os.environ["FAKE_PID"], "w").write(str(os.getpid()))
if os.environ.get("FAKE_HOLD_STDOUT"):
    import subprocess
    holder = subprocess.Popen(["sleep", "1000"])
    open(os.environ["FAKE_HOLD_STDOUT"], "w").write(str(holder.pid))
def reply(msg_id, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg_id, "result": result}) + "\n")
    sys.stdout.flush()
loads = 0
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    with open(seen, "a") as f:
        f.write(json.dumps(msg) + "\n")
    method = msg.get("method")
    if method == "initialize":
        if os.environ.get("FAKE_INIT_HANG"):
            time.sleep(1000)
        if not os.environ.get("FAKE_NO_REGISTRATION"):
            with open(log, "a") as f:
                f.write("2026-11-11T03:13:03.960303Z  INFO Client registered client_id=1 client_type=python3 --permission-mode default agent --leader --leader-socket %s stdio mode=Stdio yolo_mode=%s client_version=Some(\"1.0.77\")\n" % (sock, yolo))
        load = os.environ.get("FAKE_LOAD_SESSION", "true") == "true"
        reply(msg["id"], {"protocolVersion": 1, "agentCapabilities": {"loadSession": load}})
    elif method == "session/load":
        if os.environ.get("FAKE_LOAD_HANG"):
            time.sleep(1000)
        # Like the leader before its CLI has created the session: a definite refusal.
        refusals = os.environ.get("FAKE_LOAD_REFUSALS", "0")
        loads = loads + 1
        if refusals == "always" or loads <= int(refusals):
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32603, "message": "Path not found."}}) + "\n")
            sys.stdout.flush()
            continue
        reply(msg["id"], {})
        if perm:
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": 99, "method": "session/request_permission", "params": {"sessionId": os.environ.get("FAKE_SESSION", ""), "toolCall": {"secret": "do-not-log"}}}) + "\n")
            sys.stdout.flush()
        if os.environ.get("FAKE_STOP_READING"):
            time.sleep(1000)
    elif method == "session/prompt":
        for note in json.loads(os.environ.get("FAKE_PROMPT_NOTIFY", "[]")):
            sys.stdout.write(json.dumps(note) + "\n")
        sys.stdout.flush()
        custom = os.environ.get("FAKE_PROMPT_REPLY")
        if custom:
            response = json.loads(custom)
            response.update(jsonrpc="2.0", id=msg["id"])
            sys.stdout.write(json.dumps(response) + "\n")
            sys.stdout.flush()
            continue
        err = os.environ.get("FAKE_PROMPT_ERROR", "")
        if err:
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32000, "message": err}}) + "\n")
            sys.stdout.flush()
            continue
        if os.environ.get("FAKE_PROMPT_HANG"):
            time.sleep(1000)
            continue
        if os.environ.get("FAKE_PROMPT_EXIT"):
            sys.exit(0)
        if os.environ.get("FAKE_PROMPT_MALFORMED"):
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg["id"]}) + "\n")
            sys.stdout.flush()
            continue
        time.sleep(delay)
        reply(msg["id"], {"stopReason": "end_turn"})
"#;

fn write_fake(dir: &Path) -> PathBuf {
    let path = dir.join("fake_acp.py");
    std::fs::write(&path, FAKE_ACP).unwrap();
    path
}

fn seen_methods(path: &Path) -> Vec<String> {
    std::fs::read_to_string(path)
        .unwrap_or_default()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .filter_map(|v| v["method"].as_str().map(str::to_string))
        .collect()
}

fn join_config(dir: &Path, yolo: &str) -> (JoinConfig, PathBuf) {
    let fake = write_fake(dir);
    let log = dir.join("leader.log");
    let seen = dir.join("seen.jsonl");
    let flag = dir.join("env.flag");
    let sock = dir.join("s3.sock");
    std::fs::write(&log, "").unwrap();
    let config = JoinConfig {
        bin: "python3".into(),
        args: vec![fake.display().to_string()],
        cwd: dir.to_path_buf(),
        leader_socket: sock.clone(),
        leader_log: log,
        session_id: "sess-1".into(),
        timeout: Duration::from_secs(3),
        extra_env: vec![
            (
                "FAKE_LEADER_LOG".into(),
                dir.join("leader.log").display().to_string(),
            ),
            ("FAKE_SEEN".into(), seen.display().to_string()),
            ("FAKE_ENV_FLAG".into(), flag.display().to_string()),
            ("FAKE_YOLO".into(), yolo.into()),
            ("FAKE_SOCK".into(), sock.display().to_string()),
            ("FAKE_SESSION".into(), "sess-1".into()),
            ("CLAUDE_CODE_MESSAGING_TOKEN".into(), "redact-me".into()),
        ],
    };
    (config, seen)
}

// Expectation: status mapping reads activity, prompt_complete, permission
// requests and agent text without recording approval contents. Why: Grok's
// waiting/busy/idle pulse is how the registry tells the truth. Value: M6 can
// wire mailbox.set_activity from a pure function.
#[test]
fn interpret_grok_status_notifications() {
    let busy = interpret_notification(
        "sess-1",
        &json!({"method":"_x.ai/sessions/changed","params":{"upserted":[{"sessionId":"sess-1","activity":"working"}]}}),
    );
    assert_eq!(busy, Some(StatusEvent::Busy));
    let idle = interpret_notification(
        "sess-1",
        &json!({"method":"_x.ai/session/prompt_complete","params":{"sessionId":"sess-1"}}),
    );
    assert_eq!(idle, Some(StatusEvent::Idle));
    let wait = interpret_notification(
        "sess-1",
        &json!({"id":7,"method":"session/request_permission","params":{"sessionId":"sess-1","toolCall":{"secret":"no"}}}),
    );
    assert_eq!(wait, Some(StatusEvent::WaitingApproval));
    let detail = interpret_notification(
        "sess-1",
        &json!({"method":"session/update","params":{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}),
    );
    assert_eq!(detail, Some(StatusEvent::Detail("hi".into())));
    assert_eq!(
        interpret_notification(
            "sess-1",
            &json!({"method":"_x.ai/sessions/changed","params":{"upserted":[{"sessionId":"other","activity":"working"}]}}),
        ),
        None
    );
    let idle_activity = interpret_notification(
        "sess-1",
        &json!({"method":"_x.ai/sessions/changed","params":{"upserted":[{"sessionId":"sess-1","activity":"idle"}]}}),
    );
    assert_eq!(idle_activity, Some(StatusEvent::Idle));
    assert_eq!(
        interpret_notification(
            "sess-1",
            &json!({"method":"_x.ai/sessions/changed","params":{"upserted":[{"sessionId":"sess-1","activity":"mystery"}]}}),
        ),
        Some(StatusEvent::Unknown)
    );
    assert_eq!(
        interpret_notification(
            "sess-1",
            &json!({"method":"_x.ai/sessions/changed","params":{"upserted":[{"sessionId":"sess-1"}]}}),
        ),
        Some(StatusEvent::Unknown)
    );
    assert_eq!(
        interpret_notification(
            "sess-1",
            &json!({"id":8,"method":"session/request_permission","params":{"sessionId":"other","toolCall":{"secret":"no"}}}),
        ),
        None
    );
    assert_eq!(
        interpret_notification(
            "sess-1",
            &json!({"id":9,"method":"session/request_permission","params":{"toolCall":{"secret":"no"}}}),
        ),
        None
    );
}

// Expectation: yolo_mode=true after initialize aborts before session/load.
// Why: a joining client that registers yolo_mode=true would SetYoloMode on the
// shared session. Value: the TUI's permission mode stays the person's.
#[test]
fn join_refuses_yolo_mode_before_load() {
    let tmp = tmp();
    let (config, seen) = join_config(tmp.path(), "true");
    let err = LeaderClient::join(config).err().expect("join must fail");
    assert!(err.to_string().contains("yolo_mode=false"), "{err}");
    assert!(
        !seen_methods(&seen).contains(&"session/load".to_string()),
        "session/load must not run: {:?}",
        seen_methods(&seen)
    );
}

// Expectation: yolo_mode=false lets session/load through with yoloMode and autoMode false,
// CLAUDE_CODE_MESSAGING_* stripped, and loadSession required. Why: the proof
// loaded only after the leader logged the joining client. Value: offline join
// is the same gate live join will use.
#[test]
fn join_loads_after_observed_yolo_mode_false() {
    let tmp = tmp();
    let (config, seen) = join_config(tmp.path(), "false");
    let env_flag = tmp.path().join("env.flag");
    let client = LeaderClient::join(config).expect("join");
    let methods = seen_methods(&seen);
    assert!(methods.contains(&"initialize".to_string()), "{methods:?}");
    assert!(methods.contains(&"session/load".to_string()), "{methods:?}");
    let load = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .find(|v| v["method"] == "session/load")
        .unwrap();
    assert_eq!(load["params"]["_meta"]["yoloMode"], false);
    // Installed 1.0.77 falls back to the agent's auto default when this field is
    // absent. Reconnecting must not enable either permission mode on the session.
    assert_eq!(load["params"]["_meta"]["autoMode"], false);
    assert_eq!(load["params"]["sessionId"], "sess-1");
    assert_eq!(std::fs::read_to_string(&env_flag).unwrap(), "absent");
    let done = client.prompt("hello", Duration::from_secs(2)).unwrap();
    assert_eq!(done["stopReason"], "end_turn");
}

// Expectation: while the leader definitely refuses session/load, the join asks again until the
// session is there, and one that never appears ends the join with the leader's reason. Why: the
// Grok CLI creates its session a moment after it starts its leader, so a bridge that starts with
// it reaches the leader first; a refused load loads nothing, so asking again changes nothing.
// Value: a Grok agent's first launch gets its mailbox without being opened a second time.
#[test]
fn join_asks_again_while_the_native_session_is_not_there_yet() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_LOAD_REFUSALS".into(), "2".into()));
    let client = LeaderClient::join(config).expect("the join waits for the session");
    let loads = seen_methods(&seen)
        .iter()
        .filter(|method| *method == "session/load")
        .count();
    assert_eq!(loads, 3, "two refused loads, then the one that joined");
    let done = client.prompt("hello", Duration::from_secs(2)).unwrap();
    assert_eq!(done["stopReason"], "end_turn");

    let tmp = self::tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config.timeout = Duration::from_secs(1);
    config
        .extra_env
        .push(("FAKE_LOAD_REFUSALS".into(), "always".into()));
    let started = std::time::Instant::now();
    let error = LeaderClient::join(config)
        .err()
        .expect("a session that never appears cannot be joined");
    assert!(error.to_string().contains("Path not found"), "{error}");
    assert!(
        started.elapsed() < Duration::from_secs(4),
        "the wait is bounded"
    );
    let loads = seen_methods(&seen)
        .iter()
        .filter(|method| *method == "session/load")
        .count();
    assert!(loads >= 2, "the load was asked again: {loads}");
}

// Expectation: two session/prompt calls run one at a time. Why: ACP prompt
// blocks until end_turn; overlapping prompts corrupt the shared session.
// Value: letters queue instead of racing.
#[test]
fn relay_serializes_prompts() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_DELAY".into(), "0.25".into()));
    let client = LeaderClient::join(config).unwrap();
    let started = std::time::Instant::now();
    std::thread::scope(|scope| {
        scope.spawn(|| client.prompt("one", Duration::from_secs(3)).unwrap());
        scope.spawn(|| {
            std::thread::sleep(Duration::from_millis(30));
            client.prompt("two", Duration::from_secs(3)).unwrap();
        });
    });
    assert!(
        started.elapsed() >= Duration::from_millis(450),
        "prompts overlapped: {:?}",
        started.elapsed()
    );
    let prompts: Vec<_> = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .filter(|v| v["method"] == "session/prompt")
        .collect();
    assert_eq!(prompts.len(), 2);
}

// Expectation: session/request_permission is observed as waiting and never
// answered. Why: approvals belong to the person at the TUI. Value: the bridge
// cannot click Allow.
#[test]
fn permission_request_is_never_answered() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PERMISSION".into(), "1".into()));
    let client = LeaderClient::join(config).unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(2);
    let mut saw_wait = false;
    while std::time::Instant::now() < deadline {
        while let Some(msg) = client.poll_notification() {
            if interpret_notification("sess-1", &msg) == Some(StatusEvent::WaitingApproval) {
                saw_wait = true;
            }
        }
        if saw_wait {
            break;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(saw_wait, "permission request never observed");
    std::thread::sleep(Duration::from_millis(100));
    let inbound: Vec<_> = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .collect();
    assert!(
        inbound.iter().all(|v| v["id"] != 99),
        "bridge answered a permission request: {inbound:?}"
    );
}

#[derive(Default)]
struct RecordingTransport {
    sent: Mutex<Vec<(String, Frame)>>,
}

impl Transport for RecordingTransport {
    fn send(&self, to: &MailPeer, frame: &Frame) -> Result<(), taskai_core::Error> {
        self.sent
            .lock()
            .unwrap()
            .push((to.name.clone(), frame.clone()));
        SocketTransport.send(to, frame)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct RecordedActivity {
    activity: Activity,
    waiting_for: Option<String>,
}

#[derive(Default)]
struct TestDirectory {
    entries: Mutex<HashMap<String, MailPeer>>,
    activity: Mutex<Vec<RecordedActivity>>,
}

impl Directory for TestDirectory {
    fn live_sessions(&self) -> Result<Vec<MailPeer>, taskai_core::Error> {
        Ok(self.entries.lock().unwrap().values().cloned().collect())
    }
    fn register(&self, entry: &MailPeer) -> Result<(), taskai_core::Error> {
        self.entries
            .lock()
            .unwrap()
            .insert(entry.session_ref.clone(), entry.clone());
        Ok(())
    }
    fn set_status(
        &self,
        session_ref: &str,
        status: Activity,
        waiting_for: Option<&str>,
    ) -> Result<(), taskai_core::Error> {
        if let Some(entry) = self.entries.lock().unwrap().get_mut(session_ref) {
            entry.status = status;
            entry.waiting_for = waiting_for.map(str::to_owned);
        }
        if session_ref == "s3" {
            self.activity.lock().unwrap().push(RecordedActivity {
                activity: status,
                waiting_for: waiting_for.map(str::to_owned),
            });
        }
        Ok(())
    }
    fn unregister(&self, session_ref: &str) -> Result<(), taskai_core::Error> {
        self.entries.lock().unwrap().remove(session_ref);
        Ok(())
    }
}

struct MailboxFixture {
    mailbox: Arc<Mailbox>,
    directory: Arc<TestDirectory>,
    transport: Arc<RecordingTransport>,
    deliveries: mpsc::Receiver<Delivery>,
    peer: Mailbox,
}

impl MailboxFixture {
    fn new(dir: &Path) -> Self {
        let directory = Arc::new(TestDirectory::default());
        let transport = Arc::new(RecordingTransport::default());
        let peer_config = grok_config(
            dir,
            Identity {
                session_ref: "s2".into(),
                task_ref: "t1".into(),
                name: "codex-lead".into(),
                harness: "codex".into(),
                workspace: dir.join("ws"),
                mode: MailMode::Prompting,
            },
        );
        let peer = Mailbox::open(
            peer_config,
            directory.clone(),
            Arc::new(SocketTransport),
            |_| {},
        )
        .unwrap();
        let (tx, deliveries) = mpsc::channel();
        let mailbox = Arc::new(
            Mailbox::open(
                grok_config(dir, grok_identity(dir)),
                directory.clone(),
                transport.clone(),
                move |delivery| {
                    tx.send(delivery).unwrap();
                },
            )
            .unwrap(),
        );
        Self {
            mailbox,
            directory,
            transport,
            deliveries,
            peer,
        }
    }

    // Frames traverse the real authenticated socket; Delivery.id is assigned by core.
    fn enqueue(&self, requested: &Delivery) -> Delivery {
        let frame = match requested.kind {
            DeliveryKind::Message => Frame::Message(Letter {
                msg_id: requested.msg_id.clone(),
                from: "codex-lead".into(),
                from_mode: MailMode::Prompting,
                hop_chain: requested.chain.clone().unwrap_or_default(),
                priority: Priority::Next,
                body: requested.text.clone(),
            }),
            DeliveryKind::Notice => {
                assert_eq!(
                    self.mailbox.subscribe("codex-lead"),
                    SubscriptionResult::Subscribed
                );
                let subscription = self
                    .transport
                    .sent
                    .lock()
                    .unwrap()
                    .iter()
                    .find_map(|(_, frame)| {
                        if let Frame::Subscribe(s) = frame {
                            Some(s.msg_id.clone())
                        } else {
                            None
                        }
                    })
                    .unwrap();
                Frame::IdleNotice(IdleNotice {
                    orig_msg_id: subscription,
                    state: NoticeState::Idle,
                    finished_at: None,
                    detail: None,
                })
            }
            DeliveryKind::Receipt => panic!("receipt setup requires a send correlation"),
        };
        let recipient = self.directory.entries.lock().unwrap()["s3"].clone();
        SocketTransport.send(&recipient, &frame).unwrap();
        let mut delivery = self
            .deliveries
            .recv_timeout(Duration::from_secs(2))
            .unwrap();
        assert_eq!(delivery.kind, requested.kind);
        if requested.kind == DeliveryKind::Message {
            assert_eq!(delivery.msg_id, requested.msg_id);
            assert_eq!(
                delivery.chain.as_deref(),
                Some(requested.chain.as_deref().unwrap_or_default())
            );
        } else {
            // Deliberately poison this field to prove a notice never starts a peer chain.
            delivery.chain = requested.chain.clone();
            self.transport.sent.lock().unwrap().clear();
        }
        delivery
    }

    fn letters(&self) -> Vec<(String, Letter)> {
        self.transport
            .sent
            .lock()
            .unwrap()
            .iter()
            .filter_map(|(to, frame)| {
                if let Frame::Message(letter) = frame {
                    Some((to.clone(), letter.clone()))
                } else {
                    None
                }
            })
            .collect()
    }

    fn receipts(&self) -> Vec<Receipt> {
        self.transport
            .sent
            .lock()
            .unwrap()
            .iter()
            .filter_map(|(_, frame)| {
                if let Frame::Receipt(receipt) = frame {
                    Some(receipt.clone())
                } else {
                    None
                }
            })
            .collect()
    }

    // A real failure probe proves whether the exact delivery ID still owns its slot.
    // It runs only after assertions about the adapter's result, as fixture teardown.
    fn assert_queued(&self, delivery: &Delivery) {
        assert!(
            self.receipts().is_empty(),
            "uncertain delivery emitted a receipt"
        );
        self.mailbox
            .delivery_failed(&delivery.id, "fixture queue probe");
        let receipts = self.receipts();
        assert_eq!(
            receipts.len(),
            1,
            "the delivery slot was prematurely released"
        );
        assert_eq!(receipts[0].orig_msg_id, delivery.msg_id);
        assert_eq!(receipts[0].drop_reason.as_deref(), Some("delivery-failed"));
    }

    fn assert_accepted(&self, deliveries: &[Delivery]) {
        for delivery in deliveries {
            self.mailbox
                .delivery_failed(&delivery.id, "fixture queue probe");
        }
        self.mailbox.close();
        assert!(
            self.receipts().is_empty(),
            "an accepted delivery retained its queue slot"
        );
    }
}

impl Drop for MailboxFixture {
    fn drop(&mut self) {
        self.mailbox.close();
        self.peer.close();
    }
}

fn grok_identity(dir: &Path) -> Identity {
    Identity {
        session_ref: "s3".into(),
        task_ref: "t1".into(),
        name: "grok-lead".into(),
        harness: "grok".into(),
        workspace: dir.join("ws"),
        mode: MailMode::Prompting,
    }
}

fn grok_config(dir: &Path, identity: Identity) -> MailboxConfig {
    MailboxConfig {
        identity,
        mail_dir: dir.join("mail"),
        log_path: dir.join("messages.jsonl"),
    }
}

fn wait_for(mut condition: impl FnMut() -> bool) {
    let deadline = std::time::Instant::now() + Duration::from_secs(3);
    while !condition() {
        assert!(
            std::time::Instant::now() < deadline,
            "condition was not observed"
        );
        std::thread::sleep(Duration::from_millis(10));
    }
}

fn wait_prompt(seen: &Path) {
    wait_for(|| {
        seen_methods(seen)
            .iter()
            .any(|method| method == "session/prompt")
    });
}

fn sample_peer(identity: &Identity) -> MailPeer {
    MailPeer {
        session_ref: identity.session_ref.clone(),
        task_ref: identity.task_ref.clone(),
        name: identity.name.clone(),
        harness: identity.harness.clone(),
        workspace: identity.workspace.clone(),
        pid: 4242,
        proc_start: "Thu Jan  1 00:00:00 2026".into(),
        status: Activity::Idle,
        waiting_for: None,
        mailbox: PathBuf::from("/tmp/t5/mail/s3.sock"),
        features: vec!["notify_idle".into()],
        mode: Some(identity.mode),
    }
}

// Expectation: Identity holds only caller-known fields; MailPeer is the
// directory entry the mailbox fills with pid, procStart, socket, features
// and status. Why: M1's open config must not require process fields the
// adapter cannot know. Value: Grok constructs Identity; M2 supplies MailPeer.
#[test]
fn identity_is_separate_from_mail_peer() {
    let tmp = tmp();
    let identity = grok_identity(tmp.path());
    assert_eq!(identity.harness, "grok");
    assert_eq!(identity.session_ref, "s3");
    assert_eq!(identity.task_ref, "t1");
    assert_eq!(identity.name, "grok-lead");
    let peer = sample_peer(&identity);
    assert_eq!(peer.pid, 4242);
    assert_eq!(peer.proc_start, "Thu Jan  1 00:00:00 2026");
    assert_eq!(peer.mailbox, PathBuf::from("/tmp/t5/mail/s3.sock"));
    assert_eq!(peer.features, ["notify_idle"]);
    assert_eq!(peer.status, Activity::Idle);
    let _: &Identity = &identity;
    let _: &MailPeer = &peer;
}

// Expectation: Mailbox::open takes an explicit Transport plus Identity in
// MailboxConfig. Why: Claude routing is injected; Grok never defaults a
// transport or treats Identity as a directory peer. Value: M6 can pass
// SocketTransport and the production core Mailbox without a compatibility trait.
#[test]
fn mailbox_open_takes_explicit_transport() {
    let tmp = tmp();
    let fixture = MailboxFixture::new(tmp.path());
    let peer = fixture.directory.entries.lock().unwrap()["s3"].clone();
    assert_eq!(peer.session_ref, "s3");
    assert_eq!(peer.harness, "grok");
    assert_eq!(peer.pid, std::process::id());
    assert!(fixture.transport.sent.lock().unwrap().is_empty());
    assert_eq!(
        fixture
            .mailbox
            .send("codex-lead", "through transport", false, None)
            .status,
        taskai_core::mail::SendStatus::Sent
    );
    let sends = fixture.letters();
    assert_eq!(sends.len(), 1);
    assert_eq!(sends[0].0, "codex-lead");
    assert_eq!(sends[0].1.body, "through transport");
}

// Expectation: list_sessions and send_message delegate to Mailbox. Extra
// from / from_mode / hop_chain keys are rejected before dispatch. Why: tool
// dispatch never accepts caller-supplied sender identity, permission mode or
// hop chain. Value: the native turn's chain is the only chain a send continues.
#[test]
fn tool_dispatch_delegates_to_mailbox_without_caller_identity() {
    let tmp = tmp();
    let fixture = MailboxFixture::new(tmp.path());
    let tools = GrokTools::new(Arc::clone(&fixture.mailbox));
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "owned-session", tools).unwrap();
    let listed = call_tool(&path, "owned-session", "list_sessions", json!({}));
    assert_eq!(listed["status"], "ok");
    assert_eq!(listed["sessions"][0]["name"], "codex-lead");
    assert_eq!(listed["sessions"][0]["waitingFor"], serde_json::Value::Null);
    assert_eq!(listed["sessions"].as_array().unwrap().len(), 1);
    let extra_list = call_tool(&path, "owned-session", "list_sessions", json!({"from":"x"}));
    assert_eq!(extra_list["status"], "error");
    assert!(fixture.letters().is_empty());
    let extra_send = call_tool(
        &path,
        "owned-session",
        "send_message",
        json!({
            "to":"codex-lead", "text":"hello", "notify_when_idle":true,
            "from":"imposter", "from_mode":"bypass", "hop_chain":["evil"]
        }),
    );
    assert_eq!(extra_send["status"], "error");
    assert!(fixture.letters().is_empty());
    let sent = call_tool(
        &path,
        "owned-session",
        "send_message",
        json!({"to":"codex-lead", "text":"hello", "notify_when_idle":true}),
    );
    assert_eq!(sent["status"], "sent");
    assert_eq!(sent["to"], "codex-lead");
    assert_eq!(sent["idle_notice"], "subscribed");
    let sends = fixture.letters();
    assert_eq!(sends.len(), 1);
    assert_eq!(sends[0].0, "codex-lead");
    assert_eq!(sends[0].1.body, "hello");
    assert_eq!(sent["msg_id"], sends[0].1.msg_id);
    assert_eq!(sends[0].1.from, "grok-lead");
    assert_eq!(sends[0].1.from_mode, MailMode::Prompting);
    assert!(sends[0].1.hop_chain.is_empty());
    assert!(fixture
        .transport
        .sent
        .lock()
        .unwrap()
        .iter()
        .any(|(_, frame)| matches!(frame, Frame::Subscribe(_))));
}

// Expectation: a Message delivery holds its chain for the serialized
// session/prompt, and turn_started(Delivery.id) runs after the provider
// accepts. Why: M1 turn_started removes queued[id]; a send during the
// native turn continues that chain. Value: a rejected prompt can still
// delivery_failed, and Grok does not invent a chain.
#[test]
fn message_delivery_completes_by_id_and_carries_chain() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_DELAY".into(), "0.25".into()));
    let client = LeaderClient::join(config).unwrap();
    let (fixture, bridge) = mailbox_bridge(&tmp, client);
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", bridge.tools()).unwrap();
    let delivery = fixture.enqueue(&message_delivery(
        "d-letter",
        "please look",
        Some(vec!["tok1".into()]),
    ));
    let watcher = {
        let path = path.clone();
        let seen = seen.clone();
        std::thread::spawn(move || {
            wait_prompt(&seen);
            call_tool(
                &path,
                "sess-1",
                "send_message",
                json!({"to":"codex-lead", "text":"reply"}),
            )
        })
    };
    bridge
        .deliver(delivery.clone(), Duration::from_secs(3))
        .unwrap();
    assert_eq!(watcher.join().unwrap()["status"], "sent");
    let sends = fixture.letters();
    assert_eq!(sends.len(), 1);
    assert_eq!(
        sends[0].1.hop_chain,
        vec!["tok1".to_owned(), fixture.mailbox.hop_token().to_owned()]
    );
    let prompt = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .find(|v| v["method"] == "session/prompt")
        .unwrap();
    assert_eq!(
        prompt["params"]["prompt"][0]["text"], delivery.text,
        "relay changed the core envelope"
    );
    fixture.assert_accepted(&[delivery]);
}

// Expectation: a Notice delivery starts no peer chain, even if a chain
// field is present on the struct. Why: notices are not instructions.
// Value: a status pulse cannot extend a hop chain.
#[test]
fn notice_delivery_starts_no_chain() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_DELAY".into(), "0.25".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", bridge.tools()).unwrap();
    let delivery = fixture.enqueue(&Delivery {
        id: "d-notice".into(),
        kind: DeliveryKind::Notice,
        text: "status".into(),
        chain: Some(vec!["should-not-use".into()]),
        msg_id: "m-sub".into(),
        sender: "TaskAI".into(),
    });
    let watcher = {
        let path = path.clone();
        std::thread::spawn(move || {
            wait_prompt(&seen);
            call_tool(
                &path,
                "sess-1",
                "send_message",
                json!({"to":"codex-lead", "text":"during-notice"}),
            )
        })
    };
    bridge.deliver(delivery, Duration::from_secs(3)).unwrap();
    assert_eq!(watcher.join().unwrap()["status"], "sent");
    let sends = fixture.letters();
    assert_eq!(sends.len(), 1);
    assert!(sends[0].1.hop_chain.is_empty());
    fixture
        .mailbox
        .accept_authenticated(Frame::Subscribe(Subscription {
            msg_id: "notice-completion".into(),
            from: "codex-lead".into(),
            from_mode: MailMode::Prompting,
        }));
    bridge.tools().apply_status(StatusEvent::Idle);
    wait_for(|| {
        fixture.transport.sent.lock().unwrap().iter().any(|(_, frame)| matches!(frame, Frame::IdleNotice(n) if n.orig_msg_id == "notice-completion" && n.state == NoticeState::Idle))
    });
}

// Expectation: a rejected session/prompt calls delivery_failed(id) without
// turn_started, while the id is still queued. Why: M1 turn_started only
// removes queued[id], so a prior start makes failure a no-op. Value: the
// sender gets a dropped receipt from the mailbox, once.
#[test]
fn prompt_error_fails_delivery_by_id() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_ERROR".into(), "leader rejected prompt".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let delivery = fixture.enqueue(&message_delivery("d-fail", "go", Some(vec!["tok".into()])));
    let err = bridge
        .deliver(delivery.clone(), Duration::from_secs(3))
        .expect_err("prompt must fail");
    assert!(err.to_string().contains("leader rejected prompt"), "{err}");
    let failed = fixture.receipts();
    assert_eq!(
        failed.len(),
        1,
        "accepting before rejection loses failure correlation"
    );
    assert_eq!(failed[0].orig_msg_id, delivery.msg_id);
    assert_eq!(failed[0].status, ReceiptStatus::Dropped);
    assert_eq!(failed[0].drop_reason.as_deref(), Some("delivery-failed"));
    assert!(failed[0]
        .reason
        .as_deref()
        .unwrap()
        .contains("leader rejected prompt"));
    fixture
        .mailbox
        .delivery_failed(&delivery.id, "duplicate rejection");
    fixture.mailbox.close();
    assert_eq!(
        fixture.receipts(),
        failed,
        "duplicate completion must not emit another receipt"
    );
    assert!(seen_methods(&seen).contains(&"session/prompt".to_owned()));
}

// Expectation: Grok status pulses become Mailbox set_activity / set_detail.
// Why: the harness reports its own status; an admitted delivery is busy
// until the harness says otherwise. Value: M6 wires notifications without
// Grok talking to the registry.
#[test]
fn status_events_go_to_mailbox() {
    let tmp = tmp();
    let fixture = MailboxFixture::new(tmp.path());
    let tools = GrokTools::new(Arc::clone(&fixture.mailbox));
    tools.apply_status(StatusEvent::Busy);
    tools.apply_status(StatusEvent::WaitingApproval);
    tools.apply_status(StatusEvent::Detail("working on it".into()));
    tools.apply_status(StatusEvent::Unknown);
    tools.apply_status(StatusEvent::Idle);
    assert_eq!(
        *fixture.directory.activity.lock().unwrap(),
        vec![
            RecordedActivity {
                activity: Activity::Busy,
                waiting_for: None
            },
            RecordedActivity {
                activity: Activity::Waiting,
                waiting_for: Some("approval".into())
            },
            RecordedActivity {
                activity: Activity::Idle,
                waiting_for: None
            },
        ]
    );
    fixture
        .mailbox
        .accept_authenticated(Frame::Subscribe(Subscription {
            msg_id: "status-detail".into(),
            from: "codex-lead".into(),
            from_mode: MailMode::Prompting,
        }));
    wait_for(|| {
        fixture.transport.sent.lock().unwrap().iter().any(|(_, frame)| matches!(frame, Frame::IdleNotice(n) if n.orig_msg_id == "status-detail" && n.detail.as_deref() == Some("working on it") && n.state == NoticeState::Idle))
    });
}

// Expectation: drop shuts the accept thread down, releases the handler, and
// lets a later server bind the same path. Why: a cloned listener left in
// accept retains the mailbox after close. Value: close is a real lifetime.
#[test]
fn tool_server_drop_releases_handler_and_rebinding() {
    let tmp = tmp();
    let path = tmp.path().join("t.sock");
    let dropped = Arc::new(AtomicBool::new(false));
    let server = ToolServer::listen(
        &path,
        "owned-session",
        DropFlag {
            dropped: Arc::clone(&dropped),
            inner: StubTools::new(),
        },
    )
    .unwrap();
    drop(server);
    let deadline = std::time::Instant::now() + Duration::from_secs(2);
    while !dropped.load(Ordering::SeqCst) && std::time::Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert!(dropped.load(Ordering::SeqCst), "handler still held");
    assert_eq!(
        call_tool(&path, "owned-session", "list_sessions", json!({}))["status"],
        "bridge-unavailable"
    );
    let again = ToolServer::listen(&path, "owned-session", StubTools::new()).unwrap();
    let listed = call_tool(&path, "owned-session", "list_sessions", json!({}));
    assert_eq!(listed["status"], "ok");
    drop(again);
}

// Expectation: listen fails when the path is already a live socket or an
// ordinary file, and leaves that path intact. Why: unlinking a foreign
// socket or file is a data loss. Value: lifecycle owns stale cleanup.
#[test]
fn tool_server_refuses_existing_socket_and_file() {
    let tmp = tmp();
    let live = tmp.path().join("live.sock");
    let first = ToolServer::listen(&live, "owned-session", StubTools::new()).unwrap();
    let err = ToolServer::listen(&live, "owned-session", StubTools::new())
        .err()
        .expect("live socket must refuse");
    assert!(err.to_string().contains("already exists"), "{err}");
    assert_eq!(
        call_tool(&live, "owned-session", "list_sessions", json!({}))["status"],
        "ok"
    );
    drop(first);

    let file = tmp.path().join("file.sock");
    std::fs::write(&file, "not a socket").unwrap();
    let err = ToolServer::listen(&file, "owned-session", StubTools::new())
        .err()
        .expect("ordinary file must refuse");
    assert!(err.to_string().contains("already exists"), "{err}");
    assert_eq!(std::fs::read_to_string(&file).unwrap(), "not a socket");
}

// Expectation: drop unlinks only the inode this server bound. Why: a
// replacement server at the same path must survive the previous drop.
// Value: close cannot steal another process's socket.
#[test]
fn tool_server_drop_does_not_unlink_replaced_socket() {
    let tmp = tmp();
    let path = tmp.path().join("t.sock");
    let first = ToolServer::listen(&path, "first", StubTools::new()).unwrap();
    std::fs::remove_file(&path).unwrap();
    let second = ToolServer::listen(&path, "second", StubTools::new()).unwrap();
    drop(first);
    assert!(path.exists());
    assert_eq!(
        call_tool(&path, "second", "list_sessions", json!({}))["status"],
        "ok"
    );
    drop(second);
}

// Expectation: oversized or unterminated input is closed without dispatch,
// and send_message / list_sessions validate types before calling the
// handler. Why: unbounded read_line is a memory hazard; missing to/text
// must not become a successful send. Value: the agent sees an error.
#[test]
fn tool_socket_bounds_reads_and_validates_args() {
    let tmp = tmp();
    let path = tmp.path().join("t.sock");
    let handler = StubTools::new();
    let _server = ToolServer::listen(&path, "owned-session", handler).unwrap();

    let mut huge = UnixStream::connect(&path).unwrap();
    let _ = huge.set_read_timeout(Some(Duration::from_secs(2)));
    huge.write_all(&vec![b'x'; MAX_TOOL_LINE_BYTES + 1])
        .unwrap();
    let _ = huge.shutdown(std::net::Shutdown::Write);
    let mut reply = String::new();
    let _ = std::io::Read::read_to_string(&mut huge, &mut reply);
    assert!(reply.is_empty(), "oversized request was answered: {reply}");

    let mut partial = UnixStream::connect(&path).unwrap();
    let _ = partial.set_read_timeout(Some(Duration::from_secs(2)));
    partial
        .write_all(b"{\"session_id\":\"owned-session\",\"tool\":\"list_sessions\",\"args\":{}}")
        .unwrap();
    let _ = partial.shutdown(std::net::Shutdown::Write);
    let mut reply = String::new();
    let _ = std::io::Read::read_to_string(&mut partial, &mut reply);
    assert!(
        reply.is_empty(),
        "unterminated request was answered: {reply}"
    );

    assert_eq!(
        call_tool(
            &path,
            "owned-session",
            "send_message",
            json!({"to": "codex-lead"})
        )["status"],
        "error"
    );
    assert_eq!(
        call_tool(
            &path,
            "owned-session",
            "send_message",
            json!({"to": "codex-lead", "text": "hi", "notify_when_idle": "yes"})
        )["status"],
        "error"
    );
    assert_eq!(
        call_tool(
            &path,
            "owned-session",
            "send_message",
            json!({"text": "hi"})
        )["status"],
        "error"
    );
    assert_eq!(
        call_tool(
            &path,
            "owned-session",
            "send_message",
            json!({"to": "", "text": "hi"})
        )["status"],
        "error"
    );
}

// Expectation: a Mailbox list_sessions error is status error, not ok with
// an empty list. Why: M1's list_sessions is fallible; wrapping failure as
// ok hides a registry outage. Value: the agent is told the truth.
#[test]
fn list_sessions_error_is_not_status_ok() {
    let tmp = tmp();
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "owned-session", FailList).unwrap();
    let listed = call_tool(&path, "owned-session", "list_sessions", json!({}));
    assert_eq!(listed["status"], "error");
    assert_eq!(listed["error"], "registry locked");
    assert!(listed.get("sessions").is_none());
}

// Expectation: install keeps unrelated taskai keys, env entries, timeouts
// and comments, and strips a stored TASKAI_TOOL_SOCKET. Why: TaskAI owns
// command and args; the socket comes from pane env. Value: a user's
// timeout and comments survive, and two agents do not clobber sockets.
#[test]
fn mcp_install_preserves_taskai_env_timeout_and_comments() {
    let tmp = tmp();
    let ws = tmp.path().join("ws");
    std::fs::create_dir_all(ws.join(".grok")).unwrap();
    std::fs::write(
        ws.join(".grok/config.toml"),
        "[mcp_servers.taskai]\n# keep this comment\ncommand = \"old\"\ntimeout = 12\n\n[mcp_servers.taskai.env]\nCUSTOM = \"keep\"\nTASKAI_TOOL_SOCKET = \"old.sock\"\n",
    )
    .unwrap();
    install_mcp(&ws, &PathBuf::from("/tmp/taskai-bin")).unwrap();
    let body = std::fs::read_to_string(ws.join(".grok/config.toml")).unwrap();
    assert!(body.contains("keep this comment"), "{body}");
    assert!(body.contains("timeout = 12"), "{body}");
    assert!(body.contains("CUSTOM") && body.contains("keep"), "{body}");
    assert!(
        !body.contains("TASKAI_TOOL_SOCKET") && !body.contains("old.sock"),
        "stored socket override must be removed: {body}"
    );
    assert!(body.contains("/tmp/taskai-bin"), "{body}");
}

// Expectation: an inline taskai server table is merged in place. Why: some
// project files store servers inline. Value: timeout and extra env survive
// without converting the table.
#[test]
fn mcp_install_preserves_inline_taskai_table() {
    let tmp = tmp();
    let ws = tmp.path().join("ws");
    std::fs::create_dir_all(ws.join(".grok")).unwrap();
    std::fs::write(
        ws.join(".grok/config.toml"),
        "[mcp_servers]\ntaskai = { command = \"old\", timeout = 9, env = { CUSTOM = \"keep\", TASKAI_TOOL_SOCKET = \"old.sock\" } }\n",
    )
    .unwrap();
    install_mcp(&ws, &PathBuf::from("/tmp/taskai-bin")).unwrap();
    let body = std::fs::read_to_string(ws.join(".grok/config.toml")).unwrap();
    assert!(body.contains("timeout") && body.contains('9'), "{body}");
    assert!(body.contains("CUSTOM") && body.contains("keep"), "{body}");
    assert!(
        !body.contains("TASKAI_TOOL_SOCKET") && !body.contains("old.sock"),
        "inline stored socket override must be removed: {body}"
    );
}

// Expectation: a conflicting taskai schema fails and does not write. Why:
// a string or array is not a server table. Value: user config stays intact.
#[test]
fn mcp_install_rejects_conflicting_schema_without_write() {
    let tmp = tmp();
    let ws = tmp.path().join("ws");
    std::fs::create_dir_all(ws.join(".grok")).unwrap();
    let original = "[mcp_servers]\ntaskai = \"not-a-table\"\n";
    std::fs::write(ws.join(".grok/config.toml"), original).unwrap();
    let err = install_mcp(&ws, &PathBuf::from("/tmp/taskai-bin")).unwrap_err();
    assert!(err.to_string().contains("not a table"), "{err}");
    assert_eq!(
        std::fs::read_to_string(ws.join(".grok/config.toml")).unwrap(),
        original
    );
}

// Expectation: two agents sharing a workspace keep one static MCP entry and
// route through their own inherited sockets. Why: a stored socket made the
// last install win. Value: each GROK_SESSION_ID reaches only its ToolServer.
#[test]
fn two_agents_share_static_mcp_and_inherit_own_socket() {
    let tmp = tmp();
    let ws = tmp.path().join("ws");
    install_mcp(&ws, &PathBuf::from("/tmp/taskai-bin")).unwrap();
    let config = ws.join(".grok/config.toml");
    let body = std::fs::read_to_string(&config).unwrap();
    assert!(body.contains("[mcp_servers.taskai]"), "{body}");
    assert!(!body.contains("TASKAI_TOOL_SOCKET"), "{body}");
    install_mcp(&ws, &PathBuf::from("/tmp/taskai-bin")).unwrap();
    assert_eq!(std::fs::read_to_string(&config).unwrap(), body);

    let sock_a = tmp.path().join("a.tools.sock");
    let sock_b = tmp.path().join("b.tools.sock");
    let env_a = pane_env("sess-a", &sock_a);
    let env_b = pane_env("sess-b", &sock_b);
    assert_ne!(env_a, env_b);
    let _server_a = ToolServer::listen(&sock_a, "sess-a", StubTools::new()).unwrap();
    let _server_b = ToolServer::listen(&sock_b, "sess-b", StubTools::new()).unwrap();
    let listed_a = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}),
        Path::new(&env_a[1].1),
        &env_a[0].1,
    )
    .unwrap();
    let listed_b = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}),
        Path::new(&env_b[1].1),
        &env_b[0].1,
    )
    .unwrap();
    let payload_a: serde_json::Value =
        serde_json::from_str(listed_a["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    let payload_b: serde_json::Value =
        serde_json::from_str(listed_b["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(payload_a["status"], "ok");
    assert_eq!(payload_b["status"], "ok");
    let crossed = handle_mcp_message(
        &json!({"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}),
        Path::new(&env_a[1].1),
        &env_b[0].1,
    )
    .unwrap();
    let crossed_payload: serde_json::Value =
        serde_json::from_str(crossed["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(crossed_payload["status"], "refused-unknown-session");
    assert_eq!(std::fs::read_to_string(&config).unwrap(), body);
}

// Expectation: a timed-out session/prompt leaves the client unusable, so a
// second prompt cannot write while the first turn may still be running.
// Why: releasing the mutex on timeout overlaps an uncertain writer.
// Value: nothing is retried; the overlap is refused.
#[test]
fn prompt_timeout_makes_client_unusable_without_overlap() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config.timeout = Duration::from_millis(400);
    config
        .extra_env
        .push(("FAKE_PROMPT_HANG".into(), "1".into()));
    let client = LeaderClient::join(config).unwrap();
    let err = client
        .prompt("one", Duration::from_millis(200))
        .expect_err("prompt must time out");
    assert!(err.to_string().contains("no response"), "{err}");
    let err2 = client
        .prompt("two", Duration::from_millis(200))
        .expect_err("second prompt must not write");
    assert!(
        err2.to_string().contains("unusable") || err2.to_string().contains("no response"),
        "{err2}"
    );
    let prompts: Vec<_> = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .filter(|v| v["method"] == "session/prompt")
        .collect();
    assert_eq!(prompts.len(), 1, "overlapping prompt write: {prompts:?}");
}

// Expectation: registration matches this client's exact --leader-socket
// token, not a line that merely contains the path as a substring. Why: an
// older client or a longer socket path must not authorize session/load.
// Value: the yolo gate observes this spawn.
#[test]
fn join_ignores_socket_substring_in_older_log() {
    let tmp = tmp();
    let (config, seen) = join_config(tmp.path(), "false");
    let sock = config.leader_socket.display().to_string();
    std::fs::write(
        &config.leader_log,
        format!(
            "2026-11-11T03:13:03.960303Z  INFO Client registered client_id=9 client_type=grok --permission-mode default agent --leader --leader-socket {sock}-forged stdio mode=Stdio yolo_mode=false client_version=Some(\"1.0.77\")\n"
        ),
    )
    .unwrap();
    let client = LeaderClient::join(config).expect("join uses the spawned client line");
    assert!(seen_methods(&seen).contains(&"session/load".to_string()));
    drop(client);
}

// Expectation: a registration line whose --leader-socket token only
// contains the socket path as a substring does not authorize load.
// Why: the yolo gate is for this newly spawned client. Value: a longer
// or forged socket path cannot open the session.
#[test]
fn join_rejects_leader_socket_substring_match() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    let sock = config.leader_socket.display().to_string();
    for (key, value) in &mut config.extra_env {
        if key == "FAKE_SOCK" {
            *value = format!("{sock}-forged");
        }
    }
    let err = LeaderClient::join(config).err().expect("join must fail");
    assert!(err.to_string().contains("yolo_mode=false"), "{err}");
    assert!(
        !seen_methods(&seen).contains(&"session/load".to_string()),
        "session/load must not run: {:?}",
        seen_methods(&seen)
    );
}

// Expectation: a timed-out prompt is not delivery_failed. Why: the
// provider may have accepted the turn; the client is unusable instead.
// Value: the mailbox does not drop a delivery whose ack is uncertain.
#[test]
fn prompt_timeout_does_not_mark_delivery_failed() {
    let tmp = tmp();
    let (mut config, _) = join_config(tmp.path(), "false");
    config.timeout = Duration::from_millis(400);
    config
        .extra_env
        .push(("FAKE_PROMPT_HANG".into(), "1".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let delivery = fixture.enqueue(&message_delivery("d-hang", "one", None));
    let err = bridge
        .deliver(delivery.clone(), Duration::from_millis(200))
        .expect_err("prompt must time out");
    assert!(err.to_string().contains("no response"), "{err}");
    fixture.assert_queued(&delivery);
}

fn mailbox_bridge(tmp: &tempfile::TempDir, client: LeaderClient) -> (MailboxFixture, GrokBridge) {
    let fixture = MailboxFixture::new(tmp.path());
    let bridge = GrokBridge::new(Arc::clone(&fixture.mailbox), client);
    (fixture, bridge)
}

fn message_delivery(id: &str, text: &str, chain: Option<Vec<String>>) -> Delivery {
    Delivery {
        id: id.into(),
        kind: DeliveryKind::Message,
        text: text.into(),
        chain,
        msg_id: format!("m-{id}"),
        sender: "codex-lead".into(),
    }
}

// Expectation: a leader that reads session/prompt and exits without a reply
// does not turn_started or delivery_failed. Why: stdout EOF after a complete
// write is uncertain acceptance, not a JSON-RPC rejection. Value: M1 does
// not emit a dropped receipt for a prompt that may already be in Grok.
#[test]
fn prompt_eof_after_write_is_uncertain_acceptance() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_EXIT".into(), "1".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let delivery = fixture.enqueue(&message_delivery(
        "d-eof",
        "one",
        Some(vec!["tok-eof".into()]),
    ));
    let err = bridge
        .deliver(delivery.clone(), Duration::from_secs(3))
        .expect_err("eof must not look accepted");
    assert!(err.to_string().contains("stdout closed"), "{err}");
    assert!(fixture.receipts().is_empty());
    let later = message_delivery("d-later", "two", None);
    let err2 = bridge
        .deliver(later, Duration::from_millis(200))
        .expect_err("later prompt must not write");
    assert!(err2.to_string().contains("unusable"), "{err2}");
    assert_eq!(
        seen_methods(&seen)
            .iter()
            .filter(|method| *method == "session/prompt")
            .count(),
        1,
        "later prompt was written after eof"
    );
    fixture.assert_queued(&delivery);
}

// Expectation: observer timeout keeps the delivery chain on a later tool
// send. Why: prompt_with currently clears chain in after() even though the
// native turn can still be running and ToolServer stays up. Value: a late
// send_message does not look human-started; M6 keeps chain until a confirmed
// terminal outcome and refuses a second prompt.
#[test]
fn prompt_timeout_keeps_chain_for_tool_send() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config.timeout = Duration::from_millis(400);
    config
        .extra_env
        .push(("FAKE_PROMPT_HANG".into(), "1".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", bridge.tools()).unwrap();
    let delivery = fixture.enqueue(&message_delivery(
        "d-hang-chain",
        "one",
        Some(vec!["tok-hang".into()]),
    ));
    let err = bridge
        .deliver(delivery.clone(), Duration::from_millis(200))
        .expect_err("prompt must time out");
    assert!(err.to_string().contains("no response"), "{err}");
    let tool = call_tool(
        &path,
        "sess-1",
        "send_message",
        json!({"to":"codex-lead", "text":"late"}),
    );
    assert_eq!(tool["status"], "sent", "{tool}");
    let sends = fixture.letters();
    assert_eq!(sends.len(), 1, "{sends:?}");
    assert_eq!(
        sends[0].1.hop_chain,
        vec![
            "tok-hang".to_owned(),
            fixture.mailbox.hop_token().to_owned()
        ],
        "timeout cleared the in-flight chain"
    );
    assert_eq!(
        seen_methods(&seen)
            .iter()
            .filter(|method| *method == "session/prompt")
            .count(),
        1
    );
    fixture.assert_queued(&delivery);
}

// Expectation: a prompt reply that is neither result nor error is uncertain
// acceptance, not success or a dropped receipt. Why: malformed
// acknowledgement is not a confirmed terminal outcome. Value: M1 does not
// turn_started or delivery_failed on a junk ack.
#[test]
fn prompt_malformed_ack_is_uncertain_acceptance() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_MALFORMED".into(), "1".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let delivery = fixture.enqueue(&message_delivery(
        "d-bad-ack",
        "one",
        Some(vec!["tok-bad".into()]),
    ));
    let err = bridge
        .deliver(delivery.clone(), Duration::from_secs(3))
        .expect_err("malformed ack must not look accepted");
    assert!(err.to_string().contains("malformed"), "{err}");
    assert!(seen_methods(&seen).contains(&"session/prompt".to_owned()));
    fixture.assert_queued(&delivery);
}

fn assert_uncertain_prompt_reply(reply: serde_json::Value) {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_REPLY".into(), reply.to_string()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let delivery = fixture.enqueue(&message_delivery(
        "bad-ack",
        "one",
        Some(vec!["peer".into()]),
    ));
    let result = bridge.deliver(delivery.clone(), Duration::from_secs(3));
    assert!(
        matches!(result, Err(taskai_connectors::Error::Uncertain(_))),
        "{reply}: {result:?}"
    );
    assert_eq!(
        seen_methods(&seen)
            .iter()
            .filter(|m| *m == "session/prompt")
            .count(),
        1
    );
    fixture.assert_queued(&delivery);
}

// Expectation: only a complete RPC error proves rejection. Why: a malformed response can follow
// an accepted prompt. Value: partial errors keep real mailbox correlation and emit no false receipt.
#[test]
fn prompt_partial_rpc_error_is_uncertain() {
    for error in [
        json!({"code": -32000}),
        json!({"message": "bad"}),
        json!(null),
        json!({"code": "bad", "message": "bad"}),
    ] {
        assert_uncertain_prompt_reply(json!({"error": error}));
    }
}

#[test]
fn prompt_conflicting_rpc_error_and_result_is_uncertain() {
    assert_uncertain_prompt_reply(
        json!({"error": {"code": -32000, "message": "bad"}, "result": {"stopReason": "end_turn"}}),
    );
}

// Expectation: prompt completion has a usable stopReason, not merely a result key. Why: a null
// or malformed result cannot establish the terminal outcome. Value: the delivery stays uncertain.
#[test]
fn prompt_unusable_success_result_is_uncertain() {
    for result in [
        json!(null),
        json!({}),
        json!({"stopReason": false}),
        json!({"stopReason": ""}),
    ] {
        assert_uncertain_prompt_reply(json!({"result": result}));
    }
}

// Expectation: shutdown reports only unattempted queued letters as dropped.
// Why: a lost prompt acknowledgement does not prove that Grok rejected the turn.
// Value: cleanup cannot change an uncertain result into a false delivery receipt.
#[test]
fn close_after_uncertain_prompt_drops_only_unattempted_letters() {
    let tmp = tmp();
    let (mut config, _) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_EXIT".into(), "1".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let attempted = fixture.enqueue(&message_delivery("attempted", "one", None));
    let waiting = fixture.enqueue(&message_delivery("waiting", "two", None));
    bridge
        .deliver(attempted.clone(), Duration::from_secs(3))
        .unwrap_err();
    assert!(fixture.receipts().is_empty());
    fixture.mailbox.close();
    let receipts = fixture.receipts();
    assert_eq!(
        receipts.len(),
        1,
        "uncertain acceptance became a dropped receipt: {receipts:?}"
    );
    assert_eq!(receipts[0].orig_msg_id, waiting.msg_id);
    assert_eq!(receipts[0].drop_reason.as_deref(), Some("recipient-exited"));
    let log = std::fs::read_to_string(tmp.path().join("messages.jsonl")).unwrap();
    assert!(log
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .any(
            |entry| entry["msg_id"] == attempted.msg_id && entry["outcome"] == "delivery uncertain"
        ));
}

// Expectation: the same distinction holds while the prompt RPC is still pending.
// Why: native pane death can close the mailbox before the RPC returns an error.
// Value: marking an attempt before its write closes the shutdown race.
#[test]
fn close_while_prompt_is_in_flight_does_not_report_rejection() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_HANG".into(), "1".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let delivery = fixture.enqueue(&message_delivery("in-flight", "one", None));
    std::thread::scope(|scope| {
        let relay = scope.spawn(|| bridge.deliver(delivery, Duration::from_millis(400)));
        wait_prompt(&seen);
        fixture.mailbox.close();
        assert!(relay.join().unwrap().is_err());
    });
    assert!(
        fixture.receipts().is_empty(),
        "in-flight prompt was reported as rejected"
    );
}

// Expectation: drop cancels a connected partial request so finishing the
// line after close does not dispatch. Why: handle_conn owned the handler
// with no shutdown. Value: close joins in-flight work; a later server
// can still bind a replacement path.
#[test]
fn tool_server_drop_cancels_partial_connection() {
    let tmp = tmp();
    let path = tmp.path().join("t.sock");
    let dropped = Arc::new(AtomicBool::new(false));
    let listed = Arc::new(AtomicU32::new(0));
    let server = ToolServer::listen(
        &path,
        "owned-session",
        CountOnDrop {
            dropped: Arc::clone(&dropped),
            listed: Arc::clone(&listed),
        },
    )
    .unwrap();
    let mut partial = UnixStream::connect(&path).unwrap();
    partial
        .write_all(b"{\"session_id\":\"owned-session\",\"tool\":\"list_sessions\",\"args\":{}}")
        .unwrap();
    std::thread::sleep(Duration::from_millis(80));
    assert_eq!(listed.load(Ordering::SeqCst), 0);
    drop(server);
    let deadline = std::time::Instant::now() + Duration::from_secs(2);
    while !dropped.load(Ordering::SeqCst) && std::time::Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert!(dropped.load(Ordering::SeqCst), "handler still held");
    let _ = partial.write_all(b"\n");
    let _ = partial.flush();
    std::thread::sleep(Duration::from_millis(80));
    assert_eq!(
        listed.load(Ordering::SeqCst),
        0,
        "partial request dispatched after close"
    );
    assert_eq!(
        call_tool(&path, "owned-session", "list_sessions", json!({}))["status"],
        "bridge-unavailable"
    );
    let again = ToolServer::listen(&path, "owned-session", StubTools::new()).unwrap();
    assert_eq!(
        call_tool(&path, "owned-session", "list_sessions", json!({}))["status"],
        "ok"
    );
    drop(again);
}

// Expectation: atomic_write creates owner-only files, uses unique
// create_new staging, and concurrent writers leave one complete file.
// Why: File::create follows a pid-named temp and can chmod 0644 via umask.
// Value: workspace config stays private and untruncated.
#[test]
fn atomic_write_is_owner_only_unique_and_concurrent() {
    let tmp = tmp();
    let path = tmp.path().join("config.toml");
    let mut existing = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&path)
        .unwrap();
    existing.write_all(b"old = 1\n").unwrap();
    drop(existing);

    let planted = tmp
        .path()
        .join(format!(".config.toml.{}.tmp", std::process::id()));
    let evil = tmp.path().join("evil-target");
    std::os::unix::fs::symlink(&evil, &planted).unwrap();

    atomic_write(&path, "keep = true\ncustom = \"yes\"\n").unwrap();
    let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600, "final config is owner-only");
    let body = std::fs::read_to_string(&path).unwrap();
    assert!(body.contains("keep = true"), "{body}");
    assert!(body.contains("custom"), "{body}");
    assert!(!evil.exists(), "staging followed a planted symlink");

    let a = "[mcp_servers.taskai]\ncommand = \"a\"\n# comment-a\n";
    let b = "[mcp_servers.taskai]\ncommand = \"b\"\n# comment-b\n";
    std::thread::scope(|scope| {
        scope.spawn(|| atomic_write(&path, a).unwrap());
        scope.spawn(|| atomic_write(&path, b).unwrap());
    });
    let body = std::fs::read_to_string(&path).unwrap();
    assert!(body == a || body == b, "truncated or mixed write: {body:?}");
    let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600);
    assert!(
        std::fs::symlink_metadata(&planted).is_ok(),
        "cleanup must not remove a foreign staging path"
    );
}

// Expectation: two overlapping deliveries keep their own chain inside the
// serialized prompt, and finishing the first cannot clear the second.
// Why: prompt_with released its lock before GrokBridge reset chain.
// Value: hop chain follows the native turn that is actually in flight.
#[test]
fn overlapping_deliveries_keep_their_own_chain() {
    let tmp = tmp();
    let (mut config, _) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_DELAY".into(), "0.25".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", bridge.tools()).unwrap();
    let first = fixture.enqueue(&message_delivery("d-one", "one", Some(vec!["tok1".into()])));
    let second = fixture.enqueue(&message_delivery("d-two", "two", Some(vec!["tok2".into()])));
    let stop = Arc::new(AtomicBool::new(false));
    let poller = {
        let path = path.clone();
        let stop = Arc::clone(&stop);
        std::thread::spawn(move || {
            while !stop.load(Ordering::SeqCst) {
                call_tool(
                    &path,
                    "sess-1",
                    "send_message",
                    json!({"to":"codex-lead", "text":"probe"}),
                );
                std::thread::sleep(Duration::from_millis(20));
            }
        })
    };
    std::thread::scope(|scope| {
        scope.spawn(|| {
            bridge
                .deliver(first.clone(), Duration::from_secs(3))
                .unwrap()
        });
        scope.spawn(|| {
            std::thread::sleep(Duration::from_millis(50));
            bridge
                .deliver(second.clone(), Duration::from_secs(3))
                .unwrap()
        });
    });
    stop.store(true, Ordering::SeqCst);
    poller.join().unwrap();
    let chains: Vec<_> = fixture
        .letters()
        .into_iter()
        .map(|(_, letter)| letter.hop_chain)
        .collect();
    let own = fixture.mailbox.hop_token().to_owned();
    assert!(
        chains
            .iter()
            .any(|chain| chain == &["tok1".to_owned(), own.clone()]),
        "missing tok1 chain: {chains:?}"
    );
    assert!(
        chains
            .iter()
            .any(|chain| chain == &["tok2".to_owned(), own.clone()]),
        "missing tok2 chain: {chains:?}"
    );
    fixture.assert_accepted(&[first, second]);
}

/// Grok's native status notifications for `session`, in the shapes the adapter verified.
fn native_busy(session: &str) -> Value {
    json!({"jsonrpc":"2.0","method":"_x.ai/sessions/changed","params":{"upserted":[{"sessionId":session,"activity":"working"}]}})
}

fn native_idle(session: &str) -> Value {
    json!({"jsonrpc":"2.0","method":"_x.ai/session/prompt_complete","params":{"sessionId":session}})
}

fn native_text(session: &str, text: &str) -> Value {
    json!({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":text}}}})
}

fn native_permission(session: &str, id: u64) -> Value {
    json!({"jsonrpc":"2.0","id":id,"method":"session/request_permission","params":{"sessionId":session,"toolCall":{"secret":"do-not-log"}}})
}

/// Makes the fake client send `notes` as soon as it reads a prompt.
fn notify_on_prompt(config: &mut JoinConfig, notes: &[Value]) {
    config.extra_env.push((
        "FAKE_PROMPT_NOTIFY".into(),
        Value::Array(notes.to_vec()).to_string(),
    ));
}

/// Polls `bridge` until `count` status events arrive or three seconds pass.
fn poll_statuses(bridge: &GrokBridge, count: usize) -> Vec<StatusEvent> {
    let deadline = std::time::Instant::now() + Duration::from_secs(3);
    let mut statuses = Vec::new();
    while statuses.len() < count && std::time::Instant::now() < deadline {
        match bridge.poll_status() {
            Some(status) => statuses.push(status),
            None => std::thread::sleep(Duration::from_millis(20)),
        }
    }
    statuses
}

/// Whether `pid` names a process that still exists, a zombie included.
fn process_exists(pid: u32) -> bool {
    let mut ps = std::process::Command::new("ps");
    ps.args(["-o", "stat=", "-p", &pid.to_string()]);
    strip_messaging_env(&mut ps);
    !String::from_utf8_lossy(&ps.output().unwrap().stdout)
        .trim()
        .is_empty()
}

/// The activities the mailbox published, oldest first.
fn published(fixture: &MailboxFixture) -> Vec<(Activity, Option<String>)> {
    fixture
        .directory
        .activity
        .lock()
        .unwrap()
        .iter()
        .map(|recorded| (recorded.activity, recorded.waiting_for.clone()))
        .collect()
}

// Expectation: while a prompt is blocked, poll_status returns this session's native status in
// order (busy, the agent's text as detail, waiting for approval) and skips every other session's
// notifications; neither permission request is answered and its payload is never logged. Why:
// status must flow while the relay waits, only for the owned session, and approvals belong to the
// person. Value: the bridge shows live status without speaking for anyone.
#[test]
fn polling_reads_only_this_sessions_status_while_a_prompt_is_blocked() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    notify_on_prompt(
        &mut config,
        &[
            native_busy("other"),
            native_busy("sess-1"),
            native_text("other", "not ours"),
            native_text("sess-1", "drafting"),
            native_permission("other", 98),
            native_permission("sess-1", 99),
        ],
    );
    config
        .extra_env
        .push(("FAKE_PROMPT_HANG".into(), "1".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let delivery = fixture.enqueue(&message_delivery("d-poll", "one", None));
    std::thread::scope(|scope| {
        let relay = scope.spawn(|| bridge.deliver(delivery.clone(), Duration::from_secs(30)));
        assert_eq!(
            poll_statuses(&bridge, 3),
            vec![
                StatusEvent::Busy,
                StatusEvent::Detail("drafting".into()),
                StatusEvent::WaitingApproval
            ]
        );
        std::thread::sleep(Duration::from_millis(100));
        assert_eq!(
            bridge.poll_status(),
            None,
            "nothing else was this session's"
        );
        bridge.shutdown();
        relay.join().unwrap().unwrap_err();
    });
    let inbound: Vec<Value> = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str(line).ok())
        .collect();
    assert!(
        inbound.iter().all(|v| v["id"] != 98 && v["id"] != 99),
        "a permission request was answered"
    );
    let log = std::fs::read_to_string(tmp.path().join("messages.jsonl")).unwrap();
    assert!(
        !log.contains("do-not-log"),
        "an approval payload was logged"
    );
    fixture.assert_queued(&delivery);
}

// Expectation: shutdown wakes a prompt already written to the joined client at once, far inside
// its timeout, as uncertain; calling it again is harmless; it kills and reaps the joined client
// process and no other, so a native leader runs on; and no worker keeps the bridge alive. Why:
// the bridge must stop when its pane goes away without waiting out a prompt, and the native
// leader belongs to the person. Value: shutdown is prompt and stays inside this bridge.
#[test]
fn shutdown_wakes_a_written_prompt_and_stops_only_the_joined_client() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    let pid_file = tmp.path().join("client.pid");
    config
        .extra_env
        .push(("FAKE_PROMPT_HANG".into(), "1".into()));
    config
        .extra_env
        .push(("FAKE_PID".into(), pid_file.display().to_string()));
    let mut native_leader = {
        let mut sleep = std::process::Command::new("sleep");
        sleep.arg("30");
        strip_messaging_env(&mut sleep);
        sleep.spawn().unwrap()
    };
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let client_pid: u32 = std::fs::read_to_string(&pid_file)
        .unwrap()
        .trim()
        .parse()
        .unwrap();
    let bridge = Arc::new(bridge);
    let delivery = fixture.enqueue(&message_delivery(
        "d-shut",
        "one",
        Some(vec!["tok-shut".into()]),
    ));
    let relay = {
        let bridge = Arc::clone(&bridge);
        let delivery = delivery.clone();
        std::thread::spawn(move || {
            let started = std::time::Instant::now();
            let result = bridge.deliver(delivery, Duration::from_secs(30));
            (result, started.elapsed())
        })
    };
    wait_prompt(&seen);
    bridge.shutdown();
    let (result, waited) = relay.join().unwrap();
    assert!(
        matches!(result, Err(taskai_connectors::Error::Uncertain(_))),
        "{result:?}"
    );
    assert!(
        waited < Duration::from_secs(5),
        "the prompt waited {waited:?}"
    );
    bridge.shutdown();
    assert_eq!(Arc::strong_count(&bridge), 1, "a worker kept the bridge");
    assert!(
        !process_exists(client_pid),
        "the joined client was not killed and reaped"
    );
    assert!(
        process_exists(native_leader.id()),
        "the native leader was stopped"
    );
    native_leader.kill().unwrap();
    native_leader.wait().unwrap();
    fixture.assert_queued(&delivery);
}

// Expectation: shutdown also wakes a prompt stuck writing to a joined client that stopped
// reading, without waiting for the stdin lock that write holds, and the attempt is uncertain.
// Why: a long prompt can fill the pipe, and shutdown must not wait behind it. Value: no relay can
// wedge shutdown.
#[test]
fn shutdown_wakes_a_prompt_blocked_in_its_write() {
    let tmp = tmp();
    let (mut config, _) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_STOP_READING".into(), "1".into()));
    let client = Arc::new(LeaderClient::join(config).unwrap());
    let writer = {
        let client = Arc::clone(&client);
        std::thread::spawn(move || client.prompt(&"x".repeat(1 << 20), Duration::from_secs(30)))
    };
    std::thread::sleep(Duration::from_millis(300));
    let started = std::time::Instant::now();
    client.shutdown();
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "shutdown waited behind the write"
    );
    let result = writer.join().unwrap();
    assert!(
        matches!(result, Err(taskai_connectors::Error::Uncertain(_))),
        "{result:?}"
    );
}

// Expectation: after shutdown the written prompt stays uncertain and keeps its chain: a late tool
// call from that turn continues the proven chain; a later delivery is refused before any write and
// fails definitely; closing the mailbox logs the attempt as uncertain without a dropped receipt.
// Why: shutdown cannot learn whether Grok accepted the written prompt. Value: delivery accounting
// stays truthful through shutdown.
#[test]
fn after_shutdown_a_written_prompt_stays_uncertain_and_later_prompts_fail_definitely() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_HANG".into(), "1".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let attempted = fixture.enqueue(&message_delivery(
        "d-attempted",
        "one",
        Some(vec!["tok-a".into()]),
    ));
    std::thread::scope(|scope| {
        let relay = scope.spawn(|| bridge.deliver(attempted.clone(), Duration::from_secs(30)));
        wait_prompt(&seen);
        bridge.shutdown();
        let result = relay.join().unwrap();
        assert!(
            matches!(result, Err(taskai_connectors::Error::Uncertain(_))),
            "{result:?}"
        );
    });
    let late = bridge
        .tools()
        .send_message("codex-lead", "late reply", false)
        .unwrap();
    assert_eq!(late["status"], "sent");
    assert_eq!(
        fixture.letters().last().unwrap().1.hop_chain,
        vec!["tok-a".to_owned(), fixture.mailbox.hop_token().to_owned()]
    );
    let refused = fixture.enqueue(&message_delivery("d-refused", "two", None));
    let error = bridge
        .deliver(refused.clone(), Duration::from_secs(30))
        .unwrap_err();
    assert!(
        matches!(error, taskai_connectors::Error::Invalid(_)),
        "{error}"
    );
    let prompts = seen_methods(&seen)
        .iter()
        .filter(|method| *method == "session/prompt")
        .count();
    assert_eq!(prompts, 1, "only the first prompt was written");
    let receipts = fixture.receipts();
    assert_eq!(receipts.len(), 1, "{receipts:?}");
    assert_eq!(receipts[0].orig_msg_id, refused.msg_id);
    assert_eq!(receipts[0].drop_reason.as_deref(), Some("delivery-failed"));
    fixture.mailbox.close();
    assert_eq!(
        fixture.receipts().len(),
        1,
        "close dropped the uncertain prompt"
    );
    let log = std::fs::read_to_string(tmp.path().join("messages.jsonl")).unwrap();
    assert!(log
        .lines()
        .filter_map(|line| serde_json::from_str::<Value>(line).ok())
        .any(
            |entry| entry["msg_id"] == attempted.msg_id && entry["outcome"] == "delivery uncertain"
        ));
}

// Expectation: a prompt Grok definitely rejects leaves the session showing what native status last
// reported, idle here, not busy. Why: only native status says what Grok is doing; the bridge must
// not invent busy around a write. Value: a refused letter cannot pin the session busy.
#[test]
fn a_rejected_prompt_returns_activity_to_the_native_report() {
    let tmp = tmp();
    let (mut config, _) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_ERROR".into(), "turn refused".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    bridge.tools().apply_status(StatusEvent::Idle);
    let delivery = fixture.enqueue(&message_delivery("d-rejected", "one", None));
    bridge
        .deliver(delivery, Duration::from_secs(3))
        .unwrap_err();
    assert_eq!(
        published(&fixture).last(),
        Some(&(Activity::Idle, None)),
        "{:?}",
        published(&fixture)
    );
}

// Expectation: busy, waiting and idle come from Grok's native status alone: while a prompt waits
// for its late acceptance, the working, permission and prompt_complete notifications show busy,
// waiting for approval and idle, and the acceptance that follows leaves the session idle. Why: an
// acknowledgement can arrive after the turn already ended, and it must not overwrite native idle.
// Value: the session shows what Grok reports.
#[test]
fn native_status_alone_moves_activity_and_a_late_acceptance_keeps_idle() {
    let tmp = tmp();
    let (mut config, _) = join_config(tmp.path(), "false");
    notify_on_prompt(
        &mut config,
        &[
            native_busy("sess-1"),
            native_permission("sess-1", 99),
            native_idle("sess-1"),
        ],
    );
    config
        .extra_env
        .push(("FAKE_PROMPT_DELAY".into(), "0.5".into()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let delivery = fixture.enqueue(&message_delivery("d-late", "one", None));
    std::thread::scope(|scope| {
        let relay = scope.spawn(|| bridge.deliver(delivery.clone(), Duration::from_secs(3)));
        let statuses = poll_statuses(&bridge, 3);
        assert_eq!(
            statuses,
            vec![
                StatusEvent::Busy,
                StatusEvent::WaitingApproval,
                StatusEvent::Idle
            ]
        );
        for status in statuses {
            bridge.tools().apply_status(status);
        }
        relay.join().unwrap().unwrap();
    });
    let shown = published(&fixture);
    assert!(
        shown.contains(&(Activity::Waiting, Some("approval".into()))),
        "{shown:?}"
    );
    assert_eq!(shown.last(), Some(&(Activity::Idle, None)), "{shown:?}");
    fixture.assert_accepted(&[delivery]);
}

// Expectation: shutdown wakes a written prompt at once even when the joined client's output stays
// open after it is killed, here held by a process the client started, and a later prompt is still
// refused before any write and fails definitely. Why: waking must not depend on seeing the
// client's output close. Value: shutdown stays prompt whatever the joined client left running.
#[test]
fn shutdown_wakes_a_prompt_even_if_the_clients_output_stays_open() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    let holder_file = tmp.path().join("holder.pid");
    config
        .extra_env
        .push(("FAKE_PROMPT_HANG".into(), "1".into()));
    config
        .extra_env
        .push(("FAKE_HOLD_STDOUT".into(), holder_file.display().to_string()));
    let (fixture, bridge) = mailbox_bridge(&tmp, LeaderClient::join(config).unwrap());
    let holder: u32 = std::fs::read_to_string(&holder_file)
        .unwrap()
        .trim()
        .parse()
        .unwrap();
    let attempted = fixture.enqueue(&message_delivery("d-held", "one", None));
    std::thread::scope(|scope| {
        let relay = scope.spawn(|| {
            let started = std::time::Instant::now();
            let result = bridge.deliver(attempted.clone(), Duration::from_secs(30));
            (result, started.elapsed())
        });
        wait_prompt(&seen);
        bridge.shutdown();
        let (result, waited) = relay.join().unwrap();
        assert!(
            matches!(result, Err(taskai_connectors::Error::Uncertain(_))),
            "{result:?}"
        );
        assert!(
            waited < Duration::from_secs(5),
            "the prompt waited {waited:?}"
        );
    });
    let refused = fixture.enqueue(&message_delivery("d-after", "two", None));
    let error = bridge
        .deliver(refused.clone(), Duration::from_secs(30))
        .unwrap_err();
    assert!(
        matches!(error, taskai_connectors::Error::Invalid(_)),
        "{error}"
    );
    let receipts = fixture.receipts();
    assert_eq!(receipts.len(), 1, "{receipts:?}");
    assert_eq!(receipts[0].orig_msg_id, refused.msg_id);
    let mut kill = std::process::Command::new("kill");
    kill.arg(holder.to_string());
    strip_messaging_env(&mut kill);
    assert!(kill.status().unwrap().success(), "the output holder ran on");
}

// Expectation: leader_log_path resolves the shared leader log as Grok resolves its home:
// leader.log in an absolute GROK_HOME, else in .grok under the home directory; a relative or
// empty GROK_HOME, or a home that is not absolute, is refused instead of guessed. Why: the join
// gate must read the log the native leader writes, and a wrong guess must fail closed. Value:
// the pane's leader and the bridge's gate agree on the proof source. Each case runs in a copy of
// this test binary with exactly its environment.
#[test]
fn the_leader_log_path_follows_grok_home_and_refuses_a_relative_one() {
    const TEST: &str = "the_leader_log_path_follows_grok_home_and_refuses_a_relative_one";
    if std::env::var_os("TASKAI_LEADER_LOG_COPY").is_some() {
        match leader_log_path() {
            Ok(path) => println!("path={}", path.display()),
            Err(_) => println!("refused"),
        }
        return;
    }
    let resolved = |grok_home: Option<&str>, home: &str| {
        let mut copy = std::process::Command::new(std::env::current_exe().unwrap());
        strip_messaging_env(&mut copy);
        copy.args(["--exact", TEST, "--nocapture"])
            .env("TASKAI_LEADER_LOG_COPY", "1")
            .env("HOME", home);
        match grok_home {
            Some(value) => copy.env("GROK_HOME", value),
            None => copy.env_remove("GROK_HOME"),
        };
        let output = copy.output().unwrap();
        assert!(output.status.success());
        String::from_utf8(output.stdout)
            .unwrap()
            .lines()
            .find(|line| line.starts_with("path=") || *line == "refused")
            .unwrap()
            .to_string()
    };
    assert_eq!(
        resolved(Some("/srv/grok-home"), "/home/person"),
        "path=/srv/grok-home/leader.log"
    );
    assert_eq!(
        resolved(None, "/home/person"),
        "path=/home/person/.grok/leader.log"
    );
    assert_eq!(resolved(Some("relative/grok"), "/home/person"), "refused");
    assert_eq!(resolved(Some(""), "/home/person"), "refused");
    assert_eq!(resolved(None, "relative-home"), "refused");
}

// Startup cancellation must wake every wait and reap only the joining client. The independent
// native-process fixture remains alive, and no later startup request may follow cancellation.
fn assert_join_cancelled_at(phase: &str, methods: &[&str], written_rpc: bool) {
    struct NativeFixture(std::process::Child);
    impl Drop for NativeFixture {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config.timeout = Duration::from_secs(5);
    let pid_file = tmp.path().join("joining-client.pid");
    config.extra_env.extend([
        (phase.into(), "1".into()),
        ("FAKE_PID".into(), pid_file.display().to_string()),
    ]);
    let mut native = std::process::Command::new("sleep");
    native.arg("30");
    strip_messaging_env(&mut native);
    let mut native = NativeFixture(native.spawn().unwrap());
    let cancelled = Arc::new(AtomicBool::new(false));
    let flag = Arc::clone(&cancelled);
    let worker = std::thread::spawn(move || LeaderClient::join_with_cancel(config, flag));
    wait_for(|| seen_methods(&seen).len() == methods.len() && pid_file.exists());
    let pid: u32 = std::fs::read_to_string(&pid_file)
        .unwrap()
        .trim()
        .parse()
        .unwrap();
    let start = std::time::Instant::now();
    cancelled.store(true, Ordering::SeqCst);
    let result = worker.join().unwrap();
    let elapsed = start.elapsed();
    let error = result.err().expect("cancelled join must fail");
    assert!(
        !process_exists(pid),
        "joined client was not reaped before return"
    );
    assert!(
        native.0.try_wait().unwrap().is_none(),
        "native leader was stopped"
    );
    assert_eq!(seen_methods(&seen), methods);
    assert!(
        elapsed < Duration::from_secs(1),
        "startup cancellation waited {elapsed:?}"
    );
    assert!(error.to_string().contains("cancel"), "{error}");
    if written_rpc {
        assert!(
            matches!(error, taskai_connectors::Error::Uncertain(_)),
            "{error}"
        );
    } else {
        // The fake has sent initialize's reply, but the reader can race this
        // cancellation. If it has not observed that reply, uncertainty is honest.
        assert!(
            matches!(
                error,
                taskai_connectors::Error::Invalid(_) | taskai_connectors::Error::Uncertain(_)
            ),
            "{error}"
        );
    }
    assert_eq!(
        Arc::strong_count(&cancelled),
        1,
        "startup retained cancellation owner"
    );
}

#[test]
fn join_cancel_interrupts_initialize_and_reaps_only_its_client() {
    assert_join_cancelled_at("FAKE_INIT_HANG", &["initialize"], true);
}

#[test]
fn join_cancel_interrupts_fresh_registration_wait_without_loading() {
    assert_join_cancelled_at("FAKE_NO_REGISTRATION", &["initialize"], false);
}

#[test]
fn join_cancel_interrupts_session_load_and_reaps_only_its_client() {
    assert_join_cancelled_at("FAKE_LOAD_HANG", &["initialize", "session/load"], true);
}

// A cancelled startup must not even inspect a missing leader log or spawn the joining client.
#[test]
fn join_cancelled_before_start_does_not_spawn() {
    let tmp = tmp();
    let (mut config, seen) = join_config(tmp.path(), "false");
    config.leader_log = tmp.path().join("missing.log");
    let error = LeaderClient::join_with_cancel(config, Arc::new(AtomicBool::new(true)))
        .err()
        .expect("cancelled before start");
    assert!(
        matches!(error, taskai_connectors::Error::Invalid(_)),
        "{error}"
    );
    assert!(error.to_string().contains("cancel"), "{error}");
    assert!(!seen.exists());
    assert!(!tmp.path().join("env.flag").exists());
}
