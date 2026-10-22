# Upgrade Signal Evidence

[The query bundle](../examples/upgrade-signal-queries.json) is a maintained input
to `metrics stream`, `metrics export`, and `metrics backfill`. It collects evidence
for an offline sizing diagnosis, not production alert rules, supported capacity,
or automatic upgrades. It neither creates collectors nor changes lifecycle,
resources, APF, storage, or workload. There is no hard customer latency SLO in this
experiment; no percentage, latency, or persistence threshold here is validated.

## Scope And Contract

The JSON is the existing array of `{name, expr}` objects, without new CLI fields.
Names and expressions are recorded in each export manifest. Metric selections
come from the retained Small v1 metric inventory and raw exports: APF, etcd and
most runtime families were exported; container pressure/enforcement and effective
Go-target names were inventoried but not exported by that campaign. Presence in
that inventory is not evidence that every endpoint, release or cgroup supports
them. See the [Small v1 card](cards/small-v1.md) and its
[resource attribution method](cards/small-v1-resources.md).

- No namespace, node, replica count, resource budget, or size name is hardcoded.
  Container selectors cover **all scraped named containers**, excluding the empty
  and `POD` pseudo-containers. They do not select an HCP automatically.
- Join to discovered HCP Pod UID, container incarnation, node and owner inventory
  before assessment. Include regular and restartable-init containers deliberately;
  treat transient init maxima and Pod overhead separately for scheduling. Identify
  instrumentation rather than silently including or excluding it.
- Component metrics retain replica, endpoint and all source labels. Verify the
  process-to-container mapping: the small etcd metrics proxy exposes the main
  etcd process despite its `container="etcd-metrics"` label. A name-only join is
  unsafe. Join volume statistics to the actual etcd PVCs, not every PVC on a node.
- These queries describe the connected Prometheus scope. Management-node metrics
  are not guest-worker metrics. A multi-HCP/fleet Prometheus needs an explicitly
  scoped, reviewed run copy or narrower scrape scope; this file is not bounded
  by tenant count. Do not assume Prometheus external labels appear in local queries.
- Value queries use exact names or anchored Prometheus alternations of exact
  names, with no wildcard family export and no overlapping value selections.
  Raw counters/gauges are preserved; rates and ratios belong in offline analysis.
- Every selected metric has its own timestamp query. `timestamp()` drops the
  metric name, so the query expression/name supplies that identity; never apply
  it to a multi-name selector whose remaining labels could collide.
- The four `hist-*` value queries retain classic cumulative buckets, including
  `+Inf`. Separate `_count` and `_sum` exports are deliberately omitted: `+Inf`
  supplies observation counts, buckets support quantiles, but these exports
  cannot recover exact mean durations. Each histogram timestamp query selects
  only `le="+Inf"`, avoiding a second full bucket matrix. It is a family freshness
  indicator, **not proof that every bucket is present or equally fresh**. Validate
  bucket coverage/schema before deriving quantiles; do not merge changed schemas.
- API totals/durations select GET, LIST, CREATE, UPDATE, PATCH, DELETE and
  DELETECOLLECTION, retaining resource/subresource dimensions. WATCH, CONNECT
  and other verbs are outside this bounded API view. APF counters still cover
  their exposed flows, so their denominator is not this filtered API total.

## Bounded Streaming

For an already authorized and prepared capture, from `tooling/hcp-perf`:

```bash
hcp-perf metrics stream \
  --endpoint "$PROMETHEUS_ENDPOINT" \
  --queries examples/upgrade-signal-queries.json \
  --output "$NEW_PRIVATE_OUTPUT" \
  --duration 10m --chunk 60s --step 5s --lag 15s \
  --chunk-timeout 90s --drain 90s
```

This is a future-use example, not authorization to query or start a campaign.
The endpoint/credentials, existing output parent, new private child directory,
scrape readiness and worker supervision are separate responsibilities. Duration
is an explicit observation window, not a workload duration or validated soak.

