# Offline Performance Dashboard

[Open the combined overview](dashboard/index.html) for the three always-visible
size summaries, selected comparison and complete 37-run register. Detail pages:
[Small](dashboard/dashboard.html), [Size100](dashboard/size100.html),
and [Medium](dashboard/medium.html).
Each generated page is self-contained and can be opened directly from disk.
Medium is a separate measured dataset/pane; Small's JSON, generated page and cited
cards remain byte-for-byte unchanged.

For the measured disk inventory, report-preserving cleanup order, and the next
$0.375/hour campaign, see [Cleanup and Medium handoff](CLEANUP-AND-MEDIUM-HANDOFF.md).

The new dashboard presents reviewed performance envelopes across sizes, cohorts,
individual runs and components. It is separate from the historical
[Small v1 card](cards/small-v1.html), which remains unchanged and is not included
in the new Small dataset.

**Execution complete and request quality passed do not mean health passed.** The
Small reference run `envelope-observe5-hour-004` completed its full hour, met its
5% request-degradation allowance, and **failed health assessment**, with partial
capture. It is not a clean-hour, packed-density or host-contention qualification.

## Build And Render

### Combined Overview

The public [overview manifest](dashboard/overview.json) selects presentation,
not measurements: Small75, Size100, and all four Medium panes. Group and pane
arrays set register order; `highlights` selects and orders notable cases by
`sizeID` / `runID`, with the first case as the summary reference. Every portable
pane and run must remain in the register. Numeric measurements, budgets, dates,
assessment axes and decision text come from the three existing portable JSON
files, not hardcoded renderer facts or recomputed checkpoint percentages.

From the preserved repository root, using an existing private artifact directory:

```bash
GOWORK=off GOPROXY=off GOSUMDB=off \
  go -C tooling/hcp-perf build -o ../../_artifacts/dashboard-overview/hcp-perf .
_artifacts/dashboard-overview/hcp-perf dashboard overview \
  --input tooling/hcp-perf/docs/dashboard/overview.json \
  --output _artifacts/dashboard-overview/overview-new.html
```

The output must be new. The manifest and each portable file are strictly decoded
bounded regular JSON files (32 MiB each); at most eight distinct input groups
bound aggregate input. Portable semantic validation is shared with `render`,
including source references and distributions. Relative input paths resolve
inside the manifest directory through a confined filesystem root; escaping
symlinks, traversal, absolute paths and URLs are rejected. Groups must share
their displayed fee, target, modeled cost and charged allocation across panes.
The schema is `hcp-perf.dashboard-overview/v1`.

`input` and `detailPage` are public relative paths. Generated links are relative
to the **published HTML**, so publish beside the portable files and detail pages
with the same layout. A private candidate renders completely offline but its
relative detail links resolve only after publication into that layout. The
command never opens detail pages or source citations. Source IDs/labels/hashes
remain inert text. Reviewed text is escaped, not treated as HTML or JavaScript.

Review the candidate before publishing it as the new `docs/dashboard/index.html`;
never overwrite an unrelated file or regenerate any detail page. The overview
has its own embedded assets; existing `page.html`, `style.css`, `app.js` and all
Small/Size100/Medium JSON, HTML and cards remain unchanged. There is no fetch,
font, CDN or server dependency. All content and native keyboard disclosures work
without JavaScript; the optional script only opens and focuses local run anchors.
Preserved detail pages support **pane** anchors, not individual run selection;
the overview labels those links honestly and displays the run ID to select there.

Run targeted offline validation from the repository root:

