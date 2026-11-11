// ABOUTME: taskai binary — the live TUI, `session prompt` conduit client, `session launch`, `context`
// ABOUTME: service CLI, `mcp` stdio server, `doctor` readiness report, and `--dump-frame` for tests.

mod app;
mod bridge;
mod doctor;
mod forms;
mod gate;
mod helper;
mod launch;
mod messages;
mod observer;
mod remote_probes;
mod server_settings;
mod tmux;
mod ui_events;

use taskai_connectors::control;
use taskai_connectors::grok::forward_mode_hook;

use std::io::{self, Read};
use std::path::{Path, PathBuf};
use std::process::Command;

use taskai_connectors::opencode::OpenCodeConnector;
use taskai_core::registry::Registry;
use taskai_tui::color::set_truecolor;
use taskai_tui::render::dump;

fn taskai_home() -> PathBuf {
    if let Ok(h) = std::env::var("TASKAI_HOME") {
        return PathBuf::from(h);
    }
    PathBuf::from(std::env::var("HOME").unwrap_or_else(|_| ".".into())).join(".taskai")
}

fn io_err(e: impl std::fmt::Display) -> io::Error {
    io::Error::other(e.to_string())
}

fn main() -> io::Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.first().map(String::as_str) == Some("__opencode-runtime") {
        return opencode_runtime();
    }
    if args.first().map(String::as_str) == Some("observer") {
        return match args.as_slice() {
            [_, session_ref] => observer::run(&taskai_home(), session_ref).map_err(io_err),
            _ => Err(io::Error::other("usage: taskai observer <session_ref>")),
        };
    }
    if args.first().map(String::as_str) == Some("gate") {
        return match args.as_slice() {
            [_, session_ref] => gate::run(&taskai_home(), session_ref).map_err(io_err),
            _ => Err(io::Error::other("usage: taskai gate <session_ref>")),
        };
    }
    if args.first().map(String::as_str) == Some("bridge") {
        return match args.as_slice() {
            [_, session_ref] => bridge::run(&taskai_home(), session_ref).map_err(io_err),
            _ => Err(io::Error::other("usage: taskai bridge <session_ref>")),
        };
    }
    if args.first().map(String::as_str) == Some("session") {
        return session_cmd(&args[1..]);
    }
    if args.first().map(String::as_str) == Some("context") {
        return context_cmd(&args[1..]);
    }
    if args.first().map(String::as_str) == Some("helper") {
        return helper_cmd(&args[1..]);
    }
    if args.first().map(String::as_str) == Some("mcp") {
        return taskai_connectors::grok::run_mcp_stdio();
    }
    if args.first().map(String::as_str) == Some("grok-mode-hook") {
        if args.len() != 1 {
            return Err(io::Error::other("usage: taskai grok-mode-hook"));
        }
        // Native Grok owns the hook decision. This command only forwards evidence;
        // even a failed forward produces no stdout decision for the native hook.
        if let Err(reason) = grok_mode_hook(io::stdin().lock()) {
            eprintln!("Grok mode evidence unavailable: {reason}");
        }
        return Ok(());
    }
    if args.first().map(String::as_str) == Some("doctor") {
        return doctor::run(&taskai_home(), &args[1..]);
    }
    let mut screen: Option<String> = None;
    let mut attach: Option<PathBuf> = None;
    let (mut cols, mut rows) = (110usize, 30usize);
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--dump-frame" => {
                i += 1;
                screen = args.get(i).cloned();
            }
            "--cols" => {
                i += 1;
                cols = args.get(i).and_then(|v| v.parse().ok()).unwrap_or(cols);
            }
            "--rows" => {
                i += 1;
                rows = args.get(i).and_then(|v| v.parse().ok()).unwrap_or(rows);
            }
            "--attach-claude" => {
                i += 1;
                attach = args.get(i).map(PathBuf::from);
            }
            other => {
                eprintln!("unknown argument: {other}");
                std::process::exit(2);
            }
        }
        i += 1;
    }
    match screen {
        Some(name) => {
            set_truecolor(true);
            match dump(&name, cols, rows) {
                Some(frame) => {
                    println!("{}", frame.join("\n"));
                    Ok(())
                }
                None => {
                    eprintln!("unknown screen: {name}");
                    std::process::exit(2);
                }
            }
        }
        None => app::App::new(taskai_home())?.run(attach),
    }
}

