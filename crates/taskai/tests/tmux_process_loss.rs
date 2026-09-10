// ABOUTME: Binary-level process-loss proof for TaskAI's durable tmux viewer boundary.
// ABOUTME: A SIGKILLed real TUI reopens onto the same non-billing fake PID and draft.

#![cfg(unix)]

use std::{
    fs,
    io::{Read, Write},
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    thread,
    time::{Duration, Instant},
};

use portable_pty::{native_pty_system, Child, CommandBuilder, MasterPty, PtySize};
use taskai_core::registry::{ConnectionTarget, Registry, TmuxRuntimeLocator};
use taskai_pane::grid::Grid;

const ROWS: u16 = 42;
const COLS: u16 = 150;
const WAIT: Duration = Duration::from_secs(10);

struct TuiProcess {
    _master: Box<dyn MasterPty + Send>,
    writer: Box<dyn Write + Send>,
    child: Box<dyn Child + Send + Sync>,
    grid: Arc<Mutex<Grid>>,
}

impl TuiProcess {
    fn spawn(args: &[&str], home: &Path, fake_provider: &Path, cwd: &Path) -> TuiProcess {
        let pair = native_pty_system()
            .openpty(PtySize {
                rows: ROWS,
                cols: COLS,
                pixel_width: 0,
                pixel_height: 0,
            })
            .expect("open outer TaskAI PTY");
        let mut command = CommandBuilder::new(env!("CARGO_BIN_EXE_taskai"));
        command.args(args);
        command.cwd(cwd);
        command.env("TASKAI_HOME", home);
        command.env("TASKAI_CLAUDE_BIN", fake_provider);
        command.env("TERM", "xterm-256color");
        command.env("COLORTERM", "truecolor");

        let child = pair
            .slave
            .spawn_command(command)
            .expect("spawn real TaskAI binary");
        drop(pair.slave);
        let writer = pair.master.take_writer().expect("open TaskAI PTY writer");
        let mut reader = pair
            .master
            .try_clone_reader()
            .expect("open TaskAI PTY reader");
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

        TuiProcess {
            _master: pair.master,
            writer,
            child,
            grid,
        }
    }

    fn screen(&self) -> String {
        self.grid.lock().unwrap().text()
    }

    fn wait_for_screen(&self, needle: &str) {
        let deadline = Instant::now() + WAIT;
        loop {
            let screen = self.screen();
            if screen.contains(needle) {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "TaskAI screen never contained {needle:?}:\n{screen}"
            );
            thread::sleep(Duration::from_millis(25));
        }
    }

    fn send(&mut self, bytes: &[u8]) {
        self.writer.write_all(bytes).expect("write TaskAI input");
        self.writer.flush().expect("flush TaskAI input");
    }

    fn sigkill(&mut self) {
        let pid = self.child.process_id().expect("TaskAI child PID");
        let status = Command::new("/bin/kill")
            .args(["-KILL", &pid.to_string()])
            .status()
            .expect("send SIGKILL to TaskAI");
        assert!(status.success(), "kill command failed: {status}");
        let exit = self.child.wait().expect("reap SIGKILLed TaskAI");
        assert!(
            !exit.success(),
            "TaskAI unexpectedly reported a clean exit after SIGKILL"
        );
    }

    fn wait_for_clean_exit(&mut self) {
        let deadline = Instant::now() + WAIT;
        loop {
            match self.child.try_wait().expect("poll TaskAI exit") {
                Some(status) => {
                    assert!(status.success(), "TaskAI exited unsuccessfully: {status:?}");
                    return;
                }
                None if Instant::now() < deadline => {
                    thread::sleep(Duration::from_millis(25));
                }
                None => panic!("TaskAI did not exit after navigation q"),
            }
        }
    }
}

