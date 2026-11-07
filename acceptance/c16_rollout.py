#!/usr/bin/env python3
"""C16: a rollout under load loses no request, and after the overlap only v2 answers (D24; D12: 200/s for 30 s, overlap 5 s,
draining 2 s).

An echosvc v1 that never sleeps takes `vegeta attack -rate=200/s -duration=30s -keepalive=false` from the client netns through
the edge, so every request is a new connection that follows the edge map. Some seconds into the load, DeployService sends v2 with
overlap 5 s and draining 2 s. The switch is seen from outside, when GetService gives version 2; once the overlap that follows it
is over, `wakeclient sample` asks the edge which version answers, one line per request in c16/after.log, while v1 drains.
The report of vegeta is c16/vegeta.json.

Passes when vegeta saw only successes and every sample after the overlap is v2.
"""
import json
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c16"
NAME = "web"
OVERLAP_SECONDS = 5
DRAINING_SECONDS = 2
DEPLOY_AFTER_SECONDS = 5
SWITCH_TIMEOUT_SECONDS = 60
# A little past the overlap: the reconciler acts on its next pass and the edge after it.
AFTER_OVERLAP_MARGIN_SECONDS = 1
SAMPLES = 50


def spec_flags(digest, version):
    return ["--name", NAME, "--image", digest, "--port", str(lib.ECHO_PORT), "--cpu-millis", "200", "--memory-mib", "64",
            "--idle-seconds", "0", "--env", f"PORT={lib.ECHO_PORT}", "--env", f"VERSION={version}",
            "--overlap-seconds", str(OVERLAP_SECONDS), "--draining-seconds", str(DRAINING_SECONDS)]


def wait_version(dc, version, timeout):
    """Waits until GetService gives `version` with a running instance of it; returns the service."""
    deadline = time.monotonic() + timeout
    while True:
        svc = dc.svc("get", "--name", NAME)
        if int(svc["version"]) == version and int((svc.get("instance") or {}).get("version") or 0) == version:
            return svc
        if time.monotonic() > deadline:
            raise lib.CommandError(f"{NAME} is not at version {version} after {timeout} s: {svc}")
        time.sleep(0.05)


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c16") as dc:
        digest = dc.import_binary("echosvc", RAW)
        dc.svc("create", *spec_flags(digest, "v1"))
        svc = dc.wait_state(NAME, lib.ACTIVE, lib.ACTIVE_TIMEOUT_SECONDS)
        before = lib.sample_versions(dc, svc, 1)
        load = lib.Vegeta(dc, svc, RAW)
        time.sleep(DEPLOY_AFTER_SECONDS)
        deployed = time.monotonic()
        dc.svc("deploy", *spec_flags(digest, "v2"))
        try:
            wait_version(dc, 2, SWITCH_TIMEOUT_SECONDS)
            switch_seconds = time.monotonic() - deployed
            time.sleep(OVERLAP_SECONDS + AFTER_OVERLAP_MARGIN_SECONDS)
            after = lib.sample_versions(dc, svc, SAMPLES)
        except lib.CommandError as e:
            switch_seconds, after = None, [f"error: {e}"]
        sampled_at = time.monotonic() - load.started
        vegeta = load.report(RAW / "vegeta.json")
        final = dc.svc("get", "--name", NAME)
    (RAW / "after.log").write_text("".join(line + "\n" for line in after))

    report = {
        "after_overlap_expected": "v2",
        "before": before,
        "deploy_after_seconds": DEPLOY_AFTER_SECONDS,
        "switch_seconds": switch_seconds,
        "overlap_seconds": OVERLAP_SECONDS,
        "draining_seconds": DRAINING_SECONDS,
        "samples_after_overlap": len(after),
        "samples_done_at_seconds": sampled_at,
        "versions_after_overlap": {v: after.count(v) for v in sorted(set(after))},
        "vegeta": {k: vegeta.get(k) for k in ("requests", "rate", "success", "status_codes", "errors")},
        "final_version": int(final["version"]),
        "final_state": final["state"],
    }
    lib.write_report("c16", report)
    print(json.dumps(report), flush=True)
    ok = vegeta.get("success") == 1 and len(after) == SAMPLES and all(v == "v2" for v in after)
    if not ok:
        print("C16 failed", file=sys.stderr)
        return 1
    print("C16 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
