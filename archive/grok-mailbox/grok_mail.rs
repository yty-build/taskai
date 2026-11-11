// ABOUTME: Offline Grok mail adapter proofs — leader/client argv, workspace MCP merge,
// ABOUTME: session-bound tool socket, and fake-leader join. Never contacts a live leader.

use std::collections::HashMap;
use std::fs::OpenOptions;
use std::io::Write;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::json;
use taskai_connectors::grok::{
    atomic_write, call_tool, handle_mcp_message, install_mcp, interpret_notification,
    leader_socket_path, pane_env, tool_socket_path, Activity, Delivery, DeliveryKind, Directory,
    Frame, GrokBridge, GrokConnector, GrokTools, Identity, JoinConfig, LeaderClient, ListedSession,
    MailPeer, Mailbox, MailboxConfig, PermissionMode, SendResult, SendStatus, StatusEvent,
    ToolHandler, ToolServer, Transport, MAX_TOOL_LINE_BYTES,
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
def reply(msg_id, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg_id, "result": result}) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    with open(seen, "a") as f:
        f.write(json.dumps(msg) + "\n")
    method = msg.get("method")
    if method == "initialize":
        with open(log, "a") as f:
            f.write("Client registered client_id=1 client_type=python3 --permission-mode default agent --leader --leader-socket %s stdio mode=Stdio yolo_mode=%s\n" % (sock, yolo))
        load = os.environ.get("FAKE_LOAD_SESSION", "true") == "true"
        reply(msg["id"], {"protocolVersion": 1, "agentCapabilities": {"loadSession": load}})
    elif method == "session/load":
        reply(msg["id"], {})
        if perm:
            sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": 99, "method": "session/request_permission", "params": {"sessionId": os.environ.get("FAKE_SESSION", ""), "toolCall": {"secret": "do-not-log"}}}) + "\n")
            sys.stdout.flush()
    elif method == "session/prompt":
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

// Expectation: yolo_mode=false lets session/load through with yoloMode false,
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
    assert_eq!(load["params"]["sessionId"], "sess-1");
    assert_eq!(std::fs::read_to_string(&env_flag).unwrap(), "absent");
    let done = client.prompt("hello", Duration::from_secs(2)).unwrap();
    assert_eq!(done["stopReason"], "end_turn");
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

struct RecordingTransport {
    sent: Mutex<Vec<String>>,
}

impl Transport for RecordingTransport {
    fn send(&self, to: &MailPeer, _frame: &Frame) -> Result<(), taskai_connectors::Error> {
        self.sent.lock().unwrap().push(to.name.clone());
        Ok(())
    }
}

struct EmptyDirectory;

