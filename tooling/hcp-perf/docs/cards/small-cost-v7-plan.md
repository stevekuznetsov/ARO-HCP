# Small Cost V7: Fresh Startup

**Offline, unapplied, startup-untested candidate for a fresh attempt.** Do not
continue treating partial rollout on the old Failed RP operation as fresh-start
evidence. V1-V6 artifacts remain immutable. No live calls, application, deletion,
probe/code/image/APF changes or budget increases were performed to derive V7.

## Diagnosis And Scope

`_artifacts/small-cost-v6/lifecycle/result.json`, captured
**2026-09-11T18:09:37.453742Z**, records partial propagation on the old operation.
Old CVO still had **41Mi/46Mi**, so its repeated OOM was not evidence against
the V6 64Mi/71Mi limit. The separate old OLM Pod
`olm-operator-6756f9fbd-v27q8`, UID `61073b10-210d-4c2d-bd3a-b70d82015a7b`, had an
`availability-prober` init **OOMKilled at 18:04:57Z** after prolonged discovery
retries, with **2m/32Mi requests, 2m/36Mi limits**. Saved evidence and source
hashes are retained in the v7 allocation report. This is a long-running prober
memory failure, not an inferred catalog OOM or evidence that the budget is impossible.

The exact 92-row controlled inventory contains **17 availability-prober init
selectors**, not 24. Sixteen workloads account for **23 actual init instances**
in the 52 saved benchmark Pod layouts; `cluster-autoscaler` has no running Pod
there. All 17 selectors are explicitly configured even though their steady-state
active replica count is zero. The disabled autoscaler has no invented replica or
concurrency allowance. Kubernetes workload/container keys are unique after
dropping kind/init dimensions, so no unsupported selector extension is required.

The 17 workloads are `capi-provider`, `catalog-operator`, `cluster-autoscaler`,
`cluster-network-operator`, `cluster-policy-controller`, `cluster-storage-operator`,
`cluster-version-operator`, `csi-snapshot-controller-operator`, `dns-operator`,
`hosted-cluster-config-operator`, `ingress-operator`, `kube-controller-manager`,
`kube-scheduler`, `machine-approver`, `olm-operator`, `openshift-apiserver`, and
`packageserver`. Only init containers named `availability-prober` are covered by
this new fixed allowance, not arbitrary init containers.

## Protected Allocations

| Target | Request | Hard Limit / Policy |
| --- | --- | --- |
| All 17 named availability-prober init rows | **2m / 64Mi** | **2m / 71Mi**, Go target 53687091 bytes |
| OCM main, two replicas | **20m CPU** | CPU limit=request, modeled 20m quota each |
| Route-controller-manager main, two replicas | **20m CPU** | CPU limit=request, modeled 20m quota each |
| OAS/packageserver kas-readiness-check, six replicas | **50m CPU unchanged** | CPU limit=request |
| KCM main, two replicas | **20m CPU unchanged** | CPU limit=request |
| Ignition HAProxy, two replicas | **96Mi unchanged** | **106Mi**, non-Go exception preserved |
| CVO main | **64Mi unchanged** | **71Mi** |
| CPO main | **65Mi unchanged** | **72Mi** |
| Certified/community/redhat catalog registry CPU | **22m / 24m / 39m unchanged** | Modeled **30m / 30m / 40m** |

The 20m OCM/route allocations target startup liveness-kill evidence, not proven
minimum CPU. Catalog CPU is not blindly raised to 100m. All other CPU is scaled
from the original source weights with factor `119090909/500000000`; all other
RAM uses factor `499999999/1000000000`. Requests round down to 1m/1Mi. Protected
entries are excluded from the corresponding proportional pool. Unknown helpers
remain explicitly untested; fallback becomes **2m/31Mi**, with a **35Mi** memory
limit. Availability-prober overrides take precedence over fallback.

## Startup-Aware Cost

Cost uses the retained D16s_v6 Linux West US 3 retail price **$0.806/node-hour**,
raw 16 cores / 65536Mi, with a fixed **$0.125/HCP-hour** envelope. Downstream stays
fixed at **470m/2090Mi**; instrumentation remains outside offering totals.

| Accounting | V6 | V7 |
| --- | ---: | ---: |
| Declared controlled CPU cores | 1.739 | **1.755** |
| Declared offering CPU cores | 2.209 | **2.225** |
| Modeled effective controlled CPU cores | 2.010 | **2.010** |
| Modeled effective offering CPU cores | 2.480 | **2.480** |
| Running controlled RAM requests MiB | 7,253 | **7,036** |
| Running offering RAM requests MiB | 9,343 | **9,126** |
| Steady running controlled limits + downstream requests MiB | 10,137 | **9,906** |
| Extra from per-Pod init-stage maxima MiB | 0 | **257** |
| Conservative offering charged RAM MiB | 10,137 | **10,163** |
| Compute-equivalent USD/HCP-hour | 0.124930000 | **0.124990509033203125** |