```bash
# GOTMPDIR is for the Go build; the short relative TMPDIR resolves from each
# dashboard test package, avoiding Chrome's Unix socket path-length limit.
GOWORK=off GOPROXY=off GOSUMDB=off \
  GOTMPDIR="$PWD/_artifacts/do-tmp" TMPDIR=../../../../_artifacts/do-tmp \
  go -C tooling/hcp-perf test -race -count=1 ./cmd/dashboard ./internal/dashboard
GOWORK=off GOPROXY=off GOSUMDB=off \
  go -C tooling/hcp-perf vet ./cmd/dashboard ./internal/dashboard
TMPDIR=_artifacts/do-tmp node \
  tooling/hcp-perf/internal/dashboard/testdata/review.mjs \
  /usr/bin/google-chrome tooling/hcp-perf/docs/dashboard/index.html \
  _artifacts/do-tmp/overview-review-new _artifacts/dashboard-overview overview
```

The explicit `overview` review mode checks 1280x1000 and 390x844, all summaries
and run counts, deep-link focus, native Enter disclosures, all-expanded overflow,
no-JavaScript usefulness and zero network attempts with network emulation offline.
Screenshots and local build/test/preservation receipts live in the private
`_artifacts/dashboard-overview` directory. No live calls or cleanup are involved.

### Individual Detail Pages

Run these commands from the `ARO-HCP-perf-runner` repository root. No cloud
credentials, cluster access, image pulls, npm installation or web server are
needed. Use persistent paths, not `/tmp`.

```bash
mkdir -p _artifacts/dashboard-build
(cd tooling/hcp-perf && GOWORK=off go build -o ../../_artifacts/dashboard-build/hcp-perf .)
_artifacts/dashboard-build/hcp-perf dashboard render \
  --input tooling/hcp-perf/docs/dashboard/small.json \
  --output _artifacts/dashboard-build/dashboard.html
```

Open `_artifacts/dashboard-build/dashboard.html` directly in a browser. The
portable JSON alone is sufficient; private evidence is not needed for rendering.
The HTML embeds its data, CSS and native JavaScript. Charts, controls and source
citations do not fetch anything, and the summary/run register remains available
without JavaScript. The content security policy disallows network connections,
external images, fonts, base URLs and form submissions.

All dashboard commands require a **new output filename in an existing parent directory**:

```text
hcp-perf dashboard export --input privateManifest.json --output NEW_JSON
hcp-perf dashboard render --input portableJSON --output NEW_HTML
hcp-perf dashboard overview --input overviewManifest --output NEW_HTML
```

Outputs use exclusive creation (`O_EXCL`) and mode `0600`, and are synced and
closed before success is returned. Existing files, directories and dangling
symlinks are refused. There is no force/overwrite flag. Choose a new filename
for another build. Invalid input is rejected before output creation; export and
render complete in memory before creating the destination. A storage write,
sync or close failure can still leave an incomplete new file and returns an
error, not success.

The render input must be one regular JSON file of at most 32 MiB, with no unknown
fields or trailing JSON values. The command checks schema, unique IDs,
references, distribution coverage, finite/nonnegative monotonic percentiles,
whiskers and optional resource reference values. This is structural validation,
not evidence authentication or a new health assessment. Export also bounds its
manifest to 32 MiB and each declared source to 128 MiB.

For a reviewed documentation-page build, after dataset corrections are complete,
the maintainer can use the already-existing documentation parent:

```bash
_artifacts/dashboard-build/hcp-perf dashboard render \
  --input tooling/hcp-perf/docs/dashboard/small.json \
  --output tooling/hcp-perf/docs/dashboard/dashboard.html
```

That command must not target an existing page. Publishing the generated file is
a separate review step; this tooling does not deploy a site or change permissions
for a hosting service.

## Reproduce Small

The maintained portable input is [dashboard/small.json](dashboard/small.json).
The reviewed export currently contains **13 runs**: eight screens and five
confirmations, including repeated QPS values as separate cases. It is about
877 kB compact (about 856 KiB); pretty-printed exports are larger. Source counts
and exact byte sizes can change when evidence corrections are reviewed; do not
use them as a schema contract.

The local, **private** reproduction manifest is
`_artifacts/dashboard-small/export.json` at the repository root. It is an
allowlist of private evidence, not a public portable manifest, and those private
files are intentionally withheld. A checkout containing only the public dataset
can render it but cannot reproduce its measurements from hashes alone.

