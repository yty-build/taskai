// ABOUTME: Registry tests — stable never-recycled refs, topology rules, attribution,
// ABOUTME: atomic persistence across restarts, blast radius, and destroy semantics.

use std::path::Path;

use taskai_core::registry::{Item, Registry, SessionState, Source};

fn open(dir: &Path) -> Registry {
    Registry::open(dir).expect("open registry")
}

// Expectation: refs increment monotonically and are never recycled, even after destroy
// and restart. Why: a recycled ref could route a prompt into the wrong provider session.
// Value: exact session identity is structural, not conventional.
#[test]
fn refs_never_recycled() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = open(tmp.path());
    let t = reg.create_task("Repository evaluation", Path::new("/tmp/buzz"), Source::HumanUi).unwrap();
    let s1 = reg
        .create_session(&t, None, "Lead", "claude", Path::new("/tmp/buzz"), Source::HumanUi)
        .unwrap();
    reg.destroy(&Item::Session(s1.clone()), Source::HumanUi).unwrap();
    let s2 = reg
        .create_session(&t, None, "Lead", "claude", Path::new("/tmp/buzz"), Source::HumanUi)
        .unwrap();
    assert_ne!(s1, s2);

    drop(reg);
    let mut reg = open(tmp.path());
    let s3 = reg
        .create_session(&t, None, "Delivery", "codex", Path::new("/tmp/buzz"), Source::HumanUi)
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
    let t1 = reg.create_task("Repository evaluation", Path::new("/tmp/buzz"), Source::HumanUi).unwrap();
    let t2 = reg.create_task("Production release", Path::new("/tmp/shop"), Source::HumanUi).unwrap();
    let lead = reg
        .create_session(&t1, None, "Lead", "claude", Path::new("/tmp/buzz"), Source::HumanUi)
        .unwrap();
    let arch = reg
        .create_session(&t1, Some(&lead), "Architecture", "opencode", Path::new("/tmp/wt"), Source::HumanUi)
        .unwrap();
    let deliv = reg
        .create_session(&t1, None, "Delivery", "codex", Path::new("/tmp/buzz"), Source::HumanUi)
        .unwrap();

    assert!(reg.session(&lead).unwrap().parent.is_none());
    assert_eq!(reg.session(&arch).unwrap().parent.as_deref(), Some(lead.as_str()));
    let of_t1: Vec<_> = reg.sessions_of(&t1).iter().map(|s| s.session_ref.clone()).collect();
    assert_eq!(of_t1, vec![lead.clone(), arch.clone(), deliv.clone()]);
    let kids: Vec<_> = reg.children_of(&lead).iter().map(|s| s.session_ref.clone()).collect();
    assert_eq!(kids, vec![arch.clone()]);

    assert!(reg.create_session("t999", None, "X", "codex", Path::new("/tmp"), Source::HumanUi).is_err());
    assert!(reg
        .create_session(&t2, Some(&lead), "X", "codex", Path::new("/tmp"), Source::HumanUi)
        .is_err());
    assert!(reg
        .create_session(&t1, Some(&arch), "X", "codex", Path::new("/tmp"), Source::HumanUi)
        .is_err());
}

// Expectation: every mutation records its source — human UI or a registered session.
// Why: backend §2 requires attributing invocations to their origin. Value: the audit
// trail distinguishes what the human did from what an agent did.
#[test]
fn mutations_attributed() {
    let tmp = tempfile::tempdir().unwrap();
    let mut reg = open(tmp.path());
    let t = reg.create_task("Repository evaluation", Path::new("/tmp/buzz"), Source::HumanUi).unwrap();
    let lead = reg
        .create_session(&t, None, "Lead", "claude", Path::new("/tmp/buzz"), Source::HumanUi)
        .unwrap();
    reg.create_session(&t, Some(&lead), "Architecture", "opencode", Path::new("/tmp/wt"), Source::Session(lead.clone()))
        .unwrap();

    assert_eq!(reg.session(&lead).unwrap().created_by, Source::HumanUi);
    let audit = reg.audit();
    assert!(audit.iter().any(|a| a.action == "create_task" && a.source == Source::HumanUi));
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
        t = reg.create_task("Repository evaluation", Path::new("/tmp/buzz"), Source::HumanUi).unwrap();
        lead = reg
            .create_session(&t, None, "Lead", "claude", Path::new("/tmp/buzz"), Source::HumanUi)
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
    let t = reg.create_task("Repository evaluation", Path::new("/tmp/buzz"), Source::HumanUi).unwrap();
    let lead = reg
        .create_session(&t, None, "Lead", "claude", Path::new("/tmp/buzz"), Source::HumanUi)
        .unwrap();
    let arch = reg
        .create_session(&t, Some(&lead), "Architecture", "opencode", Path::new("/tmp/wt"), Source::HumanUi)
        .unwrap();
    let deliv = reg
        .create_session(&t, None, "Delivery", "codex", Path::new("/tmp/buzz"), Source::HumanUi)
        .unwrap();
    let ver = reg
        .create_session(&t, Some(&deliv), "Verification", "grok", Path::new("/tmp/wt2"), Source::HumanUi)
        .unwrap();

    assert_eq!(reg.blast(&Item::Session(arch.clone())).unwrap(), vec![arch.clone()]);
    assert_eq!(
        reg.blast(&Item::Session(lead.clone())).unwrap(),
        vec![lead.clone(), arch.clone()]
    );
    assert_eq!(
        reg.blast(&Item::Task(t.clone())).unwrap(),
        vec![lead.clone(), arch.clone(), deliv.clone(), ver.clone()]
    );

    let removed = reg.destroy(&Item::Session(lead.clone()), Source::HumanUi).unwrap();
    assert_eq!(removed, vec![lead.clone(), arch.clone()]);
    assert!(reg.session(&lead).is_none());
    assert!(reg.session(&arch).is_none());
    assert!(reg.session(&deliv).is_some());

    let removed = reg.destroy(&Item::Task(t.clone()), Source::HumanUi).unwrap();
    assert_eq!(removed, vec![deliv, ver]);
    assert!(reg.task(&t).is_none());
    assert!(reg.tasks().is_empty());
}
