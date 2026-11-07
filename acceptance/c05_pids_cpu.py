#!/usr/bin/env python3
"""C5: pids.max contains a fork bomb and cpu.stat respects the quota (thresholds of D12).

While two services get external load, one with pids.max starts a fork bomb and another with half a core burns several
goroutines on the CPU. pids.* and cpu.stat are read raw from the host. Passes when the bomb hit pids.max, the count stays
within it, the host can still fork, the others had no errors, and the CPU used in 5 s is between 0.50 and 1.10 x the quota.
"""
import shutil
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c05"
PIDS_MAX = 64
BOMB_TIMEOUT_SECONDS = 20
CPU_MILLIS = 500
QUOTA_FRACTION = CPU_MILLIS / 1000
CPU_WINDOW_SECONDS = 5
BURN_WORKERS = 4
LOAD_SECONDS = 25


def events_max(raw):
    for line in raw.splitlines():
        if line.startswith("max "):
            return int(line.split()[1])
    return 0


def usage_usec(cgroup):
    for line in (cgroup / "cpu.stat").read_text().splitlines():
        if line.startswith("usage_usec "):
            return int(line.split()[1])
    raise lib.CommandError(f"no usage_usec in {cgroup}/cpu.stat")


def wait_bomb(cgroup):
    deadline = time.monotonic() + BOMB_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        if events_max((cgroup / "pids.events").read_text()) > 0:
            break
        time.sleep(0.05)
    # Lets the bomb push against the limit for a moment before reading it.
    time.sleep(1)


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c05") as dc:
        digest = dc.import_binary("echosvc", RAW)
        bomb = dc.create_echo("bomb", digest, pids_max=PIDS_MAX)
        burner = dc.create_echo("burner", digest, cpu_millis=CPU_MILLIS)
        others = [dc.create_echo(name, digest) for name in ("other-a", "other-b")]
        loads = [dc.load(s, LOAD_SECONDS) for s in others]

        pids_cgroup = dc.cgroup(bomb)
        fork_status = dc.request(bomb, "/fork")
        wait_bomb(pids_cgroup)
        pids_max = int((pids_cgroup / "pids.max").read_text())
        pids_events = (pids_cgroup / "pids.events").read_text()
        pids_current = int((pids_cgroup / "pids.current").read_text())
        host_fork_ok = subprocess.run(["true"]).returncode == 0

        cpu_cgroup = dc.cgroup(burner)
        cpu_max = (cpu_cgroup / "cpu.max").read_text().strip()
        burn_status = dc.request(burner, f"/burn?seconds={CPU_WINDOW_SECONDS + 5}&workers={BURN_WORKERS}")
        # The window starts with the workers already running against the quota.
        time.sleep(1)
        start_usec, start = usage_usec(cpu_cgroup), time.monotonic()
        time.sleep(CPU_WINDOW_SECONDS)
        end_usec, wall = usage_usec(cpu_cgroup), time.monotonic() - start
        other_services = [load.result() for load in loads]

    lib.write_report("c05", {
        "pids": {"pids_max": pids_max, "pids_events_raw": pids_events, "pids_current": pids_current,
                 "host_fork_ok": host_fork_ok, "others": other_services, "fork_status": fork_status},
        "cpu": {"quota_fraction": QUOTA_FRACTION, "usage_usec_start": start_usec, "usage_usec_end": end_usec,
                "wall_seconds": wall, "cpu_max_raw": cpu_max, "burn_status": burn_status},
        "cpu_model": lib.cpu_model(),
    })
    used = (end_usec - start_usec) / (wall * 1e6)
    checks = {
        "pids_limit_applied": pids_max == PIDS_MAX,
        "bomb_hit_limit": events_max(pids_events) > 0,
        "pids_within_limit": pids_current <= pids_max,
        "host_forks": host_fork_ok,
        "others_ok": all(o["requests"] > 0 and o["errors"] == 0 for o in other_services),
        "cpu_within_quota": QUOTA_FRACTION * 0.50 <= used <= QUOTA_FRACTION * 1.10,
    }
    failed = [k for k, ok in checks.items() if not ok]
    if failed:
        print(f"C5 failed: {', '.join(failed)}; cpu used {used:.3f}, quota {QUOTA_FRACTION}", file=sys.stderr)
        return 1
    print(f"C5 ok; cpu used {used:.3f}, quota {QUOTA_FRACTION}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
