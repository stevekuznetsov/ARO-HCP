# Native Performance Action

`iteration` accepts `kind: "performance"`, `recovery: "measurement"`,
`maxAttempts: 1`, and an inline `performance` object of type
[`performance.Spec`](../internal/performance/spec.go). It calls Go libraries
directly. There is no new CLI dispatcher, Python runner, SQLite schema, automatic
population search, HCP resize, node-pool creation, PVC deletion, or population
cleanup. The existing SHA-pinned kube-burner seed library remains the sole
workload subprocess. This implementation has offline tests, not live qualification.

## Parent Contract

- Supply a fresh seed ID and namespace prefix, explicit population and optional
  `apiload.Spec`. API duration must equal measurement duration. One-hour cases
  accept at most 277 QPS under the existing one-million-offer cap.
- Supply the reviewed full `startup.Spec` component table and exact ARM resource
  ID. Identity and namespace are discovered anew and checked for replacement.
  Do not construct namespaces or adopt a live resource snapshot as policy.
- Gate dispatch on completed startup and the explicit guest credential action
  when those actions are in the same iteration. For a previously completed
  startup, submit a fresh performance iteration only after reviewing its outcome.
  The action independently requires a currently clean, controller-converged
  snapshot; it never infers readiness from a dependency alone. Historical
  restart/OOM counters are baselined before a second fresh admission snapshot;
  failures between those reads or after admission are not ignored.
- The guest kubeconfig is an absolute external path with verified TLS and no exec
  plugin. It is used for privileged setup and TokenRequest only, never measured
  canary, turnover, or API traffic. Static scoped configs live only in memory;
  evidence contains namespace and actual token expiry, not tokens or authority.
  Before any mutation, `config.openshift.io/v1` `ClusterVersion/version` must have
  a valid `spec.clusterID` UUID equal to the exact discovered HCP clusterID.
  Missing, inaccessible, deleting, malformed or wrong-cluster identity fails
  closed before seed, RBAC, APF or TokenRequest.
- Supply an already-running external Go capture owner, reference, endpoint,
  exact `up` job replica counts and raw export queries. HTTPS is required except
  for loopback HTTP. Authentication/TLS transport configuration is caller-owned;
  iteration's default client does not load a separate metrics credential file.
  No metrics server or managed stream is started or stopped by this action.
- Capture sources must include every intended replica. By default, before seeding
  and during the guard, `up` must equal one and `timestamp(up)` must be fresh for identical
  replica labels including the exact expected `job`. Default freshness is 45s;
  the existing `maxAgeSeconds` accepts 15..60s, with zero/omission selecting 45s.
  An optional `jobTiming` map overrides only explicitly named source jobs, as
  described below. This checks source availability,
  not completeness of every metric family. Final range export preserves partial
  manifests independently of workload outcome.
- Supply `expectedGuestNodes`, a name-sorted list of exact `{"name":"...","uid":"..."}`
  identities for **all** guest nodes. It requires 3..64 unique names and unique,
  nonempty UIDs, with `seed.guestNodes` equal to the list length. There is no
  omitted/default inventory or discovery-based adoption. The 64-node bound limits
  reviewed inventory and per-poll work; it is not a capacity qualification or an
  instruction to provision 64 nodes. Missing, extra (even ineligible), duplicate
  or replaced nodes fail closed. Every node must be Ready, schedulable,
  non-deleting Linux, without NoSchedule/NoExecute taints or true/unknown
  memory/disk/PID pressure. Admission and the existing 10s safety loop check this
  inventory; that loop retains its shared 10s whole-cycle deadline.
