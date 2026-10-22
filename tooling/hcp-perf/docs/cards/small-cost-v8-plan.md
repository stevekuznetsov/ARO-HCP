# Small Cost V8: Storage Memory

**Offline, unapplied, startup-untested treatment.** V7's fresh run progressed to
Ready KAS/OAS/packageserver endpoints and the previously protected components,
then recorded a cluster-storage-operator OOM. V8 changes only that main container
from **31Mi request / 35Mi limit** to **64Mi / 71Mi**. CPU remains **4m declared**;
the entire V7 CPU model, every other memory allocation, probes, images, APF and
all other policy settings stay unchanged. No live calls or application performed.

## Evidence

`_artifacts/small-cost-v7/lifecycle/fresh/first-oom.json` captured the OOM at
**2026-09-11T19:20:02.219459Z**, snapshot `startup-r2/152`. The storage main
terminated **19:19:57Z**, reason `OOMKilled`, exit 137, under 31Mi/35Mi with
GOMEMLIMIT **26004684 bytes**. It had started at 19:12:56Z. Its workload selector
is exactly `cluster-storage-operator/cluster-storage-operator`, not CSI operands.

Historical final-Small evidence in
`_artifacts/small-constrained-v1/analysis/candidate-budgets.json` shows storage
**64Mi request / 192Mi limit**, charge peak/p95 **46.203125Mi**, RSS peak/p95
**44.609375Mi**, working-set peak/p95 about **46.168Mi**. Its 35Mi v7 limit was
below all these measurements. A 71Mi v8 limit is a targeted hypothesis, not a
guaranteed startup minimum or a reason to classify all restarts as OOMs.

The memory policy remains 110% of request rounded up to MiB; storage's Go target
becomes **53687091 bytes**, still 80% of request. Existing non-Go exceptions are
unchanged. Historical profiles, wire objects, allocation reports and OOM evidence
are hash-preserved.

## Why No Cuts

The saved storage Pod already has an `availability-prober` init at **64Mi/71Mi**.
It is sequential, not a restartable sidecar. Its charged Pod maximum was therefore
`max(35Mi regular, 71Mi init) = 71Mi`; V8 is `max(71Mi regular, 71Mi init) = 71Mi`.
Raising the main limit adds **36Mi to the steady running-limit sum**, but **zero
to the already-budgeted per-Pod init maximum**. Charging another 36Mi on top would
double-count sequential stages. The reusable allocator checks this first and
would proportionally rebalance unprotected RAM only if fixed changes exceeded
the envelope. Here `rebalancedOthers=false`.

| Offering Accounting | V7 | V8 |
| --- | ---: | ---: |
| Declared CPU cores | 2.225 | **2.225** |
| Modeled effective CPU cores | 2.480 | **2.480** |
| Running RAM requests MiB | 9,126 | **9,159** |
| Steady running limits + fixed downstream requests MiB | 9,906 | **9,942** |
| Additional init-stage charge MiB | 257 | **221** |
| Total startup-aware charged RAM MiB | 10,163 | **10,163** |
| Compute-equivalent USD/HCP-hour | 0.124990509033203125 | **0.124990509033203125** |

Raw D16s_v6 capacity and retained $0.806/hour price are unchanged. Fixed downstream
accounting stays 470m/2090Mi, instrumentation excluded. Remaining margins are
**560/403m CPU (~1.389578m)** and **311/403Mi RAM (~0.771712Mi)**. This is a tight
raw resource-equivalent budget, not actual packed bills, an all-HCP hard cap or
full bootstrap/rollout-surge qualification. All 52 saved Pod-group maxima remain
identical to V7. All 91 nontargeted controlled rows remain exactly unchanged.

## Anticipatory Risk Audit

The allocation report audits **all 52 running controlled regular workload/container
rows**, including V7 requests and limits, V8 requests and limits, and saved pooled
RSS, working-set and charge p95/max. Init observations are not invented. Main
container classification uses an explicit documented name heuristic; the complete
regular-container audit remains available so the heuristic cannot hide rows.

Risks are ordered by the largest per-container `max(RSS p95, working-set p95)`
minus the current hard limit. A positive max-only deficit also appears in the
full list. Values are MiB; maxima/p95 across different metrics are not synchronized.

| Main Container | Current Limit | Quiet RSS p95 / Max | Quiet Working-Set p95 / Max | p95 Deficit |
| --- | ---: | --- | --- | ---: |
| Kube-apiserver | 563 | 798.20 / 807.12 | 803.46 / 812.39 | **240.46** |
| OpenShift-apiserver | 140 | 198.41 / 199.23 | 200.15 / 201.00 | **60.15** |
| Kube-controller-manager | 105 | 142.88 / 142.92 | 144.07 / 144.10 | **39.07** |
| OpenShift-controller-manager | 70 | 104.40 / 104.41 | 106.79 / 106.80 | **36.79** |
| Ingress-operator | 43 | 67.36 / 68.23 | 68.84 / 69.64 | **25.84** |

These are **historical warning signals, not proven required floors or predictions
of failure order**. They come from the earlier reused benchmark, not the current
fresh V7 run. RSS/working-set lack their own saved source timestamp queries and
are descriptive rather than freshness-certified. Current success at a lower
limit is not disproved by historical usage, and warm cache/population history
can differ. Charge remains separately reported rather than substituted for RSS
or Go-managed memory. No other operator, including these five, was automatically
increased. Catalog cache-heavy charge likewise does not become a Go heap floor.

## Ready Artifacts

| Artifact | Bytes | SHA256 |
| --- | ---: | --- |
| `tooling/hcp-perf/examples/small-cost-v8.json` | 17,975 | `af9adccc048f154283ce4998dfb582f72b3623704e9af188671bc2628267d7ce` |
| `_artifacts/small-cost-v8/small-cost-v8-hypershift.json` | 11,744 | `758d02ae6b04a19c5fbc63979d0a24e403cfb1337dfb8d004916c9332b4661ae` |
| `_artifacts/small-cost-v8/allocation.json` | 142,762 | `82141e762e2e8dd33696a4b533555aa83d94b5d283081ffaa8812e3da76eb820` |

Private outputs also include resolved resources/runtime env and `output-hashes.json`.
The report's `quietMemoryAudit` and ordered `mainMemoryRisks` provide the full
anticipatory list and source hashes. `scripts/cost_allocate_v8.py` reuses the
existing fixed-memory search, Pod-stage accounting and renderer. Older allocator
sources and generated artifacts were not modified for V8.

**23 allocator tests and both sizing CLI commands pass.** Tests assert the exact
storage-only difference across all 92 rows, unchanged CPU and Pod-group maxima,
protected memory, no unnecessary redistribution and cost within $0.125. The profile
and four private artifacts reproduce byte-for-byte. Read-only commands from root:

```bash
python3 -B tooling/hcp-perf/scripts/cost_allocate_v8.py
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_cost_allocate*.py' -v
```

Offline render from `tooling/hcp-perf`:

```bash
GOWORK=off go run . sizing hypershift --profile examples/small-cost-v8.json \
  --default-cpu-milli 2 --default-memory-mib 31
```

**Fallback remains 2m/31Mi**, already present in the retained wire. The explicit
storage override and all 17 prober overrides take precedence. Ready means the
artifact is validated for the parent's separately authorized treatment, not that
this task applied it or qualified startup. No live/CEL admission test was run.
