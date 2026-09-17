# HCP Scheduling Simulator

A what-if planning tool that answers: **given a region's fleet of hosted
clusters, what management-cluster node pools do we need?** It models the
[minimal-zonal control-plane scheduling](../../enhancements) design — a scarce
**balanced-AZ (zonal)** triplet plus cheap **overflow** capacity — and minimises
the scarce zonal cores, subject to CPU, memory, and **SWIFT-NIC** constraints.

## Compute and cost pane

The bottom results pane shows per-size modeled control-plane CPU/memory requests,
pod/NIC demand, and amortized MC worker-VM costs for both policies. Prices are a
bundled Azure Retail Prices API snapshot in `sample_inputs/prices.json`: East US 2,
USD, Linux pay-as-you-go, non-Spot, retrieved 2026-09-08. The pane displays the
snapshot date and priced node inventory; edit the snapshot to update rates or
use another region. It does not fetch prices during a solve.

Each MC pool's complete VM bill is allocated to its real HCPs using normalized
dominant-resource shares (CPU, memory, NICs, pods). This includes idle capacity,
node overhead and all reserves, but excludes placeholders from customer counts.
Per-size cost times fleet counts reconciles to the complete VM bill. Monthly
estimates assume 730 hours; absent sizes have demand figures but no cost estimate.
This is not marginal cost, a dollar-optimized placement, or complete service TCO:
storage, networking, AKS fees, licenses, service clusters and labor are excluded.

Accounting checks: `venv/bin/python -m unittest test_costing test_pool_accounting`.

## Quick start (container)

For the experimental full regional CP-SAT solver and greedy comparisons, see
[CP-SAT experiments](optimize/README.md). The website's default solver is unchanged.

For real deployed MC usage, see [Observed snapshots](observed/README.md).
The `/observed` page serves pre-generated Grafana/Kusto data without credentials
or live queries; `/` remains the optimization simulator.

```sh
make run          # builds the image and serves http://localhost:8099
make logs         # follow logs
make stop
```

or with compose (live-reload against the source):

```sh
docker compose up --build
```

## What it does

1. **Demand** per hosted cluster comes from *observed* control-plane usage
   (our perf runs at 3/6/12/30/49/60/120/250 worker nodes). The usage→request
   transform is a run input: `request = multiplier × p_N(per-replica usage)`,
   computed live over the whole run.
2. **Tiering** follows the real minimal-zonal pod dump
   (`sample_inputs/real_pods_minimal.json`). Under `minimal` there are three buckets:
   **zonal** (`req:zonal`, hard zone TSC) = `etcd` (3, strict 1/AZ) + the pairs
   `kube-apiserver`, `oauth-openshift`, `openshift-apiserver`,
   `openshift-oauth-apiserver`, `router`, `ignition-server-proxy`;
   **overflow** (`pref:overflow`) = float controllers + operators/catalogs;
   **unsteered** (no node-role in the dump: the CNO-managed `network-node-identity`,
   `multus-admission-controller`, `ovnkube-control-plane` + CSI controllers) whose
   placement is a toggle — `overflow` (matches the dump) or `zonal` (enhancement
   target: the failurePolicy:Fail webhooks `network-node-identity`/`multus` steered
   zonal). Under `legacy`, everything is zonal at 3 replicas.
3. **SWIFT NICs**: one per router replica, charged to the zonal pools; per node a
   SKU offers `MaxNetworkInterfaces − 1` NICs. This is frequently the *binding*
   constraint on the zonal triplet.
4. **Pods-per-node cap** (default 225, minus the non-HCP pod overhead): a fourth
   packing dimension. With many small clusters the *overflow* pool becomes
   pod-count-bound rather than CPU/mem-bound.
5. **System reservation** is *absolute*, not fractional: the ARO-HCP kubelet
   DaemonSet (`mgmt-fixes/deploy/kubelet-ds`) pins `--system-reserved=cpu=3000m,
   memory=7550Mi` on every worker node. Two modes:
   - **flat** (as deployed): the same 3000m/7550Mi on every node, so small SKUs
     lose most of their cores (a D4/E4 keeps only ~0.5 of 4 usable cores).
   - **scaled**: the reservation scales linearly with vCPU, anchored so the flat
     figure applies at 32 vCPU — small nodes keep proportionally more.
6. **Non-HCP node overhead**: worker nodes also run kube-system daemonsets (CNI,
   CSI, monitoring agents), arobit, velero, mgmt-agent, etc. as *pods* consuming
   allocatable — additional to `--system-reserved`. Defaults (468m / 3592Mi /
   11 pods per node) were measured from `prod-uksouth-mgmt-1` via
   `demand/extract_node_overhead.py` against a cluster-utilization `report.html`
   (14d p95, concurrent-replicas × per-pod usage).
