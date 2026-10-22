# Small Constrained Plan: Not Applied

**Cost-first course correction:** the current experiment is now
[Small Cost V1](small-cost-plan.md), allocated within $0.125/HCP-hour followed by
$0.25, without original-request floors. This document and its scoped candidate
remain unchanged as historical derivation and ratio evidence, not the current
minimum budget. Neither candidate has been deployed or startup-qualified.

## Selected Scoped Candidate

**The user-approved enforcement scope is direct HyperShift projector-controlled
containers only. Downstream operator-created Pods and instrumentation are allowed
outside enforcement, remain measured separately, and keep their existing requests,
limits and runtime settings.** This replaces the whole-HCP enforcement prerequisite
for this scoped experiment, not for a future whole-HCP constrained claim.

`examples/small-constrained-v1.json` is the new, versioned offline candidate.
It does **not** replace or rewrite the frozen 9.920-core peak candidate in
`_artifacts/small-constrained-v1/analysis/`. New derived artifacts live in the
separate `_artifacts/small-constrained-v1/scoped-analysis/` directory.

### Original Versus New

All figures use the same running regular/restartable-init instance inventory as
the frozen report. CPU is cores; RAM is requested MiB, not observed consumption.
"All HCP" includes KSM for continuity with the original total; product-only totals
exclude that explicitly separate instrumentation row.

| Scope | Running Instances | Original CPU | New CPU | Original RAM | New RAM |
| --- | ---: | ---: | ---: | ---: | ---: |
| All HCP, including KSM | 145 | 3.870 | **6.500** | 14,506 | **16,170** |
| Direct projector-controlled | 97 | 3.390 | **6.020** | 12,352 | **14,016** |
| Downstream operator-created | 47 | 0.470 | **0.470** | 2,090 | **2,090** |
| Instrumentation: KSM | 1 | 0.010 | **0.010** | 64 | **64** |
| Product HCP, excluding KSM | 144 | 3.860 | **6.490** | 14,442 | **16,106** |

The all-HCP request increase is **2.630 cores (+68.0%) and 1,664 MiB (+11.5%)**.
Within controlled scope it is +77.6% CPU / +13.5% RAM. Compared with the frozen
candidate, the scoped candidate reserves 3.420 fewer CPU cores and 90 fewer MiB.
The memory difference is solely from not correcting downstream requests.

Controlled CPU limits total 6.020 cores; controlled memory limits total
**15,468 MiB**, versus the original 37,056 MiB of 3x limits. KSM retains its 256Mi
memory limit and no CPU limit. All 47 downstream running containers retain absent
CPU and memory limits. Thus the sum of explicit namespace memory limits is
15,724 MiB, **not a whole-HCP hard cap**; 48 instances still have no CPU limit and
47 have no memory limit. No scheduler-effective init maxima, Pod overhead,
management services, or guest workloads are included in these request totals.

### Derivation And Scope

For each controlled workload/container, CPU request is
`max(original, ceil(max(replica/window p95(covered 1m CPU rate)) * 1.2 / .01) * .01)`
cores. This is a sustained-p95 experiment, not a promise to accommodate every
short CPU burst. Memory request remains
`max(original, ceil(worst covered cgroup charge peak * 1.1 / 16Mi) * 16Mi)`.
RAM is **peak-based, not a sustained-p95 memory estimate**. Floors are original
Small requests, not frozen peak-candidate requests. Unknown observations do not
become zero, and original floors are not rounded upward merely for alignment.

CPU limit equals request; memory limit is `ceil(request MiB * 1.10)` MiB.
GOMEMLIMIT is floor(80% of request bytes), preserving the two existing non-Go
exceptions: `ignition-server-proxy/haproxy` and `router/router`. Unknown helper
runtimes are not newly classified as Go; the existing default injection remains.
GOMAXPROCS is not inferred or overridden. Router's Swift NIC requests/limits and
all unrelated resources remain unchanged.

