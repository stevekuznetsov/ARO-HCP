# Five-QPS Runtime Appendix

Companion to [the diagnostic-hour card](small75-observe5-hour.md), not a new
capacity qualification. The September17,2026 five-QPS hour completed execution
but FAILED workload assessment for control-plane degradation. No live profile,
image, probe, workload, or safety-policy changes were made for this analysis.

## Capture

Private evidence: `_artifacts/small75-resilient-hour-20260917/observations/`.
See `final.json` and `final-20260918/{REPORT.md,seal.json,source-coverage.json,
query-coverage.json,guest-coverage.json,cpo-runtime.json,lead-hour-timeline.json}`.

Three native streams completed150/150chunks each throughSeptember18 00:11:25UTC,
covering API22:48:41-23:48:41 and final recovery23:59:23.13,650raw query hashes
verified;91queries. Zero export errors, but610query/chunk incomplete results
remain explicit. All43targets have complete15s export grids through recovery;
OCM has two failed scrape evaluations. No missing/stale critical-job evaluation
was found on that grid. This is not continuous service availability.

Guest stats were attached22:00 with a fresh credential and the same three node
UIDs.232/232/231hour records, all successful/fresh by the collector's45s
all-resource criterion; maximum actual inter-record gaps36.084/35.810/44.401s.
One cancellation at final shutdown is outside the measured hour/recovery.
Allseven owned local units ended with MainPID=0; readiness revoked. Existing
Prometheus, all10management PVCs and parent lifecycle/retention services preserved.

## CPO

Same process-start value before the hour through recovery; no observed CPO OOM.
240runtime evaluations,240distinct runtime source times;216distinct cAdvisor
source times. Maxima below are sampled, not bounds on all transient peaks.

| Metric | API Hour Max | Through Recovery Max |
| --- | ---: | ---: |
| Cgroup charge | 96.77MiB | 100.26MiB |
| Working set | 96.11MiB | 99.61MiB |
| Cgroup RSS | 94.84MiB | 98.07MiB |
| Historical cgroup maximum | 102.22MiB | 103.13MiB |
| Go Sys minus HeapReleased | 93.63MiB | 96.64MiB |
| Go heap live | 35.54MiB | 37.02MiB |
| Go heap allocated / in use | 61.55/70.05MiB | 64.51/71.37MiB |

Limit159MiB; Go soft target115.2MiB. Historical cgroup maximum baseline75.55MiB
increased, but the exact peak time is unknown. Process resident metric reached
202.64MiB during the hour, unlike cgroup charged memory; do not conflate these
accounting domains or infer an OOM. This discrepancy remains unresolved.

First-to-last hour sampled deltas:1437GCcycles,3.236132s cumulative pauses,
425.014985GC CPU-class seconds,6.675922mark-assist seconds. GC limiter metric
stayed0 and GOMAXPROCS2. No forced GC, pprof retained-heap proof, maximum individual
pause measurement, or exact-hour counter extrapolation. Pre/post checkpoints and
full labeled summaries are retained privately; empty old-container series are
not zero-valued hour data.

## Interpretation

Native offline analysis confirms all18,000logical records:17,638success,
308HTTP429,54timeouts;2.0111%degraded. Raw nearest-rank successful body p95
5.381177s agrees with the frozen millisecond-histogram5.382s. Worst posterior
generator-aligned minute22:52:41-22:53:41:47/300degraded(15.667%), successful
p958.257030s. This selected minute is not a prespecified qualification or cause.

189inspection timestamps have15unhealthy samples (OAS10,packageserver7,overlap).
Actual spacing17.628-22.314s,mean19.082804s, not5s.15/189 is a sample fraction,
not an outage-time fraction. KAS/etcd had3Ready endpoints at every recorded
critical check; this does not prove continuous availability or etcd membership.
Individual livez results/timestamps are absent; hour-only success fraction and
incident timeline remain unknown. Lead's41.041s aggregate sampled livez outage
includes recovery. Prometheus up must not be used to replace that evidence.

Offline policy fixes belong to the lead; the original run and its failure remain
immutable. This appendix does not supersede the main card or authorize new load.
