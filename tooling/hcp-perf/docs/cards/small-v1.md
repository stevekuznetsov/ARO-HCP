# Small v1: Characterized Envelope, Not Support Contract

[Open the HTML presentation](small-v1.html). It is generated from the reviewed
card JSON and [node-contention evidence](small-v1-contention.md), without live
queries or external assets.

The presentation also includes [component resource timelines](small-v1-resources.md)
with requests and Go targets taken from the recorded configuration, not current
defaults. Aggregate headline requests exclude the experimental KSM collector;
its contribution remains visible separately.

[Client and canary timelines](small-v1-experience.md) show the measured degradation
through each comparison arm, including completion-based throughput, rejection
fractions and successful-request latency. Their raw data and method are preserved
separately from the presentation.

**Experimental; personal development only, not production. Measured September 10,
2026.** This card preserves the current resource-locked population, client
behavior, and health evidence before any 2x trial. It is not a supported size,
minimum viable configuration, acceptable-latency promise, or general node upper
bound. Machine-readable values and hashed citations are in [small-v1.json](small-v1.json).

The [methodology and agent handoff](../tshirt-methodology.md) explains how this
evidence was produced and how to repeat the process for another size.

## Resource Identity

The anchor is **250m CPU / 1Gi memory per KAS container**, not the whole HCP.
[small-catalog-v2.json](../../examples/small-catalog-v2.json) defines the 17
ratio/floor overrides. Its file SHA-256 is
`2dba5969c6af970b3190fa349ddfe60917e98af50a2b26bac646ef499d3c3783`.
The separately canonicalized, validated live resource fingerprint is
`def5a106dd0ef20b2f5cb23dc3dda15b4e67f522c410f2fcdf69bc498d28e035`.
These hashes describe different objects and must not be substituted for each other.

| Policy-Controlled Container | CPU Request | Memory Request | Memory Hard Limit | Go Soft Target |
| --- | ---: | ---: | ---: | ---: |
| Kube-apiserver (each of 3) | 250m | 1024Mi | 3072Mi | 858993459 bytes, about 819.2MiB |
| Etcd (each of 3) | 100m | 256Mi | 768Mi | 214748364 bytes, about 204.8MiB |
| Catalog registry (each of 3 deployed) | 50m | 256Mi | 768Mi | about 204.8MiB |
| Kube-controller-manager | 30m | 192Mi | 576Mi | about 153.6MiB |
| OpenShift apiserver | 40m | 256Mi | 768Mi | about 204.8MiB |
| OpenShift controller-manager / cluster-policy-controller | 10m | 128Mi | 384Mi | about 102.4MiB |
| Kube-scheduler | 10m | 64Mi | 192Mi | about 51.2MiB |
| Packageserver | 125m | 320Mi | 960Mi | 256MiB |
| Control-plane-operator | 550m | 128Mi | 384Mi | about 102.4MiB |
| Hosted-cluster-config-operator | 25m | 128Mi | 384Mi | about 102.4MiB |
| Ignition-server | 150m | 128Mi | 384Mi | about 102.4MiB |
| Policy fallback for unlisted controlled containers | 10m | 64Mi | 192Mi | about 51.2MiB |
| HAProxy overrides (ignition proxy / router) | 10m | 64Mi | 192Mi | disabled, non-Go |

Requests resolve from anchor ratios with floors and 1m/1Mi rounding. The live
HC/HCP policy has fallback `10m/64Mi`, memory safety multiplier 3, and Go target
80% of requested memory, truncated to bytes. CPU limits and an explicit
GOMAXPROCS override are absent in this policy. This is **not** a claim that all
namespace containers obey the fallback: downstream controllers retain their own
requests, and some have no memory hard limit or GOMEMLIMIT. A sum of explicit
limits is not a whole-HCP memory cap.

The certified, community, and Red Hat registry containers have 5-second startup,
readiness, and liveness probe timeouts (1-second connect / 2-second RPC flags).
Marketplace has a policy override but no deployed registry. The fixed-resource
profile capability is present: the CPO image advertises the actual label
`io.openshift.hypershift.container-resource-policy=true`; do not invent a
`supports-fixed-resource-profile` image label.

Baseline-validated images, not guessed current build tags:

- CPO runtime digest: `sha256:74166e56e68bd6c3d94d0ba8bd4dcda5073b08e66521c90782220661c91e7be0`.
- HO Deployment image digest: `sha256:5ffe5a9ec1141d44cddd671cafefff61a3c52ef5278ca2c05078229cd14433e8`.
- CPO source-manifest SHA-256: `f9b581e776cd4a823949559a887afbb0651ecd8b1b63cc267dbee5692b6117a2` (dirty source snapshot, not a clean-commit equivalence claim).