- Admission subtracts every existing nonterminal Pod, including terminating Pods,
  from its actual node's Pod slots and CPU/memory requests. Kubernetes Pod request
  accounting includes init containers, restartable init containers and overhead.
  Unassigned Pods or Pods assigned to unknown nodes block admission. No label-based
  exclusion credits back fixture Pods. Fresh/restore admission additionally
  reserves five Pods/application; reuse charges existing Pods only, not a second
  steady population. Both reserve **seven** additional Pods per outstanding
  rollout (capped by application count), one canary, and two spare Pod slots/node.
  Turnover enforces `maxSurge=1`, `maxUnavailable=0`, but terminating old Pods can
  coexist with replacements. The steady population charge already covers five
  old replicas. The seven-slot reserve adds five replacements plus two policy
  margin slots, not a second charge for the old replicas or a claim that
  maxSurge is seven. Five replacements is the nominal worst-case additional
  occupancy for one healthy five-replica rollout with lingering old Pods;
  the extra two slots deliberately retain the previous reuse safety margin.
  Fresh admission now uses the same conservative reserve as reuse. Each node must
  fit the rounded-up balanced share of additional demand, using at least the
  canary's 5m CPU/8Mi memory requests. This is not a scheduler simulation or a bound
  on arbitrary controller churn. Turnover currently releases an outstanding slot
  when an intent fails, even if remote rollout debt remains; later intents can
  overlap that debt. The reservation therefore does not prove a hard bound on all
  unresolved remote Pods. No automatic 200-app/node ceiling is imposed.

### Growing Population Points

At a fixed reviewed 30 QPS, 60 applications/300 Pods, 120/600 and 240/1200 are
distinct fresh fixtures, not replica resizes or in-place population extensions.
After native `performance-cleanup` verifies removal of the previous fixture, the
parent separately arranges any needed workers and supplies their reviewed exact
identities for the next fresh case. This action never scales a node pool. Three,
six and twelve nodes are tested examples, not guaranteed sufficient capacity:
platform requests, spare slots and rollout concurrency still gate admission.

Five replicas/application stays fixed. Seed namespaces hold 20 applications each,
so these points use three, six and twelve application namespaces respectively.
An API mix restricted to the first three namespaces and GETs for `app-000000`,
`app-000020`, `app-000040` remains valid and intentionally stable; it need not
expand to sample every application namespace. Turnover still spans the declared
population with its existing hourly schedule. Existing-population reuse requires
unchanged seed, guest identity list, startup (except explicitly bound catalog
images below) and admission policies; it cannot
grow the population or silently adopt replacement nodes.

New submissions and direct performance execution require the explicit node
identity list, including three-node runs. Semantic validation still accepts an
omitted/null list only for the historical exact-three-node contract, allowing a
new controller to read/reconcile its journal and dispatch its original SHA-pinned
worker. Cleanup can validate an old source unchanged; it still checks the original
spec/outcome receipts and exact cleanup object UIDs. This does not admit a new
legacy measurement or permit the new worker to bypass node identity admission.
Frozen historical specs/outcomes and worker hashes are not rewritten or
automatically migrated. Do not replay historical measurements.

### Explicit Catalog Image Rebinding

A new run may reuse a retained population after reviewed catalog digest drift.
Set the new run's full `startup` profile to the reviewed target images and add
`imageBindings` to its `existingPopulation` reference. For example (illustrative
digests, not live approval):

```json
"existingPopulation": {
  "directory": "/absolute/prior/performance",
  "outcomeSHA256": "<sha256 of original outcome.json>",
  "inventorySHA256": "<sha256 of original population.json>",
  "imageBindings": [
    {
      "kind": "Deployment",
      "component": "community-operators-catalog",
      "container": "registry",
      "previousImage": "quay.io/example/community@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "image": "quay.io/example/community@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    },
    {
      "kind": "Deployment",
      "component": "community-operators-catalog",
      "container": "extract-content",
      "init": true,
      "previousImage": "quay.io/example/community@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "image": "quay.io/example/community@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    }
  ]
}
```

Only `certified-operators-catalog`, `community-operators-catalog`, and `redhat-operators-catalog`
Deployments are allowed, with regular `registry` or init `extract-content`.
Bind every changed container, not necessarily all three catalogs. Both old and
new images must be exact untagged `@sha256:` references. Missing, partial,
duplicate, unchanged, unknown, unused or wrong-previous bindings fail closed.
This is not observation's broader `retry-observation --image-bindings` API.

[`PopulationImageBinding` and `populationStartup`](../internal/performance/images.go)
apply only these replacements to a private in-memory source proof, then require
exact equality with the full target startup spec. KAS/release images, resources,
CPU, Go env, replicas, runtime/candidate, identity and probe policy cannot change.
The actual HCP/guest identities and target profile are still checked at admission
and during measurement; no live image is patched or silently adopted. Seed,
admission policy, node identities, population UIDs/content and pacing/critical
safety bounds are unchanged. Omission retains the original exact-match behavior.

