# Small75 Five-QPS Observation Hour

**One full 300-Pod, 5-QPS hour completed, followed by 600 seconds of recovery.
Request quality met the unchanged 5% union threshold at 2.0111%; the health
assessment FAILED for observed control-plane degradation.** This is a completed
diagnostic measurement, not a clean hour or packed/contention qualification.
Historical outcomes remain immutable; subsequent offline safety fixes do not
retroactively change this run.

## Contract

Run `envelope-observe5-hour-004` used the explicit
`stabilityPolicy: observe-degradation` and
`requestQualityPolicy: {maxDegradedBasisPoints: 500}`, with no early screening.
Ancillary degradation was recorded without individually ending measurement;
critical availability, identity, resource/image drift, node safety, critical
capture and unresolved canary cleanup remained declared stop conditions.

The fixed workload was 60 five-Pod applications across three namespaces, 60
rollouts/hour, and Deployment GET : Pod LIST : ReplicaSet LIST weights of 2:1:1.
Maximum in-flight requests was 128, request deadline 10 seconds, schedule seed 42,
and canary cycles were serial with a 30-second pause after completion. Measured
clients remained namespace-scoped; authenticated `/livez` was observer traffic.

HCP CPU, limits, images, probes, HA, cadence and APF parameters were unchanged:
**3720m / 15241Mi charged**, CPO **144/159Mi**, three KAS audit containers
**39/43Mi**, modeled cost **6142123/32768000 USD/HCP-hour < $0.1875**. Release
4.21.32 and the approved CPO/packageserver/prober images were retained; all three
catalogs remained digest-pinned. These are modeled HCP costs, not guest VM costs
or evidence of packing efficiency.

## Delivered Work

All times below are UTC on September 17, 2026.

| Window | Time |
| --- | --- |
| Measurement phase receipt | 22:48:29.130 |
| Actual API offer window | **22:48:41.559-23:48:41.560**, 3600 seconds |
| Final API body completion | 23:48:47.544 |
| Turnover recovery completion | 23:58:41.543 |
| Final recovery canary completion | 23:58:58.605 |
| Final outcome | 23:59:23.932 |

Use the actual generator window, not the earlier phase receipt, for API timing.

| Metric | Result |
| --- | ---: |
| Offered / dispatched / completed logical requests | 18000 / 18000 / 18000 |
| Successful bodies | 17638 |
| HTTP 429 / timeouts | 308 / 54 |
| Union degraded requests / dispatched | **362/18000 = 2.0111%** |
| Canceled / other HTTP / transport / body-limit errors | 0 / 0 / 0 / 0 |
| Generator-limited requests | 0 |
| Successful-body QPS, including drain | **4.891312** |
| Successful p50 / p95 / p99 | **1.971s / 5.382s / 7.622s** |
| Rollouts scheduled / accepted / completed | 60 / 60 / 60 |
| Failed rollouts / backlog / outstanding | 0 / 0 / 0 |
| Measured canaries / soft-degraded / hard failures | 48 / 9 / 0 |
| Observed new restarts / OOMs | 0 / 0 |

The union counts each logical request at most once; its denominator is dispatched
requests, not HTTP attempts. Latency percentiles include successful bodies only.
The final recovery canary was within its fitted soft expectations.

| Operation | Dispatched | Success | 429 | Timeout | Success p50 | Success p95 | Success p99 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Deployment GET | 8954 | 8905 | 49 | 0 | 1.622s | **3.924s** | 5.215s |
| Pod LIST | 4490 | 4292 | 158 | 40 | 2.897s | **6.919s** | 8.837s |
| ReplicaSet LIST | 4556 | 4441 | 101 | 14 | 2.214s | **5.428s** | 7.408s |

## Degradation

The frozen outcome reports `executionStatus: completed`, `workload.fullWindow:
true`, request-quality `status: degraded` / `capacityStatus: acceptable`, and
workload `status: violated`. Its sole workload violation is
`control-plane degradation observed during fixed window`. The native performance
action is **FAILED as assessed**, not an interrupted execution. The enclosing run
remains RUNNING while its retention action runs; that does not mean load continues.

During the actual API hour, **15/189 safety samples (7.94% of samples)** were
unhealthy. OAS appeared in 10 and packageserver in seven, with overlap; OCM and KCM
appeared in none. This sample fraction is **not an uptime percentage**. Across
measurement and recovery, the stability summary contains 222 checks, 15 unhealthy
samples, no new restart/OOM, and 1703 probe-failure increments. Event increments
are not distinct outage episodes or per-component downtime.

| Signal Across Measurement And Recovery | Observed Range | Frozen Interval Estimate |
| --- | --- | --- |
| KAS live Ready endpoint Pod UIDs | 3 at every check | 0s below two |
| Etcd live Ready endpoint Pod UIDs | 3 at every check | 0s below two |
| OAS live Ready endpoint Pod UIDs | 1-3 | 18.866s below two |
| Packageserver live Ready endpoint Pod UIDs | 2-3 | 0s below two, not full readiness |
| Authenticated guest `/livez` | Both success and failure | About **41.041s** estimated sampled failing intervals |

