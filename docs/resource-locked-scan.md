# Resource-Locked Scan

## Decision And Status

This records the user's decision for the ongoing three-node diagnostic experiment,
not a finished experiment, pass, production size, or authorization to launch more
cases. It is a pure plan: no campaign runner, autonomous optimizer, new safety
monitor, profile guard or scan-analysis implementation is introduced here. Existing
single-case primitives do not establish that all planned gates are automated.

**Freeze each case, not the initial hypothesis forever.** Candidate-v2 is a
measurement-derived starting point. Keep requests, limits, runtime settings,
images, and three guest nodes fixed within a case and its direct comparisons.
Increase application population in reviewed steps. The user permits evidence-backed,
component-specific resource increases between cases when ancillary components
cannot remain healthy. Never silently rescue an executing case or shrink its
budget merely to meet the cost target.

For the API-serving path, prioritize admission, concurrency, queueing, and APF
tuning before increasing resources. Classify by dependency, not component name:
KAS, etcd, API proxies, authentication, and synchronous admission dependencies can
all limit serving. Background controllers and catalog services need their own
diagnosis; their work is not necessarily bounded by APF. This is a preference for
serving-envelope efficiency, not a claim that APF can shrink retained state or
eliminate every minimum resource requirement.

For a resource change, record the component, evidence of its bottleneck, old/new
requests and derived memory limits/runtime targets, new profile hash, and aggregate
cost delta across all replicas. Hold population and admission settings constant
for the confirmation case. A larger ancillary allowance creates a new total-HCP
footprint even when the API-serving envelope is unchanged. Prefer fixing defective
probes when evidence demonstrates false restarts, but do not substitute more lenient
probes for resources needed to make useful progress.

The earlier client ACK timeout remains a failed observation under its original
case policy. Server-side success or a later harness correction does not prove
that the baseline would have passed. Viability of this candidate with the reviewed
measurement policy is a **working hypothesis**, not a counterfactual pass. Preserve
the original evidence, deadlines and outcome; validate any changed harness afresh.

## Scope And Sources

- The user's three-node requirement, node/Pod progress SLO intent and explicit
  console exception are the source of this diagnostic scope. They are not evidence
  that every platform operator is healthy or that the full platform is production
  ready. Record the exception and actual conditions; do not silently mark console
  or other untested functionality passed.
- [Harness validation](../tooling/hcp-perf/docs/harness-validation.md) defines the
  watch-based canary, evidence gaps and historical validation results. Benchmark
  deadlines are experiment guards, not a published customer latency SLO.
