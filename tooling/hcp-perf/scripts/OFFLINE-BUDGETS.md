# Offline Quiet Budgets

`offline_budgets.py` uses only the Python standard library and saved files. It
has no network, subprocess, Kubernetes, Azure, or profile mutation code. Its
fixed experiment inputs are the final probe-v2 image windows on 2026-09-10:
02:28-02:36 UTC (15 application Pods, not 15 applications) and 03:04-03:10 UTC
(zero application Pods). The containing HCP is reused, not a fresh installation.

Run from the perf-runner worktree. Verify the parent exists and is writable
before creating the private directories with `mkdir -m 700`. The analyzer
requires an existing private output directory and refuses unrelated locations.

```bash
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p test_offline_budgets.py -v
python3 -B tooling/hcp-perf/scripts/offline_budgets.py --output _artifacts/small-constrained-v1/analysis
python3 -B tooling/hcp-perf/scripts/offline_budgets.py --output _artifacts/small-constrained-v1/analysis --verify-output
```

`--refresh` explicitly replaces only the four generated outputs after an analyzer
change. It does not remove other agents' files. Verification rederives and
byte-compares all four outputs, without copying raw evidence or writing results.
Changing the analyzer changes its recorded input hash and requires a refresh.

## Outputs

- `candidate-budgets.json`: versioned exact namespace/kind/workload/group/container
  selectors, workload UIDs, Pod selectors, config baseline, observed actual
  replica counts, per-container budgets, Go target applicability and gates.
- `observations.json`: every one of the 145 running regular/restartable-init
  instances with Pod UID, container runtime identity, image digest, config,
  per-window statistics and freshness/coverage. No large raw series duplication.
- `input-hashes.json`: input paths, SHA256s, manifest verification status and query
  status audit. Every raw file in overlapping chunks is verified, including
  queries not used for budgets. Non-overlapping raw chunks are not read.
- `README.md`: workload/container table, totals, downstream exceptions, coverage
  and qualification blockers.

## Method

Resolve owners by UID through saved ReplicaSets, stopping at the actual workload
rather than the HCP owner. Check full versus normalized Pod configs and templates,
then require the same active UID/runtime/image/config/replica inventory across
four nearest surrounding snapshots. Preserve all templates, including disabled
workloads, completed Jobs and nonrestartable init containers, as unknown quiet
evidence with startup gates. Never match a shortened or ambiguous component name.

CPU request is `max(existing, ceil(max(covered 1m rate peak, distinct source
counter interval peak) * 1.20 / 0.01) * 0.01)` cores. Exclude the first minute of
rate evaluations so trailing rates do not mix pre-window activity. Counter
intervals use source timestamps, not repeated five-second query evaluations;
exclude resets, missing timestamps and intervals above the freshness ceiling.
Short observed intervals are retained, not floored or smoothed into tiny requests.

Memory request is `max(existing, ceil(covered charge peak * 1.10 / 16Mi) * 16Mi)`.
Use the maximum across replicas, not an average, to configure all replicas alike.
Sum that uniform request once for each running regular/restartable-init replica.
This is not scheduler-effective Pod accounting, transient-init maxima, overhead,
billing, or a whole-cluster hard cap.

CPU limit equals request. Memory limit is 110% of request rounded up to one MiB.
GOMEMLIMIT's expected target remains 80% of **request**, independently of the
hard memory limit. Preserve existing Go targets; explicitly identify observed Go
runtimes that lack an env target. Unknown runtimes are not assumed to be Go.
The arithmetic 80% expectation is provided for every known memory request, but
does not assert that every container supports the Go setting.

The default freshness ceiling is 30 seconds, explicitly configurable up to 60.
Fresh source values before a quiet boundary are excluded and separately counted,
not called missing or stale. Per-metric slot counts, source ages and distinct
counter samples are retained. Missing usage stays null. RSS, working set and Go
heap are descriptive here because their own timestamp queries were not saved.
Go-managed memory (`Sys - HeapReleased`) is unavailable and is not replaced by
heap allocation or RSS. An OOM is not a specific Go-runtime failure: cgroup charge
also includes non-Go allocations, subprocesses and charged cache.

Headroom and limits are authorized experiments, not proven requirements. Any
covered CPU/charge observation above a request flags that baseline as
misconfigured for this experiment. No existing request is lowered. Unobserved
containers retain known baselines only; absent baselines stay null. Downstream
templates without the resource-policy annotation require controller-owned
exceptions, including currently unlimited containers. Do not patch generated
Pods as a substitute for persistent configuration.

Fresh constrained startup, init/job execution, population, QPS and one-hour
confirmation gates remain mandatory. No offline report constitutes those tests.
Do not claim every workload fits until all exceptions and coverage gates pass.
