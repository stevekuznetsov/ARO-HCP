# Small v1 Resource Evidence

`small-v1-resources.json` is the offline-reviewed, portable resource time-series
dataset for card `small-v1`, using exactly `hcp-perf.resources/v1`. It is derived
from saved APF comparison evidence, not current cluster state or sizing-policy
defaults. No Azure access, live queries, campaign execution, original-worktree
changes, renderer changes, or parent-card regeneration were performed.

## Scope And Budget

The inventory is **66 active Pods, 144 regular containers and one restartable
init container**, across 44 actual Deployment/StatefulSet workload names. All
145 containers were running in both snapshots. Three completed Pods and
nonrestartable init containers are excluded. The restartable init container is
`cluster-image-registry-operator-f4877d985-2thst/apiserver-token-minter`.

| Scope | Containers | CPU Requests (cores) | Memory Requests (MiB) |
| --- | ---: | ---: | ---: |
| Entire active HCP namespace | 145 | 3.87 | 14506 |
| Excluding experimental KSM | 144 | 3.86 | 14442 |
| Experimental KSM only | 1 | 0.01 | 64 |

The KSM series has `instrumentation: true`. Its budget remains included in the
raw namespace scope; consumers must explicitly exclude it for the second row.
All 145 series have known CPU/memory requests, 98 have explicit memory limits,
and 92 have explicit literal `GOMEMLIMIT` values. All CPU limits are absent and
represented as null. The remaining 47 memory limits and 53 Go targets are null,
not zero. No whole-HCP hard cap is inferred.

Budgets come from `after/actual-pods.json`, cross-checked against full Pod specs
and the immutable `before` snapshots. Active Pod UIDs, full Pod specs, normalized
container resource/env inventories, and restart counts agree before and after.
The 19 `resource-guard.jsonl` observations report no mismatches; the two checked
policy hashes agree. These justify fixed reference lines for this experiment,
but are sampled evidence, not continuous configuration monitoring. The first
guard is at 18:33:47.916116 UTC, after the plot begins. Policy hashes alone do not
establish the budget of downstream containers.

Requests are a sum of regular and restartable-init **spec requests**, not
scheduler-effective Pod requests, transient-init maxima, Pod overhead, billing
allocation, or observed use. Recorder, CPU competitors, other management
namespaces, fleet services, and guest workers are outside this dataset.

## Timeline And Coverage

The window exactly matches `small-v1-contention.json`:

- Start: `1789065165.85`, 2026-09-10 18:32:45.850 UTC.
- End: `1789065792.701458`, 18:43:12.701458 UTC.
- Native step: 5 seconds; last evaluation: 18:43:10.850 UTC.
- 126 points per container, 18,270 point objects, without downsampling.
- All four cgroup metrics: 18,270 non-null values each, 73,080 total.
- Go-managed memory: 748 non-null values across seven containers. Three etcd
  mains and CPO have 126 each; KAS suffixes `2945p`, `bz7fk`, and `zq2j4` have
  80, 74, and 90 respectively. Other containers retain null Go-managed values.

These are evaluation slots, not independent scrapes. Gauge lookback and trailing
rates can reuse older scrapes, so full slot coverage does not establish fresh
five-second observations. The saved stream has a different evaluation offset
and is not merged, rounded onto the backfill grid, or used to fill gaps.

The actual API arms are 18:35:07.316991 to 18:38:07.317848 (50 shares), and
18:39:10.831889 to 18:42:10.832792 (100 shares). Use the contention dataset's
boundaries for annotations rather than inferring test start from first resource
samples. CPU rates include the preceding minute: initial observations can
include activity before this window, and early arm samples include pre-arm
activity. The post-stop tail is too short to demonstrate settled recovery.
Pre-start is not an idle whole-cluster control.

## Extraction Method

