#!/usr/bin/env python3
"""C15: the scheduler of D07 against the exhaustive search of `verify/cmd/placeopt`, on the same instances and objective.

Seeds 1 to 50 (D12) each generate one instance with its own `random.Random(seed)`: 3 nodes, n services with n going from 6
to 12 over the seeds, CPU and memory requests, and anti-affinity groups for some services. `anden placement solve --json` and
`placeopt` read the same file, without a datacenter. Each seed leaves `c15/instance-<seed>.json` with the instance, the
placement of the scheduler and the services placed by the optimum.

Both placements are checked here, independently of either program: every service appears, every node exists, no node goes
over its CPU or memory and no node holds two services of a group. The gap between the scheduler and the optimum is reported,
not hidden: section 7 allows `equivalente` (same problem and objective, the gap is real). Fails if a program fails, if a
placement is invalid, if placeopt does not report the count of its own placement or if the scheduler places more than the
optimum.
"""
import json
import random
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import lib

RAW = lib.REPORT_DIR / "c15"
PLACEOPT = lib.ROOT / "bin" / "placeopt"
SEEDS = range(1, 51)
MIN_SERVICES, MAX_SERVICES = 6, 12
NODE_CPU_MILLIS = (2000, 3000, 4000)
NODE_MEM_MIB = (2048, 3072, 4096)
GROUPS = ("g1", "g2")
GROUP_PROBABILITY = 0.4
TIMEOUT_SECONDS = 300


def generate(seed):
    """The instance of a seed. n cycles over 6..12, so every size appears about as often."""
    rng = random.Random(seed)
    n = MIN_SERVICES + (seed - 1) % (MAX_SERVICES - MIN_SERVICES + 1)
    nodes = [{"name": f"node{i}", "cpu_millis": rng.choice(NODE_CPU_MILLIS), "mem_mib": rng.choice(NODE_MEM_MIB)}
             for i in range(1, lib.NODE_COUNT + 1)]
    services = []
    for i in range(n):
        group = rng.choice(GROUPS) if rng.random() < GROUP_PROBABILITY else ""
        services.append({"name": f"s{i:02d}", "cpu_millis": rng.randrange(100, 2001, 100),
                         "mem_mib": rng.randrange(64, 2049, 64), "anti_affinity_group": group})
    return {"nodes": nodes, "services": services}


def run_json(cmd):
    proc = subprocess.run([str(c) for c in cmd], capture_output=True, text=True, stdin=subprocess.DEVNULL,
                          timeout=TIMEOUT_SECONDS)
    if proc.returncode != 0:
        raise lib.CommandError(f"{' '.join(str(c) for c in cmd)}: rc={proc.returncode}: {proc.stderr.strip()}")
    return json.loads(proc.stdout)


def violations(problem, placement):
    """What is wrong with a placement of problem; an empty list if it is valid. Returns also the services placed."""
    errors = []
    nodes = {n["name"]: n for n in problem["nodes"]}
    names = {s["name"] for s in problem["services"]}
    if set(placement) != names:
        errors.append(f"services {sorted(set(placement) ^ names)} missing or unknown")
    used = {name: [0, 0, set()] for name in nodes}
    placed = 0
    for svc in problem["services"]:
        node = placement.get(svc["name"])
        if node is None:
            continue
        if node not in nodes:
            errors.append(f"{svc['name']} on unknown node {node}")
            continue
        placed += 1
        u = used[node]
        u[0] += svc["cpu_millis"]
        u[1] += svc["mem_mib"]
        group = svc["anti_affinity_group"]
        if group and group in u[2]:
            errors.append(f"{svc['name']} shares {node} with group {group}")
        u[2].add(group)
    for name, (cpu, mem, _) in used.items():
        if cpu > nodes[name]["cpu_millis"] or mem > nodes[name]["mem_mib"]:
            errors.append(f"{name} over capacity: {cpu} millicores, {mem} MiB")
    return errors, placed


def main():
    lib.build()
    shutil.rmtree(RAW, ignore_errors=True)
    RAW.mkdir(parents=True)
    instances, failures = [], []
    with tempfile.TemporaryDirectory(prefix="c15-") as tmp:
        for seed in SEEDS:
            problem = generate(seed)
            path = Path(tmp) / f"problem-{seed}.json"
            path.write_text(json.dumps(problem))
            placement = run_json([lib.ANDEN, "placement", "solve", "--input", path, "--json"])["placement"]
            optimal = run_json([PLACEOPT, "--input", path])

            errors, placed = violations(problem, placement)
            opt_errors, opt_placed = violations(problem, optimal["placement"])
            errors += [f"placeopt: {e}" for e in opt_errors]
            if opt_placed != optimal["placed"]:
                errors.append(f"placeopt reports {optimal['placed']} placed, its placement has {opt_placed}")
            if placed > optimal["placed"]:
                errors.append(f"scheduler places {placed}, more than the optimum {optimal['placed']}")

            raw = {"seed": seed, "problem": problem, "placement": placement, "optimal_placed": optimal["placed"]}
            (RAW / f"instance-{seed}.json").write_text(json.dumps(raw, indent=2) + "\n")
            instances.append({"seed": seed, "n": len(problem["services"]), "placed": placed,
                              "optimal_placed": optimal["placed"], "optimal_max_occupancy": optimal["max_occupancy"],
                              "errors": errors})
            if errors:
                failures.append(seed)
                print(f"seed {seed}: {'; '.join(errors)}", flush=True)

    gaps = [i["optimal_placed"] - i["placed"] for i in instances]
    lib.write_report("c15", {
        "seeds": [SEEDS.start, SEEDS.stop - 1],
        "instances": instances,
        "optimal_matched": sum(1 for g in gaps if g == 0),
        "total_gap": sum(gaps),
        "max_gap": max(gaps),
        "conclusion": "equivalente",
        "failed_seeds": failures,
    })
    print(f"C15: {len(instances)} instances, optimum matched in {sum(1 for g in gaps if g == 0)}, "
          f"total gap {sum(gaps)} services", flush=True)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
