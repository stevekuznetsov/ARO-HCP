#!/usr/bin/env python3
"""Read-only diagnosis of a frozen offline candidate; JSON to stdout, no writes."""

import argparse
from collections import defaultdict
from decimal import Decimal
import hashlib
import json
from pathlib import Path

import offline_budgets as b


def alternatives(budget, instances):
    """Diagnostic floors, not replacement candidates or qualified CPU limits."""
    base = b.quantity(budget["baseline"]["resources"]["requests"].get("cpu"))
    memory = b.quantity(budget["baseline"]["resources"]["requests"].get("memory"))
    evidence = budget["coveredQuietEvidence"]
    peaks = [evidence[m]["max"] for m in ("cpuRateCores", "cpuDistinctIntervalCores")
             if evidence[m]["max"] is not None]
    peak = max(peaks) if peaks else None
    result = {
        "strictObservedCPU1mQuantum": b.proposed(base, peak, "1", ".001"),
        "strictObservedCPU10mQuantum": b.proposed(base, peak, "1", ".01"),
        "strictObservedMemory1MiQuantum": b.proposed(memory, evidence["chargeBytes"]["max"], "1", str(b.MIB)),
        "strictObservedMemory16MiQuantum": b.proposed(memory, evidence["chargeBytes"]["max"], "1", str(16 * b.MIB)),
    }
    for statistic in ("median", "p95", "max"):
        values = [w["metrics"]["cpuRateCores"][statistic] for i in instances for w in i["windows"]
                  if w["metrics"]["cpuRateCores"][statistic] is not None]
        result[f"oneMinute{statistic.title()}CPU10mQuantum"] = b.proposed(base, max(values) if values else None, "1", ".01")
    return result


