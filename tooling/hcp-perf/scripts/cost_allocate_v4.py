#!/usr/bin/env python3
"""Offline fixed-memory exceptions using preserved v3 CPU and original RAM weights."""

import argparse
from copy import deepcopy
from fractions import Fraction as F
import json
import os
from pathlib import Path
import subprocess

import cost_allocate_v2 as q


def fixed_memory(rows, overrides, capacity, charge, field="memoryMiB"):
    """Fixed per-container MiB, with one proportional factor for all other weights.

    charge(values) accounts for actual replicas/Pod init stages and fixed downstream.
    Overrides use exact (workload, container) keys; absent/ambiguous keys fail closed.
    """
    keys = [(r["identity"]["workload"], r["identity"]["container"]) for r in rows]
    if len(set(keys)) != len(keys) or not set(overrides) <= set(keys):
        raise ValueError("ambiguous or nonlisted fixed-memory override")
    if capacity <= 0 or any(m <= 0 or not isinstance(m, int) for m in overrides.values()):
        raise ValueError("positive capacity and integer fixed memory required")
    if field not in ("memoryMiB", "cpuMilli"):
        raise ValueError("unsupported fixed allocation resource")
    if any(r[field] <= 0 or r["replicas"] < 0 for r in rows):
        raise ValueError("invalid source weights/replicas")

    def values(scale):
        return [overrides[k] if k in overrides else int(r[field] * scale) for k, r in zip(keys, rows)]

    minimum = [overrides.get(k, 1) for k in keys]
    if charge(minimum) > capacity:
        raise ValueError("fixed memory and minimum allocations exceed budget")
    weight = sum(r[field] * r["replicas"] for k, r in zip(keys, rows) if k not in overrides)
    if weight <= 0:
        raise ValueError("no remaining running memory weight")
    scale = q.search_factor(capacity / weight, lambda f: charge(values(f)) <= capacity)
    result = values(scale)
    if min(result) < 1 or charge(result) > capacity:
        raise ValueError("unsatisfiable positive allocation")
    return scale, result


