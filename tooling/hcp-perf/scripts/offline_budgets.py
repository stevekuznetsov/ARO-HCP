#!/usr/bin/env python3
"""Derive experimental quiet-window budgets from saved evidence only (stdlib)."""

import argparse
from collections import defaultdict
from datetime import datetime
from decimal import Decimal, ROUND_CEILING
import hashlib
import json
import math
import os
from pathlib import Path
import re
import statistics


VERSION = "hcp-perf.offline-budgets/v1"
MIB = 1024 ** 2
POLICY_HASH = "hypershift.openshift.io/container-resource-policy-hash"
WINDOWS = [
    {"name": "fifteen-application-pods", "start": "2026-09-10T02:28:00Z",
     "end": "2026-09-10T02:36:00Z", "applicationPods": 15},
    {"name": "zero-application-pods", "start": "2026-09-10T03:04:00Z",
     "end": "2026-09-10T03:10:00Z", "applicationPods": 0},
]
SNAPSHOTS = [
    "probe-tolerance-v2/accepted-baseline",
    "probe-tolerance-v2/review/after",
    "density-v5-apps30/review/after",
    "density-v6-apps60/before",
]
METRICS = {
    "container-cpu-rate": "cpuRateCores",
    "container-memory": "chargeBytes",
    "container-rss": "cgroupRSSBytes",
    "container-working-set": "workingSetBytes",
    "process-rss": "processRSSBytes",
    "go-heap-alloc": "goHeapAllocBytes",
    "go-heap-inuse": "goHeapInuseBytes",
}
TIMESTAMPS = {
    "container-cpu-rate": "container-cpu-time",
    "container-cpu-total": "container-cpu-time",
    "container-memory": "container-memory-time",
    "process-rss": "process-rss-time",
}


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def timestamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def quantity(value):
    """Base units, retaining absent quantities as None rather than zero."""
    if value is None:
        return None
    match = re.fullmatch(r"([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)([a-zA-Z]*)", str(value))
    if not match:
        raise ValueError(f"invalid quantity: {value}")
    suffixes = {"": 1, "n": Decimal("1e-9"), "u": Decimal("1e-6"), "m": Decimal(".001")}
    suffixes.update({s: 1000 ** i for i, s in enumerate(["k", "M", "G", "T", "P", "E"], 1)})
    suffixes.update({s + "i": 1024 ** i for i, s in enumerate(["K", "M", "G", "T", "P", "E"], 1)})
    result = Decimal(match[1]) * suffixes[match[2]]
    if result < 0:
        raise ValueError("negative resource quantity")
    return result


def proposed(baseline, peak, headroom, quantum):
    if peak is None:
        return baseline
    raised = (Decimal(str(peak)) * Decimal(headroom) / Decimal(quantum)).to_integral_value(rounding=ROUND_CEILING) * Decimal(quantum)
    return max(baseline, raised) if baseline is not None else raised


def stats(values, baseline=None):
    values = sorted(values)
    return {"samples": len(values), "max": max(values) if values else None,
            "p95": values[math.ceil(.95 * len(values)) - 1] if values else None,
            "median": statistics.median(values) if values else None,
            "fractionAboveRequest": (sum(v > baseline for v in values) / len(values)
                                     if values and baseline is not None else None)}


class Evidence:
    def __init__(self, root):
        self.root = root.resolve()
        self.files = {}

    def read(self, path, expected=None, parse=True):
        path = path.resolve()
        if not path.is_relative_to(self.root):
            raise ValueError(f"evidence path escapes root: {path}")
        sha = hashlib.sha256()
        if parse:
            raw = path.read_bytes()
            sha.update(raw)
        else:
            with path.open("rb") as stream:
                for block in iter(lambda: stream.read(1024 * 1024), b""):
                    sha.update(block)
        actual = sha.hexdigest()
        key = str(path.relative_to(self.root))
        if expected is not None and actual != expected:
            raise ValueError(f"SHA256 mismatch: {key}")
        old = self.files.get(key)
        if old and old["sha256"] != actual:
            raise ValueError(f"evidence changed while reading: {key}")
        self.files[key] = {"sha256": actual, "bytes": path.stat().st_size,
                           "manifestHashVerified": expected is not None or bool(old and old["manifestHashVerified"])}
        return json.loads(raw) if parse else None


