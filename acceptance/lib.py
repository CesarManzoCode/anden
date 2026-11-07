"""Shared by the acceptance scripts: tree stamp, reports, the D09 namespace prefix, permissions, cache paths, the datacenter."""
import hashlib
import json
import math
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
REPORT_DIR = ROOT / ".run" / "acceptance"
NS_PREFIX = ["systemd-run", "--user", "--scope", "-q", "-p", "Delegate=yes", "--",
             "unshare", "-U", "--map-root-user", "--map-auto", "--net", "--mount", "--pid",
             "--fork", "--kill-child", "--mount-proc"]
ANDEN = ROOT / "bin" / "anden"
MKIMAGE = ROOT / "bin" / "mkimage"
WAKECLIENT = ROOT / "bin" / "wakeclient"
GOVERNOR_PATH = Path("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")
ACTIVE = "SERVICE_STATE_ACTIVE"
FAILED = "SERVICE_STATE_FAILED"
ASLEEP = "SERVICE_STATE_ASLEEP"
ECHO_PORT = 8080
UP_TIMEOUT_SECONDS = 180
ACTIVE_TIMEOUT_SECONDS = 60
SLEEP_TIMEOUT_SECONDS = 30
MEASURE_TIMEOUT_SECONDS = 60
NODE_COUNT = 3
# Runs in the client netns: one new connection per request, so that each one crosses the edge.
LOAD_SCRIPT = """
import http.client, json, sys, time
host, port, end = sys.argv[1], int(sys.argv[2]), time.monotonic() + float(sys.argv[3])
requests = errors = 0
while time.monotonic() < end:
    requests += 1
    try:
        c = http.client.HTTPConnection(host, port, timeout=5)
        c.request("GET", "/")
        r = c.getresponse()
        r.read()
        c.close()
        if r.status != 200:
            errors += 1
    except (OSError, http.client.HTTPException):
        errors += 1
    time.sleep(0.01)
print(json.dumps({"requests": requests, "errors": errors}))
"""


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
    report.update(criterion=f"C{match.group(1)}", tree=tree_stamp(), kernel=os.uname().release)
    # The table of section 6 gives C5 a `cpu` object of its own, which must survive: it then carries `cpu_model`.
    report.setdefault("cpu", cpu_model())
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


def build():
    """make build, so that a checker running one script gets binaries of the current tree."""
    subprocess.run(["make", "build"], cwd=ROOT, stdin=subprocess.DEVNULL, check=True)


def node_cpu_millis():
    """What one node offers in the simulator: a third of the CPUs this process can use."""
    return len(os.sched_getaffinity(0)) * 1000 // NODE_COUNT


def governor():
    """CPU frequency governor, or `unavailable` where the kernel exposes none."""
    try:
        return GOVERNOR_PATH.read_text().strip()
    except OSError:
        return "unavailable"


def disk_fstype():
    """Filesystem of the disk under the repository, where the state of a datacenter lives."""
    return subprocess.run(["findmnt", "-no", "FSTYPE", "--target", str(ROOT)], capture_output=True, text=True,
                          stdin=subprocess.DEVNULL, check=True).stdout.strip()


def percentiles(samples):
    """p50, p90, p99 (nearest rank) and max of a list of samples; None for each if it is empty."""
    ordered = sorted(samples)
    if not ordered:
        return {"p50": None, "p90": None, "p99": None, "max": None}
    rank = lambda pct: ordered[max(1, math.ceil(pct * len(ordered) / 100)) - 1]
    return {"p50": rank(50), "p90": rank(90), "p99": rank(99), "max": ordered[-1]}


class CommandError(RuntimeError):
    pass


