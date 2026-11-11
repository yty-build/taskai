// ABOUTME: Server/profile settings UI with explicit native-provider configuration.
// ABOUTME: Pure UI state returns actions; the app owns persistence, probes, and terminals.

use std::path::PathBuf;

use crossterm::event::{KeyCode, KeyEvent, KeyModifiers, MouseButton, MouseEvent, MouseEventKind};
use taskai_core::registry::{ConnectionTarget, HarnessSettings, Registry, ServerProfile};
use taskai_tui::chrome::{keybar, masthead};
use taskai_tui::color::{AMB, DIM, FT, GLASS, GLASS_HI, ION, OK, TEXT, VOID};
use taskai_tui::overlay::{centered_overlay, drawer_panel_view, DrawerView};
use taskai_tui::seg::{clip_segs, row, s, sb, take_cells, text_width, vrow};

use crate::forms::{self, Editor};
use crate::remote_probes::HarnessAvailability;

const HARNESS_KEYS: [&str; 4] = ["claude", "codex", "opencode", "grok"];
const LEFT: usize = 28;
const GAP: usize = 2;

#[derive(Clone, Debug, Default)]
pub struct ServerCheck {
    pub choices: Vec<HarnessAvailability>,
    pub checking: bool,
    pub error: Option<String>,
}

#[derive(Clone, Debug)]
pub enum Action {
    Back,
    Help,
    Home,
    Leave,
    Check {
        server_id: String,
    },
    OpenTerminal {
        server_id: String,
    },
    OpenHelper {
        server_id: String,
    },
    Configure {
        server_id: String,
        hkey: String,
    },
    Create {
        name: String,
        target: ConnectionTarget,
        repository_root: Option<PathBuf>,
    },
    Update {
        server_id: String,
        name: String,
        repository_root: Option<PathBuf>,
    },
    SaveHarness {
        server_id: String,
        hkey: String,
        settings: HarnessSettings,
    },
    Archive {
        server_id: String,
    },
}

#[derive(Clone, Copy, Default, PartialEq)]
enum Focus {
    #[default]
    Servers,
    Harnesses,
}

enum Dialog {
    Profile {
        server_id: Option<String>,
        name: String,
        target: String,
        root: String,
        field: usize,
    },
    Harness {
        server_id: String,
        hkey: String,
        enabled: bool,
        executable: String,
        model: String,
        field: usize,
    },
    Archive {
        server_id: String,
        name: String,
    },
}

#[derive(Clone)]
enum HitKind {
    Server(String),
    Harness(usize),
    Field(usize),
    Key(KeyEvent),
}

#[derive(Clone)]
struct Hit {
    x: usize,
    y: usize,
    width: usize,
    height: usize,
    kind: HitKind,
}

#[derive(Default)]
pub struct Settings {
    selected: Option<String>,
    harness: usize,
    focus: Focus,
    dialog: Option<Dialog>,
    editor: Editor,
    status: Option<String>,
    hits: Vec<Hit>,
}

impl Settings {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn has_dialog(&self) -> bool {
        self.dialog.is_some()
    }

    pub fn selected_server_id(&self, registry: &Registry) -> Option<String> {
        let servers = visible_servers(registry);
        self.selected
            .as_ref()
            .filter(|id| servers.iter().any(|server| &server.server_id == *id))
            .cloned()
            .or_else(|| servers.first().map(|server| server.server_id.clone()))
    }

    pub fn select_server(&mut self, server_id: &str) {
        self.selected = Some(server_id.into());
    }

    pub fn select_harness(&mut self, hkey: &str) {
        if let Some(index) = HARNESS_KEYS.iter().position(|key| *key == hkey) {
            self.harness = index;
            self.focus = Focus::Harnesses;
            self.status = None;
        }
    }

    pub fn saved(&mut self, server_id: &str) {
        self.select_server(server_id);
        self.dialog = None;
        self.editor = Editor::default();
        self.status =
            Some("Saved. New agents use these defaults; running sessions stay unchanged.".into());
    }

    pub fn set_status(&mut self, message: impl Into<String>) {
        self.status = Some(message.into());
    }