Where the reviewed manifest and all its evidence are available, reproduce into a
new artifact, never overwrite `docs/dashboard/small.json` in place:

```bash
_artifacts/dashboard-build/hcp-perf dashboard export \
  --input _artifacts/dashboard-small/export.json \
  --output _artifacts/dashboard-build/small-reexport.json
_artifacts/dashboard-build/hcp-perf dashboard render \
  --input _artifacts/dashboard-build/small-reexport.json \
  --output _artifacts/dashboard-build/small-reexport.html
```

`ReadExportSpec` resolves a relative `baseDirectory` against the manifest's
directory and makes it absolute. `directory` and each source `path` must remain
inside that private base. The exporter uses a confined filesystem root,
requires bounded regular files and verifies each source's SHA-256 before use.
It does not discover files, follow artifact-embedded references or contact a
cluster. Missing files, escaping paths, conflicting citations and hash mismatches
fail the export.

Export JSON is deterministic for the same manifest, sources and exporter: the
manifest supplies `generatedAt`, sources/components are sorted, and Go writes
indented JSON with its default HTML escaping enabled. The checked-in dataset
may be compact JSON, so compare decoded JSON values rather than whitespace or
pretty-output file hashes. A changed extractor can legitimately change summaries;
review the changed evidence interpretation before publishing a replacement.

## Reproduce Size100

The latest Size100 edition appends September22 fresh600/1200-Pod15QPS screens,
both rejected at60s with fully delivered900-request cohorts:106/900=11.7778%
and86/900=9.5556%HTTP429. The600 final canceled offer is cutoff evidence, not
generator loss in the decision cohort. Exact final drain counts and raw percentiles
remain separate. Both seeds completed; neither completed180s measurement. All four
prior run objects, prior private manifest, and Small/Medium bytes remain unchanged.
The49-declaration new profile retires only an absent historical Job UID after normal
CronJob retention; changed catalog bindings and differentday confound monotonic
population-capacity conclusions. Same16guest/six management nodes and$0.24986model.

Current manifest `_artifacts/dashboard-size100-v2/export.json` and source review
in the same directory; the original manifest remains a reproducible prior edition:

```bash
_artifacts/dashboard-medium-v4/hcp-perf dashboard export \
  --input _artifacts/dashboard-size100-v2/export.json \
  --output _artifacts/dashboard-size100-v2/size100-new-reexport.json
_artifacts/dashboard-medium-v4/hcp-perf dashboard render \
  --input _artifacts/dashboard-size100-v2/size100-new-reexport.json \
  --output _artifacts/dashboard-size100-v2/size100-new-reexport.html
```

### Original Size100 Edition

[Size100](dashboard/size100.html) is a separate100%-of-fee size, not renamed Medium:
target$0.25/hour, modeled$0.24986, charged4960m/20285Mi; steady4741m/18543Mi versus
scheduler4741m/18577Mi. Its [measured card](cards/size100-measured.md) documents
300Pods/60apps/16guestworkers on six management workers,5/10/15QPS180s passes and
20QPS60s screened-out in its original four cases. Decision76/1200HTTP429 is distinct from final84/1235 plus8
cancellations. No hour or exactmaximum claim; health/capture remain degraded/
incomplete. In-place update and existing-target recovery are not fresh startup.

Portable input: [size100.json](dashboard/size100.json). Private manifest and source
review: `_artifacts/dashboard-size100/export.json` and `source-review.json`.
The50 observed declarations include3exact-identity historical completed Jobs,
not additional running charge; the exporter retains43steady workload components.
Actual inventories, native spec, ledger and paired metrics are hash-pinned.
Small/Medium datasets, pages and cards remain byte-identical.

```bash
_artifacts/dashboard-medium-v4/hcp-perf dashboard export \
  --input _artifacts/dashboard-size100/export.json \
  --output _artifacts/dashboard-size100/size100-new-reexport.json
_artifacts/dashboard-medium-v4/hcp-perf dashboard render \
  --input _artifacts/dashboard-size100/size100-new-reexport.json \
  --output _artifacts/dashboard-size100/size100-new-reexport.html
```