1. Select nonterminal Pods from the saved actual-Pod inventory. Retain regular
   containers and only init containers with `restartPolicy: Always`. Confirm
   each retained container's running state. Cross-check the before/after UID,
   full spec, resource/env inventory, and restart count.
2. Resolve the full Pod's controller owner reference by **UID** through saved
   ReplicaSets to saved Deployments, or directly to the saved StatefulSet.
   Check kind/name and the workload-template inventory. All 66 Pods resolve;
   no normalized-name fallback or invented Deployment name is needed. In
   particular, actual workloads are `azure-cloud-controller-manager`,
   `capi-provider`, and `router`, not parent-card component aliases. KSM's real
   ReplicaSet resolves to Deployment `kube-state-metrics-hcp`; do not treat the
   shortened actual-Pod `owner` helper as authoritative.
3. Parse Kubernetes resource strings with decimal arithmetic before conversion
   to JSON numbers: `10m` CPU is 0.01 core, `64Mi` is 64 MiB, `1Gi` is 1024 MiB.
   Compare quantities semantically, since snapshots can use equivalent units.
   Parse only each container's explicit literal `GOMEMLIMIT` using Go byte
   units, then divide bytes by 1048576. Do not derive 80% of requests, resolve
   `valueFrom`/`envFrom`, or assume image defaults. Unresolved, absent, invalid,
   or `off` targets stay null with a reason in `method.bindings`.
4. Verify file-byte SHA-256 against every selected chunk manifest. Verify the
   chunk manifest against the retained `_analysis/raw-integrity.json` inventory.
   Read all 42 backfill and 17 stream chunks for both `node-queries` and
   `runtime-detail`, selecting four cgroup queries and two runtime queries.
   The extraction verifies 252 backfill plus 102 stream response files and 118
   chunk manifests. Stream identities are inspected, but only backfill values
   are plotted.
5. Join cAdvisor rows on the exact HCP namespace, Pod and container; require
   `job=cadvisor`, the saved Pod's node, Pod UID in the cgroup path, and the
   running container ID in both metric `name` and cgroup path. CPU must have
   `cpu=total`. This rejects old container incarnations and prevents duplicate
   Pod/component aggregation. All retained samples pass these identity checks.
6. For each exact native timestamp, copy saved
   `rate(container_cpu_usage_seconds_total[1m])` to `cpuCores`. Divide saved
   `container_memory_working_set_bytes`, `container_memory_rss`, and
   `container_memory_usage_bytes` by 1048576 for `workingSetMiB`, `rssMiB`, and
   `usageMiB`. No process CPU, raw-counter derivative, or namespace aggregate is
   substituted. RSS here is **cgroup RSS**, not process resident memory.
7. Derive Go-managed memory as
   `(go_memstats_sys_bytes - go_memstats_heap_released_bytes) / 1048576` only
   where every other label and evaluation timestamp match. Require matching
   process RSS and process-start metrics and one of the reviewed endpoint
   mappings below. Do not substitute heap-in-use, heap allocation, live heap,
   RSS, or total cgroup usage. These quantities are not interchangeable.
8. Collapse identical repeats at chunk boundaries; abort on conflicting
   duplicates, nonfinite or negative values, or identity mismatches. The
   inspected backfill cgroup and selected runtime values contain 28,040 exact
   repeated samples and no conflicts. This count includes process metadata and
   runtime rows that are not exported as Go-managed curves.
9. Emit one point per native grid slot per container. Missing observations are
   null, never zero, interpolated, clamped, or bridged. No additional component
   aggregate series are emitted. Extrema below are maxima of valid samples
   anywhere in the whole displayed window, not necessarily simultaneous.

## Go Process Attribution

