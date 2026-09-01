// ABOUTME: Task view — vertical session rail with manager/subordinate connectors beside
// ABOUTME: the working pane: native CLI chrome by default, ACP conduit view on toggle.

use crate::chrome::{keybar, masthead, PAD};
use crate::color::{
    harness_color, Rgb, AMB, AMB_GLASS, CLI_BG, CLI_EDGE, DIM, FT, GLASS, GLASS_HI, GLASS_IN, ION,
    KCAP, OK, TEXT, VOID, RESET,
};
use crate::fixture::Feed;
use crate::seg::{clip_segs, row, s, sb, seglen, vrow, Seg};
use crate::state::{St, View};
use crate::view::{HandoffRow, Hud, PaneBody, TaskView};

pub const RAIL_W: usize = 34;
pub const GAP: usize = 2;
pub const SPIN_GLYPHS: &[&str] = &["✳", "✻", "✽", "✻"];

pub fn ws_short(ws: &str, width: usize) -> String {
    if ws.chars().count() <= width {
        ws.to_string()
    } else {
        let tail: String = ws
            .chars()
            .rev()
            .take(width.saturating_sub(1))
            .collect::<Vec<_>>()
            .into_iter()
            .rev()
            .collect();
        format!("…{tail}")
    }
}

fn pad(n: isize) -> String {
    " ".repeat(n.max(0) as usize)
}

pub fn rail_lines(view: &TaskView, st: &St, h: usize) -> Vec<String> {
    let w = RAIL_W;
    let inner = w - 2;
    let rl = |segs: Vec<Seg>| row(&clip_segs(&segs, inner), w, GLASS, 2);

    let sel_i = st.rail_sel.min(view.sessions.len().saturating_sub(1));
    let mut top = vec![row(&[s("▄".repeat(w), Some(GLASS), Some(VOID))], w, VOID, 0)];
    top.push(rl(vec![sb(view.title.as_str(), Some(TEXT), None)]));
    top.push(rl(vec![s(view.repo.as_str(), Some(FT), None)]));
    top.push(rl(vec![]));
    top.push(rl(vec![s("TEAM", Some(FT), None)]));
    top.push(rl(vec![]));
    for (i, sess) in view.sessions.iter().enumerate() {
        let sel = st.rail_sel == i;
        let fill = if sel { GLASS_HI } else { GLASS };
        let live = sess.live;
        let dot = if sel && live {
            s("●", Some(ION), Some(fill))
        } else if live {
            s("●", Some(DIM), Some(fill))
        } else {
            s("○", Some(AMB), Some(fill))
        };
        if sess.depth == 0 && i > 0 {
            top.push(rl(vec![]));
        }
        let (name_row, h_ind) = if sess.depth == 0 {
            (
                vec![
                    s("▌", Some(harness_color(&sess.hkey)), Some(fill)),
                    s(" ", None, Some(fill)),
                    dot,
                    s(" ", None, Some(fill)),
                    Seg { text: sess.name.clone(), fg: Some(TEXT), bg: Some(fill), bold: sel },
                ],
                4usize,
            )
        } else {
            (
                vec![
                    s("▌", Some(harness_color(&sess.hkey)), Some(fill)),
                    s("  ", None, Some(fill)),
                    s("└─ ", Some(FT), Some(fill)),
                    dot,
                    s(" ", None, Some(fill)),
                    Seg { text: sess.name.clone(), fg: Some(TEXT), bg: Some(fill), bold: sel },
                ],
                7usize,
            )
        };
        top.push(row(&name_row, w, fill, 0));
        let sword = sess.state_word.as_str();
        let swcol = if sel && live { ION } else if live { FT } else { AMB };
        let mut sub = vec![
            s("▌", Some(harness_color(&sess.hkey)), Some(fill)),
            s(" ".repeat(h_ind), None, Some(fill)),
            s(sess.harness.as_str(), Some(FT), Some(fill)),
        ];
        sub.push(s(
            pad(w as isize - 2 - seglen(&sub) as isize - sword.chars().count() as isize),
            None,
            Some(fill),
        ));
        sub.push(s(sword, Some(swcol), Some(fill)));
        top.push(row(&sub, w, fill, 0));
    }
    top.push(rl(vec![]));
    top.push(rl(vec![
        s(" a ", Some(TEXT), Some(KCAP)),
        s(" subordinate   ", Some(FT), None),
        s(" m ", Some(TEXT), Some(KCAP)),
        s(" manager", Some(FT), None),
    ]));

    let mut bot = vec![rl(vec![])];
    let (sel_ws, sel_conduit) = view
        .sessions
        .get(sel_i)
        .map(|sess| (sess.ws.clone(), sess.conduit.to_lowercase()))
        .unwrap_or_default();
    let tele = [
        ("WORKSPACE", ws_short(&sel_ws, inner - 1)),
        ("CONDUIT", sel_conduit),
        ("TEAM", view.team_line.clone()),
    ];
    for (label, value) in tele {
        bot.push(rl(vec![s(label, Some(FT), None)]));
        bot.push(rl(vec![s(value, Some(DIM), None)]));
        bot.push(rl(vec![]));
    }
    bot.push(rl(vec![s(" Ctrl+G ", Some(TEXT), Some(KCAP)), s(" focus CLI", Some(FT), None)]));
    bot.push(rl(vec![]));
    bot.push(rl(vec![s(" d ", Some(TEXT), Some(KCAP)), s(" destroy task", Some(FT), None)]));
    bot.push(rl(vec![]));
    bot.push(row(&[s("▀".repeat(w), Some(GLASS), Some(VOID))], w, VOID, 0));

    let fill_n = h as isize - top.len() as isize - bot.len() as isize;
    let mut out = top;
    for _ in 0..fill_n.max(0) {
        out.push(rl(vec![]));
    }
    if fill_n >= 0 {
        out.extend(bot);
    } else {
        let keep = (h as isize - out.len() as isize).max(0) as usize;
        let skip = bot.len().saturating_sub(keep);
        out.extend(bot.into_iter().skip(skip));
    }
    out
}

