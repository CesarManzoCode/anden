"""Shared by the acceptance scripts: tree stamp, reports, the D09 namespace prefix, permissions, cache paths."""
import hashlib
import json
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
REPORT_DIR = ROOT / ".run" / "acceptance"
NS_PREFIX = ["systemd-run", "--user", "--scope", "-q", "-p", "Delegate=yes", "--",
             "unshare", "-U", "--map-root-user", "--map-auto", "--net", "--mount", "--pid",
             "--fork", "--kill-child", "--mount-proc"]


def _git(args):
    return subprocess.run(["git"] + args, cwd=ROOT, capture_output=True, text=True, check=True).stdout


def tree_stamp():
    return hashlib.sha256(
        (_git(["rev-parse", "HEAD"]) + _git(["status", "--porcelain"]) + _git(["diff", "HEAD"])).encode()
    ).hexdigest()


def cpu_model():
    for line in Path("/proc/cpuinfo").read_text().splitlines():
        if line.startswith("model name"):
            return line.split(":", 1)[1].strip()
    return "unknown"


def write_report(name, data):
    """Writes .run/acceptance/<name>.json; name is cNN, or c08cold for the cold half of C8."""
    match = re.fullmatch(r"c(\d\d)(cold)?", name)
    if match is None:
        raise ValueError(f"not a report name: {name}")
    report = dict(data)
    report.update(criterion=f"C{match.group(1)}", tree=tree_stamp(), kernel=os.uname().release, cpu=cpu_model())
    REPORT_DIR.mkdir(parents=True, exist_ok=True)
    path = REPORT_DIR / f"{name}.json"
    path.write_text(json.dumps(report, indent=2) + "\n")
    return path


def report_name(script):
    """Report name of an acceptance script: c03_oci.py is c03; only c08_cold.py differs from its prefix."""
    stem = Path(script).stem
    return "c08cold" if stem == "c08_cold" else stem[:3]


def skip(reason):
    """Writes the report with `skipped` and exits with 77. Only for a permission that is really missing (D17)."""
    write_report(report_name(sys.argv[0]), {"skipped": reason})
    print(f"skipped: {reason}", flush=True)
    sys.exit(77)


def ns_wrap(cmd):
    return NS_PREFIX + list(cmd)


def has_ns():
    cmd = ["systemd-run", "--user", "--scope", "-q", "-p", "Delegate=yes", "--",
           "unshare", "-U", "--map-root-user", "--map-auto", "true"]
    try:
        return subprocess.run(cmd, capture_output=True, stdin=subprocess.DEVNULL, timeout=30).returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


def has_sudo():
    try:
        return subprocess.run(["sudo", "-n", "true"], capture_output=True, stdin=subprocess.DEVNULL, timeout=30).returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


def require_ns():
    if not has_ns():
        skip("no delegated cgroup scope with a user namespace (systemd-run --user, unshare -U --map-auto)")


def require_sudo():
    if not has_sudo():
        skip("sudo -n is not available")


def cache_dir():
    """ANDEN_CACHE, or the .cache the Makefile uses when it is not set."""
    return Path(os.environ.get("ANDEN_CACHE") or ROOT / ".cache").resolve()


def ensure_tools():
    """Fetches runtime-tools and the images if needed: a checker runs a script without going through make accept."""
    env = {**os.environ, "ANDEN_CACHE": str(cache_dir())}
    subprocess.run(["make", "tools"], cwd=ROOT, env=env, stdin=subprocess.DEVNULL, check=True)


def locked_images():
    """Entries of images.lock. The copy of each one is the layout <cache>/images/<name>:<tag>."""
    images = []
    for line in (ROOT / "acceptance" / "images.lock").read_text().splitlines():
        if line.strip() and not line.startswith("#"):
            name, repository, tag, digest = line.split()
            images.append({"name": name, "repository": repository, "tag": tag, "digest": digest})
    return images