[`populationPredecessor`](../internal/performance/population.go) checks each failed
recovery ancestor against that link's original spec and bindings, including older
complete ancestors used for rollout markers. A newer binding never propagates
backward. `recoverFailed` remains a separate explicit choice, not needed for a
completed execution with a failed assessment. Original receipts are never edited.
The new iteration's hash-pinned spec and performance `spec.json` persist bindings;
native cleanup continues using its original, unmodified source spec.

Require V7 or newer binding-aware binaries for **submission, the serving controller,
explicit reconciliation/finalization, and cleanup** of a rebound run. Before
submitting, verify the actual long-running `iteration serve` executable is V7 or
newer, not merely the CLI or worker binary. V5/V6 submission strictly rejects
unknown JSON fields (`cmd/iteration/options.go`), but journal `loadSpec` uses
permissive `json.Unmarshal`. Normal dispatch still selects the per-run SHA-pinned
worker; this does not make old-controller finalization binding-aware.

V5/V6 drop `imageBindings` from both the loaded run spec and the performance
receipt before comparing them. Equality can pass without checking bindings;
this is **not fail-closed rejection**. Using V7 for `iteration stop` alone does not
fix it: stop only persists cancellation intent, and an old serving controller can
perform the subsequent finalization readback. Exclude old reconcilers from rebound
runs. Do not replace historical workers or rewrite journals.
QPS doublings remain separate new, explicitly reviewed fixed-window specs under
the existing offer/concurrency/deadline bounds, not an automatic sweep.

## Per-Job Timing

The capture owner supplies exact job names from its prepared target inventory in
`capture.sources`. Timing keys must match that inventory, with no wildcards,
renaming, or fallback to an unrelated healthy backend. Runtime queries require
the expected replica count, actual `up=1`, and matching source timestamp labels.

For a job actually named `kas`, the approved 60s interval, 45s timeout and 165s
freshness policy is encoded as this optional fragment inside `capture`:

```json
{
  "maxAgeSeconds": 45,
  "jobTiming": {
    "kas": {
      "intervalNanoseconds": 60000000000,
      "timeoutNanoseconds": 45000000000,
      "maxAgeNanoseconds": 165000000000
    }
  }
}
```

Use the actual source job label instead of `kas` when different. Other jobs keep
their 45s freshness bound; their externally configured 5s interval/5s timeout is
unchanged. Omitting `jobTiming` preserves existing behavior. An explicit override
requires all three durations in whole seconds, interval 5..300s, timeout from 1s
through interval, and `interval+timeout <= maxAge <= 2*interval+timeout`, capped at
10 minutes. Thus KAS's 165s is accepted without permitting global freshness 165s
or a naked zero/disabled per-job check. The frozen spec retains these declarations
as evidence; this runner does not edit or independently read the external scrape
configuration. The capture owner must ensure declared and actual timings agree.

Freshness means within the declared sampling window, not lossless collection or
freshness of every metric family. Without the opt-in history policy below,
preflight and the continuous guard still reject a single `up=0`.

### Optional Capture History

For a **new case only**, enable bounded loss tolerance with both fields inside
`capture` (all existing job timing and exact source declarations remain required):

```json
{
  "lossGraceSeconds": 60,
  "recoveryGateSeconds": 180
}
```

Omitting both preserves strict behavior. Grace is 60..180s; the gate is 120..180s
and must cover grace and every job's MaxAge. For each target, the continuous loss
bound is `max(lossGraceSeconds, job MaxAge)`: e.g. scheduler 60s, OAS 80s when
explicitly configured with MaxAge 80s, and KAS 165s. Unchanged default jobs retain
45s admission freshness, with 60s sustained-loss tolerance. Initial capture gating
adds the declared gate duration to the total action bound; later gates share the
bounded setup deadline and do not extend a measurement.

The native Go tracker queries paired `up` and `timestamp(up)` range histories on
a 5s grid over twice the loss bound. Labels and evaluation times must pair exactly;
source timestamp **values**, not evaluation timestamps, establish success. Repeated
evaluations of one scrape count once. Admission before seed, baseline, competitors
and measurement requires all current targets up and two distinct fresh successes
after each target's latest observed failure. Failed scrapes reset admission
confirmation, never the last-success clock. Good scrapes between guard polls can
therefore prevent false loss alarms. A success after an already-expired gap cannot
erase that outage. Missing, additional or changed replica identities fail closed;
there is no automatic target adoption after the initial complete inventory bind.

