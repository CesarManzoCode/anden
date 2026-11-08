#!/usr/bin/env python3
"""C29: the support code is smaller than the product code, counted with cloc as D27 says.

acceptance/loc_paths.txt declares the paths of each class. Each class is one `cloc --json --quiet --fullpath` over its paths,
without `*_test.go` and without the directories of the other class nested inside them (`--not-match-d`). The figure of each
class is SUM.code. Passes if support_loc < product_loc; it also reports the ratio against the 15 % reference of the method,
which is not a limit.
"""
import json
import re
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

LOC_PATHS = lib.ROOT / "acceptance" / "loc_paths.txt"
RAW = lib.REPORT_DIR / "c29"
CLASSES = ("product", "support")
REFERENCE_RATIO = 0.15


def loc_paths():
    """{class: [path]} from loc_paths.txt."""
    paths = {c: [] for c in CLASSES}
    for line in LOC_PATHS.read_text().splitlines():
        if not line.strip() or line.startswith("#"):
            continue
        cls, path = line.split()
        if cls not in paths:
            raise ValueError(f"loc_paths.txt: unknown class {cls!r}")
        paths[cls].append(path.strip("/"))
    return paths


def cloc_cmd(roots, excluded):
    return ["cloc", "--json", "--quiet", "--fullpath", r"--not-match-f=_test\.go$",
            *[f"--not-match-d=^{re.escape(p)}(/|$)" for p in excluded], *roots]


def main():
    RAW.mkdir(parents=True, exist_ok=True)
    paths = loc_paths()
    raw, loc = {}, {}
    for cls in CLASSES:
        others = [p for other in CLASSES if other != cls for p in paths[other]]
        excluded = [p for p in others if any(p.startswith(r + "/") for r in paths[cls])]
        cmd = cloc_cmd(paths[cls], excluded)
        p = subprocess.run(cmd, cwd=lib.ROOT, capture_output=True, text=True, stdin=subprocess.DEVNULL)
        if p.returncode != 0:
            print(f"C29 failed: {' '.join(cmd)}: rc={p.returncode}: {p.stderr}", file=sys.stderr)
            return 1
        out = json.loads(p.stdout)
        raw[cls] = {"command": cmd, "output": out}
        loc[cls] = out["SUM"]["code"]
    (RAW / "cloc.json").write_text(json.dumps(raw, indent=2) + "\n")
    ratio = loc["support"] / loc["product"] if loc["product"] else None
    lib.write_report("c29", {
        "product_loc": loc["product"], "support_loc": loc["support"], "ratio": ratio,
        "paths": paths, "reference_ratio": REFERENCE_RATIO,
        "ratio_to_reference": ratio / REFERENCE_RATIO if ratio is not None else None,
    })
    if ratio is None or ratio >= 1:
        print(f"C29 failed: support {loc['support']} lines, product {loc['product']}", file=sys.stderr)
        return 1
    print(f"C29 ok: support {loc['support']} lines, product {loc['product']}, ratio {ratio:.3f}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
