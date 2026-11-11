// ABOUTME: Claude adapter tests — the strict canonical envelope, peer frames, owned-pane cards, the
// ABOUTME: mirror and door, and the drift check; offline, in private temp dirs, never ~/.claude.

use std::{
    io::{Read, Write},
    os::unix::{
        fs::PermissionsExt,
        net::{UnixListener, UnixStream},
    },
    path::{Path, PathBuf},
    process::Command,
    sync::{
        atomic::{AtomicBool, AtomicUsize, Ordering},
        Arc, Mutex,
    },
    time::{Duration, Instant},
};

use serde_json::{json, Value};
use taskai_connectors::claude::{
    drift::{drift_check, Drift, TESTED_VERSION},
    envelope::{uds_address, Envelope},
    mirror::{sender_from_book, DoorHandler, Mirror, MirrorConfig, PID_DOMAIN},
    peer::{card_for, inbound, native_entry, outbound, ClaudeTransport},
};
use taskai_core::{
    mail::{
        proc_start, strip_messaging_env, Activity, Directory, Frame, IdleNotice, Letter, MailPeer,
        NoticeState, PermissionMode, Priority, Receipt, ReceiptStatus, Subscription, Transport,
    },
    registry::{AddressBook, Registry, Source},
    sha256_hex, Error,
};

fn chain(len: usize) -> Vec<String> {
    (0..len).map(|i| format!("{i:024x}")).collect()
}

fn letter(body: &str) -> Envelope {
    Envelope {
        from: Some("uds:/tmp/cc-socks/1.sock".into()),
        hop_chain: Some(vec!["a".repeat(24), "b".repeat(24)]),
        from_name: Some("codex-lead".into()),
        from_mode: Some(PermissionMode::Prompting),
        body: body.into(),
        ..Envelope::default()
    }
}

// Expectation: an envelope renders with its attributes in Claude's order (from, from-session,
// hop-chain, from-name, from-mode, from-plugin), each only when present, around the body. Why:
// Claude parses an envelope and drops it unless it re-renders byte for byte. Value: every letter
// TaskAI writes to a Claude session reads as one from its real sender.
#[test]
fn an_envelope_renders_in_claudes_canonical_order() {
    assert_eq!(
        letter("hi").render(),
        format!(
            "<cross-session-message from=\"uds:/tmp/cc-socks/1.sock\" hop-chain=\"{},{}\" \
             from-name=\"codex-lead\" from-mode=\"prompting\">\nhi\n</cross-session-message>",
            "a".repeat(24),
            "b".repeat(24)
        )
    );
    let full = Envelope {
        from_session: Some("local_0123".into()),
        from_plugin: Some("reviewer".into()),
        from_mode: Some(PermissionMode::Bypass),
        hop_chain: None,
        ..letter("x")
    };
    assert_eq!(
        full.render(),
        "<cross-session-message from=\"uds:/tmp/cc-socks/1.sock\" from-session=\"local_0123\" \
         from-name=\"codex-lead\" from-mode=\"bypass\" from-plugin=\"reviewer\">\nx\n\
         </cross-session-message>"
    );
    assert_eq!(
        Envelope::default().render(),
        "<cross-session-message>\n\n</cross-session-message>"
    );
}

// Expectation: parsing accepts exactly the text that equals its own rendering and gives back every
// attribute and the body; reordered or unknown attributes, invalid values, extra text, plain text
// and names Claude would rewrite are all refused. Why: Claude's grammar is strict and canonical,
// so anything else did not come from a Claude session's envelope. Value: the door admits only
// letters whose sender and mode were stated exactly once, in the one allowed form.
#[test]
fn parsing_accepts_only_canonical_envelopes() {
    let parsed = Envelope::parse(&letter("hi").render()).expect("a rendered envelope parses");
    assert_eq!(parsed, letter("hi"));
    let full = Envelope {
        from_session: Some("ses_9".into()),
        from_plugin: Some("tools".into()),
        ..letter("multi\nline\n")
    };
    assert_eq!(Envelope::parse(&full.render()), Some(full));
    let refused = [
        "plain text".to_string(),
        "<cross-session-message from-name=\"x\" from=\"uds:/a\">\nhi\n</cross-session-message>"
            .into(),
        "<cross-session-message from=\"uds:/a\" kind=\"x\">\nhi\n</cross-session-message>".into(),
        "<cross-session-message from=\"uds:/a b\">\nhi\n</cross-session-message>".into(),
        "<cross-session-message from-mode=\"auto\">\nhi\n</cross-session-message>".into(),
        format!(
            "<cross-session-message hop-chain=\"{}\">\nhi\n</cross-session-message>",
            "A".repeat(24)
        ),
        format!(
            "<cross-session-message hop-chain=\"{}\">\nhi\n</cross-session-message>",
            chain(33).join(",")
        ),
        "<cross-session-message from-session=\"has space\">\nhi\n</cross-session-message>".into(),
        "<cross-session-message from-name=\" padded\">\nhi\n</cross-session-message>".into(),
        "<cross-session-message from-name=\"zero\u{200B}width\">\nhi\n</cross-session-message>"
            .into(),
        format!("{}\n", letter("hi").render()),
        format!("x{}", letter("hi").render()),
        "<cross-session-message>hi</cross-session-message>".into(),
    ];
    for text in refused {
        assert_eq!(Envelope::parse(&text), None, "{text:?}");
    }
    assert_eq!(
        Envelope::parse(&format!(
            "<cross-session-message hop-chain=\"{}\">\nhi\n</cross-session-message>",
            chain(32).join(",")
        ))
        .and_then(|envelope| envelope.hop_chain)
        .map(|chain| chain.len()),
        Some(32)
    );
}

