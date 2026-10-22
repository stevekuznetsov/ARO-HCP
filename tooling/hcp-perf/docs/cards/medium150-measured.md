# Medium150 Measured Envelope

September 20 2400-Pod/40-QPS hour update. **The new full hour failed multiple
workload requirements.** Revised Medium previously completed the
1200-Pod / 30-QPS sustained workload hour, met-not-qualified.** Not a supported
size, production SLO, fresh clean startup or capacity ceiling.
Open the separate [Medium dashboard](../dashboard/medium.html), backed by
[portable measured JSON](../dashboard/medium.json). Small and its historical cards
are unchanged. Historical Medium v1 and the earlier preparation record describe
different stages/cohorts, not these results.

## 2400-Pod 40-QPS Hour

The new **Medium 2400-Pod Hour** pane adds the V8c run without changing any of the
prior17 measured run objects. It is separate from the V7 short brackets because
management workers increased from **five D4s_v3 (2/2/1) to six (2/2/2)** before this
hour. Guest workers remained **16 D8s_v5**, with **480 apps / 2400 resident Pods**.
Scheduler100m/package153m, charged7440m/30491Mi and modeled
**$0.374996124267578125/HCP-hour** were unchanged. Added management VM cost and
guest VM costs are separate from this modeled resource price; no live bill is claimed.

| Hour result | Raw/exported value |
| --- | ---: |
| Offered / dispatched / completed logical requests | 144000 / 144000 / 144000 |
| Successful bodies | 136112 |
| HTTP429 / degraded union | 7888 / 5.4777778% |
| API timeouts / canceled / generator loss | 0 / 0 / 0 |
| Other HTTP / transport / body-limit errors | 0 / 0 / 0 |
| Successful body QPS, including drain | 37.80705401878536 |
| Raw nearest-rank successful p95 | 1.170067598 seconds |
| Turnovers completed / scheduled | 474 / 480 |
| Turnovers failed / notStarted / backlog / outstanding | 3 / 3 / 3 / 0 |

API offering ran **20:21:43.774949797-21:21:43.775245144 UTC**, exactly
3600.000295347 seconds. Final body completion was21:21:43.949666726; it is not the
offer-stop timestamp. The600-second no-new-load recovery elapsed and the outcome
finished21:31:46.571706175. **Native executionStatus=failed, state=failed,
fullWindow=true, workload=violated.** This is a failed full hour, not an interrupted
60-second screen or generator-inconclusive case. The raw p95 is not the rounded
native histogram1.171s.144434 transport attempts are distinct from144000 logical
requests. There were no replacement offers or early checkpoints.

The native outcome retains three distinct workload violations:

- **API request quality exceeded:**7888 serverHTTP429 responses exceed the unchanged
  inclusive5% allowance. Zero API timeouts/cancellations/generator loss do not make
  this acceptable.
- **Turnover incomplete:**indices306,329,419 failed with `client rate limiter Wait
  returned an error: context deadline exceeded`; indices477-479 were not started.
  These client-side turnover deadlines are not server429s and are not API-generator
  timeouts. Only474 of480 rollouts completed.
- **Canary/recovery evidence failed:**native aggregate `canaryFailures=1`,
  `canaryRecoveries=0`; final recovery probe `not_evaluated` and recovery
  `recovered=false`. The exporter separately observes91 completed measured canary
  cycles with zero measured-cycle hard failures. That narrower cohort does not
  erase the aggregate/final recovery failure. Independent final drain to zero
  canary Pods is not proof of successful recovery quality.

There were840 critical evaluations with zero unknown or observed outage, KAS/etcd
3/3 and authenticated guest livez2/2;420 comprehensive checks had zero unhealthy
samples/new restarts/OOMs, but **55 probe increments**. OAS/packageserver endpoints
remained3/3 in observations. Health remains **degraded**, capture **incomplete**.
Capture history itself had zero warnings/incomplete=false, not a lossless export
or recorder claim. Native CPU/memory matrices and timestamp companions exist but
contain gaps; declared historical inventory cannot establish current runtime
attribution, so all43 component CPU/memory and whole-HCP distributions are null,
not idle/zero. RSS/working-set timestamp companions remain absent.

Before dispatch, parent-authorized router repair temporarily cordoned the old
zone3 node, deleted only the stuck router byUID/RV, and placed its replacement on
the added node. The old node was restored at20:09:01UTC; all three routers were
Ready for measurement. **The old stale router MTPNC leak remained unresolved and
Terminating**. This was not NIC deallocation/finalizer cleanup or a fix of that leak.
The capture node roles expanded5->6 hosts and52 targets passed the pilot. Router
repair and added management capacity must remain visible in comparisons.

