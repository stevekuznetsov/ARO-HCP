# Cost-First Small: Storage Memory V8

**Current candidate: [Small Cost V8](small-cost-v8-plan.md).** The fresh V7 run
reached the next diagnosed storage-operator OOM at 35Mi. V8 fixes that main
container at 64Mi/71Mi, preserves all V7 CPU and all other memory, and remains
within $0.125: its existing 71Mi init reservation absorbs the increase without
cuts elsewhere. The new report also ranks historical RSS/working-set memory risks
without automatically allocating floors. V8 is offline and unapplied.

**Current candidate: [Small Cost V7](small-cost-v7-plan.md).** It explicitly
allocates 64Mi/71Mi to all 17 controlled availability-prober init selectors,
20m to OCM/route mains, preserves diagnosed fixed allocations, and funds these
changes within $0.125 using per-Pod init maxima. Fresh-attempt only, unapplied.
The V6 and earlier derivations below remain historical; partial propagation on
the old Failed RP operation did not qualify their startup behavior.

## Latest V6

**V6 is ready as a verified artifact for the parent's authorized treatment, not
applied by this offline task and not startup-qualified.** Spend V5's existing
slack on exactly two diagnosed main-container changes. No CPU or memory
rescaling is necessary; all **90 other selector rows remain exactly V5**.

| Target | Replicas | V5 | V6 | Added Charged Resource |
| --- | ---: | --- | --- | --- |
| CVO main memory request/limit | 1 | 41Mi / 46Mi | **64Mi / 71Mi** | **25Mi** |
| KCM main CPU request/limit | 2 | 9m / 9m, effective 10m each | **20m / 20m, effective 20m each** | **20m total** |

CVO CPU remains 44m declared / 50m modeled quota. CVO GOMEMLIMIT changes from
**34393292 to 53687091 bytes**, still 80% of request, as a derived consequence of
the memory change. KCM memory stays 98Mi/108Mi. Both HAProxy replicas remain
96Mi/106Mi and all six OAS/packageserver readiness helpers remain 50m. Fallback
stays **2m/32Mi**. No catalog adjustments, init floors, probe/code/image/APF changes,
or global budget increases are included.

### Recovery Evidence

`_artifacts/small-cost-v5/lifecycle/recovery/result.json`, captured
**2026-09-11T17:52:30.055504Z**, reports the 50m readiness helper now completing
the unchanged probe in **1.5s**, with **three ready OAS endpoints**. This supports
the previous targeted change, not complete startup success; packageserver still
has zero ready endpoints in that snapshot.

CVO was **OOMKilled at 17:48:29Z** during payload application under 41Mi/46Mi.
The historical retained charge peak is about **68.87Mi**, from
`_artifacts/small-constrained-v1/analysis/candidate-budgets.json`'s exact CVO main
selector. Its former 64Mi request/192Mi limit ran, but the proposed **71Mi limit
is only a bounded hypothesis**, not a proven startup minimum. A post-restart
memory peak is not a pre-OOM peak.

The same recovery report records repeated liveness kills of the new 9m KCM main
replica (nine during diagnosis, restartCount ten in the final report), while
older 20m-effective-quota replicas remained running after earlier lease errors.
Set both benchmark replicas to an explicit 20m request/limit. Do not extrapolate
this to other controller or catalog minima: catalog probe failures require their
own diagnosis, and exit 137 with reason Error is not automatically an OOM.

**Lifecycle constraint:** the original RP create operation reached **Failed** at
2026-09-11T17:31:25Z, while CS was still installing. Later control-plane recovery
cannot rewrite that terminal operation. Parent must inspect supported RP recovery
before workers; do not blindly repeat PUT create. Fresh-start confirmation is
still required after any successful authorized in-place treatment.

### Exact Envelope

| Offering Metric | V5 | V6 |
| --- | ---: | ---: |
| Declared controlled CPU cores | 1.717 | **1.739** |
| Modeled effective controlled CPU cores | 1.990 | **2.010** |
| Fixed downstream CPU cores | 0.470 | **0.470** |
| Declared offering CPU cores | 2.187 | **2.209** |
| Modeled effective offering CPU cores | 2.460 | **2.480** |
| Controlled RAM requests MiB | 7,230 | **7,253** |
| Controlled charged RAM MiB | 8,022 | **8,047** |
| Fixed downstream RAM MiB | 2,090 | **2,090** |
| Offering RAM requests MiB | 9,320 | **9,343** |
| Offering charged RAM MiB | 10,112 | **10,137** |
| Raw compute equivalent USD/HCP-hour | 0.12436328125 | **0.12493000000** |

The unchanged D16s_v6 $0.806/hour raw envelope at $0.125/HCP-hour allows exactly
`1000000/403` mCPU and `4096000/403` MiB. Remaining margins are exactly
**`560/403` mCPU (~1.389578m)** and **`10789/403` MiB (~26.771712Mi)**.
Dollar margin is **$0.000070/HCP-hour**. KCM's two Pod parents change 10m -> 20m;
CVO's charged Pod memory changes 46Mi -> 71Mi. Recomputed regular/restartable-init
and sequential-init maxima introduce no additional increase on the saved layout.

This preserves the existing 52-group / 97-running-controlled-instance benchmark
accounting, observed 100000us period / 1000us minimum / 1000us quota quantum, and
fixed downstream request charges. It is not the current rollout-surge inventory,
full bootstrap concurrency, a namespace hard cap or actual packed-node bill. KSM
remains excluded. No unmeasured capacity or runtime guarantees are introduced.

### V6 Artifacts

| Artifact | Bytes | SHA256 |
| --- | ---: | --- |
| `tooling/hcp-perf/examples/small-cost-v6.json` | 10,455 | `10507593da7d6005fc09fbb650bebe5ac3baee47bcb6e84c54ac4017679309ac` |
| `_artifacts/small-cost-v6/small-cost-v6-hypershift.json` | 7,204 | `edf88af1280b7f1b9ed08ee445ec252b2d0606a98cdc0d74bcacb06e57ae42f8` |
| `_artifacts/small-cost-v6/allocation.json` | 59,998 | `2751dbad3a6669f4f64aae5454deeb39b5f56f58d0e1eded4d79cf2d0b75e07c` |

The full rendered CSC comparison allows only CVO main memory and KCM main CPU
request changes. Limits/env are verified against exact derivation; all other
policy/default fields are identical. V1-V5 profile/wire/allocation bytes remain
unchanged. Shared renderer source gained a precise identity/resource allowlist,
so historical reports are hash-preserved rather than rewritten with new source
hashes. The private v6 directory also contains resolved resources, source hashes,
diagnosis references and an output hash manifest.

Read-only replay and tests from the perf-runner root:

```bash
python3 -B tooling/hcp-perf/scripts/cost_allocate_v6.py
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_cost_allocate*.py' -v
```

