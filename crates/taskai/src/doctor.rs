// ABOUTME: Read-only readiness per harness — installed version, saved sign-in and the messaging
// ABOUTME: adapter's gate — behind `taskai doctor`, Settings and the launch gate. Never a model call.

use std::{
    ffi::OsString,
    fmt::{self, Write as _},
    io::{self, Read as _},
    os::unix::fs::OpenOptionsExt,
    path::{Path, PathBuf},
    time::Duration,
};

use taskai_connectors::{
    claude::{
        drift::{drift_check, TESTED_VERSION as CLAUDE_TESTED},
        ClaudeConnector,
    },
    codex::{daemon::DaemonClient, CodexConnector},
    grok::GrokConnector,
    opencode::OpenCodeConnector,
};
use taskai_core::registry::{ConnectionTarget, ServerProfile};

use crate::remote_probes::{self, AuthState, HarnessAvailability};

/// The Codex daemon version the adapter was tested against, as its `initialize` User-Agent names it.
pub const CODEX_TESTED_DAEMON: &str = "0.167.1";
/// The Grok release the adapter was tested against.
pub const GROK_TESTED: &str = "1.0.77";
/// The exact reason doctor gives for a workspace Grok has not trusted.
pub const GROK_UNTRUSTED: &str = "workspace not trusted by Grok";
/// Harnesses with a messaging adapter, in report order.
pub const MESSAGING_HARNESSES: [&str; 3] = ["claude", "codex", "grok"];

const DAEMON_TIMEOUT: Duration = Duration::from_secs(3);
/// Largest Grok trust store doctor reads; the installed store is a few hundred bytes per folder.
pub const TRUST_STORE_LIMIT: usize = 1 << 20;
/// Largest recorded TaskAI state doctor reads for the local profile.
pub const STATE_LIMIT: usize = 8 << 20;
const USAGE: &str = "usage: taskai doctor [--json] [--workspace <path>]";

/// One read-only check's outcome; `Unverified` states why no evidence was available.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Check {
    Ready(String),
    NotReady { reason: String, remedy: String },
    Unverified(String),
}

impl Check {
    pub fn is_ready(&self) -> bool {
        matches!(self, Check::Ready(_))
    }

    pub fn status(&self) -> &'static str {
        match self {
            Check::Ready(_) => "ready",
            Check::NotReady { .. } => "not ready",
            Check::Unverified(_) => "unverified",
        }
    }

    /// One line for Settings and the doctor text: the status, then what was seen and what to do.
    pub fn label(&self) -> String {
        match self {
            Check::Ready(detail) => format!("Ready · {detail}"),
            Check::NotReady { reason, remedy } => format!("Not ready · {reason} · {remedy}"),
            Check::Unverified(reason) => format!("Unverified · {reason}"),
        }
    }

    fn not_ready(reason: impl Into<String>, remedy: impl Into<String>) -> Check {
        Check::NotReady {
            reason: reason.into(),
            remedy: remedy.into(),
        }
    }

    fn json(&self) -> serde_json::Value {
        let mut value = serde_json::json!({"status": self.status()});
        match self {
            Check::Ready(detail) | Check::Unverified(detail) => {
                value["detail"] = serde_json::Value::String(detail.clone());
            }
            Check::NotReady { reason, remedy } => {
                value["detail"] = serde_json::Value::String(reason.clone());
                value["remedy"] = serde_json::Value::String(remedy.clone());
            }
        }
        value
    }
}

/// Native state the checks stat, read or dial; tests point every path at a private fixture.
/// A home that cannot be resolved carries the reason instead of a guess.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Paths {
    /// The shared Codex daemon's control socket under `CODEX_HOME` or `~/.codex`.
    pub codex_control_socket: Result<PathBuf, String>,
    /// Grok's home, `GROK_HOME` or `~/.grok`: `auth.json` is stat'ed only; `trusted_folders.toml`
    /// is read within a bound.
    pub grok_home: Result<PathBuf, String>,
}

impl Paths {
    pub fn from_env() -> Paths {
        let home = std::env::var_os("HOME");
        Paths {
            codex_control_socket: native_home(
                "CODEX_HOME",
                std::env::var_os("CODEX_HOME"),
                home.as_deref(),
                ".codex",
            )
            .map(|codex| {
                codex
                    .join("app-server-control")
                    .join("app-server-control.sock")
            }),
            grok_home: native_home(
                "GROK_HOME",
                std::env::var_os("GROK_HOME"),
                home.as_deref(),
                ".grok",
            ),
        }
    }

    /// Every path under one private root, so nothing real is stat'ed or dialled.
    pub fn under(root: &Path) -> Paths {
        Paths {
            codex_control_socket: Ok(root.join("codex-control.sock")),
            grok_home: Ok(root.join("grok")),
        }
    }
}

/// A native CLI's home as that CLI resolves it: an absolute `var` when set, else `default_dir`
/// under an absolute `HOME`. An empty or relative override, or an unusable `HOME`, is an explicit
/// error, never a fallback to another directory.
fn native_home(
    var: &str,
    override_value: Option<OsString>,
    home: Option<&std::ffi::OsStr>,
    default_dir: &str,
) -> Result<PathBuf, String> {
    if let Some(value) = override_value {
        if value.is_empty() {
            return Err(format!(
                "{var} is set but empty; unset it or point it at an absolute directory"
            ));
        }
        let path = PathBuf::from(&value);
        if !path.is_absolute() {
            return Err(format!(
                "{var} is not an absolute directory ({}); the native CLI resolves its home from it, so doctor does not guess",
                path.display()
            ));
        }
        return Ok(path);
    }
    let home = home.ok_or_else(|| {
        format!("HOME is unset, so the native home {default_dir} cannot be resolved; set {var} to an absolute directory")
    })?;
    let home = PathBuf::from(home);
    if !home.is_absolute() {
        return Err(format!(
            "HOME is not an absolute directory ({}); set {var} to an absolute directory",
            home.display()
        ));
    }
    Ok(home.join(default_dir))
}

/// Why a required file could not be read; never carries a byte of its content.
#[derive(Debug, PartialEq, Eq)]
enum FileFault {
    Missing,
    NotRegular(&'static str),
    TooLarge(usize),
    Unreadable(io::ErrorKind),
}

impl fmt::Display for FileFault {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            FileFault::Missing => write!(f, "is missing"),
            FileFault::NotRegular(kind) => write!(f, "is not a regular file ({kind})"),
            FileFault::TooLarge(limit) => write!(f, "is larger than {limit} bytes"),
            FileFault::Unreadable(kind) => write!(f, "could not be read ({kind:?})"),
        }
    }
}

/// Reads a regular file of at most `limit` bytes. A FIFO, socket, directory or device is refused
/// before it is opened, so nothing blocks; a larger file is refused unread.
fn read_regular_file(path: &Path, limit: usize) -> Result<Vec<u8>, FileFault> {
    read_regular_file_with(path, limit, || {})
}