fn grok_mode_hook(reader: impl Read) -> Result<(), String> {
    let socket = std::env::var_os("TASKAI_MODE_SOCKET")
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .ok_or("missing TASKAI_MODE_SOCKET")?;
    let session_id = std::env::var("GROK_SESSION_ID")
        .ok()
        .filter(|value| !value.is_empty())
        .ok_or("missing GROK_SESSION_ID")?;
    forward_mode_hook(reader, &socket, &session_id).map_err(|error| error.to_string())
}

/// Keeps OpenCode's already-approved serve + attach pair in the same durable tmux
/// runtime. This is an internal launcher, not a resident TaskAI service.
fn opencode_runtime() -> io::Result<()> {
    let cwd = std::env::current_dir()?;
    let mut connector = OpenCodeConnector::default();
    if let Ok(executable) = std::env::var("TASKAI_OPENCODE_BIN") {
        if executable.trim().is_empty() {
            return Err(io::Error::other("OpenCode executable override is empty"));
        }
        connector.bin = executable;
    }
    let server = connector.start_server(&cwd).map_err(io_err)?;
    let (program, mut args, envs) = server.attach_command();
    launch::append_opencode_session(
        &mut args,
        std::env::var("TASKAI_OPENCODE_SESSION_ID").ok().as_deref(),
    );
    let mut command = Command::new(program);
    command.args(args).envs(envs);
    taskai_core::mail::strip_messaging_env(&mut command);
    let status = command.status()?;
    drop(server);
    if status.success() {
        Ok(())
    } else {
        Err(io::Error::other(format!(
            "OpenCode attach exited with {status}"
        )))
    }
}

fn helper_cmd(args: &[String]) -> io::Result<()> {
    match args.first().map(String::as_str) {
        Some("guide") if args.len() == 1 => println!("{}", helper::guide()),
        Some("status") if args.len() == 1 => {
            println!("{}", helper_status(&taskai_home())?);
        }
        Some("request") if (2..=3).contains(&args.len()) => {
            let source = std::env::var("TASKAI_SESSION_REF").map_err(|_| io::Error::other("UI requests must originate in the dedicated TaskAI helper Manager"))?;
            let result = control::client_helper_ui(&control::sock_path(&taskai_home()), &source, &args[1], args.get(2).map(String::as_str)).map_err(io_err)?;
            if result["ok"] != true { return Err(io::Error::other(result["error"].as_str().unwrap_or("helper request failed"))); }
            println!("{}", result["message"].as_str().unwrap_or("Request queued for user review"));
        }
        _ => return Err(io::Error::other("usage: taskai helper guide | status | request open-settings [server-id] | request add-server | request new-task | request add-manager <task-ref> | request add-subordinate <manager-ref>")),
    }
    Ok(())
}

fn helper_status(home: &Path) -> io::Result<serde_json::Value> {
    // Read-only: never open the mutable Registry beside a running viewer. Only
    // expose navigation context, not runtime environment, provider IDs or audit.
    let data: serde_json::Value = match std::fs::read(home.join("state/state.json")) {
        Ok(bytes) => serde_json::from_slice(&bytes).map_err(io_err)?,
        Err(error) if error.kind() == io::ErrorKind::NotFound => serde_json::json!({}),
        Err(error) => return Err(error),
    };
    let tasks: Vec<_> = data["tasks"]
        .as_array()
        .into_iter()
        .flatten()
        .map(|task| {
            serde_json::json!({
                "ref": task["task_ref"], "name": task["name"],
                "server": task["server_profile_id"], "repository": task["repo"],
            })
        })
        .collect();
    let servers: Vec<_> = data["servers"]
        .as_array()
        .into_iter()
        .flatten()
        .map(|server| {
            serde_json::json!({
                "id": server["server_id"], "name": server["name"],
                "target": server["target"], "archived": server["archived"],
            })
        })
        .collect();
    let members: Vec<_> = data["sessions"]
        .as_array()
        .into_iter()
        .flatten()
        .map(|session| {
            serde_json::json!({
                "ref": session["session_ref"], "task": session["task_ref"],
                "name": session["name"], "role": session["role"], "parent": session["parent"],
                "harness": session["harness"], "state": session["state"],
            })
        })
        .collect();
    let helper = serde_json::json!({
        "task_ref": data["helper"]["task_ref"],
        "manager_ref": data["helper"]["manager_ref"],
        "preferred_harness": data["helper"]["preferred_harness"],
    });
    Ok(serde_json::json!({
        "helper": helper, "tasks": tasks, "servers": servers, "members": members,
        "note": "Recorded state, not a fresh runtime or login check. Use Settings to check readiness.",
    }))
}

