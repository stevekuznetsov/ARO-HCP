# Audit-Funded CPO Memory Retest

**September 17, 2026: the quiet one-hour delivered-work test passed at 300
application Pods and one offered request/second.** All 3600 requests completed
successfully and all 60 rollouts completed. No new CPO OOM or restart was recorded.
One canary was soft-degraded; capture was incomplete. This is not packed-host,
contention, fresh-startup or production-SLO qualification. [R1]

## Treatment And Results

The approved policy changed CPO memory request/limit from **105/116 MiB to
144/159 MiB**, funded by three KAS audit containers changing **52/58 MiB to
39/43 MiB**. CPO CPU remained 228m. Its derived Go target changed from **84 MiB
to 115.2 MiB** (120795955 bytes). No probe, GOMAXPROCS, HA or workload-image
change was made by this policy update. Native charge was **3720m / 15241 MiB**,
cost `6142123/32768000` USD per HCP-hour, below the 75%-of-fee raw-resource target.
This is a resource-equivalent illustration, not a packed-host cost. [R2]

Catalog payloads drifted naturally and were independently rebound before the
retest. That, prior rollouts and cache/population history prevent an image-identical
causal comparison with the earlier failed hour. The earlier CPO OOM at its 116 MiB
limit remains a failure; no old counter was differenced against this new process.
The retained CPO/OLM/prober images and release were otherwise unchanged. [R1-R2]

The fresh 300-Pod seed used 2 QPS/burst 2 and a 30-second request timeout. Subsequent
cases reused the verified population, without repeated Service creation. The
request mix was Deployment GET : Pod LIST : ReplicaSet LIST at 2:1:1 across three
namespaces, max in-flight 128 and a ten-second body deadline, without retries.
No CPU competitors ran. Successful-body QPS includes drain time. [R1]

| Case | Successful / Offered | HTTP 429 | Timeouts | Successful Body QPS | Successful p95 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 QPS, three minutes | 180 / 180 | 0 | 0 | Short screen | 2.283s |
| 1 QPS, one hour | 3600 / 3600 | 0 | 0 | 0.99955 | 2.530s |
| 3 QPS, three minutes | 540 / 540 | 0 | 0 | 2.96787 | 2.859s |
| 5 QPS, three minutes | 896 / 900 | 4 | 0 | 4.90276 | 4.343s |
| 10 QPS, three minutes | 1275 / 1800 | 479 | 46 | 6.96584 | 6.800s |

The 10-QPS case completed execution but **failed assessment because of timeouts**;
no higher rates followed. The clean 3-QPS and lightly shedding 5-QPS short screens
are not higher-rate hour qualifications. [R1]

## CPO Hour Window

Actual API offering was **01:04:12.294-02:04:12.295 UTC**. Recovery began after a
straddling canary at 02:04:25.823; the final hour outcome was 02:14:43.135 after
ten minutes of recovery. The lead recorded 60/60 scheduled/admitted/completed
rollouts, no debt, 71 measured canary cycles and one soft degradation. The native
action succeeded with completed execution and workload `met-not-qualified`, but
`performanceStatus=degraded`; 36 capture warnings and eight recoveries were not
erased. A transient packageserver convergence sample remains recorded. [R1]

All CPO values below bind the same Pod UID, cgroup container incarnation and
constant `process_start_time_seconds`. The final live status still showed that
container running since **September 16, 23:01:38 UTC**, with zero restarts. [R3-R4]

The original observer checkpoints were capture-relative, not load-relative:
00:06:08, 00:21:09, 00:36:09, 00:51:09 and 01:06:08. The first four precede the
hour by approximately 58, 43, 28 and 13 minutes; the last is only **1m56s into
load**. Their last-GC live heap was 31.16/31.70/31.87/33.67/36.05 MiB and managed
memory 83.86/88.49/88.42/91.56/99.42 MiB. They are retained as pre-hour context,
not mislabeled as a complete hour trend. [R3]

The following separate checkpoints are aligned to the actual hour. They use the
preceding native 15-second evaluation, at most 15 seconds old; exact evaluation
and original source timestamps are in the dataset. Values are MiB. [R3]

| Hour Minute | Last-GC Live Heap | HeapAlloc | HeapInuse | Sys Minus Released | Cgroup Charge | Cgroup RSS | cAdvisor Maximum |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 31.20 | 64.42 | 70.58 | 97.96 | 102.95 | 101.25 | 111.67 |
| 15 | 36.39 | 67.06 | 77.97 | 103.24 | 109.02 | 107.05 | 113.85 |
| 30 | 38.81 | 46.99 | 66.90 | 106.78 | 111.08 | 109.36 | 118.38 |
| 45 | 42.74 | 59.71 | 72.02 | 101.70 | 105.71 | 103.98 | 119.25 |
| 60 | 33.74 | 36.45 | 61.13 | 105.98 | 106.30 | 104.56 | 119.84 |

Across the hour's full valid samples:

- Last-GC live heap ranged **27.20-51.30 MiB**, ending below its minute-45 checkpoint.
  This is not monotonic growth, but one hour cannot exclude a longer-term leak.
- Maximum paired Go-managed memory was **114.95 MiB**, just below the **115.2 MiB
  soft target**. Total Go Sys rose from 113.71 to 123.53 MiB; released heap must be
  subtracted at matching timestamps, not independent maxima.