Existing frozen offline tooling accepts V9 source fields; no operational or renderer
change is needed. New output names must not already exist. Offline tests include
`TestSize100SavedDashboard` and existing desktop/mobile browser controls.

## Reproduce Medium

Version6 adds a separate **Medium 2400-Pod Hour** pane for V8c's full40-QPS hour
on **six management workers2/2/2**, unlike earlier five2/2/1;16guest nodes remain.
All17 prior run objects and source lists are unchanged. Native execution failed
with fullWindow=true:144000 delivered/completed,136112success,7888server429s,
zero API timeouts/cancellations/generator loss, but474/480rollouts with3client
rate-limiter deadline failures/3notStarted, plus unsuccessful final canary recovery.
Raw successful p95 is1.170067598s, body-complete throughput37.80705401878536QPS.
Measured-canary hard failures0/91 is a different cohort from native aggregate1
and final recovery not_evaluated/recovered=false. Health/capture caveats remain.

Private current manifest `_artifacts/dashboard-medium-v6/export.json` uses the
same frozen v4 offline exporter/renderer. New references and lossless Pod-recorder
chunk proof are in `source-review.json`; old manifests are not edited.

```bash
_artifacts/dashboard-medium-v4/hcp-perf dashboard export \
  --input _artifacts/dashboard-medium-v6/export.json \
  --output _artifacts/dashboard-medium-v6/medium-new-reexport.json
_artifacts/dashboard-medium-v4/hcp-perf dashboard render \
  --input _artifacts/dashboard-medium-v6/medium-new-reexport.json \
  --output _artifacts/dashboard-medium-v6/medium-new-reexport.html
```

The one-hour Pod recorder exceeds128MiB and is explicitly declared as lossless
NDJSON chunks; concatenation hash matches the original allowlisted file. Native
resource matrices exist but runtime attribution remains unavailable in this export,
not zero utilization. No exporter/schema/renderer or operational source change
is needed. The repaired router placement, restored old-node scheduling and still
unresolved stale MTPNC leak are distinct prerequisites, not hidden qualification.

### Preserved Quick Campaign

Version5 added **Medium Quick Capacity**, a separate V7/current-catalog
strict-screen cohort using the same revised allocation. All12 earlier measured
objects remain unchanged. Short brackets:1200Pods45pass/60fail;2400Pods30pass/45fail.
Three delivered60s checkpoint cohorts failed server HTTP429 quality, not generator
delivery: native screened-out/fullWindow=false, with actual60.100/60.735/60.332s
offer windows. Final raw counters include cutoff/drain cancellations; the decision
fractions are separately supplied in summaries, never substituted into whole-run
latency/throughput. Red shortened-window crosses indicate failed request quality.
No new hour/exactmaximum/4800Pod result is implied.

Private current manifest `_artifacts/dashboard-medium-v5/export.json` and source
review `source-review.json` pin the campaign evidence. Export/render reuse the
frozen v4 offline binary (no exporter/renderer change or operational V7 edit):

```bash
_artifacts/dashboard-medium-v4/hcp-perf dashboard export \
  --input _artifacts/dashboard-medium-v5/export.json \
  --output _artifacts/dashboard-medium-v5/medium-new-reexport.json
_artifacts/dashboard-medium-v4/hcp-perf dashboard render \
  --input _artifacts/dashboard-medium-v5/medium-new-reexport.json \
  --output _artifacts/dashboard-medium-v5/medium-new-reexport.html
```

The native metrics exist but current runtime attribution is unavailable from the
declared inventories for these five cases; resource distributions remain null, not
zero. Request statistics are exported from raw events. Guest topology is per-case
8/16nodes, terminal16D8s_v5=128vCPU; guest cost is separate from unchanged HCP
$0.374996124267578125/hour. See the measured card for exact counts, rates, catalog
binding and preserved health/capture caveats. Earlier manifests remain immutable.

