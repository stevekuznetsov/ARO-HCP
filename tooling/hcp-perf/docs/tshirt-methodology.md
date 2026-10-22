# Characterizing A Hosted Control Plane T-Shirt

## Purpose And Handoff

This is the end-to-end methodology and agent handoff from the small-size
experiments conducted September 9-10, 2026. Read it before preparing another
resource profile. It documents both the intended method and what actually ran;
it does not imply that all steps are automated or that the first size is a
production-qualified offering.

The question is: **what population and useful client activity can this resource
profile sustain, how does it degrade under excess demand, and what does the
observed/configured footprint cost?** A server that remains alive by rejecting
all useful work is not a successful workload result. Conversely, a slow completed
request or a deliberate 429 is not automatically a control-plane failure.

The completed characterization is [Small v1](cards/small-v1.md), with a
[machine-readable card](cards/small-v1.json). That card is the source for exact
values, case windows, image identities, and citations. Do not copy a number from
an intermediate chat or report without its scope. The
[doubled candidate](cards/double-small-plan.md) has been rendered and tested
offline, **not deployed or measured** at this handoff.

## Workspace And Evidence Locations

See [Storage And Recovery](storage-and-recovery.md) for the maintained tool/state
map, bootstrap cache defaults, campaign path contract, and manual reboot recovery.
The locations and automation table below preserve the original experiment handoff;
the newer [bounded campaign supervisor](../scripts/CAMPAIGN.md) now supplies guarded
multi-arm execution, but not environment provisioning or automatic load resume.

All implementation work must remain separate from the user's active checkouts.
The persistent worktrees used here are:

```text
/home/stevekuznetsov/code/Azure/ARO-HCP/src/github.com/Azure/ARO-HCP-perf-runner
  branch: skuznets/hcp-perf-runner

/home/stevekuznetsov/code/openshift/hypershift/src/github.com/openshift/hypershift-perf-sizing
  branch: skuznets/perf-sizing
```

These are handoff locations, not paths to hardcode into tooling. Confirm branch,
status, and existing work before editing. At this checkpoint, much of the new
tooling is still untracked/uncommitted and some card files are staged. An ordinary
clone does not contain all this work. Do not clean untracked files, reset the
worktrees, or assume a commit identifies all compiled source. Committing or
publishing is a separate explicit action; private evidence must not be staged.

Paths below are relative to the ARO worktree root:

| Location | What It Contains |
| --- | --- |
| `tooling/hcp-perf/` | Reusable CLI, tests, examples, scripts, and documentation |
| `docs/design/hcp-perf/` | Original architecture, HyperShift design, and deferred work; some status text is historical |
| `_artifacts/perf-single-node/` | Environment/customer preparation, custom-image builds, early failed attempts |
| `_artifacts/perf-three-node-transition/attempt-6/` | Three-node baseline, harness validation, recorder investigations, first cost calculations |
| `_artifacts/perf-resource-locked-scan/` | Density, turnover, direct API, APF comparison, guards, and supervisors |
| `_artifacts/perf-resource-locked-scan/apf-shares-comparison-20260910-1832/` | Latest matched 50/100-share comparison, raw chunks and offline reducer |
| `_artifacts/tshirt-cards/small-v1/evidence-index.json` | Private evidence inventory and integrity audit |

`_artifacts` is ignored and local. Its launchers are historical execution records,
not portable interfaces. Raw logs, kubeconfigs, tokens, and some metadata are
sensitive. Keep private directories/files private; publish only reviewed summaries.
Preserve evidence independently of the test environment before deleting PVCs or
resource groups. Local fsync and a surviving PVC are not an off-machine backup.

## What Is Automated Today

"Automated primitive" means a bounded command with defined inputs/results, not
an autonomous end-to-end experiment. See the [CLI README](../README.md) and
[case-script contract](../scripts/README.md) for current flags.

