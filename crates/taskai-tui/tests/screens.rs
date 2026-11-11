// ABOUTME: Screen-frame tests — every rendered line of every screen is exactly `cols`
// ABOUTME: cells at every size, and the chrome (masthead, keybar) sits where it must.

use taskai_tui::color::{set_truecolor, AMB, CLI_BG};
use taskai_tui::render::{dump, paint_frame, paint_update, resize_frame};
use taskai_tui::screens::pane::{cell_row, PaneCell};
use taskai_tui::seg::{cell_count, strip_sgr};

const SIZES: &[(usize, usize)] = &[(110, 30), (209, 51), (150, 42)];
const SCREENS: &[&str] = &[
    "boot",
    "home",
    "harnesses",
    "task",
    "attach",
    "attention",
    "newtask",
    "subordinate",
    "destroy",
    "help",
    "pulse",
    "messages",
];

// Expectation: for each screen at each size, the frame has exactly `rows` lines and every
// line paints exactly `cols` cells. Why: the prototype's verify() caught every layout bug
// this way. Value: regressions surface as a failing number, not a shredded screen.
#[test]
fn every_line_exact_width() {
    set_truecolor(true);
    for &screen in SCREENS {
        for &(cols, rows) in SIZES {
            let frame = dump(screen, cols, rows).expect(screen);
            assert_eq!(frame.len(), rows, "{screen} {cols}x{rows} line count");
            for (i, line) in frame.iter().enumerate() {
                assert_eq!(
                    cell_count(line),
                    cols,
                    "{screen} {cols}x{rows} line {i}: {:?}",
                    strip_sgr(line)
                );
            }
        }
    }
}

// Expectation: the live painter addresses every row directly and never relies on
// newline behavior after an exact-width row. Why: some terminals auto-wrap the
// last cell before also processing CRLF, making each refresh creep down the
// screen. Value: masthead, native pane, and overlays remain pinned indefinitely.
#[test]
fn frame_painter_is_stable_across_repeated_tall_redraws() {
    set_truecolor(true);
    let (cols, rows) = (209, 80);
    let frame = dump("subordinate", cols, rows).unwrap();
    let painted = paint_frame(&frame, None);

    assert!(
        !painted.contains('\n'),
        "frame painter must not emit line feeds"
    );
    assert!(
        !painted.contains('\r'),
        "frame painter must not emit carriage returns"
    );
    for row_number in 1..=rows {
        assert!(
            painted.contains(&format!("\x1b[{row_number};1H")),
            "missing absolute cursor address for row {row_number}"
        );
    }

    let mut parser = vt100::Parser::new(rows as u16, cols as u16, 0);
    for _ in 0..64 {
        parser.process(painted.as_bytes());
    }
    let screen = parser.screen();
    assert_eq!(screen.cell(1, 3).unwrap().contents(), "T");
    assert!(screen.contents().contains("Add subordinate — under Lead"));
    assert!(screen.contents().contains("Enter  Create"));
}

// Expectation: painting restores the requested host cursor after all cells and
// hides it when navigation owns the keyboard. Why: the child CLI cursor is
// terminal state, not a glyph in its grid. Value: focus and caret never lie.
#[test]
fn frame_painter_restores_or_hides_the_host_cursor() {
    let frame = dump("attach", 110, 30).unwrap();

    let mut visible = vt100::Parser::new(30, 110, 0);
    visible.process(paint_frame(&frame, Some((12, 34))).as_bytes());
    assert_eq!(visible.screen().cursor_position(), (12, 34));
    assert!(!visible.screen().hide_cursor());

    let mut hidden = vt100::Parser::new(30, 110, 0);
    hidden.process(paint_frame(&frame, None).as_bytes());
    assert!(hidden.screen().hide_cursor());
}

