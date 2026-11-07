#!/usr/bin/env python3
"""C18: the services of a node that crashes answer again from another node within 10 s (D08).

Two echosvc that never sleep go to one node: best fit puts the second beside the first, and a move puts it there
otherwise. A prober in the client netns starts one HTTP request per service every 100 ms through the edge, each on its
own thread with a timeout of 0.5 s, so that a hanging request does not delay the next one. After a few seconds of
answers the node is killed with `anden chaos kill`. The time without service of each one runs from its last answer
before its first failure after the crash to its first answer after its last failure. Passes when both left the killed
node and each one went at most 10 s without service.
"""
import json
import shutil
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c18"
NAMES = ["crash-a", "crash-b"]
LIMIT_SECONDS = 10
BEFORE_SECONDS = 3
AFTER_SECONDS = 25
PROBE_INTERVAL_SECONDS = 0.1
PROBE_TIMEOUT_SECONDS = 0.5
# Runs in the client netns. Prints one JSON list of {"port", "t_ns", "ok"}, t_ns being when the request started.
PROBE_SCRIPT = """
import http.client, json, sys, threading, time
host, ports, seconds = sys.argv[1], sys.argv[2].split(","), float(sys.argv[3])
interval, timeout = float(sys.argv[4]), float(sys.argv[5])
results, lock, threads = [], threading.Lock(), []
def probe(port, t_ns):
    ok = False
    try:
        c = http.client.HTTPConnection(host, int(port), timeout=timeout)
        c.request("GET", "/")
        r = c.getresponse()
        r.read()
        ok = r.status == 200
        c.close()
    except (OSError, http.client.HTTPException):
        pass
    with lock:
        results.append({"port": int(port), "t_ns": t_ns, "ok": ok})
start = time.monotonic_ns()
n = 0
while time.monotonic_ns() - start < seconds * 1e9:
    t_ns = time.monotonic_ns()
    for port in ports:
        th = threading.Thread(target=probe, args=(port, t_ns))
        th.start()
        threads.append(th)
    n += 1
    time.sleep(max(0.0, (start + n * interval * 1e9 - time.monotonic_ns()) / 1e9))
for th in threads:
    th.join()
print(json.dumps(results))
"""


def chaos(dc, action, node):
    p = dc.anden("chaos", action, node, "--json")
    if p.returncode != 0:
        raise lib.CommandError(f"anden chaos {action} {node}: rc={p.returncode}: {p.stderr}")


def gather(dc, digest):
    """Creates the services and leaves all of them on the node of the first one. Returns that node."""
    svcs = [dc.create_echo(name, digest) for name in NAMES]
    node = svcs[0]["node"]
    for svc in svcs[1:]:
        if svc["node"] == node:
            continue
        name = svc["spec"]["name"]
        dc.svc("move", "--name", name, "--node", node)
        dc.wait_state(name, lib.ASLEEP, lib.SLEEP_TIMEOUT_SECONDS)
        dc.svc("wake", "--name", name)
        if dc.wait_state(name, lib.ACTIVE, lib.ACTIVE_TIMEOUT_SECONDS)["node"] != node:
            raise lib.CommandError(f"{name} did not wake on {node}")
    return node


def downtime_seconds(samples, kill_ns, end_ns):
    """Time without service after kill_ns: from the last answer before the first failure to the first answer after the
    last failure. Without an answer after the last failure it runs to the end of the probing."""
    failures = [s["t_ns"] for s in samples if not s["ok"] and s["t_ns"] >= kill_ns]
    if not failures:
        return 0.0
    first_fail, last_fail = min(failures), max(failures)
    before = [s["t_ns"] for s in samples if s["ok"] and s["t_ns"] < first_fail]
    after = [s["t_ns"] for s in samples if s["ok"] and s["t_ns"] > last_fail]
    down_from = max(before) if before else kill_ns
    down_to = min(after) if after else end_ns
    return (down_to - down_from) / 1e9


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c18") as dc:
        digest = dc.import_binary("echosvc", RAW)
        killed = gather(dc, digest)
        svcs = {name: dc.svc("get", "--name", name) for name in NAMES}
        ports = {svc["edge_port"]: name for name, svc in svcs.items()}
        prober = subprocess.Popen(
            dc.exec_cmd("client", "python3", "-I", "-c", PROBE_SCRIPT, dc.info["edge_ip"], ",".join(map(str, ports)),
                        str(BEFORE_SECONDS + AFTER_SECONDS), str(PROBE_INTERVAL_SECONDS), str(PROBE_TIMEOUT_SECONDS)),
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        time.sleep(BEFORE_SECONDS)
        kill_ns = time.monotonic_ns()
        chaos(dc, "kill", killed)
        out, err = prober.communicate(timeout=BEFORE_SECONDS + AFTER_SECONDS + 60)
        end_ns = time.monotonic_ns()
        if prober.returncode != 0:
            raise lib.CommandError(f"prober: rc={prober.returncode}: {err}")
        samples = json.loads(out)
        (RAW / "probes.json").write_text(json.dumps(samples) + "\n")
        services = []
        for name, svc in svcs.items():
            own = [s for s in samples if s["port"] == svc["edge_port"]]
            try:
                after = dc.wait_state(name, lib.ACTIVE, lib.ACTIVE_TIMEOUT_SECONDS)
            except lib.CommandError:
                after = dc.svc("get", "--name", name)
            services.append({"name": name, "node_before": svc["node"], "node_after": after["node"],
                             "downtime_seconds": round(downtime_seconds(own, kill_ns, end_ns), 3),
                             "answered_before_kill": any(s["ok"] and s["t_ns"] < kill_ns for s in own)})
        (RAW / "status_after.json").write_text(json.dumps(dc.status(), indent=2) + "\n")

    lib.write_report("c18", {"killed_node": killed, "limit_seconds": LIMIT_SECONDS, "kill_ns": kill_ns,
                             "probe_interval_seconds": PROBE_INTERVAL_SECONDS, "services": services})
    failed = []
    if len(services) < 2:
        failed.append(f"only {len(services)} services on {killed}")
    for s in services:
        if s["node_before"] != killed:
            failed.append(f"{s['name']} was not on {killed}")
        if not s["node_after"] or s["node_after"] == killed:
            failed.append(f"{s['name']} did not leave {killed}")
        if not s["answered_before_kill"]:
            failed.append(f"{s['name']} never answered before the kill")
        if s["downtime_seconds"] > LIMIT_SECONDS:
            failed.append(f"{s['name']} went {s['downtime_seconds']} s without service")
    if failed:
        print("C18 failed: " + "; ".join(failed), file=sys.stderr)
        return 1
    print("C18 ok: " + "; ".join(f"{s['name']} down {s['downtime_seconds']} s" for s in services))
    return 0


if __name__ == "__main__":
    sys.exit(main())
