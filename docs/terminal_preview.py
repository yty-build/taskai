#!/usr/bin/env python3
# ABOUTME: Renders the TaskAI interface frames natively in the terminal — truecolor ANSI, zero dependencies.
# ABOUTME: Usage: python3 terminal_preview.py [home|task|both]. Ground truth for the terminal design; run in Ghostty/iTerm2/kitty/WezTerm.

import os
import sys
import shutil
import datetime

W = 110  # frame width in cells

# ---- palette ------------------------------------------------------------
VOID   = (4, 6, 10)
PNL    = (16, 24, 38)
PNL_HI = (22, 32, 50)
SEL    = (24, 38, 58)
CHIP   = (19, 28, 46)
KCAP   = (27, 39, 64)
BORDER = (61, 76, 106)
BORD_T = (72, 89, 124)
ION_BD = (86, 141, 179)
HAIR   = (30, 41, 64)
TEXT   = (233, 238, 246)
DIM    = (126, 138, 160)
FT     = (102, 115, 140)
ION    = (124, 214, 255)
AMB    = (255, 194, 75)
OK     = (127, 216, 164)
CLAUDE = (217, 131, 107)
CODEX  = (143, 179, 255)
OPENC  = (143, 227, 192)
GROK   = (201, 207, 219)

TRUECOLOR = os.environ.get("COLORTERM", "") in ("truecolor", "24bit")

def _c256(rgb):
    r, g, b = (round(c / 255 * 5) for c in rgb)
    return 16 + 36 * r + 6 * g + b

def fg(rgb):
    return f"\x1b[38;2;{rgb[0]};{rgb[1]};{rgb[2]}m" if TRUECOLOR else f"\x1b[38;5;{_c256(rgb)}m"

def bg(rgb):
    return f"\x1b[48;2;{rgb[0]};{rgb[1]};{rgb[2]}m" if TRUECOLOR else f"\x1b[48;5;{_c256(rgb)}m"

BOLD, RESET = "\x1b[1m", "\x1b[0m"

# ---- row assembly -------------------------------------------------------
# seg = (text, fg_rgb|None, bg_rgb|None, bold)
def S(t, f=None, b=None, bold=False):
    return (t, f, b, bold)

def row(segs, width=W, base_bg=VOID, indent=0):
    """Compose one full line: pad to width, paint base_bg under everything."""
    out, used = [], 0
    if indent:
        out.append(bg(base_bg) + " " * indent)
        used += indent
    for t, f, b, bold in segs:
        if used >= width:
            break
        if used + len(t) > width:
            t = t[: width - used]
        piece = bg(b if b else base_bg)
        if f:
            piece += fg(f)
        if bold:
            piece += BOLD
        piece += t + RESET
        out.append(piece)
        used += len(t)
    if used < width:
        out.append(bg(base_bg) + " " * (width - used))
    return "".join(out) + RESET

def seglen(segs):
    return sum(len(t) for t, _, _, _ in segs)

def clip_segs(segs, width):
    """Truncate a seg list to width cells, appending an ellipsis on overflow."""
    out, used = [], 0
    for t, f, b, bold in segs:
        if used >= width:
            break
        if used + len(t) > width:
            t = t[: max(0, width - used - 1)] + "…"
        out.append((t, f, b, bold))
        used += len(t)
    return out

def blank(n=1, base=VOID):
    return [row([], base_bg=base) for _ in range(n)]

def hairline():
    return row([S("─" * W, HAIR)])

def hud(crumb):
    now = datetime.datetime.now().strftime("%H:%M:%S")
    left = [S("◉ ", ION), S("T A S K A I", DIM, bold=True), S("   ·   ", FT), S(crumb, DIM)]
    right = [S("5 SESSIONS LIVE", DIM), S("   "), S("2 TASKS ACTIVE", DIM), S("   "),
             S("ATTENTION RAISED", AMB), S("   "), S(now, TEXT)]
    gap = W - seglen(left) - seglen(right)
    if gap < 2:
        left = clip_segs(left, W - seglen(right) - 2)
        gap = W - seglen(left) - seglen(right)
    return row(left + [S(" " * max(2, gap))] + right)

