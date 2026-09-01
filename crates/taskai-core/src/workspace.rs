// ABOUTME: Workspace provisioning — a subordinate runs in the manager's repository,
// ABOUTME: another repository, or a git worktree created next to its base repo.

use std::path::{Path, PathBuf};
use std::process::Command;

use crate::Error;

#[derive(Clone, Debug)]
pub enum WorkspaceChoice {
    SameRepo,
    OtherRepo(PathBuf),
    Worktree { base: PathBuf, branch: String, name: String },
}

/// The path the choice resolves to — shown in the drawer before anything is created.
pub fn preview(choice: &WorkspaceChoice, manager_repo: &Path) -> PathBuf {
    match choice {
        WorkspaceChoice::SameRepo => manager_repo.to_path_buf(),
        WorkspaceChoice::OtherRepo(p) => p.clone(),
        WorkspaceChoice::Worktree { base, name, .. } => {
            let repo_name = base.file_name().map(|n| n.to_string_lossy()).unwrap_or_default();
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
            if let Some(parent) = path.parent() {
                std::fs::create_dir_all(parent)?;
            }
            let out = Command::new("git")
                .arg("-C")
                .arg(base)
                .args(["worktree", "add"])
                .arg(&path)
                .args(["-b", name, branch])
                .output()?;
            if !out.status.success() {
                return Err(Error::Git(String::from_utf8_lossy(&out.stderr).trim().to_string()));
            }
            Ok(path)
        }
    }
}
