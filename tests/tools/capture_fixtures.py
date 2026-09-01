# ABOUTME: Captures deterministic ANSI frames from the Python prototype as parity fixtures.
# ABOUTME: Run from repo root: python3 tests/tools/capture_fixtures.py — writes tests/fixtures/*.ans

import os
import sys

os.environ["COLORTERM"] = "truecolor"
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "prototype"))

import taskai_tui  # noqa: E402

taskai_tui.clock = lambda: "12:00:00"

SIZES = [(110, 30), (209, 51), (150, 42)]
SPARK_T1 = [1, 3, 5, 4, 2, 5, 6, 4, 3, 5, 4, 6]
SPARK_T2 = [1, 2, 1, 3, 2, 1, 2, 3, 1, 2, 1, 1]


def fixture_state(screen):
    st = taskai_tui.St()
    st.screen = screen
    st.tick = 0
    st.blink = True
    st.boot_k = 22
    st.home_sel = 0
    st.sparks["t1"] = list(SPARK_T1)
    st.sparks["t2"] = list(SPARK_T2)
    return st


def main():
    out_dir = os.path.join(os.path.dirname(__file__), "..", "fixtures")
    os.makedirs(out_dir, exist_ok=True)
    for screen in ("boot", "home"):
        for cols, rows in SIZES:
            frame = taskai_tui.render(fixture_state(screen), cols, rows)
            path = os.path.join(out_dir, f"{screen}-{cols}x{rows}.ans")
            with open(path, "w") as f:
                f.write("\n".join(frame))
            print(f"wrote {path} ({len(frame)} lines)")


if __name__ == "__main__":
    main()