The interval accumulator starts at the first sampled failure and adds elapsed
sample time when success is next observed. `/livez` uses 0/2 internally to encode
failure/success; these are not endpoint counts. Its roughly 41-second sum spans
**measurement plus recovery**, not the API hour alone. It is neither precisely
measured downtime nor a percentage with a validated time denominator. Individual
`/livez` results and incident timestamps were not preserved, so its maximum
consecutive failing interval and hour-only success fraction are not reconstructible.
Etcd readiness is a quorum proxy, not direct member/leader proof.

## Safety Limits

The configured safety pause was **5 seconds**, but serial Kubernetes inspection
and capture queries made actual API-hour safety sample spacing **17-22 seconds,
mean 19.085 seconds** using second-rounded timestamps. This run therefore did not
provide continuous 5-second checking or validate a guaranteed 30-second stop bound.
No critical stop fired; that is not proof that no shorter outage occurred.

There were **zero hard-canary recovery incidents and no observed new restarts or
OOMs**. Consequently, the continuation/recovery branches for those failures were
not live-validated. Offline review identified safety issues, including canary
ReplicaSet drain validation; fixes are separate work required before future load.
The actual plan explicitly named all five intended critical jobs:
`kube-apiserver`, `etcd-client`, `cadvisor`, `node-exporter`, and `kubelet`.
That correct input does not validate rejection of unsafe critical-job selections.
No safety-code changes or further load were made while preparing this card.

Capture retained all 43 targets across 23 jobs. History recorded 30 warnings,
three recoveries and `incomplete: true`; final metric exports were partial and
the recorder was `recorded-not-lossless`. Missing series and lifecycle gaps are
not healthy zeroes. Separate observation-owner finalization is not inferred from
the completed performance action.

## Unloaded Comparison

The earlier unloaded 5-QPS **five-minute** screen had 1486/1500 successes, 14
HTTP 429s, no timeouts, 0.93% union degradation and successful p95 **2.123s**.
This loaded hour's p95 **5.382s** is **2.535 times** that reference.

This is not a matched-duration or randomized Pod-count comparison. Unloaded app
turnover was disabled, LIST bodies were smaller, and this hour used a fresh
`observe5-seed-001` fixture after UID-guarded cleanup of the old population.
Controller/etcd history was not reset. The safety pause changed from 15 to 5 seconds
and external observation resumed, although the API mix and APF parameters stayed
fixed. Those differences limit causal attribution.

## Retained State

Read-only checks on **September 18 at 00:07 UTC** found 66 control-plane Pods Ready,
60 app Deployments with 300 Ready replicas, the same three guest node UIDs Ready,
and canary desired/actual replicas and Pod count all zero. API and turnover workers
finished; no new producer was started. HCP, fixture, PVCs and evidence are retained.

Retention attempt `b95adc23887a16ef2f1d5240ea139d26` was active at review. Its last
checked receipt, **September 17 23:59:03.124474447 UTC**, verified all nine approved
RGs in the existing `pers-usw3stev` subscription scope. Nominal keeper end is
September 18 02:39 UTC; this is not a guarantee of future successful refreshes.
ARM rotation was previously extended to a nine-hour bound from 21:40:49 UTC;
guest certificate expiry is September 18 21:53:14 UTC, metrics token expiry
03:40:57 UTC. No service was restarted or extended for this documentation task.

## Evidence

Run: `envelope-observe5-hour-004`; performance attempt:
`0708890f44c6a9751c09090a8431408f`. Files below are under
`_artifacts/go-small-controller-20260914/state/envelope-observe5-hour-004/0708890f44c6a9751c09090a8431408f/performance/`.

| Artifact | SHA256 |
| --- | --- |
| `outcome.json` | `21526384cb678c21164dd0a6cf66e85f9bb1f0cc68b93d8f4d165e7021b96b01` |
| `results.json` | `829895b8e39aa7a9c1704830b9a0add13bac3307730d39b514957fa1912779fd` |
| `spec.json` | `ebafc581b5e307ee8b02645eac46d1794c34afab9838ae52da6845485f4efb47` |
| `events.ndjson` | `aa41efbc1d8bd56bca65d147746c4f3031341f941a2e65058b7cae5f60f1dc1f` |
| Frozen run worker | `16cbd73629078e6e69d11fbe51f70e130c664b5b61e678f6eb69c4cc20fb71a9` |

The enclosing iteration spec SHA256 is
`9195851b877ac49ff1b6d18028119f1bd25f1fd96312132b323b558cf6f11f4a`.
The retention receipt cited above has SHA256
`87f671112ea557c4d3c9e918415cb2dd8a457d3b95203ede4dc0e50d59a6ed5e`
when serialized by `jq -s -cS 'last'` including its trailing newline; this hashes
that receipt, not the growing retention journal or a future refresh.

Preparation chronology is retained in
`_artifacts/small75-envelope-20260917/OBSERVE5-HOUR-RESULT.md`.
Original failed attempts, outcomes and binaries were not rewritten. This card
was checked against frozen results and live read-only status; no new load, code
tests or CI run was performed for these documentation-only changes.
