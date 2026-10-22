# Small v1 Client Experience Method

`small-v1-experience.json` is the reviewed, offline data companion for the retained
`apf-shares-comparison-20260910-1832` comparison. It does not qualify a capacity
envelope or establish a causal benefit from changing APF shares. Only this JSON
and this method document are deliverables; no renderer or executable source is
changed. All evidence paths are relative to the persistent perf-runner worktree.

## Evidence And Scope

The source root is
`_artifacts/perf-resource-locked-scan/apf-shares-comparison-20260910-1832`.
`sources` contains 45 unique, case-sensitive IDs, repository-relative file names,
and SHA-256 digests of the complete original bytes, including whitespace:

- `comparison-intent`: `intent.json`, configured demand and comparison bounds.
- `baseline-expectations`: `baseline/expectations.json`, soft fitted expectations.
- `common-window`: `backfill-queries/manifest.json`, shared plot extent only.
- `<arm>-requests`: `<arm>/api/requests.ndjson`, complete logical request records.
- `<arm>-result`: `<arm>/api/result.json`, final counters and timing.
- `<arm>-stdout`: `<arm>/api.json`, independently retained identical result.
- `<arm>-spec`: `<arm>/api/spec.json`, identical to the result's normalized spec.
- `<group>-canary-<index>` and `<group>-canary-<index>-exit`: each serialized
  canary result and its exit status, where group is `baseline`, `shares50`, or
  `shares100`; indices are 0-4 for baseline and 0-5 for each arm.

No Azure, Kubernetes, Prometheus, or other live requests were made. No artifact
from another run or worktree supplies plotted client data. No URLs, namespaces,
object UIDs, audit IDs, tokens, request paths, or bodies are embedded in the card.
Source file paths remain necessary provenance. Hash agreement establishes byte
integrity, not independent correctness or completeness of a live measurement.

## Time And Bins

Numeric timestamps are Unix seconds. The shared `window` is
`[1789065165.85, 1789065792.701458]`, matching the existing resource/contention
cards. Its start is the manifest's evaluation-grid origin truncated to
milliseconds, not the earliest canary. All 17 canaries and both API drains fit
inside this extent. Do not infer client observations in the gaps between arms.

For each arm, `start`, `stop`, and `drainEnd` come from the API result's `started`,
`schedulingStopped`, and `finished`, respectively. Exact original strings are
also retained as `startedRFC3339`, `schedulingStoppedRFC3339`, and
`finishedRFC3339`. API finalization is not turnover completion or command exit.

Build boundaries in integer nanoseconds: start at `result.started`, advance
10 seconds, and terminate exactly at `result.finished`. Bins are contiguous
`[start,end)`; the final bin includes its upper endpoint. There are 19 bins per
arm, not 18 bins with all draining completions forced into the active window.

| Arm | Active Start UTC | Scheduling Stop UTC | API Drain End UTC | Final Bin Width |
| --- | --- | --- | --- | --- |
| shares50 | 18:35:07.316991621 | 18:38:07.317848988 | 18:38:11.769599787 | 4.452608166 s |
| shares100 | 18:39:10.831889763 | 18:42:10.832792269 | 18:42:13.622088219 | 2.790198456 s |

All dates in this table are 2026-09-10. The last bin begins 180 seconds after
start and contains a sub-millisecond active scheduling tail before `stop`, then
drain. Preserve the exact stop marker rather than labelling that whole bin as
pure drain. The later turnover completion near 18:42:40 is not API `drainEnd`.

The selected NDJSON is `apiload.Event`, not the canary's header-only
`requesttrace.Observation`. Every event has explicit `started`, `finished`, and
body-inclusive `latencyNanoseconds`. Group completions by **`finished`**, including
terminal body/transport errors. Do not use `scheduled`, `started`, header timing,
or NDJSON line order for completion association. A legacy observation without
`finished` would require its own verified duration semantics before reconstructing
`started + duration`; no such fallback is used for this pair. `finished` is
captured immediately before the monotonic latency measurement, so those two
representations need not agree to the nanosecond.

