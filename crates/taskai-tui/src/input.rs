// ABOUTME: Focus model and key routing — Ctrl+G is the boundary; CLI focus forwards
// ABOUTME: exact terminal bytes; navigation focus interprets TaskAI keys.

use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Focus {
    Nav,
    Cli,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Route {
    Boundary,
    Forward(Vec<u8>),
    Nav(KeyEvent),
}

/// Routes one key event: Ctrl+G is always the boundary; with CLI focus everything else
/// forwards as the exact bytes a terminal would send; with navigation focus TaskAI
/// interprets the key.
pub fn route(focus: Focus, ev: KeyEvent) -> Route {
    if ev.code == KeyCode::Char('g') && ev.modifiers.contains(KeyModifiers::CONTROL) {
        return Route::Boundary;
    }
    match focus {
        Focus::Nav => Route::Nav(ev),
        Focus::Cli => Route::Forward(key_bytes(ev).unwrap_or_default()),
    }
}

fn key_bytes(ev: KeyEvent) -> Option<Vec<u8>> {
    let ctrl = ev.modifiers.contains(KeyModifiers::CONTROL);
    let alt = ev.modifiers.contains(KeyModifiers::ALT);
    let mut out: Vec<u8> = Vec::new();
    if alt {
        out.push(0x1b);
    }
    match ev.code {
        KeyCode::Char(c) => {
            if ctrl {
                let lower = c.to_ascii_lowercase();
                if !lower.is_ascii_lowercase() {
                    return None;
                }
                out.push(lower as u8 - b'a' + 1);
            } else {
                let mut buf = [0u8; 4];
                out.extend_from_slice(c.encode_utf8(&mut buf).as_bytes());
            }
        }
        KeyCode::Enter => out.push(b'\r'),
        KeyCode::Backspace => out.push(0x7f),
        KeyCode::Tab => out.push(b'\t'),
        KeyCode::BackTab => out.extend_from_slice(b"\x1b[Z"),
        KeyCode::Esc => out.push(0x1b),
        KeyCode::Up => out.extend_from_slice(b"\x1b[A"),
        KeyCode::Down => out.extend_from_slice(b"\x1b[B"),
        KeyCode::Right => out.extend_from_slice(b"\x1b[C"),
        KeyCode::Left => out.extend_from_slice(b"\x1b[D"),
        KeyCode::Home => out.extend_from_slice(b"\x1b[H"),
        KeyCode::End => out.extend_from_slice(b"\x1b[F"),
        KeyCode::PageUp => out.extend_from_slice(b"\x1b[5~"),
        KeyCode::PageDown => out.extend_from_slice(b"\x1b[6~"),
        KeyCode::Delete => out.extend_from_slice(b"\x1b[3~"),
        KeyCode::Insert => out.extend_from_slice(b"\x1b[2~"),
        _ => return None,
    }
    Some(out)
}
