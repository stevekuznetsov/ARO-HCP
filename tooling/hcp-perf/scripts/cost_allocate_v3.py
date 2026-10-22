#!/usr/bin/env python3
"""V3 observed-quantum model; reuse v2 stage accounting without rewriting history."""

from fractions import Fraction as F
import json
from pathlib import Path
import re

import cost_allocate_v2 as v2


def augment(root, result, profiles, args):
    def read(path, expected=None):
        path = root / path
        raw = path.read_bytes()
        sha = v2.v1.digest(raw)
        if expected and sha != expected:
            raise ValueError(f"immutable historical hash mismatch: {path}")
        result["sourceHashes"][str(path.relative_to(root))] = sha
        return json.loads(raw)

    old = read("_artifacts/small-cost-v2/allocation.json", "674094cb5d792b19c70f6aef66727ff3dd1484adf36396a8c523eb07338c8c3e")
    manifest = read("_artifacts/small-cost-v2/output-hashes.json")
    for name, item in manifest.items():
        read("_artifacts/small-cost-v2/" + name, item["sha256"])
    for item in old["profiles"].values():
        read(item["path"], item["sha256"])
    base = "_artifacts/small-cost-v1/lifecycle/cleanup/"
    analysis = read(base + "one-shot/quota-analysis.json")
    read(base + "before-delete/cpu-quota-nodeproxy.json")
    pods = read(base + "before-delete/pods.json")["items"]
    nodes = read(base + "before-delete/nodes.json")["items"]
    by_uid = {p["metadata"]["uid"]: p for p in pods}
    container_rows, parent_rows = [], []
    for row in analysis["rows"]:
        if row["period"] != 100000:
            raise ValueError("observed period differs; do not silently extrapolate")
        if row["level"] == "container" and row.get("admittedResources") and row["quota"] is not None:
            declared = int(v2.v1.b.quantity(row["admittedResources"]["limits"]["cpu"]) * 1000)
            predicted = v2.quota_milli(declared, 100000, 1000, 1000)
            if predicted != F(str(row["quota"])) * 1000 / 100000:
                raise ValueError("observed container contradicts quantum model")
            container_rows.append({"pod": row["pod"], "container": row["container"], "declaredMilli": declared,
                                   "quotaUS": int(row["quota"]), "modeledMilli": str(predicted)})
        if row["level"] != "pod-parent":
            continue
        match = re.search(r"-pod([0-9a-f_]+)\.slice$", row["cgroup"])
        if not match:
            raise ValueError("unrecognized parent UID")
        uid = match[1].replace("_", "-")
        pod = by_uid[uid]
        if pod["metadata"]["name"] != row["pod"]:
            raise ValueError("parent UID/name mismatch")
        spec = pod["spec"]

        def cpu(c):
            return int(v2.v1.b.quantity(c["resources"]["limits"]["cpu"]) * 1000)

        regular = [cpu(c) for c in spec["containers"]]
        init = [(cpu(c), c.get("restartPolicy") == "Always") for c in spec.get("initContainers", [])]
        predicted = v2.pod_quota(regular, init, 100000, 1000, 1000)
        measured = F(str(row["quota"])) * 1000 / 100000
        if F(predicted["parentQuotaMilli"]) != measured:
            raise ValueError(f"observed Pod parent contradicts stage model: {row['pod']}")
        # Exact workload identity comes from the retained component label, never a guessed suffix.
        workload = pod["metadata"]["labels"]["hypershift.openshift.io/control-plane-component"]
        candidates = {}
        for name, model in result["models"].items():
            allocated = {(r["identity"]["workload"], r["identity"]["container"]): r["requests"]["cpuMilli"] for r in model["rows"]}
            regular_new = [allocated[(workload, c["name"])] for c in spec["containers"]]
            init_new = [(allocated[(workload, c["name"])], c.get("restartPolicy") == "Always") for c in spec.get("initContainers", [])]
            candidates[name] = v2.pod_quota(regular_new, init_new, args.cpu_period_us, args.min_quota_us, args.quota_quantum_us)
        parent_rows.append({"pod": row["pod"], "uid": uid, "workload": workload, "observedQuotaUS": row["quota"],
                            "admittedStageDeclaredMilli": predicted["declaredParentMilli"], "observedModel": predicted,
                            "candidateExtrapolations": candidates})
    if len(parent_rows) != 17:
        raise ValueError("expected 17 observed Pod parents")
    result["assumptions"]["quotaQuantumUS"] = args.quota_quantum_us
    result["assumptions"]["basis"] = "Observed startup cAdvisor sample: 100000us period, 1000us minimum and ceiling quantum. Extrapolation outside captured sample, not universal Kubernetes semantics; origin unverified."
    result["observedQuotaValidation"] = {"capturedAt": analysis["at"], "rawSamples": analysis["rawSamples"],
                                        "pairedContainerRows": len(container_rows), "podParents": len(parent_rows),
                                        "sandboxRows": sum(r["level"] == "sandbox" for r in analysis["rows"]),
                                        "containerRows": container_rows, "parentRows": parent_rows,
                                        "candidateParentSumMilli": {n: str(sum(F(r["candidateExtrapolations"][n]["effectiveQuotaMilli"]) for r in parent_rows)) for n in profiles},
                                        "runtimeNodeInfo": [n["status"]["nodeInfo"] for n in nodes],
                                        "scope": "17 observed startup parents, not 52 benchmark groups; missing samples not unlimited; candidate predictions not measured quotas"}
    for path in (Path(__file__), Path(__file__).with_name("test_cost_allocate_v3.py")):
        result["sourceHashes"][str(path.relative_to(root))] = v2.v1.digest(path.read_bytes())


if __name__ == "__main__":
    v2.main(version="v3", quantum=1000, augment=augment)
