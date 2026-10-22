#!/usr/bin/env python3
"""Offline quota-aware cost allocation. V1 files are read-only historical inputs."""

import argparse
from fractions import Fraction as F
import json
import os
from pathlib import Path
import subprocess

import cost_allocate as v1


PRECISION = 10**9


def quota_milli(cpu, period=100000, minimum=1000, quantum=1):
    if min(period, minimum, cpu, quantum) <= 0:
        raise ValueError("positive limited CPU, cpuPeriodUS, minQuotaUS and quotaQuantumUS required")
    # Keep the historical v2 integer conversion when quantum=1. Larger quanta
    # model observed runtime rounding, not a universal Kubernetes guarantee.
    quota = cpu * period // 1000 if quantum == 1 else ((cpu * period + 1000 * quantum - 1) // (1000 * quantum)) * quantum
    return F(max(minimum, quota) * 1000, period)


def pod_quota(regular, init, period=100000, minimum=1000, quantum=1):
    """Init entries are (declared millicores, restartable). Parent covers all stages.

    All supplied containers must be limited. Unbounded downstream Pods are priced
    separately at fixed requests, not assigned fictional finite quota ceilings.
    """
    stages, sidecars = [], []
    for cpu, restartable in init:
        stages.append(sidecars + [cpu])
        if restartable:
            sidecars = sidecars + [cpu]
    stages.append(regular + sidecars)
    declared = max(map(sum, stages))
    parent = quota_milli(declared, period, minimum, quantum)
    sums = [sum((quota_milli(c, period, minimum, quantum) for c in stage), F(0)) for stage in stages]
    return {"declaredParentMilli": declared, "parentQuotaMilli": str(parent),
            "uncappedStageMaximumMilli": str(max(sums)),
            "effectiveQuotaMilli": str(min(parent, max(sums))),
            "steadyQuotaMilli": str(min(parent, sums[-1]))}


def search_factor(upper, fits):
    """Largest feasible rational on a 1e-9 grid; never round across a quota step."""
    low, high = 0, int(upper * PRECISION)
    while low < high:
        mid = (low + high + 1) // 2
        if fits(F(mid, PRECISION)):
            low = mid
        else:
            high = mid - 1
    return F(low, PRECISION)


