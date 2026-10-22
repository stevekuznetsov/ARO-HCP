#!/usr/bin/env python3
"""Target storage memory; preserve V7 CPU and audit quiet-memory risks offline."""

from copy import deepcopy
from fractions import Fraction as F
import json
from pathlib import Path

import cost_allocate_v4 as shared


STORAGE = ("cluster-storage-operator", "cluster-storage-operator")


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

    history = "_artifacts/small-cost-v7/"
    old = read(history + "allocation.json", "cb26e24d3ae437b678656c20a623185b33861777fb6f290e75067c79a23510be")
    for name, item in read(history + "output-hashes.json").items():
        read(history + name, item["sha256"])
    profile = read(old["profile"]["path"], old["profile"]["sha256"])
    diagnosis = read(history + "lifecycle/fresh/first-oom.json")
    if not any((o["workload"], o["container"]) == STORAGE and o["status"]["state"]["terminated"]["reason"] == "OOMKilled" for o in diagnosis["ooms"]):
        raise ValueError("storage OOM evidence missing")
    frozen = read("_artifacts/small-constrained-v1/analysis/candidate-budgets.json", "c0617bddb79fa78d2c652895dc28ffa75dc041237523f3990670dfad13b375a8")
    inputs = read("_artifacts/small-constrained-v1/analysis/input-hashes.json", "a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e")
    pod_path = frozen["snapshots"][-1]["path"] + "/full-pods.json"
    pods = {p["metadata"]["uid"]: p for p in read(pod_path, inputs["files"][pod_path]["sha256"])["items"]}
    source = read("_artifacts/small-cost-v1/allocation.json", "97b50a5065313a1d18de90310b8a31ff5b0df1733ae1d63c16d47ab4bd02eb68")["selected"]["small-cost-v1"]["rows"]
    model = deepcopy(old["model"])
    rows = model["rows"]
    keys = [(r["identity"]["workload"], r["identity"]["container"]) for r in rows]
    index = {k: i for i, k in enumerate(keys)}
    if len(index) != len(rows) or [r["identity"] for r in rows] != [r["identity"] for r in source]:
        raise ValueError("ambiguous or changed inventory")
    fixed = {(r["workload"], r["container"]): r["memoryMiB"] for r in old["protectedMemory"]}
    fixed[STORAGE] = 64

    def memory_groups(values):
        result = []
        for group in model["podGroups"]:
            spec = pods[group["uid"]]["spec"]
            if spec.get("resources") or spec.get("overhead"):
                raise ValueError("unaccounted Pod resources")
            result.append(q.v1.scheduling_charge(spec, lambda c, kind: [0, 0, (values[index[(group["workload"], c["name"])]] * 110 + 99) // 100])[2])
        return result

    charge = lambda values: 2090 + sum(memory_groups(values))
    capacity = F(4096000, 403)
    values = [fixed.get(k, r["requests"]["memoryMiB"]) for k, r in zip(keys, rows)]
    fixed_only_charge = charge(values)
    # Avoid unnecessary redistribution: init maxima can already reserve the increase.
    scale = F(model["memoryScale"])
    if fixed_only_charge > capacity:
        scale, values = shared.fixed_memory(source, fixed, capacity, charge)
    deltas = []
    for key, row, memory in zip(keys, rows, values):
        deltas.append({"identity": row["identity"], "replicas": row["replicas"], "oldRequestMiB": row["requests"]["memoryMiB"],
                       "newRequestMiB": memory, "oldLimitMiB": row["memoryLimitMiB"], "newLimitMiB": (memory * 110 + 99) // 100})
        row["requests"]["memoryMiB"] = memory
        row["memoryLimitMiB"] = (memory * 110 + 99) // 100
    for group, memory in zip(model["podGroups"], memory_groups(values)):
        group["chargedMemoryMiB"] = memory
    model["memoryScale"] = str(scale)
    model["controlledRequestMemoryMiB"] = sum(v * r["replicas"] for v, r in zip(values, rows))
    model["offeringRequestMemoryMiB"] = model["controlledRequestMemoryMiB"] + 2090
    model["controlledChargedMemoryMiB"] = charge(values) - 2090
    model["offeringChargedMemoryMiB"] = charge(values)
    model["steadyOfferingMemoryLimitChargeMiB"] = 2090 + sum(r["memoryLimitMiB"] * r["replicas"] for r in rows)
    model["initStageExtraMemoryMiB"] = charge(values) - model["steadyOfferingMemoryLimitChargeMiB"]
    model["unusedChargedMemoryMiB"] = str(capacity - charge(values))
    model["quotaAwareUSDPerHour"] = str(F(".806") * max(F(model["effectiveOfferingCPUMilli"]) / 16000, F(charge(values), 65536)))
    if F(model["quotaAwareUSDPerHour"]) > F(".125"):
        raise ValueError("cost envelope exceeded")
    model["fallback"]["memoryMiB"] = int(64 * scale)
    profile["name"] = "small-cost-v8"
    profile["description"] = f"OFFLINE storage-operator memory64Mi/71Mi hypothesis after V7 OOM35Mi. V7 CPU EXACT, protected HAProxy96/CVO64/CPO65/init64 memory retained. Pod init maxima included within $0.125/HCP-hour raw D16s_v6. Other memory rebalanced only if needed. No probe/image/policy changes. Fallback --default-cpu-milli {model['fallback']['cpuMilli']} --default-memory-mib {model['fallback']['memoryMiB']}. Startup untested."
    for entry in profile["containers"]:
        key = entry["identity"]["workload"], entry["identity"]["container"]
        entry["floor"]["memoryMiB"] = values[index[key]]
    profile["anchor"]["memoryMiB"] = values[index[("kube-apiserver", "kube-apiserver")]]
    budgets = {(b["selector"]["workload"], b["selector"]["container"]): b for b in frozen["budgets"] if b["selector"]["containerGroup"] == "containers"}
    audit = []
    for key, before, after in zip(keys, old["model"]["rows"], rows):
        if after["identity"].get("initContainer") or key not in budgets or not after["replicas"]:
            continue
        budget = budgets[key]
        stats = budget["coveredQuietEvidence"]
        metrics = {m: {s: stats[m][s] / 1048576 if stats[m][s] is not None else None for s in ("p95", "max")}
                   for m in ("cgroupRSSBytes", "workingSetBytes", "chargeBytes")}
        maxima = [metrics[m]["max"] for m in ("cgroupRSSBytes", "workingSetBytes") if metrics[m]["max"] is not None]
        p95s = [metrics[m]["p95"] for m in ("cgroupRSSBytes", "workingSetBytes") if metrics[m]["p95"] is not None]
        main = key[1] == key[0] or key[1] in ("registry", "manager", "migrator", "cloud-controller-manager", "haproxy")
        audit.append({"identity": after["identity"], "replicas": after["replicas"], "mainContainerHeuristic": main,
                      "v7RequestMiB": before["requests"]["memoryMiB"], "v7LimitMiB": before["memoryLimitMiB"],
                      "v8RequestMiB": after["requests"]["memoryMiB"], "v8LimitMiB": after["memoryLimitMiB"],
                      "quietMetricsMiB": metrics, "quietP95OverV8LimitMiB": max(p95s) - after["memoryLimitMiB"] if p95s else None,
                      "quietMaxOverV8LimitMiB": max(maxima) - after["memoryLimitMiB"] if maxima else None,
                      "quietP95OverV7RequestMiB": max(p95s) - before["requests"]["memoryMiB"] if p95s else None})
    audit.sort(key=lambda r: (-(r["quietP95OverV8LimitMiB"] if r["quietP95OverV8LimitMiB"] is not None else -1e9), str(r["identity"])))
    risks = [r for r in audit if r["mainContainerHeuristic"] and r["quietMaxOverV8LimitMiB"] is not None and r["quietMaxOverV8LimitMiB"] > 0]
    for path in (Path(__file__), Path(__file__).with_name("test_cost_allocate_v8.py"), Path(shared.__file__), Path(q.__file__), Path(q.v1.__file__)):
        hashes[str(path.relative_to(root))] = q.v1.digest(path.read_bytes())
    for relative in ("tooling/hcp-perf/internal/sizing/sizing.go", "tooling/hcp-perf/internal/sizing/hypershift.go"):
        hashes[relative] = q.v1.digest((root / relative).read_bytes())
    return {"version": "v8", "qualified": False, "startupTested": False, "model": model, "memoryDeltas": deltas,
            "sourceHashes": hashes, "coverage": old["coverage"], "quotaAssumptions": old["quotaAssumptions"],
            "protectedMemory": [{"workload": k[0], "container": k[1], "memoryMiB": v} for k, v in fixed.items()],
            "fixedOnlyOfferingChargedMemoryMiB": fixed_only_charge, "rebalancedOthers": fixed_only_charge > capacity,
            "diagnosis": {"path": history + "lifecycle/fresh/first-oom.json", "capturedAt": diagnosis["at"],
                          "finishedAt": diagnosis["ooms"][0]["status"]["state"]["terminated"]["finishedAt"],
                          "resources": diagnosis["ooms"][0]["resources"], "runtime": diagnosis["ooms"][0]["runtime"]},
            "quietMemoryAudit": audit, "mainMemoryRisks": risks,
            "riskMethod": "All running controlled regular rows audited; main classification is name-based heuristic (container=workload or registry/manager/migrator/cloud-controller-manager/haproxy), not ownership proof. Sort by largest per-container pooled quiet RSS/working-set p95 deficit, then max. RSS/working-set lack own source timestamp queries: descriptive, not fresh startup requirements. Charge shown separately; not substituted for RSS. Missing remains null; no risk automatically allocated.",
            "hypothesis": "Storage64Mi/71Mi exceeds old quiet charge46.2Mi and failed35Mi cap; not a guaranteed startup minimum. Other operators may fail next."}, profile


if __name__ == "__main__":
    shared.main(version="v8", derive_fn=derive, parent_version="v7", resource="memory")