| Step | Current Automation | Remaining Agent/Operator Work |
| --- | --- | --- |
| Personal environment | Repository `make personal-dev-env` and scoped cleanup entrypoint | Select environment, inspect ownership/cache, diagnose deployment failures, maintain credentials |
| Customer prerequisites | Existing E2E Bicep modules; this run composed them in a private customer-only template | Select identities, networking, KMS, permissions, and prepare valid RP payloads |
| HCP/node pool lifecycle | `rp create-cluster`, `create-nodepool`, `get`, `wait`, `credentials`, `delete` | Subscription setup, payload selection, frontend connection, namespace discovery and comparison scope |
| Resource profile | `sizing resolve`, `sizing hypershift` | Select coefficients/floors; deploy compatible HO/CPO; verify CRD; guarded apply and rollback |
| Retention | `environment refresh`; exact-set gate in `run-case.sh` | Supply the complete, approved resource-group ID inventory |
| Metrics collection | `scrape generate/check`, `capture start/status/stop/cleanup` | Exporter setup, credential renewal, expected replica inventory, startup coverage, TLS troubleshooting |
| Kubernetes evidence | `record start/status/stop/health/export` | Select namespaces, raw-log consent, source coverage and collection overhead; inspect gaps |
| CPU competitors | `adversaries discover/start/status/stop` | Choose pressure policy, verify actual CPU pressure and drift, decide when to stop |
| Fresh application population | `seed render/run`, `seed identities/credentials` | Supply pinned kube-burner and application images; verify guest capacity and convergence |
| APF/canary setup | Create-only `workload setup/credentials` | Inspect effective policy/classification; remove old run-owned priority levels |
| Measured workload | `canary run`, `turnover run`, `api-load run` | Choose workload/rate, supervise related processes, interpret useful work versus shedding |
| Post-provisioning case | `scripts/run-case.sh` | Provide prepared HCP/captures/connections and primitive JSON files; external safety supervision |
| Population/rate campaign | `scan plan` is offline only | Sequence cases, retain/reseed population, compare outcomes; actual campaigns used private supervisors |
| Resource/safety guard | Private `baseline.py`, `density-step.py`, and later supervisors | Review and adapt these helpers; not yet a maintained canonical CLI service |
| Existing APF modification | Private guarded patch/restore supervisor | Snapshot full spec/UID, change one knob, verify effective seats, restore on every exit path |
| Local metrics copy | `metrics stream`, `metrics export`, `metrics backfill` | Start/supervise streams before load, cover all required families, validate integrity and gaps |
| Guest statistics | `scripts/record-guest-stats.sh` and freshness helper | Discover actual nodes, supervise one collector per node, renew credentials |
| Analysis/presentation | `analyze`, `case progress`, `assessment build`, `report render`, `cost estimate` | Select comparable windows; correlate audits; compute whole-HCP sums; review findings |
| T-shirt card | Versioned Markdown/JSON example in `docs/cards/` | Assemble and review the card and its citations; no automatic card publisher |
| Fixture cleanup | Private UID-checked cleanup scripts; RP deletion and explicit capture cleanup exist | No general guest-population/APF cleanup command yet; retain evidence separately |

The canonical case script does **not** create the HCP, start Prometheus, start
local streaming/port-forwards, continuously enforce the resource/safety contract,
run direct API pressure, patch/restore an existing APF policy, or advance a campaign.
It consumes primitive JSON files, not the output of `case plan` or `scan plan`.

## Scientific Contract

### Keep The Axes Separate

- **Resources:** per-container CPU requests, memory targets, hard safety limits,
  runtime settings, replicas and component inventory.
- **Population:** guest nodes, applications/objects, namespace distribution,
  supporting platform components and resident watches.
- **Activity:** rollout period, direct request mix/rate, concurrency, timeouts,
  connection reuse, and retry policy.
- **Admission:** KAS concurrency settings and APF classification, shares,
  borrowing, queueing, and effective seat allocations.
- **Environment:** management-node capacity/placement, competitors, storage,
  network, software versions and observation overhead.

Change one deliberate treatment dimension per comparison. Record unavoidable
differences rather than silently calling the comparison identical. Freeze the
script, executable, image digests, resolved inputs and their hashes before launch.
Source code may continue changing in parallel; the running case must not read it.

The measured baseline is a hypothesis, not Scripture. Prefer admission tuning for
API-serving dependencies before increasing their envelope. Ancillary services can
receive evidence-backed resource increases in a **new** candidate, followed by a
repeat at the same population/activity. Count the resulting replica-multiplied
cost. Do not merely relax probes when a component cannot make useful progress;
do not add resources when evidence identifies a defective probe instead.

### Report Independent Outcomes

