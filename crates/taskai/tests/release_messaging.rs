// ABOUTME: M9 release proof — the spec §11 acceptance table on the real CLIs, each session launched
// ABOUTME: by ref with no UI in the path; opt-in and billed; writes sanitized evidence and a cost note.

#![cfg(unix)]

use std::{
    cell::Cell,
    collections::{BTreeMap, BTreeSet},
    fs,
    os::unix::{fs::PermissionsExt, net::UnixStream},
    path::{Path, PathBuf},
    process::Command,
    thread,
    time::{Duration, Instant},
};

use serde_json::{json, Value};
use taskai_core::{
    mail::{strip_messaging_env, Activity},
    registry::{HarnessSettings, Item, Registry, Source, TmuxRuntimeLocator},
};

const LAUNCH_WAIT: Duration = Duration::from_secs(90);
const MODEL_WAIT: Duration = Duration::from_secs(240);
const SUBMIT_WAIT: Duration = Duration::from_secs(20);
const CLEANUP_WAIT: Duration = Duration::from_secs(30);
const HOLD_LIMIT: Duration = Duration::from_secs(900);
/// Names a file; while it does not exist the proof keeps every session up before teardown, so
/// the person can look at the live panes. Fifteen minutes at most.
const HOLD_VAR: &str = "TASKAI_M9_HOLD_UNTIL";
const BILL_FLAG: &str = "TASKAI_I_APPROVE_THE_BILL";
const DRY_RUN_FLAG: &str = "TASKAI_M9_DRY_RUN";
const TURN_CAP_VAR: &str = "TASKAI_M9_TURN_CAP";
const WORKSPACE_VAR: &str = "TASKAI_M9_WORKSPACE";
const TASK_NAME: &str = "M9 release proof";
/// What a person's terminal hands a CLI: the login shell's names and any native home the person
/// set. Nothing else reaches a launched process, so an agent session that runs this proof cannot
/// pass its own identity to the CLIs under test.
const PERSON_ENV: [&str; 13] = [
    "HOME",
    "PATH",
    "USER",
    "LOGNAME",
    "SHELL",
    "LANG",
    "LC_ALL",
    "LC_CTYPE",
    "TERM",
    "TMPDIR",
    "CODEX_HOME",
    "GROK_HOME",
    "CLAUDE_CONFIG_DIR",
];

fn env_flag(name: &str) -> bool {
    std::env::var(name).is_ok_and(|value| value == "1")
}

/// The real taskai binary with a private home and the person's environment only.
fn taskai(home: &Path) -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_taskai"));
    command.env_clear();
    for name in PERSON_ENV {
        if let Some(value) = std::env::var_os(name) {
            command.env(name, value);
        }
    }
    command.env("TASKAI_HOME", home);
    command
}

fn doctor_json(home: &Path, workspace: &Path) -> (bool, Value) {
    let output = taskai(home)
        .args(["doctor", "--json", "--workspace"])
        .arg(workspace)
        .output()
        .expect("run taskai doctor");
    let value: Value = serde_json::from_slice(&output.stdout).unwrap_or(Value::Null);
    (output.status.success(), value)
}

/// Starts or reconciles one session by ref. No viewer stays attached and nothing is typed, so
/// this step cannot reach a provider's input.
fn launch(home: &Path, session_ref: &str) -> Value {
    let output = taskai(home)
        .args(["session", "launch", session_ref])
        .output()
        .expect("run taskai session launch");
    assert!(
        output.status.success(),
        "launching {session_ref} failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    serde_json::from_slice(&output.stdout).expect("the launch report is JSON")
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

fn tmux_ok(runtime: &TmuxRuntimeLocator, args: &[&str]) -> bool {
    tmux(runtime)
        .args(args)
        .output()
        .is_ok_and(|output| output.status.success())
}

fn tmux_text(runtime: &TmuxRuntimeLocator, args: &[&str]) -> Option<String> {
    let output = tmux(runtime).args(args).output().ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).into_owned())
}

fn session_target(runtime: &TmuxRuntimeLocator) -> String {
    format!("={}", runtime.session)
}

fn window_target(runtime: &TmuxRuntimeLocator, window: &str) -> String {
    format!("={}:{window}", runtime.session)
}

fn native_screen(runtime: &TmuxRuntimeLocator) -> String {
    tmux_text(
        runtime,
        &[
            "capture-pane",
            "-p",
            "-J",
            "-S",
            "-400",
            "-t",
            &window_target(runtime, "native"),
        ],
    )
    .unwrap_or_default()
}

/// A captured screen without its blank lines, for a failure report.
fn shown(screen: &str) -> String {
    screen
        .lines()
        .map(str::trim_end)
        .filter(|line| !line.trim().is_empty())
        .collect::<Vec<_>>()
        .join("\n")
}

/// The last lines of a captured screen, where a CLI names its permission mode.
fn footer(screen: &str) -> String {
    let text = shown(screen);
    let lines: Vec<&str> = text.lines().collect();
    lines[lines.len().saturating_sub(4)..].join("\n")
}

/// The value the command a pane was started with gives `flag`, as tmux recorded it.
fn pane_flag(runtime: &TmuxRuntimeLocator, window: &str, flag: &str) -> Option<String> {
    let started = tmux_text(
        runtime,
        &[
            "list-panes",
            "-t",
            &window_target(runtime, window),
            "-F",
            "#{pane_start_command}",
        ],
    )?;
    let mut words = started
        .split_whitespace()
        .map(|word| word.trim_matches(|c| c == '\'' || c == '"'));
    words.find(|word| *word == flag)?;
    words.next().map(str::to_string)
}

fn window_exists(runtime: &TmuxRuntimeLocator, name: &str) -> bool {
    tmux_text(
        runtime,
        &[
            "list-windows",
            "-t",
            &session_target(runtime),
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
            &window_target(runtime, window),
            "-F",
            "#{pane_pid}",
        ],
    )?
    .trim()
    .parse()
    .ok()
}

fn runtime_live(runtime: &TmuxRuntimeLocator) -> bool {
    tmux_ok(runtime, &["has-session", "-t", &session_target(runtime)])
}

/// Ends one exact tmux session, the same step TaskAI's destroy runs for a runtime.
fn kill_runtime(runtime: &TmuxRuntimeLocator) -> bool {
    tmux_ok(runtime, &["kill-session", "-t", &session_target(runtime)])
}

