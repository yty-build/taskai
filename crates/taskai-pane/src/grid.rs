// ABOUTME: vt100-backed terminal grid — processes raw bytes and exposes resolved cells
// ABOUTME: styled cells plus cursor, paste, and mouse state for the enclosing TUI.

pub use vt100::{MouseProtocolEncoding, MouseProtocolMode};

pub struct Cell {
    pub text: String,
    pub fg: Option<(u8, u8, u8)>,
    pub bg: Option<(u8, u8, u8)>,
    pub bold: bool,
    pub italic: bool,
    pub underline: bool,
    pub inverse: bool,
    pub width: usize,
}

/// Cursor state reported by the child terminal. Coordinates are zero-based and
/// relative to the child's own grid.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct CursorState {
    pub row: usize,
    pub col: usize,
    pub visible: bool,
}

/// Mouse input requested by the actual child viewer, captured with its geometry.
/// TaskAI translates outer coordinates before encoding; tmux owns scrollback.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct MouseState {
    pub mode: MouseProtocolMode,
    pub encoding: MouseProtocolEncoding,
    pub rows: usize,
    pub cols: usize,
}

impl MouseState {
    /// Encode one event only when the child requested that kind of mouse input.
    ///
    /// Coordinates are zero-based. `button` is the xterm button code: left/middle/
    /// right = 0/1/2, wheel = 64..67, motion adds 32 (35 for unheld motion), and
    /// Shift/Alt/Ctrl add 4/8/16. A release names the original button, not code 3.
    /// Invalid, out-of-bounds, unsupported-mode, and wheel-release events are
    /// dropped; legacy coordinates are never truncated or wrapped.
    ///
    /// Protocol: https://invisible-island.net/xterm/ctlseqs/ctlseqs.html#h2-Mouse-Tracking
    pub fn encode(self, button: u8, row: usize, col: usize, released: bool) -> Option<Vec<u8>> {
        if row >= self.rows || col >= self.cols || button & 128 != 0 {
            return None;
        }
        let base = button & 3;
        let motion = button & 32 != 0;
        let wheel = button & 64 != 0;
        if (wheel && (motion || released))
            || (motion && released)
            || (!wheel && base == 3 && !motion)
        {
            return None;
        }
        let button = match self.mode {
            MouseProtocolMode::None => return None,
            MouseProtocolMode::Press => {
                // X10 mode reports only physical button presses, no modifiers.
                if released || motion || wheel {
                    return None;
                }
                base
            }
            MouseProtocolMode::PressRelease if motion => return None,
            MouseProtocolMode::ButtonMotion if motion && base == 3 => return None,
            _ => button,
        };
        if self.encoding == MouseProtocolEncoding::Sgr {
            return Some(
                format!(
                    "\x1b[<{button};{};{}{}",
                    col + 1,
                    row + 1,
                    if released { 'm' } else { 'M' },
                )
                .into_bytes(),
            );
        }
        // Legacy release events cannot identify the released button. Preserve
        // modifiers, but normalize the low button bits to the release code 3.
        let button = if released { (button & 28) | 3 } else { button };
        let x = col.checked_add(33)?;
        let y = row.checked_add(33)?;
        let mut encoded = b"\x1b[M".to_vec();
        match self.encoding {
            MouseProtocolEncoding::Default => {
                encoded.extend([button + 32, u8::try_from(x).ok()?, u8::try_from(y).ok()?]);
            }
            MouseProtocolEncoding::Utf8 => {
                // DECSET 1005 extends each coordinate to at most two UTF-8 bytes.
                if x > 2047 || y > 2047 {
                    return None;
                }
                for value in [usize::from(button) + 32, x, y] {
                    let mut bytes = [0; 4];
                    let codepoint = char::from_u32(value as u32)?;
                    encoded.extend_from_slice(codepoint.encode_utf8(&mut bytes).as_bytes());
                }
            }
            MouseProtocolEncoding::Sgr => unreachable!("SGR was handled above"),
        }
        Some(encoded)
    }
}

pub struct Grid {
    parser: vt100::Parser,
}

/// Temporary immutable copy, not another history/cache. vt100 cells keep their
/// codepoints inline, so cloning copies row buffers without allocating a String
/// per cell. Resolve those Strings only after releasing the live parser lock.
pub(crate) struct GridSnapshot {
    screen: vt100::Screen,
}

impl GridSnapshot {
    pub(crate) fn cells(&self) -> Vec<Vec<Cell>> {
        resolved_cells(&self.screen)
    }

