"""Variable neighborhood search over exact greedy's concrete pod inventory.

Sharding, pod AZ/role and every reserve copy are fixed by build_mc. SKU quotas
are disjoint PER MC, like greedy, NOT region-wide like CP-SAT. The objective is
lexicographic regional (zonal cores, overflow cores, physical node count), with
MC shape multiplicities included. This is a heuristic, not an optimality proof
or a proof of AZ-failure schedulability. No CP-SAT dependency is used.

The wall budget includes building/seeding. Greedy seeding is not interruptible
and search checks its deadline cooperatively, so the limit is not a hard
timeout. Fixed iterations reproduce the search when time permits;
elapsed times and time-limited stopping are naturally not reproducible.
"""

import math
import random
import time
from collections import Counter, defaultdict
from dataclasses import replace

from . import engine, exact


NEIGHBORHOODS = ("evacuate", "repack_2", "repack_4", "repack_8", "rightsize", "sku_role")
SCOPE = {
    "hcp_mc_sharding": "fixed_distribute_conserving",
    "pod_az_and_role": "fixed_build_mc",
    "reserve_demand": "fixed_build_mc_real_and_reserve_inventory",
    "sku_disjointness": "per_MC_same_as_greedy_not_global",
    "objective": "lexicographic_regional_zonal_then_overflow_cores_then_nodes",
    "az_failure_guarantee": "packable_headroom_not_failure_scenario_proof",
}


class _BudgetExpired(Exception):
    pass


def _check_budget(deadline):
    if time.perf_counter() >= deadline:
        raise _BudgetExpired


def _copy_mc(mc):
    # Only Node state is copied. Pods remain the original, read-only objects.
    return [[replace(n, cap=dict(n.cap), full=dict(n.full), system=dict(n.system),
                     daemonset=dict(n.daemonset), buffer=dict(n.buffer),
                     used=dict(n.used), keys=set(n.keys), pods=list(n.pods))
             for n in pool] for pool in mc]


def _normalize(mc, skus, cfg):
    """Discard old padding, rebuild accounting, then balance every SKU in all AZs."""
    by_name = {s.name: s for s in skus}
    for gi, nodes in enumerate(mc):
        rebuilt = []
        for node in nodes:
            if not node.pods:
                continue
            fresh = exact.new_node(cfg, by_name[node.sku])
            for pod in node.pods:
                fresh.place(pod)
            rebuilt.append(fresh)
        mc[gi] = rebuilt
    counts = [Counter(n.sku for n in az) for az in mc[:3]]
    for name in sorted(set().union(*counts)):
        target = max(c[name] for c in counts)
        for az, count in zip(mc[:3], counts):
            az.extend(exact.new_node(cfg, by_name[name]) for _ in range(target - count[name]))
    return mc


def _validate(groups, packed, cfg, skus):
    """Independent check using actual pods/capacities, never serialized rounding.

    Returns regional SKU overlap, which is deliberately NOT a rejection under
    the greedy-comparison scope. Expected groups contain original Pod objects.
    """
    by_name = {s.name: s for s in skus}
    roles = [set(), set()]
    if len(groups) != len(packed):
        raise ValueError("validation: MC conservation")
    for expected, mc in zip(groups, packed):
        if len(expected) != 4 or len(mc) != 4:
            raise ValueError("validation: missing pool")
        inventories = [Counter(n.sku for n in az) for az in mc[:3]]
        if not inventories[0] == inventories[1] == inventories[2]:
            raise ValueError("validation: per-SKU AZ balance")
        local_roles = [set(), set()]
        zones = defaultdict(list)
        for gi, (pods, nodes) in enumerate(zip(expected, mc)):
            if Counter(map(id, pods)) != Counter(id(p) for n in nodes for p in n.pods):
                raise ValueError("validation: pod conservation or fixed MC/AZ/role")
            for node in nodes:
                if node.sku not in by_name:
                    raise ValueError("validation: unknown SKU")
                local_roles[int(gi == 3)].add(node.sku)
                full, system, daemonset, buffer, cap = exact.node_caps(cfg, by_name[node.sku])
                if (node.full, node.system, node.daemonset, node.buffer, node.cap) != (
                        full, system, daemonset, buffer, cap):
                    raise ValueError("validation: SKU capacity/overhead")
                keys = {p.key for p in node.pods}
                if len(keys) != len(node.pods):
                    raise ValueError("validation: host anti-affinity")
                if node.keys != keys:
                    raise ValueError("validation: key accounting")
                for dim in exact.DIMS:
                    values = [p.dim(dim) for p in node.pods]
                    if any(not math.isfinite(v) or v < 0 for v in values):
                        raise ValueError("validation: invalid pod resource")
                    used = sum(values)
                    if (not math.isfinite(cap[dim]) or cap[dim] < 0
                            or not math.isfinite(node.used[dim])
                            or used > cap[dim] + 1e-6 or abs(used - node.used[dim]) > 1e-6):
                        raise ValueError(f"validation: {dim} capacity/accounting")
                for pod in node.pods:
                    if (gi < 3 and pod.reserve_kind in (None, "slot")
                            and pod.tier in ("zonal_etcd", "zonal_pair")):
                        zones[pod.key].append(gi)
        for azs in zones.values():
            counts = Counter(azs)
            if max(counts[a] for a in range(3)) - min(counts[a] for a in range(3)) > 1:
                raise ValueError("validation: replica AZ spread")
        if local_roles[0] & local_roles[1]:
            raise ValueError("validation: per-MC SKU disjointness")
        for region, local in zip(roles, local_roles):
            region.update(local)
    return sorted(roles[0] & roles[1])


