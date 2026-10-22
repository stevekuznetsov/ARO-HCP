# Startup Monitor

`scripts/startup.py` observes management-cluster startup independently of the RP.
It never queries RP/CS endpoints, creates load, applies sizing, starts a recorder,
changes probes, issues credentials, or performs cleanup. Parent owns lifecycle,
the allocator/ledger, metric-source provisioning, and authorization for live tests.
The maintained monitor is implemented and offline-tested; first live validation
observations were recorded on 2026-09-14. **Startup/readiness is not qualified.**

The dated private handoff under `_artifacts/small-cost-unified-20260914/` records
`startup001` with a Swift NIC profile-coverage gap, `startup002` as a late reattach
that observed the first bootstrap restart and a 50-second apply timeout, and
`startup003` as a five-minute recovery observation. That recovery checkpoint had
KAS/OAS/etcd at three Ready replicas each, packageserver at zero, and 37/54 Pods
Ready; startup remained pending. These are bounded historical checkpoints, not
current live status, uninterrupted fresh-startup coverage, or qualification. Consult
the private handoff and original snapshots for evidence and subsequent status.

The maintained observer replaces historical private V7/V12/V13 observer entrypoints,
not their lifecycle orchestration. Historical evidence remains unchanged. Follow
[storage and recovery](storage-and-recovery.md) and
[campaign recovery](../scripts/CAMPAIGN.md#durable-storage-and-reboot).

## Commands

From the `ARO-HCP-perf-runner` root:

```sh
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p test_startup.py -v
python3 -B tooling/hcp-perf/scripts/startup.py --help

# Offline: consumes reviewed input bytes, validates hashes, freezes a new run.
python3 -B tooling/hcp-perf/scripts/startup.py freeze \
  --config /persistent/private/inputs/startup.json \
  --output /persistent/private/evidence/startup-001

# Read-only live observation, only after parent authorization.
# Use the exact entrypoint returned by freeze, never mutable source.
python3 -B /persistent/private/evidence/startup-001/bundle/startup.py observe \
  --output /persistent/private/evidence/startup-001

# Offline, read-only, no kubeconfig or live API needed to analyze saved evidence.
python3 -B tooling/hcp-perf/scripts/startup.py diagnose \
  --output /persistent/private/evidence/startup-001
python3 -B tooling/hcp-perf/scripts/startup.py report \
  --output /persistent/private/evidence/startup-001
```

`report` is an alias for `diagnose`, emitting JSON to stdout without writing files.
Observation exits 0 for ready, 2 for any other terminal outcome, 1 for launch/input
errors. Offline commands exit 0 when they can produce a report; inspect its integrity
errors, coverage and state rather than interpreting that exit as startup success.

## Input Contract

Version `v1` uses JSON only. Paths expand `~` and resolve against the input file's
parent, not cwd; shell variables are not expanded. `durableStateRoot` is mandatory,
must exist, and must contain the new output. Output parent must already exist.
Management kubeconfig, Python, kubectl, output and state root must resolve outside
`/tmp`, `/var/tmp`, `/run`, `/dev/shm`, including symlink targets. These checks do
not detect every tmpfs mount or kubeconfig exec-plugin/certificate dependency.
Parent verifies actual persistent storage, access validity and external lifetimes.

This is an **input template**, not an approved runnable candidate. Replace every
placeholder, enumerate all known owner/container profiles, and use SHA256 of the
exact referenced file bytes:

```json
{
  "version": "v1",
  "durableStateRoot": "/persistent/private",
  "managementKubeconfig": "../private/management.kubeconfig",
  "kubectl": "/usr/bin/kubectl",
  "identity": {
    "csID": "EXACT-CS-ID-FROM-PARENT",
    "hcpNamespace": "exact-discovered-control-plane-namespace"
  },
  "candidate": {
    "id": "reviewed-candidate-001",
    "profiles": {"path": "startup-profiles.json", "sha256": "REPLACE_WITH_SHA256"},
    "resolvedPolicy": {"path": "resolved-policy.json", "sha256": "REPLACE_WITH_SHA256"},
    "ledger": {"path": "budget-ledger.json", "sha256": "REPLACE_WITH_SHA256"}
  },
  "requirements": [
    {"profile": "kas", "readyReplicas": 3, "endpoints": "kube-apiserver", "readyEndpoints": 3},
    {"profile": "oas", "readyReplicas": 3, "endpoints": "openshift-apiserver", "readyEndpoints": 3},
    {"profile": "pkg", "readyReplicas": 3, "endpoints": "packageserver", "readyEndpoints": 3},
    {"profile": "etcd", "readyReplicas": 3, "endpoints": "etcd-client", "readyEndpoints": 3}
  ],
  "maxDurationSeconds": 2700,
  "pollSeconds": 15,
  "failureWindowSeconds": 120,
  "stallSeconds": 180,
  "diagnostics": {"maxCommandsPerPoll": 8, "maxCommandsTotal": 128, "tailLines": 100, "limitBytes": 65536},
  "nodeMetrics": {"enabled": false, "maxNodesPerPoll": 8, "maxBytes": 4194304}
}
```

Parent supplies the exact CSID already discovered from Clusters Service. No lookup
by RP display name is available. Without `hcpNamespace`, the monitor lists HC/HCP
using the exact `api.openshift.com/id=<csID>` label, requires at most one of each,
and takes the HCP object's namespace. It does not guess namespace suffixes, HC
generated names, alternate label keys, or partial label values. With the exact
namespace supplied, Pod collection can start before HC/HCP exists. The namespace
must have been independently discovered, not constructed from an RP name.

Optional `hcNamespace`, `hcUID`, `hcpUID`, `namespaceUID` constrain expected identity.
Omit UIDs for a future fresh incarnation; first observed UIDs are bound for this
run. Namespace/HC/HCP replacement or ambiguous CSID matches yield unknown evidence,
never automatic adoption. Missing HC/HCP cannot pass readiness. Starting with only
CSID also cannot bypass namespace verification: every readiness snapshot must contain
the exact Namespace and its bound, nonempty UID. A successful but empty Namespace
GET is a coverage error, even if Pod listing succeeds or an earlier UID was bound;
it cannot establish ready or empty-before-first-Pod coverage. Starting with only
CSID cannot prove coverage before HCP discovery; supply the exact namespace when
available, and check `emptyNamespaceObservedBefore` in first-Pod coverage. An empty
snapshot is evidence of a successful empty list, not proof that no short-lived Pod
ever existed between polls. Start observation and verify a saved empty snapshot
before parent submits creation when that ordering is feasible.

## Frozen Profiles

`profiles.path` contains a nonempty array. This fragment is one of the required
profiles, not a complete inventory:

```json
[
  {
    "name": "kas",
    "owner": {"kind": "Deployment", "name": "kube-apiserver", "namespace": "$hcpNamespace"},
    "scope": "controlled",
    "expectedReplicas": 3,
    "containers": [
      {
        "name": "kube-apiserver",
        "image": "registry.example/component@sha256:REPLACE_WITH_REVIEWED_DIGEST",
        "resources": {
          "requests": {"cpu": "100m", "memory": "512Mi"},
          "limits": {"cpu": "100m", "memory": "564Mi"}
        },
        "env": [{"name": "GOMEMLIMIT", "value": "429496729B"}, {"name": "GOMAXPROCS", "value": "1"}]
      }
    ],
    "initContainers": [],
    "ephemeralContainers": []
  }
]
```

Use actual reviewed values, not this illustrative allocation. `$hcpNamespace` is
the only special namespace token and resolves to the exact discovered namespace;
literal namespaces are also supported. Optional owner `uid` pins an existing owner.
Future owners can omit it, but every observed Pod's intermediate controller chain
is still verified by UID, kind, name and namespace. Pod -> ReplicaSet -> Deployment
and Pod -> Job -> CronJob are supported. Missing intermediates, ambiguous owners,
duplicate profiles and unclassified workloads are unknown, never silently ignored.
Profiles are matched by exact owner, not workload-name prefixes or guessed labels.

Every long-running profile (Deployment, StatefulSet, DaemonSet or standalone Pod)
requires **`expectedReplicas`**, a positive integer from the reviewed layout. There
is no default or inferred count. Enumerate every known owner in both scopes, including
the full 52-resource layout when that is the candidate being tested; the monitor
does not hardcode 52 or invent missing rows. A missing expected owner blocks readiness
even if it has no entry in `requirements`. That array specifies additional endpoint
minima, not the complete workload inventory.

For each long-running profile, readiness requires exactly one non-deleting owner,
exact expected desired/current/Ready/matching Pod counts, and converged controller
status for the current `metadata.generation`. Deployments require exact `replicas`,
`readyReplicas`, `updatedReplicas`, and `availableReplicas`. StatefulSets require exact
`replicas`, `readyReplicas`, `currentReplicas`, `updatedReplicas`, and equal nonempty
current/update revisions. DaemonSets require the frozen count to equal desired/current
scheduled, Ready/available/updated counts, with zero misscheduled Pods. Missing status
fields do not count as success. Standalone Pod profiles require one matching Ready
Pod. Extra/surge/terminating Pods prevent exact convergence. Desired four with only
three Ready never passes merely because an endpoint minimum is three. Results expose
these per-profile checks separately as `profileChecks`.

Jobs/CronJobs require `lifecycle` instead of `expectedReplicas`, with explicit `mode`
and a nonempty `reason`. No unused Job is silently required or silently exempted:

```json
{"lifecycle": {"mode": "optional", "reason": "Release-specific job may never be created during this startup"}}
```

- `present`: the exact non-deleting owner must exist; completion is not a gate.
- `completed`: the owner must exist and have at least one observed Job (itself for
  a Job profile); all observed Jobs in that profile must have `Complete=True` and
  no active work. A deleted completion is not reconstructed from absence.
- `optional`: absence is permitted; if present, the owner and observed resources
  remain subject to profile, Pod readiness/completion and failure checks.
- `disabled`: requires absence or explicit suspension with no active work and no
  Pods. A long-running profile may also use this mode/reason, but must specify
  `expectedReplicas:0`; present Deployments/StatefulSets must be observed at zero
  replicas in the current generation. Disabled DaemonSets/standalone Pods must be
  absent. `scope:ignored` alone never disables owner-presence or replica checks.

Succeeded Pods are exempt from Pod Ready only when their exact owning Job is observed
Complete with no active work. A `Completed` container termination alone does not
qualify. `present`/`optional` do not waive observed failures or incomplete Pod states.

`scope` is `controlled` or `ignored`; ignored requires a nonempty `reason` and the
same frozen resource/image/Go-env equality checks. Controlled containers require
explicit CPU/memory requests and limits. Ignored containers require an explicit
`resources` object but can freeze actual absent requests/limits, for example
`"resources":{"requests":{"cpu":"10m","memory":"64Mi"}}` for an unlimited
downstream container, or `"resources":{}` if neither is specified. Missing and empty
resource maps normalize equally; an absent quantity is **not** zero and is never
filled from another field. Added/removed limits or changed requests still cause
profile mismatch. Both scopes retain failure diagnostics
and equality checks. This is an accounting exemption, not an observability exemption.
All regular/init/restartable-init/ephemeral containers belong in the inventory.
Init-container list order is preserved, including restartable sidecars, because it
affects execution and startup budgets. Regular container ordering is normalized.
Only literal non-secret `GOMEMLIMIT` and `GOMAXPROCS` env entries are allowed in
expected profiles; no arbitrary environment or credentials are embedded by policy.
Omitting Go env means expect none, not infer a runtime default. Images must use
digest references. Equivalent CPU/memory quantities normalize before comparison.

`resolvedPolicy` and `ledger` are parent-owned JSON objects. The monitor embeds
their consumed values, verifies source-byte hashes at freeze, and preserves their
hash provenance. It does not allocate, reprice, infer floors, or verify the ledger's
economic model. Expected profiles must be the parent's resolved candidate, not an
auto-approved copy of current workloads. Cost output echoes the frozen ledger;
`budgetChanged:false` means this observer made no edits, not proof no other actor did.
**Parent must validate ledger versus profiles before freeze**, including all owner
rows, expected replica counts, init ordering and controlled/ignored accounting.
Hash verification establishes input identity, not arithmetic or cross-file agreement.
The observer does not independently validate ledger/profile totals or budget caps.

## Phases

- `not-applied`: successful empty workload observation; no rollout assumed.
- `applying`: current Pods or desired templates differ from frozen profiles.
- `progressing`: classified resources, but readiness prerequisites are incomplete.
- `failing`: any observed OOM, restart history/increment, nonzero current termination,
  or liveness/startup probe kill. Continue bounded failure evidence collection.
- `ready`: verified namespace UID, HC/HCP Available, every frozen profile's explicit
  presence/lifecycle and exact long-running replica checks, required Endpoint Pod UIDs,
  all nonterminal Pods Ready, no unknown coverage or current/desired profile mismatch.
- `unknown`: API/identity/owner/profile coverage errors. Record and keep observing
  within the overall bound; missing evidence does not abort startup or pass readiness.

Readiness requires at least KAS3/OAS3/packageserver3 and etcd3 with explicit Endpoints
names and at least three distinct Ready Pod targets each. Duplicate addresses/ports
do not multiply replicas; endpoints must refer to exact Ready Pod UIDs. Add other
required groups explicitly. Actual Service objects and their conditions are saved,
but external reachability and authenticated API health are not inferred from them.
Etcd Ready replicas provide only an availability/quorum **proxy**, not a real etcd
health, leader or membership check. HC/HCP availability is not RP operation success
or full guest/worker qualification.

Profile rows separate `current-pod`, `desired-template`, and `rollout-template`
(ReplicaSet). Current resources, images/imageIDs, UID, owners, status, Go env, probe
configuration and observed policy-hash annotations remain in snapshots. A matching
desired template does not make old running Pods compliant. Resource/profile rollout
mismatch is `applying`, not a hard failure that prevents startup from converging.

**A global policy fingerprint change is not proof that every workload needs a new
Pod, nor proof that the new resources are running.** Fingerprint annotations are
provenance, not the readiness condition. Comparisons use per-owner effective frozen
container profiles, not full global-hash equality. Global fingerprint-driven rollout
behavior and targeted-vs-global reconciliation remain a separate parent/HyperShift
investigation; the runtime controller's canonical global-hash behavior remains
unresolved. The first live observations do not establish a fix. The monitor never
patches annotations or forces a rollout.

`maxDurationSeconds` defaults to 45 minutes (cap 2 hours), `pollSeconds` to 15
(15..30), `failureWindowSeconds` to 120 (0..600), and stall feedback to 180 seconds
(30..1800). First fatal evidence latches failing even if later snapshots recover.
The failure window never extends the overall monotonic deadline; clipped windows
are explicit. Historical restart/OOM status in the first snapshot is conservatively
failure evidence, so a late attach is not a clean fresh-startup test.

Meaningful progress uses high-water Ready/matching/endpoint counts, completed-init
and container readiness counts, first matching profiles, and HC/HCP availability
milestones. New timestamps, resourceVersions, replacement UIDs and repeated restart
loops do not reset the stall clock. `stalled` is feedback only, never an automatic
abort or permission to alter CPU, memory, probes or load.

## Evidence And Bounds

Each poll lists Pods, Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs,
CronJobs, Endpoints, Services and Events, plus exact-CSID HC/HCP and namespace identity.
No Secrets, ConfigMaps or all-cluster Pod dump is requested. Kubernetes conditions,
init waiting, scheduling reasons and current/previous termination state are retained.
Pending scheduling is distinct from a resource OOM. `Completed` exit zero is not
startup success when a liveness/startup kill Event identifies the restart cause.

New OOM/restart/probe-kill and waiting reasons trigger bounded current/previous logs
for the exact Pod/container. No heavy all-logs command runs every poll. Defaults
allow at most eight log commands/poll and 128 total, each with `--tail=100`,
`--limit-bytes=65536`. Failure lists cover every group even when logs are capped.
Literal missing-resource/kind or connection-refused log evidence is surfaced with
the original line; waiting by itself does not establish a missing-API root cause.
Logs can race Pod replacement and cannot recover unavailable previous incarnations.

Every kubectl command has a 10-second request/body deadline, a local process-group
timeout and bounded stdout/stderr buffers. Commands beyond the monotonic deadline
are skipped. At most two diagnostic/metric commands run in parallel; snapshot GETs
are sequential. Slow APIs can reduce coverage and exceed the nominal poll cadence,
but do not extend the total run. Request receipts include argv, exit/error and times.

Optional `nodeMetrics.enabled:true` reads existing nodes' kubelet cAdvisor endpoint
through `nodes/proxy`, with explicit RBAC approval from parent. It does not grant
RBAC, launch privileged Pods, execute shell commands, install exporters or call
`kubectl exec`. Defaults bound to eight nodes/poll and 4 MiB/source; bounds can only
be lowered. Namespace-scoped raw samples preserve available CPU cumulative counters,
quota/period, throttling, memory charge/RSS and source timestamps. No usage-rate,
reset correction, zero-fill or guessed cgroup limit is manufactured. Node caps,
source errors, truncation and disabled metrics are coverage gaps.

Required RBAC: list HC/HCP across namespaces using exact label selection; get the
target Namespace; list the workload/event/service resources above in it; get
`pods/log`. Optional cAdvisor requires get `nodes/proxy`. RBAC label filtering is
not a security boundary; use a reviewed management credential with appropriate scope.

## Storage And Recovery

Freeze creates a new 0700 run directory, private 0600 files and `bundle/` containing
the standalone Python source, resolved config, original input, provenance and hashes.
Non-secret profile/policy/ledger contents are embedded at freeze. Kubeconfig and
kubectl remain persistent absolute references with hashes, not credential copies.
Parent can archive/build source and binaries in its own durable runner bundle; each
new observer run still has a distinct verified `bundle/startup.py` entrypoint.

Execution refuses mutable-source entrypoints, relocated bundles, changed/missing
dependencies and any started run. `start-once` prevents concurrent launch/replay.
Snapshots and compact `status.json` publish atomically with fsync; `progress.jsonl`
appends wall-clock and monotonic elapsed/deadline evidence. `started.json` records
the start boundary. `outcome.json` records terminal coverage, all observed failures,
identity, interruption signal, costs and the last status. Raw snapshots and logs
are private and may contain sensitive operational data; do not publish wholesale.

TERM/INT/HUP request shutdown, stop scheduling new diagnostic work and write the
final summary. In-flight bounded commands may finish; signals do not initiate live
cleanup. SIGKILL, host loss or disk failure can prevent finalization. Missing
outcome or a start gate without outcome is interrupted-or-unknown, not ready or
proof a PID remains alive. Monotonic values cannot be reused after reboot.

`diagnose`/`report` verify bundle hashes, inspect dependency presence/hashes without
authenticating, recompute saved snapshots and aggregate failures without touching
the cluster or rewriting original outcomes. Reports distinguish a currently-ready
saved snapshot from a historically failed or interrupted run. No load or observation
resume exists. Parent reconciles independent RP operations and producers separately;
authorized further observation uses a new freeze/output and explicit coverage gap.

## Explicit Gaps

- First live observation validation is recorded above, but startup/readiness remains
  unqualified. Credential-expiry and reboot recovery are not validated by those runs.
- cAdvisor is optional and may omit quota/RSS/cgroup series. Go runtime/heap metrics
  need a separate existing source; `GOMEMLIMIT` is configuration, not measured heap.
- No lossless watches, Events/log guarantees, Pod-before-discovery reconstruction,
  remote recorder, metric backfill, off-machine backup or automatic credential renewal.
- No EndpointSlice-only support, authenticated KAS/OAS readiness probe, real etcd
  quorum test, or worker/guest-operator qualification. Missing prerequisites stay unknown
  or progressing rather than being silently removed.
- No built-in systemd installer, boot restart, campaign load admission, lifecycle
  mutation, allocator integration adapter, or global fingerprint rollout fix.
