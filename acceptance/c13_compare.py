#!/usr/bin/env python3
"""C13: the two wake routes of Andén against runc, crun and a direct `criu restore`, on the same echosvc and machine.

Five rows of n = 100 (D12), each timed with wakeclient from the client netns of the datacenter:

- anden-relaunch and anden-restore: SleepService, a confirmed ASLEEP, then one `wakeclient measure` through the edge. The clock
  runs from before connect() to the last byte, with the proxy holding the connection while the service wakes.
- runc and crun (D20): the bundle joins a netns prepared beforehand, with a veth to the root netns of the datacenter. The clock
  runs from right before `create` to the last byte of the first answer; `start` is launched without waiting for it and killed
  at 2 s. NOTIFY_SOCKET is removed, as in C6.
- criu-restore: the same container, launched with anden-runtime and dumped and restored directly with the recipe of D03, without
  proxy or control plane. The clock runs from right before `criu restore` to the last byte of the first answer.

For the last three rows no proxy holds the connection, so wakeclient connects again every millisecond (`--retry`) and reports
`end_ns` on CLOCK_MONOTONIC, the clock of time.monotonic_ns: the sample is end_ns minus the start of create or restore. Their
rootfs is an overlay over a layer with only echosvc, without cgroup limits or seccomp; the Andén rows run with 1000 millicores and
128 MiB.

All of it runs inside the datacenter: the external rows in a delegated scope that joins the user, pid and root network namespaces
of the datacenter (nsenter) and has a mount namespace of its own. `conclusion` is the one section 7 allows. Fails if a row or a
percentile is missing, or if a row has fewer than n samples.
"""
import argparse
import ctypes
import json
import os
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c13"
SAMPLES = 100
CPU_MILLIS = 1000
MEMORY_MIB = 128
RUNC = "/usr/bin/runc"
CRUN = "/usr/bin/crun"
ANDEN_RUNTIME = lib.ROOT / "bin" / "anden-runtime"
ROWS = ("anden-relaunch", "anden-restore", "runc", "crun", "criu-restore")
# Section 7: relaunch is comparable to runc, crun and restore; restore is equivalent to a direct criu restore.
CONCLUSIONS = {
    "anden-relaunch": ("comparable", ["runc", "crun", "anden-restore"]),
    "anden-restore": ("equivalente", ["criu-restore"]),
    "runc": ("comparable", ["anden-relaunch"]),
    "crun": ("comparable", ["anden-relaunch"]),
    "criu-restore": ("equivalente", ["anden-restore"]),
}
HOST_VETH, PEER_VETH = "c13h", "c13c"
HOST_ADDR, SERVICE_ADDR, PREFIX = "10.90.0.1", "10.90.0.2", 24
SERVICE = f"{SERVICE_ADDR}:{lib.ECHO_PORT}"
CONTAINER = "c13"
RETRY = "10s"
REQUEST_TIMEOUT = "10s"
# wakeclient starts before the clock, so that its own start is not timed.
CLIENT_HEAD_START_SECONDS = 0.05
START_KILL_SECONDS = 2
STEP_TIMEOUT_SECONDS = 60
INNER_TIMEOUT_SECONDS = 3600
GONE_TIMEOUT_SECONDS = 5
PR_SET_CHILD_SUBREAPER = 36
# The nodes anden-runtime binds from the host /dev. criu takes those binds as external mounts: by mountpoint on dump, and on
# restore it binds the same host node back.
DEVICES = ("null", "zero", "full", "random", "urandom", "tty")
DUMP_DEVICES = " ".join(f'--external "mnt[/dev/{d}]:dev-{d}"' for d in DEVICES)
RESTORE_DEVICES = [x for d in DEVICES for x in ("--external", f"mnt[dev-{d}]:/dev/{d}")]
# anden-runtime opens this file of the rootfs as the stdout and stderr of the container, and its /dev/null as stdin, so that
# criu finds them in the mount namespace of the container (as the agent does).
STDIO_ANNOTATION = "anden.stdio"
# Runs before the inner script: keeps the client netns on a file while /proc still shows host pids, then mounts the proc of
# the pid namespace of the datacenter, which the dump view of D03 needs.
SETUP_SH = 'set -e\nmount --bind "/proc/$1/ns/net" "$2"\nmount -t proc proc /proc\nshift 2\nexec "$@"\n'
# The dump view of D03, in a mount namespace of its own: rootfs bound on itself, tmpfs on /tmp, proc, /dev/null, the images and a
# copy of criu with its libraries, then criu through the copied loader, chrooted in the rootfs.
DUMP_SH = """set -e
m=$1; img=$2; criu=$3; interp=$4; pid=$5; inode=$6; shift 6
mount --bind "$m" "$m"
mount -t tmpfs -o mode=755 tmpfs "$m/tmp"
mount -t proc proc "$m/proc"
mount --bind /dev/null "$m/dev/null"
mkdir "$m/tmp/img" "$m/tmp/c"
mount --bind "$img" "$m/tmp/img"
cp "$criu" "$m/tmp/c/criu"
while [ $# -gt 0 ]; do cp "$2" "$m/tmp/c/$1"; shift 2; done
exec chroot "$m" "/tmp/c/$interp" --library-path /tmp/c /tmp/c/criu dump --network-lock skip --unprivileged \\
  --manage-cgroups=ignore -t "$pid" -D /tmp/img --root / --external "net[$inode]:extnet" DEVICES -o dump.log -v2
""".replace("DEVICES", DUMP_DEVICES)
RESTORE_SH = 'set -e\nmount --bind "$1" "$1"\nshift\nexec "$@"\n'