// Expectation: a body is escaped the way Claude escapes it: a backslash goes after the `<` of
// anything that reads as a closing envelope tag, in any case, with spaces or extra slashes, with
// invisible characters between its letters, with dash or underscore lookalikes, with a long s, or
// behind a lookalike bracket (which becomes `<`). An opening tag, a longer word, another tag, or
// an already escaped closing tag stays as it is. A body Claude would escape is never parsed
// unescaped. Why: Claude re-renders with this escape and shows anything that fails as plain user
// input, without a sender. Value: no letter can end its envelope early or arrive unattributed.
#[test]
fn bodies_are_escaped_exactly_as_claude_escapes_them() {
    let escaped = [
        ("a</cross-session-message>b", "a<\\/cross-session-message>b"),
        ("</CROSS-SESSION-MESSAGE>", "<\\/CROSS-SESSION-MESSAGE>"),
        (
            "< / cross-session-message >",
            "<\\ / cross-session-message >",
        ),
        ("<//cross-session-message", "<\\//cross-session-message"),
        (
            "</cross\u{200B}-session\u{00AD}-message>",
            "<\\/cross\u{200B}-session\u{00AD}-message>",
        ),
        (
            "</cross\u{2010}session_message>",
            "<\\/cross\u{2010}session_message>",
        ),
        ("</croſs-session-message>", "<\\/croſs-session-message>"),
        (
            "\u{FF1C}/cross-session-message>",
            "<\\/cross-session-message>",
        ),
        (
            "x\u{2039}\u{2215}cross-session-message",
            "x<\\\u{2215}cross-session-message",
        ),
        ("</cross-session-message", "<\\/cross-session-message"),
        // Brackets Claude reads as `<`, doubled and astral ones included.
        (
            "\u{226A}/cross-session-message>",
            "<\\/cross-session-message>",
        ),
        (
            "x\u{1D236}/cross-session-message",
            "x<\\/cross-session-message",
        ),
        (
            "\u{16B2} /cross-session-message>",
            "<\\ /cross-session-message>",
        ),
        (
            "\u{27EA}\u{2215}cross-session-message",
            "<\\\u{2215}cross-session-message",
        ),
        (
            "\u{FE3F}/cross-session-message>",
            "<\\/cross-session-message>",
        ),
        (
            "\u{2280}/cross-session-message",
            "<\\/cross-session-message",
        ),
        // Brackets Claude leaves out of its table are ordinary filler before the slash.
        (
            "<\u{226B}/cross-session-message>",
            "<\\\u{226B}/cross-session-message>",
        ),
        (
            "<\u{300A}/cross-session-message>",
            "<\\\u{300A}/cross-session-message>",
        ),
        (
            "<\u{2992}/cross-session-message",
            "<\\\u{2992}/cross-session-message",
        ),
    ];
    for (body, expected) in escaped {
        let rendered = letter(body).render();
        assert!(
            rendered.contains(&format!("\n{expected}\n")),
            "{body:?} renders as {rendered:?}"
        );
        assert_eq!(
            Envelope::parse(&rendered).map(|e| e.body),
            Some(expected.to_string())
        );
        let unescaped = rendered.replacen(expected, body, 1);
        assert_eq!(
            Envelope::parse(&unescaped),
            None,
            "unescaped {body:?} parses"
        );
    }
    let untouched = [
        "<cross-session-message from=\"uds:/x\">",
        "</cross-session-messages>",
        "</cross-session-message_x>",
        "</div> and <b>",
        "<\\/cross-session-message>",
        "</cross-\nsession-message>",
        "Vec<String> </ c>",
        // A bracket Claude reads as `>` is not filler, so no closing tag follows the `<`.
        "<\u{16F3F}/cross-session-message",
        "<\u{FE40}/cross-session-message>",
        "\u{226A}\u{2281}/cross-session-message",
        // These are not brackets to Claude at all.
        "\u{300A}/cross-session-message>",
        "\u{226B}/cross-session-message>",
    ];
    for body in untouched {
        assert!(
            letter(body).render().contains(&format!("\n{body}\n")),
            "{body:?} is left as it is"
        );
        assert_eq!(
            Envelope::parse(&letter(body).render()).map(|e| e.body),
            Some(body.to_string())
        );
    }
}

// Expectation: names and plugins render by Claude's name rule: quotes and angle brackets dropped,
// control and format characters removed, surrounding space trimmed, and more than 64 characters
// cut to 64 plus an ellipsis; a name that ends up empty is left out. Why: Claude renders names
// this way, so any other form fails its canonical check. Value: a plain address is shown exactly,
// and nothing else can pass for one.
#[test]
fn names_render_by_claudes_name_rule() {
    let named = |name: &str| Envelope {
        from_name: Some(name.into()),
        ..Envelope::default()
    };
    let head = |envelope: Envelope| {
        let rendered = envelope.render();
        rendered[..rendered.find('>').unwrap()].to_string()
    };
    assert_eq!(
        head(named("codex-lead")),
        "<cross-session-message from-name=\"codex-lead\""
    );
    assert_eq!(
        head(named(" \"Lead\" <x>\u{200B}\u{7}  ")),
        "<cross-session-message from-name=\"Lead x\""
    );
    let long = format!("{}{}", "é".repeat(64), "z");
    assert_eq!(
        head(named(&long)),
        format!(
            "<cross-session-message from-name=\"{}\u{2026}\"",
            "é".repeat(64)
        )
    );
    assert_eq!(head(named(" <> ")), "<cross-session-message");
    let truncated = Envelope {
        from_name: Some(format!("{}\u{2026}", "é".repeat(64))),
        ..Envelope::default()
    };
    assert_eq!(
        Envelope::parse(&truncated.render()),
        Some(truncated),
        "a cut name is its own rendering"
    );
}

// Expectation: a socket path becomes Claude's `uds:` address, percent-encoding every byte outside
// letters, digits and `:_/.-`, `%` included. Why: Claude compares reply addresses in this form.
// Value: a door at any path is answered.
#[test]
fn socket_addresses_are_claudes_uds_form() {
    assert_eq!(
        uds_address(Path::new("/tmp/cc-socks/42.sock")),
        "uds:/tmp/cc-socks/42.sock"
    );
    assert_eq!(
        uds_address(Path::new("/tmp/my dir/100%/é.sock")),
        "uds:/tmp/my%20dir/100%25/%C3%A9.sock"
    );
}

/// A private Claude sessions folder and a short private socket folder for one test.
struct ClaudeHome {
    sessions: tempfile::TempDir,
    sockets: tempfile::TempDir,
}

impl ClaudeHome {
    fn new() -> Self {
        Self {
            sessions: tempfile::tempdir().unwrap(),
            // Unix socket paths are short, so sockets get their own short directory.
            sockets: tempfile::Builder::new()
                .prefix("cc")
                .tempdir_in("/tmp")
                .unwrap(),
        }
    }

    fn socket(&self, name: &str) -> PathBuf {
        self.sockets.path().join(name)
    }

    /// Writes the card a Claude session in process `pid` publishes, and its key with `token`.
    fn card(&self, pid: u32, started: &str, socket: &Path, token: &str) {
        let card = json!({
            "pid": pid, "sessionId": "0c8b2a3e-session", "startedAt": 1_700_000_000_000u64,
            "kind": "interactive", "procStart": started, "peerProtocol": 1,
            "peerFeatures": ["notify_idle"], "messagingSocketPath": socket, "name": "claude-lead",
            "status": "idle"
        });
        std::fs::write(
            self.sessions.path().join(format!("{pid}.json")),
            card.to_string(),
        )
        .unwrap();
        let key = json!({"peerToken": token, "procStart": started, "pidDomain": "darwin"});
        let digest = sha256_hex(socket.to_str().unwrap().as_bytes());
        std::fs::write(
            self.sessions.path().join(format!("{pid}.{digest}.key")),
            key.to_string(),
        )
        .unwrap();
    }
}

/// The adapter as a passed drift check leaves it.
fn enabled() -> Drift {
    Drift {
        enabled: true,
        reasons: Vec::new(),
    }
}

fn this_process() -> (u32, String) {
    let pid = std::process::id();
    (pid, proc_start(pid).unwrap())
}

fn gone_pid() -> u32 {
    let mut done = Command::new("true");
    strip_messaging_env(&mut done);
    let mut child = done.spawn().unwrap();
    let pid = child.id();
    child.wait().unwrap();
    pid
}

fn claude_peer(pid: u32, started: &str, socket: &Path) -> MailPeer {
    MailPeer {
        session_ref: "s3".into(),
        task_ref: "t1".into(),
        name: "claude-lead".into(),
        harness: "claude".into(),
        workspace: "/repo".into(),
        pid,
        proc_start: started.into(),
        status: Activity::Idle,
        waiting_for: None,
        mailbox: socket.into(),
        features: vec!["notify_idle".into()],
        mode: Some(PermissionMode::Prompting),
    }
}

