// ABOUTME: Release-gate harness — a clean TASKAI_HOME needs only the taskai binary:
// ABOUTME: frames dump, context answers, and runtime directories are owner-only.

use std::path::Path;
use std::process::Command;

fn taskai() -> Command {
    let mut c = Command::new(env!("CARGO_BIN_EXE_taskai"));
    c.env_remove("TASKAI_SESSION_REF").env_remove("TASKAI_HOME");
    c
}

// Expectation: with a fresh home, the binary renders frames and answers context with no
// other dependencies. Why: a clean install requires only TaskAI plus provider CLIs
// (backend §11). Value: the install story is provable.
#[test]
fn clean_home_works_offline() {
    let tmp = tempfile::tempdir().unwrap();
    let out = taskai()
        .env("TASKAI_HOME", tmp.path())
        .args(["--dump-frame", "home", "--cols", "110", "--rows", "30"])
        .output()
        .unwrap();
    assert!(out.status.success());
    assert!(!out.stdout.is_empty());

    let out = taskai().env("TASKAI_HOME", tmp.path()).args(["context", "tree"]).output().unwrap();
    assert!(out.status.success());
    assert!(String::from_utf8_lossy(&out.stdout).contains("No tasks registered"));
}

// Expectation: the registry directory is created owner-only. Why: backend §9 — local
// runtime dirs are 0700. Value: no other user can read session topology.
#[test]
fn runtime_dirs_are_owner_only() {
    let tmp = tempfile::tempdir().unwrap();
    let out = taskai().env("TASKAI_HOME", tmp.path()).args(["context", "tree"]).output().unwrap();
    assert!(out.status.success());
    let state = tmp.path().join("state");
    assert!(state.is_dir());
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(&state).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o700, "state dir mode {mode:o}");
    }
    let _ = Path::new("noop");
}
