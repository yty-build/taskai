// ABOUTME: A native Claude peer can be addressed without inventing mode metadata absent from its card.
// ABOUTME: Private socket exchanges retain each sender's actual wire mode and mismatch accounting.

#![cfg(target_os = "macos")]

use std::{
    fs,
    io::{BufRead, BufReader},
    os::unix::net::UnixListener,
    sync::{mpsc, Arc},
    time::Duration,
};

use serde_json::{json, Value};
use taskai_connectors::claude::{
    drift::{drift_check, TESTED_VERSION},
    envelope::{uds_address, Envelope},
    mirror::sender_from_book,
    peer::{card_for, inbound, native_entry, ClaudeTransport},
};
use taskai_core::{
    mail::{
        proc_start, Directory, Identity, Mailbox, MailboxConfig, PermissionMode, SendStatus,
        SocketTransport, Transport,
    },
    registry::{AddressBook, Registry, Source},
    sha256_hex,
};

#[test]
fn native_mode_can_be_unknown_in_the_book_but_never_on_a_letter() {
    let root = tempfile::Builder::new()
        .prefix("cmode")
        .tempdir_in("/tmp")
        .unwrap();
    let state = root.path().join("state");
    let cards = root.path().join("cards");
    fs::create_dir(&cards).unwrap();
    let mut registry = Registry::open(&state).unwrap();
    let task = registry
        .create_task("Team", root.path(), Source::HumanUi)
        .unwrap();
    let native = registry
        .create_session(
            &task,
            None,
            "Native",
            "claude",
            root.path(),
            Source::HumanUi,
        )
        .unwrap();
    let bridge = registry
        .create_session(&task, None, "Bridge", "codex", root.path(), Source::HumanUi)
        .unwrap();
    let native_session = registry.session(&native).unwrap().clone();
    let bridge_session = registry.session(&bridge).unwrap().clone();
    let pid = std::process::id();
    let start = proc_start(pid).unwrap();
    let native_socket = root.path().join("native.sock");
    let native_listener = UnixListener::bind(&native_socket).unwrap();
    let card_path = cards.join(format!("{pid}.json"));
    fs::write(
        &card_path,
        json!({
            "pid": pid, "procStart": start, "sessionId": "native-owned", "startedAt": 1,
            "kind": "interactive", "peerProtocol": 1, "peerFeatures": ["notify_idle"],
            "messagingSocketPath": native_socket, "status": "idle"
        })
        .to_string(),
    )
    .unwrap();
    let digest = sha256_hex(native_socket.as_os_str().as_encoded_bytes());
    fs::write(
        cards.join(format!("{pid}.{digest}.key")),
        json!({
            "peerToken": "private-fixture-token", "procStart": start
        })
        .to_string(),
    )
    .unwrap();
    let card = card_for(&cards, pid, &start).unwrap().unwrap();
    let native_peer = native_entry(&native_session, &card, None).unwrap();
    assert!(native_peer.mode.is_none());
    let book = Arc::new(AddressBook::new(&state, &task));
    book.register(&native_peer).unwrap();
    let native_address = uds_address(&native_socket);
    assert_eq!(
        sender_from_book(book.as_ref(), &native_address).unwrap(),
        Some(native_session.address.clone())
    );
    let (sent, received) = mpsc::channel();
    let mailbox = Mailbox::open(
        MailboxConfig {
            identity: Identity {
                session_ref: bridge.clone(),
                task_ref: task.clone(),
                name: bridge_session.address.clone(),
                harness: "codex".into(),
                workspace: root.path().to_owned(),
                mode: PermissionMode::Prompting,
            },
            mail_dir: root.path().join("mail"),
            log_path: root.path().join("messages.jsonl"),
        },
        book.clone(),
        Arc::new(ClaudeTransport::new(
            cards,
            uds_address(&root.path().join("reply.sock")),
            Arc::new(SocketTransport),
            drift_check(Some(TESTED_VERSION), &[card]),
        )),
        move |delivery| {
            sent.send(delivery).unwrap();
        },
    )
    .unwrap();

    let result = mailbox.send(&native_session.address, "to native", false, None);
    assert_eq!(result.status, SendStatus::Sent);
    let (stream, _) = native_listener.accept().unwrap();
    // The synchronous transport has already written and closed. Nonblocking reads also
    // fail promptly if a regression leaves either frame missing after that completed send.
    stream.set_nonblocking(true).unwrap();
    let mut lines = BufReader::new(stream).lines();
    let auth: Value = serde_json::from_str(&lines.next().unwrap().unwrap()).unwrap();
    assert_eq!(auth["type"], "auth");
    let outgoing: Value = serde_json::from_str(&lines.next().unwrap().unwrap()).unwrap();
    assert_eq!(
        Envelope::parse(outgoing["message"]["content"].as_str().unwrap())
            .unwrap()
            .from_mode,
        Some(PermissionMode::Prompting)
    );

    let bridge_peer = book
        .live_sessions()
        .unwrap()
        .into_iter()
        .find(|peer| peer.session_ref == bridge)
        .unwrap();
    for (index, mode) in [PermissionMode::Bypass, PermissionMode::Prompting]
        .into_iter()
        .enumerate()
    {
        let envelope = Envelope {
            from: Some(native_address.clone()),
            from_session: Some("native-owned".into()),
            hop_chain: None,
            from_name: Some(native_session.address.clone()),
            from_mode: Some(mode),
            from_plugin: None,
            body: "from native".into(),
        };
        let mut wire = json!({"type":"user", "msg_id":format!("native-{index}"), "from":native_address,
            "message":{"content":envelope.render()}});
        for frame in inbound(&wire.to_string(), &native_session.address).unwrap() {
            SocketTransport.send(&bridge_peer, &frame).unwrap();
        }
        let delivery = received.recv_timeout(Duration::from_secs(2)).unwrap();
        mailbox.turn_started(&delivery.id);
        let mut missing = envelope;
        missing.from_mode = None;
        wire["message"]["content"] = json!(missing.render());
        assert!(inbound(&wire.to_string(), &native_session.address).is_err());
    }
    let log = fs::read_to_string(root.path().join("messages.jsonl")).unwrap();
    let entries: Vec<Value> = log
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect();
    let bypass = entries
        .iter()
        .find(|entry| entry["msg_id"] == "native-0")
        .unwrap();
    let prompting = entries
        .iter()
        .find(|entry| entry["msg_id"] == "native-1")
        .unwrap();
    assert!(bypass["note"].as_str().unwrap().contains("mode mismatch"));
    assert!(!prompting["note"]
        .as_str()
        .unwrap()
        .contains("mode mismatch"));
    registry.reload().unwrap();
    assert!(serde_json::to_value(
        registry
            .session(&native)
            .unwrap()
            .messaging
            .as_ref()
            .unwrap()
    )
    .unwrap()
    .get("mode")
    .is_none());
    mailbox.close();
}
