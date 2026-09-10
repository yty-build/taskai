// ABOUTME: The live TaskAI application — registry-driven screens, session panes per
// ABOUTME: harness connector, drawers that create real tasks and sessions, focus routing.

use std::collections::HashMap;
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::mpsc::{channel, sync_channel, Receiver, Sender};
use std::sync::{Arc, Mutex, Weak};
use std::time::{Duration, Instant};

use crossterm::event::{
    Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers, MouseButton, MouseEvent, MouseEventKind,
};
use crossterm::terminal;
use taskai_connectors::claude::ClaudeConnector;
use taskai_connectors::codex::CodexConnector;
use taskai_connectors::events::wait_for_event;
use taskai_connectors::grok::GrokConnector;
use taskai_connectors::opencode::OpenCodeConnector;
use taskai_connectors::{Capability, Conduit, Mode as HarnessMode};
use taskai_core::registry::{
    terminal_ref, ConnectionTarget, Item, ProviderMapping, Registry, Role, ServerProfile,
    SessionState, Source, TmuxRuntimeLocator,
};
use taskai_core::workspace::{preview, provision, suggest_worktree, WorkspaceChoice};
use taskai_pane::grid::CursorState;
use taskai_pane::Pane;
use taskai_tui::chrome::keybar;
use taskai_tui::color::{bg, CLI_BG, DIM, FT, GLASS_IN, ION, KCAP, RESET, TEXT, VOID};
use taskai_tui::input::{route, Focus, Route};
use taskai_tui::overlay::{
    centered_overlay, destroy_panel_view, drawer_panel_view, explorer_panel_width,
    form_panel_width, help_panel, DrawerView,
};
use taskai_tui::render::{paint_update, resize_frame, MIN_COLS, MIN_ROWS};
use taskai_tui::screens::pane::{cell_row, PaneCell};
use taskai_tui::screens::{boot, home, task};
use taskai_tui::seg::{clip_segs, row, s, seglen, vrow, Seg};
use taskai_tui::state::{advance, Screen as StScreen, St, View};
use taskai_tui::view::{HomeTaskRow, Hud, PaneBody, ProbeRow, RailRow, TaskView};

use taskai_connectors::control;

use crate::forms::{self, Editor};
#[cfg(test)]
use crate::launch::confirm_runtime_started;
use crate::remote_probes::{HarnessAvailability, RemoteProbes};
use crate::server_settings::{Action as SettingsAction, ServerCheck, Settings};
use crate::tmux::{self, RuntimeStatus};
use crate::ui_events::{FramePacer, InputPump, INPUT_BATCH, INPUT_BUDGET, UI_TICK};
use crate::{helper, launch};

const HKEYS: [&str; 4] = ["claude", "codex", "opencode", "grok"];
const ATTACH_BODY_TOP: usize = 4;
const TASK_PANE_BODY_TOP: usize = 6;
const TASK_PANE_BODY_LEFT: usize = task::RAIL_W + task::GAP;
const SETUP_BODY_TOP: usize = 3;
// TaskAI owns the outer viewport even while showing a native CLI. Releasing
// capture hands the wheel to the outer terminal/tmux, not to the nested PTY.
const CAPTURE_MOUSE: &str = "\x1b[?1000l\x1b[?1003l\x1b[?1002h\x1b[?1006h";

const NEW_TASK_KEYS: &[(&str, &str)] = &[
    ("Enter", "Next field"),
    ("F2", "Browse"),
    ("Tab", "Next field"),
    ("Ctrl+A", "Select all"),
    ("Ctrl+U", "Clear"),
    ("Esc", "Cancel"),
];
fn new_task_keys(focus: usize) -> Vec<(&'static str, &'static str)> {
    if focus == 2 {
        return vec![
            ("Enter", "Choose server"),
            ("←/→", "Server"),
            ("F2", "Browse"),
            ("Tab", "Next field"),
            ("Esc", "Cancel"),
        ];
    }
    let mut keys = NEW_TASK_KEYS.to_vec();
    keys[0].1 = match focus {
        2 => "Choose server",
        0 => "Create task",
        _ => "Next field",
    };
    keys
}
const FORM_KEYS: &[(&str, &str)] = &[
    ("Enter", "Create"),
    ("Tab", "Next field"),
    ("←/→", "Choose"),
    ("F5", "Recheck CLIs"),
    ("Ctrl+A", "Select all"),
    ("Ctrl+U", "Clear"),
    ("Esc", "Cancel"),
];
const EXPLORER_KEYS: &[(&str, &str)] = &[
    ("↑/↓", "Move"),
    ("Enter", "Open"),
    ("←", "Parent"),
    ("Space", "Choose"),
    ("Esc", "Cancel"),
];
const DESTROY_KEYS: &[(&str, &str)] = &[("Enter", "Destroy"), ("Esc", "Cancel")];
const HELP_KEYS: &[(&str, &str)] = &[("Esc", "Close")];

const RESTORE_TERMINAL: &str =
    "\x1b[?2026l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?7h\x1b[?6l\x1b[r\x1b[?25h\x1b[?1049l\x1b[0m";
struct HostTerminalGuard(bool);
impl Drop for HostTerminalGuard {
    fn drop(&mut self) {
        if self.0 {
            let mut out = io::stdout();
            let _ = write!(out, "{RESTORE_TERMINAL}");
            let _ = out.flush();
            let _ = terminal::disable_raw_mode();
        }
    }
}

fn accepts_text_input(key: &KeyEvent) -> bool {
    matches!(key.code, KeyCode::Char(ch) if !ch.is_control())
        && !key.modifiers.intersects(
            KeyModifiers::CONTROL
                | KeyModifiers::ALT
                | KeyModifiers::SUPER
                | KeyModifiers::HYPER
                | KeyModifiers::META,
        )
}

fn single_line_ui_text(value: &str) -> String {
    value
        .chars()
        .filter_map(|ch| match ch {
            '\n' | '\r' | '\t' => Some(' '),
            ch if ch.is_control() => None,
            ch => Some(ch),
        })
        .collect()
}

/// Replace the dimmed screen's global shortcuts with the controls that are
/// actually active while a modal owns input. The final row remains available
/// for validation/status messages.
fn modal_keybar(mut base: Vec<String>, cols: usize, keys: &[(&str, &str)]) -> Vec<String> {
    let total: usize = keys
        .iter()
        .map(|(key, label)| key.chars().count() + label.chars().count() + 7)
        .sum();
    let contextual = if total > cols {
        // Balance actual label widths, not item counts: otherwise a long first
        // row clips its last shortcut even though the second row has room.
        let width = |pairs: &[(&str, &str)]| {
            pairs
                .iter()
                .map(|(key, label)| {
                    taskai_tui::seg::text_width(key) + taskai_tui::seg::text_width(label) + 3
                })
                .sum::<usize>()
        };
        let mid = (1..keys.len())
            .min_by_key(|&mid| width(&keys[..mid]).max(width(&keys[mid..])))
            .unwrap_or(0);
        vec![
            keybar(cols, &keys[..mid])[1].clone(),
            keybar(cols, &keys[mid..])[1].clone(),
        ]
    } else {
        keybar(cols, keys)
    };
    let contextual: Vec<String> = contextual
        .into_iter()
        .map(|line| line.replace(&taskai_tui::color::fg(FT), &taskai_tui::color::fg(TEXT)))
        .collect();
    if base.len() > contextual.len() {
        let start = base.len() - contextual.len() - 1;
        base[start..start + contextual.len()].clone_from_slice(&contextual);
    }
    base
}

fn live_form_panel(
    blink: bool,
    d: &DrawerView,
    note: &[Vec<Seg>],
    width: usize,
    editor: Editor,
) -> Vec<String> {
    let mut panel = drawer_panel_view(blink, d, note, width);
    for (i, (label, value, focused)) in d.fields.iter().enumerate() {
        if label == "Harness" || label == "Server" {
            // A selector has focus, not a text caret. Its visible chevrons are
            // clickable and the same field also accepts left/right keys.
            panel[3 + i * 3] = row(
                &[s(
                    label.to_uppercase(),
                    Some(if *focused { ION } else { FT }),
                    Some(taskai_tui::color::GLASS),
                )],
                width,
                taskai_tui::color::GLASS,
                2,
            );
            panel[4 + i * 3] = row(
                &[s(
                    value,
                    Some(if *focused { ION } else { TEXT }),
                    Some(GLASS_IN),
                )],
                width,
                GLASS_IN,
                3,
            );
        }
        if matches!(
            label.as_str(),
            "Name" | "Task name" | "Repository" | "Connection" | "Role" | "Base" | "Worktree"
        ) {
            panel[4 + i * 3] = if label == "Connection" && value.trim().is_empty() {
                // A displayed default, not text users must erase before typing SSH.
                forms::input_row(
                    "Local",
                    Editor {
                        cursor: Some(0),
                        selected: false,
                    },
                    *focused,
                    width,
                )
            } else {
                forms::input_row(value, editor, *focused, width)
            };
        }
    }
    panel
}

fn form_overlay(base: &[String], panel: &[String], cols: usize) -> Vec<String> {
    let available = base.len().saturating_sub(3);
    let mut result = centered_overlay(&base[..available], panel, cols);
    result.extend_from_slice(&base[available..]);
    result
}

fn map_child_cursor(
    cursor: CursorState,
    body_top: usize,
    body_left: usize,
    body_height: usize,
    body_width: usize,
    focused: bool,
) -> Option<(usize, usize)> {
    if !focused
        || !cursor.visible
        || cursor.row >= body_height
        || body_width == 0
        || cursor.col > body_width
    {
        return None;
    }
    // vt100 can report the cursor one cell past the final column while a wrap
    // is pending. CUP cannot address that virtual position, so display it on
    // the final physical cell until the child advances.
    Some((
        body_top + cursor.row,
        body_left + cursor.col.min(body_width - 1),
    ))
}

fn terminal_paste_bytes(text: &str, bracketed: bool) -> Vec<u8> {
    if !bracketed {
        return text.as_bytes().to_vec();
    }
    let mut bytes = Vec::with_capacity(text.len() + 12);
    bytes.extend_from_slice(b"\x1b[200~");
    bytes.extend_from_slice(text.as_bytes());
    bytes.extend_from_slice(b"\x1b[201~");
    bytes
}

fn harness_title(hkey: &str) -> &'static str {
    match hkey {
        "claude" => "Claude Code",
        "codex" => "Codex",
        "opencode" => "OpenCode",
        "grok" => "Grok CLI",
        _ => "CLI",
    }
}

fn harness_mode(hkey: &str) -> HarnessMode {
    match hkey {
        "claude" => HarnessMode::OwnedTui,
        "codex" | "opencode" => HarnessMode::SharedLive,
        "grok" => HarnessMode::HandoffResume,
        _ => HarnessMode::Unsupported,
    }
}

fn tilde(p: &Path) -> String {
    let home = std::env::var("HOME").unwrap_or_default();
    let sp = p.display().to_string();
    if !home.is_empty() && sp.starts_with(&home) {
        format!("~{}", &sp[home.len()..])
    } else {
        sp
    }
}

fn private_server_name(home_dir: &Path) -> String {
    let mut hash = 0xcbf2_9ce4_8422_2325u64;
    for byte in home_dir.as_os_str().as_encoded_bytes() {
        hash ^= u64::from(*byte);
        hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
    }
    format!("taskai-{hash:016x}")
}

type BrowseResult = Result<(PathBuf, Vec<(String, bool)>), String>;
type PendingTaskCheck = (String, String, Receiver<Result<PathBuf, String>>);

struct Explorer {
    cwd: PathBuf,
    entries: Vec<(String, bool)>,
    sel: usize,
    target: ConnectionTarget,
    pending: Option<Receiver<BrowseResult>>,
    error: Option<String>,
}

impl Explorer {
    fn open(cwd: &Path) -> Explorer {
        let mut e = Explorer {
            cwd: cwd.to_path_buf(),
            entries: Vec::new(),
            sel: 0,
            target: ConnectionTarget::Local,
            pending: None,
            error: None,
        };
        e.refresh();
        e
    }

    fn refresh(&mut self) {
        self.error = None;
        self.entries.clear();
        self.sel = 0;
        if self.target == ConnectionTarget::Local {
            self.apply(tmux::browse_directory(&self.target, &self.cwd).map_err(|e| e.to_string()));
        } else {
            let (tx, rx) = channel();
            let target = self.target.clone();
            let cwd = self.cwd.clone();
            std::thread::spawn(move || {
                let _ = tx.send(tmux::browse_directory(&target, &cwd).map_err(|e| e.to_string()));
            });
            self.pending = Some(rx);
        }
    }

    fn apply(&mut self, result: Result<(PathBuf, Vec<(String, bool)>), String>) {
        match result {
            Ok((cwd, entries)) => {
                self.cwd = cwd;
                self.entries = entries;
            }
            Err(error) => self.error = Some(error),
        }
    }

    fn poll(&mut self) {
        if let Some(result) = self.pending.as_ref().and_then(|rx| rx.try_recv().ok()) {
            self.pending = None;
            self.apply(result);
        }
    }

    fn on(target: ConnectionTarget, path: PathBuf) -> Self {
        let mut ex = Self {
            cwd: path,
            entries: Vec::new(),
            sel: 0,
            target,
            pending: None,
            error: None,
        };
        ex.refresh();
        ex
    }
}

fn live_explorer_panel(ex: &Explorer, cols: usize) -> Vec<String> {
    use taskai_tui::color::{GLASS, GLASS_HI};
    let width = form_panel_width(cols);
    let path = if ex.target == ConnectionTarget::Local {
        tilde(&ex.cwd)
    } else {
        ex.cwd.display().to_string()
    };
    let mut panel = explorer_panel_width(&path, &ex.entries, ex.sel, width);
    panel[1] = row(
        &[s(
            format!("Choose repository · {}", ex.target),
            Some(TEXT),
            Some(GLASS_HI),
        )],
        width,
        GLASS_HI,
        2,
    );
    let message = if ex.pending.is_some() {
        Some("Connecting and reading folders… Esc cancels.".to_string())
    } else {
        ex.error.as_ref().map(|error| {
            format!(
                "Could not browse: {} · Esc returns to edit the path/host.",
                single_line_ui_text(error)
            )
        })
    };
    if let Some(message) = message {
        let chars: Vec<char> = message.chars().collect();
        for (i, line) in chars
            .chunks(width.saturating_sub(4).max(1))
            .take(4)
            .enumerate()
        {
            panel[6 + i] = row(
                &[s(line.iter().collect::<String>(), Some(FT), Some(GLASS))],
                width,
                GLASS,
                2,
            );
        }
    }
    let keys = panel.len() - 3;
    panel[keys] = row(
        &[s(
            "Select a folder · Enter opens it · Space uses the selected folder",
            Some(FT),
            Some(GLASS),
        )],
        width,
        GLASS,
        2,
    );
    panel
}

struct NewTaskForm {
    name: String,
    repo: String,
    server_id: String,
    focus: usize,
    explorer: Option<Explorer>,
}

struct ManagerForm {
    name: String,
    choices: Vec<HarnessAvailability>,
    hsel: usize,
    focus: usize,
}

struct SubForm {
    parent: String,
    role: String,
    choices: Vec<HarnessAvailability>,
    hsel: usize,
    ws_mode: usize, // 0 same repo · 1 new worktree · 2 another repository
    branch: String,
    wname: String,
    other: String,
    focus: usize,
    explorer: Option<Explorer>,
}

fn harness_label(choices: &[HarnessAvailability], selected: usize) -> String {
    choices.get(selected).map_or_else(
        || "Checking harnesses…".into(),
        |choice| {
            format!(
                "‹ {} ›   {}/{} · {}",
                harness_title(&choice.hkey),
                selected + 1,
                choices.len(),
                if choice.is_ready() {
                    "READY"
                } else {
                    "UNAVAILABLE"
                }
            )
        },
    )
}

fn harness_notes(
    choice: Option<&HarnessAvailability>,
    target: &str,
    width: usize,
) -> Vec<Vec<Seg>> {
    let detail = match choice {
        Some(choice) => match &choice.unavailable {
            Some(reason) => format!("{} on {target}: {reason}", harness_title(&choice.hkey)),
            None => format!(
                "Ready on {target} · version {}",
                choice.version.as_deref().unwrap_or("verified")
            ),
        },
        None => format!("Checking CLIs on {target}… F5 rechecks without losing this form."),
    };
    let mut lines = vec![String::new()];
    let mut used = 0;
    for ch in single_line_ui_text(&detail).chars() {
        let cells = forms::cell_width(ch);
        if used + cells > width.saturating_sub(4) {
            if lines.len() == 2 {
                break;
            }
            lines.push(String::new());
            used = 0;
        }
        lines.last_mut().unwrap().push(ch);
        used += cells;
    }
    lines.resize(2, String::new());
    lines
        .into_iter()
        .map(|line| vec![s(line, Some(FT), Some(taskai_tui::color::GLASS))])
        .collect()
}

fn cycle_harness(selected: &mut usize, len: usize, next: bool) {
    if len > 0 {
        *selected = if next {
            (*selected + 1) % len
        } else {
            (*selected + len - 1) % len
        };
    }
}

fn require_harness(choices: &[HarnessAvailability], selected: usize) -> Result<String, String> {
    let choice = choices
        .get(selected)
        .ok_or("Choose a harness after checking CLIs")?;
    if let Some(reason) = &choice.unavailable {
        return Err(format!(
            "{} unavailable: {reason} · Choose another harness or F5 recheck",
            harness_title(&choice.hkey)
        ));
    }
    Ok(choice.hkey.clone())
}

fn probe_local_harnesses(sender: Sender<(String, Capability)>) {
    for (hkey, program, minimum) in [
        ("claude", ClaudeConnector::default().bin, "2.0.0"),
        ("codex", CodexConnector::default().bin, "0.100.0"),
        ("opencode", OpenCodeConnector::default().bin, "1.18.0"),
        ("grok", GrokConnector::default().bin, "1.0.0"),
    ] {
        let sender = sender.clone();
        std::thread::spawn(move || {
            // A hung version command must not hide the other providers or
            // prevent F5 forever. Use the same bounded command runner as SSH.
            let cap = match tmux::probe_program(&ConnectionTarget::Local, &program) {
                Ok(detail) => taskai_connectors::gate_version(
                    Capability {
                        mode: harness_mode(hkey),
                        version: taskai_connectors::claude::parse_version(&detail),
                        detail,
                    },
                    minimum,
                ),
                Err(error) => Capability {
                    mode: HarnessMode::Unsupported,
                    version: None,
                    detail: error.to_string(),
                },
            };
            let _ = sender.send((hkey.into(), cap));
        });
    }
}

struct DestroyForm {
    item: Item,
    title: String,
    blast_lines: Vec<String>,
    expect: String,
    typed: String,
    back_home: bool,
}

struct PaneEntry {
    pane: Arc<Mutex<Pane>>,
    runtime: TmuxRuntimeLocator,
}

struct PendingLaunch {
    task_ref: String,
    result: Receiver<Result<launch::Outcome, String>>,
    input: Vec<BufferedInput>,
}

enum BufferedInput {
    Bytes(Vec<u8>),
    Paste(String),
}

type ResizeRequest = (TmuxRuntimeLocator, u16, u16);
type PendingInspection = (
    Receiver<Result<RuntimeStatus, String>>,
    Option<SessionState>,
);

fn resize_worker() -> Sender<ResizeRequest> {
    let (tx, rx) = channel::<ResizeRequest>();
    std::thread::spawn(move || {
        while let Ok(first) = rx.recv() {
            let mut latest = HashMap::new();
            for request in std::iter::once(first).chain(rx.try_iter()) {
                latest.insert(
                    format!(
                        "{}:{}:{}",
                        request.0.target, request.0.private_server, request.0.session
                    ),
                    request,
                );
            }
            for (_, (runtime, height, width)) in latest {
                // Never put an SSH round trip on the render/input thread. A newer
                // queued size supersedes intermediate resize events next pass.
                let _ = tmux::resize(&runtime, height, width);
            }
        }
    });
    tx
}

#[derive(Clone, Copy, PartialEq)]
enum PrevMode {
    Home,
    Task,
    Settings,
    Helper(usize, HelperBack),
}

enum Mode {
    Boot,
    Home,
    Find(String),
    Settings,
    Helper { selection: usize, back: HelperBack },
    ServerConsole(String),
    ChooseServer { form: NewTaskForm, selection: usize },
    Task,
    TaskCli,
    Attach,
    Help(PrevMode),
    NewTask(NewTaskForm),
    Manager(ManagerForm),
    Sub(SubForm),
    Destroy(DestroyForm),
}

#[derive(Clone, Copy, PartialEq)]
enum HelperBack {
    Home,
    Task,
    Settings,
}

struct HelperAction {
    action: String,
    argument: Option<String>,
}

#[derive(Clone, PartialEq, Eq)]
struct CliViewport {
    key: String,
    left: usize,
    top: usize,
    width: usize,
    height: usize,
}

struct CliMousePress {
    view: CliViewport,
    pane: Weak<Mutex<Pane>>,
    button: MouseButton,
    code: u8,
    row: usize,
    col: usize,
}

impl CliViewport {
    fn contains(&self, x: usize, y: usize) -> bool {
        (self.left..self.left + self.width).contains(&x)
            && (self.top..self.top + self.height).contains(&y)
    }
}

fn is_wheel(kind: MouseEventKind) -> bool {
    matches!(
        kind,
        MouseEventKind::ScrollUp
            | MouseEventKind::ScrollDown
            | MouseEventKind::ScrollLeft
            | MouseEventKind::ScrollRight
    )
}

fn mouse_packet_kind(mouse: MouseEvent) -> (u8, bool) {
    let button_code = |button| match button {
        MouseButton::Left => 0,
        MouseButton::Middle => 1,
        MouseButton::Right => 2,
    };
    let (mut button, released) = match mouse.kind {
        MouseEventKind::Down(button) => (button_code(button), false),
        MouseEventKind::Up(button) => (button_code(button), true),
        MouseEventKind::Drag(button) => (button_code(button) | 32, false),
        MouseEventKind::Moved => (35, false),
        MouseEventKind::ScrollUp => (64, false),
        MouseEventKind::ScrollDown => (65, false),
        MouseEventKind::ScrollLeft => (66, false),
        MouseEventKind::ScrollRight => (67, false),
    };
    if mouse.modifiers.contains(KeyModifiers::SHIFT) {
        button |= 4;
    }
    if mouse.modifiers.contains(KeyModifiers::ALT) {
        button |= 8;
    }
    if mouse.modifiers.contains(KeyModifiers::CONTROL) {
        button |= 16;
    }
    (button, released)
}

fn active_field(mode: &mut Mode) -> Option<&mut String> {
    match mode {
        Mode::Find(query) => Some(query),
        Mode::NewTask(f) if f.explorer.is_none() => match f.focus {
            0 => Some(&mut f.name),
            1 => Some(&mut f.repo),
            _ => None,
        },
        Mode::Manager(f) if f.focus == 0 => Some(&mut f.name),
        Mode::Sub(f) if f.explorer.is_none() => match f.focus {
            0 => Some(&mut f.role),
            3 => Some(&mut f.branch),
            4 => Some(&mut f.wname),
            5 => Some(&mut f.other),
            _ => None,
        },
        _ => None,
    }
}

fn field_identity(mode: &Mode) -> (u8, usize, bool) {
    match mode {
        Mode::Find(_) => (4, 0, false),
        Mode::NewTask(f) => (1, f.focus, f.explorer.is_some()),
        Mode::Manager(f) => (2, f.focus, false),
        Mode::Sub(f) => (3, f.focus, f.explorer.is_some()),
        _ => (0, 0, false),
    }
}

pub struct App {
    st: St,
    mode: Mode,
    focus: Focus,
    home_dir: PathBuf,
    registry: Registry,
    probes: HashMap<String, Capability>,
    remote_probes: RemoteProbes,
    pending_harness_action: Option<(String, char)>,
    probe_rx: Receiver<(String, Capability)>,
    bind_rx: Receiver<(String, String)>,
    bind_tx: Sender<(String, String)>,
    panes: HashMap<String, PaneEntry>,
    pending_launches: HashMap<String, PendingLaunch>,
    buffered_input: HashMap<String, Vec<BufferedInput>>,
    disconnected: HashMap<String, PendingInspection>,
    terminal_states: HashMap<String, &'static str>,
    resize_tx: Sender<ResizeRequest>,
    served: control::Shared,
    sel_task: Option<String>,
    task_selections: HashMap<String, String>,
    status: Option<String>,
    host_cursor: Option<(usize, usize)>,
    editor: Editor,
    form_hits: Vec<(usize, usize, usize, usize)>, // focus, row, left, width
    settings: Settings,
    settings_back: PrevMode,
    setup_pending: HashMap<String, Receiver<Result<PaneEntry, String>>>,
    server_check_pending: HashMap<String, Receiver<Result<(), String>>>,
    server_check_errors: HashMap<String, Option<String>>,
    pending_task: Option<PendingTaskCheck>,
    helper_requests: Receiver<control::UiRequest>,
    helper_action: Option<HelperAction>,
    cli_mouse_press: Option<CliMousePress>,
    leave_requested: bool,
}

impl Drop for App {
    fn drop(&mut self) {
        self.panes.clear();
        self.served.panes.lock().unwrap().clear();
        self.served.session_ids.lock().unwrap().clear();
        self.served.harnesses.lock().unwrap().clear();
        let _ = std::fs::remove_file(control::sock_path(&self.home_dir));
    }
}

impl App {
    pub fn new(home_dir: PathBuf) -> io::Result<App> {
        Self::open(home_dir, true)
    }

    #[cfg(test)]
    fn new_test(home_dir: PathBuf) -> io::Result<App> {
        // UI/registry unit fixtures explicitly supply capabilities when needed.
        // Do not launch four real provider binaries for every form/key test;
        // connector and binary integration suites exercise real discovery.
        Self::open(home_dir, false)
    }