fn read_regular_file_with(
    path: &Path,
    limit: usize,
    before_open: impl FnOnce(),
) -> Result<Vec<u8>, FileFault> {
    let kind = |meta: &std::fs::Metadata| -> Option<&'static str> {
        use std::os::unix::fs::FileTypeExt;
        let file_type = meta.file_type();
        if file_type.is_file() {
            None
        } else if file_type.is_dir() {
            Some("directory")
        } else if file_type.is_fifo() {
            Some("fifo")
        } else if file_type.is_socket() {
            Some("socket")
        } else {
            Some("special file")
        }
    };
    let meta = std::fs::metadata(path).map_err(|error| match error.kind() {
        io::ErrorKind::NotFound => FileFault::Missing,
        other => FileFault::Unreadable(other),
    })?;
    if let Some(found) = kind(&meta) {
        return Err(FileFault::NotRegular(found));
    }
    if meta.len() > limit as u64 {
        return Err(FileFault::TooLarge(limit));
    }
    before_open();
    // A path can change after the first stat. Nonblocking open prevents a replacement FIFO
    // from waiting for a writer; the descriptor must still prove it is a regular file.
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
        .map_err(|error| FileFault::Unreadable(error.kind()))?;
    let opened = file
        .metadata()
        .map_err(|error| FileFault::Unreadable(error.kind()))?;
    if let Some(found) = kind(&opened) {
        return Err(FileFault::NotRegular(found));
    }
    let mut bytes = Vec::new();
    file.take(limit as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(|error| FileFault::Unreadable(error.kind()))?;
    if bytes.len() > limit {
        return Err(FileFault::TooLarge(limit));
    }
    Ok(bytes)
}

/// The executable a launch of `hkey` would run: the configured override, else the connector's
/// default (`TASKAI_CLAUDE_BIN` for Claude). `~/` and absolute paths expand in the launch command.
pub fn configured_executable(hkey: &str, configured: Option<&str>) -> String {
    if let Some(executable) = configured {
        return executable.to_string();
    }
    match hkey {
        "claude" => ClaudeConnector::default().bin,
        "codex" => CodexConnector::default().bin,
        "grok" => GrokConnector::default().bin,
        "opencode" => OpenCodeConnector::default().bin,
        other => other.to_string(),
    }
}

/// Version, saved sign-in and the adapter gate for one harness on this machine, each bounded.
/// Launch availability (`is_ready`) never depends on the gate.
pub fn check_harness(hkey: &str, executable: &str, paths: &Paths) -> HarnessAvailability {
    remote_probes::check_local(hkey, executable, paths)
}

/// The adapter gate alone, for a caller that already holds the version; `None` for a harness
/// without a messaging adapter.
pub fn messaging_check(hkey: &str, version: Option<&str>, paths: &Paths) -> Option<Check> {
    Some(match hkey {
        "claude" => claude_gate(version),
        "codex" => match &paths.codex_control_socket {
            Ok(socket) => codex_gate(socket),
            Err(reason) => Check::Unverified(reason.clone()),
        },
        "grok" => match &paths.grok_home {
            Ok(_) => grok_gate(version),
            Err(reason) => Check::Unverified(reason.clone()),
        },
        _ => return None,
    })
}

/// Checks that need a workspace: Grok's folder trust. Never trusts anything itself.
pub fn workspace_check(hkey: &str, workspace: &Path, paths: &Paths) -> Option<Check> {
    (hkey == "grok").then(|| match &paths.grok_home {
        Ok(home) => grok_trust(workspace, &home.join("trusted_folders.toml")),
        Err(reason) => Check::Unverified(reason.clone()),
    })
}

/// Grok's saved login from its credentials file's metadata alone: a regular file (through a
/// symlink too) is present, a missing entry is signed out, and a dangling symlink or any other
/// metadata error stays unknown. The contents are never opened.
pub fn grok_saved_login(grok_home: &Path) -> AuthState {
    let file = grok_home.join("auth.json");
    match std::fs::symlink_metadata(&file) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => AuthState::SignedOut,
        Err(_) => AuthState::Unknown,
        Ok(meta) if meta.file_type().is_symlink() => match std::fs::metadata(&file) {
            Ok(target) if target.is_file() => AuthState::CredentialsPresent,
            _ => AuthState::Unknown,
        },
        Ok(meta) if meta.is_file() => AuthState::CredentialsPresent,
        Ok(_) => AuthState::Unknown,
    }
}

/// The common Claude check: the installed version is the tested release and the sample envelope
/// round-trips. Owned native cards are validated per session by the observer and the mirror.
fn claude_gate(version: Option<&str>) -> Check {
    let drift = drift_check(version, &[]);
    if !drift.enabled {
        return Check::not_ready(
            drift.reasons.join("; "),
            format!(
                "Install Claude Code {CLAUDE_TESTED}, or re-verify the Claude adapter against the \
                 installed version before enabling it"
            ),
        );
    }
    Check::Ready(format!(
        "Claude Code {CLAUDE_TESTED} is the tested release and the sample envelope round-trips; \
         checked here: version and envelope grammar only; each session's own card is validated \
         by its observer and mirror at launch"
    ))
}

fn codex_gate(socket: &Path) -> Check {
    let shown = socket.display();
    match std::fs::metadata(socket) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            return Check::not_ready(
                format!("Codex daemon socket is absent at {shown}"),
                "Run codex once; its daemon then listens at that socket for TaskAI's bridge",
            )
        }
        Err(error) => return Check::not_ready(
            format!("Codex daemon socket at {shown} could not be examined: {error}"),
            "Check the permissions of Codex's app-server-control directory, then run codex once",
        ),
        Ok(_) => {}
    }
    let handshake = DaemonClient::connect(socket, DAEMON_TIMEOUT)
        .and_then(|mut client| client.initialize(DAEMON_TIMEOUT));
    match handshake {
        Err(error) => Check::not_ready(
            format!("Codex daemon refused the handshake: {error}"),
            format!("Restart Codex so a fresh daemon listens at {shown}"),
        ),
        Ok(init) => match init.daemon_version.as_deref() {
            Some(CODEX_TESTED_DAEMON) => Check::Ready(format!(
                "daemon codex-tui/{CODEX_TESTED_DAEMON} answered initialize and its User-Agent \
                 names the tested daemon version; checked here: the handshake only, no thread, \
                 turn or interrupt"
            )),
            Some(other) => Check::not_ready(
                format!("Codex daemon {other} is not the tested {CODEX_TESTED_DAEMON}"),
                format!(
                    "Re-verify the Codex adapter against this daemon before messaging; it is \
                     tested against {CODEX_TESTED_DAEMON} only"
                ),
            ),
            None => Check::not_ready(
                "Codex daemon reported no version in its User-Agent",
                "Re-verify the Codex adapter against this daemon before messaging",
            ),
        },
    }
}

