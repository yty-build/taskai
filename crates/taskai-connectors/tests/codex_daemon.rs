// ABOUTME: Offline Codex daemon contract tests over a private Unix WebSocket.
// ABOUTME: Verifies exact thread routing, tool arguments, relay semantics, and lifecycle methods.

use std::net::Shutdown;
use std::os::unix::net::UnixListener;
use std::path::Path;
use std::sync::mpsc;
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use taskai_connectors::codex::daemon::{
    CodexActivity, DaemonClient, DaemonEvent, DaemonShutdown, EffectivePermissionMode, RelayKind,
    TaskaiTools,
};
use taskai_connectors::Error;
use tungstenite::{accept, accept_hdr, Message, WebSocket};

const WAIT: Duration = Duration::from_secs(2);

#[allow(clippy::result_large_err)] // tungstenite fixes the handshake callback's error response type.
fn fake_daemon<F>(socket: &Path, run: F) -> thread::JoinHandle<()>
where
    F: FnOnce(WebSocket<std::os::unix::net::UnixStream>) + Send + 'static,
{
    let listener = UnixListener::bind(socket).unwrap();
    thread::spawn(move || {
        let (stream, _) = listener.accept().unwrap();
        stream.set_read_timeout(Some(WAIT)).unwrap();
        let ws = accept_hdr(
            stream,
            |request: &tungstenite::handshake::server::Request, response| {
                assert_eq!(request.uri().path(), "/rpc");
                assert!(request
                    .headers()
                    .get("User-Agent")
                    .and_then(|value| value.to_str().ok())
                    .unwrap_or_default()
                    .starts_with("taskai-bridge/"));
                Ok(response)
            },
        )
        .unwrap();
        run(ws);
    })
}

fn recv(ws: &mut WebSocket<std::os::unix::net::UnixStream>) -> Value {
    serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap()
}

fn send(ws: &mut WebSocket<std::os::unix::net::UnixStream>, value: Value) {
    ws.send(Message::Text(value.to_string())).unwrap();
}

fn handshake(ws: &mut WebSocket<std::os::unix::net::UnixStream>) {
    let init = recv(ws);
    assert_eq!(init["method"], "initialize");
    assert_eq!(init["params"]["capabilities"]["experimentalApi"], true);
    send(
        ws,
        json!({"id":init["id"],"result":{"userAgent":"codex-tui/0.166.0 (test)"}}),
    );
    assert_eq!(recv(ws)["method"], "initialized");
}

fn start(ws: &mut WebSocket<std::os::unix::net::UnixStream>) {
    let request = recv(ws);
    assert_eq!(request["method"], "thread/start");
    let tools = &request["params"]["dynamicTools"][0];
    assert_eq!(tools["name"], "taskai");
    assert_eq!(tools["tools"][0]["name"], "list_sessions");
    assert_eq!(
        tools["tools"][0]["inputSchema"]["additionalProperties"],
        false
    );
    assert_eq!(tools["tools"][1]["name"], "send_message");
    assert_eq!(
        tools["tools"][1]["inputSchema"]["required"],
        json!(["to", "text"])
    );
    assert_eq!(
        tools["tools"][1]["inputSchema"]["properties"]["notify_when_idle"]["type"],
        "boolean"
    );
    send(
        ws,
        json!({"id":request["id"],"result":{"thread":{"id":"owned-thread"}}}),
    );
}

// Expectation: only an explicit thread/start creates a thread, with both TaskAI tools.
// Why: Codex dynamic tools are fixed at creation. Value: the bridge can launch without a model turn.
#[test]
fn initialize_and_create_tool_enabled_thread_without_turn() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        ws.get_mut()
            .set_read_timeout(Some(Duration::from_millis(200)))
            .unwrap();
        assert!(
            ws.read().is_err(),
            "client started a model turn without human input"
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    assert!(client.resume_command("codex").is_err());
    let init = client.initialize(WAIT).unwrap();
    assert_eq!(init.daemon_version.as_deref(), Some("0.166.0"));
    assert_eq!(
        client.start_thread(dir.path(), WAIT).unwrap(),
        "owned-thread"
    );
    assert_eq!(
        client.resume_command("codex").unwrap().1,
        vec!["resume", "owned-thread"]
    );
    assert!(DaemonClient::first_message_prompt().contains("first message"));
    server.join().unwrap();
}

// Expectation: a steered turn keeps the first letter's chain; each successful turn/start returns.
// Why: Codex emits no second turn/started for steering. Value: M6 can count each accepted delivery once.
#[test]
fn relay_preserves_native_turn_chain_when_steering() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        for i in 0..2 {
            let request = recv(&mut ws);
            assert_eq!(request["method"], "turn/start");
            assert_eq!(request["params"]["threadId"], "owned-thread");
            assert_eq!(request["params"]["input"][0]["type"], "text");
            assert_eq!(
                request["params"]["input"][0]["text"],
                if i == 0 { "Do work" } else { "More work" }
            );
            if i == 0 {
                send(
                    &mut ws,
                    json!({"method":"turn/started","params":{"threadId":"owned-thread","turn":{"id":"turn-1"}}}),
                );
            }
            send(
                &mut ws,
                json!({"id":request["id"],"result":{"turn":{"id":"turn-1"}}}),
            );
        }
        send(
            &mut ws,
            json!({"id":90,"method":"item/tool/call","params":{"threadId":"owned-thread","turnId":"turn-1","namespace":"taskai","tool":"send_message","arguments":{"to":"delegate","text":"reply"}}}),
        );
        assert_eq!(recv(&mut ws)["id"], 90);
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    let first = client
        .relay(
            RelayKind::Message,
            "Do work",
            Some(vec!["hop-a".into()]),
            WAIT,
        )
        .unwrap();
    let second = client
        .relay(
            RelayKind::Message,
            "More work",
            Some(vec!["hop-b".into()]),
            WAIT,
        )
        .unwrap();
    assert_eq!(first.turn_id, "turn-1");
    assert_eq!(second.turn_id, "turn-1");
    assert_eq!(
        client.chain_for_turn("turn-1"),
        Some(&["hop-a".to_string()][..])
    );
    assert_eq!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::TurnStarted("turn-1".into()))
    );
    let Some(DaemonEvent::ToolCall(call)) = client.next_event(WAIT).unwrap() else {
        panic!("expected owned tool call")
    };
    let mut tools = Tools { calls: vec![] };
    client.answer_tool_call(call, &mut tools).unwrap();
    assert_eq!(
        tools.calls,
        vec![(
            "delegate".into(),
            "reply".into(),
            false,
            Some(vec!["hop-a".into()])
        )]
    );
    server.join().unwrap();
}

