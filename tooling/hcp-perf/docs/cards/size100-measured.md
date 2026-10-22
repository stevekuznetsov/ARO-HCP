# Size100 Measured Envelope

September21-22,2026. **Size100 is a separate experimental size at100% of the $0.25
hosting fee**, between Small's $0.1875 and Medium's $0.375 targets. It is not
Medium100, renamed Medium, a supported size or a production SLO. Open the separate
[Size100 dashboard](../dashboard/size100.html) or [portable JSON](../dashboard/size100.json).
Published Small/Medium datasets, pages and cards are unchanged.

## 15-QPS Population Tests

September22 added only the two requested population points: fresh **600Pods /
120apps** and **1200Pods /240apps**, both on the same16guest nodes and six
management workers. Both seeds fully completed before API measurement. Each
15-QPS point was then rejected at its predeclared60-second checkpoint, not during
seed or by a hard availability/capture guard. The allocation and modeled$0.24986
HCP-hour cost did not change. All four prior measured run objects remain unchanged.

| Population | Decision Offers / Dispatched / Completed | Decision HTTP429 | Decision |
| --- | ---: | ---: | --- |
| 600Pods /120apps | 900 /900 /900 | 106 (11.7778%) | Screened-out |
| 1200Pods /240apps | 900 /900 /900 | 86 (9.5556%) | Screened-out |

Both checkpoint cohorts have **zero timeouts, cancellations and generator loss**;
the unchanged inclusive5% threshold was exceeded by serverHTTP429 responses.
Native state failed, executionStatus completed (finalized), workload/capacity
screened-out, fullWindow=false. The intended180seconds was **not completed**.
These are two single short observations, not evidence of a monotonic capacity
function:600 was worse than1200 in this cohort. The prior day's300-Pod15QPS pass
(11/2700=0.4074%) is not rescored; day, catalog, object history and population
treatment confound a perfectly controlled comparison. No hour or new QPS/refinement
was run or inferred.

| Population | Actual Offer Seconds | Final Offered / Completed | Final Success / HTTP429 / Request-Canceled | Whole-Run Successful QPS | Raw Successful p95 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 600Pods | 61.600589904 | 924 /923 | 799 /106 /18 | 12.968092631 | 2.976062681s |
| 1200Pods | 62.297076124 | 934 /934 | 832 /91 /11 | 13.352731438 | 2.686037817s |

Dispatched equals completed in both final cohorts. Successful QPS includes final
body drain; raw nearest-rank p95 differs from rounded histogram2.977/2.687s.
600API offer stop was10:56:54.594764063UTC, final body10:56:54.606935783;
1200 offer stop11:36:41.795734831UTC, final body11:36:41.808007461. Cutoff/drain
counts must not replace the fully delivered900-request decision cohorts.

The600 final `generatorLimited=1` is entirely **`canceledOffers=1` at cutoff**,
with `missedDeadline=0` and `slotsFull=0`. It is distinct from18 canceled dispatched
requests. The automatic aggregate helper labeled the point generator-limited and
finalized conservatively; its raw assessment remains unchanged. The native
checkpoint independently establishes server request-quality rejection, **not a
generator ceiling**.1200 has zero final generator-limited/canceled offers and
zero missed deadlines/full slots. API timeouts and other HTTP/transport/body-limit
errors are zero in both.

600 turnover completed1/6 scheduled, with2 failed,3 notStarted, backlog0 and
outstanding0.1200 completed2/12, with2 failed,8 notStarted, backlog1 and
outstanding0. Both measured two canaries with zero hard failures. Early cutoff is
not a full turnover-capacity success. Native health remains **degraded**, capture
**incomplete**; probe warnings56/28, with no observed unhealthy profile, new
restart/OOM, canary failure or hard guard.1200 capture history retained14warnings,
one recovery and incomplete=true. No zero-fill or clean qualification claim.

Each seed used1QPS/burst1, request30s, seed timeout1800s/setup3000s. The completed
seed windows were10:46:50.247-10:54:59.913 and11:18:11.841-11:34:28.551UTC.
Native UID cleanup separated the old300, fresh600 and fresh1200 fixtures; no
reseed over retained objects. Each capture had a fresh52-target pilot; stopped
epochs were not restarted. The15QPS/10s/max128/2:1:1 first-three-namespace API mix,
LIST100, seed42,60/120s minimum100-request checkpoints and60s recovery remained.
Application/object count and turnover also grow, not only Pod residency.

Normal CronJob retention removed one previously bound completed Job. The **new**
profile retires only that absent UID declaration: **49 =47base +2remaining exact
historical completed Jobs**, with66 Ready serving Pods/12endpoints. The observed
new completed child follows current Size100 CronJob policy. No agent Job deletion,
generic ignore or old-spec rewrite occurred; completed Jobs are not running charge.
The original four cases retain their50-declaration metadata.

New catalog registry/extract bindings, without resource/env changes:

| September22 Catalog | SHA-256 |
| --- | --- |
| Certified | `c356302d9414b0a9c4b7ca662d689776ea7e8bc6b66a09814f53a67a33b50cf6` |
| Community | `cdd79f80bf6fec37b4ba67b23d86d82100c7afee139ce8f9eb02f9e807927e15` |
| Red Hat | `958ab6074ebd031fe41089a22b63aac61a025e7a81b0265c0f37d158335570ec` |