// Expectation: an idle frame emits no terminal bytes, and a caret move does not
// toggle visibility. Why: the live loop polls frequently even when the provider is
// idle; repainting or hide/show on every poll makes native CLI cursors flicker.
// Value: an idle or moving provider caret keeps the terminal's natural blink phase.
#[test]
fn frame_painter_skips_idle_frames_and_minimizes_cursor_only_changes() {
    let frame = dump("attach", 110, 30).unwrap();

    assert_eq!(
        paint_update(&frame, None, None, None),
        paint_frame(&frame, None)
    );
    assert_eq!(paint_update(&frame, None, Some(&frame), None), "");
    assert_eq!(
        paint_update(&frame, Some((12, 34)), Some(&frame), Some((12, 34))),
        ""
    );
    assert_eq!(
        paint_update(&frame, Some((12, 35)), Some(&frame), Some((12, 34))),
        "\x1b[13;36H"
    );
    assert_eq!(
        paint_update(&frame, None, Some(&frame), Some((12, 34))),
        "\x1b[?25l"
    );
    assert_eq!(
        paint_update(&frame, Some((12, 34)), Some(&frame), None),
        "\x1b[13;35H\x1b[?25h"
    );

    let mut changed = frame.clone();
    changed[0].push('x');
    let repaint = paint_update(&changed, Some((12, 34)), Some(&frame), Some((12, 34)));
    assert!(repaint.starts_with("\x1b[?2026h\x1b[1;1H"));
    assert!(!repaint.contains("\x1b[2;1H"));
    assert!(!repaint.contains("\x1b[?25l"));
    assert!(!repaint.contains("\x1b[?25h"));
    assert!(repaint.ends_with("\x1b[13;35H\x1b[?2026l"));

    let hidden = paint_update(&changed, None, Some(&frame), Some((12, 34)));
    assert!(hidden.starts_with("\x1b[?2026h\x1b[?25l"));

    let shown = paint_update(&changed, Some((12, 34)), Some(&frame), None);
    assert!(!shown.contains("\x1b[?25l"));
    assert!(shown.ends_with("\x1b[13;35H\x1b[?25h\x1b[?2026l"));
}

// Expectation: hosted CLI rows retain the parser's physical cell widths.
// Why: an emoji is one Rust char but two terminal columns; counting chars adds
// an extra padding cell and can reintroduce right-margin scrolling. Value:
// native provider output and its cursor remain aligned.
#[test]
fn hosted_cli_rows_preserve_wide_cell_geometry() {
    let cells = vec![
        PaneCell {
            text: "✨".into(),
            fg: None,
            bg: None,
            bold: false,
            italic: false,
            underline: false,
            inverse: false,
            width: 2,
        },
        PaneCell {
            text: "X".into(),
            fg: None,
            bg: None,
            bold: false,
            italic: false,
            underline: false,
            inverse: false,
            width: 1,
        },
    ];
    let rendered = cell_row(&cells, 4, CLI_BG);
    let mut parser = vt100::Parser::new(1, 4, 0);
    parser.process(rendered.as_bytes());
    assert_eq!(parser.screen().cell(0, 0).unwrap().contents(), "✨");
    assert!(parser.screen().cell(0, 1).unwrap().is_wide_continuation());
    assert_eq!(parser.screen().cell(0, 2).unwrap().contents(), "X");
}

#[test]
fn hosted_cli_preserves_selection_and_text_attributes() {
    let cells = vec![PaneCell {
        text: "X".into(),
        fg: Some((12, 34, 56)),
        bg: Some((65, 43, 21)),
        bold: true,
        italic: true,
        underline: true,
        inverse: true,
        width: 1,
    }];
    let rendered = cell_row(&cells, 3, CLI_BG);
    let mut parser = vt100::Parser::new(1, 3, 0);
    parser.process(rendered.as_bytes());
    let cell = parser.screen().cell(0, 0).unwrap();
    assert!(cell.bold());
    assert!(cell.italic());
    assert!(cell.underline());
    assert!(cell.inverse());
    let padding = parser.screen().cell(0, 1).unwrap();
    assert!(!padding.bold());
    assert!(!padding.inverse());
    assert!(!padding.italic());
    assert!(!padding.underline());
}

// Expectation: a compact physical terminal receives a compact frame, never a
// fabricated 110x30 canvas. Why: oversized painting wraps before the user can
// resize and can create the same bottom-anchored corruption. Value: resize is
// recoverable without damaging live state.
#[test]
fn compact_terminal_uses_its_real_viewport() {
    let frame = resize_frame(80, 20);
    assert_eq!(frame.len(), 20);
    assert!(frame.iter().all(|line| cell_count(line) == 80));
    let text = frame
        .iter()
        .map(|line| strip_sgr(line))
        .collect::<Vec<_>>()
        .join("\n");
    assert!(text.contains("TASKAI · RESIZE NEEDED"));
    assert!(text.contains("Required  110 × 30"));
}

