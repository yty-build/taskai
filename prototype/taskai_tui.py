# ABOUTME: TaskAI interactive prototype — full-screen responsive TUI: vertical session rail + real-CLI pane,
# ABOUTME: native CLI chrome per harness with ACP conduit view on toggle (c). Run: COLORTERM=truecolor python3 prototype/taskai_tui.py

import os
import random
import re
import select
import shutil
import sys
import termios
import tty

from fill_system import (
    VOID, TEXT, DIM, FT, ION, AMB, OK, RED, CLAUDE, CODEX, OPENC, GROK,
    GLASS, GLASS_HI, GLASS_IN, AMB_GLASS, RED_GLASS, PAD,
    HARNESS_COLOR, DEMO, S, row, seglen, clip_segs, bg, fg, BOLD, RESET,
    TRUECOLOR, dull, clock,
)

KCAP = (27, 39, 64)   # keycap chip fill — matches the HTML concept's keybar
HAIR = (30, 41, 64)   # hairline rule above the keybar

RAIL_W = 34
GAP = 2
HUD = [("4 LIVE", ION), ("1 QUEUED", DIM), ("2 TASKS ACTIVE", DIM)]
LAMP = "ATTENTION · 1"
SPARK_GLYPHS = "▁▂▃▄▅▆▇"
SPIN_GLYPHS = ["✳", "✻", "✽", "✻"]
CLI_BG = (7, 10, 17)      # the hosted CLI's own canvas — darker than TaskAI glass
CLI_EDGE = (58, 66, 84)   # the CLI's own box-drawing chrome (its borders, not ours)

SESSIONS = [
    {"key": "lead",  "name": "Lead",         "harness": "Claude Code", "hkey": "claude",
     "state": "live", "depth": 0, "conduit": "OWNED-TUI",
     "ws": "~/GitHub/buzz"},
    {"key": "arch",  "name": "Architecture", "harness": "OpenCode",    "hkey": "opencode",
     "state": "live", "depth": 1, "conduit": "SHARED-LIVE",
     "ws": "~/GitHub/buzz-worktrees/auth-pass"},
    {"key": "deliv", "name": "Delivery",     "harness": "Codex",       "hkey": "codex",
     "state": "live", "depth": 0, "conduit": "SHARED-LIVE",
     "ws": "~/GitHub/buzz-worktrees/retry-backoff"},
    {"key": "ver",   "name": "Verification", "harness": "Grok CLI",    "hkey": "grok",
     "state": "handoff", "depth": 1, "conduit": "HANDOFF-RESUME",
     "ws": "~/GitHub/buzz-worktrees/verification-pass"},
]

def ws_short(ws, width):
    return ws if len(ws) <= width else "…" + ws[-(width - 1):]

FEEDS = {
    "lead": [e for e in DEMO["task"]["transcript"] if e[0] not in ("spin", "prompt")],
    "arch": [
        ("meta", "✻ OpenCode · worktree auth-pass"),
        ("user", "Audit the auth flow for release blockers."),
        ("tool", "Read", "src/auth/session.ts"),
        ("result", "Read 412 lines"),
        ("say", "Token refresh races the logout path — flagging as blocking."),
        ("tool", "Update", "findings.md"),
        ("result", "2 blocking issues written up"),
        ("ok", "Findings handed to Lead"),
    ],
    "deliv": [
        ("meta", "✻ Codex · /Users/me/GitHub/buzz"),
        ("user", "Land the retry/backoff branch behind the release gate."),
        ("tool", "Bash", "git rebase main"),
        ("result", "Successfully rebased retry-backoff"),
        ("tool", "Bash", "pytest tests/retry -q"),
        ("result", "48 passed · 1.9s"),
        ("say", "Branch is green — staging the gate PR."),
    ],
}
POOL = [
    [("tool", "Bash", "pytest -q"), ("result", "214 passed · 0 failed · 3.2s")],
    [("say", "Rollback gate needs a staged dry run before it can pass.")],
    [("tool", "Read", "ops/rollback-plan.md"), ("result", "Read 58 lines")],
    [("tool", "Update", "readiness.md"), ("result", "Rollback section drafted · 1 gate outstanding")],
    [("say", "Delivery's worktree merged clean — no conflicts against main.")],
]
SPIN_PHRASES = {
    "lead":  ["Synthesizing…", "Cross-checking gates…", "Folding findings…"],
    "arch":  ["Tracing auth flow…", "Writing findings…"],
    "deliv": ["Staging PR…", "Watching checks…"],
}

SGR = re.compile(r"\x1b\[[0-9;]*m")
SGR_RGB = re.compile(r"(38|48);2;(\d+);(\d+);(\d+)")

def dim_line(line, k=0.42):
    def rep(m):
        r, g, b = dull((int(m.group(2)), int(m.group(3)), int(m.group(4))), k)
        return f"{m.group(1)};2;{r};{g};{b}"
    return SGR_RGB.sub(rep, line)

def slice_cells(line, start, width):
    """Cell-accurate slice of a rendered SGR line, preserving color state."""
    out, state, col, emitting = [], "", 0, False
    i = 0
    while i < len(line):
        m = SGR.match(line, i)
        if m:
            state = "" if m.group(0) == RESET else state + m.group(0)
            if emitting:
                out.append(m.group(0))
            i = m.end()
            continue
        if col == start and not emitting:
            emitting = True
            out.append(state)
        if col >= start + width:
            break
        if emitting:
            out.append(line[i])
        col += 1
        i += 1
    got = col - start if emitting else 0
    if got < width:
        out.append(bg(VOID) + " " * (width - max(0, got)))
    return "".join(out) + RESET

