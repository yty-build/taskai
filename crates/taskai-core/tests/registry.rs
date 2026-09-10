// ABOUTME: Registry tests — stable refs, topology, local/SSH and runtime mappings,
// ABOUTME: atomic persistence across restarts, blast radius, and destroy semantics.

use std::path::Path;

use taskai_core::registry::{
    ConnectionTarget, Item, ProviderMapping, Registry, SessionState, Source,
};

fn open(dir: &Path) -> Registry {
    Registry::open(dir).expect("open registry")
}

#[test]
fn terminal_is_one_task_owned_mapping_not_a_team_member() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task_on(
            "Remote",
            Path::new("/repo"),
            ConnectionTarget::ssh("cruise4b").unwrap(),
            Source::HumanUi,
        )
        .unwrap();
    assert!(reg.task(&task).unwrap().terminal.is_none());
    let counts = reg.hud_counts();
    let terminal = reg.bind_terminal(&task, "taskai-test").unwrap();
    assert_eq!(terminal.session, "t1-terminal");
    assert_eq!(terminal.target, ConnectionTarget::ssh("cruise4b").unwrap());
    assert_eq!(
        reg.bind_terminal(&task, "another-server").unwrap(),
        terminal
    );
    assert_eq!(reg.hud_counts(), counts);
    assert!(reg.sessions_of(&task).is_empty());
    let manager = reg
        .create_session(
            &task,
            None,
            "Lead",
            "claude",
            Path::new("/repo"),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(manager, "s1", "utilities must not consume agent refs");
    reg.destroy(&Item::Session(manager), Source::HumanUi)
        .unwrap();
    drop(reg);
    let mut reg = open(root.path());
    assert_eq!(reg.task(&task).unwrap().terminal.as_ref(), Some(&terminal));
    reg.destroy(&Item::Task(task.clone()), Source::HumanUi)
        .unwrap();
    assert!(open(root.path()).task(&task).is_none());
}

#[test]
fn terminal_mapping_cannot_point_to_a_different_task_or_host() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task("Task", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    reg.bind_terminal(&task, "taskai-test").unwrap();
    let state = root.path().join("state.json");
    let original: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&state).unwrap()).unwrap();
    for field in ["session", "target"] {
        let mut value = original.clone();
        value["tasks"][0]["terminal"][field] = if field == "session" {
            serde_json::json!("t2-terminal")
        } else {
            serde_json::json!({"kind":"ssh","host":"cruise4b"})
        };
        std::fs::write(&state, serde_json::to_string(&value).unwrap()).unwrap();
        assert!(Registry::open(root.path()).is_err());
    }
}

