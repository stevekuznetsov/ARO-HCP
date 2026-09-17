# Observed Placement

Collect a fixed Grafana/Kusto snapshot, process it offline, and inspect actual
management-cluster (MC) workloads at `/observed`. This is separate from the
scheduling simulator: there is no solver, inferred demand profile, hypothetical
reserve, or live telemetry query in the page.

The observed page uses the simulator's shared `tetris.js` canvas, node/HCP lenses,
legend and detail panels. Usage/Requests replaces the simulation policy selector;
actual MC names and pool/zone groups replace synthetic full/remainder MC shapes.
Each `ocm-arohcp*` namespace is an HCP bucket with a short `HCP 1`, `HCP 2`, etc.
label, assigned in sorted MC/namespace order independently of pod/node order.
Numbers are stable within a dataset and across views; adding namespaces in a new
dataset can renumber them. Full identities remain in the details. Component labels
prefer workload-owner metadata and strip standard Kubernetes generated suffixes
when ownership is unresolved.

Unknown indicators apply to that node/resource, not to unrelated fleet warnings.
Incomplete pod usage no longer hatches the entire remaining node capacity. Known
pod usage stays visible; affected pods carry Pending, historical sampling-gap or
lifecycle-conflict context. An asterisk marks a lower-bound pod total, while a
question mark denotes missing node capacity. The coverage summary counts affected
pods rather than repeated diagnostics; full errors and warnings stay expandable.
SWIFT requests use Kubernetes' sparse extended-resource semantics: after a
successful namespace-scoped request query, no SWIFT entry means zero requested
slots. Explicit requests from any workload count, not only routers. Failed or
unavailable queries remain unknown. Node allocatable minus these scheduled pod
requests gives unrequested slots, not measured physical attachment occupancy.
In the usage view dark remainder is capacity outside measured pod usage (including
idle capacity and host usage), not an estimate of unattributed consumption. Node
details show independent node utilization and its unused-capacity complement.

The Kusto query collects core `v1` Node snapshots alongside HostedClusters from
`kubernetesResourceSnapshots`. For each Grafana-observed node, preprocessing uses
metadata at or before T (last live observation for retired nodes) to fill missing
SKU, agent-pool, zone, capacity and allocatable values. Grafana values win if both
sources exist, with disagreements reported. Deleted, ambiguous, mismatched-provider,
future and more-than-24-hour-old Node snapshots are not used. Snapshot provenance
is shown in node details. No extra nodes or usage are synthesized from Kusto.
If Node snapshots have not landed yet, collection still works with explicit unknown
metadata. Existing raw bundles without `node_snapshots` remain processable; resume
collection to obtain this new metadata without repeating successful metric calls.

The KQL projects compact objects rather than full Kubernetes resources to reduce
the risk of hitting Kusto's 64 MB result-size limit. It retains identity and
lifecycle metadata (namespace, name, UID, creation/deletion timestamps and resource
version), Node provider ID, capacity and allocatable, and these selected labels:

- `hypershift.openshift.io/hosted-cluster-size`
- `node.kubernetes.io/instance-type`
- `kubernetes.azure.com/agentpool`
- `agentpool`
- `topology.kubernetes.io/zone`

Other labels, annotations, Node taints, unschedulable state and conditions are not
collected by this projection. The baseline plus subsequent history and delete
events are preserved; rows are not sampled or silently truncated to fit the limit.
Large histories can still exceed backend limits. Changing the projection changes
the KQL cache key, so a resume fetches updated Kusto history without repeating
unchanged Grafana calls.

## Collect

### Regional Peaks in the Last Week

Use `peak` instead of `collect` to find each region's busy period independently
from node metrics, then collect detailed inventory and pod usage for each region:

```sh
# Independent regional 15-minute memory peaks across production MCs, last 7 days.
venv/bin/python -m observed.cli peak \
  --environment prod --output .cache/prod-memory-peak

# CPU peak, five-minute window, reproducible end of the search horizon.
venv/bin/python -m observed.cli peak \
  --environment prod --peak cpu --peak-lookback 7d \
  --peak-end 2026-09-08T23:18:00Z --window 5m --step 1m \
  --output .cache/prod-cpu-peak

# View the merged regional dataset (different regions can have different times).
OBSERVED_DATA=.cache/prod-memory-peak/view.json \
  venv/bin/python -m uvicorn server:app --port 8099
```

