# HCP Performance Case Runner

## Status and Scope

Agreed initial-work design, recorded 2026-09-08. This describes the target architecture, not a validated sizing recommendation. The standalone Go CLI lives in `tooling/hcp-perf`; its [README](../../../tooling/hcp-perf/README.md) tracks the implemented subset. Durable Prometheus, bounded Kubernetes snapshot recording, and CPU adversary Jobs are implemented; RP provisioning, the full case executor, and complete evidence-stream coverage remain to be implemented.

The first deliverable is a reproducible single-case runner, adversary scheduler, durable data capture, and offline analysis. It answers: under an explicit control-plane resource policy and measured CPU contention, can a fresh HCP start, converge, sustain useful work and protected progress, and recover? It does not search for an optimal population, APF configuration, or resource allocation. Availability and bounded backlog are the goal; the benchmark deadlines are not a customer latency SLO.

- [HyperShift sizing](hypershift-sizing.md) defines the resource model and required upstream design, not shipped API fields or implementation.
- [Deferred work](deferred-work.md) separates follow-on experiments from this initial stability scope.

HCP means hosted control plane; HO and CPO mean HyperShift Operator and control-plane operator; KAS and KCM mean kube-apiserver and kube-controller-manager; APF means API Priority and Fairness.

## Environment and Placement

Use an existing personal environment built through [personal-dev setup](../../personal-dev.md) and existing [deployment mechanisms](../../service-deployment-concept.md). Custom HO and CPO images must be independently selectable and recorded by digest. A custom HO image does not imply that the release-selected CPO image changed. Existing integration references include [HyperShift provisioning](../../../hack/ci/provision-hypershift.sh), [image override composition](../../../hack/ci/build-config-override.sh), and the [HO installer](../../../hypershiftoperator/deploy/templates/installer.job.yaml). These are reference points, not a request to introduce another environment provisioner.

Create a fresh HCP for every case through Resource Provider (RP) APIs, including its guest node pools when requested. Do not bypass the RP by creating a HostedCluster directly. Use the management-cluster kubeconfig for measurement, policy verification, and scoped faults; use guest access for applications and canaries. Resolve credentials at execution time and never put them in exported artifacts. See [creation flow](../../ops/hcp-cluster-creation-flow.md) and existing [RP test helpers](../../../test/util/).

Run durable orchestration, workload-driver execution, and capture services on **existing infrastructure pools**, with required node affinity and the matching tolerations. Toleration alone does not guarantee placement. Lightweight node-local collectors run on the existing worker pools hosting the measured workloads; they must not turn into heavyweight analysis or storage services. CPU competitors must be colocated on the relevant existing management worker nodes to contend with HCP processes. Guest application pods use their normal guest workers. Do not add dedicated performance pools. Record actual placements, node allocatable resources, taints, affinities, and collector overhead.

The orchestrator runs cases serially; no elaborate distributed locking scheme is needed. Preflight checks identify the target environment and conflicting experiments. Serial execution is not isolation: retained HCPs, unrelated tenants, autoscaling, node movement, and infrastructure activity remain visible confounders. Check and record headroom before the next case rather than silently deleting retained resources.

## Composable Primitives

The following command families describe intended responsibilities, not runnable command syntax or a finalized flag schema.

| Primitive | Contract |
| --- | --- |
| `sizing resolve` | Resolve a versioned profile and independent CPU/memory anchors into explicit desired values and a coverage report, without starting a case. |
| `capture start/status/stop/export` | Start a durable capture session, discover targets as they appear, expose health and gaps, stop collection explicitly, and export incremental evidence. |
| `adversaries start/status/stop` | Independently schedule bounded competitors or workload generators, expose intended and actual activity, and stop their active work. |
| `case run/status/stop` | Compose the primitives into the lifecycle below; return a durable case ID and structured status. Stop cancels work, not retained resources. |
| `analyze` | Read only exported artifacts; reproduce phase summaries, useful-work/backlog results, resource observations, and validity decisions. No live dependency. |
| `cleanup` | Explicitly select and delete retained case resources, and separately select evidence/session storage if desired. Never implicit in stop or analyze. |