| Axis | Interpretation |
| --- | --- |
| Execution | Did the harness run, finalize, and produce its terminal result? |
| Workload | Did scheduled application work actually complete, including its bounded recovery window? |
| Performance | Did completed operations exceed soft expectations? |
| Safety/stability | Were there new OOMs, restart storms, loss of quorum or protected progress, or unsafe nodes? |
| Capture | Are required identities/windows covered? What is missing, stale, truncated or unrecoverable? |
| Integrity | Do local files match their recorded hashes? |
| Economics | What configured/observed compute demand is being priced, under which capacity model? |

An assessed violation is feedback, not an unhandled harness exception. A valid
case can finish with degraded performance or incomplete capture. Preserve the
original process exit, workload exit, shutdown/export outcome, and supervisor
classification. Do not label all of these simply "failed" or turn them all into
"pass". A successful archive hash proves byte preservation, not complete logs.

### What The Canary Measures

The canary is a small, pre-created Deployment repeatedly scaled `0 -> 1 -> 0` by
a dedicated, **bounded protected**, non-exempt identity. Controllers, scheduler,
and kubelets retain their ordinary identities. The cycle checks fresh Pod UIDs,
exact ownership chains, desired/observed generation, readiness, and final removal.

Current measurement establishes client-side LIST/WATCH before mutation. Times are
client observations: watch delivery and scheduling still contribute, but there is
no five-second polling floor. ACK is the scale-write acknowledgement; Ready starts
at the same scale-up intent and **includes ACK**. Cleanup starts before its
independent LIST/WATCH setup and Deployment GET, and includes that setup, the
guarded scale-down write, and convergence. It is not just scale-down API latency.
Do not add ACK to Ready. HTTP-header timing is a different measurement again.

Warm up once, then measure five consecutive baselines. Soft expectations start at
three times the slowest baseline observation, capped by the hard bounds. A soft
overrun records actual completion and continues. Hard limits are 30 seconds for
ACK, 120 seconds for Ready and 120 seconds for cleanup; preflight has its own
30-second bound. Record skipped cadence slots without overlap or catch-up bursts.
Do not reuse baselines across changes in observation method or instrumentation.

A client timeout is a censored observation, not the request's completed latency
and not proof the mutation was rejected. In one historical case the client timed
out around 181ms, while a matching server audit recorded HTTP 200; cleanup
converged in about 5.6 seconds under continuing pressure. The earlier harness
incorrectly used the soft threshold as cancellation. That result remains
historical evidence, not a counterfactual pass under today's policy.

## Repeatable Procedure For Another Size

### 1. Preserve And Inventory Before Changing Anything

Read the preceding card, JSON citations, evidence audit, live units and resources.
Do not trust an old `HANDOFF.md` saying "running" over a later terminal outcome.
Confirm that clients, competitors and recorder writers are stopped before changing
the HCP. Capture/export can continue reading a completed immutable window.

Use the Red Hat Azure profile for **every** Azure connection and inherited
kubeconfig exec-auth process:

```bash
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
```

Inspect resource groups and caches before cleanup. This run found no prior personal
groups and no sibling step cache; the original checkout's cache described a
different environment and was left alone. Never delete by a broad prefix or remove
another checkout's cache to make a new run look clean.

Build/test from the preserved implementation, from the ARO repository root:

```bash
env GOWORK=off go -C tooling/hcp-perf test -race ./...
env GOWORK=off go -C tooling/hcp-perf vet ./...
```

Use a new output path for each build. Record the source snapshot, including dirty
and untracked source, and binary hash. A Git revision alone was insufficient here.

### 2. Resolve The Candidate And Check Scheduling Feasibility

Anchor the profile on round KAS CPU/memory requests. Independent ratios/floors
derive other controlled container requests. In the discovery configuration:

```text
Go target = 80% of memory request, unless explicitly overridden/disabled
memory safety limit = request times independently configured multiplier
CPU limit = absent
GOMAXPROCS = no profile override unless explicitly selected
```

An omitted GOMAXPROCS override preserves underlying manifest/runtime behavior;
inventory effective settings rather than assuming the environment variable was
removed or parallelism is unlimited.

