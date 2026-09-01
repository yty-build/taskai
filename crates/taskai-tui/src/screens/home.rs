// ABOUTME: Home board — full-width task slabs with harness stripes, status, managers,
// ABOUTME: sparklines, the ghost new-task row, and the keycap keybar.

use crate::chrome::{keybar, masthead, PAD};
use crate::color::{
    harness_color, AMB, AMB_GLASS, DIM, FT, GLASS, GLASS_HI, GLASS_IN, ION, KCAP, TEXT, VOID,
};
use crate::fixture::{HUD_HOME, KEYS_HOME, LAMP, TASKS};
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
    sel: usize,
    spark_t1: &[u8],
    spark_t2: &[u8],
    cols: usize,
    rows: usize,
    clock: &str,
) -> Vec<String> {
    let mut lines = masthead(cols, "PORTFOLIO", HUD_HOME, Some(LAMP), clock);
    lines.push(vrow(cols));
    for (i, t) in TASKS.iter().enumerate() {
        let is_sel = sel == i;
        let fill = if is_sel { GLASS_HI } else { GLASS };
        let stripe = harness_color(t.managers[0].hkey);
        lines.push(row(&[s("▄".repeat(cols), Some(fill), Some(VOID))], cols, VOID, 0));

        let name = vec![
            s("▌", Some(stripe), Some(fill)),
            s("  ", None, Some(fill)),
            sb(t.name, Some(TEXT), Some(fill)),
        ];
        let stat: Vec<Seg> = match t.status {
            "WORKING" => vec![s("WORKING", Some(if is_sel { ION } else { DIM }), Some(fill))],
            "NEEDS ATTENTION" => vec![s(" ATTENTION ", Some(AMB), Some(AMB_GLASS))],
            _ => vec![s("COMPLETE", Some(FT), Some(fill))],
        };
        let gap = cols as isize - PAD as isize - seglen(&name) as isize - seglen(&stat) as isize;
        let mut r1 = name;
        r1.push(s(pad(gap), None, Some(fill)));
        r1.extend(stat);
        lines.push(row(&r1, cols, fill, 0));

        let spark = match i {
            0 => spark_str(spark_t1),
            1 => spark_str(spark_t2),
            _ => "▁".repeat(12),
        };
        let scol = if is_sel && t.status == "WORKING" { ION } else { FT };
        let mut m: Vec<Seg> = vec![s("▌", Some(stripe), Some(fill)), s("  ", None, Some(fill))];
        for (j, mgr) in t.managers.iter().enumerate() {
            if j > 0 {
                m.push(s("   ", None, Some(fill)));
            }
            m.push(s("● ", Some(harness_color(mgr.hkey)), Some(fill)));
            m.push(s(format!("{} · {}", mgr.role, mgr.harness), Some(FT), Some(fill)));
        }
        m.push(s(pad(cols as isize - 52 - seglen(&m) as isize), None, Some(fill)));
        m.push(s(spark, Some(scol), Some(fill)));
        m.push(s(
            pad(cols as isize - PAD as isize - seglen(&m) as isize - t.meta.chars().count() as isize),
            None,
            Some(fill),
        ));
        m.push(s(t.meta, Some(FT), Some(fill)));
        lines.push(row(&m, cols, fill, 0));
        lines.push(row(&[s("▀".repeat(cols), Some(fill), Some(VOID))], cols, VOID, 0));
        lines.push(vrow(cols));
    }

    let ghost_sel = sel == 3;
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
