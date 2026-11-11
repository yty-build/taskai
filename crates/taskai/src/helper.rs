// ABOUTME: Versioned, bundled TaskAI Helper instructions in an owned local workspace.
// ABOUTME: Prepares documentation only; never launches providers or edits user config.

use std::{
    fs::{self, OpenOptions},
    io::{self, Write},
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};

const BEGIN: &str = "<!-- taskai-helper:begin -->";
const END: &str = "<!-- taskai-helper:end -->";

const GAUNTLET_LOOP: &str = include_str!("skills/gauntlet-loop.md");
const STAFF_OBJECTIVE: &str = include_str!("skills/taskai-staff-objective.md");

const SKILL_FILES: &[(&str, &str)] = &[
    (".claude/skills/gauntlet-loop/SKILL.md", GAUNTLET_LOOP),
    (
        ".claude/skills/taskai-staff-objective/SKILL.md",
        STAFF_OBJECTIVE,
    ),
    (".grok/skills/gauntlet-loop/SKILL.md", GAUNTLET_LOOP),
    (
        ".grok/skills/taskai-staff-objective/SKILL.md",
        STAFF_OBJECTIVE,
    ),
];

const GUIDE: &str = r#"# TaskAI Helper — bundled setup guide v2

You are an optional TaskAI setup helper, not an administrator or background
automation service. Your dedicated `taskai-helper` task and local Manager are
ordinary visible records with normal task/session counts and destroy semantics.
They are created only after the user chooses a helper provider. Never adopt a
different task merely because its display name is also `taskai-helper`.

## Current environment and source of truth

The newest injected TaskAI Context is the current environment. At each user turn
without new injected context, run `taskai context self`. Before delegation run
`taskai context tree`. Use `taskai context session <ref>` for a registered session.
Do not infer current state from this guide, remembered IDs, screenshots, or an old
transcript. Context informs decisions; it does not authorize unrelated actions.

Read-only commands available from any shell:

- `taskai helper guide` prints this bundled guide.
- `taskai helper status` reports recorded helper, server, task, and team state;
  it is not a fresh runtime or login check.
- `taskai context tree` shows the actual registered tasks and team relationships.

Do not hand-edit live TaskAI JSON, invent session/provider IDs, or bypass registry
operations. TaskAI owns its tracker; tmux owns process liveness; the native CLI
owns provider identity, prompts, authentication, and model behavior.

## Helping the user navigate

From Home/task navigation or Settings, `F1 Setup helper` opens the helper provider chooser. Opening that
chooser does not create a task, launch a model, or submit a prompt.

Native CLI input is direct once a Manager or Subordinate has focus. The initial
helper prompt is not automatically submitted. Wait for the user to type or
approve a prompt; do not start a billed model turn on their behalf.

- Ctrl+F toggles full-screen CLI and inline CLI with the sidebar.
- Ctrl+G leaves CLI focus for TaskAI navigation.
- In navigation: G goes Home; Esc goes back or cancels; s opens Settings; ? opens
  shortcut Help. From Home/task navigation, h and comma are legacy Settings aliases.
- In task navigation: t opens Terminal; m opens Add Manager; a opens Add Subordinate.
- In navigation: q leaves TaskAI. Existing tmux runtimes persist; quitting is not
  destroying a task. TaskAI destroy has an explicit confirmation/blast radius.
- Forms: Tab/Shift+Tab move fields, Ctrl+A selects text, Ctrl+U clears it, and Esc
  cancels. On New Task, Enter on Server chooses a saved server, F2 browses its
  repositories, and Enter on the final task-name field creates the task.

Saved Servers describe an immutable connection target, a default repository
root, and per-provider enable/executable/model defaults. `This computer` is Local.
Remote targets use existing SSH config aliases, not pasted passwords or arbitrary
shell commands. New Task selects Server, then Repository, then Task name. Local
and remote paths belong to different machines; never carry a path across hosts
without checking it on the chosen target. Updating defaults affects new agents,
not an existing session's launch snapshot or native provider identity.

Adding or saving a server starts non-interactive connection, tmux, version, and
supported native saved-login checks. Saving provider defaults, F5, and returning
from Configure CLI also refresh checks. Settings shows one actionable Status;
Ready requires a supported version and a native CLI report of saved login.
No check submits a model prompt. Saved login does NOT prove model access, credit,
or a successful model turn. Unsupported or failed login checks stay unknown;
never claim Ready based only on a version string.

## Reviewable UI requests

