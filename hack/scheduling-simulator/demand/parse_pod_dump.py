#!/usr/bin/env python3
"""Parse a raw Pod-dump CSV (one JSON Pod per row, single 'object1' column) from a
real hosted control plane into a compact per-component scheduling model:

  - resource requests (cpu millicores, memory MiB, aro.openshift.io/swift-nic)
  - podAntiAffinity / podAffinity topology keys + label selectors (required/preferred)
  - nodeAffinity (key/op/values)
  - topologySpreadConstraints (topologyKey, maxSkew, whenUnsatisfiable, minDomains, matchLabelKeys)
  - tolerations, nodeSelector
  - replica count (pods per app/pod-template)

Usage:
  python3 parse_pod_dump.py "/path/to/export.csv" [out.json]

This grounds the simulator's constraint model in what the scheduler actually sees.
"""
import csv, json, sys, re
from collections import defaultdict, Counter

csv.field_size_limit(sys.maxsize)


def parse_quantity_cpu(v):
    """CPU quantity -> millicores."""
    if v is None:
        return 0.0
    v = str(v)
    if v.endswith("m"):
        return float(v[:-1])
    return float(v) * 1000.0


_MEM_UNITS = {"Ki": 1/1024, "Mi": 1, "Gi": 1024, "Ti": 1024*1024,
              "K": 1000/1024/1024*1024, "M": 1000*1000/1024/1024,
              "G": 1000**3/1024/1024, "T": 1000**4/1024/1024}


def parse_quantity_mem(v):
    """Memory quantity -> MiB."""
    if v is None:
        return 0.0
    v = str(v)
    m = re.match(r"^(\d+(?:\.\d+)?)([A-Za-z]+)?$", v)
    if not m:
        return 0.0
    num = float(m.group(1)); unit = m.group(2) or ""
    if unit in ("Mi", ""):
        return num if unit == "Mi" else num / (1024*1024)  # bare = bytes
    return num * _MEM_UNITS.get(unit, 0.0)


def app_of(pod):
    l = pod["metadata"].get("labels", {})
    return (l.get("app") or l.get("openshift.io/component")
            or l.get("name") or pod["metadata"].get("generateName", "?").rstrip("-"))


def pod_requests(pod):
    cpu = mem = 0.0
    nic = 0
    for c in pod["spec"].get("containers", []):
        req = (c.get("resources", {}) or {}).get("requests", {}) or {}
        cpu += parse_quantity_cpu(req.get("cpu"))
        mem += parse_quantity_mem(req.get("memory"))
        for k, v in req.items():
            if "swift-nic" in k or k.endswith("/nic"):
                nic += int(float(v))
    return cpu, mem, nic


def anti_affinity(aff):
    out = {"required": [], "preferred": []}
    a = aff.get("podAntiAffinity") or {}
    for t in a.get("requiredDuringSchedulingIgnoredDuringExecution", []) or []:
        out["required"].append({"topologyKey": t.get("topologyKey"),
                                 "labelSelector": t.get("labelSelector")})
    for w in a.get("preferredDuringSchedulingIgnoredDuringExecution", []) or []:
        t = w.get("podAffinityTerm", {})
        out["preferred"].append({"weight": w.get("weight"), "topologyKey": t.get("topologyKey"),
                                 "labelSelector": t.get("labelSelector")})
    return out


def affinity_terms(aff, kind):
    out = {"required": [], "preferred": []}
    a = aff.get(kind) or {}
    for t in a.get("requiredDuringSchedulingIgnoredDuringExecution", []) or []:
        out["required"].append({"topologyKey": t.get("topologyKey"),
                                 "labelSelector": t.get("labelSelector")})
    for w in a.get("preferredDuringSchedulingIgnoredDuringExecution", []) or []:
        t = w.get("podAffinityTerm", {})
        out["preferred"].append({"weight": w.get("weight"), "topologyKey": t.get("topologyKey"),
                                 "labelSelector": t.get("labelSelector")})
    return out


def node_affinity(aff):
    out = {"required": [], "preferred": []}
    na = aff.get("nodeAffinity") or {}
    req = na.get("requiredDuringSchedulingIgnoredDuringExecution") or {}
    for term in req.get("nodeSelectorTerms", []) or []:
        for e in term.get("matchExpressions", []) or []:
            out["required"].append({"key": e.get("key"), "operator": e.get("operator"),
                                    "values": e.get("values")})
    for w in na.get("preferredDuringSchedulingIgnoredDuringExecution", []) or []:
        for e in w.get("preference", {}).get("matchExpressions", []) or []:
            out["preferred"].append({"weight": w.get("weight"), "key": e.get("key"),
                                     "operator": e.get("operator"), "values": e.get("values")})
    return out


def tsc_of(pod):
    out = []
    for t in pod["spec"].get("topologySpreadConstraints", []) or []:
        out.append({"topologyKey": t.get("topologyKey"), "maxSkew": t.get("maxSkew"),
                    "whenUnsatisfiable": t.get("whenUnsatisfiable"),
                    "minDomains": t.get("minDomains"),
                    "matchLabelKeys": t.get("matchLabelKeys"),
                    "labelSelector": t.get("labelSelector")})
    return out


def load_pods(path):
    pods = []
    with open(path, newline="", encoding="utf-8-sig") as f:
        r = csv.reader(f)
        next(r, None)
        for row in r:
            if row and row[0].strip():
                pods.append(json.loads(row[0]))
    return pods


def summarize(pods):
    by_app = defaultdict(list)
    for p in pods:
        by_app[app_of(p)].append(p)
    comps = {}
    for app, plist in sorted(by_app.items()):
        rep = plist[0]
        spec = rep["spec"]
        aff = spec.get("affinity", {}) or {}
        cpu, mem, nic = pod_requests(rep)
        comps[app] = {
            "replicas": len(plist),
            "requests": {"cpu_mc": round(cpu, 1), "mem_mib": round(mem, 1), "swift_nic": nic},
            "nodeSelector": spec.get("nodeSelector") or {},
            "tolerations": [{"key": t.get("key"), "operator": t.get("operator"),
                             "value": t.get("value"), "effect": t.get("effect")}
                            for t in spec.get("tolerations", []) or []],
            "podAntiAffinity": anti_affinity(aff),
            "podAffinity": affinity_terms(aff, "podAffinity"),
            "nodeAffinity": node_affinity(aff),
            "topologySpreadConstraints": tsc_of(rep),
        }
    return comps


def main():
    if len(sys.argv) < 2:
        sys.exit("usage: parse_pod_dump.py <dump.csv> [out.json]")
    path = sys.argv[1]
    out = sys.argv[2] if len(sys.argv) > 2 else None
    pods = load_pods(path)
    comps = summarize(pods)
    result = {"pod_count": len(pods), "component_count": len(comps), "components": comps}
    if out:
        with open(out, "w") as f:
            json.dump(result, f, indent=2)
        print("wrote", out)
    else:
        print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