struct Tools {
    calls: Vec<(String, String, bool, Option<Vec<String>>)>,
}
impl TaskaiTools for Tools {
    fn list_sessions(&mut self) -> Result<Value, String> {
        Ok(json!({"sessions":[]}))
    }
    fn send_message(
        &mut self,
        to: &str,
        text: &str,
        notify_when_idle: bool,
        chain: Option<&[String]>,
    ) -> Result<Value, String> {
        self.calls.push((
            to.into(),
            text.into(),
            notify_when_idle,
            chain.map(<[String]>::to_vec),
        ));
        Ok(json!({"status":"sent"}))
    }
}

// Expectation: unrelated thread calls and approvals are never answered; only exact TaskAI tools dispatch.
// Why: the daemon multiplexes human sessions. Value: TaskAI cannot act for another owner or approve work.
#[test]
fn tool_calls_filter_thread_and_never_answer_approvals() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        send(
            &mut ws,
            json!({"id":81,"method":"item/tool/call","params":{"threadId":"other-thread","namespace":"taskai","tool":"send_message","arguments":{"to":"x","text":"wrong"}}}),
        );
        send(
            &mut ws,
            json!({"id":82,"method":"item/tool/call","params":{"threadId":"owned-thread","namespace":"other","tool":"send_message","arguments":{"to":"x","text":"wrong"}}}),
        );
        send(
            &mut ws,
            json!({"id":85,"method":"item/commandExecution/requestApproval","params":{"threadId":"owned-thread","turnId":"turn-1","command":"fake command"}}),
        );
        send(
            &mut ws,
            json!({"id":83,"method":"item/tool/call","params":{"threadId":"owned-thread","namespace":"taskai","tool":"send_message","arguments":{"to":"peer","text":"hello","notify_when_idle":true,"from":"forged"}}}),
        );
        send(
            &mut ws,
            json!({"id":84,"method":"item/tool/call","params":{"threadId":"owned-thread","namespace":"taskai","tool":"send_message","arguments":{"to":"peer","text":"hello","notify_when_idle":true}}}),
        );
        let invalid = recv(&mut ws);
        assert_eq!(invalid["id"], 83);
        assert_eq!(invalid["result"]["success"], false);
        let valid = recv(&mut ws);
        assert_eq!(valid["id"], 84);
        assert_eq!(valid["result"]["success"], true);
        assert_eq!(valid["result"]["contentItems"][0]["type"], "inputText");
        ws.get_mut()
            .set_read_timeout(Some(Duration::from_millis(100)))
            .unwrap();
        assert!(ws.read().is_err(), "approval request was answered");
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    let mut tools = Tools { calls: vec![] };
    assert!(
        matches!(client.next_event(WAIT).unwrap(), Some(DaemonEvent::UnansweredRequest { method }) if method == "item/tool/call")
    );
    assert!(
        matches!(client.next_event(WAIT).unwrap(), Some(DaemonEvent::UnansweredRequest { method }) if method == "item/commandExecution/requestApproval")
    );
    for _ in 0..2 {
        let Some(DaemonEvent::ToolCall(call)) = client.next_event(WAIT).unwrap() else {
            panic!("expected tool call")
        };
        client.answer_tool_call(call, &mut tools).unwrap();
    }
    assert_eq!(
        tools.calls,
        vec![("peer".into(), "hello".into(), true, None)]
    );
    server.join().unwrap();
}

// Expectation: only the owned thread updates activity; interrupt and unarchive use exact IDs.
// Why: approval state and recovery belong in the human's pane. Value: M6 mirrors truth safely.
#[test]
fn status_detail_interrupt_and_unarchive() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        send(
            &mut ws,
            json!({"method":"thread/status/changed","params":{"threadId":"other-thread","status":{"type":"active","activeFlags":["waitingOnApproval"]}}}),
        );
        send(
            &mut ws,
            json!({"method":"thread/status/changed","params":{"threadId":"owned-thread","status":{"type":"active","activeFlags":["waitingOnApproval"]}}}),
        );
        send(
            &mut ws,
            json!({"method":"item/completed","params":{"threadId":"owned-thread","item":{"type":"agentMessage","text":"done"}}}),
        );
        for status in [
            json!({"type":"notLoaded"}),
            json!({"type":"active","activeFlags":[]}),
            json!({"type":"idle"}),
        ] {
            send(
                &mut ws,
                json!({"method":"thread/status/changed","params":{"threadId":"owned-thread","status":status}}),
            );
        }
        for method in ["turn/interrupt", "thread/unarchive"] {
            let request = recv(&mut ws);
            assert_eq!(request["method"], method);
            assert_eq!(request["params"]["threadId"], "owned-thread");
            if method == "turn/interrupt" {
                assert_eq!(request["params"]["turnId"], "turn-1");
            }
            send(&mut ws, json!({"id":request["id"],"result":{}}));
        }
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    assert_eq!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::Activity(CodexActivity::WaitingApproval))
    );
    assert_eq!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::Detail("done".into()))
    );
    for expected in [
        CodexActivity::Idle,
        CodexActivity::Busy,
        CodexActivity::Idle,
    ] {
        assert_eq!(
            client.next_event(WAIT).unwrap(),
            Some(DaemonEvent::Activity(expected))
        );
    }
    client.interrupt("turn-1", WAIT).unwrap();
    client.unarchive(WAIT).unwrap();
    server.join().unwrap();
}

// Expectation: startup notifications arriving before thread/start's reply are replayed for that thread.
// Why: the daemon may publish status immediately. Value: the registry does not begin with stale activity.
#[test]
fn replays_owned_status_emitted_before_thread_start_reply() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let (release, hold) = std::sync::mpsc::channel();
    let server = fake_daemon(&socket, move |mut ws| {
        handshake(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "thread/start");
        send(
            &mut ws,
            json!({"method":"thread/status/changed","params":{"threadId":"other-thread","status":{"type":"active","activeFlags":[]}}}),
        );
        send(
            &mut ws,
            json!({"method":"thread/status/changed","params":{"threadId":"owned-thread","status":{"type":"active","activeFlags":["waitingOnUserInput"]}}}),
        );
        send(
            &mut ws,
            json!({"id":request["id"],"result":{"thread":{"id":"owned-thread"}}}),
        );
        hold.recv_timeout(WAIT).unwrap();
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    assert_eq!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::Activity(CodexActivity::WaitingInput))
    );
    release.send(()).unwrap();
    server.join().unwrap();
}

