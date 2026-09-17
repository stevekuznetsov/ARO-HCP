"""Shared configuration for the HCP scale-test analysis tooling.

Data files (kube-burner ndjson dumps, plain or gzipped) live in DATA_DIR.
Override with the ARO_HCP_SCALE_DATA env var if they live elsewhere.
All generated caches / figures / reports are written to OUT_DIR (defaults to DATA_DIR).
"""
import os

DATA_DIR = os.environ.get("ARO_HCP_SCALE_DATA", os.path.expanduser("~/Downloads"))
OUT_DIR = os.environ.get("ARO_HCP_SCALE_OUT", DATA_DIR)

# (label, filename) — label is the worker-node count, ordered ascending by scale.
DATASETS = [
    ("3",   "3-node-aro-hcp.ndjson.gz"),
    ("6",   "6-node-aro-hcp.ndjson.gz"),
    ("12",  "12-nodes-20-client-qps-rate-aro-hcp.ndjson.gz"),
    ("30",  "30-node-aro-hcp.ndjson.gz"),
    ("49",  "49-node-hcp-raw.ndjson"),
    ("60",  "60-node-aro-hcp.ndjson.gz"),
    ("120", "120-node-aro-hcp.ndjson.gz"),
    ("250", "250-node-aro-hcp.ndjson.gz"),
    ("500", "500-node-aro-hcp.ndjson.gz"),
]

# Plot/report ordering (labels), ascending by node count.
ORDER = [lbl for lbl, _ in DATASETS]

# Cache / output file locations.
KAS_CACHE = os.path.join(OUT_DIR, "kube-apiserver-timeseries.json")
POD_CACHE = os.path.join(OUT_DIR, "controlplane-pod-timeseries.json")
FIG_STEADY = os.path.join(OUT_DIR, "kube-apiserver-steadystate.png")
FIG_SUM = os.path.join(OUT_DIR, "kube-apiserver-sum-wholerun.png")
REPORT_CSV = os.path.join(OUT_DIR, "hcp-replica-savings.csv")
REPORT_MD = os.path.join(OUT_DIR, "hcp-replica-savings-report.md")


def data_path(fn):
    return os.path.join(DATA_DIR, fn)
