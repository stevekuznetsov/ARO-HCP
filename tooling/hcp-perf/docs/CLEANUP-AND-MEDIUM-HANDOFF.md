# Evidence Cleanup And Medium Handoff

Prepared September 18, 2026. This handoff covers **local evidence cleanup first,
then a new Medium experiment**. The inventory and dashboard reproduction were
performed; **no evidence was deleted and no Medium run was launched**.

## Goal And Workspace

The user wants to keep data used to build the report, retire unnecessary older
runs before trimming current datasets, and then characterize Medium at **150% of
the $0.25/HCP-hour hosting fee: $0.375/HCP-hour**. This doubles the current Small
budget of $0.1875, not necessarily every container request or throughput.

Work in the preserved worktree, not the user's adjacent active checkout:

```text
/home/stevekuznetsov/code/Azure/ARO-HCP/src/github.com/Azure/ARO-HCP-perf-runner
```

Load its `AGENTS.md` and `CLAUDE.md`. Much of `tooling/hcp-perf` remains untracked;
a Git revision or a fresh clone does not preserve this implementation. Use native
Go tools and the durable iteration controller, not old Python launchers. Keep
private files under persistent home/worktree storage, with `umask 077`, not `/tmp`.
Do not commit credentials, raw logs, or unsanitized evidence.

## Current State

At **02:17 UTC September 18**, native controller inspection found only one active
iteration: `envelope-observe5-hour-004`. Its performance action is **FAILED**, its
retention action is **RUNNING**, and its retention worker identity was verified.
The overall run is RUNNING because the keeper is still active, not because load
is running. All observation writers were finalized earlier. Recheck this state;
the saved keeper bound was approximately 02:39 UTC.

The following enabled services were active at the audit checkpoint:

- `hcp-perf-go-small-controller-20260914.service`
- `hcp-perf-go-small-auth-20260914.service`
- `hcp-perf-go-small-rp-forward-20260914.service`

The controller executes
`_artifacts/small75-envelope-20260917/hcp-perf-observe5-02` and serves
`_artifacts/go-small-controller-20260914/state`. Authentication rotation and RP
forwarding also reference files inside that latter tree. **Do not delete those
directories or binaries because their names look historical.** Stopping the
controller does not stop its detached workers (`KillMode=process`).

Use the audit's verified Go binary to inspect without changing the live run:

```bash
ROOT=/home/stevekuznetsov/code/Azure/ARO-HCP/src/github.com/Azure/ARO-HCP-perf-runner
AUDIT="$ROOT/_artifacts/retention-audit-20260918"
STATE="$ROOT/_artifacts/go-small-controller-20260914/state"
"$AUDIT/hcp-perf" iteration status --root "$STATE"
systemctl --user show hcp-perf-go-small-controller-20260914.service \
  hcp-perf-go-small-auth-20260914.service \
  hcp-perf-go-small-rp-forward-20260914.service \
  -p Id -p ActiveState -p ExecStart -p RuntimeMaxUSec
```

Before cloud work, verify Red Hat authentication, actual HCP/node identities,
credential expiry and resource-group ownership. Use
`AZURE_CONFIG_DIR="$HOME/.azure-redhat"`. Refresh and verify `createdAt` for the
exact approved existing resource groups; preserve other tags. Keep the required
underlay/customer groups separate from the explicitly named future managed RG.
Do not infer fresh retention from a RUNNING service or reuse an expired token.

## Disk Inventory

The filesystem was **95% full**, with **15.65 GiB (16.81 GB) available** at the
audit's exact filesystem measurement. The
artifact tree occupies **122.68 GiB allocated** (130.71 GB logical file lengths).
Filesystem-wide usage is not the size of the experiment directory.

