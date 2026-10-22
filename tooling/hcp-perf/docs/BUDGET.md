# Candidate Budget Allocator

`hcp-perf budget allocate|check` is the native Go allocator, using `math/big`,
`sizing.Resolve`, `sizing.ToHyperShift`, and `cost.Calculate` without Python or
subprocesses. `scripts/budget.py` remains unchanged for historical reproduction.
Historical
`cost_allocate*.py`, their profiles, and their artifacts are not rewritten or
imported. New candidates change explicit inputs, not version-specific algorithms.
No Azure, Kubernetes, price fetch, deployment, or startup operation is performed.

## Native Go

For an explicit memory transfer without global rescaling, use
`preserveMemory: true` and set optional `rows[].fixedMemoryMiB` only on the changed
rows. All other memory requests remain `oldRequests.memoryMiB`. Keep `oldRequests`
as the actual pre-treatment baseline: overrides affect chosen allocations, not
old charges/deltas. Overrides require preserve mode, positive values at or above
protected floors, and the full Pod-stage charge must fit budget. CPU allocation
is independent and unchanged when its inputs are unchanged. Derived memory limits
and GOMEMLIMIT change with overridden requests; no other runtime setting changes.

From `tooling/hcp-perf`, with an existing private output parent:

```bash
go run . budget allocate --input examples/budget-small.json --output NEW_DIR
go run . budget check --input examples/budget-small.json --output NEW_DIR
go test ./internal/budget
```

The native output format is not byte-compatible with Python output directories.
It retains the complete input, exact JSON ledger, profile, resolved limits/env,
CSC, and cost input/result. `output-hashes.json` is written last after file fsync;
allocation never overwrites. `check` repeats the same command with the same flags
and compares the complete output set. No source receipt is a deployed-image claim.

For a CPU-only successor, add `--prior-plan FILE --name NAME
--kcm-cpu-milli 50 --catalog-cpu-milli 50`. This rebases old requests from the
prior candidate's desired policy, preserves every memory request, and raises only
the named CPU floors before reallocating unprotected weight. It saves both source
inputs and emits a **read-only** iteration `plan.json` containing cost and sizing
actions. It deliberately does not copy stale mutation guards, cluster creation,
or observation identities. Live lifecycle wiring requires a new reviewed plan.
The resulting self-contained `input.json` can also be allocated independently
into a different new directory. Its optional `preserveMemory: true` mode rejects
old memory below floors or above budget rather than changing memory.

The cost API uses whole millicores/MiB and micro-USD. Fractional charged resources
are rounded upward for that cross-check; exact fractions remain in the ledger.
Sub-micro-USD prices/budgets are rejected. Both results are illustrative resource
equivalents, not a schedulability or startup guarantee.

## Historical Python

From `tooling/hcp-perf`, Python 3.10+ and the checked-in input are sufficient:

First verify that the repository's `_artifacts` directory resides on private,
reboot-persistent storage, including any symlink targets or mounts. The commands
below create the artifact parents if absent and a **new** run root; choose a new run name
for each candidate. Directory existence alone does not verify storage durability.

```bash
set -e
umask 077
test -d ../../_artifacts || mkdir ../../_artifacts
test -d ../../_artifacts/budget-runs || mkdir ../../_artifacts/budget-runs
RUN_ROOT="$(realpath ../../_artifacts/budget-runs)/candidate-001"
mkdir "$RUN_ROOT"
python3 -B scripts/budget.py allocate --input examples/budget-small.json \
  --output "$RUN_ROOT/budget-small"
python3 -B scripts/budget.py check --input examples/budget-small.json \
  --output "$RUN_ROOT/budget-small"
python3 -B -m unittest discover -s scripts -p test_budget.py -v
```

The output parent must exist; the output directory must **not** exist for
`allocate`. No overwrite/refresh mode exists, even for identical bytes. `check`
recomputes and byte-compares the entire file set. A partial directory after an
interrupted write is not a successful output: the hash manifest is written last,
and `check` rejects incomplete contents. Choose another output directory to retry.

For a CSC, use a reviewed frozen build of the **existing CLI**. Build separately
(Go dependencies must already be cached for a fully offline build). Continue with
the persistent `RUN_ROOT` created above. The new binary directory prevents this
example from overwriting a previously frozen binary:

```bash
umask 077
test -d "$RUN_ROOT"
mkdir "$RUN_ROOT/bin"
GOWORK=off GOPROXY=off GOSUMDB=off go build -o "$RUN_ROOT/bin/hcp-perf-budget" .
python3 -B scripts/budget.py allocate --input examples/budget-small.json \
  --output "$RUN_ROOT/budget-small-wire" --hcp-perf "$RUN_ROOT/bin/hcp-perf-budget"
python3 -B scripts/budget.py check --input examples/budget-small.json \
  --output "$RUN_ROOT/budget-small-wire" --hcp-perf "$RUN_ROOT/bin/hcp-perf-budget"
HCP_PERF_BUDGET_BINARY="$RUN_ROOT/bin/hcp-perf-budget" \
  python3 -B -m unittest discover -s scripts -p test_budget.py -v
```

The optional binary is trusted executable code, not an input evidence file.
Only `sizing resolve` and `sizing hypershift` are invoked; no CRD is invented by
Python. Every selector is explicit, including unobserved init/Job selectors.
Every identity/request/limit/env and wire entry is checked. Duplicate
workload/container identities are rejected because the CSC cannot distinguish
kind/init collisions. The binary hash is retained and checked across rendering.
Use the same binary and rendering option for `check`. No live/CEL admission or
deployed-projector validation is implied by a successful render.

## Input Contract

`examples/budget-small.json` is the portable, reviewed extraction of the saved
scoped quiet inventory and latest historical candidate. It contains no cluster
IDs, namespaces, private paths, image identities, or runtime evidence dependencies.
Source-relative paths and SHA256 receipts preserve provenance without requiring
the original private files. Decimal/fraction quantities must be JSON strings;
integer quantities are allowed. Floats, zero weights, nonpositive floors, missing
resource fields, unknown Pod resources, duplicate identities, contradictory
replicas, and infeasible protected budgets fail closed.

| Field | Meaning |
| --- | --- |
| `version`, `name` | `hcp-perf.budget/v1` input schema; candidate name |
| `budgetUSDPerHour` | Positive exact USD budget, `0.125` in the example |
| `node` | SKU, region, USD price, retrieval timestamp, source URL/hash, raw `cpuMilli`/`memoryMiB`, capacity source |
| `quota` | Positive `periodUS`, `minimumUS`, `quantumUS`, and provenance |
| `factorGridDenominator` | Explicit rational factor grid, 1,000,000,000 in the example |
| `inventory` | Scoped source audit hash, source-file hashes, windows and limitations |
| `rows` | Every controlled identity, positive CPU/RAM weight, positive floor, old requests, steady replicas, runtime classification and evidence |
| `pods` | Workload, replica multiplier, regular row indexes, ordered init row indexes with explicit restartability, explicit CPU/RAM overhead |
| `downstream` | Explicit fixed request totals, enumerated unchanged downstream rows, separately reported saved-layout totals and limitations |
| `excludedInstrumentation` | Explicit excluded workload/resources and reason |
| `fallback` | Required positive CSC defaults for unlisted projector-owned containers; not included as fictional demand |
| `goMemLimitPercent` | Runtime policy, 80 in the example; `nonGo: true` disables injection per row |

`replicas` means running regular/restartable-init instances, not transient init
instances. The allocator verifies it against the layouts. Transient init rows can
have zero steady replicas yet contribute to Pod scheduling maxima. Rows absent
from all layouts remain in the profile and missing-coverage ledger, but are not
silently charged invented replicas. New workloads/replicas require new explicit
inputs. Unknown containers are not automatically labeled downstream. The CSC's
fallback cannot prevent future unseen workload creation; this ledger makes no
whole-namespace cap claim.

**Source-verified** means the historical scope audit inspected exact component
registrations and the resource-policy projection path. Each row's source path/hash
must match the embedded audit receipt. A policy-hash annotation alone is rejected.
The allocator validates these receipts and numeric consistency; it does not
re-download sources, authenticate a reviewer's assertions, or prove that deployed
images implement that source. The embedded audit hash identifies the original
reviewed evidence, not a hash of its sanitized extraction.

