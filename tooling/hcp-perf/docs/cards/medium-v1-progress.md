# Medium v1 Progress: Paused On Observer Fidelity

**Experimental personal-development evidence, September 10-11, 2026. Paused;
no active characterization load. Not a supported size or production SLO.**
The three-node 600-Pod API cases and six-node retained-600 control completed.
900 Pods reached Ready, but no measured 900-Pod API arm completed. No 1200-Pod
case, long soak, or nine-node trial ran. The final stop was sustained failure to
collect node-exporter metrics under CPU competition, not a demonstrated KAS/OOM
capacity boundary. Original failed attempts remain failed. [C1-C3]

This is a progress record, not a replacement for the [Small v1 card](small-v1.md).
See [Upgrade Signal Evidence](../upgrade-signals.md) for metric semantics and the
distinction between customer sizing, maintenance and provider problems.

## Profile And Environment

Medium uses the literal doubled Small policy, including floors and fallback:
KAS requests 500m CPU / 2048 MiB per replica; etcd main requests 200m / 512 MiB.
The fallback is 20m / 128 MiB. Policy memory safety limits are three times the
request; Go soft targets are 80% unless explicitly disabled. KAS has a 6144 MiB
hard memory limit and an observed 1638.4 MiB Go target. CPU limits are absent.
Requests are scheduling/footprint references, not hard usage caps. [C4-C5]

KAS and etcd retain three replicas. Management workers increased from Small's
three to **five Standard_D4s_v3** workers: two in each of two pools and one in
the third. One increase was explicit scheduling capacity; another was an
autoscaler addition. Infra/system pools were unchanged. Guest workers are
Standard_D8s_v5, first three and then six. The six-node control retained the
original 600 application Pod identities without rebalance, initially distributed
191/200/209/0/0/0. Additional host capacity, placement, fresh cluster state,
fixture histories and competitor allocations are confounds, not a pure isolated
RAM/CPU-size comparison. [C1-C2]

Configured actual-Pod totals, from lifecycle's six-node readiness snapshot and
cross-checked against the later six-node campaign Pod UIDs and full specs: [C4]

| Scope | Pods | Running Containers | CPU Requests | Memory Requests |
| --- | ---: | ---: | ---: | ---: |
| Entire active HCP namespace | 66 | 145 | 7.26 cores | 26858 MiB |
| Excluding experimental kube-state-metrics | 65 | 144 | 7.25 cores | 26794 MiB |
| Policy-hash-marked Pods | 52 | 97 | 6.78 cores | 24704 MiB |
| Unmarked/downstream Pods | 13 | 47 | 0.47 cores | 2090 MiB |
| Experimental kube-state-metrics only | 1 | 1 | 0.01 cores | 64 MiB |

These totals include regular containers and one restartable-init sidecar, not
terminal Pods or a blind sum of transient init containers. Scheduler-effective
CPU/memory requests, using sequential init/restartable-init maxima and Pod
overhead, equal these totals in this snapshot; no Pod-level resources were
present. Policy-marked denotes observed Pod annotation scope, not a new
per-container projection audit or proof that unmarked containers are exempt.
All 145 have CPU/memory requests; 98 have explicit memory limits, including KSM.
There is no inferred whole-HCP hard cap. Guest, recorder, competitor, management
system and fleet resources outside this namespace are excluded, not free.

## Completed Operating Points

Each API arm offered requests for 180 seconds at 600 application Pods, using six
namespace identities, GET Deployment : LIST Pods : LIST ReplicaSets weights
2:1:1, LIST limit 100, 128 maximum in-flight requests and a ten-second body
deadline. Background hourly application turnover used maxOutstanding 16.
Benchmark APF was 50 shares with Reject; canary APF was five shares with bounded
Queue. KAS inflight remained 150+50. Connections were reusable, not guaranteed
round-robin. Success requires a completed 2xx response body without a body or
transport error. QPS below includes drain time in the denominator. [C1-C2]

