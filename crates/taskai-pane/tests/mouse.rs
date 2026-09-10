// ABOUTME: Child-requested mouse protocol state and exact xterm input encoding.
// ABOUTME: History stays in tmux; these tests never infer scrolling from arrow keys.

use std::time::{Duration, Instant};

use taskai_pane::grid::Grid;
use taskai_pane::{MouseProtocolEncoding as Encoding, MouseProtocolMode as Mode, MouseState, Pane};

fn state(mode: Mode, encoding: Encoding) -> MouseState {
    MouseState {
        mode,
        encoding,
        rows: 40,
        cols: 300,
    }
}

#[test]
fn parser_exports_requested_mouse_modes_encoding_and_current_bounds() {
    let mut grid = Grid::new(40, 300);
    assert_eq!(grid.mouse(), state(Mode::None, Encoding::Default));
    grid.process(b"\x1b[?1006h");
    assert_eq!(grid.mouse().encoding, Encoding::Sgr);
    assert!(grid.mouse().encode(64, 0, 0, false).is_none());
    for (sequence, mode) in [
        (b"\x1b[?9h".as_slice(), Mode::Press),
        (b"\x1b[?1000h".as_slice(), Mode::PressRelease),
        (b"\x1b[?1002h".as_slice(), Mode::ButtonMotion),
        (b"\x1b[?1003h".as_slice(), Mode::AnyMotion),
    ] {
        grid.process(sequence);
        assert_eq!(grid.mouse(), state(mode, Encoding::Sgr));
    }
    grid.set_size(12, 80);
    assert_eq!((grid.mouse().rows, grid.mouse().cols), (12, 80));
    assert!(grid.mouse().encode(64, 12, 0, false).is_none());
    grid.process(b"\x1b[?1003l\x1b[?1006l\x1b[?1005h");
    assert_eq!(grid.mouse().mode, Mode::None);
    assert_eq!(grid.mouse().encoding, Encoding::Utf8);
    grid.process(b"\x1bc");
    assert_eq!(grid.mouse().mode, Mode::None);
    assert_eq!(grid.mouse().encoding, Encoding::Default);
}

#[test]
fn sgr_encodes_wheel_and_buttons_with_one_based_column_then_row() {
    let mouse = state(Mode::PressRelease, Encoding::Sgr);
    for button in [0, 1, 2, 64, 65, 66, 67] {
        assert_eq!(
            mouse.encode(button, 4, 299, false).unwrap(),
            format!("\x1b[<{button};300;5M").into_bytes()
        );
    }
    assert_eq!(mouse.encode(0, 4, 299, true).unwrap(), b"\x1b[<0;300;5m");
    assert_eq!(mouse.encode(2, 0, 0, true).unwrap(), b"\x1b[<2;1;1m");
}

#[test]
fn modifiers_are_preserved_for_normal_tracking_press_wheel_drag_and_release() {
    let mouse = state(Mode::ButtonMotion, Encoding::Sgr);
    for modifiers in (0..32).step_by(4) {
        for button in [1, 32, 64] {
            let code = button | modifiers;
            assert_eq!(
                mouse.encode(code, 2, 7, false).unwrap(),
                format!("\x1b[<{code};8;3M").into_bytes()
            );
        }
        assert_eq!(
            mouse.encode(modifiers | 2, 2, 7, true).unwrap(),
            format!("\x1b[<{};8;3m", modifiers | 2).into_bytes()
        );
    }
}

#[test]
fn mouse_modes_filter_release_drag_and_unheld_motion() {
    let disabled = state(Mode::None, Encoding::Sgr);
    assert!(disabled.encode(64, 0, 0, false).is_none());
    let press = state(Mode::Press, Encoding::Default);
    assert_eq!(press.encode(1 | 28, 0, 0, false).unwrap(), b"\x1b[M!!!");
    for (button, released) in [(0, true), (32, false), (35, false), (64, false)] {
        assert!(press.encode(button, 0, 0, released).is_none());
    }
    let normal = state(Mode::PressRelease, Encoding::Sgr);
    assert!(normal.encode(32, 0, 0, false).is_none());
    assert!(normal.encode(35, 0, 0, false).is_none());
    let drag = state(Mode::ButtonMotion, Encoding::Sgr);
    assert_eq!(drag.encode(34, 0, 0, false).unwrap(), b"\x1b[<34;1;1M");
    assert!(drag.encode(35, 0, 0, false).is_none());
    let motion = state(Mode::AnyMotion, Encoding::Sgr);
    assert_eq!(
        motion.encode(35 | 16, 0, 0, false).unwrap(),
        b"\x1b[<51;1;1M"
    );
}