| Destination | Saved Scrape Identity | Attribution Evidence |
| --- | --- | --- |
| Three KAS main containers | `job=kube-apiserver`, `container=kube-apiserver`, `endpoint=client`, port 6443 | Exact Pod/IP and container; Pod spec exposes the client port in the `hyperkube kube-apiserver` container; matching process metrics. |
| CPO main container | Custom namespace-qualified `controlplane-operator` job, `container=control-plane-operator`, `endpoint=metrics`, port 8080 | Exact Pod/IP, sole regular container, explicit `--metrics-addr` and named port; matching process metrics. |
| Three etcd main containers | `job=etcd-client`, `container=etcd-metrics`, `endpoint=metrics`, port 2381 | Pod spec runs `etcd grpc-proxy start` with a single backend `--endpoints=https://localhost:2382`; main etcd sets `ETCD_LISTEN_METRICS_URLS=https://0.0.0.0:2382`. The scrape container label describes the forwarding endpoint, not the observed process. |

The etcd mapping is deliberately not a naive Pod/container-label join. The
saved process RSS on this endpoint exceeds the proxy's cgroup usage at every
paired evaluation, consistent with the configured main-process metrics
forwarding and inconsistent with attributing it to the small proxy container.
This magnitude check is corroboration, not the mapping rule. Main and proxy
start within the same second in some Pods, so start time alone cannot
disambiguate them. `healthz` listens separately on port 9980; `etcd-defrag` is a
different controller. Neither receives these Go values, and neither does
`etcd-metrics`. Full source labels are used to pair runtime metrics before the
explicit destination-container mapping. No private IPs are copied into the
portable mapping metadata.

Only these seven endpoint mappings are reviewed here. Other runtime metrics
exist, but their presence alone is not sufficient to assign them. Catalog
registry Go curves remain null while their literal targets and cgroup curves
are available. Every unreviewed destination has an explicit gap reason in
`method.bindings`; missing points within a reviewed curve indicate absent
matching runtime observations, not a zero-sized Go heap.

## Observed Maxima

Memory columns are MiB; CPU is cores. KAS requests 0.25 core / 1024 MiB per
replica, with a 3072 MiB hard limit and literal Go target approximately
819.2 MiB. Etcd requests 0.1 core / 256 MiB per main, with a 768 MiB hard limit
and approximately 204.8 MiB Go target. Values below are rounded for reading;
JSON preserves the calculated numbers and original fractional timestamps.

| Main Container / Replica | CPU Max | Usage Max | Working Set Max | RSS Max | Go-Managed Max |
| --- | ---: | ---: | ---: | ---: | ---: |
| KAS `2945p` | 0.537886 | 878.828 | 862.262 | 855.062 | 826.206 |
| KAS `bz7fk` | 0.460782 | 876.180 | 861.137 | 855.219 | 824.299 |
| KAS `zq2j4` | 1.278824 | 940.426 | 924.477 | 916.414 | 895.544 |
| etcd-0 | 0.079119 | 495.688 | 236.652 | 161.750 | 144.139 |
| etcd-1 | 0.080319 | 560.172 | 203.137 | 173.117 | 148.644 |
| etcd-2 | 0.091817 | 559.793 | 203.586 | 174.297 | 148.581 |
| CPO | 1.460860 | 133.617 | 133.617 | 131.012 | 129.350 |
| Certified registry | 0.016332 | 305.801 | 116.797 | 114.059 | unknown |
| Community registry | 0.028109 | 335.172 | 197.406 | 194.812 | unknown |
| Red Hat registry | 0.137638 | 565.219 | 91.863 | 89.441 | unknown |

- KAS CPU maxima are 2.152x, 1.843x, and 5.115x their per-replica request.
  Maximum KAS CPU excess is 1.028824 cores (`zq2j4`). All KAS cgroup memory
  maxima remain below the 1024 MiB request; Go-managed maxima exceed their
  separate soft target by 7.006, 5.099, and 76.344 MiB respectively.
- Etcd cgroup usage exceeds requests by 239.688, 304.172, and 303.793 MiB,
  reaching 1.936x, 2.188x, and 2.187x requests. Its working set, RSS and measured
  Go-managed memory remain below their respective request/Go reference values.
  Do not label its cgroup excess as Go-target excess.
