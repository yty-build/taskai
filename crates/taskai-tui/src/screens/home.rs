// ABOUTME: Home board — full-width task slabs with harness stripes, status, managers,
// ABOUTME: sparklines, the ghost new-task row, and the keycap keybar.

use crate::chrome::{masthead, navigation_keybar, PAD};
use crate::color::{
    harness_color, AMB, AMB_GLASS, DIM, FT, GLASS, GLASS_HI, GLASS_IN, ION, KCAP, TEXT, VOID,
};
use crate::fixture::KEYS_HOME;
use crate::seg::{clip_segs, row, s, sb, seglen, vrow, Seg};
use crate::view::{HomeTaskRow, Hud};

pub const SPARK_GLYPHS: &str = "▁▂▃▄▅▆▇";

pub fn spark_str(levels: &[u8]) -> String {
    levels
        .iter()
        .map(|&v| SPARK_GLYPHS.chars().nth(v as usize).unwrap_or('▁'))
        .collect()
}

fn pad(n: isize) -> String {
    " ".repeat(n.max(0) as usize)
}

// Four masthead rows and three footer rows are fixed; only the cards scroll.
fn viewport(task_count: usize, sel: usize, rows: usize) -> (usize, usize, bool) {
    let content_len = task_count * 5 + 3;
    let content_h = rows.saturating_sub(7);
    if content_len <= content_h {
        return (0, content_len, false);
    }
    let inner_h = content_h.saturating_sub(2);
    let selected = sel.min(task_count);
    let selected_end = if selected == task_count {
        content_len
    } else {
        selected * 5 + 4
    };
    let start = selected_end
        .saturating_sub(inner_h)
        .min(content_len.saturating_sub(inner_h));
    (start, inner_h, true)
}

/// Hit-test a full-screen row against the same viewport used to render cards.
/// The `task_count` index is the New task card; headings, gaps and keybars are
/// deliberately not clickable task targets.
pub fn task_at_row(
    task_count: usize,
    sel: usize,
    _cols: usize,
    rows: usize,
    y: usize,
) -> Option<usize> {
    let (start, visible, scrolled) = viewport(task_count, sel, rows);
    let local = y.checked_sub(4 + usize::from(scrolled))?;
    if local >= visible {
        return None;
    }
    let content_row = start + local;
    if content_row >= task_count * 5 {
        return (content_row < task_count * 5 + 3).then_some(task_count);
    }
    (content_row % 5 < 4).then_some(content_row / 5)
}

