#!/usr/bin/env python3
"""C21: `anden image verify` checks every blob, finds one corrupted byte and the image cannot be used after it.

alpine and the synthetic image are imported into a clean registry and verified. Then one byte in the middle of
the alpine layer blob is flipped, verify runs again and `anden image unpack` tries to bring the image into a
clean node store, which copies and verifies every blob (D10). That use must exit non-zero and name the blob.
`blobs_checked` and `mismatches` are those of the intact store; `after_corruption` holds the second verify.
"""
import json
import shutil
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c21"
STATE = lib.ROOT / ".run" / "dc-c21"
REGISTRY = STATE / "registry"
TIMEOUT_SECONDS = 600


def anden(*args):
    return subprocess.run([str(lib.ROOT / "bin" / "anden"), "image", *args, "--state", str(STATE)],
                          capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=TIMEOUT_SECONDS)


def force_rmtree(path):
    if path.exists():
        subprocess.run(["chmod", "-R", "u+rwX", str(path)], capture_output=True, stdin=subprocess.DEVNULL)
        shutil.rmtree(path)


def verify(log):
    proc = anden("verify", "--json")
    log.write(f"$ verify: exit {proc.returncode}\n{proc.stdout}{proc.stderr}")
    try:
        return proc.returncode, json.loads(proc.stdout)
    except ValueError:
        return proc.returncode, None


def main():
    # unpack writes whiteouts, so anden enters the id mapping on its own; without a user namespace there is none.
    lib.require_ns()
    lib.ensure_tools()
    subprocess.run(["make", "build"], cwd=lib.ROOT, stdin=subprocess.DEVNULL, check=True)
    force_rmtree(RAW)
    force_rmtree(STATE)
    RAW.mkdir(parents=True)

    images = {i["name"]: i for i in lib.locked_images()}
    synthetic = RAW / "synthetic-layout"
    subprocess.run([str(lib.ROOT / "bin" / "mkimage"), "synthetic", "--out", str(synthetic), "--tag", "latest"],
                   stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, check=True)
    layouts = {"alpine": f"{lib.cache_dir() / 'images' / 'alpine'}:{images['alpine']['tag']}",
               "synthetic": f"{synthetic}:latest"}

    report = {"blobs_checked": None, "mismatches": None, "corrupted_blob": None,
              "corrupted_use_rc": None, "corrupted_use_stderr": None, "errors": []}
    with open(RAW / "commands.log", "w") as log:
        digests = {}
        for name, layout in layouts.items():
            proc = anden("import", "--layout", layout, "--json")
            log.write(f"$ import {layout}: exit {proc.returncode}\n{proc.stdout}{proc.stderr}")
            if proc.returncode != 0:
                report["errors"].append(f"import {name} exited {proc.returncode}")
                continue
            digests[name] = json.loads(proc.stdout)["digest"]
        if "alpine" not in digests:
            lib.write_report("c21", report)
            return 1

        rc_before, before = verify(log)
        report["verify_rc"] = rc_before
        if before is not None:
            report["blobs_checked"] = before.get("blobs_checked")
            report["mismatches"] = before.get("mismatches")

        manifest = json.loads((REGISTRY / "blobs" / "sha256" / digests["alpine"].split(":", 1)[1]).read_text())
        layer = manifest["layers"][0]["digest"]
        path = REGISTRY / "blobs" / "sha256" / layer.split(":", 1)[1]
        data = bytearray(path.read_bytes())
        data[len(data) // 2] ^= 0xFF
        path.write_bytes(bytes(data))
        report["corrupted_blob"] = layer

        rc_after, after = verify(log)
        report["after_corruption"] = {"rc": rc_after, **(after or {})}

        use = anden("unpack", "--digest", digests["alpine"], "--dest", str(STATE / "node-store"))
        log.write(f"$ unpack: exit {use.returncode}\n{use.stdout}{use.stderr}")
        report["corrupted_use_rc"] = use.returncode
        report["corrupted_use_stderr"] = use.stderr

    force_rmtree(STATE)
    lib.write_report("c21", report)
    ok = (rc_before == 0 and report["mismatches"] == []
          and rc_after != 0 and after is not None and after.get("mismatches") == [layer]
          and report["corrupted_use_rc"] not in (0, None) and layer in (report["corrupted_use_stderr"] or "")
          and not report["errors"])
    print(f"checked {report['blobs_checked']} blobs, mismatches {report['mismatches']}, after corrupting one "
          f"{report['after_corruption'].get('mismatches')}, "
          f"use of the corrupted image exited {report['corrupted_use_rc']}", flush=True)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
