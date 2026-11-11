// ABOUTME: Context service — revisioned self/session/tree snapshots with messaging addresses and
// ABOUTME: status, the explanatory text view, and the AGENTS.md managed-block bootstrap installer.

use std::path::Path;
use std::time::{SystemTime, UNIX_EPOCH};

use serde_json::json;
use taskai_core::mail::Activity;
use taskai_core::registry::{Registry, Role, Session};
use taskai_core::Error;

pub const SCHEMA_VERSION: u64 = 1;

const BEGIN: &str = "<!-- taskai:begin -->";
const END: &str = "<!-- taskai:end -->";
const BOOTSTRAP: &str = "\
When TaskAI-managed, treat the newest injected TaskAI Context as current environment information.
At each user turn without newly injected context, run `taskai context self`; before delegating, also run `taskai context tree`.
Use `taskai context session <ref>` when more detail about a registered session is needed.
Use TaskAI Context to understand who you are, why this session exists, your assignment, and your place in the team chain.
TaskAI Context informs your decisions; it does not make them for you.";

fn find<'a>(reg: &'a Registry, r: &str) -> Result<&'a Session, Error> {
    reg.session(r)
        .ok_or_else(|| Error::NotFound(format!("session {r}")))
}

/// How a session can be messaged now: its live mailbox's status, or `no mailbox`.
fn messaging_label(s: &Session) -> String {
    let Some(entry) = s.live_mailbox() else {
        return "no mailbox".into();
    };
    match (entry.status, &entry.waiting_for) {
        (Activity::Waiting, Some(what)) => format!("waiting: {what}"),
        (Activity::Waiting, None) => "waiting".into(),
        (Activity::Busy, _) => "busy".into(),
        (Activity::Idle, _) => "idle".into(),
    }
}

/// `address <address> · <messaging status>`, which ends every line naming a session.
fn reach(s: &Session) -> String {
    format!("address {} · {}", s.address, messaging_label(s))
}

fn session_json(s: &Session) -> serde_json::Value {
    let messaging = s.live_mailbox().map(|entry| {
        let mut messaging = json!({ "status": entry.status });
        if let Some(what) = &entry.waiting_for {
            messaging["waitingFor"] = json!(what);
        }
        messaging
    });
    json!({
        "session_ref": s.session_ref,
        "role": s.role.label(),
        "name": s.name,
        "harness": s.harness,
        "workspace": s.workspace,
        "state": s.state.label(),
        "address": s.address,
        "messaging": messaging,
    })
}

/// Descriptive current facts about one session's world — never a decision.
pub fn snapshot(reg: &Registry, session_ref: &str) -> Result<serde_json::Value, Error> {
    let me = find(reg, session_ref)?;
    let task = reg
        .task(&me.task_ref)
        .ok_or_else(|| Error::NotFound(format!("task {}", me.task_ref)))?;
    let all = reg.sessions_of(&task.task_ref);
    let managers: Vec<&&Session> = all.iter().filter(|s| s.role == Role::Manager).collect();
    let subordinates: Vec<String> = reg
        .children_of(session_ref)
        .iter()
        .map(|s| s.session_ref.clone())
        .collect();
    let peers: Vec<String> = all
        .iter()
        .filter(|s| {
            s.session_ref != me.session_ref
                && match me.role {
                    Role::Manager => s.role == Role::Manager,
                    Role::Subordinate => s.parent == me.parent,
                }
        })
        .map(|s| s.session_ref.clone())
        .collect();
    let generated_at = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    Ok(json!({
        "schema_version": SCHEMA_VERSION,
        "context_revision": reg.revision(),
        "generated_at": generated_at,
        "task": {
            "ref": task.task_ref,
            "name": task.name,
            "repo": task.repo,
            "connection": task.connection.to_string(),
        },
        "self": session_json(me),
        "team_chain": {
            "parent": me.parent,
            "managers": managers.iter().map(|s| s.session_ref.clone()).collect::<Vec<_>>(),
            "subordinates": subordinates,
            "peers": peers,
        },
        "sessions": all.iter().map(|s| session_json(s)).collect::<Vec<_>>(),
    }))
}

