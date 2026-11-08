#!/usr/bin/env python3
"""C27: `anden up` and `anden down` leave no trace.

Takes the same snapshot before up, while the datacenter runs and after down, always from outside the product:
cgroups with «anden» under user@<uid>.service, network namespaces with «anden» (lsns -t net and /run/netns),
lines of /proc/self/mountinfo with «anden», and nftables tables. Tables inside the datacenter are listed with
`anden exec --ns <ns> -- nft list tables`; the host's are read with `sudo -n` when it is available, and
without root otherwise, which normally sees nothing. Passes when after equals before in every list, while
running the scope, the five datacenter netns and the root table listing were visible, and the state and socket
directories are gone.
"""
import hashlib
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c27"
STATE = lib.ROOT / ".run" / "dc-c27"
ANDEN = lib.ROOT / "bin" / "anden"
NAMESPACES = ["root", "client", "node1", "node2", "node3"]
UP_TIMEOUT_SECONDS = 180


def run(cmd, timeout=60):
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=timeout)
        return p.returncode, p.stdout, p.stderr
    except (OSError, subprocess.TimeoutExpired) as e:
        return -1, "", str(e)


def user_service_cgroup():
    """The cgroup of the user's systemd manager, where systemd-run --user puts its scopes."""
    uid = os.getuid()
    unit = f"user@{uid}.service"
    for line in Path("/proc/self/cgroup").read_text().splitlines():
        path = line.split("::", 1)[-1]
        if unit in path:
            return Path("/sys/fs/cgroup" + path[: path.index(unit) + len(unit)])
    return Path(f"/sys/fs/cgroup/user.slice/user-{uid}.slice/{unit}")


def cgroups():
    base = user_service_cgroup()
    found = []
    for dirpath, dirnames, _ in os.walk(base):
        for d in dirnames:
            rel = os.path.relpath(os.path.join(dirpath, d), base)
            if "anden" in rel:
                found.append(rel)
    return sorted(found)


def netns():
    found = []
    rc, out, _ = run(["lsns", "-t", "net", "-J", "-o", "NS,PID,COMMAND"])
    if rc == 0:
        for ns in json.loads(out).get("namespaces", []):
            if "anden" in (ns.get("command") or ""):
                found.append(f"ns:{ns['ns']} pid:{ns['pid']} {ns['command']}")
    run_netns = Path("/run/netns")
    if run_netns.is_dir():
        found += [str(p) for p in run_netns.iterdir() if "anden" in p.name]
    return sorted(found)


def mounts():
    return sorted(line for line in Path("/proc/self/mountinfo").read_text().splitlines() if "anden" in line)


def nft(phase, up, sudo):
    tables = []
    if up:
        for ns in NAMESPACES:
            rc, out, err = run([str(ANDEN), "exec", "--ns", ns, "--state", str(STATE), "--", "nft", "list", "tables"])
            (RAW / f"nft_{phase}_{ns}.txt").write_text(f"rc={rc}\n{out}{err}")
            tables += [f"{ns}: {line}" for line in out.splitlines() if line.strip()]
    host = ["sudo", "-n", "nft", "list", "ruleset"] if sudo else ["nft", "list", "ruleset"]
    rc, out, err = run(host)
    (RAW / f"host_ruleset_{phase}.txt").write_text(f"cmd={' '.join(host)}\nrc={rc}\n{out}{err}")
    if rc == 0:
        tables += [f"host: {line.rstrip(' {')}" for line in out.splitlines() if line.startswith("table ")]
    return tables


def snapshot(phase, up, sudo):
    snap = {"netns": netns(), "cgroups": cgroups(), "mounts": mounts(), "nft": nft(phase, up, sudo)}
    (RAW / f"{phase}.json").write_text(json.dumps(snap, indent=2) + "\n")
    return snap


def anden(args, log, timeout=120):
    rc, out, err = run([str(ANDEN)] + args, timeout=timeout)
    (RAW / log).write_text(f"rc={rc}\n--- stdout\n{out}--- stderr\n{err}")
    return rc, out


def main():
    lib.require_ns()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    build = subprocess.run(["make", "build"], cwd=lib.ROOT, capture_output=True, text=True, stdin=subprocess.DEVNULL)
    (RAW / "build.log").write_text(build.stdout + build.stderr)
    if build.returncode != 0:
        print("make build failed; see c27/build.log", file=sys.stderr)
        return 1
    if STATE.exists():
        anden(["down", "--state", str(STATE)], "predown.log")
    sudo = lib.has_sudo()
    h = hashlib.sha1(os.path.abspath(STATE).encode()).hexdigest()[:12]
    runtime_dir = os.environ.get("XDG_RUNTIME_DIR") or "/tmp"
    sockets = Path(runtime_dir) / f"anden-{h}"

    before = snapshot("before", False, sudo)
    down_rc = None
    try:
        up_rc, _ = anden(["up", "--state", str(STATE), "--json"], "up.log", timeout=UP_TIMEOUT_SECONDS)
        during = snapshot("during", up_rc == 0, sudo)
        status_rc, _ = anden(["status", "--state", str(STATE), "--json"], "status.log")
        down_rc, _ = anden(["down", "--state", str(STATE), "--json"], "down.log")
    finally:
        if down_rc is None:
            anden(["down", "--state", str(STATE)], "down.log")
    after = snapshot("after", False, sudo)

    scope = f"anden-{h}.scope"
    checks = {
        "up_ok": up_rc == 0,
        "status_ok": status_rc == 0,
        "down_ok": down_rc == 0,
        "scope_seen_during": any(scope in c for c in during["cgroups"]),
        "dc_netns_seen_during": sum(1 for n in during["netns"] if "anden" in n) - sum(1 for n in before["netns"] if "anden" in n) >= len(NAMESPACES),
        "root_nft_listed_during": (RAW / "nft_during_root.txt").exists() and (RAW / "nft_during_root.txt").read_text().startswith("rc=0"),
        "state_removed": not STATE.exists(),
        "sockets_removed": not sockets.exists(),
    }
    for key in ("netns", "cgroups", "mounts", "nft"):
        checks[f"{key}_after_equals_before"] = after[key] == before[key]
    lib.write_report("c27", {
        "before": before, "during": during, "after": after,
        "scope_unit": scope, "host_nft_with_sudo": sudo, "checks": checks,
    })
    failed = [k for k, ok in checks.items() if not ok]
    if failed:
        print("C27 failed: " + ", ".join(failed), file=sys.stderr)
        return 1
    print("C27 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