Only the registered helper Manager may issue these requests, through the existing
TaskAI control channel with its actual TASKAI_SESSION_REF. Never forge the caller
environment. These requests only queue a reviewable UI destination; they do not
create tasks, agents, profiles, install software, or save configuration:

- `taskai helper request open-settings [server-id]`
- `taskai helper request add-server`
- `taskai helper request new-task`
- `taskai helper request add-manager <task-ref>`
- `taskai helper request add-subordinate <manager-ref>`

Tell the user what you queued. They press Ctrl+G to return to navigation, then F8
to review/open the requested destination. Esc in navigation dismisses the
pending request. The user reviews and submits the ordinary form themselves.
Use real refs from context/status, not display names. A rejected or disconnected
request is not success; explain the error and provide the manual shortcut.

## Objectives, gauntlet staffing, and recommended teams

When the user states a goal or asks you to create agents, follow
`.claude/skills/taskai-staff-objective/SKILL.md` (Grok loads the same skill
from `.grok/skills/`). Use `.claude/skills/gauntlet-loop/SKILL.md` for the
quality bar. You still do not start the work yourself.

Ask probing questions first. Do not write a paste prompt, pick harnesses, or
queue UI requests until the user has answered enough to name a real bar.

The paste-ready brief uses three headings and nothing else:

The task
what to build, for whom, and any named constraints

The build method (How)
builder and a separate harsh critic on each judgeable piece, looping until
the critic wins blind; portable loop instructions for non-Claude leads

The bar to hit (When)
a named, fetchable, comparable reference; exit when the critic picks ours,
never after a round count

Recommend a Manager and Subordinates (Claude Code, Codex, Grok CLI, OpenCode)
from `taskai helper status` readiness and the kind of work. The critic must
be a different harness and session from the builder. Do not invent remaining
usage or quota; if spend matters, ask. Do not create tasks or agents. After
the user accepts the recommendation, queue one existing
`taskai helper request ...` at a time and tell them to press Ctrl+G then F8.
They paste the brief into the Lead themselves.

## Safety, native authentication, and approvals

Do not read, copy, print, move, or edit credential stores, tokens, private SSH
keys, browser sessions, environment secrets, or provider auth files. Never ask
the user to paste credentials into chat or TaskAI forms. Authentication stays in
the provider's own CLI on the correct computer/server. Settings Configure CLI
opens that native interface; Settings Terminal is a raw shell for deliberate
maintenance. Neither is a hidden helper agent or an auth shortcut.

First explain the exact target and action. Do not install or upgrade software,
change SSH/provider/model configuration, switch models, execute model prompts,
or incur model charges without the user's explicit approval. Do not turn a UI
navigation request into permission to perform those changes. Do not send a
prompt or control sequence into another user's running provider session.

Prefer read-only diagnostics and reviewable forms. Preserve repositories,
worktrees, native session history, and running work. Never destroy a task, agent,
or tmux runtime unless that exact action has been explicitly authorized. Explain
the blast radius before confirmation. Archive hides a server from new-task
selection; it does not kill that server's existing work.

This is a bundled versioned guide, not an instruction to independently browse,
install, authenticate, create work, or run models. Ask when authority or the exact
target is unclear; do not silently improvise a broader action.
"#;

const AGENTS: &str = r#"# TaskAI Helper instructions v2

Read README.md's bundled TaskAI Helper setup guide before advising or acting.
The newest injected TaskAI Context is current. At each turn without new context,
run `taskai context self`; before delegating, run `taskai context tree`.

This is an ordinary optional local Manager. Do not read/copy credential stores,
hand-edit live tracker JSON, or silently install/change configuration or models.
Native provider authentication stays in the native CLI. A version check is not
proof of login. Model calls and maintenance changes need explicit user approval.

When the user states an objective, follow `.claude/skills/taskai-staff-objective/SKILL.md`.
Ask probing questions before writing The task / The build method (How) / The bar to hit (When).
Recommend Manager and Subordinate harnesses from helper status; do not invent quota.
Do not create tasks or agents; queue one `taskai helper request ...` after they accept.

`taskai helper guide` and `taskai helper status` are read-only. Supported
`taskai helper request ...` commands only queue existing UI destinations. The
user presses Ctrl+G then F8 to review; Esc in navigation dismisses the request.
Never forge TASKAI_SESSION_REF or target refs; use current registered context.
"#;

pub fn guide() -> &'static str {
    GUIDE
}