Use 15-60 second chunks aligned to the evaluation step. A 60-second chunk at
5-second steps has 13 evaluation slots per returned series, not 13 guaranteed
fresh scrapes. Selection is bounded by metric names, **not a guaranteed series,
byte or execution-time cap**. API resource histograms are the largest likely
source; do not add the historical broad APF/runtime regex queries alongside this
bundle and duplicate their cardinality.

Check query sizes, completed-chunk lag and timeout/error manifests in a short
observer validation before load. Queries run sequentially, and many per-family
timestamp requests also cost time. Smaller chunks reduce response size but can
increase request overhead; they do not fix an oversized target scope. If needed,
derive run-local arrays from this same file using
`map(select(.name | startswith("hist-") | not))` and
`map(select(.name | startswith("hist-")))` with `jq`, then supervise the scalar and
histogram streams independently. Both members of every histogram pair stay
together. Retain their exact queries, hashes and windows; do not create a second
maintained catalog. Split further only from measured backend constraints.

Adjacent chunks share a boundary; deduplicate by metric/query identity, complete
label set and timestamp. Do not merge nearly coincident grids from independent
streams as though they were additional scrapes. Reconstruct reset-aware rates
across chunks using source timestamps and process/container identity. Never bridge
unknown intervals or fill absent samples with zero. Calculate simultaneous HCP
sums before statistics, not sums of independent per-container percentiles.

## Absence And Capture

`up`, scrape duration and source timestamps describe collection, not customer
availability. A successful HTTP query or `status=complete` stream only establishes
that processing finished; inspect `dataQuality` and per-query manifests. An
otherwise nonempty grouped query does not prove every named family was returned.

PSI, CFS quota/throttling, Go extended runtime metrics, OOM counters, debug
compaction metrics and volume statistics can be unavailable or inapplicable on
particular endpoints/releases. Some event series only exist after an event.
Record an expected-family/endpoint inventory and an explicit absence reason.
The existing exporter has **no per-query optional flag**: empty matrices are
recorded as errors, and grouped responses require per-family inspection. Preserve
those outcomes and classify them in the assessment; do not call an unsupported
optional metric a customer outage, rewrite its error as success, or drop a
required signal just to make capture pass. Optional absence still makes any
diagnosis requiring that signal inconclusive.

Distinguish documented unsupported/inactive sources from timeout, authentication,
target loss, stale lookback values, unexpected missing replicas, and failed
queries. Even `timestamp(up)` being fresh with `up=0` means a fresh failed scrape.
Freshness and acceptable gaps must be explicit observation policy, not inferred
from a full evaluation grid. The small APF/Go exports had material gaps; an intact
file hash or a complete cgroup grid did not repair them.

## Go Memory And Heap Profiles

The bundle exports runtime metrics, not pprof profiles. In particular,
`go_memstats_heap_inuse_bytes` is **not** pprof's `inuse_space`. Keep these
measurements separate:

| Measurement | Meaning |
| --- | --- |
| `go_gc_heap_live_bytes` | Bytes marked live by the preceding GC; an inexpensive retained-object signal, without allocation stacks |
| `go_memstats_heap_alloc_bytes` | Allocated heap objects, including garbage not yet collected |
| `go_memstats_heap_inuse_bytes` | Heap spans in use, including size-class slack; not just live objects |
| `go_memstats_heap_sys_bytes` | Memory obtained for the heap, including idle and released space |
| `go_memstats_sys_bytes - go_memstats_heap_released_bytes` | Runtime-managed memory relevant to the soft `GOMEMLIMIT` target, including non-heap overhead |
| `process_resident_memory_bytes` | Process RSS, including resident mappings; not container RSS or total cgroup charge |
| pprof `inuse_space` | Sampled unfreed-object bytes attributed to allocation stacks; requires a heap profile and depends on GC accounting |