    fn profile<'a>(&self, registry: &'a Registry) -> Option<&'a ServerProfile> {
        registry.server(&self.selected_server_id(registry)?)
    }

    fn text_field(&mut self) -> Option<&mut String> {
        match self.dialog.as_mut()? {
            Dialog::Profile {
                server_id,
                name,
                target,
                root,
                field,
            } => match *field {
                0 => Some(name),
                1 if server_id.is_none() => Some(target),
                2 => Some(root),
                _ => None,
            },
            Dialog::Harness {
                executable,
                model,
                field,
                ..
            } => match *field {
                1 => Some(executable),
                2 => Some(model),
                _ => None,
            },
            Dialog::Archive { .. } => None,
        }
    }

    fn select_field(&mut self, field: usize) {
        match &mut self.dialog {
            Some(Dialog::Profile {
                field: selected, ..
            })
            | Some(Dialog::Harness {
                field: selected, ..
            }) => *selected = field,
            _ => return,
        }
        self.editor = Editor {
            cursor: None,
            selected: self.text_field().is_some_and(|field| !field.is_empty()),
        };
    }

    pub fn paste(&mut self, text: &str) {
        let path = matches!(
            &self.dialog,
            Some(Dialog::Profile { field: 2, .. }) | Some(Dialog::Harness { field: 1, .. })
        );
        let text = forms::clean_paste(text, path);
        let mut editor = self.editor;
        if let Some(field) = self.text_field() {
            editor.insert(field, &text);
            self.editor = editor;
            self.status = None;
        }
    }

    fn open_harness(&mut self, registry: &Registry) {
        let Some(profile) = self.profile(registry) else {
            return;
        };
        let hkey = HARNESS_KEYS[self.harness];
        let settings = profile.harnesses.get(hkey).cloned().unwrap_or_default();
        self.dialog = Some(Dialog::Harness {
            server_id: profile.server_id.clone(),
            hkey: hkey.into(),
            enabled: settings.enabled,
            executable: settings.executable.unwrap_or_default(),
            model: settings.model.unwrap_or_default(),
            field: 0,
        });
        self.editor = Editor::default();
    }

    pub fn handle(&mut self, key: KeyEvent, registry: &Registry) -> Option<Action> {
        self.status = None;
        if self.dialog.is_some() {
            if key.code == KeyCode::Esc {
                self.dialog = None;
                self.editor = Editor::default();
                return None;
            }
            let mut editor = self.editor;
            if let Some(field) = self.text_field() {
                if editor.key(field, key) {
                    self.editor = editor;
                    self.status = None;
                    return None;
                }
            }
            if matches!(
                key.code,
                KeyCode::Tab | KeyCode::BackTab | KeyCode::Up | KeyCode::Down
            ) {
                let fields: &[usize] = match &self.dialog {
                    Some(Dialog::Profile {
                        server_id: Some(_), ..
                    }) => &[0, 2],
                    Some(Dialog::Archive { .. }) => &[],
                    _ => &[0, 1, 2],
                };
                let current = match &self.dialog {
                    Some(Dialog::Profile { field, .. }) | Some(Dialog::Harness { field, .. }) => {
                        *field
                    }
                    _ => return None,
                };
                let index = fields
                    .iter()
                    .position(|field| *field == current)
                    .unwrap_or(0);
                let backward = matches!(key.code, KeyCode::BackTab | KeyCode::Up);
                self.select_field(
                    fields[if backward {
                        (index + fields.len() - 1) % fields.len()
                    } else {
                        (index + 1) % fields.len()
                    }],
                );
                return None;
            }
            if let Some(Dialog::Harness {
                enabled, field: 0, ..
            }) = &mut self.dialog
            {
                if matches!(
                    key.code,
                    KeyCode::Char(' ') | KeyCode::Left | KeyCode::Right
                ) {
                    *enabled = !*enabled;
                    return None;
                }
            }
            if key.code == KeyCode::Enter {
                return self.save_action();
            }
            return None;
        }
        // Leaving Settings must not depend on an available selected profile.
        if key.code == KeyCode::Esc {
            return Some(Action::Back);
        }
        // These are visible navigation shortcuts, not provider input. Modified
        // keys must not accidentally launch a console (for example Ctrl+C).
        if key.modifiers.intersects(
            KeyModifiers::CONTROL
                | KeyModifiers::ALT
                | KeyModifiers::SUPER
                | KeyModifiers::META
                | KeyModifiers::HYPER,
        ) {
            return None;
        }
        match key.code {
            KeyCode::Char('?') => return Some(Action::Help),
            KeyCode::Char('G') => return Some(Action::Home),
            KeyCode::Char('q') => return Some(Action::Leave),
            // Settings is already open. Keep h as a legacy Settings alias,
            // never as a hidden shortcut for editing a provider.
            KeyCode::Char('s' | 'h') => return None,
            _ => {}
        }
        let server_id = self.selected_server_id(registry)?;
        match key.code {
            KeyCode::Tab | KeyCode::BackTab | KeyCode::Left | KeyCode::Right => {
                self.focus = if self.focus == Focus::Servers {
                    Focus::Harnesses
                } else {
                    Focus::Servers
                };
            }
            KeyCode::Up | KeyCode::Down => {
                let down = key.code == KeyCode::Down;
                if self.focus == Focus::Harnesses {
                    self.harness = if down {
                        (self.harness + 1).min(3)
                    } else {
                        self.harness.saturating_sub(1)
                    };
                } else {
                    let servers = visible_servers(registry);
                    let index = servers
                        .iter()
                        .position(|server| server.server_id == server_id)
                        .unwrap_or(0);
                    let next = if down {
                        (index + 1).min(servers.len() - 1)
                    } else {
                        index.saturating_sub(1)
                    };
                    self.selected = Some(servers[next].server_id.clone());
                }
            }
            KeyCode::Char('n') => {
                self.dialog = Some(Dialog::Profile {
                    server_id: None,
                    name: String::new(),
                    target: String::new(),
                    root: String::new(),
                    field: 0,
                });
                self.editor = Editor::default();
            }
            KeyCode::Char('e') => {
                let profile = self.profile(registry)?;
                self.dialog = Some(Dialog::Profile {
                    server_id: Some(server_id),
                    name: profile.name.clone(),
                    target: profile.target.to_string(),
                    root: profile
                        .repository_root
                        .as_ref()
                        .map(|root| root.display().to_string())
                        .unwrap_or_default(),
                    field: 0,
                });
                self.select_field(0);
            }
            KeyCode::Enter => self.open_harness(registry),
            KeyCode::Char(' ') => {
                let profile = self.profile(registry)?;
                let hkey = HARNESS_KEYS[self.harness].to_string();
                let mut settings = profile.harnesses.get(&hkey).cloned().unwrap_or_default();
                settings.enabled = !settings.enabled;
                return Some(Action::SaveHarness {
                    server_id,
                    hkey,
                    settings,
                });
            }
            KeyCode::F(5) => return Some(Action::Check { server_id }),
            KeyCode::F(1) => return Some(Action::OpenHelper { server_id }),
            KeyCode::Char('t') => return Some(Action::OpenTerminal { server_id }),
            KeyCode::Char('c') => {
                return Some(Action::Configure {
                    server_id,
                    hkey: HARNESS_KEYS[self.harness].into(),
                })
            }
            KeyCode::Char('d') => {
                if server_id == "local" {
                    self.status = Some("This computer is built in and cannot be archived.".into());
                } else {
                    self.dialog = Some(Dialog::Archive {
                        name: self.profile(registry)?.name.clone(),
                        server_id,
                    });
                }
            }
            _ => {}
        }
        None
    }

    fn save_action(&mut self) -> Option<Action> {
        match self.dialog.as_ref()? {
            Dialog::Profile {
                server_id,
                name,
                target,
                root,
                ..
            } => {
                if name.trim().is_empty() {
                    self.status = Some("Give this server a display name.".into());
                    return None;
                }
                let repository_root = optional(root).map(PathBuf::from);
                if let Some(server_id) = server_id {
                    Some(Action::Update {
                        server_id: server_id.clone(),
                        name: name.trim().into(),
                        repository_root,
                    })
                } else {
                    let alias = target.trim().strip_prefix("ssh ").unwrap_or(target.trim());
                    match ConnectionTarget::ssh(alias) {
                        Ok(target) => Some(Action::Create {
                            name: name.trim().into(),
                            target,
                            repository_root,
                        }),
                        Err(_) => {
                            self.status = Some("Use an SSH config alias such as cruise4b, not a password or shell command.".into());
                            None
                        }
                    }
                }
            }
            Dialog::Harness {
                server_id,
                hkey,
                enabled,
                executable,
                model,
                ..
            } => {
                if hkey == "opencode" && !model.trim().is_empty() {
                    self.status = Some("OpenCode's current attach integration uses its native model default. Clear Default model to save.".into());
                    return None;
                }
                Some(Action::SaveHarness {
                    server_id: server_id.clone(),
                    hkey: hkey.clone(),
                    settings: HarnessSettings {
                        enabled: *enabled,
                        executable: optional(executable),
                        model: optional(model),
                    },
                })
            }
            Dialog::Archive { server_id, .. } => Some(Action::Archive {
                server_id: server_id.clone(),
            }),
        }
    }

    pub fn mouse(
        &mut self,
        event: MouseEvent,
        registry: &Registry,
        cols: usize,
        rows: usize,
    ) -> Option<Action> {
        if cols < 110 || rows < 30 {
            return None;
        }
        if matches!(
            event.kind,
            MouseEventKind::ScrollUp | MouseEventKind::ScrollDown
        ) {
            return self.handle(
                KeyEvent::new(
                    if event.kind == MouseEventKind::ScrollUp {
                        KeyCode::Up
                    } else {
                        KeyCode::Down
                    },
                    KeyModifiers::NONE,
                ),
                registry,
            );
        }
        if event.kind != MouseEventKind::Down(MouseButton::Left) {
            return None;
        }
        let (x, y) = (event.column as usize, event.row as usize);
        let hit = self
            .hits
            .iter()
            .find(|hit| x >= hit.x && x < hit.x + hit.width && y >= hit.y && y < hit.y + hit.height)
            .cloned()?;
        match hit.kind {
            HitKind::Key(key) => return self.handle(key, registry),
            HitKind::Server(id) => {
                self.selected = Some(id);
                self.focus = Focus::Servers;
            }
            HitKind::Harness(index) => {
                self.harness = index;
                self.focus = Focus::Harnesses;
                self.open_harness(registry);
            }
            HitKind::Field(field) => {
                if x < hit.x + 2 || x >= hit.x + hit.width.saturating_sub(3) {
                    return None;
                }
                self.select_field(field);
                if let Some(Dialog::Harness {
                    enabled, field: 0, ..
                }) = &mut self.dialog
                {
                    *enabled = !*enabled;
                } else {
                    let editor = self.editor;
                    if let Some(value) = self.text_field() {
                        if x >= hit.x + hit.width.saturating_sub(11) {
                            value.clear();
                            self.editor = Editor::default();
                        } else {
                            self.editor = Editor {
                                cursor: Some(forms::clicked_position(
                                    value,
                                    editor,
                                    hit.width.saturating_sub(16),
                                    x.saturating_sub(hit.x + 3),
                                )),
                                selected: false,
                            };
                        }
                    }
                }
            }
        }
        self.status = None;
        None
    }

    pub fn frame(
        &mut self,
        registry: &Registry,
        check: Option<&ServerCheck>,
        cols: usize,
        rows: usize,
    ) -> Vec<String> {
        self.hits.clear();
        if cols < 110 || rows < 30 {
            return taskai_tui::render::resize_frame(cols, rows);
        }
        let mut lines = masthead(cols, "SETTINGS · SERVERS", &[], None, "");
        lines.push(vrow(cols));
        let height = rows.saturating_sub(7);
        let right_width = cols - LEFT - GAP;
        let selected = self.selected_server_id(registry);
        let servers = visible_servers(registry);
        let mut left = vec![
            row(&[sb("SERVERS", Some(TEXT), None)], LEFT, GLASS, 2),
            row(&[], LEFT, GLASS, 0),
        ];
        if let Some(local) = servers.first() {
            self.server_rows(&mut left, local, selected.as_deref(), 4);
        }
        left.push(row(&[], LEFT, GLASS, 0));
        let remote = servers.iter().skip(1).copied().collect::<Vec<_>>();
        let capacity = height.saturating_sub(7) / 2;
        let selected_remote = remote
            .iter()
            .position(|server| Some(server.server_id.as_str()) == selected.as_deref())
            .unwrap_or(0);
        let start = selected_remote.saturating_sub(capacity.saturating_sub(1));
        for server in remote.iter().skip(start).take(capacity) {
            self.server_rows(&mut left, server, selected.as_deref(), 4);
        }
        if start > 0 || start + capacity < remote.len() {
            left.push(row(
                &[s("↑/↓ More servers", Some(DIM), None)],
                LEFT,
                GLASS,
                2,
            ));
        }
        while left.len() < height {
            left.push(row(&[], LEFT, GLASS, 0));
        }
        let right = self.detail_rows(registry, check, right_width, height);
        for y in 0..height {
            lines.push(format!("{}{}{}", left[y], vrow(GAP), right[y]));
        }
        let keys = if matches!(self.dialog, Some(Dialog::Archive { .. })) {
            vec![("Enter", "Archive"), ("Esc", "Cancel")]
        } else if matches!(self.dialog, Some(Dialog::Harness { field: 0, .. })) {
            vec![
                ("Tab", "Next"),
                ("Space", "Toggle"),
                ("Enter", "Save"),
                ("Esc", "Cancel"),
            ]
        } else if self.dialog.is_some() {
            vec![
                ("Tab", "Next"),
                ("Ctrl+A", "Select all"),
                ("Ctrl+U", "Clear"),
                ("Enter", "Save"),
                ("Esc", "Cancel"),
            ]
        } else {
            vec![
                ("n", "Add server"),
                ("e", "Edit server"),
                ("Enter", "Edit harness"),
                ("Space", "Enable"),
                ("F5", "Check"),
                ("F1", "Setup helper"),
                ("t", "Terminal"),
                ("c", "Configure CLI"),
                ("d", "Archive server"),
                ("s", "Settings"),
                ("?", "Help"),
                ("G", "Home"),
                ("q", "Leave"),
                ("Esc", "Back"),
            ]
        };
        if self.dialog.is_some() {
            let panel = self.dialog_panel(cols);
            self.hits.clear();
            // Keep the page identity, but quiet the body while editing. Merely
            // dimming the table leaves clipped words peeking around modal edges.
            for line in lines.iter_mut().skip(4) {
                *line = vrow(cols);
            }
            let top = height.saturating_add(4).saturating_sub(panel.len()) / 2;
            let width = panel
                .first()
                .map(|line| taskai_tui::seg::cell_count(line))
                .unwrap_or(0);
            let left = cols.saturating_sub(width) / 2;
            let editable: Vec<usize> = match &self.dialog {
                Some(Dialog::Profile {
                    server_id: Some(_), ..
                }) => vec![0, 2],
                Some(Dialog::Archive { .. }) => vec![],
                _ => vec![0, 1, 2],
            };
            for field in editable {
                self.hits.push(Hit {
                    x: left,
                    y: top + 4 + field * 3,
                    width,
                    height: 1,
                    kind: HitKind::Field(field),
                });
            }
            lines = centered_overlay(&lines, &panel, cols);
        }
        let total = keys
            .iter()
            .map(|(key, label)| key.len() + label.len() + 7)
            .sum::<usize>();
        if total > cols {
            let mid = keys.len().div_ceil(2);
            self.key_hits(&keys[..mid], cols, lines.len());
            self.key_hits(&keys[mid..], cols, lines.len() + 1);
            lines.push(keybar(cols, &keys[..mid])[1].clone());
            lines.push(keybar(cols, &keys[mid..])[1].clone());
        } else {
            self.key_hits(&keys, cols, lines.len() + 1);
            lines.extend(keybar(cols, &keys));
        }
        let hint = match self.dialog {
            Some(Dialog::Archive { .. }) => "Archiving hides this profile. Existing tasks and terminals stay untouched.",
            Some(_) => "Enter saves these defaults. Esc cancels without changing the profile or existing sessions.",
            None => "Select a provider for details. Saved defaults apply only to new agents; running sessions stay unchanged.",
        };
        lines.push(row(
            &clip_segs(
                &[s(self.status.as_deref().unwrap_or(hint), Some(DIM), None)],
                cols.saturating_sub(4),
            ),
            cols,
            VOID,
            2,
        ));
        lines.truncate(rows);
        lines
    }

    fn key_hits(&mut self, pairs: &[(&str, &str)], cols: usize, y: usize) {
        let labels = pairs
            .iter()
            .map(|(key, label)| text_width(key) + text_width(label) + 3)
            .sum::<usize>();
        let spacing = cols
            .saturating_sub(labels)
            .checked_div(pairs.len().saturating_sub(1))
            .unwrap_or(0)
            .min(4);
        let mut x = cols.saturating_sub(labels + spacing * pairs.len().saturating_sub(1)) / 2;
        for (key, label) in pairs {
            let width = text_width(key) + text_width(label) + 3;
            let event = match *key {
                "Enter" => KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
                "Esc" => KeyEvent::new(KeyCode::Esc, KeyModifiers::NONE),
                "Tab" => KeyEvent::new(KeyCode::Tab, KeyModifiers::NONE),
                "Space" => KeyEvent::new(KeyCode::Char(' '), KeyModifiers::NONE),
                "F1" => KeyEvent::new(KeyCode::F(1), KeyModifiers::NONE),
                "F5" => KeyEvent::new(KeyCode::F(5), KeyModifiers::NONE),
                "Ctrl+A" => KeyEvent::new(KeyCode::Char('a'), KeyModifiers::CONTROL),
                "Ctrl+U" => KeyEvent::new(KeyCode::Char('u'), KeyModifiers::CONTROL),
                other => KeyEvent::new(
                    KeyCode::Char(other.chars().next().unwrap_or(' ')),
                    KeyModifiers::NONE,
                ),
            };
            self.hits.push(Hit {
                x,
                y,
                width,
                height: 1,
                kind: HitKind::Key(event),
            });
            x += width + spacing;
        }
    }

    fn server_rows(
        &mut self,
        lines: &mut Vec<String>,
        profile: &ServerProfile,
        selected: Option<&str>,
        top: usize,
    ) {
        let chosen = selected == Some(profile.server_id.as_str());
        let fill = if chosen { GLASS_HI } else { GLASS };
        self.hits.push(Hit {
            x: 0,
            y: top + lines.len(),
            width: LEFT,
            height: 2,
            kind: HitKind::Server(profile.server_id.clone()),
        });
        lines.push(row(
            &clip_segs(
                &[sb(
                    format!("{} {}", if chosen { "›" } else { " " }, profile.name),
                    Some(if chosen { ION } else { TEXT }),
                    None,
                )],
                LEFT - 4,
            ),
            LEFT,
            fill,
            2,
        ));
        lines.push(row(
            &clip_segs(&[s(profile.target.to_string(), Some(DIM), None)], LEFT - 5),
            LEFT,
            fill,
            3,
        ));
    }

    fn detail_rows(
        &mut self,
        registry: &Registry,
        check: Option<&ServerCheck>,
        width: usize,
        height: usize,
    ) -> Vec<String> {
        let Some(profile) = self.profile(registry) else {
            return vec![row(&[], width, GLASS, 0); height];
        };
        let mut lines = vec![row(&[sb(&profile.name, Some(TEXT), None)], width, GLASS, 2)];
        lines.push(row(
            &[s(
                format!("CONNECTION  {}", profile.target),
                Some(DIM),
                None,
            )],
            width,
            GLASS,
            2,
        ));
        let root = profile
            .repository_root
            .as_ref()
            .map(|root| root.display().to_string())
            .unwrap_or_else(|| "Target home folder (default)".into());
        lines.push(row(
            &clip_segs(
                &[s(format!("REPOSITORY ROOT  {root}"), Some(DIM), None)],
                width - 4,
            ),
            width,
            GLASS,
            2,
        ));
        lines.push(row(&[], width, GLASS, 0));
        lines.push(row(
            &[s(
                format!("{:<20} {:<23} VERSION", "PROVIDER", "STATUS"),
                Some(FT),
                None,
            )],
            width,
            GLASS,
            2,
        ));
        for (index, hkey) in HARNESS_KEYS.iter().enumerate() {
            let settings = profile.harnesses.get(*hkey).cloned().unwrap_or_default();
            let found =
                check.and_then(|check| check.choices.iter().find(|entry| entry.hkey == *hkey));
            let version = found
                .and_then(|entry| entry.version.as_deref())
                .unwrap_or("—");
            let status = provider_status(settings.enabled, check, found);
            let chosen = self.harness == index;
            let fill = if chosen { GLASS_HI } else { GLASS };
            self.hits.push(Hit {
                x: LEFT + GAP,
                y: 4 + lines.len(),
                width,
                height: 1,
                kind: HitKind::Harness(index),
            });
            lines.push(row(
                &[
                    s(
                        format!("{:<20} ", harness_title(hkey)),
                        Some(if chosen { ION } else { TEXT }),
                        None,
                    ),
                    s(format!("{status:<23} "), Some(status_color(status)), None),
                    s(
                        take_cells(version, width.saturating_sub(50)),
                        Some(DIM),
                        None,
                    ),
                ],
                width,
                fill,
                2,
            ));
        }
        lines.push(row(&[], width, GLASS, 0));
        let hkey = HARNESS_KEYS[self.harness];
        let settings = profile.harnesses.get(hkey).cloned().unwrap_or_default();
        let found = check.and_then(|check| check.choices.iter().find(|entry| entry.hkey == hkey));
        lines.push(row(
            &[sb(
                format!("{} · Details", harness_title(hkey)),
                Some(TEXT),
                None,
            )],
            width,
            GLASS,
            2,
        ));
        lines.push(row(
            &clip_segs(
                &[s(
                    format!(
                        "Executable  {}",
                        settings.executable.as_deref().unwrap_or(hkey)
                    ),
                    Some(DIM),
                    None,
                )],
                width - 4,
            ),
            width,
            GLASS,
            2,
        ));
        lines.push(row(
            &clip_segs(
                &[s(
                    format!(
                        "Model       {}",
                        settings.model.as_deref().unwrap_or("Native default")
                    ),
                    Some(DIM),
                    None,
                )],
                width - 4,
            ),
            width,
            GLASS,
            2,
        ));
        lines.push(row(
            &[s(
                format!(
                    "Sign-in     {}",
                    if check.is_some_and(|check| check.checking) {
                        "Checking…"
                    } else if check.is_some_and(|check| check.error.is_some()) {
                        "Couldn't verify"
                    } else {
                        found
                            .map(HarnessAvailability::authentication_status)
                            .unwrap_or("Not checked")
                    }
                ),
                Some(DIM),
                None,
            )],
            width,
            GLASS,
            2,
        ));
        let messaging = messaging_status(hkey, profile, check, found, width);
        let (first, rest) = messaging.split_first().expect("wrap yields a line");
        lines.push(row(
            &[s(format!("Messaging   {first}"), Some(DIM), None)],
            width,
            GLASS,
            2,
        ));
        for text in rest {
            lines.push(row(
                &[s(format!("            {text}"), Some(DIM), None)],
                width,
                GLASS,
                2,
            ));
        }
        for text in wrap(
            "Model access isn't tested here; sign-in alone doesn't prove access.",
            width - 4,
        ) {
            lines.push(row(&[s(text, Some(DIM), None)], width, GLASS, 2));
        }
        let provider_detail = found.map(HarnessAvailability::actionable_status);
        let reason = if !settings.enabled {
            "Space enables this provider for new agents. Existing sessions are unchanged."
        } else if let Some(check) = check {
            if check.checking {
                "Checking installed versions and sign-in… navigation remains available."
            } else if let Some(error) = &check.error {
                error
            } else {
                provider_detail
                    .as_deref()
                    .unwrap_or("Provider not checked yet. Press F5 to check installed versions.")
            }
        } else {
            "Press F5 to check installed versions on this server."
        };
        for text in wrap(reason, width - 4)
            .into_iter()
            .take(height.saturating_sub(lines.len() + 2))
        {
            lines.push(row(&[s(text, Some(DIM), None)], width, GLASS, 2));
        }
        while lines.len() < height.saturating_sub(1) {
            lines.push(row(&[], width, GLASS, 0));
        }
        lines.push(row(
            &[s(
                "Tab switches server/provider lists · ↑/↓ selects · Enter edits",
                Some(FT),
                None,
            )],
            width,
            GLASS,
            2,
        ));
        lines.truncate(height);
        lines
    }

    fn dialog_panel(&self, cols: usize) -> Vec<String> {
        let width = cols.saturating_sub(12).min(112);
        let (title, fields, notes) = match self.dialog.as_ref().unwrap() {
            Dialog::Profile {
                server_id,
                name,
                target,
                root,
                field,
            } => (
                if server_id.is_some() {
                    "Edit server".into()
                } else {
                    "Add server".into()
                },
                vec![
                    ("Display name".into(), name.clone(), *field == 0),
                    (
                        if server_id.is_some() {
                            "Connection · read only".into()
                        } else {
                            "SSH alias · e.g. cruise4b".into()
                        },
                        target.clone(),
                        *field == 1,
                    ),
                    (
                        "Repository root · blank uses target home".into(),
                        root.clone(),
                        *field == 2,
                    ),
                ],
                vec![
                    "Uses your existing SSH configuration. No credentials are stored here.".into(),
                    "Existing task connections stay unchanged when profile defaults are edited."
                        .into(),
                ],
            ),
            Dialog::Harness {
                hkey,
                enabled,
                executable,
                model,
                field,
                ..
            } => (
                format!("{} · Defaults", harness_title(hkey)),
                vec![
                    (
                        "Enabled · Space toggles".into(),
                        if *enabled { "Yes".into() } else { "No".into() },
                        *field == 0,
                    ),
                    (
                        "Executable · blank uses provider name in PATH".into(),
                        executable.clone(),
                        *field == 1,
                    ),
                    (
                        "Default model · blank uses native default".into(),
                        model.clone(),
                        *field == 2,
                    ),
                ],
                vec![
                    if hkey == "opencode" {
                        "OpenCode's current attach integration supports Native default only.".into()
                    } else {
                        "Model IDs are provider-specific; TaskAI does not infer access or credentials.".into()
                    },
                    "Saved defaults apply to new agents. Existing sessions are not changed.".into(),
                ],
            ),
            Dialog::Archive { name, .. } => (
                format!("Archive {name}?"),
                vec![],
                vec![
                    "This hides the profile from new-task selection.".into(),
                    "Existing tasks, repositories, and running sessions are untouched.".into(),
                    "Enter confirms archive · Esc cancels".into(),
                ],
            ),
        };
        let notes = notes
            .into_iter()
            .flat_map(|note: String| wrap(&note, width - 4))
            .map(|text| vec![s(text, Some(DIM), None)])
            .collect::<Vec<_>>();
        let drawer = DrawerView {
            title,
            fields,
            keys: vec![],
        };
        let mut panel = drawer_panel_view(false, &drawer, &notes, width);
        for (index, (_, value, focused)) in drawer.fields.iter().enumerate() {
            let editable = match &self.dialog {
                Some(Dialog::Profile { server_id, .. }) => index != 1 || server_id.is_none(),
                Some(Dialog::Harness { .. }) => index != 0,
                _ => false,
            };
            if editable {
                panel[4 + index * 3] = forms::input_row(value, self.editor, *focused, width);
            }
        }
        panel
    }
}

