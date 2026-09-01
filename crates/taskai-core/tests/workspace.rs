// ABOUTME: Workspace provisioning tests against a real git repository — worktree preview
// ABOUTME: matches creation, and destroy preserves the repository and worktrees entirely.

use std::path::{Path, PathBuf};
use std::process::Command;

use taskai_core::registry::{Item, Registry, Source};
use taskai_core::workspace::{preview, provision, WorkspaceChoice};

fn git(dir: &Path, args: &[&str]) -> String {
    let out = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args(args)
        .output()
        .expect("run git");
    assert!(
        out.status.success(),
        "git {args:?} failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout).trim().to_string()
}

fn fixture_repo(root: &Path) -> PathBuf {
    let repo = root.join("buzz");
    std::fs::create_dir(&repo).unwrap();
    git(&repo, &["init", "-q", "-b", "main"]);
    git(&repo, &["config", "user.email", "test@taskai.local"]);
    git(&repo, &["config", "user.name", "TaskAI Test"]);
    std::fs::write(repo.join("README.md"), "buzz\n").unwrap();
    git(&repo, &["add", "README.md"]);
    git(&repo, &["commit", "-q", "-m", "seed"]);
    repo
}

// Expectation: SameRepo resolves to the manager's repo; OtherRepo requires an existing
// directory; the worktree preview path equals the path provision creates. Why: the
// subordinate drawer shows the path before creation — it must never lie. Value: what
// the user confirms is what exists afterward.
#[test]
fn preview_matches_provision() {
    let tmp = tempfile::tempdir().unwrap();
    let repo = fixture_repo(tmp.path());

    assert_eq!(preview(&WorkspaceChoice::SameRepo, &repo), repo);
    assert!(provision(
        &WorkspaceChoice::OtherRepo(tmp.path().join("missing")),
        &repo
    )
    .is_err());

    let choice = WorkspaceChoice::Worktree {
        base: repo.clone(),
        branch: "main".into(),
        name: "verification-pass".into(),
    };
    let previewed = preview(&choice, &repo);
    assert_eq!(
        previewed,
        tmp.path().join("buzz-worktrees/verification-pass")
    );
    let created = provision(&choice, &repo).unwrap();
    assert_eq!(created, previewed);
    assert!(created.join("README.md").exists());
    assert!(git(&repo, &["worktree", "list"]).contains("verification-pass"));
}

// Expectation: destroying a task removes only registry entries — the repository HEAD,
// working tree, and the provisioned worktree survive byte-identically. Why: destroy
// never removes source files, git data, or worktrees (UX vision §6). Value: the red
// key cannot cost work.
#[test]
fn destroy_preserves_repo_and_worktree() {
    let tmp = tempfile::tempdir().unwrap();
    let repo = fixture_repo(tmp.path());
    let wt = provision(
        &WorkspaceChoice::Worktree {
            base: repo.clone(),
            branch: "main".into(),
            name: "auth-pass".into(),
        },
        &repo,
    )
    .unwrap();

    let mut reg = Registry::open(&tmp.path().join("state")).unwrap();
    let t = reg
        .create_task("Repository evaluation", &repo, Source::HumanUi)
        .unwrap();
    let lead = reg
        .create_session(&t, None, "Lead", "claude", &repo, Source::HumanUi)
        .unwrap();
    reg.create_session(
        &t,
        Some(&lead),
        "Architecture",
        "opencode",
        &wt,
        Source::HumanUi,
    )
    .unwrap();

    let head = git(&repo, &["rev-parse", "HEAD"]);
    let status = git(&repo, &["status", "--porcelain"]);
    reg.destroy(&Item::Task(t), Source::HumanUi).unwrap();

    assert_eq!(git(&repo, &["rev-parse", "HEAD"]), head);
    assert_eq!(git(&repo, &["status", "--porcelain"]), status);
    assert!(repo.join("README.md").exists());
    assert!(wt.join("README.md").exists());
    assert!(git(&repo, &["worktree", "list"]).contains("auth-pass"));
}
