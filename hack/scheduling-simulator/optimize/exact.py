"""Exact packing via First-Fit-Decreasing (FFD) vector bin-packing.

Expands the real pod list for a management cluster and packs pods into nodes one
at a time, capturing indivisible large pods, SWIFT-NIC integrality, zone-spread
(distinct AZs; etcd strictly 1/AZ), host-spread (same (hcp,component) never shares
a node), node-role (zonal vs overflow), and the pods-per-node cap.

Headroom is modeled as *packable reserved pods* (not a node multiplier):
  * rollout surge  : the largest HCP(s) on the MC each surge +1 pod per multi-replica
                     Deployment (maxSurge=1); these reserved copies are packed too.
  * AZ-death        : each zonal AZ reserves a fraction of its non-etcd pair footprint
                     (copies of real pair pods) so a sibling AZ's pods can reschedule in.
Reserves are placed into existing node slack first; a new node is opened only when
they overflow. The retained Node objects (with their placed pods) drive the tetris
visualization.
"""
import math
from collections import Counter
from dataclasses import dataclass, field
from typing import Dict, List, Tuple, Optional

from skus import SKU
from demand.model import DemandModel
from demand import components as comp


@dataclass
class Pod:
    hcp: int
    hcp_size: str
    component: str
    tier: str
    cpu_mc: float
    mem_mib: float
    nic: int
    key: str                      # (hcp, component) identity for host-spread
    reserve_kind: Optional[str] = None  # None | "rollout" | "azdeath"

    def dim(self, d):
        return {"cpu": self.cpu_mc, "mem": self.mem_mib, "nic": self.nic, "pods": 1}[d]


DIMS = ("cpu", "mem", "nic", "pods")


@dataclass
class Node:
    sku: str
    cap: dict        # usable (packable) capacity per DIM
    full: dict       # full node capacity per DIM
    system: dict     # kubelet system-reserved per DIM
    daemonset: dict  # non-HCP daemonset overhead per DIM
    buffer: dict     # node buffer (held free) per DIM
    used: dict = field(default_factory=lambda: {d: 0.0 for d in DIMS})
    keys: set = field(default_factory=set)
    pods: List["Pod"] = field(default_factory=list)

    def fits(self, p: "Pod") -> bool:
        if p.key in self.keys:
            return False
        for d in DIMS:
            if self.used[d] + p.dim(d) > self.cap[d] + 1e-6:
                return False
        return True

    def place(self, p: "Pod"):
        for d in DIMS:
            self.used[d] += p.dim(d)
        self.keys.add(p.key)
        self.pods.append(p)


def apportion(distribution: Dict[str, int], target: int) -> Dict[str, int]:
    """Largest-remainder apportionment of a size distribution down to `target` HCPs."""
    total = sum(distribution.values())
    if total == 0 or target <= 0:
        return {k: 0 for k in distribution}
    raw = {k: v * target / total for k, v in distribution.items()}
    floor = {k: int(math.floor(x)) for k, x in raw.items()}
    remaining = target - sum(floor.values())
    rema = sorted(distribution, key=lambda k: raw[k] - floor[k], reverse=True)
    for k in rema[:remaining]:
        floor[k] += 1
    return floor


