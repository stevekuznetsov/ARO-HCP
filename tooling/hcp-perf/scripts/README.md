# Experimental Case Script

`run-case.sh` composes existing CLI primitives for **an already provisioned guest
with explicit node and application counts** and a known management-cluster HCP namespace. It is not an infra/RP
provisioner, campaign runner, optimizer, or qualification tool. It makes real
Kubernetes writes and refreshes Azure resource-group retention tags. There is no
live dry-run and no automatic retry/resume.

`run-case.sh` is the canonical retention-gated runner, promoted after case 4 and
the short validation exited. There is no compatibility wrapper. Do not overlap
cases or edit an executing script: Bash may still read it incrementally.
Start with `run-case.sh --validation` and reviewed short inputs before a long soak.
See [the live validation record](../docs/harness-validation.md#live-validation-record).

Run from the persistent `ARO-HCP-perf-runner` checkout. Build a trusted `HCP_PERF`
binary from the matching sources first. The parent CLI must dispatch `workload
setup` and `workload credentials` to `runWorkloadSetup`; the script checks this
before any mutation. The binary must also dispatch `environment refresh` and accept
canary `--ack-expected`, `--ready-expected`, and `--cleanup-expected`. Linux,
Bash, GNU coreutils (`timeout`, `date`, `sha256sum`, `sync`), `jq`, `kubectl`, and an already
authenticated Azure CLI (`az`) are required.

Build and publish a new recorder image from these matching sources and use its
digest in `record.json`. New Jobs execute `record health --input
/data/observations --require-ready` as a readiness probe; older images without
that flag remain unready. Rebuilding only the local CLI is insufficient.
Before seed, the recorder must be Running, nonterminating, container-ready and
have a Pod start time. The probe requires a complete checkpoint with successful
poll snapshots for every configured namespace/resource and no API errors.
This validates baseline poll sources, not watch/log initialization, future
container coverage, ongoing freshness or lossless recording. Pod start time is
not the recorder process start; use the manifest and checkpoint timestamps for
that audit. Existing submitted Jobs are not modified.

## Invocation

```bash
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
export HCP_PERF=/absolute/path/to/hcp-perf
export MGMT_KUBECONFIG=/absolute/path/to/management.kubeconfig
export GUEST_ADMIN_KUBECONFIG=/absolute/path/to/guest-admin.kubeconfig
export HCP_NAMESPACE=verified-control-plane-namespace
export CAPTURE_ENDPOINT=http://127.0.0.1:9091
export NODE_METRICS_ENDPOINT=http://127.0.0.1:9090
export CAPTURE_BUNDLE=/absolute/path/to/scrape/bundle.json
export SCRAPE_MAX_AGE_SECONDS=45

bash tooling/hcp-perf/scripts/run-case.sh --validation --settle-seconds 30 \
  --config-dir /absolute/path/to/reviewed-validation-inputs \
  --artifacts-dir /absolute/path/to/new-validation-run
# Add --contended only for an explicitly approved CPU-adversary run.
```

The artifact parent must exist and the run directory must not exist. Paths to
executables, kubeconfigs, the bundle and directories must be absolute. Azure
configuration defaults to `$HOME/.azure-redhat` only if unset; any explicit other
value (including empty) is rejected, not overwritten. The script does not log in,
change subscriptions, or invoke Azure provisioning. Credential plugins inherit
this exported Azure configuration. Never invoke with shell tracing enabled.

Keep the dedicated captures and any loopback port-forwards running throughout
execution and export. Both endpoints are Prometheus API base URLs, **not** raw
node-exporter `/metrics` URLs. They may be the same dedicated server. Optional
`HCP_PERF_BEARER_TOKEN` is consumed by the CLI and requires HTTPS. Endpoint URLs
must not contain credentials. Review endpoint-to-capture-ID mapping yourself.

The recommended collection workflow is to start a separately supervised local
`metrics stream` **before each case** and verify its first raw chunks before
launching the runner. Cover setup, baseline, steady, recovery and export, not only
the post-run window. Final script exports remain mandatory. Streaming and
`metrics backfill` use bounded windows and preserve raw responses and statuses;
they do not guarantee lossless collection. See
[Stream And Report](../README.md#stream-and-report) for commands and limits.

## Reviewed Inputs

### Short Harness Validation

Soak remains the default, including for old case JSON without `mode`. It requires
exactly 7200 steady seconds. Short runs require explicit opt-in and are harness
checks, not substitutes for the two-hour soak or evidence of sizing safety.
Do not start validation while another case is active.

The planner accepts these optional `experiment.Spec` JSON fields:

| Field | Soak | Validation |
| --- | --- | --- |
| `mode` | `"soak"` (default) | `"validation"` |
| `steadySeconds` | 7200 only (default) | 30..1800, default 300 |
| `canaryCadenceSeconds` | Default 300 | Default 30 |
| `recoverySeconds` | 1..600, default 600 | 1..120, default 30 |
| `turnoverPeriodSeconds` | 1..86400, default 3600 | Same; independent of duration |
| `deadlinesSeconds.settle` | Default 300 when omitted/zero | Same default; explicitly use 30 for a short run |

Cadence must be 5..steadySeconds. Zero-valued optional timing fields select their
defaults. Resolved mode and timings are persisted in the plan's `spec`. See
[`../examples/validation-case.json`](../examples/validation-case.json): three
nodes, zero applications, enabled canary, 300s steady, 30s cadence/recovery/settle.
`case plan --case examples/validation-case.json --profile examples/small.json`
is local planning only. `case schedule` uses the resolved turnover period and
includes offsets strictly below the selected duration. Shortening the window
alone does not accelerate churn. Schedules are capped at 80,000 rollouts.

The shell runner does **not** consume case JSON or require a new input file.
Use the existing reviewed primitive inputs plus explicit options:

```bash
bash tooling/hcp-perf/scripts/run-case.sh \
  --validation --settle-seconds 30 \
  --config-dir /absolute/path/to/reviewed-validation-inputs \
  --artifacts-dir /absolute/path/to/new-validation-run
```

In `turnover.json`, explicitly set `durationSeconds:300` and `recoverySeconds:30`;
the runner accepts 30..1800 and 1..120 respectively only with `--validation`.
Keep `guestNodes:3` and `applications:3` in seed/setup/turnover for the validated
minimal workload (15 application Pods); use zero apps only for a canary-only
check. All ordinary identity, retention, capture, readiness and export gates remain.
The shell's validation cadence defaults to `max(5,min(30,durationSeconds/5))`
(integer division); soak defaults to 300. Override with
`--canary-cadence-seconds N` (5..durationSeconds). `--settle-seconds N` is explicit,
defaults to 300 in either mode, and accepts 1..86400. The resolved settings and
expected counts are stored in hashed `inputs/session.json` and `evidence/run.json`;
`evidence/outcome.json` also records the mode and always retains `qualified:false`.

Five consecutive baselines and one unscored warmup remain unchanged, with the
same 30s/120s/120s hard budgets. Fast cycles can finish in about ten seconds each;
budgets are upper bounds, not sleeps. A five-minute steady window at 30s cadence
schedules ten steady canary slots. Slow completed cycles skip elapsed slots and
record incomplete coverage rather than aborting or catching up. Zero applications means no application lifecycle
load, but the canary still scales up/down throughout the full steady window.
Rollout offsets use the recorded period; only offsets before the window end
count, and every scheduled rollout must complete. Recovery is held even if empty
turnover completes it early.

### Client Activity Treatment

Set optional integer `turnoverPeriodSeconds` in `turnover.json` to change client
activity explicitly. Omission or zero resolves to 3600; otherwise use 1..86400.
There is no new shell period flag. Seed, setup and turnover must still agree on
guest nodes, application count and namespace prefix, regardless of period. The
600-application-Pod population ceiling does not itself authorize a rate change.
Keep the population, supporting-object recipe, seed, profile and APF configuration
fixed during an activity sweep. Choose `maxOutstanding` separately: initially 8,
held constant across period sweeps, not scaled with the requested rate.

The script records resolved `turnoverPeriodSeconds`, aggregate
`nominalRolloutsPerHour = applications * 3600 / period`, `maxOutstanding`, and
`scheduledRollouts = ceil(applications * durationSeconds / period)` in session/run
metadata. With 120 apps and 300s steady, periods 3600/1800/900/600/300 schedule
10/20/40/60/120 rollouts. These are application intents, not HTTP QPS or achieved
throughput. Five baselines and the expected canary count are unchanged.

Due work queues behind outstanding work; the same application's rollouts cannot
overlap. Keep scheduled, admitted/start, accepted and completion timestamps
separate. Only offsets inside the admission window exist in the schedule; do not
label whole future rounds as missed work. Unadmitted due intents remain debt,
not completed work; recovery admits no new work.

Every warmup, baseline and steady canary receives
`--request-log "$commands/$name.requests.jsonl"`; turnover receives
`--request-log "$commands/turnover.requests.jsonl"`. Each is a unique new private
file. HTTP records include available status, Audit-ID and APF response headers;
they do not verify identity/classification policy or measure client limiter waits
or response-body completion. Raw synchronous recording adds known client delay.
Do not remove that overhead silently between treatments.

For future activity runs, freeze and hash the newly reviewed CLI and canonical
script after the prior run has completed. Record the new harness version; do not
reuse the historical v6 patched script or rewrite historical raw/frozen artifacts.
No APF changes are part of this treatment. Cost/price source URL, timestamp,
region and SKU provenance remain in the separate cost specification, not inferred
from period, nominal rate, or workload success.

Validation recorder `durationSeconds` must be at least
`steady + recovery + settle + seed.timeoutSeconds + 1800` and at most 86400.
For the example with a 60s seed timeout this is 2220s, not a six-hour minimum.
The 1800s allowance includes baseline/warmup budgets and overhead, not mandatory
waiting or a guarantee covering every command timeout. Increase it for slower
setup or many namespaces. Optional validation adversaries require at least
`steady + recovery + settle + 120` seconds (480s for the example), at most 86400;
allow additional startup time when needed. Soak keeps the 21600s recorder and
10000s adversary minima. The turnover process bound is `steady + recovery + 120`;
credential expiry must exceed the remaining `steady + recovery + 300` seconds.
Short validation normally takes minutes, but command failure/export bounds can
take longer. It makes real writes and is not a live dry-run.

For a separately approved normal soak, omit `--validation` and explicitly set
`turnover.json` to `durationSeconds:7200`, normally `recoverySeconds:600`.
The runtime default remains soak, not short mode; settle/cadence default to 300s.

### Primitive Files

The config directory contains these JSON files. All IDs and namespaces are
explicit, not generated or adopted by the script. Reserve them exclusively for
this attempt; no other script/operator may create or write them concurrently.

| File | Contract |
| --- | --- |
| `retention.json` | Required for every new test, including zero-app and contended cases: `{"version":"v1","resourceGroups":["/subscriptions/UUID/resourceGroups/NAME", "..."]}`. Supply a reviewed, nonempty explicit list of full RG IDs covering the authorized environment/customer/managed-node groups that must survive. See `../examples/retention.json`; placeholders are not operational inputs. No subscription listing, prefix discovery, or automatic inclusion of shared resources. |
| `seed.json` | Existing seed spec, explicit integer `guestNodes` and `applications` (zero allowed), unique ID/prefix, trusted absolute `binary.path` and matching `expectedSHA256`, finite `timeoutSeconds` in 1..3600. Start from `../examples/seed.json`, not an unreviewed binary download. |
| `setup.json` | Workload setup spec: `version`, `id`, `namespacePrefix`, `guestNodes`, `applications`, `image`, optional `nodeSelector`. ID/prefix/image must match seed; explicit node and app counts must match all three specs. Current setup requires the reviewed pause digest in `internal/workload/setup.go`. Creates canary fixture/identity and APF policy even for zero apps. |
| `turnover.json` | Existing turnover spec, same prefix and explicit node/app counts, explicit schedule `seed`, `durationSeconds:7200`, `recoverySeconds:1..600` (use 600 for the sweep), `maxOutstanding:1..20`. Select the bound against guest Pod capacity. The script holds the recovery window if the CLI finishes early. |
| `record.json` | Existing management recorder spec with fresh ID, existing hcp-perf-managed namespace, approved image/storage/resources, `enableWatch:true`, `durationSeconds` at least 21600. `namespaces` must contain `HCP_NAMESPACE`; also include the known HostedCluster namespace as appropriate. |
| `captures.json` | Nonempty array of existing dedicated capture IDs, for example `[{"id":"my-full"},{"id":"my-nodes"}]`. All must report ready. The script never starts or stops these captures. |
| `queries.json` | Nonempty CLI query array for `CAPTURE_ENDPOINT`, each entry `{"name":"unique-name","expr":"reviewed PromQL"}`. Every query is required, including component/APF/container evidence appropriate to this server. Include `up` and `scrape-time` as described below. |
| `node-queries.json` | Same query format and required health queries for `NODE_METRICS_ENDPOINT`. Require node/container CPU counters and rates, memory, and source timestamps, with selectors covering relevant HCP workers and neighbors. |
| `adversaries.json` | Required only with `--contended`: existing adversary run spec with fresh ID, existing managed namespace, exact HCP `targetNamespace`, approved digest/resources/reserve, and `durationSeconds` in 10000..86400 to cover startup, settling, turnover and recovery. |
| `optional-queries.json`, `optional-node-queries.json` | Optional nonempty query arrays exported from the respective endpoints after the run, without preflight or success-manifest gates. Preserve missing/empty/error statuses as evidence gaps, never zero-fill. Command failures still fail the final outcome. Do not move required evidence here merely to pass preflight. |

The scrape bundle must be complete and explicitly include the HCP namespace.
`scrape check` checks expected jobs, not every replica. The script checks actual
short-window query manifests at both endpoints before seeding. Do not put a
query in the required set if that server cannot expose it, or hide a missing
required metric by removing the query. Review the set against real metric names.

Both required query files must designate `{"name":"up","expr":"up"}` and
`{"name":"scrape-time","expr":"timestamp(up)"}`. A scoped `up{...}` selector is
allowed, but `scrape-time` must wrap that identical selector in `timestamp(...)`.
Do not aggregate, filter out down targets, or use evaluation time in place of
source timestamps. Keep selectors broad enough to cover every expected target.
The existing CLI expected-job/target scrape checks remain mandatory.

Preflight and final export have a **capture-health gate**, not a scientific
completeness verdict. Both health queries must have `status:success`, so every
returned target series must cover the entire evaluation grid, with no ignored
gaps. Every raw `up` sample must equal 1, and both queries must return identical
target label sets. Every evaluation time minus its `timestamp(up)` value must be
nonnegative and strictly less than `SCRAPE_MAX_AGE_SECONDS` (default 45 seconds,
positive integer, recorded in `evidence/capture-health-policy.json`). This bounds
lookback-repeated source data; a 5-second query step is **not** a 5-second scrape
interval. Review the actual capture/bundle scrape intervals before selecting this
tolerance. It is not proof that no individual scrape was missed within it.

Other required families must be nonempty and contain valid finite, on-grid
samples, with no errors. Their `success` **or** `incomplete` statuses are allowed:
API/GVR label sets and cAdvisor Pods naturally appear/disappear within the window.
Requiring every such series to span the entire window incorrectly makes normal
birth/death an execution failure. Warnings and incomplete flags are preserved,
not cleared or promoted to success; hashes, raw responses and analyzer behavior
are unchanged. Errors, nonfinite values and absent required families still fail.
This permits recording for later analysis, not treating incomplete evidence as
complete or missing values as zeros. Optional export command errors still make
the outcome nonzero, and `qualified:false` is always retained.

The preflight HCP inventory retains actual Pod UIDs and requires every live Pod
to be Running/Ready (completed successful Pods are allowed). This snapshot and
the job-level scrape check do not prove every desired replica was discovered.
Use the parent capture/recorder inventory timeline to join actual Kubernetes UIDs
to expected targets and stable per-replica memory/Go series offline; a wholly
absent replica cannot be inferred from returned series alone.

For contention, `node-queries.json` must include `adversary-cpu-rate`, a reviewed
PromQL rate of CPU usage for **this run's** adversary containers. This one query
is excluded from pre-adversary checks; all other node queries are checked then
(at least one is required). The adversary query is checked after settling.
Its final export has a separate post-settle, pre-stop window under
`evidence/metrics-pressure`, rather than requiring adversary samples before the
Jobs existed. The other node queries cover the full observation window.
This is only a sample-availability check, **not automatic pressure validation**.
No threshold is guessed, no calibration occurs, and `qualified:false` is always
recorded. Review adversary-only series and measured pressure per worker offline.

## Sequence And Safety

Before each new test, the script copies `retention.json` into the new private
`inputs/` directory, makes that copy read-only (0400), and hashes it with the other
inputs. It invokes `run environment-refresh 6m "$HCP_PERF" environment refresh
--input "$inputs/retention.json" --timeout 5m` without `--dry-run` (default false).
Before any Kubernetes calls, recorder start, seed, canary setup or mutation, the
command must exit zero and its single JSON result must report `status:verified`,
`dryRun:false`, and every explicitly named RG attempted, updated and verified with
no failure or uncertain update. The returned RG set must exactly match the input
(case-insensitive, deduplicated). Missing, partial, planned, malformed or failed
results abort the run, without starting or stopping test resources. Inspect
`commands/environment-refresh.{json,stderr,exit}` and
`commands/environment-refresh-gate.*`; partial Azure tag updates are not rolled
back or retried. A previous test's successful refresh is never reused.

The script records the binary/input hashes, verifies the configured Ready guest nodes,
existing HCP Pods, capture readiness and scrape/query availability. It starts the
management recorder **before seed** and waits for it to run. Seeding has a finite
deadline; readiness gates require exactly `applications` five-replica Deployments
and `applications * 5` Running/Ready app Pods. Warmup visits `ceil(applications/20)`
namespaces, including a partially filled last namespace, and visits none for zero
apps. Empty label-selected Kubernetes Lists are valid only for zero-app gates;
missing or malformed Lists are not. Images must fit guest capacity and security policy.

Application Pods may not exceed `guestNodes * 200`. The private pre-seed node and
Pod snapshots and `commands/guest-capacity.json` retain total allocatable Pod slots,
assigned nonterminal baseline Pods, application Pods, one canary slot and
`maxOutstanding` rollout-surge slots. The aggregate must fit before seeding. This
does not prove per-node placement, IP, CPU or memory headroom; review those offline.

It creates benchmark identities, issues private per-namespace credentials, runs
workload setup, and issues separate canary credentials. Admin credentials are
never used for measured turnover or canary calls. A warmup canary cycle precedes
five consecutive successful baseline cycles. The small `jq` calculation uses
`min(3 * max(durationNanoseconds), cap)` for each stage, with 30s/120s/120s caps,
and writes `{ack,ready,cleanup}` integer nanoseconds to
`commands/baseline-expectations.json`. These are **soft expectations**, supplied as
`--ack-expected "$ack" --ready-expected "$ready" --cleanup-expected "$cleanup"`.
Every steady call independently retains `--ack-budget 30s --ready-budget 2m
--cleanup-budget 2m`; the fitted values never replace hard deadlines or shrink to
fit the cadence. No baseline CLI command is assumed. Historical
`commands/baseline-budgets.json` files remain untouched: progress/assessment readers
must prefer `baseline-expectations.json`, falling back to the old name only when
the new file is absent. Preserve historical semantics using the run's policy and
stage metadata, not today's defaults.

The session and run metadata declare `canaryPolicy:"observe-degradation"`, the
expectations file/flag references, and `hardDeadlineSeconds:{ack:30,ready:120,cleanup:120}`.
Preflight remains separately bounded at 30s; Ready includes ACK. Thus preflight,
Ready and cleanup sum to at most 270s, within the 5m CLI timeout. Each shell call
has a 6m outer timeout; independent cleanup can extend beyond CLI cancellation.
The existing 150s forced-kill grace remains a shutdown backstop, not a latency
expectation. Hard timeouts, logical/functional errors and failed cleanup still
fail the workload. A completed but degraded CLI result with exit 0 does not.

Optional adversaries start only after baseline. Status polling requires all Jobs
running with nonterminal Ready Pods, not merely successful submission. After
the configured settling interval, the script saves CPU rate evidence. It checks actual
credential expiry for the remaining run before starting two hours of turnover
with serial canaries on a five-minute start-to-start cadence. A hard-failed canary
terminates turnover, observes the configured bounded recovery with existing CPU
adversaries still running, then stops adversaries. It never starts load in a trap.
Default soak schedules 24 slots and requires `applications * 2` completed rollouts
and the full two-hour window, followed by the configured recovery window. Missing
canary slots or completed-late stages alone do not cause workload failure. They
remain degradation/coverage evidence, never a qualification pass.
Zero-app runs still configure the canary, execute warmup and all five baselines,
validate canary credential lifetime, and run the full observation window. Their
benchmark credential metadata must contain `files:[]`; no seed namespaces or
benchmark kubeconfigs are expected.

## Fixed Three-Node Sweep

[`../examples/sweep-three-node.json`](../examples/sweep-three-node.json) is a
review matrix, not an executable campaign configuration. Keep `guestNodes:3` in
every case and explicitly set `applications` to 0, 12, 30, 60, 90 and 120, yielding
0, 60, 150, 300, 450 and 600 application Pods. An external operator/orchestrator
runs cases sequentially with reviewed fresh inputs and isolated seed state; the
script does not search for a capacity limit, change node counts or launch a suite.
Because applications are retained, reusing a guest requires separately authorized
cleanup between cases or a fresh equivalent guest, not accumulating old apps.

The unseeded case is **platform plus enabled canary on three guest nodes**, not an
empty cluster or a zero-node control plane. Keeping nodes and the canary fixed
isolates the application-count change. Use 7200s steady plus 600s recovery for
every sweep point; keep other settings fixed and retain all private artifacts.
The former single-node script filename has been removed, with no compatibility
wrapper. Update external launchers to `run-case.sh`; historical runtime artifacts
are not rewritten or used as launchers.

## Shutdown And Artifacts

`evidence/outcome.json` is terminal feedback, not a capacity verdict. It preserves
`exitCode`, the original `workloadExitCode`, `shutdownOrExportFailed`, and all
existing cadence fields, and separates:

| Field | Meaning |
| --- | --- |
| `executionStatus` | `completed`, `aborted`, or `harness_error`. Completion is independent of whether expectations passed. |
| `experimentStatus` | `within_expectations`, `degraded`, `violations`, or `unknown`. Soft canary degradation does not stop the steady window. A terminal turnover gate violation does not shorten recovery. |
| `captureStatus` | `complete`, `incomplete`, or `unknown` for the runner's capture commands/gates, including optional export errors; not a claim of complete recording coverage. |
| `shutdownStatus` | `complete` or `incomplete` for attempted bounded shutdown. |
| `stopReason`, `failedLabel` | Fixed classification and original failed command label, retained across finalization commands. Unclassified failures remain harness errors. |
| `expectationViolations` | Independent degraded-canary, missed-slot, and after-window counts. Missing/unusable performance evidence is `null`, not zero. Safety remains `null`: external NonReady/safety monitor evidence has no canonical import contract. |

`evidence/failures.jsonl` records failed command labels, exit codes, phase,
classification and artifact references, never raw stderr. The expected negative
CLI dispatch probe is labeled `expected_probe`, not an experiment violation.
Malformed input is `harness_error` with reason `input_error`; failed preflight
viability gates are `aborted` with reason `preflight_not_viable`, never passed tests.

No exit-code override flag is needed. Preserve the script's nonzero status, then
run `hcp-perf assessment build` even when the case failed. A successfully assessed
violation is a valid terminal result to record and use for the next decision,
not an unhandled crash or automatically fatal campaign pipeline. The parent owns
progression and must still honor safety/review holds; a valid result is not
permission to increase load or evidence of valid capacity.

Finalization is armed before config/environment validation once a safe new artifact
directory and the JSON/sync dependencies exist. INT, TERM and HUP stop tracked
workload children; repeated signals during bounded finalization are recorded while
stop/export attempts continue. The outcome is written to a same-directory temporary
file, synced, renamed, then its parent filesystem is synced. Invalid invocation
without a safe artifact destination, SIGKILL, host/filesystem loss, or a supervisor
hard cap cannot guarantee an outcome or completed exports. Missing outcome means
unknown, not success. These changes apply only to newly frozen scripts.

Exit/signal handling terminates tracked local generators, suspends this attempt's
adversary Jobs, and checks shutdown. It stops the recorder and attempts export
even after failure, accepting terminal failed recorders as export candidates.
It fixes the metrics end time before export and leaves Prometheus running so
queries remain possible. Stop/export failures are retained and make the script
exit nonzero. There is no HCP, namespace, PVC, evidence, APF, fixture, or application
deletion. CLI stop primitives can terminate generator Pods; recorder export can
create and clean up its read-only export helper. This is not a no-mutation path.

Every wrapped command has stdout, stderr, start time and exit status under
`commands/`. The entire directory is private: seed output, raw diagnostics,
metrics and especially `private/` kubeconfigs can contain sensitive data. Inspect
`evidence/outcome.json`, `run.stderr`, and failed command artifacts. Do not publish
the directory wholesale. Partial artifacts are never reused on another attempt.

`evidence/canary-cadence.jsonl` is append-only and synced at each cycle start and
completion/skip batch. Events are `cycle-started`, `cycle-completed`, and
`missed-slot`; `slotIndex` is zero-based and `canaryNumber` is one-based.
Starts and missed slots include exact `scheduledAt`; missed slots also retain
`scheduledOffsetSeconds`, `observedAt`, `reason` and `afterWindow`. Every unattempted
slot is recorded once, including remaining slots when the run exits. After a
cycle, all slots due at or before completion are skipped; the next admission
waits for a future slot. No overlapping canaries or catch-up burst is allowed.
Command names use the slot number (`canary-steady-N`), so gaps are intentional.
A cycle admitted before the window end may finish afterward: its completion
event has `afterWindow:true`, `scored:false` and a `resultFile` reference retaining
late-satisfaction stage evidence and diagnostics. It is not a new after-window
attempt, and no new cycle starts at or after the steady end.

`evidence/outcome.json` records `expectedSteadyCanaries`, `attemptedSteadyCanaries`,
`missedSteadyCanaries`, and `completedAfterWindow`. Once steady starts, attempted
plus missed accounts for every scheduled slot. Misses, after-window completions
or execution/export failures set `observationCoverage:"incomplete"`; otherwise it
remains `"not_assessed"`, not proof of complete collection. Workload exit 0 can
coexist with late completion, fewer attempted canaries and `qualified:false`.
Parent progress/assessment consumers must read cadence events and soft stage
expectations separately from hard failures; qualification gates remain offline.

## Limitations

Before executing, verify authorization/ownership, guest Pod/IP/memory capacity,
digest provenance, storage capacity, token/certificate lifetimes, worker-wide
scrape freshness, actual sizing policy and absence of concurrent writers. This
post-provisioning script cannot reconstruct startup. A management recorder cannot
record guest namespaces: guest evidence here is CLI observations, seed snapshots
and canary/turnover results, not a guest watch/log stream.

APF object creation does not prove request classification. Running/Ready CPU
burners do not prove contention, and elapsed settling time does not prove
stability. Placement drift, missing replicas, CPU-pressure thresholds, memory
safety, cgroup budgets and achieved sizing require separate analysis. Even a zero
exit is only successful orchestration and workload completion, never a sizing or
capacity verdict. SIGKILL, host loss and expired cluster credentials can prevent
shutdown/export; finite job lifetimes are a backstop, not proof of cleanup.

See [the checkpointed personal workflow](../docs/personal-single-node.md) for
provisioning and infrastructure gates outside this script's scope.

## Local Guest Resource Recording

`record-guest-stats.sh` is a bounded personal-experiment fallback when guest
monitoring cannot schedule. It uses an existing, trusted guest administrator
kubeconfig to GET exactly `/api/v1/nodes/NAME/proxy/stats/summary`. It creates no
Kubernetes resources, identities, RBAC, or guest Pods. API-to-kubelet routing is
handled by the guest control plane; the local recorder does not need a route to
the worker's private IP. Verify that proxy path on the actual cluster first.

```bash
bash tooling/hcp-perf/scripts/record-guest-stats.sh \
  "$GUEST_ADMIN_KUBECONFIG" "$GUEST_NODE" "$NEW_ABSOLUTE_OUTPUT" 14400
jq -ne -f tooling/hcp-perf/scripts/check-guest-stats.jq \
  "$NEW_ABSOLUTE_OUTPUT/observations.jsonl"
```

Use a user systemd service for collection independent of the invoking terminal,
with `RuntimeMaxSec=14430`, `TimeoutStopSec=5`, `KillMode=control-group`, and
`UMask=0077` for a four-hour run. Pass an explicit trusted PATH and
`AZURE_CONFIG_DIR=$HOME/.azure-redhat` to that service. The script defaults to four
hours and accepts at most six hours. Credentials must remain valid for the run;
they are referenced, never copied. Any prerequisite API port-forward must also
remain available. Do not deploy these administrator credentials into Prometheus.

Every 15 seconds, one serial, timeout-bounded request records node, system
container, Pod, and container CPU/memory statistics and identities. Both local
request/response timestamps and kubelet source timestamps are retained. Missing
values stay null, not zero. Cumulative CPU is in core-nanoseconds; instantaneous
CPU is in nanocores; memory is in bytes. CPU rates derived offline must account
for counter resets and source time, not merely local polling time. This does
not provide all cAdvisor series (for example CPU throttling), guest logs, or
lossless container-lifecycle coverage.

The new directory is 0700 and files are 0600. `observations.jsonl` is appended and
synced after each request; failures contain only an exit code, never free-form
auth-plugin/server diagnostics. `manifest.json` contains PID, start time,
credential path reference and limits; `result.json` records termination. Output
is bounded to 1 GiB, with partial evidence retained. Stop only the specific
systemd unit, not other experiment processes. SIGKILL or host/storage loss can
leave partial evidence without a result; systemd supervision is not remote
storage or reboot persistence.

The streaming freshness check fails if the latest request failed, is 45 seconds
old, or any returned CPU/memory source timestamp is missing, future, or stale.
Use it before seed and at later phase boundaries, preserving its output and
exit status. It checks returned records, not absence of expected containers;
join Pod UIDs/container names against independent guest inventory and analyze
the full timeline for gaps. No existing runner gate is bypassed or automatically
extended by this standalone helper. `qualified:false` remains explicit.

Security: `get nodes/proxy`, even with `resourceNames` restricted to one worker,
is **not metrics-only or safely read-only authorization**. It can expose other
kubelet endpoints, including execution-capable paths. This helper deliberately
creates no persistent observer grant; use only the already authorized, trusted,
short-lived administrator context for the local experiment, not production
monitoring or measured workload identities.

# Guest Metrics Observer

`guest-metrics-identity.go` is an explicit operator helper for an authorized
personal environment, not part of the workload generator or automatic capture.
It reads only the HCP `admin-kubeconfig` and `metrics-client` Secrets, handles
credentials in memory, and reports reference names/authorization decisions only.
The default is diagnostic: guest `hypershift-metrics-client` binding presence and
the native client certificate's `/metrics` HTTP status.

Run from `tooling/hcp-perf`, with a verified personal management kubeconfig and a
loopback port-forward to the target HCP's `service/kube-apiserver` port 6443:

```bash
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n "$HCPNS" \
  port-forward --address=127.0.0.1 service/kube-apiserver 19443:6443
# In a separate terminal, diagnostics only:
GOWORK=off go run ./scripts/guest-metrics-identity.go \
  --kubeconfig "$MGMT_KUBECONFIG" --hcp-namespace "$HCPNS" \
  --server https://127.0.0.1:19443
```

Only after reviewing scope, repeat with `--provision`. This creates guest
namespace/ServiceAccount/ClusterRole/ClusterRoleBinding `hcp-perf-observer`, with
only GET `/metrics` and `/metrics/resources`. It requests a 12-hour TokenRequest,
checks positive metrics authorization and negative Secret-list/Pod-create access,
then verifies GET `/metrics` returns 200 with CA/server-name validation. Only then
does it create immutable management Secret `$HCPNS/hcp-perf-observer-token`, key
`token`. No admin credentials are copied or written locally. All creates are
create-only, with partial resources retained on failure; never blindly retry.
No native operator-owned RBAC or HCP workloads/budgets are changed.

The token Secret is an input to exact `endpointCredentials` entries, not a global
replacement. Preserve the emitted expiration separately and stop/export before
expiry. Capture does not monitor expiration or rotate snapshots. After stopping
all consumers and retaining evidence, an authorized operator should remove the
guest observer namespace/role/binding and management source Secret. Capture
cleanup removes session snapshots but does not delete that separate identity.
