#!/usr/bin/env python3
"""Fresh-start candidate with fixed startup exceptions and Pod-stage budget checks."""

from copy import deepcopy
from fractions import Fraction as F
import json
from pathlib import Path

import cost_allocate_v4 as shared


def derive(root):
    q = shared.q
    hashes = {}

    def read(path, expected=None):
        raw = (root / path).read_bytes()
        sha = q.v1.digest(raw)
        if expected and sha != expected:
            raise ValueError(f"historical hash mismatch: {path}")
        hashes[path] = sha
        return json.loads(raw)

    history = "_artifacts/small-cost-v6/"
    old = read(history + "allocation.json", "2751dbad3a6669f4f64aae5454deeb39b5f56f58d0e1eded4d79cf2d0b75e07c")
    for name, item in read(history + "output-hashes.json").items():
        read(history + name, item["sha256"])
    parent = read(old["profile"]["path"], old["profile"]["sha256"])
    diagnosis = read(history + "lifecycle/result.json")
    source = read("_artifacts/small-cost-v1/allocation.json", "97b50a5065313a1d18de90310b8a31ff5b0df1733ae1d63c16d47ab4bd02eb68")["selected"]["small-cost-v1"]["rows"]
    inputs = read("_artifacts/small-constrained-v1/analysis/input-hashes.json", "a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e")
    pod_path = "_artifacts/perf-resource-locked-scan/density-v6-apps60/before/full-pods.json"
    pods = {p["metadata"]["uid"]: p for p in read(pod_path, inputs["files"][pod_path]["sha256"])["items"]}
    model = deepcopy(old["model"])
    rows = model["rows"]
    if [r["identity"] for r in rows] != [r["identity"] for r in source]:
        raise ValueError("source identity order mismatch")
    keys = [(r["identity"]["workload"], r["identity"]["container"]) for r in rows]
    if len(set(keys)) != len(keys):
        raise ValueError("projector cannot distinguish duplicate kind/init identities")
    index = {k: i for i, k in enumerate(keys)}
    init_keys = [k for k, r in zip(keys, rows) if r["identity"].get("initContainer") and k[1] == "availability-prober"]
    if len(init_keys) != 17:
        raise ValueError("expected 17 controlled availability-prober init rows")
    memory_fixed = {("ignition-server-proxy", "haproxy"): 96,
                    ("cluster-version-operator", "cluster-version-operator"): 64,
                    ("control-plane-operator", "control-plane-operator"): 65,
                    **{k: 64 for k in init_keys}}
    cpu_fixed = {("openshift-apiserver", "kas-readiness-check"): 50,
                 ("packageserver", "kas-readiness-check"): 50,
                 ("kube-controller-manager", "kube-controller-manager"): 20,
                 ("openshift-controller-manager", "openshift-controller-manager"): 20,
                 ("openshift-route-controller-manager", "openshift-route-controller-manager"): 20,
                 **{k: 2 for k in init_keys}}
    for k, r in zip(keys, rows):
        if k[1] == "registry":
            cpu_fixed[k] = r["requests"]["cpuMilli"]
    period, minimum, quantum = (old["quotaAssumptions"][k] for k in ("cpuPeriodUS", "minQuotaUS", "quotaQuantumUS"))
    layouts = [(g, pods[g["uid"]]["spec"]) for g in model["podGroups"]]
    init_instances = sum(c["name"] == "availability-prober" for _, spec in layouts for c in spec.get("initContainers", []))

    def cpu_groups(values):
        result = []
        for group, spec in layouts:
            if spec.get("resources") or spec.get("overhead"):
                raise ValueError("unaccounted Pod resources")
            cpu = lambda c: max(1, values[index[(group["workload"], c["name"])]])
            result.append(q.pod_quota([cpu(c) for c in spec["containers"]],
                          [(cpu(c), c.get("restartPolicy") == "Always") for c in spec.get("initContainers", [])], period, minimum, quantum))
        return result

    def memory_groups(values):
        return [q.v1.scheduling_charge(spec, lambda c, kind: [0, 0, (110 * values[index[(group["workload"], c["name"])]] + 99) // 100])[2]
                for group, spec in layouts]

    cpu_charge = lambda values: 470 + sum(F(g["effectiveQuotaMilli"]) for g in cpu_groups(values))
    mem_charge = lambda values: 2090 + sum(memory_groups(values))
    cpu_cap, mem_cap = F(16000) * F(".125") / F(".806"), F(65536) * F(".125") / F(".806")
    before_mem = [r["requests"]["memoryMiB"] for r in rows]
    fixed_only = [memory_fixed.get(k, v) for k, v in zip(keys, before_mem)]
    cpu_scale, cpus = shared.fixed_memory(source, cpu_fixed, cpu_cap, cpu_charge, field="cpuMilli")
    mem_scale, memories = shared.fixed_memory(source, memory_fixed, mem_cap, mem_charge)
    changes = []
    for key, row, cpu, memory in zip(keys, rows, cpus, memories):
        changes.append({"identity": row["identity"], "replicas": row["replicas"], "before": deepcopy(row["requests"]),
                        "after": {"cpuMilli": cpu, "memoryMiB": memory}, "oldMemoryLimitMiB": row["memoryLimitMiB"],
                        "newMemoryLimitMiB": (memory * 110 + 99) // 100,
                        "fixedCPU": key in cpu_fixed, "fixedMemory": key in memory_fixed})
        row["requests"] = {"cpuMilli": cpu, "memoryMiB": memory}
        row["memoryLimitMiB"] = (memory * 110 + 99) // 100
    for group, cpu, memory in zip(model["podGroups"], cpu_groups(cpus), memory_groups(memories)):
        group.update(cpu)
        group["chargedMemoryMiB"] = memory
    model["cpuScale"], model["memoryScale"] = str(cpu_scale), str(mem_scale)
    model["declaredControlledCPUMilli"] = sum(c * r["replicas"] for c, r in zip(cpus, rows))
    model["declaredOfferingCPUMilli"] = model["declaredControlledCPUMilli"] + 470
    model["effectiveControlledCPUMilli"] = str(cpu_charge(cpus) - 470)
    model["effectiveOfferingCPUMilli"] = str(cpu_charge(cpus))
    model["controlledRequestMemoryMiB"] = sum(m * r["replicas"] for m, r in zip(memories, rows))
    model["offeringRequestMemoryMiB"] = model["controlledRequestMemoryMiB"] + 2090
    model["controlledChargedMemoryMiB"] = mem_charge(memories) - 2090
    model["offeringChargedMemoryMiB"] = mem_charge(memories)
    steady_limits = sum(r["memoryLimitMiB"] * r["replicas"] for r in rows)
    model["steadyOfferingMemoryLimitChargeMiB"] = steady_limits + 2090
    model["initStageExtraMemoryMiB"] = mem_charge(memories) - steady_limits - 2090
    model["quotaAwareUSDPerHour"] = str(F(".806") * max(cpu_charge(cpus) / 16000, F(mem_charge(memories), 65536)))
    model["unusedEffectiveCPUMilli"] = str(cpu_cap - cpu_charge(cpus))
    model["unusedChargedMemoryMiB"] = str(mem_cap - mem_charge(memories))
    if F(model["quotaAwareUSDPerHour"]) > F(".125"):
        raise ValueError("startup-stage cost exceeds budget")
    model["fallback"] = {"cpuMilli": int(10 * cpu_scale), "memoryMiB": int(64 * mem_scale)}
    profile = deepcopy(parent)
    profile["name"] = "small-cost-v7"
    profile["description"] = f"OFFLINE FRESH-START candidate $0.125/HCP-hour raw D16s_v6.17 controlled availability-prober init rows64Mi/71Mi CPU2m.OCM/route mains20m; readiness50m/KCM20m and catalog CPU preserved.HAProxy96Mi/CVO64Mi/CPO65Mi memory protected.Other weights reallocated with per-Pod init maxima and quota quantum.No probe/image/APF change.Fallback --default-cpu-milli {model['fallback']['cpuMilli']} --default-memory-mib {model['fallback']['memoryMiB']}.Startup untested; not a rescue of failed RP operation."
    profile["containers"] = []
    for row in rows:
        identity = row["identity"]
        kas = identity == {"kind": "Deployment", "workload": "kube-apiserver", "container": "kube-apiserver"}
        if row["requests"] == model["fallback"] and not row["nonGoException"] and not kas:
            continue
        entry = {"identity": identity, "floor": row["requests"], "cpuRatio": {"numerator": int(kas), "denominator": 1},
                 "memoryRatio": {"numerator": int(kas), "denominator": 1}}
        if row["nonGoException"]:
            entry["goMemoryLimitPercent"] = 0
        profile["containers"].append(entry)
        if kas:
            profile["anchor"] = row["requests"]
    for path in (Path(__file__), Path(__file__).with_name("test_cost_allocate_v7.py"), Path(shared.__file__), Path(q.__file__), Path(q.v1.__file__)):
        hashes[str(path.relative_to(root))] = q.v1.digest(path.read_bytes())
    for relative in ("tooling/hcp-perf/internal/sizing/sizing.go", "tooling/hcp-perf/internal/sizing/hypershift.go"):
        hashes[relative] = q.v1.digest((root / relative).read_bytes())
    return {"version": "v7", "qualified": False, "startupTested": False, "freshAttemptRequired": True,
            "model": model, "changes": changes, "sourceHashes": hashes, "quotaAssumptions": old["quotaAssumptions"],
            "coverage": {**old["coverage"], "availabilityProberRows": len(init_keys), "availabilityProberInstancesInSavedPodLayouts": init_instances,
                         "unobservedProberWorkloads": [k[0] for k in init_keys if not any(g["workload"] == k[0] for g, _ in layouts)],
                         "bootstrapScope": "simultaneous per-Pod init maxima for saved benchmark replicas; excludes absent/disabled workloads, completed Jobs and rollout surge"},
            "fixedOnlyOfferingChargedMemoryMiB": mem_charge(fixed_only),
            "protectedCPU": [{"workload": k[0], "container": k[1], "cpuMilli": v} for k, v in cpu_fixed.items()],
            "protectedMemory": [{"workload": k[0], "container": k[1], "memoryMiB": v} for k, v in memory_fixed.items()],
            "diagnosis": {"path": history + "lifecycle/result.json", "at": diagnosis["at"], "operationStatus": diagnosis["operation"]["status"],
                          "ooms": [{"pod": o["pod"], "container": o["container"], "group": o["group"], "resources": o["resources"]} for o in diagnosis["summary"]["ooms"]]},
            "limitations": ["Partial V6 propagation on old failed RP operation does not test a clean V6 startup.",
                            "64Mi init and20m controller allowances are targeted hypotheses; further readiness/CPU/OOM failures remain possible.",
                            "Catalog charge includes cache: source RAM weights are allocation weights, not hard minimum heap requirements. Catalog CPU unchanged.",
                            "Fixed downstream requests may understate consumption; raw price/capacity is not a whole-namespace cap or actual bill."]}, profile


if __name__ == "__main__":
    shared.main(version="v7", derive_fn=derive, parent_version="v6", resource="both")