## Topology And Population

This is the retained **legacy HA topology, not a minimal topology**. KAS and etcd
each have three replicas; not every component has three. The comparison inventory
contains **66 active namespace Pods, 144 regular containers plus one restartable
init sidecar = 145 running containers**, including one experimental persistent
`kube-state-metrics-hcp` Pod. Three completed Pods are excluded. The JSON preserves
the entire active workload replica inventory, including downstream components.

The full running-container request sum is **3,870m / 14,506Mi
(14.166015625GiB)** for that namespace scope. Excluding experimental KSM gives
**65 Pods, 144 running containers, 3,860m / 14,442Mi (14.103515625GiB)**.
The sidecar and KSM are distinct 10m/64Mi allocations. Earlier reports disagree
about their attribution; these numbers are recomputed from the actual comparison
inventory, not a copied subset or a finalized production billing boundary. They
include regular containers and the restartable init sidecar, not a blind sum of
all transient init containers. Reconcile scheduler-effective requests and the
experimental KSM billing scope in the independent inventory audit before pricing
a deployable profile. Recorder, competitors, management system, and fleet services
outside this namespace are excluded, not free.

| Layer | Observed Configuration |
| --- | --- |
| HCP hosts | 3 x Standard_D4s_v3, 4 vCPU / 16GiB raw per node; not production Dsv6 |
| Host allocatable CPU | 3,860m each; frozen competitor requests 635m / 1,856m / 1,613m, four burn workers each |
| Guest workers | 3 x Standard_D8s_v5, 8 vCPU / 32GiB raw each; 7,500m allocatable CPU each |
| Guest Pod capacity | 250 each, 750 total; capacity metadata, not measured sustainable throughput |
| Retained application population | 600 Ready pause Pods, 120 Deployments, 5 replicas each, 20 apps/namespace, 6 namespaces |
| Application recipe | 10m / 32M requests per Pod (decimal M, not Mi); not a representative application CPU load |
| Platform population | 96 other active guest Pods at comparison preflight; 696 total, 54 aggregate spare slots before surge/canaries |
| Latest comparison preflight placement | 198 / 208 / 194 application Pods; not evenly spread or a strict 200-app-Pods/node guarantee |

The historical density continuation ended at 195/209/196 application Pods and
228/240/228 total Pods; do not substitute that older placement for the comparison
preflight. Aggregate headroom does not guarantee per-node scheduling. CPU auto-fill
90 means 90% of free **requested** CPU headroom, not a measured 90% utilization.
Competitors were discovered once and held fixed between comparison arms.

## Density Screens

One full replacement per application per hour is the **schedule rate**, not an
hour-long soak. Each completed low-activity screen scored 300 seconds plus 30
seconds recovery, with `maxOutstanding=1`, 10 steady canaries, and CPU competition.

| Application Pods | Deployments | Completed / Scheduled Rollouts | Completed Canaries | Soft-Degraded Canaries | Case |
| --- | ---: | --- | --- | ---: | --- |
| 60 | 12 | 1/1 | 10/10 | 0 | density-v5-apps12 |
| 150 | 30 | 3/3 | 10/10 | 2 | density-v5-apps30 |
| 300 | 60 | 5/5 | 10/10 | 2 | density-v6-apps60 |
| 450 | 90 | 8/8 | 10/10 | 3 | density-v6-apps90 |
| 600 | 120 | 10/10 | 10/10 | 2 | density-v6-apps120-r2 |

All ended with zero final backlog/outstanding/not-started debt. All retained
incomplete capture. The first 600 attempt failed before scoring on a credential
harness race; it is not an HCP failure or a successful screen. V5 lacks per-request
HTTP tracing; V6 traces do not cover all cluster traffic or response-body/watch
completion. Reused etcd/cache history, accepted CNO refresh, worker replacements,
the pre-existing console exception, and instrumentation changes remain confounds.

## APF Client Behavior

The latest comparison used the same retained 600-Pod population and six benchmark
identities. Per namespace, the direct mix was GET one Deployment (weight 2), LIST
Pods (weight 1), LIST ReplicaSets (weight 1), with LIST limit 100 and no pagination.
One reusable client per namespace uses client-go/Go transport defaults, including
keep-alive and HTTP/2 where negotiated. Connections can stick to a KAS replica;
this is **not per-request round-robin** or guaranteed even backend distribution.
Client-go QPS throttling is disabled for direct load; there are no application-level
429 retries. Transport HTTP attempts are counted separately from logical requests.