    fn open(home_dir: PathBuf, probe_harnesses: bool) -> io::Result<App> {
        let mut registry = Registry::open(&home_dir.join("state")).map_err(io::Error::other)?;
        // The JSON state is a cache; tmux decides whether a recorded runtime survived.
        let observed: Vec<(String, SessionState, Option<TmuxRuntimeLocator>)> = registry
            .tasks()
            .iter()
            .flat_map(|t| registry.sessions_of(&t.task_ref))
            .map(|se| (se.session_ref.clone(), se.state, se.runtime.clone()))
            .collect();
        // Paint the portfolio immediately, even when a saved SSH host is offline.
        // The ordinary event loop reconciles these background results; until then
        // cached LIVE claims are QUEUED rather than asserted without evidence.
        let mut disconnected: HashMap<String, PendingInspection> = HashMap::new();
        let mut reconciliation = Vec::new();
        for (sref, previous, runtime) in observed {
            if previous == SessionState::Complete {
                continue;
            }
            let state = match runtime {
                Some(runtime) => {
                    let (tx, rx) = channel();
                    std::thread::spawn(move || {
                        let _ = tx.send(tmux::inspect(&runtime).map_err(|error| error.to_string()));
                    });
                    disconnected.insert(sref.clone(), (rx, Some(previous)));
                    SessionState::Queued
                }
                None if matches!(
                    previous,
                    SessionState::Live
                        | SessionState::Working
                        | SessionState::Attention
                        | SessionState::Handoff
                ) =>
                {
                    reconciliation.push(format!("{sref}: no tmux runtime was recorded"));
                    SessionState::Queued
                }
                None => previous,
            };
            if state != previous {
                let _ = registry.set_state(&sref, state);
            }
        }
        let mut terminal_states = HashMap::new();
        for runtime in registry
            .tasks()
            .iter()
            .filter_map(|task| task.terminal.clone())
        {
            let key = runtime.session.clone();
            let (tx, rx) = channel();
            std::thread::spawn(move || {
                let _ = tx.send(tmux::inspect(&runtime).map_err(|error| error.to_string()));
            });
            terminal_states.insert(key.clone(), "CHECKING");
            disconnected.insert(key, (rx, None));
        }
        if !disconnected.is_empty() {
            reconciliation.push("Checking saved terminals in the background".into());
        }
        let run = home_dir.join("run");
        std::fs::create_dir_all(&run)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&run, std::fs::Permissions::from_mode(0o700))?;
        }
        let (ptx, probe_rx) = channel();
        if probe_harnesses {
            probe_local_harnesses(ptx);
        }
        let (bind_tx, bind_rx) = channel();
        let served = control::Shared {
            panes: Arc::new(Mutex::new(HashMap::new())),
            session_ids: Arc::new(Mutex::new(HashMap::new())),
            harnesses: Arc::new(Mutex::new(HashMap::new())),
            events: run.join("claude-events.jsonl"),
            log: run.join("dispatch.jsonl"),
            conduit: Arc::new(Conduit::new()),
        };
        let (ui_tx, helper_requests) = std::sync::mpsc::sync_channel(8);
        control::serve_with_ui(control::sock_path(&home_dir), served.clone(), ui_tx)?;
        let mut st = St::new();
        st.screen = StScreen::Home; // the app drives boot itself
        st.boot_k = 0;
        Ok(App {
            st,
            mode: Mode::Boot,
            focus: Focus::Nav,
            home_dir,
            registry,
            probes: HashMap::new(),
            remote_probes: RemoteProbes::default(),
            pending_harness_action: None,
            probe_rx,
            bind_rx,
            bind_tx,
            panes: HashMap::new(),
            pending_launches: HashMap::new(),
            buffered_input: HashMap::new(),
            disconnected,
            terminal_states,
            resize_tx: resize_worker(),
            served,
            sel_task: None,
            task_selections: HashMap::new(),
            status: (!reconciliation.is_empty()).then(|| reconciliation.join(" · ")),
            host_cursor: None,
            editor: Editor::default(),
            form_hits: Vec::new(),
            settings: Settings::new(),
            settings_back: PrevMode::Home,
            setup_pending: HashMap::new(),
            server_check_pending: HashMap::new(),
            server_check_errors: HashMap::new(),
            pending_task: None,
            helper_requests,
            helper_action: None,
            cli_mouse_press: None,
            leave_requested: false,
        })
    }

    pub fn run(&mut self, attach: Option<PathBuf>) -> io::Result<()> {
        if let Some(dir) = attach {
            let dir = dir.canonicalize()?;
            let name = dir
                .file_name()
                .map(|n| n.to_string_lossy().to_string())
                .unwrap_or("task".into());
            let t = self
                .registry
                .create_task(&name, &dir, Source::HumanUi)
                .map_err(io::Error::other)?;
            let sref = self
                .registry
                .create_session(&t, None, "Lead", "claude", &dir, Source::HumanUi)
                .map_err(io::Error::other)?;
            self.sel_task = Some(t);
            let (cols, rows) = terminal::size().unwrap_or((120, 40));
            if let Err(error) = self.focus_session(&sref, cols as usize, rows as usize, false) {
                self.status = Some(format!("Lead registered as {sref}; launch failed: {error}"));
            }
        }
        let mut out = io::stdout();
        terminal::enable_raw_mode()?;
        let mut terminal_guard = HostTerminalGuard(true);
        // Own the complete alternate-screen viewport. Reset scroll margins and
        // origin mode, then disable auto-wrap because every TaskAI row paints
        // the terminal's final column explicitly.
        write!(
            out,
            "\x1b[?1049h\x1b[?2004h\x1b[?25l\x1b[?6l\x1b[r\x1b[?7l\x1b[2J\x1b[H{CAPTURE_MOUSE}"
        )?;
        out.flush()?;
        let result = self.event_loop(&mut out);
        self.cancel_cli_mouse_press();
        // Pane owns only the disposable tmux/SSH attach client. Dropping every viewer
        // before restoring the host terminal leaves the recorded tmux runtimes alone.
        self.panes.clear();
        self.served.panes.lock().unwrap().clear();
        self.served.session_ids.lock().unwrap().clear();
        let _ = std::fs::remove_file(control::sock_path(&self.home_dir));
        let runtimes: Vec<TmuxRuntimeLocator> = self
            .registry
            .tasks()
            .iter()
            .flat_map(|task| self.registry.sessions_of(&task.task_ref))
            .filter_map(|session| session.runtime.clone())
            .chain(
                self.registry
                    .tasks()
                    .iter()
                    .filter_map(|task| task.terminal.clone()),
            )
            .chain(
                self.registry
                    .servers()
                    .iter()
                    .flat_map(|server| server.setup_runtimes.values().cloned()),
            )
            .collect();
        // Leaving is local and immediate, even if an SSH host has disappeared.
        // Do not make a fresh network claim about liveness on the way out.
        write!(out, "{RESTORE_TERMINAL}")?;
        out.flush()?;
        terminal::disable_raw_mode()?;
        terminal_guard.0 = false;
        if result.is_ok() {
            if runtimes.is_empty() {
                writeln!(out, "TaskAI closed — no tracked runtimes.")?;
            } else {
                writeln!(
                    out,
                    "TaskAI detached — {} tracked tmux runtime{} left untouched. Run taskai to return.",
                    runtimes.len(), if runtimes.len() == 1 { "" } else { "s" },
                )?;
            }
        }
        out.flush()?;
        result
    }

    fn event_loop(&mut self, out: &mut impl Write) -> io::Result<()> {
        let mut last_frame: Option<Vec<String>> = None;
        let mut last_cursor: Option<(usize, usize)> = None;
        let (wake, wakes) = sync_channel(1);
        let input = InputPump::start(wake.clone())?;
        let mut watched: Option<Weak<Mutex<Pane>>> = None;
        let mut painted_revision = None;
        let mut dirty = true;
        let mut next_tick = Instant::now();
        let mut pacer = FramePacer::new(next_tick);
        let (c, r) = terminal::size().unwrap_or((120, 40));
        let (mut cols, mut rows) = (c as usize, r as usize);
        loop {
            // Forward input before doing any rendering. A bounded batch keeps
            // a continuous trackpad stream from starving output commits.
            let input_start = Instant::now();
            let mut batch_full = false;
            let mut resized = false;
            for index in 0..INPUT_BATCH {
                let Some(event) = input.try_event()? else {
                    break;
                };
                match event {
                    Event::Key(k) => {
                        if k.kind != KeyEventKind::Release {
                            if !self.handle(k, cols, rows) {
                                return Ok(());
                            }
                            dirty = true;
                        }
                    }
                    Event::Paste(text) => {
                        self.handle_paste(&text);
                        dirty = true;
                    }
                    Event::Mouse(mouse) => {
                        let cli_wheel = matches!(
                            mouse.kind,
                            MouseEventKind::ScrollUp
                                | MouseEventKind::ScrollDown
                                | MouseEventKind::ScrollLeft
                                | MouseEventKind::ScrollRight
                        ) && self.cli_viewport(cols, rows).is_some_and(|v| {
                            (mouse.column as usize) >= v.left
                                && (mouse.column as usize) < v.left + v.width
                                && (mouse.row as usize) >= v.top
                                && (mouse.row as usize) < v.top + v.height
                        });
                        self.handle_mouse(mouse, cols, rows);
                        if self.leave_requested {
                            return Ok(());
                        }
                        // A wheel packet is not a changed frame. Let the actual
                        // tmux/native response wake us, without speculative paint.
                        dirty |= !cli_wheel;
                    }
                    Event::Resize(c, r) => {
                        self.cancel_cli_mouse_press();
                        (cols, rows) = (c as usize, r as usize);
                        dirty = true;
                        resized = true;
                        // Commit this geometry before any later mouse packet.
                        batch_full = true;
                        break;
                    }
                    _ => {}
                }
                if index + 1 == INPUT_BATCH || input_start.elapsed() >= INPUT_BUDGET {
                    batch_full = true;
                    break;
                }
            }

            // Background bookkeeping and animations keep their existing 10Hz
            // cadence; terminal output does not have to wait for that cadence.
            let now = Instant::now();
            if now >= next_tick {
                next_tick = now + UI_TICK;
                dirty = true;
                self.poll_helper_requests();
                self.poll_new_task();
                self.poll_settings();
                self.poll_launches();
                self.poll_disconnected();
                advance(&mut self.st);
                match &mut self.mode {
                    Mode::NewTask(f) => {
                        if let Some(ex) = &mut f.explorer {
                            ex.poll();
                        }
                    }
                    Mode::Sub(f) => {
                        if let Some(ex) = &mut f.explorer {
                            ex.poll();
                        }
                    }
                    _ => {}
                }
                let mut probes_changed = false;
                while let Ok((hkey, cap)) = self.probe_rx.try_recv() {
                    self.probes.insert(hkey, cap);
                    probes_changed = true;
                }
                if probes_changed && self.selected_connection() == ConnectionTarget::Local {
                    self.sync_form_harnesses();
                }
                while let Ok((sref, sid)) = self.bind_rx.try_recv() {
                    let version = self
                        .probes
                        .get("claude")
                        .and_then(|c| c.version.clone())
                        .unwrap_or_default();
                    let _ = self.registry.bind_provider(
                        &sref,
                        ProviderMapping {
                            provider: "claude".into(),
                            provider_session_id: sid.clone(),
                            runtime_id: self
                                .registry
                                .session(&sref)
                                .and_then(|session| session.runtime.as_ref())
                                .map(|runtime| {
                                    format!("tmux:{}:{}", runtime.private_server, runtime.session)
                                })
                                .unwrap_or_else(|| "tmux:unbound".into()),
                            connector_version: version,
                        },
                    );
                    let _ = self.registry.set_state(&sref, SessionState::Live);
                    self.served.session_ids.lock().unwrap().insert(sref, sid);
                }
                let dead: Vec<String> = self
                    .panes
                    .iter()
                    .filter(|(_, entry)| !entry.pane.lock().unwrap().is_alive())
                    .map(|(sref, _)| sref.clone())
                    .collect();
                for sref in dead {
                    let runtime = self.panes.remove(&sref).map(|entry| entry.runtime);
                    self.served.panes.lock().unwrap().remove(&sref);
                    self.buffered_input.remove(&sref);
                    if let Some(runtime) = runtime {
                        let previous = self.registry.session(&sref).map(|se| se.state);
                        if previous.is_some_and(|state| state != SessionState::Complete) {
                            let _ = self.registry.set_state(&sref, SessionState::Queued);
                        }
                        if sref.ends_with("-terminal") {
                            self.terminal_states.insert(sref.clone(), "CHECKING");
                        }
                        let (tx, rx) = channel();
                        std::thread::spawn(move || {
                            let _ = tx.send(tmux::inspect(&runtime).map_err(|e| e.to_string()));
                        });
                        self.disconnected.insert(sref, (rx, previous));
                    }
                }
                if let Mode::Boot = self.mode {
                    let done = self.probes.len() >= HKEYS.len();
                    let cap = if done { 22 } else { 18 };
                    self.st.boot_k = (self.st.boot_k + 2).min(cap);
                    if self.st.boot_k >= 22 {
                        self.mode = Mode::Home;
                    }
                }
                self.poll_harness_action(cols, rows);
            }
            self.sync_cli_mouse_press(cols, rows);

            // Subscribe only the displayed viewer. Background CLI output must
            // not keep the outer UI busy. Weak identity catches same-key reattach.
            let visible = self
                .cli_viewport(cols, rows)
                .and_then(|v| self.panes.get(&v.key))
                .map(|entry| Arc::clone(&entry.pane));
            let same_viewer = match (&watched, &visible) {
                (Some(old), Some(current)) => old.ptr_eq(&Arc::downgrade(current)),
                (None, None) => true,
                _ => false,
            };
            if !same_viewer {
                if let Some(old) = watched.take().and_then(|p| p.upgrade()) {
                    old.lock().unwrap().clear_output_notifier();
                }
                if let Some(current) = &visible {
                    current.lock().unwrap().set_output_notifier(wake.clone());
                    watched = Some(Arc::downgrade(current));
                }
                painted_revision = None;
                dirty = true;
            }
            // Read before taking a frame snapshot. Output racing the snapshot
            // then stays unacknowledged, so the last update can never be lost.
            let revision = visible
                .as_ref()
                .map(|p| p.lock().unwrap().output_revision());
            dirty |= revision != painted_revision;
            if dirty && (resized || pacer.due(Instant::now())) {
                let frame = if cols < MIN_COLS || rows < MIN_ROWS {
                    self.host_cursor = None;
                    resize_frame(cols, rows)
                } else {
                    self.frame(cols, rows)
                };
                let cursor = self.host_cursor;
                let update = paint_update(&frame, cursor, last_frame.as_deref(), last_cursor);
                if !update.is_empty() {
                    write!(out, "{update}")?;
                    out.flush()?;
                }
                last_frame = Some(frame);
                last_cursor = cursor;
                painted_revision = revision;
                dirty = false;
                pacer.committed(Instant::now());
            }
            if batch_full {
                continue;
            }
            let now = Instant::now();
            let mut timeout = next_tick.saturating_duration_since(now);
            if dirty {
                timeout = timeout.min(pacer.wait(now));
            }
            // Both input and fresh PTY output signal this bounded channel.
            // Waiting is idle-only, never a fixed delay in the scroll path.
            let _ = wakes.recv_timeout(timeout);
        }
    }

    fn clock(&self) -> String {
        chrono::Local::now().format("%H:%M:%S").to_string()
    }

    // ---- live views ------------------------------------------------------

    fn hud(&self) -> Hud {
        let c = self.registry.hud_counts();
        Hud {
            items: vec![
                (
                    format!("{} LIVE", c.live),
                    if c.live > 0 { ION } else { DIM },
                ),
                (format!("{} QUEUED", c.queued), DIM),
                (
                    format!(
                        "{} TASK{} ACTIVE",
                        c.tasks_active,
                        if c.tasks_active == 1 { "" } else { "S" }
                    ),
                    DIM,
                ),
            ],
            lamp: (c.attention > 0).then(|| format!("ATTENTION · {}", c.attention)),
        }
    }

    fn probe_rows(&self) -> Vec<ProbeRow> {
        HKEYS
            .iter()
            .filter_map(|hkey| {
                self.probes.get(*hkey).map(|cap| ProbeRow {
                    name: harness_title(hkey).into(),
                    hkey: hkey.to_string(),
                    ready: cap.mode != HarnessMode::Unsupported,
                })
            })
            .collect()
    }

    fn home_rows(&self) -> Vec<HomeTaskRow> {
        self.registry
            .tasks()
            .iter()
            .filter(|t| self.matches_task(t))
            .map(|t| {
                let status = self.registry.task_status(&t.task_ref).to_string();
                let sessions = self.registry.sessions_of(&t.task_ref);
                let managers = sessions
                    .iter()
                    .filter(|se| se.role == Role::Manager)
                    .map(|se| {
                        (
                            se.name.clone(),
                            harness_title(&se.harness).to_string(),
                            se.harness.clone(),
                        )
                    })
                    .collect();
                let repo_name = t
                    .repo
                    .file_name()
                    .map(|n| n.to_string_lossy().to_string())
                    .unwrap_or_default();
                HomeTaskRow {
                    name: t.name.clone(),
                    status: status.clone(),
                    managers,
                    // No synthetic activity: prototype sparklines are not live
                    // telemetry. Runtime status is the evidence we actually have.
                    spark: String::new(),
                    spark_live: false,
                    meta: format!(
                        "{} session{} · {repo_name} · {}",
                        sessions.len(),
                        if sessions.len() == 1 { "" } else { "s" },
                        t.server_profile_id
                            .as_deref()
                            .and_then(|id| self.registry.server(id))
                            .map(|p| p.name.clone())
                            .unwrap_or_else(|| t.connection.to_string()),
                    ),
                }
            })
            .collect()
    }

    fn matches_task(&self, task: &taskai_core::registry::Task) -> bool {
        let Mode::Find(query) = &self.mode else {
            return true;
        };
        let haystack = format!(
            "{} {} {} {} {}",
            task.task_ref,
            task.name,
            task.repo.display(),
            task.connection,
            task.server_profile_id
                .as_deref()
                .and_then(|id| self.registry.server(id))
                .map(|p| p.name.as_str())
                .unwrap_or("")
        )
        .to_lowercase();
        haystack.contains(&query.trim().to_lowercase())
    }

    fn open_task(&mut self, task_ref: String, cols: usize, rows: usize) {
        self.sel_task = Some(task_ref.clone());
        if let Some(profile) = self.selected_profile() {
            self.remote_probes.request_profile(&profile);
        }
        let rails = self.rail_rows(&task_ref);
        self.st.rail_sel = self
            .task_selections
            .get(&task_ref)
            .and_then(|key| rails.iter().position(|r| &r.key == key))
            .unwrap_or(0);
        self.mode = Mode::Task;
        self.focus = Focus::Nav;
        // Empty tasks stay in navigation unless their utility shell has already
        // been opened. Never create a shell simply by browsing a task.
        if let Some(selected) = self.selected_pane() {
            let opened = selected.hkey != "terminal"
                || self
                    .registry
                    .task(&task_ref)
                    .is_some_and(|t| t.terminal.is_some());
            if opened {
                if let Err(error) = self.focus_session(&selected.key, cols, rows, false) {
                    self.status = Some(format!("could not open CLI: {error}"));
                }
            }
        }
    }

    fn go_home(&mut self) {
        if let Some(task_ref) = self.sel_task.clone() {
            if let Some(selected) = self.selected_pane() {
                self.task_selections.insert(task_ref.clone(), selected.key);
            }
            if let Some(index) = self
                .registry
                .tasks()
                .iter()
                .position(|t| t.task_ref == task_ref)
            {
                self.st.home_sel = index;
            }
        }
        self.focus = Focus::Nav;
        self.mode = Mode::Home;
    }

    fn rail_rows(&self, task_ref: &str) -> Vec<RailRow> {
        let sessions = self.registry.sessions_of(task_ref);
        let mut ordered: Vec<&taskai_core::registry::Session> = Vec::new();
        for m in sessions.iter().filter(|se| se.role == Role::Manager) {
            ordered.push(m);
            for c in self.registry.children_of(&m.session_ref) {
                ordered.push(c);
            }
        }
        let mut rows: Vec<RailRow> = ordered
            .into_iter()
            .map(|se| {
                let mode = harness_mode(&se.harness);
                RailRow {
                    key: se.session_ref.clone(),
                    name: se.name.clone(),
                    harness: harness_title(&se.harness).into(),
                    hkey: se.harness.clone(),
                    live: se.state == SessionState::Live && se.harness != "grok",
                    depth: if se.role == Role::Manager { 0 } else { 1 },
                    conduit: mode.label().to_uppercase(),
                    state_word: se.state.label().into(),
                    ws: tilde(&se.workspace),
                }
            })
            .collect();
        if let Some(task) = self.registry.task(task_ref) {
            let key = terminal_ref(task_ref);
            let state = self.terminal_states.get(&key).copied().unwrap_or("OPEN");
            rows.push(RailRow {
                key,
                name: "Terminal".into(),
                harness: "Shell".into(),
                hkey: "terminal".into(),
                live: state == "RUNNING",
                depth: 0,
                conduit: "SHELL".into(),
                state_word: state.into(),
                ws: if task.connection == ConnectionTarget::Local {
                    tilde(&task.repo)
                } else {
                    task.repo.display().to_string()
                },
            });
        }
        rows
    }

    fn task_view(&mut self, cols: usize, rows: usize) -> Option<TaskView> {
        let task_ref = self.sel_task.clone()?;
        let t = self.registry.task(&task_ref)?.clone();
        let sessions = self.rail_rows(&task_ref);
        let harnesses: std::collections::HashSet<&str> = sessions
            .iter()
            .filter(|se| se.hkey != "terminal")
            .map(|se| se.hkey.as_str())
            .collect();
        let agent_count = sessions.iter().filter(|se| se.hkey != "terminal").count();
        let team_line = format!(
            "{} session{} · {} harness{}",
            agent_count,
            if agent_count == 1 { "" } else { "s" },
            harnesses.len(),
            if harnesses.len() == 1 { "" } else { "es" }
        );
        let body = if sessions.is_empty() {
            PaneBody::Empty("Add the first Manager to begin —  m".into())
        } else {
            let sel = self.st.rail_sel.min(sessions.len() - 1);
            let sref = sessions[sel].key.clone();
            if let Some(pending) = self.pending_launches.get(&sref) {
                PaneBody::Empty(format!(
                    "Connecting… {} input event(s) buffered · Ctrl+G returns to navigation",
                    pending.input.len()
                ))
            } else if let Some(entry) = self.panes.get(&sref) {
                let pane = &entry.pane;
                let h = rows.saturating_sub(8);
                let pw = cols - task::RAIL_W - task::GAP;
                let (body_h, body_w) = (h.saturating_sub(3), pw);
                let mut p = pane.lock().unwrap();
                if p.size() != (body_h, body_w) {
                    let _ = p.resize(body_h as u16, body_w as u16);
                    let _ =
                        self.resize_tx
                            .send((entry.runtime.clone(), body_h as u16, body_w as u16));
                }
                let snapshot = p.snapshot();
                drop(p);
                self.host_cursor = map_child_cursor(
                    snapshot.cursor,
                    TASK_PANE_BODY_TOP,
                    TASK_PANE_BODY_LEFT,
                    body_h,
                    body_w,
                    self.focus == Focus::Cli,
                );
                PaneBody::Cells(
                    snapshot
                        .cells
                        .into_iter()
                        .map(|r| {
                            r.into_iter()
                                .map(|c| taskai_tui::screens::pane::PaneCell {
                                    text: c.text,
                                    fg: c.fg,
                                    bg: c.bg,
                                    bold: c.bold,
                                    italic: c.italic,
                                    underline: c.underline,
                                    inverse: c.inverse,
                                    width: c.width,
                                })
                                .collect()
                        })
                        .collect(),
                )
            } else if sessions[sel].hkey == "terminal" {
                PaneBody::Empty(
                    "Enter opens Terminal in this repository · t opens it from navigation".into(),
                )
            } else if sessions[sel].hkey == "grok" {
                PaneBody::Handoff {
                    note: "HANDOFF-RESUME · Grok CLI does not stream. Prompts queue; results return on completion.".into(),
                    exchanges: Vec::new(),
                    cmd: format!("taskai session prompt {sref} --stdin --wait"),
                }
            } else {
                PaneBody::Empty(format!(
                    "Enter opens {} here · Ctrl+F full screen",
                    sessions[sel].harness
                ))
            }
        };
        Some(TaskView {
            crumb: format!("TASK · {}", t.name.to_uppercase()),
            title: t.name.clone(),
            repo: if t.connection == ConnectionTarget::Local {
                tilde(&t.repo)
            } else {
                t.repo.display().to_string()
            },
            connection: t
                .server_profile_id
                .as_deref()
                .and_then(|id| self.registry.server(id))
                .map(|p| p.name.clone())
                .unwrap_or_else(|| t.connection.to_string()),
            team_line,
            sessions,
            body,
        })
    }

    // ---- frame -----------------------------------------------------------

    fn frame(&mut self, cols: usize, rows: usize) -> Vec<String> {
        self.host_cursor = None;
        self.form_hits.clear();
        if matches!(self.mode, Mode::TaskCli | Mode::Attach) {
            let focused_pane_exists = self
                .selected_pane()
                .map(|pane| {
                    self.panes.contains_key(&pane.key)
                        || self.pending_launches.contains_key(&pane.key)
                })
                .unwrap_or(false);
            if !focused_pane_exists {
                self.mode = Mode::Task;
                self.focus = Focus::Nav;
                if self.status.is_none() {
                    self.status = Some(
                        "CLI viewer disconnected — Enter reconnects; navigation restored".into(),
                    );
                }
            }
        }
        self.sync_cli_mouse_press(cols, rows);
        let clock = self.clock();
        let mut frame = match &self.mode {
            Mode::Boot => {
                let done = self.probes.len();
                let ready = self
                    .probes
                    .values()
                    .filter(|c| c.mode != HarnessMode::Unsupported)
                    .count();
                let meter = format!("{ready} of {done} harnesses ready");
                boot::frame(&self.probe_rows(), &meter, self.st.boot_k, cols, rows)
            }
            Mode::Home => home::frame(
                &self.home_rows(),
                &self.hud(),
                self.st.home_sel,
                cols,
                rows,
                &clock,
            ),
            Mode::Find(query) => {
                let matches = self.home_rows();
                let mut base =
                    home::frame(&matches, &self.hud(), self.st.home_sel, cols, rows, &clock);
                base[3] = forms::input_row(query, self.editor, true, cols);
                if matches.is_empty() && rows > 8 {
                    base[7] = row(
                        &[s(
                            "No matching tasks — edit your search or Esc to clear",
                            Some(TEXT),
                            Some(VOID),
                        )],
                        cols,
                        VOID,
                        2,
                    );
                }
                modal_keybar(
                    base,
                    cols,
                    &[
                        ("↑/↓", "Select"),
                        ("Enter", "Open"),
                        ("Ctrl+U", "Clear"),
                        ("Esc", "All tasks"),
                    ],
                )
            }
            Mode::Settings => {
                let check = self.settings_check();
                self.settings
                    .frame(&self.registry, check.as_ref(), cols, rows)
            }
            Mode::Helper { selection, .. } => self.helper_frame(*selection, cols, rows),
            Mode::ServerConsole(key) => self.setup_frame(&key.clone(), cols, rows),
            Mode::ChooseServer { selection, .. } => {
                let servers = self.active_servers();
                let mut base = vec![vrow(cols); rows];
                base[1] = row(&[s("CHOOSE SERVER", Some(TEXT), Some(VOID))], cols, VOID, 3);
                let visible = rows.saturating_sub(9).div_euclid(3).max(1);
                let start = selection.saturating_sub(visible - 1);
                for (index, server) in servers.iter().enumerate().skip(start).take(visible) {
                    let y = 4 + (index - start) * 3;
                    let bg = if index == *selection {
                        taskai_tui::color::GLASS_HI
                    } else {
                        taskai_tui::color::GLASS
                    };
                    base[y] = row(
                        &[s(
                            format!(
                                "{}  {}",
                                if index == *selection { "❯" } else { " " },
                                server.name
                            ),
                            Some(TEXT),
                            Some(bg),
                        )],
                        cols,
                        bg,
                        3,
                    );
                    base[y + 1] = row(
                        &[s(
                            format!(
                                    "{} · {}",
                                    server.target,
                                    server
                                        .repository_root
                                        .as_ref()
                                        .map(|p| p.display().to_string())
                                        .unwrap_or_else(
                                            || "Choose a repository after selecting".into()
                                        )
                                ),
                            Some(FT),
                            Some(bg),
                        )],
                        cols,
                        bg,
                        6,
                    );
                }
                modal_keybar(
                    base,
                    cols,
                    &[("↑/↓", "Server"), ("Enter", "Use server"), ("Esc", "Back")],
                )
            }
            Mode::Task => match self.task_view(cols, rows) {
                Some(view) => task::frame(&view, &self.hud(), &self.st, cols, rows, &clock),
                None => home::frame(&self.home_rows(), &self.hud(), 0, cols, rows, &clock),
            },
            Mode::TaskCli => match self.task_view(cols, rows) {
                Some(view) => task::frame_with_focus(
                    &view,
                    &self.hud(),
                    &self.st,
                    self.focus,
                    cols,
                    rows,
                    &clock,
                ),
                None => home::frame(&self.home_rows(), &self.hud(), 0, cols, rows, &clock),
            },
            Mode::Attach => self.attach_frame(cols, rows),
            Mode::Help(prev) => {
                let prev = *prev;
                let base = self.base_frame(prev, cols, rows, &clock);
                modal_keybar(
                    centered_overlay(&base, &help_panel(), cols),
                    cols,
                    HELP_KEYS,
                )
            }
            Mode::NewTask(f) => {
                if let Some(ex) = &f.explorer {
                    let panel = live_explorer_panel(ex, cols);
                    let base = home::frame(
                        &self.home_rows(),
                        &self.hud(),
                        self.st.home_sel,
                        cols,
                        rows,
                        &clock,
                    );
                    modal_keybar(form_overlay(&base, &panel, cols), cols, EXPLORER_KEYS)
                } else {
                    let d = DrawerView {
                        title: "New task · Server → Repository → Task name".into(),
                        fields: vec![
                            (
                                "Server".into(),
                                self.registry
                                    .server(&f.server_id)
                                    .map(|p| format!("‹ {} ›", p.name))
                                    .unwrap_or_else(|| "Choose a server".into()),
                                f.focus == 2,
                            ),
                            ("Repository".into(), f.repo.clone(), f.focus == 1),
                            ("Task name".into(), f.name.clone(), f.focus == 0),
                        ],
                        keys: Vec::new(),
                    };
                    let note = vec![
                        vec![s("The server owns this repository and its harness settings. Manage servers in Settings.", Some(FT), Some(taskai_tui::color::GLASS))],
                        vec![s("Enter on Server selects a saved server. Tab moves fields; F2 browses repositories.", Some(FT), Some(taskai_tui::color::GLASS))],
                    ];
                    let base = home::frame(
                        &self.home_rows(),
                        &self.hud(),
                        self.st.home_sel,
                        cols,
                        rows,
                        &clock,
                    );
                    modal_keybar(
                        form_overlay(
                            &base,
                            &live_form_panel(
                                self.st.blink,
                                &d,
                                &note,
                                form_panel_width(cols),
                                self.editor,
                            ),
                            cols,
                        ),
                        cols,
                        &new_task_keys(f.focus),
                    )
                }
            }
            Mode::Manager(f) => {
                let hlabel = harness_label(&f.choices, f.hsel);
                let target = self.selected_connection().to_string();
                let note = harness_notes(f.choices.get(f.hsel), &target, form_panel_width(cols));
                let d = DrawerView {
                    title: "Add Manager".into(),
                    fields: vec![
                        ("Name".into(), f.name.clone(), f.focus == 0),
                        ("Harness".into(), hlabel, f.focus == 1),
                    ],
                    keys: Vec::new(),
                };
                let base = self.base_frame(PrevMode::Task, cols, rows, &clock);
                modal_keybar(
                    form_overlay(
                        &base,
                        &live_form_panel(
                            self.st.blink,
                            &d,
                            &note,
                            form_panel_width(cols),
                            self.editor,
                        ),
                        cols,
                    ),
                    cols,
                    FORM_KEYS,
                )
            }
            Mode::Sub(f) => {
                let parent_name = self
                    .registry
                    .session(&f.parent)
                    .map(|p| format!("{} · {}", p.name, harness_title(&p.harness)))
                    .unwrap_or_default();
                let hlabel = harness_label(&f.choices, f.hsel);
                let target = self.selected_connection().to_string();
                let remote_task = self
                    .sel_task
                    .as_ref()
                    .and_then(|task_ref| self.registry.task(task_ref))
                    .is_some_and(|task| task.connection != ConnectionTarget::Local);
                let ws_label = if remote_task {
                    "Same repository · remote Task".to_string()
                } else {
                    match f.ws_mode {
                        0 => "‹ Same repository as Manager ›".to_string(),
                        1 => "‹ New git worktree ›".to_string(),
                        _ => format!("‹ Another repository › {}", f.other),
                    }
                };
                let mut fields = vec![
                    ("Parent".into(), parent_name, false),
                    ("Role".into(), f.role.clone(), f.focus == 0),
                    ("Harness".into(), hlabel, f.focus == 1),
                    ("Workspace".into(), ws_label, f.focus == 2 && !remote_task),
                ];
                if f.ws_mode == 1 && !remote_task {
                    fields.push(("Base".into(), f.branch.clone(), f.focus == 3));
                    fields.push(("Worktree".into(), f.wname.clone(), f.focus == 4));
                } else if f.ws_mode == 2 && !remote_task {
                    fields.push(("Repository".into(), f.other.clone(), f.focus == 5));
                }
                let d = DrawerView {
                    title: "Add Subordinate".into(),
                    fields,
                    keys: Vec::new(),
                };
                let mut note =
                    harness_notes(f.choices.get(f.hsel), &target, form_panel_width(cols));
                if f.ws_mode == 1 && !remote_task {
                    let repo = self
                        .sel_task
                        .as_ref()
                        .and_then(|t| self.registry.task(t))
                        .map(|t| t.repo.clone())
                        .unwrap_or_default();
                    let choice = WorkspaceChoice::Worktree {
                        base: repo.clone(),
                        branch: f.branch.clone(),
                        name: f.wname.clone(),
                    };
                    note.push(vec![s(
                        format!("Worktree: {}", tilde(&preview(&choice, &repo))),
                        Some(FT),
                        Some(taskai_tui::color::GLASS),
                    )]);
                }
                if let Some(ex) = &f.explorer {
                    let panel = live_explorer_panel(ex, cols);
                    let base = self.base_frame(PrevMode::Task, cols, rows, &clock);
                    modal_keybar(form_overlay(&base, &panel, cols), cols, EXPLORER_KEYS)
                } else {
                    let mut keys = FORM_KEYS.to_vec();
                    if f.ws_mode == 2 && !remote_task {
                        keys.insert(3, ("F2", "Browse"));
                    }
                    let base = self.base_frame(PrevMode::Task, cols, rows, &clock);
                    modal_keybar(
                        form_overlay(
                            &base,
                            &live_form_panel(
                                self.st.blink,
                                &d,
                                &note,
                                form_panel_width(cols),
                                self.editor,
                            ),
                            cols,
                        ),
                        cols,
                        &keys,
                    )
                }
            }
            Mode::Destroy(f) => {
                let kept = [
                    "Repository untouched",
                    "Worktrees preserved",
                    "Git history preserved",
                ];
                let panel = destroy_panel_view(
                    self.st.blink,
                    &f.title,
                    &f.blast_lines,
                    &kept,
                    &f.expect,
                    &f.typed,
                );
                let prev = if f.back_home {
                    PrevMode::Home
                } else {
                    PrevMode::Task
                };
                let base = self.base_frame(prev, cols, rows, &clock);
                modal_keybar(centered_overlay(&base, &panel, cols), cols, DESTROY_KEYS)
            }
        };
        if matches!(self.mode, Mode::Home) {
            frame = modal_keybar(
                frame,
                cols,
                &[
                    ("Enter", "Open"),
                    ("n", "New task"),
                    ("s", "Settings"),
                    ("/", "Find"),
                    ("F1", "Setup helper"),
                    ("?", "Help"),
                    ("q", "Leave"),
                ],
            );
        } else if matches!(self.mode, Mode::Task) {
            frame = modal_keybar(
                frame,
                cols,
                &[
                    ("Enter", "Type"),
                    ("Ctrl+F", "Full screen"),
                    ("a", "Add agent"),
                    ("m", "Add manager"),
                    ("t", "Terminal"),
                    ("d", "Destroy agent"),
                    ("D", "Destroy task"),
                    ("s", "Settings"),
                    ("G", "Home"),
                    ("Esc", "Back"),
                    ("F1", "Setup helper"),
                    ("?", "Help"),
                    ("q", "Leave"),
                ],
            );
        } else if matches!(self.mode, Mode::TaskCli) {
            frame = modal_keybar(
                frame,
                cols,
                &[
                    ("Ctrl+F", "Full screen"),
                    ("Ctrl+G", "Navigation"),
                    ("Wheel", "Scroll CLI"),
                    ("Esc/End", "Leave history"),
                ],
            );
        } else if self.pending_task.is_some() {
            frame = modal_keybar(frame, cols, &[("Esc", "Cancel check")]);
        }
        if let Some(msg) = &self.status {
            if let Some(last) = frame.last_mut() {
                let clipped: String = single_line_ui_text(msg)
                    .chars()
                    .take(cols.saturating_sub(4))
                    .collect();
                *last = row(&[s(clipped, Some(FT), Some(VOID))], cols, VOID, 2);
            }
        }
        if let Some(action) = &self.helper_action {
            if matches!(
                self.mode,
                Mode::Home | Mode::Task | Mode::TaskCli | Mode::Attach
            ) {
                let hint = if self.focus == Focus::Cli {
                    "Ctrl+G, then F8 to review"
                } else {
                    "F8 review · Esc dismiss"
                };
                frame[rows - 1] = row(
                    &[s(
                        format!(
                            "Helper requests {} {} · {hint} · nothing applied yet",
                            action.action,
                            action.argument.as_deref().unwrap_or("")
                        ),
                        Some(ION),
                        Some(VOID),
                    )],
                    cols,
                    VOID,
                    2,
                );
            }
        }
        let (fields, notes): (Vec<Option<usize>>, usize) = match &self.mode {
            Mode::NewTask(f) if f.explorer.is_none() => (vec![Some(2), Some(1), Some(0)], 2),
            Mode::Manager(_) => (vec![Some(0), Some(1)], 2),
            Mode::Sub(f) if f.explorer.is_none() => {
                let remote = self
                    .sel_task
                    .as_ref()
                    .and_then(|r| self.registry.task(r))
                    .is_some_and(|t| t.connection != ConnectionTarget::Local);
                if f.ws_mode == 1 && !remote {
                    (vec![None, Some(0), Some(1), Some(2), Some(3), Some(4)], 3)
                } else if f.ws_mode == 2 && !remote {
                    (vec![None, Some(0), Some(1), Some(2), Some(5)], 2)
                } else {
                    (
                        vec![None, Some(0), Some(1), if remote { None } else { Some(2) }],
                        2,
                    )
                }
            }
            _ => (Vec::new(), 0),
        };
        let height = 5 + fields.len() * 3 + notes;
        let available = rows.saturating_sub(3);
        let top = (available.saturating_sub(height) / 2)
            .min(available.saturating_sub(1).saturating_sub(height));
        let width = form_panel_width(cols);
        let left = cols.saturating_sub(width) / 2;
        self.form_hits = fields
            .iter()
            .enumerate()
            .filter_map(|(i, field)| field.map(|f| (f, top + 4 + i * 3, left, width)))
            .collect();
        frame
    }

    fn base_frame(&mut self, prev: PrevMode, cols: usize, rows: usize, clock: &str) -> Vec<String> {
        match prev {
            PrevMode::Home => home::frame(
                &self.home_rows(),
                &self.hud(),
                self.st.home_sel,
                cols,
                rows,
                clock,
            ),
            PrevMode::Task => match self.task_view(cols, rows) {
                Some(view) => task::frame(&view, &self.hud(), &self.st, cols, rows, clock),
                None => home::frame(&self.home_rows(), &self.hud(), 0, cols, rows, clock),
            },
            PrevMode::Settings => {
                let check = self.settings_check();
                self.settings
                    .frame(&self.registry, check.as_ref(), cols, rows)
            }
            PrevMode::Helper(selection, _) => self.helper_frame(selection, cols, rows),
        }
    }

    fn attach_frame(&mut self, cols: usize, rows: usize) -> Vec<String> {
        let Some(sess) = self.selected_pane() else {
            self.mode = Mode::Task;
            self.focus = Focus::Nav;
            return vec![vrow(cols); rows];
        };
        let sref = sess.key.clone();
        let b = GLASS_IN;
        let stripe = taskai_tui::color::harness_color(&sess.hkey);
        let mode_label = sess.conduit.clone();
        let mut rib: Vec<Seg> = vec![
            s("▌", Some(stripe), Some(b)),
            s("  ", None, Some(b)),
            s(
                format!("{} · {} · {sref}", sess.name, sess.harness),
                Some(DIM),
                Some(b),
            ),
        ];
        let mid = vec![s(mode_label, Some(FT), Some(b))];
        let tail = vec![
            s(" Ctrl+F ", Some(TEXT), Some(KCAP)),
            s(" Inline", Some(FT), Some(b)),
            s(" Ctrl+G ", Some(TEXT), Some(KCAP)),
            s(" Navigation", Some(FT), Some(b)),
        ];
        rib = clip_segs(&rib, cols.saturating_sub(seglen(&mid) + seglen(&tail) + 5));
        let gap1 =
            (cols as isize * 2 / 3 - seglen(&rib) as isize - seglen(&mid) as isize).max(0) as usize;
        rib.push(s(" ".repeat(gap1), None, Some(b)));
        rib.extend(mid);
        let gap2 =
            (cols as isize - 3 - seglen(&rib) as isize - seglen(&tail) as isize).max(0) as usize;
        rib.push(s(" ".repeat(gap2), None, Some(b)));
        rib.extend(tail);
        let mut lines = vec![
            taskai_tui::chrome::focus_edge(cols, "CLI INPUT", self.focus == Focus::Cli),
            row(&rib, cols, b, 0),
            row(&[s("▀".repeat(cols), Some(b), Some(VOID))], cols, VOID, 0),
            vrow(cols),
        ];
        let body_h = rows.saturating_sub(lines.len() + 1);
        if let Some(entry) = self.panes.get(&sref) {
            let mut p = entry.pane.lock().unwrap();
            if p.size() != (body_h, cols) {
                let _ = p.resize(body_h as u16, cols as u16);
                let _ = self
                    .resize_tx
                    .send((entry.runtime.clone(), body_h as u16, cols as u16));
            }
            let snapshot = p.snapshot();
            drop(p);
            self.host_cursor = map_child_cursor(
                snapshot.cursor,
                ATTACH_BODY_TOP,
                0,
                body_h,
                cols,
                self.focus == Focus::Cli,
            );
            for r in 0..body_h {
                match snapshot.cells.get(r) {
                    Some(cr) => {
                        let cells: Vec<PaneCell> = cr
                            .iter()
                            .map(|cell| PaneCell {
                                text: cell.text.clone(),
                                fg: cell.fg,
                                bg: cell.bg,
                                bold: cell.bold,
                                italic: cell.italic,
                                underline: cell.underline,
                                inverse: cell.inverse,
                                width: cell.width,
                            })
                            .collect();
                        lines.push(cell_row(&cells, cols, CLI_BG));
                    }
                    None => lines.push(row(&[], cols, CLI_BG, 0)),
                }
            }
        } else {
            for _ in 0..body_h {
                lines.push(row(&[], cols, CLI_BG, 0));
            }
        }
        lines.push(row(
            &[s(
                "Wheel  Scroll CLI · Esc / End  Leave tmux history · Ctrl+G  Navigation",
                Some(TEXT),
                Some(VOID),
            )],
            cols,
            VOID,
            0,
        ));
        let _ = format!("{}{}", bg(VOID), RESET);
        lines.truncate(rows);
        lines
    }

    // ---- helpers ---------------------------------------------------------

    fn watch_claude_binding(&self, sref: &str, events: PathBuf, offset: u64) {
        let tx = self.bind_tx.clone();
        let sref = sref.to_string();
        std::thread::spawn(move || {
            if let Ok(event) = wait_for_event(
                &events,
                "SessionStart",
                None,
                offset,
                Duration::from_secs(300),
            ) {
                if let Some(session_id) = event.session_id {
                    let _ = tx.send((sref, session_id));
                }
            }
        });
    }

    fn selected_connection(&self) -> ConnectionTarget {
        self.sel_task
            .as_ref()
            .and_then(|task_ref| self.registry.task(task_ref))
            .map(|task| task.connection.clone())
            .unwrap_or(ConnectionTarget::Local)
    }

    fn harness_choices(&self) -> Vec<HarnessAvailability> {
        let connection = self.selected_connection();
        let profile = self.selected_profile();
        if let Some(profile) = &profile {
            if let Some(Ok(choices)) = self.remote_probes.get_profile(profile) {
                return choices.clone();
            }
        }
        if connection != ConnectionTarget::Local {
            return match profile
                .as_ref()
                .and_then(|p| self.remote_probes.get_profile(p))
            {
                Some(Ok(choices)) => choices.clone(),
                result => HKEYS
                    .iter()
                    .map(|h| HarnessAvailability {
                        auth: crate::remote_probes::AuthState::Unknown,
                        hkey: (*h).into(),
                        version: None,
                        unavailable: Some(match result {
                            Some(Err(error)) => error.clone(),
                            _ => "Checking remote CLIs… wait or press F5 to recheck".into(),
                        }),
                    })
                    .collect(),
            };
        }
        HKEYS
            .iter()
            .map(|h| {
                let cap = self.probes.get(*h);
                let config = profile.as_ref().and_then(|p| p.harnesses.get(*h));
                let configured = config.and_then(|c| {
                    if !c.enabled {
                        Some("Disabled in server Settings".into())
                    } else if c.executable.is_some() || c.model.is_some() {
                        Some("Checking configured server harness… F5 rechecks".into())
                    } else {
                        None
                    }
                });
                HarnessAvailability {
                    auth: crate::remote_probes::AuthState::Unknown,
                    hkey: (*h).into(),
                    version: cap.and_then(|c| c.version.clone()),
                    unavailable: configured.or_else(|| match cap {
                        Some(c) if c.mode != HarnessMode::Unsupported => None,
                        Some(c) => Some(c.detail.clone()),
                        None => Some("Checking local CLI… F5 rechecks".into()),
                    }),
                }
            })
            .collect()
    }

    fn sync_form_harnesses(&mut self) {
        let choices = self.harness_choices();
        let (current, selected) = match &mut self.mode {
            Mode::Manager(f) => (&mut f.choices, &mut f.hsel),
            Mode::Sub(f) => (&mut f.choices, &mut f.hsel),
            _ => return,
        };
        let key = current.get(*selected).map(|c| c.hkey.clone());
        *selected = key
            .and_then(|key| choices.iter().position(|c| c.hkey == key))
            .unwrap_or(0);
        *current = choices;
    }

    fn refresh_harnesses(&mut self) {
        let target = self.selected_connection();
        if let Some(profile) = self.selected_profile() {
            self.remote_probes.refresh_profile(&profile);
        } else if target == ConnectionTarget::Local {
            // The existing boot/recheck worker is still active; do not pile up
            // subprocesses if F5 is held down.
            if self.probes.len() == HKEYS.len() {
                let (tx, rx) = channel();
                self.probe_rx = rx;
                self.probes.clear();
                probe_local_harnesses(tx);
            }
        } else {
            self.remote_probes.refresh(&target);
        }
        self.sync_form_harnesses();
        self.status = Some(format!(
            "Rechecking CLIs on {target}… your form and selected harness are preserved"
        ));
    }

    fn wait_for_remote_harnesses(&mut self, action: char) -> bool {
        let Some(task) = self
            .sel_task
            .as_ref()
            .and_then(|r| self.registry.task(r))
            .cloned()
        else {
            return false;
        };
        let Some(profile) = self.selected_profile() else {
            return false;
        };
        if task.connection == ConnectionTarget::Local
            && profile
                .harnesses
                .values()
                .all(|c| c.enabled && c.executable.is_none() && c.model.is_none())
        {
            return false;
        }
        self.remote_probes.request_profile(&profile);
        if self.remote_probes.get_profile(&profile).is_none() {
            self.pending_harness_action = Some((task.task_ref.clone(), action));
            self.status = Some(format!(
                "Checking provider CLIs on {}… Esc or navigation dismisses this request",
                task.connection
            ));
            return true;
        }
        false
    }

    fn poll_harness_action(&mut self, cols: usize, rows: usize) {
        if self.remote_probes.poll() {
            self.sync_form_harnesses();
            if matches!(self.mode, Mode::Manager(_) | Mode::Sub(_)) {
                self.status = Some(
                    "CLI check complete · unavailable harnesses explain why · F5 rechecks".into(),
                );
            }
        }
        let Some((task_ref, action)) = self.pending_harness_action.clone() else {
            return;
        };
        if !matches!(self.mode, Mode::Task) || self.sel_task.as_ref() != Some(&task_ref) {
            self.pending_harness_action = None;
            return;
        }
        let done = self
            .selected_profile()
            .is_some_and(|p| self.remote_probes.get_profile(&p).is_some());
        if done {
            self.pending_harness_action = None;
            self.handle(
                KeyEvent::new(KeyCode::Char(action), KeyModifiers::NONE),
                cols,
                rows,
            );
        }
    }

    fn selected_session(&self) -> Option<(String, taskai_core::registry::Session)> {
        let task_ref = self.sel_task.clone()?;
        let rails = self.rail_rows(&task_ref);
        let sel = self.st.rail_sel.min(rails.len().checked_sub(1)?);
        let sref = rails[sel].key.clone();
        self.registry.session(&sref).cloned().map(|se| (sref, se))
    }

    fn selected_pane(&self) -> Option<RailRow> {
        let rows = self.rail_rows(self.sel_task.as_deref()?);
        rows.get(self.st.rail_sel.min(rows.len().checked_sub(1)?))
            .cloned()
    }

    /// A visible terminal is interactive by default. Only an explicit navigation
    /// boundary leaves the pane as a preview; failed launches stay in navigation.
    fn focus_session(
        &mut self,
        sref: &str,
        cols: usize,
        rows: usize,
        fullscreen: bool,
    ) -> Result<(), String> {
        self.mode = Mode::Task;
        self.focus = Focus::Nav;
        if !self
            .panes
            .get(sref)
            .is_some_and(|entry| entry.pane.lock().unwrap().is_alive())
            && !self.pending_launches.contains_key(sref)
        {
            self.panes.remove(sref);
            let request = self.launch_request(sref, cols.max(MIN_COLS), rows.max(MIN_ROWS))?;
            let task_ref = self.sel_task.clone().ok_or("no task selected")?;
            let target = request.runtime.target.clone();
            let (tx, rx) = channel();
            std::thread::spawn(move || {
                let _ = tx.send(launch::run(request));
            });
            self.pending_launches.insert(
                sref.into(),
                PendingLaunch {
                    task_ref,
                    result: rx,
                    input: self.buffered_input.remove(sref).unwrap_or_default(),
                },
            );
            self.status = Some(format!(
                "Opening {sref} on {target}… input is buffered · Ctrl+G navigation"
            ));
        }
        self.mode = if fullscreen {
            Mode::Attach
        } else {
            Mode::TaskCli
        };
        self.focus = Focus::Cli;
        Ok(())
    }

    fn launch_request(
        &mut self,
        sref: &str,
        cols: usize,
        rows: usize,
    ) -> Result<launch::Request, String> {
        let task_ref = self.sel_task.clone().ok_or("no task selected")?;
        let task = self
            .registry
            .task(&task_ref)
            .cloned()
            .ok_or("task missing")?;
        let server = private_server_name(&self.home_dir);
        let (session, runtime, repo) = if sref == terminal_ref(&task_ref) {
            let runtime = self
                .registry
                .bind_terminal(&task_ref, &server)
                .map_err(|e| e.to_string())?;
            self.terminal_states.insert(sref.into(), "CONNECTING");
            (None, runtime, task.repo)
        } else {
            let session = self
                .registry
                .session(sref)
                .cloned()
                .ok_or("unknown session")?;
            if session.task_ref != task_ref {
                return Err("session is not in this task".into());
            }
            if session.runtime.is_none() {
                self.registry
                    .bind_runtime(sref, &server)
                    .map_err(|e| e.to_string())?;
            }
            let session = self
                .registry
                .session(sref)
                .cloned()
                .ok_or("session missing")?;
            let runtime = session.runtime.clone().ok_or("runtime mapping missing")?;
            let repo = session.workspace.clone();
            (Some(session), runtime, repo)
        };
        Ok(launch::Request {
            session,
            runtime,
            home_dir: self.home_dir.clone(),
            repo,
            rows: rows.saturating_sub(11).max(10) as u16,
            cols: cols.saturating_sub(TASK_PANE_BODY_LEFT).max(1) as u16,
        })
    }

    fn accept_launch(&mut self, key: &str, outcome: launch::Outcome) {
        let pane = Arc::new(Mutex::new(outcome.pane));
        let runtime = outcome.runtime;
        if let Some(session) = self.registry.session(key).cloned() {
            if runtime.target == ConnectionTarget::Local {
                self.served
                    .panes
                    .lock()
                    .unwrap()
                    .insert(key.into(), pane.clone());
                self.served
                    .harnesses
                    .lock()
                    .unwrap()
                    .insert(key.into(), session.harness.clone());
                if let Some(provider) = session.provider {
                    self.served
                        .session_ids
                        .lock()
                        .unwrap()
                        .insert(key.into(), provider.provider_session_id);
                }
                if let Some((events, offset)) = outcome.claude_events {
                    self.watch_claude_binding(key, events, offset);
                }
            }
            let state = match session.harness.as_str() {
                "grok" => SessionState::Handoff,
                "claude" if outcome.created && runtime.target == ConnectionTarget::Local => {
                    SessionState::Working
                }
                _ => SessionState::Live,
            };
            let _ = self.registry.set_state(key, state);
        } else {
            self.terminal_states.insert(key.into(), "RUNNING");
        }
        // A spawned attach client is not ready for input until its first native
        // frame. tmux switches the viewer PTY into raw mode during attachment;
        // earlier input can be echoed and flushed instead of reaching the CLI.
        self.buffered_input.entry(key.into()).or_default();
        self.panes.insert(key.into(), PaneEntry { pane, runtime });
    }

    fn poll_launches(&mut self) {
        let done: Vec<_> = self
            .pending_launches
            .iter()
            .filter_map(|(key, pending)| match pending.result.try_recv() {
                Ok(result) => Some((key.clone(), result)),
                Err(std::sync::mpsc::TryRecvError::Disconnected) => Some((
                    key.clone(),
                    Err(
                        "launch worker disconnected; the saved runtime mapping was retained".into(),
                    ),
                )),
                Err(std::sync::mpsc::TryRecvError::Empty) => None,
            })
            .collect();
        for (key, result) in done {
            let pending = self.pending_launches.remove(&key).unwrap();
            match result {
                Ok(outcome) => {
                    self.accept_launch(&key, outcome);
                    self.buffered_input
                        .entry(key.clone())
                        .or_default()
                        .extend(pending.input);
                    if self.selected_pane().is_some_and(|p| p.key == key) {
                        self.status = Some(format!("{key} connected · Ctrl+G navigation"));
                    }
                }
                Err(error) => {
                    let buffered = pending.input.len();
                    if buffered > 0 {
                        self.buffered_input.insert(key.clone(), pending.input);
                    }
                    if self.registry.session(&key).is_some() {
                        let _ = self.registry.set_state(&key, SessionState::Queued);
                    } else {
                        self.terminal_states.insert(key.clone(), "UNAVAILABLE");
                    }
                    if self.selected_pane().is_some_and(|p| p.key == key) {
                        self.mode = Mode::Task;
                        self.focus = Focus::Nav;
                    }
                    if self.sel_task.as_ref() == Some(&pending.task_ref) {
                        self.status = Some(format!("Could not open {key}: {error} · {buffered} input event(s) retained · Enter retries"));
                    }
                }
            }
        }
        let ready: Vec<String> = self
            .buffered_input
            .keys()
            .filter(|key| {
                self.panes
                    .get(*key)
                    .is_some_and(|e| e.pane.lock().unwrap().has_output())
            })
            .cloned()
            .collect();
        for key in ready {
            let input = self.buffered_input.remove(&key).unwrap();
            if let Some(entry) = self.panes.get(&key) {
                let mut pane = entry.pane.lock().unwrap();
                for item in input {
                    let bytes = match item {
                        BufferedInput::Bytes(bytes) => bytes,
                        BufferedInput::Paste(text) => {
                            terminal_paste_bytes(&text, pane.snapshot().bracketed_paste)
                        }
                    };
                    let _ = pane.write_bytes(&bytes);
                }
            }
        }
    }

    fn poll_disconnected(&mut self) {
        let done: Vec<_> = self
            .disconnected
            .iter()
            .filter_map(|(key, (rx, previous))| {
                rx.try_recv()
                    .ok()
                    .map(|result| (key.clone(), result, *previous))
            })
            .collect();
        for (key, result, previous) in done {
            self.disconnected.remove(&key);
            if self.panes.contains_key(&key) || self.pending_launches.contains_key(&key) {
                continue;
            }
            let alive = matches!(result, Ok(RuntimeStatus::Live { .. }));
            if let Some(session) = self.registry.session(&key).cloned() {
                if alive {
                    let state = match previous {
                        Some(
                            state @ (SessionState::Complete
                            | SessionState::Working
                            | SessionState::Attention
                            | SessionState::Handoff),
                        ) => state,
                        _ if session.harness == "grok" => SessionState::Handoff,
                        _ => SessionState::Live,
                    };
                    let _ = self.registry.set_state(&key, state);
                } else if matches!(self.mode, Mode::Boot | Mode::Home) {
                    let message = match &result {
                        Ok(RuntimeStatus::Missing) => {
                            format!("{key}: recorded tmux session is missing")
                        }
                        Ok(_) => format!("{key}: runtime exited"),
                        Err(error) => {
                            format!("{key}: could not inspect its saved runtime: {error}")
                        }
                    };
                    self.status = Some(match self.status.take() {
                        Some(prior) => format!("{prior} · {message}"),
                        None => message,
                    });
                }
            }
            if self
                .registry
                .tasks()
                .iter()
                .any(|t| t.terminal.as_ref().is_some_and(|r| r.session == key))
            {
                self.terminal_states.insert(
                    key.clone(),
                    match result {
                        Ok(RuntimeStatus::Live { .. }) => "RUNNING",
                        Ok(_) => "EXITED",
                        Err(_) => "UNAVAILABLE",
                    },
                );
            }
            if self.selected_pane().is_some_and(|p| p.key == key) {
                self.status = Some(if alive {
                    "Viewer disconnected; the tmux process is still running — Enter reconnects"
                        .into()
                } else if self.terminal_states.get(&key) == Some(&"EXITED") {
                    "Terminal exited — Enter opens a new shell in the repository".into()
                } else {
                    "CLI stopped or could not be verified — Enter retries the saved runtime".into()
                });
            }
        }
    }

    fn forward_cli_bytes(&mut self, key: &str, bytes: &[u8]) {
        if let Some(buffer) = self.buffered_input.get_mut(key) {
            buffer.push(BufferedInput::Bytes(bytes.to_vec()));
        } else if let Some(entry) = self.panes.get(key) {
            let _ = entry.pane.lock().unwrap().write_bytes(bytes);
        } else if let Some(pending) = self.pending_launches.get_mut(key) {
            if pending.input.len() < 4096 {
                pending.input.push(BufferedInput::Bytes(bytes.to_vec()));
            } else {
                self.status =
                    Some("Opening CLI: input buffer is full; wait before typing more".into());
            }
        }
    }

    #[cfg(test)]
    fn spawn_pane(&mut self, sref: &str, cols: usize, rows: usize) -> Result<(), String> {
        self.sel_task = self
            .registry
            .session(sref)
            .map(|session| session.task_ref.clone());
        let request = self.launch_request(sref, cols, rows)?;
        let outcome = launch::run(request)?;
        self.accept_launch(sref, outcome);
        Ok(())
    }

    fn create_manager(&mut self, f: &ManagerForm) -> Result<String, String> {
        let task_ref = self.sel_task.clone().ok_or("no task selected")?;
        let repo = self
            .registry
            .task(&task_ref)
            .ok_or("task missing")?
            .repo
            .clone();
        let hkey = require_harness(&f.choices, f.hsel)?;
        let name = if f.name.trim().is_empty() {
            "Lead"
        } else {
            f.name.trim()
        };
        self.registry
            .create_session(&task_ref, None, name, &hkey, &repo, Source::HumanUi)
            .map_err(|e| e.to_string())
    }

    fn create_sub(&mut self, f: &SubForm) -> Result<String, String> {
        let task_ref = self.sel_task.clone().ok_or("no task selected")?;
        let task = self.registry.task(&task_ref).ok_or("task missing")?;
        let repo = task.repo.clone();
        let connection = task.connection.clone();
        let hkey = require_harness(&f.choices, f.hsel)?;
        let parent = self
            .registry
            .session(&f.parent)
            .ok_or("the selected Manager no longer exists")?;
        if parent.task_ref != task_ref || parent.role != Role::Manager {
            return Err("select a Manager in this task before adding a Subordinate".into());
        }
        if f.ws_mode == 1 && f.wname.trim().is_empty() {
            return Err("Enter a worktree name, or choose Same repository".into());
        }
        if connection != ConnectionTarget::Local && f.ws_mode != 0 {
            return Err("remote Subordinates currently use the Task repository".into());
        }
        let choice = match f.ws_mode {
            0 => WorkspaceChoice::SameRepo,
            1 => WorkspaceChoice::Worktree {
                base: repo.clone(),
                branch: f.branch.clone(),
                name: f.wname.trim().to_string(),
            },
            _ => {
                let raw = f.other.trim();
                let path = if raw == "~" || raw.starts_with("~/") {
                    PathBuf::from(std::env::var("HOME").unwrap_or_default())
                        .join(raw.trim_start_matches('~').trim_start_matches('/'))
                } else {
                    PathBuf::from(raw)
                };
                let path = path
                    .canonicalize()
                    .map_err(|e| format!("Could not open repository {raw}: {e}"))?;
                WorkspaceChoice::OtherRepo(path)
            }
        };
        let ws = provision(&choice, &repo).map_err(|e| e.to_string())?;
        let role = if f.role.trim().is_empty() {
            "Subordinate"
        } else {
            f.role.trim()
        };
        self.registry
            .create_session(
                &task_ref,
                Some(&f.parent),
                role,
                &hkey,
                &ws,
                Source::HumanUi,
            )
            .map_err(|e| e.to_string())
    }

    fn destroy_item(&mut self, item: &Item) -> Result<Vec<String>, String> {
        let refs = self.registry.blast(item).map_err(|e| e.to_string())?;
        let opening = self
            .pending_launches
            .iter()
            .any(|(key, pending)| match item {
                Item::Task(task) => &pending.task_ref == task,
                Item::Session(_) => refs.contains(key),
            });
        if opening {
            return Err("A CLI in this scope is still opening. Wait for it to finish, then confirm again; no records were removed".into());
        }
        let mut runtimes: Vec<TmuxRuntimeLocator> = refs
            .iter()
            .filter_map(|sref| {
                self.registry
                    .session(sref)
                    .and_then(|session| session.runtime.clone())
            })
            .collect();

        if let Item::Task(task_ref) = item {
            if let Some(runtime) = self
                .registry
                .task(task_ref)
                .and_then(|t| t.terminal.clone())
            {
                self.panes.remove(&runtime.session);
                runtimes.push(runtime);
            }
        }

        // Disconnect viewers first. The exact tmux sessions remain alive until the
        // explicit kill below succeeds; an unreachable SSH target keeps JSON intact.
        for sref in &refs {
            self.buffered_input.remove(sref);
            self.panes.remove(sref);
            self.served.panes.lock().unwrap().remove(sref);
            self.served.session_ids.lock().unwrap().remove(sref);
            self.served.harnesses.lock().unwrap().remove(sref);
        }
        for runtime in &runtimes {
            tmux::kill(runtime).map_err(|e| {
                format!(
                    "could not stop {} on {}: {e}; tracker records were preserved",
                    runtime.session, runtime.target
                )
            })?;
        }
        let removed = self
            .registry
            .destroy(item, Source::HumanUi)
            .map_err(|e| e.to_string())?;
        if let Item::Task(task_ref) = item {
            self.terminal_states.remove(&terminal_ref(task_ref));
        }
        Ok(removed)
    }

    fn handle_paste(&mut self, text: &str) {
        if self.pending_task.is_some() {
            return;
        }
        if matches!(self.mode, Mode::Settings) {
            self.settings.paste(text);
            return;
        }
        if let Mode::ServerConsole(key) = &self.mode {
            let key = key.clone();
            if let Some(buffer) = self.buffered_input.get_mut(&key) {
                if text.len() <= 65_536 {
                    buffer.push(BufferedInput::Paste(text.into()));
                } else {
                    self.status =
                        Some("Wait for the setup console before pasting more than 64 KiB".into());
                }
            } else if let Some(entry) = self.panes.get(&key) {
                let mut pane = entry.pane.lock().unwrap();
                let bytes = terminal_paste_bytes(text, pane.snapshot().bracketed_paste);
                let _ = pane.write_bytes(&bytes);
            }
            return;
        }
        if self.focus == Focus::Cli && matches!(self.mode, Mode::TaskCli | Mode::Attach) {
            if let Some(selected) = self.selected_pane() {
                if let Some(pending) = self.pending_launches.get_mut(&selected.key) {
                    if text.len() <= 65_536 {
                        pending.input.push(BufferedInput::Paste(text.into()));
                    } else {
                        self.status = Some(
                            "Wait for the CLI to connect before pasting more than 64 KiB".into(),
                        );
                    }
                } else if let Some(buffer) = self.buffered_input.get_mut(&selected.key) {
                    buffer.push(BufferedInput::Paste(text.into()));
                } else if let Some(entry) = self.panes.get(&selected.key) {
                    let mut pane = entry.pane.lock().unwrap();
                    let bytes = terminal_paste_bytes(text, pane.snapshot().bracketed_paste);
                    let _ = pane.write_bytes(&bytes);
                }
            }
            return;
        }

        let finding = matches!(self.mode, Mode::Find(_));
        let path = matches!(&self.mode, Mode::NewTask(f) if f.focus == 1)
            || matches!(&self.mode, Mode::Sub(f) if f.focus == 5);
        if let Some(value) = active_field(&mut self.mode) {
            let text = forms::clean_paste(text, path);
            if text.is_empty() {
                return;
            }
            // A pasted absolute path/SSH command replaces a default or previous
            // value; partial pastes still insert at the visible caret.
            if (path && (text.starts_with('/') || text.starts_with("~/")))
                || (!path && text.starts_with("ssh ") && value.starts_with("ssh "))
            {
                self.editor.selected = true;
            }
            self.editor.insert(value, &text);
            if finding {
                self.st.home_sel = 0;
            }
            return;
        }
        let text = single_line_ui_text(text.trim_end_matches(['\r', '\n']));
        if let Mode::Destroy(form) = &mut self.mode {
            form.typed.push_str(&text);
        }
    }

    // ---- input -----------------------------------------------------------

    fn handle(&mut self, k: KeyEvent, cols: usize, rows: usize) -> bool {
        self.sync_cli_mouse_press(cols, rows);
        let keep_running = self.handle_key(k, cols, rows) && !self.leave_requested;
        self.sync_cli_mouse_press(cols, rows);
        if !keep_running {
            self.cancel_cli_mouse_press();
        }
        keep_running
    }

    fn handle_key(&mut self, k: KeyEvent, cols: usize, rows: usize) -> bool {
        // A resize must not trap users inside a form. Escape is safe without
        // visible geometry; all editing/activation still waits for a usable view.
        let compact_escape = k.code == KeyCode::Esc
            && self.focus == Focus::Nav
            && !matches!(
                self.mode,
                Mode::TaskCli | Mode::Attach | Mode::ServerConsole(_)
            );
        if (cols < MIN_COLS || rows < MIN_ROWS) && !compact_escape {
            if k.code == KeyCode::Char('g')
                && k.modifiers.contains(KeyModifiers::CONTROL)
                && self.focus == Focus::Cli
            {
                self.mode = if matches!(self.mode, Mode::ServerConsole(_)) {
                    Mode::Settings
                } else {
                    Mode::Task
                };
                self.focus = Focus::Nav;
            } else if self.focus == Focus::Nav
                && k.code == KeyCode::Char('q')
                && (matches!(
                    self.mode,
                    Mode::Home | Mode::Task | Mode::Helper { .. } | Mode::Help(_)
                ) || matches!(self.mode, Mode::Settings) && !self.settings.has_dialog())
            {
                return false;
            }
            return true;
        }
        if self.pending_task.is_some() {
            if k.code == KeyCode::Esc {
                self.pending_task = None;
                self.status = Some("Repository check cancelled; your draft is preserved".into());
            }
            return true;
        }
        if self.focus == Focus::Nav && matches!(self.mode, Mode::Home | Mode::Task) {
            if k.code == KeyCode::F(1) {
                self.open_helper(cols, rows);
                return true;
            }
            if self.helper_action.is_some() && k.code == KeyCode::Esc {
                self.helper_action = None;
                self.status = Some("Helper request dismissed; nothing was changed".into());
                return true;
            }
            if self.helper_action.is_some() && k.code == KeyCode::F(8) {
                self.review_helper_action(cols, rows);
                return true;
            }
        }
        if k.code == KeyCode::F(5) && matches!(self.mode, Mode::Manager(_) | Mode::Sub(_)) {
            self.refresh_harnesses();
            return true;
        }
        if matches!(self.mode, Mode::Home | Mode::Task) && k.code == KeyCode::Char(',') {
            self.open_settings();
            return true;
        }
        if matches!(&self.mode, Mode::NewTask(f) if f.focus == 2 && f.explorer.is_none())
            && k.code == KeyCode::Enter
        {
            self.open_server_picker();
            return true;
        }
        let finding = matches!(self.mode, Mode::Find(_));
        if let Some(value) = active_field(&mut self.mode) {
            if self.editor.key(value, k) {
                if finding {
                    self.st.home_sel = 0;
                }
                self.status = None;
                return true;
            }
        }
        let previous = field_identity(&self.mode);
        let result = self.handle_inner(k, cols, rows);
        if previous != field_identity(&self.mode) {
            self.editor = Editor {
                cursor: None,
                selected: active_field(&mut self.mode).is_some_and(|v| !v.is_empty()),
            };
        }
        result
    }

    fn cli_viewport(&self, cols: usize, rows: usize) -> Option<CliViewport> {
        if cols < MIN_COLS || rows < MIN_ROWS {
            return None;
        }
        let (key, left, top, width, height) = match &self.mode {
            Mode::Task | Mode::TaskCli => (
                self.selected_pane()?.key,
                TASK_PANE_BODY_LEFT,
                TASK_PANE_BODY_TOP,
                cols.saturating_sub(TASK_PANE_BODY_LEFT),
                rows.saturating_sub(11),
            ),
            Mode::Attach => (
                self.selected_pane()?.key,
                0,
                ATTACH_BODY_TOP,
                cols,
                rows.saturating_sub(5),
            ),
            Mode::ServerConsole(key) => {
                (key.clone(), 0, SETUP_BODY_TOP, cols, rows.saturating_sub(5))
            }
            _ => return None,
        };
        Some(CliViewport {
            key,
            left,
            top,
            width,
            height,
        })
    }

    /// End an existing gesture only in the viewer that received its press. A
    /// release is never queued or redirected to a newly selected/replaced pane.
    fn cancel_cli_mouse_press(&mut self) {
        let Some(press) = self.cli_mouse_press.take() else {
            return;
        };
        let Some(pane) = press.pane.upgrade() else {
            return;
        };
        let mut pane = pane.lock().unwrap();
        let mouse = pane.mouse_state();
        let row = press.row.min(mouse.rows.saturating_sub(1));
        let col = press.col.min(mouse.cols.saturating_sub(1));
        if let Some(bytes) = mouse.encode(press.code, row, col, true) {
            let _ = pane.write_bytes(&bytes);
        }
    }

    fn sync_cli_mouse_press(&mut self, cols: usize, rows: usize) {
        let Some(press) = &self.cli_mouse_press else {
            return;
        };
        let valid = self.focus == Focus::Cli
            && self.pending_task.is_none()
            && !self.buffered_input.contains_key(&press.view.key)
            && self.cli_viewport(cols, rows).as_ref() == Some(&press.view)
            && self.panes.get(&press.view.key).is_some_and(|entry| {
                Weak::ptr_eq(&press.pane, &Arc::downgrade(&entry.pane))
                    && entry.pane.lock().unwrap().size() == (press.view.height, press.view.width)
            });
        if !valid {
            self.cancel_cli_mouse_press();
        }
    }

    /// Route only the child rectangle, never the surrounding chrome. Wheel
    /// input may inspect the selected CLI while navigation retains keyboard
    /// focus. Buttons/drag require CLI focus; a navigation click only focuses.
    fn forward_cli_mouse(&mut self, mouse: MouseEvent, cols: usize, rows: usize) -> bool {
        self.sync_cli_mouse_press(cols, rows);
        // A second press ends the first gesture instead of silently abandoning
        // its release (including a press over surrounding chrome).
        if matches!(mouse.kind, MouseEventKind::Down(_)) {
            self.cancel_cli_mouse_press();
        }
        let Some(view) = self.cli_viewport(cols, rows) else {
            return false;
        };
        if !is_wheel(mouse.kind) && self.focus != Focus::Cli {
            return false;
        }
        let x = mouse.column as usize;
        let y = mouse.row as usize;
        let captured = match mouse.kind {
            MouseEventKind::Drag(button) | MouseEventKind::Up(button) => self
                .cli_mouse_press
                .as_ref()
                .is_some_and(|press| press.view == view && press.button == button),
            _ => false,
        };
        if !view.contains(x, y) && !captured {
            return false;
        }
        if matches!(mouse.kind, MouseEventKind::Drag(_) | MouseEventKind::Up(_)) && !captured {
            return true;
        }
        // Mouse coordinates describe the current frame, not future frames.
        // Do not enqueue gestures during launch, reconnection, or resizing.
        if self.buffered_input.contains_key(&view.key) {
            return true;
        }
        let Some(entry) = self.panes.get(&view.key) else {
            return true;
        };
        let mut pane = entry.pane.lock().unwrap();
        if pane.size() != (view.height, view.width) {
            return true;
        }
        let (button, released) = mouse_packet_kind(mouse);
        // A drag begun inside the CLI ends at its edge even if released over
        // chrome. The child must not be left with a stuck mouse selection.
        let col = x
            .saturating_sub(view.left)
            .min(view.width.saturating_sub(1));
        let row = y
            .saturating_sub(view.top)
            .min(view.height.saturating_sub(1));
        if let Some(bytes) = pane.mouse_state().encode(button, row, col, released) {
            if pane.write_bytes(&bytes).is_ok() {
                match mouse.kind {
                    MouseEventKind::Down(pressed) => {
                        self.cli_mouse_press = Some(CliMousePress {
                            view,
                            pane: Arc::downgrade(&entry.pane),
                            button: pressed,
                            code: button,
                            row,
                            col,
                        });
                    }
                    MouseEventKind::Drag(_) => {
                        if let Some(press) = &mut self.cli_mouse_press {
                            press.row = row;
                            press.col = col;
                        }
                    }
                    _ => {}
                }
            }
        }
        if released {
            self.cli_mouse_press = None;
        }
        true
    }

    fn handle_mouse(&mut self, mouse: MouseEvent, cols: usize, rows: usize) {
        self.sync_cli_mouse_press(cols, rows);
        if self.pending_task.is_some() {
            return;
        }
        if cols < MIN_COLS || rows < MIN_ROWS {
            return;
        }
        if self.forward_cli_mouse(mouse, cols, rows) {
            return;
        }
        if let Mode::Helper { selection, .. } = &mut self.mode {
            let top = rows.saturating_sub(25) / 2;
            let width = form_panel_width(cols);
            let left = cols.saturating_sub(width) / 2;
            if mouse.kind == MouseEventKind::Down(MouseButton::Left)
                && (left..left + width).contains(&(mouse.column as usize))
            {
                let y = mouse.row as usize;
                if (top + 7..top + 15).contains(&y) {
                    *selection = (y - top - 7) / 2;
                }
            } else if matches!(
                mouse.kind,
                MouseEventKind::ScrollUp | MouseEventKind::ScrollDown
            ) {
                if mouse.kind == MouseEventKind::ScrollUp {
                    *selection = selection.saturating_sub(1);
                } else {
                    *selection = (*selection + 1).min(3);
                }
            }
            return;
        }
        if matches!(self.mode, Mode::Settings) {
            if let Some(action) = self.settings.mouse(mouse, &self.registry, cols, rows) {
                self.settings_action(action, cols, rows);
            }
            return;
        }
        if let Mode::ChooseServer { selection, .. } = &mut self.mode {
            if mouse.kind == MouseEventKind::Down(MouseButton::Left)
                && mouse.row >= 4
                && (mouse.row as usize) < rows.saturating_sub(4)
            {
                let visible = rows.saturating_sub(9).div_euclid(3).max(1);
                let start = selection.saturating_sub(visible - 1);
                let index = start + (mouse.row as usize - 4) / 3;
                let count = self
                    .registry
                    .servers()
                    .iter()
                    .filter(|p| !p.archived)
                    .count();
                if index < count {
                    *selection = index;
                    self.handle(
                        KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
                        cols,
                        rows,
                    );
                }
            } else if matches!(
                mouse.kind,
                MouseEventKind::ScrollUp | MouseEventKind::ScrollDown
            ) {
                let code = if mouse.kind == MouseEventKind::ScrollUp {
                    KeyCode::Up
                } else {
                    KeyCode::Down
                };
                self.handle(KeyEvent::new(code, KeyModifiers::NONE), cols, rows);
            }
            return;
        }
        if matches!(self.mode, Mode::Home | Mode::Find(_) | Mode::Task) {
            match mouse.kind {
                MouseEventKind::ScrollUp | MouseEventKind::ScrollDown => {
                    // The rail is the only navigation list in a task view.
                    // Wheel over its header, footer, or CLI never selects an agent.
                    if matches!(self.mode, Mode::Task)
                        && ((mouse.column as usize) >= task::RAIL_W
                            || (mouse.row as usize) < TASK_PANE_BODY_TOP
                            || (mouse.row as usize) >= rows.saturating_sub(5))
                    {
                        return;
                    }
                    let key = if mouse.kind == MouseEventKind::ScrollUp {
                        KeyCode::Up
                    } else {
                        KeyCode::Down
                    };
                    self.handle(KeyEvent::new(key, KeyModifiers::NONE), cols, rows);
                    return;
                }
                _ => {}
            }
        }
        if mouse.kind != MouseEventKind::Down(MouseButton::Left) {
            return;
        }
        let x = mouse.column as usize;
        let y = mouse.row as usize;
        if let Mode::Find(query) = &mut self.mode {
            if y == 3 {
                if x >= cols.saturating_sub(11) {
                    query.clear();
                    self.editor = Editor::default();
                    self.st.home_sel = 0;
                } else {
                    self.editor = Editor {
                        cursor: Some(forms::clicked_position(
                            query,
                            self.editor,
                            cols.saturating_sub(16),
                            x.saturating_sub(3),
                        )),
                        selected: false,
                    };
                }
                return;
            }
        }
        if matches!(self.mode, Mode::Home | Mode::Find(_)) {
            let tasks: Vec<String> = self
                .registry
                .tasks()
                .iter()
                .filter(|t| self.matches_task(t))
                .map(|t| t.task_ref.clone())
                .collect();
            if let Some(index) = home::task_at_row(tasks.len(), self.st.home_sel, cols, rows, y) {
                self.st.home_sel = index;
                if let Some(task_ref) = tasks.get(index) {
                    self.open_task(task_ref.clone(), cols, rows);
                } else {
                    self.open_newtask();
                }
            }
            return;
        }
        if matches!(self.mode, Mode::Task) {
            if x < task::RAIL_W {
                if let Some(view) = self.task_view(cols, rows) {
                    if let Some(index) = task::rail_at_row(&view, &self.st, rows, y) {
                        self.st.rail_sel = index;
                        self.handle(
                            KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
                            cols,
                            rows,
                        );
                    }
                }
            } else if x >= TASK_PANE_BODY_LEFT
                && y >= TASK_PANE_BODY_TOP
                && y < rows.saturating_sub(5)
            {
                self.handle(
                    KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
                    cols,
                    rows,
                );
            }
            return;
        }
        if let Some(&(field, value_row, left, width)) =
            self.form_hits.iter().find(|(_, row, left, width)| {
                (*row == y || row.saturating_sub(1) == y)
                    && x >= *left + 2
                    && x < *left + *width - 2
            })
        {
            let previous = field_identity(&self.mode);
            match &mut self.mode {
                Mode::NewTask(f) => f.focus = field,
                Mode::Manager(f) => f.focus = field,
                Mode::Sub(f) => f.focus = field,
                _ => return,
            }
            if field == 2 && matches!(self.mode, Mode::NewTask(_)) {
                self.open_server_picker();
                return;
            }
            if field == 1 && y == value_row {
                let selector = match &mut self.mode {
                    Mode::Manager(f) => Some((&f.choices, &mut f.hsel)),
                    Mode::Sub(f) => Some((&f.choices, &mut f.hsel)),
                    _ => None,
                };
                if let Some((choices, selected)) = selector {
                    if let Some(choice) = choices.get(*selected) {
                        let right = left
                            + 6
                            + harness_title(&choice.hkey)
                                .chars()
                                .map(forms::cell_width)
                                .sum::<usize>();
                        if (left + 3..left + 5).contains(&x) {
                            cycle_harness(selected, choices.len(), false);
                        } else if (right.saturating_sub(1)..=right + 1).contains(&x) {
                            cycle_harness(selected, choices.len(), true);
                        }
                    }
                }
            }
            let same = previous == field_identity(&self.mode);
            if let Some(value) = active_field(&mut self.mode) {
                if x >= left + width - 11 {
                    value.clear();
                    self.editor = Editor::default();
                } else {
                    let editor = if same {
                        self.editor
                    } else {
                        Editor {
                            cursor: Some(0),
                            selected: false,
                        }
                    };
                    let p = forms::clicked_position(
                        value,
                        editor,
                        width.saturating_sub(16),
                        x.saturating_sub(left + 3),
                    );
                    self.editor = Editor {
                        cursor: Some(p),
                        selected: false,
                    };
                }
            }
            self.status = None;
            return;
        }
        let ex = match &mut self.mode {
            Mode::NewTask(f) => f.explorer.as_mut(),
            Mode::Sub(f) => f.explorer.as_mut(),
            _ => None,
        };
        if let Some(ex) = ex {
            let panel = live_explorer_panel(ex, cols);
            let available = rows.saturating_sub(3);
            let top = (available.saturating_sub(panel.len()) / 2)
                .min(available.saturating_sub(1).saturating_sub(panel.len()));
            let width = form_panel_width(cols);
            let left = cols.saturating_sub(width) / 2;
            if x >= left + 2 && x < left + width - 2 && y >= top + 6 && y < top + 18 {
                let start = ex
                    .sel
                    .saturating_sub(11)
                    .min(ex.entries.len().saturating_sub(12));
                let index = start + y - top - 6;
                if index < ex.entries.len() {
                    ex.sel = index;
                }
            }
        }
    }

    fn handle_inner(&mut self, k: KeyEvent, cols: usize, rows: usize) -> bool {
        if self.pending_harness_action.take().is_some()
            && k.code == KeyCode::Esc
            && matches!(self.mode, Mode::Task)
        {
            self.status = Some("Provider check dismissed".into());
            return true;
        }
        self.status = None;
        let ctrl_c = k.code == KeyCode::Char('c') && k.modifiers.contains(KeyModifiers::CONTROL);
        match &mut self.mode {
            Mode::Helper { selection, back } => {
                match k.code {
                    KeyCode::Char('q') if k.modifiers.is_empty() => return false,
                    KeyCode::Char('G')
                        if !k
                            .modifiers
                            .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT) =>
                    {
                        self.go_home()
                    }
                    KeyCode::Char('?') => {
                        self.mode = Mode::Help(PrevMode::Helper(*selection, *back));
                    }
                    KeyCode::Esc => {
                        self.mode = match *back {
                            HelperBack::Home => Mode::Home,
                            HelperBack::Task => Mode::Task,
                            HelperBack::Settings => Mode::Settings,
                        };
                    }
                    KeyCode::Up | KeyCode::Left | KeyCode::BackTab => {
                        *selection = selection.saturating_sub(1)
                    }
                    KeyCode::Down | KeyCode::Right | KeyCode::Tab => {
                        *selection = (*selection + 1).min(HKEYS.len() - 1)
                    }
                    KeyCode::F(5) => self.check_server("local", true),
                    KeyCode::Char('s' | 'h') if k.modifiers.is_empty() => {
                        match *back {
                            HelperBack::Home => self.settings_back = PrevMode::Home,
                            HelperBack::Task => self.settings_back = PrevMode::Task,
                            HelperBack::Settings => {}
                        }
                        self.settings.select_server("local");
                        if let Some(hkey) = HKEYS.get(*selection) {
                            self.settings.select_harness(hkey);
                        }
                        self.mode = Mode::Settings;
                    }
                    KeyCode::Enter => {
                        let choice = *selection;
                        if let Err(error) = self.start_helper(choice, cols, rows) {
                            self.status = Some(error);
                        }
                    }
                    _ => {}
                }
                true
            }
            Mode::Settings => {
                if let Some(action) = self.settings.handle(k, &self.registry) {
                    self.settings_action(action, cols, rows);
                }
                true
            }
            Mode::ServerConsole(key) => {
                let key = key.clone();
                match route(Focus::Cli, k) {
                    Route::Boundary => {
                        self.mode = Mode::Settings;
                        self.focus = Focus::Nav;
                        if let Some(server) = self
                            .registry
                            .servers()
                            .iter()
                            .find(|s| s.setup_runtimes.values().any(|r| r.session == key))
                            .map(|s| s.server_id.clone())
                        {
                            self.check_server(&server, true);
                        }
                    }
                    Route::Forward(bytes) => self.forward_cli_bytes(&key, &bytes),
                    _ => {}
                }
                true
            }
            Mode::ChooseServer { selection, .. } => {
                let count = self
                    .registry
                    .servers()
                    .iter()
                    .filter(|p| !p.archived)
                    .count();
                match k.code {
                    KeyCode::Up => *selection = selection.saturating_sub(1),
                    KeyCode::Down => *selection = (*selection + 1).min(count.saturating_sub(1)),
                    KeyCode::Enter | KeyCode::Esc => {
                        let selected = if k.code == KeyCode::Enter {
                            self.registry
                                .servers()
                                .iter()
                                .filter(|p| !p.archived)
                                .nth(*selection)
                                .cloned()
                        } else {
                            None
                        };
                        let Mode::ChooseServer { mut form, .. } =
                            std::mem::replace(&mut self.mode, Mode::Home)
                        else {
                            unreachable!()
                        };
                        if let Some(server) = selected {
                            if form.server_id != server.server_id {
                                form.repo = server
                                    .repository_root
                                    .as_ref()
                                    .map(|p| p.display().to_string())
                                    .unwrap_or_default();
                            }
                            form.server_id = server.server_id;
                            form.focus = 1;
                        }
                        self.mode = Mode::NewTask(form);
                    }
                    _ => {}
                }
                true
            }
            Mode::Boot => {
                if ctrl_c || k.code == KeyCode::Char('q') {
                    return false;
                }
                self.mode = Mode::Home;
                self.focus = Focus::Nav;
                true
            }
            Mode::Home | Mode::Find(_) => {
                if ctrl_c {
                    return false;
                }
                let tasks: Vec<String> = self
                    .registry
                    .tasks()
                    .iter()
                    .filter(|t| self.matches_task(t))
                    .map(|t| t.task_ref.clone())
                    .collect();
                let n_tasks = tasks.len();
                match k.code {
                    KeyCode::Esc => {
                        self.mode = Mode::Home;
                        self.st.home_sel = 0;
                    }
                    KeyCode::Char('/') => {
                        self.mode = Mode::Find(String::new());
                        self.st.home_sel = 0;
                    }
                    KeyCode::Char('q') => return false,
                    KeyCode::Up => self.st.home_sel = self.st.home_sel.saturating_sub(1),
                    KeyCode::Down => self.st.home_sel = (self.st.home_sel + 1).min(n_tasks),
                    KeyCode::Char('s' | 'h') => self.open_settings(),
                    KeyCode::Char('?') => self.mode = Mode::Help(PrevMode::Home),
                    KeyCode::Char('n') => self.open_newtask(),
                    KeyCode::Enter => {
                        if self.st.home_sel >= n_tasks {
                            self.open_newtask();
                        } else {
                            self.open_task(tasks[self.st.home_sel].clone(), cols, rows);
                        }
                    }
                    _ => {}
                }
                true
            }
            Mode::Task => {
                if ctrl_c {
                    return false;
                }
                let rails = self
                    .sel_task
                    .as_ref()
                    .map(|t| self.rail_rows(t))
                    .unwrap_or_default();
                match k.code {
                    KeyCode::Char('q') => return false,
                    KeyCode::Esc => self.go_home(),
                    KeyCode::Enter => {
                        if let Some(selected) = self.selected_pane() {
                            if let Err(error) = self.focus_session(&selected.key, cols, rows, false)
                            {
                                self.status = Some(format!("could not open CLI: {error}"));
                            }
                        } else {
                            self.status = Some("add a Manager first —  m".into());
                        }
                    }
                    KeyCode::Char('t') => {
                        if let Some(task_ref) = self.sel_task.clone() {
                            let key = terminal_ref(&task_ref);
                            self.st.rail_sel = rails.len().saturating_sub(1);
                            if let Err(error) = self.focus_session(&key, cols, rows, false) {
                                self.status = Some(format!("could not open Terminal: {error}"));
                            }
                        }
                    }
                    KeyCode::Up => self.st.rail_sel = self.st.rail_sel.saturating_sub(1),
                    KeyCode::Down => {
                        self.st.rail_sel = (self.st.rail_sel + 1).min(rails.len().saturating_sub(1))
                    }
                    KeyCode::Char('c') => {
                        if self.selected_pane().is_some_and(|p| p.hkey == "terminal") {
                            self.status =
                                Some("Terminal is a native shell; it has no ACP view".into());
                            return true;
                        }
                        if self.selected_pane().is_some_and(|p| {
                            self.panes.contains_key(&p.key)
                                || self.pending_launches.contains_key(&p.key)
                        }) {
                            self.status = Some("Native CLI is active; an ACP visualization is not available for this session".into());
                            return true;
                        }
                        self.st.view = if self.st.view == View::Native {
                            View::Acp
                        } else {
                            View::Native
                        }
                    }
                    KeyCode::Char('G')
                        if !k.modifiers.contains(KeyModifiers::CONTROL)
                            && !k.modifiers.contains(KeyModifiers::ALT) =>
                    {
                        self.go_home()
                    }
                    KeyCode::Char('s' | 'h') => self.open_settings(),
                    KeyCode::Char('?') => self.mode = Mode::Help(PrevMode::Task),
                    KeyCode::Char('m') => {
                        if self.wait_for_remote_harnesses('m') {
                            return true;
                        }
                        let choices = self.harness_choices();
                        let hsel = choices
                            .iter()
                            .position(HarnessAvailability::is_ready)
                            .unwrap_or(0);
                        self.mode = Mode::Manager(ManagerForm {
                            name: "Lead".into(),
                            choices,
                            hsel,
                            focus: 0,
                        });
                    }
                    KeyCode::Char('a') => match self.selected_session() {
                        Some((sref, sess)) if sess.role == Role::Manager => {
                            if self.wait_for_remote_harnesses('a') {
                                return true;
                            }
                            let choices = self.harness_choices();
                            let hsel = choices
                                .iter()
                                .position(HarnessAvailability::is_ready)
                                .unwrap_or(0);
                            {
                                let worktree = self
                                    .registry
                                    .task(&sess.task_ref)
                                    .filter(|t| t.connection == ConnectionTarget::Local)
                                    .and_then(|t| suggest_worktree(&t.repo));
                                self.mode = Mode::Sub(SubForm {
                                    parent: sref,
                                    role: "".into(),
                                    choices,
                                    hsel,
                                    ws_mode: usize::from(worktree.is_some()),
                                    branch: worktree
                                        .as_ref()
                                        .map(|(branch, _)| branch.clone())
                                        .unwrap_or_else(|| "HEAD".into()),
                                    wname: worktree
                                        .map(|(_, name)| name)
                                        .unwrap_or_else(|| "agent-1".into()),
                                    other: "".into(),
                                    focus: 0,
                                    explorer: None,
                                });
                            }
                        }
                        Some(_) => {
                            self.status =
                                Some("select a Manager in the rail to add a subordinate".into())
                        }
                        None => self.status = Some("add a Manager first —  m".into()),
                    },
                    KeyCode::Char('d') => {
                        if let Some((sref, session)) = self.selected_session() {
                            let item = Item::Session(sref.clone());
                            let blast = self.registry.blast(&item).unwrap_or_default();
                            let scope = if session.role == Role::Manager {
                                "Manager"
                            } else {
                                "Subordinate"
                            };
                            let mut blast_lines = vec![format!(
                                "{} session{} will be closed",
                                blast.len(),
                                if blast.len() == 1 { "" } else { "s" }
                            )];
                            blast_lines.push(format!("{scope} record removed from TaskAI"));
                            self.mode = Mode::Destroy(DestroyForm {
                                item,
                                title: format!("Destroy {scope} — {}", session.name),
                                blast_lines,
                                expect: sref,
                                typed: String::new(),
                                back_home: false,
                            });
                        } else {
                            self.status = Some("Terminal is not an agent. Type exit in the shell to close it; D destroys the task.".into());
                        }
                    }
                    KeyCode::Char('D') => {
                        if let Some(task_ref) = self.sel_task.clone() {
                            if let Some(t) = self.registry.task(&task_ref) {
                                let title = format!("Destroy task — {}", t.name);
                                let expect = task_ref.clone();
                                let blast = self
                                    .registry
                                    .blast(&Item::Task(task_ref.clone()))
                                    .unwrap_or_default();
                                let mut blast_lines = vec![format!(
                                    "{} session{} will be closed",
                                    blast.len(),
                                    if blast.len() == 1 { "" } else { "s" }
                                )];
                                blast_lines.push("Task record removed from TaskAI".into());
                                if t.terminal.is_some() {
                                    blast_lines.push("Utility Terminal will also be closed".into());
                                }
                                self.mode = Mode::Destroy(DestroyForm {
                                    item: Item::Task(task_ref),
                                    title,
                                    blast_lines,
                                    expect,
                                    typed: String::new(),
                                    back_home: true,
                                });
                            }
                        }
                    }
                    KeyCode::Char('f' | 'F')
                        if k.modifiers.contains(KeyModifiers::CONTROL)
                            && !k.modifiers.contains(KeyModifiers::ALT) =>
                    {
                        if let Some(selected) = self.selected_pane() {
                            match self.focus_session(&selected.key, cols, rows, true) {
                                Ok(()) => {}
                                Err(e) => self.status = Some(format!("launch failed: {e}")),
                            }
                        } else {
                            self.status = Some("add a Manager first —  m".into());
                        }
                    }
                    _ => {}
                }
                true
            }
            Mode::TaskCli => {
                match route(self.focus, k) {
                    Route::Boundary => {
                        self.mode = Mode::Task;
                        self.focus = Focus::Nav;
                    }
                    Route::Layout => self.mode = Mode::Attach,
                    Route::Forward(bytes) => {
                        if let Some(selected) = self.selected_pane() {
                            self.forward_cli_bytes(&selected.key, &bytes);
                        }
                    }
                    Route::Nav(_) => {}
                }
                true
            }
            Mode::Attach => {
                match route(self.focus, k) {
                    Route::Boundary => {
                        self.mode = Mode::Task;
                        self.focus = Focus::Nav;
                    }
                    Route::Layout => self.mode = Mode::TaskCli,
                    Route::Forward(bytes) => {
                        if let Some(selected) = self.selected_pane() {
                            self.forward_cli_bytes(&selected.key, &bytes);
                        }
                    }
                    Route::Nav(_) => {}
                }
                true
            }
            Mode::Help(prev) => {
                let prev = *prev;
                if k.code == KeyCode::Char('q') && k.modifiers.is_empty() {
                    return false;
                }
                if k.code == KeyCode::Char('G')
                    && !k
                        .modifiers
                        .intersects(KeyModifiers::CONTROL | KeyModifiers::ALT)
                {
                    self.go_home();
                    return true;
                }
                if matches!(k.code, KeyCode::Esc | KeyCode::Enter | KeyCode::Char('?')) {
                    self.mode = match prev {
                        PrevMode::Home => Mode::Home,
                        PrevMode::Task => Mode::Task,
                        PrevMode::Settings => Mode::Settings,
                        PrevMode::Helper(selection, back) => Mode::Helper { selection, back },
                    };
                }
                true
            }
            Mode::NewTask(f) => {
                if let Some(ex) = &mut f.explorer {
                    if k.code == KeyCode::Esc {
                        f.explorer = None;
                        return true;
                    }
                    if ex.pending.is_some() || ex.error.is_some() {
                        return true;
                    }
                    match k.code {
                        KeyCode::Up => ex.sel = ex.sel.saturating_sub(1),
                        KeyCode::Down => {
                            ex.sel = (ex.sel + 1).min(ex.entries.len().saturating_sub(1))
                        }
                        KeyCode::Enter | KeyCode::Right => {
                            let (name, is_dir) = ex.entries[ex.sel].clone();
                            if is_dir {
                                if name == ".." {
                                    if let Some(p) = ex.cwd.parent() {
                                        ex.cwd = p.to_path_buf();
                                    }
                                } else {
                                    ex.cwd = ex.cwd.join(name);
                                }
                                ex.refresh();
                            }
                        }
                        KeyCode::Left => {
                            if let Some(p) = ex.cwd.parent() {
                                ex.cwd = p.to_path_buf();
                                ex.refresh();
                            }
                        }
                        KeyCode::Char(' ') => {
                            let chosen = match ex.entries.get(ex.sel) {
                                Some((name, true)) if name != ".." => ex.cwd.join(name),
                                _ => ex.cwd.clone(),
                            };
                            f.repo = chosen.display().to_string();
                            f.explorer = None;
                            if f.name.trim().is_empty() {
                                f.name = chosen
                                    .file_name()
                                    .unwrap_or_default()
                                    .to_string_lossy()
                                    .replace('-', " ");
                            }
                            f.focus = 0;
                        }
                        KeyCode::Esc => f.explorer = None,
                        _ => {}
                    }
                    return true;
                }
                if k.code == KeyCode::F(2) {
                    let connection =
                        match self.registry.server(&f.server_id).filter(|p| !p.archived) {
                            Some(server) => server.target.clone(),
                            None => {
                                self.status = Some("Select an active server first".into());
                                f.focus = 2;
                                return true;
                            }
                        };
                    let start = if f.repo.trim().is_empty() {
                        if connection == ConnectionTarget::Local {
                            std::env::current_dir().unwrap_or_else(|_| "/".into())
                        } else {
                            PathBuf::from("~")
                        }
                    } else {
                        PathBuf::from(f.repo.trim())
                    };
                    f.explorer = Some(Explorer::on(connection, start));
                    return true;
                }
                match k.code {
                    KeyCode::Esc => self.mode = Mode::Home,
                    KeyCode::Tab | KeyCode::Down => {
                        f.focus = match f.focus {
                            2 => 1,
                            1 => 0,
                            _ => 2,
                        }
                    }
                    KeyCode::BackTab | KeyCode::Up => {
                        f.focus = match f.focus {
                            2 => 0,
                            0 => 1,
                            _ => 2,
                        }
                    }
                    KeyCode::Left | KeyCode::Right if f.focus == 2 => {
                        let servers: Vec<_> = self
                            .registry
                            .servers()
                            .iter()
                            .filter(|p| !p.archived)
                            .collect();
                        if !servers.is_empty() {
                            let mut index = servers
                                .iter()
                                .position(|p| p.server_id == f.server_id)
                                .unwrap_or(0);
                            cycle_harness(&mut index, servers.len(), k.code == KeyCode::Right);
                            if f.server_id != servers[index].server_id {
                                f.server_id = servers[index].server_id.clone();
                                f.repo = servers[index]
                                    .repository_root
                                    .as_ref()
                                    .map(|p| p.display().to_string())
                                    .unwrap_or_default();
                            }
                        }
                    }
                    KeyCode::Enter => {
                        if f.focus != 0 {
                            f.focus = if f.focus == 2 { 1 } else { 0 };
                            if f.focus == 0 && f.name.trim().is_empty() {
                                f.name = Path::new(f.repo.trim())
                                    .file_name()
                                    .unwrap_or_default()
                                    .to_string_lossy()
                                    .replace('-', " ");
                            }
                            return true;
                        }
                        let connection =
                            match self.registry.server(&f.server_id).filter(|p| !p.archived) {
                                Some(server) => server.target.clone(),
                                None => {
                                    f.focus = 2;
                                    self.status = Some("Select an active server first".into());
                                    return true;
                                }
                            };
                        if f.repo.trim().is_empty() {
                            f.focus = 1;
                            self.status = Some(
                                "Choose a repository first: paste its path or press F2 to browse."
                                    .into(),
                            );
                            return true;
                        }
                        let repo = if connection == ConnectionTarget::Local
                            && (f.repo.trim() == "~" || f.repo.trim().starts_with("~/"))
                        {
                            PathBuf::from(std::env::var("HOME").unwrap_or_default()).join(
                                f.repo
                                    .trim()
                                    .strip_prefix('~')
                                    .unwrap()
                                    .trim_start_matches('/'),
                            )
                        } else {
                            PathBuf::from(f.repo.trim())
                        };
                        if f.focus == 1 && connection == ConnectionTarget::Local && !repo.is_dir() {
                            let start = if repo.is_dir() {
                                repo
                            } else {
                                std::env::current_dir().unwrap_or_else(|_| "/".into())
                            };
                            f.explorer = Some(Explorer::open(&start));
                        } else if f.name.trim().is_empty() {
                            f.focus = 0;
                            self.status = Some("name the task first".into());
                        } else if connection == ConnectionTarget::Local && !repo.is_dir() {
                            f.focus = 1;
                            self.status = Some("repository path does not exist".into());
                        } else if connection != ConnectionTarget::Local
                            && !repo.is_absolute()
                            && f.repo != "~"
                            && !f.repo.starts_with("~/")
                        {
                            f.focus = 1;
                            self.status = Some(
                                "remote repository must be an absolute path, ~, or start with ~/"
                                    .into(),
                            );
                        } else {
                            let name = f.name.trim().to_string();
                            let repo = if connection == ConnectionTarget::Local {
                                match repo.canonicalize() {
                                    Ok(repo) => repo,
                                    Err(error) => {
                                        f.focus = 1;
                                        self.status = Some(format!(
                                            "could not resolve repository path: {error}"
                                        ));
                                        return true;
                                    }
                                }
                            } else {
                                let (tx, rx) = channel();
                                self.pending_task = Some((f.server_id.clone(), name, rx));
                                self.status = Some(format!(
                                    "Checking repository on {connection}… Esc cancels"
                                ));
                                std::thread::spawn(move || {
                                    let check = || {
                                        tmux::probe(&connection).map_err(|e| e.to_string())?;
                                        tmux::resolve_repository(&connection, &repo)
                                            .map_err(|e| e.to_string())?
                                            .ok_or_else(|| {
                                                format!("Repository does not exist on {connection}")
                                            })
                                    };
                                    let _ = tx.send(check());
                                });
                                return true;
                            };
                            match self.registry.create_task_on_server(
                                &name,
                                &repo,
                                &f.server_id,
                                Source::HumanUi,
                            ) {
                                Ok(t) => {
                                    self.sel_task = Some(t);
                                    self.st.rail_sel = 0;
                                    self.mode = Mode::Task;
                                }
                                Err(e) => self.status = Some(format!("create failed: {e}")),
                            }
                        }
                    }
                    _ => {}
                }
                true
            }
            Mode::Manager(f) => {
                match k.code {
                    KeyCode::Esc => self.mode = Mode::Task,
                    KeyCode::Tab | KeyCode::BackTab | KeyCode::Down | KeyCode::Up => {
                        f.focus = 1 - f.focus
                    }
                    KeyCode::Left if f.focus == 1 => {
                        cycle_harness(&mut f.hsel, f.choices.len(), false)
                    }
                    KeyCode::Right if f.focus == 1 => {
                        cycle_harness(&mut f.hsel, f.choices.len(), true)
                    }
                    KeyCode::Enter => {
                        let form = ManagerForm {
                            name: f.name.clone(),
                            choices: f.choices.clone(),
                            hsel: f.hsel,
                            focus: f.focus,
                        };
                        match self.create_manager(&form) {
                            Ok(sref) => {
                                self.mode = Mode::Task;
                                if let Some(t) = self.sel_task.clone() {
                                    let rails = self.rail_rows(&t);
                                    if let Some(i) = rails.iter().position(|r| r.key == sref) {
                                        self.st.rail_sel = i;
                                    }
                                }
                                match self.focus_session(&sref, cols, rows, false) {
                                    Ok(()) => {
                                        self.status = Some(format!(
                                            "Opening {sref}… type-ahead is buffered · Ctrl+G navigation"
                                        ))
                                    }
                                    Err(e) => {
                                        self.status =
                                            Some(format!("registered {sref}; launch failed: {e}"))
                                    }
                                }
                            }
                            Err(e) => self.status = Some(format!("create failed: {e}")),
                        }
                    }
                    _ => {}
                }
                true
            }
            Mode::Sub(f) => {
                let remote_task = self
                    .sel_task
                    .as_ref()
                    .and_then(|r| self.registry.task(r))
                    .is_some_and(|t| t.connection != ConnectionTarget::Local);
                if let Some(ex) = &mut f.explorer {
                    if k.code == KeyCode::Esc {
                        f.explorer = None;
                        return true;
                    }
                    if ex.pending.is_some() || ex.error.is_some() {
                        return true;
                    }
                    match k.code {
                        KeyCode::Up => ex.sel = ex.sel.saturating_sub(1),
                        KeyCode::Down => {
                            ex.sel = (ex.sel + 1).min(ex.entries.len().saturating_sub(1))
                        }
                        KeyCode::Enter | KeyCode::Right => {
                            let (name, is_dir) = ex.entries[ex.sel].clone();
                            if is_dir {
                                if name == ".." {
                                    if let Some(p) = ex.cwd.parent() {
                                        ex.cwd = p.to_path_buf();
                                    }
                                } else {
                                    ex.cwd = ex.cwd.join(name);
                                }
                                ex.refresh();
                            }
                        }
                        KeyCode::Left => {
                            if let Some(p) = ex.cwd.parent() {
                                ex.cwd = p.to_path_buf();
                                ex.refresh();
                            }
                        }
                        KeyCode::Char(' ') => {
                            let chosen = match ex.entries.get(ex.sel) {
                                Some((name, true)) if name != ".." => ex.cwd.join(name),
                                _ => ex.cwd.clone(),
                            };
                            f.other = chosen.display().to_string();
                            f.explorer = None;
                        }
                        KeyCode::Esc => f.explorer = None,
                        _ => {}
                    }
                    return true;
                }
                let fields: &[usize] = if remote_task {
                    &[0, 1]
                } else if f.ws_mode == 1 {
                    &[0, 1, 2, 3, 4]
                } else if f.ws_mode == 2 {
                    &[0, 1, 2, 5]
                } else {
                    &[0, 1, 2]
                };
                let position = fields
                    .iter()
                    .position(|field| *field == f.focus)
                    .unwrap_or(0);
                match k.code {
                    KeyCode::Esc => self.mode = Mode::Task,
                    KeyCode::F(2) if !remote_task && f.ws_mode == 2 => {
                        let start = if f.other.trim().is_empty() {
                            std::env::current_dir().unwrap_or_else(|_| "/".into())
                        } else {
                            PathBuf::from(f.other.trim())
                        };
                        f.explorer = Some(Explorer::open(&start));
                    }
                    KeyCode::Tab | KeyCode::Down => f.focus = fields[(position + 1) % fields.len()],
                    KeyCode::BackTab | KeyCode::Up => {
                        f.focus = fields[(position + fields.len() - 1) % fields.len()]
                    }
                    KeyCode::Left => match f.focus {
                        1 => cycle_harness(&mut f.hsel, f.choices.len(), false),
                        2 if !remote_task => f.ws_mode = f.ws_mode.saturating_sub(1),
                        _ => {}
                    },
                    KeyCode::Right => match f.focus {
                        1 => cycle_harness(&mut f.hsel, f.choices.len(), true),
                        2 if !remote_task => {
                            f.ws_mode = (f.ws_mode + 1).min(2);
                            if f.ws_mode == 2 && f.other.is_empty() {
                                let start = std::env::current_dir().unwrap_or_else(|_| "/".into());
                                f.explorer = Some(Explorer::open(&start));
                            }
                        }
                        _ => {}
                    },
                    KeyCode::Enter => {
                        let form = SubForm {
                            parent: f.parent.clone(),
                            role: f.role.clone(),
                            choices: f.choices.clone(),
                            hsel: f.hsel,
                            ws_mode: f.ws_mode,
                            branch: f.branch.clone(),
                            wname: f.wname.clone(),
                            other: f.other.clone(),
                            focus: f.focus,
                            explorer: None,
                        };
                        match self.create_sub(&form) {
                            Ok(sref) => {
                                self.mode = Mode::Task;
                                if let Some(t) = self.sel_task.clone() {
                                    let rails = self.rail_rows(&t);
                                    if let Some(i) = rails.iter().position(|r| r.key == sref) {
                                        self.st.rail_sel = i;
                                    }
                                }
                                match self.focus_session(&sref, cols, rows, false) {
                                    Ok(()) => {
                                        self.status = Some(format!(
                                            "Opening {sref}… type-ahead is buffered · Ctrl+G navigation"
                                        ))
                                    }
                                    Err(e) => {
                                        self.status =
                                            Some(format!("registered {sref}; launch failed: {e}"))
                                    }
                                }
                            }
                            Err(e) => self.status = Some(format!("create failed: {e}")),
                        }
                    }
                    _ => {}
                }
                true
            }
            Mode::Destroy(f) => {
                match k.code {
                    KeyCode::Esc => {
                        self.mode = Mode::Task;
                    }
                    KeyCode::Backspace => {
                        f.typed.pop();
                    }
                    KeyCode::Char(ch) if accepts_text_input(&k) => f.typed.push(ch),
                    KeyCode::Enter => {
                        if f.typed == f.expect {
                            let item = f.item.clone();
                            match self.destroy_item(&item) {
                                Ok(removed) => {
                                    if matches!(item, Item::Task(_)) {
                                        self.sel_task = None;
                                        self.mode = Mode::Home;
                                        self.st.home_sel = 0;
                                    } else {
                                        self.mode = Mode::Task;
                                        self.st.rail_sel = 0;
                                    }
                                    self.status = Some(format!(
                                        "destroyed — {} session{} closed; repository and worktrees preserved",
                                        removed.len(),
                                        if removed.len() == 1 { "" } else { "s" }
                                    ));
                                }
                                Err(e) => self.status = Some(format!("destroy failed: {e}")),
                            }
                        } else {
                            self.status = Some("confirmation text does not match".into());
                        }
                    }
                    _ => {}
                }
                true
            }
        }
    }

    fn active_servers(&self) -> Vec<ServerProfile> {
        self.registry
            .servers()
            .iter()
            .filter(|p| !p.archived)
            .cloned()
            .collect()
    }

    fn open_server_picker(&mut self) {
        let old = std::mem::replace(&mut self.mode, Mode::Home);
        if let Mode::NewTask(form) = old {
            let selection = self
                .active_servers()
                .iter()
                .position(|p| p.server_id == form.server_id)
                .unwrap_or(0);
            self.mode = Mode::ChooseServer { form, selection };
        } else {
            self.mode = old;
        }
    }

    fn selected_profile(&self) -> Option<ServerProfile> {
        let task = self.registry.task(self.sel_task.as_deref()?)?;
        task.server_profile_id
            .as_deref()
            .and_then(|id| self.registry.server(id))
            .cloned()
    }

    fn helper_choices(&self) -> Vec<HarnessAvailability> {
        let local = self.registry.server("local");
        Self::helper_choices_from_result(local.and_then(|p| self.remote_probes.get_profile(p)))
    }

    fn helper_choices_from_result(
        result: Option<&crate::remote_probes::ProbeResult>,
    ) -> Vec<HarnessAvailability> {
        let reason = match result {
            Some(Ok(choices)) => return choices.clone(),
            Some(Err(error)) => format!(
                "Couldn't verify local harnesses: {error}. F5 retries; s opens local Settings"
            ),
            None => "Checking local harnesses… F5 retries; s opens local Settings".into(),
        };
        HKEYS
            .iter()
            .map(|key| HarnessAvailability {
                hkey: (*key).into(),
                version: None,
                unavailable: Some(reason.clone()),
                auth: crate::remote_probes::AuthState::Unknown,
            })
            .collect()
    }

    fn open_helper(&mut self, cols: usize, rows: usize) {
        let back = match self.mode {
            Mode::Settings => HelperBack::Settings,
            Mode::Task => HelperBack::Task,
            _ => HelperBack::Home,
        };
        if let Some(profile) = self.registry.server("local") {
            self.remote_probes.request_profile(profile);
        }
        let existing = self
            .registry
            .helper()
            .manager_ref
            .as_deref()
            .and_then(|id| self.registry.session(id))
            .map(|s| s.harness.clone());
        let choices = self.helper_choices();
        let preferred = existing
            .as_deref()
            .or(self.registry.helper().preferred_harness.as_deref());
        let selection = preferred
            .and_then(|key| HKEYS.iter().position(|k| *k == key))
            .or_else(|| {
                choices.iter().position(|c| {
                    c.is_ready() && c.auth == crate::remote_probes::AuthState::SignedIn
                })
            })
            .or_else(|| choices.iter().position(HarnessAvailability::is_ready))
            .unwrap_or(0);
        self.mode = Mode::Helper { selection, back };
        self.focus = Focus::Nav;
        self.status = None;
        if existing.is_some() {
            if let Err(error) = self.start_helper(selection, cols, rows) {
                self.status = Some(error);
            }
        }
    }

    fn helper_frame(&self, selection: usize, cols: usize, rows: usize) -> Vec<String> {
        let width = form_panel_width(cols);
        let left = cols.saturating_sub(width) / 2;
        let top = rows.saturating_sub(25) / 2;
        let mut frame = vec![vrow(cols); rows];
        let mut put = |offset: usize, text: String, selected: bool| {
            let color = if selected {
                taskai_tui::color::GLASS_HI
            } else {
                taskai_tui::color::GLASS
            };
            let content = row(
                &[s(
                    text,
                    Some(if selected { ION } else { TEXT }),
                    Some(color),
                )],
                width,
                color,
                3,
            );
            frame[top + offset] = format!(
                "{}{}{}",
                row(&[], left, VOID, 0),
                content,
                row(&[], cols - left - width, VOID, 0)
            );
        };
        for i in 0..22 {
            put(i, String::new(), false);
        }
        put(1, "HELP ME SET UP · taskai-helper".into(), false);
        put(
            3,
            "One local Manager to guide servers, login, and building your team.".into(),
            false,
        );
        put(
            4,
            "Runs locally; can guide any saved server. Resumes in tmux, no extra service.".into(),
            false,
        );
        put(
            5,
            "Choose its local harness; your existing project Managers stay unchanged.".into(),
            false,
        );
        let choices = self.helper_choices();
        let checking = self
            .registry
            .server("local")
            .is_some_and(|profile| self.remote_probes.get_profile(profile).is_none());
        for (index, choice) in choices.iter().enumerate() {
            put(
                7 + index * 2,
                format!(
                    "{}  {:<14} {}",
                    if index == selection { "❯" } else { " " },
                    harness_title(&choice.hkey),
                    if checking {
                        "Checking"
                    } else {
                        choice.status_label()
                    }
                ),
                index == selection,
            );
        }
        let detail = choices
            .get(selection)
            .map(|c| {
                c.unavailable.clone().unwrap_or_else(|| {
                    format!(
                        "{} · Model access is checked by the native CLI",
                        c.authentication_status()
                    )
                })
            })
            .unwrap_or_default();
        put(16, detail, false);
        put(
            18,
            "No usable local harness? Press s for local Settings and sign in there.".into(),
            false,
        );
        put(
            19,
            "Enter opens the helper CLI; prompts use your normal provider account.".into(),
            false,
        );
        put(
            20,
            "Type your question after the CLI opens. No prompt is sent automatically.".into(),
            false,
        );
        modal_keybar(
            frame,
            cols,
            &[
                ("↑/↓", "Harness"),
                ("Enter", "Open helper"),
                ("s", "Settings"),
                ("F5", "Check"),
                ("?", "Help"),
                ("G", "Home"),
                ("q", "Leave"),
                ("Esc", "Back"),
            ],
        )
    }

    fn start_helper(&mut self, selection: usize, cols: usize, rows: usize) -> Result<(), String> {
        let hkey = *HKEYS.get(selection).ok_or("Choose a local harness")?;
        let has_manager = self
            .registry
            .helper()
            .manager_ref
            .as_deref()
            .is_some_and(|id| self.registry.session(id).is_some());
        if !has_manager {
            let choices = self.helper_choices();
            require_harness(&choices, selection)?;
            if choices[selection].auth == crate::remote_probes::AuthState::SignedOut {
                return Err("Sign in to this local CLI first: s opens local Settings, c opens its native CLI; return and F5 rechecks.".into());
            }
        }
        let workspace = helper::prepare_workspace(&self.home_dir)?;
        let (task, manager, created) = self
            .registry
            .ensure_helper(hkey, &workspace, Source::HumanUi)
            .map_err(|e| e.to_string())?;
        self.sel_task = Some(task.clone());
        self.st.rail_sel = self
            .rail_rows(&task)
            .iter()
            .position(|s| s.key == manager)
            .unwrap_or(0);
        self.focus_session(&manager, cols, rows, false)?;
        if created {
            self.status = Some("Helper opened with its setup guide. Complete any native login/trust prompt, then ask: Help me set up TaskAI.".into());
        }
        Ok(())
    }

    fn validate_helper_action(&self, action: &str, argument: Option<&str>) -> Result<(), String> {
        match action {
            "open-settings" => if argument.is_none_or(|id| self.registry.server(id).is_some_and(|s| !s.archived)) { Ok(()) } else { Err("Select an active server ID from taskai helper status".into()) },
            "add-server" | "new-task" if argument.is_none() => Ok(()),
            "add-manager" if argument.is_some_and(|id| self.registry.task(id).is_some()) => Ok(()),
            "add-subordinate" if argument.is_some_and(|id| self.registry.session(id).is_some_and(|s| s.role == Role::Manager)) => Ok(()),
            _ => Err("Invalid helper request. Use open-settings [server-id], add-server, new-task, add-manager <task-ref>, or add-subordinate <manager-ref>.".into()),
        }
    }

    fn poll_helper_requests(&mut self) {
        while let Ok(request) = self.helper_requests.try_recv() {
            let result =
                if self.registry.helper().manager_ref.as_deref() != Some(request.source.as_str()) {
                    Err("Only the dedicated taskai-helper Manager can request setup forms".into())
                } else if self.helper_action.is_some() {
                    Err("A helper request is already awaiting user review".into())
                } else {
                    self.validate_helper_action(&request.action, request.argument.as_deref())
                };
            let accepted = result.is_ok();
            if request.reply.send(result).is_ok() && accepted {
                self.helper_action = Some(HelperAction {
                    action: request.action,
                    argument: request.argument,
                });
            }
        }
    }

    fn review_helper_action(&mut self, cols: usize, rows: usize) {
        let Some(action) = self.helper_action.take() else {
            return;
        };
        if let Err(error) = self.validate_helper_action(&action.action, action.argument.as_deref())
        {
            self.status = Some(error);
            return;
        }
        self.status = None;
        match action.action.as_str() {
            "open-settings" | "add-server" => {
                self.open_settings();
                if let Some(id) = action.argument {
                    self.settings.select_server(&id);
                }
                if action.action == "add-server" {
                    let _ = self.settings.handle(
                        KeyEvent::new(KeyCode::Char('n'), KeyModifiers::NONE),
                        &self.registry,
                    );
                }
            }
            "new-task" => self.open_newtask(),
            "add-manager" => {
                self.sel_task = action.argument;
                self.st.rail_sel = 0;
                self.mode = Mode::Task;
                self.handle(
                    KeyEvent::new(KeyCode::Char('m'), KeyModifiers::NONE),
                    cols,
                    rows,
                );
            }
            "add-subordinate" => {
                let manager = action.argument.unwrap();
                let task = self.registry.session(&manager).unwrap().task_ref.clone();
                self.sel_task = Some(task.clone());
                self.st.rail_sel = self
                    .rail_rows(&task)
                    .iter()
                    .position(|s| s.key == manager)
                    .unwrap_or(0);
                self.mode = Mode::Task;
                self.handle(
                    KeyEvent::new(KeyCode::Char('a'), KeyModifiers::NONE),
                    cols,
                    rows,
                );
            }
            _ => {}
        }
        self.editor = Editor {
            cursor: None,
            selected: active_field(&mut self.mode).is_some_and(|s| !s.is_empty()),
        };
    }

    fn open_settings(&mut self) {
        self.settings_back = if matches!(self.mode, Mode::Task) {
            PrevMode::Task
        } else {
            PrevMode::Home
        };
        if let Some(profile) = self.selected_profile() {
            self.settings.select_server(&profile.server_id);
        }
        self.mode = Mode::Settings;
        self.focus = Focus::Nav;
        self.status = None;
    }

    fn settings_check(&self) -> Option<ServerCheck> {
        let id = self.settings.selected_server_id(&self.registry)?;
        let profile = self.registry.server(&id)?;
        let result = self.remote_probes.get_profile(profile);
        Some(ServerCheck {
            choices: result
                .and_then(|r| r.as_ref().ok())
                .cloned()
                .unwrap_or_default(),
            checking: result.is_none() || self.server_check_pending.contains_key(&id),
            error: self
                .server_check_errors
                .get(&id)
                .cloned()
                .flatten()
                .or_else(|| result.and_then(|r| r.as_ref().err()).cloned()),
        })
    }

    fn check_server(&mut self, id: &str, force: bool) {
        let Some(profile) = self.registry.server(id).cloned() else {
            return;
        };
        if force {
            self.remote_probes.refresh_profile(&profile);
        } else {
            self.remote_probes.request_profile(&profile);
        }
        if !self.server_check_pending.contains_key(id)
            && (force || !self.server_check_errors.contains_key(id))
        {
            let (tx, rx) = channel();
            self.server_check_pending.insert(id.into(), rx);
            std::thread::spawn(move || {
                let _ = tx.send(
                    tmux::probe(&profile.target)
                        .map(|_| ())
                        .map_err(|e| e.to_string()),
                );
            });
        }
    }

    fn poll_settings(&mut self) {
        if matches!(self.mode, Mode::Settings) {
            if let Some(id) = self.settings.selected_server_id(&self.registry) {
                if self
                    .registry
                    .server(&id)
                    .is_some_and(|p| self.remote_probes.get_profile(p).is_none())
                {
                    self.check_server(&id, false);
                }
            }
        }
        let completed: Vec<_> = self
            .server_check_pending
            .iter()
            .filter_map(|(id, rx)| match rx.try_recv() {
                Ok(result) => Some((id.clone(), result)),
                Err(std::sync::mpsc::TryRecvError::Disconnected) => Some((
                    id.clone(),
                    Err("Server check stopped unexpectedly; press F5 to retry".into()),
                )),
                Err(std::sync::mpsc::TryRecvError::Empty) => None,
            })
            .collect();
        for (id, result) in completed {
            self.server_check_pending.remove(&id);
            self.server_check_errors.insert(id, result.err());
        }
        let setups: Vec<_> = self
            .setup_pending
            .iter()
            .filter_map(|(key, rx)| match rx.try_recv() {
                Ok(result) => Some((key.clone(), result)),
                Err(std::sync::mpsc::TryRecvError::Disconnected) => Some((
                    key.clone(),
                    Err("Setup launch ended unexpectedly; reopen to retry".into()),
                )),
                _ => None,
            })
            .collect();
        for (key, result) in setups {
            self.setup_pending.remove(&key);
            match result {
                Ok(entry) => {
                    self.panes.insert(key, entry);
                }
                Err(error) => {
                    self.buffered_input.remove(&key);
                    self.settings.set_status(error);
                    if matches!(&self.mode, Mode::ServerConsole(selected) if selected == &key) {
                        self.mode = Mode::Settings;
                        self.focus = Focus::Nav;
                    }
                }
            }
        }
    }

    fn settings_action(&mut self, action: SettingsAction, cols: usize, rows: usize) {
        let result: Result<Option<String>, String> = match action {
            SettingsAction::Help => {
                self.mode = Mode::Help(PrevMode::Settings);
                Ok(None)
            }
            SettingsAction::Home => {
                self.go_home();
                Ok(None)
            }
            SettingsAction::Leave => {
                self.leave_requested = true;
                Ok(None)
            }
            SettingsAction::OpenHelper { server_id } => {
                self.settings.select_server(&server_id);
                self.open_helper(cols, rows);
                Ok(None)
            }
            SettingsAction::Back => {
                self.mode = if self.settings_back == PrevMode::Task {
                    Mode::Task
                } else {
                    Mode::Home
                };
                Ok(None)
            }
            SettingsAction::Check { server_id } => {
                self.check_server(&server_id, true);
                Ok(None)
            }
            SettingsAction::OpenTerminal { server_id } => {
                self.open_setup(&server_id, None, cols, rows).map(|_| None)
            }
            SettingsAction::Configure { server_id, hkey } => self
                .open_setup(&server_id, Some(&hkey), cols, rows)
                .map(|_| None),
            SettingsAction::Create {
                name,
                target,
                repository_root,
            } => self
                .registry
                .create_server(&name, target, repository_root, Source::HumanUi)
                .map(Some)
                .map_err(|e| e.to_string()),
            SettingsAction::Update {
                server_id,
                name,
                repository_root,
            } => self
                .registry
                .update_server(&server_id, &name, repository_root, Source::HumanUi)
                .map(|_| Some(server_id))
                .map_err(|e| e.to_string()),
            SettingsAction::SaveHarness {
                server_id,
                hkey,
                settings,
            } => self
                .registry
                .set_server_harness(&server_id, &hkey, settings, Source::HumanUi)
                .map(|_| Some(server_id))
                .map_err(|e| e.to_string()),
            SettingsAction::Archive { server_id } => self
                .registry
                .archive_server(&server_id, Source::HumanUi)
                .map(|_| Some("local".into()))
                .map_err(|e| e.to_string()),
        };
        match result {
            Ok(Some(id)) => {
                self.settings.saved(&id);
                self.check_server(&id, true);
                self.settings.set_status(
                    "Saved. Defaults apply to new agents; running sessions are unchanged.",
                );
            }
            Ok(None) => {}
            Err(error) => self.settings.set_status(error),
        }
    }

    fn open_setup(
        &mut self,
        id: &str,
        harness: Option<&str>,
        cols: usize,
        rows: usize,
    ) -> Result<(), String> {
        let profile = self
            .registry
            .server(id)
            .cloned()
            .ok_or("Server profile no longer exists")?;
        let harness = harness.map(str::to_owned);
        let runtime = self
            .registry
            .bind_server_setup(
                id,
                harness.as_deref().unwrap_or("terminal"),
                &private_server_name(&self.home_dir),
            )
            .map_err(|e| e.to_string())?;
        let key = runtime.session.clone();
        if !self
            .panes
            .get(&key)
            .is_some_and(|e| e.pane.lock().unwrap().is_alive())
            && !self.setup_pending.contains_key(&key)
        {
            let (tx, rx) = channel();
            self.setup_pending.insert(key.clone(), rx);
            self.buffered_input.entry(key.clone()).or_default();
            let home = self.home_dir.clone();
            std::thread::spawn(move || {
                let open = || -> Result<PaneEntry, String> {
                    let status = tmux::inspect(&runtime).map_err(|e| e.to_string())?;
                    if !matches!(status, RuntimeStatus::Live { .. }) {
                        let command = launch::settings_command(&profile, harness.as_deref())?;
                        let root = profile
                            .repository_root
                            .clone()
                            .unwrap_or_else(|| PathBuf::from("~"));
                        let root = if profile.target == ConnectionTarget::Local
                            && (root == Path::new("~") || root.starts_with("~/"))
                        {
                            PathBuf::from(
                                std::env::var_os("HOME")
                                    .ok_or("HOME is required for the setup folder")?,
                            )
                            .join(root.strip_prefix("~").unwrap())
                        } else {
                            root
                        };
                        let cwd = tmux::resolve_repository(&profile.target, &root)
                            .map_err(|e| e.to_string())?
                            .ok_or_else(|| {
                                format!(
                                    "Default repository folder {} does not exist on {}",
                                    root.display(),
                                    profile.name
                                )
                            })?;
                        if matches!(status, RuntimeStatus::Dead { .. }) {
                            tmux::kill(&runtime).map_err(|e| e.to_string())?;
                        }
                        tmux::create(
                            &runtime,
                            &command.program,
                            &command.args,
                            &cwd,
                            &[],
                            rows.saturating_sub(5) as u16,
                            cols as u16,
                        )
                        .map_err(|e| e.to_string())?;
                        launch::confirm_runtime_started(&runtime, "Setup console")?;
                    }
                    // A new ignore-size viewer already has the desired parser
                    // size, so rendering cannot discover an old native size.
                    // Resize the persisted window before attaching to it.
                    if matches!(status, RuntimeStatus::Live { rows: old_rows, cols: old_cols, .. }
                        if (old_rows, old_cols) != (rows.saturating_sub(5) as u16, cols as u16))
                        && !tmux::resize(&runtime, rows.saturating_sub(5) as u16, cols as u16)
                            .map_err(|e| e.to_string())?
                    {
                        return Err("Setup console exited before reattachment; its saved mapping was retained".into());
                    }
                    let attach = tmux::attach_command(&runtime).map_err(|e| e.to_string())?;
                    let args: Vec<_> = attach.args.iter().map(String::as_str).collect();
                    let pane = Pane::spawn(
                        &attach.program,
                        &args,
                        &home,
                        rows.saturating_sub(5) as u16,
                        cols as u16,
                        &[],
                    )
                    .map_err(|e| e.to_string())?;
                    Ok(PaneEntry {
                        pane: Arc::new(Mutex::new(pane)),
                        runtime,
                    })
                };
                let _ = tx.send(open());
            });
        }
        self.mode = Mode::ServerConsole(key);
        self.focus = Focus::Cli;
        self.status = None;
        Ok(())
    }

    fn setup_frame(&mut self, key: &str, cols: usize, rows: usize) -> Vec<String> {
        let mut frame = vec![vrow(cols); rows];
        frame[0] = taskai_tui::chrome::focus_edge(cols, "CLI INPUT", self.focus == Focus::Cli);
        let server = self
            .registry
            .servers()
            .iter()
            .find(|s| s.setup_runtimes.values().any(|r| r.session == key));
        let name = server.map(|s| s.name.as_str()).unwrap_or("Server");
        frame[1] = row(
            &[s(format!("SETUP · {name} · {key}"), Some(TEXT), Some(VOID))],
            cols,
            VOID,
            2,
        );
        let height = rows.saturating_sub(5);
        if let Some(entry) = self.panes.get(key) {
            let mut pane = entry.pane.lock().unwrap();
            if pane.size() != (height, cols) {
                let _ = pane.resize(height as u16, cols as u16);
                let _ = self
                    .resize_tx
                    .send((entry.runtime.clone(), height as u16, cols as u16));
            }
            let snapshot = pane.snapshot();
            self.host_cursor = map_child_cursor(snapshot.cursor, 3, 0, height, cols, true);
            for (r, cells) in snapshot.cells.iter().take(height).enumerate() {
                let cells: Vec<_> = cells
                    .iter()
                    .map(|c| PaneCell {
                        text: c.text.clone(),
                        fg: c.fg,
                        bg: c.bg,
                        bold: c.bold,
                        italic: c.italic,
                        underline: c.underline,
                        inverse: c.inverse,
                        width: c.width,
                    })
                    .collect();
                frame[3 + r] = cell_row(&cells, cols, CLI_BG);
            }
        } else {
            frame[4] = row(
                &[s(
                    if self.setup_pending.contains_key(key) {
                        "Opening setup console… input is buffered"
                    } else {
                        "Setup console exited or disconnected. Ctrl+G returns; open it again to reconnect."
                    },
                    Some(FT),
                    Some(VOID),
                )],
                cols,
                VOID,
                2,
            );
        }
        frame[rows - 1] = row(
            &[s(
                "Ctrl+G  Settings · Wheel  Scroll CLI · Esc/End  Leave history · Console persists in tmux",
                Some(TEXT),
                Some(VOID),
            )],
            cols,
            VOID,
            2,
        );
        frame
    }

    fn open_newtask(&mut self) {
        let repo = self
            .registry
            .server("local")
            .and_then(|p| p.repository_root.as_ref())
            .map(|p| p.display().to_string())
            .unwrap_or_default();
        self.mode = Mode::NewTask(NewTaskForm {
            name: String::new(),
            repo,
            server_id: "local".into(),
            focus: 2,
            explorer: None,
        });
    }

    fn poll_new_task(&mut self) {
        let Some((_, _, rx)) = &self.pending_task else {
            return;
        };
        let result = match rx.try_recv() {
            Ok(result) => result,
            Err(std::sync::mpsc::TryRecvError::Empty) => return,
            Err(_) => Err("Repository check stopped unexpectedly; try again".into()),
        };
        let (server_id, name, _) = self.pending_task.take().unwrap();
        if !matches!(self.mode, Mode::NewTask(_)) {
            return;
        }
        let result = result.and_then(|repo| {
            self.registry
                .create_task_on_server(&name, &repo, &server_id, Source::HumanUi)
                .map_err(|e| e.to_string())
        });
        match result {
            Ok(task) => {
                self.sel_task = Some(task);
                self.st.rail_sel = 0;
                self.mode = Mode::Task;
                self.status = None;
            }
            Err(error) => self.status = Some(error),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::OsString;
    use std::time::Instant;
    use taskai_tui::seg::strip_sgr;

    struct EnvRestore {
        name: &'static str,
        value: Option<OsString>,
        _guard: std::sync::MutexGuard<'static, ()>,
    }

    impl EnvRestore {
        fn set(name: &'static str, value: &Path) -> Self {
            static PROVIDER_ENV: Mutex<()> = Mutex::new(());
            let guard = PROVIDER_ENV
                .lock()
                .unwrap_or_else(|poison| poison.into_inner());
            let old = std::env::var_os(name);
            std::env::set_var(name, value);
            Self {
                name,
                value: old,
                _guard: guard,
            }
        }
    }

    impl Drop for EnvRestore {
        fn drop(&mut self) {
            match &self.value {
                Some(value) => std::env::set_var(self.name, value),
                None => std::env::remove_var(self.name),
            }
        }
    }

    struct RuntimeCleanup(Option<TmuxRuntimeLocator>);

    impl Drop for RuntimeCleanup {
        fn drop(&mut self) {
            if let Some(runtime) = &self.0 {
                let _ = tmux::kill(runtime);
            }
        }
    }

    struct RuntimeSetCleanup(Vec<TmuxRuntimeLocator>);

    impl Drop for RuntimeSetCleanup {
        fn drop(&mut self) {
            for runtime in self.0.iter().rev() {
                let _ = tmux::kill(runtime);
            }
        }
    }

    fn unique_private_server(label: &str) -> String {
        use std::sync::atomic::{AtomicU64, Ordering};

        static NEXT: AtomicU64 = AtomicU64::new(1);
        format!(
            "taskai-test-{label}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        )
    }

    fn private_tmux_sessions(private_server: &str) -> Vec<String> {
        let output = std::process::Command::new("/usr/bin/env")
            .args(["-u", "TMUX", "-u", "TMUX_PANE", "-u", "TMUX_TMPDIR"])
            .args([
                "tmux",
                "-N",
                "-f",
                "/dev/null",
                "-L",
                private_server,
                "list-sessions",
                "-F",
                "#{session_name}",
            ])
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "list private tmux sessions: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        let mut sessions: Vec<String> = String::from_utf8_lossy(&output.stdout)
            .lines()
            .map(str::to_owned)
            .collect();
        sessions.sort();
        sessions
    }

    fn start_test_runtime(
        app: &mut App,
        cleanup: &mut RuntimeSetCleanup,
        session_ref: &str,
        private_server: &str,
        workspace: &Path,
    ) -> TmuxRuntimeLocator {
        app.registry
            .bind_runtime(session_ref, private_server)
            .unwrap();
        let runtime = app
            .registry
            .session(session_ref)
            .unwrap()
            .runtime
            .clone()
            .unwrap();
        // Register cleanup before creation so even a partially successful tmux command is
        // limited to this exact temporary session when an assertion aborts the test.
        cleanup.0.push(runtime.clone());
        let pid_file = workspace.join(format!("{session_ref}.pid"));
        let cwd_file = workspace.join(format!("{session_ref}.cwd"));
        tmux::create(
            &runtime,
            "/bin/sh",
            &[
                "-c".into(),
                "printf '%s\\n' \"$$\" > \"$TASKAI_TEST_PID\"; pwd -P > \"$TASKAI_TEST_CWD\"; exec sleep 600"
                    .into(),
            ],
            workspace,
            &[
                (
                    "TASKAI_TEST_PID".into(),
                    pid_file.display().to_string(),
                ),
                (
                    "TASKAI_TEST_CWD".into(),
                    cwd_file.display().to_string(),
                ),
            ],
            24,
            80,
        )
        .unwrap();
        app.registry
            .set_state(session_ref, SessionState::Live)
            .unwrap();
        assert!(eventually(|| {
            pid_file.exists()
                && cwd_file.exists()
                && matches!(tmux::inspect(&runtime), Ok(RuntimeStatus::Live { .. }))
        }));
        assert_eq!(
            std::fs::read_to_string(cwd_file).unwrap().trim(),
            workspace.canonicalize().unwrap().display().to_string()
        );
        runtime
    }

    fn eventually(mut predicate: impl FnMut() -> bool) -> bool {
        for _ in 0..100 {
            if predicate() {
                return true;
            }
            std::thread::sleep(Duration::from_millis(25));
        }
        false
    }

    #[test]
    fn cli_mouse_routes_only_the_child_rectangle_in_every_view() {
        for (view_name, left, top, width, height) in [
            ("inline", TASK_PANE_BODY_LEFT, TASK_PANE_BODY_TOP, 114, 31),
            (
                "navigation",
                TASK_PANE_BODY_LEFT,
                TASK_PANE_BODY_TOP,
                114,
                31,
            ),
            ("fullscreen", 0, ATTACH_BODY_TOP, 150, 37),
            ("setup", 0, SETUP_BODY_TOP, 150, 37),
        ] {
            let root = tempfile::tempdir().unwrap();
            let mut app = App::new_test(root.path().join("state")).unwrap();
            let task = app
                .registry
                .create_task("Mouse", root.path(), Source::HumanUi)
                .unwrap();
            let manager = app
                .registry
                .create_session(&task, None, "Lead", "claude", root.path(), Source::HumanUi)
                .unwrap();
            app.registry
                .create_session(&task, None, "Peer", "claude", root.path(), Source::HumanUi)
                .unwrap();
            let pane = Pane::spawn(
                "/bin/sh",
                &[
                    "-c",
                    "stty raw -echo; printf '\\033[?1002h\\033[?1006hREADY'; exec cat >mouse-input",
                ],
                root.path(),
                height,
                width,
                &[],
            )
            .unwrap();
            assert!(eventually(|| pane
                .mouse_state()
                .encode(64, 0, 0, false)
                .is_some()));
            app.panes.insert(
                manager.clone(),
                PaneEntry {
                    pane: Arc::new(Mutex::new(pane)),
                    runtime: TmuxRuntimeLocator {
                        target: ConnectionTarget::Local,
                        private_server: "taskai-mouse-test".into(),
                        session: manager.clone(),
                    },
                },
            );
            app.sel_task = Some(task);
            app.focus = if view_name == "navigation" {
                Focus::Nav
            } else {
                Focus::Cli
            };
            app.mode = match view_name {
                "inline" => Mode::TaskCli,
                "navigation" => Mode::Task,
                "fullscreen" => Mode::Attach,
                _ => Mode::ServerConsole(manager.clone()),
            };
            let wheel = |x, y, kind, modifiers| MouseEvent {
                kind,
                column: x as u16,
                row: y as u16,
                modifiers,
            };
            app.handle_mouse(
                wheel(
                    left + 10,
                    top + 5,
                    MouseEventKind::ScrollUp,
                    KeyModifiers::NONE,
                ),
                150,
                42,
            );
            app.handle_mouse(
                wheel(
                    left + 12,
                    top + 7,
                    MouseEventKind::ScrollDown,
                    KeyModifiers::CONTROL | KeyModifiers::SHIFT,
                ),
                150,
                42,
            );
            let expected = b"\x1b[<64;11;6M\x1b[<85;13;8M";
            assert!(
                eventually(|| std::fs::read(root.path().join("mouse-input"))
                    .ok()
                    .as_deref()
                    == Some(expected)),
                "{view_name}: translated wheel did not reach child"
            );
            // Headers, gap below body, bottom shortcuts and (when focused) rail
            // must not enter child input or move the selected keyboard recipient.
            for (x, y) in [
                (left + 5, top - 1),
                (left + 5, top + height as usize),
                (100, 41),
            ] {
                app.handle_mouse(
                    wheel(x, y, MouseEventKind::ScrollDown, KeyModifiers::NONE),
                    150,
                    42,
                );
            }
            if view_name == "inline" {
                app.handle_mouse(
                    wheel(5, 10, MouseEventKind::ScrollDown, KeyModifiers::NONE),
                    150,
                    42,
                );
            }
            assert_eq!(
                app.st.rail_sel, 0,
                "{view_name}: scrolling chrome changed recipient"
            );
            assert_eq!(
                app.focus,
                if view_name == "navigation" {
                    Focus::Nav
                } else {
                    Focus::Cli
                }
            );
            // Gestures must not queue while the viewer is initializing or when
            // terminal resize has invalidated the displayed coordinate system.
            app.buffered_input.insert(manager.clone(), Vec::new());
            app.handle_mouse(
                wheel(
                    left + 3,
                    top + 3,
                    MouseEventKind::ScrollUp,
                    KeyModifiers::NONE,
                ),
                150,
                42,
            );
            assert!(app.buffered_input[&manager].is_empty());
            app.buffered_input.remove(&manager);
            app.handle_mouse(
                wheel(
                    left + 3,
                    top + 3,
                    MouseEventKind::ScrollUp,
                    KeyModifiers::NONE,
                ),
                151,
                42,
            );
            // A final in-bounds packet is an ordered barrier: any erroneous
            // preceding forwarding would appear before it in the same input log.
            app.handle_mouse(
                wheel(
                    left + 10,
                    top + 5,
                    MouseEventKind::ScrollUp,
                    KeyModifiers::NONE,
                ),
                150,
                42,
            );
            let final_expected = [expected.as_slice(), b"\x1b[<64;11;6M"].concat();
            assert!(
                eventually(|| std::fs::read(root.path().join("mouse-input"))
                    .ok()
                    .as_deref()
                    == Some(final_expected.as_slice())),
                "{view_name}: chrome/pending/resize leaked input"
            );
        }
    }

    #[test]
    fn cli_mouse_drag_releases_at_body_edge_and_navigation_click_does_not_leak() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        let task = app
            .registry
            .create_task("Drag", root.path(), Source::HumanUi)
            .unwrap();
        let manager = app
            .registry
            .create_session(&task, None, "Lead", "claude", root.path(), Source::HumanUi)
            .unwrap();
        let pane = Pane::spawn(
            "/bin/sh",
            &[
                "-c",
                "stty raw -echo; printf '\\033[?1002h\\033[?1006hREADY'; exec cat >mouse-input",
            ],
            root.path(),
            31,
            114,
            &[],
        )
        .unwrap();
        assert!(eventually(|| pane
            .mouse_state()
            .encode(64, 0, 0, false)
            .is_some()));
        app.panes.insert(
            manager.clone(),
            PaneEntry {
                pane: Arc::new(Mutex::new(pane)),
                runtime: TmuxRuntimeLocator {
                    target: ConnectionTarget::Local,
                    private_server: "taskai-mouse-test".into(),
                    session: manager,
                },
            },
        );
        app.sel_task = Some(task);
        app.mode = Mode::TaskCli;
        app.focus = Focus::Cli;
        let event = |kind, x, y| MouseEvent {
            kind,
            column: x,
            row: y,
            modifiers: KeyModifiers::NONE,
        };
        app.handle_mouse(
            event(MouseEventKind::Down(MouseButton::Left), 40, 10),
            150,
            42,
        );
        app.handle_mouse(
            event(MouseEventKind::Drag(MouseButton::Left), 0, 0),
            150,
            42,
        );
        app.handle_mouse(event(MouseEventKind::Up(MouseButton::Left), 0, 0), 150, 42);
        let expected = b"\x1b[<0;5;5M\x1b[<32;1;1M\x1b[<0;1;1m";
        assert!(eventually(|| std::fs::read(
            root.path().join("mouse-input")
        )
        .ok()
        .as_deref()
            == Some(expected)));
        assert!(app.cli_mouse_press.is_none());
        app.mode = Mode::Task;
        app.focus = Focus::Nav;
        assert!(!app.forward_cli_mouse(
            event(MouseEventKind::Down(MouseButton::Left), 40, 10),
            150,
            42
        ));
        app.mode = Mode::TaskCli;
        app.focus = Focus::Cli;
        assert!(app.forward_cli_mouse(
            event(MouseEventKind::Up(MouseButton::Left), 40, 10),
            150,
            42
        ));
        app.handle_mouse(event(MouseEventKind::ScrollDown, 40, 10), 150, 42);
        let expected = [expected.as_slice(), b"\x1b[<65;5;5M"].concat();
        assert!(eventually(|| std::fs::read(
            root.path().join("mouse-input")
        )
        .ok()
        .as_deref()
            == Some(expected.as_slice())));
    }

    #[test]
    fn cli_mouse_capture_cancels_once_without_leaking_to_another_viewer() {
        for transition in [
            "ctrl-g",
            "layout",
            "resize",
            "pane-resize",
            "compact",
            "modal",
            "other-pane",
            "replacement",
            "pending",
        ] {
            let root = tempfile::tempdir().unwrap();
            let mut app = App::new_test(root.path().join("state")).unwrap();
            let task = app
                .registry
                .create_task("Gesture", root.path(), Source::HumanUi)
                .unwrap();
            let mut refs = Vec::new();
            let mut viewers = Vec::new();
            for name in ["original", "other"] {
                let session = app
                    .registry
                    .create_session(&task, None, name, "claude", root.path(), Source::HumanUi)
                    .unwrap();
                let pane = Pane::spawn(
                    "/bin/sh",
                    &[
                        "-c",
                        "stty raw -echo; printf '\\033[?1002h\\033[?1006hREADY'; exec cat >\"$1\"",
                        "mouse-fixture",
                        name,
                    ],
                    root.path(),
                    31,
                    114,
                    &[],
                )
                .unwrap();
                assert!(eventually(|| pane
                    .mouse_state()
                    .encode(64, 0, 0, false)
                    .is_some()));
                let pane = Arc::new(Mutex::new(pane));
                app.panes.insert(
                    session.clone(),
                    PaneEntry {
                        pane: pane.clone(),
                        runtime: TmuxRuntimeLocator {
                            target: ConnectionTarget::Local,
                            private_server: "taskai-mouse-test".into(),
                            session: session.clone(),
                        },
                    },
                );
                refs.push(session);
                viewers.push(pane);
            }
            app.sel_task = Some(task);
            app.mode = Mode::TaskCli;
            app.focus = Focus::Cli;
            let event = |kind, column, row, modifiers| MouseEvent {
                kind,
                column,
                row,
                modifiers,
            };
            let modifiers = KeyModifiers::CONTROL | KeyModifiers::SHIFT;
            app.handle_mouse(
                event(MouseEventKind::Down(MouseButton::Left), 40, 10, modifiers),
                150,
                42,
            );
            app.handle_mouse(
                event(MouseEventKind::Drag(MouseButton::Left), 48, 14, modifiers),
                150,
                42,
            );
            assert!(app.cli_mouse_press.is_some(), "{transition}: no capture");
            let (mut cols, mut rows) = (150, 42);
            match transition {
                "ctrl-g" | "layout" => {
                    app.handle(
                        KeyEvent::new(
                            KeyCode::Char(if transition == "ctrl-g" { 'g' } else { 'f' }),
                            KeyModifiers::CONTROL,
                        ),
                        cols,
                        rows,
                    );
                    assert!(
                        app.cli_mouse_press.is_none(),
                        "{transition}: keyboard cancellation"
                    );
                }
                "resize" => cols = 151,
                "pane-resize" => viewers[0].lock().unwrap().resize(30, 113).unwrap(),
                "compact" => (cols, rows) = (20, 10),
                "modal" => {
                    app.mode = Mode::Help(PrevMode::Task);
                    app.frame(cols, rows);
                    assert!(app.cli_mouse_press.is_none(), "modal: frame cancellation");
                }
                "other-pane" => app.st.rail_sel = 1,
                "replacement" => {
                    app.panes.get_mut(&refs[0]).unwrap().pane = viewers[1].clone();
                }
                "pending" => {
                    app.buffered_input.insert(refs[0].clone(), Vec::new());
                }
                _ => unreachable!(),
            }
            // The physical release after cancellation must not produce another
            // release in the old pane or an unmatched release in the new one.
            for _ in 0..2 {
                app.handle_mouse(
                    event(
                        MouseEventKind::Up(MouseButton::Left),
                        60,
                        20,
                        KeyModifiers::NONE,
                    ),
                    cols,
                    rows,
                );
            }
            assert!(
                app.cli_mouse_press.is_none(),
                "{transition}: capture survived"
            );
            assert!(app.buffered_input.values().all(Vec::is_empty));
            // Ordered barriers prove the complete logs, not an intermediate
            // prefix before a leaked release happens to reach the fixture.
            for pane in &viewers {
                pane.lock().unwrap().write_bytes(b"BARRIER").unwrap();
            }
            let expected = b"\x1b[<20;5;5M\x1b[<52;13;9M\x1b[<20;13;9mBARRIER";
            assert!(
                eventually(
                    || std::fs::read(root.path().join("original")).ok().as_deref()
                        == Some(expected)
                ),
                "{transition}: original viewer did not get exactly one last-position release"
            );
            assert!(
                eventually(
                    || std::fs::read(root.path().join("other")).ok().as_deref() == Some(b"BARRIER")
                ),
                "{transition}: release leaked to the other viewer"
            );
        }
    }

    #[test]
    fn cli_mouse_navigation_footer_does_not_focus_the_terminal() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        let task = app
            .registry
            .create_task("Footer", root.path(), Source::HumanUi)
            .unwrap();
        app.registry
            .create_session(&task, None, "Lead", "claude", root.path(), Source::HumanUi)
            .unwrap();
        app.sel_task = Some(task);
        app.mode = Mode::Task;
        app.focus = Focus::Nav;
        app.handle_mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: 80,
                row: 37,
                modifiers: KeyModifiers::NONE,
            },
            150,
            42,
        );
        assert!(matches!(app.mode, Mode::Task));
        assert_eq!(app.focus, Focus::Nav);
        assert!(app.pending_launches.is_empty());
        assert!(app.status.is_none());
    }

    #[test]
    fn accepted_viewer_buffers_new_input_until_its_first_native_frame() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        let task = app
            .registry
            .create_task("Viewer", root.path(), Source::HumanUi)
            .unwrap();
        let runtime = app
            .registry
            .bind_terminal(&task, "taskai-test-first-frame")
            .unwrap();
        let key = runtime.session.clone();
        let pane = Pane::spawn("/bin/sh", &["-c", "stty raw -echo; while [ ! -f ready ]; do sleep 0.01; done; printf '\\033[2J\\033[H'; dd bs=1 count=1 of=input 2>/dev/null; sleep 1"], root.path(), 24, 80, &[]).unwrap();
        app.sel_task = Some(task);
        app.st.rail_sel = 0;
        app.mode = Mode::TaskCli;
        app.focus = Focus::Cli;
        app.accept_launch(
            &key,
            launch::Outcome {
                pane,
                runtime,
                created: true,
                claude_events: None,
            },
        );
        app.handle(
            KeyEvent::new(KeyCode::Char('x'), KeyModifiers::NONE),
            150,
            42,
        );
        assert_eq!(app.buffered_input[&key].len(), 1);
        assert!(!root.path().join("input").exists());
        std::fs::write(root.path().join("ready"), b"ready").unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            app.poll_launches();
            if std::fs::read(root.path().join("input")).ok().as_deref() == Some(b"x") {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "input did not reach first-frame-ready viewer"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        assert!(!app.buffered_input.contains_key(&key));
        assert!(app.panes[&key].pane.lock().unwrap().has_output());
        assert!(app.panes[&key]
            .pane
            .lock()
            .unwrap()
            .text()
            .trim()
            .is_empty());
    }

    #[test]
    fn helper_chooser_is_non_mutating_and_clearly_explains_local_scope() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            app.mode = Mode::Helper {
                selection: 0,
                back: HelperBack::Settings,
            };
            let frame = app.frame(cols, rows);
            assert_eq!(frame.len(), rows);
            assert!(frame
                .iter()
                .all(|line| taskai_tui::seg::cell_count(line) == cols));
            let text = frame
                .iter()
                .map(|l| strip_sgr(l))
                .collect::<Vec<_>>()
                .join("\n");
            assert!(text.contains("Runs locally"));
            assert!(text.contains("No prompt is sent automatically"));
            assert!(text.contains("Open helper"));
            assert!(text
                .lines()
                .any(|line| line.contains("Claude Code") && line.contains("Checking")));
            let top = rows.saturating_sub(25) / 2;
            app.handle_mouse(
                MouseEvent {
                    kind: MouseEventKind::Down(MouseButton::Left),
                    column: (cols / 2) as u16,
                    row: (top + 11) as u16,
                    modifiers: KeyModifiers::NONE,
                },
                cols,
                rows,
            );
            assert!(matches!(app.mode, Mode::Helper { selection: 2, .. }));
            app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), cols, rows);
            assert!(matches!(app.mode, Mode::Settings));
        }
        assert!(app.registry.tasks().is_empty());
        assert!(app.panes.is_empty());
        assert!(app.pending_launches.is_empty());
        assert!(!app.home_dir.join("helper").exists());
    }

    #[test]
    fn helper_local_settings_retains_provider_and_returns_to_its_actual_origin() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        let task = app
            .registry
            .create_task("Existing project", root.path(), Source::HumanUi)
            .unwrap();
        let remote = app
            .registry
            .create_server(
                "Other server",
                ConnectionTarget::ssh("review-fixture").unwrap(),
                None,
                Source::HumanUi,
            )
            .unwrap();
        app.sel_task = Some(task);
        for (back, previous, returns_to_task) in [
            (HelperBack::Home, PrevMode::Task, false),
            (HelperBack::Task, PrevMode::Home, true),
            (HelperBack::Settings, PrevMode::Task, true),
            (HelperBack::Settings, PrevMode::Home, false),
        ] {
            for (selection, hkey) in HKEYS.iter().enumerate() {
                app.settings.select_server(&remote);
                app.settings.select_harness("claude");
                app.settings_back = previous;
                app.mode = Mode::Helper { selection, back };
                app.focus = Focus::Nav;
                app.handle(
                    KeyEvent::new(KeyCode::Char('h'), KeyModifiers::NONE),
                    150,
                    42,
                );
                assert!(matches!(app.mode, Mode::Settings));
                assert_eq!(
                    app.settings.selected_server_id(&app.registry).as_deref(),
                    Some("local")
                );
                // Inspect the action only: this regression must never launch a
                // native provider while checking the h → c handoff.
                let action = app.settings.handle(
                    KeyEvent::new(KeyCode::Char('c'), KeyModifiers::NONE),
                    &app.registry,
                );
                assert!(
                    matches!(action, Some(SettingsAction::Configure { server_id, hkey: selected }) if server_id == "local" && selected == *hkey)
                );
                app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 150, 42);
                assert_eq!(matches!(app.mode, Mode::Task), returns_to_task);
                assert_eq!(matches!(app.mode, Mode::Home), !returns_to_task);
            }
        }
        assert!(app.registry.helper().manager_ref.is_none());
        assert!(app.panes.is_empty());
        assert!(app.pending_launches.is_empty());
    }

    #[test]
    fn helper_catalogue_failure_retains_error_and_is_not_presented_as_pending() {
        let pending = App::helper_choices_from_result(None);
        assert_eq!(pending.len(), HKEYS.len());
        assert!(pending.iter().all(|choice| choice
            .unavailable
            .as_deref()
            .unwrap()
            .starts_with("Checking local harnesses")));
        let failed = Err("probe worker ended unexpectedly".into());
        let choices = App::helper_choices_from_result(Some(&failed));
        assert_eq!(choices.len(), HKEYS.len());
        for choice in choices {
            let detail = choice.unavailable.as_deref().unwrap();
            assert!(detail.contains("probe worker ended unexpectedly"));
            assert!(detail.contains("F5 retries"));
            assert!(!detail.contains("Checking"));
            assert_eq!(choice.status_label(), "Couldn't verify");
            assert!(!choice.is_ready());
        }
        let ready = Ok(vec![HarnessAvailability {
            hkey: "claude".into(),
            version: Some("2.1.226".into()),
            unavailable: None,
            auth: crate::remote_probes::AuthState::SignedIn,
        }]);
        assert_eq!(
            App::helper_choices_from_result(Some(&ready)),
            *ready.as_ref().unwrap()
        );
    }

    #[test]
    fn helper_requests_require_explicit_review_and_preserve_current_drafts() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        let (_, manager, _) = app
            .registry
            .ensure_helper("claude", root.path(), Source::HumanUi)
            .unwrap();
        let (tx, rx) = std::sync::mpsc::sync_channel(8);
        app.helper_requests = rx;
        app.open_newtask();
        if let Mode::NewTask(form) = &mut app.mode {
            form.name = "Do not lose my draft".into();
        }
        let before = std::fs::read(app.home_dir.join("state/state.json")).unwrap();
        let (reply, receive) = channel();
        tx.send(control::UiRequest {
            source: manager.clone(),
            action: "add-server".into(),
            argument: None,
            reply,
        })
        .unwrap();
        app.poll_helper_requests();
        assert!(receive.recv().unwrap().is_ok());
        app.handle(KeyEvent::new(KeyCode::F(8), KeyModifiers::NONE), 150, 42);
        assert!(matches!(&app.mode, Mode::NewTask(f) if f.name=="Do not lose my draft"));
        assert!(app.helper_action.is_some());
        assert_eq!(
            std::fs::read(app.home_dir.join("state/state.json")).unwrap(),
            before
        );
        app.mode = Mode::Home;
        app.handle(KeyEvent::new(KeyCode::F(8), KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::Settings));
        assert!(app.helper_action.is_none());
        assert!(app
            .frame(150, 42)
            .iter()
            .any(|line| strip_sgr(line).contains("Add server")));
        assert_eq!(
            std::fs::read(app.home_dir.join("state/state.json")).unwrap(),
            before
        );
        let (reply, receive) = channel();
        tx.send(control::UiRequest {
            source: "s999".into(),
            action: "new-task".into(),
            argument: None,
            reply,
        })
        .unwrap();
        app.poll_helper_requests();
        assert!(receive.recv().unwrap().is_err());
        assert!(app.helper_action.is_none());
        let (reply, receive) = channel();
        tx.send(control::UiRequest {
            source: manager,
            action: "new-task".into(),
            argument: None,
            reply,
        })
        .unwrap();
        app.poll_helper_requests();
        assert!(receive.recv().unwrap().is_ok());
        app.mode = Mode::Home;
        app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 150, 42);
        assert!(app.helper_action.is_none());
        assert_eq!(
            std::fs::read(app.home_dir.join("state/state.json")).unwrap(),
            before
        );
    }

    #[test]
    fn helper_ui_bridge_uses_existing_socket_without_needing_an_attached_pane() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        let (_, manager, _) = app
            .registry
            .ensure_helper("claude", root.path(), Source::HumanUi)
            .unwrap();
        let socket = control::sock_path(&app.home_dir);
        let client = std::thread::spawn(move || {
            control::client_helper_ui(&socket, &manager, "new-task", None)
        });
        let deadline = Instant::now() + Duration::from_secs(5);
        while !client.is_finished() {
            app.poll_helper_requests();
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(10));
        }
        let result = client.join().unwrap().unwrap();
        assert_eq!(result["ok"], true);
        assert!(result["message"].as_str().unwrap().contains("not applied"));
        assert!(app.panes.is_empty());
        assert!(app.helper_action.is_some());
        assert_eq!(app.registry.tasks().len(), 1);
    }

    #[test]
    fn pending_repository_check_locks_draft_and_escape_discards_completion() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.open_newtask();
        if let Mode::NewTask(f) = &mut app.mode {
            f.name = "Draft".into();
            f.focus = 0;
        }
        let (tx, rx) = channel();
        app.pending_task = Some(("local".into(), "Draft".into(), rx));
        app.handle_paste("changed");
        app.handle(
            KeyEvent::new(KeyCode::Char('x'), KeyModifiers::NONE),
            150,
            42,
        );
        assert!(matches!(&app.mode, Mode::NewTask(f) if f.name == "Draft"));
        assert!(app
            .frame(150, 42)
            .iter()
            .any(|r| strip_sgr(r).contains("Cancel check")));
        app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 150, 42);
        assert!(tx.send(Ok(root.path().to_path_buf())).is_err());
        app.poll_new_task();
        assert!(app.registry.tasks().is_empty());
        assert!(matches!(&app.mode, Mode::NewTask(f) if f.name == "Draft"));
    }

    #[test]
    fn repository_completion_creates_task_on_selected_saved_server() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        let id = app
            .registry
            .create_server(
                "Build machine",
                ConnectionTarget::ssh("example-host").unwrap(),
                None,
                Source::HumanUi,
            )
            .unwrap();
        app.open_newtask();
        let (tx, rx) = channel();
        app.pending_task = Some((id.clone(), "Remote project".into(), rx));
        tx.send(Ok("/srv/project".into())).unwrap();
        app.poll_new_task();
        let task = &app.registry.tasks()[0];
        assert_eq!(task.server_profile_id.as_deref(), Some(id.as_str()));
        assert_eq!(task.repo, PathBuf::from("/srv/project"));
        assert_eq!(
            task.connection,
            ConnectionTarget::ssh("example-host").unwrap()
        );
        assert!(matches!(app.mode, Mode::Task));
    }

    #[test]
    fn settings_shortcuts_are_visible_and_return_to_the_correct_screen() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.mode = Mode::Home;
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            assert!(app
                .frame(cols, rows)
                .iter()
                .any(|r| strip_sgr(r).contains("s  Settings")));
        }
        app.handle(
            KeyEvent::new(KeyCode::Char('h'), KeyModifiers::NONE),
            150,
            42,
        );
        assert!(matches!(app.mode, Mode::Settings));
        app.settings_action(SettingsAction::Back, 150, 42);
        assert!(matches!(app.mode, Mode::Home));
        let task = app
            .registry
            .create_task("Test", root.path(), Source::HumanUi)
            .unwrap();
        app.sel_task = Some(task);
        app.mode = Mode::Task;
        app.handle(
            KeyEvent::new(KeyCode::Char(','), KeyModifiers::NONE),
            150,
            42,
        );
        assert!(matches!(app.mode, Mode::Settings));
        app.settings_action(SettingsAction::Back, 150, 42);
        assert!(matches!(app.mode, Mode::Task));
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            assert!(app
                .frame(cols, rows)
                .iter()
                .any(|r| strip_sgr(r).contains("s  Settings")));
        }
    }

    #[test]
    fn escape_after_failed_server_add_cancels_nearest_layer_even_when_compact() {
        for (cols, rows) in [(110, 30), (273, 58), (109, 30), (110, 29), (40, 12)] {
            for from_task in [false, true] {
                let root = tempfile::tempdir().unwrap();
                let mut app = App::new_test(root.path().join("home")).unwrap();
                let task = app
                    .registry
                    .create_task("Previous task", root.path(), Source::HumanUi)
                    .unwrap();
                app.sel_task = Some(task.clone());
                app.mode = if from_task { Mode::Task } else { Mode::Home };
                let before = std::fs::read(app.home_dir.join("state/state.json")).unwrap();
                app.handle(
                    KeyEvent::new(KeyCode::Char('s'), KeyModifiers::NONE),
                    150,
                    42,
                );
                app.handle(
                    KeyEvent::new(KeyCode::Char('n'), KeyModifiers::NONE),
                    150,
                    42,
                );
                app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
                assert!(app.settings.has_dialog());
                // A stale request from the prior task must not eat a dialog's
                // first Escape, regardless of the current render dimensions.
                app.pending_harness_action = Some((task, 'm'));
                app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), cols, rows);
                assert!(matches!(app.mode, Mode::Settings));
                assert!(!app.settings.has_dialog());
                assert!(app.pending_harness_action.is_none());
                app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), cols, rows);
                assert_eq!(matches!(app.mode, Mode::Task), from_task);
                assert_eq!(matches!(app.mode, Mode::Home), !from_task);
                assert_eq!(
                    std::fs::read(app.home_dir.join("state/state.json")).unwrap(),
                    before
                );
                assert!(!app.leave_requested);
            }
        }
    }

    #[test]
    fn compact_form_does_not_treat_q_or_ctrl_g_as_navigation() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        app.mode = Mode::Settings;
        app.handle(
            KeyEvent::new(KeyCode::Char('n'), KeyModifiers::NONE),
            150,
            42,
        );
        app.handle_paste("Keep this draft");
        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('q'), KeyModifiers::NONE),
            80,
            20
        ));
        app.handle(
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            80,
            20,
        );
        assert!(app.settings.has_dialog());
        assert!(matches!(app.mode, Mode::Settings));
        assert!(app
            .frame(150, 42)
            .iter()
            .any(|row| strip_sgr(row).contains("Keep this draft")));
        app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 80, 20);
        assert!(!app.settings.has_dialog());
    }

    #[test]
    fn settings_help_returns_to_selection_and_does_not_launch_helper() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        app.mode = Mode::Settings;
        app.settings.select_harness("codex");
        app.handle(
            KeyEvent::new(KeyCode::Char('?'), KeyModifiers::NONE),
            150,
            42,
        );
        assert!(matches!(app.mode, Mode::Help(PrevMode::Settings)));
        assert!(app.registry.tasks().is_empty());
        app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::Settings));
        assert!(
            matches!(app.settings.handle(KeyEvent::new(KeyCode::Char('c'), KeyModifiers::NONE), &app.registry), Some(SettingsAction::Configure { hkey, .. }) if hkey == "codex")
        );
        app.mode = Mode::Helper {
            selection: 2,
            back: HelperBack::Settings,
        };
        app.handle(
            KeyEvent::new(KeyCode::Char('?'), KeyModifiers::NONE),
            150,
            42,
        );
        app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 150, 42);
        assert!(matches!(
            app.mode,
            Mode::Helper {
                selection: 2,
                back: HelperBack::Settings
            }
        ));
        app.mode = Mode::Settings;
        app.handle(
            KeyEvent::new(KeyCode::Char('G'), KeyModifiers::SHIFT),
            150,
            42,
        );
        assert!(matches!(app.mode, Mode::Home));
    }

    #[test]
    fn shortcut_labels_fit_task_footer_at_minimum_and_large_sizes() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        app.sel_task = Some(
            app.registry
                .create_task("Task", root.path(), Source::HumanUi)
                .unwrap(),
        );
        app.mode = Mode::Task;
        for (cols, rows) in [(110, 30), (150, 42), (273, 58)] {
            let frame = app.frame(cols, rows);
            let footer = frame[rows - 3..rows - 1]
                .iter()
                .map(|r| strip_sgr(r))
                .collect::<Vec<_>>()
                .join("\n");
            for label in [
                "s  Settings",
                "t  Terminal",
                "?  Help",
                "G  Home",
                "Esc  Back",
                "q  Leave",
                "F1  Setup helper",
                "d  Destroy agent",
                "D  Destroy task",
            ] {
                assert!(
                    footer.contains(label),
                    "{cols}x{rows} missing {label}: {footer}"
                );
            }
        }
    }

    #[test]
    fn focus_highlight_tracks_input_owner_without_resizing_or_inventing_a_cursor() {
        for child_visible in [false, true] {
            let root = tempfile::tempdir().unwrap();
            let mut app = App::new_test(root.path().join("home")).unwrap();
            let task = app
                .registry
                .create_task("Focus", root.path(), Source::HumanUi)
                .unwrap();
            let manager = app
                .registry
                .create_session(&task, None, "Lead", "claude", root.path(), Source::HumanUi)
                .unwrap();
            let script = format!(
                "stty raw -echo; printf '\\033[?25{}FOCUS_FIXTURE'; exec cat",
                if child_visible { "h" } else { "l" }
            );
            let pane = Pane::spawn("/bin/sh", &["-c", &script], root.path(), 31, 114, &[]).unwrap();
            assert!(eventually(|| pane.text().contains("FOCUS_FIXTURE")));
            let pane = Arc::new(Mutex::new(pane));
            app.panes.insert(
                manager.clone(),
                PaneEntry {
                    pane: Arc::clone(&pane),
                    runtime: TmuxRuntimeLocator {
                        target: ConnectionTarget::Local,
                        private_server: "taskai-focus-test".into(),
                        session: manager,
                    },
                },
            );
            app.sel_task = Some(task);
            app.mode = Mode::Task;
            app.focus = Focus::Nav;
            let nav = app.frame(150, 42);
            assert!(strip_sgr(&nav[4]).starts_with("▸ NAVIGATION"));
            assert!(app.host_cursor.is_none());
            for _ in 0..3 {
                app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
                let cli = app.frame(150, 42);
                assert!(strip_sgr(&cli[4]).contains("▸ CLI INPUT"));
                assert!(!strip_sgr(&cli[4]).contains("▸ NAVIGATION"));
                assert_eq!(app.host_cursor.is_some(), child_visible);
                assert_eq!(pane.lock().unwrap().size(), (31, 114));
                for kind in [
                    MouseEventKind::Moved,
                    MouseEventKind::ScrollUp,
                    MouseEventKind::ScrollDown,
                ] {
                    app.handle_mouse(
                        MouseEvent {
                            kind,
                            column: 5,
                            row: 10,
                            modifiers: KeyModifiers::NONE,
                        },
                        150,
                        42,
                    );
                    assert_eq!(
                        app.focus,
                        Focus::Cli,
                        "pointer movement must not steal focus"
                    );
                }
                app.handle(
                    KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
                    150,
                    42,
                );
                app.frame(150, 42);
                assert!(app.host_cursor.is_none());
                assert_eq!(pane.lock().unwrap().size(), (31, 114));
            }
            app.mode = Mode::Attach;
            app.focus = Focus::Cli;
            assert!(strip_sgr(&app.frame(150, 42)[0]).starts_with("▸ CLI INPUT"));
            assert_eq!(app.host_cursor.is_some(), child_visible);
        }
    }

    #[test]
    fn navigation_letters_and_escape_remain_native_cli_input() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        let (task, manager, _) = app
            .registry
            .ensure_helper("claude", root.path(), Source::HumanUi)
            .unwrap();
        app.sel_task = Some(task);
        for mode in [
            Mode::TaskCli,
            Mode::Attach,
            Mode::ServerConsole(manager.clone()),
        ] {
            app.mode = mode;
            app.focus = Focus::Cli;
            app.buffered_input.insert(manager.clone(), Vec::new());
            for code in [
                KeyCode::Esc,
                KeyCode::Char('s'),
                KeyCode::Char('t'),
                KeyCode::Char('?'),
                KeyCode::Char('G'),
                KeyCode::Char('q'),
            ] {
                assert!(app.handle(KeyEvent::new(code, KeyModifiers::NONE), 150, 42));
                assert!(app.focus == Focus::Cli);
            }
            let bytes: Vec<u8> = app.buffered_input[&manager]
                .iter()
                .flat_map(|input| match input {
                    BufferedInput::Bytes(bytes) => bytes.clone(),
                    _ => panic!("expected key bytes"),
                })
                .collect();
            assert_eq!(bytes, b"\x1bst?Gq");
            assert!(!app.leave_requested);
        }
    }

    #[cfg(unix)]
    #[test]
    fn settings_create_schedules_checks_before_any_frame_or_poll() {
        const CHILD: &str = "TASKAI_TEST_ADD_SERVER_CHECK_CHILD";
        const LOG: &str = "TASKAI_TEST_ADD_SERVER_CHECK_LOG";
        if std::env::var_os(CHILD).is_none() {
            use std::os::unix::fs::PermissionsExt;
            use std::process::{Command, Stdio};

            // Only this exact test's subprocess receives the SSH shim. Mutating
            // process-wide PATH would race unrelated parallel tests and could
            // accidentally redirect a real session's launch.
            let fixture = tempfile::tempdir().unwrap();
            let ssh = fixture.path().join("ssh");
            let log = fixture.path().join("checks.log");
            std::fs::write(
                &ssh,
                r#"#!/bin/sh
case "$*" in
  *claude*) probe=claude; version='2.1.263 (Claude Code)' ;;
  *codex*) probe=codex; version='codex-cli 0.150.0' ;;
  *opencode*) probe=opencode; version='1.2.0' ;;
  *grok*) probe=grok; version='1.0.13' ;;
  *tmux*) probe=tmux; version='tmux 3.4' ;;
  *) printf '%s\n' 'Unexpected isolated SSH probe' >&2; exit 70 ;;
