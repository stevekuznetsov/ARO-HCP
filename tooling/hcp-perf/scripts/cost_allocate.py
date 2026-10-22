#!/usr/bin/env python3
"""Cost-first raw-capacity allocation. Only fetch-prices accesses the public network."""

import argparse
from datetime import datetime, timezone
from fractions import Fraction as F
import hashlib
import json
import os
from pathlib import Path
import subprocess
import urllib.parse
import urllib.request

import offline_budgets as b


SKUS = {"Standard_D16s_v6": (16000, 65536), "Standard_D32s_v6": (32000, 131072),
        "Standard_E16s_v6": (16000, 131072), "Standard_E32s_v6": (32000, 262144)}
API = "https://prices.azure.com/api/retail/prices"
INVENTORY = "_artifacts/small-constrained-v1/scoped-analysis/scoped-candidate.json"
INVENTORY_HASH = "ecf852a25da2ee2d9baab9ca0bbf01012407d78267c1f047ca907c75345e339a"


def encode(value):
    return (json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n").encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def save_new(path, data, refresh=False):
    if not path.parent.is_dir() or path.is_symlink():
        raise ValueError(f"expected existing parent and nonsymlink output: {path}")
    if path.exists() and path.read_bytes() == data:
        return
    with path.open("wb" if refresh else "xb") as stream:
        stream.write(data)


def select_price(items, sku):
    matches = []
    for item in items:
        labels = " ".join(str(item.get(k, "")) for k in ("productName", "skuName", "meterName"))
        if (item.get("armSkuName") == sku and item.get("armRegionName") == "westus3"
                and item.get("type") == "Consumption" and item.get("currencyCode") == "USD"
                and item.get("serviceName") == "Virtual Machines" and item.get("unitOfMeasure") == "1 Hour"
                and item.get("isPrimaryMeterRegion") is True and item.get("tierMinimumUnits") == 0
                and not any(x in labels.lower() for x in ("windows", "spot", "low priority", "lowpriority"))):
            matches.append(item)
    if len(matches) != 1:
        raise ValueError(f"expected one exact Linux on-demand retail record for {sku}, got {len(matches)}")
    price = F(str(matches[0]["retailPrice"]))
    if price <= 0 or price != F(str(matches[0]["unitPrice"])) or (price * 1000000).denominator != 1:
        raise ValueError(f"invalid or ambiguous hourly retail price: {sku}")
    return matches[0]


def fetch_prices(output):
    if not output.is_dir() or output.stat().st_mode & 0o077:
        raise ValueError("price output directory must already exist and be private (0700)")
    records, pages = {}, []
    for sku in SKUS:
        query = f"serviceName eq 'Virtual Machines' and armRegionName eq 'westus3' and armSkuName eq '{sku}' and priceType eq 'Consumption'"
        source = API + "?" + urllib.parse.urlencode({"api-version": "2023-01-01-preview", "currencyCode": "'USD'", "$filter": query})
        url, items, visited = source, [], set()
        while url:
            parsed = urllib.parse.urlparse(url)
            if parsed.scheme != "https" or parsed.netloc != "prices.azure.com" or url in visited:
                raise ValueError("unexpected retail pagination URL")
            visited.add(url)
            with urllib.request.urlopen(url, timeout=90) as response:
                raw = response.read()
            retrieved = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
            page = json.loads(raw)
            filename = f"{sku}-{len(visited)}.json"
            save_new(output / filename, raw)
            pages.append({"path": filename, "sha256": digest(raw), "sourceURL": url, "retrievedAt": retrieved})
            items.extend(page["Items"])
            url = page.get("NextPageLink")
        record = select_price(items, sku)
        records[sku] = {"record": record, "sourceURL": source, "retrievedAt": retrieved,
                        "currency": "USD", "hourlyRetailPrice": str(record["retailPrice"]),
                        "rawCPUMilli": SKUS[sku][0], "rawMemoryMiB": SKUS[sku][1]}
    snapshot = {"version": "v1", "region": "westus3", "currency": "USD", "priceBasis": "Linux Consumption on-demand retail, primary meter; not Spot, Low Priority, Windows or reservation",
                "capacityBasis": "raw-illustration", "records": records, "pages": pages,
                "capacitySources": ["https://learn.microsoft.com/azure/virtual-machines/sizes/general-purpose/dsv6-series",
                                    "https://learn.microsoft.com/azure/virtual-machines/sizes/memory-optimized/esv6-series"]}
    save_new(output / "prices.json", encode(snapshot))
    print(json.dumps({sku: r["hourlyRetailPrice"] for sku, r in records.items()}, indent=2))


def allocate(containers, raw_cpu, raw_memory, price, budget, fixed_cpu, fixed_memory,
             limit_aware=True, uniform=True, minimum_cpu=1, minimum_memory=1):
    """Exact fractional envelopes; round requests down, never restore old floors.

    Reserve one MiB per running controlled instance before scaling in limit mode.
    This bounds every ceil(1.1 * request) rounding remainder without overspending.
    """
    if min(raw_cpu, raw_memory, price, budget, minimum_cpu, minimum_memory) <= 0 or min(fixed_cpu, fixed_memory) < 0:
        raise ValueError("capacities, price, budget and minima must be positive; fixed demand nonnegative")
    if not containers or any(min(c["cpuMilli"], c["memoryMiB"]) <= 0 or c["replicas"] < 0 for c in containers):
        raise ValueError("positive source requests and nonnegative replicas required")
    cpu_total = sum(c["cpuMilli"] * c["replicas"] for c in containers)
    mem_total = sum(c["memoryMiB"] * c["replicas"] for c in containers)
    count = sum(c["replicas"] for c in containers)
    if min(cpu_total, mem_total) <= 0:
        raise ValueError("running controlled demand required")
    cpu_cap, mem_cap = F(raw_cpu) * budget / price, F(raw_memory) * budget / price
    cpu_scale = (cpu_cap - fixed_cpu) / cpu_total
    mem_scale = (mem_cap - fixed_memory - (count if limit_aware else 0)) / (F(11, 10) * mem_total if limit_aware else mem_total)
    if min(cpu_scale, mem_scale) <= 0:
        raise ValueError("fixed downstream demand and rounding reserve exhaust budget")
    if uniform:
        cpu_scale = mem_scale = min(cpu_scale, mem_scale)
    rows = []
    for c in containers:
        cpu, memory = int(c["cpuMilli"] * cpu_scale), int(c["memoryMiB"] * mem_scale)
        if cpu < minimum_cpu or memory < minimum_memory:
            raise ValueError(f"zero/subminimum allocation for {c['identity']}: {cpu}m/{memory}Mi; do not clamp upward")
        rows.append({**c, "allocatedCPUMilli": cpu, "allocatedMemoryMiB": memory,
                     "memoryLimitMiB": (memory * 110 + 99) // 100})
    cpu = sum(c["allocatedCPUMilli"] * c["replicas"] for c in rows)
    memory = sum(c["allocatedMemoryMiB"] * c["replicas"] for c in rows)
    limits = sum(c["memoryLimitMiB"] * c["replicas"] for c in rows)
    request_cost = price * max(F(cpu + fixed_cpu, raw_cpu), F(memory + fixed_memory, raw_memory))
    limit_cost = price * max(F(cpu + fixed_cpu, raw_cpu), F(limits + fixed_memory, raw_memory))
    charged_memory = limits if limit_aware else memory
    if cpu + fixed_cpu > cpu_cap or charged_memory + fixed_memory > mem_cap:
        raise ValueError("rounded allocation exceeds envelope")
    return {"model": ("uniform" if uniform else "independent") + ("-limit-aware" if limit_aware else "-requests"),
            "cpuScale": str(cpu_scale), "memoryScale": str(mem_scale),
            "rawEntitlementCPUMilli": str(cpu_cap), "rawEntitlementMemoryMiB": str(mem_cap),
            "controlledCPUMilli": cpu, "controlledMemoryMiB": memory, "controlledMemoryLimitMiB": limits,
            "fixedDownstreamCPUMilli": fixed_cpu, "fixedDownstreamMemoryMiB": fixed_memory,
            "offeringCPUMilli": cpu + fixed_cpu, "offeringMemoryMiB": memory + fixed_memory,
            "offeringChargedMemoryMiB": charged_memory + fixed_memory,
            "unusedCPUMilli": str(cpu_cap - cpu - fixed_cpu),
            "unusedChargedMemoryMiB": str(mem_cap - charged_memory - fixed_memory),
            "requestEquivalentUSDPerHour": str(request_cost), "limitAwareEquivalentUSDPerHour": str(limit_cost),
            "withinBudgetWithLimits": limit_cost <= budget, "rows": rows}


def scheduling_charge(pod, resources):
    """Kubernetes regular + restartable-init accounting, and sequential init maxima.

    resources(container, group) returns CPU/request/charged-memory, with unbounded
    downstream memory deliberately charged at request (not a hard cap).
    """
    regular = [0, 0, 0]
    for container in pod.get("containers", []):
        regular = [a + v for a, v in zip(regular, resources(container, "containers"))]
    sidecars, peak = [0, 0, 0], [0, 0, 0]
    for container in pod.get("initContainers", []):
        value = resources(container, "initContainers")
        if container.get("restartPolicy") == "Always":
            sidecars = [a + v for a, v in zip(sidecars, value)]
            stage = sidecars
        else:
            stage = [a + v for a, v in zip(sidecars, value)]
        peak = [max(a, v) for a, v in zip(peak, stage)]
    return [max(a, p) for a, p in zip([a + s for a, s in zip(regular, sidecars)], peak)]


def derive(root, price_path):
    hashes = {}

    def read(path, expected=None):
        raw = path.read_bytes()
        if expected and digest(raw) != expected:
            raise ValueError(f"hash mismatch: {path}")
        label = str(path.relative_to(root)) if path.is_relative_to(root) else str(path)
        hashes[label] = digest(raw)
        return json.loads(raw)

    inventory = read(root / INVENTORY, INVENTORY_HASH)
    prices = read(price_path)
    if prices["currency"] != "USD" or prices["region"] != "westus3" or set(prices["records"]) != set(SKUS):
        raise ValueError("price snapshot must contain all four exact SKUs")
    raw_items = []
    for page in prices["pages"]:
        raw_items.extend(read(price_path.parent / page["path"], page["sha256"])["Items"])
    for sku, record in prices["records"].items():
        datetime.fromisoformat(record["retrievedAt"].replace("Z", "+00:00"))
        if record["currency"] != "USD" or not record["sourceURL"].startswith(API + "?"):
            raise ValueError("invalid price provenance")
        if select_price(raw_items, sku) != record["record"] or F(record["hourlyRetailPrice"]) != F(str(record["record"]["retailPrice"])):
            raise ValueError("selected retail record differs from saved raw page")
        if (record["rawCPUMilli"], record["rawMemoryMiB"]) != SKUS[sku]:
            raise ValueError("unexpected raw capacity")
    containers, fixed = [], [0, 0]
    for row in inventory["rows"]:
        req = row["candidateResources"].get("requests", {})
        if row["scope"] == "controlled":
            containers.append({"identity": row["projectorIdentity"], "selector": row["selector"],
                               "cpuMilli": int(b.quantity(req["cpu"]) * 1000),
                               "memoryMiB": int(b.quantity(req["memory"]) / b.MIB),
                               "replicas": row["activeReplicas"],
                               "nonGoException": not any(e["name"] == "GOMEMLIMIT" for e in row["candidateRuntimeEnv"])})
        elif row["scope"] == "downstream" and row["activeReplicas"]:
            fixed[0] += int(b.quantity(req["cpu"]) * 1000) * row["activeReplicas"]
            fixed[1] += int(b.quantity(req["memory"]) / b.MIB) * row["activeReplicas"]
    if fixed != [470, 2090] or (sum(c["cpuMilli"] * c["replicas"] for c in containers),
                               sum(c["memoryMiB"] * c["replicas"] for c in containers)) != (6020, 14016):
        raise ValueError("source ratio inventory changed")
    frozen = read(root / "_artifacts/small-constrained-v1/analysis/candidate-budgets.json",
                  "c0617bddb79fa78d2c652895dc28ffa75dc041237523f3990670dfad13b375a8")
    observations = read(root / "_artifacts/small-constrained-v1/analysis/observations.json",
                        "abea0bf189f9b941e68601b2d16d2f2726e26d19aed03b754ba6c6b119d64558")
    pod_path = frozen["snapshots"][-1]["path"] + "/full-pods.json"
    frozen_inputs = read(root / "_artifacts/small-constrained-v1/analysis/input-hashes.json",
                        "a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e")
    pods = read(root / pod_path, frozen_inputs["files"][pod_path]["sha256"])["items"]
    pod_workload = {}
    for item in observations["instances"]:
        s = item["selector"]
        key = (s["kind"], s["workload"])
        if item["podUID"] in pod_workload and pod_workload[item["podUID"]] != key:
            raise ValueError("ambiguous pod owner mapping")
        pod_workload[item["podUID"]] = key
    rows_by_selector = {(r["selector"]["kind"], r["selector"]["workload"], r["selector"]["containerGroup"], r["selector"]["container"]): r
                        for r in inventory["rows"]}
    models, selected, profiles = [], {}, {}
    for budget, name in ((F(1, 8), "small-cost-v1"), (F(1, 4), "double-small-cost-v1")):
        options = []
        for sku, (cpu, memory) in SKUS.items():
            price = F(prices["records"][sku]["hourlyRetailPrice"])
            for limit_aware in (False, True):
                for uniform in (False, True):
                    allocation = allocate(containers, cpu, memory, price, budget, *fixed, limit_aware, uniform)
                    model = {"sku": sku, "budgetUSDPerHour": str(budget), **allocation}
                    models.append({k: v for k, v in model.items() if k != "rows"})
                    if limit_aware and uniform:
                        options.append(model)
        # Highest preserved CPU:RAM-pair fraction; ties prefer fewer cores then SKU name.
        chosen = max(options, key=lambda a: (F(a["cpuScale"]), -SKUS[a["sku"]][0], a["sku"]))
        allocated = {b.canonical(c["selector"]): c for c in chosen["rows"]}
        scheduler, absent = [0, 0, 0], []
        seen_pods = set()
        for pod in pods:
            uid = pod["metadata"]["uid"]
            if uid not in pod_workload:
                continue
            seen_pods.add(uid)
            kind, workload = pod_workload[uid]
            if workload == "kube-state-metrics-hcp":
                continue
            if pod["spec"].get("overhead") or pod["spec"].get("resources"):
                raise ValueError("Pod overhead or Pod-level resources require explicit allocation accounting")

            def resource(container, group):
                key = kind, workload, group, container["name"]
                row = rows_by_selector[key]
                if row["scope"] == "controlled":
                    a = allocated[b.canonical(row["selector"])]
                    return [a["allocatedCPUMilli"], a["allocatedMemoryMiB"], a["memoryLimitMiB"]]
                req = container.get("resources", {}).get("requests", {})
                if "cpu" not in req or "memory" not in req:
                    absent.append({"pod": pod["metadata"]["name"], "container": container["name"], "group": group})
                # Missing requests are zero only in Kubernetes scheduling arithmetic,
                # not a claim of zero usage or a startup budget.
                return [int(b.quantity(req.get("cpu", "0")) * 1000),
                        int(b.quantity(req.get("memory", "0")) / b.MIB),
                        int(b.quantity(req.get("memory", "0")) / b.MIB)]

            scheduler = [a + v for a, v in zip(scheduler, scheduling_charge(pod["spec"], resource))]
        if seen_pods != set(pod_workload):
            raise ValueError("incomplete actual Pod layout")
        price = F(prices["records"][chosen["sku"]]["hourlyRetailPrice"])
        raw_cpu, raw_memory = SKUS[chosen["sku"]]
        startup_cost = price * max(F(scheduler[0], raw_cpu), F(scheduler[2], raw_memory))
        if startup_cost > budget:
            raise ValueError("actual running-Pod init scheduling maxima exceed budget; no profile emitted")
        chosen["runningPodScheduling"] = {"cpuMilli": scheduler[0], "memoryRequestMiB": scheduler[1],
                                            "chargedMemoryMiB": scheduler[2], "equivalentUSDPerHour": str(startup_cost),
                                            "absentDownstreamInitRequests": absent,
                                            "excludedCompletedJobsAndDisabledWorkloads": True,
                                            "unmeasuredBootstrap": True}
        scale = F(chosen["cpuScale"])
        fallback = {"cpuMilli": int(10 * scale), "memoryMiB": int(64 * scale)}
        if min(fallback.values()) < 1:
            raise ValueError("scaled fallback below representable minimum")
        entries = []
        for c in chosen["rows"]:
            floor = {"cpuMilli": c["allocatedCPUMilli"], "memoryMiB": c["allocatedMemoryMiB"]}
            identity = c["identity"]
            kas = identity == {"kind": "Deployment", "workload": "kube-apiserver", "container": "kube-apiserver"}
            if floor == fallback and not c["nonGoException"] and not kas:
                continue
            entry = {"identity": identity, "floor": floor, "cpuRatio": {"numerator": int(kas), "denominator": 1},
                     "memoryRatio": {"numerator": int(kas), "denominator": 1}}
            if c["nonGoException"]:
                entry["goMemoryLimitPercent"] = 0
            entries.append(entry)
        anchor = next(c["floor"] for c in entries if c["identity"]["workload"] == "kube-apiserver" and c["identity"]["container"] == "kube-apiserver")
        profiles[name] = {"version": "v1", "name": name,
                          "description": f"COST-FIRST STARTUP-UNTESTED candidate: ${float(budget):.3f}/HCP-hour raw {chosen['sku']} compute equivalent, not a bill or namespace cap. Uniform scale {scale} preserves source CPU:RAM pairs before 1m/1Mi round-down; no original floors. Downstream fixed requests 470m/2090Mi; KSM excluded. Memory limits fully charged at ceil(110%). REQUIRED sizing hypershift fallback flags: --default-cpu-milli {fallback['cpuMilli']} --default-memory-mib {fallback['memoryMiB']}. Failure at startup or tiny population/QPS is acceptable evidence, not permission to exceed budget.",
                          "anchor": anchor, "rounding": {"cpuMilli": 1, "memoryMiB": 1},
                          "safetyLimit": {"numerator": 11, "denominator": 10},
                          "cpuLimitEqualsRequest": True, "goMemLimitPercent": 80, "containers": entries}
        chosen["fallback"] = fallback
        selected[name] = chosen
    for relative in ("tooling/hcp-perf/scripts/cost_allocate.py", "tooling/hcp-perf/scripts/test_cost_allocate.py",
                     "tooling/hcp-perf/internal/sizing/sizing.go", "tooling/hcp-perf/internal/sizing/hypershift.go",
                     "tooling/hcp-perf/internal/cost/cost.go", "tooling/hcp-perf/cost_cli.go"):
        hashes[relative] = digest((root / relative).read_bytes())
    result = {"version": "v1", "qualified": False, "capacityBasis": "raw-illustration", "currency": "USD",
              "feeUSDPerHCPHour": "0.25", "firstBudgetUSDPerHCPHour": "0.125", "nextBudgetUSDPerHCPHour": "0.25",
              "sourceHashes": hashes, "sourceRatio": {"controlledInstances": 97, "cpuMilli": 6020, "memoryMiB": 14016},
              "instrumentationExcluded": {"instances": 1, "cpuMilli": 10, "memoryMiB": 64, "memoryLimitMiB": 256},
              "models": models, "selected": selected,
              "warnings": ["Raw SKU resource equivalents are not a guaranteed $/HCP bill: no allocatable, system/DaemonSet, fragmentation, HA, NIC or Pod-slot allowance.",
                           "Downstream resources stay unchanged and unbounded; fixed requests may undercharge actual consumption (including measured OVN socks-proxy excess).",
                           "Uniform scaling preserves every CPU:RAM pair and cross-container ratios before quantization. Independent alternatives preserve each resource's distribution but change CPU:RAM pairs.",
                           "No original floors; a budget can fail startup or tiny population/QPS. Bootstrap, completed Jobs and disabled workloads are unmeasured; no concurrency budget invented.",
                           "The 0.25 candidate is a doubled cost envelope, not exactly doubled requests: the fixed downstream charge is subtracted once."]}
    return result, profiles


def run_allocation(root, prices, output, write, refresh=False, hypershift=None):
    if not output.is_dir() or output.is_symlink() or output.stat().st_mode & 0o077:
        raise ValueError("allocation output directory must exist and be private")
    if refresh and (not output.is_relative_to(root / "_artifacts/small-cost-v1")
                    or not (output / "allocation.json").is_file()):
        raise ValueError("refresh requires this experiment's existing allocation output")
    result, profiles = derive(root, prices)
    outputs = {}
    env = dict(os.environ, GOWORK="off", GOPROXY="off", GOSUMDB="off")
    for name, profile in profiles.items():
        path = root / "tooling/hcp-perf/examples" / (name + ".json")
        desired = encode(profile)
        if write or refresh:
            save_new(path, desired, refresh)
        elif path.read_bytes() != desired:
            raise ValueError(f"profile does not reproduce: {path}")
        result.setdefault("profiles", {})[name] = {"path": str(path.relative_to(root)), "bytes": len(desired), "sha256": digest(desired)}
        chosen = result["selected"][name]
        for command in ("resolve", "hypershift"):
            args = ["go", "run", ".", "sizing", command, "--profile", str(path)]
            if command == "hypershift":
                args += ["--default-cpu-milli", str(chosen["fallback"]["cpuMilli"]), "--default-memory-mib", str(chosen["fallback"]["memoryMiB"])]
            raw = subprocess.run(args, cwd=root / "tooling/hcp-perf", env=env, check=True, capture_output=True).stdout
            outputs[f"{name}-{command}.json"] = raw
        resolved = json.loads(outputs[f"{name}-resolve.json"])
        rendered = json.loads(outputs[f"{name}-hypershift.json"])
        if hypershift:
            import jsonschema
            import yaml
            schema_path = hypershift / "cmd/install/assets/crds/hypershift-operator/scheduling.hypershift.openshift.io_clustersizingconfigurations.yaml"
            schema_bytes = schema_path.read_bytes()
            crd = yaml.safe_load(schema_bytes)
            schema = next(v["schema"]["openAPIV3Schema"] for v in crd["spec"]["versions"] if v["name"] == "v1alpha1")
            jsonschema.Draft4Validator(schema).validate(rendered)
            result["localOpenAPISchema"] = {"source": str(schema_path), "sha256": digest(schema_bytes),
                                            "CELAdmissionTested": False, "liveAdmissionTested": False}
        policy = rendered["spec"]["sizes"][0]["effects"]["containerResourcePolicy"]
        explicit = {(c["identity"]["workload"], c["identity"]["container"]): c for c in resolved["containers"]}
        wire = {(c["workload"], c["container"]): c for c in policy["containers"]}
        if explicit.keys() != wire.keys() or len(explicit) != len(resolved["containers"]) or policy["memoryLimitPercent"] != 110 or policy["cpuLimitEqualsRequest"] is not True:
            raise ValueError("rendered policy differs")
        for c in chosen["rows"]:
            key = c["identity"]["workload"], c["identity"]["container"]
            requests = {"cpu": f"{c['allocatedCPUMilli']}m", "memory": f"{c['allocatedMemoryMiB']}Mi"}
            if wire.get(key, {"requests": policy["defaultRequests"]})["requests"] != requests:
                raise ValueError(f"effective wire allocation differs: {key}")
            if key in explicit:
                want_env = {} if c["nonGoException"] else {"GOMEMLIMIT": str(c["allocatedMemoryMiB"] * b.MIB * 80 // 100) + "B"}
                r = explicit[key]
                if r["requests"] != requests or r["limits"] != {"cpu": requests["cpu"], "memory": f"{c['memoryLimitMiB']}Mi"} or r.get("env", {}) != want_env:
                    raise ValueError(f"resolved requests/limits/Go env differ: {key}")
        result["profiles"][name]["everyControlledRowMatchesCLI"] = True
        price_record = json.loads(prices.read_bytes())["records"][chosen["sku"]]
        cpu, memory = SKUS[chosen["sku"]]
        spec = {"version": "v1", "currency": "USD", "feeMicroUSDPerHCPHour": 250000,
                "targetPercent": 50 if name == "small-cost-v1" else 100,
                "skus": [{"name": chosen["sku"], "region": "westus3", "capacityBasis": "raw-illustration",
                          "rawCPUMilli": cpu, "rawMemoryMiB": memory,
                          "allocatableCPUMilli": 0, "allocatableMemoryMiB": 0,
                          "overheadCPUMilli": 0, "overheadMemoryMiB": 0,
                          "priceMicroUSDPerHour": int(F(price_record["hourlyRetailPrice"]) * 1000000),
                          "priceSourceURL": price_record["sourceURL"], "priceRetrievedAt": price_record["retrievedAt"]}],
                "dataPoints": [{"name": name + "-limit-aware-charge", "sourceKind": "requests",
                                "guestNodes": 0, "apps": 0,
                                "demand": {"cpuMilli": chosen["offeringCPUMilli"], "memoryMiB": chosen["offeringChargedMemoryMiB"],
                                           "podSlots": 0, "swiftNICs": 0}}]}
        outputs[f"{name}-cost-input.json"] = encode(spec)
        estimate = subprocess.run(["go", "run", ".", "cost", "estimate", "--input", "/dev/stdin"],
                                  input=encode(spec), cwd=root / "tooling/hcp-perf", env=env,
                                  check=True, capture_output=True).stdout
        if not json.loads(estimate)["estimates"][0]["withinTarget"]:
            raise ValueError("existing cost estimator rejects allocation")
        outputs[f"{name}-cost-estimate.json"] = estimate
    outputs["allocation.json"] = encode(result)
    outputs["output-hashes.json"] = encode({name: {"bytes": len(raw), "sha256": digest(raw)} for name, raw in outputs.items()})
    if not output.is_dir() or output.stat().st_mode & 0o077:
        raise ValueError("allocation output directory must exist and be private")
    for name, raw in outputs.items():
        if write or refresh:
            save_new(output / name, raw, refresh)
        elif (output / name).read_bytes() != raw:
            raise ValueError(f"output does not reproduce: {name}")
    print(json.dumps({"profiles": result["profiles"],
                      "selected": {name: {k: v for k, v in a.items() if k != "rows"} for name, a in result["selected"].items()}}, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    fetch = sub.add_parser("fetch-prices", help="read-only public Azure Retail API; exclusive local snapshots")
    fetch.add_argument("--output", type=Path, required=True)
    allocation = sub.add_parser("allocate", help="offline derivation, CLI verification and byte-for-byte replay")
    allocation.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[3])
    allocation.add_argument("--prices", type=Path, required=True)
    allocation.add_argument("--output", type=Path, required=True)
    allocation.add_argument("--hypershift-root", type=Path, help="optional local CRD OpenAPI validation using PyYAML/jsonschema")
    mode = allocation.add_mutually_exclusive_group()
    mode.add_argument("--write", action="store_true", help="exclusively create generated profiles and outputs; default verifies")
    mode.add_argument("--refresh", action="store_true", help="explicitly refresh this tool's own versioned outputs only")
    args = parser.parse_args()
    if args.command == "fetch-prices":
        fetch_prices(args.output.resolve())
    else:
        run_allocation(args.root.resolve(), args.prices.resolve(), args.output.resolve(), args.write, args.refresh,
                       args.hypershift_root.resolve() if args.hypershift_root else None)


if __name__ == "__main__":
    main()
