#!/usr/bin/env python3
"""C14: a request that races a sleep is always answered, on both wake routes (D12: M = 100 per route).

One echosvc per route. Each repetition starts from a service ACTIVE on its node, launches SleepService and, with a delay that
sweeps from 0 to 20 ms, one `wakeclient measure` with a unique id from the client netns through the edge. SleepService only
changes the desired state, and the node begins the sleep up to a heartbeat later, so the delay counts from the moment the node
begins it: a trigger in the client netns watches the counters of the node in `nodes/<node>/transitions.json`, which the agent
rewrites at each change of state, and fires the request once `sleeps` of the service grows. Starting wakeclient adds its own
few milliseconds to every delay.

A SleepService that fails, or a sleep that the request aborts because the service woke again, is not a client error. An error
is a request that wakeclient reports as failed, and a timeout one that ran out of time. hits_during_transition is how much the
counter `held` of the service grew: requests the proxy held while the service was going to sleep, during the dump of CRIU too.
Passes with M repetitions per route, no error, no timeout and at least one hit per route, so that the race was really run.
"""
import json
import shutil
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c14"
REPETITIONS = 100
MAX_DELAY_MS = 20.0
CPU_MILLIS = 1000
MEMORY_MIB = 128
ROUTES = ("relaunch", "restore")
REQUEST_TIMEOUT = "10s"
TRIGGER_WAIT_SECONDS = 10
REPETITION_TIMEOUT_SECONDS = 60
SETTLE_TIMEOUT_SECONDS = 30
# Runs in the client netns: says it is watching, waits until the node begins a sleep of the service, waits the delay and runs
# wakeclient. Its last line is the one of wakeclient.
TRIGGER_SCRIPT = """
import json, subprocess, sys, time
path, name, before, delay_ms, wait_s, wakeclient, addr, rid, timeout = sys.argv[1:10]
before, delay, end = int(before), float(delay_ms) / 1000, time.monotonic() + float(wait_s)
triggered = False
print("watching", flush=True)
while time.monotonic() < end:
    try:
        entry = json.load(open(path)).get(name) or {}
    except (OSError, ValueError):
        entry = {}
    if entry.get("sleeps", 0) > before:
        triggered = True
        break
    time.sleep(0.0002)
fired = time.monotonic()
time.sleep(max(0.0, delay - (time.monotonic() - fired)))
spawn = time.monotonic()
p = subprocess.run([wakeclient, "measure", "--addr", addr, "--id", rid, "--timeout", timeout], capture_output=True, text=True)
try:
    line = json.loads(p.stdout)
except ValueError:
    line = {"ok": False, "error": "rc=%d: %s" % (p.returncode, p.stderr.strip())}
line["triggered"] = triggered
line["waited_ms"] = (spawn - fired) * 1000
print(json.dumps(line))
"""


def read_counters(path, name):
    try:
        return json.loads(path.read_text()).get(name) or {}
    except (OSError, ValueError):
        return {}


def ready(dc, name, path):
    """Back to ACTIVE in the API and on the node before the next repetition."""
    dc.svc("wake", "--name", name)
    dc.wait_state(name, lib.ACTIVE, lib.ACTIVE_TIMEOUT_SECONDS)
    deadline = time.monotonic() + SETTLE_TIMEOUT_SECONDS
    while read_counters(path, name).get("state", "ACTIVE") != "ACTIVE":
        if time.monotonic() > deadline:
            raise lib.CommandError(f"{name} is {read_counters(path, name)} on its node after {SETTLE_TIMEOUT_SECONDS} s")
        time.sleep(0.01)


def classify(line):
    if line.get("ok"):
        return "ok"
    return "timeout" if "timeout" in (line.get("error") or "").lower() else "error"


def run_route(dc, digest, route):
    svc = dc.create_echo(route, digest, cpu_millis=CPU_MILLIS, memory_mib=MEMORY_MIB, extra=["--wake-route", route])
    path = dc.state / "nodes" / svc["node"] / "transitions.json"
    held_start = read_counters(path, route).get("held", 0)
    lines = []
    for i in range(REPETITIONS):
        delay = MAX_DELAY_MS * i / (REPETITIONS - 1)
        try:
            ready(dc, route, path)
        except lib.CommandError as e:
            lines.append({"repetition": i, "delay_ms": delay, "ok": False, "error": str(e), "outcome": "error"})
            break
        before = read_counters(path, route)
        trigger = subprocess.Popen(
            dc.exec_cmd("client", "python3", "-I", "-c", TRIGGER_SCRIPT, str(path), route, str(before.get("sleeps", 0)),
                        str(delay), str(TRIGGER_WAIT_SECONDS), str(lib.WAKECLIENT), dc.edge_addr(svc),
                        f"c14-{route}-{i}", REQUEST_TIMEOUT),
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        # SleepService only once the trigger watches, or it could miss the start of the sleep.
        trigger.stdout.readline()
        sleep = dc.anden("svc", "sleep", "--name", route, "--json")
        try:
            out, err = trigger.communicate(timeout=REPETITION_TIMEOUT_SECONDS)
            line = json.loads(out.strip().splitlines()[-1])
        except subprocess.TimeoutExpired:
            trigger.kill()
            trigger.communicate()
            line = {"ok": False, "error": "timeout: the trigger did not finish"}
        except (ValueError, IndexError):
            line = {"ok": False, "error": f"trigger rc={trigger.returncode}: {err.strip()}"}
        after = read_counters(path, route)
        line.update(repetition=i, delay_ms=delay, sleep_rc=sleep.returncode,
                    held_delta=after.get("held", 0) - before.get("held", 0), outcome=classify(line))
        lines.append(line)
    held_end = read_counters(path, route).get("held", 0)
    (RAW / f"{route}.log").write_text("".join(json.dumps(l) + "\n" for l in lines))
    return {
        "m": len(lines),
        "errors": sum(l["outcome"] == "error" for l in lines),
        "timeouts": sum(l["outcome"] == "timeout" for l in lines),
        "hits_during_transition": held_end - held_start,
        "sleep_rejected": sum(l.get("sleep_rc", 0) != 0 for l in lines),
        "not_triggered": sum(not l.get("triggered", True) for l in lines),
    }


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    routes = {}
    with lib.Datacenter("c14") as dc:
        digest = dc.import_binary("echosvc", RAW)
        for route in ROUTES:
            routes[route] = run_route(dc, digest, route)
            path = dc.state / "nodes"
            for node in path.iterdir():
                if (node / "transitions.json").exists():
                    shutil.copyfile(node / "transitions.json", RAW / f"{node.name}.transitions.json")

    lib.write_report("c14", {"routes": routes, "repetitions_per_route": REPETITIONS, "max_delay_ms": MAX_DELAY_MS})
    failed = {r: v for r, v in routes.items()
              if v["m"] < REPETITIONS or v["errors"] or v["timeouts"] or v["hits_during_transition"] < 1}
    print(json.dumps(routes), flush=True)
    if failed or len(routes) < len(ROUTES):
        print(f"C14 failed: {failed}", file=sys.stderr)
        return 1
    print("C14 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