#[test]
fn resize_screen_keeps_escape_recovery_visible_even_at_twenty_columns() {
    for (cols, rows) in [(20, 10), (40, 12), (80, 20)] {
        let frame = resize_frame(cols, rows);
        assert_eq!(frame.len(), rows);
        assert!(frame.iter().all(|line| cell_count(line) == cols));
        let text = frame
            .iter()
            .map(|line| strip_sgr(line))
            .collect::<Vec<_>>()
            .join("\n");
        assert!(text.contains("Esc Back/Cancel"));
        assert!(text.contains("Ctrl+G Navigation"));
        assert!(text.contains("q Leave (navigation)"));
    }
}

// Expectation: home opens with the three-row masthead — soft top, HUD strip with wordmark,
// crumb, canon counts and fixed clock, soft bottom. Why: the masthead is the product's
// first impression and the HUD vocabulary is canon. Value: chrome drift fails fast.
#[test]
fn home_masthead_layout() {
    set_truecolor(true);
    let frame = dump("home", 110, 30).unwrap();
    assert!(strip_sgr(&frame[0]).chars().all(|c| c == '▄'));
    let hud = strip_sgr(&frame[1]);
    assert!(hud.contains("TASKAI"), "{hud}");
    assert!(hud.contains("PORTFOLIO"), "{hud}");
    assert!(hud.contains("4 LIVE"), "{hud}");
    assert!(hud.contains("ATTENTION · 1"), "{hud}");
    assert!(hud.contains("12:00:00"), "{hud}");
    assert!(strip_sgr(&frame[2]).chars().all(|c| c == '▀'));
}

// Expectation: the keybar is a hairline rule then centered keycap chips, third and second
// line from the frame bottom. Why: T rejected plain-text shortcuts; the keycap grammar is
// part of the approved design. Value: the keybar can't silently regress.
#[test]
fn home_keybar_layout() {
    set_truecolor(true);
    let (cols, rows) = (110, 30);
    let frame = dump("home", cols, rows).unwrap();
    assert!(strip_sgr(&frame[rows - 3]).chars().all(|c| c == '─'));
    let keys = strip_sgr(&frame[rows - 2]);
    for label in [
        " Enter ",
        " Open",
        " n ",
        " New task",
        " s ",
        " Settings",
        " F1 ",
        " Setup helper",
        " ? ",
        " q ",
        " Leave",
    ] {
        assert!(keys.contains(label), "missing {label:?} in {keys:?}");
    }
}

// Expectation: home shows the three fixture tasks with canon states and the ghost row.
// Why: the board is the portfolio at a glance; states are canon vocabulary.
// Value: content and state wording verified together.
#[test]
fn home_board_content() {
    set_truecolor(true);
    let frame = dump("home", 110, 30).unwrap();
    let text = frame
        .iter()
        .map(|l| strip_sgr(l))
        .collect::<Vec<_>>()
        .join("\n");
    for needle in [
        "Repository evaluation",
        "WORKING",
        "Production release",
        " ATTENTION ",
        "API migration",
        "COMPLETE",
        "+ New task",
        " n ",
        "Lead · Claude Code",
        "Delivery · Codex",
    ] {
        assert!(text.contains(needle), "missing {needle:?}");
    }
}

// Expectation: boot renders the centered slab — wordmark, tagline, all five probes with
// READY/SETUP, and the finished meter caption. Why: boot is the first frame a user ever
// sees and the probe truthfulness rule starts here. Value: the launch moment is pinned.
#[test]
fn boot_slab_content() {
    set_truecolor(true);
    let frame = dump("boot", 110, 30).unwrap();
    let text = frame
        .iter()
        .map(|l| strip_sgr(l))
        .collect::<Vec<_>>()
        .join("\n");
    for needle in [
        "T A S K A I",
        "Your agents. Your terminal. One boundary.",
        "Claude Code",
        "Codex",
        "OpenCode",
        "Grok CLI",
        "Aider",
        "SETUP",
        "4 of 5 harnesses ready",
    ] {
        assert!(text.contains(needle), "missing {needle:?}");
    }
    assert!(text.matches("✓ READY").count() >= 4, "ready probes");
}

