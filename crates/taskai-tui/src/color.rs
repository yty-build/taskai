// ABOUTME: Brand palette and SGR color emission for the FILL design system.
// ABOUTME: Truecolor with a 256-color fallback matching the prototype's rounding.

use std::sync::OnceLock;

pub type Rgb = (u8, u8, u8);

pub const VOID: Rgb = (4, 6, 10);
pub const TEXT: Rgb = (233, 238, 246);
pub const DIM: Rgb = (126, 138, 160);
pub const FT: Rgb = (102, 115, 140);
pub const ION: Rgb = (124, 214, 255);
pub const AMB: Rgb = (255, 194, 75);
pub const OK: Rgb = (127, 216, 164);
pub const RED: Rgb = (242, 108, 108);
pub const CLAUDE: Rgb = (217, 131, 107);
pub const CODEX: Rgb = (143, 179, 255);
pub const OPENC: Rgb = (143, 227, 192);
pub const GROK: Rgb = (201, 207, 219);

pub const GLASS: Rgb = (17, 24, 38);
pub const GLASS_HI: Rgb = (25, 35, 54);
pub const GLASS_IN: Rgb = (9, 13, 22);
pub const AMB_GLASS: Rgb = (70, 53, 21);
pub const RED_GLASS: Rgb = (64, 24, 24);
pub const KCAP: Rgb = (27, 39, 64);
pub const HAIR: Rgb = (30, 41, 64);
pub const CLI_BG: Rgb = (7, 10, 17);
pub const CLI_EDGE: Rgb = (58, 66, 84);

pub const BOLD: &str = "\x1b[1m";
pub const RESET: &str = "\x1b[0m";

static TRUECOLOR: OnceLock<bool> = OnceLock::new();

pub fn set_truecolor(on: bool) {
    let _ = TRUECOLOR.set(on);
}

pub fn truecolor() -> bool {
    *TRUECOLOR.get_or_init(|| {
        matches!(
            std::env::var("COLORTERM").as_deref(),
            Ok("truecolor") | Ok("24bit")
        )
    })
}

pub fn c256(rgb: Rgb) -> u8 {
    let ch = |c: u8| (c as f64 / 255.0 * 5.0).round() as u16;
    (16 + 36 * ch(rgb.0) + 6 * ch(rgb.1) + ch(rgb.2)) as u8
}

pub fn fg(rgb: Rgb) -> String {
    if truecolor() {
        format!("\x1b[38;2;{};{};{}m", rgb.0, rgb.1, rgb.2)
    } else {
        format!("\x1b[38;5;{}m", c256(rgb))
    }
}

pub fn bg(rgb: Rgb) -> String {
    if truecolor() {
        format!("\x1b[48;2;{};{};{}m", rgb.0, rgb.1, rgb.2)
    } else {
        format!("\x1b[48;5;{}m", c256(rgb))
    }
}

/// Pull a color toward the void — for content dimmed under an overlay.
pub fn dull(rgb: Rgb, k: f64) -> Rgb {
    let d = |v: u8, o: u8| (o as f64 + (v as f64 - o as f64) * k) as u8;
    (d(rgb.0, VOID.0), d(rgb.1, VOID.1), d(rgb.2, VOID.2))
}

pub fn harness_color(hkey: &str) -> Rgb {
    match hkey {
        "claude" => CLAUDE,
        "codex" => CODEX,
        "opencode" => OPENC,
        "grok" => GROK,
        _ => DIM,
    }
}
