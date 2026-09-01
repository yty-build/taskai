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
    Feed { entries: &'static [Feed], phrase: &'static str },
    /// The real hosted CLI's grid.
    Cells(Vec<Vec<PaneCell>>),
    /// Handoff ledger: correspondence plus the queued marker and command hint.
    Handoff { note: String, exchanges: Vec<HandoffRow>, cmd: String },
    /// Nothing attached yet.
    Empty(String),
}

pub struct TaskView {
    pub crumb: String,
    pub title: String,
    pub repo: String,
    pub team_line: String,
    pub sessions: Vec<RailRow>,
    pub body: PaneBody,
}