impl Directory for EmptyDirectory {
    fn live_sessions(&self) -> Result<Vec<MailPeer>, taskai_connectors::Error> {
        Ok(Vec::new())
    }
    fn register(&self, _entry: &MailPeer) -> Result<(), taskai_connectors::Error> {
        Ok(())
    }
    fn set_status(
        &self,
        _session_ref: &str,
        _status: Activity,
        _waiting_for: Option<&str>,
    ) -> Result<(), taskai_connectors::Error> {
        Ok(())
    }
    fn unregister(&self, _session_ref: &str) -> Result<(), taskai_connectors::Error> {
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct RecordedSend {
    to: String,
    text: String,
    notify_when_idle: bool,
    chain: Option<Vec<String>>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct RecordedActivity {
    activity: Activity,
    waiting_for: Option<String>,
}

#[derive(Clone)]
struct FakeMailbox {
    identity: Identity,
    transport_held: bool,
    sends: Arc<Mutex<Vec<RecordedSend>>>,
    started: Arc<Mutex<Vec<String>>>,
    failed: Arc<Mutex<Vec<(String, String)>>>,
    queued: Arc<Mutex<HashMap<String, String>>>,
    activity: Arc<Mutex<Vec<RecordedActivity>>>,
    details: Arc<Mutex<Vec<String>>>,
    listed: Vec<ListedSession>,
    transport_sends: Arc<Mutex<Vec<String>>>,
}

impl FakeMailbox {
    fn open(
        config: MailboxConfig,
        _directory: Arc<dyn Directory>,
        transport: Arc<dyn Transport>,
        _deliver: impl Fn(Delivery) + Send + Sync + 'static,
    ) -> Self {
        let _ = transport;
        Self {
            identity: config.identity,
            transport_held: true,
            sends: Arc::new(Mutex::new(Vec::new())),
            started: Arc::new(Mutex::new(Vec::new())),
            failed: Arc::new(Mutex::new(Vec::new())),
            queued: Arc::new(Mutex::new(HashMap::new())),
            activity: Arc::new(Mutex::new(Vec::new())),
            details: Arc::new(Mutex::new(Vec::new())),
            listed: vec![ListedSession {
                name: "codex-lead".into(),
                harness: "codex".into(),
                status: Activity::Idle,
                waiting_for: None,
            }],
            transport_sends: Arc::new(Mutex::new(Vec::new())),
        }
    }

    fn enqueue(&self, delivery: &Delivery) {
        self.queued
            .lock()
            .unwrap()
            .insert(delivery.id.clone(), delivery.msg_id.clone());
    }

    fn queued_ids(&self) -> Vec<String> {
        let mut ids: Vec<_> = self.queued.lock().unwrap().keys().cloned().collect();
        ids.sort();
        ids
    }
}

impl Mailbox for FakeMailbox {
    fn send(
        &self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
        chain: Option<&[String]>,
    ) -> SendResult {
        self.sends.lock().unwrap().push(RecordedSend {
            to: to.into(),
            text: text.into(),
            notify_when_idle,
            chain: chain.map(|c| c.to_vec()),
        });
        SendResult {
            status: SendStatus::Sent,
            to: to.into(),
            msg_id: "m-rec".into(),
            target_status: Some(Activity::Idle),
            idle_notice: None,
            error: None,
        }
    }

    fn subscribe(&self, _to: &str) -> taskai_connectors::grok::SubscriptionResult {
        taskai_connectors::grok::SubscriptionResult::Subscribed
    }

    fn list_sessions(&self) -> Result<Vec<ListedSession>, taskai_connectors::Error> {
        Ok(self.listed.clone())
    }

    fn set_activity(&self, activity: Activity, waiting_for: Option<&str>) {
        self.activity.lock().unwrap().push(RecordedActivity {
            activity,
            waiting_for: waiting_for.map(str::to_string),
        });
    }

    fn set_detail(&self, text: &str) {
        self.details.lock().unwrap().push(text.into());
    }

    fn turn_started(&self, delivery_id: &str) {
        if self.queued.lock().unwrap().remove(delivery_id).is_some() {
            self.started.lock().unwrap().push(delivery_id.into());
        }
    }

    fn delivery_failed(&self, delivery_id: &str, reason: &str) {
        if self.queued.lock().unwrap().remove(delivery_id).is_none() {
            return;
        }
        self.failed
            .lock()
            .unwrap()
            .push((delivery_id.into(), reason.into()));
    }

    fn close(&self) {}
}

fn grok_identity(dir: &Path) -> Identity {
    Identity::grok(
        "s3",
        "t1",
        "grok-lead",
        dir.join("ws"),
        PermissionMode::Prompting,
    )
}

fn grok_config(dir: &Path, identity: Identity) -> MailboxConfig {
    MailboxConfig {
        identity,
        mail_dir: dir.join("mail"),
        log_path: dir.join("messages.jsonl"),
        max_deliveries: 10,
    }
}

fn wait_busy(mailbox: &FakeMailbox, timeout: Duration) -> bool {
    let deadline = std::time::Instant::now() + timeout;
    while std::time::Instant::now() < deadline {
        if mailbox
            .activity
            .lock()
            .unwrap()
            .iter()
            .any(|entry| entry.activity == Activity::Busy)
        {
            return true;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    false
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
        mode: identity.mode,
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
// SocketTransport without Grok importing core mailbox code.
#[test]
fn mailbox_open_takes_explicit_transport() {
    let tmp = tmp();
    let identity = grok_identity(tmp.path());
    let transport = Arc::new(RecordingTransport {
        sent: Mutex::new(Vec::new()),
    });
    let mailbox = FakeMailbox::open(
        grok_config(tmp.path(), identity.clone()),
        Arc::new(EmptyDirectory),
        transport,
        |_| {},
    );
    assert!(mailbox.transport_held);
    assert_eq!(mailbox.identity.session_ref, "s3");
    assert_eq!(mailbox.identity.harness, "grok");
    assert!(mailbox.transport_sends.lock().unwrap().is_empty());
}

// Expectation: list_sessions and send_message delegate to Mailbox. Extra
// from / from_mode / hop_chain keys are rejected before dispatch. Why: tool
// dispatch never accepts caller-supplied sender identity, permission mode or
// hop chain. Value: the native turn's chain is the only chain a send continues.
#[test]
fn tool_dispatch_delegates_to_mailbox_without_caller_identity() {
    let tmp = tmp();
    let mailbox = Arc::new(FakeMailbox::open(
        grok_config(tmp.path(), grok_identity(tmp.path())),
        Arc::new(EmptyDirectory),
        Arc::new(RecordingTransport {
            sent: Mutex::new(Vec::new()),
        }),
        |_| {},
    ));
    let tools = GrokTools::new(Arc::clone(&mailbox));
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "owned-session", tools).unwrap();

    let listed = call_tool(&path, "owned-session", "list_sessions", json!({}));
    assert_eq!(listed["status"], "ok");
    assert_eq!(listed["sessions"][0]["name"], "codex-lead");
    assert_eq!(listed["sessions"][0]["waitingFor"], serde_json::Value::Null);

    let extra_list = call_tool(
        &path,
        "owned-session",
        "list_sessions",
        json!({"from": "x"}),
    );
    assert_eq!(extra_list["status"], "error");
    assert!(mailbox.sends.lock().unwrap().is_empty());

    let extra_send = call_tool(
        &path,
        "owned-session",
        "send_message",
        json!({
            "to": "codex-lead",
            "text": "hello",
            "notify_when_idle": true,
            "from": "imposter",
            "from_mode": "bypass",
            "hop_chain": ["evil"]
        }),
    );
    assert_eq!(extra_send["status"], "error");
    assert!(mailbox.sends.lock().unwrap().is_empty());

    let sent = call_tool(
        &path,
        "owned-session",
        "send_message",
        json!({"to": "codex-lead", "text": "hello", "notify_when_idle": true}),
    );
    assert_eq!(sent["status"], "sent");
    assert_eq!(sent["to"], "codex-lead");
    assert_eq!(sent["msg_id"], "m-rec");
    let sends = mailbox.sends.lock().unwrap().clone();
    assert_eq!(sends.len(), 1);
    assert_eq!(sends[0].to, "codex-lead");
    assert_eq!(sends[0].text, "hello");
    assert!(sends[0].notify_when_idle);
    assert_eq!(sends[0].chain, None);
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
    let mailbox = Arc::new(FakeMailbox::open(
        grok_config(tmp.path(), grok_identity(tmp.path())),
        Arc::new(EmptyDirectory),
        Arc::new(RecordingTransport {
            sent: Mutex::new(Vec::new()),
        }),
        |_| {},
    ));
    let bridge = GrokBridge::new(Arc::clone(&mailbox), client);
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", bridge.tools()).unwrap();
    let delivery = Delivery {
        id: "d-letter".into(),
        kind: DeliveryKind::Message,
        text: "from codex-lead: please look".into(),
        chain: Some(vec!["tok1".into()]),
        msg_id: "m-letter".into(),
        sender: "codex-lead".into(),
    };
    mailbox.enqueue(&delivery);

    let watcher = {
        let mailbox = Arc::clone(&mailbox);
        let path = path.clone();
        std::thread::spawn(move || {
            assert!(
                wait_busy(&mailbox, Duration::from_secs(2)),
                "turn never busy"
            );
            call_tool(
                &path,
                "sess-1",
                "send_message",
                json!({"to": "codex-lead", "text": "reply"}),
            )
        })
    };

    bridge.deliver(delivery, Duration::from_secs(3)).unwrap();
    let tool = watcher.join().unwrap();
    assert_eq!(tool["status"], "sent");
    assert_eq!(
        *mailbox.started.lock().unwrap(),
        vec!["d-letter".to_string()]
    );
    assert!(mailbox.failed.lock().unwrap().is_empty());
    let sends = mailbox.sends.lock().unwrap().clone();
    assert_eq!(sends.len(), 1);
    assert_eq!(sends[0].chain.as_deref(), Some(&["tok1".to_string()][..]));
    let methods = seen_methods(&seen);
    assert!(
        methods.contains(&"session/prompt".to_string()),
        "{methods:?}"
    );
}

// Expectation: a Notice delivery starts no peer chain, even if a chain
// field is present on the struct. Why: notices are not instructions.
// Value: a status pulse cannot extend a hop chain.
#[test]
fn notice_delivery_starts_no_chain() {
    let tmp = tmp();
    let (mut config, _) = join_config(tmp.path(), "false");
    config
        .extra_env
        .push(("FAKE_PROMPT_DELAY".into(), "0.25".into()));
    let client = LeaderClient::join(config).unwrap();
    let mailbox = Arc::new(FakeMailbox::open(
        grok_config(tmp.path(), grok_identity(tmp.path())),
        Arc::new(EmptyDirectory),
        Arc::new(RecordingTransport {
            sent: Mutex::new(Vec::new()),
        }),
        |_| {},
    ));
    let bridge = GrokBridge::new(Arc::clone(&mailbox), client);
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", bridge.tools()).unwrap();

    let delivery = Delivery {
        id: "d-notice".into(),
        kind: DeliveryKind::Notice,
        text: "TaskAI: codex-lead is idle".into(),
        chain: Some(vec!["should-not-use".into()]),
        msg_id: "m-sub".into(),
        sender: "TaskAI".into(),
    };
    mailbox.enqueue(&delivery);

    let watcher = {
        let mailbox = Arc::clone(&mailbox);
        let path = path.clone();
        std::thread::spawn(move || {
            assert!(
                wait_busy(&mailbox, Duration::from_secs(2)),
                "turn never busy"
            );
            call_tool(
                &path,
                "sess-1",
                "send_message",
                json!({"to": "codex-lead", "text": "during-notice"}),
            )
        })
    };

    bridge.deliver(delivery, Duration::from_secs(3)).unwrap();
    watcher.join().unwrap();
    assert_eq!(
        *mailbox.started.lock().unwrap(),
        vec!["d-notice".to_string()]
    );
    let sends = mailbox.sends.lock().unwrap().clone();
    assert_eq!(sends.len(), 1);
    assert_eq!(sends[0].chain, None);
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
    let client = LeaderClient::join(config).unwrap();
    let mailbox = Arc::new(FakeMailbox::open(
        grok_config(tmp.path(), grok_identity(tmp.path())),
        Arc::new(EmptyDirectory),
        Arc::new(RecordingTransport {
            sent: Mutex::new(Vec::new()),
        }),
        |_| {},
    ));
    let bridge = GrokBridge::new(Arc::clone(&mailbox), client);
    let delivery = Delivery {
        id: "d-fail".into(),
        kind: DeliveryKind::Message,
        text: "go".into(),
        chain: Some(vec!["tok".into()]),
        msg_id: "m-fail".into(),
        sender: "codex-lead".into(),
    };
    mailbox.enqueue(&delivery);
    let err = bridge
        .deliver(delivery, Duration::from_secs(3))
        .expect_err("prompt must fail");
    assert!(err.to_string().contains("leader rejected prompt"), "{err}");
    assert!(
        mailbox.started.lock().unwrap().is_empty(),
        "turn_started before reject drops failure metadata"
    );
    let failed = mailbox.failed.lock().unwrap().clone();
    assert_eq!(failed.len(), 1);
    assert_eq!(failed[0].0, "d-fail");
    assert!(failed[0].1.contains("leader rejected prompt"), "{failed:?}");
    assert!(mailbox.queued_ids().is_empty());
    assert!(seen_methods(&seen).contains(&"session/prompt".to_string()));
}

// Expectation: Grok status pulses become Mailbox set_activity / set_detail.
// Why: the harness reports its own status; an admitted delivery is busy
// until the harness says otherwise. Value: M6 wires notifications without
// Grok talking to the registry.
#[test]
fn status_events_go_to_mailbox() {
    let tmp = tmp();
    let mailbox = Arc::new(FakeMailbox::open(
        grok_config(tmp.path(), grok_identity(tmp.path())),
        Arc::new(EmptyDirectory),
        Arc::new(RecordingTransport {
            sent: Mutex::new(Vec::new()),
        }),
        |_| {},
    ));
    let tools = GrokTools::new(Arc::clone(&mailbox));
    tools.apply_status(StatusEvent::Busy);
    tools.apply_status(StatusEvent::WaitingApproval);
    tools.apply_status(StatusEvent::Detail("working on it".into()));
    tools.apply_status(StatusEvent::Unknown);
    tools.apply_status(StatusEvent::Idle);
    let activity = mailbox.activity.lock().unwrap().clone();
    assert_eq!(
        activity,
        vec![
            RecordedActivity {
                activity: Activity::Busy,
                waiting_for: None,
            },
            RecordedActivity {
                activity: Activity::Waiting,
                waiting_for: Some("approval".into()),
            },
            RecordedActivity {
                activity: Activity::Idle,
                waiting_for: None,
            },
        ]
    );
    assert_eq!(
        *mailbox.details.lock().unwrap(),
        vec!["working on it".to_string()]
    );
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
            "Client registered client_id=9 client_type=grok --permission-mode default agent --leader --leader-socket {sock}-forged stdio mode=Stdio yolo_mode=false\n"
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
    let client = LeaderClient::join(config).unwrap();
    let mailbox = Arc::new(FakeMailbox::open(
        grok_config(tmp.path(), grok_identity(tmp.path())),
        Arc::new(EmptyDirectory),
        Arc::new(RecordingTransport {
            sent: Mutex::new(Vec::new()),
        }),
        |_| {},
    ));
    let bridge = GrokBridge::new(Arc::clone(&mailbox), client);
    let delivery = Delivery {
        id: "d-hang".into(),
        kind: DeliveryKind::Message,
        text: "one".into(),
        chain: None,
        msg_id: "m-hang".into(),
        sender: "codex-lead".into(),
    };
    mailbox.enqueue(&delivery);
    let err = bridge
        .deliver(delivery, Duration::from_millis(200))
        .expect_err("prompt must time out");
    assert!(err.to_string().contains("no response"), "{err}");
    assert!(mailbox.started.lock().unwrap().is_empty());
    assert!(mailbox.failed.lock().unwrap().is_empty());
    assert_eq!(mailbox.queued_ids(), vec!["d-hang".to_string()]);
}

fn mailbox_bridge(
    tmp: &tempfile::TempDir,
    client: LeaderClient,
) -> (Arc<FakeMailbox>, GrokBridge<FakeMailbox>) {
    let mailbox = Arc::new(FakeMailbox::open(
        grok_config(tmp.path(), grok_identity(tmp.path())),
        Arc::new(EmptyDirectory),
        Arc::new(RecordingTransport {
            sent: Mutex::new(Vec::new()),
        }),
        |_| {},
    ));
    let bridge = GrokBridge::new(Arc::clone(&mailbox), client);
    (mailbox, bridge)
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
    let client = LeaderClient::join(config).unwrap();
    let (mailbox, bridge) = mailbox_bridge(&tmp, client);
    let delivery = message_delivery("d-eof", "one", Some(vec!["tok-eof".into()]));
    mailbox.enqueue(&delivery);
    let err = bridge
        .deliver(delivery, Duration::from_secs(3))
        .expect_err("eof after write must not look accepted");
    assert!(
        err.to_string().contains("stdout closed"),
        "eof must be distinguished from rpc reject: {err}"
    );
    assert!(mailbox.started.lock().unwrap().is_empty());
    assert!(
        mailbox.failed.lock().unwrap().is_empty(),
        "eof after write must not drop the delivery: {:?}",
        mailbox.failed.lock().unwrap()
    );
    assert_eq!(mailbox.queued_ids(), vec!["d-eof".to_string()]);
    let prompts = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .filter(|v| v["method"] == "session/prompt")
        .count();
    assert_eq!(prompts, 1);
    let later = message_delivery("d-later", "two", None);
    let err2 = bridge
        .deliver(later, Duration::from_millis(200))
        .expect_err("later prompt must not write");
    assert!(err2.to_string().contains("unusable"), "{err2}");
    let prompts = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .filter(|v| v["method"] == "session/prompt")
        .count();
    assert_eq!(prompts, 1, "later prompt was written after eof");
    assert_eq!(mailbox.queued_ids(), vec!["d-eof".to_string()]);
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
    let client = LeaderClient::join(config).unwrap();
    let (mailbox, bridge) = mailbox_bridge(&tmp, client);
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", bridge.tools()).unwrap();
    let delivery = message_delivery("d-hang-chain", "one", Some(vec!["tok-hang".into()]));
    mailbox.enqueue(&delivery);
    let err = bridge
        .deliver(delivery, Duration::from_millis(200))
        .expect_err("prompt must time out");
    assert!(err.to_string().contains("no response"), "{err}");
    let tool = call_tool(
        &path,
        "sess-1",
        "send_message",
        json!({"to": "codex-lead", "text": "late"}),
    );
    assert_eq!(tool["status"], "sent", "{tool}");
    let sends = mailbox.sends.lock().unwrap().clone();
    assert_eq!(sends.len(), 1, "{sends:?}");
    assert_eq!(
        sends[0].chain,
        Some(vec!["tok-hang".to_string()]),
        "timeout cleared the in-flight chain"
    );
    assert!(mailbox.started.lock().unwrap().is_empty());
    assert!(mailbox.failed.lock().unwrap().is_empty());
    assert_eq!(mailbox.queued_ids(), vec!["d-hang-chain".to_string()]);
    let prompts = std::fs::read_to_string(&seen)
        .unwrap()
        .lines()
        .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
        .filter(|v| v["method"] == "session/prompt")
        .count();
    assert_eq!(prompts, 1);
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
    let client = LeaderClient::join(config).unwrap();
    let (mailbox, bridge) = mailbox_bridge(&tmp, client);
    let delivery = message_delivery("d-bad-ack", "one", Some(vec!["tok-bad".into()]));
    mailbox.enqueue(&delivery);
    let err = bridge
        .deliver(delivery, Duration::from_secs(3))
        .expect_err("malformed ack must not look accepted");
    assert!(
        err.to_string().contains("malformed"),
        "malformed ack must be distinguished: {err}"
    );
    assert!(mailbox.started.lock().unwrap().is_empty());
    assert!(mailbox.failed.lock().unwrap().is_empty());
    assert_eq!(mailbox.queued_ids(), vec!["d-bad-ack".to_string()]);
    assert!(seen_methods(&seen).contains(&"session/prompt".to_string()));
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
    let client = LeaderClient::join(config).unwrap();
    let mailbox = Arc::new(FakeMailbox::open(
        grok_config(tmp.path(), grok_identity(tmp.path())),
        Arc::new(EmptyDirectory),
        Arc::new(RecordingTransport {
            sent: Mutex::new(Vec::new()),
        }),
        |_| {},
    ));
    let bridge = GrokBridge::new(Arc::clone(&mailbox), client);
    let path = tmp.path().join("t.sock");
    let _server = ToolServer::listen(&path, "sess-1", bridge.tools()).unwrap();
    let first = Delivery {
        id: "d-one".into(),
        kind: DeliveryKind::Message,
        text: "one".into(),
        chain: Some(vec!["tok1".into()]),
        msg_id: "m-one".into(),
        sender: "codex-lead".into(),
    };
    let second = Delivery {
        id: "d-two".into(),
        kind: DeliveryKind::Message,
        text: "two".into(),
        chain: Some(vec!["tok2".into()]),
        msg_id: "m-two".into(),
        sender: "codex-lead".into(),
    };
    mailbox.enqueue(&first);
    mailbox.enqueue(&second);

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
                    json!({"to": "codex-lead", "text": "probe"}),
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
    let _ = poller.join();

    let chains: Vec<_> = mailbox
        .sends
        .lock()
        .unwrap()
        .iter()
        .filter_map(|send| send.chain.clone())
        .collect();
    assert!(
        chains.iter().any(|chain| chain == &["tok1".to_string()]),
        "missing tok1 chain: {chains:?}"
    );
    assert!(
        chains.iter().any(|chain| chain == &["tok2".to_string()]),
        "missing tok2 chain: {chains:?}"
    );
    let mut started = mailbox.started.lock().unwrap().clone();
    started.sort();
    assert_eq!(started, vec!["d-one".to_string(), "d-two".to_string()]);
}
