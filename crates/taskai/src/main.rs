// ABOUTME: taskai binary — TUI, `--attach-claude <dir>` live session host, `session prompt`
// ABOUTME: conduit client, and `--dump-frame` for tests and parity tooling.

mod control;

use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use crossterm::terminal;
use taskai_connectors::claude::{install_context_hook, install_hooks, ClaudeConnector};
use taskai_connectors::events::wait_for_event;
use taskai_connectors::{Conduit, Mode as ConduitMode};
use taskai_core::registry::{ProviderMapping, Registry, SessionState, Source};
use taskai_pane::Pane;
use taskai_tui::color::set_truecolor;
use taskai_tui::input::{route, Focus, Route};
use taskai_tui::render::{dump, render};
use taskai_tui::screens::pane as pane_screen;
use taskai_tui::state::{advance, Screen, St};

enum Mode {
    Board,
    Pane,
}

struct App {
    st: St,
    mode: Mode,
    focus: Focus,
    pane: Option<Arc<Mutex<Pane>>>,
    title: String,
}

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
        None => run_tui(attach),
    }
}

fn session_cmd(args: &[String]) -> io::Result<()> {
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
    match control::client_prompt(&sock, session_ref, text.trim_end(), wait) {
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

fn clock_now() -> String {
    chrono::Local::now().format("%H:%M:%S").to_string()
}

/// Registers the task and Lead session, installs hooks, launches the real Claude CLI in
/// a pane, watches for its SessionStart to bind the provider session, serves the socket.
fn setup_attach(dir: &Path, cols: usize, rows: usize) -> io::Result<(Arc<Mutex<Pane>>, String)> {
    let dir = dir.canonicalize()?;
    let home = taskai_home();
    let run = home.join("run");
    std::fs::create_dir_all(&run)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&run, std::fs::Permissions::from_mode(0o700))?;
    }
    let connector = ClaudeConnector::default();
    let cap = connector.probe();
    if cap.mode != ConduitMode::OwnedTui {
        return Err(io_err(format!("claude unavailable: {}", cap.detail)));
    }
    let events = install_hooks(&dir, &run).map_err(io_err)?;
    taskai_context::install_bootstrap(&dir).map_err(io_err)?;
    if let Ok(exe) = std::env::current_exe() {
        install_context_hook(&dir, &exe).map_err(io_err)?;
    }
    let mut registry = Registry::open(&home.join("state")).map_err(io_err)?;
    let name = dir.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_else(|| "task".into());
    let task = registry.create_task(&name, &dir, Source::HumanUi).map_err(io_err)?;
    let sref = registry
        .create_session(&task, None, "Lead", "claude", &dir, Source::HumanUi)
        .map_err(io_err)?;

    let (br, bc) = pane_screen::body_size(cols, rows);
    let (prog, cargs) = connector.launch_command();
    let argv: Vec<&str> = cargs.iter().map(String::as_str).collect();
    let home_str = home.display().to_string();
    let envs = [("TASKAI_SESSION_REF", sref.as_str()), ("TASKAI_HOME", home_str.as_str())];
    let pane = Arc::new(Mutex::new(
        Pane::spawn(&prog, &argv, &dir, br as u16, bc as u16, &envs).map_err(io_err)?,
    ));

    let session_id = Arc::new(Mutex::new(None::<String>));
    let offset = std::fs::metadata(&events).map(|m| m.len()).unwrap_or(0);
    {
        let events = events.clone();
        let sid = Arc::clone(&session_id);
        let sref2 = sref.clone();
        let version = cap.version.clone().unwrap_or_default();
        std::thread::spawn(move || {
            if let Ok(ev) = wait_for_event(&events, "SessionStart", None, offset, Duration::from_secs(300)) {
                if let Some(id) = ev.session_id {
                    *sid.lock().unwrap() = Some(id.clone());
                    let _ = registry.bind_provider(
                        &sref2,
                        ProviderMapping {
                            provider: "claude".into(),
                            provider_session_id: id,
                            runtime_id: format!("pid:{}", std::process::id()),
                            connector_version: version,
                        },
                    );
                    let _ = registry.set_state(&sref2, SessionState::Live);
                }
            }
        });
    }
    control::serve(
        control::sock_path(&home),
        control::Served {
            session_ref: sref.clone(),
            pane: Arc::clone(&pane),
            events,
            session_id,
            conduit: Arc::new(Conduit::new()),
        },
    )?;
    Ok((pane, format!("Lead · Claude Code · {sref}")))
}