fn read_lines(listener: &UnixListener) -> Vec<Value> {
    let (mut stream, _) = listener.accept().unwrap();
    // Refused on macOS when the writer already finished, which is when no timeout is needed.
    let _ = stream.set_read_timeout(Some(Duration::from_secs(3)));
    let mut text = String::new();
    stream.read_to_string(&mut text).unwrap();
    text.lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect()
}

const DOOR: &str = "uds:/tmp/cc-socks/4242.sock";

fn letter_frame() -> Frame {
    Frame::Message(Letter {
        msg_id: "m1".into(),
        from: "codex-lead".into(),
        from_mode: PermissionMode::Prompting,
        hop_chain: vec!["a".repeat(24)],
        priority: Priority::Next,
        body: "review this".into(),
    })
}

// Expectation: a Claude session's card is read only from `<pid>.json` for the exact pane process
// TaskAI owns: its pid and UTC start time must match the card and a running process. Another
// process's card at that pid, a card left by an exited process, or no card gives nothing; a card
// that is not valid JSON of Claude's schema is an error. Why: pids are reused, and TaskAI may act
// only on sessions it launched. Value: TaskAI never writes into a Claude session it does not own.
#[test]
fn a_card_is_read_only_for_the_exact_live_process_that_owns_it() {
    let home = ClaudeHome::new();
    let (me, started) = this_process();
    let socket = home.socket("me.sock");
    home.card(me, &started, &socket, &"7".repeat(32));
    let card = card_for(home.sessions.path(), me, &started)
        .unwrap()
        .unwrap();
    assert_eq!((card.pid, card.peer_protocol), (me, Some(1)));
    assert_eq!(
        card.messaging_socket_path.as_deref(),
        Some(socket.as_path())
    );
    assert_eq!(card.peer_features, ["notify_idle"]);
    assert!(
        card_for(home.sessions.path(), me, "Thu Jan  1 00:00:00 1970")
            .unwrap()
            .is_none()
    );
    let gone = gone_pid();
    home.card(gone, "Thu Jan  1 00:00:00 1970", &socket, &"7".repeat(32));
    assert!(
        card_for(home.sessions.path(), gone, "Thu Jan  1 00:00:00 1970")
            .unwrap()
            .is_none()
    );
    assert!(card_for(home.sessions.path(), 999_999, &started)
        .unwrap()
        .is_none());
    std::fs::write(home.sessions.path().join(format!("{me}.json")), "{not json").unwrap();
    assert!(card_for(home.sessions.path(), me, &started).is_err());
}

// Expectation: Claude frames at the door become mailbox frames attributed to the sender the door
// resolved: a user frame with a canonical envelope becomes a letter (mode, chain, priority, body);
// a status receipt becomes one receipt per message id it answers, with Claude's expired-refused
// read back as refused; a subscription and an idle notice translate field for field. A letter
// without a canonical envelope, without a mode, or whose envelope names another reply address,
// and any other frame, is refused with a reason. Why: the core admits only §3 frames, and Claude's
// grammar is strict. Value: Claude sessions take part in TaskAI messaging without core knowing
// Claude's protocol.
#[test]
fn claude_frames_at_the_door_become_mailbox_frames() {
    let from = "uds:/tmp/cc-socks/77.sock";
    let envelope = Envelope {
        from: Some(from.into()),
        hop_chain: Some(vec!["c".repeat(24)]),
        from_name: Some("claude-lead".into()),
        from_mode: Some(PermissionMode::Bypass),
        body: "please review".into(),
        ..Envelope::default()
    };
    let user = |content: String, frame_from: &str| {
        json!({"msgV": 1, "type": "user", "msg_id": "u1", "priority": "now", "from": frame_from,
               "message": {"role": "user", "content": content}})
        .to_string()
    };
    assert_eq!(
        inbound(&user(envelope.render(), from), "claude-lead").unwrap(),
        vec![Frame::Message(Letter {
            msg_id: "u1".into(),
            from: "claude-lead".into(),
            from_mode: PermissionMode::Bypass,
            hop_chain: vec!["c".repeat(24)],
            priority: Priority::Now,
            body: "please review".into(),
        })]
    );
    let modeless = Envelope {
        from_mode: None,
        ..envelope.clone()
    };
    for refused in [
        user("plain words".into(), from),
        user(
            envelope.render().replace(
                "from-name=\"claude-lead\"",
                "from-name=\"claude\u{200B}-lead\"",
            ),
            from,
        ),
        user(modeless.render(), from),
        user(envelope.render(), "uds:/tmp/cc-socks/78.sock"),
        json!({"type": "auth", "token": "x"}).to_string(),
        json!({"type": "control", "action": "artifact_replies_yielded", "msgV": 1, "msg_id": "c9",
               "from": from})
        .to_string(),
        "{not json".into(),
    ] {
        assert!(inbound(&refused, "claude-lead").is_err(), "{refused}");
    }
    let status = |extra: Value| {
        let mut frame = json!({"type": "control", "action": "peer_message_status", "msgV": 1,
                               "msg_id": "c1", "from": from, "orig_msg_id": "m1"});
        frame
            .as_object_mut()
            .unwrap()
            .extend(extra.as_object().unwrap().clone());
        frame.to_string()
    };
    assert_eq!(
        inbound(
            &status(json!({"status": "held", "reason": "waiting for a person"})),
            "claude-lead"
        )
        .unwrap(),
        vec![Frame::Receipt(Receipt {
            orig_msg_id: "m1".into(),
            status: ReceiptStatus::Held,
            reason: Some("waiting for a person".into()),
            drop_reason: None,
            dropped_msg_ids: Vec::new(),
        })]
    );
    assert_eq!(
        inbound(
            &status(json!({"status": "expired", "status_detail": "refused"})),
            "claude-lead"
        )
        .unwrap()[0],
        Frame::Receipt(Receipt {
            orig_msg_id: "m1".into(),
            status: ReceiptStatus::Refused,
            reason: None,
            drop_reason: None,
            dropped_msg_ids: Vec::new(),
        })
    );
    let dropped = inbound(
        &status(
            json!({"status": "dropped", "reason": "queue full", "drop_reason": "queue-full",
                       "dropped_msg_ids": ["m1", "m2"]}),
        ),
        "claude-lead",
    )
    .unwrap();
    let answered: Vec<(&str, &[String])> = dropped
        .iter()
        .map(|frame| match frame {
            Frame::Receipt(r) => (r.orig_msg_id.as_str(), r.dropped_msg_ids.as_slice()),
            other => panic!("{other:?}"),
        })
        .collect();
    assert_eq!(
        answered,
        [
            ("m1", ["m1".to_string()].as_slice()),
            ("m2", ["m2".to_string()].as_slice())
        ]
    );
    assert_eq!(
        inbound(
            &json!({"type": "control", "action": "notify_when_idle", "msgV": 1, "msg_id": "sub1",
                    "from": from, "from_mode": "prompting"})
            .to_string(),
            "claude-lead"
        )
        .unwrap(),
        vec![Frame::Subscribe(Subscription {
            msg_id: "sub1".into(),
            from: "claude-lead".into(),
            from_mode: PermissionMode::Prompting,
        })]
    );
    assert_eq!(
        inbound(
            &json!({"type": "control", "action": "peer_idle_notice", "msgV": 1, "msg_id": "n1",
                    "from": from, "orig_msg_id": "sub0", "state": "idle",
                    "finished_at": 1_700_000_000_000u64, "detail": "done"})
            .to_string(),
            "claude-lead"
        )
        .unwrap(),
        vec![Frame::IdleNotice(IdleNotice {
            orig_msg_id: "sub0".into(),
            state: NoticeState::Idle,
            finished_at: Some(1_700_000_000_000),
            detail: Some("done".into()),
        })]
    );
}

