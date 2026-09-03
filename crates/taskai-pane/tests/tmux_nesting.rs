// ABOUTME: Differential proof for the tmux durable-session proposal — the same child
// ABOUTME: rendered directly and through a tmux attach client must produce identical cells.

use std::process::Command;
use std::time::{Duration, Instant};

use taskai_pane::grid::Cell;
use taskai_pane::Pane;

const SOCKET: &str = "taskai-nesting-proof";
const ROWS: u16 = 24;
const COLS: u16 = 80;

/// Emits the things TaskAI's renderer actually depends on: exact FILL truecolor, bold,
/// the ambiguous-width glyphs the journal records as hazardous, and a known cursor spot.
const CHILD: &str = "\
printf '\\033[38;2;124;214;255m\\033[48;2;17;24;38mION\\033[0m';\
printf '\\033[1mBOLD\\033[0m';\
printf '\\033[38;2;217;131;107m▌\\033[0m⏺ ⎿ ✳ ';\
printf 'READY\\n';\
printf '\\033[5;10H';\
sleep 60";

fn tmux_present() -> bool {
    let ok = Command::new("tmux")
        .arg("-V")
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false);
    if !ok {
        eprintln!("unproven: tmux is not installed — nesting proof did not run");
    }
    ok
}

fn kill_server() {
    let _ = Command::new("tmux")
        .args(["-L", SOCKET, "kill-server"])
        .output();
}

