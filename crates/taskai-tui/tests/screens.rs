// ABOUTME: Screen-frame tests — every rendered line of every screen is exactly `cols`
// ABOUTME: cells at every size, and the chrome (masthead, keybar) sits where it must.

use taskai_tui::color::{set_truecolor, CLI_BG};
use taskai_tui::render::{dump, paint_frame, resize_frame};
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
            width: 2,
        },
        PaneCell {
            text: "X".into(),
            fg: None,
            bg: None,
            bold: false,
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
        " h ",
        " Harnesses",
        " ? ",
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
