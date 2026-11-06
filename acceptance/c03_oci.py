#!/usr/bin/env python3
"""C3: runs the runtime-tools tests of oci_subset.txt against anden-runtime, under the D09 namespace prefix.

Each test is validation/<test>/<test>.t, run from $ANDEN_CACHE/runtime-tools with RUNTIME set to the absolute
path of bin/anden-runtime and a limit of 120 s. Fails if the subset is missing or a test gives `not ok`.
"""
import os
import re
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

TIMEOUT_SECONDS = 120
SUBSET = lib.ROOT / "acceptance" / "oci_subset.txt"
NOT_OK = re.compile(r"^not ok\b")
OK = re.compile(r"^ok\b")


def main():
    raw = lib.REPORT_DIR / "c03"
    raw.mkdir(parents=True, exist_ok=True)
    tests = SUBSET.read_text().split() if SUBSET.is_file() else []
    if not tests:
        lib.write_report("c03", {"results": {}, "error": f"missing or empty subset {SUBSET}"})
        print(f"missing or empty subset {SUBSET}", flush=True)
        return 1
    lib.require_ns()
    subprocess.run(["make", "build"], cwd=lib.ROOT, stdin=subprocess.DEVNULL, check=True)
    lib.ensure_tools()

    suite = lib.cache_dir() / "runtime-tools"
    runtime = (lib.ROOT / "bin" / "anden-runtime").resolve()
    env = {**os.environ, "RUNTIME": str(runtime)}
    results = {}
    failed = []
    for test in tests:
        binary = suite / "validation" / test / f"{test}.t"
        tap = raw / f"{test}.tap"
        if not binary.is_file():
            tap.write_text(f"# missing {binary}\n")
            results[test] = {"rc": -1, "error": "missing test binary"}
            failed.append(test)
            continue
        try:
            proc = subprocess.run(lib.ns_wrap([str(binary)]), cwd=suite, env=env, stdin=subprocess.DEVNULL,
                                  capture_output=True, text=True, timeout=TIMEOUT_SECONDS)
            out, err, rc, timed_out = proc.stdout, proc.stderr, proc.returncode, False
        except subprocess.TimeoutExpired as e:
            out = e.stdout.decode(errors="replace") if isinstance(e.stdout, bytes) else (e.stdout or "")
            err = e.stderr.decode(errors="replace") if isinstance(e.stderr, bytes) else (e.stderr or "")
            rc, timed_out = -9, True
        tap.write_text(out)
        (raw / f"{test}.stderr").write_text(err)
        lines = [line.strip() for line in out.splitlines()]
        not_ok = [line for line in lines if NOT_OK.match(line)]
        oks = [line for line in lines if OK.match(line)]
        results[test] = {"rc": rc, "ok": len(oks), "not_ok": len(not_ok), "timed_out": timed_out}
        status = "pass"
        if rc != 0 or not_ok or not oks or timed_out:
            failed.append(test)
            status = "FAIL"
        print(f"{test}: {status} (rc {rc}, {len(oks)} ok, {len(not_ok)} not ok{', timeout' if timed_out else ''})",
              flush=True)

    lib.write_report("c03", {"runtime": str(runtime), "subset": tests, "results": results, "failed": failed})
    if failed:
        print(f"failed: {' '.join(failed)}", flush=True)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
