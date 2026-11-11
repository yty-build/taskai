// ABOUTME: Stateless local/SSH tmux commands for durable TaskAI agent runtimes.
// ABOUTME: Every operation targets one validated private server and exact session ref.

use std::{
    fmt,
    io::Read,
    os::unix::ffi::OsStrExt,
    path::{Path, PathBuf},
    process::{Command, Output, Stdio},
    sync::mpsc,
    time::{Duration, Instant},
};

use taskai_core::registry::{ConnectionTarget, TmuxRuntimeLocator};

const STRIPPED_TMUX_ENV: [&str; 3] = ["TMUX", "TMUX_PANE", "TMUX_TMPDIR"];
const SSH_CONNECT_TIMEOUT_SECS: &str = "10";
const CONTROL_TIMEOUT: Duration = Duration::from_secs(15);
const MAX_CONTROL_OUTPUT: usize = 1_048_576;
// Use the target user's ordinary interactive-login PATH only for providers.
// Startup banners belong neither in version responses nor the native CLI pane.
// Save the terminal descriptors/cwd across startup, then restore them and exec
// positional argv verbatim: provider arguments are never shell source.
const REMOTE_PROVIDER_SCRIPT: &str = r#"exec 3>&1 4>&2
exec "${SHELL:-/bin/sh}" -lic 'exec 1>&3 2>&4 3>&- 4>&-; CDPATH= cd -P "$1" || exit; shift; case "$1" in "~/"*) taskai_program="${HOME:?HOME is required for ~/executables}/${1#??}"; shift; set -- "$taskai_program" "$@";; esac; exec /usr/bin/env -- "$@"' taskai-provider "$PWD" "$@" >/dev/null 2>/dev/null"#;
const REMOTE_REPOSITORY_MISSING: i32 = 3;
const REMOTE_REPOSITORY_SCRIPT: &str = r#"input=$1
case "$input" in
    "~")
        [ -n "${HOME:-}" ] || exit 4
        candidate=$HOME
        ;;
    "~/"*)
        [ -n "${HOME:-}" ] || exit 4
        candidate="${HOME}/${input#??}"
        ;;
    /*)
        candidate=$input
        ;;
    *)
        exit 2
        ;;
esac
[ -d "$candidate" ] || exit 3
CDPATH= cd -P "$candidate" 2>/dev/null || exit 3
case "$PWD" in
    /*) ;;
    *) exit 4 ;;
esac
printf "%s\000" "$PWD"
"#;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Probe {
    pub version: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RuntimeStatus {
    Live {
        pid: u32,
        cols: u16,
        rows: u16,
    },
    Dead {
        pid: u32,
        exit_code: Option<i32>,
        cols: u16,
        rows: u16,
    },
    Missing,
}

/// Exact native pane identity from the private tmux server, with an OS-verified
/// process start. A bridge window cannot satisfy this identity.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NativeIdentity {
    pub pane_id: String,
    pub pid: u32,
    pub proc_start: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AttachCommand {
    /// The executable passed to `Pane::spawn`.
    pub program: String,
    /// Owned arguments; callers can make a temporary `Vec<&str>` for `Pane::spawn`.
    pub args: Vec<String>,
}

#[derive(Debug)]
pub enum Error {
    InvalidInput(String),
    Spawn {
        operation: &'static str,
        source: std::io::Error,
    },
    CommandFailed {
        operation: &'static str,
        code: Option<i32>,
        stderr: String,
    },
    InvalidOutput(String),
    Timeout {
        operation: &'static str,
        after: Duration,
        detail: String,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidInput(message) | Self::InvalidOutput(message) => f.write_str(message),
            Self::Spawn { operation, source } => write!(f, "tmux {operation}: {source}"),
            Self::Timeout { operation, after, detail } => write!(
                f,
                "tmux {operation} timed out after {} seconds ({detail}); the saved runtime is retained for retry",
                after.as_secs(),
            ),
            Self::CommandFailed {
                operation,
                code,
                stderr,
            } => {
                let status = code
                    .map(|value| value.to_string())
                    .unwrap_or_else(|| "signal".into());
                if stderr.is_empty() {
                    write!(f, "tmux {operation} failed ({status})")
                } else {
                    write!(f, "tmux {operation} failed ({status}): {stderr}")
                }
            }
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Spawn { source, .. } => Some(source),
            _ => None,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct Invocation {
    program: String,
    args: Vec<String>,
}

/// Verify that tmux is available on a local or SSH target.
pub fn probe(target: &ConnectionTarget) -> Result<Probe, Error> {
    validate_target(target)?;
    let invocation = target_invocation(target, &["tmux".into(), "-V".into()])?;
    let output = run(&invocation, "probe")?;
    require_success(output, "probe").and_then(|output| {
        let version = String::from_utf8_lossy(&output.stdout).trim().to_owned();
        if version.starts_with("tmux ") {
            Ok(Probe { version })
        } else {
            Err(Error::InvalidOutput(format!(
                "unexpected tmux version response: {version:?}"
            )))
        }
    })
}

/// Run a provider's harmless version handshake on the recorded target.
///
/// This prevents local availability from being mistaken for remote availability. It
/// starts no provider session and submits no model turn.
pub fn probe_program(target: &ConnectionTarget, program: &str) -> Result<String, Error> {
    validate_target(target)?;
    if program.is_empty() {
        return Err(Error::InvalidInput(
            "provider program may not be empty".into(),
        ));
    }
    let provider = provider_launch_command(target, program, &["--version".into()])?;
    let mut command = vec![provider.program];
    command.extend(provider.args);
    let invocation = target_invocation(target, &command)?;
    let output = require_success(run(&invocation, "provider probe")?, "provider probe")?;
    let stdout = String::from_utf8_lossy(&output.stdout).trim().to_owned();
    let stderr = String::from_utf8_lossy(&output.stderr).trim().to_owned();
    let detail = if stdout.is_empty() { stderr } else { stdout };
    if detail.is_empty() {
        Err(Error::InvalidOutput(format!(
            "{program} --version returned no version text"
        )))
    } else {
        Ok(detail)
    }
}

/// Bounded native login-status query. The caller must reduce this transient
/// output to a sanitized enum and must never log/display its stdout or stderr.
/// Nonzero status is returned intact so a known signed-out response remains
/// distinguishable from transport failure. No auth or model operation is run.
pub fn probe_auth_status(
    target: &ConnectionTarget,
    program: &str,
    args: &[String],
) -> Result<Output, Error> {
    probe_auth_status_with_runner(target, program, args, &|invocation, operation| {
        run_with_limits(invocation, operation, Duration::from_secs(10), 65_536)
    })
}

fn probe_auth_status_with_runner<R>(
    target: &ConnectionTarget,
    program: &str,
    args: &[String],
    runner: &R,
) -> Result<Output, Error>
where
    R: Fn(&Invocation, &'static str) -> Result<Output, Error> + ?Sized,
{
    if args != ["auth", "status", "--json"] && args != ["login", "status"] {
        return Err(Error::InvalidInput(
            "Only native read-only authentication status commands are permitted".into(),
        ));
    }
    let provider = provider_launch_command(target, program, args)?;
    let mut command = vec![provider.program];
    command.extend(provider.args);
    runner(
        &target_invocation(target, &command)?,
        "authentication status",
    )
}

/// Prepare an executable in the same environment used for provider discovery.
/// Local processes remain unchanged. An SSH runtime refreshes its login-shell
/// environment on every actual launch, even if its tmux server has stale PATH.
/// The wrapper execs through to the provider; no additional resident process or
/// daemon is kept and exact resume IDs remain separate argv values.
pub fn provider_launch_command(
    target: &ConnectionTarget,
    program: &str,
    args: &[String],
) -> Result<AttachCommand, Error> {
    validate_target(target)?;
    if program.is_empty() {
        return Err(Error::InvalidInput(
            "provider program may not be empty".into(),
        ));
    }
    if *target == ConnectionTarget::Local {
        let program =
            match program.strip_prefix("~/") {
                Some(relative) => PathBuf::from(std::env::var_os("HOME").ok_or_else(|| {
                    Error::InvalidInput("HOME is required for ~/executables".into())
                })?)
                .join(relative)
                .to_string_lossy()
                .into_owned(),
                None => program.into(),
            };
        return Ok(AttachCommand {
            program,
            args: args.to_vec(),
        });
    }
    let mut wrapped = vec![
        "-c".into(),
        REMOTE_PROVIDER_SCRIPT.into(),
        "taskai-provider".into(),
        program.into(),
    ];
    wrapped.extend_from_slice(args);
    Ok(AttachCommand {
        program: "/bin/sh".into(),
        args: wrapped,
    })
}

/// Create exactly one detached tmux session and directly exec the supplied program.
///
/// `program` and every argument are passed as separate tmux arguments. On SSH targets,
/// each argument is independently POSIX-quoted before OpenSSH hands it to the remote
/// login shell.
#[allow(clippy::too_many_arguments)]
pub fn create(
    locator: &TmuxRuntimeLocator,
    program: &str,
    args: &[String],
    cwd: &Path,
    env: &[(String, String)],
    rows: u16,
    cols: u16,
) -> Result<(), Error> {
    // A private server retained from an older TaskAI process may still carry
    // inherited Claude identity keys. Remove only matching names before tmux
    // creates a new provider pane.
    scrub_private_environment(locator, false)?;
    create_with_runner(locator, program, args, cwd, env, rows, cols, &run)
}

/// One detached TaskAI process beside the native pane. The sidecar occupies
/// fixed window 1; a live or unexpected window there is never replaced.
pub fn create_sidecar(
    locator: &TmuxRuntimeLocator,
    name: &str,
    program: &str,
    args: &[String],
    cwd: &Path,
    env: &[(String, String)],
) -> Result<(), Error> {
    validate_locator(locator)?;
    if locator.target != ConnectionTarget::Local {
        return Err(Error::InvalidInput(
            "sidecar requires a local runtime".into(),
        ));
    }
    if !matches!(name, "bridge" | "observer") || program.is_empty() {
        return Err(Error::InvalidInput(
            "invalid sidecar name or program".into(),
        ));
    }
    let cwd = cwd
        .to_str()
        .ok_or_else(|| Error::InvalidInput("sidecar path must be valid UTF-8".into()))?;
    if !matches!(inspect(locator)?, RuntimeStatus::Live { .. }) {
        return Err(Error::InvalidInput("native pane is not live".into()));
    }
    if !matches!(inspect_sidecar(locator, name)?, RuntimeStatus::Missing) {
        return Err(Error::InvalidInput("sidecar window already exists".into()));
    }
    for (key, _) in env {
        validate_environment_name(key)?;
        if taskai_core::mail::is_messaging_env_name(std::ffi::OsStr::new(key)) {
            return Err(Error::InvalidInput(
                "sidecar may not set Claude messaging identity".into(),
            ));
        }
    }
    scrub_private_environment(locator, true)?;
    let mut command = tmux_prefix(locator);
    command.extend([
        "new-window".into(),
        "-d".into(),
        "-t".into(),
        sidecar_window_target(locator),
        "-n".into(),
        name.into(),
        "-c".into(),
        cwd.into(),
    ]);
    for (key, value) in env {
        command.extend(["-e".into(), format!("{key}={value}")]);
    }
    command.extend(["--".into(), program.into()]);
    command.extend(args.iter().cloned());
    command.extend([
        ";".into(),
        "select-window".into(),
        "-t".into(),
        native_window_target(locator),
    ]);
    let invocation = target_invocation(&locator.target, &command)?;
    require_success(run(&invocation, "create sidecar")?, "create sidecar")?;
    Ok(())
}

pub fn inspect_sidecar(locator: &TmuxRuntimeLocator, name: &str) -> Result<RuntimeStatus, Error> {
    validate_locator(locator)?;
    if !matches!(name, "bridge" | "observer") {
        return Err(Error::InvalidInput("invalid sidecar name".into()));
    }
    let mut command = tmux_prefix(locator);
    command.extend([
        "list-panes".into(),
        "-t".into(),
        sidecar_pane_target(locator),
        "-F".into(),
        "#{window_name}\t#{pane_dead}\t#{pane_pid}\t#{pane_dead_status}\t#{window_width}\t#{window_height}".into(),
    ]);
    let invocation = target_invocation(&locator.target, &command)?;
    let output = run(&invocation, "inspect sidecar")?;
    if output_means_missing(&output) {
        return Ok(RuntimeStatus::Missing);
    }
    let output = require_success(output, "inspect sidecar")?;
    let prefix = format!("{name}\t");
    let bytes = output
        .stdout
        .strip_prefix(prefix.as_bytes())
        .ok_or_else(|| Error::InvalidOutput("sidecar window has an unexpected name".into()))?;
    parse_inspection(bytes)
}

pub fn kill_sidecar(locator: &TmuxRuntimeLocator, name: &str) -> Result<bool, Error> {
    match inspect_sidecar(locator, name)? {
        RuntimeStatus::Missing => return Ok(false),
        RuntimeStatus::Live { .. } | RuntimeStatus::Dead { .. } => {}
    }
    let mut command = tmux_prefix(locator);
    command.extend([
        "kill-window".into(),
        "-t".into(),
        sidecar_window_target(locator),
    ]);
    optional_success_with_runner(locator, command, "kill sidecar", &run)
}

fn scrub_private_environment(
    locator: &TmuxRuntimeLocator,
    session_exists: bool,
) -> Result<(), Error> {
    validate_locator(locator)?;
    if locator.target != ConnectionTarget::Local {
        return Ok(());
    }
    for global in [true, false]
        .into_iter()
        .take(if session_exists { 2 } else { 1 })
    {
        let mut command = tmux_prefix(locator);
        command.push("show-environment".into());
        if global {
            command.push("-g".into());
        } else {
            command.extend(["-t".into(), exact_session(&locator.session)]);
        }
        let invocation = target_invocation(&locator.target, &command)?;
        let output = run(&invocation, "inspect private environment")?;
        if output_means_missing(&output) && !session_exists {
            return Ok(());
        }
        let output = require_success(output, "inspect private environment")?;
        for line in output.stdout.split(|byte| *byte == b'\n') {
            let displayed = line.split(|byte| *byte == b'=').next().unwrap_or_default();
            if !displayed.starts_with(b"CLAUDE_CODE_MESSAGING_") {
                continue;
            }
            let name = decode_tmux_environment_name(displayed)?;
            if !taskai_core::mail::is_messaging_env_name(std::ffi::OsStr::from_bytes(&name)) {
                continue;
            }
            let mut unset = Command::new("tmux");
            unset.args([
                "-f",
                "/dev/null",
                "-L",
                &locator.private_server,
                "set-environment",
                "-u",
            ]);
            if global {
                unset.arg("-g");
            } else {
                unset.args(["-t", &exact_session(&locator.session)]);
            }
            unset.arg(std::ffi::OsStr::from_bytes(&name));
            require_success(
                run_command_with_limits(
                    unset,
                    "scrub private environment",
                    CONTROL_TIMEOUT,
                    MAX_CONTROL_OUTPUT,
                )?,
                "scrub private environment",
            )?;
        }
        // show-environment is a human rendering, not a reversible wire format:
        // literal backslashes may look like octal escapes and control bytes may
        // be printed literally. Never create a pane while any apparent matching
        // name remains, even if that means declining an ambiguous server state.
        let after = run(&invocation, "verify private environment")?;
        let after = require_success(after, "verify private environment")?;
        if after
            .stdout
            .split(|byte| *byte == b'\n')
            .any(|line| line.starts_with(b"CLAUDE_CODE_MESSAGING_"))
        {
            return Err(Error::InvalidOutput(
                "private tmux environment could not be safely scrubbed".into(),
            ));
        }
    }
    Ok(())
}

/// Decode the octal form tmux can use for non-ASCII name bytes. Its display
/// format is ambiguous for some literal backslashes and controls, so callers
/// must verify that no matching rendered name remains before spawning a pane.
/// Values are never inspected.
fn decode_tmux_environment_name(displayed: &[u8]) -> Result<Vec<u8>, Error> {
    let mut decoded = Vec::with_capacity(displayed.len());
    let mut index = 0;
    while index < displayed.len() {
        if displayed[index] != b'\\' {
            decoded.push(displayed[index]);
            index += 1;
            continue;
        }
        if displayed.get(index + 1) == Some(&b'\\') {
            decoded.push(b'\\');
            index += 2;
            continue;
        }
        let digits = displayed.get(index + 1..index + 4).ok_or_else(|| {
            Error::InvalidOutput("private environment name has an invalid escape".into())
        })?;
        if !digits.iter().all(|byte| (b'0'..=b'7').contains(byte)) {
            return Err(Error::InvalidOutput(
                "private environment name has an invalid escape".into(),
            ));
        }
        let value = u16::from(digits[0] - b'0') * 64
            + u16::from(digits[1] - b'0') * 8
            + u16::from(digits[2] - b'0');
        decoded.push(u8::try_from(value).map_err(|_| {
            Error::InvalidOutput("private environment name has an invalid escape".into())
        })?);
        index += 4;
    }
    Ok(decoded)
}

/// Use the target account's ordinary login shell, never the local machine's
/// shell path on an SSH host. No provider/context hooks or sidecar are installed.
pub fn create_terminal(
    locator: &TmuxRuntimeLocator,
    cwd: &Path,
    rows: u16,
    cols: u16,
) -> Result<(), Error> {
    create(
        locator,
        "/bin/sh",
        &[
            "-c".into(),
            "unset TASKAI_SESSION_REF TASKAI_HOME; exec \"${SHELL:-/bin/sh}\" -l".into(),
        ],
        cwd,
        &[],
        rows,
        cols,
    )
}

#[allow(clippy::too_many_arguments)]
fn create_with_runner<R>(
    locator: &TmuxRuntimeLocator,
    program: &str,
    args: &[String],
    cwd: &Path,
    env: &[(String, String)],
    rows: u16,
    cols: u16,
    runner: &R,
) -> Result<(), Error>
where
    R: Fn(&Invocation, &'static str) -> Result<Output, Error> + ?Sized,
{
    validate_locator(locator)?;
    if program.is_empty() {
        return Err(Error::InvalidInput(
            "runtime program may not be empty".into(),
        ));
    }
    if rows == 0 || cols == 0 {
        return Err(Error::InvalidInput(
            "runtime rows and columns must be nonzero".into(),
        ));
    }
    let cwd = cwd
        .to_str()
        .ok_or_else(|| Error::InvalidInput("runtime path must be valid UTF-8".into()))?;
    for (name, _) in env {
        validate_environment_name(name)?;
        if STRIPPED_TMUX_ENV.contains(&name.as_str()) {
            return Err(Error::InvalidInput(format!(
                "runtime environment may not override {name}"
            )));
        }
        if taskai_core::mail::is_messaging_env_name(std::ffi::OsStr::new(name)) {
            return Err(Error::InvalidInput(
                "runtime may not set Claude messaging identity".into(),
            ));
        }
    }

    let mut tmux = tmux_prefix(locator);
    // Configure the private server before it creates the first pane. The child therefore
    // sees tmux-256color, while the xterm-256color viewer receives exact RGB rather than
    // tmux's 256-colour approximation. `start-server` and the following commands are one
    // tmux command list; no resident TaskAI service is introduced.
    tmux.extend([
        "start-server".into(),
        ";".into(),
        "set-option".into(),
        "-g".into(),
        "default-terminal".into(),
        "tmux-256color".into(),
        ";".into(),
        "set-option".into(),
        "-s".into(),
        "terminal-features".into(),
        "xterm-256color:RGB,tmux-256color:RGB".into(),
        ";".into(),
    ]);
    tmux.extend([
        "new-session".into(),
        "-d".into(),
        "-s".into(),
        locator.session.clone(),
        "-n".into(),
        "native".into(),
        "-x".into(),
        cols.to_string(),
        "-y".into(),
        rows.to_string(),
        "-c".into(),
        cwd.into(),
    ]);
    for (name, value) in env {
        tmux.extend(["-e".into(), format!("{name}={value}")]);
    }
    tmux.extend(["-e".into(), "COLORTERM=truecolor".into()]);
    tmux.push("--".into());
    tmux.push(program.into());
    tmux.extend(args.iter().cloned());

    // These commands are part of the same tmux command list as creation. A live pane
    // survives viewer detachment. If the provider itself exits, tmux removes the session
    // and its attach client exits too, so TaskAI cannot keep painting a dead CLI as LIVE.
    tmux.extend([
        ";".into(),
        "set-option".into(),
        "-t".into(),
        locator.session.clone(),
        "status".into(),
        "off".into(),
        ";".into(),
        "set-option".into(),
        "-t".into(),
        locator.session.clone(),
        "prefix".into(),
        "None".into(),
        ";".into(),
        "set-option".into(),
        "-t".into(),
        locator.session.clone(),
        "prefix2".into(),
        "None".into(),
    ]);
    tmux.push(";".into());
    tmux.extend(private_mouse_commands(locator));

    let invocation = target_invocation(&locator.target, &tmux)?;
    require_success(runner(&invocation, "create")?, "create")?;
    Ok(())
}

/// Resolve a repository path on its recorded local or SSH target.
///
/// Local paths retain the existing behavior: an existing directory is returned exactly
/// as supplied and a missing directory is `None`. Remote paths must be absolute, `~`,
/// or start with `~/`; successful remote resolution returns the canonical absolute
/// directory reported by that host. SSH/command failures and malformed output are errors.
pub fn resolve_repository(
    target: &ConnectionTarget,
    path: &Path,
) -> Result<Option<PathBuf>, Error> {
    resolve_repository_with_runner(target, path, &run)
}

/// Read only one directory level. No remote daemon, recursive scan, or shell
/// interpolation of the supplied path. NUL delimiters preserve spaces safely.
pub fn browse_directory(
    target: &ConnectionTarget,
    path: &Path,
) -> Result<(PathBuf, Vec<(String, bool)>), Error> {
    validate_target(target)?;
    if *target == ConnectionTarget::Local {
        let path = if path == Path::new("~") || path.starts_with("~/") {
            PathBuf::from(std::env::var("HOME").unwrap_or_default())
                .join(path.strip_prefix("~").unwrap())
        } else {
            path.to_path_buf()
        };
        let cwd = path.canonicalize().map_err(|source| Error::Spawn {
            operation: "browse directory",
            source,
        })?;
        let rd = std::fs::read_dir(&cwd).map_err(|source| Error::Spawn {
            operation: "browse directory",
            source,
        })?;
        let mut entries = Vec::new();
        for entry in rd.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            if !name.starts_with('.')
                && !name.chars().any(char::is_control)
                && entry.path().is_dir()
            {
                entries.push((name, true));
            }
        }
        entries.sort();
        entries.insert(0, ("..".into(), true));
        return Ok((cwd, entries));
    }
    let input = remote_repository_input(path)?;
    let script = format!("{REMOTE_REPOSITORY_SCRIPT}\nfor entry in ./*; do\n [ -d \"$entry\" ] || continue\n printf '%s\\000' \"${{entry#./}}\"\ndone\n");
    let invocation = target_invocation(
        target,
        &[
            "sh".into(),
            "-c".into(),
            script,
            "taskai-browse".into(),
            input.into(),
        ],
    )?;
    let output = run_browse(&invocation)?;
    if output.status.code() == Some(REMOTE_REPOSITORY_MISSING) {
        return Err(Error::InvalidInput(format!(
            "folder {} does not exist on {target}",
            path.display()
        )));
    }
    let output = require_success(output, "browse repository")?;
    parse_directory_listing(&output.stdout)
}

fn parse_directory_listing(bytes: &[u8]) -> Result<(PathBuf, Vec<(String, bool)>), Error> {
    if bytes.last() != Some(&0) {
        return Err(Error::InvalidOutput("incomplete directory listing".into()));
    }
    let split = bytes.iter().position(|&b| b == 0).unwrap();
    let cwd = parse_remote_repository(&bytes[..=split])?;
    let mut entries = Vec::new();
    for name in bytes[split + 1..]
        .split(|&b| b == 0)
        .filter(|s| !s.is_empty())
    {
        let name = std::str::from_utf8(name)
            .map_err(|_| Error::InvalidOutput("directory name is not UTF-8".into()))?;
        if name.contains('/') || name == "." || name == ".." || name.chars().any(char::is_control) {
            return Err(Error::InvalidOutput(
                "directory listing contained an unsafe name".into(),
            ));
        }
        entries.push((name.to_owned(), true));
    }
    entries.sort();
    entries.insert(0, ("..".into(), true));
    Ok((cwd, entries))
}

/// Browsing has a wall-clock deadline as well as SSH's connection timeout.
fn run_browse(invocation: &Invocation) -> Result<Output, Error> {
    run(invocation, "browse repository")
}

fn resolve_repository_with_runner<R>(
    target: &ConnectionTarget,
    path: &Path,
    runner: &R,
) -> Result<Option<PathBuf>, Error>
where
    R: Fn(&Invocation, &'static str) -> Result<Output, Error> + ?Sized,
{
    validate_target(target)?;
    match target {
        ConnectionTarget::Local => Ok(path.is_dir().then(|| path.to_path_buf())),
        ConnectionTarget::Ssh { .. } => {
            let input = remote_repository_input(path)?;
            let invocation = repository_resolution_invocation(target, input)?;
            let output = runner(&invocation, "resolve repository")?;
            if output.status.success() {
                parse_remote_repository(&output.stdout).map(Some)
            } else if output.status.code() == Some(REMOTE_REPOSITORY_MISSING) {
                Ok(None)
            } else {
                require_success(output, "resolve repository").map(|_| None)
            }
        }
    }
}

fn remote_repository_input(path: &Path) -> Result<&str, Error> {
    let path = path
        .to_str()
        .ok_or_else(|| Error::InvalidInput("remote repository path must be valid UTF-8".into()))?;
    if path == "~" || path.starts_with("~/") || path.starts_with('/') {
        Ok(path)
    } else {
        Err(Error::InvalidInput(
            "remote repository must be an absolute path, ~, or start with ~/".into(),
        ))
    }
}

fn repository_resolution_invocation(
    target: &ConnectionTarget,
    input: &str,
) -> Result<Invocation, Error> {
    target_invocation(
        target,
        &[
            "sh".into(),
            "-c".into(),
            REMOTE_REPOSITORY_SCRIPT.into(),
            "taskai-resolve-repository".into(),
            input.into(),
        ],
    )
}

fn parse_remote_repository(stdout: &[u8]) -> Result<PathBuf, Error> {
    let Some(path) = stdout.strip_suffix(&[0]) else {
        return Err(Error::InvalidOutput(
            "remote repository resolver returned an unterminated path".into(),
        ));
    };
    if path.is_empty() || path.contains(&0) {
        return Err(Error::InvalidOutput(
            "remote repository resolver returned an invalid path".into(),
        ));
    }
    let path = std::str::from_utf8(path)
        .map_err(|_| Error::InvalidOutput("remote repository path is not valid UTF-8".into()))?;
    if path.chars().any(char::is_control) {
        return Err(Error::InvalidOutput(
            "remote repository resolver returned terminal control characters".into(),
        ));
    }
    let path = PathBuf::from(path);
    if !path.is_absolute() {
        return Err(Error::InvalidOutput(
            "remote repository resolver did not return an absolute path".into(),
        ));
    }
    Ok(path)
}

/// Inspect the one native pane retained in a TaskAI tmux session.
pub fn inspect(locator: &TmuxRuntimeLocator) -> Result<RuntimeStatus, Error> {
    inspect_with_runner(locator, &run)
}

pub fn native_identity(locator: &TmuxRuntimeLocator) -> Result<Option<NativeIdentity>, Error> {
    validate_locator(locator)?;
    if locator.target != ConnectionTarget::Local {
        return Err(Error::InvalidInput(
            "native identity requires a local runtime".into(),
        ));
    }
    let mut command = tmux_prefix(locator);
    command.extend([
        "list-panes".into(),
        "-t".into(),
        native_pane_target(locator),
        "-F".into(),
        "#{pane_id}\t#{pane_pid}\t#{pane_dead}".into(),
    ]);
    let invocation = target_invocation(&locator.target, &command)?;
    let output = run(&invocation, "native identity")?;
    if output_means_missing(&output) {
        return Ok(None);
    }
    let output = require_success(output, "native identity")?;
    let line = std::str::from_utf8(&output.stdout)
        .map_err(|_| Error::InvalidOutput("native identity is not UTF-8".into()))?
        .trim_end_matches('\n');
    let mut fields = line.split('\t');
    let pane_id = fields.next().unwrap_or_default();
    let pid = fields
        .next()
        .unwrap_or_default()
        .parse::<u32>()
        .map_err(|_| Error::InvalidOutput("native pane has no numeric process ID".into()))?;
    let dead = fields.next().unwrap_or_default();
    if fields.next().is_some() || !pane_id.starts_with('%') || pane_id[1..].parse::<u32>().is_err()
    {
        return Err(Error::InvalidOutput(
            "native pane has malformed identity".into(),
        ));
    }
    if dead == "1" {
        return Ok(None);
    }
    if dead != "0" {
        return Err(Error::InvalidOutput(
            "native pane has malformed liveness".into(),
        ));
    }
    let Some(proc_start) = taskai_core::mail::proc_start(pid) else {
        return Ok(None);
    };
    Ok(Some(NativeIdentity {
        pane_id: pane_id.into(),
        pid,
        proc_start,
    }))
}

#[cfg(test)]
pub fn capture_test_terminal(locator: &TmuxRuntimeLocator) -> Result<String, Error> {
    validate_locator(locator)?;
    let mut command = tmux_prefix(locator);
    command.extend([
        "capture-pane".into(),
        "-p".into(),
        "-t".into(),
        native_pane_target(locator),
    ]);
    let invocation = target_invocation(&locator.target, &command)?;
    let output = require_success(
        run(&invocation, "capture test terminal")?,
        "capture test terminal",
    )?;
    Ok(String::from_utf8_lossy(&output.stdout).into_owned())
}

fn inspect_with_runner<R>(locator: &TmuxRuntimeLocator, runner: &R) -> Result<RuntimeStatus, Error>
where
    R: Fn(&Invocation, &'static str) -> Result<Output, Error> + ?Sized,
{
    let first = inspect_once_with_runner(locator, runner);
    let retryable = locator.target != ConnectionTarget::Local
        && matches!(
            &first,
            Err(Error::Timeout { .. })
                | Err(Error::CommandFailed {
                    code: Some(255),
                    ..
                })
        );
    if !retryable {
        return first;
    }
    let first_error = first.unwrap_err().to_string();
    // Inspection is read-only and exact-target. Retry one transient SSH failure
    // without retrying create/destroy or mistaking an unreachable host for a
    // missing runtime. Each attempt retains the ordinary 15-second deadline.
    inspect_once_with_runner(locator, runner).map_err(|mut error| {
        let previous = format!("; first inspection attempt also failed: {first_error}");
        match &mut error {
            Error::Timeout { detail, .. } => detail.push_str(&previous),
            Error::CommandFailed { stderr, .. } => stderr.push_str(&previous),
            _ => return Error::InvalidOutput(format!("{error}{previous}")),
        }
        error
    })
}

fn inspect_once_with_runner<R>(
    locator: &TmuxRuntimeLocator,
    runner: &R,
) -> Result<RuntimeStatus, Error>
where
    R: Fn(&Invocation, &'static str) -> Result<Output, Error> + ?Sized,
{
    validate_locator(locator)?;
    let mut tmux = tmux_prefix(locator);
    tmux.extend([
        "list-panes".into(),
        "-t".into(),
        native_pane_target(locator),
        "-F".into(),
        "#{pane_dead}\t#{pane_pid}\t#{pane_dead_status}\t#{window_width}\t#{window_height}".into(),
    ]);
    let invocation = target_invocation(&locator.target, &tmux)?;
    let output = runner(&invocation, "inspect")?;
    if output_means_missing(&output) {
        return Ok(RuntimeStatus::Missing);
    }
    let output = require_success(output, "inspect")?;
    parse_inspection(&output.stdout)
}

/// Configure only TaskAI's private server. Native mouse-aware applications keep
/// receiving mouse input via tmux's default bindings; other panes use tmux's
/// own history. The copy-mode overlay shows the history position without a
/// second scrollback buffer or polling service in TaskAI.
fn private_mouse_commands(locator: &TmuxRuntimeLocator) -> Vec<String> {
    let mut commands = vec![
        "set-option".into(),
        "-t".into(),
        // set-option parses a target-pane; the colon makes the exact session
        // explicit (plain =session is rejected by tmux 3.5a here).
        format!("{}:", exact_session(&locator.session)),
        "mouse".into(),
        "on".into(),
    ];
    for table in ["copy-mode", "copy-mode-vi"] {
        for key in ["Escape", "End"] {
            commands.extend([
                ";".into(),
                "bind-key".into(),
                "-T".into(),
                table.into(),
                key.into(),
                "send-keys".into(),
                "-X".into(),
                "cancel".into(),
            ]);
        }
    }
    commands
}

/// Build the disposable attach client launched inside TaskAI's portable PTY.
pub fn attach_command(locator: &TmuxRuntimeLocator) -> Result<AttachCommand, Error> {
    validate_locator(locator)?;
    let mut tmux = vec![
        "tmux".into(),
        "-N".into(),
        "-f".into(),
        "/dev/null".into(),
        "-L".into(),
        locator.private_server.clone(),
    ];
    // Reapply when attaching persisted sessions created by older versions.
    // These commands run on the locator's private server, never outer tmux.
    tmux.extend(private_mouse_commands(locator));
    tmux.extend([
        ";".into(),
        "attach-session".into(),
        "-E".into(),
        "-f".into(),
        "ignore-size".into(),
        "-t".into(),
        native_window_target(locator),
    ]);

    let mut owned_args = env_unset_args();
    match &locator.target {
        ConnectionTarget::Local => owned_args.append(&mut tmux),
        ConnectionTarget::Ssh { host } => {
            let remote = shell_join(&remote_env_command(tmux));
            owned_args.extend([
                "ssh".into(),
                "-tt".into(),
                "-e".into(),
                "none".into(),
                "-o".into(),
                "BatchMode=yes".into(),
                "-o".into(),
                format!("ConnectTimeout={SSH_CONNECT_TIMEOUT_SECS}"),
                "-o".into(),
                "ServerAliveInterval=5".into(),
                "-o".into(),
                "ServerAliveCountMax=2".into(),
                host.clone(),
                remote,
            ]);
        }
    }
    Ok(AttachCommand {
        program: "/usr/bin/env".into(),
        args: owned_args,
    })
}

/// Resize the exact single window without attaching another client.
///
/// Returns `false` if the recorded session no longer exists.
pub fn resize(locator: &TmuxRuntimeLocator, rows: u16, cols: u16) -> Result<bool, Error> {
    validate_locator(locator)?;
    if rows == 0 || cols == 0 {
        return Err(Error::InvalidInput(
            "runtime rows and columns must be nonzero".into(),
        ));
    }
    let mut tmux = tmux_prefix(locator);
    tmux.extend([
        "resize-window".into(),
        "-t".into(),
        native_window_target(locator),
        "-x".into(),
        cols.to_string(),
        "-y".into(),
        rows.to_string(),
    ]);
    optional_success_with_runner(locator, tmux, "resize", &run)
}

/// Kill the exact recorded session. The private server exits naturally when empty.
///
/// Returns `false` if the session was already missing. This never calls `kill-server`.
pub fn kill(locator: &TmuxRuntimeLocator) -> Result<bool, Error> {
    kill_with_runner(locator, &run)
}

fn kill_with_runner<R>(locator: &TmuxRuntimeLocator, runner: &R) -> Result<bool, Error>
where
    R: Fn(&Invocation, &'static str) -> Result<Output, Error> + ?Sized,
{
    validate_locator(locator)?;
    let mut tmux = tmux_prefix(locator);
    tmux.extend([
        "kill-session".into(),
        "-t".into(),
        exact_session(&locator.session),
    ]);
    optional_success_with_runner(locator, tmux, "kill", runner)
}

fn optional_success_with_runner<R>(
    locator: &TmuxRuntimeLocator,
    tmux: Vec<String>,
    operation: &'static str,
    runner: &R,
) -> Result<bool, Error>
where
    R: Fn(&Invocation, &'static str) -> Result<Output, Error> + ?Sized,
{
    let invocation = target_invocation(&locator.target, &tmux)?;
    let output = runner(&invocation, operation)?;
    if output.status.success() {
        Ok(true)
    } else if output_means_missing(&output) {
        Ok(false)
    } else {
        require_success(output, operation).map(|_| true)
    }
}

fn output_means_missing(output: &Output) -> bool {
    if output.status.code() != Some(1) {
        return false;
    }
    let stderr = String::from_utf8_lossy(&output.stderr);
    stderr.contains("can't find session:")
        || stderr.contains("can't find window:")
        || stderr.contains("can't find pane:")
        || stderr.contains("no server running on")
        || (stderr.contains("error connecting to")
            && (stderr.contains("No such file or directory")
                || stderr.contains("Connection refused")))
}

fn tmux_prefix(locator: &TmuxRuntimeLocator) -> Vec<String> {
    vec![
        "tmux".into(),
        "-f".into(),
        "/dev/null".into(),
        "-L".into(),
        locator.private_server.clone(),
    ]
}

fn native_window_target(locator: &TmuxRuntimeLocator) -> String {
    format!("{}:0", exact_session(&locator.session))
}

fn native_pane_target(locator: &TmuxRuntimeLocator) -> String {
    format!("{}:0.0", exact_session(&locator.session))
}

fn sidecar_window_target(locator: &TmuxRuntimeLocator) -> String {
    format!("{}:1", exact_session(&locator.session))
}

fn sidecar_pane_target(locator: &TmuxRuntimeLocator) -> String {
    format!("{}:1.0", exact_session(&locator.session))
}

fn target_invocation(target: &ConnectionTarget, command: &[String]) -> Result<Invocation, Error> {
    validate_target(target)?;
    match target {
        ConnectionTarget::Local => {
            let (program, args) = command
                .split_first()
                .ok_or_else(|| Error::InvalidInput("empty local command".into()))?;
            Ok(Invocation {
                program: program.clone(),
                args: args.to_vec(),
            })
        }
        ConnectionTarget::Ssh { host } => Ok(Invocation {
            program: "ssh".into(),
            args: vec![
                "-o".into(),
                "BatchMode=yes".into(),
                "-o".into(),
                format!("ConnectTimeout={SSH_CONNECT_TIMEOUT_SECS}"),
                host.clone(),
                shell_join(&remote_env_command(command.to_vec())),
            ],
        }),
    }
}

fn run(invocation: &Invocation, operation: &'static str) -> Result<Output, Error> {
    run_with_limits(invocation, operation, CONTROL_TIMEOUT, MAX_CONTROL_OUTPUT)
}

/// Bound both process execution and pipe draining. `Command::output()` can wait
/// forever after an SSH handshake or when a descendant retains an output pipe.
/// A timeout stops only this disposable control client, never a tmux runtime.
fn run_with_limits(
    invocation: &Invocation,
    operation: &'static str,
    timeout: Duration,
    max_output: usize,
) -> Result<Output, Error> {
    let mut command = Command::new(&invocation.program);
    command.args(&invocation.args);
    run_command_with_limits(command, operation, timeout, max_output)
}

fn run_command_with_limits(
    mut command: Command,
    operation: &'static str,
    timeout: Duration,
    max_output: usize,
) -> Result<Output, Error> {
    for name in STRIPPED_TMUX_ENV {
        command.env_remove(name);
    }
    taskai_core::mail::strip_messaging_env(&mut command);
    let mut child = command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|source| Error::Spawn { operation, source })?;
    let (sender, receiver) = mpsc::channel();
    let pipes: [Box<dyn Read + Send>; 2] = [
        Box::new(child.stdout.take().unwrap()),
        Box::new(child.stderr.take().unwrap()),
    ];
    for (index, pipe) in pipes.into_iter().enumerate() {
        let sender = sender.clone();
        std::thread::spawn(move || {
            let mut bytes = Vec::new();
            let mut pipe = pipe.take(max_output as u64 + 1);
            let result = (|| {
                let mut buffer = [0u8; 8192];
                loop {
                    match pipe.read(&mut buffer) {
                        Ok(0) => return Ok(()),
                        Ok(n) => {
                            bytes.extend_from_slice(&buffer[..n]);
                        }
                        Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                        Err(error) => return Err(error),
                    }
                }
            })();
            let _ = sender.send((index, result.map(|_| bytes)));
        });
    }
    drop(sender);
    let deadline = Instant::now() + timeout;
    let mut streams: [Option<Vec<u8>>; 2] = [None, None];
    let result = (|| {
        let mut status = None;
        loop {
            while let Ok((index, result)) = receiver.try_recv() {
                let bytes = result.map_err(|source| Error::Spawn { operation, source })?;
                if bytes.len() > max_output {
                    return Err(Error::InvalidOutput(format!(
                        "tmux {operation} output exceeded {max_output} bytes; use a narrower request"
                    )));
                }
                streams[index] = Some(bytes);
            }
            if status.is_none() {
                status = child
                    .try_wait()
                    .map_err(|source| Error::Spawn { operation, source })?;
            }
            if let Some(status) = status {
                if streams.iter().all(Option::is_some) {
                    return Ok(Output {
                        status,
                        stdout: streams[0].take().unwrap(),
                        stderr: streams[1].take().unwrap(),
                    });
                }
            }
            if Instant::now() >= deadline {
                return Err(Error::Timeout {
                    operation,
                    after: timeout,
                    detail: format!(
                        "control client {}; stdout {}, stderr {}",
                        status.map_or_else(
                            || "still running".into(),
                            |status| format!("exited {status}")
                        ),
                        if streams[0].is_some() {
                            "closed"
                        } else {
                            "open"
                        },
                        if streams[1].is_some() {
                            "closed"
                        } else {
                            "open"
                        },
                    ),
                });
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    })();
    if result.is_err() {
        let _ = child.kill();
        let _ = child.wait();
    }
    result
}

fn require_success(output: Output, operation: &'static str) -> Result<Output, Error> {
    if output.status.success() {
        return Ok(output);
    }
    Err(Error::CommandFailed {
        operation,
        code: output.status.code(),
        stderr: String::from_utf8_lossy(&output.stderr).trim().to_owned(),
    })
}

fn parse_inspection(stdout: &[u8]) -> Result<RuntimeStatus, Error> {
    let text = String::from_utf8_lossy(stdout);
    let mut lines = text.lines().filter(|line| !line.trim().is_empty());
    let line = lines
        .next()
        .ok_or_else(|| Error::InvalidOutput("tmux inspect returned no pane".into()))?;
    if lines.next().is_some() {
        return Err(Error::InvalidOutput(
            "TaskAI tmux session contains more than one pane".into(),
        ));
    }
    let mut fields = line.split('\t');
    let dead = fields.next();
    let pid = parse_field::<u32>(fields.next(), "pane PID")?;
    let dead_status = fields.next();
    let cols = parse_field::<u16>(fields.next(), "window width")?;
    let rows = parse_field::<u16>(fields.next(), "window height")?;
    if fields.next().is_some() {
        return Err(Error::InvalidOutput(format!(
            "unexpected tmux inspect response: {line:?}"
        )));
    }
    match dead {
        Some("0") => Ok(RuntimeStatus::Live { pid, cols, rows }),
        Some("1") => {
            let exit_code = match dead_status {
                Some("") | None => None,
                Some(value) => Some(value.parse::<i32>().map_err(|_| {
                    Error::InvalidOutput(format!("invalid pane exit status: {value:?}"))
                })?),
            };
            Ok(RuntimeStatus::Dead {
                pid,
                exit_code,
                cols,
                rows,
            })
        }
        value => Err(Error::InvalidOutput(format!(
            "invalid pane state: {value:?}"
        ))),
    }
}

fn parse_field<T: std::str::FromStr>(value: Option<&str>, name: &str) -> Result<T, Error> {
    value
        .and_then(|value| value.parse::<T>().ok())
        .ok_or_else(|| Error::InvalidOutput(format!("invalid {name} in tmux response")))
}

fn validate_locator(locator: &TmuxRuntimeLocator) -> Result<(), Error> {
    validate_target(&locator.target)?;
    validate_internal_name(&locator.private_server, "private tmux server")?;
    if let Some((server, key)) = locator
        .session
        .strip_prefix("setup-")
        .and_then(|name| name.rsplit_once('-'))
    {
        let server_is_internal = server == "local"
            || server.strip_prefix("server-").is_some_and(|number| {
                !number.is_empty() && number.bytes().all(|byte| byte.is_ascii_digit())
            });
        if server_is_internal
            && matches!(key, "terminal" | "claude" | "codex" | "opencode" | "grok")
        {
            return Ok(());
        }
    }
    let digits = locator
        .session
        .strip_prefix('s')
        .or_else(|| {
            locator
                .session
                .strip_prefix('t')
                .and_then(|s| s.strip_suffix("-terminal"))
        })
        .unwrap_or_default();
    if digits.is_empty() || !digits.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(Error::InvalidInput(format!(
            "tmux session must be an internal agent, task-terminal or server-setup ref, got {:?}",
            locator.session
        )));
    }
    Ok(())
}

fn validate_target(target: &ConnectionTarget) -> Result<(), Error> {
    let ConnectionTarget::Ssh { host } = target else {
        return Ok(());
    };
    validate_internal_name(host, "SSH host alias")
}

fn validate_internal_name(value: &str, kind: &str) -> Result<(), Error> {
    let valid = !value.is_empty()
        && !value.starts_with('-')
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'));
    if valid {
        Ok(())
    } else {
        Err(Error::InvalidInput(format!("invalid {kind}: {value:?}")))
    }
}

fn validate_environment_name(name: &str) -> Result<(), Error> {
    let mut bytes = name.bytes();
    let first = bytes.next();
    let valid = first.is_some_and(|byte| byte.is_ascii_alphabetic() || byte == b'_')
        && bytes.all(|byte| byte.is_ascii_alphanumeric() || byte == b'_');
    if valid {
        Ok(())
    } else {
        Err(Error::InvalidInput(format!(
            "invalid environment variable name: {name:?}"
        )))
    }
}

fn exact_session(session: &str) -> String {
    format!("={session}")
}

fn env_unset_args() -> Vec<String> {
    STRIPPED_TMUX_ENV
        .iter()
        .flat_map(|name| ["-u".into(), (*name).into()])
        .collect()
}

fn remote_env_command(mut command: Vec<String>) -> Vec<String> {
    let mut remote = vec!["env".into()];
    remote.extend(env_unset_args());
    remote.append(&mut command);
    remote
}

fn shell_join(args: &[String]) -> String {
    args.iter()
        .map(|arg| posix_quote(arg))
        .collect::<Vec<_>>()
        .join(" ")
}

fn posix_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

#[cfg(test)]
mod tests {
    use std::{
        cell::RefCell,
        collections::VecDeque,
        env, fs,
        os::unix::ffi::OsStringExt,
        path::PathBuf,
        sync::atomic::{AtomicU64, Ordering},
        thread,
        time::{Duration, Instant},
    };

    use taskai_core::registry::{Registry, Source};
    use taskai_pane::grid::Cell;
    use taskai_pane::{MouseProtocolEncoding, MouseProtocolMode, Pane};
    use tempfile::TempDir;

    use super::*;

    #[test]
    fn directory_listing_preserves_spaces_and_rejects_control_sequences() {
        let (cwd, entries) =
            parse_directory_listing(b"/home/ubuntu/github\0my project\0smb-deal-flow\0").unwrap();
        assert_eq!(cwd, Path::new("/home/ubuntu/github"));
        assert_eq!(entries[1].0, "my project");
        for malformed in [
            b"/home\0bad\x1b[2J\0".as_slice(),
            b"/home\0../escape\0",
            b"/home\0unterminated",
        ] {
            assert!(parse_directory_listing(malformed).is_err());
        }
    }

    #[test]
    #[ignore = "read-only SSH acceptance check; requires cruise2"]
    fn cruise2_directory_picker_reads_real_folders() {
        let target = ConnectionTarget::ssh("cruise2").unwrap();
        let (home, _) = browse_directory(&target, Path::new("~")).unwrap();
        assert!(home.is_absolute());
        let (cwd, entries) = browse_directory(&target, Path::new("~/github")).unwrap();
        assert_eq!(cwd, home.join("github"));
        assert!(entries.len() > 1);
        assert!(browse_directory(
            &target,
            Path::new("/tmp/taskai-nonexistent-picker-path-7d91")
        )
        .is_err());
        assert!(
            browse_directory(&target, Path::new("/tmp/$(printf taskai-picker-injection)")).is_err()
        );
    }

    static NEXT_NAMESPACE: AtomicU64 = AtomicU64::new(1);

    fn namespace() -> String {
        format!(
            "taskai-test-{}-{}",
            std::process::id(),
            NEXT_NAMESPACE.fetch_add(1, Ordering::Relaxed)
        )
    }

    // A controlled login profile: noisy startup, user-installed binaries added to
    // PATH, and a cwd change. It runs no real provider or user startup configuration.
    const PROVIDER_TEST_SHELL: &str = r#"#!/bin/sh
printf 'profile banner 99.99.99\n'
printf 'profile warning\n' >&2
PATH="$TASKAI_TEST_PROVIDER_BIN:/usr/bin:/bin"
export PATH
cd / || exit
[ "$1" = '-lic' ] || exit 19
shift
taskai_test_script=$1
shift
exec /bin/sh -c "$taskai_test_script" "$@"
"#;

    #[cfg(unix)]
    #[test]
    fn provider_login_wrapper_preserves_argv_environment_cwd_and_clean_output() {
        use std::os::unix::fs::PermissionsExt;

        let temp = TempDir::new().unwrap();
        let bin = temp.path().join("provider bin");
        fs::create_dir(&bin).unwrap();
        let shell = temp.path().join("login shell");
        fs::write(&shell, PROVIDER_TEST_SHELL).unwrap();
        fs::set_permissions(&shell, fs::Permissions::from_mode(0o700)).unwrap();
        let provider = bin.join("provider with spaces");
        fs::write(&provider, "#!/bin/sh\nprintf '<%s>\\n' \"$@\"; printf 'cwd=%s\\nref=%s\\n' \"$PWD\" \"$TASKAI_SESSION_REF\"; printf 'provider stderr\\n' >&2\n").unwrap();
        fs::set_permissions(&provider, fs::Permissions::from_mode(0o700)).unwrap();
        let target = ConnectionTarget::ssh("fixture").unwrap();
        let args = vec![
            "--resume".into(),
            "literal ' \" ; $(touch TASKAI_UNEXPECTED) $HOME".into(),
            String::new(),
        ];
        for program in ["provider with spaces", provider.to_str().unwrap()] {
            let command = provider_launch_command(&target, program, &args).unwrap();
            let output = Command::new(&command.program)
                .args(&command.args)
                .env("SHELL", &shell)
                .env("PATH", "/usr/bin:/bin")
                .env("TASKAI_TEST_PROVIDER_BIN", &bin)
                .env("TASKAI_SESSION_REF", "s42")
                .current_dir(temp.path())
                .output()
                .unwrap();
            assert!(output.status.success(), "{output:?}");
            assert_eq!(
                String::from_utf8(output.stdout).unwrap(),
                format!("<--resume>\n<literal ' \" ; $(touch TASKAI_UNEXPECTED) $HOME>\n<>\ncwd={}\nref=s42\n", fs::canonicalize(temp.path()).unwrap().display())
            );
            assert_eq!(output.stderr, b"provider stderr\n");
            assert!(!temp.path().join("TASKAI_UNEXPECTED").exists());
        }
        assert_eq!(
            provider_launch_command(&ConnectionTarget::Local, "local cli", &args).unwrap(),
            AttachCommand {
                program: "local cli".into(),
                args
            }
        );
    }

    #[test]
    fn provider_login_wrapper_does_not_execute_shell_functions() {
        // The real target's shell may define shortcuts with provider names. The
        // dynamic exec must find an executable, not turn a function into READY.
        if !Path::new("/bin/zsh").is_file() {
            return;
        }
        let temp = TempDir::new().unwrap();
        fs::write(
            temp.path().join(".zshrc"),
            "taskai_missing_provider() { printf '99.0.0 function-only'; }\nalias taskai_missing_provider='printf alias-only'\n",
        )
        .unwrap();
        let target = ConnectionTarget::ssh("fixture").unwrap();
        let command = provider_launch_command(&target, "taskai_missing_provider", &[]).unwrap();
        let output = Command::new(&command.program)
            .args(&command.args)
            .env("SHELL", "/bin/zsh")
            .env("ZDOTDIR", temp.path())
            .output()
            .unwrap();
        assert!(!output.status.success());
        assert!(output.stdout.is_empty());
    }

    #[test]
    fn provider_home_relative_executable_is_resolved_without_shell_evaluation() {
        let local = provider_launch_command(&ConnectionTarget::Local, "~/bin/cli with spaces", &[])
            .unwrap();
        assert_eq!(
            local.program,
            PathBuf::from(std::env::var_os("HOME").unwrap())
                .join("bin/cli with spaces")
                .to_string_lossy()
        );
        let remote = provider_launch_command(
            &ConnectionTarget::ssh("fixture").unwrap(),
            "~/bin/cli with spaces",
            &["literal $HOME".into()],
        )
        .unwrap();
        assert_eq!(
            &remote.args[remote.args.len() - 2..],
            ["~/bin/cli with spaces", "literal $HOME"]
        );
        assert!(remote.args[1].contains("taskai_program="));
    }

    #[derive(Default)]
    struct RecordingRunner {
        calls: RefCell<Vec<(&'static str, Invocation)>>,
        outputs: RefCell<VecDeque<Output>>,
    }

    impl RecordingRunner {
        fn with_outputs(outputs: impl IntoIterator<Item = Output>) -> Self {
            Self {
                calls: RefCell::new(Vec::new()),
                outputs: RefCell::new(outputs.into_iter().collect()),
            }
        }

        fn execute(
            &self,
            invocation: &Invocation,
            operation: &'static str,
        ) -> Result<Output, Error> {
            self.calls
                .borrow_mut()
                .push((operation, invocation.clone()));
            Ok(self
                .outputs
                .borrow_mut()
                .pop_front()
                .expect("recording runner needs one output per call"))
        }

        fn calls(&self) -> Vec<(&'static str, Invocation)> {
            self.calls.borrow().clone()
        }
    }

    fn fake_output(code: i32, stdout: &str, stderr: &str) -> Output {
        let mut output = Command::new("/bin/sh")
            .arg("-c")
            .arg(format!("exit {code}"))
            .output()
            .unwrap();
        output.stdout = stdout.as_bytes().to_vec();
        output.stderr = stderr.as_bytes().to_vec();
        output
    }

    fn local_locator(server: &str, session: &str) -> TmuxRuntimeLocator {
        TmuxRuntimeLocator {
            target: ConnectionTarget::Local,
            private_server: server.into(),
            session: session.into(),
        }
    }

    fn tmux_available() -> bool {
        probe(&ConnectionTarget::Local).is_ok()
    }

    fn eventually(predicate: impl FnMut() -> bool) -> bool {
        // PTYs and tmux redraw asynchronously; readiness, not CPU scheduling
        // within two seconds, is the behavior under test in a parallel suite.
        eventually_within(Duration::from_secs(10), predicate)
    }

    fn eventually_within(timeout: Duration, mut predicate: impl FnMut() -> bool) -> bool {
        let deadline = Instant::now() + timeout;
        loop {
            if predicate() {
                return true;
            }
            if Instant::now() >= deadline {
                return false;
            }
            thread::sleep(Duration::from_millis(50));
        }
    }

    struct RuntimeGuard(TmuxRuntimeLocator);

    impl Drop for RuntimeGuard {
        fn drop(&mut self) {
            let _ = kill(&self.0);
        }
    }

    struct RemoteDirGuard {
        target: ConnectionTarget,
        path: PathBuf,
    }

    impl Drop for RemoteDirGuard {
        fn drop(&mut self) {
            let Some(path) = self.path.to_str() else {
                return;
            };
            if !path.starts_with("/tmp/taskai-remote-test-") {
                return;
            }
            if let Ok(invocation) = target_invocation(
                &self.target,
                &["rm".into(), "-rf".into(), "--".into(), path.into()],
            ) {
                let _ = run(&invocation, "clean remote test directory");
            }
        }
    }

    fn target_output(target: &ConnectionTarget, command: &[String]) -> Output {
        let invocation = target_invocation(target, command).unwrap();
        run(&invocation, "remote test command").unwrap()
    }

    fn remote_file(target: &ConnectionTarget, path: &Path) -> Option<String> {
        let output = target_output(target, &["cat".into(), path.to_string_lossy().into_owned()]);
        output
            .status
            .success()
            .then(|| String::from_utf8_lossy(&output.stdout).into_owned())
    }

    fn default_tmux_snapshot(target: &ConnectionTarget) -> (Option<i32>, Vec<u8>, Vec<u8>) {
        let output = target_output(
            target,
            &[
                "tmux".into(),
                "list-sessions".into(),
                "-F".into(),
                "#{session_name}".into(),
            ],
        );
        (output.status.code(), output.stdout, output.stderr)
    }

    fn attached_client_count(locator: &TmuxRuntimeLocator) -> Option<usize> {
        let mut command = tmux_prefix(locator);
        command.extend([
            "list-clients".into(),
            "-t".into(),
            exact_session(&locator.session),
            "-F".into(),
            "#{client_pid}".into(),
        ]);
        let invocation = target_invocation(&locator.target, &command).ok()?;
        let output = run(&invocation, "count test clients").ok()?;
        output.status.success().then(|| {
            String::from_utf8_lossy(&output.stdout)
                .lines()
                .filter(|line| !line.trim().is_empty())
                .count()
        })
    }

    fn assert_cell_grids_equal(expected: &[Vec<Cell>], actual: &[Vec<Cell>], label: &str) {
        assert_eq!(actual.len(), expected.len(), "{label}: row count changed");
        for (row, (expected, actual)) in expected.iter().zip(actual).enumerate() {
            assert_eq!(
                actual.len(),
                expected.len(),
                "{label}: cell count changed on row {row}"
            );
            for (column, (expected, actual)) in expected.iter().zip(actual).enumerate() {
                assert!(
                    actual.text == expected.text
                        && actual.fg == expected.fg
                        && actual.bg == expected.bg
                        && actual.bold == expected.bold
                        && actual.italic == expected.italic
                        && actual.underline == expected.underline
                        && actual.inverse == expected.inverse
                        && actual.width == expected.width,
                    "{label}: cell ({row},{column}) changed: expected ({:?}, {:?}, {:?}, {}, {}), got ({:?}, {:?}, {:?}, {}, {})",
                    expected.text,
                    expected.fg,
                    expected.bg,
                    expected.bold,
                    expected.width,
                    actual.text,
                    actual.fg,
                    actual.bg,
                    actual.bold,
                    actual.width,
                );
            }
        }
    }

    #[test]
    fn validates_locators_and_quotes_every_remote_argument() {
        let target = ConnectionTarget::Ssh {
            host: "cruise2".into(),
        };
        let command = vec![
            "tmux".into(),
            "-L".into(),
            "taskai-safe".into(),
            "display-message".into(),
            "apostrophe ' and ; are data".into(),
        ];
        let invocation = target_invocation(&target, &command).unwrap();
        assert_eq!(invocation.program, "ssh");
        assert_eq!(invocation.args[4], "cruise2");
        assert_eq!(
            invocation.args[5],
            "'env' '-u' 'TMUX' '-u' 'TMUX_PANE' '-u' 'TMUX_TMPDIR' 'tmux' '-L' 'taskai-safe' 'display-message' 'apostrophe '\"'\"' and ; are data'"
        );

        let bad_host = ConnectionTarget::Ssh {
            host: "-oProxyCommand=bad".into(),
        };
        assert!(target_invocation(&bad_host, &command).is_err());
        let bad_session = TmuxRuntimeLocator {
            target: ConnectionTarget::Local,
            private_server: "taskai-safe".into(),
            session: "user supplied".into(),
        };
        assert!(attach_command(&bad_session).is_err());

        let ordinary_failure = Command::new("/bin/sh")
            .args(["-c", "printf 'permission denied\\n' >&2; exit 1"])
            .output()
            .unwrap();
        assert!(!output_means_missing(&ordinary_failure));
        let missing = Command::new("/bin/sh")
            .args(["-c", "printf \"can't find session: s42\\n\" >&2; exit 1"])
            .output()
            .unwrap();
        assert!(output_means_missing(&missing));
    }

    #[test]
    fn settings_locators_accept_only_internal_server_ids_and_known_utilities() {
        for session in [
            "setup-local-terminal",
            "setup-server-1-claude",
            "setup-server-42-opencode",
            "setup-local-codex",
            "setup-server-3-grok",
        ] {
            let locator = local_locator("taskai-settings-test", session);
            assert!(attach_command(&locator).is_ok(), "{session}");
        }
        for session in [
            "setup-other-terminal",
            "setup-server--terminal",
            "setup-local-unknown",
            "setup-local-terminal;kill-server",
            "setup-server-1-codex:0",
        ] {
            assert!(
                attach_command(&local_locator("taskai-settings-test", session)).is_err(),
                "{session}"
            );
        }
    }

    #[test]
    fn authentication_status_routes_exact_argv_and_rejects_mutating_commands() {
        let remote = ConnectionTarget::ssh("fixture-host").unwrap();
        let runner = RecordingRunner::with_outputs([fake_output(1, "", "Not logged in")]);
        let output = probe_auth_status_with_runner(
            &remote,
            "/path/cli with ' spaces",
            &["login".into(), "status".into()],
            &|invocation, operation| runner.execute(invocation, operation),
        )
        .unwrap();
        assert_eq!(output.status.code(), Some(1));
        let calls = runner.calls();
        assert_eq!(calls[0].1.program, "ssh");
        assert!(calls[0]
            .1
            .args
            .last()
            .unwrap()
            .ends_with("'/path/cli with '\"'\"' spaces' 'login' 'status'"));
        let local_runner =
            RecordingRunner::with_outputs([fake_output(0, "{\"loggedIn\":true}", "")]);
        probe_auth_status_with_runner(
            &ConnectionTarget::Local,
            "/local custom cli",
            &["auth".into(), "status".into(), "--json".into()],
            &|invocation, operation| local_runner.execute(invocation, operation),
        )
        .unwrap();
        let local = local_runner.calls();
        assert_eq!(local[0].1.program, "/local custom cli");
        assert_eq!(local[0].1.args, ["auth", "status", "--json"]);
        for args in [
            vec!["login".into()],
            vec!["auth".into(), "logout".into()],
            vec!["models".into()],
        ] {
            assert!(
                probe_auth_status_with_runner(&remote, "claude", &args, &|_, _| panic!(
                    "unsafe auth operation reached runner"
                ))
                .is_err()
            );
        }
    }

    #[test]
    fn control_command_timeout_does_not_wait_forever() {
        let invocation = Invocation {
            program: "/bin/sh".into(),
            args: vec!["-c".into(), "exec sleep 30".into()],
        };
        let started = Instant::now();
        assert!(matches!(
            run_with_limits(
                &invocation,
                "test stalled command",
                Duration::from_millis(100),
                1024
            ),
            Err(Error::Timeout {
                operation: "test stalled command",
                ..
            })
        ));
        assert!(started.elapsed() < Duration::from_secs(2));
    }

    #[test]
    fn control_deadline_also_bounds_pipes_inherited_by_a_descendant() {
        let invocation = Invocation {
            program: "/bin/sh".into(),
            args: vec!["-c".into(), "sleep 1 & exit 0".into()],
        };
        let started = Instant::now();
        assert!(matches!(
            run_with_limits(
                &invocation,
                "test inherited pipes",
                Duration::from_millis(100),
                1024,
            ),
            Err(Error::Timeout { .. })
        ));
        assert!(started.elapsed() < Duration::from_millis(900));
    }

    #[test]
    fn control_command_drains_both_pipes_and_bounds_output() {
        let invocation = Invocation {
            program: "/bin/sh".into(),
            args: vec![
                "-c".into(),
                "head -c 65536 /dev/zero; head -c 65536 /dev/zero >&2".into(),
            ],
        };
        let output =
            run_with_limits(&invocation, "test output", Duration::from_secs(5), 131072).unwrap();
        assert!(output.status.success());
        assert_eq!(output.stdout.len(), 65536);
        assert_eq!(output.stderr.len(), 65536);
        assert!(matches!(
            run_with_limits(
                &invocation,
                "test excess output",
                Duration::from_secs(5),
                1024
            ),
            Err(Error::InvalidOutput(_))
        ));
    }

    // AT-09. Expectation: the exact same create/inspect/destroy operations used in
    // production are routed through SSH for a recorded remote target, while a local
    // target never invokes SSH. Why: a plausible-looking helper command is insufficient
    // if one lifecycle operation can accidentally fall back to the local machine. Value:
    // this proves routing, argument boundaries, remote cwd, and the disposable attach
    // command without opening a network connection or starting a provider.
    #[test]
    fn at_09_injected_runner_routes_the_complete_lifecycle_to_its_recorded_target() {
        let remote = TmuxRuntimeLocator {
            target: ConnectionTarget::ssh("cruise2").unwrap(),
            private_server: "taskai-at09".into(),
            session: "s42".into(),
        };
        let remote_runner = RecordingRunner::with_outputs([
            fake_output(0, "", ""),
            fake_output(0, "0\t4242\t\t91\t31\n", ""),
            fake_output(0, "", ""),
        ]);
        let remote_execute = |invocation: &Invocation, operation: &'static str| {
            remote_runner.execute(invocation, operation)
        };
        let remote_repo = Path::new("/srv/repo with an ' apostrophe");
        create_with_runner(
            &remote,
            "/opt/fake cli",
            &["literal ; $HOME ' data".into()],
            remote_repo,
            &[("TASKAI_TEST_VALUE".into(), "remote value".into())],
            31,
            91,
            &remote_execute,
        )
        .unwrap();
        assert_eq!(
            inspect_with_runner(&remote, &remote_execute).unwrap(),
            RuntimeStatus::Live {
                pid: 4242,
                cols: 91,
                rows: 31
            }
        );
        assert!(kill_with_runner(&remote, &remote_execute).unwrap());

        let calls = remote_runner.calls();
        assert_eq!(
            calls
                .iter()
                .map(|(operation, _)| *operation)
                .collect::<Vec<_>>(),
            ["create", "inspect", "kill"]
        );
        for (_, invocation) in &calls {
            assert_eq!(invocation.program, "ssh");
            assert_eq!(
                invocation
                    .args
                    .iter()
                    .take(5)
                    .map(String::as_str)
                    .collect::<Vec<_>>(),
                ["-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "cruise2"]
            );
        }
        let create_command = calls[0].1.args.last().unwrap();
        assert!(create_command.contains("'new-session' '-d' '-s' 's42'"));
        assert!(create_command.contains("'-c' '/srv/repo with an '\"'\"' apostrophe'"));
        assert!(create_command.contains("'/opt/fake cli' 'literal ; $HOME '\"'\"' data'"));
        assert!(create_command.contains("'-e' 'TASKAI_TEST_VALUE=remote value'"));
        assert!(calls[1]
            .1
            .args
            .last()
            .unwrap()
            .contains("'list-panes' '-t' '=s42:0.0'"));
        assert!(calls[2]
            .1
            .args
            .last()
            .unwrap()
            .contains("'kill-session' '-t' '=s42'"));

        let attach = attach_command(&remote).unwrap();
        assert_eq!(attach.program, "/usr/bin/env");
        assert_eq!(
            &attach.args[6..19],
            [
                "ssh",
                "-tt",
                "-e",
                "none",
                "-o",
                "BatchMode=yes",
                "-o",
                "ConnectTimeout=10",
                "-o",
                "ServerAliveInterval=5",
                "-o",
                "ServerAliveCountMax=2",
                "cruise2",
            ]
        );
        assert!(attach
            .args
            .last()
            .unwrap()
            .contains("'attach-session' '-E' '-f' 'ignore-size' '-t' '=s42:0'"));

        let local = local_locator("taskai-at09-local", "s9");
        let local_runner = RecordingRunner::with_outputs([
            fake_output(0, "", ""),
            fake_output(0, "0\t99\t\t80\t24\n", ""),
            fake_output(0, "", ""),
        ]);
        let local_execute = |invocation: &Invocation, operation: &'static str| {
            local_runner.execute(invocation, operation)
        };
        create_with_runner(
            &local,
            "/bin/fake",
            &[],
            Path::new("/tmp/local repo"),
            &[],
            24,
            80,
            &local_execute,
        )
        .unwrap();
        assert!(matches!(
            inspect_with_runner(&local, &local_execute).unwrap(),
            RuntimeStatus::Live { pid: 99, .. }
        ));
        assert!(kill_with_runner(&local, &local_execute).unwrap());
        let local_calls = local_runner.calls();
        assert!(local_calls
            .iter()
            .all(|(_, invocation)| invocation.program == "tmux"));
        let cwd = local_calls[0]
            .1
            .args
            .iter()
            .position(|arg| arg == "-c")
            .expect("local create has a working-directory argument");
        assert_eq!(local_calls[0].1.args[cwd + 1], "/tmp/local repo");
        let local_attach = attach_command(&local).unwrap();
        assert!(local_attach.args.iter().any(|arg| arg == "tmux"));
        assert!(!local_attach.args.iter().any(|arg| arg == "ssh"));
    }

    // AT-11. Expectation: an SSH transport failure is an error, while tmux's exact
    // missing-session response is Missing. Neither inspection mutates the durable
    // locator, launches a replacement, nor invokes local tmux. Why: unreachable hosts
    // must remain retryable and must never cause work to start on the wrong machine.
    // Value: reconciliation has deterministic evidence without relying on DNS/network.
    #[test]
    fn at_11_remote_failure_is_distinct_from_missing_and_retains_the_mapping() {
        let root = TempDir::new().unwrap();
        let state = root.path().join("state");
        let repo = Path::new("/srv/taskai-at11-repository");
        let target = ConnectionTarget::ssh("cruise2").unwrap();
        let mut registry = Registry::open(&state).unwrap();
        let task = registry
            .create_task_on("Remote", repo, target, Source::HumanUi)
            .unwrap();
        let session = registry
            .create_session(&task, None, "Lead", "claude", repo, Source::HumanUi)
            .unwrap();
        registry.bind_runtime(&session, "taskai-at11").unwrap();
        let locator = registry
            .session(&session)
            .and_then(|record| record.runtime.clone())
            .unwrap();

        let unreachable_runner = RecordingRunner::with_outputs((0..2).map(|_| {
            fake_output(
                255,
                "",
                "ssh: connect to host cruise2 port 22: Operation timed out\n",
            )
        }));
        let unreachable_execute = |invocation: &Invocation, operation: &'static str| {
            unreachable_runner.execute(invocation, operation)
        };
        let unreachable = inspect_with_runner(&locator, &unreachable_execute);
        assert!(unreachable
            .as_ref()
            .unwrap_err()
            .to_string()
            .contains("Operation timed out"));
        assert!(matches!(
            unreachable,
            Err(Error::CommandFailed {
                operation: "inspect",
                code: Some(255),
                ..
            })
        ));

        let missing_runner =
            RecordingRunner::with_outputs([fake_output(1, "", "can't find session: s1\n")]);
        let missing_execute = |invocation: &Invocation, operation: &'static str| {
            missing_runner.execute(invocation, operation)
        };
        assert_eq!(
            inspect_with_runner(&locator, &missing_execute).unwrap(),
            RuntimeStatus::Missing
        );

        for (runner, expected_calls) in [(&unreachable_runner, 2), (&missing_runner, 1)] {
            let calls = runner.calls();
            assert_eq!(
                calls.len(),
                expected_calls,
                "only a transient SSH failure is retried"
            );
            assert_eq!(calls[0].0, "inspect");
            assert_eq!(calls[0].1.program, "ssh", "must not fall back locally");
            let remote_command = calls[0].1.args.last().unwrap();
            assert!(remote_command.contains("'list-panes' '-t' '=s1:0.0'"));
            assert!(!remote_command.contains("'new-session'"));
        }

        assert_eq!(
            registry.session(&session).unwrap().runtime.as_ref(),
            Some(&locator)
        );
        drop(registry);
        let reopened = Registry::open(&state).unwrap();
        assert_eq!(
            reopened.session(&session).unwrap().runtime.as_ref(),
            Some(&locator),
            "the failed/missing remote locator remains durable for a later retry"
        );
    }

    #[test]
    fn remote_inspection_retries_one_transient_failure_without_mutating_runtime() {
        let locator = TmuxRuntimeLocator {
            target: ConnectionTarget::ssh("retry-host").unwrap(),
            private_server: "taskai-retry-test".into(),
            session: "s1".into(),
        };
        let calls = RefCell::new(0usize);
        let execute = |invocation: &Invocation, operation| {
            assert_eq!(operation, "inspect");
            assert_eq!(invocation.program, "ssh");
            assert!(invocation
                .args
                .last()
                .unwrap()
                .contains("'list-panes' '-t' '=s1:0.0'"));
            let mut count = calls.borrow_mut();
            *count += 1;
            if *count == 1 {
                Err(Error::Timeout {
                    operation,
                    after: CONTROL_TIMEOUT,
                    detail: "first transport stalled".into(),
                })
            } else {
                Ok(fake_output(0, "0\t1234\t\t80\t24\n", ""))
            }
        };
        assert!(matches!(
            inspect_with_runner(&locator, &execute).unwrap(),
            RuntimeStatus::Live { pid: 1234, .. }
        ));
        assert_eq!(*calls.borrow(), 2);

        let twice = RefCell::new(0usize);
        let execute = |_: &Invocation, operation| {
            *twice.borrow_mut() += 1;
            Err(Error::Timeout {
                operation,
                after: CONTROL_TIMEOUT,
                detail: format!("stall {}", twice.borrow()),
            })
        };
        let error = inspect_with_runner(&locator, &execute)
            .unwrap_err()
            .to_string();
        assert_eq!(*twice.borrow(), 2);
        assert!(error.contains("stall 1") && error.contains("stall 2"));
        assert!(error.contains("first inspection attempt also failed"));

        let mut local = locator;
        local.target = ConnectionTarget::Local;
        *twice.borrow_mut() = 0;
        assert!(inspect_with_runner(&local, &execute).is_err());
        assert_eq!(
            *twice.borrow(),
            1,
            "local errors do not trigger SSH retry policy"
        );
    }

    #[test]
    fn repository_resolution_keeps_connection_and_path_separate() {
        let temp = TempDir::new().unwrap();
        assert_eq!(
            resolve_repository(&ConnectionTarget::Local, temp.path()).unwrap(),
            Some(temp.path().to_path_buf())
        );
        assert_eq!(
            resolve_repository(&ConnectionTarget::Local, Path::new(".")).unwrap(),
            Some(PathBuf::from(".")),
            "local paths retain their current, non-canonicalizing behavior"
        );
        assert_eq!(
            resolve_repository(&ConnectionTarget::Local, &temp.path().join("missing")).unwrap(),
            None
        );

        let target = ConnectionTarget::Ssh {
            host: "cruise2".into(),
        };
        let input = "~/repo with an ' apostrophe; $(still-data)";
        let invocation = repository_resolution_invocation(&target, input).unwrap();
        assert_eq!(invocation.program, "ssh");
        let remote_command = invocation.args.last().unwrap();
        assert!(remote_command.contains(&posix_quote(REMOTE_REPOSITORY_SCRIPT)));
        assert!(
            remote_command.ends_with(&format!(
                "{} {}",
                posix_quote("taskai-resolve-repository"),
                posix_quote(input)
            )),
            "the user path must be one safely quoted positional argument: {remote_command}"
        );
    }

    #[test]
    fn injected_remote_resolution_accepts_home_and_absolute_paths() {
        let target = ConnectionTarget::ssh("cruise2").unwrap();
        for (input, resolved) in [
            ("~", "/home/remote"),
            ("~/src/repository", "/home/remote/src/repository"),
            ("/srv/repository-link", "/real/repository"),
        ] {
            let stdout = format!("{resolved}\0");
            let runner = RecordingRunner::with_outputs([fake_output(0, &stdout, "")]);
            let execute = |invocation: &Invocation, operation: &'static str| {
                runner.execute(invocation, operation)
            };
            assert_eq!(
                resolve_repository_with_runner(&target, Path::new(input), &execute).unwrap(),
                Some(PathBuf::from(resolved))
            );
            let calls = runner.calls();
            assert_eq!(calls.len(), 1);
            assert_eq!(calls[0].0, "resolve repository");
            assert_eq!(calls[0].1.program, "ssh");
            assert!(calls[0].1.args.last().unwrap().ends_with(&format!(
                "{} {}",
                posix_quote("taskai-resolve-repository"),
                posix_quote(input)
            )));
        }
    }

    #[test]
    fn remote_resolution_rejects_relative_paths_without_invoking_ssh() {
        let target = ConnectionTarget::ssh("cruise2").unwrap();
        let runner = RecordingRunner::default();
        let execute = |invocation: &Invocation, operation: &'static str| {
            runner.execute(invocation, operation)
        };
        for input in ["", "repo", "./repo", "../repo", "~other/repo"] {
            assert!(matches!(
                resolve_repository_with_runner(&target, Path::new(input), &execute),
                Err(Error::InvalidInput(_))
            ));
        }
        assert!(
            runner.calls().is_empty(),
            "invalid paths must fail before SSH"
        );
    }

    #[test]
    fn remote_resolution_distinguishes_missing_failure_and_invalid_output() {
        let target = ConnectionTarget::ssh("cruise2").unwrap();

        let missing =
            RecordingRunner::with_outputs([fake_output(REMOTE_REPOSITORY_MISSING, "", "")]);
        assert_eq!(
            resolve_repository_with_runner(
                &target,
                Path::new("~/missing"),
                &|invocation, operation| missing.execute(invocation, operation),
            )
            .unwrap(),
            None
        );

        let unreachable = RecordingRunner::with_outputs([fake_output(
            255,
            "",
            "ssh: connect to host cruise2: Operation timed out",
        )]);
        assert!(matches!(
            resolve_repository_with_runner(&target, Path::new("~"), &|invocation, operation| {
                unreachable.execute(invocation, operation)
            },),
            Err(Error::CommandFailed {
                operation: "resolve repository",
                code: Some(255),
                ..
            })
        ));

        for malformed in [
            "/not-terminated",
            "relative\0",
            "/two\0paths\0",
            "/escape\x1b[2J\0",
            "/line\nbreak\0",
        ] {
            let runner = RecordingRunner::with_outputs([fake_output(0, malformed, "")]);
            assert!(matches!(
                resolve_repository_with_runner(
                    &target,
                    Path::new("/input"),
                    &|invocation, operation| runner.execute(invocation, operation),
                ),
                Err(Error::InvalidOutput(_))
            ));
        }
    }

    #[cfg(unix)]
    #[test]
    fn fixed_remote_script_expands_home_and_returns_a_physical_absolute_directory() {
        let temp = TempDir::new().unwrap();
        let home = temp.path().join("remote home");
        let nested = home.join("repo with ' and ; data");
        fs::create_dir_all(&nested).unwrap();
        std::os::unix::fs::symlink(&nested, home.join("repository-link")).unwrap();
        let canonical_home = fs::canonicalize(&home).unwrap();
        let canonical_nested = fs::canonicalize(&nested).unwrap();

        for (input, expected) in [
            ("~".to_string(), canonical_home.clone()),
            (
                "~/repo with ' and ; data".to_string(),
                canonical_nested.clone(),
            ),
            ("~/repository-link".to_string(), canonical_nested),
            (
                nested.join("..").to_string_lossy().into_owned(),
                canonical_home.clone(),
            ),
        ] {
            let output = Command::new("/bin/sh")
                .arg("-c")
                .arg(REMOTE_REPOSITORY_SCRIPT)
                .arg("taskai-resolve-repository")
                .arg(&input)
                .env("HOME", &home)
                .output()
                .unwrap();
            assert!(
                output.status.success(),
                "resolver failed for {input:?}: {}",
                String::from_utf8_lossy(&output.stderr)
            );
            assert_eq!(parse_remote_repository(&output.stdout).unwrap(), expected);
        }
    }

    #[test]
    fn local_runtime_can_be_inspected_resized_and_killed_exactly() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let server = namespace();
        let first = local_locator(&server, "s1");
        let second = local_locator(&server, "s2");
        let _first_guard = RuntimeGuard(first.clone());
        let _second_guard = RuntimeGuard(second.clone());
        let script = "printf '%s\\n' \"$$\" > pid; pwd > cwd; printf '%s\\n' \"$TASKAI_TEST_VALUE\" > env; printf '%s|%s\\n' \"$TERM\" \"$COLORTERM\" > terminal-env; while :; do sleep 1; done";

        create(
            &first,
            "/bin/sh",
            &["-c".into(), script.into()],
            temp.path(),
            &[(
                "TASKAI_TEST_VALUE".into(),
                "separate argv; not shell".into(),
            )],
            24,
            80,
        )
        .unwrap();
        create(
            &second,
            "/bin/sh",
            &["-c".into(), "while :; do sleep 1; done".into()],
            temp.path(),
            &[],
            25,
            81,
        )
        .unwrap();

        assert!(eventually(|| temp.path().join("pid").exists()));
        assert_eq!(
            fs::read_to_string(temp.path().join("cwd")).unwrap().trim(),
            temp.path().to_str().unwrap()
        );
        assert_eq!(
            fs::read_to_string(temp.path().join("env")).unwrap().trim(),
            "separate argv; not shell"
        );
        assert_eq!(
            fs::read_to_string(temp.path().join("terminal-env"))
                .unwrap()
                .trim(),
            "tmux-256color|truecolor"
        );
        let recorded_pid = fs::read_to_string(temp.path().join("pid"))
            .unwrap()
            .trim()
            .parse::<u32>()
            .unwrap();
        assert!(matches!(
            inspect(&first).unwrap(),
            RuntimeStatus::Live {
                pid,
                cols: 80,
                rows: 24
            } if pid == recorded_pid
        ));

        assert!(resize(&first, 33, 101).unwrap());
        assert!(eventually(|| matches!(
            inspect(&first),
            Ok(RuntimeStatus::Live {
                cols: 101,
                rows: 33,
                ..
            })
        )));

        assert!(kill(&first).unwrap());
        assert!(matches!(inspect(&first).unwrap(), RuntimeStatus::Missing));
        assert!(matches!(
            inspect(&second).unwrap(),
            RuntimeStatus::Live { .. }
        ));
        assert!(!kill(&first).unwrap());
    }

    #[test]
    fn native_operations_ignore_a_selected_bridge_window() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let locator = local_locator(&namespace(), "s42");
        let _guard = RuntimeGuard(locator.clone());
        create(
            &locator,
            "/bin/sh",
            &[
                "-c".into(),
                "printf 'NATIVE_MARKER\\n'; while :; do sleep 1; done".into(),
            ],
            temp.path(),
            &[],
            24,
            80,
        )
        .unwrap();
        let native_pid = match inspect(&locator).unwrap() {
            RuntimeStatus::Live { pid, .. } => pid,
            status => panic!("unexpected native status: {status:?}"),
        };
        test_private_command(
            &locator,
            &[
                "new-window",
                "-d",
                "-t",
                "=s42",
                "-n",
                "bridge",
                "--",
                "/bin/sh",
                "-c",
                "while :; do sleep 1; done",
            ],
        );
        test_private_command(&locator, &["select-window", "-t", "=s42:1"]);
        assert!(matches!(
            inspect(&locator).unwrap(),
            RuntimeStatus::Live { pid, .. } if pid == native_pid
        ));
        assert!(eventually(|| capture_test_terminal(&locator)
            .unwrap()
            .contains("NATIVE_MARKER")));
        assert_eq!(
            attach_command(&locator).unwrap().args.last().unwrap(),
            "=s42:0"
        );
        assert!(resize(&locator, 31, 99).unwrap());
        assert!(eventually(|| matches!(
            inspect(&locator),
            Ok(RuntimeStatus::Live { pid, cols: 99, rows: 31 }) if pid == native_pid
        )));
    }

    #[test]
    fn bridge_window_uses_exact_slot_and_scrubs_private_tmux_environments() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let locator = local_locator(&namespace(), "s42");
        let _guard = RuntimeGuard(locator.clone());
        create(
            &locator,
            "/bin/sh",
            &["-c".into(), "while :; do sleep 1; done".into()],
            temp.path(),
            &[],
            24,
            80,
        )
        .unwrap();
        test_private_command(
            &locator,
            &[
                "set-environment",
                "-g",
                "CLAUDE_CODE_MESSAGING_POISON",
                "synthetic",
            ],
        );
        test_private_command(
            &locator,
            &[
                "set-environment",
                "-t",
                "=s42",
                "CLAUDE_CODE_MESSAGING_OTHER",
                "synthetic",
            ],
        );
        test_private_command(
            &locator,
            &[
                "set-environment",
                "-t",
                "=s42",
                "TASKAI_TEST_UNRELATED",
                "kept",
            ],
        );
        let script = "if [ -n \"${CLAUDE_CODE_MESSAGING_POISON+x}\" ] || [ -n \"${CLAUDE_CODE_MESSAGING_OTHER+x}\" ]; then printf present > poison; else printf absent > poison; fi; printf '%s' \"$TASKAI_TEST_UNRELATED\" > unrelated; while :; do sleep 1; done";
        create_sidecar(
            &locator,
            "bridge",
            "/bin/sh",
            &["-c".into(), script.into()],
            temp.path(),
            &[],
        )
        .unwrap();
        assert!(eventually(|| temp.path().join("poison").exists()));
        assert_eq!(
            fs::read_to_string(temp.path().join("poison")).unwrap(),
            "absent"
        );
        assert_eq!(
            fs::read_to_string(temp.path().join("unrelated")).unwrap(),
            "kept"
        );
        assert!(matches!(
            inspect_sidecar(&locator, "bridge").unwrap(),
            RuntimeStatus::Live { .. }
        ));
        assert!(matches!(
            inspect(&locator).unwrap(),
            RuntimeStatus::Live { .. }
        ));
        assert_eq!(
            test_private_command(
                &locator,
                &["display-message", "-p", "-t", "=s42:0", "#{window_active}"]
            ),
            "1"
        );
        let identity = native_identity(&locator).unwrap().unwrap();
        assert_eq!(
            identity.pid,
            match inspect(&locator).unwrap() {
                RuntimeStatus::Live { pid, .. } => pid,
                _ => unreachable!(),
            }
        );
        assert!(identity.pane_id.starts_with('%'));
        assert!(taskai_core::mail::is_live(
            identity.pid,
            &identity.proc_start
        ));
        test_private_command(&locator, &["kill-pane", "-t", "=s42:0.0"]);
        assert!(eventually(|| matches!(
            inspect(&locator),
            Ok(RuntimeStatus::Missing)
        )));
        assert!(native_identity(&locator).unwrap().is_none());
        assert!(matches!(
            inspect_sidecar(&locator, "bridge").unwrap(),
            RuntimeStatus::Live { .. }
        ));
    }

    #[test]
    fn retained_private_tmux_scrubs_non_utf8_messaging_name() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let locator = local_locator(&namespace(), "s42");
        let _guard = RuntimeGuard(locator.clone());
        create(
            &locator,
            "/bin/sh",
            &["-c".into(), "while :; do sleep 1; done".into()],
            temp.path(),
            &[],
            24,
            80,
        )
        .unwrap();
        let raw_name = std::ffi::OsString::from_vec(b"CLAUDE_CODE_MESSAGING_\xff".to_vec());
        let output = Command::new("tmux")
            .args([
                "-f",
                "/dev/null",
                "-L",
                &locator.private_server,
                "set-environment",
                "-g",
            ])
            .arg(&raw_name)
            .arg("synthetic")
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "private tmux rejected raw environment name"
        );
        create_sidecar(
            &locator,
            "bridge",
            "/bin/sh",
            &["-c".into(), "if env | LC_ALL=C grep -q '^CLAUDE_CODE_MESSAGING_'; then printf present > raw; else printf absent > raw; fi; while :; do sleep 1; done".into()],
            temp.path(),
            &[],
        )
        .unwrap();
        assert!(eventually(|| temp.path().join("raw").exists()));
        assert_eq!(
            fs::read_to_string(temp.path().join("raw")).unwrap(),
            "absent"
        );
    }

    #[test]
    fn ambiguous_retained_name_fails_closed_before_sidecar_spawn() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let locator = local_locator(&namespace(), "s42");
        let _guard = RuntimeGuard(locator.clone());
        create(
            &locator,
            "/bin/sh",
            &["-c".into(), "while :; do sleep 1; done".into()],
            temp.path(),
            &[],
            24,
            80,
        )
        .unwrap();
        for raw_name in [
            b"CLAUDE_CODE_MESSAGING_\xff".as_slice(),
            b"CLAUDE_CODE_MESSAGING_\\377".as_slice(),
        ] {
            let name = std::ffi::OsString::from_vec(raw_name.to_vec());
            let output = Command::new("tmux")
                .args([
                    "-f",
                    "/dev/null",
                    "-L",
                    &locator.private_server,
                    "set-environment",
                    "-g",
                ])
                .arg(name)
                .arg("synthetic")
                .output()
                .unwrap();
            assert!(output.status.success(), "private fixture rejected a name");
        }
        let error = create_sidecar(
            &locator,
            "bridge",
            "/bin/sh",
            &[
                "-c".into(),
                "printf spawned > spawned; while :; do sleep 1; done".into(),
            ],
            temp.path(),
            &[],
        )
        .unwrap_err();
        assert!(error.to_string().contains("could not be safely scrubbed"));
        assert!(!temp.path().join("spawned").exists());
        assert!(matches!(
            inspect_sidecar(&locator, "bridge").unwrap(),
            RuntimeStatus::Missing
        ));
    }

    #[test]
    fn control_suffix_retained_name_fails_closed_before_sidecar_spawn() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let locator = local_locator(&namespace(), "s42");
        let _guard = RuntimeGuard(locator.clone());
        create(
            &locator,
            "/bin/sh",
            &["-c".into(), "while :; do sleep 1; done".into()],
            temp.path(),
            &[],
            24,
            80,
        )
        .unwrap();
        let name = std::ffi::OsString::from_vec(b"CLAUDE_CODE_MESSAGING_\nSUFFIX".to_vec());
        let output = Command::new("tmux")
            .args([
                "-f",
                "/dev/null",
                "-L",
                &locator.private_server,
                "set-environment",
                "-t",
                "=s42",
            ])
            .arg(name)
            .arg("synthetic")
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "private fixture rejected a control name"
        );
        let error = create_sidecar(
            &locator,
            "bridge",
            "/bin/sh",
            &[
                "-c".into(),
                "printf spawned > spawned; while :; do sleep 1; done".into(),
            ],
            temp.path(),
            &[],
        )
        .unwrap_err();
        assert!(error.to_string().contains("could not be safely scrubbed"));
        assert!(!temp.path().join("spawned").exists());
        assert!(matches!(
            inspect_sidecar(&locator, "bridge").unwrap(),
            RuntimeStatus::Missing
        ));
    }

    #[test]
    fn native_create_rejects_explicit_messaging_environment() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let locator = local_locator(&namespace(), "s42");
        let error = create(
            &locator,
            "/bin/sh",
            &["-c".into(), "while :; do sleep 1; done".into()],
            temp.path(),
            &[("CLAUDE_CODE_MESSAGING_POISON".into(), "synthetic".into())],
            24,
            80,
        )
        .unwrap_err();
        assert!(error.to_string().contains("Claude messaging identity"));
        assert!(matches!(inspect(&locator).unwrap(), RuntimeStatus::Missing));
    }

    #[test]
    fn provider_exit_removes_session_so_the_viewer_cannot_look_live() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let locator = local_locator(&namespace(), "s9");
        let _guard = RuntimeGuard(locator.clone());
        create(
            &locator,
            "/bin/sh",
            &[
                "-c".into(),
                "printf 'READY\\n'; IFS= read -r line; exit 17".into(),
            ],
            temp.path(),
            &[],
            24,
            80,
        )
        .unwrap();

        let attach = attach_command(&locator).unwrap();
        let arg_refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let mut viewer = Pane::spawn(&attach.program, &arg_refs, temp.path(), 24, 80, &[]).unwrap();
        assert!(eventually(|| viewer.text().contains("READY")));
        viewer.write_bytes(b"exit-now\r").unwrap();
        assert!(eventually(|| !viewer.is_alive()));
        assert!(eventually(|| matches!(
            inspect(&locator),
            Ok(RuntimeStatus::Missing)
        )));
        assert!(!kill(&locator).unwrap());
    }

    #[test]
    fn portable_pty_viewer_detaches_and_reattaches_to_same_terminal() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let locator = local_locator(&namespace(), "s7");
        let _guard = RuntimeGuard(locator.clone());
        create(
            &locator,
            "/bin/sh",
            &[
                "-c".into(),
                "printf 'READY\\n'; while IFS= read -r line; do printf 'GOT:%s\\n' \"$line\"; done"
                    .into(),
            ],
            temp.path(),
            &[],
            24,
            80,
        )
        .unwrap();
        let original_pid = match inspect(&locator).unwrap() {
            RuntimeStatus::Live { pid, .. } => pid,
            state => panic!("expected live runtime, got {state:?}"),
        };

        let attach = attach_command(&locator).unwrap();
        let arg_refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let mut first = Pane::spawn(&attach.program, &arg_refs, temp.path(), 24, 80, &[]).unwrap();
        assert!(eventually(|| first.text().contains("READY")));
        first.write_bytes(b"unsubmitted-draft").unwrap();
        assert!(eventually(|| first.text().contains("unsubmitted-draft")));
        drop(first);

        assert!(matches!(
            inspect(&locator).unwrap(),
            RuntimeStatus::Live { pid, .. } if pid == original_pid
        ));
        let attach = attach_command(&locator).unwrap();
        let arg_refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let second = Pane::spawn(&attach.program, &arg_refs, temp.path(), 24, 80, &[]).unwrap();
        assert!(eventually(|| second.text().contains("unsubmitted-draft")));
        drop(second);
        assert!(matches!(
            inspect(&locator).unwrap(),
            RuntimeStatus::Live { pid, .. } if pid == original_pid
        ));
    }

    #[test]
    fn configured_runtime_preserves_truecolor_cells() {
        if !tmux_available() {
            return;
        }
        let temp = TempDir::new().unwrap();
        let child =
            "printf '\\033[38;2;124;214;255m\\033[48;2;17;24;38mRGB\\033[0m READY\\n'; sleep 60";
        let direct = Pane::spawn("/bin/sh", &["-c", child], temp.path(), 24, 80, &[]).unwrap();
        assert!(eventually(|| direct.text().contains("READY")));

        let locator = local_locator(&namespace(), "s8");
        let _guard = RuntimeGuard(locator.clone());
        create(
            &locator,
            "/bin/sh",
            &["-c".into(), child.into()],
            temp.path(),
            &[],
            24,
            80,
        )
        .unwrap();
        let attach = attach_command(&locator).unwrap();
        let arg_refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let nested = Pane::spawn(&attach.program, &arg_refs, temp.path(), 24, 80, &[]).unwrap();
        assert!(eventually(|| nested.text().contains("READY")));

        let direct_cells = direct.cells();
        let nested_cells = nested.cells();
        for column in 0..3 {
            let direct = &direct_cells[0][column];
            let nested = &nested_cells[0][column];
            assert!(
                nested.text == direct.text
                    && nested.fg == direct.fg
                    && nested.bg == direct.bg
                    && nested.bold == direct.bold
                    && nested.width == direct.width,
                "RGB cell {column} changed through the TaskAI tmux runtime: direct=({:?}, {:?}, {:?}, {}, {}), nested=({:?}, {:?}, {:?}, {}, {})",
                direct.text,
                direct.fg,
                direct.bg,
                direct.bold,
                direct.width,
                nested.text,
                nested.fg,
                nested.bg,
                nested.bold,
                nested.width,
            );
        }
    }

    #[test]
    fn attach_command_unsets_outer_tmux_and_targets_exact_session() {
        let local = local_locator("taskai-safe", "s42");
        let command = attach_command(&local).unwrap();
        assert_eq!(command.program, "/usr/bin/env");
        assert_eq!(
            &command.args[..12],
            vec![
                "-u",
                "TMUX",
                "-u",
                "TMUX_PANE",
                "-u",
                "TMUX_TMPDIR",
                "tmux",
                "-N",
                "-f",
                "/dev/null",
                "-L",
                "taskai-safe",
            ]
        );
        let settings = private_mouse_commands(&local);
        assert_eq!(&command.args[12..12 + settings.len()], settings);
        assert_eq!(
            &command.args[12 + settings.len()..],
            [
                ";",
                "attach-session",
                "-E",
                "-f",
                "ignore-size",
                "-t",
                "=s42:0"
            ]
        );
        assert_eq!(&settings[..5], ["set-option", "-t", "=s42:", "mouse", "on"]);
        for table in ["copy-mode", "copy-mode-vi"] {
            for key in ["Escape", "End"] {
                assert!(settings.windows(7).any(
                    |args| args == ["bind-key", "-T", table, key, "send-keys", "-X", "cancel"]
                ));
            }
        }
    }

    fn test_private_command(locator: &TmuxRuntimeLocator, args: &[&str]) -> String {
        let mut command = tmux_prefix(locator);
        command.extend(args.iter().map(|arg| (*arg).into()));
        let output = target_output(&locator.target, &command);
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8(output.stdout).unwrap().trim().into()
    }

    fn mouse_fixture_diagnostics(locator: &TmuxRuntimeLocator) -> String {
        let mut command = tmux_prefix(locator);
        command.extend([
            "list-panes".into(), "-t".into(), exact_session(&locator.session), "-F".into(),
            "PANE pid=#{pane_pid} tty=#{pane_tty} in_mode=#{pane_in_mode} scroll=#{scroll_position} history=#{history_size} size=#{pane_width}x#{pane_height} mouse_any=#{mouse_any_flag} mouse_sgr=#{mouse_sgr_flag}".into(),
            ";".into(), "list-clients".into(), "-t".into(), exact_session(&locator.session), "-F".into(),
            "CLIENT flags=#{client_flags} size=#{client_width}x#{client_height} written=#{client_written} discarded=#{client_discarded}".into(),
            ";".into(), "capture-pane".into(), "-p".into(), "-t".into(), format!("{}:", exact_session(&locator.session)),
        ]);
        match target_invocation(&locator.target, &command)
            .and_then(|invocation| run(&invocation, "mouse fixture diagnostics"))
        {
            Ok(output) => format!(
                "status={:?}\n{}\n{}",
                output.status.code(),
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            ),
            Err(error) => format!("native diagnostics unavailable: {error}"),
        }
    }

    fn mouse_history_lifecycle(target: ConnectionTarget) {
        probe(&target).unwrap();
        let temp = TempDir::new().unwrap();
        let locator = TmuxRuntimeLocator {
            target: target.clone(),
            private_server: namespace(),
            session: "s1".into(),
        };
        let _guard = RuntimeGuard(locator.clone());
        let outer_before = default_tmux_snapshot(&target);
        create(
            &locator,
            "/bin/sh",
            &["-c".into(), "i=0; while [ \"$i\" -lt 120 ]; do printf 'HISTORY-%03d\\n' \"$i\"; i=$((i+1)); done; printf 'SCROLL_LIVE_READY\\n'; exec cat".into()],
            Path::new("/tmp"),
            &[],
            24,
            80,
        ).unwrap();
        let original = inspect(&locator).unwrap();
        assert!(matches!(original, RuntimeStatus::Live { .. }));
        assert_eq!(
            test_private_command(&locator, &["show-options", "-v", "-t", "=s1:", "mouse"]),
            "on"
        );
        // Emulate a saved pre-upgrade session. Attaching must repair its private
        // mouse setting without respawning the existing process.
        test_private_command(&locator, &["set-option", "-t", "=s1:", "mouse", "off"]);
        let attach = attach_command(&locator).unwrap();
        let args: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let mut viewer = Pane::spawn(&attach.program, &args, temp.path(), 24, 80, &[]).unwrap();
        // A visible frame can precede tmux's mouse-protocol negotiation. This
        // fixture writes literal SGR packets, unlike production's state-aware
        // encoder, so wait for that exact protocol before sending any wheels.
        assert!(
            eventually_within(Duration::from_secs(20), || {
                let mouse = viewer.mouse_state();
                mouse.mode != MouseProtocolMode::None
                    && mouse.encoding == MouseProtocolEncoding::Sgr
                    && viewer.text().contains("SCROLL_LIVE_READY")
            }),
            "mouse={:?}; {}",
            viewer.mouse_state(),
            viewer.text()
        );
        assert_eq!(
            test_private_command(&locator, &["show-options", "-v", "-t", "=s1:", "mouse"]),
            "on"
        );
        // This travels through the real attach PTY, not tmux send-keys or an
        // artificial renderer buffer. The default no-mouse-app path enters history.
        for _ in 0..8 {
            viewer.write_bytes(b"\x1b[<64;5;5M").unwrap();
        }
        assert!(
            eventually(|| !viewer.text().contains("SCROLL_LIVE_READY")),
            "viewer mouse={:?}; viewer:\n{}\nnative:\n{}",
            viewer.mouse_state(),
            viewer.text(),
            mouse_fixture_diagnostics(&locator)
        );
        assert_eq!(
            test_private_command(
                &locator,
                &["display-message", "-p", "-t", "=s1:", "#{pane_in_mode}"]
            ),
            "1"
        );
        assert!(viewer.text().contains("HISTORY-"));
        assert!(
            viewer
                .text()
                .lines()
                .next()
                .unwrap_or_default()
                .contains('/'),
            "copy-mode position missing: {}",
            viewer.text()
        );
        viewer.write_bytes(b"\x1b[F").unwrap();
        assert!(
            eventually(|| viewer.text().contains("SCROLL_LIVE_READY")),
            "{}",
            viewer.text()
        );
        assert_eq!(
            test_private_command(
                &locator,
                &["display-message", "-p", "-t", "=s1:", "#{pane_in_mode}"]
            ),
            "0"
        );
        test_private_command(
            &locator,
            &["set-option", "-w", "-t", "=s1:", "mode-keys", "vi"],
        );
        for _ in 0..8 {
            viewer.write_bytes(b"\x1b[<64;5;5M").unwrap();
        }
        assert!(
            eventually(|| !viewer.text().contains("SCROLL_LIVE_READY")),
            "{}",
            viewer.text()
        );
        viewer.write_bytes(b"\x1b").unwrap();
        assert!(
            eventually(|| viewer.text().contains("SCROLL_LIVE_READY")),
            "vi Escape did not return live: {}",
            viewer.text()
        );
        drop(viewer);
        assert_eq!(inspect(&locator).unwrap(), original);

        // A mouse-aware native application owns its wheel events; tmux must
        // not replace those with history navigation.
        let mouse = TmuxRuntimeLocator {
            session: "s2".into(),
            ..locator.clone()
        };
        let _mouse_guard = RuntimeGuard(mouse.clone());
        create(
            &mouse,
            "/bin/sh",
            &["-c".into(), "stty raw -echo; printf '\\033[?1000h\\033[?1006hMOUSE_READY\\n'; dd bs=1 count=10 2>/dev/null | od -An -tx1; printf '\\033[?1000l\\033[?1006lMOUSE_DONE\\n'; exec cat".into()],
            Path::new("/tmp"), &[], 24, 80,
        ).unwrap();
        let attach = attach_command(&mouse).unwrap();
        let args: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let mut viewer = Pane::spawn(&attach.program, &args, temp.path(), 24, 80, &[]).unwrap();
        assert!(
            eventually_within(Duration::from_secs(20), || {
                let mouse = viewer.mouse_state();
                mouse.mode != MouseProtocolMode::None
                    && mouse.encoding == MouseProtocolEncoding::Sgr
                    && viewer.text().contains("MOUSE_READY")
            }),
            "mouse={:?}; {}",
            viewer.mouse_state(),
            viewer.text()
        );
        viewer.write_bytes(b"\x1b[<64;5;5M").unwrap();
        assert!(
            eventually(|| viewer.text().contains("MOUSE_DONE")),
            "{}",
            viewer.text()
        );
        let words = viewer
            .text()
            .split_whitespace()
            .collect::<Vec<_>>()
            .join(" ");
        assert!(
            words.contains("1b 5b 3c 36 34 3b 35 3b 35 4d"),
            "wheel packet changed: {}",
            viewer.text()
        );
        assert_eq!(
            test_private_command(
                &mouse,
                &["display-message", "-p", "-t", "=s2:", "#{pane_in_mode}"]
            ),
            "0"
        );
        drop(viewer);
        assert_eq!(default_tmux_snapshot(&target), outer_before);
    }

    #[test]
    fn private_mouse_routes_native_input_and_history_without_restarting_runtime() {
        if tmux_available() {
            mouse_history_lifecycle(ConnectionTarget::Local);
        }
    }

    #[test]
    fn configured_ssh_mouse_routes_native_input_and_history() {
        if let Ok(host) = env::var("TASKAI_REMOTE_TEST_HOST") {
            mouse_history_lifecycle(ConnectionTarget::ssh(host).unwrap());
        }
    }

    #[test]
    fn configured_ssh_provider_versions_use_the_users_login_environment() {
        let Ok(host) = env::var("TASKAI_REMOTE_TEST_HOST") else {
            return;
        };
        let target = ConnectionTarget::ssh(host).unwrap();
        for program in ["claude", "codex", "grok"] {
            let version = probe_program(&target, program).unwrap();
            eprintln!("{program}: {version}");
            assert!(!version.contains("gitstatus"));
            assert!(!version.contains("can't change option"));
            assert!(version.chars().any(|character| character.is_ascii_digit()));
        }
    }

    #[test]
    fn configured_ssh_provider_launch_refreshes_stale_path_and_preserves_process() {
        let Ok(host) = env::var("TASKAI_REMOTE_TEST_HOST") else {
            return;
        };
        let target = ConnectionTarget::ssh(host).unwrap();
        let server = namespace().replace("taskai-test-", "taskai-remote-test-");
        let remote_root = PathBuf::from(format!("/tmp/{server}"));
        let _remote_guard = RemoteDirGuard {
            target: target.clone(),
            path: remote_root.clone(),
        };
        let seed = TmuxRuntimeLocator {
            target: target.clone(),
            private_server: server.clone(),
            session: "s0".into(),
        };
        let locator = TmuxRuntimeLocator {
            session: "s1".into(),
            ..seed.clone()
        };
        let _seed_guard = RuntimeGuard(seed.clone());
        let _runtime_guard = RuntimeGuard(locator.clone());
        let fake = r#"#!/bin/sh
if [ "${1-}" = --version ]; then printf '2.1.0 (Fixture CLI)\n'; exit; fi
printf '%s\n' "$$" > pid
printf '%s\n' "$PWD" > cwd
printf '<%s>\n' "$@" > argv
printf '%s\n' "$TASKAI_SESSION_REF" > context
printf x >> launches
printf 'LOGIN-PATH-READY\n'
while IFS= read -r line; do printf 'GOT:%s\n' "$line"; done
"#;
        let setup = target_output(&target, &[
            "/bin/sh".into(), "-c".into(),
            "umask 077; mkdir -p -- \"$1/provider bin\"; printf '%s' \"$2\" > \"$1/provider bin/provider with spaces\"; printf '%s' \"$3\" > \"$1/login shell\"; chmod 700 \"$1/provider bin/provider with spaces\" \"$1/login shell\"".into(),
            "taskai-provider-fixture".into(), remote_root.display().to_string(), fake.into(), PROVIDER_TEST_SHELL.into(),
        ]);
        assert!(setup.status.success(), "{setup:?}");
        assert_eq!(
            probe_program(
                &target,
                remote_root
                    .join("provider bin/provider with spaces")
                    .to_str()
                    .unwrap()
            )
            .unwrap(),
            "2.1.0 (Fixture CLI)"
        );

        // Keep a private server alive before installing the per-launch PATH.
        // Neither its original environment nor later attach clients knows this bin.
        create(
            &seed,
            "/bin/sh",
            &["-c".into(), "exec sleep 180".into()],
            &remote_root,
            &[],
            24,
            80,
        )
        .unwrap();
        let command = provider_launch_command(
            &target,
            "provider with spaces",
            &[
                "--resume".into(),
                "exact id ; $(printf unexpected) ' \"".into(),
                String::new(),
            ],
        )
        .unwrap();
        let mut args = vec![
            format!("SHELL={}/login shell", remote_root.display()),
            format!(
                "TASKAI_TEST_PROVIDER_BIN={}/provider bin",
                remote_root.display()
            ),
            command.program,
        ];
        args.extend(command.args);
        create(
            &locator,
            "/usr/bin/env",
            &args,
            &remote_root,
            &[
                ("PATH".into(), "/usr/bin:/bin".into()),
                ("TASKAI_SESSION_REF".into(), "s1".into()),
            ],
            24,
            80,
        )
        .unwrap();
        let local_dir = TempDir::new().unwrap();
        let attach = attach_command(&locator).unwrap();
        let refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let mut first = Pane::spawn(&attach.program, &refs, local_dir.path(), 24, 80, &[]).unwrap();
        assert!(
            eventually_within(Duration::from_secs(15), || first
                .text()
                .contains("LOGIN-PATH-READY")),
            "{}",
            first.text()
        );
        assert!(!first.text().contains("profile banner"));
        assert!(!first.text().contains("profile warning"));
        let pid = remote_file(&target, &remote_root.join("pid"))
            .unwrap()
            .trim()
            .parse::<u32>()
            .unwrap();
        assert!(
            matches!(inspect(&locator).unwrap(), RuntimeStatus::Live { pid: observed, .. } if observed == pid)
        );
        assert_eq!(
            remote_file(&target, &remote_root.join("argv")).unwrap(),
            "<--resume>\n<exact id ; $(printf unexpected) ' \">\n<>\n"
        );
        assert_eq!(
            remote_file(&target, &remote_root.join("cwd"))
                .unwrap()
                .trim(),
            remote_root.to_str().unwrap()
        );
        assert_eq!(
            remote_file(&target, &remote_root.join("context")).unwrap(),
            "s1\n"
        );
        first.write_bytes(b"unsent-login-path-draft").unwrap();
        assert!(eventually(|| first
            .text()
            .contains("unsent-login-path-draft")));
        drop(first);
        let second = Pane::spawn(&attach.program, &refs, local_dir.path(), 24, 80, &[]).unwrap();
        assert!(
            eventually_within(Duration::from_secs(15), || second
                .text()
                .contains("unsent-login-path-draft")),
            "{}",
            second.text()
        );
        assert!(
            matches!(inspect(&locator).unwrap(), RuntimeStatus::Live { pid: observed, .. } if observed == pid)
        );
        assert_eq!(
            remote_file(&target, &remote_root.join("launches")).unwrap(),
            "x"
        );
        drop(second);
        assert!(kill(&locator).unwrap());
        assert_eq!(inspect(&locator).unwrap(), RuntimeStatus::Missing);
        assert!(matches!(
            inspect(&seed).unwrap(),
            RuntimeStatus::Live { .. }
        ));
        assert!(kill(&seed).unwrap());
    }

    // Expectation: when TASKAI_REMOTE_TEST_HOST is explicitly set, the same durable
    // terminal lifecycle and terminal fidelity work through SSH without invoking a model
    // provider. Why: a syntactically correct SSH command is not proof that detach/reopen,
    // rendering, input, resizing, viewer cleanup, and destruction work on the real target.
    // Value: this is the non-billing AT-10/AT-13/AT-14 acceptance proof.
    #[test]
    fn configured_ssh_runtime_survives_detach_and_is_destroyed_exactly() {
        let Ok(host) = env::var("TASKAI_REMOTE_TEST_HOST") else {
            eprintln!("skipped: set TASKAI_REMOTE_TEST_HOST for the SSH lifecycle proof");
            return;
        };
        let target = ConnectionTarget::ssh(host).unwrap();
        probe(&target).expect("remote tmux prerequisite");
        let server = namespace().replace("taskai-test-", "taskai-remote-test-");
        let remote_root = PathBuf::from(format!("/tmp/{server}"));
        let locator = TmuxRuntimeLocator {
            target: target.clone(),
            private_server: server,
            session: "s1".into(),
        };
        let _runtime_guard = RuntimeGuard(locator.clone());
        let _remote_guard = RemoteDirGuard {
            target: target.clone(),
            path: remote_root.clone(),
        };
        let default_before = default_tmux_snapshot(&target);
        let terminal_fixture = "\
            printf 'PRIMARY-SCREEN-MARKER';\
            printf '\\033[?1049h\\033[2J\\033[H';\
            printf '\\033[38;2;124;214;255m\\033[48;2;17;24;38mION\\033[0m';\
            printf '\\033[1mBOLD\\033[0m';\
            printf '\\033[38;2;217;131;107m▌\\033[0m界 ';\
            printf 'FAKE CLI — NO MODEL CALLS\\n';\
            printf '\\033[5;10H\\033[?2004h\\033[?25h';";
        let local_viewer_dir = TempDir::new().unwrap();
        let direct_script = [terminal_fixture, "exec cat\n"].concat();
        let direct = Pane::spawn(
            "/bin/sh",
            &["-c", &direct_script],
            local_viewer_dir.path(),
            24,
            80,
            &[],
        )
        .unwrap();
        assert!(eventually_within(Duration::from_secs(5), || {
            let snapshot = direct.snapshot();
            direct.text().contains("FAKE CLI — NO MODEL CALLS")
                && snapshot.cursor.row == 4
                && snapshot.cursor.col == 9
                && snapshot.cursor.visible
                && snapshot.bracketed_paste
        }));
        let direct_snapshot = direct.snapshot();
        assert!(!direct.text().contains("PRIMARY-SCREEN-MARKER"));
        drop(direct);

        let fake = [
            "#!/bin/sh\n\
             if [ \"${1-}\" = \"--version\" ]; then printf '2.1.0 (Fake CLI)\\n'; exit 0; fi\n\
             printf '%s\\n' \"$$\" > pid\n\
             pwd > cwd\n\
             printf x >> launches\n\
             (while :; do printf x >> heartbeat; sleep 0.05; done) &\n\
             beat=$!\n\
             trap 'kill \"$beat\" 2>/dev/null || true' EXIT HUP INT TERM\n",
            terminal_fixture,
            "exec tee terminal-input\n",
        ]
        .concat();
        let setup = target_output(
            &target,
            &[
                "sh".into(),
                "-c".into(),
                "umask 077; mkdir -p -- \"$1\"; printf '%s' \"$2\" > \"$1/fake-cli\"; chmod 700 \"$1/fake-cli\"; printf 'preserve me\\n' > \"$1/repository-sentinel\"".into(),
                "taskai-setup".into(),
                remote_root.to_string_lossy().into_owned(),
                fake,
            ],
        );
        assert!(
            setup.status.success(),
            "remote setup failed: {}",
            String::from_utf8_lossy(&setup.stderr)
        );
        assert!(resolve_repository(&target, &remote_root).unwrap().is_some());

        let program = remote_root.join("fake-cli");
        assert_eq!(
            probe_program(&target, program.to_str().unwrap()).unwrap(),
            "2.1.0 (Fake CLI)"
        );
        create(
            &locator,
            program.to_str().unwrap(),
            &[],
            &remote_root,
            &[("TASKAI_TEST_VALUE".into(), "remote".into())],
            24,
            80,
        )
        .unwrap();
        assert!(eventually(|| remote_file(
            &target,
            &remote_root.join("pid")
        )
        .is_some()));
        assert_eq!(
            remote_file(&target, &remote_root.join("cwd"))
                .unwrap()
                .trim(),
            remote_root.to_str().unwrap()
        );
        let original_pid = remote_file(&target, &remote_root.join("pid"))
            .unwrap()
            .trim()
            .parse::<u32>()
            .unwrap();
        assert!(matches!(
            inspect(&locator).unwrap(),
            RuntimeStatus::Live { pid, .. } if pid == original_pid
        ));

        let attach = attach_command(&locator).unwrap();
        let arg_refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let mut first = Pane::spawn(
            &attach.program,
            &arg_refs,
            local_viewer_dir.path(),
            24,
            80,
            &[],
        )
        .unwrap();
        assert!(eventually_within(Duration::from_secs(15), || {
            let snapshot = first.snapshot();
            first.text().contains("FAKE CLI — NO MODEL CALLS")
                && snapshot.cursor.row == 4
                && snapshot.cursor.col == 9
                && snapshot.cursor.visible
                && snapshot.bracketed_paste
        }));
        let remote_snapshot = first.snapshot();
        assert_cell_grids_equal(
            &direct_snapshot.cells,
            &remote_snapshot.cells,
            "direct versus SSH/tmux",
        );
        assert_eq!(remote_snapshot.cursor, direct_snapshot.cursor);
        assert!(remote_snapshot.bracketed_paste);
        assert!(!first.text().contains("PRIMARY-SCREEN-MARKER"));
        for cell in remote_snapshot.cells[0].iter().take(3) {
            assert_eq!(cell.fg, Some((124, 214, 255)));
            assert_eq!(cell.bg, Some((17, 24, 38)));
        }
        let wide = remote_snapshot
            .cells
            .iter()
            .flatten()
            .find(|cell| cell.text == "界")
            .expect("SSH/tmux rendering retained the wide glyph");
        assert_eq!(wide.width, 2);
        assert!(eventually_within(Duration::from_secs(10), || {
            attached_client_count(&locator) == Some(1)
        }));
        first.write_bytes(b"remote-draft-survives").unwrap();
        assert!(eventually_within(Duration::from_secs(5), || first
            .text()
            .contains("remote-draft-survives")));
        drop(first);
        assert!(eventually_within(Duration::from_secs(10), || {
            attached_client_count(&locator) == Some(0)
        }));

        let heartbeat_before = remote_file(&target, &remote_root.join("heartbeat"))
            .unwrap_or_default()
            .len();
        assert!(eventually_within(Duration::from_secs(10), || {
            remote_file(&target, &remote_root.join("heartbeat"))
                .is_some_and(|value| value.len() > heartbeat_before)
        }));
        assert!(matches!(
            inspect(&locator).unwrap(),
            RuntimeStatus::Live { pid, .. } if pid == original_pid
        ));

        let attach = attach_command(&locator).unwrap();
        let arg_refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let mut second = Pane::spawn(
            &attach.program,
            &arg_refs,
            local_viewer_dir.path(),
            24,
            80,
            &[],
        )
        .unwrap();
        assert!(eventually_within(Duration::from_secs(15), || second
            .text()
            .contains("remote-draft-survives")));
        assert!(eventually_within(Duration::from_secs(10), || {
            attached_client_count(&locator) == Some(1)
        }));
        assert!(matches!(
            inspect(&locator).unwrap(),
            RuntimeStatus::Live { pid, .. } if pid == original_pid
        ));

        second
            .write_bytes(b"\x15\x1b[200~remote-paste-probe\x1b[201~\r")
            .unwrap();
        assert!(eventually_within(Duration::from_secs(10), || {
            remote_file(&target, &remote_root.join("terminal-input")).as_deref()
                == Some("\x1b[200~remote-paste-probe\x1b[201~\n")
        }));

        assert!(resize(&locator, 32, 100).unwrap());
        second.resize(32, 100).unwrap();
        assert!(eventually_within(Duration::from_secs(10), || matches!(
            inspect(&locator),
            Ok(RuntimeStatus::Live {
                pid,
                cols: 100,
                rows: 32
            }) if pid == original_pid
        )));
        assert_eq!(second.size(), (32, 100));
        drop(second);
        assert!(eventually_within(Duration::from_secs(10), || {
            attached_client_count(&locator) == Some(0)
        }));

        let attach = attach_command(&locator).unwrap();
        let arg_refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let third = Pane::spawn(
            &attach.program,
            &arg_refs,
            local_viewer_dir.path(),
            32,
            100,
            &[],
        )
        .unwrap();
        assert!(eventually_within(Duration::from_secs(15), || third
            .text()
            .contains("remote-paste-probe")));
        assert!(eventually_within(Duration::from_secs(10), || {
            attached_client_count(&locator) == Some(1)
        }));
        assert!(matches!(
            inspect(&locator).unwrap(),
            RuntimeStatus::Live { pid, .. } if pid == original_pid
        ));
        drop(third);
        assert!(eventually_within(Duration::from_secs(10), || {
            attached_client_count(&locator) == Some(0)
        }));
        assert_eq!(
            remote_file(&target, &remote_root.join("launches"))
                .unwrap()
                .len(),
            1
        );

        assert!(kill(&locator).unwrap());
        assert!(matches!(inspect(&locator).unwrap(), RuntimeStatus::Missing));
        assert_eq!(
            remote_file(&target, &remote_root.join("repository-sentinel")).unwrap(),
            "preserve me\n"
        );
        assert_eq!(default_tmux_snapshot(&target), default_before);
    }
}