Quiet weights are the source candidate's worst replica/window CPU p95 and peak
cgroup-charge-derived requests, including original floor effects, **not raw
simultaneous usage ratios**. Unobserved rows retain explicitly labeled historical
baseline weights with null quiet evidence; no usage or startup minima are invented.
`quietPeakMemoryMiB` is charge, not RSS. Risk comparisons do not replace these
statistics with one another.

## Exact Model

For each resource independently, the allocator first charges **all explicit
floors** and fixed downstream demand. If this exceeds the resource entitlement,
allocation fails. It then selects the largest feasible factor on the declared
rational grid:

```text
entitlement = rawNodeCapacity * budget / nodePrice
request_i = max(explicitFloor_i, floor(weight_i * factor))
CPU limit_i = CPU request_i
memory limit_i = ceil(memory request_i * 11/10) MiB
GOMEMLIMIT_i = floor(memory request_i * 1048576 * percent/100) bytes
cost = nodePrice * max(offeringEffectiveCPU/rawCPU, offeringChargedRAM/rawRAM)
```

These are **floors**, not fixed allocations: a sufficiently large scaled weight
can exceed a protected value. There is no additive floor-plus-weight bonus and
no resource-specific hidden minimum. Explicit 1m/1Mi floors mean representability,
not viability. Floors participate inside every feasibility check; nothing is
clamped upward after allocation. CPU and RAM share no interchangeable dollar
credit. Unused quantization slack remains unused; the next grid step's charge is
reported to demonstrate the binding boundary. Ratios can change, and neither
factor is a throughput multiplier or guarantee.

`stages()` is the single pure scheduler-stage definition. Sequential init stages
include previously started restartable init containers; the final stage includes
regular containers plus all restartable init containers. Scheduler requests take
the largest stage sum plus explicit Pod overhead. Memory charges round each
container limit **before** taking the largest stage sum, then add overhead.
Pod-level resources are unsupported and rejected rather than guessed.

```text
quotaUS(c) = max(minimumUS, ceil(c * periodUS / (1000 * quantumUS)) * quantumUS)
quotaMilli(c) = quotaUS(c) * 1000 / periodUS
parent = quotaMilli(max declared stage CPU + overhead CPU)
effectivePodCPU = min(parent, max(sum(child quotaMilli) + overhead CPU))
```

Overhead is one explicit reserved allowance outside child quotas, still bounded
by the parent; it is not an additional fabricated container quota. Each Pod's
maximum is multiplied by its replica count. Three 3m containers in one Pod cost
10m under the example model, not the 30m sum of their child quotas. Three separate
Pods cost 30m. The example's overhead is explicitly zero.

The 100000/1000/1000us parameters matched 24 captured startup containers and 17
parents. Their origin in the deployed runtime was not verified; extending them
to the saved 52-Pod benchmark layout is an assumption, not universal Kubernetes
semantics. The fixed 470m/2090Mi downstream charge matches the retained benchmark
request accounting, but downstream is not finitely capped at those numbers.
Different `actualLayoutRequests` are reported, not substituted silently. Missing
downstream init requests in the historical scheduler audit were treated as zero
only for request arithmetic, never as evidence of zero consumption.

## Combined Seed

Reviewed input SHA256:
`0da0ce2a8bab96e664c12f76c352d0fd43b59ca30ba0f1b6834da37ea6243c00`.

The extraction was checked against the immutable scoped-inventory, saved-Pod,
source-weight and latest profile hashes. Latest actual row values take precedence
over stale inherited `protectedCPU`/`protectedMemory` maps.

Memory floors retain HAProxy96, CVO64, CPO65, storage64, ingress96, **CNO80**,
OLM/catalog-operator64, all 17 availability-prober init selectors64, and the other
explicit consolidated operator/serving allowances. KAS560 and etcd203 preserve the
latest targeted values. CPU floors retain readiness helpers50, KCM/OCM/route20,
OLM/catalog-operator20, token-minters10, availability-probers2, catalog registries
22/24/39, etcd healthz20, KAS99, CPO60 and etcd26. The JSON is the exhaustive list.

