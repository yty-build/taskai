// ABOUTME: Screen dispatch — routes state to the active screen renderer and builds
// ABOUTME: deterministic fixture frames for --dump-frame and tests.

use crate::fixture::{FIXED_CLOCK, SPARK_T1, SPARK_T2};
use crate::screens::{boot, home};
use crate::state::{Screen, St};

pub fn render(st: &St, cols: usize, rows: usize, clock: &str) -> Vec<String> {
    match st.screen {
        Screen::Boot => boot::frame(st.boot_k, cols, rows),
        Screen::Home => home::frame(st.home_sel, &st.spark_t1, &st.spark_t2, cols, rows, clock),
    }
}

/// Deterministic fixture frame for a named screen — the parity/test entry point.
pub fn dump(screen: &str, cols: usize, rows: usize) -> Option<Vec<String>> {
    match screen {
        "boot" => Some(boot::frame(22, cols, rows)),
        "home" => Some(home::frame(0, &SPARK_T1, &SPARK_T2, cols, rows, FIXED_CLOCK)),
        _ => None,
    }
}
