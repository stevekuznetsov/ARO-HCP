"""VM SKU catalog for the simulator.

A SKU exposes three schedulable dimensions, mirroring ARO-HCP's own model
(fleet/pkg/azure/skucache/skucache.go):
  cpu       = vCPUs
  memory    = MemoryGB (treated as GiB)
  swift_nic = MaxNetworkInterfaces - 1   (primary/host NIC reserved)
"""
import os
from dataclasses import dataclass
from typing import List

import yaml

DEFAULT_CATALOG = os.path.join(os.path.dirname(__file__), "sample_inputs", "skus.yaml")


@dataclass(frozen=True)
class SKU:
    name: str
    vcpu: int
    memory_gib: float
    max_nics: int
    series: str = ""

    @property
    def swift_nic(self) -> int:
        return max(self.max_nics - 1, 0)

    # Usable capacity after an ABSOLUTE system reservation (kubelet --system-reserved).
    # The ARO-HCP kubelet DaemonSet pins a flat cpu=3000m, memory=7550Mi on every
    # worker node (mgmt-fixes/deploy/kubelet-ds), so the reservation is subtracted as
    # an absolute amount, not a fraction — this is why small SKUs are inefficient.
    def usable_cpu_mc(self, reserved_cpu_mc: float) -> float:
        return max(self.vcpu * 1000.0 - reserved_cpu_mc, 0.0)

    def usable_mem_mib(self, reserved_mem_mib: float) -> float:
        return max(self.memory_gib * 1024.0 - reserved_mem_mib, 0.0)


def load_catalog(path=DEFAULT_CATALOG) -> List[SKU]:
    with open(path) as f:
        raw = yaml.safe_load(f)
    return [SKU(**s) for s in raw["skus"]]


def catalog_to_dicts(skus: List[SKU]):
    return [{"name": s.name, "vcpu": s.vcpu, "memory_gib": s.memory_gib,
             "max_nics": s.max_nics, "swift_nic": s.swift_nic, "series": s.series}
            for s in skus]
