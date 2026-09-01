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

pub struct HarnessRow {
    pub name: &'static str,
    pub hkey: &'static str,
    pub bin: &'static str,
    pub sessions: &'static str,
    pub conduit: &'static str,
    pub setup: bool,
}

pub const HARNESSES: &[HarnessRow] = &[
    HarnessRow { name: "Claude Code", hkey: "claude", bin: "claude · v2.1", sessions: "1 live", conduit: "owned-tui", setup: false },
    HarnessRow { name: "Codex", hkey: "codex", bin: "codex · v0.9", sessions: "1 live", conduit: "shared-live", setup: false },
    HarnessRow { name: "OpenCode", hkey: "opencode", bin: "opencode · v1.4", sessions: "2 live", conduit: "shared-live", setup: false },
    HarnessRow { name: "Grok CLI", hkey: "grok", bin: "grok · v0.3", sessions: "1 active", conduit: "handoff-resume", setup: false },
    HarnessRow { name: "Aider", hkey: "aider", bin: "not found on PATH", sessions: "—", conduit: "—", setup: true },
];

pub const KEYS_HARNESS: &[(&str, &str)] =
    &[("p", "Probe"), ("c", "Configure"), ("+", "Custom CLI"), ("T", "Home"), ("?", "Help")];

pub struct SessionRow {
    pub key: &'static str,
    pub name: &'static str,
    pub harness: &'static str,
    pub hkey: &'static str,
    pub live: bool,
    pub depth: u8,
    pub conduit: &'static str,
    pub state_word: &'static str,
    pub ws: &'static str,
}

pub const SESSIONS: &[SessionRow] = &[
    SessionRow { key: "lead", name: "Lead", harness: "Claude Code", hkey: "claude", live: true, depth: 0, conduit: "OWNED-TUI", state_word: "LIVE", ws: "~/GitHub/buzz" },
    SessionRow { key: "arch", name: "Architecture", harness: "OpenCode", hkey: "opencode", live: true, depth: 1, conduit: "SHARED-LIVE", state_word: "LIVE", ws: "~/GitHub/buzz-worktrees/auth-pass" },
    SessionRow { key: "deliv", name: "Delivery", harness: "Codex", hkey: "codex", live: true, depth: 0, conduit: "SHARED-LIVE", state_word: "LIVE", ws: "~/GitHub/buzz-worktrees/retry-backoff" },
    SessionRow { key: "ver", name: "Verification", harness: "Grok CLI", hkey: "grok", live: false, depth: 1, conduit: "HANDOFF-RESUME", state_word: "HANDOFF", ws: "~/GitHub/buzz-worktrees/verification-pass" },
];

