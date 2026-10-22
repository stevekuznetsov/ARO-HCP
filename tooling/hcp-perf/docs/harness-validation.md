# Harness Validation

Prove the harness on a short run before spending another long cycle on it. A
successful command, complete stream, or attractive report does not qualify a
sizing profile. Any missing critical data makes the experiment **invalid** for
its intended conclusion, not a qualified result with a caveat. Preserve partial
and full records and their original statuses so the failure can be diagnosed.

## Validation Matrix

Status at this documentation checkpoint, based on the reported checks and current
implementation, not a new live run performed while writing these docs:

| Stage | What it establishes | Current evidence / remaining gate |
| --- | --- | --- |
| Offline unit and integration | CLI routing, truncated schedule, mocked Kubernetes behavior, local chunk persistence, hashes, progress and embedded report data. | Offline integration succeeded. `TestShortValidationFlow` uses an HTTP fixture and fabricated canary/turnover records; it is not a guest run. |
| One-minute real observer check | Existing live Prometheus can be streamed locally and the resulting report opened. No application load is started. | Real local stream completed and browser report check passed. This does not establish all-replica coverage, recorder-PVC recovery or workload validity. |
| Five-minute actual guest workload | Seed/create, identity-scoped reads and rollouts, canary timing, incremental evidence, recovery and stop/export on the guest. | Workload exit 0 on 2026-09-09: 3 apps/15 Pods, ten canaries, one rollout, 300s steady, 30s recovery, approximately 8m30s wall time. Recorder started after seed; logs incomplete. New pre-seed readiness gate implemented, not live-validated. |
| Full soak | Sustained behavior under the reviewed treatment for 7200s plus recovery, with complete critical evidence. | Case 4 completed 24 successful canaries, 7200s steady and 600s recovery, but exited 1 due to export/health failures. No clean soak qualification or physical robustness/pressure validation. |
| Collector/network failure injection | Failure visibility, partial-data preservation, bounded shutdown and export recovery. | Not validated; requires separate authorized attempts, never injection into a scored case. |

Do not relabel the observer check as a short live guest workload. Keep the actual
artifact locations, source revision/binary digest, UTC interval and reviewer
decision with each stage's record. A browser pass establishes presentation, not
metric semantics or physical durability.

## Live Validation Record

The retained repository-root-relative evidence is
`_artifacts/perf-three-node-transition/attempt-6/run-validation-1/Result.md`, with
raw artifacts under that same `run-validation-1/` directory. The single quiet run
started at **2026-09-09 17:43:34 UTC** and completed at **17:52:04 UTC**, about
8m30s wall time. Case 4 had exited at 17:42:34 UTC; the validation unit was then
confirmed inactive/dead before canonical runner promotion.

- Seven explicit retention RGs were refreshed and verified before test resources.
- Actual seed ran 17:44:17.762 through 17:44:21.606 UTC. The recorder process
  started **after seed**, at 17:44:28.184 UTC; its first complete checkpoint was
  17:44:35.039 UTC. Pre-seed recording was not established.
- Three guest nodes hosted exactly three seeded applications and 15 Running/Ready
  application Pods. Warmup, all five baselines and all ten 30s-cadence steady
  canaries passed; one scheduled application rollout completed with no failure,
  not-started work, backlog or outstanding work.
- Steady covered 17:46:00.177 through 17:51:00.177 UTC, with recovery through 17:51:30.200.
  The final outcome was exit 0, `shutdownOrExportFailed:false`, `qualified:false`.
- Recorder stop/export passed with zero nonterminal recorder Pods. The retained
  `evidence/record/archive.tar` is 9,044,480 bytes, with independently verified
  SHA-256 `0c1f83a30d4082696a615ff4ebf105b0a892e2d67dcaca145624b34708f54839`.
  The archive/PVC remain private; raw logs may contain secrets.
- Recorder logs are incomplete: 642 errors and 216 gaps. Watch initialization
  succeeded with zero gaps; polls had zero API errors. Export success does not
  establish complete raw logging.
