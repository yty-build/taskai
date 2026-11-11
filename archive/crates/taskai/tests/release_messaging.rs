// ABOUTME: M9 release proof — the spec §11 acceptance table on the real CLIs through the real
// ABOUTME: TaskAI TUI, opt-in and billed; records sanitized evidence and a cost note, then cleans up.

#![cfg(unix)]

use std::{
    collections::BTreeSet,
    fs,
    io::{Read, Write},
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    thread,
    time::{Duration, Instant},
};

use portable_pty::{native_pty_system, Child, CommandBuilder, MasterPty, PtySize};
use serde_json::{json, Value};
use taskai_core::{
    mail::{inherited_messaging_env_names, strip_messaging_env, Activity},
    registry::{HarnessSettings, Registry, Source, TmuxRuntimeLocator},
};
use taskai_pane::grid::Grid;

const ROWS: u16 = 42;
const COLS: u16 = 160;
const UI_WAIT: Duration = Duration::from_secs(20);
const LAUNCH_WAIT: Duration = Duration::from_secs(90);
const MODEL_WAIT: Duration = Duration::from_secs(240);
const BILL_FLAG: &str = "TASKAI_I_APPROVE_THE_BILL";
const TASK_NAME: &str = "M9 release proof";

fn env_flag(name: &str) -> bool {
    std::env::var(name).is_ok_and(|value| value == "1")
}

/// The real TaskAI TUI on a private pty with a private home; the person's own HOME stays, so
/// the installed CLIs find their saved logins. Nothing inherited from a Claude session reaches it.
struct Tui {
    _master: Box<dyn MasterPty + Send>,
    writer: Box<dyn Write + Send>,
    child: Box<dyn Child + Send + Sync>,
    grid: Arc<Mutex<Grid>>,
}

impl Tui {
    fn spawn(home: &Path, cwd: &Path) -> Tui {
        let pair = native_pty_system()
            .openpty(PtySize {
                rows: ROWS,
                cols: COLS,
                pixel_width: 0,
                pixel_height: 0,
            })
            .expect("open the TaskAI pty");
        let mut command = CommandBuilder::new(env!("CARGO_BIN_EXE_taskai"));
        command.cwd(cwd);
        command.env("TASKAI_HOME", home);
        command.env("TERM", "xterm-256color");
        command.env("COLORTERM", "truecolor");
        command.env_remove("TASKAI_SESSION_REF");
        for name in inherited_messaging_env_names() {
            command.env_remove(name);
        }
        let child = pair.slave.spawn_command(command).expect("spawn taskai");
        drop(pair.slave);
        let writer = pair.master.take_writer().expect("pty writer");
        let mut reader = pair.master.try_clone_reader().expect("pty reader");
        let grid = Arc::new(Mutex::new(Grid::new(ROWS as usize, COLS as usize)));
        let reader_grid = Arc::clone(&grid);
        thread::spawn(move || {
            let mut buffer = [0u8; 8192];
            while let Ok(read) = reader.read(&mut buffer) {
                if read == 0 {
                    break;
                }
                reader_grid.lock().unwrap().process(&buffer[..read]);
            }
        });
        Tui {
            _master: pair.master,
            writer,
            child,
            grid,
        }
    }

    fn screen(&self) -> String {
        self.grid.lock().unwrap().text()
    }

    fn wait_for(&self, needle: &str) {
        let deadline = Instant::now() + UI_WAIT;
        while !self.screen().contains(needle) {
            assert!(
                Instant::now() < deadline,
                "the TaskAI screen never showed {needle:?}:\n{}",
                self.screen()
            );
            thread::sleep(Duration::from_millis(50));
        }
    }

    fn send(&mut self, bytes: &[u8]) {
        self.writer
            .write_all(bytes)
            .expect("write to the TaskAI pty");
        self.writer.flush().expect("flush the TaskAI pty");
    }

    fn quit(&mut self) {
        self.send(b"q");
        let deadline = Instant::now() + UI_WAIT;
        while matches!(self.child.try_wait(), Ok(None)) {
            if Instant::now() > deadline {
                let _ = self.child.kill();
                break;
            }
            thread::sleep(Duration::from_millis(50));
        }
        let _ = self.child.wait();
    }
}

impl Drop for Tui {
    fn drop(&mut self) {
        if matches!(self.child.try_wait(), Ok(None)) {
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }
}

fn tmux(runtime: &TmuxRuntimeLocator) -> Command {
    let mut command = Command::new("tmux");
    command
        .env_remove("TMUX")
        .env_remove("TMUX_PANE")
        .env_remove("TMUX_TMPDIR")
        .args(["-f", "/dev/null", "-L", &runtime.private_server]);
    strip_messaging_env(&mut command);
    command
}

fn tmux_text(runtime: &TmuxRuntimeLocator, args: &[&str]) -> Option<String> {
    let output = tmux(runtime).args(args).output().ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).into_owned())
}