Generalized projection is implemented in the isolated HyperShift worktree, not
assumed present in the pinned released image. It covers HyperShift-owned regular,
init and sidecar containers, including the direct etcd-recovery path. Downstream
operator-created workloads remain separately measured and are not automatically
tuned. Verify actual coverage; the fallback is not proof that every Pod in the
namespace uses it.

For the prepared literal doubled candidate, double anchors, floors and fallback
requests. Merely doubling the KAS anchor can leave floor-bound helpers unchanged.

```bash
"$HCP_PERF" sizing resolve --profile tooling/hcp-perf/examples/double-small-v1.json
"$HCP_PERF" sizing hypershift \
  --profile tooling/hcp-perf/examples/double-small-v1.json \
  --default-cpu-milli 20 --default-memory-mib 128
```

These commands render; they do not apply. The doubled renderer still uses class
name `small`. Identify candidates by their profile/content hashes, not that label.
Check each management node's effective requests, usable Pod/NIC capacity, rollout
surge and placement. Doubling a namespace total that fits in aggregate can still
make a single controller-heavy node unschedulable. Extra nodes or different SKUs
are environment changes to record, not evidence of a pure fixed-host comparison.

### 3. Deploy Compatible Images And The Owning Configuration

Reuse [personal environment setup](../../../docs/personal-dev.md). Choose HO and
CPO independently; build/push pinned images and retain OCI/source metadata. HO
performs CPO metadata reads with the HostedCluster pull secret. The first CPO in
the service ACR could be pulled by some cluster infrastructure but was inaccessible
to that metadata path. Mirroring it into the authorized OCP ACR fixed the actual
problem; kubelet permissions alone were not the solution.

The experimental CPO runtime used pinned UBI9 plus `xz` because its usual base was
inaccessible. Current-CPO versus 4.21 payload compatibility required version gates
for TLS flags and ingress image keys. Preserve the tested source/runtime details,
not only the sizing patch. Revalidate new payload versions instead of blindly
carrying those accommodations forward.

Verify the installed CRD supports `containerResourcePolicy`. Snapshot the owning
CSC, use guarded changes, and inspect HC/HCP annotations and actual Pod templates
after convergence. The bootstrap change ensures policy is selected before first
managed workload creation. Do not test constrained startup by booting generously
and shrinking afterward.

Personal Helm/e2e defaults select `e2e_minimal`, with tiny requests but no equivalent
hard/runtime constraints. Clear conflicting overrides deliberately. The RP's
experimental size tag accepts `Minimal`, not an arbitrary size; selecting actual
t-shirt overrides is a separate Admin/configuration operation. Do not patch a
reconciled Deployment and assume the setting will persist. Re-running deployment
pipelines can overwrite a manually selected personal CSC; check the owner again.

### 4. Provision The Guest And Establish A Platform Baseline

Use existing customer-infrastructure Bicep modules for networking, identities and
KMS, then the **personal RP API** for HCP and node pools. Do not use an ARM HCP
deployment as a substitute for the personal frontend. RP primitives require a
registered subscription and explicit resource/payload files; they do not silently
register or replace subscription features.

Persist accepted operation URLs immediately. If a client stops waiting, use
`rp wait` to observe the existing operation instead of repeating create. The first
poller omitted the legitimate `Provisioning` state; that was a harness bug, not a
failed server operation. A later installing cluster could not accept a changed CPO
through CS; it was deleted through the RP and recreated with correct inputs.

Prefer a fresh HCP to confirm a new size from startup. Reusing an HCP for short
diagnostic population/rate sweeps is allowed if clearly recorded: deleting apps
does not reset etcd revisions, caches, prior rollouts or APF demand history.
Delete HCPs through the RP before deleting their managed/customer resources.

Use **three guest workers** for this HA reference workload. One guest worker
could not satisfy ordinary platform anti-affinity; monitoring and registry failed
to converge. That was not a reason to change management-node count or reduce KAS
replicas. Three workers resolved those topology issues. The unrelated pre-existing
console/OIDC configuration issue remained an explicit scope exception.

Discover nodes, capacity, current platform Pods and release configuration at run
time. No node name, Pod UID, 4KiB allocatable difference or label count should be
mistaken for the immutable resource budget. Keep such changes as comparison metadata.

### 5. Make Evidence Collection Ready Before Load

