"""Fast (closed-form) pool sizing and SKU selection.

Zonal and overflow pools are separable: the zonal pool depends only on the zonal
demand + zonal SKU, the overflow pool only on the overflow demand + overflow SKU,
so each is chosen independently. Zonal is provisioned as an AZ-balanced triplet
(identical node count in each of 3 AZ pools).
"""
import math
from dataclasses import dataclass, asdict
from typing import List, Optional

from skus import SKU


@dataclass
class PoolResult:
    role: str                 # "zonal" (per-AZ) or "overflow"
    sku: str
    nodes: int                # per-AZ for zonal; total for overflow
    az_count: int             # 3 for zonal, N for overflow
    total_nodes: int          # nodes * az_count
    total_cores: int
    total_mem_gib: float      # provisioned memory (nodes * SKU memory) — waste proxy
    # utilisation (0..1) of the provisioned pool, per dimension (incl. headroom in demand)
    util_cpu: float
    util_mem: float
    util_nic: Optional[float]
    util_pods: float
    binding: str              # which dimension set the node count

    def to_dict(self):
        return asdict(self)

    # Cost/waste ordering: fewest scarce cores, then fewest nodes (each node incurs
    # per-node overhead), then least provisioned memory (avoid over-sized SKUs like
    # an E16 at 33% memory when a D16 at 66% does the same job for half the RAM).
    @property
    def sort_key(self):
        return (self.total_cores, self.total_nodes, self.total_mem_gib)


def _ceil_div(a, b):
    return int(math.ceil(a / b)) if b > 0 else math.inf


def size_zonal_pool(zonal_total, sku: SKU, reserve_fn, headroom, usable_pods,
                    usable_nic_fn=lambda s: s.swift_nic) -> Optional[PoolResult]:
    """zonal_total: aggregate {cpu_mc, mem_mib, nic, pods} across all 3 AZs for one MC.
    headroom: multiplier applied to demand (AZ-failure reserve * rollout surge).
    reserve_fn(sku) -> (reserved_cpu_mc, reserved_mem_mib)."""
    # balanced across 3 AZs
    per_az = {k: v / 3.0 * headroom for k, v in zonal_total.items()}
    swift = usable_nic_fn(sku)
    if per_az["nic"] > 0 and swift <= 0:
        return None  # cannot host router NICs
    rc, rm = reserve_fn(sku)
    ucpu = sku.usable_cpu_mc(rc)
    umem = sku.usable_mem_mib(rm)
    if ucpu <= 0 or umem <= 0:
        return None  # reservation consumes the whole node
    n_cpu = _ceil_div(per_az["cpu_mc"], ucpu)
    n_mem = _ceil_div(per_az["mem_mib"], umem)
    n_nic = _ceil_div(per_az["nic"], swift) if per_az["nic"] > 0 else 0
    n_pods = _ceil_div(per_az["pods"], usable_pods)
    nodes = max(n_cpu, n_mem, n_nic, n_pods, 1)
    binding = max((("cpu", n_cpu), ("mem", n_mem), ("nic", n_nic), ("pods", n_pods)),
                  key=lambda x: x[1])[0]
    return PoolResult(
        role="zonal", sku=sku.name, nodes=nodes, az_count=3, total_nodes=nodes * 3,
        total_cores=nodes * 3 * sku.vcpu, total_mem_gib=nodes * 3 * sku.memory_gib,
        util_cpu=per_az["cpu_mc"] / (nodes * ucpu) if ucpu else 0.0,
        util_mem=per_az["mem_mib"] / (nodes * umem) if umem else 0.0,
        util_nic=(per_az["nic"] / (nodes * swift)) if swift else None,
        util_pods=per_az["pods"] / (nodes * usable_pods) if usable_pods else 0.0,
        binding=binding,
    )


def size_overflow_pool(overflow_total, sku: SKU, reserve_fn, headroom, usable_pods,
                       az_count=1) -> Optional[PoolResult]:
    """overflow_total: aggregate {cpu_mc, mem_mib, pods} for one MC (no NIC)."""
    dem = {k: v * headroom for k, v in overflow_total.items()}
    rc, rm = reserve_fn(sku)
    ucpu = sku.usable_cpu_mc(rc)
    umem = sku.usable_mem_mib(rm)
    if ucpu <= 0 or umem <= 0:
        return None
    n_cpu = _ceil_div(dem["cpu_mc"], ucpu)
    n_mem = _ceil_div(dem["mem_mib"], umem)
    n_pods = _ceil_div(dem.get("pods", 0), usable_pods)
    nodes = max(n_cpu, n_mem, n_pods, 1)
    binding = max((("cpu", n_cpu), ("mem", n_mem), ("pods", n_pods)), key=lambda x: x[1])[0]
    return PoolResult(
        role="overflow", sku=sku.name, nodes=nodes, az_count=az_count, total_nodes=nodes,
        total_cores=nodes * sku.vcpu, total_mem_gib=nodes * sku.memory_gib,
        util_cpu=dem["cpu_mc"] / (nodes * ucpu) if ucpu else 0.0,
        util_mem=dem["mem_mib"] / (nodes * umem) if umem else 0.0,
        util_nic=None,
        util_pods=dem.get("pods", 0) / (nodes * usable_pods) if usable_pods else 0.0,
        binding=binding,
    )


def best_zonal(zonal_total, skus: List[SKU], reserve_fn, headroom, usable_pods,
               usable_nic_fn=lambda s: s.swift_nic) -> Optional[PoolResult]:
    """Minimise scarce zonal cores, then nodes, then provisioned memory (least waste)."""
    cands = [size_zonal_pool(zonal_total, s, reserve_fn, headroom, usable_pods, usable_nic_fn)
             for s in skus]
    cands = [c for c in cands if c]
    if not cands:
        return None
    return min(cands, key=lambda p: p.sort_key)


def best_overflow(overflow_total, skus: List[SKU], reserve_fn, headroom, usable_pods,
                  az_count=1) -> Optional[PoolResult]:
    """Minimise overflow cores, then nodes, then provisioned memory (least waste)."""
    if overflow_total["cpu_mc"] <= 0 and overflow_total["mem_mib"] <= 0:
        return None  # nothing on overflow (legacy policy)
    cands = [size_overflow_pool(overflow_total, s, reserve_fn, headroom, usable_pods, az_count)
             for s in skus]
    cands = [c for c in cands if c]
    if not cands:
        return None
    return min(cands, key=lambda p: p.sort_key)