Each arm offered 50 requests/s for 180 seconds: 9,000 offers, max in-flight 128,
10-second request/body deadline, seed 42. Background turnover retained the hourly
schedule, **maxOutstanding=16**, six scheduled rollouts over 180 seconds plus 30
seconds recovery. Six protected canaries per arm ran at 30-second cadence, with
ACK/Ready/Cleanup hard deadlines of 30/120/120 seconds. Canary APF remained 5
shares with Queue (8 queues, hand size 2, queue length 10); benchmark was Reject,
lendable 0, borrowing limit unset. KAS total inflight stayed **200 = 150 + 50**.

| Same-Window Metric | 50 Shares | 100 Shares |
| --- | ---: | ---: |
| API scheduling window, UTC | 18:35:07.317-18:38:07.318 | 18:39:10.832-18:42:10.833 |
| Nominal seats per KAS | 33 | 56 |
| Sampled current allocation across replicas | 62-128 | 116-138 |
| Dispatched logical requests | 8,369 | 8,095 |
| Successful bodies | 2,885 | 2,670 |
| Successful QPS, active denominator including drain completions | 16.028 | 14.833 |
| HTTP 429 | 5,447 | 5,346 |
| Body/request timeouts | 37 | 79 |
| Generator-limited offers | 631 | 905 |
| Successful GET p95, seconds | 3.560 | 4.043 |
| Successful LIST p95, seconds | 8.282 | 9.169 |
| Bounded completed / accepted rollouts | 6/6 | 5/6 |
| Hard-completed / soft-degraded canaries | 6/6 | 6/5 |

The QPS values above divide all successful bodies, including drain completions,
by the actual approximately 180-second scheduling span. Strictly completed within
that active span: 2,842 / 2,622; including drain time in the denominator gives
15.641 / 14.607 QPS. HTTP 200 headers with a body timeout are **not successes**.
Displayed decimals are rounded observations, not precision or repeatability claims.

Borrowing matters: current allocations substantially exceeded nominal seats, so
50-to-100 shares did not double a dedicated budget. Server counters observed
`concurrency-limit` rejection, independently of client classification; they do
not prove a per-request or per-verb cause. Both generators dropped offers locally.
This is achieved behavior under these client settings, not true APF capacity.

100 shares had lower successful throughput, worse successful GET/LIST p95, more
timeouts, and one incomplete rollout at the 210-second bound. That accepted
rollout converged in later quiescence, leaving all six eventually converged; the
bounded failure remains a failure, not a rejected write or crash. Roughly 65-66%
of dispatched requests were shed, and even successful short GET/LIST requests
took seconds, with LIST p95 near nine seconds. **This is poor interactive client
experience, not a promise of acceptable behavior.** One small sequential
comparison does not establish a repeatable causal estimate or justify more shares.

## Resource Use

Keep configuration and usage, and different cases/windows, separate:

| Case And Window | Scope / Statistic | Observation |
| --- | --- | --- |
| Latest APF comparison, 100-share active window | Busiest KAS cgroup usage sampled maximum | 940.426MiB; 91.84% of request, 30.61% of hard limit |
| Same 100-share window | Same KAS Go sys-minus-heap-released sampled maximum | 895.544MiB; 109.32% of managed soft target; partial coverage |
| Latest APF comparison, 50-share active window | Busiest KAS observed-span CPU rate | 0.551 cores, about 220% of its 250m request |
| Earlier direct-api-1655, nominal 100-QPS case window 17:01:56.550-17:04:56.550 UTC | Whole namespace including KSM, max simultaneous summed 1m CPU rate / cgroup usage | 3.326256 cores / 9.965706GiB |

The earlier direct-load estimate has 36 evaluation timestamps with all 145
container identities for each metric (5-second grid, not independent scrapes).
Deduplicate namespace/Pod/container per timestamp, sum simultaneous containers,
then take the maximum; CPU and memory maxima occur at different times. It is
not a latest-comparison whole-HCP measurement or proof that the nominal 100-QPS
case completed. Including its later recovery changes the CPU maximum, so the
window is part of the value. Usage includes reclaimable cache; it is not RSS,
working set, Go managed memory, or a schedulable request.

Available management-worker samples show 100% non-idle CPU. KAS consumption above
request is resource overspending, not resource-policy drift or an automatic hard
failure. Go's soft target is not the cgroup hard limit. Sampled extrema are lower
bounds on true peaks, not safe memory ceilings.

## Independent Health

