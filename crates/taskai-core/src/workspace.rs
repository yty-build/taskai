// ABOUTME: Workspace provisioning — a subordinate runs in the manager's repository,
// ABOUTME: another repository, or a git worktree created next to its base repo.

use std::collections::HashSet;
use std::path::{Component, Path, PathBuf};
use std::process::Command;

use crate::{mail::strip_messaging_env, Error};

#[derive(Clone, Debug)]
pub enum WorkspaceChoice {
    SameRepo,
    OtherRepo(PathBuf),
    Worktree {
        base: PathBuf,
        branch: String,
        name: String,
    },
}

/// Suggest a worktree only when the repository has a usable commit. The base is
/// the current branch (or HEAD when detached); the name avoids both existing git
/// branches and preserved worktree directories from earlier agents.
pub fn suggest_worktree(repo: &Path) -> Option<(String, String)> {
    resolve_commit(repo, "HEAD").ok()?;
    let symbolic = git()
        .arg("-C")
        .arg(repo)
        .args(["symbolic-ref", "--quiet", "--short", "HEAD"])
        .output()
        .ok()?;
    let branch = if symbolic.status.success() {
        String::from_utf8_lossy(&symbolic.stdout).trim().to_string()
    } else {
        "HEAD".into()
    };
    let refs = git()
        .arg("-C")
        .arg(repo)
        .args(["for-each-ref", "--format=%(refname:short)", "refs/heads/"])
        .output()
        .ok()?;
    if !refs.status.success() {
        return None;
    }
    let refs = String::from_utf8_lossy(&refs.stdout);
    let names: HashSet<&str> = refs.lines().collect();
    for index in 1.. {
        let name = format!("agent-{index}");
        let path = preview(
            &WorkspaceChoice::Worktree {
                base: repo.to_path_buf(),
                branch: branch.clone(),
                name: name.clone(),
            },
            repo,
        );
        let branch_prefix = format!("{name}/");
        let branch_taken = names.contains(name.as_str())
            || names
                .iter()
                .any(|existing| existing.starts_with(&branch_prefix));
        if !branch_taken {
            match path.symlink_metadata() {
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    return Some((branch, name));
                }
                Err(_) => return None,
                Ok(_) => {}
            }
        }
    }
    unreachable!("the integer range is unbounded")
}

fn resolve_commit(repo: &Path, revision: &str) -> Result<String, Error> {
    if revision.trim().is_empty() {
        return Err(Error::Invalid(
            "choose a base branch or commit first".into(),
        ));
    }
    let output = git()
        .arg("-C")
        .arg(repo)
        .args(["rev-parse", "--verify", "--quiet", "--end-of-options"])
        .arg(format!("{revision}^{{commit}}"))
        .output()?;
    if !output.status.success() {
        return Err(Error::Git(format!(
            "base {revision:?} is not a commit in {}; choose an existing branch or use Same repository",
            repo.display()
        )));
    }
    Ok(String::from_utf8_lossy(&output.stdout).trim().to_string())
}

fn validate_worktree(base: &Path, branch: &str, name: &str, path: &Path) -> Result<String, Error> {
    let mut components = Path::new(name).components();
    if !matches!(components.next(), Some(Component::Normal(_)))
        || components.next().is_some()
        || name.contains(['/', '\\'])
    {
        return Err(Error::Invalid(
            "worktree name must be a single folder name, such as agent-1 (no path separators)"
                .into(),
        ));
    }
    let valid_name = git()
        .args(["check-ref-format", "--branch", name])
        .output()?;
    if !valid_name.status.success() {
        return Err(Error::Invalid(
            "worktree name must also be a valid new git branch name, such as agent-1".into(),
        ));
    }
    let commit = resolve_commit(base, branch)?;
    if path.symlink_metadata().is_ok() {
        return Err(Error::Invalid(format!(
            "worktree folder {} already exists; choose another name",
            path.display()
        )));
    }
    let existing = git()
        .arg("-C")
        .arg(base)
        .args(["show-ref", "--verify", "--quiet"])
        .arg(format!("refs/heads/{name}"))
        .output()?;
    if existing.status.success() {
        return Err(Error::Invalid(format!(
            "git branch {name:?} already exists; choose another worktree name"
        )));
    }
    if existing.status.code() != Some(1) {
        return Err(Error::Git(
            String::from_utf8_lossy(&existing.stderr).trim().to_string(),
        ));
    }
    let descendants = git()
        .arg("-C")
        .arg(base)
        .args(["for-each-ref", "--format=%(refname)"])
        .arg(format!("refs/heads/{name}/"))
        .output()?;
    if !descendants.status.success() {
        return Err(Error::Git(
            String::from_utf8_lossy(&descendants.stderr)
                .trim()
                .to_string(),
        ));
    }
    if !descendants.stdout.is_empty() {
        return Err(Error::Invalid(format!(
            "git branches already use the {name:?} prefix; choose another worktree name"
        )));
    }
    Ok(commit)
}

/// The path the choice resolves to — shown in the drawer before anything is created.
pub fn preview(choice: &WorkspaceChoice, manager_repo: &Path) -> PathBuf {
    match choice {
        WorkspaceChoice::SameRepo => manager_repo.to_path_buf(),
        WorkspaceChoice::OtherRepo(p) => p.clone(),
        WorkspaceChoice::Worktree { base, name, .. } => {
            let repo_name = base
                .file_name()
                .map(|n| n.to_string_lossy())
                .unwrap_or_default();
            base.parent()
                .unwrap_or(Path::new("."))
                .join(format!("{repo_name}-worktrees"))
                .join(name)
        }
    }
}

/// Resolves the workspace, creating the worktree when chosen. The created path always
/// equals the previewed path.
pub fn provision(choice: &WorkspaceChoice, manager_repo: &Path) -> Result<PathBuf, Error> {
    let path = preview(choice, manager_repo);
    match choice {
        WorkspaceChoice::SameRepo => Ok(path),
        WorkspaceChoice::OtherRepo(p) => {
            if p.is_dir() {
                Ok(path)
            } else {
                Err(Error::NotFound(format!("repository {}", p.display())))
            }
        }
        WorkspaceChoice::Worktree { base, branch, name } => {
            // Validate before creating even an empty parent directory. The name
            // is also a git branch, never a path that can escape the preview.
            let commit = validate_worktree(base, branch, name, &path)?;
            if let Some(parent) = path.parent() {
                std::fs::create_dir_all(parent)?;
            }
            let out = git()
                .arg("-C")
                .arg(base)
                .args(["worktree", "add", "-b", name, "--"])
                .arg(&path)
                .arg(commit)
                .output()?;
            if !out.status.success() {
                return Err(Error::Git(
                    String::from_utf8_lossy(&out.stderr).trim().to_string(),
                ));
            }
            Ok(path)
        }
    }
}

/// git, without the Claude messaging variables TaskAI inherited; these calls set no variables of
/// their own.
fn git() -> Command {
    let mut git = Command::new("git");
    strip_messaging_env(&mut git);
    git
}