def derive(root):
    hashes = {}

    def read(relative, expected=None):
        raw = (root / relative).read_bytes()
        sha = q.v1.digest(raw)
        if expected and sha != expected:
            raise ValueError(f"historical hash mismatch: {relative}")
        hashes[relative] = sha
        return json.loads(raw)

    history = "_artifacts/small-cost-v3/"
    old = read(history + "allocation.json", "beb18ed9fe004491d45710d8d33986f5e44bc16d587b227056aeeab12c820e93")
    for filename, item in read(history + "output-hashes.json").items():
        read(history + filename, item["sha256"])
    for item in old["profiles"].values():
        read(item["path"], item["sha256"])
    parent = read("tooling/hcp-perf/examples/small-cost-v3.json", "2cac7b641f4322760f8b319b23267ba9b890aef9bb80950fa212cde61a811e0e")
    source = read("_artifacts/small-cost-v1/allocation.json", "97b50a5065313a1d18de90310b8a31ff5b0df1733ae1d63c16d47ab4bd02eb68")["selected"]["small-cost-v1"]["rows"]
    frozen = read("_artifacts/small-constrained-v1/analysis/candidate-budgets.json", "c0617bddb79fa78d2c652895dc28ffa75dc041237523f3990670dfad13b375a8")
    evidence = next(r for r in frozen["budgets"] if r["selector"]["workload"] == "ignition-server-proxy" and r["selector"]["container"] == "haproxy")
    medium_path = "_artifacts/medium-v1-20260910/verification/005006/pods.json"
    medium = read(medium_path)["items"]
    medium = [p for p in medium if p["metadata"]["name"].startswith("ignition-server-proxy-")]
    if len(medium) != 2 or any(p["spec"]["containers"][0]["resources"]["requests"]["memory"] != "128Mi" or
                              p["spec"]["containers"][0]["resources"]["limits"]["memory"] != "384Mi" or
                              not p["status"]["containerStatuses"][0]["ready"] for p in medium):
        raise ValueError("Medium HAProxy baseline differs")
    read(history + "lifecycle/failure.json", "665e2fefce31383f14119eecc4a26f9c8962e1b74b311d1a3a0b68c27729a6bb")
    pod_path = frozen["snapshots"][-1]["path"] + "/full-pods.json"
    inputs = read("_artifacts/small-constrained-v1/analysis/input-hashes.json", "a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e")
    pods = {p["metadata"]["uid"]: p for p in read(pod_path, inputs["files"][pod_path]["sha256"])["items"]}
    model = deepcopy(old["models"]["small-cost-v3"])
    rows = model["rows"]
    if [r["identity"] for r in rows] != [r["identity"] for r in source]:
        raise ValueError("source inventory order differs")
    index = {(r["identity"]["workload"], r["identity"]["container"]): i for i, r in enumerate(rows)}
    layouts = []
    for group in model["podGroups"]:
        pod = pods[group["uid"]]
        spec = pod["spec"]
        if spec.get("overhead") or spec.get("resources"):
            raise ValueError("unaccounted Pod resources")
        layouts.append((group, spec))

    def memory_groups(values):
        return [q.v1.scheduling_charge(spec, lambda c, kind: [0, 0, (110 * values[index[(group["workload"], c["name"])]] + 99) // 100])[2]
                for group, spec in layouts]

    def charge(values):
        return 2090 + sum(memory_groups(values))

    cap = F(65536) * F(".125") / F(".806")
    key = ("ignition-server-proxy", "haproxy")
    if source[index[key]]["replicas"] != 2:
        raise ValueError("expected both HAProxy replicas")
    scale, values = fixed_memory(source, {key: 96}, cap, charge)
    deltas = []
    for row, memory in zip(rows, values):
        previous = row["requests"]["memoryMiB"]
        row["requests"]["memoryMiB"] = memory
        row["memoryLimitMiB"] = (memory * 110 + 99) // 100
        deltas.append({"identity": row["identity"], "replicas": row["replicas"], "oldRequestMiB": previous,
                       "newRequestMiB": memory, "requestDeltaMiB": memory - previous,
                       "oldLimitMiB": (previous * 110 + 99) // 100, "newLimitMiB": row["memoryLimitMiB"]})
    for group, memory in zip(model["podGroups"], memory_groups(values)):
        group["chargedMemoryMiB"] = memory
    model["controlledRequestMemoryMiB"] = sum(m * r["replicas"] for m, r in zip(values, rows))
    model["offeringRequestMemoryMiB"] = model["controlledRequestMemoryMiB"] + 2090
    model["offeringChargedMemoryMiB"] = charge(values)
    model["controlledChargedMemoryMiB"] = charge(values) - 2090
    model["memoryScale"] = str(scale)
    model["unusedChargedMemoryMiB"] = str(cap - charge(values))
    model["quotaAwareUSDPerHour"] = str(F(".806") * max(F(model["effectiveOfferingCPUMilli"]) / 16000, F(charge(values), 65536)))
    if F(model["quotaAwareUSDPerHour"]) > F(".125"):
        raise ValueError("cost envelope exceeded")
    model["fallback"] = {"cpuMilli": old["models"]["small-cost-v3"]["fallback"]["cpuMilli"], "memoryMiB": int(64 * scale)}
    profile = deepcopy(parent)
    profile["name"] = "small-cost-v4"
    profile["description"] = f"OFFLINE STARTUP-UNTESTED fixed $0.125/HCP-hour D16s_v6 candidate. Both ignition-server-proxy/haproxy replicas fixed at 96Mi request/106Mi limit; remaining RAM proportional to original source weights. V3 CPU and quota model unchanged. Hypothesis, not proven startup allowance. Required render flags: --default-cpu-milli {model['fallback']['cpuMilli']} --default-memory-mib {model['fallback']['memoryMiB']}. No other policy/image/APF changes."
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
    for path in (Path(__file__), Path(__file__).with_name("test_cost_allocate_v4.py"), Path(q.__file__), Path(q.v1.__file__)):
        hashes[str(path.relative_to(root))] = q.v1.digest(path.read_bytes())
    for relative in ("tooling/hcp-perf/internal/sizing/sizing.go", "tooling/hcp-perf/internal/sizing/hypershift.go"):
        hashes[relative] = q.v1.digest((root / relative).read_bytes())
    result = {"version": "v4", "qualified": False, "startupTested": False, "model": model, "memoryDeltas": deltas,
              "sourceHashes": hashes, "quotaAssumptions": old["assumptions"], "coverage": old["coverage"],
              "fixedMemoryOverrides": [{"workload": key[0], "container": key[1], "replicas": 2, "requestMiB": 96, "limitMiB": 106}],
              "evidence": {"small": {"resources": evidence["baselineResourceQuantities"], "statistics": evidence["coveredQuietEvidence"]},
                           "medium": [{"pod": p["metadata"]["name"], "resources": p["spec"]["containers"][0]["resources"],
                                       "ready": p["status"]["containerStatuses"][0]["ready"], "restarts": p["status"]["containerStatuses"][0]["restartCount"]} for p in medium]},
              "hypothesis": "96Mi request/106Mi limit exceeds recorded Small charge and failed 55Mi v3 limit, but is not a proven fresh-start minimum. Remaining containers may become the next failure."}
    return result, profile


def main(version="v4", derive_fn=derive, parent_version="v3", resource="memory", allowed_changes=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[3])
    parser.add_argument("--write", action="store_true")
    args = parser.parse_args()
    root = args.root.resolve()
    name = "small-cost-" + version
    output = root / ("_artifacts/" + name)
    if not output.is_dir() or output.is_symlink() or output.stat().st_mode & 0o077:
        raise ValueError("existing private v4 output directory required")
    result, profile = derive_fn(root)
    path = root / ("tooling/hcp-perf/examples/" + name + ".json")
    raw = q.v1.encode(profile)
    if args.write:
        q.v1.save_new(path, raw)
    elif path.read_bytes() != raw:
        raise ValueError("profile differs")
    result["profile"] = {"path": str(path.relative_to(root)), "bytes": len(raw), "sha256": q.v1.digest(raw)}
    outputs = {}
    for command in ("resolve", "hypershift"):
        cmd = ["go", "run", ".", "sizing", command, "--profile", str(path)]
        if command == "hypershift":
            cmd += ["--default-cpu-milli", str(result["model"]["fallback"]["cpuMilli"]), "--default-memory-mib", str(result["model"]["fallback"]["memoryMiB"])]
        outputs[name + "-" + command + ".json"] = subprocess.run(cmd, cwd=root / "tooling/hcp-perf", env=dict(os.environ, GOWORK="off", GOPROXY="off", GOSUMDB="off"), check=True, capture_output=True).stdout
    wire = json.loads(outputs[name + "-hypershift.json"])
    parent_wire = json.loads((root / f"_artifacts/small-cost-{parent_version}/small-cost-{parent_version}-hypershift.json").read_bytes())
    policy = wire["spec"]["sizes"][0]["effects"]["containerResourcePolicy"]
    old_policy = parent_wire["spec"]["sizes"][0]["effects"]["containerResourcePolicy"]
    by_key = {(r["identity"]["workload"], r["identity"]["container"]): r for r in result["model"]["rows"]}
    explicit = {(r["workload"], r["container"]): r for r in policy["containers"]}
    resolved = {(r["identity"]["workload"], r["identity"]["container"]): r for r in json.loads(outputs[name + "-resolve.json"])["containers"]}
    for key, row in by_key.items():
        req = {"cpu": str(row["requests"]["cpuMilli"]) + "m", "memory": str(row["requests"]["memoryMiB"]) + "Mi"}
        if explicit.get(key, {"requests": policy["defaultRequests"]})["requests"] != req:
            raise ValueError("effective allocation mismatch")
        if key in resolved:
            env = {} if row["nonGoException"] else {"GOMEMLIMIT": str(row["requests"]["memoryMiB"] * 1048576 * 80 // 100) + "B"}
            if resolved[key]["limits"] != {"cpu": req["cpu"], "memory": str(row["memoryLimitMiB"]) + "Mi"} or resolved[key].get("env", {}) != env:
                raise ValueError("resolved limits/runtime mismatch")
    # Verify the complete rendered CSC changes only the selected resource requests.
    expected = deepcopy(parent_wire)
    expected_policy = expected["spec"]["sizes"][0]["effects"]["containerResourcePolicy"]
    if resource == "both":
        expected_policy["defaultRequests"] = {"cpu": str(result["model"]["fallback"]["cpuMilli"]) + "m", "memory": str(result["model"]["fallback"]["memoryMiB"]) + "Mi"}
        expected_policy["containers"] = []
        for c in sorted(profile["containers"], key=lambda c: (c["identity"]["workload"], c["identity"]["container"])):
            entry = {"workload": c["identity"]["workload"], "container": c["identity"]["container"],
                     "requests": {"cpu": str(c["floor"]["cpuMilli"]) + "m", "memory": str(c["floor"]["memoryMiB"]) + "Mi"}}
            if c.get("goMemoryLimitPercent") == 0:
                entry["goMemoryLimitPercent"] = 0
            expected_policy["containers"].append(entry)
        allowed_changes = {}
    if allowed_changes is None:
        expected_policy["defaultRequests"][resource] = policy["defaultRequests"][resource]
    for entry in expected_policy["containers"]:
        key = entry["workload"], entry["container"]
        resources = (resource,) if allowed_changes is None else allowed_changes.get(key, ())
        for selected in resources:
            field, suffix = ("memoryMiB", "Mi") if selected == "memory" else ("cpuMilli", "m")
            entry["requests"][selected] = str(by_key[key]["requests"][field]) + suffix
    if wire != expected:
        raise ValueError("wire changed outside intended resource requests")
    result["validation"] = {"everyControlledRowMatchesCLI": True, "wireChangesOnlyMemoryRequests": True, "CPUQuotaModelUnchanged": True} if resource == "memory" else {"everyControlledRowMatchesCLI": True, "wireChangesOnlyCPURequests": True, "allMemoryAndRuntimeSettingsUnchanged": True}
    if allowed_changes is not None:
        result["validation"] = {"everyControlledRowMatchesCLI": True, "wireMatchesExactAllowedChanges": True,
                                "fallbackAndOtherPoliciesUnchanged": True}
    if resource == "both":
        result["validation"] = {"everyControlledRowMatchesCLI": True, "onlyResourceEntriesAndFallbackChanged": True,
                                "nonResourceCSCSettingsUnchanged": True}
    outputs["allocation.json"] = q.v1.encode(result)
    outputs["output-hashes.json"] = q.v1.encode({name: {"bytes": len(data), "sha256": q.v1.digest(data)} for name, data in outputs.items()})
    for relative, sha in result["sourceHashes"].items():
        if q.v1.digest((root / relative).read_bytes()) != sha:
            raise ValueError("source changed during replay")
    for name, data in outputs.items():
        if args.write:
            q.v1.save_new(output / name, data)
        elif (output / name).read_bytes() != data:
            raise ValueError(f"output differs: {name}")
    print(json.dumps({"profile": result["profile"], "model": {k: v for k, v in result["model"].items() if k not in ("rows", "podGroups")}, "deltas": [d for d in result.get("memoryDeltas", result.get("cpuDeltas", [])) if d["identity"]["container"] in ("haproxy", "kube-apiserver", "control-plane-operator", "kas-readiness-check")]}, indent=2))


if __name__ == "__main__":
    main()