V8c's explicit screenedPopulation pinned the latest q45's own original
spec/results/outcome/inventory. Native semantic/topology/UID-content proof passed,
with no reseed, historical receipt rewrite or ancestor bypass. Release4.21.32 and
the current image/env/resource profile were unchanged during measurement; no
fresh clean startup or topology-controlled comparison is claimed.

Finalized failure was verified read-only after stop/reconcile; no load replay.
At the terminal handoff2400 workload Pods and16 guest nodes were Ready, canary Pods
zero, owned writers stopped and capture stopped with its PVC retained. Shared V8c
controller/auth/RP services remained running. These are handoff observations, not
fresh live checks by this publication. The earlier180s brackets and earlier accepted
1200-Pod V6 hour remain separate evidence; this40-QPS hour does not establish an
exact sustainable ceiling or authorize another treatment.

## Quick Capacity Campaign

September 20 V7 strict screens retain scheduler100m/packageserver153m, declared
CPU7220m and charged **7440m / 30491Mi**, modeled **$0.374996124267578125/HCP-hour**.
The new **Medium Quick Capacity** pane separates this runtime/catalog cohort from
the prior 12 measured run objects, which remain exactly unchanged.

| Pods / Apps / Nodes | Offered QPS | Actual Offer Seconds | Decision Cohort HTTP429 | Outcome |
| --- | ---: | ---: | ---: | --- |
| 1200 / 240 / 8 | 60 | 60.100349 | 309 / 3600 = 8.5833% | Screened-out at60s |
| 1200 / 240 / 8 | 45 | 180.000750 | 169 / 8100 = 2.0864% | Passed180s |
| 2400 / 480 / 16 | 30 | 180.000302 | 96 / 5400 = 1.7778% | Passed180s |
| 2400 / 480 / 16 | 60 | 60.734904 | 234 / 3600 = 6.5000% | Screened-out at60s |
| 2400 / 480 / 16 | 45 | 60.331726 | 144 / 2700 = 5.3333% | Screened-out at60s |

These are **server HTTP429 request-quality failures**, not unusable or
generator-inconclusive cases. All decision cohorts were fully dispatched/completed,
with zero timeouts or generator loss. Native execution `completed` means finalized;
failed points retain state failed, workload/capacity `screened-out`, request quality
exceeded and `fullWindow=false`. The intended180s window was not completed.
The chart uses red shortened-window crosses for these failed request gates, versus
full-window pass markers. The exact checkpoint fraction is in each case's summary;
the displayed final request counters and latency distributions use the raw whole run.

Cutoff and drain add requests after the checkpoint, including cancellations:

| Pods / QPS | Final Successful / Completed | Final HTTP429 / Canceled | Whole-Run Successful QPS | Raw Successful p95 |
| --- | ---: | ---: | ---: | ---: |
| 1200 / 60 | 3293 / 3606 | 309 / 4 | 54.786056582 | 683.304540ms |
| 1200 / 45 | 7931 / 8100 | 169 / 0 | 44.051182535 | 551.505934ms |
| 2400 / 30 | 5304 / 5400 | 96 / 0 | 29.456052046 | 780.126198ms |
| 2400 / 60 | 3371 / 3644 | 250 / 23 | 55.492336419 | 600.159946ms |
| 2400 / 45 | 2558 / 2714 | 144 / 12 | 42.390517492 | 764.002847ms |

Offered/dispatched/completed totals coincide in each final cohort. Successful QPS
includes drain; raw nearest-rank p95 excludes rejected/canceled bodies. Failed-point
whole-run useful throughput is **not** sustained accepted capacity. In particular,
250/3644 is not the 234/3600 rejection cohort used to reject2400/60.

The bounded short-test brackets are **45 passing / 60 failing at1200 Pods**, and
**30 passing / 45 failing at2400 Pods**. They are not exact maxima, universal API
ceilings or HCP crash limits. No sustained hour ran at these new points; the prior
1200/30 V6 hour remains a different case. No4800-Pod point ran.

Policy: strict, inclusive5% logical429-or-timeout allowance, declared60/120s
checkpoints/minimum100completed, intended180s plus60s recovery, ten-second body
deadline,128in-flight, seed42, first3fixture namespaces, Deployment GET : Pod LIST :
ReplicaSet LIST2:1:1 and LISTlimit100. V7 pacing reported zero missedDeadline,
slotsFull and canceledOffers. Turnover grows with apps: accepted screens completed
12/12 rollouts at1200 and24/24 at2400; early screens completed2/12,6/24 and5/24
in execution order. Do not treat unrun scheduled work as accepted steady operation.