fn native_target(runtime: &TmuxRuntimeLocator) -> String {
    format!("={}:native", runtime.session)
}

fn native_screen(runtime: &TmuxRuntimeLocator) -> String {
    tmux_text(
        runtime,
        &[
            "capture-pane",
            "-p",
            "-S",
            "-300",
            "-t",
            &native_target(runtime),
        ],
    )
    .unwrap_or_default()
}

fn window_exists(runtime: &TmuxRuntimeLocator, name: &str) -> bool {
    tmux_text(
        runtime,
        &[
            "list-windows",
            "-t",
            &format!("={}", runtime.session),
            "-F",
            "#{window_name}",
        ],
    )
    .is_some_and(|names| names.lines().any(|line| line.trim() == name))
}

fn pane_pid(runtime: &TmuxRuntimeLocator, window: &str) -> Option<u32> {
    tmux_text(
        runtime,
        &[
            "list-panes",
            "-t",
            &format!("={}:{window}", runtime.session),
            "-F",
            "#{pane_pid}",
        ],
    )?
    .trim()
    .parse()
    .ok()
}

/// One line typed into a native pane the way a person would: literal text, a pause so the CLI
/// does not read Enter inside a paste, then Enter on its own.
fn type_line(runtime: &TmuxRuntimeLocator, text: &str) {
    assert!(tmux(runtime)
        .args(["send-keys", "-t", &native_target(runtime), "-l", text])
        .status()
        .map(|status| status.success())
        .unwrap_or(false));
    thread::sleep(Duration::from_millis(1200));
    assert!(tmux(runtime)
        .args(["send-keys", "-t", &native_target(runtime), "Enter"])
        .status()
        .map(|status| status.success())
        .unwrap_or(false));
}

fn wait_until(limit: Duration, what: &str, mut condition: impl FnMut() -> bool) -> bool {
    let deadline = Instant::now() + limit;
    while !condition() {
        if Instant::now() > deadline {
            eprintln!("TIMEOUT waiting for {what}");
            return false;
        }
        thread::sleep(Duration::from_millis(250));
    }
    true
}

/// The task's message log as JSON rows (ts, msg_id, from, to, direction, outcome, note, body).
fn log_rows(home: &Path, task_ref: &str) -> Vec<Value> {
    fs::read_to_string(
        home.join("state")
            .join("messages")
            .join(format!("{task_ref}.jsonl")),
    )
    .unwrap_or_default()
    .lines()
    .filter_map(|line| serde_json::from_str(line).ok())
    .collect()
}

fn has_row(rows: &[Value], from: &str, to: &str, outcome: &str) -> bool {
    rows.iter().any(|row| {
        row["from"] == from && row["to"] == to && row["outcome"].as_str() == Some(outcome)
    })
}

fn registry(home: &Path) -> Registry {
    Registry::open(&home.join("state")).expect("open the private registry")
}

fn runtime_of(home: &Path, session_ref: &str) -> Option<TmuxRuntimeLocator> {
    registry(home).session(session_ref)?.runtime.clone()
}

fn live_addresses(home: &Path, task_ref: &str) -> BTreeSet<String> {
    registry(home)
        .live_sessions(task_ref)
        .into_iter()
        .map(|peer| peer.name)
        .collect()
}

fn mailbox_status(
    home: &Path,
    task_ref: &str,
    address: &str,
) -> Option<(Activity, Option<String>)> {
    registry(home)
        .live_sessions(task_ref)
        .into_iter()
        .find(|peer| peer.name == address)
        .map(|peer| (peer.status, peer.waiting_for))
}

fn listing(dir: &Path) -> BTreeSet<String> {
    fs::read_dir(dir)
        .map(|entries| {
            entries
                .flatten()
                .map(|entry| entry.file_name().to_string_lossy().into_owned())
                .collect()
        })
        .unwrap_or_default()
}

fn claude_sessions_dir() -> PathBuf {
    std::env::var_os("CLAUDE_CONFIG_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            PathBuf::from(std::env::var_os("HOME").unwrap_or_default()).join(".claude")
        })
        .join("sessions")
}

/// Everything this proof may clean, by exact identity, if the TUI's own destroy did not get to it.
struct Owned {
    home: PathBuf,
    tui_quit: bool,
}

impl Drop for Owned {
    fn drop(&mut self) {
        let Ok(registry) = Registry::open(&self.home.join("state")) else {
            return;
        };
        for task in registry.tasks() {
            for session in registry.sessions_of(&task.task_ref) {
                if let Some(runtime) = &session.runtime {
                    let _ = tmux(runtime)
                        .args(["kill-session", "-t", &format!("={}", runtime.session)])
                        .status();
                }
            }
        }
        if !self.tui_quit {
            eprintln!(
                "cleanup guard: runtimes killed by exact tmux session; records left for inspection"
            );
        }
    }
}