Count `dispatched` separately by each event's **`started`** timestamp. It denotes
observed execution starts, not worker-queue admission or wire transmissions.
Exactly one unique `sequence` exists per final dispatched logical request.

## Counts And Rates

Every completion bin uses all terminal logical records as its denominator,
including records without a received HTTP status. A record contributes once to
`completed`, even when its `httpAttempts` is greater than one.

- `successful`: HTTP 2xx **and** absent/empty `errClass`. HTTP 200 with a body-read
  timeout is not successful.
- `http429`: `statusCode == 429`, independently of `errClass`.
- `timeouts`: `errClass == "timeout"`, independently of HTTP status.
- `otherErrors`: neither successful, HTTP 429, nor timeout. This is a residual
  record count, not a sum of possibly overlapping error counters.
- `rejectedFraction`: `http429 / completed`; null when there are no completions.
  This is a fraction in [0,1], not a percent, and not `429 / HTTP attempts` or a
  header-only denominator. Multiply by 100 only for percentage presentation.
- `successfulQPS`: successful completions divided by the actual bin width.
- `completionQPS`: all terminal completions divided by the actual bin width.

HTTP 429 and timeout flags can overlap. Do not stack them as disjoint categories.
`totals.http429TimeoutOverlap` records the overlap explicitly; it is zero in both
arms here. Empty measured counts/rates are zero; undefined fractions and latency
quantiles are null, never zero or NaN.

`totals.offered` and `totals.generatorLimited` are final scheduling counters.
`bins[].offered` is deliberately **null in every bin**. There is no independently
observed per-bin offered/drop stream; the retained admitted sequence IDs are not
used to invent one. `offeredQPS` is the configured 50 QPS, not actual delivered
load. `targetOfferedQPS` contains two exact segments `{start,end,qps}` per arm:
50 during `[start,stop)`, then 0 during `[stop,drainEnd]`. Render this as
**Target Offered QPS**, never as measured offered or successful traffic. Do not
derive integer per-bin offers from rate times width or distribute missed ticks.

Whole-arm `totals.successfulQPS` and `totals.completionQPS` use the entire API
start-to-finish duration including drain. They are not averages of bin rates and
do not divide draining completions by 180 seconds. Per-bin rates use their own
width, calculated before converting nanoseconds to binary64 Unix timestamps.

| Counter | shares50 | shares100 |
| --- | ---: | ---: |
| Offered | 9000 | 9000 |
| Dispatched / completed | 8369 | 8095 |
| Successful | 2885 | 2670 |
| HTTP 429 | 5447 | 5346 |
| Timeouts | 37 | 79 |
| HTTP 200 timeouts, excluded from success | 37 | 78 |
| Other errors | 0 | 0 |
| HTTP 429 + timeout overlap | 0 | 0 |
| Generator limited | 631 | 905 |
| Wire HTTP attempts | 8539 | 8510 |
| Rejected fraction | 0.6508543434102043 | 0.6604076590487955 |

The remaining shares100 timeout has no HTTP status (`statusCode == 0`). Higher
HTTP attempt counts represent transport-level transmissions; they do not create
additional terminal logical records or change rejection denominators.

## Latency

Sort raw `latencyNanoseconds` within the completion bin. For percentile p and
sample count n, take the one-based order statistic `ceil(p*n/100)` and divide by
1e9. There is no histogram interpolation, rounding to milliseconds, smoothing,
or percentile-of-percentiles. An empty subset produces null.

- `latencyP50Seconds` and `latencyP95Seconds`: all terminal outcomes, including
  fast HTTP 429s and timeouts; these are not successful-user latency.
- `successLatencyP95Seconds`: successful body-complete responses only.
- `getSuccessP95Seconds` and `listSuccessP95Seconds`: successful records split
  using `spec.targets[event.target].verb`, not HTTP method (both use HTTP GET).

Label all-outcome latency explicitly and show successful latency, successful
throughput, and HTTP 429 fraction together. Faster rejection must not appear as
better successful user experience. The final partial bins have smaller samples
and should not be presented as equally precise estimates. Final result latency
histograms are not the source of plotted per-bin quantiles.

## Canaries

