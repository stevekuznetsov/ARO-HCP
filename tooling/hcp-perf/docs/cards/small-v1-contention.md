# Small v1 CPU Contention Evidence

`small-v1-contention.json` is the reviewed, portable numeric subset for the small
card's contention chart. It contains 126 native 5-second evaluation slots per
management worker, 378 points total, without downsampling. No Azure access or
live changes were used to produce it. The parent card and its citations are
unchanged. Raw evidence is not included in this portable dataset.

## Observed Result

The burners consumed real CPU, not just scheduled requests. Workers 2 and 3
rose from roughly 25% and 23% non-idle before startup to 100% median non-idle
during both API arms. Their CPU PSI rose from approximately 7.6% and 7.3% median
to approximately 86-89%. Worker 1 was already saturated before these burners
started. This is evidence of actual competition and a temporal association,
not a controlled causal estimate of the burners' contribution to all node load.

| Worker | Pre-start busy median | 50 shares busy median (valid/expected) | 100 shares busy median (valid/expected) | Burner cores median, 50 / 100 | CPU PSI median, 50 / 100 (valid/expected) |
| --- | ---: | ---: | ---: | ---: | --- |
| Worker 1 | 99.968% | 100% (27/36) | 99.994% (8/36) | 1.078 / 1.001 | 94.146% (7/36) / unknown (0/36) |
| Worker 2 | 24.932% | 100% (36/36) | 100% (36/36) | 2.734 / 2.772 | 89.458% (36/36) / 85.753% (34/36) |
| Worker 3 | 23.018% | 100% (36/36) | 100% (36/36) | 2.592 / 2.595 | 86.543% (26/36) / 88.705% (28/36) |

Each arm has 36/36 burner CPU samples on each node. At 50 shares, 34/36 busy
samples exceed 90% on Workers 2 and 3; the first samples still include pre-start
idle time in the trailing 1-minute rate. At 100 shares, 36/36 exceed 90% on
those two nodes. All valid Worker 1 busy samples exceed 90%, but missing slots
must not be counted as busy or idle.

The frozen deterministic requests are 0.635, 1.856 and 1.613 CPU cores, with
four burn workers in each container. The planning input of 90% of free
*requested* headroom is neither 90% measured utilization nor a CPU usage limit.
Container rates aggregate the four burn workers; they are not four separate
per-process measurements. These are three four-core management workers hosting
the HCP, not the eight-core guest workers.

## Timeline

All times are UTC on 2026-09-10; machine-readable boundaries preserve source
precision. The plot starts at 18:32:45.850 and ends at its last evaluation at
18:43:10.850. The requested evidence window ends at 18:43:12.701458.

| Event | Time | Meaning |
| --- | --- | --- |
| Pre-start observations | 18:32:45.850 to 18:34:26.231263 | 21 busy slots per node; no measured burner CPU values |
| Start command invoked | 18:34:26.231263 | CLI action, not container startup |
| Frozen headroom snapshot | 18:34:26.847066912 | One discovery; requests held fixed across arms |
| All three containers started | 18:34:28 | `running.startedAt` in `cpu-frozen.json` |
| Start command returned | 18:34:28.238652 | Successful submission, not measured CPU utilization |
| First burner CPU rate | 18:34:45.850 / 18:34:50.850 | Workers 1/2, then Worker 3; earlier absence stays null |
| 50-shares API arm | 18:35:07.316991 to 18:38:07.317848 | Actual API active window, not earlier intent/case setup |
| 100-shares API arm | 18:39:10.831889 to 18:42:10.832792 | Actual API active window |
| Stop command invoked | 18:42:55.676465 | Does not imply instantaneous stop on all nodes |
| Retained container termination | 18:42:58 | Workers 1/3; Worker 2 exact termination timestamp absent |
| Stop command returned | 18:42:59.685170 | Stopped session, zero nonterminal Pods in response |
| Post-stop tail | 18:43:00.850 to 18:43:10.850 | Three slots, not settled CPU recovery |

There is no empty 30-second CPU baseline to turn into zeroes. The first seven
backfill chunks report an empty **adversary CPU** matrix, while their host CPU
queries return data. The campaign source's `quiet-baseline` runs five canaries
before startup; its explicit `pause(30)` is **after** competitors are ready.
The first resource guard record is at 18:33:47.916116, about 38 seconds before
the start command, and checks configuration mismatches, not CPU idleness.
Thus Workers 2/3 have a lower-utilization pre-start reference, but there is no
idle whole-cluster control and no measured pre-start burner zero.

Only 13.016 seconds of the requested window follow the stop command's return.
CPU rates still include pre-stop consumption for up to 60 seconds. Workers
2/3 end at 87.009%/86.300% busy, while short-interval PSI ends at 6.227%/5.189%.
Those observations are retained, not extended into an invented recovered tail.

## Derivation

- `busyPercent = 100 * (1 - sum(per-core idle rate) / 4)`, from saved
  `rate(node_cpu_seconds_total[1m])`. All four cores must have finite valid idle
  rates. The divisor is actual capacity, not 3.86 allocatable cores or the sum
  of available modes. Non-idle includes iowait and steal; it is not solely
  useful work. Guest nodes and `guest`/`guest_nice` modes are excluded.