- [Candidate-v2](../tooling/hcp-perf/docs/hypershift-sizing.md#catalog-v2-candidate)
  records the catalog-OOM follow-up, not a qualified minimum. The retained rendered
  input is `_artifacts/perf-three-node-transition/attempt-6/input/candidate-v2.json`;
  preserve its exact bytes and hash rather than regenerating it from moving sources.
- [Admission tuning](../tooling/hcp-perf/docs/admission-tuning.md),
  [assessment semantics](../tooling/hcp-perf/docs/assessment.md) and the
  [cost model](../tooling/hcp-perf/docs/cost-model.md) remain the primitive contracts.
  This decision narrows the sequence; it does not add tool inputs or CLI flags.
- The user's recalled external HCP fee is $0.25/HCP-hour, with a 50% comparison
  target of $0.125/HCP-hour. Label this as user-supplied recall, not a newly verified
  tariff. Retain fee provenance and exact retail price sources/dates for any
  economic comparison; use no assumed discounts, credits or negotiated rates.

Current diagnostic cases may reuse **HCP F** after separately authorized cleanup
of prior applications and run-owned APF objects, with inventory proving no retained
load or stray priority levels contaminate the comparison. This does **not** reset
etcd, its history, caches or prior cluster lifecycle. Label each case `reused HCP`
and record that history. Reuse is an explicit diagnostic exception to the original
[fresh-case design](design/hcp-perf/README.md), not startup qualification. A fresh
HCP confirmation under the same profile from first startup is a mandatory gate
before recommending a production size, alongside the broader production checks.

## Immutable Cases

Freeze each case before launch: resolved configs, seed, endpoint/payload recipe,
APF and KAS settings, capture/retention inputs, tool input snapshot, phase lengths,
deadlines, safety criteria, script bytes, source revision plus dirty-source diff,
binary hash, and generator/recorder/HO/CPO/release/application image digests. Record
target identity, UTC interval, HCP reuse history and the comparison's single changed
variable. Do not overwrite old inputs, reports or raw evidence.

Parallel source editing by the user is active. An executing case must use its
frozen script and binary, not whatever is currently in the checkout. Do not edit
an active script, replace its binary, or promote a new build mid-case. Changed
sources require a separately named binary and reviewed harness validation before
a new scored case. New admission treatments also get new case/run identities.

The request-profile stability guard is a required comparison gate, not an
implemented automatic retuner. Before and after each case, retain the actual CSC
resource policy and projected Deployment/StatefulSet/Pod container resources and
runtime settings, including fallback, overrides, init containers and helpers.
Record object UID, resourceVersion, owner/revision, replicas, actual image IDs,
node identity/capacity/placement and observation timestamps. Observe through the
case as well, so matching endpoints cannot hide transient drift.

Compare actual values with the frozen profile, not merely the profile name or a
requested patch. UID/resourceVersion changes identify objects to inspect; expected
status updates alone are not resource drift. Explain replacements and retain both
identities. Changed resource/runtime settings, image, node capacity, replica count
or unaccounted placement/contention invalidate the fixed-profile comparison. Do
not retune and resume the same case. Missing critical guard evidence leaves the
comparison unverified, not implicitly stable.

## Workload Sweep

| Review Order | Applications | Steady Application Pods | Desired Full Replacements/Hour |
| --- | --- | --- | --- |
| Repeat validation | 3 | 15 | 3 |
| Next | 12 | 60 | 12 |
| Next | 30 | 150 | 30 |
| Next | 60 | 300 | 60 |
| Next | 90 | 450 | 90 |
| Endpoint | 120 | 600 | 120 |

Keep three guest nodes throughout. The endpoint is 200 application Pods per node
on average, not a schedulability guarantee. Platform Pods, canaries, rollout surge,
guest CPU/memory and Pod/IP headroom are additional; verify each node's capacity.

The fixed supporting-object recipe is five replicas in one Deployment plus one
Service, ConfigMap and Secret per application, with 20 applications per namespace
and the matching namespace-scoped benchmark identity/RBAC. Retain exact templates,
image, rollout strategy/surge, object sizes and partial-namespace behavior from
the frozen inputs. Do not export Secret contents to prove the recipe.

Stagger one full five-Pod replacement per application per hour via the fixed
pod-template mutation. Keep API verbs/endpoints, read/list mix, per-operation
request/response payload-size recipe, object bytes, client concurrency/outstanding
bounds and retry policy constant. Population changes aggregate work and returned
object counts; record actual response bytes rather than claiming every larger LIST
has the same wire size. Do not independently ramp API request rate, add endpoints,
change payload bytes or accelerate churn to squeeze an hour into a short case.

For a 300-second validation window, both 3 and 12 applications schedule one
replacement, with the next 12-app event at 300 seconds excluded. Expected work is
derived from the frozen schedule and window, not rounded down to zero. Short
validation is not a sustained-throughput proof; longer cases require separate
review after the harness and capture gates pass. The existing local planner's
zero-start/unique-population rules do not execute this repeated-case sequence.

Measure desired scheduled work, attempted requests, acknowledgements, unique
completed replacements and fresh Ready Pod identities separately. Useful customer
work is completed application replacement, not just canary progress or HTTP 2xx.
Track debt as desired work due minus unique useful completions, including overdue
not-started work and admitted-but-unfinished work. Show debt trajectory, age and
recovery drain against expected desired throughput. Preserve 429s, retry guidance,
every retry attempt and amplification in load accounting; retries are not new
useful completions and rejections do not erase desired work. **Never declare a
case stable when no customer work completes**, even if rejection protects a healthy
canary. Some completions alone also do not establish sustainable throughput.

## Progress And Safety

Use one warmup followed by five fresh successful watch-based baselines. For each
stage, record the soft expectation `min(3 * slowest baseline, hard cap)` separately
from its hard deadline. Missing or failed baseline cycles do not establish a valid
baseline, and historical polling thresholds are not reusable watch baselines.

| Condition | Decision |
| --- | --- |
| ACK, Ready or Cleanup exceeds its soft 3x baseline expectation | Record degraded performance and continue observing within the hard deadline; this alone does not stop progression. |
| ACK fails to complete within 30 seconds | Stop new workload admission/progression; preserve the failure and perform bounded safe recovery/shutdown. |
| Ready fails to complete within 120 seconds from scale-up start, including ACK | Same hard stop; Ready and ACK budgets are not additive. |
| Cleanup fails to complete within its independent 120-second bound | Same hard stop; preserve remaining object/owner identities. |
| OOMKilled or another actual unsafe condition | Safety abort: stop active generators promptly, do not retain pressure for normal recovery. Preserve diagnostics where safe. |
| Restart/liveness safety criterion trips | Safety abort and record the triggering identity, timestamps and evidence; do not advance to the next population. |
| Resource-profile drift or missing critical evidence | Invalidate or withhold the intended comparison and hold review; do not call it an API outage or repair it by retuning. |

Chosen restart/liveness monitor criterion for this plan: **three or more container
restarts, or three or more distinct failed liveness probes, for the same monitored
HCP component/container within a rolling 60 seconds**. Retain Pod/container UIDs
and correlate replacements to their owner so replacement does not reset the
history unnoticed. Record the exact monitored inventory, event/counter source,
sampling cadence and this threshold in the frozen tool input snapshot before use.
Repeated reads of one Event are not multiple failures. This is a chosen experiment
safety criterion, not a measured SLO or a claim of existing monitor support; its
availability must be validated before relying on it. An OOM or other actual unsafe
condition aborts without waiting for three events. Missing safety visibility holds
the next case for review rather than providing evidence of safety.

Progression is operator-reviewed, not automatic. Soft degradation is an observation,
not an abort trigger. Hard deadlines and actual safety conditions stop progression;
preserve the trigger and stop acknowledgements, then diagnose before any new case.
Independent capture may remain alive for evidence. Stopping load does not authorize
deletion of the HCP, applications, captures, PVCs or local artifacts.

Always assess and retain a terminal case, including one with a nonzero process exit.
The canonical runner now distinguishes execution completion/abort/harness error,
experiment expectations, and capture/shutdown failures in `evidence/outcome.json`.
An assessed expectation violation is useful next-decision feedback, not an
unhandled crash that should kill the parent workflow. Continue the review loop,
not necessarily workload admission: the safety and evidence holds above still
apply. Never treat failed capture as valid capacity or an input/harness error as
a passed experiment. Frozen active and historical scripts/artifacts remain unchanged.

## Admission At The Boundary

Start with the current benchmark **Reject / 50 shares** and protected canary
**5 shares / Queue length 10**, with queue count 8 and hand size 2. Snapshot actual
FlowSchemas, all priority levels and per-replica classification/convergence; shares
are relative to the whole APF inventory, including unused levels. They are not a
fixed request rate, a fixed number of seats, or a 50:5 split of only these classes.

At a reviewed workload boundary, hold population and the entire compute profile
fixed: no CPU/memory/GOMEM, GPU, node or HCP-image changes. Prefer the **KAS total
seat/inflight limit first**, currently 200 from non-mutating 150 plus mutating 50.
Record both effective flags on every replica; 200 is the current configured total,
not a measured global throughput guarantee. Change one selected inflight field in
one new case, keeping the other and APF policy fixed. KAS settings are separate
from run-owned setup admission inputs. Wait for rollout/convergence and reverify
unchanged resources before baseline and scoring.

Then, if needed, compare one APF admission knob per new case: one class's shares,
benchmark Reject-to-Queue treatment, or one queue-length setting. Do not combine
seat, shares, queue, load or compute changes in a case. APF controls concurrent
admission and queuing, not request-rate throttling. Judge improvement by useful
completion/debt and protected progress as well as queue/rejection/seat evidence,
not by making 429s disappear. No optimum or autonomous search is claimed.

## Independent Outcomes

Record three independent axes, plus profile validity and economic reporting:

- **Workload:** desired/attempted/completed customer work, canary stage outcomes,
  hard failures, debt, recovery and generator shutdown. Preserve original failures.
- **Performance:** stage latencies versus soft expectations, degraded observations,
  desired versus achieved useful throughput, resources within the locked profile
  and safety evidence. A workload exit zero can still be degraded or inconclusive.
- **Capture:** target coverage, freshness, gaps, raw labels, recorder/export status
  and evidence integrity. Complete export is not proof of complete source capture.

`up == 0` means a Prometheus metrics scrape failed for that target. It is not itself
proof that the customer API was unavailable. Missing targets and stale data are
also distinct capture conditions; do not turn missing metrics into zero usage.

Likewise, a correlated audit `ResponseComplete` with HTTP 200 records server-side
success, **not client receipt**. Keep its audit ID, path, object UID, received and
completed timestamps, source hash and correlation method alongside the independent
client ACK-timeout case metadata. Do not replace client timeout elapsed time with
server latency, relabel the case passed, or infer APF/CPU causation. The existing
assessment schema does not automatically implement all three planned scan axes.

## Capture And Analysis

Refresh and verify the exact reviewed retention inventory **before every test**,
including repeats and observer validation. Preserve the fresh result; an old
refresh or dry run is not protection, and refresh is not a deletion-proof lease.

Start separately supervised real-time local chunk capture before setup/baseline,
verify initial raw chunks and recorder pre-seed readiness, and cover steady,
recovery and shutdown. Retain full raw labels, sample/source timestamps, per-target
identity, gaps, errors, hashes and coverage inventories, not only sampled HTML
plots or aggregate percentiles. Preserve raw artifacts privately and record PVC/PV
ownership and export checks. Recovered backfill is labeled separately and cannot
restore samples or logs that were never collected.

The planned scan-analysis artifact uses **analysis version `v2`** and references
immutable per-case raw artifacts, tool/config/script/source/binary snapshots,
before/after resource guards, independent outcomes, desired/completed/debt data,
soft/hard thresholds, safety triggers, capture validity and cost provenance. Keep
candidate-v2 (resource profile) distinct from analysis v2 (interpretation). This is
a planned artifact contract, not a claim that the current assessment decoder
accepts v2: its documented schema/analysis version remains v1. New analysis must
use new output paths and preserve original reports and statuses. Mid-run reports
remain provisional; elapsed time or a successful render never means finished/pass.

## Economic Interpretation

Report the $0.125/HCP-hour comparison target and actual result, even when it misses.
It is not a forced resource budget, safety threshold, or reason to raise the fee.
Keep configured requests and measured usage separate; memory requests/GOMEM targets
are not hard usage ceilings. Report CPU, cgroup/RSS/working-set memory, Go heap/GC
and safety limits with their own definitions, not as interchangeable quantities.

The fixed-profile cost includes **all** HCP components and untuned overhead, not
only KAS or the components being studied. Show tuned component demand, fixed/untuned
HCP overhead and shared management/collector/platform allocations separately, then
show the included total without double counting. Label costs outside the chosen
compute model, such as storage/network/service charges and guest workers, separately;
do not silently treat exclusions as free. Untuned overhead remains in the relevant
total even if later optimization might reduce it.

Compare exact undiscounted SKU/region prices and capacity bases. A raw-capacity
resource-equivalent estimate is a **lower bound**, not achievable packing or a
production density result; allocatable/usable capacity, HA, fragmentation, failure
headroom and actual pool allocation can increase cost. Do not add CPU and memory
price shares as independent bills or equate low observed usage with schedulable
requests. Retain the user's recalled external fee separately from infrastructure
cost evidence. Validate useful work and the profile first; only then propose a
new measured resource/population relationship or separately authorized optimization.
