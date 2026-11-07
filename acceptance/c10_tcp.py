#!/usr/bin/env python3
"""C10: the wake works for a TCP service that is not HTTP, and with an unmodified client.

valkey-server, packed with its libraries and deployed through the API, runs without persistence. Each cycle starts with the
service confirmed ASLEEP: PING wakes it, and SET and GET then go on the woken instance. A SET does not outlive the sleep with
relaunch, so each cycle has its own key. Passes when every answer of redis-cli, run with only -h and -p, is the expected one.
"""
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c10"
VALKEY = "/usr/bin/valkey-server"
PORT = 6379
CYCLES = 5
CPU_MILLIS = 500
MEMORY_MIB = 256
CLIENT_TIMEOUT_SECONDS = 60
# Protected mode only lets loopback in, and the proxy reaches the service from svc0.
VALKEY_CMD = [VALKEY, "--port", str(PORT), "--bind", "0.0.0.0", "--protected-mode", "no", "--save", ""]


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c10") as dc:
        digest = dc.import_pack(VALKEY, RAW, VALKEY_CMD)
        dc.svc("create", "--name", "valkey", "--image", digest, "--port", str(PORT), "--cpu-millis", str(CPU_MILLIS),
               "--memory-mib", str(MEMORY_MIB), "--idle-seconds", "0")
        svc = dc.wait_state("valkey", lib.ACTIVE, lib.ACTIVE_TIMEOUT_SECONDS)
        client_args = ["-h", dc.info["edge_ip"], "-p", str(svc["edge_port"])]
        slept = 0
        cases = []
        for cycle in range(CYCLES):
            if dc.sleep_confirmed("valkey")["state"] == lib.ASLEEP:
                slept += 1
            key, value = f"c10-key-{cycle}", f"value-{cycle}"
            for cmd, expected in ((["PING"], "PONG"), (["SET", key, value], "OK"), (["GET", key], value)):
                p = dc.exec("client", "redis-cli", *client_args, *cmd, timeout=CLIENT_TIMEOUT_SECONDS)
                actual = p.stdout.strip() if p.returncode == 0 else f"rc={p.returncode}: {p.stderr.strip()}"
                cases.append({"cmd": " ".join(cmd), "expected": expected, "actual": actual})

    slept_verified = slept == CYCLES
    lib.write_report("c10", {"cycles": CYCLES, "slept_verified": slept_verified, "client_args": client_args, "cases": cases})
    wrong = [c for c in cases if c["actual"] != c["expected"]]
    if wrong or not slept_verified:
        print(f"C10 failed: slept {slept}/{CYCLES}; {wrong}", file=sys.stderr)
        return 1
    print(f"C10 ok: {len(cases)} cases over {CYCLES} cycles")
    return 0


if __name__ == "__main__":
    sys.exit(main())
