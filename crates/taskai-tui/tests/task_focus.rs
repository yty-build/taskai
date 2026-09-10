// ABOUTME: Task-pane focus tests — navigation and embedded CLI input share one layout.
// ABOUTME: Full-screen CLI focus remains a distinct third state outside this renderer.

use taskai_tui::fixture;
use taskai_tui::input::Focus;
use taskai_tui::overlay::help_panel;
use taskai_tui::screens::attach;
use taskai_tui::screens::task::{frame, frame_with_focus};
use taskai_tui::seg::{cell_count, strip_sgr};
use taskai_tui::state::St;

const SIZES: &[(usize, usize)] = &[(110, 30), (150, 42), (209, 51)];

#[test]
fn terminal_is_separate_and_visible_when_selected_in_a_large_team() {
    let mut view = fixture::task_view(0);
    let agent = view.sessions[0].clone();
    view.sessions = vec![agent.clone(); 14];
    let mut terminal = agent;
    terminal.hkey = "terminal".into();
    terminal.name = "Terminal".into();
    terminal.harness = "Shell".into();
    terminal.conduit = "SHELL".into();
    terminal.state_word = "OPEN".into();
    terminal.live = false;
    view.sessions.push(terminal);
    let mut st = St::new();
    st.rail_sel = 14;
    for &(cols, rows) in SIZES {
        let rendered = frame(&view, &fixture::hud(), &st, cols, rows, "12:34:56");
        assert!(rendered.iter().all(|line| cell_count(line) == cols));
        let text = plain(&rendered);
        assert!(text.contains("UTILITIES"));
        assert!(text.contains("t  Terminal"));
        assert!(text.contains("Shell · open"));
        assert!(text.contains("Enter to type"));
        assert!(
            !text.contains("c  View"),
            "utility shell has no ACP view toggle"
        );
    }
}

fn plain(lines: &[String]) -> String {
    lines
        .iter()
        .map(|line| strip_sgr(line))
        .collect::<Vec<_>>()
        .join("\n")
}

#[test]
fn public_task_frame_is_the_navigation_focus_wrapper() {
    let st = St::new();
    let view = fixture::task_view(0);
    let hud = fixture::hud();

    assert_eq!(
        frame(&view, &hud, &st, 150, 42, "12:34:56"),
        frame_with_focus(&view, &hud, &st, Focus::Nav, 150, 42, "12:34:56")
    );
}

#[test]
fn cli_focus_keeps_the_task_rail_and_shows_only_reserved_shortcuts() {
    let st = St::new();
    let view = fixture::task_view(0);
    let hud = fixture::hud();

    for &(cols, rows) in SIZES {
        let rendered = frame_with_focus(&view, &hud, &st, Focus::Cli, cols, rows, "12:34:56");
        assert_eq!(rendered.len(), rows);
        assert!(rendered.iter().all(|line| cell_count(line) == cols));

        let text = plain(&rendered);
        assert!(
            text.contains("TEAM"),
            "session rail disappeared at {cols}x{rows}"
        );
        assert!(
            !text.contains("CLI INPUT ACTIVE"),
            "CLI focus must not light the sidebar"
        );
        assert!(
            text.contains("▸ CLI INPUT"),
            "missing pane focus cue at {cols}x{rows}"
        );
        assert!(
            text.contains("─ NAVIGATION"),
            "sidebar should retain its inactive identity"
        );
        assert!(!text.contains("▸ NAVIGATION"));
        assert!(
            text.contains("Ctrl+F  Full screen"),
            "missing layout key at {cols}x{rows}"
        );
        assert!(
            text.contains("Ctrl+G  Navigation"),
            "missing navigation key at {cols}x{rows}"
        );
        assert!(
            !text.contains("Toggle view"),
            "navigation shortcut leaked at {cols}x{rows}"
        );
        assert!(
            !text.contains("Add Subordinate"),
            "navigation shortcut leaked at {cols}x{rows}"
        );
        assert!(
            !text.contains(" Enter  Full screen"),
            "focus action remained after focus at {cols}x{rows}"
        );
    }
}