def distribute_conserving(distribution: Dict[str, int], usable: int) -> List[Dict[str, int]]:
    """Split the *actual* integer fleet across management clusters so that EVERY
    requested cluster is scheduled exactly once (counts are conserved), each full
    MC holds `usable` HCPs, and a trailing remainder MC holds the rest.

    Unlike a per-MC apportionment (which floors independently and silently drops
    rare large clusters), this deals the real multiset: `usable`-sized MCs are made
    as identical as possible while the leftover clusters — including the scarce big
    ones — are spread across MCs so the totals add back up to the request.

    Returns a list of per-MC size dicts (the last one may be a smaller remainder).
    """
    dist = {k: int(v) for k, v in distribution.items() if int(v) > 0}
    total = sum(dist.values())
    if total == 0:
        return []
    usable = max(1, int(usable))
    if total <= usable:
        return [dict(dist)]                      # single MC holds the whole fleet

    n_full = total // usable
    rem = total - n_full * usable

    # Carve the remainder MC first (proportional slice), leaving the rest for full MCs.
    remainder_mc = apportion(dist, rem) if rem else {}
    remaining = {k: dist.get(k, 0) - remainder_mc.get(k, 0) for k in dist}

    # Identical base chunk per full MC, then a small per-MC top-up from the pool.
    base = {k: remaining[k] // n_full for k in remaining}
    pool = {k: remaining[k] - base[k] * n_full for k in remaining}  # 0 <= pool[k] < n_full
    d = usable - sum(base.values())              # items each full MC still needs

    full_mcs = [dict(base) for _ in range(n_full)]
    # Flatten the pool largest-size-first and fill each MC's top-up to `d` in turn;
    # contiguous filling keeps adjacent MCs identical -> good de-duplication.
    pool_items = []
    for size in sorted(pool, key=lambda s: -int(s)):
        pool_items += [size] * pool[size]
    i = 0
    for mc in full_mcs:
        for _ in range(d):
            s = pool_items[i]; i += 1
            mc[s] = mc.get(s, 0) + 1
    # Any leftover pool items belong to the remainder MC (already sized to `rem`).
    out = [{k: v for k, v in mc.items() if v} for mc in full_mcs]
    if rem:
        out.append({k: v for k, v in remainder_mc.items() if v})
    return out


def group_shapes(mcs: List[Dict[str, int]]) -> List[Tuple[Dict[str, int], int]]:
    """Collapse identical per-MC size dicts into (dict, count), preserving order of
    first appearance."""
    order: List[Tuple[Tuple, Dict[str, int]]] = []
    counts: Dict[Tuple, int] = {}
    for mc in mcs:
        key = tuple(sorted((k, v) for k, v in mc.items()))
        if key not in counts:
            counts[key] = 0
            order.append((key, mc))
        counts[key] += 1
    return [(mc, counts[key]) for key, mc in order]



# ---------------------------------------------------------------------------
# Build the per-MC pod sets (working + reserve), split into zonal-by-AZ + overflow
# ---------------------------------------------------------------------------

def build_mc(model, distribution, policy, percentile, multiplier,
             unsteered_placement="overflow", az_failure_reserve=0.5,
             concurrent_rolling_hcps=1, reserve_slots=0, reserve_size=None):
    """Return (zonal_by_az[3], overflow) lists of Pod, including reserve pods."""
    zonal_by_az: List[List[Pod]] = [[], [], []]
    overflow: List[Pod] = []
    hcp_pods: Dict[int, List[Pod]] = {}   # working pods per hcp (for rollout surge)
    hcp_mem: Dict[int, float] = {}        # total mem per hcp (to pick the largest)
    hcp_id = 0
    rot = 0
    for size, count in distribution.items():
        if count <= 0:
            continue
        cd = model.cluster_demand(size, policy, percentile, multiplier,
                                  unsteered_placement=unsteered_placement)
        for _ in range(count):
            mine: List[Pod] = []
            for c in cd.components:
                for r in range(c.replicas):
                    p = Pod(hcp_id, size, c.name, c.tier, c.cpu_mc, c.mem_mib, c.nic,
                            f"{hcp_id}:{c.name}")
                    mine.append(p)
                    if c.zonal:
                        az = (rot + r) % 3
                        zonal_by_az[az].append(p)
                    else:
                        overflow.append(p)
                if c.zonal:
                    rot = (rot + 1) % 3  # rotate start per component for global balance
            hcp_pods[hcp_id] = mine
            hcp_mem[hcp_id] = sum(p.mem_mib for p in mine)
            hcp_id += 1

    _add_rollout_reserve(hcp_pods, hcp_mem, zonal_by_az, overflow, concurrent_rolling_hcps)
    _add_azdeath_reserve(zonal_by_az, az_failure_reserve)

    # Reserved HCP slots: explicit, always-held headroom of `reserve_slots` clusters
    # of a representative `reserve_size`. Built like real HCPs but flagged "slot" so
    # they are drawn distinctly and excluded from rollout/AZ-death selection.
    if reserve_slots and reserve_size is not None:
        cd = model.cluster_demand(reserve_size, policy, percentile, multiplier,
                                  unsteered_placement=unsteered_placement)
        for _ in range(int(reserve_slots)):
            for c in cd.components:
                for r in range(c.replicas):
                    p = Pod(hcp_id, reserve_size, c.name, c.tier, c.cpu_mc, c.mem_mib,
                            c.nic, f"{hcp_id}:{c.name}", reserve_kind="slot")
                    if c.zonal:
                        zonal_by_az[(rot + r) % 3].append(p)
                    else:
                        overflow.append(p)
                if c.zonal:
                    rot = (rot + 1) % 3
            hcp_id += 1
    return zonal_by_az, overflow


def _add_rollout_reserve(hcp_pods, hcp_mem, zonal_by_az, overflow, n_hcps):
    """Largest n_hcps HCPs each surge +1 pod per multi-replica Deployment."""
    if n_hcps <= 0 or not hcp_mem:
        return
    largest = sorted(hcp_mem, key=lambda h: -hcp_mem[h])[:n_hcps]
    az_fill = [len(z) for z in zonal_by_az]
    for h in largest:
        # count replicas per (component) to know which are multi-replica Deployments
        by_comp: Dict[str, List[Pod]] = {}
        for p in hcp_pods[h]:
            by_comp.setdefault(p.component, []).append(p)
        for cname, reps in by_comp.items():
            if len(reps) < 2 or comp.is_statefulset(cname):
                continue  # only multi-replica Deployments surge
            proto = reps[0]
            surge = Pod(proto.hcp, proto.hcp_size, proto.component, proto.tier,
                        proto.cpu_mc, proto.mem_mib, proto.nic, proto.key,
                        reserve_kind="rollout")
            if proto.tier in ("zonal_etcd", "zonal_pair"):
                az = min(range(3), key=lambda i: az_fill[i])  # least-loaded AZ
                zonal_by_az[az].append(surge); az_fill[az] += 1
            else:
                overflow.append(surge)


def _add_azdeath_reserve(zonal_by_az, fraction):
    """Each zonal AZ reserves `fraction` of its non-etcd pair footprint as copies of
    real pair pods (so a sibling AZ's pods could reschedule in)."""
    if fraction <= 0:
        return
    for az, pods in enumerate(zonal_by_az):
        pairs = [p for p in pods if p.tier == "zonal_pair" and p.reserve_kind is None]
        target_mem = fraction * sum(p.mem_mib for p in pairs)
        if target_mem <= 0:
            continue
        acc = 0.0
        extra = []
        i = 0
        for p in pairs:
            if acc >= target_mem:
                break
            extra.append(Pod(p.hcp, p.hcp_size, p.component, p.tier, p.cpu_mc, p.mem_mib,
                             p.nic, f"azdeath:{az}:{i}", reserve_kind="azdeath"))
            acc += p.mem_mib
            i += 1
        zonal_by_az[az].extend(extra)


# ---------------------------------------------------------------------------
# Packing
# ---------------------------------------------------------------------------

def node_caps(cfg, sku):
    """Return (full, system, daemonset, buffer, cap-usable) dicts keyed by DIM."""
    full = {"cpu": sku.vcpu * 1000.0, "mem": sku.memory_gib * 1024.0,
            "nic": sku.swift_nic, "pods": cfg.max_pods_per_node}
    sc, sm = cfg.system_reserved(sku)
    system = {"cpu": sc, "mem": sm, "nic": 0.0, "pods": 0.0}
    daemonset = {"cpu": cfg.node_overhead_cpu_mc, "mem": cfg.node_overhead_mem_mib,
                 "nic": 0.0, "pods": cfg.node_overhead_pods}
    buffer = {"cpu": cfg.buffer_cpu * full["cpu"], "mem": cfg.buffer_mem * full["mem"],
              "nic": cfg.buffer_nic * full["nic"], "pods": cfg.buffer_pods * full["pods"]}
    cap = {d: full[d] - system[d] - daemonset[d] - buffer[d] for d in DIMS}
    cap["nic"] = float(math.floor(cap["nic"]))
    cap["pods"] = float(math.floor(cap["pods"]))
    return full, system, daemonset, buffer, cap


def new_node(cfg, sku):
    full, system, daemonset, buffer, cap = node_caps(cfg, sku)
    return Node(sku.name, cap, full, system, daemonset, buffer)


def ffd_pack(pods: List[Pod], sku: SKU, cfg) -> Optional[List[Node]]:
    """First-Fit-Decreasing pack pods into nodes of one SKU. Returns the node list,
    or None if a single pod cannot fit any node of this SKU."""
    if not pods:
        return []
    _, _, _, _, cap = node_caps(cfg, sku)
    if cap["cpu"] <= 0 or cap["mem"] <= 0:
        return None
    def size_key(p):
        return max((p.dim(d) / cap[d]) if cap[d] else 0 for d in DIMS)
    ordered = sorted(pods, key=size_key, reverse=True)
    nodes: List[Node] = []
    for p in ordered:
        placed = False
        for n in nodes:
            if n.fits(p):
                n.place(p); placed = True; break
        if not placed:
            n = new_node(cfg, sku)
            if not n.fits(p):
                return None  # a single pod exceeds a whole node in some dimension
            n.place(p); nodes.append(n)
    return nodes


def _remake_node(node: Node, sku: SKU, cfg) -> Node:
    """Return a copy of `node` re-homed onto a different SKU (same placed pods)."""
    full, system, daemonset, buffer, cap = node_caps(cfg, sku)
    n = Node(sku.name, cap, full, system, daemonset, buffer)
    n.pods = list(node.pods)
    n.used = dict(node.used)
    n.keys = set(node.keys)
    return n


def _best_fit_sku(node: Node, allowed: List[SKU], cfg) -> Optional[SKU]:
    """Smallest-core allowed SKU whose usable capacity still holds this node's pods."""
    best = None
    for s in sorted(allowed, key=lambda s: (s.vcpu, s.memory_gib, s.name)):
        _, _, _, _, cap = node_caps(cfg, s)
        if cap["cpu"] <= 0 or cap["mem"] <= 0:
            continue
        if all(node.used[d] <= cap[d] + 1e-6 for d in DIMS):
            if best is None or s.vcpu < best.vcpu:
                best = s
    return best


def hetero_pack(pods: List[Pod], allowed: List[SKU], cfg) -> Optional[List[Node]]:
    """Heterogeneous pack: FFD into a homogeneous base SKU, then right-size every node
    down to the smallest allowed SKU that still fits it. Try each base and keep the mix
    with the fewest total cores. Returns a (possibly mixed-SKU) node list, or None if a
    single pod cannot fit any allowed SKU."""
    if not pods:
        return []
    if not allowed:
        return None
    allowed = sorted(allowed, key=lambda s: (s.vcpu, s.memory_gib, s.name))
    best_nodes, best_cores = None, None
    for base in allowed:
        packed = ffd_pack(pods, base, cfg)
        if packed is None:
            continue
        rebuilt = []
        ok = True
        for n in packed:
            s = _best_fit_sku(n, allowed, cfg)
            if s is None:
                ok = False
                break
            rebuilt.append(_remake_node(n, s, cfg))
        if not ok:
            continue
        cores = sum(rn.full["cpu"] / 1000.0 for rn in rebuilt)
        if best_cores is None or cores < best_cores:
            best_cores, best_nodes = cores, rebuilt
    return best_nodes


def _cores(nodes: List[Node]) -> float:
    return sum(n.full["cpu"] / 1000.0 for n in nodes)


def _used_skus(node_groups) -> set:
    return {n.sku for grp in node_groups for n in grp}


def _pool_stats(nodes: List[Node], az_count: int, role: str) -> dict:
    """Summarize physical inventory; zonal display counts are per balanced AZ."""
    total_nodes = len(nodes)
    n_per = total_nodes // az_count if role == "zonal" else total_nodes
    used = {d: sum(n.used[d] for n in nodes) for d in DIMS}
    cap = {d: sum(n.cap[d] for n in nodes) for d in DIMS}
    util = {d: (used[d] / cap[d] if cap[d] else 0.0) for d in DIMS}
    binding = max(util.items(), key=lambda kv: kv[1])[0] if any(cap.values()) else "cpu"
    cnt = Counter(n.sku for n in nodes)
    if role == "zonal":
        cnt = Counter({name: count // az_count for name, count in cnt.items()})
    mix = " · ".join(f"{c}× {name.replace('Standard_', '')}" for name, c in cnt.most_common())
    return {"sku": mix or "–", "skus": dict(cnt), "nodes": n_per, "az_count": az_count,
            "total_nodes": total_nodes,
            "total_cores": round(_cores(nodes)),
            "total_mem_gib": round(sum(n.full["mem"] / 1024.0 for n in nodes), 1),
            "util_cpu": util["cpu"], "util_mem": util["mem"],
            "util_nic": util["nic"] if cap["nic"] else None, "util_pods": util["pods"],
            "binding": binding, "role": role}


def _pack_zonal(zonal_by_az, allowed, cfg):
    """Pack each AZ heterogeneously (restricted to `allowed`). Returns (stats, nodes_by_az,
    used_sku_set) or None if infeasible."""
    packed = [hetero_pack(az, allowed, cfg) for az in zonal_by_az]
    if any(p is None for p in packed):
        return None
    counts = [Counter(n.sku for n in az) for az in packed]
    # Balance each SKU independently, retaining the idle capacity in the packing.
    for sku in sorted(allowed, key=lambda s: (s.vcpu, s.memory_gib, s.name)):
        target = max((count[sku.name] for count in counts), default=0)
        for az, count in zip(packed, counts):
            az.extend(new_node(cfg, sku) for _ in range(target - count[sku.name]))
    stats = _pool_stats([n for az in packed for n in az], az_count=3, role="zonal")
    return stats, packed, _used_skus(packed)


def _pack_overflow(overflow_pods, allowed, cfg, az_count):
    nodes = hetero_pack(overflow_pods, allowed, cfg)
    if nodes is None:
        return None
    stats = _pool_stats(nodes, az_count=az_count, role="overflow")
    stats["util_nic"] = None
    return stats, nodes, _used_skus([nodes])


def size_pools_disjoint(zonal_by_az, overflow_pods, skus, cfg):
    """Size the zonal (balanced triplet) and overflow pools with heterogeneous SKU mixes,
    under the Azure constraint that a SKU used zonally cannot also be used in overflow.

    Packs each pool over all SKUs; if they share a SKU, branch (forbid the shared SKU on
    one side) and keep the split that minimises (zonal_cores, overflow_cores) — zonal cores
    are the scarce resource, so they win ties. Returns (zstats, znodes, ostats, onodes)."""
    az_o = cfg.overflow_az_count
    by_name = {s.name: s for s in sorted(skus, key=lambda s: (s.vcpu, s.memory_gib, s.name))}
    zcache, ocache = {}, {}          # keyed only on the pool's own allowed set (decoupled)

    def pack_z(names):
        if names not in zcache:
            zcache[names] = _pack_zonal(zonal_by_az, [s for n, s in by_name.items() if n in names], cfg)
        return zcache[names]

    def pack_o(names):
        if names not in ocache:
            ocache[names] = (_pack_overflow(overflow_pods, [s for n, s in by_name.items() if n in names], cfg, az_o)
                             if overflow_pods else (_pool_stats([], az_o, "overflow"), [], set()))
        return ocache[names]

    memo = {}

    def solve(allowed_z_names, allowed_o_names):
        key = (allowed_z_names, allowed_o_names)
        if key in memo:
            return memo[key]
        zp, op = pack_z(allowed_z_names), pack_o(allowed_o_names)
        if zp is None or op is None:
            memo[key] = None
            return None
        zstats, znodes, zused = zp
        ostats, onodes, oused = op
        conflict = zused & oused
        if not conflict:
            result = (zstats, znodes, ostats, onodes)
            memo[key] = result
            return result
        c = sorted(conflict)[0]
        # Branch: drop the shared SKU from overflow, or from zonal.
        a = solve(allowed_z_names, allowed_o_names - {c})
        b = solve(allowed_z_names - {c}, allowed_o_names)

        def cost(r):
            return (r[0]["total_cores"], r[2]["total_cores"]) if r else (float("inf"),) * 2
        result = min((a, b), key=cost)
        memo[key] = result if cost(result)[0] != float("inf") else None
        return memo[key]

    allnames = frozenset(s.name for s in skus)
    return solve(allnames, allnames) or (None, None, None, None)


def pod_to_json(p: Pod):
    return {"hcp": p.hcp, "hcp_size": p.hcp_size, "component": p.component,
            "tier": p.tier, "cpu_mc": round(p.cpu_mc, 1), "mem_mib": round(p.mem_mib, 1),
            "nic": p.nic, "reserve": p.reserve_kind}


def _res_json(d):
    return {"cpu_mc": round(d["cpu"], 1), "mem_mib": round(d["mem"], 1),
            "nic": round(d["nic"], 3), "pods": round(d["pods"], 2)}


def node_to_json(n: Node):
    return {"sku": n.sku,
            "full": _res_json(n.full),
            "cap": _res_json(n.cap),
            "used": _res_json(n.used),
            "infra": {"system": _res_json(n.system),
                      "daemonset": _res_json(n.daemonset),
                      "buffer": _res_json(n.buffer)},
            "pods": [pod_to_json(p) for p in n.pods]}
