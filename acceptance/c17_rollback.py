#!/usr/bin/env python3
"""C17: a deploy whose healthcheck never passes is rolled back without touching the edge, under load (D24; D12: 200/s for 30 s,
overlap 5 s, draining 2 s).

An echosvc that never sleeps is created as v1 and deployed as v2, and the script waits for that rollout to end. Then
`vegeta attack -rate=200/s -duration=30s -keepalive=false` runs from the client netns through the edge, every request with the
header X-Request-Id: c17, and a few seconds in DeployService sends v3: the same echosvc with FAIL_HEALTH set, so its HTTP
healthcheck of /healthz answers 503, with a healthcheck timeout of 10 s. v3 writes each request it receives to LOG_PATH, in a
volume of its node that outlives it; that file, copied to c17/v3_requests.log, is what reached v3. It exists from the start
of v3, which shows that v3 did run. After the timeout and a margin, GetService and `wakeclient sample` say which version is
active. The report of vegeta is c17/vegeta.json.

Passes when vegeta saw only successes, v3 ran and received no request, and v2 is the active version both for the API and for
whoever samples the edge.
"""
import json
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c17"
NAME = "web"
OVERLAP_SECONDS = 5
DRAINING_SECONDS = 2
HEALTHCHECK_TIMEOUT_SECONDS = 10
DEPLOY_AFTER_SECONDS = 3
# The reconciler gives up v3 on its first pass past the timeout; the margin covers that and the agent destroying v3.
ROLLBACK_MARGIN_SECONDS = 5
ROLLOUT_TIMEOUT_SECONDS = 60
VOLUME = "v3log"
LOG_DIR = "/v3log"
LOG_NAME = "requests.log"
REQUEST_ID = "c17"
SAMPLES = 20


def spec_flags(digest, version, *extra):
    return ["--name", NAME, "--image", digest, "--port", str(lib.ECHO_PORT), "--cpu-millis", "200", "--memory-mib", "64",
            "--idle-seconds", "0", "--env", f"PORT={lib.ECHO_PORT}", "--env", f"VERSION={version}",
            "--overlap-seconds", str(OVERLAP_SECONDS), "--draining-seconds", str(DRAINING_SECONDS), *extra]


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


def deploy_when_free(dc, flags, timeout):
    """DeployService, again while the previous rollout of the service has not ended."""
    deadline = time.monotonic() + timeout
    while True:
        try:
            return dc.svc("deploy", *flags)
        except lib.CommandError as e:
            if "in progress" not in str(e) or time.monotonic() > deadline:
                raise
        time.sleep(0.5)


def v3_log(dc):
    """The request log of v3 in the volume of whichever node ran it, or None if v3 never opened it."""
    for path in sorted((dc.state / "nodes").glob(f"*/volumes/{VOLUME}/{LOG_NAME}")):
        return path.read_text()
    return None


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    with lib.Datacenter("c17") as dc:
        digest = dc.import_binary("echosvc", RAW)
        dc.svc("create", *spec_flags(digest, "v1"))
        dc.wait_state(NAME, lib.ACTIVE, lib.ACTIVE_TIMEOUT_SECONDS)
        dc.svc("deploy", *spec_flags(digest, "v2"))
        svc = wait_version(dc, 2, ROLLOUT_TIMEOUT_SECONDS)
        # The rollout to v2 ends after its overlap and its draining.
        time.sleep(OVERLAP_SECONDS + DRAINING_SECONDS + 1)
        before = lib.sample_versions(dc, svc, 1)

        load = lib.Vegeta(dc, svc, RAW, headers=[f"X-Request-Id: {REQUEST_ID}"])
        time.sleep(DEPLOY_AFTER_SECONDS)
        deploy_when_free(dc, spec_flags(digest, "v3", "--env", "FAIL_HEALTH=1", "--health-path", "/healthz",
                                        "--healthcheck-timeout-seconds", str(HEALTHCHECK_TIMEOUT_SECONDS),
                                        "--volume", f"{VOLUME}:{LOG_DIR}", "--env", f"LOG_PATH={LOG_DIR}/{LOG_NAME}"),
                         ROLLOUT_TIMEOUT_SECONDS)
        # What the API said of the version while v3 was trying, every half second.
        api_versions = []
        end = time.monotonic() + HEALTHCHECK_TIMEOUT_SECONDS + ROLLBACK_MARGIN_SECONDS
        while time.monotonic() < end:
            api_versions.append(int(dc.svc("get", "--name", NAME)["version"]))
            time.sleep(0.5)
        final = dc.svc("get", "--name", NAME)
        samples = lib.sample_versions(dc, svc, SAMPLES)
        vegeta = load.report(RAW / "vegeta.json")
        log = v3_log(dc)
    (RAW / "v3_requests.log").write_text(log or "")
    (RAW / "samples.log").write_text("".join(line + "\n" for line in samples))

    final_active = samples[0] if len(set(samples)) == 1 else None
    v3_requests = len((log or "").splitlines())
    report = {
        "final_active_version": final_active,
        "before": before,
        "healthcheck_timeout_seconds": HEALTHCHECK_TIMEOUT_SECONDS,
        "deploy_after_seconds": DEPLOY_AFTER_SECONDS,
        "api_versions_during_deploy": sorted(set(api_versions)),
        "final_api_version": int(final["version"]),
        "final_api_instance_version": int((final.get("instance") or {}).get("version") or 0),
        "final_state": final["state"],
        "samples": {v: samples.count(v) for v in sorted(set(samples))},
        "v3_started": log is not None,
        "v3_requests": v3_requests,
        "vegeta": {k: vegeta.get(k) for k in ("requests", "rate", "success", "status_codes", "errors")},
    }
    lib.write_report("c17", report)
    print(json.dumps(report), flush=True)
    ok = (vegeta.get("success") == 1 and final_active == "v2" and report["v3_started"] and v3_requests == 0
          and report["final_api_version"] == 2 and report["final_api_instance_version"] == 2 and final["state"] == lib.ACTIVE)
    if not ok:
        print("C17 failed", file=sys.stderr)
        return 1
    print("C17 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
