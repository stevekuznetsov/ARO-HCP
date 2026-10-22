# Iteration Controller

## Existing-Cluster Image Trial

`rp-update-cluster-image` is an explicit remote mutation for a Succeeded cluster.
Its RP parameters include the complete reviewed `expectedResource`, digest-pinned
`image`, the image-only derived `payload`, `managementKubeconfig`, optional trusted
`allowExecCredential`, and exact `targetIdentity`. It checks HC/HCP/namespace UIDs
and ARM identity, then compares an immediate resource GET to the reviewed snapshot
before one PUT. Only the canonical CPO-image tag may differ. No resource, release,
node-pool, or sizing update is supported through this action.

The personal RP currently does not supply an ETag. Consequently the GET/PUT guard
is **not atomic**: the environment claim and an exclusive writer are required.
Accepted updates use the existing observation-only recovery path; PUT is never
replayed. Final verification requires the existing Succeeded resource and selected
image tag. A preflight or ambiguous error remains blocked for operator review.

`startup-observe-existing` accepts a normal frozen `startup` profile plus an exact
`observationIdentity`. It uses the existing continuation observer against that
identity, without an empty-HC requirement or candidate write. It cannot qualify a
fresh startup. For image trials, order it after completed image update and keep
the expected resource policy unchanged; allow rollout transients but require the
configured sustained availability window and exact new image bindings.

The CLI contract is in `spec.go`. `Open(root)` uses the current Go executable;
`NewController(root, Options)` exposes executor, HTTP client and detached process
factory seams. `Submit` validates and persists without starting work. A CLI should
use its normal Bind/Validate/Complete/Run options pattern and strict JSON decoder,
then call `Reconcile` or `Serve` to dispatch. The executable must implement:

```
iteration worker --root ROOT --attempt TOKEN
```

The worker calls `Open`, `Work`, and `Close`. Controller shutdown is not a stop
request. `Stop` only persists intent; workers poll it directly and cancel their
library contexts. It cannot cancel an RP operation. Reconciliation finalizes
undispatched stopped actions. Unknown remote outcomes remain `BLOCKED`, including
after stop, and retain the environment claim.

## Scope

Supported action kinds directly call sizing Resolve/ToHyperShift, cost Calculate,
RP CreateCluster/CreateNodePool/Inspect/Resume/Delete, and metrics Export. Action
inputs are inline typed JSON. Dependencies reference earlier actions, with
`started` or `completed` milestones; start means a worker has claimed the action,
not that a remote service has accepted it. Explicit `maxAttempts` is 1-10 and
`timeoutNanoseconds` bounds each attempt. A lost read-only action can retry;
lost metrics capture blocks rather than silently changing the measurement.

RP mutations never replay. An accepted `operation.json` enables observation-only
Resume in a fresh attempt directory, followed by action-specific Inspect:
creation requires an existing Succeeded resource; deletion requires HTTP 404.
Without an accepted operation URL, even a crash during preflight conservatively
blocks. Generic inspection does not prove a mutation finished. Resource IDs and
operation URLs are imported into central status during reconciliation, including
while the worker is active. `Status` itself is a journal read, not reconciliation.

Workload setup, arbitrary action-output wiring and the complete Python campaign
workflow are not ported. There is no generic command escape hatch, shell DAG, or Python execution.
There is not yet an operator API for resolving blocked claims. Do not remove a
claim or reuse its environment until the remote outcome has been established.

## Durability

Linux local filesystems only: SQLite WAL with FULL synchronous mode, immediate
transactions, per-run reconcile flock, per-attempt worker flock, and transactional
claim/completion fencing. Worker PID, boot ID and start ticks are recorded; no PID
is ever signaled. Workers start in independent sessions. A dispatch intent is
committed before exec, so a crash before claim can dispatch the same token again
without executing twice. The environment claim is keyed by ID, not incarnation:
changing a label cannot bypass another run's claim.

The pure-Go `modernc.org/sqlite` driver is pinned to `v1.34.5` (module Go minimum
1.21, compatible with Go 1.25). CGO and a system SQLite library are unnecessary.
Cobra is pinned for the parent CLI, not imported by this library.