The new export retains the43 steady component references. It uses explicit
observed image rebindings on captured historical workload objects, not invented
current Deployment/Pod/runtime identities. CPU distributions cover36/43 components
at600 and37/43 at1200; whole-HCP CPU/memory totals are unavailable (0/4 complete
aligned opportunities), not zero consumption. Missing replica/runtime/freshness
evidence remains excluded.

At the combined handoff,1200/1200 workload Pods and16guest nodes were Ready,
canary Pods zero, captures/owned writers stopped and claims free. The600 fixture
had been natively cleaned. This is reported terminal evidence, not a fresh live
check. The retained1200 own screened-population receipts would be required for
future reuse, not old600/300 inventory. No further test or hour is authorized here.

## Original 300-Pod Cases

The original four cases used **300 resident Pods /60 applications /16 guest workers**, with
six management workers. Request gates passed at5,10 and15QPS for180seconds;
20QPS was screened out at the predeclared60-second checkpoint. The practical
short-test bracket is **15 passing /20 failing**, not an exact maximum or universal
server ceiling. That cohort has **no hour or unloaded-baseline result**; the new
600/1200 tests are the separate September22 cohort above.

| Offered QPS | Actual Offer Seconds | Final Successful / Dispatched | Final HTTP429 / Canceled | Successful QPS | Raw Successful p95 |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 5 | 180.000377488 | 900 /900 | 0 /0 | 4.998870514 | 1.403548459s |
| 10 | 180.000095189 | 1799 /1800 | 1 /0 | 9.991405799 | 0.622870461s |
| 15 | 180.000987129 | 2689 /2700 | 11 /0 | 14.935419770 | 0.651952321s |
| 20 | 61.786405370 | 1143 /1235 | 84 /8 | 18.495618187 | 1.353365819s |

Offered/dispatched/completed totals coincide in each final cohort. All four have
zero API timeouts, generator-limited offers, transport, other HTTP and body-limit
errors. Successful QPS includes final body drain; p95 is nearest-rank over raw
successful request events, not the rounded native histograms1.404/.623/.652/1.354s.
The first run delivered **900 requests**, not9000.

The20QPS decision cohort was **76/1200 HTTP429 =6.333333%**, exceeding the inclusive
5% allowance with all1200 requests delivered/completed and no timeout/cancellation
or generator loss in that cohort. Final cutoff/drain added traffic:84 rejections
and8 cancellations among1235 completed requests. Do not substitute84/1235 for the
decision fraction, or interpret18.4956 useful QPS as accepted capacity. Native
state failed, request quality exceeded, capacity/workload `screened-out`,
`fullWindow=false`; execution `completed` means finalized, not a full180s pass.

The5/10/15QPS passes are **met-not-qualified**, not clean qualification. Native
health is **degraded** and capture **incomplete in all four cases**. Ancillary
probe increments were12/7/2/2 in QPS order; no observed unhealthy profiles, new
restarts/OOMs, canary failures or hard guards. Passing cases completed3/3 rollouts
and5 measured canaries each;20QPS completed1/3 scheduled rollouts and2 measured
canaries before cutoff. Zero measured hard canary failures is scoped evidence,
not continuous health or lossless observation.

Policy remained strict:180s intended measurement plus60s recovery,60/120s
checkpoints with minimum100 completed, ten-second request deadline,128 in-flight,
seed42, Deployment GET : Pod LIST : ReplicaSet LIST weights2:1:1 across the first
three fixture namespaces, LISTlimit100. Fresh60-app seed used1QPS/burst1 and
900s seed/1200s setup bounds. Native population verification/reuse joined later
points without reseeding; the15QPS midpoint used the20QPS point's own screened
population pins. Earlier failures and10-pass/20-fail evidence were not rescored.

## Budget And Profile

| Accounting | Size100 |
| --- | ---: |
| Target / hosting fee denominator | $0.25 /$0.25 per HCP-hour |
| Exact modeled cost | 12493/50000 = $0.24986 per HCP-hour |
| Charged CPU / memory | 4960m /20285Mi |
| Steady requests | 4741m /18543Mi |
| Scheduler-effective requests | 4741m /18577Mi |
| Fixed downstream, included once | 470m /2090Mi |

The model spends99.944% of the fee, leaving$0.00014/hour quantization slack.
CPU/memory are independent bounds, not additive spending allowances. It retains
the September11 Linux consumption `Standard_D16s_v6` West US3 snapshot at
$0.806/node-hour and raw16000m/65536Mi, not actual spending on the execution hosts.
Steady, scheduler-effective and quota/limit-charged accounting are distinct.

| Main Container | Replicas | CPU / Memory Request | Memory Limit |
| --- | ---: | --- | ---: |
| KAS | 3 | 325m /1180Mi | 1298Mi |
| etcd | 3 | 81m /626Mi | 689Mi |
| CPO | 1 | 309m /200Mi | 220Mi |
| Scheduler | 2 | 100m /88Mi | 97Mi |
| Packageserver | 3 | 150m /368Mi | 405Mi |
| KCM | 2 | 67m /220Mi | 242Mi |
| OAS | 3 | 67m /294Mi | 324Mi |
| CAPZ | 1 | 67m /178Mi | 196Mi |