| Guest Nodes | Offered QPS | Successful Bodies / Offers | Successful QPS | HTTP 429 | Successful p95 GET / Pod LIST / RS LIST |
| --- | ---: | ---: | ---: | ---: | --- |
| 3 | 50 | 8976 / 9000 | 49.80699 | 24 | 164 / 300 / 207 ms |
| 3 | 15 | 2700 / 2700 | 14.99420 | 0 | 95 / 165 / 128 ms |
| 3 | 30 | 5400 / 5400 | 29.99022 | 0 | 84 / 173 / 112 ms |
| 6, retained population | 30 | 5400 / 5400 | 29.98651 | 0 | 104.6 / 287.8 / 158.4 ms |

All four completed six of six scheduled rollouts and six of six canaries, with
no API timeouts or generator-limited offers. A separate three-node 600-Pod
density arm had one completed soft-degraded ACK; it was not a hard timeout.
The rounded Small reference of about 15 successful QPS and the Medium hypothesis
of 30 successful QPS are not customer guarantees. The measured 50-QPS arm
exceeded that hypothesis, but neither it nor these short sequential tests
establishes sustainable maximum capacity or an acceptable error-rate/latency SLO.

900-Pod seeding was attempted in two later runs, with explicit UID-guarded
cleanup/reseed between them. All 900 Pods were Ready in the final snapshots,
but observation stops occurred before seed/setup returned and before a scored
900-Pod API arm. Readiness is population convergence only. Planned 1200-Pod,
12-identity and alternate-turnover treatments are **unmeasured**. [C2-C3]

## Resource Signals

The following describe the three-node 600-Pod API windows, not the later failed
900-Pod setup. Etcd size/health, APF allocation/demand, KAS memory and paired Go
memory have 36/36 fresh native evaluation slots per replica per arm. Slots are
not independent scrapes. Cgroup CPU/PSI derivatives have only 10-12 valid distinct
source-time pairs per replica/arm; node-exporter coverage is incomplete. [C5]

- Etcd quota is **8 GiB per member**, exactly the same as Small despite doubled
  memory requests. Allocated backend size stayed **83.45-83.48 MiB/member**, about
  **1.02% of quota**. Observed in-use size across the arms was **41.04-69.38 MiB**,
  below 0.85% of quota. Do not sum replica copies. Quota is independent of RAM.
- Proposal-pending sampled maxima across members were 1/2/1 at 50/15/30 QPS;
  medians were zero. Failed-proposal counters stayed zero, has-leader stayed one,
  and leader-change counters did not increase in these windows.
- At 50 QPS, per-member interpolated fsync p99 was **13.56-14.45 ms** and backend
  commit p99 **15.90-27.94 ms**, from 34 valid adjacent bucket-delta pairs/member.
  These are scoped histogram estimates, not validated disk thresholds. Allocation
  minus in-use and compaction history are maintenance context, not evidence to
  increase RAM. Slow storage or peer faults need provider diagnosis first.
- Benchmark nominal allocation remained **33 seats/KAS**; current allocation
  was **57-62** at 50 QPS. Demand high-water maxima were **62/62/38** across the
  three replicas. Two reached sampled allocation while clients saw 24 rejections;
  server rejection reason was `concurrency-limit`. Event-series births prevent
  naive counter subtraction from recovering every first rejection.
- KAS cgroup charge peaked at **1515.80 MiB**, below the 2048 MiB request and
  6144 MiB hard limit. Paired Go Sys-minus-HeapReleased peaked at **1483.07 MiB**,
  below the effective 1638.4 MiB soft target. Observed KAS memory PSI stayed zero.
  These sampled maxima are not safe ceilings or interchangeable memory measures.
- At 50 QPS, per-KAS CPU service medians were **0.259-0.346 cores**, with maxima
  **0.647-0.679 cores** against 0.5-core requests. CPU-wait PSI medians were
  **14.26-20.70%**. Above-request service is not throttling or configuration drift.

