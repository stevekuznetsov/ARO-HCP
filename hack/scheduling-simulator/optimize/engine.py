"""Simulation engine: fleet -> region demand -> management-cluster sharding ->
per-MC node-pool sizing (fast path), with a legacy-vs-minimal comparison.
"""
import math
from dataclasses import dataclass, field, asdict
from typing import Dict, List

from demand.model import DemandModel
from skus import SKU, load_catalog
from . import fast
from . import exact


@dataclass
class RunConfig:
    policy: str = "minimal"            # "minimal" | "legacy"
    percentile: float = 75.0           # usage -> request percentile
    multiplier: float = 1.0            # usage -> request multiplier
    hcps_per_mc: int = 100             # hard cap of HCP slots per management cluster
    # Explicit, always-held headroom: `reserve_slots` of the `hcps_per_mc` slots on
    # every MC are kept as reserved placeholder HCPs of a representative `reserve_size`
    # (a stand-in for near-future growth). Usable real slots = hcps_per_mc - reserve_slots.
    reserve_slots: int = 5             # reserved placeholder HCPs held on every MC
    reserve_size: str = "30"           # worker-node size of each reserved placeholder
    # System reservation (kubelet --system-reserved). ARO-HCP pins a FLAT
    # cpu=3000m, memory=7550Mi on every worker node regardless of size.
    reservation_mode: str = "scaled"   # "flat" | "scaled" (scale reservation by vCPU)
    system_reserved_cpu_mc: float = 3000.0
    system_reserved_mem_mib: float = 7550.0
    scaled_reference_vcpu: int = 32    # in "scaled" mode the flat value applies at this vCPU
    # Non-HCP workload overhead per worker node (kube-system daemonsets, arobit,
    # velero, mgmt-agent, ...). Runs as PODS consuming allocatable, so it is
    # additional to --system-reserved. Defaults measured from prod-uksouth-mgmt-1
    # (cluster-utilization report, 14d p95). Always flat (daemonsets don't scale
    # with node size).
    node_overhead_cpu_mc: float = 468.0
    node_overhead_mem_mib: float = 3592.0
    node_overhead_pods: int = 11
    # Per-resource node buffer: a fraction of each node's FULL capacity held free
    # (headroom you never pack into). Default: 10% memory, 0 for the rest.
    buffer_cpu: float = 0.0
    buffer_mem: float = 0.10
    buffer_nic: float = 0.0
    buffer_pods: float = 0.0
    az_failure_reserve: float = 0.50   # fraction of each AZ's non-etcd pair load reserved for AZ death
    rollout_surge: float = 0.15        # (fast-path only) extra headroom fraction for rolling updates
    concurrent_rolling_hcps: int = 1   # exact: # of largest HCPs whose rollout surge is reserved
    overflow_az_count: int = 1         # AZ span of the overflow pool(s)
    max_pods_per_node: int = 225       # kubelet/CNI pods-per-node cap
    unsteered_placement: str = "overflow"  # where CNO-managed/CSI (no node-role) pods land: "overflow"|"zonal"
    mode: str = "fast"                 # "fast" (closed-form) | "exact" (FFD bin-packing)

    @property
    def usable_pods_per_node(self):
        return max(int(round(self.max_pods_per_node * (1.0 - self.buffer_pods)))
                   - self.node_overhead_pods, 1)

    @property
    def usable_slots(self):
        """Real HCP slots per MC after holding back the reserved placeholders."""
        return max(1, int(self.hcps_per_mc) - int(self.reserve_slots))

    def usable_nic(self, sku):
        import math
        return int(math.floor(sku.swift_nic * (1.0 - self.buffer_nic)))

    def system_reserved(self, sku):
        """kubelet --system-reserved (cpu_mc, mem_mib), flat or scaled by vCPU."""
        if self.reservation_mode == "scaled":
            f = sku.vcpu / max(self.scaled_reference_vcpu, 1)
            return self.system_reserved_cpu_mc * f, self.system_reserved_mem_mib * f
        return self.system_reserved_cpu_mc, self.system_reserved_mem_mib

    def reserved(self, sku):
        """Total absolute (cpu_mc, mem_mib) unavailable to HCP pods on a node of this
        SKU: kubelet --system-reserved + non-HCP daemonset overhead + the node buffer."""
        sc, sm = self.system_reserved(sku)
        full_cpu, full_mem = sku.vcpu * 1000.0, sku.memory_gib * 1024.0
        return (sc + self.node_overhead_cpu_mc + self.buffer_cpu * full_cpu,
                sm + self.node_overhead_mem_mib + self.buffer_mem * full_mem)