- CPO requests 0.55 core / 128 MiB. Its CPU maximum is 2.656x request and its
  Go-managed maximum is 1.263x the approximately 102.4 MiB target, an excess of
  26.950 MiB. Its hard memory limit is 384 MiB.
- All three registry containers request 0.05 core / 256 MiB. Red Hat registry
  has the largest namespace cgroup-usage/request ratio: 565.219 / 256 = 2.208x,
  or 309.219 MiB above request, while its working set is only 91.863 MiB.
- Across all containers, 30 have a CPU sample above request, 15 have usage
  above memory request, nine have working set above request, and four reviewed
  Go curves exceed the literal Go target. The largest CPU/request ratio is
  ingress-operator at 9.227x (0.092266 cores against 0.01); the largest working
  set/request ratio is OVN's `jbmmr/socks-proxy` at 2.185x (21.848 / 10 MiB).

Above-request consumption is overspending, not configuration drift or an
automatic hard-limit failure. GOMEMLIMIT is a soft runtime target, not a
container memory cap. This short, reused-cluster sequential experiment does not
establish sustainable capacity, causality, a breaking point, or production SLOs.

## Portable Contract

Top-level fields are exactly `schemaVersion`, `cardID`, `window`, `series`,
`sources`, `limitations`, and `method`. The window contains numeric Unix-second
`start`/`end` and `expectedStepSeconds: 5`. Each series has the requested identity,
workload, Pod, container, replica label, init flag, nullable controlled flag,
requests, limits, nullable Go target, and points. Only the KSM series additionally
has `instrumentation: true`. Each point contains the timestamp and five nullable
metrics. `controlled: true` means a policy hash exists on the containing Pod;
null means no such evidence, not a proven exemption or per-container audit.

`method.bindings` links each series ID to its saved Pod UID, actual owner chain,
container group, resource source, policy annotation, and missing-target/runtime
reasons. `method.goMappings` records the reviewed source labels and destination
IDs. `method.coverage`, `requestTotals`, and validation counts expose the exact
scope and calculation history without adding extra plotted series.

Use a bounded workload/container selector, defaulting to main `kube-apiserver`
with all three replicas. Offer etcd, CPO, each deployed registry catalog, and
the full inventory. Keep **all** budgets known to the card, not just selected
curves. There are 145 series, below the renderer's 512-series limit, but they
must not all be plotted simultaneously. Use separate CPU and memory scales;
show request, optional hard limit, and optional Go target distinctly. Break
lines at null and label unavailable Go data as unknown. Never duplicate the
per-Pod curves with component aggregates or silently omit instrumentation from
the namespace totals. This change supplies the dataset and contract only;
renderer implementation is owned separately.

## Provenance And Review

All evidence paths are relative to this worktree root. The primary run is
`_artifacts/perf-resource-locked-scan/apf-shares-comparison-20260910-1832`.
`sources` contains 483 unique `{id, file, sha256, query?}` records. Every selected
raw metric response carries its exact saved query, filename and file-byte hash;
chunk manifests and the pre-existing integrity inventory provide the hash chain.
Inventory, full Pod/workload evidence, policy checks, resource guard and the
contention window source are also hashed. Source hashes establish byte identity,
not metric completeness, source freshness, or independent experiment quality.

A separate offline validation pass rehashed all 483 references, independently
enumerated the 145 identities from full Pod specs, reparsed all budgets and
literal Go targets, checked the exact schema keys and native grid, compared all
73,080 exported cgroup values directly with raw query results, and recalculated
all Go differences and overspending counts. All checks passed. The JSON uses
one compact line per point to bound size without dropping samples or precision.
No secrets, credentials, raw logs, full env arrays, private addresses, cgroup
paths, or container runtime IDs are included in the portable dataset.
