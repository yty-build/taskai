// ABOUTME: Optional helper identity, ordinary registry lifecycle, and atomic creation.
// ABOUTME: Uses isolated JSON only; never launches tmux, providers, or SSH.

use std::path::Path;

use taskai_core::registry::{
    ConnectionTarget, HarnessSettings, HelperSettings, Item, LaunchSettings, ProviderMapping,
    Registry, Role, Source,
};

#[test]
fn helper_is_optional_and_legacy_state_does_not_create_an_agent() {
    let root = tempfile::tempdir().unwrap();
    std::fs::write(
        root.path().join("state.json"),
        r#"{
        "next_task":0,"next_session":0,"tasks":[],"sessions":[],"audit":[]
    }"#,
    )
    .unwrap();
    let registry = Registry::open(root.path()).unwrap();
    assert_eq!(registry.helper(), &HelperSettings::default());
    assert!(registry.tasks().is_empty());
    assert_eq!(registry.hud_counts().queued, 0);
}

#[test]
fn helper_uses_new_exact_refs_not_an_identically_named_user_task() {
    let root = tempfile::tempdir().unwrap();
    let workspace = root.path().join("helper");
    let mut registry = Registry::open(&root.path().join("state")).unwrap();
    let user_task = registry
        .create_task("taskai-helper", &workspace, Source::HumanUi)
        .unwrap();
    let user_manager = registry
        .create_session(
            &user_task,
            None,
            "TaskAI Helper",
            "claude",
            &workspace,
            Source::HumanUi,
        )
        .unwrap();
    let (task, manager, created) = registry
        .ensure_helper("codex", &workspace, Source::HumanUi)
        .unwrap();
    assert!(created);
    assert_ne!(task, user_task);
    assert_ne!(manager, user_manager);
    assert_eq!(registry.tasks().len(), 2);
    assert_eq!(registry.task(&task).unwrap().name, "taskai-helper");
    assert_eq!(
        registry.task(&task).unwrap().connection,
        ConnectionTarget::Local
    );
    assert_eq!(
        registry.task(&task).unwrap().server_profile_id.as_deref(),
        Some("local")
    );
    let helper = registry.session(&manager).unwrap();
    assert_eq!(helper.role, Role::Manager);
    assert!(helper.parent.is_none());
    assert_eq!(helper.workspace, workspace);
    assert_eq!(registry.hud_counts().queued, 2);
    assert_eq!(registry.hud_counts().tasks_active, 2);
    assert_eq!(registry.helper().task_ref.as_deref(), Some(task.as_str()));
    assert_eq!(
        registry.helper().manager_ref.as_deref(),
        Some(manager.as_str())
    );
    assert_eq!(
        registry.helper().preferred_harness.as_deref(),
        Some("codex")
    );
}

#[test]
fn reopening_helper_preserves_native_identity_and_original_launch_snapshot() {
    let root = tempfile::tempdir().unwrap();
    let workspace = root.path().join("helper");
    let mut registry = Registry::open(&root.path().join("state")).unwrap();
    registry
        .set_server_harness(
            "local",
            "claude",
            HarnessSettings {
                enabled: true,
                executable: Some("/opt/qa/claude".into()),
                model: Some("chosen-model".into()),
            },
            Source::HumanUi,
        )
        .unwrap();
    let (task, manager, _) = registry
        .ensure_helper("claude", &workspace, Source::HumanUi)
        .unwrap();
    registry
        .bind_runtime(&manager, "taskai-helper-test")
        .unwrap();
    let provider = ProviderMapping {
        provider: "claude".into(),
        provider_session_id: "exact-session".into(),
        runtime_id: "runtime".into(),
        connector_version: "test".into(),
    };
    registry.bind_provider(&manager, provider.clone()).unwrap();
    registry
        .set_server_harness(
            "local",
            "claude",
            HarnessSettings {
                enabled: false,
                executable: None,
                model: None,
            },
            Source::HumanUi,
        )
        .unwrap();
    let before = std::fs::read(root.path().join("state/state.json")).unwrap();
    drop(registry);
    let mut registry = Registry::open(&root.path().join("state")).unwrap();
    assert_eq!(
        registry
            .ensure_helper("codex", &workspace, Source::HumanUi)
            .unwrap(),
        (task, manager.clone(), false)
    );
    let session = registry.session(&manager).unwrap();
    assert_eq!(session.harness, "claude");
    assert_eq!(
        session.launch,
        LaunchSettings {
            executable: Some("/opt/qa/claude".into()),
            model: Some("chosen-model".into())
        }
    );
    assert_eq!(session.provider.as_ref(), Some(&provider));
    assert_eq!(session.runtime.as_ref().unwrap().session, manager);
    assert_eq!(
        registry.helper().preferred_harness.as_deref(),
        Some("claude")
    );
    assert_eq!(
        std::fs::read(root.path().join("state/state.json")).unwrap(),
        before
    );
}

