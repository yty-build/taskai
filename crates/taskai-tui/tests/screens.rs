// ABOUTME: Screen-frame tests — every rendered line of every screen is exactly `cols`
// ABOUTME: cells at every size, and the chrome (masthead, keybar) sits where it must.

use taskai_tui::color::set_truecolor;
use taskai_tui::render::dump;
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
    for label in [" Enter ", " Open", " n ", " New task", " h ", " Harnesses", " ? "] {
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
    let text = frame.iter().map(|l| strip_sgr(l)).collect::<Vec<_>>().join("\n");
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
    let text = frame.iter().map(|l| strip_sgr(l)).collect::<Vec<_>>().join("\n");
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
