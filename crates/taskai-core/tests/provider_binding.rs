// ABOUTME: Proves that concurrent launchers choose one durable native provider identity.
// ABOUTME: Uses private registries and child test processes, never a provider or native state.

use std::{
    fs,
    os::unix::fs::MetadataExt,
    path::Path,
    process::{Child, Command, Stdio},
    time::{Duration, Instant},
};
use taskai_core::{
    mail::strip_messaging_env,
    registry::{ConnectionTarget, ProviderMapping, Registry, Source},
    Error,
};

fn fixture(root: &Path) -> (Registry, String) {
    let mut registry = Registry::open(root).unwrap();
    let task = registry.create_task("Task", root, Source::HumanUi).unwrap();
    let session = registry
        .create_session(&task, None, "Grok", "grok", root, Source::HumanUi)
        .unwrap();
    registry
        .bind_runtime(&session, "taskai-binding-test")
        .unwrap();
    (registry, session)
}

fn proposed(session: &str, id: &str) -> ProviderMapping {
    ProviderMapping {
        provider: "grok".into(),
        provider_session_id: id.into(),
        runtime_id: format!("tmux:taskai-binding-test:{session}"),
        connector_version: "1.0.77".into(),
    }
}

fn snapshot(root: &Path) -> (Vec<u8>, u64) {
    let path = root.join("state.json");
    (fs::read(&path).unwrap(), fs::metadata(path).unwrap().ino())
}

// A second stale launcher must use the first saved ID and leave the file untouched.
#[test]
fn first_provider_binding_wins_across_stale_handles() {
    let root = tempfile::tempdir().unwrap();
    let (mut first, session) = fixture(root.path());
    let mut stale = Registry::open(root.path()).unwrap();
    let winner = proposed(&session, "native-first");
    assert_eq!(
        first
            .bind_provider_if_absent(&session, winner.clone())
            .unwrap(),
        winner
    );
    let before = snapshot(root.path());
    let mut other = proposed(&session, "native-second");
    other.connector_version = "later-connector".into();
    assert_eq!(
        stale.bind_provider_if_absent(&session, other).unwrap(),
        winner
    );
    assert_eq!(
        stale.session(&session).unwrap().provider.as_ref(),
        Some(&winner)
    );
    assert_eq!(
        snapshot(root.path()),
        before,
        "returning a winner must not write"
    );
}

// Foreign or incomplete candidates and stored mappings must never become launch authority.
#[test]
fn invalid_provider_bindings_fail_without_writing() {
    let root = tempfile::tempdir().unwrap();
    let (mut registry, session) = fixture(root.path());
    let valid = proposed(&session, "native-owned");
    for field in ["provider", "runtime", "id", "version"] {
        let mut bad = valid.clone();
        match field {
            "provider" => bad.provider = "claude".into(),
            "runtime" => bad.runtime_id = "tmux:foreign:s2".into(),
            "id" => bad.provider_session_id.clear(),
            "version" => bad.connector_version.clear(),
            _ => unreachable!(),
        }
        let before = snapshot(root.path());
        assert!(
            matches!(
                registry.bind_provider_if_absent(&session, bad.clone()),
                Err(Error::Invalid(_))
            ),
            "bad candidate {field}"
        );
        assert_eq!(snapshot(root.path()), before);
        assert!(registry.session(&session).unwrap().provider.is_none());

        registry.bind_provider(&session, bad).unwrap();
        let before = snapshot(root.path());
        assert!(
            matches!(
                registry.bind_provider_if_absent(&session, valid.clone()),
                Err(Error::Invalid(_))
            ),
            "bad existing {field}"
        );
        assert_eq!(snapshot(root.path()), before);
        registry.clear_provider(&session).unwrap();
    }
    let before = snapshot(root.path());
    assert!(matches!(
        registry.bind_provider_if_absent("s999", valid),
        Err(Error::NotFound(_))
    ));
    assert_eq!(snapshot(root.path()), before);
}

