#!/usr/bin/env python3
"""Build compact per-size, per-component usage profiles for the scheduling simulator
from the control-plane pod time-series cache produced by hack/hcp-scale-analysis.

Output: sample_inputs/profiles.json
  {
    "sizes": ["3","6","12","30","49","60","120","250"],
    "profiles": {
       "<size>": {
          "<component>": {"cpu_mc": [...per-replica pooled whole-run samples...],
                          "mem_mib": [...],
                          "replicas": <observed concurrent replica count>}
       }
    },
    "router_profile": {"cpu_mc":[...], "mem_mib":[...]}   # from the 49-node run (only run with a router)
  }

Percentiles are computed at run time from these pooled samples, so the usage->request
transform (percentile + multiplier) stays a live input.
"""
import json, os, sys
from collections import defaultdict

# Cache produced by hack/hcp-scale-analysis/extract_all.py
CACHE = os.environ.get(
    "ARO_HCP_SCALE_POD_CACHE",
    os.path.expanduser("~/Downloads/controlplane-pod-timeseries.json"),
)
OUT = os.path.join(os.path.dirname(os.path.dirname(__file__)), "sample_inputs", "profiles.json")
SIZES = ["3", "6", "12", "30", "49", "60", "120", "250"]

def workload(pod):
    s = pod.split("-")
    return "-".join(s[:-1]) if s[-1].isdigit() else "-".join(s[:-2])

def size_profiles(dataset):
    """dataset: {'cpu':{pod:[[t,v]]}, 'mem':{...}} -> {component:{cpu_mc,mem_mib,replicas}}."""
    pods_cpu = defaultdict(list)   # component -> [pod,...]
    for pod in dataset["cpu"]:
        pods_cpu[workload(pod)].append(pod)
    pods_mem = defaultdict(list)
    for pod in dataset["mem"]:
        pods_mem[workload(pod)].append(pod)
    comps = set(pods_cpu) | set(pods_mem)
    out = {}
    for c in comps:
        cpu = [v for p in pods_cpu.get(c, []) for _, v in dataset["cpu"][p]]
        mem = [v for p in pods_mem.get(c, []) for _, v in dataset["mem"][p]]
        replicas = max(len(pods_cpu.get(c, [])), len(pods_mem.get(c, [])))
        out[c] = {"cpu_mc": cpu, "mem_mib": mem, "replicas": replicas}
    return out

def main():
    if not os.path.exists(CACHE):
        sys.exit(f"pod cache not found: {CACHE}\n"
                 f"Run hack/hcp-scale-analysis/extract_all.py first (or set ARO_HCP_SCALE_POD_CACHE).")
    data = json.load(open(CACHE))
    profiles = {}
    for sz in SIZES:
        if sz not in data:
            print(f"WARN: size {sz} missing from cache", file=sys.stderr); continue
        profiles[sz] = size_profiles(data[sz])

    # Router profile: only the 49-node run actually deployed a `router` (haproxy w/ SWIFT NIC).
    router = {"cpu_mc": [], "mem_mib": []}
    if "49" in data:
        rp_cpu = [v for p in data["49"]["cpu"] if p.startswith("router-") for _, v in data["49"]["cpu"][p]]
        rp_mem = [v for p in data["49"]["mem"] if p.startswith("router-") for _, v in data["49"]["mem"][p]]
        router = {"cpu_mc": rp_cpu, "mem_mib": rp_mem}

    out = {"sizes": [s for s in SIZES if s in profiles], "profiles": profiles, "router_profile": router}
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w") as f:
        json.dump(out, f)
    # summary
    print(f"wrote {OUT}")
    print(f"router samples: cpu={len(router['cpu_mc'])} mem={len(router['mem_mib'])}")
    for sz in out["sizes"]:
        print(f"  size {sz:>4}: {len(profiles[sz])} components")

if __name__ == "__main__":
    main()
