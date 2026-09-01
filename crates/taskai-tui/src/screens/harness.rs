// ABOUTME: Harness bay — the table of native CLIs: readiness, binary, live sessions,
// ABOUTME: conduit mode; SETUP rows in amber, everything else quiet.

use crate::chrome::{keybar, masthead, PAD};
use crate::color::{harness_color, AMB, DIM, FT, GLASS, GLASS_HI, TEXT, VOID};
use crate::fixture::KEYS_HARNESS;
use crate::seg::{row, s, sb, seglen, vrow, Seg};
use crate::view::{HarnessRowView, Hud};

fn pad(n: isize) -> String {
    " ".repeat(n.max(0) as usize)
}

pub fn frame(
    rows_v: &[HarnessRowView],
    summary: &str,
    hud: &Hud,
    cols: usize,
    rows: usize,
    clock: &str,
) -> Vec<String> {
    let hud_items: Vec<(&str, crate::color::Rgb)> =
        hud.items.iter().map(|(t, c)| (t.as_str(), *c)).collect();
    let mut lines = masthead(cols, "HARNESSES", &hud_items, hud.lamp.as_deref(), clock);
    lines.push(vrow(cols));
    let b = GLASS_HI;
    lines.push(row(
        &[s("▄".repeat(cols), Some(GLASS), Some(VOID))],
        cols,
        VOID,
        0,
    ));
    let head = vec![sb("Harness bay", Some(TEXT), Some(b))];
    let tail = vec![s(summary, Some(FT), Some(b))];
    let mut hrow = head;
    hrow.push(s(
        pad(cols as isize - PAD as isize * 2 - seglen(&hrow) as isize - seglen(&tail) as isize),
        None,
        Some(b),
    ));
    hrow.extend(tail);
    lines.push(row(&hrow, cols, b, PAD));
    lines.push(row(&[], cols, GLASS, 0));
    let (c1, c2, c3, c4) = (6usize, 40usize, 70usize, 100usize);
    let lab = vec![
        s(" ".repeat(c1), None, Some(GLASS)),
        s("HARNESS", Some(FT), Some(GLASS)),
        s(" ".repeat(c2 - c1 - 7), None, Some(GLASS)),
        s("BINARY", Some(FT), Some(GLASS)),
        s(" ".repeat(c3 - c2 - 6), None, Some(GLASS)),
        s("SESSIONS", Some(FT), Some(GLASS)),
        s(" ".repeat(c4 - c3 - 8), None, Some(GLASS)),
        s("CONDUIT", Some(FT), Some(GLASS)),
    ];
    lines.push(row(&lab, cols, GLASS, 0));
    lines.push(row(&[], cols, GLASS, 0));
    for h in rows_v {
        let fill = GLASS;
        let sess = h.sessions.as_str();
        let state = if h.setup { "SETUP" } else { "READY" };
        let scol = if h.setup { AMB } else { FT };
        let dot = if h.setup {
            s("○ ", Some(AMB), Some(fill))
        } else {
            s("● ", Some(DIM), Some(fill))
        };
        let mut segs: Vec<Seg> = vec![
            s("▌", Some(harness_color(&h.hkey)), Some(fill)),
            s("  ", None, Some(fill)),
            dot,
            s(h.name.as_str(), Some(TEXT), Some(fill)),
        ];
        segs.push(s(
            pad(c2 as isize - seglen(&segs) as isize),
            None,
            Some(fill),
        ));
        segs.push(s(h.bin.as_str(), Some(FT), Some(fill)));
        segs.push(s(
            pad(c3 as isize - seglen(&segs) as isize),
            None,
            Some(fill),
        ));
        segs.push(s(sess, Some(DIM), Some(fill)));
        segs.push(s(
            pad(c4 as isize - seglen(&segs) as isize),
            None,
            Some(fill),
        ));
        segs.push(s(h.conduit.as_str(), Some(FT), Some(fill)));
        segs.push(s(
            pad(cols as isize
                - PAD as isize
                - seglen(&segs) as isize
                - state.chars().count() as isize),
            None,
            Some(fill),
        ));
        segs.push(s(state, Some(scol), Some(fill)));
        lines.push(row(&segs, cols, fill, 0));
    }
    lines.push(row(&[], cols, GLASS, 0));
    lines.push(row(
        &[s("▀".repeat(cols), Some(GLASS), Some(VOID))],
        cols,
        VOID,
        0,
    ));
    while lines.len() < rows.saturating_sub(3) {
        lines.push(vrow(cols));
    }
    lines.extend(keybar(cols, KEYS_HARNESS));
    lines.push(vrow(cols));
    lines.truncate(rows);
    lines
}
