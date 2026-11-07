#!/usr/bin/env python3
"""C1: the published API generates the code in the repository, and an external client walks it.

Regenerates with `make gen` into a temporary directory and compares it with gen/. Then apiclient, which only knows the
generated code, runs create, get, sleep, wake, delete and get_after_delete against a datacenter. Passes when nothing differs
and each operation has the expected gRPC code and, where it settles, the expected state.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c01"
GRPC_OK = 0
GRPC_NOT_FOUND = 5
EXPECTED = {
    "create": (GRPC_OK, lib.ACTIVE),
    "get": (GRPC_OK, lib.ACTIVE),
    "sleep": (GRPC_OK, lib.ASLEEP),
    "wake": (GRPC_OK, lib.ACTIVE),
    "delete": (GRPC_OK, None),
    "get_after_delete": (GRPC_NOT_FOUND, None),
}
CLIENT_TIMEOUT_SECONDS = 300
GEN_TIMEOUT_SECONDS = 300


def tree_files(root):
    return {str(p.relative_to(root)): p.read_bytes() for p in Path(root).rglob("*") if p.is_file()}


def generated_diff():
    """Files that differ between gen/ and a fresh `make gen`; ["make gen failed"] if it could not run."""
    env = {**os.environ, "ANDEN_CACHE": str(lib.cache_dir())}
    with tempfile.TemporaryDirectory() as out:
        p = subprocess.run(["make", "gen", f"GEN_OUT={out}"], cwd=lib.ROOT, env=env, capture_output=True, text=True,
                           stdin=subprocess.DEVNULL, timeout=GEN_TIMEOUT_SECONDS)
        (RAW / "gen.log").write_text(p.stdout + p.stderr)
        if p.returncode != 0:
            return ["make gen failed"]
        fresh, committed = tree_files(out), tree_files(lib.ROOT / "gen")
    return sorted(name for name in fresh.keys() | committed.keys() if fresh.get(name) != committed.get(name))


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    diff = generated_diff()
    with lib.Datacenter("c01") as dc:
        digest = dc.import_binary("echosvc", RAW)
        client = subprocess.run([str(lib.ROOT / "bin" / "apiclient"), "--socket", dc.info["api_socket"],
                                 "--name", "c01", "--image-digest", digest, "--port", str(lib.ECHO_PORT)],
                                capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=CLIENT_TIMEOUT_SECONDS)
    (RAW / "client.log").write_text(client.stdout)
    (RAW / "client.err").write_text(client.stderr)
    ops = [json.loads(line) for line in client.stdout.splitlines() if line.strip()]

    lib.write_report("c01", {"generated_diff_empty": not diff, "generated_diff": diff, "ops": ops, "client_rc": client.returncode})
    got = {o["op"]: o for o in ops}
    checks = {"generated_diff_empty": not diff, "client_rc": client.returncode == 0}
    for op, (code, state) in EXPECTED.items():
        o = got.get(op)
        checks[op] = o is not None and o["grpc_code"] == code and (state is None or o["state"] == state)
    failed = [k for k, ok in checks.items() if not ok]
    if failed:
        print(f"C1 failed: {', '.join(failed)}; diff {diff}; {client.stderr}", file=sys.stderr)
        return 1
    print("C1 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