fn session_cmd(args: &[String]) -> io::Result<()> {
    if args.first().map(String::as_str) == Some("cancel") {
        let Some(session_ref) = args.get(1) else {
            eprintln!("usage: taskai session cancel <ref>");
            std::process::exit(2);
        };
        let sock = control::sock_path(&taskai_home());
        return match control::client_cancel(&sock, session_ref) {
            Ok(resp) if resp["ok"] == serde_json::Value::Bool(true) => {
                println!("cancel transported to {session_ref}");
                Ok(())
            }
            Ok(resp) => {
                eprintln!("error: {}", resp["error"].as_str().unwrap_or("unknown"));
                std::process::exit(1);
            }
            Err(e) => {
                eprintln!("error: {e}");
                std::process::exit(1);
            }
        };
    }
    if args.first().map(String::as_str) == Some("launch") {
        let [_, session_ref] = args else {
            eprintln!("usage: taskai session launch <ref>");
            std::process::exit(2);
        };
        let home = taskai_home();
        return match launch::run_detached(&home, &app::private_server_name(&home), session_ref) {
            Ok(report) => {
                println!("{report}");
                Ok(())
            }
            Err(error) => {
                eprintln!("error: {error}");
                std::process::exit(1);
            }
        };
    }
    if args.first().map(String::as_str) != Some("prompt") {
        eprintln!("usage: taskai session prompt <ref> --stdin --wait [--format json]");
        std::process::exit(2);
    }
    let Some(session_ref) = args.get(1).filter(|a| !a.starts_with("--")) else {
        eprintln!("usage: taskai session prompt <ref> --stdin --wait [--format json]");
        std::process::exit(2);
    };
    let (mut use_stdin, mut wait, mut format_json) = (false, false, false);
    let mut i = 2;
    while i < args.len() {
        match args[i].as_str() {
            "--stdin" => use_stdin = true,
            "--wait" => wait = true,
            "--format" => {
                i += 1;
                format_json = args.get(i).map(String::as_str) == Some("json");
            }
            other => {
                eprintln!("unknown argument: {other}");
                std::process::exit(2);
            }
        }
        i += 1;
    }
    if !use_stdin {
        eprintln!("prompt text comes from stdin: pass --stdin");
        std::process::exit(2);
    }
    let mut text = String::new();
    io::stdin().read_to_string(&mut text)?;
    let sock = control::sock_path(&taskai_home());
    let source = std::env::var("TASKAI_SESSION_REF").ok();
    match control::client_prompt(&sock, session_ref, text.trim_end(), wait, source.as_deref()) {
        Ok(resp) => {
            let ok = resp["ok"] == serde_json::Value::Bool(true);
            if format_json {
                println!("{resp}");
            } else if ok {
                println!(
                    "completed · session {}",
                    resp["session_id"].as_str().unwrap_or("?")
                );
            } else {
                eprintln!("error: {}", resp["error"].as_str().unwrap_or("unknown"));
            }
            if !ok {
                std::process::exit(1);
            }
            Ok(())
        }
        Err(e) => {
            eprintln!("error: {e}");
            std::process::exit(1);
        }
    }
}

