// ABOUTME: Blocking launch preparation for a disposable background worker.
// ABOUTME: It owns no registry state; locators must be persisted before calling it.

use std::{
    path::{Path, PathBuf},
    time::Duration,
};

use taskai_connectors::{
    claude::{install_context_hook, install_hooks, session_events_path, ClaudeConnector},
    codex::CodexConnector,
    grok::GrokConnector,
};
use taskai_core::registry::{
    ConnectionTarget, LaunchSettings, ServerProfile, Session, TmuxRuntimeLocator,
};
use taskai_pane::Pane;

use crate::tmux::{self, RuntimeStatus};

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
    } else if let Some(session) = &session {
        if local && session.harness == "claude" && session.provider.is_none() {
            claude_events = Some((session_events_path(&run_dir, &session.session_ref), 0));
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
    })
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
    let mut env = vec![("TASKAI_SESSION_REF".into(), session.session_ref.clone())];
    let (program, args) = match session.harness.as_str() {
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
    use super::*;
    use taskai_core::registry::{ProviderMapping, Registry, Source};

    struct RuntimeGuard(TmuxRuntimeLocator);
    impl Drop for RuntimeGuard {
        fn drop(&mut self) {
            let _ = tmux::kill(&self.0);
        }
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
