# Small At 75% Of The HCP Fee

**Personal-development experiment, September 15-16, 2026. Lifecycle provisioning
demonstrated; performance and capacity not qualified. Final review: September 16,
00:27 UTC.**

The native Go controller created a hosted control plane and three
`Standard_D8s_v5` guest workers within the modeled **$0.1875/HCP-hour** envelope.
CAPZ's resource increase cleared its worker-provisioning OOM blocker. All three
nodes subsequently passed an eleven-sample, greater-than-five-minute Ready and
pressure check.

This is not an end-to-end performance success. The first quiet validation stopped
during baseline collection after a capture-source failure. Its replacement failed
before seeding because a catalog was unready. At final review, the guest nodes
remained healthy, but the community catalog was unavailable following repeated
liveness-triggered restarts. No throughput, application capacity, or one-hour
confirmation result was obtained.

## Budget Chronology

The target is **75% of a $0.25/HCP-hour fee**, not a $0.1875 fee. The illustrative
compute model uses West US 3 Linux on-demand `Standard_D16s_v6`: $0.806/hour,
16000m CPU and 65536Mi memory. Cost is the larger of the charged CPU and memory
fractions, multiplied by the node price. It is not an invoice or a demonstrated
packing density. Actual management workers were D4-series hosts, not D16s_v6
benchmark hosts.

| Treatment | Charged CPU | Charged Memory | Modeled $/HCP-hour | Observed Result |
| --- | ---: | ---: | ---: | --- |
| 75% V1 | 3720m | 15180Mi | 0.187395 | RP succeeded; all core workloads became available. The original strict probe gate rejected individual probe failures. A later audit found 18 minutes of sampled availability without restart, OOM, or probe-kill increments. |
| Measured-weight V2 | 3720m | 15180Mi | 0.187395 | Restored measured CPU weights, including KAS 244m and CPO 231m. Passed the explicit availability gate and retrieved credentials. Worker provisioning exposed CAPZ OOMs at a 71Mi limit. |
| CAPZ V3 | 3720m | 15243Mi | 0.187467316 | CAPZ raised to 50m/128Mi request, 141Mi limit. Native cluster, observation, credential, and three-worker actions succeeded. |

The allocator accounts for Pod parent CPU caps, replicas, sequential init maxima
and restartable-init overlap. Memory limits are 110% of requests rounded up to
MiB; Go memory targets are 80% of requests, with explicit non-Go exceptions.
The fixed downstream contribution is 470m/2090Mi; it is accounted for but not
enforced as a downstream hard cap. Instrumentation is excluded from this offering
model. Saved-layout accounting is not proof of complete live peak demand.

CAPZ V3's native wire SHA-256:
`f8678c7989576b98ae12d505e61825e4c40420065887a976eb50034fe97b847a`.

## Lifecycle Evidence

- V3 control-plane availability passed from **22:39:19 to 22:44:34 UTC** on
  September 15: 47 required workloads, 66 Ready Pods and 12 required endpoints.
- Three guest nodes became Ready at **23:10:29, 23:11:09 and 23:12:06 UTC**.
- Eleven read-only checks from **23:16:27 to 23:21:36 UTC** verified the same
  three node UIDs, the intended VM type, Ready status, and no memory, disk or PID
  pressure. NodePool Ready, AllMachinesReady and AllNodesHealthy were true.
- CAPZ had **zero restarts** through provisioning and at the final review.
- OpenShift was explicitly pinned to **4.21.32**, release digest
  `afc68ed6ce1af80d6e43014d16ad7b3f2067be6bebd970aa556c6a445290c8ee`.
- CPO digest `19848b826504783497b22c598271521c6178dd473e9aea8d0ccf8cc7c3f49d02`
  and fixed prober digest
  `edc5ba19183cd4bb2924c43e75b353a6ba54810324da7bdf43637de67aae85e2`
  remained unchanged. No OLM concurrency treatment was applied.

The explicit `probeFailurePolicy: "availability"` retains failed probe attempts
as quality warnings. Actual unready replicas/endpoints, new OOMs, restarts,
probe-triggered kills, identity/profile drift and incomplete inspection still
prevent availability qualification. Historical startup failures are retained;
passing a later window does not establish a clean fresh startup.

## Performance Attempts

| Case | Executed | Outcome |
| --- | --- | --- |
| Quiet validation 001 | Seeded three applications / 15 Pods; configured scoped RBAC/APF and credentials; completed warmup and two baseline canaries | A scheduler scrape returned `up=0` during the next baseline. Safety canceled the case; canary cleanup completed. The scored five-minute window never began. |
| Quiet validation 002 | Native UID-pinned cleanup of case 001, then HCP preflight | Cleanup verified both namespaces and four APF objects absent. Certified catalog was unready at preflight: 65/66 Ready Pods, 12 endpoints. No case-002 seed or measured load was created. |

