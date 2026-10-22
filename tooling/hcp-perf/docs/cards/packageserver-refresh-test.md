# Packageserver Refresh Test

**Completed September 16, 2026. The bounded quiet cadence/jitter test passed.**
All three packageserver replicas completed the expected catalog refreshes with
complete package counts. The temporary five-minute cadence was restored to the
original unset OLMConfig field, selecting the unchanged twelve-hour default.
The new images, four workers and 20% jitter remain deployed. No one-hour,
fresh-startup, capacity, density or general performance qualification follows.
No kube-burner population, APF fixtures or CPU competitors ran. [C1-C4]

## Source And Configuration

The source change is based on `openshift/operator-framework-olm` revision
`149d553b2057320d761c4d1913098a66787dd09a`, with retained source patches and
binary/image provenance. It bounds package work across catalogs within each
provider, makes worker count configurable, and schedules refresh jitter through
the delaying queue rather than creating a timer/goroutine per notification. [C5]

| Setting | Deployed/Test Value | Meaning |
| --- | --- | --- |
| `--package-refresh-workers` | 4 | Shared package-work slots per provider, not four total across all three replicas |
| `--catalog-refresh-jitter` | 0.2 | Fraction of the informer interval for periodic resync delay |
| `--interval` default | 12h | Unchanged default; OLMConfig overrides it |
| `OLMConfig/cluster.spec.features.packageServerSyncInterval` | Temporarily `5m`, then restored to absent | Existing canonical cadence control, not an injected image default |
| RPC/cache operation timeout | 5m | Independent of cadence; starts after the queued delay |

For same-resourceVersion informer resync, delay is drawn from
`[0, interval * jitter)`, independently per catalog/notification/replica. The
five-minute treatment therefore schedules `[0,60s)` extra delay. Initial,
watch-update and reconnect requests use `[0,30s)` when jitter is enabled. Queue
contention can add delay beyond the scheduled jitter; the informer itself still
ticks every five minutes. This is not a fixed five-to-six-minute interval measured
from the previous refresh completion. At the restored twelve-hour default,
periodic jitter can be up to 2h24m; initial/reconnect delay remains bounded by 30s.

The source tests cover worker bounds, multiple catalogs/channels, slot release,
cancellation, deterministic jitter bounds, unchanged RPC timeout and CLI validation.
Truncated package/bundle streams abort without garbage-collecting unvisited cache
entries. The live evidence below checks refreshed contents, not merely stale-cache
availability. Catalog RPC concurrency metrics were unavailable: four-slot sharing
is source/test evidence, not a measured cross-replica concurrency maximum. [C1, C5]

## Image Identity

| Image | Verified Digest |
| --- | --- |
| CPO wrapper | `33e5c072b86237ce47e4cf8a0e87eb04e679b69bfc793d0e70efb1414f9903fb` |
| Package-server/OLM image | `50b1a8603a52b59ca583c4b30d95d24efc3abb4e590473ddc2e626f835e9eb6c` |

Both are retained under the experiment's `arohcpocpdev.azurecr.io/stevekuznetsov/`
repositories. The CPO wrapper adds the OLM override while preserving the prior
availability-prober override and real CPO binary. The package image changes only
`/bin/package-server`; other OLM binaries remain byte-identical. Because the OLM
image key has several consumers, the lead's binding table covers all affected
main/init/helper images, not only packageserver. All three final packageserver
Pod `imageID` values match the package-server digest above. [C4-C6]

The release remains 4.21.32. Resources, limits, probes and catalog identities did
not change across the settled cadence comparison. The native charge remains
**3720m CPU / 15243 MiB, $0.187467316 per HCP-hour**, the retained raw
resource-equivalent model, not a bill or demonstrated packing cost. Guest VMs and
observation infrastructure are outside that control-plane model. [C1, C3]

## Baseline And Boundaries

The earlier CPO image update caused controller-driven guest replacement:
Replace RollingUpdate with maxSurge 1/maxUnavailable 0, desired replicas still
three and autoscaling disabled. All three original guest-node UIDs were replaced,
with a temporary fourth-node surge. HC/HCP/namespace identities stayed unchanged.
The test accepted this as a **new post-replacement baseline**, not preservation
of the original workers or an isolated fresh-startup comparison. [C1, C3]

