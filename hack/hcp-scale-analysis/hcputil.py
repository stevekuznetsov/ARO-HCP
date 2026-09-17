"""Shared helpers for reading kube-burner dumps."""
import gzip, json

def opener(path):
    return gzip.open(path, "rt") if path.endswith(".gz") else open(path, "rt")

def cluster_name(path):
    """Return jobSummary.clusterName (the target hosted cluster) or None.

    Some dumps (e.g. staggered conformance runs) contain many hosted clusters in
    one file; the control-plane namespace of interest is the one whose name
    contains this clusterName."""
    with opener(path) as f:
        for line in f:
            if '"jobSummary"' not in line or 'clusterName' not in line:
                continue
            try:
                r = json.loads(line)
            except ValueError:
                continue
            if r.get("metricName") == "jobSummary":
                return r.get("clusterName")
    return None

def drop_transient(pod_series, frac=0.5):
    """Drop pods whose sample count is < frac * the max across pods (removes
    rolled-over / transient replicas so a 3-replica workload stays 3 replicas).
    pod_series: {pod: [[epoch,val],...]}. Returns a filtered copy."""
    if not pod_series:
        return pod_series
    maxlen = max(len(v) for v in pod_series.values())
    thr = frac * maxlen
    return {p: v for p, v in pod_series.items() if len(v) >= thr}