### Preserved Hour Publication

The version4 publication introduced **two separate allocation panes**. `Medium Original`
retains all eight prior measured run objects unchanged (scheduler8m/packageserver
213m, declaredCPU7216m). `Medium Revised` adds V5 and V6 1200-Pod screens/hours
(scheduler100m/packageserver153m, declaredCPU7220m). Both charge7440m/30491Mi at
the same modeled $0.374996124267578125/hour; guest costs remain excluded.
The revised pane defaults to V6's full workload hour: execution completed,
request gate passed, workload met-not-qualified, health degraded/capture incomplete.
The completed V5 hour stays failed-assessment/inconclusive with704generator losses.
No original result is reallocated or rescored, and no2400-Pod progression is implied.

Current private manifest: `_artifacts/dashboard-medium-v4/export.json`; new source
review: `source-review.json` in that directory. Export reuses frozen v2; rendering
uses the v4 offline binary with the corrected `#run-notes` mobile wrapping selector:

```bash
_artifacts/dashboard-medium-v2/hcp-perf dashboard export \
  --input _artifacts/dashboard-medium-v4/export.json \
  --output _artifacts/dashboard-medium-v4/medium-new-reexport.json
_artifacts/dashboard-medium-v4/hcp-perf dashboard render \
  --input _artifacts/dashboard-medium-v4/medium-new-reexport.json \
  --output _artifacts/dashboard-medium-v4/medium-new-reexport.html
```

The new hour Pod recorder files require lossless, hash-proven NDJSON chunks below
the exporter128MiB source bound, declared through existing `podChunks`. Native
CPU/memory matrices and timestamp companions exist with partial series coverage;
aligned whole-HCP hour coverage is236/240 CPU and239/240 memory, not absent or zero.
No exporter/schema or operational source changes are needed. The rendering CSS
selector fix and test overlay are frozen in `renderer-freeze.json`. Per-run specs,
inventories and ledger citations distinguish profiles. The community catalog drift
and population reuse are explicit confounds, not fresh clean startup qualification.

### Preserved Diagnostic Versions

