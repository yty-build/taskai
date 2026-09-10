// ABOUTME: Overlay compositor and floating panels — dims the rendered backdrop with an
// ABOUTME: SGR-preserving cell slicer and splices drawers, destroy, and help over it.

use crate::color::{
    bg, dull, DIM, FT, GLASS, GLASS_HI, GLASS_IN, ION, KCAP, RED, RED_GLASS, RESET, TEXT, VOID,
};
use crate::fixture::{
    Drawer, DESTROY_BLAST, DESTROY_CONFIRM, DESTROY_KEPT, DESTROY_TITLE, HELP_CLI, HELP_CONDUITS,
    HELP_NAVIGATION, HELP_TASK, NEWTASK, NEWTASK_NOTE, SUBORDINATE, SUBORDINATE_NOTE,
    SUBORDINATE_PREVIEW,
};
use crate::seg::{cell_count, char_width, row, s, sb, seglen, Seg};

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
    let mut complete_glyph = false;
    let mut written = 0;
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
        let glyph_width = char_width(chars[i]);
        if glyph_width == 0 {
            if emitting && complete_glyph {
                out.push(chars[i]);
            }
            i += 1;
            continue;
        }
        if col >= start + width {
            break;
        }
        let next = col + glyph_width;
        let overlap = next.min(start + width).saturating_sub(col.max(start));
        if overlap > 0 && !emitting {
            emitting = true;
            out.push_str(&state);
        }
        complete_glyph = overlap == glyph_width;
        if overlap > 0 {
            if complete_glyph {
                out.push(if chars[i].is_control() {
                    '�'
                } else {
                    chars[i]
                });
            } else {
                // An overlay edge may bisect a double-width glyph. Paint the
                // uncovered half as a space rather than shifting every cell.
                out.push_str(&" ".repeat(overlap));
            }
            written += overlap;
        }
        col = next;
        i += 1;
    }
    if written < width {
        out.push_str(&bg(VOID));
        out.push_str(&" ".repeat(width - written));
    }
    out.push_str(RESET);
    out
}

pub fn panel_width(panel: &[String]) -> usize {
    panel.first().map(|l| cell_count(l)).unwrap_or(0)
}

/// Dim the base and splice a floating panel over it, starting no lower than
/// the last position that leaves the status row visible.
pub fn overlay(base: &[String], panel: &[String], cols: usize, top: usize) -> Vec<String> {
    let pw = panel_width(panel).min(cols);
    let ind = cols.saturating_sub(pw) / 2;
    let mut out: Vec<String> = base.iter().map(|l| dim_line(l, 0.42)).collect();
    let usable_rows = out.len().saturating_sub(1);
    let top = top.min(usable_rows.saturating_sub(panel.len()));
    for (i, prow) in panel.iter().enumerate() {
        let r = top + i;
        if r >= usable_rows {
            break;
        }
        out[r] = format!(
            "{}{}{}",
            slice_cells(&out[r], 0, ind),
            slice_cells(prow, 0, pw),
            slice_cells(&out[r], ind + pw, cols - ind - pw)
        );
    }
    out
}

/// Center a panel within the live terminal. `overlay` still clamps the result
/// upward when a compact viewport needs the space to keep every panel row and
/// the status line visible.
pub fn centered_overlay(base: &[String], panel: &[String], cols: usize) -> Vec<String> {
    let top = base.len().saturating_sub(panel.len()) / 2;
    overlay(base, panel, cols, top)
}

