// ABOUTME: Pane tests — the vt100 grid resolves real byte streams to styled cells, and a
// ABOUTME: real process in a real pty renders into (and receives input through) the pane.

use std::process::Command;
use std::time::{Duration, Instant};

use taskai_core::mail::{inherited_messaging_env_names, strip_messaging_env};
use taskai_pane::grid::Grid;
use taskai_pane::Pane;

// Expectation: truecolor, indexed, and default colors resolve exactly — SGR truecolor to
// its RGB, the 16 named colors and the 6x6x6 cube to xterm values, default to None.
// Why: the pane's promise is the CLI's exact colors. Value: color fidelity is testable.
#[test]
fn colors_resolve_exactly() {
    let mut g = Grid::new(24, 80);
    g.process(b"\x1b[38;2;10;20;30mhi\x1b[0m plain \x1b[31mred \x1b[44mblue-bg");
    let cells = g.cells();
    assert_eq!(cells[0][0].text, "h");
    assert_eq!(cells[0][0].fg, Some((10, 20, 30)));
    assert_eq!(cells[0][3].text, "p");
    assert_eq!(cells[0][3].fg, None);
    assert_eq!(cells[0][9].text, "r");
    assert_eq!(cells[0][9].fg, Some((205, 0, 0)));
    assert_eq!(cells[0][13].text, "b");
    assert_eq!(cells[0][13].bg, Some((0, 0, 238)));
}

#[test]
fn native_selection_and_emphasis_attributes_are_preserved() {
    let mut grid = Grid::new(3, 20);
    grid.process(b"\x1b[3;4;7mselected\x1b[0m plain");
    let cells = grid.cells();
    let selected = &cells[0][0];
    assert!(selected.italic && selected.underline && selected.inverse);
    let plain = &cells[0][9];
    assert!(!plain.italic && !plain.underline && !plain.inverse);
}

// Expectation: bold reaches the cell; carriage-return/line-feed move the cursor; content
// past the bottom scrolls. Why: real CLIs live on these basics. Value: the grid is a
// faithful terminal, not a line buffer.
#[test]
fn grid_is_a_real_terminal() {
    let mut g = Grid::new(3, 20);
    g.process(b"\x1b[1mbold\x1b[0m\r\nsecond");
    let cells = g.cells();
    assert!(cells[0][0].bold);
    assert_eq!(cells[1][0].text, "s");

    let mut g = Grid::new(3, 20);
    g.process(b"one\r\ntwo\r\nthree\r\nfour");
    let text = g.text();
    assert!(!text.contains("one"), "scrolled off: {text}");
    assert!(text.contains("four"));
}

// Expectation: resize changes the grid dimensions. Why: the pane resizes with the
// terminal and rail. Value: no stale geometry.
#[test]
fn grid_resizes() {
    let mut g = Grid::new(10, 20);
    g.process(b"x");
    g.set_size(24, 80);
    assert_eq!(g.cells().len(), 24);
    assert_eq!(g.cells()[0].len(), 80);
}

// Expectation: the grid preserves the child terminal's zero-based cursor
// coordinates and DECTCEM visibility state. Why: TaskAI redraws the child grid
// into another terminal and must restore this metadata after painting cells.
// Value: native Claude/Codex text input has a real visible caret.
#[test]
fn cursor_position_and_visibility_are_preserved() {
    let mut g = Grid::new(10, 20);
    g.process(b"abc");
    assert_eq!(
        g.cursor(),
        taskai_pane::grid::CursorState {
            row: 0,
            col: 3,
            visible: true,
        }
    );

    g.process(b"\x1b[?25l\x1b[4;7H");
    assert_eq!(
        g.cursor(),
        taskai_pane::grid::CursorState {
            row: 3,
            col: 6,
            visible: false,
        }
    );

    g.process(b"\x1b[?25h");
    assert!(g.cursor().visible);
}

// Expectation: TaskAI can see whether the nested CLI requested bracketed paste. Why:
// the outer terminal reports a paste as text, so TaskAI must restore DECSET 2004 markers
// only when the focused child expects them. Value: multiline pastes keep native-CLI
// submission semantics through the nested terminal.
#[test]
fn bracketed_paste_mode_is_preserved() {
    let mut g = Grid::new(10, 20);
    assert!(!g.bracketed_paste());
    g.process(b"\x1b[?2004h");
    assert!(g.bracketed_paste());
    g.process(b"\x1b[?2004l");
    assert!(!g.bracketed_paste());
}

fn wait_for(pane: &Pane, needle: &str) {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if pane.text().contains(needle) {
            return;
        }
        assert!(
            Instant::now() < deadline,
            "timed out waiting for {needle:?}; grid:\n{}",
            pane.text()
        );
        std::thread::sleep(Duration::from_millis(50));
    }
}

