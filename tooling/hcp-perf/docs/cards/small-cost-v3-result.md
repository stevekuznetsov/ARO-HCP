# Small Cost-v3: Fresh Startup Failure

**Experimental personal-development result, September 11, 2026. Not production
qualification. Lifecycle cleanup verified terminal at 15:54:55 UTC.** [C7]

Both ignition-server-proxy HAProxy replicas were recorded `OOMKilled`, exit 137,
at **15:28:47 and 15:28:48 UTC**, with a **55 MiB memory limit**. The first retained
failure snapshot at 15:28:59 UTC showed one restart each and no observed
controlled-policy mismatch. The candidate was not advanced to guest workers.
No budget increase or probe relaxation was applied. [C3-C5]

This candidate failed the bounded fresh-startup test. It does **not** establish
that the $0.125 envelope cannot work, or that a different allocation would succeed.

## Budget And Scope

The approved [Small Cost-v3 profile](../../examples/small-cost-v3.json) uses
independent CPU/RAM allocation within a **$0.125/HCP-hour** raw compute-equivalent
budget. Its pricing reference is **Standard_D16s_v6 at $0.806/hour**, West US 3,
Linux on-demand Consumption retail, retrieved September 11 at 14:06:18 UTC.
The model uses 16 vCPUs and 65536 MiB, not allocatable capacity or demonstrated
production packing. [C1-C2]

| Allocation Basis | CPU | Memory |
| --- | ---: | ---: |
| Declared offering requests | 2.154 cores | 9370 MiB |
| Quota-aware / charged offering | 2.480 modeled effective cores | 10158 MiB |
| Included fixed downstream requests | 0.470 cores | 2090 MiB |

The quota-aware modeled cost is **$0.12493/HCP-hour**. These totals describe the
saved benchmark layout used for allocation, **not the incomplete startup's live
footprint or an invoice**. Controlled memory is charged at its rounded hard limit;
downstream memory is charged at unchanged requests, not a proven usage ceiling.
Experimental instrumentation is excluded from the offering model. [C1]

Projector-controlled regular/init containers use **CPU limit = request**,
**memory limit = 110% of request rounded up to MiB**, and **Go memory target = 80%
of request**, with explicit non-Go exceptions. Fallback requests are **2m CPU /
33 MiB**. Downstream operands are excluded from this enforcement, but their fixed
requests remain in cost accounting; this is not an all-HCP hard resource cap. [C1-C2]

Execution used the unchanged **Standard_D4s_v3 management topology**, not D16s_v6
benchmark hosts. The failure handoff lists four management worker instances across
three worker pools, with system/infra pools separate. No guest node pool was
submitted. Do not infer D16s_v6 performance from this run. [C4]

## Observed Failure

Values below are per container from the first failure snapshot. Quota equivalents
are ceilings derived from captured quota/period pairs, **not measured CPU service**.
No whole-inventory live quota total is asserted. [C3-C5]

| Container | CPU Request / Declared Limit | Observed Quota Equivalent | Memory Request / Limit | Runtime And Outcome |
| --- | --- | --- | --- | --- |
| HAProxy, both ignition proxies | 2m / 2m | 10m each; container and Pod parent | 50 / 55 MiB | Non-Go; no Go override; both OOMKilled, exit 137 |
| Control-plane operator | 160m / 160m | 160m | 66 / 73 MiB | GOMEMLIMIT 55364812 bytes; not the recorded first-OOM trigger |
| Etcd main | 35m / 35m | Not asserted here | 284 / 313 MiB | GOMEMLIMIT 238236467 bytes; not the recorded first-OOM trigger |
| Fallback availability-prober init | 2m / 2m | Not asserted here | 33 / 37 MiB | GOMEMLIMIT 27682406 bytes |

HAProxy's captured quota was **1000 microseconds per 100000-microsecond period**,
equivalent to 10m despite the admitted 2m limit. CPO was 16000/100000, or 160m.
This supports the minimum/quantum distinction in the economic model for these
sampled containers, not a universal quota rule. [C1, C5]

The observed OOMs occurred under tight memory hard limits. HAProxy is non-Go;
this is **not evidence of a Go GC or GOMEMLIMIT defect**. The saved status does not
provide an exact required-memory peak, a sufficient replacement limit, or isolate
CPU starvation as a cause. Short quiet memory observations and proportional
budget allocation are different inputs; a quiet observation is not a startup
memory floor. This run does not prove that a particular allocation recipe is
generally invalid, only that this deployed candidate hit the recorded boundary.

## Tests Not Run

| Planned Stage | Result |
| --- | --- |
| Fresh constrained startup | Failed at the first observed HAProxy OOM boundary |
| Guest provisioning and three-app validation | **NOT RUN**; no guest credentials or workers |
| 60/150/300/450/600-Pod population screens | **NOT RUN** |
| Low-rate 1/5/10/15/30-QPS characterization | **NOT RUN** |
| Selected one-hour confirmation and turnover of every app | **NOT RUN** |