def diagnose(root, analysis):
    filenames = ("candidate-budgets.json", "observations.json", "input-hashes.json")
    data = {name: json.loads((analysis / name).read_bytes()) for name in filenames}
    report, observations, hashes = [data[name] for name in filenames]
    if any(value["schemaVersion"] != b.VERSION for value in data.values()):
        raise ValueError("unsupported frozen analysis version")
    evidence = b.Evidence(root)
    for relative, item in hashes["files"].items():
        evidence.read(root / relative, item["sha256"], parse=False)
    frozen = {name: hashlib.sha256((analysis / name).read_bytes()).hexdigest() for name in filenames}
    by_selector = defaultdict(list)
    for item in observations["instances"]:
        by_selector[b.canonical(item["selector"])].append(item)
    totals, rows = defaultdict(Decimal), []
    for budget in report["budgets"]:
        instances = by_selector[b.canonical(budget["selector"])]
        count = budget["activeReplicas"]
        if count != len(instances):
            raise ValueError("replica inventory mismatch")
        proposed = alternatives(budget, instances)
        for key, value in proposed.items():
            if count and value is None:
                raise ValueError("missing active diagnostic request")
            if count:
                totals[key] += value * count
        base = budget["baseline"]["resources"]["requests"]
        cpu_delta = ((b.quantity(budget["candidate"]["requests"]["cpu"]) - b.quantity(base["cpu"])) * count
                     if count else Decimal(0))
        mem_delta = ((b.quantity(budget["candidate"]["requests"]["memory"]) - b.quantity(base["memory"])) * count
                     if count else Decimal(0))
        windows = []
        for wi, window in enumerate(report["windows"]):
            metrics = [i["windows"][wi]["metrics"] for i in instances]
            audits = [a for m in metrics for a in m["cpuDistinctIntervalCores"]["sourceAudit"]]
            peaks = [a["peakInterval"] for a in audits if a["peakInterval"]]
            windows.append({"window": window["name"],
                "maxReplicaOneMinuteMedian": max((m["cpuRateCores"]["median"] for m in metrics), default=None),
                "maxReplicaOneMinuteP95": max((m["cpuRateCores"]["p95"] for m in metrics), default=None),
                "oneMinutePeak": max((m["cpuRateCores"]["max"] for m in metrics), default=None),
                "peakCounterInterval": max(peaks, key=lambda p: p["cores"]) if peaks else None})
        rows.append({"selector": budget["selector"], "replicas": count,
                     "baseline": budget["baselineResourceQuantities"], "candidate": budget["candidate"],
                     "cpuIncreaseCores": float(cpu_delta), "memoryIncreaseMiB": float(mem_delta / b.MIB),
                     "alternatives": {k: float(v) if v is not None else None for k, v in proposed.items()},
                     "pooledStats": budget["coveredQuietEvidence"], "windows": windows})
    bounds = [(b.timestamp(w["start"]), b.timestamp(w["end"])) for w in report["windows"]]
    source = root / "_artifacts/perf-resource-locked-scan/probe-tolerance-v2/stream-node-queries"
    namespace = observations["instances"][0]["namespace"]
    streams, grids, _ = b.load_stream(evidence, source, bounds, namespace)
    concurrent, cpo_minutes = [], []
    for wi, (start, end) in enumerate(bounds):
        expected = {at for at in grids[wi] if at >= start + 60}
        slots = defaultdict(dict)
        for item in observations["instances"]:
            for key, series in streams["container-cpu-rate"].items():
                if not b.match_series(series["labels"], item, True):
                    continue
                times = streams["container-cpu-time"].get(key, {}).get("points", {})
                for at, value in series["points"].items():
                    source_time = times.get(at)
                    if at not in expected or source_time is None or not 0 <= at - source_time <= report["freshnessSeconds"]:
                        continue
                    if item["id"] in slots[at]:
                        raise ValueError("duplicate concurrent metric identity")
                    slots[at][item["id"]] = value
        complete = {at: values for at, values in slots.items() if len(values) == len(observations["instances"])}
        concurrent.append({"window": report["windows"][wi]["name"], "expectedSlots": len(expected),
                           "completeSlots": len(complete), "wholeHCPOneMinuteCores": b.stats([sum(v.values()) for v in complete.values()])})
        cpo = [i for i in observations["instances"] if i["selector"]["workload"] == "control-plane-operator"
               and i["container"] == "control-plane-operator"]
        if len(cpo) != 1:
            raise ValueError("ambiguous CPO instance")
        minutes = defaultdict(list)
        for at in sorted(complete):
            minutes[int((at - start) // 60)].append(complete[at][cpo[0]["id"]])
        cpo_minutes.append({"window": report["windows"][wi]["name"], "minuteAfterStart": {
            str(m): b.stats(v) for m, v in sorted(minutes.items())}})
    # Verify the readonly diagnostic never rewrote the frozen artifacts.
    if frozen != {name: hashlib.sha256((analysis / name).read_bytes()).hexdigest() for name in filenames}:
        raise ValueError("frozen analysis changed during diagnosis")
    return {"schemaVersion": "hcp-perf.offline-budget-diagnosis/v1", "candidateUnchanged": True,
            "frozenOutputHashes": frozen, "frozenInputsReverified": len(hashes["files"]),
            "rawFilesVerifiedAgainstManifests": sum(i["manifestHashVerified"] for i in hashes["files"].values()),
            "baselineAndCandidateTotals": report["totals"], "diagnosticTotals": {k: float(v) for k, v in totals.items()},
            "concurrentCPU": concurrent, "cpoMinuteTrend": cpo_minutes,
            "rows": sorted(rows, key=lambda r: (-r["cpuIncreaseCores"], b.canonical(r["selector"]))),
            "interpretation": ["Alternatives are not applied, not selected, and not safe-limit recommendations.",
                "Strict correction has no discretionary headroom; 1m CPU / 1Mi memory quantization is explicit.",
                "Same-quantum strict columns separate headroom from rounding effects.",
                "Sustained estimates use maximum per-replica/window one-minute median or p95, floor at existing requests, then round increases to 10m.",
                "P95 and median exclude bursts and cannot establish safe CPU limits or startup budgets.",
                "The sum of replicated worst-case requests is not simultaneous observed consumption.",
                "Saved query set has no reconcile counts/durations, workqueue metrics or CPU profile; CPU shape alone cannot prove a reconcile loop."]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[3])
    args = parser.parse_args()
    root = args.root.resolve()
    result = diagnose(root, root / "_artifacts/small-constrained-v1/analysis")
    print(json.dumps(result, sort_keys=True, indent=2, allow_nan=False))


if __name__ == "__main__":
    main()
