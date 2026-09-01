// ABOUTME: Context service tests — snapshot completeness, revision monotonicity, secret
// ABOUTME: omission, the explanatory text view, and the AGENTS.md bootstrap installer.

use std::path::Path;

use taskai_context::{install_bootstrap, snapshot, text_view};
use taskai_core::registry::{Registry, SessionState, Source};

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
    assert_eq!(snap["self"]["session_ref"], arch.as_str());
    assert_eq!(snap["self"]["role"], "Subordinate");
    assert_eq!(snap["team_chain"]["parent"], lead.as_str());
    let sessions = snap["sessions"].as_array().unwrap();
    assert_eq!(sessions.len(), 3);
    assert!(sessions
        .iter()
        .any(|s| s["session_ref"] == lead.as_str() && s["state"] == "LIVE"));
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