`peak` defaults to memory, a seven-day search, a 15-minute averaging window,
and one-minute sampling. `collect` still defaults to one hour. All normal
environment, cluster, endpoint, cache and publication options also apply.
Repeatable `--region` limits detailed collection to those regions; peak search
still scans the selected environments before this filter is applied.
`--peak-end` bounds the search; `--at` is rejected because peak mode chooses T.
Neither existing dataset nor running server changes until the new bundle is
explicitly published and the server restarted/rebuilt.

CPU ranks summed non-idle node cores (excluding guest double-counting), using
five-minute rates. Memory ranks node total minus available bytes. Scrape replicas
are deduplicated before summing. The score is **absolute regional consumption**, not
percentage utilization or a sum of independently selected per-MC peaks. One
common `[T-window,T)` interval is selected **per region**, maximizing its mean rather than a
single instantaneous spike. Exact ties choose the most recent window. Use a
narrower `--cluster` scope to find the peak for an individual MC.

The scan runs low-cardinality node/inventory queries in daily chunks. Pod queries
run only for each region's final window, keeping week-long high-cardinality pod collection out of
the flow. Samples must cover all MCs in that region observed anywhere in the search horizon for
the complete candidate interval and at T. Missing samples are not zero, and any
failed scan datasource blocks selection even with `--allow-partial`: an incomplete
scan cannot establish regional peaks. Cluster aggregates do not prove every node
scrape was present. Fleet changes or poor telemetry may leave no qualifying common
window; narrow the scope or horizon rather than silently dropping an MC.

Before detailed collection, a compact HostedCluster-only Kusto preflight retains
the latest baseline strictly before the original window start minus `--size-settle`
(S), with no arbitrary lookback floor, and state changes from S through the frozen
search end. Kusto projects only identity, lifecycle metadata and the size label,
then sorts by environment, region, MC, UID and timestamp and compares consecutive
rows with `serialize`/`prev`. Only the first row per UID, size changes and changes
in Delete status are returned as compact objects. Repeated samples and Add/Update
churn do not inflate the response; `small -> medium -> small` retains both resizes,
unlike grouping by size. Node snapshots and full Kubernetes metadata are excluded.
This reduces the risk of the 64 MB result limit without sampling or truncating
state runs; backend failures still block preflight.

Only actual known-to-known size changes count as resizes per MC and HC UID.
Initial Add/size assignments, including `None -> e2e`, do not move the window.
Unknown samples do not erase the last known assigned size: `small -> unknown ->
medium` counts as a resize when medium is observed, but `small -> unknown -> small`
does not. Unknown intervals remain the detailed processor's validation concern.
Delete/Deleted events terminate the UID; later samples cannot revive it.

An affected window moves **forward** past the transition and settling interval,
then includes a full averaging window (15 minutes by default). All later
transitions through the search end are considered before collecting once. The
adjusted window must end at/before that bound; otherwise that region is blocked.
The original selected timestamp and score remain unchanged in `peak_selection`;
`adjusted_at`, `adjusted_start`, and `adjustment_seconds` describe the collected
window. No new peak score is claimed for an adjusted window, nor is it claimed to
be the next-highest stable peak.

Unavailable Kusto blocks that region but does not stop other regions. Detailed
collection still runs the normal processor validation for missing/stale metadata,
stability and coverage; preflight does not bypass it or enable `--allow-partial`.
Short windows may also expose CPU rate warm-up gaps for newly started containers.

`.peak-selection.json` freezes the horizon end before querying; `peak-search.json`
retains the scan results, source coverage, ranking settings and regional winners.
Rerun the identical command to resume at the same horizon using cached responses.
Use a new output directory for a new horizon or different ranking settings.
`--refresh` re-queries the frozen horizon for late ingestion. A changed final time
gets a separate timestamped child directory; prior child bundles remain intact.
Unchanged preflight calls use the frozen search end as their cache partition;
normal detailed collection uses its own T and separate HC/Node KQL. Unchanged
reruns reuse both caches without authentication. Preflight raw responses and
baselines remain in `preflight.json`, and detailed raw data remains independently
processable. Browser loading stays offline.

To run only one region's preflight from an existing regional peak selection,
without repeating the weekly scan or collecting detailed metrics, invoke the
function directly from `hack/scheduling-simulator`:

```sh
venv/bin/python -B - <<'PY'
import argparse
import json
from pathlib import Path
from observed.cli import regional_preflight

output = Path(".cache/prod-memory-peak")
selection = json.loads((output / "peak-search.json").read_text())["selection"]
regional = next(r for r in selection["regions"]
                if (r["environment"], r["region"]) == ("prod", "uksouth"))
args = argparse.Namespace(output=output, cache_dir=Path(".cache/observed"),
                          refresh=False, kusto=[],
                          window=selection["window_seconds"], size_settle=900)
record = regional_preflight(args, regional)
print(json.dumps({k: record[k] for k in ("transitions", "warnings")}, indent=2))
PY
```

Use the original settling interval in seconds and any endpoint overrides in
`kusto` (for example `["prod/uksouth=https://query-endpoint"]`). This calls only
that region's Kusto endpoint on a cache miss and writes its `preflight.json`;
it does not adjust windows or publish a view. Changed KQL invalidates older
preflight query caches automatically. This is not an offline command unless
matching successful responses are already cached.

Peak output has this structure (`at` is the final Unix timestamp in seconds):

```text
output/
  .peak-selection.json
  peak-search.json
  regional-manifest.json
  view.json                         # only when merged publication is allowed
  regions/
    prod-uksouth/
      preflight.json                # full compact response, query, bounds, diagnostics
      {at}/
        raw.json
        manifest.json
        view.json                   # only when child publication is allowed
        .processed.json
```

`regional-manifest.json` is checkpointed atomically before collection and after
every region, recording original/adjusted windows, successful paths, blocked
reasons and diagnostics. Existing legacy root `raw.json` is not overwritten;
peak no longer collects detailed data into the root. Use a child's `raw.json`
with offline `process`, not the peak root.

The merged root view contains only allowed child views. Its top-level `at` and
`start` are null; each MC carries its own ISO timestamps and `peak_selection`,
and `regional_windows` records the regional outcomes. This is a **mixed-time**
dataset, not simultaneous fleet totals. Any regional error prevents the merged
view by default while retaining successful child views. `--allow-partial` permits
merging available child views with diagnostics, but never fabricates a blocked
region's data. Regional errors return `2`, even when a partial merge is published.

Run these commands from `hack/scheduling-simulator`. Create the local environment
with `make venv` if needed (Python 3.12; dependency installation needs network).

```sh
# All default Grafana environments and discovered management clusters.
venv/bin/python -m observed.cli collect --output observed-data

# Run the exact command again: reuse the original T, not a new wall-clock time.
venv/bin/python -m observed.cli collect --output observed-data

# Bypass both cached responses and prior raw records to pick up late ingestion.
# The same output still pins the original T.
venv/bin/python -m observed.cli collect --output observed-data --refresh

# Reproducible end time T, aligned to the one-minute sample grid.
# Use a new output directory when changing time or metric scope.
venv/bin/python -m observed.cli collect \
  --at 2026-09-08T12:00:00Z --window 1h --step 1m \
  --output /tmp/opencode/observed-20260908-1200

# Optional scope: --environment and --region may be repeated; --cluster is a regex.
venv/bin/python -m observed.cli collect \
  --at 2026-09-08T12:00:00Z --window 1h --step 1m \
  --environment int --region uksouth --cluster '^int-uksouth-mgmt-1$' \
  --output /tmp/opencode/observed-int-20260908-1200
```

Choose a timestamp within telemetry retention. Without `--at`, a new collection
uses now minus five minutes, rounded down to `--step`; a resumed collection uses
the prior `raw.json` T. Explicit timestamps must include a
timezone: the literal `--at` must align to `--step`; an explicit end time is not
silently rounded to the grid.
Durations are positive integers followed by `s`, `m`, `h`, or `d`.

Nonempty range series must be contiguous subsets of the requested ticks from
window start through T, inclusive, at `--step` spacing. Grafana may omit leading
or trailing ticks for short-lived series; these aligned subsets (including a
single tick) are valid. Interior gaps, coarser spacing, reordered or duplicate
ticks, shifted timestamps, and samples outside the window are rejected.
When frame metadata includes an `executedQueryString` with a reported `Step`, it
must also match the requested step: `Step: 5m` is rejected for `--step 1m`, even
if the returned timestamps alone look valid. Recollect with the reported step
when Grafana changes resolution. Null values on the correct grid remain unknown
samples. Genuine empty frames are accepted as empty results, not fabricated
zero-valued series. Accepting a trimmed grid does not establish complete pod
coverage; the processor still checks telemetry against observed inventory.

