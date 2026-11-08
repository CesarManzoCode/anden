#!/usr/bin/env python3
"""C28: from a clean clone, one command builds and runs every check and criterion without sudo, and exits with 0.

`git clone` of the repository to .run/acceptance/c28/clone: only what is committed at HEAD, nothing that is not versioned.
In the clone it runs `make check && make accept` with ANDEN_ACCEPT_SKIP=c28, so that it does not clone itself again, and
ANDEN_CACHE of this repository, so that it reuses the images and runtime-tools already fetched. The whole output goes to
c28/build.log. Passes if the command exits with 0.
"""
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

COMMAND = "make check && make accept"
RAW = lib.REPORT_DIR / "c28"
CLONE = RAW / "clone"
TIMEOUT_SECONDS = 4 * 3600


def git(*args, cwd=lib.ROOT):
    return subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True, stdin=subprocess.DEVNULL, check=True).stdout.strip()


def remove(path):
    """rmtree, or `rm -rf` as root of a user namespace with the subordinate ids mapped when a datacenter left files
    rmtree cannot remove: overlayfs leaves its work directory with mode 000, and an instance writes with mapped ids."""
    try:
        shutil.rmtree(path)
    except OSError:
        subprocess.run(["unshare", "-U", "--map-root-user", "--map-auto", "rm", "-rf", "--", str(path)],
                       stdin=subprocess.DEVNULL, check=True)


def main():
    RAW.mkdir(parents=True, exist_ok=True)
    if CLONE.exists():
        # A datacenter of the clone that broke can leave files only its user namespace could remove; down first.
        anden = CLONE / "bin" / "anden"
        for state in (CLONE / ".run").glob("dc-*") if anden.exists() else ():
            subprocess.run([str(anden), "down", "--state", str(state)], capture_output=True,
                           stdin=subprocess.DEVNULL, timeout=120)
        remove(CLONE)
    git("clone", str(lib.ROOT), str(CLONE))
    head = git("rev-parse", "HEAD", cwd=CLONE)
    env = {**os.environ, "ANDEN_ACCEPT_SKIP": "c28", "ANDEN_CACHE": str(lib.cache_dir())}
    log = RAW / "build.log"
    started = time.monotonic()
    with log.open("w") as out:
        try:
            rc = subprocess.run(["bash", "-c", COMMAND], cwd=CLONE, env=env, stdin=subprocess.DEVNULL, stdout=out,
                                stderr=subprocess.STDOUT, timeout=TIMEOUT_SECONDS).returncode
        except subprocess.TimeoutExpired:
            out.write(f"\nc28: timed out after {TIMEOUT_SECONDS} s\n")
            rc = None
    seconds = round(time.monotonic() - started, 1)
    lib.write_report("c28", {"command": COMMAND, "rc": rc, "head": head, "source_head": git("rev-parse", "HEAD"),
                             "anden_accept_skip": env["ANDEN_ACCEPT_SKIP"], "anden_cache": env["ANDEN_CACHE"],
                             "seconds": seconds})
    if rc != 0:
        print(f"C28 failed: `{COMMAND}` in the clone exited with {rc}; see {log}", file=sys.stderr)
        return 1
    print(f"C28 ok: `{COMMAND}` in a clean clone of {head} exited with 0 in {seconds} s")
    return 0


if __name__ == "__main__":
    sys.exit(main())
