# Bounded Campaign Supervisor

Current capture policy uses bounded120s raw `up` and `timestamp(up)` history at1s
evaluation, with source timestamps deduplicating lookback replay. Continuous loss
is60s since a verified fresh successful source sample, NOT the duration of recovery
confirmation. A recovered sample after an already expired loss interval cannot
erase that failure. Interleaved failed scrapes remain sourceFailures/gap evidence.
Historical failures invalidate admission successes at or before their source time
without advancing the last-success clock. Historical deduplication preserves the
earliest valid evaluation witness, so later stale lookback replay cannot erase a
real success. Nonfinite/invalid values, nonbinary up values and future timestamps
are not accepted as source witnesses.
Arm admission separately requires presently healthy exact identities and two
distinct fresh successful source samples, within120s settling. Duplicate/unexpected
identity observations reset admission confirmation and cannot advance it.
Shared up labels (scheduler paths) are explicitly non-attributable history; only
exact path-specific target API observations advance those identities. Query failures,
size/time bounds, cancellation and all prerequisite failures retain gate outcomes.
No capture configuration or required metric is removed by this policy.

`campaign.py` composes existing hcp-perf primitives on a prepared personal guest.
It does not provision, scale, configure capture, tune existing APF, delete PVCs,
or claim production qualification. Python 3.11+, Linux/systemd, kubectl, Go and
an authenticated `$HOME/.azure-redhat` profile are required.

Use reviewed JSON configuration, not historical artifact launchers. Run:

```sh
python3 -B -m unittest discover -s tooling/hcp-perf/scripts -p test_campaign.py -v
GOWORK=off go -C tooling/hcp-perf test -race ./...
GOWORK=off go -C tooling/hcp-perf vet ./...
python3 -B tooling/hcp-perf/scripts/campaign.py freeze --config /private/config.json --output /private/new-run
python3 -B tooling/hcp-perf/scripts/campaign.py launch --output /private/new-run
python3 -B tooling/hcp-perf/scripts/campaign.py review --output /private/completed-run
```

The new output's parent must exist. Freeze copies the complete module source,
runner, reviewed JSON and pinned kube-burner, builds a new executable, records
credential references/hashes (not credential copies), and hashes the bundle.
Never edit or reuse a frozen run. Configuration is trusted operator input, not
an untrusted API. Private artifacts must not be published wholesale.

## Durable Storage And Reboot

New freezes resolve every supported filesystem reference relative to the **campaign
config file's parent**, never the shell working directory: kubeconfigs, lock,
seed binary, record/retention/adversary input files, capture ready file, stream
manifests, guest-stat outputs, expected-target/final-query files, limits inventory,
and predecessor run/snapshot/inventory paths. Tilde expansion is supported; shell
variable expansion is not. URLs and PromQL stay unchanged. Partial-cleanup
`inventory` is also config-relative, not predecessor-relative. Supply an absolute
path when moving configurations between locations; do not rely on cwd fallback.

`pressureCPUQuery` and `pressureBusyQuery` remain literal expressions. Optional
`pressureCPUQueryFile` / `pressureBusyQueryFile` read UTF-8 expression files at
freeze and cannot be combined with the corresponding inline field. Non-secret
input documents become embedded values in hashed bundle/config.json; queries,
retention, binary, complete source and a capture-ready provenance snapshot are
also retained. Running readiness checks still use the external live handoff and
manifests, NOT the frozen snapshot as a freshness substitute. Credentials are
absolute references plus hashes, never silently copied or replaced by backups.

