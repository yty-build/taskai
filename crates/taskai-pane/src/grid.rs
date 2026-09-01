// ABOUTME: vt100-backed terminal grid — processes raw bytes and exposes resolved cells
// ABOUTME: (glyph, rgb colors, bold) for blitting into the TUI pane.

pub struct Cell {
    pub text: String,
    pub fg: Option<(u8, u8, u8)>,
    pub bg: Option<(u8, u8, u8)>,
    pub bold: bool,
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

pub struct Grid {
    parser: vt100::Parser,
}

impl Grid {
    pub fn new(rows: usize, cols: usize) -> Grid {
        Grid { parser: vt100::Parser::new(rows as u16, cols as u16, 0) }
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
        let screen = self.parser.screen();
        let (row, col) = screen.cursor_position();
        CursorState {
            row: row as usize,
            col: col as usize,
            visible: !screen.hide_cursor(),
        }
    }

    /// Resolved cells row by row; wide glyphs occupy one Cell with width 2 and their
    /// continuation column is skipped.
    pub fn cells(&self) -> Vec<Vec<Cell>> {
        let screen = self.parser.screen();
        let (rows, cols) = screen.size();
        (0..rows)
            .map(|r| {
                let mut out = Vec::new();
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
                            width: if cell.is_wide() { 2 } else { 1 },
                        });
                    }
                    c += 1;
                }
                out
            })
            .collect()
    }

    pub fn text(&self) -> String {
        self.parser.screen().contents()
    }
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
            (v[(n / 36) as usize], v[((n % 36) / 6) as usize], v[(n % 6) as usize])
        }
        _ => {
            let g = 8 + 10 * (i - 232);
            (g, g, g)
        }
    }
}