During an already-admitted fixed measurement, brief scrape failures or query errors
are warnings while the bounded last-success lease remains valid. The existing
workload schedule continues; it is not paused, rescaled, extended or increased.
No new treatment/population/QPS stage is admitted while capture is unready. Sustained
loss cancels the run and preserves existing measurement uncertainty semantics.
Recorder errors, competitor CPU evidence checks and HCP safety checks are unchanged.

Every distinct failed scrape, observed recovery duration and query failure is
recorded in `events.ndjson` as `capture-history`. Outcome `captureHistory` includes
warning/recovery counts and a latched `incomplete` flag. Tolerated capture gaps do
not become workload violations or disappear after recovery; final export status
remains separate. A 5s query grid is bounded observation, not a promise to see every
short-lived scrape state or collect every metric. Existing BLOCKED cases and their
immutable outcomes must not be replayed or rewritten with this policy.

Large metrics responses and their collection consume HCP CPU and observer-server
resources. Keep the same capture workload/timing policy for baseline and measured
treatment at a given point, and record policy changes between points; observation
overhead is part of the experiment, not free capacity. No HCP probes, limits or
resource budgets are changed by this policy.

## Phases And Evidence

### Observe Degradation

For a new fixed-window diagnostic confirmation, set:

```json
"stabilityPolicy": "observe-degradation",
"requestQualityPolicy": {"maxDegradedBasisPoints": 500}
```

Also set `capture.criticalJobs`, enable capture history, and use a 5s startup safety
poll. Mandatory `capture.sourceRoles` binds five roles to five distinct exact jobs
in both `capture.sources` and `capture.criticalJobs`. For example, using illustrative
job names that must be replaced with the actual reviewed capture inventory:

```json
"sourceRoles": {
  "kas": "kube-apiserver",
  "etcd": "etcd-client",
  "container": "cadvisor",
  "node": "node-exporter",
  "kubelet": "kubelet"
}
```

All five roles are required, and omitting a job from the critical set is rejected.
These are operator-reviewed semantic bindings, not proof from a Prometheus job
name that the endpoint implements the asserted metric family. All source
families and queries remain present. Omitted policy or `strict` keeps historical
behavior. This mode rejects early screening configuration: the request quality
threshold is evaluated at the end, not used to truncate this confirmation.

Initial setup/baseline admission remains strict. After measurement starts,
restarts, OOMs, probe failures and ancillary unavailability are recorded rather
than individually terminating the window. Actual resource/image/identity drift,
incomplete Kubernetes inspection, unsafe control-plane node pressure/NotReady,
critical capture failure and unresolved canary cleanup remain stop conditions.
Fewer than two distinct live Ready KAS or etcd endpoint Pod UIDs for 30s stops the
run; ownership is checked through the actual controller UID chain. Critical checks
run independently on a 5s ticker, with concurrent KAS, etcd and authenticated guest
`/livez` branches sharing a 4s whole-cycle deadline. Unknown/timeout cycles do not
reset a known outage or renew the successful-evaluation heartbeat. An independent
watchdog cancels load after 15s without a successful complete evaluation. Decisions
cancel before potentially slow evidence publication. The outcome `critical` and
`critical-availability` events contain actual evaluation times, counts and unknown
samples: a configured 5s ticker is not a claim of lossless 5s observations.

Full profile, ancillary availability and node-safety inspection run separately
every 10s with a 10s whole-cycle context; capture runs in another worker and cannot
serialize the critical evaluation. API clients must honor request cancellation.
Filesystem/kernel stalls can still delay worker teardown; stale heartbeat causes
load cancellation independently of capture and evidence writes.
Etcd endpoint readiness is only a quorum proxy, not direct leader/member proof.

Measured canary deadline-only errors can continue only after independent scoped
read-only drain verification of the original Deployment UID, already scaled to
zero, with no owned Pods/ReplicaSet work or selector-matching unresolved Pods.
A Pod left behind after its ReplicaSet disappears or is replaced blocks recovery,
including terminal/deleting Pods; unknown ownership is never treated as absence.
The additional wait is at most 120s;
no scale retry, repair, admin fallback, or overlapping canary is performed.
Joined permission, identity, watch or non-deadline errors are not recoverable.
The failed result and actual timings remain immutable in the event journal;
`canaryFailures`/`canaryRecoveries` summarize them. Subsequent cycles remain serial,
with no catch-up. A drain longer than the remaining overall bound still stops.