fn grok_gate(version: Option<&str>) -> Check {
    match version {
        Some(GROK_TESTED) => Check::Ready(format!(
            "Grok {GROK_TESTED} is the tested release; each session's leader must register \
             TaskAI's client with yolo_mode=false, enforced at launch; each workspace must be \
             trusted by Grok (doctor --workspace checks one)"
        )),
        Some(other) => Check::not_ready(
            format!("Grok {other} is not the tested {GROK_TESTED}"),
            format!(
                "Install Grok {GROK_TESTED}, or re-verify the Grok adapter (leader flags, MCP \
                 trust gate) against the installed version"
            ),
        ),
        None => Check::not_ready(
            "Grok's version is unknown",
            format!("Install Grok {GROK_TESTED} or set its executable in Settings"),
        ),
    }
}

/// Trust is exact to the folder as Grok recorded it, in the given or the canonical spelling;
/// anything else, including a missing store, fails closed. A store that cannot be read or parsed
/// is unverified, not untrusted.
fn grok_trust(workspace: &Path, store: &Path) -> Check {
    let remedy = format!(
        "Open grok in {} once and accept its trust prompt; TaskAI never trusts a folder for you",
        workspace.display()
    );
    let bytes = match read_regular_file(store, TRUST_STORE_LIMIT) {
        Ok(bytes) => bytes,
        Err(FileFault::Missing) => return Check::not_ready(GROK_UNTRUSTED, remedy),
        Err(fault) => {
            return Check::Unverified(format!("Grok's trust store {} {fault}", store.display()))
        }
    };
    // Parser errors would quote the store's text; the reason names the file only.
    let Ok(doc) = std::str::from_utf8(&bytes)
        .ok()
        .and_then(|text| text.parse::<toml_edit::DocumentMut>().ok())
        .ok_or(())
    else {
        return Check::Unverified(format!(
            "Grok's trust store {} is not valid TOML",
            store.display()
        ));
    };
    let mut spellings = vec![workspace.to_path_buf()];
    if let Ok(canonical) = workspace.canonicalize() {
        if !spellings.contains(&canonical) {
            spellings.push(canonical);
        }
    }
    let trusted = spellings.iter().any(|spelling| {
        doc.get("folders")
            .and_then(|folders| folders.get(spelling.display().to_string()))
            .and_then(|folder| folder.get("trusted"))
            .and_then(|value| value.as_bool())
            == Some(true)
    });
    if trusted {
        Check::Ready(format!("Grok trusts {}", workspace.display()))
    } else {
        Check::not_ready(GROK_UNTRUSTED, remedy)
    }
}

/// One harness's rows in the report: the executable that was checked and what it showed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HarnessReport {
    pub executable: String,
    pub availability: HarnessAvailability,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Report {
    pub harnesses: Vec<HarnessReport>,
    pub workspace: Option<(PathBuf, Vec<(String, Check)>)>,
}

/// The three messaging harnesses with the profile's configured executables, each checked in its
/// own bounded thread so one hanging CLI leaves the others reported.
pub fn report(profile: &ServerProfile, paths: &Paths, workspace: Option<&Path>) -> Report {
    let harnesses = std::thread::scope(|scope| {
        MESSAGING_HARNESSES
            .iter()
            .map(|hkey| {
                let executable = configured_executable(
                    hkey,
                    profile
                        .harnesses
                        .get(*hkey)
                        .and_then(|settings| settings.executable.as_deref()),
                );
                scope.spawn(move || HarnessReport {
                    availability: check_harness(hkey, &executable, paths),
                    executable,
                })
            })
            .collect::<Vec<_>>()
            .into_iter()
            .map(|handle| handle.join().expect("a readiness check panicked"))
            .collect()
    });
    let workspace = workspace.map(|workspace| {
        let checks = MESSAGING_HARNESSES
            .iter()
            .filter_map(|hkey| {
                workspace_check(hkey, workspace, paths).map(|check| (hkey.to_string(), check))
            })
            .collect();
        (workspace.to_path_buf(), checks)
    });
    Report {
        harnesses,
        workspace,
    }
}

impl HarnessReport {
    /// Launchable, with a saved login (a credentials file counts, still unverified) and a ready
    /// messaging gate.
    pub fn ready(&self) -> bool {
        let seen = &self.availability;
        seen.is_ready()
            && seen.auth.saved_login()
            && seen.messaging.as_ref().is_some_and(Check::is_ready)
    }
}

impl Report {
    /// Every adapter row is ready and every workspace check passed.
    pub fn green(&self) -> bool {
        self.harnesses.iter().all(HarnessReport::ready)
            && self
                .workspace
                .as_ref()
                .is_none_or(|(_, checks)| checks.iter().all(|(_, check)| check.is_ready()))
    }

    fn not_ready_count(&self) -> usize {
        self.harnesses.iter().filter(|row| !row.ready()).count()
            + self
                .workspace
                .iter()
                .flat_map(|(_, checks)| checks.iter())
                .filter(|(_, check)| !check.is_ready())
                .count()
    }

    pub fn text(&self) -> String {
        let mut out = String::from("TaskAI doctor\n");
        for row in &self.harnesses {
            let seen = &row.availability;
            let _ = writeln!(
                out,
                "{} · {}",
                crate::app::harness_title(&seen.hkey),
                row.executable
            );
            let _ = writeln!(out, "  installed   {}", installed_line(seen));
            let _ = writeln!(out, "  sign-in     {}", seen.authentication_status());
            let _ = writeln!(
                out,
                "  messaging   {}",
                seen.messaging
                    .as_ref()
                    .map(Check::label)
                    .unwrap_or_else(|| "No adapter".into())
            );
        }
        if let Some((workspace, checks)) = &self.workspace {
            let _ = writeln!(out, "workspace {}", workspace.display());
            for (hkey, check) in checks {
                let _ = writeln!(out, "  {hkey:<11} {}", check.label());
            }
        }
        if self.green() {
            out.push_str("doctor: green\n");
        } else {
            let _ = writeln!(out, "doctor: red · {} not ready", self.not_ready_count());
        }
        out
    }

    pub fn json(&self) -> serde_json::Value {
        let harnesses: Vec<serde_json::Value> = self
            .harnesses
            .iter()
            .map(|row| {
                let seen = &row.availability;
                serde_json::json!({
                    "hkey": seen.hkey,
                    "title": crate::app::harness_title(&seen.hkey),
                    "executable": row.executable,
                    "version": seen.version,
                    "launch": {"available": seen.is_ready(), "reason": seen.unavailable},
                    "sign_in": sign_in_word(seen.auth),
                    "messaging": seen.messaging.as_ref().map(Check::json),
                })
            })
            .collect();
        let workspace = self.workspace.as_ref().map(|(workspace, checks)| {
            serde_json::json!({
                "path": workspace,
                "checks": checks.iter().map(|(hkey, check)| {
                    let mut value = check.json();
                    value["hkey"] = serde_json::Value::String(hkey.clone());
                    value
                }).collect::<Vec<_>>(),
            })
        });
        serde_json::json!({
            "green": self.green(),
            "harnesses": harnesses,
            "workspace": workspace,
        })
    }
}