After native UID cleanup of1200, the normal guarded node-pool update scaled
**8 to16 Standard_D8s_v5 guest nodes**; all16 Ready identities were bound. Fresh
**480-app/2400-Pod** seeding at1QPS/burst1 fully converged before measurement
(3000s seed/3600s setup bounds). Sixteen guest VMs mean **128vCPUs**, +8VMs/+64vCPUs
and disks versus the prior level, approximately double same-SKU guest compute
spend. No measured guest USD rate is claimed; guest cost is outside the unchanged
HCP price. Management topology and exact4.21.32 remain unchanged.

Community registry/extract image naturally changed to
`6feb26008aa39e6c5b48f1b68f3a8bcb60e47d5a524c7e86655186eaf70e2382`, from the prior
`c2975e6c9ae0f6705d3f6e098399c81accb6b1115a059bd125e9e661bb432f33`, with explicit
native V7 bindings. No claim of a perfectly image-controlled comparison or fresh
clean cluster startup. First-three-namespace sampling is not uniform full-population
sampling; objects, turnover, guest capacity and chronology also change.

All five native health states are degraded and capture incomplete. Probe increments
were **6/26/22/1/3**; no observed unhealthy full-profile samples, restarts, OOMs,
canary failures or hard guards. Strict screens omit the earlier hour's critical-role
loop. Native raw metrics are retained, but this export cannot establish current
runtime identity from its declared full-inventory/recorder pairing: all43 component
CPU/memory distributions and whole-HCP totals are unavailable, **not zero**. The
inventory explicitly combines captured prior objects with the current community
Deployment; it does not fabricate replacement Pod IDs. Coarse60s KAS scrape can
miss transient queues/seats, so absent gauges do not prove causal saturation.

At17:30:46 terminal handoff,2400 workload Pods were Ready, canary Pods zero,
16guest nodes retained, capture stopped/PVC retained and owned writers stopped.
This publication performed no live checks/writes or network actions. Future reuse
would require the latest failed-chain native contract, not an earlier passing
population receipt. No further progression is authorized by this report.

## Revised Allocation

The dashboard separates **Medium Revised** from **Medium Original**. The original
eight measured run objects, citations and native outcomes remain unchanged. Do
not retroactively apply the new resource profile to historical runs.

| Accounting / main container | Original | Revised |
| --- | ---: | ---: |
| Scheduler request = limit, two replicas | 8m | 100m |
| Scheduler enforced quota | 10m | 100m |
| Packageserver request = limit, three replicas | 213m | 153m |
| Packageserver child / parent quota | 220m / 270m | 160m / 210m |
| Declared steady and scheduler CPU | 7216m | 7220m |
| Economic CPU / memory charge | 7440m / 30491Mi | unchanged |
| Steady / scheduler memory requests | 27808Mi / 27860Mi | unchanged |
| Modeled HCP cost / hour | $0.374996124267578125 | unchanged |

The quota-aware +180m/-180m replicated transfer is budget-neutral despite a 4m
declared CPU increase. Packageserver is **153m, not its 150m floor**; unchanged
helpers and parent quota constrain child totals. Memory, Go targets and runtime
expectations are unchanged; packageserver runtime GOMAXPROCS remains unknown.
The $0.375 HCP target excludes guest VMs/disks, observers and provider overhead.
Each revised run cites the revised ledger, native spec and post-rollout inventory;
its chart references come from that profile. Original ledger references remain.

After existing-cluster policy rollout, the fresh 240-app / 1200-Pod population
seeded at 1 QPS / burst 1 in **994.161 seconds** (11:57:02.888-12:13:37.049 UTC),
then converged and completed baselines. This is not fresh clean cluster startup.
The V5 hour reused its screen population. V6 reused the completed failed-assessment
V5 hour, then its own completed screen, with native UID/content/outcome verification,
no reseed or ancestry bypass.

All four cases used **240 apps / 1200 resident Pods / eight guest nodes**, 30 QPS,
first-three-of-twelve-namespace 2:1:1 query sampling, ten-second deadline and 128
maximum in-flight. Turnover is 240/hour, not the original 60/hour.