/// Broad but bounded form width used by the live drawers. The compact minimum
/// retains six cells of breathing room per side; ultrawide terminals cap the
/// form so the input fields remain easy to scan.
pub fn form_panel_width(cols: usize) -> usize {
    let cap = if cols >= 280 { 160 } else { 144 };
    cols.saturating_sub(12).min(cap)
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

pub struct DrawerView {
    pub title: String,
    pub fields: Vec<(String, String, bool)>,
    pub keys: Vec<(&'static str, &'static str)>,
}

pub fn drawer_panel_view(blink: bool, d: &DrawerView, note: &[Vec<Seg>], pw: usize) -> Vec<String> {
    let b = GLASS_HI;
    let mut lines = vec![row(&[s("▄".repeat(pw), Some(b), Some(VOID))], pw, VOID, 0)];
    lines.push(row(&[sb(d.title.as_str(), Some(TEXT), Some(b))], pw, b, 2));
    lines.push(row(&[], pw, GLASS, 0));
    for (label, value, focus) in &d.fields {
        lines.push(row(
            &[s(label.to_uppercase(), Some(FT), Some(GLASS))],
            pw,
            GLASS,
            2,
        ));
        let mut val = vec![
            s(" ", None, Some(GLASS_IN)),
            s(
                value.as_str(),
                Some(if *focus { TEXT } else { DIM }),
                Some(GLASS_IN),
            ),
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
    if !d.keys.is_empty() {
        lines.push(keys_row(&d.keys, pw));
        lines.push(row(&[], pw, GLASS, 0));
    }
    lines.push(row(
        &[s("▀".repeat(pw), Some(GLASS), Some(VOID))],
        pw,
        VOID,
        0,
    ));
    lines
}

pub fn drawer_panel(blink: bool, drawer: &Drawer, note: &[Vec<Seg>], pw: usize) -> Vec<String> {
    let view = DrawerView {
        title: drawer.title.into(),
        fields: drawer
            .fields
            .iter()
            .map(|(l, v, f)| (l.to_string(), v.to_string(), *f))
            .collect(),
        keys: drawer.keys.to_vec(),
    };
    drawer_panel_view(blink, &view, note, pw)
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

pub fn destroy_panel_view(
    blink: bool,
    title: &str,
    blast: &[String],
    kept: &[&str],
    expect: &str,
    typed: &str,
) -> Vec<String> {
    let pw = 62;
    let mut lines = vec![row(
        &[s("▄".repeat(pw), Some(RED_GLASS), Some(VOID))],
        pw,
        VOID,
        0,
    )];
    lines.push(row(
        &[sb(title, Some(RED), Some(RED_GLASS))],
        pw,
        RED_GLASS,
        2,
    ));
    lines.push(row(&[], pw, GLASS, 0));
    for b in blast {
        lines.push(row(
            &[
                s("● ", Some(RED), Some(GLASS)),
                s(b.as_str(), Some(TEXT), Some(GLASS)),
            ],
            pw,
            GLASS,
            2,
        ));
    }
    lines.push(row(&[], pw, GLASS, 0));
    for k in kept {
        lines.push(row(
            &[
                s("✓ ", Some(FT), Some(GLASS)),
                s(*k, Some(DIM), Some(GLASS)),
            ],
            pw,
            GLASS,
            2,
        ));
    }
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[
            s("Type ", Some(FT), Some(GLASS)),
            sb(expect, Some(TEXT), Some(GLASS)),
            s(" to confirm", Some(FT), Some(GLASS)),
        ],
        pw,
        GLASS,
        2,
    ));
    let cur = if blink { "▍" } else { " " };
    let mut val = vec![
        s(format!(" {typed}"), Some(TEXT), Some(GLASS_IN)),
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
    lines.push(row(
        &[s("▀".repeat(pw), Some(GLASS), Some(VOID))],
        pw,
        VOID,
        0,
    ));
    lines
}

pub fn destroy_panel(blink: bool) -> Vec<String> {
    let blast: Vec<String> = DESTROY_BLAST.iter().map(|b| b.to_string()).collect();
    destroy_panel_view(
        blink,
        DESTROY_TITLE,
        &blast,
        DESTROY_KEPT,
        DESTROY_CONFIRM,
        DESTROY_CONFIRM,
    )
}

/// The repository explorer: navigate directories, choose one with Space.
pub fn explorer_panel(cwd: &str, entries: &[(String, bool)], sel: usize) -> Vec<String> {
    explorer_panel_width(cwd, entries, sel, 66)
}

pub fn explorer_panel_width(
    cwd: &str,
    entries: &[(String, bool)],
    sel: usize,
    pw: usize,
) -> Vec<String> {
    let b = GLASS_HI;
    let mut lines = vec![row(&[s("▄".repeat(pw), Some(b), Some(VOID))], pw, VOID, 0)];
    lines.push(row(
        &[sb("Choose repository", Some(TEXT), Some(b))],
        pw,
        b,
        2,
    ));
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[s("CURRENT PATH", Some(FT), Some(GLASS))],
        pw,
        GLASS,
        2,
    ));
    let shown_path: String = if cwd.chars().count() > pw - 6 {
        format!(
            "…{}",
            cwd.chars()
                .rev()
                .take(pw - 7)
                .collect::<Vec<_>>()
                .into_iter()
                .rev()
                .collect::<String>()
        )
    } else {
        cwd.to_string()
    };
    let mut pathrow = vec![s(format!(" {shown_path}"), Some(DIM), Some(GLASS_IN))];
    pathrow.push(s(
        " ".repeat((pw as isize - 4 - seglen(&pathrow) as isize).max(0) as usize),
        None,
        Some(GLASS_IN),
    ));
    lines.push(row(&pathrow, pw, GLASS, 2));
    lines.push(row(&[], pw, GLASS, 0));
    let window = 12usize;
    let start = sel.saturating_sub(window.saturating_sub(1).min(sel));
    let start = start.min(entries.len().saturating_sub(window.min(entries.len())));
    for (i, (name, is_dir)) in entries.iter().enumerate().skip(start).take(window) {
        let selected = i == sel;
        let fill = if selected { GLASS_HI } else { GLASS };
        let marker = if *is_dir { "▸ " } else { "  " };
        let label = if *is_dir {
            format!("{name}/")
        } else {
            name.clone()
        };
        let mut segs = vec![
            s(marker, Some(if selected { ION } else { FT }), Some(fill)),
            s(label, Some(if selected { TEXT } else { DIM }), Some(fill)),
        ];
        segs.push(s(
            " ".repeat((pw as isize - 4 - seglen(&segs) as isize).max(0) as usize),
            None,
            Some(fill),
        ));
        lines.push(row(&segs, pw, fill, 2));
    }
    for _ in entries.len().saturating_sub(start).min(window)..window {
        lines.push(row(&[], pw, GLASS, 0));
    }
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(keys_row(
        &[
            ("↑/↓", "Move"),
            ("Enter", "Open"),
            ("←", "Parent"),
            ("Space", "Choose"),
            ("Esc", "Cancel"),
        ],
        pw,
    ));
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[s("▀".repeat(pw), Some(GLASS), Some(VOID))],
        pw,
        VOID,
        0,
    ));
    lines
}