def derive(root, period, minimum, quantum=1, version="v2"):
    history = root / "_artifacts/small-cost-v1"
    hashes = {}

    def read(path, expected=None):
        raw = path.read_bytes()
        sha = v1.digest(raw)
        if expected and sha != expected:
            raise ValueError(f"historical hash mismatch: {path}")
        hashes[str(path.relative_to(root))] = sha
        return json.loads(raw)

    old = read(history / "allocation.json", "97b50a5065313a1d18de90310b8a31ff5b0df1733ae1d63c16d47ab4bd02eb68")
    manifest = read(history / "output-hashes.json")
    for name, item in manifest.items():
        read(history / name, item["sha256"])
    for name, item in old["profiles"].items():
        read(root / item["path"], item["sha256"])
    prices = read(history / "prices.json", "5eb154b47fa2b62fcc58f1ef0b466de11a2190a1ca29f20a34dafe83c54b5614")
    for page in prices["pages"]:
        read(history / page["path"], page["sha256"])
    # Pin stable first-failure evidence, not another owner's active cleanup logs.
    for relative in ("lifecycle/startup-failure.json", "lifecycle/failure-resource-audit.json",
                     "lifecycle/startup-r2/000/pods.json", "lifecycle/startup-r2/000/admitted-resources.json"):
        read(history / relative)
    frozen_path = root / "_artifacts/small-constrained-v1/analysis"
    frozen = read(frozen_path / "candidate-budgets.json", "c0617bddb79fa78d2c652895dc28ffa75dc041237523f3990670dfad13b375a8")
    observations = read(frozen_path / "observations.json", "abea0bf189f9b941e68601b2d16d2f2726e26d19aed03b754ba6c6b119d64558")
    inputs = read(frozen_path / "input-hashes.json", "a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e")
    pod_path = frozen["snapshots"][-1]["path"] + "/full-pods.json"
    pods = read(root / pod_path, inputs["files"][pod_path]["sha256"])["items"]
    source = old["selected"]["small-cost-v1"]["rows"]
    by_selector = {(c["selector"]["kind"], c["selector"]["workload"], c["selector"]["containerGroup"], c["selector"]["container"]): i
                   for i, c in enumerate(source)}
    owners = {}
    for item in observations["instances"]:
        key = item["selector"]["kind"], item["selector"]["workload"]
        if item["podUID"] in owners and owners[item["podUID"]] != key:
            raise ValueError("ambiguous saved Pod owner")
        owners[item["podUID"]] = key
    layouts, seen, instance_count = [], set(), 0
    controlled_workloads = {(c["selector"]["kind"], c["selector"]["workload"]) for c in source}
    for pod in pods:
        uid = pod["metadata"]["uid"]
        if uid not in owners:
            continue
        seen.add(uid)
        owner = owners[uid]
        if owner not in controlled_workloads:
            continue
        spec = pod["spec"]
        if spec.get("resources") or spec.get("overhead"):
            raise ValueError("Pod-level resources/overhead require separate accounting")
        regular = [by_selector[(*owner, "containers", c["name"])] for c in spec["containers"]]
        init = [(by_selector[(*owner, "initContainers", c["name"])], c.get("restartPolicy") == "Always")
                for c in spec.get("initContainers", [])]
        instance_count += len(regular) + sum(restart for _, restart in init)
        layouts.append({"pod": pod["metadata"]["name"], "uid": uid, "workload": owner[1], "regular": regular, "init": init})
    if seen != set(owners) or instance_count != 97:
        raise ValueError("saved layout coverage differs from 97 controlled instances")

    def charge(cpu_values, memory_values, detail=False):
        declared = sum(c * s["replicas"] for c, s in zip(cpu_values, source))
        requests = sum(m * s["replicas"] for m, s in zip(memory_values, source))
        effective, memory, pod_rows = F(0), 0, []
        for layout in layouts:
            q = pod_quota([max(1, cpu_values[i]) for i in layout["regular"]],
                          [(max(1, cpu_values[i]), restart) for i, restart in layout["init"]], period, minimum, quantum)
            effective += F(q["effectiveQuotaMilli"])
            # Memory parent limits use Kubernetes regular/init maxima, including
            # rounding each container's 110% limit before computing stage sums.
            spec = {"containers": [{"name": i} for i in layout["regular"]],
                    "initContainers": [{"name": i, "restartPolicy": "Always" if restart else ""} for i, restart in layout["init"]]}
            mem = v1.scheduling_charge(spec, lambda c, group: [0, 0, (memory_values[c["name"]] * 110 + 99) // 100])[2]
            memory += mem
            if detail:
                pod_rows.append({"pod": layout["pod"], "uid": layout["uid"], "workload": layout["workload"], **q, "chargedMemoryMiB": mem})
        return {"declaredControlledCPUMilli": declared, "effectiveControlledCPUMilli": str(effective),
                "declaredOfferingCPUMilli": declared + 470, "effectiveOfferingCPUMilli": str(effective + 470),
                "controlledRequestMemoryMiB": requests, "offeringRequestMemoryMiB": requests + 2090,
                "controlledChargedMemoryMiB": memory, "offeringChargedMemoryMiB": memory + 2090,
                "podGroups": pod_rows}

    regression = {}
    for name, chosen in old["selected"].items():
        q = charge([c["allocatedCPUMilli"] for c in chosen["rows"]], [c["allocatedMemoryMiB"] for c in chosen["rows"]], True)
        q["quotaAwareUSDPerHour"] = str(F(".806") * max(F(q["effectiveOfferingCPUMilli"]) / 16000, F(q["offeringChargedMemoryMiB"], 65536)))
        regression[name] = q
    models, profiles = {}, {}
    for budget, prefix in ((F(1, 8), "small-cost-" + version), (F(1, 4), "double-small-cost-" + version)):
        for uniform in (True, False):
            name = (prefix if uniform else prefix + "-independent") if version == "v2" else (prefix + "-uniform" if uniform else prefix)
            cpu_cap, mem_cap = 16000 * budget / F(".806"), 65536 * budget / F(".806")
            upper_cpu = (cpu_cap - 470) / 6020
            upper_mem = (mem_cap - 2090) / (F(11, 10) * 14016)
            # Reject even the representable minimum if quotas already exhaust CPU.
            floor_cpu = [1] * len(source)
            if F(charge(floor_cpu, [1] * len(source))["effectiveOfferingCPUMilli"]) > cpu_cap:
                raise ValueError("quota minima exhaust cost envelope")

            def values(factor, field):
                return [int(c[field] * factor) for c in source]

            if uniform:
                def fits(factor):
                    q = charge(values(factor, "cpuMilli"), values(factor, "memoryMiB"))
                    return F(q["effectiveOfferingCPUMilli"]) <= cpu_cap and q["offeringChargedMemoryMiB"] <= mem_cap
                cpu_scale = mem_scale = search_factor(min(upper_cpu, upper_mem), fits)
            else:
                cpu_scale = search_factor(upper_cpu, lambda f: F(charge(values(f, "cpuMilli"), [1] * len(source))["effectiveOfferingCPUMilli"]) <= cpu_cap)
                mem_scale = search_factor(upper_mem, lambda f: charge(floor_cpu, values(f, "memoryMiB"))["offeringChargedMemoryMiB"] <= mem_cap)
            cpus, memories = values(cpu_scale, "cpuMilli"), values(mem_scale, "memoryMiB")
            if min(cpus + memories) < 1:
                raise ValueError("zero/subminimum allocation; no original floors restored")
            q = charge(cpus, memories, True)
            cost = F(".806") * max(F(q["effectiveOfferingCPUMilli"]) / 16000, F(q["offeringChargedMemoryMiB"], 65536))
            if cost > budget:
                raise ValueError("quota-aware envelope exceeded")
            fallback = {"cpuMilli": int(10 * cpu_scale), "memoryMiB": int(64 * mem_scale)}
            entries, rows = [], []
            for c, cpu, mem in zip(source, cpus, memories):
                floor = {"cpuMilli": cpu, "memoryMiB": mem}
                identity = c["identity"]
                kas = identity == {"kind": "Deployment", "workload": "kube-apiserver", "container": "kube-apiserver"}
                rows.append({"identity": identity, "replicas": c["replicas"], "requests": floor,
                             "memoryLimitMiB": (mem * 110 + 99) // 100, "nonGoException": c["nonGoException"]})
                if floor != fallback or c["nonGoException"] or kas:
                    entry = {"identity": identity, "floor": floor, "cpuRatio": {"numerator": int(kas), "denominator": 1}, "memoryRatio": {"numerator": int(kas), "denominator": 1}}
                    if c["nonGoException"]:
                        entry["goMemoryLimitPercent"] = 0
                    entries.append(entry)
            anchor = next(c["floor"] for c in entries if c["identity"]["workload"] == "kube-apiserver" and c["identity"]["container"] == "kube-apiserver")
            models[name] = {**q, "budgetUSDPerHour": str(budget), "quotaAwareUSDPerHour": str(cost),
                            "cpuScale": str(cpu_scale), "memoryScale": str(mem_scale), "fallback": fallback,
                            "unusedEffectiveCPUMilli": str(cpu_cap - F(q["effectiveOfferingCPUMilli"])),
                            "unusedChargedMemoryMiB": str(mem_cap - q["offeringChargedMemoryMiB"]), "rows": rows}
            description = f"OFFLINE UNTESTED quota-aware cost-v2 {('uniform comparison' if uniform else 'independent CPU/RAM diagnostic')}. Budget ${float(budget):.3f}/HCP-hour raw D16s_v6 equivalent. ASSUMED cpuPeriodUS={period}, minQuotaUS={minimum}, finite Pod parent quotas; verify live before qualification. No original floors. Required render flags: --default-cpu-milli {fallback['cpuMilli']} --default-memory-mib {fallback['memoryMiB']}. Not approved for application."
            if version != "v2":
                description = f"OFFLINE UNTESTED cost-{version} {('uniform comparison' if uniform else 'recommended independent CPU/RAM candidate')}. Budget ${float(budget):.3f}/HCP-hour raw D16s_v6 equivalent. cpuPeriodUS={period}, minQuotaUS={minimum}, quotaQuantumUS={quantum}: based on saved startup samples, origin unknown, not universal Kubernetes behavior. Parent-capped stage model extrapolated to source benchmark layout. No original floors. Required render flags: --default-cpu-milli {fallback['cpuMilli']} --default-memory-mib {fallback['memoryMiB']}. No live application authorized."
            profiles[name] = {"version": "v1", "name": name, "description": description,
                              "anchor": anchor, "rounding": {"cpuMilli": 1, "memoryMiB": 1}, "safetyLimit": {"numerator": 11, "denominator": 10}, "cpuLimitEqualsRequest": True, "goMemLimitPercent": 80, "containers": entries}
    for path in (Path(__file__), Path(__file__).with_name("test_cost_allocate_v2.py"), Path(v1.__file__)):
        hashes[str(path.relative_to(root))] = v1.digest(path.read_bytes())
    for relative in ("tooling/hcp-perf/internal/sizing/sizing.go", "tooling/hcp-perf/internal/sizing/hypershift.go",
                     "tooling/hcp-perf/main.go", "tooling/hcp-perf/json.go", "tooling/hcp-perf/go.mod", "tooling/hcp-perf/go.sum"):
        hashes[relative] = v1.digest((root / relative).read_bytes())
    return {"version": version, "assumptions": {"cpuPeriodUS": period, "minQuotaUS": minimum, "verifiedLive": False,
            "parentQuota": "quota(Kubernetes Pod regular/restartable-init/sequential-init declared limit maximum)", "factorGridDenominator": PRECISION},
            "coverage": {"controlledRunningInstances": 97, "controlledPodGroups": len(layouts), "controlledRows": len(source),
                         "downstreamFixedInstances": 47, "downstreamCPUMilli": 470, "downstreamMemoryMiB": 2090,
                         "instrumentationExcluded": {"cpuMilli": 10, "memoryMiB": 64}, "bootstrapTested": False,
                         "scope": "saved source benchmark Pod layout, not failed startup's incomplete inventory; completed Jobs/disabled workloads excluded"},
            "v1Regression": regression, "models": models, "sourceHashes": hashes}, profiles


def main(version="v2", quantum=1, augment=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[3])
    parser.add_argument("--cpu-period-us", type=int, default=100000)
    parser.add_argument("--min-quota-us", type=int, default=1000)
    parser.add_argument("--quota-quantum-us", type=int, default=quantum)
    parser.add_argument("--write", action="store_true")
    parser.add_argument("--hypershift-root", type=Path, help="local CRD OpenAPI validation; no admission or network")
    args = parser.parse_args()
    quota_milli(1, args.cpu_period_us, args.min_quota_us, args.quota_quantum_us)
    root = args.root.resolve()
    output = root / ("_artifacts/small-cost-" + version)
    if not output.is_dir() or output.is_symlink() or output.stat().st_mode & 0o077:
        raise ValueError("create private versioned cost directory first")
    result, profiles = derive(root, args.cpu_period_us, args.min_quota_us, args.quota_quantum_us, version)
    if augment:
        augment(root, result, profiles, args)
    outputs = {}
    env = dict(os.environ, GOWORK="off", GOPROXY="off", GOSUMDB="off")
    for name, profile in profiles.items():
        path = (output if name.endswith(("independent", "uniform")) else root / "tooling/hcp-perf/examples") / (name + ".json")
        raw = v1.encode(profile)
        if args.write:
            v1.save_new(path, raw)
        elif path.read_bytes() != raw:
            raise ValueError(f"profile differs: {name}")
        result.setdefault("profiles", {})[name] = {"path": str(path.relative_to(root)), "bytes": len(raw), "sha256": v1.digest(raw)}
        model = result["models"][name]
        for command in ("resolve", "hypershift"):
            cmd = ["go", "run", ".", "sizing", command, "--profile", str(path)]
            if command == "hypershift":
                cmd += ["--default-cpu-milli", str(model["fallback"]["cpuMilli"]), "--default-memory-mib", str(model["fallback"]["memoryMiB"])]
            outputs[f"{name}-{command}.json"] = subprocess.run(cmd, cwd=root / "tooling/hcp-perf", env=env, check=True, capture_output=True).stdout
        resolved = json.loads(outputs[f"{name}-resolve.json"])
        policy = json.loads(outputs[f"{name}-hypershift.json"])["spec"]["sizes"][0]["effects"]["containerResourcePolicy"]
        if policy["memoryLimitPercent"] != 110 or policy["cpuLimitEqualsRequest"] is not True or policy["goMemoryLimitPercent"] != 80:
            raise ValueError("rendered limit policy mismatch")
        if args.hypershift_root:
            import jsonschema
            import yaml
            schema_path = args.hypershift_root.resolve() / "cmd/install/assets/crds/hypershift-operator/scheduling.hypershift.openshift.io_clustersizingconfigurations.yaml"
            schema_raw = schema_path.read_bytes()
            crd = yaml.safe_load(schema_raw)
            schema = next(v["schema"]["openAPIV3Schema"] for v in crd["spec"]["versions"] if v["name"] == "v1alpha1")
            jsonschema.Draft4Validator(schema).validate(json.loads(outputs[f"{name}-hypershift.json"]))
            result["schemaValidation"] = {"path": str(schema_path), "sha256": v1.digest(schema_raw),
                                          "OpenAPIPassed": True, "CELAdmissionTested": False, "liveAdmissionTested": False}
        explicit = {(c["identity"]["workload"], c["identity"]["container"]): c for c in resolved["containers"]}
        wire = {(c["workload"], c["container"]): c for c in policy["containers"]}
        if len(wire) != len(policy["containers"]) or wire.keys() != explicit.keys():
            raise ValueError("ambiguous rendered keys")
        for row in model["rows"]:
            key = row["identity"]["workload"], row["identity"]["container"]
            req = {"cpu": str(row["requests"]["cpuMilli"]) + "m", "memory": str(row["requests"]["memoryMiB"]) + "Mi"}
            if wire.get(key, {"requests": policy["defaultRequests"]})["requests"] != req:
                raise ValueError("effective fallback/override mismatch")
            if key in explicit:
                want_env = {} if row["nonGoException"] else {"GOMEMLIMIT": str(row["requests"]["memoryMiB"] * 1048576 * 80 // 100) + "B"}
                if explicit[key]["requests"] != req or explicit[key]["limits"] != {"cpu": req["cpu"], "memory": str(row["memoryLimitMiB"]) + "Mi"} or explicit[key].get("env", {}) != want_env:
                    raise ValueError("resolved request/limit/env mismatch")
        result["profiles"][name]["everyControlledRowMatchesCLI"] = True
    # Recheck every input, including historical lifecycle and deployed wire bytes.
    for relative, sha in result["sourceHashes"].items():
        if v1.digest((root / relative).read_bytes()) != sha:
            raise ValueError(f"source changed during derivation: {relative}")
    outputs["allocation.json"] = v1.encode(result)
    outputs["output-hashes.json"] = v1.encode({name: {"sha256": v1.digest(raw), "bytes": len(raw)} for name, raw in outputs.items()})
    for name, raw in outputs.items():
        if args.write:
            v1.save_new(output / name, raw)
        elif (output / name).read_bytes() != raw:
            raise ValueError(f"replay differs: {name}")
    print(json.dumps({"profiles": result["profiles"], "coverage": result["coverage"],
                      "v1Regression": {n: {k: v for k, v in m.items() if k != "podGroups"} for n, m in result["v1Regression"].items()},
                      "models": {n: {k: v for k, v in m.items() if k not in ("rows", "podGroups")} for n, m in result["models"].items()}}, indent=2))


if __name__ == "__main__":
    main()
