"""Cached, regional peak search, independent of detailed collection.

Scores are mean absolute cores or bytes over [T-window, T), not utilization
percentages. Every telemetry-observed MC in a region's horizon must have inventory and
usage at every window tick and at T. This is not an independent fleet inventory,
and cluster aggregates cannot establish completeness of individual node scrapes.
"""
from collections import deque
import hashlib
import json
import math
from pathlib import Path
import re
import time
from urllib.parse import urlsplit


def rank_window(series, window_seconds=900, step_seconds=60,
                search_start=None, search_end=None):
    """Rank {env/cluster: {epoch: value}}; missing/nonfinite values invalidate ticks.

    Return selected_at, score, clusters, window_seconds and step_seconds. The
    sample at selected_at is required for coverage but excluded from the score.
    Exact ties select the latest end. No qualifying window raises ValueError.
    """
    if (not isinstance(step_seconds, int) or step_seconds <= 0 or
            not isinstance(window_seconds, int) or window_seconds < step_seconds or
            window_seconds % step_seconds):
        raise ValueError("window must be a positive multiple of step")
    times = [t for samples in series.values() for t in samples]
    if not series or not times:
        raise ValueError("No management clusters/data in the requested scope")
    start = min(times) if search_start is None else search_start
    end = max(times) if search_end is None else search_end
    if (not isinstance(start, (int, float)) or not isinstance(end, (int, float)) or
            not math.isfinite(start) or not math.isfinite(end) or
            start % step_seconds or end % step_seconds or end - start < window_seconds):
        raise ValueError("search bounds must align to step and span at least window")
    size = window_seconds // step_seconds
    window = deque()
    best = None
    for tick in range(int(start), int(end) + 1, step_seconds):
        values = [samples.get(tick) for samples in series.values()]
        if any(v is None or not isinstance(v, (int, float)) or
               not math.isfinite(v) or v < 0 for v in values):
            window.clear()
            continue
        if len(window) == size:
            score = math.fsum(window) / size
            if best is None or score >= best["score"]:
                best = {"selected_at": tick, "score": score,
                        "clusters": sorted(series), "window_seconds": window_seconds,
                        "step_seconds": step_seconds}
            window.popleft()
        value = math.fsum(values)
        window.append(value)
    if best is None:
        raise ValueError("No common window with full management-cluster coverage")
    return best


def forward_window(selected_at, window, step, settle, transitions, search_end):
    """Move a window forward past transitions and settling, within search_end.

    Times are epoch seconds; transitions must already be filtered for relevance.
    Both window endpoints exclude transitions, but settling may end at the start.
    Metadata staleness and class sizes are independent of this calculation.
    """
    if (not isinstance(step, int) or step <= 0 or not isinstance(window, int) or
            window < step or window % step):
        raise ValueError("window must be a positive multiple of step")
    transitions = list(transitions)
    if (any(not isinstance(t, (int, float)) or not math.isfinite(t)
            for t in [selected_at, search_end, settle, *transitions]) or settle < 0):
        raise ValueError("times must be finite and settle must be nonnegative")
    end = selected_at
    while True:
        if end > search_end:
            raise ValueError("forward window exceeds search_end")
        start = end - window
        overlapping = [t for t in transitions
                       if t <= end and (t >= start or t + settle > start)]
        if not overlapping:
            return end
        latest = max(overlapping)
        start = math.ceil((latest + settle) / step) * step
        # With zero settling, an aligned transition still cannot be an endpoint.
        if start <= latest:
            start = (math.floor(latest / step) + 1) * step
        end = start + window