fn optional(value: &str) -> Option<String> {
    (!value.trim().is_empty()).then(|| value.trim().to_string())
}

/// The messaging adapter's gate for the selected harness, wrapped to the details width. It is
/// separate from launch status: a native provider launches whether or not messaging is ready.
fn messaging_status(
    hkey: &str,
    profile: &ServerProfile,
    check: Option<&ServerCheck>,
    found: Option<&HarnessAvailability>,
    width: usize,
) -> Vec<String> {
    let text = if profile.target != ConnectionTarget::Local {
        "Local servers only".to_string()
    } else if hkey == "opencode" {
        "No adapter for OpenCode".to_string()
    } else if check.is_some_and(|check| check.checking) {
        "Checking…".to_string()
    } else {
        match found.and_then(|found| found.messaging.as_ref()) {
            Some(gate) => gate.label(),
            None => "Not checked · F5 checks".to_string(),
        }
    };
    // The label follows a 12-cell caption inside the 2-cell padded details column.
    wrap(&text, width.saturating_sub(16).max(20))
}

fn provider_status(
    enabled: bool,
    check: Option<&ServerCheck>,
    found: Option<&HarnessAvailability>,
) -> &'static str {
    if !enabled {
        "Disabled"
    } else if check.is_some_and(|check| check.checking) {
        "Checking"
    } else if check.is_some_and(|check| check.error.is_some()) {
        "Couldn't verify"
    } else {
        found
            .map(HarnessAvailability::status_label)
            .unwrap_or("Couldn't verify")
    }
}