    pub(crate) fn cursor(&self) -> CursorState {
        screen_cursor(&self.screen)
    }

    pub(crate) fn mouse(&self) -> MouseState {
        screen_mouse(&self.screen)
    }

    pub(crate) fn bracketed_paste(&self) -> bool {
        self.screen.bracketed_paste()
    }
}

impl Grid {
    pub fn new(rows: usize, cols: usize) -> Grid {
        Grid {
            parser: vt100::Parser::new(rows as u16, cols as u16, 0),
        }
    }

    pub fn process(&mut self, bytes: &[u8]) {
        self.parser.process(bytes);
    }

    pub fn set_size(&mut self, rows: usize, cols: usize) {
        self.parser.set_size(rows as u16, cols as u16);
    }

    pub fn size(&self) -> (usize, usize) {
        let (r, c) = self.parser.screen().size();
        (r as usize, c as usize)
    }

    pub fn cursor(&self) -> CursorState {
        screen_cursor(self.parser.screen())
    }

    /// Whether the child requested DECSET 2004 bracketed-paste input.
    pub fn bracketed_paste(&self) -> bool {
        self.parser.screen().bracketed_paste()
    }

    pub fn mouse(&self) -> MouseState {
        screen_mouse(self.parser.screen())
    }

    pub(crate) fn snapshot(&self) -> GridSnapshot {
        GridSnapshot {
            screen: self.parser.screen().clone(),
        }
    }

    /// Resolved cells row by row; wide glyphs occupy one Cell with width 2 and their
    /// continuation column is skipped.
    pub fn cells(&self) -> Vec<Vec<Cell>> {
        resolved_cells(self.parser.screen())
    }

    pub fn text(&self) -> String {
        self.parser.screen().contents()
    }
}

fn screen_cursor(screen: &vt100::Screen) -> CursorState {
    let (row, col) = screen.cursor_position();
    CursorState {
        row: row as usize,
        col: col as usize,
        visible: !screen.hide_cursor(),
    }
}

fn screen_mouse(screen: &vt100::Screen) -> MouseState {
    let (rows, cols) = screen.size();
    MouseState {
        mode: screen.mouse_protocol_mode(),
        encoding: screen.mouse_protocol_encoding(),
        rows: rows as usize,
        cols: cols as usize,
    }
}

fn resolved_cells(screen: &vt100::Screen) -> Vec<Vec<Cell>> {
    let (rows, cols) = screen.size();
    (0..rows)
        .map(|r| {
            let mut out = Vec::with_capacity(cols as usize);
            let mut c = 0;
            while c < cols {
                if let Some(cell) = screen.cell(r, c) {
                    if cell.is_wide_continuation() {
                        c += 1;
                        continue;
                    }
                    let mut text = cell.contents();
                    if text.is_empty() {
                        text = " ".into();
                    }
                    out.push(Cell {
                        text,
                        fg: color(cell.fgcolor()),
                        bg: color(cell.bgcolor()),
                        bold: cell.bold(),
                        italic: cell.italic(),
                        underline: cell.underline(),
                        inverse: cell.inverse(),
                        width: if cell.is_wide() { 2 } else { 1 },
                    });
                }
                c += 1;
            }
            out
        })
        .collect()
}

fn color(c: vt100::Color) -> Option<(u8, u8, u8)> {
    match c {
        vt100::Color::Default => None,
        vt100::Color::Idx(i) => Some(idx_rgb(i)),
        vt100::Color::Rgb(r, g, b) => Some((r, g, b)),
    }
}

const NAMED: [(u8, u8, u8); 16] = [
    (0, 0, 0),
    (205, 0, 0),
    (0, 205, 0),
    (205, 205, 0),
    (0, 0, 238),
    (205, 0, 205),
    (0, 205, 205),
    (229, 229, 229),
    (127, 127, 127),
    (255, 0, 0),
    (0, 255, 0),
    (255, 255, 0),
    (92, 92, 255),
    (255, 0, 255),
    (0, 255, 255),
    (255, 255, 255),
];

/// The xterm 256-color palette: 16 named, a 6x6x6 cube, then the grayscale ramp.
pub fn idx_rgb(i: u8) -> (u8, u8, u8) {
    match i {
        0..=15 => NAMED[i as usize],
        16..=231 => {
            let v = [0u8, 95, 135, 175, 215, 255];
            let n = i - 16;
            (
                v[(n / 36) as usize],
                v[((n % 36) / 6) as usize],
                v[(n % 6) as usize],
            )
        }
        _ => {
            let g = 8 + 10 * (i - 232);
            (g, g, g)
        }
    }
}

