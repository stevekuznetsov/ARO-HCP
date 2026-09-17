#!/usr/bin/env python3
"""Per-workload p75 report for the hosted control plane, with 3->2 replica
savings modeled two ways. Emits CSV + Markdown."""
import json
import numpy as np
from collections import defaultdict
from datasets import ORDER, POD_CACHE, REPORT_CSV, REPORT_MD

DT = 30.0
P = 75  # percentile of interest (top of box)

def workload(pod):
    segs = pod.split("-")
    if segs[-1].isdigit():
        return "-".join(segs[:-1])           # statefulset ordinal
    return "-".join(segs[:-2])               # deployment: strip rs-hash + pod-suffix

def resample(series, grid, t0):
    arr = np.array(series)
    te, ve = arr[:, 0] - t0, arr[:, 1]
    g = np.interp(grid, te, ve, left=np.nan, right=np.nan)
    g[(grid < te.min() - 1) | (grid > te.max() + 1)] = np.nan
    return g

def sum_series(reps):
    valid = np.sum(~np.isnan(reps), axis=0) == reps.shape[0]
    s = np.nansum(reps, axis=0)
    s[~valid] = np.nan
    return s[~np.isnan(s)]

def build_reps(pods, podmap):
    allt = np.concatenate([np.array(podmap[p])[:, 0] for p in pods])
    t0, t1 = allt.min(), allt.max()
    grid = np.arange(0.0, t1 - t0 + DT, DT)
    reps = np.array([resample(podmap[p], grid, t0) for p in pods])
    return reps, list(pods)

def pctl(a, p=P):
    a = a[~np.isnan(a)]
    return float(np.percentile(a, p)) if a.size else float("nan")

def analyze(podmap):
    wl = defaultdict(list)
    for pod in podmap:
        wl[workload(pod)].append(pod)
    out = {}
    for w, pods in wl.items():
        reps, _ = build_reps(pods, podmap)
        n = len(pods)
        sum_p75 = pctl(sum_series(reps))
        rep_p75 = np.array([pctl(reps[i]) for i in range(n)])
        drop2_p75 = float("nan")
        if n >= 2:
            keep = np.argsort(rep_p75)[::-1][:2]   # 2 busiest replicas
            drop2_p75 = pctl(sum_series(reps[keep]))
        out[w] = dict(n=n, sum_p75=sum_p75,
                      rep_p75_min=float(np.nanmin(rep_p75)),
                      rep_p75_med=float(np.nanmedian(rep_p75)),
                      rep_p75_max=float(np.nanmax(rep_p75)),
                      drop2_p75=drop2_p75)
    return out