/// Installs/refreshes only our marked sections, preserving all unrelated text.
/// Refuse symlinks and malformed marker blocks instead of overwriting an
/// unexpected file. All files are validated before any is changed.
pub fn prepare_workspace(home: &Path) -> Result<PathBuf, String> {
    let workspace = home.join("helper");
    match fs::symlink_metadata(&workspace) {
        Ok(metadata) if metadata.is_dir() && !metadata.file_type().is_symlink() => {}
        Ok(_) => {
            return Err(format!(
                "Helper workspace must be a real directory: {}",
                workspace.display()
            ))
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            fs::create_dir_all(&workspace)
                .map_err(|error| format!("Create helper workspace: {error}"))?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                fs::set_permissions(&workspace, fs::Permissions::from_mode(0o700))
                    .map_err(|error| format!("Protect helper workspace: {error}"))?;
            }
        }
        Err(error) => return Err(format!("Read helper workspace: {error}")),
    }
    let mut prepared = [("AGENTS.md", AGENTS), ("README.md", GUIDE)]
        .into_iter()
        .map(|(name, content)| {
            let path = workspace.join(name);
            let previous = read_owned_file(&path)?;
            let updated = managed_content(&previous, content)?;
            Ok((path, previous, updated))
        })
        .collect::<Result<Vec<_>, String>>()?;
    for (rel, content) in SKILL_FILES {
        let path = workspace.join(rel);
        let previous = read_owned_file(&path)?;
        let updated = managed_skill_content(&previous, content)?;
        prepared.push((path, previous, updated));
    }
    for (path, previous, updated) in &prepared {
        if previous != updated {
            if let Some(parent) = path.parent() {
                ensure_owned_dir(parent, &workspace)?;
            }
        }
    }
    for (path, previous, updated) in prepared {
        if previous != updated {
            atomic_write(&path, updated.as_bytes())
                .map_err(|error| format!("Write helper guide {}: {error}", path.display()))?;
        }
    }
    workspace
        .canonicalize()
        .map_err(|error| format!("Resolve helper workspace: {error}"))
}

fn ensure_owned_dir(dir: &Path, workspace: &Path) -> Result<(), String> {
    if dir == workspace {
        return Ok(());
    }
    if let Some(parent) = dir.parent() {
        ensure_owned_dir(parent, workspace)?;
    }
    match fs::symlink_metadata(dir) {
        Ok(metadata) if metadata.file_type().is_symlink() => Err(format!(
            "Helper skill path must not be a symlink: {}",
            dir.display()
        )),
        Ok(metadata) if metadata.is_dir() => Ok(()),
        Ok(_) => Err(format!(
            "Helper skill path must be a directory: {}",
            dir.display()
        )),
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            fs::create_dir(dir).map_err(|error| {
                format!("Create helper skill directory {}: {error}", dir.display())
            })?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                fs::set_permissions(dir, fs::Permissions::from_mode(0o700)).map_err(|error| {
                    format!("Protect helper skill directory {}: {error}", dir.display())
                })?;
            }
            Ok(())
        }
        Err(error) => Err(format!(
            "Read helper skill directory {}: {error}",
            dir.display()
        )),
    }
}

fn read_owned_file(path: &Path) -> Result<String, String> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.is_file() && !metadata.file_type().is_symlink() => {
            fs::read_to_string(path)
                .map_err(|error| format!("Read helper guide {}: {error}", path.display()))
        }
        Ok(_) => Err(format!(
            "Helper guide must be a real text file: {}",
            path.display()
        )),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(String::new()),
        Err(error) => Err(format!("Read helper guide {}: {error}", path.display())),
    }
}

fn managed_content(previous: &str, content: &str) -> Result<String, String> {
    let block = format!("{BEGIN}\n{content}{END}");
    match (previous.find(BEGIN), previous.find(END)) {
        (None, None) => Ok(format!("{previous}{}{block}\n", if previous.is_empty() || previous.ends_with('\n') { "" } else { "\n" })),
        (Some(start), Some(end)) if start < end && previous.matches(BEGIN).count() == 1 && previous.matches(END).count() == 1 => {
            Ok(format!("{}{block}{}", &previous[..start], &previous[end + END.len()..]))
        }
        _ => Err("Helper guide has an incomplete or duplicate managed block; existing content was preserved".into()),
    }
}

