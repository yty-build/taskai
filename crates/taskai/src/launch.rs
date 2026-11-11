// ABOUTME: Blocking launch preparation for a disposable background worker.
// ABOUTME: It owns no registry state; locators must be persisted before calling it.

use std::{
    io::Read,
    path::{Path, PathBuf},
    time::Duration,
};

use taskai_connectors::{
    claude::{install_context_hook, install_hooks, session_events_path, ClaudeConnector},
    codex::CodexConnector,
    grok::{self, GrokConnector, PermissionMode as GrokPermissionMode},
};
use taskai_core::registry::{
    ConnectionTarget, LaunchSettings, ProviderMapping, Registry, ServerProfile, Session,
    TmuxRuntimeLocator,
};
use taskai_pane::Pane;

use crate::{
    bridge::codex_intent::{CodexIntents, SavedCreationPlan},
    doctor::{self, Paths},
    tmux::{self, RuntimeStatus},
};

pub struct Request {
    /// None opens the task's utility shell, never a provider or agent record.
    pub session: Option<Session>,
    pub runtime: TmuxRuntimeLocator,
    pub home_dir: PathBuf,
    pub repo: PathBuf,
    pub rows: u16,
    pub cols: u16,
}

pub struct Outcome {
    pub pane: Pane,
    pub runtime: TmuxRuntimeLocator,
    pub created: bool,
    /// The UI starts its existing session-binding watcher at this byte offset.
    pub claude_events: Option<(PathBuf, u64)>,
    /// A passive adapter may fail without preventing the native CLI from opening.
    pub messaging_warning: Option<String>,
}

/// Save the exact native Grok session before any process can be spawned. Reopening always
/// reuses this ID; a stale or foreign mapping is an error, never a replacement session.
pub(crate) fn ensure_grok_mapping(
    registry: &mut Registry,
    session_ref: &str,
) -> Result<ProviderMapping, String> {
    registry.reload().map_err(|error| error.to_string())?;
    let session = registry
        .session(session_ref)
        .ok_or("Grok session is not in the registry")?;
    if session.harness != "grok" {
        return Err("provider binding requires a Grok session".into());
    }
    let runtime = session
        .runtime
        .as_ref()
        .ok_or("Grok has no saved runtime")?;
    if runtime.target != ConnectionTarget::Local {
        return Err("provider binding requires a local Grok runtime".into());
    }
    let runtime_id = format!("tmux:{}:{}", runtime.private_server, runtime.session);
    let chosen = registry
        .bind_provider_if_absent(
            session_ref,
            ProviderMapping {
                provider: "grok".into(),
                provider_session_id: fresh_grok_session_id()?,
                runtime_id,
                // This records the adapter's prelaunch binding, not a probed CLI version.
                connector_version: "taskai-grok-m6/prelaunch-unverified".into(),
            },
        )
        .map_err(|error| error.to_string())?;
    if !is_grok_session_id(&chosen.provider_session_id) {
        return Err("Grok saved session ID is not a UUID".into());
    }
    Ok(chosen)
}

fn fresh_grok_session_id() -> Result<String, String> {
    let mut bytes = [0u8; 16];
    std::fs::File::open("/dev/urandom")
        .and_then(|mut file| file.read_exact(&mut bytes))
        .map_err(|error| format!("Could not allocate Grok session ID: {error}"))?;
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    Ok(format!(
        "{:02x}{:02x}{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}{:02x}{:02x}{:02x}{:02x}",
        bytes[0], bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7],
        bytes[8], bytes[9], bytes[10], bytes[11], bytes[12], bytes[13], bytes[14], bytes[15]
    ))
}

pub(crate) fn is_grok_session_id(id: &str) -> bool {
    id.len() == 36
        && id.bytes().enumerate().all(|(index, byte)| {
            if matches!(index, 8 | 13 | 18 | 23) {
                byte == b'-'
            } else {
                byte.is_ascii_hexdigit()
            }
        })
}

/// Give a session what its first launch needs and describe that launch: a tmux mapping on
/// `private_server` when none is saved and, for local Grok, its saved native session ID.
pub(crate) fn session_request(
    registry: &mut Registry,
    home_dir: &Path,
    private_server: &str,
    session_ref: &str,
    rows: u16,
    cols: u16,
) -> Result<Request, String> {
    let session = registry
        .session(session_ref)
        .cloned()
        .ok_or("unknown session")?;
    if session.runtime.is_none() {
        registry
            .bind_runtime(session_ref, private_server)
            .map_err(|error| error.to_string())?;
    }
    let grok_mapping = if session.harness == "grok"
        && registry
            .session(session_ref)
            .and_then(|saved| saved.runtime.as_ref())
            .is_some_and(|runtime| runtime.target == ConnectionTarget::Local)
    {
        Some(ensure_grok_mapping(registry, session_ref)?)
    } else {
        None
    };
    let mut session = registry
        .session(session_ref)
        .cloned()
        .ok_or("session missing")?;
    if let Some(mapping) = grok_mapping {
        session.provider = Some(mapping);
    }
    let runtime = session.runtime.clone().ok_or("runtime mapping missing")?;
    let repo = session.workspace.clone();
    Ok(Request {
        session: Some(session),
        runtime,
        home_dir: home_dir.to_path_buf(),
        repo,
        rows,
        cols,
    })
}

/// Window size of a runtime started with no viewer; the first viewer to attach resizes it.
const DETACHED_ROWS: u16 = 40;
const DETACHED_COLS: u16 = 160;

/// Start one session's durable runtime and messaging sidecar and leave no viewer attached, or
/// put back a missing sidecar beside a runtime that is already live. Nothing is typed into the
/// native pane. An agent inside a managed session may not start its teammates this way.
pub fn run_detached(
    home_dir: &Path,
    private_server: &str,
    session_ref: &str,
) -> Result<serde_json::Value, String> {
    if std::env::var_os("TASKAI_SESSION_REF").is_some() {
        return Err(
            "session launch is for the person's shell; it is refused inside a TaskAI session"
                .into(),
        );
    }
    let mut registry =
        Registry::open(&home_dir.join("state")).map_err(|error| error.to_string())?;
    let mut request = session_request(
        &mut registry,
        home_dir,
        private_server,
        session_ref,
        DETACHED_ROWS,
        DETACHED_COLS,
    )?;
    // A runtime that is already live keeps the size its last viewer gave it.
    if let RuntimeStatus::Live { rows, cols, .. } =
        tmux::inspect(&request.runtime).map_err(|error| error.to_string())?
    {
        (request.rows, request.cols) = (rows, cols);
    }
    let outcome = run(request)?;
    // Dropping the outcome ends only its disposable viewer; the tmux runtime stays.
    Ok(serde_json::json!({
        "session": session_ref,
        "created": outcome.created,
        "messaging_warning": outcome.messaging_warning,
    }))
}