#[derive(Clone)]
pub enum Feed {
    Meta(&'static str),
    User(&'static str),
    Say(&'static str),
    Tool(&'static str, &'static str),
    ResultLine(&'static str),
    Okay(&'static str),
    Ask(&'static str, &'static str),
}

pub const FEED_LEAD: &[Feed] = &[
    Feed::Meta("✻ Claude Code · /Users/me/GitHub/buzz"),
    Feed::User("Evaluate this repository for release readiness."),
    Feed::Say("I'll run the evaluation in three passes — structure, auth, gates."),
    Feed::Tool("Bash", "git log --oneline -6"),
    Feed::ResultLine("094f2aa docs: focus vision on terminal experience …"),
    Feed::Tool("Read", "docs/release-gates.md"),
    Feed::ResultLine("Read 214 lines"),
    Feed::Tool("Bash", "taskai context tree --format text"),
    Feed::ResultLine("Task T1 → you (Lead) → Architecture · peer Delivery"),
    Feed::Say("Architecture is on the auth worktree — folding its findings in."),
    Feed::Tool("Update", "readiness.md"),
    Feed::ResultLine("Added 3 sections · release gates drafted"),
    Feed::Okay("Gate review updated · waiting on Verification"),
];

pub const FEED_ARCH: &[Feed] = &[
    Feed::Meta("✻ OpenCode · worktree auth-pass"),
    Feed::User("Audit the auth flow for release blockers."),
    Feed::Tool("Read", "src/auth/session.ts"),
    Feed::ResultLine("Read 412 lines"),
    Feed::Say("Token refresh races the logout path — flagging as blocking."),
    Feed::Tool("Update", "findings.md"),
    Feed::ResultLine("2 blocking issues written up"),
    Feed::Okay("Findings handed to Lead"),
];

pub const FEED_DELIV: &[Feed] = &[
    Feed::Meta("✻ Codex · /Users/me/GitHub/buzz"),
    Feed::User("Land the retry/backoff branch behind the release gate."),
    Feed::Tool("Bash", "git rebase main"),
    Feed::ResultLine("Successfully rebased retry-backoff"),
    Feed::Tool("Bash", "pytest tests/retry -q"),
    Feed::ResultLine("48 passed · 1.9s"),
    Feed::Say("Branch is green — staging the gate PR."),
];

pub const FEED_SHIP: &[Feed] = &[
    Feed::Meta("✻ OpenCode · /Users/me/GitHub/shop"),
    Feed::Say("Release candidate v2.4.0 built and smoke-tested."),
    Feed::Tool("Bash", "git tag v2.4.0"),
    Feed::ResultLine("tag created"),
    Feed::Ask("Push release tag v2.4.0 to origin?", "[y/N]"),
];

pub const FEED_ATTACH_EXTRA: &[Feed] = &[
    Feed::User("Fold Architecture's auth findings into the gate review."),
    Feed::Say("Merging the worktree notes — two gates change status."),
    Feed::Tool("Read", "../buzz-worktrees/auth-pass/findings.md"),
    Feed::ResultLine("Read 96 lines · 2 blocking issues resolved upstream"),
    Feed::Tool("Update", "readiness.md"),
    Feed::ResultLine("Auth gate: PASS · Rollback gate: needs a dry run"),
    Feed::Okay("Gate review v2 written · 1 gate outstanding"),
];

pub const SPIN_PHRASES: &[(&str, &str)] = &[
    ("lead", "Synthesizing…"),
    ("arch", "Tracing auth flow…"),
    ("deliv", "Staging PR…"),
];

pub const TASK_CRUMB: &str = "TASK · REPOSITORY EVALUATION";
pub const ATTENTION_CRUMB: &str = "TASK · PRODUCTION RELEASE";
pub const ATTACH_COMPOSER: &str = "Schedule the dry run against staging tonight";

pub struct Handoff {
    pub kind: &'static str,
    pub ts: &'static str,
    pub text: &'static str,
}

pub const HANDOFF_NOTE: &str =
    "HANDOFF-RESUME · Grok CLI does not stream. Prompts queue; results return on completion.";
pub const HANDOFF_EXCHANGES: &[Handoff] = &[
    Handoff { kind: "sent", ts: "17:02", text: "Verify release gates against docs/release-gates.md." },
    Handoff { kind: "ret", ts: "17:09", text: "Pass 1 complete — 3 gates checked, 1 flagged: rollback untested." },
    Handoff { kind: "sent", ts: "17:31", text: "Re-check rollback gate after the staging dry run." },
    Handoff { kind: "wait", ts: "", text: "Queued · resumes on completion" },
];
pub const HANDOFF_CMD: &str = "taskai session prompt verification --stdin --wait";

pub struct Drawer {
    pub title: &'static str,
    pub fields: &'static [(&'static str, &'static str, bool)],
    pub keys: &'static [(&'static str, &'static str)],
}

pub const DRAWER_KEYS: &[(&str, &str)] = &[("Enter", "Create"), ("Tab", "Next field"), ("Esc", "Cancel")];

pub const NEWTASK: Drawer = Drawer {
    title: "New task",
    fields: &[
        ("Name", "Checkout resilience", true),
        ("Workspace", "/Users/me/GitHub/shop", false),
        ("Manager", "Claude Code · owned-tui", false),
        ("Base", "main", false),
    ],
    keys: DRAWER_KEYS,
};
pub const NEWTASK_NOTE: &str =
    "Manager session starts in the workspace root. Add subordinates any time.";

pub const SUBORDINATE: Drawer = Drawer {
    title: "Add subordinate — under Lead",
    fields: &[
        ("Role", "Verification", true),
        ("Harness", "Grok CLI · handoff-resume", false),
        ("Base", "main", false),
        ("Worktree", "verification-pass", false),
    ],
    keys: DRAWER_KEYS,
};
pub const SUBORDINATE_PREVIEW: &str = "/Users/me/GitHub/buzz-worktrees/verification-pass";
pub const SUBORDINATE_NOTE: &str =
    "Runs in an isolated worktree. Repo and history are preserved on destroy.";

pub const DESTROY_TITLE: &str = "Destroy task — API migration";
pub const DESTROY_BLAST: &[&str] = &["2 sessions will be closed", "Task record removed from TaskAI"];
pub const DESTROY_KEPT: &[&str] =
    &["Repository untouched", "Worktrees preserved", "Git history preserved"];
pub const DESTROY_CONFIRM: &str = "api-migration";

pub const HELP_BOUNDARY: (&str, &str) = (
    "Ctrl+G",
    "The boundary — the only chord TaskAI reserves. Everything else belongs to your CLI.",
);
pub const HELP_GLOBAL: &[(&str, &str)] =
    &[("T", "Home"), ("n", "New task"), ("h", "Harnesses"), ("/", "Find"), ("?", "Help")];
pub const HELP_TASK: &[(&str, &str)] = &[
    ("Enter", "Attach"),
    ("a", "Add Subordinate"),
    ("m", "Add Manager"),
    ("d", "Destroy"),
];
pub const HELP_CONDUITS: &[(&str, &str)] = &[
    ("owned-tui", "TaskAI hosts the CLI's own interface, full fidelity"),
    ("shared-live", "The CLI streams live; you type into it directly"),
    ("handoff-resume", "Prompts queue; results return on completion — never shown as live"),
];