Each private run directory contains a read-only `spec.json`, initial hash manifest,
and copied executable, synced before journal publication. SHA-256 is checked
before dispatch and work; the executing binary is checked as well. Failed
submissions leave their reserved directory, not a reusable half-submitted run.
The executable and inline inputs are frozen, **not the source checkout** or
external services/evidence. Credentials stay external. Kubeconfig parameters require
absolute external paths, including nested references, without copying credential contents. RP
payloads and metrics data are private artifacts, not sanitized public reports.
No arbitrary executor/transport error text is stored in central status.

This is a new, unshipped schema; no Python database migration is provided.
New databases are marked with SQLite `user_version=2`. Existing databases must
have that version and the exact supported table definitions; unknown, unversioned
and incomplete databases are rejected without creating or migrating tables.

Initial actions recheck dependency and sibling eligibility transactionally when
claimed, not just when dispatched. Read-only retries and accepted-operation
recovery are exempt from sibling-failure gates so uncertain remote operations
can still be observed. Invalid or malformed RP receipts permanently mark that
action `receiptInvalid` and `BLOCKED`; reconciliation does not retry them. Receipt
version, canonical resource/request identity, method and operation URL are checked
against the frozen action and immutable journal binding before recovery HTTP.
The first operation association still relies on the private receipt written by
the provision library; an operation status endpoint cannot prove that association.
Per-run reconciliation errors do not stop Serve from processing other runs.

## Native Startup Lifecycle

`iteration retry-observation --image-bindings FILE --reason REASON` accepts a
reviewed array of image-only expectation corrections. Each entry identifies
`kind`, `component`, `container`, optional `init`, `previousImage`, and a new
digest-pinned `image`. The previous image must match the original frozen spec.
The immutable attempt revision records all corrections and the reason; the run
spec is unchanged. Resources, replicas, lifecycle requirements and candidate CSC
cannot change through this API. A subsequent binary-only retry retains these
corrections. These are observation corrections, not live image updates, and the
continuation still cannot qualify as a fresh startup.

New typed actions are `candidate-apply`, `startup-observe`, `retention-refresh`,
and `rp-admin-credential`. CLI method signatures are unchanged. The `armed` status
field changes the `started` milestone for observation/retention: these actions
must durably establish the empty-target baseline / verified first tag refresh
before creation is eligible. The marker is cleared when the long-running action
ends. Use a new private root; version 1 databases are deliberately not migrated.

`startup-example.json` is a schema-validated **inventory template**, not a claim
that its placeholder policy, payload, resource groups or four core profiles match
a live environment. The parent operator must replace all `replaceWith` fields,
CSC UID/RV/spec, API identity, and external paths, and supply the **entire** expected
Deployment/StatefulSet/DaemonSet/Job/CronJob/standalone Pod container inventory (regular and init containers,
including sidecars), endpoint requirements, and actual requests/limits. Undeclared
workloads or pods fail qualification. Job profiles require `lifecycle` with mode
`completed`, `present`, or `optional`, plus a nonempty `reason`. `completed` requires
a Job Complete condition; `present` requires existence without serving readiness;
`optional` permits absence but still checks resources and failures when present.
CronJob modes are `present` (unsuspended) and `disabled` (suspended with no unfinished
or active child Jobs). Batch profiles omit replicas/endpoints. Exact standalone
Pod profiles use the Job modes; present/optional Pods must be Ready when present.
Unknown standalone Pods remain violations, not wildcard matches.

For example, declare `Job/featuregate-generator` with
`"lifecycle":{"mode":"completed","reason":"bootstrap feature gates"}` and
`CronJob/olm-collect-profiles` with
`"lifecycle":{"mode":"present","reason":"scheduled profile collection"}`.
Both still require their complete container resource profiles. Generated Jobs and
Pods inherit a CronJob profile only via controller UID links (Pod -> Job -> CronJob).
An exact Job profile can override the inherited resource profile. No name-prefix
fallback permits dynamic etcd recovery Jobs: supply their exact name/profile or a
verified CronJob owner, otherwise they remain undeclared inventory.

The sequence is retention first, candidate apply, observation arm, RP create in
parallel with observation, then explicit credential retrieval. Observation is
55 minutes in the template; RP create has one hour, retention two hours. These
are bounded independent workers. RP failure does not cancel an active observer.
Healthy means observed profiles, exact owned-pod inventory, ready replicas and
endpoints are continuously good for five minutes. New probe failures, restarts,
OOMs and profile drift are recorded without stopping capture. `healthyWindow`
can recover; `freshQualified` cannot recover from earlier observed violations.
This remains polling evidence, not proof no event occurred between samples.