def run(cmd, **kw):
    kw.setdefault("timeout", STEP_TIMEOUT_SECONDS)
    p = subprocess.run([str(c) for c in cmd], capture_output=True, text=True, stdin=subprocess.DEVNULL, **kw)
    if p.returncode != 0:
        raise lib.CommandError(f"{' '.join(str(c) for c in cmd)}: rc={p.returncode}: {p.stderr.strip()}")
    return p.stdout


def ip_batch(lines, netns=None):
    cmd = ["ip", "-batch", "-"] if netns is None else ["nsenter", f"--net={netns}", "--", "ip", "-batch", "-"]
    p = subprocess.run(cmd, input="\n".join(lines) + "\n", capture_output=True, text=True, timeout=STEP_TIMEOUT_SECONDS)
    if p.returncode != 0:
        raise lib.CommandError(f"ip -batch: {p.stderr.strip()}")


def parse_ldd(out):
    """Files the loader needs for criu as (name the loader looks for, path), and the name of the loader (as in the agent)."""
    libs, interp = [], None
    for line in out.splitlines():
        fields = line.split()
        if not fields:
            continue
        if "not found" in line:
            raise lib.CommandError(f"ldd: missing library: {line.strip()}")
        if len(fields) >= 3 and fields[1] == "=>":
            name, path = fields[0], fields[2]
        elif fields[0].startswith("/"):
            name, path = fields[0], fields[0]
        else:
            continue
        if not path.startswith("/"):
            continue
        if name.startswith("/"):
            interp = os.path.basename(name)
        libs.append((os.path.basename(name), path))
    if interp is None:
        raise lib.CommandError(f"ldd: no dynamic loader in {out!r}")
    return libs, interp


def config(rootfs, netns, uts=None, ipc=None):
    """The bundle of every external row: echosvc with its own pid and mount namespaces in the prepared netns. With uts and ipc,
    the container joins those too, as D03 asks of what criu dumps, and has its stdio opened inside the rootfs."""
    caps = ["CAP_KILL", "CAP_NET_BIND_SERVICE"]
    namespaces = [{"type": "pid"}, {"type": "mount"}, {"type": "network", "path": str(netns)}]
    namespaces += [{"type": "uts", "path": str(uts)}, {"type": "ipc", "path": str(ipc)}] if uts else [{"type": "uts"}, {"type": "ipc"}]
    cfg = {
        "ociVersion": "1.2.0",
        "process": {
            "terminal": False,
            "user": {"uid": 0, "gid": 0},
            "args": ["/echosvc"],
            "env": ["PATH=/usr/bin:/bin", f"PORT={lib.ECHO_PORT}"],
            "cwd": "/",
            "capabilities": {k: caps for k in ("bounding", "effective", "permitted")},
            "noNewPrivileges": True,
        },
        "root": {"path": str(rootfs), "readonly": False},
        "mounts": [
            {"destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid", "noexec", "nodev"]},
            {"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
             "options": ["nosuid", "strictatime", "mode=755", "size=65536k"]},
        ],
        "linux": {"namespaces": namespaces},
    }
    if uts:
        cfg["annotations"] = {STDIO_ANNOTATION: "/.anden/stdio.log"}
    return cfg