For each process with a configured Go target, preserve its effective
`go_gc_gomemlimit_bytes`, heap live/goal/alloc/inuse/sys values, allocation counter,
GC cycles, GC/assist CPU, pause sum/count, limiter-cycle index, GOMAXPROCS and
process start time. Pair these with the container's actual memory request, hard
limit, charge, RSS, working set and pressure. A cgroup hard-limit OOM is enforced
by the kernel using charged memory, **not by the kubelet comparing RSS to a
limit**. Node-pressure eviction and process RSS are separate concepts.

Useful comparisons are runtime-managed memory versus effective Go target, live
heap versus target, and allocation/GC-cycle/assist rates versus useful progress.
The ratio of runtime GC CPU to runtime total CPU is a runtime accounting fraction,
not a fraction of CPU service delivered by the OS: runtime total includes available
processor time. Retain process/container CPU and quota pressure alongside it.
HeapInuse minus HeapAlloc describes span slack; HeapSys minus HeapInuse minus
HeapReleased describes unreleased idle heap. Only calculate differences from
matching process identities and source times; retain inconsistent observations
as a coverage limitation rather than clamping them to zero.

Metric availability is per endpoint, not implied by the presence of a
`GOMEMLIMIT` environment variable. Verify each intended process/replica and list
unsupported or unmapped families explicitly. Forwarding endpoints such as etcd's
metrics proxy require the process attribution described above. Missing metrics
are not zero-sized heaps. Select these additions in a **new, hashed run bundle**;
editing this maintained example does not update active or frozen captures.

The tested CPO exposes these runtime metrics, but its heap/allocs URLs returned
404 on both health port 6060 and metrics port 8080. Its profiling listener is
disabled and there is no existing enable flag in that tested build. No CPO heap
snapshot or `inuse_space` growth claim follows from the saved runtime gauges.
Enabling profiling would be a separate explicit image/configuration treatment,
not an automatic discovery action or a public profiling Service.

Where a process already exposes an authorized profiling endpoint, keep profiles
private and compare snapshots from the **same process incarnation**. A passive
heap profile can lag GC; a forced-GC profile (`heap?gc=1`) changes the workload.
Prefer comparable forced-GC snapshots outside the scored window, and label any
in-window profiling overhead. Cumulative `alloc_space` and retained `inuse_space`
answer different questions. Do not compare counters or heap growth across a
restart, or infer a leak merely from allocation churn. Heap and GC metrics give
continuous context; stack profiles are targeted follow-up evidence.

## Diagnosis, Not A Trigger

