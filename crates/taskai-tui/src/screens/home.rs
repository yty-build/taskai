// ABOUTME: Home board — full-width task slabs with harness stripes, status, managers,
// ABOUTME: sparklines, the ghost new-task row, and the keycap keybar.

use crate::chrome::{keybar, masthead, PAD};
use crate::color::{
    harness_color, AMB, AMB_GLASS, DIM, FT, GLASS, GLASS_HI, GLASS_IN, ION, KCAP, TEXT, VOID,
};
use crate::fixture::KEYS_HOME;
use crate::view::{HomeTaskRow, Hud};
use crate::seg::{row, s, sb, seglen, vrow, Seg};

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
    for (i, t) in tasks.iter().enumerate() {
        let is_sel = sel == i;
        let fill = if is_sel { GLASS_HI } else { GLASS };
        let stripe = harness_color(t.managers.first().map(|m| m.2.as_str()).unwrap_or(""));
        lines.push(row(&[s("▄".repeat(cols), Some(fill), Some(VOID))], cols, VOID, 0));

        let name = vec![
            s("▌", Some(stripe), Some(fill)),
            s("  ", None, Some(fill)),
            sb(t.name.as_str(), Some(TEXT), Some(fill)),
        ];
        let stat: Vec<Seg> = match t.status.as_str() {
            "WORKING" => vec![s("WORKING", Some(if is_sel { ION } else { DIM }), Some(fill))],
            "NEEDS ATTENTION" => vec![s(" ATTENTION ", Some(AMB), Some(AMB_GLASS))],
            other => vec![s(other, Some(FT), Some(fill))],
        };
        let gap = cols as isize - PAD as isize - seglen(&name) as isize - seglen(&stat) as isize;
        let mut r1 = name;
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
        m.push(s(pad(cols as isize - 52 - seglen(&m) as isize), None, Some(fill)));
        m.push(s(spark, Some(scol), Some(fill)));
        m.push(s(
            pad(cols as isize - PAD as isize - seglen(&m) as isize - t.meta.chars().count() as isize),
            None,
            Some(fill),
        ));
        m.push(s(t.meta.as_str(), Some(FT), Some(fill)));
        lines.push(row(&m, cols, fill, 0));
        lines.push(row(&[s("▀".repeat(cols), Some(fill), Some(VOID))], cols, VOID, 0));
        lines.push(vrow(cols));
    }

    let ghost_sel = sel == tasks.len();
    let gfill = if ghost_sel { GLASS } else { GLASS_IN };
    lines.push(row(&[s("▄".repeat(cols), Some(gfill), Some(VOID))], cols, VOID, 0));
    let mut g = vec![s("+ New task", Some(if ghost_sel { DIM } else { FT }), Some(gfill))];
    g.push(s(
        pad(cols as isize - PAD as isize * 2 - seglen(&g) as isize - 3),
        None,
        Some(gfill),
    ));
    g.push(s(" n ", Some(TEXT), Some(KCAP)));
    lines.push(row(&g, cols, gfill, PAD));
    lines.push(row(&[s("▀".repeat(cols), Some(gfill), Some(VOID))], cols, VOID, 0));

    while lines.len() < rows.saturating_sub(3) {
        lines.push(vrow(cols));
    }
    lines.extend(keybar(cols, KEYS_HOME));
    lines.push(vrow(cols));
    lines.truncate(rows);
    lines
}