def _objective(packed, counts):
    return tuple(sum(count * sum(
        (n.full["cpu"] / 1000 if metric < 2 else 1)
        for gi, pool in enumerate(mc) for n in pool
        if metric == 2 or (gi == 3) == (metric == 1))
        for mc, count in zip(packed, counts)) for metric in range(3))


def _allowed(mc, role, skus):
    other = {n.sku for gi, pool in enumerate(mc) if (gi == 3) != role for n in pool if n.pods}
    return [s for s in skus if s.name not in other]


def _signature(mc):
    """Ignore node/pod ordering, but retain SKU, pool and original pod identity."""
    return tuple(tuple(sorted((n.sku, tuple(sorted(map(id, n.pods)))) for n in pool))
                 for pool in mc)


def _repack(pods, allowed, cfg, rng, deadline):
    _check_budget(deadline)
    pods = list(pods)
    if not pods:
        return []
    allowed = sorted(allowed, key=lambda s: (s.vcpu, s.memory_gib, s.name))
    best, best_cost = None, None
    for base in allowed:
        _check_budget(deadline)
        cap = exact.node_caps(cfg, base)[-1]
        if cap["cpu"] <= 0 or cap["mem"] <= 0:
            continue
        rng.shuffle(pods)  # Randomize FFD ties independently for each base.
        ordered = sorted(pods, key=lambda p: max(
            p.dim(d) / cap[d] if cap[d] else 0 for d in exact.DIMS), reverse=True)
        nodes = []
        for pod in ordered:
            _check_budget(deadline)
            for node in nodes:
                _check_budget(deadline)
                if node.fits(pod):
                    node.place(pod)
                    break
            else:
                node = exact.new_node(cfg, base)
                if not node.fits(pod):
                    break
                node.place(pod)
                nodes.append(node)
        else:
            rebuilt = []
            for node in nodes:
                _check_budget(deadline)
                sku = exact._best_fit_sku(node, allowed, cfg)
                if sku is None:
                    break
                rebuilt.append(exact._remake_node(node, sku, cfg))
            else:
                cost = (exact._cores(rebuilt), len(rebuilt), sum(n.full["mem"] for n in rebuilt))
                if best_cost is None or cost < best_cost:
                    best, best_cost = rebuilt, cost
    _check_budget(deadline)
    if best is not None:
        return best
    # A homogeneous base must fit EVERY pod. Complementary
    # CPU/memory SKUs may have no such base, yet form a feasible mixed inventory.
    nodes = []
    for pod in pods:
        _check_budget(deadline)
        for node in nodes:
            _check_budget(deadline)
            if node.fits(pod):
                node.place(pod)
                break
        else:
            for sku in allowed:
                _check_budget(deadline)
                node = exact.new_node(cfg, sku)
                if node.fits(pod):
                    node.place(pod)
                    nodes.append(node)
                    break
            else:
                return None
    return nodes


