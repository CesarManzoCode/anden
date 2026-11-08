#!/usr/bin/env python3
"""Fills the cache: runtime-tools built at a pinned commit and the images of images.lock. Idempotent."""
import argparse
import os
import shutil
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RUNTIME_TOOLS_URL = "https://github.com/opencontainers/runtime-tools"


def head(repo):
    return subprocess.run(["git", "-C", str(repo), "rev-parse", "HEAD"], capture_output=True, text=True, check=True).stdout.strip()


def checkout(repo, commit):
    if not (repo / ".git").is_dir():
        partial = repo.with_name(repo.name + ".tmp")
        shutil.rmtree(partial, ignore_errors=True)
        subprocess.run(["git", "clone", RUNTIME_TOOLS_URL, str(partial)], check=True)
        partial.rename(repo)
    if head(repo) != commit:
        known = subprocess.run(["git", "-C", str(repo), "cat-file", "-e", f"{commit}^{{commit}}"], capture_output=True)
        if known.returncode != 0:
            subprocess.run(["git", "-C", str(repo), "fetch", "origin", commit], check=True)
        subprocess.run(["git", "-C", str(repo), "checkout", "--detach", commit], check=True)
    if head(repo) != commit:
        sys.exit(f"{repo} is not at {commit}")


def validation_tests(repo):
    """The list the runtime-tools Makefile computes: every .go under validation/ whose path lacks "util", as .t."""
    tests = []
    for path in sorted((repo / "validation").rglob("*.go")):
        rel = f"./{path.relative_to(repo).as_posix()}"
        if "util" not in rel:
            tests.append(rel[:-len(".go")] + ".t")
    return tests


def fetch_runtime_tools(cache, commit):
    repo = cache / "runtime-tools"
    stamp = cache / "runtime-tools.built"
    if stamp.is_file() and stamp.read_text().strip() == commit and (repo / ".git").is_dir() and head(repo) == commit:
        return
    checkout(repo, commit)
    tests = validation_tests(repo)
    if not tests:
        sys.exit(f"no validation tests under {repo}")
    # This runs under `make tools`: its flags and variables must not reach the inner make.
    env = {k: v for k, v in os.environ.items() if k not in ("MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES", "MAKELEVEL")}
    subprocess.run(["make", "runtimetest", "validation-executables", "VALIDATION_TESTS=" + " ".join(tests)],
                   cwd=repo, env=env, check=True)
    stamp.write_text(commit + "\n")


def fetch_image(images, image):
    layout = images / image["name"]
    stamp = images / f"{image['name']}.digest"
    if stamp.is_file() and stamp.read_text().strip() == image["digest"] and (layout / "index.json").is_file():
        return
    partial = images / f"{image['name']}.tmp"
    shutil.rmtree(partial, ignore_errors=True)
    # skopeo rejects a reference with both tag and digest, so the tag only names the copy.
    subprocess.run(["skopeo", "copy", "--override-os", "linux", "--override-arch", "amd64",
                    f"docker://{image['repository']}@{image['digest']}", f"oci:{partial}:{image['tag']}"], check=True)
    shutil.rmtree(layout, ignore_errors=True)
    partial.rename(layout)
    stamp.write_text(image["digest"] + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cache", required=True, type=Path)
    parser.add_argument("--runtime-tools-commit", required=True)
    args = parser.parse_args()
    cache = args.cache.resolve()
    images = cache / "images"
    images.mkdir(parents=True, exist_ok=True)
    fetch_runtime_tools(cache, args.runtime_tools_commit)
    for image in lib.locked_images():
        fetch_image(images, image)
    return 0


if __name__ == "__main__":
    sys.exit(main())
