# ABOUTME: FILL design system + demo data model for the TaskAI end-to-end terminal experience.
# ABOUTME: Frame modules import * from here; the language is instrument glass — slabs, stripes, sparklines, zero line borders.

import os
import re
import sys
import shutil
import datetime

W = 110  # frame width in cells

# ---- brand palette ------------------------------------------------------
VOID   = (4, 6, 10)        # the OLED ground
TEXT   = (233, 238, 246)   # primary
DIM    = (126, 138, 160)   # secondary
FT     = (102, 115, 140)   # faint / tertiary
ION    = (124, 214, 255)   # THE accent — live / focus / selection only
AMB    = (255, 194, 75)    # attention only
OK     = (127, 216, 164)   # success only
RED    = (242, 108, 108)   # destroy only
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

def show(lines):
    cols = shutil.get_terminal_size((W, 40)).columns
    margin = max(0, (cols - W) // 2)
    pad = " " * margin
    for ln in lines:
        sys.stdout.write(pad + ln + "\n")
    sys.stdout.write(RESET)
    sys.stdout.flush()

def clock():
    return datetime.datetime.now().strftime("%H:%M:%S")

def verify(lines):
    """Every rendered line must be exactly W cells after ANSI stripping."""
    strip = re.compile(r"\x1b\[[0-9;]*m")
    bad = [(i, len(strip.sub("", ln))) for i, ln in enumerate(lines)
           if len(strip.sub("", ln)) != W]
    if bad:
        sys.stderr.write(f"WIDTH FAIL {bad[:8]}\n")
        sys.exit(1)

# ---- FILL design constants ----------------------------------------------
GLASS     = (17, 24, 38)   # slab fill
GLASS_HI  = (25, 35, 54)   # raised fill — focus, headers, control zones
GLASS_IN  = (9, 13, 22)    # inset fill — user turns, telemetry, input fields
AMB_GLASS = (70, 53, 21)   # amber glass — attention lamp / chips
RED_GLASS = (64, 24, 24)   # red glass — destroy zone only
PAD       = 3              # interior side padding (rhythm unit)
GUTTER    = 2              # pure-void rows between slabs
HARNESS_COL = 38           # harness / second column stop
SPARK_COL   = 77           # sparkline / third column stop

HARNESS_COLOR = {"claude": CLAUDE, "codex": CODEX, "opencode": OPENC,
                 "grok": GROK, "aider": DIM}

def dull(rgb, k=0.38):
    """Pull a color toward the void — for content dimmed under an overlay."""
    return tuple(int(VOID[i] + (rgb[i] - VOID[i]) * k) for i in range(3))

def soft_top(fill):
    """Half-block upper edge: reads as a rounded slab top on the void."""
    return row([S("▄" * W, fill, VOID)])

def soft_bot(fill):
    """Half-block lower edge."""
    return row([S("▀" * W, fill, VOID)])

def soft_top_w(fill, indent, width):
    """Partial-width slab top (for drawers/modals floating over a screen)."""
    return row([S("▄" * width, fill, VOID)], indent=indent)

def soft_bot_w(fill, indent, width):
    return row([S("▀" * width, fill, VOID)], indent=indent)

def masthead(crumb, right, lamp=None):
    """The HUD strip. right = [(text, color)] · lamp = amber chip text or None."""
    B = GLASS_HI
    hud = [S("TASKAI", TEXT, B, bold=True), S("   ", None, B), S(crumb, FT, B)]
    rsegs = []
    for i, (t, colr) in enumerate(right):
        if i:
            rsegs.append(S(" · ", FT, B))
        rsegs.append(S(t, colr, B))
    if lamp:
        rsegs += [S(" · ", FT, B), S(" " + lamp + " ", AMB, AMB_GLASS)]
    rsegs += [S("   ", None, B), S(clock(), TEXT, B)]
    gap = W - PAD - seglen(hud) - seglen(rsegs) - PAD
    return [soft_top(B),
            row(hud + [S(" " * gap, None, B)] + rsegs, base_bg=B, indent=PAD),
            soft_bot(B)]

def keysrow(pairs, indent=6):
    """Quiet key hints on the void: key DIM · label FT · 3-cell gaps."""
    segs = []
    for i, (key, label) in enumerate(pairs):
        if i:
            segs.append(S("   ", None, VOID))
        segs += [S(key, DIM, VOID), S(" ", None, VOID), S(label, FT, VOID)]
    return row(segs, base_bg=VOID, indent=indent)

# ---- demo data model (fixed — the one consistent world) ------------------
DEMO = {
    "hud_home":  [("4 LIVE", ION), ("1 QUEUED", DIM), ("2 TASKS ACTIVE", DIM)],
    "lamp": "ATTENTION · 1",

    # boot
    "tagline": "Your agents. Your terminal. One boundary.",
    "probes": [
        ("Claude Code", "claude",   "ready"),
        ("Codex",       "codex",    "ready"),
        ("OpenCode",    "opencode", "ready"),
        ("Grok CLI",    "grok",     "ready"),
        ("Aider",       "aider",    "needs setup"),
    ],
    "boot_meter": "4 of 5 harnesses ready",

    # home board
    "tasks": [
        {"name": "Repository evaluation", "status": "WORKING", "sel": True,
         "managers": [("Lead", "Claude Code", "claude"), ("Delivery", "Codex", "codex")],
         "spark": "▂▄▆▅▃▆▇▅▄▆▅▇", "meta": "4 sessions · buzz"},
        {"name": "Production release", "status": "NEEDS ATTENTION", "sel": False,
         "managers": [("Ship", "OpenCode", "opencode")],
         "spark": "▂▃▂▄▃▂▃▄▂▃▂▂", "meta": "1 session · shop"},
        {"name": "API migration", "status": "COMPLETE", "sel": False,
         "managers": [("Migrator", "Grok CLI", "grok"), ("Docs", "Codex", "codex")],
         "spark": "▁▁▁▁▁▁▁▁▁▁▁▁", "meta": "2 sessions · atlas"},
    ],
    "keys_home": [("Enter", "Open"), ("n", "New task"), ("h", "Harnesses"),
                  ("/", "Find"), ("?", "Help")],

    # harness bay
    "harnesses": [
        {"name": "Claude Code", "hkey": "claude",   "bin": "claude · v2.1",  "sessions": "1 live", "conduit": "owned-tui",      "state": "READY"},
        {"name": "Codex",       "hkey": "codex",    "bin": "codex · v0.9",   "sessions": "1 live", "conduit": "shared-live",    "state": "READY"},
        {"name": "OpenCode",    "hkey": "opencode", "bin": "opencode · v1.4","sessions": "2 live", "conduit": "shared-live",    "state": "READY"},
        {"name": "Grok CLI",    "hkey": "grok",     "bin": "grok · v0.3",    "sessions": "1 active", "conduit": "handoff-resume", "state": "READY"},
        {"name": "Aider",       "hkey": "aider",    "bin": "not found on PATH", "sessions": "—",  "conduit": "—",              "state": "SETUP"},
    ],
    "keys_harness": [("p", "Probe"), ("c", "Configure"), ("+", "Custom CLI"),
                     ("T", "Home"), ("?", "Help")],

    # new task drawer (over dimmed home)
    "newtask": {
        "title": "New task",
        "fields": [
            ("Name",      "Checkout resilience", True),
            ("Workspace", "/Users/me/GitHub/shop", False),
            ("Manager",   "Claude Code · owned-tui", False),
            ("Base",      "main", False),
        ],
        "note": "Manager session starts in the workspace root. Add subordinates any time.",
        "keys": [("Enter", "Create"), ("Tab", "Next field"), ("Esc", "Cancel")],
    },

    # task view (t1) — the approved frame
    "task": {
        "crumb": "TASK · REPOSITORY EVALUATION",
        "sessions": [
            {"name": "Lead",         "harness": "Claude Code", "hkey": "claude",   "state": "live",    "focus": True,  "depth": 0},
            {"name": "Architecture", "harness": "OpenCode",    "hkey": "opencode", "state": "live",    "focus": False, "depth": 1},
            {"name": "Delivery",     "harness": "Codex",       "hkey": "codex",    "state": "live",    "focus": False, "depth": 0},
            {"name": "Verification", "harness": "Grok CLI",    "hkey": "grok",     "state": "handoff", "focus": False, "depth": 1},
        ],
        "telemetry": [("WORKSPACE", "/Users/me/GitHub/buzz"),
                      ("CONDUIT", "owned-tui"),
                      ("TEAM", "4 sessions · 4 harnesses")],
        "transcript": [
            ("meta",   "✻ Claude Code · /Users/me/GitHub/buzz"),
            ("user",   "Evaluate this repository for release readiness."),
            ("say",    "I'll run the evaluation in three passes — structure, auth, gates."),
            ("tool",   "Bash", "git log --oneline -6"),
            ("result", "094f2aa docs: focus vision on terminal experience …"),
            ("tool",   "Read", "docs/release-gates.md"),
            ("result", "Read 214 lines"),
            ("tool",   "Bash", "taskai context tree --format text"),
            ("result", "Task T1 → you (Lead) → Architecture · peer Delivery"),
            ("say",    "Architecture is on the auth worktree — folding its findings in."),
            ("tool",   "Update", "readiness.md"),
            ("result", "Added 3 sections · release gates drafted"),
            ("ok",     "Gate review updated · waiting on Verification"),
            ("spin",   "Synthesizing…", "esc to interrupt"),
            ("prompt", ""),
        ],
        "keys": [("Enter", "Attach"), ("a", "Add Subordinate"), ("m", "Add Manager"),
                 ("d", "Destroy"), ("T", "Home"), ("Ctrl+G", "Focus CLI")],
    },

    # attached / CLI focus (owned-tui, full bleed)
    "attach": {
        "ribbon_left": "Lead · Claude Code",
        "ribbon_mode": "owned-tui",
        "ribbon_hint": ("Ctrl+G", "Return to TaskAI"),
        "transcript_extra": [
            ("user",   "Fold Architecture's auth findings into the gate review."),
            ("say",    "Merging the worktree notes — two gates change status."),
            ("tool",   "Read", "../buzz-worktrees/auth-pass/findings.md"),
            ("result", "Read 96 lines · 2 blocking issues resolved upstream"),
            ("tool",   "Update", "readiness.md"),
            ("result", "Auth gate: PASS · Rollback gate: needs a dry run"),
            ("ok",     "Gate review v2 written · 1 gate outstanding"),
            ("spin",   "Drafting rollback dry-run plan…", "esc to interrupt"),
        ],
        "composer": "Schedule the dry run against staging tonight",
    },

    # add subordinate drawer (over dimmed task view)
    "subordinate": {
        "title": "Add subordinate — under Lead",
        "fields": [
            ("Role",     "Verification", True),
            ("Harness",  "Grok CLI · handoff-resume", False),
            ("Base",     "main", False),
            ("Worktree", "verification-pass", False),
        ],
        "preview": "/Users/me/GitHub/buzz-worktrees/verification-pass",
        "note": "Runs in an isolated worktree. Repo and history are preserved on destroy.",
        "keys": [("Enter", "Create"), ("Tab", "Next field"), ("Esc", "Cancel")],
    },

    # grok handoff-resume (never shown as live)
    "handoff": {
        "crumb": "TASK · REPOSITORY EVALUATION · VERIFICATION",
        "banner": "HANDOFF-RESUME · Grok CLI does not stream. Prompts queue; results return on completion.",
        "exchanges": [
            ("sent",  "17:02", "Verify release gates against docs/release-gates.md."),
            ("ret",   "17:09", "Pass 1 complete — 3 gates checked, 1 flagged: rollback untested."),
            ("sent",  "17:41", "Re-check rollback gate after the staging dry run."),
            ("wait",  "", "Queued · resumes on completion"),
        ],
        "cmd": "taskai session prompt verification --stdin --wait",
        "keys": [("Enter", "Send prompt"), ("r", "Refresh"), ("T", "Home"),
                 ("Ctrl+G", "Focus CLI")],
    },

    # attention answered in place (t2 · Production release)
    "attention": {
        "crumb": "TASK · PRODUCTION RELEASE",
        "sessions": [
            {"name": "Ship", "harness": "OpenCode", "hkey": "opencode",
             "state": "attention", "focus": True, "depth": 0},
        ],
        "transcript": [
            ("meta",   "✻ OpenCode · /Users/me/GitHub/shop"),
            ("say",    "Release candidate v2.4.0 built and smoke-tested."),
            ("tool",   "Bash", "git tag v2.4.0"),
            ("result", "tag created"),
            ("ask",    "Push release tag v2.4.0 to origin?", "[y/N]"),
            ("prompt", "y"),
        ],
        "note": "Answer lands in the CLI exactly as if you typed it there.",
        "keys": [("y", "Approve"), ("n", "Decline"), ("Ctrl+G", "Focus CLI"),
                 ("T", "Home")],
    },

    # destroy modal (over dimmed task view, t3)
    "destroy": {
        "title": "Destroy task — API migration",
        "blast": ["2 sessions will be closed", "Task record removed from TaskAI"],
        "kept":  ["Repository untouched", "Worktrees preserved", "Git history preserved"],
        "confirm_label": "Type the task name to confirm",
        "confirm_value": "api-migration",
        "keys": [("Enter", "Destroy"), ("Esc", "Cancel")],
    },

    # help overlay
    "help": {
        "title": "Help",
        "boundary": ("Ctrl+G", "The boundary — the only chord TaskAI reserves. Everything else belongs to your CLI."),
        "sections": [
            ("GLOBAL",  [("T", "Home"), ("n", "New task"), ("h", "Harnesses"), ("/", "Find"), ("?", "Help")]),
            ("TASK",    [("Enter", "Attach"), ("a", "Add Subordinate"), ("m", "Add Manager"), ("d", "Destroy")]),
            ("CONDUITS", []),
        ],
        "conduits": [
            ("owned-tui",      "TaskAI hosts the CLI's own interface, full fidelity"),
            ("shared-live",    "The CLI streams live; you type into it directly"),
            ("handoff-resume", "Prompts queue; results return on completion — never shown as live"),
        ],
        "keys": [("Esc", "Close")],
    },
}