#[test]
fn navigation_copy_offers_full_screen_and_home_without_overloading_enter() {
    let st = St::new();
    let view = fixture::task_view(0);
    let hud = fixture::hud();
    let text = plain(&frame(&view, &hud, &st, 150, 42, "12:34:56"));

    assert!(text.contains("Ctrl+F  Full screen"));
    assert!(text.contains("Enter  Type"));
    assert!(text.contains("G  Home"));
    assert!(text.contains("q  Leave"));
    assert!(!text.contains("Enter  Full screen"));
    assert!(!text.contains("Ctrl+G  Focus CLI"));
    assert!(!text.contains("Enter  Attach"));

    let help = plain(&help_panel());
    assert!(help.contains("Toggle fullscreen / inline CLI"));
    assert!(help.contains("Return to navigation"));
    assert!(!help.contains("Toggle native / ACP view"));
    assert!(help.contains("Leave; sessions continue"));
}

#[test]
fn canonical_navigation_keys_and_destructive_verbs_fit_every_supported_size() {
    for &(cols, rows) in SIZES {
        for screen in ["home", "harnesses", "task"] {
            let frame = taskai_tui::render::dump(screen, cols, rows).unwrap();
            assert_eq!(frame.len(), rows);
            assert!(frame.iter().all(|line| cell_count(line) == cols));
            let footer = plain(&frame[rows - 3..]);
            for label in ["F1  Setup helper", "?  Help", "q  Leave"] {
                assert!(
                    footer.contains(label),
                    "{screen} {cols}: missing {label}: {footer}"
                );
            }
            if screen != "harnesses" {
                assert!(footer.contains("s  Settings"), "{screen} {cols}: {footer}");
            }
            if screen != "home" {
                assert!(footer.contains("t  Terminal"), "{screen} {cols}: {footer}");
                assert!(footer.contains("G  Home"), "{screen} {cols}: {footer}");
            }
            if screen == "task" {
                for label in ["d  Destroy agent", "D  Destroy task", "Esc  Back"] {
                    assert!(footer.contains(label), "{cols}: missing {label}: {footer}");
                }
            }
            assert!(!footer.contains("h  Harnesses"));
            assert!(!footer.contains("s  Terminal"));
        }
    }
}

#[test]
fn shortcut_help_names_its_scope_and_does_not_offer_unavailable_live_views() {
    let panel = help_panel();
    assert!(panel.iter().all(|line| cell_count(line) == 68));
    let help = plain(&panel);
    for label in [
        "NAVIGATION",
        "TASK",
        "Settings",
        "Setup helper",
        "Terminal",
        "New task (Home only)",
        "Find (Home only)",
        "Destroy agent",
        "Destroy task",
        "Back / cancel UI",
        "Ctrl+F",
        "Ctrl+G",
    ] {
        assert!(help.contains(label), "missing {label}: {help}");
    }
    assert!(!help.contains("GLOBAL"));
    assert!(!help.contains("Harnesses"));
    assert!(!help.contains("Toggle native / ACP"));
}

#[test]
fn full_screen_ribbon_names_layout_and_navigation_actions() {
    let rendered = attach::frame(&St::new(), 150, 42);
    let text = plain(&rendered);

    assert!(text.contains("Ctrl+F  Inline"));
    assert!(text.contains("Ctrl+G  Navigation"));
    assert!(!text.contains("Return to TaskAI"));
}

#[test]
fn pane_header_reports_the_actual_non_live_state() {
    let mut view = fixture::task_view(0);
    view.sessions[0].live = false;
    for state in ["READY", "ATTENTION", "COMPLETE", "HANDOFF", "SETUP"] {
        view.sessions[0].state_word = state.into();
        let rendered = frame(&view, &fixture::hud(), &St::new(), 150, 42, "12:34:56");
        let pane_header = strip_sgr(&rendered[5]);
        assert!(
            pane_header.contains(state),
            "missing actual state {state}: {pane_header}"
        );
        assert!(!pane_header.contains("QUEUED"));
    }
}

#[test]
fn real_or_connecting_cli_never_advertises_the_demo_only_view_toggle() {
    let mut view = fixture::task_view(0);
    for body in [
        taskai_tui::view::PaneBody::Cells(Vec::new()),
        taskai_tui::view::PaneBody::Empty("Connecting to provider…".into()),
    ] {
        view.body = body;
        let rendered = frame(&view, &fixture::hud(), &St::new(), 150, 42, "12:34:56");
        let text = plain(&rendered);
        assert!(!text.contains("c  View"));
        assert!(!text.contains("view: NATIVE"));
        assert!(text.contains("Enter to type"));
    }
}
