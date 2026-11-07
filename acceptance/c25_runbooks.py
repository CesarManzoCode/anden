#!/usr/bin/env python3
"""C25: every alert of ops/alerts.yml has a runbook, and every runbook has signals, diagnosis with real commands and mitigation.

Reads ops/alerts.yml line by line, in the form it has (`- alert: NAME`, then `runbook: PATH` in its annotations), because the
standard library has no YAML. For each alert: annotations.runbook is ops/runbooks/<alert>.md and exists; it has the headings
`## Señales`, `## Diagnóstico` and `## Mitigación`; its code blocks have at least one `anden` command, and every one of them is
a command of the usage that bin/anden prints. Passes with at least 2 alerts, AndenNodeDown and AndenWakeOverBudget among them,
and no problem.
"""
import re
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

ALERTS = lib.ROOT / "ops" / "alerts.yml"
REQUIRED = ("AndenNodeDown", "AndenWakeOverBudget")
HEADINGS = ("## Señales", "## Diagnóstico", "## Mitigación")
ALERT_RE = re.compile(r'^\s*-\s*alert:\s*"?([A-Za-z0-9_]+)"?\s*$')
RUNBOOK_RE = re.compile(r'^\s*runbook:\s*"?([^"\s]+)"?\s*$')
FENCE_RE = re.compile(r"^\s*```")


def alerts():
    """[(alert, runbook or None)] in the order of the file."""
    out = []
    for line in ALERTS.read_text().splitlines():
        if match := ALERT_RE.match(line):
            out.append([match.group(1), None])
        elif (match := RUNBOOK_RE.match(line)) and out:
            out[-1][1] = match.group(1)
    return [tuple(a) for a in out]


def usage_commands():
    """The commands of the usage of bin/anden: their words up to the first flag, with `a|b` expanded."""
    p = subprocess.run([str(lib.ANDEN)], capture_output=True, text=True, stdin=subprocess.DEVNULL)
    commands = set()
    for line in p.stderr.splitlines():
        words = line.split()
        if not words or words[0] != "anden":
            continue
        heads = [[]]
        for word in words[1:]:
            if word.startswith("-") or word.startswith("[") or not re.fullmatch(r"[a-z|_]+", word):
                break
            heads = [h + [alt] for h in heads for alt in word.split("|")]
        commands.update(" ".join(h) for h in heads if h)
    return commands


def runbook_commands(text):
    """The words up to the first flag of each `anden` line inside code blocks."""
    out, inside = [], False
    for line in text.splitlines():
        if FENCE_RE.match(line):
            inside = not inside
            continue
        words = line.split()
        if inside and words and words[0] == "anden":
            head = []
            for word in words[1:]:
                if word.startswith("-") or not re.fullmatch(r"[a-z_]+", word):
                    break
                head.append(word)
            out.append(" ".join(head))
    return out


def main():
    lib.build()
    usage = usage_commands()
    found = alerts()
    problems = []
    if not usage:
        problems.append("bin/anden printed no usage")
    if len(found) < 2:
        problems.append(f"{len(found)} alerts in ops/alerts.yml, want at least 2")
    for name in REQUIRED:
        if name not in [a for a, _ in found]:
            problems.append(f"{name} is not in ops/alerts.yml")
    checked = []
    for alert, runbook in found:
        entry = {"alert": alert, "runbook": runbook}
        checked.append(entry)
        if runbook != f"ops/runbooks/{alert}.md":
            problems.append(f"{alert}: runbook {runbook!r}, want ops/runbooks/{alert}.md")
            continue
        path = lib.ROOT / runbook
        if not path.is_file():
            problems.append(f"{alert}: {runbook} does not exist")
            continue
        text = path.read_text()
        lines = text.splitlines()
        entry["missing_headings"] = [h for h in HEADINGS if h not in lines]
        entry["commands"] = runbook_commands(text)
        entry["unknown_commands"] = [c for c in entry["commands"] if c not in usage]
        if entry["missing_headings"]:
            problems.append(f"{alert}: missing {entry['missing_headings']}")
        if not entry["commands"]:
            problems.append(f"{alert}: no anden command in a code block")
        if entry["unknown_commands"]:
            problems.append(f"{alert}: commands not in the usage of anden: {entry['unknown_commands']}")
    lib.write_report("c25", {"alerts": checked, "usage_commands": sorted(usage), "problems": problems})
    if problems:
        print("C25 failed: " + "; ".join(problems), file=sys.stderr)
        return 1
    print(f"C25 ok: {len(checked)} alerts, each with its runbook")
    return 0


if __name__ == "__main__":
    sys.exit(main())
