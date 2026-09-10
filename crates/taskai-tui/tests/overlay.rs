// ABOUTME: Live overlay geometry regressions for broad, centered forms at compact,
// ABOUTME: screenshot-sized, and ultrawide terminal viewports.

use taskai_tui::color::{set_truecolor, DIM, FT, GLASS};
use taskai_tui::overlay::{
    centered_overlay, drawer_panel_view, form_panel_width, slice_cells, DrawerView,
};
use taskai_tui::seg::{cell_count, row, s, strip_sgr, vrow};

fn live_worktree_panel(cols: usize) -> Vec<String> {
    let drawer = DrawerView {
        title: "Add Subordinate".into(),
        fields: vec![
            ("Parent".into(), "Lead · Codex".into(), false),
            ("Role".into(), "Verification".into(), true),
            ("Harness".into(), "‹ Claude Code ›".into(), false),
            ("Workspace".into(), "‹ New git worktree ›".into(), false),
            ("Base".into(), "main".into(), false),
            ("Worktree".into(), "verification-pass".into(), false),
        ],
        keys: vec![
            ("Enter", "Create"),
            ("Tab", "Next field"),
            ("←/→", "Choose"),
            ("Esc", "Cancel"),
        ],
    };
    let note = vec![
        vec![s("CODE WILL LIVE AT", Some(FT), Some(GLASS))],
        vec![s(
            "~/Documents/GitHub/acp-gateway-worktrees/verification-pass",
            Some(DIM),
            Some(GLASS),
        )],
        vec![s(
            "Repo and history are preserved on destroy.",
            Some(FT),
            Some(GLASS),
        )],
    ];
    drawer_panel_view(false, &drawer, &note, form_panel_width(cols))
}

// Expectation: the real six-field worktree form is centered and every field,
// action, and edge remains visible. Why: the fixture form is shorter and did
// not catch the live dialog falling below the viewport. Value: the complete
// subordinate action is usable at every supported size.
#[test]
fn live_subordinate_form_is_centered_broad_and_complete() {
    set_truecolor(true);
    for (cols, rows, expected_width) in [(110, 30, 98), (209, 51, 144), (364, 58, 160)] {
        let panel = live_worktree_panel(cols);
        assert_eq!(panel.len(), 28);
        assert_eq!(form_panel_width(cols), expected_width);

        let base = vec![vrow(cols); rows];
        let composed = centered_overlay(&base, &panel, cols);
        assert_eq!(composed.len(), rows);
        assert!(composed.iter().all(|line| cell_count(line) == cols));

        let plain: Vec<String> = composed.iter().map(|line| strip_sgr(line)).collect();
        let title_row = plain
            .iter()
            .position(|line| line.contains("Add Subordinate"))
            .unwrap();
        let preferred_top = rows.saturating_sub(panel.len()) / 2;
        let max_top = rows.saturating_sub(1).saturating_sub(panel.len());
        let expected_top = preferred_top.min(max_top);
        let expected_left = (cols - expected_width) / 2;
        assert_eq!(title_row, expected_top + 1);
        assert_eq!(
            plain[title_row].find("Add Subordinate"),
            Some(expected_left + 2)
        );

        let text = plain.join("\n");
        for expected in [
            "PARENT",
            "ROLE",
            "HARNESS",
            "WORKSPACE",
            "BASE",
            "WORKTREE",
            "CODE WILL LIVE AT",
            "verification-pass",
            "Repo and history are preserved",
            "Enter",
            "Create",
            "Tab",
            "Next field",
            "←/→",
            "Choose",
            "Esc",
            "Cancel",
        ] {
            assert!(
                text.contains(expected),
                "{cols}x{rows} missing {expected:?}"
            );
        }

        let bottom = expected_top + panel.len() - 1;
        let edge: String = plain[bottom]
            .chars()
            .skip(expected_left)
            .take(expected_width)
            .collect();
        assert!(edge.chars().all(|ch| ch == '▀'));
    }
}

#[test]
fn overlays_slice_wide_and_combining_cells_without_displacing_background() {
    let source = row(&[s("A界e\u{301}界Z", Some(DIM), Some(GLASS))], 12, GLASS, 0);
    for start in 0..12 {
        for width in 0..=12 - start {
            let sliced = slice_cells(&source, start, width);
            assert_eq!(
                cell_count(&sliced),
                width,
                "start {start}, width {width}: {sliced:?}"
            );
        }
    }
    assert_eq!(strip_sgr(&slice_cells(&source, 2, 4)), " e\u{301}界");
    assert_eq!(strip_sgr(&slice_cells(&source, 0, 2)), "A ");
    let base = vec![source; 8];
    let panel = vec![row(&[s("弹窗", Some(DIM), Some(GLASS))], 6, GLASS, 0); 3];
    let composed = centered_overlay(&base, &panel, 12);
    assert!(composed.iter().all(|line| cell_count(line) == 12));
    // A defensive clamp also keeps diagnostic/test callers safe when a panel
    // is wider than the viewport, rather than underflowing the right slice.
    let oversized = centered_overlay(&vec![vrow(4); 8], &panel, 4);
    assert!(oversized.iter().all(|line| cell_count(line) == 4));
}