The 133 derived rows comprise **92 controlled rows** (53 quietly observed, 39
unobserved), **40 downstream rows** (36 observed, four unobserved), and one KSM
row. The profile needs only **25 explicit overrides**; the other 67 controlled
rows resolve exactly through the existing **10m/64Mi fallback**. All 39
unobserved controlled rows retain that known fallback, now with 10m CPU / 71Mi
memory limits. **Startup is untested. No new startup budget was invented.** The
four absent downstream init requests remain absent, rather than receiving fallback.

The old `policyManaged` field is only a saved annotation heuristic. The new
`annotationHeuristicPolicyManaged` records that separately from source-supported
`projectorIdentity` and `projectorSource`. All 92 controlled rows were mapped to
explicit local component registrations plus the common pre-submission projector
path. The observed Job `olm-collect-profiles-29816042` maps through its saved
UID-matched Job and explicit CronJob owner reference to `olm-collect-profiles`,
not by stripping a guessed suffix. Its unchanged budget uses fallback.
**Source-supported scope and historical annotation agreement are not actual
constrained Pod coverage.** That field remains false on every new analysis row.

### Schema Handoff

The profile uses the existing sizing CLI `v1` schema, 1m/1Mi resolver quanta,
KAS unit ratios with a 580m/1024Mi anchor, and zero ratios plus static floors for
other overrides. Analysis already performs 10m/16Mi request rounding; using those
as resolver quanta would incorrectly change retained floors or hard-limit rounding.
The renderer emits `containerResourcePolicy` with `defaultRequests: 10m/64Mi`,
`cpuLimitEqualsRequest: true`, `memoryLimitPercent: 110`, and
`goMemoryLimitPercent: 80`. Overrides use only supported exact workload/container
keys; no namespace, kind, init, or downstream-enforcement extension is invented.
No duplicate reduced keys were found. This is the artifact contract for the
projector owner; no projector source was edited by this analysis task.

Local `sizing resolve` and `sizing hypershift` succeed. A precise comparison
checks every controlled row against explicit overrides or effective fallback,
including requests, CPU/memory limits, Go target, non-Go exceptions, and retained
extended resources; all excluded resources/env are unchanged. The rendered object
passes the local CRD's OpenAPI schema validator. **Kubernetes CEL admission and
live admission were not executed.** These checks do not qualify operator images.

### Ready Artifacts

Paths are relative to the `ARO-HCP-perf-runner` root. Ready means offline review
and validation only, **not ready to apply**.

| Path | Bytes | SHA256 |
| --- | ---: | --- |
| `tooling/hcp-perf/examples/small-constrained-v1.json` | 7,858 | `79e50e13247a4456c16ae15eddc00f7565593cc49d0286e89974d5e6d64e656e` |
| `_artifacts/small-constrained-v1/scoped-analysis/resolved-profile.json` | 9,794 | `39e4bc510744567a5be46a715b87c30e0716e821af966757a99d3b00095bce77` |
| `_artifacts/small-constrained-v1/scoped-analysis/cluster-sizing.json` | 7,228 | `e97d4349d1b903034cef92f1411a5340f4d0f92ed65b119467ae64b040efb072` |
| `_artifacts/small-constrained-v1/scoped-analysis/scoped-candidate.json` | 379,371 | `ecf852a25da2ee2d9baab9ca0bbf01012407d78267c1f047ca907c75345e339a` |

`scoped-candidate.json` records input/source hashes, all rows, unchanged measurement
coverage, and the split totals. `output-hashes.json` records derived byte hashes.
All **883 frozen input hashes**, including 592 manifest-verified raw files, are
rechecked on each reproduction. The three frozen output hashes are pinned in the
new analyzer. Relevant local projector, API, CRD, mapping, and resolver sources
are hashed separately, so dirty-worktree source identity does not rely on HEAD.

Read-only reproduction, with the local projector worktree path in `HYPERSHIFT_ROOT`:

```bash
python3 -B tooling/hcp-perf/scripts/scoped_offline_budgets.py \
  --hypershift-root "$HYPERSHIFT_ROOT"
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_*offline_budgets.py' -v
```

