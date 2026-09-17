"""Bounded regional CP-SAT experiments, independent of the FFD solver.

The default mode="full" jointly chooses HCP -> MC, pod AZ and node placement,
node SKU/count and the REGION-WIDE zonal/overflow SKU partition. MC count is
fixed to ceil(real HCPs / usable_slots); node roles follow the demand policy.
Each real HCP stays entirely on one MC; growth placeholders stay on their MC.
Rollout reserves belong to the largest K actual occupants of EACH solved MC
(ties use global HCP ID). AZ-death reserves copy a prefix of the real pair pods
actually assigned to each MC/AZ, reaching the configured memory fraction.
Surge AZs are free decisions, not the FFD least-loaded-AZ heuristic. Growth
slots are excluded from both reserve selections, as in exact.build_mc.

mode="pinned" retains the original fixed distribute_conserving/build_mc demand,
including fixed AZs, for apples-to-apples placement comparisons. Unlike pinned,
full can change the reserve footprint when sharding or AZ assignments change.
Zonal SKU inventories are identical in all three AZs of each MC. Overflow
inventory is physical inventory, counted once regardless of its AZ span.

Each pod occupies a unit interval at an integer node index. A fixed unit
blocker at every candidate node consumes max_capacity - selected_SKU_capacity.
Four cumulative constraints therefore enforce CPU, memory, NIC and pod limits
without a pods x nodes Boolean matrix. SKU 0 is an absent, zero-capacity node.

The objective is lexicographic (regional zonal cores, regional overflow cores),
encoded with an overflow upper-bound weight, matching exact's priority. Default
slot limits are aggregate lower bounds * slot_factor + slot_slack, NOT proven
upper bounds. OPTIMAL/INFEASIBLE and objective bounds apply only to this bounded,
conservatively rounded model. Increase slots_per_group or use
slot_factor/slot_slack to explore a wider search space. No greedy run or hint is
required. CPU/memory demands round UP and capacities DOWN to 0.001 native units;
NIC/pods remain integral. Feasible solutions are validated in original units.
Full AZ-death prefix selection uses floored prefix memory, ceiled total memory,
and a reserve fraction rounded UP to millionths (denominator <= 1,000,000).
Rounding can add a reserve copy but cannot under-reserve. This is
the existing packable-headroom policy, NOT a proof of AZ-failure schedulability.

OR-Tools is imported lazily. time_limit limits SOLVING, not Python model build
or validation; max_pods/max_model_size guard construction, not solver RSS.
Parallel portfolio search has a fixed seed but is not reproducible; workers=1
is recommended for repeatability. No incumbent means region=None, never zero
cost or a fake empty packing.
"""

import math
import sys
import time
from collections import Counter, defaultdict
from dataclasses import dataclass, replace
from decimal import Decimal, ROUND_CEILING, ROUND_FLOOR
from fractions import Fraction

from . import exact
from demand import components as comp


@dataclass
class _WorkPod:
    pod: exact.Pod
    zonal: bool
    source: int = None  # index of the working pod copied by an optional reserve


def _reserve_fraction(value):
    # Float multiplication before ceil can round down at a millionth boundary.
    return Fraction(math.ceil(Fraction(str(value)) * 1000000), 1000000)


def _full_workload(distribution, cfg, cluster_demands, n_mcs):
    """Enumerate mandatory pods and potential reserves, without assigning a shard."""
    sizes = [s for s, count in distribution.items() for _ in range(count)]
    total = len(sizes)
    sizes += [cfg.reserve_size] * (n_mcs * cfg.reserve_slots)
    records, rollouts, deaths = [], [], []
    for hcp, size in enumerate(sizes):
        for c in cluster_demands[size].components:
            first = len(records)
            for _ in range(c.replicas):
                pod = exact.Pod(hcp, size, c.name, c.tier, c.cpu_mc, c.mem_mib,
                                c.nic, f"{hcp}:{c.name}",
                                reserve_kind=None if hcp < total else "slot")
                if (hcp < total and c.zonal and c.tier == "zonal_pair"
                        and cfg.az_failure_reserve > 0):
                    deaths.append(_WorkPod(replace(pod, reserve_kind="azdeath",
                                                   key=f"azdeath:{len(records)}"),
                                           True, len(records)))
                records.append(_WorkPod(pod, c.zonal))
            if (hcp < total and c.replicas > 1 and cfg.concurrent_rolling_hcps > 0
                    and not comp.is_statefulset(c.name)):
                rollouts.append(_WorkPod(replace(records[first].pod, reserve_kind="rollout"),
                                         c.zonal, first))
    return sizes, records + rollouts + deaths