Discovery uses HostedCluster annotation
`azure.microsoft.com/hcp-cluster-azure-resource-id` (verified in the deployed ACM
namespace mapping policy), then the exact HostedControlPlane annotation
`hypershift.openshift.io/cluster=HC_NAMESPACE/HC_NAME`. HC `spec.clusterID` is the
cluster UUID; **CSID is the distinct `api.openshift.com/id` label**. An absent HC
CSID label is recorded as unknown, never replaced by the UUID. HCP CSID, when
present alongside HC CSID, must match; HCP `spec.clusterID`, when present, must
match the HC UUID but is not a discovery fallback.
It records HC/HCP/namespace UIDs and cross-checks CSID labels when present. Missing
annotations mean waiting, never a naming fallback. Identity is imported into
central action status during reconciliation. `history.ndjson`, `violations.ndjson`,
`startup-status.json` and `record/` are private per-attempt evidence.

Candidate apply GETs CSC and selected HCs, checks UID/RV/spec and emptiness,
dry-runs an Update, rechecks emptiness, performs one Update with original RV,
then verifies desired spec. It never replays a lost update. An exclusive personal
environment is still required: the HC-empty check and RP create are not an atomic
cross-service transaction. Server admission/defaulting must preserve the reviewed
desired spec; unexpected dry-run changes are rejected.

Retention uses `retention.RefreshNative`, not the existing Azure-CLI-backed
`retention.Refresh`. Native HTTP goes only to `https://management.azure.com`, reads
an external bearer-token file each cycle, preflights every required allowlisted RG,
merges `createdAt`, and verifies the result without retrying writes. Optional RGs
skip only 404 until first observed; explicitly include managed/node RG IDs in that list.
Once observed, a group is promoted into the required set, persisted as
`requiredResourceGroups` in the fsynced retention journal before the next cycle.
A later 404 fails refresh. A lost retention worker blocks and is not replayed
with an empty observed set. The external ARM token is reloaded every cycle and
must be valid/rotated for the full run; a typical one-hour token is insufficient
for the two-hour example. Use an independently authorized token-file refresh
service; this controller does not silently renew credentials. A 401/403 fails
refresh, clears readiness and blocks new initial mutations. Already accepted
RP operations remain bounded and observed, not remotely canceled. No account switch,
credential subprocess, Azure discovery or broad name-prefix tagging occurs.

Management kubeconfig must be readable and stable, using credentials directly or
absolute external certificate/token files. Exec credentials are rejected by default.
Set `allowExecCredential: true` separately on `candidate` and `startup` to trust
the external kubeconfig's client-go credential plugin. Its `exec.command` must be
absolute and `interactiveMode` must be `Never`. Its arguments and environment are
used by client-go without shell wrapping. Set `exec.env` explicitly, including
`AZURE_CONFIG_DIR=/absolute/path/to/.azure-redhat`, to select the intended account
cache rather than relying on worker inheritance. Legacy `auth-provider` remains
unsupported. The trusted kubeconfig, executable and credential cache are external,
not frozen or hash-pinned by the iteration; opt-in explicitly accepts that trust.
Required RBAC: list HC/HCP across namespaces; get/update CSC; list/read namespaces,
pods, deployments, statefulsets, daemonsets, replicasets, jobs, cronjobs, events and endpoints.
The RP must be reachable directly; no CS endpoint or external port-forward is
needed for identity discovery. Credential output must be a new absolute path in
an existing private directory. Cleanup remains an explicitly confirmed `rp-delete`
action, never automatic rollback after a failed experiment.

### Candidate Preflight Recovery

CSC JSON is decoded with exact int64 numeric values, including nested arrays.
Integer-valued Kubernetes/JSON representations compare consistently; fractional,
exponent/decimal encodings and int64 overflow are rejected rather than rounded.
New candidate workers report failures before the real Update as definitive
`FAILED`, including UID/RV/spec guards and dry-run rejection. Errors after the
non-dry-run Update is issued remain `BLOCKED`.

The current controller can read-only recheck a historical blocked candidate even
when the run used an older frozen worker. It requires the original attempt flock
and no matching live process, then GETs the CSC and requires the exact original
UID, resourceVersion and expected spec. On success it journals
`candidateVerification` (attempt token, frozen spec hash, name, UID, RV, timestamp)
and marks only that candidate `FAILED`. It does not replay Update, mark success,
replace the frozen executable, or clear other blocked actions. Failed verification
or a live worker leaves the candidate blocked. No generic force-resolution or
manual database modification is involved.

