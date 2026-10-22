#!/usr/bin/env python3
"""Bounded, read-only HCP startup evidence. No RP, load, apply, or resume API."""

import argparse
import concurrent.futures
import copy
import datetime
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import subprocess
import sys
import time


GROUPS = {"containers": "containerStatuses", "initContainers": "initContainerStatuses",
          "ephemeralContainers": "ephemeralContainerStatuses"}
WORKLOADS = {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}
CS_LABEL = "api.openshift.com/id"
POLICY_HASH = "hypershift.openshift.io/container-resource-policy-hash"
RESOURCES = "pods,deployments,statefulsets,daemonsets,replicasets,jobs,cronjobs,endpoints,services,events"
CORE = [("kube-apiserver", "Deployment", 3), ("openshift-apiserver", "Deployment", 3),
        ("packageserver", "Deployment", 3), ("etcd", "StatefulSet", 3)]
VOLATILE = ("/tmp", "/var/tmp", "/run", "/dev/shm")


def require(ok, message):
    if not ok:
        raise ValueError(message)


def stamp():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


def read(path):
    return json.loads(Path(path).read_bytes())


def save(path, value):
    """Atomic publication, including directory sync; output tree is private."""
    path = Path(path)
    tmp = path.with_name(path.name + ".tmp")
    with tmp.open("xb") as stream:
        os.chmod(tmp, 0o600)
        stream.write(encoded(value))
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(tmp, path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def persistent(path):
    path = Path(path).resolve()
    require(not any(path.is_relative_to(p) for p in VOLATILE), "volatile dependency: " + str(path))
    return path


def quantity(value):
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)([numkKMGTPE]|[KMGTPE]i)?", str(value))
    require(match is not None, "unsupported resource quantity: " + str(value))
    scales = {"": 1, "n": Decimal("1e-9"), "u": Decimal("1e-6"), "m": Decimal(".001"), "k": 1000, "K": 1000}
    scales.update({s: 1000**i for i, s in enumerate("MGTPE", 2)})
    scales.update({s + "i": 1024**i for i, s in enumerate("KMGTPE", 1)})
    return Decimal(match[1]) * scales[match[2] or ""]


def profile(spec):
    result = {}
    for group in GROUPS:
        result[group] = []
        for container in spec.get(group, []):
            resources = copy.deepcopy(container.get("resources", {}))
            for bound in ("requests", "limits"):
                if bound in resources and not resources[bound]:
                    del resources[bound]
                for resource, value in resources.get(bound, {}).items():
                    resources[bound][resource] = str(quantity(value).normalize())
            result[group].append(dict(name=container["name"], image=container["image"], resources=resources,
                                      restartPolicy=container.get("restartPolicy"),
                                      env=sorted((e for e in container.get("env", []) if e["name"] in
                                                  ("GOMEMLIMIT", "GOMAXPROCS")), key=lambda e: e["name"])))
        if group != "initContainers":
            result[group].sort(key=lambda c: c["name"])
    return result