| Option | Default | Constraint / meaning |
| --- | --- | --- |
| `--window` | `1h` | Positive multiple of step; usage interval is T minus window through T |
| `--step` | `1m` | Must not exceed window; controls Grafana range-query resolution |
| `--search-back` | `24h` | At least window; HostedCluster history and stable-window search horizon |
| `--size-settle` | `15m` | Positive settling interval after an assigned-size change |
| `--workers` | `4` | 1 through 8 concurrent metric jobs |
| `--environment` | All defaults | Repeatable `int`, `stg`, `prod` |
| `--region` | All discovered | Repeatable regional datasource filter for detailed collection |
| `--cluster` | `.*-mgmt-\d+$` | Regex applied to discovered MC names |
| `--output` | `observed-data` | Absent/empty directory starts a collection; a collector `raw.json` enables resume |
| `--cache-dir` | `.cache/observed` | Shared validated response cache, relative to the working directory |
| `--refresh` | Off | Bypass response cache and prior raw records; replace successes with freshly validated responses |

### Cache And Resume

Successful per-call responses are saved atomically and reused across output
directories. Grafana keys include the full query URL, resource audience, datasource
UID, expression, start, T, step, range/instant mode, and cache format revision.
Kusto keys include the full URL, resource audience, database and complete KQL.
All entries, including datasource discovery and Kusto authentication metadata,
are partitioned by collection T. Tokens are never keys or stored values. Cache
hits need neither a token nor an Azure CLI invocation.

Grafana frames and sampling grids and Kusto result parsing/diagnostics must pass
before a query response is saved. HTTP-200 backend errors, partial Kusto failures,
and malformed results are not cached. Valid empty results are cached. Entries
are revalidated on read; corrupt or invalid entries cause a new request. Atomic
sibling-file replacement prevents partial JSON reads. `--refresh` invalidates each
visited old entry before fetching, so a failed refresh cannot fall back to that
entry on the next run.

Resume rebuilds `raw.json`, snapshots, errors and warnings rather than appending
to old results, then publishes from new or cached processing results. Matching successful metric records from prior raw
data seed the cache after revalidating their original responses. Matching includes
environment, region, cluster, metric, expression, times, step, datasource, mode
and endpoint (legacy records use the source environment URL). These raw records
take precedence over shared cache entries, preserving the capture even if another
output refreshed the same query. Failed records are
retried, not accepted as successes. Checkpoints carry an incomplete-collection
error until collection finishes; old error lists are not copied into a new run.
A resumed run removes its old view and manifest before collection begins, so an
interrupted or failed run cannot expose a stale successful publication.

`collection_config` in raw data and the manifest records T, window, step,
environments, cluster regex, Grafana/Kusto mappings, settling interval and search
horizon. Regions are recorded only when `--region` is supplied, so unfiltered
legacy bundles retain their resume contract. Repeat the original scope/time options when resuming; only omitted
`--at` is inherited automatically. Changing these settings in-place is rejected,
even with `--refresh`; use a new output directory instead. Kusto mapping changes
are allowed: metadata is rebuilt using per-query cache entries, so changing KQL
(such as adding Node history) fetches only new queries, not hundreds of unchanged
metrics. Workers, cache location and partial-publication policy may also change.
Nonempty directories without a valid collector raw bundle are not resumable.

Legacy raw bundles have no recorded cluster regex. Their time, environment and
Grafana endpoint settings are checked, and original discovery responses are
filtered using the supplied regex; use the original regex for faithful resume.
This missing setting cannot be recovered from already-filtered series. The next
run records it explicitly. Legacy Kusto responses are not reused directly from
raw data; expect one metadata/query fetch per region when no disk cache exists.

Both `collect` and offline `process` also use an output-local `.processed.json`
cache to skip heavy processing when inputs have not changed. Its digest includes
the bytes of `observed/process.py` and the raw processing inputs, including
normalized metric series, HostedCluster/Node snapshots, errors and warnings.
Original metric `response` envelopes and `kusto_queries` are excluded because the
processor consumes their normalized data instead. Metric query records are sorted
for hashing so concurrent completion order does not cause unnecessary processing.
Changing inputs or processor source invalidates this cache. Unlike the shared
response cache, it is not automatically reused across output directories.