pub fn help_panel() -> Vec<String> {
    let pw = 68;
    let b = GLASS_HI;
    let mut lines = vec![row(&[s("▄".repeat(pw), Some(b), Some(VOID))], pw, VOID, 0)];
    lines.push(row(&[sb("Help", Some(TEXT), Some(b))], pw, b, 2));
    lines.push(row(&[], pw, GLASS, 0));
    for (key, description) in HELP_CLI {
        lines.push(row(
            &[
                sb(format!(" {key} "), Some(TEXT), Some(GLASS_IN)),
                s(format!("  {description}"), Some(DIM), Some(GLASS)),
            ],
            pw,
            GLASS,
            2,
        ));
    }
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[
            s("NAVIGATION", Some(FT), Some(GLASS)),
            s(" ".repeat(30 - 10), None, Some(GLASS)),
            s("TASK", Some(FT), Some(GLASS)),
        ],
        pw,
        GLASS,
        4,
    ));
    let rows_n = HELP_NAVIGATION.len().max(HELP_TASK.len());
    for i in 0..rows_n {
        let mut segs: Vec<Seg> = Vec::new();
        if let Some((k, l)) = HELP_NAVIGATION.get(i) {
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
        &[
            s(" Esc ", Some(TEXT), Some(KCAP)),
            s(" Close", Some(FT), Some(GLASS)),
        ],
        pw,
        GLASS,
        2,
    ));
    lines.push(row(&[], pw, GLASS, 0));
    lines.push(row(
        &[s("▀".repeat(pw), Some(GLASS), Some(VOID))],
        pw,
        VOID,
        0,
    ));
    lines
}