| Pacing / case | Successful / Dispatched / Offered | Successful QPS | HTTP 429 | Timeouts | Generator-Limited | Raw Successful p95 | Rollouts |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| V5 five-minute screen | 8989 / 9000 / 9000 | 29.958602534 | 11 | 0 | 0 | 275.116668 ms | 20 / 20 |
| V5 full hour | 107102 / 107296 / 108000 | 29.749594466 | 194 | 0 | 704 | 300.230748 ms | 240 / 240 |
| V6 five-minute screen | 8984 / 9000 / 9000 | 29.941588342 | 16 | 0 | 0 | 366.132458 ms | 20 / 20 |
| V6 full hour | 107782 / 108000 / 108000 | 29.938985387 | 218 | 0 | 0 | 332.979120 ms | 240 / 240 |

Percentiles are exported nearest-rank raw successful-body values, not rounded
native histograms. Successful QPS includes drain. Other HTTP, transport, canceled
and body-limit errors are zero in these cases.

**V5 hour: execution completed, assessment failed/inconclusive.** Full window and
240 rollouts completed, but 704 offers were not dispatched. Native state failed,
workload violated, capacity inconclusive. Its 0.1808% degraded union does not make
it acceptable. It is not an interrupted hour or server capacity ceiling; V6 does
not rescore it.

**V6 hour: execution completed, request gate passed, workload met-not-qualified.**
The 14:13:37-15:13:37 UTC API window offered, dispatched and completed all 108000
logical requests. The 218 rejections (0.201852%) fit the unchanged 5% allowance,
with zero timeouts/generator loss, 240/240 rollouts and 102 measured canaries with
zero hard failures. Recovery/finalization ended 15:23:47.658783091 UTC. V6
absolute-deadline pacing recorded zero missed deadlines, full-slot losses and
canceled offers; 108081 transport attempts are not 108000 logical requests.
This result is **not UNUSABLE**.

V6 hour retained 842 critical evaluations, zero unknown, and 422 profile checks
with zero unhealthy samples/restarts/OOMs, but **29 probe increments**, health
**degraded**, capture **incomplete**. Zero capture-history warnings do not erase
native series gaps or polling limitations. V6 screen health is **observed-healthy**,
capture incomplete with eleven final capture-history query failures. V5 screen/hour
health is degraded and capture incomplete. Request acceptance is not blanket
health/capture qualification.

Both full hours have native CPU/memory response files and timestamp companions:
partial, not absent. Aligned whole-HCP CPU/memory coverage is **236/240 and
239/240** for each hour, versus 16/20 and 19/20 for each screen. RSS/working set
lack independent timestamp companions. No gaps were zero-filled. The original
interrupted hour's absent native resource responses remain absent.

Community catalog naturally drifted to SHA-256
`c2975e6c9ae0f6705d3f6e098399c81accb6b1115a059bd125e9e661bb432f33`.
Image-only rebinding retained interrupted observation evidence. Exact 4.21.32 and
node topology remained, but this is not a perfectly image-controlled causal
comparison. Donor throttling/headroom is not proof of unconstrained demand or
startup/refresh safety.

At the V6 handoff, 1200 application Pods were Ready, canary Pods zero, capture
stopped/PVC retained and owned writers/generators stopped. Eight guest nodes and
the revised allocation were retained; this report made no live check/change.
The requested sustained workload goal is observed for this hour. **No 2400-Pod
progression was started or inferred.** Probe warnings, capture gaps, fresh startup,
contention, maintenance and failure-headroom qualification remain open.

## Original Measured Points

The original six screens offered API load for 300 seconds at **60 applications / 300
steady Pods on three guest workers**. The first seeded a fresh complete workload
population after separate failed-attempt cleanup; the sweep reused verified
population identities without reseeding. That is not fresh cluster startup.

| Offered QPS | Successful / Dispatched | Successful QPS | HTTP 429 | Timeouts | Raw Successful p95 | Request Gate |
| ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 5 | 1500 / 1500 | 4.9987 | 0 | 0 | 164.150 ms | Passed |
| 7 | 2100 / 2100 | 6.9982 | 0 | 0 | 170.315 ms | Passed |
| 10 | 3000 / 3000 | 9.9978 | 0 | 0 | 182.161 ms | Passed |
| 15 | 4500 / 4500 | 14.9967 | 0 | 0 | 191.136 ms | Passed |
| 20 | 5997 / 6000 | 19.9859 | 3 | 0 | 213.579 ms | Passed, 0.05% degraded |
| 30 | 8996 / 9000 | 29.9332 | 4 | 0 | 197.112 ms | Passed, 0.0444% degraded |