- Maximum charge/working set was **117.52 MiB**, cgroup RSS **115.79 MiB**, and
  reported cAdvisor maximum **119.84 MiB**, below the **159 MiB hard limit**.
  These are observed metric maxima, not a direct kernel `memory.peak` or proof
  that every transient peak was sampled. Memory requests are not caps.
- Process RSS is retained separately and is not substituted for cgroup charge,
  cgroup RSS, heap or Go-managed memory. [R3]

## GC And CPU

The CPO runtime counters have **240/240 fresh evaluation slots**, no observed
counter reset, and an observed source span of **3585 seconds**. The full source
identity and constant process-start value were checked before differences. [R3]

| Measurement | Observed Delta / Rate |
| --- | --- |
| Allocation | 42,208,665,416 bytes (39.31 GiB), **11.23 MiB/s** |
| GC cycles | **1283**, **0.3579 cycles/s** |
| GC mark-assist CPU class | **10.574 seconds**, 0.00295 class-seconds/s |
| Total GC CPU class | **470.119 seconds**, 0.13113 class-seconds/s |
| Total runtime CPU class | **7171.397 seconds**, 2.00039 class-seconds/s |
| GC / total runtime CPU class | **6.555%** |
| Assist / total runtime CPU class | **0.1474%** |
| OS process CPU | **531.02 seconds**, approximately **0.1481 cores** |

GOMAXPROCS stayed two. Go CPU-class accounting includes runtime categories and is
**not OS CPU service**: the class fractions must not be divided by process CPU or
presented as literal cgroup utilization. Allocation throughput is not retained
heap growth. The last-enabled GC-limiter cycle metric stayed zero in this window.
No forced GC, heap profile or `inuse_space` profile was taken: CPO heap/allocs
profiling returned 404 and had no existing enable flag. All observations are passive
metrics; the older process's 84 MiB target is contextual, not a counter baseline.

## Coverage And Finalization

CPO selected runtime/cgroup gauges have 240/240 valid hour evaluation slots with
source age at most 45 seconds. Runtime sampled-source spacing was 15 seconds;
cAdvisor spacing reached approximately **30.61 seconds**. The export grid changed
from earlier five-second exports to **15 seconds** to bound storage. Full grids
are not independent scrapes and cannot recover missed transient peaks. [R3]

Coverage is not universal: the initial inventory contained **210 regular/init
container entries**, 149 literal GOMEMLIMIT entries including 57 terminated
containers, **27 exposed runtime endpoints and 25 mapped containers**. Two OVN
endpoints were left unmapped; registry catalogs lacked reviewed Go endpoints.
Etcd proxy metrics were explicitly attributed to the main process from its
localhost forwarding configuration. No claim that all configured Go targets were
observed is made. [R4]

The six-GiB budget stopped metric/guest writers at **02:41:34 UTC**. The cleanup
receipt's failure was the already-exited runtime watcher being absent, not proof
that data writers remained active; terminal main PIDs were independently checked.
Local completed metric windows stopped near **02:40:18**, before the last 10-QPS
recovery outcome at **02:41:24.700**. A new bounded backfill preserved only that
approximately 66-second tail from the existing Prometheus. It cannot repair
unavailable source scrapes. The original budget/cancellation receipts remain. [R4]

Finalization verified **16,234 metric response files** and all runtime/guest files,
**6,531,408,182 bytes total**, without a duplicate archive. Query records include
**296 errors, 1137 incomplete and 14801 successful**: optional allocator families
were initially absent, and neither those errors nor other gaps became zeros.
Guest files have 622 records each, one request error and one additional non-fresh
observation. All observation writers are terminal; `ready.json` is inactive/false,
with its earlier handshake preserved. One Prometheus and all ten original PVCs
remain, and parent controller/auth/retention/RP services were untouched. Retention
is separately owned through approximately 06:33 UTC. [R4]

The useful result is a completed quiet one-hour operating point at one offered
QPS under this revised memory policy, with explicit soft degradation and capture
limitations. It does not erase the earlier OOM, establish a causal allocation
benefit independent of catalog/history changes, or qualify a higher-rate hour.

## Evidence

Paths are private, relative to the perf-runner worktree. Raw response and process
identity hashes are retained transitively in the summaries. Hashes prove byte
identity, not losslessness or off-machine backup.

| ID | Path | SHA-256 |
| --- | --- | --- |
| R1 | `_artifacts/cpo-memory-investigation-20260916/RETEST-RESULT.md` | `79062e2c052823103e36b9761f06bf81632b8fcaca3d03cfd79e5b24cc66d3e2` |
| R2 | `_artifacts/cpo-memory-investigation-20260916/LIVE-UPDATE.md` | `ed5ce709dcb7e577aef6cef5e42fe2b7e761df33f509d0add68799dddd310cb2` |
| R3 | `_artifacts/cpo-memory-investigation-20260916/observations-retest/final-20260917/cpo-hour-runtime.json` | `80640e9f846e691d7b1441652f851d23e492cd7280eed7390f3af90896d216ff` |
| R4 | `_artifacts/cpo-memory-investigation-20260916/observations-retest/final.json` | `7901da150cbe29295d11f274a4e1f45ba4d0008ca67de0bd04d028cfb4a13607` |