Capture sessions have their own IDs, lifecycle, and storage. A case may attach to an existing session or create one; capture is not required to be case-owned. Record attachment, creation, and ownership decisions so case exit does not accidentally stop another caller's capture. Independent sessions can span startup, scored activity, recovery, and later debugging.

Agent state and evidence are persisted incrementally on a persistent volume (PV), not buffered on a laptop until success. The CLI submits, observes, and controls durable execution; disconnecting an observer is not case cancellation. Explicit stop or terminal case exit stops every active generator in that case's scope. Each workload Job also has an active deadline and each generator a self-enforced deadline, so loss of orchestration cannot produce indefinite load. Those deadlines expire load generation, not the HCP or applications.

Persist phase transitions, action IDs, deadlines, heartbeat/checkpoint state, shutdown acknowledgements, and ownership inventory. After an executor or generator worker crash, preserve the interrupted result and do not automatically rerun or restart the scored interval. Reattachment may collect surviving evidence, recover capture with declared gaps, and complete bounded shutdown. A restarted collector must report its missing interval rather than imply continuity.

## Case Inputs

A versioned case specification fixes the random seed, image digests, HCP/node configuration, sizing profile, APF candidate, application population, turnover schedule, outstanding-work bounds, competitor placement/weights, capture configuration, and all phase/deadline/safety settings. Resolve defaults into the artifact before execution. Record desired and observed values separately, including unsupported or missing capabilities. Keep sizing, APF, and competitor settings fixed during scoring; no adaptive optimization.

Preflight checks include authorization, source reachability, capture PV capacity, required infra placement, worker headroom, image availability, selected-policy support in both HO and CPO, and generator shutdown capability. Verify the resource policy can apply from first startup, not only after a generously sized HCP has warmed its caches. An empty case has no application population or worker Deployment canary; report those checks as not applicable, not passed.

## Case Lifecycle

| Phase | Required behavior |
| --- | --- |
| Capture | Start or attach capture and verify its health **before** HCP creation. Discover newly created objects by case identity and then UID. |
| Create | Create a fresh HCP through the RP under the resolved resource constraints. Capture startup failures as first-class results. |
| Populate | Bring up requested guest nodes, pre-pull the pinned application image, seed objects, and wait for explicit node/operator/application convergence within recorded bounds. |
| Baseline | Before competitors or turnover, complete five consecutive Deployment `0 -> 1 -> 0` canary cycles. Freeze stage-specific deadlines from these measurements. Empty cases omit this phase. |
| Calibrate | Start continuously runnable CPU competitors; calibrate fixed request weights from node allocatable CPU, reserved headroom, and actual runnable pressure. Record the resulting placement and service. |
| Settle | Wait a bounded, configured interval for stable competitor pressure and system convergence. Record its predicate, bound, and outcome. Do not settle indefinitely or retune during scoring. |
| Score | Run the fixed workload/adversary schedule for two hours with protected canary progress and broad capture. |
| Recover | For up to ten minutes, stop admitting new turnovers but retain CPU competitors. Let admitted work finish and observe backlog, protected progress, and component recovery. |
| Stop | Stop all active generators, including canary, turnover/retry workers, and competitors. Persist shutdown outcomes. Capture evidence and HCP/application objects remain available. |

Baseline stage definitions must be replayable from the timeline: acknowledgement is API acceptance of the intended scale mutation; scale-up is one ready/available replica at the intended generation, measured from the original scale-up intent and **including acknowledgement**; cleanup is completion of the scale-down to zero with canary pods gone, measured from scale-down intent. Record scale-down acknowledgement as well. No cycle overlaps the preceding cycle's cleanup.

For each stage `s`, let `B_s` be the slowest observation among the five consecutive baseline cycles. Set `deadline_s = min(3 * B_s, cap_s)`, with caps of 30 seconds for acknowledgement, 120 seconds for scale-up including acknowledgement, and 120 seconds for cleanup. Freeze these deadlines before competition. Failure to establish the baseline is a failed pre-score phase, not a reason to silently relax the cap. Canary cadence and its bounded concurrency are explicit case inputs.