// Expectation: a first human message is sent only on an explicit call, and provider errors surface.
// Why: TaskAI may offer an empty-thread resume fallback but cannot start a model turn on its own.
// Value: the person owns the first turn, with a clear failure if the daemon rejects it.
#[test]
fn first_human_message_requires_explicit_text_and_surfaces_daemon_error() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "turn/start");
        assert_eq!(request["params"]["input"][0]["text"], "hello from person");
        send(
            &mut ws,
            json!({"id":request["id"],"error":{"code":-32000,"message":"thread unavailable"}}),
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    assert!(client.first_human_message("   ", WAIT).is_err());
    assert!(client
        .first_human_message("hello from person", WAIT)
        .unwrap_err()
        .to_string()
        .contains("thread unavailable"));
    server.join().unwrap();
}

// Expectation: mailbox-prepared notices reach Codex exactly as supplied and start no peer chain.
// Why: M1 owns notice wording and correlation. Value: no duplicate label or invented instruction.
#[test]
fn relay_uses_mailbox_prepared_notice_text_without_a_chain() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "turn/start");
        assert_eq!(
            request["params"]["input"][0]["text"],
            "[TaskAI notice] ready text"
        );
        send(
            &mut ws,
            json!({"id":request["id"],"result":{"turn":{"id":"notice-turn"}}}),
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    client
        .relay(
            RelayKind::Notice,
            "[TaskAI notice] ready text",
            Some(vec!["forged".into()]),
            WAIT,
        )
        .unwrap();
    assert_eq!(client.chain_for_turn("notice-turn"), None);
    server.join().unwrap();
}

// Expectation: a turn ending just before the next turn/start cannot erase the next letter's chain.
// Why: completion and new-start notifications can precede or follow the request result.
// Value: replies from the new turn preserve the right hop lineage, while steering keeps the old one.
#[test]
fn turn_boundary_assigns_chain_to_result_turn_in_both_event_orders() {
    for started_before_reply in [true, false] {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("daemon.sock");
        let server = fake_daemon(&socket, move |mut ws| {
            handshake(&mut ws);
            start(&mut ws);
            let first = recv(&mut ws);
            assert_eq!(first["method"], "turn/start");
            send(
                &mut ws,
                json!({"method":"turn/started","params":{"threadId":"owned-thread","turn":{"id":"turn-1"}}}),
            );
            send(
                &mut ws,
                json!({"id":first["id"],"result":{"turn":{"id":"turn-1"}}}),
            );
            let second = recv(&mut ws);
            assert_eq!(second["method"], "turn/start");
            send(
                &mut ws,
                json!({"method":"turn/completed","params":{"threadId":"owned-thread","turn":{"id":"turn-1"}}}),
            );
            send(
                &mut ws,
                json!({"method":"turn/started","params":{"threadId":"owned-thread","turn":{"id":"unrelated"}}}),
            );
            if started_before_reply {
                send(
                    &mut ws,
                    json!({"method":"turn/started","params":{"threadId":"owned-thread","turn":{"id":"turn-2"}}}),
                );
            }
            send(
                &mut ws,
                json!({"id":second["id"],"result":{"turn":{"id":"turn-2"}}}),
            );
            if !started_before_reply {
                send(
                    &mut ws,
                    json!({"method":"turn/started","params":{"threadId":"owned-thread","turn":{"id":"turn-2"}}}),
                );
            }
            send(
                &mut ws,
                json!({"id":90,"method":"item/tool/call","params":{"threadId":"owned-thread","turnId":"turn-2","namespace":"taskai","tool":"send_message","arguments":{"to":"delegate","text":"reply"}}}),
            );
            assert_eq!(recv(&mut ws)["id"], 90);
        });
        let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
        client.initialize(WAIT).unwrap();
        client.start_thread(dir.path(), WAIT).unwrap();
        client
            .relay(RelayKind::Message, "first", Some(vec!["old".into()]), WAIT)
            .unwrap();
        client
            .relay(RelayKind::Message, "second", Some(vec!["new".into()]), WAIT)
            .unwrap();
        let mut tools = Tools { calls: vec![] };
        for _ in 0..5 {
            if let Some(DaemonEvent::ToolCall(call)) = client.next_event(WAIT).unwrap() {
                client.answer_tool_call(call, &mut tools).unwrap();
                break;
            }
        }
        assert_eq!(client.chain_for_turn("unrelated"), None);
        assert_eq!(
            tools.calls,
            vec![(
                "delegate".into(),
                "reply".into(),
                false,
                Some(vec!["new".into()])
            )]
        );
        server.join().unwrap();
    }
}

// Expectation: a tool queued during another turn/start keeps its old turn's known origin.
// Why: a following completion can remove the turn map before the queued call is dispatched.
// Value: peer lineage survives, while a human-started turn still sends without a chain.
#[test]
fn queued_old_turn_tool_keeps_origin_after_completion_during_relay() {
    for first_chain in [Some(vec!["peer".into()]), None] {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("daemon.sock");
        let server = fake_daemon(&socket, |mut ws| {
            handshake(&mut ws);
            start(&mut ws);
            let first = recv(&mut ws);
            send(
                &mut ws,
                json!({"id":first["id"],"result":{"turn":{"id":"turn-old"}}}),
            );
            let second = recv(&mut ws);
            assert_eq!(second["method"], "turn/start");
            send(
                &mut ws,
                json!({"id":94,"method":"item/tool/call","params":{"threadId":"owned-thread","turnId":"turn-old","namespace":"taskai","tool":"send_message","arguments":{"to":"delegate","text":"old reply"}}}),
            );
            send(
                &mut ws,
                json!({"method":"turn/completed","params":{"threadId":"owned-thread","turn":{"id":"turn-old"}}}),
            );
            send(
                &mut ws,
                json!({"id":second["id"],"result":{"turn":{"id":"turn-next"}}}),
            );
            let response = recv(&mut ws);
            assert_eq!(response["id"], 94);
            assert_eq!(response["result"]["success"], true);
        });
        let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
        client.initialize(WAIT).unwrap();
        client.start_thread(dir.path(), WAIT).unwrap();
        client
            .relay(RelayKind::Message, "first", first_chain.clone(), WAIT)
            .unwrap();
        client
            .relay(
                RelayKind::Message,
                "second",
                Some(vec!["next".into()]),
                WAIT,
            )
            .unwrap();
        let Some(DaemonEvent::ToolCall(call)) = client.next_event(WAIT).unwrap() else {
            panic!("expected queued old-turn tool call")
        };
        let mut tools = Tools { calls: vec![] };
        client.answer_tool_call(call, &mut tools).unwrap();
        assert_eq!(tools.calls.len(), 1);
        assert_eq!(tools.calls[0].3, first_chain);
        server.join().unwrap();
    }
}

