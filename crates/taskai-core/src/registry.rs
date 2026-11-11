// ABOUTME: The Registry — one truth for tasks, sessions, stable never-recycled refs, messaging
// ABOUTME: addresses and mailboxes, provider/tmux mappings, state, and locked atomic persistence.

use std::{
    collections::BTreeMap,
    fmt,
    fs::{self, OpenOptions},
    os::unix::fs::OpenOptionsExt,
    path::{Path, PathBuf},
};

use rustix::{
    fs::{flock, FlockOperation},
    io::Errno,
};
use serde::{Deserialize, Serialize};

use crate::{
    mail::{is_address, is_live, Activity, Directory, MailPeer, PermissionMode, MAX_ADDRESS_CHARS},
    write_private, Error,
};

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
    /// The session's messaging address: assigned once, unique in its task, and a plain address
    /// (`mail::is_address`). `name` stays the person's display label.
    #[serde(default)]
    pub address: String,
    /// The mailbox a bridge registered for this session, while it has one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub messaging: Option<MessagingEntry>,
}

/// What a bridge registered for its session's mailbox. Kept apart from `runtime` (the tmux
/// locator) and `provider` (the native identity): `pid` and `proc_start` are the bridge's own.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MessagingEntry {
    pub pid: u32,
    /// The bridge's start time as `ps -o lstart` prints it in UTC.
    pub proc_start: String,
    /// The mailbox socket.
    pub mailbox: PathBuf,
    pub features: Vec<String>,
    /// Observed permission mode, if the native peer reports it. This is not a wire assertion.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mode: Option<PermissionMode>,
    pub status: Activity,
    /// What a waiting session waits for; set only while waiting.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub waiting_for: Option<String>,
}

impl Session {
    /// The mailbox its bridge registered, while it can be routed to: the address is plain, the
    /// mailbox path is absolute, and the bridge still runs with its recorded start time.
    pub fn live_mailbox(&self) -> Option<&MessagingEntry> {
        let entry = self.messaging.as_ref()?;
        let routable = is_address(&self.address)
            && entry.mailbox.is_absolute()
            && is_live(entry.pid, &entry.proc_start);
        routable.then_some(entry)
    }
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
    /// Top-level settings this build does not know, kept as they are.
    #[serde(flatten)]
    other_settings: serde_json::Map<String, serde_json::Value>,
}

pub struct Registry {
    dir: PathBuf,
    data: Data,
}

