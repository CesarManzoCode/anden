#!/usr/bin/env python3
"""C12: every wake leaves its breakdown in seven phases, and they add up to what the client measured (D12).

Two echosvc, one per wake route. For each route, 100 wakes: SleepService and a confirmed ASLEEP, then one `wakeclient measure`
from the client netns through the edge. Each measure is crossed with the line of wake_events.jsonl of the same connection: one
written during that measure, by the node of the service, whose client_addr has the local port of the client and whose
t_accept_ns (CLOCK_MONOTONIC, the clock of time.monotonic_ns) falls between the start and the end of the measure. Passes with at
least 100 matched wakes on each route, as the event names it, and at least 99 % of all wakes with
|sum of phases - client| <= max(5 ms, 10 % of the client). A measure that failed or found no event counts against that 99 %.
"""
import json
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c12"
SAMPLES = 100
CPU_MILLIS = 1000
MEMORY_MIB = 128
ROUTES = ("relaunch", "restore")
PHASES = ("detection", "placement", "rootfs", "network", "start", "readiness", "first_byte")
EVENT_WAIT_SECONDS = 5
MIN_FRACTION = 0.99
ABS_TOLERANCE_MS = 5.0
REL_TOLERANCE = 0.10


def events_path(dc, svc):
    return dc.state / "nodes" / svc["node"] / "wake_events.jsonl"


def read_events(path):
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def port_of(addr):
    _, _, port = (addr or "").rpartition(":")
    return int(port) if port.isdigit() else None


def match_event(path, seen, line, start_ns, end_ns):
    """The event of the measure's connection among those written after the first `seen`, or None."""
    deadline = time.monotonic() + EVENT_WAIT_SECONDS
    while True:
        events = read_events(path)
        for ev in events[seen:]:
            if port_of(ev.get("client_addr")) == line.get("local_port") and start_ns <= ev.get("t_accept_ns", -1) <= end_ns:
                return ev
        if not line.get("ok") or time.monotonic() > deadline:
            return None
        time.sleep(0.05)


def within_tolerance(wake):
    if wake["phases_ms"] is None or not wake["ok"]:
        return False
    total = sum(wake["phases_ms"][p] for p in PHASES)
    return abs(total - wake["client_ms"]) <= max(ABS_TOLERANCE_MS, REL_TOLERANCE * wake["client_ms"])


def run_route(dc, digest, route):
    svc = dc.create_echo(route, digest, cpu_millis=CPU_MILLIS, memory_mib=MEMORY_MIB, extra=["--wake-route", route])
    path = events_path(dc, svc)
    wakes, lines = [], []
    for _ in range(SAMPLES):
        try:
            dc.sleep_confirmed(route)
        except lib.CommandError as e:
            lines.append({"ok": False, "error": str(e)})
            wakes.append({"route": None, "route_requested": route, "client_ms": None, "phases_ms": None, "ok": False})
            break
        seen = len(read_events(path))
        start_ns = time.monotonic_ns()
        line = dc.measure(svc)
        end_ns = time.monotonic_ns()
        lines.append(line)
        ev = match_event(path, seen, line, start_ns, end_ns)
        wakes.append({
            "route": ev["route"] if ev else None,
            "route_requested": route,
            "client_ms": line.get("client_ms"),
            "phases_ms": ev["phases_ms"] if ev else None,
            "local_port": line.get("local_port"),
            "client_addr": ev["client_addr"] if ev else None,
            "ok": bool(line.get("ok")),
        })
    if path.exists():
        shutil.copyfile(path, RAW / f"{route}.events.jsonl")
    (RAW / f"{route}.client.log").write_text("".join(json.dumps(l) + "\n" for l in lines))
    return wakes


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    wakes = []
    with lib.Datacenter("c12") as dc:
        digest = dc.import_binary("echosvc", RAW)
        for route in ROUTES:
            wakes += run_route(dc, digest, route)

    matched = {r: sum(1 for w in wakes if w["route"] == r and w["phases_ms"] is not None) for r in ROUTES}
    good = sum(within_tolerance(w) for w in wakes)
    fraction = good / len(wakes) if wakes else 0.0
    lib.write_report("c12", {
        "wakes": wakes, "samples_per_route": SAMPLES, "matched_per_route": matched, "within_tolerance": good,
        "within_tolerance_fraction": fraction, "tolerance": {"abs_ms": ABS_TOLERANCE_MS, "rel": REL_TOLERANCE},
    })
    failed = [r for r in ROUTES if matched[r] < SAMPLES]
    if failed or fraction < MIN_FRACTION:
        print(f"C12 failed: matched {matched}, within tolerance {good}/{len(wakes)}", file=sys.stderr)
        return 1
    print(f"C12 ok: matched {matched}, within tolerance {good}/{len(wakes)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