// Expectation: a new turn's queued tool gets the request-result chain even if completion came first.
// Why: the result is the only reliable turn ID to letter-chain correlation for steering.
// Value: an early tool cannot be sent as human-originated merely because the turn is complete.
#[test]
fn queued_result_turn_tool_keeps_chain_when_completed_before_ack() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "turn/start");
        send(
            &mut ws,
            json!({"method":"turn/started","params":{"threadId":"owned-thread","turn":{"id":"turn-new"}}}),
        );
        send(
            &mut ws,
            json!({"id":95,"method":"item/tool/call","params":{"threadId":"owned-thread","turnId":"turn-new","namespace":"taskai","tool":"send_message","arguments":{"to":"delegate","text":"early reply"}}}),
        );
        send(
            &mut ws,
            json!({"method":"turn/completed","params":{"threadId":"owned-thread","turn":{"id":"turn-new"}}}),
        );
        send(
            &mut ws,
            json!({"id":request["id"],"result":{"turn":{"id":"turn-new"}}}),
        );
        let response = recv(&mut ws);
        assert_eq!(response["id"], 95);
        assert_eq!(response["result"]["success"], true);
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    client
        .relay(
            RelayKind::Message,
            "peer letter",
            Some(vec!["peer".into()]),
            WAIT,
        )
        .unwrap();
    assert!(matches!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::TurnStarted(_))
    ));
    let Some(DaemonEvent::ToolCall(call)) = client.next_event(WAIT).unwrap() else {
        panic!("expected queued new-turn tool call")
    };
    let mut tools = Tools { calls: vec![] };
    client.answer_tool_call(call, &mut tools).unwrap();
    assert_eq!(tools.calls[0].3, Some(vec!["peer".into()]));
    server.join().unwrap();
}

// Expectation: a tool first observed after its completed peer turn cannot invent a human origin.
// Why: no queued snapshot or active map can prove its chain at that point.
// Value: the bridge returns an explicit tool error instead of an unchained send.
#[test]
fn late_completed_peer_tool_is_refused_when_origin_was_not_queued() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let request = recv(&mut ws);
        send(
            &mut ws,
            json!({"id":request["id"],"result":{"turn":{"id":"turn-peer"}}}),
        );
        send(
            &mut ws,
            json!({"method":"turn/completed","params":{"threadId":"owned-thread","turn":{"id":"turn-peer"}}}),
        );
        send(
            &mut ws,
            json!({"id":96,"method":"item/tool/call","params":{"threadId":"owned-thread","turnId":"turn-peer","namespace":"taskai","tool":"send_message","arguments":{"to":"delegate","text":"late reply"}}}),
        );
        let response = recv(&mut ws);
        assert_eq!(response["id"], 96);
        assert_eq!(response["result"]["success"], false);
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    client
        .relay(
            RelayKind::Message,
            "peer letter",
            Some(vec!["peer".into()]),
            WAIT,
        )
        .unwrap();
    assert!(matches!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::TurnCompleted(_))
    ));
    let Some(DaemonEvent::ToolCall(call)) = client.next_event(WAIT).unwrap() else {
        panic!("expected late tool call")
    };
    let mut tools = Tools { calls: vec![] };
    client.answer_tool_call(call, &mut tools).unwrap();
    assert!(tools.calls.is_empty());
    server.join().unwrap();
}

// Expectation: a verified registry thread is resumed and subscribed, with no new thread or turn.
// Why: a bridge may be recreated while its native pane remains live. Value: exact session continuity.
#[test]
fn attach_resubscribes_to_exact_owned_thread_and_restores_tools() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "thread/resume");
        assert_eq!(
            request["params"],
            json!({"threadId":"owned-thread","excludeTurns":true})
        );
        send(
            &mut ws,
            json!({"method":"thread/status/changed","params":{"threadId":"owned-thread","status":{"type":"idle"}}}),
        );
        send(
            &mut ws,
            json!({"id":request["id"],"result":{"thread":{"id":"owned-thread"},"approvalPolicy":"never"}}),
        );
        send(
            &mut ws,
            json!({"id":91,"method":"item/tool/call","params":{"threadId":"owned-thread","turnId":"turn-1","namespace":"taskai","tool":"list_sessions","arguments":{}}}),
        );
        assert_eq!(recv(&mut ws)["id"], 91);
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    assert!(client.attach_owned_thread("", WAIT).is_err());
    assert_eq!(
        client.attach_owned_thread("owned-thread", WAIT).unwrap(),
        "owned-thread"
    );
    assert_eq!(client.thread_id(), Some("owned-thread"));
    assert_eq!(
        client.effective_permission().unwrap().mode,
        EffectivePermissionMode::Bypass
    );
    assert_eq!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::Activity(CodexActivity::Idle))
    );
    let Some(DaemonEvent::ToolCall(call)) = client.next_event(WAIT).unwrap() else {
        panic!("expected resumed tool call")
    };
    client
        .answer_tool_call(call, &mut Tools { calls: vec![] })
        .unwrap();
    server.join().unwrap();
}

// Expectation: a mismatched resume response never binds the client to another thread.
// Why: daemon subscriptions are multiplexed. Value: no cross-session tool or status routing.
#[test]
fn attach_rejects_returned_thread_identity_mismatch() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "thread/resume");
        send(
            &mut ws,
            json!({"id":request["id"],"result":{"thread":{"id":"other-thread"},"approvalPolicy":"on-request"}}),
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    assert!(client
        .attach_owned_thread("owned-thread", WAIT)
        .unwrap_err()
        .to_string()
        .contains("thread.id mismatch"));
    assert_eq!(client.thread_id(), None);
    assert_eq!(client.effective_permission(), None);
    server.join().unwrap();
}