**21 allocator tests and Go cost/sizing tests pass.** Both sizing CLI commands pass;
all 92 rows and the exact two-component delta are checked. Profile and private
outputs reproduce byte-for-byte. No new live/CEL admission or application test
was performed. Offline render from `tooling/hcp-perf`:

```bash
GOWORK=off go run . sizing hypershift --profile examples/small-cost-v6.json \
  --default-cpu-milli 2 --default-memory-mib 32
```

Use the retained wire artifact for the separately authorized, guarded owning-CSC
new treatment. Preserve prior treatment evidence and record the boundary; this
analysis does not perform application, restart, RP recovery or cleanup.

## Historical V5

## Latest V5

**Progress update, September 11, 2026, approximately 17:26 UTC:** V5 was applied
as a CPU-only diagnostic on the existing v4 HCP. KAS rollout completed; OAS helper
replacements remain Pending and the 50m probe is unmeasured. The full HCP is not
Ready. See [the measured v4/v5 progress and paused state](small-cost-v4-progress.md).
The allocation rationale below is not a fresh-start or workload qualification.

The V5 treatment fixes CPU requests and limits at
**50m** for exactly `openshift-apiserver/kas-readiness-check` and
`packageserver/kas-readiness-check`, three benchmark replicas each. Preserve
**every V4 memory request/limit and Go target**, including both HAProxy replicas
at 96Mi/106Mi. No probe deadlines, code, images, APF, general budget or other
policies change. The existing fixed-memory allocation search/renderer now accepts
a CPU weight field; v5 reuses it and the existing quota/Pod-stage model rather
than copying those algorithms. V4 artifacts remain immutable; its shared source
hash changes, so the old report is hash-verified rather than newly byte-rederived.

### Diagnosed Treatment

Authoritative saved source:
`_artifacts/small-cost-v4/lifecycle/diagnostic-continuation/result.json`,
**2026-09-11T16:58:09.004935Z**, with raw confirmation Pod/EndpointSlice receipts
in `confirm-1789145875/`. All three OAS main containers were Ready, but their
`kas-readiness-check` sidecars were not. The unchanged five-second bash/curl KAS
livez probe succeeded only after **13.099s wall / 0.065s user CPU**; declared CPU
was **5m**, observed `cpu.max` **1000/100000us = 10m**. Diagnostics recorded
**769/772 throttled periods**. CPO direct checks returned KAS readyz in **0.373s**
and OAS Pod readyz in **0.428s**, but OAS Service had **zero ready endpoints**.
Diagnostic exec itself adds load; these are causal-path observations, not clean
capacity measurements. Packageserver uses the same helper and receives the same
targeted allowance; no blanket init/startup CPU floor is introduced.

V4 HAProxy succeeded over the observed startup window: both Ready, zero restarts,
no OOM, charge peaks about **72.3Mi** under 96Mi/106Mi. Sources are V4 lifecycle
`HANDOFF.md`, `metric-peaks.json` and retained startup resources. V4 did not finish
startup because of the diagnosed readiness gate, not another HAProxy OOM.

### Strict Budget

The six helper child quotas now total **300m**, compared with their previous
60m effective child-quota sum, an increase of **240m**. This is not blindly added
to the offering total: shared Pod parents cap concurrent helper/main/other quotas.
The exact rational search fixes those helpers at 50m, charges downstream's fixed
470m, and rescales all other controlled CPU by one common factor applied to the
original source weights. Factor is **249999999/1000000000**; per-container
requests round down to 1m, then the observed 1000us quantum and Pod-stage caps
are included in the cost check. No original floors are restored.

| Offering Metric | V4 | V5 |
| --- | ---: | ---: |
| Declared controlled CPU cores | 1.684 | **1.717** |
| Modeled effective controlled CPU cores | 2.010 | **1.990** |
| Fixed downstream CPU cores | 0.470 | **0.470** |
| Declared offering CPU cores | 2.154 | **2.187** |
| Modeled effective offering CPU cores | 2.480 | **2.460** |
| Offering RAM requests MiB | 9,320 | **9,320** |
| Offering charged RAM MiB | 10,112 | **10,112** |
| Raw compute equivalent USD/HCP-hour | 0.124930000 | **0.12436328125** |

The budget remains **$0.125/HCP-hour**, D16s_v6 at $0.806/hour, raw CPU entitlement
2.481389578 cores and RAM entitlement 10163.771712Mi. CPU slack is **21.39m**;
RAM slack remains **51.77Mi**. Quota/rounding steps prevent arbitrary exact budget
equality; the selected point stays strictly below it. Memory now determines the
larger raw cost share. Declared CPU can rise while modeled quota CPU falls because
CPU is moved from quota-inflated tiny allocations to explicit 50m helpers and
other Pod aggregates cross downward quota steps.

| Largest Declared CPU Changes, Per Container | Replicas | V4 m | V5 m | Replica-Weighted Delta m |
| --- | ---: | ---: | ---: | ---: |
| OAS kas-readiness-check | 3 | 5 | **50** | +135 |
| Packageserver kas-readiness-check | 3 | 5 | **50** | +135 |
| KAS main | 3 | 169 | **144** | -75 |
| Packageserver main | 3 | 67 | **57** | -30 |
| CPO | 1 | 160 | **137** | -23 |
| Etcd main | 3 | 35 | **29** | -18 |
| Cluster-network-operator | 1 | 76 | **64** | -12 |
| Ignition server | 2 | 43 | **37** | -12 |
| OAS main | 3 | 26 | **22** | -12 |
| Ignition HAProxy | 2 | 2 | **2** | 0 |

This deliberately reduces nonfixed CPU scale by about **14.6%**. KAS's declared
main reduction is ~14.8%, CPO's ~14.4%; either can become the next bottleneck.
Modeled parent quotas are **160m per KAS Pod**, **140m CPO**, **80m per OAS Pod**,
and **110m per packageserver Pod**. Each helper's child quota is 50m, but a shared
parent cap means that is not guaranteed delivered CPU under concurrent contention.
The unchanged five-second probe must be measured after any authorized treatment.

Coverage remains the 52 benchmark Pod groups / 97 controlled running instances,
not the current incomplete startup's exact footprint. All 92 controlled rows retain
identical memory and runtime policy. Fixed downstream may use more than requests;
KSM remains excluded. These raw resource equivalents are not a namespace hard cap,
actual bill, or proof of fresh-start/population/QPS viability.

### V5 Handoff

| Artifact | Bytes | SHA256 |
| --- | ---: | --- |
| `tooling/hcp-perf/examples/small-cost-v5.json` | 10,502 | `0186968aba7d6a64c078ce38479dd270f0d1805e3cc2200da26aed9103e3f9a4` |
| `_artifacts/small-cost-v5/small-cost-v5-hypershift.json` | 7,203 | `5f562b6861e3d9f4613268c973ae77af87b8f15301a1d4235d8512abb5f95a24` |
| `_artifacts/small-cost-v5/allocation.json` | 83,417 | `2d2f77364603f1ea60bcdc62959083961f21ff561342cce63688ab47f042c56d` |