def _candidate(current, name, skus, cfg, rng, deadline):
    mc = _copy_mc(current)
    # Balancing-only nodes must not keep an otherwise retired SKU assigned.
    mc = [[n for n in pool if n.pods] for pool in mc]
    nonempty = [gi for gi, pool in enumerate(mc) if pool]
    if not nonempty:
        return mc
    gi = rng.choice(nonempty)
    role = gi == 3
    allowed = _allowed(mc, role, skus)
    if name == "evacuate":
        pool = mc[gi]
        rng.shuffle(pool)
        target_index = min(range(len(pool)), key=lambda i: max(
            pool[i].used[d] / pool[i].cap[d] if pool[i].cap[d] else 0 for d in exact.DIMS))
        target = pool.pop(target_index)
        pods = list(target.pods)
        rng.shuffle(pods)
        for pod in pods:
            _check_budget(deadline)
            for node in pool:
                _check_budget(deadline)
                if node.fits(pod):
                    node.place(pod)
                    break
            else:
                return None
    elif name.startswith("repack_"):
        k = int(name.split("_")[1])
        selected = rng.sample(range(len(mc[gi])), min(k, len(mc[gi])))
        pods = [p for ni in selected for p in mc[gi][ni].pods]
        rebuilt = _repack(pods, allowed, cfg, rng, deadline)
        if rebuilt is None:
            return None
        mc[gi] = [n for ni, n in enumerate(mc[gi]) if ni not in selected] + rebuilt
    elif name == "rightsize":
        for index, pool in enumerate(mc):
            if (index == 3) != role:
                continue
            for ni, node in enumerate(pool):
                _check_budget(deadline)
                sku = exact._best_fit_sku(node, allowed, cfg)
                if sku is None:
                    return None
                pool[ni] = exact._remake_node(node, sku, cfg)
    elif name == "sku_role":
        retired = rng.choice(sorted({n.sku for index, pool in enumerate(mc)
                                     if (index == 3) == role for n in pool}))
        alternatives = [s for s in allowed if s.name != retired]
        for index, pool in enumerate(mc):
            if (index == 3) != role:
                continue
            pods = [p for n in pool if n.sku == retired for p in n.pods]
            rebuilt = _repack(pods, alternatives, cfg, rng, deadline)
            if rebuilt is None:
                return None
            mc[index] = [n for n in pool if n.sku != retired] + rebuilt
        # Once the last node of the retired SKU is gone in this role, the
        # opposite pool can use it. Never borrow a SKU still used by that pool.
        other_allowed = _allowed(mc, not role, skus)
        for index, pool in enumerate(mc):
            if (index == 3) == role:
                continue
            for ni, node in enumerate(pool):
                _check_budget(deadline)
                sku = exact._best_fit_sku(node, other_allowed, cfg)
                if sku is None:
                    return None
                pool[ni] = exact._remake_node(node, sku, cfg)
    else:
        raise ValueError(f"unknown neighborhood: {name}")
    _check_budget(deadline)
    return _normalize(mc, skus, cfg)


