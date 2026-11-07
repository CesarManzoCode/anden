#!/usr/bin/env python3
"""C11: the internal name of a service follows it when it sleeps, moves to another node and wakes there (D22).

An echosvc that never sleeps on its own. From the client netns, `dig @10.80.0.1 <name>.anden.internal +short` must give its
address. The service is then confirmed ASLEEP, moved with MoveService to another node and woken with WakeService. Before the
wake, the sleeping set of the old node must no longer hold the old address and the one of the new node must hold the new
address. After it, the instance must run on the new node, dig must give the new address, and an HTTP request from the client
netns to that address must answer 200. The client is the client netns: `anden exec` reaches only client, root and the nodes,
and echosvc has no resolver to query from inside a container.
"""
import json
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c11"
NAME = "web"
FQDN = f"{NAME}.anden.internal"
DNS_SERVER = "10.80.0.1"
SUBNETS = {"node1": "10.81.", "node2": "10.82.", "node3": "10.83."}
MOVE_TIMEOUT_SECONDS = 30
SET_TIMEOUT_SECONDS = 15
CONNECT_SCRIPT = ("import http.client,sys\n"
                  "c=http.client.HTTPConnection(sys.argv[1],int(sys.argv[2]),timeout=10)\n"
                  "c.request('GET','/')\nprint(c.getresponse().status)\n")


def dig(dc):
    p = dc.exec("client", "dig", f"@{DNS_SERVER}", FQDN, "+short", timeout=30)
    return p.stdout if p.returncode == 0 else f"rc={p.returncode}: {p.stdout}{p.stderr}"


def sleeping_set(dc, node):
    p = dc.exec(node, "nft", "list", "set", "inet", "anden_wake", "sleeping", timeout=30)
    return p.stdout if p.returncode == 0 else f"rc={p.returncode}: {p.stderr}"


def wait_moved(dc, node, old_addr):
    """Waits until the service reads ASLEEP with the new home and an address that is not the old one."""
    deadline = time.monotonic() + MOVE_TIMEOUT_SECONDS
    while True:
        svc = dc.svc("get", "--name", NAME)
        if svc["state"] == lib.ASLEEP and svc["node"] == node and svc["addr"] and svc["addr"] != old_addr:
            return svc
        if time.monotonic() > deadline:
            raise lib.CommandError(f"{NAME} after the move: {svc['state']} on {svc['node']} at {svc['addr']}")
        time.sleep(0.1)


def wait_sets(dc, old_node, old_addr, new_node, new_addr):
    """Waits until each agent updated its sleeping set. Returns whether both did, and the raw sets."""
    deadline = time.monotonic() + SET_TIMEOUT_SECONDS
    while True:
        old_set, new_set = sleeping_set(dc, old_node), sleeping_set(dc, new_node)
        ok = (not old_set.startswith("rc=") and f"{old_addr} . " not in old_set
              and f"{new_addr} . " in new_set)
        if ok or time.monotonic() > deadline:
            return ok, old_set, new_set
        time.sleep(0.2)


def connect(dc, addr):
    p = dc.exec("client", "python3", "-I", "-c", CONNECT_SCRIPT, addr, str(lib.ECHO_PORT), timeout=60)
    return p.returncode == 0 and p.stdout.strip() == "200"


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c11") as dc:
        digest = dc.import_binary("echosvc", RAW)
        svc = dc.create_echo(NAME, digest)
        node_before, addr_before = svc["node"], svc["addr"]
        dig_raw_before = dig(dc)

        dc.sleep_confirmed(NAME)
        node_after = next(n for n in sorted(SUBNETS) if n != node_before)
        dc.svc("move", "--name", NAME, "--node", node_after)
        moved = wait_moved(dc, node_after, addr_before)
        sets_ok, old_set, new_set = wait_sets(dc, node_before, addr_before, node_after, moved["addr"])
        (RAW / f"sleeping-{node_before}.txt").write_text(old_set)
        (RAW / f"sleeping-{node_after}.txt").write_text(new_set)

        dc.svc("wake", "--name", NAME)
        woken = dc.wait_state(NAME, lib.ACTIVE, lib.ACTIVE_TIMEOUT_SECONDS)
        addr_after = woken["addr"]
        woken_node = (woken.get("instance") or {}).get("node")
        dig_raw_after = dig(dc)
        connect_after_ok = connect(dc, dig_raw_after.strip())
        (RAW / "services.json").write_text(json.dumps({"before": svc, "moved": moved, "woken": woken}, indent=2) + "\n")

    report = {
        "name": FQDN,
        "node_before": node_before,
        "node_after": woken_node,
        "addr_before": addr_before,
        "addr_after": addr_after,
        "dig_raw_before": dig_raw_before,
        "dig_raw_after": dig_raw_after,
        "connect_after_ok": connect_after_ok,
        "sleeping_sets_moved": sets_ok,
    }
    lib.write_report("c11", report)
    problems = []
    if dig_raw_before.strip() != addr_before:
        problems.append(f"dig before gave {dig_raw_before!r}, the service is at {addr_before}")
    if woken_node != node_after or not addr_after.startswith(SUBNETS[node_after]) or addr_after == addr_before:
        problems.append(f"woke on {woken_node} at {addr_after}, expected {node_after} in {SUBNETS[node_after]}0.0/16")
    if dig_raw_after.strip() != addr_after:
        problems.append(f"dig after gave {dig_raw_after!r}, the service is at {addr_after}")
    if not sets_ok:
        problems.append("the sleeping sets did not follow the move; see c11/sleeping-*.txt")
    if not connect_after_ok:
        problems.append(f"no HTTP 200 from {dig_raw_after.strip()}:{lib.ECHO_PORT}")
    if problems:
        print("C11 failed: " + "; ".join(problems), file=sys.stderr)
        return 1
    print(f"C11 ok: {FQDN} went from {node_before} {addr_before} to {node_after} {addr_after}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