// Expectation: resume's live status snapshot is available even without a follow-up notification.
// Why: a recreated bridge must show an already waiting pane accurately. Value: M6 mirrors approval wait.
#[test]
fn attach_surfaces_live_status_from_resume_response() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let (release, hold) = std::sync::mpsc::channel();
    let server = fake_daemon(&socket, move |mut ws| {
        handshake(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "thread/resume");
        send(
            &mut ws,
            json!({"id":request["id"],"result":{
                "thread":{"id":"owned-thread","status":{"type":"active","activeFlags":["waitingOnApproval"]}},
                "approvalPolicy":"on-request"
            }}),
        );
        hold.recv_timeout(WAIT).unwrap();
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.attach_owned_thread("owned-thread", WAIT).unwrap();
    assert_eq!(
        client.effective_permission().unwrap().mode,
        EffectivePermissionMode::Prompting
    );
    assert_eq!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::Activity(CodexActivity::WaitingApproval))
    );
    release.send(()).unwrap();
    server.join().unwrap();
}

// Expectation: native approvalPolicy survives creation without TaskAI choosing a policy.
// Why: mailbox sender mode must reflect the native effective mode. Value: honest peer envelopes.
#[test]
fn start_thread_exposes_prompting_bypass_and_unknown_policies() {
    for (native, expected) in [
        (Some("on-request"), EffectivePermissionMode::Prompting),
        (Some("never"), EffectivePermissionMode::Bypass),
        (None, EffectivePermissionMode::Unknown),
        (Some("future-policy"), EffectivePermissionMode::Unknown),
    ] {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("daemon.sock");
        let server = fake_daemon(&socket, move |mut ws| {
            handshake(&mut ws);
            let request = recv(&mut ws);
            assert_eq!(request["method"], "thread/start");
            assert!(request["params"].get("approvalPolicy").is_none());
            let mut result = json!({"thread":{"id":"owned-thread"}});
            if let Some(policy) = native {
                result["approvalPolicy"] = json!(policy);
            }
            send(&mut ws, json!({"id":request["id"],"result":result}));
        });
        let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
        client.initialize(WAIT).unwrap();
        client.start_thread(dir.path(), WAIT).unwrap();
        let policy = client.effective_permission().unwrap();
        assert_eq!(policy.mode, expected);
        assert_eq!(policy.native_approval_policy.as_deref(), native);
        server.join().unwrap();
    }
}

// Expectation: Ping frames cannot extend either event or request deadlines indefinitely.
// Why: the daemon may send control traffic without JSON work. Value: bridge loops stay responsive.
#[test]
fn control_frames_respect_event_and_request_deadlines() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        for _ in 0..80 {
            if ws.send(Message::Ping(Vec::new())).is_err() {
                break;
            }
            thread::sleep(Duration::from_millis(5));
        }
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    let begin = Instant::now();
    let event = client.next_event(Duration::from_millis(80));
    let event_elapsed = begin.elapsed();
    let begin = Instant::now();
    let request = client.unarchive(Duration::from_millis(80));
    let request_elapsed = begin.elapsed();
    drop(client);
    server.join().unwrap();
    assert!(matches!(event, Ok(None)), "{event:?}");
    assert!(request.unwrap_err().to_string().contains("timeout"));
    assert!(
        event_elapsed < Duration::from_millis(220),
        "event took {event_elapsed:?}"
    );
    assert!(
        request_elapsed < Duration::from_millis(220),
        "request took {request_elapsed:?}"
    );
}

// Expectation: a complete turn/start request followed by EOF has uncertain acceptance.
// Why: the daemon may have accepted the turn before losing its reply. Value: no false dropped receipt.
#[test]
fn relay_eof_after_complete_request_is_uncertain() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        assert_eq!(recv(&mut ws)["method"], "turn/start");
        ws.get_mut().shutdown(Shutdown::Both).unwrap();
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    assert!(matches!(
        client.relay(
            RelayKind::Message,
            "letter",
            Some(vec!["peer".into()]),
            WAIT
        ),
        Err(Error::Uncertain(_))
    ));
    assert!(matches!(
        client.relay(
            RelayKind::Message,
            "later",
            Some(vec!["other".into()]),
            WAIT
        ),
        Err(Error::Invalid(_))
    ));
    server.join().unwrap();
}

// Expectation: a daemon's explicit JSON-RPC error is a definite rejection, so a later relay may write.
// Why: an error response proves the provider did not accept that request. Value: M6 can fail it by ID.
#[test]
fn relay_rpc_rejection_is_definite_and_allows_later_write() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let rejected = recv(&mut ws);
        assert_eq!(rejected["method"], "turn/start");
        send(
            &mut ws,
            json!({"id":rejected["id"],"error":{"code":-32000,"message":"provider rejected"}}),
        );
        let accepted = recv(&mut ws);
        assert_eq!(accepted["method"], "turn/start");
        assert_eq!(accepted["params"]["input"][0]["text"], "later");
        send(
            &mut ws,
            json!({"id":accepted["id"],"result":{"turn":{"id":"turn-later"}}}),
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    assert!(matches!(
        client.relay(
            RelayKind::Message,
            "rejected",
            Some(vec!["peer".into()]),
            WAIT
        ),
        Err(Error::Invalid(_))
    ));
    assert_eq!(
        client
            .relay(RelayKind::Message, "later", Some(vec!["new".into()]), WAIT)
            .unwrap()
            .turn_id,
        "turn-later"
    );
    server.join().unwrap();
}