pub fn feed_rows(
    entries: &[Feed],
    pw: usize,
    base: Rgb,
    accent: Rgb,
    native: bool,
) -> Vec<String> {
    let mut out = Vec::new();
    let fr = |segs: Vec<Seg>, b: Rgb| row(&clip_segs(&segs, pw - 4), pw, b, 2);
    for entry in entries {
        if matches!(entry, Feed::Meta(_)) && native {
            continue;
        }
        if matches!(entry, Feed::User(_) | Feed::Okay(_) | Feed::Ask(..)) {
            out.push(row(&[], pw, base, 0));
        }
        match entry {
            Feed::Meta(t) => {
                let mut chars = t.chars();
                let first = chars.next().map(|c| c.to_string()).unwrap_or_default();
                let rest: String = chars.collect();
                out.push(fr(
                    vec![s(first, Some(accent), Some(base)), s(rest, Some(FT), Some(base))],
                    base,
                ));
            }
            Feed::User(t) => out.push(fr(
                vec![
                    s("> ", Some(if native { DIM } else { FT }), Some(base)),
                    s(*t, Some(TEXT), Some(base)),
                ],
                base,
            )),
            Feed::Say(t) => out.push(fr(
                vec![s("⏺ ", Some(DIM), Some(base)), s(*t, Some(TEXT), Some(base))],
                base,
            )),
            Feed::Tool(name, arg) => out.push(fr(
                vec![
                    s("⏺ ", Some(OK), Some(base)),
                    s(*name, Some(TEXT), Some(base)),
                    s("(", Some(FT), Some(base)),
                    s(*arg, Some(DIM), Some(base)),
                    s(")", Some(FT), Some(base)),
                ],
                base,
            )),
            Feed::ResultLine(t) => out.push(fr(
                vec![s("  ⎿  ", Some(FT), Some(base)), s(*t, Some(DIM), Some(base))],
                base,
            )),
            Feed::Okay(t) => out.push(fr(
                vec![s("✓ ", Some(OK), Some(base)), s(*t, Some(DIM), Some(base))],
                base,
            )),
            Feed::Ask(q, tag) => {
                let mut segs = vec![
                    s("○ ", Some(AMB), Some(AMB_GLASS)),
                    s(*q, Some(TEXT), Some(AMB_GLASS)),
                ];
                segs.push(s(
                    pad(pw as isize - 4 - seglen(&segs) as isize - tag.chars().count() as isize),
                    None,
                    Some(AMB_GLASS),
                ));
                segs.push(s(*tag, Some(AMB), Some(AMB_GLASS)));
                out.push(row(&segs, pw, AMB_GLASS, 2));
            }
        }
    }
    out
}