[Medium's measured card](cards/medium150-measured.md) documents the September 18
Medium150 cohort: six original 60-app / 300-Pod screens and the interrupted
20-QPS diagnostic hour on three guest nodes, plus a 30-QPS five-minute screen at
120 apps / 600 Pods on six nodes. The eight-node, 240-app / 1200-Pod attempt failed
during seed and is cited diagnostic metadata, not a measured API point. Its final
214-app / 1070-Ready-Pod population converged after abort. There is no empty/unloaded
result. The hour is **partial / UNUSABLE**, not an accepted hour or
capacity ceiling. Observer/VPN loss is strongly supported, not definitive cause
attribution. Startup was interrupted and continued against the existing target,
not a fresh clean startup. No failing request-quality upper bound was found.

The maintained input is [dashboard/medium.json](dashboard/medium.json). The private
persistent manifest for the prior diagnostic edition is `_artifacts/dashboard-medium-v3/export.json`. V3 adds only
size-level diagnostic metadata for the September 19 1-QPS slow-seed failure:
about 815s before the same scheduler capture guard, 204 apps / 1020 Ready Pods
after abort, no API measurement. All eight v2 measured run objects and source
lists remain exactly unchanged. The original measured source review remains
`_artifacts/dashboard-medium-v2/source-review.json`; the new 16 diagnostic references
are in `_artifacts/dashboard-medium-v3/diagnostic-sources.json`, checked against
the original slow-seed allowlist/local seal by `review.mjs`. Their public hashes
are size configuration facts, not additions to historical run citations. The previous
`_artifacts/dashboard-medium/export.json` and all its frozen outputs remain intact;
the first seven exported run objects remain unchanged. The original
`medium-v4-retry-20260918T1450Z` and `medium-sweep-20260918T1518Z` allowlists pin
terminal native evidence and streams; the new
`medium-doubling-live-20260918T2048Z` allowlist pins the added screen and diagnostic
evidence. Explicit reviewed supplements pin the
minimum raw request events, canary events, inventories, recorder files and native
CPU/memory matrices with timestamp companions. Native matrix hashes are checked
against their saved metrics manifests. Private paths/IDs never become public
source locations; raw sources remain necessary for re-export, not rendering.

```bash
# From the worktree root; all output filenames must be new.
_artifacts/dashboard-medium-v2/hcp-perf dashboard export \
  --input _artifacts/dashboard-medium-v3/export.json \
  --output _artifacts/dashboard-medium-v3/medium-new-reexport.json
_artifacts/dashboard-medium-v2/hcp-perf dashboard render \
  --input _artifacts/dashboard-medium-v3/medium-new-reexport.json \
  --output _artifacts/dashboard-medium-v3/medium-new-reexport.html
```

V3 reuses the unchanged frozen v2 binary; `binary-reference.json` pins its hash
and existing source freeze without duplicating them. No renderer/source change
was needed. Build the maintained binary as in Build And Render if absent. Frozen v4
exports the six screens unchanged, including their newer outcome fields, but
cannot export the hour's missing native range responses. The maintained exporter
accepts empty CPU/timestamp or memory/timestamp references **only when both exact
query names occur once with status `error` and no file/hash in the hash-verified
native metrics manifest**. All measured distributions for that pair remain null,
with excluded opportunities and explanatory notes. A missing success response,
one-sided reference, duplicate query or fabricated empty matrix is not a substitute.
No frozen source, binary, original outcome or native manifest is modified. The
existing exporter also handles V5's added `expectedGuestNodes` source field:
the 600-Pod run matches a frozen-v4 export. Reviewed per-run counts are six nodes,
not the original three or terminal eight. No exporter/schema change was needed
for doubling; renderer labels and the non-JavaScript register now explicitly show
Pods/apps/nodes. The failed seed has no results/API file and remains only prose
and inert diagnostic citations, outside the measured run list and QPS chart.

Doubling includes objects and turnover (120 rollouts/hour, 10/screen versus 5),
not only Pods. API sampling remains fixed to the first three namespaces, even
with six populated namespaces. Raw successful p95 is 202.280204ms versus the saved
203ms histogram. Scheduler throttling and capture loss support pressure during
1200-Pod seeding, not a proven capacity ceiling or definitive failure mode.

`requestedMemoryGiB` is steady 27808Mi, not scheduler-effective 27860Mi. Charged
resources are 7440m/30491Mi at modeled $0.374996124267578/HCP-hour, against the
$0.375 target. The card and dataset retain pricing scope, actual 5-worker 2/2/1
management topology, per-run 3/6 guest workers, terminal 8 guest workers, exact
4.21.32 and current catalog hashes. Guest VM/disk costs remain excluded from the
unchanged HCP budget; eight guest VMs are not the cost of the measured six-node run.
Request acceptance, health, capture and execution remain separate axes.

Offline tests include `TestFailedMetricQueries` and `TestMediumSavedDashboard`,
which checks real measured points, interrupted-hour semantics, decoded embedded
JSON and desktop/mobile browser behavior when Chrome/Chromium is available. Use
a short persistent `TMPDIR` for Chrome profiles (long Unix socket paths can fail).
The Small-only page intentionally retains its historical unpopulated Medium
placeholder; use the separate Medium link above rather than altering Small's bytes.

## Reading The Results

- Keep cohort, screen/confirmation, loaded/unloaded population, stability policy,
  offered QPS and actual duration together. Repeats are not pooled. An unloaded
  run has no application Pods but is not an empty control plane or necessarily
  an absence of API load.
- The reference hour offered 18,000 requests and completed 17,638 successful
  bodies. Its logical 429-or-timeout union was 362/18,000 (2.0111%), below its 5%
  allowance. Health still failed for OAS/packageserver degradation. Strict
  4/5-QPS confirmations stopped early and remain distinct from that observation
  policy; the older mutable-catalog 1-QPS hour is a different image cohort.
- Successful QPS is successful bodies divided by actual start-to-final-body
  elapsed time, including drain. Offered rate is a workload setting, not measured
  successful throughput. HTTP 429 and timeout counts can overlap; use the saved
  union, not their sum, when assessing request degradation.
- `requests.maxDegradedFraction: null` preserves historical strict-policy
  assessment without inventing a zero union threshold. A numeric value is an
  explicit allowance in `[0,1]`, including an explicit zero. Likewise,
  `canaryHardFailed: null` means unknown, not zero observed failures; a supplied
  count must be nonnegative. Do not infer a bound from `canaryCycles` when the
  failure and measured-cycle cohorts are not established to be identical.
- Quantiles are nearest-rank empirical values: sort the entire covered cohort
  of `n` observations and choose rank `max(1, ceil(p*n))`. Q1, median, Q3, p95
  and p99 are computed in Go, not estimated from plots or recomputed in JavaScript.
- Tukey whiskers are the observed extrema within the Q1/Q3 fences of 1.5 times
  the interquartile range. The full maximum and outlier count remain available.
  Timeouts, incomplete canary stages and excluded observations are not zero
  latency. Censored/excluded counts and missing opportunities accompany the
  completed-sample summaries; a null distribution means unavailable, not idle.
- Successful API latency includes bodies finishing during drain. Resource
  windows use the actual API offer interval, excluding warmup and recovery.
  Canary distributions use measured cycles, exclude baseline/recovery, retain
  incomplete stages as censored, and distinguish Ready (including ACK) from
  independent cleanup.
- CPU is derived from raw counter deltas paired with advancing source timestamps,
  excluding resets, nonpositive timestamps and source gaps over 45 seconds, then
  integrated on the saved export grid. Memory is cgroup charge in MiB with its
  own freshness checks. Working set and RSS require their own timestamp
  companions; unavailable values are not substituted for cgroup charge.
- Component totals include required replicas and steady containers, including
  native restartable init sidecars. Zero-replica workloads, completed Jobs,
  non-restartable init containers and kube-state-metrics instrumentation are
  excluded. Inventory identity and freshness gaps stay missing.
- Aggregate simultaneous complete samples **before** computing a whole-HCP
  percentile. A sum of component p95s is not whole-HCP p95. Composition shares
  at a chosen component percentile are descriptive, not an aligned total or a
  capacity measurement. Complete resource metadata does not imply complete
  measured coverage: many Small runs have partial component observations and
  unavailable whole-HCP totals.
- Requests, hard limits and explicit `GOMEMLIMIT` targets are distinct reference
  lines. Go targets can cover only some containers and are not measured Go heap
  use. If a component lacks a finite limit on any required container, there is
  no full-component cap. Native/non-Go processes do not acquire an inferred Go
  target. Preserve the partial-reference notes.

## Cost Scope

Small's reviewed fee is $0.25/HCP-hour; its 75% target is $0.1875/HCP-hour and
its modeled resource-equivalent cost is about $0.1874427/HCP-hour. The target
envelope is about 3.722 cores / 14.888 GiB; charged resources are 3.720 cores /
14.884 GiB, distinct from steady requests of 3.507 cores / 13.638 GiB. These are
metadata from the reviewed allocation, not numbers inferred by the browser.

The underlying divisible-resource model is:

```text
target = fee * targetPercent / 100
share = max(chargedCPU / usableCPU, chargedMemory / usableMemory,
            podSlots / usablePodSlots, swiftNICs / usableNICs)
resourceEquivalentCost = nodePrice * share
```

Optional pod/NIC dimensions apply only when their capacities are supplied. CPU
and memory shares are not added and are not independently spendable budgets.
See [Cost Model](cost-model.md) and [Budget Allocator](BUDGET.md) for capacity,
rounding and allocation semantics. Small's pricing metadata names a
`Standard_D16s_v6` raw equivalent in `westus3`, whereas execution used D4-class
management hosts. This is not measured packing on the priced SKU. The declared
scope includes HCP steady workloads and startup charging treatment, excludes
kube-state-metrics, and does not charge guest VMs or observation infrastructure.
Storage, network, service overhead, fragmentation, HA and failure headroom are
not silently covered by this compute-only illustration.

## Add A Size

The contract is defined in [types.go](../internal/dashboard/types.go) and the
private export contract in [manifest.go](../internal/dashboard/manifest.go).
The portable schema is `hcp-perf.dashboard/v1`; the private manifest schema is
`hcp-perf.dashboard-export/v1`. Do not reuse the historical card schema.

1. Supply reviewed dataset metadata (`title`, `generatedAt`) and a new entry in
   `sizes`: unique `id`, display `name`, revision, status, description, budget
   scope/pricing/execution metadata, configuration facts and limitations.
2. Add explicit manifest `runs` referencing `sizeID` and a unique per-size
   `runID`. Select an existing run as that size's `defaultRun`. Supply public
   `runMeta` labels, cohort, `kind` (`screen` or `confirmation`), population
   (`loaded` or `unloaded`), application/Pod/node counts, contention marker and
   separate execution/request-quality/health/capture assessments. These labels
   are reviewed claims, not automatically inferred qualifications.
3. Declare the private run directory, HCP namespace and hash-pinned source
   references. `RunSources` requires spec, results, outcome, requests, events,
   metrics manifest, CPU and memory data with timestamp companions, inventories,
   Pods, Deployments and StatefulSets. Optional Pod chunks, independently timed
   RSS/working-set sources and extra evidence are explicitly declared, not
   auto-discovered. Each `InputRef` has `path`, `sha256`, public `id` and public
   `label`; reuse an ID only for the same citation.
4. Export to a new portable JSON. Export rebuilds runs, sources and the component
   catalog from evidence; supplied dataset runs/components are not trusted as
   measured inputs. The catalog supplies stable component IDs, display names
   and `controlled`/`downstream` scope labels. Every usage `componentID` must
   reference the corresponding size's catalog; source references must resolve
   to unique top-level citations. Keep full component metadata even where usage
   distributions are unavailable.
5. Review the portable output for privacy, evidence scope, distinct policies,
   coverage, resource reference notes and correct default run. Render that JSON
   to a new HTML file with the commands above. Adding a size, component label or
   run requires metadata/evidence changes, not changes to embedded assets.

A size may also be explicitly represented in portable JSON with no runs and no
default run, provided its metadata honestly says it is unmeasured. The current
Small-only presentation shows Medium, Large, XL and XXL as **Not populated**
placeholders. They contain no measured capacity, price or performance facts and
must not be filled by scaling Small's results.

## Privacy And Tests

Only reviewed summary fields and inert source IDs/labels/hashes enter portable
JSON. Private filesystem paths and HCP namespace are used during export, not
copied as source locations. Raw request payloads, credentials, raw logs and raw
Prometheus labels are not embedded. Public prose is still user-supplied: review
titles, notes, labels, configuration and assessment text before sharing. The
exporter is not a general-purpose secret detector. A SHA-256 citation is neither
authorization to read a path nor proof that a shared summary is correct.

Rendering reads only the supplied JSON, never citation paths or URLs. HTML
templating, Go's default JSON escaping and DOM `textContent` keep supplied text
out of executable markup. Statistical computation happens in Go during export;
native JavaScript only presents the summaries and selected views.

Run offline tests from `tooling/hcp-perf`:

```bash
GOWORK=off go test ./cmd/dashboard ./cmd/iteration
GOWORK=off go test ./...
```

Command tests cover flags, strict bounded JSON, semantic validation, cancellation,
private manifest base resolution/confinement, inert malicious source labels,
escaping, new-file permissions, existing-output races and rendering the real
portable Small fixture when present. They do not copy the private evidence or
qualify live cluster behavior.
