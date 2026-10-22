#!/usr/bin/env python3
"""Reproduce the direct-projector candidate, without live access or frozen edits."""

import argparse
from collections import defaultdict
from copy import deepcopy
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import subprocess

import offline_budgets as b


FROZEN = {
    "candidate-budgets.json": "c0617bddb79fa78d2c652895dc28ffa75dc041237523f3990670dfad13b375a8",
    "observations.json": "abea0bf189f9b941e68601b2d16d2f2726e26d19aed03b754ba6c6b119d64558",
    "input-hashes.json": "a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e",
}
V2 = "control-plane-operator/controllers/hostedcontrolplane/v2/"
# Exact component registrations, not the snapshot's policy-hash heuristic.
DIRECT = {
    "azure-cloud-controller-manager": "cloud_controller_manager/azure",
    "capi-provider": "capi_provider",
    "catalog-operator": "olm/catalog_operator",
    "certified-operators-catalog": "olm/catalogs",
    "cluster-api": "capi_manager",
    "cluster-autoscaler": "autoscaler",
    "cluster-image-registry-operator": "registryoperator",
    "cluster-network-operator": "cno",
    "cluster-node-tuning-operator": "nto",
    "cluster-policy-controller": "clusterpolicy",
    "cluster-storage-operator": "storage",
    "cluster-version-operator": "cvo",
    "community-operators-catalog": "olm/catalogs",
    "control-plane-operator": "controlplaneoperator",
    "control-plane-pki-operator": "pkioperator",
    "csi-snapshot-controller-operator": "snapshotcontroller",
    "dns-operator": "dnsoperator",
    "etcd": "etcd",
    "featuregate-generator": "fg",
    "hosted-cluster-config-operator": "configoperator",
    "ignition-server": "ignitionserver",
    "ignition-server-proxy": "ignitionserver_proxy",
    "ingress-operator": "ingressoperator",
    "konnectivity-agent": "konnectivity_agent",
    "kube-apiserver": "kas",
    "kube-controller-manager": "kcm",
    "kube-scheduler": "kube_scheduler",
    "kube-storage-version-migrator": "kube_storage_version_migrator",
    "machine-approver": "machine_approver",
    "olm-collect-profiles": "olm/collect_profiles",
    "olm-operator": "olm/olm_operator",
    "openshift-apiserver": "oapi",
    "openshift-controller-manager": "ocm",
    "openshift-route-controller-manager": "routecm",
    "packageserver": "olm/packageserver",
    "redhat-operators-catalog": "olm/catalogs",
    "router": "router",
}
DOWNSTREAM = {
    "azure-disk-csi-driver-controller", "azure-disk-csi-driver-operator",
    "azure-file-csi-driver-controller", "azure-file-csi-driver-operator",
    "cloud-network-config-controller", "csi-snapshot-controller",
    "multus-admission-controller", "network-node-identity", "ovnkube-control-plane",
}
PROFILE = "tooling/hcp-perf/examples/small-constrained-v1.json"
OUTPUT = "_artifacts/small-constrained-v1/scoped-analysis"
SCHEMA = "cmd/install/assets/crds/hypershift-operator/scheduling.hypershift.openshift.io_clustersizingconfigurations.yaml"


def encoded(value):
    return (json.dumps(value, sort_keys=True, indent=2, allow_nan=False) + "\n").encode()