// Expectation: mailbox frames leave as Claude's wire frames, all answering to the bridge's door:
// a letter as a user frame whose content is the canonical envelope; a receipt as
// peer_message_status (refused sent as Claude's expired with detail refused); a subscription as
// notify_when_idle under the subscription's own msg_id; an idle notice as peer_idle_notice; each
// control frame with a fresh UUID msg_id. Why: Claude routes answers to the `from` it receives and
// drops anything outside its grammar. Value: Claude sessions read TaskAI letters and notices
// natively and answer the right door.
#[test]
fn mailbox_frames_leave_as_claudes_wire_frames() {
    let user = outbound(&letter_frame(), DOOR);
    assert_eq!(
        user,
        json!({"msgV": 1, "type": "user", "msg_id": "m1", "priority": "next", "from": DOOR,
               "message": {"role": "user", "content": Envelope {
                   from: Some(DOOR.into()),
                   hop_chain: Some(vec!["a".repeat(24)]),
                   from_name: Some("codex-lead".into()),
                   from_mode: Some(PermissionMode::Prompting),
                   body: "review this".into(),
                   ..Envelope::default()
               }.render()}})
    );
    let receipt = |status, drop_reason: Option<&str>| {
        outbound(
            &Frame::Receipt(Receipt {
                orig_msg_id: "u1".into(),
                status,
                reason: Some("why".into()),
                drop_reason: drop_reason.map(str::to_string),
                dropped_msg_ids: drop_reason
                    .map(|_| vec!["u1".to_string()])
                    .unwrap_or_default(),
            }),
            DOOR,
        )
    };
    let refused = receipt(ReceiptStatus::Refused, None);
    assert_eq!(
        (
            &refused["type"],
            &refused["action"],
            &refused["status"],
            &refused["status_detail"]
        ),
        (
            &json!("control"),
            &json!("peer_message_status"),
            &json!("expired"),
            &json!("refused")
        )
    );
    assert_eq!(
        (
            &refused["orig_msg_id"],
            &refused["reason"],
            &refused["from"],
            &refused["msgV"]
        ),
        (&json!("u1"), &json!("why"), &json!(DOOR), &json!(1))
    );
    let dropped = receipt(ReceiptStatus::Dropped, Some("hop-loop"));
    assert_eq!(
        (
            &dropped["status"],
            &dropped["drop_reason"],
            &dropped["dropped_msg_ids"]
        ),
        (&json!("dropped"), &json!("hop-loop"), &json!(["u1"]))
    );
    let id = |frame: &Value| frame["msg_id"].as_str().unwrap().to_string();
    assert!(is_uuid_v4(&id(&refused)) && id(&refused) != id(&dropped));
    let subscribe = outbound(
        &Frame::Subscribe(Subscription {
            msg_id: "sub7".into(),
            from: "codex-lead".into(),
            from_mode: PermissionMode::Bypass,
        }),
        DOOR,
    );
    assert_eq!(
        subscribe,
        json!({"type": "control", "action": "notify_when_idle", "msgV": 1, "msg_id": "sub7",
               "from": DOOR, "from_mode": "bypass"})
    );
    let notice = outbound(
        &Frame::IdleNotice(IdleNotice {
            orig_msg_id: "sub9".into(),
            state: NoticeState::Exited,
            finished_at: Some(5),
            detail: None,
        }),
        DOOR,
    );
    assert_eq!(
        (
            &notice["action"],
            &notice["orig_msg_id"],
            &notice["state"],
            &notice["from"]
        ),
        (
            &json!("peer_idle_notice"),
            &json!("sub9"),
            &json!("exited"),
            &json!(DOOR)
        )
    );
    assert_eq!(notice["finished_at"], json!(5));
    assert!(notice.get("detail").is_none() && is_uuid_v4(&id(&notice)));
}

fn is_uuid_v4(id: &str) -> bool {
    let groups: Vec<&str> = id.split('-').collect();
    groups.iter().map(|group| group.len()).eq([8, 4, 4, 4, 12])
        && groups.iter().all(|group| {
            group
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        })
        && groups[2].starts_with('4')
        && groups[3].starts_with(['8', '9', 'a', 'b'])
}

/// Records what the transport hands on for peers that are not Claude sessions.
#[derive(Default)]
struct Others {
    sent: Mutex<Vec<String>>,
}

impl Transport for Others {
    fn send(&self, to: &MailPeer, _frame: &Frame) -> Result<(), Error> {
        self.sent.lock().unwrap().push(to.name.clone());
        Ok(())
    }
}

// Expectation: the Claude transport writes to a Claude peer only through the card of that exact
// live process: it authenticates with the peerToken from the card's key file, then writes the
// Claude frame. It refuses a peer with no live card for its pid and start time, a card whose
// socket is not the peer's mailbox, a key from another process start, and a frame over 1 MiB, and
// hands every other harness to the TaskAI transport. Why: a stale address-book entry or a reused
// pid must never reach a stranger, and the key is the only credential Claude accepts. Value:
// letters reach exactly the Claude session TaskAI launched.
#[test]
fn the_claude_transport_writes_only_through_the_owned_card() {
    let home = ClaudeHome::new();
    let (me, started) = this_process();
    let socket = home.socket("claude.sock");
    let listener = UnixListener::bind(&socket).unwrap();
    let token = "5".repeat(32);
    home.card(me, &started, &socket, &token);
    let others = Arc::new(Others::default());
    let transport = ClaudeTransport::new(
        home.sessions.path().into(),
        DOOR.into(),
        others.clone(),
        enabled(),
    );
    let peer = claude_peer(me, &started, &socket);
    transport.send(&peer, &letter_frame()).unwrap();
    let lines = read_lines(&listener);
    assert_eq!(lines.len(), 2);
    assert_eq!(lines[0], json!({"type": "auth", "token": token}));
    assert_eq!(lines[1], outbound(&letter_frame(), DOOR));
    let codex = MailPeer {
        harness: "codex".into(),
        name: "codex-helper".into(),
        ..peer.clone()
    };
    transport.send(&codex, &letter_frame()).unwrap();
    assert_eq!(*others.sent.lock().unwrap(), ["codex-helper"]);
    assert!(transport
        .send(
            &claude_peer(me, "Thu Jan  1 00:00:00 1970", &socket),
            &letter_frame()
        )
        .is_err());
    assert!(transport
        .send(
            &claude_peer(me, &started, &home.socket("other.sock")),
            &letter_frame()
        )
        .is_err());
    let oversized = Frame::Message(Letter {
        body: "x".repeat(1 << 20),
        ..match letter_frame() {
            Frame::Message(letter) => letter,
            _ => unreachable!(),
        }
    });
    assert!(transport.send(&peer, &oversized).is_err());
    let digest = sha256_hex(socket.to_str().unwrap().as_bytes());
    std::fs::write(
        home.sessions.path().join(format!("{me}.{digest}.key")),
        json!({"peerToken": token, "procStart": "Thu Jan  1 00:00:00 1970", "pidDomain": "darwin"})
            .to_string(),
    )
    .unwrap();
    assert!(
        transport.send(&peer, &letter_frame()).is_err(),
        "a key from another process start"
    );
}