The new three-node set and control-plane health passed the lead's ten-minute
baseline, **12:25:12.464-12:35:14 UTC**. The lead then applied the single cadence
field with dry-run and UID/resourceVersion-guarded JSON Patch, retaining the exact
original object. Restoration removed the added spec and returned exactly to its
prior absence. One expected exit-0/Completed restart per packageserver occurred
on each boundary. Package API availability was temporarily interrupted; it was
not continuously available across configuration and cache repopulation. [C1-C4]

## Refresh Results

The treatment observation spans **12:36:50-13:00:32 UTC**, with 42 health samples.
Sample zero caught the expected restart/API interruption; all 41 subsequent
samples had no health issues. Sampled continuous health was
**12:37:24.581-13:00:32.129**, or **23m7.547s**. Probe warnings appeared in 29
samples and remain degraded probe-quality evidence. There were no post-boundary
restart increments, new probe kills or observed OOM increments in the treatment.
Polling cannot prove absence of every short-lived event. [C1-C3]

Each of the nine catalog/replica combinations logged one initial schedule and
four periodic schedules, five starts and five successful finishes:

| Catalog | Packages In Every Successful Refresh | Successful Refreshes |
| --- | ---: | ---: |
| Community | 307 | 15 |
| Certified | 178 | 15 |
| Red Hat | 149 | 15 |

Total: **45 successful refreshes**, comprising nine initial and 36 periodic.
No unresolved starts, failed packages, refresh/stream errors, cancellations or
timeouts were recorded in the lead's treatment log analysis. Maximum scheduled
delay was **59.434911740s**; maximum full-refresh duration was **108.216789053s**.
All ten post-population aggregated package-API samples returned 307/178/149.
Those API requests were load-balanced, not direct per-replica reads; complete
per-replica evidence comes from successful finish logs and their package counts.
This is stronger than merely seeing a finish message or continuing to serve an
old cache. [C1-C2]

Restoration logs confirm **12h / four workers / 20% jitter / 5m RPC** on all
replicas. Recovery passed five minutes from **13:02:33**, with final receipt
**13:07:41.817 UTC**. A 13:04 cache-population sample still had only 145 community
packages; the later 13:06 sample returned 307/178/149 with successful finishes.
Do not claim cache completeness throughout the restart/repopulation boundary. [C4]

## Resource Observations

The independent raw stream reducer uses native five-second evaluation slots,
matching source timestamps and full cgroup label sets. The following working-set
maxima compare the settled baseline with the healthy treatment window. Each main
container has **121/121** fresh baseline slots and **277/277** fresh treatment
slots. They are not independent scrapes. Packageserver process incarnations
changed at the cadence boundary despite unchanged Pod UIDs; these descriptive
maxima are not a causal same-process memory comparison. [C7]

| Main Container / Replica Suffix | Baseline Max Working Set MiB | Treatment Max Working Set MiB | Treatment CPU-Service Max Cores | Valid CPU Source-Delta Pairs |
| --- | ---: | ---: | ---: | ---: |
| Packageserver `7llvg` | 171.48 | 179.66 | 0.09870 | 91 |
| Packageserver `9k6cc` | 172.81 | 174.68 | 0.09895 | 94 |
| Packageserver `b748l` | 171.24 | 192.14 | 0.09856 | 91 |
| Community registry | 62.00 | 71.18 | 0.04995 | 89 |
| Certified registry | 40.36 | 44.61 | 0.05006 | 93 |
| Red Hat registry | 38.29 | 48.30 | 0.07034 | 95 |

CPU rates use valid adjacent source-counter differences only, without bridging
missing/reset samples. Maxima are sampled service received, not runnable demand or
configured entitlement. One-minute lead samples have smaller registry maxima
(67.46/43.07/43.59 MiB); the higher values above come from the denser saved stream,
not a contradiction or values to combine across resolutions.

Memory measures are distinct: during treatment, registry maximum total cgroup
charge was **229.65/254.01/540.29 MiB** for community/certified/Red Hat, while RSS
maxima were **69.91/43.35/45.66 MiB**. File-backed/reclaimable charge must not be
called retained Go heap. Working set is not a hard-limit or irreducible-memory
proof. The earlier image rollout also replaced registry Pods and rebound mutable
catalog tags; no causal overnight-memory improvement is established. [C1, C7]

## Coverage And Finalization