impl Registry {
    /// Opens (or creates) the registry in an owner-only directory, bringing its state up to date.
    pub fn open(dir: &Path) -> Result<Self, Error> {
        std::fs::create_dir_all(dir)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
        }
        let _held = RegistryLock::take(dir)?;
        // Only a directory with nothing at state.json starts a new registry; anything there must
        // read as one.
        let (registry, on_disk) = match fs::symlink_metadata(dir.join("state.json")) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                (Self::up_to_date(dir, Data::default())?, None)
            }
            Err(error) => return Err(error.into()),
            Ok(_) => {
                let (registry, on_disk) = Self::read(dir)?;
                (registry, Some(on_disk))
            }
        };
        registry.save(on_disk.as_deref())?;
        Ok(registry)
    }

    /// Re-reads the registry as it is on disk, picking up other processes' changes. On failure the
    /// handle keeps its last good view.
    pub fn reload(&mut self) -> Result<(), Error> {
        self.data = Self::read(&self.dir)?.0.data;
        Ok(())
    }

    /// The registry as `state.json` holds it now, validated and brought up to date in memory,
    /// with the file's text, so a save can tell whether it changes anything. A missing or
    /// dangling state file is an error: only `open` starts a registry.
    fn read(dir: &Path) -> Result<(Self, String), Error> {
        let on_disk =
            fs::read_to_string(dir.join("state.json")).map_err(|error| match error.kind() {
                std::io::ErrorKind::NotFound => {
                    Error::NotFound(format!("no TaskAI registry at {}", dir.display()))
                }
                _ => error.into(),
            })?;
        let data: Data = serde_json::from_str(&on_disk)
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
        Ok((Self::up_to_date(dir, data)?, on_disk))
    }

    /// The registry `data` describes, brought up to date: server profiles, helper refs and
    /// session addresses.
    fn up_to_date(dir: &Path, data: Data) -> Result<Self, Error> {
        let mut registry = Registry {
            dir: dir.to_path_buf(),
            data,
        };
        registry.migrate_servers();
        registry.validate_servers()?;
        registry.heal_helper();
        registry.assign_addresses();
        Ok(registry)
    }

    /// Saves the state, owner-only and atomically, unless the file already holds exactly it.
    fn save(&self, on_disk: Option<&str>) -> Result<(), Error> {
        let body = serde_json::to_string_pretty(&self.data)
            .map_err(|e| Error::Invalid(format!("serialize: {e}")))?;
        if on_disk == Some(body.as_str()) {
            return Ok(());
        }
        write_private(&self.dir.join("state.json"), &body)
    }

    /// Applies `change` to the registry as it is on disk now, under the registry's lock, and
    /// saves the result, so no process's change is lost to another's stale copy. The handle's
    /// view becomes that state; a failed change saves nothing.
    fn transact<T>(
        &mut self,
        change: impl FnOnce(&mut Self) -> Result<T, Error>,
    ) -> Result<T, Error> {
        let _held = RegistryLock::take(&self.dir)?;
        let (fresh, on_disk) = Self::read(&self.dir)?;
        self.data = fresh.data;
        let before = self.data.clone();
        let changed = change(self);
        let outcome = changed.and_then(|value| self.save(Some(&on_disk)).map(|()| value));
        if outcome.is_err() {
            self.data = before;
        }
        outcome
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
    /// New records are saved together in one transaction, so a disabled harness or
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
        self.transact(|reg| {
            if let Some(task_ref) = &reg.data.helper.task_ref {
                if reg
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
                reg.data.helper.task_ref.clone(),
                reg.data.helper.manager_ref.clone(),
            ) {
                return Ok((task_ref, manager_ref, false));
            }
            let task_ref = match reg.data.helper.task_ref.clone() {
                Some(task_ref) => task_ref,
                None => {
                    reg.insert_task_on_server("taskai-helper", workspace, "local", source.clone())?
                }
            };
            let manager_ref =
                reg.insert_session(&task_ref, None, "TaskAI Helper", harness, workspace, source)?;
            reg.data.helper = HelperSettings {
                preferred_harness: Some(harness.into()),
                task_ref: Some(task_ref.clone()),
                manager_ref: Some(manager_ref.clone()),
            };
            Ok((task_ref, manager_ref, true))
        })
    }

    fn heal_helper(&mut self) {
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
    }

    pub fn create_server(
        &mut self,
        name: &str,
        target: ConnectionTarget,
        repository_root: Option<PathBuf>,
        source: Source,
    ) -> Result<String, Error> {
        self.transact(|reg| reg.insert_server(name, target, repository_root, source))
    }

    fn insert_server(
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
        self.transact(|reg| {
            let server = reg
                .data
                .servers
                .iter_mut()
                .find(|server| server.server_id == id)
                .ok_or_else(|| Error::NotFound(format!("server {id}")))?;
            server.name = name;
            server.repository_root = repository_root;
            reg.note("update_server", id, source);
            Ok(())
        })
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
        self.transact(|reg| {
            let server = reg
                .data
                .servers
                .iter_mut()
                .find(|server| server.server_id == id)
                .ok_or_else(|| Error::NotFound(format!("server {id}")))?;
            server.harnesses.insert(hkey.into(), settings);
            reg.note("set_server_harness", id, source);
            Ok(())
        })
    }

    /// Archiving hides a profile from new task creation. It does not destroy,
    /// detach, retarget, or otherwise mutate any task or session on that server.
    pub fn archive_server(&mut self, id: &str, source: Source) -> Result<(), Error> {
        if id == "local" {
            return Err(Error::Invalid("This computer cannot be archived".into()));
        }
        self.transact(|reg| {
            let server = reg
                .data
                .servers
                .iter_mut()
                .find(|server| server.server_id == id)
                .ok_or_else(|| Error::NotFound(format!("server {id}")))?;
            if server.archived {
                return Ok(());
            }
            server.archived = true;
            reg.note("archive_server", id, source);
            Ok(())
        })
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
        self.transact(|reg| {
            let server = reg
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
            reg.note("bind_server_setup", id, Source::HumanUi);
            Ok(runtime)
        })
    }

    fn next_server_id(&mut self) -> String {
        self.data.next_server += 1;
        format!("server-{}", self.data.next_server)
    }

    fn migrate_servers(&mut self) {
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
        }
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
        self.transact(|reg| {
            let active = reg
                .data
                .servers
                .iter()
                .find(|server| server.target == connection && !server.archived)
                .map(|server| server.server_id.clone());
            let server_id = match active {
                Some(server_id) => server_id,
                None => {
                    let server_name = match &connection {
                        ConnectionTarget::Local => "This computer",
                        ConnectionTarget::Ssh { host } => host.as_str(),
                    }
                    .to_string();
                    reg.insert_server(&server_name, connection, None, source.clone())?
                }
            };
            reg.insert_task_on_server(name, repo, &server_id, source)
        })
    }

    pub fn create_task_on_server(
        &mut self,
        name: &str,
        repo: &Path,
        server_id: &str,
        source: Source,
    ) -> Result<String, Error> {
        self.transact(|reg| reg.insert_task_on_server(name, repo, server_id, source))
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
        self.transact(|reg| reg.insert_session(task, parent, name, harness, workspace, source))
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
        let address = derive_address(harness, name, &session_ref, &self.addresses_in(task));
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
            address,
            messaging: None,
        });
        self.note("create_session", &session_ref, source);
        Ok(session_ref)
    }

    /// Gives each session saved without an address one, in record order, by the same rule as a
    /// new session. An address already saved is kept exactly.
    fn assign_addresses(&mut self) {
        for index in 0..self.data.sessions.len() {
            let session = &self.data.sessions[index];
            if !session.address.is_empty() {
                continue;
            }
            let address = derive_address(
                &session.harness,
                &session.name,
                &session.session_ref,
                &self.addresses_in(&session.task_ref),
            );
            self.data.sessions[index].address = address;
        }
    }

    /// The addresses already held by sessions of `task`.
    fn addresses_in(&self, task: &str) -> Vec<String> {
        self.data
            .sessions
            .iter()
            .filter(|s| s.task_ref == task && !s.address.is_empty())
            .map(|s| s.address.clone())
            .collect()
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

    /// The routable sessions of `task_ref`, as the address book lists them: `name` is the address.
    pub fn live_sessions(&self, task_ref: &str) -> Vec<MailPeer> {
        self.sessions_of(task_ref)
            .into_iter()
            .filter_map(|session| {
                let entry = session.live_mailbox()?;
                Some(MailPeer {
                    session_ref: session.session_ref.clone(),
                    task_ref: session.task_ref.clone(),
                    name: session.address.clone(),
                    harness: session.harness.clone(),
                    workspace: session.workspace.clone(),
                    pid: entry.pid,
                    proc_start: entry.proc_start.clone(),
                    status: entry.status,
                    waiting_for: entry.waiting_for.clone(),
                    mailbox: entry.mailbox.clone(),
                    features: entry.features.clone(),
                    mode: entry.mode,
                })
            })
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
        self.transact(|reg| {
            let s = reg
                .data
                .sessions
                .iter_mut()
                .find(|s| s.session_ref == r)
                .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
            s.state = state;
            Ok(())
        })
    }

    pub fn bind_provider(&mut self, r: &str, mapping: ProviderMapping) -> Result<(), Error> {
        self.transact(|reg| {
            let s = reg
                .data
                .sessions
                .iter_mut()
                .find(|s| s.session_ref == r)
                .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
            s.provider = Some(mapping);
            Ok(())
        })
    }

    /// Chooses one provider ID before spawning a local tmux-backed native session. The
    /// candidate and any existing winner must belong to this session's current harness and
    /// runtime. Competing valid candidates return the saved winner without changing it;
    /// missing/foreign/malformed identities fail without writing. The adapter still validates
    /// its native ID format. This choice and persistence share one fresh-disk transaction.
    pub fn bind_provider_if_absent(
        &mut self,
        r: &str,
        mapping: ProviderMapping,
    ) -> Result<ProviderMapping, Error> {
        self.transact(|reg| {
            let session = reg
                .data
                .sessions
                .iter_mut()
                .find(|session| session.session_ref == r)
                .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
            let runtime = session.runtime.as_ref().ok_or_else(|| {
                Error::Invalid(format!("session {r} has no runtime for provider binding"))
            })?;
            let task = reg
                .data
                .tasks
                .iter()
                .find(|task| task.task_ref == session.task_ref)
                .ok_or_else(|| Error::NotFound(format!("task {}", session.task_ref)))?;
            if runtime.target != ConnectionTarget::Local
                || task.connection != ConnectionTarget::Local
                || runtime.session != r
            {
                return Err(Error::Invalid(
                    "provider binding requires this session's exact local runtime".into(),
                ));
            }
            let expected_runtime = format!("tmux:{}:{}", runtime.private_server, runtime.session);
            let valid = |binding: &ProviderMapping| {
                binding.provider == session.harness
                    && binding.runtime_id == expected_runtime
                    && !binding.provider_session_id.trim().is_empty()
                    && !binding.connector_version.trim().is_empty()
            };
            if !valid(&mapping) {
                return Err(Error::Invalid(
                    "candidate provider binding is incomplete or belongs to another harness/runtime"
                        .into(),
                ));
            }
            if let Some(existing) = &session.provider {
                if !valid(existing) {
                    return Err(Error::Invalid(
                        "saved provider binding is incomplete or belongs to another harness/runtime"
                            .into(),
                    ));
                }
                return Ok(existing.clone());
            }
            session.provider = Some(mapping.clone());
            Ok(mapping)
        })
    }

    /// Drops a provider binding proven unresumable — reconciled from native truth.
    pub fn clear_provider(&mut self, r: &str) -> Result<(), Error> {
        self.transact(|reg| {
            let s = reg
                .data
                .sessions
                .iter_mut()
                .find(|s| s.session_ref == r)
                .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
            s.provider = None;
            Ok(())
        })
    }

    /// Records the durable tmux process location for one agent session.
    ///
    /// The exact tmux session name is the stable TaskAI session ref, never a user-entered
    /// Task, Manager, or role name. Its target is copied from the owning Task.
    pub fn bind_runtime(&mut self, r: &str, private_server: &str) -> Result<(), Error> {
        validate_internal_name(private_server, "tmux private server")?;
        self.transact(|reg| {
            let task_ref = reg
                .session(r)
                .ok_or_else(|| Error::NotFound(format!("session {r}")))?
                .task_ref
                .clone();
            let target = reg
                .task(&task_ref)
                .ok_or_else(|| Error::NotFound(format!("task {task_ref}")))?
                .connection
                .clone();
            target.validate()?;
            let s = reg
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
            Ok(())
        })
    }

    /// Persist one task-owned utility shell before creating its runtime. Repeated
    /// opens reuse the same mapping; agent refs, counts and hierarchy are untouched.
    pub fn bind_terminal(
        &mut self,
        task_ref: &str,
        private_server: &str,
    ) -> Result<TmuxRuntimeLocator, Error> {
        validate_internal_name(private_server, "terminal tmux server")?;
        self.transact(|reg| {
            let task = reg
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
            reg.note("bind_terminal", task_ref, Source::HumanUi);
            Ok(runtime)
        })
    }

    /// Removes only the tmux process locator, leaving provider identity untouched.
    pub fn clear_runtime(&mut self, r: &str) -> Result<(), Error> {
        self.transact(|reg| {
            let s = reg
                .data
                .sessions
                .iter_mut()
                .find(|s| s.session_ref == r)
                .ok_or_else(|| Error::NotFound(format!("session {r}")))?;
            s.runtime = None;
            Ok(())
        })
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
        self.transact(|reg| {
            let removed = reg.blast(item)?;
            reg.data
                .sessions
                .retain(|s| !removed.contains(&s.session_ref));
            if let Item::Task(r) = item {
                reg.data.tasks.retain(|t| t.task_ref != *r);
            }
            reg.heal_helper();
            let label = match item {
                Item::Task(r) => r.clone(),
                Item::Session(r) => r.clone(),
            };
            reg.note("destroy", &label, source);
            Ok(removed)
        })
    }
}

