// ABOUTME: Focus-model tests — Ctrl+G owns navigation, Ctrl+F owns layout, and every
// ABOUTME: other focused key forwards as exact bytes to the native CLI.

use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use taskai_tui::input::{route, Focus, Route};

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

fn ctrl(c: char) -> KeyEvent {
    KeyEvent::new(KeyCode::Char(c), KeyModifiers::CONTROL)
}

fn ctrl_alt(c: char) -> KeyEvent {
    KeyEvent::new(KeyCode::Char(c), KeyModifiers::CONTROL | KeyModifiers::ALT)
}

fn shift(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::SHIFT)
}

// Expectation: with CLI focus, Ctrl+G returns to navigation and Ctrl+F changes layout.
// Why: each reserved chord has exactly one job. Value: the boundary is provable.
#[test]
fn focus_chords_have_distinct_routes() {
    assert_eq!(route(Focus::Cli, ctrl('g')), Route::Boundary);
    assert_eq!(route(Focus::Cli, ctrl('f')), Route::Layout);
    assert_eq!(route(Focus::Cli, ctrl('G')), Route::Boundary);
    assert_eq!(route(Focus::Cli, ctrl('F')), Route::Layout);
    assert_ne!(route(Focus::Cli, ctrl('c')), Route::Boundary);
    assert_ne!(route(Focus::Cli, key(KeyCode::Esc)), Route::Boundary);
    assert_ne!(route(Focus::Cli, key(KeyCode::Char('g'))), Route::Boundary);
}

// Expectation: with CLI focus, every other key forwards as the exact bytes a terminal
// would send — single-letter TaskAI commands included. Why: typing `a`, `m`, `d`, or
// prose inside a CLI can never trigger TaskAI actions. Value: passthrough is verbatim.
#[test]
fn cli_focus_forwards_exact_bytes() {
    let cases: &[(KeyEvent, &[u8])] = &[
        (key(KeyCode::Char('a')), b"a"),
        (key(KeyCode::Char('m')), b"m"),
        (key(KeyCode::Char('d')), b"d"),
        (key(KeyCode::Char('q')), b"q"),
        (key(KeyCode::Char('f')), b"f"),
        (key(KeyCode::Char('g')), b"g"),
        (key(KeyCode::Char('G')), b"G"),
        (key(KeyCode::Enter), b"\r"),
        (key(KeyCode::Backspace), b"\x7f"),
        (key(KeyCode::Tab), b"\t"),
        (key(KeyCode::Esc), b"\x1b"),
        (key(KeyCode::Up), b"\x1b[A"),
        (key(KeyCode::Down), b"\x1b[B"),
        (key(KeyCode::Right), b"\x1b[C"),
        (key(KeyCode::Left), b"\x1b[D"),
        (key(KeyCode::Home), b"\x1b[H"),
        (key(KeyCode::End), b"\x1b[F"),
        (shift(KeyCode::BackTab), b"\x1b[Z"),
        (ctrl('c'), b"\x03"),
        (ctrl('d'), b"\x04"),
        (ctrl('r'), b"\x12"),
        (ctrl_alt('f'), b"\x1b\x06"),
        (ctrl_alt('g'), b"\x1b\x07"),
    ];
    for (ev, bytes) in cases {
        assert_eq!(
            route(Focus::Cli, *ev),
            Route::Forward(bytes.to_vec()),
            "key {ev:?}"
        );
    }
}

// Expectation: with navigation focus, keys are interpreted by TaskAI, never forwarded.
// Why: single-letter commands only work while navigation has focus. Value: the two
// focus worlds cannot bleed into each other.
#[test]
fn nav_focus_interprets() {
    assert_eq!(
        route(Focus::Nav, key(KeyCode::Char('a'))),
        Route::Nav(key(KeyCode::Char('a')))
    );
    assert_eq!(route(Focus::Nav, ctrl('g')), Route::Nav(ctrl('g')));
    assert_eq!(route(Focus::Nav, ctrl('f')), Route::Nav(ctrl('f')));
    assert!(!matches!(
        route(Focus::Nav, key(KeyCode::Char('x'))),
        Route::Forward(_)
    ));
}