Successful QPS divides completed successful response bodies by actual elapsed time
including drain. Percentiles above are nearest-rank calculations over raw successful
request events, not the rounded histogram percentiles in native summaries/handoffs
(165/171/183/192/214/198 ms). Rejected, canceled and timed-out requests are not
zero latency. The logical 429-or-timeout union, not their sum, is compared with
the explicit 5% allowance.

Each screen completed five of five scheduled rollouts and nine measured canaries,
with zero generator-limited offers, observed restarts, OOMs or hard canary failures.
Probe-failure increments over the full actions were **6/1/0/7/4/6**, respectively.
Only 10 QPS was `observed-healthy` / `observed-not-lossless`; other screens were
health `degraded` / capture `incomplete`. Request acceptance does not rewrite those
axes. Native request quality at 20/30 QPS remains `degraded`, with capacity status
`acceptable`; dashboard `passed` means the bounded request gate, not zero errors.

For the original allocation, the highest tested acceptable short rate was **30 offered QPS for five minutes
only**, now at both 300 and 600 Pods. The original 300-Pod screen achieved 29.9332
successful QPS; the new 600-Pod screen achieved 29.98095. No failing API
request-quality upper bound was found.
There is no maximum-capacity claim and no empty/unloaded baseline in this dataset.

## Population Doubling

The later V5 screen measured **120 applications / 600 Pods / six actual guest
nodes** at 30 offered QPS for 300.000467 seconds, starting at 21:12:58.519 UTC
(API admission start, distinct from the earlier overall measurement phase).
All applications had converged before measurement. Native execution completed,
`fullWindow=true`, workload `met-not-qualified`, request quality `degraded` with
capacity `acceptable`. Health remains **degraded**, capture **incomplete**.

| Population | Successful / Dispatched | Successful QPS | HTTP 429 | Timeouts | Raw Successful p95 | Rollouts |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 120 apps / 600 Pods / 6 nodes | 8996 / 9000 | 29.98095 | 4 | 0 | 202.280 ms | 10 / 10 |

The saved histogram p95 is **203 ms**; the dashboard computes **202.280204 ms**
nearest-rank from raw successful request events. The degraded union is 0.044444%,
below 5%; no generator-limited offers, other errors, hard canary failures, observed
restarts or OOMs. Nine measured canaries completed. There were 79 comprehensive
safety samples, zero unhealthy samples, and nine probe-failure increments over the
full action. These are not continuous-health or clean-capture qualifications.

This treatment doubles **application/object count and turnover, not only Pods**.
One turnover per application per hour means 120 rollouts/hour and ten scheduled
rollouts in five minutes, versus 60/hour and five for the original population.
The six populated namespaces do not imply six-namespace API sampling: the API
still samples only the **first three namespaces**, with three fixed Deployment
GET targets plus Pod/ReplicaSet LISTs. Configured weights remain 2:1:1, LIST limit
100, maxInFlight 128 and ten-second body deadline. Actual dispatched operations
were **4469 GET / 2274 Pod LIST / 2257 ReplicaSet LIST**; successes were
4468/2272/2256. This is not a doubled API query population or isolated Pods-only
causal comparison. Guest capacity, object counts, turnover and chronology changed.

The 600-Pod fixture was then removed through native UID-guarded cleanup before
the eight-node seed attempt. Its measured population is not the later retained
partial population. The exporter accepts V5's `expectedGuestNodes` source field
without changing operational contracts; public per-run counts are reviewed against
the six pinned identities, not inferred from terminal node count.

## Seed-Only Stop

The next attempt targeted **240 applications / 1200 Pods on eight guest nodes**.
Seeding began at 21:33:41.475 UTC and was canceled at 21:40:48.569 UTC by sustained
`kube-scheduler-resources` capture loss, with an 80-second bound. The 900-second
seed timeout was not reached. There was **no measured API window, warmup, turnover
or canary phase**, and no native results file or completed population receipt.
It is deliberately diagnostic metadata, not a plotted 1200-Pod/30-QPS point or
fabricated zero-valued API distribution. Native execution failed, workload was
`not-evaluated`, `fullWindow=false`; it was not accepted or rescored.

At 21:43:49 the partial snapshot contained 214 Deployments / 1070 Pods, **964 Ready
and 106 Pending**. The later 21:45:05 terminal snapshot had **214 Deployments /
1070 Ready Pods**, after load stopped. That convergence is not a completed
1200-Pod seed or measured residency under API load.

