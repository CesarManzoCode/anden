#!/usr/bin/env python3
"""C2: the desired and observed state survive a SIGKILL of the control plane.

Creates one service per node (each one asks for all the CPU of a node, so first fit spreads them), takes `anden
status --json` and the raw cgroup.procs of each instance, kills andend with SIGKILL from the host and waits for the
new andend. Passes when the snapshot after equals the one before, processes included, so nothing was started again,
and every service still answers through the edge.
"""
import json
import os
import shutil
import signal
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c02"
NAMES = [f"dur-{i}" for i in range(1, lib.NODE_COUNT + 1)]
RESTART_TIMEOUT_SECONDS = 30
SETTLE_SECONDS = 3


def snapshot(dc):
    status = dc.status()
    procs = {s["spec"]["name"]: (dc.cgroup(s) / "cgroup.procs").read_text() for s in status["services"]}
    return {"status": status, "cgroup_procs": procs}


def wait_new_control_plane(dc, old):
    deadline = time.monotonic() + RESTART_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        pid = dc.control_plane_pid()
        if pid and pid != old and dc.anden("status", "--json").returncode == 0:
            return pid
        time.sleep(0.1)
    raise lib.CommandError(f"no new andend answered in {RESTART_TIMEOUT_SECONDS} s")


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c02") as dc:
        digest = dc.import_binary("echosvc", RAW)
        for name in NAMES:
            dc.create_echo(name, digest, cpu_millis=lib.node_cpu_millis())
        before = snapshot(dc)
        (RAW / "before.json").write_text(json.dumps(before, indent=2, sort_keys=True) + "\n")
        nodes = {s["node"] for s in before["status"]["services"]}

        pid_before = dc.control_plane_pid()
        os.kill(pid_before, signal.SIGKILL)
        pid_after = wait_new_control_plane(dc, pid_before)
        # The time for a wrong reconciler pass or a lost lease to show.
        time.sleep(SETTLE_SECONDS)
        after = snapshot(dc)
        (RAW / "after.json").write_text(json.dumps(after, indent=2, sort_keys=True) + "\n")
        answers = {s["spec"]["name"]: dc.request(s) for s in after["status"]["services"]}

    lib.write_report("c02", {
        "signal": "SIGKILL", "pid_before": pid_before, "pid_after": pid_after,
        "nodes_with_services": len(nodes), "answers_after": answers,
    })
    checks = {
        "new_pid": pid_before != pid_after,
        "services_on_every_node": len(nodes) == lib.NODE_COUNT,
        "state_unchanged": before == after,
        "services_answer": all(code == 200 for code in answers.values()),
    }
    failed = [k for k, ok in checks.items() if not ok]
    if failed:
        print("C2 failed: " + ", ".join(failed), file=sys.stderr)
        return 1
    print("C2 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