Small's 50-share comparison achieved 15.641 body-complete QPS including drain,
with 5447 HTTP 429s and substantially greater observed APF seat demand. Medium's
improved useful work and lower residence demand are consistent observations, not
a controlled causal estimate. APF demand/allocation, rejection/wait, per-container
CPU pressure and Go target/GC behavior are candidate leading indicators to retain.
No production upgrade rule follows without sustained customer impact, reliable
coverage and evidence that the proposed profile changes the limiting resource.

## Capture Failures

The campaign encountered different observation failures; they must not be merged
into one HCP capacity result. [C1-C3, C6-C7]

1. Initial three-node `run-v1` stopped at a transient node-exporter preflight
   failure before 600-Pod seeding. The failed outcome was preserved.
2. Three-node `run-v2` completed the reported cases. Its guard recorded 36
   gap-bearing observations and 13 recovered node-exporter episodes. A 69.54-second
   poll-observed episode included confirmation time, not proven continuous loss.
3. Six-node `run-six-v2` completed the retained-600 control, then stopped during
   900-Pod setup. Approximately 14-second guard polls missed successes between
   five-second scrapes. Independent paired history contained 12 up and six down
   evaluation slots, successful source timestamps at most 15.004 seconds apart.
   The 69-second classification was a sampling/recovery-policy limitation.
4. The corrected `run-history-v1` used raw `up` plus `timestamp(up)` history for
   sustained loss, separately from strict two-fresh-success admission. **21 frozen
   offline gate tests pass**, independently rerun for this card. Its final stop
   was different: last successful source **02:38:18.804 UTC**, followed by **13
   distinct failed source samples through 02:39:23.811** with no intervening
   success. The guard reported **72.100 seconds since success**; independent stop
   timestamp subtraction gives **72.165 seconds**. First post-stop up evaluation
   was **02:39:35**. This demonstrates sustained metric collection loss, not a
   proven KAS outage or exact exporter-process unavailability interval.

The final failed target had `context deadline exceeded`, measured scrape duration
5.0013 seconds and configured scrape interval/timeout both five seconds. Its
source-history evidence distinguishes this failure from the earlier aliasing.
The final campaign had no sampled resource/node/OOM/restart violations; its sole
safety failure was capture loss. The changed gate did not erase older outcomes.

Post-finalization offline review fixed two additional edge cases in the maintained
runner: historical failures now invalidate pre-failure admission confirmation
without refreshing the last-success clock; deduplication preserves a fresh source
witness when later evaluations replay it stale. Invalid/nonfinite values and
future timestamps are rejected as witnesses. The maintained suite now has **24
passing tests**, including the exact success105/failure110/success115 and
success100/120/172-with-stale-replay regressions; full module race tests and vet
also pass. These changes were not applied to frozen runs and were not exercised
live. Offline rechecking of the original raw evidence against the finalized audit
still finds 13 failed source samples after 02:38:18.804, no intervening success,
and 72.165 seconds to the stop. The historical 21-test frozen result and failed
run outcomes above remain unchanged. [C7, C9]

## Paused State

Campaign generators, competitors and recorders are stopped, with no active local
campaign children and no finalization errors reported. The 900-Pod fixture remains
Ready and retained, not generating experiment load. Observation-owned scalar and
histogram workers and all six guest-stat collectors were gracefully terminated
around **02:43 UTC** and verified with zero main PIDs. [C3, C8]

Streams retain terminal `incomplete` status after intentional cancellation, not
a fabricated clean capture. Fully returned windows extend through **02:42:25**,
covering the last attempt and recovery tail; no final backfill was needed. All
**2988 renewed-stream query files**, **1,271,256,766 bytes**, were hash-verified.
Six guest-stat files were hashed; five collectors recorded zero failed polls,
one recorded one failed poll. Signal exit 143 is preserved. Byte integrity is
not lossless capture or off-machine backup.

Prometheus remains Running/Ready with its 16 GiB PVC and 12-hour retention. All
41 historical capture PVCs and all 46 PVCs present at observation setup remain;
no PVC was deleted. Local forwarding is not workload and retains its prior
bounded lifetime. Old streams, handover bridges, original failed runs and raw
gap evidence remain private. Retention expiry can remove Prometheus samples;
do not assume the surviving PVC preserves them indefinitely.

