"""Scheduling-constraint model, grounded in real hosted-control-plane pod dumps.

Each control-plane component carries a set of *scheduling constraints* that the
exact solver must honour pod-for-pod. These are derived from real Pod specs
(see demand/parse_pod_dump.py) rather than assumed:

  ZONE_SPREAD  - replicas must occupy distinct availability zones.
                 legacy:  required podAntiAffinity topologyKey=topology.kubernetes.io/zone
                 minimal: topologySpreadConstraints maxSkew=1 zone (balanced);
                          etcd stays strict DoNotSchedule minDomains=3.
  HOST_SPREAD  - replicas must occupy distinct nodes
                 (required podAntiAffinity topologyKey=kubernetes.io/hostname).
  COLOCATE     - soft podAffinity(kubernetes.io/hostname) packing a hosted
                 cluster's pods together; an objective term, never a hard rule.
  NODE_ROLE    - under minimal, zone-critical pods carry node affinity/toleration
                 for the zonal pools; float pods for the overflow pools.

`load_constraints()` reads a parsed dump (sample_inputs/real_pods_*.json) and
returns per-component flags; `constraint_spec()` merges those with the policy
tiering in components.py so both the fast and exact solvers read one source.
"""
import json
import os

from . import components as comp

ZONE_KEY = "topology.kubernetes.io/zone"
HOST_KEY = "kubernetes.io/hostname"

LEGACY_DUMP = os.path.join(os.path.dirname(os.path.dirname(__file__)),
                           "sample_inputs", "real_pods_legacy.json")


def _has_required(section, key):
    return any(t.get("topologyKey") == key for t in section.get("required", []))


def _has_preferred(section, key):
    return any(t.get("topologyKey") == key for t in section.get("preferred", []))


def load_constraints(dump_path=LEGACY_DUMP):
    """Return {component: {zone_spread, host_spread, colocate}} from a parsed dump."""
    if not os.path.exists(dump_path):
        return {}
    d = json.load(open(dump_path))
    out = {}
    for name, c in d["components"].items():
        out[name] = {
            "zone_spread": _has_required(c["podAntiAffinity"], ZONE_KEY),
            "host_spread": _has_required(c["podAntiAffinity"], HOST_KEY),
            "colocate": _has_preferred(c["podAffinity"], HOST_KEY),
        }
    return out


# Fallback zone-spread set (from the enhancement) when a component is absent from a dump.
_FALLBACK_ZONE_SPREAD = comp.ZONAL_PAIR_COMPONENTS | {"etcd"}


def constraint_spec(component, policy, observed_zone_spread=None):
    """Merge real-dump constraint flags with policy tiering into one spec.

    Returns dict:
      placement   : "zonal" | "overflow"
      zone_spread : bool  (hard cross-AZ spread required)
      zone_strict : bool  (etcd: DoNotSchedule, never co-locate; else balanced)
      host_spread : bool  (distinct nodes)
      nic         : int   (swift NICs per replica)
    """
    tier = comp.tier_of(component)
    placement = "zonal" if comp.is_zonal(component, policy) else "overflow"

    if policy == "legacy":
        # Legacy: the real dump tells us which components are zone-spread today.
        if observed_zone_spread is not None:
            zone_spread = observed_zone_spread
        else:
            zone_spread = component in _FALLBACK_ZONE_SPREAD
    else:
        # Minimal: only the zone-critical tiers spread; float goes to overflow (no zone spread).
        zone_spread = tier in (comp.ZONAL_ETCD, comp.ZONAL_PAIR)

    return {
        "placement": placement,
        "zone_spread": zone_spread,
        "zone_strict": (tier == comp.ZONAL_ETCD),
        "host_spread": True,   # every HA component keeps distinct-node spread
        "nic": comp.nic_per_replica(component),
    }