The processed cache is written atomically and reused only with a matching digest
and supported view schema version. Unreadable or malformed JSON and incompatible
schema versions cause reprocessing. A cache hit preserves the original
`generated_at`, but still regenerates the manifest and applies the current
publication policy: a cached partial result cannot publish without
`--allow-partial`, and no discovered MCs remains an error. The hidden cache file is
not a published view. Treat it as sensitive telemetry just like `view.json`.
`--refresh` refreshes telemetry responses, not this processing cache; if refreshed
inputs are unchanged, heavy processing can still be skipped. Delete only the
output's `.processed.json` to force offline recomputation without recollecting.

There is no cache expiry, eviction or cross-process collection lock. Use separate
output directories for concurrent collections. Cached telemetry is a local
snapshot, not a guarantee against ingestion delays or backend retention changes;
use `--refresh` for late-arriving data. Cache directories contain sensitive
telemetry and are not partitioned by signed-in user: the endpoint/resource
boundary prevents cross-audience reuse, not local access by another user. Keep
them private, do not commit or distribute them, and keep them outside container
build contexts when they should not be included. Delete the cache when no longer
needed; raw bundles remain independently processable.
Reproducibility applies to saved telemetry inputs, not byte-identical artifacts:
generation timestamps on cache misses and concurrent query completion order can vary,
and updated processing code can produce different derived views.

### Credentials

Install Azure CLI (`az`) and establish the appropriate identity and permissions
before collecting. The collector uses the **existing Azure CLI identity** via
`az account get-access-token`; it never runs `az login`, changes accounts, or
automatically logs in. Access to one environment does not imply access to all
three. Select `--environment` when the current identity has narrower access.
Authentication failures require you to authenticate for the target environment
and retry in the same output directory with the original options.

Grafana tokens use the Azure Managed Grafana audience. Kusto's authentication
metadata supplies its resource audience, including the MFA audience when
required. Tokens stay in collector memory and are not written into bundles or caches.
HTTP redirects are rejected rather than forwarding Authorization to another URL.
Offline processing and viewing require neither Azure CLI nor credentials.

### Endpoints

Default Grafana mappings:

| Environment | URL |
| --- | --- |
| `prod` | `https://arohcp-prod-g5d9a9akashnb5gd.suk.grafana.azure.com` |
| `stg` | `https://arohcp-stg-cackgmg7dtf6hzg3.suk.grafana.azure.com` |
| `int` | `https://aro-dev-int-dhbxfxe0hxc0fahf.eus.grafana.azure.com` |

Default Kusto mappings (database `ServiceLogs`):

| Environment / Region | URL |
| --- | --- |
| `int/uksouth` | `https://hcp-int-uk.uksouth.kusto.windows.net` |
| `stg/uksouth` | `https://hcp-stg-uk-2.uksouth.kusto.windows.net` |
| `stg/westus3` | `https://hcp-stg-us-1.westus3.kusto.windows.net` |
| `prod/eastus2euap` | `https://hcp-prod-usc.eastus2.kusto.windows.net` |
| `prod/switzerlandnorth` | `https://hcp-prod-ch-2.switzerlandnorth.kusto.windows.net` |
| `prod/australiaeast` | `https://hcp-prod-au.australiaeast.kusto.windows.net` |
| `prod/brazilsouth` | `https://hcp-prod-br.brazilsouth.kusto.windows.net` |
| `prod/canadacentral` | `https://hcp-prod-ca.canadacentral.kusto.windows.net` |
| `prod/centralindia` | `https://hcp-prod-in.centralindia.kusto.windows.net` |
| `prod/uksouth` | `https://hcp-prod-uk.uksouth.kusto.windows.net` |
| `prod/westeurope` | `https://hcp-prod-eu.westeurope.kusto.windows.net` |
| `prod/eastus2` | `https://hcp-prod-us.eastus2.kusto.windows.net` |
| `prod/westus` | `https://hcp-prod-us.eastus2.kusto.windows.net` |

