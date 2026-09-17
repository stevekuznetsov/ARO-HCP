"""Compare exact greedy packing with CP-SAT or VNS, without exporting packings.

Run from the simulator directory: python -m optimize.benchmark --case small.
Runs are in-process; no RSS or tracemalloc-based memory claims are made.
"""

import argparse
from collections import Counter, defaultdict, deque
from dataclasses import asdict
from importlib.metadata import PackageNotFoundError, version
import json
import math
from pathlib import Path
import platform
import tempfile
import time

from costing import PRICES
from demand.model import DemandModel, PROFILES_PATH
from skus import load_catalog
from . import cpsat, engine, exact, vns


CASES = {
    "small": {"30": 2},
    "medium": {"12": 10, "30": 5, "250": 2},
    "default": {"12": 200, "30": 150, "60": 80, "120": 40, "250": 10},
    "user": {"12": 197, "30": 153, "60": 80, "120": 40, "250": 7},
}
NOTES = [
    "Both approaches optimize lexicographic zonal then overflow cores, not dollars.",
    "CP-SAT uses the standard portfolio (including LNS unless disabled), not custom VNS.",
    "CP-SAT bounds and OPTIMAL/INFEASIBLE status apply only to its bounded, rounded model.",
    "Full mode can change sharding, AZ placement and reserve demand; pinned fixes them.",
    "Pinned comparisons replay the legacy builder, including reserve-role routing differences.",
    "not_same_policy deltas are descriptive side-by-side differences, not policy-equivalent savings.",
    "Greedy SKU disjointness is per MC; a regional overlap is not a valid baseline.",
    "AZ reserve is packable headroom, not proof of AZ-failure schedulability.",
    "Wall time measures each solver call; CP search limit excludes build and validation.",
    "Runs are in-process; memory is not measured. Demand uses rounded packing JSON.",
    "Bill is worker-VM inventory only, using costing.PRICES; overflow is counted once.",
]
VNS_NOTES = [
    "VNS fixes greedy sharding, build_mc pod AZ/role and all real/reserve demand.",
    "SKU disjointness is per MC for both approaches, not global; regional overlap is reported.",
    "VNS minimizes zonal cores, then overflow cores, then nodes, not dollars; no optimality bound.",
    "Fixed comparisons replay legacy reserve routing, not a claim of demand-role policy compliance.",
    "AZ reserve is packable headroom, not proof of AZ-failure schedulability.",
    "VNS time limit includes build/seeding and is cooperative; individual packing calls can overrun.",
    "Repeat with --seed; fixed iterations reproduce search only when the time budget permits.",
    "Runs are in-process; memory is not measured. Demand uses rounded packing JSON.",
    "Bill is worker-VM inventory only, using costing.PRICES; overflow is counted once.",
]


def _validate_greedy(result, distribution, cfg, model, skus, fixed=False):
    """Rehydrate original demand, not rounded JSON, for the independent validator."""
    fleet = Counter()
    mc_count = 0
    for mc in result["management_clusters"]:
        count = mc["count"]
        if not isinstance(count, int) or count < 1:
            raise ValueError("validation: MC multiplicity")
        if not 1 <= sum(mc["hcp_mix"].values()) <= cfg.usable_slots:
            raise ValueError("validation: MC HCP slots")
        fleet.update({s: n * count for s, n in mc["hcp_mix"].items()})
        mc_count += count
        zonal, overflow = exact.build_mc(
            model, mc["hcp_mix"], cfg.policy, cfg.percentile, cfg.multiplier,
            cfg.unsteered_placement, cfg.az_failure_reserve,
            cfg.concurrent_rolling_hcps, cfg.reserve_slots, cfg.reserve_size)
        expected = [*zonal, overflow]
        actual = []
        by_name = {s.name: s for s in skus}
        for pods, nodes in zip(expected, [*mc["packing"]["zonal"], mc["packing"]["overflow"]]):
            originals = defaultdict(deque)
            for pod in pods:
                originals[tuple(sorted(exact.pod_to_json(pod).items()))].append(pod)
            packed = []
            for node in nodes:
                rebuilt = exact.new_node(cfg, by_name[node["sku"]])
                for pod in node["pods"]:
                    matches = originals[tuple(sorted(pod.items()))]
                    if not matches:
                        raise ValueError("validation: unexpected or duplicated pod")
                    rebuilt.place(matches.popleft())
                encoded = exact.node_to_json(rebuilt)
                if any(encoded[key] != node[key] for key in ("cap", "full", "used")):
                    raise ValueError("validation: node accounting")
                packed.append(rebuilt)
            actual.append(packed)
        # VNS permits balanced AZ spread for more than three replicas. Keep the
        # CP-SAT baseline's stricter validation unchanged for its comparisons.
        validator = vns._validate if fixed else cpsat._validate
        validator([expected], [actual], cfg, skus)
    if fleet != Counter(distribution) or mc_count != result["n_mcs"]:
        raise ValueError("validation: regional HCP/MC conservation")