// Expectation: a lost acknowledgment freezes future relay writes but keeps a known old turn's chain.
// Why: completion may arrive during the failed request and tools may arrive afterward.
// Value: known late replies retain lineage, while unknown turns get an explicit tool error.
#[test]
fn relay_timeout_refuses_second_write_and_handles_late_tools_safely() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let (release, hold) = std::sync::mpsc::channel();
    let server = fake_daemon(&socket, move |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let first = recv(&mut ws);
        send(
            &mut ws,
            json!({"id":first["id"],"result":{"turn":{"id":"turn-old"}}}),
        );
        let uncertain = recv(&mut ws);
        assert_eq!(uncertain["method"], "turn/start");
        send(
            &mut ws,
            json!({"method":"turn/completed","params":{"threadId":"owned-thread","turn":{"id":"turn-old"}}}),
        );
        hold.recv_timeout(WAIT).unwrap();
        for (id, turn_id) in [(91, "turn-old"), (92, "turn-unknown")] {
            send(
                &mut ws,
                json!({"id":id,"method":"item/tool/call","params":{"threadId":"owned-thread","turnId":turn_id,"namespace":"taskai","tool":"send_message","arguments":{"to":"delegate","text":"reply"}}}),
            );
            let response = recv(&mut ws);
            assert_eq!(response["id"], id);
            assert_eq!(response["result"]["success"], id == 91);
        }
        ws.get_mut()
            .set_read_timeout(Some(Duration::from_millis(100)))
            .unwrap();
        assert!(
            ws.read().is_err(),
            "a second relay was written after uncertainty"
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    client
        .relay(
            RelayKind::Message,
            "first",
            Some(vec!["old-chain".into()]),
            WAIT,
        )
        .unwrap();
    assert!(matches!(
        client.relay(
            RelayKind::Message,
            "uncertain",
            Some(vec!["new-chain".into()]),
            Duration::from_millis(80)
        ),
        Err(Error::Uncertain(_))
    ));
    assert!(matches!(
        client.relay(
            RelayKind::Message,
            "later",
            Some(vec!["later".into()]),
            WAIT
        ),
        Err(Error::Invalid(_))
    ));
    release.send(()).unwrap();
    let mut tools = Tools { calls: vec![] };
    let mut answered = 0;
    while answered < 2 {
        match client.next_event(WAIT).unwrap() {
            Some(DaemonEvent::ToolCall(call)) => {
                client.answer_tool_call(call, &mut tools).unwrap();
                answered += 1;
            }
            Some(DaemonEvent::TurnCompleted(_)) => {}
            other => panic!("unexpected event: {other:?}"),
        }
    }
    assert_eq!(
        tools.calls,
        vec![(
            "delegate".into(),
            "reply".into(),
            false,
            Some(vec!["old-chain".into()])
        )]
    );
    server.join().unwrap();
}

// Expectation: a successful-looking result without turn.id is uncertain and blocks later writes.
// Why: an accepted new turn may be running but cannot be correlated to the delivery's chain.
// Value: a late tool on that turn gets an error instead of an invented human-started chain.
#[test]
fn relay_malformed_success_is_uncertain_and_rejects_unknown_turn_tool() {
    for result in [
        json!({"turn":{}}),
        json!({"turn":{"id":3}}),
        json!({"other":true}),
    ] {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("daemon.sock");
        let server = fake_daemon(&socket, move |mut ws| {
            handshake(&mut ws);
            start(&mut ws);
            let request = recv(&mut ws);
            assert_eq!(request["method"], "turn/start");
            send(
                &mut ws,
                json!({"method":"turn/started","params":{"threadId":"owned-thread","turn":{"id":"turn-unattributed"}}}),
            );
            send(&mut ws, json!({"id":request["id"],"result":result}));
            send(
                &mut ws,
                json!({"id":93,"method":"item/tool/call","params":{"threadId":"owned-thread","turnId":"turn-unattributed","namespace":"taskai","tool":"send_message","arguments":{"to":"delegate","text":"reply"}}}),
            );
            let response = recv(&mut ws);
            assert_eq!(response["id"], 93);
            assert_eq!(response["result"]["success"], false);
            ws.get_mut()
                .set_read_timeout(Some(Duration::from_millis(100)))
                .unwrap();
            assert!(
                ws.read().is_err(),
                "a later relay was written after malformed ack"
            );
        });
        let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
        client.initialize(WAIT).unwrap();
        client.start_thread(dir.path(), WAIT).unwrap();
        assert!(matches!(
            client.relay(
                RelayKind::Message,
                "letter",
                Some(vec!["peer".into()]),
                WAIT
            ),
            Err(Error::Uncertain(_))
        ));
        assert!(matches!(
            client.relay(
                RelayKind::Message,
                "later",
                Some(vec!["later".into()]),
                WAIT
            ),
            Err(Error::Invalid(_))
        ));
        let mut tools = Tools { calls: vec![] };
        let call = loop {
            match client.next_event(WAIT).unwrap() {
                Some(DaemonEvent::TurnStarted(_)) => {}
                Some(DaemonEvent::ToolCall(call)) => break call,
                other => panic!("expected late tool call, got {other:?}"),
            }
        };
        client.answer_tool_call(call, &mut tools).unwrap();
        assert!(tools.calls.is_empty());
        server.join().unwrap();
    }
}

// Expectation: a response with neither result nor error is an uncertain acknowledgment.
// Why: the request may have started a turn even though its correlation data was omitted.
// Value: M6 retains the in-flight delivery instead of emitting a false nondelivery receipt.
#[test]
fn relay_missing_result_is_uncertain() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "turn/start");
        send(&mut ws, json!({"id":request["id"]}));
        ws.get_mut()
            .set_read_timeout(Some(Duration::from_millis(100)))
            .unwrap();
        assert!(ws.read().is_err(), "a second turn/start was written");
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    assert!(matches!(
        client.relay(
            RelayKind::Message,
            "letter",
            Some(vec!["peer".into()]),
            WAIT
        ),
        Err(Error::Uncertain(_))
    ));
    assert!(matches!(
        client.relay(RelayKind::Message, "later", Some(vec!["peer".into()]), WAIT),
        Err(Error::Invalid(_))
    ));
    server.join().unwrap();
}

// Expectation: without an owned thread, relay fails before a provider write is attempted.
// Why: the bridge has no valid destination thread. Value: M6 can fail this delivery by ID.
#[test]
fn relay_without_owned_thread_is_definite_no_write() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        ws.get_mut()
            .set_read_timeout(Some(Duration::from_millis(100)))
            .unwrap();
        assert!(
            ws.read().is_err(),
            "turn/start was written before thread ownership"
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    assert!(matches!(
        client.relay(
            RelayKind::Message,
            "letter",
            Some(vec!["peer".into()]),
            WAIT
        ),
        Err(Error::NotAttached(_))
    ));
    server.join().unwrap();
}