def controller(obj, objects):
    """Stop at the workload, never collapse it to the owning HCP."""
    seen = set()
    while obj["kind"] in ("Pod", "ReplicaSet"):
        refs = [r for r in obj["metadata"].get("ownerReferences", []) if r.get("controller")]
        if not refs and obj["kind"] == "Pod":
            return obj
        if len(refs) != 1 or refs[0]["uid"] not in objects:
            raise ValueError(f"unresolved exact owner: {obj['metadata']['name']}")
        ref = refs[0]
        owner = objects[ref["uid"]]
        if (owner["kind"], owner["metadata"]["name"], owner["metadata"].get("namespace")) != (
                ref["kind"], ref["name"], obj["metadata"].get("namespace")):
            raise ValueError("owner UID/kind/name/namespace mismatch")
        if ref["uid"] in seen:
            raise ValueError("owner cycle")
        seen.add(ref["uid"])
        obj = owner
    return obj


def container_config(c):
    return {"resources": {k: {r: str(quantity(v)) for r, v in c.get("resources", {}).get(k, {}).items()
                               if r in ("cpu", "memory")} for k in ("requests", "limits")},
            "image": c["image"], "restartPolicy": c.get("restartPolicy"),
            "env": sorted([e for e in c.get("env", []) if e["name"] in ("GOMEMLIMIT", "GOMAXPROCS")],
                          key=lambda e: e["name"])}


def inventory(pods, workloads, normalized):
    objects = {w["metadata"]["uid"]: w for w in workloads}
    actual = {(p["metadata"]["uid"], c["group"], c["name"]): c
              for p in normalized for c in p["containers"]}
    rows, instances = {}, {}
    for obj in workloads + [p for p in pods if not p["metadata"].get("ownerReferences")]:
        if obj["kind"] == "ReplicaSet":
            continue
        spec = obj["spec"]
        template = (spec["jobTemplate"]["spec"]["template"] if obj["kind"] == "CronJob"
                    else obj if obj["kind"] == "Pod" else spec["template"])
        for group in ("containers", "initContainers"):
            for c in template["spec"].get(group, []):
                key = (obj["metadata"]["namespace"], obj["kind"], obj["metadata"]["name"], group, c["name"])
                rows[key] = {"selector": dict(zip(("namespace", "kind", "workload", "containerGroup", "container"), key)),
                             "workloadUID": obj["metadata"]["uid"], "podSelector": spec.get("selector"),
                             "template": c, "replicas": spec.get("replicas"),
                             "policyManaged": POLICY_HASH in template.get("metadata", {}).get("annotations", {}),
                             "instances": []}
    for pod in pods:
        workload = controller(pod, objects)
        pmeta = pod["metadata"]
        statuses = {s["name"]: s for field in ("containerStatuses", "initContainerStatuses")
                    for s in pod["status"].get(field, [])}
        for group in ("containers", "initContainers"):
            for c in pod["spec"].get(group, []):
                key = (pmeta["namespace"], workload["kind"], workload["metadata"]["name"], group, c["name"])
                if key not in rows:
                    raise ValueError(f"missing workload template: {key}")
                if container_config(c) != container_config(actual[(pmeta["uid"], group, c["name"])]):
                    raise ValueError(f"full/normalized inventory mismatch: {key}")
                if container_config(c) != container_config(rows[key]["template"]):
                    raise ValueError(f"actual/template config mismatch: {key}")
                status = statuses.get(c["name"], {})
                steady = group == "containers" or c.get("restartPolicy") == "Always"
                running = steady and pod["status"].get("phase") not in ("Succeeded", "Failed") and "running" in status.get("state", {})
                identity = "/".join((pmeta["uid"], group, c["name"]))
                item = {"id": identity, "pod": pmeta["name"], "podUID": pmeta["uid"], "group": group,
                        "container": c["name"], "namespace": pmeta["namespace"], "node": pod["spec"].get("nodeName"),
                        "containerID": status.get("containerID"), "imageID": status.get("imageID"),
                        "restartCount": status.get("restartCount"), "running": running,
                        "startedAt": status.get("state", {}).get("running", {}).get("startedAt"),
                        "config": container_config(c), "row": key}
                instances[identity] = item
                rows[key]["instances"].append(identity)
    return rows, instances


def stable_active(inventories):
    reference = None
    for _, instances in inventories:
        active = {k: v for k, v in instances.items() if v["running"]}
        if reference is not None and active != reference:
            raise ValueError("active UID/image/runtime/config/replica version changed across snapshots")
        reference = active
    return reference


def labels_key(labels):
    return tuple(sorted((k, v) for k, v in labels.items() if k != "__name__"))