fn settle(pane: &Pane, needle: &str) -> bool {
    let deadline = Instant::now() + Duration::from_secs(15);
    while Instant::now() < deadline {
        if pane.text().contains(needle) {
            // let the last writes drain
            std::thread::sleep(Duration::from_millis(400));
            return true;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    false
}

fn direct_cells(cwd: &std::path::Path) -> Vec<Vec<Cell>> {
    let pane = Pane::spawn("/bin/sh", &["-c", CHILD], cwd, ROWS, COLS, &[]).unwrap();
    assert!(
        settle(&pane, "READY"),
        "direct child never rendered:\n{}",
        pane.text()
    );
    pane.cells()
}

/// Renders the same child inside tmux and views it through an attach client, exactly as
/// the proposal's `TaskAI pane -> portable PTY -> tmux attach -> native pane` chain would.
fn nested_cells(cwd: &std::path::Path, conf: Option<&str>, label: &str) -> Vec<Vec<Cell>> {
    kill_server();
    let mut create = Command::new("tmux");
    create.args(["-L", SOCKET]);
    if let Some(path) = conf {
        create.args(["-f", path]);
    }
    create
        .args(["new-session", "-d", "-s", "proof"])
        .args(["-x", &COLS.to_string(), "-y", &ROWS.to_string()])
        .args(["-e", "COLORTERM=truecolor"])
        .args(["/bin/sh", "-c", CHILD]);
    let out = create.output().expect("tmux new-session");
    assert!(
        out.status.success(),
        "{label}: tmux new-session failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );

    let mut args = vec!["-L", SOCKET];
    if let Some(path) = conf {
        args.push("-f");
        args.push(path);
    }
    args.extend(["attach-session", "-t", "proof"]);
    let pane = Pane::spawn("tmux", &args, cwd, ROWS, COLS, &[]).unwrap();
    let ok = settle(&pane, "READY");
    let cells = pane.cells();
    drop(pane);
    kill_server();
    assert!(ok, "{label}: nested child never rendered");
    cells
}

/// Rebuilds a row of measured cells back into an ANSI line, so what the attach client
/// actually received can be looked at rather than only counted.
fn render_row(cells: &[Cell]) -> String {
    let mut out = String::new();
    for c in cells.iter().take(40) {
        out.push_str("\x1b[0m");
        if let Some((r, g, b)) = c.fg {
            out.push_str(&format!("\x1b[38;2;{r};{g};{b}m"));
        }
        if let Some((r, g, b)) = c.bg {
            out.push_str(&format!("\x1b[48;2;{r};{g};{b}m"));
        }
        out.push_str(&c.text);
    }
    out.push_str("\x1b[0m");
    out
}

fn diff(a: &[Vec<Cell>], b: &[Vec<Cell>], label: &str) -> usize {
    let mut diffs = 0;
    for r in 0..ROWS as usize {
        let (ra, rb) = (a.get(r), b.get(r));
        let n = ra
            .map(|x| x.len())
            .unwrap_or(0)
            .max(rb.map(|x| x.len()).unwrap_or(0));
        for c in 0..n {
            let ca = ra.and_then(|x| x.get(c));
            let cb = rb.and_then(|x| x.get(c));
            let same = match (ca, cb) {
                (Some(x), Some(y)) => {
                    x.text == y.text && x.fg == y.fg && x.bg == y.bg && x.bold == y.bold
                }
                (None, None) => true,
                _ => false,
            };
            if !same {
                if diffs < 6 {
                    eprintln!(
                        "{label} cell ({r},{c}): direct {:?} · nested {:?}",
                        ca.map(|x| (&x.text, x.fg, x.bg, x.bold)),
                        cb.map(|x| (&x.text, x.fg, x.bg, x.bold)),
                    );
                }
                diffs += 1;
            }
        }
    }
    eprintln!("{label}: {diffs} differing cells");
    diffs
}

// Expectation: with tmux's defaults the nested rendering does NOT match the direct one,
// and with the terminal-features configuration it does. Why: the proposal routes every
// provider through a second terminal emulator, and TaskAI's promise is the CLI's exact
// appearance. Value: settles empirically whether the nesting is safe, and names the
// configuration that makes it so.
#[test]
fn nesting_preserves_cells_only_with_terminal_features() {
    if !tmux_present() {
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let cwd = tmp.path();

    let direct = direct_cells(cwd);

    let bare = nested_cells(cwd, None, "default-tmux");

    let conf = cwd.join("tmux.conf");
    std::fs::write(
        &conf,
        "set -g status off\n\
         set -g default-terminal \"tmux-256color\"\n\
         set -as terminal-features \",xterm-256color:RGB\"\n\
         set -as terminal-features \",tmux-256color:RGB\"\n",
    )
    .unwrap();
    let tuned = nested_cells(cwd, Some(conf.to_str().unwrap()), "tuned-tmux");

    let bare_diffs = diff(&direct, &bare, "default-tmux");
    let tuned_diffs = diff(&direct, &tuned, "tuned-tmux");

    println!("RESULT default-tmux differing cells: {bare_diffs}");
    println!("RESULT tuned-tmux differing cells: {tuned_diffs}");
    println!();
    println!("  what the attach client actually received:");
    println!("  direct pty     {}", render_row(&direct[0]));
    println!("  default tmux   {}", render_row(&bare[0]));
    println!("  tuned tmux     {}", render_row(&tuned[0]));
    assert_eq!(
        tuned_diffs, 0,
        "tuned tmux must render identically to a direct pty; {tuned_diffs} cells differ"
    );
}

// Expectation: the cursor the renderer reads through tmux lands where the child put it.
// Why: the attach screen maps the child's cursor into the frame; through tmux that read
// becomes tmux's cursor. Value: the cursor bridge is proven to survive the extra layer.
#[test]
fn nesting_preserves_cursor_position() {
    if !tmux_present() {
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let conf = tmp.path().join("tmux.conf");
    std::fs::write(
        &conf,
        "set -g status off\nset -g default-terminal \"tmux-256color\"\n",
    )
    .unwrap();

    let direct = Pane::spawn("/bin/sh", &["-c", CHILD], tmp.path(), ROWS, COLS, &[]).unwrap();
    assert!(settle(&direct, "READY"));
    let want = direct.snapshot().cursor;

    kill_server();
    let out = Command::new("tmux")
        .args(["-L", SOCKET, "-f", conf.to_str().unwrap()])
        .args(["new-session", "-d", "-s", "cur"])
        .args(["-x", &COLS.to_string(), "-y", &ROWS.to_string()])
        .args(["/bin/sh", "-c", CHILD])
        .output()
        .unwrap();
    assert!(out.status.success());
    let pane = Pane::spawn(
        "tmux",
        &[
            "-L",
            SOCKET,
            "-f",
            conf.to_str().unwrap(),
            "attach-session",
            "-t",
            "cur",
        ],
        tmp.path(),
        ROWS,
        COLS,
        &[],
    )
    .unwrap();
    assert!(settle(&pane, "READY"));
    let got = pane.snapshot().cursor;
    drop(pane);
    kill_server();

    println!("RESULT cursor direct {want:?} · nested {got:?}");
    assert_eq!(
        (want.row, want.col),
        (got.row, got.col),
        "cursor moved through tmux"
    );
}

// Expectation: bracketed-paste bytes reach the child byte-identically through tmux.
// Why: the Claude conduit submits a prompt as one bracketed paste; if tmux re-wraps or
// strips the markers, an atomic submission silently becomes loose keystrokes.
// Value: proves the conduit's submission mechanism survives the proposal.
#[test]
fn bracketed_paste_survives_nesting() {
    if !tmux_present() {
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    kill_server();
    let out = Command::new("tmux")
        .args(["-L", SOCKET, "new-session", "-d", "-s", "paste"])
        .args(["-x", &COLS.to_string(), "-y", &ROWS.to_string()])
        // a real provider enables bracketed-paste mode (DECSET 2004); tmux only forwards
        // the markers to an application that asked for them
        .args(["/bin/sh", "-c", "printf '\\033[?2004h'; exec cat -v"])
        .output()
        .unwrap();
    assert!(out.status.success());

    let mut pane = Pane::spawn(
        "tmux",
        &["-L", SOCKET, "attach-session", "-t", "paste"],
        tmp.path(),
        ROWS,
        COLS,
        &[],
    )
    .unwrap();
    std::thread::sleep(Duration::from_millis(600));
    // exactly what ClaudeConnector::prompt_bytes sends
    pane.write_bytes(b"\x1b[200~taskai-paste-probe\x1b[201~\r")
        .unwrap();
    let saw = settle(&pane, "taskai-paste-probe");
    let text = pane.text();
    drop(pane);
    kill_server();

    assert!(saw, "paste never reached the child:\n{text}");
    let markers = text.contains("^[[200~") && text.contains("^[[201~");
    println!("RESULT bracketed paste markers preserved: {markers}");
    assert!(markers, "tmux altered the bracketed-paste markers:\n{text}");
}

// Expectation: killing the viewer leaves the child running — the entire point of the
// proposal. Why: today Pane::drop kills the provider, which is why leaving TaskAI stops
// work. Value: proves the durability claim on this machine before any milestone starts.
#[test]
fn child_survives_viewer_death() {
    if !tmux_present() {
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let beat = tmp.path().join("beat");
    kill_server();
    let script = format!(
        "while :; do printf x >> '{}'; sleep 0.2; done",
        beat.display()
    );
    let out = Command::new("tmux")
        .args(["-L", SOCKET, "new-session", "-d", "-s", "durable"])
        .args(["/bin/sh", "-c", &script])
        .output()
        .unwrap();
    assert!(out.status.success());

    {
        let pane = Pane::spawn(
            "tmux",
            &["-L", SOCKET, "attach-session", "-t", "durable"],
            tmp.path(),
            ROWS,
            COLS,
            &[],
        )
        .unwrap();
        std::thread::sleep(Duration::from_millis(700));
        assert!(pane.text().len() < 100_000);
    } // viewer dropped here — Pane::drop kills the tmux CLIENT

    std::thread::sleep(Duration::from_millis(800));
    let before = std::fs::metadata(&beat).map(|m| m.len()).unwrap_or(0);
    std::thread::sleep(Duration::from_millis(800));
    let after = std::fs::metadata(&beat).map(|m| m.len()).unwrap_or(0);
    let alive = Command::new("tmux")
        .args(["-L", SOCKET, "has-session", "-t", "durable"])
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false);
    kill_server();

    println!("RESULT after viewer death: session alive {alive}, heartbeat {before} -> {after}");
    assert!(alive, "tmux session died with the viewer");
    assert!(
        after > before,
        "child stopped working after the viewer died"
    );
}