// Expectation: incomplete JSON-RPC errors cannot prove that turn/start was rejected.
// Why: both a numeric code and string message are required for a definite rejection.
// Value: the attempted delivery remains uncertain, while a later blocked one fails definitely.
#[test]
fn relay_partial_rpc_error_is_uncertain_but_later_refusal_is_definite() {
    for partial_error in [
        json!({"code":-32000}),
        json!({"message":"partial rejection"}),
        json!({"code":"not-numeric","message":"partial rejection"}),
    ] {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("daemon.sock");
        let server = fake_daemon(&socket, move |mut ws| {
            handshake(&mut ws);
            start(&mut ws);
            let request = recv(&mut ws);
            assert_eq!(request["method"], "turn/start");
            send(&mut ws, json!({"id":request["id"],"error":partial_error}));
            ws.get_mut()
                .set_read_timeout(Some(Duration::from_millis(100)))
                .unwrap();
            assert!(
                ws.read().is_err(),
                "a later blocked relay reached the daemon"
            );
        });
        let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
        client.initialize(WAIT).unwrap();
        client.start_thread(dir.path(), WAIT).unwrap();
        assert!(matches!(
            client.relay(RelayKind::Message, "first", Some(vec!["peer".into()]), WAIT),
            Err(Error::Uncertain(_))
        ));
        assert!(matches!(
            client.relay(RelayKind::Message, "later", Some(vec!["peer".into()]), WAIT),
            Err(Error::Invalid(_))
        ));
        server.join().unwrap();
    }
}

/// Accepts one `/rpc` connection on `listener` for a fake daemon serving several clients.
fn accept_rpc(listener: &UnixListener) -> WebSocket<std::os::unix::net::UnixStream> {
    let (stream, _) = listener.accept().unwrap();
    stream
        .set_read_timeout(Some(Duration::from_secs(10)))
        .unwrap();
    accept(stream).unwrap()
}

/// Answers the next request, which must be turn/start, with `turn_id`.
fn answer_turn(ws: &mut WebSocket<std::os::unix::net::UnixStream>, turn_id: &str) {
    let request = recv(ws);
    assert_eq!(request["method"], "turn/start");
    send(
        ws,
        json!({"id":request["id"],"result":{"turn":{"id":turn_id}}}),
    );
}

fn shared_across_threads<T: Send + Sync>(_: &T) {}

// Expectation: a shutdown handle wakes its client's turn/start, written and waiting for an answer,
// at once and far inside its timeout, as uncertain, and the earlier turn keeps its proven chain;
// calling it again is harmless, and the client's next relay is refused before any write. The handle sends the daemon nothing, and another
// connection to the same daemon keeps working, as does a connection made after its client is gone.
// Why: the bridge must stop waiting on a relay without waiting out its timeout, while the daemon
// and its other connections belong to others. Value: shutdown is prompt and stays inside one
// connection.
#[test]
fn a_shutdown_handle_wakes_a_blocked_relay_and_closes_only_its_own_connection() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let listener = UnixListener::bind(&socket).unwrap();
    let (written, relay_written) = mpsc::channel();
    // Each connection is served on its own thread: when the daemon sees the first one end must
    // not hold up the others.
    let daemon = thread::spawn(move || {
        let mut first = accept_rpc(&listener);
        handshake(&mut first);
        start(&mut first);
        let mut second = accept_rpc(&listener);
        handshake(&mut second);
        start(&mut second);
        let second_served = thread::spawn(move || {
            answer_turn(&mut second, "turn-second");
            second
        });
        answer_turn(&mut first, "turn-1");
        assert_eq!(recv(&mut first)["method"], "turn/start");
        written.send(()).unwrap();
        let first_read = thread::spawn(move || {
            let mut after_turn_start = Vec::new();
            while let Ok(message) = first.read() {
                after_turn_start.push(message.to_string());
            }
            after_turn_start
        });
        let mut third = accept_rpc(&listener);
        handshake(&mut third);
        start(&mut third);
        answer_turn(&mut third, "turn-third");
        (first_read, second_served.join().unwrap(), third)
    });
    let mut first = DaemonClient::connect(&socket, WAIT).unwrap();
    first.initialize(WAIT).unwrap();
    first.start_thread(dir.path(), WAIT).unwrap();
    let mut second = DaemonClient::connect(&socket, WAIT).unwrap();
    second.initialize(WAIT).unwrap();
    second.start_thread(dir.path(), WAIT).unwrap();
    let proven = first
        .relay(
            RelayKind::Message,
            "earlier",
            Some(vec!["peer-1".into()]),
            WAIT,
        )
        .unwrap();
    assert_eq!(proven.turn_id, "turn-1");
    let handle: DaemonShutdown = first.shutdown_handle().unwrap();
    shared_across_threads(&handle);
    let relay = thread::spawn(move || {
        let started = Instant::now();
        let result = first.relay(
            RelayKind::Message,
            "letter",
            Some(vec!["peer".into()]),
            Duration::from_secs(30),
        );
        (result, started.elapsed(), first)
    });
    relay_written.recv_timeout(WAIT).unwrap();
    handle.shutdown();
    handle.shutdown();
    let (result, waited, mut first) = relay.join().unwrap();
    assert!(matches!(result, Err(Error::Uncertain(_))), "{result:?}");
    assert!(
        waited < Duration::from_secs(5),
        "the relay waited {waited:?}"
    );
    assert_eq!(
        first.chain_for_turn("turn-1"),
        Some(&["peer-1".to_string()][..])
    );
    assert!(matches!(
        first.relay(RelayKind::Message, "later", None, WAIT),
        Err(Error::Invalid(_))
    ));
    let still_served = second
        .relay(RelayKind::Message, "still served", None, WAIT)
        .unwrap();
    assert_eq!(still_served.turn_id, "turn-second");
    drop(first);
    let mut third = DaemonClient::connect(&socket, WAIT).unwrap();
    third.initialize(WAIT).unwrap();
    third.start_thread(dir.path(), WAIT).unwrap();
    handle.shutdown();
    let after_old_handle = third
        .relay(RelayKind::Message, "after the old handle", None, WAIT)
        .unwrap();
    assert_eq!(after_old_handle.turn_id, "turn-third");
    drop(handle);
    let (first_read, _second, _third) = daemon.join().unwrap();
    let after_turn_start = first_read.join().unwrap();
    assert!(
        after_turn_start.is_empty(),
        "the handle wrote to the daemon: {after_turn_start:?}"
    );
}

// Expectation: once the client and its shutdown handle are both dropped, the daemon sees the
// connection end at once, not a read timeout. Why: the handle keeps a duplicate of the socket, and
// on macOS a shutdown does not always reach the daemon while any descriptor remains, so only
// closing them all ends the connection for certain. Value: a bridge that drops its client and its
// handle leaves no daemon connection open.
#[test]
fn dropping_the_client_and_its_handle_ends_the_connection() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let (ended, connection_end) = mpsc::channel();
    let server = fake_daemon(&socket, move |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let started = Instant::now();
        let end = ws.read();
        let timed_out = matches!(
            &end,
            Err(tungstenite::Error::Io(error))
                if matches!(error.kind(), std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut)
        );
        ended.send((timed_out, started.elapsed())).unwrap();
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    let handle = client.shutdown_handle().unwrap();
    drop(client);
    drop(handle);
    let (timed_out, after) = connection_end.recv_timeout(Duration::from_secs(5)).unwrap();
    assert!(
        !timed_out,
        "the connection stayed open until the read timeout"
    );
    assert!(after < Duration::from_secs(1), "the end took {after:?}");
    server.join().unwrap();
}