impl Drop for TuiProcess {
    fn drop(&mut self) {
        if matches!(self.child.try_wait(), Ok(None)) {
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }
}

struct RuntimeCleanup {
    home: PathBuf,
}

impl Drop for RuntimeCleanup {
    fn drop(&mut self) {
        let Ok(registry) = Registry::open(&self.home.join("state")) else {
            return;
        };
        for task in registry.tasks() {
            for session in registry.sessions_of(&task.task_ref) {
                let Some(runtime) = &session.runtime else {
                    continue;
                };
                if runtime.target == ConnectionTarget::Local {
                    let _ = tmux(runtime)
                        .args(["kill-session", "-t", &format!("={}", runtime.session)])
                        .status();
                }
            }
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
    command
}

fn tmux_text(runtime: &TmuxRuntimeLocator, args: &[&str]) -> String {
    let output = tmux(runtime).args(args).output().expect("run tmux");
    assert!(
        output.status.success(),
        "tmux {:?} failed: {}",
        args,
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).expect("tmux output is UTF-8")
}

fn wait_until(message: &str, mut condition: impl FnMut() -> bool) {
    let deadline = Instant::now() + WAIT;
    while !condition() {
        assert!(Instant::now() < deadline, "{message}");
        thread::sleep(Duration::from_millis(25));
    }
}

fn fake_file(workspace: &Path, session_ref: &str, suffix: &str) -> PathBuf {
    workspace.join(format!(".taskai-fake-{session_ref}-{suffix}"))
}

fn read_pid(path: &Path) -> u32 {
    fs::read_to_string(path)
        .expect("read fake provider PID")
        .trim()
        .parse()
        .expect("parse fake provider PID")
}

// Expectation: abruptly killing the real TaskAI TUI (without q or Rust destructors)
// drops only its tmux attach client. A second real TaskAI process must render the same
// tmux pane, PID, and unsubmitted draft, while the fake launch count stays one.
// Why: AT-06 covers the failure boundary that an in-process App::drop test cannot.
// Value: a terminal crash cannot silently stop or replace in-flight agent work.
#[test]
fn unexpected_viewer_process_loss_reattaches_same_tmux_pid_and_draft() {
    let root = tempfile::tempdir().expect("temporary AT-06 root");
    let home = root.path().join("taskai-home");
    let workspace = root.path().join("at06-process-loss-repository");
    fs::create_dir_all(&workspace).expect("create fake repository");
    let fake_provider = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../tests/fixtures/fake-provider.sh")
        .canonicalize()
        .expect("canonical fake provider");
    let _cleanup = RuntimeCleanup { home: home.clone() };

    let workspace_arg = workspace.to_str().expect("UTF-8 workspace");
    let mut first = TuiProcess::spawn(
        &["--attach-claude", workspace_arg],
        &home,
        &fake_provider,
        root.path(),
    );
    first.wait_for_screen("Ctrl+F");
    first.send(b"\x06");

    let pid_path = fake_file(&workspace, "s1", "pid");
    let launch_path = fake_file(&workspace, "s1", "launches");
    let heartbeat_path = fake_file(&workspace, "s1", "heartbeat");
    wait_until("fake provider never started", || pid_path.exists());
    let provider_pid = read_pid(&pid_path);
    first.wait_for_screen(&format!("PID {provider_pid}"));

    let draft = "AT06-DRAFT-SURVIVES-SIGKILL";
    first.send(draft.as_bytes());
    first.wait_for_screen(draft);
    assert_eq!(
        fs::read(&launch_path).expect("read launch count").len(),
        1,
        "the fake provider must have launched exactly once"
    );

    let registry = Registry::open(&home.join("state")).expect("open live tracker");
    let task = registry.tasks().first().expect("one Task");
    let session = registry
        .sessions_of(&task.task_ref)
        .into_iter()
        .next()
        .expect("one Manager");
    let runtime = session.runtime.clone().expect("durable tmux locator");
    assert_eq!(runtime.target, ConnectionTarget::Local);
    assert_eq!(runtime.session, session.session_ref);
    let pane_observation = tmux_text(
        &runtime,
        &[
            "list-panes",
            "-t",
            &format!("={}", runtime.session),
            "-F",
            "#{pane_id}\t#{pane_pid}",
        ],
    );
    let (pane_id, pane_pid) = pane_observation
        .trim()
        .split_once('\t')
        .expect("one tmux pane observation");
    assert_eq!(
        pane_pid.parse::<u32>().expect("parse tmux pane PID"),
        provider_pid
    );
    let pane_id = pane_id.to_owned();

    let heartbeat_before = fs::metadata(&heartbeat_path)
        .expect("initial heartbeat")
        .len();
    first.sigkill();
    drop(first);

    wait_until("orphaned TaskAI attach client did not exit", || {
        tmux_text(&runtime, &["list-clients", "-F", "#{client_pid}"])
            .trim()
            .is_empty()
    });
    wait_until("fake provider stopped after TaskAI SIGKILL", || {
        fs::metadata(&heartbeat_path)
            .map(|metadata| metadata.len() > heartbeat_before)
            .unwrap_or(false)
    });
    assert_eq!(
        tmux_text(
            &runtime,
            &[
                "list-panes",
                "-t",
                &format!("={}", runtime.session),
                "-F",
                "#{pane_pid}",
            ],
        )
        .trim(),
        provider_pid.to_string(),
        "the original provider PID must survive TaskAI process loss"
    );
    assert!(
        tmux_text(&runtime, &["capture-pane", "-p", "-t", &pane_id],).contains(draft),
        "the unsubmitted terminal draft must survive TaskAI process loss"
    );

    let task_name = workspace
        .file_name()
        .expect("workspace name")
        .to_string_lossy();
    let mut second = TuiProcess::spawn(&[], &home, &fake_provider, root.path());
    second.wait_for_screen(&task_name);
    second.send(b"\r");
    second.wait_for_screen("Ctrl+F");
    second.send(b"\x06");
    second.wait_for_screen(&format!("PID {provider_pid}"));
    second.wait_for_screen(draft);

    assert_eq!(
        fs::read(&launch_path)
            .expect("read launch count after reopen")
            .len(),
        1,
        "reopening TaskAI must attach, not launch a replacement provider"
    );
    assert_eq!(
        read_pid(&pid_path),
        provider_pid,
        "the fake provider's recorded PID changed"
    );

    second.send(b"\x07");
    second.wait_for_screen("q  Leave");
    second.send(b"q");
    second.wait_for_clean_exit();
    assert_eq!(
        tmux_text(
            &runtime,
            &[
                "list-panes",
                "-t",
                &format!("={}", runtime.session),
                "-F",
                "#{pane_pid}",
            ],
        )
        .trim(),
        provider_pid.to_string(),
        "normal viewer detach after recovery must also preserve the provider"
    );
}
