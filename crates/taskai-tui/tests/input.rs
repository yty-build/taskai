// ABOUTME: Focus-model tests — Ctrl+G is the one boundary chord; with CLI focus every
// ABOUTME: other key forwards as exact bytes; with navigation focus keys are interpreted.

use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use taskai_tui::input::{route, Focus, Route};

fn key(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::NONE)
}

fn ctrl(c: char) -> KeyEvent {
    KeyEvent::new(KeyCode::Char(c), KeyModifiers::CONTROL)
}

fn shift(code: KeyCode) -> KeyEvent {
    KeyEvent::new(code, KeyModifiers::SHIFT)
}

// Expectation: with CLI focus, Ctrl+G returns to navigation and nothing else does.
// Why: the vision reserves exactly one chord. Value: the boundary is provable.
#[test]
fn ctrl_g_is_the_only_boundary() {
    assert_eq!(route(Focus::Cli, ctrl('g')), Route::Boundary);
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
        (key(KeyCode::Char('T')), b"T"),
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
    assert_eq!(route(Focus::Nav, key(KeyCode::Char('a'))), Route::Nav(key(KeyCode::Char('a'))));
    assert_eq!(route(Focus::Nav, ctrl('g')), Route::Boundary);
    assert!(!matches!(route(Focus::Nav, key(KeyCode::Char('x'))), Route::Forward(_)));
}