Normal completion includes recovery, not just the two-hour window. A functional failure ends scoring, records the triggering condition, and enters bounded diagnostics/recovery where safe. Safety abort releases generators immediately rather than retaining competitors for recovery. User cancellation is recorded distinctly. Crossing a memory request/target is an observation only: it does not stop a case or change its policy. OOMs, component unavailability, failure to make protected progress, and worker failures are different events and must not be conflated with target overruns.

## Workload and Adversaries

### Causal Turnover

Use a pinned kube-burner version/image for deterministic **seeding only**. A custom turnover driver maintains causal identities across desired mutations, API attempts, and controller completion; kube-burner is not the scored lifecycle scheduler.

- Target 200 application pods per guest node: five replicas per application, plus a Service, ConfigMap, and Secret per application. Twenty applications per namespace gives 100 pods per namespace and two namespaces per node. System pods, canary pods, and rollout surge are additional headroom, not included in the 200.
- Stagger one replacement of each application's replica set per hour by changing a pod-template annotation. Pin deployment strategy and image so the intended operation and surge budget are reproducible. Do not delete all applications or burst all replacements at the hour boundary.
- Bound outstanding work globally and per identity. Never supersede an unfinished rollout with another update to the same application. Record overdue intended operations as backlog rather than dropping them or hiding them by slowing the intended schedule.
- Retries have bounded budgets and seeded jitter. For every logical operation, record intended time, each attempted request, API acknowledgement, observed generation/UID/revision, rollout completion or failure, and backlog changes. Separate useful completed rollouts from request volume and retry amplification.
- Use one namespace-scoped service account per application namespace with least-privilege RBAC; APF distinguishes users with `ByUser`. Use a separate, bounded, protected canary identity, not an exempt identity. Controllers and other system components retain normal system identities.
- Pre-pull the application image and verify guest worker CPU, memory, pod capacity, network/IP capacity, and rollout headroom. Record scheduling, image-pull, and worker resource delays so a worker bottleneck is not misreported as control-plane capacity.

### CPU Competition

CPU competitors remain continuously runnable and use fixed CPU **request weights**, not a feedback loop that pursues a CPU-utilization target throughout scoring. Allocate the competitor budget from each target node's allocatable resources after accounting for HCP placement, infrastructure/system use, collectors, and declared headroom. Record calibration and freeze the competitor configuration before the scored window.

HCP containers have no CPU limits. Requests influence scheduling and proportional CPU service under contention; they are not guaranteed entitlements, fixed slices, or ceilings. Measure actual CPU service, saturation, runnable pressure, throttling, node movement, and co-tenants. Missing contention or unexpected placement makes the intended experiment unverified, even if canaries pass. Memory competitors are not the primary adversary: discover the footprint first.

### APF Candidate

Supply an explicit APF candidate with the case and capture its desired and actual FlowSchemas, PriorityLevelConfigurations, and relevant KAS settings. Initially use reject-only admission for benchmark traffic and bounded queues for system and canary classes. The canary is protected but not exempt; benchmark overload must not be hidden by unlimited queues or privileged clients.

Record API status codes, retry guidance, APF response headers identifying the matched FlowSchema and priority level, concurrency/seat usage, queue/rejection metrics, and system/canary progress. Useful-work completion and backlog distinguish a healthy overload response from accepted requests that never converge. APF/resource optimization and deeper sensitivity studies are deferred; no candidate changes during the score.

## Evidence and Analysis

Capture broadly at a recorded cadence, keeping raw time series rather than only percentile summaries. Existing monitoring is context, not proof that every required stream is retained: see [HCP metrics architecture](../../sre/hcp-metrics.md), the [SRE allowlist](../../../hypershiftoperator/deploy/templates/sre-metrics-set.configmap.yaml), and [Prometheus configuration](../../../observability/prometheus/). Discover and document filtering, unsupported metrics, permission failures, and scrape gaps before making claims.

