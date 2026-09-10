// ABOUTME: Pane host screen — the working pane canvas hosting a real CLI's grid,
// ABOUTME: with a header strip and the focus-dependent keybar.

use crate::chrome::{keybar, masthead, soft_bot, soft_top, PAD};
use crate::color::{bg, fg, Rgb, BOLD, CLI_BG, FT, GLASS_HI, ION, RESET, TEXT};
use crate::fixture::{HUD_HOME, LAMP};
use crate::input::Focus;
use crate::seg::{row, s, sb, seglen, vrow, Seg};

pub struct PaneCell {
    pub text: String,
    pub fg: Option<Rgb>,
    pub bg: Option<Rgb>,
    pub bold: bool,
    pub italic: bool,
    pub underline: bool,
    pub inverse: bool,
    pub width: usize,
}

/// Paint terminal cells using the physical width reported by the child parser.
/// A character count is insufficient here: emoji and CJK glyphs occupy two
/// columns, while combining sequences can contain multiple chars in one cell.
pub fn cell_row(cells: &[PaneCell], width: usize, base: Rgb) -> String {
    let mut out = String::with_capacity(width.saturating_add(48));
    let mut used = 0usize;
    let mut previous_style = None;
    for cell in cells {
        let cell_width = cell.width.max(1);
        if used >= width || cell_width > width - used {
            break;
        }
        let style = (
            cell.fg,
            cell.bg.unwrap_or(base),
            cell.bold,
            cell.italic,
            cell.underline,
            cell.inverse,
        );
        if previous_style != Some(style) {
            // A row is self-contained, and a changed style fully resets omitted
            // attributes/default foreground. Equal neighboring cells share one
            // prefix instead of repainting color and reset codes per glyph.
            out.push_str(RESET);
            out.push_str(&bg(style.1));
            if let Some(color) = cell.fg {
                out.push_str(&fg(color));
            }
            if cell.bold {
                out.push_str(BOLD);
            }
            if cell.italic {
                out.push_str("\x1b[3m");
            }
            if cell.underline {
                out.push_str("\x1b[4m");
            }
            if cell.inverse {
                out.push_str("\x1b[7m");
            }
            previous_style = Some(style);
        }
        out.push_str(&cell.text);
        used += cell_width;
    }
    if used < width {
        out.push_str(RESET);
        out.push_str(&bg(base));
        out.push_str(&" ".repeat(width - used));
    }
    out.push_str(RESET);
    out
}

/// The grid size the hosted CLI should run at for a terminal of (cols, rows).
pub fn body_size(cols: usize, rows: usize) -> (usize, usize) {
    (rows.saturating_sub(9), cols)
}

pub fn frame(
    title: &str,
    cells: &[Vec<PaneCell>],
    focus: Focus,
    cols: usize,
    rows: usize,
    clock: &str,
) -> Vec<String> {
    let mut lines = masthead(cols, "TASK · SHELL", HUD_HOME, Some(LAMP), clock);
    lines.push(vrow(cols));

    let b = GLASS_HI;
    let head = vec![sb(title, Some(TEXT), Some(b))];
    let tail: Vec<Seg> = match focus {
        Focus::Cli => vec![
            s("● ", Some(ION), Some(b)),
            s("CLI HAS FOCUS", Some(ION), Some(b)),
        ],
        Focus::Nav => vec![s("NAVIGATION", Some(FT), Some(b))],
    };
    let gap = (cols as isize - 2 * PAD as isize - seglen(&head) as isize - seglen(&tail) as isize)
        .max(1) as usize;
    let mut hrow = head;
    hrow.push(s(" ".repeat(gap), None, Some(b)));
    hrow.extend(tail);
    lines.push(soft_top(b, cols));
    lines.push(row(&hrow, cols, b, PAD));

    let (body_rows, _) = body_size(cols, rows);
    for r in 0..body_rows {
        match cells.get(r) {
            Some(cr) => lines.push(cell_row(cr, cols, CLI_BG)),
            None => lines.push(row(&[], cols, CLI_BG, 0)),
        }
    }
    lines.push(soft_bot(CLI_BG, cols));

    let keys: &[(&str, &str)] = match focus {
        Focus::Cli => &[("Ctrl+F", "Change layout"), ("Ctrl+G", "Navigation")],
        Focus::Nav => &[("Ctrl+F", "Full screen"), ("G", "Home"), ("q", "Leave")],
    };
    lines.extend(keybar(cols, keys));
    lines.push(vrow(cols));
    while lines.len() < rows {
        lines.push(vrow(cols));
    }
    lines.truncate(rows);
    lines
}