esac
printf '%s\n' "$probe" >> "$TASKAI_TEST_ADD_SERVER_CHECK_LOG"
printf '%s\n' "$version"
"#,
            )
            .unwrap();
            std::fs::set_permissions(&ssh, std::fs::Permissions::from_mode(0o755)).unwrap();
            let path = std::env::join_paths(std::iter::once(fixture.path().to_path_buf()).chain(
                std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
            ))
            .unwrap();
            let mut child = Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "app::tests::settings_create_schedules_checks_before_any_frame_or_poll",
                    "--nocapture",
                ])
                .env(CHILD, "1")
                .env(LOG, &log)
                .env("PATH", path)
                .stdout(Stdio::piped())
                .stderr(Stdio::piped())
                .spawn()
                .unwrap();
            let deadline = Instant::now() + Duration::from_secs(15);
            while child.try_wait().unwrap().is_none() {
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    let output = child.wait_with_output().unwrap();
                    panic!(
                        "isolated server check test timed out: {} {}",
                        String::from_utf8_lossy(&output.stdout),
                        String::from_utf8_lossy(&output.stderr)
                    );
                }
                std::thread::sleep(Duration::from_millis(10));
            }
            let output = child.wait_with_output().unwrap();
            assert!(
                output.status.success(),
                "isolated server check test failed: {} {}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
            return;
        }

        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        app.mode = Mode::Settings;
        let log =
            PathBuf::from(std::env::var_os(LOG).expect("child requires its isolated SSH log"));
        assert!(!log.exists());

        // A failed Add must not create an entry or check a different existing
        // server as a side effect. Local is already the built-in profile.
        app.settings_action(
            SettingsAction::Create {
                name: "Duplicate local".into(),
                target: ConnectionTarget::Local,
                repository_root: None,
            },
            150,
            42,
        );
        assert_eq!(app.registry.servers().len(), 1);
        assert!(app.server_check_pending.is_empty());
        assert!(app
            .remote_probes
            .get_profile(app.registry.server("local").unwrap())
            .is_none());
        assert!(!log.exists());

        let target = ConnectionTarget::ssh("isolated-add-check").unwrap();
        app.settings_action(
            SettingsAction::Create {
                name: "Immediate checks".into(),
                target: target.clone(),
                repository_root: Some("/srv/projects".into()),
            },
            150,
            42,
        );
        let profile = app
            .registry
            .servers()
            .iter()
            .find(|p| p.target == target)
            .unwrap();
        let id = profile.server_id.clone();
        assert_eq!(
            app.settings.selected_server_id(&app.registry).as_deref(),
            Some(id.as_str())
        );
        assert!(
            app.server_check_pending.contains_key(&id),
            "Add must schedule tmux immediately"
        );
        assert!(app.settings_check().unwrap().checking);
        let reopened = Registry::open(&app.home_dir.join("state")).unwrap();
        let persisted = reopened
            .server(&id)
            .expect("Add must persist the profile immediately");
        assert_eq!(persisted.target, target);
        assert_eq!(
            persisted.repository_root.as_deref(),
            Some(Path::new("/srv/projects"))
        );

        // Deliberately never call frame(), poll_settings(), or cache refresh:
        // only the Add action can have started these five harmless checks.
        // Scheduling is asserted synchronously above. Allow subprocess CPU
        // scheduling under full-suite load, without a 3s machine-speed SLA.
        let deadline = Instant::now() + Duration::from_secs(8);
        loop {
            let started = std::fs::read_to_string(&log).unwrap_or_default();
            if ["tmux", "claude", "codex", "opencode", "grok"]
                .iter()
                .all(|probe| started.lines().any(|line| line == *probe))
            {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "Add did not start all checks: {started}"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        assert!(app.registry.tasks().is_empty());
        assert!(app.panes.is_empty());
    }

    #[test]
    fn setup_default_local_folder_opens_and_reuses_the_same_tracked_shell() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.open_setup("local", None, 110, 30).unwrap();
        let runtime = app.registry.server("local").unwrap().setup_runtimes["terminal"].clone();
        let _cleanup = RuntimeCleanup(Some(runtime.clone()));
        let deadline = Instant::now() + Duration::from_secs(10);
        while !app.setup_pending.is_empty() {
            app.poll_settings();
            assert!(Instant::now() < deadline, "setup launch did not finish");
            std::thread::sleep(Duration::from_millis(20));
        }
        assert!(
            app.panes.contains_key(&runtime.session),
            "default local folder did not open"
        );
        let original = tmux::inspect(&runtime).unwrap();
        assert!(matches!(original, RuntimeStatus::Live { .. }));
        app.panes.clear();
        app.mode = Mode::Settings;
        app.open_setup("local", None, 150, 42).unwrap();
        while !app.setup_pending.is_empty() {
            app.poll_settings();
            assert!(Instant::now() < deadline, "setup reattach did not finish");
            std::thread::sleep(Duration::from_millis(20));
        }
        assert!(app.panes.contains_key(&runtime.session));
        let RuntimeStatus::Live { pid, .. } = original else {
            unreachable!()
        };
        assert!(
            matches!(tmux::inspect(&runtime).unwrap(), RuntimeStatus::Live { pid: current, cols: 150, rows: 37 } if current == pid)
        );
        assert_eq!(
            app.panes[&runtime.session].pane.lock().unwrap().size(),
            (37, 150)
        );
        assert!(app.registry.tasks().is_empty());
        assert_eq!(app.registry.hud_counts().live, 0);
        app.panes.clear();
    }

    #[test]
    fn setup_console_boundary_returns_to_settings_without_sending_input() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        for (cols, rows) in [(80, 20), (150, 42)] {
            app.mode = Mode::ServerConsole("setup-local-terminal".into());
            app.focus = Focus::Cli;
            app.handle(
                KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
                cols,
                rows,
            );
            assert!(matches!(app.mode, Mode::Settings));
            assert_eq!(app.focus, Focus::Nav);
        }
    }

    #[test]
    fn portfolio_search_click_and_escape_follow_real_task_ids() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state-home")).unwrap();
        let mut target = String::new();
        for index in 0..35 {
            let task = app
                .registry
                .create_task(&format!("Project {index:02}"), root.path(), Source::HumanUi)
                .unwrap();
            if index == 31 {
                target = task;
            }
        }
        app.mode = Mode::Home;
        app.handle(
            KeyEvent::new(KeyCode::Char('/'), KeyModifiers::NONE),
            150,
            42,
        );
        app.handle_paste("project 31");
        assert_eq!(app.home_rows().len(), 1);
        assert_eq!(app.home_rows()[0].name, "Project 31");
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert_eq!(app.sel_task.as_ref(), Some(&target));
        assert!(matches!(app.mode, Mode::Task));
        app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::Home));
        assert_eq!(app.st.home_sel, 31);
        let row = (0..42)
            .find(|y| home::task_at_row(35, 31, 150, 42, *y) == Some(31))
            .unwrap();
        app.handle_mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: 10,
                row: row as u16,
                modifiers: KeyModifiers::NONE,
            },
            150,
            42,
        );
        assert_eq!(app.sel_task.as_ref(), Some(&target));
        assert!(matches!(app.mode, Mode::Task));
        assert!(
            app.panes.is_empty(),
            "browsing empty task must not create a shell"
        );
        assert!(app
            .home_rows()
            .iter()
            .all(|t| t.spark.is_empty() && !t.spark_live));
    }

    #[test]
    fn task_returns_to_selected_agent_and_click_enters_cli() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        let task = app
            .registry
            .create_task("Focus memory", root.path(), Source::HumanUi)
            .unwrap();
        app.registry
            .create_session(&task, None, "First", "claude", root.path(), Source::HumanUi)
            .unwrap();
        let second = app
            .registry
            .create_session(&task, None, "Second", "codex", root.path(), Source::HumanUi)
            .unwrap();
        let pane = Pane::spawn("/bin/cat", &[], root.path(), 31, 114, &[]).unwrap();
        app.panes.insert(
            second.clone(),
            PaneEntry {
                pane: Arc::new(Mutex::new(pane)),
                runtime: TmuxRuntimeLocator {
                    target: ConnectionTarget::Local,
                    private_server: "taskai-test-no-runtime".into(),
                    session: second.clone(),
                },
            },
        );
        app.sel_task = Some(task.clone());
        app.st.rail_sel = 1;
        app.mode = Mode::Task;
        app.go_home();
        app.open_task(task, 150, 42);
        assert_eq!(app.selected_pane().unwrap().key, second);
        assert!(matches!(app.mode, Mode::TaskCli));
        app.handle(
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            150,
            42,
        );
        let view = app.task_view(150, 42).unwrap();
        let row = (0..42)
            .find(|y| task::rail_at_row(&view, &app.st, 42, *y) == Some(1))
            .unwrap();
        app.handle_mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: 10,
                row: row as u16,
                modifiers: KeyModifiers::NONE,
            },
            150,
            42,
        );
        assert!(matches!(app.mode, Mode::TaskCli));
        assert_eq!(app.selected_pane().unwrap().key, second);
    }

    #[test]
    fn child_cursor_maps_into_each_focused_cli_view() {
        let child = CursorState {
            row: 7,
            col: 19,
            visible: true,
        };
        assert_eq!(
            map_child_cursor(
                child,
                TASK_PANE_BODY_TOP,
                TASK_PANE_BODY_LEFT,
                40,
                100,
                true,
            ),
            Some((13, 55))
        );
        assert_eq!(
            map_child_cursor(child, ATTACH_BODY_TOP, 0, 40, 100, true),
            Some((11, 19))
        );
        assert_eq!(
            map_child_cursor(
                child,
                TASK_PANE_BODY_TOP,
                TASK_PANE_BODY_LEFT,
                40,
                100,
                false,
            ),
            None
        );
        assert_eq!(
            map_child_cursor(
                CursorState {
                    visible: false,
                    ..child
                },
                TASK_PANE_BODY_TOP,
                TASK_PANE_BODY_LEFT,
                40,
                100,
                true,
            ),
            None
        );
        assert_eq!(
            map_child_cursor(
                CursorState { row: 40, ..child },
                TASK_PANE_BODY_TOP,
                TASK_PANE_BODY_LEFT,
                40,
                100,
                true,
            ),
            None
        );
        assert_eq!(
            map_child_cursor(
                CursorState { col: 100, ..child },
                TASK_PANE_BODY_TOP,
                TASK_PANE_BODY_LEFT,
                40,
                100,
                true,
            ),
            Some((13, 135))
        );
    }

    #[test]
    fn focused_terminal_paste_restores_only_requested_brackets() {
        assert_eq!(terminal_paste_bytes("one\ntwo", false), b"one\ntwo");
        assert_eq!(
            terminal_paste_bytes("one\ntwo", true),
            b"\x1b[200~one\ntwo\x1b[201~"
        );
    }

    #[test]
    fn startup_confirmation_rejects_a_process_that_only_flashes_live() {
        let root = tempfile::tempdir().unwrap();
        let runtime = TmuxRuntimeLocator {
            target: ConnectionTarget::Local,
            private_server: format!("taskai-test-flash-{}", std::process::id()),
            session: "s99".into(),
        };
        let _cleanup = RuntimeCleanup(Some(runtime.clone()));
        tmux::create(
            &runtime,
            "/bin/sh",
            &["-c".into(), "sleep 0.1".into()],
            root.path(),
            &[],
            24,
            80,
        )
        .unwrap();
        assert!(confirm_runtime_started(&runtime, "fake").is_err());
    }

    #[test]
    fn focused_app_paste_reaches_child_with_native_bracketed_semantics() {
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        std::fs::create_dir_all(&workspace).unwrap();
        let mut app = App::new_test(root.path().join("taskai-home")).unwrap();
        let task_ref = app
            .registry
            .create_task("Paste", &workspace, Source::HumanUi)
            .unwrap();
        let session_ref = app
            .registry
            .create_session(
                &task_ref,
                None,
                "Lead",
                "claude",
                &workspace,
                Source::HumanUi,
            )
            .unwrap();
        let pane = Pane::spawn(
            "/bin/sh",
            &["-c", "printf '\\033[?2004h'; exec cat -v"],
            &workspace,
            24,
            80,
            &[],
        )
        .unwrap();
        let pane = Arc::new(Mutex::new(pane));
        assert!(eventually(|| pane
            .lock()
            .unwrap()
            .snapshot()
            .bracketed_paste));
        app.panes.insert(
            session_ref.clone(),
            PaneEntry {
                pane,
                runtime: TmuxRuntimeLocator {
                    target: ConnectionTarget::Local,
                    private_server: "taskai-test-paste".into(),
                    session: session_ref.clone(),
                },
            },
        );
        app.sel_task = Some(task_ref);
        app.mode = Mode::TaskCli;
        app.focus = Focus::Cli;
        app.handle_paste("first line\nsecond line");

        assert!(eventually(|| {
            let text = app.panes[&session_ref].pane.lock().unwrap().text();
            text.contains("^[[200~first line") && text.contains("second line^[[201~")
        }));
    }

    // Expectation: the application records one tmux runtime, drops only its viewer,
    // reopens onto the same process and draft, then Task Destroy stops that runtime but
    // preserves the repository. Why: this is the approved behind-the-scenes replacement
    // contract. Value: registry, app, pane and tmux behavior are proven together for free.
    #[test]
    fn app_detaches_reattaches_and_destroys_one_durable_runtime() {
        use std::os::unix::fs::PermissionsExt;

        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        let taskai_home = root.path().join("taskai-home");
        std::fs::create_dir_all(&workspace).unwrap();
        std::fs::write(workspace.join("repository-sentinel"), "preserve me\n").unwrap();
        let fake = root.path().join("fake-claude");
        std::fs::write(
            &fake,
            "#!/bin/sh\n\
             if [ \"${1-}\" = \"--version\" ]; then echo '2.1.0 (Fake Claude)'; exit 0; fi\n\
             printf '%s\\n' \"$$\" > pid\n\
             printf x >> launches\n\
             (while :; do printf x >> heartbeat; sleep 0.05; done) &\n\
             beat=$!\n\
             trap 'kill \"$beat\" 2>/dev/null || true' EXIT HUP INT TERM\n\
             printf 'FAKE CLI — NO MODEL CALLS\\n'\n\
             cat\n",
        )
        .unwrap();
        std::fs::set_permissions(&fake, std::fs::Permissions::from_mode(0o755)).unwrap();
        let _env = EnvRestore::set("TASKAI_CLAUDE_BIN", &fake);

        let _cleanup = RuntimeCleanup(Some(TmuxRuntimeLocator {
            target: ConnectionTarget::Local,
            private_server: private_server_name(&taskai_home),
            session: "s1".into(),
        }));
        let (task_ref, session_ref, runtime, pid, heartbeat_before);
        {
            let mut app = App::new_test(taskai_home.clone()).unwrap();
            task_ref = app
                .registry
                .create_task("Durable", &workspace, Source::HumanUi)
                .unwrap();
            session_ref = app
                .registry
                .create_session(
                    &task_ref,
                    None,
                    "Lead",
                    "claude",
                    &workspace,
                    Source::HumanUi,
                )
                .unwrap();
            app.spawn_pane(&session_ref, 150, 42).unwrap();
            assert!(eventually(|| workspace.join("pid").exists()));
            runtime = app
                .registry
                .session(&session_ref)
                .unwrap()
                .runtime
                .clone()
                .unwrap();
            app.registry
                .bind_provider(
                    &session_ref,
                    ProviderMapping {
                        provider: "claude".into(),
                        provider_session_id: "provider-session-survives".into(),
                        runtime_id: format!("tmux:{}:{}", runtime.private_server, runtime.session),
                        connector_version: "fake".into(),
                    },
                )
                .unwrap();
            pid = std::fs::read_to_string(workspace.join("pid"))
                .unwrap()
                .trim()
                .parse::<u32>()
                .unwrap();
            assert!(matches!(
                tmux::inspect(&runtime),
                Ok(RuntimeStatus::Live { pid: observed, .. }) if observed == pid
            ));
            app.panes[&session_ref]
                .pane
                .lock()
                .unwrap()
                .write_bytes(b"draft-survives")
                .unwrap();
            assert!(eventually(|| app.panes[&session_ref]
                .pane
                .lock()
                .unwrap()
                .text()
                .contains("draft-survives")));
            heartbeat_before = std::fs::metadata(workspace.join("heartbeat"))
                .map(|m| m.len())
                .unwrap_or(0);
        }
        let mut cleanup = RuntimeCleanup(Some(runtime.clone()));
        assert!(eventually(|| {
            std::fs::metadata(workspace.join("heartbeat"))
                .map(|m| m.len() > heartbeat_before)
                .unwrap_or(false)
        }));

        let mut app = App::new_test(taskai_home).unwrap();
        // Startup no longer waits for tmux/SSH. The cached state stays queued
        // until the background inspection confirms the durable process.
        assert_eq!(
            app.registry.session(&session_ref).unwrap().state,
            SessionState::Queued
        );
        assert!(eventually(|| {
            app.poll_disconnected();
            app.disconnected.is_empty()
        }));
        assert_eq!(
            app.registry.session(&session_ref).unwrap().state,
            SessionState::Working
        );
        assert!(matches!(
            tmux::inspect(&runtime),
            Ok(RuntimeStatus::Live { pid: observed, .. }) if observed == pid
        ));
        app.sel_task = Some(task_ref.clone());
        app.spawn_pane(&session_ref, 150, 42).unwrap();
        assert_eq!(
            app.served
                .session_ids
                .lock()
                .unwrap()
                .get(&session_ref)
                .map(String::as_str),
            Some("provider-session-survives")
        );
        assert!(eventually(|| app.panes[&session_ref]
            .pane
            .lock()
            .unwrap()
            .text()
            .contains("draft-survives")));
        assert_eq!(
            std::fs::read_to_string(workspace.join("launches"))
                .unwrap()
                .len(),
            1
        );

        app.destroy_item(&Item::Task(task_ref)).unwrap();
        assert!(matches!(
            tmux::inspect(&runtime).unwrap(),
            RuntimeStatus::Missing
        ));
        assert_eq!(
            std::fs::read_to_string(workspace.join("repository-sentinel")).unwrap(),
            "preserve me\n"
        );
        cleanup.0 = None;
    }

    // AT-03. Expectation: the Task is only a tracker grouping; every Manager and
    // Subordinate gets one distinct tmux session and the recorded workspace is the
    // process's real cwd. Value: persistence and destruction can address agents exactly.
    #[test]
    fn each_agent_maps_to_one_tmux_session_and_task_maps_to_none() {
        let root = tempfile::tempdir().unwrap();
        let repository = root.path().join("repository");
        let manager_workspace = repository.clone();
        let first_workspace = root.path().join("first-subordinate");
        let second_workspace = root.path().join("second-subordinate");
        for workspace in [&manager_workspace, &first_workspace, &second_workspace] {
            std::fs::create_dir_all(workspace).unwrap();
        }
        std::fs::write(repository.join("sentinel"), b"repository stays\n").unwrap();

        let mut app = App::new_test(root.path().join("taskai-home")).unwrap();
        let task_ref = app
            .registry
            .create_task("Three agents", &repository, Source::HumanUi)
            .unwrap();
        let manager = app
            .registry
            .create_session(
                &task_ref,
                None,
                "Lead",
                "claude",
                &manager_workspace,
                Source::HumanUi,
            )
            .unwrap();
        let first = app
            .registry
            .create_session(
                &task_ref,
                Some(&manager),
                "Research",
                "claude",
                &first_workspace,
                Source::HumanUi,
            )
            .unwrap();
        let second = app
            .registry
            .create_session(
                &task_ref,
                Some(&manager),
                "Verification",
                "claude",
                &second_workspace,
                Source::HumanUi,
            )
            .unwrap();

        let private_server = unique_private_server("one-session-per-agent");
        let mut cleanup = RuntimeSetCleanup(Vec::new());
        let runtimes = [
            start_test_runtime(
                &mut app,
                &mut cleanup,
                &manager,
                &private_server,
                &manager_workspace,
            ),
            start_test_runtime(
                &mut app,
                &mut cleanup,
                &first,
                &private_server,
                &first_workspace,
            ),
            start_test_runtime(
                &mut app,
                &mut cleanup,
                &second,
                &private_server,
                &second_workspace,
            ),
        ];

        let expected = vec![manager.clone(), first.clone(), second.clone()];
        let mut expected_sorted = expected.clone();
        expected_sorted.sort();
        assert_eq!(private_tmux_sessions(&private_server), expected_sorted);
        assert!(!private_tmux_sessions(&private_server).contains(&task_ref));
        assert_eq!(app.registry.sessions_of(&task_ref).len(), 3);
        assert!(app
            .registry
            .sessions_of(&task_ref)
            .iter()
            .all(|session| session.runtime.is_some()));
        let distinct: std::collections::HashSet<_> = runtimes
            .iter()
            .map(|runtime| (&runtime.private_server, &runtime.session))
            .collect();
        assert_eq!(distinct.len(), 3);
        assert_eq!(
            std::fs::read(repository.join("sentinel")).unwrap(),
            b"repository stays\n"
        );
    }

    // AT-08. Expectation: selected-Subordinate, Manager-family, and Task destruction
    // stop only their mapped tmux sessions and remove only matching tracker records.
    // Value: the user's other active Task and every repository remain untouched.
    #[test]
    fn scoped_destroy_kills_exact_runtimes_and_preserves_peer_task_and_repositories() {
        let root = tempfile::tempdir().unwrap();
        let repo_a = root.path().join("repository-a");
        let repo_b = root.path().join("repository-b");
        std::fs::create_dir_all(&repo_a).unwrap();
        std::fs::create_dir_all(&repo_b).unwrap();
        let sentinel_a = b"task-a repository sentinel\n";
        let sentinel_b = b"task-b repository sentinel\n";
        std::fs::write(repo_a.join("sentinel"), sentinel_a).unwrap();
        std::fs::write(repo_b.join("sentinel"), sentinel_b).unwrap();

        let mut app = App::new_test(root.path().join("taskai-home")).unwrap();
        let task_a = app
            .registry
            .create_task("Task A", &repo_a, Source::HumanUi)
            .unwrap();
        let task_b = app
            .registry
            .create_task("Task B", &repo_b, Source::HumanUi)
            .unwrap();
        let manager_a = app
            .registry
            .create_session(
                &task_a,
                None,
                "Manager A",
                "claude",
                &repo_a,
                Source::HumanUi,
            )
            .unwrap();
        let subordinate_a1 = app
            .registry
            .create_session(
                &task_a,
                Some(&manager_a),
                "A one",
                "claude",
                &repo_a,
                Source::HumanUi,
            )
            .unwrap();
        let subordinate_a2 = app
            .registry
            .create_session(
                &task_a,
                Some(&manager_a),
                "A two",
                "claude",
                &repo_a,
                Source::HumanUi,
            )
            .unwrap();
        let peer_manager = app
            .registry
            .create_session(
                &task_a,
                None,
                "Peer Manager",
                "claude",
                &repo_a,
                Source::HumanUi,
            )
            .unwrap();
        let peer_subordinate = app
            .registry
            .create_session(
                &task_a,
                Some(&peer_manager),
                "Peer Subordinate",
                "claude",
                &repo_a,
                Source::HumanUi,
            )
            .unwrap();
        let manager_b = app
            .registry
            .create_session(
                &task_b,
                None,
                "Manager B",
                "claude",
                &repo_b,
                Source::HumanUi,
            )
            .unwrap();
        let subordinate_b = app
            .registry
            .create_session(
                &task_b,
                Some(&manager_b),
                "B one",
                "claude",
                &repo_b,
                Source::HumanUi,
            )
            .unwrap();

        let private_server = unique_private_server("scoped-destroy");
        let mut cleanup = RuntimeSetCleanup(Vec::new());
        let mut runtimes = std::collections::HashMap::new();
        for (session_ref, workspace) in [
            (&manager_a, &repo_a),
            (&subordinate_a1, &repo_a),
            (&subordinate_a2, &repo_a),
            (&peer_manager, &repo_a),
            (&peer_subordinate, &repo_a),
            (&manager_b, &repo_b),
            (&subordinate_b, &repo_b),
        ] {
            let runtime = start_test_runtime(
                &mut app,
                &mut cleanup,
                session_ref,
                &private_server,
                workspace,
            );
            runtimes.insert(session_ref.clone(), runtime);
        }

        assert_eq!(private_tmux_sessions(&private_server).len(), 7);

        assert_eq!(
            app.destroy_item(&Item::Session(subordinate_a1.clone()))
                .unwrap(),
            vec![subordinate_a1.clone()]
        );
        assert!(app.registry.session(&subordinate_a1).is_none());
        assert!(matches!(
            tmux::inspect(&runtimes[&subordinate_a1]).unwrap(),
            RuntimeStatus::Missing
        ));
        for survivor in [
            &manager_a,
            &subordinate_a2,
            &peer_manager,
            &peer_subordinate,
            &manager_b,
            &subordinate_b,
        ] {
            assert!(matches!(
                tmux::inspect(&runtimes[survivor]),
                Ok(RuntimeStatus::Live { .. })
            ));
        }

        assert_eq!(
            app.destroy_item(&Item::Session(manager_a.clone())).unwrap(),
            vec![manager_a.clone(), subordinate_a2.clone()]
        );
        for removed in [&manager_a, &subordinate_a2] {
            assert!(app.registry.session(removed).is_none());
            assert!(matches!(
                tmux::inspect(&runtimes[removed]).unwrap(),
                RuntimeStatus::Missing
            ));
        }
        for peer in [&peer_manager, &peer_subordinate] {
            assert!(app.registry.session(peer).is_some());
            assert!(matches!(
                tmux::inspect(&runtimes[peer]),
                Ok(RuntimeStatus::Live { .. })
            ));
        }

        assert_eq!(
            app.destroy_item(&Item::Task(task_a.clone())).unwrap(),
            vec![peer_manager.clone(), peer_subordinate.clone()]
        );
        assert!(app.registry.task(&task_a).is_none());
        assert!(app.registry.task(&task_b).is_some());
        assert!(app.registry.session(&manager_b).is_some());
        assert!(app.registry.session(&subordinate_b).is_some());
        for survivor in [&manager_b, &subordinate_b] {
            assert!(matches!(
                tmux::inspect(&runtimes[survivor]),
                Ok(RuntimeStatus::Live { .. })
            ));
        }
        let mut remaining = vec![manager_b.clone(), subordinate_b.clone()];
        remaining.sort();
        assert_eq!(private_tmux_sessions(&private_server), remaining);
        assert_eq!(std::fs::read(repo_a.join("sentinel")).unwrap(), sentinel_a);
        assert_eq!(std::fs::read(repo_b.join("sentinel")).unwrap(), sentinel_b);
    }

    #[test]
    fn startup_reconciliation_preserves_terminal_states_and_explains_missing_runtimes() {
        let root = tempfile::tempdir().unwrap();
        let home = root.path().join("taskai-home");
        let workspace = root.path().join("workspace");
        std::fs::create_dir_all(&workspace).unwrap();
        let (stale, complete, missing);
        {
            let mut registry = Registry::open(&home.join("state")).unwrap();
            let task = registry
                .create_task("Reconcile", &workspace, Source::HumanUi)
                .unwrap();
            stale = registry
                .create_session(
                    &task,
                    None,
                    "Legacy live",
                    "claude",
                    &workspace,
                    Source::HumanUi,
                )
                .unwrap();
            registry.set_state(&stale, SessionState::Live).unwrap();
            complete = registry
                .create_session(
                    &task,
                    None,
                    "Complete",
                    "claude",
                    &workspace,
                    Source::HumanUi,
                )
                .unwrap();
            registry
                .set_state(&complete, SessionState::Complete)
                .unwrap();
            missing = registry
                .create_session(
                    &task,
                    None,
                    "Missing",
                    "claude",
                    &workspace,
                    Source::HumanUi,
                )
                .unwrap();
            registry
                .bind_runtime(&missing, "taskai-test-recorded-but-missing")
                .unwrap();
            registry.set_state(&missing, SessionState::Live).unwrap();
        }

        let mut app = App::new_test(home).unwrap();
        assert!(eventually(|| {
            app.poll_disconnected();
            app.disconnected.is_empty()
        }));
        assert_eq!(
            app.registry.session(&stale).unwrap().state,
            SessionState::Queued
        );
        assert_eq!(
            app.registry.session(&complete).unwrap().state,
            SessionState::Complete
        );
        assert_eq!(
            app.registry.session(&missing).unwrap().state,
            SessionState::Queued
        );
        assert!(app.registry.session(&missing).unwrap().runtime.is_some());
        let status = app.status.as_deref().unwrap();
        assert!(status.contains(&format!("{stale}: no tmux runtime was recorded")));
        assert!(status.contains(&format!("{missing}: recorded tmux session is missing")));
    }

    #[test]
    fn destroy_shortcuts_separate_selected_agent_from_whole_task() {
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        std::fs::create_dir_all(&workspace).unwrap();
        let mut app = App::new_test(root.path().join("taskai-home")).unwrap();
        let task_ref = app
            .registry
            .create_task("Scoped destroy", &workspace, Source::HumanUi)
            .unwrap();
        let manager = app
            .registry
            .create_session(
                &task_ref,
                None,
                "Lead",
                "claude",
                &workspace,
                Source::HumanUi,
            )
            .unwrap();
        let subordinate = app
            .registry
            .create_session(
                &task_ref,
                Some(&manager),
                "Verification",
                "claude",
                &workspace,
                Source::HumanUi,
            )
            .unwrap();
        app.sel_task = Some(task_ref.clone());
        app.mode = Mode::Task;
        app.st.rail_sel = 0;

        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('d'), KeyModifiers::NONE),
            150,
            42,
        ));
        let Mode::Destroy(selected) = &app.mode else {
            panic!("lowercase d did not open selected-agent destroy");
        };
        assert_eq!(selected.item, Item::Session(manager));
        assert_eq!(selected.expect, "s1");
        assert_eq!(selected.blast_lines[0], "2 sessions will be closed");

        app.mode = Mode::Task;
        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('D'), KeyModifiers::SHIFT),
            150,
            42,
        ));
        let Mode::Destroy(task) = &app.mode else {
            panic!("uppercase D did not open task destroy");
        };
        assert_eq!(task.item, Item::Task(task_ref));
        assert!(task.blast_lines[0].starts_with("2 sessions"));
        assert_eq!(subordinate, "s2");
    }

    fn settle_launch(app: &mut App) {
        let remote = app
            .sel_task
            .as_ref()
            .and_then(|r| app.registry.task(r))
            .is_some_and(|t| t.connection != ConnectionTarget::Local);
        // SSH inspect may safely retry one 15s transport timeout; the UI does not
        // wait on this helper, it continues polling the background result.
        let deadline = Instant::now() + Duration::from_secs(if remote { 45 } else { 20 });
        while !app.pending_launches.is_empty() && Instant::now() < deadline {
            app.poll_launches();
            std::thread::sleep(Duration::from_millis(25));
        }
        assert!(
            app.pending_launches.is_empty(),
            "launch did not finish: {:?}",
            app.status
        );
        app.poll_launches();
    }

    #[test]
    fn utility_terminal_rejects_a_missing_repository_without_starting_a_shell() {
        let root = tempfile::tempdir().unwrap();
        let home = root.path().join("taskai-home");
        let workspace = root.path().join("missing-repository");
        let mut app = App::new_test(home).unwrap();
        let task = app
            .registry
            .create_task("Missing", &workspace, Source::HumanUi)
            .unwrap();
        app.sel_task = Some(task.clone());
        app.mode = Mode::Task;
        app.handle(
            KeyEvent::new(KeyCode::Char('t'), KeyModifiers::NONE),
            150,
            42,
        );
        settle_launch(&mut app);
        assert!(matches!(app.mode, Mode::Task));
        assert!(app.status.as_ref().unwrap().contains("does not exist"));
        let runtime = app.registry.task(&task).unwrap().terminal.as_ref().unwrap();
        assert!(matches!(
            tmux::inspect(runtime).unwrap(),
            RuntimeStatus::Missing
        ));
        assert!(app.panes.is_empty());
    }

    #[test]
    fn local_utility_terminal_lifecycle() {
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("repository");
        std::fs::create_dir(&workspace).unwrap();
        std::fs::write(workspace.join("sentinel"), "repository unchanged").unwrap();
        utility_terminal_lifecycle(root.path(), ConnectionTarget::Local, &workspace);
        assert_eq!(
            std::fs::read_to_string(workspace.join("sentinel")).unwrap(),
            "repository unchanged"
        );
    }

    #[test]
    #[ignore = "real shell only, no model calls; requires SSH cruise4b"]
    fn remote_utility_terminal_lifecycle() {
        let root = tempfile::tempdir().unwrap();
        utility_terminal_lifecycle(
            root.path(),
            ConnectionTarget::ssh("cruise4b").unwrap(),
            Path::new("/tmp"),
        );
    }

    fn utility_terminal_lifecycle(root: &Path, target: ConnectionTarget, workspace: &Path) {
        let home = root.join("taskai-home");
        let mut cleanup = RuntimeSetCleanup(Vec::new());
        let mut app = App::new_test(home.clone()).unwrap();
        let task = app
            .registry
            .create_task_on("Utility", workspace, target.clone(), Source::HumanUi)
            .unwrap();
        let manager = app
            .registry
            .create_session(&task, None, "Lead", "claude", workspace, Source::HumanUi)
            .unwrap();
        app.sel_task = Some(task.clone());
        app.mode = Mode::Task;
        let before = app.registry.hud_counts();
        assert!(app.registry.task(&task).unwrap().terminal.is_none());
        cleanup.0.push(TmuxRuntimeLocator {
            target: target.clone(),
            private_server: private_server_name(&home),
            session: terminal_ref(&task),
        });
        app.handle(
            KeyEvent::new(KeyCode::Char('t'), KeyModifiers::NONE),
            150,
            42,
        );
        assert!(matches!(app.mode, Mode::TaskCli), "{:?}", app.status);
        assert!(app.selected_session().is_none());
        settle_launch(&mut app);
        let key = terminal_ref(&task);
        assert!(
            app.panes.contains_key(&key),
            "shell viewer did not open: {:?}",
            app.status
        );
        let runtime = app.registry.task(&task).unwrap().terminal.clone().unwrap();
        let RuntimeStatus::Live { pid, .. } = tmux::inspect(&runtime).unwrap() else {
            panic!("shell not live")
        };
        assert_eq!(app.registry.hud_counts(), before);
        app.handle_paste("printf 'UTILITY_''READY\\n'; pwd; test -z \"${TASKAI_SESSION_REF:-}\" && printf 'NO_AGENT_''CONTEXT\\n'");
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        // A real user's login shell may run conda/plugin initialization before
        // it reads input. Give that startup more time than the fake-provider tests.
        let deadline = Instant::now() + Duration::from_secs(20);
        let mut command_arrived = false;
        while Instant::now() < deadline {
            app.poll_launches();
            let text = app.panes[&key].pane.lock().unwrap().text();
            if text.contains("UTILITY_READY")
                && text.contains("NO_AGENT_CONTEXT")
                && text.contains(workspace.to_str().unwrap())
            {
                command_arrived = true;
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(
            command_arrived,
            "shell output: {}",
            app.panes[&key].pane.lock().unwrap().text()
        );
        let pane_identity = Arc::as_ptr(&app.panes[&key].pane);
        app.handle(
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            150,
            42,
        );
        app.handle(
            KeyEvent::new(KeyCode::Char('t'), KeyModifiers::NONE),
            150,
            42,
        );
        assert_eq!(Arc::as_ptr(&app.panes[&key].pane), pane_identity);
        app.handle(
            KeyEvent::new(KeyCode::Char('f'), KeyModifiers::CONTROL),
            150,
            42,
        );
        assert!(matches!(app.mode, Mode::Attach));
        assert!(app
            .frame(150, 42)
            .iter()
            .any(|line| strip_sgr(line).contains("Terminal · Shell")));
        app.handle(
            KeyEvent::new(KeyCode::Char('f'), KeyModifiers::CONTROL),
            150,
            42,
        );
        assert!(eventually(|| {
            app.frame(150, 42);
            app.host_cursor.is_some()
        }));
        app.handle_paste("unsubmitted-utility-draft");
        assert!(eventually(|| app.panes[&key]
            .pane
            .lock()
            .unwrap()
            .text()
            .contains("unsubmitted-utility-draft")));
        drop(app);
        let mut app = App::new_test(home.clone()).unwrap();
        app.sel_task = Some(task.clone());
        app.mode = Mode::Task;
        app.handle(
            KeyEvent::new(KeyCode::Char('t'), KeyModifiers::NONE),
            150,
            42,
        );
        settle_launch(&mut app);
        assert!(
            matches!(tmux::inspect(&runtime).unwrap(), RuntimeStatus::Live { pid: current, .. } if current == pid)
        );
        assert!(eventually(|| app.panes[&key]
            .pane
            .lock()
            .unwrap()
            .text()
            .contains("unsubmitted-utility-draft")));
        app.destroy_item(&Item::Session(manager)).unwrap();
        assert!(
            matches!(tmux::inspect(&runtime).unwrap(), RuntimeStatus::Live { pid: current, .. } if current == pid)
        );
        assert!(app.registry.sessions_of(&task).is_empty());
        // Shell exit closes the runtime; opening again reuses the stored locator.
        app.st.rail_sel = 0;
        let before_exit = {
            let mut pane = app.panes[&key].pane.lock().unwrap();
            (
                pane.is_alive(),
                pane.snapshot().bracketed_paste,
                pane.text(),
            )
        };
        app.handle(
            KeyEvent::new(KeyCode::Char('u'), KeyModifiers::CONTROL),
            150,
            42,
        );
        app.handle_paste("exit");
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        let deadline = Instant::now() + Duration::from_secs(20);
        let mut exited = false;
        let mut final_inspection;
        loop {
            app.poll_launches();
            final_inspection = tmux::inspect(&runtime);
            if matches!(final_inspection, Ok(RuntimeStatus::Missing)) {
                exited = true;
                break;
            }
            if Instant::now() >= deadline {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        if !exited {
            let after_exit = {
                let mut pane = app.panes[&key].pane.lock().unwrap();
                (
                    pane.is_alive(),
                    pane.snapshot().bracketed_paste,
                    pane.text(),
                )
            };
            panic!("shell exit did not close runtime: {final_inspection:?}; before={before_exit:?}; after={after_exit:?}; focused_cli={}; buffered={}; status={:?}; native={:?}",
                app.focus == Focus::Cli, app.buffered_input.contains_key(&key), app.status,
                tmux::capture_test_terminal(&runtime));
        }
        assert!(eventually(|| !app.panes[&key]
            .pane
            .lock()
            .unwrap()
            .is_alive()));
        app.mode = Mode::Task;
        app.focus = Focus::Nav;
        app.handle(
            KeyEvent::new(KeyCode::Char('t'), KeyModifiers::NONE),
            150,
            42,
        );
        settle_launch(&mut app);
        assert!(matches!(
            tmux::inspect(&runtime).unwrap(),
            RuntimeStatus::Live { .. }
        ));
        assert_eq!(
            app.registry.task(&task).unwrap().terminal.as_ref(),
            Some(&runtime)
        );
        let peer = app
            .registry
            .create_task_on("Peer", workspace, target, Source::HumanUi)
            .unwrap();
        let peer_runtime = app
            .registry
            .bind_terminal(&peer, &private_server_name(&home))
            .unwrap();
        cleanup.0.push(peer_runtime.clone());
        tmux::create_terminal(&peer_runtime, workspace, 31, 114).unwrap();
        confirm_runtime_started(&peer_runtime, "Terminal").unwrap();
        app.destroy_item(&Item::Task(task)).unwrap();
        assert!(matches!(
            tmux::inspect(&runtime).unwrap(),
            RuntimeStatus::Missing
        ));
        assert!(matches!(
            tmux::inspect(&peer_runtime).unwrap(),
            RuntimeStatus::Live { .. }
        ));
        assert!(app.registry.task(&peer).unwrap().terminal.is_some());
    }

    #[test]
    fn new_manager_and_subordinate_accept_input_without_an_extra_focus_key() {
        use std::os::unix::fs::PermissionsExt;
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        std::fs::create_dir(&workspace).unwrap();
        let fake = root.path().join("input-provider");
        std::fs::write(&fake, "#!/bin/sh\nif [ \"${1-}\" = --version ]; then echo '2.1.0 (Test Claude)'; exit; fi\nprintf '\\033[?25hready\\n'\nexec cat\n").unwrap();
        std::fs::set_permissions(&fake, std::fs::Permissions::from_mode(0o755)).unwrap();
        let _env = EnvRestore::set("TASKAI_CLAUDE_BIN", &fake);
        let home = root.path().join("taskai-home");
        let _cleanup = RuntimeSetCleanup(
            ["s1", "s2"]
                .iter()
                .map(|sref| TmuxRuntimeLocator {
                    target: ConnectionTarget::Local,
                    private_server: private_server_name(&home),
                    session: (*sref).into(),
                })
                .collect(),
        );
        let mut app = App::new_test(home).unwrap();
        let task_ref = app
            .registry
            .create_task("Focus", &workspace, Source::HumanUi)
            .unwrap();
        app.sel_task = Some(task_ref);
        app.mode = Mode::Manager(ManagerForm {
            name: "Lead".into(),
            choices: vec![HarnessAvailability {
                auth: crate::remote_probes::AuthState::Unknown,
                hkey: "claude".into(),
                version: Some("2.1.0".into()),
                unavailable: None,
            }],
            hsel: 0,
            focus: 0,
        });
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::TaskCli), "{:?}", app.status);
        assert_eq!(app.focus, Focus::Cli);
        let manager = app.selected_session().unwrap().0;
        app.handle_paste("manager-can-type");
        settle_launch(&mut app);
        assert!(eventually(|| {
            app.poll_launches();
            app.panes[&manager]
                .pane
                .lock()
                .unwrap()
                .text()
                .contains("manager-can-type")
        }));
        app.frame(150, 42);
        assert!(app.host_cursor.is_some());
        app.handle(
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            150,
            42,
        );
        app.mode = Mode::Sub(SubForm {
            parent: manager,
            role: "Research".into(),
            choices: vec![HarnessAvailability {
                auth: crate::remote_probes::AuthState::Unknown,
                hkey: "claude".into(),
                version: Some("2.1.0".into()),
                unavailable: None,
            }],
            hsel: 0,
            ws_mode: 0,
            branch: String::new(),
            wname: String::new(),
            other: String::new(),
            focus: 0,
            explorer: None,
        });
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::TaskCli), "{:?}", app.status);
        let subordinate = app.selected_session().unwrap().0;
        app.handle_paste("subordinate-can-type");
        settle_launch(&mut app);
        assert!(eventually(|| {
            app.poll_launches();
            app.panes[&subordinate]
                .pane
                .lock()
                .unwrap()
                .text()
                .contains("subordinate-can-type")
        }));
        app.frame(150, 42);
        assert!(app.host_cursor.is_some());
    }

    #[test]
    fn empty_task_and_failed_focus_remain_in_navigation() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.registry
            .create_task("Empty", root.path(), Source::HumanUi)
            .unwrap();
        app.mode = Mode::Home;
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::Task));
        assert_eq!(app.focus, Focus::Nav);
        assert!(app.focus_session("s404", 150, 42, false).is_err());
        assert!(matches!(app.mode, Mode::Task));
        assert_eq!(app.focus, Focus::Nav);
    }

    #[test]
    fn task_navigation_opens_and_toggles_cli_layouts_then_returns_home() {
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        std::fs::create_dir_all(&workspace).unwrap();
        let mut app = App::new_test(root.path().join("taskai-home")).unwrap();
        let task_ref = app
            .registry
            .create_task("Embedded focus", &workspace, Source::HumanUi)
            .unwrap();
        let session_ref = app
            .registry
            .create_session(
                &task_ref,
                None,
                "Lead",
                "claude",
                &workspace,
                Source::HumanUi,
            )
            .unwrap();
        let pane = Pane::spawn("/bin/cat", &[], &workspace, 31, 114, &[]).unwrap();
        app.panes.insert(
            session_ref.clone(),
            PaneEntry {
                pane: Arc::new(Mutex::new(pane)),
                runtime: TmuxRuntimeLocator {
                    target: ConnectionTarget::Local,
                    private_server: "taskai-test-focus".into(),
                    session: session_ref.clone(),
                },
            },
        );
        app.sel_task = Some(task_ref);
        app.mode = Mode::Task;
        app.focus = Focus::Nav;
        let pane_identity = Arc::as_ptr(&app.panes[&session_ref].pane);

        let navigation = app.frame(150, 42);
        let navigation_text = navigation
            .iter()
            .map(|line| strip_sgr(line))
            .collect::<Vec<_>>()
            .join("\n");
        assert!(navigation_text.contains("Ctrl+F  Full screen"));
        assert!(navigation_text.contains("G  Home"));
        assert!(!navigation_text.contains("Enter  Full screen"));

        assert!(app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42));
        assert!(matches!(app.mode, Mode::TaskCli));
        assert_eq!(app.focus, Focus::Cli);
        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            150,
            42,
        ));
        assert!(matches!(app.mode, Mode::Task));

        // Opening the task from Home also enters its existing CLI immediately.
        app.mode = Mode::Home;
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::TaskCli));
        app.handle_paste("direct-input");
        assert!(eventually(|| app.panes[&session_ref]
            .pane
            .lock()
            .unwrap()
            .text()
            .contains("direct-input")));
        app.frame(150, 42);
        assert!(app.host_cursor.is_some());
        app.handle(
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            150,
            42,
        );
        app.frame(150, 42);
        assert!(app.host_cursor.is_none());

        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('f'), KeyModifiers::CONTROL),
            150,
            42,
        ));
        assert!(matches!(app.mode, Mode::Attach));
        assert_eq!(app.focus, Focus::Cli);
        assert_eq!(app.panes.len(), 1);
        assert_eq!(Arc::as_ptr(&app.panes[&session_ref].pane), pane_identity);

        let full_screen = app.frame(150, 42);
        let full_screen_text = full_screen
            .iter()
            .map(|line| strip_sgr(line))
            .collect::<Vec<_>>()
            .join("\n");
        assert!(full_screen_text.contains("Lead · Claude Code"));
        assert!(full_screen_text.contains("Ctrl+F  Inline"));
        assert!(full_screen_text.contains("Ctrl+G  Navigation"));
        assert!(!full_screen_text.contains("TASK · EMBEDDED FOCUS"));
        assert!(!full_screen_text.contains("TEAM"));

        for key in [
            KeyEvent::new(KeyCode::Char('q'), KeyModifiers::NONE),
            KeyEvent::new(KeyCode::Char('G'), KeyModifiers::SHIFT),
            KeyEvent::new(KeyCode::Char('a'), KeyModifiers::NONE),
            KeyEvent::new(KeyCode::Char('m'), KeyModifiers::NONE),
            KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
        ] {
            assert!(app.handle(key, 150, 42));
            assert!(matches!(app.mode, Mode::Attach));
            assert_eq!(app.focus, Focus::Cli);
        }

        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('f'), KeyModifiers::CONTROL),
            150,
            42,
        ));
        assert!(matches!(app.mode, Mode::TaskCli));
        assert_eq!(app.focus, Focus::Cli);
        assert_eq!(app.panes.len(), 1);
        assert_eq!(Arc::as_ptr(&app.panes[&session_ref].pane), pane_identity);

        let embedded = app.frame(150, 42);
        let embedded_text = embedded
            .iter()
            .map(|line| strip_sgr(line))
            .collect::<Vec<_>>()
            .join("\n");
        assert!(embedded_text.contains("TASK · EMBEDDED FOCUS"));
        assert!(embedded_text.contains("TEAM"));
        assert!(embedded_text.contains("▸ CLI INPUT"));
        assert!(embedded_text.contains("Ctrl+F  Full screen"));
        assert!(embedded_text.contains("Ctrl+G  Navigation"));
        assert!(!embedded_text.contains("Ctrl+F  Inline"));

        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('q'), KeyModifiers::NONE),
            150,
            42,
        ));
        assert!(matches!(app.mode, Mode::TaskCli));
        assert_eq!(app.focus, Focus::Cli);

        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('f'), KeyModifiers::CONTROL),
            150,
            42,
        ));
        assert!(matches!(app.mode, Mode::Attach));
        assert_eq!(app.focus, Focus::Cli);
        assert_eq!(app.panes.len(), 1);
        assert_eq!(Arc::as_ptr(&app.panes[&session_ref].pane), pane_identity);

        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            150,
            42,
        ));
        assert!(matches!(app.mode, Mode::Task));
        assert_eq!(app.focus, Focus::Nav);

        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('G'), KeyModifiers::SHIFT),
            150,
            42,
        ));
        assert!(matches!(app.mode, Mode::Home));
        assert_eq!(app.panes.len(), 1);
        assert_eq!(Arc::as_ptr(&app.panes[&session_ref].pane), pane_identity);
    }

    #[test]
    fn vanished_cli_restores_task_navigation_from_both_focused_views() {
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("workspace");
        std::fs::create_dir_all(&workspace).unwrap();
        let mut app = App::new_test(root.path().join("taskai-home")).unwrap();
        let task_ref = app
            .registry
            .create_task("Vanished CLI", &workspace, Source::HumanUi)
            .unwrap();
        app.registry
            .create_session(
                &task_ref,
                None,
                "Lead",
                "claude",
                &workspace,
                Source::HumanUi,
            )
            .unwrap();
        app.sel_task = Some(task_ref);

        for mode in [Mode::TaskCli, Mode::Attach] {
            app.mode = mode;
            app.focus = Focus::Cli;
            app.status = None;
            let frame = app.frame(150, 42);

            assert!(matches!(app.mode, Mode::Task));
            assert_eq!(app.focus, Focus::Nav);
            assert_eq!(
                app.status.as_deref(),
                Some("CLI viewer disconnected — Enter reconnects; navigation restored")
            );
            let text = frame
                .iter()
                .map(|line| strip_sgr(line))
                .collect::<Vec<_>>()
                .join("\n");
            assert!(text.contains("Ctrl+F  Full screen"));
            assert!(text.contains("G  Home"));
        }
    }

    #[test]
    fn new_task_modal_replaces_inactive_home_shortcuts() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("taskai-home")).unwrap();
        app.mode = Mode::NewTask(NewTaskForm {
            name: "Remote task".into(),
            repo: "~/github".into(),
            server_id: "local".into(),
            focus: 1,
            explorer: None,
        });

        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            let frame = app.frame(cols, rows);
            let expected = modal_keybar(vec![String::new(); rows], cols, NEW_TASK_KEYS);
            assert_eq!(&frame[rows - 3..rows - 1], &expected[rows - 3..rows - 1]);
            let keybar = frame[rows - 3..rows - 1]
                .iter()
                .map(|line| strip_sgr(line))
                .collect::<Vec<_>>()
                .join("\n");

            assert!(keybar.contains("Enter  Next field"));
            assert!(keybar.contains("F2  Browse"));
            assert!(keybar.contains("Ctrl+A  Select all"));
            assert!(keybar.contains("Ctrl+U  Clear"));
            assert!(keybar.contains("Tab  Next field"));
            assert!(keybar.contains("Esc  Cancel"));
            assert!(!keybar.contains("Open"));
            assert!(!keybar.contains("New task"));
            assert!(!keybar.contains("Harnesses"));
            assert!(!keybar.contains("Find"));
            assert!(!keybar.contains("Help"));
            assert!(!keybar.contains("Leave"));
            assert_eq!(
                frame
                    .iter()
                    .map(|line| strip_sgr(line))
                    .filter(|line| line.contains("Enter") && line.contains("Next field"))
                    .count(),
                1,
                "form controls should have one canonical location"
            );
        }
    }

    #[test]
    fn tab_never_browses_or_changes_selected_server() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        let remote = app
            .registry
            .create_server(
                "Remote",
                ConnectionTarget::ssh("cruise4b").unwrap(),
                Some("/home/ubuntu/github".into()),
                Source::HumanUi,
            )
            .unwrap();
        for server_id in ["local", remote.as_str()] {
            for (focus, forward, backward) in [(2, 1, 0), (1, 0, 2), (0, 2, 1)] {
                for (key, expected) in [
                    (KeyCode::Tab, forward),
                    (KeyCode::Down, forward),
                    (KeyCode::BackTab, backward),
                    (KeyCode::Up, backward),
                ] {
                    app.mode = Mode::NewTask(NewTaskForm {
                        name: "Draft".into(),
                        repo: "~/github/custom-repo".into(),
                        server_id: server_id.into(),
                        focus,
                        explorer: None,
                    });
                    app.handle(KeyEvent::new(key, KeyModifiers::NONE), 150, 42);
                    assert_eq!(field_identity(&app.mode), (1, expected, false));
                    assert!(app.status.is_none());
                    let Mode::NewTask(f) = &app.mode else {
                        unreachable!()
                    };
                    assert_eq!(f.server_id, server_id);
                    assert_eq!(f.repo, "~/github/custom-repo");
                    assert_eq!(f.name, "Draft");
                    assert!(app.registry.tasks().is_empty());
                }
            }
        }
    }

    #[test]
    fn local_default_is_visible_and_enter_explicitly_chooses_server() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.open_newtask();
        assert_eq!(field_identity(&app.mode), (1, 2, false));
        let frame = app.frame(150, 42);
        let (_, y, _, _) = *app
            .form_hits
            .iter()
            .find(|(field, _, _, _)| *field == 2)
            .unwrap();
        assert!(strip_sgr(&frame[y]).contains("This computer"));
        assert!(strip_sgr(&frame[40]).contains("Choose server"));
        app.handle_paste("ssh cruise4b");
        assert!(active_field(&mut app.mode).is_none());
        app.handle(
            KeyEvent::new(KeyCode::Char('u'), KeyModifiers::CONTROL),
            150,
            42,
        );
        let Mode::NewTask(f) = &app.mode else {
            unreachable!()
        };
        assert_eq!(f.server_id, "local");
        assert!(f.repo.is_empty());
        assert!(f.name.is_empty());
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::ChooseServer { selection: 0, .. }));
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert_eq!(field_identity(&app.mode), (1, 1, false));
        app.handle(KeyEvent::new(KeyCode::F(2), KeyModifiers::NONE), 150, 42);
        let Mode::NewTask(f) = &app.mode else {
            unreachable!()
        };
        assert_eq!(f.explorer.as_ref().unwrap().target, ConnectionTarget::Local);
    }

    #[test]
    fn name_enter_with_missing_repository_does_not_open_a_surprise_picker() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.mode = Mode::NewTask(NewTaskForm {
            name: "Project".into(),
            repo: String::new(),
            server_id: "local".into(),
            focus: 0,
            explorer: None,
        });
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert_eq!(field_identity(&app.mode), (1, 1, false));
        assert!(app
            .status
            .as_deref()
            .unwrap()
            .contains("Choose a repository"));
        assert_eq!(new_task_keys(0)[0], ("Enter", "Create task"));
        assert_eq!(new_task_keys(2)[0], ("Enter", "Choose server"));
        assert!(!new_task_keys(2)
            .iter()
            .any(|(key, _)| *key == "Ctrl+U" || *key == "Ctrl+A"));
    }

    #[test]
    fn repository_first_paste_click_clear_and_reverse_tab() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.open_newtask();
        assert_eq!(field_identity(&app.mode), (1, 2, false));
        app.handle(KeyEvent::new(KeyCode::Tab, KeyModifiers::NONE), 150, 42);
        assert_eq!(field_identity(&app.mode), (1, 1, false));
        app.handle_paste("  /home/ubuntu/github/   \r\n smb-deal-flow  \n");
        assert_eq!(
            active_field(&mut app.mode).unwrap(),
            "/home/ubuntu/github/smb-deal-flow"
        );
        app.handle_paste(" /home/another repo \n");
        assert_eq!(active_field(&mut app.mode).unwrap(), "/home/another repo");
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            app.frame(cols, rows);
            let (_, row, left, width) = *app
                .form_hits
                .iter()
                .find(|(field, _, _, _)| *field == 1)
                .unwrap();
            app.handle_mouse(
                MouseEvent {
                    kind: MouseEventKind::Down(MouseButton::Left),
                    column: (left + width - 9) as u16,
                    row: row as u16,
                    modifiers: KeyModifiers::NONE,
                },
                cols,
                rows,
            );
            assert!(active_field(&mut app.mode).unwrap().is_empty());
            app.handle_paste("/home/another repo");
        }
        app.handle(KeyEvent::new(KeyCode::Tab, KeyModifiers::NONE), 150, 42);
        assert_eq!(field_identity(&app.mode), (1, 0, false));
        app.handle(
            KeyEvent::new(KeyCode::BackTab, KeyModifiers::SHIFT),
            150,
            42,
        );
        assert_eq!(field_identity(&app.mode), (1, 1, false));
        app.handle(
            KeyEvent::new(KeyCode::Char('u'), KeyModifiers::CONTROL),
            150,
            42,
        );
        assert!(active_field(&mut app.mode).unwrap().is_empty());
    }

    #[test]
    fn local_picker_selects_repository_before_task_creation() {
        let root = tempfile::tempdir().unwrap();
        let repo = root.path().join("smb-deal-flow");
        std::fs::create_dir(&repo).unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.open_newtask();
        app.handle(KeyEvent::new(KeyCode::Tab, KeyModifiers::NONE), 150, 42);
        app.handle_paste(repo.to_str().unwrap());
        app.handle(KeyEvent::new(KeyCode::F(2), KeyModifiers::NONE), 150, 42);
        let Mode::NewTask(f) = &app.mode else {
            panic!("form closed");
        };
        assert_eq!(
            f.explorer.as_ref().unwrap().cwd,
            repo.canonicalize().unwrap()
        );
        app.handle(
            KeyEvent::new(KeyCode::Char(' '), KeyModifiers::NONE),
            150,
            42,
        );
        let Mode::NewTask(f) = &app.mode else {
            panic!("form closed");
        };
        assert_eq!(f.name, "smb deal flow");
        assert_eq!(f.focus, 0);
        assert!(app.registry.tasks().is_empty());
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert!(matches!(app.mode, Mode::Task));
        assert_eq!(app.registry.tasks().len(), 1);
        assert_eq!(
            app.registry.tasks()[0].server_profile_id.as_deref(),
            Some("local")
        );
        assert_eq!(app.registry.tasks()[0].connection, ConnectionTarget::Local);
    }

    #[test]
    fn cancelled_browse_cannot_replace_current_form() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.open_newtask();
        app.handle(KeyEvent::new(KeyCode::Tab, KeyModifiers::NONE), 150, 42);
        let (tx, rx) = channel();
        let Mode::NewTask(f) = &mut app.mode else {
            unreachable!()
        };
        f.explorer = Some(Explorer {
            cwd: "~".into(),
            entries: vec![],
            sel: 0,
            target: ConnectionTarget::ssh("cruise2").unwrap(),
            pending: Some(rx),
            error: None,
        });
        app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 150, 42);
        assert!(tx.send(Ok(("/home/ubuntu".into(), vec![]))).is_err());
        assert_eq!(field_identity(&app.mode), (1, 1, false));
    }

    #[test]
    fn new_task_server_picker_cancel_and_same_selection_preserve_draft() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.registry
            .create_server(
                "Remote",
                ConnectionTarget::ssh("cruise4b").unwrap(),
                Some("/home/ubuntu/github".into()),
                Source::HumanUi,
            )
            .unwrap();
        app.mode = Mode::NewTask(NewTaskForm {
            name: "Unsubmitted project".into(),
            repo: "/custom/local/repository".into(),
            server_id: "local".into(),
            focus: 2,
            explorer: None,
        });
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Down, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), 150, 42);
        let Mode::NewTask(f) = &app.mode else {
            panic!("draft not restored")
        };
        assert_eq!(f.server_id, "local");
        assert_eq!(f.repo, "/custom/local/repository");
        assert_eq!(f.name, "Unsubmitted project");
        assert_eq!(f.focus, 2);
        assert!(f.explorer.is_none());

        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        let Mode::NewTask(f) = &app.mode else {
            panic!("draft not restored")
        };
        assert_eq!(f.server_id, "local");
        assert_eq!(f.repo, "/custom/local/repository");
        assert_eq!(f.name, "Unsubmitted project");
        assert_eq!(f.focus, 1);
        assert!(app.registry.tasks().is_empty());
    }

    #[test]
    fn new_task_server_picker_defaults_are_isolated_and_archived_servers_hidden() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.registry
            .update_server(
                "local",
                "This computer",
                Some(root.path().to_path_buf()),
                Source::HumanUi,
            )
            .unwrap();
        let archived = app
            .registry
            .create_server(
                "Archived remote",
                ConnectionTarget::ssh("old-server").unwrap(),
                Some("/old/repositories".into()),
                Source::HumanUi,
            )
            .unwrap();
        app.registry
            .archive_server(&archived, Source::HumanUi)
            .unwrap();
        let remote = app
            .registry
            .create_server(
                "Project server",
                ConnectionTarget::ssh("cruise4b").unwrap(),
                Some("/home/ubuntu/github".into()),
                Source::HumanUi,
            )
            .unwrap();
        let without_root = app
            .registry
            .create_server(
                "Fresh server",
                ConnectionTarget::ssh("new-server").unwrap(),
                None,
                Source::HumanUi,
            )
            .unwrap();
        app.open_newtask();
        let Mode::NewTask(f) = &app.mode else {
            unreachable!()
        };
        assert_eq!(f.repo, root.path().display().to_string());
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        let text = app
            .frame(150, 42)
            .iter()
            .map(|line| strip_sgr(line))
            .collect::<Vec<_>>()
            .join("\n");
        assert!(text.contains("This computer"));
        assert!(text.contains("Project server"));
        assert!(!text.contains("Archived remote"));
        app.handle(KeyEvent::new(KeyCode::Down, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        let Mode::NewTask(f) = &mut app.mode else {
            unreachable!()
        };
        assert_eq!(f.server_id, remote);
        assert_eq!(f.repo, "/home/ubuntu/github");
        assert_eq!(f.focus, 1);
        f.name = "Preserved name".into();
        f.repo = "/home/ubuntu/github/custom".into();
        f.focus = 2;
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Down, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        let Mode::NewTask(f) = &mut app.mode else {
            unreachable!()
        };
        assert_eq!(f.server_id, without_root);
        assert!(
            f.repo.is_empty(),
            "a new server must not inherit another host's path"
        );
        assert_eq!(f.name, "Preserved name");
        f.focus = 2;
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Up, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Up, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        app.handle(KeyEvent::new(KeyCode::F(2), KeyModifiers::NONE), 150, 42);
        let Mode::NewTask(f) = &app.mode else {
            unreachable!()
        };
        assert_eq!(f.server_id, "local");
        let explorer = f.explorer.as_ref().unwrap();
        assert_eq!(explorer.target, ConnectionTarget::Local);
        assert_eq!(explorer.cwd, root.path().canonicalize().unwrap());
        assert!(app.registry.tasks().is_empty());
        assert_eq!(
            app.registry
                .server(&remote)
                .unwrap()
                .repository_root
                .as_deref(),
            Some(Path::new("/home/ubuntu/github"))
        );
    }

    #[test]
    fn new_task_single_server_arrows_preserve_typed_repository() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("state")).unwrap();
        app.mode = Mode::NewTask(NewTaskForm {
            name: "Draft".into(),
            repo: "/custom/local/repository".into(),
            server_id: "local".into(),
            focus: 2,
            explorer: None,
        });
        for key in [KeyCode::Left, KeyCode::Right] {
            app.handle(KeyEvent::new(key, KeyModifiers::NONE), 150, 42);
            let Mode::NewTask(f) = &app.mode else {
                unreachable!()
            };
            assert_eq!(f.server_id, "local");
            assert_eq!(f.repo, "/custom/local/repository");
            assert_eq!(f.name, "Draft");
        }
    }

    #[test]
    fn new_task_server_label_and_value_clicks_open_picker_at_every_size() {
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            let root = tempfile::tempdir().unwrap();
            let mut app = App::new_test(root.path().join("state")).unwrap();
            let remote = app
                .registry
                .create_server(
                    "QA server",
                    ConnectionTarget::ssh("qa-server").unwrap(),
                    Some("/repos".into()),
                    Source::HumanUi,
                )
                .unwrap();
            app.open_newtask();
            for label in [true, false] {
                app.frame(cols, rows);
                let (_, y, left, _) = *app
                    .form_hits
                    .iter()
                    .find(|(field, _, _, _)| *field == 2)
                    .unwrap();
                app.handle_mouse(
                    MouseEvent {
                        kind: MouseEventKind::Down(MouseButton::Left),
                        column: (left + 8) as u16,
                        row: (if label { y - 1 } else { y }) as u16,
                        modifiers: KeyModifiers::NONE,
                    },
                    cols,
                    rows,
                );
                assert!(matches!(app.mode, Mode::ChooseServer { selection: 0, .. }));
                let frame = app.frame(cols, rows);
                assert_eq!(frame.len(), rows);
                assert!(frame
                    .iter()
                    .all(|line| taskai_tui::seg::cell_count(line) == cols));
                assert!(strip_sgr(&frame[1]).contains("CHOOSE SERVER"));
                app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), cols, rows);
            }
            app.handle(
                KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
                cols,
                rows,
            );
            app.handle_mouse(
                MouseEvent {
                    kind: MouseEventKind::Down(MouseButton::Left),
                    column: 12,
                    row: 7,
                    modifiers: KeyModifiers::NONE,
                },
                cols,
                rows,
            );
            let Mode::NewTask(f) = &app.mode else {
                panic!("server click did not select")
            };
            assert_eq!(f.server_id, remote);
            assert_eq!(f.repo, "/repos");
            assert_eq!(f.focus, 1);
        }
    }

    #[test]
    fn modified_letters_never_leak_into_modal_text_fields() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("taskai-home")).unwrap();
        let forbidden = [
            KeyEvent::new(KeyCode::Char('f'), KeyModifiers::CONTROL),
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            KeyEvent::new(KeyCode::Char('x'), KeyModifiers::ALT),
            KeyEvent::new(KeyCode::Char('x'), KeyModifiers::SUPER),
            KeyEvent::new(KeyCode::Char('x'), KeyModifiers::HYPER),
            KeyEvent::new(KeyCode::Char('x'), KeyModifiers::META),
        ];

        app.mode = Mode::NewTask(NewTaskForm {
            name: String::new(),
            repo: String::new(),
            server_id: "local".into(),
            focus: 0,
            explorer: None,
        });
        for key in forbidden {
            assert!(app.handle(key, 150, 42));
        }
        let Mode::NewTask(form) = &app.mode else {
            panic!("new-task modal unexpectedly closed");
        };
        assert!(form.name.is_empty());

        assert!(app.handle(
            KeyEvent::new(KeyCode::Char('q'), KeyModifiers::NONE),
            150,
            42,
        ));
        let Mode::NewTask(form) = &app.mode else {
            panic!("new-task modal unexpectedly closed");
        };
        assert_eq!(form.name, "q");

        app.handle_paste("\x1b]52;c;payload\x07\nnext");
        let Mode::NewTask(form) = &app.mode else {
            panic!("new-task modal unexpectedly closed");
        };
        assert_eq!(form.name, "q]52;c;payload next");
        assert!(!form.name.chars().any(char::is_control));

        app.status = Some("\x1b[2Jbad\nstatus".into());
        let frame = app.frame(150, 42);
        let status = frame.last().unwrap();
        assert!(!status.contains("\x1b[2J"));
        assert!(strip_sgr(status).contains("[2Jbad status"));
    }

    #[test]
    fn harness_selector_arrows_and_clicks_cycle_all_choices_at_every_size() {
        for subordinate in [false, true] {
            for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
                let root = tempfile::tempdir().unwrap();
                let mut app = App::new_test(root.path().join("home")).unwrap();
                let task = app
                    .registry
                    .create_task("Selector", root.path(), Source::HumanUi)
                    .unwrap();
                let parent = app
                    .registry
                    .create_session(&task, None, "Lead", "claude", root.path(), Source::HumanUi)
                    .unwrap();
                app.sel_task = Some(task);
                let choices = HKEYS
                    .iter()
                    .map(|h| HarnessAvailability {
                        auth: crate::remote_probes::AuthState::Unknown,
                        hkey: (*h).into(),
                        version: Some(if *h == "grok" { "0.2.117" } else { "2.1.0" }.into()),
                        unavailable: (*h == "grok")
                            .then(|| "Installed 0.2.117; requires 1.0.0 or newer".into()),
                    })
                    .collect::<Vec<_>>();
                app.mode = if subordinate {
                    Mode::Sub(SubForm {
                        parent,
                        role: "Keep this draft".into(),
                        choices,
                        hsel: 0,
                        ws_mode: 1,
                        branch: "main".into(),
                        wname: "new-agent".into(),
                        other: String::new(),
                        focus: 0,
                        explorer: None,
                    })
                } else {
                    Mode::Manager(ManagerForm {
                        name: "Keep this draft".into(),
                        choices,
                        hsel: 0,
                        focus: 0,
                    })
                };
                let frame = app.frame(cols, rows);
                assert_eq!(frame.len(), rows);
                assert!(frame
                    .iter()
                    .all(|line| taskai_tui::seg::cell_count(line) == cols));
                let (_, y, left, _) = *app.form_hits.iter().find(|(f, _, _, _)| *f == 1).unwrap();
                assert!(strip_sgr(&frame[y]).contains("Claude Code"));
                assert!(frame.iter().any(|line| strip_sgr(line).contains("F5")));
                let click = |x: usize, y: usize| MouseEvent {
                    kind: MouseEventKind::Down(MouseButton::Left),
                    column: x as u16,
                    row: y as u16,
                    modifiers: KeyModifiers::NONE,
                };
                // The label focuses, without secretly cycling or submitting.
                app.handle_mouse(click(left + 8, y - 1), cols, rows);
                assert_eq!(field_identity(&app.mode).1, 1);
                for name in ["Codex", "OpenCode", "Grok CLI", "Claude Code"] {
                    app.handle(
                        KeyEvent::new(KeyCode::Right, KeyModifiers::NONE),
                        cols,
                        rows,
                    );
                    assert!(strip_sgr(&app.frame(cols, rows)[y]).contains(&format!("‹ {name} ›")));
                }
                app.handle_mouse(click(left + 6 + "Claude Code".len(), y), cols, rows);
                assert!(strip_sgr(&app.frame(cols, rows)[y]).contains("‹ Codex ›"));
                app.handle_mouse(click(left + 3, y), cols, rows);
                assert!(strip_sgr(&app.frame(cols, rows)[y]).contains("‹ Claude Code ›"));
                app.handle(KeyEvent::new(KeyCode::Left, KeyModifiers::NONE), cols, rows);
                let frame = app
                    .frame(cols, rows)
                    .iter()
                    .map(|line| strip_sgr(line))
                    .collect::<Vec<_>>()
                    .join("\n");
                assert!(frame.contains("UNAVAILABLE"));
                assert!(frame.contains("requires 1.0.0"));
                assert!(!frame
                    .lines()
                    .find(|line| line.contains("‹ Grok CLI ›"))
                    .unwrap()
                    .contains('▍'));
                app.handle(
                    KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
                    cols,
                    rows,
                );
                assert!(app
                    .status
                    .as_deref()
                    .unwrap()
                    .contains("Grok CLI unavailable"));
                assert!(app.pending_launches.is_empty());
                assert_eq!(
                    app.registry
                        .sessions_of(app.sel_task.as_ref().unwrap())
                        .len(),
                    1
                );
                assert!(!root.path().join(".worktrees/new-agent").exists());
                app.handle(KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE), cols, rows);
                assert!(matches!(app.mode, Mode::Task));
            }
        }
    }

    #[test]
    fn harness_refresh_preserves_draft_selection_and_blocks_newly_unavailable_choice() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        let task = app
            .registry
            .create_task("Refresh", root.path(), Source::HumanUi)
            .unwrap();
        app.sel_task = Some(task.clone());
        // No harness is ready: still open a form with visible explanations.
        app.mode = Mode::Task;
        app.handle(
            KeyEvent::new(KeyCode::Char('m'), KeyModifiers::NONE),
            150,
            42,
        );
        let Mode::Manager(f) = &app.mode else {
            panic!("missing unavailable choices")
        };
        assert_eq!(f.choices.len(), 4);
        assert!(f.choices.iter().all(|c| !c.is_ready()));
        if let Mode::Manager(f) = &mut app.mode {
            f.name = "Saved draft".into();
            f.hsel = 1;
        }
        // A repeated F5 while local discovery is incomplete reuses the worker.
        app.handle(KeyEvent::new(KeyCode::F(5), KeyModifiers::NONE), 150, 42);
        app.probes.insert(
            "codex".into(),
            Capability {
                mode: HarnessMode::SharedLive,
                version: Some("0.153.4".into()),
                detail: "ready".into(),
            },
        );
        app.sync_form_harnesses();
        let Mode::Manager(f) = &app.mode else {
            unreachable!()
        };
        assert_eq!(f.name, "Saved draft");
        assert_eq!(require_harness(&f.choices, f.hsel).unwrap(), "codex");
        app.probes.insert(
            "codex".into(),
            Capability {
                mode: HarnessMode::Unsupported,
                version: None,
                detail: "executable missing".into(),
            },
        );
        app.sync_form_harnesses();
        let Mode::Manager(f) = &app.mode else {
            unreachable!()
        };
        assert_eq!(f.name, "Saved draft");
        assert_eq!(f.choices[f.hsel].hkey, "codex");
        app.handle(KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE), 150, 42);
        assert!(app
            .status
            .as_deref()
            .unwrap()
            .contains("executable missing"));
        assert!(app.registry.sessions_of(&task).is_empty());
    }

    #[test]
    fn review_compact_viewport_ignores_clicks_and_mutations_but_allows_safe_exit() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        let task = app
            .registry
            .create_task("Compact safety", root.path(), Source::HumanUi)
            .unwrap();
        let session = app
            .registry
            .create_session(&task, None, "Lead", "claude", root.path(), Source::HumanUi)
            .unwrap();
        let pane = Pane::spawn("/bin/cat", &[], root.path(), 31, 114, &[]).unwrap();
        app.panes.insert(
            session.clone(),
            PaneEntry {
                pane: Arc::new(Mutex::new(pane)),
                runtime: TmuxRuntimeLocator {
                    target: ConnectionTarget::Local,
                    private_server: "taskai-review-compact".into(),
                    session,
                },
            },
        );
        app.sel_task = Some(task.clone());
        app.mode = Mode::Task;
        app.frame(150, 42);
        for kind in [
            MouseEventKind::Down(MouseButton::Left),
            MouseEventKind::ScrollDown,
        ] {
            app.handle_mouse(
                MouseEvent {
                    kind,
                    column: 10,
                    row: 8,
                    modifiers: KeyModifiers::NONE,
                },
                20,
                10,
            );
        }
        for code in [
            KeyCode::Enter,
            KeyCode::Char('m'),
            KeyCode::Char('a'),
            KeyCode::Char('D'),
        ] {
            assert!(app.handle(KeyEvent::new(code, KeyModifiers::NONE), 20, 10));
        }
        assert!(matches!(app.mode, Mode::Task));
        assert_eq!(app.st.rail_sel, 0);
        assert_eq!(app.registry.sessions_of(&task).len(), 1);
        assert!(app.pending_launches.is_empty());
        app.mode = Mode::TaskCli;
        app.focus = Focus::Cli;
        app.handle(
            KeyEvent::new(KeyCode::Char('g'), KeyModifiers::CONTROL),
            20,
            10,
        );
        assert_eq!(app.focus, Focus::Nav);
        assert!(!app.handle(
            KeyEvent::new(KeyCode::Char('q'), KeyModifiers::NONE),
            20,
            10
        ));
    }

    #[test]
    fn review_fullscreen_long_unicode_name_keeps_both_escape_routes_visible() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        let task = app
            .registry
            .create_task("Long name", root.path(), Source::HumanUi)
            .unwrap();
        app.registry
            .create_session(
                &task,
                None,
                &"東京 Manager with a long assignment ".repeat(30),
                "claude",
                root.path(),
                Source::HumanUi,
            )
            .unwrap();
        app.sel_task = Some(task);
        app.mode = Mode::Attach;
        app.focus = Focus::Cli;
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            let frame = app.attach_frame(cols, rows);
            let ribbon = strip_sgr(&frame[1]);
            assert!(
                ribbon.contains("Ctrl+F  Inline"),
                "inline escape clipped at {cols}: {ribbon}"
            );
            assert!(
                ribbon.contains("Ctrl+G  Navigation"),
                "navigation escape clipped at {cols}: {ribbon}"
            );
            assert!(frame
                .iter()
                .all(|line| taskai_tui::seg::cell_count(line) == cols));
            assert_eq!(frame.len(), rows);
        }
    }

    #[test]
    fn review_pending_launch_blocks_exact_destroy_scope_without_touching_peer_task() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        let task = app
            .registry
            .create_task("Opening", root.path(), Source::HumanUi)
            .unwrap();
        let manager = app
            .registry
            .create_session(&task, None, "Lead", "claude", root.path(), Source::HumanUi)
            .unwrap();
        let child = app
            .registry
            .create_session(
                &task,
                Some(&manager),
                "Child",
                "codex",
                root.path(),
                Source::HumanUi,
            )
            .unwrap();
        let peer = app
            .registry
            .create_task("Unrelated", root.path(), Source::HumanUi)
            .unwrap();
        let (_sender, receiver) = channel();
        app.pending_launches.insert(
            child.clone(),
            PendingLaunch {
                task_ref: task.clone(),
                result: receiver,
                input: Vec::new(),
            },
        );
        for item in [
            Item::Task(task.clone()),
            Item::Session(manager.clone()),
            Item::Session(child.clone()),
        ] {
            let error = app.destroy_item(&item).unwrap_err();
            assert!(error.contains("still opening"));
            assert!(app.registry.task(&task).is_some());
            assert_eq!(app.registry.sessions_of(&task).len(), 2);
        }
        app.destroy_item(&Item::Task(peer.clone())).unwrap();
        assert!(app.registry.task(&peer).is_none());
        assert!(app.pending_launches.contains_key(&child));
        app.pending_launches.clear();
        let (_utility_sender, utility_receiver) = channel();
        app.pending_launches.insert(
            terminal_ref(&task),
            PendingLaunch {
                task_ref: task.clone(),
                result: utility_receiver,
                input: Vec::new(),
            },
        );
        assert!(app.destroy_item(&Item::Task(task.clone())).is_err());
        app.destroy_item(&Item::Session(manager.clone())).unwrap();
        assert!(app.registry.session(&manager).is_none());
        assert!(app.registry.task(&task).is_some());
        assert!(app.pending_launches.contains_key(&terminal_ref(&task)));
    }

    #[test]
    fn review_search_clear_click_restores_results_without_opening_a_task() {
        let root = tempfile::tempdir().unwrap();
        let mut app = App::new_test(root.path().join("home")).unwrap();
        for name in ["Alpha", "Beta", "Gamma"] {
            app.registry
                .create_task(name, root.path(), Source::HumanUi)
                .unwrap();
        }
        app.mode = Mode::Home;
        app.handle(
            KeyEvent::new(KeyCode::Char('/'), KeyModifiers::NONE),
            150,
            42,
        );
        app.handle_paste("not a matching task");
        assert!(app.home_rows().is_empty());
        app.frame(150, 42);
        app.handle_mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: 141,
                row: 3,
                modifiers: KeyModifiers::NONE,
            },
            150,
            42,
        );
        assert!(matches!(&app.mode, Mode::Find(query) if query.is_empty()));
        assert_eq!(app.home_rows().len(), 3);
        assert_eq!(app.st.home_sel, 0);
        assert!(app.sel_task.is_none());
        assert!(app.pending_launches.is_empty());
    }
}