type Recipe = (String, Vec<String>, Vec<(String, String)>);

const SETTINGS_EXEC_SCRIPT: &str = r#"for taskai_name in $(/usr/bin/env | /usr/bin/sed -n 's/^\(TASKAI_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$taskai_name"; done
exec /usr/bin/env -- "$@""#;

/// Inspect once, optionally create, then attach a disposable viewer. A failed
/// attachment never destroys the durable process or discards its saved mapping.
pub fn run(request: Request) -> Result<Outcome, String> {
    let Request {
        session,
        runtime,
        home_dir,
        repo,
        rows,
        cols,
    } = request;
    if let Some(session) = &session {
        if session.session_ref != runtime.session {
            return Err("launch session does not match its saved tmux mapping".into());
        }
    } else if !runtime.session.ends_with("-terminal") {
        return Err("utility launch requires a task-terminal mapping".into());
    }
    let local = runtime.target == ConnectionTarget::Local;
    let status = tmux::inspect(&runtime).map_err(|error| error.to_string())?;
    let created = !matches!(status, RuntimeStatus::Live { .. });
    let mut claude_events = None;
    let mut messaging_warning = None;
    let mut codex_plan = None;
    let run_dir = home_dir.join("run");
    if created {
        // A tmux -c path that no longer exists can fall back to the home
        // directory. Validate before starting any provider or utility shell.
        let resolved = tmux::resolve_repository(&runtime.target, &repo)
            .map_err(|error| error.to_string())?
            .ok_or_else(|| {
                format!(
                    "Repository {} does not exist on {}",
                    repo.display(),
                    runtime.target
                )
            })?;
        let recipe = session
            .as_ref()
            .map(|session| recipe(session, &runtime, &home_dir))
            .transpose()?;
        if let Some(session) = session
            .as_ref()
            .filter(|session| local && session.harness == "codex" && session.provider.is_some())
        {
            let id = &session.provider.as_ref().unwrap().provider_session_id;
            let plan = CodexIntents::saved_creation_plan(&home_dir, &session.session_ref, id)?;
            if plan == SavedCreationPlan::FirstHumanGate {
                CodexIntents::stage_gate_adoption(&home_dir, &session.session_ref, id)?;
            }
            codex_plan = Some(plan);
        }
        if let Some(session) = &session {
            if local {
                if session.harness == "claude" {
                    install_hooks(&resolved, &run_dir).map_err(|error| error.to_string())?;
                    let events = session_events_path(&run_dir, &session.session_ref);
                    let offset = std::fs::metadata(&events)
                        .map(|metadata| metadata.len())
                        .unwrap_or(0);
                    claude_events = Some((events, offset));
                    if let Ok(exe) = std::env::current_exe() {
                        let _ = install_context_hook(&resolved, &exe);
                    }
                }
                if session.harness == "grok" {
                    let exe = std::env::current_exe().map_err(|error| error.to_string())?;
                    grok::install_mcp(&resolved, &exe).map_err(|error| error.to_string())?;
                    grok::install_mode_hook(&resolved, &exe).map_err(|error| error.to_string())?;
                    grok::ensure_leader_socket_dir(&grok::leader_socket_path(
                        &run_dir,
                        &session.session_ref,
                    ))
                    .map_err(|error| error.to_string())?;
                }
                let _ = taskai_context::install_bootstrap(&resolved);
            }
        }
        if matches!(status, RuntimeStatus::Dead { .. }) {
            tmux::kill(&runtime).map_err(|error| error.to_string())?;
        }
        if let Some((program, args, env)) = recipe {
            tmux::create(&runtime, &program, &args, &resolved, &env, rows, cols)
                .map_err(|error| error.to_string())?;
        } else {
            tmux::create_terminal(&runtime, &resolved, rows, cols)
                .map_err(|error| error.to_string())?;
        }
        confirm_runtime_started(
            &runtime,
            session.as_ref().map_or("Terminal", |s| s.harness.as_str()),
        )?;
        if let Some(session) = session.as_ref().filter(|session| {
            local
                && session.harness == "codex"
                && session.provider.is_some()
                && codex_plan != Some(SavedCreationPlan::FirstHumanGate)
        }) {
            if let Err(error) = record_codex_direct_resume(&home_dir, session, &runtime) {
                messaging_warning =
                    Some(format!("Codex native resume identity unavailable: {error}"));
            }
        }
    } else if let Some(session) = &session {
        if local && session.harness == "claude" && session.provider.is_none() {
            claude_events = Some((session_events_path(&run_dir, &session.session_ref), 0));
        }
    }
    if let Some(session) = &session {
        if local && session.harness == "grok" {
            if let Err(reason) = grok_messaging_gate(session, &repo, &Paths::from_env()) {
                messaging_warning = Some(reason);
            }
        }
        if local
            && session.harness == "claude"
            && ensure_observer(session, &runtime, &home_dir).is_err()
        {
            messaging_warning = Some(
                "Claude messaging observer unavailable; reopen this native session to retry".into(),
            );
        }
        if local
            && (session.harness == "codex" || session.harness == "grok")
            && messaging_warning.is_none()
            && ensure_bridge(session, &runtime, &home_dir).is_err()
        {
            messaging_warning = Some(format!(
                "{} messaging bridge unavailable; reopen this native session to retry",
                if session.harness == "grok" {
                    "Grok"
                } else {
                    "Codex"
                }
            ));
        }
    }
    if let RuntimeStatus::Live {
        rows: existing_rows,
        cols: existing_cols,
        ..
    } = status
    {
        // The attach client deliberately ignores its own size. A fresh viewer
        // already has the requested parser dimensions, so the UI cannot infer
        // an old persisted window size from that viewer after reopening.
        if (existing_rows, existing_cols) != (rows, cols)
            && !tmux::resize(&runtime, rows, cols).map_err(|error| error.to_string())?
        {
            return Err(
                "runtime disappeared before attachment; its saved mapping was retained for retry"
                    .into(),
            );
        }
    }
    let attach = tmux::attach_command(&runtime).map_err(|error| error.to_string())?;
    let args = attach.args.iter().map(String::as_str).collect::<Vec<_>>();
    // The viewer's cwd is unrelated to the durable process cwd. Keep attaching
    // possible even if a live process's repository has since been moved.
    let pane = Pane::spawn(&attach.program, &args, &home_dir, rows, cols, &[])
        .map_err(|error| error.to_string())?;
    Ok(Outcome {
        pane,
        runtime,
        created,
        claude_events,
        messaging_warning,
    })
}