The lead retained 23 one-minute metric batches: sampled `up` values were one,
maximum sampled source age 59.423s. The independent five-second exports expose
additional transient failures: **9 down target-slots during baseline, 11 during
treatment, one during recovery**, across 43 target series. Returned grids were
complete under declared per-job source-age bounds (KAS 165s, OAS 80s, others 45s).
These counts are not durations of continuous outage and do not imply every source
family was complete. The quiet test's sampled health pass is not full native
performance capture qualification. [C1, C7]

The observation-only log collector polled with timestamp overlap and exact-line
deduplication, bounded at 2 MiB per Pod UID. All three new registry logs reached
that cap **before treatment**: community 12:18:19, certified 12:19:55, Red Hat
12:22:08. Their later registry logs are missing. Fourteen earlier container-start
log requests also returned errors. Packageserver logs stayed below 50 KiB each
and extend past 13:11, covering treatment/restoration timestamps, but polling,
restart history and line deduplication are not lossless logging. The 45-refresh
claim uses the lead's separately audited packageserver records; it does not rely
on capped registry logs. Catalog RPC metrics were absent, not zero. [C2, C6]

After verifying coverage beyond final recovery, native Go finalization stopped
both metric streams and the guest-stat/log writer with bounded graceful waits.
All writer PIDs are zero; the successful baseline watcher was already finished.
Finalized **13:12:01 UTC**. The last processed metric windows end **13:10:52.975/
.982 UTC**, beyond the recovery receipt, so no backfill was needed. Preserved in
place: **4897 hash-verified metric response files** plus nine log files and five
guest-node UID files, **1,424,809,752 bytes total**. Terminal cancellation and
incomplete-series statuses remain explicit: 903 scalar and 66 histogram query
records were incomplete, with no query-error records. No duplicate archive was
created. All ten original PVC UIDs remain. [C6]

One existing Prometheus remains active with its PVC and unchanged scrape timing;
new CPO/package-server images remain deployed. Parent retention/controller/auth
services were not stopped. The test is complete; the old observation baseline
marker is historical, not permission for another load run. Durable files under
`/home` survive reboot or `/tmp` removal, but are not an off-machine backup.

The central native `image-trial-001` remains BLOCKED on its expired original
observer and lifecycle-correct historical Job/image expectations. That does not
invalidate this separately authorized bounded test, nor does this test silently
clear the iteration gate. A future performance case requires renewed capture and
reviewed historical completed-Job expectations. No one-hour claim is made. [C1]

## Evidence

Paths are relative to the perf-runner worktree. Files are private local evidence,
not distributed raw logs or public backup. Summaries retain raw-source hashes;
hash agreement establishes byte identity, not collection completeness.

| ID | Path | SHA-256 |
| --- | --- | --- |
| C1 | `_artifacts/go-small-75pct-20260915/image-trial-001/CADENCE-RESULT.md` | `7730edbfa2b0f02020ec62dab801ea3f37770e408517663efec69826461e9c55` |
| C2 | `_artifacts/go-small-controller-20260914/diagnostics/cadence-observe-001/analysis.json` | `a224e8daeca04c5f80736e6abbac9bba700b624faab497e8ca06c8f2d4c4fe0c` |
| C3 | `_artifacts/go-small-controller-20260914/diagnostics/cadence-observe-001/audit.json` | `7f63f9c7ef73ba329196668cb82b8fdf981ed71d440ec5286e19001bf0d54fba` |
| C4 | `_artifacts/go-small-controller-20260914/diagnostics/cadence-restored-001/result.json` | `5b931a99e753826b24b4e0a470b9868740e60770b35b5f3ba6a4736283b1e75b` |
| C5 | `_artifacts/dependencies/package-server-concurrency/configurable-jitter/HANDOFF.md` | `115b3deee300b4d3a3a484fddb7f4fa81837f0634a12ea16791b10f23d615c94` |
| C6 | `_artifacts/packageserver-config-jitter-20260916/observations/final.json` | `08f8a12408b44d9f0b1626572f12565a6a05bba8eb06238d3cfdcdebc7f0000e` |
| C7 | `_artifacts/packageserver-config-jitter-20260916/observations/final-20260916/resource-coverage.json` | `f42eefa1b4c071afb57bb0e177048f6159cfe1dae88840a229c141d6bd0682ed` |
| C8 | `_artifacts/go-small-75pct-20260915/image-trial-001/bindings.json` | `833546c167713e8b44cf5b8ba08a59a2a266ad3203a737eba25de7d6cb54365b` |