class Inner:
    """The external rows, inside the namespaces of the datacenter."""

    def __init__(self, work):
        self.work = work
        self.client_ns = work / "client.ns"
        self.net, self.uts, self.ipc = work / "net.ns", work / "uts.ns", work / "ipc.ns"
        self.merged = work / "merged"
        self.stdio = self.merged / ".anden" / "stdio.log"

    def prepare(self):
        w = self.work
        # Recursive, so that the bind of the client netns that SETUP_SH left on client.ns stays visible.
        run(["mount", "--rbind", w, w])
        for f in (self.net, self.uts, self.ipc):
            f.write_bytes(b"")
        run(["unshare", f"--net={self.net}", f"--uts={self.uts}", f"--ipc={self.ipc}", "true"])
        ip_batch([f"link add {HOST_VETH} type veth peer name {PEER_VETH}", f"link set {PEER_VETH} netns {self.net}",
                  f"addr add {HOST_ADDR}/{PREFIX} dev {HOST_VETH}", f"link set {HOST_VETH} up"])
        ip_batch(["link set lo up", f"link set {PEER_VETH} name eth0", "link set eth0 addrgenmode none",
                  f"addr add {SERVICE_ADDR}/{PREFIX} dev eth0", "link set eth0 up", f"route add default via {HOST_ADDR}"],
                 netns=self.net)
        lower = w / "lower"
        for d in ("proc", "dev", "tmp", ".anden"):
            (lower / d).mkdir(parents=True, exist_ok=True)
        (lower / "dev" / "null").write_bytes(b"")
        shutil.copy2(lib.ROOT / "bin" / "echosvc", lower / "echosvc")
        for d in ("upper", "ovl-work", "merged"):
            (w / d).mkdir(exist_ok=True)
        run(["mount", "-t", "overlay", "overlay", "-o",
             f"lowerdir={lower},upperdir={w / 'upper'},workdir={w / 'ovl-work'},userxattr", self.merged])
        for name, joined in (("plain", False), ("joined", True)):
            bundle = w / f"bundle-{name}"
            bundle.mkdir(exist_ok=True)
            cfg = config(self.merged, self.net, self.uts, self.ipc) if joined else config(self.merged, self.net)
            (bundle / "config.json").write_text(json.dumps(cfg, indent=2) + "\n")

    def cleanup(self):
        subprocess.run(["ip", "link", "del", HOST_VETH], capture_output=True, stdin=subprocess.DEVNULL)

    def client(self, log):
        """wakeclient in the client netns, retrying until the service answers."""
        cmd = ["nsenter", f"--net={self.client_ns}", "--", str(lib.WAKECLIENT), "measure", "--addr", SERVICE,
               "--retry", RETRY, "--timeout", REQUEST_TIMEOUT]
        return subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=log, text=True)

    def finish(self, client, start_ns):
        """The line of wakeclient with the sample: from start_ns to its last byte."""
        try:
            out, _ = client.communicate(timeout=STEP_TIMEOUT_SECONDS)
            line = json.loads(out)
        except (subprocess.TimeoutExpired, ValueError) as e:
            client.kill()
            client.wait()
            return {"ok": False, "error": f"wakeclient: {e}"}
        if line.get("ok") and line.get("end_ns", 0) > start_ns:
            line["sample_ms"] = (line["end_ns"] - start_ns) / 1e6
        else:
            line["ok"] = False
        return line

    def remove(self, cmd):
        subprocess.run(cmd + ["kill", CONTAINER, "KILL"], capture_output=True, stdin=subprocess.DEVNULL)
        subprocess.run(cmd + ["delete", "--force", CONTAINER], capture_output=True, stdin=subprocess.DEVNULL)
        reap()

    def runtime_row(self, name, cmd, log):
        """create and start of one runtime per sample, timed from before create (D20)."""
        lines = []
        bundle = self.work / "bundle-plain"
        for _ in range(SAMPLES):
            self.remove(cmd)
            client = self.client(log)
            time.sleep(CLIENT_HEAD_START_SECONDS)
            start_ns = time.monotonic_ns()
            with open(self.stdio, "ab") as stdio:
                create = subprocess.run(cmd + ["create", "--bundle", str(bundle), CONTAINER], stdin=subprocess.DEVNULL,
                                        stdout=stdio, stderr=stdio, timeout=STEP_TIMEOUT_SECONDS)
            start = None
            if create.returncode == 0:
                start = subprocess.Popen(cmd + ["start", CONTAINER], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                         stderr=log)
            else:
                client.kill()
            line = self.finish(client, start_ns)
            if create.returncode != 0:
                line = {"ok": False, "error": f"create exited {create.returncode}"}
            if start is not None:
                try:
                    start.wait(timeout=max(0.0, START_KILL_SECONDS - (time.monotonic_ns() - start_ns) / 1e9))
                except subprocess.TimeoutExpired:
                    start.kill()
                    start.wait()
            lines.append(line)
            self.remove(cmd)
        return lines

    def container_pid(self, cmd):
        return json.loads(run(cmd + ["state", CONTAINER]))["pid"]

    def criu_row(self, log):
        """Launch with anden-runtime, dump with the view of D03 and time the restore, once per sample."""
        cmd = [str(ANDEN_RUNTIME), "--root", str(self.work / "state-anden")]
        criu = shutil.which("criu")
        libs, interp = parse_ldd(run(["ldd", criu], env={**os.environ, "LC_ALL": "C"}))
        lib_args = [x for pair in libs for x in pair]
        inode = os.stat(self.net).st_ino
        images = self.work / "img"
        pidfile = self.work / "restore.pid"
        lines = []
        for _ in range(SAMPLES):
            pid = None
            try:
                self.remove(cmd)
                with open(self.stdio, "ab") as stdio:
                    subprocess.run(cmd + ["create", "--bundle", str(self.work / "bundle-joined"), CONTAINER],
                                   stdin=subprocess.DEVNULL, stdout=stdio, stderr=stdio, timeout=STEP_TIMEOUT_SECONDS, check=True)
                run(cmd + ["start", CONTAINER])
                ready = self.finish(self.client(log), 0)
                if not ready.get("ok"):
                    raise lib.CommandError(f"the launched container did not answer: {ready}")
                shutil.rmtree(images, ignore_errors=True)
                images.mkdir()
                run(["nsenter", f"--uts={self.uts}", f"--ipc={self.ipc}", "--", "unshare", "--mount", "--", "sh", "-c", DUMP_SH,
                     "sh", self.merged, images, criu, interp, self.container_pid(cmd), inode, *lib_args])
                self.remove(cmd)
                pidfile.unlink(missing_ok=True)
                net_fd = os.open(self.net, os.O_RDONLY)
                try:
                    client = self.client(log)
                    time.sleep(CLIENT_HEAD_START_SECONDS)
                    start_ns = time.monotonic_ns()
                    restore = subprocess.run(
                        ["nsenter", f"--uts={self.uts}", f"--ipc={self.ipc}", "--", "unshare", "--mount", "--", "sh", "-c",
                         RESTORE_SH, "sh", str(self.merged), criu, "restore", "--network-lock", "skip", "--unprivileged",
                         "--manage-cgroups=ignore", "-D", str(images), "--root", str(self.merged),
                         "--inherit-fd", f"fd[{net_fd}]:extnet", *RESTORE_DEVICES, "-d", "--pidfile", str(pidfile), "-o", "restore.log", "-v2"],
                        stdin=subprocess.DEVNULL, capture_output=True, text=True, pass_fds=(net_fd,),
                        timeout=STEP_TIMEOUT_SECONDS)
                finally:
                    os.close(net_fd)
                if restore.returncode != 0:
                    client.kill()
                    client.wait()
                    raise lib.CommandError(f"criu restore: rc={restore.returncode}: {restore.stderr.strip()}; see {images}/restore.log")
                pid = int(pidfile.read_text().strip())
                line = self.finish(client, start_ns)
            except (lib.CommandError, subprocess.SubprocessError, OSError, ValueError) as e:
                line = {"ok": False, "error": str(e)}
            finally:
                if pid is not None:
                    kill_tree(pid)
                self.remove(cmd)
            lines.append(line)
        return lines