/// One task's address book over TaskAI's registry directory: the `mail::Directory` a bridge's
/// mailbox resolves names in and registers into. It keeps no copy; every call reads or changes
/// the registry as it is on disk.
pub struct AddressBook {
    dir: PathBuf,
    task_ref: String,
}

impl AddressBook {
    pub fn new(registry_dir: impl Into<PathBuf>, task_ref: impl Into<String>) -> Self {
        Self {
            dir: registry_dir.into(),
            task_ref: task_ref.into(),
        }
    }

    /// A transaction on the registry as it is on disk. A missing registry fails the read under
    /// the lock, so it is never taken for an empty one.
    fn transact<T>(
        &self,
        change: impl FnOnce(&mut Registry) -> Result<T, Error>,
    ) -> Result<T, Error> {
        let mut registry = Registry {
            dir: self.dir.clone(),
            data: Data::default(),
        };
        registry.transact(change)
    }

    /// Clears the mailbox entry of a session of this book's task, but only while it is exactly the
    /// one recorded for `pid`, `proc_start` and `mailbox`; true when it was cleared. Any other
    /// entry, such as a newer registration, stays and nothing is written. The session, its runtime
    /// and provider mapping stay, and no file is touched. Unlike `unregister`, any process may
    /// clear, so an observer can retract a native session's entry it registered.
    pub fn clear_if_matches(
        &self,
        session_ref: &str,
        pid: u32,
        proc_start: &str,
        mailbox: &Path,
    ) -> Result<bool, Error> {
        self.transact(|registry| {
            let matching = registry.data.sessions.iter_mut().find(|s| {
                s.session_ref == session_ref
                    && s.task_ref == self.task_ref
                    && s.messaging.as_ref().is_some_and(|entry| {
                        entry.pid == pid
                            && entry.proc_start == proc_start
                            && entry.mailbox == mailbox
                    })
            });
            let Some(session) = matching else {
                return Ok(false);
            };
            session.messaging = None;
            Ok(true)
        })
    }

