#!/usr/bin/env python3
"""C22: two images that share layers keep one copy of each layer in the store.

nginx and nginx-perl are imported into a clean registry and `du -sb` measures blobs/. Copies are counted by
hashing every file under blobs/, not by file name, so a layer stored under another name would still count.
Importing writes no whiteouts and mounts nothing, so this criterion needs no namespace.
"""
import hashlib
import json
import shutil
import subprocess
import sys
from collections import Counter
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c22"
STATE = lib.ROOT / ".run" / "dc-c22"
BLOBS = STATE / "registry" / "blobs"
NAMES = ("nginx", "nginx-perl")
EXPECTED_SHARED = 7  # D25
TIMEOUT_SECONDS = 600


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return "sha256:" + h.hexdigest()


def main():
    lib.ensure_tools()
    subprocess.run(["make", "build"], cwd=lib.ROOT, stdin=subprocess.DEVNULL, check=True)
    shutil.rmtree(RAW, ignore_errors=True)
    shutil.rmtree(STATE, ignore_errors=True)
    RAW.mkdir(parents=True)

    images = {i["name"]: i for i in lib.locked_images()}
    layers = {}
    errors = []
    with open(RAW / "commands.log", "w") as log:
        for name in NAMES:
            layout = f"{lib.cache_dir() / 'images' / name}:{images[name]['tag']}"
            proc = subprocess.run([str(lib.ROOT / "bin" / "anden"), "image", "import", "--state", str(STATE),
                                   "--layout", layout, "--json"],
                                  capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=TIMEOUT_SECONDS)
            log.write(f"$ import {layout}: exit {proc.returncode}\n{proc.stdout}{proc.stderr}")
            if proc.returncode != 0:
                errors.append(f"import {name} exited {proc.returncode}")
                continue
            digest = json.loads(proc.stdout)["digest"]
            manifest = json.loads((BLOBS / "sha256" / digest.split(":", 1)[1]).read_text())
            layers[name] = {layer["digest"]: layer["size"] for layer in manifest["layers"]}
    if errors:
        shutil.rmtree(STATE, ignore_errors=True)
        lib.write_report("c22", {"errors": errors})
        print(f"errors: {errors}", flush=True)
        return 1

    du = subprocess.run(["du", "-sb", str(BLOBS)], capture_output=True, text=True, stdin=subprocess.DEVNULL, check=True)
    (RAW / "du.txt").write_text(du.stdout)
    du_bytes = int(du.stdout.split()[0])

    unique = {**layers[NAMES[0]], **layers[NAMES[1]]}
    unique_layers_bytes = sum(unique.values())
    total_layers_bytes = sum(layers[NAMES[0]].values()) + sum(layers[NAMES[1]].values())
    shared = sorted(set(layers[NAMES[0]]) & set(layers[NAMES[1]]))
    stored = Counter(sha256_file(p) for p in BLOBS.rglob("*") if p.is_file())
    copies = {digest: stored[digest] for digest in sorted(unique)}
    shutil.rmtree(STATE, ignore_errors=True)

    lib.write_report("c22", {"du_bytes": du_bytes, "unique_layers_bytes": unique_layers_bytes,
                             "total_layers_bytes": total_layers_bytes, "shared_layers": shared, "copies": copies})
    print(f"du {du_bytes} bytes, unique layers {unique_layers_bytes}, without sharing {total_layers_bytes}, "
          f"{len(shared)} shared layers", flush=True)
    ok = (len(shared) >= EXPECTED_SHARED and all(n == 1 for n in copies.values())
          and unique_layers_bytes <= du_bytes < total_layers_bytes)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
