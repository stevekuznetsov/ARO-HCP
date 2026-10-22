# Small75 Load Results

The latest [five-QPS observation hour](small75-observe5-hour.md) completed one full
300-Pod hour plus recovery under an explicit diagnostic policy: **2.0111% combined
429-or-timeout met the 5% request threshold, but health assessment FAILED for
degradation**. The 300-Pod fixture is retained. See the
[loaded/unloaded envelope comparison](small75-envelope-results.md) for earlier
screens. Historical strict results below are preserved, not rescored.

## Pinned Three-QPS Hour

**September 17: a full 3-QPS hour and 600-second recovery completed with catalog
pins, but request quality did not pass.** Of 10,800 dispatched/completed requests,
10,740 succeeded, 40 returned HTTP 429, and 20 timed out. Successful-body throughput
was **2.980/s**, p50 **1.407s**, p95 **4.486s**. All 60 rollouts completed with zero
debt. There were 54 measured canaries, four softly degraded, no OOM, and one KCM
restart. Native execution completed; workload assessment remains `FAILED` because
of the timeouts. The 1-QPS hour below remains the clean completed request result.

Pinning used the supported three HostedCluster catalog-image annotations, mirrored
by HO to HCP. CPO selected digest references for both `registry` and
`extract-content` and removed deployment ImageStream triggers. The selected bytes
already matched independent ACR lookups; no CPO image, resource, probe, or HA change
was required. Pins remained intact through the hour; the image guard stayed on.

After native cleanup of the interrupted population, a fresh 2-QPS seed completed
300 Pods and all 60 Services. The hour reused that verified population. At finish,
300 Pods were Ready and the canary drained. External observation streams ended
before the case; native recording and final metrics export remained available,
with partial family coverage explicitly reported rather than called lossless.

Report: `_artifacts/cpo-memory-investigation-20260916/PINNED-HOUR3-RESULT.md`.
Run `cpo144-pinned-hour3-001`, attempt `4ba0a5b61defbc358b983dc666e99586`.
Results SHA256 `ef1269f85e7335e1cd1bfa65afedb66875245a7cf656be061c110bb21896494f`;
outcome SHA256 `0c6541f14071f4133b3c654cf7f89c07197ff3c5964a3bf712fc6e78c47455d2`.

## Three-QPS Confirmation

**September 17, 11:46 UTC: the planned 3-QPS hour was interrupted by catalog image
drift, not an OOM or request-timeout boundary.** The same verified 300-Pod
population was reused without reseeding or competitors. At 11:34:21, all three
catalog images diverged from the frozen profile and the guard stopped load.
CPU, memory, Go settings, and the approved CSC remained unchanged.

The 684.8-second partial window delivered **2,046 successful responses, two 429s,
and six guard-canceled requests out of 2,054 offers**. There were zero timeouts,
transport errors, or generator-limited requests. Successful-response p50/p95 were
**1.664s / 4.114s**. Twelve measured canaries completed without soft degradation;
11 of 60 planned rollouts completed, one was canceled/failed, and 48 were not
started. No new restart or OOM was observed. Earlier transient OAS/packageserver
readiness samples remain recorded.

**Three QPS is still not hour-confirmed.** The passing 1-QPS hour below remains
the completed long-duration result. No new catalog digests were silently accepted
mid-window. Following ten minutes without load, the final snapshot showed 66 Ready
control-plane Pods, 300 Ready app Pods, the same three Ready nodes, and a drained
canary. CPO was Ready with zero restarts; sampled charge peaked at 129.34MiB,
not an instantaneous memory-peak guarantee.

Details: `_artifacts/cpo-memory-investigation-20260916/HOUR3-RESULT.md`.
Run `cpo144-hour3-001`, attempt `9c6fa0ecf52a0cd4cc265e2d2898ea7f` remains
`BLOCKED` with its evidence/claim retained. Results SHA256
`ee9a9fa66ca8356aafa77f529bb5a84b3c1764d0dc2731567c16cc8525218169`;
outcome SHA256 `78cfe08bbb5d73e0ab6d0b85671d96d8519b402bd8e27f40196b4a721513e9d1`.

## Audit-Funded Retest

