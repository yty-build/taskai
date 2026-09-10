// ABOUTME: The Registry — one truth for tasks, sessions, stable never-recycled refs,
// ABOUTME: local/SSH targets, provider/tmux mappings, state, and atomic persistence.

use std::{
    collections::BTreeMap,
    fmt,
    path::{Path, PathBuf},
};

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

/// Where a Task's repository and agent runtimes live.
///
/// SSH targets contain only a host alias from the user's SSH configuration. Credentials,
/// SSH options, and shell fragments are deliberately outside the registry model.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum ConnectionTarget {
    #[default]
    Local,
    Ssh {
        host: String,
    },
}

impl ConnectionTarget {
    /// Creates a remote target from a safe SSH config host alias.
    pub fn ssh(host: impl Into<String>) -> Result<Self, Error> {
        let target = Self::Ssh { host: host.into() };
        target.validate()?;
        Ok(target)
    }

    /// Command arguments for connecting to the target. Local tasks need no command.
    pub fn command_args(&self) -> Vec<&str> {
        match self {
            Self::Local => Vec::new(),
            Self::Ssh { host } => vec!["ssh", host.as_str()],
        }
    }

    fn validate(&self) -> Result<(), Error> {
        let Self::Ssh { host } = self else {
            return Ok(());
        };
        let safe = !host.is_empty()
            && !host.starts_with('-')
            && host
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'));
        if safe {
            Ok(())
        } else {
            Err(Error::Invalid(format!(
                "SSH target must be a host alias, got {host:?}"
            )))
        }
    }
}

impl fmt::Display for ConnectionTarget {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Local => f.write_str("<local>"),
            Self::Ssh { host } => write!(f, "ssh {host}"),
        }
    }
}

/// Durable tmux location for one Manager or Subordinate.
///
/// Provider identity remains in `Session::provider`; tmux locates the process and does
/// not replace the provider's native session identity.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct TmuxRuntimeLocator {
    pub target: ConnectionTarget,
    pub private_server: String,
    pub session: String,
}

/// Optional native CLI overrides. Empty settings preserve the provider's native
/// defaults and are snapshotted when an agent record is created.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaunchSettings {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub executable: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct HarnessSettings {
    #[serde(default = "enabled_by_default")]
    pub enabled: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub executable: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
}

fn enabled_by_default() -> bool {
    true
}

