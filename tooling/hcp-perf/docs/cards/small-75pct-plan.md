# Small 75% Fee Budget Plan

## V3 CAPZ Provisioning

**Latest candidate `small-75pct-capz-v3`: offline, not applied, qualified=false.**
V2 subsequently passed a five-minute availability window and issued credentials,
but accepted three-worker creation hit CAPZ management-lease timeouts and an
OOMKilled manager at4m/64Mi request,71Mi limit. Saved result at21:17UTC reports
zero guest nodes and a nonterminal accepted operation; lifecycle owner must settle
it without replay or cleanup from this offline handoff.

Only CAPZ manager's protected floor changes to **50m/128Mi** (hard141Mi,
Go soft107374182B, no new GOMAXPROCS override). All v2 measured weights, other
floors, existing runtime settings, original images and exact4.21.32 are retained.
Provisioning peak is unknown; this is an explicit CPU/memory hypothesis.

Native reallocation yields **3720m / 15243Mi**, exact memory-bound cost
**6142929/32768000 USD/hour** (approximately$0.187467316), under$0.1875.
CPU margin840/403m; memory margin1071/403Mi. Protected charges before scaling
are2520m/9000Mi. KAS244->240m and CPO231->228m provide40m effective quota savings;
KAS and etcd lose1Mi per replica and Red Hat catalog loses1Mi, funding CAPZ's70Mi
hard-limit increase with7Mi savings plus existing margin. No protected floor is
lowered and no v2 above-floor allocation is silently promoted to a floor.

Full main/donor comparison and risk notes:
`_artifacts/go-small-75pct-20260915/budget-capz-v3/README.md` and
`provenance/comparison.md`. KAS now240m/846Mi, CPO228m/105Mi, KCM50m/158Mi,
etcd60m/449Mi, catalogs50m/344Mi,50m/317Mi,66m/502Mi. Donor changes require
revalidation; Red Hat catalog553Mi hard limit has less than1Mi margin over its
historical quiet charge peak. No memory safety or worker qualification is claimed.

Wire: `budget-capz-v3/native/hypershift.json` under that artifact root.
SHA256 `f8678c7989576b98ae12d505e61825e4c40420065887a976eb50034fe97b847a`.
Ledger SHA256 `d8238c97da525092b1a6b1006ec76e3bda213a238fce5de9c637b19acbd8b32b`.
Fallback remains2m/21Mi,CPU limit=request,memory110%,Go80%. The provenance plan
is non-executable; no live cloud change was made. Same-binary native checks and
budget/sizing/cost/preparation race tests passed; previous artifacts preserved.

## V2 Weight Correction

**Historical v2 preparation: offline at creation, qualified=false.** V1
inherited donor-adjusted CPU weights while retaining a measured-p95 label. V2
restores both resource weights for all 92 full identities from
`tooling/hcp-perf/examples/budget-small.json`, preserving the latest small07 floors,
runtime overrides, layout and $0.1875 budget. V1 allocations above floors are NOT
promoted to new floors. The immutable v1 artifacts are retained.

| CPU weight | V1 | Restored source |
| --- | --- | --- |
| CPO main | 60 | 550 |
| KAS main | 99 | 580 |
| etcd main | 60 | 120 |
| KAS bootstrap | 40 | 10 |

These are the only four numeric changes; all memory and catalog weights match
the source. KCM's original CPU weight is **40**, unchanged. A JSON integer versus
an equivalent rational string is not counted as a weight change. Source weights
are historical derived proportions, including original baseline floors/rounding,
not raw simultaneous p95 measurements. Unobserved baseline rows remain explicitly
unmeasured. Each new row records source basis and source-file SHA256, and the
input inventory lists every changed weight. Older treatment notes remain history,
not the authority for the restored weights.

Protected CPU charge remains 2480m before weighted scaling. CPU factor is now
42173913/100000000; memory factor remains 828124999/1000000000. Whole charges stay
**3720m / 15180Mi**, exact cost **37479/200000 = $0.187395/HCP-hour**, CPU margin
**840/403m**, memory margin **26460/403Mi**. Next CPU tick costs 3750m, over budget.
Steady/scheduler CPU requests are 3486m, not the quota charge. Replicas, init-stage
semantics, downstream 470m/2090Mi and all 92 selectors are unchanged.