fn context_cmd(args: &[String]) -> io::Result<()> {
    let sub = args.first().map(String::as_str).unwrap_or("");
    let target = args.get(1).filter(|a| !a.starts_with("--")).cloned();
    let mut format_json = false;
    let mut i = 1;
    while i < args.len() {
        if args[i] == "--format" {
            i += 1;
            format_json = args.get(i).map(String::as_str) == Some("json");
        }
        i += 1;
    }
    let self_ref = std::env::var("TASKAI_SESSION_REF").ok();

    if sub == "inject" {
        // Hook path: emit fresh context for the hosting session; never fail the CLI.
        if let Some(r) = self_ref {
            if let Ok(reg) = Registry::open(&taskai_home().join("state")) {
                if let Ok(text) = taskai_context::text_view(&reg, &r) {
                    println!(
                        "{}",
                        serde_json::json!({"hookSpecificOutput": {
                            "hookEventName": "UserPromptSubmit",
                            "additionalContext": format!("TaskAI Context (revision {})\n{text}", reg.revision()),
                        }})
                    );
                }
            }
        }
        return Ok(());
    }

    let reg = Registry::open(&taskai_home().join("state")).map_err(io_err)?;
    let fail = |m: String| -> io::Result<()> {
        eprintln!("error: {m}");
        std::process::exit(1);
    };
    match sub {
        "self" => {
            let Some(r) = self_ref else {
                eprintln!("not inside a TaskAI-managed session (TASKAI_SESSION_REF unset)");
                std::process::exit(1);
            };
            let out = if format_json {
                taskai_context::snapshot(&reg, &r).map(|v| v.to_string())
            } else {
                taskai_context::text_view(&reg, &r)
            };
            match out {
                Ok(s) => println!("{s}"),
                Err(e) => return fail(e.to_string()),
            }
        }
        "session" => {
            let Some(r) = target else {
                eprintln!("usage: taskai context session <ref> [--format text|json]");
                std::process::exit(2);
            };
            let out = if format_json {
                taskai_context::snapshot(&reg, &r).map(|v| v.to_string())
            } else {
                taskai_context::session_view(&reg, &r)
            };
            match out {
                Ok(s) => println!("{s}"),
                Err(e) => return fail(e.to_string()),
            }
        }
        "tree" => {
            let scope = self_ref.and_then(|r| reg.session(&r).map(|s| s.task_ref.clone()));
            if format_json {
                let tasks: Vec<serde_json::Value> = reg
                    .tasks()
                    .iter()
                    .filter(|t| scope.as_deref().is_none_or(|s| s == t.task_ref))
                    .map(|t| {
                        serde_json::json!({
                            "ref": t.task_ref,
                            "name": t.name,
                            "repo": t.repo,
                            "sessions": reg.sessions_of(&t.task_ref).iter().map(|s| serde_json::json!({
                                "session_ref": s.session_ref,
                                "role": s.role.label(),
                                "name": s.name,
                                "harness": s.harness,
                                "parent": s.parent,
                                "workspace": s.workspace,
                                "state": s.state.label(),
                            })).collect::<Vec<_>>(),
                        })
                    })
                    .collect();
                println!(
                    "{}",
                    serde_json::json!({"schema_version": 1, "context_revision": reg.revision(), "tasks": tasks})
                );
            } else {
                println!("{}", taskai_context::tree_view(&reg, scope.as_deref()));
            }
        }
        _ => {
            eprintln!("usage: taskai context self|session <ref>|tree [--format text|json]");
            std::process::exit(2);
        }
    }
    Ok(())
}

#[cfg(test)]
mod helper_status_tests {
    use super::*;

    #[test]
    fn helper_status_never_creates_or_rewrites_registry_and_only_exports_context() {
        let home = tempfile::tempdir().unwrap();
        assert_eq!(
            helper_status(home.path()).unwrap()["tasks"],
            serde_json::json!([])
        );
        assert!(!home.path().join("state").exists());
        std::fs::create_dir(home.path().join("state")).unwrap();
        let path = home.path().join("state/state.json");
        let data = serde_json::json!({
            "helper": {"task_ref":"t1", "manager_ref":"s1", "preferred_harness":"codex"},
            "tasks": [{"task_ref":"t1", "name":"taskai-helper", "server_profile_id":"local", "repo":"/helper"}],
            "servers": [{"server_id":"local", "name":"This computer", "target":{"kind":"local"}, "archived":false,
                "harnesses":{"codex":{"executable":"private-executable", "model":"private-model"}}}],
            "sessions": [{"session_ref":"s1", "task_ref":"t1", "name":"TaskAI Helper", "role":"manager",
                "parent":null, "harness":"codex", "state":"queued", "provider":{"provider_session_id":"private-provider-id"}}],
            "audit":[{"private":"private-audit-value"}],
            "future_field":"private-future-value",
        });
        let bytes = serde_json::to_vec_pretty(&data).unwrap();
        std::fs::write(&path, &bytes).unwrap();
        let status = helper_status(home.path()).unwrap();
        assert_eq!(status["helper"]["manager_ref"], "s1");
        assert_eq!(status["members"][0]["role"], "manager");
        assert_eq!(status["servers"][0]["id"], "local");
        assert!(status["note"].as_str().unwrap().contains("not a fresh"));
        assert!(!status.to_string().contains("private-"));
        assert_eq!(std::fs::read(&path).unwrap(), bytes);
    }
}
