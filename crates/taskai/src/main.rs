// ABOUTME: taskai binary — opens the TUI; `taskai --dump-frame <screen> [--cols N --rows N]`
// ABOUTME: prints one deterministic frame for tests and parity tooling.

use std::io::{self, Write};
use std::time::Duration;

use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use crossterm::terminal;
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
    pane: Option<Pane>,
}

fn main() -> io::Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mut screen: Option<String> = None;
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
        None => run_tui(),
    }
}

fn clock_now() -> String {
    chrono::Local::now().format("%H:%M:%S").to_string()
}

fn run_tui() -> io::Result<()> {
    let mut out = io::stdout();
    terminal::enable_raw_mode()?;
    write!(out, "\x1b[?1049h\x1b[?25l\x1b[2J")?;
    out.flush()?;
    let result = event_loop(&mut out);
    write!(out, "\x1b[?25h\x1b[?1049l\x1b[0m")?;
    out.flush()?;
    terminal::disable_raw_mode()?;
    result
}

fn event_loop(out: &mut impl Write) -> io::Result<()> {
    let mut app = App { st: St::new(), mode: Mode::Board, focus: Focus::Cli, pane: None };
    loop {
        advance(&mut app.st);
        let (c, r) = terminal::size().unwrap_or((120, 40));
        let (cols, rows) = ((c as usize).max(110), (r as usize).max(30));
        let frame = match app.mode {
            Mode::Board => render(&app.st, cols, rows, &clock_now()),
            Mode::Pane => {
                let (br, bc) = pane_screen::body_size(cols, rows);
                if let Some(p) = &mut app.pane {
                    if p.size() != (br, bc) {
                        let _ = p.resize(br as u16, bc as u16);
                    }
                }
                let view: Vec<Vec<pane_screen::PaneCell>> = app
                    .pane
                    .as_ref()
                    .map(|p| {
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
                    })
                    .unwrap_or_default();
                pane_screen::frame("Shell", &view, app.focus, cols, rows, &clock_now())
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
                if !handle(&mut app, k, cols, rows) {
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
                Screen::Home => match k.code {
                    KeyCode::Char('q') => return false,
                    KeyCode::Up => app.st.home_sel = app.st.home_sel.saturating_sub(1),
                    KeyCode::Down => app.st.home_sel = (app.st.home_sel + 1).min(3),
                    KeyCode::Enter => {
                        if app.pane.is_none() {
                            let (br, bc) = pane_screen::body_size(cols, rows);
                            let shell = std::env::var("SHELL").unwrap_or_else(|_| "/bin/sh".into());
                            let cwd = std::env::current_dir().unwrap_or_else(|_| "/".into());
                            if let Ok(p) = Pane::spawn(&shell, &[], &cwd, br as u16, bc as u16) {
                                app.pane = Some(p);
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
                    if let Some(p) = &mut app.pane {
                        let _ = p.write_bytes(&bytes);
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
