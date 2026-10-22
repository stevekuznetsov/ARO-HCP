# Durable Offline Assessment

An assessment records what a run established, what remains unknown, and what a
reviewer should carry forward. It is not a qualification engine, live monitor,
configuration optimizer, or replacement for immutable source artifacts.

```sh
hcp-perf assessment build --input RUN_DIR \
  --metrics RUN_DIR/evidence/metrics-capture \
  --metrics RUN_DIR/evidence/metrics-nodes \
  --recorder-summary EXTRACTED/stream-summary.json \
  --request-evidence REVIEW/request-evidence-v1.json \
  --output NEW_JSON
```

`--metrics` is repeatable and optional; each value is an exact export root, not a
recursive search. `--recorder-summary` is optional and must reference an explicitly
provided, already extracted stream summary. The builder never untars recordings,
parses raw log streams, executes recorded commands, or contacts Kubernetes or
Prometheus. No `--notes` import is implemented in this initial version. Reviewer
conclusions are not invented or automatically promoted to observed findings.

The output parent must exist. Output creation is exclusive, mode `0600`, with file
and parent-directory sync. Existing assessments and input artifacts are never
overwritten. Input-evidence failures are assessment findings, not CLI failures;
invalid flags, an unavailable run root, cancellation, and output errors fail the
command. Invalid explicitly supplied request evidence also fails the command.
Thus CLI exit zero means the assessment was written, not that the run
passed. Inspect `status` and `findings`.

New runner outcomes add `status.execution` (`completed`, `aborted`,
`harness_error`, `unknown`) and `status.experiment` (`within_expectations`,
`degraded`, `violations`, `unknown`). The validated runner fields are retained in
`outcome`, with a source-hashed finding; `commandProblems` retains sanitized
label-based entries from `evidence/failures.jsonl`. Invalid classifications produce
inconclusive findings without echoing raw content. Historical outcomes lacking
classification retain unknown execution; combined nonzero exits are never guessed
to be expectation violations.

Existing workload, performance, capture and qualification fields keep their
semantics. In particular, workload `fail` and execution `completed` can coexist
when the full window finished but a terminal workload gate failed. Completed soft
violations with a failed metrics export can retain workload exit zero, degraded
performance and incomplete capture. Neither case establishes valid capacity.
Runner capture `complete` only describes its own command gates and cannot override
the assessor's independently observed missing/nonfinite metric or recorder evidence.
Coverage counts are independent of performance counts; unknown safety observations
remain null rather than inventing a pass from an absent external monitor artifact.

An assessed violation is normal terminal experiment feedback. Parent orchestration
should record it and proceed to its next reviewed decision, not equate the failed
gate with an unhandled crash. Genuine CLI/input/output failures still fail; safety
aborts and incomplete evidence still require the applicable review hold. No new
continuation flag or automatic campaign execution is introduced.

## Integration API

```go
result, err := assessment.Build(ctx, assessment.Options{
    Input: runDir,
    Metrics: metricRoots,
    RecorderSummary: summaryFile, // optional
    RequestEvidence: requestFile, // optional reviewer-correlated audit summary
    Now: generatedAt,             // optional; defaults to current UTC time
})
```

The CLI handler is `runAssessment(ctx, args, out)` and receives full arguments
starting with `assessment build`. `report render --assessment FILE` decodes and
embeds an explicitly supplied assessment as `data.assessment`. With `--input`, it
also checks known experiment IDs and SHA-256 equality on shared evidence paths;
at least one matching in-run path is required, not just shared external metrics.
It never follows arbitrary imported evidence paths. Without `--input`, importing
an assessment cannot perform that run match.

