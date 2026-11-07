#!/usr/bin/env python3
"""C8, cold cache: the same wake as c08_warm.py after sync and dropping the page cache (D12, D18). It has no threshold.

Dropping the cache needs real root, so this exits with 77 if `sudo -n` fails (D17). Passes with 50 samples and no errors.
"""
import json
import shutil
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c08cold"
SAMPLES = 50
CPU_MILLIS = 1000
DROP_CACHES = "/proc/sys/vm/drop_caches"


def drop_caches():
    subprocess.run(["sync"], check=True, stdin=subprocess.DEVNULL)
    p = subprocess.run(["sudo", "-n", "tee", DROP_CACHES], input="3\n", capture_output=True, text=True)
    if p.returncode != 0:
        raise lib.CommandError(f"sudo -n tee {DROP_CACHES}: rc={p.returncode}: {p.stderr}")


def main():
    lib.require_ns()
    lib.require_sudo()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c08cold") as dc:
        digest = dc.import_binary("echosvc", RAW)
        svc = dc.create_echo("cold", digest, cpu_millis=CPU_MILLIS)
        lines = dc.wake_series(svc, SAMPLES, before=drop_caches)
    (RAW / "client.log").write_text("".join(json.dumps(line) + "\n" for line in lines))
    samples = [line["client_ms"] for line in lines if line["ok"]]
    errors = sum(not line["ok"] for line in lines)
    stats = lib.percentiles(samples)

    lib.write_report("c08cold", {
        "cache": "cold", "samples_ms": samples, "errors": errors, **stats, "governor": lib.governor(), "disk": lib.disk_fstype(),
    })
    if len(samples) < SAMPLES or errors:
        print(f"C8 cold failed: n={len(samples)} errors={errors} {stats}", file=sys.stderr)
        return 1
    print(f"C8 cold ok: {stats}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