Run Prometheus and recorder Jobs on existing infra pools, not the contended HCP
worker pool. Required infra labels/tolerations and persistent storage are encoded
by capture/record primitives. Lightweight node exporters run on the workers being
observed. Their resources and overhead count in the environment inventory.

Generate component scraping from actual ServiceMonitors/PodMonitors, materialize
only the required TLS/credential keys, and verify each expected replica. Keep TLS
verification on. Use explicit metadata for intentionally inactive components;
do not equate a missing autoscaler with a healthy active scrape.

Actual problems encountered included missing guest metrics RBAC, catalog/OLM
bearer authentication, unsupported monitor fields, and duplicate scheduler output
labels. `scrape check` verifies active pools but does not by itself prove every
expected replica exists. Count by discovered identities and paths, not a memorized
"43 targets". A scrape returning 403, 401 or timing out is capture evidence, not
automatically a customer API outage.

Start local `metrics stream` before load and verify completed, hash-checked chunks.
It blocks as a local worker, so supervise it independently. Start remote recording
and require readiness based on an actual complete baseline poll checkpoint, not
Job submission or Pending Pod state. Then verify timestamps establish that
recording preceded seeding. Readiness is not proof of lossless watches or logs.

Recorders use separate observation/log clients, bounded concurrency and periodic
disk flushing. Default log budgets are 20 QPS/40 burst, with up to 256 followers
in the reviewed configuration; flushing is normally once per second rather than
per log line. These fixed recorder policies are overhead, not a resource-free
observer. Pin a compatible remote image: rebuilding the local CLI alone does not
upgrade readiness probes or stream behavior in existing Jobs.

Keep continuous raw CPU/memory/APF/API/runtime metric families and labels, not just
percentiles or graphs. Guest statistics are a separate source from management
node metrics. Include source timestamps, target health, resource/placement
snapshots, phase markers, client events, audit IDs, watch/log lifecycle and gap
records. Explicitly consent to raw logs, which can contain secrets.

See [Upgrade Signal Evidence](upgrade-signals.md) for the bounded query bundle
and the distinction between sizing evidence and maintenance/provider diagnoses.

Before **each test**, refresh and verify the exact approved resource-group set:

```bash
"$HCP_PERF" environment refresh --input "$CONFIG/retention.json" --timeout 5m
```

This merges `createdAt` while preserving other tags. It is not a lease and cannot
cancel cleanup already underway. Also check credentials, TLS validity, recorder/
generator deadlines, port-forward lifetimes and disk headroom for setup through
final export. Observer renewal is not currently a fully maintained CLI workflow.

### 6. Validate The Harness Quickly

Use the canonical script with explicit short inputs before a long soak:

```bash
bash tooling/hcp-perf/scripts/run-case.sh --validation --settle-seconds 30 \
  --config-dir "$CONFIG" --artifacts-dir "$RUN"
```

Set matching `guestNodes:3` and `applications:3` in seed/setup/turnover inputs,
300s steady, 30s recovery, and the original 3600s turnover period. This yields
15 Pods, ten 30-second canary slots, and one scheduled rollout. Zero applications
is a separate valid case; it still needs a canary and full observation duration.
Node count no longer implies application count.

The script automates retention and preflight, recorder startup/readiness, seed and
convergence, identities/APF, canary warmup/five baselines, optional adversaries,
settling, workload/canaries, recovery and final export attempts. Supply separately
prepared captures, connections, local streams, and continuous safety/resource
guards. Add `--contended` for the declared pressure treatment.

Prove both successful execution and intentional failure handling. An expected
overrun must not kill the runner; an interrupted collector must produce incomplete
evidence; repeated TERM during finalization must not abandon exports. Confirm no
active generator remains after return. Do not spend two hours discovering a token,
selector, export-size, watch permission, or zero-application scheduling bug.

### 7. Establish Empty Then Increasing Population

At a fixed candidate, measure the empty three-worker platform before applications.
The reference population is five Pods per Deployment, one Service/ConfigMap/Secret
per application, and 20 applications per namespace. Runtime system Pods, canaries,
surge and terminating Pods are additional.

The small campaign screened 60, 150, 300, 450 and 600 application Pods for five
minutes each, with the three-app validation as a prior check. The nominal activity
was one full replacement per application per hour. The endpoint is **200 app
Pods/node on average**, not a Kubernetes limit or uniform placement guarantee.
The observed kubelet limit was 250 total Pods/node. Check per-node headroom: an
aggregate of 54 free slots once included a node with only four free slots.

