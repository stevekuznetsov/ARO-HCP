#!/usr/bin/env python3
"""Fixed readiness-helper CPU, immutable v4 memory, shared quota allocator/renderer."""

from copy import deepcopy
from fractions import Fraction as F
import json
from pathlib import Path

import cost_allocate_v4 as shared


HELPERS = {("openshift-apiserver", "kas-readiness-check"): 50,
           ("packageserver", "kas-readiness-check"): 50}


def derive(root):
    q = shared.q
    hashes = {}

    def read(relative, sha=None):
        raw = (root / relative).read_bytes()
        actual = q.v1.digest(raw)
        if sha and sha != actual:
            raise ValueError(f"historical bytes changed: {relative}")
        hashes[relative] = actual
        return json.loads(raw)

    history = "_artifacts/small-cost-v4/"
    old = read(history + "allocation.json", "f4d80e5d9d301ce6b54bd92934f1fa3792b86269c21a676a6fd4edf494fae16a")
    for filename, item in read(history + "output-hashes.json").items():
        read(history + filename, item["sha256"])
    parent = read(old["profile"]["path"], old["profile"]["sha256"])
    diagnosis_path = history + "lifecycle/diagnostic-continuation/result.json"
    diagnosis = read(diagnosis_path)
    if diagnosis["status"] != "diagnosed-readiness-helper-cpu-starvation":
        raise ValueError("unexpected diagnosis")
    receipts = history + "lifecycle/diagnostic-continuation/confirm-1789145875/"
    for filename in ("pods.json", "endpoints.json"):
        read(receipts + filename)
    source = read("_artifacts/small-cost-v1/allocation.json", "97b50a5065313a1d18de90310b8a31ff5b0df1733ae1d63c16d47ab4bd02eb68")["selected"]["small-cost-v1"]["rows"]
    pod_path = "_artifacts/perf-resource-locked-scan/density-v6-apps60/before/full-pods.json"
    inputs = read("_artifacts/small-constrained-v1/analysis/input-hashes.json", "a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e")
    pods = {p["metadata"]["uid"]: p for p in read(pod_path, inputs["files"][pod_path]["sha256"])["items"]}
    model = deepcopy(old["model"])
    rows = model["rows"]
    if [r["identity"] for r in rows] != [r["identity"] for r in source]:
        raise ValueError("ratio inventory mismatch")
    index = {(r["identity"]["workload"], r["identity"]["container"]): i for i, r in enumerate(rows)}
    if any(rows[index[k]]["replicas"] != 3 for k in HELPERS):
        raise ValueError("expected six running helper replicas")
    assumptions = old["quotaAssumptions"]
    period, minimum, quantum = (assumptions[k] for k in ("cpuPeriodUS", "minQuotaUS", "quotaQuantumUS"))

    def groups(values):
        result = []
        for group in model["podGroups"]:
            spec = pods[group["uid"]]["spec"]
            if spec.get("resources") or spec.get("overhead"):
                raise ValueError("unaccounted Pod resources")
            cpu = lambda c: max(1, values[index[(group["workload"], c["name"])]])
            quota = q.pod_quota([cpu(c) for c in spec["containers"]],
                                [(cpu(c), c.get("restartPolicy") == "Always") for c in spec.get("initContainers", [])],
                                period, minimum, quantum)
            result.append({**group, **quota})
        return result

    def charge(values):
        return 470 + sum(F(g["effectiveQuotaMilli"]) for g in groups(values))

    cap = F(16000) * F(".125") / F(".806")
    scale, values = shared.fixed_memory(source, HELPERS, cap, charge, field="cpuMilli")
    deltas = []
    for row, cpu in zip(rows, values):
        previous = row["requests"]["cpuMilli"]
        row["requests"]["cpuMilli"] = cpu
        deltas.append({"identity": row["identity"], "replicas": row["replicas"], "oldCPUMilli": previous,
                       "newCPUMilli": cpu, "replicatedDeltaMilli": (cpu - previous) * row["replicas"]})
    model["podGroups"] = groups(values)
    model["cpuScale"] = str(scale)
    model["declaredControlledCPUMilli"] = sum(v * r["replicas"] for v, r in zip(values, rows))
    model["declaredOfferingCPUMilli"] = model["declaredControlledCPUMilli"] + 470
    model["effectiveOfferingCPUMilli"] = str(charge(values))
    model["effectiveControlledCPUMilli"] = str(charge(values) - 470)
    model["unusedEffectiveCPUMilli"] = str(cap - charge(values))
    model["quotaAwareUSDPerHour"] = str(F(".806") * max(charge(values) / 16000, F(model["offeringChargedMemoryMiB"], 65536)))
    if F(model["quotaAwareUSDPerHour"]) > F(".125"):
        raise ValueError("fixed cost envelope exceeded")
    model["fallback"]["cpuMilli"] = int(10 * scale)
    profile = deepcopy(parent)
    profile["name"] = "small-cost-v5"
    profile["description"] = f"OFFLINE new treatment: OAS/packageserver kas-readiness-check fixed 50m each, six benchmark replicas. Other CPU proportional to original weights within $0.125/HCP-hour including observed quota quantum/Pod caps. V4 memory EXACT, including HAProxy96Mi/106Mi. No probe/code/image/APF changes. Fallback --default-cpu-milli {model['fallback']['cpuMilli']} --default-memory-mib {model['fallback']['memoryMiB']}. Fresh startup confirmation required even if an explicitly authorized in-place treatment succeeds."
    for entry in profile["containers"]:
        k = entry["identity"]["workload"], entry["identity"]["container"]
        entry["floor"]["cpuMilli"] = values[index[k]]
    profile["anchor"]["cpuMilli"] = values[index[("kube-apiserver", "kube-apiserver")]]
    for before, after in zip(old["model"]["rows"], rows):
        if before["requests"]["memoryMiB"] != after["requests"]["memoryMiB"] or before["memoryLimitMiB"] != after["memoryLimitMiB"]:
            raise ValueError("memory changed")
    for path in (Path(__file__), Path(__file__).with_name("test_cost_allocate_v5.py"), Path(shared.__file__), Path(q.__file__), Path(q.v1.__file__)):
        hashes[str(path.relative_to(root))] = q.v1.digest(path.read_bytes())
    for relative in ("tooling/hcp-perf/internal/sizing/sizing.go", "tooling/hcp-perf/internal/sizing/hypershift.go"):
        hashes[relative] = q.v1.digest((root / relative).read_bytes())
    return {"version": "v5", "qualified": False, "startupTested": False, "model": model,
            "cpuDeltas": sorted(deltas, key=lambda d: (-abs(d["replicatedDeltaMilli"]), str(d["identity"]))),
            "sourceHashes": hashes, "quotaAssumptions": assumptions, "coverage": old["coverage"],
            "fixedCPUOverrides": [{"workload": k[0], "container": k[1], "replicas": 3, "requestAndLimitMilli": v} for k, v in HELPERS.items()],
            "diagnosis": {"path": diagnosis_path, "at": diagnosis["at"], "probe": diagnosis["probe"], "directChecksFromCPO": diagnosis["directChecksFromCPO"]},
            "hypothesis": "50m may let shell/curl readiness complete within unchanged 5s deadline. Other CPU reductions can expose new bottlenecks; current-HCP treatment success is not fresh-start qualification."}, profile


if __name__ == "__main__":
    shared.main(version="v5", derive_fn=derive, parent_version="v4", resource="cpu")