/// Where tmux keeps the socket of the private server `server`; tmux ignores `TMPDIR` for it.
fn tmux_socket_file(server: &str) -> PathBuf {
    // SAFETY: getuid has no arguments and cannot fail.
    let uid = unsafe { libc::getuid() };
    PathBuf::from(format!("/tmp/tmux-{uid}/{server}"))
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

fn registry(home: &Path) -> Registry {
    Registry::open(&home.join("state")).expect("open the private registry")
}

/// The task's recorded runtimes, Claude first so no closing notice can start a turn in it, then
/// Codex, whose turns run in the shared daemon, then the rest.
fn runtimes_in_teardown_order(home: &Path, task_ref: &str) -> Vec<TmuxRuntimeLocator> {
    let saved = registry(home);
    let mut sessions = saved.sessions_of(task_ref);
    sessions.sort_by_key(|session| match session.harness.as_str() {
        "claude" => 0,
        "codex" => 1,
        _ => 2,
    });
    sessions
        .iter()
        .filter_map(|session| session.runtime.clone())
        .collect()
}

/// The task's message log rows: ts, msg_id, from, to, direction, outcome, note, body.
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

fn has_row(rows: &[Value], direction: &str, from: &str, to: &str, outcome: &str) -> bool {
    rows.iter().any(|row| {
        row["direction"] == direction
            && row["from"] == from
            && row["to"] == to
            && row["outcome"] == outcome
    })
}

/// Letters, notices and receipts the log shows were handed to the session at `address`. A native
/// Claude session keeps no TaskAI log, so the sending bridge's row stands for the delivery.
fn deliveries_into(rows: &[Value], address: &str, native_claude: bool) -> u64 {
    rows.iter()
        .filter(|row| {
            let outcome = row["outcome"].as_str().unwrap_or_default();
            let note = row["note"].as_str().unwrap_or_default();
            if row["to"] != address {
                return false;
            }
            if native_claude {
                row["direction"] == "out"
                    && (outcome == "sent"
                        || (outcome.ends_with(" sent")
                            && (outcome.starts_with("idle notice ")
                                || outcome.starts_with("receipt "))))
            } else {
                row["direction"] == "in"
                    && (outcome == "admitted"
                        || (outcome.starts_with("idle notice ")
                            && note == "answers this agent's subscription")
                        || (outcome.starts_with("receipt ") && note == "answers this agent's send"))
            }
        })
        .count() as u64
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

/// Socket files under the private run folder, each with whether something still listens on it.
fn run_sockets(home: &Path) -> Vec<Value> {
    walk(&home.join("run"))
        .into_iter()
        .filter(|path| path.extension().is_some_and(|ext| ext == "sock"))
        .map(|path| {
            json!({
                "path": path.strip_prefix(home).unwrap_or(&path).display().to_string(),
                "listening": UnixStream::connect(&path).is_ok(),
            })
        })
        .collect()
}

fn today() -> String {
    let output = Command::new("date").arg("+%Y-%m-%d").output().unwrap();
    String::from_utf8_lossy(&output.stdout).trim().to_string()
}

fn git(repo: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .arg("-C")
        .arg(repo)
        .args(args)
        .output()
        .unwrap();
    String::from_utf8_lossy(&output.stdout).trim().to_string()
}

/// A completion marker the prompt never spells, so the prompt's own echo in the pane cannot be
/// read as the agent's answer.
struct Marker {
    word: &'static str,
    number: u32,
}

impl Marker {
    fn spoken(&self) -> String {
        format!(
            "the word {} immediately followed by the number {}, written as one token",
            self.word, self.number
        )
    }

    fn token(&self) -> String {
        format!("{}{}", self.word, self.number)
    }
}

struct Agent {
    session_ref: String,
    address: String,
    harness: &'static str,
    runtime: TmuxRuntimeLocator,
}

struct Evidence {
    path: PathBuf,
    value: Value,
}

impl Evidence {
    fn scenario(&mut self, criterion: &str, outcome: &str, detail: Value) {
        eprintln!("[{criterion}] {outcome}: {detail}");
        self.value["scenarios"]
            .as_array_mut()
            .unwrap()
            .push(json!({"criterion": criterion, "outcome": outcome, "evidence": detail}));
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

/// The run's private state, its evidence and its turn count against the approved cap.
struct Proof {
    home: PathBuf,
    task_ref: String,
    evidence: Evidence,
    cap: u64,
    typed: BTreeMap<&'static str, u64>,
    agents: Vec<(String, &'static str)>,
    /// The turn count at which the proof ended every runtime for passing the cap, once it has.
    stopped_at: Cell<Option<u64>>,
}

impl Proof {
    fn rows(&self) -> Vec<Value> {
        log_rows(&self.home, &self.task_ref)
    }

    /// Creates one session record and launches it by ref.
    fn start(&mut self, parent: Option<&str>, name: &str, harness: &'static str) -> Agent {
        let workspace = registry(&self.home)
            .task(&self.task_ref)
            .expect("the proof task")
            .repo
            .clone();
        let session_ref = registry(&self.home)
            .create_session(
                &self.task_ref,
                parent,
                name,
                harness,
                &workspace,
                Source::HumanUi,
            )
            .expect("create the session record");
        let report = launch(&self.home, &session_ref);
        assert_eq!(report["created"], true, "{session_ref} was already running");
        assert!(
            report["messaging_warning"].is_null(),
            "{session_ref} launched without messaging: {}",
            report["messaging_warning"]
        );
        let saved = registry(&self.home);
        let session = saved.session(&session_ref).expect("the launched session");
        let agent = Agent {
            session_ref: session_ref.clone(),
            address: session.address.clone(),
            harness,
            runtime: session.runtime.clone().expect("the launch saves a runtime"),
        };
        self.agents.push((agent.address.clone(), harness));
        self.record_sidecar(&agent);
        eprintln!(
            "launched {} as {} ({session_ref})",
            agent.harness, agent.address
        );
        agent
    }

    /// Copies what a session's bridge or observer window prints into a file under the private
    /// root. A sidecar that stops takes its window, and the reason it gave, with it.
    fn record_sidecar(&self, agent: &Agent) {
        let log = self
            .home
            .with_file_name(format!("sidecar-{}.log", agent.session_ref));
        for window in ["bridge", "observer"] {
            if window_exists(&agent.runtime, window) {
                tmux_ok(
                    &agent.runtime,
                    &[
                        "pipe-pane",
                        "-o",
                        "-t",
                        &window_target(&agent.runtime, window),
                        &format!("cat >> '{}'", log.display()),
                    ],
                );
            }
        }
    }

    /// The last lines a session's sidecar printed, for a failure report.
    fn sidecar_said(&self, agent: &Agent) -> String {
        let log = self
            .home
            .with_file_name(format!("sidecar-{}.log", agent.session_ref));
        let text = String::from_utf8_lossy(&fs::read(log).unwrap_or_default()).into_owned();
        let lines: Vec<&str> = text
            .lines()
            .map(str::trim)
            .filter(|l| !l.is_empty())
            .collect();
        lines[lines.len().saturating_sub(12)..].join(" | ")
    }

    /// A live session's status, what it waits for, and the permission class the address book
    /// holds for it, which its bridge declares on every letter it sends.
    fn peer(&self, agent: &Agent) -> Option<(Activity, Option<String>, Option<String>)> {
        registry(&self.home)
            .live_sessions(&self.task_ref)
            .into_iter()
            .find(|peer| peer.name == agent.address)
            .map(|peer| {
                (
                    peer.status,
                    peer.waiting_for,
                    peer.mode.map(|mode| format!("{mode:?}").to_lowercase()),
                )
            })
    }

    fn declared_mode(&self, agent: &Agent) -> String {
        self.peer(agent)
            .and_then(|(_, _, mode)| mode)
            .unwrap_or_else(|| "unknown".into())
    }

    fn is_live(&self, agent: &Agent) -> bool {
        self.peer(agent).is_some()
    }

    fn status(&self, agent: &Agent) -> Option<Activity> {
        self.peer(agent).map(|(status, _, _)| status)
    }

    /// Model turns per harness: prompts this proof typed plus deliveries the log shows. A letter
    /// that steers a running Codex turn is counted as a turn.
    fn turns(&self) -> BTreeMap<&'static str, u64> {
        let rows = self.rows();
        let mut turns = self.typed.clone();
        for (address, harness) in &self.agents {
            *turns.entry(harness).or_default() +=
                deliveries_into(&rows, address, *harness == "claude");
        }
        turns
    }

    fn spent(&self) -> u64 {
        self.turns().values().sum()
    }

    /// Waits like `wait_until`, and gives up at once when the counted turns pass the cap T
    /// approved: agents start turns in each other that no prompt of this proof typed. Past the
    /// cap every runtime of the task is ended and no later wait sees its condition, so nothing
    /// more is typed.
    fn wait(&self, limit: Duration, what: &str, mut condition: impl FnMut() -> bool) -> bool {
        wait_until(limit, what, || self.past_the_cap() || condition())
            && self.stopped_at.get().is_none()
    }

    /// Keeps the proof where it is while the file `TASKAI_M9_HOLD_UNTIL` names does not exist, so
    /// the person can look at the live panes. Returns at once when the variable is unset.
    fn hold(&self, why: &str) {
        if let Some(release) = std::env::var_os(HOLD_VAR).map(PathBuf::from) {
            eprintln!("HOLD: {why}; create {} to go on", release.display());
            self.wait(HOLD_LIMIT, "the hold to be released", || release.exists());
        }
    }

    /// Whether the counted turns have passed the cap. The first time they have, ends every
    /// runtime of the task by exact tmux session, in the order teardown uses.
    fn past_the_cap(&self) -> bool {
        if self.stopped_at.get().is_some() {
            return true;
        }
        let spent = self.spent();
        if spent <= self.cap {
            return false;
        }
        self.stopped_at.set(Some(spent));
        eprintln!(
            "TURN CAP PASSED: {spent} turns counted against a cap of {}; ending every runtime of the proof task",
            self.cap
        );
        for runtime in runtimes_in_teardown_order(&self.home, &self.task_ref) {
            kill_runtime(&runtime);
        }
        true
    }

    /// Whether a scenario expected to cost `turns` still fits under the approved cap. A scenario
    /// that does not fit is recorded as not run; nothing is typed for it.
    fn affords(&mut self, criterion: &str, turns: u64) -> bool {
        let spent = self.spent();
        if spent + turns <= self.cap {
            return true;
        }
        self.evidence.scenario(
            criterion,
            "not run",
            json!({"reason": "the approved turn cap would be exceeded", "spent": spent, "needs": turns, "cap": self.cap}),
        );
        false
    }

    /// Types one prompt into a native pane the way a person would: the text, a pause so the CLI
    /// does not read Enter as part of a paste, then Enter alone. The session must leave the state
    /// it rested in; Enter is pressed once more if it has not. The prompt counts as a turn from
    /// the first Enter, whether or not the session is then seen to take it.
    fn submit(&mut self, agent: &Agent, text: &str) -> bool {
        let mut rested = None;
        if !self.wait(MODEL_WAIT, &format!("{} to settle", agent.address), || {
            rested = self
                .status(agent)
                .filter(|status| *status != Activity::Busy);
            rested.is_some()
        }) {
            return false;
        }
        let target = window_target(&agent.runtime, "native");
        if !tmux_ok(&agent.runtime, &["send-keys", "-t", &target, "-l", text]) {
            return false;
        }
        *self.typed.entry(agent.harness).or_default() += 1;
        for _ in 0..2 {
            thread::sleep(Duration::from_millis(1200));
            if !tmux_ok(&agent.runtime, &["send-keys", "-t", &target, "Enter"]) {
                return false;
            }
            let taken = self.wait(
                SUBMIT_WAIT,
                &format!("{} to take the prompt", agent.address),
                || {
                    self.status(agent)
                        .is_some_and(|status| status != Activity::Idle && Some(status) != rested)
                },
            );
            if taken {
                return true;
            }
        }
        false
    }

    /// Records a scenario that was not started because an earlier one it depends on did not pass.
    fn skip(&mut self, criterion: &str, reason: &str) {
        self.evidence
            .scenario(criterion, "not run", json!({"reason": reason}));
    }

    /// Whether the session's registry record carries no messaging entry: its bridge closed the
    /// mailbox itself, which a process that only died would not have done.
    fn unregistered(&self, agent: &Agent) -> bool {
        registry(&self.home)
            .session(&agent.session_ref)
            .is_some_and(|session| session.messaging.is_none())
    }

    fn sees(&self, agent: &Agent, marker: &Marker) -> bool {
        native_screen(&agent.runtime).contains(&marker.token())
    }
}

/// Ends, by exact tmux session, every runtime the private registry still records if the proof
/// stops early. Records stay for inspection; nothing outside the private home is touched.
struct Owned {
    home: PathBuf,
    finished: bool,
}

impl Drop for Owned {
    fn drop(&mut self) {
        if self.finished {
            return;
        }
        let Ok(registry) = Registry::open(&self.home.join("state")) else {
            return;
        };
        for task in registry.tasks() {
            for session in registry.sessions_of(&task.task_ref) {
                if let Some(runtime) = &session.runtime {
                    kill_runtime(runtime);
                }
            }
        }
        eprintln!("cleanup guard: owned runtimes ended by exact tmux session");
    }
}

/// Ends one private tmux server by its exact name and removes its socket file.
struct PrivateServer(String);

impl Drop for PrivateServer {
    fn drop(&mut self) {
        let _ = Command::new("tmux")
            .env_remove("TMUX")
            .env_remove("TMUX_PANE")
            .env_remove("TMUX_TMPDIR")
            .args(["-f", "/dev/null", "-L", &self.0, "kill-server"])
            .output();
        let _ = fs::remove_file(tmux_socket_file(&self.0));
    }
}

/// A proof over a private home whose task has a Codex and a Grok session, each bound to a live
/// tmux session on one private server the way a launch leaves them, with `counted` deliveries
/// in the task's message log and a cap of `cap` turns.
fn proof_with_counted_turns(
    label: &str,
    cap: u64,
    counted: usize,
) -> (
    tempfile::TempDir,
    PrivateServer,
    Proof,
    Vec<TmuxRuntimeLocator>,
) {
    let root = tempfile::tempdir().expect("private root");
    let home = root.path().join("home");
    let workspace = root.path().join("workspace");
    fs::create_dir_all(&workspace).unwrap();
    fs::create_dir_all(&home).unwrap();
    let server = PrivateServer(format!("taskai-m9cap-{}-{label}", std::process::id()));
    let mut saved = registry(&home);
    let task_ref = saved
        .create_task("cap", &workspace, Source::HumanUi)
        .unwrap();
    let mut agents = Vec::new();
    let mut runtimes = Vec::new();
    for (name, harness) in [("lead", "codex"), ("verification", "grok")] {
        let session_ref = saved
            .create_session(&task_ref, None, name, harness, &workspace, Source::HumanUi)
            .unwrap();
        saved.bind_runtime(&session_ref, &server.0).unwrap();
        let session = saved.session(&session_ref).unwrap();
        let runtime = session.runtime.clone().unwrap();
        assert!(
            tmux_ok(
                &runtime,
                &["new-session", "-d", "-s", &runtime.session, "sleep 600"]
            ),
            "start the stand-in runtime of {session_ref}"
        );
        agents.push((session.address.clone(), harness));
        runtimes.push(runtime);
    }
    let log = home.join("state").join("messages");
    fs::create_dir_all(&log).unwrap();
    let row =
        json!({"direction": "in", "from": agents[1].0, "to": agents[0].0, "outcome": "admitted"});
    fs::write(
        log.join(format!("{task_ref}.jsonl")),
        format!("{row}\n").repeat(counted),
    )
    .unwrap();
    let proof = Proof {
        home,
        task_ref,
        evidence: Evidence {
            path: root.path().join("evidence.json"),
            value: json!({"scenarios": []}),
        },
        cap,
        typed: BTreeMap::new(),
        agents,
        stopped_at: Cell::new(None),
    };
    (root, server, proof, runtimes)
}

// Expectation: a wait gives up as soon as the counted turns pass the approved cap, and ends every
// runtime of the proof task by its exact tmux session. Why: agents start turns in each other that
// no prompt of the proof typed, so declining to type more does not hold the bill at the cap T
// approved. Value: the cap is a ceiling on what a run can spend, not only on what the proof asks.
#[test]
fn a_wait_ends_every_runtime_once_the_counted_turns_pass_the_cap() {
    let (_root, _server, proof, runtimes) = proof_with_counted_turns("over", 1, 2);
    assert!(runtimes.iter().all(runtime_live));
    let began = Instant::now();

    let held = proof.wait(
        Duration::from_secs(20),
        "a condition that never holds",
        || false,
    );

    assert!(
        !held,
        "a wait cut short by the cap did not see its condition"
    );
    assert!(
        began.elapsed() < Duration::from_secs(10),
        "the wait gave up only at its own limit"
    );
    assert_eq!(proof.stopped_at.get(), Some(2));
    assert!(
        runtimes.iter().all(|runtime| !runtime_live(runtime)),
        "a runtime outlived the cap"
    );
    // Once stopped, a condition that holds no longer counts: nothing more may be typed.
    assert!(!proof.wait(Duration::from_secs(5), "a condition that holds", || true));
}

// Expectation: at or under the cap a wait behaves as a plain wait: it reports its condition, times
// out at its own limit, and ends nothing. Why: the safeguard must not cut short a run that is
// spending what T approved. Value: the cap check costs a healthy run nothing.
#[test]
fn a_wait_at_the_cap_ends_nothing() {
    let (_root, _server, proof, runtimes) = proof_with_counted_turns("at", 2, 2);

    assert!(proof.wait(Duration::from_secs(5), "a condition that holds", || true));
    assert!(!proof.wait(
        Duration::from_millis(600),
        "a condition that never holds",
        || false
    ));

    assert_eq!(proof.stopped_at.get(), None);
    assert!(runtimes.iter().all(runtime_live));
}

// Expectation: with the person's three logins and nothing else, the acceptance table holds on the
// installed CLIs. Codex and Grok, each told only a goal, find and message each other with the
// Claude adapter disabled by a drift failure (6, 7, 11, 13). With it enabled, Claude lists the
// mirrored sessions and round-trips with both (12, 1); a letter a bridge will never deliver comes
// back to Claude as a delivery notice (10); a Codex approval shows as waiting (8); a delegate
// stopped mid-task sends exited (9); a bridge dies with its pane and returns on relaunch (14);
// ending the runtimes leaves nothing owned behind (4). Why: backend §11 makes live identity and
// safety the release gate. Value: one repeatable command ties every claim in the spec to a run.
//
// Sessions start through `taskai session launch`, never through the UI: the UI gives the selected
// CLI the keyboard, and a script's navigation keys once became Codex turns. The only keystrokes
// here are the scenario prompts, sent to one exact native pane each.
#[test]
#[ignore = "billed: real Claude, Codex and Grok model turns; needs TASKAI_I_APPROVE_THE_BILL=1 and T's approval"]
fn release_proof_on_the_installed_clis() {
    if !env_flag(BILL_FLAG) {
        eprintln!("unproven: the release proof launches the real CLIs; set {BILL_FLAG}=1 under T's approval");
        return;
    }
    let dry_run = env_flag(DRY_RUN_FLAG);
    let cap: u64 = if dry_run {
        0
    } else {
        std::env::var(TURN_CAP_VAR)
            .ok()
            .and_then(|value| value.parse().ok())
            .unwrap_or_else(|| panic!("{TURN_CAP_VAR}: the model-turn cap T approved for this run"))
    };
    let workspace = PathBuf::from(
        std::env::var_os(WORKSPACE_VAR)
            .unwrap_or_else(|| panic!("{WORKSPACE_VAR}: a folder Grok already trusts")),
    );
    assert!(
        workspace.is_dir(),
        "{} is not a directory",
        workspace.display()
    );
    let repo_root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..");
    let evidence_path = repo_root.join("tests/evidence").join(format!(
        "session-messaging-m9-{}-{}.json",
        if dry_run { "dry-run" } else { "release-proof" },
        today()
    ));
    let started = Instant::now();

    let root = tempfile::Builder::new()
        .prefix("m9")
        .tempdir_in("/tmp")
        .expect("private root");
    let home = root.path().join("home");
    fs::create_dir_all(&home).unwrap();
    let mut guard = Owned {
        home: home.clone(),
        finished: false,
    };

    // Native state before anything runs; the same listings must match at the end.
    let cards_before = listing(&claude_sessions_dir());
    let doors_before = listing(Path::new("/tmp/cc-socks"));

    // Criterion 3, installed half: the machine is green before any override exists.
    let (healthy_green, healthy) = doctor_json(&home, &workspace);
    let mut evidence = Evidence {
        path: evidence_path,
        value: json!({
            "milestone": "M9 release proof",
            "date": today(),
            "mode": if dry_run { "dry run: zero prompts typed" } else { "billed" },
            "tested_commit": git(&repo_root, &["rev-parse", "HEAD"]),
            "uncommitted_changes": git(&repo_root, &["status", "--porcelain", "--untracked-files=no"]),
            "driver": "taskai session launch <ref>; no UI process; prompts typed only into exact native panes",
            "launch_environment_names": PERSON_ENV,
            "installed": healthy["harnesses"],
            "turn_cap": cap,
            "scenarios": [],
            "cleanup": {},
            "residual": [],
        }),
    };
    evidence.save();
    if !healthy_green {
        // An installed CLI outside its adapter's tested version stops the proof before any launch.
        evidence.scenario(
            "3",
            "fail",
            json!({
                "installed_doctor_green": false,
                "launched_nothing": true,
                "not_ready": healthy["harnesses"]
                    .as_array()
                    .into_iter()
                    .flatten()
                    .filter(|row| row["messaging"]["status"] != "ready")
                    .map(|row| json!({
                        "harness": row["hkey"],
                        "installed": row["version"],
                        "reason": row["messaging"]["detail"],
                        "remedy": row["messaging"]["remedy"],
                    }))
                    .collect::<Vec<_>>(),
                "workspace": healthy["workspace"]["checks"],
            }),
        );
    }
    assert!(
        healthy_green,
        "doctor must be green on the installed CLIs before the proof:\n{healthy}"
    );

    // The Claude adapter is disabled by a drift failure: the configured Claude reports a version
    // the adapter was not tested against. No installed CLI is altered.
    let wrapper = root.path().join("claude-drift-wrapper");
    fs::write(
        &wrapper,
        "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '2.1.288 (M9 drift wrapper)'; exit 0; fi\nexec claude \"$@\"\n",
    )
    .unwrap();
    fs::set_permissions(&wrapper, fs::Permissions::from_mode(0o700)).unwrap();
    let task_ref = {
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
        registry
            .create_task(TASK_NAME, &workspace, Source::HumanUi)
            .unwrap()
    };
    let mut proof = Proof {
        home: home.clone(),
        task_ref: task_ref.clone(),
        evidence,
        cap,
        typed: BTreeMap::new(),
        agents: Vec::new(),
        stopped_at: Cell::new(None),
    };

    // Setup gate: with the drift wrapper only Claude's adapter is not ready (criterion 3).
    let (green, report) = doctor_json(&home, &workspace);
    let rows = report["harnesses"].as_array().cloned().unwrap_or_default();
    let row = |hkey: &str| {
        rows.iter()
            .find(|row| row["hkey"] == hkey)
            .cloned()
            .unwrap_or(Value::Null)
    };
    let others_ready = ["codex", "grok"].iter().all(|hkey| {
        row(hkey)["messaging"]["status"] == "ready" && row(hkey)["launch"]["available"] == true
    });
    let claude_disabled = row("claude")["messaging"]["status"] == "not ready";
    proof.evidence.scenario(
        "3",
        if !green && claude_disabled && others_ready {
            "pass"
        } else {
            "fail"
        },
        json!({
            "installed_doctor_green": healthy_green,
            "with_drift_wrapper_doctor_green": green,
            "claude_messaging_with_wrapper": row("claude")["messaging"],
            "codex_and_grok_ready_with_wrapper": others_ready,
            "codex_messaging": row("codex")["messaging"]["detail"],
            "workspace_trust": report["workspace"]["checks"],
            "note": "the installed CLIs were not altered; the override is the product's configured-executable setting; each adapter's other fail-closed gates are covered offline by doctor::tests",
        }),
    );
    assert!(
        claude_disabled && others_ready,
        "only Claude's adapter may be disabled before the first phase:\n{report}"
    );
    assert_eq!(
        report["workspace"]["checks"][0]["status"], "ready",
        "the proof workspace must already be trusted by Grok: {}",
        report["workspace"]
    );

    // Codex: the pane runs the first-human gate and the bridge creates one owned thread.
    let codex = proof.start(None, "lead", "codex");
    let gate_shown = proof.wait(LAUNCH_WAIT, "the Codex gate prompt", || {
        native_screen(&codex.runtime).contains("type your first message")
    });
    let codex_bridge = proof.wait(LAUNCH_WAIT, "the Codex bridge window", || {
        window_exists(&codex.runtime, "bridge")
    });
    let codex_live = proof.wait(LAUNCH_WAIT, "the Codex mailbox", || proof.is_live(&codex));
    let codex_thread = registry(&home)
        .session(&codex.session_ref)
        .and_then(|session| session.provider.as_ref())
        .map(|mapping| mapping.provider_session_id.clone());

    // Grok: a leader pane in the trusted workspace; the bridge joins it with yolo_mode=false.
    let grok = proof.start(Some(&codex.session_ref), "verification", "grok");
    let grok_bridge = proof.wait(LAUNCH_WAIT, "the Grok bridge window", || {
        window_exists(&grok.runtime, "bridge")
    });
    let grok_live = proof.wait(LAUNCH_WAIT, "the Grok mailbox", || proof.is_live(&grok));
    let grok_mode_known = proof.peer(&grok).is_some_and(|(_, _, mode)| mode.is_some());
    let launched = gate_shown && codex_bridge && codex_live && grok_bridge && grok_live;
    proof.evidence.scenario(
        "launch",
        if launched && !grok_mode_known {
            "pass"
        } else {
            "fail"
        },
        json!({
            "codex_first_human_gate_shown": gate_shown,
            "codex_bridge_window": codex_bridge,
            "codex_mailbox_live_before_any_turn": codex_live,
            "codex_one_thread_recorded": codex_thread.is_some(),
            "grok_bridge_window": grok_bridge,
            "grok_mailbox_live": grok_live,
            "grok_join_required_yolo_mode_false": grok_live,
            "grok_registry_mode_unknown": !grok_mode_known,
            "codex_status_at_rest": proof.status(&codex).map(|status| format!("{status:?}").to_lowercase()),
            "codex_bridge_declares_mode": proof.declared_mode(&codex),
            "grok_status_at_rest": proof.status(&grok).map(|status| format!("{status:?}").to_lowercase()),
            "codex_bridge_said": proof.sidecar_said(&codex),
            "grok_bridge_said": proof.sidecar_said(&grok),
            "note": "the Grok bridge publishes a mailbox only after the leader logs its client with yolo_mode=false",
        }),
    );
    if !launched {
        proof.hold("the launch did not complete; the sessions are still up for inspection");
    }
    assert!(
        launched,
        "Codex and Grok did not both come up with messaging.\nCodex bridge said: {}\nGrok bridge said: {}\nCodex pane:\n{}\nGrok pane:\n{}",
        proof.sidecar_said(&codex),
        proof.sidecar_said(&grok),
        shown(&native_screen(&codex.runtime)),
        shown(&native_screen(&grok.runtime))
    );

    let mut both_delegations = false;
    if !dry_run {
        // Criteria 6 and 11: Codex, told only a goal, finds Grok and delegates with an idle notice.
        let asked = Marker {
            word: "PONG",
            number: 7316,
        };
        let done = Marker {
            word: "DONE",
            number: 6601,
        };
        let mut c6 = false;
        let mut detail = json!({});
        if proof.affords("6+11", 4) {
            let typed = proof.submit(
                &codex,
                &format!(
                    "You are one agent in a TaskAI task. Goal: get the Grok agent in this task to send you one line, {}. Use your taskai tools: list the sessions to find it, send it one message asking for exactly that reply, and ask to be notified when it goes idle. When both its reply and the idle notice have reached you, print {} and stop. Do nothing else.",
                    asked.spoken(),
                    done.spoken()
                ),
            );
            let exchanged = typed
                && proof.wait(MODEL_WAIT, "the Codex to Grok delegation", || {
                    let rows = proof.rows();
                    has_row(&rows, "in", &codex.address, &grok.address, "admitted")
                        && has_row(&rows, "in", &grok.address, &codex.address, "admitted")
                        && has_row(
                            &rows,
                            "in",
                            &grok.address,
                            &codex.address,
                            "idle notice idle",
                        )
                });
            let reported = exchanged
                && proof.wait(MODEL_WAIT, "Codex to report", || proof.sees(&codex, &done));
            let rows = proof.rows();
            c6 = exchanged && reported;
            detail = json!({
                "first_human_message_taken_by_the_gate": typed,
                "codex_letter_sent": has_row(&rows, "out", &codex.address, &grok.address, "sent"),
                "grok_admitted_it": has_row(&rows, "in", &codex.address, &grok.address, "admitted"),
                "codex_subscribed_to_grok": has_row(&rows, "out", &codex.address, &grok.address, "subscribed"),
                "grok_reply_sent": has_row(&rows, "out", &grok.address, &codex.address, "sent"),
                "codex_admitted_the_reply": has_row(&rows, "in", &grok.address, &codex.address, "admitted"),
                "grok_idle_notice_sent": has_row(&rows, "out", &grok.address, &codex.address, "idle notice idle sent"),
                "codex_received_the_idle_notice": has_row(&rows, "in", &grok.address, &codex.address, "idle notice idle"),
                "codex_reported_done": reported,
                "recipient_named_in_the_prompt": false,
                "claude_adapter": "disabled by drift; no TaskAI-launched Claude session existed",
            });
            proof
                .evidence
                .scenario("6+11", if c6 { "pass" } else { "fail" }, detail.clone());
        }

        // Criterion 7: Grok does the same toward Codex through its MCP tools.
        let asked = Marker {
            word: "PING",
            number: 2954,
        };
        let done = Marker {
            word: "DONE",
            number: 7702,
        };
        let mut c7 = false;
        if !c6 {
            proof.skip("7", "criterion 6 did not pass");
        } else if proof.affords("7", 4) {
            let before = proof.rows().len();
            let mode_known_before = proof.peer(&grok).is_some_and(|(_, _, mode)| mode.is_some());
            let typed = proof.submit(
                &grok,
                &format!(
                    "You are one agent in a TaskAI task. Goal: get the Codex agent in this task to send you one line, {}. Use the taskai MCP tools: list the sessions to find it, send it one message asking for exactly that reply, and ask to be notified when it goes idle. When both its reply and the idle notice have reached you, print {} and stop. Do nothing else.",
                    asked.spoken(),
                    done.spoken()
                ),
            );
            let exchanged = typed
                && proof.wait(MODEL_WAIT, "the Grok to Codex delegation", || {
                    let rows = proof.rows();
                    let new = &rows[before.min(rows.len())..];
                    has_row(new, "in", &grok.address, &codex.address, "admitted")
                        && has_row(new, "in", &codex.address, &grok.address, "admitted")
                        && has_row(new, "in", &codex.address, &grok.address, "idle notice idle")
                });
            let reported =
                exchanged && proof.wait(MODEL_WAIT, "Grok to report", || proof.sees(&grok, &done));
            let rows = proof.rows();
            let new = &rows[before.min(rows.len())..];
            c7 = exchanged && reported;
            proof.evidence.scenario(
                "7",
                if c7 { "pass" } else { "fail" },
                json!({
                    "prompt_taken": typed,
                    "grok_letter_sent": has_row(new, "out", &grok.address, &codex.address, "sent"),
                    "grok_send_carried_one_use_hook_evidence": has_row(new, "out", &grok.address, &codex.address, "sent"),
                    "codex_admitted_it": has_row(new, "in", &grok.address, &codex.address, "admitted"),
                    "grok_subscribed_to_codex": has_row(new, "out", &grok.address, &codex.address, "subscribed"),
                    "codex_reply_admitted_by_grok": has_row(new, "in", &codex.address, &grok.address, "admitted"),
                    "grok_received_the_idle_notice": has_row(new, "in", &codex.address, &grok.address, "idle notice idle"),
                    "grok_reported_done": reported,
                    "grok_registry_mode_unknown_before_and_after": !mode_known_before
                        && !proof.peer(&grok).is_some_and(|(_, _, mode)| mode.is_some()),
                    "note": "the Grok bridge refuses a send without matching one-use hook evidence, so a sent letter is that match",
                }),
            );
        }
        both_delegations = c6 && c7;
        proof.evidence.scenario(
            "13",
            if both_delegations { "pass" } else { "fail" },
            json!({
                "criteria_6_and_7_passed_with_claude_disabled_by_drift": both_delegations,
                "first_delegation": detail,
            }),
        );
    }

    // Criterion 14 with Claude enabled: a bridge dies with its pane only and returns on relaunch,
    // and the bridges that return mirror into Claude.
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
    // The Codex daemon cannot resume a thread that has no turn ("no rollout found"), so a Codex
    // bridge can be recreated only after its first human message. A rehearsal types nothing and
    // recreates the Grok bridge alone; the billed proof recreates both.
    let recreated: Vec<&Agent> = if dry_run {
        vec![&grok]
    } else {
        vec![&codex, &grok]
    };
    let old_bridges: Vec<Option<u32>> = recreated
        .iter()
        .map(|agent| pane_pid(&agent.runtime, "bridge"))
        .collect();
    let killed = recreated.iter().all(|agent| {
        tmux_ok(
            &agent.runtime,
            &[
                "kill-window",
                "-t",
                &window_target(&agent.runtime, "bridge"),
            ],
        )
    });
    let natives_survived =
        window_exists(&codex.runtime, "native") && window_exists(&grok.runtime, "native");
    let unregistered = proof.wait(
        LAUNCH_WAIT,
        "the killed bridges to close their mailboxes",
        || recreated.iter().all(|agent| proof.unregistered(agent)),
    );
    let relaunched = recreated
        .iter()
        .all(|agent| launch(&home, &agent.session_ref)["created"] == false);
    for agent in &recreated {
        proof.record_sidecar(agent);
    }
    let bridges_back = proof.wait(LAUNCH_WAIT, "the killed bridges to return", || {
        recreated.iter().zip(&old_bridges).all(|(agent, old)| {
            let now = pane_pid(&agent.runtime, "bridge");
            now.is_some() && now != *old
        })
    });
    let both_live = proof.wait(LAUNCH_WAIT, "both mailboxes after relaunch", || {
        proof.is_live(&codex) && proof.is_live(&grok)
    });
    let bridge_pids: Vec<u32> = recreated
        .iter()
        .filter_map(|agent| pane_pid(&agent.runtime, "bridge"))
        .collect();
    let mirrored = bridge_pids.len() == recreated.len()
        && proof.wait(LAUNCH_WAIT, "Claude mirror cards", || {
            let cards = listing(&claude_sessions_dir());
            bridge_pids
                .iter()
                .all(|pid| cards.contains(&format!("{pid}.json")))
        });
    let returned = killed && natives_survived && relaunched && bridges_back && both_live;
    let said: Vec<String> = recreated
        .iter()
        .map(|agent| format!("{}: {}", agent.harness, proof.sidecar_said(agent)))
        .collect();
    proof.evidence.scenario(
        "14",
        if returned && unregistered {
            "pass"
        } else {
            "fail"
        },
        json!({
            "bridges_killed_and_relaunched": recreated.iter().map(|agent| agent.harness).collect::<Vec<_>>(),
            "codex_bridge_in_a_rehearsal": if dry_run {
                "not recreated: the Codex daemon cannot resume a thread that has no turn, so a Codex bridge that stops before the first human message cannot return"
            } else {
                "recreated after its first human turn"
            },
            "bridge_windows_killed": killed,
            "native_panes_survived": natives_survived,
            "killed_bridges_closed_their_mailboxes": unregistered,
            "relaunch_restarted_no_native_pane": relaunched,
            "bridges_returned_with_new_processes": bridges_back,
            "mailboxes_live_after_relaunch": both_live,
            "what_the_returned_bridges_said": if returned { Vec::new() } else { said },
        }),
    );
    proof.evidence.scenario(
        "claude-mirror",
        if mirrored { "pass" } else { "fail" },
        json!({
            "each_returned_bridge_published_a_claude_card": mirrored,
            "bridges": recreated.iter().map(|agent| agent.harness).collect::<Vec<_>>(),
        }),
    );

    // A native Claude session: its observer registers the exact card of its pane.
    let claude = proof.start(None, "reviewer", "claude");
    let observer = proof.wait(LAUNCH_WAIT, "the Claude observer window", || {
        window_exists(&claude.runtime, "observer")
    });
    let claude_pid = pane_pid(&claude.runtime, "native");
    let card = proof.wait(LAUNCH_WAIT, "the Claude card", || {
        claude_pid
            .is_some_and(|pid| listing(&claude_sessions_dir()).contains(&format!("{pid}.json")))
    });
    let claude_live =
        card && proof.wait(LAUNCH_WAIT, "the Claude mailbox", || proof.is_live(&claude));
    let asks_for_trust = !card
        && native_screen(&claude.runtime)
            .to_lowercase()
            .contains("trust");
    proof.evidence.scenario(
        "claude-launch",
        if observer && card && claude_live {
            "pass"
        } else {
            "fail"
        },
        json!({
            "observer_window": observer,
            "claude_card_published": card,
            "claude_addressable_in_the_registry": claude_live,
            "claude_pane_asks_the_person_to_trust_the_folder": asks_for_trust,
            "observer_said": proof.sidecar_said(&claude),
            "claude_screen_when_not_addressable": if claude_live { String::new() } else { shown(&native_screen(&claude.runtime)) },
        }),
    );
    if asks_for_trust {
        eprintln!("HUMAN STEP: Claude asks to trust the proof workspace; the person accepts that in Claude itself, once");
    }

    // Claude delivers a letter only when its sender declares Claude's own permission class, and
    // holds any other for the person, whom this proof never stands in for. Each class is read
    // here without a turn, so no paid turn goes to a reply Claude would hold.
    let claude_footer = footer(&native_screen(&claude.runtime));
    let claude_names_bypass = ["bypass permissions", "plan mode"]
        .iter()
        .any(|label| claude_footer.to_lowercase().contains(label));
    let codex_mode = proof.declared_mode(&codex);
    let grok_flag = pane_flag(&grok.runtime, "native", "--permission-mode");
    let parity = claude_live
        && !claude_names_bypass
        && codex_mode == "prompting"
        && grok_flag.as_deref() == Some("default");
    proof.evidence.scenario(
        "mode-parity",
        if parity { "pass" } else { "fail" },
        json!({
            "claude_addressable": claude_live,
            "claude_footer": claude_footer,
            "claude_footer_names_bypass_or_plan": claude_names_bypass,
            "codex_bridge_declares": codex_mode,
            "grok_pane_started_with_permission_mode": grok_flag,
            "note": "Claude Code 2.1.322 counts a session as bypass in bypassPermissions mode, or in plan mode with bypass available, and as prompting otherwise; it accepts a letter whose sender declares its own class and holds any other for the person",
        }),
    );

    let mut probe_file = None;
    let mut approval_left_waiting = false;
    if !dry_run && both_delegations && mirrored && claude_live && parity {
        // Criterion 12: Claude's ListAgents lists TaskAI's sessions through the mirrors.
        let mut listed = false;
        if proof.affords("12", 1) {
            let typed = proof.submit(
                &claude,
                "Call your ListAgents tool once and print every agent name it returns, one per line, then stop.",
            );
            listed = typed
                && proof.wait(MODEL_WAIT, "ListAgents output", || {
                    let screen = native_screen(&claude.runtime);
                    screen.contains(&codex.address) && screen.contains(&grok.address)
                });
            proof.evidence.scenario(
                "12-listing",
                if listed { "pass" } else { "fail" },
                json!({
                    "prompt_taken": typed,
                    "claude_printed_both_mirrored_names": listed,
                    "note": "criterion 12 also needs 1, 8, 9 and 10 below; the round trips resolve these names through Claude's own registry",
                }),
            );
        }

        // Criterion 1: Claude to Codex and back, then Claude to Grok and back.
        let mut round_trips = Vec::new();
        for (peer, asked, done) in [
            (
                &codex,
                Marker {
                    word: "HELLO",
                    number: 4471,
                },
                Marker {
                    word: "DONE",
                    number: 1101,
                },
            ),
            (
                &grok,
                Marker {
                    word: "HELLO",
                    number: 5582,
                },
                Marker {
                    word: "DONE",
                    number: 1202,
                },
            ),
        ] {
            if !listed || !proof.affords("1 (one round trip)", 3) {
                break;
            }
            let typed = proof.submit(
                &claude,
                &format!(
                    "Use your SendMessage tool to send the agent named {} this message: Reply to me with exactly one line, {}. Do not ask for an idle notice. Then stop. Later, when its reply reaches you, print {} and stop.",
                    peer.address,
                    asked.spoken(),
                    done.spoken()
                ),
            );
            let mut claude_status_at_reply = None;
            let replied = typed
                && proof.wait(MODEL_WAIT, "the reply to Claude", || {
                    let status = proof.status(&claude);
                    if has_row(&proof.rows(), "out", &peer.address, &claude.address, "sent") {
                        claude_status_at_reply.get_or_insert(status);
                        true
                    } else {
                        false
                    }
                });
            let woke = replied
                && proof.wait(MODEL_WAIT, "Claude to report", || {
                    proof.sees(&claude, &done)
                });
            let rows = proof.rows();
            round_trips.push(json!({
                "peer": peer.harness,
                "prompt_taken": typed,
                "bridge_admitted_claudes_letter": has_row(&rows, "in", &claude.address, &peer.address, "admitted"),
                "bridge_sent_the_reply_to_claude": replied,
                "claude_status_when_the_reply_was_sent": claude_status_at_reply
                    .flatten()
                    .map(|status| format!("{status:?}").to_lowercase()),
                "claude_woke_and_reported": woke,
            }));
        }
        let c1 = round_trips.len() == 2
            && round_trips
                .iter()
                .all(|trip| trip["claude_woke_and_reported"] == true);
        if listed {
            proof.evidence.scenario(
                "1",
                if c1 { "pass" } else { "fail" },
                json!({"round_trips": round_trips}),
            );
        } else {
            proof.skip("1", "Claude did not list the mirrored sessions");
        }
        if !c1 {
            for criterion in ["10", "8", "9"] {
                proof.skip(criterion, "the Claude round trips did not pass");
            }
        }

        // Criterion 10: a letter a bridge will never deliver comes back to Claude as a delivery
        // notice. A second Codex session is left at its first-human gate, where letters queue
        // without a turn; ending it drops the queued letter and the bridge tells the sender.
        if c1 && proof.affords("10", 2) {
            let sink = proof.start(None, "sink", "codex");
            let sink_live = proof.wait(LAUNCH_WAIT, "the parked Codex mailbox", || {
                proof.is_live(&sink)
            });
            let noticed = Marker {
                word: "NOTICE",
                number: 1010,
            };
            let typed = sink_live
                && proof.submit(
                    &claude,
                    &format!(
                        "Use your SendMessage tool to send the agent named {} the one-line message PING. Do not ask for an idle notice. Then stop. Later, if a delivery notice about that message reaches you, print {} and then the status that notice reports.",
                        sink.address,
                        noticed.spoken()
                    ),
                );
            let queued = typed
                && proof.wait(MODEL_WAIT, "the letter to queue at the gate", || {
                    has_row(
                        &proof.rows(),
                        "in",
                        &claude.address,
                        &sink.address,
                        "admitted",
                    )
                });
            let ended = queued && kill_runtime(&sink.runtime);
            let receipt = ended
                && proof.wait(LAUNCH_WAIT, "the dropped receipt", || {
                    has_row(
                        &proof.rows(),
                        "out",
                        &sink.address,
                        &claude.address,
                        "receipt dropped sent",
                    )
                });
            let rendered = receipt
                && proof.wait(MODEL_WAIT, "Claude to show the delivery notice", || {
                    proof.sees(&claude, &noticed)
                });
            let sink_thread = registry(&home)
                .session(&sink.session_ref)
                .and_then(|session| session.provider.as_ref())
                .map(|mapping| mapping.provider_session_id.clone());
            let _ =
                registry(&home).destroy(&Item::Session(sink.session_ref.clone()), Source::HumanUi);
            proof.evidence.scenario(
                "10",
                if rendered { "pass" } else { "fail" },
                json!({
                    "parked_codex_session_live_at_its_gate": sink_live,
                    "prompt_taken": typed,
                    "letter_admitted_and_queued_without_a_codex_turn": queued,
                    "parked_session_ended": ended,
                    "bridge_sent_a_dropped_receipt_to_claude": receipt,
                    "claude_reported_the_delivery_notice": rendered,
                    "receipt_status": "dropped (recipient-exited)",
                    "parked_codex_thread_retained_in_provider_history": sink_thread,
                    "note": "a TaskAI bridge never refuses a letter; the receipt it sends is for a letter its session exits without reading",
                }),
            );
        }

        // Criterion 8: a Codex approval shows as waiting in the registry and in Claude's listing.
        // The approval is left to the person; nothing here answers it.
        if c1 && proof.affords("8", 2) {
            let probe = PathBuf::from(std::env::var_os("HOME").unwrap_or_default())
                .join(format!("m9-approval-probe-{}.txt", std::process::id()));
            let typed = proof.submit(
                &codex,
                &format!(
                    "Run exactly this shell command and nothing else: touch {} . If it needs my approval, request it and wait for me.",
                    probe.display()
                ),
            );
            probe_file = Some(probe.clone());
            let waiting = typed
                && proof.wait(
                    Duration::from_secs(120),
                    "Codex to wait on an approval",
                    || {
                        matches!(
                            proof.peer(&codex),
                            Some((Activity::Waiting, Some(ref reason), _)) if reason == "approval"
                        )
                    },
                );
            // What Claude's listing reads is the bridge's mirror card, written by TaskAI itself.
            let mirror_card: Value = pane_pid(&codex.runtime, "bridge")
                .and_then(|pid| fs::read(claude_sessions_dir().join(format!("{pid}.json"))).ok())
                .and_then(|bytes| serde_json::from_slice(&bytes).ok())
                .unwrap_or(Value::Null);
            // Claude may have used the word earlier; only a new occurrence counts.
            let mentions = |screen: String| screen.to_lowercase().matches("waiting").count();
            let before = mentions(native_screen(&claude.runtime));
            let listed_waiting = waiting
                && proof.submit(
                    &claude,
                    "Call your ListAgents tool once and print each agent's name and its status exactly as returned, one per line, then stop.",
                )
                && proof.wait(MODEL_WAIT, "Claude to list the waiting session", || {
                    mentions(native_screen(&claude.runtime)) > before
                });
            proof.evidence.scenario(
                "8",
                if waiting && listed_waiting {
                    "pass"
                } else {
                    "fail"
                },
                json!({
                    "prompt_taken": typed,
                    "registry_shows_waiting_approval": waiting,
                    "mirror_card_status": mirror_card["status"],
                    "mirror_card_waiting_for": mirror_card["waitingFor"],
                    "claude_listing_shows_waiting": listed_waiting,
                    "command_ran_without_an_approval": probe.exists(),
                    "note": "the approval request stays unanswered in the Codex pane and thread; the proof never answers one",
                }),
            );
            if waiting {
                eprintln!("HUMAN STEP: the Codex pane holds an unanswered approval; the proof ends that runtime without answering it");
            }
            approval_left_waiting = waiting;
        }

        // Criterion 9: a delegate stopped mid-task sends exited, and the Claude subscriber is told.
        if c1 && proof.affords("9", 3) {
            let noticed = Marker {
                word: "NOTICE",
                number: 9090,
            };
            let typed = proof.submit(
                &claude,
                &format!(
                    "Use your SendMessage tool to send the agent named {} this message: Run the shell command sleep 240 and reply only after it finishes. Ask for an idle notice with that send. Then stop. Later, when an idle notice about that agent reaches you, print {} and then the state that notice reports.",
                    grok.address,
                    noticed.spoken()
                ),
            );
            let working = typed
                && proof.wait(MODEL_WAIT, "Grok to start the task", || {
                    let rows = proof.rows();
                    has_row(&rows, "in", &claude.address, &grok.address, "admitted")
                        && has_row(
                            &rows,
                            "in",
                            &claude.address,
                            &grok.address,
                            "subscription recorded",
                        )
                        && proof
                            .status(&grok)
                            .is_some_and(|status| status != Activity::Idle)
                });
            if working {
                thread::sleep(Duration::from_secs(5));
            }
            let status_when_stopped = proof
                .status(&grok)
                .map(|status| format!("{status:?}").to_lowercase());
            let mid_task = working && status_when_stopped.as_deref().is_some_and(|s| s != "idle");
            let ended = mid_task && kill_runtime(&grok.runtime);
            let exited = ended
                && proof.wait(LAUNCH_WAIT, "the exited notice", || {
                    has_row(
                        &proof.rows(),
                        "out",
                        &grok.address,
                        &claude.address,
                        "idle notice exited sent",
                    )
                });
            let told = exited
                && proof.wait(MODEL_WAIT, "Claude to show the exit", || {
                    proof.sees(&claude, &noticed)
                });
            proof.evidence.scenario(
                "9",
                if told { "pass" } else { "fail" },
                json!({
                    "prompt_taken": typed,
                    "grok_took_the_task_and_claude_subscribed": working,
                    "grok_status_when_stopped": status_when_stopped,
                    "grok_runtime_ended_mid_task": ended,
                    "bridge_sent_exited_to_claude": exited,
                    "claude_reported_the_notice": told,
                }),
            );
        }
    } else if !dry_run {
        for criterion in ["12-listing", "1", "10", "8", "9"] {
            proof.skip(
                criterion,
                "the first phase, the Claude mirrors, the Claude launch or the mode parity did not pass",
            );
        }
    }

    if let Some(spent) = proof.stopped_at.get() {
        proof.evidence.scenario(
            "turn-cap",
            "fail",
            json!({
                "turns_counted_when_stopped": spent,
                "cap": cap,
                "note": "agents started turns in each other past the approved cap; every runtime was ended at once and nothing more was typed",
            }),
        );
    }
    proof.hold("every session is still up before teardown");

    // Criterion 4: end every runtime the way destroy does, remove the records, and prove nothing
    // owned remains.
    let turns = proof.turns();
    let rows_before_teardown = proof.rows().len();
    let runtimes = runtimes_in_teardown_order(&home, &task_ref);
    for runtime in &runtimes {
        kill_runtime(runtime);
    }
    let runtimes_gone = wait_until(CLEANUP_WAIT, "the runtimes to end", || {
        runtimes.iter().all(|runtime| !runtime_live(runtime))
    });
    let settled = wait_until(CLEANUP_WAIT, "the bridges to finish closing", || {
        run_sockets(&home).is_empty()
            && listing(&claude_sessions_dir()) == cards_before
            && listing(Path::new("/tmp/cc-socks")) == doors_before
    });
    let records_removed = registry(&home)
        .destroy(&Item::Task(task_ref.clone()), Source::HumanUi)
        .is_ok()
        && registry(&home).tasks().is_empty();
    guard.finished = runtimes_gone;
    let cards_after = listing(&claude_sessions_dir());
    let doors_after = listing(Path::new("/tmp/cc-socks"));
    let sockets_left = run_sockets(&home);
    // The private tmux server exits with its last session and leaves its socket file behind.
    let server_gone = runtimes
        .first()
        .is_none_or(|runtime| !tmux_ok(runtime, &["list-sessions"]));
    let mut server_socket_removed = false;
    if let (true, Some(runtime)) = (server_gone, runtimes.first()) {
        server_socket_removed = fs::remove_file(tmux_socket_file(&runtime.private_server)).is_ok();
    }
    let probe_created = probe_file.as_ref().is_some_and(|probe| probe.exists());
    if let (true, Some(probe)) = (probe_created, &probe_file) {
        let _ = fs::remove_file(probe);
    }
    let teardown_rows = proof.rows();
    let late_deliveries: u64 = proof
        .agents
        .iter()
        .map(|(address, harness)| {
            deliveries_into(
                &teardown_rows[rows_before_teardown.min(teardown_rows.len())..],
                address,
                *harness == "claude",
            )
        })
        .sum();
    let clean = records_removed
        && runtimes_gone
        && settled
        && cards_after == cards_before
        && doors_after == doors_before
        && sockets_left.is_empty()
        && server_gone;
    let cleanup = json!({
        "runtimes_ended": runtimes_gone,
        "records_removed": records_removed,
        "claude_cards_as_before": cards_after == cards_before,
        "claude_cards_added": cards_after.difference(&cards_before).collect::<Vec<_>>(),
        "cc_socks_as_before": doors_after == doors_before,
        "cc_socks_added": doors_after.difference(&doors_before).collect::<Vec<_>>(),
        "socket_files_left_under_the_private_run_folder": sockets_left,
        "private_tmux_server_gone": server_gone,
        "private_tmux_socket_file_removed": server_socket_removed,
        "deliveries_started_by_teardown": late_deliveries,
        "approval_probe_file_created": probe_created,
        "how": "each runtime ended by exact tmux kill-session and the task record destroyed through the registry, the two steps TaskAI's destroy runs; the UI's own destroy is covered by app tests",
    });
    proof.evidence.value["cleanup"] = cleanup.clone();
    proof
        .evidence
        .scenario("4", if clean { "pass" } else { "fail" }, cleanup);
    // What the run leaves outside its private home, stated only where it happened.
    let mut residual = vec![
        if dry_run {
            format!("Codex thread {codex_thread:?} was created for the proof and never given a turn")
        } else {
            format!("Codex thread {codex_thread:?} created for the proof stays in Codex's own history (not swept)")
        },
        "Grok keeps the record of the proof's Grok session in its own history (not swept)".to_string(),
        "Grok's trust entry for the proof workspace predates the proof and is unchanged".to_string(),
        "TaskAI's bootstrap, hook and MCP files written into the proof workspace by launch stay there".to_string(),
        "no native card, key, door or leader socket of a foreign session was touched".to_string(),
    ];
    if proof.typed.get("claude").is_some_and(|typed| *typed > 0) {
        residual.push(
            "Claude keeps the transcript of the proof's Claude session in its own history (not swept)"
                .to_string(),
        );
    }
    if approval_left_waiting {
        residual.push(
            "the Codex thread left waiting on an approval stays waiting in the shared daemon; no model call runs while it waits"
                .to_string(),
        );
    }
    proof.evidence.value["residual"] = json!(residual);
    proof.evidence.value["turns"] = json!(turns);
    proof.evidence.value["cost_note"] = json!(format!(
        "model turns counted as prompts typed plus letters, notices and receipts delivered: {turns:?}; total {} of cap {cap}; {late_deliveries} more started during teardown; wall time {}s",
        turns.values().sum::<u64>(),
        started.elapsed().as_secs()
    ));
    proof.evidence.save();
    eprintln!("COST NOTE: {}", proof.evidence.value["cost_note"]);
    eprintln!("evidence: {}", proof.evidence.path.display());
    let failed: Vec<String> = proof.evidence.value["scenarios"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|scenario| scenario["outcome"] != "pass")
        .map(|scenario| {
            format!(
                "{} ({})",
                scenario["criterion"].as_str().unwrap_or("?"),
                scenario["outcome"].as_str().unwrap_or("?")
            )
        })
        .collect();
    assert!(
        failed.is_empty(),
        "criteria not met: {failed:?}; see {}",
        proof.evidence.path.display()
    );
}