    /// The entry this process registered for a session of this book's task.
    fn own_entry<'a>(
        &self,
        registry: &'a mut Registry,
        session_ref: &str,
    ) -> Result<&'a mut MessagingEntry, Error> {
        registry
            .data
            .sessions
            .iter_mut()
            .find(|s| s.session_ref == session_ref && s.task_ref == self.task_ref)
            .and_then(|s| s.messaging.as_mut())
            .filter(|entry| entry.pid == std::process::id())
            .ok_or_else(|| {
                Error::NotFound(format!(
                    "no mailbox of this process for session {session_ref}"
                ))
            })
    }
}

impl Directory for AddressBook {
    fn live_sessions(&self) -> Result<Vec<MailPeer>, Error> {
        let (registry, _) = Registry::read(&self.dir)?;
        Ok(registry.live_sessions(&self.task_ref))
    }

    /// Records the bridge's mailbox on its session. The session must be in this book's task under
    /// the entry's address and harness; a live entry of another process is never replaced.
    fn register(&self, entry: &MailPeer) -> Result<(), Error> {
        self.transact(|registry| {
            let session = registry
                .data
                .sessions
                .iter_mut()
                .find(|s| s.session_ref == entry.session_ref)
                .ok_or_else(|| Error::NotFound(format!("session {}", entry.session_ref)))?;
            if session.task_ref != self.task_ref || entry.task_ref != self.task_ref {
                return Err(Error::Invalid(format!(
                    "session {} is not in task {}",
                    entry.session_ref, self.task_ref
                )));
            }
            if session.address != entry.name || session.harness != entry.harness {
                return Err(Error::Invalid(format!(
                    "session {} is {} ({}), not {} ({})",
                    session.session_ref,
                    session.address,
                    session.harness,
                    entry.name,
                    entry.harness
                )));
            }
            if let Some(held) = session.live_mailbox() {
                if held.pid != entry.pid {
                    return Err(Error::Invalid(format!(
                        "session {} already has a live mailbox in process {}",
                        session.session_ref, held.pid
                    )));
                }
            }
            session.messaging = Some(MessagingEntry {
                pid: entry.pid,
                proc_start: entry.proc_start.clone(),
                mailbox: entry.mailbox.clone(),
                features: entry.features.clone(),
                mode: entry.mode,
                status: entry.status,
                waiting_for: entry
                    .waiting_for
                    .clone()
                    .filter(|_| entry.status == Activity::Waiting),
            });
            Ok(())
        })
    }

