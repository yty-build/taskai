// ABOUTME: Screen dispatch — routes state to the active screen renderer, composites
// ABOUTME: overlays, and builds deterministic fixture frames for --dump-frame and tests.

use crate::fixture::FIXED_CLOCK;
use crate::overlay::{destroy_panel, help_panel, newtask_panel, overlay, sub_panel};
use crate::screens::{attach, attention, boot, harness, home, task};
use crate::state::{Screen, St};

pub fn render(st: &St, cols: usize, rows: usize, clock: &str) -> Vec<String> {
    match st.screen {
        Screen::Boot => boot::frame(st.boot_k, cols, rows),
        Screen::Home => {
            home::frame(st.home_sel, &st.spark_t1, &st.spark_t2, cols, rows, clock)
        }
        Screen::Harnesses => harness::frame(cols, rows, clock),
        Screen::Task => task::frame(st, cols, rows, clock),
        Screen::Attach => attach::frame(st, cols, rows),
        Screen::Attention => attention::frame(st, cols, rows, clock),
        Screen::NewTask | Screen::Subordinate | Screen::Destroy | Screen::Help => {
            let base = match st.prev {
                Screen::Home | Screen::Harnesses => {
                    home::frame(st.home_sel, &st.spark_t1, &st.spark_t2, cols, rows, clock)
                }
                _ => task::frame(st, cols, rows, clock),
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