#[test]
fn malformed_codes_wheel_releases_and_outside_coordinates_are_dropped() {
    let mouse = state(Mode::AnyMotion, Encoding::Sgr);
    for (button, released) in [
        (3, false),
        (3, true),
        (64, true),
        (67, true),
        (32, true),
        (96, false),
        (128, false),
        (255, false),
    ] {
        assert!(
            mouse.encode(button, 0, 0, released).is_none(),
            "button={button}, released={released}"
        );
    }
    for (row, col) in [(40, 0), (0, 300), (usize::MAX, 0), (0, usize::MAX)] {
        assert!(mouse.encode(64, row, col, false).is_none());
    }
    assert!(MouseState {
        rows: 0,
        cols: 0,
        ..mouse
    }
    .encode(64, 0, 0, false)
    .is_none());
}

#[test]
fn legacy_encoding_uses_release_three_and_never_wraps_large_coordinates() {
    let mouse = MouseState {
        rows: 500,
        cols: 500,
        ..state(Mode::ButtonMotion, Encoding::Default)
    };
    assert_eq!(mouse.encode(64, 0, 0, false).unwrap(), b"\x1b[M`!!");
    assert_eq!(mouse.encode(16, 0, 0, true).unwrap(), b"\x1b[M3!!");
    assert_eq!(mouse.encode(2 | 4, 0, 0, true).unwrap(), b"\x1b[M'!!");
    assert_eq!(
        mouse.encode(0, 222, 222, false).unwrap(),
        [27, 91, 77, 32, 255, 255]
    );
    assert!(mouse.encode(0, 223, 0, false).is_none());
    assert!(mouse.encode(0, 0, 223, false).is_none());
    let huge = MouseState {
        rows: usize::MAX,
        cols: usize::MAX,
        ..mouse
    };
    assert!(huge.encode(0, usize::MAX - 1, 0, false).is_none());
}

#[test]
fn utf8_encoding_extends_coordinates_only_to_its_documented_limit() {
    let mouse = MouseState {
        rows: 3000,
        cols: 3000,
        ..state(Mode::PressRelease, Encoding::Utf8)
    };
    assert_eq!(
        mouse.encode(0, 2014, 95, false).unwrap(),
        "\x1b[M \u{80}\u{7ff}".as_bytes()
    );
    assert_eq!(
        mouse.encode(2 | 16, 0, 95, true).unwrap(),
        "\x1b[M3\u{80}!".as_bytes()
    );
    assert!(mouse.encode(0, 2015, 0, false).is_none());
    assert!(mouse.encode(0, 0, 2015, false).is_none());
}

#[test]
fn pane_snapshot_and_fast_mouse_state_read_the_actual_child_request() {
    let root = tempfile::tempdir().unwrap();
    let pane = Pane::spawn(
        "/bin/sh",
        &[
            "-c",
            "printf '\\033[?1002h\\033[?1006hMOUSE_READY'; read line",
        ],
        root.path(),
        12,
        80,
        &[],
    )
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while !pane.text().contains("MOUSE_READY") {
        assert!(
            Instant::now() < deadline,
            "child did not request its protocol"
        );
        std::thread::sleep(Duration::from_millis(10));
    }
    let snapshot = pane.snapshot();
    assert_eq!(snapshot.mouse, pane.mouse_state());
    assert_eq!(snapshot.mouse.mode, Mode::ButtonMotion);
    assert_eq!(snapshot.mouse.encoding, Encoding::Sgr);
    assert_eq!((snapshot.mouse.rows, snapshot.mouse.cols), (12, 80));
    assert_eq!(
        snapshot.mouse.encode(65, 1, 3, false).unwrap(),
        b"\x1b[<65;4;2M"
    );
}