- **Workload:** both full API arms completed; all 12 protected canaries hard-completed, 11 soft-degraded; one bounded treatment rollout failed then converged later.
- **Safety:** 71 observations, zero recorded new OOM/hard failures, restart deltas, non-Ready violations, observation errors, or resource drift. No crash point found. This is scoped observation, not historical or continuous proof; cross-replacement restart-storm scoring remains a gap.
- **Recorder/recordio:** archived summary spans 18:33:58.054-18:43:01.728 UTC; 55 polls, 440 samples, zero polling API errors; 8/8 watches initialized, zero recorded watch/log errors or gaps. Shutdown is `canceled`, `live.incomplete=true`. Periodic durability is 1 second; successful export does not establish lossless capture.
- **Integrity:** archive SHA-256 matches; review reports 3,127 raw files / 5,902,723,338 bytes verified without mismatches. Integrity is independent of capture completeness.
- **Metrics:** 487 query records successful, 516 incomplete; APF/Go gaps up to about 94 seconds at 50 shares and 50 seconds at 100. One management worker lacks treatment CPU/PSI. Cgroup KAS memory samples cover both active windows; APF/Go/process and whole-node coverage do not.
- **Finalization:** actual outcome is `completed`, all children stopped, 600 Pods retained, APF restored to 50; original partial-node-backfill error remains. The reducer's generic "campaign final outcome/clean finalization unavailable" warning is retained, but the later actual outcome is present, not missing. Neither means clean capture or qualification.

## Cost Illustration

Target: **$0.125/HCP-hour**, 50% of a $0.25 fee, not a new fee recommendation.
The retained [Azure retail API](https://prices.azure.com/api/retail/prices) snapshot
verified September 9 at 21:43:29.779968 UTC gives West US 3 Linux Consumption
**Standard_D16s_v6 $0.806/hour** and **Standard_E16s_v6 $1.058/hour**.
These are v6, not the v3 illustrative CLI fixture. No fresh price query was made.
Raw capacities are 16 cores and 64/128GiB; measured D4s_v3 CPU is not validated
as throughput-equivalent to v6 CPU.

Use only the optimistic divisible CPU/memory lower bound
`price * max(cpuCores / 16, memoryGiB / rawNodeMemoryGiB)`, never add the shares.

| Demand Basis (Not A Bill) | D16s_v6 $/HCP-hour | E16s_v6 $/HCP-hour |
| --- | ---: | ---: |
| Comparison namespace running requests including experimental KSM | 0.194951 | 0.255904 |
| Same inventory excluding KSM | 0.194448 | 0.255243 |
| Earlier direct-load window's max CPU / max usage, explicitly separate | 0.167560 | 0.219949 |

All these illustrations exceed the target. They exclude allocatable reservations,
system/recorder overhead, HA/failure reserve, fragmentation, placement, Pod/Swift
NIC constraints, storage, network, services, and guest workers. Three router Swift
NIC requests are not free capacity. No production packing, "we fit", general node
upper bound, or acceptable performance follows from this arithmetic.

## Carry Forward

Preserve this small resource profile and its evidence unchanged. **Recommendation,
not applied:** no further APF-share-only increase before the first separately
approved 2x resource comparison. Start that comparison at benchmark shares 50,
total KAS inflight 200, the same replicas, node topology, 600-Pod recipe, direct
mix/rate/client settings, and background schedule; only then consider APF retuning.
Placement and headroom must pass preflight; unchanged node count is an experimental
control, not a claim that larger requests can schedule there.

A 500m/2Gi KAS anchor is an anchor-relative 2x experiment. With original ratios,
zero-ratio floors and fallback requests can remain fixed, as can downstream
components outside the projector; it is **not a literal doubling of the whole
HCP bill**. The separately prepared [Double Small Plan](double-small-plan.md)
describes doubling all policy-controlled requests, including floors/defaults.
Choose and record the exact scaling rule before a trial; neither interpretation
authorizes applying a profile, adding nodes, or starting live load.

## Evidence

All `sourceFile` paths in [the JSON card](small-v1.json) are relative to the
`ARO-HCP-perf-runner` workspace root. Citation IDs distinguish live inventory,
profile bytes, density milestones, comparison review/manifests, recorder archive
members, earlier direct-load windows, and retail provenance. Private `_artifacts/`
files are not distributed with this card; hashes identify retained local evidence,
not public availability or completeness. Do not publish unsanitized archives,
raw logs, kubeconfigs, credentials, or unrelated resource identifiers.

Related documentation: [hcp-perf](../../README.md), [API Load](../api-load.md),
[Admission Tuning](../admission-tuning.md), [Sizing](../hypershift-sizing.md), and
[Cost Model](../cost-model.md). Parent README/index wiring is intentionally deferred.
