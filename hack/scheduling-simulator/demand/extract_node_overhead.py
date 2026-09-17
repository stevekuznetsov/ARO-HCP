#!/usr/bin/env python3
"""Extract per-node NON-HCP workload overhead on worker (user) node pools from a
cluster-utilization report.html (tooling/cluster-utilization output).

Worker-pool nodes on a management cluster run HCP control-plane pods (ocm-*
namespaces) plus a floor of non-HCP overhead: kube-system daemonsets (CNI, CSI,
monitoring agents), arobit log forwarding, velero, mgmt-agent, etc. That floor
reduces the capacity available to HCPs and must be reserved per node in the
simulator, on top of the kubelet --system-reserved.

Method: join the peak-snapshot CONCURRENT replica count (correct concurrency,
avoids counting churned/rolled instances) with the window per-pod p95 usage
(correct per-pod cost), then divide by the worker-node count.

Usage:
  python3 extract_node_overhead.py ~/Downloads/report.html [cluster-substring]
"""
import json
import re
import sys

HCP_NAMESPACES = {"ocm", "klusterlet"}  # normalized matchers in the report


def load_report(path):
    html = open(path, encoding="utf-8").read()
    m = re.search(r'<script id="report" type="application/json">(.*?)</script>', html, re.S)
    if not m:
        sys.exit("no embedded report JSON found")
    return json.loads(m.group(1))


def node_overhead(unit):
    nodes = sum(p["nodeCount"] for p in unit["nodePools"] if p["category"] == "user")
    if nodes == 0:
        return None
    is_hcp = lambda ns: ns in HCP_NAMESPACES
    # concurrent replicas at the (memory) peak, keyed by (namespace, workload)
    peak = {(w["namespace"], w["workload"]): w["replicas"]
            for w in unit["peakWorkloads"]
            if w["poolCategory"] == "user" and not is_hcp(w["namespace"])}
    win = {(w["namespace"], w["workload"]): w
           for w in unit["workloads"]
           if w["poolCategory"] == "user" and not is_hcp(w["namespace"])}
    tot_cpu = tot_mem = tot_pods = 0.0
    rows = []
    for k, conc in peak.items():
        w = win.get(k)
        if not w:
            continue
        tot_cpu += conc * w["cpuUsage"]
        tot_mem += conc * w["memUsage"]
        tot_pods += conc
        rows.append((k[0], k[1], conc, w["cpuUsage"], w["memUsage"]))
    return {
        "cluster": unit["cluster"], "nodes": nodes,
        "per_node_cpu_cores": tot_cpu / nodes,
        "per_node_mem_gib": tot_mem / nodes / 2**30,
        "per_node_pods": tot_pods / nodes,
        "per_node_cpu_mc": round(tot_cpu / nodes * 1000),
        "per_node_mem_mib": round(tot_mem / nodes / 2**20),
        "rows": sorted(rows, key=lambda r: -r[2] * r[4]),
    }


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    report = load_report(sys.argv[1])
    want = sys.argv[2] if len(sys.argv) > 2 else "mgmt"
    for u in report["units"]:
        if u["role"] != "mgmt" or want not in u["cluster"]:
            continue
        o = node_overhead(u)
        if not o:
            continue
        print(f"{o['cluster']}: {o['nodes']} worker nodes")
        print(f"  per-node non-HCP overhead: cpu={o['per_node_cpu_mc']}m "
              f"mem={o['per_node_mem_mib']}Mi ({o['per_node_mem_gib']:.2f}Gi) "
              f"pods={o['per_node_pods']:.1f}")
        for ns, wl, conc, cpu, mem in o["rows"][:12]:
            print(f"    {ns:22} {wl:30} conc={conc:3} "
                  f"cpu/pod={cpu:.3f} mem/pod={mem/2**20:6.0f}Mi")


if __name__ == "__main__":
    main()