**September 17: the audit-funded CPO memory treatment completed a one-hour quiet
test at 300 application Pods and 1 offered QPS.** All 3,600 response bodies were
successful, with no shedding/timeouts and successful-response p95 **2.530s**.
All 60 scheduled application rollouts completed, with no debt. No new restart or
OOM was recorded. Of 71 actual canary cycles, one was softly degraded; a transient
packageserver convergence sample and partial capture coverage remain explicit.
This is delivered-work success, not packed/contention or lossless-observation
qualification. The earlier failed hour below remains unchanged.

The exact two-row treatment was CPO **144Mi request / 159Mi limit**, funded by
three KAS audit containers at **39Mi / 43Mi**. CPU remained **3720m**; charged
memory is **15241Mi**, cost **6142123/32768000 USD/hour**, below the 75% ceiling.
Images, probes, GOMAXPROCS and HA settings were not changed. Mutable catalog data
did change naturally and was independently rebound before the retest, so this is
not an image-identical causal comparison. pprof remains unavailable on this image.

| Subsequent Quiet Screen | Successful | Shed 429 | Timeouts | Success p95 |
| --- | ---: | ---: | ---: | ---: |
| 3 QPS, 180s | 540/540 | 0 | 0 | 2.859s |
| 5 QPS, 180s | 896/900 | 4 | 0 | 4.343s |
| 10 QPS, 180s | 1275/1800 | 479 | 46 | 6.800s |

The 10-QPS screen is a failed request-quality boundary; higher rates were not
attempted. Only 1 QPS has a completed hour. The same verified population was reused
across these rate tests. At 02:42 UTC the 300 application Pods, three guest nodes,
and CPO were Ready; CPO had zero restarts and the canary was drained.

Retest evidence: `_artifacts/cpo-memory-investigation-20260916/RETEST-RESULT.md`.
Hour run `cpo144-hour1-001`, attempt `4d404fc6b05024a94df7abdede7ef480`:
`results.json` SHA256 `0909144d94bc7bb0057e93aaf8c9da69e1c3a050c952548396e187d422f106d1`,
`outcome.json` SHA256 `76439f9f2e8defae9e3f545d28fd24f525e4ce6dcaa6a184a7c9cc0c224c9481`.
Files are under the state root defined in the evidence section below.

## Original Campaign

**September 16, 2026: short quiet tests completed, but no one-hour operating
point was established.** A retained 300-Pod population passed a three-minute
1-QPS read screen. The planned hour stopped after about 28 minutes when the
control-plane operator (CPO) was OOMKilled. Heavy host contention had separately
failed with only 15 application Pods. These are different failure dimensions,
not a 15-Pod capacity ceiling or a packed-density qualification. [R1-R6]

## Fixed Configuration

- CAPZ V3 charge: **3720m CPU / 15243MiB, $0.187467316 per HCP-hour**, unchanged.
- Cost uses a raw D16s_v6 resource-equivalent illustration; actual management
  workers are D4-class hosts. This is not demonstrated packing or an Azure bill.
- Guest VMs and observation infrastructure are outside the control-plane model.
- Release remained **4.21.32**; CPO digest `33e5c072...`, OLM/packageserver
  `50b1a860...`, availability-prober `edc5ba19...`. Full pins remain in the frozen
  hour specification. No image, resource, probe, HA, or replica rescue was applied.
- Packageserver retained four workers, 20% jitter, and the restored **12-hour**
  catalog cadence. The separate [refresh test](packageserver-refresh-test.md)
  exercised periodic jitter; hourly application turnover is not catalog refresh.
- Application recipe: five pause Pods per app, each requesting 10m CPU/32Mi,
  with one Deployment, Service, ConfigMap, and synthetic Secret per app. [R1, R7]

## Executed Tests