def reap():
    """Collects the exited processes this subreaper inherited: container inits and restored trees."""
    while True:
        try:
            pid, _ = os.waitpid(-1, os.WNOHANG)
        except ChildProcessError:
            return
        if pid == 0:
            return


def kill_tree(pid):
    """SIGKILL to the init of a restored pid namespace, which ends all of it, and waits for it to go."""
    try:
        os.kill(pid, signal.SIGKILL)
    except ProcessLookupError:
        return
    deadline = time.monotonic() + GONE_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        try:
            if os.waitpid(pid, os.WNOHANG)[0] == pid:
                return
        except ChildProcessError:
            if not Path(f"/proc/{pid}").exists():
                return
        time.sleep(0.01)


def inner(work):
    os.environ.pop("NOTIFY_SOCKET", None)
    # The restored tree and the container inits outlive their parents; as a subreaper this process collects them.
    ctypes.CDLL(None, use_errno=True).prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
    state = Inner(work)
    rows, errors = {}, {}
    with open(work / "inner.log", "a") as log:
        try:
            state.prepare()
            for name, cmd in (("runc", [RUNC, "--root", str(work / "state-runc")]),
                              ("crun", [CRUN, "--root", str(work / "state-crun")])):
                rows[name] = state.runtime_row(name, cmd, log)
            rows["criu-restore"] = state.criu_row(log)
        except (lib.CommandError, subprocess.SubprocessError, OSError) as e:
            errors["inner"] = str(e)
        finally:
            state.cleanup()
    (work / "external.json").write_text(json.dumps({"rows": rows, "errors": errors}) + "\n")
    return 0


