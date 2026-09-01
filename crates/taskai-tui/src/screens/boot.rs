// ABOUTME: Boot screen — centered glass slab with wordmark, tagline, harness probes,
// ABOUTME: and the readiness meter.

use crate::color::{dull, harness_color, AMB, DIM, FT, GLASS, GLASS_IN, ION, TEXT, VOID};
use crate::fixture::TAGLINE;
use crate::seg::{row, s, sb, seglen, vrow, Seg};
use crate::view::ProbeRow;

const SW: usize = 56;

pub fn frame(
    probes: &[ProbeRow],
    meter_done: &str,
    boot_k: usize,
    cols: usize,
    rows: usize,
) -> Vec<String> {
    let ind = cols.saturating_sub(SW) / 2;
    let srow = |segs: Vec<Seg>| -> String {
        let used = seglen(&segs);
        let mut v = vec![s("   ", None, Some(GLASS))];
        v.extend(segs);
        v.push(s(
            " ".repeat((SW - 3).saturating_sub(used)),
            None,
            Some(GLASS),
        ));
        row(&v, cols, VOID, ind)
    };

    let mut lines: Vec<String> = (0..rows.saturating_sub(15) / 2)
        .map(|_| vrow(cols))
        .collect();
    lines.push(row(
        &[s("▄".repeat(SW), Some(GLASS), Some(VOID))],
        cols,
        VOID,
        ind,
    ));
    lines.push(srow(vec![]));
    let mark = "T A S K A I";
    lines.push(srow(vec![
        s(
            " ".repeat((SW - 6 - mark.chars().count()) / 2),
            None,
            Some(GLASS),
        ),
        sb(mark, Some(TEXT), Some(GLASS)),
    ]));
    lines.push(srow(vec![
        s(
            " ".repeat((SW - 6 - TAGLINE.chars().count()) / 2),
            None,
            Some(GLASS),
        ),
        s(TAGLINE, Some(FT), Some(GLASS)),
    ]));
    lines.push(srow(vec![]));
    let shown = probes.len().min((boot_k / 3 + 1).max(1));
    for p in &probes[..shown] {
        let mut segs = vec![
            s("▌", Some(harness_color(&p.hkey)), Some(GLASS)),
            s("  ", None, Some(GLASS)),
            s(p.name.as_str(), Some(DIM), Some(GLASS)),
        ];
        let tail: Vec<Seg> = if p.ready {
            vec![
                s("✓ ", Some(FT), Some(GLASS)),
                s("READY", Some(FT), Some(GLASS)),
            ]
        } else {
            vec![s("SETUP", Some(AMB), Some(GLASS))]
        };
        segs.push(s(
            " ".repeat((SW - 6).saturating_sub(seglen(&segs) + seglen(&tail))),
            None,
            Some(GLASS),
        ));
        segs.extend(tail);
        lines.push(srow(segs));
    }
    for _ in 0..probes.len().saturating_sub(shown) {
        lines.push(srow(vec![]));
    }
    lines.push(srow(vec![]));
    let k = boot_k.min(19);
    let cap = if boot_k >= 19 {
        meter_done
    } else {
        "probing harnesses…"
    };
    lines.push(srow(vec![
        s(" ".repeat(k), None, Some(dull(ION, 0.30))),
        s(" ".repeat(24 - k), None, Some(GLASS_IN)),
        s("  ", None, Some(GLASS)),
        s(cap, Some(FT), Some(GLASS)),
    ]));
    lines.push(srow(vec![]));
    lines.push(row(
        &[s("▀".repeat(SW), Some(GLASS), Some(VOID))],
        cols,
        VOID,
        ind,
    ));
    while lines.len() < rows {
        lines.push(vrow(cols));
    }
    lines.truncate(rows);
    lines
}
