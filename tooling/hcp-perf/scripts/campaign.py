#!/usr/bin/env python3
"""Bounded personal-environment campaigns. Never provisions or qualifies a cluster."""
import argparse
import collections
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from decimal import Decimal
import re


LABEL = "hcp-perf.azure.com/run-id"
FINALIZATION_SECONDS = 1000
ARM_DRAIN_SECONDS = 360


def stamp():
    return datetime.now(timezone.utc).isoformat()


def epoch(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def read(path):
    return json.loads(Path(path).read_text())


def save(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_suffix(path.suffix + ".tmp")
    with temporary.open("w") as stream:
        json.dump(value, stream, indent=2, sort_keys=True)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)
    fd = os.open(path.parent, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def resolve_config_paths(spec, config, root):
    """Only documented filesystem fields; URLs, PromQL and resource names are not paths."""
    spec = json.loads(json.dumps(spec))
    base = Path(config).resolve().parent
    def path(value):
        require(isinstance(value, str) and value and not any(ord(c) < 32 for c in value)
                and not re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*://", value), "expected nonempty filesystem path")
        return str((base / Path(value).expanduser()).resolve())
    for key in ("guestKubeconfig", "managementKubeconfig", "lockFile"):
        spec[key] = path(spec[key])
    spec["seed"]["binary"]["path"] = path(spec["seed"]["binary"]["path"])
    capture = spec["capture"]
    capture["readyFile"] = path(capture["readyFile"])
    require(isinstance(capture["streamManifests"], list), "streamManifests must be an array")
    capture["streamManifests"] = [path(p) for p in capture["streamManifests"]]
    for entry in capture.get("guestStats", []):
        entry["output"] = path(entry["output"])
    for obj, key in ((spec, "record"), (spec, "retention"), (spec, "adversaries"),
                     (capture, "expectedTargets"), (capture, "finalQueries")):
        if isinstance(obj.get(key), str):
            obj[key] = path(obj[key])
        else:
            expected = list if key in ("expectedTargets", "finalQueries") else dict
            require(isinstance(obj.get(key), expected), "invalid input type: " + key)
    for key in ("pressureCPUQuery", "pressureBusyQuery"):
        file_key = key + "File"
        if file_key in spec:
            require(key not in spec, "choose query text or query file: " + key)
            spec[key] = Path(path(spec.pop(file_key))).read_text().strip()
            require(spec[key], "empty pressure query file")
    if spec.get("requireLimits") and isinstance(spec["requireLimits"]["inventory"], str):
        spec["requireLimits"]["inventory"] = path(spec["requireLimits"]["inventory"])
    if spec.get("adoptPrevious"):
        for key in ("run", "snapshot"):
            spec["adoptPrevious"][key] = path(spec["adoptPrevious"][key])
    if spec.get("cleanupPrevious"):
        previous = spec["cleanupPrevious"]
        if isinstance(previous, str):
            spec["cleanupPrevious"] = path(previous)
        else:
            previous["run"] = path(previous["run"])
            previous["inventory"] = path(previous["inventory"])
    if "durableStateRoot" in spec:
        spec["durableStateRoot"] = path(spec["durableStateRoot"])
        require(Path(root).resolve().is_relative_to(spec["durableStateRoot"]), "output outside durableStateRoot")
        for dependency in [str(Path(root).resolve()), spec["durableStateRoot"], *external_paths(spec)]:
            require(not any(Path(dependency).resolve().is_relative_to(p) for p in ("/tmp", "/var/tmp", "/run", "/dev/shm")),
                    "volatile durable dependency: " + dependency)
    return spec


def external_paths(spec):
    capture = spec["capture"]
    return [spec[k] for k in ("guestKubeconfig", "managementKubeconfig", "lockFile")] + [capture["readyFile"]] + capture["streamManifests"] + [
        str(Path(e["output"]) / "observations.jsonl") for e in capture.get("guestStats", [])]


def unit_definition(spec, root):
    require(re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]*", spec["unit"]) is not None, "invalid systemd unit name")
    def quote(value, command=False):
        value = str(value)
        require(not any(ord(c) < 32 for c in value), "control character in systemd value")
        value = value.replace('\\', '\\\\').replace('"', '\\"').replace('%', '%%')
        return '"' + (value.replace('$', '$$') if command else value) + '"'
    root = Path(root).resolve()
    return "\n".join([
        "[Unit]", "Description=Bounded start-once hcp-perf campaign", "[Service]", "Type=exec", "Restart=no",
        "WorkingDirectory=" + quote(root), "RuntimeMaxSec=" + str(spec["runtimeSeconds"]),
        "TimeoutStopSec=" + str(FINALIZATION_SECONDS), "KillMode=mixed", "UMask=0077",
        "Environment=" + quote("AZURE_CONFIG_DIR=" + str(Path.home() / ".azure-redhat")),
        "Environment=" + quote("PATH=" + os.environ["PATH"]),
        "ExecStart=" + " ".join(quote(x, command=True) for x in (sys.executable, "-B", root / "bundle/campaign.py", "run", "--output", root)), ""])


def recover_inspect(root):
    """Offline and read-only: a persisted PID/status is never proof of a live process."""
    root = Path(root)
    started, outcome = root / "started.json", root / "outcome.json"
    result = dict(qualified=False, liveState="unknown-not-queried", automaticResumeAllowed=False,
                  state="terminal-recorded" if outcome.exists() else "started-without-outcome-interrupted-or-unknown" if started.exists() else "not-started")
    result["outcome"] = read(outcome) if outcome.exists() else None
    manifest = root / "bundle/sha256.json"
    result["bundleIntegrity"] = {"missing": [], "changed": []}
    if manifest.exists():
        for name, expected in read(manifest).items():
            file = root / "bundle" / name
            require(file.resolve().is_relative_to((root / "bundle").resolve()), "unsafe frozen manifest path")
            if not file.is_file():
                result["bundleIntegrity"]["missing"].append(name)
            elif digest(file) != expected:
                result["bundleIntegrity"]["changed"].append(name)
    else:
        result["bundleIntegrity"]["missing"].append("sha256.json")
    config = root / "bundle/config.json"
    if config.exists():
        spec = read(config)
        result["dependencies"] = [dict(path=p, absolute=Path(p).is_absolute(),
             exists=Path(p).exists() if Path(p).is_absolute() else None) for p in external_paths(spec)]
        result["externalLifetimes"] = spec.get("externalLifetimes", [])
        result["credentialsValidity"] = "not-verified-offline"
    return result


def require(condition, reason):
    if not condition:
        raise RuntimeError(reason)


def retention_gate(spec, result):
    rows = result.get("resourceGroups", [])
    require(result.get("version") == "v1" and result.get("status") == "verified"
            and result.get("dryRun") is False and result.get("failure") is None,
            "retention result not verified")
    require(rows and all(r.get("status") == "verified" and r.get("attempted") is True
                        and r.get("updated") is True and r.get("verified") is True
                        and r.get("possiblyUpdated") is False and r.get("failure") is None for r in rows),
            "retention partial or ambiguous")
    require(sorted(r["id"].lower() for r in rows) == sorted(set(x.lower() for x in spec["resourceGroups"])),
            "retention exact-set mismatch")


def ready(obj):
    return not obj["metadata"].get("deletionTimestamp") and any(
        c["type"] == "Ready" and c["status"] == "True" for c in obj.get("status", {}).get("conditions", []))


def container_profile(spec):
    return {group: [{k: c.get(k) for k in ("name", "image", "resources", "restartPolicy")}
                    | {"runtimeEnv": [e for e in c.get("env", []) if e["name"] in ("GOMEMLIMIT", "GOMAXPROCS")]}
                    for c in spec.get(group, [])] for group in ("containers", "initContainers")}


def resource_lock(workloads):
    return sorted((o["kind"], o["metadata"]["name"], o["metadata"]["uid"],
                   json.dumps({"replicas": o["spec"].get("replicas"),
                               "containers": container_profile(o["spec"]["template"]["spec"])}, sort_keys=True))
                  for o in workloads)


def quantity(value):
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)([numkKMGTPE]|[KMGTPE]i)?", str(value))
    require(match is not None, "limits unsupported resource quantity: " + str(value))
    suffix = match[2] or ""
    scale = {"": 1, "n": Decimal("1e-9"), "u": Decimal("1e-6"), "m": Decimal(".001"), "k": 1000, "K": 1000}
    scale.update({s: 1000**i for i, s in enumerate("MGTPE", 2)})
    scale.update({s+"i": 1024**i for i, s in enumerate("KMGTPE", 1)})
    return Decimal(match[1]) * scale[suffix]


def limits_inventory(objects, policy):
    """Every object is controlled or an exact, profile-locked owner exemption."""
    profiles = policy["inventory"]
    require(profiles, "limits inventory empty")
    percent = Decimal(str(policy["memoryLimitPercent"]))
    tolerance = Decimal(str(policy.get("memoryRoundingBytes", 0)))
    require(percent.is_finite() and tolerance.is_finite() and percent >= 100 and tolerance >= 0, "invalid limits ratio/rounding")
    rows = []
    owners = {o["metadata"]["uid"]: o for o in objects if o["metadata"].get("uid")}
    for obj in objects:
        template = obj if obj["kind"] == "Pod" else (
            obj["spec"]["jobTemplate"]["spec"]["template"] if obj["kind"] == "CronJob" else obj["spec"]["template"])
        labels = template["metadata"].get("labels", {})
        matches = [p for p in profiles if p["selector"] and all(labels.get(k) == v for k, v in p["selector"].items())]
        owner = obj
        seen = set()
        while True:
            refs = [r for r in owner["metadata"].get("ownerReferences", []) if r.get("controller")]
            require(len(refs) <= 1, "limits ambiguous controller ownership")
            if not refs:
                break
            ref = refs[0]
            require(ref["uid"] not in seen, "limits ownership cycle")
            seen.add(ref["uid"])
            parent = owners.get(ref["uid"])
            if parent is None:
                break
            require(parent["kind"] == ref["kind"] and parent["metadata"]["name"] == ref["name"]
                    and parent["metadata"].get("namespace") == owner["metadata"].get("namespace"), "limits owner reference mismatch")
            owner = parent
        exemptions = [p for p in policy.get("ignoredOwners", []) if all(
            p["owner"].get(k) == (owner["kind"] if k == "kind" else owner["metadata"].get(k))
            for k in ("kind", "name", "namespace", "uid"))]
        if exemptions:
            require(len(exemptions) == 1 and not matches, "limits controlled/exempt overlap")
            exemption = exemptions[0]
            require(exemption.get("reason") and exemption["owner"].get("uid"), "limits exemption requires reason and UID")
            require(container_profile(template["spec"]) == container_profile(exemption)
                    and template["spec"].get("ephemeralContainers", []) == exemption.get("ephemeralContainers", []),
                    "limits exempt profile drift: " + obj["metadata"]["name"])
            rows.append(dict(kind=obj["kind"], name=obj["metadata"]["name"], uid=obj["metadata"].get("uid"),
                             scope="explicit-owner-exemption", owner=exemption["owner"], reason=exemption["reason"]))
            continue
        require(len(matches) == 1, "limits inventory requires one profile for " + obj["metadata"]["name"])
        profile = matches[0]
        actual = container_profile(template["spec"])
        actual["ephemeralContainers"] = container_profile({"containers": template["spec"].get("ephemeralContainers", [])})["containers"]
        expected = container_profile(profile)
        expected["ephemeralContainers"] = container_profile({"containers": profile.get("ephemeralContainers", [])})["containers"]
        for group in actual:
            for containers in (actual[group], expected[group]):
                for container in containers:
                    resources = container.get("resources") or {}
                    require(all(resource in resources.get(bound, {}) for bound in ("requests", "limits") for resource in ("cpu", "memory")),
                            "limits missing request/limit: " + obj["metadata"]["name"] + "/" + container["name"])
                    normalized = {bound: {resource: quantity(resources[bound][resource]) for resource in ("cpu", "memory")}
                                  for bound in ("requests", "limits")}
                    request, limit = normalized["requests"], normalized["limits"]
                    require(request["cpu"] > 0 and request["memory"] > 0 and limit["cpu"] == request["cpu"], "limits CPU must equal positive request")
                    require(limit["memory"] >= request["memory"] and abs(limit["memory"] - request["memory"] * percent / 100) <= tolerance,
                            "limits memory ratio mismatch")
                    container["resources"] = normalized
        require(actual == expected, "limits frozen profile drift: " + obj["metadata"]["name"])
        rows.append(dict(kind=obj["kind"], name=obj["metadata"]["name"], uid=obj["metadata"].get("uid"), profile=profile["name"], scope="controlled"))
    require(rows, "limits live inventory empty")
    return rows


def validate_campaign(spec):
    require(not spec.get("planningOnly"), "planning-only inputs cannot execute")
    integer = lambda value: type(value) is int
    require(spec["version"] == "v1" and spec["arms"] and integer(spec["runtimeSeconds"]) and 1 <= spec["runtimeSeconds"] <= 7200,
            "invalid bounded campaign")
    require(integer(spec["recoverySeconds"]) and 0 <= spec["recoverySeconds"] <= 600, "invalid recoverySeconds")
    require(len({a["name"] for a in spec["arms"]}) == len(spec["arms"]), "duplicate arms")
    for arm in spec["arms"]:
        seconds = arm["durationSeconds"]
        require(arm["name"].replace("-", "").isalnum() and integer(seconds) and 30 <= seconds <= 3600, "invalid arm name/duration")
        require(type(arm.get("holdAfterArm", False)) is bool, "holdAfterArm must be boolean")
        cadence = arm.get("canaryCadenceSeconds", spec["canaryCadenceSeconds"])
        require(integer(cadence) and 5 <= cadence <= seconds, "invalid canary cadence")
        require(integer(arm["applications"]) and 0 <= arm["applications"] <= 40000, "invalid applications")
        period = arm.get("turnoverPeriodSeconds", 3600)
        require(integer(period) and 1 <= period <= 86400, "invalid turnover period")
        require((arm["applications"] * seconds + period - 1) // period <= 80000, "turnover schedule cap")
        if "offeredQPS" in arm:
            rate = arm["offeredQPS"]
            require(integer(rate) and 1 <= rate <= 10000 and rate * seconds <= 1000000, "API offered-operation cap/rate")
    long = any(a["durationSeconds"] > 300 for a in spec["arms"])
    if long:
        require(integer(spec.get("setupBudgetSeconds")) and spec["setupBudgetSeconds"] > 0, "long arms require explicit setupBudgetSeconds")
        require(spec["setupBudgetSeconds"] + sum(a["durationSeconds"] + spec["recoverySeconds"] for a in spec["arms"])
                + ARM_DRAIN_SECONDS <= spec["runtimeSeconds"], "campaign runtime cannot cover setup/work/drain")
        for kind in ("record", "adversaries"):
            if kind == "adversaries" and not any(a.get("contended") for a in spec["arms"]):
                continue
            require(integer(spec[kind]["durationSeconds"]) and spec["runtimeSeconds"] <= spec[kind]["durationSeconds"] <= 86400,
                    "long campaign requires full-runtime " + kind + " lifetime")
        names = {x["name"] for x in spec.get("externalLifetimes", [])}
        require({"guest-access", "management-access", "capture-forward"} <= names, "long campaign requires external access/forward lifetimes")
    if spec.get("requireLimits"):
        require(isinstance(spec["requireLimits"], dict), "requireLimits must contain explicit inventory and ratio")
        require(spec["requireLimits"]["inventory"], "limits inventory empty")
        policy = spec["requireLimits"]
        exemptions = policy.get("ignoredOwners", [])
        require(all(p.get("reason") and all(p.get("owner", {}).get(k) for k in ("kind", "name", "namespace", "uid"))
                    and p.get("containers") for p in exemptions), "limits exemptions require exact owner and frozen containers")
        require(len({p["owner"]["uid"] for p in exemptions}) == len(exemptions), "limits duplicate ignored owner")
        fixtures = [dict(kind="Pod", metadata=dict(name=p["name"], labels=p["selector"]), spec=p) for p in policy["inventory"]]
        limits_inventory(fixtures, policy)


def lifetime_gate(expirations, now, remaining):
    for name, expires in expirations.items():
        require(math.isfinite(expires) and expires > now + remaining, "insufficient lifetime: " + name)


def job_lifetime_gate(jobs, now, remaining):
    require(jobs, "missing owned Jobs for lifetime gate")
    for job in jobs:
        spec, status = job["spec"], job.get("status", {})
        require(not spec.get("suspend") and status.get("startTime") and type(spec.get("activeDeadlineSeconds")) is int,
                "Job missing active lifetime")
        require(not any(c["type"] in ("Failed", "Complete") and c["status"] == "True" for c in status.get("conditions", [])),
                "Job already terminal")
        lifetime_gate({job["metadata"]["name"]: epoch(status["startTime"])+spec["activeDeadlineSeconds"]}, now, remaining)


def turnover_expected(arm):
    period = arm.get("turnoverPeriodSeconds", 3600)
    return (arm["applications"] * arm["durationSeconds"] + period - 1) // period


def retained_gate(previous, live):
    require(previous["outcome"]["execution"] == "completed" and not previous["outcome"]["activeLocalChildren"]
            and not previous["outcome"]["finalizationErrors"], "retained predecessor not completed/stopped")
    for kind in ("pods", "deployments", "apf"):
        identity = lambda objects: {o["metadata"]["uid"]: o["spec"] for o in objects}
        require(identity(previous[kind]) == identity(live[kind]), "retained UID/spec/template mismatch: " + kind)
    require(all(ready(p) for p in live["pods"]), "retained Pods not Ready")
    old = {n["metadata"]["uid"]: (n["metadata"]["name"], n["status"]["allocatable"]) for n in previous["nodes"]}
    new = {n["metadata"]["uid"]: (n["metadata"]["name"], n["status"]["allocatable"]) for n in live["nodes"]}
    require(len(new) > len(old) and all(new.get(k) == v for k, v in old.items()) and all(ready(n) for n in live["nodes"]),
            "retained control requires additive Ready nodes only")
    require(previous["resourceLock"] == [list(x) for x in live["resourceLock"]], "retained HCP resource drift")
    require(previous["managementNodeUIDs"] == sorted(n["metadata"]["uid"] for n in live["managementNodes"]), "management node change")
    require(previous["cscSpec"] == live["cscSpec"], "retained sizing policy drift")


def api_namespaces(seed, arm):
    count = arm.get("apiIdentities", math.ceil(seed["applications"] / 20))
    require(isinstance(count, int) and 1 <= count <= math.ceil(seed["applications"] / 20), "invalid API identity count")
    return [seed["namespacePrefix"] + "-" + str(i) for i in range(count)]


class Safety:
    """Monotonic timers; restart increments survive replacement Pod UIDs."""
    def __init__(self, pods, nodes, selectors, grace=45, restart_limit=3):
        self.nodes = {(n["metadata"]["name"], n["metadata"]["uid"]) for n in nodes}
        self.selectors, self.grace, self.restart_limit = selectors, grace, restart_limit
        self.counts, self.ooms, self.increments, self.since = {}, set(), [], {}
        self.observe(pods, nodes, 0, baseline=True)

    def observe(self, pods, nodes, now, baseline=False):
        failures, violations = [], []
        if {(n["metadata"]["name"], n["metadata"]["uid"]) for n in nodes} != self.nodes:
            failures.append("node identity set changed")
        for n in nodes:
            if not ready(n) or n.get("spec", {}).get("unschedulable") or any(
                    c["type"] in ("MemoryPressure", "DiskPressure", "PIDPressure") and c["status"] != "False"
                    for c in n.get("status", {}).get("conditions", [])):
                failures.append("unsafe node: " + n["metadata"]["name"])
        for p in pods:
            if p.get("status", {}).get("phase") != "Succeeded" and not ready(p):
                violations.append("nonReady: " + p["metadata"]["name"])
            for s in (p.get("status", {}).get("containerStatuses", []) + p.get("status", {}).get("initContainerStatuses", [])
                      + p.get("status", {}).get("ephemeralContainerStatuses", [])):
                key = (p["metadata"]["uid"], s["name"])
                count = s.get("restartCount", 0)
                delta = max(0, count - self.counts.get(key, 0))
                if not baseline and delta:
                    self.increments.append((now, delta))
                    violations.append("restart: " + p["metadata"]["name"] + "/" + s["name"])
                self.counts[key] = count
                for state in ("state", "lastState"):
                    term = s.get(state, {}).get("terminated", {})
                    if term.get("reason") == "OOMKilled":
                        event = key + (term.get("finishedAt"),)
                        if event not in self.ooms and not baseline:
                            failures.append("new OOM: " + p["metadata"]["name"] + "/" + s["name"])
                        self.ooms.add(event)
        self.increments = [(at, n) for at, n in self.increments if now - at <= 120]
        if sum(n for _, n in self.increments) >= self.restart_limit:
            failures.append("restart storm in 120s")
        for name, config in self.selectors.items():
            selected = [p for p in pods if all(p["metadata"].get("labels", {}).get(k) == v
                                              for k, v in config["labels"].items())]
            healthy = sum(ready(p) for p in selected)
            if healthy < config["minimumReady"]:
                self.since.setdefault(name, now)
                violations.append(name + " below protected minimum")
                if now - self.since[name] >= self.grace:
                    failures.append(name + " unavailable beyond grace")
            else:
                self.since.pop(name, None)
        return violations, failures


def target_key(target):
    return json.dumps({k: target[k] for k in ("labels", "scrapeUrl", "scrapePool")}, sort_keys=True)


class CaptureHealth:
    """Source-time loss, separate from admission confirmation and observed coverage gaps."""
    def __init__(self, expected, loss_seconds):
        self.expected = {target_key(t): t for t in expected}
        require(self.expected and len(self.expected) == len(expected), "empty/duplicate capture identities")
        self.loss_seconds = loss_seconds
        self.started = None
        self.success, self.confirmed, self.floor, self.gaps, self.failed = {}, {}, {}, {}, set()
        self.labels = collections.Counter(json.dumps(t["labels"], sort_keys=True) for t in expected)

    def observe(self, targets, now, wall, history=(), timestamps=()):
        if self.started is None:
            self.started = wall
        current = {target_key(t): t for t in targets}
        bad, recovered, evidence = {}, [], []
        invalid = set(current) != set(self.expected) or len(current) != len(targets)
        if invalid:
            bad["identity-set"] = "unexpected or duplicate target identity"
            for key in self.expected:
                self.confirmed[key] = set()
                self.floor[key] = wall
        label_key = lambda row: json.dumps({k: v for k, v in row["metric"].items() if k != "__name__"}, sort_keys=True)
        sources = {label_key(r): dict(r["values"]) for r in timestamps}
        raw = {}
        for row in history:
            label = label_key(row)
            samples = {}
            for evaluation, value in row["values"]:
                source = sources.get(label, {}).get(evaluation)
                if source is not None:
                    try:
                        source, value = float(source), float(value)
                    except (TypeError, ValueError):
                        continue
                    # Preserve the freshest witness; later lookback replay must not overwrite it.
                    if (math.isfinite(source) and math.isfinite(evaluation) and value in (0, 1)
                            and source <= evaluation <= wall
                            and (source not in samples or evaluation < samples[source][1])):
                        samples[source] = (value, evaluation)
            raw[label] = samples
        for key, expected in self.expected.items():
            target = current.get(key)
            reason, source = None, None
            interval, timeout = [float(expected[k].removesuffix("s")) for k in ("scrapeInterval", "scrapeTimeout")]
            fresh_age = 2 * interval + timeout
            label = json.dumps(expected["labels"], sort_keys=True)
            events = {}
            if not invalid and self.labels[label] == 1:
                events = {s: value for s, (value, evaluation) in raw.get(label, {}).items()
                          if 0 <= evaluation-s < fresh_age and self.started-self.loss_seconds <= s <= wall}
            if target is None:
                reason = "missing"
            else:
                try:
                    require(all(target[k] == expected[k] for k in ("scrapeInterval", "scrapeTimeout")), "scrape timing drift")
                    source = epoch(target["lastScrape"])
                    require(0 <= wall - source < fresh_age, "stale or future scrape")
                    require(target["health"] == "up" and not target.get("lastError"), "down")
                except (ValueError, KeyError, RuntimeError) as error:
                    reason = str(error)
                if not invalid and source is not None and 0 <= wall-source < fresh_age:
                    # Alias labels cannot identify a scheduler path. Only this exact target API can.
                    # Millisecond normalization also avoids counting its raw-history copy twice.
                    events[round(source, 3)] = 1 if reason is None else 0
            last = self.success.get(key, self.started)
            if key not in self.success:
                initial = [s for s, value in events.items() if value == 1 and 0 <= wall-s < fresh_age]
                if initial:
                    last = max(initial)
            good = self.confirmed.setdefault(key, set())
            for at, value in sorted(events.items()):
                if value != 1:
                    evidence.append(dict(identity=key, sourceTime=at, value=value))
                    good.difference_update(s for s in tuple(good) if s <= at)
                    self.floor[key] = max(self.floor.get(key, self.started), at)
                    continue
                if at > last:
                    # Detect an expired loss interval before recovery can erase it.
                    if at-last >= self.loss_seconds:
                        self.failed.add(key)
                    last = at
                if at >= self.floor.get(key, self.started) and 0 <= wall-at < fresh_age:
                    good.add(round(at, 3))
            self.success[key] = last
            good.intersection_update(s for s in good if 0 <= wall-s < fresh_age)
            if wall-last >= self.loss_seconds:
                self.failed.add(key)
            if reason or invalid:
                good.clear()
                self.floor[key] = wall
            if reason:
                bad[key] = reason
        for key, began in list(self.gaps.items()):
            if key not in bad:
                recovered.append(dict(identity=key, observedGapSeconds=now-began))
                del self.gaps[key]
        for key in bad:
            self.gaps.setdefault(key, now)
        return dict(admissible=not bad and not self.failed and all(len(self.confirmed[k]) >= 2 for k in self.expected),
                    gaps=[dict(identity=k, reason=v, durationSeconds=now-self.gaps[k]) for k, v in bad.items()],
                    sourceFailures=evidence, recovered=recovered, sustained=sorted(self.failed),
                    secondsSinceFreshSuccess={k: wall-v for k, v in self.success.items()},
                    aliasHistoryNotAttributed=[k for k, t in self.expected.items() if self.labels[json.dumps(t["labels"], sort_keys=True)] > 1])


class Runner:
    def __init__(self, root):
        self.root = Path(root)
        self.config = read(self.root / "bundle/config.json")
        self.cli = self.root / "bundle/hcp-perf"
        self.halt, self.guard_done = threading.Event(), threading.Event()
        self.children, self.counter, self.mutex = [], 0, threading.Lock()
        self.reason, self.finalizing = None, False
        self.session_expirations = {}
        self.env = dict(os.environ, AZURE_CONFIG_DIR=str(Path.home() / ".azure-redhat"))
        self.deadline = time.monotonic() + self.config["runtimeSeconds"]

    def phase(self, name, **kwargs):
        save(self.root / "status.json", dict(at=stamp(), phase=name, qualified=False, **kwargs))

    def progression_hold(self, arm):
        if not arm.get("holdAfterArm"):
            return False
        save(self.root / "progression-hold.json", dict(at=stamp(), completedArm=arm["name"],
             reason="explicit review of useful progress, capture and budget before increasing treatment", qualified=False))
        return True

    def check(self):
        if self.halt.is_set() or time.monotonic() >= self.deadline:
            raise RuntimeError(self.reason or "campaign stop requested or runtime exhausted")

    def start(self, args, label):
        if not self.finalizing:
            self.check()
        with self.mutex:
            self.counter += 1
            path = self.root / "commands" / (f"{self.counter:05d}-" + label)
        path.parent.mkdir(exist_ok=True, mode=0o700)
        save(str(path) + ".command.json", dict(at=stamp(), argv=list(map(str, args))))
        with Path(str(path) + ".json").open("xb") as out, Path(str(path) + ".stderr").open("xb") as err:
            process = subprocess.Popen(list(map(str, args)), stdout=out, stderr=err, env=self.env,
                                       start_new_session=True)
        self.children.append((process, path))
        return process, path

    @staticmethod
    def terminate(process, grace=130):
        if process.poll() is not None:
            return process.returncode
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            return process.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            return process.wait(timeout=10)

    def wait(self, child, bound, strict=True):
        process, path = child
        deadline = time.monotonic() + bound
        interrupted = False
        while process.poll() is None:
            if time.monotonic() >= deadline or (not self.finalizing and (self.halt.is_set() or time.monotonic() >= self.deadline)):
                interrupted = True
                self.terminate(process)
                break
            time.sleep(.2)
        code = process.wait()
        save(str(path) + ".exit.json", dict(at=stamp(), exitCode=code, interrupted=interrupted))
        if strict:
            require(code == 0 and not interrupted, "command did not complete: " + path.name)
        try:
            value = read(str(path) + ".json")
        except (ValueError, OSError):
            value = None
        return value

    def run(self, args, label, bound=60, strict=True):
        return self.wait(self.start(args, label), bound, strict)

    def perf(self, args, label, bound=90, strict=True):
        return self.run([self.cli, *args], label, bound, strict)

    def kube(self, cluster, args, label, bound=45):
        return self.run(["kubectl", "--kubeconfig", self.config[cluster + "Kubeconfig"],
                         "--request-timeout=30s", *args], label, bound)

    def get(self, cluster, resource, namespace=None, extra=()):
        return self.kube(cluster, ["get", resource, *( ["-n", namespace] if namespace else ["-A"]),
                                   *extra, "-o", "json"], cluster + "-" + resource.replace(",", "-"))

    def pause(self, seconds):
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            self.check()
            time.sleep(min(.5, until - time.monotonic()))

    def query(self, expr):
        url = self.config["capture"]["endpoint"] + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
        with urllib.request.urlopen(url, timeout=20) as response:
            value = json.load(response)
        require(value.get("status") == "success", "Prometheus query failed")
        return value["data"]["result"]

    def targets(self):
        with urllib.request.urlopen(self.config["capture"]["endpoint"] + "/api/v1/targets", timeout=10) as response:
            value = json.load(response)
        require(value.get("status") == "success", "capture target API unavailable")
        return value["data"]["activeTargets"]

    def capture_sample(self):
        sample = dict(at=stamp(), targets=[], history=[], timestamps=[], error=None)
        end = time.time()
        try:
            for field, expression in (("history", "up"), ("timestamps", "timestamp(up)")):
                args = urllib.parse.urlencode(dict(query=expression, start=end-120, end=end, step=1, timeout="5s"))
                with urllib.request.urlopen(self.config["capture"]["endpoint"] + "/api/v1/query_range?" + args, timeout=8) as response:
                    data = response.read(8*1024*1024+1)
                require(len(data) <= 8*1024*1024, "capture history response bound")
                value = json.loads(data)
                require(value.get("status") == "success", "capture history query failed")
                sample[field] = value["data"]["result"]
            sample["targets"] = self.targets()
        except Exception as error:
            sample["error"] = str(error)
        return sample

    def retention(self, arm):
        spec = self.config["retention"]
        value = self.perf(["environment", "refresh", "--input", self.root / "bundle/retention.json",
                           "--timeout", "5m"], "retention-" + arm, 360)
        retention_gate(spec, value)
        save(self.root / "retention" / (arm + ".json"), value)

    def observations_ready(self, label="initial"):
        c = self.config["capture"]
        begin, attempt, status, failure = time.monotonic(), 0, "interrupted", None
        directory = self.root / "capture-gates" / label
        try:
            require(Path(c["readyFile"]).is_file(), "capture owner's readiness handoff missing")
            handoff = read(c["readyFile"])
            require(handoff.get("captureReady") is True and handoff.get("endpoint") == c["endpoint"], "capture owner has not declared matching readiness")
            require(epoch(handoff["tokenExpiresAt"]) > time.time() + 1800, "capture token lifetime")
            remaining = max(0, self.deadline-time.monotonic()) + FINALIZATION_SECONDS
            expirations = {x["name"]: epoch(x["validUntil"]) for x in self.config.get("externalLifetimes", [])}
            if any(a["durationSeconds"] > 300 for a in self.config.get("arms", [])):
                expirations["observer-token"] = epoch(handoff["tokenExpiresAt"])
            for path in c["streamManifests"]:
                value = read(path)
                require(value["status"] == "running" and value["completedChunks"] > 0
                        and 0 <= time.time()-epoch(value["updated"]) < 180, "capture stream not fresh: " + path)
                require(epoch(value["end"]) > time.time()+600, "capture stream nearing deadline")
                if any(a["durationSeconds"] > 300 for a in self.config.get("arms", [])):
                    expirations[path] = epoch(value["end"])
            lifetime_gate(expirations, time.time(), remaining)
            require(c["streamManifests"], "no external streams supplied")
            for entry in c.get("guestStats", []):
                value = self.run(["jq", "-ne", "-f", self.root / "bundle/source/scripts/check-guest-stats.jq",
                                  Path(entry["output"]) / "observations.jsonl"], "guest-stats-fresh", 20)
                require(value and value["fresh"] and value["node"] == entry["node"], "guest stats freshness/identity")
            health = CaptureHealth(c["expectedTargets"], c["lossSeconds"])
            while True:
                self.check()
                attempt += 1
                sample = self.capture_sample()
                result = health.observe(sample["targets"], time.monotonic(), time.time(), sample["history"], sample["timestamps"])
                save(directory / f"{attempt:04d}.json", dict(sample, elapsedSeconds=time.monotonic()-begin, **result))
                self.check()
                require(time.monotonic()-begin < c["gateSeconds"], "capture recovery gate deadline")
                require(not result["sustained"], "sustained capture loss before arm admission")
                if result["admissible"] and not sample["error"]:
                    status = "recovered-ready"
                    return c["expectedTargets"]
                require(time.monotonic()-begin < c["gateSeconds"], "capture recovery gate deadline")
                self.pause(c["pollSeconds"])
        except Exception as error:
            failure = str(error)
            status = "interrupted" if self.halt.is_set() else "blocked"
            raise
        finally:
            save(directory / "outcome.json", dict(at=stamp(), status=status, attempts=attempt, error=failure,
                 elapsedSeconds=time.monotonic()-begin, qualified=False, coverage="gaps retained; not completeness qualification"))

    def guard(self, baseline, safety, jobs):
        unavailable, iteration = {}, 0
        capture = CaptureHealth(jobs, self.config["capture"]["lossSeconds"])
        while not self.guard_done.is_set():
            iteration += 1
            now = time.monotonic()
            violations, failures = [], []
            try:
                pods = self.get("management", "pods", self.config["hcpNamespace"])["items"]
                nodes = self.get("management", "nodes")["items"] + self.get("guest", "nodes")["items"]
                violations, failures = safety.observe(pods, nodes, now)
                workloads = self.get("management", "deployments,statefulsets,daemonsets", self.config["hcpNamespace"])["items"]
                require(resource_lock(workloads) == baseline, "HCP resource/replica/image/runtime drift")
                if self.config.get("requireLimits"):
                    all_workloads = self.get("management", "deployments,statefulsets,daemonsets,replicasets,jobs,cronjobs", self.config["hcpNamespace"])["items"]
                    coverage = limits_inventory(pods + all_workloads, self.config["requireLimits"])
                    save(self.root / "limits" / f"{iteration:05d}.json", dict(at=stamp(), coverage=coverage))
                require(shutil.disk_usage(self.root).free >= self.config["minimumDiskFreeBytes"], "local disk headroom")
                sample = self.capture_sample()
                capture_result = capture.observe(sample["targets"], time.monotonic(), time.time(), sample["history"], sample["timestamps"])
                save(self.root / "capture-guard" / f"{iteration:05d}.json", dict(sample, **capture_result))
                failures += ["sustained capture target loss: " + k for k in capture_result["sustained"]]
                for path in self.config["capture"]["streamManifests"]:
                    value = read(path)
                    if value["status"] == "running" and time.time() - epoch(value["updated"]) < 180:
                        unavailable.pop(path, None)
                    else:
                        unavailable.setdefault(path, now)
                unavailable.pop("guard-observation", None)
            except Exception as error:
                violations.append(str(error))
                if "drift" in str(error) or "disk headroom" in str(error) or "limits" in str(error):
                    failures.append(str(error))
                unavailable.setdefault("guard-observation", now)
            failures += ["observation unavailable 60s: " + k for k, at in unavailable.items() if now - at >= 60]
            save(self.root / "safety" / f"{iteration:05d}.json", dict(at=stamp(), violations=violations,
                                                                     failures=failures, unavailable=unavailable))
            if failures:
                self.reason = "safety_stop"
                save(self.root / "safety-stop.json", dict(at=stamp(), failures=failures))
                self.halt.set()
                return
            self.guard_done.wait(self.config["guardIntervalSeconds"])

    def session(self, kind, action, spec):
        args = [kind, action, "--kubeconfig", self.config["managementKubeconfig"], "--timeout", "1m"]
        if action == "start":
            path = self.root / "resolved" / (kind + ".json")
            save(path, spec)
            args += ["--input", path]
            # Submission time is conservative relative to remote process startup.
            self.session_expirations[kind] = time.time() + spec["durationSeconds"]
        else:
            args += ["--namespace", spec["namespace"], "--id", spec["id"]]
        return self.perf(args, kind + "-" + action)

    def session_ready(self, kind, spec):
        until = time.monotonic() + 240
        while time.monotonic() < until:
            value = self.session(kind, "status", spec)
            if kind == "record":
                healthy = value["phase"] == "running" and any(p.get("ready") and not p.get("terminating") for p in value.get("pods", []))
            else:
                healthy = (value["phase"] == "running" and not value["submissionIncomplete"]
                           and value["nonterminalPods"] > 0 and value["placementValid"]
                           and all(j["phase"] == "running" and j.get("pods")
                                   and all(p["ready"] and not p["deleting"] for p in j["pods"]) for j in value["jobs"]))
            if healthy:
                return value
            self.pause(5)
        raise RuntimeError(kind + " readiness deadline")

    def canary(self, directory, name, credentials, expected):
        args = ["canary", "run", "--namespace", self.canary_namespace, "--deployment", "canary",
                "--kubeconfig", credentials, "--timeout", "5m", "--ack-budget", "30s", "--ready-budget", "120s",
                "--cleanup-budget", "120s", "--request-log", directory / (name + ".requests.jsonl")]
        for stage, value in (expected or {}).items():
            args += ["--" + stage + "-expected", str(value) + "ns"]
        result = self.perf(args, name, 360)
        save(directory / (name + ".json"), result)
        return result

    def population(self, arm, directory):
        c, apps = self.config, arm["applications"]
        run_id = c["id"] + "-" + arm["name"]
        seed = dict(c["seed"], id=run_id, namespacePrefix=run_id, applications=apps,
                    guestNodes=c["guestNodes"], binary=dict(c["seed"]["binary"], path=str(self.root / "bundle/kube-burner")))
        save(directory / "seed.json", seed)
        require(apps * 5 <= c["guestNodes"] * 200, "application reference ceiling exceeded")
        nodes = self.get("guest", "nodes")["items"]
        pods = self.get("guest", "pods")["items"]
        free = sum(int(n["status"]["allocatable"]["pods"]) for n in nodes) - sum(
            p.get("status", {}).get("phase") not in ("Succeeded", "Failed") for p in pods)
        require(free >= apps * 5 + c["maxOutstanding"] + 1, "insufficient aggregate Pod slots")
        self.perf(["seed", "run", "--input", directory / "seed.json", "--kubeconfig", c["guestKubeconfig"],
                   "--timeout", str(seed["timeoutSeconds"]) + "s", "--output", directory / "seed"], "seed", seed["timeoutSeconds"] + 30)
        until = time.monotonic() + 300
        while True:
            deployments = self.get("guest", "deployments", extra=["-l", LABEL + "=" + run_id])["items"]
            app_pods = self.get("guest", "pods", extra=["-l", LABEL + "=" + run_id])["items"]
            if len(deployments) == apps and all(d["status"].get("observedGeneration") == d["metadata"]["generation"]
                    and all(d["status"].get(k, 0) == 5 for k in ("updatedReplicas", "readyReplicas", "availableReplicas")) for d in deployments
                    ) and len(app_pods) == apps * 5 and all(ready(p) for p in app_pods):
                break
            require(time.monotonic() < until, "seed convergence deadline")
            self.pause(5)
        self.perf(["seed", "identities", "--input", directory / "seed.json", "--kubeconfig", c["guestKubeconfig"], "--timeout", "3m"], "identities", 200)
        creds = self.perf(["seed", "credentials", "--input", directory / "seed.json", "--kubeconfig", c["guestKubeconfig"],
                           "--timeout", "3m", "--expiration", "3h", "--output", directory / "credentials"], "credentials", 200)
        setup = {k: seed[k] for k in ("version", "id", "namespacePrefix", "guestNodes", "applications", "image")}
        setup["admission"] = c["admission"]
        save(directory / "setup.json", setup)
        fixture = self.perf(["workload", "setup", "--input", directory / "setup.json", "--kubeconfig", c["guestKubeconfig"], "--timeout", "3m"], "setup", 200)
        save(directory / "fixture-uids.json", fixture)
        canary_creds = self.perf(["workload", "credentials", "--input", directory / "setup.json", "--kubeconfig", c["guestKubeconfig"],
                                  "--expiration", "3h", "--timeout", "3m", "--output", directory / "canary.kubeconfig"], "canary-credentials", 200)
        require(all(epoch(x["validUntil"]) > time.time() + max(3600, self.deadline-time.monotonic()+ARM_DRAIN_SECONDS)
                    for x in creds["files"] + [canary_creds]), "credential lifetime")
        save(directory / "credential-provenance.json", dict(benchmark=creds, canary=canary_creds,
             hashes={str(p.relative_to(directory)): digest(p) for p in directory.rglob("*.kubeconfig")}))
        self.canary_namespace = fixture["canaryNamespace"]
        owned = self.get("guest", "namespaces,flowschemas,prioritylevelconfigurations", extra=["-l", LABEL + "=" + run_id])["items"]
        save(directory / "owned.json", owned)
        require(len([o for o in owned if o["kind"] == "Namespace"]) == math.ceil(apps / 20) + 1, "owned namespace inventory incomplete")
        all_pods = self.get("guest", "pods")["items"]
        slots = {n["metadata"]["name"]: int(n["status"]["allocatable"]["pods"]) - sum(
                 p["spec"].get("nodeName") == n["metadata"]["name"] and p.get("status", {}).get("phase") not in ("Succeeded", "Failed")
                 for p in all_pods) for n in nodes}
        save(directory / "guest-slots.json", slots)
        require(min(slots.values()) >= 2 and sum(slots.values()) >= c["maxOutstanding"] + 1, "per-node/aggregate surge slots exhausted")
        apf = self.get("guest", "flowschemas,prioritylevelconfigurations")["items"]
        save(directory / "apf.json", apf)
        for profile in ("benchmark", "canary"):
            plc = next(o for o in apf if o["kind"] == "PriorityLevelConfiguration" and o["metadata"]["name"] == "hcp-perf-" + profile + "-" + run_id)
            effective = plc["spec"]["limited"]
            require(effective["nominalConcurrencyShares"] == c["admission"][profile]["nominalConcurrencyShares"], "APF shares mismatch")
            require(effective["limitResponse"]["type"] == ("Queue" if profile == "canary" else "Reject"), "APF response mismatch")
        return seed

    def cleanup_population(self, directory):
        # Only exact acknowledged run-owned Namespace/APF UIDs. Never PVCs or broad prefixes.
        for obj in read(directory / "owned.json"):
            kind, meta = obj["kind"], obj["metadata"]
            require(meta.get("labels", {}).get(LABEL) == read(directory / "seed.json")["id"], "cleanup ownership mismatch")
            if kind == "Namespace":
                pvcs = self.get("guest", "persistentvolumeclaims", meta["name"])["items"]
                require(not pvcs, "refusing namespace cleanup containing PVCs")
                uri = "/api/v1/namespaces/" + meta["name"]
            else:
                require(kind in ("FlowSchema", "PriorityLevelConfiguration"), "cleanup kind not allowed")
                uri = "/apis/flowcontrol.apiserver.k8s.io/v1/" + {"FlowSchema": "flowschemas", "PriorityLevelConfiguration": "prioritylevelconfigurations"}[kind] + "/" + meta["name"]
            options = directory / ("delete-" + meta["uid"] + ".json")
            save(options, dict(apiVersion="v1", kind="DeleteOptions", preconditions=dict(uid=meta["uid"]), propagationPolicy="Foreground"))
            self.kube("guest", ["delete", "--raw", uri, "-f", options], "uid-delete")
        until = time.monotonic() + 180
        run_id = read(directory / "seed.json")["id"]
        while self.get("guest", "namespaces,flowschemas,prioritylevelconfigurations", extra=["-l", LABEL + "=" + run_id])["items"]:
            require(time.monotonic() < until, "owned cleanup convergence deadline")
            self.pause(5)
        save(directory / "cleanup.json", dict(at=stamp(), remainingUIDs=[]))

    def adopt_population(self, directory, previous):
        seed = dict(previous["seed"], guestNodes=self.config["guestNodes"])
        selector = ["-l", LABEL + "=" + seed["id"]]
        live = dict(pods=self.get("guest", "pods", extra=selector)["items"],
                    deployments=self.get("guest", "deployments", extra=selector)["items"],
                    apf=self.get("guest", "flowschemas,prioritylevelconfigurations")["items"],
                    nodes=self.get("guest", "nodes")["items"], managementNodes=self.get("management", "nodes")["items"],
                    resourceLock=resource_lock(self.get("management", "deployments,statefulsets,daemonsets", self.config["hcpNamespace"])["items"]),
                    cscSpec=self.get("management", "clustersizingconfiguration", extra=["cluster"])["spec"])
        save(directory / "adoption-live.json", live)
        retained_gate(previous, live)
        owned = self.get("guest", "namespaces,flowschemas,prioritylevelconfigurations", extra=selector)["items"]
        require({o["metadata"]["uid"] for o in owned} == {o["metadata"]["uid"] for o in previous["owned"]}, "retained owned UID set changed")
        save(directory / "owned.json", owned)
        save(directory / "seed.json", seed)
        setup = dict(previous["setup"], guestNodes=self.config["guestNodes"])
        save(directory / "setup.json", setup)
        self.canary_namespace = "hcp-perf-canary-" + seed["id"]
        save(directory / "apf.json", live["apf"])
        for kind, args in (("benchmark", ["seed", "credentials", "--input", directory / "seed.json", "--output", directory / "credentials"]),
                           ("canary", ["workload", "credentials", "--input", directory / "setup.json", "--output", directory / "canary.kubeconfig"])):
            receipt = self.perf([*args, "--kubeconfig", self.config["guestKubeconfig"], "--expiration", "3h", "--timeout", "3m"], "adopt-credentials", 200)
            require(all(epoch(x["validUntil"]) > time.time() + self.config["runtimeSeconds"] for x in receipt.get("files", [receipt])), "retained token lifetime")
            save(directory / (kind + "-credentials.json"), receipt)
        distribution = collections.Counter(p["spec"]["nodeName"] for p in live["pods"])
        save(directory / "adoption.json", dict(at=stamp(), retainedPodUIDs=len(live["pods"]),
             distribution={n["metadata"]["name"]: distribution[n["metadata"]["name"]] for n in live["nodes"]},
             predecessor=previous["source"], reseeded=False, onlyTreatment="guest node expansion; fresh observer/baseline/CPU session"))
        return seed

    def measure(self, arm, directory, population, seed, expected):
        c, seconds = self.config, arm["durationSeconds"]
        cadence_seconds = arm.get("canaryCadenceSeconds", c["canaryCadenceSeconds"])
        remaining = seconds + c["recoverySeconds"] + ARM_DRAIN_SECONDS
        self.check()
        require(time.monotonic() + remaining < self.deadline, "arm exceeds campaign hard deadline")
        if seconds > 300 or c.get("externalLifetimes"):
            expiration = dict(self.session_expirations)
            metadata = population / "credential-provenance.json"
            if metadata.exists():
                credentials = read(metadata)
                files = credentials["benchmark"]["files"] + [credentials["canary"]]
            else:
                files = read(population / "benchmark-credentials.json")["files"] + [read(population / "canary-credentials.json")]
            expiration.update({"credential-"+str(i): epoch(x["validUntil"]) for i, x in enumerate(files)})
            require("record" in expiration and (not arm.get("contended") or "adversaries" in expiration), "missing session lifetime")
            lifetime_gate(expiration, time.time(), remaining)
            for kind in self.session_expirations:
                spec = c[kind]
                jobs = self.get("management", "jobs", spec["namespace"], ["-l", LABEL + "=" + spec["id"]])["items"]
                job_lifetime_gate(jobs, time.time(), remaining)
            save(directory / "lifetime-gate.json", dict(at=stamp(), requiredSeconds=remaining, expirations=expiration))
            lifetime_gate(expiration, time.time(), remaining)
        self.check()
        require(time.monotonic() + remaining < self.deadline, "arm exceeds campaign hard deadline")
        lifetime_gate({x["name"]: epoch(x["validUntil"]) for x in c.get("externalLifetimes", [])},
                      time.time(), remaining + FINALIZATION_SECONDS)
        turnover = dict(version="v1", namespacePrefix=seed["namespacePrefix"], guestNodes=c["guestNodes"], applications=seed["applications"],
                        durationSeconds=seconds, recoverySeconds=c["recoverySeconds"], turnoverPeriodSeconds=arm.get("turnoverPeriodSeconds", 3600), maxOutstanding=c["maxOutstanding"], seed=42)
        save(directory / "turnover.json", turnover)
        api = None
        if arm.get("offeredQPS"):
            namespaces = api_namespaces(seed, arm)
            targets = []
            for i, ns in enumerate(namespaces):
                targets += [dict(namespace=ns, apiVersion="apps/v1", resource="deployments", verb="get", name=f"app-{i*20:06d}", weight=2),
                            dict(namespace=ns, apiVersion="v1", resource="pods", verb="list", limit=100, weight=1),
                            dict(namespace=ns, apiVersion="apps/v1", resource="replicasets", verb="list", limit=100, weight=1)]
            spec = dict(version="v1", durationSeconds=seconds, ratePerSecond=arm["offeredQPS"], maxInFlight=128,
                        requestTimeoutSeconds=10, seed=42, namespacePrefix=seed["namespacePrefix"], namespaces=namespaces, targets=targets)
            save(directory / "api.json", spec)
            api = self.start([self.cli, "api-load", "run", "--input", directory / "api.json", "--credentials-dir", population / "credentials", "--output", directory / "api"], "api")
        begin = time.monotonic()
        save(directory / "window.json", dict(start=stamp(), durationSeconds=seconds, recoverySeconds=c["recoverySeconds"],
             canaryCadenceSeconds=cadence_seconds, expectedRollouts=turnover_expected(arm)))
        rollout = self.start([self.cli, "turnover", "run", "--input", directory / "turnover.json", "--credentials-dir", population / "credentials",
                              "--output", directory / "turnover", "--request-log", directory / "turnover.requests.jsonl"], "turnover")
        cadence = []
        for slot in range(math.ceil(seconds / cadence_seconds)):
            self.check()
            due = begin + slot * cadence_seconds
            if time.monotonic() >= min(due + cadence_seconds, begin + seconds):
                cadence.append(dict(slot=slot, status="missed"))
                continue
            self.pause(max(0, due - time.monotonic()))
            self.canary(directory, "canary-" + str(slot), population / "canary.kubeconfig", expected)
            cadence.append(dict(slot=slot, status="completed", afterWindow=time.monotonic() >= begin + seconds))
            save(directory / "cadence.json", cadence)
        if api:
            self.wait(api, max(1, begin + seconds + 30 - time.monotonic()))
        result = self.wait(rollout, max(1, begin + seconds + c["recoverySeconds"] + 120 - time.monotonic()))
        self.pause(max(0, begin + seconds + c["recoverySeconds"] - time.monotonic()))
        save(directory / "outcome.json", dict(at=stamp(), execution="completed", turnover=result, cadence=cadence, qualified=False))
        if api:
            summarize_api(directory / "api")
        require(result["scheduled"] == turnover_expected(arm), "turnover schedule mismatch")
        require(not any(result.get(k, 0) for k in ("failed", "notStarted", "outstanding", "backlog")), "turnover_debt: no automatic next arm")
        require(result["completed"] == result["scheduled"], "turnover completion mismatch")

    def execute(self):
        c = self.config
        started, errors, state, thread = [], [], "aborted", None
        self.phase("preflight")
        try:
            for name, value in read(self.root / "bundle/sha256.json").items():
                require(digest(self.root / "bundle" / name) == value, "frozen bundle integrity: " + name)
            jobs = self.observations_ready()
            nodes = self.get("guest", "nodes")["items"]
            require(len(nodes) == c["guestNodes"] and all(ready(n) for n in nodes), "guest node readiness/count")
            pods = self.get("management", "pods", c["hcpNamespace"])["items"]
            require(pods and all(ready(p) or p.get("status", {}).get("phase") == "Succeeded" for p in pods), "HCP not initially ready")
            management_nodes = self.get("management", "nodes")["items"]
            workloads = self.get("management", "deployments,statefulsets,daemonsets", c["hcpNamespace"])["items"]
            if c.get("requireLimits"):
                all_workloads = self.get("management", "deployments,statefulsets,daemonsets,replicasets,jobs,cronjobs", c["hcpNamespace"])["items"]
                save(self.root / "limits-preflight.json", dict(at=stamp(), coverage=limits_inventory(pods + all_workloads, c["requireLimits"])))
            baseline = resource_lock(workloads)
            save(self.root / "resource-lock.json", baseline)
            save(self.root / "fixture-inventory.json", dict(guest=self.get("guest", "namespaces,flowschemas,prioritylevelconfigurations"),
                  managementPVCs=self.get("management", "persistentvolumeclaims")))
            kas = self.get("management", "configmap", c["hcpNamespace"], ["kas-config"])
            save(self.root / "kas-config.json", kas)
            kas_args = json.loads(kas["data"]["config.json"])["apiServerArguments"]
            require(kas_args["max-requests-inflight"] == ["150"] and kas_args["max-mutating-requests-inflight"] == ["50"], "KAS inflight not 150+50")
            if c.get("cleanupPrevious"):
                previous = c["cleanupPrevious"]
                self.retention("cleanup-previous")
                for kind, spec in previous["sessions"].items():
                    value = self.session(kind, "status", spec)
                    require(value["nonterminalPods"] == 0 and value["phase"] in ("stopped", "completed"), "previous session still active")
                directory = self.root / "cleanup-previous"
                save(directory / "owned.json", previous["owned"])
                save(directory / "seed.json", previous["seed"])
                live = self.get("guest", "namespaces,flowschemas,prioritylevelconfigurations", extra=["-l", LABEL + "=" + previous["seed"]["id"]])["items"]
                save(directory / "live-before.json", live)
                require({o["metadata"]["uid"] for o in live} == {o["metadata"]["uid"] for o in previous["owned"]}, "previous fixture UID set changed")
                if previous.get("partialInventory"):
                    full = self.get("guest", "namespaces,deployments,replicasets,services,configmaps,secrets,serviceaccounts,roles,rolebindings",
                                    extra=["-l", LABEL + "=" + previous["seed"]["id"]])["items"]
                    require({o["metadata"]["uid"] for o in full} == {o["metadata"]["uid"] for o in previous["partialInventory"]}, "partial ownership exact UID mismatch")
                    for obj in full:
                        if obj["kind"] == "Deployment":
                            old = next(o for o in previous["partialInventory"] if o["metadata"]["uid"] == obj["metadata"]["uid"])
                            require(obj["spec"] == old["spec"], "partial Deployment template/spec changed")
                self.cleanup_population(directory)
                # The explicitly authorized prior workload is excluded, not any other preexisting fixture.
                inventory = read(self.root / "fixture-inventory.json")
                removed = {o["metadata"]["uid"] for o in previous["owned"]}
                inventory["guest"]["items"] = [o for o in inventory["guest"]["items"] if o["metadata"]["uid"] not in removed]
                save(self.root / "fixture-inventory.json", inventory)
            existing = self.get("guest", "namespaces", extra=["-l", LABEL])["items"]
            allowed = {o["metadata"]["uid"] for o in c.get("adoptPrevious", {}).get("owned", []) if o["kind"] == "Namespace"}
            require({o["metadata"]["uid"] for o in existing} == allowed, "existing workload namespaces; refusing unapproved adoption")
            if c.get("adoptPrevious"):
                for kind, spec in c["adoptPrevious"]["sessions"].items():
                    value = self.session(kind, "status", spec)
                    require(value["phase"] in ("stopped", "completed") and value["nonterminalPods"] == 0, "adopt predecessor still active")
            for kind in ("record", "adversaries"):
                spec = c[kind]
                existing = self.get("management", "configmap", spec["namespace"], [kind + "-" + spec["id"], "--ignore-not-found"])
                require(not existing, "session ID already exists; refusing adoption")
            safety = Safety(pods, nodes + management_nodes, c["protectedComponents"])
            require(not safety.ooms, "existing HCP OOM requires review")
            thread = threading.Thread(target=self.guard, args=(baseline, safety, jobs), daemon=True)
            thread.start()
            expected, population, seed = None, None, None
            for arm in c["arms"]:
                self.phase("arm-preflight", arm=arm)
                directory = self.root / "arms" / arm["name"]
                directory.mkdir(parents=True, mode=0o700)
                self.retention(arm["name"])
                self.observations_ready(arm["name"])
                if not started:
                    started.append(("record", c["record"]))
                    self.session("record", "start", c["record"])
                    self.session_ready("record", c["record"])
                    save(self.root / "record-before-seed.json", dict(at=stamp(), record=c["record"]))
                if arm.get("observationOnly"):
                    started.append(("adversaries", c["adversaries"]))
                    save(self.root / "adversaries-frozen.json", self.session("adversaries", "start", c["adversaries"]))
                    self.session_ready("adversaries", c["adversaries"])
                    self.phase("observation-only-contended", arm=arm)
                    self.pause(arm["durationSeconds"])
                    self.observations_ready(arm["name"] + "-after")
                    save(directory / "outcome.json", dict(at=stamp(), execution="completed", observationOnly=True, qualified=False))
                    continue
                if seed is None or arm["applications"] != seed["applications"]:
                    if population:
                        self.cleanup_population(population)
                        inventory = read(self.root / "fixture-inventory.json")
                        removed = {o["metadata"]["uid"] for o in read(population / "owned.json")}
                        inventory["guest"]["items"] = [o for o in inventory["guest"]["items"] if o["metadata"]["uid"] not in removed]
                        save(self.root / "fixture-inventory.json", inventory)
                    self.phase("seeding", arm=arm)
                    population = directory
                    if seed is None and c.get("adoptPrevious"):
                        seed = self.adopt_population(directory, c["adoptPrevious"])
                        require(arm["applications"] == seed["applications"], "adopt first arm population mismatch")
                    else:
                        seed = self.population(arm, directory)
                    self.canary(directory, "warmup", directory / "canary.kubeconfig", None)
                    if expected is None:
                        baseline_cycles = [self.canary(directory, "baseline-" + str(i), directory / "canary.kubeconfig", None) for i in range(5)]
                        expected = {stage: min(3 * max(x[stage]["durationNanoseconds"] for x in baseline_cycles), cap * 10**9)
                                    for stage, cap in (("ack", 30), ("ready", 120), ("cleanup", 120))}
                        require(all(v > 0 for v in expected.values()), "invalid fresh canary baselines")
                        save(self.root / "expectations.json", expected)
                if arm.get("contended") and not any(k == "adversaries" for k, _ in started):
                    started.append(("adversaries", c["adversaries"]))
                    value = self.session("adversaries", "start", c["adversaries"])
                    save(self.root / "adversaries-frozen.json", value)
                    self.session_ready("adversaries", c["adversaries"])
                    self.pause(45)
                if arm.get("contended"):
                    pressure = self.session_ready("adversaries", c["adversaries"])
                    require(pressure["placementValid"], "competitor placement drift")
                    cpu, busy = self.query(c["pressureCPUQuery"]), self.query(c["pressureBusyQuery"])
                    save(directory / "pressure.json", dict(at=stamp(), status=pressure, cpu=cpu, busy=busy))
                    require(cpu and busy and all(float(r["value"][1]) > 0 for r in cpu), "actual competitor CPU pressure not observed")
                self.observations_ready(arm["name"] + "-admission")
                for kind, spec in started:
                    self.session_ready(kind, spec)
                self.phase("measuring", arm=arm)
                self.measure(arm, directory, population, seed, expected)
                if self.progression_hold(arm):
                    break
            state = "completed"
        except Exception as error:
            self.reason = self.reason or str(error)
            save(self.root / "stop.json", dict(at=stamp(), reason=self.reason, exception=type(error).__name__))
            if "directory" in locals() and not (directory / "outcome.json").exists():
                save(directory / "outcome.json", dict(at=stamp(), execution="aborted", stopReason=self.reason,
                     phase=read(self.root / "status.json")["phase"], qualified=False))
        finally:
            self.finalizing = True
            self.phase("finalizing", execution=state, reason=self.reason)
            self.guard_done.set()
            if thread:
                thread.join(timeout=150)
            for process, path in self.children:
                if process.poll() is None:
                    code = self.terminate(process)
                    save(str(path) + ".exit.json", dict(at=stamp(), exitCode=code, finalizationStop=True))
            for kind, spec in reversed(started):
                try:
                    self.session(kind, "stop", spec)
                    until = time.monotonic() + 150
                    while True:
                        value = self.session(kind, "status", spec)
                        if value["nonterminalPods"] == 0 and not value.get("submissionIncomplete", False):
                            break
                        require(time.monotonic() < until, kind + " stop not confirmed")
                        time.sleep(3)
                except Exception as error:
                    errors.append(str(error))
            for kind, spec in started:
                if kind == "record":
                    try:
                        self.perf(["record", "export", "--namespace", spec["namespace"], "--id", spec["id"],
                                   "--kubeconfig", c["managementKubeconfig"], "--timeout", "5m", "--output", self.root / "record-export"], "record-export", 330)
                    except Exception as error:
                        errors.append(str(error))
            for cluster in ("management", "guest"):
                try:
                    save(self.root / (cluster + "-final.json"), self.get(cluster, "pods"))
                except Exception as error:
                    errors.append(str(error))
            if (self.root / "fixture-inventory.json").exists():
                try:
                    before = read(self.root / "fixture-inventory.json")
                    after = dict(guest=self.get("guest", "namespaces,flowschemas,prioritylevelconfigurations"),
                                 managementPVCs=self.get("management", "persistentvolumeclaims"))
                    missing = {kind: sorted({o["metadata"]["uid"] for o in before[kind]["items"]}
                                           - {o["metadata"]["uid"] for o in after[kind]["items"]}) for kind in before}
                    save(self.root / "fixture-preservation.json", dict(at=stamp(), missingUIDs=missing))
                    require(not any(missing.values()), "preexisting fixture UID missing")
                except Exception as error:
                    errors.append(str(error))
            # External capture stays owned/running. The frozen query set is exported even after a stop.
            try:
                self.perf(["metrics", "export", "--endpoint", c["capture"]["endpoint"], "--queries", self.root / "bundle/final-queries.json",
                           "--start", read(self.root / "started.json")["at"], "--end", stamp(), "--step", "15s", "--output", self.root / "metrics-final"],
                          "metrics-final", 240)
            except Exception as error:
                errors.append(str(error))
            live = [p.pid for p, _ in self.children if p.poll() is None]
            save(self.root / "outcome.json", dict(at=stamp(), execution=state, stopReason=self.reason,
                 finalizationErrors=errors, activeLocalChildren=live, qualified=False, captureLifecycleOwnedExternally=True,
                  fixturesRetained=True, nextDecision=("review progression hold before selecting another arm" if (self.root / "progression-hold.json").exists()
                  else "ready for lifecycle review; scaling is not performed" if state == "completed" else "review bounded stop before further load")))
            self.phase("terminal", execution=state, reason=self.reason, finalizationErrors=errors)
        return 0 if state == "completed" and not errors else 1


def summarize_api(directory):
    result = read(directory / "result.json")
    rows = [json.loads(line) for line in (directory / "requests.ndjson").read_text().splitlines()]
    elapsed = epoch(result["finished"]) - epoch(result["started"])
    successful = [r for r in rows if 200 <= r.get("statusCode", 0) < 300 and not r.get("errClass")]
    groups = {}
    for resource in ("deployments", "pods", "replicasets"):
        values = sorted(r["latencyNanoseconds"] / 1e9 for r in successful if result["spec"]["targets"][r["target"]]["resource"] == resource)
        groups[resource] = dict(successful=len(values), p50Seconds=values[math.ceil(len(values)*.5)-1] if values else None,
                                p95Seconds=values[math.ceil(len(values)*.95)-1] if values else None, maxSeconds=max(values) if values else None)
    save(directory.parent / "api-summary.json", dict(at=stamp(), counters=result, successfulBodyComplete=len(successful),
         successQPSIncludingDrain=len(successful)/elapsed, successPerOfferedSecond=len(successful)/result["spec"]["durationSeconds"],
         elapsedIncludingDrainSeconds=elapsed, successfulLatency=groups,
         statusCounts=dict(collections.Counter(str(r.get("statusCode")) for r in rows)),
         errorCounts=dict(collections.Counter(r.get("errClass", "") for r in rows)),
         apfUIDs=sorted({(r.get("apfFlowSchemaUID", ""), r.get("apfPriorityLevelUID", "")) for r in rows}),
          goalSuccessQPS=None, enforcedErrorRateSLO=None, qualified=False))


def review(root):
    """Local-only summary, preserving separate workload, capture and safety evidence."""
    if not (root / "outcome.json").exists():
        print(json.dumps(recover_inspect(root), indent=2))
        return
    arms = []
    for arm in read(root / "bundle/config.json")["arms"]:
        directory = root / "arms" / arm["name"]
        outcome = read(directory / "outcome.json") if (directory / "outcome.json").exists() else None
        canaries = [read(p) for p in sorted(directory.glob("canary-*.json")) if p.stem.removeprefix("canary-").isdigit()]
        rows = [json.loads(line) for p in directory.glob("*.requests.jsonl") for line in p.read_text().splitlines()]
        apf = {o["metadata"]["uid"]: dict(kind=o["kind"], name=o["metadata"]["name"], spec=o["spec"])
               for o in read(directory / "apf.json")} if (directory / "apf.json").exists() else {}
        arms.append(dict(spec=arm, outcome=outcome, canaries=dict(completed=len(canaries), degraded=sum(x["degraded"] for x in canaries),
             maximumSeconds={stage: max((x[stage]["durationNanoseconds"] / 1e9 for x in canaries), default=None) for stage in ("ack", "ready", "cleanup")}),
             requestStatusCounts=dict(collections.Counter(str(x.get("statusCode")) for x in rows)),
             classification=[dict(flowSchemaUID=fs, priorityLevelUID=pl, flowSchema=apf.get(fs), priorityLevel=apf.get(pl))
                             for fs, pl in sorted({(x.get("apfFlowSchemaUID", ""), x.get("apfPriorityLevelUID", "")) for x in rows})]))
    observations = [read(p) for p in sorted((root / "safety").glob("*.json"))]
    frozen = read(root / "bundle/sha256.json")
    mismatches = [name for name, value in frozen.items() if digest(root / "bundle" / name) != value]
    archive = root / "record-export/archive.tar"
    save(root / "review.json", dict(at=stamp(), arms=arms, outcome=read(root / "outcome.json"),
         frozenIntegrityMismatches=mismatches, archiveSHA256=digest(archive) if archive.exists() else None,
         safety=dict(observations=len(observations), violations=[x for x in observations if x["violations"]], failures=[x for x in observations if x["failures"]]),
         qualified=False, collectorCoverage="not qualified; inspect raw stream and recorder gap evidence"))


def freeze(config, root):
    config, root = Path(config).resolve(), Path(root).resolve()
    spec = read(config)
    require(not spec.get("planningOnly"), "planning-only inputs cannot freeze")
    spec = resolve_config_paths(spec, config, root)
    root.mkdir(mode=0o700)
    bundle = root / "bundle"
    bundle.mkdir(mode=0o700)
    for key in ("record", "retention", "adversaries"):
        if isinstance(spec[key], str):
            spec[key] = read(spec[key])
    if isinstance(spec["capture"]["finalQueries"], str):
        spec["capture"]["finalQueries"] = read(spec["capture"]["finalQueries"])
    if isinstance(spec["capture"]["expectedTargets"], str):
        spec["capture"]["expectedTargets"] = read(spec["capture"]["expectedTargets"])["data"]["activeTargets"]
    policy = spec["capture"]
    require(0 < policy["pollSeconds"] <= 10 and 0 < policy["lossSeconds"] <= policy["gateSeconds"] <= 180,
            "capture retry bounds invalid")
    CaptureHealth(policy["expectedTargets"], policy["lossSeconds"])
    require(not (spec.get("cleanupPrevious") and spec.get("adoptPrevious")), "choose cleanup or adoption")
    if spec.get("adoptPrevious"):
        adoption = spec["adoptPrevious"]
        prior, snapshot = Path(adoption["run"]), Path(adoption["snapshot"])
        config = read(prior / "bundle/config.json")
        population = prior / "arms" / adoption["populationArm"]
        seed = read(population / "seed.json")
        select = lambda rows: [o for o in rows["items"] if o["metadata"].get("labels", {}).get(LABEL) == seed["id"]]
        previous = dict(source=str(prior), outcome=read(prior / "outcome.json"), seed=seed, setup=read(population / "setup.json"),
            owned=read(population / "owned.json"), pods=select(read(prior / "guest-final.json")),
            deployments=select(read(snapshot / "guest-deployments.json")), apf=read(snapshot / "apf.json")["items"],
            nodes=read(snapshot / "guest-nodes.json")["items"], resourceLock=read(prior / "resource-lock.json"),
            managementNodeUIDs=sorted(n["metadata"]["uid"] for n in read(snapshot / "mgmt-nodes.json")["items"]),
            cscSpec=next(o["spec"] for o in read(snapshot / "csc.json")["items"] if o["metadata"]["name"] == "cluster"),
            sessions={k: config[k] for k in ("record", "adversaries")})
        require(previous["outcome"]["execution"] == "completed" and not previous["outcome"]["activeLocalChildren"]
                and not previous["outcome"]["finalizationErrors"], "adopt requires completed terminal predecessor")
        require(config["admission"] == spec["admission"] and config["hcpNamespace"] == spec["hcpNamespace"], "adopt policy/cluster mismatch")
        spec["adoptPrevious"] = previous
    if spec.get("cleanupPrevious"):
        partial = spec["cleanupPrevious"] if isinstance(spec["cleanupPrevious"], dict) else None
        prior = Path(partial["run"] if partial else spec["cleanupPrevious"])
        outcome = read(prior / "outcome.json")
        require(not outcome["activeLocalChildren"] and not outcome["finalizationErrors"], "prior shutdown incomplete")
        config = read(prior / "bundle/config.json")
        populations = [prior / "arms" / a["name"] for a in config["arms"] if (prior / "arms" / a["name"] / "owned.json").exists()
                       and not (prior / "arms" / a["name"] / "cleanup.json").exists()]
        if partial:
            populations = [prior / "arms" / partial["populationArm"]]
            inventory = read(prior / partial["inventory"])
            owned = [o for o in inventory if o["kind"] in ("Namespace", "FlowSchema", "PriorityLevelConfiguration")]
        else:
            require(len(populations) == 1, "require one exact retained population")
            owned = read(populations[0] / "owned.json")
        spec["cleanupPrevious"] = dict(source=str(prior), outcome=outcome, outcomeSHA256=digest(prior / "outcome.json"),
            owned=owned, seed=read(populations[0] / "seed.json"),
            sessions={k: config[k] for k in ("record", "adversaries")})
        if partial:
            spec["cleanupPrevious"]["partialInventory"] = inventory
    if spec.get("requireLimits") and isinstance(spec["requireLimits"]["inventory"], str):
        spec["requireLimits"]["inventory"] = read(spec["requireLimits"]["inventory"])
    validate_campaign(spec)
    shutil.copytree(Path(__file__).resolve().parent.parent, bundle / "source", ignore=shutil.ignore_patterns("__pycache__"))
    shutil.copy2(__file__, bundle / "campaign.py")
    shutil.copy2(spec["seed"]["binary"]["path"], bundle / "kube-burner")
    require(digest(bundle / "kube-burner") == spec["seed"]["binary"]["expectedSHA256"], "kube-burner hash mismatch")
    spec["seed"]["binary"]["path"] = str(bundle / "kube-burner")
    save(bundle / "capture-ready-at-freeze.json", read(spec["capture"]["readyFile"]))
    save(bundle / "config.json", spec)
    save(bundle / "retention.json", spec["retention"])
    save(bundle / "final-queries.json", spec["capture"]["finalQueries"])
    with (root / "build.log").open("xb") as log:
        subprocess.run(["go", "build", "-o", str(bundle / "hcp-perf"), "."], cwd=bundle / "source",
                       env=dict(os.environ, GOWORK="off"), stdout=log, stderr=subprocess.STDOUT, check=True, timeout=240)
    save(bundle / "provenance.json", dict(at=stamp(), source=str(Path(__file__).resolve().parent.parent),
         credentials={k: {"path": spec[k], "sha256": digest(spec[k])} for k in ("guestKubeconfig", "managementKubeconfig")},
          sourceIncludesDirtyAndUntracked=True, qualified=False))
    with (bundle / "campaign.service").open("x") as unit:
        unit.write(unit_definition(spec, root))
        unit.flush()
        os.fsync(unit.fileno())
    save(bundle / "sha256.json", {str(p.relative_to(bundle)): digest(p) for p in bundle.rglob("*") if p.is_file()})
    for p in bundle.rglob("*"):
        if p.is_file():
            p.chmod(0o500 if p.name in ("hcp-perf", "kube-burner") else 0o400)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("freeze", "launch", "run", "review", "recover-inspect"))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--config", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    root = args.output.resolve()
    if args.command == "recover-inspect":
        print(json.dumps(recover_inspect(root), indent=2))
        return 0
    if args.command == "review":
        review(root)
        return 0
    if args.command == "freeze":
        require(args.config is not None, "--config required")
        freeze(args.config, root)
        return 0
    spec = read(root / "bundle/config.json")
    if args.command == "launch":
        require(not (root / "started.json").exists(), "campaign already started; no automatic resume")
        subprocess.run(["systemd-run", "--user", "--unit=" + spec["unit"], "--property=RuntimeMaxSec=" + str(spec["runtimeSeconds"]),
                        "--property=TimeoutStopSec=1000", "--property=KillMode=mixed", "--property=UMask=0077", "--property=Restart=no",
                        "--working-directory=" + str(root),
                        "--setenv=AZURE_CONFIG_DIR=" + str(Path.home() / ".azure-redhat"), "--setenv=PATH=" + os.environ["PATH"],
                        sys.executable, "-B", str(root / "bundle/campaign.py"), "run", "--output", str(root)], check=True)
        return 0
    with Path(spec["lockFile"]).open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        require(not (root / "started.json").exists(), "campaign already started")
        save(root / "started.json", dict(at=stamp(), pid=os.getpid()))
        runner = Runner(root)
        def stop(signum, _frame):
            if not runner.finalizing:
                runner.reason = runner.reason or "signal_" + signal.Signals(signum).name
                runner.halt.set()
        for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            signal.signal(signum, stop)
        return runner.execute()


if __name__ == "__main__":
    sys.exit(main())
