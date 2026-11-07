#!/usr/bin/env python3
"""C8, warm cache: a wake answers in under 1 s at p99 with the layers on the node and nothing dropped (D12, D18).

Before each measure, SleepService and a confirmed ASLEEP. wakeclient then measures from the client netns through the edge,
from before connect() to the last byte. The first 30 wakes are discarded. Passes with 300 samples, no errors and p99 under
1000 ms. This only says "under 1 s here": section 7 of the contract does not allow comparing it with Railway's figure.
"""
import json
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c08"
WARMUP = 30
SAMPLES = 300
P99_LIMIT_MS = 1000
CPU_MILLIS = 1000


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c08") as dc:
        digest = dc.import_binary("echosvc", RAW)
        svc = dc.create_echo("warm", digest, cpu_millis=CPU_MILLIS)
        lines = dc.wake_series(svc, WARMUP + SAMPLES)
    (RAW / "client.log").write_text("".join(json.dumps(line) + "\n" for line in lines))
    samples = [line["client_ms"] for line in lines[WARMUP:] if line["ok"]]
    errors = sum(not line["ok"] for line in lines)
    stats = lib.percentiles(samples)

    lib.write_report("c08", {
        "cache": "warm", "samples_ms": samples, "warmup_discarded": WARMUP, "errors": errors, **stats,
        "governor": lib.governor(), "disk": lib.disk_fstype(),
    })
    if len(samples) < SAMPLES or errors or stats["p99"] >= P99_LIMIT_MS:
        print(f"C8 warm failed: n={len(samples)} errors={errors} {stats}", file=sys.stderr)
        return 1
    print(f"C8 warm ok: {stats}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