CPU competitors discover the actual target Pod placements and all scheduler-
effective requests, including init semantics and overhead. Default admission fills
90% of free CPU-request headroom, then runs enough workers to remain runnable on
the host's CPU capacity. This is not a 90% utilization guarantee or exact HCP CPU
entitlement. Measure actual CPU service/pressure. Freeze the resolved competitors
through a matched comparison; placement drift must be reported, not silently
retuned during scoring.

Soft latency changes do not stop the scan. The private safety supervisor used new
OOMs, restart storms, sustained loss of all KAS/quorum, unsafe nodes, or prolonged
observation loss as bounded-stop conditions; individual NonReady observations
became recorded violations. Review that policy for each run. It is not built into
the canonical case script, and it does not yet aggregate storms across replacement
Pod UIDs. Finalization stops generators and preserves evidence; cleanup is separate.

### 8. Hold Population Fixed And Increase Activity

After the population screen, distinguish object-state cost from request pressure.
At 600 Pods, increase `turnoverPeriodSeconds` inversely: 3600, 1800, 900, 600, and
so on. Record the actual period, not an informal "2x". For 120 apps over five
minutes these periods schedule 10, 20, 40 and 60 rollouts. Each rollout still
replaces five Pods; the supporting object population remains approximately fixed.

Choose `maxOutstanding` separately and keep it fixed through an activity comparison.
The small work moved from one outstanding rollout during population screening to
16 for activity tests. If rollout execution or guest Pod slots exhaust that bound,
report generator/guest limitations instead of calling them APF capacity. Do not
supersede an unfinished rollout and count both intents as completed. Record intended,
admitted, acknowledged and completed times, retries, failures and final debt.

Rollouts were insufficient to probe API admission on their own. Use `api-load run`
for explicit bounded read pressure while retaining population, background churn
and the protected canary. The measured mix used six namespace identities, with
weight 2 for Deployment GET and weight 1 each for Pod and ReplicaSet LIST, limit
100, without following pagination. This is a specified synthetic workload, not
customer-observed representativeness.

Direct load is open-loop, with offered rate and max-in-flight independent. It
records skipped offers as `generatorLimited`. Preserve connection reuse/stickiness;
do not force even KAS distribution. Measure each replica and the whole HCP. Static
namespace credentials prevent ambient/admin/exec-auth fallback. There is no
application-level 429 retry loop, but idempotent transport replay can occur; count
logical operations and HTTP attempts separately.

### 9. Tune Admission At The Same Resources And Load

Verify classification from returned FlowSchema/priority-level UIDs against the
recorded APF inventory, and correlate rejections with server counters when available.
Observe actual current/nominal seats and borrowing, not just shares. APF controls
concurrency and estimated work, not QPS; LISTs can consume multiple seats.

The tested baseline used benchmark shares 50 with Reject, and canary shares 5
with a bounded queue. KAS inflight inputs were 150+50=200 per replica. Nominal
benchmark allocation was 33, but borrowing raised observed effective allocations
well above 100. The controlled 50-to-100 share change produced 56 nominal seats,
not a doubling of effective throughput. These seat values depend on the complete
share inventory and must be rediscovered for another size/environment.

At the chosen offered load, change only one admission knob: total KAS concurrency,
benchmark shares, or shallow buffering, for example. Preserve system/protected
progress. Snapshot UID/resourceVersion/full spec, apply a guarded change, wait for
effective configuration, then measure a full new arm. Restore the original policy
in finalization and record verification; historical campaigns also used a separate
systemd restore backstop. `workload setup` is create-only and is not this patch tool.

Track completed bodies and useful rollout progress, not only 200 headers or all-
outcome latency. An overall p95 can improve simply because fast rejections dominate.
Report successful GET/LIST latency separately. A client with all slots occupied
cannot establish an unrestricted server capacity ceiling.

### 10. Finalize, Analyze Offline, And Write The Card

Stop new demand, observe the configured recovery interval, then stop every load
generator and verify terminal state. Retain HCP/apps and evidence until separately
authorized cleanup. An accepted suspension request is not proof that Pods stopped.
Preserve both original and recovered outcomes if finalization itself failed.

