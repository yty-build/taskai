// ABOUTME: Registry tests — stable refs, topology, local/SSH and runtime mappings,
// ABOUTME: atomic persistence across restarts, blast radius, and destroy semantics.

use std::{
    collections::HashSet,
    os::unix::fs::{MetadataExt, PermissionsExt},
    path::Path,
    process::{Child, Command, Stdio},
    sync::{mpsc, Arc},
    time::Duration,
};

use taskai_core::{
    mail::{
        is_address, proc_start, strip_messaging_env, Activity, Directory, Identity, MailPeer,
        Mailbox, MailboxConfig, PermissionMode, SendStatus, SocketTransport,
    },
    registry::{
        AddressBook, ConnectionTarget, HarnessSettings, Item, ProviderMapping, Registry,
        SessionState, Source,
    },
    Error,
};

/// This test process's pid and start time, as a bridge running here would register them.
fn this_process() -> (u32, String) {
    let pid = std::process::id();
    (pid, proc_start(pid).expect("this process has a start time"))
}

/// A process standing in for another session's bridge: alive until killed.
fn other_bridge() -> (Child, u32, String) {
    let mut sleep = Command::new("sleep");
    sleep.arg("30");
    strip_messaging_env(&mut sleep);
    let child = sleep.spawn().unwrap();
    let pid = child.id();
    let started = proc_start(pid).expect("a running child has a start time");
    (child, pid, started)
}

/// A pid whose process has exited and been reaped.
fn gone_pid() -> u32 {
    let mut done = Command::new("true");
    strip_messaging_env(&mut done);
    let mut child = done.spawn().unwrap();
    let pid = child.id();
    child.wait().unwrap();
    pid
}

/// The mailbox entry a bridge for `session_ref` would register, with the given process.
fn entry(reg: &Registry, session_ref: &str, pid: u32, started: &str, mailbox: &str) -> MailPeer {
    let session = reg.session(session_ref).unwrap();
    MailPeer {
        session_ref: session.session_ref.clone(),
        task_ref: session.task_ref.clone(),
        name: session.address.clone(),
        harness: session.harness.clone(),
        workspace: session.workspace.clone(),
        pid,
        proc_start: started.into(),
        status: Activity::Idle,
        waiting_for: None,
        mailbox: mailbox.into(),
        features: vec!["notify_idle".into()],
        mode: Some(PermissionMode::Prompting),
    }
}

fn session_in(reg: &mut Registry, task: &str, name: &str, harness: &str) -> String {
    reg.create_session(
        task,
        None,
        name,
        harness,
        Path::new("/repo"),
        Source::HumanUi,
    )
    .unwrap()
}

fn refs(peers: Vec<MailPeer>) -> Vec<String> {
    peers.into_iter().map(|peer| peer.session_ref).collect()
}

fn open(dir: &Path) -> Registry {
    Registry::open(dir).expect("open registry")
}

/// The registry file's inode and text: an atomic save always makes a new inode, so equal values
/// mean nothing was written.
fn state_as_written(dir: &Path) -> (u64, String) {
    let state = dir.join("state.json");
    (
        std::fs::metadata(&state).unwrap().ino(),
        std::fs::read_to_string(&state).unwrap(),
    )
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
                Path::new("/srv/taskai"),
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
                Path::new("/srv/taskai"),
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

// Expectation: two handles on one registry directory, as the UI and a bridge process hold, never
// lose each other's changes. A change through a handle opened before another handle's changes
// applies to the state on disk, so sessions, destroys and server settings made elsewhere survive,
// refs stay unique, and the handle's view catches up. Why: each handle persisted its whole
// in-memory snapshot, so the stalest writer erased or resurrected the rest. Value: the UI and the
// bridges can share one registry.
#[test]
fn a_stale_handle_never_erases_another_handles_changes() {
    let root = tempfile::tempdir().unwrap();
    let mut ui = open(root.path());
    let task = ui
        .create_task("Shared", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let lead = ui
        .create_session(
            &task,
            None,
            "Lead",
            "claude",
            Path::new("/repo"),
            Source::HumanUi,
        )
        .unwrap();
    let doomed = ui
        .create_session(
            &task,
            Some(&lead),
            "Scratch",
            "opencode",
            Path::new("/repo"),
            Source::HumanUi,
        )
        .unwrap();
    let mut other = open(root.path());
    let helper = other
        .create_session(
            &task,
            Some(&lead),
            "Helper",
            "codex",
            Path::new("/repo"),
            Source::HumanUi,
        )
        .unwrap();
    other.set_state(&helper, SessionState::Working).unwrap();
    other
        .destroy(&Item::Session(doomed.clone()), Source::HumanUi)
        .unwrap();
    other
        .set_server_harness(
            "local",
            "grok",
            HarnessSettings {
                enabled: false,
                ..HarnessSettings::default()
            },
            Source::HumanUi,
        )
        .unwrap();
    ui.set_state(&lead, SessionState::Live).unwrap();
    let reviewer = ui
        .create_session(
            &task,
            Some(&lead),
            "Reviewer",
            "claude",
            Path::new("/repo"),
            Source::HumanUi,
        )
        .unwrap();
    assert_ne!(reviewer, helper, "refs stay unique across handles");
    assert_eq!(
        ui.session(&helper).map(|s| s.state),
        Some(SessionState::Working),
        "a change brings the handle's view up to date"
    );
    let reopened = open(root.path());
    assert_eq!(
        reopened.session(&helper).map(|s| s.state),
        Some(SessionState::Working)
    );
    assert_eq!(
        reopened.session(&lead).map(|s| s.state),
        Some(SessionState::Live)
    );
    assert!(reopened.session(&reviewer).is_some());
    assert!(
        reopened.session(&doomed).is_none(),
        "a destroyed session stays destroyed"
    );
    assert!(
        !reopened.server("local").unwrap().harnesses["grok"].enabled,
        "a server setting made elsewhere is kept"
    );
}

// Expectation: after changes the saved state is owner-only, the registry directory holds nothing
// but state.json and its lock, and a top-level setting this build does not know survives every
// change. Why: several processes rewrite the registry; a staging file must not linger or be
// readable by others, and a setting another component saved must not be dropped by a rewrite.
// Value: frequent bridge writes cost nothing the person saved.
#[test]
fn saving_is_owner_only_and_keeps_settings_it_does_not_know() {
    let root = tempfile::tempdir().unwrap();
    drop(open(root.path()));
    let state = root.path().join("state.json");
    let mut body: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&state).unwrap()).unwrap();
    body["ui_settings"] = serde_json::json!({"theme": "dark"});
    std::fs::write(&state, serde_json::to_vec_pretty(&body).unwrap()).unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task("Kept", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    reg.create_session(
        &task,
        None,
        "Lead",
        "claude",
        Path::new("/repo"),
        Source::HumanUi,
    )
    .unwrap();
    assert_eq!(
        std::fs::metadata(&state).unwrap().permissions().mode() & 0o777,
        0o600
    );
    let mut names: Vec<String> = std::fs::read_dir(root.path())
        .unwrap()
        .map(|entry| entry.unwrap().file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    assert_eq!(names, ["state.json", "state.lock"]);
    let saved: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&state).unwrap()).unwrap();
    assert_eq!(saved["ui_settings"], serde_json::json!({"theme": "dark"}));
}