| Test | Delivered Work | Outcome |
| --- | --- | --- |
| Quiet 15 Pods | Five-minute window, eight measured canaries and one scheduled turnover completed | Recovery canary exceeded a soft expectation; original assessment remains failed, not a crash |
| Contended 15 Pods | Three four-worker CPU competitors; about 107 seconds of measurement | OAS/packageserver readiness loss and OCM probe-triggered restarts; competitors stopped and recovery verified |
| Quiet 60 Pods | Five-minute window plus recovery | Completed |
| Quiet 150 Pods | Fresh 180-second window after capture correction | Completed |
| Quiet 300 Pods | 180-second window plus recovery | Completed; not a maximum or hour qualification |
| Quiet 450-Pod seed | Setup only | Readiness loss and probe-triggered restart; Service POST timeouts; no density measurement |
| Slow 300-Pod seed | All 60 Services and complete app population | Completed at 2 QPS/burst 2 |
| Retained 300 Pods, 5 QPS | 900 dispatched/completed: 857 successful, 38 HTTP 429, five timeouts | Completed execution, failed workload assessment |
| Retained 300 Pods, 1 QPS | 180/180 successful; no shedding or errors | Three-minute screen passed |
| Retained 300 Pods, planned hour at 1 QPS | 1689 offered: 1687 successful, one timeout, one canceled | CPO OOM; incomplete hour |

The 5-QPS successful-body throughput was **4.605/s**, with successful-response
p95 **5.522s**. At 1 QPS it was **0.9977/s**, p95 **1.807s**. Higher rates were
not attempted after the five timeouts. Read mix was Deployment GET : Pod LIST :
ReplicaSet LIST = 2:1:1 across the same three namespaces, with 128 maximum
in-flight requests, a ten-second request deadline, and no generator retries.
These are Kubernetes API reads, not application HTTP or ARM throughput. [R2, R3]

The seed pacing change addressed a separate setup problem: server evidence tied
earlier failures to Service POST timeouts in the ClusterIP allocation path, not
completed read-load capacity. The slow seed kept the **30-second HTTP timeout**;
only pacing changed to 2 QPS/burst 2 and the overall seed bound to 900 seconds.
It completed in about 364 seconds, including convergence. [R1]

## Population And Measurement

Failed partial populations were cleaned through exact owned-object UID guards,
not adopted as successful seeds. The slow seed and subsequent read cases used
one retained population. Each successor verified predecessor outcome/inventory
hashes, object UIDs and desired content, HCP identity, seed policy, RBAC/APF, and
Deployment convergence before minting fresh scoped credentials. No admin fallback
was used for read load. Turnover history accumulated naturally; this was not a
fresh-cluster comparison between rates. [R1, R8-R10]

Soft latency overruns are recorded as degradation in newer runs, separately from
hard deadlines and incomplete work. Historical failed outcomes were not rewritten.
Canaries wait 30 seconds **after each completed cycle**, not fixed 30-second launch
slots. The initial quiet test's eight canaries are actual coverage, not ten nominal
slots. The hour did not run long enough to claim 120 cycles. [R1, R6]

Capture used one existing Prometheus. Scheduler timing was separately corrected
to 30s interval/20s timeout/80s maximum age; KAS remained 60/45/165 and OAS 30/20/80.
Source gaps, partial metric families, and bounded audit coverage remain explicit.
Repeated query evaluations are not independent scrapes; server histograms without
HTTP-code separation do not supply successful-response latency. [R1]

## Failed Hour

Measurement began **18:56:53 UTC**. CPO was **OOMKilled at 19:24:32**, exit 137;
the guard observed the OOM/restart at **19:25:02** and stopped API/turnover load.
The approximately 1689-second offer window was not the planned 3600 seconds.
Of 60 scheduled rollouts, 29 were admitted, 28 completed, one failed, and 31 were
not started. Final outstanding work was zero, but the hour was incomplete. The
partial successful-response p95 was 3.917s, not a one-hour result. [R4, R5]

CPO retained a **105Mi request / 116Mi limit**, 228m CPU, and **84Mi GOMEMLIMIT**.
The last five minutes showed roughly **4.95MiB/s allocation**, **652 GC cycles**,
and **54 HCP status-version conflicts**. Managed Go memory (`Sys - HeapReleased`)
ranged 82.3-95.3MiB. Actual GOMAXPROCS was two. [R11]

The last cAdvisor source sample was **20 seconds before the OOM**, around 96Mi
charged memory; the sampled window peaked around 97Mi. It missed the terminal
spike. Process RSS includes substantial file-backed mappings and is not the same
as cgroup charge. Elevated retained memory and reconcile allocation churn are
supported; a leak, exact allocation stack, or direct 1-QPS causation is not proven.
Post-restart recovery passed five minutes without a resource change. [R11, R12]

