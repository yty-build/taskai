// ABOUTME: Focus model and key routing — Ctrl+G returns to navigation, Ctrl+F changes
// ABOUTME: the focused CLI layout, and every other CLI key forwards as terminal bytes.

use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Focus {
    Nav,
    Cli,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Route {
    Boundary,
    Layout,
    Forward(Vec<u8>),
    Nav(KeyEvent),
}

/// Routes one key event: Ctrl+G is the navigation boundary and Ctrl+F changes the CLI
/// layout. With CLI focus, every other key forwards as the exact bytes a terminal would
/// send; with navigation focus TaskAI interprets the key.
pub fn route(focus: Focus, ev: KeyEvent) -> Route {
    if focus == Focus::Cli
        && ev.modifiers.contains(KeyModifiers::CONTROL)
        && !ev.modifiers.contains(KeyModifiers::ALT)
    {
        match ev.code {
            KeyCode::Char('g' | 'G') => return Route::Boundary,
            KeyCode::Char('f' | 'F') => return Route::Layout,
            _ => {}
        }
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