/// Skill discovery requires frontmatter at byte zero. Keep the ownership marker
/// inside a YAML comment so metadata and instructions remain one managed block.
fn managed_skill_content(previous: &str, content: &str) -> Result<String, String> {
    if !previous.is_empty() && !previous.contains(BEGIN) && !previous.contains(END) {
        return Err(
            "Helper skill already exists without a managed block; existing content was preserved"
                .into(),
        );
    }
    let metadata_and_body = content
        .strip_prefix("---\n")
        .filter(|rest| rest.contains("\n---\n"))
        .ok_or("Bundled helper skill has no YAML frontmatter")?;
    // Validate marker counts and preserve the suffix, including user notes.
    let updated = managed_content(previous, content)?;
    let start = updated.find(BEGIN).expect("validated managed block");
    let end = updated.find(END).expect("validated managed block") + END.len();
    let prefix = &updated[..start];
    // Migrate the old marker-before-frontmatter format. User text that preceded
    // that block moves below it, intact, so it cannot hide the skill metadata.
    let notes = prefix.strip_prefix("---\n# ").unwrap_or(prefix);
    Ok(format!(
        "---\n# {BEGIN}\n{metadata_and_body}{END}{}{notes}",
        &updated[end..]
    ))
}

fn atomic_write(path: &Path, content: &[u8]) -> io::Result<()> {
    static NEXT: AtomicU64 = AtomicU64::new(0);
    let temporary = path.with_extension(format!(
        "taskai-helper-{}-{}.tmp",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(&temporary)?;
    let result = (|| {
        file.write_all(content)?;
        file.sync_all()?;
        if let Ok(metadata) = fs::metadata(path) {
            fs::set_permissions(&temporary, metadata.permissions())?;
        }
        fs::rename(&temporary, path)
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bundled_workspace_is_idempotent_and_preserves_user_text_and_files() {
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("helper");
        fs::create_dir(&workspace).unwrap();
        fs::write(
            workspace.join("AGENTS.md"),
            "User policy without a final newline",
        )
        .unwrap();
        fs::write(
            workspace.join("README.md"),
            format!("User preface\n{BEGIN}\nOld v0 guide\n{END}\nUser suffix\n"),
        )
        .unwrap();
        fs::write(workspace.join("notes.txt"), "Keep this exactly").unwrap();
        assert_eq!(
            prepare_workspace(root.path()).unwrap(),
            workspace.canonicalize().unwrap()
        );
        let agents = fs::read_to_string(workspace.join("AGENTS.md")).unwrap();
        let readme = fs::read_to_string(workspace.join("README.md")).unwrap();
        assert!(agents.starts_with("User policy without a final newline\n"));
        assert!(readme.starts_with("User preface\n"));
        assert!(readme.ends_with("User suffix\n"));
        assert!(!readme.contains("Old v0"));
        assert!(readme.contains(guide()));
        prepare_workspace(root.path()).unwrap();
        assert_eq!(
            fs::read_to_string(workspace.join("AGENTS.md")).unwrap(),
            agents
        );
        assert_eq!(
            fs::read_to_string(workspace.join("README.md")).unwrap(),
            readme
        );
        assert_eq!(
            fs::read_to_string(workspace.join("notes.txt")).unwrap(),
            "Keep this exactly"
        );
    }

    #[test]
    fn malformed_file_does_not_partially_update_the_other_guide() {
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("helper");
        fs::create_dir(&workspace).unwrap();
        fs::write(workspace.join("AGENTS.md"), "Keep existing policy").unwrap();
        fs::write(
            workspace.join("README.md"),
            format!("Keep existing guide\n{BEGIN}"),
        )
        .unwrap();
        assert!(prepare_workspace(root.path()).is_err());
        assert_eq!(
            fs::read_to_string(workspace.join("AGENTS.md")).unwrap(),
            "Keep existing policy"
        );
    }

    #[cfg(unix)]
    #[test]
    fn workspace_and_guide_symlinks_are_never_followed() {
        let root = tempfile::tempdir().unwrap();
        let target = tempfile::tempdir().unwrap();
        std::os::unix::fs::symlink(target.path(), root.path().join("helper")).unwrap();
        assert!(prepare_workspace(root.path()).is_err());
        assert_eq!(fs::read_dir(target.path()).unwrap().count(), 0);

        let root = tempfile::tempdir().unwrap();
        fs::create_dir(root.path().join("helper")).unwrap();
        let external = target.path().join("outside.md");
        fs::write(&external, "Never overwrite").unwrap();
        std::os::unix::fs::symlink(&external, root.path().join("helper/README.md")).unwrap();
        assert!(prepare_workspace(root.path()).is_err());
        assert_eq!(fs::read_to_string(external).unwrap(), "Never overwrite");
        assert!(!root.path().join("helper/AGENTS.md").exists());
    }

    #[test]
    fn guide_names_only_reviewable_actions_and_preserves_native_auth_authority() {
        for required in [
            "taskai context self",
            "taskai context tree",
            "taskai helper status",
            "taskai helper request open-settings [server-id]",
            "taskai helper request add-server",
            "taskai helper request new-task",
            "taskai helper request add-manager <task-ref>",
            "taskai helper request add-subordinate <manager-ref>",
            "Ctrl+G",
            "F8",
            "explicit approval",
            "Do not read, copy",
            "Do not hand-edit live TaskAI JSON",
            "The task",
            "The build method (How)",
            "The bar to hit (When)",
            "probing questions",
            "Do not create tasks or agents",
            "Recommend",
        ] {
            assert!(
                guide().contains(required),
                "missing guide safety/navigation: {required}"
            );
        }
    }

    #[test]
    fn bundled_workspace_installs_gauntlet_and_staff_skills_for_claude_and_grok() {
        let root = tempfile::tempdir().unwrap();
        let workspace = prepare_workspace(root.path()).unwrap();
        for rel in [
            ".claude/skills/gauntlet-loop/SKILL.md",
            ".claude/skills/taskai-staff-objective/SKILL.md",
            ".grok/skills/gauntlet-loop/SKILL.md",
            ".grok/skills/taskai-staff-objective/SKILL.md",
        ] {
            let path = workspace.join(rel);
            let body = fs::read_to_string(&path)
                .unwrap_or_else(|error| panic!("missing {}: {error}", path.display()));
            assert!(
                body.contains(BEGIN) && body.contains(END),
                "{} must be a managed helper block",
                path.display()
            );
            let metadata = body
                .strip_prefix("---\n")
                .and_then(|rest| rest.split_once("\n---\n"))
                .expect("installed skills must start with YAML frontmatter")
                .0;
            assert!(metadata.contains("\nname: "));
            assert!(metadata.contains("\ndescription: "));
        }
        let gauntlet =
            fs::read_to_string(workspace.join(".claude/skills/gauntlet-loop/SKILL.md")).unwrap();
        assert!(gauntlet.contains("name: gauntlet-loop"));
        assert!(gauntlet.contains("Named."));
        assert!(gauntlet.contains("Fetchable."));
        assert!(gauntlet.contains("Comparable."));
        assert!(gauntlet.contains("Matt Shumer"));
        assert!(gauntlet.contains("CC BY 4.0"));
        let staff =
            fs::read_to_string(workspace.join(".claude/skills/taskai-staff-objective/SKILL.md"))
                .unwrap();
        assert!(staff.contains("name: taskai-staff-objective"));
        assert!(staff.contains("The task"));
        assert!(staff.contains("The build method (How)"));
        assert!(staff.contains("The bar to hit (When)"));
        assert!(staff.contains("Do not create tasks or agents"));
        assert!(staff.contains("taskai helper request"));
        assert!(staff.contains("Claude Code"));
        assert!(staff.contains("Codex"));
        assert!(staff.contains("Grok CLI"));
        assert!(staff.contains("OpenCode"));
        assert!(
            staff.contains("Never invent remaining usage"),
            "must refuse invented provider quota"
        );
        let again =
            fs::read_to_string(workspace.join(".claude/skills/gauntlet-loop/SKILL.md")).unwrap();
        prepare_workspace(root.path()).unwrap();
        assert_eq!(
            fs::read_to_string(workspace.join(".claude/skills/gauntlet-loop/SKILL.md")).unwrap(),
            again
        );
    }

    #[test]
    fn skill_refresh_migrates_old_markers_and_preserves_notes() {
        let previous =
            format!("User preface\n{BEGIN}\n---\nname: old\n---\nOld body\n{END}\nUser suffix\n");
        let updated = managed_skill_content(&previous, GAUNTLET_LOOP).unwrap();
        assert!(updated.starts_with("---\n# <!-- taskai-helper:begin -->\nname: gauntlet-loop\n"));
        assert!(!updated.contains("Old body"));
        assert!(updated.ends_with("User suffix\nUser preface\n"));
        assert_eq!(
            managed_skill_content(&updated, GAUNTLET_LOOP).unwrap(),
            updated
        );
        assert!(managed_skill_content("---\nname: user-owned\n---\n", GAUNTLET_LOOP).is_err());
        assert!(managed_skill_content(BEGIN, GAUNTLET_LOOP).is_err());
    }
}