def improve_packing(groups, skus, cfg, time_limit=20, seed=1, max_iterations=1000,
                    *, counts=None, initial=None, shake_probability=0.15):
    """Return (best Node packing, stats); no seed incumbent returns (None, stats).

    groups[shape][0:3] are original zonal Pod lists; [3] is overflow. counts
    weights identical MC shapes (default one each). initial, when supplied, has
    the same layout with Nodes, and must contain those exact original Pods.
    Neither initial nor any Pod is mutated. Without initial, seed with the
    engine's size_pools_disjoint. Validation failures reject the seed, not repair
    it silently. Invalid arguments raise ValueError. Per-neighborhood feasible
    includes no_op candidates: identical SKU/pod assignments up to node/pod
    order. no_op is a subset, never counted as accepted or shakes_accepted.
    """
    started = time.perf_counter()
    if (not math.isfinite(time_limit) or time_limit < 0
            or (max_iterations is not None and (not isinstance(max_iterations, int)
                                               or isinstance(max_iterations, bool) or max_iterations < 0))
            or not 0 <= shake_probability <= 1):
        raise ValueError("invalid search limits")
    counts = [1] * len(groups) if counts is None else list(counts)
    if len(counts) != len(groups) or any(type(c) is not int or c < 1 for c in counts):
        raise ValueError("counts must be positive MC shape multiplicities")
    skus = sorted(skus, key=lambda s: (s.vcpu, s.memory_gib, s.name))
    if len({s.name for s in skus}) != len(skus):
        raise ValueError("SKU names must be unique")
    if any(type(s.vcpu) is not int or s.vcpu <= 0 for s in skus):
        raise ValueError("SKU cores must be positive integers")
    stats = {"status": "NOT_RUN", "seed": seed, "scope": dict(SCOPE), "validated": False,
             "seed_time_seconds": 0.0, "search_time_seconds": 0.0, "runtime_seconds": 0.0,
             "time_limit_seconds": time_limit, "max_iterations": max_iterations,
             "objective": None, "seed_objective": None, "current_objective": None,
             "best_objective_bound": None, "best_history": [], "iterations": 0,
             "shakes_accepted": 0, "worse_shakes_accepted": 0,
             "neighborhoods": {n: dict(attempted=0, feasible=0, infeasible=0,
                                        timed_out=0, no_op=0, accepted=0, improved=0,
                                        shakes_accepted=0) for n in NEIGHBORHOODS}}
    try:
        if not groups:
            raise ValueError("empty fleet")
        if initial is None:
            initial = []
            for group in groups:
                if len(group) != 4:
                    raise ValueError("validation: missing pool")
                zs, zn, os, on = exact.size_pools_disjoint(group[:3], group[3], skus, cfg)
                if zs is None or os is None:
                    raise ValueError("greedy seed could not pack all pools")
                initial.append([*zn, on])
        _validate(groups, initial, cfg, skus)
    except ValueError as exc:
        stats.update(status="SEED_INFEASIBLE", error=str(exc),
                     seed_time_seconds=time.perf_counter() - started)
        stats["runtime_seconds"] = stats["seed_time_seconds"]
        return None, stats
    current = [_copy_mc(mc) for mc in initial]
    best = current
    cost = best_cost = _objective(current, counts)
    stats["seed_objective"] = list(cost)
    stats["seed_time_seconds"] = time.perf_counter() - started
    stats["best_history"].append([0, stats["seed_time_seconds"], *cost])
    search_started = time.perf_counter()
    deadline = started + time_limit
    rng = random.Random(seed)
    neighborhood = 0
    iteration = 0
    while (max_iterations is None or iteration < max_iterations) and time.perf_counter() < deadline:
        iteration += 1
        name = NEIGHBORHOODS[neighborhood]
        counter = stats["neighborhoods"][name]
        counter["attempted"] += 1
        mi = rng.randrange(len(groups))
        try:
            changed = _candidate(current[mi], name, skus, cfg, rng, deadline)
            if changed is None:
                raise ValueError("neighborhood repair failed")
            _validate([groups[mi]], [changed], cfg, skus)
            _check_budget(deadline)
        except _BudgetExpired:
            counter["timed_out"] += 1
            stats["stop_reason"] = "time_limit"
            break
        except ValueError:
            counter["infeasible"] += 1
            neighborhood = (neighborhood + 1) % len(NEIGHBORHOODS)
            continue
        counter["feasible"] += 1
        if _signature(changed) == _signature(current[mi]):
            counter["no_op"] += 1
            neighborhood = (neighborhood + 1) % len(NEIGHBORHOODS)
            continue
        candidate = list(current)
        candidate[mi] = changed
        candidate_cost = _objective(candidate, counts)
        improving = candidate_cost < cost
        # Bounded uphill/equal shaking explores alternative SKU ownership and
        # pod orders. The best incumbent is retained separately and never lost.
        shake = (not improving and rng.random() < shake_probability
                 and candidate_cost[0] <= best_cost[0] + max(3, best_cost[0] * 0.25)
                 and candidate_cost[1] <= best_cost[1] + max(1, best_cost[1] * 0.25))
        if improving or shake:
            counter["accepted"] += 1
            if shake:
                stats["shakes_accepted"] += 1
                counter["shakes_accepted"] += 1
                stats["worse_shakes_accepted"] += int(candidate_cost > cost)
            current, cost = candidate, candidate_cost
        if candidate_cost < best_cost:
            best, best_cost = candidate, candidate_cost
            counter["improved"] += 1
            stats["best_history"].append([iteration, time.perf_counter() - started, *best_cost])
        neighborhood = 0 if improving else (neighborhood + 1) % len(NEIGHBORHOODS)
    overlap = _validate(groups, best, cfg, skus)
    stats.update(status="FEASIBLE", validated=True, iterations=iteration,
                 objective=list(best_cost), current_objective=list(cost),
                 regional_sku_overlap=overlap, regional_sku_disjoint=not overlap,
                 stop_reason=stats.get("stop_reason", "iteration_limit"
                                       if iteration == max_iterations else "time_limit"),
                 search_time_seconds=time.perf_counter() - search_started,
                 runtime_seconds=time.perf_counter() - started)
    return best, stats