## Next Observer Experiment

**Proposal only; not applied and no further load authorized by this card.**
Current node-exporter requests **50m CPU / 128 MiB**, has a **512 MiB memory
limit and no CPU limit**, and runs on each management worker. Container command
is not overridden. Arguments set host proc/sys/root paths and enable
`--collector.pressure`, `--collector.processes`, and
`--collector.vmstat.fields=.*`; default collectors are not disabled. Readiness
is HTTP `/`, every ten seconds with a one-second timeout and failure threshold
three; there is no liveness or startup probe. The readiness deadline is separate
from the five-second Prometheus scrape deadline. [C7]

Start with a **short observation-only matched test at the retained population**,
not another population/API sweep. Keep HCP/APF, scrape interval/timeout, source
identity inventory, required CPU/memory/PSI families and source-history loss policy
fixed. Record exporter CPU service/PSI, restarts/readiness, response sizes and
`node_scrape_collector_duration_seconds` / `node_scrape_collector_success` before
and during a separately approved bounded CPU-pressure interval.

The smallest first treatment is to increase only exporter CPU request, for example
**50m to 200m as an explicit unvalidated hypothesis**, retaining current memory and
collector settings. Check scheduling first and count the added request/overhead.
Freeze competitor requests rather than auto-refilling headroom after this change;
otherwise the comparison changes both observer budget and competition. Document
this as an observation-infrastructure treatment, not a new HCP size or free capacity.
Do not claim 200m is adequate until measured source success and freshness prove it.

If collector timings instead identify unnecessary expensive work, test a separate
minimal collector configuration: remove process enumeration or narrow vmstat only
after confirming they are outside required evidence. Preserve CPU, memory, PSI
and needed disk signals. Do not simultaneously change budget, collector set,
timeout and probe policy. Do not move host observations off the host without an
equivalent verified source. Only resume characterization after bounded observer
validation passes the unchanged source-history gate; never advance by hiding
node-exporter or simply increasing the loss allowance.

## Evidence Index

Paths are relative to the perf-runner worktree. Let `R` denote
`_artifacts/medium-v1-20260910/`. These private files are not distributed with the
card. Hashes identify reviewed bytes, not public availability or completeness;
metric/resource summaries retain underlying raw-source hashes. No credentials,
private addresses, raw object IDs or logs are embedded here.

| Citation | Path Under R | SHA-256 |
| --- | --- | --- |
| C1 | `campaign/result-v2.json` | `3fbde23efd169625a9ed19cef45a4cc5421b36971f6cc41d61e8ee02b6f41e0a` |
| C2 | `campaign/result-six.json` | `ca46e5d7e4bcf8debe17fd309e19466efb7dc8bcf966532c5702a1fde9545817` |
| C3 | `campaign/result-history.json` | `f2b0d81c89020cd0282a65113b67c4950560b1d1d9a50f8c97a61b41e2d40be0` |
| C4 | `observations/check-0230/resource-totals.json` | `5d5728beb48a65621760c5f2f42b4bda8073c29d647f87bf83641b8ee5fa60c4` |
| C5 | `observations/renewal-0205/candidate-results.json` | `77669f12ae0d3a9308f5a40e6e36233c534ed04be79320686b5473c8668e7388` |
| C6 | `observations/check-0230/guard-evidence.json` | `e7eb3b8c0880f6245fe04997eca51bc4e8aa6d3f4cf318e4e9efc00ef7a80967` |
| C7 | `observations/final-0300/outage-review.json` | `ebfb9b2b86b3e134b8c10078f0209d66677c8c78c07b5bcd1d05e9f96e7b0488` |
| C8 | `observations/final-0300/terminal.json` | `fcc42a4ec4e9aaf1e3fc71ab61006ada4d9382328136f92f9943230e2560e8c6` |
| C9 | `observations/final-0300/gate-tests.log` | `adb7775bd67620815149d60c69f4a6d2524a9f866d1976caea3ef81c73dd47be` |