CPU limits equal requests, memory limits round each container to110%, Go targets
are80%. Five explicit GOMAXPROCS=1 overrides and non-Go exceptions are retained;
there is no inferred CPU-derived runtime setting. Parent/child quota quantization,
ordered transient/restartable init stages and Pod overhead remain in the native
ledger. Allocation weights derive from prior Medium allocated requests, **not new
measured unconstrained demand** or proof that reduced CPO/KAS/etcd/memory is safe
for fresh startup. The original floors were retained, including scheduler100m,
packageserver150m and CPO144Mi.

## Lifecycle And Inventory

Size100 was applied **in place to the existing HCP**, not freshly provisioned.
The original45-minute observer never established its five-minute healthy window
because router placement, changed catalogs and immutable historical Jobs blocked
the exact profile. Its541 samples and stopped outcome remain unchanged.

Recovery used a parent-authorized temporary cordon and one UID/RV deletion of the
unready router; replacement became Ready on the newer zone3 host and the old node
was restored at13:25:58UTC. No NIC/finalizer cleanup was claimed and the old Swift
leak was not declared fixed. A new image-bound native existing-target observation
then passed its five-minute healthy window, starting13:26:51UTC. This is still
**not fresh clean startup**.

The profile observes **50 declarations =47 base components +3 exact-name/UID/
owner-CronJob-UID historical completed Jobs**, with66 Ready serving Pods and12
endpoints. Those immutable Jobs retain old8m/112Mi requests,8m/124Mi limits and
their original Go target; they were Complete/Active0. The future CronJob template
uses Size100's4m/71Mi policy. Historical Jobs are neither falsely reallocated nor
charged as running, and no generic ignore-Job waiver was used.

The dashboard's **43 steady workload components /144 containers** are a different
scope: nonzero Deployments/StatefulSets and restartable init sidecars, excluding
completed Jobs, transient init and kube-state-metrics instrumentation. Their
references sum to4741m/18543Mi, not scheduler memory18577Mi or charged20285Mi.
All43 CPU/memory distributions are available in the three full screens, with
aligned whole-HCP CPU coverage8/12; memory10/12 at5QPS and11/12 at10/15QPS.
At20QPS40 component CPU distributions are available, whole-HCP CPU unavailable,
and aligned memory3/4. Missing identity/freshness opportunities remain excluded,
never zero; RSS/working set lack independent timestamp companions. Quantiles sum
simultaneous complete samples first, not component percentiles.

## Environment And Limits

Execution used **six Standard_D4s_v3 management workers2/2/2**, plus system/infra,
and **sixteen Standard_D8s_v5 guest workers (128vCPU)** retained for only300 workload
Pods. This is not a packed-density comparison. Guest VMs/disks, observers,
management system/infra, provider services, storage/network, fragmentation, surge
and failure headroom are outside the HCP economic claim. No guest USD bill or
D4s_v3-to-D16s_v6 throughput equivalence is implied.

Exact OpenShift4.21.32 was retained. Current registry/extract catalog digests:

| Catalog | SHA-256 |
| --- | --- |
| Certified | `3cb08f70f6fce1863b1b0e26bb335a52343c465f7b980180c6f9aebcc69947e2` |
| Community | `8778b9791b489bf7c3fb62ad27234043870107d2381343a9745b619a78d903af` |
| Red Hat | `7c3056c7a8d0847d54551f8ed8536c1c81d851acff03848ba56b1ccc17361571` |

Natural catalog drift, in-place history, guest/management topology and capture
epochs prevent perfect comparability with Small or Medium. Source predicates were
not weakened to hide drift. Native V9 source and historical run bytes are untouched.

At the14:22:41 midpoint handoff,300 workload Pods were Ready, canary Pods zero,
16guest nodes retained, claims free and owned processes/capture stopped with PVC
retained. This offline publication made no live check or mutation. No further
population/rate/hour result is authorized or inferred by the short bracket.

## Evidence

Current private reproduction manifest: `_artifacts/dashboard-size100-v2/export.json`;
exact source authorities/hashes: `source-review.json`. Native specs/outcomes,
results, raw request events, full inventories, recorders, raw CPU/memory matrices
and timestamp companions are pinned. Original budget seal, apply/recovery/midpoint
handoffs and exact historical Job bindings support metadata. No credential content,
private IDs, raw labels or payloads enter the portable dataset.

The original `_artifacts/dashboard-size100/export.json`, published snapshots and
four measured run objects remain unchanged. New allowlists and lifecycle/catalog
supplements support only the two appended runs. No historical card is rewritten
in its private snapshot. The existing frozen offline exporter/renderer is reused. Rendering requires only
portable JSON; re-export requires private evidence, not merely citation hashes.
See [Dashboard](../DASHBOARD.md) for commands. Hashes identify reviewed bytes, not
an off-machine backup or full performance qualification.
