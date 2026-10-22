# Small Cost-v4 Progress: HAProxy Improved, Startup Pending

**Experimental, not production-qualified. Latest recorded v5 continuation snapshot:
September 11, 2026, approximately 17:26 UTC; handoff written 17:26:47 UTC.**
V4 avoided the [v3 HAProxy OOM boundary](small-cost-v3-result.md) in its observed
startup window, but did not complete cluster startup. V5 is a partially rolled-out
CPU diagnostic applied to that existing v4 HCP, **not a fresh v5 startup**. [C1-C7]

## HAProxy Outcome

| Observation | Memory Request / Limit | Sampled Container Charge | Result |
| --- | --- | --- | --- |
| Original Small reference | 64 / 192 MiB | Peak 72.867 MiB | Historical measurement, not a startup minimum [C1] |
| Cost-v3 | 50 / 55 MiB | No sufficient peak claim | Both ignition-proxy HAProxy replicas OOMKilled; see prior card |
| Cost-v4 | **96 / 106 MiB** | **72.305 / 72.289 MiB**, one peak per replica | Both Ready, zero restarts, no observed OOM at the bounded handoff [C2-C3] |

The v4 HAProxy CPU request/limit remained 2m; its non-Go exception was preserved.
This is a successful targeted result over the observed window, not proof that
96 MiB is the minimum, that memory is sufficient at workload scale, or that the
whole cluster is fixed. Metric collection recorded 859 requests and eight errors;
cached cAdvisor samples and polling gaps mean these are observed maxima, not
lossless peaks or simultaneous whole-HCP totals. [C2-C3]

## Fixed Budget

Both candidates retain the **$0.125/HCP-hour** raw compute-equivalent budget using
the D16s_v6 **$0.806/hour** pricing model. Actual execution remains on D4s_v3
management hosts, not a D16s_v6 performance benchmark. [C1, C5]

| Modeled Offering | V4 | V5 |
| --- | ---: | ---: |
| Raw compute equivalent, USD/HCP-hour | **0.124930** | **0.12436328125** |
| Memory requests / charged memory, MiB | 9320 / 10112 | 9320 / 10112 |
| Declared / modeled effective CPU, cores | 2.154 / 2.480 | 2.187 / 2.460 |

V4 fixed HAProxy memory and redistributed other memory within the budget. V5
preserves every v4 memory request/limit and Go target; it reallocates CPU only.
Controlled CPU limits equal requests and memory limits remain rounded 110%, with
Go targets at 80% where applicable. Unchanged downstream requests of **470m CPU /
2090 MiB** remain in cost accounting but outside projector enforcement; excluded
instrumentation and downstream consumption above requests are not free or bounded
by this model. Totals describe the saved benchmark allocation, not the current
mixed rollout's measured cost or a namespace-wide hard cap. [C1, C5]

## Readiness Diagnosis

V4 was `startup-not-converged-at-bound` at 16:50:12 UTC with no observed OOM.
The 16:58:09 diagnostic result found all three OAS main containers Ready, but
their `kas-readiness-check` sidecars withheld all OAS endpoints from Service
routing. The unchanged five-second bash/curl KAS livez probe completed in
**13.099 seconds wall time**, using **0.065 seconds user CPU**, at **5m declared /
10m observed quota-equivalent CPU**. Diagnostics recorded **769/772 throttled
periods**. Direct checks from CPO returned KAS readyz HTTP 200 in 0.373 seconds
and OAS Pod readyz HTTP 200 in 0.428 seconds, while the OAS Service had zero ready
endpoints. [C3-C4]

This supports the helper CPU/probe-path diagnosis, not a general OAS crash or
universal minimum CPU allowance. Diagnostic exec adds CPU load; throttled seconds
are not a wall-clock outage duration. The comparison used the existing probe's
TLS behavior, not an independent certificate-validation test. [C4]

## V5 Applied, Not Confirmed

V5 assigns **50m request/limit** to the OAS and packageserver readiness helpers,
with other controlled CPU redistributed under the same budget. Probe deadlines,
images, memory and Go policy remain unchanged. Shared Pod-parent quotas still
limit delivered CPU; a 50m child limit is not guaranteed CPU service. [C5]

The owning policy propagated, but the 17:19:41 result retained six template CPU
mismatches during dependency-gated rollout. The later snapshot supersedes its
KAS status: **all three KAS replicas completed rollout at 17:24:39 UTC**. OAS and
packageserver replacements were still Pending at the approximately 17:26 snapshot;
the new OAS template contains the 50m helper, but **no running 50m OAS helper was
measured**. OAS/packageserver ready endpoint counts remained zero. [C6-C7]

The recorded OAS scheduler reason excluded three nodes for untolerated taints and
four for required anti-affinity while older rollout Pods were terminating. The
underlying reason for the continued overlap was not established. This checkpoint
does **not** prove a permanent deadlock, insufficient host capacity, or failure of
the 50m helper treatment. No full-HCP readiness or fully enforced v5 cost claim is
justified. Fresh v5 startup confirmation is still required after any successful
diagnostic rollout because this treatment reused v4 bootstrap state. [C7]

## Paused State

The parent stopped further investigation for this turn because feedback had become
prolonged. **Guest workers, application/population/QPS tests, and one-hour turnover
confirmation are NOT RUN.** There is no measured zero-QPS or capacity verdict.

At the latest recorded handoff, the HCP remained live and the original RP operation
was Provisioning; no guest credentials were issued. The observer was inactive,
with no new long-running diagnostic worker. The existing RP forward remained
active with its previously recorded expiry around **20:30 UTC**. No automatic
delete, recreate, capacity change, or long test is implied. Previous evidence and
46 historical capture PVCs were recorded preserved. State can change after this
snapshot; this document was written without live operations. [C6-C7]

## Evidence Index

Private repository-relative paths, not public download links. Let `R4` denote
`_artifacts/small-cost-v4/` and `R5` denote `_artifacts/small-cost-v5/`. Hashes
identify reviewed bytes, not complete capture or qualification. Raw IDs, addresses,
credentials and logs are intentionally omitted.

| Citation | Private Path | SHA-256 |
| --- | --- | --- |
| C1 | `R4/allocation.json` | `f4d80e5d9d301ce6b54bd92934f1fa3792b86269c21a676a6fd4edf494fae16a` |
| C2 | `R4/lifecycle/metric-peaks.json` | `6daa0daa8475ae5e6e9254f8bf15b488fa4edf4b44ad2c66b593d1becf046652` |
| C3 | `R4/lifecycle/bounded-handoff.json` | `1fffda3e686296f54681cb61b50be648a852e0c84c50df082f809755a6403165` |
| C4 | `R4/lifecycle/diagnostic-continuation/result.json` | `b34fb455018f4068642d9b12576c2baae2254cda9a7ec3c6a771749f5a5336da` |
| C5 | `R5/allocation.json` | `2d2f77364603f1ea60bcdc62959083961f21ff561342cce63688ab47f042c56d` |
| C6 | `R5/lifecycle/result.json` | `0b1d08903e3f85e9bef68b9986234a5a69ccaa671adf10c799e7856ffd2123e7` |
| C7 | `R5/lifecycle/continuation/latest-result.json` | `31880a6b6caa41602869a7ec58977a0186c3d9c16df201c93c28fc66f6a700e3` |