def kcap(key, label, last=False):
    return [S(f" {key} ", TEXT, KCAP), S(f" {label}", DIM), S("" if last else "    ")]

def keybar(pairs):
    segs = []
    for i, (k, l) in enumerate(pairs):
        segs += kcap(k, l, i == len(pairs) - 1)
    pad = max(0, (W - seglen(segs)) // 2)
    return [hairline(), row([S(" " * pad)] + segs)]

# ---- card / panel primitives -------------------------------------------
def card_top(w, indent=0, border=BORD_T, fill=PNL, dashed=False):
    ch = "╌" if dashed else "─"
    return row([S("╭" + ch * (w - 2) + "╮", border, None if dashed else fill)], indent=indent)

def card_bot(w, indent=0, border=BORDER, fill=PNL, dashed=False):
    ch = "╌" if dashed else "─"
    return row([S("╰" + ch * (w - 2) + "╯", border, None if dashed else fill)], indent=indent)

def card_row(segs, w, indent=0, border=BORDER, fill=PNL, edge="│"):
    inner = w - 2
    segs = clip_segs(segs, inner)
    used = seglen(segs)
    body = list(segs)
    if used < inner:
        body.append(S(" " * (inner - used)))
    return row([S(edge, border, fill)] + [(t, f, b if b else fill, bold) for t, f, b, bold in body]
               + [S(edge, border, fill)], indent=indent)

def sep_row(w, indent=0, border=BORDER, fill=PNL):
    return row([S("├" + "─" * (w - 2) + "┤", border, fill)], indent=indent)

# ---- HOME frame ---------------------------------------------------------
def home_frame():
    lines = []
    lines += blank(1)
    lines.append(hud("PORTFOLIO"))
    lines.append(hairline())
    lines += blank(1)
    lines.append(row([S("  Tasks", TEXT, bold=True), S("   3 teams · one terminal", FT)]))
    lines += blank(1)

    chips = [S("  ")]
    for name, n, ready in (("Claude Code", 1, True), ("Codex", 1, True), ("OpenCode", 2, True),
                           ("Grok CLI", 1, True), ("Aider", 0, False)):
        if ready:
            chips += [S(" ✓ ", DIM, CHIP), S(name, TEXT, CHIP), S(" · ", FT, CHIP),
                      S(str(n), ION, CHIP), S(" LIVE ", FT, CHIP), S("  ")]
        else:
            chips += [S(" ! ", AMB, CHIP), S(name, DIM, CHIP), S(" · SETUP ", FT, CHIP), S("  ")]
    lines.append(row(chips))
    lines += blank(1)

    cw, ind = W - 4, 2
    tasks = [
        ("●", ION, "Repository evaluation", "Lead · Claude Code,  Delivery · Codex",
         [ION, ION], "WORKING", ION, True),
        ("!", AMB, "Production release", "Ship · OpenCode", [AMB], "NEEDS ATTENTION", AMB, False),
        ("✓", DIM, "API migration", "Migrator · Grok CLI,  Docs · Codex",
         [FT, FT], "COMPLETE", DIM, False),
    ]
    for glyph, gcol, name, mgrs, dots, status, scol, selected in tasks:
        fill = SEL if False else (PNL_HI if selected else PNL)
        border = ION_BD if selected else BORDER
        border_top = ION_BD if selected else BORD_T
        lines.append(card_top(cw, ind, border_top, fill))
        lines.append(card_row([], cw, ind, border, fill))
        dotsegs = []
        for d in dots:
            dotsegs += [S("● ", d)]
        stat = [S(status, scol)]
        left = [S("  "), S(glyph, gcol), S("  "), S(name, TEXT, bold=True)]
        gap1 = (cw - 2) - seglen(left) - seglen(dotsegs) - seglen(stat) - 6
        r1 = left + [S(" " * max(1, gap1))] + dotsegs + [S("   ")] + stat + [S("  ")]
        lines.append(card_row(r1, cw, ind, border, fill))
        lines.append(card_row([S("     Managers: ", FT), S(mgrs, FT)], cw, ind, border, fill))
        lines.append(card_row([], cw, ind, border, fill))
        lines.append(card_bot(cw, ind, border, fill))
        lines += blank(1)

    lines.append(card_top(cw, ind, BORDER, None, dashed=True))
    pad = (cw - 2 - 14) // 2
    lines.append(row([S("┆", BORDER)] + [S(" " * pad), S("+ New task ", FT), S(" n ", TEXT, KCAP)]
                     + [S(" " * (cw - 2 - pad - 14))] + [S("┆", BORDER)], indent=ind))
    lines.append(card_bot(cw, ind, BORDER, None, dashed=True))
    lines += blank(1)
    lines += keybar([("Enter", "Open"), ("n", "New task"), ("h", "Harnesses"),
                     ("/", "Find"), ("?", "Help")])
    lines += blank(1)
    return lines

# ---- TASK frame ---------------------------------------------------------
def task_frame():
    RW, G = 32, 2
    PW = W - RW - G
    ri, pi = RW - 2, PW - 2

    def rrow(segs, fill=PNL, border=BORDER):
        segs = clip_segs(segs, ri)
        return [S("│", border, fill)] + [(t, f, b if b else fill, bold) for t, f, b, bold in
                (segs + [S(" " * max(0, ri - seglen(segs)))])] + [S("│", border, fill)]

    def prow(segs, fill=PNL, border=BORDER):
        segs = clip_segs(segs, pi)
        return [S("│", border, fill)] + [(t, f, b if b else fill, bold) for t, f, b, bold in
                (segs + [S(" " * max(0, pi - seglen(segs)))])] + [S("│", border, fill)]

    tree = [
        ("● ", ION, "Lead", "Claude Code", CLAUDE, True, False),
        ("└─ ● ", ION, "Architecture", "OpenCode", OPENC, False, False),
        ("● ", ION, "Delivery", "Codex", CODEX, False, False),
        ("└─ ◌ ", ION, "Verification", "Grok CLI", GROK, False, True),
    ]
    rail = [[S("╭" + "─" * ri + "╮", BORD_T, PNL)]]
    rail.append(rrow([S(" Repository evaluation", TEXT, bold=True)]))
    rail.append(rrow([S(" /Users/me/GitHub/buzz", FT)]))
    rail.append([S("├" + "─" * ri + "┤", BORDER, PNL)])
    rail.append(rrow([]))
    for pre, gcol, name, harness, hcol, selected, hand in tree:
        fill = SEL if selected else PNL
        glyph = "◌ " if hand else pre
        segs = [S(" " + ("" if pre.startswith("└") else ""), None),
                S(pre, BORDER if pre.startswith("└") else gcol if not hand else ION),
                S(name, TEXT, bold=selected)]
        gap = ri - seglen(segs) - len(harness) - 4
        segs += [S(" " * max(1, gap)), S("• ", hcol), S(harness, FT), S(" ")]
        rail.append(rrow(segs, fill))
        rail.append(rrow([]))
    rail.append(rrow([S(" + Manager ", FT), S(" m ", TEXT, KCAP)]))
    fill_at = len(rail)
    rail.append([S("├" + "─" * ri + "┤", BORDER, PNL)])
    for label, val in (("W O R K S P A C E", "/Users/me/GitHub/buzz"),
                       ("C O N D U I T", "owned-tui"),
                       ("T E A M", "4 sessions · 4 harnesses")):
        rail.append(rrow([S(" " + label, FT)]))
        rail.append(rrow([S(" " + val, DIM)]))
    rail.append([S("├" + "─" * ri + "┤", BORDER, PNL)])
    rail.append(rrow([S(" "), S(" Enter ", TEXT, KCAP), S(" attach session", FT)]))
    rail.append(rrow([S(" "), S(" Ctrl+G ", TEXT, KCAP), S(" the boundary", FT)]))
    rail.append([S("╰" + "─" * ri + "╯", BORDER, PNL)])

    pane = [[S("╭" + "─" * pi + "╮", BORD_T, PNL)]]
    head = [S(" Lead", TEXT, bold=True), S("  "), S("● ", CLAUDE), S("Claude Code", DIM)]
    chip = [S(" ● LIVE ", ION, CHIP)]
    gap = pi - seglen(head) - len("OWNED-TUI") - seglen(chip) - 4
    pane.append(prow(head + [S(" " * max(1, gap)), S("OWNED-TUI", FT), S("  ")] + chip + [S(" ")]))
    pane.append([S("├" + "─" * pi + "┤", BORDER, PNL)])
    pane.append(prow([]))
    body = [
        [S(" ✻ Claude Code · /Users/me/GitHub/buzz", FT)],
        [S(" > ", ION), S("Evaluate this repository for release readiness.", TEXT)],
        [S(" ⏺ I'll run the evaluation in three passes — structure, auth, gates.", DIM)],
        [S(" ⏺ ", DIM), S("Bash", TEXT, bold=True), S("(git log --oneline -6)", DIM)],
        [S("   ⎿ 094f2aa docs: focus vision on terminal experience …", FT)],
        [S(" ⏺ ", DIM), S("Read", TEXT, bold=True), S("(docs/release-gates.md)", DIM)],
        [S("   ⎿ Read 214 lines", FT)],
        [S(" ⏺ ", DIM), S("Bash", TEXT, bold=True), S("(taskai context tree --format text)", DIM)],
        [S("   ⎿ Task T1 → you (Lead) → Architecture · peer Delivery", FT)],
        [S(" ⏺ Architecture is on the auth worktree — folding its findings in.", DIM)],
        [S(" ⏺ ", DIM), S("Update", TEXT, bold=True), S("(readiness.md)", DIM)],
        [S("   ⎿ Added 3 sections · release gates drafted", FT)],
        [S(" ✓ ", OK), S("Gate review updated · waiting on Verification", DIM)],
        [],
        [S(" ✳ Synthesizing… ", CLAUDE), S("(esc to interrupt)", FT)],
        [],
        [S(" > ", ION), S("▉", ION)],
    ]
    for segs in body:
        pane.append(prow(segs))
    pane.append(prow([]))
    pane.append([S("╰" + "─" * pi + "╯", BORDER, PNL)])

    while len(rail) < len(pane):
        rail.insert(fill_at, rrow([]))
    while len(pane) < len(rail):
        pane.insert(len(pane) - 1, prow([]))

    lines = []
    lines += blank(1)
    lines.append(hud("TASK · REPOSITORY EVALUATION"))
    lines.append(hairline())
    lines += blank(1)
    for r, p_ in zip(rail, pane):
        lines.append(row(r + [S(" " * G)] + p_))
    lines += blank(1)
    lines += keybar([("Enter", "Attach"), ("a", "Add Subordinate"), ("m", "Add Manager"),
                     ("d", "Destroy"), ("T", "Home"), ("Ctrl+G", "Focus CLI")])
    lines += blank(1)
    return lines

# ---- main ---------------------------------------------------------------
def show(lines):
    cols = shutil.get_terminal_size((W, 40)).columns
    margin = max(0, (cols - W) // 2)
    pad = " " * margin
    for ln in lines:
        sys.stdout.write(pad + ln + "\n")
    sys.stdout.write(RESET)
    sys.stdout.flush()

def main():
    which = sys.argv[1] if len(sys.argv) > 1 else "both"
    cols = shutil.get_terminal_size((W, 40)).columns
    if cols < W:
        sys.stdout.write(f"note: terminal is {cols} cols; this preview is designed for ≥{W}.\n")
    if not TRUECOLOR:
        sys.stdout.write("note: COLORTERM is not 'truecolor' — using 256-color fallback (Tier 2).\n")
    if which in ("home", "both"):
        show(home_frame())
    if which == "both":
        sys.stdout.write("\n")
    if which in ("task", "both"):
        show(task_frame())

if __name__ == "__main__":
    main()