# ---- state --------------------------------------------------------------
class St:
    def __init__(self):
        self.screen = "boot"
        self.prev = "home"
        self.view = "native"          # native | acp
        self.tick = 0
        self.boot_k = 0
        self.home_sel = 0
        self.rail_sel = 0
        self.task = "t1"              # t1 | t2 (attention)
        self.spin_i = 0
        self.blink = True
        self.typed = 0
        self.spin_t0 = 0
        self.tokens = 800
        self.answered = False
        self.sparks = {k: [random.randint(0, 3) for _ in range(12)]
                       for k in ("t1", "t2", "lead", "arch", "deliv")}
        self.feeds = {k: list(v) for k, v in FEEDS.items()}
        self.feeds["ship"] = [e for e in DEMO["attention"]["transcript"]
                              if e[0] not in ("prompt",)]
        self.pool_i = 0
        self.phrase = {k: 0 for k in SPIN_PHRASES}

def advance(st):
    st.tick += 1
    if st.tick % 3 == 0:
        for sp in st.sparks.values():
            sp.append(max(0, min(6, sp[-1] + random.choice((-1, 0, 0, 1, 1)))))
            del sp[0]
    if st.tick % 4 == 0:
        st.spin_i = (st.spin_i + 1) % len(SPIN_GLYPHS)
    if st.tick % 6 == 0:
        st.blink = not st.blink
    if st.tick % 5 == 0:
        st.tokens += random.randint(3, 30)
    if st.tick % 90 == 0:
        for k in st.phrase:
            st.phrase[k] = (st.phrase[k] + 1) % len(SPIN_PHRASES[k])
        st.spin_t0 = st.tick
    if st.screen in ("task", "attach") and st.tick % random.randint(30, 46) == 0:
        st.feeds["lead"] += POOL[st.pool_i % len(POOL)]
        st.pool_i += 1
        if len(st.feeds["lead"]) > 70:
            del st.feeds["lead"][: len(st.feeds["lead"]) - 70]
    if st.screen == "attach" and st.tick % 2 == 0:
        st.typed = min(st.typed + 1, len(DEMO["attach"]["composer"]))
    if st.screen == "boot":
        st.boot_k = min(st.boot_k + 2, 22)
        if st.boot_k >= 22 and st.tick % 5 == 0:
            st.screen = "home"

def spark_str(levels):
    return "".join(SPARK_GLYPHS[v] for v in levels)

# ---- shared chrome ------------------------------------------------------
def mh(cols, crumb):
    B = GLASS_HI
    hud = [S("TASKAI", TEXT, B, bold=True), S("   ", None, B), S(crumb, FT, B)]
    rsegs = []
    for i, (t, c) in enumerate(HUD):
        if i:
            rsegs.append(S(" · ", FT, B))
        rsegs.append(S(t, c, B))
    rsegs += [S(" · ", FT, B), S(" " + LAMP + " ", AMB, AMB_GLASS),
              S("   ", None, B), S(clock(), TEXT, B)]
    gap = cols - PAD - seglen(hud) - seglen(rsegs) - PAD
    return [row([S("▄" * cols, B, VOID)], width=cols),
            row(hud + [S(" " * max(1, gap), None, B)] + rsegs, width=cols,
                base_bg=B, indent=PAD),
            row([S("▀" * cols, B, VOID)], width=cols)]

