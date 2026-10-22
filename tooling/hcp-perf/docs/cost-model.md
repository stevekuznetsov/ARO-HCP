# Compute Cost Model

For fixed-cost allocation rather than estimating an existing footprint, see
[Small Cost V1](cards/small-cost-plan.md) and `scripts/cost_allocate.py`. That
offline primitive subtracts fixed downstream requests, scales measured candidate
proportions without original floors, and charges rounded controlled memory limits.
It reuses this estimator for an independent selected-allocation check; the
`cost estimate` API below is unchanged.

`hcp-perf cost estimate --input examples/cost.json` consumes static JSON and
emits deterministic JSON. No Azure calls, live discovery, or price fetching are
performed. `cost.Calculate(Spec) (Result, error)` is independent of assessment,
scraping and CLI packages. Parent routing calls `runCost(args, io.Writer)` with
arguments after `cost`; the CLI uses the parent's `decodeFile(path, destination)`.

## Inputs

Version is `v1`, currency is `USD`. Money inputs are positive integer micro-USD
per hour, not floating point or decimal dollars: `250000` means $0.25/HCP-hour.
`targetPercent: 50` sets a $0.125 budget. Any positive int64 percentage is allowed,
including 100, 1000 and larger fee multiples. No fee increase is forced when an
estimate fails the target.

Each data point supplies the **entire HCP** aggregate demand in CPU milli, memory
MiB, HCP pod slots and Swift NICs. Sum all workload/container requests multiplied
by their replica counts before calling the calculator. KAS alone is not the HCP.
There is deliberately no container aggregator, pod discovery or extrapolation
helper. Guest workload pods are not management-cluster HCP pod-slot demand.

Use `sourceKind: requests` for configured request forecasts. For observed inputs,
use `sourceKind: observed`, `cpuStatistic: mean` or `p95`, and
`memoryStatistic: rss`. Aggregate simultaneous replica samples before computing
the HCP CPU statistic; a sum of individual p95s is not the HCP p95. Record the RSS
aggregation/window in the evidence artifact. Do not add request and observed
values together or treat observed usage as guaranteed schedulable requests.

`evidence` optionally contains `{ "path": "artifact.json", "sha256": "<64 hex digits>" }`.
References, demand, source kind, statistics, guest-node count, app count and SKU
metadata are carried into each output estimate for later assessment. Hash syntax
is validated, but files are not read or hashes verified. Missing references warn;
even with references this calculator does not independently label demand known
or qualify a configuration. Later assessment must verify evidence and scope.

## Capacity And Prices

Every SKU requires a unique name, region, positive hourly price, source URL and
RFC3339 retrieval timestamp. Supply the exact SKU generation and your retail
snapshot's retrieval date. Prices and metadata are user claims, not verified here.

- `capacityBasis: allocatable`: use Kubernetes allocatable CPU/memory, then subtract
  only the additional configured system/DaemonSet overhead not already excluded.
- `capacityBasis: usable`: the `allocatableCPUMilli`/`allocatableMemoryMiB` fields
  hold explicitly post-overhead capacities. Overhead must be zero to avoid double counting.
- `capacityBasis: raw-illustration`: use separate `rawCPUMilli`/`rawMemoryMiB`
  fields and zero allocatable fields. Configured overhead is subtracted, defaulting
  to zero. Every estimate warns that allocatable capacity is unknown.

CPU/memory overhead must be nonnegative and cannot exceed capacity. Optional
`usablePodSlots` and `usableNICs` are already post-overhead. An absent optional
capacity excludes that dimension with a warning; an explicit zero capacity with
positive demand is an error. Zero demand contributes no constraint, even with
zero capacity. An entirely empty HCP has zero cost and a null (unbounded)
homogeneous capacity, not a realistic qualified configuration.

The example uses user-supplied West US 3 price assumptions of $0.806/hour for
`Standard_D16s_v3` and $1.058/hour for `Standard_E16s_v3`, with raw 16-core,
64/128-GiB capacity. Names/generation and retrieval timestamps in this illustrative
fixture must be replaced with the exact records from your existing
[Azure retail snapshot](https://prices.azure.com/api/retail/prices) before use as
price evidence. The fixture timestamp is illustrative, not a claim of a fresh
query. Filter for the intended Linux consumption meter and currency; do not mix
Windows, spot or reservation prices. No allocatable capacity was measured here.
Actual testing on a four-core D4s v3 does not cost-qualify either 16-core SKU.

## Calculation

For each SKU/data-point pair:

```text
share = max(cpu / usableCPU, memory / usableMemory,
            podSlots / usablePodSlots, swiftNICs / usableNICs)
resourceEquivalentCost = nodePrice * share
ratioOfFee = resourceEquivalentCost / fee
budget = fee * targetPercent / 100
budgetCPU = floor(usableCPU * budget / nodePrice)
budgetMemory = floor(usableMemory * budget / nodePrice)
theoreticalHomogeneousCapacity = floor(1 / share)
```

Optional fractions exist only when their capacity is supplied. CPU and memory
must **both** fit their budget envelopes; optional dimensions must also fit for
`withinTarget`. Envelopes are integer-unit limits, not independently spendable
CPU and memory budgets. CPU and memory node-price shares are never added.
All ties appear in `bottleneck`, in stable CPU/memory/pods/NIC order.

`math/big.Rat` prevents intermediate overflow and makes target comparisons and
floor bounds exact. USD/hour and ratios are rounded decimal strings with nine
fractional digits for display, not financial accounting. Budget integer limits
are decimal strings so even large fee multiples cannot overflow JSON consumers'
integer types. `withinTarget` uses the exact value, not the rounded display.

This is a divisible-resource lower bound and a theoretical homogeneous integer
upper bound, **not density qualification**. Fragmentation, pod shapes, HA,
anti-affinity, zones, failure headroom, latency/SLOs and scheduling restrictions
can reduce achievable density. Capacity greater than a node yields an integer
bound of zero, rather than pretending that HCP fits on that node.

Optional `pool: { "hcpCount": 10, "nodes": [{ "skuName": "Standard_D16s_v3", "count": 3 }] }`
reports `actualPackedCostUSDhr = sum(nodeCount * nodePrice) / hcpCount`. This is
inventory allocation, not simulated packing: the full node bill includes idle
capacity, overhead and system workloads on the supplied nodes. Service/system
infrastructure outside the pool is excluded. All results are compute-only and
exclude PVC/storage, network and service charges, guest worker costs and any
unlisted infrastructure. Supply the correct management pool and denominator.

## Assessment Workflow

Keep three guest nodes fixed and supply data points for the desired 0..120-app
scan (five guest pods/app means 600 at the endpoint). The example contains only
two invented endpoint footprints to demonstrate the schema, not benchmark data
or an inferred growth curve. Derive each complete HCP footprint from configured
replicas or measured evidence. If exploring KAS-anchor proportions, preserve
those container proportions upstream and retain the anchor and scaling rule in
evidence; do not equate guest pod count or KAS demand with total HCP demand.

Compare target results across the supplied SKUs. If none meets $0.125/hour, report
that economic result. A smaller/cheaper candidate is useful only after independent
viability testing; this estimator neither raises the fee nor calls a theoretical
minimum a viable deployment. Preserve the data points and warnings for subsequent
performance and density assessment.