The directory also contains resolved resources/runtime env and an output hash
manifest. The complete rendered CSC comparison permits **only CPU-request changes**;
memory, fallback memory, limit ratios, Go settings, inflight settings and all other
fields remain exactly V4. Source hashes pin V4 artifacts, original CPU weights,
saved benchmark layouts and the fresh diagnosis. Earlier artifact bytes are untouched.

Read-only replay and tests from the perf-runner root:

```bash
python3 -B tooling/hcp-perf/scripts/cost_allocate_v5.py
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_cost_allocate*.py' -v
```

Twenty allocator tests pass, including six fixed helper replicas, parent-cap costing,
unknown/unsatisfiable overrides and exact unchanged memory. Both sizing commands
pass; all 92 rows match their effective policy and the profile/four private files
reproduce byte-for-byte. **Fallback remains 2m/32Mi**, derived rather than guessed:

```bash
GOWORK=off go run . sizing hypershift --profile examples/small-cost-v5.json \
  --default-cpu-milli 2 --default-memory-mib 32
```

No live call or mutation was made here. Parent may apply the reviewed wire through
its separately authorized, resourceVersion-guarded owning CSC path on the current
HCP, explicitly recording a **new treatment boundary**, not rewriting V4's result.
An in-place recovery is not fresh-start qualification: even if successful, retain
the original evidence and later perform fresh startup confirmation before capacity
claims. No probe or source-code relaxation is part of this treatment.

## Historical V4

## Latest V4

**V4 is offline, unapplied and startup-untested.** Both ignition proxy HAProxy
replicas now have fixed **96Mi requests / 106Mi hard limits**. Their CPU is
unchanged from v3: **2m declared, 10m modeled effective quota each**. This targets
the actual failed component rather than increasing the general $0.125/HCP-hour
budget. All original v1/v2/v3 profiles, wire objects, analyses and lifecycle
evidence are preserved. V3 was subsequently deployed and failed at 55Mi; see the
[v3 result](small-cost-v3-result.md), which supersedes its historical offline
status below.

### Evidence

The final Small benchmark has **two Ready HAProxy replicas, zero restarts**, each
requesting **64Mi with a 192Mi limit**, with no CPU limit (10m request). Saved
quiet-window charge peak is **76,406,784 bytes = 72.8671875Mi**, and cgroup RSS
peak is **70,778,880 bytes = 67.5Mi**. RSS is descriptive because its own timestamp
query was not saved; it is not substituted for charge. These are reused-cluster
quiet observations, not fresh constrained-startup peaks.

Sources: `_artifacts/small-constrained-v1/analysis/candidate-budgets.json`, exact
`Deployment/ignition-server-proxy/containers/haproxy` selector, and
`_artifacts/perf-resource-locked-scan/density-v6-apps60/before/full-pods.json` for
actual resources/Ready/restarts. The Small analyzer's final-image windows were
2026-09-10 02:28-02:36 and 03:04-03:10 UTC.

Medium's actual saved Pod snapshot
`_artifacts/medium-v1-20260910/verification/005006/pods.json` independently confirms
both HAProxy replicas **Ready, zero restarts, 128Mi request / 384Mi limit**,
20m CPU requests and no CPU limits. This verifies a working configuration, **not
that 128Mi was required or a 106Mi limit is sufficient**. V3's
`_artifacts/small-cost-v3/lifecycle/failure.json` records the later 55Mi OOMs
(15:28:47/48 UTC). All these inputs are hashed in the v4 allocation report.

**Hypothesis:** 96Mi/106Mi provides room above the observed 72.87Mi charge and
the failed 55Mi cap. It remains below the historical working 192Mi limit and is
not a guaranteed fresh-start minimum. HAProxy is non-Go; its existing Go exception
is preserved. This is not a Go-runtime-memory fix.

### Fixed-Budget Redistribution

Only HAProxy is removed from proportional RAM scaling. Its two 106Mi limits are
charged first; all other controlled containers retain the **same original source
RAM weights** with a common factor `515624999/1000000000`, rounding requests down
to MiB and limits up to MiB. The search includes actual replica and init-stage
accounting. Exact fixed override identities must exist and be unambiguous;
unsatisfiable fixed allocations fail instead of silently raising the budget.

| Offering Model | V3 | V4 |
| --- | ---: | ---: |
| Declared CPU cores | 2.154 | **2.154** |
| Modeled effective CPU cores | 2.480 | **2.480** |
| Controlled RAM requests MiB | 7,280 | **7,230** |
| Controlled charged RAM MiB | 8,068 | **8,022** |
| Fixed downstream RAM requests MiB | 2,090 | **2,090** |
| Offering RAM requests MiB | 9,370 | **9,320** |
| Offering charged RAM MiB | 10,158 | **10,112** |
| Raw RAM entitlement MiB | 10,163.77 | **10,163.77** |
| Raw compute equivalent USD/HCP-hour | 0.124930000 | **0.124930000** |

Remaining charged-memory slack is exactly `20864/403` MiB (~51.77Mi). It is left
unused because the next common-factor rounding step, including many 64Mi source
helpers, would exceed the envelope; no ad hoc redistribution changes relative
weights. No other memory request drops by more than 8Mi per container.

| Per-Container Requests / Limits MiB | V3 | V4 | Replicas |
| --- | --- | --- | ---: |
| Ignition HAProxy | 50 / 55 | **96 / 106** | 2 |
| Kube-apiserver | 535 / 589 | **527 / 580** | 3 |
| Control-plane-operator | 66 / 73 | **65 / 72** | 1 |
| Fallback | 33 / 37 | **32 / 36** | varies |

KAS/CPO remain below prior measured charge and may become the next failure. A
successful HAProxy start would not establish complete HCP startup or load capacity.
The unchanged modeled CPU uses period 100000us, minimum 1000us, quantum 1000us and
Pod parent caps, based on saved startup samples rather than universal runtime
semantics. The 52-group benchmark layout is distinct from complete fresh bootstrap.
Downstream is still charged at requests despite potentially higher actual usage;
KSM remains excluded. Raw capacity, not actual pool bills/packing, defines this
economic envelope. No namespace hard-cap claim is made.

### Ready Artifacts

| Artifact | Bytes | SHA256 |
| --- | ---: | --- |
| `tooling/hcp-perf/examples/small-cost-v4.json` | 10,439 | `e250192d12db7c5c43b58fdc35c54b35962eb92e326ca5a1bd9c640f53f8c7d8` |
| `_artifacts/small-cost-v4/small-cost-v4-hypershift.json` | 7,202 | `7f6a09f5bcbe135015adf0462589895cf245b7d7ecb97e507bc2cb4b818aeef9` |
| `_artifacts/small-cost-v4/allocation.json` | 90,425 | `f4d80e5d9d301ce6b54bd92934f1fa3792b86269c21a676a6fd4edf494fae16a` |

