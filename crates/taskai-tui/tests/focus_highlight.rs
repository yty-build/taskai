// ABOUTME: Focus has one steady owner; selection is retained without repainting
// ABOUTME: native terminal cells, moving the grid, or changing provider identity.

use taskai_tui::chrome::focus_edge;
use taskai_tui::color::{set_truecolor, GLASS_HI, GLASS_IN, ION};
use taskai_tui::fixture;
use taskai_tui::input::Focus;
use taskai_tui::render::paint_frame;
use taskai_tui::screens::{attach, pane::PaneCell, task};
use taskai_tui::seg::{cell_count, strip_sgr};
use taskai_tui::state::St;
use taskai_tui::view::PaneBody;

fn grid(frame: &[String], cols: usize, rows: usize) -> vt100::Parser {
    let mut parser = vt100::Parser::new(rows as u16, cols as u16, 0);
    parser.process(paint_frame(frame, None).as_bytes());
    parser
}

#[test]
fn focus_edge_is_fixed_width_and_has_a_non_color_active_marker() {
    set_truecolor(true);
    for cols in [0, 1, 10, 34, 110, 273] {
        for active in [false, true] {
            let edge = focus_edge(cols, "NAVIGATION", active);
            assert_eq!(cell_count(&edge), cols);
            if cols >= 34 {
                assert!(strip_sgr(&edge).starts_with(if active {
                    "▸ NAVIGATION"
                } else {
                    "─ NAVIGATION"
                }));
            }
        }
    }
}

#[test]
fn focus_moves_between_headers_while_selected_agent_stays_subtly_marked() {
    set_truecolor(true);
    for (cols, rows) in [(110, 30), (150, 42), (209, 51), (273, 58)] {
        for selected in [0, 1] {
            let view = fixture::task_view(0);
            let mut st = St::new();
            st.rail_sel = selected;
            let nav = task::frame_with_focus(
                &view,
                &fixture::hud(),
                &st,
                Focus::Nav,
                cols,
                rows,
                "12:34:56",
            );
            let cli = task::frame_with_focus(
                &view,
                &fixture::hud(),
                &st,
                Focus::Cli,
                cols,
                rows,
                "12:34:56",
            );
            for frame in [&nav, &cli] {
                assert_eq!(frame.len(), rows);
                assert!(frame.iter().all(|line| cell_count(line) == cols));
            }
            assert!(strip_sgr(&nav[4]).starts_with("▸ NAVIGATION"));
            assert!(!strip_sgr(&nav[4]).contains("▸ CLI INPUT"));
            assert!(strip_sgr(&cli[4]).starts_with("─ NAVIGATION"));
            assert!(strip_sgr(&cli[4]).contains("▸ CLI INPUT"));
            let name = &view.sessions[selected].name;
            let (y, x) = nav
                .iter()
                .enumerate()
                .find_map(|(y, line)| {
                    let rail: String = strip_sgr(line).chars().take(task::RAIL_W).collect();
                    rail.find(name)
                        .map(|byte| (y, rail[..byte].chars().count()))
                })
                .unwrap();
            assert_eq!(task::rail_at_row(&view, &st, rows, y), Some(selected));
            let ng = grid(&nav, cols, rows);
            let cg = grid(&cli, cols, rows);
            let n = ng.screen().cell(y as u16, x as u16).unwrap();
            let c = cg.screen().cell(y as u16, x as u16).unwrap();
            assert_eq!(
                n.bgcolor(),
                vt100::Color::Rgb(GLASS_HI.0, GLASS_HI.1, GLASS_HI.2)
            );
            assert_eq!(
                c.bgcolor(),
                vt100::Color::Rgb(GLASS_IN.0, GLASS_IN.1, GLASS_IN.2)
            );
            assert!(n.bold());
            assert!(!c.bold());
            assert_eq!(ng.screen().cell(y as u16, 2).unwrap().contents(), "▸");
            assert_eq!(cg.screen().cell(y as u16, 2).unwrap().contents(), " ");
            // The harness stripe and state lamp still mean identity/state,
            // not focus, and must not change color when typing starts.
            assert_eq!(
                ng.screen().cell(y as u16, 0).unwrap().fgcolor(),
                cg.screen().cell(y as u16, 0).unwrap().fgcolor()
            );
            let dot = strip_sgr(&nav[y])
                .chars()
                .position(|ch| ch == '●' || ch == '○')
                .unwrap();
            assert_eq!(
                ng.screen().cell(y as u16, dot as u16).unwrap().fgcolor(),
                cg.screen().cell(y as u16, dot as u16).unwrap().fgcolor()
            );
            let edge = cg
                .screen()
                .cell(4, (task::RAIL_W + task::GAP) as u16)
                .unwrap();
            assert_eq!(edge.fgcolor(), vt100::Color::Rgb(ION.0, ION.1, ION.2));
        }
    }
}

