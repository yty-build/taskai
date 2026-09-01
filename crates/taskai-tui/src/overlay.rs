// ABOUTME: Overlay compositor and floating panels — dims the rendered backdrop with an
// ABOUTME: SGR-preserving cell slicer and splices drawers, destroy, and help over it.

use crate::color::{
    bg, dull, DIM, FT, GLASS, GLASS_HI, GLASS_IN, ION, KCAP, RED, RED_GLASS, TEXT, VOID, RESET,
};
use crate::fixture::{
    Drawer, DESTROY_BLAST, DESTROY_CONFIRM, DESTROY_KEPT, DESTROY_TITLE, HELP_BOUNDARY,
    HELP_CONDUITS, HELP_GLOBAL, HELP_TASK, NEWTASK, NEWTASK_NOTE, SUBORDINATE, SUBORDINATE_NOTE,
    SUBORDINATE_PREVIEW,
};
use crate::seg::{row, s, sb, seglen, strip_sgr, Seg};

/// Regex-free port of the prototype's dim_line: every truecolor SGR component
/// (38;2;r;g;b / 48;2;r;g;b) is pulled toward the void.
pub fn dim_line(line: &str, k: f64) -> String {
    let chars: Vec<char> = line.chars().collect();
    let mut out = String::new();
    let mut i = 0;
    while i < chars.len() {
        if chars[i] == '\u{1b}' && i + 1 < chars.len() && chars[i + 1] == '[' {
            let mut j = i + 2;
            while j < chars.len() && (chars[j].is_ascii_digit() || chars[j] == ';') {
                j += 1;
            }
            if j < chars.len() && chars[j] == 'm' {
                let params: String = chars[i + 2..j].iter().collect();
                let toks: Vec<&str> = params.split(';').collect();
                let mut rebuilt: Vec<String> = Vec::new();
                let mut t = 0;
                while t < toks.len() {
                    if (toks[t] == "38" || toks[t] == "48")
                        && t + 4 < toks.len()
                        && toks[t + 1] == "2"
                    {
                        let rgb: Option<(u8, u8, u8)> = match (
                            toks[t + 2].parse(),
                            toks[t + 3].parse(),
                            toks[t + 4].parse(),
                        ) {
                            (Ok(r), Ok(g), Ok(b)) => Some((r, g, b)),
                            _ => None,
                        };
                        if let Some(rgb) = rgb {
                            let d = dull(rgb, k);
                            rebuilt.push(toks[t].into());
                            rebuilt.push("2".into());
                            rebuilt.push(d.0.to_string());
                            rebuilt.push(d.1.to_string());
                            rebuilt.push(d.2.to_string());
                            t += 5;
                            continue;
                        }
                    }
                    rebuilt.push(toks[t].into());
                    t += 1;
                }
                out.push_str("\u{1b}[");
                out.push_str(&rebuilt.join(";"));
                out.push('m');
                i = j + 1;
                continue;
            }
        }
        out.push(chars[i]);
        i += 1;
    }
    out
}

/// Cell-accurate slice of a rendered SGR line, preserving color state.
pub fn slice_cells(line: &str, start: usize, width: usize) -> String {
    let chars: Vec<char> = line.chars().collect();
    let mut out = String::new();
    let mut state = String::new();
    let mut col = 0usize;
    let mut emitting = false;
    let mut i = 0;
    while i < chars.len() {
        if chars[i] == '\u{1b}' && i + 1 < chars.len() && chars[i + 1] == '[' {
            let mut j = i + 2;
            while j < chars.len() && (chars[j].is_ascii_digit() || chars[j] == ';') {
                j += 1;
            }
            if j < chars.len() && chars[j] == 'm' {
                let code: String = chars[i..=j].iter().collect();
                if code == RESET {
                    state.clear();
                } else {
                    state.push_str(&code);
                }
                if emitting {
                    out.push_str(&code);
                }
                i = j + 1;
                continue;
            }
        }
        if col == start && !emitting {
            emitting = true;
            out.push_str(&state);
        }
        if col >= start + width {
            break;
        }
        if emitting {
            out.push(chars[i]);
        }
        col += 1;
        i += 1;
    }
    let got = if emitting { col.saturating_sub(start) } else { 0 };
    if got < width {
        out.push_str(&bg(VOID));
        out.push_str(&" ".repeat(width - got));
    }
    out.push_str(RESET);
    out
}