The private directory also includes resolved requests/limits/Go env, every memory
delta and an output hash manifest. The exact wire comparison to retained v3 allows
**only memory-request changes**, including fallback; CPU, limit policies, Go
exceptions, inflight/APF-related CSC settings and all other CSC fields are unchanged.
No images, APF objects, probes or live resources were changed.

The reusable `fixed_memory()` primitive and v4 derivation are in
`tooling/hcp-perf/scripts/cost_allocate_v4.py`; older allocator scripts are untouched.
Read-only reproduction and tests from the perf-runner root:

```bash
python3 -B tooling/hcp-perf/scripts/cost_allocate_v4.py
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_cost_allocate*.py' -v
```

All **18 allocator tests pass**, including fixed-replica charges, nonlisted
override rejection, unsatisfiable budgets and bounded redistribution. Both sizing
CLI commands pass and all 92 controlled rows match explicit/fallback resources.
The profile and four private outputs reproduce byte-for-byte. No new live or CEL
admission tests were performed.

From `tooling/hcp-perf`, the exact offline render command is:

```bash
GOWORK=off go run . sizing hypershift --profile examples/small-cost-v4.json \
  --default-cpu-milli 2 --default-memory-mib 32
```

**Fallback 2m/32Mi is derived, mandatory, and present in the retained wire object.**
Parent execution must use the verified artifact under its separate authorization;
this change contains no application operation.

## Historical V3

## Latest V3

**V3 is offline, unapplied and startup-untested. Independent CPU/RAM scaling is
now the recommended candidate**, preserving proportions within each resource,
not forcing CPU:RAM pairs. V1 and v2 profile/wire/allocation artifacts remain
immutable. The min-only v2 cost model is superseded, not silently corrected in its
historical outputs. The shared v2 implementation now accepts an optional quantum;
its default `1` retains historical min-only numeric semantics. Its source hash
has changed, so old v2 report byte-reproduction is not claimed with this source;
original output hashes are verified independently by the v3 replay.

### Observed Basis

`_artifacts/small-cost-v1/lifecycle/cleanup/one-shot/quota-analysis.json`, captured
**2026-09-11T15:11:11.677212Z**, refers to the retained
`cleanup/before-delete/cpu-quota-nodeproxy.json`. It has 99 raw samples and 41
quota/period-paired rows: **24 containers and 17 Pod parents**, plus 17 sandbox
rows with period but absent quota. Every recorded period is **100000us**. Missing
quota is not interpreted as unlimited. Checked admitted container examples are
**3m -> 1000us, 6m -> 1000us, 23m -> 3000us, 183m -> 19000us**.

V3 uses explicit `cpuPeriodUS=100000`, `minQuotaUS=1000`, and
**`quotaQuantumUS=1000`**:

```text
quotaUS = max(minQuotaUS,
              ceil(milliCPU * cpuPeriodUS / (1000 * quotaQuantumUS)) * quotaQuantumUS)
effectiveMilliCPU = quotaUS * 1000 / cpuPeriodUS
```

Round to the quantum before applying the minimum. Pod-stage parent caps remain
the bound on concurrent child quotas; do not sum rounded containers blindly.
All 24 matched containers and all 17 parents match this model. Parent identity is
joined by cgroup Pod UID to the saved before-delete Pod specification. Container
samples may include completed init or old runtime identities, so this is a
point-in-time evidence check, not continuous admission/usage coverage.

**This is observed behavior of this startup sample, not universal Kubernetes
semantics.** Saved nodes report containerd 2.2.4, kubelet v1.35.7, Azure Linux 3.0,
kernel 6.6.150.1-1.azl3. Locally available
`github.com/opencontainers/cgroups@v0.0.6/systemd/common.go`, `addCPUQuota`, lines
320-328 rounds fractional CPU percentages upward and writes the adjusted quota
to cgroupfs. That is compatible with a 1000us quantum at a 100ms period. The
exact deployed runc/cgroups source revision and active code path were not verified;
the origin therefore remains unconfirmed. Other periods can have different
runtime quanta; custom parameters are a model, not a claim of observed support.

### Current Numbers

Same D16s_v6 $0.806/hour raw capacity model and 52 benchmark Pod groups / 97
controlled running instances. Fixed downstream remains 470m/2090Mi; KSM is
excluded. Charged memory includes every controlled rounded 110% limit plus fixed
downstream requests, not a namespace hard cap.

| V3 Model | Budget | Declared CPU | Effective CPU | Request MiB | Charged MiB | USD/HCP-Hour | Unused Charged MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Small independent, recommended | 0.125 | 2.154 | **2.480** | 9,370 | 10,158 | **0.124930000** | **5.77** |
| Small uniform comparison | 0.125 | 2.154 | **2.480** | 6,127 | 6,556 | **0.124930000** | 3,607.77 |
| Double independent, recommended | 0.250 | 4.754 | **4.950** | 18,609 | 20,313 | **0.249821136** | **14.54** |
| Double uniform comparison | 0.250 | 4.754 | **4.950** | 12,058 | 13,103 | **0.249356250** | 7,224.54 |

Controlled declared/effective CPU totals are **1.684/2.010 cores** for Small and
**4.284/4.480 cores** for double. Effective offering CPU slack is **1.39m** and
**12.78m**. Requests remain below original floors; quota rounding changes the
modeled ceiling, not scheduler requests. Independent RAM allocations remain
7280/8068Mi controlled requests/limits and 16519/18223Mi respectively. The
1e-9-grid rational factor search includes the quantum and actual Pod stage caps.

Under the now-observed quantum, historical v1 Small would model at **2.750 cores /
$0.138531250/hour**, and doubled v1 at **5.130 cores / $0.258423750/hour** on the
benchmark layout. These supersede the min-only v2 diagnostic numbers below, not
the immutable original bytes. The failed-startup evidence still does not prove
the fixed $0.125 envelope impossible.

### Observed Parents

The 17 captured startup parent groups are a separate coverage set from the 52
benchmark groups. For each row, declared aggregate parent limit -> modeled quota
is in millicores. Counts are actual captured parents, including two capi-provider
Pods; candidate columns are **predictions on these saved layouts**, not new live
measurements. Uniform and independent share CPU values.

| Workload | Count | Original Declared -> Observed | Small V3 | Double V3 |
| --- | ---: | --- | --- | --- |
| capi-provider | 2 | 3 -> 10 | 2 -> 10 | 7 -> 10 |
| cluster-api | 1 | 23 -> 30 | 20 -> 20 | 50 -> 50 |
| control-plane-operator | 1 | 183 -> 190 | 160 -> 160 | 394 -> 400 |
| etcd | 3 | 55 -> 60 | 47 -> 50 | 120 -> 120 |
| ignition-server | 2 | 50 -> 50 | 43 -> 50 | 107 -> 110 |
| ignition-server-proxy | 2 | 3 -> 10 | 2 -> 10 | 7 -> 10 |
| kube-apiserver | 3 | 211 -> 220 | 182 -> 190 | 457 -> 460 |
| router | 3 | 3 -> 10 | 2 -> 10 | 7 -> 10 |