fn spinner_tail(st: &St) -> String {
    let elapsed = (st.tick.saturating_sub(st.spin_t0)) / 10;
    format!(
        "(esc to interrupt · {elapsed}s · ↓ {:.1}k tokens)",
        st.tokens as f64 / 1000.0
    )
}

fn fit_body(mut lines: Vec<String>, rows_: Vec<String>, body_h: usize, pw: usize, base: Rgb) -> Vec<String> {
    if rows_.len() > body_h {
        let skip = rows_.len() - body_h;
        lines.extend(rows_.into_iter().skip(skip));
    } else {
        let missing = body_h - rows_.len();
        lines.extend(rows_);
        for _ in 0..missing {
            lines.push(row(&[], pw, base, 0));
        }
    }
    lines
}

pub fn pane_lines(view: &TaskView, st: &St, pw: usize, h: usize) -> Vec<String> {
    let sel_i = st.rail_sel.min(view.sessions.len().saturating_sub(1));
    let Some(sess) = view.sessions.get(sel_i) else {
        return empty_pane(pw, h, "No session attached — add the first Manager with  m");
    };
    let accent = harness_color(&sess.hkey);
    let b = GLASS_HI;
    let head = vec![
        sb(sess.name.as_str(), Some(TEXT), Some(b)),
        s(" · ", Some(FT), Some(b)),
        s(sess.harness.as_str(), Some(accent), Some(b)),
    ];
    let mut right: Vec<Seg> =
        vec![s(sess.conduit.as_str(), Some(FT), Some(b)), s("   ", None, Some(b))];
    if sess.live {
        right.push(s("● ", Some(ION), Some(b)));
        right.push(s("LIVE", Some(ION), Some(b)));
        right.push(s("   ", None, Some(b)));
    } else {
        right.push(s("◌ ", Some(AMB), Some(b)));
        right.push(s("QUEUED", Some(AMB), Some(b)));
        right.push(s("   ", None, Some(b)));
    }
    right.push(s("c ", Some(DIM), Some(b)));
    right.push(s(
        format!("view: {}", if st.view == View::Native { "NATIVE" } else { "ACP" }),
        Some(FT),
        Some(b),
    ));
    let gap = (pw as isize - 2 * PAD as isize - seglen(&head) as isize - seglen(&right) as isize)
        .max(1) as usize;
    let mut hrow = head;
    hrow.push(s(" ".repeat(gap), None, Some(b)));
    hrow.extend(right);
    let mut lines = vec![
        row(&[s("▄".repeat(pw), Some(b), Some(VOID))], pw, VOID, 0),
        row(&hrow, pw, b, PAD),
    ];

    match &view.body {
        PaneBody::Handoff { note, exchanges, cmd } => {
            lines.extend(handoff_pane(st, note, exchanges, cmd, pw, h - lines.len()));
            lines
        }
        PaneBody::Feed { entries, phrase } => {
            let native = st.view == View::Native;
            let base = if native { CLI_BG } else { GLASS };
            let body_h =
                (h as isize - lines.len() as isize - if native { 5 } else { 3 }).max(0) as usize;
            let mut rows_ = vec![row(&[], pw, base, 0)];
            if native {
                let hdr = if sess.hkey == "claude" {
                    format!("✻ {} · claude-fable-5 · {}", sess.harness, sess.ws)
                } else {
                    format!("✻ {} · {}", sess.harness, sess.ws)
                };
                let mut chars = hdr.chars();
                let first = chars.next().map(|c| c.to_string()).unwrap_or_default();
                let rest: String = chars.collect();
                rows_.push(row(
                    &[s(first, Some(accent), Some(base)), s(rest, Some(FT), Some(base))],
                    pw,
                    base,
                    2,
                ));
                rows_.push(row(&[], pw, base, 0));
            }
            rows_.extend(feed_rows(entries, pw, base, accent, native));
            rows_.push(row(&[], pw, base, 0));
            rows_.push(row(
                &[
                    s(
                        format!("{} ", SPIN_GLYPHS[st.spin_i % SPIN_GLYPHS.len()]),
                        Some(accent),
                        Some(base),
                    ),
                    s(*phrase, Some(TEXT), Some(base)),
                    s(format!("  {}", spinner_tail(st)), Some(FT), Some(base)),
                ],
                pw,
                base,
                2,
            ));
            lines = fit_body(lines, rows_, body_h, pw, base);
            let cur = if st.blink { "█" } else { " " };
            if native {
                let bw = pw - 4;
                lines.push(row(
                    &[s(format!("╭{}╮", "─".repeat(bw - 2)), Some(CLI_EDGE), Some(base))],
                    pw,
                    base,
                    2,
                ));
                lines.push(row(
                    &[
                        s("│", Some(CLI_EDGE), Some(base)),
                        s(" > ", Some(DIM), Some(base)),
                        s(cur, Some(TEXT), Some(base)),
                        s(" ".repeat(bw - 6), None, Some(base)),
                        s("│", Some(CLI_EDGE), Some(base)),
                    ],
                    pw,
                    base,
                    2,
                ));
                lines.push(row(
                    &[s(format!("╰{}╯", "─".repeat(bw - 2)), Some(CLI_EDGE), Some(base))],
                    pw,
                    base,
                    2,
                ));
                lines.push(row(&[s("? for shortcuts", Some(FT), Some(base))], pw, base, 4));
            } else {
                lines.push(row(
                    &[s("❯ ", Some(ION), Some(GLASS_HI)), s(cur, Some(ION), Some(GLASS_HI))],
                    pw,
                    GLASS_HI,
                    PAD,
                ));
                lines.push(row(&[], pw, GLASS_HI, 0));
            }
            lines.push(row(
                &[s("▀".repeat(pw), Some(if native { base } else { GLASS_HI }), Some(VOID))],
                pw,
                VOID,
                0,
            ));
            lines
        }
        PaneBody::Cells(cells) => {
            let base = CLI_BG;
            let body_h = (h as isize - lines.len() as isize - 1).max(0) as usize;
            for r in 0..body_h {
                match cells.get(r) {
                    Some(cr) => {
                        let mut segs: Vec<Seg> = Vec::new();
                        for c in cr {
                            match segs.last_mut() {
                                Some(last)
                                    if last.fg == c.fg
                                        && last.bg == c.bg
                                        && last.bold == c.bold =>
                                {
                                    last.text.push_str(&c.text)
                                }
                                _ => segs.push(Seg {
                                    text: c.text.clone(),
                                    fg: c.fg,
                                    bg: c.bg,
                                    bold: c.bold,
                                }),
                            }
                        }
                        lines.push(row(&segs, pw, base, 0));
                    }
                    None => lines.push(row(&[], pw, base, 0)),
                }
            }
            lines.push(row(&[s("▀".repeat(pw), Some(base), Some(VOID))], pw, VOID, 0));
            lines
        }
        PaneBody::Empty(msg) => {
            lines.extend(empty_pane(pw, h - lines.len(), msg));
            lines
        }
    }
}