Without RAM redistribution, the protected init increases would charge **10,382Mi**,
above the **10163.771712Mi** envelope. V7 charges the maximum concurrent regular /
restartable-init / sequential-init stage **for each Pod**, then sums those maxima
across the 52 saved benchmark Pods. It does not pretend zero steady replicas means
zero startup cost. This permits all those Pods to be in their own worst memory
stage simultaneously. Sequential init containers within a Pod are not all summed.

Remaining margins: **560/403m CPU (~1.389578m)** and **311/403Mi RAM (~0.771712Mi)**.
Dollar margin is **$0.000009490966796875/HCP-hour**. This is intentionally tight.
The next common-factor quantization step does not fit; unused slack is not
redistributed by arbitrary per-container edits.

CPU uses the observed-sample **100000us period, 1000us minimum, 1000us quota
quantum**, with parent cgroup caps and init-stage accounting. These parameters
remain a model extrapolation beyond the saved startup samples, not universal
Kubernetes semantics. The memory/cost guard covers the **saved replica layout**,
not disabled/new workloads, completed Jobs, rollout surge, management overhead,
AKS allocatable loss, actual packed-node bills or a full namespace hard cap.
Downstream usage can exceed its fixed requests. Fresh-start capture must retain
the actual inventory and independently check these omissions.

## Exact Tradeoffs

Per-container values; CPU is declared mCPU, RAM is request/hard-limit MiB:

| Container | CPU V6 -> V7 | RAM V6 -> V7 |
| --- | --- | --- |
| Availability-prober init | **2 -> 2** | **32/36 -> 64/71** |
| OCM main | **4 -> 20** | 65/72 -> **63/70** |
| Route-controller-manager main | **2 -> 20** | 32/36 -> **31/35** |
| KAS main | 144 -> **138** | 527/580 -> **511/563** |
| CPO main | 137 -> **130** | **65/72 unchanged** |
| Etcd main | 29 -> **28** | 280/308 -> **271/299** |
| Certified registry | **22 unchanged** | 214/236 -> **207/228** |
| Community registry | **24 unchanged** | 197/217 -> **191/211** |
| Redhat registry | **39 unchanged** | 313/345 -> **303/334** |

Unprotected CPU scale is about 4.7% lower; CPO's rounded request falls about 5.1%,
not zero or a hidden blanket CPU floor. KAS loses 16Mi requested memory per replica.
Catalog/etcd charge includes substantial cache in the source evidence; those RAM
weights are allocations, not hard heap floors or proof these lower limits work.
Likewise, OCM/route now have more CPU but slightly less memory and may expose a
different failure. No successful startup, supported population/QPS, or qualified
minimum is claimed.

## Verified Artifacts

| Artifact | Bytes | SHA256 |
| --- | ---: | --- |
| `tooling/hcp-perf/examples/small-cost-v7.json` | 18,062 | `46686b3c8108e81042c7e8dedd6f286c8aff3c43915f7b18f0e0a5bd9f7c6dc2` |
| `_artifacts/small-cost-v7/small-cost-v7-hypershift.json` | 11,744 | `35e91d5e5b43151410611b257c1e03070c7fc21c6831e22aded7d33fab5303e4` |
| `_artifacts/small-cost-v7/allocation.json` | 105,399 | `cb26e24d3ae437b678656c20a623185b33861777fb6f290e75067c79a23510be` |

The private directory also contains resolved requests/limits/Go env, all per-row
changes, protected key lists, init coverage and output hashes. Both sizing CLI
commands pass, all 92 controlled rows match explicit overrides or fallback, and
the full rendered CSC retains all non-resource policy settings. No duplicate
reduced workload/container keys were found. Previous profiles/wire/allocation
bytes are hash-preserved; shared renderer source changed to support the new
explicit init entries, without rewriting older reports' source hashes.

Read-only replay and tests from the perf-runner root:

```bash
python3 -B tooling/hcp-perf/scripts/cost_allocate_v7.py
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_cost_allocate*.py' -v
```

**22 allocator tests pass**, including protected memory/CPU, exact 17-init selector
coverage, duplicate mapping checks and the startup-max budget that would fail
without redistribution. Profile and private artifacts reproduce byte-for-byte.
Fresh startup and live/CEL admission were not tested here. Offline render from
`tooling/hcp-perf`:

```bash
GOWORK=off go run . sizing hypershift --profile examples/small-cost-v7.json \
  --default-cpu-milli 2 --default-memory-mib 31
```

**Fallback flags are mandatory; the retained wire object already contains them.**
Parent can use the verified artifact for its separately authorized fresh attempt,
with images/policy installed and checked before creation. This task does not
delete the failed attempt, retry its RP PUT, apply CSC, or start an observer.