- Capture/node exports, analyses and health gates passed. Existing incremental
  local evidence in `../run-empty-contended-4/local-stream-20260909T1711Z` covered
  validation, while retaining `dataQuality:incomplete` for changing series.
- No CPU adversaries, live calibration, HCP tuning/budget changes, retry or source
  correction occurred. This proves the exercised quiet harness path, not lossless
  collection, all-replica coverage, physical fault tolerance, sizing or capacity.

Case 4 (`../run-empty-contended-4/Result.md`, relative to the validation run)
completed 24 successful canaries, two hours steady from 15:29:26.650 UTC and ten
minutes recovery, but exited 1 at 17:42:34 UTC due to export/health failures. It is
not a clean soak qualification. Local streaming began at 17:10:53.739 UTC, not at
the 15:29 steady start, so it did not continuously cover the full soak. Read-only
backfill into new outputs can recover metrics still retained in Prometheus, but
cannot recover missing raw logs or samples absent from the source.

The new pre-seed recorder readiness gate is implemented, **not live-validated**.
It requires a newly built/published, digest-pinned recorder image; rebuilding only
the local CLI is insufficient. Next validation must prove live that recorder
readiness and a complete baseline poll checkpoint with zero API errors precede
seed. This gate does not establish watch/log initialization or lossless coverage.

Raw `_artifacts` evidence is not rewritten. Human `Result.md` summaries carry
dated corrections above preserved, explicitly historical originals. The old launch record names the
now-removed staging filename; it is evidence, not a reusable launcher. Preserve
the executed script snapshot and source/binary hashes with future attempts.
Current runner inputs/binary hashes do not themselves preserve a script copy;
promotion does not retroactively create one or prove which script bytes ran.

These historical canary results used the earlier five-second polling default;
their timings, deadlines and statuses are not modified by the watch implementation
or a new report. Missing per-artifact measurement metadata remains unspecified,
not inferred from this historical context. Before scoring watch-based canaries,
require five fresh successful watch-based baseline cycles after warmup and derive
new expectations. Never reuse fitted thresholds across the polling-to-watch boundary. This
documentation update is not a new live validation or authorization to start a loop.

## Paths And Preflight

Commands below run from the **`ARO-HCP-perf-runner` repository root**, unlike the
module-relative examples in the main README. `HCP_PERF` is an absolute path to a
trusted binary built from the matching implementation. Use a new reviewed config,
fresh IDs and new output paths for each attempt. Existing artifact parents must
exist; no command should overwrite old evidence.

```bash
export HCP_PERF=/absolute/path/to/hcp-perf
export CONFIG=/absolute/path/to/reviewed-validation-inputs
export RUN=/absolute/path/to/new-validation-run
export OBSERVER=/absolute/path/to/new-observer-stream
export REPORT=/absolute/path/to/new-observer-report.html
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
export MGMT_KUBECONFIG=/absolute/path/to/management.kubeconfig
export GUEST_ADMIN_KUBECONFIG=/absolute/path/to/guest-admin.kubeconfig
export HCP_NAMESPACE=verified-control-plane-namespace
export CAPTURE_BUNDLE=/absolute/path/to/scrape/bundle.json
export CAPTURE_ENDPOINT=http://127.0.0.1:9091
export NODE_METRICS_ENDPOINT=http://127.0.0.1:9090
export SCRAPE_MAX_AGE_SECONDS=45
```

Both endpoints are existing dedicated Prometheus API base URLs, not exporter
`/metrics` URLs. Preserve the endpoint-to-capture-ID mapping and keep captures and
authorized loopback port-forwards alive through export. Do not enable shell tracing.

- Confirm the old case and its generators have exited before another workload.
  Bash can still read an executing script: do not edit/replace `run-case.sh` while
  active. `run-case.sh` is now canonical after confirmed process exit; use fresh
  reviewed inputs with `--validation` before another long soak.
