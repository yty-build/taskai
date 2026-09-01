// ABOUTME: The live TaskAI application — registry-driven screens, session panes per
// ABOUTME: harness connector, drawers that create real tasks and sessions, focus routing.

use std::collections::HashMap;
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::mpsc::{channel, Receiver, Sender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use crossterm::terminal;
use taskai_connectors::claude::{install_context_hook, install_hooks, ClaudeConnector};
use taskai_connectors::codex::CodexConnector;
use taskai_connectors::events::wait_for_event;
use taskai_connectors::grok::GrokConnector;
use taskai_connectors::opencode::{OpenCodeConnector, Server as OcServer};
use taskai_connectors::{Capability, Conduit, Mode as HarnessMode};
use taskai_core::registry::{Item, ProviderMapping, Registry, Role, SessionState, Source};
use taskai_core::workspace::{preview, provision, WorkspaceChoice};
use taskai_pane::Pane;
use taskai_tui::color::{bg, CLI_BG, DIM, FT, GLASS_IN, ION, KCAP, RESET, TEXT, VOID};
use taskai_tui::input::{route, Focus, Route};
use taskai_tui::overlay::{
    destroy_panel_view, drawer_panel_view, explorer_panel, help_panel, overlay, DrawerView,
};
use taskai_tui::screens::{boot, harness, home, task};
use taskai_tui::seg::{row, s, seglen, vrow, Seg};
use taskai_tui::state::{advance, Screen as StScreen, St, View};
use taskai_tui::view::{HarnessRowView, HomeTaskRow, Hud, PaneBody, ProbeRow, RailRow, TaskView};

use crate::control;

const HKEYS: [&str; 4] = ["claude", "codex", "opencode", "grok"];

fn harness_title(hkey: &str) -> &'static str {
    match hkey {
        "claude" => "Claude Code",
        "codex" => "Codex",
        "opencode" => "OpenCode",
        "grok" => "Grok CLI",
        _ => "CLI",
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

struct Explorer {
    cwd: PathBuf,
    entries: Vec<(String, bool)>,
    sel: usize,
}

impl Explorer {
    fn open(cwd: &Path) -> Explorer {
        let mut e = Explorer { cwd: cwd.to_path_buf(), entries: Vec::new(), sel: 0 };
        e.refresh();
        e
    }

    fn refresh(&mut self) {
        let mut dirs: Vec<(String, bool)> = Vec::new();
        if let Ok(rd) = std::fs::read_dir(&self.cwd) {
            for entry in rd.flatten() {
                let name = entry.file_name().to_string_lossy().to_string();
                if name.starts_with('.') {
                    continue;
                }
                if entry.path().is_dir() {
                    dirs.push((name, true));
                }
            }
        }
        dirs.sort();
        self.entries = vec![("..".to_string(), true)];
        self.entries.extend(dirs);
        self.sel = 0;
    }
}

struct NewTaskForm {
    name: String,
    repo: String,
    focus: usize,
    explorer: Option<Explorer>,
}

struct ManagerForm {
    name: String,
    ready: Vec<String>,
    hsel: usize,
    focus: usize,
}

struct SubForm {
    parent: String,
    role: String,
    ready: Vec<String>,
    hsel: usize,
    ws_mode: usize, // 0 same repo · 1 new worktree · 2 another repository
    branch: String,
    wname: String,
    other: String,
    focus: usize,
    explorer: Option<Explorer>,
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
    spawned: std::time::Instant,
    resumed: bool,
}

#[derive(Clone, Copy, PartialEq)]
enum PrevMode {
    FromHome,
    FromTask,
}

enum Mode {
    Boot,
    Home,
    Harnesses,
    Task,
    Attach,
    Help(PrevMode),
    NewTask(NewTaskForm),
    Manager(ManagerForm),
    Sub(SubForm),
    Destroy(DestroyForm),
}

pub struct App {
    st: St,
    mode: Mode,
    focus: Focus,
    home_dir: PathBuf,
    registry: Registry,
    probes: HashMap<String, Capability>,
    probe_rx: Receiver<(String, Capability)>,
    bind_rx: Receiver<(String, String)>,
    bind_tx: Sender<(String, String)>,
    panes: HashMap<String, PaneEntry>,
    oc_servers: HashMap<PathBuf, OcServer>,
    served: control::Shared,
    sel_task: Option<String>,
    status: Option<String>,
}

impl App {
    pub fn new(home_dir: PathBuf) -> io::Result<App> {
        let mut registry = Registry::open(&home_dir.join("state")).map_err(io::Error::other)?;
        // A fresh runtime has no panes: anything recorded live last run is now resumable,
        // not live — truthful state over stale claims.
        let stale: Vec<String> = registry
            .tasks()
            .iter()
            .flat_map(|t| registry.sessions_of(&t.task_ref))
            .filter(|se| {
                matches!(
                    se.state,
                    SessionState::Live | SessionState::Working | SessionState::Attention
                )
            })
            .map(|se| se.session_ref.clone())
            .collect();
        for sref in stale {
            let _ = registry.set_state(&sref, SessionState::Queued);
        }
        let run = home_dir.join("run");
        std::fs::create_dir_all(&run)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&run, std::fs::Permissions::from_mode(0o700))?;
        }
        let (ptx, probe_rx) = channel();
        std::thread::spawn(move || {
            let _ = ptx.send(("claude".to_string(), ClaudeConnector::default().probe()));
            let _ = ptx.send(("codex".to_string(), CodexConnector::default().probe()));
            let _ = ptx.send(("opencode".to_string(), OpenCodeConnector::default().probe()));
            let _ = ptx.send(("grok".to_string(), GrokConnector::default().probe()));
        });
        let (bind_tx, bind_rx) = channel();
        let served = control::Shared {
            panes: Arc::new(Mutex::new(HashMap::new())),
            session_ids: Arc::new(Mutex::new(HashMap::new())),
            events: run.join("claude-events.jsonl"),
            conduit: Arc::new(Conduit::new()),
        };
        control::serve(control::sock_path(&home_dir), served.clone())?;
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
            probe_rx,
            bind_rx,
            bind_tx,
            panes: HashMap::new(),
            oc_servers: HashMap::new(),
            served,
            sel_task: None,
            status: None,
        })
    }

    pub fn run(&mut self, attach: Option<PathBuf>) -> io::Result<()> {
        if let Some(dir) = attach {
            let dir = dir.canonicalize()?;
            let name =
                dir.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or("task".into());
            let t = self
                .registry
                .create_task(&name, &dir, Source::HumanUi)
                .map_err(io::Error::other)?;
            let sref = self
                .registry
                .create_session(&t, None, "Lead", "claude", &dir, Source::HumanUi)
                .map_err(io::Error::other)?;
            self.sel_task = Some(t);
            self.mode = Mode::Task;
            self.status = Some(format!("Lead registered as {sref} — Enter attaches"));
        }
        let mut out = io::stdout();
        terminal::enable_raw_mode()?;
        write!(out, "\x1b[?1049h\x1b[?25l\x1b[2J")?;
        out.flush()?;
        let result = self.event_loop(&mut out);
        write!(out, "\x1b[?25h\x1b[?1049l\x1b[0m")?;
        out.flush()?;
        terminal::disable_raw_mode()?;
        result
    }

    fn event_loop(&mut self, out: &mut impl Write) -> io::Result<()> {
        loop {
            advance(&mut self.st);
            while let Ok((hkey, cap)) = self.probe_rx.try_recv() {
                self.probes.insert(hkey, cap);
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
                        runtime_id: format!("pid:{}", std::process::id()),
                        connector_version: version,
                    },
                );
                let _ = self.registry.set_state(&sref, SessionState::Live);
                self.served.session_ids.lock().unwrap().insert(sref, sid);
            }
            let dead: Vec<(String, bool, bool)> = self
                .panes
                .iter()
                .map(|(sref, e)| {
                    let alive = e.pane.lock().unwrap().is_alive();
                    (sref.clone(), alive, e.resumed && e.spawned.elapsed() < Duration::from_secs(15))
                })
                .filter(|(_, alive, _)| !alive)
                .map(|(sref, _, early_resume)| (sref, false, early_resume))
                .collect();
            for (sref, _, early_resume) in dead {
                self.panes.remove(&sref);
                self.served.panes.lock().unwrap().remove(&sref);
                if early_resume {
                    let _ = self.registry.clear_provider(&sref);
                    self.status =
                        Some(format!("{sref}: native session was not resumable — Enter starts fresh"));
                }
                if let Some(se) = self.registry.session(&sref) {
                    if se.state != SessionState::Complete {
                        let _ = self.registry.set_state(&sref, SessionState::Queued);
                    }
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
            let (c, r) = terminal::size().unwrap_or((120, 40));
            let (cols, rows) = ((c as usize).max(110), (r as usize).max(30));
            let frame = self.frame(cols, rows);
            write!(out, "\x1b[?2026h\x1b[H{}\x1b[?2026l", frame.join("\r\n"))?;
            out.flush()?;

            let mut waited = false;
            while event::poll(if waited { Duration::ZERO } else { Duration::from_millis(100) })? {
                waited = true;
                if let Event::Key(k) = event::read()? {
                    if k.kind == KeyEventKind::Release {
                        continue;
                    }
                    if !self.handle(k, cols, rows) {
                        return Ok(());
                    }
                }
            }
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
                (format!("{} LIVE", c.live), if c.live > 0 { ION } else { DIM }),
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
            .enumerate()
            .map(|(i, t)| {
                let status = self.registry.task_status(&t.task_ref).to_string();
                let sessions = self.registry.sessions_of(&t.task_ref);
                let managers = sessions
                    .iter()
                    .filter(|se| se.role == Role::Manager)
                    .map(|se| {
                        (se.name.clone(), harness_title(&se.harness).to_string(), se.harness.clone())
                    })
                    .collect();
                let spark = if status == "WORKING" {
                    home::spark_str(if i % 2 == 0 { &self.st.spark_t1 } else { &self.st.spark_t2 })
                } else {
                    "▁".repeat(12)
                };
                let repo_name = t
                    .repo
                    .file_name()
                    .map(|n| n.to_string_lossy().to_string())
                    .unwrap_or_default();
                HomeTaskRow {
                    name: t.name.clone(),
                    status: status.clone(),
                    managers,
                    spark,
                    spark_live: status == "WORKING",
                    meta: format!(
                        "{} session{} · {repo_name}",
                        sessions.len(),
                        if sessions.len() == 1 { "" } else { "s" }
                    ),
                }
            })
            .collect()
    }

    fn harness_rows(&self) -> (Vec<HarnessRowView>, String) {
        let mut rows = Vec::new();
        let mut ready = 0;
        for hkey in HKEYS {
            let cap = self.probes.get(hkey);
            let setup = cap.map(|c| c.mode == HarnessMode::Unsupported).unwrap_or(true);
            if !setup {
                ready += 1;
            }
            let live = self
                .registry
                .tasks()
                .iter()
                .flat_map(|t| self.registry.sessions_of(&t.task_ref))
                .filter(|se| se.harness == hkey && se.state == SessionState::Live)
                .count();
            rows.push(HarnessRowView {
                name: harness_title(hkey).into(),
                hkey: hkey.into(),
                bin: match cap {
                    Some(c) if c.mode != HarnessMode::Unsupported => match &c.version {
                        Some(v) => format!("{hkey} · v{v}"),
                        None => hkey.to_string(),
                    },
                    _ => "not found on PATH".into(),
                },
                sessions: if live > 0 { format!("{live} live") } else { "—".into() },
                conduit: cap
                    .filter(|c| c.mode != HarnessMode::Unsupported)
                    .map(|c| c.mode.label().to_string())
                    .unwrap_or("—".into()),
                setup,
            });
        }
        let c = self.registry.hud_counts();
        let summary = format!("{ready} of {} ready · {} live · {} queued", HKEYS.len(), c.live, c.queued);
        (rows, summary)
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
        ordered
            .into_iter()
            .map(|se| {
                let mode = self
                    .probes
                    .get(&se.harness)
                    .map(|c| c.mode)
                    .unwrap_or(HarnessMode::Unsupported);
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
            .collect()
    }

    fn task_view(&mut self, cols: usize, rows: usize) -> Option<TaskView> {
        let task_ref = self.sel_task.clone()?;
        let t = self.registry.task(&task_ref)?.clone();
        let sessions = self.rail_rows(&task_ref);
        let harnesses: std::collections::HashSet<&str> =
            sessions.iter().map(|se| se.hkey.as_str()).collect();
        let team_line = format!(
            "{} session{} · {} harness{}",
            sessions.len(),
            if sessions.len() == 1 { "" } else { "es" },
            harnesses.len(),
            if harnesses.len() == 1 { "" } else { "es" }
        );
        let body = if sessions.is_empty() {
            PaneBody::Empty("Add the first Manager to begin —  m".into())
        } else {
            let sel = self.st.rail_sel.min(sessions.len() - 1);
            let sref = sessions[sel].key.clone();
            if let Some(entry) = self.panes.get(&sref) {
                let pane = &entry.pane;
                let h = rows.saturating_sub(8);
                let pw = cols - task::RAIL_W - task::GAP;
                let (body_h, body_w) = (h.saturating_sub(3), pw);
                let mut p = pane.lock().unwrap();
                if p.size() != (body_h, body_w) {
                    let _ = p.resize(body_h as u16, body_w as u16);
                }
                PaneBody::Cells(
                    p.cells()
                        .into_iter()
                        .map(|r| {
                            r.into_iter()
                                .map(|c| taskai_tui::screens::pane::PaneCell {
                                    text: c.text,
                                    fg: c.fg,
                                    bg: c.bg,
                                    bold: c.bold,
                                })
                                .collect()
                        })
                        .collect(),
                )
            } else if sessions[sel].hkey == "grok" {
                PaneBody::Handoff {
                    note: "HANDOFF-RESUME · Grok CLI does not stream. Prompts queue; results return on completion.".into(),
                    exchanges: Vec::new(),
                    cmd: format!("taskai session prompt {sref} --stdin --wait"),
                }
            } else {
                PaneBody::Empty(format!(
                    "Enter launches {} here",
                    sessions[sel].harness
                ))
            }
        };
        Some(TaskView {
            crumb: format!("TASK · {}", t.name.to_uppercase()),
            title: t.name.clone(),
            repo: tilde(&t.repo),
            team_line,
            sessions,
            body,
        })
    }

    // ---- frame -----------------------------------------------------------

    fn frame(&mut self, cols: usize, rows: usize) -> Vec<String> {
        let clock = self.clock();
        let mut frame = match &self.mode {
            Mode::Boot => {
                let done = self.probes.len();
                let ready =
                    self.probes.values().filter(|c| c.mode != HarnessMode::Unsupported).count();
                let meter = format!("{ready} of {done} harnesses ready");
                boot::frame(&self.probe_rows(), &meter, self.st.boot_k, cols, rows)
            }
            Mode::Home => {
                home::frame(&self.home_rows(), &self.hud(), self.st.home_sel, cols, rows, &clock)
            }
            Mode::Harnesses => {
                let (hrows, summary) = self.harness_rows();
                harness::frame(&hrows, &summary, &self.hud(), cols, rows, &clock)
            }
            Mode::Task => match self.task_view(cols, rows) {
                Some(view) => task::frame(&view, &self.hud(), &self.st, cols, rows, &clock),
                None => home::frame(&self.home_rows(), &self.hud(), 0, cols, rows, &clock),
            },
            Mode::Attach => self.attach_frame(cols, rows),
            Mode::Help(prev) => {
                let prev = *prev;
                let base = self.base_frame(prev, cols, rows, &clock);
                overlay(&base, &help_panel(), cols, 6)
            }
            Mode::NewTask(f) => {
                if let Some(ex) = &f.explorer {
                    let panel =
                        explorer_panel(&tilde(&ex.cwd), &ex.entries, ex.sel);
                    let base = home::frame(
                        &self.home_rows(),
                        &self.hud(),
                        self.st.home_sel,
                        cols,
                        rows,
                        &clock,
                    );
                    overlay(&base, &panel, cols, 4)
                } else {
                    let d = DrawerView {
                        title: "New task".into(),
                        fields: vec![
                            ("Name".into(), f.name.clone(), f.focus == 0),
                            ("Repository".into(), f.repo.clone(), f.focus == 1),
                        ],
                        keys: vec![
                            ("Enter", "Create / Browse"),
                            ("Tab", "Next field"),
                            ("Esc", "Cancel"),
                        ],
                    };
                    let note = vec![vec![s(
                        "Manager sessions start in the repository root.",
                        Some(FT),
                        Some(taskai_tui::color::GLASS),
                    )]];
                    let base = home::frame(
                        &self.home_rows(),
                        &self.hud(),
                        self.st.home_sel,
                        cols,
                        rows,
                        &clock,
                    );
                    overlay(&base, &drawer_panel_view(self.st.blink, &d, &note, 58), cols, 6)
                }
            }
            Mode::Manager(f) => {
                let hlabel = f
                    .ready
                    .get(f.hsel)
                    .map(|h| {
                        format!(
                            "‹ {} ›   {}",
                            harness_title(h),
                            self.probes.get(h).map(|c| c.mode.label()).unwrap_or("")
                        )
                    })
                    .unwrap_or("no ready harnesses".into());
                let d = DrawerView {
                    title: "Add Manager".into(),
                    fields: vec![
                        ("Name".into(), f.name.clone(), f.focus == 0),
                        ("Harness".into(), hlabel, f.focus == 1),
                    ],
                    keys: vec![
                        ("Enter", "Create"),
                        ("Tab", "Next field"),
                        ("←/→", "Choose"),
                        ("Esc", "Cancel"),
                    ],
                };
                let base = self.base_frame(PrevMode::FromTask, cols, rows, &clock);
                overlay(&base, &drawer_panel_view(self.st.blink, &d, &[], 58), cols, 6)
            }
            Mode::Sub(f) => {
                let parent_name = self
                    .registry
                    .session(&f.parent)
                    .map(|p| format!("{} · {}", p.name, harness_title(&p.harness)))
                    .unwrap_or_default();
                let hlabel = f
                    .ready
                    .get(f.hsel)
                    .map(|h| format!("‹ {} ›", harness_title(h)))
                    .unwrap_or("no ready harnesses".into());
                let ws_label = match f.ws_mode {
                    0 => "‹ Same repository as Manager ›".to_string(),
                    1 => "‹ New git worktree ›".to_string(),
                    _ => format!("‹ Another repository › {}", f.other),
                };
                let mut fields = vec![
                    ("Parent".into(), parent_name, false),
                    ("Role".into(), f.role.clone(), f.focus == 0),
                    ("Harness".into(), hlabel, f.focus == 1),
                    ("Workspace".into(), ws_label, f.focus == 2),
                ];
                if f.ws_mode == 1 {
                    fields.push(("Base".into(), f.branch.clone(), f.focus == 3));
                    fields.push(("Worktree".into(), f.wname.clone(), f.focus == 4));
                }
                let d = DrawerView {
                    title: "Add Subordinate".into(),
                    fields,
                    keys: vec![
                        ("Enter", "Create"),
                        ("Tab", "Next field"),
                        ("←/→", "Choose"),
                        ("Esc", "Cancel"),
                    ],
                };
                let note = if f.ws_mode == 1 {
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
                    vec![
                        vec![s("CODE WILL LIVE AT", Some(FT), Some(taskai_tui::color::GLASS))],
                        vec![s(
                            tilde(&preview(&choice, &repo)),
                            Some(DIM),
                            Some(taskai_tui::color::GLASS),
                        )],
                        vec![s(
                            "Repo and history are preserved on destroy.",
                            Some(FT),
                            Some(taskai_tui::color::GLASS),
                        )],
                    ]
                } else {
                    Vec::new()
                };
                if let Some(ex) = &f.explorer {
                    let panel = explorer_panel(&tilde(&ex.cwd), &ex.entries, ex.sel);
                    let base = self.base_frame(PrevMode::FromTask, cols, rows, &clock);
                    overlay(&base, &panel, cols, 4)
                } else {
                    let base = self.base_frame(PrevMode::FromTask, cols, rows, &clock);
                    overlay(&base, &drawer_panel_view(self.st.blink, &d, &note, 58), cols, 5)
                }
            }
            Mode::Destroy(f) => {
                let kept = ["Repository untouched", "Worktrees preserved", "Git history preserved"];
                let panel = destroy_panel_view(
                    self.st.blink,
                    &f.title,
                    &f.blast_lines,
                    &kept,
                    &f.expect,
                    &f.typed,
                );
                let prev = if f.back_home { PrevMode::FromHome } else { PrevMode::FromTask };
                let base = self.base_frame(prev, cols, rows, &clock);
                overlay(&base, &panel, cols, 6)
            }
        };
        if let Some(msg) = &self.status {
            if let Some(last) = frame.last_mut() {
                let clipped: String = msg.chars().take(cols.saturating_sub(4)).collect();
                *last = row(&[s(clipped, Some(FT), Some(VOID))], cols, VOID, 2);
            }
        }
        frame
    }

    fn base_frame(&mut self, prev: PrevMode, cols: usize, rows: usize, clock: &str) -> Vec<String> {
        match prev {
            PrevMode::FromHome => {
                home::frame(&self.home_rows(), &self.hud(), self.st.home_sel, cols, rows, clock)
            }
            PrevMode::FromTask => match self.task_view(cols, rows) {
                Some(view) => task::frame(&view, &self.hud(), &self.st, cols, rows, clock),
                None => home::frame(&self.home_rows(), &self.hud(), 0, cols, rows, clock),
            },
        }
    }

    fn attach_frame(&mut self, cols: usize, rows: usize) -> Vec<String> {
        let Some((sref, sess)) = self.selected_session() else {
            self.mode = Mode::Task;
            return vec![vrow(cols); rows];
        };
        let b = GLASS_IN;
        let stripe = taskai_tui::color::harness_color(&sess.harness);
        let mode_label = self
            .probes
            .get(&sess.harness)
            .map(|c| c.mode.label())
            .unwrap_or("unsupported");
        let mut rib: Vec<Seg> = vec![
            s("▌", Some(stripe), Some(b)),
            s("  ", None, Some(b)),
            s(
                format!("{} · {} · {sref}", sess.name, harness_title(&sess.harness)),
                Some(DIM),
                Some(b),
            ),
        ];
        let mid = vec![s(mode_label, Some(FT), Some(b))];
        let tail = vec![
            s(" Ctrl+G ", Some(TEXT), Some(KCAP)),
            s(" Return to TaskAI", Some(FT), Some(b)),
        ];
        let gap1 = (cols as isize * 2 / 3 - seglen(&rib) as isize - seglen(&mid) as isize).max(0)
            as usize;
        rib.push(s(" ".repeat(gap1), None, Some(b)));
        rib.extend(mid);
        let gap2 = (cols as isize - 3 - seglen(&rib) as isize - seglen(&tail) as isize).max(0)
            as usize;
        rib.push(s(" ".repeat(gap2), None, Some(b)));
        rib.extend(tail);
        let mut lines = vec![
            row(&[s("▄".repeat(cols), Some(b), Some(VOID))], cols, VOID, 0),
            row(&rib, cols, b, 0),
            row(&[s("▀".repeat(cols), Some(b), Some(VOID))], cols, VOID, 0),
            vrow(cols),
        ];
        let body_h = rows.saturating_sub(lines.len() + 1);
        if let Some(entry) = self.panes.get(&sref) {
            let mut p = entry.pane.lock().unwrap();
            if p.size() != (body_h, cols) {
                let _ = p.resize(body_h as u16, cols as u16);
            }
            let cells = p.cells();
            drop(p);
            for r in 0..body_h {
                match cells.get(r) {
                    Some(cr) => {
                        let mut segs: Vec<Seg> = Vec::new();
                        for c in cr {
                            match segs.last_mut() {
                                Some(last)
                                    if last.fg == c.fg && last.bg == c.bg && last.bold == c.bold =>
                                {
                                    last.text.push_str(&c.text)
                                }
                                _ => segs.push(Seg {
                                    text: c.text.clone(),
                                    fg: c.fg,
                                    bg: c.bg,
                                    bold: c.bold,
                                }),
                            }
                        }
                        lines.push(row(&segs, cols, CLI_BG, 0));
                    }
                    None => lines.push(row(&[], cols, CLI_BG, 0)),
                }
            }
        } else {
            for _ in 0..body_h {
                lines.push(row(&[], cols, CLI_BG, 0));
            }
        }
        lines.push(row(&[s("▀".repeat(cols), Some(CLI_BG), Some(VOID))], cols, VOID, 0));
        let _ = format!("{}{}", bg(VOID), RESET);
        lines.truncate(rows);
        lines
    }

    // ---- helpers ---------------------------------------------------------

    fn ready_harnesses(&self) -> Vec<String> {
        HKEYS
            .iter()
            .filter(|h| {
                self.probes
                    .get(**h)
                    .map(|c| c.mode != HarnessMode::Unsupported)
                    .unwrap_or(false)
            })
            .map(|h| h.to_string())
            .collect()
    }

    fn selected_session(&self) -> Option<(String, taskai_core::registry::Session)> {
        let task_ref = self.sel_task.clone()?;
        let rails = self.rail_rows(&task_ref);
        let sel = self.st.rail_sel.min(rails.len().checked_sub(1)?);
        let sref = rails[sel].key.clone();
        self.registry.session(&sref).cloned().map(|se| (sref, se))
    }

    fn spawn_pane(&mut self, sref: &str, cols: usize, rows: usize) -> Result<(), String> {
        if self.panes.contains_key(sref) {
            return Ok(());
        }
        let sess = self.registry.session(sref).cloned().ok_or("unknown session")?;
        let ws = sess.workspace.clone();
        let run = self.home_dir.join("run");
        let home_str = self.home_dir.display().to_string();
        let body_h = rows.saturating_sub(8).saturating_sub(3).max(10);
        let body_w = cols - task::RAIL_W - task::GAP;
        let resume_id = sess.provider.as_ref().map(|p| p.provider_session_id.clone());

        let (prog, args, mut envs): (String, Vec<String>, Vec<(String, String)>) = match sess
            .harness
            .as_str()
        {
            "claude" => {
                let events = install_hooks(&ws, &run).map_err(|e| e.to_string())?;
                let _ = taskai_context::install_bootstrap(&ws);
                if let Ok(exe) = std::env::current_exe() {
                    let _ = install_context_hook(&ws, &exe);
                }
                let offset = std::fs::metadata(&events).map(|m| m.len()).unwrap_or(0);
                let tx = self.bind_tx.clone();
                let sref2 = sref.to_string();
                std::thread::spawn(move || {
                    if let Ok(ev) =
                        wait_for_event(&events, "SessionStart", None, offset, Duration::from_secs(300))
                    {
                        if let Some(id) = ev.session_id {
                            let _ = tx.send((sref2, id));
                        }
                    }
                });
                let args = match &resume_id {
                    Some(id) => vec!["--resume".to_string(), id.clone()],
                    None => Vec::new(),
                };
                ("claude".to_string(), args, Vec::new())
            }
            "codex" => {
                let _ = taskai_context::install_bootstrap(&ws);
                let args = match &resume_id {
                    Some(id) => vec!["resume".to_string(), id.clone()],
                    None => Vec::new(),
                };
                ("codex".to_string(), args, Vec::new())
            }
            "opencode" => {
                let _ = taskai_context::install_bootstrap(&ws);
                if !self.oc_servers.contains_key(&ws) {
                    let server = OpenCodeConnector::default()
                        .start_server(&ws)
                        .map_err(|e| e.to_string())?;
                    self.oc_servers.insert(ws.clone(), server);
                }
                let server = &self.oc_servers[&ws];
                let (prog, args, envs) = server.attach_command();
                (prog, args, envs)
            }
            "grok" => {
                let _ = taskai_context::install_bootstrap(&ws);
                let args = match &resume_id {
                    Some(id) => vec!["--resume".to_string(), id.clone()],
                    None => Vec::new(),
                };
                ("grok".to_string(), args, Vec::new())
            }
            other => return Err(format!("no connector for {other}")),
        };
        envs.push(("TASKAI_SESSION_REF".into(), sref.to_string()));
        envs.push(("TASKAI_HOME".into(), home_str));
        let envrefs: Vec<(&str, &str)> =
            envs.iter().map(|(k, v)| (k.as_str(), v.as_str())).collect();
        let argrefs: Vec<&str> = args.iter().map(String::as_str).collect();
        let pane = Pane::spawn(&prog, &argrefs, &ws, body_h as u16, body_w as u16, &envrefs)
            .map_err(|e| e.to_string())?;
        let pane = Arc::new(Mutex::new(pane));
        self.panes.insert(
            sref.to_string(),
            PaneEntry {
                pane: Arc::clone(&pane),
                spawned: std::time::Instant::now(),
                resumed: resume_id.is_some(),
            },
        );
        if sess.harness == "claude" {
            self.served.panes.lock().unwrap().insert(sref.to_string(), pane);
        }
        let state = match sess.harness.as_str() {
            "grok" => SessionState::Handoff,
            "claude" => SessionState::Working, // Live lands when SessionStart binds
            _ => SessionState::Live,
        };
        let _ = self.registry.set_state(sref, state);
        Ok(())
    }

    fn create_manager(&mut self, f: &ManagerForm) -> Result<String, String> {
        let task_ref = self.sel_task.clone().ok_or("no task selected")?;
        let repo = self.registry.task(&task_ref).ok_or("task missing")?.repo.clone();
        let hkey = f.ready.get(f.hsel).ok_or("no ready harness")?.clone();
        let name = if f.name.trim().is_empty() { "Lead" } else { f.name.trim() };
        self.registry
            .create_session(&task_ref, None, name, &hkey, &repo, Source::HumanUi)
            .map_err(|e| e.to_string())
    }

    fn create_sub(&mut self, f: &SubForm) -> Result<String, String> {
        let task_ref = self.sel_task.clone().ok_or("no task selected")?;
        let repo = self.registry.task(&task_ref).ok_or("task missing")?.repo.clone();
        let hkey = f.ready.get(f.hsel).ok_or("no ready harness")?.clone();
        let choice = match f.ws_mode {
            0 => WorkspaceChoice::SameRepo,
            1 => WorkspaceChoice::Worktree {
                base: repo.clone(),
                branch: f.branch.clone(),
                name: if f.wname.trim().is_empty() {
                    f.role.to_lowercase().replace(' ', "-")
                } else {
                    f.wname.trim().to_string()
                },
            },
            _ => WorkspaceChoice::OtherRepo(PathBuf::from(f.other.clone())),
        };
        let ws = provision(&choice, &repo).map_err(|e| e.to_string())?;
        let role = if f.role.trim().is_empty() { "Subordinate" } else { f.role.trim() };
        self.registry
            .create_session(&task_ref, Some(&f.parent), role, &hkey, &ws, Source::HumanUi)
            .map_err(|e| e.to_string())
    }

    // ---- input -----------------------------------------------------------

    fn handle(&mut self, k: KeyEvent, cols: usize, rows: usize) -> bool {
        self.status = None;
        let ctrl_c = k.code == KeyCode::Char('c') && k.modifiers.contains(KeyModifiers::CONTROL);
        match &mut self.mode {
            Mode::Boot => {
                self.st.boot_k = 22;
                true
            }
            Mode::Home => {
                if ctrl_c {
                    return false;
                }
                let n_tasks = self.registry.tasks().len();
                match k.code {
                    KeyCode::Char('q') => return false,
                    KeyCode::Up => self.st.home_sel = self.st.home_sel.saturating_sub(1),
                    KeyCode::Down => self.st.home_sel = (self.st.home_sel + 1).min(n_tasks),
                    KeyCode::Char('h') => self.mode = Mode::Harnesses,
                    KeyCode::Char('?') => self.mode = Mode::Help(PrevMode::FromHome),
                    KeyCode::Char('n') => self.open_newtask(),
                    KeyCode::Enter => {
                        if self.st.home_sel >= n_tasks {
                            self.open_newtask();
                        } else {
                            let t = self.registry.tasks()[self.st.home_sel].task_ref.clone();
                            self.sel_task = Some(t);
                            self.st.rail_sel = 0;
                            self.mode = Mode::Task;
                        }
                    }
                    _ => {}
                }
                true
            }
            Mode::Harnesses => {
                if ctrl_c || k.code == KeyCode::Char('q') {
                    return !matches!(k.code, KeyCode::Char('q')) && !ctrl_c;
                }
                match k.code {
                    KeyCode::Esc | KeyCode::Char('T') => self.mode = Mode::Home,
                    KeyCode::Char('?') => self.mode = Mode::Help(PrevMode::FromHome),
                    _ => {}
                }
                true
            }
            Mode::Task => {
                if ctrl_c {
                    return false;
                }
                let rails = self.sel_task.as_ref().map(|t| self.rail_rows(t)).unwrap_or_default();
                match k.code {
                    KeyCode::Char('q') => return false,
                    KeyCode::Up => self.st.rail_sel = self.st.rail_sel.saturating_sub(1),
                    KeyCode::Down => {
                        self.st.rail_sel =
                            (self.st.rail_sel + 1).min(rails.len().saturating_sub(1))
                    }
                    KeyCode::Char('c') => {
                        self.st.view =
                            if self.st.view == View::Native { View::Acp } else { View::Native }
                    }
                    KeyCode::Char('T') => self.mode = Mode::Home,
                    KeyCode::Char('h') => self.mode = Mode::Harnesses,
                    KeyCode::Char('?') => self.mode = Mode::Help(PrevMode::FromTask),
                    KeyCode::Char('m') => {
                        let ready = self.ready_harnesses();
                        if ready.is_empty() {
                            self.status = Some("no ready harnesses — check the bay with  h".into());
                        } else {
                            self.mode = Mode::Manager(ManagerForm {
                                name: "Lead".into(),
                                ready,
                                hsel: 0,
                                focus: 0,
                            });
                        }
                    }
                    KeyCode::Char('a') => match self.selected_session() {
                        Some((sref, sess)) if sess.role == Role::Manager => {
                            let ready = self.ready_harnesses();
                            if ready.is_empty() {
                                self.status =
                                    Some("no ready harnesses — check the bay with  h".into());
                            } else {
                                self.mode = Mode::Sub(SubForm {
                                    parent: sref,
                                    role: "".into(),
                                    ready,
                                    hsel: 0,
                                    ws_mode: 1,
                                    branch: "main".into(),
                                    wname: "".into(),
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
                        if let Some(task_ref) = self.sel_task.clone() {
                            if let Some(t) = self.registry.task(&task_ref) {
                                let title = format!("Destroy task — {}", t.name);
                                let expect =
                                    t.name.to_lowercase().replace(' ', "-");
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
                    KeyCode::Enter | KeyCode::Char('g')
                        if k.code == KeyCode::Enter
                            || k.modifiers.contains(KeyModifiers::CONTROL) =>
                    {
                        if let Some((sref, _)) = self.selected_session() {
                            match self.spawn_pane(&sref, cols, rows) {
                                Ok(()) => {
                                    self.mode = Mode::Attach;
                                    self.focus = Focus::Cli;
                                }
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
            Mode::Attach => {
                match route(self.focus, k) {
                    Route::Boundary => {
                        self.mode = Mode::Task;
                        self.focus = Focus::Nav;
                    }
                    Route::Forward(bytes) => {
                        if let Some((sref, _)) = self.selected_session() {
                            if let Some(e) = self.panes.get(&sref) {
                                let _ = e.pane.lock().unwrap().write_bytes(&bytes);
                            }
                        }
                    }
                    Route::Nav(_) => {}
                }
                true
            }
            Mode::Help(prev) => {
                let prev = *prev;
                if matches!(k.code, KeyCode::Esc | KeyCode::Enter | KeyCode::Char('?')) {
                    self.mode = match prev {
                        PrevMode::FromHome => Mode::Home,
                        PrevMode::FromTask => Mode::Task,
                    };
                }
                true
            }
            Mode::NewTask(f) => {
                if let Some(ex) = &mut f.explorer {
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
                        }
                        KeyCode::Esc => f.explorer = None,
                        _ => {}
                    }
                    return true;
                }
                match k.code {
                    KeyCode::Esc => self.mode = Mode::Home,
                    KeyCode::Tab | KeyCode::Down | KeyCode::Up => f.focus = 1 - f.focus,
                    KeyCode::Backspace => {
                        if f.focus == 0 {
                            f.name.pop();
                        } else {
                            f.repo.pop();
                        }
                    }
                    KeyCode::Char(ch) => {
                        if f.focus == 0 {
                            f.name.push(ch);
                        } else {
                            f.repo.push(ch);
                        }
                    }
                    KeyCode::Enter => {
                        let repo = PathBuf::from(f.repo.clone());
                        if f.focus == 1 && !repo.is_dir() {
                            let start = if repo.is_dir() {
                                repo
                            } else {
                                std::env::current_dir().unwrap_or_else(|_| "/".into())
                            };
                            f.explorer = Some(Explorer::open(&start));
                        } else if f.name.trim().is_empty() {
                            f.focus = 0;
                            self.status = Some("name the task first".into());
                        } else if !repo.is_dir() {
                            f.focus = 1;
                            self.status = Some("repository path does not exist".into());
                        } else {
                            let name = f.name.trim().to_string();
                            match self.registry.create_task(&name, &repo, Source::HumanUi) {
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
                    KeyCode::Tab | KeyCode::Down | KeyCode::Up => f.focus = 1 - f.focus,
                    KeyCode::Left if f.focus == 1 => f.hsel = f.hsel.saturating_sub(1),
                    KeyCode::Right if f.focus == 1 => {
                        f.hsel = (f.hsel + 1).min(f.ready.len().saturating_sub(1))
                    }
                    KeyCode::Backspace if f.focus == 0 => {
                        f.name.pop();
                    }
                    KeyCode::Char(ch) if f.focus == 0 => f.name.push(ch),
                    KeyCode::Enter => {
                        let form = ManagerForm {
                            name: f.name.clone(),
                            ready: f.ready.clone(),
                            hsel: f.hsel,
                            focus: f.focus,
                        };
                        match self.create_manager(&form) {
                            Ok(sref) => {
                                self.mode = Mode::Task;
                                if let Some(t) = self.sel_task.clone() {
                                    let rails = self.rail_rows(&t);
                                    if let Some(i) =
                                        rails.iter().position(|r| r.key == sref)
                                    {
                                        self.st.rail_sel = i;
                                    }
                                }
                                match self.spawn_pane(&sref, cols, rows) {
                                    Ok(()) => {
                                        self.status =
                                            Some(format!("{sref} launched — Enter attaches"))
                                    }
                                    Err(e) => {
                                        self.status = Some(format!("registered {sref}; launch failed: {e}"))
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
                if let Some(ex) = &mut f.explorer {
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
                let max_focus = if f.ws_mode == 1 { 4 } else { 2 };
                match k.code {
                    KeyCode::Esc => self.mode = Mode::Task,
                    KeyCode::Tab | KeyCode::Down => f.focus = (f.focus + 1) % (max_focus + 1),
                    KeyCode::Up => f.focus = f.focus.checked_sub(1).unwrap_or(max_focus),
                    KeyCode::Left => match f.focus {
                        1 => f.hsel = f.hsel.saturating_sub(1),
                        2 => f.ws_mode = f.ws_mode.saturating_sub(1),
                        _ => {}
                    },
                    KeyCode::Right => match f.focus {
                        1 => f.hsel = (f.hsel + 1).min(f.ready.len().saturating_sub(1)),
                        2 => {
                            f.ws_mode = (f.ws_mode + 1).min(2);
                            if f.ws_mode == 2 && f.other.is_empty() {
                                let start = std::env::current_dir().unwrap_or_else(|_| "/".into());
                                f.explorer = Some(Explorer::open(&start));
                            }
                        }
                        _ => {}
                    },
                    KeyCode::Backspace => match f.focus {
                        0 => {
                            f.role.pop();
                        }
                        3 => {
                            f.branch.pop();
                        }
                        4 => {
                            f.wname.pop();
                        }
                        _ => {}
                    },
                    KeyCode::Char(ch) => match f.focus {
                        0 => f.role.push(ch),
                        3 => f.branch.push(ch),
                        4 => f.wname.push(ch),
                        _ => {}
                    },
                    KeyCode::Enter => {
                        let form = SubForm {
                            parent: f.parent.clone(),
                            role: f.role.clone(),
                            ready: f.ready.clone(),
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
                                    if let Some(i) =
                                        rails.iter().position(|r| r.key == sref)
                                    {
                                        self.st.rail_sel = i;
                                    }
                                }
                                match self.spawn_pane(&sref, cols, rows) {
                                    Ok(()) => {
                                        self.status =
                                            Some(format!("{sref} launched — Enter attaches"))
                                    }
                                    Err(e) => {
                                        self.status = Some(format!("registered {sref}; launch failed: {e}"))
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
                        self.mode = if f.back_home { Mode::Task } else { Mode::Task };
                    }
                    KeyCode::Backspace => {
                        f.typed.pop();
                    }
                    KeyCode::Char(ch) => f.typed.push(ch),
                    KeyCode::Enter => {
                        if f.typed == f.expect {
                            let item = f.item.clone();
                            match self.registry.destroy(&item, Source::HumanUi) {
                                Ok(removed) => {
                                    for sref in &removed {
                                        self.panes.remove(sref);
                                        self.served.panes.lock().unwrap().remove(sref);
                                        self.served.session_ids.lock().unwrap().remove(sref);
                                    }
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

    fn open_newtask(&mut self) {
        let cwd = std::env::current_dir().unwrap_or_else(|_| "/".into());
        self.mode = Mode::NewTask(NewTaskForm {
            name: String::new(),
            repo: cwd.display().to_string(),
            focus: 0,
            explorer: None,
        });
    }
}