When `--input` is supplied without `--assessment`, rendering builds an embedded
assessment but no durable JSON sidecar. Its optional `--recorder-summary` and
`--request-evidence` require `--input`. When importing with `--assessment`, supply
the original explicit sources again for fresh hash and failure validation. Metrics-only rendering does
not invent an assessment. Use `assessment build` for the durable machine artifact;
neither path qualifies a run. See the [existing-run example](../README.md#review-an-existing-run)
for separate new `assessment.json` and `report-reviewed.html` outputs that preserve
historical reports and evidence.

## Public Schema

`failures[]` includes the failed command, stage, operation, client status, elapsed
kind/time, nullable deadline, completion observation, Deployment UID, recovery,
application outcome and evidence references. A timeout is time to client failure,
not completed request latency. An optional `serverObservation` records HTTP code,
audit-only duration, received/completed timestamps, audit ID, correlation,
provenance, optional reviewer-declared source line, and evidence references.

## Request Evidence

Both `assessment build` and `report render --input RUN_DIR` accept optional
`--request-evidence FILE`. The supported schema matches this structure:

```json
{
  "schemaVersion": "hcp-perf.request-evidence/v1",
  "observations": [{
    "command": "canary-steady-7",
    "auditID": "705d557d-8564-4104-b940-865da7ab45a3",
    "verb": "update",
    "path": "/apis/apps/v1/namespaces/example/deployments/canary/scale",
    "objectUID": "DEPLOYMENT-UID-FROM-CANARY-RESULT",
    "responseCode": 200,
    "stage": "ResponseComplete",
    "receivedAt": "2026-09-09T22:40:31.678180Z",
    "completedAt": "2026-09-09T22:40:31.786989Z",
    "sourcePath": "record/logs.ndjson",
    "sourceSHA256": "SHA256-OF-EXACT-SOURCE-BYTES",
    "sourceLine": 101551,
    "correlation": "path-uid-time-serial"
  }]
}
```

The summary is bounded to 1 MiB. Each observation must uniquely match a failed
ACK command and its recorded Deployment UID. Paths must be canonical namespaced
Deployment `canary` scale paths, without hosts, queries or credentials. HTTP codes
must be integers in 100..599; timestamps must be present, ordered and not future.
The source is a regular, unaliased file below the summary directory, at most
32 GiB, streamed only for SHA256 verification. Parent traversal, absolute source
paths, symlinks and hash mismatches fail the import. Optional `artifactReferences`
are not followed or promoted into verified evidence. `sourceLine`, when supplied,
must be positive but is not parsed or independently checked against the log.

Provenance is explicitly `reviewer-correlated source-hash-verified`. Matching
hashes prove byte linkage, not authenticity or that summarized fields occur in
those bytes. Correlation is reviewer-declared path/UID/time/serial matching, not
shared client request-ID correlation. The builder never subtracts client times
from audit times. A 2xx audit report sets `applicationOutcome` to
`server_success_observed`; it does not establish client receipt, change the
failed workload gate, qualify service availability, or identify CPU/APF causation.
Other codes remain visible without inferring whether the mutation was applied.

With `report render --assessment FILE --input RUN_DIR`, the original
`--request-evidence` is required to revalidate imported audit observations. Source
changes, missing sources, changed failure details or changed server observations
reject the import. Assessment-only rendering remains an explicitly unverified
import and never authorizes reads of embedded paths.

## Schema Fields

All listed fields are emitted unless marked optional. Arrays are JSON arrays,
including when empty. Numeric data-point values are JSON numbers, never formatted
strings. Unknown counts and window endpoints are `null`, not zero.

| Object | Fields |
| --- | --- |
| Root | `schemaVersion`, `analysisVersion`, `generatedAt`, `experiment`, `status`, `findings[]`, `failures[]`, `evidence[]`, `dataPoints[]`, `carryForward[]`, `metricsCoverage[]` |
| `experiment` | `id`, `title`, `question`, `mode`, `guestNodes`, `applications`, `pods`, `steadySeconds`, `cpuTreatment`, `measurementMode`, `fieldEvidence` (field name to evidence ID array) |
| `status` | `workload`: `pass\|fail\|incomplete`; `capture`: `complete\|incomplete\|unknown`; `qualification`: `not_assessed` |
| Finding | `id`, `category`: `workload\|measurement\|capture\|resources`, `status`: `observed\|inconclusive`, `statement`, `evidence[]` (IDs), `scope` |
| Evidence | `id`, `path`, `sha256`; optional `schema`, `window`, `query`, `jsonPointer`, `units` |
| Data point | `id`, `name`, `value`, `unit`, `statistic`, `window`, `population`, `provenance`, `measurementMethod`, `limitations[]`, `applicability` |
| Provenance | `evidenceRefs[]`, `jsonPointers[]`; optional `formula` |
| Carry forward | `id`, `action`, `recommendation`, `evidenceRefs[]`, `dataPointRefs[]`, `scope`, `blockedBy[]`, `conditions[]`, `kind`: `hypothesis\|retain\|fix_harness` |
| Window | `start`, `end` (RFC3339 timestamps or `null`) |
| Metrics coverage | `evidenceRefs[]`, `query` (query name), `status` (Analyze status), `window`, `sampleWindow`, `series`, `samples`, `validSamples`, `invalidSamples`, `expected`, `missingApprox`, `incompleteSeries`, `fraction` (number or `null`) |

`schemaVersion` is exactly `hcp-perf.assessment/v1`; `analysisVersion` is `v1`.
`generatedAt` is UTC. Schema-compatible additive fields are permitted. Consumers
must reject unsupported major schema versions; `Decode` also rejects unknown
analysis versions, empty documents and invalid status axes. It is not evidence
authentication or a full semantic validator for reviewer-authored documents.

IDs are unique within one assessment. They are deterministic for the same inputs,
explicit source order, and clock, but are not cross-assessment identities. Use
source hashes and scoped point names when comparing assessments. `path` is relative
to `--input`; explicitly supplied external roots legitimately produce `../` paths.
Consumers must not treat these paths as safe extraction destinations or
automatically follow them. JSON pointers describe source fields; formulas, when
present, explain derivations rather than executable instructions.

## Evidence Rules

The run-file allowlist is:

- `inputs/seed.json`, `inputs/turnover.json`, `inputs/session.json`, `inputs/case.json`, `inputs/profile.json`.
- `evidence/run.json`, `evidence/outcome.json`, `evidence/resolved-profile.json`.
- `commands/guest-capacity.json`, `commands/turnover.json`, `evidence/turnover/result.json`.
- `commands/canary-baseline-N.json`, `commands/canary-steady-N.json`, and their matching `.exit` files, for positive integer `N`.
- `commands/baseline-budgets.json`; failed canary JSON and matching JSON `.stderr` (including `canary-warmup`), with error text used only for classification and never persisted.
- Known workload command `.exit` and `.started` markers, including turnover and its gate, baseline, seeding, warmup, adversary startup, settle, `failure-recovery` and `recovery-window`. Only markers are read for commands that produce credential or object output. Capture/finalization exits, the expected `workload-dispatch` failure, and arbitrary command names are not workload evidence.
- `evidence/record/manifest.json` and, for streaming hash verification only, `evidence/record/archive.tar`.

Metadata reads are bounded to 1 MiB per file and reject leaf symlinks and aliased
parent directories. Missing, malformed, future-version, oversized or unavailable
metadata is not success. SHA256 covers the exact bytes decoded. The builder never
follows arbitrary artifact names from input documents. It does not include
credentials, raw object dumps, arbitrary stderr, or raw log filenames in evidence discovery.
Explicit request evidence authorizes bounded streaming hashes of its named sources
as described above; raw log contents are never parsed or embedded.
Explicit metrics queries and their labels may still contain private application
information: assessment output is private, not automatically publication-safe.

Experiment fields come from explicit recorded values, not directory names, current
source-code defaults, elapsed duration, or an expected profile name. Contradictory
values produce an inconclusive finding and leave the field unknown. Text defaults
to `unknown`; measurement mode defaults to `unspecified`. Mode is never inferred
as `validation` or `soak` from duration. `contendedRequested` records the requested
CPU treatment, not proof of actual contention. `pods` is the explicitly recorded
application-pod count, excluding system pods, canaries, and surge; no arbitrary
replica multiplier is assumed when that count is absent.

Sizing anchors are read only from `inputs/profile.json` or
`evidence/resolved-profile.json` at `/anchor/cpuMilli` and `/anchor/memoryMiB`.
They are configured policy data points, not live applied requests. A run spec's
`expectedProfile` is not a sizing anchor and is not resolved against today's code.

## Assessment Semantics

Workload pass requires all explicitly expected baseline and steady canaries to
have successful command exits and completed, positive, non-future stages; a
consistent final turnover result with every scheduled rollout completed and no
failures/backlog/outstanding work; the full explicitly recorded steady duration;
and terminal workload success. The result's scheduled count must also match
`inputs/session.json.scheduledRollouts`. If that field is absent, the builder uses
`ceil(applications * steadySeconds / 3600)` from explicit, nonconflicting input
counts, limited to the canonical 0..40000 applications and 1..7200 seconds.
An invalid or null explicit schedule cannot fall back to that calculation.
`turnover.intended` preserves the configured count, evidence references and the
formula when derived. A mismatch blocks pass even if `turnover-gate.exit` is zero.
Zero applications with zero intended and completed rollouts is legitimate when
the remaining workload evidence is complete.

Outcome JSON requires integer `exitCode` in 0..255 and boolean
`shutdownOrExportFailed`. An optional `workloadExitCode`, when present, must be an
integer in 0..255, not null, a string or a fractional number. Any typed decoding
error invalidates the workload outcome and emits a sourced inconclusive finding;
partially allocated zero-valued pointers are never treated as successful exits.
The capture flag is decoded separately and never inferred from the workload exit.

The producer contract for `evidence/outcome.json` is:

```json
{
  "exitCode": 1,
  "workloadExitCode": 0,
  "shutdownOrExportFailed": true
}
```

`workloadExitCode` must be the workload exit captured **before** shutdown/export
changes the combined `exitCode`, including any recovery-window failure. This
example permits workload pass with incomplete capture only when all required
workload evidence is independently present. A nonzero explicit workload exit or
any known failed workload command yields `fail`, regardless of the capture flag or
an otherwise successful outcome. Started commands without valid exit evidence
block pass. Conflicting combined exit/capture evidence cannot establish pass.
The parent owns adding this field to the script; assessment does not synthesize it.

Legacy artifacts without `workloadExitCode` can establish terminal workload success
only with combined exit zero and shutdown/export failure false. A legacy nonzero
combined exit with shutdown/export failure true remains workload `incomplete`
unless direct workload failure evidence establishes `fail`. Successful turnover
and recovery gates cannot remove this ambiguity. Completed canary and turnover
observations are retained as sourced observations and data points, not promoted to
overall pass. Missing or unsupported completion evidence yields `incomplete`,
never an empty-run pass.

Canary data-point names are `canary.{baseline,steady}.{ack,ready,cleanup}`, with
separate `median`, `max`, and `count` statistics. Durations are converted from
recorded nanoseconds to seconds; an even-sized median averages the two middle
values. Failed/incomplete cycles are excluded and that limitation is retained.
Each population has its observed stage window and original command/exit hashes.
`measurementMethod` comes from sample `measurementMode`; absent fields remain
`unspecified`, even though the current canary implementation uses watches. Mixed
methods are labeled `mixed` and are not suitable for direct latency comparison.

`turnover.{scheduled,completed,failed,notStarted}` are actual result counters, not
the requested period schedule. They count application rollouts, not pod changes,
HTTP requests, or APF receipts. `benchmark.{guestNodes,applications,pods}` are
explicit configuration counts, not observed inventory.

`turnover.intended` checks explicit scheduled counts against
`ceil(applications * steadySeconds / turnoverPeriodSeconds)` when population and
duration are available. Intent is read from recorded seed/turnover/session/case
inputs, run metadata and turnover result `spec` fields, not today's CLI defaults.
An absent or zero period resolves to the historical 3600-second default. Explicit
null, malformed, out-of-range or conflicting values leave workload incomplete;
neither an explicit count nor a successful gate overrides contradictory evidence.
Bounds are checked before arithmetic (40,000 apps, 7200 seconds, period 1..86400)
and schedules above 80,000 are invalid. Calculation provenance records the resolved
divisor and source hashes. Nominal rate and workload completion do not establish
HTTP throughput, APF correctness, capacity, or price provenance.

Metrics use `metrics.Analyze`, including filename, SHA256, response structure,
sample finiteness and evaluation-grid checks. Coverage totals include every
analyzed series, without chart sampling or a 50-series cutoff. Evaluation windows
and the union of observed sample windows are retained separately. Invalid queries
have error status and no reusable coverage counts; zero series in such an entry
means no usable analysis, not proof the original source contained no series.
Missing samples can reflect series birth/death as well as scrape gaps. Coverage
is not availability or proof of freshness: PromQL lookback and omitted/unseen
series require separate review. Generic per-series maxima are not summed, metric
names are not trusted as units, and no whole-HCP CPU or memory formula is guessed.

Recorder errors and gaps become `recorder.{watch,logs}.{errors,gaps}` count points.
For example, a supplied summary with 642 log errors and 216 log gaps preserves
those values and makes capture `incomplete` even if workload passed. A summary
with missing counters does not establish zeros. Archive size/SHA256 verifies byte
integrity only. A detached clean summary cannot establish archive linkage, first
successful poll, readiness before seeding, startup coverage, or whole-run coverage.
Consequently this initial builder never emits whole-run capture `complete`;
without negative evidence it remains `unknown`, including legacy validation runs.
Recorder metadata extraction, first-poll timeline analysis and verified archive
linkage are not implemented. Raw archives are never automatically opened as tar.

Qualification is always `not_assessed`. Findings and carry-forward items have
explicit scope and references. Retained observations require equivalent workload,
treatment and measurement methods before comparison. Capture repair recommendations
are `fix_harness`, not claims of a control-plane defect. Always collect a new
baseline when changing polling to watch-based measurement; do not reuse baseline
deadlines across that boundary. Nothing automatically applies configuration,
qualifies a size, infers safe memory from a minimum, or proposes an arbitrary next
size. Whole-HCP resource hypotheses require separately curated, unit-aware,
time-aligned evidence and reviewer-owned formulas.
