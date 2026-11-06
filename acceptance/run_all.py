#!/usr/bin/env python3
"""Runs every acceptance criterion. Accepts exit 0 and 77 and fails on anything else.

ANDEN_ACCEPT_SKIP lists, separated by spaces, the criteria to leave out: c28 by number (c08 is both of its
halves), c08cold by report or c08_cold by script. ANDEN_CACHE is the cache of images and runtime-tools.
"""
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib


def main():
    skip = set(os.environ.get("ANDEN_ACCEPT_SKIP", "").split())
    env = {**os.environ, "ANDEN_CACHE": str(lib.cache_dir())}
    scripts = sorted((lib.ROOT / "acceptance").glob("c[0-9][0-9]_*.py"))
    if not scripts:
        print("no acceptance scripts found", flush=True)
        return 1
    failed = []
    for script in scripts:
        if {script.stem[:3], script.stem, lib.report_name(script)} & skip:
            print(f"skip {script.name}: in ANDEN_ACCEPT_SKIP", flush=True)
            continue
        print(f"run {script.name}", flush=True)
        rc = subprocess.run([sys.executable, str(script)], cwd=lib.ROOT, env=env, stdin=subprocess.DEVNULL).returncode
        print(f"{script.name}: exit {rc}", flush=True)
        if rc not in (0, 77):
            failed.append(script.name)
    if failed:
        print(f"failed: {' '.join(failed)}", flush=True)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