7. **Sharding**: HCPs are packed into management clusters up to a configurable cap
   (default 100), filling each MC to a target (default 95%) before opening a new one.
8. **Headroom is packable reserved pods**, not a node multiplier:
   - **rollout surge** — the *largest* HCP on the MC surges +1 pod per multi-replica
     Deployment (maxSurge=1); `concurrent_rolling_hcps` (default 1) reserves for the
     top-N largest. etcd (a StatefulSet) does not surge.
   - **AZ-death** (zonal only) — each AZ reserves `az_failure_reserve` (default 0.5)
     of its **non-etcd pair** footprint as copies of real pair pods, so a sibling
     AZ's pods can reschedule in. etcd is excluded (can't reschedule under
     `DoNotSchedule`/`minDomains`).
   Reserves are packed into existing node slack first; a new node opens only on
   overflow. (Fast mode approximates this by adding the reserve to aggregate demand.)
9. **Node pools** are sized per MC: a balanced zonal triplet (×3 AZ) and an
   overflow pool, choosing SKUs from a catalog to minimise scarce zonal cores
   (tie-broken by fewest nodes, then least provisioned memory).

Two solvers:

- **fast** (default): closed-form per-SKU sizing from aggregate demand + reserves.
  Sub-second. No per-node/pod detail.
- **precise** (checkbox / exact): First-Fit-Decreasing vector bin-packing over the
  actual pods, modelling every constraint pod-for-pod — capacity (cpu/mem/nic/pods),
  zone-spread (distinct AZs; etcd strictly 1/AZ), host-spread (distinct nodes),
  node-role (zonal vs overflow) — plus the reserved pods above. Retains the full
  packing (which pod on which node) that drives the visualization.

## Visualization

The result view is a **tetris packing dashboard** (canvas): each node is a grid of
cells (1 cell = a fixed unit of the selected resource) and each pod is a contiguous
blob sized by its footprint. Controls: **resource** (cpu/mem/nic/pods — the whole
view is single-resource), **lens** (node bin-packing vs per-HCP tenant cards),
**color** (by HCP or component), **policy** (minimal/legacy), **MC** (full ×N /
remainder), **density**.

- Each node shows a base band of **system-reserved** + **daemonset overhead**,
  working pod blobs, hatched **rollout**/**AZ-death** reserve blobs, and blank free
  cells; a per-pool **binding** badge (e.g. the memory view still tells you zonal is
  NIC-bound).
- **Click an HCP** (any of its pods) to highlight its whole control plane across
  every node and open a side panel breaking it down pod-by-pod (zonal vs overflow,
  reserve copies). **Click a node** for its pod list + all-dimension fill.
- State is shareable via URL hash, e.g. `#lens=hcp&resource=nic&policy=legacy`.
- The **precise** solver is required for the grid; in fast mode only the summary +
  raw-number tables show. Raw pool/comparison tables collapse under the dashboard.

## Layout

```
server.py            FastAPI app (form + /solve)
skus.py              VM SKU catalog loader (vCPU, mem, swift_nic = maxNICs-1)
demand/
  components.py      component -> tier / replicas(policy) / NIC
  constraints.py     real scheduling constraints from a parsed pod dump
  model.py           per-cluster demand (live percentile of observed usage)
  build_profiles.py  regenerate profiles.json from the perf cache
  parse_pod_dump.py  parse a raw Pod-dump CSV -> per-component constraints
optimize/
  engine.py          RunConfig, region demand, MC sharding, simulate()
  fast.py            closed-form pool sizing + SKU selection
  exact.py           FFD packing (constraints + packable reserves), retains placement
templates/
  index.html         input form
  result.html        tetris dashboard shell + inlined data + collapsible tables
static/
  tetris.js          canvas packing visualization (node/HCP lenses, picking)
  style.css
sample_inputs/
  skus.yaml          default SKU catalog (D/E-series v6, verified via `az`)
  profiles.json      observed usage profiles (prebuilt; shipped in the image)
  real_pods_legacy.json  constraints parsed from a real HCP pod dump
templates/, static/  HTMX + Jinja UI
```

## Regenerating inputs

- **Usage profiles** come from `hack/hcp-scale-analysis` (run its `extract_all.py`
  to produce `~/Downloads/controlplane-pod-timeseries.json`, then `make profiles`).
- **Constraint model** from a real pod dump:
  `./venv/bin/python demand/parse_pod_dump.py dump.csv sample_inputs/real_pods_<policy>.json`.

## Local dev (no container)

```sh
make venv     # python3.12 venv + deps
make dev      # uvicorn --reload on :8099
```
> Note: use Python 3.12 — jinja2 currently trips over a 3.14 stdlib change.