#[test]
fn disabled_or_invalid_helper_creation_is_atomic_and_does_not_consume_refs() {
    let root = tempfile::tempdir().unwrap();
    let workspace = root.path().join("helper");
    let mut registry = Registry::open(&root.path().join("state")).unwrap();
    registry
        .set_server_harness(
            "local",
            "claude",
            HarnessSettings {
                enabled: false,
                ..HarnessSettings::default()
            },
            Source::HumanUi,
        )
        .unwrap();
    let before = std::fs::read(root.path().join("state/state.json")).unwrap();
    assert!(registry
        .ensure_helper("claude", &workspace, Source::HumanUi)
        .is_err());
    assert!(registry
        .ensure_helper("terminal", &workspace, Source::HumanUi)
        .is_err());
    assert!(registry
        .ensure_helper("codex", Path::new("relative"), Source::HumanUi)
        .is_err());
    assert_eq!(registry.helper(), &HelperSettings::default());
    assert!(registry.tasks().is_empty());
    assert_eq!(
        std::fs::read(root.path().join("state/state.json")).unwrap(),
        before
    );
    assert_eq!(
        registry
            .ensure_helper("codex", &workspace, Source::HumanUi)
            .unwrap(),
        ("t1".into(), "s1".into(), true)
    );
}

#[test]
fn failed_persistence_leaves_helper_and_registry_unchanged() {
    let root = tempfile::tempdir().unwrap();
    let state = root.path().join("state");
    let mut registry = Registry::open(&state).unwrap();
    let before = std::fs::read(state.join("state.json")).unwrap();
    // A directory where this process stages its save fails the save, as a full disk would:
    // unlinking a directory is refused even to root.
    let staging = state.join(format!(".state.json.{}.tmp", std::process::id()));
    std::fs::create_dir(&staging).unwrap();
    assert!(registry
        .ensure_helper("codex", root.path(), Source::HumanUi)
        .is_err());
    assert!(registry.tasks().is_empty());
    assert!(registry.audit().is_empty());
    assert_eq!(registry.helper(), &HelperSettings::default());
    assert_eq!(std::fs::read(state.join("state.json")).unwrap(), before);
    std::fs::remove_dir(&staging).unwrap();
    assert_eq!(
        registry
            .ensure_helper("codex", root.path(), Source::HumanUi)
            .unwrap(),
        ("t1".into(), "s1".into(), true)
    );
}

#[test]
fn normal_destroy_clears_only_helper_refs_and_recreation_never_recycles_them() {
    let root = tempfile::tempdir().unwrap();
    let mut registry = Registry::open(&root.path().join("state")).unwrap();
    let (task, manager, _) = registry
        .ensure_helper("claude", root.path(), Source::HumanUi)
        .unwrap();
    registry
        .destroy(&Item::Session(manager.clone()), Source::HumanUi)
        .unwrap();
    assert_eq!(registry.helper().task_ref.as_deref(), Some(task.as_str()));
    assert!(registry.helper().manager_ref.is_none());
    assert_eq!(
        registry.helper().preferred_harness.as_deref(),
        Some("claude")
    );
    let (same_task, new_manager, created) = registry
        .ensure_helper("codex", root.path(), Source::HumanUi)
        .unwrap();
    assert!(created);
    assert_eq!(same_task, task);
    assert_ne!(new_manager, manager);
    registry
        .destroy(&Item::Task(task.clone()), Source::HumanUi)
        .unwrap();
    assert_eq!(
        registry.helper(),
        &HelperSettings {
            preferred_harness: Some("codex".into()),
            ..HelperSettings::default()
        }
    );
    let (new_task, next_manager, _) = registry
        .ensure_helper("codex", root.path(), Source::HumanUi)
        .unwrap();
    assert_ne!(new_task, task);
    assert_ne!(next_manager, new_manager);
}

#[test]
fn stale_helper_refs_heal_without_adopting_an_unrelated_manager() {
    let root = tempfile::tempdir().unwrap();
    let state = root.path().join("state");
    let mut registry = Registry::open(&state).unwrap();
    let (helper_task, helper_manager, _) = registry
        .ensure_helper("claude", root.path(), Source::HumanUi)
        .unwrap();
    let user_task = registry
        .create_task("User project", root.path(), Source::HumanUi)
        .unwrap();
    let user_manager = registry
        .create_session(
            &user_task,
            None,
            "Lead",
            "codex",
            root.path(),
            Source::HumanUi,
        )
        .unwrap();
    registry
        .destroy(&Item::Session(helper_manager), Source::HumanUi)
        .unwrap();
    let file = state.join("state.json");
    let mut json: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&file).unwrap()).unwrap();
    json["helper"]["manager_ref"] = user_manager.clone().into();
    std::fs::write(&file, serde_json::to_vec_pretty(&json).unwrap()).unwrap();
    let mut registry = Registry::open(&state).unwrap();
    assert!(registry.helper().manager_ref.is_none());
    let (same_task, new_manager, _) = registry
        .ensure_helper("claude", root.path(), Source::HumanUi)
        .unwrap();
    assert_eq!(same_task, helper_task);
    assert_ne!(new_manager, user_manager);
    assert_eq!(registry.session(&user_manager).unwrap().task_ref, user_task);
    assert!(registry
        .ensure_helper("claude", &root.path().join("other"), Source::HumanUi)
        .is_err());
    assert_eq!(registry.task(&same_task).unwrap().repo, root.path());
}
