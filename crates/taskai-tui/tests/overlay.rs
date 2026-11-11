// ABOUTME: Live overlay geometry regressions for broad, centered forms at compact,
// ABOUTME: screenshot-sized, and ultrawide terminal viewports.

use taskai_tui::color::{set_truecolor, AMB, DIM, FT, GLASS};
use taskai_tui::overlay::{
    centered_overlay, drawer_panel_view, form_panel_width, messages_panel, slice_cells, DrawerView,
};
use taskai_tui::seg::{cell_count, row, s, strip_sgr, vrow};
use taskai_tui::view::{MessageLog, MessageRow};

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
            "~/Documents/GitHub/taskai-worktrees/verification-pass",
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

// Expectation: an empty message log says so instead of showing a bare frame, and every
// panel line is exactly as wide as the first. Why: a task with no letters yet is the common
// case right after launch. Value: the person learns the view works before anything is sent.
#[test]
fn empty_message_log_says_so_and_keeps_width() {
    set_truecolor(true);
    let panel = messages_panel("Repository evaluation", &MessageLog::default(), 150, 42);
    let width = cell_count(&panel[0]);
    assert!((60..=150).contains(&width), "width {width}");
    for (i, line) in panel.iter().enumerate() {
        assert_eq!(cell_count(line), width, "line {i}: {:?}", strip_sgr(line));
    }
    let text = panel
        .iter()
        .map(|l| strip_sgr(l))
        .collect::<Vec<_>>()
        .join("\n");
    assert!(
        text.contains("Message log · Repository evaluation"),
        "{text}"
    );
    assert!(text.contains("No messages yet"), "{text}");
}

fn plain(panel: &[String]) -> String {
    panel
        .iter()
        .map(|l| strip_sgr(l))
        .collect::<Vec<_>>()
        .join("\n")
}

fn letters(n: usize) -> Vec<MessageRow> {
    (1..=n)
        .map(|i| MessageRow {
            ts: format!("17:{i:02}:00"),
            from: "claude-lead".into(),
            to: "codex-delivery".into(),
            outcome: "delivered".into(),
            text: format!("letter-{i:02}"),
            mismatch: false,
        })
        .collect()
}

fn assert_width_exact(panel: &[String]) {
    let width = cell_count(&panel[0]);
    for (i, line) in panel.iter().enumerate() {
        assert_eq!(cell_count(line), width, "line {i}: {:?}", strip_sgr(line));
    }
}

// Expectation: at a frame height whose panel holds ten rows, ten letters show without an
// indicator; eleven show the newest nine under "↑ 2 earlier", so the indicator's own row is
// counted and the newest letter stays visible. Why: a count that is one too small misstates
// what the person is not seeing. Value: the hidden count is exact at the boundary.
#[test]
fn message_log_overflow_counts_hidden_rows_exactly() {
    set_truecolor(true);
    let height = 20;
    let ten = MessageLog {
        rows: letters(10),
        ..Default::default()
    };
    let panel = messages_panel("Gate review", &ten, 150, height);
    let text = plain(&panel);
    assert!(!text.contains("earlier"), "{text}");
    assert!(
        text.contains("letter-01") && text.contains("letter-10"),
        "{text}"
    );
    assert!(
        panel.len() <= height - 3,
        "panel {} lines at height {height}",
        panel.len()
    );
    assert_width_exact(&panel);

    let eleven = MessageLog {
        rows: letters(11),
        ..Default::default()
    };
    let panel = messages_panel("Gate review", &eleven, 150, height);
    let text = plain(&panel);
    assert!(text.contains("↑ 2 earlier"), "{text}");
    assert!(
        !text.contains("letter-01") && !text.contains("letter-02"),
        "{text}"
    );
    assert!(
        text.contains("letter-03") && text.contains("letter-11"),
        "{text}"
    );
    assert!(
        panel.len() <= height - 3,
        "panel {} lines at height {height}",
        panel.len()
    );
    assert_width_exact(&panel);
}