pub fn panel_width(panel: &[String]) -> usize {
    panel.first().map(|l| strip_sgr(l).chars().count()).unwrap_or(0)
}

/// Dim the base and splice a floating panel over it, starting at `top`.
pub fn overlay(base: &[String], panel: &[String], cols: usize, top: usize) -> Vec<String> {
    let pw = panel_width(panel);
    let ind = cols.saturating_sub(pw) / 2;
    let mut out: Vec<String> = base.iter().map(|l| dim_line(l, 0.42)).collect();
    for (i, prow) in panel.iter().enumerate() {
        let r = top + i;
        if r >= out.len() {
            break;
        }
        out[r] = format!(
            "{}{}{}",
            slice_cells(&out[r], 0, ind),
            prow,
            slice_cells(&out[r], ind + pw, cols - ind - pw)
        );
    }
    out
}

fn keys_row(keys: &[(&str, &str)], pw: usize) -> String {
    let mut ks: Vec<Seg> = Vec::new();
    for (i, (k, l)) in keys.iter().enumerate() {
        if i > 0 {
            ks.push(s("   ", None, Some(GLASS)));
        }
        ks.push(s(format!(" {k} "), Some(TEXT), Some(KCAP)));
        ks.push(s(format!(" {l}"), Some(FT), Some(GLASS)));
    }
    row(&ks, pw, GLASS, 2)
}

pub fn drawer_panel(blink: bool, drawer: &Drawer, note: &[Vec<Seg>], pw: usize) -> Vec<String> {
    let b = GLASS_HI;
    let mut lines = vec![row(&[s("▄".repeat(pw), Some(b), Some(VOID))], pw, VOID, 0)];
    lines.push(row(&[sb(drawer.title, Some(TEXT), Some(b))], pw, b, 2));
    lines.push(row(&[], pw, GLASS, 0));
    for (label, value, focus) in drawer.fields {
        lines.push(row(&[s(label.to_uppercase(), Some(FT), Some(GLASS))], pw, GLASS, 2));
        let mut val = vec![
            s(" ", None, Some(GLASS_IN)),
            s(*value, Some(if *focus { TEXT } else { DIM }), Some(GLASS_IN)),
        ];
        if *focus {
            val.push(s(if blink { "▍" } else { " " }, Some(ION), Some(GLASS_IN)));
        }
        val.push(s(
            " ".repeat((pw as isize - 4 - seglen(&val) as isize).max(0) as usize),
            None,
            Some(GLASS_IN),
        ));
        lines.push(row(&val, pw, GLASS, 2));
        lines.push(row(&[], pw, GLASS, 0));
    }
    for segs in note {
        lines.push(row(segs, pw, GLASS, 2));
    }
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(keys_row(drawer.keys, pw));
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(&[s("▀".repeat(pw), Some(GLASS), Some(VOID))], pw, VOID, 0));
    lines
}

fn clip54(t: &str) -> String {
    t.chars().take(54).collect()
}

pub fn newtask_panel(blink: bool) -> Vec<String> {
    let note = vec![vec![s(clip54(NEWTASK_NOTE), Some(FT), Some(GLASS))]];
    drawer_panel(blink, &NEWTASK, &note, 58)
}

pub fn sub_panel(blink: bool) -> Vec<String> {
    let note = vec![
        vec![s("CODE WILL LIVE AT", Some(FT), Some(GLASS))],
        vec![s(SUBORDINATE_PREVIEW, Some(DIM), Some(GLASS))],
        vec![s("", None, Some(GLASS))],
        vec![s(clip54(SUBORDINATE_NOTE), Some(FT), Some(GLASS))],
    ];
    drawer_panel(blink, &SUBORDINATE, &note, 58)
}