/// Records what a Claude door hands on, resolving only the reply addresses it is given.
#[derive(Default)]
struct Recorder {
    senders: Vec<(String, String)>,
    accepted: Mutex<Vec<Frame>>,
    refused: Mutex<Vec<String>>,
    /// How long each sender lookup takes, to hold a frame mid-handling.
    lookup_delay: Duration,
    /// Sender lookups started.
    lookups: AtomicUsize,
}

impl DoorHandler for Recorder {
    fn sender(&self, from: &str) -> Option<String> {
        self.lookups.fetch_add(1, Ordering::SeqCst);
        std::thread::sleep(self.lookup_delay);
        self.senders
            .iter()
            .find(|(reply, _)| reply == from)
            .map(|(_, name)| name.clone())
    }

    fn accept(&self, frame: Frame) {
        self.accepted.lock().unwrap().push(frame);
    }

    fn refuse(&self, reason: &str) {
        self.refused.lock().unwrap().push(reason.into());
    }
}

/// Waits up to three seconds for `done`, so a test controls what follows the door's threads.
fn eventually(done: impl Fn() -> bool) -> bool {
    let deadline = Instant::now() + Duration::from_secs(3);
    while !done() {
        if Instant::now() > deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    true
}

fn mode_of(path: &Path) -> u32 {
    std::fs::metadata(path).unwrap().permissions().mode() & 0o777
}

fn mirror_config(home: &ClaudeHome) -> MirrorConfig {
    MirrorConfig {
        sessions_dir: home.sessions.path().into(),
        socket_dir: home.sockets.path().into(),
        name: "codex-lead".into(),
        harness: "codex".into(),
        session_id: "thread-1".into(),
        cwd: "/repo".into(),
    }
}

fn read_json(path: &Path) -> Value {
    serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap()
}

/// The door socket and key path a mirror in this process uses.
fn door_paths(home: &ClaudeHome) -> (PathBuf, PathBuf, PathBuf) {
    let pid = std::process::id();
    let door = home.sockets.path().join(format!("{pid}.sock"));
    let digest = sha256_hex(door.to_str().unwrap().as_bytes());
    let key = home.sessions.path().join(format!("{pid}.{digest}.key"));
    let card = home.sessions.path().join(format!("{pid}.json"));
    (door, key, card)
}

fn letter_line(from: &str, body: &str) -> String {
    let content = Envelope {
        from: Some(from.into()),
        from_name: Some("claude-lead".into()),
        from_mode: Some(PermissionMode::Prompting),
        body: body.into(),
        ..Envelope::default()
    }
    .render();
    json!({"msgV": 1, "type": "user", "msg_id": "u1", "priority": "next", "from": from,
           "message": {"role": "user", "content": content}})
    .to_string()
}

// Expectation: preparing a mirror binds the bridge's Claude door, owner-only, at
// `<socket dir>/<pid>.sock` and exposes its address, but publishes no card and no key, so no
// Claude session can find or authenticate to it yet; dropping it unpublished removes the socket.
// Why: the door's handler needs the mailbox, which needs the door's address first. Value: no
// Claude session can reach the bridge before something is ready to receive.
#[test]
fn preparing_binds_the_door_but_publishes_nothing() {
    let home = ClaudeHome::new();
    let (door, key, card) = door_paths(&home);
    let prepared = Mirror::prepare(mirror_config(&home), &enabled()).unwrap();
    assert_eq!(prepared.address(), uds_address(&door));
    assert_eq!(mode_of(&door), 0o600);
    assert!(!key.exists() && !card.exists());
    assert_eq!(std::fs::read_dir(home.sessions.path()).unwrap().count(), 0);
    drop(prepared);
    assert!(!door.exists());
}

// Expectation: publishing writes the owner-only key, `<pid>.<sha256 of the door path>.key` with a
// 32-hex peerToken, this process's start and its pid domain, and then the owner-only card naming
// the bridge's address, harness, provider session, workspace, Claude version, protocol 1,
// notify_idle and the door, which Claude's own lookup reads back. Why: Claude finds peers by card
// and authenticates to them with the key. Value: Claude sessions list and message the bridge
// natively.
#[test]
fn publishing_writes_an_owner_only_key_and_card_for_the_door() {
    let home = ClaudeHome::new();
    let (door, key, card) = door_paths(&home);
    let (me, started) = this_process();
    let mirror = Mirror::prepare(mirror_config(&home), &enabled())
        .unwrap()
        .publish(Arc::new(Recorder::default()))
        .unwrap();
    assert_eq!(mirror.address(), uds_address(&door));
    assert_eq!((mode_of(&key), mode_of(&card)), (0o600, 0o600));
    let written = read_json(&key);
    let token = written["peerToken"].as_str().unwrap();
    assert!(token.len() == 32 && token.bytes().all(|b| b.is_ascii_hexdigit()));
    assert_eq!(written["procStart"], json!(started));
    assert_eq!(written["pidDomain"], json!(PID_DOMAIN));
    let published = read_json(&card);
    for (field, value) in [
        ("pid", json!(me)),
        ("sessionId", json!("thread-1")),
        ("cwd", json!("/repo")),
        ("procStart", json!(started)),
        ("version", json!(TESTED_VERSION)),
        ("peerProtocol", json!(1)),
        ("peerFeatures", json!(["notify_idle"])),
        ("kind", json!("interactive")),
        ("entrypoint", json!("taskai-bridge:codex")),
        ("pidDomain", json!(PID_DOMAIN)),
        ("messagingSocketPath", json!(door)),
        ("name", json!("codex-lead")),
        ("status", json!("idle")),
    ] {
        assert_eq!(published[field], value, "{field}");
    }
    let read_back = card_for(home.sessions.path(), me, &started)
        .unwrap()
        .unwrap();
    assert_eq!(read_back.socket(), Some(door.as_path()));
}

// Expectation: the door admits nothing until published, and loses nothing at startup: a Claude
// session that connected while the mirror was only prepared waits, and once published, its
// authenticated letter reaches the handler. A wrong token, a frame from a reply address the
// handler does not know, and a letter whose envelope is not canonical are refused with a reason
// and never accepted. Why: the card must not lead anyone to a door whose mailbox is not yet
// listening, and only TaskAI's own Claude sessions may be heard. Value: no startup letter is lost,
// and no stranger's frame reaches the agent.
#[test]
fn the_door_serves_only_once_published_and_only_authenticated_known_senders() {
    let home = ClaudeHome::new();
    let (door, key, _card) = door_paths(&home);
    let from = "uds:/tmp/cc-socks/77.sock";
    let recorder = Arc::new(Recorder {
        senders: vec![(from.into(), "claude-lead".into())],
        ..Recorder::default()
    });
    let prepared = Mirror::prepare(mirror_config(&home), &enabled()).unwrap();
    let mut early = UnixStream::connect(&door).unwrap();
    let mirror = prepared.publish(recorder.clone()).unwrap();
    let token = read_json(&key)["peerToken"].as_str().unwrap().to_string();
    let auth = json!({"type": "auth", "token": token}).to_string();
    early
        .write_all(format!("{auth}\n{}\n", letter_line(from, "early")).as_bytes())
        .unwrap();
    drop(early);
    assert!(
        eventually(|| recorder.accepted.lock().unwrap().len() == 1),
        "the startup letter arrives"
    );
    let send = |lines: &[String]| {
        let mut stream = UnixStream::connect(&door).unwrap();
        stream
            .write_all(format!("{}\n", lines.join("\n")).as_bytes())
            .unwrap();
    };
    send(&[
        json!({"type": "auth", "token": "0".repeat(32)}).to_string(),
        letter_line(from, "forged"),
    ]);
    assert!(eventually(|| recorder.refused.lock().unwrap().len() == 1));
    let unescaped = format!(
        "<cross-session-message from=\"{from}\" from-mode=\"prompting\">\n\
         a</cross-session-message>b\n</cross-session-message>"
    );
    send(&[
        auth.clone(),
        letter_line("uds:/tmp/cc-socks/99.sock", "stranger"),
        json!({"msgV": 1, "type": "user", "msg_id": "u2", "priority": "next", "from": from,
               "message": {"role": "user", "content": unescaped}})
        .to_string(),
        letter_line(from, "late"),
    ]);
    assert!(eventually(|| recorder.accepted.lock().unwrap().len() == 2));
    assert!(eventually(|| recorder.refused.lock().unwrap().len() == 3));
    let bodies: Vec<String> = recorder
        .accepted
        .lock()
        .unwrap()
        .iter()
        .map(|frame| match frame {
            Frame::Message(letter) => format!("{}: {}", letter.from, letter.body),
            other => panic!("{other:?}"),
        })
        .collect();
    assert_eq!(bodies, ["claude-lead: early", "claude-lead: late"]);
    let refused = recorder.refused.lock().unwrap().clone();
    assert!(refused[0].contains("auth"), "{refused:?}");
    assert!(
        refused[1].contains("uds:/tmp/cc-socks/99.sock"),
        "{refused:?}"
    );
    assert!(refused[2].contains("canonical"), "{refused:?}");
    drop(mirror);
}

// Expectation: the mirror's status follows the bridge: waiting with what it waits for, then busy
// without it. Closing removes the card, the key and the door socket. If something else has
// replaced the card, a status change is refused and leaves it, and closing removes only the key
// and socket that are still the mirror's own. Why: another process may own those paths by then.
// Value: Claude sees what the bridge is doing, and a closing bridge never deletes another's files.
#[test]
fn the_mirror_updates_and_removes_only_its_own_files() {
    let home = ClaudeHome::new();
    let (door, key, card) = door_paths(&home);
    let mirror = Mirror::prepare(mirror_config(&home), &enabled())
        .unwrap()
        .publish(Arc::new(Recorder::default()))
        .unwrap();
    mirror
        .set_status(Activity::Waiting, Some("approval"))
        .unwrap();
    let shown = |card: &Path| {
        let card = read_json(card);
        (card["status"].clone(), card.get("waitingFor").cloned())
    };
    assert_eq!(shown(&card), (json!("waiting"), Some(json!("approval"))));
    mirror.set_status(Activity::Busy, Some("approval")).unwrap();
    assert_eq!(shown(&card), (json!("busy"), None));
    mirror.close();
    assert!(!card.exists() && !key.exists() && !door.exists());

    let mirror = Mirror::prepare(mirror_config(&home), &enabled())
        .unwrap()
        .publish(Arc::new(Recorder::default()))
        .unwrap();
    std::fs::remove_file(&card).unwrap();
    std::fs::write(&card, r#"{"replaced": true}"#).unwrap();
    assert!(mirror.set_status(Activity::Idle, None).is_err());
    mirror.close();
    assert_eq!(read_json(&card), json!({"replaced": true}));
    assert!(!key.exists() && !door.exists());
}

/// An address book listing fixed peers.
struct Peers(Vec<MailPeer>);

impl Directory for Peers {
    fn live_sessions(&self) -> Result<Vec<MailPeer>, Error> {
        Ok(self.0.clone())
    }

    fn register(&self, _entry: &MailPeer) -> Result<(), Error> {
        Ok(())
    }

    fn set_status(&self, _: &str, _: Activity, _: Option<&str>) -> Result<(), Error> {
        Ok(())
    }

    fn unregister(&self, _: &str) -> Result<(), Error> {
        Ok(())
    }
}

// Expectation: a Claude reply address resolves to the address of the live Claude session of this
// task whose mailbox is that socket, decoding Claude's percent-encoding; a socket held by another
// harness's session, an unknown socket and anything not a `uds:` address resolve to nothing. Why:
// the address book is the allowlist, and a reply address is only a claim to be checked against it.
// Value: the door hears exactly TaskAI's own Claude sessions.
#[test]
fn reply_addresses_resolve_only_to_live_claude_sessions_of_the_task() {
    let (me, started) = this_process();
    let claude = claude_peer(me, &started, Path::new("/tmp/cc-socks/my dir/7.sock"));
    let codex = MailPeer {
        harness: "codex".into(),
        name: "codex-helper".into(),
        mailbox: "/tmp/cc-socks/8.sock".into(),
        ..claude.clone()
    };
    let book = Peers(vec![claude, codex]);
    assert_eq!(
        sender_from_book(
            &book,
            &uds_address(Path::new("/tmp/cc-socks/my dir/7.sock"))
        )
        .unwrap(),
        Some("claude-lead".to_string())
    );
    for from in [
        "uds:/tmp/cc-socks/8.sock",
        "uds:/tmp/cc-socks/9.sock",
        "/tmp/cc-socks/my%20dir/7.sock",
        "bridge:/tmp/cc-socks/my%20dir/7.sock",
    ] {
        assert_eq!(sender_from_book(&book, from).unwrap(), None, "{from}");
    }
}

fn card(pid: u32, protocol: Option<u32>, socket: Option<&str>, started: Option<&str>) -> Value {
    let mut card = json!({"pid": pid, "sessionId": "s", "startedAt": 1u64, "kind": "interactive"});
    if let Some(protocol) = protocol {
        card["peerProtocol"] = json!(protocol);
    }
    if let Some(socket) = socket {
        card["messagingSocketPath"] = json!(socket);
    }
    if let Some(started) = started {
        card["procStart"] = json!(started);
    }
    card
}

// Expectation: the drift check passes only for the tested Claude Code version, owned cards that
// offer peer protocol 1 at an absolute socket with their process start, and a sample envelope
// that round-trips; on a platform whose pid domain is unverified it fails too. Every reason is
// listed, naming the version or the card's pid. Why: the protocol is undocumented and versioned,
// so anything untested must fail closed. Value: a Claude update disables only the Claude adapter,
// with the reason shown, instead of misrouting letters.
#[test]
fn the_drift_check_passes_only_what_was_tested() {
    let parse = |value: Value| serde_json::from_value(value).unwrap();
    let good = parse(card(
        10,
        Some(1),
        Some("/tmp/cc-socks/10.sock"),
        Some("then"),
    ));
    let passed = drift_check(Some(TESTED_VERSION), std::slice::from_ref(&good));
    let platform_verified = PID_DOMAIN == "darwin";
    assert_eq!(passed.enabled, platform_verified, "{:?}", passed.reasons);
    let failed = drift_check(
        Some("2.1.290"),
        &[
            good,
            parse(card(
                11,
                Some(2),
                Some("/tmp/cc-socks/11.sock"),
                Some("then"),
            )),
            parse(card(12, Some(1), None, Some("then"))),
            parse(card(13, Some(1), Some("relative.sock"), Some("then"))),
            parse(card(14, Some(1), Some("/tmp/cc-socks/14.sock"), None)),
        ],
    );
    assert!(!failed.enabled);
    let reasons = failed.reasons.join(" | ");
    assert!(
        reasons.contains("2.1.290") && reasons.contains(TESTED_VERSION),
        "{reasons}"
    );
    for pid in ["11", "12", "13", "14"] {
        assert!(reasons.contains(&format!("pid {pid}")), "{reasons}");
    }
    assert!(!reasons.contains("pid 10"), "{reasons}");
    let unknown = drift_check(None, &[]);
    assert!(!unknown.enabled && unknown.reasons.join(" ").contains("version"));
    assert!(enabled().require().is_ok());
    let error = unknown.require().unwrap_err().to_string();
    assert!(
        error.contains("Claude adapter is disabled") && error.contains("version"),
        "{error}"
    );
}

// Expectation: a failed drift check disables the Claude adapter alone: no mirror can be prepared,
// and the transport refuses every Claude peer with the drift reasons while it still carries
// letters to every other harness. Why: the specification gates each adapter on its own. Value: a
// Claude update can stop Claude messaging, never Codex or Grok messaging.
#[test]
fn a_failed_drift_disables_only_the_claude_adapter() {
    let home = ClaudeHome::new();
    let disabled = drift_check(Some("0.0.1"), &[]);
    assert!(!disabled.enabled);
    assert!(Mirror::prepare(mirror_config(&home), &disabled).is_err());
    let (door, ..) = door_paths(&home);
    assert!(!door.exists(), "no door is bound");
    let (me, started) = this_process();
    let others = Arc::new(Others::default());
    let transport = ClaudeTransport::new(
        home.sessions.path().into(),
        DOOR.into(),
        others.clone(),
        disabled,
    );
    let error = transport
        .send(
            &claude_peer(me, &started, &home.socket("claude.sock")),
            &letter_frame(),
        )
        .unwrap_err()
        .to_string();
    assert!(error.contains("0.0.1"), "{error}");
    let codex = MailPeer {
        harness: "codex".into(),
        name: "codex-helper".into(),
        ..claude_peer(me, &started, &home.socket("codex.sock"))
    };
    transport.send(&codex, &letter_frame()).unwrap();
    assert_eq!(*others.sent.lock().unwrap(), ["codex-helper"]);
}

// Expectation: a native Claude session's address-book entry is built from its validated card:
// the card's own pid and start time, its protocol-1 socket as the mailbox, its features, and its
// status with what it waits for (an unknown status shows as busy), under the TaskAI session's
// address. Registering it again with the card's newer status refreshes the same entry, because
// the address book lets a process re-register an entry held by the same pid; once that process
// is gone the entry is no longer routable. A card without a protocol-1 socket or a start time
// gives no entry. Why: the address book's set_status and unregister act only for the calling
// process, and a native Claude session is not TaskAI's process. Value: Claude sessions TaskAI
// launched are addressable, with live status, and only while they run.
#[test]
fn a_native_claude_session_is_entered_and_refreshed_from_its_card() {
    let home = ClaudeHome::new();
    let state = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(state.path()).unwrap();
    let task = registry
        .create_task("Team", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let lead = registry
        .create_session(
            &task,
            None,
            "Lead",
            "claude",
            Path::new("/repo"),
            Source::HumanUi,
        )
        .unwrap();
    let session = registry.session(&lead).unwrap().clone();
    let (me, started) = this_process();
    let socket = home.socket("claude.sock");
    home.card(me, &started, &socket, &"5".repeat(32));
    let card = card_for(home.sessions.path(), me, &started)
        .unwrap()
        .unwrap();
    let entry = native_entry(&session, &card, PermissionMode::Prompting).unwrap();
    assert_eq!(
        (
            entry.name.as_str(),
            entry.harness.as_str(),
            entry.pid,
            entry.proc_start.as_str()
        ),
        ("claude-lead", "claude", me, started.as_str())
    );
    assert_eq!(
        (
            entry.mailbox.as_path(),
            entry.status,
            entry.features.as_slice()
        ),
        (
            socket.as_path(),
            Activity::Idle,
            ["notify_idle".to_string()].as_slice()
        )
    );
    let book = AddressBook::new(state.path(), &task);
    book.register(&entry).unwrap();
    assert_eq!(book.live_sessions().unwrap()[0].status, Activity::Idle);
    let mut waiting: Value = read_json(&home.sessions.path().join(format!("{me}.json")));
    waiting["status"] = json!("waiting");
    waiting["waitingFor"] = json!("approval");
    std::fs::write(
        home.sessions.path().join(format!("{me}.json")),
        waiting.to_string(),
    )
    .unwrap();
    let card = card_for(home.sessions.path(), me, &started)
        .unwrap()
        .unwrap();
    book.register(&native_entry(&session, &card, PermissionMode::Prompting).unwrap())
        .unwrap();
    let listed = book.live_sessions().unwrap();
    assert_eq!(
        (listed[0].status, listed[0].waiting_for.as_deref()),
        (Activity::Waiting, Some("approval"))
    );
    let mut unknown = waiting.clone();
    unknown["status"] = json!("parked");
    std::fs::write(
        home.sessions.path().join(format!("{me}.json")),
        unknown.to_string(),
    )
    .unwrap();
    let card = card_for(home.sessions.path(), me, &started)
        .unwrap()
        .unwrap();
    assert_eq!(
        native_entry(&session, &card, PermissionMode::Prompting)
            .unwrap()
            .status,
        Activity::Busy
    );
    let mut silent = unknown.clone();
    silent["peerProtocol"] = json!(2);
    std::fs::write(
        home.sessions.path().join(format!("{me}.json")),
        silent.to_string(),
    )
    .unwrap();
    let card = card_for(home.sessions.path(), me, &started)
        .unwrap()
        .unwrap();
    assert!(native_entry(&session, &card, PermissionMode::Prompting).is_none());
    let helper = registry
        .create_session(
            &task,
            None,
            "Helper",
            "claude",
            Path::new("/repo"),
            Source::HumanUi,
        )
        .unwrap();
    let helper = registry.session(&helper).unwrap().clone();
    let mut sleeper = Command::new("sleep");
    sleeper.arg("30");
    strip_messaging_env(&mut sleeper);
    let mut sleeper = sleeper.spawn().unwrap();
    let sleeper_started = proc_start(sleeper.id()).unwrap();
    home.card(
        sleeper.id(),
        &sleeper_started,
        &home.socket("helper.sock"),
        &"6".repeat(32),
    );
    let card = card_for(home.sessions.path(), sleeper.id(), &sleeper_started)
        .unwrap()
        .unwrap();
    book.register(&native_entry(&helper, &card, PermissionMode::Prompting).unwrap())
        .unwrap();
    let listed = |book: &AddressBook| -> Vec<String> {
        book.live_sessions()
            .unwrap()
            .into_iter()
            .map(|peer| peer.name)
            .collect()
    };
    assert!(listed(&book).contains(&helper.address));
    sleeper.kill().unwrap();
    sleeper.wait().unwrap();
    assert!(
        !listed(&book).contains(&helper.address),
        "a gone process is not routable"
    );
}

/// Authenticates to a published mirror's door with the token its key holds.
fn authenticated(door: &Path, key: &Path) -> UnixStream {
    let token = read_json(key)["peerToken"].as_str().unwrap().to_string();
    let mut stream = UnixStream::connect(door).unwrap();
    stream
        .write_all(format!("{}\n", json!({"type": "auth", "token": token})).as_bytes())
        .unwrap();
    stream
}

/// True once the door has ended the connection, so a read finds its end rather than waiting.
/// Reads without blocking, as macOS refuses a read timeout on a socket already disconnected.
fn ended(stream: &UnixStream) -> bool {
    stream.set_nonblocking(true).unwrap();
    eventually(|| {
        let mut reader = stream;
        match reader.read(&mut [0; 4096]) {
            Ok(read) => read == 0,
            Err(error) => error.kind() == std::io::ErrorKind::ConnectionReset,
        }
    })
}

// Expectation: closing a mirror ends admission on connections the door already accepted and
// authenticated: a valid letter written on such a connection after close reaches no handler, and
// the door ends every accepted connection, one that sends nothing more included, instead of
// leaving it open. Why: stopping the accept loop does not
// stop a reader that is already serving, and a closed mirror must not keep delivering Claude
// letters while the mailbox and other adapters stay open. Value: once closed, the Claude adapter
// is silent for that bridge.
#[test]
fn closing_the_mirror_ends_admission_on_accepted_connections() {
    let home = ClaudeHome::new();
    let (door, key, _card) = door_paths(&home);
    let from = "uds:/tmp/cc-socks/77.sock";
    let recorder = Arc::new(Recorder {
        senders: vec![(from.into(), "claude-lead".into())],
        ..Recorder::default()
    });
    let mirror = Mirror::prepare(mirror_config(&home), &enabled())
        .unwrap()
        .publish(recorder.clone())
        .unwrap();
    let mut stream = authenticated(&door, &key);
    let mut idle = authenticated(&door, &key);
    stream
        .write_all(format!("{}\n", letter_line(from, "before close")).as_bytes())
        .unwrap();
    idle.write_all(format!("{}\n", letter_line(from, "then idle")).as_bytes())
        .unwrap();
    assert!(eventually(|| recorder.accepted.lock().unwrap().len() == 2));
    mirror.close();
    let _ = stream.write_all(format!("{}\n", letter_line(from, "after close")).as_bytes());
    assert!(ended(&stream), "the door ended the accepted connection");
    assert!(
        ended(&idle),
        "the door ended an accepted connection that sent nothing more"
    );
    std::thread::sleep(Duration::from_millis(300));
    assert_eq!(recorder.accepted.lock().unwrap().len(), 2);
    assert!(recorder.refused.lock().unwrap().is_empty());
}

// Expectation: a frame the door is already handing on when close is called lands before close
// returns, and nothing lands after. Why: a door that only checks it is open before handing a frame
// on can pass the check, lose the race to close and deliver after close returned; close must wait
// for that frame instead. Value: close is a clean cut even mid-frame, and a letter the door had
// begun handing on is not lost.
#[test]
fn close_waits_for_a_frame_already_being_handed_on() {
    let home = ClaudeHome::new();
    let (door, key, _card) = door_paths(&home);
    let from = "uds:/tmp/cc-socks/77.sock";
    let recorder = Arc::new(Recorder {
        senders: vec![(from.into(), "claude-lead".into())],
        lookup_delay: Duration::from_millis(300),
        ..Recorder::default()
    });
    let mirror = Mirror::prepare(mirror_config(&home), &enabled())
        .unwrap()
        .publish(recorder.clone())
        .unwrap();
    let mut stream = authenticated(&door, &key);
    stream
        .write_all(format!("{}\n", letter_line(from, "mid-close")).as_bytes())
        .unwrap();
    assert!(eventually(|| recorder.lookups.load(Ordering::SeqCst) == 1));
    mirror.close();
    let at_close = recorder.accepted.lock().unwrap().len();
    std::thread::sleep(Duration::from_millis(500));
    assert_eq!(
        at_close, 1,
        "the frame being handed on landed before close returned"
    );
    assert_eq!(recorder.accepted.lock().unwrap().len(), 1);
}

// Expectation: with letters arriving on an accepted connection while the mirror closes, the count
// the handler saw when close returned never grows afterwards. Why: a check made before handling a
// frame can pass just before close and deliver just after it; admission and close must exclude
// each other. Value: "closed" means no further delivery, however the timing falls.
#[test]
fn frames_racing_a_close_never_reach_the_handler_after_it() {
    let home = ClaudeHome::new();
    let (door, key, _card) = door_paths(&home);
    let from = "uds:/tmp/cc-socks/77.sock";
    let recorder = Arc::new(Recorder {
        senders: vec![(from.into(), "claude-lead".into())],
        ..Recorder::default()
    });
    let mirror = Mirror::prepare(mirror_config(&home), &enabled())
        .unwrap()
        .publish(recorder.clone())
        .unwrap();
    let mut stream = authenticated(&door, &key);
    stream
        .set_write_timeout(Some(Duration::from_millis(200)))
        .unwrap();
    let line = format!("{}\n", letter_line(from, "flood"));
    let stop = Arc::new(AtomicBool::new(false));
    let writing = Arc::clone(&stop);
    let writer = std::thread::spawn(move || {
        while !writing.load(Ordering::SeqCst) && stream.write_all(line.as_bytes()).is_ok() {}
    });
    assert!(eventually(|| recorder.accepted.lock().unwrap().len() >= 20));
    mirror.close();
    let at_close = recorder.accepted.lock().unwrap().len();
    std::thread::sleep(Duration::from_millis(300));
    stop.store(true, Ordering::SeqCst);
    writer.join().unwrap();
    assert_eq!(recorder.accepted.lock().unwrap().len(), at_close);
}

// Expectation: when publishing fails (here a directory stands where the card belongs), nothing
// reaches the handler, a connection waiting since the mirror was prepared is ended, and the key
// and door socket are removed while the directory is left alone. Why: the door is already serving
// when the key and card are written, so a failed publish must shut it the same way close does.
// Value: a bridge that fails to appear in Claude's folder leaves no live door behind.
#[test]
fn a_failed_publish_shuts_the_door_and_leaves_nothing_behind() {
    let home = ClaudeHome::new();
    let (door, key, card) = door_paths(&home);
    std::fs::create_dir(&card).unwrap();
    let recorder = Arc::new(Recorder::default());
    let prepared = Mirror::prepare(mirror_config(&home), &enabled()).unwrap();
    let early = UnixStream::connect(&door).unwrap();
    assert!(prepared.publish(recorder.clone()).is_err());
    assert!(ended(&early), "the waiting connection was ended");
    assert!(!key.exists() && !door.exists());
    assert!(card.is_dir(), "what stood at the card's path is left alone");
    std::thread::sleep(Duration::from_millis(200));
    assert!(recorder.accepted.lock().unwrap().is_empty());
    assert!(recorder.refused.lock().unwrap().is_empty());
}