    /// Publishes this process's session status; `waiting_for` is kept only while waiting.
    fn set_status(
        &self,
        session_ref: &str,
        status: Activity,
        waiting_for: Option<&str>,
    ) -> Result<(), Error> {
        self.transact(|registry| {
            let entry = self.own_entry(registry, session_ref)?;
            entry.status = status;
            entry.waiting_for = waiting_for
                .filter(|_| status == Activity::Waiting)
                .map(str::to_string);
            Ok(())
        })
    }

    /// Removes this process's mailbox entry; the session, its address and topology stay.
    fn unregister(&self, session_ref: &str) -> Result<(), Error> {
        self.transact(|registry| {
            let session = registry
                .data
                .sessions
                .iter_mut()
                .find(|s| s.session_ref == session_ref && s.task_ref == self.task_ref);
            if let Some(session) = session {
                let ours = session
                    .messaging
                    .as_ref()
                    .is_some_and(|entry| entry.pid == std::process::id());
                if ours {
                    session.messaging = None;
                }
            }
            Ok(())
        })
    }
}

/// Exclusive hold on a registry directory while one change reads, applies and saves
/// `state.json`, so the changes of every process apply one after another to the state on disk.
struct RegistryLock {
    _file: fs::File,
}

impl RegistryLock {
    fn take(dir: &Path) -> Result<Self, Error> {
        let file = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .open(dir.join("state.lock"))?;
        loop {
            match flock(&file, FlockOperation::LockExclusive) {
                Ok(()) => return Ok(Self { _file: file }),
                Err(Errno::INTR) => {}
                Err(error) => return Err(std::io::Error::from(error).into()),
            }
        }
    }
}