def external_rows(dc, work):
    """Runs the inner part in a delegated scope, inside the user, pid and root network namespaces of the datacenter."""
    (work / "client.ns").write_bytes(b"")
    ns = dc.info["ns"]
    cmd = ["systemd-run", "--user", "--scope", "-q", "-p", "Delegate=yes", "--",
           "nsenter", "--preserve-credentials", "-U", "-n", "-p", "-t", str(ns["root"]), "--",
           "unshare", "--mount", "--", "sh", "-c", SETUP_SH, "sh", str(ns["client"]), str(work / "client.ns"),
           sys.executable, "-I", str(Path(__file__).resolve()), "--inner", str(work)]
    proc = subprocess.run(cmd, cwd=lib.ROOT, stdin=subprocess.DEVNULL, capture_output=True, text=True,
                          timeout=INNER_TIMEOUT_SECONDS)
    (RAW / "inner.out").write_text(proc.stdout + proc.stderr)
    try:
        result = json.loads((work / "external.json").read_text())
    except (OSError, ValueError):
        return {}, {"inner": f"exited {proc.returncode} without a result"}
    return result["rows"], result["errors"]


def anden_row(dc, digest, route):
    svc = dc.create_echo(route, digest, cpu_millis=CPU_MILLIS, memory_mib=MEMORY_MIB, extra=["--wake-route", route])
    lines = dc.wake_series(svc, SAMPLES)
    for line in lines:
        if line.get("ok"):
            line["sample_ms"] = line["client_ms"]
    return lines


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inner", type=Path)
    args = parser.parse_args()
    if args.inner:
        return inner(args.inner)

    lib.require_ns()
    missing = [t for t in (RUNC, CRUN) if not os.access(t, os.X_OK)] + ([] if shutil.which("criu") else ["criu"])
    if missing:
        print(f"missing: {missing}", flush=True)
        lib.write_report("c13", {"error": f"missing: {missing}"})
        return 1
    # The kernel leaves the work directory of the overlay with mode 000; it is ours, so it is opened first.
    subprocess.run(["chmod", "-R", "u+rwX", str(RAW)], capture_output=True, stdin=subprocess.DEVNULL)
    shutil.rmtree(RAW, ignore_errors=True)
    work = RAW / "work"
    work.mkdir(parents=True)
    lines, errors = {}, {}
    with lib.Datacenter("c13") as dc:
        digest = dc.import_binary("echosvc", RAW)
        for route in ("relaunch", "restore"):
            lines[f"anden-{route}"] = anden_row(dc, digest, route)
        external, errors = external_rows(dc, work)
        lines.update(external)

    rows = []
    for route in ROWS:
        row_lines = lines.get(route)
        if row_lines is None:
            continue
        (RAW / f"{route}.log").write_text("".join(json.dumps(l) + "\n" for l in row_lines))
        samples = [l["sample_ms"] for l in row_lines if l.get("ok") and "sample_ms" in l]
        conclusion, against = CONCLUSIONS[route]
        rows.append({"route": route, **lib.percentiles(samples), "n": len(samples), "conclusion": conclusion,
                     "against": against, "failures": len(row_lines) - len(samples)})
    lib.write_report("c13", {"image": {"name": "echosvc", "digest": digest}, "rows": rows, "samples_per_row": SAMPLES,
                             "errors": errors})
    for row in rows:
        print(f"{row['route']:15} n={row['n']:3} p50={row['p50']} p90={row['p90']} p99={row['p99']} max={row['max']} "
              f"{row['conclusion']}", flush=True)
    present = {r["route"] for r in rows}
    bad = [r for r in ROWS if r not in present]
    bad += [r["route"] for r in rows if r["n"] < SAMPLES or any(r[k] is None for k in ("p50", "p90", "p99", "max"))]
    if bad or errors:
        print(f"C13 failed: rows {bad}, errors {errors}", file=sys.stderr)
        return 1
    print("C13 ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
