// ABOUTME: The Registry — one truth for tasks, sessions, stable never-recycled refs,
// ABOUTME: topology, states, attribution, and atomically persisted state.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::Error;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum SessionState {
    Setup,
    Ready,
    Queued,
    Working,
    Live,
    Handoff,
    Attention,
    Complete,
}

impl SessionState {
    pub fn label(&self) -> &'static str {
        match self {
            SessionState::Setup => "SETUP",
            SessionState::Ready => "READY",
            SessionState::Queued => "QUEUED",
            SessionState::Working => "WORKING",
            SessionState::Live => "LIVE",
            SessionState::Handoff => "HANDOFF",
            SessionState::Attention => "ATTENTION",
            SessionState::Complete => "COMPLETE",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum Role {
    Manager,
    Subordinate,
}

impl Role {
    pub fn label(&self) -> &'static str {
        match self {
            Role::Manager => "Manager",
            Role::Subordinate => "Subordinate",
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum Source {
    HumanUi,
    Session(String),
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Item {
    Task(String),
    Session(String),
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct HudCounts {
    pub live: usize,
    pub queued: usize,
    pub attention: usize,
    pub tasks_active: usize,
}

/// The exact provider binding behind a session ref, filled in when a connector attaches.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct ProviderMapping {
    pub provider: String,
    pub provider_session_id: String,
    pub runtime_id: String,
    pub connector_version: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Task {
    pub task_ref: String,
    pub name: String,
    pub repo: PathBuf,
    pub created_by: Source,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Session {
    pub session_ref: String,
    pub task_ref: String,
    pub parent: Option<String>,
    pub role: Role,
    pub name: String,
    pub harness: String,
    pub workspace: PathBuf,
    pub state: SessionState,
    pub created_by: Source,
    pub provider: Option<ProviderMapping>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Audit {
    pub action: String,
    pub item: String,
    pub source: Source,
}

#[derive(Default, Serialize, Deserialize)]
struct Data {
    next_task: u64,
    next_session: u64,
    tasks: Vec<Task>,
    sessions: Vec<Session>,
    audit: Vec<Audit>,
}

pub struct Registry {
    dir: PathBuf,
    data: Data,
}

impl Registry {
    /// Opens (or creates) the registry in an owner-only directory.
    pub fn open(dir: &Path) -> Result<Self, Error> {
        std::fs::create_dir_all(dir)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
        }
        let state = dir.join("state.json");
        let data = if state.exists() {
            serde_json::from_str(&std::fs::read_to_string(&state)?)
                .map_err(|e| Error::Invalid(format!("state.json: {e}")))?
        } else {
            Data::default()
        };
        Ok(Registry {
            dir: dir.to_path_buf(),
            data,
        })
    }

    fn persist(&self) -> Result<(), Error> {
        let tmp = self.dir.join("state.json.tmp");
        let body = serde_json::to_string_pretty(&self.data)
            .map_err(|e| Error::Invalid(format!("serialize: {e}")))?;
        std::fs::write(&tmp, body)?;
        std::fs::rename(&tmp, self.dir.join("state.json"))?;
        Ok(())
    }

    fn note(&mut self, action: &str, item: &str, source: Source) {
        self.data.audit.push(Audit {
            action: action.into(),
            item: item.into(),
            source,
        });
    }

    pub fn create_task(
        &mut self,
        name: &str,
        repo: &Path,
        source: Source,
    ) -> Result<String, Error> {
        self.data.next_task += 1;
        let task_ref = format!("t{}", self.data.next_task);
        self.data.tasks.push(Task {
            task_ref: task_ref.clone(),
            name: name.into(),
            repo: repo.to_path_buf(),
            created_by: source.clone(),
        });
        self.note("create_task", &task_ref, source);
        self.persist()?;
        Ok(task_ref)
    }

    pub fn create_session(
        &mut self,
        task: &str,
        parent: Option<&str>,
        name: &str,
        harness: &str,
        workspace: &Path,
        source: Source,
    ) -> Result<String, Error> {
        if self.task(task).is_none() {
            return Err(Error::NotFound(format!("task {task}")));
        }
        let role = match parent {
            None => Role::Manager,
            Some(p) => {
                let ps = self
                    .session(p)
                    .ok_or_else(|| Error::NotFound(format!("parent {p}")))?;
                if ps.task_ref != task {
                    return Err(Error::Invalid(format!(
                        "parent {p} belongs to {}",
                        ps.task_ref
                    )));
                }
                if ps.role != Role::Manager {
                    return Err(Error::Invalid(format!("parent {p} is not a manager")));
                }
                Role::Subordinate
            }
        };
        self.data.next_session += 1;
        let session_ref = format!("s{}", self.data.next_session);
        self.data.sessions.push(Session {
            session_ref: session_ref.clone(),
            task_ref: task.into(),
            parent: parent.map(str::to_string),
            role,
            name: name.into(),
            harness: harness.into(),
            workspace: workspace.to_path_buf(),
            state: SessionState::Queued,
            created_by: source.clone(),
            provider: None,
        });
        self.note("create_session", &session_ref, source);
        self.persist()?;
        Ok(session_ref)
    }

    pub fn task(&self, r: &str) -> Option<&Task> {
        self.data.tasks.iter().find(|t| t.task_ref == r)
    }

    pub fn session(&self, r: &str) -> Option<&Session> {
        self.data.sessions.iter().find(|s| s.session_ref == r)
    }

    pub fn tasks(&self) -> &[Task] {
        &self.data.tasks
    }

    pub fn sessions_of(&self, task: &str) -> Vec<&Session> {
        self.data
            .sessions
            .iter()
            .filter(|s| s.task_ref == task)
            .collect()
    }

    pub fn children_of(&self, session: &str) -> Vec<&Session> {
        self.data
            .sessions
            .iter()
            .filter(|s| s.parent.as_deref() == Some(session))
            .collect()
    }

    pub fn audit(&self) -> &[Audit] {
        &self.data.audit
    }

    /// Monotonic revision — rises with every attributed mutation.
    pub fn revision(&self) -> u64 {
        self.data.audit.len() as u64
    }

    /// HUD counts derive from the one session list; a Handoff session is never LIVE.
    pub fn hud_counts(&self) -> HudCounts {
        let mut c = HudCounts::default();
        for s in &self.data.sessions {
            match s.state {
                SessionState::Live => c.live += 1,
                SessionState::Queued | SessionState::Handoff => c.queued += 1,
                SessionState::Attention => c.attention += 1,
                _ => {}
            }
        }
        c.tasks_active = self
            .data
            .tasks
            .iter()
            .filter(|t| {
                let sessions = self.sessions_of(&t.task_ref);
                !sessions.is_empty() && sessions.iter().any(|s| s.state != SessionState::Complete)
            })
            .count();
        c
    }

    /// The task row's aggregate state word: attention rises through the hierarchy.
    pub fn task_status(&self, task: &str) -> &'static str {
        let sessions = self.sessions_of(task);
        if sessions.is_empty() {
            return "READY";
        }
        if sessions.iter().any(|s| s.state == SessionState::Attention) {
            return "NEEDS ATTENTION";
        }
        if sessions.iter().all(|s| s.state == SessionState::Complete) {
            return "COMPLETE";
        }
        "WORKING"
    }

    pub fn set_state(&mut self, r: &str, state: SessionState) -> Result<(), Error> {
        let s = self
            .data
            .sessions
            .iter_mut()
            .find(|s| s.session_ref == r)
            .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
        s.state = state;
        self.persist()
    }

    pub fn bind_provider(&mut self, r: &str, mapping: ProviderMapping) -> Result<(), Error> {
        let s = self
            .data
            .sessions
            .iter_mut()
            .find(|s| s.session_ref == r)
            .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
        s.provider = Some(mapping);
        self.persist()
    }

    /// Drops a provider binding proven unresumable — reconciled from native truth.
    pub fn clear_provider(&mut self, r: &str) -> Result<(), Error> {
        let s = self
            .data
            .sessions
            .iter_mut()
            .find(|s| s.session_ref == r)
            .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
        s.provider = None;
        self.persist()
    }

    /// The exact session refs destroy would remove, in confirmation order:
    /// each manager immediately followed by its subordinates.
    pub fn blast(&self, item: &Item) -> Result<Vec<String>, Error> {
        match item {
            Item::Session(r) => {
                let s = self
                    .session(r)
                    .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
                let mut out = vec![s.session_ref.clone()];
                out.extend(self.children_of(r).iter().map(|c| c.session_ref.clone()));
                Ok(out)
            }
            Item::Task(r) => {
                if self.task(r).is_none() {
                    return Err(Error::NotFound(format!("task {r}")));
                }
                let mut out = Vec::new();
                for s in self.sessions_of(r) {
                    if s.role == Role::Manager {
                        out.push(s.session_ref.clone());
                        out.extend(
                            self.children_of(&s.session_ref)
                                .iter()
                                .map(|c| c.session_ref.clone()),
                        );
                    }
                }
                Ok(out)
            }
        }
    }

    /// Removes exactly the blast-radius entries (and the task entry for a task).
    /// Never touches repositories, worktrees, or provider-native history.
    pub fn destroy(&mut self, item: &Item, source: Source) -> Result<Vec<String>, Error> {
        let removed = self.blast(item)?;
        self.data
            .sessions
            .retain(|s| !removed.contains(&s.session_ref));
        if let Item::Task(r) = item {
            self.data.tasks.retain(|t| t.task_ref != *r);
        }
        let label = match item {
            Item::Task(r) => r.clone(),
            Item::Session(r) => r.clone(),
        };
        self.note("destroy", &label, source);
        self.persist()?;
        Ok(removed)
    }
}