fn empty_pane(pw: usize, h: usize, msg: &str) -> Vec<String> {
    let mut lines = Vec::new();
    let mid = h / 2;
    for i in 0..h.saturating_sub(1) {
        if i == mid {
            let padn = pw.saturating_sub(msg.chars().count()) / 2;
            lines.push(row(
                &[s(" ".repeat(padn), None, Some(GLASS)), s(msg, Some(FT), Some(GLASS))],
                pw,
                GLASS,
                0,
            ));
        } else {
            lines.push(row(&[], pw, GLASS, 0));
        }
    }
    lines.push(row(&[s("▀".repeat(pw), Some(GLASS), Some(VOID))], pw, VOID, 0));
    lines
}

pub fn handoff_pane(
    st: &St,
    note: &str,
    exchanges: &[HandoffRow],
    cmd: &str,
    pw: usize,
    h: usize,
) -> Vec<String> {
    let base = GLASS;
    let mut lines = vec![row(&[], pw, base, 0)];
    lines.push(row(
        &[s("◌ ", Some(AMB), Some(base)), s(note, Some(DIM), Some(base))],
        pw,
        base,
        2,
    ));
    lines.push(row(&[], pw, base, 0));
    for ex in exchanges {
        match ex.kind.as_str() {
            "sent" => lines.push(row(
                &[
                    s("❯ ", Some(DIM), Some(base)),
                    s(format!("{}   ", ex.ts), Some(FT), Some(base)),
                    s(ex.text.as_str(), Some(TEXT), Some(base)),
                ],
                pw,
                base,
                2,
            )),
            "ret" => lines.push(row(
                &[
                    s("  ⎿ ", Some(FT), Some(base)),
                    s(format!("{}   ", ex.ts), Some(FT), Some(base)),
                    s(ex.text.as_str(), Some(DIM), Some(base)),
                ],
                pw,
                base,
                2,
            )),
            _ => {
                lines.push(row(&[], pw, base, 0));
                lines.push(row(
                    &[
                        s("◌ ", Some(AMB), Some(GLASS_IN)),
                        s("Queued · resumes on completion", Some(AMB), Some(GLASS_IN)),
                    ],
                    pw,
                    GLASS_IN,
                    2,
                ));
            }
        }
    }
    lines.push(row(&[], pw, base, 0));
    let cur = if st.blink { "▍" } else { " " };
    lines.push(row(
        &[
            s("❯ ", Some(ION), Some(GLASS_IN)),
            s(cmd, Some(DIM), Some(GLASS_IN)),
            s(format!(" {cur}"), Some(ION), Some(GLASS_IN)),
        ],
        pw,
        GLASS_IN,
        2,
    ));
    while lines.len() < h.saturating_sub(1) {
        lines.push(row(&[], pw, base, 0));
    }
    lines.push(row(&[s("▀".repeat(pw), Some(base), Some(VOID))], pw, VOID, 0));
    lines.truncate(h);
    lines
}