The predicted 17-parent sums are **1.070 cores Small / 2.480 cores double**.
They are not full-offering totals and do not imply that all future bootstrap
Jobs, disabled workloads or replicas are covered. Full bootstrap remains unknown.

### Memory And Paths

Recommended Small HAProxy/KAS/CPO memory limits are **55/589/73Mi**;
recommended double limits are **125/1333/167Mi**. Small remains below prior
observed charge for all three and may fail startup. Uniform Small would use
**31/329/41Mi**, worsening the already-failed 36Mi HAProxy case. Independent
allocation uses RAM more fully without claiming a startup fix.

Recommended fallback flags: Small **2m/33Mi** (37Mi memory limit), double
**7m/75Mi** (83Mi memory limit). Uniform diagnostic fallback is 2m/18Mi and
7m/45Mi. CPU limit=request and GOMEMLIMIT=80% of request remain unchanged, with
existing non-Go exceptions. All 92 controlled rows resolve exactly through the
explicit entries or scaled fallback.

| Profile | Bytes | SHA256 |
| --- | ---: | --- |
| `tooling/hcp-perf/examples/small-cost-v3.json` | 10,504 | `2cac7b641f4322760f8b319b23267ba9b890aef9bb80950fa212cde61a811e0e` |
| `tooling/hcp-perf/examples/double-small-cost-v3.json` | 10,532 | `ae78c04a522d0ef4cab6924fb847c3fb5ba968c47e387a6d640401ed5f68c7b0` |

`_artifacts/small-cost-v3/` contains both `*-uniform.json` comparison profiles,
all four `*-resolve.json` / `*-hypershift.json` outputs, `allocation.json` with
observed/candidate parent comparisons and input hashes, and `output-hashes.json`.
Do not render without the required fallback flags; use retained wire objects for
review. Offline reproduction from the perf-runner root:

```bash
python3 -B tooling/hcp-perf/scripts/cost_allocate_v3.py \
  --cpu-period-us 100000 --min-quota-us 1000 --quota-quantum-us 1000 \
  --hypershift-root "$HYPERSHIFT_ROOT"
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_cost_allocate*.py' -v
```

Sixteen allocator tests pass, including 23m/183m regressions, custom quantum,
parent caps, init stages and all 17 observed parent joins. All four profiles pass
resolve/render, exact row comparison and local CRD OpenAPI checks. Live/CEL
admission, deployed source verification and v3 startup were not performed.

## Historical V2 Review

All v2 numerical claims below used only the minimum quota, not the observed
quantum. They are retained for history and superseded by v3 above.

## Current Status

**Small cost-v1 was deployed and failed startup. V2 is offline, unapplied and
untested.** All original cost-v1 profiles, wire objects, allocation/price outputs,
and stable first-failure evidence remain byte-identical historical inputs. The
v1 allocator itself is unchanged. Do not run its historical `--refresh` command
against these immutable artifacts. Active lifecycle cleanup belongs to its owner;
this offline task neither operates on it nor freezes its actively written logs.

The failure is recorded in
`_artifacts/small-cost-v1/lifecycle/startup-failure.json`, captured
**2026-09-11T14:49:55.228380Z**, and `startup-r2/000/pods.json` plus
`admitted-resources.json`. Two `ignition-server-proxy/haproxy` containers had
**3m/32Mi requests and 3m/36Mi limits**, with `OOMKilled`, exit 137. The earliest
retained termination is **14:49:26Z**, followed by 14:49:39/42Z. No Go env was
injected for HAProxy. `failure-resource-audit.json` retained 14 controlled
regular/init containers with zero policy mismatches. The lifecycle stopped before
worker creation, population or QPS tests. Initial polling used the wrong name;
corrected discovery retained original Pod UIDs and previous terminations, but
does not establish complete first-start observation. Actual management hosts were
D4s_v3; D16s_v6 remains an economic model, not deployed host capacity.

This is a valid failed-startup experiment, **not proof that $0.125 is impossible**.
The source benchmark remains valid ratio evidence; it does not prove these scaled
budgets work. No resource rescue, deployment or cleanup was performed in this pass.

## CPU Quota Correction

The explicit model parameters are `cpuPeriodUS=100000` and `minQuotaUS=1000`.
**Both, along with finite Pod parent quotas and matching parent/container periods,
are assumptions until verified on the actual kubelet/runtime/cgroups.** The
default conversion is:

```text
quotaUS(milliCPU) = max(minQuotaUS, floor(milliCPU * cpuPeriodUS / 1000))
effectiveMilliCPU = quotaUS * 1000 / cpuPeriodUS
```

A positive 3m limit therefore permits a modeled 10m quota. CPU request remains 3m;
do not relabel it a 10m scheduler request. Conversion uses integer microseconds,
not floating-point rounding. Custom periods and minimum quotas are supported.

For each saved controlled Pod, the parent declared CPU limit is the Kubernetes
maximum across regular containers plus restartable init, and sequential init
stages with previously started restartable init. Convert that parent limit to
quota; each stage's effective ceiling is the **minimum of the parent quota and
the sum of concurrent child quotas**. Sum the maximum stage ceiling once per Pod,
not blindly rounded-up containers or sequential stages. Steady-state child/parent
bounds are also recorded. For example, three 3m containers in one Pod sum to
30m child quotas but the 9m parent converts to 10m and bounds them at 10m total.
Three separate single-container Pods instead sum to 30m.

Coverage is **52 controlled Pod groups / 97 running instances**, with all 92
controlled selector rows preserved. These are the UID-matched source benchmark
layouts, not an assertion that the failed startup reached that inventory.
Completed Jobs, disabled workloads, changed replicas and full bootstrap concurrency
remain unmeasured. Downstream's 47 instances are still charged **470m/2,090Mi fixed
requests**, not fictional finite quotas. KSM's 10m/64Mi is excluded and separately
reported. Consequently the offering totals below are modeled controlled quota
ceilings plus fixed downstream accounting, **not a namespace CPU or RAM cap**.

| Historical V1 | Declared Offering CPU | Quota-Aware Offering CPU | Corrected USD/HCP-Hour |
| --- | ---: | ---: | ---: |
| Small | 2.440 | **2.611** | **0.131529125** |
| Doubled cost | 4.907 | **4.970** | **0.250363750** |

Both historical profiles exceed their intended economic envelope under these
quota assumptions. Their bytes and original reports are preserved, not rewritten
to hide the defect. These figures are regression-tested from saved Pod layouts.

## V2 Candidates