The reviewed candidate's combined bootstrap floor is **50m/64Mi**, selected by the
agent from evidence rather than explicitly prescribed by the user, and deliberately
above the latest historical 30m/40Mi. The previous 50m/64Mi treatment addressed
apply timeouts; its later reduction funded KAS memory after an OOM. Neither value
is proven sufficient.
No readiness/liveness deadlines, images, APF settings or deployment state change.

| Resource | Old | Combined | Exact Entitlement | Slack |
| --- | ---: | ---: | --- | --- |
| Effective offering CPU m | 2480 | 2480 | 1000000/403 | 560/403 |
| Charged offering RAM MiB | 10148 | 10101 | 4096000/403 | 25297/403 |
| USD/HCP-hour | 0.12493 | 0.12493 | 0.125 | 0.00007 |

The retained D16s_v6 Linux consumption price is $0.806/node-hour, West US 3,
retrieved 2026-09-11T14:06:18.998275Z, using raw 16000m/65536Mi capacity. This is
not an AKS allocatable model or packed-node bill; system/HA/fragmentation, disks,
network/services, guest workers, downstream excess, surge, and absent workloads
remain outside the economic claim. KSM10m/64Mi (256Mi limit) is excluded explicitly.

## Outputs And Risk

`input.json` freezes the exact input bytes. `candidate.json` is an existing-schema
sizing profile. `ledger.json` records exact factors, protected charges, remaining
budget, requests, limits, Go targets, source coverage, old/new row and Pod deltas,
donors, missing layout rows, and per-component startup notes. `ledger.md` presents
all rows and economic totals. `output-hashes.json` hashes every other output.
Optional `resolve.json` and `hypershift.json` come only from the existing CLI.
Input byte/canonical hashes, allocator hash, and optional CLI hash are retained.
Changed inputs/code/binary require a new output directory, not historical rewrites.

Steady donor contributions are replica-weighted **request** reductions; they are
not additive savings under Pod init maxima or shared quota caps. Use the exact
replicated Pod charge deltas to reconcile the cost change.

| Component | Combined Requests | Main Remaining Risk |
| --- | --- | --- |
| KAS main | 106m/560Mi, three replicas | 616Mi limit remains below historical quiet charge; CRD startup/load not qualified |
| KAS bootstrap | 50m/64Mi, three replicas | Deadline allowance restored; no fresh-start evidence for combination |
| CPO | 100m/65Mi | CPU can exceed its60m floor;72Mi memory limit remains a risk |
| Etcd main / healthz | 26m/203Mi;20m/20Mi | Main224Mi and helper22Mi limits; helper remains CPU-protected, memory reduced |
| HAProxy | 1m/96Mi, two replicas | Non-Go,106Mi cap retained;1m declared request is not10m scheduler entitlement |
| CNO / ingress | 47m/80Mi;34m/96Mi | Memory protected, CPU donors may expose deadlines |
| Packageserver main / readiness | 42m/176Mi;50m/20Mi | Main CPU donor; helper memory reduced despite protected CPU |
| Catalog registries | 22m/136Mi;24m/125Mi;39m/199Mi | CPU floors retained; RAM donors below historical charge (which includes cache) |

Five selectors have no saved Pod layout: both autoscaler selectors, both
featuregate-generator selectors, and olm-collect-profiles. Every other binding
is checked, including transient init and restartable stages, but none establishes
complete fresh startup. No throughput, density, startup, or actual-bill qualification
is claimed. A separately authorized startup campaign must test the immutable
candidate and preserve first-failure evidence.

## Next Workflow

Follow the [startup monitor workflow](STARTUP.md) for separately authorized
observation. The parent lifecycle's current immutable candidate remains at
repository-root `_artifacts/budget-small-reviewed`; the new-run examples above
do not replace or regenerate it.

Missing startup profile coverage remains a blocker under correction, not a reason
to infer readiness from a successful budget check. Before observation, reconcile
the startup owner/container profiles with every candidate selector, explicitly
resolve the five missing-layout selectors above, and record any additional startup
workloads, replicas, init stages or surge outside this benchmark. Freeze the exact
reviewed profiles, resolved policy and ledger hashes through the startup workflow;
do not treat an absent profile or observation as zero demand or successful coverage.