pub fn frame(
    view: &TaskView,
    hud: &Hud,
    st: &St,
    cols: usize,
    rows: usize,
    clock: &str,
) -> Vec<String> {
    let hud_items: Vec<(&str, Rgb)> = hud.items.iter().map(|(t, c)| (t.as_str(), *c)).collect();
    let mut lines = masthead(cols, &view.crumb, &hud_items, hud.lamp.as_deref(), clock);
    lines.push(vrow(cols));
    let h = rows.saturating_sub(lines.len() + 4);
    let pw = cols - RAIL_W - GAP;
    let rail = rail_lines(view, st, h);
    let pane = pane_lines(view, st, pw, h);
    let gap = format!("{}{}", crate::color::bg(VOID), " ".repeat(GAP));
    for i in 0..h {
        let r = rail.get(i).cloned().unwrap_or_else(|| row(&[], RAIL_W, VOID, 0));
        let p = pane.get(i).cloned().unwrap_or_else(|| row(&[], pw, VOID, 0));
        lines.push(format!("{r}{gap}{p}{RESET}"));
    }
    lines.push(vrow(cols));
    lines.extend(keybar(
        cols,
        &[
            ("Enter", "Attach"),
            ("c", "Toggle view"),
            ("a", "Add Subordinate"),
            ("m", "Add Manager"),
            ("d", "Destroy"),
            ("T", "Home"),
            ("Ctrl+G", "Focus CLI"),
        ],
    ));
    lines.push(vrow(cols));
    lines.truncate(rows);
    lines
}