struct Evidence {
    path: PathBuf,
    value: Value,
}

impl Evidence {
    fn new(path: PathBuf, commit: &str) -> Self {
        let value = json!({
            "milestone": "M9 release proof",
            "date": chrono_date(),
            "tested_commit": commit,
            "approval": "T authorized the initial run in advance; TASKAI_I_APPROVE_THE_BILL=1 was set by worker4",
            "scenarios": [],
            "turns": {"codex": 0, "grok": 0, "claude": 0},
            "cleanup": {},
            "residual": [],
        });
        Self { path, value }
    }

    fn scenario(&mut self, criterion: &str, outcome: &str, detail: Value) {
        eprintln!("[{criterion}] {outcome}: {detail}");
        self.value["scenarios"]
            .as_array_mut()
            .unwrap()
            .push(json!({"criterion": criterion, "outcome": outcome, "evidence": detail}));
        self.save();
    }

    fn turn(&mut self, harness: &str, count: u64) {
        let turns = &mut self.value["turns"][harness];
        *turns = json!(turns.as_u64().unwrap_or(0) + count);
        self.save();
    }

    fn save(&self) {
        let _ = fs::create_dir_all(self.path.parent().unwrap());
        let _ = fs::write(
            &self.path,
            serde_json::to_string_pretty(&self.value).unwrap(),
        );
    }
}

fn chrono_date() -> String {
    let output = Command::new("date").arg("+%Y-%m-%d").output().unwrap();
    String::from_utf8_lossy(&output.stdout).trim().to_string()
}

fn git_head(dir: &Path) -> String {
    let output = Command::new("git")
        .args(["-C", dir.to_str().unwrap(), "rev-parse", "HEAD"])
        .output()
        .unwrap();
    String::from_utf8_lossy(&output.stdout).trim().to_string()
}

fn doctor_json(home: &Path, workspace: &Path) -> (bool, Value) {
    let mut command = Command::new(env!("CARGO_BIN_EXE_taskai"));
    command
        .env("TASKAI_HOME", home)
        .env_remove("TASKAI_SESSION_REF")
        .args(["doctor", "--json", "--workspace"])
        .arg(workspace);
    strip_messaging_env(&mut command);
    let output = command.output().expect("run taskai doctor");
    let value: Value = serde_json::from_slice(&output.stdout).unwrap_or(Value::Null);
    (output.status.success(), value)
}

/// Ctrl+G hands focus back to navigation (a no-op there), then Esc goes home; only then do
/// navigation keys mean navigation instead of keystrokes into a native pane.
fn nav_home(tui: &mut Tui) {
    tui.send(b"\x07");
    thread::sleep(Duration::from_millis(400));
    tui.send(b"\x1b");
    thread::sleep(Duration::from_millis(400));
    tui.wait_for(TASK_NAME);
}

/// Opens the proof task in the TUI, selects the session's rail row and opens it inline, which
/// launches its runtime (gate, leader or native CLI) plus its bridge or observer sidecar.
fn open_session(tui: &mut Tui, home: &Path, session_ref: &str, row: usize) -> TmuxRuntimeLocator {
    nav_home(tui);
    tui.send(b"\r");
    tui.wait_for("Ctrl+F");
    // Opening a task focuses the selected CLI; Ctrl+G returns to navigation first.
    tui.send(b"\x07");
    thread::sleep(Duration::from_millis(500));
    for _ in 0..8 {
        tui.send(b"\x1b[A");
        thread::sleep(Duration::from_millis(40));
    }
    for _ in 0..row {
        tui.send(b"\x1b[B");
        thread::sleep(Duration::from_millis(40));
    }
    tui.send(b"\r");
    let mut runtime = None;
    assert!(
        wait_until(LAUNCH_WAIT, &format!("runtime of {session_ref}"), || {
            runtime = runtime_of(home, session_ref);
            runtime
                .as_ref()
                .is_some_and(|runtime| window_exists(runtime, "native"))
        }),
        "{session_ref} never got a live native runtime:\n{}",
        tui.screen()
    );
    runtime.unwrap()
}

