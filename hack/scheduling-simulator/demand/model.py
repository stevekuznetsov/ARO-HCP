"""Per-cluster control-plane demand, computed live from observed usage profiles.

request_per_replica = multiplier * percentile_p(observed per-replica usage)

The percentile and multiplier are run inputs (the usage->request transform).
"""
import json
import os
from dataclasses import dataclass, field
from typing import List

import numpy as np

from . import components as comp

PROFILES_PATH = os.environ.get(
    "ARO_HCP_SIM_PROFILES",
    os.path.join(os.path.dirname(os.path.dirname(__file__)), "sample_inputs", "profiles.json"),
)


@dataclass(frozen=True)
class Component:
    name: str
    tier: str
    zonal: bool
    replicas: int
    cpu_mc: float      # per replica
    mem_mib: float     # per replica
    nic: int           # per replica

    @property
    def total_cpu_mc(self):
        return self.cpu_mc * self.replicas

    @property
    def total_mem_mib(self):
        return self.mem_mib * self.replicas

    @property
    def total_nic(self):
        return self.nic * self.replicas


@dataclass
class ClusterDemand:
    size: str
    policy: str
    components: List[Component] = field(default_factory=list)

    def _agg(self, zonal: bool):
        cpu = sum(c.total_cpu_mc for c in self.components if c.zonal == zonal)
        mem = sum(c.total_mem_mib for c in self.components if c.zonal == zonal)
        nic = sum(c.total_nic for c in self.components if c.zonal == zonal)
        pods = sum(c.replicas for c in self.components if c.zonal == zonal)
        return {"cpu_mc": cpu, "mem_mib": mem, "nic": nic, "pods": pods}

    def zonal(self):
        return self._agg(True)

    def overflow(self):
        return self._agg(False)

    def zonal_pair(self):
        """Non-etcd zonal aggregate (the pairs that can reschedule on AZ death)."""
        pods = [c for c in self.components if c.zonal and c.tier == "zonal_pair"]
        return {"cpu_mc": sum(c.total_cpu_mc for c in pods),
                "mem_mib": sum(c.total_mem_mib for c in pods),
                "nic": sum(c.total_nic for c in pods),
                "pods": sum(c.replicas for c in pods)}


class DemandModel:
    def __init__(self, profiles_path=PROFILES_PATH):
        with open(profiles_path) as f:
            self._data = json.load(f)
        self.sizes = self._data["sizes"]
        self._profiles = self._data["profiles"]
        self._router = self._data.get("router_profile", {"cpu_mc": [], "mem_mib": []})

    @staticmethod
    def _pctl(samples, p):
        if not samples:
            return 0.0
        return float(np.percentile(np.asarray(samples, dtype=float), p))

    def cluster_demand(self, size, policy, percentile=75.0, multiplier=1.0,
                       include_router=True, unsteered_placement="overflow") -> ClusterDemand:
        """Build the per-cluster component demand for one hosted cluster of `size`."""
        if size not in self._profiles:
            raise KeyError(f"no profile for size {size!r}; have {self.sizes}")
        prof = self._profiles[size]
        cd = ClusterDemand(size=size, policy=policy)

        for name, p in prof.items():
            if name in comp.SYNTHETIC_COMPONENTS:
                continue  # router injected separately from its own profile
            observed = int(p.get("replicas", 1))
            replicas = comp.replicas_for(name, observed, policy)
            cd.components.append(Component(
                name=name,
                tier=comp.tier_of(name),
                zonal=comp.is_zonal(name, policy, unsteered_placement),
                replicas=replicas,
                cpu_mc=multiplier * self._pctl(p.get("cpu_mc", []), percentile),
                mem_mib=multiplier * self._pctl(p.get("mem_mib", []), percentile),
                nic=comp.nic_per_replica(name),
            ))

        if include_router:
            replicas = comp.replicas_for("router", 3, policy)
            cd.components.append(Component(
                name="router",
                tier=comp.tier_of("router"),
                zonal=comp.is_zonal("router", policy, unsteered_placement),
                replicas=replicas,
                cpu_mc=multiplier * self._pctl(self._router.get("cpu_mc", []), percentile),
                mem_mib=multiplier * self._pctl(self._router.get("mem_mib", []), percentile),
                nic=comp.nic_per_replica("router"),
            ))

        # oauth-openshift and openshift-oauth-apiserver are zone-critical pairs in the
        # real minimal-zonal dump but absent from the usage profiles (the conformance
        # perf clusters did not deploy them). Inject them from a fixed per-replica
        # footprint so the zonal tier is complete. Scaled by `multiplier` only
        # (size-independent approximation; refine if usage data becomes available).
        for name, cpu_mc, mem_mib in comp.INJECTED_ZONAL:
            if name in prof:
                continue  # profile exists, already added above
            replicas = comp.replicas_for(name, 2, policy)
            cd.components.append(Component(
                name=name, tier=comp.tier_of(name),
                zonal=comp.is_zonal(name, policy, unsteered_placement),
                replicas=replicas, cpu_mc=multiplier * cpu_mc, mem_mib=multiplier * mem_mib,
                nic=comp.nic_per_replica(name),
            ))
        return cd