For an old run, use the **newly built CLI** to request stop and reconcile:

```sh
hcp-perf iteration stop --root ROOT --id small-01
hcp-perf iteration reconcile --root ROOT --id small-01
hcp-perf iteration status --root ROOT --id small-01
```

Wait for workers to exit and reconcile again if needed. Confirm the candidate has
the durable unchanged verification and inspect **all** remaining action states.
The environment claim is released only when no active or uncertain actions remain;
an old retention worker can still report a blocked refresh and is not cleared by
candidate verification. Submit reviewed fresh inputs as `small-02` with the new
binary only after that claim has safely cleared. Never replace `small-01/worker`
or resume its old frozen candidate implementation.

### Replace Only Observation

To replace a faulty frozen startup observer without touching an accepted RP
operation, first stop the **old serve process only**, not the iteration or its
process group. Detached RP/retention workers continue. Use the new executable:

```sh
hcp-perf iteration retry-observation --root ROOT --id small-02 --action observe \
  --reason 'Fix distinct HostedCluster UUID and CSID handling'
hcp-perf iteration serve --root ROOT --interval 1s
```

The core API is `RetryObservation(ctx, runID, actionID, reason) error`. It permits
only running/blocked/failed `startup-observe` in a nonterminal, unstopped run. It
verifies the exact ARM/HC/HCP identity, writes a private immutable per-attempt
worker and `observation-revision.json`, then sends SIGTERM through a Linux pidfd
after boot/start-tick identity checks. It waits for the old process and flock to
be gone, rechecks identity, and transactionally dispatches the new attempt. No
PID-only signaling, SIGKILL fallback, RP replay, candidate update, run stop, or
replacement of the original frozen executable occurs. Timeout leaves the run
unchanged apart from the old observer's own exit and reserved audit artifacts;
the operator can retry after investigation.

The original spec/profile hashes remain unchanged. Replacement ignores the
original measurement attempt limit only for this explicit operator action, with
an absolute ten-attempt bound. The new observer uses its original bounded duration
starting at replacement time, and binds the existing identity rather than requiring
an empty target. Original evidence remains separate. **Fresh qualification is
always false for continuations**, even after five minutes of current health.
Do not run the old controller alongside the new one: it cannot dispatch revision
binaries. Old detached sibling workers may continue; immutable revision manifests
rehydrate fields they do not know when they serialize journal updates. Schema 2
is retained for those live workers; no database migration or editing is required.

### Startup Progress In Status

Startup inputs may explicitly select `"probeFailurePolicy":"availability"`.
Omission or `"strict-probes"` preserves the original strict gate for existing
frozen plans. Availability treats incremental `Unhealthy` probe-attempt events as
warnings, not proof a container was killed. Exact resources, inventory, replicas,
Pod readiness and required endpoints still gate sustained health. OOMs, restarts
and kubelet `Killing` events explicitly attributed to failed liveness/startup probes
remain hard failures and reset the window. Other Killing reasons are not inferred
to be probe failures; their effects remain covered by readiness/restart checks.

New evidence records `probeFailurePolicy`, `warnings`, `newProbeFailures`,
`newProbeKills` and `probeQuality` (`clean` or `degraded` over this observation).
Status exposes sanitized `warningCodes` separately from `issueCodes`/`lastError`.
Under availability, a genuinely fresh run may have `freshQualified:true` together
with `probeQuality:degraded`: it satisfied availability, not strict probe quality.
Earlier hard failures or an interrupted observer still prevent fresh qualification.
Existing snapshots and frozen inputs are not rewritten or retroactively qualified;
an old plan without the field remains strict even with a newer observer binary.

`status` and `list` now enrich the journal response from the current startup
attempt's bounded, durable `startup-status.json`; they make no Kubernetes/RP calls
and do not load credentials. `actions[].startupProgress` includes:

- `phase`, `observedAt`, `stale`, `staleAfterSeconds`, and `snapshotState`.
- Sanitized `lastError`/`issueCodes` and bounded `unhealthyWorkloads` resource names.
- `coverage`: observed/required workloads, ready/required Pods and endpoints, and
  whether inspection completed. Absent coverage means the frozen worker did not
  record it, not zero or healthy coverage.
- `lastSuccessfulPoll`, `healthySince`, latest `newRestarts`, `newOOMs`,
  `newProbeFailures`, `healthyWindow`, and `freshQualified` when recorded.