pub(crate) fn grok_messaging_gate(
    session: &Session,
    workspace: &Path,
    paths: &Paths,
) -> Result<(), String> {
    let executable = doctor::configured_executable("grok", session.launch.executable.as_deref());
    let availability = doctor::check_harness("grok", &executable, paths);
    if let Some(reason) = availability.unavailable {
        return Err(format!("Grok messaging unavailable: {reason}"));
    }
    let check = availability
        .messaging
        .ok_or("Grok messaging check is missing")?;
    if !check.is_ready() {
        return Err(format!("Grok messaging unavailable: {}", check.label()));
    }
    let trust = doctor::workspace_check("grok", workspace, paths)
        .ok_or("Grok workspace trust check is missing")?;
    if !trust.is_ready() {
        return Err(format!("Grok messaging unavailable: {}", trust.label()));
    }
    Ok(())
}

fn record_codex_direct_resume(
    home: &Path,
    session: &Session,
    runtime: &TmuxRuntimeLocator,
) -> Result<(), String> {
    let mapping = session
        .provider
        .as_ref()
        .ok_or("Codex has no saved thread ID")?;
    let runtime_id = format!("tmux:{}:{}", runtime.private_server, runtime.session);
    if mapping.provider != "codex"
        || mapping.provider_session_id.is_empty()
        || mapping.runtime_id != runtime_id
    {
        return Err("Codex saved provider mapping is invalid".into());
    }
    let native = tmux::native_identity(runtime)
        .map_err(|error| error.to_string())?
        .ok_or("Codex direct resume has no live native pane")?;
    CodexIntents::new(home, &session.session_ref, native)?
        .record_direct_resume(&mapping.provider_session_id)
}

fn ensure_observer(
    session: &Session,
    runtime: &TmuxRuntimeLocator,
    home_dir: &Path,
) -> Result<(), String> {
    let exe = std::env::current_exe().map_err(|error| error.to_string())?;
    ensure_observer_with_exe(session, runtime, home_dir, &exe)
}

fn ensure_bridge(
    session: &Session,
    runtime: &TmuxRuntimeLocator,
    home_dir: &Path,
) -> Result<(), String> {
    let exe = std::env::current_exe().map_err(|error| error.to_string())?;
    ensure_bridge_with_exe(session, runtime, home_dir, &exe)
}

fn ensure_bridge_with_exe(
    session: &Session,
    runtime: &TmuxRuntimeLocator,
    home_dir: &Path,
    exe: &Path,
) -> Result<(), String> {
    match tmux::inspect_sidecar(runtime, "bridge").map_err(|error| error.to_string())? {
        RuntimeStatus::Live { .. } => return Ok(()),
        RuntimeStatus::Dead { .. } => {
            tmux::kill_sidecar(runtime, "bridge").map_err(|error| error.to_string())?;
        }
        RuntimeStatus::Missing => {}
    }
    let exe = exe.to_str().ok_or("TaskAI executable path is not UTF-8")?;
    tmux::create_sidecar(
        runtime,
        "bridge",
        exe,
        &["bridge".into(), session.session_ref.clone()],
        home_dir,
        &[
            ("TASKAI_HOME".into(), home_dir.display().to_string()),
            ("TASKAI_SESSION_REF".into(), session.session_ref.clone()),
        ],
    )
    .map_err(|error| error.to_string())
}

fn ensure_observer_with_exe(
    session: &Session,
    runtime: &TmuxRuntimeLocator,
    home_dir: &Path,
    exe: &Path,
) -> Result<(), String> {
    match tmux::inspect_sidecar(runtime, "observer").map_err(|error| error.to_string())? {
        RuntimeStatus::Live { .. } => return Ok(()),
        RuntimeStatus::Dead { .. } => {
            tmux::kill_sidecar(runtime, "observer").map_err(|error| error.to_string())?;
        }
        RuntimeStatus::Missing => {}
    }
    let exe = exe.to_str().ok_or("TaskAI executable path is not UTF-8")?;
    tmux::create_sidecar(
        runtime,
        "observer",
        exe,
        &["observer".into(), session.session_ref.clone()],
        home_dir,
        &[
            ("TASKAI_HOME".into(), home_dir.display().to_string()),
            ("TASKAI_SESSION_REF".into(), session.session_ref.clone()),
        ],
    )
    .map_err(|error| error.to_string())
}