#[test]
fn focus_changes_never_recolor_shift_or_animate_native_cells() {
    set_truecolor(true);
    for (cols, rows) in [(110, 30), (150, 42), (209, 51), (273, 58)] {
        let mut view = fixture::task_view(0);
        view.body = PaneBody::Cells(
            (0..rows)
                .map(|_| {
                    vec![
                        PaneCell {
                            text: "界".into(),
                            fg: Some((12, 180, 120)),
                            bg: Some((20, 40, 60)),
                            bold: true,
                            italic: true,
                            underline: true,
                            inverse: false,
                            width: 2,
                        },
                        PaneCell {
                            text: "x".into(),
                            fg: Some((190, 30, 170)),
                            bg: Some((10, 20, 30)),
                            bold: false,
                            italic: false,
                            underline: false,
                            inverse: true,
                            width: 1,
                        },
                    ]
                })
                .collect(),
        );
        let mut st = St::new();
        let nav = task::frame_with_focus(
            &view,
            &fixture::hud(),
            &st,
            Focus::Nav,
            cols,
            rows,
            "12:34:56",
        );
        let cli = task::frame_with_focus(
            &view,
            &fixture::hud(),
            &st,
            Focus::Cli,
            cols,
            rows,
            "12:34:56",
        );
        let ng = grid(&nav, cols, rows);
        let cg = grid(&cli, cols, rows);
        for y in 6..rows - 5 {
            for x in task::RAIL_W + task::GAP..cols {
                let n = ng.screen().cell(y as u16, x as u16).unwrap();
                let c = cg.screen().cell(y as u16, x as u16).unwrap();
                assert_eq!(
                    (
                        n.contents(),
                        n.fgcolor(),
                        n.bgcolor(),
                        n.bold(),
                        n.italic(),
                        n.underline(),
                        n.inverse()
                    ),
                    (
                        c.contents(),
                        c.fgcolor(),
                        c.bgcolor(),
                        c.bold(),
                        c.italic(),
                        c.underline(),
                        c.inverse()
                    )
                );
            }
        }
        st.blink = !st.blink;
        st.spin_i += 1;
        assert_eq!(
            cli,
            task::frame_with_focus(
                &view,
                &fixture::hud(),
                &st,
                Focus::Cli,
                cols,
                rows,
                "12:34:56"
            )
        );
        let full = attach::frame(&st, cols, rows);
        assert!(strip_sgr(&full[0]).starts_with("▸ CLI INPUT"));
    }
}

#[test]
fn selected_utility_has_the_same_focus_marker_without_becoming_an_agent() {
    let mut view = fixture::task_view(0);
    view.sessions[0].hkey = "terminal".into();
    view.sessions[0].name = "Terminal".into();
    let st = St::new();
    let nav = task::frame_with_focus(&view, &fixture::hud(), &st, Focus::Nav, 110, 30, "12:34:56");
    let cli = task::frame_with_focus(&view, &fixture::hud(), &st, Focus::Cli, 110, 30, "12:34:56");
    assert!(nav
        .iter()
        .any(|r| strip_sgr(r).starts_with("▸  t  Terminal")));
    assert!(!cli
        .iter()
        .any(|r| strip_sgr(r).starts_with("▸  t  Terminal")));
    assert!(cli.iter().any(|r| strip_sgr(r).contains("t  Terminal")));
}