def _add(dst, src):
    for k, v in src.items():
        dst[k] = dst.get(k, 0.0) + v


def region_demand(model: DemandModel, distribution: Dict[str, int], cfg: RunConfig):
    """Aggregate zonal + overflow + zonal-pair demand over the whole region's fleet."""
    zonal = {"cpu_mc": 0.0, "mem_mib": 0.0, "nic": 0.0, "pods": 0.0}
    overflow = {"cpu_mc": 0.0, "mem_mib": 0.0, "nic": 0.0, "pods": 0.0}
    zpair = {"cpu_mc": 0.0, "mem_mib": 0.0, "nic": 0.0, "pods": 0.0}
    total_hcps = 0
    for size, count in distribution.items():
        if count <= 0:
            continue
        cd = model.cluster_demand(size, cfg.policy, cfg.percentile, cfg.multiplier,
                                  unsteered_placement=cfg.unsteered_placement)
        z, o, zp = cd.zonal(), cd.overflow(), cd.zonal_pair()
        for _ in range(count):
            _add(zonal, z)
            _add(overflow, o)
            _add(zpair, zp)
        total_hcps += count
    return zonal, overflow, zpair, total_hcps


def _scale(vec, f):
    return {k: v * f for k, v in vec.items()}


def apportion_dist(distribution, target):
    return exact.apportion(distribution, target)


def size_one_mc(zonal_demand, overflow_demand, zpair_demand, skus: List[SKU], cfg: RunConfig):
    """Size the zonal (x3) + overflow pools for one MC (fast/aggregate path).
    Reserves are added to demand (rollout fraction + AZ-death fraction of pair load)
    rather than multiplying node count."""
    z = dict(zonal_demand)
    for k in z:
        z[k] += cfg.az_failure_reserve * zpair_demand.get(k, 0.0) + cfg.rollout_surge * zonal_demand[k]
    o = {k: v * (1.0 + cfg.rollout_surge) for k, v in overflow_demand.items()}
    zpool = fast.best_zonal(z, skus, cfg.reserved, 1.0, cfg.usable_pods_per_node, cfg.usable_nic)
    opool = fast.best_overflow(o, skus, cfg.reserved, 1.0, cfg.usable_pods_per_node,
                               cfg.overflow_az_count)
    zonal_cores = zpool.total_cores if zpool else 0
    overflow_cores = opool.total_cores if opool else 0
    return {
        "zonal_pool": zpool.to_dict() if zpool else None,
        "overflow_pool": opool.to_dict() if opool else None,
        "zonal_cores": zonal_cores,
        "overflow_cores": overflow_cores,
    }


def size_one_mc_exact(model, distribution_mc, skus, cfg, want_packing=False):
    """Exact per-MC sizing: expand the MC's pods (+ reserve pods) and FFD-pack them,
    retaining the winning packing for the visualization."""
    zonal_by_az, overflow = exact.build_mc(
        model, distribution_mc, cfg.policy, cfg.percentile, cfg.multiplier,
        cfg.unsteered_placement, cfg.az_failure_reserve, cfg.concurrent_rolling_hcps,
        reserve_slots=cfg.reserve_slots, reserve_size=cfg.reserve_size)
    # Heterogeneous, disjoint-SKU sizing of both pools.
    zstats, znodes, ostats, onodes = exact.size_pools_disjoint(
        zonal_by_az, overflow, skus, cfg)
    out = {
        "zonal_pool": zstats,
        "overflow_pool": ostats,
        "zonal_cores": zstats["total_cores"] if zstats else 0,
        "overflow_cores": ostats["total_cores"] if ostats else 0,
    }
    if want_packing:
        out["packing"] = {
            "zonal": [[exact.node_to_json(n) for n in az] for az in (znodes or [])],
            "overflow": [exact.node_to_json(n) for n in (onodes or [])],
        }
    return out


