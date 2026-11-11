// ABOUTME: Context service tests — snapshot completeness, revision monotonicity, secret
// ABOUTME: omission, the explanatory text view, and the AGENTS.md bootstrap installer.

use std::{path::Path, process::Command};

use taskai_context::{install_bootstrap, session_view, snapshot, text_view, tree_view};
use taskai_core::{
    mail::{proc_start, strip_messaging_env, Activity, Directory, MailPeer, PermissionMode},
    registry::{AddressBook, ConnectionTarget, Registry, SessionState, Source},
};

/// Registers a mailbox for `session_ref` as a bridge in process `pid`, started at `started`.
fn register(dir: &Path, reg: &Registry, session_ref: &str, pid: u32, started: &str) {
    let session = reg.session(session_ref).unwrap();
    AddressBook::new(dir, &session.task_ref)
        .register(&MailPeer {
            session_ref: session.session_ref.clone(),
            task_ref: session.task_ref.clone(),
            name: session.address.clone(),
            harness: session.harness.clone(),
            workspace: session.workspace.clone(),
            pid,
            proc_start: started.into(),
            status: Activity::Idle,
            waiting_for: None,
            mailbox: format!("/run/mail/{session_ref}.sock").into(),
            features: vec!["notify_idle".into()],
            mode: Some(PermissionMode::Prompting),
        })
        .unwrap();
}

/// A seeded registry where Lead's bridge is live and waiting on an approval, and Delivery's bridge
/// has exited without unregistering; Architecture never had one.
fn seeded_with_mailboxes(dir: &Path) -> (Registry, String, String, String, String) {
    let (mut reg, t, lead, arch, deliv) = seeded(dir);
    let me = std::process::id();
    register(dir, &reg, &lead, me, &proc_start(me).unwrap());
    let mut exited = Command::new("true");
    strip_messaging_env(&mut exited);
    let mut exited = exited.spawn().unwrap();
    let gone = exited.id();
    exited.wait().unwrap();
    register(dir, &reg, &deliv, gone, "Thu Jan  1 00:00:00 1970");
    AddressBook::new(dir, &t)
        .set_status(&lead, Activity::Waiting, Some("approval"))
        .unwrap();
    reg.reload().unwrap();
    (reg, t, lead, arch, deliv)
}

