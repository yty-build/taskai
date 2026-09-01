// ABOUTME: Tests for the FILL palette and segment/row engine — token values match the
// ABOUTME: prototype exactly; rows always paint exactly `width` cells.

use taskai_tui::color::{
    bg, c256, dull, fg, harness_color, set_truecolor, AMB, AMB_GLASS, CLAUDE, CLI_BG, CLI_EDGE,
    CODEX, DIM, FT, GLASS, GLASS_HI, GLASS_IN, GROK, HAIR, ION, KCAP, OK, OPENC, RED, RED_GLASS,
    RESET, TEXT, VOID,
};
use taskai_tui::seg::{cell_count, clip_segs, row, s, sb, seglen, strip_sgr};

// Expectation: every token equals the value in prototype/fill_system.py and the prototype's
// local constants. Why: the FILL chroma budget is the brand; a drifted token breaks parity
// silently. Value: parity failures point at layout, never at palette drift.
#[test]
fn tokens_match_prototype() {
    assert_eq!(VOID, (4, 6, 10));
    assert_eq!(TEXT, (233, 238, 246));
    assert_eq!(DIM, (126, 138, 160));
    assert_eq!(FT, (102, 115, 140));
    assert_eq!(ION, (124, 214, 255));
    assert_eq!(AMB, (255, 194, 75));
    assert_eq!(OK, (127, 216, 164));
    assert_eq!(RED, (242, 108, 108));
    assert_eq!(CLAUDE, (217, 131, 107));
    assert_eq!(CODEX, (143, 179, 255));
    assert_eq!(OPENC, (143, 227, 192));
    assert_eq!(GROK, (201, 207, 219));
    assert_eq!(GLASS, (17, 24, 38));
    assert_eq!(GLASS_HI, (25, 35, 54));
    assert_eq!(GLASS_IN, (9, 13, 22));
    assert_eq!(AMB_GLASS, (70, 53, 21));
    assert_eq!(RED_GLASS, (64, 24, 24));
    assert_eq!(KCAP, (27, 39, 64));
    assert_eq!(HAIR, (30, 41, 64));
    assert_eq!(CLI_BG, (7, 10, 17));
    assert_eq!(CLI_EDGE, (58, 66, 84));
    assert_eq!(harness_color("claude"), CLAUDE);
    assert_eq!(harness_color("codex"), CODEX);
    assert_eq!(harness_color("opencode"), OPENC);
    assert_eq!(harness_color("grok"), GROK);
    assert_eq!(harness_color("aider"), DIM);
}

// Expectation: the 256-color fallback uses the prototype's cube formula.
// Why: on non-truecolor terminals both implementations must degrade identically.
// Value: one formula, verified, never re-derived by eye.
#[test]
fn c256_matches_prototype_formula() {
    assert_eq!(c256(VOID), 16);
    assert_eq!(c256(TEXT), 231);
    assert_eq!(c256((255, 0, 0)), 196);
}

// Expectation: dull() pulls a color toward the void with truncating arithmetic like
// Python's int(). Why: overlay dimming must produce identical RGB on both sides.
// Value: dimmed overlays compare cell-for-cell in parity tests.
#[test]
fn dull_truncates_toward_void() {
    assert_eq!(dull(ION, 0.30), (40, 68, 83));
    assert_eq!(dull(VOID, 0.5), VOID);
}

// Expectation: a row always paints exactly `width` cells — short segs pad with base_bg,
// long segs truncate. Why: the FILL grammar is background paint; one wrong cell shreds
// a slab. Value: layout bugs become failing numbers.
#[test]
fn row_paints_exact_width() {
    set_truecolor(true);
    let r = row(&[s("hi", Some(ION), Some(GLASS))], 20, VOID, 0);
    assert_eq!(cell_count(&r), 20);
    assert!(r.ends_with(RESET));

    let long = row(&[s("abcdefghij", Some(TEXT), None)], 4, VOID, 0);
    assert_eq!(cell_count(&long), 4);
    assert_eq!(strip_sgr(&long), "abcd");

    let ind = row(&[s("x", Some(TEXT), None)], 10, GLASS, 3);
    assert_eq!(cell_count(&ind), 10);
    assert_eq!(strip_sgr(&ind), "   x      ");
}

// Expectation: emission order per segment is bg, fg, bold, text, RESET — the prototype's
// byte order. Why: parity compares SGR-parsed cells; attribute order affects nothing
// visually but keeping it identical keeps raw diffs readable. Value: debuggable fixtures.
#[test]
fn row_emits_prototype_byte_order() {
    set_truecolor(true);
    let r = row(&[sb("A", Some(TEXT), Some(GLASS))], 1, VOID, 0);
    let expected = format!("{}{}\u{1b}[1mA{}", bg(GLASS), fg(TEXT), RESET);
    assert!(r.starts_with(&expected), "got: {r:?}");
}

// Expectation: clip_segs truncates a seg list to width cells and marks overflow with a
// trailing ellipsis. Why: rail rows clip long names without painting past the slab.
// Value: no rail overflow at any terminal width.
#[test]
fn clip_appends_ellipsis_on_overflow() {
    let segs = vec![s("Repository evaluation", Some(TEXT), None)];
    let clipped = clip_segs(&segs, 10);
    assert_eq!(seglen(&clipped), 10);
    assert!(clipped.last().unwrap().text.ends_with('…'));

    let fits = clip_segs(&segs, 30);
    assert_eq!(seglen(&fits), 21);
}
