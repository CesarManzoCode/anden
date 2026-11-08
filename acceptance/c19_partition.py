#!/usr/bin/env python3
"""C19: a partition never leaves two active instances of a singleton (D08).

A `writersvc` singleton that never sleeps appends a beat every 50 ms to writers.log in its volume. Its node is cut off
with `anden chaos partition`; the script waits until the service is ACTIVE on another node and the new writer is
beating there, heals the partition and lets both nodes run a few seconds more. A volume is a directory of each node, so
writers.log is the beats of every node merged in the order of t_ns, which is CLOCK_MONOTONIC in all of them. Passes
when no two writers overlap: each one's last beat comes before the next one's first, one started after the partition
and one still beats after the heal.
"""
import json
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c19"
NAME = "writer"
VOLUME = "data"
PORT = 8080
BEFORE_SECONDS = 2
MOVE_TIMEOUT_SECONDS = 30
AFTER_MOVE_SECONDS = 2
AFTER_HEAL_SECONDS = 6


def chaos(dc, action, node):
    p = dc.anden("chaos", action, node, "--json")
    if p.returncode != 0:
        raise lib.CommandError(f"anden chaos {action} {node}: rc={p.returncode}: {p.stderr}")


def volume_log(dc, node):
    return dc.state / "nodes" / node / "volumes" / VOLUME / "writers.log"


def read_beats(path):
    """The beats of one file. A line cut by a killed writer is left out."""
    beats = []
    try:
        lines = path.read_text().splitlines()
    except FileNotFoundError:
        return beats
    for line in lines:
        try:
            beat = json.loads(line)
        except ValueError:
            continue
        if isinstance(beat, dict) and "writer" in beat and "t_ns" in beat:
            beats.append(beat)
    return beats


def wait_moved(dc, old_node):
    """Waits until the singleton is ACTIVE on another node and its writer beats there. Returns that node."""
    deadline = time.monotonic() + MOVE_TIMEOUT_SECONDS
    while True:
        svc = dc.svc("get", "--name", NAME)
        node = svc["node"]
        if svc["state"] == lib.ACTIVE and node and node != old_node and read_beats(volume_log(dc, node)):
            return node
        if time.monotonic() > deadline:
            raise lib.CommandError(f"{NAME} after the partition: {svc['state']} on {node}")
        time.sleep(0.1)


def spans(beats):
    """First and last t_ns of each writer, ordered by the first."""
    out = {}
    for b in beats:
        lo, hi = out.get(b["writer"], (b["t_ns"], b["t_ns"]))
        out[b["writer"]] = (min(lo, b["t_ns"]), max(hi, b["t_ns"]))
    return sorted(out.items(), key=lambda kv: kv[1][0])


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c19") as dc:
        digest = dc.import_binary("writersvc", RAW)
        dc.svc("create", "--name", NAME, "--image", digest, "--port", str(PORT), "--cpu-millis", "200",
               "--memory-mib", "64", "--idle-seconds", "0", "--singleton", "--volume", f"{VOLUME}:/data",
               "--env", f"PORT={PORT}", "--env", "LOG_PATH=/data/writers.log")
        partitioned = dc.wait_state(NAME, lib.ACTIVE, lib.ACTIVE_TIMEOUT_SECONDS)["node"]
        time.sleep(BEFORE_SECONDS)
        partition_start_ns = time.monotonic_ns()
        chaos(dc, "partition", partitioned)
        try:
            node_after = wait_moved(dc, partitioned)
            time.sleep(AFTER_MOVE_SECONDS)
        finally:
            chaos(dc, "heal", partitioned)
            heal_ns = time.monotonic_ns()
        time.sleep(AFTER_HEAL_SECONDS)
        (RAW / "status_after.json").write_text(json.dumps(dc.status(), indent=2) + "\n")
        beats = []
        for n in range(1, lib.NODE_COUNT + 1):
            beats += read_beats(volume_log(dc, f"node{n}"))
    beats.sort(key=lambda b: b["t_ns"])
    (RAW / "writers.log").write_text("".join(json.dumps({"writer": b["writer"], "t_ns": b["t_ns"]}) + "\n" for b in beats))

    ordered = spans(beats)
    overlaps = [f"{wa} and {wb}" for (wa, (_, hi)), (wb, (lo, _)) in zip(ordered, ordered[1:]) if hi >= lo]
    gaps = [round((b[1][0] - a[1][1]) / 1e9, 3) for a, b in zip(ordered, ordered[1:])]
    lib.write_report("c19", {
        "partition_start_ns": partition_start_ns, "heal_ns": heal_ns, "node_partitioned": partitioned,
        "node_after": node_after, "writers": [{"writer": w, "first_ns": lo, "last_ns": hi} for w, (lo, hi) in ordered],
        "gaps_seconds": gaps, "beats": len(beats),
    })
    failed = []
    if len(ordered) < 2:
        failed.append("the singleton never moved")
    if overlaps:
        failed.append("writers overlap: " + ", ".join(overlaps))
    if not any(lo > partition_start_ns for _, (lo, _) in ordered):
        failed.append("no writer started after the partition")
    if not any(hi > heal_ns for _, (_, hi) in ordered):
        failed.append("no writer beats after the heal")
    if failed:
        print("C19 failed: " + "; ".join(failed), file=sys.stderr)
        return 1
    print(f"C19 ok: {len(ordered)} writers, gaps {gaps} s")
    return 0


if __name__ == "__main__":
    sys.exit(main())