def load_stream(evidence, directory, windows, namespace):
    wanted = set(METRICS) | set(TIMESTAMPS.values()) | {"container-cpu-total", "up", "scrape-time"}
    series, grids, audit = defaultdict(dict), defaultdict(set), []
    expressions = {}
    for path in sorted(directory.glob("chunk-*/manifest.json")):
        manifest = evidence.read(path)
        if not any(timestamp(manifest["start"]) < end and timestamp(manifest["end"]) >= start
                   for start, end in windows):
            continue
        if manifest.get("version") != 1 or manifest["stepNanoseconds"] != 5_000_000_000:
            raise ValueError("unsupported stream manifest version or step")
        begin, end = timestamp(manifest["start"]), timestamp(manifest["end"])
        for index, (start, stop) in enumerate(windows):
            grids[index].update(round(begin + n * 5, 3) for n in range(round((end - begin) / 5) + 1)
                                if start <= begin + n * 5 < stop)
        for query in manifest["queries"]:
            name = query["name"]
            if name in expressions and expressions[name] != query["expr"]:
                raise ValueError(f"query expression changed: {name}")
            expressions[name] = query["expr"]
            raw = evidence.read(path.parent / query["file"], query["sha256"], parse=name in wanted)
            audit.append({"manifest": str(path.relative_to(evidence.root)), "query": name,
                          "status": query["status"], "warnings": len(query.get("warnings", []))})
            if name not in wanted:
                continue
            if raw.get("status") != "success" or raw.get("data", {}).get("resultType") != "matrix":
                continue
            for item in raw["data"]["result"]:
                labels = item["metric"]
                if labels.get("namespace") != namespace and name not in ("up", "scrape-time"):
                    continue
                key = labels_key(labels)
                entry = series[name].setdefault(key, {"labels": dict(key), "points": {}})
                for at, value in item["values"]:
                    if not any(start <= at < stop for start, stop in windows):
                        continue
                    value = float(value)
                    if not math.isfinite(value):
                        continue
                    if at in entry["points"] and entry["points"][at] != value:
                        raise ValueError(f"conflicting overlapping raw samples: {name}")
                    entry["points"][at] = value
    if "container-cpu-rate" in expressions and "[1m]" not in expressions["container-cpu-rate"]:
        raise ValueError("CPU rate is not a one-minute rate")
    return series, grids, audit


def counter_intervals(points, source_times, start, end, freshness):
    samples = {}
    rejected = 0
    for at, value in sorted(points.items()):
        source = source_times.get(at)
        if source is None or not start <= source < end or not 0 <= at - source <= freshness:
            rejected += 1
            continue
        if source in samples and samples[source] != value:
            raise ValueError("different counter values at one source timestamp")
        samples[source] = value
    intervals, resets, gaps = [], 0, 0
    ordered = sorted(samples.items())
    for (before, previous), (at, value) in zip(ordered, ordered[1:]):
        if value < previous:
            resets += 1
        elif at - before > freshness:
            gaps += 1
        else:
            intervals.append({"start": before, "end": at, "cores": (value - previous) / (at - before)})
    return intervals, {"distinctSourceSamples": len(samples), "resets": resets,
                       "longGaps": gaps, "rejectedEvaluationSamples": rejected,
                       "coveredSeconds": sum(p["end"] - p["start"] for p in intervals),
                       "intervalDurationSeconds": stats([p["end"] - p["start"] for p in intervals]),
                       "peakInterval": max(intervals, key=lambda p: p["cores"]) if intervals else None,
                       "firstSource": ordered[0][0] if ordered else None,
                       "lastSource": ordered[-1][0] if ordered else None}


def match_series(labels, instance, cgroup):
    if (labels.get("namespace"), labels.get("pod"), labels.get("container")) != (
            instance["namespace"], instance["pod"], instance["container"]):
        return False
    if cgroup:
        cid = (instance["containerID"] or "").split("://")[-1]
        uid = instance["podUID"]
        if not cid or cid != labels.get("name") or not any(u in labels.get("id", "") for u in (uid, uid.replace("-", "_"))):
            return False
        if labels.get("node", labels.get("instance")) != instance["node"]:
            return False
    return True