pub fn frame(
    tasks: &[HomeTaskRow],
    hud: &Hud,
    sel: usize,
    cols: usize,
    rows: usize,
    clock: &str,
) -> Vec<String> {
    let hud_items: Vec<(&str, crate::color::Rgb)> =
        hud.items.iter().map(|(t, c)| (t.as_str(), *c)).collect();
    let mut lines = masthead(cols, "PORTFOLIO", &hud_items, hud.lamp.as_deref(), clock);
    lines.push(vrow(cols));
    let content_top = lines.len();
    for (i, t) in tasks.iter().enumerate() {
        let is_sel = sel == i;
        let fill = if is_sel { GLASS_HI } else { GLASS };
        let stripe = harness_color(t.managers.first().map(|m| m.2.as_str()).unwrap_or(""));
        lines.push(row(
            &[s("▄".repeat(cols), Some(fill), Some(VOID))],
            cols,
            VOID,
            0,
        ));

        let name = vec![
            s("▌", Some(stripe), Some(fill)),
            s("  ", None, Some(fill)),
            sb(t.name.as_str(), Some(TEXT), Some(fill)),
        ];
        let stat: Vec<Seg> = match t.status.as_str() {
            "WORKING" => vec![s(
                "WORKING",
                Some(if is_sel { ION } else { DIM }),
                Some(fill),
            )],
            "NEEDS ATTENTION" => vec![s(" ATTENTION ", Some(AMB), Some(AMB_GLASS))],
            other => vec![s(other, Some(FT), Some(fill))],
        };
        let mut r1 = clip_segs(&name, cols.saturating_sub(PAD + seglen(&stat) + 1));
        let gap = cols as isize - PAD as isize - seglen(&r1) as isize - seglen(&stat) as isize;
        r1.push(s(pad(gap), None, Some(fill)));
        r1.extend(stat);
        lines.push(row(&r1, cols, fill, 0));

        let spark = t.spark.clone();
        let scol = if is_sel && t.spark_live { ION } else { FT };
        let mut m: Vec<Seg> = vec![s("▌", Some(stripe), Some(fill)), s("  ", None, Some(fill))];
        for (j, (role, harness, hkey)) in t.managers.iter().enumerate() {
            if j > 0 {
                m.push(s("   ", None, Some(fill)));
            }
            m.push(s("● ", Some(harness_color(hkey)), Some(fill)));
            m.push(s(format!("{role} · {harness}"), Some(FT), Some(fill)));
        }
        // A long manager list must not push the task's repository/count summary
        // off the screen. Keep both pieces readable and mark a clipped list.
        let meta = vec![s(
            crate::screens::task::ws_short(&t.meta, cols / 2),
            Some(FT),
            Some(fill),
        )];
        let spark_start = cols
            .saturating_sub(52)
            .min(cols.saturating_sub(PAD + seglen(&meta) + t.spark.chars().count() + 1));
        m = clip_segs(&m, spark_start.saturating_sub(1));
        m.push(s(
            pad(spark_start as isize - seglen(&m) as isize),
            None,
            Some(fill),
        ));
        m.push(s(spark, Some(scol), Some(fill)));
        m.push(s(
            pad(cols as isize - PAD as isize - seglen(&m) as isize - seglen(&meta) as isize),
            None,
            Some(fill),
        ));
        m.extend(meta);
        lines.push(row(&m, cols, fill, 0));
        lines.push(row(
            &[s("▀".repeat(cols), Some(fill), Some(VOID))],
            cols,
            VOID,
            0,
        ));
        lines.push(vrow(cols));
    }

    let ghost_sel = sel == tasks.len();
    let gfill = if ghost_sel { GLASS } else { GLASS_IN };
    lines.push(row(
        &[s("▄".repeat(cols), Some(gfill), Some(VOID))],
        cols,
        VOID,
        0,
    ));
    let mut g = vec![s(
        "+ New task",
        Some(if ghost_sel { DIM } else { FT }),
        Some(gfill),
    )];
    g.push(s(
        pad(cols as isize - PAD as isize * 2 - seglen(&g) as isize - 3),
        None,
        Some(gfill),
    ));
    g.push(s(" n ", Some(TEXT), Some(KCAP)));
    lines.push(row(&g, cols, gfill, PAD));
    lines.push(row(
        &[s("▀".repeat(cols), Some(gfill), Some(VOID))],
        cols,
        VOID,
        0,
    ));

    // The portfolio is a viewport, not a document: keep the selected task (or
    // New task) visible and reserve the footer even with hundreds of tasks.
    let content_len = lines.len().saturating_sub(content_top);
    let (start, inner_h, scrolled) = viewport(tasks.len(), sel, rows);
    if scrolled {
        let end = (start + inner_h).min(content_len);
        let content = lines.split_off(content_top);
        lines.push(row(
            &[s(
                if start > 0 {
                    "↑ More tasks above"
                } else {
                    ""
                },
                Some(FT),
                None,
            )],
            cols,
            VOID,
            PAD,
        ));
        lines.extend(content.into_iter().skip(start).take(inner_h));
        lines.push(row(
            &[s(
                if end < content_len {
                    "↓ More tasks below"
                } else {
                    ""
                },
                Some(FT),
                None,
            )],
            cols,
            VOID,
            PAD,
        ));
    }

    while lines.len() < rows.saturating_sub(3) {
        lines.push(vrow(cols));
    }
    lines.extend(navigation_keybar(cols, KEYS_HOME));
    lines.push(vrow(cols));
    lines.truncate(rows);
    lines
}