Use repeatable `--grafana ENV=https://host` and
`--kusto ENV/REGION=https://host` mappings to override these or add Kusto regions.
The supported production mappings are included above, but `int/westus3` still
has no default: supply `--kusto int/westus3=https://your-query-endpoint` when
collecting that region. HTTPS is required; trailing slashes and a trailing `/dashboards` are
removed. Kusto endpoints are explicit mappings, not guessed from region names.
A discovered region without a Kusto mapping prevents default publication.

Grafana discovery uses regional `services-*` Prometheus datasources and their
paired `hcps-*` datasource. Node inventory discovers MCs; pod inventory discovers
namespaces, including pending pods. Non-`ocm-*` pod metadata comes from services,
`ocm-*` metadata from HCPs, and container usage from services. Coverage is the
**telemetry-observed fleet**, not an independent inventory of all deployed MCs.
The cluster filter does not redact original discovery responses in the raw data.

## Bundles

`collect` writes these files in the output directory; `peak` writes them in each
timestamped regional child directory described above:

| File | Contents |
| --- | --- |
| `raw.json` | Schema v1 collector inputs: collection config, absolute times, query expressions, normalized series, original Grafana/Kusto responses (including rejected responses), projected HostedCluster/Node metadata/history, provenance and collection errors; copied by `process` when writing a separate output bundle |
| `manifest.json` | Summary derived from the processed raw data: collection config when present, timestamps, sources, errors, warnings, transitions, suggested rerun, and `partial` flag; written only after processing returns a view |
| `view.json` | Processed schema v1 `mode: observed` data embedded by the server; old view removed before processing, new view written only when publication is permitted |
| `.processed.json` | Output-local processing cache containing an inputs/source digest and processed view, possibly partial; not read by the server and safe to delete to force recomputation |

