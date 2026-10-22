# Pinned Catalog Three-QPS Hour

**September 17, 2026: full one-hour execution completed, but workload assessment
failed because 20 requests timed out. This is not a clean three-QPS pass.** The
earlier [successful one-QPS hour](cpo-memory-retest.md) remains a separate result.
No packed-host, contention or production-SLO qualification is claimed.

## Delivered Work

The lead pinned all three catalogs using supported HostedCluster annotations,
verified their digests independently, and recorded HO/CPO propagation. No direct
Deployment patch, CPU/memory, CPO image, probe, HA or cadence change was made.
The previous image-drift-interrupted hour remains failed. Its population was
UID-cleaned; a fresh slow seed produced 300 Pods before the pinned hour reused
that verified population. Cluster history was not reset.

Measurement ran **12:48:19.789-13:48:19.789 UTC**, with final recovery outcome at
**13:59:10.219**. The quiet GET Deployment : LIST Pods : LIST ReplicaSets mix
remained 2:1:1, with 128 maximum in-flight requests, ten-second deadlines and no
API retry or CPU competitors.

| Result | Observed |
| --- | ---: |
| Offered / dispatched / completed | 10800 / 10800 / 10800 |
| Successful bodies | 10740 |
| HTTP 429 | 40 |
| Timeouts | 20 |
| Successful-body QPS including drain | 2.97998 |
| Successful p50 / p95 | 1.407s / 4.486s |
| Completed / scheduled rollouts | 60 / 60 |
| Measured canaries / soft-degraded | 54 / 4 |
| New OOM / restart | 0 / 1 KCM |

The KCM restart was observed at 13:47:26, with snapshot time 13:47:13. It was not
an OOM. The configured guard permitted recovery; do not describe the hour as
zero-restart health or attribute individual request timeouts to this restart
without request-level evidence. Native capture retained 17 warnings, seven
recoveries and an incomplete verdict. Catalog pins remained selected without drift.

## CPO Memory

CPO remained the same process throughout the hour, with constant
`process_start_time_seconds`; the policy stayed at **144 MiB request, 159 MiB
hard limit and 115.2 MiB Go soft target**. Selected CPO metrics have **240/240
fresh native 15-second evaluation slots**, after bounded tail recovery. These
are not independent scrapes, and coverage is not universal across all processes.

| Hour Measurement | MiB |
| --- | ---: |
| Maximum cgroup charge / working set | 134.02 |
| Maximum cgroup RSS | 132.07 |
| Maximum paired Sys-minus-HeapReleased | 133.54 |
| Last-GC live heap range | 41.50-89.75 |
| Reported cAdvisor maximum | 156.57 |

The cAdvisor maximum was **already present at hour start and stayed constant**.
It is not a new hour peak or direct kernel `memory.peak` observation. Managed
memory exceeded the soft Go target while sampled cgroup charge stayed below the
hard limit; those boundaries and memory quantities are not interchangeable.

Hour-aligned last-GC live heap at minutes 0/15/30/45/60 was approximately
**80.32/62.56/83.71/84.24/45.14 MiB**. Checkpoints use the preceding native
evaluation, with exact source timestamps retained. This fluctuating sequence is
not a proof against longer-term growth. GC counters increased by **3895 cycles
over 3585 observed seconds**, approximately **1.0865 cycles/s**. Allocation was
**21,246,781,312 bytes**, approximately **5.652 MiB/s**. No counters were differenced
across the old OOM process; no forced GC or pprof capture was performed.

## Observation Boundary

External streams ended at **13:39:33 UTC**, before the last nine minutes of
measurement and recovery. Only the missing **13:39:33-13:59:10** Prometheus window
was backfilled from the retained server. Forty-four query transport failures in
the first backfill minute were preserved; only those failed queries were retried
into a separate artifact. Missing source scrapes cannot be repaired by backfill.

Guest kubelet-stat collection ended at **13:39:34.915**. Its remaining hour and
recovery history is unavailable from that collector and was not reconstructed.
The native performance polling recorder is a separate evidence source.

Across the pinned hour, 43 `up` series returned 240 evaluation slots each, with
five down target-slots: two KCM and three OpenShift controller-manager. These are
neither continuous-outage durations nor evidence that all metric families were
complete. Request histogram scope remains measured GET/LIST resources; successful
body latency comes from client records, not a nonexistent histogram status label.

Finalization verified **15470 response files plus guest-stat files**, totaling
**2,345,965,677 bytes**, without a duplicate archive. Original failed backfill
records and 1192 incomplete query records remain. All observation writers are
terminal and readiness is revoked; one Prometheus and all ten original PVCs remain.
No parent controller, retention, auth or RP-forward service was stopped. The
retained population/HCP is not generating observation-owned load.

## Evidence

Authoritative lead report:
`_artifacts/cpo-memory-investigation-20260916/PINNED-HOUR3-RESULT.md`.
Native attempt: `cpo144-pinned-hour3-001/4ba0a5b61defbc358b983dc666e99586/performance`
under the Go controller state root. Result SHA-256:
`ef1269f85e7335e1cd1bfa65afedb66875245a7cf656be061c110bb21896494f`;
outcome SHA-256:
`0c6541f14071f4133b3c654cf7f89c07197ff3c5964a3bf712fc6e78c47455d2`.

Independent observation evidence under
`_artifacts/cpo-memory-investigation-20260916/observations-hour3/`:

- `final.json`: finalization, epoch bounds and guest-stat gap.
- `final-pinned-20260917/hour-runtime.json`: full source labels, timing, memory,
  GC and source-coverage summaries.
- `final-pinned-20260917/query-file-hashes.json`: every preserved metric response.
- `final-pinned-20260917/checksums.json`: report and summary hashes.
- `final-pinned-20260917/units-terminal.txt`: writer termination evidence.

Private durable local evidence is not a public archive or off-machine backup.
Future tests require fresh readiness and parent-owned retention coordination;
neither high successful throughput nor full-window execution erases the timeout
assessment failure.