fn seeded(dir: &Path) -> (Registry, String, String, String, String) {
    let mut reg = Registry::open(dir).unwrap();
    let t = reg
        .create_task(
            "Repository evaluation",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let lead = reg
        .create_session(
            &t,
            None,
            "Lead",
            "claude",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let arch = reg
        .create_session(
            &t,
            Some(&lead),
            "Architecture",
            "opencode",
            Path::new("/tmp/buzz-worktrees/auth-pass"),
            Source::HumanUi,
        )
        .unwrap();
    let deliv = reg
        .create_session(
            &t,
            None,
            "Delivery",
            "codex",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    reg.set_state(&lead, SessionState::Live).unwrap();
    (reg, t, lead, arch, deliv)
}

// Expectation: the snapshot answers who am I, why do I exist, my team chain, and who is
// around me — task, self, team_chain, sessions — with schema_version and a revision.
// Why: backend §5 defines exactly this shape. Value: agents get complete facts.
#[test]
fn snapshot_is_complete() {
    let tmp = tempfile::tempdir().unwrap();
    let (reg, t, lead, arch, _deliv) = seeded(tmp.path());
    let snap = snapshot(&reg, &arch).unwrap();

    assert_eq!(snap["schema_version"], 1);
    assert!(snap["context_revision"].as_u64().unwrap() > 0);
    assert_eq!(snap["task"]["ref"], t.as_str());
    assert_eq!(snap["task"]["name"], "Repository evaluation");
    assert_eq!(snap["task"]["connection"], "<local>");
    assert_eq!(snap["self"]["session_ref"], arch.as_str());
    assert_eq!(snap["self"]["role"], "Subordinate");
    assert_eq!(snap["team_chain"]["parent"], lead.as_str());
    let sessions = snap["sessions"].as_array().unwrap();
    assert_eq!(sessions.len(), 3);
    assert!(sessions
        .iter()
        .any(|s| s["session_ref"] == lead.as_str() && s["state"] == "LIVE"));
}

// Expectation: remote repository paths remain paths while connection is a separate SSH
// command. Why: a path on cruise2 must never be mistaken for a local directory. Value:
// humans and agents can see both what repository they own and how it is reached.
#[test]
fn remote_context_names_repository_and_connection_separately() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = Registry::open(tmp.path()).unwrap();
    let task = reg
        .create_task_on(
            "Remote",
            Path::new("/srv/taskai"),
            ConnectionTarget::ssh("cruise2").unwrap(),
            Source::HumanUi,
        )
        .unwrap();
    let session = reg
        .create_session(
            &task,
            None,
            "Lead",
            "claude",
            Path::new("/srv/taskai"),
            Source::HumanUi,
        )
        .unwrap();

    let snap = snapshot(&reg, &session).unwrap();
    assert_eq!(snap["task"]["repo"], "/srv/taskai");
    assert_eq!(snap["task"]["connection"], "ssh cruise2");
    let text = text_view(&reg, &session).unwrap();
    assert!(text.contains("/srv/taskai via ssh cruise2"), "{text}");
}

// Expectation: the snapshot never carries credentials, pids, sockets, or transcript
// paths. Why: backend §5 omits provider internals. Value: context can be pasted
// anywhere without leaking runtime secrets.
#[test]
fn snapshot_omits_secrets() {
    let tmp = tempfile::tempdir().unwrap();
    let (reg, _t, lead, _arch, _deliv) = seeded(tmp.path());
    let body = snapshot(&reg, &lead).unwrap().to_string().to_lowercase();
    for banned in ["pid", "socket", "credential", "token", "transcript"] {
        assert!(!body.contains(banned), "snapshot leaks {banned:?}: {body}");
    }
}

// Expectation: the revision rises with every topology mutation. Why: the newest revision
// supersedes copies retained in native history. Value: staleness is detectable.
#[test]
fn revision_is_monotonic() {
    let tmp = tempfile::tempdir().unwrap();
    let (mut reg, t, lead, _arch, _deliv) = seeded(tmp.path());
    let r1 = snapshot(&reg, &lead).unwrap()["context_revision"]
        .as_u64()
        .unwrap();
    reg.create_session(
        &t,
        Some(&lead),
        "Verification",
        "grok",
        Path::new("/tmp/wt2"),
        Source::HumanUi,
    )
    .unwrap();
    let r2 = snapshot(&reg, &lead).unwrap()["context_revision"]
        .as_u64()
        .unwrap();
    assert!(r2 > r1, "{r2} > {r1}");
}

// Expectation: the text view explains rather than dumps — it tells the session who it
// is, why it exists, its chain, peers, and workspace in prose naming real refs.
// Why: backend §5 requires an explanatory human/model view. Value: a fresh model turn
// understands its place without parsing JSON.
#[test]
fn text_view_explains() {
    let tmp = tempfile::tempdir().unwrap();
    let (reg, _t, lead, arch, _deliv) = seeded(tmp.path());
    let text = text_view(&reg, &arch).unwrap();
    assert!(text.contains("You are Subordinate"), "{text}");
    assert!(text.contains("Architecture"), "{text}");
    assert!(text.contains("Repository evaluation"), "{text}");
    assert!(text.contains(&lead), "{text}");
    assert!(text.contains("/tmp/buzz-worktrees/auth-pass"), "{text}");
    assert!(text.contains("Report findings to"), "{text}");
}

// Expectation: unknown refs error. Why: context must never invent topology.
// Value: honest failure over fabricated facts.
#[test]
fn unknown_ref_errors() {
    let tmp = tempfile::tempdir().unwrap();
    let (reg, ..) = seeded(tmp.path());
    assert!(snapshot(&reg, "s999").is_err());
}

// Expectation: the bootstrap installer writes the managed block between markers, appends
// to existing AGENTS.md without touching user content, replaces only its own block on
// re-install, and links CLAUDE.md when absent. Why: backend §5 — never silently
// overwrite an instruction file. Value: safe on any repository.
#[test]
fn bootstrap_installs_managed_block() {
    let tmp = tempfile::tempdir().unwrap();
    let ws = tmp.path();
    std::fs::write(
        ws.join("AGENTS.md"),
        "# My project rules\nKeep tests green.\n",
    )
    .unwrap();

    install_bootstrap(ws).unwrap();
    install_bootstrap(ws).unwrap();

    let body = std::fs::read_to_string(ws.join("AGENTS.md")).unwrap();
    assert!(
        body.contains("# My project rules"),
        "user content preserved"
    );
    assert_eq!(
        body.matches("taskai:begin").count(),
        1,
        "one managed block: {body}"
    );
    assert!(body.contains("taskai context self"), "{body}");
    assert!(body.contains("taskai context tree"), "{body}");

    let claude = ws.join("CLAUDE.md");
    assert!(claude.exists());
    let meta = std::fs::symlink_metadata(&claude).unwrap();
    assert!(
        meta.file_type().is_symlink(),
        "CLAUDE.md is a symlink to AGENTS.md"
    );
}

// Expectation: an existing CLAUDE.md is never replaced. Why: never overwrite user
// instruction files. Value: TaskAI is a guest, not an owner.
#[test]
fn existing_claude_md_untouched() {
    let tmp = tempfile::tempdir().unwrap();
    let ws = tmp.path();
    std::fs::write(ws.join("CLAUDE.md"), "my own claude rules\n").unwrap();
    install_bootstrap(ws).unwrap();
    assert_eq!(
        std::fs::read_to_string(ws.join("CLAUDE.md")).unwrap(),
        "my own claude rules\n"
    );
}

// Expectation: each session in a snapshot names its messaging address and, while its bridge is
// live, its messaging status and what it waits for; a session whose bridge exited or never ran
// shows no messaging, and no pid or socket appears. Why: agents choose whom to message from the
// snapshot. Value: an agent addresses a teammate exactly and sees whether it is free.
#[test]
fn snapshot_names_each_sessions_address_and_live_messaging_status() {
    let tmp = tempfile::tempdir().unwrap();
    let (reg, _t, lead, arch, deliv) = seeded_with_mailboxes(tmp.path());
    let snap = snapshot(&reg, &arch).unwrap();
    let session = |r: &str| {
        snap["sessions"]
            .as_array()
            .unwrap()
            .iter()
            .find(|s| s["session_ref"] == r)
            .unwrap()
            .clone()
    };
    assert_eq!(session(&lead)["address"], "claude-lead");
    assert_eq!(
        session(&lead)["messaging"],
        serde_json::json!({"status": "waiting", "waitingFor": "approval"})
    );
    assert_eq!(session(&arch)["address"], "opencode-architecture");
    assert!(session(&arch)["messaging"].is_null());
    assert!(session(&deliv)["messaging"].is_null(), "an exited bridge");
    assert_eq!(snap["self"]["address"], "opencode-architecture");
    let body = snap.to_string().to_lowercase();
    for banned in ["pid", "sock", "/run/mail"] {
        assert!(!body.contains(banned), "snapshot leaks {banned:?}: {body}");
    }
}

// Expectation: every tree line ends with the session's address and messaging status: busy,
// idle, `waiting: <what>`, or `no mailbox` when no live bridge holds one. Why: agents run
// `taskai context tree` before delegating. Value: the tree says whom to message and whether
// they can take a letter now.
#[test]
fn tree_shows_each_sessions_address_and_messaging_status() {
    let tmp = tempfile::tempdir().unwrap();
    let (mut reg, t, lead, arch, deliv) = seeded_with_mailboxes(tmp.path());
    let tree = tree_view(&reg, Some(&t));
    let lines: Vec<&str> = tree.lines().collect();
    assert_eq!(
        lines[1],
        format!(
            "  Lead ({lead}) · claude · LIVE · /tmp/buzz · address claude-lead · waiting: approval"
        )
    );
    assert_eq!(
        lines[2],
        format!(
            "    └─ Architecture ({arch}) · opencode · QUEUED · /tmp/buzz-worktrees/auth-pass · address opencode-architecture · no mailbox"
        )
    );
    assert_eq!(
        lines[3],
        format!("  Delivery ({deliv}) · codex · QUEUED · /tmp/buzz · address codex-delivery · no mailbox")
    );
    AddressBook::new(tmp.path(), &t)
        .set_status(&lead, Activity::Busy, None)
        .unwrap();
    reg.reload().unwrap();
    assert!(
        tree_view(&reg, Some(&t))
            .lines()
            .nth(1)
            .unwrap()
            .ends_with("· address claude-lead · busy"),
        "{}",
        tree_view(&reg, Some(&t))
    );
}

// Expectation: the text view tells a session its own address and names each subordinate's and
// peer's address and messaging status; the session view does the same for the session it
// describes. Why: these are the prose views agents read each turn. Value: the reply address and
// availability are in the words an agent already reads.
#[test]
fn text_and_session_views_name_addresses_and_messaging_status() {
    let tmp = tempfile::tempdir().unwrap();
    let (reg, _t, lead, arch, deliv) = seeded_with_mailboxes(tmp.path());
    let lead_text = text_view(&reg, &lead).unwrap();
    assert!(
        lead_text.contains("Your messaging address is claude-lead."),
        "{lead_text}"
    );
    assert!(
        lead_text.contains(&format!(
            "Your subordinate Architecture ({arch}) · opencode · QUEUED runs in /tmp/buzz-worktrees/auth-pass · address opencode-architecture · no mailbox."
        )),
        "{lead_text}"
    );
    assert!(
        lead_text.contains(&format!(
            "Peer Delivery ({deliv}) · codex · QUEUED works in /tmp/buzz · address codex-delivery · no mailbox."
        )),
        "{lead_text}"
    );
    let delivery_text = text_view(&reg, &deliv).unwrap();
    assert!(
        delivery_text.contains(&format!(
            "Peer Lead ({lead}) · claude · LIVE works in /tmp/buzz · address claude-lead · waiting: approval."
        )),
        "{delivery_text}"
    );
    let view = session_view(&reg, &lead).unwrap();
    assert!(
        view.ends_with("Harness claude · state LIVE · workspace /tmp/buzz · address claude-lead · waiting: approval."),
        "{view}"
    );
}
