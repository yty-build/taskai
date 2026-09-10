// ABOUTME: Realistic portfolio/team sizes keep selection, target identity, and actions visible.
// ABOUTME: Hit testing shares renderer viewport state, preventing clicks on scrolled-away rows.

use taskai_tui::fixture;
use taskai_tui::input::Focus;
use taskai_tui::screens::{home, task};
use taskai_tui::seg::{cell_count, strip_sgr};
use taskai_tui::state::St;

const SIZES: &[(usize, usize)] = &[(110, 30), (150, 42), (209, 51)];

fn plain(lines: &[String]) -> Vec<String> {
    lines.iter().map(|line| strip_sgr(line)).collect()
}

#[test]
fn portfolio_scrolls_every_selection_and_keeps_footer_and_mouse_targets() {
    let template = fixture::home_rows(&[], &[]).remove(0);
    let tasks = (0..40)
        .map(|i| {
            let mut task = template.clone();
            task.name = format!("Unique task {i:02}");
            task
        })
        .collect::<Vec<_>>();
    for &(cols, rows) in SIZES {
        for selected in 0..=tasks.len() {
            let frame = home::frame(&tasks, &fixture::hud(), selected, cols, rows, "12:34:56");
            assert_eq!(frame.len(), rows);
            assert!(frame.iter().all(|row| cell_count(row) == cols));
            let text = plain(&frame);
            let title = if selected == tasks.len() {
                "+ New task".into()
            } else {
                tasks[selected].name.clone()
            };
            let y = text
                .iter()
                .position(|line| line.contains(&title))
                .unwrap_or_else(|| panic!("missing selection {selected} at {cols}x{rows}"));
            assert_eq!(
                home::task_at_row(tasks.len(), selected, cols, rows, y),
                Some(selected)
            );
            assert!(text[rows - 2].contains("q  Leave"));
            assert_eq!(
                home::task_at_row(tasks.len(), selected, cols, rows, rows - 2),
                None
            );
            assert_eq!(
                home::task_at_row(tasks.len(), selected, cols, rows, 1),
                None
            );
        }
    }
}

#[test]
fn long_task_names_and_manager_lists_do_not_hide_connection_or_status() {
    let mut task = fixture::home_rows(&[], &[]).remove(0);
    task.name = "An extremely long task description ".repeat(20);
    task.managers = vec![
        (
            "Manager with a long role ".repeat(10),
            "Claude Code".into(),
            "claude".into()
        );
        8
    ];
    task.meta = "9 sessions · smb-deal-flow · ssh cruise4b".into();
    for &(cols, rows) in SIZES {
        let frame = home::frame(&[task.clone()], &fixture::hud(), 0, cols, rows, "12:34:56");
        let text = plain(&frame);
        assert!(text[5].contains("WORKING"), "status clipped at {cols}");
        assert!(
            text[6].contains("ssh cruise4b"),
            "target clipped at {cols}: {}",
            text[6]
        );
        assert!(frame.iter().all(|row| cell_count(row) == cols));
    }
}

#[test]
fn team_selection_and_click_mapping_survive_long_lists() {
    for terminal in [false, true] {
        let mut view = fixture::task_view(0);
        let template = view.sessions[0].clone();
        view.sessions = (0..30)
            .map(|i| {
                let mut entry = template.clone();
                entry.name = format!("Agent {i:02}");
                entry.depth = u8::from(i % 4 != 0);
                entry
            })
            .collect();
        if terminal {
            let mut utility = template;
            utility.hkey = "terminal".into();
            utility.name = "Terminal".into();
            utility.harness = "Shell".into();
            view.sessions.push(utility);
        }
        for &(cols, rows) in SIZES {
            for selected in 0..view.sessions.len() {
                let mut st = St::new();
                st.rail_sel = selected;
                let frame = task::frame_with_focus(
                    &view,
                    &fixture::hud(),
                    &st,
                    Focus::Cli,
                    cols,
                    rows,
                    "12:34:56",
                );
                let text = plain(&frame);
                let y = text
                    .iter()
                    .position(|line| {
                        line.chars()
                            .take(task::RAIL_W)
                            .collect::<String>()
                            .contains(&view.sessions[selected].name)
                    })
                    .unwrap_or_else(|| panic!("selected {selected} missing at {cols}x{rows}"));
                assert_eq!(task::rail_at_row(&view, &st, rows, y), Some(selected));
                assert!(text[4].contains("▸ CLI INPUT"));
                assert!(frame.iter().all(|row| cell_count(row) == cols));
                assert_eq!(task::rail_at_row(&view, &st, rows, rows - 2), None);
            }
        }
    }
}

#[test]
fn task_focus_and_hud_survive_long_titles() {
    let mut view = fixture::task_view(0);
    view.crumb = "A very long task breadcrumb ".repeat(20);
    view.sessions[0].name = "Long manager name ".repeat(20);
    let st = St::new();
    for &(cols, rows) in SIZES {
        let frame = task::frame_with_focus(
            &view,
            &fixture::hud(),
            &st,
            Focus::Cli,
            cols,
            rows,
            "12:34:56",
        );
        let text = plain(&frame);
        assert!(text[1].contains("12:34:56"));
        assert!(text[4].contains("▸ CLI INPUT"));
        assert!(frame.iter().all(|row| cell_count(row) == cols));
    }
}