- `adversaryCores` is the saved 1-minute container CPU rate for the exact
  `cpu-burn` Pod from the frozen placement. No KAS, whole-namespace or guest CPU
  aggregate is substituted.
- `psiPercent` is 100 times the difference of saved
  `node_pressure_cpu_waiting_seconds_total` divided by its **source timestamp**
  difference, using paired `timestamp(...)` queries. It measures CPU waiting
  wall-time fraction, not core consumption; do not divide it by four. It is a
  short-interval derivative, not a saved Prometheus 1-minute PSI rate.
- PSI needs both adjacent evaluation slots, source timestamps increasing by
  at most 10 seconds, source age at most 10 seconds at each slot, and a counter
  increase between zero and elapsed source time. Missing, reused, stale,
  reset or invalid pairs yield null. No zero filling, clamping or bridging.
- Deduplicate exporter series by node/core/mode and burner series by
  node/Pod/container at each timestamp before aggregation. Identical repeats
  at chunk boundaries collapse; conflicting duplicates abort. This extraction
  found 5,205 identical repeated samples across the inspected inputs and no
  conflicting or nonfinite samples.
- Use the backfill's native grid throughout. Stream and backfill have different
  evaluation offsets; merging them would overcount nearly coincident slots.
  All selected stream inputs were inspected and verified, but are not plotted.

`review.json` uses observed-span raw-counter summaries, not this saved PromQL
rate calculation. Its 100% arm summaries must not be stretched across the
plot. In particular, it records no Worker 1 raw CPU/PSI samples in the treatment
arm, while the saved 1-minute CPU rate query retains eight early evaluation
slots using preceding scrapes, through 18:39:45.850. Those eight rates are
retained with this caveat, not represented as fresh treatment scrapes. All
Worker 1 treatment PSI points are null.

## Renderer Contract

The schema version is exactly `hcp-perf.contention/v1`.

```typescript
type Point = {
  timestamp: number; // Unix seconds, UTC; fractional seconds preserved
  busyPercent: number | null;
  adversaryCores: number | null;
  psiPercent: number | null;
};
type Series = {
  id: string; // worker-1, worker-2, worker-3
  label: string; // Worker 1, Worker 2, Worker 3
  nodeSuffix: string;
  sourceNode: string;
  capacityCores: number;
  adversaryRequestCores: number;
  burnWorkers: number;
  points: Point[];
};
```

`series[]` contains the three series above. `units` declares percent versus CPU
cores explicitly. `window` supplies the requested bounds, duration, step and
126 expected points per node. `boundaries[]` supplies `{id, timestamp, meaning,
sourceFile}` events. Display actual node names only as provenance; chart labels
are Worker 1/2/3. The node names are identifiers, not credentials.

`summary[]` has `{id, start, end, durationSeconds, nodes[]}` for `pre-start`,
`burners-running-before-api`, `shares50`, `shares100`, `post-stop-command` and
`timeline`. Each node has `id` and summaries named `busyPercent`,
`adversaryCores`, `psiPercent`. Each summary has `sampleCount`,
`expectedSampleCount`, `coverageFraction`, `min`, `max`, `median`,
`firstTimestamp` and `lastTimestamp`; busy additionally has
`fractionAbove90Percent`. Fractions are 0-1, not percentages. Statistics use
only finite, non-null samples in half-open windows `[start,end)`; empty
statistics are null. Median averages the two central values for even counts.
Coverage counts evaluation slots, not independent scrapes or measured seconds.

Break each metric's line at nulls. Never connect across missing slots, shade
all nodes as continuously above 90%, or label this as 900 seconds of 95%
coverage. Use separate scales for percent and CPU cores. Boundaries describe
actions and API windows; they do not turn unknown samples into measurements.

## Provenance

All paths in `sourceEvidence` are relative to the ARO-HCP worktree root.
The run is
`_artifacts/perf-resource-locked-scan/apf-shares-comparison-20260910-1832`.
The JSON retains exact queries, windows, 5-second steps and 60-second CPU rate
windows. The 42 `metricManifests` references each contain a path and SHA-256;
each referenced chunk manifest maps query names to raw response filenames and
SHA-256 hashes. This is a transitive file-byte hash chain, not a claim of data
completeness. No raw addresses, Pod/container IDs, logs, archives or credentials
are copied into the portable evidence.

The offline extraction recomputed all 236 selected metric hashes across 42
backfill and 17 stream chunks. All 59 chunk manifest hashes match the existing
card-cited `review.json` manifest inventory. The review, frozen plan and intent
hashes also match the unchanged parent card's citations. `sourceEvidence`
includes the action records and saved campaign source for timing forensics;
that source was read, never executed. The generated numeric candidate was
reviewed before publication, with an independent recheck of the final numeric
values and summaries against raw inputs. No campaign reducer, live finalizer
or automatic parent-card regeneration was run.
