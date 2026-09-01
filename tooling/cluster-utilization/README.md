# cluster-utilization

A "what do we use our clusters for?" report for ARO-HCP. It answers:

- **Where is all my compute going?** — capacity vs. requests vs. sustained usage,
  broken down per region/cluster and by namespace → workload.
- **Which workloads are not tuned correctly?** — top-N workloads whose sustained
  usage exceeds their requests (under-provisioned) or sits far below them
  (over-provisioned), plus workloads consuming compute with no request at all.
- **Where do our requests miss the mark?** — commitment (requested/allocatable),
  utilization (used/allocatable), and efficiency (used/requested) at every level.

Each ARO-HCP cluster is exposed as a separate Prometheus (Azure Monitor
Workspace) datasource inside Azure Managed Grafana. The two workspaces hold
**disjoint** workloads and are analyzed separately:

- `services-*` — platform / service / management-cluster workloads.
- `hcps-*` — hosted control planes (the per-HCP `ocm-*` namespaces).

## Workflow

The tool runs in three stages that share an on-disk cache, so the (slow) query
stage runs once and the (fast) analysis/render stages can be re-run offline
while iterating on normalization heuristics.

```
collect  -> .cache/<env>__<workspace>__<cluster>.json   (queries Grafana)
process  -> report.json                                  (offline: rollup + normalize)
render   -> report.html                                  (offline: self-contained HTML)
```

### 1. collect

Queries every matching Prometheus datasource for:

- node capacity/allocatable + instance type (`kube_node_status_*`, `kube_node_labels`)
- per-container requests & limits (`kube_pod_container_resource_{requests,limits}`)
- pod → workload ownership (`namespace_workload_pod:kube_pod_owner:relabel`, with
  a `kube_pod_owner` + `kube_replicaset_owner` fallback)
- sustained per-pod usage: `quantile_over_time(<p>, (max by (cluster,namespace,pod,container)
  (<usage>))[<window>:<step>])`, preferring the mixin recording rules
  (`node_namespace_pod_container:*`) and falling back to raw cAdvisor.

Usage queries are **batched by namespace** to keep each long-window subquery
under Grafana's HTTP timeout. Results are cached per cluster; a fresh cache file
is skipped unless `--refresh` is passed.

### 2. process

Reads the cache offline and splits each datasource's rows by the `cluster` label
into **per-cluster units** (a single `services-<region>` datasource carries many
clusters, e.g. `int-westus3-mgmt-1`, `-mgmt-2`, `-svc-1`), merging the services
and hcps workspaces for the same cluster. For each cluster it:

- derives env / region / role from the cluster name (`<env>-<region>-<role>-<n>`),
- classifies nodes into AKS pools (`system` / `infra` / `user`) from the node name,
- attributes each pod to its node pool (via `kube_pod_info`), so workloads carry
  the pool they run on,
- rolls pods up into workloads (deployment / statefulset / daemonset / job /
  unowned) and **normalizes similar namespaces** so per-cluster families
  aggregate into stable identifiers. Built-in rules collapse `ocm-*` (hosted
  control planes) and `klusterlet-*`; extend with `--normalize-rules`:

```yaml
rules:
  - pattern: '^ocm-int-.*'
    replacement: ocm-int
```

File rules take priority over the built-in defaults. Two transforms keep the
workload set meaningful rather than exploding in cardinality:

- **Current-pod filtering.** Usage is queried over a long window, so it also
  returns pods from old rollouts. Only pods that exist *now* (present in the
  instant owner/requests results) contribute to a workload's footprint.
- **CronJob collapsing.** `job` workloads named `<cronjob>-<timestamp>` collapse
  to `<cronjob>`, so hundreds of one-shot runs become one workload.

The output is a flat list of per-cluster units; the renderer builds all
navigation and rollups by filtering/aggregating them.

### 3. render

Emits a single self-contained `report.html` (data inlined; Tailwind + ECharts
from CDNs). The top selector is **multi-select clusters × multi-select node
pools** (system / infra / user); a workload is attributed to the pool its pods
run on, so selecting "user" pools scopes the compute view to where HCPs and
services actually run. A CPU/Memory toggle and a Used/Requested treemap toggle
re-render client-side; Top-N can be disabled to show every row. Each view shows
summary cards (commitment / utilization / efficiency), a node-pool capacity
breakdown, a per-cluster capacity/requested/used bar chart, a "where compute
goes" treemap (namespace → workload, with % of total), and top-N mis-tuned
tables.

## Usage

Authentication uses your ambient Azure credentials (`az login` / managed
identity) scoped to the Azure Managed Grafana service application. The tool
defaults `AZURE_TOKEN_CREDENTIALS=dev` when unset so `az login` works out of the
box; set it explicitly for CI/managed-identity runs.

Find the Grafana endpoint for an environment with:

```sh
az grafana list --query "[].{name:name, endpoint:properties.endpoint}" -o table
```

```sh
# one shot
make all GRAFANA_URL=https://arohcp-int-xxxx.suk.grafana.azure.com/

# or stage by stage
./cluster-utilization collect --grafana-url https://.../ --window 7d
./cluster-utilization process
./cluster-utilization render
open report.html
```

Key flags: `--datasource-pattern` (default `^(services|hcps)-`), `--window`
(`7d`), `--percentile` (`0.95`), `--namespace-batch` (`20`, auto-halved on
timeout), `--refresh`, `--normalize-rules`. Set `CU_VERBOSITY=1` for
per-datasource debug logging (recording-rule detection, batch halving, skips).