def summarize(result, approach, distribution, cfg, model, skus, wall_seconds, mode="full"):
    solver = dict(result.get("solver", {}))
    row = {"approach": approach, "policy": cfg.policy,
           "status": solver.get("status", "NOT_SOLVED"), "wall_seconds": wall_seconds,
           "build_seconds": solver.get("seed_time_seconds" if approach == "vns" else "build_time_seconds"),
           "search_seconds": solver.get("search_time_seconds" if approach == "vns" else "solve_time_seconds"),
           "solver": solver, "feasible": False, "verified": False,
           "comparable": False, "reason": result.get("error"),
           "metrics": None, "regional_sku_overlap": [], "warnings": [],
           "reserve_role_difference": [], "policy_violation": False}
    mcs = result.get("management_clusters", [])
    if result.get("error") or result.get("region") is None or not mcs:
        return row
    if any(mc.get("zonal_pool") is None or mc.get("overflow_pool") is None
           or not mc.get("packing") or len(mc["packing"]["zonal"]) != 3 for mc in mcs):
        row["reason"] = "one or more MC pools could not be packed"
        return row
    try:
        if approach == "greedy":
            _validate_greedy(result, distribution, cfg, model, skus, fixed=mode == "fixed")
        elif approach == "vns":
            if solver.get("status") != "FEASIBLE" or not solver.get("validated"):
                raise ValueError("VNS incumbent was not independently validated")
            _validate_greedy(result, distribution, cfg, model, skus, fixed=True)
        elif solver.get("status") not in ("FEASIBLE", "OPTIMAL") or not solver.get("validated"):
            raise ValueError("CP-SAT incumbent was not independently validated")
    except ValueError as exc:
        row.update(status="INVALID", reason=str(exc))
        return row
    row.update(feasible=True, verified=True)
    if approach == "greedy":
        row["status"] = "FEASIBLE"
    pools = {}
    missing_prices = set()
    component_roles = {}
    role_differences = {}
    for role in ("zonal", "overflow"):
        pool = {"cores": 0, "nodes": 0, "memory_gib": 0, "pod_capacity": 0,
                "nic_capacity": 0, "hourly": 0, "sku_mix": Counter(),
                "real_demand": dict.fromkeys(("cpu_cores", "memory_gib", "pods", "nic"), 0),
                "reserved_demand": dict.fromkeys(("cpu_cores", "memory_gib", "pods", "nic"), 0)}
        for mc in mcs:
            count = mc["count"]
            nodes = ([n for az in mc["packing"][role] for n in az]
                     if role == "zonal" else mc["packing"][role])
            for node in nodes:
                pool["sku_mix"][node["sku"]] += count
                pool["nodes"] += count
                pool["cores"] += node["full"]["cpu_mc"] / 1000 * count
                pool["memory_gib"] += node["full"]["mem_mib"] / 1024 * count
                pool["pod_capacity"] += node["cap"]["pods"] * count
                pool["nic_capacity"] += node["cap"]["nic"] * count
                price = PRICES["hourly"].get(node["sku"])
                if price is None or not math.isfinite(price) or price < 0:
                    missing_prices.add(node["sku"])
                else:
                    pool["hourly"] += price * count
                for pod in node["pods"]:
                    demand = pool["reserved_demand" if pod["reserve"] else "real_demand"]
                    demand["cpu_cores"] += pod["cpu_mc"] / 1000 * count
                    demand["memory_gib"] += pod["mem_mib"] / 1024 * count
                    demand["pods"] += count
                    demand["nic"] += pod["nic"] * count
                    if pod["reserve"]:
                        size = pod["hcp_size"]
                        if size not in component_roles:
                            cd = model.cluster_demand(size, cfg.policy, cfg.percentile, cfg.multiplier,
                                                      unsteered_placement=cfg.unsteered_placement)
                            component_roles[size] = {c.name: "zonal" if c.zonal else "overflow"
                                                     for c in cd.components}
                        expected_role = component_roles[size][pod["component"]]
                        if role != expected_role:
                            key = (size, pod["component"], pod["reserve"], expected_role, role)
                            diff = role_differences.setdefault(key, {
                                "hcp_size": size, "component": pod["component"],
                                "reserve_kind": pod["reserve"], "expected_pool": expected_role,
                                "actual_pool": role, "pods": 0, "cpu_cores": 0, "memory_gib": 0})
                            diff["pods"] += count
                            diff["cpu_cores"] += pod["cpu_mc"] / 1000 * count
                            diff["memory_gib"] += pod["mem_mib"] / 1024 * count
        pool["sku_mix"] = dict(pool["sku_mix"])
        pools[role] = pool
    overlap = sorted(set(pools["zonal"]["sku_mix"]) & set(pools["overflow"]["sku_mix"]))
    row.update(regional_sku_overlap=overlap, comparable=mode == "fixed" or not overlap)
    if overlap:
        if mode == "fixed":
            row["warnings"].append("Per-MC comparison only, not globally SKU-disjoint: regional "
                                   "zonal/overflow SKU overlap: " + ", ".join(overlap))
        else:
            row["status"] = "NONCOMPLIANT"
            row["warnings"].append("Not a valid regional baseline: zonal/overflow SKU overlap: " + ", ".join(overlap))
    row["reserve_role_difference"] = [role_differences[key] for key in sorted(role_differences)]
    row["policy_violation"] = bool(role_differences)
    if role_differences:
        row["warnings"].append("policy_violation: reserve pods routed contrary to demand Component.zonal: "
                               + ", ".join(f"{d['component']} ({d['reserve_kind']}, {d['pods']} pods: "
                                           f"{d['actual_pool']} instead of {d['expected_pool']})"
                                           for d in row["reserve_role_difference"]))
        if mode == "full":
            row.update(comparable=False, status="NONCOMPLIANT")
    if mode == "pinned":
        row["warnings"].append("Pinned comparison: replay legacy builder actual demand, including "
                               "reserve routing; not a claim of demand-role policy compliance.")
    if mode == "fixed":
        row["warnings"].append("Fixed build_mc comparison: pinned sharding, pod AZ/role and reserves; "
                               "per-MC SKU disjointness, not global. Replay legacy builder reserve "
                               "routing, not a claim of demand-role policy compliance.")
    if missing_prices:
        row["warnings"].append("Missing/invalid hourly prices: " + ", ".join(sorted(missing_prices)))
        for pool in pools.values():
            pool["hourly"] = None
    totals = {key: sum(p[key] for p in pools.values()) for key in
              ("cores", "nodes", "memory_gib", "pod_capacity", "nic_capacity")}
    totals["hourly"] = None if missing_prices else sum(p["hourly"] for p in pools.values())
    for kind in ("real_demand", "reserved_demand"):
        totals[kind] = {key: sum(p[kind][key] for p in pools.values())
                        for key in pools["zonal"][kind]}
    totals["total_demand"] = {key: totals["real_demand"][key] + totals["reserved_demand"][key]
                              for key in totals["real_demand"]}
    row["metrics"] = {**pools, "total": totals, "n_mcs": result["n_mcs"]}
    return row


