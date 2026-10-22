# hcp-perf

Experiment primitives for the [HCP performance case runner design](../../docs/design/hcp-perf/README.md),
including personal-RP provisioning, guest workloads, live capture, and bounded
CPU adversaries. A post-provisioning single-case script composes workload and
capture primitives; the bounded [campaign supervisor](scripts/CAMPAIGN.md) adds
guarded multi-arm execution on a prepared guest. Environment preparation,
observation lifetimes, and existing APF patch/restore still require operator work.
There is no general autonomous campaign driver or automatic guest-fixture cleanup.

For a tool/state map and reboot or `/tmp`-wipe recovery, read
[Storage And Recovery](docs/storage-and-recovery.md). Durable files do not imply
automatic resume, valid credentials, stopped remote Jobs, or an off-machine backup.

For durable typed Go action execution, see [Durable Go Iterations](docs/ITERATION.md).
The iteration controller freezes a Go executable and inline sizing, cost, RP and
metrics inputs, with detached workers and conservative crash recovery. It does not
yet orchestrate the complete workload, startup or campaign workflows below.

For the new multi-run offline presentation, see [Performance Dashboard](docs/DASHBOARD.md):
`dashboard export` converts a private reviewed manifest to portable JSON, and
`dashboard render` creates standalone HTML without private sources or network access.
The Small reference completed execution and passed request quality but failed health;
it is not a clean capacity qualification. The historical Small v1 card below is separate.

For new constrained candidates, use the maintained [Budget Allocator](docs/BUDGET.md)
and [Startup Monitor](docs/STARTUP.md), not historical artifact launchers. From this
module directory, with reviewed inputs and existing private, persistent output parents:

```sh
python3 -B scripts/budget.py allocate --input examples/budget-small.json --output "$BUDGET_OUTPUT"
python3 -B scripts/budget.py check --input examples/budget-small.json --output "$BUDGET_OUTPUT"
python3 -B scripts/startup.py freeze --config "$STARTUP_CONFIG" --output "$STARTUP_OUTPUT"
# Only after parent authorization; use the exact frozen entrypoint returned above.
python3 -B "$STARTUP_OUTPUT/bundle/startup.py" observe --output "$STARTUP_OUTPUT"
python3 -B scripts/startup.py report --output "$STARTUP_OUTPUT"
```

Set the output variables to new absolute paths. Parent must reconcile the budget
ledger with complete startup profiles before freeze; allocation does not generate
the entire startup input or authorize deployment/load. Observation is RP-independent
and read-only; first live validation does not qualify readiness. Runtime-controller
canonical global-hash rollout behavior remains unresolved; see the startup guide.

**Start here for another t-shirt:** [Characterizing A Hosted Control Plane
T-Shirt](docs/tshirt-methodology.md) records the complete methodology, actual
automation boundaries, historical lessons, evidence locations, and improvement
priorities. The [Small v1 card](docs/cards/small-v1.md) preserves the measured
baseline; the [Double Small plan](docs/cards/double-small-plan.md) is not yet a
measured result.

The [Small v1 HTML presentation](docs/cards/small-v1.html) leads with findings,
then methodology, measured node contention, client behavior, resources, and
evidence limitations. Generate a new offline presentation from this module:

```bash
GOWORK=off go run . card render \
  --input docs/cards/small-v1.json \
  --contention docs/cards/small-v1-contention.json \
  --resources docs/cards/small-v1-resources.json \
  --experience docs/cards/small-v1-experience.json \
  --output /absolute/path/to/new-small-card.html
```

Output paths must be new. No raw logs or live sources are loaded; portable input
data includes provenance paths and hashes and should still be reviewed before
sharing. Worker toggles and sample-coverage strips distinguish missing data from
zero utilization. The HTML is a presentation of the characterized envelope, not
a qualification or an automatic sizing recommendation.

The optional resource dataset adds per-container CPU and memory timelines with
recorded request lines and overspending highlights. Memory views distinguish
cgroup usage, working set, RSS, Go-managed memory, GOMEMLIMIT, and hard safety
limits. All-HCP totals exclude marked instrumentation by default and require
complete matching timestamps; missing data is not zero-filled. See the
[resource extraction method](docs/cards/small-v1-resources.md) for scope and
source freshness limitations.

The optional [client-experience dataset](docs/cards/small-v1-experience.md) adds
completion-time throughput, HTTP 429 fraction, successful GET/LIST latency, and
canary phase durations on the same UTC timeline. Configured offered QPS is not
measured dispatch; 429 fractions use completed logical requests. Partial drain
bins use their actual width, and undefined latency/fractions remain missing.

For a checkpointed, manual workflow, see
[Personal Single-Node](docs/personal-single-node.md). It distinguishes an
as-deployed baseline from a scientifically qualified sizing/contention case.

Before another long cycle, follow [Harness Validation](docs/harness-validation.md):
prove local evidence, then the observer, then a five-minute actual guest workload
before the full soak. Offline tests and a completed local stream/report are not
proof that the short guest workload or physical contention has been validated.