def measure(instance, streams, grids, bounds, freshness):
    results, values = [], defaultdict(list)
    baseline = instance["config"]["resources"]["requests"]
    for wi, (start, end) in enumerate(bounds):
        result = {"window": WINDOWS[wi]["name"], "metrics": {}}
        for query, metric in METRICS.items():
            si = 0 if query.startswith("container-") else 1
            series = streams[si]
            matched = [(key, entry) for key, entry in series.get(query, {}).items()
                       if match_series(entry["labels"], instance, si == 0)]
            samples, source_samples, ages, slots = {}, set(), [], set()
            boundary_slots, stale_slots = set(), set()
            verified = query in TIMESTAMPS
            # Rates must have their entire trailing minute inside the quiet window.
            expected = {at for at in grids[si][wi] if query != "container-cpu-rate" or at >= start + 60}
            ambiguous = set()
            for key, entry in matched:
                times = series.get(TIMESTAMPS.get(query), {}).get(key, {}).get("points", {})
                for at, value in entry["points"].items():
                    if at not in expected or value < 0:
                        continue
                    source = times.get(at)
                    if verified:
                        if source is None or not 0 <= at - source <= freshness:
                            stale_slots.add(at)
                            continue
                        if not start <= source < end:
                            boundary_slots.add(at)
                            continue
                        ages.append(at - source)
                        source_samples.add((key, source))
                    if at in samples:
                        ambiguous.add(at)
                    samples[at] = value
                    slots.add(at)
            for at in ambiguous:
                samples.pop(at)
                slots.discard(at)
            request = baseline.get("cpu" if query == "container-cpu-rate" else "memory")
            stat = stats(samples.values(), float(request) if request is not None else None)
            stat.update({"expectedSlots": len(expected), "coveredSlots": len(slots),
                         "slotCoverage": len(slots) / len(expected) if expected else None,
                         "freshnessVerified": verified, "distinctSourceSamples": len(source_samples) if verified else None,
                         "maxSourceAgeSeconds": max(ages) if ages else None,
                         "ambiguousSlotsRejected": len(ambiguous),
                         "preWindowSourceSlotsExcluded": len(boundary_slots),
                         "staleOrMissingTimestampSlots": len(stale_slots),
                         "missingSeriesSlots": len(expected - slots - boundary_slots - stale_slots),
                         "freshnessNote": None if verified else "no metric-specific timestamp query; descriptive only"})
            result["metrics"][metric] = stat
            values[metric].extend(samples.values())
        all_intervals, interval_audits = [], []
        series = streams[0]
        for key, entry in series.get("container-cpu-total", {}).items():
            if not match_series(entry["labels"], instance, True):
                continue
            times = series.get("container-cpu-time", {}).get(key, {}).get("points", {})
            intervals, audit = counter_intervals({t: v for t, v in entry["points"].items() if start <= t < end},
                                                 times, start, end, freshness)
            all_intervals.extend(intervals)
            interval_audits.append(audit)
        # Multiple exporters of the same cgroup are not independent replicas.
        if len(interval_audits) > 1:
            raise ValueError(f"ambiguous counter exporters: {instance['id']}")
        interval_values = [p["cores"] for p in all_intervals]
        result["metrics"]["cpuDistinctIntervalCores"] = stats(interval_values, float(baseline["cpu"]) if "cpu" in baseline else None)
        result["metrics"]["cpuDistinctIntervalCores"]["sourceAudit"] = interval_audits
        values["cpuDistinctIntervalCores"].extend(interval_values)
        results.append(result)
    return results, values


