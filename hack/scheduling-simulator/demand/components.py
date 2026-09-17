"""Component tiering for the minimal-zonal-scheduling model.

Grounded in the real minimal-zonal control-plane pod dump
(/tmp/opencode/minimal-zonal-control-plane-pods, summarized in `_scheduling-index.txt`),
which shows THREE placement buckets under the Minimal policy as currently
implemented (the CPO side; the companion cluster-network-operator change is not
yet in the dump):

  ZONAL      req:zonal node role, hard zone TSC (DoNotSchedule)
             -> etcd (3, strict 1/AZ) + kube-apiserver, oauth-openshift,
                openshift-apiserver, openshift-oauth-apiserver, router,
                ignition-server-proxy (all 2-replica pairs)
  OVERFLOW   pref:overflow node role (soft), HA controllers carry a best-effort
             (ScheduleAnyway) zone TSC
             -> all float controllers + single-replica operators/catalogs
  UNSTEERED  no node role at all (scheduler places freely) — the CNO-managed
             operands and CSI controllers:
             network-node-identity (3), multus-admission-controller (2),
             ovnkube-control-plane (2), azure-disk/file-csi-driver-controller,
             cloud-network-config-controller, csi-snapshot-controller

Under `legacy`, every HA component is spread across AZs at 3 replicas and there
is no overflow pool.

UNSTEERED placement is configurable (`unsteered_placement`):
  - "overflow" (default): they carry no zonal requirement in the dump, so they do
    not force scarce zonal capacity — the conservative choice for zonal sizing.
  - "zonal": models the enhancement's *target* once the CNO change lands, where
    network-node-identity and multus (failurePolicy: Fail blocking webhooks) are
    steered zonal. (When set, only the blocking-webhook pair is treated zonal; the
    rest stay overflow.)
"""

ZONAL_ETCD = "zonal_etcd"
ZONAL_PAIR = "zonal_pair"
FLOAT = "float"
UNSTEERED = "unsteered"

# req:zonal, hard zone TSC — exactly the set in the dump's `_scheduling-index.txt`.
ZONAL_PAIR_COMPONENTS = {
    "kube-apiserver",
    "oauth-openshift",
    "router",
    "ignition-server-proxy",
    "openshift-apiserver",
    "openshift-oauth-apiserver",
}

# No node-role steering in the dump (CNO-managed operands + CSI controllers).
UNSTEERED_COMPONENTS = {
    "network-node-identity",
    "multus-admission-controller",
    "ovnkube-control-plane",
    "azure-disk-csi-driver-controller",
    "azure-file-csi-driver-controller",
    "cloud-network-config-controller",
    "csi-snapshot-controller",
}

# Blocking-webhook backends (failurePolicy: Fail) — steered zonal under the
# enhancement's target once the CNO change lands.
BLOCKING_WEBHOOK_COMPONENTS = {
    "network-node-identity",
    "multus-admission-controller",
}

# SWIFT NICs are consumed one per router pod. The dump shows no pod requesting the
# extended resource, so this stays a modeled quantity (see enhancement / capacity code).
NIC_PER_REPLICA = {"router": 1}

# `router` is synthesised for every size from the 49-node profile (the only perf
# run that actually deployed a router); its NIC is the real planning cost.
SYNTHETIC_COMPONENTS = {"router"}

# Zone-critical pairs present in the real minimal dump but absent from the usage
# profiles (the conformance perf clusters did not deploy them). Injected with a
# fixed per-replica footprint taken from the dump's pod requests (per replica),
# size-independent. Refine if usage data becomes available.
#   (component, cpu_mc_per_replica, mem_mib_per_replica)
INJECTED_ZONAL = [
    ("oauth-openshift", 50.0, 110.0),
    ("openshift-oauth-apiserver", 175.0, 130.0),
]


def tier_of(component):
    if component == "etcd":
        return ZONAL_ETCD
    if component in ZONAL_PAIR_COMPONENTS:
        return ZONAL_PAIR
    if component in UNSTEERED_COMPONENTS:
        return UNSTEERED
    return FLOAT


def nic_per_replica(component):
    return NIC_PER_REPLICA.get(component, 0)


def is_statefulset(component):
    """etcd is the only StatefulSet; everything else is a Deployment. Rollout surge
    (maxSurge=1) applies to multi-replica Deployments, not the StatefulSet."""
    return component == "etcd"


def replicas_for(component, observed_replicas, policy):
    """Replica count for a component under a policy.

    policy: "legacy" or "minimal".
    observed_replicas: concurrent replicas seen in the perf data (fallback).
    """
    t = tier_of(component)
    if t == ZONAL_ETCD:
        return 3  # quorum, always
    if t == ZONAL_PAIR:
        return 2 if policy == "minimal" else 3
    # FLOAT / UNSTEERED: replica count unchanged by policy; use observed (min 1).
    return max(observed_replicas, 1)


def is_zonal(component, policy, unsteered_placement="overflow"):
    """Whether a component lands on the balanced zonal triplet under a policy."""
    if policy == "legacy":
        return True  # everything zonal under legacy
    t = tier_of(component)
    if t in (ZONAL_ETCD, ZONAL_PAIR):
        return True
    if t == UNSTEERED and unsteered_placement == "zonal":
        # target state: only the blocking-webhook backends are steered zonal
        return component in BLOCKING_WEBHOOK_COMPONENTS
    return False