- Before **each validation**, including an observer check, refresh the reviewed
  `retention.json` allowlist with `environment refresh --input FILE --timeout 5m`.
  Require zero exit, `status:verified`, `dryRun:false`, every exact RG attempted,
  updated and verified, no uncertain/failure result, and fresh UTC `createdAt`
  timestamps. Inventory environment, customer and managed/node RGs by full ID.
  An old refresh or a dry run does not count. Refresh does not cancel a cleanup
  already running and is neither a lease nor a guarantee against deletion.
- Verify authorization, exclusive ownership, guest node count and per-node Pod/IP,
  CPU/memory capacity including platform Pods, canary and rollout surge. Review
  image digests, kube-burner executable hash, token/certificate expiry and storage.
- Review every required source/query against real metrics. `up` plus
  `timestamp(up)` must use identical, unaggregated target selectors, including down
  targets. Ready capture and `scrape check` are not missing-replica detectors.
- Assign a named operator to the capture/recorder PVCs and backing PVs: record
  namespace, PVC/PV identity, StorageClass/reclaim policy, capacity, export location
  and eventual deletion approval. The caller owns local chunks and reports. No
  successful run transfers evidence ownership or authorizes deletion.

The canonical script repeats the retention refresh and exact-inventory gate before
test resources. It reads the primitive files documented in
[scripts/README.md](../scripts/README.md#primitive-files), not a case-plan JSON.

## Offline And Observer

Run tests without Azure or Kubernetes access, from the repository root:

```bash
env GOWORK=off go -C tooling/hcp-perf test -race ./...
env GOWORK=off go -C tooling/hcp-perf test -run '^TestShortValidationFlow$' -v .
"$HCP_PERF" case plan \
  --case tooling/hcp-perf/examples/validation-case.json \
  --profile tooling/hcp-perf/examples/small.json
"$HCP_PERF" case schedule \
  --case tooling/hcp-perf/examples/validation-case.json \
  --profile tooling/hcp-perf/examples/small.json
```

The example has zero applications and therefore no application rollout. Planning
is offline, not a sizing-policy apply or live execution. The current CLI
`case schedule` filters offsets strictly below the resolved steady duration;
the underlying hourly schedule is not accelerated.

After the retention/source preflight, an observer-only check against the existing
capture uses the implemented streaming flags:

```bash
"$HCP_PERF" environment refresh --input "$CONFIG/retention.json" --timeout 5m
"$HCP_PERF" scrape check --input "$CAPTURE_BUNDLE" \
  --endpoint "$CAPTURE_ENDPOINT" --timeout 1m
"$HCP_PERF" metrics stream --endpoint "$CAPTURE_ENDPOINT" \
  --queries "$CONFIG/queries.json" --output "$OBSERVER" \
  --duration 1m --chunk 30s --step 5s --lag 15s \
  --drain 90s --chunk-timeout 90s
"$HCP_PERF" analyze --input "$OBSERVER/chunk-000000"
"$HCP_PERF" report render \
  --metrics "$OBSERVER/chunk-000000" --metrics "$OBSERVER/chunk-000001" \
  --output "$REPORT"
```

Inspect and gate the refresh result before proceeding; this command block is not
an automatic retention-result checker. Repeat the observer check for a separate
node capture with its own query set and new output path. During the stream, use a
second terminal to inspect the checkpoint after the first batch:

```bash
jq '{status,dataQuality,updated,completedChunks,pendingChunks,behind,errorCount,incompleteCount}' \
  "$OBSERVER/manifest.json"
```

The first window becomes exportable after chunk plus lag (45s here), then query
execution takes additional time. Confirm raw output before the stream ends;
stdout is only a final result. `complete` means attempted windows, not complete
source coverage. `behind`, pending chunks, errors and incomplete query records
must remain visible. See [Stream And Report](../README.md#stream-and-report) for
all defaults/bounds. No `--start`, resume, retry or automatic backfill exists for
streaming. For history preceding this stream, use the separate fixed-window CLI
while the source still retains it:

```bash
"$HCP_PERF" metrics backfill --endpoint "$CAPTURE_ENDPOINT" \
  --queries "$CONFIG/queries.json" --output /absolute/path/to/new-backfill \
  --start 2026-09-09T17:44:07Z --end 2026-09-09T17:51:31Z \
  --chunk 1m --step 5s --timeout 30m
```

Replace example timestamps with the reviewed interval. Backfill bounds the range
to 24h/1440 windows and chunks to at most 5m, preserving raw responses, hashes,
per-query statuses and `.window.json` under each `chunk-NNNNNN/`, plus a root
checkpoint. It never retries, resumes, splits failed windows or overwrites an
existing output. End is not rounded up; adjacent inclusive windows share a grid
boundary. Deduplicate only by query, full labels and timestamp. Bounded exports
cannot reconstruct absent source data or guarantee lossless collection.

## Five-Minute Workload

Use `guestNodes:3` consistently in seed/setup/turnover for the three-node check.
Start with the validated `applications:3` (15 steady application Pods) to exercise
seed/create, list/read and rollout paths; 12 apps (60 Pods) is a larger check.
Zero applications tests platform plus canary, not application lifecycle.

The unaccelerated hourly schedule has a first offset of zero. Both 3 and 12 apps
in 300 seconds schedule **one rollout event**, not six or twelve:
`ceil(applications * 300 / 3600) == 1`. For 12 apps, the next offset is 300s and
is excluded. Expect five new application Pod identities from that completed
five-replica rollout, separate from seed Pods and canary Pods. Verify actual
ownership/generation/readiness evidence rather than treating the schedule as proof.

Set these reviewed values in the primitive inputs and invocation:

| Setting | Value / requirement |
| --- | --- |
| `seed.json`, `setup.json`, `turnover.json` | Matching node/app counts and identity/prefix; seed/setup also match image and ID. |
| `turnover.json` | `durationSeconds:300`, `recoverySeconds:30`, explicit `seed`, reviewed `maxOutstanding` in 1..20. |
| Script mode | `--validation`; without it, soak requires exactly 7200 steady seconds. |
| Settling | `--settle-seconds 30`; omission still defaults to 300 in either mode. |
| Steady canaries | `--canary-cadence-seconds 30`, ten scheduled start-to-start slots in 300s; record missed slots, never catch up. |
| Baseline | One unscored warmup, then five fresh consecutive successful watch-based cycles; derive new soft expectations, never reuse historical polling thresholds; no skipped/failed-cycle retry. |
| Recorder lifetime | At least `300 + 30 + 30 + seed.timeoutSeconds + 1800`, at most 86400s. With a 60s seed timeout, minimum 2220s. Include the HCP namespace and `enableWatch:true`. |
| Optional adversary lifetime | At least `300 + 30 + 30 + 120 = 480s`, at most 86400s; allow additional startup time. |

```bash
bash tooling/hcp-perf/scripts/run-case.sh \
  --validation --settle-seconds 30 --canary-cadence-seconds 30 \
  --config-dir "$CONFIG" --artifacts-dir "$RUN"
```

Do not add `--contended` until the quiet harness works and the CPU treatment is
separately authorized. Validation permits 30..1800 steady and 1..120 recovery
seconds. Cadence is 5..steadySeconds; its validation default is
`max(5,min(30,durationSeconds/5))` using integer division. Settle accepts 1..86400s.
Five-minute **steady** is not five-minute wall time: seed, image warmup, baseline,
settle, recovery, recorder setup and export are additional. Baseline budgets are
30s ACK / 120s Ready / 120s cleanup, not mandatory sleeps. The runner holds the
30s recovery window even if no turnover remains.

### Canary Contract

One cycle is **`0 -> 1 -> 0`** for the dedicated guest Deployment. Ready requires a
fresh Running/Ready, non-deleting Pod UID, exact Deployment -> ReplicaSet -> Pod
controller ownership, and observed Deployment generation/status convergence.
Cleanup waits for zero Deployment/ReplicaSet replicas and no owned Pods, including
terminating or terminal Pods. Labels or a previously Ready Pod are insufficient.
The client uses the bounded protected APF identity, never an exempt/admin identity;
controllers, scheduler and kubelets keep their native identities and ordinary
request paths. A passed cycle does not prove every path continuously healthy or
establish APF classification without separate evidence.

The client registers Deployment, ReplicaSet and Pod LIST/WATCH streams from their
respective LIST resourceVersions before scale-up. Cleanup has independent bounded
observation setup before scale-down. Completion is client watch receipt of the
evidence satisfying convergence, with monotonic elapsed timing, not server
transition timestamps. There is no fixed-poll quantization, but network,
controller reconciliation, watch delivery and client observation latency remain.
Cross-kind streams are not an atomic snapshot; stream gaps/errors invalidate the
measurement rather than being repaired into success by a relist.

ACK measures scale-up start to API acknowledgement. Ready starts at that same
instant and **includes ACK**; they are not additive. Cleanup is a separate latency
from its own start through observation setup, scale-down and observed removal.
Preflight is separate from ACK/Ready. Inspect each stage's own recorded duration,
hard budget, soft expectation, completion, `measurementMode:watch` and watch diagnostics, not just exit
zero or the apparent width of a timeline bar.

New runs use `canaryPolicy:"observe-degradation"` and objective
`"Observe and record progress and degradation, boundedhardstop"`. A single exceeded
expectation is not a reason to abort: observation continues to the hard deadline,
and a completed degraded cycle returns zero. Hard timeout, logical/functional
error or cleanup failure still ends admission and triggers bounded recovery.
Five baseline cycles produce `{ack,ready,cleanup}` nanoseconds in
`commands/baseline-expectations.json`, each `min(3 * maximum, cap)` with unchanged
30s/120s/120s caps. Steady calls use `--ack-expected`, `--ready-expected`, and
`--cleanup-expected` for those values, independently of fixed `--ack-budget 30s
--ready-budget 2m --cleanup-budget 2m`. Preflight is bounded at 30s. Since Ready
includes ACK, preflight plus Ready plus cleanup is at most 270s, within the 5m CLI
timeout; independent cleanup may extend past cancellation. The outer call bound
is 6m, with the existing 150s forced-kill grace as a shutdown backstop.

Cadence is admission scheduling, not a workload timeout. After each serial cycle,
all elapsed slots (including a slot exactly at completion) are recorded as missed
and the runner waits for a future slot. No overlap or catch-up burst is allowed.
Every unattempted slot gets one `missed-slot` event with its index and scheduled
UTC time in synced `evidence/canary-cadence.jsonl`; command numbering uses slot
index plus one, leaving explicit gaps. A final active cycle may finish after
steady ends. Its `cycle-completed` event retains `afterWindow:true`, `scored:false`
and `resultFile`; late stage satisfaction and diagnostics stay in the command
result. No after-window slot is attempted. These cases may have workload exit 0,
but fewer samples/late completion mean incomplete observation coverage and never
automatic qualification. This policy and the new scheduler are offline-tested,
not a new live-validation result.

As the default collection workflow, start separately supervised local metrics
streams before launching each case, sized to cover setup,
baseline, steady and recovery, with new private output directories. The case
script still performs final bounded exports; it does not launch these streams.
Verify their first chunks before trusting them with the full window. A stream
started mid-run cannot reconstruct earlier phases by itself.

For contended validation, `adversaries discover` previews live target placement.
Omitting `reserveCPUMilli` and `cpuFillPercent` selects 90% free CPU request
headroom per target worker; an explicit `cpuFillPercent` is 1..100 and mutually
exclusive with a reserve. This is automatic placement/request fill, **not** an
empirical CPU-pressure feedback controller. Memory defaults are 32/128 MiB, not
measured requirements. Start freezes the registered plan. Keep treatment fixed
while scoring, inspect `placementValid`, `placementValidity` and `drift`, and
verify actual pressure per worker. Presence of adversary CPU samples alone is
insufficient. There is no automatic replan or pressure-threshold qualification.

## Progress And Artifacts

Pass the case **root** to progress, not `$RUN/evidence`. In another terminal:

```bash
"$HCP_PERF" case progress --input "$RUN" | jq '{
  mode,phase,activityPhase,provisional,qualified,observedAt,
  lastActivityAt,lastActivitySource,staleAgeSeconds,steady,
  baseline:(.baseline|{expected,passed,pending,failed,remaining}),
  steadyCanaries:(.steadyCanaries|{expected,passed,pending,failed,remaining}),
  turnover,outcome,conditions,warnings
}'
```

This is a local snapshot, not process liveness. Preserve phase-boundary snapshots
under new private filenames outside the run root: `phase`, `activityPhase`, mode,
provisional/outcome state, last activity, elapsed/remaining/overdue time, baseline
and steady counts, pending/failed stages, budgets, conditions and warnings matter.
Missing fields remain unknown; do not manufacture completion from elapsed time.

| Path | Expected evidence |
| --- | --- |
| `$RUN/inputs/session.json` | Resolved mode, 300s steady, 30s recovery/settle/cadence, five baselines, ten steady canaries, one scheduled rollout for 3 or 12 apps. |
| `$RUN/inputs/`, `$RUN/evidence/*inputs.sha256` | Reviewed input snapshots and hashes, including retention. |
| `$RUN/commands/NAME.{started,json,stderr,exit}` | Command timing, result and failure evidence, including retention refresh/gate and seed inventories. |
| `$RUN/commands/baseline-expectations.json` | Soft ACK/Ready/cleanup thresholds in integer nanoseconds; referenced with flag names by session/run metadata, separately from `hardDeadlineSeconds`. |
| `$RUN/evidence/canary-cadence.jsonl` | `cycle-started`, `cycle-completed`, `missed-slot`; zero-based `slotIndex`, one-based `canaryNumber`, scheduled times, completion/result references and after-window diagnostics. |
| `$RUN/evidence/run.json`, `outcome.json` | Mode, run metadata and final stop/export outcome; `qualified:false` remains. |
| `$RUN/evidence/window-start.txt`, `steady-start.txt`, `window-end.txt` | Observation and steady boundaries, not inferred startup coverage. |
| `$RUN/evidence/turnover/{events.ndjson,result.json}` | Incremental rollout state, final counts and ownership/readiness snapshots. |
| `$RUN/evidence/record/{archive.tar,manifest.json}` | Read-only recorder-PVC export and integrity metadata; preserve failed-session exports too. |
| `$RUN/evidence/metrics-capture/`, `metrics-nodes/`, optional `metrics-pressure/` | Final bounded exports; preflight exports use `preflight-capture/` and `preflight-nodes/`. |
| `$OBSERVER/manifest.json`, `journal.jsonl`, `chunk-NNNNNN/` | Separately managed stream checkpoints and full raw local query chunks. |
| `$RUN/private/` | Seed outputs and benchmark/canary credentials; never publish wholesale. |

Outcome fields for parent readers are `expectedSteadyCanaries`,
`attemptedSteadyCanaries`, `missedSteadyCanaries`, `completedAfterWindow`, and
`observationCoverage` (`incomplete` on misses, after-window completion or failures;
otherwise `not_assessed`). After steady admission starts, attempted plus missed
must equal expected, including early-failure exits. Progress/assessment readers
must prefer `commands/baseline-expectations.json` and fall back to historical
`commands/baseline-budgets.json` only if the new file is absent. Interpret old
thresholds using historical policy/stage metadata; do not rewrite prior artifacts
or relabel their hard failures as soft degradation. The runner changes do not
themselves implement the parent readers or qualification gates.

After final export, build a durable assessment and render a reviewed report from
the **same existing run**. This is offline reanalysis, not another case launch.
For a second-generation report, leave the historical HTML and raw artifacts
read-only. Use an existing private review directory; the new `assessment.json`
and `report-reviewed.html` filenames must not already exist:

```bash
"$HCP_PERF" assessment build --input "$RUN" \
  --metrics "$RUN/evidence/metrics-capture" \
  --metrics "$RUN/evidence/metrics-nodes" \
  --recorder-summary /absolute/path/to/extracted/stream-summary.json \
  --output /absolute/path/to/review/assessment.json
"$HCP_PERF" report render --input "$RUN" \
  --metrics "$RUN/evidence/metrics-capture" \
  --metrics "$RUN/evidence/metrics-nodes" \
  --assessment /absolute/path/to/review/assessment.json \
  --output /absolute/path/to/review/report-reviewed.html
```

The optional `--recorder-summary` is an explicit, already extracted external file;
omit it when unavailable. The builder does not extract archives or read raw logs.
A detached summary's counters are evidence, not proof of archive linkage, pre-seed
readiness or whole-run coverage. Neither command contacts the cluster or starts
workloads. Repeated `--metrics` selects exact export roots, including individual
completed stream chunks. Do not supply the stream parent expecting recursion.

`--input` plus `--assessment` requires SHA-256 equality on shared evidence paths,
at least one matching in-run path, and no conflicting explicit experiment IDs.
External metrics alone do not identify the run; imported paths are not followed
automatically. This check is not authentication or a full revalidation of every
referenced artifact. Without `--input`, assessment-only rendering cannot match a
run. Without `--assessment`, a run input builds an embedded assessment and permits
`--recorder-summary`; that flag requires `--input` and conflicts with `--assessment`.
Rendering creates no implicit JSON sidecar.

The [machine schema](assessment.md#public-schema) preserves `schemaVersion`,
`analysisVersion`, findings and evidence references with hashes, numeric data
points with `unit`, `population`, `window`, `measurementMethod` and provenance,
and carry-forward scope, blockers and conditions. `assessment build` writes a new
`0600` JSON file and syncs the file and parent directory. CLI success means the
artifact was written, not that the run passed. Workload/capture statuses stay
separate; qualification remains `not_assessed`. Do not guess missing methods,
counts or statuses, or auto-apply carry-forward recommendations. Do not include
credentials, Secret data or raw logs in review inputs; outputs remain private.

Reports can be rendered mid-run under new filenames, but do not refresh. They embed local
progress, labels, summaries and visualization points, not recorder archives or
raw logs. Reports are bounded sampled previews: `--max-series` defaults to 200
(1..1000 overall), `--max-points` to 300 (2..500 per series), and non-up queries
retain at most 20 preview series each. Omitted counts remain visible; all-series
query totals are not limited to the preview. Selection is not lossless or
statistically representative. Raw chunks, hashes and full warnings remain
unchanged as the full-resolution record; an HTML pass cannot establish capacity.

For visual review, expect one canary group with three separate lanes: **API
acknowledgement**, **Pod ready (includes ACK)** and **Removed after scale-down**
(Cleanup). There is no duplicate canary command-start row. Recognized phase starts
occupy a separate band, and all commands remain in details. Short stages use a
minimum-width marker; tooltips and the one-row-per-cycle table retain exact times
and each stage's own latency/budget. Missing completions remain unknown.

Use **Display labels** to choose `namespace`/`pod`/`container` instead of crowding
the legend with every raw label. Expand **Full labels and selected series metadata**
to inspect full labels and sample/coverage facts. Colliding display names retain
distinct raw series with stable `[series N]` suffixes, not merged or aggregated
traces. Changing display labels must not change plotted data.

The default 200-series selection is a UI heuristic, not evidence that every
component was analyzed for sizing. Full-sample metadata for selected series and
all-series query totals are distinct from the downsampled plot; omitted series
remain in the raw files. Values retain raw query units. No all-component or
whole-HCP CPU/memory/GC calculation is implemented yet; such conclusions require
separate unit-aware, time-aligned evidence and reviewer-owned formulas.

## Acceptance Checklist

- [ ] Source coverage: compare independent desired Deployment/StatefulSet replica
  inventory and Pod/container UIDs with targets and per-series samples throughout
  every phase. `up==1` for returned series cannot reveal an entirely missing
  replica. Include all HCP workers, neighbors/adversaries, component metrics and
  guest evidence needed for the conclusion. The management recorder is not a
  guest watch/log recorder, and a post-provisioning run cannot prove startup.
- [ ] Freshness and completeness: retain source timestamps as well as evaluation
  timestamps, down/stale/absent targets, gaps, nonfinite samples, query errors and
  hashes. Account for real series birth/death without promoting `incomplete` to
  `success`. Missing critical data invalidates the experiment even if script gates
  permit recording to finish. Optional queries cannot hide a required source.
- [ ] Physical interpretation: distinguish Go post-GC live heap, heap allocation,
  GC frequency/pause/CPU from process RSS and container working set/usage. Preserve
  per-process identity and resets; do not infer live heap from RSS or memory safety
  from a short run. For contention, verify CPU usage and pressure/saturation on
  each relevant worker against the reviewed criterion, not request fill or Job Ready.
- [ ] Workload and phases: verify exact seed counts, five successful baselines,
  ten accounted steady slots (attempted plus explicitly missed), one completed application rollout with five new Ready Pod
  identities, full 300s admission window and 30s recovery. Inspect failed/pending
  stages and timestamps; expected counts are not execution evidence. Missed slots,
  soft degradation and after-window satisfaction remain visible even on workload
  exit 0. A clean coverage claim still requires all ten in-window samples; apply
  qualification separately rather than aborting observation on one soft threshold.
- [ ] Incremental output: inspect actual raw chunks and recorder checkpoints while
  running, before final export. Confirm storage headroom and that lag/backlog is
  visible. Filesystem sync and unit tests do not prove PV/host-failure durability.
- [ ] Shutdown: confirm stopped local turnover/canary processes, suspended/terminal
  adversary and recorder Jobs and zero nonterminal generator Pods; inspect partial
  submissions and export-helper cleanup. No orphaned generators may remain.
  Intentionally retained applications, fixtures, captures and PVCs are inventory,
  not an excuse to ignore unknown resources. Prometheus stays running for export.
- [ ] Offline visual review: open the HTML with networking unavailable and source
  files inaccessible to the browser, without deleting the originals. Check desktop
  and narrow view, phase/canary timeline, budgets, query filtering, series selection,
  CPU/memory/GC panels, gaps, missing-source warnings and partial/failed records.
  Verify one group/three lanes per canary without duplicate command-start rows,
  display-label collision suffixes, full-label expansion and unchanged raw traces.
  Full-resolution source records and hashes must still be retained separately.
- [ ] Privacy and ownership: raw metrics/HTML labels, expressions and server errors
  may be sensitive. Raw logs require both `enableLogs:true` and `rawLogs:true` with
  explicit acknowledgement; their exports are not sanitized. Keep credentials,
  diagnostics, chunks, archives and reports private. Obtain the evidence owner's
  approval before PVC/namespace deletion or local pruning; check PV reclaim policy.

## Soak And Faults

Only after the short workload checklist passes, run the reviewed full soak without
`--validation`: 7200s steady, normally 300s canary cadence (24 steady cycles),
five baselines and 600s recovery. Refresh retention again and use fresh identities
or separately authorized removal of the previous apps; retained load must not
silently accumulate. Keep the scored treatment frozen. A full soak still needs
all critical evidence and a separate analysis, never an automatic sizing verdict.

With the prior soak finished and its evidence preserved, use separate
authorized attempts for collector termination and network interruption. Record
the exact target/process, injection UTC interval, expected gaps and recovery
procedure. Do not interrupt a scored case's collector or port-forward.
Retain existing chunks, journals, manifests, PVC data, partial records and error
statuses. Verify failure visibility and no orphaned generators, then export
terminal failed recorder sessions only after no nonterminal writer Pods remain.
Restart local streaming into a **new** directory; use `metrics backfill` for any
recoverable historical gap. Never overwrite or mark the interrupted record full.

Stopping collection is not evidence cleanup. Leave source storage intact until
exports, hashes, offline review and ownership handoff are verified. Robustness
fixes and mocked tests do not establish physical failure tolerance until these
live checks have actually completed and their evidence has been reviewed.
