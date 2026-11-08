#!/usr/bin/env python3
"""C26: what a sleeping service costs, and how waking one behaves as their number grows (D12).

Minimal echosvc services are created and put to sleep up to each step of D12: 10, 50, 100, 250, 500 and 1000. With all
of them ASLEEP, memory.current of the datacenter's scope is read raw; the cost per sleeping service is what it grew
over the scope with no service, divided by their number. Then 50 services chosen at random are woken, one at a time,
with wakeclient from the client netns through the edge, and each one is put back to sleep. Stops after the step whose
memory reaches 6 GiB, or after the last one. Writes c26/curve.csv. Fails on any client error or if the curve is not
generated.
"""
import concurrent.futures
import csv
import json
import random
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c26"
STEPS = (10, 50, 100, 250, 500, 1000)
LIMIT_BYTES = 6 << 30
WAKES_PER_STEP = 50
SEED = 26
# Small enough that a few of them awake at once fit in any node: what is measured is the sleeping ones.
CPU_MILLIS = 50
MEMORY_MIB = 32
# Services created and put to sleep at once.
WORKERS = 8
CURVE_FIELDS = ("services", "memory_current_bytes", "memory_per_sleeping_kib", "wake_p50_ms", "wake_p99_ms", "errors")


def scope_memory(dc):
    return int((Path(dc.info["scope_cgroup"]) / "memory.current").read_text())


def add_sleeping(dc, digest, name):
    """Creates one minimal echosvc and leaves it ASLEEP. Returns its service as the API gives it."""
    svc = dc.create_echo(name, digest, cpu_millis=CPU_MILLIS, memory_mib=MEMORY_MIB)
    dc.sleep_confirmed(name)
    return svc


def wake_step(dc, services, rng, log):
    """WAKES_PER_STEP wakes of services chosen at random, each one put back to sleep after it answers."""
    samples, errors = [], 0
    for svc in rng.choices(services, k=WAKES_PER_STEP):
        name = svc["spec"]["name"]
        line = dc.measure(svc)
        if line.get("ok"):
            try:
                dc.sleep_confirmed(name)
            except lib.CommandError as e:
                line = {**line, "ok": False, "error": f"back to sleep: {e}"}
        log.write(json.dumps({"services": len(services), "service": name, **line}) + "\n")
        if line.get("ok"):
            samples.append(line["client_ms"])
        else:
            errors += 1
    return samples, errors


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    rng = random.Random(SEED)
    steps, rows, services = [], [], []
    stopped_by, failure = "last_step", None
    with lib.Datacenter("c26") as dc, open(RAW / "wakes.log", "w") as log:
        digest = dc.import_binary("echosvc", RAW)
        baseline = scope_memory(dc)
        with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS) as pool:
            for target in STEPS:
                names = [f"d{i:04d}" for i in range(len(services), target)]
                try:
                    services.extend(pool.map(lambda name: add_sleeping(dc, digest, name), names))
                except lib.CommandError as e:
                    stopped_by, failure = "error", f"building the step of {target}: {e}"
                    break
                memory = scope_memory(dc)
                samples, errors = wake_step(dc, services, rng, log)
                stats = lib.percentiles(samples)
                per_sleeping_kib = (memory - baseline) / len(services) / 1024
                steps.append({"services": len(services), "wake_p99_ms": stats["p99"],
                              "memory_per_sleeping_kib": per_sleeping_kib, "errors": errors})
                rows.append({"services": len(services), "memory_current_bytes": memory,
                             "memory_per_sleeping_kib": f"{per_sleeping_kib:.1f}", "wake_p50_ms": stats["p50"],
                             "wake_p99_ms": stats["p99"], "errors": errors})
                print(f"C26 step {len(services)}: memory.current {memory} B, {per_sleeping_kib:.1f} KiB per sleeping "
                      f"service, {stats}, errors {errors}", flush=True)
                if memory >= LIMIT_BYTES:
                    stopped_by = "limit"
                    break

    if rows:
        with open(RAW / "curve.csv", "w", newline="") as f:
            writer = csv.DictWriter(f, fieldnames=CURVE_FIELDS)
            writer.writeheader()
            writer.writerows(rows)
    lib.write_report("c26", {
        "limit_bytes": LIMIT_BYTES, "steps": steps, "baseline_memory_bytes": baseline, "stopped_by": stopped_by,
        "failure": failure, "wakes_per_step": WAKES_PER_STEP, "seed": SEED,
        "service": {"cpu_millis": CPU_MILLIS, "memory_mib": MEMORY_MIB},
    })
    checks = {
        "curve_generated": (RAW / "curve.csv").is_file() and failure is None,
        "no_client_errors": all(step["errors"] == 0 for step in steps),
    }
    failed = [k for k, ok in checks.items() if not ok]
    if failed:
        print("C26 failed: " + ", ".join(failed) + (f": {failure}" if failure else ""), file=sys.stderr)
        return 1
    print(f"C26 ok: {len(steps)} steps, stopped by {stopped_by}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