def simulate(distribution: Dict[str, int], cfg: RunConfig,
             model: DemandModel = None, skus: List[SKU] = None):
    model = model or DemandModel()
    skus = skus or load_catalog()

    zonal, overflow, zpair, total = region_demand(model, distribution, cfg)
    if total == 0:
        return {"error": "empty fleet"}

    # Management-cluster sharding: every requested cluster is placed exactly once.
    # Each MC holds up to `usable_slots` real HCPs (+ reserve_slots placeholders).
    usable = cfg.usable_slots
    shards = exact.distribute_conserving(distribution, usable)     # per-MC real size dicts
    shapes = exact.group_shapes(shards)                            # [(dict, count)]
    n_mcs = len(shards)
    per_mc = usable

    if cfg.mode == "exact":
        # Exact: pack each distinct MC shape pod-for-pod; identical shapes share a lane.
        result_mcs = []
        for i, (dist_mc, count) in enumerate(shapes):
            mc = size_one_mc_exact(model, dist_mc, skus, cfg, want_packing=True)
            result_mcs.append({
                "kind": "full" if sum(dist_mc.values()) >= usable else "remainder",
                "hcps": sum(dist_mc.values()), "count": count, "hcp_mix": dist_mc, **mc})
    else:
        # Fast (aggregate) path: scale region demand by each shape's HCP share.
        result_mcs = []
        for dist_mc, count in shapes:
            frac = sum(dist_mc.values()) / total
            mc = size_one_mc(_scale(zonal, frac), _scale(overflow, frac),
                             _scale(zpair, frac), skus, cfg)
            result_mcs.append({
                "kind": "full" if sum(dist_mc.values()) >= usable else "remainder",
                "hcps": sum(dist_mc.values()), "count": count, "hcp_mix": dist_mc, **mc})

    region_zonal_cores = sum(m["zonal_cores"] * m["count"] for m in result_mcs)
    region_overflow_cores = sum(m["overflow_cores"] * m["count"] for m in result_mcs)
    region_zonal_nodes = sum((m["zonal_pool"]["total_nodes"] if m["zonal_pool"] else 0) * m["count"]
                             for m in result_mcs)
    region_overflow_nodes = sum((m["overflow_pool"]["total_nodes"] if m["overflow_pool"] else 0) * m["count"]
                                for m in result_mcs)

    return {
        "policy": cfg.policy,
        "total_hcps": total,
        "n_mcs": n_mcs,
        "per_mc_hcps": per_mc,
        "region": {
            "zonal_cores": region_zonal_cores,
            "overflow_cores": region_overflow_cores,
            "zonal_nodes": region_zonal_nodes,
            "overflow_nodes": region_overflow_nodes,
            "hcp_per_zonal_core": total / region_zonal_cores if region_zonal_cores else None,
        },
        "management_clusters": result_mcs,
        "demand_per_mc": {
            "zonal": _scale(zonal, per_mc / total),
            "overflow": _scale(overflow, per_mc / total),
        },
    }


def compare_policies(distribution: Dict[str, int], cfg: RunConfig,
                     model: DemandModel = None, skus: List[SKU] = None):
    """Run both policies and report the scarce-zonal-core delta."""
    model = model or DemandModel()
    skus = skus or load_catalog()
    out = {}
    for pol in ("legacy", "minimal"):
        c = RunConfig(**{**asdict(cfg), "policy": pol})
        out[pol] = simulate(distribution, c, model, skus)
    lz = out["legacy"]["region"]["zonal_cores"]
    mz = out["minimal"]["region"]["zonal_cores"]
    out["delta"] = {
        "zonal_cores_saved": lz - mz,
        "zonal_cores_saved_pct": (100.0 * (lz - mz) / lz) if lz else 0.0,
    }
    return out