| Main | V1 CPU request/limit | V2 CPU request/limit | V2 child quota | RAM request/hard limit Mi |
| --- | --- | --- | --- | --- |
| KAS x3 | 106m | **244m** | 250m | 847/932 |
| CPO | 60m | **231m** | 240m | 105/116 |
| KCM x2 | 50m | **50m** | 50m | 158/174 |
| etcd x3 | 60m | **60m** | 60m | 450/495 |
| Certified catalog | 60m | **50m** | 50m | 344/379 |
| Community catalog | 67m | **50m** | 50m | 317/349 |
| Red Hat catalog | 107m | **67m** | 70m | 503/554 |
| OAS x3 | 60m | **50m** | 50m | 211/233 |
| Package server x3 | 154m | **96m** | 100m | 264/291 |
| Ignition x2 | 100m | **63m** | 70m | 132/146 |

Other main CPU donors include CNO 174->109m, CVO 120->75m, ingress 127->80m,
HCCO 53->33m and cluster API 47->29m. Full per-selector v1/v2 deltas and floors
are in the v2 ledger and `provenance/comparison.md`; these are intentional weighted
reductions, not proven safe reductions. KAS bootstrap keeps its 40m protected
floor despite restoring weight 10. KCM/etcd remain floor-bound; source weights
do not imply every component's CPU grows. All memory requests/hard limits/Go
soft limits, GOMAXPROCS and non-Go exceptions are unchanged from v1.

V2 hard memory limits remain above the recorded historical quiet peaks: KAS
932 vs ~829.19Mi, CPO 116 vs ~93.71Mi, KCM 174 vs ~144.10Mi, etcd 495 vs
~482.01Mi, catalogs 379/349/554 vs ~374.23/~341.49/~552.05Mi. Catalog margins
remain narrow; Go soft limits are lower. These are historical charge peaks, not
verified startup RSS minima or a health guarantee.

V1 was subsequently applied by the lifecycle owner. Its saved
`_artifacts/go-small-75pct-20260915/LIVE-RESULT.md` reports Available HC/HCP,
eventual 66/66 Ready, zero new OOMs across the continuation, but no required
five-minute failure-free window. New probe increments blocked qualification;
there were no workers or performance load. The user reports approximately 18
minutes of stable availability and considers the gate overstrict; that judgment
does not establish a defect in the gate or justify changing it in this arm.
Do not transfer v1 availability to untested v2. Mutable catalog tags also confounded
v1's strict image-identical comparison.

V2 retains original CPO `19848b826504783497b22c598271521c6178dd473e9aea8d0ccf8cc7c3f49d02`
and prober `edc5ba19183cd4bb2924c43e75b353a6ba54810324da7bdf43637de67aae85e2`,
with **exact release 4.21.32**, release digest
`afc68ed6ce1af80d6e43014d16ad7b3f2067be6bebd970aa556c6a445290c8ee`.
No OLM concurrency image, API budget, probe or fallback changes. Reverify exact
catalog digests before lifecycle testing; the offline sizing profile cannot pin
images. D4v3 test hardware still differs from D16s_v6 illustrative cost hardware.

V2 artifact root: `_artifacts/go-small-75pct-20260915/budget-measured-v2/`.

- Native profile: `native/candidate.json`; CSC: `native/hypershift.json`.
- CSC SHA256: `4822f08ba03fb5cffb32547d299e06bc5e380cbca46e6b160c3203faafa6e18c`.
- Ledger SHA256: `d0c3437867b9e0100633b9ecc5ec20873b97bcf24ca22d03c5d725348e627601`.
- Input SHA256: `813cb0212e04526f32bead410c493571fbaec2b5edbb550b3dd17b3dff8d05e4`.
- `provenance/weight-changes.json` lists all changed weights and source basis.
- `provenance/plan.json` binds the unchanged image/release, binary, source and wire
  receipts; offline only, no stale executable lifecycle guards.