def run_case(name, distribution, cfg, model, skus, search, checkpoint=None, solver="cpsat"):
    mode = "fixed" if solver == "vns" else search.get("mode", "full")
    delta_key = f"difference_{solver}_minus_greedy"
    output = {"case": name, "distribution": dict(distribution), "policies": {}}
    for policy in ("legacy", "minimal"):
        config = engine.RunConfig(**{**asdict(cfg), "policy": policy, "mode": "exact"})
        rows = []
        comparison = {"config": asdict(config), "runs": rows,
                      delta_key: None, "comparison_basis": "incomplete"}
        if solver == "vns":
            comparison["scope"] = dict(vns.SCOPE)
        output["policies"][policy] = comparison
        for approach in ("greedy", solver):
            print(f"Starting {name}/{policy}/{approach} (mode={mode})", flush=True)
            started = time.perf_counter()
            try:
                if approach == "greedy":
                    result = engine.simulate(distribution, config, model, skus)
                elif approach == "vns":
                    result = vns.solve_region(distribution, config, model, skus, **search)
                else:
                    result = cpsat.solve_region(distribution, config, model, skus, **search)
            except ValueError as exc:
                result = {"error": str(exc), "solver": {"status": "REJECTED",
                          "objective": None, "best_objective_bound": None}}
            wall = time.perf_counter() - started
            rows.append(summarize(result, approach, distribution, config, model, skus, wall,
                                  mode=mode))
            del result  # Do not retain large packings between solver calls.
            if checkpoint:
                checkpoint(output)
            print(f"Completed {name}/{policy}/{approach}: {rows[-1]['status']} ({wall:.3f}s)", flush=True)
        difference = None
        basis = ("fixed_build_mc_per_mc" if solver == "vns" else
                 "replay_legacy_builder" if mode == "pinned" else
                 "not_same_policy" if any(r["policy_violation"] for r in rows) else "same_policy")
        if all(r["feasible"] and r["verified"] and
               (r["comparable"] or (basis == "not_same_policy" and not r["regional_sku_overlap"]))
               for r in rows):
            greedy, optimized = (r["metrics"] for r in rows)
            difference = {"zonal_cores": optimized["zonal"]["cores"] - greedy["zonal"]["cores"],
                          "overflow_cores": optimized["overflow"]["cores"] - greedy["overflow"]["cores"]}
            for key in ("cores", "nodes", "memory_gib", "hourly", "pod_capacity", "nic_capacity"):
                a, b = greedy["total"][key], optimized["total"][key]
                difference[key] = None if a is None or b is None else b - a
        comparison.update({delta_key: difference, "comparison_basis": basis})
        if checkpoint:
            checkpoint(output)
    return output


