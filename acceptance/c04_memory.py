#!/usr/bin/env python3
"""C4: a service over its memory limit dies inside its cgroup and the others go on without errors.

Three services; while two get external load through the edge, the third is asked to allocate far over memory.max.
memory.events is read raw from the host. Passes when oom_kill grew, the service ended FAILED and the others had
requests and no errors.
"""
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c04"
LIMIT_MIB = 64
HOG_MIB = 1024
LOAD_SECONDS = 15
DIE_TIMEOUT_SECONDS = 10


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c04") as dc:
        digest = dc.import_binary("echosvc", RAW)
        hog = dc.create_echo("hog", digest, memory_mib=LIMIT_MIB)
        others = [dc.create_echo(name, digest) for name in ("other-a", "other-b")]
        cgroup = dc.cgroup(hog)
        memory_max = (cgroup / "memory.max").read_text().strip()

        loads = [dc.load(s, LOAD_SECONDS) for s in others]
        dc.request(hog, f"/hog?mib={HOG_MIB}")
        final = dc.wait_state("hog", lib.FAILED, DIE_TIMEOUT_SECONDS)
        events = (cgroup / "memory.events").read_text()
        other_services = [load.result() for load in loads]
        (RAW / "memory.events").write_text(events)

    lib.write_report("c04", {
        "memory_events_raw": events, "memory_max_raw": memory_max, "hog_final_state": final["state"],
        "other_services": other_services,
    })
    oom = [int(line.split()[1]) for line in events.splitlines() if line.startswith("oom_kill ")]
    checks = {
        "limit_applied": memory_max == str(LIMIT_MIB << 20),
        "oom_killed": bool(oom) and oom[0] > 0,
        "others_ok": all(o["requests"] > 0 and o["errors"] == 0 for o in other_services),
    }
    failed = [k for k, ok in checks.items() if not ok]
    if failed:
        print("C4 failed: " + ", ".join(failed), file=sys.stderr)
        return 1
    print("C4 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