pub fn destroy_panel(blink: bool) -> Vec<String> {
    let pw = 62;
    let mut lines = vec![row(&[s("▄".repeat(pw), Some(RED_GLASS), Some(VOID))], pw, VOID, 0)];
    lines.push(row(&[sb(DESTROY_TITLE, Some(RED), Some(RED_GLASS))], pw, RED_GLASS, 2));
    lines.push(row(&[], pw, GLASS, 0));
    for b in DESTROY_BLAST {
        lines.push(row(
            &[s("● ", Some(RED), Some(GLASS)), s(*b, Some(TEXT), Some(GLASS))],
            pw,
            GLASS,
            2,
        ));
    }
    lines.push(row(&[], pw, GLASS, 0));
    for k in DESTROY_KEPT {
        lines.push(row(
            &[s("✓ ", Some(FT), Some(GLASS)), s(*k, Some(DIM), Some(GLASS))],
            pw,
            GLASS,
            2,
        ));
    }
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[
            s("Type ", Some(FT), Some(GLASS)),
            sb(DESTROY_CONFIRM, Some(TEXT), Some(GLASS)),
            s(" to confirm", Some(FT), Some(GLASS)),
        ],
        pw,
        GLASS,
        2,
    ));
    let cur = if blink { "▍" } else { " " };
    let mut val = vec![
        s(format!(" {DESTROY_CONFIRM}"), Some(TEXT), Some(GLASS_IN)),
        s(cur, Some(ION), Some(GLASS_IN)),
    ];
    val.push(s(
        " ".repeat((pw as isize - 4 - seglen(&val) as isize).max(0) as usize),
        None,
        Some(GLASS_IN),
    ));
    lines.push(row(&val, pw, GLASS, 2));
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[
            s(" Enter ", Some(TEXT), Some(KCAP)),
            s(" Destroy", Some(RED), Some(GLASS)),
            s("    ", None, Some(GLASS)),
            s(" Esc ", Some(TEXT), Some(KCAP)),
            s(" Cancel", Some(FT), Some(GLASS)),
        ],
        pw,
        GLASS,
        2,
    ));
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(&[s("▀".repeat(pw), Some(GLASS), Some(VOID))], pw, VOID, 0));
    lines
}

pub fn help_panel() -> Vec<String> {
    let pw = 68;
    let b = GLASS_HI;
    let mut lines = vec![row(&[s("▄".repeat(pw), Some(b), Some(VOID))], pw, VOID, 0)];
    lines.push(row(&[sb("Help", Some(TEXT), Some(b))], pw, b, 2));
    lines.push(row(&[], pw, GLASS, 0));
    let (key, desc) = HELP_BOUNDARY;
    let head: String = desc.chars().take(56).collect();
    let tail: String = desc.chars().skip(56).take(60).collect();
    lines.push(row(
        &[
            sb(format!(" {key} "), Some(TEXT), Some(GLASS_IN)),
            s(format!("  {head}"), Some(DIM), Some(GLASS)),
        ],
        pw,
        GLASS,
        2,
    ));
    lines.push(row(&[s(tail, Some(DIM), Some(GLASS))], pw, GLASS, 8));
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[
            s("GLOBAL", Some(FT), Some(GLASS)),
            s(" ".repeat(30 - 6), None, Some(GLASS)),
            s("TASK", Some(FT), Some(GLASS)),
        ],
        pw,
        GLASS,
        4,
    ));
    let rows_n = HELP_GLOBAL.len().max(HELP_TASK.len());
    for i in 0..rows_n {
        let mut segs: Vec<Seg> = Vec::new();
        if let Some((k, l)) = HELP_GLOBAL.get(i) {
            segs.push(s(format!("{k:>5} "), Some(DIM), Some(GLASS)));
            segs.push(s(*l, Some(FT), Some(GLASS)));
        }
        let padn = 34usize.saturating_sub(seglen(&segs));
        segs.push(s(" ".repeat(padn), None, Some(GLASS)));
        if let Some((k, l)) = HELP_TASK.get(i) {
            segs.push(s(format!("{k:>5} "), Some(DIM), Some(GLASS)));
            segs.push(s(*l, Some(FT), Some(GLASS)));
        }
        lines.push(row(&segs, pw, GLASS, 2));
    }
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(&[s("CONDUITS", Some(FT), Some(GLASS))], pw, GLASS, 4));
    for (name, desc) in HELP_CONDUITS {
        let d: String = desc.chars().take(46).collect();
        lines.push(row(
            &[
                s(format!("{name:>16}"), Some(DIM), Some(GLASS)),
                s(format!("  {d}"), Some(FT), Some(GLASS)),
            ],
            pw,
            GLASS,
            2,
        ));
    }
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[s(" Esc ", Some(TEXT), Some(KCAP)), s(" Close", Some(FT), Some(GLASS))],
        pw,
        GLASS,
        2,
    ));
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(&[s("▀".repeat(pw), Some(GLASS), Some(VOID))], pw, VOID, 0));
    lines
}