def print_report(report):
    def number(value):
        return "NA" if value is None else f"{value:.3f}"

    print("Case Policy Approach Status Wall(s) Build(s) Search(s) Zcores Ocores Nodes GiB Bill/h")
    for case in report["cases"]:
        for policy, comparison in case["policies"].items():
            for row in comparison["runs"]:
                metrics = row["metrics"]
                values = ([metrics["zonal"]["cores"], metrics["overflow"]["cores"],
                           metrics["total"]["nodes"], metrics["total"]["memory_gib"],
                           metrics["total"]["hourly"]] if metrics else [None] * 5)
                print(case["case"], policy, row["approach"], row["status"],
                      *(number(v) for v in [row["wall_seconds"], row["build_seconds"],
                                            row["search_seconds"], *values]))
                if row["approach"] == "cpsat":
                    print("  objective=" + number(row["solver"].get("objective")),
                          "bound=" + number(row["solver"].get("best_objective_bound")))
                if row["approach"] == "vns":
                    print("  VNS build includes seeding; neighborhood counters:")
                    for name, counters in row["solver"].get("neighborhoods", {}).items():
                        print(f"    {name}: attempted={counters['attempted']} "
                              f"feasible={counters['feasible']} improving={counters['improved']} "
                              f"shaken={counters['shakes_accepted']} no_op={counters.get('no_op', 0)}")
                for message in ([row["reason"]] if row["reason"] else []) + row["warnings"]:
                    print("  " + message)
            solver = "vns" if "difference_vns_minus_greedy" in comparison else "cpsat"
            delta = comparison[f"difference_{solver}_minus_greedy"]
            print(f"  {'VNS' if solver == 'vns' else 'CP-SAT'} minus greedy:", "NA (requires verified, comparable incumbents)"
                  if delta is None else f"[{comparison['comparison_basis']}] "
                  + ", ".join(f"{k}={number(v)}" for k, v in delta.items()))
    for note in report["notes"]:
        print(note)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", choices=[*CASES, "all"], default="small")
    parser.add_argument("--fleet", help="JSON size-to-count object; overrides --case (not all)")
    parser.add_argument("--solver", choices=("cpsat", "vns"), default="cpsat")
    parser.add_argument("--time-limit", type=float, default=30)
    parser.add_argument("--seed", type=int, default=1, help="search seed; repeat runs with caller-selected seeds")
    parser.add_argument("--max-iterations", type=int, default=1000, help="VNS iteration limit")
    parser.add_argument("--workers", type=int, default=8)
    parser.add_argument("--mode", choices=("full", "pinned"), default="full",
                        help="CP-SAT mode; VNS always fixes build_mc demand and uses per-MC SKU disjointness")
    parser.add_argument("--max-pods", type=int, default=10000)
    parser.add_argument("--max-model-size", type=int, default=200000)
    parser.add_argument("--reserve-slots", type=int, default=5)
    parser.add_argument("--az-reserve", type=float, default=engine.RunConfig().az_failure_reserve)
    parser.add_argument("--no-lns", action="store_true", help="disable LNS in the standard CP-SAT portfolio")
    parser.add_argument("--output", type=Path, help="optional summary JSON path")
    args = parser.parse_args(argv)
    if (not math.isfinite(args.time_limit) or args.time_limit < 0 or args.workers < 1
            or args.max_pods < 1 or args.max_model_size < 1 or args.max_iterations < 0
            or not 0 <= args.reserve_slots < engine.RunConfig().hcps_per_mc
            or not math.isfinite(args.az_reserve) or not 0 <= args.az_reserve <= 1):
        parser.error("invalid search limits or reserve settings")
    cases = CASES if args.case == "all" else {args.case: CASES[args.case]}
    if args.fleet is not None:
        if args.case == "all":
            parser.error("--fleet cannot be combined with --case all")
        try:
            fleet = json.loads(args.fleet)
        except json.JSONDecodeError as exc:
            parser.error(f"invalid --fleet JSON: {exc}")
        if (not isinstance(fleet, dict) or not fleet
                or any(type(n) is not int or n < 0 for n in fleet.values())
                or not sum(fleet.values())):
            parser.error("--fleet must be a nonempty size-to-nonnegative-integer object with real HCPs")
        cases = {"custom": {s: n for s, n in fleet.items() if n}}
    model, skus = DemandModel(), load_catalog()
    sizes = {s for dist in cases.values() for s in dist}
    if args.reserve_slots:
        sizes.add("30")
    if sizes - set(model.sizes):
        parser.error("unknown fleet sizes: " + ", ".join(sorted(sizes - set(model.sizes))))
    cfg = engine.RunConfig(mode="exact", reserve_slots=args.reserve_slots,
                           reserve_size="30", reservation_mode="scaled",
                           az_failure_reserve=args.az_reserve)
    search = {"time_limit": args.time_limit, "workers": args.workers, "seed": args.seed,
              "mode": args.mode, "use_lns": not args.no_lns, "max_pods": args.max_pods,
              "max_model_size": args.max_model_size, "slots_per_group": None,
              "slot_factor": 2.0, "slot_slack": 2}
    if args.solver == "vns":
        search = {"time_limit": args.time_limit, "seed": args.seed, "max_iterations": args.max_iterations}
    try:
        ortools_version = version("ortools")
    except PackageNotFoundError:
        ortools_version = None
    report = {"schema_version": 1, "provenance": {"python": platform.python_version(),
              "ortools": ortools_version, "profiles_path": str(PROFILES_PATH),
              "catalog": [asdict(s) for s in skus], "prices": PRICES,
              "solver": args.solver, "search": search},
              "notes": VNS_NOTES if args.solver == "vns" else NOTES, "cases": [], "status": "running"}

    def checkpoint(case=None):
        if case is not None:
            for index, existing in enumerate(report["cases"]):
                if existing["case"] == case["case"]:
                    report["cases"][index] = case
                    break
            else:
                report["cases"].append(case)
        if args.output:
            # Replace atomically so a reader never observes a partially written JSON document.
            with tempfile.NamedTemporaryFile(mode="w", dir=args.output.parent,
                                             prefix=args.output.name + ".", delete=False) as stream:
                temporary = Path(stream.name)
                try:
                    stream.write(json.dumps(report, indent=2, allow_nan=False) + "\n")
                    stream.close()
                    temporary.replace(args.output)
                finally:
                    temporary.unlink(missing_ok=True)

    checkpoint()
    try:
        for name, distribution in cases.items():
            checkpoint(run_case(name, distribution, cfg, model, skus, search,
                                checkpoint=checkpoint, solver=args.solver))
        report["status"] = "complete"
    except KeyboardInterrupt:
        report["status"] = "interrupted"
        print("Interrupted; preserving completed rows.", flush=True)
    finally:
        if report["status"] == "running":
            report["status"] = "failed"
        checkpoint()
    print_report(report)
    return 130 if report["status"] == "interrupted" else 0


if __name__ == "__main__":
    raise SystemExit(main())