- Defaults remain **2m/21Mi**, CPU limit=request, memory limit 110%, Go 80%.
- Existing frozen Go binary and native allocator check outputs byte-for-byte;
  budget/sizing/cost race tests and full identity/floor/runtime invariants pass.

From `_artifacts/go-small-75pct-20260915/`:

```bash
./bin/hcp-perf budget check --input budget-measured-v2/native/input.json \
  --output budget-measured-v2/native
```

## Historical V1 Plan

The following sections preserve the original v1 preparation and results for
comparison. The original preparation was offline; later live results are above.

## Treatment

Source: `_artifacts/go-small-controller-20260914/budget-11-runtime-catalog/input.json`.
Budget changes from $0.125 to **$0.1875/HCP-hour = 75% of the $0.25 fee**. Set
`preserveMemory=false` to redistribute both resources by the existing weights.
All floors, weights, replica/layout counts, Go policies, fallback and provenance
remain unchanged. `GOMAXPROCS=1` stays explicit on KAS, KCM and three catalog
mains. No request decreases relative to the budget11 allocation.

Required image remains CPO
`arohcpocpdev.azurecr.io/stevekuznetsov/control-plane-operator-perf@sha256:19848b826504783497b22c598271521c6178dd473e9aea8d0ccf8cc7c3f49d02`;
fixed prober remains
`arohcpocpdev.azurecr.io/stevekuznetsov/availability-prober-perf@sha256:edc5ba19183cd4bb2924c43e75b353a6ba54810324da7bdf43637de67aae85e2`.
Do not introduce the separate OLM concurrency image in this arm. Direct-curl
readiness behavior, probe periods/deadlines and all other image selections remain
as small07. The CSC does not itself enforce image selection.

## Economics

Frozen price: Standard_D16s_v6, westus3, $0.806/node-hour; retrieved
2026-09-11T14:06:18.998275Z from Azure retail pricing. Raw capacity 16000m/65536Mi,
not measured allocatable capacity. Price URL and SHA256 are retained in input.

| Quantity | $0.125 baseline | $0.1875 candidate |
| --- | --- | --- |
| CPU entitlement | 1000000/403m | 1500000/403m |
| Memory entitlement | 4096000/403Mi | 6144000/403Mi |
| Whole charged CPU | 2480m | **3720m** |
| Whole charged memory | 10161Mi | **15180Mi** |
| Exact cost USD/HCP-hour | 4094883/32768000 | **37479/200000 = 0.187395** |

CPU entitlement is approximately 3.722084 cores; memory entitlement is
6000/403 GiB (approximately 14.888337 GiB). CPU margin is **840/403m**; memory
margin is **26460/403Mi**. Cost margin is **21/200000 = $0.000105/hour**. Fixed
downstream stays 470m/2090Mi; controlled charges are 3250m/13090Mi. Declared steady
requests total 3566m/13908Mi, distinct from quota/limit charges; scheduler requests
are 3566m/13932Mi. Ordered init stages, restartable-init prefix sums, per-container
memory ceilings and shared Pod quota caps are included. Counts remain 92
selectors, 52 saved Pod instances and 97 running controlled container instances.

Capacity increases 1.5x, not every request. Fixed downstream and protected floors
stay fixed, while independent weighted factors become 336111111/500000000 CPU
and 828124999/1000000000 memory. Next ticks charge 3730m and 15248Mi respectively,
both outside budget. No arbitrary floor/weight changes consume the remaining
small margins.

## Component Changes

Per-container requests are m/Mi. Hard CPU limits equal requests; quota rounding
can raise their modeled effective ceiling. Memory hard limits are ceil(1.1x
request), Go soft limits are floor(80% request bytes), not 80% hard limit.

