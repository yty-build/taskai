// ABOUTME: Attach screen — full-bleed hosted CLI with the return ribbon on top;
// ABOUTME: the CLI owns everything below the ribbon.

use crate::chrome::PAD;
use crate::color::{CLAUDE, CLI_BG, CLI_EDGE, DIM, FT, GLASS_IN, KCAP, TEXT, VOID};
use crate::fixture::{ATTACH_COMPOSER, FEED_LEAD};
use crate::screens::task::{feed_rows, SPIN_GLYPHS};
use crate::seg::{row, s, seglen, vrow, Seg};
use crate::state::St;

fn pad(n: isize) -> String {
    " ".repeat(n.max(0) as usize)
}

pub fn frame(st: &St, cols: usize, rows: usize) -> Vec<String> {
    let b = GLASS_IN;
    let mut rib: Vec<Seg> = vec![
        s("▌", Some(CLAUDE), Some(b)),
        s("  ", None, Some(b)),
        s("Lead · Claude Code", Some(DIM), Some(b)),
    ];
    let mid = vec![s("owned-tui", Some(FT), Some(b))];
    let tail = vec![
        s(" Ctrl+G ", Some(TEXT), Some(KCAP)),
        s(" Return to TaskAI", Some(FT), Some(b)),
    ];
    rib.push(s(
        pad(cols as isize * 2 / 3 - seglen(&rib) as isize - seglen(&mid) as isize),
        None,
        Some(b),
    ));
    rib.extend(mid);
    rib.push(s(
        pad(cols as isize - PAD as isize - seglen(&rib) as isize - seglen(&tail) as isize),
        None,
        Some(b),
    ));
    rib.extend(tail);
    let mut lines = vec![
        row(&[s("▄".repeat(cols), Some(b), Some(VOID))], cols, VOID, 0),
        row(&rib, cols, b, 0),
        row(&[s("▀".repeat(cols), Some(b), Some(VOID))], cols, VOID, 0),
        vrow(cols),
    ];
    let base = CLI_BG;
    let h = rows - lines.len();
    let body_h = h.saturating_sub(6);
    let mut rows_ = vec![row(&[], cols, base, 0)];
    let hdr = "✻ Claude Code · claude-fable-5 · /Users/me/GitHub/buzz";
    let mut chars = hdr.chars();
    let first = chars.next().map(|c| c.to_string()).unwrap_or_default();
    let rest: String = chars.collect();
    rows_.push(row(
        &[
            s(first, Some(CLAUDE), Some(base)),
            s(rest, Some(FT), Some(base)),
        ],
        cols,
        base,
        2,
    ));
    rows_.push(row(&[], cols, base, 0));
    rows_.extend(feed_rows(FEED_LEAD, cols, base, CLAUDE, true));
    rows_.push(row(&[], cols, base, 0));
    let elapsed = (st.tick.saturating_sub(st.spin_t0)) / 10;
    rows_.push(row(
        &[
            s(
                format!("{} ", SPIN_GLYPHS[st.spin_i % SPIN_GLYPHS.len()]),
                Some(CLAUDE),
                Some(base),
            ),
            s("Drafting rollback dry-run plan…", Some(TEXT), Some(base)),
            s(
                format!(
                    "  (esc to interrupt · {elapsed}s · ↓ {:.1}k tokens)",
                    st.tokens as f64 / 1000.0
                ),
                Some(FT),
                Some(base),
            ),
        ],
        cols,
        base,
        2,
    ));
    if rows_.len() > body_h {
        let skip = rows_.len() - body_h;
        lines.extend(rows_.into_iter().skip(skip));
    } else {
        let missing = body_h - rows_.len();
        lines.extend(rows_);
        for _ in 0..missing {
            lines.push(row(&[], cols, base, 0));
        }
    }
    let cur = if st.blink { "█" } else { " " };
    let text: String = ATTACH_COMPOSER.chars().take(st.typed).collect();
    let bw = cols - 4;
    lines.push(row(
        &[s(
            format!("╭{}╮", "─".repeat(bw - 2)),
            Some(CLI_EDGE),
            Some(base),
        )],
        cols,
        base,
        2,
    ));
    lines.push(row(
        &[
            s("│", Some(CLI_EDGE), Some(base)),
            s(" > ", Some(DIM), Some(base)),
            s(text.clone(), Some(TEXT), Some(base)),
            s(cur, Some(TEXT), Some(base)),
            s(
                pad(bw as isize - 7 - text.chars().count() as isize),
                None,
                Some(base),
            ),
            s("│", Some(CLI_EDGE), Some(base)),
        ],
        cols,
        base,
        2,
    ));
    lines.push(row(
        &[s(
            format!("╰{}╯", "─".repeat(bw - 2)),
            Some(CLI_EDGE),
            Some(base),
        )],
        cols,
        base,
        2,
    ));
    lines.push(row(
        &[
            s("? for shortcuts", Some(FT), Some(base)),
            s(
                "   ⏵⏵ accept edits on (shift+tab to cycle)",
                Some(FT),
                Some(base),
            ),
        ],
        cols,
        base,
        4,
    ));
    lines.push(row(
        &[s("▀".repeat(cols), Some(base), Some(VOID))],
        cols,
        VOID,
        0,
    ));
    while lines.len() < rows {
        lines.push(vrow(cols));
    }
    lines.truncate(rows);
    lines
}
