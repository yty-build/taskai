// ABOUTME: Shared TaskAI chrome — masthead HUD strip, keycap keybar, soft slab edges.
// ABOUTME: Byte-for-byte port of the prototype's mh()/keys2()/soft edges.

use crate::color::{Rgb, AMB, AMB_GLASS, FT, GLASS_HI, HAIR, KCAP, TEXT, VOID};
use crate::seg::{row, s, sb, seglen, Seg};

/// Interior side padding — the rhythm unit.
pub const PAD: usize = 3;

/// Half-block upper edge: reads as a rounded slab top on the void.
pub fn soft_top(fill: Rgb, width: usize) -> String {
    row(&[s("▄".repeat(width), Some(fill), Some(VOID))], width, VOID, 0)
}

/// Half-block lower edge.
pub fn soft_bot(fill: Rgb, width: usize) -> String {
    row(&[s("▀".repeat(width), Some(fill), Some(VOID))], width, VOID, 0)
}

/// The HUD strip: wordmark, crumb, right-side counts, attention lamp, clock.
pub fn masthead(
    cols: usize,
    crumb: &str,
    hud: &[(&str, Rgb)],
    lamp: Option<&str>,
    clock: &str,
) -> Vec<String> {
    let b = GLASS_HI;
    let mut left = vec![
        sb("TASKAI", Some(TEXT), Some(b)),
        s("   ", None, Some(b)),
        s(crumb, Some(FT), Some(b)),
    ];
    let mut right: Vec<Seg> = Vec::new();
    for (i, (t, c)) in hud.iter().enumerate() {
        if i > 0 {
            right.push(s(" · ", Some(FT), Some(b)));
        }
        right.push(s(*t, Some(*c), Some(b)));
    }
    if let Some(l) = lamp {
        right.push(s(" · ", Some(FT), Some(b)));
        right.push(s(format!(" {l} "), Some(AMB), Some(AMB_GLASS)));
    }
    right.push(s("   ", None, Some(b)));
    right.push(s(clock, Some(TEXT), Some(b)));
    let gap = (cols as isize - 2 * PAD as isize - seglen(&left) as isize - seglen(&right) as isize)
        .max(1) as usize;
    left.push(s(" ".repeat(gap), None, Some(b)));
    left.extend(right);
    vec![soft_top(b, cols), row(&left, cols, b, PAD), soft_bot(b, cols)]
}

/// Hairline rule, then centered keycap chips: ` key ` on KCAP fill, label on the void.
pub fn keybar(cols: usize, pairs: &[(&str, &str)]) -> Vec<String> {
    let mut segs: Vec<Seg> = Vec::new();
    for (i, (key, label)) in pairs.iter().enumerate() {
        if i > 0 {
            segs.push(s("    ", None, Some(VOID)));
        }
        segs.push(s(format!(" {key} "), Some(TEXT), Some(KCAP)));
        segs.push(s(format!(" {label}"), Some(FT), Some(VOID)));
    }
    let pad = cols.saturating_sub(seglen(&segs)) / 2;
    let mut centered = vec![s(" ".repeat(pad), None, Some(VOID))];
    centered.extend(segs);
    vec![
        row(&[s("─".repeat(cols), Some(HAIR), Some(VOID))], cols, VOID, 0),
        row(&centered, cols, VOID, 0),
    ]
}