Staleness is computed at status read time: older than max(three poll intervals,
one minute), or an implausibly future timestamp. No timestamp claims live health.
Malformed/missing snapshots remain explicit diagnostics, not controller errors.
Legacy frozen observer snapshots still expose their sanitized identity errors;
new coverage fields require a worker built with this instrumentation. Raw API
errors, URLs, logs and credentials are never copied into the progress view.

`lastReconciledAt`, `reconcileState` (`running`, `completed`, `error`, or an artifact
availability state), and `controllerProcess` come from an atomically persisted
run-local `reconcile-status.json`. `controllerIdentityVerified` and each action's
`workerIdentityVerified` check PID/boot/start ticks locally at status-read time,
not API health or heartbeat freshness. A CLI reconciliation process can have
already exited by the next status read. These derived fields and files require
no SQLite schema change and cannot be erased by old workers reserializing Run.

### Reconcile An Accepted RP Operation

An exhausted automatic `maxAttempts` budget is not evidence that the remote
operation stopped. Use the current CLI to explicitly make another bounded check:

```sh
hcp-perf iteration reconcile-operation --root ROOT --id small-02 \
  --action create --timeout 5m
hcp-perf iteration status --root ROOT --id small-02
```

Core API: `ReconcileOperation(ctx, runID, actionID, timeout) error`. This command
executes the observation in its own CLI process, not the old frozen mutation
worker. It records an exclusive attempt and `operation-check.json` audit before
HTTP, holds the attempt flock, and calls provision Resume with the validated
immutable operation URL. It never PUTs, DELETEs, replaces inputs, or retries a
mutation. Each explicit call has its own timeout (default 5m, maximum 30m),
independent of the automatic attempt limit; repeating an incomplete check creates
another observation attempt. A concurrent/live operation observer is rejected.

The command is permitted after a run stop request because observing an already
accepted operation is not new mutation work. Canceling the CLI or reaching its
timeout persists `BLOCKED`; repeat the check later. HTTP 200 Failed/Canceled from
the accepted operation status endpoint establishes terminal failure and records
`FAILED`. Resource GET provisioningState alone never establishes that outcome.
Operation success still requires the action-specific final resource verification.
The resource may remain present and Failed: cleanup is a separate reviewed,
explicitly confirmed delete action after the run has settled. Other active or
blocked workers retain the environment claim until their own lifecycle resolves.

When the operation is terminal, the lifecycle owner can request run stop and
reconcile the remaining observers/retention without replaying RP creation:

```sh
hcp-perf iteration stop --root ROOT --id small-02
hcp-perf iteration reconcile --root ROOT --id small-02
hcp-perf iteration status --root ROOT --id small-02
```

```sh
hcp-perf iteration status --root ROOT --id small-02 | jq '{
  state, lastReconciledAt, reconcileState, controllerIdentityVerified,
  observers: [.actions[] | select(.startupProgress != null) |
    {id, state, workerIdentityVerified, startupProgress}]
}'
```

### Complete Profile Contract

Components accept `scope` (`controlled`, the default, or `ignored`) and `reason`.
Ignored scope requires a reason and allows exact resource absence, such as
`"resources": {}` or requests without limits. It does **not** disable resource,
image, Go environment, ownership, readiness or failure checks. Controlled scope
still requires positive CPU/memory requests and limits.

Each regular/init container accepts optional `image` and `env`. A supplied image
must be `registry/path@sha256:<64 lowercase hex characters>` and must exactly match
the observed template and Pod `spec.image`; runtime-specific status image IDs are
not substituted. `env` is an array of literal `{name,value}` entries restricted
to `GOMEMLIMIT` and `GOMAXPROCS`. `env: []` requires both to be absent; omitting
`env` leaves those variables unchecked. Other environment variables are not copied
or inspected. `valueFrom` is rejected for expected or observed Go variables when
the profile supplies `env`.

Serving owners can use `"replicas":0` with
`"lifecycle":{"mode":"disabled","reason":"autoscaling disabled"}`.
The owner must exist, its template must match, desired replicas must be zero and
there must be no nonterminal owned Pods, including terminating Pods. Only explicit
`"allowAbsent":true` on that lifecycle also permits the owner to be absent;
unexpected orphan Pods still fail inventory checks. This covers the existing
zero-replica cluster-autoscaler without treating it as a missing serving replica.