| Main | Old request | New request | CPU quota | New memory hard / Go soft Mi | GOMAXPROCS |
| --- | --- | --- | --- | --- | --- |
| KAS x3 | 106/560 | 106/847 | 110m | 932 / ~677.6 | 1 |
| KCM x2 | 50/144 | 50/158 | 50m | 174 / ~126.4 | 1 |
| etcd x3 | 60/203 | 60/450 | 60m | 495 / 360 | unchanged, omitted |
| CPO x1 | 60/65 | 60/105 | 60m | 116 / 84 | unchanged, omitted |
| Certified catalog | 50/194 | 60/344 | 60m | 379 / ~275.2 | 1 |
| Community catalog | 50/148 | 67/317 | 70m | 349 / ~253.6 | 1 |
| Red Hat catalog | 50/199 | 107/503 | 110m | 554 / ~402.4 | 1 |
| OAS x3 | 50/192 | 60/211 | 60m | 233 / ~168.8 | unchanged, omitted |
| Package server x3 | 30/176 | 154/264 | 160m | 291 / ~211.2 | unchanged, omitted |

The full 92-selector table with exact Go byte values is in
`budget/handoff/comparison.md` under the artifact root below. Audit sidecars grow
from 8Mi to 52Mi by their unchanged historical weight; the prior low floor is
retained, not treated as a ceiling. Catalog/OLM operator mains stay at 20m/64Mi.

## Health Risks

- Saved small07 diagnosis records all three KAS OOMs at 616Mi and post-restart
  memory.current around 434-543Mi. New 932Mi hard and ~677.6Mi Go soft limits are
  above those recovery samples. These are cgroup memory, **not verified RSS** or
  startup maxima. Historical quiet peak charge was ~829.19Mi, also below 932Mi.
- KAS CPU stays 110m quota despite the larger budget. Prior pressure reached
  ~106.9m use with heavy throttling; this treatment may not fix CPU/lease latency.
  KCM CPU stays 50m; prior lease loss need not be a KCM memory problem.
- Historical quiet peak charges: certified ~374.23Mi, community ~341.49Mi,
  Red Hat ~552.05Mi, etcd ~482.01Mi, CPO ~93.71Mi, KCM ~144.10Mi. All new hard
  limits exceed those values, but catalogs have only about 2-8Mi headroom. Go
  soft limits are below these peaks; GC, non-Go memory and concurrency can still
  cause pressure. These are not simultaneous demand or proven safe RSS bounds.
- No fresh per-component RSS qualification is claimed. Recommendation: test this
  exact arm with OOM/restart history, RSS/cgroup charge, CPU throttling and lease
  deadlines; do not silently increase selected limits beyond this budget.
- Actual personal test workers use D4v3, unlike the illustrative D16s_v6 pricing
  model. Cost equivalence is not density, schedulability or throughput proof.
  Fixed downstream requests do not enforce a downstream cap; absent layouts,
  rollout surge and excluded instrumentation remain limitations.

## Lifecycle Handoff

Artifact root: `_artifacts/go-small-75pct-20260915/` in ARO-HCP-perf-runner.

- Full wire: `budget/native/hypershift.json`.
- Wire SHA256: `9a8c5cdc8ddd77f8bd1d24c1f029bd25583b5f4c5ab752a72aca0af1cd760371`.
- Input SHA256: `f23f9b6a2766e04af7a1f0901146a69eef554885abdfc76413b928a07e33409b`.
- Fallback: **2m CPU / 21Mi memory**, CPU limit=request, memory limit 110%, Go 80%.
- `budget/handoff/plan.json` binds image, source input, binary and wire hashes;
  it is non-executable, not a copied live lifecycle spec.
- `budget/handoff/fee-cost-input.json` and `fee-cost.json` explicitly use fee
  250000 micro-USD and targetPercent 75. Native allocator cost output represents
  the same target as 187500 micro-USD at 100%; both comparisons are exact.
- `bin/hcp-perf` was built from the complete frozen module under
  `provenance/source/`, including dirty/untracked code. Source hashes and Git
  HEAD/status/staged and unstaged diffs are archived; not mislabeled a clean commit.
- Existing CLI allocate/check used the same frozen binary. Frozen budget/sizing/
  cost race tests and preparation floor/runtime invariants passed. Hash checks
  cover all native and handoff outputs; prior candidates remain unchanged.

From the artifact root, reproduce the native immutable check:

```bash
./bin/hcp-perf budget check --input budget/input.json --output budget/native
```

Lifecycle owner must supply fresh guards/identities and verify the unchanged
image/runtime selections before testing. No new cloud resources or live settings
were changed during this preparation.