// Expectation: once a shutdown handle has finished on an idle client, the client's next relay and
// its first human message both fail as definite no-writes, Error::Invalid, and nothing reaches the
// daemon. Why: a request the client never wrote is known not to be accepted, so the mailbox can
// fail it at once instead of holding it as uncertain. Value: shutdown keeps delivery accounting
// exact.
#[test]
fn after_a_finished_shutdown_a_relay_or_first_message_is_a_definite_no_write() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let (arrived, after_start) = mpsc::channel();
    let server = fake_daemon(&socket, move |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let mut requests = Vec::new();
        while let Ok(message) = ws.read() {
            requests.push(message.to_string());
        }
        arrived.send(requests).unwrap();
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    let handle = client.shutdown_handle().unwrap();
    handle.shutdown();
    let relay = client.relay(
        RelayKind::Message,
        "letter",
        Some(vec!["peer".into()]),
        WAIT,
    );
    assert!(matches!(relay, Err(Error::Invalid(_))), "{relay:?}");
    let first = client.first_human_message("hello", WAIT);
    assert!(matches!(first, Err(Error::Invalid(_))), "{first:?}");
    drop(client);
    drop(handle);
    let requests = after_start.recv_timeout(WAIT).unwrap();
    assert!(
        requests.is_empty(),
        "a request reached the daemon: {requests:?}"
    );
    server.join().unwrap();
}

// Only the exact bound thread may change effective policy. Missing/unknown settings
// revoke the previous mode, and queued native events keep their wire order before tools.
#[test]
fn effective_permission_events_follow_only_the_owned_thread_and_keep_order() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let (release, hold) = mpsc::channel();
    let server = fake_daemon(&socket, move |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "turn/start");
        send(
            &mut ws,
            json!({"method":"thread/settings/updated","params":{
                "threadId":"foreign-thread","threadSettings":{"approvalPolicy":"never"}
            }}),
        );
        for settings in [
            json!({"approvalPolicy":"on-request"}),
            json!({"approvalPolicy":"never"}),
            json!({}),
            json!({"approvalPolicy":"future-policy"}),
            json!({"approvalPolicy":null}),
            json!({"approvalPolicy":{"untrusted":true}}),
        ] {
            send(
                &mut ws,
                json!({"method":"thread/settings/updated","params":{
                    "threadId":"owned-thread","threadSettings":settings
                }}),
            );
        }
        send(
            &mut ws,
            json!({"id":91,"method":"item/tool/call","params":{
                "threadId":"owned-thread","turnId":"turn-1","namespace":"taskai",
                "tool":"list_sessions","arguments":{}
            }}),
        );
        send(
            &mut ws,
            json!({"id":request["id"],"result":{"turn":{"id":"turn-1"}}}),
        );
        hold.recv_timeout(WAIT).unwrap();
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    client.first_human_message("private fixture", WAIT).unwrap();
    assert_eq!(
        client.effective_permission().unwrap().mode,
        EffectivePermissionMode::Unknown
    );
    for expected in [
        EffectivePermissionMode::Prompting,
        EffectivePermissionMode::Bypass,
        EffectivePermissionMode::Unknown,
        EffectivePermissionMode::Unknown,
        EffectivePermissionMode::Unknown,
        EffectivePermissionMode::Unknown,
    ] {
        let Some(DaemonEvent::EffectivePermission(permission)) = client.next_event(WAIT).unwrap()
        else {
            panic!("expected exact-thread policy event before tool dispatch")
        };
        assert_eq!(permission.mode, expected);
    }
    assert!(matches!(
        client.next_event(WAIT).unwrap(),
        Some(DaemonEvent::ToolCall(_))
    ));
    release.send(()).unwrap();
    server.join().unwrap();
}

// A notification arriving before thread/start returns can be newer than that reply.
// Binding must replay exact-thread events after the reply's initial policy is installed.
#[test]
fn effective_permission_early_update_survives_thread_binding() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        let request = recv(&mut ws);
        assert_eq!(request["method"], "thread/start");
        send(
            &mut ws,
            json!({"method":"thread/settings/updated","params":{
                "threadId":"owned-thread","threadSettings":{"approvalPolicy":"never"}
            }}),
        );
        send(
            &mut ws,
            json!({"id":request["id"],"result":{
                "thread":{"id":"owned-thread"},"approvalPolicy":"on-request"
            }}),
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    assert_eq!(
        client.effective_permission().unwrap().mode,
        EffectivePermissionMode::Bypass
    );
    let Some(DaemonEvent::EffectivePermission(permission)) = client.next_event(WAIT).unwrap()
    else {
        panic!("early owned event must remain available to the runtime")
    };
    assert_eq!(permission.mode, EffectivePermissionMode::Bypass);
    server.join().unwrap();
}

// Expectation: a frame the daemon sent just before it closed the connection is still read: the
// client's next read returns it instead of failing to set its read timeout. Why: macOS refuses a
// read timeout (EINVAL) on a socket closed both ways, though its frames are still buffered and a
// read cannot block, so a daemon that answers and closes at once lost its answer under load.
// Value: a reply or status that arrives with the daemon's close is never misreported.
#[test]
fn a_frame_sent_just_before_the_daemon_closes_is_still_read() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("daemon.sock");
    let server = fake_daemon(&socket, |mut ws| {
        handshake(&mut ws);
        start(&mut ws);
        send(
            &mut ws,
            json!({"method":"thread/status/changed","params":{"threadId":"owned-thread","status":{"type":"active","activeFlags":["waitingOnApproval"]}}}),
        );
    });
    let mut client = DaemonClient::connect(&socket, WAIT).unwrap();
    client.initialize(WAIT).unwrap();
    client.start_thread(dir.path(), WAIT).unwrap();
    server.join().unwrap();
    let event = client.next_event(WAIT);
    assert!(
        matches!(event, Ok(Some(DaemonEvent::Activity(_)))),
        "{event:?}"
    );
}