The reproduction byte-compares all four new outputs, checks the checked-in
profile against the derivation, runs both offline CLI commands with dependency
network access disabled, and validates against local CRD OpenAPI using installed
PyYAML/jsonschema. It never rewrites existing artifacts. Eleven Python tests and
the Go sizing package tests pass. No live calls, image builds, or application
were performed. A prior session ID was not supplied; this pass reconstructed
context from the saved analysis rather than claiming a resumed session.

### Remaining Gates

Hold application until explicit authorization and pinned HO/CPO capability
verification. A future fresh run must verify actual controlled regular/init/Job
requests, limits, and runtime settings from first creation, then test startup,
population, QPS, and one-hour confirmation with OOM/restart, readiness, throttling,
latency and error gates. Continue measuring downstream/KSM separately, including
their original-request exceedances, without treating their lack of constrained
limits as a blocker to this **scoped** experiment. Do not claim whole-HCP limits.

## Historical Frozen Review

**The remainder documents the previous whole-HCP peak-based proposal and its
then-unresolved scope decision. Its 9.920-core numbers and whole-HCP enforcement
blockers are historical, not the selected scoped candidate above.**

**Offline proposal, not a qualified baseline. No profile was applied, no live
calls made, and the frozen 9.92-core candidate was not reduced by this review.**
CPU-limit support is enabled in the experimental resolver/projector **source**;
CPU=request and 110%-of-request memory limits (rounded up to MiB) are selected
experimental targets, not verified deployed-image capability or successful
constrained startup. See [projector status and enforcement gaps](../hypershift-sizing.md#constrained-limits).

## What The Totals Mean

Evidence: final-image 2026-09-10 02:28-02:36 UTC with **15 application Pods**
(three applications), and 03:04-03:10 UTC with zero application Pods. This is a
reused HCP after earlier activity, not pristine empty-cluster startup. All totals
count the same 145 running regular/restartable-init instances, including KSM.
Uniform requests take the maximum across replicas, then multiply by actual
replicas. They are not simultaneous consumption or scheduler-effective init maxima.

| Budget | CPU Cores | Memory Requests MiB |
| --- | ---: | ---: |
| Original Small | 3.870 | 14,506 |
| Strict observed correction, no headroom, 1m CPU / 1Mi memory rounding | 8.253 | 15,777 |
| Strict observed correction at candidate's 10m / 16Mi rounding | 8.490 | 15,940 |
| Frozen peak-plus-headroom candidate | **9.920** | **16,260** |
| Historical Medium, different experiment | 7.260 | 26,858 |

Strict correction retains existing requests as floors and raises them to the
covered peak: CPU uses the larger of the one-minute rate and distinct-source
counter interval peaks; RAM uses cgroup charge. The analysis authors chose the
**20% CPU / 10% charge headroom**, not the user; these are discretionary
experiments, not measured requirements. The same-quantum comparison attributes
1.430 cores and 320 MiB to headroom plus its rounding interactions. Finer strict
rounding removes another 0.237 cores / 163 MiB. Total candidate increase over
Small is 6.050 cores (+156.3%) and 1,754 MiB (+12.1%). Memory limits total
17,947 MiB. GOMEMLIMIT remains 80% of **request**, not of the hard limit.

Diagnostic sustained estimates, **not replacement candidates or safe limits**:
take the maximum per-replica/window one-minute median or p95, keep existing
requests as minimums, round increases up to 10m, then multiply by replicas.
Median-based requests total **4.135 cores**; p95-based requests **5.790 cores**.
Using one-minute maxima alone gives **6.020 cores**. These omit short bursts and
cannot satisfy an every-observed-interval-within-request criterion or prove that
startup works with CPU limits. Medium had no CPU limits and a different sizing
rule/environment; 9.92 > 7.26 is not evidence that empty Small needs more sustained
CPU than loaded Medium. See [Medium progress](medium-v1-progress.md).

## Largest CPU Drivers

All per-container CPU figures below are millicores. Median/p95/max pool covered
one-minute replica observations; this can hide an uneven replica, so the
sustained alternatives above instead use worst per-replica/window statistics.
Delta is replica-weighted cores. Counter maxima use distinct source intervals,
not repeated five-second query evaluations. Top two account for **53.3%** of the
increase; top six account for **75.6%**.

| Workload / Container | Replicas | Old -> Candidate m | 1m Median / p95 / Max m | Interval Max m | Delta Cores |
| --- | ---: | ---: | ---: | ---: | ---: |
| kube-apiserver / kube-apiserver | 3 | 250 -> 830 | 166 / 264 / 503 | 692 | +1.740 |
| packageserver / packageserver | 3 | 125 -> 620 | 5.4 / 148 / 199 | 515 | +1.485 |
| cluster-network-operator / cluster-network-operator | 1 | 10 -> 410 | 58 / 214 / 272 | 337 | +0.400 |
| cluster-version-operator / cluster-version-operator | 1 | 10 -> 350 | 3.2 / 127 / 154 | 286 | +0.340 |
| redhat-operators-catalog / registry | 1 | 50 -> 360 | 10 / 106 / 152 | 298 | +0.310 |
| openshift-apiserver / openshift-apiserver | 3 | 40 -> 140 | 22 / 32 / 85 | 115 | +0.300 |
| ingress-operator / ingress-operator | 1 | 10 -> 230 | 135 / 156 / 164 | 184 | +0.220 |
| control-plane-operator / control-plane-operator | 1 | 550 -> 750 | 227 / 343 / 383 | 620 | +0.200 |
| etcd / etcd | 3 | 100 -> 150 | 58 / 82 / 99 | 120 | +0.150 |
| community-operators-catalog / registry | 1 | 50 -> 200 | 12 / 67 / 79 | 160 | +0.150 |
| certified-operators-catalog / registry | 1 | 50 -> 180 | 13 / 61 / 107 | 143 | +0.130 |
| hosted-cluster-config-operator / hosted-cluster-config-operator | 1 | 25 -> 140 | 43 / 62 / 69 | 111 | +0.115 |

Fully covered, simultaneous whole-HCP one-minute CPU median / p95 / max is
**1.611 / 1.791 / 1.912 cores** in the 15-Pod window (84 slots), and
**1.757 / 2.250 / 2.306 cores** in the zero-Pod window (60 slots). The first
minute is excluded to prevent trailing rates mixing pre-window activity.

The zero-Pod window drives major outliers. Packageserver goes from a worst-replica
p95 of 6.4m with 15 Pods to 190.8m with zero Pods, while its median stays near
5.7m. Its 515m peak spans 11.135 seconds starting 03:08:02 UTC. All three catalog
peaks begin around 03:07:51-03:07:59. This temporal alignment is consistent with
shared periodic/catalog activity, but does not identify the trigger or prove a
bug. KAS's worst-replica zero-Pod p95 is 480.6m, versus 268.3m with 15 Pods;
its 692m peak spans 19.995 seconds starting 03:06:09.

CPO is **not the dominant increase and has no one-minute exceedance of its old
550m request**. Its 620m peak spans 13.644 seconds starting 03:05:24. One-minute
median is 243m with 15 Pods and 194m with zero Pods, with repeated activity rather
than monotonic runaway growth. CNO and ingress operator exceed their 10m requests
in every covered one-minute observation; HCCO exceeds 25m in 95.1%. These are
credible baseline-floor or background-work investigations, not just rare spikes.
The saved query set has no reconcile count/duration, workqueue metric, or CPU
profile. The probe recorder summary ends at 02:26:11, before these windows.
**Residual reconciliation or a controller bug remains possible, not established
or ruled out.** Do not relabel all measured background work as necessary capacity.

## Largest RAM Drivers

Per-container MiB; deltas include replicas. Etcd plus three registries account
for **1,504 / 1,754 MiB (85.7%)** of the increase.

| Workload / Container | Replicas | Old -> Candidate | Charge Median / p95 / Peak | RSS / Working-Set Peaks | Delta MiB |
| --- | ---: | ---: | ---: | ---: | ---: |
| etcd / etcd | 3 | 256 -> 544 | 445 / 480 / 482 | 161 / 214 | +864 |
| redhat-operators-catalog / registry | 1 | 256 -> 608 | 486 / 552 / 552 | 96 / 98 | +352 |
| certified-operators-catalog / registry | 1 | 256 -> 416 | 200 / 367 / 374 | 198 / 200 | +160 |
| community-operators-catalog / registry | 1 | 256 -> 384 | 180 / 291 / 341 | 202 / 204 | +128 |
| ignition-server-proxy / haproxy | 2 | 64 -> 96 | 72.9 / 72.9 / 72.9 | 67.5 / 72.9 | +64 |
| ignition-server / ignition-server | 2 | 128 -> 160 | 98.6 / 143.4 / 143.4 | 54.2 / 106.6 | +64 |
| ovnkube-control-plane / socks-proxy | 2 | 10 -> 32 | 21.5 / 21.9 / 21.9 | 20.8 / 21.9 | +44 |

Large charge-versus-RSS/working-set differences are consistent with substantial
cache/other charged memory, not proof of a Go heap requirement or leak. These are
separate maxima, not a synchronized cache decomposition. RSS/working-set are
descriptive without their own saved timestamp queries. Go-managed memory is
unobserved, not substituted with Go heap or RSS. Reused caches/population history
may matter. An OOM under 110% limits would not specifically diagnose Go failure.

## Enforcement And Unknowns

There are 133 workload/container rows: 90 represent the 145 running instances;
**43 rows have no quiet running observation**, including transient init,
completed jobs and disabled workloads. Their known requests are retained only,
not validated. Four init rows have absent requests and remain **null**:
`cloud-network-config-controller/hosted-cluster-kubecfg-setup`,
`multus-admission-controller/hosted-cluster-kubecfg-setup`,
`network-node-identity/hosted-cluster-kubecfg-setup`, and
`ovnkube-control-plane/init-ip`.

Existing snapshots have **0/145 CPU limits and 98/145 explicit memory limits**.
The analysis's **41 unprojected selector rows** span ten downstream/instrumentation
workloads, including 48 running instances (47 downstream plus KSM). These counts
are different units, not contradictory coverage claims. The projector agent's
documented scope confirms that merely listing an override does not project CNO,
CSI or KSM Pods. Complete pre-creation owning-operator enforcement or a separately
delivered fail-closed admission path is still missing. No patch-after-startup,
LimitRange default or resource guard closes that gap.

## Frozen Evidence And Decision

Private outputs: `_artifacts/small-constrained-v1/analysis/`. All **883 input
hashes reverified**, including **592 raw files** checked against retained chunk
manifests. No two-hour archive was pulled. All 145 CPU/charge series are covered;
512 pre-window memory source slots are explicitly excluded, not zero-filled.
There are 8,039 distinct counter samples, zero resets and zero long gaps.

| Frozen File | SHA256 |
| --- | --- |
| candidate-budgets.json | `c0617bddb79fa78d2c652895dc28ffa75dc041237523f3990670dfad13b375a8` |
| observations.json | `abea0bf189f9b941e68601b2d16d2f2726e26d19aed03b754ba6c6b119d64558` |
| input-hashes.json | `a8b7447bcedfc6049df57fc976e5bf04af444b817925cd4510699a71c952164e` |

Read-only reproduction from the perf-runner root:

```bash
python3 -B tooling/hcp-perf/scripts/diagnose_offline_budgets.py
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_*offline_budgets.py' -v
```

Nine deterministic tests pass. The diagnostic rechecks hashes and writes only
stdout; it never rewrites the candidate. **Hold application.** The fundamental
decision is whether to reserve replicated short-interval peaks, or first diagnose
and reduce avoidable background work before rederiving budgets. Sustained-only
estimates are not an implicit decision to accept burst throttling. After that
decision and complete enforcement, fresh constrained startup/init/job, population,
QPS and one-hour confirmation gates remain mandatory. There is no qualified
constrained baseline yet.