def search(args):
    """Return regional selections with shared config, bounds and query provenance.

    .peak-selection.json freezes config before any requests. peak-search.json
    checkpoints validated raw query responses, source coverage, errors and the
    final selection. Resuming revalidates and reranks snapshots without requests;
    refresh bypasses snapshots/cache but keeps the original search end.
    """
    # cli imports this module when integrating peak selection with collection.
    from .cli import Client, GRAFANAS, GRAFANA_RESOURCE, datasources, mappings, query, write_json

    metric = getattr(args, "peak", None) or "memory"
    window = getattr(args, "window", None)
    if window is None:
        window = 900
    step = getattr(args, "step", 60)
    lookback = getattr(args, "peak_lookback", 604800)
    explicit_end = getattr(args, "peak_end", None)
    if metric not in ("cpu", "memory"):
        raise ValueError("peak resource must be cpu or memory")
    if (not isinstance(step, int) or step <= 0 or not isinstance(window, int) or
            window < step or window % step):
        raise ValueError("window must be a positive multiple of step")
    if not isinstance(lookback, int) or lookback < window:
        raise ValueError("peak-lookback must be at least window")
    if explicit_end is not None and (not isinstance(explicit_end, int) or explicit_end % step):
        raise ValueError("peak-end must align to step")
    grafanas = mappings(GRAFANAS, getattr(args, "grafana", []) or [])
    environments = getattr(args, "environment", None) or sorted(grafanas)
    if "all" in environments:
        environments = sorted(grafanas)
    environments = sorted(set(environments))
    cluster = getattr(args, "cluster", r".*-mgmt-\d+$")
    pattern = re.compile(cluster)
    for env in environments:
        if env not in grafanas:
            raise ValueError(f"No Grafana URL for {env}")
        url = urlsplit(grafanas[env])
        if url.scheme != "https" or not url.hostname or url.username or url.password or url.query or url.fragment:
            raise ValueError("Grafana URLs must be HTTPS endpoints without credentials, query or fragment")
    output = Path(args.output)
    frozen_path = output / ".peak-selection.json"
    raw_path = output / "peak-search.json"
    prior = json.loads(raw_path.read_text()) if raw_path.exists() else None
    if (output / "raw.json").exists() and prior is None:
        raise ValueError("output already contains a non-peak collection; use a new output directory")
    frozen = json.loads(frozen_path.read_text()) if frozen_path.exists() else prior
    if frozen is not None and (frozen.get("schema_version") != 1 or "config" not in frozen):
        raise ValueError("Invalid peak search checkpoint; use a new output directory")
    end = explicit_end if explicit_end is not None else (
        frozen["config"]["search_end"] if frozen else int(time.time() - 300) // step * step)
    # Round the lower bound inward so all evaluations stay inside the lookback.
    start = ((end - lookback + step - 1) // step) * step
    config = {"metric": metric, "window_seconds": window, "step_seconds": step,
              "lookback_seconds": lookback, "search_start": start, "search_end": end,
              "environments": environments, "cluster": cluster,
              "grafana": {env: grafanas[env] for env in environments}}
    for previous in (frozen, prior):
        if previous is not None and previous.get("config") != config:
            raise ValueError("cannot change peak search settings in-place; use a new output directory")
    config_key = hashlib.sha256(json.dumps(config, sort_keys=True).encode()).hexdigest()
    output.mkdir(parents=True, exist_ok=True)
    write_json(frozen_path, {"schema_version": 1, "config": config, "config_key": config_key})
    raw = {"schema_version": 1, "config": config, "config_key": config_key,
           "sources": [], "queries": [], "selection": None,
           "errors": ["Peak search interrupted before completion"],
           "warnings": ["Coverage is telemetry-observed MCs, not an independent fleet inventory.",
                        "Cluster aggregates do not prove individual node scrape completeness."]}
    write_json(raw_path, raw)
    client = Client(getattr(args, "cache_dir", None), getattr(args, "refresh", False), end)
    print(f"Searching {metric} peak over {lookback // 3600}h; averaging {window}s windows at {step}s resolution", flush=True)
    old_sources = {s["environment"]: s for s in (prior or {}).get("sources", [])}
    scope = ("environment", "url", "datasource", "metric", "expression", "start", "end", "step")
    old_queries = {tuple(q[k] for k in scope): q for q in (prior or {}).get("queries", [])
                   if not q.get("error")}
    usage, inventory = {}, {}
    # Inclusive chunk ends are disjoint and aligned even for steps not dividing a day.
    chunk_ticks = max(1, 86400 // step)
    for env in environments:
        base = grafanas[env]
        source = {"environment": env, "url": base, "datasources": [], "coverage": []}
        raw["sources"].append(source)
        try:
            _, discovered = client.cached(
                base + "/api/datasources", GRAFANA_RESOURCE, validate=datasources,
                snapshot=old_sources.get(env, {}).get("discovery"))
            # Discovery may include connection configuration. Retain only routing metadata.
            source["discovery"] = [{"uid": d["uid"], "type": d["type"]} for d in discovered]
            for uid in sorted({d["uid"] for d in discovered if d["type"] == "prometheus"}):
                if not uid.startswith("services-"):
                    continue
                region = uid.removeprefix("services-")
                if len(region) <= 3:
                    continue
                if region.startswith(("int-", "stg-", "prod-")):
                    if not region.startswith(env + "-"):
                        continue
                    region = region.removeprefix(env + "-")
                if len(region) <= 3:
                    continue
                source["datasources"].append(uid)
                selector = "cluster=~" + json.dumps(re.escape(env) + "-.*-mgmt-[0-9]+")
                if metric == "cpu":
                    expression = ('sum by (cluster) (max by (cluster,instance,cpu,mode) '
                                  '(rate(node_cpu_seconds_total{' + selector +
                                  ',job=~"node|node-exporter",mode!="idle",mode!="guest",mode!="guest_nice"}[5m])))')
                else:
                    expression = ('sum by (cluster) (max by (cluster,instance) '
                                  '(node_memory_MemTotal_bytes{' + selector + ',job=~"node|node-exporter"}) - '
                                  'max by (cluster,instance) (node_memory_MemAvailable_bytes{' +
                                  selector + ',job=~"node|node-exporter"}))')
                expressions = (("inventory", "count by (cluster) (max by (cluster,node) (kube_node_info{" + selector + "}))"),
                               (metric, expression))
                coverage = {"datasource": uid, "region": region, "clusters": [],
                            "successful_queries": 0, "failed_queries": 0}
                source["coverage"].append(coverage)
                names = set()
                for chunk_start in range(start, end + 1, chunk_ticks * step):
                    chunk_end = min(end, chunk_start + (chunk_ticks - 1) * step)
                    for kind, expr in expressions:
                        row = {"environment": env, "url": base, "region": region, "datasource": uid,
                               "metric": kind, "expression": expr, "start": chunk_start,
                               "end": chunk_end, "step": step}
                        raw["queries"].append(row)
                        print(f"Peak scan {env}/{uid} {kind}: {chunk_start} .. {chunk_end} (cached when available)", flush=True)
                        old = old_queries.get(tuple(row[k] for k in scope), {})
                        try:
                            row["response"], parsed = query(client, base, uid, expr, chunk_start,
                                                            chunk_end, step, snapshot=old.get("response"))
                            target = inventory if kind == "inventory" else usage
                            for item in parsed:
                                name = item["labels"].get("cluster", "")
                                if not re.fullmatch(re.escape(env) + r"-.*-mgmt-[0-9]+", name) or not pattern.search(name):
                                    continue
                                key = env + "/" + name
                                names.add(key)
                                samples = target.setdefault((row["environment"], row["region"]), {}).setdefault(key, {})
                                for tick, value in item["samples"]:
                                    if value is None:
                                        continue
                                    value = float(value)
                                    if not math.isfinite(value) or value < 0 or (kind == "inventory" and value == 0):
                                        continue
                                    if tick in samples and samples[tick] != value:
                                        raise ValueError(f"Conflicting cluster aggregates for {key} at {tick}")
                                    samples[tick] = value
                            coverage["successful_queries"] += 1
                        except Exception as exc:
                            row["error"] = str(exc)
                            if hasattr(exc, "response"):
                                row["response"] = exc.response
                            coverage["failed_queries"] += 1
                            raw["errors"].append(f"{env}/{uid}/{kind}: {exc}")
                        coverage["clusters"] = sorted(names)
                        write_json(raw_path, raw)
            if not source["datasources"]:
                raise ValueError("No regional services datasources discovered")
        except Exception as exc:
            source["error"] = str(exc)
            raw["errors"].append(f"{env}: {exc}")
        write_json(raw_path, raw)
    raw["errors"].pop(0)
    try:
        if raw["errors"]:
            raise ValueError("Peak search incomplete: every datasource must succeed; see peak-search.json")
        regions = []
        for env, region in sorted(usage.keys() | inventory.keys()):
            region_usage = usage.get((env, region), {})
            region_inventory = inventory.get((env, region), {})
            series = {key: {tick: value for tick, value in region_usage.get(key, {}).items()
                            if tick in region_inventory.get(key, {})}
                      for key in region_usage.keys() | region_inventory.keys()}
            regional = rank_window(series, window, step, start, end)
            regional.update({"environment": env, "region": region,
                             "original_selected_at": regional["selected_at"],
                             "metric": metric, "units": "cores" if metric == "cpu" else "bytes",
                             "search_start": start, "search_end": end})
            regions.append(regional)
        if not regions:
            raise ValueError("No management clusters/data in the requested scope")
        selection = {"scope": "regional", "regions": regions, "metric": metric,
                     "window_seconds": window, "step_seconds": step,
                     "search_start": start, "search_end": end, "config": config,
                     "config_key": config_key, "queries": raw["queries"],
                     "sources": raw["sources"], "warnings": raw["warnings"]}
        # Queries and source metadata live once in the on-disk search bundle.
        raw["selection"] = {k: v for k, v in selection.items() if k not in ("queries", "sources", "warnings")}
    except ValueError as exc:
        if not raw["errors"]:
            raw["errors"].append(str(exc))
        raise
    finally:
        write_json(raw_path, raw)
    return selection