def validate(config):
    require(config.get("version") == "v1", "startup input version must be v1")
    require(set(config) <= {"version", "durableStateRoot", "managementKubeconfig", "kubectl", "identity",
                           "candidate", "requirements", "maxDurationSeconds", "pollSeconds", "failureWindowSeconds",
                           "stallSeconds", "diagnostics", "nodeMetrics"}, "unknown startup input field")
    ident = config["identity"]
    require(set(ident) <= {"csID", "hcpNamespace", "hcNamespace", "hcUID", "hcpUID", "namespaceUID"}, "unknown identity field")
    require(re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]*", ident.get("csID", "")), "exact CSID is required, not RP name")
    for key in ("hcpNamespace", "hcNamespace"):
        if key in ident:
            require(re.fullmatch(r"[a-z0-9][a-z0-9-]*", ident[key]), "invalid " + key)
    for key, default, low, high in (("maxDurationSeconds", 2700, 1, 7200), ("pollSeconds", 15, 15, 30),
                                   ("failureWindowSeconds", 120, 0, 600), ("stallSeconds", 180, 30, 1800)):
        value = config.setdefault(key, default)
        require(type(value) is int and low <= value <= high, "invalid " + key)
    candidate = config["candidate"]
    require(set(candidate) == {"id", "profiles", "resolvedPolicy", "ledger"}, "candidate requires id, profiles, resolvedPolicy, ledger")
    require(isinstance(candidate["id"], str) and candidate["id"], "candidate id required")
    profiles = candidate["profiles"]
    require(isinstance(profiles, list) and profiles, "resolved profiles must be a nonempty array")
    names, owners = set(), set()
    for p in profiles:
        require(set(p) <= {"name", "owner", "scope", "reason", "expectedReplicas", "lifecycle", *GROUPS}, "unknown profile field")
        require(p["name"] not in names, "duplicate profile name")
        names.add(p["name"])
        owner = p["owner"]
        require(set(owner) <= {"kind", "name", "namespace", "uid"} and owner["kind"] in WORKLOADS | {"Pod"}
                and owner.get("name") and owner.get("namespace"), "exact profile owner required")
        key = tuple(owner[k] for k in ("kind", "name", "namespace"))
        require(key not in owners, "ambiguous profile owner")
        owners.add(key)
        require(p["scope"] in ("controlled", "ignored") and (p["scope"] != "ignored" or p.get("reason")), "explicit scope/reason required")
        lifecycle = p.get("lifecycle", {})
        require(isinstance(lifecycle, dict) and set(lifecycle) <= {"mode", "reason"}, "invalid lifecycle")
        if owner["kind"] in ("Job", "CronJob"):
            require(lifecycle.get("mode") in ("present", "completed", "optional", "disabled")
                    and lifecycle.get("reason") and "expectedReplicas" not in p,
                    "Job/CronJob requires explicit lifecycle mode/reason, not expectedReplicas")
        else:
            require(not lifecycle or lifecycle.get("mode") == "disabled" and lifecycle.get("reason"),
                    "long-running lifecycle may only be explicitly disabled with reason")
            replicas = p.get("expectedReplicas")
            require(type(replicas) is int and (replicas == 0 if lifecycle else replicas > 0),
                    "expectedReplicas required; zero only for explicitly disabled owners")
            require(owner["kind"] != "Pod" or replicas in (0, 1), "standalone Pod expectedReplicas must be zero or one")
        require(p.get("containers"), "profile containers required")
        for group in GROUPS:
            require(len({c["name"] for c in p.get(group, [])}) == len(p.get(group, [])), "duplicate container")
            for c in p.get(group, []):
                require(re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", c.get("image", "")), "expected image must be digest-pinned")
                require(isinstance(c.get("resources"), dict), "explicit resources object required, including ignored absence")
                if p["scope"] == "controlled":
                    require(all(r in c["resources"].get(b, {}) for b in ("requests", "limits") for r in ("cpu", "memory")),
                            "controlled expected requests and limits must be explicit")
                require(all(e.get("name") in ("GOMEMLIMIT", "GOMAXPROCS") and set(e) == {"name", "value"}
                            and isinstance(e["value"], str)
                            for e in c.get("env", [])), "freeze only explicit non-secret Go env")
        profile(p)
    require(isinstance(candidate["resolvedPolicy"], dict) and isinstance(candidate["ledger"], dict), "policy and ledger must be JSON objects")
    requirements = config["requirements"]
    require(isinstance(requirements, list) and requirements, "explicit readiness requirements required")
    require(len({r["profile"] for r in requirements}) == len(requirements), "duplicate readiness profile")
    for r in requirements:
        require(set(r) <= {"profile", "readyReplicas", "endpoints", "readyEndpoints"} and r["profile"] in names,
                "invalid readiness requirement")
        require(type(r["readyReplicas"]) is int and r["readyReplicas"] > 0, "positive readiness count required")
        p = next(p for p in profiles if p["name"] == r["profile"])
        require(r["readyReplicas"] <= p.get("expectedReplicas", 0), "readiness minimum exceeds expectedReplicas")
        require(isinstance(r.get("endpoints"), str) and re.fullmatch(r"[a-z0-9][a-z0-9.-]*", r["endpoints"])
                and type(r.get("readyEndpoints")) is int and r["readyEndpoints"] > 0, "exact Endpoints name/count required")
        require(r["readyEndpoints"] <= p["expectedReplicas"], "endpoint minimum exceeds expectedReplicas")
    for name, kind, minimum in CORE:
        matches = [p for p in profiles if p["owner"]["name"] == name and p["owner"]["kind"] == kind]
        require(len(matches) == 1, "required core owner: " + name)
        rows = [r for r in requirements if r["profile"] == matches[0]["name"]]
        require(len(rows) == 1 and rows[0]["readyReplicas"] >= minimum and rows[0]["readyEndpoints"] >= minimum,
                "required core replicas/endpoints: " + name)
    for field, defaults in (("diagnostics", dict(maxCommandsPerPoll=8, maxCommandsTotal=128, tailLines=100, limitBytes=65536)),
                            ("nodeMetrics", dict(enabled=False, maxNodesPerPoll=8, maxBytes=4194304))):
        values = config.setdefault(field, {})
        require(set(values) <= set(defaults), "unknown " + field + " field")
        for key, default in defaults.items():
            value = values.setdefault(key, default)
            if key == "enabled":
                require(type(value) is bool, "enabled must be boolean")
            else:
                require(type(value) is int and 1 <= value <= default, "invalid bound: " + key)
    return config


def freeze(config_path, output):
    config_path, output = Path(config_path).resolve(), Path(output).resolve()
    config = read(config_path)
    base = config_path.parent
    resolve = lambda value: (base / Path(value).expanduser()).resolve()
    root = persistent(resolve(config["durableStateRoot"]))
    require(root.is_dir() and output.is_relative_to(root), "output must be beneath existing durableStateRoot")
    persistent(output)
    config["durableStateRoot"] = str(root)
    kube = persistent(resolve(config["managementKubeconfig"]))
    require(kube.is_file(), "management kubeconfig missing")
    config["managementKubeconfig"] = str(kube)
    tool = config.get("kubectl", "kubectl")
    tool = str(resolve(tool)) if "/" in tool else shutil.which(tool)
    require(tool and os.access(tool, os.X_OK), "kubectl executable missing")
    config["kubectl"] = str(persistent(tool))
    persistent(sys.executable)
    inputs = {}
    for field in ("profiles", "resolvedPolicy", "ledger"):
        ref = config["candidate"][field]
        require(set(ref) == {"path", "sha256"}, "candidate input requires path and sha256: " + field)
        path = resolve(ref["path"])
        data = path.read_bytes()
        require(digest(data) == ref["sha256"], "candidate input hash mismatch: " + field)
        config["candidate"][field] = json.loads(data)
        inputs[field] = dict(path=str(path), sha256=digest(data))
    validate(config)
    require(output.parent.is_dir(), "output parent must exist")
    output.mkdir(mode=0o700)
    os.chmod(output, 0o700)
    bundle = output / "bundle"
    bundle.mkdir(mode=0o700)
    source = Path(__file__).resolve()
    for name, data in (("startup.py", source.read_bytes()), ("config.json", encoded(config)),
                       ("input.json", config_path.read_bytes())):
        with (bundle / name).open("xb") as stream:
            os.chmod(bundle / name, 0o600)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
    save(bundle / "provenance.json", dict(at=stamp(), inputs=inputs, source=str(source),
         python=dict(path=sys.executable, version=sys.version),
         managementKubeconfig=dict(path=str(kube), sha256=digest(kube.read_bytes())),
         kubectl=dict(path=config["kubectl"], sha256=digest(Path(config["kubectl"]).read_bytes())),
         output=str(output), entrypoint=[sys.executable, "-B", str(bundle / "startup.py"), "observe", "--output", str(output)]))
    save(bundle / "sha256.json", {p.name: digest(p.read_bytes()) for p in bundle.iterdir()})
    return dict(output=str(output), entrypoint=read(bundle / "provenance.json")["entrypoint"])


def identity(objects, expected, bound):
    result = dict(bound)
    for kind, prefix in (("HostedCluster", "hc"), ("HostedControlPlane", "hcp")):
        matches = [o for o in objects if o["kind"] == kind and o["metadata"].get("labels", {}).get(CS_LABEL) == expected["csID"]]
        require(len(matches) <= 1, "ambiguous " + kind + " for exact CSID")
        if not matches:
            continue
        obj = matches[0]
        meta = obj["metadata"]
        for key, actual in ((prefix + "Namespace", meta["namespace"]), (prefix + "UID", meta["uid"])):
            require(not expected.get(key) or expected[key] == actual, "expected identity mismatch: " + key)
            require(not result.get(key) or result[key] == actual, "identity replaced: " + key)
            result[key] = actual
        result[prefix + "Name"] = meta["name"]
    return result


def owner_for(obj, objects):
    """Resolve intermediate controllers by UID, never by Pod-name prefix."""
    seen = set()
    while obj["kind"] in ("Pod", "ReplicaSet", "Job"):
        refs = [r for r in obj["metadata"].get("ownerReferences", []) if r.get("controller")]
        require(len(refs) <= 1, "ambiguous controller")
        if not refs:
            break
        ref = refs[0]
        if obj["kind"] == "Job" and ref["kind"] != "CronJob":
            break
        require(ref["uid"] not in seen, "controller cycle")
        seen.add(ref["uid"])
        parent = objects.get(ref["uid"])
        require(parent is not None, "missing controller UID " + ref["uid"])
        require(parent["kind"] == ref["kind"] and parent["metadata"]["name"] == ref["name"] and
                parent["metadata"].get("namespace") == obj["metadata"].get("namespace"), "controller identity mismatch")
        obj = parent
    return {"kind": obj["kind"], **{k: obj["metadata"].get(k) for k in ("name", "namespace", "uid")}}


def is_ready(obj):
    return not obj["metadata"].get("deletionTimestamp") and any(
        c["type"] == "Ready" and c["status"] == "True" for c in obj.get("status", {}).get("conditions", []))


def analyze(snapshot, config):
    objects = snapshot["objects"]
    index = {o["metadata"]["uid"]: o for o in objects if o["metadata"].get("uid")}
    rows, failures, unknown, triggers, milestones = [], [], list(snapshot.get("errors", [])), [], set()
    namespaces = [o for o in objects if o["kind"] == "Namespace" and o["metadata"].get("name") == snapshot.get("namespace")]
    namespace_uid = snapshot.get("identity", {}).get("namespaceUID")
    if not namespace_uid or len(namespaces) != 1 or namespaces[0]["metadata"].get("uid") != namespace_uid:
        unknown.append("namespace UID unverified in this snapshot")
    profiles = config["candidate"]["profiles"]
    pods = [o for o in objects if o["kind"] == "Pod"]
    pod_profiles, pod_matches, container_progress = {}, {}, {}
    for obj in objects:
        kind, meta = obj["kind"], obj["metadata"]
        if kind not in WORKLOADS | {"Pod", "ReplicaSet"}:
            continue
        template = obj if kind == "Pod" else (obj["spec"]["jobTemplate"]["spec"]["template"] if kind == "CronJob" else obj["spec"]["template"])
        row = dict(kind=kind, name=meta["name"], uid=meta["uid"], namespace=meta["namespace"],
                   observedProfile=profile(template["spec"]), policyHash=template["metadata"].get("annotations", {}).get(POLICY_HASH),
                   role="current-pod" if kind == "Pod" else "rollout-template" if kind == "ReplicaSet" else "desired-template")
        try:
            row["owner"] = owner_for(obj, index)
            matches = [p for p in profiles if all(row["owner"].get(k) == (
                snapshot.get("namespace") if k == "namespace" and v == "$hcpNamespace" else v)
                for k, v in p["owner"].items())]
            require(len(matches) == 1, "no unique frozen profile")
            p = matches[0]
            row.update(profile=p["name"], scope=p["scope"], reason=p.get("reason"), matchesExpected=row["observedProfile"] == profile(p))
            if kind == "Pod":
                pod_profiles[meta["uid"]] = p["name"]
                pod_matches[meta["uid"]] = row["matchesExpected"]
            if row["matchesExpected"]:
                milestones.add("policy:" + p["name"] + ":" + row["role"])
        except ValueError as exc:
            row["unknown"] = str(exc)
            unknown.append(kind + "/" + meta["name"] + ": " + str(exc))
        rows.append(row)
        if kind != "Pod":
            continue
        status = obj.get("status", {})
        for condition in status.get("conditions", []):
            if condition["type"] == "PodScheduled" and condition["status"] == "False":
                failures.append(dict(reason="PendingScheduling", pod=meta["name"], uid=meta["uid"], detail=condition, fatal=False))
        for group, status_key in GROUPS.items():
            statuses = {s["name"]: s for s in status.get(status_key, [])}
            for container in template["spec"].get(group, []):
                st = statuses.get(container["name"], {})
                key = dict(pod=meta["name"], uid=meta["uid"], container=container["name"], group=group)
                progress_key = (pod_profiles.get(meta["uid"]), group, container["name"])
                if progress_key[0] and (st.get("ready") or (group == "initContainers" and
                        container.get("restartPolicy") != "Always" and st.get("state", {}).get("terminated", {}).get("exitCode") == 0)):
                    container_progress[progress_key] = container_progress.get(progress_key, 0) + 1
                for state_key in ("state", "lastState"):
                    terminated = st.get(state_key, {}).get("terminated", {})
                    reason = terminated.get("reason")
                    if reason == "OOMKilled" or (state_key == "state" and terminated.get("exitCode", 0) != 0):
                        failure = dict(key, reason="OOMKilled" if reason == "OOMKilled" else "TerminatedError", fatal=True,
                                       detail=terminated, state=state_key)
                        failures.append(failure)
                        triggers.append(dict(key, trigger=reason, token=terminated, previous=state_key == "lastState"))
                if st.get("restartCount", 0):
                    failures.append(dict(key, reason="RestartObserved", fatal=True, count=st["restartCount"]))
                    triggers.append(dict(key, trigger="restart", token=st["restartCount"], previous=True))
                waiting = st.get("state", {}).get("waiting")
                if waiting:
                    failures.append(dict(key, reason="InitWaiting" if group == "initContainers" else "ContainerWaiting", fatal=False, detail=waiting))
                    triggers.append(dict(key, trigger="waiting", token=waiting.get("reason"), previous=False))
    for key, count in container_progress.items():
        milestones.update(":".join(key) + ":completed-or-ready:" + str(n) for n in range(1, count + 1))
    for event in (o for o in objects if o["kind"] == "Event"):
        ref, message = event.get("involvedObject", {}), event.get("message", "")
        if ref.get("uid") not in {p["metadata"]["uid"] for p in pods}:
            continue
        probe = "liveness" if "liveness probe" in message.lower() else "startup" if "startup probe" in message.lower() else "readiness" if "readiness probe" in message.lower() else None
        if probe:
            fatal = event.get("reason") == "Killing" and probe in ("liveness", "startup")
            failures.append(dict(reason="ProbeKill" if fatal else "ProbeFailure", probe=probe, pod=ref["name"], uid=ref["uid"],
                                 fatal=fatal, detail=message, eventUID=event["metadata"]["uid"], count=event.get("count", 1)))
            if fatal:
                pod = index[ref["uid"]]
                for group in GROUPS:
                    for c in pod["spec"].get(group, []):
                        if ref.get("fieldPath") == group + "{" + c["name"] + "}":
                            triggers.append(dict(pod=ref["name"], uid=ref["uid"], container=c["name"], group=group,
                                                 trigger="probe-kill", token=[event["metadata"]["uid"], event.get("count", 1)], previous=True))
    checks = []
    for requirement in config["requirements"]:
        selected = [p for p in pods if pod_profiles.get(p["metadata"]["uid"]) == requirement["profile"]]
        ready = {p["metadata"]["uid"] for p in selected if is_ready(p)}
        matching_ready = {uid for uid in ready if pod_matches.get(uid)}
        endpoints = [o for o in objects if o["kind"] == "Endpoints" and o["metadata"]["name"] == requirement["endpoints"]]
        addresses = set()
        for endpoint in endpoints:
            for subset in endpoint.get("subsets", []):
                for address in subset.get("addresses", []):
                    ref = address.get("targetRef", {})
                    if ref.get("kind") == "Pod" and ref.get("uid") in ready:
                        addresses.add(ref["uid"])
        check = dict(profile=requirement["profile"], ready=len(ready), matchingReady=len(matching_ready),
                     required=requirement["readyReplicas"], endpoints=len(addresses), requiredEndpoints=requirement["readyEndpoints"])
        check["satisfied"] = len(matching_ready) >= check["required"] and len(addresses & matching_ready) >= check["requiredEndpoints"]
        checks.append(check)
        # High-water counters, not new timestamps, Pod UIDs, or repeated restart loops.
        for metric in ("ready", "matchingReady", "endpoints"):
            milestones.update(f"{requirement['profile']}:{metric}:{n}" for n in range(1, min(check[metric], check["required"]) + 1))
    profile_checks, completed_job_uids = [], set()
    for p in profiles:
        kind = p["owner"]["kind"]
        selected_rows = [r for r in rows if r.get("profile") == p["name"]]
        owners = [index[r["uid"]] for r in selected_rows if r["kind"] == kind and r["uid"] == r["owner"]["uid"]]
        selected_pods = [pod for pod in pods if pod_profiles.get(pod["metadata"]["uid"]) == p["name"]]
        mode = p.get("lifecycle", {}).get("mode", "running")
        check = dict(profile=p["name"], scope=p["scope"], lifecycle=mode, ownerCount=len(owners),
                     current=len(selected_pods), ready=sum(is_ready(pod) for pod in selected_pods),
                     matchingReady=sum(is_ready(pod) and pod_matches.get(pod["metadata"]["uid"], False) for pod in selected_pods),
                     satisfied=False)
        owner = owners[0] if len(owners) == 1 else None
        present = owner is not None and not owner["metadata"].get("deletionTimestamp")
        if mode == "disabled":
            disabled = not owners
            if present:
                if kind in ("Deployment", "StatefulSet"):
                    disabled = owner["spec"].get("replicas") == 0 and owner.get("status", {}).get("replicas", 0) == 0
                    disabled &= owner.get("status", {}).get("observedGeneration") == owner["metadata"].get("generation")
                elif kind in ("Job", "CronJob"):
                    disabled = owner["spec"].get("suspend") is True and not owner.get("status", {}).get("active")
            check["satisfied"] = disabled and not selected_pods
        elif kind in ("Job", "CronJob"):
            jobs = [index[r["uid"]] for r in selected_rows if r["kind"] == "Job"]
            complete = lambda job: not job["metadata"].get("deletionTimestamp") and not job.get("status", {}).get("active") and any(
                c["type"] == "Complete" and c["status"] == "True" for c in job.get("status", {}).get("conditions", []))
            check["completedJobs"] = sum(complete(job) for job in jobs)
            completed_job_uids.update(job["metadata"]["uid"] for job in jobs if complete(job))
            check["satisfied"] = (mode == "optional" and not owners and not selected_rows) or (
                present and (mode != "completed" or bool(jobs) and all(complete(job) for job in jobs)))
            for job in jobs:
                if any(c["type"] == "Failed" and c["status"] == "True" for c in job.get("status", {}).get("conditions", [])):
                    failures.append(dict(reason="JobFailed", uid=job["metadata"]["uid"], job=job["metadata"]["name"], fatal=True))
        else:
            expected = p["expectedReplicas"]
            check["expectedReplicas"] = expected
            if present:
                status = owner.get("status", {})
                generation = owner["metadata"].get("generation")
                generation_current = generation is not None and status.get("observedGeneration") == generation
                if kind in ("Deployment", "StatefulSet"):
                    desired = owner["spec"].get("replicas")
                    fields = ("replicas", "readyReplicas", "updatedReplicas", "availableReplicas") if kind == "Deployment" else (
                        "replicas", "readyReplicas", "currentReplicas", "updatedReplicas")
                    converged = all(status.get(field) == expected for field in fields)
                    if kind == "StatefulSet":
                        converged &= bool(status.get("currentRevision")) and status.get("currentRevision") == status.get("updateRevision")
                elif kind == "DaemonSet":
                    desired = status.get("desiredNumberScheduled")
                    converged = all(status.get(field) == expected for field in (
                        "currentNumberScheduled", "numberReady", "numberAvailable", "updatedNumberScheduled"))
                    converged &= status.get("numberMisscheduled") == 0
                else:
                    desired, converged, generation_current = 1, is_ready(owner), True
                check.update(desired=desired, observedGeneration=status.get("observedGeneration"), generation=generation,
                             controllerConverged=bool(converged and generation_current))
                check["satisfied"] = desired == expected and converged and generation_current and all(
                    check[field] == expected for field in ("current", "ready", "matchingReady"))
        if present:
            milestones.add("owner:" + p["name"] + ":present")
        if check["satisfied"]:
            milestones.add("owner:" + p["name"] + ":converged")
        profile_checks.append(check)
    for kind in ("HostedCluster", "HostedControlPlane"):
        selected = [o for o in objects if o["kind"] == kind and o["metadata"].get("labels", {}).get(CS_LABEL) == config["identity"]["csID"]]
        available = len(selected) == 1 and not selected[0]["metadata"].get("deletionTimestamp") and any(
            c["type"] == "Available" and c["status"] == "True" and c.get("observedGeneration", selected[0]["metadata"].get("generation")) == selected[0]["metadata"].get("generation")
            for c in selected[0].get("status", {}).get("conditions", []))
        checks.append(dict(profile=kind, satisfied=available))
        if selected:
            milestones.add(kind + ":exists")
        if available:
            milestones.add(kind + ":available")
    current = [r for r in rows if r["role"] == "current-pod"]
    desired = [r for r in rows if r["role"] == "desired-template"]
    drift = [r for r in current + desired if r.get("matchesExpected") is False]
    nonready = [p["metadata"]["name"] for p in pods if not is_ready(p) and not (
        p.get("status", {}).get("phase") == "Succeeded" and any(
            ref.get("controller") and ref.get("kind") == "Job" and ref.get("uid") in completed_job_uids
            for ref in p["metadata"].get("ownerReferences", [])))]
    fatal = any(f["fatal"] for f in failures)
    phase = "unknown" if unknown else "not-applied" if not rows else "applying" if drift else "progressing"
    if fatal:
        phase = "failing"
    elif not unknown and current and not drift and not nonready and all(c["satisfied"] for c in checks + profile_checks):
        phase = "ready"
    etcd_profile = next(p["name"] for p in profiles if p["owner"]["name"] == "etcd" and p["owner"]["kind"] == "StatefulSet")
    etcd = next(c for c in checks if c["profile"] == etcd_profile)
    return dict(phase=phase, pods=len(pods), readyPods=sum(is_ready(p) for p in pods), profiles=rows, checks=checks, profileChecks=profile_checks,
                failures=failures, unknown=unknown, nonready=nonready, triggers=triggers, milestones=sorted(milestones),
                logFindings=snapshot.get("diagnostics", {}).get("logFindings", []),
                mixedRollout=bool(drift), etcdQuorumProxy=dict(ready=etcd["ready"], quorum=etcd["required"] // 2 + 1,
                    available=etcd["ready"] >= etcd["required"] // 2 + 1, proofOfQuorum=False))


class Kubectl:
    def __init__(self, config, monotonic=time.monotonic):
        self.config, self.monotonic = config, monotonic

    def command(self, args, deadline, max_bytes=16777216):
        timeout = min(10, deadline - self.monotonic())
        if timeout <= 0:
            return dict(args=args, error="observation deadline", stdout="", at=stamp())
        argv = [self.config["kubectl"], "--kubeconfig", self.config["managementKubeconfig"], "--request-timeout=10s", *args]
        receipt = dict(args=argv, at=stamp())
        buffers = {"stdout": bytearray(), "stderr": bytearray()}
        try:
            with subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True) as process:
                end = self.monotonic() + timeout
                with selectors.DefaultSelector() as selector:
                    selector.register(process.stdout, selectors.EVENT_READ, "stdout")
                    selector.register(process.stderr, selectors.EVENT_READ, "stderr")
                    while selector.get_map():
                        remaining = end - self.monotonic()
                        if remaining <= 0:
                            receipt["error"] = "command timeout"
                            break
                        for key, _ in selector.select(min(remaining, .1)):
                            data = os.read(key.fileobj.fileno(), 65536)
                            if not data:
                                selector.unregister(key.fileobj)
                                continue
                            limit = max_bytes if key.data == "stdout" else 65536
                            available = limit - len(buffers[key.data])
                            buffers[key.data].extend(data[:available])
                            if len(data) > available:
                                receipt.update(error="response size bound", truncated=True)
                                break
                        if receipt.get("error"):
                            break
                if receipt.get("error"):
                    os.killpg(process.pid, signal.SIGKILL)
                try:
                    receipt["exitCode"] = process.wait(timeout=max(.01, end - self.monotonic()))
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                    receipt["error"] = "command timeout"
                if receipt.get("exitCode") and not receipt.get("error"):
                    receipt["error"] = "kubectl failed"
        except OSError as exc:
            receipt["error"] = str(exc)
        receipt.update({key: bytes(value).decode(errors="replace") for key, value in buffers.items()})
        receipt["finishedAt"] = stamp()
        return receipt


class Monitor:
    def __init__(self, config, output, client, monotonic=time.monotonic, sleep=time.sleep):
        self.config, self.output, self.client = config, Path(output), client
        self.monotonic, self.sleep = monotonic, sleep
        self.bound, self.seen_triggers, self.milestones, self.failures = {}, set(), set(), {}
        self.command_count = 0
        self.interrupted = None
        self.empty_namespace = False
        self.first_pod = None
        self.coverage_errors = []

    def stop(self, signum, _frame):
        self.interrupted = signal.Signals(signum).name

    def collect(self, deadline):
        objects, receipts, errors = [], [], []

        def get(args):
            if self.interrupted:
                errors.append("interrupted collection")
                return []
            receipt = self.client.command(["get", *args, "-o", "json"], deadline)
            receipts.append(receipt)
            if receipt.get("error"):
                errors.append(" ".join(args) + ": " + receipt["error"])
                return []
            if not receipt["stdout"].strip() and "--ignore-not-found" in args:
                return []
            try:
                value = json.loads(receipt["stdout"])
                return value.get("items", []) if value.get("kind", "").endswith("List") else [value]
            except (ValueError, AttributeError):
                errors.append("invalid Kubernetes JSON: " + " ".join(args))
                return []

        discovered = get(["hostedclusters,hostedcontrolplanes", "-A", "-l", CS_LABEL + "=" + self.config["identity"]["csID"]])
        objects.extend(discovered)
        try:
            self.bound = identity(discovered, self.config["identity"], self.bound)
        except ValueError as exc:
            errors.append(str(exc))
            return dict(objects=objects, receipts=receipts, errors=errors, identity=self.bound)
        namespace = self.bound.get("hcpNamespace") or self.config["identity"].get("hcpNamespace")
        if not namespace:
            errors.append("HCP namespace not yet discovered; no Pod coverage")
        if namespace:
            namespaces = get(["namespace", namespace, "--ignore-not-found"])
            if len(namespaces) != 1 or namespaces[0].get("kind") != "Namespace" or namespaces[0].get("metadata", {}).get("name") != namespace or not namespaces[0].get("metadata", {}).get("uid"):
                errors.append("namespace absent or UID unverified")
            else:
                uid = namespaces[0]["metadata"]["uid"]
                expected = self.bound.get("namespaceUID") or self.config["identity"].get("namespaceUID")
                if expected and uid != expected:
                    errors.append("namespace UID replaced")
                    return dict(objects=objects, receipts=receipts, errors=errors, identity=self.bound)
                self.bound.update(hcpNamespace=namespace, namespaceUID=uid)
            objects.extend(namespaces)
            objects.extend(get([RESOURCES, "-n", namespace]))
        return dict(objects=objects, receipts=receipts, errors=errors, identity=dict(self.bound), namespace=namespace)

    def diagnostics(self, snapshot, result, deadline):
        jobs = []
        bounds = self.config["diagnostics"]
        triggers = []
        for trigger in result["triggers"]:
            triggers.append(trigger)
            if trigger["previous"]:
                triggers.append(dict(trigger, previous=False))
        for trigger in triggers:
            key = digest(encoded(trigger))
            if key in self.seen_triggers:
                continue
            if len(jobs) >= bounds["maxCommandsPerPoll"] or self.command_count >= bounds["maxCommandsTotal"]:
                break
            self.seen_triggers.add(key)
            self.command_count += 1
            args = ["logs", trigger["pod"], "-n", snapshot["namespace"], "-c", trigger["container"],
                    "--tail=" + str(bounds["tailLines"]), "--limit-bytes=" + str(bounds["limitBytes"])]
            if trigger["previous"]:
                args.append("--previous")
            jobs.append((trigger, args, bounds["limitBytes"]))
        metrics = self.config["nodeMetrics"]
        nodes = sorted({p["spec"]["nodeName"] for p in snapshot["objects"] if p["kind"] == "Pod" and p["spec"].get("nodeName")})
        if metrics["enabled"]:
            for node in nodes[:metrics["maxNodesPerPoll"]]:
                jobs.append((dict(source="kubelet-cadvisor", node=node),
                             ["get", "--raw", "/api/v1/nodes/" + node + "/proxy/metrics/cadvisor"], metrics["maxBytes"]))
        def execute(job):
            source, args, limit = job
            if self.interrupted:
                return dict(source=source, receipt=dict(error="interrupted", stdout=""))
            receipt = self.client.command(args, deadline, limit)
            if source.get("source") == "kubelet-cadvisor":
                # Keep raw counters/gauges, including timestamps and quota/period, scoped to this namespace.
                needle = 'namespace="' + snapshot["namespace"] + '"'
                receipt["stdout"] = "\n".join(line for line in receipt.get("stdout", "").splitlines() if needle in line)
            return dict(source=source, receipt=receipt)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            records = list(pool.map(execute, jobs))
        findings = []
        for record in records:
            if "trigger" not in record["source"] or record["receipt"].get("error"):
                continue
            for line in record["receipt"].get("stdout", "").splitlines():
                for text, reason in (("the server could not find the requested resource", "MissingAPIResource"),
                                     ("no matches for kind", "MissingAPIKind"), ("connection refused", "APIConnectionRefused")):
                    if text in line.lower():
                        findings.append(dict(source=record["source"], reason=reason, evidence=line, inferredCause=False))
        return dict(records=records, logFindings=findings, logCommandsTotal=self.command_count,
                    pendingLogTriggers=sum(digest(encoded(t)) not in self.seen_triggers for t in triggers),
                    logsCapped=self.command_count >= bounds["maxCommandsTotal"],
                    metricsEnabled=metrics["enabled"], nodesSkipped=max(0, len(nodes) - metrics["maxNodesPerPoll"]) if metrics["enabled"] else len(nodes),
                    unavailable=["Go runtime/heap metrics require a separate source", "CPU/RSS/quota absent from source are unknown, never zero"])

    def run(self):
        start = self.monotonic()
        deadline = start + self.config["maxDurationSeconds"]
        last_progress, first_failure, count = start, None, 0
        last = dict(phase="unknown")
        outcome = "deadline"
        save(self.output / "started.json", dict(at=stamp(), monotonicStart=start, monotonicDeadline=deadline,
             pid=os.getpid(), mutations=False, rpQueried=False, resumeSupported=False))
        try:
            while self.monotonic() < deadline and not self.interrupted:
                tick = self.monotonic()
                snapshot = self.collect(deadline)
                result = analyze(snapshot, self.config)
                now = self.monotonic()
                if result["pods"] and self.first_pod is None:
                    self.first_pod = dict(at=stamp(), elapsedSeconds=now - start, emptyNamespaceObservedBefore=self.empty_namespace)
                if not result["pods"] and snapshot.get("namespace") and not snapshot["errors"]:
                    self.empty_namespace = True
                fresh = set(result["milestones"]) - self.milestones
                if fresh:
                    last_progress = now
                    self.milestones.update(fresh)
                for failure in result["failures"]:
                    self.failures[digest(encoded(failure))] = failure
                if any(f["fatal"] for f in result["failures"]) and first_failure is None:
                    first_failure = now
                if first_failure is not None:
                    result["phase"] = "failing"
                snapshot["diagnostics"] = self.diagnostics(snapshot, result, min(deadline, tick + self.config["pollSeconds"]))
                result["logFindings"] = snapshot["diagnostics"]["logFindings"]
                diagnostic_errors = [dict(source=r["source"], error=r["receipt"]["error"])
                                     for r in snapshot["diagnostics"]["records"] if r["receipt"].get("error")]
                if result["unknown"] or diagnostic_errors or snapshot["diagnostics"]["pendingLogTriggers"]:
                    self.coverage_errors.append(dict(at=stamp(), snapshot=count + 1, unknown=result["unknown"],
                        diagnosticErrors=diagnostic_errors, pendingLogTriggers=snapshot["diagnostics"]["pendingLogTriggers"]))
                count += 1
                name = f"snapshot-{count:06d}.json"
                last = dict(at=stamp(), elapsedSeconds=self.monotonic() - start, monotonicDeadline=deadline,
                            snapshot=name, **{k: v for k, v in result.items() if k not in ("profiles", "triggers", "milestones")},
                            noProgressSeconds=now - last_progress, stalled=now - last_progress >= self.config["stallSeconds"],
                            firstPod=self.first_pod, emptyNamespaceObserved=self.empty_namespace,
                            failureWindowClipped=first_failure is not None and first_failure + self.config["failureWindowSeconds"] > deadline)
                save(self.output / name, dict(at=last["at"], elapsedSeconds=last["elapsedSeconds"], **snapshot, analysis=result))
                with (self.output / "progress.jsonl").open("ab") as stream:
                    stream.write(encoded(last))
                    stream.flush()
                    os.fsync(stream.fileno())
                save(self.output / "status.json", last)
                if result["phase"] == "ready":
                    outcome = "ready"
                    break
                if first_failure is not None and self.monotonic() >= first_failure + self.config["failureWindowSeconds"]:
                    outcome = "failure-window-complete"
                    break
                self.sleep(max(0, min(deadline, tick + self.config["pollSeconds"]) - self.monotonic()))
        except Exception as exc:
            outcome = "observer-error"
            last["error"] = str(exc)
        finally:
            if self.interrupted:
                outcome = "interrupted"
            final = dict(at=stamp(), outcome=outcome, signal=self.interrupted, snapshots=count, last=last,
                         identity=self.bound, elapsedSeconds=self.monotonic() - start, failures=list(self.failures.values()),
                         coverage=dict(firstPod=self.first_pod, emptyNamespaceObserved=self.empty_namespace, lossless=False,
                                       gaps=self.coverage_errors, metricsEnabled=self.config["nodeMetrics"]["enabled"],
                                       failureWindowSecondsObserved=None if first_failure is None else self.monotonic() - first_failure),
                         costs=dict(ledger=self.config["candidate"]["ledger"], ledgerSHA256=digest(encoded(self.config["candidate"]["ledger"])),
                                    repriced=False, budgetChanged=False),
                         mutations=False, rpQueried=False, loadStarted=False)
            save(self.output / "outcome.json", final)
            save(self.output / "status.json", dict(last, terminal=True, outcome=outcome, signal=self.interrupted))
        return final


def diagnose(output):
    """Offline, read-only, including interrupted runs and missing dependencies."""
    output = Path(output).resolve()
    bundle = output / "bundle"
    errors = []
    hashes = read(bundle / "sha256.json")
    for name, expected in hashes.items():
        require(Path(name).name == name, "invalid bundle hash path")
        path = bundle / name
        if not path.is_file() or digest(path.read_bytes()) != expected:
            errors.append("missing/changed bundle file: " + name)
    config = read(bundle / "config.json")
    provenance = read(bundle / "provenance.json")
    dependencies = []
    for key in ("managementKubeconfig", "kubectl"):
        ref = provenance[key]
        path = Path(ref["path"])
        dependencies.append(dict(kind=key, path=str(path), exists=path.is_file(),
                                 hashMatches=path.is_file() and digest(path.read_bytes()) == ref["sha256"]))
    snapshots, failures, latest, diagnostics = 0, {}, None, []
    for path in sorted(output.glob("snapshot-*.json")):
        try:
            snapshot = read(path)
            latest = analyze(snapshot, config)
            diagnostic = snapshot.get("diagnostics", {})
            diagnostics.append(dict(snapshot=path.name, **{k: v for k, v in diagnostic.items() if k != "records"},
                commands=[dict(source=r["source"], error=r["receipt"].get("error"), exitCode=r["receipt"].get("exitCode"))
                          for r in diagnostic.get("records", [])]))
            snapshots += 1
            for failure in latest["failures"]:
                failures[digest(encoded(failure))] = failure
        except (ValueError, KeyError) as exc:
            errors.append(path.name + ": " + str(exc))
    outcome = read(output / "outcome.json") if (output / "outcome.json").is_file() else None
    return dict(state=outcome["outcome"] if outcome else "started-without-outcome-interrupted-or-unknown" if
                (output / "started.json").exists() or (output / "start-once").exists() else "never-started",
                outcome=outcome, snapshots=snapshots, latest=latest, failures=list(failures.values()), diagnostics=diagnostics,
                integrityErrors=errors, dependencies=dependencies, liveStateChecked=False, credentialsValidated=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    f = commands.add_parser("freeze", help="offline validation and private, immutable input/source bundle")
    f.add_argument("--config", required=True)
    f.add_argument("--output", required=True)
    for name in ("observe", "diagnose", "report"):
        sub = commands.add_parser(name)
        sub.add_argument("--output", required=True)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        if args.command == "freeze":
            result = freeze(args.config, args.output)
        elif args.command in ("diagnose", "report"):
            result = diagnose(args.output)
        else:
            output = persistent(args.output)
            require(output.stat().st_uid == os.getuid() and output.stat().st_mode & 0o777 == 0o700,
                    "run directory must be current-user-owned mode 0700")
            inspection = diagnose(output)
            require(not inspection["integrityErrors"], "bundle integrity check failed")
            require(inspection["state"] == "never-started", "startup observation cannot resume/reuse a run")
            require(all(d["hashMatches"] for d in inspection["dependencies"]), "dependency missing/changed; freeze a new run")
            provenance = read(output / "bundle/provenance.json")
            require(provenance["output"] == str(output), "do not relocate frozen runs")
            require(Path(__file__).resolve() == output / "bundle/startup.py", "execute the frozen entrypoint, not mutable source")
            config = validate(read(output / "bundle/config.json"))
            persistent(config["managementKubeconfig"])
            # Exclusive start gate also protects two concurrent launchers.
            with (output / "start-once").open("x"):
                pass
            monitor = Monitor(config, output, Kubectl(config))
            for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
                signal.signal(sig, monitor.stop)
            result = monitor.run()
        print(json.dumps(result, sort_keys=True))
        return 0 if args.command != "observe" or result["outcome"] == "ready" else 2
    except (ValueError, OSError, KeyError) as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
