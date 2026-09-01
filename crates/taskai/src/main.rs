// ABOUTME: taskai binary — the live TUI, `session prompt` conduit client, `context`
// ABOUTME: service CLI, and `--dump-frame` for tests and parity tooling.

mod app;

use taskai_connectors::control;

use std::io::{self, Read};
use std::path::PathBuf;

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
    if args.first().map(String::as_str) == Some("session") {
        return session_cmd(&args[1..]);
    }
    if args.first().map(String::as_str) == Some("context") {
        return context_cmd(&args[1..]);
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
                println!("completed · session {}", resp["session_id"].as_str().unwrap_or("?"));
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