/// A messaging address for a session, unused among `taken`: `<harness>-<name>` as lowercase ASCII
/// letters, digits and single dashes, without repeating a harness prefix the name already has, cut
/// to 64 characters. A name with no ASCII letter or digit gives `<harness>-<ref>`. On a collision
/// `-<ref>` is appended, then a counter, with the base cut to fit.
fn derive_address(harness: &str, name: &str, session_ref: &str, taken: &[String]) -> String {
    let harness = slug(harness);
    let name = slug(name);
    let session_ref = slug(session_ref);
    let base = if name.is_empty() {
        joined(&harness, &session_ref)
    } else if name == harness || name.starts_with(&format!("{harness}-")) {
        name
    } else {
        joined(&harness, &name)
    };
    let unused = |candidate: &String| !taken.contains(candidate);
    let whole = cut(&base, MAX_ADDRESS_CHARS);
    if unused(&whole) {
        return whole;
    }
    (1..)
        .map(|attempt: u32| {
            let suffix = match attempt {
                1 => format!("-{session_ref}"),
                _ => format!("-{session_ref}-{attempt}"),
            };
            let room = MAX_ADDRESS_CHARS.saturating_sub(suffix.len());
            format!("{}{suffix}", cut(&base, room))
        })
        .find(unused)
        .expect("counters run until an address is unused")
}

/// Lowercase ASCII letters and digits of `text`, every other run of characters as one dash,
/// with no dash at either end.
fn slug(text: &str) -> String {
    let mut slug = String::new();
    for c in text.chars() {
        if c.is_ascii_alphanumeric() {
            slug.push(c.to_ascii_lowercase());
        } else if !slug.is_empty() && !slug.ends_with('-') {
            slug.push('-');
        }
    }
    if slug.ends_with('-') {
        slug.pop();
    }
    slug
}

fn joined(prefix: &str, rest: &str) -> String {
    match (prefix.is_empty(), rest.is_empty()) {
        (true, _) => rest.into(),
        (_, true) => prefix.into(),
        _ => format!("{prefix}-{rest}"),
    }
}

/// The first `room` characters of an ASCII slug, without a trailing dash.
fn cut(slug: &str, room: usize) -> String {
    slug[..slug.len().min(room)].trim_end_matches('-').into()
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
