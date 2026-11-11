// ABOUTME: Workspace provisioning tests against a real git repository — worktree preview
// ABOUTME: matches creation, and destroy preserves the repository and worktrees entirely.

use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Command;

use taskai_core::mail::{inherited_messaging_env_names, strip_messaging_env};
use taskai_core::registry::{Item, Registry, Source};
use taskai_core::workspace::{preview, provision, suggest_worktree, WorkspaceChoice};

fn git(dir: &Path, args: &[&str]) -> String {
    let mut git = Command::new("git");
    git.arg("-C").arg(dir).args(args);
    strip_messaging_env(&mut git);
    let out = git.output().expect("run git");
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

#[test]
fn suggestions_follow_current_branch_and_skip_preserved_names() {
    let tmp = tempfile::tempdir().unwrap();
    let repo = fixture_repo(tmp.path());
    git(&repo, &["branch", "-m", "trunk"]);
    git(&repo, &["branch", "agent-1"]);
    std::fs::create_dir_all(tmp.path().join("buzz-worktrees/agent-2")).unwrap();
    git(&repo, &["branch", "agent-3/topic"]);
    let (branch, name) = suggest_worktree(&repo).unwrap();
    assert_eq!(branch, "trunk");
    assert_eq!(name, "agent-4");
    let created = provision(
        &WorkspaceChoice::Worktree {
            base: repo.clone(),
            branch,
            name,
        },
        &repo,
    )
    .unwrap();
    assert_eq!(
        git(&created, &["rev-parse", "HEAD"]),
        git(&repo, &["rev-parse", "HEAD"])
    );
    assert_eq!(suggest_worktree(&repo).unwrap().1, "agent-5");
}

#[test]
fn detached_head_can_supply_a_worktree_base() {
    let tmp = tempfile::tempdir().unwrap();
    let repo = fixture_repo(tmp.path());
    git(&repo, &["checkout", "--detach", "-q"]);
    assert_eq!(
        suggest_worktree(&repo),
        Some(("HEAD".into(), "agent-1".into()))
    );
}

#[test]
fn plain_directories_and_unborn_repositories_do_not_offer_broken_worktrees() {
    let tmp = tempfile::tempdir().unwrap();
    assert_eq!(suggest_worktree(tmp.path()), None);
    git(tmp.path(), &["init", "-q", "-b", "main"]);
    assert_eq!(suggest_worktree(tmp.path()), None);
}

#[test]
fn invalid_worktree_names_and_bases_do_not_create_directories() {
    let tmp = tempfile::tempdir().unwrap();
    let repo = fixture_repo(tmp.path());
    let outside = tmp.path().join("outside");
    for name in [
        "",
        ".",
        "..",
        "../outside",
        "nested/name",
        "nested\\name",
        "bad name",
        "--orphan",
        outside.to_str().unwrap(),
    ] {
        let choice = WorkspaceChoice::Worktree {
            base: repo.clone(),
            branch: "main".into(),
            name: name.into(),
        };
        assert!(
            provision(&choice, &repo).is_err(),
            "accepted unsafe name {name:?}"
        );
        assert!(!tmp.path().join("buzz-worktrees").exists());
        assert!(!outside.exists());
    }
    for branch in ["", "not-a-branch", "--help"] {
        let choice = WorkspaceChoice::Worktree {
            base: repo.clone(),
            branch: branch.into(),
            name: "valid-name".into(),
        };
        assert!(provision(&choice, &repo).is_err());
        assert!(!tmp.path().join("buzz-worktrees").exists());
    }
}

#[test]
fn existing_branches_and_folders_are_preserved_on_validation_failure() {
    let tmp = tempfile::tempdir().unwrap();
    let repo = fixture_repo(tmp.path());
    git(&repo, &["branch", "agent-1"]);
    let choice = WorkspaceChoice::Worktree {
        base: repo.clone(),
        branch: "main".into(),
        name: "agent-1".into(),
    };
    assert!(provision(&choice, &repo)
        .unwrap_err()
        .to_string()
        .contains("already exists"));
    assert!(!tmp.path().join("buzz-worktrees").exists());
    git(&repo, &["branch", "agent-prefix/topic"]);
    let choice = WorkspaceChoice::Worktree {
        base: repo.clone(),
        branch: "main".into(),
        name: "agent-prefix".into(),
    };
    assert!(provision(&choice, &repo)
        .unwrap_err()
        .to_string()
        .contains("prefix"));
    assert!(!tmp.path().join("buzz-worktrees").exists());
    let target = tmp.path().join("buzz-worktrees/agent-2");
    std::fs::create_dir_all(&target).unwrap();
    std::fs::write(target.join("sentinel"), "do not touch").unwrap();
    let choice = WorkspaceChoice::Worktree {
        base: repo.clone(),
        branch: "main".into(),
        name: "agent-2".into(),
    };
    assert!(provision(&choice, &repo)
        .unwrap_err()
        .to_string()
        .contains("already exists"));
    assert_eq!(
        std::fs::read_to_string(target.join("sentinel")).unwrap(),
        "do not touch"
    );
    assert!(!git(&repo, &["branch", "--list", "agent-2"]).contains("agent-2"));
}

/// A `git` that records each call's subcommand and whether a Claude messaging variable reached it,
/// judged by name only, then runs the real git.
const GIT_RECORDER: &str = r#"#!/bin/sh
verdict=$(LC_ALL=C awk 'BEGIN { for (name in ENVIRON) if (name ~ /^CLAUDE_CODE_MESSAGING_/) found = 1; print (found ? "messaging=true" : "messaging=false") }')
skip=
for arg in "$@"; do
  if [ -n "$skip" ]; then skip=; continue; fi
  case "$arg" in
    -C) skip=1 ;;
    -*) ;;
    *) printf '%s %s\n' "$verdict" "$arg" >> "$TASKAI_GIT_VERDICTS"; break ;;
  esac