fn status_color(status: &str) -> taskai_tui::color::Rgb {
    match status {
        "Ready" => OK,
        "Checking" => ION,
        "Sign in needed" | "Update needed" | "Couldn't verify" => AMB,
        "Disabled" => FT,
        _ => DIM,
    }
}

fn visible_servers(registry: &Registry) -> Vec<&ServerProfile> {
    let mut servers = registry
        .servers()
        .iter()
        .filter(|server| !server.archived)
        .collect::<Vec<_>>();
    servers.sort_by_key(|server| server.server_id != "local");
    servers
}

fn harness_title(hkey: &str) -> &str {
    match hkey {
        "claude" => "Claude Code",
        "codex" => "Codex",
        "opencode" => "OpenCode",
        "grok" => "Grok",
        _ => hkey,
    }
}

fn wrap(text: &str, width: usize) -> Vec<String> {
    let mut lines = Vec::new();
    let mut line = String::new();
    for word in text.split_whitespace() {
        if !line.is_empty() && text_width(&line) + 1 + text_width(word) > width {
            lines.push(line);
            line = String::new();
        }
        if !line.is_empty() {
            line.push(' ');
        }
        line.push_str(word);
    }
    if !line.is_empty() {
        lines.push(line);
    }
    lines
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::remote_probes::AuthState;
    use taskai_core::registry::Source;
    use taskai_tui::seg::{cell_count, strip_sgr};

    fn key(code: KeyCode) -> KeyEvent {
        KeyEvent::new(code, KeyModifiers::NONE)
    }

    fn plain(frame: &[String]) -> String {
        frame
            .iter()
            .map(|line| strip_sgr(line))
            .collect::<Vec<_>>()
            .join("\n")
    }

    fn click_footer(
        settings: &mut Settings,
        registry: &Registry,
        code: KeyCode,
        cols: usize,
        rows: usize,
    ) -> Option<Action> {
        let frame = settings.frame(registry, None, cols, rows);
        let hit = settings
            .hits
            .iter()
            .find(|hit| matches!(&hit.kind, HitKind::Key(key) if key.code == code))
            .cloned()
            .expect("shortcut must have a rendered hit target");
        assert!(
            hit.x + hit.width <= cols,
            "shortcut clipped at {cols}x{rows}"
        );
        assert!(hit.y < frame.len(), "shortcut below frame at {cols}x{rows}");
        settings.mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: hit.x as u16,
                row: hit.y as u16,
                modifiers: KeyModifiers::NONE,
            },
            registry,
            cols,
            rows,
        )
    }

    #[test]
    fn failed_add_server_can_cancel_by_escape_or_click_then_go_back() {
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            for click in [false, true] {
                for failure in ["name", "alias", "root", "duplicate"] {
                    let root = tempfile::tempdir().unwrap();
                    let mut registry = Registry::open(root.path()).unwrap();
                    if failure == "duplicate" {
                        registry
                            .create_server(
                                "Existing",
                                ConnectionTarget::ssh("fixture").unwrap(),
                                None,
                                Source::HumanUi,
                            )
                            .unwrap();
                    }
                    let count = registry.servers().len();
                    let mut settings = Settings::new();
                    settings.handle(key(KeyCode::Char('n')), &registry);
                    if failure != "name" {
                        settings.paste("Draft server");
                    }
                    settings.handle(key(KeyCode::Tab), &registry);
                    settings.paste(if failure == "alias" {
                        "fixture; exit"
                    } else {
                        "fixture"
                    });
                    settings.handle(key(KeyCode::Tab), &registry);
                    settings.paste(if failure == "root" {
                        "relative/path"
                    } else {
                        "/srv/projects"
                    });
                    if let Some(action) = settings.handle(key(KeyCode::Enter), &registry) {
                        let Action::Create {
                            name,
                            target,
                            repository_root,
                        } = action
                        else {
                            panic!("unexpected save action");
                        };
                        let error = registry
                            .create_server(&name, target, repository_root, Source::HumanUi)
                            .expect_err("fixture must fail validation or persistence");
                        settings.set_status(error.to_string());
                    }
                    assert!(settings.status.is_some(), "missing {failure} error");
                    assert!(settings.has_dialog());
                    let cancel = if click {
                        click_footer(&mut settings, &registry, KeyCode::Esc, cols, rows)
                    } else {
                        settings.handle(key(KeyCode::Esc), &registry)
                    };
                    assert!(cancel.is_none(), "Cancel must return to Settings first");
                    assert!(!settings.has_dialog());
                    assert!(settings.status.is_none());
                    assert!(!settings.editor.selected);
                    assert!(settings.editor.cursor.is_none());
                    assert_eq!(registry.servers().len(), count);
                    let back = if click {
                        click_footer(&mut settings, &registry, KeyCode::Esc, cols, rows)
                    } else {
                        settings.handle(key(KeyCode::Esc), &registry)
                    };
                    assert!(matches!(back, Some(Action::Back)));
                }
            }
        }
    }

    #[test]
    fn canonical_settings_shortcuts_are_literal_inside_profile_fields() {
        let root = tempfile::tempdir().unwrap();
        let registry = Registry::open(root.path()).unwrap();
        let mut settings = Settings::new();
        for code in [KeyCode::Char('s'), KeyCode::Char('h')] {
            assert!(settings.handle(key(code), &registry).is_none());
            assert!(!settings.has_dialog());
        }
        settings.handle(key(KeyCode::Char('n')), &registry);
        for field in 0..3 {
            for ch in "sht?qG".chars() {
                assert!(settings.handle(key(KeyCode::Char(ch)), &registry).is_none());
            }
            assert_eq!(settings.text_field().unwrap(), "sht?qG");
            assert!(settings.handle(key(KeyCode::F(1)), &registry).is_none());
            assert!(settings.has_dialog());
            if field < 2 {
                settings.handle(key(KeyCode::Tab), &registry);
            }
        }
        settings.handle(key(KeyCode::Esc), &registry);
        assert!(!settings.has_dialog());
    }

    #[test]
    fn canonical_settings_footer_actions_match_keyboard_and_stay_visible() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(root.path()).unwrap();
        let id = registry
            .create_server(
                "Fixture server",
                ConnectionTarget::ssh("fixture").unwrap(),
                None,
                Source::HumanUi,
            )
            .unwrap();
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            for click in [false, true] {
                for code in [
                    KeyCode::Char('s'),
                    KeyCode::Char('t'),
                    KeyCode::Char('?'),
                    KeyCode::F(1),
                    KeyCode::Char('G'),
                    KeyCode::Char('q'),
                    KeyCode::Char('e'),
                    KeyCode::Enter,
                    KeyCode::Char('d'),
                ] {
                    let mut settings = Settings::new();
                    settings.select_server(&id);
                    let frame = plain(&settings.frame(&registry, None, cols, rows));
                    for label in [
                        "Edit server",
                        "Edit harness",
                        "Archive server",
                        "Settings",
                        "Terminal",
                        "Help",
                        "Setup helper",
                        "Home",
                        "Leave",
                    ] {
                        assert!(frame.contains(label), "missing {label} at {cols}x{rows}");
                    }
                    let action = if click {
                        click_footer(&mut settings, &registry, code, cols, rows)
                    } else {
                        settings.handle(key(code), &registry)
                    };
                    match code {
                        KeyCode::Char('s') => {
                            assert!(action.is_none());
                            assert!(!settings.has_dialog());
                        }
                        KeyCode::Char('t') => assert!(
                            matches!(action, Some(Action::OpenTerminal { server_id }) if server_id == id)
                        ),
                        KeyCode::Char('?') => assert!(matches!(action, Some(Action::Help))),
                        KeyCode::F(1) => assert!(
                            matches!(action, Some(Action::OpenHelper { server_id }) if server_id == id)
                        ),
                        KeyCode::Char('G') => assert!(matches!(action, Some(Action::Home))),
                        KeyCode::Char('q') => assert!(matches!(action, Some(Action::Leave))),
                        KeyCode::Char('e') => assert!(matches!(
                            settings.dialog,
                            Some(Dialog::Profile {
                                server_id: Some(_),
                                ..
                            })
                        )),
                        KeyCode::Enter => {
                            assert!(matches!(settings.dialog, Some(Dialog::Harness { .. })))
                        }
                        KeyCode::Char('d') => {
                            assert!(matches!(settings.dialog, Some(Dialog::Archive { .. })))
                        }
                        _ => unreachable!(),
                    }
                }
            }
        }
    }

    #[test]
    fn settings_frames_are_exact_and_never_infer_authentication_from_versions() {
        let root = tempfile::tempdir().unwrap();
        let registry = Registry::open(root.path()).unwrap();
        let check = ServerCheck {
            choices: HARNESS_KEYS
                .iter()
                .map(|hkey| HarnessAvailability {
                    hkey: (*hkey).into(),
                    version: Some("2.1.226".into()),
                    unavailable: None,
                    auth: AuthState::Unknown,
                    messaging: None,
                })
                .collect(),
            checking: false,
            error: None,
        };
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            let mut settings = Settings::new();
            let frame = settings.frame(&registry, Some(&check), cols, rows);
            assert_eq!(frame.len(), rows);
            assert!(frame.iter().all(|line| cell_count(line) == cols));
            let text = plain(&frame);
            if std::env::var_os("TASKAI_SETTINGS_PREVIEW").is_some() {
                println!("Settings overview {cols}x{rows}\n{text}");
            }
            for label in [
                "This computer",
                "Claude Code",
                "Codex",
                "OpenCode",
                "Grok",
                "STATUS",
                "VERSION",
                "Couldn't verify",
                "Sign-in",
                "Messaging   Not checked · F5 checks",
                "Native default",
                "F5",
                "Configure CLI",
                "Setup helper",
            ] {
                assert!(text.contains(label), "missing {label} at {cols}x{rows}");
            }
            assert!(!text.contains("Authenticated"));
            assert!(!text.contains("COMPATIBILITY"));
            assert!(!text.contains("ENABLED"));
            for provider in ["Claude Code", "Codex", "OpenCode", "Grok"] {
                let provider_row = frame
                    .iter()
                    .map(|line| strip_sgr(line))
                    .find(|line| line.contains(provider) && line.contains("Couldn't verify"))
                    .unwrap();
                assert!(provider_row.contains("2.1.226"));
                assert!(!provider_row.contains("Not checked"));
            }
            settings.handle(key(KeyCode::Enter), &registry);
            let frame = settings.frame(&registry, Some(&check), cols, rows);
            if std::env::var_os("TASKAI_SETTINGS_PREVIEW").is_some() {
                println!("Harness defaults {cols}x{rows}\n{}", plain(&frame));
            }
            assert!(frame.iter().all(|line| cell_count(line) == cols));
            assert!(plain(&frame).contains("DEFAULT MODEL"));
            assert!(plain(&frame).contains("[Clear]"));
            assert!(!plain(&frame).contains("Version is compatible"));
            assert!(!plain(&frame).contains("Tab switches server/provider"));
        }
    }

    // Expectation: the details show the messaging gate's own line — its reason and remedy
    // wrapped, `No adapter` for OpenCode — while the table keeps the launch status, so a drifted
    // Claude still reads Ready to launch. Why: build-model M7 — Settings shows the same readiness
    // result as doctor, and a failed check disables one adapter, not the provider. Value: the
    // person sees exactly what to do without losing native launches.
    #[test]
    fn messaging_line_shows_the_gate_without_changing_launch_status() {
        use crate::doctor::Check;
        let root = tempfile::tempdir().unwrap();
        let registry = Registry::open(root.path()).unwrap();
        let check = ServerCheck {
            choices: vec![
                HarnessAvailability {
                    hkey: "claude".into(),
                    version: Some("2.1.288".into()),
                    unavailable: None,
                    auth: AuthState::SignedIn,
                    messaging: Some(Check::NotReady {
                        reason: "Claude Code 2.1.288 is not the tested 2.1.289".into(),
                        remedy: "Install Claude Code 2.1.289 before enabling it".into(),
                    }),
                },
                HarnessAvailability {
                    hkey: "opencode".into(),
                    version: Some("1.18.0".into()),
                    unavailable: None,
                    auth: AuthState::Unknown,
                    messaging: None,
                },
            ],
            checking: false,
            error: None,
        };
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            let mut settings = Settings::new();
            let frame = settings.frame(&registry, Some(&check), cols, rows);
            assert!(frame.iter().all(|line| cell_count(line) == cols));
            let text = plain(&frame);
            assert!(
                text.lines().any(|line| line.contains("Claude Code")
                    && line.contains("Ready")
                    && line.contains("2.1.288")),
                "launch status must stay Ready at {cols}x{rows}:\n{text}"
            );
            assert!(
                text.contains("Messaging   Not ready · Claude Code 2.1.288 is not the tested"),
                "{text}"
            );
            assert!(text.contains("Install Claude Code 2.1.289"), "{text}");
            settings.harness = 2;
            let text = plain(&settings.frame(&registry, Some(&check), cols, rows));
            assert!(
                text.contains("Messaging   No adapter for OpenCode"),
                "{text}"
            );
        }
        let checking = ServerCheck {
            checking: true,
            ..ServerCheck::default()
        };
        let mut settings = Settings::new();
        let text = plain(&settings.frame(&registry, Some(&checking), 110, 30));
        assert!(text.contains("Messaging   Checking…"), "{text}");
    }

    #[test]
    fn provider_state_overrides_do_not_reuse_stale_ready_claims() {
        let checking = ServerCheck {
            checking: true,
            ..ServerCheck::default()
        };
        assert_eq!(provider_status(false, Some(&checking), None), "Disabled");
        assert_eq!(provider_status(true, Some(&checking), None), "Checking");
        let failed = ServerCheck {
            error: Some("SSH could not verify host".into()),
            ..ServerCheck::default()
        };
        assert_eq!(
            provider_status(true, Some(&failed), None),
            "Couldn't verify"
        );
        assert_eq!(provider_status(true, None, None), "Couldn't verify");
        let known_ready = HarnessAvailability {
            hkey: "claude".into(),
            version: Some("2.1.226".into()),
            unavailable: None,
            auth: AuthState::SignedIn,
            messaging: None,
        };
        assert_eq!(provider_status(true, None, Some(&known_ready)), "Ready");
        assert_eq!(
            provider_status(true, Some(&checking), Some(&known_ready)),
            "Checking"
        );
        assert_eq!(
            provider_status(true, Some(&failed), Some(&known_ready)),
            "Couldn't verify"
        );
        assert_eq!(provider_status(false, None, Some(&known_ready)), "Disabled");
    }

    #[test]
    fn single_status_distinguishes_login_and_unknown_without_changing_launch_compatibility() {
        let root = tempfile::tempdir().unwrap();
        let registry = Registry::open(root.path()).unwrap();
        for (auth, expected) in [
            (AuthState::SignedIn, "Ready"),
            (AuthState::SignedOut, "Sign in needed"),
            (AuthState::Unknown, "Couldn't verify"),
        ] {
            let choice = HarnessAvailability {
                hkey: "claude".into(),
                version: Some("2.1.226".into()),
                unavailable: None,
                auth,
                messaging: None,
            };
            assert!(
                choice.is_ready(),
                "Auth UI must not silently disable launch compatibility"
            );
            let check = ServerCheck {
                choices: vec![choice],
                checking: false,
                error: None,
            };
            for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
                let mut settings = Settings::new();
                let frame = settings.frame(&registry, Some(&check), cols, rows);
                assert_eq!(frame.len(), rows);
                assert!(frame.iter().all(|line| cell_count(line) == cols));
                let text = plain(&frame);
                assert!(
                    text.lines().any(|line| line.contains("Claude Code")
                        && line.contains(expected)
                        && line.contains("2.1.226")),
                    "missing {expected} at {cols}x{rows}"
                );
                assert!(text.contains("Model access isn't tested here"));
            }
        }
    }

    #[test]
    fn signed_in_never_overrides_update_or_unsupported_status_in_the_table() {
        let root = tempfile::tempdir().unwrap();
        let registry = Registry::open(root.path()).unwrap();
        for (hkey, version, detail, expected, selected) in [
            ("grok", "0.2.117", "Installed version 0.2.117; this connector requires 1.0.0 or newer", "Update needed", 3),
            ("opencode", "1.18.0", "Remote OpenCode is not supported yet: its shared-live serve/attach launcher currently requires local TaskAI. Local OpenCode remains available.", "Unsupported", 2),
        ] {
            let check = ServerCheck {
                choices: vec![HarnessAvailability {
                    hkey: hkey.into(),
                    version: Some(version.into()),
                    unavailable: Some(detail.into()),
                    auth: AuthState::SignedIn,
                    messaging: None,
                }],
                checking: false,
                error: None,
            };
            for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
                let mut settings = Settings::new();
                settings.harness = selected;
                let frame = settings.frame(&registry, Some(&check), cols, rows);
                let text = plain(&frame);
                assert!(frame.iter().all(|line| cell_count(line) == cols));
                assert!(text.lines().any(|line| line.contains(harness_title(hkey)) && line.contains(expected) && line.contains(version)), "missing {expected} at {cols}x{rows}");
                assert!(!text.lines().any(|line| line.contains(harness_title(hkey)) && line.contains("Ready")));
            }
        }
    }

    #[test]
    fn setup_helper_action_is_visible_and_clickable_for_the_selected_server() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(root.path()).unwrap();
        let id = registry
            .create_server(
                "Build server",
                ConnectionTarget::ssh("build").unwrap(),
                None,
                Source::HumanUi,
            )
            .unwrap();
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            let mut settings = Settings::new();
            settings.select_server(&id);
            assert!(
                matches!(settings.handle(key(KeyCode::F(1)), &registry), Some(Action::OpenHelper { server_id }) if server_id == id)
            );
            let frame = settings.frame(&registry, None, cols, rows);
            assert!(plain(&frame).contains("Setup helper"));
            let hit = settings
                .hits
                .iter()
                .find(|hit| {
                    matches!(
                        hit.kind,
                        HitKind::Key(KeyEvent {
                            code: KeyCode::F(1),
                            ..
                        })
                    )
                })
                .cloned()
                .unwrap();
            let action = settings.mouse(
                MouseEvent {
                    kind: MouseEventKind::Down(MouseButton::Left),
                    column: hit.x as u16,
                    row: hit.y as u16,
                    modifiers: KeyModifiers::NONE,
                },
                &registry,
                cols,
                rows,
            );
            assert!(matches!(action, Some(Action::OpenHelper { server_id }) if server_id == id));
        }
    }

    #[test]
    fn absent_probe_result_is_not_reported_as_compatible_and_control_keys_do_not_launch() {
        let root = tempfile::tempdir().unwrap();
        let registry = Registry::open(root.path()).unwrap();
        let mut settings = Settings::new();
        let frame = settings.frame(&registry, Some(&ServerCheck::default()), 110, 30);
        let text = plain(&frame);
        assert!(!text.contains("Version is compatible"));
        assert!(text.contains("Provider not checked yet"));
        assert!(settings
            .handle(
                KeyEvent::new(KeyCode::Char('c'), KeyModifiers::CONTROL),
                &registry
            )
            .is_none());
        assert!(
            matches!(settings.handle(key(KeyCode::Char('c')), &registry),
            Some(Action::Configure { server_id, hkey }) if server_id == "local" && hkey == "claude")
        );
    }

    #[test]
    fn server_list_pins_local_and_scrolls_selected_profile_with_correct_mouse_target() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(root.path()).unwrap();
        let mut last = String::new();
        for index in 0..35 {
            last = registry
                .create_server(
                    &format!("Server {index:02}"),
                    ConnectionTarget::ssh(format!("host-{index}")).unwrap(),
                    None,
                    Source::HumanUi,
                )
                .unwrap();
        }
        for (cols, rows) in [(110, 30), (150, 42), (209, 51)] {
            let mut settings = Settings::new();
            settings.select_server(&last);
            let frame = settings.frame(&registry, None, cols, rows);
            let text = plain(&frame);
            assert!(text.contains("This computer"));
            assert!(text.contains("Server 34"));
            assert!(frame.iter().all(|line| cell_count(line) == cols));
            let local = settings
                .hits
                .iter()
                .find(|hit| matches!(&hit.kind, HitKind::Server(id) if id == "local"))
                .cloned()
                .unwrap();
            settings.mouse(
                MouseEvent {
                    kind: MouseEventKind::Down(MouseButton::Left),
                    column: 3,
                    row: local.y as u16,
                    modifiers: KeyModifiers::NONE,
                },
                &registry,
                cols,
                rows,
            );
            assert_eq!(
                settings.selected_server_id(&registry).as_deref(),
                Some("local")
            );
        }
    }

    #[test]
    fn profile_save_is_explicit_and_errors_preserve_the_draft_and_immutable_target() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(root.path()).unwrap();
        let mut settings = Settings::new();
        settings.handle(key(KeyCode::Char('n')), &registry);
        settings.paste("Cruise build");
        settings.handle(key(KeyCode::Tab), &registry);
        settings.paste("ssh cruise4b");
        settings.handle(key(KeyCode::Tab), &registry);
        settings.paste("/home/ubuntu/github");
        let action = settings.handle(key(KeyCode::Enter), &registry).unwrap();
        assert!(settings.dialog.is_some());
        assert_eq!(registry.servers().len(), 1);
        settings.set_status("Save failed; retry");
        assert!(plain(&settings.frame(&registry, None, 110, 30)).contains("Cruise build"));
        let Action::Create {
            name,
            target,
            repository_root,
        } = action
        else {
            panic!("wrong action");
        };
        assert_eq!(target, ConnectionTarget::ssh("cruise4b").unwrap());
        assert_eq!(repository_root, Some(PathBuf::from("/home/ubuntu/github")));
        let id = registry
            .create_server(&name, target, repository_root, Source::HumanUi)
            .unwrap();
        settings.saved(&id);
        assert!(settings.dialog.is_none());
        settings.handle(key(KeyCode::Char('e')), &registry);
        settings.handle(key(KeyCode::Tab), &registry);
        assert!(matches!(
            &settings.dialog,
            Some(Dialog::Profile { field: 2, .. })
        ));
        let frame = settings.frame(&registry, None, 110, 30);
        assert!(plain(&frame).contains("CONNECTION · READ ONLY"));
        assert!(!settings
            .hits
            .iter()
            .any(|hit| matches!(hit.kind, HitKind::Field(1))));
    }

    #[test]
    fn harness_defaults_return_values_without_modifying_running_state() {
        let root = tempfile::tempdir().unwrap();
        let registry = Registry::open(root.path()).unwrap();
        let mut settings = Settings::new();
        settings.handle(key(KeyCode::Enter), &registry);
        settings.handle(key(KeyCode::Char(' ')), &registry);
        settings.handle(key(KeyCode::Tab), &registry);
        settings.paste("/opt/tools/claude");
        settings.handle(key(KeyCode::Tab), &registry);
        settings.paste("provider/model");
        let action = settings.handle(key(KeyCode::Enter), &registry).unwrap();
        let Action::SaveHarness {
            server_id,
            hkey,
            settings: saved,
        } = action
        else {
            panic!("wrong action");
        };
        assert_eq!(server_id, "local");
        assert_eq!(hkey, "claude");
        assert!(!saved.enabled);
        assert_eq!(saved.executable.as_deref(), Some("/opt/tools/claude"));
        assert_eq!(saved.model.as_deref(), Some("provider/model"));
        assert!(registry.server("local").unwrap().harnesses.is_empty());
        settings.saved("local");
        settings.harness = 2;
        settings.handle(key(KeyCode::Enter), &registry);
        settings.handle(key(KeyCode::Tab), &registry);
        settings.handle(key(KeyCode::Tab), &registry);
        settings.paste("not-supported");
        assert!(settings.handle(key(KeyCode::Enter), &registry).is_none());
        assert!(settings
            .status
            .as_deref()
            .unwrap()
            .contains("native model default"));
        assert!(settings.dialog.is_some());
    }

    #[test]
    fn mouse_clear_and_footer_actions_use_the_rendered_geometry() {
        let root = tempfile::tempdir().unwrap();
        let registry = Registry::open(root.path()).unwrap();
        let mut settings = Settings::new();
        settings.handle(key(KeyCode::Char('n')), &registry);
        settings.paste("Long draft name");
        let frame = settings.frame(&registry, None, 110, 30);
        assert!(plain(&frame).contains("Select all"));
        assert!(!plain(&frame).contains("Space  Toggle"));
        let hit = settings
            .hits
            .iter()
            .find(|hit| matches!(hit.kind, HitKind::Field(0)))
            .cloned()
            .unwrap();
        settings.mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: (hit.x + hit.width - 1) as u16,
                row: hit.y as u16,
                modifiers: KeyModifiers::NONE,
            },
            &registry,
            110,
            30,
        );
        assert_eq!(settings.text_field().unwrap(), "Long draft name");
        settings.mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: (hit.x + hit.width - 8) as u16,
                row: hit.y as u16,
                modifiers: KeyModifiers::NONE,
            },
            &registry,
            110,
            30,
        );
        assert_eq!(settings.text_field().unwrap(), "");
        settings.paste("Replace this");
        let select_all = settings
            .hits
            .iter()
            .find(|hit| {
                matches!(
                    hit.kind,
                    HitKind::Key(KeyEvent {
                        code: KeyCode::Char('a'),
                        modifiers: KeyModifiers::CONTROL,
                        ..
                    })
                )
            })
            .cloned()
            .unwrap();
        settings.mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: select_all.x as u16,
                row: select_all.y as u16,
                modifiers: KeyModifiers::NONE,
            },
            &registry,
            110,
            30,
        );
        settings.paste("Replacement");
        assert_eq!(settings.text_field().unwrap(), "Replacement");
        settings.handle(key(KeyCode::Esc), &registry);
        settings.frame(&registry, None, 110, 30);
        let check = settings
            .hits
            .iter()
            .find(|hit| {
                matches!(
                    hit.kind,
                    HitKind::Key(KeyEvent {
                        code: KeyCode::F(5),
                        ..
                    })
                )
            })
            .cloned()
            .unwrap();
        let action = settings.mouse(
            MouseEvent {
                kind: MouseEventKind::Down(MouseButton::Left),
                column: check.x as u16,
                row: check.y as u16,
                modifiers: KeyModifiers::NONE,
            },
            &registry,
            110,
            30,
        );
        assert!(matches!(action, Some(Action::Check { server_id }) if server_id == "local"));
    }

    #[test]
    fn archive_requires_confirmation_and_cannot_remove_this_computer() {
        let root = tempfile::tempdir().unwrap();
        let mut registry = Registry::open(root.path()).unwrap();
        let mut settings = Settings::new();
        assert!(settings
            .handle(key(KeyCode::Char('d')), &registry)
            .is_none());
        assert!(settings.dialog.is_none());
        let id = registry
            .create_server(
                "Old build host",
                ConnectionTarget::ssh("old-build").unwrap(),
                None,
                Source::HumanUi,
            )
            .unwrap();
        settings.select_server(&id);
        assert!(settings
            .handle(key(KeyCode::Char('d')), &registry)
            .is_none());
        let frame = settings.frame(&registry, None, 110, 30);
        assert!(plain(&frame).contains("running sessions are untouched"));
        assert!(
            matches!(settings.handle(key(KeyCode::Enter), &registry), Some(Action::Archive { server_id }) if server_id == id)
        );
        assert!(!registry.server(&id).unwrap().archived);
    }
}
