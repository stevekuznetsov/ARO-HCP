#!/usr/bin/env python3
"""Extract kube-apiserver (main container) per-replica CPU & memory time series
from kube-burner ndjson dumps (plain or gzipped). Caches result to JSON."""
import json, os, sys, datetime as dt
from datasets import DATASETS, KAS_CACHE, data_path
from hcputil import opener, cluster_name, drop_transient

def extract(path, target=None):
    """Return {'cpu'|'mem': {pod: [[epoch,value],...]}} for the kube-apiserver container.
    If `target` (clusterName) is given, only keep the matching hosted-cluster namespace."""
    out = {"cpu": {}, "mem": {}, "start": None, "end": None}
    with opener(path) as f:
        for line in f:
            if 'Controlplane' not in line:
                continue
            if '"container":"kube-apiserver"' not in line:
                continue
            try:
                r = json.loads(line)
            except ValueError:
                continue
            m = r.get("metricName")
            if m == "podCPU-Controlplane":
                key, mult = "cpu", 10.0                # %core -> millicores
            elif m == "podMemory-Controlplane":
                key, mult = "mem", 1.0 / (1024 * 1024)  # bytes -> MiB
            else:
                continue
            lab = r.get("labels", {})
            if lab.get("container") != "kube-apiserver":
                continue
            if target and target not in lab.get("namespace", ""):
                continue
            pod = lab.get("pod", "")
            epoch = dt.datetime.fromisoformat(r["timestamp"].replace("Z", "+00:00")).timestamp()
            out[key].setdefault(pod, []).append([epoch, r["value"] * mult])
            if out["start"] is None or epoch < out["start"]:
                out["start"] = epoch
            if out["end"] is None or epoch > out["end"]:
                out["end"] = epoch
    for key in ("cpu", "mem"):
        out[key] = drop_transient(out[key])           # drop rolled-over replicas
        for pod in out[key]:
            out[key][pod].sort()
    return out

def main():
    result = {}
    for label, fn in DATASETS:
        path = data_path(fn)
        if not os.path.exists(path):
            print(f"SKIP {label}: {fn} not found", file=sys.stderr); continue
        print(f"parsing {label} ({fn}) ...", file=sys.stderr)
        target = cluster_name(path)
        d = extract(path, target)
        npts = sum(len(v) for v in d["cpu"].values())
        print(f"  {label}: target={target} | {len(d['cpu'])} cpu replicas, "
              f"{len(d['mem'])} mem replicas, {npts} cpu points", file=sys.stderr)
        result[label] = d
    with open(KAS_CACHE, "w") as f:
        json.dump(result, f)
    print(f"cached -> {KAS_CACHE}", file=sys.stderr)

if __name__ == "__main__":
    main()
