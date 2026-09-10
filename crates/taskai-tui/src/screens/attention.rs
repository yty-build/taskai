// ABOUTME: Attention screen — the amber ask answered in place: single-session rail on an
// ABOUTME: amber-blend fill, the pane's ask row, and the y/n composer.

use crate::chrome::{keybar, masthead, PAD};
use crate::color::{
    bg, harness_color, Rgb, AMB, AMB_GLASS, FT, GLASS, GLASS_HI, ION, OPENC, RESET, TEXT, VOID,
};
use crate::fixture::{ATTENTION_CRUMB, FEED_SHIP, HUD_HOME, LAMP};
use crate::screens::task::{feed_rows, GAP, RAIL_W};
use crate::seg::{clip_segs, row, s, sb, seglen, vrow, Seg};
use crate::state::St;

fn pad(n: isize) -> String {
    " ".repeat(n.max(0) as usize)
}

fn blend(a: Rgb, b: Rgb, k: f64) -> Rgb {
    let m = |x: u8, y: u8| (x as f64 + (y as f64 - x as f64) * k) as u8;
    (m(a.0, b.0), m(a.1, b.1), m(a.2, b.2))
}

pub fn frame(st: &St, cols: usize, rows: usize, clock: &str) -> Vec<String> {
    let mut lines = masthead(cols, ATTENTION_CRUMB, HUD_HOME, Some(LAMP), clock);
    lines.push(vrow(cols));
    let h = rows.saturating_sub(lines.len() + 4);
    let pw = cols - RAIL_W - GAP;
    let w = RAIL_W;
    let rl = |segs: Vec<Seg>| row(&clip_segs(&segs, w - 4), w, GLASS, 2);

    let hkey = "opencode";
    let afill = blend(GLASS, AMB_GLASS, 0.3);
    let mut rail = vec![
        row(&[s("▄".repeat(w), Some(GLASS), Some(VOID))], w, VOID, 0),
        rl(vec![sb("Production release", Some(TEXT), None)]),
        rl(vec![s("/Users/me/GitHub/shop", Some(FT), None)]),
        rl(vec![]),
    ];
    rail.push(row(
        &[
            s("▌", Some(harness_color(hkey)), Some(afill)),
            s(" ", None, Some(afill)),
            s("○ ", Some(AMB), Some(afill)),
            sb("Ship", Some(TEXT), Some(afill)),
        ],
        w,
        afill,
        0,
    ));
    let mut sub = vec![
        s("▌", Some(harness_color(hkey)), Some(afill)),
        s("   ", None, Some(afill)),
        s("OpenCode", Some(FT), Some(afill)),
    ];
    sub.push(s(
        pad(w as isize - 2 - seglen(&sub) as isize - 9),
        None,
        Some(afill),
    ));
    sub.push(s("ATTENTION", Some(AMB), Some(afill)));
    rail.push(row(&sub, w, afill, 0));
    while rail.len() < h.saturating_sub(1) {
        rail.push(rl(vec![]));
    }
    rail.push(row(
        &[s("▀".repeat(w), Some(GLASS), Some(VOID))],
        w,
        VOID,
        0,
    ));

    let b = GLASS_HI;
    let head = vec![
        sb("Ship", Some(TEXT), Some(b)),
        s(" · ", Some(FT), Some(b)),
        s("OpenCode", Some(OPENC), Some(b)),
    ];
    let right = vec![
        s("SHARED-LIVE", Some(FT), Some(b)),
        s("   ", None, Some(b)),
        s("○ ", Some(AMB), Some(b)),
        s("ATTENTION", Some(AMB), Some(b)),
    ];
    let gap = (pw as isize - 2 * PAD as isize - seglen(&head) as isize - seglen(&right) as isize)
        .max(1) as usize;
    let mut hrow = head;
    hrow.push(s(" ".repeat(gap), None, Some(b)));
    hrow.extend(right);
    let mut pane = vec![
        row(&[s("▄".repeat(pw), Some(b), Some(VOID))], pw, VOID, 0),
        row(&hrow, pw, b, PAD),
    ];
    pane.push(row(&[], pw, GLASS, 0));
    pane.extend(feed_rows(FEED_SHIP, pw, GLASS, OPENC, false));
    pane.push(row(&[], pw, GLASS, 0));
    let ans = if st.answered { "y ✓ approved" } else { "y" };
    let cur = if st.blink && !st.answered { "▍" } else { " " };
    pane.push(row(
        &[
            s("❯ ", Some(ION), Some(GLASS_HI)),
            s(ans, Some(TEXT), Some(GLASS_HI)),
            s(cur, Some(ION), Some(GLASS_HI)),
        ],
        pw,
        GLASS_HI,
        PAD,
    ));
    while pane.len() < h.saturating_sub(1) {
        pane.push(row(&[], pw, GLASS, 0));
    }
    pane.push(row(
        &[s("▀".repeat(pw), Some(GLASS_HI), Some(VOID))],
        pw,
        VOID,
        0,
    ));

    let gapstr = format!("{}{}", bg(VOID), " ".repeat(GAP));
    for i in 0..h {
        let r = rail.get(i).cloned().unwrap_or_else(|| row(&[], w, VOID, 0));
        let p = pane
            .get(i)
            .cloned()
            .unwrap_or_else(|| row(&[], pw, VOID, 0));
        lines.push(format!("{r}{gapstr}{p}{RESET}"));
    }
    lines.push(vrow(cols));
    lines.extend(keybar(
        cols,
        &[
            ("y", "Approve"),
            ("n", "Decline"),
            ("G", "Home"),
            ("Ctrl+F", "Full screen"),
        ],
    ));
    lines.push(vrow(cols));
    lines.truncate(rows);
    lines
}
