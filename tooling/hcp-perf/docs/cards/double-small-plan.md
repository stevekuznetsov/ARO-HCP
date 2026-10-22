# Double Small Plan

**Render only, unqualified candidate. Do not apply to the running/preserved small
card.** Finish that card first; a later 2x trial needs separate approval and a new
evidence boundary. No Azure calls, policy application, node changes, or source
changes outside the profile and sizing tests are part of this preparation.

## Meaning Of 2x

Follow the [agent methodology handoff](../tshirt-methodology.md) before deployment.
It separates maintained primitives from the private supervisors used for Small v1
and lists the startup, capacity, capture, cleanup and comparison gates to repeat.

`examples/double-small-v1.json` doubles **all policy-controlled CPU and memory
requests**, including fixed floors and unlisted regular/init containers when
rendered with the fallback flags below. It does not promise 2x throughput,
observed usage, management capacity, or whole-cluster resource consumption.

Doubling only the KAS anchor (250m/1024Mi to 500m/2048Mi) while retaining ratios
and original floors is a valid anchor-relative experiment, but **not a full total
doubling**: fixed helpers and renderer defaults would remain unchanged. That is
not the literal 2x experiment selected here. Both anchors and every floor double;
ratios remain unchanged. All current quantities resolve exactly at 1m/1Mi, so
rounding quanta stay unchanged. Tests compare every resolved request and safety
limit, not just anchors. If future fractional ratios break exact doubling,
doubling the original quanta as well preserves the scaling identity.

The baseline is the actual `small-catalog-v2.json`, not the earlier incomplete
`small.json` or `small-complete.json`. Offline comparison against
`_artifacts/perf-resource-locked-scan/apf-shares-comparison-20260910-1832/after/resource-policy.json`
(relative to the sibling repository root) confirms both `hc` and `hcp` match the
baseline render after quantity normalization (`1Gi` = `1024Mi`). There are **17
overrides**, not 18 or 21. Catalog memory ratios are 1/4 in this baseline, not
zero; their floors still double from 256Mi to 512Mi. No inventory is invented.

| Budget | Baseline | Double Small |
| --- | --- | --- |
| KAS CPU / memory request | 250m / 1024Mi | 500m / 2048Mi |
| KAS memory safety limit | 3072Mi | 6144Mi |
| KAS GOMEMLIMIT | 858993459B (~819.2MiB) | 1717986918B (~1638.4MiB) |
| etcd CPU / memory request | 100m / 256Mi | 200m / 512Mi |
| etcd memory safety limit | 768Mi | 1536Mi |
| Each catalog registry request | 50m / 256Mi | 100m / 512Mi |
| Each catalog registry safety limit | 768Mi | 1536Mi |
| HAProxy and fallback requests | 10m / 64Mi | 20m / 128Mi |
| HAProxy and fallback safety limit | 192Mi | 384Mi |
| One-of-each override request subtotal | 1520m / 3904Mi | 3040m / 7808Mi |
| One-of-each override safety-limit subtotal | 11712Mi | 23424Mi |

Subtotals count each of the 17 overrides once, including the potentially absent
marketplace catalog; they exclude fallback containers and replica multiplicity.
They are **not HCP totals or management-node capacity**. GOMEMLIMIT is
`floor(requestMiB * 1048576 * 80 / 100)` bytes, not twice the already-rounded old
value (etcd becomes 429496729B). The 3x safety multiplier and 80% Go target remain
unchanged. The two non-Go HAProxy overrides retain `goMemoryLimitPercent: 0`;
`GOMAXPROCS` remains unset and CPU limits remain absent.

## Offline Verification

Run from the exact sibling `ARO-HCP-perf-runner/tooling/hcp-perf` directory:

```bash
GOWORK=off go test ./internal/sizing -run TestDoubleSmallV1Example -v
GOWORK=off go run . sizing resolve --profile examples/double-small-v1.json
GOWORK=off go run . sizing hypershift --profile examples/double-small-v1.json \
  --default-cpu-milli 20 --default-memory-mib 128
sha256sum examples/small-catalog-v2.json examples/double-small-v1.json
```

The fallback flags are **required**: the CLI defaults remain 10m/64Mi and are not
derived from the profile anchor. In the rendered CRD,
`spec.sizes[0].effects.containerResourcePolicy.defaultRequests` is
`{"cpu":"20m","memory":"128Mi"}`, versus `{"cpu":"10m","memory":"64Mi"}`.
Every `containers[].requests.cpu` and `.memory` is also exactly 2x.
`memoryLimitMultiplier: 3` gives fallback memory 384Mi; `goMemoryLimitPercent: 80`
gives fallback GOMEMLIMIT 107374182B. This covers containers owned by the
HyperShift projector, not management services outside it.

File SHA-256 (raw profile bytes, not a rendered policy hash):

```text
2dba5969c6af970b3190fa349ddfe60917e98af50a2b26bac646ef499d3c3783  examples/small-catalog-v2.json
84bd7a5598de9f8a40331a54df3ee2837c92fdbd654443bfca2b9d07e165f3b7  examples/double-small-v1.json
```

`sizing resolve` reports `profile: double-small-v1`; the rendered CSC still uses
`spec.sizes[0].name: small`, `criteria.from: 0`, no upper bound, and
`management.placeholders: 0`. Capacity is omitted, not inferred from the anchor
or the subtotal. This renderer does not resize a management pool.

## Later Trial Gate

Keep API/APF shares and `maximumRequestsInflight: 150` /
`maximumMutatingRequestsInflight: 50` unchanged for the first comparison, isolating
resource effects. Preserve the existing 5-second catalog probe settings, images,
actual three-replica HA components, guest node count/SKU, application population,
and offered load. Do not widen the guest node population or tune probes alongside
resources. These controls are not all encoded in the profile; verify them at the
future trial boundary. Existing small profiles and cards remain untouched.

**Management placement/headroom is a blocking preflight, not an aggregate check.**
The supplied planning estimate doubles HCP CPU requests from about 3.87 to 7.74
cores. Across three D4 management nodes, 11.58 allocatable cores minus roughly
3.04 platform cores leaves apparent aggregate room (10.78 cores combined). But
the supplied worker-1 skew would put about 4.4 cores of HCP requests alone on a
node with 3.86 allocatable cores. That placement cannot schedule, even before
platform pods. These estimates are not a fresh live placement verification.

Before any approval, recompute replica-weighted requests from actual regular/init
containers, pod overhead, platform/DaemonSet reservations, rollout surge,
affinity/anti-affinity, topology constraints, and current allocatable CPU/memory
per eligible management node. Require a suitable new management pool or verified
autoscaler capacity to add nodes of the same SKU if necessary; neither change is
authorized here. Rediscover HCP placement and competitor/adversary placement and
budgets after scheduling rather than replaying old node assignments. Record any
management-capacity change as a comparison caveat. Do not infer safety from the
3x memory limits or aggregate CPU arithmetic.

Only after the old card is preserved, placement is feasible, and a separate trial
is approved should the [personal apply gates](../hypershift-sizing.md#explicit-apply-gate)
be considered. The CSC is management-cluster-wide; replacing it can retune other
HCPs, including the preserved one. Verify projector coverage, templates and actual
Pods, fallback/init requests, memory limits, runtime exceptions, and rollout
convergence before collecting a new card. This plan is not permission to apply.