// Expectation: refs increment monotonically and are never recycled, even after destroy
// and restart. Why: a recycled ref could route a prompt into the wrong provider session.
// Value: exact session identity is structural, not conventional.
#[test]
fn refs_never_recycled() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = open(tmp.path());
    let t = reg
        .create_task(
            "Repository evaluation",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let s1 = reg
        .create_session(
            &t,
            None,
            "Lead",
            "claude",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    reg.destroy(&Item::Session(s1.clone()), Source::HumanUi)
        .unwrap();
    let s2 = reg
        .create_session(
            &t,
            None,
            "Lead",
            "claude",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    assert_ne!(s1, s2);

    drop(reg);
    let mut reg = open(tmp.path());
    let s3 = reg
        .create_session(
            &t,
            None,
            "Delivery",
            "codex",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    assert!(![s1, s2].contains(&s3));
}

// Expectation: role is derived from topology — no parent means Manager, a parent means
// Subordinate; a parent must be a Manager of the same task. Why: the vision's tree is
// Task -> Manager -> Subordinate, depth one. Value: impossible topologies can't exist.
#[test]
fn topology_rules() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = open(tmp.path());
    let t1 = reg
        .create_task(
            "Repository evaluation",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let t2 = reg
        .create_task(
            "Production release",
            Path::new("/tmp/shop"),
            Source::HumanUi,
        )
        .unwrap();
    let lead = reg
        .create_session(
            &t1,
            None,
            "Lead",
            "claude",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let arch = reg
        .create_session(
            &t1,
            Some(&lead),
            "Architecture",
            "opencode",
            Path::new("/tmp/wt"),
            Source::HumanUi,
        )
        .unwrap();
    let deliv = reg
        .create_session(
            &t1,
            None,
            "Delivery",
            "codex",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();

    assert!(reg.session(&lead).unwrap().parent.is_none());
    assert_eq!(
        reg.session(&arch).unwrap().parent.as_deref(),
        Some(lead.as_str())
    );
    let of_t1: Vec<_> = reg
        .sessions_of(&t1)
        .iter()
        .map(|s| s.session_ref.clone())
        .collect();
    assert_eq!(of_t1, vec![lead.clone(), arch.clone(), deliv.clone()]);
    let kids: Vec<_> = reg
        .children_of(&lead)
        .iter()
        .map(|s| s.session_ref.clone())
        .collect();
    assert_eq!(kids, vec![arch.clone()]);

    assert!(reg
        .create_session(
            "t999",
            None,
            "X",
            "codex",
            Path::new("/tmp"),
            Source::HumanUi
        )
        .is_err());
    assert!(reg
        .create_session(
            &t2,
            Some(&lead),
            "X",
            "codex",
            Path::new("/tmp"),
            Source::HumanUi
        )
        .is_err());
    assert!(reg
        .create_session(
            &t1,
            Some(&arch),
            "X",
            "codex",
            Path::new("/tmp"),
            Source::HumanUi
        )
        .is_err());
}

// Expectation: every mutation records its source — human UI or a registered session.
// Why: backend §2 requires attributing invocations to their origin. Value: the audit
// trail distinguishes what the human did from what an agent did.
#[test]
fn mutations_attributed() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = open(tmp.path());
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
    reg.create_session(
        &t,
        Some(&lead),
        "Architecture",
        "opencode",
        Path::new("/tmp/wt"),
        Source::Session(lead.clone()),
    )
    .unwrap();

    assert_eq!(reg.session(&lead).unwrap().created_by, Source::HumanUi);
    let audit = reg.audit();
    assert!(audit
        .iter()
        .any(|a| a.action == "create_task" && a.source == Source::HumanUi));
    assert!(audit
        .iter()
        .any(|a| a.action == "create_session" && a.source == Source::Session(lead.clone())));
}

// Expectation: state survives restart byte-identically, and a leftover torn temp file is
// ignored. Why: leave-and-resume is journey step 10; persistence is atomic rename.
// Value: a crash can lose at most the in-flight write, never the store.
#[test]
fn persistence_survives_restart() {
    let tmp = tempfile::tempdir().unwrap();
    let (t, lead);
    {
        let mut reg = open(tmp.path());
        t = reg
            .create_task(
                "Repository evaluation",
                Path::new("/tmp/buzz"),
                Source::HumanUi,
            )
            .unwrap();
        lead = reg
            .create_session(
                &t,
                None,
                "Lead",
                "claude",
                Path::new("/tmp/buzz"),
                Source::HumanUi,
            )
            .unwrap();
        reg.set_state(&lead, SessionState::Live).unwrap();
    }
    std::fs::write(tmp.path().join("state.json.tmp"), b"{torn").unwrap();
    let reg = open(tmp.path());
    assert_eq!(reg.tasks().len(), 1);
    assert_eq!(reg.session(&lead).unwrap().state, SessionState::Live);
    assert_eq!(reg.session(&lead).unwrap().task_ref, t);
}

// Expectation: set_state on an unknown ref is an error, never a silent no-op.
// Why: truthful state is a §2 responsibility. Value: state bugs surface immediately.
#[test]
fn set_state_unknown_ref_errors() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = open(tmp.path());
    assert!(reg.set_state("s999", SessionState::Live).is_err());
}

// Expectation: blast lists exactly what destroy will remove, in confirmation order —
// a subordinate alone; a manager with its children; a task with every session. Destroy
// removes exactly that and, for a task, the task entry. Why: UX vision §6 shows the
// exact blast radius before anything stops. Value: destroy is trustworthy.
#[test]
fn blast_radius_and_destroy() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = open(tmp.path());
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
            Path::new("/tmp/wt"),
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
    let ver = reg
        .create_session(
            &t,
            Some(&deliv),
            "Verification",
            "grok",
            Path::new("/tmp/wt2"),
            Source::HumanUi,
        )
        .unwrap();

    assert_eq!(
        reg.blast(&Item::Session(arch.clone())).unwrap(),
        vec![arch.clone()]
    );
    assert_eq!(
        reg.blast(&Item::Session(lead.clone())).unwrap(),
        vec![lead.clone(), arch.clone()]
    );
    assert_eq!(
        reg.blast(&Item::Task(t.clone())).unwrap(),
        vec![lead.clone(), arch.clone(), deliv.clone(), ver.clone()]
    );

    let removed = reg
        .destroy(&Item::Session(lead.clone()), Source::HumanUi)
        .unwrap();
    assert_eq!(removed, vec![lead.clone(), arch.clone()]);
    assert!(reg.session(&lead).is_none());
    assert!(reg.session(&arch).is_none());
    assert!(reg.session(&deliv).is_some());

    let removed = reg
        .destroy(&Item::Task(t.clone()), Source::HumanUi)
        .unwrap();
    assert_eq!(removed, vec![deliv, ver]);
    assert!(reg.task(&t).is_none());
    assert!(reg.tasks().is_empty());
}

// Expectation: HUD counts derive from the one session list — a HANDOFF session never
// counts LIVE; ATTENTION rolls up to its task; COMPLETE tasks drop out of active.
// Why: the journey-coherence lesson — state miscounts are product-truth bugs.
// Value: the masthead can't lie.
#[test]
fn hud_counts_and_task_status() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = Registry::open(tmp.path()).unwrap();
    let t1 = reg
        .create_task(
            "Repository evaluation",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let lead = reg
        .create_session(
            &t1,
            None,
            "Lead",
            "claude",
            Path::new("/tmp/buzz"),
            Source::HumanUi,
        )
        .unwrap();
    let ver = reg
        .create_session(
            &t1,
            Some(&lead),
            "Verification",
            "grok",
            Path::new("/tmp/wt"),
            Source::HumanUi,
        )
        .unwrap();
    reg.set_state(&lead, SessionState::Live).unwrap();
    reg.set_state(&ver, SessionState::Handoff).unwrap();

    let t2 = reg
        .create_task(
            "Production release",
            Path::new("/tmp/shop"),
            Source::HumanUi,
        )
        .unwrap();
    let ship = reg
        .create_session(
            &t2,
            None,
            "Ship",
            "opencode",
            Path::new("/tmp/shop"),
            Source::HumanUi,
        )
        .unwrap();
    reg.set_state(&ship, SessionState::Attention).unwrap();

    let t3 = reg
        .create_task("API migration", Path::new("/tmp/atlas"), Source::HumanUi)
        .unwrap();
    let done = reg
        .create_session(
            &t3,
            None,
            "Migrator",
            "grok",
            Path::new("/tmp/atlas"),
            Source::HumanUi,
        )
        .unwrap();
    reg.set_state(&done, SessionState::Complete).unwrap();

    let t4 = reg
        .create_task("Empty", Path::new("/tmp/e"), Source::HumanUi)
        .unwrap();

    let hud = reg.hud_counts();
    assert_eq!(hud.live, 1, "handoff and attention are never LIVE");
    assert_eq!(hud.queued, 1, "handoff counts queued");
    assert_eq!(hud.attention, 1);
    assert_eq!(
        hud.tasks_active, 2,
        "t1 and t2; complete t3 and empty t4 excluded"
    );

    assert_eq!(reg.task_status(&t1), "WORKING");
    assert_eq!(reg.task_status(&t2), "NEEDS ATTENTION");
    assert_eq!(reg.task_status(&t3), "COMPLETE");
    assert_eq!(reg.task_status(&t4), "READY");
}

// Expectation: an omitted connection remains local, while an SSH target stores only its
// safe host alias and round-trips independently from the repository path. Why: existing
// callers and state must remain local without migration, while remote paths are meaningful
// only on their target. Value: the tracker says both where a repo is and how to reach it.
#[test]
fn connection_targets_round_trip_and_display() {
    let tmp = tempfile::tempdir().unwrap();
    let (local, remote);
    {
        let mut reg = open(tmp.path());
        local = reg
            .create_task("Local", Path::new("/tmp/local-repo"), Source::HumanUi)
            .unwrap();
        remote = reg
            .create_task_on(
                "Remote",
                Path::new("/srv/remote-repo"),
                ConnectionTarget::ssh("cruise2").unwrap(),
                Source::HumanUi,
            )
            .unwrap();

        assert_eq!(reg.task(&local).unwrap().connection.to_string(), "<local>");
        assert!(reg
            .task(&local)
            .unwrap()
            .connection
            .command_args()
            .is_empty());
        assert_eq!(
            reg.task(&remote).unwrap().connection.command_args(),
            vec!["ssh", "cruise2"]
        );
        assert_eq!(
            reg.task(&remote).unwrap().connection.to_string(),
            "ssh cruise2"
        );
    }

    let reg = open(tmp.path());
    assert_eq!(
        reg.task(&local).unwrap().connection,
        ConnectionTarget::Local
    );
    assert_eq!(
        reg.task(&remote).unwrap().connection,
        ConnectionTarget::Ssh {
            host: "cruise2".into()
        }
    );
    assert_eq!(
        reg.task(&remote).unwrap().repo,
        Path::new("/srv/remote-repo")
    );

    for unsafe_alias in ["", "-oProxyCommand=bad", "user@cruise2", "cruise 2"] {
        assert!(ConnectionTarget::ssh(unsafe_alias).is_err());
    }
}

// Expectation: both the current unversioned registry and a state file labeled schema v1
// load without connection/runtime fields. Why: tmux persistence is additive. Value:
// installing the new binary does not strand existing Tasks or provider sessions.
#[test]
fn legacy_state_defaults_to_local_without_runtime() {
    for schema_version in [None, Some(1)] {
        let tmp = tempfile::tempdir().unwrap();
        let mut body = serde_json::json!({
            "next_task": 1,
            "next_session": 1,
            "tasks": [{
                "task_ref": "t1",
                "name": "Legacy task",
                "repo": "/tmp/legacy",
                "created_by": "HumanUi"
            }],
            "sessions": [{
                "session_ref": "s1",
                "task_ref": "t1",
                "parent": null,
                "role": "Manager",
                "name": "Lead",
                "harness": "claude",
                "workspace": "/tmp/legacy",
                "state": "Live",
                "created_by": "HumanUi",
                "provider": null
            }],
            "audit": []
        });
        if let Some(version) = schema_version {
            body.as_object_mut()
                .unwrap()
                .insert("schema_version".into(), version.into());
        }
        std::fs::write(
            tmp.path().join("state.json"),
            serde_json::to_vec_pretty(&body).unwrap(),
        )
        .unwrap();

        let reg = open(tmp.path());
        assert_eq!(reg.task("t1").unwrap().connection, ConnectionTarget::Local);
        assert!(reg.session("s1").unwrap().runtime.is_none());
        assert_eq!(reg.session("s1").unwrap().state, SessionState::Live);
    }
}

// Expectation: tmux identity and native provider identity persist independently. Why:
// tmux locates a durable process but must never replace a Claude/Codex/etc session ID.
// Value: detaching or repairing one mapping cannot silently erase the other identity.
#[test]
fn runtime_and_provider_identity_remain_separate() {
    let tmp = tempfile::tempdir().unwrap();
    let session_ref;
    let provider = ProviderMapping {
        provider: "claude".into(),
        provider_session_id: "provider-session-42".into(),
        runtime_id: "provider-runtime-9".into(),
        connector_version: "1".into(),
    };
    {
        let mut reg = open(tmp.path());
        let task_ref = reg
            .create_task_on(
                "Remote evaluation",
                Path::new("/srv/acp-gateway"),
                ConnectionTarget::ssh("cruise2").unwrap(),
                Source::HumanUi,
            )
            .unwrap();
        session_ref = reg
            .create_session(
                &task_ref,
                None,
                "Lead",
                "claude",
                Path::new("/srv/acp-gateway"),
                Source::HumanUi,
            )
            .unwrap();
        reg.bind_provider(&session_ref, provider.clone()).unwrap();
        reg.bind_runtime(&session_ref, "taskai-test-17").unwrap();

        let session = reg.session(&session_ref).unwrap();
        assert_eq!(session.provider.as_ref(), Some(&provider));
        let runtime = session.runtime.as_ref().unwrap();
        assert_eq!(runtime.session, session_ref);
        assert_eq!(runtime.private_server, "taskai-test-17");
        assert_eq!(
            runtime.target,
            ConnectionTarget::Ssh {
                host: "cruise2".into()
            }
        );
    }

    let mut reg = open(tmp.path());
    assert_eq!(
        reg.session(&session_ref).unwrap().provider.as_ref(),
        Some(&provider)
    );
    assert!(reg.session(&session_ref).unwrap().runtime.is_some());

    reg.clear_runtime(&session_ref).unwrap();
    assert!(reg.session(&session_ref).unwrap().runtime.is_none());
    assert_eq!(
        reg.session(&session_ref).unwrap().provider.as_ref(),
        Some(&provider)
    );

    reg.bind_runtime(&session_ref, "taskai-test-17").unwrap();
    reg.clear_provider(&session_ref).unwrap();
    assert!(reg.session(&session_ref).unwrap().provider.is_none());
    assert!(reg.session(&session_ref).unwrap().runtime.is_some());

    assert!(reg.bind_runtime("s999", "taskai-test-17").is_err());
    assert!(reg.bind_runtime(&session_ref, "-not-a-server").is_err());
    assert!(reg.clear_runtime("s999").is_err());
}
