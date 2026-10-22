#!/usr/bin/env python3
"""Spend existing V5 slack on exactly CVO memory and KCM CPU; no rescaling."""

from copy import deepcopy
from fractions import Fraction as F
import json
from pathlib import Path

import cost_allocate_v4 as shared


CVO = ("cluster-version-operator", "cluster-version-operator")
KCM = ("kube-controller-manager", "kube-controller-manager")
ALLOWED = {CVO: ("memory",), KCM: ("cpu",)}


def derive(root):
    q = shared.q
    hashes = {}

    def read(relative, expected=None):
        raw = (root / relative).read_bytes()
        sha = q.v1.digest(raw)
        if expected and sha != expected:
            raise ValueError(f"historical hash mismatch: {relative}")
        hashes[relative] = sha
        return json.loads(raw)

    history = "_artifacts/small-cost-v5/"
    old = read(history + "allocation.json", "2d2f77364603f1ea60bcdc62959083961f21ff561342cce63688ab47f042c56d")
    for name, item in read(history + "output-hashes.json").items():
        read(history + name, item["sha256"])
    profile = read(old["profile"]["path"], old["profile"]["sha256"])
    diagnosis = read(history + "lifecycle/recovery/result.json")
    if diagnosis["status"] != "readiness-helper-fixed-next-sizing-blockers":
        raise ValueError("unexpected recovery diagnosis")
    frozen = read("_artifacts/small-constrained-v1/analysis/candidate-budgets.json", "c0617bddb79fa78d2c652895dc28ffa75dc041237523f3990670dfad13b375a8")
    cvo_evidence = next(r for r in frozen["budgets"] if (r["selector"]["workload"], r["selector"]["container"]) == CVO)
    pod_path = frozen["snapshots"][-1]["path"] + "/full-pods.json"
    inputs = read("_artifacts/small-constrained-v1/analysis/input-hashes.json", "a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e")
    pods = {p["metadata"]["uid"]: p for p in read(pod_path, inputs["files"][pod_path]["sha256"])["items"]}
    model = deepcopy(old["model"])
    rows = {(r["identity"]["workload"], r["identity"]["container"]): r for r in model["rows"]}
    if rows[CVO]["replicas"] != 1 or rows[KCM]["replicas"] != 2:
        raise ValueError("unexpected CVO/KCM replica counts")
    rows[CVO]["requests"]["memoryMiB"] = 64
    rows[CVO]["memoryLimitMiB"] = 71
    rows[KCM]["requests"]["cpuMilli"] = 20
    period, minimum, quantum = (old["quotaAssumptions"][k] for k in ("cpuPeriodUS", "minQuotaUS", "quotaQuantumUS"))
    for group in model["podGroups"]:
        spec = pods[group["uid"]]["spec"]
        if spec.get("overhead") or spec.get("resources"):
            raise ValueError("unaccounted Pod resources")
        def row(c):
            return rows[(group["workload"], c["name"])]
        group.update(q.pod_quota([row(c)["requests"]["cpuMilli"] for c in spec["containers"]],
                                [(row(c)["requests"]["cpuMilli"], c.get("restartPolicy") == "Always") for c in spec.get("initContainers", [])],
                                period, minimum, quantum))
        group["chargedMemoryMiB"] = q.v1.scheduling_charge(spec, lambda c, kind: [0, 0, row(c)["memoryLimitMiB"]])[2]
    model["declaredControlledCPUMilli"] = sum(r["replicas"] * r["requests"]["cpuMilli"] for r in model["rows"])
    model["declaredOfferingCPUMilli"] = model["declaredControlledCPUMilli"] + 470
    cpu = sum(F(g["effectiveQuotaMilli"]) for g in model["podGroups"])
    model["effectiveControlledCPUMilli"] = str(cpu)
    model["effectiveOfferingCPUMilli"] = str(cpu + 470)
    model["controlledRequestMemoryMiB"] = sum(r["replicas"] * r["requests"]["memoryMiB"] for r in model["rows"])
    model["offeringRequestMemoryMiB"] = model["controlledRequestMemoryMiB"] + 2090
    memory = sum(g["chargedMemoryMiB"] for g in model["podGroups"])
    model["controlledChargedMemoryMiB"] = memory
    model["offeringChargedMemoryMiB"] = memory + 2090
    model["quotaAwareUSDPerHour"] = str(F(".806") * max((cpu + 470) / 16000, F(memory + 2090, 65536)))
    model["unusedEffectiveCPUMilli"] = str(F(16000) * F(".125") / F(".806") - cpu - 470)
    model["unusedChargedMemoryMiB"] = str(F(65536) * F(".125") / F(".806") - memory - 2090)
    if F(model["quotaAwareUSDPerHour"]) > F(".125"):
        raise ValueError("targeted changes exceed budget; no silent rescaling")
    expected = deepcopy(old["model"]["rows"])
    for r in expected:
        key = r["identity"]["workload"], r["identity"]["container"]
        if key == CVO:
            r["requests"]["memoryMiB"], r["memoryLimitMiB"] = 64, 71
        if key == KCM:
            r["requests"]["cpuMilli"] = 20
    if model["rows"] != expected:
        raise ValueError("unintended container change")
    profile["name"] = "small-cost-v6"
    profile["description"] = "OFFLINE new treatment within unchanged $0.125/HCP-hour raw D16s_v6 envelope. Only CVO main memory64Mi/71Mi and two KCM main CPU20m requests/limits change from V5. No other rescaling; six readiness helpers50m, HAProxy96Mi/106Mi, catalogs and policies preserved. Fallback --default-cpu-milli 2 --default-memory-mib 32. Startup untested; failed RP operation requires separately supported recovery."
    for entry in profile["containers"]:
        key = entry["identity"]["workload"], entry["identity"]["container"]
        if key == CVO:
            entry["floor"]["memoryMiB"] = 64
        if key == KCM:
            entry["floor"]["cpuMilli"] = 20
    for path in (Path(__file__), Path(__file__).with_name("test_cost_allocate_v6.py"), Path(shared.__file__), Path(q.__file__)):
        hashes[str(path.relative_to(root))] = q.v1.digest(path.read_bytes())
    for relative in ("tooling/hcp-perf/internal/sizing/sizing.go", "tooling/hcp-perf/internal/sizing/hypershift.go"):
        hashes[relative] = q.v1.digest((root / relative).read_bytes())
    return {"version": "v6", "qualified": False, "startupTested": False, "model": model, "sourceHashes": hashes,
            "coverage": old["coverage"], "quotaAssumptions": old["quotaAssumptions"], "rescaledOtherContainers": False,
            "changes": [{"workload": CVO[0], "container": CVO[1], "replicas": 1, "memoryRequestMiB": [41, 64], "memoryLimitMiB": [46, 71], "GOMEMLIMITBytes": [34393292, 53687091]},
                        {"workload": KCM[0], "container": KCM[1], "replicas": 2, "cpuRequestAndLimitMilli": [9, 20], "effectiveQuotaMilli": [10, 20]}],
            "evidence": {"path": history + "lifecycle/recovery/result.json", "at": diagnosis["at"],
                         "probeWallSeconds": diagnosis["probeWallSeconds"], "oasEndpoints": diagnosis["endpoints"]["openshift-apiserver"],
                         "cvo": diagnosis["nextSizingRequest"], "cpuReview": diagnosis["cpuReviewRequests"],
                         "historicalCVOChargePeakBytes": cvo_evidence["coveredQuietEvidence"]["chargeBytes"]["max"],
                         "rpOperationStatus": diagnosis["operation"]["status"], "lifecycleBlock": diagnosis["lifecycleBlock"]},
            "hypothesis": "CVO71Mi limit and KCM20m quotas may clear diagnosed failures, not proven minima. No catalog/controller guesswork. Recovery on current HCP does not change failed RP operation or qualify fresh startup."}, profile


if __name__ == "__main__":
    shared.main(version="v6", derive_fn=derive, parent_version="v5", allowed_changes=ALLOWED)