// Expectation: dump() rejects unknown screen names. Why: test tooling must fail loudly,
// not render a default screen for a typo. Value: honest tooling.
#[test]
fn dump_rejects_unknown_screen() {
    assert!(dump("nope", 110, 30).is_none());
}

fn screen_grid(frame: &[String], cols: usize, rows: usize) -> vt100::Parser {
    let mut parser = vt100::Parser::new(rows as u16, cols as u16, 0);
    parser.process(frame.join("\r\n").as_bytes());
    parser
}

/// Foreground colour of the first cell of every occurrence of `needle` in the painted frame.
fn fg_of(frame: &[String], cols: usize, rows: usize, needle: &str) -> Vec<vt100::Color> {
    let parser = screen_grid(frame, cols, rows);
    let screen = parser.screen();
    let mut found = Vec::new();
    for r in 0..rows as u16 {
        let text: String = (0..cols as u16)
            .map(|c| {
                let cell = screen.cell(r, c).expect("cell");
                let glyph = cell.contents();
                if glyph.is_empty() {
                    ' '
                } else {
                    glyph.chars().next().unwrap()
                }
            })
            .collect();
        let mut from = 0;
        while let Some(at) = text[from..].find(needle) {
            let col = text[..from + at].chars().count() as u16;
            found.push(screen.cell(r, col).expect("cell").fgcolor());
            from += at + needle.len();
        }
    }
    found
}

fn amber() -> vt100::Color {
    vt100::Color::Rgb(AMB.0, AMB.1, AMB.2)
}

// Expectation: in the pulse frame, the session waiting on an approval reads
// "WAITING · approval" in amber, in the rail and in the pane header, while the busy
// session's "LIVE · busy" and the idle session's "LIVE · idle" are not amber. Why: an
// approval nobody can see is a hang. Value: working and stuck are told apart at a glance,
// without opening the pane.
#[test]
fn waiting_session_is_amber_and_distinct_from_busy() {
    set_truecolor(true);
    let (cols, rows) = (150, 42);
    let frame = dump("pulse", cols, rows).expect("pulse");
    let waiting = fg_of(&frame, cols, rows, "WAITING · approval");
    assert_eq!(waiting.len(), 2, "rail and pane header both show the wait");
    assert!(waiting.iter().all(|c| *c == amber()), "{waiting:?}");
    let busy = fg_of(&frame, cols, rows, "LIVE · busy");
    assert_eq!(
        busy.len(),
        1,
        "the busy session shows its activity once, in the rail"
    );
    assert!(busy.iter().all(|c| *c != amber()), "{busy:?}");
    let idle = fg_of(&frame, cols, rows, "LIVE · idle");
    assert_eq!(idle.len(), 1);
    assert!(idle.iter().all(|c| *c != amber()), "{idle:?}");
}

// Expectation: the message log overlay names the task, lists each logged letter as time,
// sender → recipient, outcome and the first line of its body, and marks a permission-mode
// mismatch in amber. Why: a log nobody can read is not a pulse. Value: who asked whom for
// what, and which letter crossed modes, is visible without opening any session.
#[test]
fn message_log_overlay_lists_letters_and_marks_mode_mismatch() {
    set_truecolor(true);
    let (cols, rows) = (150, 42);
    let frame = dump("messages", cols, rows).expect("messages");
    let text = frame
        .iter()
        .map(|l| strip_sgr(l))
        .collect::<Vec<_>>()
        .join("\n");
    for needle in [
        "Message log · Repository evaluation",
        "17:02:11",
        "claude-lead → codex-delivery",
        "delivered",
        "Land the retry/backoff branch behind the release gate.",
        "grok-verification → claude-lead",
        "dropped hop-runaway",
        "mode mismatch",
        "Esc",
        "Close",
    ] {
        assert!(text.contains(needle), "missing {needle:?} in\n{text}");
    }
    let marks = fg_of(&frame, cols, rows, "mode mismatch");
    assert_eq!(marks.len(), 1, "one letter crossed modes");
    assert_eq!(marks[0], amber());
}