done
exec "$TASKAI_REAL_GIT" "$@"
"#;

// Expectation: every git command workspace provisioning runs (the suggestion's rev-parse,
// symbolic-ref and branch listing, and a worktree's name check, branch checks and worktree add)
// inherits no CLAUDE_CODE_MESSAGING_* variable from TaskAI. Why: TaskAI is often started from a
// Claude session, and git runs hooks and helpers that would inherit its socket and token. Value:
// nothing a workspace operation starts can speak as the person's Claude session. A copy of this
// test binary inherits a fake variable, and a git placed first on its PATH records, by name only,
// whether one reached it.
#[test]
fn workspace_git_commands_inherit_no_claude_messaging_variables() {
    if let Some(repo) = std::env::var_os("TASKAI_WORKSPACE_ENV_REPO") {
        let repo = PathBuf::from(repo);
        println!("inherited={}", !inherited_messaging_env_names().is_empty());
        let (branch, name) = suggest_worktree(&repo).expect("a worktree suggestion");
        let choice = WorkspaceChoice::Worktree {
            base: repo.clone(),
            branch,
            name,
        };
        println!("provisioned={}", provision(&choice, &repo).is_ok());
        return;
    }
    let root = tempfile::tempdir().unwrap();
    let repo = fixture_repo(root.path());
    let recorder_dir = root.path().join("bin");
    std::fs::create_dir(&recorder_dir).unwrap();
    let recorder = recorder_dir.join("git");
    std::fs::write(&recorder, GIT_RECORDER).unwrap();
    std::fs::set_permissions(&recorder, std::fs::Permissions::from_mode(0o755)).unwrap();
    let search = std::env::var_os("PATH").unwrap();
    let real_git = std::env::split_paths(&search)
        .map(|dir| dir.join("git"))
        .find(|candidate| candidate.is_file())
        .expect("git on PATH");
    let path =
        std::env::join_paths(std::iter::once(recorder_dir).chain(std::env::split_paths(&search)))
            .unwrap();
    let verdicts = root.path().join("git-verdicts");
    let mut copy = Command::new(std::env::current_exe().unwrap());
    strip_messaging_env(&mut copy);
    let output = copy
        .args([
            "--exact",
            "workspace_git_commands_inherit_no_claude_messaging_variables",
            "--nocapture",
        ])
        .env("TASKAI_WORKSPACE_ENV_REPO", &repo)
        .env("CLAUDE_CODE_MESSAGING_SITE_PROBE", "fake")
        .env("PATH", path)
        .env("TASKAI_REAL_GIT", real_git)
        .env("TASKAI_GIT_VERDICTS", &verdicts)
        .output()
        .unwrap();
    assert!(output.status.success());
    let printed = String::from_utf8(output.stdout).unwrap();
    assert!(
        printed.lines().any(|line| line == "inherited=true"),
        "the copy inherits the fake variable"
    );
    assert!(printed.lines().any(|line| line == "provisioned=true"));
    let recorded = std::fs::read_to_string(&verdicts).unwrap();
    let clean = |subcommand: &str| {
        recorded
            .lines()
            .filter(|line| *line == format!("messaging=false {subcommand}"))
            .count()
    };
    for subcommand in [
        "rev-parse",
        "symbolic-ref",
        "check-ref-format",
        "show-ref",
        "worktree",
    ] {
        assert!(clean(subcommand) >= 1, "{subcommand} ran without one");
    }
    assert!(
        clean("for-each-ref") >= 2,
        "both branch listings ran without one"
    );
    assert!(
        recorded
            .lines()
            .all(|line| line.starts_with("messaging=false ")),
        "no git command saw a messaging variable"
    );
}