| Set | Allocated Size | Meaning |
| --- | ---: | --- |
| Entire `_artifacts`, before the audit | 122.68 GiB | Includes raw evidence, copies, source trees, binaries and operational state |
| Exact current dashboard input closure | **1.002 GiB** | 233 files, all hashes checked |
| Entire 13 selected dashboard run directories | **2.115 GiB** | A simple conservative first keep boundary |
| Other run directories inside the shared journal root | 7.250 GiB | Not a deletion list; inspect journal ownership and references |
| Old scenario roots absent from direct dashboard inputs | **63.782 GiB gross** | First release-review priority, after extracting exceptions |

Largest top-level roots, rounded by `du -h`:

| `_artifacts/` Root | Size | First Treatment |
| --- | ---: | --- |
| `perf-resource-locked-scan` | 30 GiB | Retire old raw campaign after preserving required provenance exceptions |
| `perf-three-node-transition` | 22 GiB | Same; original empty/validation evidence is not in the current dashboard |
| `personal-reset-20260914` | 14 GiB | Review backups separately; keep operational rebuild inputs and receipts |
| `go-small-controller-20260914` | 14 GiB | Keep journal and live dependencies; trim only reviewed terminal runs later |
| `cpo-memory-investigation-20260916` | 9.3 GiB | Keep current Small budget, required inventory and selected diagnostic evidence |
| `perf75-load-20260916` | 8.7 GiB | Retire superseded bulk capture after preserving report/incident sources |
| `medium-v1-20260910` | 6.7 GiB | Historical Medium, not the proposed $0.375 Medium |
| `perf75-slow-seed-20260916` | 6.3 GiB | Retire bulk capture only after preserving CPO OOM and seeding diagnosis evidence |

These numbers overlap neither each other nor the parent total when used as this
top-level table. They are **not guaranteed reclaimable bytes**: `/home` is Btrfs,
and copies may share extents. A same-filesystem quarantine rename frees no space.

### Audit Artifacts

Private authoritative files are in `_artifacts/retention-audit-20260918/`:

- `inventory-v2.json`: exact input paths, realpaths, sizes, SHA256 values, run IDs
  and attempt IDs. `Files` is the explicit dashboard keep list.
- `summary-v2.json`: grouped sizes, the 21 old-scenario roots, and
  `MetadataEvidenceAndAuditFiles`, a supplementary 35-file budget/provenance list.
- `HANDOFF.md`: detailed measurement method, selected-run table, source-chunk
  proofs and release gates. Read it before deletion.
- `frozen-source.tar` / `frozen-source.json`: current exporter source, dependencies,
  assets, tests and file hashes; about 14 MiB, not another full evidence copy.
- `hcp-perf`, `reexport.json`, `reexport.html`: verified executable and reproduction.

The 233 files total **1,075,896,300 logical bytes**. All matched the private
manifest and the portable citations. Re-export matched the maintained Small JSON
as decoded JSON, and HTML rendering passed. The portable dataset itself is only
**883,182 bytes**, and the generated dashboard **994,762 bytes** at this checkpoint.

## Retention Policy

Keep four explicit sets, not every old artifact by default:

1. **Current dashboard rebuild:** the 233-file closure, exact private export
   manifest, portable dataset/HTML, and frozen exporter source/binary.
2. **Claims and next-size inputs:** exact current Small budget, pricing receipt,
   resource/image/catalog/runtime configuration, and compact evidence supporting
   the health, contention and startup caveats shown in the report.
3. **Operational continuity:** SQLite journal, claims/locks, live worker binaries,
   service definitions and their input dependencies, private credentials, current
   retention inventory, customer infrastructure and pending-operation receipts.
4. **Source work:** maintained Go implementation, isolated HyperShift changes, and
   OLM/prober PR checkouts and build provenance until their own source retention is
   settled. These are not disposable metric datasets.

Under the requested report-focused policy, older raw experiments outside these
sets may be retired. Preserve their small portable summaries, and record that
their raw evidence was retired. Do not continue claiming those historical reports
are fully reproducible. A citation hash is not a backup of the cited bytes.

The dashboard exporter does not read archives, TSDBs, SQLite or historical worker
binaries. However, its metadata includes reviewed claims; the direct export list
alone is **not** a complete proof of every historical narrative. Retain selected
incident evidence or explicitly narrow the associated claims before releasing it.

