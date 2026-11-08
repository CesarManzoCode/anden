#!/usr/bin/env python3
"""C30: vault/DESIGN.md exists and has the four sections the contract asks for.

Only checks existence and sections, not content. `sections` lists the titles of the `## ` headings outside code blocks, in
the order of the file. With a path as argument it checks that file instead and writes no report: the Go test uses it on a
temporary file.
"""
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

DESIGN = lib.ROOT / "vault" / "DESIGN.md"
REQUIRED = ("Decisiones", "Alternativas descartadas", "Deuda técnica conocida", "Lo que no se demostró")
HEADING_RE = re.compile(r"^##\s+(.+?)\s*#*\s*$")
FENCE_RE = re.compile(r"^\s*```")


def sections(text):
    """The titles of the level 2 headings, outside fenced code blocks."""
    out, inside = [], False
    for line in text.splitlines():
        if FENCE_RE.match(line):
            inside = not inside
            continue
        if not inside and (match := HEADING_RE.match(line)):
            out.append(match.group(1))
    return out


def main(argv):
    design = Path(argv[0]) if argv else DESIGN
    exists = design.is_file()
    found = sections(design.read_text(encoding="utf-8")) if exists else []
    missing = [title for title in REQUIRED if title not in found]
    if not argv:
        lib.write_report("c30", {"path": str(design.relative_to(lib.ROOT)), "exists": exists,
                                 "sections": found, "missing": missing})
    if not exists:
        print(f"C30 failed: {design} does not exist", file=sys.stderr)
        return 1
    if missing:
        print(f"C30 failed: {design} is missing " + ", ".join(f"## {m}" for m in missing), file=sys.stderr)
        return 1
    print(f"C30 ok: {design} has the {len(REQUIRED)} required sections")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
