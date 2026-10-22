# Small75 Loaded And Unloaded Envelope

## Latest Diagnostic Hour

The [five-QPS observation hour](small75-observe5-hour.md) completed one full
**300-Pod hour plus 600 seconds of recovery** under explicit `observe-degradation`
policy. **2.0111% union request degradation met the unchanged 5% threshold;
health assessment FAILED for OAS/packageserver degradation.** All 60 rollouts
completed; no new restart, OOM or hard-canary failure was observed.

The portable card includes per-operation latency, the unloaded comparison,
sampled availability and safety-review limitations, current retained state and
artifact hashes. Earlier strict outcomes and historical snapshots below remain
unchanged; they are not the current fixture state or retroactively rescored results.

## Earlier Strict Epoch

September 17, 2026. **Loaded five-minute screens accepted 300 Pods plus 4 or 5
offered QPS under the declared 5% request threshold. Both longer confirmations
failed hard requirements: canary cleanup at 5 QPS and controller health/capture
at 4 QPS. Neither rate is hour-qualified.**
The unloaded comparison accepted 10 QPS for five minutes; 15 QPS exceeded 5%.

## Policy

The opt-in request policy accepts an inclusive maximum of **5% of dispatched
logical requests receiving HTTP 429 OR timing out**. Union accounting prevents
double-counting a request with both signals. Separate counts are retained below.
Other HTTP/transport/body errors, generator limitation, incomplete delivery,
unfinished rollouts and hard health/canary failures remain unacceptable. Soft
latency degradation is recorded, not silently promoted to hard failure. Omitted
policy preserves historical strict behavior; old outcomes were not rescored.

Resources, images, probes, HA and catalog pins stayed fixed: CPO 144/159Mi,
three KAS audit containers 39/43Mi, **3720m / 15241Mi charged**, exact modeled
cost **6142123/32768000 USD/HCP-hour**, below $0.1875. This is quiet testing,
not qualification under the previously failed host-contention scenario.

## Comparison

All rows below used five-minute offer windows, the same three namespaces and
Deployment GET : Pod LIST : ReplicaSet LIST mix of 2:1:1, 128 maximum in-flight
requests, ten-second request deadline, and no API retries. Successful-body QPS
includes final request drain; p95 includes successful responses only.

| Population | Offered QPS | Dispatched | Success | 429 | Timeout | Combined | Success QPS | Success p95 | Result |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 300 Pods, restored | 4 | 1200 | 1189 | 11 | 0 | **0.92%** | 3.940 | 4.348s | Screen accepted; 5/5 rollouts |
| 300 Pods, repeat | 4 | 1200 | 1196 | 4 | 0 | **0.33%** | 3.949 | 4.264s | Screen accepted; 5/5 rollouts |
| 300 Pods | 5 | 1500 | 1456 | 39 | 5 | **2.93%** | 4.788 | 6.290s | Screen accepted; 5/5 rollouts |
| 300 Pods | 7 | 2100 | 1943 | 155 | 2 | **7.48%** | 6.347 | 6.418s | Threshold exceeded; only 3/5 rollouts completed |
| 0 app Pods | 5 | 1500 | 1486 | 14 | 0 | **0.93%** | 4.945 | 2.123s | Accepted |
| 0 app Pods | 7 | 2100 | 2074 | 26 | 0 | **1.24%** | 6.910 | 2.407s | Accepted |
| 0 app Pods | 10 | 3000 | 2889 | 111 | 0 | **3.70%** | 9.538 | 2.277s | Accepted |
| 0 app Pods | 15 | 4500 | 3809 | 691 | 0 | **15.36%** | 12.559 | 4.321s | Threshold exceeded |

These bracket tested points, not precise maxima: loaded 6 QPS and unloaded
11-14 QPS were not tested. No higher loaded rate followed the 7-QPS failure.

## Loaded Confirmations

The 5-QPS confirmation began measurement at **18:29:03 UTC** and stopped around
**18:58:51**, before completing an hour. A canary scale-down request was accepted
but its Pod remained after the **120-second hard cleanup deadline**. A controller
restart was recorded; no OOM was recorded. Of 8,941 dispatched requests, 8,730
succeeded, 155 were shed, 49 timed out, and seven were canceled by the stop.
The observed union fraction was 2.28%, but hard failure and incomplete execution
make capacity **inconclusive/unusable**, not acceptable. Only 26/60 planned
rollouts completed. The 5% allowance does not override this failure.

After the unloaded comparison, a native guarded restore scaled the same 60
Deployment UIDs from zero to five replicas and waited for all 300 Pods before
warmup. Two 4-QPS five-minute screens passed with no new restarts or OOMs.
These were separate windows, not an uninterrupted ten-minute qualification.

The subsequent **4-QPS hour failed**. API measurement ran **20:41:27-20:57:36
UTC**, about 16m09s of the planned hour; final outcome was written at 20:57:48.
Of **3,875 dispatched requests**, 3,828 succeeded, 35 received 429, five timed
out, and seven were canceled. The union fraction was **1.03%**, successful p95
**5.431s**, with no generator limitation or other HTTP/transport/body errors.
Only **15/60 rollouts completed**, two failed at cancellation and 43 never
started. Fourteen measured canaries completed; ten exceeded soft expectations.
The planned full-hour measurement and 600-second recovery were not completed.