## Cleanup Sequence

### 1. Freeze The Keep Set

Recheck the private audit against the current manifest. Its exact hash was:

```text
d9c50d36b129d9d971ca62a6fdd7b149528cf5c9b0bcbef661735c8f1bb4a19b
```

Produce a reviewed machine-readable keep manifest combining the four sets above.
Resolve realpaths and symlinks; reject paths outside the chosen worktree. Include
in-root dependencies referenced by JSON, kubeconfigs, systemd units and scripts.
For customer Bicep, retain its relative imports into `test/e2e-setup/bicep/modules`.
Do not blindly follow arbitrary raw-artifact fields as instructions.

Take a consistent SQLite backup using SQLite's backup API after reconciling worker
state. **Do not copy only `iteration.sqlite` while its WAL is active.** Keep active
attempt directories and locks in place. Do not delete journal rows or reset claims
to facilitate cleanup. Small's retained guest fixtures are not local disk clutter.

Before deleting originals, create a private release bundle preserving the selected
files' root-relative layout, manifest, portable output and frozen exporter. Verify
all hashes and re-export from that relocated bundle without access to the original
raw directories. The audit proved reproduction **in place**, not this isolated
relocation test. Retain a consistent operational backup separately; a same-disk
bundle survives `/tmp` loss but is not an off-machine backup.

### 2. Release Whole Old Scenarios First

Review `OldScenarioRootsNotInExporter` in `summary-v2.json` as the first batch:
the old locked scan, three-node transition, old Medium, single-node attempts and
superseded cost-v1-v13 investigations. Together these are about 63.8 GiB gross.

Before releasing a root, extract any required pricing, customer-infrastructure,
source, image or incident evidence into a durable keep bundle. Preserve original
relative paths where a retained consumer requires them. Otherwise create a new,
versioned consumer manifest and prove reproduction against the relocated bytes;
do not overwrite frozen experiment inputs.

For each candidate directory, require zero intersection with the expanded keep
set and no active service/worker dependency. Emit a **dry-run deletion manifest**
with exact paths, sizes, exclusion reasons and preconditions. Never use a broad
`rm -rf _artifacts/*` or select deletions solely by date/prefix.

Release the reviewed batch, log what was removed, then re-export and render the
dashboard into new filenames. Verify all input hashes and decoded-JSON equality
with `docs/dashboard/small.json`; compare data, not pretty-print whitespace.
Measure actual `df` free-space change rather than treating `du` as reclaimed space.

### 3. Trim Current Evidence Only If Needed

Once the old scenarios have been released, reassess space. Prefer keeping the
entire 13 selected run directories initially: about 2.115 GiB is already modest.

If further reduction is needed, retain the explicit file closure rather than
whole metrics/record directories. Three oversized Pod journals have verified,
newline-aligned chunks in `dashboard-small`; concatenation hashes match their
original **445,995,628 bytes**. Those three originals are redundant for dashboard
export. The other ten unsplit Pod journals are still required. Never delete all
`record/pods.ndjson` files based on that optimization.

Do not truncate NDJSON, replace raw observations with percentile summaries, or
delete source-time companions. Quartiles, failure counts, incarnation binding
and resource coverage depend on those records. Keep missing evidence missing;
do not zero-fill after pruning. Archives or historical TSDB copies that are not
needed by the selected report/incident set can be released separately.

Suggested cleanup exit gate: **at least 40 GiB free**, exact dashboard reproduction
still passing, operational/source dependencies intact, and a saved deletion log.
This is a disk-headroom guardrail for Medium, not an allocation or performance SLO.

Reproduction uses existing commands, with new output paths:

```bash
"$AUDIT/hcp-perf" dashboard export \
  --input "$ROOT/_artifacts/dashboard-small/export.json" \
  --output "$AUDIT/after-cleanup.json"
"$AUDIT/hcp-perf" dashboard render \
  --input "$AUDIT/after-cleanup.json" \
  --output "$AUDIT/after-cleanup.html"
```

