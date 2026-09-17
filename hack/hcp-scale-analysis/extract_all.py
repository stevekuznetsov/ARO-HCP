#!/usr/bin/env python3
"""Extract ALL hosted-control-plane pods (pod-level = sum of the pod's containers)
CPU & memory time series from kube-burner dumps. Caches per-dataset per-pod series."""
import json, os, sys, datetime as dt
from collections import defaultdict
from datasets import DATASETS, POD_CACHE, data_path
from hcputil import opener, cluster_name, drop_transient

def extract(path, target=None):
    # per metric: pod -> {epoch -> summed value across containers}
    agg = {"cpu": defaultdict(lambda: defaultdict(float)),
           "mem": defaultdict(lambda: defaultdict(float))}
    conts = {"cpu": defaultdict(set), "mem": defaultdict(set)}
    with opener(path) as f:
        for line in f:
            if 'Controlplane' not in line:
                continue
            if 'podCPU-Controlplane' in line:
                key, mult = "cpu", 10.0
            elif 'podMemory-Controlplane' in line:
                key, mult = "mem", 1.0 / (1024 * 1024)
            else:
                continue
            try:
                r = json.loads(line)
            except ValueError:
                continue
            m = r.get("metricName")
            if m not in ("podCPU-Controlplane", "podMemory-Controlplane"):
                continue
            key = "cpu" if m == "podCPU-Controlplane" else "mem"
            mult = 10.0 if key == "cpu" else 1.0 / (1024 * 1024)
            lab = r["labels"]
            if target and target not in lab.get("namespace", ""):
                continue
            cont = lab.get("container", "")
            if cont in ("", "POD"):
                continue
            pod = lab.get("pod", "")
            epoch = dt.datetime.fromisoformat(r["timestamp"].replace("Z", "+00:00")).timestamp()
            agg[key][pod][epoch] += r["value"] * mult
            conts[key][pod].add(cont)
    out = {"cpu": {}, "mem": {}, "containers": {}}
    for key in ("cpu", "mem"):
        series = {pod: sorted([[t, v] for t, v in tv.items()]) for pod, tv in agg[key].items()}
        out[key] = drop_transient(series)             # drop rolled-over replicas
    allpods = set(out["cpu"]) | set(out["mem"])
    for pod in allpods:
        out["containers"][pod] = sorted(conts["cpu"].get(pod, set()) | conts["mem"].get(pod, set()))
    return out

def main():
    result = {}
    for label, fn in DATASETS:
        path = data_path(fn)
        if not os.path.exists(path):
            print(f"SKIP {label}: not found", file=sys.stderr); continue
        print(f"parsing {label} ...", file=sys.stderr)
        target = cluster_name(path)
        d = extract(path, target)
        print(f"  {label}: target={target} | {len(d['cpu'])} pods (cpu), "
              f"{len(d['mem'])} pods (mem)", file=sys.stderr)
        result[label] = d
    with open(POD_CACHE, "w") as f:
        json.dump(result, f)
    print(f"cached -> {POD_CACHE}", file=sys.stderr)

if __name__ == "__main__":
    main()
