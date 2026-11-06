#!/usr/bin/env python3
"""C6: runs bin/probe in one bundle with anden-runtime and with /usr/bin/runc and compares both reports.

The config.json is the same for both: own pid, network, UTS, IPC and mount namespaces, no uidMappings, and a
seccomp filter that blocks mount and swapon. Each runtime runs under the D09 namespace prefix with the
container's stdout redirected to a file. runc follows D20: create, then start without waiting for it to return.
NOTIFY_SOCKET is removed from the runtimes' environment: runc would add a mount and an env var to the container and
bind a socket under its --root, and that path does not fit in sun_path here, so `runc start` fails.
"""
import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c06"
RUNC = "/usr/bin/runc"
TIMEOUT_SECONDS = 60
RUNC_START_KILL_SECONDS = 2
EXPECTED = {"pid1": True, "hostname_own": True, "host_procs_visible": False,
            "blocked_syscalls": {"mount": "EPERM", "swapon": "EPERM"}}
CAPS = ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SYS_ADMIN"]


def config(host_pid, host_comm):
    # CAP_SYS_ADMIN stays so that, without the filter, mount would fail with ENOENT and not with EPERM.
    caps = {k: CAPS for k in ("bounding", "effective", "permitted")}
    return {
        "ociVersion": "1.2.0",
        "process": {
            "terminal": False,
            "user": {"uid": 0, "gid": 0},
            "args": ["/probe"],
            "env": ["PATH=/usr/bin:/bin", f"PROBE_HOST_HOSTNAME={socket.gethostname()}",
                    f"PROBE_HOST_PID={host_pid}", f"PROBE_HOST_COMM={host_comm}"],
            "cwd": "/",
            "capabilities": caps,
            "noNewPrivileges": True,
        },
        "root": {"path": "rootfs", "readonly": False},
        "hostname": "anden-c06",
        "mounts": [
            {"destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid", "noexec", "nodev"]},
            {"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
             "options": ["nosuid", "strictatime", "mode=755", "size=65536k"]},
        ],
        "linux": {
            "namespaces": [{"type": t} for t in ("pid", "network", "uts", "ipc", "mount")],
            "seccomp": {
                "defaultAction": "SCMP_ACT_ALLOW",
                "architectures": ["SCMP_ARCH_X86_64"],
                "syscalls": [{"names": ["mount", "swapon"], "action": "SCMP_ACT_ERRNO"}],
            },
        },
    }


def runtime_state(cmd, container):
    proc = subprocess.run(cmd + ["state", container], capture_output=True, text=True, stdin=subprocess.DEVNULL)
    if proc.returncode != 0:
        return None
    try:
        return json.loads(proc.stdout).get("status")
    except ValueError:
        return None


def run_one(name, cmd, bundle, out_path, log):
    """Creates and starts the container with its stdout in out_path and waits until it stops."""
    container = f"anden-c06-{name}"
    subprocess.run(cmd + ["delete", "--force", container], capture_output=True, stdin=subprocess.DEVNULL)
    with open(out_path, "wb") as out, open(log, "ab") as err:
        create = subprocess.run(cmd + ["create", "--bundle", str(bundle), container], stdin=subprocess.DEVNULL,
                                stdout=out, stderr=err, timeout=TIMEOUT_SECONDS)
    if create.returncode != 0:
        return f"create exited {create.returncode}"
    with open(log, "ab") as err:
        if name == "runc":
            start = subprocess.Popen(cmd + ["start", container], stdin=subprocess.DEVNULL,
                                     stdout=subprocess.DEVNULL, stderr=err)
            try:
                start.wait(timeout=RUNC_START_KILL_SECONDS)
            except subprocess.TimeoutExpired:
                start.kill()
                start.wait()
        else:
            start = subprocess.run(cmd + ["start", container], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                   stderr=err, timeout=TIMEOUT_SECONDS)
            if start.returncode != 0:
                return f"start exited {start.returncode}"
    deadline = time.monotonic() + TIMEOUT_SECONDS
    while runtime_state(cmd, container) not in ("stopped", None):
        if time.monotonic() > deadline:
            return "container did not stop"
        time.sleep(0.05)
    subprocess.run(cmd + ["delete", "--force", container], capture_output=True, stdin=subprocess.DEVNULL)
    return None


def inner(work):
    """Runs inside the namespace prefix: a sleep is the host process the container must not see."""
    os.environ.pop("NOTIFY_SOCKET", None)
    host = subprocess.Popen(["sleep", "600"], stdin=subprocess.DEVNULL)
    errors = {}
    try:
        comm = Path(f"/proc/{host.pid}/comm").read_text().strip()
        bundle = work / "bundle"
        (bundle / "config.json").write_text(json.dumps(config(host.pid, comm), indent=2) + "\n")
        runtimes = {
            "anden": [str((lib.ROOT / "bin" / "anden-runtime").resolve()), "--root", str(work / "state-anden")],
            "runc": [RUNC, "--root", str(work / "state-runc")],
        }
        for name, cmd in runtimes.items():
            try:
                error = run_one(name, cmd, bundle, RAW / f"{name}.json", RAW / f"{name}.log")
            except subprocess.TimeoutExpired as e:
                error = f"timeout: {e.cmd}"
            if error:
                errors[name] = error
    finally:
        host.kill()
        host.wait()
    (work / "errors.json").write_text(json.dumps(errors) + "\n")
    return 0


def read_report(path):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inner", type=Path)
    args = parser.parse_args()
    if args.inner:
        return inner(args.inner)

    lib.require_ns()
    if not os.access(RUNC, os.X_OK):
        print(f"{RUNC} is missing", flush=True)
        lib.write_report("c06", {"error": f"{RUNC} is missing"})
        return 1
    subprocess.run(["make", "build"], cwd=lib.ROOT, stdin=subprocess.DEVNULL, check=True)

    shutil.rmtree(RAW, ignore_errors=True)
    work = RAW / "work"
    rootfs = work / "bundle" / "rootfs"
    for d in ("proc", "dev", "tmp"):
        (rootfs / d).mkdir(parents=True, exist_ok=True)
    shutil.copy2(lib.ROOT / "bin" / "probe", rootfs / "probe")

    inner_cmd = lib.ns_wrap([sys.executable, "-I", str(Path(__file__).resolve()), "--inner", str(work)])
    proc = subprocess.run(inner_cmd, cwd=lib.ROOT, stdin=subprocess.DEVNULL, capture_output=True, text=True,
                          timeout=4 * TIMEOUT_SECONDS)
    (RAW / "inner.log").write_text(proc.stdout + proc.stderr)
    errors = read_report(work / "errors.json")
    if errors is None:
        errors = {"inner": f"exited {proc.returncode} without a result"}

    anden, runc = read_report(RAW / "anden.json"), read_report(RAW / "runc.json")
    equal = anden is not None and anden == runc
    isolated = anden == EXPECTED
    lib.write_report("c06", {"anden": anden, "runc": runc, "expected": EXPECTED, "equal": equal,
                             "isolated": isolated, "errors": errors})
    print(f"anden: {anden}\nrunc:  {runc}\nequal: {equal}, isolated as expected: {isolated}", flush=True)
    if errors:
        print(f"errors: {errors}", flush=True)
    return 0 if equal and isolated and not errors else 1


if __name__ == "__main__":
    sys.exit(main())
