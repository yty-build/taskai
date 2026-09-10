// ABOUTME: Segment/row engine — compose styled segments into width-exact painted ANSI rows.
// ABOUTME: Widths use terminal cells so user names, repository paths and overlays can contain Unicode.

use crate::color::{bg, fg, Rgb, BOLD, RESET, VOID};
use unicode_width::UnicodeWidthChar;

#[derive(Clone, Debug)]
pub struct Seg {
    pub text: String,
    pub fg: Option<Rgb>,
    pub bg: Option<Rgb>,
    pub bold: bool,
}

pub fn s(text: impl Into<String>, fg: Option<Rgb>, bg: Option<Rgb>) -> Seg {
    Seg {
        text: text.into(),
        fg,
        bg,
        bold: false,
    }
}

pub fn sb(text: impl Into<String>, fg: Option<Rgb>, bg: Option<Rgb>) -> Seg {
    Seg {
        text: text.into(),
        fg,
        bg,
        bold: true,
    }
}

pub fn text_width(t: &str) -> usize {
    t.chars().map(char_width).sum()
}

pub fn char_width(ch: char) -> usize {
    if ch.is_control() {
        1
    } else {
        ch.width().unwrap_or(1)
    }
}

pub fn take_cells(t: &str, n: usize) -> String {
    let mut used = 0;
    t.chars()
        .take_while(|ch| {
            used += char_width(*ch);
            used <= n
        })
        .map(|ch| if ch.is_control() { '�' } else { ch })
        .collect()
}

pub fn seglen(segs: &[Seg]) -> usize {
    segs.iter().map(|g| text_width(&g.text)).sum()
}

/// Compose one full line: pad to width, paint base_bg under everything.
pub fn row(segs: &[Seg], width: usize, base_bg: Rgb, indent: usize) -> String {
    let mut out = String::new();
    let mut used = 0usize;
    let ind = indent.min(width);
    if ind > 0 {
        out.push_str(&bg(base_bg));
        out.push_str(&" ".repeat(ind));
        used += ind;
    }
    for g in segs {
        if used >= width {
            break;
        }
        // User-provided text is data, never terminal escape sequences.
        let t = take_cells(&g.text, width - used);
        out.push_str(&bg(g.bg.unwrap_or(base_bg)));
        if let Some(f) = g.fg {
            out.push_str(&fg(f));
        }
        if g.bold {
            out.push_str(BOLD);
        }
        out.push_str(&t);
        out.push_str(RESET);
        used += text_width(&t);
    }
    if used < width {
        out.push_str(&bg(base_bg));
        out.push_str(&" ".repeat(width - used));
    }
    out.push_str(RESET);
    out
}

/// Truncate a seg list to width cells, appending an ellipsis on overflow.
pub fn clip_segs(segs: &[Seg], width: usize) -> Vec<Seg> {
    if seglen(segs) <= width {
        return segs.to_vec();
    }
    if width == 0 {
        return Vec::new();
    }
    let mut out = Vec::new();
    let mut used = 0usize;
    for g in segs {
        let mut t = take_cells(&g.text, width.saturating_sub(used + 1));
        let clipped = text_width(&g.text) > text_width(&t);
        used += text_width(&t);
        if clipped || used == width - 1 {
            t.push('…');
        }
        out.push(Seg {
            text: t,
            ..g.clone()
        });
        if clipped || used == width - 1 {
            break;
        }
    }
    out
}

/// A pure-void row: the vertical rhythm unit between slabs.
pub fn vrow(width: usize) -> String {
    row(&[], width, VOID, 0)
}

pub fn strip_sgr(line: &str) -> String {
    let b: Vec<char> = line.chars().collect();
    let mut out = String::new();
    let mut i = 0;
    while i < b.len() {
        if b[i] == '\u{1b}' && i + 1 < b.len() && b[i + 1] == '[' {
            let mut j = i + 2;
            while j < b.len() && (b[j].is_ascii_digit() || b[j] == ';') {
                j += 1;
            }
            if j < b.len() && b[j] == 'm' {
                i = j + 1;
                continue;
            }
        }
        out.push(b[i]);
        i += 1;
    }
    out
}

pub fn cell_count(line: &str) -> usize {
    text_width(&strip_sgr(line))
}