#[cfg(test)]
mod snapshot_tests {
    use super::*;
    use std::sync::Mutex;

    fn assert_cells_equal(left: &[Vec<Cell>], right: &[Vec<Cell>]) {
        assert_eq!(left.len(), right.len());
        for (left, right) in left.iter().zip(right) {
            assert_eq!(left.len(), right.len());
            for (left, right) in left.iter().zip(right) {
                assert_eq!(left.text, right.text);
                assert_eq!(left.fg, right.fg);
                assert_eq!(left.bg, right.bg);
                assert_eq!(left.bold, right.bold);
                assert_eq!(left.italic, right.italic);
                assert_eq!(left.underline, right.underline);
                assert_eq!(left.inverse, right.inverse);
                assert_eq!(left.width, right.width);
            }
        }
    }

    #[test]
    fn cloned_snapshot_keeps_cells_and_metadata_coherent_after_live_mutation() {
        let mut grid = Grid::new(12, 30);
        grid.process("PRIMARY\x1b[?1049h\x1b[38;2;19;73;211m\x1b[48;5;124m\x1b[1;3;4;7mA界e\u{301}\x1b[5;10H\x1b[?25l\x1b[?2004h\x1b[?1002h\x1b[?1006h".as_bytes());
        let expected = grid.cells();
        let cursor = grid.cursor();
        let mouse = grid.mouse();
        let locked = Mutex::new(grid);
        let snapshot = locked.lock().unwrap().snapshot();
        {
            let mut live = locked
                .try_lock()
                .expect("snapshot must not retain parser lock");
            live.process(
                b"\x1b[?1049l\x1b[?25h\x1b[?2004l\x1b[?1002l\x1b[?1006l\x1b[0m\x1b[2J\x1b[HCHANGED",
            );
            live.set_size(18, 60);
        }
        assert_cells_equal(&snapshot.cells(), &expected);
        assert_eq!(snapshot.cursor(), cursor);
        assert_eq!(snapshot.mouse(), mouse);
        assert!(snapshot.bracketed_paste());
        assert_eq!((snapshot.mouse().rows, snapshot.mouse().cols), (12, 30));
        let live = locked.lock().unwrap();
        assert_eq!(live.size(), (18, 60));
        assert!(!live.bracketed_paste());
        assert_eq!(live.mouse().mode, MouseProtocolMode::None);
    }

    #[test]
    fn snapshot_critical_section_profile() {
        if std::env::var_os("TASKAI_SNAPSHOT_PROFILE").is_none() {
            return;
        }
        use std::hint::black_box;
        use std::time::Instant;
        for (rows, cols) in [(40, 173), (60, 220)] {
            let mut grid = Grid::new(rows, cols);
            // Allocate both screens, as an attached tmux client does, and fill
            // the active one with representative styled text and blank cells.
            grid.process(b"PRIMARY\x1b[?1049h\x1b[38;2;124;214;255m");
            for row in 0..rows {
                grid.process(
                    format!(
                        "\x1b[{};1H{:width$}",
                        row + 1,
                        "snapshot profiling text",
                        width = cols / 2
                    )
                    .as_bytes(),
                );
            }
            let locked = Mutex::new(grid);
            let mut old_ns = Vec::new();
            let mut clone_ns = Vec::new();
            for _ in 0..200 {
                let started = Instant::now();
                let cells = locked.lock().unwrap().cells();
                old_ns.push(started.elapsed().as_nanos());
                black_box(&cells);
                drop(cells);
                let started = Instant::now();
                let snapshot = locked.lock().unwrap().snapshot();
                clone_ns.push(started.elapsed().as_nanos());
                // Still do the same export; it simply no longer blocks parsing.
                black_box(snapshot.cells());
            }
            old_ns.sort_unstable();
            clone_ns.sort_unstable();
            eprintln!("snapshot {cols}x{rows}: vt100Cell={}B; two-screen cell storage={}B; critical-section median old_export={}us clone={}us; p95 old={}us clone={}us", std::mem::size_of::<vt100::Cell>(), 2 * rows * cols * std::mem::size_of::<vt100::Cell>(), old_ns[100] / 1000, clone_ns[100] / 1000, old_ns[190] / 1000, clone_ns[190] / 1000);
        }
    }
}