def derive(root, freshness=30):
    evidence = Evidence(root)
    scan = root / "_artifacts/perf-resource-locked-scan"
    source = scan / "probe-tolerance-v2"
    snapshots, inventories, guest_inventories = [], [], []
    for relative in SNAPSHOTS:
        directory = scan / relative
        identity = evidence.read(directory / "identity.json")
        if identity.get("version") != "v1":
            raise ValueError("unsupported snapshot identity version")
        config = evidence.read(directory / "sizing-configuration.json")
        sizing_spec = config["spec"]
        policies = evidence.read(directory / "resource-policy.json")
        if digest(config["spec"]) != identity["configSHA256"]:
            raise ValueError("configuration provenance hash mismatch")
        if any(digest(policies[k]) != h for k, h in identity["policySHA256"].items()):
            raise ValueError("policy provenance hash mismatch")
        lock = evidence.read(directory / "resource-lock-v2.json")
        lockid = evidence.read(directory / "resource-identity-v2.json")
        if lockid["version"] != "resource-policy-v2" or digest(lock) != lockid["sha256"]:
            raise ValueError("resource inventory provenance hash mismatch")
        pods = evidence.read(directory / "full-pods.json")["items"]
        workloads = evidence.read(directory / "full-workloads.json")["items"]
        normalized = evidence.read(directory / "actual-pods.json")
        inventories.append(inventory(pods, workloads, normalized))
        guest_inventories.append(evidence.read(directory / "guest-inventory.json"))
        for filename in ("health.json", "workload-templates.json"):
            evidence.read(directory / filename)
        snapshots.append({"path": str(directory.relative_to(root)), "identity": identity})
    active = stable_active(inventories)
    if len(active) != 145:
        raise ValueError(f"expected 145 active container instances, found {len(active)}")
    if len({s["identity"]["fingerprint"] for s in snapshots}) != 1:
        raise ValueError("snapshot treatment fingerprint changed")
    accepted = evidence.read(source / "accepted-baseline/accepted-treatment.json")
    if snapshots[0]["identity"]["fingerprint"] != accepted["fingerprint"]:
        raise ValueError("snapshot is not the accepted treatment")
    evidence.read(source / "review/summary.json")
    evidence.read(source / "supervisor-result.json")
    evidence.read(source / "HANDOFF.md", parse=False)
    final_digest = "sha256:74166e56e68bd6c3d94d0ba8bd4dcda5073b08e66521c90782220661c91e7be0"
    for item in active.values():
        if "control-plane-operator-perf:probe-20260910" in item["config"]["image"] and final_digest not in (item["imageID"] or ""):
            raise ValueError("final treatment runtime image digest mismatch")
    rows, instances = inventories[-1]
    bounds = [(timestamp(w["start"]), timestamp(w["end"])) for w in WINDOWS]
    window_provenance = []
    for wi, (start, end) in enumerate(bounds):
        chosen = snapshots[wi * 2:wi * 2 + 2]
        guest = guest_inventories[wi * 2 + 1]
        application_pods = [p for p in guest if p["kind"] == "Pod"
                            and p["metadata"].get("namespace", "").startswith("perf-")
                            and p.get("phase") not in ("Succeeded", "Failed")]
        if len(application_pods) != WINDOWS[wi]["applicationPods"]:
            raise ValueError("saved guest inventory does not match quiet-window population")
        window_provenance.append({"window": WINDOWS[wi]["name"],
                                 "snapshotPaths": [s["path"] for s in chosen],
                                 "snapshotOffsetsFromStartSeconds": [timestamp(s["identity"]["capturedAt"]) - start for s in chosen],
                                 "snapshotOffsetsFromEndSeconds": [timestamp(s["identity"]["capturedAt"]) - end for s in chosen],
                                 "applicationPodsInNearestEndSnapshot": len(application_pods),
                                 "populationIsSnapshotEvidenceNotContinuousProof": True})
    namespace = next(iter(active.values()))["namespace"]
    loaded = [load_stream(evidence, source / d, bounds, namespace)
              for d in ("stream-node-queries", "stream-queries")]
    streams, grids = [s[0] for s in loaded], [s[1] for s in loaded]
    for grid in grids:
        for wi, (start, end) in enumerate(bounds):
            if len(grid[wi]) != (end - start) / 5:
                raise ValueError("quiet window manifest grid has gaps or overlapping offsets")
    observed, grouped = [], defaultdict(lambda: defaultdict(list))
    blocked = defaultdict(set)
    for iid, item in sorted(active.items()):
        windows, values = measure(item, streams, grids, bounds, freshness)
        observed.append({**{k: v for k, v in item.items() if k != "row"}, "selector": rows[item["row"]]["selector"],
                         "windows": windows})
        for metric, samples in values.items():
            grouped[item["row"]][metric].extend(samples)
        for window in windows:
            for metric in ("chargeBytes", "cpuRateCores"):
                metric_stats = window["metrics"][metric]
                if metric_stats["coveredSlots"] + metric_stats["preWindowSourceSlotsExcluded"] != metric_stats["expectedSlots"]:
                    blocked[item["row"]].add(f"{window['window']}: {metric} missing/stale slots")
            audits = window["metrics"]["cpuDistinctIntervalCores"]["sourceAudit"]
            if not audits or any(a["resets"] or a["longGaps"] or a["distinctSourceSamples"] < 2 for a in audits):
                blocked[item["row"]].add(f"{window['window']}: counter coverage/reset gate")
    budgets, totals = [], {"baselineCPUCores": Decimal(0), "baselineMemoryBytes": Decimal(0),
                          "candidateCPUCores": Decimal(0), "candidateMemoryBytes": Decimal(0),
                          "candidateMemoryLimitBytes": Decimal(0), "activeInstances": len(active)}
    for key, row in sorted(rows.items()):
        config = container_config(row["template"])
        requests = config["resources"]["requests"]
        cpu, memory = quantity(requests.get("cpu")), quantity(requests.get("memory"))
        summary = {metric: stats(grouped[key][metric], float(cpu if metric.startswith("cpu") else memory)
                                if (cpu if metric.startswith("cpu") else memory) is not None else None)
                   for metric in list(METRICS.values()) + ["cpuDistinctIntervalCores"]}
        peaks = [summary[m]["max"] for m in ("cpuRateCores", "cpuDistinctIntervalCores") if summary[m]["max"] is not None]
        pcpu = proposed(cpu, max(peaks) if peaks else None, "1.2", ".01")
        pmemory = proposed(memory, summary["chargeBytes"]["max"], "1.1", str(16 * MIB))
        limit = proposed(None, pmemory, "1.1", str(MIB)) if pmemory is not None else None
        count = sum(i in active for i in row["instances"])
        gates = sorted(blocked[key])
        if not count:
            gates.append("no running replica in quiet evidence; startup/job/disabled-workload measurement required")
        if key[3] == "initContainers":
            gates.append("constrained init/startup gate required, including restartable-init startup")
        if cpu is None or memory is None:
            gates.append("baseline request absent; do not substitute zero or a default")
        if not row["policyManaged"]:
            gates.append("downstream/instrumentation override required; sizing policy alone does not cover this template")
        gomem = next((e for e in config["env"] if e["name"] == "GOMEMLIMIT"), None)
        go_observed = summary["goHeapAllocBytes"]["samples"] > 0
        go_mode = "existing-target" if gomem else "observed-go-runtime-needs-explicit-env" if go_observed else "runtime-unverified-no-env-inferred"
        target = int(pmemory * Decimal(".8")) if pmemory is not None else None
        budget = {"selector": row["selector"], "workloadUID": row["workloadUID"], "podSelector": row["podSelector"],
                  "policyManaged": row["policyManaged"], "configuredReplicas": row["replicas"],
                  "activeReplicas": count, "instanceIDs": sorted(row["instances"]),
                  "baseline": config, "baselineResourceQuantities": row["template"].get("resources", {}),
                  "coveredQuietEvidence": summary,
                  "goManagedMemoryBytes": None,
                  "requestBaselineMisconfigured": bool(any(summary[m]["fractionAboveRequest"]
                      for m in ("cpuRateCores", "cpuDistinctIntervalCores", "chargeBytes"))),
                  "candidateBasis": {"cpu": "covered peak plus experimental headroom" if peaks else "baseline retained; unobserved",
                                     "memory": "covered charge peak plus experimental headroom" if summary["chargeBytes"]["samples"] else "baseline retained; unobserved"},
                  "candidate": {"requests": {"cpu": f"{pcpu * 1000:f}m" if pcpu is not None else None,
                                             "memory": f"{pmemory / MIB:g}Mi" if pmemory is not None else None},
                                "limits": {"cpu": f"{pcpu * 1000:f}m" if pcpu is not None else None,
                                           "memory": f"{limit / MIB:g}Mi" if limit is not None else None},
                                "expectedGOMEMLIMITBytesAt80PercentRequest": target,
                                "goTargetApplicability": go_mode,
                                "GOMEMLIMIT": str(target) if (gomem or go_observed) and target is not None else None},
                  "gates": gates, "qualified": False}
        budgets.append(budget)
        if count:
            for field, value in (("baselineCPUCores", cpu), ("baselineMemoryBytes", memory),
                                 ("candidateCPUCores", pcpu), ("candidateMemoryBytes", pmemory),
                                 ("candidateMemoryLimitBytes", limit)):
                if value is None:
                    raise ValueError("cannot sum absent active budget")
                totals[field] += value * count
    for key, value in totals.items():
        if isinstance(value, Decimal):
            totals[key] = float(value)
    # Include the analyzer itself in the replay input manifest.
    evidence.read(Path(__file__), parse=False)
    for name in ("test_offline_budgets.py", "OFFLINE-BUDGETS.md"):
        evidence.read(Path(__file__).with_name(name), parse=False)
    report = {"schemaVersion": VERSION, "qualified": False, "offlineOnly": True,
              "windows": WINDOWS, "snapshots": snapshots, "finalTreatmentImageDigest": final_digest,
              "windowProvenance": window_provenance,
              "configBaseline": {"sizingSpec": sizing_spec, "observedPolicies": policies},
              "freshnessSeconds": freshness, "headroom": {"cpu": 1.2, "cpuRoundingMillicores": 10,
                  "memoryCharge": 1.1, "memoryRoundingMiB": 16, "memoryLimitToRequest": 1.1,
                  "memoryLimitRoundingMiB": 1, "goTargetToRequest": .8, "experimentalNotProven": True},
              "totals": totals, "budgets": budgets,
              "requiredGates": ["fresh constrained startup including all init containers and jobs",
                  "re-read every actual downstream/regular/init request, CPU limit, memory limit and Go target",
                  "constrained population and object-count progression", "constrained QPS progression",
                  "one-hour confirmation at accepted population and QPS, with complete fresh coverage",
                  "no OOM, restart churn, sustained non-Ready, unacceptable throttling, latency or errors",
                  "every workload stays within requests/limits; resolve all downstream exceptions before qualification"],
              "limitations": ["Quiet peaks are observations, not proven requirements or safe startup budgets.",
                  "No request decreases. Missing observations remain null; retained baselines are not measured proposals.",
                  "CPU uses max of fresh 1m rates and distinct source counter intervals; first 60s of rates excluded.",
                  "Counter resets and intervals longer than freshness threshold are excluded, not repaired or zeroed.",
                  "Memory budget uses cgroup charge, not RSS, working set, Go heap or Go-managed memory.",
                  "RSS/working-set/Go heap lack metric-specific timestamp queries here; descriptive, not freshness-certified.",
                  "Go-managed memory (Sys minus HeapReleased) is unobserved in these streams; heap is not a substitute.",
                  "GOMEMLIMIT is a soft Go runtime target at 80% of request, not of the hard memory limit.",
                  "OOM can reflect cgroup charge, page cache, native allocations, subprocesses or startup; it is not a specific Go failure.",
                  "Sums count actual running regular and restartable-init replicas once; not scheduler-effective init maxima or Pod overhead.",
                  "Snapshot agreement is sampled provenance, not continuous configuration proof."]}
    report["coverageSummary"] = []
    for wi, window in enumerate(WINDOWS):
        entries = [o["windows"][wi]["metrics"] for o in observed]
        report["coverageSummary"].append({"window": window["name"], "instances": len(entries),
            "metrics": {metric: {
                "expectedSlots": sum(e[metric]["expectedSlots"] for e in entries),
                "coveredSlots": sum(e[metric]["coveredSlots"] for e in entries),
                "preWindowSourceSlotsExcluded": sum(e[metric]["preWindowSourceSlotsExcluded"] for e in entries),
                "staleOrMissingTimestampSlots": sum(e[metric]["staleOrMissingTimestampSlots"] for e in entries),
                "missingSeriesSlots": sum(e[metric]["missingSeriesSlots"] for e in entries),
                "minimumReplicaSlotCoverage": min(e[metric]["slotCoverage"] for e in entries),
                "maximumSourceAgeSeconds": max((e[metric]["maxSourceAgeSeconds"] for e in entries
                                                if e[metric]["maxSourceAgeSeconds"] is not None), default=None),
            } for metric in ("cpuRateCores", "chargeBytes")}})
    hashes = {"schemaVersion": VERSION, "files": dict(sorted(evidence.files.items())),
              "integrityScope": "All raw files in overlapping chunks verified against retained manifests. Other inputs hashed for replay; canonical config, policy and resource-lock hashes checked. Hashes are not signatures.",
              "queryAudit": [q for s in loaded for q in s[2]]}
    return report, {"schemaVersion": VERSION, "instances": observed}, hashes