Case 001's two completed baseline canaries had Ack/Ready/Cleanup durations of
**0.501/5.112/3.482 seconds** and **0.355/3.562/7.023 seconds**. These are setup
observations, not scored latency or SLO results. No measured APF/API-QPS phase,
CPU-competitor case, population screen, or one-hour confirmation ran.

Capture timing was explicitly measured and configured: KAS 60s interval / 45s
timeout / 165s freshness; OpenShift API 30s / 20s / 80s. Other sources retained
45s default freshness. The owner validated 43 targets across 23 jobs before the
first case. Bounded source-loss grace was added for subsequent cases only; it
does not erase case 001's gap, qualify incomplete metric families, or permit
HCP readiness failures. Evaluation-grid repetitions are not independent scrapes.

## Final Failure Classification

The final control-plane object snapshot completed at **00:27:15.980 UTC,
September 16**. It contained 66 running Pods, **65 Ready**. KAS, OpenShift API,
packageserver and etcd-client each had three Ready endpoints: **12/12 core
endpoints**. Community catalog had zero Ready endpoints.

| Container | CPU Request / Limit | Memory Request / Limit | Final Status And Latest Failure |
| --- | --- | --- | --- |
| Certified catalog registry | 50m / 50m | 344Mi / 379Mi | Ready in this snapshot; 17 restarts. Kubelet liveness kill at 00:21:22, exit 0 (`Completed`) at 00:21:38. |
| Community catalog registry | 50m / 50m | 317Mi / 349Mi | Unready; 13 restarts. Kubelet liveness kill at 00:26:12, exit 0 (`Completed`) at 00:26:16. |
| CAPZ manager | 50m / 50m | 128Mi / 141Mi | Ready; zero restarts since 22:21:49. |

The latest catalog terminations are **liveness-triggered restarts, not OOMKills**.
That classification comes from matching kubelet Killing events and container
termination times; exit 0 alone would not establish the cause. It does not rule
out CPU or memory pressure contributing to missed probes. Restart totals are
lifetime counters, not all failures occurring during a performance case.

At the contemporaneous guest check, all three original node UIDs were still Ready
with no memory, disk or PID pressure. Their Ready transition times were unchanged.
The final control-plane snapshot SHA-256 is
`5a92c7c8cbffdf2770373b843514551cfc3d223b04f3fd8c74854322bc9b6209`.
Kubernetes LISTs and polling are not an atomic or lossless cluster-wide history.

## Preservation And Next Candidate

Both performance attempts are finalized `FAILED`, not running. Case 001's
population, canary and four APF objects were deleted by the typed Go cleanup
action and absence-verified; case 002 created no fixtures. Final read-only checks
found no validation fixtures and no managed CPU-burn Jobs or Pods. The HCP, guest
nodes, PVCs, old capture data and immutable case evidence remain preserved.

Only native retention orchestration is active under `perf75-retain-003`.
It verified **all nine resource groups at 00:24:53.237 UTC**, with its three-hour
bound ending approximately **03:24:53 UTC September 16**. External observers
remain separately owned; no observer-stop action was taken in this review.

The maintained Go workflow now includes read-only verification of finalized
failed non-competitor cases and separate UID-pinned population cleanup. Recovery
does not replay measurement; cleanup checks identity/ownership, rejects PVCs and
unexpected workloads, and does not remove finalizers. Full Go tests and targeted
performance/iteration race tests passed during this work. No Python lifecycle or
manual SQLite editing was used.

A revision-matched OLM/packageserver bounded-refresh-concurrency image has been
built and verified offline, **but not applied**. It targets unbounded per-package
catalog refresh fan-out. It may help catalog pressure, but neither memory savings
nor live stability are proven. No further profile or image change is part of
this result. These observations do not prove the 75% budget is impossible.

## Local Evidence

Private artifacts are intentionally not committed and contain the exact resource
identities and receipts. Paths below are relative to the workspace artifact root.

- `go-small-75pct-20260915/budget/native/`: V1 input, ledger and wire.
- `go-small-75pct-20260915/budget-measured-v2/native/`: measured-weight V2.
- `go-small-75pct-20260915/budget-capz-v3/native/`: CAPZ V3 input and cost proof.
- `go-small-75pct-20260915/LIVE-V{2,3}-RESULT.md`: detailed lifecycle chronology.
- `go-small-75pct-20260915/perf-v3/VALIDATION-00{1,2}-RESULT.md`: case outcomes.
- `go-small-controller-20260914/diagnostics/20260916T002711Z/objects.json`:
  final control-plane snapshot and adjacent command receipt.
- `go-small-controller-20260914/diagnostics/guest-v3-20260915T231622Z/`:
  five-minute guest-node verification.
- `go-small-controller-20260914/state/`: schema-2 native journal, frozen plans,
  failure-verification and cleanup receipts, case measurements and raw exports.
- `go-small-75pct-20260915/observations-v3/`: capture timing experiments,
  source-validation receipts, guest stats and retained streams.
- `dependencies/package-server-concurrency/HANDOFF.md`: matched source revision,
  build/test provenance and unapplied image digests.
