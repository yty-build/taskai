// ABOUTME: Screen dispatch over the fixture world — used by --dump-frame, parity tests,
// ABOUTME: and the demo; the live app calls the screen renderers with registry views.

use crate::color::{DIM, ION, VOID};
use crate::fixture::{self, BOOT_METER, FIXED_CLOCK, HARNESS_SUMMARY};
use crate::overlay::{destroy_panel, help_panel, newtask_panel, overlay, sub_panel};
use crate::screens::{attach, attention, boot, harness, home, task};
use crate::seg::{row, s, vrow};
use crate::state::{Screen, St};
use std::fmt::Write as _;

pub const MIN_COLS: usize = 110;
pub const MIN_ROWS: usize = 30;

/// Paint a complete frame without relying on terminal newline or auto-wrap
/// behavior. Every rendered row already occupies exactly `cols` cells; joining
/// those rows with CRLF can therefore scroll on terminals that advance both at
/// the right margin and for the following newline. Absolute cursor addressing
/// keeps repeated redraws pinned to the same viewport rows.
pub fn paint_frame(frame: &[String], cursor: Option<(usize, usize)>) -> String {
    let mut painted = String::from("\x1b[?2026h\x1b[?25l");
    for (index, line) in frame.iter().enumerate() {
        let _ = write!(painted, "\x1b[{};1H{}", index + 1, line);
    }
    match cursor {
        Some((row, col)) => {
            let _ = write!(painted, "\x1b[{};{}H\x1b[?25h", row + 1, col + 1);
        }
        None => painted.push_str("\x1b[?25l"),
    }
    painted.push_str("\x1b[?2026l");
    painted
}

/// Paint only what changed since the previous terminal commit. Changed rows remain one
/// synchronized update, while an already-visible cursor is repositioned without a
/// hide/show cycle that would restart its blink.
pub fn paint_update(
    frame: &[String],
    cursor: Option<(usize, usize)>,
    previous_frame: Option<&[String]>,
    previous_cursor: Option<(usize, usize)>,
) -> String {
    let Some(previous_frame) = previous_frame else {
        return paint_frame(frame, cursor);
    };
    if previous_frame != frame {
        let mut painted = String::from("\x1b[?2026h");
        if previous_cursor.is_some() && cursor.is_none() {
            painted.push_str("\x1b[?25l");
        }
        for (index, line) in frame.iter().enumerate() {
            if previous_frame.get(index) != Some(line) {
                let _ = write!(painted, "\x1b[{};1H{}", index + 1, line);
            }
        }
        if let Some((row, col)) = cursor {
            let _ = write!(painted, "\x1b[{};{}H", row + 1, col + 1);
            if previous_cursor.is_none() {
                painted.push_str("\x1b[?25h");
            }
        }
        painted.push_str("\x1b[?2026l");
        return painted;
    }
    match (previous_cursor, cursor) {
        (None, None) => String::new(),
        (Some(previous), Some(current)) if previous == current => String::new(),
        (None, Some((row, col))) => format!("\x1b[{};{}H\x1b[?25h", row + 1, col + 1),
        (Some(_), None) => "\x1b[?25l".to_string(),
        (Some(_), Some((row, col))) => format!("\x1b[{};{}H", row + 1, col + 1),
    }
}

/// A truthful frame for terminals that cannot contain the application yet.
/// It uses the physical viewport dimensions rather than rendering a larger
/// virtual canvas that would wrap and scroll.
pub fn resize_frame(cols: usize, rows: usize) -> Vec<String> {
    let messages = [
        "TASKAI · RESIZE NEEDED".to_string(),
        format!("Current   {cols} × {rows}"),
        format!("Required  {MIN_COLS} × {MIN_ROWS}"),
        "Esc Back/Cancel".to_string(),
        "Ctrl+G Navigation".to_string(),
        "q Leave (navigation)".to_string(),
        "Enlarge the terminal; live state is preserved.".to_string(),
    ];
    let mut frame = vec![vrow(cols); rows];
    let top = rows.saturating_sub(messages.len()) / 2;
    for (index, message) in messages.iter().enumerate() {
        if let Some(target) = frame.get_mut(top + index) {
            *target = row(
                &[s(
                    message,
                    Some(if index == 0 { ION } else { DIM }),
                    Some(VOID),
                )],
                cols,
                VOID,
                cols.saturating_sub(message.chars().count()) / 2,
            );
        }
    }
    frame
}

pub fn render(st: &St, cols: usize, rows: usize, clock: &str) -> Vec<String> {
    match st.screen {
        Screen::Boot => boot::frame(&fixture::probe_rows(), BOOT_METER, st.boot_k, cols, rows),
        Screen::Home => home::frame(
            &fixture::home_rows(&st.spark_t1, &st.spark_t2),
            &fixture::hud(),
            st.home_sel,
            cols,
            rows,
            clock,
        ),
        Screen::Harnesses => harness::frame(
            &fixture::harness_rows(),
            HARNESS_SUMMARY,
            &fixture::hud(),
            cols,
            rows,
            clock,
        ),
        Screen::Task => task::frame(
            &fixture::task_view(st.rail_sel),
            &fixture::hud(),
            st,
            cols,
            rows,
            clock,
        ),
        Screen::Attach => attach::frame(st, cols, rows),
        Screen::Attention => attention::frame(st, cols, rows, clock),
        Screen::NewTask | Screen::Subordinate | Screen::Destroy | Screen::Help => {
            let base = match st.prev {
                Screen::Home | Screen::Harnesses => home::frame(
                    &fixture::home_rows(&st.spark_t1, &st.spark_t2),
                    &fixture::hud(),
                    st.home_sel,
                    cols,
                    rows,
                    clock,
                ),
                _ => task::frame(
                    &fixture::task_view(st.rail_sel),
                    &fixture::hud(),
                    st,
                    cols,
                    rows,
                    clock,
                ),
            };
            let panel = match st.screen {
                Screen::NewTask => newtask_panel(st.blink),
                Screen::Subordinate => sub_panel(st.blink),
                Screen::Destroy => destroy_panel(st.blink),
                _ => help_panel(),
            };
            overlay(&base, &panel, cols, 6)
        }
    }
}

/// Deterministic fixture frame for a named screen — the parity/test entry point.
pub fn dump(screen: &str, cols: usize, rows: usize) -> Option<Vec<String>> {
    let mut st = St::new();
    st.boot_k = 22;
    match screen {
        "boot" => st.screen = Screen::Boot,
        "home" => st.screen = Screen::Home,
        "harnesses" => st.screen = Screen::Harnesses,
        "task" => st.screen = Screen::Task,
        "attach" => st.screen = Screen::Attach,
        "attention" => st.screen = Screen::Attention,
        "newtask" => {
            st.screen = Screen::NewTask;
            st.prev = Screen::Home;
        }
        "subordinate" => {
            st.screen = Screen::Subordinate;
            st.prev = Screen::Task;
        }
        "destroy" => {
            st.screen = Screen::Destroy;
            st.prev = Screen::Task;
        }
        "help" => {
            st.screen = Screen::Help;
            st.prev = Screen::Home;
        }
        _ => return None,
    }
    Some(render(&st, cols, rows, FIXED_CLOCK))
}