Observed failed scheduler scrape source timestamps preceded cancellation, including
21:38:55.657, 21:39:55.657 and 21:40:25.657 UTC on the older zone-3 host. At
21:40:30, two-minute cAdvisor rates showed about **0.009975 CPU cores / 99.238%
throttled periods** for that scheduler replica, versus 0.00734 / 64.405% for the
other replica. The configured scheduler CPU limit remained **8m**, memory request
138Mi / limit 152Mi. Quota quantization and sampling can yield observed CPU above
the requested milliCPU without an allocation change. Logs retained slow FIFO
handlers; liveness deadline events also occurred. Both scheduler Pods remained
Running/Ready with zero restarts after abort; 44 comprehensive samples had zero
unhealthy samples, zero new restarts/OOMs and 11 probe warnings.

This **supports scheduler/metrics-serving pressure during population creation**,
not a proven root cause or capacity ceiling. The exact failed-scrape HTTP error was
not retained; later targets had recovered. Do not infer TLS versus body timeout,
or a server 30-QPS ceiling. The guest slot gate passed, no FailedScheduling events
were found in the queried snapshot, and no VPN drop was found in the inspected
network interval. Those scoped negatives are not universal proof. No budget,
probe, timing waiver or retry was used to bypass the stop. No 2400-Pod/12-node
point ran. Retained partial state is not a completed reusable population.

## Slower Seed Retry