Offline CPO options **128/141Mi** or **144/159Mi** were discussed, not applied.
An audit-helper donor of 39Mi request/43Mi limit merits independent ledger/startup
review, not a validated recommendation. Etcd sampled 493.9Mi against a 494Mi limit
and is not a safe-looking donor. No global rescaling or runtime change was made.

## Final Snapshot

At **19:45 UTC**, the read-only snapshot showed 66/66 Ready control-plane Pods,
300/300 Ready application Pods, the same three Ready guest nodes, a zero-replica
canary with no Pods, and no CPU-burner Pods. API and turnover producers had exited.
The live sizing spec exactly matched frozen CAPZ V3, not an offline CPO proposal.
CPO was Ready with its one recorded OOM restart. HCP and population are preserved.
[R7, R12]

`slow75-hour1-001` remains **BLOCKED**, with its failed outcome and durable claim
intact; further claim release requires native verified recovery. No claim change
or cleanup was performed during this final review. All nine RGs were verified at
**19:44:39 UTC**. Retention is scheduled through approximately **22:54 UTC**.
Controller, RP forwarding, and ARM auth rotation were active; the latest ARM token
receipt expires **20:50:01 UTC**, with the current rotator's nine-hour bound ending
approximately **03:44 UTC September 17**. These are observed state and configured
bounds, not guarantees of future refresh or availability.

## Evidence

Paths are workspace-relative private artifact references, not public download
links. They contain no embedded credentials here. Hashes identify exact retained
bytes; no old outcomes were edited to make this card pass.

Let `S` denote `_artifacts/go-small-controller-20260914/state/` and `P` denote
`_artifacts/perf75-slow-seed-20260916/`.

| Ref | Artifact | SHA256 |
| --- | --- | --- |
| R1 | `P/RESULT.md` | `f4c98db4c42a237ff3f1215841a4bb3bdd74000246d01d4a135772733ea13347` |
| R2 | `S/slow75-qps5-001/b371f5c5bff83d24054c7470e9a8c4b9/performance/results.json` | `df803e723e4a4d5144a775e9915c03d234bb9a8eb6497db9567f918a36cea135` |
| R3 | `S/slow75-qps1-001/329b35f4419f15b899938b0650f9089b/performance/results.json` | `c95fc9968f3e1e4e03bb57d596d00785221ac92924ad7bfe0399b7842e34dc25` |
| R4 | `S/slow75-hour1-001/74cee430cbaedbe6c70347d415d338f3/performance/outcome.json` | `33843636adba66dc9ddb288e4f9735de272a6732e923f79634b7da39fb0ea447` |
| R5 | Same hour attempt, `performance/results.json` | `e07ae4d1b5098a681238961528e7113ae2ba9ea7a3bb668f2ba10a2010fac1a3` |
| R6 | `S/load75-quiet300-002/cf62f078416b950570b19ec0c9cec530/performance/outcome.json` | `d9ebf927db6056092f4727d5120cce3b4e8d7919f064dd6317c8fc423393c613` |
| R7 | `S/slow75-hour1-001/spec.json` | `3748e0eb6351539f87d1c164526316bad91a4d13b7fbf49f745018ede5f4312f` |
| R8 | `S/slow75-seed-001/0e7fb3e9effe27c8f008532d2c820adc/performance/population.json` | `137cd7076fea7aa846b5602fdea6f212c45d0bc472ab37de43ecb47ac263ad31` |
| R9 | R2 attempt, `performance/population.json` | `17e937c0de9072cd457f79f5bf1ccd8d9d29b81053a8f4427486a7b2c9690af9` |
| R10 | R3 attempt, `performance/population.json` | `a7dbdff0b62e72b5d4e07d36a07606cb6143554b4627df2cfaf0030bec52c917` |
| R11 | `P/cpo-oom-20260916T194025Z/REPORT.md` | `ae5c5cbe7f31b5dee0facd9d80c0a3e48e911c20011bb15c678ca07c66064eca` |
| R12 | `_artifacts/go-small-controller-20260914/diagnostics/20260916T194504Z/objects.json` | `1c6ac0c34514fe6e474c7336cdf66c25956472f8d779d2968bcfb9ef34a2a400` |

Earlier validation details remain in
`_artifacts/perf75-load-20260916/VALIDATION-RESULT.md` and `QUIET-PROGRESS.md`.
The final review changed documentation only; no load, cleanup, CPO correction,
source fix, or resource update was dispatched.