// Expectation: when the reader stopped short of the file's start, the panel says so, with
// the exact count of loaded rows it also had to hide, and still keeps the newest row. Why:
// the view is a recent log, and the person must know it is not the whole history. Value: no
// silent gap.
#[test]
fn message_log_marks_unloaded_history() {
    set_truecolor(true);
    let height = 20;
    let five = MessageLog {
        rows: letters(5),
        truncated: true,
        ..Default::default()
    };
    let text = plain(&messages_panel("Gate review", &five, 150, height));
    assert!(text.contains("↑ older history not loaded"), "{text}");
    assert!(
        text.contains("letter-01") && text.contains("letter-05"),
        "{text}"
    );

    let ten = MessageLog {
        rows: letters(10),
        truncated: true,
        ..Default::default()
    };
    let panel = messages_panel("Gate review", &ten, 150, height);
    let text = plain(&panel);
    assert!(
        text.contains("↑ 1 earlier · older history not loaded"),
        "{text}"
    );
    assert!(
        !text.contains("letter-01") && text.contains("letter-02"),
        "{text}"
    );
    assert!(text.contains("letter-10"), "{text}");
    assert!(
        panel.len() <= height - 3,
        "panel {} lines at height {height}",
        panel.len()
    );
    assert_width_exact(&panel);
}

// Expectation: a log that exists but cannot be read shows the reason, never "No messages
// yet". Why: an unreadable log is a fault, and an empty log is the normal start; the person
// must not confuse the two. Value: the fault is visible where the person looks for messages.
#[test]
fn unreadable_message_log_shows_the_reason() {
    set_truecolor(true);
    let broken = MessageLog {
        unavailable: Some("Is a directory (os error 21)".into()),
        ..Default::default()
    };
    let panel = messages_panel("Gate review", &broken, 150, 42);
    let text = plain(&panel);
    assert!(
        text.contains("Log unavailable · Is a directory (os error 21)"),
        "{text}"
    );
    assert!(!text.contains("No messages yet"), "{text}");
    assert_width_exact(&panel);
}

/// Foreground colour of the first cell of `needle` in the painted panel.
fn panel_fg(panel: &[String], needle: &str) -> Option<vt100::Color> {
    let cols = cell_count(&panel[0]);
    let mut parser = vt100::Parser::new(panel.len() as u16, cols as u16, 0);
    parser.process(panel.join("\r\n").as_bytes());
    let screen = parser.screen();
    (0..panel.len() as u16).find_map(|r| {
        let text: String = (0..cols as u16)
            .map(|c| {
                let glyph = screen.cell(r, c).expect("cell").contents();
                glyph.chars().next().unwrap_or(' ')
            })
            .collect();
        text.find(needle).map(|at| {
            screen
                .cell(r, text[..at].chars().count() as u16)
                .unwrap()
                .fgcolor()
        })
    })
}

// Expectation: a letter whose provider write was never acknowledged before the bridge closed
// shows its literal outcome, "delivery uncertain", in amber. Why: it is neither delivered nor
// refused, and the person must not read it as either. Value: the unresolved letter stands
// out without being mislabelled.
#[test]
fn uncertain_delivery_is_amber_and_keeps_its_wording() {
    set_truecolor(true);
    let log = MessageLog {
        rows: vec![MessageRow {
            ts: "17:02:11".into(),
            from: "codex-delivery".into(),
            to: "grok-verification".into(),
            outcome: "delivery uncertain".into(),
            text: "Re-check the rollback gate.".into(),
            mismatch: false,
        }],
        ..Default::default()
    };
    let panel = messages_panel("Gate review", &log, 150, 42);
    let text = plain(&panel);
    assert!(text.contains("delivery uncertain"), "{text}");
    assert!(!text.contains("dropped"), "{text}");
    let amber = vt100::Color::Rgb(AMB.0, AMB.1, AMB.2);
    assert_eq!(panel_fg(&panel, "delivery uncertain"), Some(amber));
    assert_ne!(panel_fg(&panel, "codex-delivery"), Some(amber));
}