// Expectation: every session gets a messaging address when it is created: `<harness>-<display
// name>` in lowercase ASCII letters, digits and single dashes, without repeating a harness prefix
// the name already carries, cut to 64 characters, `<harness>-<ref>` for a name with no ASCII
// letters or digits, and `-<ref>` appended on a collision within the task. The display name is
// never changed, an address never changes afterwards, and another task may use the same one.
// Why: agents address each other by plain names, while people name agents freely. Value: any
// display name works, and a reply always reaches the session it names.
#[test]
fn every_session_gets_a_stable_plain_address_and_keeps_its_display_name() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task("Names", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let create = |reg: &mut Registry, task: &str, name: &str, harness: &str| {
        reg.create_session(
            task,
            None,
            name,
            harness,
            Path::new("/repo"),
            Source::HumanUi,
        )
        .unwrap()
    };
    let helper = create(&mut reg, &task, "TaskAI Helper", "claude");
    let lead = create(&mut reg, &task, "codex-lead", "codex");
    let second = create(&mut reg, &task, "Lead", "codex");
    let unicode = create(&mut reg, &task, "日本語", "grok");
    let accented = create(&mut reg, &task, "Équipe Rouge!", "opencode");
    let long = create(
        &mut reg,
        &task,
        &"a very long display name ".repeat(5),
        "claude",
    );
    let address = |reg: &Registry, r: &str| reg.session(r).unwrap().address.clone();
    assert_eq!(address(&reg, &helper), "claude-taskai-helper");
    assert_eq!(address(&reg, &lead), "codex-lead");
    assert_eq!(address(&reg, &second), format!("codex-lead-{second}"));
    assert_eq!(address(&reg, &unicode), format!("grok-{unicode}"));
    assert_eq!(address(&reg, &accented), "opencode-quipe-rouge");
    let long_address = address(&reg, &long);
    assert!(
        long_address.len() <= 64 && long_address.starts_with("claude-a-very-long-display-name-a"),
        "{long_address}"
    );
    for (r, name) in [
        (&helper, "TaskAI Helper"),
        (&unicode, "日本語"),
        (&accented, "Équipe Rouge!"),
    ] {
        assert_eq!(reg.session(r).unwrap().name, name);
    }
    let all: Vec<String> = reg
        .sessions_of(&task)
        .iter()
        .map(|s| s.address.clone())
        .collect();
    assert!(all.iter().all(|a| is_address(a)), "{all:?}");
    assert_eq!(
        all.iter().collect::<HashSet<_>>().len(),
        all.len(),
        "{all:?}"
    );
    reg.destroy(&Item::Session(lead.clone()), Source::HumanUi)
        .unwrap();
    assert_eq!(
        address(&open(root.path()), &second),
        format!("codex-lead-{second}"),
        "an address never changes"
    );
    let elsewhere = reg
        .create_task("Elsewhere", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let other_lead = create(&mut reg, &elsewhere, "Lead", "codex");
    assert_eq!(
        address(&reg, &other_lead),
        "codex-lead",
        "another task may use the same address"
    );
}

// Expectation: sessions saved before addresses existed get one when the registry first opens, in
// record order and by the same rule as new sessions; an address already saved is kept exactly,
// and reopening rewrites nothing. Why: existing tasks must join messaging without being
// recreated. Value: upgrading TaskAI makes existing agents addressable.
#[test]
fn legacy_sessions_get_addresses_once_when_the_registry_opens() {
    let tmp = tempfile::tempdir().unwrap();
    let session = |r: &str, name: &str, address: Option<&str>| {
        let mut session = serde_json::json!({
            "session_ref": r, "task_ref": "t1", "parent": null, "role": "Manager",
            "name": name, "harness": "codex", "workspace": "/tmp/legacy", "state": "Live",
            "created_by": "HumanUi", "provider": null
        });
        if let Some(address) = address {
            session["address"] = address.into();
        }
        session
    };
    let body = serde_json::json!({
        "next_task": 1,
        "next_session": 3,
        "tasks": [{"task_ref": "t1", "name": "Legacy", "repo": "/tmp/legacy",
                   "created_by": "HumanUi"}],
        "sessions": [session("s1", "Lead", None), session("s2", "Lead", None),
                     session("s3", "Reviewer", Some("Custom.Name"))],
        "audit": []
    });
    let state = tmp.path().join("state.json");
    std::fs::write(&state, serde_json::to_vec_pretty(&body).unwrap()).unwrap();
    let reg = open(tmp.path());
    assert_eq!(reg.session("s1").unwrap().address, "codex-lead");
    assert_eq!(reg.session("s2").unwrap().address, "codex-lead-s2");
    assert_eq!(reg.session("s3").unwrap().address, "Custom.Name");
    let saved = std::fs::read(&state).unwrap();
    drop(open(tmp.path()));
    assert_eq!(
        std::fs::read(&state).unwrap(),
        saved,
        "reopening rewrites nothing"
    );
}

// Expectation: a task's address book lists only sessions of that task whose bridge registered a
// mailbox and still runs: an entry whose pid is gone, whose pid now belongs to a process started
// at another time, whose record is invalid (a relative mailbox path), or that belongs to another
// task, is not routable, and a session without an entry is absent. Each listed peer carries the
// session's address and its registered mailbox. Why: a stale or recycled pid would route a letter
// to a stranger. Value: only live TaskAI sessions of the caller's task can be messaged.
#[test]
fn the_address_book_lists_only_live_registered_sessions_of_its_task() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let one = reg
        .create_task("One", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let two = reg
        .create_task("Two", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let live = session_in(&mut reg, &one, "Live", "codex");
    let gone = session_in(&mut reg, &one, "Gone", "grok");
    let reused = session_in(&mut reg, &one, "Reused", "claude");
    let invalid = session_in(&mut reg, &one, "Invalid", "opencode");
    let silent = session_in(&mut reg, &one, "Silent", "claude");
    let elsewhere = session_in(&mut reg, &two, "Elsewhere", "codex");
    let (me, started) = this_process();
    let book_one = AddressBook::new(root.path(), &one);
    let book_two = AddressBook::new(root.path(), &two);
    let long_ago = "Thu Jan  1 00:00:00 1970";
    book_one
        .register(&entry(&reg, &live, me, &started, "/run/mail/live.sock"))
        .unwrap();
    book_one
        .register(&entry(
            &reg,
            &gone,
            gone_pid(),
            long_ago,
            "/run/mail/gone.sock",
        ))
        .unwrap();
    book_one
        .register(&entry(&reg, &reused, me, long_ago, "/run/mail/reused.sock"))
        .unwrap();
    book_one
        .register(&entry(&reg, &invalid, me, &started, "relative.sock"))
        .unwrap();
    book_two
        .register(&entry(
            &reg,
            &elsewhere,
            me,
            &started,
            "/run/mail/elsewhere.sock",
        ))
        .unwrap();
    assert_eq!(
        refs(book_one.live_sessions().unwrap()),
        std::slice::from_ref(&live)
    );
    assert_eq!(
        refs(book_two.live_sessions().unwrap()),
        std::slice::from_ref(&elsewhere)
    );
    reg.reload().unwrap();
    assert_eq!(refs(reg.live_sessions(&one)), std::slice::from_ref(&live));
    let listed = book_one.live_sessions().unwrap().remove(0);
    let session = reg.session(&live).unwrap();
    assert_eq!(
        (listed.name.as_str(), listed.harness.as_str(), listed.pid),
        (session.address.as_str(), "codex", me)
    );
    assert_eq!(listed.mailbox, Path::new("/run/mail/live.sock"));
    assert!(session.live_mailbox().is_some());
    for dead in [&gone, &reused, &invalid, &silent] {
        assert!(
            reg.session(dead).unwrap().live_mailbox().is_none(),
            "{dead}"
        );
    }
}

// Expectation: registering needs the session to exist in the book's task under the address and
// harness the entry names, and never takes over an entry another live process holds; once that
// process is gone, its stale entry is replaced. Why: a misconfigured or doubled bridge must not
// steal a session's address. Value: one session, one live mailbox, the right one.
#[test]
fn registering_checks_the_session_and_never_takes_a_live_entry_from_another_process() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let one = reg
        .create_task("One", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let two = reg
        .create_task("Two", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let mine = session_in(&mut reg, &one, "Lead", "codex");
    let foreign = session_in(&mut reg, &two, "Lead", "codex");
    let (me, started) = this_process();
    let book = AddressBook::new(root.path(), &one);
    let mut unknown = entry(&reg, &mine, me, &started, "/run/mail/x.sock");
    unknown.session_ref = "s99".into();
    assert!(book.register(&unknown).is_err(), "no such session");
    assert!(
        book.register(&entry(&reg, &foreign, me, &started, "/run/mail/x.sock"))
            .is_err(),
        "a session of another task"
    );
    let mut renamed = entry(&reg, &mine, me, &started, "/run/mail/x.sock");
    renamed.name = "codex-someone-else".into();
    assert!(
        book.register(&renamed).is_err(),
        "not the session's address"
    );
    let mut reharnessed = entry(&reg, &mine, me, &started, "/run/mail/x.sock");
    reharnessed.harness = "grok".into();
    assert!(
        book.register(&reharnessed).is_err(),
        "not the session's harness"
    );
    let (mut bridge, bridge_pid, bridge_started) = other_bridge();
    book.register(&entry(
        &reg,
        &mine,
        bridge_pid,
        &bridge_started,
        "/run/mail/other.sock",
    ))
    .unwrap();
    assert!(
        book.register(&entry(&reg, &mine, me, &started, "/run/mail/mine.sock"))
            .is_err(),
        "another live process holds the session"
    );
    reg.reload().unwrap();
    assert_eq!(
        reg.session(&mine).unwrap().messaging.as_ref().unwrap().pid,
        bridge_pid
    );
    bridge.kill().unwrap();
    bridge.wait().unwrap();
    book.register(&entry(&reg, &mine, me, &started, "/run/mail/mine.sock"))
        .unwrap();
    reg.reload().unwrap();
    assert_eq!(
        reg.session(&mine).unwrap().messaging.as_ref().unwrap().pid,
        me
    );
}

// Expectation: set_status and unregister change only an entry this process registered. Waiting
// keeps what it waits for and any other status drops it; another process's entry is refused for
// status and left by unregister; unregister removes only the mailbox entry, keeping the session,
// its address, state, tmux runtime and provider identity, and repeating it does nothing; a
// status for an unregistered session is refused rather than re-creating the entry. Why: a stale
// bridge must not overwrite its successor, and a bridge's exit is not the session's end. Value:
// the address book follows bridges without ever losing topology.
#[test]
fn status_and_unregister_change_only_this_process_entry_and_keep_the_session() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task("One", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let mine = session_in(&mut reg, &task, "Lead", "codex");
    let theirs = session_in(&mut reg, &task, "Helper", "grok");
    reg.bind_runtime(&mine, "taskai-test").unwrap();
    let provider = ProviderMapping {
        provider: "codex".into(),
        provider_session_id: "thread-1".into(),
        runtime_id: "runtime-1".into(),
        connector_version: "test".into(),
    };
    reg.bind_provider(&mine, provider.clone()).unwrap();
    reg.set_state(&mine, SessionState::Live).unwrap();
    let (me, started) = this_process();
    let (mut bridge, bridge_pid, bridge_started) = other_bridge();
    let book = AddressBook::new(root.path(), &task);
    book.register(&entry(&reg, &mine, me, &started, "/run/mail/mine.sock"))
        .unwrap();
    book.register(&entry(
        &reg,
        &theirs,
        bridge_pid,
        &bridge_started,
        "/run/mail/theirs.sock",
    ))
    .unwrap();
    let shown = |reg: &mut Registry, r: &str| {
        reg.reload().unwrap();
        reg.session(r)
            .unwrap()
            .messaging
            .as_ref()
            .map(|entry| (entry.status, entry.waiting_for.clone()))
    };
    book.set_status(&mine, Activity::Waiting, Some("approval"))
        .unwrap();
    assert_eq!(
        shown(&mut reg, &mine),
        Some((Activity::Waiting, Some("approval".into())))
    );
    book.set_status(&mine, Activity::Busy, Some("approval"))
        .unwrap();
    assert_eq!(shown(&mut reg, &mine), Some((Activity::Busy, None)));
    assert!(book.set_status(&theirs, Activity::Busy, None).is_err());
    book.unregister(&theirs).unwrap();
    assert_eq!(shown(&mut reg, &theirs), Some((Activity::Idle, None)));
    book.unregister(&mine).unwrap();
    book.unregister(&mine).unwrap();
    reg.reload().unwrap();
    let session = reg.session(&mine).unwrap();
    assert!(session.messaging.is_none());
    assert_eq!(session.name, "Lead");
    assert_eq!(session.address, "codex-lead");
    assert_eq!(session.state, SessionState::Live);
    assert_eq!(session.runtime.as_ref().unwrap().session, mine);
    assert_eq!(session.provider.as_ref(), Some(&provider));
    assert!(
        book.set_status(&mine, Activity::Idle, None).is_err(),
        "an unregistered session is not re-created"
    );
    assert!(shown(&mut reg, &mine).is_none());
    bridge.kill().unwrap();
    bridge.wait().unwrap();
}

// Expectation: the address book fails, rather than listing nobody, when the registry is missing,
// unreadable (here a directory stands where state.json belongs, which no user can read as a file)
// or malformed; registering and clearing fail too, clearing with NotFound for a missing registry
// and Invalid for a malformed one, and neither creates a registry. Why: an empty list would tell an agent that its teammates
// are gone when the registry is merely broken. Value: a broken registry is reported, not hidden.
#[test]
fn a_missing_unreadable_or_malformed_registry_is_an_error_never_an_empty_book() {
    let root = tempfile::tempdir().unwrap();
    let dir = root.path().join("state");
    let book = AddressBook::new(&dir, "t1");
    let (me, started) = this_process();
    let clear = || book.clear_if_matches("s1", me, &started, Path::new("/run/mail/s1.sock"));
    assert!(book.live_sessions().is_err(), "no registry directory");
    assert!(
        matches!(clear(), Err(Error::Io(_))),
        "no registry directory"
    );
    std::fs::create_dir_all(&dir).unwrap();
    assert!(book.live_sessions().is_err(), "no state.json");
    assert!(matches!(clear(), Err(Error::NotFound(_))), "no state.json");
    assert!(
        !dir.join("state.json").exists(),
        "clearing creates no registry"
    );
    std::fs::write(dir.join("state.json"), "{not json").unwrap();
    assert!(book.live_sessions().is_err(), "malformed");
    assert!(matches!(clear(), Err(Error::Invalid(_))), "malformed");
    let stray = MailPeer {
        session_ref: "s1".into(),
        task_ref: "t1".into(),
        name: "codex-lead".into(),
        harness: "codex".into(),
        workspace: "/repo".into(),
        pid: me,
        proc_start: started.clone(),
        status: Activity::Idle,
        waiting_for: None,
        mailbox: "/run/mail/s1.sock".into(),
        features: Vec::new(),
        mode: Some(PermissionMode::Prompting),
    };
    assert!(book.register(&stray).is_err());
    std::fs::remove_file(dir.join("state.json")).unwrap();
    std::fs::create_dir(dir.join("state.json")).unwrap();
    assert!(book.live_sessions().is_err(), "unreadable");
    assert!(matches!(clear(), Err(Error::Io(_))), "unreadable");
}

// Expectation: clear_if_matches clears a session's mailbox entry only when the session is in the
// book's task and its entry is exactly the one recorded for the given pid, start time and mailbox,
// whichever process asks; the session keeps its name, address, state, runtime and provider
// mapping, and native files stay. A wrong task or ref, any one differing identity component, or a
// newer registration that replaced the entry returns false and writes nothing. Why: a native
// Claude session's observer is not the process its entry names, so it cannot use unregister, and
// a replacement registration must survive a stale observer. Value: the observer retracts exactly
// what it published, and nothing else.
#[test]
fn clearing_a_mailbox_entry_needs_its_exact_identity_and_writes_nothing_otherwise() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task("One", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let other_task = reg
        .create_task("Two", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let native = session_in(&mut reg, &task, "Lead", "claude");
    reg.bind_runtime(&native, "taskai-test").unwrap();
    let provider = ProviderMapping {
        provider: "claude".into(),
        provider_session_id: "claude-session-1".into(),
        runtime_id: "runtime-1".into(),
        connector_version: "test".into(),
    };
    reg.bind_provider(&native, provider.clone()).unwrap();
    reg.set_state(&native, SessionState::Live).unwrap();
    let socks = root.path().join("cc-socks");
    std::fs::create_dir(&socks).unwrap();
    let first_socket = socks.join("first.sock");
    let next_socket = socks.join("next.sock");
    std::fs::write(&first_socket, "").unwrap();
    std::fs::write(&next_socket, "").unwrap();
    let book = AddressBook::new(root.path(), &task);
    let (mut first, first_pid, first_started) = other_bridge();
    let first_entry = entry(
        &reg,
        &native,
        first_pid,
        &first_started,
        first_socket.to_str().unwrap(),
    );
    book.register(&first_entry).unwrap();
    let registered = state_as_written(root.path());
    let other_book = AddressBook::new(root.path(), &other_task);
    let misses = [
        (
            other_book.clear_if_matches(&native, first_pid, &first_started, &first_socket),
            "another task's book",
        ),
        (
            book.clear_if_matches("s-none", first_pid, &first_started, &first_socket),
            "an unknown ref",
        ),
        (
            book.clear_if_matches(&native, first_pid + 1, &first_started, &first_socket),
            "another pid",
        ),
        (
            book.clear_if_matches(
                &native,
                first_pid,
                "Thu Jan  1 00:00:00 1970",
                &first_socket,
            ),
            "another start time",
        ),
        (
            book.clear_if_matches(&native, first_pid, &first_started, &next_socket),
            "another mailbox",
        ),
    ];
    for (cleared, case) in misses {
        assert!(!cleared.unwrap(), "{case} clears nothing");
    }
    assert_eq!(
        state_as_written(root.path()),
        registered,
        "nothing was written"
    );
    first.kill().unwrap();
    first.wait().unwrap();
    let (mut next, next_pid, next_started) = other_bridge();
    book.register(&entry(
        &reg,
        &native,
        next_pid,
        &next_started,
        next_socket.to_str().unwrap(),
    ))
    .unwrap();
    let replaced = state_as_written(root.path());
    assert!(
        !book
            .clear_if_matches(&native, first_pid, &first_started, &first_socket)
            .unwrap(),
        "a stale identity leaves the replacement"
    );
    assert_eq!(
        state_as_written(root.path()),
        replaced,
        "nothing was written"
    );
    assert!(book
        .clear_if_matches(&native, next_pid, &next_started, &next_socket)
        .unwrap());
    reg.reload().unwrap();
    let session = reg.session(&native).unwrap();
    assert!(session.messaging.is_none());
    assert_eq!(session.name, "Lead");
    assert_eq!(session.address, "claude-lead");
    assert_eq!(session.state, SessionState::Live);
    assert_eq!(session.runtime.as_ref().unwrap().session, native);
    assert_eq!(session.provider.as_ref(), Some(&provider));
    assert!(
        first_socket.exists() && next_socket.exists(),
        "native files stay"
    );
    assert!(
        !book
            .clear_if_matches(&native, next_pid, &next_started, &next_socket)
            .unwrap(),
        "clearing again finds nothing"
    );
    next.kill().unwrap();
    next.wait().unwrap();
}

// Expectation: a bridge's entry survives the UI's next change even though the UI never saw it,
// and the UI's view shows bridge statuses after reload. Why: the UI holds one long-lived handle;
// its writes must merge with the bridges', and its reads are a snapshot until reloaded. Value: the
// UI can never erase a bridge's mailbox and can show what each agent is doing.
#[test]
fn a_bridge_entry_survives_the_ui_and_reload_shows_its_status() {
    let root = tempfile::tempdir().unwrap();
    let mut ui = open(root.path());
    let task = ui
        .create_task("One", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let lead = session_in(&mut ui, &task, "Lead", "codex");
    let book = AddressBook::new(root.path(), &task);
    let (me, started) = this_process();
    book.register(&entry(&ui, &lead, me, &started, "/run/mail/lead.sock"))
        .unwrap();
    ui.set_state(&lead, SessionState::Live).unwrap();
    assert!(
        open(root.path())
            .session(&lead)
            .unwrap()
            .messaging
            .is_some(),
        "the UI's change kept the bridge's entry"
    );
    book.set_status(&lead, Activity::Busy, None).unwrap();
    let status = |ui: &Registry| {
        ui.session(&lead)
            .unwrap()
            .messaging
            .as_ref()
            .unwrap()
            .status
    };
    assert_eq!(status(&ui), Activity::Idle, "the UI's view is a snapshot");
    ui.reload().unwrap();
    assert_eq!(status(&ui), Activity::Busy);
}

// Expectation: two real mailboxes opened over the registry's address book find each other by
// address, exchange a letter, publish the recipient's busy status into the registry, and on close
// leave the session in place without its mailbox entry. Why: this is the M1 Directory contract
// served by TaskAI's real registry. Value: bridges can be wired with nothing but a registry path.
#[test]
fn mailboxes_resolve_and_publish_through_the_address_book() {
    let root = tempfile::tempdir().unwrap();
    let state = root.path().join("state");
    let mut reg = Registry::open(&state).unwrap();
    let task = reg
        .create_task("One", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let lead = session_in(&mut reg, &task, "Lead", "codex");
    let helper = session_in(&mut reg, &task, "Helper", "grok");
    let open_mailbox = |reg: &Registry, r: &str| {
        let session = reg.session(r).unwrap();
        let (sender, deliveries) = mpsc::channel();
        let mailbox = Mailbox::open(
            MailboxConfig {
                identity: Identity {
                    session_ref: session.session_ref.clone(),
                    task_ref: session.task_ref.clone(),
                    name: session.address.clone(),
                    harness: session.harness.clone(),
                    workspace: session.workspace.clone(),
                    mode: PermissionMode::Prompting,
                },
                mail_dir: root.path().join("run").join("mail"),
                log_path: root.path().join("messages").join("t1.jsonl"),
            },
            Arc::new(AddressBook::new(&state, &task)),
            Arc::new(SocketTransport),
            move |delivery| {
                let _ = sender.send(delivery);
            },
        )
        .unwrap();
        (mailbox, deliveries)
    };
    let (lead_box, _lead_deliveries) = open_mailbox(&reg, &lead);
    let (helper_box, helper_deliveries) = open_mailbox(&reg, &helper);
    let helper_address = reg.session(&helper).unwrap().address.clone();
    let listed: Vec<String> = lead_box
        .list_sessions()
        .unwrap()
        .into_iter()
        .map(|listed| listed.name)
        .collect();
    assert_eq!(listed, std::slice::from_ref(&helper_address));
    assert_eq!(
        lead_box.send(&helper_address, "hello", false, None).status,
        SendStatus::Sent
    );
    let letter = helper_deliveries
        .recv_timeout(Duration::from_secs(3))
        .unwrap();
    assert_eq!(letter.sender, reg.session(&lead).unwrap().address);
    reg.reload().unwrap();
    assert_eq!(
        reg.session(&helper)
            .unwrap()
            .messaging
            .as_ref()
            .unwrap()
            .status,
        Activity::Busy
    );
    helper_box.close();
    reg.reload().unwrap();
    assert!(reg.session(&helper).unwrap().messaging.is_none());
    assert_eq!(refs(reg.live_sessions(&task)), std::slice::from_ref(&lead));
}

const WORKERS: usize = 4;
const ROUNDS: usize = 20;

// Expectation: bridge processes and the UI changing one registry at the same time lose nothing:
// each of four worker processes registers its session's mailbox, then alternates status updates
// with creating sessions, while this process, as the UI, keeps changing a session's state and
// creating sessions of its own. Afterwards every created session exists under a unique ref and
// address, each worker's last status is recorded, and the UI's last state stands. Why: bridges and
// the UI write the same state.json, and any change applied to a stale copy erases the others.
// Value: concurrent agents and the person never undo each other. The workers are copies of this
// test binary, so each is a separate process with its own lock handle.
#[test]
fn simultaneous_processes_keep_every_status_and_topology_change() {
    if let Some(dir) = std::env::var_os("TASKAI_REGISTRY_WORKER_DIR") {
        let var = |name: &str| std::env::var(name).unwrap();
        let (task, session, worker) = (
            var("TASKAI_REGISTRY_WORKER_TASK"),
            var("TASKAI_REGISTRY_WORKER_SESSION"),
            var("TASKAI_REGISTRY_WORKER_INDEX"),
        );
        let dir = Path::new(&dir);
        let mut reg = open(dir);
        let book = AddressBook::new(dir, &task);
        let (me, started) = this_process();
        let mailbox = format!("/run/mail/{session}.sock");
        book.register(&entry(&reg, &session, me, &started, &mailbox))
            .unwrap();
        for round in 0..ROUNDS {
            reg.create_session(
                &task,
                None,
                &format!("worker {worker} round {round}"),
                "codex",
                Path::new("/repo"),
                Source::Session(session.clone()),
            )
            .unwrap();
            let status = [Activity::Busy, Activity::Idle][round % 2];
            book.set_status(&session, status, None).unwrap();
        }
        book.set_status(
            &session,
            Activity::Waiting,
            Some(&format!("worker {worker}")),
        )
        .unwrap();
        return;
    }
    let root = tempfile::tempdir().unwrap();
    let mut ui = open(root.path());
    let task = ui
        .create_task("Busy", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let lead = session_in(&mut ui, &task, "Lead", "claude");
    let workers: Vec<String> = (0..WORKERS)
        .map(|n| session_in(&mut ui, &task, &format!("Worker {n}"), "codex"))
        .collect();
    let children: Vec<Child> = workers
        .iter()
        .enumerate()
        .map(|(n, session)| {
            let mut worker = Command::new(std::env::current_exe().unwrap());
            strip_messaging_env(&mut worker);
            worker
                .args([
                    "--exact",
                    "simultaneous_processes_keep_every_status_and_topology_change",
                    "--nocapture",
                ])
                .env("TASKAI_REGISTRY_WORKER_DIR", root.path())
                .env("TASKAI_REGISTRY_WORKER_TASK", &task)
                .env("TASKAI_REGISTRY_WORKER_SESSION", session)
                .env("TASKAI_REGISTRY_WORKER_INDEX", n.to_string())
                .stdout(Stdio::piped())
                .stderr(Stdio::piped())
                .spawn()
                .unwrap()
        })
        .collect();
    let states = [SessionState::Live, SessionState::Working];
    for round in 0..ROUNDS {
        ui.set_state(&lead, states[round % 2]).unwrap();
        session_in(&mut ui, &task, &format!("ui round {round}"), "grok");
    }
    for child in children {
        let finished = child.wait_with_output().unwrap();
        assert!(
            finished.status.success(),
            "worker failed ({}): {}{}",
            finished.status,
            String::from_utf8_lossy(&finished.stdout),
            String::from_utf8_lossy(&finished.stderr)
        );
    }
    let reg = open(root.path());
    let sessions = reg.sessions_of(&task);
    assert_eq!(sessions.len(), 1 + WORKERS + WORKERS * ROUNDS + ROUNDS);
    let unique_refs: HashSet<&str> = sessions.iter().map(|s| s.session_ref.as_str()).collect();
    let unique_addresses: HashSet<&str> = sessions.iter().map(|s| s.address.as_str()).collect();
    assert_eq!(unique_refs.len(), sessions.len(), "refs are unique");
    assert_eq!(
        unique_addresses.len(),
        sessions.len(),
        "addresses are unique"
    );
    let names: HashSet<&str> = sessions.iter().map(|s| s.name.as_str()).collect();
    for round in 0..ROUNDS {
        assert!(names.contains(format!("ui round {round}").as_str()));
        for n in 0..WORKERS {
            assert!(names.contains(format!("worker {n} round {round}").as_str()));
        }
    }
    for (n, session) in workers.iter().enumerate() {
        let entry = reg.session(session).unwrap().messaging.as_ref();
        let recorded = entry.map(|entry| (entry.status, entry.waiting_for.clone()));
        assert_eq!(
            recorded,
            Some((Activity::Waiting, Some(format!("worker {n}")))),
            "worker {n}'s last status is kept"
        );
    }
    assert_eq!(reg.session(&lead).unwrap().state, states[(ROUNDS - 1) % 2]);
}

// Expectation: collisions stay unique and plain where a cut or a fallback makes two names meet: a
// second session with the same 80-character name gets the base cut short enough for `-<ref>` to
// fit in 64 characters, and a fallback that another session's display name already took moves on
// to `-<ref>`. Why: cutting and falling back are exactly where different names can produce the
// same address. Value: no two sessions of a task share an address, however they are named.
#[test]
fn addresses_stay_unique_and_plain_through_cuts_and_fallbacks() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task("Names", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let long = "x".repeat(80);
    let first = session_in(&mut reg, &task, &long, "codex");
    let second = session_in(&mut reg, &task, &long, "codex");
    let squatter = session_in(&mut reg, &task, "grok s4", "grok");
    let unicode = session_in(&mut reg, &task, "日本語", "grok");
    assert_eq!(
        (
            first.as_str(),
            second.as_str(),
            squatter.as_str(),
            unicode.as_str()
        ),
        ("s1", "s2", "s3", "s4")
    );
    let address = |r: &str| reg.session(r).unwrap().address.clone();
    assert_eq!(address(&first), format!("codex-{}", "x".repeat(58)));
    assert_eq!(address(&second), format!("codex-{}-s2", "x".repeat(55)));
    assert_eq!(address(&squatter), "grok-s4");
    assert_eq!(address(&unicode), "grok-s4-s4");
    for r in [&first, &second, &squatter, &unicode] {
        assert!(
            is_address(&address(r)) && address(r).len() <= 64,
            "{}",
            address(r)
        );
    }
}

// Expectation: when state.json is a dangling symlink (here the real file is moved aside first),
// the address book's lookups and writes fail instead of finding an empty registry, nothing is
// written over the symlink, and opening the registry fails rather than starting a fresh one in
// its place; once the file is back, everything works again. Why: an existence check followed by a
// read that treats "not found" as "new" let a broken registry pass as empty, and a write then
// replaced it. Value: a broken registry is reported, never silently reset.
#[test]
fn a_dangling_state_file_is_an_error_and_is_never_replaced() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task("One", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let lead = session_in(&mut reg, &task, "Lead", "codex");
    let (me, started) = this_process();
    let book = AddressBook::new(root.path(), &task);
    book.register(&entry(&reg, &lead, me, &started, "/run/mail/lead.sock"))
        .unwrap();
    assert_eq!(
        refs(book.live_sessions().unwrap()),
        std::slice::from_ref(&lead)
    );
    let state = root.path().join("state.json");
    let archived = root.path().join("state.json.archived");
    std::fs::rename(&state, &archived).unwrap();
    std::os::unix::fs::symlink(root.path().join("missing.json"), &state).unwrap();
    assert!(book.live_sessions().is_err(), "a dangling state file");
    assert!(book.set_status(&lead, Activity::Busy, None).is_err());
    assert!(book.unregister(&lead).is_err());
    assert!(book
        .register(&entry(&reg, &lead, me, &started, "/run/mail/lead.sock"))
        .is_err());
    assert!(Registry::open(root.path()).is_err());
    assert!(
        std::fs::symlink_metadata(&state)
            .unwrap()
            .file_type()
            .is_symlink(),
        "nothing was written over the dangling state file"
    );
    assert!(!root.path().join("missing.json").exists());
    std::fs::remove_file(&state).unwrap();
    std::fs::rename(&archived, &state).unwrap();
    assert_eq!(
        refs(book.live_sessions().unwrap()),
        std::slice::from_ref(&lead)
    );
}

// Expectation: when state.json vanishes under a handle that already read it, reloading and every
// change fail, the handle keeps its last good view, and no state file is recreated; opening the
// directory afterwards creates a fresh registry, as opening always has. Why: a handle that took a
// missing file for a new registry would reset to empty or save a registry holding only its next
// change. Value: losing the registry is loud, and nothing is overwritten on the way.
#[test]
fn a_vanished_state_file_is_reported_and_the_last_good_view_kept() {
    let root = tempfile::tempdir().unwrap();
    let mut reg = open(root.path());
    let task = reg
        .create_task("One", Path::new("/repo"), Source::HumanUi)
        .unwrap();
    let lead = session_in(&mut reg, &task, "Lead", "codex");
    let state = root.path().join("state.json");
    std::fs::rename(&state, root.path().join("state.json.archived")).unwrap();
    assert!(reg.reload().is_err());
    assert!(reg.session(&lead).is_some(), "the last good view is kept");
    assert!(reg.set_state(&lead, SessionState::Live).is_err());
    assert!(reg
        .create_task("Two", Path::new("/repo"), Source::HumanUi)
        .is_err());
    assert_eq!(reg.tasks().len(), 1);
    assert!(!state.exists(), "no state file was recreated");
    let fresh = open(root.path());
    assert!(fresh.tasks().is_empty(), "opening creates a fresh registry");
    assert!(state.exists());
}