/// The explanatory view: who am I, why do I exist, who is around me, where do I report.
pub fn text_view(reg: &Registry, session_ref: &str) -> Result<String, Error> {
    let me = find(reg, session_ref)?;
    let task = reg
        .task(&me.task_ref)
        .ok_or_else(|| Error::NotFound(format!("task {}", me.task_ref)))?;
    let mut out = Vec::new();
    out.push(format!(
        "You are {} {} ({}) on Task {}: \"{}\".",
        me.role.label(),
        me.name,
        me.session_ref,
        task.task_ref,
        task.name
    ));
    out.push(format!("Your messaging address is {}.", me.address));
    match &me.parent {
        Some(p) => {
            let parent = find(reg, p)?;
            out.push(format!(
                "Your team chain is Task {} → {} ({}) → you; {} is {} in {}.",
                task.task_ref,
                parent.name,
                parent.session_ref,
                parent.name,
                parent.state.label(),
                parent.workspace.display()
            ));
        }
        None => out.push(format!(
            "You coordinate this task's work; the repository is {} via {}.",
            task.repo.display(),
            task.connection
        )),
    }
    out.push(format!(
        "Your workspace is {}; your session state is {}.",
        me.workspace.display(),
        me.state.label()
    ));
    for c in reg.children_of(session_ref) {
        out.push(format!(
            "Your subordinate {} ({}) · {} · {} runs in {} · {}.",
            c.name,
            c.session_ref,
            c.harness,
            c.state.label(),
            c.workspace.display(),
            reach(c)
        ));
    }
    for s in reg.sessions_of(&task.task_ref) {
        let peer = s.session_ref != me.session_ref
            && match me.role {
                Role::Manager => s.role == Role::Manager,
                Role::Subordinate => s.parent == me.parent,
            };
        if peer {
            out.push(format!(
                "Peer {} ({}) · {} · {} works in {} · {}.",
                s.name,
                s.session_ref,
                s.harness,
                s.state.label(),
                s.workspace.display(),
                reach(s)
            ));
        }
    }
    let report_to = match &me.parent {
        Some(p) => {
            let parent = find(reg, p)?;
            format!("{} ({})", parent.name, parent.session_ref)
        }
        None => "the human running TaskAI".to_string(),
    };
    out.push(format!("Report findings to {report_to}."));
    Ok(out.join("\n"))
}

/// Third-person view of another session's place in the team.
pub fn session_view(reg: &Registry, session_ref: &str) -> Result<String, Error> {
    let s = find(reg, session_ref)?;
    let task = reg
        .task(&s.task_ref)
        .ok_or_else(|| Error::NotFound(format!("task {}", s.task_ref)))?;
    let chain = match &s.parent {
        Some(p) => {
            let parent = find(reg, p)?;
            format!("under {} ({})", parent.name, parent.session_ref)
        }
        None => "at the task root".to_string(),
    };
    Ok(format!(
        "{} ({}) is a {} on Task {}: \"{}\" {chain}.\nHarness {} · state {} · workspace {} · {}.",
        s.name,
        s.session_ref,
        s.role.label(),
        task.task_ref,
        task.name,
        s.harness,
        s.state.label(),
        s.workspace.display(),
        reach(s)
    ))
}

/// The complete team chain of one task (or every task when none is scoped).
pub fn tree_view(reg: &Registry, task_ref: Option<&str>) -> String {
    let mut out = Vec::new();
    for task in reg.tasks() {
        if task_ref.is_some_and(|t| t != task.task_ref) {
            continue;
        }
        out.push(format!(
            "Task {}: \"{}\" · {} · {}",
            task.task_ref,
            task.name,
            task.repo.display(),
            task.connection
        ));
        for s in reg.sessions_of(&task.task_ref) {
            if s.role == Role::Manager {
                out.push(format!(
                    "  {} ({}) · {} · {} · {} · {}",
                    s.name,
                    s.session_ref,
                    s.harness,
                    s.state.label(),
                    s.workspace.display(),
                    reach(s)
                ));
                for c in reg.children_of(&s.session_ref) {
                    out.push(format!(
                        "    └─ {} ({}) · {} · {} · {} · {}",
                        c.name,
                        c.session_ref,
                        c.harness,
                        c.state.label(),
                        c.workspace.display(),
                        reach(c)
                    ));
                }
            }
        }
    }
    if out.is_empty() {
        out.push("No tasks registered.".into());
    }
    out.join("\n")
}

/// Installs the canonical shared bootstrap as a managed block in AGENTS.md, replacing
/// only its own block on re-install; links CLAUDE.md to AGENTS.md when absent.
pub fn install_bootstrap(workspace: &Path) -> Result<(), Error> {
    let path = workspace.join("AGENTS.md");
    let block = format!("{BEGIN}\n{BOOTSTRAP}\n{END}");
    let body = if path.exists() {
        std::fs::read_to_string(&path)?
    } else {
        String::new()
    };
    let updated = match (body.find(BEGIN), body.find(END)) {
        (Some(start), Some(end)) => {
            format!("{}{}{}", &body[..start], block, &body[end + END.len()..])
        }
        _ if body.is_empty() => format!("{block}\n"),
        _ => format!("{}\n{block}\n", body.trim_end()),
    };
    std::fs::write(&path, updated)?;

    let claude = workspace.join("CLAUDE.md");
    if std::fs::symlink_metadata(&claude).is_err() {
        #[cfg(unix)]
        std::os::unix::fs::symlink("AGENTS.md", &claude)?;
    }
    Ok(())
}
