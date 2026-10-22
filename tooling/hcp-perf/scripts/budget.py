#!/usr/bin/env python3
"""Offline, exact-rational candidate allocation. See docs/BUDGET.md."""
import argparse
from collections import Counter
from datetime import datetime
from fractions import Fraction as F
import hashlib
import json
from math import ceil
from pathlib import Path
import re
import subprocess
import tempfile


RESOURCES = ("cpuMilli", "memoryMiB")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def encode(value):
    return (json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n").encode()


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def integer(value, minimum=1):
    require(type(value) is int and minimum <= value <= 2**63 - 1, "invalid integer resource/count")
    return value


def rational(value):
    require(type(value) in (str, int), "use exact strings or integers, not floats")
    result = F(value)
    require(result > 0, "positive finite rational required")
    return result


def provenance(value):
    require(isinstance(value, dict) and bool(value.get("basis")), "provenance basis required")
    require(re.fullmatch(r"[0-9a-f]{64}", value.get("sha256", "")), "source SHA256 required")


def stages(pod):
    """One scheduler-stage definition shared by CPU, memory, and old/new costing."""
    result, sidecars = [], []
    for entry in pod["init"]:
        result.append(sidecars + [entry["row"]])
        if entry["restartable"]:
            sidecars = sidecars + [entry["row"]]
    return result + [pod["regular"] + sidecars]


def quota(cpu, config):
    require(cpu > 0, "zero/unlimited CPU cannot be modeled as finite quota")
    period, minimum, quantum = (integer(config[k]) for k in ("periodUS", "minimumUS", "quantumUS"))
    return F(max(minimum, ceil(F(cpu * period, 1000 * quantum)) * quantum) * 1000, period)


def pod_charge(pod, values, resource, config):
    stage_values = [[values[i] for i in stage] for stage in stages(pod)]
    overhead = pod["overhead"][resource]
    requested = max(map(sum, stage_values)) + overhead
    if resource == "memoryMiB":
        charged = max(sum(ceil(F(v * 11, 10)) for v in stage) for stage in stage_values) + overhead
        return {"request": requested, "charge": charged}
    parent = quota(requested, config)
    children = [sum((quota(v, config) for v in stage), F(0)) + overhead for stage in stage_values]
    return {"request": requested, "charge": min(parent, max(children)),
            "parentQuotaMilli": parent, "uncappedStageMaximumMilli": max(children),
            "steadyQuotaMilli": min(parent, children[-1])}


def validate(spec):
    require(set(spec) == {"version", "name", "budgetUSDPerHour", "node", "quota", "inventory", "rows", "pods",
                         "downstream", "excludedInstrumentation", "fallback", "factorGridDenominator", "goMemLimitPercent"},
            "unknown or missing input field")
    require(spec["version"] == "hcp-perf.budget/v1", "unsupported input version")
    require(bool(re.fullmatch(r"[a-z0-9][a-z0-9-]*", spec["name"])), "invalid candidate name")
    rational(spec["budgetUSDPerHour"])
    node = spec["node"]
    require(node["currency"] == "USD" and node["capacityBasis"] == "raw", "USD/raw model required")
    require(all(node[k] for k in ("sku", "region", "sourceURL", "capacitySource")), "node provenance incomplete")
    require(datetime.fromisoformat(node["retrievedAt"].replace("Z", "+00:00")).tzinfo is not None,
            "retrieval timestamp needs timezone")
    provenance(node)
    rational(node["priceUSDPerHour"])
    for k in RESOURCES:
        integer(node[k])
        integer(spec["fallback"][k])
    integer(spec["factorGridDenominator"])
    integer(spec["goMemLimitPercent"])
    require(spec["goMemLimitPercent"] <= 100, "invalid Go memory percentage")
    provenance(spec["quota"])
    quota(1, spec["quota"])
    provenance(spec["inventory"])
    require(spec["inventory"]["scopeMethod"] == "source-verified-projector", "annotation-only scope rejected")
    require(spec["inventory"]["windows"] and spec["inventory"]["limitations"], "inventory scope/windows required")
    rows = spec["rows"]
    require(0 < len(rows) <= 256 and spec["pods"], "nonempty inventory/layout required")
    keys, counts, present = set(), Counter(), Counter()
    for row in rows:
        require(set(row) == {"identity", "replicas", "nonGo", "source", "startupNote", "floorBasis", "weightBasis",
                             "weights", "floors", "oldRequests", "quietObserved", "quietPeakMemoryMiB", "quietP95CPUMilli"},
                "unknown or missing row field")
        identity = row["identity"]
        require(set(identity) <= {"kind", "workload", "container", "initContainer"}, "unknown identity field")
        for k in ("kind", "workload", "container"):
            require(isinstance(identity[k], str) and identity[k].strip() == identity[k] and identity[k], "invalid identity")
        require(type(identity.get("initContainer", False)) is bool, "invalid init identity")
        key = identity["workload"], identity["container"]
        require(key not in keys, "resolver cannot distinguish duplicate workload/container")
        keys.add(key)
        integer(row["replicas"], 0)
        require(type(row["nonGo"]) is bool, "explicit nonGo classification required")
        provenance(row["source"])
        require(row["source"]["path"] and row["source"]["method"] == "source-verified-projector", "source verification receipt required")
        require(spec["inventory"]["sourceAuditHashes"].get(row["source"]["path"]) == row["source"]["sha256"],
                "row source differs from scoped audit receipt")
        require(row["startupNote"] and row["floorBasis"] and row["weightBasis"], "row basis/startup note required")
        require(type(row["quietObserved"]) is bool, "quiet coverage required")
        for field in ("weights", "floors", "oldRequests"):
            require(set(row[field]) == set(RESOURCES), "unknown/missing resource: " + field)
        for k in RESOURCES:
            rational(row["weights"][k])
            integer(row["floors"][k])
            integer(row["oldRequests"][k])
        for k in ("quietPeakMemoryMiB", "quietP95CPUMilli"):
            require(k in row, "missing quiet evidence field")
            if row[k] is not None:
                rational(row[k])
        require(not row["quietObserved"] or all(row[k] is not None for k in
                ("quietPeakMemoryMiB", "quietP95CPUMilli")), "missing observed quiet evidence")
    for pod in spec["pods"]:
        require(set(pod) == {"workload", "replicas", "regular", "init", "overhead"}, "unsupported Pod field/resources")
        integer(pod["replicas"])
        require(pod["regular"], "regular Pod containers required")
        for k in RESOURCES:
            integer(pod["overhead"][k], 0)
        bindings = [(i, False, True) for i in pod["regular"]]
        for entry in pod["init"]:
            require(set(entry) == {"row", "restartable"} and type(entry["restartable"]) is bool, "invalid init layout")
            bindings.append((entry["row"], True, entry["restartable"]))
        require(len({i for i, _, _ in bindings}) == len(bindings), "duplicate Pod binding")
        for i, init, running in bindings:
            integer(i, 0)
            require(i < len(rows), "unknown Pod row")
            identity = rows[i]["identity"]
            require(identity["workload"] == pod["workload"] and identity.get("initContainer", False) == init,
                    "Pod binding identity mismatch")
            present[i] += pod["replicas"]
            if running:
                counts[i] += pod["replicas"]
    require(all(counts[i] == r["replicas"] for i, r in enumerate(rows)), "replica/layout mismatch")
    require(sum(counts.values()) > 0, "no running controlled weight")
    downstream = spec["downstream"]
    provenance(downstream)
    require(downstream["mode"] == "fixed-requests" and downstream["limitations"], "explicit downstream accounting required")
    downstream_keys = [(r["workload"], r["container"]) for r in downstream["rows"]]
    require(len(set(downstream_keys)) == len(downstream_keys) and not keys.intersection(downstream_keys), "duplicate/overlapping downstream inventory")
    for k in RESOURCES:
        integer(downstream[k], 0)
        require(sum(integer(r["requests"][k]) * integer(r["replicas"]) for r in downstream["rows"]) == downstream[k],
                "fixed downstream inventory total differs")
        integer(downstream["actualLayoutRequests"][k], 0)
    require(spec["excludedInstrumentation"], "explicit instrumentation exclusion required")
    for row in spec["excludedInstrumentation"]:
        require(row["reason"] and row["workload"], "exclusion reason required")
        require(row["workload"] not in {k[0] for k in keys.union(downstream_keys)}, "instrumentation scope overlaps offering")
        for k in RESOURCES:
            integer(row["requests"][k])
    return present


def allocate(spec):
    present = validate(spec)
    rows, node, pods = spec["rows"], spec["node"], spec["pods"]
    price, budget = rational(node["priceUSDPerHour"]), rational(spec["budgetUSDPerHour"])
    allocations, resource_ledger = {}, {}
    for resource in RESOURCES:
        capacity = node[resource] * budget / price
        fixed = spec["downstream"][resource]
        weights = [rational(r["weights"][resource]) for r in rows]
        floors = [r["floors"][resource] for r in rows]

        def charge(values):
            return fixed + sum(p["replicas"] * pod_charge(p, values, resource, spec["quota"])["charge"] for p in pods)

        protected = charge(floors)
        require(protected <= capacity, f"{resource}: protected allocations exceed budget ({protected} > {capacity})")
        grid = spec["factorGridDenominator"]

        def values(tick):
            return [max(floor, int(weight * F(tick, grid))) for weight, floor in zip(weights, floors)]

        # A single running row already exceeds capacity at this upper bound.
        active = next(i for i, row in enumerate(rows) if row["replicas"])
        low, high = 0, ceil((capacity + 1) / weights[active] * grid)
        while low < high:
            mid = (low + high + 1) // 2
            if charge(values(mid)) <= capacity:
                low = mid
            else:
                high = mid - 1
        chosen = values(low)
        require(min(chosen) > 0 and charge(chosen) <= capacity, "invalid final allocation")
        for value in chosen:
            integer(value)
        old = [r["oldRequests"][resource] for r in rows]
        allocations[resource] = chosen
        resource_ledger[resource] = {"factor": str(F(low, grid)), "capacity": str(capacity),
            "protectedCharge": str(protected), "remainingAfterProtected": str(capacity - protected),
            "offeringCharge": str(charge(chosen)), "oldOfferingCharge": str(charge(old)),
            "unused": str(capacity - charge(chosen)), "nextGridCharge": str(charge(values(low + 1))),
            "steadyRequests": str(fixed + sum(v * r["replicas"] for v, r in zip(chosen, rows))),
            "schedulerRequests": str(fixed + sum(p["replicas"] * pod_charge(p, chosen, resource, spec["quota"])["request"] for p in pods))}
    ledger_rows, entries = [], []
    for i, row in enumerate(rows):
        request = {k: allocations[k][i] for k in RESOURCES}
        memory_limit = ceil(F(request["memoryMiB"] * 11, 10))
        require(memory_limit * 1048576 <= 2**63 - 1, "memory bytes exceed resolver range")
        percent = 0 if row["nonGo"] else spec["goMemLimitPercent"]
        env = {"GOMEMLIMIT": str(request["memoryMiB"] * 1048576 * percent // 100) + "B"} if percent else {}
        identity = row["identity"]
        kas = identity == {"kind": "Deployment", "workload": "kube-apiserver", "container": "kube-apiserver"}
        entry = {"identity": identity, "floor": request,
                 "cpuRatio": {"numerator": int(kas), "denominator": 1},
                 "memoryRatio": {"numerator": int(kas), "denominator": 1}}
        if row["nonGo"]:
            entry["goMemoryLimitPercent"] = 0
        entries.append(entry)
        delta = {k: request[k] - row["oldRequests"][k] for k in RESOURCES}
        risks = [row["startupNote"]]
        if not present[i]:
            risks.append("Absent from saved Pod layouts; allocation is NOT charged startup coverage.")
        if not row["quietObserved"]:
            risks.append("No quiet usage evidence; weight is explicit historical baseline, not measured demand.")
        if row["quietPeakMemoryMiB"] is not None and F(row["quietPeakMemoryMiB"]) > memory_limit:
            risks.append("Memory limit below historical quiet charge peak (not a startup minimum).")
        if any(v < 0 for v in delta.values()):
            risks.append("Donor reduction may expose CPU/deadline or memory failures.")
        ledger_rows.append({**row, "layoutInstances": present[i], "requests": request,
            "limits": {"cpu": f'{request["cpuMilli"]}m', "memory": f"{memory_limit}Mi"}, "env": env,
            "oldLimits": {"cpu": f'{row["oldRequests"]["cpuMilli"]}m',
                          "memory": f'{ceil(F(row["oldRequests"]["memoryMiB"] * 11, 10))}Mi'},
            "oldEnv": {"GOMEMLIMIT": str(row["oldRequests"]["memoryMiB"] * 1048576 * percent // 100) + "B"} if percent else {},
            "delta": delta, "steadyDonorContribution": {k: max(0, -v) * row["replicas"] for k, v in delta.items()},
            "childQuotaMilli": str(quota(request["cpuMilli"], spec["quota"])), "risks": risks})
    anchors = [e["floor"] for e in entries if e["cpuRatio"]["numerator"]]
    require(len(anchors) == 1, "resolver requires exactly one regular Deployment KAS anchor")
    profile = {"version": "v1", "name": spec["name"], "description": "Offline budget candidate; startup and throughput unqualified.",
        "anchor": anchors[0], "rounding": dict.fromkeys(RESOURCES, 1), "safetyLimit": {"numerator": 11, "denominator": 10},
        "cpuLimitEqualsRequest": True, "goMemLimitPercent": spec["goMemLimitPercent"], "containers": entries}
    pod_rows = []
    for pod in pods:
        item = {**pod, "resources": {}}
        for k in RESOURCES:
            new = pod_charge(pod, allocations[k], k, spec["quota"])
            old = pod_charge(pod, [r["oldRequests"][k] for r in rows], k, spec["quota"])
            item["resources"][k] = {**{key: str(v) for key, v in new.items()}, "oldCharge": str(old["charge"]),
                "replicatedChargeDelta": str((new["charge"] - old["charge"]) * pod["replicas"])}
        pod_rows.append(item)
    cost = price * max(F(resource_ledger[k]["offeringCharge"]) / node[k] for k in RESOURCES)
    ledger = {"version": spec["version"], "qualified": False, "startupTested": False,
        "inputCanonicalSHA256": digest(encode(spec)),
        "costUSDPerHour": str(cost), "formula": "nodePrice * max(offeringQuotaMilli/rawMilli, offeringChargedMiB/rawMiB)",
        "resources": resource_ledger, "rows": ledger_rows, "pods": pod_rows,
        "missingLayoutRows": [i for i in range(len(rows)) if not present[i]],
        "inventory": spec["inventory"], "node": node, "quota": spec["quota"],
        "downstream": spec["downstream"], "excludedInstrumentation": spec["excludedInstrumentation"],
        "resolverVerified": False}
    return profile, ledger


def markdown(ledger):
    node, downstream = ledger["node"], ledger["downstream"]
    lines = ["# Candidate Budget Ledger", "", f'Exact cost: **{ledger["costUSDPerHour"]} USD/HCP-hour**.',
        f'Input SHA256: `{ledger["inputSHA256"]}`.', "", ledger["formula"], "",
        f'{node["sku"]}, {node["region"]}: {node["priceUSDPerHour"]} USD/node-hour; raw {node["cpuMilli"]}m/{node["memoryMiB"]}Mi; retrieved {node["retrievedAt"]}.',
        f'Fixed downstream: {downstream["cpuMilli"]}m/{downstream["memoryMiB"]}Mi. Saved-layout requests: {downstream["actualLayoutRequests"]}.',
        f'Excluded instrumentation: {ledger["excludedInstrumentation"]}.',
        f'Existing CLI verified: {ledger["resolverVerified"]}. Missing layout row indexes: {ledger["missingLayoutRows"]}.', "",
        "Independent floor-constrained ratios are not throughput guarantees. Not startup-qualified.",
        "Downstream is fixed request accounting, not a hard cap. Source receipts do not verify deployed images.", "",
        "| Resource | Factor | Protected charge | Remaining | New charge | Old charge | Slack |",
        "| --- | --- | --- | --- | --- | --- | --- |"]
    for k, v in ledger["resources"].items():
        lines.append("| " + " | ".join([k] + [v[f] for f in ("factor", "protectedCharge", "remainingAfterProtected", "offeringCharge", "oldOfferingCharge", "unused")]) + " |")
    lines += ["", "Steady donors are request deltas, NOT additive Pod-stage savings; exact Pod deltas are in ledger.json.", "",
        "| Workload/container | Steady/layout replicas | Old m/Mi | New m/Mi | CPU/memory limits | GOMEMLIMIT | Donor m/Mi | Risks |",
        "| --- | --- | --- | --- | --- | --- | --- | --- |"]
    for row in ledger["rows"]:
        pair = lambda value: "/".join(str(value[k]) for k in RESOURCES)
        identity = row["identity"]
        lines.append(f'| {identity["workload"]}/{identity["container"]} | {row["replicas"]}/{row["layoutInstances"]} | '
                     f'{pair(row["oldRequests"])} | {pair(row["requests"])} | {row["limits"]["cpu"]}/{row["limits"]["memory"]} | '
                     f'{row["env"].get("GOMEMLIMIT", "omitted")} | {pair(row["steadyDonorContribution"])} | {"; ".join(row["risks"])} |')
    return ("\n".join(lines) + "\n").encode()


def render(binary, profile_path, spec, ledger):
    """Use the existing CLI, never synthesize the CSC schema here."""
    raw = binary.read_bytes()
    outputs = {}
    for verb in ("resolve", "hypershift"):
        args = [str(binary.resolve()), "sizing", verb, "--profile", str(profile_path)]
        if verb == "hypershift":
            args += ["--default-cpu-milli", str(spec["fallback"]["cpuMilli"]), "--default-memory-mib", str(spec["fallback"]["memoryMiB"])]
        outputs[verb + ".json"] = subprocess.run(args, check=True, capture_output=True, timeout=120).stdout
    require(binary.read_bytes() == raw, "CLI binary changed during rendering")
    resolved = json.loads(outputs["resolve.json"])["containers"]
    policy = json.loads(outputs["hypershift.json"])["spec"]["sizes"][0]["effects"]["containerResourcePolicy"]
    key = lambda row: (row["identity"]["workload"], row["identity"]["container"])
    resolved = {key(r): r for r in resolved}
    wire = {(r["workload"], r["container"]): r for r in policy["containers"]}
    require(len(resolved) == len(wire) == len(ledger["rows"]), "CLI coverage mismatch")
    require(policy["defaultRequests"] == {"cpu": f'{spec["fallback"]["cpuMilli"]}m', "memory": f'{spec["fallback"]["memoryMiB"]}Mi'}, "fallback mismatch")
    require(policy["cpuLimitEqualsRequest"] is True and policy["memoryLimitPercent"] == 110 and
            policy["goMemoryLimitPercent"] == spec["goMemLimitPercent"], "wire policy mismatch")
    for row in ledger["rows"]:
        r, w = resolved[key(row)], wire[key(row)]
        expected = {"cpu": f'{row["requests"]["cpuMilli"]}m', "memory": f'{row["requests"]["memoryMiB"]}Mi'}
        require(r["identity"] == row["identity"] and r["requests"] == w["requests"] == expected and
                r["limits"] == row["limits"] and r.get("env", {}) == row["env"], "CLI allocation/runtime mismatch")
        require(w.get("goMemoryLimitPercent", spec["goMemLimitPercent"]) == (0 if row["nonGo"] else spec["goMemLimitPercent"]), "wire Go exception mismatch")
    ledger.update(resolverVerified=True, cliSHA256=digest(raw))
    return outputs


def run(input_path, output, check=False, binary=None):
    raw = input_path.read_bytes()
    def unique(pairs):
        result = dict(pairs)
        require(len(result) == len(pairs), "duplicate JSON key")
        return result
    spec = json.loads(raw, object_pairs_hook=unique)
    profile, ledger = allocate(spec)
    ledger["inputSHA256"] = digest(raw)
    ledger["allocatorSHA256"] = digest(Path(__file__).read_bytes())
    outputs = {"input.json": raw, "candidate.json": encode(profile)}
    require(output.parent.is_dir() and not output.is_symlink(), "existing output parent/non-symlink required")
    require(output.is_dir() if check else not output.exists(), "check needs existing output; allocate never overwrites")
    with tempfile.TemporaryDirectory(prefix="budget-", dir=output.parent) as temporary:
        path = Path(temporary) / "candidate.json"
        path.write_bytes(outputs["candidate.json"])
        if binary:
            outputs.update(render(binary, path, spec, ledger))
        outputs.update({"ledger.json": encode(ledger), "ledger.md": markdown(ledger)})
        outputs["output-hashes.json"] = encode({name: digest(data) for name, data in outputs.items()})
        if check:
            require({p.name for p in output.iterdir()} == set(outputs), "output file set differs")
            for name, data in outputs.items():
                require(not (output / name).is_symlink() and (output / name).read_bytes() == data, "output differs: " + name)
        else:
            # Exclusive directory creation is the no-overwrite boundary. Manifest is written last.
            output.mkdir(mode=0o700)
            for name, data in outputs.items():
                with (output / name).open("xb") as stream:
                    stream.write(data)
    return {k: ledger[k] for k in ("inputSHA256", "costUSDPerHour", "resources", "resolverVerified")}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("verb", choices=("allocate", "check"))
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--hcp-perf", type=Path, help="optional frozen existing CLI binary for resolve/CSC cross-check")
    args = parser.parse_args()
    try:
        print(json.dumps(run(args.input, args.output, args.verb == "check", args.hcp_perf), indent=2))
    except (ValueError, KeyError, TypeError, ArithmeticError, OSError, subprocess.SubprocessError) as error:
        parser.exit(1, f"budget: {error}\n")


if __name__ == "__main__":
    main()