There is **no measured zero-QPS result**, throughput limit, supported Pod count,
or latency/SLO conclusion. The reusable campaign plan remains offline-only and
non-runnable (`planningOnly:true`). The parent reports a current rerun of **34
Python campaign tests passing in 0.481 seconds**. The preparation handoff also
records prior full-module race-test/vet passes; no new full-module run or live
qualification is claimed by this documentation update. See the
[campaign contract](../../scripts/CAMPAIGN.md). [C6]

## Evidence And Lifecycle

The startup observer began at 15:22:09 UTC. Twenty empty namespace Pod snapshots
preceded the first observed three Pods, created at 15:26:11 UTC. The capture audit
records 37 namespace snapshots through failure, zero reported policy mismatches,
and 67 quota/period samples collected through an existing read-only node proxy.
Pod resources, templates, events and OOM statuses were retained; attempted
failure logs were empty. Polling is not lossless logging or continuous memory
measurement. No workload generators, CPU adversaries or dedicated Prometheus
were launched for this candidate. [C3-C5]

At the original 15:32:42 UTC reporting checkpoint, the failure handoff still showed
the create operation Provisioning and its bounded waiter/forward active, with all
46 capture PVCs retained. That historical checkpoint remains unchanged; a client
wait timeout would not itself cancel server-side creation. [C4-C5]

The lifecycle owner's subsequent **15:54:55 UTC terminal cleanup result** records
RP deletion Succeeded, RP GET 404, no remaining Clusters Service entries, no
remaining HC/HCP/NodePool resources or target namespaces, and the managed resource
group absent. All **46 capture PVCs** and customer/underlay resources were preserved;
CSC and budget were unchanged, and cleanup used no force. Create, startup, cleanup
and RP-forward units were verified inactive. Cleanup is complete, while the startup
failure remains a failed result. No lifecycle, capture, or workload operation was
performed to update this report. [C7]

## Next Decision

Two possible experiments require a separate decision and a new candidate:

1. Reallocate per-component CPU/RAM ratios within the same $0.125 budget, informed
   by startup evidence and explicit memory floors. Reprice quota rounding and
   downstream demand; do not simply raise HAProxy memory outside the envelope.
2. Evaluate an Es_v6 memory-rich economic model as a separate CPU/memory/price
   tradeoff. Verify exact pricing and capacity, then test on the intended host
   configuration before making performance or packing claims.

Neither option was applied or shown sufficient. Preserve failed-attempt evidence
and the terminal cleanup receipt, and validate any new candidate from fresh
startup before population, QPS, or hour-long confirmation.

## Evidence Index

All paths below are **private, repository-relative local evidence**, not portable
download links. Let `R` be `_artifacts/small-cost-v3/`. Hashes identify reviewed
bytes, not complete capture or off-machine backup. No credentials, resource IDs,
private addresses or logs are embedded in this card.

| Citation | Path | SHA-256 |
| --- | --- | --- |
| C1 | `R/allocation.json` | `beb18ed9fe004491d45710d8d33986f5e44bc16d587b227056aeeab12c820e93` |
| C2 | `_artifacts/small-cost-v1/prices.json` | `5eb154b47fa2b62fcc58f1ef0b466de11a2190a1ca29f20a34dafe83c54b5614` |
| C2 | `R/small-cost-v3-hypershift.json` | `c8e3be473898c8925b0e0e37e34f5d30fc1b14364beae670bdb08505eeebb313` |
| C3 | `R/lifecycle/failure.json` | `665e2fefce31383f14119eecc4a26f9c8962e1b74b311d1a3a0b68c27729a6bb` |
| C3 | `R/lifecycle/startup/044/hcp-0/pods.json` | `e23c9ab782891d1d889de5b4da7343d583d5e8c6dc6822580f071101707c7e5d` |
| C4 | `R/lifecycle/failure-handoff.json` | `0df404afe0258b6d0a9a81ea6fe9f5d35324ed15104e6c5d34ce6021792814f6` |
| C5 | `R/lifecycle/capture-audit.json` | `fb70af46985ab64f9c607d53a5df49ff0447aff4c0a907c60969c02283d51c72` |
| C5 | `R/lifecycle/startup/044/hcp-0/quotas.json` | `d67ecec7ba0e7d794c48ee835531d3bd536018200bbb377c6c7acb40d16c67cc` |
| C6 | `R/campaign/plan.json` | `3ca8c69649d67eb4daa92cbfc65119de17060923208fcc9588d815292c17e46a` |
| C6 | `R/campaign/HANDOFF.md` | `d46343375343b68e1ffedebb3e8c5ffd91cb9cea915a9ba35084f599e44c5ed3` |
| C7 | `R/lifecycle/cleanup/result.json` | `6a916f3088246c4ab66c4a637b0ff6e6ae600a0541aa1af145415c5d83aa4e2e` |