If either output already exists, choose a new name. Do not rerun the old
`dashboard-small/build-manifest.go` preparation helper as a validation step; it
writes the manifest/chunks and expects the oversized originals.

## Medium Allocation

Use the latest Small input and **resolved allocations** from:

```text
_artifacts/cpo-memory-investigation-20260916/budget-audit-funded/native/
```

Small charges **3720m / 15241Mi**, costing **$0.1874427185/hour**. Its input is
in memory-preservation mode, with CPO `fixedMemoryMiB:144` and KAS audit
`fixedMemoryMiB:39`. Its `oldRequests` still describe the pre-transfer baseline.
**Changing just the budget to 0.375 will leave memory fixed.**

Prepare a new self-contained input, without editing Small:

1. Join rows to the current Small ledger by exact kind/workload/container/init
   identity; update `oldRequests` to the actual resolved Small requests.
2. Use those current CPU/memory allocations as the new controlled weights. Label
   them **Small-allocation-derived proportions**, not newly measured usage.
3. Set `budgetUSDPerHour` to `"0.375"`, `preserveMemory` to `false`, and remove
   every `fixedMemoryMiB`. Keep existing protected floors as minima.
4. Keep downstream accounting at **470m/2090Mi**, included once, and preserve
   replica counts, init ordering, overhead, quota model and instrumentation scope.
5. Preserve CPU=request limits, 110% rounded memory limits, 80% Go targets and
   explicit runtime overrides. KAS, KCM and catalog mains use GOMAXPROCS1; CPO had
   no explicit override and was observed at two. Do not introduce a runtime change.

At the retained **D16s_v6 $0.806/hour** raw price, Medium gets an entitlement of
**7.444168734 vCPU / 29.776674938 GiB**. Reuse that price snapshot for comparability,
or treat a price refresh as an explicit economic change. Fixed downstream demand
is subtracted only once, so a budget-filling controlled allocation can grow more
than 2x. Rounding and Pod-stage accounting require the actual native ledger.

Do not reuse historical `double-small-v1.json`: it uses another baseline, 3x
memory limits and no CPU limits. Do not use `budget --prior-plan` here: it enables
memory preservation for CPU-only successors.

```bash
"$HCP_PERF" budget allocate --input "$MEDIUM_INPUT" --output "$NEW_BUDGET_DIR"
"$HCP_PERF" budget check --input "$MEDIUM_INPUT" --output "$NEW_BUDGET_DIR"
```

`HCP_PERF` must be a **new reviewed immutable Go build**, with its source snapshot
and hash retained. Run module race tests and vet before freezing it. The current
source fixes the old 19-second safety-loop problem with independent critical
checks: 5s cadence, 4s cycle bound, 15s watchdog. Reusing an older frozen executable
would silently reintroduce the flaw.

## Medium Test Sequence

Keep the initial comparison resource-only: release **4.21.32**, the reviewed
packageserver concurrency/jitter image, prober/CPO code, three HA replicas, catalog
digest pins, APF and request mix. Resolve exact digests from the retained Small
spec and verify registry availability. Do not rely on mutable `v4.21` tags.

1. Verify the current personal environment, credentials, retained Small cluster
   and Go journal. Finish or stop existing actions through native verified recovery.
   Retain Small report data before any separately recorded HCP cleanup. Do not
   tear down the personal underlay just to start Medium.
2. Refresh the exact retention allowlist; prepare sufficient management placement
   and rollout headroom. Record any host-count/SKU change. Prices reference Dsv6;
   prior execution used D4-class hosts, not calibrated v6 performance.
3. Submit a fresh Medium candidate through the Go controller. Arm native startup
   recording before creation; discover the HC by exact ARM annotation. Diagnose
   the management cluster throughout startup, independently of RP completion.