// Expectation: a real process spawned in a real pty renders its output into the grid.
// Why: this is the working pane — the CLI's own rendering, no re-implementation.
// Value: the conduit is proven with a live process, not recorded bytes.
#[test]
fn real_process_renders_into_grid() {
    let cwd = std::env::current_dir().unwrap();
    let pane = Pane::spawn(
        "/bin/sh",
        &["-c", "printf 'taskai-pane-ok'; sleep 2"],
        &cwd,
        24,
        80,
        &[],
    )
    .unwrap();
    wait_for(&pane, "taskai-pane-ok");
}

/// Shell that prints, judging names only, whether any Claude messaging variable and the kept
/// explicit variable reached it.
const PANE_VERDICT: &str = r#"LC_ALL=C awk 'BEGIN { for (name in ENVIRON) { if (name ~ /^CLAUDE_CODE_MESSAGING_/) found = 1; if (name == "TASKAI_KEEP_EXPLICIT") kept = 1 } print "verdict messaging=" (found ? "true" : "false") " kept=" (kept ? "true" : "false") " end" }'; sleep 1"#;

// Expectation: a pane's process inherits no CLAUDE_CODE_MESSAGING_* variable from TaskAI and gets
// none passed to the pane explicitly, while other explicit variables still arrive. Why:
// portable-pty copies TaskAI's whole environment into the child, and a pane runs the provider CLI
// the person drives. Value: no pane can speak as the person's Claude session. A copy of this test
// binary inherits a fake variable, and the pane's process reports by name only.
#[test]
fn pane_processes_get_no_claude_messaging_variables() {
    if std::env::var_os("TASKAI_PANE_ENV_COPY").is_none() {
        let mut copy = Command::new(std::env::current_exe().unwrap());
        strip_messaging_env(&mut copy);
        let output = copy
            .args([
                "--exact",
                "pane_processes_get_no_claude_messaging_variables",
                "--nocapture",
            ])
            .env("TASKAI_PANE_ENV_COPY", "1")
            .env("CLAUDE_CODE_MESSAGING_SITE_PROBE", "fake")
            .output()
            .unwrap();
        assert!(output.status.success());
        let printed = String::from_utf8(output.stdout).unwrap();
        assert!(
            printed.lines().any(|line| line == "inherited=true"),
            "the copy inherits the fake variable"
        );
        assert!(printed
            .lines()
            .any(|line| line == "verdict messaging=false kept=true end"));
        return;
    }
    println!("inherited={}", !inherited_messaging_env_names().is_empty());
    let cwd = std::env::current_dir().unwrap();
    let pane = Pane::spawn(
        "/bin/sh",
        &["-c", PANE_VERDICT],
        &cwd,
        5,
        80,
        &[
            ("CLAUDE_CODE_MESSAGING_EXPLICIT", "set-on-the-pane"),
            ("TASKAI_KEEP_EXPLICIT", "kept"),
        ],
    )
    .unwrap();
    wait_for(&pane, " end");
    let text = pane.text();
    let verdict = text.lines().find(|line| line.starts_with("verdict "));
    println!("{}", verdict.unwrap_or_default().trim_end());
}

// Expectation: bytes written to the pane reach the child's stdin (echoed by the pty).
// Why: CLI focus forwards keystrokes verbatim. Value: typing is proven end to end.
#[test]
fn input_reaches_real_process() {
    let cwd = std::env::current_dir().unwrap();
    let mut pane = Pane::spawn("/bin/cat", &[], &cwd, 24, 80, &[]).unwrap();
    pane.write_bytes(b"hello-from-taskai\r").unwrap();
    wait_for(&pane, "hello-from-taskai");
}

#[test]
fn repeated_pty_resizes_keep_the_parser_and_native_terminal_in_sync() {
    let cwd = std::env::current_dir().unwrap();
    let mut pane = Pane::spawn(
        "/bin/sh",
        &[
            "-c",
            "printf 'READY\\n'; while IFS= read -r line; do stty size; done",
        ],
        &cwd,
        24,
        80,
        &[],
    )
    .unwrap();
    wait_for(&pane, "READY");
    for (rows, cols) in [(12, 34), (40, 110), (24, 80)] {
        pane.resize(rows, cols).unwrap();
        pane.resize(rows, cols).unwrap(); // unchanged geometry is a no-op
        pane.write_bytes(b"size\r").unwrap();
        wait_for(&pane, &format!("{rows} {cols}"));
        let snapshot = pane.snapshot();
        assert_eq!(snapshot.cells.len(), rows as usize);
        assert!(snapshot
            .cells
            .iter()
            .all(|row| row.iter().map(|cell| cell.width).sum::<usize>() == cols as usize));
    }
}
