// ABOUTME: Parity tests — Rust frames must match the Python prototype cell-for-cell
// ABOUTME: (glyph, fg, bg, bold) at every size. ANSI parsed through vt100 is ground truth.

use taskai_tui::color::set_truecolor;
use taskai_tui::render::dump;

const SIZES: &[(usize, usize)] = &[(110, 30), (209, 51), (150, 42)];

fn grid(lines: &[String], cols: usize, rows: usize) -> vt100::Parser {
    let mut parser = vt100::Parser::new(rows as u16, cols as u16, 0);
    parser.process(lines.join("\r\n").as_bytes());
    parser
}

// Expectation: for boot and home at each size, every cell of the Rust frame equals the
// prototype fixture — contents, fg, bg, bold. Why: the prototype is the approved design;
// the production TUI is its replica. Value: any drift names the exact cell that moved.
#[test]
fn frames_match_prototype_fixtures() {
    set_truecolor(true);
    let fixture_dir = format!("{}/../../tests/fixtures", env!("CARGO_MANIFEST_DIR"));
    for screen in [
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
    ] {
        for &(cols, rows) in SIZES {
            let path = format!("{fixture_dir}/{screen}-{cols}x{rows}.ans");
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
                                w.contents(), w.fgcolor(), w.bgcolor(), w.bold(),
                                g.contents(), g.fgcolor(), g.bgcolor(), g.bold()
                            );
                        }
                        diffs += 1;
                    }
                }
            }
            assert_eq!(diffs, 0, "{screen} {cols}x{rows}: {diffs} cells differ");
        }
    }
}