def markdown(report):
    lines = ["# Small Constrained V1 Offline Candidate", "", "Experimental candidate only; not qualified and not applied.", "",
             "## Totals", "", "```json", json.dumps(report["totals"], indent=2), "```", "",
             "## Coverage", "", "```json", json.dumps(report["coverageSummary"], indent=2), "```", "",
             "Fresh source samples before the quiet boundary are excluded, not mislabeled as stale or filled with zero.",
             "The 30-second freshness ceiling is an explicit analysis gate, not proof that five-second evaluations are independent scrapes.", "",
             "## Workload/Container Table", "",
             "CPU is millicores; memory is MiB. Stats are max / p95 / median / fraction above baseline request.",
             "CPU interval and 1m statistics are separate. Samples pool covered replica observations; proposals use the maximum, never their sum.",
             "Unknown is not zero. Full per-replica/window freshness and coverage are in observations.json.", "",
             "| Workload | Group/Container | Replicas | Baseline CPU/Mem | CPU 1m | CPU intervals | Charge | Candidate CPU/Mem | Gates |",
             "| --- | --- | ---: | --- | --- | --- | --- | --- | --- |"]
    def fmt(stat, scale):
        return " / ".join("unknown" if stat[k] is None else f"{stat[k] * (1 if k == 'fractionAboveRequest' else scale):.4g}"
                          for k in ("max", "p95", "median", "fractionAboveRequest"))
    for b in report["budgets"]:
        s, r, c, e = b["selector"], b["baseline"]["resources"]["requests"], b["candidate"]["requests"], b["coveredQuietEvidence"]
        base = " / ".join("unknown" if r.get(k) is None else f"{float(r[k]) * scale:g}" for k, scale in (("cpu", 1000), ("memory", 1 / MIB)))
        lines.append(f"| {s['kind']}/{s['workload']} | {s['containerGroup']}/{s['container']} | {b['activeReplicas']} | {base} | {fmt(e['cpuRateCores'], 1000)} | {fmt(e['cpuDistinctIntervalCores'], 1000)} | {fmt(e['chargeBytes'], 1 / MIB)} | {c['cpu']} / {c['memory']} | {'; '.join(b['gates']) or 'confirmation required'} |")
    lines += ["", "## Downstream Exceptions", "", "These exact selectors need controller-owned configuration/override work; do not patch generated Pods.", ""]
    lines += [f"- `{b['selector']['kind']}/{b['selector']['workload']} {b['selector']['containerGroup']}/{b['selector']['container']}`"
              for b in report["budgets"] if not b["policyManaged"]]
    lines += ["", "## Required Gates", ""] + [f"- {g}" for g in report["requiredGates"]]
    lines += ["", "## Interpretation", ""] + [f"- {g}" for g in report["limitations"]]
    return "\n".join(lines) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[3])
    parser.add_argument("--output", type=Path, required=True, help="existing private, empty directory")
    parser.add_argument("--freshness-seconds", type=float, default=30)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--refresh", action="store_true", help="replace only this tool's four existing generated outputs")
    mode.add_argument("--verify-output", action="store_true", help="recompute and byte-compare without writing")
    args = parser.parse_args()
    root, output = args.root.resolve(), args.output.resolve()
    if not output.is_relative_to(root / "_artifacts/small-constrained-v1/analysis"):
        parser.error("output must be within the authorized private analysis directory")
    if not output.is_dir() or output.stat().st_mode & 0o077:
        parser.error("output must exist and be private (0700)")
    filenames = {"candidate-budgets.json", "observations.json", "input-hashes.json", "README.md"}
    existing = {p.name for p in output.iterdir()}
    if args.refresh or args.verify_output:
        if not filenames <= existing or any((output / name).is_symlink() for name in filenames):
            parser.error("refresh/verification requires this tool's existing regular output files")
        if json.loads((output / "candidate-budgets.json").read_text()).get("schemaVersion") != VERSION:
            parser.error("refusing to replace an unknown output version")
    elif existing:
        parser.error("output must be empty unless --refresh or --verify-output is specified")
    if args.freshness_seconds <= 0 or args.freshness_seconds > 60:
        parser.error("freshness must be >0 and <=60 seconds")
    os.umask(0o077)
    report, observations, hashes = derive(root, args.freshness_seconds)
    outputs = {filename: json.dumps(value, sort_keys=True, indent=2, allow_nan=False) + "\n"
               for filename, value in (("candidate-budgets.json", report), ("observations.json", observations), ("input-hashes.json", hashes))}
    outputs["README.md"] = markdown(report)
    for filename, text in outputs.items():
        if args.verify_output:
            if (output / filename).read_bytes() != text.encode():
                raise ValueError(f"offline replay differs: {filename}")
        else:
            with (output / filename).open("w" if args.refresh else "x") as stream:
                stream.write(text)
    print(json.dumps({"output": str(output), "totals": report["totals"], "budgetRows": len(report["budgets"]),
                      "inputFiles": len(hashes["files"]), "replayVerified": args.verify_output, "qualified": False}, indent=2))


if __name__ == "__main__":
    main()