fn recipe(
    session: &Session,
    runtime: &TmuxRuntimeLocator,
    home_dir: &Path,
) -> Result<Recipe, String> {
    let local = runtime.target == ConnectionTarget::Local;
    let resume_id = session
        .provider
        .as_ref()
        .map(|provider| provider.provider_session_id.as_str());
    let cold_gate = if session.harness == "codex" && local {
        resume_id
            .map(|id| CodexIntents::saved_creation_plan(home_dir, &session.session_ref, id))
            .transpose()?
            == Some(SavedCreationPlan::FirstHumanGate)
    } else {
        false
    };
    let mut env = vec![("TASKAI_SESSION_REF".into(), session.session_ref.clone())];
    let (program, args) = match session.harness.as_str() {
        "codex" if local && (resume_id.is_none() || cold_gate) => (
            std::env::current_exe()
                .map_err(|error| error.to_string())?
                .display()
                .to_string(),
            vec!["gate".into(), session.session_ref.clone()],
        ),
        "opencode" if local => (
            {
                validate_model("opencode", &session.launch)?;
                let executable = session.launch.executable.clone().unwrap_or_else(|| taskai_connectors::opencode::OpenCodeConnector::default().bin);
                let executable = tmux::provider_launch_command(&runtime.target, &executable, &[]).map_err(|error| error.to_string())?.program;
                env.push(("TASKAI_OPENCODE_BIN".into(), executable));
                env.push(("TASKAI_OPENCODE_SESSION_ID".into(), resume_id.unwrap_or_default().into()));
                std::env::current_exe().map_err(|error| error.to_string())?.display().to_string()
            },
            vec!["__opencode-runtime".into()],
        ),
        "opencode" => return Err("remote OpenCode needs TaskAI installed on that host; other harnesses support remote tmux persistence".into()),
        "grok" if local => {
            validate_model("grok", &session.launch)?;
            let mapping = session.provider.as_ref().ok_or("Grok has no saved native session ID")?;
            let runtime_id = format!("tmux:{}:{}", runtime.private_server, runtime.session);
            if mapping.provider != "grok"
                || mapping.runtime_id != runtime_id
                || !is_grok_session_id(&mapping.provider_session_id)
            {
                return Err("Grok saved session ID is not for this exact runtime".into());
            }
            let leader_socket = grok::leader_socket_path(&home_dir.join("run"), &session.session_ref);
            let tool_socket = grok::tool_socket_path(&home_dir.join("run"), &session.session_ref);
            let mode_socket = grok::mode_socket_path(&home_dir.join("run"), &session.session_ref);
            if [
                leader_socket.as_path(),
                tool_socket.as_path(),
                mode_socket.as_path(),
            ]
            .iter()
            .any(|path| path.to_str().is_none())
            {
                return Err("Grok private socket path is not UTF-8".into());
            }
            env.extend(grok::pane_env(&mapping.provider_session_id, &tool_socket));
            env.push(("TASKAI_MODE_SOCKET".into(), mode_socket.display().to_string()));
            let connector = GrokConnector {
                bin: doctor::configured_executable("grok", session.launch.executable.as_deref()),
            };
            if connector.bin.trim().is_empty() {
                return Err("Grok executable override is empty".into());
            }
            connector.pane_command(
                &leader_socket,
                &mapping.provider_session_id,
                GrokPermissionMode::Prompting,
                session.launch.model.as_deref(),
            )
        }
        other => configured_native_command(&runtime.target, other, &session.launch, resume_id)?,
    };
    if local {
        env.push(("TASKAI_HOME".into(), home_dir.display().to_string()));
    }
    let command = tmux::provider_launch_command(&runtime.target, &program, &args)
        .map_err(|error| error.to_string())?;
    Ok((command.program, command.args, env))
}

fn validate_model(harness: &str, settings: &LaunchSettings) -> Result<(), String> {
    if settings
        .model
        .as_ref()
        .is_some_and(|model| model.trim().is_empty())
    {
        return Err("Model override cannot be empty; choose Native default instead".into());
    }
    if harness == "opencode" && settings.model.is_some() {
        return Err("OpenCode attach does not support a model launch override; use native OpenCode settings".into());
    }
    Ok(())
}

fn configured_native_command(
    target: &ConnectionTarget,
    harness: &str,
    settings: &LaunchSettings,
    resume_id: Option<&str>,
) -> Result<(String, Vec<String>), String> {
    validate_model(harness, settings)?;
    let (default_program, mut args) = match harness {
        "claude" if *target != ConnectionTarget::Local => ("claude".into(), Vec::new()),
        "claude" => ClaudeConnector::default().launch_command(),
        "codex" => CodexConnector::default().launch_command(),
        "grok" => GrokConnector::default().launch_command(),
        "opencode" => (
            taskai_connectors::opencode::OpenCodeConnector::default().bin,
            Vec::new(),
        ),
        other => return Err(format!("no connector for {other}")),
    };
    let program = settings.executable.clone().unwrap_or(default_program);
    if program.trim().is_empty() {
        return Err("Executable override cannot be empty; choose Native default instead".into());
    }
    // Each verified native CLI accepts --model as an option. Never turn a model
    // string into a prompt, shell source, or part of the exact provider resume ID.
    if let Some(model) = &settings.model {
        args.extend(["--model".into(), model.clone()]);
    }
    if let Some(id) = resume_id {
        args.extend([
            if harness == "codex" {
                "resume"
            } else {
                "--resume"
            }
            .into(),
            id.into(),
        ]);
    }
    Ok((program, args))
}

/// Prepare target-local argv for a settings tmux runtime. The caller persists
/// its server setup locator and passes this command to tmux::create. No SSH
/// client, agent identity, hook, credential or prompt is created by this helper.
pub fn settings_command(
    profile: &ServerProfile,
    harness: Option<&str>,
) -> Result<tmux::AttachCommand, String> {
    let command = match harness {
        Some(harness) => {
            let settings = profile.harnesses.get(harness).cloned().unwrap_or_default();
            let launch = LaunchSettings {
                executable: settings.executable,
                model: settings.model,
            };
            let (program, args) =
                configured_native_command(&profile.target, harness, &launch, None)?;
            tmux::provider_launch_command(&profile.target, &program, &args)
                .map_err(|error| error.to_string())?
        }
        None => tmux::AttachCommand {
            program: "/bin/sh".into(),
            args: vec!["-c".into(), "exec \"${SHELL:-/bin/sh}\" -l".into()],
        },
    };
    let mut args = vec![
        "-c".into(),
        SETTINGS_EXEC_SCRIPT.into(),
        "taskai-settings-exec".into(),
        command.program,
    ];
    args.extend(command.args);
    Ok(tmux::AttachCommand {
        program: "/bin/sh".into(),
        args,
    })
}

/// The existing OpenCode serve/attach runtime resumes only this exact native
/// session. Empty means a fresh native session, never "most recent".
pub fn append_opencode_session(args: &mut Vec<String>, session_id: Option<&str>) {
    if let Some(session_id) = session_id.filter(|value| !value.is_empty()) {
        args.extend(["--session".into(), session_id.into()]);
    }
}