At 20:57:09 the safety record reported OCM and packageserver not ready, one new
restart, and no new OOM. At 20:57:36 the OCM capture source exceeded its sustained
loss bound. This was not merely a transport outage: Kubernetes events corroborate
OCM liveness failures and packageserver KAS-readiness timeouts. The observed
request fraction cannot override hard health failure or incomplete delivery.

Read-only follow-up found OCM Pod `openshift-controller-manager-6fcbd47469-dvmf8`
at **14 total restarts**, versus ten at baseline and eleven in the final recorded
Pod sample. Thus four additional restarts were observed across measurement and
post-stop recovery, not just the one visible to the frozen outcome. Its latest
termination at 20:59:53 was exit 255 after SIGTERM during API connectivity checking;
a matching liveness-kill event was present. This does not establish the underlying
CPU/API bottleneck or a memory failure. Packageserver KAS-readiness timeout events
continued through 21:07 despite a 66/66 Ready snapshot at 21:08:40. Recovery is a
point-in-time observation, **not a clean sustained recovery window**.

The earlier clean 1-QPS hour remains a separate result in
[Small75 Load Results](small75-load-results.md). Neither historical successes nor
this new threshold retroactively erase earlier failures.

## Unloaded Method

After read-only verification of failed-case finalization, current object UIDs and
recorded rollout-marker changes, a native guarded step scaled the same 60 owned
Deployments from five replicas to zero. It retained Services, ConfigMaps, synthetic
Secrets, namespaces, scoped identities and APF configuration. Per-object UID/RV
patch intents/responses were recorded. It waited for zero application Pods before
measurement. Application turnover was explicitly disabled; canaries still ran.

This is a **zero-application-Pod comparison**, not an empty cluster. GET targets
are preserved, but Pod LIST payloads are smaller and application rollout activity
is absent. The sequential tests also retain cluster/controller history. Therefore
the difference is not attributable solely to idle Pod count or a randomized causal
experiment. Each case refits soft canary expectations; no fixed latency SLO is
implied. Canary cadence waits 30 seconds after completion, not fixed launch slots.

## Evidence And Final State

Artifacts are under `_artifacts/small75-envelope-20260917/` and the durable state
root `_artifacts/go-small-controller-20260914/state/`. Every case preserves its
spec, outcome, request events, turnover results, population hashes and metrics.

| Run | Performance Attempt |
| --- | --- |
| envelope-loaded5-001 | f66f87981159fc31d3dfb81e1dbbe045 |
| envelope-loaded7-001 | 1bd9c6945442ba2acda9bf727d40a6fc |
| envelope-loaded5-hour-001 | cc4156caf619a361834bf9df3b9001fc |
| envelope-unloaded5-001 | a3154b7b52081709749232b88c33750f |
| envelope-unloaded7-001 | f367f28df53d92709021c568f9d9dbe3 |
| envelope-unloaded10-001 | 851731cb793adf6ee9ba56132fde32b1 |
| envelope-unloaded15-001 | a1fde1cde566fa5e5cbb424a3981fac8 |
| envelope-loaded4-001 | 07310d76b27268382d9c74cad3684952 |
| envelope-loaded4-002 | 9372dc3019ba61ff8575bcbacb48e24b |
| envelope-loaded4-hour-001 | 42728f2691d2043322cc9a7efdf9ff67 |

Observation reused one Prometheus, renewed a scoped token, and kept scrape timing
unchanged. An initial forward failed during the token rollout; the failed readiness
attempt is retained. A later five-minute source check passed before load. Native
per-case source-history gates and final exports remained active; partial metric
families/record coverage are not described as lossless or zero-filled.

At **21:09:35 UTC**, all 60 app Deployments had **300 Ready replicas**, the same
three guest node UIDs were Ready, and the canary had zero replicas and zero Pods.
Objects and HCP are preserved; the failed performance worker is no longer active,
and no new load or competitor was started. Live CSC spec equals the approved wire;
HC/HCP catalog pins and registry/extraction images are unchanged, with no triggers.

`envelope-loaded4-hour-001` remains orchestration **BLOCKED**, with a finalized
failed performance outcome and its claim preserved. Its retention action remains
RUNNING, last nine-RG verification **21:09:11 UTC**, nominally through **00:39 UTC
September 18**. The ARM rotator is active but its current systemd runtime limit
ends about **22:03 UTC**, earlier than retention; the last checked token expires
**22:35:20 UTC**. Later retention coverage requires fresh auth verification, not
an assumption based on keeper duration. No lifecycle stop/replay or cleanup was
performed to release the claim. Detailed follow-up is in
`_artifacts/small75-envelope-20260917/LOADED4-HOUR-RESULT.md`.

Safe claim: **60 five-Pod applications plus 4 or 5 offered QPS met the 5% union
threshold in five-minute screens, but neither sustained the hour's hard requirements.**
The zero-app-Pod comparison met the same threshold at 10 QPS for five minutes.