Noncritical scrape outages or replacement labels are recorded as incomplete and
do not cancel measurement. Their frozen identity is not adopted or relabeled
healthy; repeated checks continue to expose the gap. Critical jobs remain strict.
The local recorder and competitor CPU checks are unchanged. Turnover intents
that merely fail to drain by their existing bound retain debt/results without
canceling independent API collection; unsafe authorization/ownership/evidence
errors are not waived. No failed intent is retried or called completed.

Outcome stability evidence includes sample/restart/OOM/probe counts and OAS and
packageserver endpoint minima/maxima and outage intervals. Independent `critical`
evidence contains KAS/etcd/livez counters and the successful heartbeat. Raw
per-poll safety events retain the component-specific issues. Recorded degradation
still violates final workload assessment even when execution completes its full
window. This mode measures brokenness; it does not turn it into qualification.
It does not alter HCP probes, images, resources or budgets. No existing case is
replayed or modified by opting a new case into this policy.

Final reporting separates `workload.healthStatus` and `workload.captureStatus`
from workload acceptance. Probe warnings, unknown critical samples and capture
incompleteness set `degraded:true`, `performanceStatus:degraded`, and human-readable
`warnings`. Capture-only gaps do not add workload violations or turn otherwise
accepted request/rollout results into failed workload assessment. This reporting
applies to newly finalized cases only, never rewriting historical outcomes.

The output directory is created exclusively, mode 0700, before any mutation.
Spec/status/outcome are mode 0600, atomically renamed and fsynced; events append
with fsync. Existing output is never reused. An absent outcome after process loss
means interrupted/unknown, not success. Iteration's measurement recovery blocks
replay; no debt restoration or inferred continuation is implemented.

The local namespace recorder must have a complete fresh checkpoint before seed.
The runner then seeds, configures identities/APF/canary, mints scoped credentials,
performs one warmup and five serial baselines, and fits soft canary expectations.
The hard 30s/120s/120s bounds remain independent. Measurement lasts 300 or 3600
seconds, with hourly turnover that is truncated rather than accelerated for short
cases. Canaries are serial with a configured delay between cycles. A cycle
admitted before the cutoff may finish during recovery; its actual timestamps are
retained, not clipped. Recovery admits no new turnover/API offers and lasts
30..600 seconds; a final bounded canary probe follows it.

The action timeout must exceed `MaxDuration() + 5m`. `MaxDuration` includes setup,
measurement, recovery, the optional capture gate, and 540 seconds for a straddling
canary and recovery probe.
Each library retains its own request/deadline bounds. Outcomes separate
`executionStatus`, the typed `workload` assessment, degraded canary performance,
recorder gaps, export gaps and unresolved competitor cleanup. `results.json`
retains turnover scheduled/completed/backlog/outstanding counters and API counters,
successful-body QPS, all-response latency and successful-body latency percentiles.
QPS uses elapsed time through body completion, including drain, not intended rate.

`ValidateResult` requires the complete configured admission/recovery window and
all scheduled turnover completed without debt. API errors, timeout/cancellation,
body errors, missed offers and zero successful response bodies violate workload
requirements even when the generator returns nil. HTTP 429 shedding is recorded
separately and permitted alongside useful successful responses; all-429 is
unusable, not a harness crash. Soft canary degradation is reported separately and
does not fail a completed cycle; hard canary deadlines remain mandatory.
A durable assessment after completed execution and verified competitor cleanup
returns a typed failure: iteration marks the action `FAILED`, not `BLOCKED`, so
completed dependencies cannot advance. Interrupted execution or uncertain cleanup
retains `BLOCKED` measurement behavior. No case retries or changes budgets.
Even `met-not-qualified` is not a capacity qualification or complete metric claim.

### Optional Request Quality

New API sweep cases can opt into an inclusive combined 429-or-timeout allowance
inside the performance spec (not inside `api`):

```json
"requestQualityPolicy": {
  "maxDegradedBasisPoints": 500
}
```

#### Early Screening

For a newly submitted long point, explicitly freeze cumulative rejection
checkpoints rather than waiting an hour for an already failing request stream:

```json
"requestQualityPolicy": {
  "maxDegradedBasisPoints": 500,
  "screeningSeconds": [300, 600],
  "minimumCompletedRequests": 100
}
```

The API scheduler freezes the logical requests offered/dispatched through each
checkpoint. The collector waits for **all dispatched requests in that cumulative
cohort** to settle under their existing request timeout, including body reads.
Later requests cannot dilute that cohort, and pending requests never count as
successes. Exactly 5% continues; greater than 5% rejects. Decisions retain separate
429/timeout counters and the event-derived union counter. Request events are
flushed/fsynced before the `request-quality-screen` decision is journaled.

Checkpoints are ordered unique seconds >=60 and strictly before the configured
measurement end, at most ten, with enough intended requests for the declared
minimum (100..1,000,000). Both screening fields must be omitted to disable early
screening. An insufficient or generator-limited cohort is `inconclusive`, never
a passing capacity point or a server-ceiling rejection. Non-permitted failures
are `unusable`; the existing final assessment still rejects them. A continued
screen says only `continue-not-qualified`, not that the complete point passed.

Rejection cancels API/turnover children, allows the current hard-bounded canary
cycle to finish, and enters the configured recovery window and final recovery
probe. Safety guards, competitor cleanup, metrics export and immutable evidence
still apply. `results.json` retains screening receipts plus partial generator
results; workload status and request-quality capacity status are `screened-out`,
with `fullWindow:false`. A settled deliberate rejection with verified finalization
becomes `FAILED`, not a successful shortened hour. Other safety, cleanup or evidence
failures retain uncertainty. No automatic retry, QPS increase or hour-long follow-up
is launched. Prefer explicit 3m/5m screening cases first; this optional mechanism
also stops a later long case that deteriorates at its declared checkpoints.

The denominator is **logical dispatched requests**, never offered requests or
transport attempts. The numerator counts a request once if HTTP status is 429 OR
its terminal error is a timeout, including body timeouts after HTTP 200/429. A 429
whose body times out is not counted twice. Separate 429 and timeout counters remain
visible and may overlap. Acceptance uses integer arithmetic:
`degradedRequests * 10000 <= dispatched * maxDegradedBasisPoints`. Exactly 5% is
accepted; 5.01% fails. The reporting fraction is not used for the decision.

`workload.requestQuality.status` is `met`, `degraded` (within the allowance),
`exceeded`, or `not-measured`. `capacityStatus` independently reports `acceptable`,
`exceeded`, `inconclusive`, or `unusable`. Generator-limited offers, unfinished
requests, incomplete windows and turnover debt do not pass as evidence of a server
ceiling. No successful bodies, invalid counters, missing versioned union accounting
or nonfinite throughput are unusable. Old artifacts are not retroactively assessed
by guessing overlaps. All unexpected HTTP responses (including 401/403/500),
non-timeout transport errors, cancellation and body-limit errors remain failures
even below the threshold. At least one useful successful response is required.

With this policy, 3% timeouts can be accepted and are reported without changing
the request deadline. 3% disjoint 429 plus 3% timeouts fails. Omission preserves
the historical strict timeout rule and uncapped 429 shedding with useful replies.
The policy requires API load and an integer threshold in [0,10000].

Latency is measurement, not an additional acceptance barrier: large completed
latencies and soft canary ratios do not fail the point. Hard canary deadlines,
protected progress, full windows, rollout completion and existing OOM/restart/
sustained-unavailability guards still apply. An accepted degraded point may finish
successfully; an assessed violation with verified cleanup becomes `FAILED` and
cannot satisfy a completed dependency. This does not automate a QPS search, retry,
or budget change. Parent selects each fresh 3m/5m/1h fixed point explicitly.

API evidence retains existing all-outcome latency fields for historical consumers.
Use `successfulLatencyP50Nanoseconds`, `successfulLatencyP95Nanoseconds`, and
`successfulLatencyP99Nanoseconds` for useful latency. `operations` reports the same
successful-body percentiles and QPS separately for Deployment GET, Pod LIST and
ReplicaSet LIST, pooling only identical API-version/resource/verb operations.
Fast rejections cannot improve these successful-body percentiles. QPS includes
drain time; the request-quality denominator remains a logical count, not a rate.