For explicit post-provisioning cases, start with
[`scripts/run-case.sh --validation`](scripts/run-case.sh) and the
[case script instructions](scripts/README.md), including the fixed three-node
application sweep and unseeded platform-plus-canary baseline. The canonical runner
was promoted after both prior runs exited; no compatibility wrapper remains.
Without `--validation`, the runtime default is still a 7200s soak. Do not edit an
executing script or overlap cases. The [live validation record](docs/harness-validation.md#live-validation-record)
documents workload exit 0 in about 8m30s (3 apps/15 Pods, ten canaries, one rollout),
but the recorder started after seed and logs were incomplete (642 errors, 216
gaps); watch initialization had zero gaps and polls had zero API errors. Case 4
passed 24 canaries over two hours steady plus ten minutes recovery but exited 1
due to export/health failures, so neither run is a clean evidence qualification.
Local streaming began at 17:10:53 UTC, not the soak's 15:29 steady start; read-only
backfill can recover Prometheus-retained metrics, not missing raw logs. The new
pre-seed readiness gate is implemented but not live-validated and requires a new
digest-pinned recorder image, not just a rebuilt local CLI. Live pre-seed readiness
is the next validation scope, not an established result.

The runner now observes degradation instead of aborting on a single missed
baseline-derived expectation. Steady calls pass soft `--ack-expected`,
`--ready-expected`, and `--cleanup-expected` while retaining hard budgets of
30s/120s/120s. Completed degraded cycles continue; hard timeouts and functional
errors still stop workload admission and trigger bounded recovery. Elapsed cadence
slots are recorded, not burst-replayed, and after-window completions remain
unscored diagnostics. Workload exit 0 is not qualification or complete observation
coverage. See [the artifact contract](scripts/README.md#shutdown-and-artifacts).

## Implemented

- Strict JSON input validation and machine-readable output/errors.
- Exact, deterministic per-container sizing from KAS anchors, ratios, floors, and rounding.
- Offline experimental HyperShift CSC rendering with all-container fallback policy.
- Case plans and seeded, period-configurable application-rollout schedules (hourly by default).
- Protected-canary baseline deadline calculation (library).
- One bounded, ownership-checked Deployment canary cycle through an existing guest identity.
- Offline CPU competitor plans from explicit node snapshots.
- Live worker placement/request discovery and registered, self-expiring CPU Jobs.
- Personal-RP cluster/node-pool create, get, explicitly confirmed delete, and private admin credentials.
- Pinned kube-burner seed rendering/execution, namespace identities, and expiring credentials.
- Two-hour guest Deployment turnover with bounded outstanding work and incremental evidence.
- Standalone scrape generation from scoped monitor snapshots and runtime target checks.
- Standalone Prometheus capture on infra nodes with persistent metrics storage.
- Selected-key TLS snapshots from generated scrape bundles into capture namespaces.
- Independent infra recorder Jobs with PVC-backed sanitized snapshots, opt-in watches, and acknowledged raw logs.
- Read-only recorder-PVC export, including terminal failed sessions, with archive validation and hashes.
- Bounded Prometheus range-query export with incremental files and a manifest.
- Bounded local metrics streaming into private, hashed chunks before run completion.
- Offline, per-series sample/coverage analysis with artifact integrity checks.
- Read-only case progress and self-contained offline HTML reports.
- Durable offline assessments with evidence hashes, scoped data points and conditional carry-forward items.
- Opt-in short harness validation in the case script; soak remains the default.

Run from this module directory on Linux with Go 1.25.7 or newer. Dependencies
are the versions in `go.mod`/`go.sum`, including Kubernetes/client-go v0.35.3,
component-helpers, k8s.io/utils, and sigs.k8s.io/yaml. This is not a
standard-library-only tool. `seed run` additionally needs a separately installed,
SHA-256-pinned kube-burner v1.17.3 executable; the runner image does not include it.

```bash
export GOWORK=off
go mod download
go test -race ./...
go run . --help
go run . sizing resolve --profile examples/small.json
go run . sizing hypershift --profile examples/small-complete.json
go run . case plan --case examples/case.json --profile examples/small.json
go run . case schedule --case examples/case.json --profile examples/small.json
go run . adversaries plan --input examples/nodes.json
```

The `small.json` sizing example is deliberately an **incomplete coefficient illustration**,
not an operational policy covering all HyperShift-managed containers. Resolution
does not establish coverage or viability. CPU limits are never emitted;
GOMAXPROCS is absent unless explicitly set. Safety limits apply only to memory.
Per-container `goMemoryLimitPercent` overrides the profile percentage; zero omits
GOMEMLIMIT, allowing explicit non-Go entries.

Profiles use integer millicores/MiB and rational coefficients to avoid rounding
ambiguity. `sizing resolve` output is an experiment schema, **not** a
ClusterSizingConfiguration object. Kind/workload/container/init identity is kept
separate so similarly named sidecars are not accidentally merged.

`sizing hypershift` renders an experimental CSC from `small-complete.json` or a
selected profile, without applying it. It requires the custom HyperShift schema
and HO/CPO projector support. See [Personal HyperShift Sizing](docs/hypershift-sizing.md)
for API limits, initial budgets, fallback coverage, and the explicit personal-only
apply gate. The candidate is not a measured minimum or proof of startup safety.

`examples/small-catalog-v2.json` is a separate next-attempt candidate after the
attempt-4 catalog startup OOM. It preserves the original profile and all non-catalog
resource budgets; see the [evidence and render-only gate](docs/hypershift-sizing.md#catalog-v2-candidate).

Case plans include bounded startup, seeding, baseline, calibration, settling,
steady, recovery, and shutdown as applicable. The phase total excludes
capture/session setup and must not be used as a complete remote Job deadline.
Scheduled operations represent intended work, not proof of execution. No pending
work is automatically skipped or considered complete by these primitives.

## Environment Retention

Before **each new test**, refresh retention for the explicitly approved resource
groups with a trusted local CLI and authenticated `az`:

```bash
hcp-perf environment refresh --input retention.json --timeout 5m
```

The input is `{"version":"v1","resourceGroups":["/subscriptions/UUID/resourceGroups/NAME"]}`;
see [`examples/retention.json`](examples/retention.json). Review the full IDs for
the environment, customer and managed/node RGs that must survive. This is an
explicit allowlist, not subscription-wide listing, name-prefix discovery, or
automatic inclusion of shared resources. Only authorize groups you intend to
retain. The command uses `$HOME/.azure-redhat` and never logs in or switches accounts.

The timeout defaults to 5m (maximum 10m); `--dry-run` defaults to false. A dry run
only reads and returns a plan, so it cannot satisfy a test's retention preflight.
The command reads every target first, merges a fresh UTC `createdAt` tag while
preserving other tags, then rereads to verify it. JSON output includes overall
and per-RG status, attempted/updated/verified flags, and sanitized failures.
Require both a zero exit and verification of every listed group; partial or
uncertain updates must be inspected, not retried or rolled back automatically.
This is not permanent retention or protection against concurrent deletion. Before
each validation, verify the exact inventory and fresh UTC timestamps. Refreshing
tags neither cancels an already-running cleanup nor provides a retention lease.

The [case runner](scripts/README.md) requires `retention.json` in its config
directory, preserves a hashed read-only input copy, and gates all test activity
on refresh plus exact-RG verification. Both validation and soak use the canonical
`scripts/run-case.sh`. Historical inputs and artifacts remain unchanged.

## Runner Image

Build with the module directory as context, not the repository root:

```bash
docker build -t example.azurecr.io/hcp-perf:local .
docker run --rm --read-only example.azurecr.io/hcp-perf:local --help
docker push example.azurecr.io/hcp-perf:local
```

Replace the example registry with one accessible to your management cluster.
Record the pushed image digest and use `registry/repository@sha256:<digest>` in
the adversary RunSpec. Tags are not accepted. Both `examples/adversaries.json`
and `examples/capture.json` contain **all-zero placeholder digests** that must be
replaced with verified runner and Prometheus server digests, respectively.
Validation checks digest syntax, not image existence or provenance; it does not
reject the all-zero placeholder. The builder uses the version tag `golang:1.25.7`,
not an immutable base digest. For reproducible rebuilds, also pin a verified
builder digest and record the source revision and target platform.

The multi-stage image contains a static `/hcp-perf` binary and the builder's CA
bundle, runs as UID/GID 1000, and has no shell or home directory. CPU Jobs invoke
`/hcp-perf cpu burn` directly, override the UID/GID to 65532, disable service-account
token mounting, and use a read-only root filesystem. They need no writable volume.
The management kubeconfig used by the submitting CLI is never copied to these
remote Jobs. For containerized CLI use, explicitly mount input files and pass
`--kubeconfig`; kubeconfig exec-auth helpers are not included in the scratch image.

Metrics export needs a writable mounted parent directory, owned by the runtime
UID, with a **new** child path such as `--output /artifacts/run-1`. Do not rely on
`/` or a home directory for artifacts. HTTPS export uses the included system CA
bundle; for private trust, mount a trusted CA bundle read-only and set
`SSL_CERT_FILE` to its path. Do not disable certificate verification.

## Live Capture

Use only an authorized test management cluster. Outside a cluster, every live
command needs an explicit `--kubeconfig`; omission selects in-cluster credentials,
not the default local kubeconfig or `KUBECONFIG`. The examples below assume
`MGMT_KUBECONFIG` points to your management kubeconfig. The caller needs Kubernetes
permissions to create/read/manage the capture resources and, if discovery is
enabled, create and bind the cluster-scoped read roles.

Before starting, edit `examples/capture.json`: set `id` to `small` to match the
adversary example, replace the Prometheus digest, and select an existing
StorageClass with suitable capacity, binding, retention, and backup policies.
Capture requires nodes labeled `aro-hcp.azure.com/role=infra`, tolerates
`infra=true:NoSchedule`, and runs Prometheus as UID 1000 with fsGroup 2000.
Admission policies must permit these settings. Time-based retention does not
guarantee that the disk cannot fill.

```bash
go run . capture start --input examples/capture.json \
  --config examples/prometheus.yaml --kubeconfig "$MGMT_KUBECONFIG" \
  --timeout 10m --wait=true
go run . capture status --id small --kubeconfig "$MGMT_KUBECONFIG"
```

`capture start` creates `hcp-perf-small`, a ServiceAccount, immutable ConfigMap,
explicit PVC, Service, and Prometheus StatefulSet. It never adopts an existing
namespace. After a failed or interrupted start, inspect `status` and the returned
inventory rather than retrying start as an update. Partial resources are retained;
API timeouts can leave resources beyond the last confirmed inventory. There is
no resume/update command.

`--timeout` defaults to 5m and bounds the CLI operation, not the session lifetime.
`--wait` defaults to true for capture start/stop; `--wait=false` returns without
waiting for pod convergence. Capture runs until explicitly stopped. A timeout
does not undo creation or stop remote work.

The supplied Prometheus YAML scrapes only `localhost:9090`. It demonstrates a
baseline server setup, **not HCP or experiment metrics coverage**. Ready means
Prometheus server readiness, not successful scrapes. Structural configuration
validation is not Prometheus semantic validation; validate with the selected
image's `promtool check config` and inspect actual targets and samples.

### Authenticated Scraping

With `kubernetesDiscovery=false`, capture mounts no service-account token and
creates no RBAC. With `kubernetesDiscovery=true`, it mounts the projected token
and API CA and grants get/list/watch on nodes, services, endpoints, pods, and
EndpointSlices across **all namespaces**, plus get on `nodes/metrics` and the
non-resource URL `/metrics`. It does not grant `nodes/proxy`, Secrets, or writes. These
permissions remain until cleanup; adversary Jobs must not use this ServiceAccount.

The flag does not add scrape jobs. Supply actual `kubernetes_sd_configs` and
scrape authorization/TLS settings in the YAML. The commented cAdvisor job in
`examples/prometheus.yaml` is an opt-in starting point for all management workers,
not a guarantee that the cluster trusts the same CA for API and kubelet serving
certificates. Verify addresses/SANs, CA trust, token audience and authentication,
authorization, and network reachability. Custom CA/Secret projections are
supported through exact `tlsMounts` requirements, normally supplied by
`capture start --scrape-bundle`. Arbitrary host paths/volumes are not accepted.
Never work around trust failures by disabling TLS verification.

Before measuring contention, verify successful, fresh samples for **every worker
hosting the target and all relevant containers on those workers**, including
neighbors and adversaries. Add and verify HCP component, node, and other required
scrapes for your experiment separately. Discovery alone proves none of this.
This is standalone Prometheus: operator-managed `ServiceMonitor`/`PodMonitor`
objects are not automatically consumed or continuously reconciled.

### Generated Scrape Bundles

Edit `examples/scrape.json` to identify actual HCP monitor namespaces, the
management cluster, and a verified node-exporter Service/named port. Set
`prometheusNamespace` to the new capture namespace and
`prometheusServiceAccount` to `prometheus` (the capture account's actual name).
Remove or replace the example namespace regex; when both forms are supplied,
the regex intersects the explicit names. Intervals are 5s or 15s.

```bash
go run . scrape generate --input scrape.json --kubeconfig "$MGMT_KUBECONFIG" \
  --output ./new-scrape-bundle --timeout 5m
go run . capture start --input capture.json \
  --scrape-bundle ./new-scrape-bundle/bundle.json \
  --kubeconfig "$MGMT_KUBECONFIG" --timeout 10m
go run . scrape check --input ./new-scrape-bundle/bundle.json \
  --endpoint http://localhost:9090 --timeout 1m
```

The last command requires an operator-managed port-forward. Generation reads
monitor objects, not Secret contents, and preserves partial bundles with blockers
on failure. It renders worker kubelet/cAdvisor, an explicitly configured **HTTP**
node-exporter endpoint, and supported in-scope component monitors. Management
defaults disable node-exporter; the example Service is not guaranteed to exist.
An HTTPS/authenticated exporter needs separately reviewed configuration or a
generator enhancement, not an invented Service or disabled TLS verification.

Capture requires a gap-free bundle and `kubernetesDiscovery=true`.
`--scrape-bundle` replaces `--config` and cannot be combined with spec `tlsMounts`.
It reads only named source objects, copies only selected keys into immutable
Secrets/ConfigMaps, and mounts them read-only. The submitting identity needs the
source read permissions; the Prometheus account gets no Secret read permission.
Snapshots have no automatic credential rotation/expiry monitoring. Capture labels
its namespace `network.openshift.io/policy-group=monitoring`.

`bearerTokenSecret` and `authorization.credentials` Secret selectors are rendered
as selected-key credential files, using the same copy/mount mechanism as TLS.
References resolve in the **monitor namespace**, not its selected target namespace.
`authorization.credentialsFile` and `bearerTokenFile` are also supported. Nonempty
credentials require HTTPS and cannot follow redirects. Conflicting authentication,
optional keys and incomplete selectors are blockers. Empty legacy bearer selectors
and `tlsConfig.cert: {}` emitted by Operator API serialization mean absent settings.
`FileReference`/`FileMountRequirement` generalize TLS mounts; the `tlsMounts` JSON
field, paths and Go aliases remain because existing capture sessions persist them.
Its optional allowlist covers TLS and HTTP credential references alike.

A management ServiceAccount token does not authorize guest KAS metrics. Prefer
the monitor's native client certificate and guest metrics RBAC when available.
If bootstrap has not installed that RBAC, explicitly select each endpoint needing
a separately provisioned guest observer token in the scrape input:

```json
"endpointCredentials": [{
  "monitorKind": "ServiceMonitor",
  "namespace": "hcp-example",
  "name": "kube-apiserver",
  "endpointIndex": 0,
  "bearerTokenSecret": {
    "kind": "Secret", "namespace": "hcp-example",
    "name": "hcp-perf-observer-token", "key": "token"
  }
}]
```

This replaces only that endpoint's bearer/client-certificate authentication;
the CA, server name, URL, discovery identity and other endpoints are unchanged.
Removing the old client certificate matters: KAS can authenticate it before a
bearer token. Duplicate, unmatched and out-of-scope overrides fail closed. There
is no automatic mapping of guest tokens to every HCP monitor. Generator and
capture do not provision guest identities. See
[`scripts/guest-metrics-identity.go`](scripts/guest-metrics-identity.go) and the
[operator instructions](scripts/README.md#guest-metrics-observer) for explicit,
personal-only setup. Token expiry must cover the run; immutable snapshots cannot
be renewed in place, so use a new capture session when credentials change.

New namespaces/monitors and changed monitor specs require regeneration. Capture
config is immutable: use a **new capture ID/session**, not a second start against
the same ID. A pre-HCP generic cAdvisor session can observe worker containers
early, but cannot supply nonexistent HCP component monitors. Later endpoint/pod
discovery is dynamic only within the generated monitor snapshot. `scrape check`
requires an up target per expected scrape pool and rejects unhealthy active
targets, but does not establish missing-replica coverage or sample freshness.

Configuration is stored verbatim in a ConfigMap, **not a Secret**. Do not put
inline tokens, passwords, private keys, or credential-bearing URLs in it. Use
file-based credentials where available. The HTTP Service is cluster-internal
without authentication or TLS; no ingress, network isolation, admin API, reload
endpoint, or port-forward is created. Restrict access appropriately.

## Live CPU Adversaries

After capture creates `hcp-perf-small`, edit `examples/adversaries.json` to use
your pushed runner digest and the real HCP control-plane `targetNamespace`
(the valid lowercase `ocm-example` is only a placeholder). Choose explicit
duration, CPU reserve, and memory bounds appropriate for the test.

```bash
go run . adversaries start --input examples/adversaries.json \
  --kubeconfig "$MGMT_KUBECONFIG" --timeout 5m
go run . adversaries status --namespace hcp-perf-small --id cpu-small \
  --kubeconfig "$MGMT_KUBECONFIG"
go run . adversaries stop --namespace hcp-perf-small --id cpu-small \
  --kubeconfig "$MGMT_KUBECONFIG"
go run . adversaries status --namespace hcp-perf-small --id cpu-small \
  --kubeconfig "$MGMT_KUBECONFIG"
```

`adversaries discover --input FILE --kubeconfig FILE --timeout 5m` is the read-only
placement/headroom preview. The live RunSpec accepts `cpuFillPercent` (1..100,
default 90 when no reserve is supplied) or explicit `reserveCPUMilli`, not both.
The default requests 90% of each discovered worker's free CPU **request** headroom,
not 90% measured utilization. Memory request/limit default to 32/128 MiB. These are
JSON fields, not CLI flags; an existing example with an explicit reserve keeps
that policy. Discovery also checks memory request headroom and available Pod slots.

Start accepts a RunSpec, **not an externally supplied node snapshot**. It discovers
live target placement and scheduler-effective CPU commitments across namespaces,
then records the snapshot and plan in an immutable registration before creating
one Job per target worker. Workers must be Ready, schedulable, have the expected
worker/pool labels, and have request headroom after the reserve. This is a
point-in-time observation, not a capacity reservation or continuous replanning.
This dynamic placement/fill is not empirical pressure feedback. Keep the treatment
frozen during the scored window; status reports placement validity/drift rather
than moving or resizing Jobs. Revalidate worker coverage and measured CPU pressure
separately, and invalidate a scored window whose required placement/pressure fails.
The namespace must already be labeled `app.kubernetes.io/managed-by=hcp-perf` and
must differ from the target namespace. Labels are safety checks, not authorization.

Jobs have a CPU request but no CPU limit, explicit memory request/limit, and
runnable workers sized to host CPU capacity. CPU generators enforce the requested
`durationSeconds` (1..86400, at most 24h); each Job also has an active deadline of
that duration plus 60 seconds and no retries. The CLI timeout is independent.
Submission/running status is not proof of measured contention or memory headroom.

Run IDs are create-only. Partial submission retains the registration and created
Jobs for inspection and explicit stop. Stop is asynchronous and reports `stopping`
while pods remain; poll status until `stopped` before assuming pods are gone.
Stop is not evidence cleanup. Neither
this workflow nor capture cleanup deletes the target HCP or its Azure resources.

Stop intent is recorded before suspending Jobs. A partial submission with missing
expected Jobs deliberately remains `stopping` with `submissionIncomplete=true`:
the tool does not claim that a potentially interrupted submission is safely
finished. Inspect the recorded Job inventory and stop outcome. Terminal Pods
remain as evidence but do not themselves prevent stopped status.

## Durable Kubernetes Recording

After starting capture, edit `examples/record.json` with its namespace, a pinned
runner image, explicit target namespaces, and storage/resource bounds:

```bash
go run . record start --input examples/record.json --kubeconfig "$MGMT_KUBECONFIG"
go run . record status --namespace hcp-perf-small --id observations \
  --kubeconfig "$MGMT_KUBECONFIG"
go run . record stop --namespace hcp-perf-small --id observations \
  --kubeconfig "$MGMT_KUBECONFIG"
```

Use the ID/namespace actually configured in the specification. Start returns
submission status, not proof of complete observation streams. The recorder runs
independently of the invoking CLI, on infra with a dedicated PVC, read-only root
filesystem, projected service-account credentials, and finite lifetime. No Job
retry restarts an interrupted recording. Stop suspends the Job; poll status to
confirm that nonterminal Pods are gone. Evidence remains on the PVC.

The worker polls Pods, Events, Deployments, and StatefulSets in explicitly named
namespaces, including namespaces that do not exist yet. It stores operational
identity, placement, images, resource settings, reasons, counts, and status, not
environment values, arguments, annotations, Secret bodies, or free-form error
messages. It does not discover an RP cluster's eventual HCP namespace yet.
Polling can miss short-lived objects and transitions. Set `enableWatch: true`
for sanitized list/watch streams; set **both** `enableLogs: true` and
`rawLogs: true` only after acknowledging that raw logs can contain secrets.
These are JSON spec fields, not command flags. Profiles and node/cgroup-specific
collection remain unimplemented.

The API permissions allow read-only listing of those resource kinds cluster-wide
so recording can begin before target namespaces exist. Namespace selection limits
polling, not authorization. Pods returned by the API can contain sensitive data
even though persisted output is sanitized. Review this permission grant for the
personal environment; no Secret read or mutation permission is granted.
Watch adds cluster-wide watch permission and logs add cluster-wide `pods/log`
get permission. Selection scopes collection, not authorization.

Files beneath `/data/observations` contain the input manifest, incremental NDJSON
streams, checkpoints, and terminal result. Writes are synced incrementally; actual
durability depends on the filesystem/storage provider. `maxObservationBytes` bounds
the combined poll files (default 1 GiB, maximum 8 GiB). The worker reports that boundary rather than silently
truncating a stream. API errors are recorded and polling continues; the eventual
result is unsuccessful if required streams failed. A stopped/killed worker may
leave only partial records, which must not be interpreted as complete capture.

Watch reconnects/relists and logs are best-effort, not a lossless audit. Logs
start with at most 1000 lines per identity and can overlap on reconnect; short-lived
pods, rotations, and older restarts can be missed. Inspect `watch.ndjson`,
`logs.ndjson`, and `stream-summary.json` for gaps/errors. `maxStreamBytes` bounds
each live file (default 1 GiB, maximum 8 GiB), separately from the snapshot budget.
`maxConcurrentLogs` bounds concurrent log requests per recorder worker (default
256, range 1..256). Zero or omission selects the default for all three limits.
Effective limits are recorded in `manifest.json` and `stream-summary.json`.
Both session and worker specs accept `streamSyncIntervalMillis`: zero/omission
defaults to 1000 ms; explicit values must be integers from 100 through 5000.
Each live file uses one fixed 64 KiB write buffer shared by all followers
(128 KiB total for watch and logs, below 1 MiB). Capacity writes flush to the OS;
an independent worker flushes and fsyncs each file at the configured interval,
not per line. Buffered accepted bytes count toward `maxStreamBytes`.
The resolved interval is retained in session/worker specs and the manifest;
`stream-summary.json` reports `durability.flushIntervalMilliseconds` and
`durability.maximumBufferedBytes` (per file). These are policy, not live health.
Normal stop joins followers and flush workers, flushes all accepted complete JSON
records including final gap/lifecycle markers, and performs final fsync before
closing. Flush, sync, and close failures fail the result; asynchronous storage
failures cancel the affected streams and remain latched through finalization.
Sync does not hold the append lock, but capacity writes/flushes can block it on
disk I/O; a filesystem stall cannot be preempted by cancellation. A crash can
lose the last sync interval, longer if storage stalls, and can leave a partial
final NDJSON line. This is not lossless physical-power-outage protection.
Namespace polling and log discovery remain serial; broad selections can take
longer than the poll interval, and capacity drops remain explicit gaps.

Both `record start --input FILE` (session JSON) and `record worker --input FILE`
(worker JSON) accept integer `logRequestQPS` (default 20, range 1..100) and
`logRequestBurst` (default 40, at least the resolved QPS and at most 200).
Zero/omission selects each default independently; a QPS above 40 needs an explicit
burst at least as large. One log bucket covers all followers, including Pod UID
checks, log opens, reconnects and EOF verification. Polling, LIST/WATCH and log
discovery share a separate fixed 10 QPS/20 burst bucket. Thus the default aggregate
worker budget is 30 QPS/60 burst, not per namespace or follower; multiple recorder
workers multiply that pressure. Existing metrics collectors use independent clients.
These observer-only settings do not change HCP API/APF configuration.

At 20 QPS/40 burst, 215 followers' 430 startup GETs need at least 19.5 seconds
before network/server overhead, within each attempt's 30-second deadline. Lower
budgets or overloaded APIs can still fail; concurrency and request rate remain
bounded, not automatically increased. `error.requestStage` distinguishes
`discoverPods`, `verifyPodIdentity`, `openLogs`, `streamRead`, and `verifyLogEOF`.
Local client-go reservation failures use `clientRateLimited`, separate from server
`tooManyRequests`; pre-header renewal deadlines are `timeout`, not healthy renewals.
No raw API error messages or response bodies are persisted.

New sessions persist resolved request settings in immutable `session.json` and
`spec.json`; worker `manifest.json` and `stream-summary.json` retain them too.
Export metadata includes `configuredLogQPS` and `configuredLogBurst` from the
registration. Older registrations lacking these fields leave them absent on export
rather than claiming that an older worker used today's defaults.

All discovered init, regular and ephemeral containers are required, including
previous logs when restart count is nonzero. Current and previous retrievals use
distinct identities; reconnects retain the one-second overlap. A clean EOF is
complete only when a fresh Pod GET verifies the same UID/restart and terminated
state (or a previous-log retrieval). Disappearance or replacement is not proof
of completion. Real failures have a three-attempt budget; planned live-follow
renewals do not consume that budget, but terminal/previous retrievals are bounded.
Transient capacity pressure is retried on later discovery, not marked done;
capacity gaps still mean **partial capture**, even if later retrieval succeeds.
Size for current containers **plus previous requests**, not just Pod count. The
256 cap leaves headroom for roughly 220 concurrent streams, not 220 containers
with arbitrary restarts; larger selections require separate recorders.

The example opts into 256 log requests and 8 GiB each for polling, watch and log
data, with a 32 GiB PVC, 500m CPU/512 MiB memory requests and a 2 GiB memory limit.
This is recorder overhead on infra nodes, not HCP workload sizing. Logs still require `enableLogs: true` and `rawLogs: true`;
watch still requires `enableWatch: true`. Provision disk for up to 24 GiB of data
plus checkpoints/metadata and retain private exports until explicitly pruned.
Rebuild and publish the runner, then use its new digest-pinned image in the
session spec; an older image does not implement these options.

Once status confirms a completed, terminal failed, or acknowledged suspended
writer with no nonterminal recorder Pods, export it:

```bash
go run . record export --namespace hcp-perf-small --id observations \
  --kubeconfig "$MGMT_KUBECONFIG" --output ./new-record-export --timeout 10m
```

Export creates a five-minute read-only PVC helper using the registered runner
digest and streams `/hcp-perf record archive` through Kubernetes exec/SPDY. It
needs helper Pod create/get/delete and exec permissions, working volume ownership
and attachment, infra capacity, and compatible exec transport. These are live
preflight requirements, not guarantees from unit tests. Export never stops the
writer or deletes the PVC. It validates allowlisted tar entries, bounds the
archive to 32 GiB/8 GiB per file, and writes private `archive.tar` and `manifest.json`.
Small files retain USTAR headers; exact 8 GiB files use GNU headers because they
exceed USTAR's size field. These export ceilings cover all configured data budgets.
Ordinary failed exports remove incomplete local output; helper-cleanup failure
preserves it and reports `helperCleanupFailed`. Preserve the PVC in either case.
Hashes establish integrity, not completeness; archives containing raw logs are
not sanitized. There is no offline recorder/timeline analyzer yet.

For a manually mounted
volume, `record worker --input FILE --kubeconfig FILE` runs the same bounded worker
directly; without `--kubeconfig` it uses in-cluster credentials.

## Guest Canary

The standalone canary primitive uses the credentials supplied by the caller; it
does not create its fixture, RBAC, or APF policy. Prepare a dedicated namespace
and a Deployment converged at zero replicas, then use the bounded protected
identity's **guest** kubeconfig, not an administrator or the management kubeconfig:

```bash
go run . canary run --namespace hcp-perf-canary --deployment canary \
  --kubeconfig "$CANARY_GUEST_KUBECONFIG" \
  --ack-budget 30s --ready-budget 2m --cleanup-budget 2m
```

One canary is a serial **`0 -> 1 -> 0` Deployment cycle**, not a read of an
already-running application. Readiness requires a fresh Running/Ready, non-deleting
Pod UID, the exact Deployment -> ReplicaSet -> Pod controller UID chain, and
Deployment generation/status convergence. Cleanup requires observed zero replicas
and no owned Pods, including terminating or terminal Pods. This is client-observed
convergence across resource streams, not an atomic server-wide snapshot.

The guest client needs Deployment get/list/watch, scale update, and ReplicaSet/Pod
list/watch in the fixture namespace. Its protected APF priority must be **bounded,
not exempt**. Deployment/ReplicaSet controllers, the scheduler and kubelets retain
their native identities and ordinary request paths; protecting the initiating
client does not exempt the controller chain. Policy installation and actual APF
classification evidence remain separate operator responsibilities.

The client establishes LIST/WATCH streams from each resource kind's LIST
resourceVersion **before mutation**. Successful Ready/removal completion uses the
client receipt time of the watch evidence satisfying convergence; elapsed durations
use the client's monotonic clock, not server object timestamps. There is no fixed
poll-interval quantization. Network, controller reconciliation, watch-delivery and
client observation latency remain; this is not pure API-server execution latency.
Results record `measurementMode:watch` and watch diagnostics. A closed, expired,
errored or overflowing measured stream invalidates the phase rather than silently
relisting it into a successful sample.

| Timing | Measurement |
| --- | --- |
| ACK | Scale-up request start to successful API acknowledgement. |
| Ready | Same scale-up start to observed readiness; **includes ACK**, so do not add them. |
| Cleanup | Its own start through observation setup, scale-down and observed removal; separate from Ready. |

Each stage has its own recorded latency, soft expectation (when supplied), and
hard budget; preflight is separately
measured and excluded from ACK/Ready. Writes have no application-level retry; a
rejection or conflict is an explicit failed cycle. APF classification header
capture and automatic baseline execution by the standalone primitive remain
unimplemented. The case script orchestrates its own baseline and records
`canaryPolicy:"observe-degradation"`. It calculates each expectation as
`min(3 * baseline maximum, cap)` with ACK/Ready/cleanup caps of 30s/120s/120s and
writes integer nanoseconds in `commands/baseline-expectations.json`. It passes
these via the three `--*-expected` flags, never as fitted `--*-budget` values.
The budget flags retain hard defaults of 30s/120s/120s; preflight is independently
bounded at 30s, the CLI at 5m, and each runner call at a 6m outer timeout (plus
the existing forced-shutdown grace). Exceeding an expectation does not cancel
observation or fail a completed cycle. Logical/functional errors still fail.

Historical canaries used a five-second polling default. Their raw results and
deadlines remain unchanged; a new renderer does not turn them into watch samples.
Require **five fresh consecutive successful watch-based baseline cycles** after
unscored warmup before deriving expectations for watch-based measurements. Do not
reuse polling-derived thresholds or guess a missing historical measurement method.
New readers must prefer `commands/baseline-expectations.json`, with fallback to
persisted `commands/baseline-budgets.json` only when the new file is absent. The
old name and its original semantics remain historical evidence, not rewritten
soft-policy runs. Progress/assessment integration must use the recorded policy.

After a scale-up attempt, bounded scale-down cleanup is attempted even on timeout
or cancellation. It can therefore extend beyond the caller timeout by up to the
cleanup budget. Original and cleanup failures are both returned with structured
last-observed state. This is not HCP cleanup: only the designated canary is scaled
back to zero. Do not run concurrent writers against that Deployment.

## Stop And Cleanup

Stop workload producers first and poll their status. Keep capture running through
any desired recovery window, then **export needed metrics before stopping capture**:
stopping also removes access to its HTTP server. For example, run this in a
separate terminal and use the export commands below with `http://localhost:9090`:

```bash
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n hcp-perf-small \
  port-forward service/prometheus 9090:9090
```

After export and verification:

```bash
go run . capture stop --id small --kubeconfig "$MGMT_KUBECONFIG" \
  --timeout 10m --wait=true
go run . capture status --id small --kubeconfig "$MGMT_KUBECONFIG"
```

Stop scales Prometheus to zero and retains its PVC, configuration, Service,
ServiceAccount, and any discovery RBAC. The Prometheus disk persists metrics;
recorder Jobs use separate PVCs and must be stopped/exported separately. Optional
container-log streams reside there, not on the Prometheus disk. Status diagnostics
alone are not a durable stream.

Only after evidence is safely exported, explicitly consent to destructive cleanup:

```bash
go run . capture cleanup --id small --delete-evidence \
  --kubeconfig "$MGMT_KUBECONFIG" --timeout 5m
```

Cleanup requires an owned StatefulSet with zero desired replicas and no
nonterminal pods anywhere in the namespace, including terminating/adversary pods.
All Jobs must also be terminal or explicitly suspended, including submitted Jobs
whose Pods have not yet appeared. Stop recorders as well as adversaries.
Stop all producers first: this check cannot prevent a concurrent Job creating
new pods. A partial start without a StatefulSet needs operator-reviewed manual
cleanup. Successful cleanup means namespace deletion was **accepted**, not
completed. It destroys the entire namespace, including adversary Jobs and the
PVC; the disk may also be deleted according to the StorageClass reclaim policy.
Namespace deletion also garbage-collects the owned discovery and recorder RBAC. There is no
automatic evidence export or HCP cleanup.

## Export Existing Metrics

Supply a JSON array of named queries. Scope selectors to the intended experiment;
the exporter does not discover namespaces or choose metrics automatically.

```json
[
  {
    "name": "container-memory",
    "expr": "container_memory_usage_bytes{namespace=\"YOUR_HCP_NAMESPACE\",container!=\"\"}"
  }
]
```

```bash
go run . metrics export \
  --endpoint http://localhost:9090 \
  --queries queries.json \
  --start 2026-09-08T15:00:00Z --end 2026-09-08T16:00:00Z \
  --step 30s --output ./new-capture
go run . analyze --input ./new-capture
```

For Azure Managed Prometheus, use its direct HTTPS endpoint and supply ambient
authentication without recording it in artifacts:

```bash
export HCP_PERF_BEARER_TOKEN="$(AZURE_CONFIG_DIR="$HOME/.azure-redhat" az account get-access-token \
  --resource https://prometheus.monitor.azure.com --query accessToken -o tsv)"
```

The CLI refuses bearer authentication over HTTP and does not follow redirects.
Tokens are not refreshed; this is a bounded export primitive, not a durable
capture session. Queries are sequential with a 60-second deadline per query.
Use smaller explicit windows/query sets if a backend rejects query cost.

Export requires a **new** output directory with an existing parent. It writes
private files containing raw responses and a manifest of query definitions,
times, status, warnings, file names, and SHA-256 digests. Each response is synced
before its manifest entry is finalized. The command does not promise filesystem
crash recovery or complete durable collection. Cancellation/error may leave
partial evidence; preserve it and inspect the manifest. A nonzero exit can still
produce useful structured stdout. Errors are JSON on stderr.

Analysis keeps labels/series separate, checks hashes, and reports missing or
nonfinite samples instead of treating them as zero. PromQL step coverage is not
proof of scrape freshness: lookback can repeat a source sample. Capture source
timestamps and scrape health when freshness matters. Analysis is not a capacity
verdict, a cgroup budget check, or evidence that contention occurred.

Raw metrics can contain sensitive labels or server errors. Protect exports and
review queries; do not query credentials, Secret contents, or arbitrary payloads.

## Stream And Report

`metrics stream` blocks locally and starts its window **now**. It is not a remote
collector, does not detach, and is not automatically started by the case script.
Use an operator-supervised process and persistent private local storage. The
parent of each new output directory/file must already exist.

Start local streaming **before each case**, verify the first raw chunks, and size
its duration to cover setup, baseline, steady, recovery and export. This is the
recommended default collection workflow, not an optional post-run-only step.
The case script's final exports remain separate and mandatory. Neither streaming
nor filesystem sync establishes lossless sampling or host/PV-failure durability.

```bash
go run . metrics stream --endpoint http://127.0.0.1:9090 \
  --queries queries.json --output ./new-metrics-stream \
  --duration 1m --chunk 30s --step 5s --lag 15s \
  --drain 90s --chunk-timeout 90s
```

Required flags are `--endpoint`, `--queries`, and `--output`. Timing flags are:

| Flag | Default | Contract |
| --- | --- | --- |
| `--duration` | `6h` | At least one chunk, at most 24h; starts now. |
| `--chunk` | `30s` | 1s..5m, an exact multiple of step; final window may be shorter. |
| `--step` | `5s` | At least 1s and no larger than chunk; evaluation, not scrape interval. |
| `--lag` | `15s` | 0..5m delay after a window for scraping. |
| `--drain` | `90s` | Positive, at most 5m catch-up after duration plus lag. |
| `--chunk-timeout` | `90s` | Positive, at most 90s for the whole sequential query batch. |

The root `manifest.json` is an atomic checkpoint; `journal.jsonl` and
`chunk-000000/`, `chunk-000001/`, etc. retain per-window manifests, `stream.json`,
and raw query responses with hashes. Output appears after each completed batch,
not only at the end; stdout contains only the final JSON result. Inspect
`status`, `dataQuality`, `completedChunks`, `pendingChunks`, `behind`, error and
incomplete counts. `status:complete` means windows were attempted, not that every
query succeeded or every source was present. Preserve per-query statuses/warnings.

Queries are not retried. A crash can leave the journal ahead of the checkpoint;
there is no resume or reuse of the output directory. A new stream does **not**
backfill earlier time. Recover still-retained history with a separate bounded
`metrics backfill --start ... --end ...` into a new directory. Adjacent chunks share
one inclusive boundary; deduplicate only by query, full labels, and timestamp.
The same authentication/privacy restrictions as `metrics export` apply.

For a fixed historical interval, `metrics backfill` immediately exports serial,
bounded windows without waiting for wall time:

```bash
go run . metrics backfill --endpoint http://127.0.0.1:9090 \
  --queries queries.json --output ./new-metrics-backfill \
  --start 2026-09-09T17:44:07Z --end 2026-09-09T17:51:31Z \
  --chunk 1m --step 5s --timeout 30m
```

Use reviewed UTC boundaries, not these example times for a new case. Required
flags are endpoint, queries, output, start and end. The range is at most 24h and
1440 windows; chunk defaults to 1m, must be a step multiple and at most 5m. Step
defaults to 5s (minimum 1s); the whole-operation timeout defaults to 30m (maximum
24h). The grid is anchored at start; end is never rounded up. The new private
directory contains `manifest.json` with planned/completed/pending windows and
quality counts, plus `chunk-NNNNNN/` raw responses, hashes, export manifests and
`.window.json`. Errors/incomplete data remain explicit. There is no retry, resume,
adaptive splitting, expression rewriting or overwrite of previous evidence.
Backfill cannot recover samples absent from the source or prove scrape freshness.

```bash
go run . case progress --input /absolute/path/to/run
go run . report render --input /absolute/path/to/run \
  --metrics ./new-metrics-stream/chunk-000000 \
  --metrics ./new-metrics-stream/chunk-000001 --output ./new-report.html
```

`case progress` accepts only `--input`, the **run root**, not its `evidence/`
subdirectory. It returns one local snapshot, not live process status. `report
render` requires `--output` and at least one of `--input`, repeated `--metrics`,
or `--assessment`.
Each metrics path is an exact export/chunk root; it does not recursively load a
stream root or automatically discover metrics beneath a run. The report is a
static, private HTML file with embedded data, JavaScript and SVG, requiring no
server or source files when opened. It preserves labels, gaps, warnings and source
integrity failures rather than substituting zeros. Reports use bounded sampling
for display: `--max-series` defaults to 200 (1..1000 overall), and `--max-points`
defaults to 300 (2..500 per series); non-up queries retain at most 20 preview
series each. Query counts/coverage include all analyzed series, and omitted counts
are explicit. This selection is not statistically representative or lossless;
short segments may disappear. Raw local responses, hashes and full warnings are
preserved unchanged and remain the full-resolution evidence, not the HTML preview.

### Review An Existing Run

These commands are **offline**: reuse the same existing run and exports, not a
new workload. Use an existing private output parent and new filenames. For a
second-generation report, keep the historical report and all source artifacts
read-only; `assessment.json` and `report-reviewed.html` below must not exist yet.
The optional recorder summary is an explicitly supplied, already extracted file
outside the run; omit that flag if unavailable. Neither command extracts raw logs.

```bash
go run . assessment build --input /absolute/path/to/existing-run \
  --metrics /absolute/path/to/existing-run/evidence/metrics-capture \
  --metrics /absolute/path/to/existing-run/evidence/metrics-nodes \
  --recorder-summary /absolute/path/to/extracted/stream-summary.json \
  --output /absolute/path/to/review/assessment.json
go run . report render --input /absolute/path/to/existing-run \
  --metrics /absolute/path/to/existing-run/evidence/metrics-capture \
  --metrics /absolute/path/to/existing-run/evidence/metrics-nodes \
  --assessment /absolute/path/to/review/assessment.json \
  --output /absolute/path/to/review/report-reviewed.html
```

With both `--input` and `--assessment`, rendering checks explicit experiment IDs
when known and SHA-256 equality for shared evidence paths. At least one matching
in-run evidence path is required; shared external metrics alone cannot tie an
assessment to a run. It does not follow arbitrary imported evidence paths or
authenticate provenance. Assessment-only rendering is allowed but cannot perform
this run match. Without `--assessment`, `--input` builds an embedded assessment;
optional `--recorder-summary` then requires `--input` and cannot be combined with
`--assessment`. Rendering never writes an implicit assessment sidecar. Use
`assessment build` for the durable machine artifact.

The [assessment schema](docs/assessment.md#public-schema) records `schemaVersion`,
`analysisVersion`, findings with evidence IDs and hashes, and numeric `dataPoints`
with `unit`, `population`, `window`, `measurementMethod` and provenance references.
`carryForward` items retain scope, blockers and conditions, not auto-applied tuning.
The builder writes a new private JSON file with file and parent-directory sync;
exit zero means written, not qualified. Workload and capture status remain separate,
qualification is `not_assessed`, and unknown values/methods are not guessed. Do not
put credentials, Secret data or raw logs into review inputs; metrics labels and
expressions can still be sensitive, so keep both outputs private.

### Read The Preview

The timeline shows **one group per canary with three lanes**: API acknowledgement,
Pod ready (includes ACK), and Removed after scale-down (Cleanup). It does not add a
duplicate command-start row for the same canary; command records remain in details.
Short stages have minimum-width markers, not inflated durations. Hover for exact
times and compare each stage's own seconds against its recorded budget.

Choose **Display labels** such as `namespace`, `pod`, and `container` for readable
series names; expand **Full labels and selected series metadata** for the complete
embedded label set and sample/coverage facts. Distinct raw series with identical
display names get stable `[series N]` collision suffixes. This changes labels only,
never series identity, samples or aggregation.

The default 200-series UI selection is a heuristic, not an all-component analysis.
Full-sample metadata is retained for selected series, and query totals cover all
analyzed series; omitted labels/samples remain only in the raw sources. Charts use
raw query units, not name-based conversions. There is no all-component or whole-HCP
CPU/memory/GC calculation or automatic sizing recommendation yet. Review scoped,
unit-aware, time-aligned evidence separately before carrying any result forward.

See [Harness Validation](docs/harness-validation.md) for root-relative commands,
expected artifacts, live acceptance checks and separate failure-injection gates.

## Personal RP And Guest Workloads

`rp` supports `2024-06-10-preview` and `2025-12-23-preview` against a trusted
personal frontend origin, with loopback HTTP or verified HTTPS. It sends synthetic
ARM identity headers, not an Azure bearer token. The subscription must already be
registered in that RP with the configured tenant. It creates neither customer
infrastructure nor registration. Supply an API-version-correct raw resource JSON
payload referring to existing customer resources, plus a reference such as:

```json
{"resourceGroup":"YOUR_CUSTOMER_RG","cluster":"YOUR_CLUSTER"}
```

For node pools add `"nodePool":"YOUR_POOL"`. Every RP command requires a new
`--output` directory, including get and credentials. Do not reuse output paths.

```bash
go run . rp create-cluster --input rp.json --resource cluster-ref.json \
  --payload cluster-payload.json --output ./rp-create --timeout 1h
go run . rp create-nodepool --input rp.json --resource nodepool-ref.json \
  --payload nodepool-payload.json --output ./rp-nodepool --timeout 1h
go run . rp get --input rp.json --resource cluster-ref.json --output ./rp-get
go run . rp credentials --input rp.json --resource cluster-ref.json \
  --output ./rp-credential-receipt --credentials-output "$GUEST_KUBECONFIG"
```

Create checks freshness with GET before PUT; that is not atomic against another
creator. Preserve accepted operation URLs after timeout rather than blindly
retrying. Normal payloads/responses are private artifacts; credential-operation
bodies are never archived or embedded in result JSON. The new credential file is
0600. `rp delete` requires `--confirm-delete` and deletes only the referenced
resource, never its resource group; there is no automatic rollback/cleanup.

```bash
go run . seed render --input seed.json --output ./seed-preview
go run . seed run --input seed.json --kubeconfig "$GUEST_KUBECONFIG" \
  --output ./seed-execution --timeout 1h
go run . seed identities --input seed.json --kubeconfig "$GUEST_KUBECONFIG"
go run . seed credentials --input seed.json --kubeconfig "$GUEST_KUBECONFIG" \
  --output "$PRIVATE_CREDENTIALS_DIR" --expiration 6h
go run . turnover run --input turnover.json \
  --credentials-dir "$PRIVATE_CREDENTIALS_DIR" --output ./turnover-evidence
```

For one guest node, seed creates two namespaces, each with 20 five-replica apps:
**40 applications and 200 app Pods total**, plus Services and synthetic
ConfigMaps/Secrets. Plan capacity for system Pods and rollout surge as well.
The seed binary pin is for the extracted executable, not the release archive;
`--binary` overrides the path but retains the hash check. Identity setup is
create-only and scoped to owned seed namespaces. Credentials are expiring
namespace-SA kubeconfigs; check `credentials.json`'s actual `validUntil`, not just
the requested lifetime. Keep credentials separate from shareable evidence.

Use identical `guestNodes`, `applications` and `namespacePrefix` in seed, setup
and turnover inputs. Optional `turnoverPeriodSeconds` in case, scan and turnover
specs defaults to 3600 when absent or zero; explicit periods accept 1..86400.
`case schedule` calls `RolloutsForPeriod(apps, seed, duration, period)` with resolved
plan timings, includes only offsets before the admission end, and rejects more
than 80,000 rollouts. At 120 apps / 300s, periods 3600, 1800, 900, 600 and 300
schedule 10, 20, 40, 60 and 120 intents, respectively. Baselines/canaries do not change.

Turnover defaults to two hourly rounds (7200s), then up to 600s recovery
without new admissions, with explicit `maxOutstanding`. A period change is an
explicit **client activity treatment**, not a population or APF change. Choose
`maxOutstanding` independently, initially 8 and constant across period sweeps.
Queued due work retains scheduled/admitted/accepted/completed timing; future
rounds outside the window are not missed events. Nominal rate is intent, not
completed throughput or HTTP QPS. See the [runner contract](scripts/README.md#client-activity-treatment).

Turnover events record backlog, acceptance, completion, and failures, not HTTP/APF
receipts. Optional `--request-log NEW_PATH` on `canary run` and `turnover run`
records bounded private HTTP evidence (status, Audit-ID and APF headers when
available). The canonical script wires it for every canary, including warmup and
baseline, and turnover, using unique `commands/*.requests.jsonl` paths. Header
timing excludes limiter waits/body completion; synchronous raw recording adds
known delay. Freeze a new version for future runs, never patch historical raw
evidence or reuse the old v6 script after recording a new activity version.
Activity evidence and [price provenance](docs/cost-model.md) remain separate.
A successful exit
can still leave unadmitted work; inspect `result.json` and `events.ndjson`.
Neither identities nor credentials install APF. Policy installation, protected
canary fixtures and classification verification remain explicit operator work;
the canonical runner orchestrates the five-cycle baseline. Never substitute
administrator credentials for turnover/canaries.

## Seed Image Pulls

The [seed example](examples/seed.json) performs **no image pre-pull**. Generated
kube-burner jobs explicitly set `preLoadImages: false`: v1.17.3's preloader can
adopt the fixed `preload-kube-burner` namespace and delete all namespaces labeled
`kube-burner-preload=true`. Cleanup, job/global GC, and churn remain disabled;
seeding performs no automatic cluster cleanup, including after failure.

Complete image pulls and warmup **outside the scored window**. Seed render/run
outputs and persisted bundles/results include a `warning` explaining this policy.
Any future managed pre-pull must be a separate explicit operation with scoped
ownership and no automatic cleanup; it is not implemented here.

## Next Implementation Steps

1. Qualify measured coefficient profiles and verify the custom HyperShift sizing projection live.
2. Verify live monitor/TLS/exporter coverage, replica inventory, freshness, and recorder-PVC export on the target environment.
3. Add customer-infrastructure/payload integration and automatic future-HCP namespace discovery.
4. Install/verify bounded APF policy, collect HTTP classification receipts, and orchestrate baseline canaries and experiment phases.
5. Add a full remote workload/campaign driver, recorder timeline analysis, and operator-approved cleanup orchestration around scoped RP delete.

See [deferred work](../../docs/design/hcp-perf/deferred-work.md) for the separate
campaign driver, fault qualification, memory-pressure validation, and state-limit work.
