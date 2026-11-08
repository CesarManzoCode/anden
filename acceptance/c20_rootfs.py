#!/usr/bin/env python3
"""C20: the rootfs Andén mounts equals the one `umoci unpack --rootless` writes, for alpine, nginx and the
synthetic image of mkimage.

umoci runs as the user, outside any namespace, as D10 fixes. Andén imports each image and mounts it with
`anden image mount` inside the D09 namespace prefix, where it is already root and the overlay stays mounted
while the tree is walked. Both trees are walked there, with the same function, and written as sorted
`path\ttype\tmode\tsha256` lines. The hash is of the content for a file and of the target for a symlink, and
`-` for anything else. Owners and times are left out (D10). So is the root directory itself: its mode comes
from the store directory in Andén and from umoci's own mkdir.

Entries named `.wh.*` are left out of both trees. umoci removes a whiteout when it applies it and an overlay
never shows one, so none is expected. If a residual marker did appear on either side, it would be bookkeeping
of the layer format and not a file of the image, and comparing it would measure the tool and not the rootfs.
"""
import argparse
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c20"
STATE = lib.ROOT / ".run" / "dc-c20"
UMOCI = "umoci"
TIMEOUT_SECONDS = 1800
KINDS = [(stat.S_ISREG, "f"), (stat.S_ISDIR, "d"), (stat.S_ISLNK, "l"), (stat.S_ISCHR, "c"),
         (stat.S_ISBLK, "b"), (stat.S_ISFIFO, "p"), (stat.S_ISSOCK, "s")]


def kind(mode):
    for test, name in KINDS:
        if test(mode):
            return name
    return "?"


def digest(path, st):
    if stat.S_ISREG(st.st_mode):
        h = hashlib.sha256()
        with open(path, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                h.update(chunk)
        return h.hexdigest()
    if stat.S_ISLNK(st.st_mode):
        return hashlib.sha256(os.fsencode(os.readlink(path))).hexdigest()
    return "-"


def walk(root):
    rows = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if not d.startswith(".wh.")]
        for name in dirnames + filenames:
            if name.startswith(".wh."):
                continue
            path = os.path.join(dirpath, name)
            st = os.lstat(path)
            rel = "/" + os.path.relpath(path, root)
            rows.append(f"{rel}\t{kind(st.st_mode)}\t{stat.S_IMODE(st.st_mode):04o}\t{digest(path, st)}")
    return sorted(rows)


def write_tsv(path, rows):
    with open(path, "w", encoding="utf-8", errors="surrogateescape") as f:
        f.writelines(row + "\n" for row in rows)


def force_rmtree(path):
    if path.exists():
        subprocess.run(["chmod", "-R", "u+rwX", str(path)], capture_output=True, stdin=subprocess.DEVNULL)
        shutil.rmtree(path)


def inner(work):
    """Runs inside the namespace prefix: import, mount and walk with Andén, then walk umoci's rootfs."""
    plan = json.loads((work / "plan.json").read_text())
    anden = str(lib.ROOT / "bin" / "anden")
    result = {"digests": {}, "errors": {}}
    for name, layout in plan.items():
        try:
            imp = subprocess.run([anden, "image", "import", "--state", str(STATE), "--layout", layout, "--json"],
                                 capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=TIMEOUT_SECONDS)
            if imp.returncode != 0:
                result["errors"][name] = f"import exited {imp.returncode}: {imp.stderr.strip()}"
                continue
            d = json.loads(imp.stdout)["digest"]
            result["digests"][name] = d
            merged = work / name / "merged"
            mnt = subprocess.run([anden, "image", "mount", "--state", str(STATE), "--digest", d, "--dest", str(merged)],
                                 capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=TIMEOUT_SECONDS)
            if mnt.returncode != 0:
                result["errors"][name] = f"mount exited {mnt.returncode}: {mnt.stderr.strip()}"
                continue
            write_tsv(RAW / f"{name}.anden.tsv", walk(merged))
            write_tsv(RAW / f"{name}.umoci.tsv", walk(work / name / "umoci" / "rootfs"))
            subprocess.run(["umount", str(merged)], capture_output=True, stdin=subprocess.DEVNULL)
        except (OSError, ValueError, KeyError, subprocess.TimeoutExpired) as e:
            result["errors"][name] = f"{type(e).__name__}: {e}"
    (work / "inner.json").write_text(json.dumps(result) + "\n")
    return 0


def read_rows(path):
    try:
        return path.read_text(encoding="utf-8", errors="surrogateescape").splitlines()
    except OSError:
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inner", type=Path)
    args = parser.parse_args()
    if args.inner:
        return inner(args.inner)

    lib.require_ns()
    if shutil.which(UMOCI) is None:
        print(f"{UMOCI} is missing", flush=True)
        lib.write_report("c20", {"error": f"{UMOCI} is missing"})
        return 1
    lib.ensure_tools()
    subprocess.run(["make", "build"], cwd=lib.ROOT, stdin=subprocess.DEVNULL, check=True)

    force_rmtree(RAW)
    force_rmtree(STATE)
    work = RAW / "work"
    work.mkdir(parents=True)
    images = {i["name"]: i for i in lib.locked_images()}
    plan = {name: f"{lib.cache_dir() / 'images' / name}:{images[name]['tag']}" for name in ("alpine", "nginx")}
    synthetic = work / "synthetic-layout"
    subprocess.run([str(lib.ROOT / "bin" / "mkimage"), "synthetic", "--out", str(synthetic), "--tag", "latest"],
                   stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, check=True)
    plan["synthetic"] = f"{synthetic}:latest"
    (work / "plan.json").write_text(json.dumps(plan) + "\n")

    errors = {}
    for name, layout in plan.items():
        (work / name).mkdir()
        proc = subprocess.run([UMOCI, "unpack", "--rootless", "--image", layout, str(work / name / "umoci")],
                              capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=TIMEOUT_SECONDS)
        (RAW / f"{name}.umoci.log").write_text(proc.stdout + proc.stderr)
        if proc.returncode != 0:
            errors[name] = f"umoci exited {proc.returncode}"

    inner_cmd = lib.ns_wrap([sys.executable, "-I", str(Path(__file__).resolve()), "--inner", str(work)])
    proc = subprocess.run(inner_cmd, cwd=lib.ROOT, stdin=subprocess.DEVNULL, capture_output=True, text=True,
                          timeout=len(plan) * 2 * TIMEOUT_SECONDS)
    (RAW / "inner.log").write_text(proc.stdout + proc.stderr)
    try:
        result = json.loads((work / "inner.json").read_text())
    except (OSError, ValueError):
        result = {"digests": {}, "errors": {"inner": f"exited {proc.returncode} without a result"}}
    errors.update(result["errors"])

    equal, differences = {}, {}
    for name in plan:
        anden, umoci = read_rows(RAW / f"{name}.anden.tsv"), read_rows(RAW / f"{name}.umoci.tsv")
        equal[name] = anden is not None and anden == umoci and len(anden) > 0
        if anden is not None and umoci is not None:
            differences[name] = sorted(set(anden) ^ set(umoci))[:50]
    force_rmtree(work)
    force_rmtree(STATE)

    lib.write_report("c20", {"images": list(plan), "digests": result["digests"], "equal": equal,
                             "differences": differences, "errors": errors})
    print(f"equal: {equal}", flush=True)
    if errors:
        print(f"errors: {errors}", flush=True)
    return 0 if all(equal.values()) and not errors else 1


if __name__ == "__main__":
    sys.exit(main())