Export recorder data even after a failed recording. Use `metrics backfill` with
small time windows when a full-window histogram query exceeds backend limits.
Backfill can recover retained source samples, not missing scrapes or lost log bytes.
Copy data before Prometheus retention expires; the tested server had one-day
retention and no enabled admin snapshot API.

```bash
"$HCP_PERF" case progress --input "$RUN"
"$HCP_PERF" analyze --input "$METRICS_EXPORT"
"$HCP_PERF" assessment build --input "$RUN" --metrics "$METRICS_EXPORT" \
  --recorder-summary "$RECORDER_SUMMARY" --output "$NEW_ASSESSMENT"
"$HCP_PERF" report render --input "$RUN" --metrics "$METRICS_EXPORT" \
  --recorder-summary "$RECORDER_SUMMARY" --assessment "$NEW_ASSESSMENT" \
  --output "$NEW_REPORT"
```

Repeat `--metrics` for explicit export roots. Reports are self-contained bounded
previews; raw files retain full labels/resolution. Imported assessments require
matching evidence. Archive extraction and audit/request correlation currently
require separate reviewed analysis, not an automatic trustworthy join.

For footprint calculations, deduplicate scrape copies by actual identity, sum
containers at common timestamps, then compute whole-HCP statistics. Never sum
independent per-container p95s or fill missing samples with zero. Keep per-replica
peaks. Distinguish memory request, total cgroup charge, RSS, working set, retained
heap and Go `Sys - HeapReleased`. Etcd file-backed memory is not all irreducible heap.
Count replica/init semantics correctly and separate experimental instrumentation.

For client rates, state the denominator and whether completions during drain are
included. The first comparison's approximately 16 successful QPS includes drain
completions divided by a 180-second offered window; HTTP attempts per elapsed time
is a different rate. Token expiry, body-read timeout and a 429 with `Retry-After`
exceeding a call deadline are distinct events. One rejected write is not a KAS crash.

Price configured demand and observed demand separately. A first resource-equivalent
estimate uses `VM price * max(CPU share, memory share)`, not their sum. Actual packed
cost also includes usable capacity, Pod/NIC limits, placement, failure reserves and
idle capacity. Keep region, SKU generation, price date, Linux/Windows and purchasing
model explicit. Small ran on D4s_v3 hosts; pricing it on D16s_v6/E16s_v6 is an
illustration, not a demonstrated production packing result. The target is
$0.125/HCP-hour, half the user-supplied $0.25 fee; do not distort a profile to claim it.

The final card must include:

- Versioned profile, fallback, source/image/probe identities, all-replica inventory
  and precisely scoped configured totals.
- Nodes, application and platform populations, namespace/object recipe, activity
  mix/rate, concurrency, timeouts, retry/connection behavior and actual APF policy.
- Achieved body-complete throughput, successful-operation latency, shedding,
  generator saturation, rollout completion/debt, canary degradation and recovery.
- CPU/memory/runtime observations with windows, statistics and source coverage;
  OOM/restart/quorum evidence separately from scrape health.
- Cost assumptions, baseline drift/reuse/rollout confounds, missing data, and what
  is a tested operating point versus a hypothesis or recommendation.
- Hashed evidence references and actionable carry-forward items with applicability
  conditions. Do not carry a fitted canary baseline into a different method/size.

## What The Small Campaign Actually Taught Us

This is a navigational summary; [the card](cards/small-v1.md) supplies exact values.