fn installed_line(seen: &HarnessAvailability) -> String {
    match (&seen.version, &seen.unavailable) {
        (Some(version), None) => version.clone(),
        (Some(version), Some(reason)) => format!("{version} · {reason}"),
        (None, Some(reason)) => format!("not found · {reason}"),
        (None, None) => "unknown".into(),
    }
}

fn sign_in_word(auth: AuthState) -> &'static str {
    match auth {
        AuthState::SignedIn => "signed in",
        AuthState::SignedOut => "signed out",
        AuthState::CredentialsPresent => "credentials present",
        AuthState::Unknown => "unknown",
    }
}

/// The local server profile from the recorded state, read without opening the mutable
/// registry beside a running viewer; a fresh home has no overrides.
fn local_profile(home: &Path) -> io::Result<ServerProfile> {
    let fallback = ServerProfile {
        server_id: "local".into(),
        name: "This computer".into(),
        target: ConnectionTarget::Local,
        repository_root: None,
        harnesses: Default::default(),
        archived: false,
        setup_runtimes: Default::default(),
    };
    let state = home.join("state").join("state.json");
    let bytes = match read_regular_file(&state, STATE_LIMIT) {
        Ok(bytes) => bytes,
        Err(FileFault::Missing) => return Ok(fallback),
        Err(fault) => {
            return Err(io::Error::other(format!(
                "TaskAI state {} {fault}",
                state.display()
            )))
        }
    };
    // A parser error would quote the state; the message names the file only.
    let data: serde_json::Value = serde_json::from_slice(&bytes).map_err(|_| {
        io::Error::other(format!(
            "TaskAI state {} is not valid JSON",
            state.display()
        ))
    })?;
    Ok(data["servers"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|server| serde_json::from_value::<ServerProfile>(server.clone()).ok())
        .find(|server| server.server_id == "local")
        .unwrap_or(fallback))
}