Unloaded same-fixture baselines are not added by this acceptance change. The
current population inventory requires five replicas per application (zero only
for the canary), and population hashes include desired content. Scaling existing
applications to zero will fail that contract. Setting applications to zero also
removes the allowed application GET targets. Supporting 60 retained Deployments
across three namespaces at zero replicas needs a separate explicit population/
turnover contract; do not bypass inventory or substitute different read fixtures.

The continuous guard reuses startup profile checks plus current-generation
controller convergence. Actual and desired resources must match frozen profiles.
New OOMs or three restarts/owner in 120 seconds (including replacement Pod UIDs)
stop load. Transient readiness/probe issues are recorded; two minutes of continuous
unhealthiness stops load. Recovery additionally requires current health.

Optional competitors require a reviewed sorted node/request plan, explicit
adversary bounds, and `cpuJob`, the exact external cAdvisor scrape job. Arbitrary
`cpuQueries` are no longer accepted: the runner constructs exact namespace/Pod/
`cpu-burn` counter selectors from the registered worker inventory.
They start only after baseline. Discovery and submission plans must match the
frozen allocations and target Pod placement. Registration and Job UIDs are pinned
from submission; Pod UIDs and container start times are pinned before measurement.
Every expected Job must have exactly one Running, Ready, non-deleting worker on
its frozen node. Each worker needs exactly one positive `rate(counter[1m])` and
a paired fresh `timestamp(counter)` source value with identical labels, not just
a fresh query evaluation timestamp. Duplicate series, zero CPU, stale counters,
replacement workers and partial readiness fail the gate. Existing managed
CPU competitors must already be suspended and drained, never adopted. Finally,
the runner cancels/joins local children, suspends its remote competitors, verifies
no nonterminal Pods remain, and attempts final export even on failure. Failed
verification explicitly warns that load may remain; it does not claim cleanup.
Stop retries conflicts/transient failures with fresh UID/resourceVersion-guarded
reads and continues other owned Jobs after an error. Finalization repeats Stop
within its cleanup budget, retaining identity pins and the last error. Missing
Jobs after partial submission remain unresolved; they are never created to make
cleanup pass. Export has a separate budget even after cleanup expires.

## Limits

### Failed Case Recovery And Cleanup

An explicit iteration stop/reconcile can read-only verify a finalized failed
performance case without competitors: frozen spec/outcome, exact HCP and guest
identity, and drained owned canary. It records a hash-pinned verification receipt
and marks the measurement failed without replaying it. Missing or incomplete
outcomes and cases with competitors remain blocked.

The separate `performance-cleanup` action accepts `performanceCleanup` with the
original `source` performance spec, absolute case `directory`, `outcomeSHA256`,
and exact `objects` (`kind`, `name`, `uid`). Only the complete derived namespace,
FlowSchema and PriorityLevelConfiguration inventory is accepted. Preflight checks
all UIDs/ownership and refuses PVCs or unexpected workloads/Pods. Deletes use
UID/resourceVersion preconditions and verify absence; no finalizers are removed.
It is a guarded mutation with one attempt, not automatic rollback or blind replay.

- No live run, cluster03-specific generated profile, or private runnable input is
  included. Parent supplies fresh reviewed inputs after startup stabilizes.
- Capture source checks do not validate collector target discovery, all raw
  metric families, TLS/source identity behind a loopback proxy, or losslessness.
  Local recording is polling only; raw log/watch options are not enabled here.
- Startup inventory matching and endpoint semantics remain those of its native
  library; this is not guest-operator qualification, authenticated readiness, or
  direct etcd membership/health validation.
- Positive competitor CPU proves activity, not saturation. Unmanaged/external
  writers are outside local exclusion.
- Offline tests cover admission/credentials, capture freshness, setup failure,
  safety classification, iteration validation and hourly schedule truncation.
  Tests also cover wrong-cluster Run preflight, per-worker readiness/freshness,
  partial submission, suspension conflicts, UID replacement, cleanup/export
  failure isolation, and unusable/degraded workload results. Variable guest tests
  cover 3/6/12 nodes, inventory bounds and identity replacement, per-node safety,
  Pod slots/requests/init/overhead and fresh/reuse/restore rollout reserves.
  Full successful
  one-hour orchestration and process-loss cleanup still require additional
  end-to-end validation. Existing
  workload libraries test their cancellation and soft/hard deadline behavior.

