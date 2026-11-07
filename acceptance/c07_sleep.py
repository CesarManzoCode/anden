#!/usr/bin/env python3
"""C7: a service with no traffic goes to sleep by itself, and nothing of it stays running.

An echosvc with idle_seconds = 2 receives no request and no SleepService. Once the API says ASLEEP, cgroup.procs and
memory.current are read raw from the host. Passes when it had a process before, has none now and holds at most 8 MiB
(D12). A request through the edge then has to wake it by the relaunch route.
"""
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c07"
IDLE_SECONDS = 2
SLEEP_TIMEOUT_SECONDS = 30
MEMORY_LIMIT_BYTES = 8 << 20
WAKE_ROUTE = "relaunch"


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c07") as dc:
        digest = dc.import_binary("echosvc", RAW)
        svc = dc.create_echo("sleeper", digest, idle_seconds=IDLE_SECONDS)
        cgroup = dc.cgroup(svc)
        procs_before = len((cgroup / "cgroup.procs").read_text().split())

        start = time.monotonic()
        asleep = dc.wait_state("sleeper", lib.ASLEEP, SLEEP_TIMEOUT_SECONDS)
        waited = time.monotonic() - start
        procs_raw = (cgroup / "cgroup.procs").read_text()
        memory = int((cgroup / "memory.current").read_text())
        (RAW / "cgroup.procs").write_text(procs_raw)
        (RAW / "memory.current").write_text(f"{memory}\n")

        wake_status = dc.request(asleep)
        woken = dc.wait_state("sleeper", lib.ACTIVE, SLEEP_TIMEOUT_SECONDS)
        wake_route = woken["instance"]["route"]

    lib.write_report("c07", {
        "idle_seconds": IDLE_SECONDS, "waited_seconds": waited, "procs_before": procs_before,
        "cgroup_procs_raw": procs_raw, "memory_current_bytes": memory, "state": asleep["state"],
        "wake_status": wake_status, "wake_route": wake_route,
    })
    checks = {
        "had_processes": procs_before >= 1,
        "no_processes": procs_raw.strip() == "",
        "memory_returned": memory <= MEMORY_LIMIT_BYTES,
        "asleep": asleep["state"] == lib.ASLEEP,
        "woke_answering": wake_status == 200,
        "woke_by_relaunch": wake_route == WAKE_ROUTE,
    }
    failed = [k for k, ok in checks.items() if not ok]
    if failed:
        print("C7 failed: " + ", ".join(failed), file=sys.stderr)
        return 1
    print("C7 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