Exit code `0` means a view was published without processor errors. Exit code `2`
means incomplete data, including no discovered MCs. By default, a partial run
retains raw data and writes the manifest but does **not** publish a view. A
reprocess removes an existing `view.json` before reading/parsing raw input or
running the processor, so invalid JSON and processing exceptions also cannot
leave a stale successful view behind. These failures do not generate a new
manifest; an older manifest may remain and should not be mistaken for a new
result. Resumed collection removes both old publication files once its settings
have been validated; rejected setting changes leave the existing bundle untouched. Invalid arguments,
input data, and other caught execution failures return `1` (argument-parser
errors use argparse's exit code `2`).

Reprocess a collected bundle entirely offline, without network, response-cache lookups,
Azure CLI or credentials (even if the raw data contains collection errors):

```sh
venv/bin/python -m observed.cli process \
  /tmp/opencode/observed-20260908-1200/raw.json --output observed-data

# Repeat unchanged inputs: reuse observed-data/.processed.json, still no network.
venv/bin/python -m observed.cli process \
  /tmp/opencode/observed-20260908-1200/raw.json --output observed-data

# Explicit opt-in is required to publish a view with processor errors.
venv/bin/python -m observed.cli process \
  /tmp/opencode/observed-20260908-1200/raw.json --output observed-data \
  --allow-partial
```

`--allow-partial` also works on `collect`. It writes the partial view but still
returns `2`; it does not make missing telemetry complete. `process` copies raw data
alongside the manifest and view when output differs from the source directory. Warnings alone do not
block CLI publication or set `manifest.partial`. The frontend is more cautious:
warnings, missing metadata, or incomplete coverage also trigger its "Partial
data / inspect coverage" notice, even for a CLI-successful bundle.

Kusto responses must include a result table with `object`, `timestamp`, and
`uid` columns; an empty matching table is valid, but unrelated diagnostic tables
alone are not. Unrelated property tables are skipped rather than parsed as primary
rows. Severity 3 diagnostics with a zero status code become warnings.
Severity 2 or lower, or any nonzero status code, marks the query incomplete even
if primary rows were also returned. Embedded dictionary rows containing
`Exceptions` in primary or severity tables also mark the query incomplete,
including result-size truncation reported after valid rows. These partial
responses are retained for diagnosis but never committed to the response cache;
their primary rows are not accepted as complete snapshots. Retained raw responses
do not imply that a complete view can be published.

## View And Share

By default, the server reads `observed-data/view.json` relative to the simulator
directory. `OBSERVED_DATA` is an **environment variable pointing to a view file**,
not a directory or an HTTP route:

```sh
OBSERVED_DATA=/tmp/opencode/observed-20260908-1200/view.json \
  venv/bin/python -m uvicorn server:app --port 8099
```

Open `http://localhost:8099/observed`. The file is read once at process startup;
restart the server after replacing it. A missing file shows an empty-state page;
invalid JSON/schema shows a load error. The view JSON is embedded in the HTML;
the browser does not fetch live telemetry or authenticate to Azure.

To embed a snapshot in an image, collect or process into `observed-data` and run:

```sh
make run
# Builds the image and serves http://localhost:8099/observed.

# Alternatively, build a named image to share through an approved channel.
make build IMAGE=hcp-scheduling-simulator:observed-20260908
```

The Dockerfile copies the build context, including `observed-data`. Sharing that
image shares the bundled snapshot without requiring the recipient to collect
again. Rebuild and redeploy when the snapshot changes; `make run` replaces the
local `hcpsim` container. Only `view.json` is needed for display, so keep raw data
outside the build context when it should not be included in the image.

**Sensitive telemetry:** raw HostedCluster (HC) objects contain metadata and can
include additional object fields, names, namespaces, UIDs, and infrastructure
details. Views, manifests, and source discovery responses also expose operational
information. Collector auth tokens are not stored, but this is not a general
redaction pipeline. Do not git commit generated/bundled files, and share bundles
or Docker images only through approved channels with authorized recipients.
`observed-data/` is git-ignored, not Docker-ignored; custom output paths may not be
git-ignored. The `/observed` page itself does not add authentication.

## Interpretation

- Node and HCP lenses show recorded placement. CPU and memory support window
  usage or requests at T; pod counts and NIC requests are at T only. HCP tiles
  group workloads by assigned size and do not invent node capacity.
- Usage is time-weighted over the **whole window**, including departed pods and
  retired nodes. A sample represents up to one step of time; the endpoint sample
  at T establishes current inventory and adds no duration to the average. A pod
  present for only part of the window contributes only that fraction. CPU uses
  Prometheus five-minute rates; memory is container working set. Node gauges are
  independent measured averages, not adjusted to match pod totals.
- Range-query resolution is not continuous observation. Scrape gaps, ingestion
  delay, and short-lived pods can be missed or under-sampled between ticks;
  reported full coverage cannot prove that every short-lived workload was seen.
- Pending does not prove zero usage. Missing active-pod samples or phase coverage
  are errors; usage gaps remain unknown/null rather than idle capacity. Unknown
  requests, NIC capacity, labels, and placement are not replaced by simulator
  defaults. Unplaced workloads remain visible without a synthetic node.
- Current requests/counts exclude departed pods. Retired nodes retain historical
  capacity, not capacity available at T. In requests mode, only current nodes add
  reported capacity minus allocatable. In usage mode, blank space is
  **unattributed, not free**. Inspect the quality notice and workload details,
  including zero/unknown values, before drawing utilization conclusions.
- HostedCluster metadata is the latest observed record at or before T, scoped by
  environment, MC and UID. Kusto retains the latest baseline at/before the search
  start plus subsequent observations, including deletes. This is not a live read:
  an old baseline or a missing update/delete can leave stale size or lifecycle
  state. Inspect `metadata_at`; missing or stale baseline/latest metadata remains
  a processor validation error, including after peak preflight adjustment.
- Assigned-size changes intersecting the window or its preceding settling
  interval block default publication. Peak mode first attempts forward adjustment
  as described above; ordinary `collect` does not adjust automatically. A baseline with a known size is required
  to establish stability. The default search is 24 hours with 15 minutes of
  settling; suggested windows must fit wholly within the search horizon and be
  stable across the observed HCP histories. The processor caps its search at
  24 hours even if a larger `--search-back` is collected. Unknown HCP metadata can
  prevent a suggestion. Suggestions are not applied automatically and are not
  proof of metric coverage in the earlier window: rerun collection, retaining
  your environment/cluster/endpoint options and choosing a fresh output path.

## Tests

```sh
# Targeted regional preflight contracts (offline, including cached resume).
venv/bin/python -B -m unittest observed.test_cli -k preflight -v

# Full Python suite.
venv/bin/python -B -m unittest test_costing test_pool_accounting observed.test_cli observed.test_peak observed.test_process -v
```

CLI tests use fake clients, mocked Azure CLI/HTTP calls and synthetic fixtures.
They require no network or credentials; temporary bundle directories use
`tempfile.TemporaryDirectory(dir="/tmp/opencode")`.