Run offline tests sequentially from `tooling/hcp-perf`, with `TMPDIR` set to a
persistent private directory (0700, short enough for Unix socket paths), not a
shared temporary directory used for credentials:

```sh
GOWORK=off go test -p 1 ./...
GOWORK=off go test -race -p 1 ./...
GOWORK=off go vet -p 1 ./...
```

## Screened-Out Population Reuse

`existingPopulation.screenedPopulation` is an explicit alternative to
`recoverFailed`, never a fallback for a bad pin. It contains `specSHA256` and
`resultsSHA256`; the reference's outcome and inventory pins must name the same
finalized run's own files. Only a loaded, quiet, retained population from a
completed execution in recovery with `status=screened-out`, `fullWindow=false`,
finalized zero-outstanding/zero-backlog turnover and a rejection receipt qualifies.
An interrupted/failed execution or missing inventory does not qualify.

This makes that complete inventory authoritative, just as for full-window reuse;
it does not validate or repair its historical reference chain. Exact live inventory
completeness, convergence, UIDs/content and zero canary Pods are required. Seed,
node identities, admission and startup checks (including explicit image bindings)
remain in force. The new spec and results pins prevent substituting policy or
finalization receipts. Historical evidence and workload assessments are unchanged.

Inventory lists and the subsequent canary Pod list are not an atomic cluster
snapshot or a lock against external writers. This read-only admission proof assumes
the coordinated sole-writer workflow; existing identity, capacity, scoped-identity,
canary and safety guards still apply. It never overwrites live objects to make
their content match the inventory and does not authorize concurrent mutation.

Require V8c for submission, the actual serving controller, workers and all explicit
reconciliation/finalization/cleanup operations. Exclude older reconcilers before
submitting a new run. Older strict submission rejects the new field; permissive
decoding can discard it on both sides of a later spec comparison. Do not assume
mixed-version finalization fails closed. Perform the separately authorized idle
V8c controller handover first, not just a CLI or worker upgrade. V8 is superseded;
its screened receipt checks were insufficient and it must not serve the new run.

V8b additionally checks same-run consistency: source API/policy, normalized API
spec and final counters, ordered predeclared checkpoints with exact offer counts,
checkpoint timestamps and decisions recomputed by `screenRequests`, matching
outcome quality/capture/resource identity and no guard failure. Turnover must match
the source's native hourly schedule, seed, population, duration, recovery and
concurrency. Intent indices, names, rollout IDs, timestamps, terminal states,
accepted UIDs and aggregate counters must agree. Future unadmitted pending intents
are permitted only without execution evidence; they are not outstanding work.

Every screened inventory must contain exactly the native generated object set,
including all ServiceAccounts, Roles and RoleBindings. On the same live LIST objects
used to compute inventory hashes, seed templates (`seed.Render`), shared identity
objects and native setup objects constrain APF, RBAC, selectors, image/resources and
the entire Deployment spec. Only specified Kubernetes defaults, kube-burner tracking
labels, API-assigned Service addresses and ServiceAccount pull-secret references
are normalized for this comparison. Their actual content remains hash-pinned.
V8c also permits the controller-populated ServiceAccount `secrets` list when the
native renderer leaves it unspecified. Entries must be unique valid name-only
references, matching the captured OpenShift dockercfg shape and legacy token
references. This is a field-ownership allowance, not proof that each reference was
controller-created; the exact reviewed checkpoint remains that provenance anchor.
The entire original list still participates in inventory hashing. A changed name,
removed list, or changed SA UID fails checkpoint equality. Explicitly rendered
references, automount policy, RBAC and other user-controlled fields are not ignored.
No Secret payload is fetched or changed. V8b's rejection of genuine generated
dockercfg references is corrected without weakening current-content drift checks.
Recorded accepted rollout markers must match the current Deployment; a pending or
unaccepted intent cannot introduce a marker from the selected run. Earlier native
rollout markers are allowed only with the exact complete checkpoint hash. No object
is repaired, adopted or rewritten by verification. Legacy reuse is unchanged.

Never edit a frozen spec or clear `recoverFailed` on a historical reference to make
it pass. Use a NEW explicit screened-population reference. For a subsequent completed
full-window run, use that run's own outcome/inventory with ordinary reuse instead.
