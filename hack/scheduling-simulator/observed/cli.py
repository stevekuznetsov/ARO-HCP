"""Collect Grafana/Kusto observations, then publish an offline observed view.

Authentication stays in the collector: Azure CLI tokens are never written to bundles.
Run from the simulator directory: python -m observed.cli --help.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
from copy import deepcopy
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
import re
import subprocess
import shutil
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

from .process import _identity, process_bundle


GRAFANAS = {
    "prod": "https://arohcp-prod-g5d9a9akashnb5gd.suk.grafana.azure.com",
    "stg": "https://arohcp-stg-cackgmg7dtf6hzg3.suk.grafana.azure.com",
    "int": "https://aro-dev-int-dhbxfxe0hxc0fahf.eus.grafana.azure.com",
}
# Explicit known endpoints, not inferred from metric-region names. Other regions
# need --kusto mappings; absence blocks publication unless --allow-partial is set.
KUSTOS = {
    "int/uksouth": "https://hcp-int-uk.uksouth.kusto.windows.net",
    "stg/uksouth": "https://hcp-stg-uk-2.uksouth.kusto.windows.net",
    "stg/westus3": "https://hcp-stg-us-1.westus3.kusto.windows.net",
    "prod/eastus2euap": "https://hcp-prod-usc.eastus2.kusto.windows.net",
    "prod/switzerlandnorth": "https://hcp-prod-ch-2.switzerlandnorth.kusto.windows.net",
    "prod/australiaeast": "https://hcp-prod-au.australiaeast.kusto.windows.net",
    "prod/brazilsouth": "https://hcp-prod-br.brazilsouth.kusto.windows.net",
    "prod/canadacentral": "https://hcp-prod-ca.canadacentral.kusto.windows.net",
    "prod/centralindia": "https://hcp-prod-in.centralindia.kusto.windows.net",
    "prod/uksouth": "https://hcp-prod-uk.uksouth.kusto.windows.net",
    "prod/westeurope": "https://hcp-prod-eu.westeurope.kusto.windows.net",
    "prod/eastus2": "https://hcp-prod-us.eastus2.kusto.windows.net",
    "prod/westus": "https://hcp-prod-us.eastus2.kusto.windows.net",
}
GRAFANA_RESOURCE = "ce34e7e5-485f-4d76-964f-b3d2b16d1e4f"
CACHE_VERSION = 1


def write_json(path, value):
    # Unique sibling files keep concurrent writers/readers from seeing partial JSON.
    temp = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, prefix=path.name + ".",
                                         suffix=".tmp", delete=False) as stream:
            temp = Path(stream.name)
            json.dump(value, stream, allow_nan=False)
        temp.replace(path)
    finally:
        if temp is not None:
            temp.unlink(missing_ok=True)


def duration(text):
    match = re.fullmatch(r"(\d+)(s|m|h|d)", text)
    if not match or int(match[1]) <= 0:
        raise argparse.ArgumentTypeError("use a positive duration, e.g. 1h or 30s")
    return int(match[1]) * {"s": 1, "m": 60, "h": 3600, "d": 86400}[match[2]]


def timestamp(text):
    try:
        value = datetime.fromisoformat(text.replace("Z", "+00:00"))
        if value.tzinfo is None:
            raise ValueError()
        return int(value.timestamp())
    except ValueError:
        raise argparse.ArgumentTypeError("timestamp must include timezone, e.g. 2026-09-08T12:00:00Z")


def iso(epoch):
    return datetime.fromtimestamp(epoch, timezone.utc).isoformat().replace("+00:00", "Z")


def mappings(defaults, overrides):
    result = dict(defaults)
    for item in overrides:
        key, sep, url = item.partition("=")
        if not sep or not url.startswith("https://"):
            raise ValueError("mapping must be KEY=https://host")
        result[key] = url.rstrip("/").removesuffix("/dashboards")
    return result


class Client:
    def __init__(self, cache_dir=None, refresh=False, at=None):
        self.tokens = {}
        self.lock = threading.Lock()
        self.cache_dir = cache_dir
        self.refresh = refresh
        self.at = at

    def cached(self, url, resource=None, body=None, *, validate, snapshot=None):
        key = {"version": CACHE_VERSION, "url": url, "resource": resource,
               "body": body, "at": self.at}
        if snapshot is not None and not self.refresh:
            try:
                parsed = validate(snapshot)
                response = snapshot
            except (ValueError, RuntimeError, KeyError, TypeError, AttributeError, IndexError):
                snapshot = None
        path = None
        if self.cache_dir is not None:
            digest = hashlib.sha256(json.dumps(key, sort_keys=True).encode()).hexdigest()
            path = self.cache_dir / (digest + ".json")
            if not self.refresh and snapshot is None:
                try:
                    entry = json.loads(path.read_text())
                    if entry["key"] == key:
                        response = entry["response"]
                        return response, validate(response)
                except (OSError, ValueError, RuntimeError, KeyError, TypeError, AttributeError, IndexError):
                    pass
            # A failed refresh/retry must not resurrect an older cached success.
            if self.refresh or snapshot is None:
                path.unlink(missing_ok=True)
        if snapshot is None or self.refresh:
            response = self.request(url, resource, body)
            try:
                parsed = validate(response)
            except Exception as exc:
                exc.response = response
                raise
        if path is not None:
            path.parent.mkdir(parents=True, exist_ok=True)
            write_json(path, {"key": key, "response": response})
        return response, parsed

    def token(self, resource):
        with self.lock:
            if resource not in self.tokens or self.tokens[resource][0] < time.time():
                proc = subprocess.run(["az", "account", "get-access-token", "--resource", resource,
                                       "--output", "json"], capture_output=True, text=True)
                if proc.returncode:
                    raise RuntimeError("Azure CLI authentication failed; authenticate for the target environment and retry")
                data = json.loads(proc.stdout)
                self.tokens[resource] = (time.time() + 1200, data["accessToken"])
            return self.tokens[resource][1]

    def request(self, url, resource=None, body=None):
        for attempt in range(3):
            headers = {"Content-Type": "application/json"}
            if resource:
                headers["Authorization"] = "Bearer " + self.token(resource)
            req = urllib.request.Request(url, data=json.dumps(body).encode() if body is not None else None,
                                         headers=headers)
            try:
                class NoRedirect(urllib.request.HTTPRedirectHandler):
                    def redirect_request(self, req, fp, code, msg, headers, newurl):
                        raise RuntimeError("Refusing HTTP redirect on authenticated telemetry request")
                with urllib.request.build_opener(NoRedirect).open(req, timeout=120) as response:
                    return json.load(response)
            except urllib.error.HTTPError as exc:
                if exc.code not in (429, 500, 502, 503, 504) or attempt == 2:
                    detail = exc.read(8192).decode(errors="replace")
                    raise RuntimeError(f"HTTP {exc.code} from {url.split('/api/')[0]}: {detail}") from None
            except (urllib.error.URLError, TimeoutError):
                if attempt == 2:
                    raise RuntimeError(f"Connection failed for {url.split('/api/')[0]}") from None
            time.sleep(2 ** attempt)


def frames(response):
    if response.get("error"):
        raise RuntimeError("Grafana query failed: " + str(response["error"]))
    result = response.get("results", {}).get("A")
    if result is None or result.get("error") or result.get("status", 200) not in (0, 200):
        raise RuntimeError("Grafana query failed: " + str((result or {}).get("error", "missing result")))
    series = []
    for frame in result.get("frames", []):
        fields = frame["schema"]["fields"]
        values = frame["data"]["values"]
        if len(fields) != len(values) or len({len(v) for v in values}) > 1:
            raise ValueError("Grafana frame has mismatched fields or sample lengths")
        if not fields and not values:
            continue
        times = next((values[i] for i, f in enumerate(fields) if f.get("type") == "time"), None)
        if times is None:
            raise ValueError("Grafana frame has no time field")
        for i, field in enumerate(fields):
            if field.get("type") != "number":
                continue
            series.append({"labels": field.get("labels", {}),
                           "samples": [[t / 1000, v] for t, v in zip(times, values[i])]})
    return series


def query(client, base, uid, expr, start, at, step, instant=False, snapshot=None):
    body = {"from": str(start * 1000), "to": str(at * 1000), "queries": [{
        "refId": "A", "datasource": {"type": "prometheus", "uid": uid}, "expr": expr,
        "instant": instant, "range": not instant, "intervalMs": step * 1000,
        "maxDataPoints": (at - start) // step + 1,
    }]}

    def validate(response):
        series = frames(response)
        if not instant:
            validate_grid(series, start, at, step, response, allow_sparse=True)
        return series

    return client.cached(base + "/api/ds/query", GRAFANA_RESOURCE, body, validate=validate, snapshot=snapshot)


def validate_grid(series, start, at, step, response=None, allow_sparse=False):
    verified_step = False
    if response:
        for frame in response.get("results", {}).get("A", {}).get("frames", []):
            executed = frame.get("schema", {}).get("meta", {}).get("executedQueryString", "")
            match = re.search(r"Step:\s*((?:\d+(?:\.\d+)?[smhd])+)", executed)
            if match:
                seconds = sum(float(n) * {"s": 1, "m": 60, "h": 3600, "d": 86400}[u]
                              for n, u in re.findall(r"(\d+(?:\.\d+)?)([smhd])", match[1]))
                if seconds != step:
                    raise ValueError("Grafana executed a different sampling step; recollect with the reported step")
                verified_step = True
    expected = list(range(start, at + 1, step))
    for item in series:
        times = [t for t, _ in item["samples"]]
        # Only executed-step metadata distinguishes aligned missing ticks from a
        # coarser grid. Keep gaps missing, never interpret them as inactivity.
        if times and (any(t not in expected for t in times) or
                      any(b <= a or ((b - a != step) and not (allow_sparse and verified_step)) for a, b in zip(times, times[1:]))):
            raise ValueError("Grafana returned a different sampling grid; choose a larger --step and recollect")


def kusto_query(env, clusters, search_start, at):
    # Query baseline + all observations in the search horizon, rather than take 1
    # or filtering deletes before reducing latest state.
    return f'''let T=datetime({iso(at)});
let S=datetime({iso(search_start)});
let observations=kubernetesResourceSnapshots
| where timestamp <= T
| where environment == {json.dumps(env)} and cluster in ({','.join(json.dumps(c) for c in clusters)})
| where (apiVersion == 'hypershift.openshift.io/v1beta1' and objectKind == 'HostedCluster')
     or (apiVersion == 'v1' and objectKind == 'Node')
| project environment,region,cluster,timestamp,event,uid,namespace,name,objectKind,
    object=bag_pack('apiVersion',apiVersion,'kind',objectKind,
        'spec', iff(objectKind == 'Node', bag_pack('providerID',object.spec.providerID), dynamic(null)),
        'status', iff(objectKind == 'Node', bag_pack('capacity',object.status.capacity,
            'allocatable',object.status.allocatable), dynamic(null)),
        'metadata', bag_pack('namespace',namespace,'name',name,'uid',uid,
        'labels',bag_pack(
            'hypershift.openshift.io/hosted-cluster-size',object.metadata.labels['hypershift.openshift.io/hosted-cluster-size'],
            'node.kubernetes.io/instance-type',object.metadata.labels['node.kubernetes.io/instance-type'],
            'kubernetes.azure.com/agentpool',object.metadata.labels['kubernetes.azure.com/agentpool'],
            'agentpool',object.metadata.labels['agentpool'],
            'topology.kubernetes.io/zone',object.metadata.labels['topology.kubernetes.io/zone']),
        'creationTimestamp',object.metadata.creationTimestamp,
        'deletionTimestamp',object.metadata.deletionTimestamp,'resourceVersion',object.metadata.resourceVersion));
union (observations | where timestamp <= S | summarize arg_max(timestamp, *) by environment,region,cluster,objectKind,uid),
      (observations | where timestamp > S)
| project environment,region,cluster,timestamp,event,uid,namespace,name,objectKind,object
| order by timestamp asc'''


def kusto_rows(response):
    tables = response.get("Tables", [])
    if response.get("error") or not tables:
        raise RuntimeError("Kusto query returned an error or no tables")
    rows, warnings = [], []
    found_primary = False
    for table in tables:
        cols = [c["ColumnName"] for c in table["Columns"]]
        primary = {"object", "timestamp", "uid"}.issubset(cols)
        found_primary |= primary
        if not primary and "Severity" not in cols:
            continue
        for values in table["Rows"]:
            if isinstance(values, dict) and values.get("Exceptions"):
                raise RuntimeError("Kusto returned partial results: " + json.dumps(values["Exceptions"]))
            if primary and len(values) != len(cols):
                raise ValueError("Kusto row has mismatched columns")
            row = dict(zip(cols, values))
            if "Severity" in cols:
                if row["Severity"] <= 2 or row.get("StatusCode", 0) != 0:
                    raise RuntimeError("Kusto query incomplete: " + str(row.get("StatusDescription")))
                if row["Severity"] == 3:
                    warnings.append("Kusto warning: " + str(row.get("StatusDescription")))
            if primary:
                if isinstance(row["object"], str):
                    row["object"] = json.loads(row["object"])
                if not isinstance(row["object"], dict):
                    raise ValueError("Kusto object must be a JSON object")
                rows.append(row)
    if not found_primary:
        raise RuntimeError("Kusto response has no HostedCluster result table")
    return rows, warnings


def regional_preflight(args, regional):
    """Retain HC state runs and known-size resizes, independently of collection."""
    env, region = regional["environment"], regional["region"]
    names = sorted({name.removeprefix(env + "/") for name in regional["clusters"]})
    start = regional["original_selected_at"] - args.window - args.size_settle
    end = regional["search_end"]
    root = args.output / "regions" / f"{env}-{region}"
    root.mkdir(parents=True, exist_ok=True)
    path = root / "preflight.json"
    endpoint = mappings(KUSTOS, args.kusto).get(env + "/" + region)
    # Only the baseline scans all history. Daily changes retain their first UID
    # state so Python can detect resizes across chunk boundaries, including A -> B -> A.
    projection = f'''
| where environment == {json.dumps(env)} and region == {json.dumps(region)}
| where cluster in ({','.join(json.dumps(c) for c in names)})
| where apiVersion == 'hypershift.openshift.io/v1beta1' and objectKind == 'HostedCluster'
| project environment,region,cluster,timestamp,uid,namespace,name,objectKind,
    event=tostring(column_ifexists('event', dynamic(null))),
    size=tostring(object.metadata.labels['hypershift.openshift.io/hosted-cluster-size'])'''
    runs = '''
| extend isDeleted=tolower(event) in ('delete', 'deleted')
| sort by environment asc,region asc,cluster asc,uid asc,timestamp asc
| serialize
| extend firstUID=row_number() == 1 or environment != prev(environment)
    or region != prev(region) or cluster != prev(cluster) or uid != prev(uid),
    previousSize=prev(size), previousDeleted=prev(isDeleted)
| where firstUID or size != previousSize or isDeleted != previousDeleted'''
    packed = '''
| project environment,region,cluster,timestamp,event,uid,namespace,name,objectKind,
    object=bag_pack('kind',objectKind,'metadata',bag_pack(
        'namespace',namespace,'name',name,'uid',uid,
        'labels',bag_pack('hypershift.openshift.io/hosted-cluster-size',size)))
| order by timestamp asc'''
    queries = [f'''let S=datetime({iso(start)});
kubernetesResourceSnapshots
| where timestamp < S''' + projection + '''
| summarize arg_max(timestamp, *) by environment,region,cluster,uid''' + packed]
    # Half-open chunks, with a final exact cap to preserve the inclusive search end.
    for chunk_start in range(start, end + 1, 86400):
        chunk_end = min(chunk_start + 86400, end + 1)
        bounds = f'''let S=datetime({iso(chunk_start)});
let T=datetime({iso(chunk_end)});
kubernetesResourceSnapshots
| where timestamp >= S and timestamp < T'''
        if chunk_end > end:
            bounds += f'\n| where timestamp <= datetime({iso(end)})'
        queries.append(bounds + projection + runs + packed)
    config = {"environment": env, "region": region, "clusters": names,
              "start": start, "end": end, "url": endpoint, "database": "ServiceLogs"}
    prior = None
    try:
        saved = json.loads(path.read_text())
        if all(saved.get("config", {}).get(key) == config[key] for key in ("url", "database", "end")):
            prior = saved
    except (OSError, ValueError, AttributeError):
        pass
    prior_queries = {q["query"]: q for q in (prior or {}).get("queries", [])
                     if not q.get("error") and not args.refresh}
    record = {"schema_version": 1, "config": config, "transitions": [], "warnings": [],
              "queries": [dict(prior_queries.get(kql, {}), query=kql) for kql in queries]}
    try:
        if not endpoint:
            raise ValueError(f"configure --kusto {env}/{region}=https://query-endpoint for HCP metadata")
        client = Client(args.cache_dir, args.refresh, end)
        record["authentication_metadata"], audience = client.cached(
            endpoint + "/v1/rest/auth/metadata", validate=kusto_audience,
            snapshot=(prior or {}).get("authentication_metadata"))
        rows = []
        for query_record in record["queries"]:
            try:
                query_record["response"], (chunk_rows, warnings) = client.cached(
                    endpoint + "/v1/rest/query", audience,
                    {"db": "ServiceLogs", "csl": query_record["query"]},
                    validate=kusto_rows, snapshot=query_record.get("response"))
                rows.extend(chunk_rows)
                record["warnings"].extend(warnings)
            except Exception as exc:
                query_record.pop("response", None)
                query_record["error"] = str(exc)
                if hasattr(exc, "response"):
                    query_record["response"] = exc.response
                raise
            finally:
                write_json(path, record)
        # Preserve the combined response shape for existing preflight readers.
        record["response"] = {"Tables": [table for q in record["queries"] for table in q["response"]["Tables"]]}
        histories = {}
        for row in rows:
            if (row.get("environment") != env or row.get("region") != region or
                    row.get("cluster") not in names or
                    row.get("objectKind", row["object"].get("kind")) != "HostedCluster"):
                continue
            when = datetime.fromisoformat(row["timestamp"].replace("Z", "+00:00"))
            if when.tzinfo is None:
                raise ValueError("HostedCluster timestamp must include timezone")
            tick = when.timestamp()
            if tick <= end:
                histories.setdefault((row["cluster"], row["uid"]), []).append((tick, row))
        for (cluster, uid), history in sorted(histories.items()):
            previous, deleted, seen = None, False, set()
            for tick, row in sorted(history, key=lambda item: item[0]):
                identity = json.dumps(row, sort_keys=True)
                if identity in seen:
                    continue
                seen.add(identity)
                metadata = row["object"].get("metadata", {})
                if str(row.get("event", "")).lower() in ("delete", "deleted"):
                    deleted = True
                if deleted:
                    continue
                size = (metadata.get("labels") or {}).get("hypershift.openshift.io/hosted-cluster-size") or None
                # Initial assignments are not resizes. Unknown intervals do not
                # erase the last known size; detailed collection validates them.
                if size is None:
                    continue
                if previous is not None and size != previous and tick >= start:
                    record["transitions"].append({"environment": env, "region": region,
                        "cluster": cluster, "uid": uid, "at": tick, "from": previous, "to": size})
                previous = size
        record["transitions"].sort(key=lambda item: (item["at"], item["cluster"], item["uid"]))
    except Exception as exc:
        record["error"] = str(exc)
        if hasattr(exc, "response"):
            record["response"] = exc.response
        raise
    finally:
        write_json(path, record)
    return record


def collect_regional_peak(args, selection):
    """Collect independent regional windows; never replace a historical root raw bundle."""
    from .peak import forward_window

    args.output.mkdir(parents=True, exist_ok=True)
    (args.output / "view.json").unlink(missing_ok=True)
    manifest = {"schema_version": 1, "scope": "regional", "search_end": selection["search_end"],
                "regions": [], "errors": ["Regional collection did not finish"],
                "warnings": list(selection.get("warnings", []))}
    merged = {"schema_version": 1, "mode": "observed", "at": None, "start": None,
              "window_seconds": args.window, "step_seconds": args.step,
              "generated_at": iso(time.time()), "sources": [], "errors": [],
              "warnings": [*manifest["warnings"],
                  "Mixed-time regional windows: these are not simultaneous fleet totals."],
              "transitions": [], "suggestion": None, "regional_windows": manifest["regions"],
              "management_clusters": []}
    for regional in selection["regions"]:
        if getattr(args, "region", None) and regional["region"] not in args.region:
            continue
        manifest["regions"].append({"environment": regional["environment"], "region": regional["region"],
                                    "status": "pending", "peak_selection": deepcopy(regional)})
    write_json(args.output / "regional-manifest.json", manifest)
    for entry in manifest["regions"]:
        regional = entry["peak_selection"]
        env, region = entry["environment"], entry["region"]
        root = args.output / "regions" / f"{env}-{region}"
        original_at = regional["original_selected_at"]
        regional.update(original_start=original_at - args.window, settle_seconds=args.size_settle)
        entry.update(status="preflight", preflight=str(root / "preflight.json"), errors=[], warnings=[])
        write_json(args.output / "regional-manifest.json", manifest)
        try:
            preflight = regional_preflight(args, regional)
            entry["warnings"].extend(preflight["warnings"])
            entry["transitions"] = preflight["transitions"]
            adjusted_at = forward_window(original_at, args.window, args.step, args.size_settle,
                                         [t["at"] for t in preflight["transitions"]], regional["search_end"])
            regional.update(adjusted_at=adjusted_at,
                            adjusted_start=adjusted_at - args.window, adjustment_seconds=adjusted_at - original_at,
                            preflight=entry["preflight"])
            # score and selected_at still describe the original ranked window, not this adjustment.
            child = deepcopy(args)
            child.environment, child.region = [env], [region]
            child.cluster = "^(?:" + "|".join(re.escape(name.removeprefix(env + "/"))
                                             for name in regional["clusters"]) + ")$"
            child.at, child.output = adjusted_at, root / str(adjusted_at)
            child.peak_selection = deepcopy(regional)
            entry.update(status="collecting", at=iso(adjusted_at), start=iso(adjusted_at - args.window),
                         output=str(child.output), raw=str(child.output / "raw.json"))
            write_json(args.output / "regional-manifest.json", manifest)
            print(f"{env}/{region}: original peak {iso(original_at)}; collecting "
                  f"{entry['start']} .. {entry['at']} (original {regional['metric']} score {regional['score']})",
                  flush=True)
            status = collect(child)
            diagnostics_path = child.output / "manifest.json"
            if diagnostics_path.exists():
                diagnostics = json.loads(diagnostics_path.read_text())
                entry["errors"].extend(diagnostics.get("errors", []))
                entry["warnings"].extend(diagnostics.get("warnings", []))
            view_path = child.output / "view.json"
            if status and not entry["errors"]:
                entry["errors"].append(f"Detailed collection returned {status}")
            if not view_path.exists() or (entry["errors"] and not args.allow_partial):
                raise RuntimeError("Detailed collection did not publish an allowed view")
            view = json.loads(view_path.read_text())
            entry["errors"] = list(dict.fromkeys([*entry["errors"], *view["errors"]]))
            if entry["errors"] and not args.allow_partial:
                raise RuntimeError("Detailed view has errors; merged publication refused")
            entry.update(status="partial" if entry["errors"] else "success", view=str(view_path))
            for mc in view["management_clusters"]:
                mc.update(at=entry["at"], start=entry["start"], peak_selection=deepcopy(regional))
                merged["management_clusters"].append(mc)
            for source in view["sources"]:
                if source not in merged["sources"]:
                    merged["sources"].append(source)
            entry["warnings"] = list(dict.fromkeys([*entry["warnings"], *view["warnings"]]))
            merged["transitions"].extend(view["transitions"])
        except Exception as exc:
            entry.update(status="blocked", reason=str(exc))
            entry["errors"].append(str(exc))
        merged["errors"].extend(f"{env}/{region}: {error}" for error in entry["errors"])
        merged["warnings"].extend(f"{env}/{region}: {warning}" for warning in entry["warnings"])
        manifest["errors"] = ["Regional collection did not finish", *merged["errors"]]
        write_json(args.output / "regional-manifest.json", manifest)
    if not merged["management_clusters"]:
        merged["errors"].append("No regional management-cluster views available")
    manifest.update(errors=merged["errors"], warnings=merged["warnings"], partial=bool(merged["errors"]))
    write_json(args.output / "regional-manifest.json", manifest)
    for error in merged["errors"]:
        print(error, file=sys.stderr)
    if merged["management_clusters"] and (not merged["errors"] or args.allow_partial):
        write_json(args.output / "view.json", merged)
        print(f"Published mixed-time regional view {args.output / 'view.json'}", flush=True)
    else:
        print("No merged view published; inspect regional-manifest.json. Regional bundles retained.", file=sys.stderr)
    return 2 if merged["errors"] else 0


def datasources(response):
    if not isinstance(response, list) or any(
            not isinstance(d, dict) or not isinstance(d.get("uid"), str) or
            not isinstance(d.get("type"), str) for d in response):
        raise ValueError("Invalid Grafana datasource discovery response")
    return response


def kusto_audience(response):
    aad = response["AzureAD"]
    audience = aad["KustoServiceResourceId"]
    if response.get("error") or not isinstance(audience, str) or not audience.startswith("https://"):
        raise ValueError("Invalid Kusto authentication metadata")
    if aad.get("LoginMfaRequired"):
        audience = audience.replace(".kusto.", ".kustomfa.")
    return audience


def collect(args):
    prior = None
    if args.output.exists() and any(args.output.iterdir()):
        if not (args.output / "raw.json").is_file():
            if not getattr(args, "peak_selection", None):
                raise ValueError("output directory must be empty or contain raw.json to resume")
        else:
            prior = json.loads((args.output / "raw.json").read_text())
        if prior is not None and (not isinstance(prior, dict) or prior.get("schema_version") != 1 or not all(
                key in prior for key in ("at", "start", "window_seconds", "step_seconds",
                                          "search_start", "settle_seconds", "sources", "queries"))):
            raise ValueError("raw.json is not a resumable collection")
    at = args.at if args.at is not None else prior["at"] if prior is not None else int(time.time() - 300)
    if (args.at is not None or prior is not None) and at % args.step:
        raise ValueError("--at must align to --step (Grafana aligns range samples); choose an aligned timestamp")
    at -= at % args.step
    if args.window % args.step or args.step > args.window:
        raise ValueError("window must be a multiple of step")
    if args.search_back < args.window:
        raise ValueError("search-back must be at least window")
    grafanas = mappings(GRAFANAS, args.grafana)
    kustos = mappings(KUSTOS, args.kusto)
    selected = sorted(set(args.environment or grafanas))
    mc_pattern = re.compile(args.cluster)
    config = {"at": at, "window": args.window, "step": args.step,
              "environments": selected, "cluster": args.cluster,
              "grafana": {env: grafanas[env] for env in selected}, "kusto": kustos,
              "settle": args.size_settle, "search": args.search_back}
    if getattr(args, "region", None):
        config["regions"] = sorted(set(args.region))
    if prior is not None:
        previous = prior.get("collection_config")
        if previous is None and (args.output / "manifest.json").is_file():
            previous = json.loads((args.output / "manifest.json").read_text()).get("collection_config")
        if previous is None:
            # Legacy bundles did not record the regex. Reparse original responses
            # below, then apply the requested filter instead of trusting filtered series.
            previous = {"at": prior["at"], "window": prior["window_seconds"],
                        "step": prior["step_seconds"], "settle": prior["settle_seconds"],
                        "search": prior["at"] - prior["search_start"], "cluster": args.cluster,
                        "environments": sorted(s["environment"] for s in prior["sources"]),
                        "grafana": {s["environment"]: s["url"] for s in prior["sources"]}}
        changed = [key for key in config.keys() | previous.keys()
                   if key != "kusto" and config.get(key) != previous.get(key)]
        if changed or prior["start"] != at - args.window:
            raise ValueError("cannot change collection settings in-place (" + ", ".join(changed) +
                             "); use a new output directory")
    args.output.mkdir(parents=True, exist_ok=True)
    for name in ("view.json", "manifest.json"):
        (args.output / name).unlink(missing_ok=True)
    start = at - args.window
    raw = {"schema_version": 1, "at": at, "start": start, "window_seconds": args.window,
           "step_seconds": args.step, "search_start": at - args.search_back,
           "settle_seconds": args.size_settle, "sources": [], "queries": [], "snapshots": [], "node_snapshots": [],
           "collection_config": config,
            "errors": [], "warnings": ["Coverage is telemetry-observed MCs, not an independent deployed-fleet inventory."]}
    peak_selection = getattr(args, "peak_selection", None) or (prior or {}).get("peak_selection")
    if peak_selection:
        raw["peak_selection"] = {k: v for k, v in peak_selection.items() if k not in ("queries", "sources")}
    interrupted = "Collection did not finish; raw checkpoint is incomplete"
    raw["errors"].append(interrupted)

    def checkpoint():
        write_json(args.output / "raw.json", raw)

    checkpoint()
    client = Client(args.cache_dir, args.refresh, at)
    prior_sources = {s["environment"]: s for s in (prior or {}).get("sources", [])}
    prior_queries = {}
    scope = ("environment", "region", "cluster", "datasource", "metric", "expression", "start", "end", "step")
    for row in (prior or {}).get("queries", []):
        if not row.get("error") or row.get("response") is not None:
            base = row.get("url", prior_sources.get(row.get("environment"), {}).get("url"))
            prior_queries[(base, row.get("instant", False), *(row.get(k) for k in scope))] = row
    print(f"Collecting {iso(start)} .. {iso(at)} at {args.step}s resolution", flush=True)
    jobs = []
    regions = []

    def capture(env, region, base, uid, cluster, metric, expr, required=True, instant=False):
        row = {"environment": env, "region": region, "cluster": cluster, "datasource": uid,
               "metric": metric, "required": required, "expression": expr,
               "start": start, "end": at, "step": args.step, "series": [], "url": base, "instant": instant}
        old = prior_queries.get((base, instant, *(row[k] for k in scope)))
        try:
            if old is not None and not args.refresh:
                row["response"], row["series"] = query(
                    client, base, uid, expr, start, at, args.step, instant, snapshot=old.get("response"))
            else:
                row["response"], row["series"] = query(client, base, uid, expr, start, at, args.step, instant)
            if not instant:
                if metric in ("node_info", "node_labels", "capacity", "allocatable", "pod_info",
                              "pod_phase", "pod_owner", "replicaset_owner", "requests", "init_requests"):
                    # A scrape replica's hole is harmless when another observes the same identity.
                    coverage = {}
                    for series in row["series"]:
                        labels = series["labels"]
                        key = (labels.get("cluster") or cluster, _identity(metric, labels))
                        coverage.setdefault(key, set()).update(
                            t for t, value in series["samples"]
                            if value is not None and math.isfinite(float(value)))
                    times = [sorted(ticks) for ticks in coverage.values()]
                else:
                    times = [[t for t, _ in s["samples"]] for s in row["series"]]
                if any(b - a > args.step for ticks in times for a, b in zip(ticks, ticks[1:])):
                    row["error"] = "Query has interior sampling gaps; retained observations, but absence during gaps is unverified"
        except Exception as exc:
            row["error"] = str(exc)
            if hasattr(exc, "response"):
                row["response"] = exc.response
        return row

    for env in sorted(selected):
        base = grafanas[env]
        source = {"environment": env, "url": base, "datasources": []}
        raw["sources"].append(source)
        try:
            old_source = prior_sources.get(env, {})
            discovery_response, discovered = client.cached(
                base + "/api/datasources", GRAFANA_RESOURCE, validate=datasources,
                snapshot=old_source.get("discovery") if old_source.get("url") == base else None)
            source["discovery"] = discovery_response
        except Exception as exc:
            raw["errors"].append(f"{env}: datasource discovery failed: {exc}")
            continue
        uids = {d["uid"] for d in discovered if d["type"] == "prometheus"}
        if not any(u.startswith("services-") for u in uids):
            raw["errors"].append(f"{env}: no services datasources discovered")
        for uid in sorted(uids):
            if not uid.startswith("services-") or len(uid.removeprefix("services-")) <= 3:
                continue
            region = uid.removeprefix("services-")
            if region.startswith(("int-", "stg-", "prod-")):
                if not region.startswith(env + "-"):
                    continue
                region = region.removeprefix(env + "-")
            if getattr(args, "region", None) and region not in args.region:
                continue
            hcp_uid = uid.replace("services-", "hcps-", 1)
            source["datasources"].append(uid)
            discovery = capture(env, region, base, uid, None, "node_info", f'kube_node_info{{cluster=~"{env}-.*-mgmt-[0-9]+"}}')
            # Filter raw series only after recording original response; selectors may
            # intentionally target one MC for a probe.
            discovery["series"] = [s for s in discovery["series"] if mc_pattern.search(s["labels"].get("cluster", ""))]
            raw["queries"].append(discovery)
            checkpoint()
            names = sorted({s["labels"]["cluster"] for s in discovery["series"]})
            regions.append((env, region, names))
            for name in names:
                print(f"Discovered {env}/{region}/{name}", flush=True)
                selector = "cluster=" + json.dumps(name)
                for metric, expr, required in (
                    ("node_labels", f"kube_node_labels{{{selector}}}", False),
                    ("capacity", f"kube_node_status_capacity{{{selector}}}", True),
                    ("allocatable", f"kube_node_status_allocatable{{{selector}}}", True),
                    ("node_cpu", f'sum by (cluster,node,instance) (rate(node_cpu_seconds_total{{{selector},mode!="idle",mode!="guest",mode!="guest_nice"}}[5m]))', True),
                    ("node_memory", f'node_memory_MemTotal_bytes{{{selector}}} - node_memory_MemAvailable_bytes{{{selector}}}', True),
                ):
                    jobs.append((env, region, base, uid, name, metric, expr, required, False))
                # Namespace discovery uses pod inventory, not usage, so pending and
                # unsampled pods are not silently omitted. KSM routing splits HCPs.
                for source_uid, ns_filter in ((uid, 'namespace!~"ocm-.*"'), (hcp_uid, 'namespace=~"ocm-.*"')):
                    if source_uid not in uids:
                        raw["errors"].append(f"{env}/{name}: missing {source_uid} datasource")
                        continue
                    inv = capture(env, region, base, source_uid, name, "pod_info", f'kube_pod_info{{{selector},{ns_filter}}}')
                    raw["queries"].append(inv)
                    checkpoint()
                    namespaces = sorted({s["labels"]["namespace"] for s in inv["series"] if "namespace" in s["labels"]})
                    for ns in namespaces:
                        sel = selector + ",namespace=" + json.dumps(ns)
                        for metric, prom_metric, required in (
                            ("pod_phase", "kube_pod_status_phase", True),
                            ("pod_owner", "kube_pod_owner", False),
                            ("replicaset_owner", "kube_replicaset_owner", False),
                            ("requests", "kube_pod_container_resource_requests", True),
                            ("init_requests", "kube_pod_init_container_resource_requests", False),
                        ):
                            jobs.append((env, region, base, source_uid, name, metric,
                                         f"{prom_metric}{{{sel}}}", required, False))
                        for metric, expr in (
                            ("cpu", f'rate(container_cpu_usage_seconds_total{{{sel},container!="",container!="POD"}}[5m])'),
                            ("memory", f'container_memory_working_set_bytes{{{sel},container!="",container!="POD"}}'),
                        ):
                            jobs.append((env, region, base, uid, name, metric, expr, True, False))
    # Bounded concurrency avoids monopolizing Grafana/AMW with fleet-wide queries.
    try:
        with ThreadPoolExecutor(max_workers=args.workers) as pool:
            futures = [pool.submit(capture, *job) for job in jobs]
            for i, future in enumerate(as_completed(futures), 1):
                row = future.result()
                raw["queries"].append(row)
                print(f"[{i}/{len(jobs)}] {row['environment']}/{row['cluster']} {row['metric']} "
                      + ("FAILED " + row["error"] if row.get("error") else f"{len(row['series'])} series"), flush=True)
                # Individual responses are already durable in the query cache.
                # Avoid repeatedly serializing a fleet-sized raw bundle.
                if i % 250 == 0:
                    checkpoint()
        for env, region, names in regions:
            if not names:
                continue
            endpoint = kustos.get(env + "/" + region)
            if not endpoint:
                raw["errors"].append(f"{env}/{region}: configure --kusto {env}/{region}=https://query-endpoint for HCP metadata")
                continue
            kql = kusto_query(env, names, raw["search_start"], at)
            record = {"environment": env, "region": region, "url": endpoint, "database": "ServiceLogs", "query": kql}
            raw.setdefault("kusto_queries", []).append(record)
            try:
                _, audience = client.cached(endpoint + "/v1/rest/auth/metadata", validate=kusto_audience)
                response, (rows, warnings) = client.cached(
                    endpoint + "/v1/rest/query", audience,
                    {"db": "ServiceLogs", "csl": kql}, validate=kusto_rows)
                record["response"] = response
                raw["warnings"].extend(f"{env}/{region}: {warning}" for warning in warnings)
                for row in rows:
                    destination = "node_snapshots" if row.get("objectKind", row["object"].get("kind")) == "Node" else "snapshots"
                    raw[destination].append(row)
                print(f"Collected HostedCluster and Node history {env}/{region}", flush=True)
            except Exception as exc:
                record["error"] = str(exc)
                if hasattr(exc, "response"):
                    record["response"] = exc.response
                raw["errors"].append(f"{env}/{region}: Kusto metadata query failed: {exc}")
            checkpoint()
        raw["errors"].remove(interrupted)
    finally:
        checkpoint()
    return publish(raw, args.output, args.allow_partial)


def publish(raw, output, allow_partial):
    target = output / "view.json"
    if target.exists():
        target.unlink()
    # Processing is pure apart from generated_at. Key the cached view by both its
    # inputs and processor source, so code fixes automatically reprocess old data.
    inputs = {k: v for k, v in raw.items() if k != "kusto_queries"}
    inputs["queries"] = sorted(
        ({k: v for k, v in q.items() if k != "response"} for q in raw.get("queries", [])),
        key=lambda q: (q.get("environment", ""), q.get("region", ""), q.get("cluster") or "", q.get("metric", ""), q.get("expression", "")))
    digest = hashlib.sha256(json.dumps(inputs, sort_keys=True).encode()
                            + Path(__file__).with_name("process.py").read_bytes()).hexdigest()
    processed_path = output / ".processed.json"
    view = None
    try:
        cached = json.loads(processed_path.read_text())
        required = ("management_clusters", "at", "start", "generated_at", "sources", "errors", "warnings", "transitions", "suggestion")
        if (cached["digest"] == digest and cached["view"].get("schema_version") == 1
                and all(k in cached["view"] for k in required)):
            view = cached["view"]
            print("Reusing processed view from disk", flush=True)
    except (OSError, ValueError, KeyError, TypeError, AttributeError):
        pass
    if view is None:
        view = process_bundle(raw)
        write_json(processed_path, {"digest": digest, "view": view})
    if not view["management_clusters"]:
        view["errors"].append("No management clusters discovered in the requested scope/window")
    if raw.get("peak_selection"):
        view["peak_selection"] = raw["peak_selection"]
    manifest = {k: view[k] for k in ("schema_version", "at", "start", "generated_at", "sources",
                                     "errors", "warnings", "transitions", "suggestion")}
    manifest["partial"] = bool(view["errors"])
    if raw.get("peak_selection"):
        manifest["peak_selection"] = raw["peak_selection"]
    if "collection_config" in raw:
        manifest["collection_config"] = raw["collection_config"]
    output.mkdir(parents=True, exist_ok=True)
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2, allow_nan=False))
    # A failed reprocess must not leave a stale previously published view behind.
    if view["errors"]:
        print("Collection is incomplete:", file=sys.stderr)
        for error in view["errors"][:20]:
            print("  " + error, file=sys.stderr)
        if len(view["errors"]) > 20:
            print(f"  ... {len(view['errors'])} errors total; see manifest.json", file=sys.stderr)
        if view["suggestion"]:
            print("Suggested rerun: python -m observed.cli collect " + view["suggestion"]["command"], file=sys.stderr)
        if not allow_partial:
            print("Raw data retained; no view published. Inspect manifest.json or reprocess with --allow-partial.", file=sys.stderr)
            return 2
    target.write_text(json.dumps(view, allow_nan=False))
    print(f"Published {target} ({len(view['management_clusters'])} MCs)", flush=True)
    return 2 if view["errors"] else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    collect_parser = sub.add_parser("collect", help="read Grafana/Kusto using current Azure CLI identity")
    collect_parser.add_argument("--at", type=timestamp, help="aligned RFC3339 end time; default prior T on resume, otherwise now minus 5 minutes")
    collect_parser.add_argument("--window", type=duration, help="averaging window (collect: 1h; peak: 15m)")
    collect_parser.add_argument("--step", type=duration, default=60)
    collect_parser.add_argument("--search-back", type=duration, default=86400)
    collect_parser.add_argument("--size-settle", type=duration, default=900)
    collect_parser.add_argument("--environment", action="append", choices=list(GRAFANAS))
    collect_parser.add_argument("--region", action="append", help="repeatable region filter for detailed collection")
    collect_parser.add_argument("--cluster", default=r".*-mgmt-\d+$", help="regex filter; default all MCs")
    collect_parser.add_argument("--grafana", action="append", default=[], metavar="ENV=https://host")
    collect_parser.add_argument("--kusto", action="append", default=[], metavar="ENV/REGION=https://host")
    collect_parser.add_argument("--workers", type=int, choices=range(1, 9), default=4)
    collect_parser.add_argument("--output", type=Path, default=Path("observed-data"))
    collect_parser.add_argument("--cache-dir", type=Path, default=Path(".cache/observed"))
    collect_parser.add_argument("--refresh", action="store_true", help="bypass prior raw records and cached responses")
    collect_parser.add_argument("--allow-partial", action="store_true")
    peak_parser = sub.add_parser("peak", parents=[collect_parser], add_help=False,
                                 help="find and collect independent regional node-usage peak windows")
    peak_parser.add_argument("--peak", choices=("cpu", "memory"), default="memory", help="rank absolute regional consumption")
    peak_parser.add_argument("--peak-lookback", type=duration, default=604800, help="search horizon (default 7d)")
    peak_parser.add_argument("--peak-end", type=timestamp, help="aligned search end; default frozen on resume, otherwise now minus 5 minutes")
    process_parser = sub.add_parser("process", help="regenerate view offline from raw bundle")
    process_parser.add_argument("raw", type=Path)
    process_parser.add_argument("--output", type=Path, default=Path("observed-data"))
    process_parser.add_argument("--allow-partial", action="store_true")
    regional_parser = sub.add_parser("publish-regional", help="publish completed regional captures offline")
    regional_parser.add_argument("root", type=Path)
    regional_parser.add_argument("--output", type=Path, default=Path("observed-data"))
    regional_parser.add_argument("--allow-partial", action="store_true")
    args = parser.parse_args()
    if args.command in ("collect", "peak") and args.window is None:
        args.window = 900 if args.command == "peak" else 3600
    try:
        if args.command == "publish-regional":
            from .publish import publish_regions
            return publish_regions(args.root, args.output, args.allow_partial)
        if args.command == "peak":
            if args.at is not None:
                raise ValueError("peak chooses --at; use --peak-end to bound the search")
            from .peak import search
            return collect_regional_peak(args, search(args))
        if args.command == "collect":
            return collect(args)
        if (args.output / "view.json").exists():
            (args.output / "view.json").unlink()
        raw = json.loads(args.raw.read_text())
        args.output.mkdir(parents=True, exist_ok=True)
        if args.raw.resolve() != (args.output / "raw.json").resolve():
            shutil.copyfile(args.raw, args.output / "raw.json")
        return publish(raw, args.output, args.allow_partial)
    except (ValueError, OSError, RuntimeError) as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
