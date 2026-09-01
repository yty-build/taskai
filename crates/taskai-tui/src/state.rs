// ABOUTME: TUI state and the tick-driven advance loop — screen, selections, boot progress,
// ABOUTME: cursor blink, deterministic spark walks.

use crate::fixture::{SPARK_T1, SPARK_T2};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Screen {
    Boot,
    Home,
}

pub struct St {
    pub screen: Screen,
    pub tick: u64,
    pub boot_k: usize,
    pub home_sel: usize,
    pub blink: bool,
    pub spark_t1: Vec<u8>,
    pub spark_t2: Vec<u8>,
    rng: u64,
}

impl St {
    pub fn new() -> Self {
        St {
            screen: Screen::Boot,
            tick: 0,
            boot_k: 0,
            home_sel: 0,
            blink: true,
            spark_t1: SPARK_T1.to_vec(),
            spark_t2: SPARK_T2.to_vec(),
            rng: 0x9E37_79B9_7F4A_7C15,
        }
    }

    fn next_rng(&mut self) -> u64 {
        let mut x = self.rng;
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        self.rng = x;
        x
    }
}

impl Default for St {
    fn default() -> Self {
        Self::new()
    }
}

fn walk(rng_val: u64, spark: &mut Vec<u8>) {
    // Same drift bias as the prototype: choices (-1, 0, 0, 1, 1).
    let delta: i8 = [-1, 0, 0, 1, 1][(rng_val % 5) as usize];
    let last = *spark.last().unwrap_or(&3) as i8;
    let next = (last + delta).clamp(0, 6) as u8;
    spark.push(next);
    spark.remove(0);
}

pub fn advance(st: &mut St) {
    st.tick += 1;
    if st.tick % 3 == 0 {
        let (a, b) = (st.next_rng(), st.next_rng());
        walk(a, &mut st.spark_t1);
        walk(b, &mut st.spark_t2);
    }
    if st.tick % 6 == 0 {
        st.blink = !st.blink;
    }
    if st.screen == Screen::Boot {
        st.boot_k = (st.boot_k + 2).min(22);
        if st.boot_k >= 22 && st.tick % 5 == 0 {
            st.screen = Screen::Home;
        }
    }
}
