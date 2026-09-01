// ABOUTME: taskai binary — opens the TUI; `taskai --dump-frame <screen> [--cols N --rows N]`
// ABOUTME: prints one deterministic frame for tests and parity tooling.

use std::io::{self, Write};
use std::time::Duration;

use crossterm::event::{self, Event, KeyCode, KeyEventKind, KeyModifiers};
use crossterm::terminal;
use taskai_tui::color::set_truecolor;
use taskai_tui::render::{dump, render};
use taskai_tui::state::{advance, Screen, St};

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
    let mut st = St::new();
    loop {
        advance(&mut st);
        let (c, r) = terminal::size().unwrap_or((120, 40));
        let (cols, rows) = ((c as usize).max(110), (r as usize).max(30));
        let frame = render(&st, cols, rows, &clock_now());
        write!(out, "\x1b[?2026h\x1b[H{}\x1b[?2026l", frame.join("\r\n"))?;
        out.flush()?;
        if event::poll(Duration::from_millis(100))? {
            if let Event::Key(k) = event::read()? {
                if k.kind == KeyEventKind::Release {
                    continue;
                }
                if !handle(&mut st, k.code, k.modifiers) {
                    return Ok(());
                }
            }
        }
    }
}

fn handle(st: &mut St, code: KeyCode, mods: KeyModifiers) -> bool {
    if code == KeyCode::Char('c') && mods.contains(KeyModifiers::CONTROL) {
        return false;
    }
    match st.screen {
        Screen::Boot => {
            st.screen = Screen::Home;
            true
        }
        Screen::Home => match code {
            KeyCode::Char('q') => false,
            KeyCode::Up => {
                st.home_sel = st.home_sel.saturating_sub(1);
                true
            }
            KeyCode::Down => {
                st.home_sel = (st.home_sel + 1).min(3);
                true
            }
            _ => true,
        },
    }
}