| Evidence | Required content |
| --- | --- |
| Manifest and provenance | Artifact/schema versions; resolved case and profile; seeds; source revisions and dirty-build provenance; tool, generator, HO, CPO, release and application image digests; target IDs; time sources. |
| Desired versus actual | Resource/runtime/APF policy, all container roles, workload owners and UIDs, pod revisions and restarts, node placement/allocatable/headroom, component versions, and drift over time. |
| Raw measurements | Per-container CPU service and throttling; node pressure; cgroup memory total/anon/file/kernel; RSS and working set; available Go runtime/GC/heap series; API/APF/etcd/controller/queue and worker metrics. Preserve units and metric definitions. |
| Logs, events, objects | Component and generator logs, Kubernetes events, conditions, status transitions, and allowlisted object metadata/spec fields needed to explain policy and progress. Include startup, failure, recovery, and shutdown. |
| Causal timeline | Phase boundaries, baseline samples/deadlines, competitor calibration, intended/attempted/acknowledged/completed work, retries, backlog, faults, failures, recovery, and terminal reasons. |
| Capture health | Target inventory, collection intervals, gaps and causes, clock uncertainty, collector resource overhead, storage pressure, dropped records, and recovered segments. |
| Retention inventory | Created versus attached resources/sessions, owner and UID, surviving HCP/apps/PVs, active-generator stop acknowledgements, unresolved shutdowns, and explicit cleanup targets. |

Exclude credentials, kubeconfigs, tokens, Secret bodies, and arbitrary workload bodies from artifacts. Use field allowlists and redaction at collection boundaries, including logs/events/metric labels that could leak sensitive values. Capture object identity, size/count summaries, and permitted status rather than copying arbitrary ConfigMap or application payloads. Creating benchmark Secrets does not authorize exporting their contents. Versioned seed templates permit reproduction without collecting live Secret bodies.

Memory quantities are not interchangeable. Cgroup charge includes anonymous, file, and kernel memory; RSS and working set use different accounting. Go's `Sys - HeapReleased`, live heap, and heap-profile `inuse_space` answer different questions and none equals cgroup usage. Correlate them instead of calling working set retained heap. Capture inexpensive runtime metrics during scoring; forced GC, heap/CPU profiles, and other intrusive debugging belong to explicitly marked intervals outside the scored window. Measure capture overhead on infra and node-local collectors.

Offline analysis must run from the exported bundle without live Prometheus, Kubernetes, Azure, or credentials. Reconstruct phase results and useful work/backlog alongside protected progress, pressure/service, memory trajectories, failures, and recovery. Report execution outcome, functional outcome, and evidence validity separately. A useful negative result is a structured failure with bounded diagnostics and a replayable timeline, not an unbounded hang or a deleted cluster. Never infer zero usage, stability, or a capacity bound from missing streams.

## Stop, Retention, and Cleanup

Every terminal case path performs bounded generator shutdown and records any failure to stop. Capture can remain running independently; stopping capture preserves its PV and exportable evidence. HCPs and applications remain for inspection until an explicit, separate cleanup request. The runner must not introduce automatic HCP expiry.

Cleanup uses the recorded ownership inventory, refuses ambiguous targets, and does not delete another caller's attached session or shared infrastructure. HCP deletion follows the RP lifecycle rather than deleting lower-level objects that controllers recreate. See [deletion troubleshooting](../../ops/cleanup-stuck-cluster-deletion.md). Evidence deletion is a distinct explicit choice. Existing personal-environment sweepers still apply independently of runner retention; check [personal-dev retention](../../personal-dev.md#full-personal-dev-environment-setup) and [sweeper policy](../../../tooling/cleanup-sweeper/resourcegroups.policy.yaml) before a long run.

## Initial Acceptance

The initial implementation is complete when a caller can resolve a policy, compose independent capture/adversary sessions, execute one serial fresh-HCP case through the specified phases, stop generators safely, export evidence after interruption, and reproduce the report offline. Verify empty and populated cases, failed startup/baseline, functional recovery, safety abort, cancellation, capture gaps, worker crash without score rerun, attached-session ownership, and explicit cleanup. Upstream policy support and complete actual-container coverage are prerequisites for claiming a constrained case; missing support must be reported, not silently substituted with manifest patching or admission mutation.