class Load:
    """External load on one service: HTTP from the client netns through the edge. wakeclient can replace the loader."""

    def __init__(self, dc, name, edge_port, seconds):
        self.name = name
        self.proc = subprocess.Popen(
            dc.exec_cmd("client", "python3", "-I", "-c", LOAD_SCRIPT, dc.info["edge_ip"], str(edge_port), str(seconds)),
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def result(self, timeout=120):
        out, err = self.proc.communicate(timeout=timeout)
        if self.proc.returncode != 0:
            raise CommandError(f"load on {self.name}: rc={self.proc.returncode}: {err}")
        return {"name": self.name, **json.loads(out)}


class Datacenter:
    """A datacenter of its own for one criterion: up on enter, down on exit, with --state .run/dc-<criterion>."""

    def __init__(self, criterion):
        self.state = ROOT / ".run" / f"dc-{criterion}"
        self.info = {}

    def anden(self, *args, timeout=60):
        return subprocess.run([str(ANDEN), *args, "--state", str(self.state)], capture_output=True, text=True,
                              stdin=subprocess.DEVNULL, timeout=timeout)

    def _checked(self, *args, timeout=60):
        p = self.anden(*args, timeout=timeout)
        if p.returncode != 0:
            raise CommandError(f"anden {' '.join(args)}: rc={p.returncode}: {p.stderr}")
        return p.stdout

    def __enter__(self):
        build()
        if self.state.exists():
            self.anden("down", timeout=120)
        self._checked("up", "--json", timeout=UP_TIMEOUT_SECONDS)
        self.info = json.loads((self.state / "dc.json").read_text())
        return self

    def __exit__(self, *exc):
        self.anden("down", timeout=120)
        return False

    def exec_cmd(self, ns, *cmd):
        return [str(ANDEN), "exec", "--ns", ns, "--state", str(self.state), "--", *cmd]

    def exec(self, ns, *cmd, timeout=60):
        return subprocess.run(self.exec_cmd(ns, *cmd), capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=timeout)

    def svc(self, action, *args, timeout=60):
        return json.loads(self._checked("svc", action, *args, "--json", timeout=timeout))

    def status(self):
        return json.loads(self._checked("status", "--json"))

    def import_pack(self, binary_path, raw_dir, cmd=()):
        """Packs a binary in a one layer image with this Cmd, imports it and returns its digest."""
        layout = Path(raw_dir) / f"image-{Path(binary_path).name}"
        shutil.rmtree(layout, ignore_errors=True)
        cmd_flags = [flag for arg in cmd for flag in ("--cmd", arg)]
        subprocess.run([str(MKIMAGE), "pack", "--bin", str(binary_path), "--out", str(layout), "--tag", "latest", *cmd_flags],
                       check=True, capture_output=True, stdin=subprocess.DEVNULL)
        out = self._checked("image", "import", "--layout", f"{layout}:latest", "--json")
        return json.loads(out)["digest"]

    def import_binary(self, binary, raw_dir):
        """Packs bin/<binary> in a one layer image, imports it and returns its digest."""
        return self.import_pack(ROOT / "bin" / binary, raw_dir)

    def create_echo(self, name, digest, cpu_millis=200, memory_mib=64, pids_max=0, idle_seconds=0, extra=()):
        """Creates an echosvc, by default one that never sleeps, and waits until it answers."""
        self.svc("create", "--name", name, "--image", digest, "--port", str(ECHO_PORT), "--cpu-millis", str(cpu_millis),
                 "--memory-mib", str(memory_mib), "--pids-max", str(pids_max), "--idle-seconds", str(idle_seconds),
                 "--env", f"PORT={ECHO_PORT}", *extra)
        return self.wait_state(name, ACTIVE, ACTIVE_TIMEOUT_SECONDS)

    def wait_state(self, name, state, timeout):
        deadline = time.monotonic() + timeout
        while True:
            svc = self.svc("get", "--name", name)
            if svc["state"] == state:
                return svc
            if time.monotonic() > deadline:
                raise CommandError(f"{name} is {svc['state']} after {timeout} s, expected {state}")
            time.sleep(0.1)

    def sleep_confirmed(self, name):
        """SleepService on an ACTIVE service, and waits for ASLEEP. It starts from ACTIVE because the API goes behind the
        node: a service that a connection just woke can still read ASLEEP, and that would confirm nothing."""
        self.wait_state(name, ACTIVE, ACTIVE_TIMEOUT_SECONDS)
        self.svc("sleep", "--name", name)
        return self.wait_state(name, ASLEEP, SLEEP_TIMEOUT_SECONDS)

    def edge_addr(self, svc):
        return f"{self.info['edge_ip']}:{svc['edge_port']}"

    def measure(self, svc):
        """One `wakeclient measure` from the client netns through the edge. Returns its line, with ok false if it broke."""
        try:
            p = self.exec("client", str(WAKECLIENT), "measure", "--addr", self.edge_addr(svc), timeout=MEASURE_TIMEOUT_SECONDS)
        except subprocess.TimeoutExpired as e:
            return {"ok": False, "error": str(e)}
        try:
            return json.loads(p.stdout)
        except ValueError:
            return {"ok": False, "error": f"rc={p.returncode}: {p.stderr}"}

    def wake_series(self, svc, count, before=None):
        """`count` measures of a wake, each one of a service confirmed ASLEEP, with `before` right before each measure.
        Returns the lines of wakeclient. If the service stops cooperating, the last line is the error and the series ends."""
        lines = []
        for _ in range(count):
            try:
                self.sleep_confirmed(svc["spec"]["name"])
                if before is not None:
                    before()
            except CommandError as e:
                lines.append({"ok": False, "error": str(e)})
                break
            lines.append(self.measure(svc))
        return lines

    def cgroup(self, svc):
        """Directory of the cgroup of a service's instance, as the host sees it."""
        instance = svc["instance"]
        return Path(self.info["scope_cgroup"]) / "dc" / instance["node"] / f"svc-{svc['spec']['name']}-{instance['generation']}"

    def control_plane_pid(self):
        """Host pid of andend, or None while there is none."""
        procs = (Path(self.info["scope_cgroup"]) / "dc" / "cp" / "cgroup.procs").read_text().split()
        return int(procs[0]) if procs else None

    def request(self, svc, path="/", timeout=30):
        """One HTTP request from the client netns through the edge. Returns the status, or None if it broke."""
        script = ("import http.client,sys\n"
                  "c=http.client.HTTPConnection(sys.argv[1],int(sys.argv[2]),timeout=10)\n"
                  "c.request('GET',sys.argv[3])\nprint(c.getresponse().status)\n")
        p = self.exec("client", "python3", "-I", "-c", script, self.info["edge_ip"], str(svc["edge_port"]), path, timeout=timeout)
        return int(p.stdout) if p.returncode == 0 and p.stdout.strip() else None

    def load(self, svc, seconds):
        return Load(self, svc["spec"]["name"], svc["edge_port"], seconds)