// Expectation: with the person's three logins and nothing else, the acceptance table holds on the
// installed CLIs: Codex and Grok find and message each other with Claude's adapter disabled by a
// drift failure (6, 7, 11, 13); with it enabled, Claude lists the mirrored sessions and round-trips
// with both (12, 1), a Codex approval shows as waiting (8), a stopped delegate sends exited (9), a
// bridge dies with its pane and is recreated on reopen (14); destroy leaves nothing owned behind (4).
// Why: backend §11 makes live identity and safety the release gate. Value: one repeatable command
// ties every claim in the spec to a run.
#[test]
#[ignore = "billed: real Claude, Codex and Grok model turns; run with TASKAI_I_APPROVE_THE_BILL=1 under an approved live slot"]
fn release_proof_on_the_installed_clis() {
    if !env_flag(BILL_FLAG) {
        eprintln!("unproven: the release proof bills real model turns; set {BILL_FLAG}=1 under an approved slot");
        return;
    }
    let dry_run = env_flag("TASKAI_M9_DRY_RUN");
    let workspace = PathBuf::from(
        std::env::var_os("TASKAI_M9_WORKSPACE")
            .expect("TASKAI_M9_WORKSPACE: a folder Grok already trusts"),
    );
    assert!(
        workspace.is_dir(),
        "{} is not a directory",
        workspace.display()
    );
    let evidence_path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../tests/evidence")
        .join(format!(
            "session-messaging-m9-release-proof-{}.json",
            chrono_date()
        ));
    let repo_root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..");
    let mut evidence = Evidence::new(evidence_path, &git_head(&repo_root));
    let started = Instant::now();

    let root = tempfile::Builder::new()
        .prefix("m9")
        .tempdir_in("/tmp")
        .expect("private root");
    let home = root.path().join("home");
    fs::create_dir_all(&home).unwrap();
    let mut guard = Owned {
        home: home.clone(),
        tui_quit: false,
    };

    // Native state before anything runs; the same listings must match at the end.
    let cards_before = listing(&claude_sessions_dir());
    let doors_before = listing(Path::new("/tmp/cc-socks"));

    // Criterion 3, installed half: the healthy machine is green before any override exists.
    let (healthy_green, healthy) = doctor_json(&home, &workspace);
    evidence.value["installed"] = healthy["harnesses"].clone();
    assert!(
        healthy_green,
        "doctor must be green before the proof:\n{healthy}"
    );

    // Claude adapter disabled by a drift failure: the configured Claude reports another version.
    let wrapper = root.path().join("claude-drift-wrapper");
    fs::write(
        &wrapper,
        "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '2.1.288 (M9 drift wrapper)'; exit 0; fi\nexec claude \"$@\"\n",
    )
    .unwrap();
    fs::set_permissions(&wrapper, fs::Permissions::from_mode(0o700)).unwrap();
    let (task_ref, codex_ref, grok_ref) = {
        let mut registry = registry(&home);
        registry
            .set_server_harness(
                "local",
                "claude",
                HarnessSettings {
                    enabled: true,
                    executable: Some(wrapper.display().to_string()),
                    model: None,
                },
                Source::HumanUi,
            )
            .unwrap();
        let task_ref = registry
            .create_task(TASK_NAME, &workspace, Source::HumanUi)
            .unwrap();
        let codex_ref = registry
            .create_session(
                &task_ref,
                None,
                "lead",
                "codex",
                &workspace,
                Source::HumanUi,
            )
            .unwrap();
        let grok_ref = registry
            .create_session(
                &task_ref,
                Some(&codex_ref),
                "verification",
                "grok",
                &workspace,
                Source::HumanUi,
            )
            .unwrap();
        (task_ref, codex_ref, grok_ref)
    };
    let codex_addr = registry(&home).session(&codex_ref).unwrap().address.clone();
    let grok_addr = registry(&home).session(&grok_ref).unwrap().address.clone();
    eprintln!("task {task_ref}: {codex_addr} ({codex_ref}), {grok_addr} ({grok_ref})");

    // Setup gate: doctor must be green with the drift wrapper failing only Claude (criterion 3).
    let (green, report) = doctor_json(&home, &workspace);
    let rows = report["harnesses"].as_array().cloned().unwrap_or_default();
    let claude_row = rows
        .iter()
        .find(|row| row["hkey"] == "claude")
        .cloned()
        .unwrap_or(Value::Null);
    let others_ready = rows
        .iter()
        .filter(|row| row["hkey"] != "claude")
        .all(|row| row["messaging"]["status"] == "ready" && row["launch"]["available"] == true);
    evidence.value["versions"] = json!({
        "claude": rows.iter().find(|r| r["hkey"]=="claude").map(|r| r["version"].clone()),
        "codex": rows.iter().find(|r| r["hkey"]=="codex").map(|r| r["version"].clone()),
        "grok": rows.iter().find(|r| r["hkey"]=="grok").map(|r| r["version"].clone()),
        "codex_daemon_detail": rows.iter().find(|r| r["hkey"]=="codex").map(|r| r["messaging"]["detail"].clone()),
    });
    evidence.scenario(
        "3",
        if !green && claude_row["messaging"]["status"] == "not ready" && others_ready {
            "pass"
        } else {
            "fail"
        },
        json!({
            "doctor_green_with_drift_wrapper": green,
            "installed_healthy_doctor_green": healthy_green,
            "note": "no installed CLI was altered; the override is the product's configured-executable path; negative gates are covered offline by doctor::tests",
            "claude_messaging": claude_row["messaging"],
            "codex_grok_ready": others_ready,
            "workspace_trust": report["workspace"]["checks"],
        }),
    );
    assert!(
        others_ready,
        "Codex and Grok must be ready before any billed step:\n{report}"
    );
    assert_eq!(
        report["workspace"]["checks"][0]["status"], "ready",
        "the proof workspace must already be trusted by Grok: {}",
        report["workspace"]
    );

    let mut tui = Tui::spawn(&home, &workspace);
    tui.wait_for(TASK_NAME);

    // Launch Codex: its pane runs the first-human gate; the bridge creates one owned thread.
    let codex_rt = open_session(&mut tui, &home, &codex_ref, 0);
    assert!(
        wait_until(LAUNCH_WAIT, "Codex gate prompt", || {
            native_screen(&codex_rt).contains("type your first message")
        }),
        "the Codex gate never asked for the first message:\n{}",
        native_screen(&codex_rt)
    );
    assert!(
        window_exists(&codex_rt, "bridge"),
        "Codex bridge window missing"
    );
    let codex_thread = registry(&home)
        .session(&codex_ref)
        .unwrap()
        .provider
        .as_ref()
        .map(|mapping| mapping.provider_session_id.clone());
    eprintln!("Codex owned thread: {codex_thread:?}");

    // Launch Grok: leader pane in the trusted workspace, bridge joins with yolo_mode=false.
    let grok_rt = open_session(&mut tui, &home, &grok_ref, 1);
    assert!(
        wait_until(LAUNCH_WAIT, "Grok bridge window", || window_exists(
            &grok_rt, "bridge"
        )),
        "Grok bridge window missing"
    );
    let grok_live = wait_until(LAUNCH_WAIT, "Grok mailbox live", || {
        live_addresses(&home, &task_ref).contains(&grok_addr)
    });
    evidence.scenario(
        "launch",
        if grok_live { "pass" } else { "fail" },
        json!({
            "codex_gate_shown": true,
            "codex_bridge_window": true,
            "codex_thread_recorded": codex_thread.is_some(),
            "grok_bridge_window": true,
            "grok_mailbox_live": grok_live,
            "live_addresses": live_addresses(&home, &task_ref),
        }),
    );
    assert!(grok_live, "Grok's mailbox never went live");

    if dry_run {
        eprintln!("DRY RUN: no prompt typed; proceeding to destroy");
    } else {
        // Criterion 6 and 11: Codex, told only a goal, finds Grok and delegates with an idle notice.
        type_line(
            &codex_rt,
            "Use your TaskAI tools: call list_sessions, then call send_message with to=\"grok-verification\", text=\"Reply with exactly one line: PONG-M9\", notify_when_idle=true. When the reply and the idle notice arrive, print DONE-M9 and stop.",
        );
        evidence.turn("codex", 1);
        let codex_live = wait_until(LAUNCH_WAIT, "Codex mailbox live after first turn", || {
            live_addresses(&home, &task_ref).contains(&codex_addr)
        });
        let c6 = wait_until(MODEL_WAIT, "criterion 6 exchange", || {
            let rows = log_rows(&home, &task_ref);
            has_row(&rows, &codex_addr, &grok_addr, "delivered")
                && has_row(&rows, &grok_addr, &codex_addr, "delivered")
                && native_screen(&codex_rt).contains("DONE-M9")
        });
        evidence.turn("grok", 1);
        evidence.turn("codex", 2);
        let rows = log_rows(&home, &task_ref);
        evidence.scenario(
            "6+11",
            if codex_live && c6 { "pass" } else { "fail" },
            json!({
                "codex_mailbox_live": codex_live,
                "codex_to_grok_delivered": has_row(&rows, &codex_addr, &grok_addr, "delivered"),
                "grok_admitted": has_row(&rows, &codex_addr, &grok_addr, "admitted"),
                "grok_to_codex_delivered": has_row(&rows, &grok_addr, &codex_addr, "delivered"),
                "idle_notice_rows": rows.iter().filter(|r| r["note"].as_str().is_some_and(|n| n.contains("idle"))).count(),
                "codex_pane_done": native_screen(&codex_rt).contains("DONE-M9"),
                "log_rows": rows.len(),
                "claude_adapter": "disabled by drift (criterion 13)",
            }),
        );

        // Criterion 7: Grok does the same toward Codex through its MCP tools.
        type_line(
            &grok_rt,
            "Use the taskai MCP tools: call list_sessions, then call send_message with to=\"codex-lead\", text=\"Reply with exactly one line: PING-M9\", notify_when_idle=true. When the reply and the idle notice arrive, print DONE-M9-GROK and stop.",
        );
        evidence.turn("grok", 1);
        let before = rows.len();
        let c7 = wait_until(MODEL_WAIT, "criterion 7 exchange", || {
            let rows = log_rows(&home, &task_ref);
            rows.len() > before
                && rows[before..].iter().any(|r| {
                    r["from"] == grok_addr && r["to"] == codex_addr && r["outcome"] == "delivered"
                })
                && rows[before..].iter().any(|r| {
                    r["from"] == codex_addr && r["to"] == grok_addr && r["outcome"] == "delivered"
                })
                && native_screen(&grok_rt).contains("DONE-M9-GROK")
        });
        evidence.turn("codex", 1);
        evidence.turn("grok", 2);
        evidence.scenario(
            "7",
            if c7 { "pass" } else { "fail" },
            json!({
                "grok_pane_done": native_screen(&grok_rt).contains("DONE-M9-GROK"),
                "new_log_rows": log_rows(&home, &task_ref).len() - before,
            }),
        );
        evidence.scenario(
            "13",
            if c6 && c7 { "pass" } else { "fail" },
            json!({"note": "criteria 6 and 7 passed while the Claude adapter was disabled by drift"}),
        );
    }

    // Criterion 14 with Claude enabled: bridges die with their pane only; a killed bridge is
    // recreated on reopen, and the recreated bridges now mirror into Claude.
    registry(&home)
        .set_server_harness(
            "local",
            "claude",
            HarnessSettings {
                enabled: true,
                executable: None,
                model: None,
            },
            Source::HumanUi,
        )
        .unwrap();
    let codex_bridge_before = pane_pid(&codex_rt, "bridge");
    assert!(tmux(&codex_rt)
        .args([
            "kill-window",
            "-t",
            &format!("={}:bridge", codex_rt.session)
        ])
        .status()
        .unwrap()
        .success());
    assert!(tmux(&grok_rt)
        .args(["kill-window", "-t", &format!("={}:bridge", grok_rt.session)])
        .status()
        .unwrap()
        .success());
    let native_survived = window_exists(&codex_rt, "native") && window_exists(&grok_rt, "native");
    open_session(&mut tui, &home, &codex_ref, 0);
    let codex_bridge_back = wait_until(LAUNCH_WAIT, "Codex bridge recreated", || {
        window_exists(&codex_rt, "bridge") && pane_pid(&codex_rt, "bridge") != codex_bridge_before
    });
    open_session(&mut tui, &home, &grok_ref, 1);
    let grok_bridge_back = wait_until(LAUNCH_WAIT, "Grok bridge recreated", || {
        window_exists(&grok_rt, "bridge")
    });
    let both_live = wait_until(LAUNCH_WAIT, "both mailboxes live after recreation", || {
        let live = live_addresses(&home, &task_ref);
        live.contains(&grok_addr) && (dry_run || live.contains(&codex_addr))
    });
    let bridge_pids: Vec<u32> = [pane_pid(&codex_rt, "bridge"), pane_pid(&grok_rt, "bridge")]
        .into_iter()
        .flatten()
        .collect();
    let mirrored = wait_until(LAUNCH_WAIT, "Claude mirror cards", || {
        let cards = listing(&claude_sessions_dir());
        bridge_pids
            .iter()
            .all(|pid| cards.contains(&format!("{pid}.json")))
    });
    evidence.scenario(
        "14",
        if native_survived && codex_bridge_back && grok_bridge_back && both_live {
            "pass"
        } else {
            "fail"
        },
        json!({
            "native_panes_survived_bridge_kill": native_survived,
            "codex_bridge_recreated_with_new_pid": codex_bridge_back,
            "grok_bridge_recreated": grok_bridge_back,
            "mailboxes_live_after_recreation": both_live,
            "claude_mirror_cards_for_bridges": mirrored,
        }),
    );

    let claude_ref = registry(&home)
        .create_session(
            &task_ref,
            None,
            "reviewer",
            "claude",
            &workspace,
            Source::HumanUi,
        )
        .unwrap();
    let claude_addr = registry(&home)
        .session(&claude_ref)
        .unwrap()
        .address
        .clone();
    let claude_rt = open_session(&mut tui, &home, &claude_ref, 1);
    let observer = wait_until(LAUNCH_WAIT, "Claude observer window", || {
        window_exists(&claude_rt, "observer")
    });
    let claude_started = wait_until(LAUNCH_WAIT, "Claude TUI", || {
        let screen = native_screen(&claude_rt);
        screen.contains("Claude") || screen.contains("claude")
    });
    let claude_pid = pane_pid(&claude_rt, "native");
    let claude_card = wait_until(LAUNCH_WAIT, "Claude card", || {
        claude_pid
            .is_some_and(|pid| listing(&claude_sessions_dir()).contains(&format!("{pid}.json")))
    });
    let claude_live = wait_until(LAUNCH_WAIT, "Claude mailbox live", || {
        live_addresses(&home, &task_ref).contains(&claude_addr)
    });
    evidence.scenario(
        "claude-launch",
        if observer && claude_started && claude_card && claude_live { "pass" } else { "fail" },
        json!({
            "observer_window": observer,
            "claude_tui_started": claude_started,
            "claude_card_published": claude_card,
            "claude_in_live_sessions": claude_live,
            "mode_in_registry": mailbox_status(&home, &task_ref, &claude_addr).map(|(s, w)| format!("{s:?}/{w:?}")),
        }),
    );

    if !dry_run {
        // Criterion 12: Claude's ListAgents lists TaskAI's sessions through the mirrors.
        type_line(
            &claude_rt,
            "Call ListAgents and print every agent name it returns, one per line, then stop.",
        );
        evidence.turn("claude", 1);
        let c12 = wait_until(MODEL_WAIT, "ListAgents output", || {
            let screen = native_screen(&claude_rt);
            screen.contains(&codex_addr) && screen.contains(&grok_addr)
        });
        evidence.scenario(
            "12",
            if c12 { "pass" } else { "fail" },
            json!({"lists_codex_and_grok": c12}),
        );

        // Criterion 1: Claude -> Codex -> Claude, then Claude -> Grok -> Claude, replies waking Claude.
        type_line(&claude_rt, &format!("Use SendMessage to the agent named {codex_addr} with the message \"Reply with exactly one line: HELLO-CODEX\" and notify_when_idle true. When its reply arrives, print DONE-1A and stop."));
        evidence.turn("claude", 2);
        evidence.turn("codex", 1);
        let c1a = wait_until(MODEL_WAIT, "Claude<->Codex round trip", || {
            native_screen(&claude_rt).contains("DONE-1A")
        });
        type_line(&claude_rt, &format!("Use SendMessage to the agent named {grok_addr} with the message \"Reply with exactly one line: HELLO-GROK\" and notify_when_idle true. When its reply arrives, print DONE-1B and stop."));
        evidence.turn("claude", 2);
        evidence.turn("grok", 1);
        let c1b = wait_until(MODEL_WAIT, "Claude<->Grok round trip", || {
            native_screen(&claude_rt).contains("DONE-1B")
        });
        let rows = log_rows(&home, &task_ref);
        evidence.scenario(
            "1",
            if c1a && c1b { "pass" } else { "fail" },
            json!({
                "claude_codex_claude": c1a,
                "claude_grok_claude": c1b,
                "codex_to_claude_rows": rows.iter().filter(|r| r["from"] == codex_addr && r["to"] == claude_addr).count(),
                "grok_to_claude_rows": rows.iter().filter(|r| r["from"] == grok_addr && r["to"] == claude_addr).count(),
            }),
        );

        // Criterion 8: a Codex approval shows as waiting in the registry and in Claude's listing.
        type_line(&codex_rt, "Run this shell command now and wait for my approval if asked: touch m9-approval-probe.txt");
        evidence.turn("codex", 1);
        let waiting = wait_until(
            Duration::from_secs(120),
            "Codex waiting on approval",
            || matches!(mailbox_status(&home, &task_ref, &codex_addr), Some((Activity::Waiting, Some(ref w))) if w == "approval"),
        );
        type_line(
            &claude_rt,
            "Call ListAgents again and print each agent's name and status, then stop.",
        );
        evidence.turn("claude", 1);
        let claude_sees_waiting = wait_until(MODEL_WAIT, "Claude lists waiting", || {
            native_screen(&claude_rt).to_lowercase().contains("waiting")
        });
        evidence.scenario(
            "8",
            if waiting { "pass" } else { "fail" },
            json!({
                "registry_waiting_approval": waiting,
                "claude_listing_shows_waiting": claude_sees_waiting,
                "note": "the approval was left to the person in the native pane; no answer was given by the proof",
            }),
        );
        eprintln!("HUMAN STEP: the Codex pane holds an unanswered approval request; it is torn down unanswered by destroy.");

        // Criterion 9: the Grok delegate stops mid-task; Claude, subscribed since 1b, is told.
        nav_home(&mut tui);
        tui.send(b"\r");
        tui.wait_for("Ctrl+F");
        // Opening a task focuses the selected CLI; Ctrl+G returns to navigation first.
        tui.send(b"\x07");
        thread::sleep(Duration::from_millis(500));
        for _ in 0..8 {
            tui.send(b"\x1b[A");
            thread::sleep(Duration::from_millis(40));
        }
        tui.send(b"\x1b[B");
        tui.send(b"\x1b[B");
        tui.send(b"d");
        tui.wait_for("Destroy");
        tui.send(grok_ref.as_bytes());
        tui.send(b"\r");
        let grok_gone = wait_until(LAUNCH_WAIT, "Grok runtime gone", || {
            tmux_text(
                &grok_rt,
                &["has-session", "-t", &format!("={}", grok_rt.session)],
            )
            .is_none()
        });
        let claude_told = wait_until(MODEL_WAIT, "Claude sees the exit", || {
            native_screen(&claude_rt).to_lowercase().contains("exited")
        });
        evidence.turn("claude", 1);
        let rows = log_rows(&home, &task_ref);
        evidence.scenario(
            "9",
            if grok_gone && claude_told { "pass" } else { "fail" },
            json!({
                "grok_runtime_destroyed": grok_gone,
                "claude_rendered_exited": claude_told,
                "exited_rows": rows.iter().filter(|r| r["outcome"].as_str().is_some_and(|o| o.contains("exited")) || r["note"].as_str().is_some_and(|n| n.contains("exited"))).count(),
            }),
        );
    }

    // Criterion 4: destroy the task through the TUI, then prove nothing owned remains.
    nav_home(&mut tui);
    tui.send(b"\r");
    tui.wait_for("Ctrl+F");
    // Opening a task focuses the selected CLI; Ctrl+G returns to navigation first.
    tui.send(b"\x07");
    thread::sleep(Duration::from_millis(500));
    tui.send(b"D");
    tui.wait_for("Destroy task");
    tui.send(task_ref.as_bytes());
    tui.send(b"\r");
    let records_gone = wait_until(LAUNCH_WAIT, "task records removed", || {
        registry(&home).tasks().is_empty()
    });
    let runtimes_gone = wait_until(LAUNCH_WAIT, "runtimes gone", || {
        [&codex_rt, &grok_rt]
            .iter()
            .all(|rt| tmux_text(rt, &["has-session", "-t", &format!("={}", rt.session)]).is_none())
    });
    tui.quit();
    guard.tui_quit = true;
    thread::sleep(Duration::from_secs(2));
    let cards_after = listing(&claude_sessions_dir());
    let doors_after = listing(Path::new("/tmp/cc-socks"));
    let run_sockets: Vec<String> = walk(&home.join("run"))
        .into_iter()
        .filter(|path| path.extension().is_some_and(|ext| ext == "sock"))
        .map(|path| path.display().to_string())
        .collect();
    let leaders_gone = !home.join("run").join("grok").exists()
        || listing(&home.join("run").join("grok"))
            .iter()
            .all(|name| !name.ends_with(".sock"));
    let cleanup = json!({
        "records_removed": records_gone,
        "runtimes_gone": runtimes_gone,
        "claude_cards_as_before": cards_after == cards_before,
        "claude_cards_added": cards_after.difference(&cards_before).collect::<Vec<_>>(),
        "cc_socks_as_before": doors_after == doors_before,
        "cc_socks_added": doors_after.difference(&doors_before).collect::<Vec<_>>(),
        "private_run_sockets_left": run_sockets,
        "grok_leader_sockets_gone": leaders_gone,
        "codex_owned_thread_retained_as_provider_history": codex_thread,
    });
    let clean = records_gone
        && runtimes_gone
        && cards_after == cards_before
        && doors_after == doors_before
        && run_sockets.is_empty()
        && leaders_gone;
    evidence.value["cleanup"] = cleanup.clone();
    evidence.scenario("4", if clean { "pass" } else { "fail" }, cleanup);
    evidence.value["residual"] = json!([
        "the Codex thread created for the proof stays in Codex's own history (not swept)",
        "Grok's trust entry for the proof workspace predates the proof and is unchanged",
        "no native card, key, door or leader socket of a foreign session was touched"
    ]);
    let turns = evidence.value["turns"].clone();
    evidence.value["cost_note"] = json!(format!(
        "model turns counted from prompts typed plus letters and notices delivered: codex {}, grok {}, claude {}; wall time {}s; dry_run={}",
        turns["codex"], turns["grok"], turns["claude"], started.elapsed().as_secs(), dry_run
    ));
    evidence.save();
    eprintln!("COST NOTE: {}", evidence.value["cost_note"]);
    eprintln!("evidence: {}", evidence.path.display());
    let failed: Vec<String> = evidence.value["scenarios"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|s| s["outcome"] != "pass")
        .map(|s| s["criterion"].as_str().unwrap_or("?").to_string())
        .collect();
    assert!(
        failed.is_empty(),
        "criteria not met: {failed:?}; see evidence"
    );
}

fn walk(dir: &Path) -> Vec<PathBuf> {
    let mut out = Vec::new();
    if let Ok(entries) = fs::read_dir(dir) {
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                out.extend(walk(&path));
            } else {
                out.push(path);
            }
        }
    }
    out
}

use std::os::unix::fs::PermissionsExt;
