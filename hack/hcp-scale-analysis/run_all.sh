#!/usr/bin/env bash
# Run the full HCP scale-test analysis pipeline.
#
# Data files (kube-burner ndjson dumps) are read from ARO_HCP_SCALE_DATA
# (default: ~/Downloads). Caches, figures and reports are written to
# ARO_HCP_SCALE_OUT (default: same as the data dir). See datasets.py.
#
# Requires: python3 with numpy + matplotlib.
set -euo pipefail
cd "$(dirname "$0")"

echo "== extracting kube-apiserver replica time series =="
python3 extract_kas.py

echo "== extracting all control-plane pod time series =="
python3 extract_all.py

echo "== plotting steady-state figure =="
python3 plot_kas.py

echo "== plotting whole-run sum figure =="
python3 plot_kas_sum.py

echo "== generating per-workload / replica-savings report =="
python3 report_replicas.py

echo "== done =="