def keys2(cols, pairs):
    segs = []
    for i, (key, label) in enumerate(pairs):
        if i:
            segs.append(S("    ", None, VOID))
        segs += [S(" " + key + " ", TEXT, KCAP), S(" " + label, FT, VOID)]
    pad = max(0, (cols - seglen(segs)) // 2)
    return [row([S("─" * cols, HAIR, VOID)], width=cols),
            row([S(" " * pad, None, VOID)] + segs, width=cols)]

def vrow(cols):
    return row([], width=cols)

# ---- boot / home / harnesses -------------------------------------------
def boot2(st, cols, rows):
    SW = 56
    ind = (cols - SW) // 2
    lines = [vrow(cols) for _ in range((rows - 15) // 2)]

    def srow(segs):
        used = seglen(segs)
        return row([S(" " * 3, None, GLASS)] + segs
                   + [S(" " * (SW - 3 - used), None, GLASS)], width=cols, indent=ind)

    lines.append(row([S("▄" * SW, GLASS, VOID)], width=cols, indent=ind))
    lines.append(srow([]))
    mark = "T A S K A I"
    lines.append(srow([S(" " * ((SW - 6 - len(mark)) // 2), None, GLASS),
                       S(mark, TEXT, GLASS, bold=True)]))
    tag = DEMO["tagline"]
    lines.append(srow([S(" " * ((SW - 6 - len(tag)) // 2), None, GLASS),
                       S(tag, FT, GLASS)]))
    lines.append(srow([]))
    shown = min(len(DEMO["probes"]), max(1, st.boot_k // 3 + 1))
    for name, hkey, state in DEMO["probes"][:shown]:
        segs = [S("▌", HARNESS_COLOR[hkey], GLASS), S("  ", None, GLASS),
                S(name, DIM, GLASS)]
        tail = ([S("✓ ", FT, GLASS), S("READY", FT, GLASS)] if state == "ready"
                else [S("SETUP", AMB, GLASS)])
        segs += [S(" " * (SW - 6 - seglen(segs) - seglen(tail)), None, GLASS)] + tail
        lines.append(srow(segs))
    for _ in range(len(DEMO["probes"]) - shown):
        lines.append(srow([]))
    lines.append(srow([]))
    k = min(st.boot_k, 19)
    cap = DEMO["boot_meter"] if st.boot_k >= 19 else "probing harnesses…"
    lines.append(srow([S(" " * k, None, dull(ION, 0.30)),
                       S(" " * (24 - k), None, GLASS_IN),
                       S("  ", None, GLASS), S(cap, FT, GLASS)]))
    lines.append(srow([]))
    lines.append(row([S("▀" * SW, GLASS, VOID)], width=cols, indent=ind))
    while len(lines) < rows:
        lines.append(vrow(cols))
    return lines[:rows]

def home2(st, cols, rows):
    lines = mh(cols, "PORTFOLIO")
    lines.append(vrow(cols))
    sparkmap = {0: "t1", 1: "t2", 2: None}
    for i, t in enumerate(DEMO["tasks"]):
        sel = st.home_sel == i
        fill = GLASS_HI if sel else GLASS
        stripe = HARNESS_COLOR[t["managers"][0][2]]
        lines.append(row([S("▄" * cols, fill, VOID)], width=cols))
        name = [S("▌", stripe, fill), S("  ", None, fill),
                S(t["name"], TEXT, fill, bold=True)]
        if t["status"] == "WORKING":
            stat = [S("WORKING", ION if sel else DIM, fill)]
        elif t["status"] == "NEEDS ATTENTION":
            stat = [S(" ATTENTION ", AMB, AMB_GLASS)]
        else:
            stat = [S("COMPLETE", FT, fill)]
        gap = cols - PAD - seglen(name) - seglen(stat)
        lines.append(row(name + [S(" " * gap, None, fill)] + stat,
                         width=cols, base_bg=fill))
        skey = sparkmap[i]
        spark = spark_str(st.sparks[skey]) if skey else "▁" * 12
        scol = ION if (sel and t["status"] == "WORKING") else FT
        m = [S("▌", stripe, fill), S("  ", None, fill)]
        for j, (role, harness, hkey) in enumerate(t["managers"]):
            if j:
                m.append(S("   ", None, fill))
            m += [S("● ", HARNESS_COLOR[hkey], fill),
                  S(f"{role} · {harness}", FT, fill)]
        m += [S(" " * (cols - 52 - seglen(m)), None, fill), S(spark, scol, fill)]
        meta = t["meta"]
        m += [S(" " * (cols - PAD - seglen(m) - len(meta)), None, fill),
              S(meta, FT, fill)]
        lines.append(row(m, width=cols, base_bg=fill))
        lines.append(row([S("▀" * cols, fill, VOID)], width=cols))
        lines.append(vrow(cols))
    ghost_sel = st.home_sel == 3
    gfill = GLASS if ghost_sel else GLASS_IN
    lines.append(row([S("▄" * cols, gfill, VOID)], width=cols))
    g = [S("+ New task", DIM if ghost_sel else FT, gfill)]
    g += [S(" " * (cols - PAD * 2 - seglen(g) - 3), None, gfill), S(" n ", TEXT, KCAP)]
    lines.append(row(g, width=cols, base_bg=gfill, indent=PAD))
    lines.append(row([S("▀" * cols, gfill, VOID)], width=cols))
    while len(lines) < rows - 3:
        lines.append(vrow(cols))
    lines += keys2(cols, DEMO["keys_home"])
    lines.append(vrow(cols))
    return lines[:rows]

def harness2(st, cols, rows):
    lines = mh(cols, "HARNESSES")
    lines.append(vrow(cols))
    B = GLASS_HI
    lines.append(row([S("▄" * cols, GLASS, VOID)], width=cols))
    head = [S("Harness bay", TEXT, B, bold=True)]
    tail = [S("4 of 5 ready · 4 live · 1 queued", FT, B)]
    lines.append(row(head + [S(" " * (cols - PAD * 2 - seglen(head) - seglen(tail)), None, B)]
                     + tail, width=cols, base_bg=B, indent=PAD))
    lines.append(row([], width=cols, base_bg=GLASS))
    c1, c2, c3, c4 = 6, 40, 70, 100
    lab = [S(" " * c1, None, GLASS), S("HARNESS", FT, GLASS),
           S(" " * (c2 - c1 - 7), None, GLASS), S("BINARY", FT, GLASS),
           S(" " * (c3 - c2 - 6), None, GLASS), S("SESSIONS", FT, GLASS),
           S(" " * (c4 - c3 - 8), None, GLASS), S("CONDUIT", FT, GLASS)]
    lines.append(row(lab, width=cols, base_bg=GLASS))
    lines.append(row([], width=cols, base_bg=GLASS))
    for h in DEMO["harnesses"]:
        fill = GLASS
        sess = "1 queued" if h["hkey"] == "grok" else h["sessions"]
        state = ("SETUP" if h["state"] == "SETUP" else "READY")
        scol = AMB if state == "SETUP" else FT
        dot = S("○ ", AMB, fill) if state == "SETUP" else S("● ", DIM, fill)
        segs = [S("▌", HARNESS_COLOR[h["hkey"]], fill), S("  ", None, fill), dot,
                S(h["name"], TEXT, fill)]
        segs += [S(" " * (c2 - seglen(segs)), None, fill), S(h["bin"], FT, fill)]
        segs += [S(" " * (c3 - seglen(segs)), None, fill), S(sess, DIM, fill)]
        segs += [S(" " * (c4 - seglen(segs)), None, fill), S(h["conduit"], FT, fill)]
        segs += [S(" " * (cols - PAD - seglen(segs) - len(state)), None, fill),
                 S(state, scol, fill)]
        lines.append(row(segs, width=cols, base_bg=fill))
    lines.append(row([], width=cols, base_bg=GLASS))
    lines.append(row([S("▀" * cols, GLASS, VOID)], width=cols))
    while len(lines) < rows - 3:
        lines.append(vrow(cols))
    lines += keys2(cols, DEMO["keys_harness"])
    lines.append(vrow(cols))
    return lines[:rows]

# ---- task view: vertical rail + CLI pane --------------------------------
def rail_lines(st, H):
    w = RAIL_W
    inner = w - 2

    def rl(segs, fill=GLASS):
        return row(clip_segs(segs, inner), width=w, base_bg=fill, indent=2)

    sel_s = SESSIONS[st.rail_sel]
    top = [row([S("▄" * w, GLASS, VOID)], width=w)]
    top.append(rl([S("Repository evaluation", TEXT, bold=True)]))
    top.append(rl([S("/Users/me/GitHub/buzz", FT)]))
    top.append(rl([]))
    top.append(rl([S("TEAM", FT)]))
    top.append(rl([]))
    for i, s in enumerate(SESSIONS):
        sel = st.rail_sel == i
        fill = GLASS_HI if sel else GLASS
        live = s["state"] == "live"
        dot = S("●", ION, fill) if (sel and live) else (
              S("●", DIM, fill) if live else S("○", AMB, fill))
        if s["depth"] == 0 and i > 0:
            top.append(rl([]))
        if s["depth"] == 0:
            name_row = [S("▌", HARNESS_COLOR[s["hkey"]], fill),
                        S(" ", None, fill), dot, S(" ", None, fill),
                        S(s["name"], TEXT, fill, bold=sel)]
            h_ind = 4
        else:
            name_row = [S("▌", HARNESS_COLOR[s["hkey"]], fill),
                        S("  ", None, fill), S("└─ ", FT, fill), dot,
                        S(" ", None, fill), S(s["name"], TEXT, fill, bold=sel)]
            h_ind = 7
        top.append(row(name_row, width=w, base_bg=fill))
        sword = s["state"].upper()
        swcol = ION if (sel and live) else (FT if live else AMB)
        sub = [S("▌", HARNESS_COLOR[s["hkey"]], fill),
               S(" " * h_ind, None, fill), S(s["harness"], FT, fill)]
        sub += [S(" " * (w - 2 - seglen(sub) - len(sword)), None, fill),
                S(sword, swcol, fill)]
        top.append(row(sub, width=w, base_bg=fill))
    top.append(rl([]))
    top.append(rl([S(" a ", TEXT, KCAP), S(" subordinate   ", FT),
                   S(" m ", TEXT, KCAP), S(" manager", FT)]))

    bot = [rl([])]
    tele = [("WORKSPACE", ws_short(sel_s["ws"], inner - 1)),
            ("CONDUIT", sel_s["conduit"].lower()),
            ("TEAM", "4 sessions · 4 harnesses")]
    for label, value in tele:
        bot.append(rl([S(label, FT)]))
        bot.append(rl([S(value, DIM)]))
        bot.append(rl([]))
    bot.append(rl([S(" Ctrl+G ", TEXT, KCAP), S(" focus CLI", FT)]))
    bot.append(rl([]))
    bot.append(rl([S(" d ", TEXT, KCAP), S(" destroy task", FT)]))
    bot.append(rl([]))
    bot.append(row([S("▀" * w, GLASS, VOID)], width=w))

    fill_n = H - len(top) - len(bot)
    return top + [rl([]) for _ in range(max(0, fill_n))] + (
        bot if fill_n >= 0 else bot[-(H - len(top)):])

def feed_rows(entries, pw, base, accent, native):
    """Render feed entries as rows of width pw on the CLI canvas."""
    out = []

    def fr(segs, b=base):
        return row(clip_segs(segs, pw - 4), width=pw, base_bg=b, indent=2)

    for entry in entries:
        kind = entry[0]
        if kind == "meta" and native:
            continue
        if kind in ("user", "ok", "ask"):
            out.append(row([], width=pw, base_bg=base))
        if kind == "meta":
            out.append(fr([S(entry[1][0], accent), S(entry[1][1:], FT)]))
        elif kind == "user":
            out.append(fr([S("> ", DIM if native else FT),
                           S(entry[1], TEXT)]))
        elif kind == "say":
            out.append(fr([S("⏺ ", DIM), S(entry[1], TEXT)]))
        elif kind == "tool":
            out.append(fr([S("⏺ ", OK), S(entry[1], TEXT), S("(", FT),
                           S(entry[2], DIM), S(")", FT)]))
        elif kind == "result":
            out.append(fr([S("  ⎿  ", FT), S(entry[1], DIM)]))
        elif kind == "ok":
            out.append(fr([S("✓ ", OK), S(entry[1], DIM)]))
        elif kind == "ask":
            q = [S("○ ", AMB, AMB_GLASS), S(entry[1], TEXT, AMB_GLASS)]
            q += [S(" " * (pw - 4 - seglen(q) - len(entry[2])), None, AMB_GLASS),
                  S(entry[2], AMB, AMB_GLASS)]
            out.append(row(q, width=pw, base_bg=AMB_GLASS, indent=2))
    return out

def pane_lines(st, pw, H):
    s = SESSIONS[st.rail_sel]
    accent = HARNESS_COLOR[s["hkey"]]
    B = GLASS_HI
    head = [S(s["name"], TEXT, B, bold=True), S(" · ", FT, B),
            S(s["harness"], accent, B)]
    live = s["state"] == "live"
    right = [S(s["conduit"], FT, B), S("   ", None, B)]
    if live:
        right += [S("● ", ION, B), S("LIVE", ION, B), S("   ", None, B)]
    else:
        right += [S("◌ ", AMB, B), S("QUEUED", AMB, B), S("   ", None, B)]
    right += [S("c ", DIM, B),
              S("view: " + ("NATIVE" if st.view == "native" else "ACP"), FT, B)]
    gap = pw - PAD - seglen(head) - seglen(right) - PAD
    lines = [row([S("▄" * pw, B, VOID)], width=pw),
             row(head + [S(" " * max(1, gap), None, B)] + right,
                 width=pw, base_bg=B, indent=PAD)]

    if s["key"] == "ver":
        return lines + handoff_pane(st, pw, H - len(lines))

    base = CLI_BG if st.view == "native" else GLASS
    body_h = H - len(lines) - (5 if st.view == "native" else 3)
    feed = st.feeds.get(s["key"], [])
    rows_ = [row([], width=pw, base_bg=base)]
    if st.view == "native":
        hdr = f"✻ {s['harness']} · claude-fable-5 · {s['ws']}" \
            if s["hkey"] == "claude" else f"✻ {s['harness']} · {s['ws']}"
        rows_.append(row([S(hdr[0], accent, base), S(hdr[1:], FT, base)],
                         width=pw, base_bg=base, indent=2))
        rows_.append(row([], width=pw, base_bg=base))
    rows_ += feed_rows(feed, pw, base, accent, st.view == "native")
    rows_.append(row([], width=pw, base_bg=base))
    elapsed = (st.tick - st.spin_t0) // 10
    phrase = SPIN_PHRASES[s["key"]][st.phrase[s["key"]]]
    spin_tail = f"(esc to interrupt · {elapsed}s · ↓ {st.tokens/1000:.1f}k tokens)"
    rows_.append(row([S(SPIN_GLYPHS[st.spin_i] + " ", accent, base),
                      S(phrase, TEXT, base), S("  " + spin_tail, FT, base)],
                     width=pw, base_bg=base, indent=2))
    body = rows_[-body_h:] if len(rows_) > body_h else rows_ + \
        [row([], width=pw, base_bg=base) for _ in range(body_h - len(rows_))]
    lines += body

    cur = "█" if st.blink else " "
    if st.view == "native":
        bw = pw - 4
        lines.append(row([S("╭" + "─" * (bw - 2) + "╮", CLI_EDGE, base)],
                         width=pw, base_bg=base, indent=2))
        lines.append(row([S("│", CLI_EDGE, base), S(" > ", DIM, base),
                          S(cur, TEXT, base),
                          S(" " * (bw - 6), None, base), S("│", CLI_EDGE, base)],
                         width=pw, base_bg=base, indent=2))
        lines.append(row([S("╰" + "─" * (bw - 2) + "╯", CLI_EDGE, base)],
                         width=pw, base_bg=base, indent=2))
        lines.append(row([S("? for shortcuts", FT, base)],
                         width=pw, base_bg=base, indent=4))
    else:
        lines.append(row([S("❯ ", ION, GLASS_HI), S(cur, ION, GLASS_HI)],
                         width=pw, base_bg=GLASS_HI, indent=PAD))
        lines.append(row([], width=pw, base_bg=GLASS_HI))
    lines.append(row([S("▀" * pw, base if st.view == "native" else GLASS_HI, VOID)],
                     width=pw))
    return lines

def handoff_pane(st, pw, H):
    base = GLASS
    lines = [row([], width=pw, base_bg=base)]
    note = "HANDOFF-RESUME · Grok CLI does not stream. Prompts queue; results return on completion."
    lines.append(row([S("◌ ", AMB, base), S(note, DIM, base)],
                     width=pw, base_bg=base, indent=2))
    lines.append(row([], width=pw, base_bg=base))
    for kind, ts, text in DEMO["handoff"]["exchanges"]:
        ts = "17:31" if ts == "17:41" else ts
        if kind == "sent":
            lines.append(row([S("❯ ", DIM, base), S(ts + "   ", FT, base),
                              S(text, TEXT, base)], width=pw, base_bg=base, indent=2))
        elif kind == "ret":
            lines.append(row([S("  ⎿ ", FT, base), S(ts + "   ", FT, base),
                              S(text, DIM, base)], width=pw, base_bg=base, indent=2))
        else:
            q = [S("◌ ", AMB, GLASS_IN), S("Queued · resumes on completion", AMB, GLASS_IN)]
            lines.append(row([], width=pw, base_bg=base))
            lines.append(row(q, width=pw, base_bg=GLASS_IN, indent=2))
    lines.append(row([], width=pw, base_bg=base))
    cur = "▍" if st.blink else " "
    lines.append(row([S("❯ ", ION, GLASS_IN),
                      S(DEMO["handoff"]["cmd"], DIM, GLASS_IN),
                      S(" " + cur, ION, GLASS_IN)],
                     width=pw, base_bg=GLASS_IN, indent=2))
    while len(lines) < H - 1:
        lines.append(row([], width=pw, base_bg=base))
    lines.append(row([S("▀" * pw, base, VOID)], width=pw))
    return lines[: H]

def task2(st, cols, rows):
    lines = mh(cols, DEMO["task"]["crumb"])
    lines.append(vrow(cols))
    H = rows - len(lines) - 4
    pw = cols - RAIL_W - GAP
    rail = rail_lines(st, H)
    pane = pane_lines(st, pw, H)
    gap = bg(VOID) + " " * GAP
    for i in range(H):
        r = rail[i] if i < len(rail) else row([], width=RAIL_W)
        p = pane[i] if i < len(pane) else row([], width=pw)
        lines.append(r + gap + p + RESET)
    lines.append(vrow(cols))
    lines += keys2(cols, [("Enter", "Attach"), ("c", "Toggle view"),
                          ("a", "Add Subordinate"), ("m", "Add Manager"),
                          ("d", "Destroy"), ("T", "Home"),
                          ("Ctrl+G", "Focus CLI")])
    lines.append(vrow(cols))
    return lines[:rows]

def attach2(st, cols, rows):
    B = GLASS_IN
    rib = [S("▌", CLAUDE, B), S("  ", None, B),
           S("Lead · Claude Code", DIM, B)]
    mid = [S("owned-tui", FT, B)]
    tail = [S(" Ctrl+G ", TEXT, KCAP), S(" Return to TaskAI", FT, B)]
    rib += [S(" " * (cols * 2 // 3 - seglen(rib) - seglen(mid)), None, B)] + mid
    rib += [S(" " * (cols - PAD - seglen(rib) - seglen(tail)), None, B)] + tail
    lines = [row([S("▄" * cols, B, VOID)], width=cols),
             row(rib, width=cols, base_bg=B),
             row([S("▀" * cols, B, VOID)], width=cols),
             vrow(cols)]
    base = CLI_BG
    H = rows - len(lines)
    body_h = H - 6
    rows_ = [row([], width=cols, base_bg=base)]
    hdr = "✻ Claude Code · claude-fable-5 · /Users/me/GitHub/buzz"
    rows_.append(row([S(hdr[0], CLAUDE, base), S(hdr[1:], FT, base)],
                     width=cols, base_bg=base, indent=2))
    rows_.append(row([], width=cols, base_bg=base))
    rows_ += feed_rows(st.feeds["lead"], cols, base, CLAUDE, True)
    rows_.append(row([], width=cols, base_bg=base))
    elapsed = (st.tick - st.spin_t0) // 10
    rows_.append(row([S(SPIN_GLYPHS[st.spin_i] + " ", CLAUDE, base),
                      S("Drafting rollback dry-run plan…", TEXT, base),
                      S(f"  (esc to interrupt · {elapsed}s · ↓ {st.tokens/1000:.1f}k tokens)",
                        FT, base)], width=cols, base_bg=base, indent=2))
    body = rows_[-body_h:] if len(rows_) > body_h else rows_ + \
        [row([], width=cols, base_bg=base) for _ in range(body_h - len(rows_))]
    lines += body
    cur = "█" if st.blink else " "
    text = DEMO["attach"]["composer"][: st.typed]
    bw = cols - 4
    lines.append(row([S("╭" + "─" * (bw - 2) + "╮", CLI_EDGE, base)],
                     width=cols, base_bg=base, indent=2))
    lines.append(row([S("│", CLI_EDGE, base), S(" > ", DIM, base),
                      S(text, TEXT, base), S(cur, TEXT, base),
                      S(" " * max(0, bw - 7 - len(text)), None, base),
                      S("│", CLI_EDGE, base)], width=cols, base_bg=base, indent=2))
    lines.append(row([S("╰" + "─" * (bw - 2) + "╯", CLI_EDGE, base)],
                     width=cols, base_bg=base, indent=2))
    lines.append(row([S("? for shortcuts", FT, base),
                      S("   ⏵⏵ accept edits on (shift+tab to cycle)", FT, base)],
                     width=cols, base_bg=base, indent=4))
    lines.append(row([S("▀" * cols, base, VOID)], width=cols))
    return lines[:rows]

# ---- overlays -----------------------------------------------------------
def overlay(base_lines, panel, cols, top):
    """Dim the base and splice a floating panel (list of pw-wide lines) over it."""
    pw = panel_width(panel)
    ind = (cols - pw) // 2
    out = [dim_line(l) for l in base_lines]
    for i, prow_ in enumerate(panel):
        r = top + i
        if r >= len(out):
            break
        out[r] = slice_cells(out[r], 0, ind) + prow_ + \
            slice_cells(out[r], ind + pw, cols - ind - pw)
    return out

def panel_width(panel):
    return len(SGR.sub("", panel[0]))

def drawer_panel(st, title, fields, note, keys, pw=58):
    B = GLASS_HI
    lines = [row([S("▄" * pw, B, VOID)], width=pw)]
    lines.append(row([S(title, TEXT, B, bold=True)], width=pw, base_bg=B, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    for label, value, focus in fields:
        lines.append(row([S(label.upper(), FT, GLASS)], width=pw,
                         base_bg=GLASS, indent=2))
        cur = [S("▍" if (focus and st.blink) else " ", ION, GLASS_IN)] if focus else []
        val = [S(" ", None, GLASS_IN),
               S(value, TEXT if focus else DIM, GLASS_IN)] + cur
        val += [S(" " * (pw - 4 - seglen(val)), None, GLASS_IN)]
        lines.append(row(val, width=pw, base_bg=GLASS, indent=2))
        lines.append(row([], width=pw, base_bg=GLASS))
    for seg in note:
        lines.append(row(seg, width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    ks = []
    for i, (k, l) in enumerate(keys):
        if i:
            ks.append(S("   ", None, GLASS))
        ks += [S(" " + k + " ", TEXT, KCAP), S(" " + l, FT, GLASS)]
    lines.append(row(ks, width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    lines.append(row([S("▀" * pw, GLASS, VOID)], width=pw))
    return lines

def newtask_panel(st):
    nt = DEMO["newtask"]
    return drawer_panel(st, nt["title"], nt["fields"],
                        [[S(nt["note"][:54], FT, GLASS)]], nt["keys"])

def sub_panel(st):
    sb = DEMO["subordinate"]
    note = [[S("CODE WILL LIVE AT", FT, GLASS)],
            [S(sb["preview"], DIM, GLASS)],
            [S("", None, GLASS)],
            [S(sb["note"][:54], FT, GLASS)]]
    return drawer_panel(st, sb["title"], sb["fields"], note, sb["keys"])

def destroy_panel(st):
    d = DEMO["destroy"]
    pw = 62
    lines = [row([S("▄" * pw, RED_GLASS, VOID)], width=pw)]
    lines.append(row([S(d["title"], RED, RED_GLASS, bold=True)],
                     width=pw, base_bg=RED_GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    for b in d["blast"]:
        lines.append(row([S("● ", RED, GLASS), S(b, TEXT, GLASS)],
                         width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    for k in d["kept"]:
        lines.append(row([S("✓ ", FT, GLASS), S(k, DIM, GLASS)],
                         width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    lines.append(row([S("Type ", FT, GLASS), S(d["confirm_value"], TEXT, GLASS, bold=True),
                      S(" to confirm", FT, GLASS)], width=pw, base_bg=GLASS, indent=2))
    cur = "▍" if st.blink else " "
    val = [S(" " + d["confirm_value"], TEXT, GLASS_IN), S(cur, ION, GLASS_IN)]
    val += [S(" " * (pw - 4 - seglen(val)), None, GLASS_IN)]
    lines.append(row(val, width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    lines.append(row([S(" Enter ", TEXT, KCAP), S(" Destroy", RED, GLASS),
                      S("    ", None, GLASS), S(" Esc ", TEXT, KCAP),
                      S(" Cancel", FT, GLASS)],
                     width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    lines.append(row([S("▀" * pw, GLASS, VOID)], width=pw))
    return lines

def help_panel(st):
    h = DEMO["help"]
    pw = 68
    B = GLASS_HI
    lines = [row([S("▄" * pw, B, VOID)], width=pw)]
    lines.append(row([S("Help", TEXT, B, bold=True)], width=pw, base_bg=B, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    key, desc = h["boundary"]
    lines.append(row([S(" " + key + " ", TEXT, GLASS_IN, bold=True),
                      S("  " + desc[:56], DIM, GLASS)],
                     width=pw, base_bg=GLASS, indent=2))
    lines.append(row([S(desc[56:][:60], DIM, GLASS)], width=pw,
                     base_bg=GLASS, indent=8))
    lines.append(row([], width=pw, base_bg=GLASS))
    g, t = h["sections"][0], h["sections"][1]
    lines.append(row([S(g[0], FT, GLASS), S(" " * (30 - len(g[0])), None, GLASS),
                      S(t[0], FT, GLASS)], width=pw, base_bg=GLASS, indent=4))
    for i in range(max(len(g[1]), len(t[1]))):
        segs = []
        if i < len(g[1]):
            k, l = g[1][i]
            segs += [S(f"{k:>5} ", DIM, GLASS), S(l, FT, GLASS)]
        pad = 34 - seglen(segs)
        segs.append(S(" " * pad, None, GLASS))
        if i < len(t[1]):
            k, l = t[1][i]
            segs += [S(f"{k:>5} ", DIM, GLASS), S(l, FT, GLASS)]
        lines.append(row(segs, width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    lines.append(row([S("CONDUITS", FT, GLASS)], width=pw, base_bg=GLASS, indent=4))
    for name, desc in h["conduits"]:
        lines.append(row([S(f"{name:>16}", DIM, GLASS), S("  " + desc[:46], FT, GLASS)],
                         width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    lines.append(row([S(" Esc ", TEXT, KCAP), S(" Close", FT, GLASS)],
                     width=pw, base_bg=GLASS, indent=2))
    lines.append(row([], width=pw, base_bg=GLASS))
    lines.append(row([S("▀" * pw, GLASS, VOID)], width=pw))
    return lines

# ---- attention (t2 task view via same layout) ---------------------------
def attention2(st, cols, rows):
    lines = mh(cols, DEMO["attention"]["crumb"])
    lines.append(vrow(cols))
    H = rows - len(lines) - 4
    pw = cols - RAIL_W - GAP
    w = RAIL_W

    def rl(segs, fill=GLASS):
        return row(clip_segs(segs, w - 4), width=w, base_bg=fill, indent=2)

    a = DEMO["attention"]["sessions"][0]
    afill = tuple(int(GLASS[i] + (AMB_GLASS[i] - GLASS[i]) * 0.3) for i in range(3))
    rail = [row([S("▄" * w, GLASS, VOID)], width=w),
            rl([S("Production release", TEXT, bold=True)]),
            rl([S("/Users/me/GitHub/shop", FT)]),
            rl([])]
    rail.append(row([S("▌", HARNESS_COLOR[a["hkey"]], afill), S(" ", None, afill),
                     S("○ ", AMB, afill), S(a["name"], TEXT, afill, bold=True)],
                    width=w, base_bg=afill))
    sub = [S("▌", HARNESS_COLOR[a["hkey"]], afill), S("   ", None, afill),
           S(a["harness"], FT, afill)]
    sub += [S(" " * (w - 2 - seglen(sub) - 9), None, afill), S("ATTENTION", AMB, afill)]
    rail.append(row(sub, width=w, base_bg=afill))
    while len(rail) < H - 1:
        rail.append(rl([]))
    rail.append(row([S("▀" * w, GLASS, VOID)], width=w))

    B = GLASS_HI
    head = [S("Ship", TEXT, B, bold=True), S(" · ", FT, B), S("OpenCode", OPENC, B)]
    right = [S("SHARED-LIVE", FT, B), S("   ", None, B), S("○ ", AMB, B),
             S("ATTENTION", AMB, B)]
    gap = pw - PAD - seglen(head) - seglen(right) - PAD
    pane = [row([S("▄" * pw, B, VOID)], width=pw),
            row(head + [S(" " * max(1, gap), None, B)] + right,
                width=pw, base_bg=B, indent=PAD)]
    body = feed_rows(st.feeds["ship"], pw, GLASS, OPENC, False)
    pane.append(row([], width=pw, base_bg=GLASS))
    pane += body
    pane.append(row([], width=pw, base_bg=GLASS))
    ans = "y" if not st.answered else "y ✓ approved"
    cur = "▍" if (st.blink and not st.answered) else " "
    pane.append(row([S("❯ ", ION, GLASS_HI), S(ans, TEXT, GLASS_HI),
                     S(cur, ION, GLASS_HI)], width=pw, base_bg=GLASS_HI, indent=PAD))
    while len(pane) < H - 1:
        pane.append(row([], width=pw, base_bg=GLASS))
    pane.append(row([S("▀" * pw, GLASS_HI, VOID)], width=pw))

    gapstr = bg(VOID) + " " * GAP
    for i in range(H):
        r = rail[i] if i < len(rail) else row([], width=w)
        p = pane[i] if i < len(pane) else row([], width=pw)
        lines.append(r + gapstr + p + RESET)
    lines.append(vrow(cols))
    lines += keys2(cols, [("y", "Approve"), ("n", "Decline"), ("T", "Home"),
                          ("Ctrl+G", "Focus CLI")])
    lines.append(vrow(cols))
    return lines[:rows]

# ---- render / input -----------------------------------------------------
def render(st, cols, rows):
    if st.screen == "boot":
        return boot2(st, cols, rows)
    if st.screen == "home":
        return home2(st, cols, rows)
    if st.screen == "harnesses":
        return harness2(st, cols, rows)
    if st.screen == "task":
        return task2(st, cols, rows)
    if st.screen == "attach":
        return attach2(st, cols, rows)
    if st.screen == "attention":
        return attention2(st, cols, rows)
    if st.screen in ("newtask", "subordinate", "destroy", "help"):
        base_fn = home2 if st.prev in ("home", "harnesses") else task2
        base = base_fn(st, cols, rows)
        panel = {"newtask": newtask_panel, "subordinate": sub_panel,
                 "destroy": destroy_panel, "help": help_panel}[st.screen](st)
        return overlay(base, panel, cols, top=6)
    return home2(st, cols, rows)

def handle(st, key):
    sc = st.screen
    if key == "\x03":
        return False
    if sc == "boot":
        st.screen = "home"
        return True
    if sc == "attach":
        if key == "\x07":
            st.screen = "task"
        return True
    if key == "q":
        return False
    if key == "T":
        st.screen = "home"
        return True
    if key == "?":
        st.prev, st.screen = sc, "help"
        return True
    if sc == "home":
        if key == "UP":
            st.home_sel = max(0, st.home_sel - 1)
        elif key == "DOWN":
            st.home_sel = min(3, st.home_sel + 1)
        elif key == "h":
            st.screen = "harnesses"
        elif key == "n":
            st.prev, st.screen = sc, "newtask"
        elif key == "ENTER":
            st.screen = {0: "task", 1: "attention", 2: "task", 3: "newtask"}[st.home_sel]
            if st.screen == "newtask":
                st.prev = "home"
    elif sc == "task":
        if key == "UP":
            st.rail_sel = max(0, st.rail_sel - 1)
        elif key == "DOWN":
            st.rail_sel = min(len(SESSIONS) - 1, st.rail_sel + 1)
        elif key == "c":
            st.view = "acp" if st.view == "native" else "native"
        elif key in ("ENTER", "\x07"):
            if SESSIONS[st.rail_sel]["state"] == "live":
                st.screen = "attach"
        elif key == "a":
            st.prev, st.screen = sc, "subordinate"
        elif key == "d":
            st.prev, st.screen = sc, "destroy"
        elif key == "h":
            st.screen = "harnesses"
    elif sc in ("newtask", "subordinate", "destroy", "help"):
        if key in ("ESC", "ENTER"):
            st.screen = st.prev if st.prev else "home"
    elif sc == "harnesses":
        if key == "ESC":
            st.screen = "home"
    elif sc == "attention":
        if key == "y":
            st.answered = True
        elif key in ("n", "ESC"):
            st.screen = "home"
        elif key == "\x07":
            st.screen = "attach"
    return True

def parse_keys(data):
    keys, i = [], 0
    while i < len(data):
        c = data[i]
        if c == "\x1b":
            if data[i:i + 3] == "\x1b[A":
                keys.append("UP"); i += 3; continue
            if data[i:i + 3] == "\x1b[B":
                keys.append("DOWN"); i += 3; continue
            if data[i:i + 2] == "\x1b[":
                i += 3; continue
            keys.append("ESC"); i += 1; continue
        if c in ("\r", "\n"):
            keys.append("ENTER")
        else:
            keys.append(c)
        i += 1
    return keys

def blit(lines, cols, rows):
    body = lines[:rows]
    body += [bg(VOID) + " " * cols + RESET] * max(0, rows - len(body))
    sys.stdout.write("\x1b[?2026h\x1b[H" + "\r\n".join(body) + "\x1b[?2026l")
    sys.stdout.flush()

def main():
    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    sys.stdout.write("\x1b[?1049h\x1b[?25l\x1b[2J")
    st = St()
    try:
        tty.setraw(fd)
        running = True
        while running:
            advance(st)
            size = shutil.get_terminal_size((120, 40))
            cols, rows = max(110, size.columns), max(30, size.lines)
            blit(render(st, cols, rows), cols, rows)
            r, _, _ = select.select([fd], [], [], 0.1)
            if r:
                data = os.read(fd, 64).decode("utf-8", "ignore")
                for key in parse_keys(data):
                    running = handle(st, key)
                    if not running:
                        break
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)
        sys.stdout.write("\x1b[?25h\x1b[?1049l" + RESET)
        sys.stdout.flush()

if __name__ == "__main__":
    main()
