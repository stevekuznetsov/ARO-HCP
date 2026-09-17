"""Offline, measured management-cluster usage. No scheduling-model defaults.

Range samples describe intervals of ``step`` seconds; the instant at ``at``
only describes current inventory. Missing active-pod telemetry is not idle time.
"""

from bisect import bisect_right
from collections import defaultdict
from datetime import datetime, timezone
import math
import json
import re


SIZE_LABEL = "hypershift.openshift.io/hosted-cluster-size"
RESOURCES = {"cpu": ("cpu_mc", 1000), "memory": ("mem_mib", 1 / 2**20),
             "pods": ("pods", 1), "aro_openshift_io_swift_nic": ("nic", 1)}


def _epoch(value):
    if isinstance(value, str):
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    return float(value)


def _iso(value):
    return datetime.fromtimestamp(value, timezone.utc).isoformat().replace("+00:00", "Z")


def _quantity(value):
    """Parse a Kubernetes quantity into base units (cores, bytes, or counts)."""
    match = re.fullmatch(r"([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))"
                         r"([eE][+-]?[0-9]+|[numkMGTPE]|[KMGTPE]i)?", str(value))
    if not match:
        return None
    number, suffix = match.groups()
    suffix = suffix or ""
    if suffix.endswith("i"):
        factor = 1024 ** ("KMGTPE".index(suffix[0]) + 1)
    elif suffix.startswith(("e", "E")) and len(suffix) > 1:
        # Parse the exponent with float to handle overflow without huge integers.
        number, factor = number + suffix, 1
    else:
        factor = {"": 1, "n": 1e-9, "u": 1e-6, "m": 1e-3, "k": 1e3,
                  "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18}[suffix]
    result = float(number) * factor
    return result if math.isfinite(result) and result >= 0 else None


def _pod_uid(labels):
    if labels.get("uid"):
        return labels["uid"]
    match = re.search(r"(?:^|[/\-])pod([^/]+?)(?:\.slice)?(?:/|$)", labels.get("id", ""))
    return match.group(1).replace("_", "-") if match else ""


def _identity(metric, labels):
    """Ignore scrape/source labels, but retain actual Kubernetes identities."""
    if metric in ("node_cpu", "node_memory"):
        return (labels.get("node") or labels.get("instance", ""),)
    if metric in ("node_info", "node_labels"):
        keys = ("node",)
    elif metric in ("capacity", "allocatable"):
        keys = ("node", "resource")
    elif metric == "replicaset_owner":
        keys = ("namespace", "replicaset", "owner_kind", "owner_name")
    else:
        keys = ("namespace", "pod", "uid")
        keys += {
            "pod_info": ("node",), "pod_phase": ("phase",),
            "pod_owner": ("owner_kind", "owner_name"),
            "cpu": ("container", "node", "id"),
            "memory": ("container", "node", "id"),
            "requests": ("container", "resource", "node"),
            "init_requests": ("container", "resource", "node"),
            "pod_overhead": ("resource", "node"),
        }.get(metric, ())
    return tuple(labels.get(key, "") for key in keys)


def _provider_id(value):
    # Azure ARM resource IDs are case-insensitive; Grafana lowercases them.
    if value and value.lower().startswith("azure:///subscriptions/"):
        return value.lower()
    return value


def process_bundle(raw):
    """Convert raw schema v1 into view schema v1, retaining publication errors."""
    if raw.get("schema_version") != 1:
        raise ValueError("unsupported raw schema_version")
    at = _epoch(raw["at"])
    window = float(raw.get("window_seconds", 3600))
    start = _epoch(raw.get("start", at - window))
    step = int(raw["step_seconds"])
    settle = float(raw.get("settle_seconds", 900))
    metadata_max_age = float(raw.get("metadata_max_age_seconds", 86400))
    if not math.isfinite(metadata_max_age) or metadata_max_age <= 0:
        raise ValueError("metadata_max_age_seconds must be finite and positive")
    if step <= 0 or window <= 0 or settle < 0 or not math.isclose(at - start, window):
        raise ValueError("invalid window, step, or settle duration")
    search_start = max(_epoch(raw.get("search_start", at - 86400)), at - 86400)
    view = {
        "schema_version": 1, "mode": "observed", "at": _iso(at), "start": _iso(start),
        "window_seconds": window, "step_seconds": step,
        "generated_at": _iso(datetime.now(timezone.utc).timestamp()),
        "sources": [{k: v for k, v in s.items() if k != "discovery"} for s in raw.get("sources", [])],
        "errors": list(raw.get("errors", [])),
        "warnings": list(raw.get("warnings", [])), "transitions": [],
        "suggestion": None, "management_clusters": [],
    }
    clusters = {}
    data = defaultdict(lambda: defaultdict(lambda: defaultdict(dict)))
    histories = defaultdict(list)
    node_histories = defaultdict(list)
    creations = {}
    phase_conflicts = defaultdict(set)
    query_scopes = []
    request_scopes = defaultdict(list)
    init_request_errors = set()

    def cluster(env, name, region):
        key = (env, name)
        if key not in clusters:
            clusters[key] = {"id": f"{env}/{name}", "name": name, "environment": env,
                             "region": region, "hcps": [], "nodes": [],
                             "unplaced_pods": [], "warnings": []}
        elif region and clusters[key]["region"] not in (None, "", region):
            view["errors"].append(f"{env}/{name}: conflicting regions")
        elif region:
            clusters[key]["region"] = region
        return key

    for query in raw.get("queries", []):
        env, region, metric = query["environment"], query["region"], query["metric"]
        if query.get("cluster"):
            cluster(env, query["cluster"], region)
        query_scopes.append(query)
        if metric == "requests":
            selector = re.search(r'namespace=("(?:[^"\\]|\\.)*")', query.get("expression", ""))
            namespaces = ({json.loads(selector[1])} if selector else
                          {s["labels"].get("namespace") for s in query.get("series", [])})
            scope = (env, region, query.get("cluster"), metric)
            availability = (_epoch(query["start"]), _epoch(query["end"]), bool(query.get("error")))
            for namespace in namespaces:
                request_scopes[scope + (namespace,)].append(availability)
        if metric == "init_requests" and query.get("error"):
            init_request_errors.add((env, query.get("cluster")))
        if query.get("error"):
            view["errors"].append(f"{env}/{query.get('cluster') or region} {metric}: {query['error']}")
        for series in query.get("series", []):
            labels = dict(series["labels"])
            if metric == "pod_phase":
                labels["phase"] = labels.get("phase", "").lower()
            if metric in ("cpu", "memory") and _pod_uid(labels):
                labels["uid"] = _pod_uid(labels)
            name = labels.get("cluster") or query.get("cluster")
            if not name:
                view["errors"].append(f"{env}/{region} {metric}: missing cluster label")
                continue
            key = cluster(env, name, region)
            identity = _identity(metric, labels)
            for timestamp, value in series.get("samples", []):
                timestamp = _epoch(timestamp)
                if timestamp > at:
                    continue
                try:
                    value = float(value) if value is not None else None
                    if value is not None and not math.isfinite(value):
                        value = None
                except (TypeError, ValueError):
                    value = None
                rows = data[key][metric][timestamp]
                if identity in rows:
                    old_labels, old_value = rows[identity]
                    if old_value is not None and value is not None and old_value != value:
                        if metric == "pod_phase" and {old_value, value} <= {0, 1}:
                            phase_conflicts[key].add((timestamp, identity[:3]))
                        else:
                            view["errors"].append(f"{env}/{name} {metric}: conflicting duplicate at {_iso(timestamp)}")
                        # Retain the conflict and avoid source-order dependence.
                        value = max(old_value, value)
                    elif old_value is not None:
                        value = old_value
                    labels = {**labels, **old_labels}
                rows[identity] = (labels, value)

    for snapshot in raw.get("snapshots", []):
        timestamp = _epoch(snapshot["timestamp"])
        if timestamp > at:
            continue
        key = cluster(snapshot["environment"], snapshot["cluster"], snapshot.get("region"))
        obj = snapshot.get("object") or {}
        uid = snapshot.get("uid") or obj.get("metadata", {}).get("uid")
        if not uid:
            view["errors"].append(f"{'/'.join(key)}: HostedCluster snapshot missing UID")
            continue
        histories[key + (uid,)].append((timestamp, snapshot))

    # Node snapshots are enrichment only: they must never discover a cluster/node.
    for snapshot in raw.get("node_snapshots", []):
        timestamp = _epoch(snapshot["timestamp"])
        if timestamp > at:
            continue
        meta = (snapshot.get("object") or {}).get("metadata", {})
        nkey = (snapshot["environment"], snapshot["cluster"], snapshot.get("region"),
                snapshot.get("name") or meta.get("name"))
        node_histories[nkey].append((timestamp, snapshot))

    def relevant_history(records, begin, end, created):
        baseline = [record for timestamp, record in records if timestamp <= begin]
        if baseline and baseline[-1]["state"] == "deleted":
            return any(record["state"] != "deleted" for timestamp, record in records if begin < timestamp <= end)
        return created is None or created <= end

    def metadata_problem(records, begin, end, created):
        if not relevant_history(records, begin, end, created):
            return None
        baseline = [item for item in records if item[0] <= begin]
        if not baseline:
            if created is None or created < begin:
                return "missing baseline metadata"
            if created > end:
                return None  # A witnessed creation establishes absence before it.
        relevant = baseline[-1:] + [item for item in records if begin < item[0] <= end]
        for index, (timestamp, record) in enumerate(relevant):
            if record["state"] == "deleted":
                continue  # Deletion is durable, unlike a stale active snapshot.
            if record["size"] is None:
                return "missing assigned size metadata"
            if timestamp <= begin and begin - timestamp > metadata_max_age:
                return "stale baseline metadata"
            until = relevant[index + 1][0] if index + 1 < len(relevant) else end
            if until - timestamp > metadata_max_age:
                return "stale latest metadata" if until == end else "stale metadata within window"
        return None

    # Kusto can retain stale HCPs long after their last pod. Only namespaces with
    # pod inventory or telemetry in this window contribute to size validation.
    pod_namespaces = defaultdict(dict)
    for key, metrics in data.items():
        for metric in ("pod_info", "pod_phase", "pod_owner", "requests", "init_requests",
                       "pod_overhead", "cpu", "memory"):
            for timestamp, entries in metrics.get(metric, {}).items():
                if start <= timestamp <= at:
                    for labels, value in entries.values():
                        if value is None or (metric in ("pod_info", "pod_phase", "pod_owner") and value <= 0):
                            continue
                        if metric in ("cpu", "memory") and not labels.get("container"):
                            continue
                        if labels.get("pod") and labels.get("container") != "POD":
                            namespace = labels.get("namespace", "")
                            pod_namespaces[key][namespace] = min(
                                pod_namespaces[key].get(namespace, timestamp), timestamp)

    # Keep the baseline before search_start, too: it can establish the first transition.
    stability = []
    contributing = set()
    for hkey, history in sorted(histories.items()):
        history.sort(key=lambda item: item[0])
        baseline = [snapshot for timestamp, snapshot in history if timestamp <= search_start]
        if baseline and baseline[-1].get("event", "").lower() in ("delete", "deleted") and not any(
            snapshot.get("event", "").lower() not in ("delete", "deleted")
            for timestamp, snapshot in history if timestamp > search_start
        ):
            del histories[hkey]
            continue
        records = []
        snapshot_conflicts = []
        previous = None
        for timestamp, snapshot in history:
            event = snapshot.get("event", "").lower()
            if event in ("add", "added"):
                creations.setdefault(hkey, timestamp)
            meta = (snapshot.get("object") or {}).get("metadata", {})
            if event in ("delete", "deleted") and not meta and previous:
                record = dict(previous)
            else:
                namespace = snapshot.get("namespace") or meta.get("namespace", "")
                name = snapshot.get("name") or meta.get("name", "")
                record = {"id": hkey[2], "name": name, "namespace": namespace,
                          "control_plane_namespace": (namespace + "-" + name).replace(".", "-"),
                          "size": meta.get("labels", {}).get(SIZE_LABEL)}
            record.update(metadata_at=_iso(timestamp), state=(
                "deleted" if event in ("delete", "deleted") else
                "deleting" if meta.get("deletionTimestamp") else "active"))
            if (previous and previous["state"] != "deleted" and record["state"] != "deleted"
                    and previous["size"] is not None and record["size"] is not None
                    and previous["size"] != record["size"] and timestamp >= search_start):
                transition = {"environment": hkey[0], "cluster": hkey[1], "hcp_id": hkey[2],
                              "at": _iso(timestamp), "from_size": previous["size"],
                              "to_size": record["size"]}
                if transition not in view["transitions"]:
                    view["transitions"].append(transition)
            if records and records[-1][0] == timestamp and records[-1][1] != record:
                snapshot_conflicts.append(f"{'/'.join(hkey)}: conflicting snapshots at {_iso(timestamp)}")
            records.append((timestamp, record))
            previous = record
        histories[hkey] = records
        requested = relevant_history(records, start, at, creations.get(hkey))
        if requested:
            clusters[hkey[:2]]["hcps"].append(dict(records[-1][1]))
        first_contribution = min((pod_namespaces[hkey[:2]][namespace]
                                  for _, record in records
                                  for namespace in {record["namespace"], record["control_plane_namespace"]}
                                  if namespace in pod_namespaces[hkey[:2]]), default=None)
        matched = requested and first_contribution is not None
        if matched:
            contributing.add(hkey)
            stability.append((records, creations.get(hkey)))
        target = view["errors"] if matched else clusters[hkey[:2]]["warnings"]
        target.extend(snapshot_conflicts)
        # Initialization before any contribution need not have an assigned size.
        # Known resizes still use the full requested window below.
        begin = max(start, first_contribution) if matched else start
        problem = metadata_problem(records, begin, at, creations.get(hkey))
        if problem:
            message = f"{'/'.join(hkey)}: unknown size stability; {problem} (maximum age {metadata_max_age:g}s)"
            if not matched:
                message += "; no matching pod data in window (informational)"
            clusters[hkey[:2]]["warnings"].append(message)
            if matched:
                view["errors"].append(message)
        if requested and records[-1][1]["size"] is None:
            clusters[hkey[:2]]["warnings"].append(f"{'/'.join(hkey)}: assigned size label missing")

    transitions = sorted((item for item in view["transitions"]
                          if (item["environment"], item["cluster"], item["hcp_id"]) in contributing),
                         key=lambda item: (item["at"], item["environment"], item["cluster"], item["hcp_id"]))
    view["transitions"] = transitions
    changes = [(_epoch(item["at"]), (item["environment"], item["cluster"], item["hcp_id"])) for item in transitions]

    def intersects_transition(begin, end):
        return any(((t <= end and t + settle > begin) or begin <= t <= end)
                   and relevant_history(histories[hkey], begin, end, creations.get(hkey))
                   for t, hkey in changes)

    def stable(end):
        begin = end - window
        if intersects_transition(begin, end):
            return False
        for records, created in stability:
            if metadata_problem(records, begin, end, created):
                return False
        return True

    if intersects_transition(start, at):
        view["errors"].append("requested window intersects a HostedCluster size transition or settling interval")
        candidate = at - step
        while candidate - window >= search_start:
            if stable(candidate):
                view["suggestion"] = {"at": _iso(candidate), "start": _iso(candidate - window),
                                      "command": f"--at {_iso(candidate)} --window {window:g}s --step {step}s"}
                break
            candidate -= step

    if not clusters and any(q.get("required") for q in query_scopes):
        view["errors"].append("required queries returned no management-cluster inventory")

    for key, mc in sorted(clusters.items()):
        metrics = data[key]
        ticks = defaultdict(set)
        widths = {}
        required = set()
        for query in query_scopes:
            if query["environment"] != key[0] or query["region"] != mc["region"]:
                continue
            if query.get("cluster") and query["cluster"] != key[1]:
                continue
            metric = query["metric"]
            qstep = int(query.get("step", step))
            if qstep <= 0:
                raise ValueError("query step must be positive")
            if metric in widths and widths[metric] != qstep:
                view["errors"].append(f"{mc['id']} {metric}: inconsistent query steps")
            widths[metric] = min(widths.get(metric, qstep), qstep)
            t = _epoch(query["start"])
            while t <= min(_epoch(query["end"]), at):
                ticks[metric].add(t)
                t += qstep
            if query.get("required"):
                required.add(metric)
        for metric, samples in metrics.items():
            ticks[metric].update(samples)
        ticks = {metric: sorted(values) for metric, values in ticks.items()}

        def rows(metric, timestamp):
            times = ticks.get(metric, [])
            index = bisect_right(times, timestamp) - 1
            if index < 0:
                return []
            sample_at = times[index]
            if sample_at < start:
                return []
            # Never extrapolate the current inventory from a historical range sample.
            if timestamp == at and sample_at != at:
                return []
            if timestamp >= sample_at + widths.get(metric, step):
                return []
            return list(metrics.get(metric, {}).get(sample_at, {}).values())

        boundaries = {start, at}
        for metric, times in ticks.items():
            for t in times:
                if start < t < at:
                    boundaries.add(t)
                expiry = t + widths.get(metric, step)
                if start < expiry < at:
                    boundaries.add(expiry)
        boundaries = sorted(boundaries)
        intervals = list(zip(boundaries[:-1], boundaries[1:])) + [(at, at)]

        # Old captures use exporter instance (a node name or IP:port). Resolve
        # against inventory at the sample time, never against a pod-name heuristic.
        for metric in ("node_cpu", "node_memory"):
            for t, entries in metrics.get(metric, {}).items():
                mapped = {}
                inventory = [(l, v) for l, v in rows("node_info", t) if v is not None and v > 0]
                for labels, value in entries.values():
                    labels = dict(labels)
                    if not labels.get("node"):
                        instance = labels.get("instance", "")
                        host = instance.split("]", 1)[0].lstrip("[") if instance.startswith("[") else (
                            instance.rsplit(":", 1)[0] if instance.count(":") == 1 else instance)
                        matches = {l["node"] for l, _ in inventory if l.get("node")
                                   and (instance == l["node"] or host == l.get("internal_ip"))}
                        if len(matches) != 1:
                            if value is not None:
                                view["errors"].append(f"{mc['id']}: {metric} instance {instance!r} has no unique node mapping")
                            continue
                        labels["node"] = matches.pop()
                    identity = (labels["node"],)
                    if identity in mapped:
                        old = mapped[identity][1]
                        if old is not None and value is not None and old != value:
                            view["errors"].append(f"{mc['id']} {metric}: conflicting node samples at {_iso(t)}")
                        if old is not None:
                            value = max(old, value) if value is not None else old
                    mapped[identity] = (labels, value)
                metrics[metric][t] = mapped

        pods, nodes = {}, {}
        active_at = {}
        alive_by_name = defaultdict(lambda: defaultdict(list))
        phase_gaps = defaultdict(float)

        def node(name):
            if name not in nodes:
                 nodes[name] = {"id": f"{mc['id']}/{name}", "name": name, "sku": None,
                                "pool": None, "zone": None, "current": False,
                                "metadata_at": None, "metadata_source": None, "metadata_uid": None,
                               "capacity": dict.fromkeys(("cpu_mc", "mem_mib", "pods", "nic")),
                               "allocatable": dict.fromkeys(("cpu_mc", "mem_mib", "pods", "nic")),
                               "node_usage": {"cpu_mc": None, "mem_mib": None}, "pods": []}
            return nodes[name]

        for metric, samples in metrics.items():
            if metric.startswith("node_") or metric in ("capacity", "allocatable"):
                for t, entries in samples.items():
                    if start <= t <= at:
                        for labels, value in entries.values():
                            if labels.get("node") and value is not None:
                                node(labels["node"])

        for t, end in intervals:
            alive = defaultdict(list)
            phases = defaultdict(set)
            for l, v in rows("pod_phase", t):
                if v is not None and v > 0:
                    phases[(l.get("namespace"), l.get("pod"), l.get("uid", ""))].add(l.get("phase"))
            terminal = {p for p, values in phases.items() if values and values <= {"succeeded", "failed"}}
            uncertain = set()
            for p, values in phases.items():
                if len(values) > 1 or (t, p) in phase_conflicts[key]:
                    safe = len(values) > 1 and (values <= {"pending", "running"}
                                                or values <= {"succeeded", "failed"})
                    if not safe:
                        terminal.discard(p)
                        uncertain.add(p)
                    mc["warnings"].append(f"{mc['id']}/{'/'.join(p)}: conflicting pod phases {','.join(sorted(values))} at {_iso(t)}"
                                  + (" (same activity classification)" if safe else " (activity uncertain)"))
            terminal_by_name = defaultdict(set)
            for ns, name, uid in terminal:
                terminal_by_name[(ns, name)].add(uid)
            inventory_present = False
            for labels, value in rows("node_info", t):
                if value is not None and value > 0 and labels.get("node"):
                    item = node(labels["node"])
                    if t == at:
                        item["current"] = True
            for labels, value in rows("pod_info", t):
                if value is None or value <= 0:
                    continue
                inventory_present = True
                ns, name, uid = labels.get("namespace", ""), labels.get("pod", ""), labels.get("uid", "")
                pkey = (ns, name, uid)
                if pkey not in pods:
                    pods[pkey] = {"id": f"{mc['id']}/{ns}/{name}/{uid or 'unknown'}", "name": name,
                                  "namespace": ns, "component": name, "hcp_id": None, "hcp_size": None,
                                  "current": False, "usage": {"cpu_mc": None, "mem_mib": None},
                                  "requests": {"cpu_mc": None, "mem_mib": None, "nic": None},
                                  "coverage": {"cpu": 0.0, "memory": 0.0}}
                    if not uid:
                        mc["warnings"].append(f"{mc['id']}/{ns}/{name}: pod UID missing")
                state = active_at.setdefault(pkey, {"times": {}, "node": "", "last": t})
                state["last"] = t
                state["info_seen"] = True
                state.setdefault("first_seen", t)
                if labels.get("node"):
                    if state["node"] and state["node"] != labels["node"]:
                        view["errors"].append(f"{pods[pkey]['id']}: conflicting node placement for pod UID")
                    state["node"] = labels["node"]
                    node(labels["node"])
                is_terminal = pkey in terminal or (ns, name, "") in terminal
                pod_phases = phases.get(pkey, set()) | phases.get((ns, name, ""), set())
                known_phase = bool(pod_phases) and (pod_phases <= {"pending", "running"}
                                                   or pod_phases <= {"succeeded", "failed"})
                known_phase = known_phase and pkey not in uncertain and (ns, name, "") not in uncertain
                pods[pkey]["phase"] = next(iter(pod_phases)) if len(pod_phases) == 1 and known_phase else "unknown"
                if pkey in uncertain or (ns, name, "") in uncertain:
                    state["lifecycle_conflict"] = True
                if not is_terminal:
                    state["times"][t] = end - t
                    state.setdefault("phases", {})[t] = pods[pkey]["phase"]
                    if pkey not in alive[(ns, name)]:
                        alive[(ns, name)].append(pkey)
                    if not known_phase:
                        phase_gaps[pkey] += end - t
                    if t == at and known_phase:
                        pods[pkey]["current"] = True
            alive_by_name[t] = alive
            if t < at and not inventory_present and any(v is not None and v > 0 for _, v in rows("node_info", t)):
                view["errors"].append(f"{mc['id']}: pod_info inventory gap at {_iso(t)}")
            # Orphans remain visible, but cannot establish liveness or a guessed placement.
            for metric in ("cpu", "memory", "requests", "init_requests", "pod_overhead", "pod_owner", "pod_phase"):
                for labels, value in rows(metric, t):
                    if labels.get("container") == "POD" or (
                        metric in ("cpu", "memory") and not labels.get("container")
                    ):
                        continue
                    if metric in ("pod_owner", "pod_phase") and (value is None or value <= 0):
                        continue
                    ns, name = labels.get("namespace", ""), labels.get("pod", "")
                    candidates = [p for p in alive.get((ns, name), [])
                                  if (not labels.get("uid") or labels["uid"] == p[2])
                                  and (not labels.get("node") or not active_at[p]["node"]
                                       or labels["node"] == active_at[p]["node"])]
                    # Terminal pod_info is still metadata, but must not resurrect a pod.
                    terminal_uids = terminal_by_name.get((ns, name), set())
                    terminal_match = bool(terminal_uids) and (
                        not labels.get("uid") or "" in terminal_uids or labels["uid"] in terminal_uids)
                    if not candidates and not terminal_match and value is not None:
                        pkey = (ns, name, labels.get("uid", ""))
                        if pkey not in pods:
                            pods[pkey] = {"id": f"{mc['id']}/{ns}/{name}/{pkey[2] or 'unknown'}",
                                          "name": name, "namespace": ns, "component": name,
                                          "hcp_id": None, "hcp_size": None, "current": False,
                                          "usage": {"cpu_mc": None, "mem_mib": None},
                                          "requests": {"cpu_mc": None, "mem_mib": None, "nic": None},
                                          "coverage": {"cpu": 0.0, "memory": 0.0}}
                            active_at[pkey] = {"times": {}, "node": labels.get("node", ""), "last": t}
                            if labels.get("node"):
                                node(labels["node"])
                        message = f"{pods[pkey]['id']}: {metric} has no active pod_info at {_iso(t)}"
                        active_at[pkey]["orphan"] = True
                        mc["warnings"].append(message)

        # A positive inventory entry plus terminal phase throughout every observed
        # interval contributes nothing. Do not hide a pod with even one unknown interval.
        for pkey in list(pods):
            state = active_at[pkey]
            if not state["times"] and state.get("info_seen") and not state.get("orphan"):
                del pods[pkey]
                del active_at[pkey]
        for pkey, duration in phase_gaps.items():
            mc["warnings"].append(f"{pods[pkey]['id']}: pod_phase coverage missing or unknown ({duration:g} interval seconds; current state unverified where absent)")

        pod_index = {}

        def pod_rows(metric, pkey, timestamp):
            ns, name, uid = pkey
            matches = []
            index_key = (metric, timestamp)
            if index_key not in pod_index:
                index = defaultdict(list)
                for labels, value in rows(metric, timestamp):
                    index[(labels.get("namespace", ""), labels.get("pod", ""))].append((labels, value))
                pod_index[index_key] = index
            for labels, value in pod_index[index_key].get((ns, name), []):
                observed_uid = labels.get("uid", "")
                if observed_uid and observed_uid != uid:
                    continue
                placement = active_at[pkey]["node"]
                if labels.get("node") and placement and labels["node"] != placement:
                    continue
                candidates = [other for other in alive_by_name[timestamp].get(pkey[:2], [])
                              if not labels.get("node") or not active_at[other]["node"]
                              or labels["node"] == active_at[other]["node"]]
                if not observed_uid and len(candidates) > 1:
                    view["errors"].append(f"{mc['id']}/{ns}/{name}: ambiguous pod UID at {_iso(timestamp)}")
                    continue
                matches.append((labels, value))
            return matches

        def pod_metadata(metric, pkey, timestamp):
            if pods[pkey]["current"]:
                return pod_rows(metric, pkey, timestamp)
            for t in sorted(active_at[pkey]["times"], reverse=True):
                if t <= timestamp:
                    matches = pod_rows(metric, pkey, t)
                    if matches:
                        return matches
            return pod_rows(metric, pkey, timestamp)

        cluster_histories = [history for hkey, history in histories.items() if hkey[:2] == key]
        hcp_index = {}
        request_availability = {}

        def request_query_available(metric, namespace, timestamp):
            cache_key = (metric, namespace, timestamp)
            if cache_key not in request_availability:
                scoped = [error for cluster_name in (None, key[1])
                          for begin, end, error in request_scopes.get(
                              (key[0], mc["region"], cluster_name, metric, namespace), [])
                          if begin <= timestamp <= end]
                request_availability[cache_key] = bool(scoped) and not any(scoped)
            return request_availability[cache_key]

        for pkey, item in sorted(pods.items()):
            state = active_at[pkey]
            item.setdefault("phase", "unknown")
            item["usage_issues"] = []
            if state.get("lifecycle_conflict") or state.get("orphan"):
                item["usage_issues"].append("lifecycle-conflict")
            last = at if item["current"] else max(state["times"], default=state["last"])
            owners = [(l, v) for l, v in pod_metadata("pod_owner", pkey, last) if v is not None and v > 0]
            if owners:
                owner = owners[0][0]
                item["component"] = owner.get("owner_name") or item["name"]
                if owner.get("owner_kind", "").lower() == "replicaset":
                    rs = [l for l, v in rows("replicaset_owner", last) if v is not None and v > 0
                          and l.get("namespace") == pkey[0] and l.get("replicaset") == owner.get("owner_name")]
                    if rs:
                        item["component"] = rs[0].get("owner_name") or item["component"]
                    else:
                        mc["warnings"].append(f"{item['id']}: ReplicaSet owner metadata missing")
            else:
                mc["warnings"].append(f"{item['id']}: pod owner metadata missing")
            if last not in hcp_index:
                index = defaultdict(list)
                for history in cluster_histories:
                    position = bisect_right(history, last, key=lambda entry: entry[0]) - 1
                    if position >= 0:
                        record = history[position][1]
                        for namespace in {record["namespace"], record["control_plane_namespace"]}:
                            index[namespace].append(record)
                hcp_index[last] = index
            hcps = hcp_index[last].get(pkey[0], [])
            live_hcps = [hcp for hcp in hcps if hcp["state"] != "deleted"]
            if live_hcps:
                hcps = live_hcps
            if len(hcps) == 1:
                item["hcp_id"], item["hcp_size"] = hcps[0]["id"], hcps[0]["size"]
            elif hcps or pkey[0].startswith("ocm-"):
                message = f"{item['id']}: HostedCluster metadata missing or ambiguous; size stability unknown"
                mc["warnings"].append(message)
                view["errors"].append(message)

            duration = sum(state["times"].values())
            if not duration:
                mc["warnings"].append(f"{item['id']}: unknown lifetime; no observed active interval duration")
                item["usage_issues"].append("unknown-lifetime")
            for metric, field, factor in (("cpu", "cpu_mc", 1000), ("memory", "mem_mib", 1 / 2**20)):
                total, covered = 0.0, 0.0
                missing_phases = set()
                first_covered, last_missing = None, None
                for t, width in state["times"].items():
                    if not width:
                        continue
                    containers = defaultdict(list)
                    for labels, value in pod_rows(metric, pkey, t):
                        # Grafana can pad an old container ID with nulls after a
                        # restart. Absence of that ID is not a missing live container.
                        if value is not None and labels.get("container") not in (None, "", "POD"):
                            containers[labels["container"]].append((labels, value))
                    # Requests and the other usage metric can expose a missing sidecar.
                    expected = {l["container"] for other in ("requests", "cpu", "memory")
                                for l, v in pod_rows(other, pkey, t)
                                if l.get("container") not in (None, "", "POD") and v is not None}
                    values = []
                    for entries in containers.values():
                        granular = [(l, v) for l, v in entries if l.get("id")]
                        entries = granular or entries
                        unique = defaultdict(set)
                        for labels, value in entries:
                            unique[labels.get("id", "")].add(value)
                        if any(len(v) != 1 or None in v for v in unique.values()):
                            values.append(None)
                            view["errors"].append(f"{item['id']}: conflicting or missing {metric} container samples at {_iso(t)}")
                        else:
                            values.append(sum(next(iter(v)) for v in unique.values()))
                    if values and None not in values and expected <= containers.keys():
                        total += sum(values) * width
                        covered += width
                        if first_covered is None:
                            first_covered = t
                    else:
                        missing_phases.add(state["phases"][t])
                        last_missing = t
                item["coverage"][metric] = covered / duration if duration else 0.0
                if duration and covered == duration:
                    item["usage"][field] = total / window * factor
                elif duration:
                    # First observation is not proof of creation. A short leading
                    # prefix can be rate warmup, but later gaps remain unexplained.
                    warmup = (item["current"] and item["phase"] == "running"
                              and state["first_seen"] > start and first_covered is not None
                              and last_missing < first_covered
                              and first_covered - state["first_seen"] <= 300)
                    unexplained = item["current"] and "running" in missing_phases and not warmup
                    reason = ("sampling-gap" if unexplained or not item["current"] else
                              "pending" if item["phase"] == "pending" else "startup")
                    if reason not in item["usage_issues"]:
                        item["usage_issues"].append(reason)
                    target = view["errors"] if unexplained else mc["warnings"]
                    context = reason if item["current"] else "historical/uncertain lifecycle"
                    if warmup:
                        context = "startup/first-observed sampling gap"
                    target.append(f"{item['id']}: incomplete {metric} coverage ({covered:g}/{duration:g} active seconds; {context})")

            request_containers = {l["container"] for metric in ("requests", "cpu", "memory")
                                  for l, v in pod_metadata(metric, pkey, last)
                                   if v is not None and l.get("container") not in (None, "", "POD")}
            for resource, (field, factor) in RESOURCES.items():
                if field == "pods":
                    continue
                values = {}
                for metric in ("requests", "init_requests", "pod_overhead"):
                    per_container = defaultdict(set)
                    for labels, value in pod_metadata(metric, pkey, last):
                        if value is not None and labels.get("resource") == resource and labels.get("container") != "POD":
                            per_container[labels.get("container", "")].add(value)
                    if per_container and all(len(v) == 1 and None not in v for v in per_container.values()):
                        if field != "nic" and metric == "requests" and not request_containers <= per_container.keys():
                            continue
                        amounts = [next(iter(v)) for v in per_container.values()]
                        values[metric] = max(amounts) if metric == "init_requests" else sum(amounts)
                # Extended-resource requests are sparse: a successful namespace
                # query omits containers that request zero SWIFT slots. Do not
                # demand an explicit zero series from every non-router sidecar.
                nic_entries = [(l, v) for l, v in pod_metadata("requests", pkey, last)
                               if l.get("resource") == resource and v is not None]
                if field == "nic" and not nic_entries and request_query_available("requests", pkey[0], last):
                    values.setdefault("requests", 0)
                if field == "nic" and key in init_request_errors:
                    values.pop("requests", None)
                if "requests" in values:
                    item["requests"][field] = (max(values.get("requests", 0), values.get("init_requests", 0))
                                               + values.get("pod_overhead", 0)) * factor
            missing = [field for field, value in item["requests"].items() if value is None]
            if missing:
                mc["warnings"].append(f"{item['id']}: resource requests unknown ({', '.join(missing)}); absent series do not establish zero")
            if state["node"]:
                nodes[state["node"]]["pods"].append(item)
            else:
                mc["unplaced_pods"].append(item)
                mc["warnings"].append(f"{item['id']}: node placement missing")

        # Keep raw metadata samples separate from interval rows: only the latter
        # obey query gaps/expiry and the prohibition on current extrapolation.
        node_index = defaultdict(lambda: defaultdict(lambda: defaultdict(list)))
        node_last = {}
        resource_values = defaultdict(set)
        for metric, samples in metrics.items():
            if not (metric.startswith("node_") or metric in ("capacity", "allocatable")):
                continue
            for t, entries in samples.items():
                for l, v in entries.values():
                    name = l.get("node")
                    if name not in nodes or v is None:
                        continue
                    node_index[metric][name][t].append((l, v))
                    if start <= t <= at:
                        node_last[name] = max(node_last.get(name, start), t)
                        if metric in ("capacity", "allocatable"):
                            resource_values[(metric, name, l.get("resource"))].add(v)
        node_seen = defaultdict(set)
        node_gauges = defaultdict(list)
        for t in boundaries:
            for l, v in rows("node_info", t):
                if v is not None and v > 0:
                    node_seen[l.get("node")].add(t)
            for metric in ("node_cpu", "node_memory"):
                for l, v in rows(metric, t):
                    if v is not None:
                        node_gauges[(metric, l.get("node"), t)].append(v)

        for name, item in sorted(nodes.items()):
            seen = node_seen[name]
            last = at if item["current"] else node_last.get(name, start)

            def node_metadata(metric):
                samples = node_index[metric][name]
                times = [at] if item["current"] else sorted(
                    (t for t in samples if t <= last), reverse=True)
                resources = {}
                for t in times:
                    matches = samples.get(t, [])
                    if metric in ("capacity", "allocatable"):
                        for l, v in matches:
                            resources.setdefault(l.get("resource"), (l, v))
                        continue
                    if matches:
                        return matches
                return list(resources.values())

            if not seen:
                mc["warnings"].append(f"{item['id']}: node_info metadata missing")
            labels = {}
            for metric in ("node_info", "node_labels"):
                for l, value in node_metadata(metric):
                    if l.get("node") == name and value is not None and value > 0:
                        labels.update(l)
            for field, alternatives in {
                "sku": ("label_node_kubernetes_io_instance_type", "label_beta_kubernetes_io_instance_type"),
                "pool": ("label_kubernetes_azure_com_agentpool", "label_agentpool"),
                "zone": ("label_topology_kubernetes_io_zone", "label_failure_domain_beta_kubernetes_io_zone"),
            }.items():
                item[field] = next((labels[k] for k in alternatives if labels.get(k)), None)
            for metric in ("capacity", "allocatable"):
                for l, value in node_metadata(metric):
                    if l.get("node") == name and l.get("resource") in RESOURCES and value is not None:
                        field, factor = RESOURCES[l["resource"]]
                        item[metric][field] = value * factor
                for resource in RESOURCES:
                    values = resource_values[(metric, name, resource)]
                    if len(values) > 1:
                        mc["warnings"].append(f"{item['id']}: {metric} {resource} changed during window ({min(values):g}..{max(values):g})")
            inventory = sorted((t, l) for t, entries in node_index["node_info"][name].items()
                               if start <= t <= at for l, v in entries if v > 0)
            snapshot_at = at if item["current"] else (inventory[-1][0] if inventory else last)
            first_live = snapshot_at
            provider_id = _provider_id(inventory[-1][1].get("provider_id")) if inventory else None
            # A gap or provider change bounds the latest observed incarnation.
            for t, l in reversed(inventory):
                if first_live - t > widths.get("node_info", step) or _provider_id(l.get("provider_id")) != provider_id:
                    break
                first_live = t
            records = [(t, s) for t, s in node_histories.get(key + (mc["region"], name), [])
                       if t <= snapshot_at]
            deleted = {s.get("uid") or (s.get("object") or {}).get("metadata", {}).get("uid")
                       for _, s in records if s.get("event", "").lower() in ("delete", "deleted")}
            candidates = []
            for t, s in records:
                obj = s.get("object") or {}
                meta = obj.get("metadata", {})
                uid = s.get("uid") or meta.get("uid")
                # A delete is durable for a UID, even if a delayed update follows it.
                if uid in deleted:
                    continue
                if (not uid or obj.get("apiVersion") != "v1" or obj.get("kind") != "Node"
                        or meta.get("name") != name or meta.get("uid", uid) != uid):
                    mc["warnings"].append(f"{item['id']}: invalid Node snapshot identity; ignored")
                    continue
                snapshot_provider = _provider_id(obj.get("spec", {}).get("providerID"))
                if provider_id and snapshot_provider != provider_id:
                    mc["warnings"].append(f"{item['id']}: Node snapshot provider_id disagreement; ignored")
                    continue
                try:
                    created = _epoch(meta["creationTimestamp"]) if meta.get("creationTimestamp") else None
                except (TypeError, ValueError, OverflowError):
                    created = float("nan")
                if created is not None and (not math.isfinite(created) or created > min(first_live, t)):
                    mc["warnings"].append(f"{item['id']}: Node snapshot creationTimestamp disagrees with node lifetime; ignored")
                    continue
                candidates.append((t, uid, obj))
            # Without a UID metric, multiple surviving identities are not a safe join.
            selected = None
            if len({uid for _, uid, _ in candidates}) > 1:
                mc["warnings"].append(f"{item['id']}: ambiguous Node snapshot incarnation; ignored")
            elif candidates:
                latest = max(t for t, _, _ in candidates)
                newest = [(uid, obj) for t, uid, obj in candidates if t == latest]
                if any(record != newest[0] for record in newest[1:]):
                    mc["warnings"].append(f"{item['id']}: conflicting Node snapshots at {_iso(latest)}; ignored")
                elif snapshot_at - latest > 86400:
                    mc["warnings"].append(f"{item['id']}: stale Node snapshot at {_iso(latest)} (maximum age 86400s); ignored")
                else:
                    uid, selected = newest[0]
                    item.update(metadata_at=_iso(latest), metadata_source="kusto", metadata_uid=uid)
            if selected is not None:
                snapshot_labels = selected["metadata"].get("labels", {})
                fallback = {}
                for field, alternatives in {
                    "sku": ("node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"),
                    "pool": ("kubernetes.azure.com/agentpool", "agentpool"),
                    "zone": ("topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone"),
                }.items():
                    fallback[field] = next((snapshot_labels[k] for k in alternatives if snapshot_labels.get(k)), None)
                for metric in ("capacity", "allocatable"):
                    for resource, (field, factor) in RESOURCES.items():
                        resource = "aro.openshift.io/swift-nic" if field == "nic" else resource
                        quantity = selected.get("status", {}).get(metric, {}).get(resource)
                        if quantity is None:
                            continue
                        value = _quantity(quantity)
                        if value is None or not math.isfinite(value * factor):
                            mc["warnings"].append(f"{item['id']}: invalid Node snapshot {metric} {resource} quantity {quantity!r}; ignored")
                        else:
                            fallback[f"{metric}.{field}"] = value * factor
                for field, value in fallback.items():
                    if value is None:
                        continue
                    target, name_field = item, field
                    if "." in field:
                        metric, name_field = field.split(".")
                        target = item[metric]
                    current = target[name_field]
                    if current is None:
                        target[name_field] = value
                    elif not (math.isclose(current, value) if isinstance(value, (int, float)) else current == value):
                        mc["warnings"].append(f"{item['id']}: {field} disagreement: Grafana={current!r}, Node snapshot={value!r}; keeping Grafana")
            for field in ("sku", "pool", "zone"):
                if item[field] is None:
                    mc["warnings"].append(f"{item['id']}: {field} metadata missing")
            for metric in ("capacity", "allocatable"):
                if any(item[metric][field] is None for field in ("cpu_mc", "mem_mib", "pods")):
                    mc["warnings"].append(f"{item['id']}: {metric} metadata missing")
            for metric, field, factor in (("node_cpu", "cpu_mc", 1000), ("node_memory", "mem_mib", 1 / 2**20)):
                total, complete, measured = 0.0, True, False
                for t, end in intervals[:-1]:
                    values = node_gauges.get((metric, name, t), [])
                    other_gauge = "node_memory" if metric == "node_cpu" else "node_cpu"
                    other_present = bool(node_gauges.get((other_gauge, name, t)))
                    if t not in seen and not values and not other_present:
                        continue
                    measured = True
                    if len(values) != 1 or values[0] is None:
                        complete = False
                    else:
                        total += values[0] * (end - t)
                if complete and measured:
                    item["node_usage"][field] = total / window * factor
                else:
                    target = view["errors"] if metric in required else mc["warnings"]
                    target.append(f"{item['id']}: incomplete {metric} coverage")
            mc["nodes"].append(item)

        for metric in required:
            expected = bool(nodes) if metric.startswith("node_") or metric in ("capacity", "allocatable") else bool(pods)
            if metric == "node_info":
                expected = True
            elif metric == "pod_info":
                expected = bool(nodes or pods)
            if metric == "replicaset_owner":
                expected = any(l.get("owner_kind", "").lower() == "replicaset" for t in boundaries for l, _ in rows("pod_owner", t))
            if expected and not any(v is not None for t in boundaries for _, v in rows(metric, t)):
                view["errors"].append(f"{mc['id']}: empty required query {metric}")
        if not mc["hcps"] and any(p[0].startswith("ocm-") for p in pods):
            mc["warnings"].append(f"{mc['id']}: HostedCluster history missing; size stability unknown")
        mc["warnings"] = sorted(set(mc["warnings"]))
        view["management_clusters"].append(mc)
    view["errors"] = sorted(set(view["errors"]))
    if any("size stability unknown" in error for error in view["errors"]):
        view["suggestion"] = None
    view["warnings"] = sorted(set(view["warnings"]))
    return view