`scripts/cost_allocate_v2.py` performs exact rational binary search on a 1e-9 factor
grid, checking Pod-bounded quota CPU and per-container rounded 110% memory limits
inside factor selection. It rounds requests down to 1m/1Mi, rejects zero results,
and never restores original floors. Minimum representable requests must themselves
fit quota cost. The factor upper bounds are the raw CPU and memory entitlements
minus fixed downstream requests. Limit-aware memory is evaluated after rounding
each limit and taking Pod init-stage maxima, rather than using an uncharged 10%
allowance. Quantization slack remains unused; it is not arbitrarily redistributed.

To avoid silently changing the previous pair-preservation decision,
`examples/small-cost-v2.json` and `examples/double-small-cost-v2.json` are **uniform
comparison candidates**. The separately named `*-independent.json` profiles and
wire artifacts are executable offline diagnostics. **Recommend independent CPU
and RAM factors for the next reviewed decision**, preserving relative CPU weights
and relative RAM weights independently, rather than forcing CPU:RAM pairs. No
profile in this section is approved for live application.

All rows use D16s_v6 at the retained $0.806/hour price. Memory charged is controlled
rounded limits plus unchanged downstream requests; RAM requests are shown separately.

| V2 Model | Budget | Declared CPU | Effective CPU | Offering Request MiB | Charged MiB | USD/HCP-Hour | Unused Charged MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Small uniform | 0.125 | 2.308 | **2.479** | 6,351 | 6,790 | **0.124879625** | 3,373.77 |
| Small independent | 0.125 | 2.308 | **2.479** | 9,370 | 10,158 | **0.124929016** | **5.77** |
| Doubled uniform | 0.250 | 4.897 | **4.960** | 12,466 | 13,537 | **0.249860000** | 6,790.54 |
| Doubled independent | 0.250 | 4.897 | **4.960** | 18,609 | 20,313 | **0.249860000** | **14.54** |

Effective CPU slack is 2.39m for Small and 2.78m for doubled cost. Controlled-only
declared/effective CPU is **1.838/2.009 cores** and **4.427/4.490 cores** respectively.
Controlled RAM requests/charged MiB are 4,261/4,700 (Small uniform), 7,280/8,068
(Small independent), 10,376/11,447 (double uniform), 16,519/18,223 (double
independent). Add 470m/2,090Mi fixed downstream to obtain offering totals. Add
KSM's 10m/64Mi requests separately for experiment-environment accounting.

### Startup Risk

Per-container memory request/limit MiB, not replica-weighted:

| Container | Small Uniform | Small Independent | Double Uniform | Double Independent |
| --- | --- | --- | --- | --- |
| Ignition HAProxy | 29 / **32** | 50 / **55** | 71 / **79** | 113 / **125** |
| Kube-apiserver | 316 / **348** | 535 / **589** | 762 / **839** | 1,211 / **1,333** |
| Control-plane-operator | 39 / **43** | 66 / **73** | 95 / **105** | 151 / **167** |

Small independent uses the RAM that uniform v1 left idle (~2.8GiB), but is **not a
known startup fix**. Its HAProxy 55Mi limit remains below the benchmark's ~72.9Mi
observed charge; KAS 589Mi is below ~829Mi, and CPO 73Mi below ~93.7Mi. These are
reused benchmark observations, not calibrated fresh-start requirements, but they
show why failure remains plausible. Small uniform reduces HAProxy below the
already-failed 36Mi limit and should not be mistaken for the recommended rescue.
Even doubled independent has no proven startup/QPS capacity.

Fallbacks also scale, including all unobserved controlled init/Job rows:

| Model | Fallback Requests | Memory Limit |
| --- | --- | --- |
| Small uniform | 3m / 19Mi | 21Mi |
| Small independent | 3m / 33Mi | 37Mi |
| Double uniform | 7m / 47Mi | 52Mi |
| Double independent | 7m / 75Mi | 83Mi |

CPU limit=request, GOMEMLIMIT=80% of memory request, existing non-Go exceptions,
GOMAXPROCS preservation and extended-resource preservation remain unchanged.
The quota model does not change the declared CPU resource or invent a 10m floor.
Downstream excess and raw-host/allocatable/packing/HA overhead remain economic risks.

## V2 Reproduction

From `ARO-HCP-perf-runner`, with the local HyperShift worktree path in
`HYPERSHIFT_ROOT`:

```bash
python3 -B tooling/hcp-perf/scripts/cost_allocate_v2.py \
  --cpu-period-us 100000 --min-quota-us 1000 \
  --hypershift-root "$HYPERSHIFT_ROOT"
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p 'test_cost_allocate*.py' -v
```

This rederives and byte-compares only v2 files, verifies original v1 hashes and
stable failure evidence, resolves/renders all four profiles, and compares every
controlled row against explicit or fallback policy. Optional local CRD OpenAPI
validation is used here; CEL/live admission is not tested. Go dependency network
access is disabled. `--write` creates only absent v2 files (identical existing
bytes are accepted); there is no v2 refresh/overwrite mode. Custom quota parameters
can be exercised through `derive()` and unit tests; different results cannot
overwrite the retained default-assumption v2 artifacts.

| Profile | Bytes | SHA256 |
| --- | ---: | --- |
| `tooling/hcp-perf/examples/small-cost-v2.json` | 10,385 | `4d5fc2d775e28f92e342b2891aa1df8f2474a83d4ec93e0daca8725ef29a04ad` |
| `tooling/hcp-perf/examples/double-small-cost-v2.json` | 10,409 | `d0fe2ea35af1564c29dd486ca8a85dd104b80ddf2c928050bd4787086edc6a1e` |
| `_artifacts/small-cost-v2/small-cost-v2-independent.json` | 10,412 | `dd9359ff7d78c360dd1ff720cb067bf7ff210bcb1cfff2cefbdc4fa4783c405f` |
| `_artifacts/small-cost-v2/double-small-cost-v2-independent.json` | 10,440 | `2a0fd85f2b4c5eb609cc52f3d4421bc81d8e080e18d10281766da1788464a2aa` |

`_artifacts/small-cost-v2/allocation.json` includes assumptions, declared/effective
totals, all 52 Pod quota groups, per-container allocations, regressions and input
hashes. Each profile has `*-resolve.json` and `*-hypershift.json` outputs there;
`output-hashes.json` pins their bytes. **Use the saved wire files for review**:
raw rendering requires the appropriate fallback flags from the table, and default
10m/64Mi rendering is not this candidate. Thirteen v1/v2 allocator tests pass,
including sub-10m, shared/separate parents, transient/restartable init, custom
period/minimum, rational boundaries and exact v1 regressions. No live operations.

## Historical V1 Analysis

The remainder preserves the original allocation explanation. Its deployment
status, uniform recommendation and declared-CPU cost claims are superseded by
the failure and quota-aware review above. Its files remain immutable evidence.