def solve_region(distribution, cfg, model, skus, time_limit=20, seed=1, max_iterations=1000):
    """Return standard simulate output including packing and solver statistics.

    Always seeds exact greedy, regardless of cfg.mode. time_limit includes the
    seed; zero still returns a validated seed without searching. max_iterations
    may be None for time-only stopping. No feasible seed returns an explicit
    error with region=None, never a zero-cost pseudo-solution. See SCOPE.
    """
    started = time.perf_counter()
    if any(type(n) is not int or n < 0 for n in distribution.values()):
        raise ValueError("distribution counts must be nonnegative integers")
    if (not math.isfinite(time_limit) or time_limit < 0
            or (max_iterations is not None and (type(max_iterations) is not int or max_iterations < 0))):
        raise ValueError("invalid search limits")
    if (type(cfg.reserve_slots) is not int or cfg.reserve_slots < 0
            or type(cfg.hcps_per_mc) is not int or cfg.hcps_per_mc <= cfg.reserve_slots):
        raise ValueError("MC slots must leave room for real HCPs")
    if (type(cfg.concurrent_rolling_hcps) is not int or cfg.concurrent_rolling_hcps < 0
            or not math.isfinite(cfg.az_failure_reserve) or not 0 <= cfg.az_failure_reserve <= 1
            or type(cfg.overflow_az_count) is not int or not 1 <= cfg.overflow_az_count <= 3):
        raise ValueError("invalid reserve or overflow AZ configuration")
    distribution = {s: n for s, n in distribution.items() if n}
    shards = exact.distribute_conserving(distribution, cfg.usable_slots)
    shapes = exact.group_shapes(shards)
    groups = []
    for shard, _ in shapes:
        zonal, overflow = exact.build_mc(
            model, shard, cfg.policy, cfg.percentile, cfg.multiplier,
            cfg.unsteered_placement, cfg.az_failure_reserve, cfg.concurrent_rolling_hcps,
            cfg.reserve_slots, cfg.reserve_size)
        groups.append([*zonal, overflow])
    build_seconds = time.perf_counter() - started
    packed, stats = improve_packing(groups, skus, cfg, max(0, time_limit - build_seconds),
                                    seed, max_iterations, counts=[c for _, c in shapes])
    stats["seed_time_seconds"] += build_seconds
    stats["time_limit_seconds"] = time_limit
    for row in stats["best_history"]:
        row[1] += build_seconds
    result = {"policy": cfg.policy, "total_hcps": sum(distribution.values()),
              "n_mcs": len(shards), "per_mc_hcps": cfg.usable_slots,
              "region": None, "management_clusters": [], "solver": stats}
    if packed is None:
        result["error"] = stats["error"]
        if not distribution:
            stats["status"] = "EMPTY_FLEET"
    else:
        fleet = Counter()
        for (shard, count), mc in zip(shapes, packed):
            fleet.update({s: n * count for s, n in shard.items()})
            zstats = exact._pool_stats([n for az in mc[:3] for n in az], 3, "zonal")
            ostats = exact._pool_stats(mc[3], cfg.overflow_az_count, "overflow")
            ostats["util_nic"] = None
            result["management_clusters"].append({
                "kind": "full" if sum(shard.values()) == cfg.usable_slots else "remainder",
                "hcps": sum(shard.values()), "count": count, "hcp_mix": shard,
                "zonal_pool": zstats, "overflow_pool": ostats,
                "zonal_cores": zstats["total_cores"], "overflow_cores": ostats["total_cores"],
                "packing": {"zonal": [[exact.node_to_json(n) for n in az] for az in mc[:3]],
                            "overflow": [exact.node_to_json(n) for n in mc[3]]}})
        if fleet != Counter(distribution):
            raise ValueError("validation: regional HCP conservation")
        region = {f"{role}_{metric}": sum(mc["count"] * mc[f"{role}_pool"][f"total_{metric}"]
                                         for mc in result["management_clusters"])
                  for role in ("zonal", "overflow") for metric in ("cores", "nodes")}
        region["hcp_per_zonal_core"] = (result["total_hcps"] / region["zonal_cores"]
                                        if region["zonal_cores"] else None)
        result["region"] = region
        zonal, overflow, _, total = engine.region_demand(model, distribution, cfg)
        result["demand_per_mc"] = {"zonal": engine._scale(zonal, cfg.usable_slots / total),
                                   "overflow": engine._scale(overflow, cfg.usable_slots / total)}
    stats["runtime_seconds"] = time.perf_counter() - started
    return result