fn run_tui(attach: Option<PathBuf>) -> io::Result<()> {
    let (c, r) = terminal::size().unwrap_or((120, 40));
    let (cols, rows) = ((c as usize).max(110), (r as usize).max(30));
    let mut app = App {
        st: St::new(),
        mode: Mode::Board,
        focus: Focus::Cli,
        pane: None,
        title: "Shell".into(),
    };
    if let Some(dir) = attach {
        let (pane, title) = setup_attach(&dir, cols, rows)?;
        app.pane = Some(pane);
        app.title = title;
        app.mode = Mode::Pane;
        app.st.screen = Screen::Home;
    }
    let mut out = io::stdout();
    terminal::enable_raw_mode()?;
    write!(out, "\x1b[?1049h\x1b[?25l\x1b[2J")?;
    out.flush()?;
    let result = event_loop(&mut out, &mut app);
    write!(out, "\x1b[?25h\x1b[?1049l\x1b[0m")?;
    out.flush()?;
    terminal::disable_raw_mode()?;
    result
}

fn event_loop(out: &mut impl Write, app: &mut App) -> io::Result<()> {
    loop {
        advance(&mut app.st);
        let (c, r) = terminal::size().unwrap_or((120, 40));
        let (cols, rows) = ((c as usize).max(110), (r as usize).max(30));
        let frame = match app.mode {
            Mode::Board => render(&app.st, cols, rows, &clock_now()),
            Mode::Pane => {
                let (br, bc) = pane_screen::body_size(cols, rows);
                let view: Vec<Vec<pane_screen::PaneCell>> = match &app.pane {
                    Some(p) => {
                        let mut p = p.lock().unwrap();
                        if p.size() != (br, bc) {
                            let _ = p.resize(br as u16, bc as u16);
                        }
                        p.cells()
                            .into_iter()
                            .map(|row| {
                                row.into_iter()
                                    .map(|c| pane_screen::PaneCell {
                                        text: c.text,
                                        fg: c.fg,
                                        bg: c.bg,
                                        bold: c.bold,
                                    })
                                    .collect()
                            })
                            .collect()
                    }
                    None => Vec::new(),
                };
                pane_screen::frame(&app.title, &view, app.focus, cols, rows, &clock_now())
            }
        };
        write!(out, "\x1b[?2026h\x1b[H{}\x1b[?2026l", frame.join("\r\n"))?;
        out.flush()?;

        let mut waited = false;
        while event::poll(if waited { Duration::ZERO } else { Duration::from_millis(100) })? {
            waited = true;
            if let Event::Key(k) = event::read()? {
                if k.kind == KeyEventKind::Release {
                    continue;
                }
                if !handle(app, k, cols, rows) {
                    return Ok(());
                }
            }
        }
    }
}

fn handle(app: &mut App, k: KeyEvent, cols: usize, rows: usize) -> bool {
    match app.mode {
        Mode::Board => {
            if k.code == KeyCode::Char('c') && k.modifiers.contains(KeyModifiers::CONTROL) {
                return false;
            }
            match app.st.screen {
                Screen::Boot => app.st.screen = Screen::Home,
                _ => match k.code {
                    KeyCode::Char('q') => return false,
                    KeyCode::Up => app.st.home_sel = app.st.home_sel.saturating_sub(1),
                    KeyCode::Down => app.st.home_sel = (app.st.home_sel + 1).min(3),
                    KeyCode::Enter => {
                        if app.pane.is_none() {
                            let (br, bc) = pane_screen::body_size(cols, rows);
                            let shell = std::env::var("SHELL").unwrap_or_else(|_| "/bin/sh".into());
                            let cwd = std::env::current_dir().unwrap_or_else(|_| "/".into());
                            if let Ok(p) = Pane::spawn(&shell, &[], &cwd, br as u16, bc as u16, &[]) {
                                app.pane = Some(Arc::new(Mutex::new(p)));
                            }
                        }
                        if app.pane.is_some() {
                            app.mode = Mode::Pane;
                            app.focus = Focus::Cli;
                        }
                    }
                    _ => {}
                },
            }
            true
        }
        Mode::Pane => {
            match route(app.focus, k) {
                Route::Boundary => {
                    app.focus = match app.focus {
                        Focus::Cli => Focus::Nav,
                        Focus::Nav => Focus::Cli,
                    }
                }
                Route::Forward(bytes) => {
                    if let Some(p) = &app.pane {
                        let _ = p.lock().unwrap().write_bytes(&bytes);
                    }
                }
                Route::Nav(ev) => match ev.code {
                    KeyCode::Enter => app.focus = Focus::Cli,
                    KeyCode::Char('q') => return false,
                    KeyCode::Char('T') => app.mode = Mode::Board,
                    _ => {}
                },
            }
            true
        }
    }
}