**Original offline proposal, now deployed and startup-failed, not a qualified offering.** The governing target
is now **$0.125/HCP-hour**, half the $0.25 fee, followed by a **$0.25/HCP-hour**
experiment. Original request floors do not constrain allocation. Startup or tiny
population/QPS failure is acceptable experimental evidence; it does not authorize
silently increasing the cost envelope. The corrected and frozen peak candidates
remain unchanged as ratio/evidence sources, not minimum allocations.

## Fresh Retail Prices

Public Azure Retail API queried **2026-09-11 14:06:18-19 UTC**, independently for
all four exact ARM SKUs, region `westus3`, USD, Linux `Consumption`, `1 Hour`,
primary meter, zero tier. Windows, Spot and Low Priority records are excluded;
ambiguous or missing matches fail closed. Retained effective date: 2024-10-01.
Retrieval time is fresh; effective date is not rewritten to appear fresh.

| SKU | Raw vCPU / GiB | USD/Node-Hour | $0.125 Raw CPU Entitlement | Raw RAM Entitlement GiB |
| --- | --- | ---: | ---: | ---: |
| Standard_D16s_v6 | 16 / 64 | **0.806** | 2.48139 | 9.92556 |
| Standard_D32s_v6 | 32 / 128 | **1.613** | 2.47985 | 9.91940 |
| Standard_E16s_v6 | 16 / 128 | **1.058** | 1.89036 | 15.12287 |
| Standard_E32s_v6 | 32 / 256 | **2.117** | 1.88947 | 15.11573 |