def main():
    data = json.load(open(POD_CACHE))
    labels = [l for l in ORDER if l in data]
    res = {}
    for lab in labels:
        res[lab] = {"cpu": analyze(data[lab]["cpu"]),
                    "mem": analyze(data[lab]["mem"])}

    # ---- CSV (exhaustive) ----
    header = ["nodes", "workload", "n_replicas",
              "cpu_sum_p75_mc", "cpu_rep_p75_min_mc", "cpu_rep_p75_med_mc", "cpu_rep_p75_max_mc",
              "cpu_save_2of3_mc", "cpu_save_dropLSB_mc",
              "mem_sum_p75_MiB", "mem_rep_p75_min_MiB", "mem_rep_p75_med_MiB", "mem_rep_p75_max_MiB",
              "mem_save_2of3_MiB", "mem_save_dropLSB_MiB"]
    rows = []
    for lab in labels:
        wls = sorted(set(res[lab]["cpu"]) | set(res[lab]["mem"]))
        for w in wls:
            c = res[lab]["cpu"].get(w, {}); m = res[lab]["mem"].get(w, {})
            n = c.get("n", m.get("n", 0))
            def sav(d):
                if not d or d.get("n", 0) < 2 or np.isnan(d["sum_p75"]):
                    return ("", "")
                s2of3 = d["sum_p75"] / 3.0 if d["n"] == 3 else d["sum_p75"] * (1 - (d["n"]-1)/d["n"])
                sdrop = d["sum_p75"] - d["drop2_p75"] if d["n"] >= 2 else float("nan")
                return (f"{s2of3:.0f}", f"{sdrop:.0f}")
            csav = sav(c); msav = sav(m)
            rows.append([lab, w, n,
                f"{c.get('sum_p75',float('nan')):.0f}", f"{c.get('rep_p75_min',float('nan')):.0f}",
                f"{c.get('rep_p75_med',float('nan')):.0f}", f"{c.get('rep_p75_max',float('nan')):.0f}",
                csav[0], csav[1],
                f"{m.get('sum_p75',float('nan')):.0f}", f"{m.get('rep_p75_min',float('nan')):.0f}",
                f"{m.get('rep_p75_med',float('nan')):.0f}", f"{m.get('rep_p75_max',float('nan')):.0f}",
                msav[0], msav[1]])
    import csv as csvmod
    with open(REPORT_CSV, "w", newline="") as f:
        w = csvmod.writer(f); w.writerow(header); w.writerows(rows)

    # ---- Markdown ----
    def cores(mc): return mc / 1000.0
    def gib(mib): return mib / 1024.0
    L = []
    L.append("# ARO-HCP hosted control plane — per-workload p75 usage & 3→2 replica savings\n")
    L.append("Source: kube-burner `podCPU/podMemory-Controlplane` metrics (pod-level = sum of a pod's "
             "containers). Statistic: **p75 over the whole run** of the summed-replica time series "
             "(the top of the box in the earlier plots). CPU in millicores (1000 = 1 core), memory = "
             "working-set MiB.\n")
    L.append("**Savings models for dropping 1 of 3 replicas** (both shown because the true behavior is "
             "load-dependent):\n")
    L.append("- **2/3-of-sum**: assumes total workload usage scales linearly with replica count → saving "
             "= ⅓ × sum_p75. Optimistic for stateless/idle-bound components; wrong for load-bound ones "
             "whose requests just redistribute.\n")
    L.append("- **drop-least-busy (LSB)**: recomputes p75 of the sum of the 2 busiest replicas and "
             "subtracts → saving = the least-busy replica's marginal contribution. Conservative; ignores "
             "load redistribution onto the survivors.\n")
    L.append("\n**Interpreting CPU vs memory:**\n")
    L.append("- **CPU is load-bound**: the API request volume is fixed by the cluster, so removing a "
             "replica mostly *redistributes* work onto the survivors. The realistic CPU saving is only "
             "the per-replica *baseline* (idle) cost — expect it near or below the drop-LSB column, well "
             "under 2/3-of-sum.\n")
    L.append("- **Memory is mostly per-replica state** (each API server / etcd keeps its own watch "
             "cache / DB working set, sized by the cluster, not by replica count). Dropping a replica "
             "removes ~a full replica's footprint, so the memory saving is *real* and 2/3-of-sum ≈ "
             "drop-LSB ≈ one replica. This is where the compute win actually is.\n")
    L.append("\nFull per-workload detail (incl. per-replica min/med/max) is in `hcp-replica-savings.csv`.\n")

    def candidates(lab):
        cc = res[lab]["cpu"]
        return sorted([w for w in cc if cc[w]["n"] == 3],
                      key=lambda w: -(res[lab]["mem"].get(w, {}).get("sum_p75", 0) or 0))

    L.append("\n## Cluster-wide 3→2 savings (summed over all 3-replica workloads)\n")
    L.append("| nodes | CPU 2/3-of-sum | CPU drop-LSB | Mem 2/3-of-sum | Mem drop-LSB |")
    L.append("|--:|--:|--:|--:|--:|")
    for lab in labels:
        cc = res[lab]["cpu"]; mm = res[lab]["mem"]; cand = candidates(lab)
        c23 = sum(cc[w]["sum_p75"]/3 for w in cand)
        cds = sum(cc[w]["sum_p75"]-cc[w]["drop2_p75"] for w in cand)
        m23 = sum(mm[w]["sum_p75"]/3 for w in cand if w in mm)
        mds = sum(mm[w]["sum_p75"]-mm[w]["drop2_p75"] for w in cand if w in mm)
        L.append(f"| {lab} | {cores(c23):.2f} cores | {cores(cds):.2f} cores "
                 f"| {gib(m23):.2f} GiB | {gib(mds):.2f} GiB |")

    for lab in labels:
        L.append(f"\n## {lab} nodes — 3-replica workloads\n")
        L.append("| workload | CPU sum p75 (mc) | CPU/replica p75 (mc) | CPU save 2/3 | CPU save LSB "
                 "| Mem sum p75 (MiB) | Mem/replica p75 (MiB) | Mem save 2/3 | Mem save LSB |")
        L.append("|---|--:|--:|--:|--:|--:|--:|--:|--:|")
        cc = res[lab]["cpu"]; mm = res[lab]["mem"]
        for w in candidates(lab):
            c = cc.get(w); m = mm.get(w)
            if not c or c["n"] != 3:
                continue
            L.append(f"| {w} | {c['sum_p75']:.0f} | {c['rep_p75_med']:.0f} "
                     f"| {c['sum_p75']/3:.0f} | {c['sum_p75']-c['drop2_p75']:.0f} "
                     f"| {m['sum_p75']:.0f} | {m['rep_p75_med']:.0f} "
                     f"| {m['sum_p75']/3:.0f} | {m['sum_p75']-m['drop2_p75']:.0f} |")

    for lab in labels:
        L.append(f"\n## {lab} nodes — all workloads (p75)\n")
        L.append("| workload | reps | CPU sum p75 (mc) | CPU/replica med (mc) | Mem sum p75 (MiB) | Mem/replica med (MiB) |")
        L.append("|---|--:|--:|--:|--:|--:|")
        cc = res[lab]["cpu"]; mm = res[lab]["mem"]
        wls = sorted(set(cc) | set(mm), key=lambda w: -(mm.get(w, {}).get("sum_p75", 0) or 0))
        for w in wls:
            c = cc.get(w, {}); m = mm.get(w, {})
            n = c.get("n", m.get("n", 0))
            L.append(f"| {w} | {n} | {c.get('sum_p75',float('nan')):.0f} | {c.get('rep_p75_med',float('nan')):.0f} "
                     f"| {m.get('sum_p75',float('nan')):.0f} | {m.get('rep_p75_med',float('nan')):.0f} |")

    with open(REPORT_MD, "w") as f:
        f.write("\n".join(L) + "\n")
    print("wrote", REPORT_CSV)
    print("wrote", REPORT_MD)
    print("\n=== cluster-wide 3->2 savings (3-replica workloads) ===")
    print(f"{'nodes':>5} {'CPU 2/3':>9} {'CPU LSB':>9} {'Mem 2/3':>10} {'Mem LSB':>10}")
    for lab in labels:
        cc = res[lab]["cpu"]; mm = res[lab]["mem"]; cand = candidates(lab)
        c23 = sum(cc[w]["sum_p75"]/3 for w in cand)
        cds = sum(cc[w]["sum_p75"]-cc[w]["drop2_p75"] for w in cand)
        m23 = sum(mm[w]["sum_p75"]/3 for w in cand if w in mm)
        mds = sum(mm[w]["sum_p75"]-mm[w]["drop2_p75"] for w in cand if w in mm)
        print(f"{lab:>5} {cores(c23):8.2f}c {cores(cds):8.2f}c {gib(m23):9.2f}G {gib(mds):9.2f}G  "
              f"[{len(cand)} 3-rep workloads]")

if __name__ == "__main__":
    main()
