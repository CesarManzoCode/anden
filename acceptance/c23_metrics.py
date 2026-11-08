#!/usr/bin/env python3
"""C23: andend serves at 10.80.0.1:9090/metrics the sum of what the agents report, valid for promtool.

Two echosvc, one per wake route. Each is put to sleep (confirmed ASLEEP) and woken by one `wakeclient measure` from the client
netns through the edge. Then /metrics is fetched from the root netns until the wake counts of both routes show up, saved to
c23/metrics.txt and passed to `promtool check metrics` on stdin. Passes with promtool exit 0, anden_node_up for the 3 nodes,
the 7 phases of anden_wake_phase_seconds for both routes, and at least one wake of each route in anden_wake_duration_seconds.
"""
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c23"
URL = "http://10.80.0.1:9090/metrics"
ROUTES = ("relaunch", "restore")
PHASES = ("detection", "placement", "rootfs", "network", "start", "readiness", "first_byte")
NODES = ("node1", "node2", "node3")
CPU_MILLIS = 1000
MEMORY_MIB = 128
FETCH_TIMEOUT_SECONDS = 15
FETCH_SCRIPT = "import sys,urllib.request\nsys.stdout.write(urllib.request.urlopen(sys.argv[1],timeout=10).read().decode())\n"
SAMPLE_RE = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})?\s+(\S+)')
LABEL_RE = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"')


def samples(text):
    """(name, labels, value) of each sample line of the text format."""
    out = []
    for line in text.splitlines():
        match = SAMPLE_RE.match(line)
        if match and not line.startswith("#"):
            out.append((match.group(1), dict(LABEL_RE.findall(match.group(2) or "")), float(match.group(3))))
    return out


def value(parsed, name, **labels):
    for n, l, v in parsed:
        if n == name and l == labels:
            return v
    return None


def fetch(dc):
    p = dc.exec("root", "python3", "-I", "-c", FETCH_SCRIPT, URL)
    if p.returncode != 0:
        raise lib.CommandError(f"GET {URL}: rc={p.returncode}: {p.stderr}")
    return p.stdout


def wake_counts(parsed):
    return {r: value(parsed, "anden_wake_duration_seconds_count", route=r) or 0 for r in ROUTES}


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    measures = {}
    with lib.Datacenter("c23") as dc:
        digest = dc.import_binary("echosvc", RAW)
        for route in ROUTES:
            svc = dc.create_echo(route, digest, cpu_millis=CPU_MILLIS, memory_mib=MEMORY_MIB, extra=["--wake-route", route])
            dc.sleep_confirmed(route)
            measures[route] = dc.measure(svc)
        deadline = time.monotonic() + FETCH_TIMEOUT_SECONDS
        while True:
            text = fetch(dc)
            if all(wake_counts(samples(text)).values()) or time.monotonic() > deadline:
                break
            time.sleep(0.2)
    (RAW / "metrics.txt").write_text(text)
    promtool = subprocess.run(["promtool", "check", "metrics"], input=text, capture_output=True, text=True)
    (RAW / "promtool.log").write_text(promtool.stdout + promtool.stderr)

    parsed = samples(text)
    counts = wake_counts(parsed)
    node_up = {n: value(parsed, "anden_node_up", node=n) for n in NODES}
    missing_phases = [f"{p}/{r}" for r in ROUTES for p in PHASES
                      if value(parsed, "anden_wake_phase_seconds_count", phase=p, route=r) is None]
    lib.write_report("c23", {
        "promtool_rc": promtool.returncode, "url": URL, "measures": measures, "wake_counts": counts, "node_up": node_up,
        "missing_phases": missing_phases,
    })
    problems = []
    if promtool.returncode != 0:
        problems.append(f"promtool check metrics exit {promtool.returncode}: {promtool.stdout}{promtool.stderr}")
    if any(v is None for v in node_up.values()):
        problems.append(f"anden_node_up: {node_up}")
    if missing_phases:
        problems.append(f"no anden_wake_phase_seconds for {missing_phases}")
    if not all(counts.values()):
        problems.append(f"wakes per route in anden_wake_duration_seconds: {counts}")
    if problems:
        print("C23 failed: " + "; ".join(problems), file=sys.stderr)
        return 1
    print(f"C23 ok: promtool exit 0, wakes {counts}, node_up {node_up}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