`arms[].canaries` holds six steady cycles per arm. Root `canaries` holds the five
shared baseline cycles **once**, with `armID: "baseline"`. Do not copy these into
each arm or concatenate them twice in the renderer. Every cycle preserves
`measurementMode: "watch"`. Canary `started` is preflight start; plotted stage
starts come from each actual stage, not the cadence schedule or command marker.

`stages.ack`, `stages.ready`, and `stages.cleanup` retain source start, completed,
and monotonic duration independently. Ready and Ack share their start: **Ready
includes Ack**, so never sum Ack + Ready to describe cycle duration. Preflight
is not included in Ack/Ready/Cleanup duration. Numeric seconds support plotting;
`startedRFC3339`, `completedRFC3339`, and integer `durationNanoseconds` preserve
the exact source values despite binary64 timestamp precision. Do not recompute
duration from rounded plotting endpoints. Source zero completion timestamps
would become null, not apparent successful endpoints.

`expectedSeconds` is a soft performance expectation, first from
`expectedDurationNanoseconds` in that stage. The retained baseline expectations
file is the documented steady-only fallback, not a hard deadline. All 12 steady
results actually contain matching expectations: Ack 0.286871079 seconds, Ready
5.048170794 seconds, Cleanup 9.235945074 seconds. All five baseline cycles lack
expectations and are `not_evaluated`; their `expectedSeconds` stays null rather
than retroactively assessing the calibration samples against their fitted target.

`hardDeadlineSeconds` comes from actual `deadlineNanoseconds`: Ack 30 seconds,
Ready 120 seconds, Cleanup 120 seconds for all 17 cycles. These agree with the
retained intent, so no CLI default inference or legacy baseline-budget fallback
is needed. Missing hard deadline evidence must remain null. Soft threshold
exceedance is degradation, not completion failure. Canary `status` is `completed`
only when all three stages completed and command exit is zero; a nonzero exit
means `failed`, and incomplete stages with a zero exit would be `unknown`.

## Renderer Contract

The schema identifier is exactly `hcp-perf.experience/v1`, with `cardID: "small-v1"`.
All proposed arm, bin, and stage plotting field names remain unchanged. These
are the explicit additions to the proposed shape:

- Root `window: {start,end}` and `canaries: [...]` for shared alignment/baselines.
- Arm `targetOfferedQPS: [{start,end,qps}]`, `sourceIDs: {requests,result,spec}`,
  `startedRFC3339`, `schedulingStoppedRFC3339`, and `finishedRFC3339`.
- Canary `armID` and `measurementMode`.
- Stage `startedRFC3339`, `completedRFC3339`, and `durationNanoseconds`.

`method.status` is `reviewed`. `totals` contains `offered`, `dispatched`,
`completed`, `successful`, `http429`, `timeouts`, `otherErrors`,
`generatorLimited`, `httpAttempts`, `http429TimeoutOverlap`, `http200Timeouts`,
`rejectedFraction`, `successfulQPS`, and `completionQPS`. Source final
`rejected429` is deliberately mapped to the requested `http429` name. Each
canary's `id` is also the source ID of its result; append `-exit` for its exit
artifact. No case-insensitive or fuzzy source lookup is needed.

## Offline Verification

The card was regenerated from the retained files and verified against them, not
just checked for JSON syntax. The verification reread all 45 hashes, checked
identical stdout/final results and specs, unique sequence IDs, timestamps within
the API result extent, final and target request counts, wire attempt totals,
error classifications, canary exit statuses, exact phase values, and soft/hard
metadata. Each bin was recomputed from raw records and summed back to the final
dispatched/completed/outcome totals. All values are finite or explicit null;
the JSON is below 150,000 bytes.

To independently reproduce a bin, parse timestamps without dropping their
nanosecond fractions, select by `finished` in its half-open interval, apply the
predicates above, and divide counts by its exact nanosecond width / 1e9. Select
`started` separately for dispatch count. Sort each latency subset directly for
nearest-rank quantiles. For canaries, compare original timestamp strings and
integer durations, not differences between numeric endpoints. Validate every
source digest before comparing calculated values. Keep the retained artifacts
available: the public companion intentionally does not duplicate raw requests.