| Evidence | Interpretation And Correlation |
| --- | --- |
| APF rejection, dispatch, queue, wait, inflight and seats | Keep reason/flow/priority-level and KAS replica. Compare executing seats with current allocation, not request counts with nominal shares; borrowing changes allocation. Reject policies can shed without queue growth. Split wait histograms by `execute`. Demand high-watermarks can reveal work missed by instantaneous gauges. Policy/classification or a hot replica can require tuning rather than a larger size. |
| Useful throughput and latency | Pair server evidence with successful body-complete client operations, successful GET/LIST latency, canary ACK/Ready/Cleanup, rollout completion/debt, offered/dispatched rates and generator limits. Fast 429s must not improve the apparent successful latency. Server 2xx is not proof of body delivery. The retained API duration histogram has no status-code label and cannot yield successful-only latency by filtering or joining request counts. |
| Container CPU service and pressure | Rate of `container_cpu_usage_seconds_total` is CPU service received, not runnable demand. Pair with per-container CPU waiting PSI, actual request, node contention and useful work. Throttled-period fraction uses matching CFS period counters; throttled seconds are not a portable percentage of denied CPU. No configured quota means no inferred CPU cap, even when usage exceeds request. |
| Cgroup memory | Compare total charge, working set, RSS and cache separately with actual memory request and optional hard limit. A request is a scheduling/footprint target, **not a cap**. Cache is not all irreducible heap, and working set is not an exact unreclaimable-memory measurement. OOM/pressure, sustained retained demand and impact are stronger evidence than one above-request sample. Node pressure may instead identify provider placement/packing trouble. |
| Go memory and GC | Managed memory is `go_memstats_sys_bytes - go_memstats_heap_released_bytes`, not RSS or heap-live. Compare with effective `go_gc_gomemlimit_bytes` and recorded settings, recognizing disabled/unlimited values. GOMEMLIMIT is soft and distinct from the container limit. Correlate live/goal, allocation/cycle rates, GC assist and limiter changes with progress. The limiter metric is a last-cycle index, not a count of limiter events or proof of current activation. GC CPU classes are runtime accounting, not OS cgroup cores; do not divide them by process CPU as a literal utilization percentage. |
| Etcd allocated and in-use bytes / quota | Compute each member's total/quota, in-use/quota and total-minus-in-use at matching times, with positive denominators. Do not sum replica copies. Large allocation with lower in-use bytes suggests fragmentation/defragmentation; in-use includes retained MVCC history, so inspect compaction and workload growth before calling it irreducible data. Revision distance is history context, not bytes or elapsed time. |
| Etcd disk and proposals | Per-member WAL fsync/backend commit bucket deltas, proposal pending/failed, committed-minus-applied backlog and leadership changes distinguish storage/apply/availability trouble. Defrag activity and container IO pressure provide context. PVC free space is neither backend quota nor disk performance. Slow disk, leadership instability or provider storage/network faults require operational diagnosis first, not an automatic customer upgrade. |

Etcd backend quota is configured independently of RAM. A larger memory request,
GOMEMLIMIT or container hard limit does not inherently increase that quota, PVC
space, disk IOPS or throughput. An upgrade recommendation must identify which
constraint the proposed profile actually changes. Compaction/defragmentation,
provider disk/network repair, APF tuning and resource-size changes are distinct
remedies, even when each can produce similar API symptoms.

This minimal bundle does not diagnose every cause. Peer RTT/network failure
metrics, node disk-device statistics, cgroup full-stall/reclaim events, GC pause
histograms and APF execution/seat-width histograms remain targeted follow-up
captures. Pod resource requests, configured limits, owner/replica inventory,
storage class/performance settings and customer-useful work require separate
recorded evidence. Inspect effective cgroup limits alongside Pod specs: absence
or unlimited sentinel values must not become a zero limit or fabricated headroom.

## Cross-Size Validation

Treat **15 body-complete QPS for small as a rounded reference operating point**,
not a validated capacity minimum or a hard SLO. The small comparison achieved
about 15.64/14.61 successful QPS including drain time, with heavy shedding,
generator limits and poor successful-request latency. Its alternate active-window
denominator gives different numbers; do not mix them. See the
[client experience method](cards/small-v1-experience.md).

**30 body-complete QPS for medium is a campaign hypothesis**, not an observed
result, customer promise or automatic doubling law. First replay the small
workload on each precisely identified profile, then independently vary activity
and admission. Keep population/mix, measurement method, timeout/retry behavior,
replicas and host contention comparable; record unavoidable topology/storage
changes. Validate both initial startup and steady behavior, repeat/reverse arms,
and observe bounded recovery. Report successful-operation latency distributions,
shedding, generator saturation, protected progress, OOM/restarts and evidence gaps
without inventing a pass/fail latency threshold. Higher QPS alone is not success
if useful work or protected progress regresses.

For each size, preserve actual per-container requests, optional hard limits, Go
settings, APF effective allocation, etcd quota and storage profile. Assess sustained
impact plus matching resource pressure; separate maintenance/provider findings
and unknown capture from size evidence. Use the existing assessment's scoped
data points, evidence references and conditional hypotheses where appropriate;
its current metrics reducer does not implement these diagnoses automatically.
Production rules need separately agreed customer objectives, validated windows,
persistence/hysteresis and cross-size evidence. This bundle supplies none of those
thresholds and intentionally emits no production alert rules.