// Runtime identity is re-read under the same lock as the choice, not trusted from a launch copy.
#[test]
fn binding_rechecks_the_current_local_runtime() {
    let root = tempfile::tempdir().unwrap();
    let (mut updater, session) = fixture(root.path());
    let mut stale = Registry::open(root.path()).unwrap();
    let candidate = proposed(&session, "native-owned");
    updater.bind_runtime(&session, "taskai-replaced").unwrap();
    let before = snapshot(root.path());
    assert!(stale
        .bind_provider_if_absent(&session, candidate.clone())
        .is_err());
    assert_eq!(snapshot(root.path()), before);
    updater.clear_runtime(&session).unwrap();
    let before = snapshot(root.path());
    assert!(stale.bind_provider_if_absent(&session, candidate).is_err());
    assert_eq!(snapshot(root.path()), before);

    let task = updater
        .create_task_on(
            "Remote",
            root.path(),
            ConnectionTarget::ssh("fixture-host").unwrap(),
            Source::HumanUi,
        )
        .unwrap();
    let remote = updater
        .create_session(&task, None, "Remote", "grok", root.path(), Source::HumanUi)
        .unwrap();
    updater
        .bind_runtime(&remote, "taskai-binding-test")
        .unwrap();
    let before = snapshot(root.path());
    assert!(stale
        .bind_provider_if_absent(&remote, proposed(&remote, "remote-native"))
        .is_err());
    assert_eq!(snapshot(root.path()), before);
}

struct Children(Vec<Child>);

// Failed persistence must not return a usable ID or leave a binding in the caller's view.
#[test]
fn failed_binding_persistence_leaves_no_candidate() {
    let root = tempfile::tempdir().unwrap();
    let (mut registry, session) = fixture(root.path());
    let before = snapshot(root.path());
    let staging = root
        .path()
        .join(format!(".state.json.{}.tmp", std::process::id()));
    fs::create_dir(&staging).unwrap();
    assert!(matches!(
        registry.bind_provider_if_absent(&session, proposed(&session, "unsaved")),
        Err(Error::Io(_))
    ));
    assert!(registry.session(&session).unwrap().provider.is_none());
    assert_eq!(snapshot(root.path()), before);
}

impl Drop for Children {
    fn drop(&mut self) {
        for child in &mut self.0 {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

fn wait_for(mut condition: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !condition() {
        assert!(
            Instant::now() < deadline,
            "private process barrier timed out"
        );
        std::thread::sleep(Duration::from_millis(10));
    }
}

// Separate processes begin with the same absent mapping; both must receive the same committed ID.
#[test]
fn provider_binding_is_one_decision_across_processes() {
    if let Some(root) = std::env::var_os("TASKAI_BIND_TEST_ROOT") {
        let root = Path::new(&root);
        let index = std::env::var("TASKAI_BIND_TEST_INDEX").unwrap();
        let session = std::env::var("TASKAI_BIND_TEST_SESSION").unwrap();
        let mut registry = Registry::open(root).unwrap();
        assert!(registry.session(&session).unwrap().provider.is_none());
        fs::write(root.join(format!("ready-{index}")), "ready").unwrap();
        wait_for(|| root.join("go").exists());
        let winner = registry
            .bind_provider_if_absent(&session, proposed(&session, &format!("native-{index}")))
            .unwrap();
        fs::write(
            root.join(format!("result-{index}")),
            winner.provider_session_id,
        )
        .unwrap();
        return;
    }
    let root = tempfile::tempdir().unwrap();
    let (_, session) = fixture(root.path());
    let mut children = Children(Vec::new());
    for index in ["1", "2"] {
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args([
                "--exact",
                "provider_binding_is_one_decision_across_processes",
            ])
            .env("TASKAI_BIND_TEST_ROOT", root.path())
            .env("TASKAI_BIND_TEST_SESSION", &session)
            .env("TASKAI_BIND_TEST_INDEX", index)
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        strip_messaging_env(&mut command);
        children.0.push(command.spawn().unwrap());
    }
    wait_for(|| root.path().join("ready-1").exists() && root.path().join("ready-2").exists());
    fs::write(root.path().join("go"), "go").unwrap();
    wait_for(|| {
        children.0.iter_mut().all(|child| {
            child.try_wait().unwrap().is_some_and(|status| {
                assert!(status.success(), "private binding child failed: {status}");
                true
            })
        })
    });
    let first = fs::read_to_string(root.path().join("result-1")).unwrap();
    let second = fs::read_to_string(root.path().join("result-2")).unwrap();
    assert_eq!(first, second, "launchers selected different native IDs");
    assert!(matches!(first.as_str(), "native-1" | "native-2"));
    assert_eq!(
        Registry::open(root.path())
            .unwrap()
            .session(&session)
            .unwrap()
            .provider
            .as_ref()
            .unwrap()
            .provider_session_id,
        first
    );
}