For live durable campaigns set `durableStateRoot` to a persistent private directory
(for example under the user's home or a persistent mounted data volume). Output
must resolve beneath it. Output, state root, credentials, lock and observation
dependencies must not resolve beneath `/tmp`, `/var/tmp`, `/run` or `/dev/shm`,
including through symlinks. The check is opt-in so offline tests may use ordinary
TemporaryDirectory; omitting it makes no durability claim. It cannot detect every
tmpfs mount, storage loss, expired credentials or missing external kubeconfig
certificate/token/exec-plugin dependencies. Parent must verify those, Azure auth,
tool installation and actual filesystem persistence. The input source/burner may
be temporary because their consumed bytes are copied before execution.

Every new bundle contains hashed `campaign.service`: absolute WorkingDirectory
and ExecStart, correctly escaped values, `Restart=no`, bounded RuntimeMaxSec and
TimeoutStopSec, and **no Install/WantedBy section**. Launch remains a transient
user systemd unit with explicit working directory and Restart=no. It does not
install, enable, replace, or start any persistent unit at boot. The durable unit
file preserves the launch definition when transient systemd state disappears.

After reboot, first inspect without API calls or file writes:

```sh
python3 -B tooling/hcp-perf/scripts/campaign.py recover-inspect --output /persistent/run
```

This verifies available bundle hashes and reports missing/changed files and
external dependencies. `started.json` without `outcome.json` means
`started-without-outcome-interrupted-or-unknown`, never success or proof of a live
PID. Even a terminal outcome does not establish current remote state. `review`
falls back to this inspection when outcome is absent, without synthesizing an
outcome or failing on partial request logs. Inspection does not acquire/delete
the lock, consult systemd, renew credentials or resume Jobs.

**Never remove started.json or auto-resume an interrupted load.** Parent must
reconcile owned remote Jobs and their finite deadlines, capture gaps, accepted
operations, fixtures and credentials. Preserve partial evidence and original
outcomes; any authorized continuation uses new run/session IDs and a new freeze.
No foreground session or reboot persistence can guarantee final export after
SIGKILL/host loss. Observation/lifecycle recovery remains separately owned.

Only for a verified **never-started**, intact bundle, after current readiness and
external lifetimes are revalidated, an operator may use `launch` again. If a
persistent unit definition is specifically needed, manually install/link the
saved campaign.service under the reviewed unique unit name, refusing any existing
unit, reload user systemd, and start explicitly. Do not enable it at boot and do
not overwrite another unit. Its same frozen run entrypoint enforces the start-once
gate. No automatic restore/install command is provided for historical artifacts.

## Configuration

Top-level inputs: `version:v1`, DNS-safe `id`, unique systemd `unit`, shared
absolute `lockFile` for the target environment, `runtimeSeconds` (at most 7200),
`minimumDiskFreeBytes`, `guardIntervalSeconds`, explicit `guestNodes`,
`maxOutstanding`, `recoverySeconds`, `canaryCadenceSeconds`, management/guest
kubeconfig references, `hcpNamespace`, seed template, admission policy,
record/adversary specs, capture handoff, protected component selectors, and arms.
`record` and `retention` may be JSON objects or paths resolved at freeze time.
All remaining primitive specifications use the existing CLI schemas.

Capture input has `endpoint`, `readyFile`, `streamManifests`, and a small
`finalQueries` array. The external handoff must declare `captureReady:true`, the
matching endpoint, and `tokenExpiresAt`. Streams must be running with completed
chunks, recent manifest timestamps and remaining lifetime. Capture owner verifies
chunk hashes and expected replica inventory; this runner independently checks
live up/source timestamps. Per-target identity source loss for 60s stops new demand.
`expectedTargets` is a reviewed target API response path resolved at freeze, or
its activeTargets array. Identity includes labels, scrape URL and pool, so the
two scheduler paths sharing up labels remain distinct. Actual scrapeInterval and
scrapeTimeout are frozen and checked. No missing target is removed or rediscovered
to make an arm pass. `lossSeconds:60`, `gateSeconds:120`, `pollSeconds:5` tolerate
transient loss but require two distinct successful fresh scrapes per identity
before each arm's setup and immediately before measured demand. Fresh means source
age below twice the actual interval plus timeout (15s for the observed5s/5s).
No capture/forward/stream start or stop is performed here. The runner owns only
the explicit new recorder session, starts it before seeding, and exports finally.

Each arm has a unique `name`, `applications`, `durationSeconds` (30..3600),
`contended`, and optional `offeredQPS`. Population changes remove only previous
owned Namespace/APF UIDs using server-side DeleteOptions preconditions. Any PVC
in an owned namespace blocks deletion. Partial setup remains for review; there
is no automatic retry/adoption. Final population and APF remain intact.

### Hour Confirmation

An hour arm uses `durationSeconds:3600`, `turnoverPeriodSeconds:3600`, and normally
`canaryCadenceSeconds:60` (per-arm override); 30s is also supported. This schedules
exactly one rollout per application, with offsets strictly before the hour end.
Recovery admits no new work. The runner verifies scheduled and completed counts;
completion debt is not hidden by a successful client exit. One hour at30/50QPS
offers108000/180000 operations, below the primitive's1000000 cap. Duration3601
and oversized offer budgets are rejected before launch. No error-rate SLO is added.

For any arm longer than300s, configuration requires explicit `setupBudgetSeconds`
and `externalLifetimes`. Runtime remains capped at7200s. Setup budget plus all arm
windows/recovery plus360s drain allowance must fit. This is an operator planning
budget, not a promise every setup operation can consume its maximum timeout.
Before each arm, runtime, token receipts, conservative session submission-based
expiry and actual Job startTime/activeDeadlineSeconds must cover work, recovery
and drain. Ended/suspended/missing Jobs block admission. No runtime extension or
automatic credential renewal occurs. A shortened remaining runtime blocks an hour
before generators start, rather than admitting a knowingly truncated hour.

The following is an interface fragment, not a runnable approved campaign:

```json
{
  "runtimeSeconds": 7200,
  "setupBudgetSeconds": 1200,
  "recoverySeconds": 60,
  "externalLifetimes": [
    {"name": "guest-access", "validUntil": "PARENT_VERIFIED_RFC3339"},
    {"name": "management-access", "validUntil": "PARENT_VERIFIED_RFC3339"},
    {"name": "capture-forward", "validUntil": "PARENT_VERIFIED_RFC3339"}
  ],
  "arms": [{"name": "hour", "applications": 120, "durationSeconds": 3600,
            "turnoverPeriodSeconds": 3600, "offeredQPS": 30,
            "canaryCadenceSeconds": 60, "contended": true}]
}
```

Add every other required external dependency, including RP/KAS forwards when the
actual access path needs them. These are parent-attested lifetimes, not discovered
renewable leases. External access, capture forward, observer token and streams
must cover remaining campaign runtime plus the1000s outer finalization allowance.
Record/adversary specs must cover the full planned runtime for long campaigns;
they are still stopped early in finalization. Parent owns fresh startup, access,
capture provisioning/renewal and any split between screening and confirmation.

### Explicit Limits Policy

Omit `requireLimits` for historical unconstrained inputs. New constrained runs
must supply `requireLimits:{"memoryLimitPercent":110,"memoryRoundingBytes":0,
"inventory":"/private/approved-container-profiles.json"}`. The percentage and
allowed absolute byte rounding are explicit parent decisions, not inferred from
observations. No workload mutation is performed by this check.

The inventory is an array of uniquely matching profiles with `name`, nonempty
`selector` (Pod-template labels), `containers`, and any `initContainers` or
`ephemeralContainers`. Each container records `name`, `image`, full CPU/memory
`resources.requests` and `resources.limits`, optional `restartPolicy`, and `env`
entries for GOMEMLIMIT/GOMAXPROCS. Use the parent's corrected candidate, not an
auto-approved snapshot of whatever happens to be running. Quantities normalize
equivalent units. CPU limit must equal positive request; memory limit must match
the selected percentage within explicit rounding. Runtime targets/images must
match the frozen profile exactly; no guessed GOMEMLIMIT policy is imposed.

Checks cover ALL actual Pods and Deployment/StatefulSet/DaemonSet/Job/CronJob
templates in `hcpNamespace`, regardless of managed-by labels, including terminal
Pods, ordinary init containers, restartable init sidecars and ephemeral containers.
Missing/ambiguous/unlimited profiles fail closed. Coverage receipts identify each
object and profile. Existing template/replica locks remain; profile drift or new
OOM in any observed HCP container is fatal. Observer/competitor namespaces and
guest application Pods are outside this HCP guard scope, not silently asserted
constrained. If "all workloads" includes those, their owning primitives currently
need separate changes: seeded apps omit limits, canary has its own fixed limits,
and competitors intentionally have no CPU limit. Parent must settle that scope
before launch; do not claim global enforcement from HCP namespace validation.

For an explicitly scoped cost candidate, `requireLimits.ignoredOwners` may enumerate
downstream or instrumentation exemptions. Each entry needs `owner` with exact
kind/name/namespace/UID, a nonempty `reason`, and the same frozen container/init
profile fields. Actual Pod controller chains are resolved through UID-verified
ReplicaSets/Jobs to the enumerated owner. Missing intermediate owners fail closed.
Exemption and controlled selectors must not overlap. Exempt profiles still require
exact resources/image/runtime equality and remain covered by safety/OOM checks;
only the CPU=request/memory-ratio assertion is skipped. Every object must resolve
to exactly one controlled profile or one explicit exemption, never a default ignore.
Old inputs omitting exemptions retain the original all-HCP strict behavior.

`planningOnly:true` prevents both validation and freezing. Per-arm
`holdAfterArm:true` writes `progression-hold.json` after a completed measured arm,
then finalizes generators/export and retains fixtures rather than advancing.
This supports operator-selected population/rate discovery without choosing an
unapproved soft-latency/error threshold. Hard failures/debt still stop immediately;
completed soft degradation remains evidence for the hold. `goalSuccessQPS` in API
summaries is null, not an implicit15/30QPS requirement. A capacity point with small
useful throughput may be valid evidence. Same-node retained-population restart is
not supplied by the node-expansion-only adoption interface; resolve that handoff
explicitly before planning unattended multi-run rate comparisons.

### Resource Reporting

CPU usage above request is NOT an automatic safety failure, even with quota:
counter intervals, quota periods, burst credit and source timing can produce
short-window excess. Preserve raw CPU counters, source timestamps, quota/period,
throttling and PSI. Analyze counter resets separately, never turn them into peaks.
Report per-component/replica peak and sustained-window CPU service versus request,
window length and coverage, plus memory charge/RSS/working set and Go runtime target
separately. Distinguish configured limit drift from measured overspend. Do not sum
independent replica p95s or fill missing samples with zero. The supervisor records
limits coverage; resource peak/sustained reduction remains offline analysis, not
an invented CPU SLO. Tight memory OOM is always a stop, not a tolerated overspend.

The recipe is five pause Pods/app, 20 apps/namespace, hourly turnover, explicit
maxOutstanding, one canary warmup and five fresh initial baselines. Read pressure
uses Deployment GET:Pod LIST:ReplicaSet LIST at 2:1:1 in each namespace, seed42,
128 in-flight, 10s body-inclusive deadlines, limit100, and no pagination.
Canary soft expectations are three times the slowest fresh baseline, capped by
30s ACK/120s Ready/120s cleanup hard bounds. Slow completion is evidence, not a
reason to shrink hard deadlines or enforce an unapproved error-rate SLO.

Competitors are discovered/started once, with 90% free-request CPU fill. The
registration and resolved plan remain fixed across all contended arms, including
population changes; there is no rediscovery/retuning between matched arms.
`pressureCPUQuery` and `pressureBusyQuery` must cover actual competitors and
hosts. Positive CPU evidence is required, but is not a 90% utilization guarantee.
Host busy/PSI and per-worker CPU need offline analysis; this does not impose a
made-up utilization target. CPU requests are not CPU limits.

## Safety And Results

Before each arm: exact-set retention refresh, capture freshness and lifetimes.
Continuous guard: HCP template resource/replica/image/runtime drift, fresh local
streams, disk headroom, node identity/Ready/pressure, new OOM, three restart
increments across Pod UIDs in 120s, and configured protected replica minima
with 45s grace. NonReady observations are retained separately. Quorum checks use
Ready replicas as a proxy, not a replacement for etcd metrics.

`status.json` is atomic live progress; `outcome.json` is terminal. Commands have
private argv/stdout/stderr/exit receipts. Each arm retains exact specs, UIDs,
credential expiry/hashes, APF inventory, cadence, turnover debt and API outcomes.
API summaries separate body-complete throughput (including drain denominator),
success per offered second, successful GET/LIST latency, shedding and generator
limits. APF UID pairs need joining to the retained inventory for attribution.

Capture retry attempts and per-target gap/recovery durations are retained under
`capture-gates/<phase>/`; continuous sampled evidence uses `capture-guard/` and
the same per-target loss/recovery policy. Recovery does not erase gaps or qualify
coverage as complete. Interruptions write a terminal gate outcome. Raw streams
remain the source for scrapes missed between supervisor polls.

Optional `cleanupPrevious` names a prior terminal campaign with exactly one
retained population. Freeze copies its exact owned UID inventory and outcome hash.
Before cleanup the new runner checks prior remote sessions stopped, refreshes
retention and requires exact live UID-set equality. Cleanup receipts belong only
to the new run; the old outcome and frozen artifacts are never rewritten.

For an explicitly authorized node-expansion control, `adoptPrevious` instead
specifies `run`, `populationArm`, and a reviewed pre-expansion `snapshot` directory.
Freeze captures the completed predecessor, owned UIDs, final Pod specs/UIDs,
Deployment specs/templates, APF specs/UIDs, original node identities/allocatable,
management node UIDs, CSC spec and HCP resource lock. Live adoption requires exact
preservation plus additive Ready guest nodes. It does not reseed or call setup;
only namespace credentials are reissued. Fresh canary baselines and competitors
are still established. Subsequent population cleanup is explicitly run-owned and
retains receipts in the new run, never the predecessor. Review the snapshot's
provenance; it is trusted operator evidence, not a reconstructed historical proof.

Arms may specify `apiIdentities` (first N namespace identities, validated against
population) and `turnoverPeriodSeconds`. Defaults retain all namespaces and hourly
per-app churn. Thus120/180/240 apps with periods3600/5400/7200 hold120 aggregate
rollouts/hour, while a separate240-app3600s arm tests hourly per-app turnover.
Reseeding changes fixture UIDs, APF names and namespace identities: it is not
equivalent to preserved-population expansion, even when client count stays six.
Optional capture `guestStats` entries contain exact node/output pairs; latest
records must pass the maintained source-freshness check at every arm boundary.

TERM/INT/HUP stop new work, reap tracked process groups, stop/verify owned Jobs,
and attempt recorder and final metric exports regardless of the initial error.
Repeated signals do not interrupt finalization. systemd uses RuntimeMaxSec,
TimeoutStopSec=1000 and KillMode=mixed as the outer backstop. SIGKILL, host loss,
blocked disk or expired credentials can still prevent completion; absent results
are unknown, never success. Capture remains externally owned and active.

Limitations: this is an experimental supervisor, not a generic workflow engine.
The non-overlap lock only coordinates cooperating runners; review live processes
before launch. It does not renew credentials, protect against other operators,
guarantee lossless logs, create off-machine backups, or prove scheduling/CPU
capacity. Node autoscaling/placement changes terminate comparisons rather than
silently changing the environment. Stop on unproductive debugging; retain the
bounded result and make the smallest tested correction in a new run.