def derive(root, hypershift):
    hashes = {}

    def read(path, label, expected=None):
        data = path.read_bytes()
        digest = hashlib.sha256(data).hexdigest()
        if expected is not None and digest != expected:
            raise ValueError(f"source hash mismatch: {label}")
        hashes[label] = digest
        return data

    frozen = root / "_artifacts/small-constrained-v1/analysis"
    data = {name: json.loads(read(frozen / name, f"perf/{(frozen / name).relative_to(root)}", digest))
            for name, digest in FROZEN.items()}
    report, observations, inputs = (data[name] for name in FROZEN)
    for relative, item in inputs["files"].items():
        read(root / relative, f"perf/{relative}", item["sha256"])
    for relative in (
        "tooling/hcp-perf/scripts/scoped_offline_budgets.py",
        "tooling/hcp-perf/scripts/test_scoped_offline_budgets.py",
        "tooling/hcp-perf/internal/sizing/sizing.go",
        "tooling/hcp-perf/internal/sizing/hypershift.go",
        "tooling/hcp-perf/internal/sizing/small_constrained_test.go",
        "tooling/hcp-perf/main.go", "tooling/hcp-perf/json.go",
        "tooling/hcp-perf/go.mod", "tooling/hcp-perf/go.sum",
    ):
        read(root / relative, f"perf/{relative}")
    sources = {}
    for workload, directory in DIRECT.items():
        relative = V2 + directory + "/component.go"
        text = read(hypershift / relative, f"hypershift/{relative}").decode()
        if f'"{workload}"' not in text or "component.New" not in text:
            raise ValueError(f"direct component mapping no longer verified: {workload}")
        sources[workload] = relative
    for relative in (
        SCHEMA, "api/scheduling/v1alpha1/clustersizingconfiguration_types.go",
        "support/controlplane-component/container-resource-policy.go",
        "support/controlplane-component/defaults.go",
        "support/controlplane-component/controlplane-component.go",
        "support/controlplane-component/cronjob.go",
        "hypershift-operator/controllers/hostedcluster/hostedcluster_controller.go",
        "hypershift-operator/controllers/hostedcluster/container_resource_policy.go",
        "hypershift-operator/controllers/hostedcluster/etcd_recovery.go",
        "control-plane-operator/controllers/hostedcontrolplane/hostedcontrolplane_controller.go",
    ):
        read(hypershift / relative, f"hypershift/{relative}")

    policy = report["configBaseline"]["observedPolicies"]["hcp"]
    if policy != report["configBaseline"]["observedPolicies"]["hc"]:
        raise ValueError("baseline policy mismatch")
    if policy["defaultRequests"] != {"cpu": "10m", "memory": "64Mi"} or policy["goMemoryLimitPercent"] != 80:
        raise ValueError("unexpected baseline fallback")
    exceptions = {(c["workload"], c["container"]): c["goMemoryLimitPercent"]
                  for c in policy["containers"] if "goMemoryLimitPercent" in c}
    if exceptions != {("ignition-server-proxy", "haproxy"): 0, ("router", "router"): 0}:
        raise ValueError("runtime exceptions require review")
    relative = report["snapshots"][-1]["path"] + "/full-workloads.json"
    workloads = json.loads(read(root / relative, f"perf/{relative}"))["items"]
    by_uid = {w["metadata"]["uid"]: w for w in workloads}
    instances = defaultdict(list)
    for item in observations["instances"]:
        instances[b.canonical(item["selector"])].append(item)

    rows, entries, seen = [], [], set()
    totals = {scope: {"rows": 0, "activeInstances": 0, "unobservedRows": 0,
                     "baselineCPUMilli": 0, "candidateCPUMilli": 0,
                     "baselineMemoryMiB": 0, "candidateMemoryMiB": 0,
                     "baselineExplicitCPULimitMilli": 0, "candidateExplicitCPULimitMilli": 0,
                     "baselineExplicitMemoryLimitMiB": 0, "candidateExplicitMemoryLimitMiB": 0,
                     "baselineUnlimitedCPUInstances": 0, "candidateUnlimitedCPUInstances": 0,
                     "baselineUnlimitedMemoryInstances": 0, "candidateUnlimitedMemoryInstances": 0}
              for scope in ("controlled", "downstream", "instrumentation", "allHCP")}
    for old in report["budgets"]:
        selector = old["selector"]
        workload, kind = selector["workload"], selector["kind"]
        mapping_owner = None
        if kind == "Job":
            obj = by_uid[old["workloadUID"]]
            if obj["metadata"]["name"] != workload:
                raise ValueError("Job identity mismatch")
            owners = [o for o in obj["metadata"].get("ownerReferences", []) if o.get("controller")]
            if len(owners) == 1 and owners[0]["kind"] == "CronJob":
                mapping_owner = owners[0]
                workload, kind = mapping_owner["name"], "CronJob"
        scope = ("controlled" if workload in DIRECT else "downstream" if workload in DOWNSTREAM
                 else "instrumentation" if workload == "kube-state-metrics-hcp" else None)
        if scope is None:
            raise ValueError(f"unmapped workload: {selector}")
        identity = {"kind": kind, "workload": workload, "container": selector["container"]}
        if selector["containerGroup"] == "initContainers":
            identity["initContainer"] = True
        active = instances[b.canonical(selector)]
        if len(active) != old["activeReplicas"]:
            raise ValueError("replica inventory mismatch")
        p95s = [w["metrics"]["cpuRateCores"]["p95"] for i in active for w in i["windows"]]
        if active and (len(p95s) != len(active) * len(report["windows"]) or None in p95s):
            raise ValueError("missing replica/window p95")
        p95 = max(p95s) if p95s else None
        charge = old["coveredQuietEvidence"]["chargeBytes"]["max"]
        baseline = deepcopy(old["baselineResourceQuantities"])
        candidate = deepcopy(baseline)
        expected_env = deepcopy(old["baseline"]["env"])
        entry = None
        if scope == "controlled":
            key = workload, selector["container"]
            if key in seen:
                raise ValueError(f"ambiguous projector key across kind/init: {key}")
            seen.add(key)
            base = baseline.get("requests", {})
            if base.get("cpu") is None or base.get("memory") is None:
                raise ValueError(f"unknown controlled baseline: {selector}; no startup budget invented")
            cpu = b.proposed(b.quantity(base["cpu"]), p95, "1.2", ".01") * 1000
            memory = b.proposed(b.quantity(base["memory"]), charge, "1.1", str(16 * b.MIB)) / b.MIB
            if cpu != int(cpu) or memory != int(memory):
                raise ValueError("budget not representable by sizing CLI")
            cpu, memory = int(cpu), int(memory)
            limit = (memory * 110 + 99) // 100
            candidate.setdefault("requests", {}).update(cpu=f"{cpu}m", memory=f"{memory}Mi")
            candidate.setdefault("limits", {}).update(cpu=f"{cpu}m", memory=f"{limit}Mi")
            percent = exceptions.get(key, 80)
            expected_env = [e for e in expected_env if e["name"] != "GOMEMLIMIT"]
            if percent:
                expected_env.append({"name": "GOMEMLIMIT", "value": str(memory * b.MIB * percent // 100)})
            # Explicit entries only where the preserved fallback is insufficient.
            if (cpu, memory) != (10, 64) or key in exceptions:
                unit = key == ("kube-apiserver", "kube-apiserver")
                ratio = {"numerator": int(unit), "denominator": 1}
                entry = {"identity": identity, "cpuRatio": ratio, "memoryRatio": ratio,
                         "floor": {"cpuMilli": cpu, "memoryMiB": memory}}
                if key in exceptions:
                    entry["goMemoryLimitPercent"] = percent
                entries.append(entry)
        row = {"selector": selector, "workloadUID": old["workloadUID"], "scope": scope,
               "annotationHeuristicPolicyManaged": old["policyManaged"],
               "projectorIdentity": identity if scope == "controlled" else None,
               "projectorSource": sources.get(workload), "cronJobOwnerReference": mapping_owner,
               "actualConstrainedCoverageVerified": False, "activeReplicas": len(active),
               "worstReplicaWindowP95CPUCores": p95, "worstChargePeakBytes": charge,
               "startupTested": False, "quietObserved": bool(active),
               "baselineResources": baseline, "candidateResources": candidate,
               "baselineRuntimeEnv": old["baseline"]["env"], "candidateRuntimeEnv": expected_env,
               "profileOverride": entry is not None,
               "basis": "p95 CPU / peak charge with original floors" if active and scope == "controlled"
                        else "baseline retained; startup untested" if scope == "controlled"
                        else "outside enforcement scope; resources and runtime env unchanged"}
        rows.append(row)
        for group in (scope, "allHCP"):
            t = totals[group]
            t["rows"] += 1
            t["activeInstances"] += len(active)
            t["unobservedRows"] += not active
            for label, resources in (("baseline", baseline), ("candidate", candidate)):
                for resource, suffix, scale in (("cpu", "CPUMilli", Decimal(1000)),
                                                ("memory", "MemoryMiB", Decimal(1) / b.MIB)):
                    if not active:
                        continue
                    request = b.quantity(resources["requests"].get(resource))
                    if request is None:
                        raise ValueError("absent active request")
                    t[label + suffix] += int(request * scale) * len(active)
                    limit = b.quantity(resources.get("limits", {}).get(resource))
                    if limit is None:
                        t[label + "Unlimited" + ("CPU" if resource == "cpu" else "Memory") + "Instances"] += len(active)
                    else:
                        t[label + "Explicit" + ("CPULimitMilli" if resource == "cpu" else "MemoryLimitMiB")] += int(limit * scale) * len(active)
    entries.sort(key=lambda e: (e["identity"]["kind"], e["identity"]["workload"], e["identity"]["container"]))
    anchor = next(e["floor"] for e in entries if e["identity"]["workload"] == "kube-apiserver"
                  and e["identity"]["container"] == "kube-apiserver")
    profile = {"version": "v1", "name": "small-constrained-v1",
               "description": "OFFLINE, startup untested. Direct HyperShift projector scope only; downstream and instrumentation unchanged. Worst replica/window 1m CPU p95 x1.2 rounded up 10m; charge peak x1.1 rounded up 16Mi; original requests are floors. CPU limit=request, memory limit=110% rounded up MiB, GOMEMLIMIT=80% of request with existing non-Go exceptions. Preserve 10m/64Mi fallback; no unknown startup budgets inferred.",
               "anchor": anchor, "rounding": {"cpuMilli": 1, "memoryMiB": 1},
               "safetyLimit": {"numerator": 11, "denominator": 10},
               "cpuLimitEqualsRequest": True, "goMemLimitPercent": 80, "containers": entries}
    result = {"schemaVersion": "hcp-perf.scoped-offline-budgets/v1", "offlineOnly": True,
              "qualified": False, "startupTested": False, "frozenOutputHashes": FROZEN,
              "frozenInputsReverified": len(inputs["files"]),
              "rawManifestVerifiedFiles": sum(i["manifestHashVerified"] for i in inputs["files"].values()),
              "windows": report["windows"], "totals": totals, "rows": rows,
              "measurementCoverage": report["coverageSummary"],
              "coverageMeaning": "Source-supported direct projection is not actual admitted-container coverage. Annotation is a separate historical heuristic. No new images or startup were tested.",
              "sourceHashes": dict(sorted(hashes.items()))}
    return result, profile


def compare(result, profile, resolved, rendered):
    """Compare each derived selector against the exact effective CLI policy."""
    expected = {(e["identity"]["workload"], e["identity"]["container"]): e for e in profile["containers"]}
    actual = {(e["identity"]["workload"], e["identity"]["container"]): e for e in resolved["containers"]}
    if len(actual) != len(resolved["containers"]) or actual.keys() != expected.keys():
        raise ValueError("resolved inventory differs")
    policy = rendered["spec"]["sizes"][0]["effects"]["containerResourcePolicy"]
    if (policy["defaultRequests"] != {"cpu": "10m", "memory": "64Mi"}
            or policy["memoryLimitPercent"] != 110 or policy["goMemoryLimitPercent"] != 80
            or policy["cpuLimitEqualsRequest"] is not True or "memoryLimitMultiplier" in policy):
        raise ValueError("rendered policy differs")
    wire = {(e["workload"], e["container"]): e for e in policy["containers"]}
    if len(wire) != len(policy["containers"]) or wire.keys() != expected.keys():
        raise ValueError("rendered inventory differs")
    for row in result["rows"]:
        if row["scope"] != "controlled":
            if row["candidateResources"] != row["baselineResources"] or row["candidateRuntimeEnv"] != row["baselineRuntimeEnv"]:
                raise ValueError("excluded container changed")
            continue
        identity = row["projectorIdentity"]
        key = identity["workload"], identity["container"]
        requests = row["candidateResources"]["requests"]
        limits = row["candidateResources"]["limits"]
        # The projector preserves extended resources (for example router swift-nic).
        for group in ("requests", "limits"):
            before = {k: v for k, v in row["baselineResources"].get(group, {}).items() if k not in ("cpu", "memory")}
            after = {k: v for k, v in row["candidateResources"][group].items() if k not in ("cpu", "memory")}
            if before != after:
                raise ValueError(f"unrelated resource changed: {key}")
        requests = {k: requests[k] for k in ("cpu", "memory")}
        limits = {k: limits[k] for k in ("cpu", "memory")}
        runtime = {e["name"]: e["value"] for e in row["candidateRuntimeEnv"]}
        if key in actual:
            r, w = actual[key], wire[key]
            if r["identity"] != identity or r["requests"] != requests or r["limits"] != limits or w["requests"] != requests:
                raise ValueError(f"exact resolved budget differs: {key}")
            env = {k: v.removesuffix("B") if k == "GOMEMLIMIT" else v for k, v in r.get("env", {}).items()}
            # Omitted GOMAXPROCS preserves the manifest value, it is not inferred.
            if env != {k: v for k, v in runtime.items() if k == "GOMEMLIMIT"}:
                raise ValueError(f"resolved runtime differs: {key}")
            if w.get("goMemoryLimitPercent", 80) != expected[key].get("goMemoryLimitPercent", 80) or "goMaxProcs" in w:
                raise ValueError(f"wire runtime differs: {key}")
        elif requests != policy["defaultRequests"] or limits != {"cpu": "10m", "memory": "71Mi"} or runtime.get("GOMEMLIMIT") != "53687091":
            raise ValueError(f"fallback does not reproduce inventory: {key}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[3])
    parser.add_argument("--hypershift-root", type=Path, required=True)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--print-profile", action="store_true")
    mode.add_argument("--write-output", action="store_true")
    args = parser.parse_args()
    root, hypershift = args.root.resolve(), args.hypershift_root.resolve()
    result, profile = derive(root, hypershift)
    if args.print_profile:
        print(json.dumps(profile, indent=2))
        return
    profile_bytes = (root / PROFILE).read_bytes()
    if json.loads(profile_bytes) != profile:
        raise ValueError("profile differs from precise derived inventory")
    # Both subcommands are offline. Disable dependency network access as well.
    env = dict(os.environ, GOWORK="off", GOPROXY="off", GOSUMDB="off")
    outputs = {}
    for command, filename in (("resolve", "resolved-profile.json"), ("hypershift", "cluster-sizing.json")):
        outputs[filename] = subprocess.run(
            ["go", "run", ".", "sizing", command, "--profile", "examples/small-constrained-v1.json"],
            cwd=root / "tooling/hcp-perf", env=env, check=True, capture_output=True).stdout
    compare(result, profile, json.loads(outputs["resolved-profile.json"]), json.loads(outputs["cluster-sizing.json"]))
    # Local OpenAPI validation, not Kubernetes admission or deployed capability.
    import jsonschema
    import yaml
    crd = yaml.safe_load((hypershift / SCHEMA).read_bytes())
    schema = next(v["schema"]["openAPIV3Schema"] for v in crd["spec"]["versions"] if v["name"] == "v1alpha1")
    jsonschema.Draft4Validator(schema).validate(json.loads(outputs["cluster-sizing.json"]))
    result["profile"] = {"path": PROFILE, "bytes": len(profile_bytes), "sha256": hashlib.sha256(profile_bytes).hexdigest()}
    result["validation"] = {"profileMatchesDerivation": True, "resolvedAndRenderedMatchEveryControlledRow": True,
                            "excludedResourcesAndEnvUnchanged": True, "localCRDOpenAPISchema": True,
                            "CELAdmissionExecuted": False, "liveAdmissionExecuted": False}
    outputs["scoped-candidate.json"] = encoded(result)
    outputs["output-hashes.json"] = encoded({name: {"bytes": len(value), "sha256": hashlib.sha256(value).hexdigest()}
                                             for name, value in sorted(outputs.items())})
    output = root / OUTPUT
    if not output.is_dir() or output.is_symlink() or output.stat().st_mode & 0o077:
        raise ValueError("scoped output directory must already exist and be private (0700)")
    for name, value in outputs.items():
        path = output / name
        if args.write_output:
            # Exclusive creation: never rewrite a frozen or concurrent artifact.
            with path.open("xb") as stream:
                stream.write(value)
        elif path.read_bytes() != value:
            raise ValueError(f"reproduction differs: {name}")
    print(json.dumps({"totals": result["totals"], "profile": result["profile"], "output": OUTPUT,
                      "explicitOverrides": len(profile["containers"]), "replayVerified": not args.write_output}, indent=2))


if __name__ == "__main__":
    main()