The 32-core prices are **not twice** the 16-core prices: D32 is $0.001/hour more
than $1.612, and E32 is $0.001/hour more than $2.116. Raw capacities are from the
Microsoft Learn [Dsv6](https://learn.microsoft.com/azure/virtual-machines/sizes/general-purpose/dsv6-series)
and [Esv6](https://learn.microsoft.com/azure/virtual-machines/sizes/memory-optimized/esv6-series)
tables. These are raw VM specifications, not AKS allocatable or measured usable
capacity. The Esv6 page labels its basics column GB but identifies the series in
GiB; this experiment uses 128/256 GiB, matching that series specification.

`_artifacts/small-cost-v1/prices.json` preserves each selected full record, exact
source URL, RFC3339 retrieval timestamp, currency and hourly price. Four original
API responses and their SHA256 hashes are retained alongside it. No authenticated
Azure or Kubernetes API was used.

## Allocation Rule

Source: 97 controlled running regular/restartable-init instances, totaling
**6,020m CPU / 14,016Mi RAM**, from the scoped sustained-p95 CPU / peak-charge
candidate. These are evidence-derived request proportions with the previous
floor effects, **not raw simultaneous usage ratios**. The old floors are removed
when scaling. The 47 downstream instances stay fixed at **470m / 2,090Mi**.
KSM's **10m / 64Mi request, 256Mi limit** is excluded from offering economics and
reported separately as experiment overhead. Management services and guest workers
are not in this offering-resource calculation.

For node price `P`, target `B`, raw CPU `C`, raw memory `M`:

```text
raw CPU entitlement = B/P * C
raw memory entitlement = B/P * M
CPU factor = (raw CPU entitlement - fixed downstream CPU) / 6020m
request-only RAM factor = (raw RAM entitlement - 2090Mi) / 14016Mi
limit-aware RAM factor = (raw RAM entitlement - 2090Mi - 97Mi) / (1.1 * 14016Mi)
recommended uniform factor = min(CPU factor, limit-aware RAM factor)
container requests = floor(source requests * factor), separately to 1m and 1Mi
CPU limit = CPU request
memory limit = ceil(memory request MiB * 1.1) MiB
GOMEMLIMIT = floor(memory request bytes * 0.8)
```

The 97Mi reserve conservatively covers less than one MiB of limit rounding per
running controlled instance. The final cost calculation charges the **actual
rounded limit sum**, not this reserve twice. All calculations/comparisons use
exact rational numbers. Zero, negative, or configurable subminimum allocations
are rejected, never clamped upward. The representability defaults, 1m/1Mi, are
not viable-startup minima. Replica counts are included before selecting factors
and after per-container rounding. Quantization slightly distorts ratios downward;
unused slack is not spent through an arbitrary redistribution.

The report computes four models per SKU/budget: request-only versus limit-aware,
each with independent CPU/RAM factors versus one uniform factor. **Recommended:
uniform, limit-aware.** This preserves intra-container CPU:RAM pairs as well as
cross-container CPU and RAM distributions before rounding. Independent allocation
can use more RAM within the same raw envelope, but changes CPU:RAM pairs; it is
reported as an explicit alternative, not substituted into the selected profile.

## Selected Candidates

**D16s v6 is recommended** because it preserves the largest common demand factor;
CPU binds in all four uniform allocations. Tie-breaking prefers fewer raw cores,
then SKU name. E16's memory-rich shape cannot improve a CPU-bound common factor.
This is an economic ranking, not a scheduling or performance ranking.

| Scope / Charge | Ratio Source | $0.125 Candidate | $0.25 Candidate |
| --- | ---: | ---: | ---: |
| Controlled CPU cores | 6.020 | **1.970** | **4.437** |
| Controlled RAM requests MiB | 14,016 | **4,641** | **10,393** |
| Controlled RAM limits MiB | 15,468 | **5,185** | **11,467** |
| Fixed downstream CPU / RAM | 0.470 / 2,090 | unchanged | unchanged |
| Offering CPU cores | 6.490 | **2.440** | **4.907** |
| Offering RAM requests MiB | 16,106 | **6,731** | **12,483** |
| Offering charged RAM: controlled limits + downstream requests MiB | 17,558 | **7,275** | **13,557** |
| Environment CPU, including KSM | 6.500 | **2.450** | **4.917** |
| Environment RAM requests, including KSM MiB | 16,170 | **6,795** | **12,547** |
| Raw limit-aware compute equivalent USD/HCP-hour | not cost constrained | **0.122915** | **0.247190125** |
| Unused CPU entitlement m | n/a | **41.39** | **55.78** |
| Unused charged RAM entitlement MiB | n/a | **2,888.77** | **6,770.54** |

Uniform factors are exactly `81059/242606` and `181059/242606`, approximately
0.334118 and 0.746309. The $0.25 profile is a doubled **cost budget**, not exactly
doubled container requests: downstream fixed requests are subtracted only once.
RAM slack is deliberate pair preservation. It is not cash credit that permits
more CPU: resource-equivalent cost is `P * max(CPU/C, chargedRAM/M)`, never the
sum of independently priced CPU and RAM shares.

| SKU | $0.125 Offering CPU / Request MiB / Charged MiB | $0.25 Offering CPU / Request MiB / Charged MiB |
| --- | --- | --- |
| D16s v6 | 2.440 / 6,731 / 7,275 | 4.907 / 12,483 / 13,557 |
| D32s v6 | 2.440 / 6,727 / 7,271 | 4.906 / 12,477 / 13,548 |
| E16s v6 | 1.846 / 5,377 / 5,749 | 3.717 / 9,768 / 10,580 |
| E32s v6 | 1.846 / 5,377 / 5,749 | 3.717 / 9,765 / 10,577 |

### Independent Alternatives

At $0.125, independent request-only D16 allocation gives 2.440 cores / 10,091Mi
of offering requests, but charging the 110% controlled limits reaches 10,932Mi,
**exceeding** the raw memory budget. Independent limit-aware D16 instead yields
2.440 cores / 9,320Mi requests / 10,105Mi charged memory and stays within budget.
E16 independent limit-aware yields 1.846 cores / 14,155Mi requests / 15,404Mi
charged memory. These alternatives change CPU:RAM pairs and need an explicit
decision if more memory at the expense of pair preservation is desired.

All eight independent request-only SKU/budget models overspend when their memory
limits are fully charged. Both selected uniform models happen to be CPU-bound,
so request-only and limit-aware select identical final requests. Keeping the
limit-aware rule prevents that coincidence from hiding future memory overspend.

## Startup And Enforcement

Profile fallback must also shrink: **3m/21Mi** for `small-cost-v1`, **7m/47Mi**
for `double-small-cost-v1`; hard memory limits are 24Mi and 52Mi respectively.
All 39 unobserved controlled rows use this scaled source fallback. No startup
allowance was guessed, and no 10m/64Mi original floor was left behind. Known
HAProxy/router Go exceptions remain disabled; CPU limits and memory limits still
apply. Existing explicit GOMAXPROCS and unrelated extended resources are preserved
by the projector, not inferred from quotas.

UID-matched saved running Pod layouts were checked with Kubernetes sequential
init maxima and cumulative restartable-init accounting. Their effective request
and conservative charged-memory totals equal the running totals above. Missing
downstream init requests contribute zero **only to scheduler arithmetic**, never
as evidence of zero usage. Four selector rows correspond to eight actual init
instances in those layouts. Completed Jobs, disabled workloads, new replicas and
fresh bootstrap concurrency remain unmeasured; the allocator does not invent a
concurrency budget. No claim is made that these profiles fit every startup phase.

Downstream enforcement remains excluded by approval. Their original resources
are untouched, and their observed excess (notably OVN socks-proxy charge above
its request) remains a cost risk. **This is not a namespace hard cap.** Actual
charged memory can exceed the fixed downstream requests. The raw compute model
also omits allocatable reductions, DaemonSets/system overhead, fragmentation,
HA/failure capacity, topology, Pod/NIC constraints, disks and network/services.
Therefore "within budget" means within the requested **raw resource-equivalent
model**, not a guaranteed actual bill or profitable deployed offering. A packed
pool must separately demonstrate its complete node bill per HCP.

## Reproduction

Implementation: `scripts/cost_allocate.py`, a reusable exact allocator and public
price snapshot command. Existing `cost estimate` remains unchanged and independently
confirms both selected charged-demand models. It labels those input data points
`*-limit-aware-charge`: their memory field is modeled limits plus fixed requests,
not a claim that Kubernetes requests equal limits. Unknown guest population,
Pod/NIC capacity and actual packing are not inferred from this cost check.

From the perf-runner root, offline replay (set `HYPERSHIFT_ROOT` to the local
`hypershift-perf-sizing` worktree):

```bash
python3 -B tooling/hcp-perf/scripts/cost_allocate.py allocate \
  --prices _artifacts/small-cost-v1/prices.json \
  --output _artifacts/small-cost-v1 --hypershift-root "$HYPERSHIFT_ROOT"
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p test_cost_allocate.py -v
```

The default mode verifies profile and output bytes, hashes the pinned ratio
inventory and raw price pages, rederives all 32 alternatives, checks every one of
92 controlled rows against the sizing CLI's effective policy, and reruns existing
`cost estimate`. Go dependency networking is disabled. Local CRD OpenAPI schema
validation is available with `--hypershift-root`; CEL/live admission is not run.
`--write` exclusively creates generated outputs; `--refresh` explicitly refreshes
only this script's own profiles/artifacts, never historical candidates/prices.

To fetch new prices, first create a different private output directory with an
existing verified parent, then run `fetch-prices --output <directory>`. That is
the only allocator command with public network access. It preserves raw pages,
timestamps and source URLs rather than silently replacing the retained snapshot.

From `tooling/hcp-perf`, these are offline render commands, **not deployment**:

```bash
GOWORK=off go run . sizing hypershift --profile examples/small-cost-v1.json \
  --default-cpu-milli 3 --default-memory-mib 21
GOWORK=off go run . sizing hypershift --profile examples/double-small-cost-v1.json \
  --default-cpu-milli 7 --default-memory-mib 47
```

**The fallback flags are mandatory.** The existing sizing profile schema does not
embed fallback fields; no undocumented schema extension was added. Rendering
without these flags leaves the generic CLI's 10m/64Mi fallback and is **not this
allocation**. Prefer the retained validated wire artifacts.

## Review Artifacts

| File | Bytes | SHA256 |
| --- | ---: | --- |
| `examples/small-cost-v1.json` | 10,571 | `41bbe94dbe117178b0d9c873259a4b034bb513c9d361b62b4ee5c97ee7431043` |
| `examples/double-small-cost-v1.json` | 10,595 | `3f4bab4e51a193d327afea8c55e6272bb578f1123eda29637ce00860aeae2f09` |

Private evidence directory: `_artifacts/small-cost-v1/`. It contains `prices.json`
and four raw responses, `allocation.json` (source hashes, all alternatives,
selected per-container inventories and Pod-layout checks), each profile's
`*-resolve.json`, `*-hypershift.json`, `*-cost-input.json`, `*-cost-estimate.json`,
and `output-hashes.json`. These are ready for parent review, **not deployment**.

Price snapshot SHA256:
`5eb154b47fa2b62fcc58f1ef0b466de11a2190a1ca29f20a34dafe83c54b5614`.
Allocation report SHA256:
`97b50a5065313a1d18de90310b8a31ff5b0df1733ae1d63c16d47ab4bd02eb68`.
Validated wire artifact hashes are
`11ccc602bc9a24358d481ecb84b2f32ac724424f29f0a186b75a2541761ad5c7`
for Small and
`4e2eea248c32ee271e51a6dc895d3eb9971bc9239d33700ab1d3255f84ae0559`
for doubled-cost Small. Eight allocator tests, the existing Go cost/sizing package
tests, both CLI resolves/renders and OpenAPI checks pass. Offline reproduction
byte-compares all nine generated private outputs and both profiles. The original
9.920-core candidate, scoped 6.500-core candidate and constrained profile hashes
remain unchanged. No deployment, image build, live admission or bootstrap test
was performed.
