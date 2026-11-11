// ABOUTME: View models — what each screen renders, independent of where it came from:
// ABOUTME: the fixture world for parity tests, or the live registry and panes in the app.

use crate::color::Rgb;
use crate::fixture::Feed;
use crate::screens::pane::PaneCell;

#[derive(Clone)]
pub struct ProbeRow {
    pub name: String,
    pub hkey: String,
    pub ready: bool,
}

#[derive(Clone)]
pub struct Hud {
    pub items: Vec<(String, Rgb)>,
    pub lamp: Option<String>,
}

#[derive(Clone)]
pub struct HomeTaskRow {
    pub name: String,
    pub status: String,
    pub managers: Vec<(String, String, String)>,
    pub spark: String,
    pub spark_live: bool,
    pub meta: String,
}

#[derive(Clone)]
pub struct HarnessRowView {
    pub name: String,
    pub hkey: String,
    pub bin: String,
    pub sessions: String,
    pub conduit: String,
    pub setup: bool,
}

#[derive(Clone)]
pub struct RailRow {
    pub key: String,
    pub name: String,
    pub harness: String,
    pub hkey: String,
    pub live: bool,
    pub depth: u8,
    pub conduit: String,
    pub state_word: String,
    pub ws: String,
    /// What the session's mailbox reports: `busy`, `idle` or `waiting`; None when it has none.
    pub status: Option<String>,
    /// What a waiting session waits for, such as `approval` or `input`.
    pub waiting_for: Option<String>,
}

/// One line of a task's message log: a letter, receipt or notice between two sessions.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MessageRow {
    pub ts: String,
    pub from: String,
    pub to: String,
    pub outcome: String,
    /// First line of the body, or the log's note when the line carried no body.
    pub text: String,
    /// The letter crossed permission modes between sender and recipient.
    pub mismatch: bool,
}

/// What the message log view shows: the recent records, whether older history was left
/// unread, and why the log could not be read when it could not.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct MessageLog {
    pub rows: Vec<MessageRow>,
    /// Records older than the ones read exist in the file.
    pub truncated: bool,
    /// The log exists but could not be read; the reason, for the person.
    pub unavailable: Option<String>,
}

#[derive(Clone)]
pub struct HandoffRow {
    pub kind: String,
    pub ts: String,
    pub text: String,
}

/// What fills the working pane for the selected session.
pub enum PaneBody {
    /// Demo feed with an animated spinner phrase (fixture world only).
    Feed {
        entries: &'static [Feed],
        phrase: &'static str,
    },
    /// The real hosted CLI's grid.
    Cells(Vec<Vec<PaneCell>>),
    /// Handoff ledger: correspondence plus the queued marker and command hint.
    Handoff {
        note: String,
        exchanges: Vec<HandoffRow>,
        cmd: String,
    },
    /// Nothing attached yet.
    Empty(String),
}

pub struct TaskView {
    pub crumb: String,
    pub title: String,
    pub repo: String,
    pub connection: String,
    pub team_line: String,
    pub sessions: Vec<RailRow>,
    pub body: PaneBody,
}
