// ABOUTME: CLI tests — the real taskai binary answering context self/session/tree with
// ABOUTME: process-bound identity, honest errors, and a hook inject that never fails.

use std::path::Path;
use std::process::Command;

use taskai_core::registry::{Registry, Source};

fn taskai() -> Command {
    let mut c = Command::new(env!("CARGO_BIN_EXE_taskai"));
    c.env_remove("TASKAI_SESSION_REF").env_remove("TASKAI_HOME");
    c
}

fn seed(home: &Path) -> (String, String, String) {
    let mut reg = Registry::open(&home.join("state")).unwrap();
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
    (t, lead, arch)
}

// Expectation: `context self` refuses when no registered binding exists — identity is
// process-bound, never assumed. Why: backend §5 binds self to the registered process.
// Value: an unmanaged shell can't impersonate a session.
#[test]
fn context_self_requires_binding() {
    let tmp = tempfile::tempdir().unwrap();
    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "self"])
        .output()
        .unwrap();
    assert!(!out.status.success());
    let err = String::from_utf8_lossy(&out.stderr);
    assert!(err.contains("TASKAI_SESSION_REF"), "{err}");
}

// Expectation: `context self` inside a bound session explains identity; json form
// carries the full snapshot. Why: the session's shell inherits the binding from its
// pane. Value: self-awareness works from any subprocess of the session.
#[test]
fn context_self_and_session_answer() {
    let tmp = tempfile::tempdir().unwrap();
    let (_t, lead, arch) = seed(tmp.path());

    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .env("TASKAI_SESSION_REF", &arch)
        .args(["context", "self"])
        .output()
        .unwrap();
    assert!(out.status.success());
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(text.contains("You are Subordinate Architecture"), "{text}");
    assert!(text.contains(&lead), "{text}");

    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "session", &lead, "--format", "json"])
        .output()
        .unwrap();
    assert!(out.status.success());
    let v: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
    assert_eq!(v["self"]["session_ref"], lead.as_str());
    assert_eq!(v["self"]["role"], "Manager");
}

// Expectation: `context tree` prints the full chain with `└─` subordinate connectors;
// unknown session refs error cleanly. Why: the tree is the whole relevant team chain.
// Value: agents and humans read the same truth.
#[test]
fn context_tree_and_unknown_ref() {
    let tmp = tempfile::tempdir().unwrap();
    seed(tmp.path());

    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "tree"])
        .output()
        .unwrap();
    assert!(out.status.success());
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(text.contains("Repository evaluation"), "{text}");
    assert!(text.contains("└─"), "{text}");
    assert!(text.contains("Architecture"), "{text}");

    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "session", "s999"])
        .output()
        .unwrap();
    assert!(!out.status.success());
}

// Expectation: the inject hook never breaks the hosting CLI — no binding means silent
// success with empty output; a binding emits the additionalContext envelope.
// Why: a failing hook would disturb the native CLI (backend §5: report stale, never
// break). Value: context delivery is safe by construction.
#[test]
fn inject_never_fails() {
    let tmp = tempfile::tempdir().unwrap();
    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["context", "inject"])
        .output()
        .unwrap();
    assert!(out.status.success());
    assert!(out.stdout.is_empty());

    let (_t, lead, _arch) = seed(tmp.path());
    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .env("TASKAI_SESSION_REF", &lead)
        .args(["context", "inject"])
        .output()
        .unwrap();
    assert!(out.status.success());
    let v: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
    assert_eq!(v["hookSpecificOutput"]["hookEventName"], "UserPromptSubmit");
    assert!(v["hookSpecificOutput"]["additionalContext"]
        .as_str()
        .unwrap()
        .contains("You are Manager Lead"));
}
