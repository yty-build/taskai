// ABOUTME: Server profile migration, immutable targets, new-session defaults, and
// ABOUTME: durable non-agent setup mappings. These tests never launch providers/SSH.

use std::path::{Path, PathBuf};

use serde_json::{json, Value};
use taskai_core::registry::{
    ConnectionTarget, HarnessSettings, Item, LaunchSettings, ProviderMapping, Registry,
    SessionState, Source,
};

fn ssh(host: &str) -> ConnectionTarget {
    ConnectionTarget::ssh(host).unwrap()
}

fn settings(executable: &str, model: &str) -> HarnessSettings {
    HarnessSettings {
        enabled: true,
        executable: Some(executable.into()),
        model: Some(model.into()),
    }
}

fn read_state(root: &Path) -> Value {
    serde_json::from_str(&std::fs::read_to_string(root.join("state.json")).unwrap()).unwrap()
}

#[test]
fn legacy_migration_deduplicates_hosts_and_preserves_all_existing_identity() {
    let root = tempfile::tempdir().unwrap();
    let original = json!({
        "next_task": 3, "next_session": 1,
        "tasks": [
            {"task_ref":"t1","name":"Local","repo":"/local/repo","created_by":"HumanUi"},
            {"task_ref":"t2","name":"Remote one","repo":"/srv/one","connection":{"kind":"ssh","host":"cruise4b"},"created_by":"HumanUi",
             "terminal":{"target":{"kind":"ssh","host":"cruise4b"},"private_server":"taskai-preserved","session":"t2-terminal"}},
            {"task_ref":"t3","name":"Remote two","repo":"/srv/two","connection":{"kind":"ssh","host":"cruise4b"},"created_by":"HumanUi"}
        ],
        "sessions": [{
            "session_ref":"s1","task_ref":"t2","parent":null,"role":"Manager","name":"Lead","harness":"claude","workspace":"/srv/one","state":"Live","created_by":"HumanUi",
            "provider":{"provider":"claude","provider_session_id":"native-exact-id","runtime_id":"native-runtime-id","connector_version":"2.1.0"},
            "runtime":{"target":{"kind":"ssh","host":"cruise4b"},"private_server":"taskai-preserved","session":"s1"}
        }],
        "audit":[]
    });
    std::fs::write(
        root.path().join("state.json"),
        serde_json::to_string_pretty(&original).unwrap(),
    )
    .unwrap();
    let mut registry = Registry::open(root.path()).unwrap();
    assert_eq!(registry.servers().len(), 2);
    assert_eq!(registry.server("local").unwrap().name, "This computer");
    assert_eq!(
        registry.task("t1").unwrap().server_profile_id.as_deref(),
        Some("local")
    );
    let profile = registry
        .task("t2")
        .unwrap()
        .server_profile_id
        .clone()
        .unwrap();
    assert_eq!(
        registry.task("t3").unwrap().server_profile_id.as_deref(),
        Some(profile.as_str())
    );
    assert_eq!(registry.server(&profile).unwrap().target, ssh("cruise4b"));
    assert_eq!(
        registry.session("s1").unwrap().launch,
        LaunchSettings::default()
    );
    assert_eq!(registry.session("s1").unwrap().state, SessionState::Live);
    assert!(
        registry.audit().is_empty(),
        "migration is not a new user action"
    );
    let migrated = read_state(root.path());
    for key in [
        "provider",
        "runtime",
        "session_ref",
        "task_ref",
        "workspace",
        "state",
    ] {
        assert_eq!(migrated["sessions"][0][key], original["sessions"][0][key]);
    }
    assert_eq!(
        migrated["tasks"][1]["terminal"],
        original["tasks"][1]["terminal"]
    );
    assert_eq!(
        migrated["tasks"][1]["connection"],
        original["tasks"][1]["connection"]
    );
    let once = std::fs::read(root.path().join("state.json")).unwrap();
    drop(Registry::open(root.path()).unwrap());
    assert_eq!(
        std::fs::read(root.path().join("state.json")).unwrap(),
        once,
        "reopen must not rewrite migrated state"
    );
    registry
        .set_server_harness(
            &profile,
            "claude",
            settings("/opt/claude", "new-model"),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(
        registry.session("s1").unwrap().launch,
        LaunchSettings::default(),
        "legacy sessions never inherit new defaults"
    );
    let child = registry
        .create_session(
            "t2",
            Some("s1"),
            "Child",
            "claude",
            Path::new("/srv/one"),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(child, "s2");
    assert_eq!(
        registry.session(&child).unwrap().launch.model.as_deref(),
        Some("new-model")
    );
}

#[test]
fn defaults_are_native_and_new_sessions_snapshot_only_their_own_server() {
    let root = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(root.path()).unwrap();
    let defaults: HarnessSettings = serde_json::from_value(json!({})).unwrap();
    assert_eq!(defaults, HarnessSettings::default());
    assert!(defaults.enabled);
    let remote = registry
        .create_server(
            " Remote ",
            ssh("cruise4b"),
            Some("~/github".into()),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(registry.server(&remote).unwrap().name, "Remote");
    registry
        .set_server_harness(
            &remote,
            "claude",
            settings("~/bin/claude", "sonnet"),
            Source::HumanUi,
        )
        .unwrap();
    let local_task = registry
        .create_task("Local", Path::new("/local"), Source::HumanUi)
        .unwrap();
    let remote_task = registry
        .create_task_on_server(
            "Remote",
            Path::new("/srv/project"),
            &remote,
            Source::HumanUi,
        )
        .unwrap();
    let local_session = registry
        .create_session(
            &local_task,
            None,
            "Lead",
            "claude",
            Path::new("/local"),
            Source::HumanUi,
        )
        .unwrap();
    let remote_session = registry
        .create_session(
            &remote_task,
            None,
            "Lead",
            "claude",
            Path::new("/srv/project"),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(
        registry.session(&local_session).unwrap().launch,
        LaunchSettings::default(),
        "local legacy binary override must remain possible via None"
    );
    let saved = registry.session(&remote_session).unwrap().launch.clone();
    assert_eq!(
        saved,
        LaunchSettings {
            executable: Some("~/bin/claude".into()),
            model: Some("sonnet".into())
        }
    );
    registry
        .set_server_harness(
            &remote,
            "claude",
            settings("/new/claude", "opus"),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(registry.session(&remote_session).unwrap().launch, saved);
    let new_session = registry
        .create_session(
            &remote_task,
            None,
            "Another",
            "claude",
            Path::new("/srv/project"),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(
        registry
            .session(&new_session)
            .unwrap()
            .launch
            .model
            .as_deref(),
        Some("opus")
    );
    registry
        .set_server_harness(
            &remote,
            "claude",
            HarnessSettings {
                enabled: false,
                ..HarnessSettings::default()
            },
            Source::HumanUi,
        )
        .unwrap();
    let before = registry.sessions_of(&remote_task).len();
    assert!(registry
        .create_session(
            &remote_task,
            None,
            "Blocked",
            "claude",
            Path::new("/srv/project"),
            Source::HumanUi
        )
        .unwrap_err()
        .to_string()
        .contains("disabled"));
    assert_eq!(registry.sessions_of(&remote_task).len(), before);
    assert_eq!(registry.session(&remote_session).unwrap().launch, saved);
    drop(registry);
    assert_eq!(
        Registry::open(root.path())
            .unwrap()
            .session(&remote_session)
            .unwrap()
            .launch,
        saved
    );
}

#[test]
fn archive_and_profile_edits_never_retarget_or_destroy_existing_work() {
    let root = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(root.path()).unwrap();
    let profile = registry
        .create_server("Cruise", ssh("cruise4b"), None, Source::HumanUi)
        .unwrap();
    let task = registry
        .create_task_on_server(
            "Project",
            Path::new("/srv/project"),
            &profile,
            Source::HumanUi,
        )
        .unwrap();
    let session = registry
        .create_session(
            &task,
            None,
            "Lead",
            "codex",
            Path::new("/srv/project"),
            Source::HumanUi,
        )
        .unwrap();
    registry.bind_runtime(&session, "taskai-existing").unwrap();
    registry
        .bind_provider(
            &session,
            ProviderMapping {
                provider: "codex".into(),
                provider_session_id: "preserved-provider-id".into(),
                runtime_id: "runtime-id".into(),
                connector_version: "1.0.0".into(),
            },
        )
        .unwrap();
    registry.set_state(&session, SessionState::Live).unwrap();
    let runtime = registry.session(&session).unwrap().runtime.clone();
    let provider = registry.session(&session).unwrap().provider.clone();
    let terminal = registry.bind_terminal(&task, "taskai-existing").unwrap();
    let setup = registry
        .bind_server_setup(&profile, "terminal", "taskai-existing")
        .unwrap();
    let counts = registry.hud_counts();
    registry
        .update_server(
            &profile,
            "Renamed",
            Some("/different/browser/root".into()),
            Source::HumanUi,
        )
        .unwrap();
    registry.archive_server(&profile, Source::HumanUi).unwrap();
    let revision = registry.revision();
    registry.archive_server(&profile, Source::HumanUi).unwrap();
    assert_eq!(registry.revision(), revision, "archive is idempotent");
    assert_eq!(registry.hud_counts(), counts);
    assert_eq!(registry.session(&session).unwrap().runtime, runtime);
    assert_eq!(registry.session(&session).unwrap().provider, provider);
    assert_eq!(
        registry.session(&session).unwrap().state,
        SessionState::Live
    );
    assert_eq!(
        registry.task(&task).unwrap().repo,
        PathBuf::from("/srv/project")
    );
    assert_eq!(registry.task(&task).unwrap().connection, ssh("cruise4b"));
    assert_eq!(
        registry.task(&task).unwrap().terminal.as_ref(),
        Some(&terminal)
    );
    assert_eq!(
        registry.server(&profile).unwrap().setup_runtimes["terminal"],
        setup
    );
    assert!(registry
        .create_task_on_server("Blocked", Path::new("/other"), &profile, Source::HumanUi)
        .is_err());
    assert!(registry
        .create_session(
            &task,
            Some(&session),
            "Child",
            "codex",
            Path::new("/srv/project"),
            Source::HumanUi
        )
        .is_ok());
    registry
        .destroy(&Item::Task(task), Source::HumanUi)
        .unwrap();
    assert_eq!(
        registry.server(&profile).unwrap().setup_runtimes["terminal"],
        setup,
        "task destroy cannot own a server's setup utility"
    );
}

#[test]
fn setup_utilities_are_stable_non_agent_mappings() {
    let root = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(root.path()).unwrap();
    let counts = registry.hud_counts();
    for key in ["terminal", "claude", "codex", "opencode", "grok"] {
        let runtime = registry
            .bind_server_setup("local", key, "taskai-setup")
            .unwrap();
        assert_eq!(runtime.session, format!("setup-local-{key}"));
        assert_eq!(runtime.target, ConnectionTarget::Local);
        assert_eq!(
            registry
                .bind_server_setup("local", key, "ignored-new-server")
                .unwrap(),
            runtime
        );
    }
    assert_eq!(registry.hud_counts(), counts);
    assert!(registry.tasks().is_empty());
    assert_eq!(registry.server("local").unwrap().setup_runtimes.len(), 5);
    assert!(registry
        .bind_server_setup("local", "../escape", "taskai-setup")
        .is_err());
    assert!(registry
        .bind_server_setup("local", "terminal", "bad;server")
        .is_err());
    assert!(registry
        .bind_server_setup("missing", "terminal", "taskai-setup")
        .is_err());
    assert!(registry.archive_server("local", Source::HumanUi).is_err());
    drop(registry);
    assert_eq!(
        Registry::open(root.path())
            .unwrap()
            .server("local")
            .unwrap()
            .setup_runtimes
            .len(),
        5
    );
}

#[test]
fn invalid_profile_settings_fail_before_persistence_or_id_consumption() {
    let root = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(root.path()).unwrap();
    let before = read_state(root.path());
    for name in [
        "",
        "  ",
        "bad\nname",
        "\nleading",
        "trailing\n",
        "bad\u{1b}name",
    ] {
        assert!(registry
            .create_server(name, ssh("cruise4b"), None, Source::HumanUi)
            .is_err());
    }
    for path in [
        "relative/path",
        "",
        "/bad\npath",
        " /padding",
        "~/bad\tpath",
    ] {
        assert!(registry
            .create_server(
                "Remote",
                ssh("cruise4b"),
                Some(path.into()),
                Source::HumanUi
            )
            .is_err());
    }
    assert_eq!(read_state(root.path()), before);
    let profile = registry
        .create_server(
            "Remote",
            ssh("cruise4b"),
            Some("/home/my projects".into()),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(profile, "server-1");
    for executable in [
        "",
        "claude --model opus",
        "../claude",
        "bin/claude",
        "claude;echo",
        "-claude",
        "~/bin/claude\n",
    ] {
        assert!(
            registry
                .set_server_harness(
                    &profile,
                    "claude",
                    HarnessSettings {
                        executable: Some(executable.into()),
                        ..HarnessSettings::default()
                    },
                    Source::HumanUi
                )
                .is_err(),
            "accepted executable {executable:?}"
        );
    }
    for model in ["", " ", "-option", "model\nnext", "model\u{0}"] {
        assert!(registry
            .set_server_harness(
                &profile,
                "claude",
                HarnessSettings {
                    model: Some(model.into()),
                    ..HarnessSettings::default()
                },
                Source::HumanUi
            )
            .is_err());
    }
    assert!(registry
        .set_server_harness(
            &profile,
            "unknown",
            HarnessSettings::default(),
            Source::HumanUi
        )
        .is_err());
    registry
        .set_server_harness(
            &profile,
            "claude",
            settings("/Applications/Provider Tools/claude", "vendor/model"),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(registry.server(&profile).unwrap().harnesses.len(), 1);
}

#[test]
fn profile_ids_are_not_recycled_and_existing_tasks_stay_with_archived_profile() {
    let root = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(root.path()).unwrap();
    let old = registry
        .create_server("Old", ssh("cruise4b"), None, Source::HumanUi)
        .unwrap();
    let task = registry
        .create_task_on(
            "Old task",
            Path::new("/repo"),
            ssh("cruise4b"),
            Source::HumanUi,
        )
        .unwrap();
    assert!(registry
        .create_server("Duplicate", ssh("cruise4b"), None, Source::HumanUi)
        .is_err());
    registry.archive_server(&old, Source::HumanUi).unwrap();
    let new = registry
        .create_server("New", ssh("cruise4b"), None, Source::HumanUi)
        .unwrap();
    assert_eq!(old, "server-1");
    assert_eq!(new, "server-2");
    assert_eq!(
        registry.task(&task).unwrap().server_profile_id.as_deref(),
        Some(old.as_str())
    );
    let new_task = registry
        .create_task_on(
            "New task",
            Path::new("/repo"),
            ssh("cruise4b"),
            Source::HumanUi,
        )
        .unwrap();
    assert_eq!(
        registry
            .task(&new_task)
            .unwrap()
            .server_profile_id
            .as_deref(),
        Some(new.as_str())
    );
    drop(registry);
    let mut registry = Registry::open(root.path()).unwrap();
    assert_eq!(
        registry
            .create_server("Other", ssh("cruise2"), None, Source::HumanUi)
            .unwrap(),
        "server-3"
    );
}

#[test]
fn malformed_saved_targets_and_setup_ownership_are_rejected_without_rewrite() {
    let root = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(root.path()).unwrap();
    let profile = registry
        .create_server("Remote", ssh("cruise4b"), None, Source::HumanUi)
        .unwrap();
    registry
        .create_task_on_server("Task", Path::new("/repo"), &profile, Source::HumanUi)
        .unwrap();
    registry
        .bind_server_setup(&profile, "claude", "taskai-setup")
        .unwrap();
    let original = read_state(root.path());
    let state_path = root.path().join("state.json");
    for corruption in 0..4 {
        let mut value = original.clone();
        match corruption {
            0 => value["servers"][1]["target"] = json!({"kind":"ssh","host":"other-host"}),
            1 => value["servers"][1]["setup_runtimes"]["claude"]["session"] = json!("s1"),
            2 => value["servers"][0]["archived"] = json!(true),
            _ => value["tasks"][0]["server_profile_id"] = json!("missing"),
        }
        let body = serde_json::to_string_pretty(&value).unwrap();
        std::fs::write(&state_path, &body).unwrap();
        assert!(Registry::open(root.path()).is_err());
        assert_eq!(std::fs::read_to_string(&state_path).unwrap(), body);
    }
}

#[test]
fn profile_counter_recovery_uses_the_highest_persisted_id() {
    let root = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(root.path()).unwrap();
    registry
        .create_server("Existing", ssh("cruise4b"), None, Source::HumanUi)
        .unwrap();
    let mut value = read_state(root.path());
    value["next_server"] = json!(0);
    value["servers"][1]["server_id"] = json!("server-17");
    std::fs::write(
        root.path().join("state.json"),
        serde_json::to_string_pretty(&value).unwrap(),
    )
    .unwrap();
    drop(registry);
    let mut registry = Registry::open(root.path()).unwrap();
    assert_eq!(
        registry
            .create_server("Next", ssh("cruise2"), None, Source::HumanUi)
            .unwrap(),
        "server-18"
    );
}
