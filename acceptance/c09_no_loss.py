#!/usr/bin/env python3
"""C9: concurrent requests on a sleeping service are neither lost nor duplicated (D12).

An echosvc logs each X-Request-Id to a volume, which outlives its relaunches. In each of 5 rounds, with the service confirmed
ASLEEP, 32 clients make 10 requests each through the edge, each request on a connection of its own and with a unique id. The
client never retries. Passes when every client request got an answer and the service log has each of those ids exactly once.
"""
import json
import shutil
import sys
from collections import Counter
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c09"
CLIENTS = 32
ROUNDS = 5
REQUESTS = 10
CPU_MILLIS = 1000
MEMORY_MIB = 128
VOLUME = "c09-log"
LOG_DIR = "/log"
LOG_NAME = "service.log"
ROUND_TIMEOUT_SECONDS = 300


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c09") as dc:
        digest = dc.import_binary("echosvc", RAW)
        svc = dc.create_echo("target", digest, cpu_millis=CPU_MILLIS, memory_mib=MEMORY_MIB, extra=[
            "--volume", f"{VOLUME}:{LOG_DIR}", "--env", f"LOG_PATH={LOG_DIR}/{LOG_NAME}"])
        verified = 0
        client_log = ""
        for round_number in range(ROUNDS):
            if dc.sleep_confirmed("target")["state"] == lib.ASLEEP:
                verified += 1
            p = dc.exec("client", str(lib.WAKECLIENT), "load", "--addr", dc.edge_addr(svc), "--clients", str(CLIENTS),
                        "--requests", str(REQUESTS), "--prefix", f"round{round_number}", timeout=ROUND_TIMEOUT_SECONDS)
            client_log += p.stdout
        service_log = (dc.state / "nodes" / svc["node"] / "volumes" / VOLUME / LOG_NAME).read_text()
    (RAW / "client.log").write_text(client_log)
    (RAW / "service.log").write_text(service_log)

    lib.write_report("c09", {
        "clients": CLIENTS, "rounds": ROUNDS, "requests_per_client": REQUESTS, "rounds_asleep_verified": verified,
    })
    client = [json.loads(line) for line in client_log.splitlines() if line.strip()]
    service = Counter(json.loads(line)["id"] for line in service_log.splitlines() if line.strip())
    ids = [c["id"] for c in client]
    checks = {
        "every_round_asleep": verified == ROUNDS,
        "all_requests_made": len(ids) == CLIENTS * ROUNDS * REQUESTS,
        "client_ids_unique": len(set(ids)) == len(ids),
        "all_answered": all(c["ok"] for c in client),
        "none_duplicated": all(count == 1 for count in service.values()),
        "none_lost_or_unknown": set(ids) == set(service),
    }
    failed = [k for k, ok in checks.items() if not ok]
    if failed:
        print(f"C9 failed: {', '.join(failed)}", file=sys.stderr)
        return 1
    print(f"C9 ok: {len(ids)} requests")
    return 0


if __name__ == "__main__":
    sys.exit(main())
