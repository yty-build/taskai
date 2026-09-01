// ABOUTME: Fixture world for headless frames and parity tests — mirrors the prototype's DEMO
// ABOUTME: data so dumped frames are deterministic and comparable cell-for-cell.

use crate::color::{Rgb, DIM, ION};

pub const TAGLINE: &str = "Your agents. Your terminal. One boundary.";
pub const BOOT_METER: &str = "4 of 5 harnesses ready";
pub const FIXED_CLOCK: &str = "12:00:00";
pub const LAMP: &str = "ATTENTION · 1";

pub const HUD_HOME: &[(&str, Rgb)] = &[
    ("4 LIVE", ION),
    ("1 QUEUED", DIM),
    ("2 TASKS ACTIVE", DIM),
];

pub struct Probe {
    pub name: &'static str,
    pub hkey: &'static str,
    pub ready: bool,
}

pub const PROBES: &[Probe] = &[
    Probe { name: "Claude Code", hkey: "claude", ready: true },
    Probe { name: "Codex", hkey: "codex", ready: true },
    Probe { name: "OpenCode", hkey: "opencode", ready: true },
    Probe { name: "Grok CLI", hkey: "grok", ready: true },
    Probe { name: "Aider", hkey: "aider", ready: false },
];

pub struct ManagerRef {
    pub role: &'static str,
    pub harness: &'static str,
    pub hkey: &'static str,
}

pub struct TaskRow {
    pub name: &'static str,
    pub status: &'static str,
    pub managers: &'static [ManagerRef],
    pub meta: &'static str,
}

pub const TASKS: &[TaskRow] = &[
    TaskRow {
        name: "Repository evaluation",
        status: "WORKING",
        managers: &[
            ManagerRef { role: "Lead", harness: "Claude Code", hkey: "claude" },
            ManagerRef { role: "Delivery", harness: "Codex", hkey: "codex" },
        ],
        meta: "4 sessions · buzz",
    },
    TaskRow {
        name: "Production release",
        status: "NEEDS ATTENTION",
        managers: &[ManagerRef { role: "Ship", harness: "OpenCode", hkey: "opencode" }],
        meta: "1 session · shop",
    },
    TaskRow {
        name: "API migration",
        status: "COMPLETE",
        managers: &[
            ManagerRef { role: "Migrator", harness: "Grok CLI", hkey: "grok" },
            ManagerRef { role: "Docs", harness: "Codex", hkey: "codex" },
        ],
        meta: "2 sessions · atlas",
    },
];

pub const KEYS_HOME: &[(&str, &str)] = &[
    ("Enter", "Open"),
    ("n", "New task"),
    ("h", "Harnesses"),
    ("/", "Find"),
    ("?", "Help"),
];

/// Spark levels matching the DEMO spark strings "▂▄▆▅▃▆▇▅▄▆▅▇" and "▂▃▂▄▃▂▃▄▂▃▂▂".
pub const SPARK_T1: [u8; 12] = [1, 3, 5, 4, 2, 5, 6, 4, 3, 5, 4, 6];
pub const SPARK_T2: [u8; 12] = [1, 2, 1, 3, 2, 1, 2, 3, 1, 2, 1, 1];
