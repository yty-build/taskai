// ABOUTME: Pane host screen — the working pane canvas hosting a real CLI's grid,
// ABOUTME: with a header strip and the focus-dependent keybar.

use crate::chrome::{keybar, masthead, soft_bot, soft_top, PAD};
use crate::color::{Rgb, CLI_BG, FT, GLASS_HI, ION, TEXT};
use crate::fixture::{HUD_HOME, LAMP};
use crate::input::Focus;
use crate::seg::{row, s, sb, seglen, vrow, Seg};

pub struct PaneCell {
    pub text: String,
    pub fg: Option<Rgb>,
    pub bg: Option<Rgb>,
    pub bold: bool,
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
        Focus::Cli => vec![s("● ", Some(ION), Some(b)), s("CLI HAS FOCUS", Some(ION), Some(b))],
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
            Some(cr) => {
                let mut segs: Vec<Seg> = Vec::new();
                for c in cr {
                    match segs.last_mut() {
                        Some(last)
                            if last.fg == c.fg && last.bg == c.bg && last.bold == c.bold =>
                        {
                            last.text.push_str(&c.text)
                        }
                        _ => segs.push(Seg {
                            text: c.text.clone(),
                            fg: c.fg,
                            bg: c.bg,
                            bold: c.bold,
                        }),
                    }
                }
                lines.push(row(&segs, cols, CLI_BG, 0));
            }
            None => lines.push(row(&[], cols, CLI_BG, 0)),
        }
    }
    lines.push(soft_bot(CLI_BG, cols));

    let keys: &[(&str, &str)] = match focus {
        Focus::Cli => &[("Ctrl+G", "TaskAI navigation")],
        Focus::Nav => &[("Enter", "Focus CLI"), ("T", "Home"), ("q", "Quit")],
    };
    lines.extend(keybar(cols, keys));
    lines.push(vrow(cols));
    while lines.len() < rows {
        lines.push(vrow(cols));
    }
    lines.truncate(rows);
    lines
}