The user-approved fresh retry targeted the same **240 applications / 1200 Pods /
eight guest nodes**, after native UID-bound cleanup removed the previous partial
214-app population. Seed rate changed from 2 QPS / burst 2 to **1 QPS / burst 1**,
with the same 30-second request deadline; seed timeout increased from 900 to
**1800 seconds**, setup allowance to **3000 seconds**. HCP resources, images,
probes, guest nodes and capture policy remained unchanged. [Slow-seed citations
in the dashboard's configuration section]

Seeding ran September 19 **00:43:26.407-00:57:01.397 UTC**, about **815 seconds**,
before the 1800-second timeout. The same `kube-scheduler-resources` endpoint on the
older zone-3 scheduler reached the unchanged **80-second sustained-loss bound**;
the initiating guard fired at **00:57:01.364428168 UTC**. Execution failed in seed,
workload `not-evaluated`, `fullWindow=false`. No warmup, planned 30-QPS API window,
turnover or canary measurement started. There is no new API distribution or chart
point. All eight existing measured run objects remain unchanged.

Unlike the earlier attempt, contemporaneous Prometheus target polls captured
the actual GET error **`context deadline exceeded`** for `/metrics/resources`:

| Target Poll UTC | State | Error |
| --- | --- | --- |
| 00:56:32.109635985 | down | context deadline exceeded |
| 00:56:47.194100884 | down | context deadline exceeded |
| 00:57:02.247847937 | down | context deadline exceeded |
| 00:57:17.334133675 | down | context deadline exceeded |

These are target-observation times, not four independent scrape timestamps. Native
capture retained a failed source witness at 00:56:38.265 UTC with `QueryFailed=false`.
Scrape interval/timeout remained 30s/20s; subsequent recovery does not erase the
failure. The generic deadline error does not identify TLS versus response-header
or body timeout.

At 00:56:45 UTC, two-minute cAdvisor rates reported about **0.009856 CPU cores /
98.803% throttled periods** for the affected replica, versus 0.005863 cores /
60.448% for the other. **Throttled-period fraction is not percentage of time
blocked.** The configured CPU limit remained **8m**; the saved cAdvisor quota and
period were **1000us / 100000us**, equivalent to an effective **10m** quota. This
is quota quantization, not a resource increase. Native comprehensive observation
retained 83 samples, zero observed unhealthy samples/new restarts/OOMs and 17 probe
increments; capture retained three warnings, zero recoveries and incomplete status.

The final post-abort snapshot contained **204 Deployments / 1020 Ready Pods**,
not a completed 1200-Pod seed. The earlier 2-QPS attempt ended at 214/1070 and this
1-QPS attempt at 204/1020. These differing observations do **not** establish a hard
population limit or precise capacity bracket. Slower seeding alone did not avoid
the guard; repeated evidence supports scheduler/metrics-serving pressure during
creation, not proven root cause, achieved 1200 capacity or a 30-QPS API ceiling.
Eight D8s_v5 guest nodes and the HCP economic charge remain unchanged; guest costs
remain excluded. At the 01:04 UTC handoff the new partial population was retained,
with no generators or owned capture writers. This offline update made no live
state check or operational change.

## Interrupted Hour

The selected 20-QPS diagnostic hour planned 3600 seconds plus 600 seconds recovery.
It measured only **1010.989 seconds** before the critical evidence watchdog stopped
the run: execution `failed`, `fullWindow=false`, workload `violated`, capacity
**UNUSABLE**. The original pane selects this failed attempt by default; the revised
pane defaults to the V6 accepted workload hour.

- 20,219 offers; 20,146 dispatched; 19,910 successful bodies; 109 timeouts;
  127 cancellations; 73 generator-limited offers; zero HTTP 429.
- Partial successful throughput 19.6933 QPS; raw successful p95 207.720 ms.
- 17 of 60 planned rollouts completed; 29 measured canaries, one hard failure.
- The timeout union alone was 0.5411%, below 5%, but unfinished work/window,
  cancellations, missed offers and canary failure make the hour unusable.

Last successful critical evaluation was 16:30:20.677 UTC. Host NetworkManager
recorded tunnel removal at 16:30:21.156 UTC, followed by synchronized guest and
management access failures; two critical cycles were unknown before the watchdog
fired at 16:30:36.293 UTC. Regular local disk checks continued without a storage
guard trip. **Observer/VPN transport loss is strongly supported, not definitive
cause attribution.** Unknown evaluations are not proven quorum loss. Later read-only
observations found healthy endpoints/nodes and unchanged KAS restart counts; they
do not fill the missing interval or prove uninterrupted server health. Who or what
removed the tunnel remains unresolved. This is neither an accepted hour nor a
server request-quality failure/capacity ceiling.

Native hour range queries failed without response files. Component resource
references remain, but CPU/memory distributions are null with excluded opportunities,
not fabricated empty matrices or zero consumption. Independent raw streams remain
private; they are not silently merged into the native result or claimed lossless.

## Allocation And Topology

These are the **original allocation** values; the revised CPU difference is in
the table above. Memory and economic charge remain common to both revisions.

| Accounting | Original Medium150 |
| --- | ---: |
| Hosting fee denominator | $0.25 / HCP-hour |
| 150% target | $0.375 / HCP-hour |
| Modeled resource-equivalent cost | $0.374996124267578 / HCP-hour |
| Charged resources | 7440m / 30491Mi |
| Steady requests | 7216m / 27808Mi |
| Scheduler-effective requests | 7216m / 27860Mi |
| Fixed downstream, included once | 470m / 2090Mi |

The retained Linux on-demand `Standard_D16s_v6` raw-capacity model uses $0.806/hour
in `westus3`, retrieved September 11. It prices the maximum CPU/memory share, not
their sum, and is not an actual cloud bill or measured packing on that SKU.
Charging includes startup treatment; scheduler/init accounting differs from steady
usage. The dashboard's requested-memory field is **steady**, not scheduler-effective.
Kube-state-metrics (10m/64Mi), guest VMs, observers, management system/infra,
service overhead, storage, networking, fragmentation and HA/failure headroom are
excluded, not free. Controlled CPU limits equal requests; memory limits are rounded
110%, Go soft targets 80%, with five retained GOMAXPROCS=1 overrides. Downstream
limits and partial Go targets remain distinct; there is no inferred whole-HCP cap.

Execution used **five Standard_D4s_v3 management workers**, distributed **2/2/1**
across three zones, plus unchanged system and two infrastructure nodes. Guest
workers were **three Standard_D8s_v5 in one zone** for the original runs, **six**
for the 600-Pod screen, and **eight** for the failed 1200-Pod seed. Eight remained
at the 21:46 UTC operational handoff; this offline update did not recheck live
state or scale down. That is 64 guest vCPUs versus the original 24: **five more
VMs / 40 more vCPUs**, 2.667 times the VM and 64-GiB Premium_LRS OS-disk count.
Guest cost is excluded from the unchanged **$0.375/HCP-hour** target; no current
guest retail dollar price was measured or invented. Placement remained concentrated
on older management hosts despite spare hosts; no rebalance occurred. Feasible
placement is not realized uniform rollout headroom or zone-failure tolerance.

The measured release was exact **OpenShift 4.21.32**, release SHA-256
`afc68ed6ce1af80d6e43014d16ad7b3f2067be6bebd970aa556c6a445290c8ee`.
Current registry and extract catalog digests were:

| Catalog | SHA-256 |
| --- | --- |
| Certified | `e3bdbafac1fdaef7cf6b6f3beceadec8ae023d176909c98cbc05dd23a1a9fe77` |
| Community | `214cae0612470d6d5bbdf32ff74a68dbfda507f1bd8a1232305e947572b6e31c` |
| Red Hat | `7fb82b8cc2ff0a38a32841a3838cbc4d16c383d999acb8c9ce6aeb52ca6aa52d` |

These differ from Small's cohort. Catalogs, lifecycle history, actual placement and
capture epochs confound a purely resource-only comparison. API settings retain
Deployment GET : Pod LIST : ReplicaSet LIST weights 2:1:1, LIST limit 100,
128 maximum in-flight, ten-second body deadline and no workload retries; seeding
uses 2 QPS / burst 2. Application turnover is 60/hour in the original population
and 120/hour in the 600-Pod screen.

## Startup And Coverage

Original startup observation was interrupted over a missing router Swift-resource
expectation. A corrected immutable expectation then passed **existing-target
continuation**, not fresh startup. Original probe kills/restarts remain evidence;
there is no clean startup claim. Guest console remained unavailable, so neither
RP success nor these screens establishes full guest health.

The original six screens each have 43 component CPU/memory distributions, covering 144 steady
containers. Whole-HCP CPU covers 16/20 aligned grid opportunities; memory covers
19/20. Values are summed simultaneously before quantiles, never summed component
p95s. Native sidecars count; completed Jobs, non-restartable init containers,
zero-replica workloads and instrumentation do not. Missing lifecycle identity or
freshness remains missing. RSS/working set have no independent timestamp companions
in this native export and are unavailable. These are limited observations, not
startup peaks, safe usage ceilings or lossless capture.

The 600-Pod screen also has 43 component distributions / 144 steady containers,
with whole-HCP CPU **15/20** and memory **18/20** aligned opportunities. Its
declared inventories include a later full Pod/Deployment snapshot; exact runtime
and recorder matching still excludes unmatched earlier identities. No later
readiness snapshot substitutes for measured source freshness.

## Evidence

V4 publication adds the scheduler100 V5/V6 screen/hour cases in a separate pane,
defaulting to the V6 hour. `source-review.json` pins new allowlist/seal sources and
the revised ledger. The two hour Pod recorders exceed the 128MiB per-file bound;
lossless NDJSON chunks are explicitly declared, and concatenation SHA-256 equals
the original allowlisted recorder. No source samples are trimmed or fabricated.
The frozen v2 offline exporter is reused; new V6 diagnostic fields require no
parser changes. Rendering uses a new offline binary with a one-selector mobile
note-wrapping fix, separately frozen. Prior card/page/JSON snapshots are retained
privately and no previous manifest references this mutable measured card.

All measured fields come through the existing dashboard exporter from exact
hash-pinned native specs, outcomes/results, raw API request and canary events,
CPU/memory timestamp matrices, full inventories and recorder evidence. The private
persistent reproduction manifest is `_artifacts/dashboard-medium-v6/export.json`;
`source-review.json` distinguishes original allowlist pins, native-manifest child
hashes and explicitly reviewed supplements. The original retry and sweep allowlists
and handoffs are themselves cited. The private startup handoff, ledger and topology
snapshots support metadata, not newly inferred measurements. The doubling allowlist
pins the new canonical run and seed-stop diagnostics; both partial and final
population snapshots are cited. The previous `_artifacts/dashboard-medium/export.json`
and its evidence/reproduction outputs remain frozen. The seven earlier exported
run objects and their native outcomes are unchanged; the new dataset adds one
measured run and diagnostic metadata, not a fabricated ninth measured run.

The v3 slow-seed update changes size-level diagnostic metadata only. It preserves
all eight v2 run objects, including their source lists, exactly. The existing
exporter has only run-level extra evidence, so new diagnostics are separately
hash-verified by `_artifacts/dashboard-medium-v3/diagnostic-sources.json` against
the original slow-seed allowlist or local seal; public labels and exact hashes
appear in size configuration rather than being appended to a historical run.
Sixteen selected sources cover native spec/outcome/guard/seed timing/events,
quota metrics, target polls, CPU/throttled-period rates and final population.
The previous v1/v2 private manifests, outputs and cited handoffs are unchanged.
The frozen v2 offline exporter/renderer is reused without a rebuild or source copy.

Portable source IDs/labels/hashes do not expose private paths, cluster IDs,
credentials, raw labels or payloads. Hashes identify evidence, not a public backup.
See [Dashboard](../DASHBOARD.md) for reproduction, exporter compatibility, privacy
scope and offline tests. No live/cloud/resource changes were needed to publish this
separate dataset.
