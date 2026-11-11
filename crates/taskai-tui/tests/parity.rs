// ABOUTME: Parity tests — rendered frames must match the golden frames cell-for-cell
// ABOUTME: (glyph, fg, bg, bold) at every size. ANSI parsed through vt100 is ground truth.

use taskai_tui::color::set_truecolor;
use taskai_tui::render::dump;

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

fn grid(lines: &[String], cols: usize, rows: usize) -> vt100::Parser {
    let mut parser = vt100::Parser::new(rows as u16, cols as u16, 0);
    parser.process(lines.join("\r\n").as_bytes());
    parser
}

fn fixture_dir() -> String {
    format!("{}/../../tests/fixtures", env!("CARGO_MANIFEST_DIR"))
}

// Expectation: every cell of every screen at every size equals the golden frame —
// contents, fg, bg, bold. Why: the golden frames are the approved terminal design, and
// nothing else enforces it. Value: any drift names the exact cell that moved.
#[test]
fn frames_match_golden() {
    set_truecolor(true);
    if std::env::var("TASKAI_BLESS").is_ok() {
        eprintln!("blessing: skipping comparison, see rerecord_golden_frames");
        return;
    }
    let dir = fixture_dir();
    for screen in SCREENS {
        for &(cols, rows) in SIZES {
            let path = format!("{dir}/{screen}-{cols}x{rows}.ans");
            let raw = std::fs::read_to_string(&path)
                .unwrap_or_else(|e| panic!("missing golden frame {path}: {e}"));
            let want_lines: Vec<String> = raw.split('\n').map(str::to_string).collect();
            let got_lines = dump(screen, cols, rows).unwrap();
            let want = grid(&want_lines, cols, rows);
            let got = grid(&got_lines, cols, rows);
            let mut diffs = 0;
            for r in 0..rows as u16 {
                for c in 0..cols as u16 {
                    let w = want.screen().cell(r, c).expect("want cell");
                    let g = got.screen().cell(r, c).expect("got cell");
                    if w.contents() != g.contents()
                        || w.fgcolor() != g.fgcolor()
                        || w.bgcolor() != g.bgcolor()
                        || w.bold() != g.bold()
                    {
                        if diffs < 5 {
                            eprintln!(
                                "{screen} {cols}x{rows} cell ({r},{c}): \
                                 want {:?} fg{:?} bg{:?} b{} · got {:?} fg{:?} bg{:?} b{}",
                                w.contents(),
                                w.fgcolor(),
                                w.bgcolor(),
                                w.bold(),
                                g.contents(),
                                g.fgcolor(),
                                g.bgcolor(),
                                g.bold()
                            );
                        }
                        diffs += 1;
                    }
                }
            }
            assert_eq!(
                diffs, 0,
                "{screen} {cols}x{rows}: {diffs} cells differ — if this change to the design \
                 is intended, re-record with TASKAI_BLESS=1 and review the diff"
            );
        }
    }
}

// Expectation: with TASKAI_BLESS set, the golden frames are re-recorded from the current
// renderer and the change lands in the diff for review. Why: an intentional design change
// otherwise has no way to update its own contract. Value: changing the approved look is a
// deliberate, reviewable act rather than a dead end.
#[test]
fn rerecord_golden_frames() {
    if std::env::var("TASKAI_BLESS").is_err() {
        return;
    }
    set_truecolor(true);
    let dir = fixture_dir();
    let mut written = 0;
    for screen in SCREENS {
        for &(cols, rows) in SIZES {
            let frame = dump(screen, cols, rows).expect(screen);
            std::fs::write(
                format!("{dir}/{screen}-{cols}x{rows}.ans"),
                frame.join("\n"),
            )
            .expect("write golden frame");
            written += 1;
        }
    }
    eprintln!(
        "blessed {written} golden frames — review `git diff tests/fixtures` before committing"
    );
}