4. Require sustained availability and then provision **three guest workers**.
   Keep source identity, actual quota/limits, credential and observation lifetimes
   verified. Do not enlarge resources during a scored window.
5. Establish quiet validation, then replay **60 applications / 300 Pods at 5 QPS**.
   Seed at **2 QPS / burst 2**, retaining the 30s request timeout. Require all
   Deployments/Services/ConfigMaps/Secrets to converge before measurement.
6. Increase QPS in **five-minute screens**, retaining the same population and
   client identities. Bracket the first request-quality failure, refine below it,
   then select a promising point for a **3600s window plus 600s recovery**.
7. Measure the matched **zero-application-Pod** baseline at selected rates by
   guarded scaling of the same Deployments to zero. Retain API fixtures, namespaces
   and APF; explicitly disable application turnover. Restore through the guarded
   native path if needed. Do not call this an empty Kubernetes cluster.
8. Test greater population and host contention as separate treatments. The earlier
   Small contention failure remains visible; Medium inherits no contention claim.

Request quality accepts **at most 5%** of dispatched logical requests receiving
HTTP 429 **or** timing out, counted once per request. Report separate counts,
successful-body QPS and success-only latency. Generator limitation, authorization
errors and incomplete delivery must not masquerade as server capacity.

For the diagnostic hour use `stabilityPolicy: "observe-degradation"` with
`requestQualityPolicy.maxDegradedBasisPoints: 500`. Keep the five explicit critical
source roles and their jobs: KAS, etcd, container/cAdvisor, node and kubelet. Record
ancillary restarts, canary failures and missing telemetry; do not turn completed
execution into a clean-health claim. Sustained critical availability loss, unsafe
nodes, resource/identity drift and unresolved canary cleanup still stop the run.
Early screening and diagnostic-hour mode are currently mutually exclusive: use
separate short cases to screen rather than adding early checkpoints to the hour.

**Known extension gate:** native performance currently requires exactly three
guest nodes. Six workers / 1200 Pods is a later hypothesis, not a runnable input
change alone. Extend and test the node-count/capacity contract before that phase.
Existing-population/unloaded reuse also rejects competitors; preserve explicit
quiet versus contended fixtures and pressure plans.

The controller command shape is:

```bash
"$HCP_PERF" iteration submit --root "$STATE_ROOT" \
  --input "$REVIEWED_PLAN" --id "$NEW_RUN_ID"
"$HCP_PERF" iteration status --root "$STATE_ROOT" --id "$NEW_RUN_ID"
"$HCP_PERF" iteration watch --root "$STATE_ROOT" --id "$NEW_RUN_ID" --interval 1s
```

`submit` queues execution immediately if `serve` is running; it is **not a dry
run**. Use the existing reviewed persistent controller service, updating its
executable deliberately while preserving active workers. Never run a second
uncoordinated lifecycle script, replay accepted PUT/DELETE operations, rewrite
frozen inputs, or manually release SQLite claims.

## Finish And Handoff

Publish Medium as its own dataset/pane, with the exact budget, input/image hashes,
host topology, population, client mix, duration, outcomes and coverage. Reuse the
dashboard exporter and renderer; do not extrapolate Small's distributions.

For each retained Medium case, declare its dashboard source allowlist at export
time so the next cleanup does not require another archaeological audit. Keep
raw per-request and per-canary samples, resource counters with source times,
incarnation/owner inventory, outcomes, timing boundaries and the minimum provenance.
Keep observation and workload state separate, verify no generators remain, and
record the next retention deadline and credential expiry.

Deliver: deletion manifest and actual reclaimed bytes; successful Small re-export;
new Medium input/ledger and frozen executable; native run IDs/current status;
measured loaded/unloaded envelope and hour results; explicit blockers and gaps.

References: [Dashboard](DASHBOARD.md), [Budget](BUDGET.md),
[Iterations](ITERATION.md), [Performance](PERFORMANCE.md),
[Storage and recovery](storage-and-recovery.md),
[latest Small hour](cards/small75-observe5-hour.md).
