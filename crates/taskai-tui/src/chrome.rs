// ABOUTME: Shared TaskAI chrome — masthead HUD strip, keycap keybar, soft slab edges.
// ABOUTME: Byte-for-byte port of the prototype's mh()/keys2()/soft edges.

use crate::color::{Rgb, AMB, AMB_GLASS, FT, GLASS_HI, HAIR, ION, KCAP, TEXT, VOID};
use crate::seg::{clip_segs, row, s, sb, seglen, Seg};

/// Interior side padding — the rhythm unit.
pub const PAD: usize = 3;

/// A steady focus indicator in an existing chrome row, never in native cells.
/// The arrow and label identify the input owner independently of its color.
pub fn focus_edge(cols: usize, label: &str, active: bool) -> String {
    let color = if active { ION } else { FT };
    let mut segs = vec![
        s(if active { "▸ " } else { "─ " }, Some(color), Some(VOID)),
        Seg {
            text: label.into(),
            fg: Some(color),
            bg: Some(VOID),
            bold: active,
        },
        s(" ", Some(color), Some(VOID)),
    ];
    segs.push(s(
        "─".repeat(cols.saturating_sub(seglen(&segs))),
        Some(if active { ION } else { HAIR }),
        Some(VOID),
    ));
    row(&segs, cols, VOID, 0)
}

/// Half-block upper edge: reads as a rounded slab top on the void.
pub fn soft_top(fill: Rgb, width: usize) -> String {
    row(
        &[s("▄".repeat(width), Some(fill), Some(VOID))],
        width,
        VOID,
        0,
    )
}

/// Half-block lower edge.
pub fn soft_bot(fill: Rgb, width: usize) -> String {
    row(
        &[s("▀".repeat(width), Some(fill), Some(VOID))],
        width,
        VOID,
        0,
    )
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
    left = clip_segs(&left, cols.saturating_sub(2 * PAD + seglen(&right) + 1));
    let gap = (cols as isize - 2 * PAD as isize - seglen(&left) as isize - seglen(&right) as isize)
        .max(1) as usize;
    left.push(s(" ".repeat(gap), None, Some(b)));
    left.extend(right);
    vec![
        soft_top(b, cols),
        row(&left, cols, b, PAD),
        soft_bot(b, cols),
    ]
}

/// Hairline rule, then centered keycap chips: ` key ` on KCAP fill, label on the void.
pub fn keybar(cols: usize, pairs: &[(&str, &str)]) -> Vec<String> {
    let mut segs: Vec<Seg> = Vec::new();
    let labels_width: usize = pairs
        .iter()
        .map(|(k, l)| k.chars().count() + l.chars().count() + 3)
        .sum();
    let spacing = if pairs.len() > 1 {
        cols.saturating_sub(labels_width)
            .checked_div(pairs.len() - 1)
            .unwrap_or(0)
            .min(4)
    } else {
        0
    };
    for (i, (key, label)) in pairs.iter().enumerate() {
        if i > 0 {
            segs.push(s(" ".repeat(spacing), None, Some(VOID)));
        }
        segs.push(s(format!(" {key} "), Some(TEXT), Some(KCAP)));
        segs.push(s(format!(" {label}"), Some(FT), Some(VOID)));
    }
    let pad = cols.saturating_sub(seglen(&segs)) / 2;
    let mut centered = vec![s(" ".repeat(pad), None, Some(VOID))];
    centered.extend(segs);
    vec![
        row(
            &[s("─".repeat(cols), Some(HAIR), Some(VOID))],
            cols,
            VOID,
            0,
        ),
        row(&centered, cols, VOID, 0),
    ]
}

/// Keep the same two-row footer while making long navigation maps readable.
/// Leave `keybar` itself unwrapped: modal/mouse-hit callers already arrange
/// individual chip rows and rely on its stable rule-plus-chips contract.
pub fn navigation_keybar(cols: usize, pairs: &[(&str, &str)]) -> Vec<String> {
    let widths: Vec<_> = pairs
        .iter()
        .map(|(key, label)| key.chars().count() + label.chars().count() + 3)
        .collect();
    let total: usize = widths.iter().sum();
    if total <= cols || pairs.len() < 2 {
        return keybar(cols, pairs);
    }
    let mut prefix = 0usize;
    let mut best = (usize::MAX, 1);
    for (index, width) in widths.iter().enumerate().take(pairs.len() - 1) {
        prefix += width;
        let widest = prefix.max(total - prefix);
        if widest < best.0 {
            best = (widest, index + 1);
        }
    }
    vec![
        keybar(cols, &pairs[..best.1])[1].clone(),
        keybar(cols, &pairs[best.1..])[1].clone(),
    ]
}