| Episode | Observation | Methodological Consequence |
| --- | --- | --- |
| Tiny e2e requests | KAS consumed far more than its 100Mi scheduling request | Requests alone were not a footprint boundary; measure real consumption |
| Initial constrained startups | Registry authorization, payload CLI/image-key compatibility, then catalog OOM | Separate setup defects from genuine resource failures; preserve every attempt |
| One guest worker | Platform anti-affinity prevented convergence | Use three guest workers for the HA reference baseline |
| Early long soak | Polling quantization, over-sensitive ACK cancellation, late capture, export-size failures | Validate the harness in minutes; use watches and separate soft/hard thresholds |
| Recorder investigations | Shared 5-QPS client limiter, per-record fsync, buffered shutdown | Isolate observation/log clients, batch durability with explicit bounds, record lifecycle/gaps |
| Catalog under CPU contention | Fast health RPCs, but one-second exec probe deadlines caused restarts | Probe-tolerance treatment at unchanged requests; do not assume APF fixes localhost probes |
| Population screens | 60 through 600 application Pods completed short functional windows | 600 is the selected reference ceiling, not an intrinsic maximum |
| Increased rollout rate | Slower/unfinished useful work without a demonstrated crash | Record scheduler/client concurrency limits and completion debt |
| Direct GET/LIST pressure | Majority 429s, about 15-16 body-complete QPS, protected canaries still completed | We reached admission pressure and poor UX, not proof of an OOM boundary |
| Benchmark shares 50 versus 100 | Roughly 16.03 versus 14.83 successful QPS; worse latency and one late rollout at 100 | More nominal shares did not help this sequential comparison; borrowing and CPU costs matter |
| Final audit | Thousands of files intact, but real APF/Go scrape gaps and some unrecoverable logs | A sourced experimental card is possible; clean production qualification is not |

## Improvements To Prioritize

| Priority | Improvement | Acceptance Evidence |
| --- | --- | --- |
| P0 | Promote artifact-only safety/resource guards and retained-population/direct-load supervisors | New agent can launch the same bounded comparison without editing Python paths, old IDs or expected counts |
| P0 | Unify discovery into a versioned experiment inventory | Nodes, HCP/Pod/owner IDs, replicas, credentials, monitors, queries, resource groups and outputs derive from minimal inputs, with reviewable resolved values |
| P0 | Own observer/token/forward lifetimes and readiness | No healthy-looking run begins with expiring credentials, Pending recording, stale nodes or a forward ending before export |
| P0 | Preserve exact retention gating in every campaign arm | Each arm verifies the approved ID set and fresh tags, not a hardcoded count or campaign-start-only refresh |
| P1 | Canonical UID-guarded cleanup and APF patch/restore primitives | Partial-start, interrupted-run and failed-restore tests; no leftover population or priority levels in the next comparison |
| P1 | Supervise local evidence and durable off-machine transfer | First chunks before load, bounded lag, explicit gaps, final drain and verified archive transfer survive client/session loss |
| P1 | Improve overloaded scrape fidelity and log continuity | Reduce five-second timeout gaps without weakening health claims; distinguish replayed log overlap, actual missing bytes and unavailable history |
| P1 | Better client/retry and generator-limit characterization | Intent age remains fixed through bounded backoff; rejected writes and observation retries do not masquerade as crashes; report achieved demand independently |
| P1 | Canonical comparable analysis and card generation | One versioned schema joins workload, stability, capture, cost and source provenance; no stale narrative or silent reconstruction of missing outcomes |
| P2 | Stronger experimental confirmation | Repeat/reverse APF arm order, extend promising windows, confirm a fresh HCP, then fault recovery at fixed resources |

Do not respond to an instrumentation failure with another long soak. Fix and test
the specific path offline, run a short observer or three-app validation, and only
then resume the interrupted experimental question. Run durable workers, expose
quick progress snapshots, and use waiting time for offline analysis. Never spend
hours synchronously waiting while the user cannot tell whether work is running.

## Next-Size Checklist

The prepared [Double Small plan](cards/double-small-plan.md) is the next candidate,
not a conclusion. Before applying it:

1. Preserve Small v1 and its private evidence inventory; identify any still-running
   collectors, borrowed fixtures and original APF settings.
2. Confirm complete transfer of this uncommitted implementation and both custom
   HyperShift changes if another checkout/agent will be used.
3. Render the doubled overrides **and** doubled fallback, verify the intended
   exact changes, and establish per-node scheduling/rollout feasibility.
4. Create a new baseline identity. Keep guest population, workload recipe and
   initial admission policy comparable; record necessary management placement or
   capacity changes. Do not compare only profile labels.
5. Validate startup and short capture/client execution. Establish fresh canary
   expectations; record violations without aborting completed work.
6. Repeat the population/activity sequence needed to characterize that size.
   First replay the small card's 600-Pod/50-QPS workload before attributing gains
   to resource size; tune APF in separate arms afterward.
7. Finalize, verify stop/restore, export, analyze and publish a new experimental
   card with explicit uncertainties. Do not overwrite the small card or infer a
   universal node limit from this one request/object recipe.