pub fn confirm_runtime_started(runtime: &TmuxRuntimeLocator, harness: &str) -> Result<(), String> {
    // Check the same process twice across a settle window. Counting time spent
    // waiting for the first SSH response against that window can falsely reject
    // a healthy runtime on a slow connection. Each inspection has its own
    // bounded control-command deadline.
    let observe = || match tmux::inspect(runtime) {
        Ok(RuntimeStatus::Live { pid, .. }) => Ok(pid),
        Ok(RuntimeStatus::Dead { exit_code, .. }) => Err(format!(
            "{harness} exited during startup{}; its tmux mapping was retained for inspection",
            exit_code.map_or_else(String::new, |code| format!(" ({code})"))
        )),
        Ok(RuntimeStatus::Missing) => Err(format!(
            "{harness} runtime disappeared during startup; its mapping was retained"
        )),
        Err(error) => Err(format!(
            "runtime was created but could not be verified on {}: {error}",
            runtime.target
        )),
    };
    let first = observe()?;
    std::thread::sleep(Duration::from_millis(250));
    if observe()? != first {
        return Err(format!(
            "{harness} process changed during startup; its mapping was retained"
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use super::*;
    use crate::bridge::codex_intent::SavedThreadOrigin;
    use taskai_core::registry::{ProviderMapping, Registry, Source};

    struct RuntimeGuard(TmuxRuntimeLocator);
    impl Drop for RuntimeGuard {
        fn drop(&mut self) {
            let _ = tmux::kill(&self.0);
        }
    }

    #[test]
    fn missing_claude_observer_recreates_without_restarting_native_pane() {
        if tmux::probe(&ConnectionTarget::Local).is_err() {
            return;
        }
        use std::os::unix::fs::PermissionsExt;
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        std::fs::create_dir(&workspace).unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let session_ref = registry
            .create_session(&task, None, "Claude", "claude", &workspace, Source::HumanUi)
            .unwrap();
        registry
            .bind_runtime(
                &session_ref,
                &format!("taskai-observer-{}", std::process::id()),
            )
            .unwrap();
        let session = registry.session(&session_ref).unwrap().clone();
        let runtime = session.runtime.clone().unwrap();
        let _guard = RuntimeGuard(runtime.clone());
        tmux::create(
            &runtime,
            "/bin/sh",
            &["-c".into(), "while :; do sleep 1; done".into()],
            &workspace,
            &[],
            24,
            80,
        )
        .unwrap();
        let native = tmux::native_identity(&runtime).unwrap().unwrap();
        let fake = root.path().join("fake-observer");
        std::fs::write(&fake, "#!/bin/sh\nwhile :; do sleep 1; done\n").unwrap();
        std::fs::set_permissions(&fake, std::fs::Permissions::from_mode(0o755)).unwrap();
        ensure_observer_with_exe(&session, &runtime, root.path(), &fake).unwrap();
        let first = match tmux::inspect_sidecar(&runtime, "observer").unwrap() {
            RuntimeStatus::Live { pid, .. } => pid,
            other => panic!("unexpected observer status: {other:?}"),
        };
        ensure_observer_with_exe(&session, &runtime, root.path(), &fake).unwrap();
        assert!(matches!(
            tmux::inspect_sidecar(&runtime, "observer").unwrap(),
            RuntimeStatus::Live { pid, .. } if pid == first
        ));
        assert!(tmux::kill_sidecar(&runtime, "observer").unwrap());
        ensure_observer_with_exe(&session, &runtime, root.path(), &fake).unwrap();
        assert!(matches!(
            tmux::inspect_sidecar(&runtime, "observer").unwrap(),
            RuntimeStatus::Live { pid, .. } if pid != first
        ));
        assert_eq!(tmux::native_identity(&runtime).unwrap().unwrap(), native);
    }

    #[test]
    fn snapshotted_models_executables_and_resume_ids_stay_separate_native_arguments() {
        let settings = LaunchSettings {
            executable: Some("/provider path/cli with spaces".into()),
            model: Some("model ; stays one argument".into()),
        };
        for harness in ["claude", "codex", "grok"] {
            let (program, args) = configured_native_command(
                &ConnectionTarget::Local,
                harness,
                &settings,
                Some("exact id ; $(not shell)"),
            )
            .unwrap();
            assert_eq!(program, "/provider path/cli with spaces");
            assert_eq!(
                args,
                [
                    "--model",
                    "model ; stays one argument",
                    if harness == "codex" {
                        "resume"
                    } else {
                        "--resume"
                    },
                    "exact id ; $(not shell)"
                ]
            );
            let native = configured_native_command(
                &ConnectionTarget::Local,
                harness,
                &LaunchSettings::default(),
                None,
            )
            .unwrap();
            assert!(
                native.1.is_empty(),
                "legacy sessions retain native defaults"
            );
        }
        assert!(
            configured_native_command(&ConnectionTarget::Local, "opencode", &settings, None)
                .unwrap_err()
                .contains("does not support a model")
        );
        let remote = configured_native_command(
            &ConnectionTarget::ssh("fixture").unwrap(),
            "claude",
            &LaunchSettings::default(),
            None,
        )
        .unwrap();
        assert_eq!(
            remote.0, "claude",
            "local executable overrides never leak into SSH defaults"
        );
    }

    #[test]
    fn settings_commands_are_target_local_and_strip_agent_context() {
        let mut profile = ServerProfile {
            server_id: "fixture".into(),
            name: "Fixture".into(),
            target: ConnectionTarget::Local,
            repository_root: None,
            harnesses: Default::default(),
            archived: false,
            setup_runtimes: Default::default(),
        };
        profile.harnesses.insert(
            "claude".into(),
            taskai_core::registry::HarnessSettings {
                enabled: true,
                executable: Some("/custom cli".into()),
                model: Some("explicit model".into()),
            },
        );
        let local = settings_command(&profile, Some("claude")).unwrap();
        assert_eq!(local.program, "/bin/sh");
        assert_eq!(
            &local.args[3..],
            ["/custom cli", "--model", "explicit model"]
        );
        profile.target = ConnectionTarget::ssh("fixture-host").unwrap();
        let remote = settings_command(&profile, Some("claude")).unwrap();
        assert_eq!(remote.program, "/bin/sh");
        assert!(!remote.args.iter().any(|arg| arg == "ssh"));
        assert_eq!(
            &remote.args[remote.args.len() - 3..],
            ["/custom cli", "--model", "explicit model"]
        );
        let terminal = settings_command(&profile, None).unwrap();
        assert!(terminal.args.last().unwrap().contains("${SHELL:-/bin/sh}"));
        assert!(!terminal.args.iter().any(|arg| arg == "--model"));
    }

    #[cfg(unix)]
    #[test]
    fn settings_terminal_clears_all_taskai_context_but_preserves_auth_environment() {
        use std::os::unix::fs::PermissionsExt;
        let temp = tempfile::tempdir().unwrap();
        let shell = temp.path().join("settings test shell");
        std::fs::write(&shell, "#!/bin/sh\nprintf '%s|%s|%s|%s' \"${TASKAI_SESSION_REF-unset}\" \"${TASKAI_HOME-unset}\" \"${TASKAI_FUTURE_CONTEXT-unset}\" \"$AUTH_PRESERVE_TEST\"\n").unwrap();
        std::fs::set_permissions(&shell, std::fs::Permissions::from_mode(0o700)).unwrap();
        let output = std::process::Command::new("/bin/sh")
            .args([
                "-c",
                SETTINGS_EXEC_SCRIPT,
                "settings-test",
                "/bin/sh",
                "-c",
                "exec \"$SHELL\" -l",
            ])
            .env("SHELL", &shell)
            .env("TASKAI_SESSION_REF", "s42")
            .env("TASKAI_HOME", "/fixture/taskai")
            .env("TASKAI_FUTURE_CONTEXT", "must not leak")
            .env("AUTH_PRESERVE_TEST", "fixture-auth")
            .output()
            .unwrap();
        assert!(output.status.success());
        assert_eq!(output.stdout, b"unset|unset|unset|fixture-auth");
    }

    #[test]
    fn grok_workspace_gate_reports_native_trust_remedy_from_private_paths() {
        use std::os::unix::fs::PermissionsExt;

        let root = tempfile::tempdir().unwrap();
        let paths = Paths::under(root.path());
        let workspace = root.path().join("workspace");
        std::fs::create_dir(&workspace).unwrap();
        let home = paths.grok_home.as_ref().unwrap();
        std::fs::create_dir_all(home).unwrap();
        let executable = root.path().join("fake-grok");
        std::fs::write(&executable, "#!/bin/sh\nprintf 'grok 1.0.77\\n'\n").unwrap();
        std::fs::set_permissions(&executable, std::fs::Permissions::from_mode(0o700)).unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let key = registry
            .create_session(&task, None, "Grok", "grok", &workspace, Source::HumanUi)
            .unwrap();
        let mut session = registry.session(&key).unwrap().clone();
        session.launch.executable = Some(executable.display().to_string());
        let reason = grok_messaging_gate(&session, &workspace, &paths).unwrap_err();
        assert!(reason.contains("workspace not trusted by Grok"), "{reason}");
        assert!(reason.contains("accept its trust prompt"), "{reason}");
        std::fs::write(
            home.join("trusted_folders.toml"),
            format!("[folders.\"{}\"]\ntrusted = true\n", workspace.display()),
        )
        .unwrap();
        grok_messaging_gate(&session, &workspace, &paths).unwrap();
    }

    #[test]
    fn grok_mapping_is_persisted_once_before_launch_and_reused_on_reopen() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        let task = registry
            .create_task("Grok", root.path(), Source::HumanUi)
            .unwrap();
        let key = registry
            .create_session(&task, None, "Grok", "grok", root.path(), Source::HumanUi)
            .unwrap();
        registry.bind_runtime(&key, "taskai-grok-mapping").unwrap();
        let mut stale = Registry::open(&root.path().join("state")).unwrap();
        let first = ensure_grok_mapping(&mut registry, &key).unwrap();
        assert!(is_grok_session_id(&first.provider_session_id));
        assert!(!first.connector_version.is_empty());
        assert_eq!(ensure_grok_mapping(&mut stale, &key).unwrap(), first);
        assert_eq!(ensure_grok_mapping(&mut registry, &key).unwrap(), first);
        assert_eq!(
            registry.session(&key).unwrap().provider.as_ref(),
            Some(&first)
        );
        let mut reopened = Registry::open(&root.path().join("state")).unwrap();
        assert_eq!(ensure_grok_mapping(&mut reopened, &key).unwrap(), first);
        assert_eq!(
            reopened.session(&key).unwrap().provider.as_ref(),
            Some(&first)
        );
    }

    #[test]
    fn local_grok_requires_saved_exact_id_and_routes_both_private_sockets() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        let task = registry
            .create_task("Grok", root.path(), Source::HumanUi)
            .unwrap();
        let key = registry
            .create_session(&task, None, "Grok", "grok", root.path(), Source::HumanUi)
            .unwrap();
        registry.bind_runtime(&key, "taskai-grok-recipe").unwrap();
        let mut session = registry.session(&key).unwrap().clone();
        let runtime = session.runtime.clone().unwrap();
        assert!(recipe(&session, &runtime, root.path()).is_err());
        session.provider = Some(ProviderMapping {
            provider: "grok".into(),
            provider_session_id: "01234567-89ab-4cde-8fab-0123456789ab".into(),
            runtime_id: format!("tmux:{}:{}", runtime.private_server, runtime.session),
            connector_version: "1.0.77".into(),
        });
        let (_, args, env) = recipe(&session, &runtime, root.path()).unwrap();
        assert!(args.contains(&"--leader".into()));
        assert!(args.contains(&"01234567-89ab-4cde-8fab-0123456789ab".into()));
        assert!(env.contains(&(
            "GROK_SESSION_ID".into(),
            "01234567-89ab-4cde-8fab-0123456789ab".into()
        )));
        assert!(env
            .iter()
            .any(|(key, value)| key == "TASKAI_MODE_SOCKET" && value.ends_with(".mode.sock")));
        assert!(env
            .iter()
            .any(|(key, value)| key == "TASKAI_TOOL_SOCKET" && value.ends_with(".tools.sock")));
        session.provider.as_mut().unwrap().runtime_id = "other-runtime".into();
        assert!(recipe(&session, &runtime, root.path()).is_err());
    }

    #[test]
    fn launch_recipes_preserve_exact_provider_identity_and_remote_context_boundary() {
        let temp = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(&temp.path().join("state")).unwrap();
        let task = registry
            .create_task("Recipe", temp.path(), Source::HumanUi)
            .unwrap();
        let key = registry
            .create_session(&task, None, "Lead", "claude", temp.path(), Source::HumanUi)
            .unwrap();
        let mut session = registry.session(&key).unwrap().clone();
        session.provider = Some(ProviderMapping {
            provider: "claude".into(),
            provider_session_id: "exact-id ; stays argv".into(),
            runtime_id: "runtime".into(),
            connector_version: "test".into(),
        });
        let mut runtime = TmuxRuntimeLocator {
            target: ConnectionTarget::Local,
            private_server: "taskai-recipe-test".into(),
            session: key.clone(),
        };
        for (harness, resume_flag) in [("claude", "--resume"), ("codex", "resume")] {
            session.harness = harness.into();
            let (_, args, env) = recipe(&session, &runtime, temp.path()).unwrap();
            assert_eq!(
                &args[args.len() - 2..],
                &[resume_flag, "exact-id ; stays argv"]
            );
            assert!(env.iter().any(|(name, _)| name == "TASKAI_HOME"));
        }
        runtime.target = ConnectionTarget::ssh("recipe-remote").unwrap();
        let (_, _, env) = recipe(&session, &runtime, temp.path()).unwrap();
        assert_eq!(env, vec![("TASKAI_SESSION_REF".into(), key)]);
        session.harness = "opencode".into();
        assert!(recipe(&session, &runtime, temp.path())
            .unwrap_err()
            .contains("remote OpenCode"));
        runtime.target = ConnectionTarget::Local;
        assert_eq!(
            recipe(&session, &runtime, temp.path()).unwrap().1,
            ["__opencode-runtime"]
        );
        session.launch.executable = Some("/path/opencode custom".into());
        let (_, _, env) = recipe(&session, &runtime, temp.path()).unwrap();
        assert!(env.contains(&("TASKAI_OPENCODE_BIN".into(), "/path/opencode custom".into())));
        let saved_id = env
            .iter()
            .find(|(name, _)| name == "TASKAI_OPENCODE_SESSION_ID")
            .unwrap()
            .1
            .as_str();
        let mut args = vec!["attach".into(), "http://127.0.0.1:1234".into()];
        append_opencode_session(&mut args, Some(saved_id));
        assert_eq!(&args[2..], ["--session", "exact-id ; stays argv"]);
        append_opencode_session(&mut args, None);
        append_opencode_session(&mut args, Some(""));
        assert_eq!(args.len(), 4);
    }

    #[test]
    fn new_local_codex_uses_a_gate_and_saved_id_resumes_exactly() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        let task = registry
            .create_task("Task", root.path(), Source::HumanUi)
            .unwrap();
        let session_ref = registry
            .create_session(&task, None, "Codex", "codex", root.path(), Source::HumanUi)
            .unwrap();
        registry
            .bind_runtime(&session_ref, "taskai-codex-gate-test")
            .unwrap();
        let session = registry.session(&session_ref).unwrap();
        let runtime = session.runtime.as_ref().unwrap();
        let (program, args, env) = recipe(session, runtime, root.path()).unwrap();
        assert_eq!(
            program,
            std::env::current_exe().unwrap().display().to_string()
        );
        assert_eq!(args, ["gate", session_ref.as_str()]);
        assert!(env.contains(&("TASKAI_SESSION_REF".into(), session_ref.clone())));
        assert!(env.iter().any(|(key, _)| key == "TASKAI_HOME"));
        let mut restored = session.clone();
        restored.provider = Some(ProviderMapping {
            provider: "codex".into(),
            provider_session_id: "owned exact id".into(),
            runtime_id: "tmux:taskai-codex-gate-test:s1".into(),
            connector_version: "test".into(),
        });
        let (program, args, _) = recipe(&restored, runtime, root.path()).unwrap();
        assert_eq!(program, "codex");
        assert_eq!(args, ["resume", "owned exact id"]);
    }

    #[test]
    fn restored_codex_launch_records_only_its_exact_live_direct_resume() {
        if tmux::probe(&ConnectionTarget::Local).is_err() {
            return;
        }
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        std::fs::create_dir(&workspace).unwrap();
        let fake = root.path().join("fake-codex");
        std::fs::write(&fake, "#!/bin/sh\n[ \"$1\" = resume ] && [ \"$2\" = owned-thread ] || exit 3\nexec sleep 10\n").unwrap();
        std::fs::set_permissions(&fake, std::fs::Permissions::from_mode(0o755)).unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        registry
            .set_server_harness(
                "local",
                "codex",
                taskai_core::registry::HarnessSettings {
                    enabled: true,
                    executable: Some(fake.display().to_string()),
                    model: None,
                },
                Source::HumanUi,
            )
            .unwrap();
        let task = registry
            .create_task("Task", &workspace, Source::HumanUi)
            .unwrap();
        let session_ref = registry
            .create_session(&task, None, "Codex", "codex", &workspace, Source::HumanUi)
            .unwrap();
        registry
            .bind_runtime(
                &session_ref,
                &format!("taskai-resume-test-{}", std::process::id()),
            )
            .unwrap();
        let runtime = registry
            .session(&session_ref)
            .unwrap()
            .runtime
            .clone()
            .unwrap();
        let _guard = RuntimeGuard(runtime.clone());
        registry
            .bind_provider(
                &session_ref,
                ProviderMapping {
                    provider: "codex".into(),
                    provider_session_id: "owned-thread".into(),
                    runtime_id: format!("tmux:{}:{}", runtime.private_server, runtime.session),
                    connector_version: "fixture".into(),
                },
            )
            .unwrap();
        let request = || Request {
            session: Some(registry.session(&session_ref).unwrap().clone()),
            runtime: runtime.clone(),
            home_dir: root.path().into(),
            repo: workspace.clone(),
            rows: 24,
            cols: 80,
        };
        let first = run(request()).unwrap();
        assert!(first.created);
        let native = tmux::native_identity(&runtime).unwrap().unwrap();
        let intents = CodexIntents::new(root.path(), &session_ref, native.clone()).unwrap();
        assert_eq!(
            intents.saved_thread_origin("owned-thread").unwrap(),
            SavedThreadOrigin::DirectResume
        );
        assert!(!intents.human_established("owned-thread").unwrap());
        drop(first);
        let second = run(request()).unwrap();
        assert!(!second.created);
        assert_eq!(tmux::native_identity(&runtime).unwrap().unwrap(), native);
        drop(second);
    }

    #[test]
    fn saved_unattempted_codex_thread_reopens_at_human_gate_not_empty_tui_resume() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(&root.path().join("state")).unwrap();
        let task = registry
            .create_task("Task", root.path(), Source::HumanUi)
            .unwrap();
        let session_ref = registry
            .create_session(&task, None, "Codex", "codex", root.path(), Source::HumanUi)
            .unwrap();
        registry
            .bind_runtime(&session_ref, "taskai-cold-reopen-test")
            .unwrap();
        let runtime = registry
            .session(&session_ref)
            .unwrap()
            .runtime
            .clone()
            .unwrap();
        registry
            .bind_provider(
                &session_ref,
                ProviderMapping {
                    provider: "codex".into(),
                    provider_session_id: "owned-thread".into(),
                    runtime_id: format!("tmux:{}:{}", runtime.private_server, runtime.session),
                    connector_version: "fixture".into(),
                },
            )
            .unwrap();
        let old = crate::tmux::NativeIdentity {
            pane_id: "%77".into(),
            pid: 999_999,
            proc_start: "old-gate".into(),
        };
        let intents = CodexIntents::new(root.path(), &session_ref, old.clone()).unwrap();
        intents.begin_creation().unwrap();
        intents.record_created("owned-thread").unwrap();
        std::fs::write(
            root.path().join("run/codex/s1.gate.json"),
            serde_json::json!({
                "sessionRef":"s1", "paneId":old.pane_id, "pid":old.pid,
                "procStart":old.proc_start,
            })
            .to_string(),
        )
        .unwrap();
        let saved = registry.session(&session_ref).unwrap();
        let (program, args, _) = recipe(saved, &runtime, root.path()).unwrap();
        assert_eq!(
            program,
            std::env::current_exe().unwrap().display().to_string()
        );
        assert_eq!(args, ["gate", "s1"]);
        CodexIntents::stage_gate_adoption(root.path(), &session_ref, "owned-thread").unwrap();
        assert!(root.path().join("run/codex/s1.adoption.json").exists());
        intents.begin_first_turn("owned-thread").unwrap();
        assert!(
            recipe(saved, &runtime, root.path()).is_err(),
            "an uncertain first turn cannot reopen the gate"
        );
        intents.record_first_turn_accepted("owned-thread").unwrap();
        let (program, args, _) = recipe(saved, &runtime, root.path()).unwrap();
        assert_eq!(program, "codex");
        assert_eq!(args, ["resume", "owned-thread"]);
    }

    #[test]
    fn utility_launch_attaches_same_runtime_and_rejects_missing_repository() {
        if tmux::probe(&ConnectionTarget::Local).is_err() {
            return;
        }
        let temp = tempfile::tempdir().unwrap();
        let runtime = TmuxRuntimeLocator {
            target: ConnectionTarget::Local,
            private_server: format!("taskai-launch-test-{}", std::process::id()),
            session: "t1-terminal".into(),
        };
        let _guard = RuntimeGuard(runtime.clone());
        let request = |repo| Request {
            session: None,
            runtime: runtime.clone(),
            home_dir: temp.path().into(),
            repo,
            rows: 24,
            cols: 80,
        };
        assert!(run(request(temp.path().join("missing"))).is_err());
        assert_eq!(tmux::inspect(&runtime).unwrap(), RuntimeStatus::Missing);
        let first = run(request(temp.path().into())).unwrap();
        assert!(first.created);
        assert!(first.claude_events.is_none());
        let original = tmux::inspect(&runtime).unwrap();
        drop(first);
        let RuntimeStatus::Live { pid, .. } = original else {
            panic!("expected live utility runtime");
        };
        let mut resized_request = request(temp.path().into());
        resized_request.rows = 31;
        resized_request.cols = 114;
        let second = run(resized_request).unwrap();
        assert!(!second.created);
        assert_eq!(second.pane.size(), (31, 114));
        let resized = RuntimeStatus::Live {
            pid,
            rows: 31,
            cols: 114,
        };
        assert_eq!(tmux::inspect(&runtime).unwrap(), resized);
        drop(second);
        assert_eq!(tmux::inspect(&runtime).unwrap(), resized);
    }

    #[test]
    fn settings_shell_uses_its_recorded_tmux_locator_and_survives_viewer_close() {
        if tmux::probe(&ConnectionTarget::Local).is_err() {
            return;
        }
        let temp = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(&temp.path().join("state")).unwrap();
        let runtime = registry
            .bind_server_setup(
                "local",
                "terminal",
                &format!("taskai-settings-test-{}", std::process::id()),
            )
            .unwrap();
        let _guard = RuntimeGuard(runtime.clone());
        let command = settings_command(registry.server("local").unwrap(), None).unwrap();
        tmux::create(
            &runtime,
            &command.program,
            &command.args,
            temp.path(),
            &[("TASKAI_SESSION_REF".into(), "must-be-unset".into())],
            24,
            80,
        )
        .unwrap();
        let attach = tmux::attach_command(&runtime).unwrap();
        let refs: Vec<&str> = attach.args.iter().map(String::as_str).collect();
        let mut viewer = Pane::spawn(&attach.program, &refs, temp.path(), 24, 80, &[]).unwrap();
        // Match the host's first-frame input gate: spawning an attach process
        // does not mean tmux has switched its PTY to raw input yet. A command
        // sent during that setup can be echoed by the outer line discipline
        // and discarded when tmux initializes the terminal.
        let deadline = std::time::Instant::now() + Duration::from_secs(10);
        while !viewer.has_output() {
            assert!(
                std::time::Instant::now() < deadline,
                "settings viewer did not render its first frame; runtime: {:?}; native pane: {:?}",
                tmux::inspect(&runtime),
                tmux::capture_test_terminal(&runtime)
            );
            std::thread::sleep(Duration::from_millis(20));
        }
        viewer
            .write_bytes(b"printf '%s' \"${TASKAI_SESSION_REF-unset}\" > settings-context\r")
            .unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(20);
        while std::fs::read_to_string(temp.path().join("settings-context"))
            .ok()
            .as_deref()
            != Some("unset")
        {
            // Native capture confirms early input stays queued while this
            // account's zsh runs Conda initialization. Under parallel tests that
            // startup can exceed ten seconds; use the same bounded eventual
            // budget as the app's real-login-shell lifecycle test.
            assert!(
                std::time::Instant::now() < deadline,
                "settings input did not reach shell: {}; runtime: {:?}; native pane: {:?}",
                viewer.text(),
                tmux::inspect(&runtime),
                tmux::capture_test_terminal(&runtime)
            );
            std::thread::sleep(Duration::from_millis(20));
        }
        let original = tmux::inspect(&runtime).unwrap();
        assert!(matches!(original, RuntimeStatus::Live { .. }));
        drop(viewer);
        let second = Pane::spawn(&attach.program, &refs, temp.path(), 24, 80, &[]).unwrap();
        assert_eq!(tmux::inspect(&runtime).unwrap(), original);
        drop(second);
        assert_eq!(
            registry.tasks().len(),
            0,
            "settings do not create agent tasks"
        );
        assert!(tmux::kill(&runtime).unwrap());
        assert_eq!(tmux::inspect(&runtime).unwrap(), RuntimeStatus::Missing);
    }
}