def _add_full_placement(problem, cp_model, records, sizes, total, cfg, n_mcs,
                        slots, demands, intervals, resource_demands):
    """Integer coordinates encode MC/AZ; only HCP/MC and pair/AZ indicators expand."""
    hcp_mc = [problem.new_int_var(0, n_mcs - 1, f"hcp{h}_mc") for h in range(len(sizes))]
    memberships = []
    for h in range(total):
        row = []
        for mc in range(n_mcs):
            member = problem.new_bool_var(f"hcp{h}_in{mc}")
            problem.add(hcp_mc[h] == mc).only_enforce_if(member)
            problem.add(hcp_mc[h] != mc).only_enforce_if(member.Not())
            row.append(member)
        memberships.append(row)
    for mc in range(n_mcs):
        count = sum(row[mc] for row in memberships)
        problem.add(count >= 1)
        problem.add(count <= cfg.usable_slots)
    problem.add(hcp_mc[0] == 0)  # MC labels are interchangeable.
    for h in range(total, len(sizes)):
        problem.add(hcp_mc[h] == (h - total) // cfg.reserve_slots)

    # Rank each HCP amongst its actual MC occupants, not amongst a pinned shard.
    memory = Counter()
    for record in records:
        if record.pod.reserve_kind is None:
            memory[record.pod.hcp] += record.pod.mem_mib
    rolling = {}
    if cfg.concurrent_rolling_hcps:
        prefix = [0] * n_mcs
        for h in sorted(range(total), key=lambda h: (-memory[h], h)):
            selected = problem.new_bool_var(f"hcp{h}_rolling")
            rank = problem.new_int_var(0, total - 1, f"hcp{h}_rank")
            problem.add_element(hcp_mc[h], prefix, rank)
            problem.add(rank < cfg.concurrent_rolling_hcps).only_enforce_if(selected)
            problem.add(rank >= cfg.concurrent_rolling_hcps).only_enforce_if(selected.Not())
            rolling[h] = selected
            for mc in range(n_mcs):
                after = problem.new_int_var(0, total, f"hcp{h}_prefix{mc}")
                problem.add(after == prefix[mc] + memberships[h][mc])
                prefix[mc] = after

    width = 4 * slots
    variables, keys, zones = [], defaultdict(list), defaultdict(list)
    for i, record in enumerate(records):
        pod = record.pod
        domain = cp_model.Domain.from_intervals([
            [mc * width, mc * width + 3 * slots - 1] if record.zonal else
            [mc * width + 3 * slots, (mc + 1) * width - 1]
            for mc in range(n_mcs)])
        node = problem.new_int_var_from_domain(domain, f"pod{i}_node")
        problem.add_division_equality(hcp_mc[pod.hcp], node, width)
        bucket = problem.new_int_var(0, 4 * n_mcs - 1, f"pod{i}_bucket")
        problem.add_division_equality(bucket, node, slots)
        zone = problem.new_int_var(0, 3, f"pod{i}_zone")
        problem.add(zone == bucket - 4 * hcp_mc[pod.hcp])
        presence = None
        if pod.reserve_kind == "rollout":
            presence = rolling[pod.hcp]
        elif pod.reserve_kind == "azdeath":
            presence = problem.new_bool_var(f"pod{i}_reserved")
            problem.add(bucket == variables[record.source][1])
        if presence is None:
            interval = problem.new_fixed_size_interval_var(node, 1, f"pod{i}")
        else:
            interval = problem.new_optional_fixed_size_interval_var(node, 1, presence, f"pod{i}")
        intervals.append(interval)
        for d in range(4):
            resource_demands[d].append(demands[id(pod)][d])
        variables.append((node, bucket, presence))
        if presence is None:
            keys[pod.key].append(node)
            if record.zonal:
                zones[pod.key].append(zone)
        elif pod.reserve_kind == "rollout":
            for other in keys[pod.key]:
                problem.add(node != other).only_enforce_if(presence)
    for nodes in keys.values():
        if len(nodes) > 1:
            problem.add_all_different(nodes)
    for key, replicas in zones.items():
        if len(replicas) <= 3:
            problem.add_all_different(replicas)
        else:
            # General zonal components can have >3 replicas: enforce maxSkew=1.
            for az in range(3):
                members = []
                for ri, zone in enumerate(replicas):
                    member = problem.new_bool_var(f"{key}_{ri}_az{az}")
                    problem.add(zone == az).only_enforce_if(member)
                    problem.add(zone != az).only_enforce_if(member.Not())
                    members.append(member)
                problem.add(sum(members) >= len(replicas) // 3)
                problem.add(sum(members) <= math.ceil(len(replicas) / 3))

    # Each MC/AZ reserves a deterministic prefix of its actual real pair pods.
    # Integer bounds conservatively implement the memory threshold, not a fixed
    # per-HCP copy count or a footprint inherited from a different sharding.
    fraction = _reserve_fraction(cfg.az_failure_reserve)
    copies = [(i, r) for i, r in enumerate(records) if r.pod.reserve_kind == "azdeath"]
    max_memory = sum(demands[id(r.pod)][1] for _, r in copies)
    for mc in range(n_mcs):
        for az in range(3):
            members = []
            for i, record in copies:
                member = problem.new_bool_var(f"pair{i}_m{mc}a{az}")
                source_bucket = variables[record.source][1]
                problem.add(source_bucket == 4 * mc + az).only_enforce_if(member)
                problem.add(source_bucket != 4 * mc + az).only_enforce_if(member.Not())
                members.append(member)
            # Materialize once: repeating this sum in every prefix constraint
            # would serialize O(pair_count**2) terms per MC/AZ.
            target = problem.new_int_var(0, fraction.numerator * max_memory,
                                         f"m{mc}a{az}_reserve_target")
            problem.add(target == fraction.numerator * sum(
                member * demands[id(record.pod)][1]
                for member, (_, record) in zip(members, copies)))
            prefix = 0
            for member, (i, record) in zip(members, copies):
                presence = variables[i][2]
                problem.add(prefix * fraction.denominator < target).only_enforce_if([member, presence])
                problem.add(prefix * fraction.denominator >= target).only_enforce_if([member, presence.Not()])
                after = problem.new_int_var(0, max_memory, f"pair{i}_m{mc}a{az}_prefix")
                problem.add(after == prefix + member * _integer(record.pod.mem_mib, 1000, ROUND_FLOOR))
                prefix = after
    return hcp_mc, variables


def _validate_full(records, sizes, total, cfg, n_mcs, assignments, locations, packed, skus):
    """Recompute reserve selection and all placement invariants without CP variables."""
    if len(assignments) != len(sizes) or any(not 0 <= mc < n_mcs for mc in assignments):
        raise ValueError("validation: HCP assignments")
    if len(locations) != len(records):
        raise ValueError("validation: pod location conservation")
    counts = Counter(assignments[:total])
    if any(not 1 <= counts[mc] <= cfg.usable_slots for mc in range(n_mcs)):
        raise ValueError("validation: MC HCP slot limit")
    for h in range(total, len(sizes)):
        if assignments[h] != (h - total) // cfg.reserve_slots:
            raise ValueError("validation: growth slot MC")
    memory = Counter()
    for record in records:
        if record.pod.reserve_kind is None:
            memory[record.pod.hcp] += record.pod.mem_mib
    rolling = set()
    for mc in range(n_mcs):
        occupants = [h for h in range(total) if assignments[h] == mc]
        rolling.update(sorted(occupants, key=lambda h: (-memory[h], h))[:cfg.concurrent_rolling_hcps])
    pairs = defaultdict(list)
    for i, record in enumerate(records):
        if record.pod.reserve_kind == "azdeath":
            source = locations[record.source]
            if source is None:
                raise ValueError("validation: missing working pair pod")
            pairs[source[:2]].append(i)
    death_selected = set()
    fraction = _reserve_fraction(cfg.az_failure_reserve)
    for indices in pairs.values():
        target = fraction * sum(_integer(records[i].pod.mem_mib, 1000, ROUND_CEILING) for i in indices)
        prefix = 0
        for i in indices:
            if prefix < target:
                death_selected.add(i)
            prefix += _integer(records[i].pod.mem_mib, 1000, ROUND_FLOOR)
        # Independently check the requested, unrounded fraction in original units.
        requested = Fraction(str(cfg.az_failure_reserve)) * sum(
            Fraction(str(records[i].pod.mem_mib)) for i in indices)
        reserved = sum(Fraction(str(records[i].pod.mem_mib))
                       for i in indices if locations[i] is not None)
        if reserved < requested:
            raise ValueError("validation: AZ-death reserve memory coverage")
    expected = [[[], [], [], []] for _ in range(n_mcs)]
    zones = defaultdict(list)
    for i, (record, location) in enumerate(zip(records, locations)):
        pod = record.pod
        selected = (pod.hcp in rolling if pod.reserve_kind == "rollout" else
                    i in death_selected if pod.reserve_kind == "azdeath" else True)
        if selected != (location is not None):
            raise ValueError("validation: mandatory/reserve pod conservation")
        if location is None:
            continue
        mc, group, _ = location
        if mc != assignments[pod.hcp] or (group < 3) != record.zonal:
            raise ValueError("validation: HCP split or pod role")
        if pod.reserve_kind == "azdeath" and location[:2] != locations[record.source][:2]:
            raise ValueError("validation: AZ-death reserve location")
        if record.zonal and pod.reserve_kind in (None, "slot"):
            zones[pod.key].append(group)
        expected[mc][group].append(pod)
    for azs in zones.values():
        counts = Counter(azs)
        if max(counts[a] for a in range(3)) - min(counts[a] for a in range(3)) > 1:
            raise ValueError("validation: zonal replica spread")
    _validate(expected, packed, cfg, skus)


def _integer(value, scale, rounding):
    if not math.isfinite(value) or value < 0:
        raise ValueError(f"resource values must be finite and nonnegative: {value}")
    return int((Decimal(str(value)) * scale).to_integral_value(rounding=rounding))


def _validate(groups, packed, cfg, skus):
    """Check concrete inventory independently of CP variables/rounded demands."""
    by_name = {s.name: s for s in skus}
    regional_roles = [set(), set()]
    if len(groups) != len(packed):
        raise ValueError("validation: MC conservation")
    for expected, actual in zip(groups, packed):
        if len(actual) != 4:
            raise ValueError("validation: missing pool")
        inventories = [Counter(n.sku for n in az) for az in actual[:3]]
        if not inventories[0] == inventories[1] == inventories[2]:
            raise ValueError("validation: per-SKU AZ balance")
        replica_azs = defaultdict(list)
        for group, (pods, nodes) in enumerate(zip(expected, actual)):
            if Counter(map(id, pods)) != Counter(id(p) for n in nodes for p in n.pods):
                raise ValueError("validation: pod conservation or fixed MC/AZ/role")
            for node in nodes:
                regional_roles[int(group == 3)].add(node.sku)
                cap = exact.node_caps(cfg, by_name[node.sku])[-1]
                if node.cap != cap:
                    raise ValueError("validation: SKU capacity")
                if len({p.key for p in node.pods}) != len(node.pods):
                    raise ValueError("validation: host anti-affinity")
                for dim in exact.DIMS:
                    used = sum(p.dim(dim) for p in node.pods)
                    if used > cap[dim] + 1e-6 or abs(used - node.used[dim]) > 1e-6:
                        raise ValueError(f"validation: {dim} capacity/accounting")
                for pod in node.pods:
                    if (group < 3 and pod.reserve_kind in (None, "slot")
                            and pod.tier in ("zonal_etcd", "zonal_pair")):
                        replica_azs[pod.key].append(group)
        for azs in replica_azs.values():
            if len(set(azs)) != len(azs):
                raise ValueError("validation: replica AZ anti-affinity")
    if regional_roles[0] & regional_roles[1]:
        raise ValueError("validation: regional SKU disjointness")


def solve_region(distribution, cfg, model, skus, time_limit=30, workers=8, seed=1,
                 *, slots_per_group=None, slot_factor=2.0, slot_slack=2,
                 max_pods=10000, max_model_size=200000, cp_solver=None,
                 use_lns=True, log_search=False, mode="full"):
    """Return the engine result shape plus ``solver`` metadata and explicit scope.

    slots_per_group: optional positive integer cap for EACH MC/AZ and overflow
    pool (including balancing-only nodes). Full mode uses uniform-sized groups
    so division of the global node index identifies the MC/AZ. Derived bounds
    use average regional potential demand plus slack, not a pre-packed shard.
    mode="pinned" fixes sharding and AZs for comparison with the original solver.
    max_model_size limits estimated model units
    (pod variables plus node variables, SKU indicators and table entries).
    log_search also prints construction stage timings to stderr before solving;
    solver.build_stage_seconds records elapsed times at these stage boundaries.
    cp_solver: optional caller-provided ortools CpSolver; search settings above
    override its corresponding parameters. Errors/no incumbent return an error,
    region=None, management_clusters=[] and solver.status. Invalid arguments or
    failed independent validation raise ValueError. Packing is always included.
    """
    started = time.perf_counter()
    if mode not in ("full", "pinned"):
        raise ValueError("mode must be full or pinned")
    if any(not isinstance(v, int) or isinstance(v, bool) or v < 0
           for v in distribution.values()):
        raise ValueError("distribution counts must be nonnegative integers")
    distribution = {s: n for s, n in distribution.items() if n}
    if (not math.isfinite(time_limit) or time_limit < 0
            or not isinstance(workers, int) or workers < 1
            or not math.isfinite(slot_factor) or slot_factor < 1
            or not isinstance(slot_slack, int) or slot_slack < 0
            or not isinstance(max_pods, int) or max_pods < 1
            or not isinstance(max_model_size, int) or max_model_size < 1):
        raise ValueError("invalid search limits")
    if slots_per_group is not None and (
            not isinstance(slots_per_group, int) or slots_per_group < 1):
        raise ValueError("slots_per_group must be a positive integer")
    if (not isinstance(cfg.reserve_slots, int) or cfg.reserve_slots < 0
            or not isinstance(cfg.hcps_per_mc, int) or cfg.hcps_per_mc <= cfg.reserve_slots):
        raise ValueError("MC slots must leave room for real HCPs")
    if (not isinstance(cfg.concurrent_rolling_hcps, int) or cfg.concurrent_rolling_hcps < 0
            or not math.isfinite(cfg.az_failure_reserve) or not 0 <= cfg.az_failure_reserve <= 1):
        raise ValueError("rollout count must be nonnegative and AZ reserve fraction in [0, 1]")
    total = sum(distribution.values())
    stats = {"status": "NOT_RUN", "build_time_seconds": 0.0,
             "solve_time_seconds": 0.0, "runtime_seconds": 0.0,
             "objective": None, "best_objective_bound": None,
             "workers": workers, "seed": seed, "use_lns": use_lns,
             "mode": mode,
             "build_stage_seconds": {},
             "search_space_bounded": True, "validated": False,
             "scope": {"hcp_mc_sharding": "fixed_distribute_conserving",
                       "pod_az_and_role": "fixed_build_mc",
                       "reserve_demand": "fixed_build_mc",
                       "sku_disjointness": "region",
                       "objective": "lexicographic_zonal_then_overflow_cores",
                       "resource_scale": {"cpu": 1000, "mem": 1000,
                                          "nic": 1, "pods": 1}}}
    if mode == "full":
        stats["scope"].update(
            hcp_mc_sharding="joint", pod_az_and_role="joint_AZ_policy_fixed_role",
            mc_count="fixed_minimum_from_usable_slots",
            reserve_demand="dynamic_per_MC_topK_rollout_and_per_AZ_pair_prefix",
            az_reserve_fraction=str(_reserve_fraction(cfg.az_failure_reserve)),
            az_failure_guarantee="packable_headroom_not_failure_scenario_proof")
    result = {"policy": cfg.policy, "total_hcps": total, "n_mcs": 0,
              "per_mc_hcps": cfg.usable_slots, "region": None,
              "management_clusters": [], "solver": stats}

    def fail(status, message):
        stats["status"] = status
        stats["runtime_seconds"] = time.perf_counter() - started
        if not stats["build_time_seconds"]:
            stats["build_time_seconds"] = stats["runtime_seconds"]
        result["error"] = message
        return result

    def stage(name):
        elapsed = time.perf_counter() - started
        stats["build_stage_seconds"][name] = elapsed
        if log_search:
            print(f"CP-SAT construction: {name} at {elapsed:.3f}s", file=sys.stderr, flush=True)

    if not total:
        return fail("EMPTY_FLEET", "empty fleet")
    try:
        from ortools.sat.python import cp_model
    except ImportError:
        return fail("DEPENDENCY_MISSING", "CP-SAT requires ortools")

    # Guard before expanding either the shard list or concrete reserve pods.
    n_mcs = math.ceil(total / cfg.usable_slots)
    result["n_mcs"] = n_mcs
    if n_mcs > max_model_size:
        return fail("MODEL_TOO_LARGE", "MC count exceeds max_model_size")
    demand_pods = {}
    cluster_demands = {}
    for size in set(distribution) | ({cfg.reserve_size} if cfg.reserve_slots else set()):
        cd = model.cluster_demand(size, cfg.policy, cfg.percentile, cfg.multiplier,
                                  unsteered_placement=cfg.unsteered_placement)
        demand_pods[size] = sum(c.replicas for c in cd.components)
        cluster_demands[size] = cd
    real_pods = sum(demand_pods[s] * n for s, n in distribution.items())
    # Each real pod can generate at most one rollout and one AZ-death copy.
    estimate = (real_pods * (1 + int(cfg.concurrent_rolling_hcps > 0)
                             + int(cfg.az_failure_reserve > 0))
                + n_mcs * cfg.reserve_slots * demand_pods.get(cfg.reserve_size, 0))
    if estimate > max_pods:
        return fail("MODEL_TOO_LARGE", f"pod upper estimate {estimate} exceeds max_pods={max_pods}")
    if mode == "full" and total * n_mcs * 12 > max_model_size:
        return fail("MODEL_TOO_LARGE", "HCP/MC membership model exceeds max_model_size")
    if mode == "full":
        sizes, records = _full_workload(distribution, cfg, cluster_demands, n_mcs)
        # Only used to derive regional candidate-slot bounds, never to pin pods.
        groups = [[[r.pod for r in records if r.zonal], [], [],
                   [r.pod for r in records if not r.zonal]]]
    else:
        shards = exact.distribute_conserving(distribution, cfg.usable_slots)
        groups = []
        for shard in shards:
            zonal, overflow = exact.build_mc(
                model, shard, cfg.policy, cfg.percentile, cfg.multiplier,
                cfg.unsteered_placement, cfg.az_failure_reserve,
                cfg.concurrent_rolling_hcps, cfg.reserve_slots, cfg.reserve_size)
            groups.append([*zonal, overflow])
    stats["pod_count"] = sum(len(p) for mc in groups for p in mc)
    skus = sorted(skus, key=lambda s: (s.vcpu, s.memory_gib, s.name))
    if len({s.name for s in skus}) != len(skus):
        raise ValueError("SKU names must be unique")
    scales = [1000, 1000, 1, 1]
    capacities = [[0] * 4]
    candidates = []
    for sku in skus:
        if not isinstance(sku.vcpu, int) or sku.vcpu <= 0:
            raise ValueError("SKU cores must be positive integers")
        cap = exact.node_caps(cfg, sku)[-1]
        if any(cap[d] < 0 for d in exact.DIMS):
            continue  # SKU cannot even hold configured node overhead.
        capacities.append([_integer(cap[d], scale, ROUND_FLOOR)
                           for d, scale in zip(exact.DIMS, scales)])
        candidates.append(sku)
    if not candidates:
        return fail("INFEASIBLE", "no usable SKU capacities")
    maxima = [max(c[d] for c in capacities) for d in range(4)]
    demands = {}
    bounds = []
    for mc in groups:
        lower = []
        for pods in mc:
            for pod in pods:
                demands[id(pod)] = [_integer(pod.dim(d), scale, ROUND_CEILING)
                                    for d, scale in zip(exact.DIMS, scales)]
                if (mode == "pinned" or pod.reserve_kind in (None, "slot")) and not any(
                           all(a <= b for a, b in zip(demands[id(pod)], cap))
                           for cap in capacities[1:]):
                    return fail("INFEASIBLE", "a rounded pod exceeds every candidate SKU")
            resource_lb = [math.ceil(sum(demands[id(p)][d] for p in pods) / maxima[d])
                           if maxima[d] else 0 for d in range(4)]
            lower.append(max(*resource_lb, max(Counter(p.key for p in pods).values(), default=0)))
        zpods, opods = sum(map(len, mc[:3])), len(mc[3])
        zbound = (slots_per_group if slots_per_group is not None else
                  math.ceil(max(lower[:3]) * slot_factor) + slot_slack)
        obound = (slots_per_group if slots_per_group is not None else
                  math.ceil(lower[3] * slot_factor) + slot_slack)
        # Total zonal pods is a safe ceiling even when balancing mixed SKUs.
        bounds.append([min(zbound, zpods)] * 3 + [min(obound, opods)])
    if mode == "full":
        if slots_per_group is None:
            average_lb = max(
                (math.ceil(sum(demands[id(p)][d] for p in pool) / (maxima[d] * divisor))
                 if maxima[d] else 0)
                for pool, divisor in ((groups[0][0], 3 * n_mcs), (groups[0][3], n_mcs))
                for d in range(4))
            replica_lb = max(Counter(r.pod.key for r in records if not r.zonal).values(), default=1)
            slots = min(max(1, len(records)),
                        math.ceil(max(average_lb, replica_lb) * slot_factor) + slot_slack)
        else:
            slots = slots_per_group
        bounds = [[slots] * 4 for _ in range(n_mcs)]
        groups = [[[], [], [], []] for _ in range(n_mcs)]
    stats["slots_per_mc_group"] = bounds
    node_count = sum(map(sum, bounds))
    model_units = stats["pod_count"] * 6 + node_count * (30 + 9 * len(candidates))
    if mode == "full":
        pair_count = sum(r.pod.reserve_kind == "azdeath" for r in records)
        model_units += (stats["pod_count"] * 16 + total * n_mcs * 12
                        + pair_count * 3 * n_mcs * 16)
    stats.update(candidate_nodes=node_count, estimated_model_size=model_units,
                  candidate_skus=[s.name for s in candidates])
    stage("size_estimated")
    if log_search:
        print(f"CP-SAT model estimate: {model_units} units, {stats['pod_count']} potential pods, "
              f"{node_count} candidate nodes (limit {max_model_size})", file=sys.stderr, flush=True)
    if model_units > max_model_size:
        return fail("MODEL_TOO_LARGE", f"estimated model size {model_units} exceeds max_model_size={max_model_size}")

    stage("nodes_started")
    problem = cp_model.CpModel()
    role = [problem.new_bool_var(f"sku_{s.name}_zonal") for s in candidates]
    cores = [0] + [s.vcpu for s in candidates]
    table = [[i, cores[i], *cap] for i, cap in enumerate(capacities)]
    intervals, resource_demands = [], [[] for _ in exact.DIMS]
    node_vars, pod_vars, costs = [], [], [[], []]
    offset = 0
    for mi, (mc, mc_bounds) in enumerate(zip(groups, bounds)):
        mc_nodes, mc_pods, counts = [], [], []
        keys = defaultdict(list)
        for gi, (pods, bound) in enumerate(zip(mc, mc_bounds)):
            group_nodes, group_pods = [], []
            indicators = [[] for _ in candidates]
            for ni in range(bound):
                name = f"m{mi}g{gi}n{ni}"
                choice = problem.new_int_var(0, len(candidates), name)
                core = problem.new_int_var(0, max(cores), name + "_cores")
                caps = [problem.new_int_var(0, maximum, name + f"_cap{d}")
                        for d, maximum in enumerate(maxima)]
                problem.add_allowed_assignments([choice, core, *caps], table)
                for si in range(len(candidates)):
                    selected = problem.new_bool_var(name + f"_sku{si + 1}")
                    problem.add(choice == si + 1).only_enforce_if(selected)
                    problem.add(choice != si + 1).only_enforce_if(selected.Not())
                    problem.add_implication(selected, role[si] if gi < 3 else role[si].Not())
                    indicators[si].append(selected)
                if group_nodes:
                    problem.add(group_nodes[-1] >= choice)  # interchangeable slots
                group_nodes.append(choice)
                costs[int(gi == 3)].append(core)
                intervals.append(problem.new_fixed_size_interval_var(offset + ni, 1, name + "_block"))
                for d, maximum in enumerate(maxima):
                    resource_demands[d].append(maximum - caps[d])
            for pi, pod in enumerate(pods):
                if not bound:
                    return fail("INFEASIBLE", "no candidate slots for nonempty pool")
                node = problem.new_int_var(offset, offset + bound - 1, f"m{mi}g{gi}p{pi}")
                intervals.append(problem.new_fixed_size_interval_var(node, 1, f"m{mi}g{gi}p{pi}_interval"))
                for d in range(4):
                    resource_demands[d].append(demands[id(pod)][d])
                keys[pod.key].append(node)
                group_pods.append((pod, node, offset))
            counts.append([sum(values) for values in indicators])
            mc_nodes.append(group_nodes)
            mc_pods.append(group_pods)
            offset += bound
        for si in range(len(candidates)):
            problem.add(counts[0][si] == counts[1][si])
            problem.add(counts[0][si] == counts[2][si])
        for replicas in keys.values():
            if len(replicas) > 1:
                problem.add_all_different(replicas)
        node_vars.append(mc_nodes)
        pod_vars.append(mc_pods)
    if mode == "full":
        stage("full_placement_started")
        hcp_mc, full_variables = _add_full_placement(
            problem, cp_model, records, sizes, total, cfg, n_mcs, slots,
            demands, intervals, resource_demands)
    for d, maximum in enumerate(maxima):
        problem.add_cumulative(intervals, resource_demands[d], maximum)
    weight = sum(b[3] for b in bounds) * max(cores) + 1
    if node_count * max(cores) * weight >= 2**62:
        raise ValueError("objective exceeds safe CP-SAT integer range")
    problem.minimize(weight * sum(costs[0]) + sum(costs[1]))
    stats["objective_zonal_weight"] = weight
    stage("validation_started")
    validation_error = problem.validate()
    if validation_error:
        return fail("MODEL_INVALID", validation_error)
    solver = cp_solver if cp_solver is not None else cp_model.CpSolver()
    solver.parameters.max_time_in_seconds = time_limit
    solver.parameters.num_search_workers = workers
    solver.parameters.random_seed = seed
    solver.parameters.use_lns = use_lns
    solver.parameters.log_search_progress = log_search
    stats["build_time_seconds"] = time.perf_counter() - started
    stage("solve_started")
    solve_started = time.perf_counter()
    status = solver.solve(problem)
    stats.update(status=solver.status_name(status),
                 solve_time_seconds=time.perf_counter() - solve_started,
                 best_objective_bound=solver.best_objective_bound,
                 branches=solver.num_branches, conflicts=solver.num_conflicts)
    if status not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return fail(stats["status"], "no CP-SAT incumbent in the bounded search space")

    packed = []
    if mode == "full":
        inventory = [[[exact.new_node(cfg, candidates[solver.value(v) - 1])
                       if solver.value(v) else None for v in choices]
                      for choices in mc] for mc in node_vars]
        assignments = [solver.value(v) for v in hcp_mc]
        locations = []
        for record, (variable, _, presence) in zip(records, full_variables):
            if presence is not None and not solver.value(presence):
                locations.append(None)
                continue
            index = solver.value(variable)
            mc, local = divmod(index, 4 * slots)
            group, ni = divmod(local, slots)
            node = inventory[mc][group][ni]
            if node is None:
                raise ValueError("validation: pod on absent node")
            node.place(record.pod)
            locations.append((mc, group, ni))
        packed = [[[n for n in group if n is not None] for group in mc] for mc in inventory]
        _validate_full(records, sizes, total, cfg, n_mcs, assignments, locations, packed, candidates)
        shards = [dict(Counter(sizes[h] for h in range(total) if assignments[h] == mc))
                  for mc in range(n_mcs)]
        result["hcp_assignments"] = [{"hcp": h, "hcp_size": sizes[h], "mc": assignments[h]}
                                     for h in range(total)]
        stats["active_pod_count"] = sum(location is not None for location in locations)
    else:
        for mc_nodes, mc_pods in zip(node_vars, pod_vars):
            actual = []
            for choices, placements in zip(mc_nodes, mc_pods):
                nodes = [exact.new_node(cfg, candidates[solver.value(v) - 1])
                         if solver.value(v) else None for v in choices]
                for pod, variable, start in placements:
                    node = nodes[solver.value(variable) - start]
                    if node is None:
                        raise ValueError("validation: pod on absent node")
                    node.place(pod)
                actual.append([n for n in nodes if n is not None])
            packed.append(actual)
        _validate(groups, packed, cfg, candidates)
    if Counter({s: sum(mc.get(s, 0) for mc in shards) for s in distribution}) != Counter(distribution):
        raise ValueError("validation: HCP conservation")
    for shard, actual in zip(shards, packed):
        zstats = exact._pool_stats([n for az in actual[:3] for n in az], 3, "zonal")
        ostats = exact._pool_stats(actual[3], cfg.overflow_az_count, "overflow")
        ostats["util_nic"] = None
        result["management_clusters"].append({
            "kind": "full" if sum(shard.values()) == cfg.usable_slots else "remainder",
            "hcps": sum(shard.values()), "count": 1, "hcp_mix": shard,
            "zonal_pool": zstats, "overflow_pool": ostats,
            "zonal_cores": zstats["total_cores"], "overflow_cores": ostats["total_cores"],
            "packing": {"zonal": [[exact.node_to_json(n) for n in az] for az in actual[:3]],
                        "overflow": [exact.node_to_json(n) for n in actual[3]]}})
    region = {f"{role}_{metric}": sum(mc[f"{role}_pool"][f"total_{metric}"]
                                     for mc in result["management_clusters"])
              for role in ("zonal", "overflow") for metric in ("cores", "nodes")}
    region["hcp_per_zonal_core"] = total / region["zonal_cores"] if region["zonal_cores"] else None
    result["region"] = region
    objective = weight * region["zonal_cores"] + region["overflow_cores"]
    if abs(objective - solver.objective_value) > 0.5:
        raise ValueError("validation: objective accounting")
    stats.update(objective=objective, validated=True,
                 runtime_seconds=time.perf_counter() - started)
    return result