/// `taskai doctor [--json] [--workspace <path>]`: prints the report and exits 0 only when green.
pub fn run(home: &Path, args: &[String]) -> io::Result<()> {
    let mut json = false;
    let mut workspace = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--json" => json = true,
            "--workspace" => {
                i += 1;
                workspace = Some(PathBuf::from(
                    args.get(i).ok_or_else(|| io::Error::other(USAGE))?,
                ));
            }
            _ => return Err(io::Error::other(USAGE)),
        }
        i += 1;
    }
    let report = report(
        &local_profile(home)?,
        &Paths::from_env(),
        workspace.as_deref(),
    );
    if json {
        println!("{}", serde_json::to_string_pretty(&report.json())?);
    } else {
        print!("{}", report.text());
    }
    if report.green() {
        Ok(())
    } else {
        std::process::exit(1)
    }
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;
    use std::os::unix::net::UnixListener;

    use serde_json::json;
    use tungstenite::{accept, Message};

    use super::*;

    fn script(path: &Path, body: &str) {
        std::fs::write(path, body).unwrap();
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700)).unwrap();
    }

    fn fixture_paths(root: &Path) -> Paths {
        let paths = Paths::under(root);
        std::fs::create_dir_all(paths.grok_home.as_ref().unwrap()).unwrap();
        paths
    }

    fn grok_home(paths: &Paths) -> &Path {
        paths.grok_home.as_ref().unwrap()
    }

    fn codex_socket(paths: &Paths) -> &Path {
        paths.codex_control_socket.as_ref().unwrap()
    }

    /// A private daemon that answers one initialize with `user_agent`, then hangs up.
    fn fake_daemon(socket: &Path, user_agent: &'static str) -> std::thread::JoinHandle<()> {
        let listener = UnixListener::bind(socket).unwrap();
        std::thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let mut ws = accept(stream).unwrap();
            let init: serde_json::Value =
                serde_json::from_str(ws.read().unwrap().to_text().unwrap()).unwrap();
            assert_eq!(init["method"], "initialize");
            ws.send(Message::Text(
                json!({"id": init["id"], "result": {"userAgent": user_agent}}).to_string(),
            ))
            .unwrap();
            let _ = ws.read();
        })
    }

    // Expectation: the credentials file is judged by metadata alone — unreadable but present is
    // present, absent is signed out, a dangling symlink or an unreadable parent is unknown.
    // Why: the lead's review — no auth content may be opened and a path error must not pass as
    // signed out. Value: Grok's sign-in row is honest about how weak its evidence is.
    #[test]
    fn grok_saved_login_reads_metadata_only() {
        let root = tempfile::tempdir().unwrap();
        let home = root.path().join("grok");
        std::fs::create_dir_all(&home).unwrap();
        assert_eq!(grok_saved_login(&home), AuthState::SignedOut);
        assert_eq!(
            grok_saved_login(&root.path().join("never-ran")),
            AuthState::SignedOut
        );
        let file = home.join("auth.json");
        std::fs::write(&file, "{\"token\":\"FIXTURE-SECRET\"}").unwrap();
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o000)).unwrap();
        assert_eq!(grok_saved_login(&home), AuthState::CredentialsPresent);
        std::fs::set_permissions(&file, std::fs::Permissions::from_mode(0o600)).unwrap();
        std::fs::remove_file(&file).unwrap();
        std::os::unix::fs::symlink(home.join("missing-target"), &file).unwrap();
        assert_eq!(grok_saved_login(&home), AuthState::Unknown);
        std::fs::remove_file(&file).unwrap();
        std::fs::create_dir(&file).unwrap();
        assert_eq!(grok_saved_login(&home), AuthState::Unknown);
        std::fs::remove_dir(&file).unwrap();
        let sealed = root.path().join("sealed");
        std::fs::create_dir_all(sealed.join("grok")).unwrap();
        std::fs::set_permissions(&sealed, std::fs::Permissions::from_mode(0o000)).unwrap();
        let state = grok_saved_login(&sealed.join("grok"));
        std::fs::set_permissions(&sealed, std::fs::Permissions::from_mode(0o700)).unwrap();
        assert_eq!(state, AuthState::Unknown);
    }

    // Expectation: Claude's common gate is version and envelope support only, labelled as such;
    // an untested or unknown version fails closed and no card in any sessions directory is read.
    // Why: the lead's scope decision — owned native cards are validated by the M6 observer and the
    // M3 mirror per session; a foreign process is not TaskAI's readiness evidence. Value: doctor
    // never touches native state it does not own, and says exactly what it checked.
    #[test]
    fn claude_gate_checks_version_and_envelope_only() {
        let root = tempfile::tempdir().unwrap();
        let paths = fixture_paths(root.path());
        let Some(Check::Ready(detail)) = messaging_check("claude", Some(CLAUDE_TESTED), &paths)
        else {
            panic!("the tested version must pass")
        };
        assert!(
            detail.contains("checked here: version and envelope grammar only"),
            "{detail}"
        );
        assert!(detail.contains("validated by its observer"), "{detail}");
        assert!(!detail.contains("card(s)"), "{detail}");
        let Some(Check::NotReady { reason, remedy }) =
            messaging_check("claude", Some("2.1.288"), &paths)
        else {
            panic!("an untested version must fail closed")
        };
        assert!(
            reason.contains("2.1.288 is not the tested 2.1.322"),
            "{reason}"
        );
        assert!(remedy.contains("re-verify"), "{remedy}");
        assert!(matches!(
            messaging_check("claude", None, &paths),
            Some(Check::NotReady { reason, .. }) if reason.contains("version is unknown")
        ));
    }

    // Expectation: Codex is ready only when the control socket answers initialize as the
    // tested daemon; an absent socket, a refused connection and another version each fail
    // closed with their own reason and remedy, and no thread or turn is ever requested.
    // Why: spec §7 reads the daemon version from initialize, and the brief forbids test turns.
    // Value: the person learns to start Codex once instead of debugging a silent bridge.
    #[test]
    fn codex_gate_needs_the_tested_daemon_over_its_control_socket() {
        let root = tempfile::Builder::new()
            .prefix("d7")
            .tempdir_in("/tmp")
            .unwrap();
        let paths = fixture_paths(root.path());
        assert!(matches!(
            messaging_check("codex", Some("0.167.0"), &paths),
            Some(Check::NotReady { reason, remedy }) if reason.contains("socket is absent") && remedy.contains("Run codex once")
        ));

        let daemon = fake_daemon(codex_socket(&paths), "codex-tui/0.167.1 (fixture)");
        let Some(Check::Ready(detail)) = messaging_check("codex", Some("0.167.0"), &paths) else {
            panic!("the tested daemon must pass")
        };
        assert!(detail.contains("codex-tui/0.167.1"), "{detail}");
        daemon.join().unwrap();
        std::fs::remove_file(codex_socket(&paths)).unwrap();

        let daemon = fake_daemon(codex_socket(&paths), "codex-tui/0.166.0 (fixture)");
        assert!(matches!(
            messaging_check("codex", Some("0.167.0"), &paths),
            Some(Check::NotReady { reason, .. }) if reason.contains("0.166.0 is not the tested 0.167.1")
        ));
        daemon.join().unwrap();
        std::fs::remove_file(codex_socket(&paths)).unwrap();

        drop(UnixListener::bind(codex_socket(&paths)).unwrap());
        assert!(matches!(
            messaging_check("codex", Some("0.167.0"), &paths),
            Some(Check::NotReady { reason, remedy }) if reason.contains("refused the handshake") && remedy.contains("Restart Codex")
        ));
    }

    // Expectation: Grok's gate is the tested version; OpenCode has no gate. Why: the leader
    // and trust checks belong to a session and a workspace, which doctor names in its detail.
    // Value: nothing pretends to have verified a leader that does not exist yet.
    #[test]
    fn grok_gate_is_the_tested_version_and_opencode_has_none() {
        let root = tempfile::tempdir().unwrap();
        let paths = fixture_paths(root.path());
        assert!(matches!(
            messaging_check("grok", Some(GROK_TESTED), &paths),
            Some(Check::Ready(detail)) if detail.contains("yolo_mode=false") && detail.contains("trusted by Grok")
        ));
        assert!(matches!(
            messaging_check("grok", Some("1.0.70"), &paths),
            Some(Check::NotReady { reason, .. }) if reason == "Grok 1.0.70 is not the tested 1.0.77"
        ));
        assert!(matches!(
            messaging_check("grok", None, &paths),
            Some(Check::NotReady { remedy, .. }) if remedy.contains("Install Grok 1.0.77")
        ));
        assert_eq!(messaging_check("opencode", Some("1.18.0"), &paths), None);
        assert_eq!(workspace_check("claude", root.path(), &paths), None);
    }

    // Expectation: a workspace is trusted only by an exact `trusted = true` entry under its
    // given or canonical spelling; a missing store, another folder or `trusted = false` fail
    // closed with the exact reason T required, and an unreadable store is unverified.
    // Why: Grok 1.0.77 refuses project MCP servers in untrusted folders. Value: the remedy
    // names the native prompt and TaskAI never trusts a folder itself.
    #[test]
    fn workspace_trust_is_exact_and_fails_closed() {
        let root = tempfile::tempdir().unwrap();
        let paths = fixture_paths(root.path());
        let store = grok_home(&paths).join("trusted_folders.toml");
        let canonical = root.path().canonicalize().unwrap().join("ws");
        std::fs::create_dir_all(&canonical).unwrap();
        // An alternate spelling of the same folder through a private symlink, portable across
        // hosts that do and do not alias their temp directory.
        std::os::unix::fs::symlink(
            root.path().canonicalize().unwrap(),
            root.path().join("link"),
        )
        .unwrap();
        let workspace = root.path().join("link").join("ws");
        assert_ne!(canonical, workspace);
        assert_eq!(workspace.canonicalize().unwrap(), canonical);

        let Some(Check::NotReady { reason, remedy }) = workspace_check("grok", &workspace, &paths)
        else {
            panic!("no store means nothing is trusted")
        };
        assert_eq!(reason, GROK_UNTRUSTED);
        assert!(remedy.contains("accept its trust prompt"), "{remedy}");

        std::fs::write(
            &store,
            format!(
                "[folders.\"{}\"]\ntrusted = true\ndecided_at = 1\n\n[folders.\"{}\"]\ntrusted = false\n",
                root.path().join("other").display(),
                root.path().join("refused").display()
            ),
        )
        .unwrap();
        assert!(matches!(
            workspace_check("grok", &workspace, &paths),
            Some(Check::NotReady { reason, .. }) if reason == GROK_UNTRUSTED
        ));
        assert!(matches!(
            workspace_check("grok", &root.path().join("refused"), &paths),
            Some(Check::NotReady { reason, .. }) if reason == GROK_UNTRUSTED
        ));
        assert!(matches!(
            workspace_check("grok", &root.path().join("other"), &paths),
            Some(Check::Ready(_))
        ));

        std::fs::write(
            &store,
            format!("[folders.\"{}\"]\ntrusted = true\n", canonical.display()),
        )
        .unwrap();
        assert!(matches!(
            workspace_check("grok", &workspace, &paths),
            Some(Check::Ready(detail)) if detail.contains("Grok trusts")
        ));

        std::fs::write(&store, "[folders\ntrusted = ").unwrap();
        assert!(matches!(
            workspace_check("grok", &workspace, &paths),
            Some(Check::Unverified(reason)) if reason.contains("not valid TOML")
        ));
    }

    // Expectation: one harness check reads the configured executable's version, its native
    // sign-in and its gate, and a failing executable fails every row of that harness alone.
    // Why: the brief — the same configured executable as launch, bounded, fail closed per
    // adapter. Value: M6 can gate a launch on exactly this value.
    #[test]
    fn check_harness_reports_version_login_and_gate_from_one_executable() {
        let root = tempfile::tempdir().unwrap();
        let paths = fixture_paths(root.path());
        let claude = root.path().join("claude fixture");
        script(
            &claude,
            "#!/bin/sh\ncase \"$1\" in\n--version) echo '2.1.322 (Claude Code)';;\nauth) echo '{\"loggedIn\":true,\"email\":\"FIXTURE-SECRET\"}';;\n*) exit 9;;\nesac\n",
        );
        let seen = check_harness("claude", &claude.display().to_string(), &paths);
        assert_eq!(seen.version.as_deref(), Some("2.1.322"));
        assert!(seen.is_ready());
        assert_eq!(seen.auth, AuthState::SignedIn);
        assert!(
            matches!(&seen.messaging, Some(Check::Ready(_))),
            "{:?}",
            seen.messaging
        );

        let broken = root.path().join("broken");
        script(&broken, "#!/bin/sh\nexit 9\n");
        let seen = check_harness("claude", &broken.display().to_string(), &paths);
        assert_eq!(seen.version, None);
        assert!(!seen.is_ready());
        assert_eq!(seen.auth, AuthState::Unknown);
        assert!(matches!(
            &seen.messaging,
            Some(Check::NotReady { reason, .. }) if reason.contains("version is unknown")
        ));

        let grok = root.path().join("grok-cli");
        script(
            &grok,
            "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\necho 'grok 1.0.77 (abc) [stable]'\n",
        );
        std::fs::write(grok_home(&paths).join("auth.json"), "FIXTURE-SECRET").unwrap();
        let seen = check_harness("grok", &grok.display().to_string(), &paths);
        assert_eq!(seen.version.as_deref(), Some("1.0.77"));
        assert_eq!(seen.auth, AuthState::CredentialsPresent);
        assert_eq!(
            seen.authentication_status(),
            "Saved credentials present · not verified"
        );
        assert!(matches!(&seen.messaging, Some(Check::Ready(_))));
        assert_eq!(
            configured_executable("grok", Some("~/bin/grok")),
            "~/bin/grok"
        );
        assert_eq!(configured_executable("codex", None), "codex");
    }

    // Expectation: the report checks the three adapters with the profile's executables, keeps a
    // broken Claude from hiding Codex and Grok, renders stable text and JSON, is red while any
    // gate fails, and never echoes native auth output. Why: build-model M7 — one truthful place,
    // output stable for the UI. Value: M9 reads `doctor: green` before spending a cent.
    #[test]
    fn report_isolates_a_failing_harness_and_renders_stable_text_and_json() {
        let root = tempfile::Builder::new()
            .prefix("d7")
            .tempdir_in("/tmp")
            .unwrap();
        let paths = fixture_paths(root.path());
        let broken = root.path().join("broken-claude");
        script(&broken, "#!/bin/sh\nexit 9\n");
        let codex = root.path().join("codex");
        script(
            &codex,
            "#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 0.167.0';;\nlogin) echo 'Logged in using ChatGPT as FIXTURE-SECRET';;\n*) exit 9;;\nesac\n",
        );
        let grok = root.path().join("grok-cli");
        script(
            &grok,
            "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\necho 'grok 1.0.77 (abc) [stable]'\n",
        );
        let mut profile = ServerProfile {
            server_id: "local".into(),
            name: "This computer".into(),
            target: ConnectionTarget::Local,
            repository_root: None,
            harnesses: Default::default(),
            archived: false,
            setup_runtimes: Default::default(),
        };
        for (hkey, bin) in [("claude", &broken), ("codex", &codex), ("grok", &grok)] {
            profile.harnesses.insert(
                hkey.into(),
                taskai_core::registry::HarnessSettings {
                    enabled: true,
                    executable: Some(bin.display().to_string()),
                    model: None,
                },
            );
        }
        let workspace = root.path().join("ws");
        std::fs::create_dir_all(&workspace).unwrap();
        let daemon = fake_daemon(codex_socket(&paths), "codex-tui/0.167.1 (fixture)");
        let outcome = report(&profile, &paths, Some(&workspace));
        daemon.join().unwrap();

        assert!(!outcome.green());
        let rows: Vec<&str> = outcome
            .harnesses
            .iter()
            .map(|row| row.availability.hkey.as_str())
            .collect();
        assert_eq!(rows, ["claude", "codex", "grok"]);
        assert!(!outcome.harnesses[0].availability.is_ready());
        assert!(matches!(
            &outcome.harnesses[1].availability.messaging,
            Some(Check::Ready(_))
        ));
        assert_eq!(outcome.harnesses[1].availability.auth, AuthState::SignedIn);
        assert_eq!(outcome.harnesses[2].availability.auth, AuthState::SignedOut);
        let (path, checks) = outcome.workspace.as_ref().unwrap();
        assert_eq!(path, &workspace);
        assert!(
            matches!(&checks[..], [(hkey, Check::NotReady { reason, .. })] if hkey == "grok" && reason == GROK_UNTRUSTED)
        );

        let text = outcome.text();
        for line in [
            &format!("Claude Code · {}", broken.display()),
            "  installed   not found · ",
            "  sign-in     Couldn't verify",
            "  messaging   Not ready · Claude Code's version is unknown",
            &format!("Codex · {}", codex.display()),
            "  installed   0.167.0",
            "  sign-in     CLI reports signed in",
            "  messaging   Ready · daemon codex-tui/0.167.1",
            "  sign-in     Sign in needed",
            "  messaging   Ready · Grok 1.0.77",
            &format!("workspace {}", workspace.display()),
            "  grok        Not ready · workspace not trusted by Grok · Open grok in",
            // broken Claude, Grok without a saved login, untrusted workspace
            "doctor: red · 3 not ready",
        ] {
            assert!(text.contains(line), "missing {line:?} in\n{text}");
        }
        assert!(!text.contains("SECRET"), "{text}");

        let json = outcome.json();
        assert_eq!(json["green"], false);
        assert_eq!(json["harnesses"][0]["launch"]["available"], false);
        assert_eq!(json["harnesses"][0]["messaging"]["status"], "not ready");
        assert_eq!(json["harnesses"][1]["version"], "0.167.0");
        assert_eq!(json["harnesses"][1]["sign_in"], "signed in");
        assert_eq!(json["harnesses"][1]["messaging"]["status"], "ready");
        assert_eq!(json["harnesses"][2]["sign_in"], "signed out");
        assert_eq!(json["workspace"]["checks"][0]["hkey"], "grok");
        assert_eq!(json["workspace"]["checks"][0]["detail"], GROK_UNTRUSTED);
        assert!(!json.to_string().contains("SECRET"));

        std::fs::write(
            grok_home(&paths).join("trusted_folders.toml"),
            format!("[folders.\"{}\"]\ntrusted = true\n", workspace.display()),
        )
        .unwrap();
        profile.harnesses.get_mut("claude").unwrap().executable = Some(codex.display().to_string());
        std::fs::write(grok_home(&paths).join("auth.json"), "FIXTURE-SECRET").unwrap();
        std::fs::remove_file(codex_socket(&paths)).unwrap();
        let daemon = fake_daemon(codex_socket(&paths), "codex-tui/0.167.1 (fixture)");
        let outcome = report(&profile, &paths, Some(&workspace));
        daemon.join().unwrap();
        assert!(!outcome.green(), "a Codex binary is not Claude 2.1.322");
        assert!(outcome.text().ends_with("doctor: red · 1 not ready\n"));
    }

    fn row(
        hkey: &str,
        auth: AuthState,
        unavailable: Option<&str>,
        messaging: Check,
    ) -> HarnessReport {
        HarnessReport {
            executable: hkey.into(),
            availability: HarnessAvailability {
                hkey: hkey.into(),
                version: Some("1.0.0".into()),
                unavailable: unavailable.map(str::to_string),
                auth,
                messaging: Some(messaging),
            },
        }
    }

    fn all_ready() -> Report {
        Report {
            harnesses: vec![
                row(
                    "claude",
                    AuthState::SignedIn,
                    None,
                    Check::Ready("ok".into()),
                ),
                row(
                    "codex",
                    AuthState::SignedIn,
                    None,
                    Check::Ready("ok".into()),
                ),
                row(
                    "grok",
                    AuthState::CredentialsPresent,
                    None,
                    Check::Ready("ok".into()),
                ),
            ],
            workspace: None,
        }
    }

    // Expectation: green needs every row launchable, with a saved login (a stat-only credentials
    // file counts, still unverified) and a ready gate; a signed-out, unknown or unavailable row
    // turns the verdict red in text and JSON alike, with the count agreeing. Why: the lead's
    // review — three adapters with nobody signed in must not read green. Value: M9 can trust one
    // line.
    #[test]
    fn green_requires_launch_saved_login_and_gate_on_every_row() {
        let green = all_ready();
        assert!(green.green());
        assert!(
            green.text().ends_with("doctor: green\n"),
            "{}",
            green.text()
        );
        assert_eq!(green.json()["green"], true);

        let mut variants: Vec<(&str, Report)> = Vec::new();
        for (name, auth) in [
            ("signed out", AuthState::SignedOut),
            ("unknown login", AuthState::Unknown),
        ] {
            let mut report = all_ready();
            report.harnesses[1].availability.auth = auth;
            variants.push((name, report));
        }
        let mut unavailable = all_ready();
        unavailable.harnesses[0].availability.unavailable = Some("codex is not claude".into());
        variants.push(("unavailable launch", unavailable));
        let mut gate = all_ready();
        gate.harnesses[2].availability.messaging = Some(Check::Unverified("no home".into()));
        variants.push(("unverified gate", gate));
        let mut missing_gate = all_ready();
        missing_gate.harnesses[2].availability.messaging = None;
        variants.push(("no gate", missing_gate));
        for (name, report) in &variants {
            assert!(!report.green(), "{name} must be red");
            assert!(
                report.text().ends_with("doctor: red · 1 not ready\n"),
                "{name}: {}",
                report.text()
            );
            assert_eq!(report.json()["green"], false, "{name}");
        }
        let mut two = all_ready();
        two.harnesses[0].availability.auth = AuthState::SignedOut;
        two.harnesses[1].availability.messaging = Some(Check::not_ready("down", "up"));
        assert!(
            two.text().ends_with("doctor: red · 2 not ready\n"),
            "{}",
            two.text()
        );
    }

    // A regular file can become a FIFO after stat. Run that exact race in an owned child,
    // so the old blocking open is a bounded failure instead of stranding the test suite.
    #[test]
    fn file_replaced_by_fifo_between_stat_and_open_is_refused_without_blocking() {
        use std::{
            ffi::CString,
            os::unix::ffi::OsStrExt,
            process::{Child, Command, Stdio},
            time::Instant,
        };

        const CHILD_ROOT: &str = "TASKAI_DOCTOR_FIFO_TEST_ROOT";
        if let Some(root) = std::env::var_os(CHILD_ROOT) {
            let root = PathBuf::from(root);
            let path = root.join("state.json");
            let result = read_regular_file_with(&path, STATE_LIMIT, || {
                std::fs::rename(&path, root.join("original.json")).unwrap();
                let name = CString::new(path.as_os_str().as_bytes()).unwrap();
                // SAFETY: name is a live, NUL-terminated owned path and mkfifo retains no pointer.
                assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
                std::fs::write(root.join("opening"), b"ready").unwrap();
            });
            assert_eq!(result, Err(FileFault::NotRegular("fifo")));
            return;
        }

        struct OwnedChild(Child);
        impl Drop for OwnedChild {
            fn drop(&mut self) {
                let _ = self.0.kill();
                let _ = self.0.wait();
            }
        }

        let root = tempfile::tempdir().unwrap();
        std::fs::write(root.path().join("state.json"), b"{}").unwrap();
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args([
                "--exact",
                "doctor::tests::file_replaced_by_fifo_between_stat_and_open_is_refused_without_blocking",
            ])
            .env(CHILD_ROOT, root.path())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        taskai_core::mail::strip_messaging_env(&mut command);
        let mut child = OwnedChild(command.spawn().unwrap());
        let startup_deadline = Instant::now() + Duration::from_secs(5);
        while !root.path().join("opening").exists() {
            assert!(
                child.0.try_wait().unwrap().is_none(),
                "private child exited before the swap"
            );
            assert!(
                Instant::now() < startup_deadline,
                "private child did not reach the swap"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        let finish_deadline = Instant::now() + Duration::from_secs(2);
        loop {
            if let Some(status) = child.0.try_wait().unwrap() {
                assert!(status.success(), "private child failed the FIFO refusal");
                break;
            }
            assert!(
                Instant::now() < finish_deadline,
                "doctor blocked opening the replacement FIFO"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    // Expectation: the trust store and the recorded state are read only when they are regular
    // files within a size bound: a FIFO is refused without blocking, an oversized file and a
    // malformed one are reported without echoing a byte of their content, and missing stays
    // distinct from unreadable. Why: the lead's review — an unbounded read_to_string can hang
    // doctor and leak a store's text into a reason. Value: doctor's output is safe to paste.
    #[test]
    fn required_reads_are_bounded_refuse_fifos_and_never_echo_content() {
        let root = tempfile::tempdir().unwrap();
        let paths = fixture_paths(root.path());
        let store = grok_home(&paths).join("trusted_folders.toml");
        let workspace = root.path().join("ws");
        std::fs::create_dir_all(&workspace).unwrap();

        std::fs::write(&store, "[folders\nFIXTURE-SECRET-MARKER = ").unwrap();
        let Some(Check::Unverified(reason)) = workspace_check("grok", &workspace, &paths) else {
            panic!("malformed store must be unverified")
        };
        assert!(reason.contains("not valid TOML"), "{reason}");
        assert!(!reason.contains("FIXTURE-SECRET-MARKER"), "{reason}");

        let oversized = "# FIXTURE-SECRET-MARKER\n".repeat(TRUST_STORE_LIMIT / 24 + 2);
        assert!(oversized.len() > TRUST_STORE_LIMIT);
        std::fs::write(&store, &oversized).unwrap();
        let Some(Check::Unverified(reason)) = workspace_check("grok", &workspace, &paths) else {
            panic!("oversized store must be unverified")
        };
        assert!(reason.contains("larger than"), "{reason}");
        assert!(!reason.contains("FIXTURE-SECRET-MARKER"), "{reason}");

        std::fs::remove_file(&store).unwrap();
        assert!(std::process::Command::new("mkfifo")
            .arg(&store)
            .status()
            .unwrap()
            .success());
        let (tx, rx) = std::sync::mpsc::channel();
        let fifo_paths = paths.clone();
        let fifo_workspace = workspace.clone();
        std::thread::spawn(move || {
            let _ = tx.send(workspace_check("grok", &fifo_workspace, &fifo_paths));
        });
        match rx.recv_timeout(Duration::from_secs(5)) {
            Ok(Some(Check::Unverified(reason))) => {
                assert!(reason.contains("not a regular file"), "{reason}")
            }
            other => panic!("a FIFO store must be refused without blocking: {other:?}"),
        }
        std::fs::remove_file(&store).unwrap();
        assert!(matches!(
            workspace_check("grok", &workspace, &paths),
            Some(Check::NotReady { reason, .. }) if reason == GROK_UNTRUSTED
        ));

        let home = root.path().join("taskai");
        let state = home.join("state");
        std::fs::create_dir_all(&state).unwrap();
        assert_eq!(local_profile(&home).unwrap().server_id, "local");
        std::fs::write(
            state.join("state.json"),
            "{\"servers\": [FIXTURE-SECRET-MARKER",
        )
        .unwrap();
        let error = local_profile(&home).unwrap_err().to_string();
        assert!(error.contains("not valid JSON"), "{error}");
        assert!(!error.contains("FIXTURE-SECRET-MARKER"), "{error}");
        std::fs::write(
            state.join("state.json"),
            format!("{{\"pad\": \"{}\"}}", "x".repeat(STATE_LIMIT)),
        )
        .unwrap();
        assert!(local_profile(&home)
            .unwrap_err()
            .to_string()
            .contains("larger than"));
        std::fs::remove_file(state.join("state.json")).unwrap();
        assert!(std::process::Command::new("mkfifo")
            .arg(state.join("state.json"))
            .status()
            .unwrap()
            .success());
        let (tx, rx) = std::sync::mpsc::channel();
        let fifo_home = home.clone();
        std::thread::spawn(move || {
            let _ = tx.send(local_profile(&fifo_home).map_err(|error| error.to_string()));
        });
        match rx.recv_timeout(Duration::from_secs(5)) {
            Ok(Err(error)) => assert!(error.contains("not a regular file"), "{error}"),
            other => panic!("a FIFO state must be refused without blocking: {other:?}"),
        }
        std::fs::remove_file(state.join("state.json")).unwrap();
        std::fs::write(
            state.join("state.json"),
            serde_json::json!({"servers": [{"server_id": "local", "name": "This computer",
                "target": {"kind": "local"}, "harnesses": {"grok": {"enabled": true, "executable": "/opt/grok"}}}]})
            .to_string(),
        )
        .unwrap();
        let profile = local_profile(&home).unwrap();
        assert_eq!(
            profile.harnesses["grok"].executable.as_deref(),
            Some("/opt/grok")
        );
    }

    // Expectation: a native home comes from an absolute override or an absolute HOME; an empty
    // or relative override and an unusable HOME are explicit errors that surface as unverified
    // Grok/Codex gates and an unknown Grok login, never as a look into ~/.grok or the current
    // directory. Why: the lead's review and Grok's own resolution of GROK_HOME (e9adb8d).
    // Value: doctor never reports another home's state as this one's.
    #[test]
    fn native_home_overrides_must_be_absolute_and_fail_explicitly() {
        let abs = std::ffi::OsStr::new("/srv/home");
        assert_eq!(
            native_home(
                "GROK_HOME",
                Some("/opt/grok-home".into()),
                Some(abs),
                ".grok"
            ),
            Ok(PathBuf::from("/opt/grok-home"))
        );
        assert_eq!(
            native_home("GROK_HOME", None, Some(abs), ".grok"),
            Ok(PathBuf::from("/srv/home/.grok"))
        );
        for (name, value) in [("relative", "grok-home"), ("empty", "")] {
            let error =
                native_home("GROK_HOME", Some(value.into()), Some(abs), ".grok").unwrap_err();
            assert!(error.contains("GROK_HOME"), "{name}: {error}");
            assert!(
                !error.contains(".grok/"),
                "{name} must not fall back: {error}"
            );
        }
        assert!(native_home("CODEX_HOME", None, None, ".codex")
            .unwrap_err()
            .contains("HOME is unset"));
        assert!(native_home(
            "CODEX_HOME",
            None,
            Some(std::ffi::OsStr::new("relative-home")),
            ".codex"
        )
        .unwrap_err()
        .contains("not an absolute directory"));

        let root = tempfile::tempdir().unwrap();
        let paths = Paths {
            codex_control_socket: Err("CODEX_HOME is not an absolute directory (x)".into()),
            grok_home: Err("GROK_HOME is set but empty".into()),
        };
        assert!(matches!(
            messaging_check("grok", Some(GROK_TESTED), &paths),
            Some(Check::Unverified(reason)) if reason.contains("GROK_HOME")
        ));
        assert!(matches!(
            messaging_check("codex", Some("0.167.0"), &paths),
            Some(Check::Unverified(reason)) if reason.contains("CODEX_HOME")
        ));
        assert!(matches!(
            workspace_check("grok", root.path(), &paths),
            Some(Check::Unverified(reason)) if reason.contains("GROK_HOME")
        ));
        let grok = root.path().join("grok-cli");
        script(
            &grok,
            "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\necho 'grok 1.0.77 (abc) [stable]'\n",
        );
        let seen = check_harness("grok", &grok.display().to_string(), &paths);
        assert_eq!(seen.auth, AuthState::Unknown);
        assert!(matches!(&seen.messaging, Some(Check::Unverified(_))));
    }

    // Expectation: the Codex Ready text claims only what ran — initialize and the User-Agent
    // version — and names what did not. Why: the lead's review; the approved live check used no
    // turn/start or turn/interrupt, and a fake fixture proves no native method. Value: nobody
    // reads a method set into a handshake.
    #[test]
    fn codex_ready_text_claims_only_the_handshake() {
        let root = tempfile::Builder::new()
            .prefix("d7")
            .tempdir_in("/tmp")
            .unwrap();
        let paths = fixture_paths(root.path());
        let daemon = fake_daemon(codex_socket(&paths), "codex-tui/0.167.1 (fixture)");
        let Some(Check::Ready(detail)) = messaging_check("codex", Some("0.167.0"), &paths) else {
            panic!("the tested daemon must pass")
        };
        daemon.join().unwrap();
        assert!(detail.contains("answered initialize"), "{detail}");
        assert!(detail.contains("User-Agent"), "{detail}");
        assert!(detail.contains("no thread, turn or interrupt"), "{detail}");
        assert!(!detail.contains("turn/start"), "{detail}");
        assert!(!detail.contains("verified"), "{detail}");
    }
}