impl Default for HarnessSettings {
    fn default() -> Self {
        Self {
            enabled: true,
            executable: None,
            model: None,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct ServerProfile {
    pub server_id: String,
    pub name: String,
    pub target: ConnectionTarget,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub repository_root: Option<PathBuf>,
    #[serde(default)]
    pub harnesses: BTreeMap<String, HarnessSettings>,
    #[serde(default)]
    pub archived: bool,
    #[serde(default)]
    pub setup_runtimes: BTreeMap<String, TmuxRuntimeLocator>,
}

/// Optional setup helper. These are ordinary visible task/session refs, never
/// name-based aliases for a user's existing work.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct HelperSettings {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub preferred_harness: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub task_ref: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub manager_ref: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Task {
    pub task_ref: String,
    pub name: String,
    pub repo: PathBuf,
    #[serde(default)]
    pub connection: ConnectionTarget,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub server_profile_id: Option<String>,
    /// Lazy utility shell. Not an agent, provider session, or team member.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub terminal: Option<TmuxRuntimeLocator>,
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
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub runtime: Option<TmuxRuntimeLocator>,
    #[serde(default)]
    pub launch: LaunchSettings,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Audit {
    pub action: String,
    pub item: String,
    pub source: Source,
}

#[derive(Clone, Default, Serialize, Deserialize)]
struct Data {
    next_task: u64,
    next_session: u64,
    #[serde(default)]
    next_server: u64,
    #[serde(default)]
    servers: Vec<ServerProfile>,
    #[serde(default)]
    helper: HelperSettings,
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
        let original = if state.exists() {
            serde_json::from_str(&std::fs::read_to_string(&state)?)
                .map_err(|e| Error::Invalid(format!("state.json: {e}")))?
        } else {
            serde_json::json!({})
        };
        let data = if state.exists() {
            let data: Data = serde_json::from_value(original.clone())
                .map_err(|e| Error::Invalid(format!("state.json: {e}")))?;
            for task in &data.tasks {
                task.connection.validate()?;
                if let Some(terminal) = &task.terminal {
                    validate_internal_name(&terminal.private_server, "terminal tmux server")?;
                    if terminal.target != task.connection
                        || terminal.session != terminal_ref(&task.task_ref)
                    {
                        return Err(Error::Invalid(format!(
                            "terminal locator does not belong to {}",
                            task.task_ref
                        )));
                    }
                }
            }
            for session in &data.sessions {
                if let Some(runtime) = &session.runtime {
                    runtime.target.validate()?;
                    validate_internal_name(&runtime.private_server, "tmux private server")?;
                    if runtime.session != session.session_ref {
                        return Err(Error::Invalid(format!(
                            "tmux session for {} must use its internal ref",
                            session.session_ref
                        )));
                    }
                }
            }
            data
        } else {
            Data::default()
        };
        let mut registry = Registry {
            dir: dir.to_path_buf(),
            data,
        };
        let mut migrated = registry.migrate_servers()?;
        registry.validate_servers()?;
        migrated |= registry.heal_helper();
        if migrated {
            registry.persist()?;
        }
        Ok(registry)
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

    pub fn servers(&self) -> &[ServerProfile] {
        &self.data.servers
    }

    pub fn server(&self, id: &str) -> Option<&ServerProfile> {
        self.data
            .servers
            .iter()
            .find(|server| server.server_id == id)
    }

    pub fn helper(&self) -> &HelperSettings {
        &self.data.helper
    }

    /// Creates or reopens the dedicated local helper using only its saved refs.
    /// Existing managers retain their harness, launch snapshot, and native IDs.
    /// New records are staged and persisted together, so a disabled harness or
    /// persistence failure cannot leave an orphan task or consume visible refs.
    pub fn ensure_helper(
        &mut self,
        harness: &str,
        workspace: &Path,
        source: Source,
    ) -> Result<(String, String, bool), Error> {
        validate_harness_key(harness)?;
        validate_repository_root(Some(workspace))?;
        if !workspace.is_absolute() {
            return Err(Error::Invalid(
                "helper workspace must be an absolute local path".into(),
            ));
        }
        let mut staged = Self {
            dir: self.dir.clone(),
            data: self.data.clone(),
        };
        let healed = staged.heal_helper();
        if let Some(task_ref) = &staged.data.helper.task_ref {
            if staged
                .task(task_ref)
                .is_some_and(|task| task.repo != workspace)
            {
                return Err(Error::Invalid(
                    "the existing helper belongs to another workspace; its task was not retargeted"
                        .into(),
                ));
            }
        }
        if let (Some(task_ref), Some(manager_ref)) = (
            staged.data.helper.task_ref.clone(),
            staged.data.helper.manager_ref.clone(),
        ) {
            if healed {
                staged.persist()?;
                self.data = staged.data;
            }
            return Ok((task_ref, manager_ref, false));
        }
        let task_ref = match staged.data.helper.task_ref.clone() {
            Some(task_ref) => task_ref,
            None => {
                staged.insert_task_on_server("taskai-helper", workspace, "local", source.clone())?
            }
        };
        let manager_ref =
            staged.insert_session(&task_ref, None, "TaskAI Helper", harness, workspace, source)?;
        staged.data.helper = HelperSettings {
            preferred_harness: Some(harness.into()),
            task_ref: Some(task_ref.clone()),
            manager_ref: Some(manager_ref.clone()),
        };
        staged.persist()?;
        self.data = staged.data;
        Ok((task_ref, manager_ref, true))
    }

    fn heal_helper(&mut self) -> bool {
        let previous = self.data.helper.clone();
        if self
            .data
            .helper
            .preferred_harness
            .as_deref()
            .is_some_and(|h| validate_harness_key(h).is_err())
        {
            self.data.helper.preferred_harness = None;
        }
        let task = self
            .data
            .helper
            .task_ref
            .as_deref()
            .and_then(|r| self.task(r));
        if !task.is_some_and(|task| {
            task.connection == ConnectionTarget::Local
                && task.server_profile_id.as_deref() == Some("local")
        }) {
            self.data.helper.task_ref = None;
            self.data.helper.manager_ref = None;
        } else {
            let manager = self
                .data
                .helper
                .manager_ref
                .as_deref()
                .and_then(|r| self.session(r));
            let valid = manager.is_some_and(|manager| {
                Some(manager.task_ref.as_str()) == self.data.helper.task_ref.as_deref()
                    && manager.role == Role::Manager
                    && manager.parent.is_none()
                    && task.is_some_and(|task| manager.workspace == task.repo)
            });
            if !valid {
                self.data.helper.manager_ref = None;
            } else {
                self.data.helper.preferred_harness = manager.map(|manager| manager.harness.clone());
            }
        }
        previous != self.data.helper
    }

    pub fn create_server(
        &mut self,
        name: &str,
        target: ConnectionTarget,
        repository_root: Option<PathBuf>,
        source: Source,
    ) -> Result<String, Error> {
        target.validate()?;
        let name = validated_name(name)?;
        validate_repository_root(repository_root.as_deref())?;
        if self
            .data
            .servers
            .iter()
            .any(|server| !server.archived && server.target == target)
        {
            return Err(Error::Invalid(format!(
                "an active server profile already exists for {target}"
            )));
        }
        let id = self.next_server_id();
        self.data.servers.push(ServerProfile {
            server_id: id.clone(),
            name,
            target,
            repository_root,
            harnesses: BTreeMap::new(),
            archived: false,
            setup_runtimes: BTreeMap::new(),
        });
        self.note("create_server", &id, source);
        self.persist()?;
        Ok(id)
    }

    /// A server's connection target is immutable. Changing its display name or
    /// repository browser root cannot retarget existing tasks or runtimes.
    pub fn update_server(
        &mut self,
        id: &str,
        name: &str,
        repository_root: Option<PathBuf>,
        source: Source,
    ) -> Result<(), Error> {
        let name = validated_name(name)?;
        validate_repository_root(repository_root.as_deref())?;
        let server = self
            .data
            .servers
            .iter_mut()
            .find(|server| server.server_id == id)
            .ok_or_else(|| Error::NotFound(format!("server {id}")))?;
        server.name = name;
        server.repository_root = repository_root;
        self.note("update_server", id, source);
        self.persist()
    }

    pub fn set_server_harness(
        &mut self,
        id: &str,
        hkey: &str,
        settings: HarnessSettings,
        source: Source,
    ) -> Result<(), Error> {
        validate_harness_key(hkey)?;
        validate_launch(&LaunchSettings {
            executable: settings.executable.clone(),
            model: settings.model.clone(),
        })?;
        let server = self
            .data
            .servers
            .iter_mut()
            .find(|server| server.server_id == id)
            .ok_or_else(|| Error::NotFound(format!("server {id}")))?;
        server.harnesses.insert(hkey.into(), settings);
        self.note("set_server_harness", id, source);
        self.persist()
    }

    /// Archiving hides a profile from new task creation. It does not destroy,
    /// detach, retarget, or otherwise mutate any task or session on that server.
    pub fn archive_server(&mut self, id: &str, source: Source) -> Result<(), Error> {
        if id == "local" {
            return Err(Error::Invalid("This computer cannot be archived".into()));
        }
        let server = self
            .data
            .servers
            .iter_mut()
            .find(|server| server.server_id == id)
            .ok_or_else(|| Error::NotFound(format!("server {id}")))?;
        if server.archived {
            return Ok(());
        }
        server.archived = true;
        self.note("archive_server", id, source);
        self.persist()
    }

    /// Settings utilities are owned by their server profile, never agent records.
    pub fn bind_server_setup(
        &mut self,
        id: &str,
        key: &str,
        private_server: &str,
    ) -> Result<TmuxRuntimeLocator, Error> {
        if key != "terminal" {
            validate_harness_key(key)?;
        }
        validate_internal_name(private_server, "setup tmux server")?;
        let server = self
            .data
            .servers
            .iter_mut()
            .find(|server| server.server_id == id)
            .ok_or_else(|| Error::NotFound(format!("server {id}")))?;
        if let Some(runtime) = server.setup_runtimes.get(key) {
            return Ok(runtime.clone());
        }
        let runtime = TmuxRuntimeLocator {
            target: server.target.clone(),
            private_server: private_server.into(),
            session: format!("setup-{id}-{key}"),
        };
        server.setup_runtimes.insert(key.into(), runtime.clone());
        self.note("bind_server_setup", id, Source::HumanUi);
        self.persist()?;
        Ok(runtime)
    }

    fn next_server_id(&mut self) -> String {
        self.data.next_server += 1;
        format!("server-{}", self.data.next_server)
    }

    fn migrate_servers(&mut self) -> Result<bool, Error> {
        let mut changed = false;
        let highest = self
            .data
            .servers
            .iter()
            .filter_map(|server| {
                server
                    .server_id
                    .strip_prefix("server-")?
                    .parse::<u64>()
                    .ok()
            })
            .max()
            .unwrap_or(0);
        if self.data.next_server < highest {
            self.data.next_server = highest;
            changed = true;
        }
        if self.server("local").is_none() {
            self.data.servers.insert(
                0,
                ServerProfile {
                    server_id: "local".into(),
                    name: "This computer".into(),
                    target: ConnectionTarget::Local,
                    repository_root: None,
                    harnesses: BTreeMap::new(),
                    archived: false,
                    setup_runtimes: BTreeMap::new(),
                },
            );
            changed = true;
        }
        for index in 0..self.data.tasks.len() {
            if self.data.tasks[index].server_profile_id.is_some() {
                continue;
            }
            let target = self.data.tasks[index].connection.clone();
            let id = if let Some(server) = self
                .data
                .servers
                .iter()
                .find(|server| server.target == target && !server.archived)
            {
                server.server_id.clone()
            } else {
                let id = self.next_server_id();
                let name = match &target {
                    ConnectionTarget::Local => "This computer".into(),
                    ConnectionTarget::Ssh { host } => host.clone(),
                };
                self.data.servers.push(ServerProfile {
                    server_id: id.clone(),
                    name,
                    target,
                    repository_root: None,
                    harnesses: BTreeMap::new(),
                    archived: false,
                    setup_runtimes: BTreeMap::new(),
                });
                id
            };
            self.data.tasks[index].server_profile_id = Some(id);
            changed = true;
        }
        Ok(changed)
    }

    fn validate_servers(&self) -> Result<(), Error> {
        let mut seen = std::collections::HashSet::new();
        for server in &self.data.servers {
            validate_internal_name(&server.server_id, "server profile id")?;
            if !seen.insert(server.server_id.as_str()) {
                return Err(Error::Invalid(format!(
                    "duplicate server profile {}",
                    server.server_id
                )));
            }
            server.target.validate()?;
            validated_name(&server.name)?;
            validate_repository_root(server.repository_root.as_deref())?;
            if server.server_id == "local"
                && (server.target != ConnectionTarget::Local || server.archived)
            {
                return Err(Error::Invalid(
                    "the local profile must be active on this computer".into(),
                ));
            }
            if server.server_id != "local" && server.target == ConnectionTarget::Local {
                return Err(Error::Invalid(
                    "local connections use the built-in local profile".into(),
                ));
            }
            for settings in server.harnesses.values() {
                validate_launch(&LaunchSettings {
                    executable: settings.executable.clone(),
                    model: settings.model.clone(),
                })?;
            }
            for (key, runtime) in &server.setup_runtimes {
                if key != "terminal" {
                    validate_harness_key(key)?;
                }
                validate_internal_name(&runtime.private_server, "setup tmux server")?;
                if runtime.target != server.target
                    || runtime.session != format!("setup-{}-{key}", server.server_id)
                {
                    return Err(Error::Invalid(format!(
                        "setup runtime does not belong to {}",
                        server.server_id
                    )));
                }
            }
        }
        for task in &self.data.tasks {
            let server = task
                .server_profile_id
                .as_deref()
                .and_then(|id| self.server(id))
                .ok_or_else(|| {
                    Error::Invalid(format!(
                        "task {} refers to a missing server profile",
                        task.task_ref
                    ))
                })?;
            if server.target != task.connection {
                return Err(Error::Invalid(format!(
                    "server profile cannot retarget task {}",
                    task.task_ref
                )));
            }
        }
        for session in &self.data.sessions {
            validate_launch(&session.launch)?;
        }
        Ok(())
    }

    pub fn create_task(
        &mut self,
        name: &str,
        repo: &Path,
        source: Source,
    ) -> Result<String, Error> {
        self.create_task_on(name, repo, ConnectionTarget::Local, source)
    }

    /// Creates a Task whose repository is interpreted on the given connection target.
    pub fn create_task_on(
        &mut self,
        name: &str,
        repo: &Path,
        connection: ConnectionTarget,
        source: Source,
    ) -> Result<String, Error> {
        connection.validate()?;
        let server_id = if let Some(server) = self
            .data
            .servers
            .iter()
            .find(|server| server.target == connection && !server.archived)
        {
            server.server_id.clone()
        } else {
            let name = match &connection {
                ConnectionTarget::Local => "This computer",
                ConnectionTarget::Ssh { host } => host.as_str(),
            }
            .to_string();
            self.create_server(&name, connection, None, source.clone())?
        };
        self.create_task_on_server(name, repo, &server_id, source)
    }

    pub fn create_task_on_server(
        &mut self,
        name: &str,
        repo: &Path,
        server_id: &str,
        source: Source,
    ) -> Result<String, Error> {
        let task_ref = self.insert_task_on_server(name, repo, server_id, source)?;
        self.persist()?;
        Ok(task_ref)
    }

    fn insert_task_on_server(
        &mut self,
        name: &str,
        repo: &Path,
        server_id: &str,
        source: Source,
    ) -> Result<String, Error> {
        let server = self
            .server(server_id)
            .ok_or_else(|| Error::NotFound(format!("server {server_id}")))?;
        if server.archived {
            return Err(Error::Invalid(format!(
                "server {} is archived",
                server.name
            )));
        }
        let connection = server.target.clone();
        self.data.next_task += 1;
        let task_ref = format!("t{}", self.data.next_task);
        self.data.tasks.push(Task {
            task_ref: task_ref.clone(),
            name: name.into(),
            repo: repo.to_path_buf(),
            connection,
            server_profile_id: Some(server_id.into()),
            terminal: None,
            created_by: source.clone(),
        });
        self.note("create_task", &task_ref, source);
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
        let session_ref = self.insert_session(task, parent, name, harness, workspace, source)?;
        self.persist()?;
        Ok(session_ref)
    }

    fn insert_session(
        &mut self,
        task: &str,
        parent: Option<&str>,
        name: &str,
        harness: &str,
        workspace: &Path,
        source: Source,
    ) -> Result<String, Error> {
        let task_entry = self
            .task(task)
            .ok_or_else(|| Error::NotFound(format!("task {task}")))?;
        let settings = task_entry
            .server_profile_id
            .as_deref()
            .and_then(|id| self.server(id))
            .and_then(|server| server.harnesses.get(harness))
            .cloned()
            .unwrap_or_default();
        if !settings.enabled {
            return Err(Error::Invalid(format!(
                "{harness} is disabled for this task's server profile"
            )));
        }
        let launch = LaunchSettings {
            executable: settings.executable,
            model: settings.model,
        };
        validate_launch(&launch)?;
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
            runtime: None,
            launch,
        });
        self.note("create_session", &session_ref, source);
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

    /// Records the durable tmux process location for one agent session.
    ///
    /// The exact tmux session name is the stable TaskAI session ref, never a user-entered
    /// Task, Manager, or role name. Its target is copied from the owning Task.
    pub fn bind_runtime(&mut self, r: &str, private_server: &str) -> Result<(), Error> {
        validate_internal_name(private_server, "tmux private server")?;
        let task_ref = self
            .session(r)
            .ok_or_else(|| Error::NotFound(format!("session {r}")))?
            .task_ref
            .clone();
        let target = self
            .task(&task_ref)
            .ok_or_else(|| Error::NotFound(format!("task {task_ref}")))?
            .connection
            .clone();
        target.validate()?;
        let s = self
            .data
            .sessions
            .iter_mut()
            .find(|s| s.session_ref == r)
            .expect("session was resolved above");
        s.runtime = Some(TmuxRuntimeLocator {
            target,
            private_server: private_server.into(),
            session: r.into(),
        });
        self.persist()
    }

    /// Persist one task-owned utility shell before creating its runtime. Repeated
    /// opens reuse the same mapping; agent refs, counts and hierarchy are untouched.
    pub fn bind_terminal(
        &mut self,
        task_ref: &str,
        private_server: &str,
    ) -> Result<TmuxRuntimeLocator, Error> {
        validate_internal_name(private_server, "terminal tmux server")?;
        let task = self
            .data
            .tasks
            .iter_mut()
            .find(|t| t.task_ref == task_ref)
            .ok_or_else(|| Error::NotFound(format!("task {task_ref}")))?;
        if let Some(runtime) = &task.terminal {
            return Ok(runtime.clone());
        }
        let runtime = TmuxRuntimeLocator {
            target: task.connection.clone(),
            private_server: private_server.into(),
            session: terminal_ref(task_ref),
        };
        task.terminal = Some(runtime.clone());
        self.note("bind_terminal", task_ref, Source::HumanUi);
        self.persist()?;
        Ok(runtime)
    }

    /// Removes only the tmux process locator, leaving provider identity untouched.
    pub fn clear_runtime(&mut self, r: &str) -> Result<(), Error> {
        let s = self
            .data
            .sessions
            .iter_mut()
            .find(|s| s.session_ref == r)
            .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
        s.runtime = None;
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
        self.heal_helper();
        let label = match item {
            Item::Task(r) => r.clone(),
            Item::Session(r) => r.clone(),
        };
        self.note("destroy", &label, source);
        self.persist()?;
        Ok(removed)
    }
}

pub fn terminal_ref(task_ref: &str) -> String {
    format!("{task_ref}-terminal")
}

fn validated_name(name: &str) -> Result<String, Error> {
    if name.chars().any(char::is_control) {
        return Err(Error::Invalid(
            "server name must be nonempty single-line text".into(),
        ));
    }
    let name = name.trim();
    if name.is_empty() {
        return Err(Error::Invalid(
            "server name must be nonempty single-line text".into(),
        ));
    }
    Ok(name.into())
}

fn validate_harness_key(key: &str) -> Result<(), Error> {
    if matches!(key, "claude" | "codex" | "opencode" | "grok") {
        Ok(())
    } else {
        Err(Error::Invalid(format!("unknown harness {key:?}")))
    }
}

fn validate_repository_root(path: Option<&Path>) -> Result<(), Error> {
    let Some(path) = path else {
        return Ok(());
    };
    let text = path
        .to_str()
        .ok_or_else(|| Error::Invalid("repository root must be valid text".into()))?;
    if text.is_empty()
        || text.trim() != text
        || text.chars().any(char::is_control)
        || !(path.is_absolute() || text == "~" || text.starts_with("~/"))
    {
        return Err(Error::Invalid(
            "repository root must be an absolute path, ~, or ~/path without control characters"
                .into(),
        ));
    }
    Ok(())
}

fn validate_launch(settings: &LaunchSettings) -> Result<(), Error> {
    if let Some(executable) = &settings.executable {
        let simple_command = !executable.starts_with('-')
            && executable.bytes().all(|byte| {
                byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b'+')
            });
        if executable.is_empty()
            || executable.trim() != executable
            || executable.chars().any(char::is_control)
            || !(simple_command
                || Path::new(executable).is_absolute()
                || executable.starts_with("~/"))
        {
            return Err(Error::Invalid(
                "executable must be one command name or an absolute/~/ path, not a shell command"
                    .into(),
            ));
        }
    }
    if let Some(model) = &settings.model {
        if model.is_empty()
            || model.trim() != model
            || model.starts_with('-')
            || model.chars().any(char::is_control)
        {
            return Err(Error::Invalid(
                "model must be a nonempty identifier without control characters".into(),
            ));
        }
    }
    Ok(())
}

fn validate_internal_name(value: &str, kind: &str) -> Result<(), Error> {
    let safe = !value.is_empty()
        && !value.starts_with('-')
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'));
    if safe {
        Ok(())
    } else {
        Err(Error::Invalid(format!(
            "{kind} is not an internal safe name"
        )))
    }
}
