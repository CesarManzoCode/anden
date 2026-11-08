#!/usr/bin/env python3
"""C24: the alerts of ops/alerts.yml are valid and their tests pass.

Runs `promtool check rules ops/alerts.yml` and `promtool test rules ops/alerts_test.yml`, and keeps their output in c24/.
Passes when both exit 0. It needs no datacenter.
"""
import shutil
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c24"
ALERTS = lib.ROOT / "ops" / "alerts.yml"
TESTS = lib.ROOT / "ops" / "alerts_test.yml"


def promtool(log, *args):
    p = subprocess.run(["promtool", *args], capture_output=True, text=True, stdin=subprocess.DEVNULL, cwd=lib.ROOT)
    (RAW / log).write_text(p.stdout + p.stderr)
    return p.returncode


def main():
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    check_rc = promtool("check_rules.log", "check", "rules", str(ALERTS))
    test_rc = promtool("test_rules.log", "test", "rules", str(TESTS))
    lib.write_report("c24", {"check_rules_rc": check_rc, "test_rules_rc": test_rc})
    if check_rc != 0 or test_rc != 0:
        print(f"C24 failed: check rules exit {check_rc}, test rules exit {test_rc}; see {RAW}", file=sys.stderr)
        return 1
    print("C24 ok: check rules and test rules exit 0")
    return 0


if __name__ == "__main__":
    sys.exit(main())
