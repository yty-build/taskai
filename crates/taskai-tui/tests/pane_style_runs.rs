use taskai_tui::color::{bg, fg, Rgb, BOLD, CLI_BG, RESET};
use taskai_tui::screens::pane::{cell_row, PaneCell};

// Keep the pre-optimization emitter as an independent semantic reference.
fn legacy_cell_row(cells: &[PaneCell], width: usize, base: Rgb) -> String {
    let mut out = String::new();
    let mut used = 0;
    for cell in cells {
        let cell_width = cell.width.max(1);
        if used >= width || cell_width > width - used {
            break;
        }
        out.push_str(&bg(cell.bg.unwrap_or(base)));
        if let Some(color) = cell.fg {
            out.push_str(&fg(color));
        }
        if cell.bold {
            out.push_str(BOLD);
        }
        if cell.italic {
            out.push_str("\x1b[3m");
        }
        if cell.underline {
            out.push_str("\x1b[4m");
        }
        if cell.inverse {
            out.push_str("\x1b[7m");
        }
        out.push_str(&cell.text);
        out.push_str(RESET);
        used += cell_width;
    }
    if used < width {
        out.push_str(&bg(base));
        out.push_str(&" ".repeat(width - used));
    }
    out.push_str(RESET);
    out
}

fn cell(text: &str, width: usize, flags: u8, fg: Option<Rgb>, bg: Option<Rgb>) -> PaneCell {
    PaneCell {
        text: text.into(),
        fg,
        bg,
        bold: flags & 1 != 0,
        italic: flags & 2 != 0,
        underline: flags & 4 != 0,
        inverse: flags & 8 != 0,
        width,
    }
}

fn equivalent(cells: &[PaneCell], width: usize, base: Rgb) {
    let mut expected = vt100::Parser::new(2, (width + 2) as u16, 0);
    let mut actual = vt100::Parser::new(2, (width + 2) as u16, 0);
    expected.process(legacy_cell_row(cells, width, base).as_bytes());
    actual.process(cell_row(cells, width, base).as_bytes());
    // Appending an unstyled marker checks that no row-end style escapes into
    // the next rendered row. Extra columns also reveal accidental width drift.
    expected.process(b"\r\n!");
    actual.process(b"\r\n!");
    assert_eq!(
        actual.screen().cursor_position(),
        expected.screen().cursor_position()
    );
    for row in 0..2 {
        for col in 0..(width + 2) as u16 {
            let a = actual.screen().cell(row, col).unwrap();
            let e = expected.screen().cell(row, col).unwrap();
            assert_eq!(
                (
                    a.contents(),
                    a.fgcolor(),
                    a.bgcolor(),
                    a.bold(),
                    a.italic(),
                    a.underline(),
                    a.inverse(),
                    a.is_wide(),
                    a.is_wide_continuation(),
                ),
                (
                    e.contents(),
                    e.fgcolor(),
                    e.bgcolor(),
                    e.bold(),
                    e.italic(),
                    e.underline(),
                    e.inverse(),
                    e.is_wide(),
                    e.is_wide_continuation(),
                ),
                "cell ({row},{col}) at width {width}"
            );
        }
    }
}

#[test]
fn every_style_transition_matches_the_legacy_terminal_grid() {
    for base in [CLI_BG, (14, 32, 96)] {
        for previous in 0..16 {
            for next in 0..16 {
                for fg in [None, Some((12, 34, 56))] {
                    for bg in [None, Some(base), Some((65, 43, 21))] {
                        let cells = [
                            cell("A", 1, previous, fg, bg),
                            cell("B", 1, previous, fg, bg),
                            cell("e\u{301}", 1, next, None, None),
                            cell("界", 2, next, None, None),
                            cell("Z", 1, next, fg, bg),
                        ];
                        for width in [0, 1, 3, 4, 5, 6, 8] {
                            equivalent(&cells, width, base);
                        }
                    }
                }
            }
        }
    }
}

#[test]
fn clipping_padding_and_zero_width_metadata_match_legacy_cells() {
    equivalent(&[], 0, CLI_BG);
    equivalent(&[], 9, CLI_BG);
    for width in 0..9 {
        equivalent(
            &[
                cell("X", 0, 15, None, Some((1, 2, 3))),
                cell("✨", 2, 15, None, Some((1, 2, 3))),
                cell("e\u{301}", 1, 0, None, None),
            ],
            width,
            CLI_BG,
        );
    }
    equivalent(&[cell("X", usize::MAX, 0, None, None)], 8, CLI_BG);
}

#[test]
fn row_starts_from_default_attributes_and_ends_with_unstyled_base_padding() {
    let mut base = vt100::Parser::new(1, 1, 0);
    base.process(format!("{} ", bg(CLI_BG)).as_bytes());
    let expected_bg = base.screen().cell(0, 0).unwrap().bgcolor();
    let mut parser = vt100::Parser::new(1, 5, 0);
    parser.process(b"\x1b[1;3;4;7;38;2;99;88;77m");
    parser.process(cell_row(&[cell("X", 1, 0, None, None)], 5, CLI_BG).as_bytes());
    for col in 0..5 {
        let c = parser.screen().cell(0, col).unwrap();
        assert_eq!(c.fgcolor(), vt100::Color::Default);
        assert_eq!(c.bgcolor(), expected_bg);
        assert!(!c.bold() && !c.italic() && !c.underline() && !c.inverse());
    }
}

#[test]
fn equal_style_rows_have_a_text_sized_byte_budget() {
    for width in [110, 173, 209, 300] {
        for colored in [false, true] {
            let cells: Vec<_> = (0..width)
                .map(|_| {
                    cell(
                        "X",
                        1,
                        if colored { 15 } else { 0 },
                        colored.then_some((128, 192, 255)),
                        colored.then_some((10, 20, 30)),
                    )
                })
                .collect();
            let rendered = cell_row(&cells, width, CLI_BG);
            assert!(
                rendered.len() <= width + 80,
                "per-cell style overhead returned"
            );
            assert!(rendered.len() * 10 < legacy_cell_row(&cells, width, CLI_BG).len());
            equivalent(&cells, width, CLI_BG);
        }
    }
}

#[test]
fn implicit_and_explicit_base_background_share_the_same_style_run() {
    let cells = [
        cell("A", 1, 0, None, None),
        cell("B", 1, 0, None, Some(CLI_BG)),
        cell("C", 1, 0, None, None),
    ];
    let output = cell_row(&cells, 3, CLI_BG);
    assert!(output.contains("ABC"));
    assert_eq!(output.matches(&bg(CLI_BG)).count(), 1);
    equivalent(&cells, 3, CLI_BG);
}
