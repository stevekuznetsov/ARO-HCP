# HyperShift Sizing Policy Design

## Status and Boundary

This is the upstream design needed by the [HCP case runner](README.md), not an implementation, shipped API proposal with finalized field names, or production sizing recommendation. The runner resolves a versioned experimental profile; HyperShift must authoritatively reconcile it. Do not implement this by patching live Deployments against their owner or adding an admission webhook.

Cover **every HyperShift-owned regular container, init container, and sidecar**, including restartable init sidecars, injected helpers, CPO/CAPI components managed by HO, ignition, and direct Job paths. Workloads produced by other operators are measured and inventoried but not tuned here. In particular, starting CNO or a CSI operator through HyperShift does not bring that operator's downstream workloads into policy scope.

## Resource Model

Use per-KAS-container CPU and memory requests as independent anchors. The exploratory small starting point is **250m CPU and 1Gi memory per KAS replica**, not a total for all replicas and not a demonstrated sufficient allocation. Replicas and topology remain explicit case configuration.

For each workload/container identity `i`, resolve independently:

```text
cpuRequest_i = roundUp(max(cpuFloor_i, cpuRatio_i * kasCPU), cpuQuantum_i)
memoryRequest_i = roundUp(max(memoryFloor_i, memoryRatio_i * kasMemory), memoryQuantum_i)
goMemLimit_i = floor(0.80 * memoryRequest_i_bytes)  # Go processes only
memoryLimit_i = roundUp(safetyMultiplier_i * memoryRequest_i, memoryQuantum_i)
cpuLimit_i = absent
```

The profile explicitly versions ratios, floors, rounding quanta, runtime classification, and any exceptions for each workload/container. Keys must identify workload kind and component/workload identity plus container role/name, not a global container-name match. Common names such as `manager`, proxies, or probers must not accidentally share a policy. Validate duplicate, unmatched, and missing entries against the actual release inventory; absence of coverage is not permission to use a silent generic fallback. Include startup-only and conditional workloads in coverage tests.

Use measured CPU p95 as the seed for request estimates, round it to the declared granularity, and preserve the source window and sampling method. Do not apply a blanket safety multiplier to CPU requests. CPU and memory ratios are independent hypotheses. Larger anchors must test whether the ratios remain valid; neither linear scaling nor validity at upper sizes follows from a small-case result. Floors prevent tiny helpers and startup tasks from scaling below a meaningful allocation.

`GOMEMLIMIT` is 80% of the resolved memory **request**, not of the safety limit, for containers whose relevant process is Go. Emit Go runtime byte syntax (`B`, `KiB`, `MiB`, `GiB`), not Kubernetes quantity syntax such as `1Gi`. For example, 80% of 1Gi rounded down is `858993459B`. It is a soft runtime-managed-memory target, not a hard bound on process RSS or cgroup charge. Runtime identity must be explicit; do not assume every image is Go or that every process in an image obeys one setting.

The memory safety-limit multiplier is independently configurable; **3** is an exploratory example, not a production default or request-sizing multiplier. Validate it is sufficient to produce a legal limit at least as large as the request, and record resolved values. Crossing the request or memory target only records an overrun for offline analysis. A roomy safety limit allows footprint discovery without treating the request as a hard memory ceiling; eventual request-equals-limit validation is [deferred](deferred-work.md#memory-pressure-and-hard-limits).

Do not set CPU limits on covered HCP containers, including helpers. Preserve unrelated extended-resource requirements and their required request/limit equality. `GOMAXPROCS` is an optional, explicit experimental setting with **no profile default**. In particular, do not derive `GOMAXPROCS=1` from a sub-core CPU request: the revised agreement is to preserve CPU bursts and parallelism. Record the effective runtime behavior and existing component settings; omission by this policy is not proof of unlimited runtime parallelism.

## Historical Evidence

The September 8 samples discussed during design came from **lightly populated e2e activity, not an idle cluster**. The node-pool-upgrade interval was approximately 15:11-15:52 UTC. Approximate observations were:

| Component | Working set | CPU p95 |
| --- | --- | --- |
| KAS | 1197Mi | 142m |
| etcd | 214Mi | 77m |
| KCM | 148Mi | 25m |

These are context for experiments, not a reproducible dataset embedded in this document or evidence for a shipping profile. Recover source queries, series identity, replica aggregation, and exact windows before using them quantitatively. In particular, KAS working set above the proposed 1Gi request does not prove retained live heap above 1Gi. Compare cgroup total/anon/file/kernel, RSS, working set, Go `Sys - HeapReleased`, live heap, GC behavior, and profile `inuse_space` with their distinct definitions. Intrusive heap/profile collection must be outside scoring.

## Existing Upstream Trace

The following paths are relative to the **`openshift/hypershift` repository**, not ARO-HCP. They were inspected in the local upstream checkout at base revision `badd40555328303ab0730ce65d28dfce43983f76`; a local checkout is design context, not proof that a release image contains the same behavior. Revalidate against each tested HO/CPO image revision.

| Upstream path | Existing behavior and design relevance |
| --- | --- |
| `api/scheduling/v1alpha1/clustersizingconfiguration_types.go` | `Effects.ResourceRequests` holds `ResourceRequest` entries with `DeploymentName`, `ContainerName`, optional CPU and memory requests. `Effects.KASGoMemLimit` is KAS-specific. These fields do not provide general CPU/memory limits or per-container runtime policy. |
| `hypershift-operator/controllers/scheduler/util/scheduler.go` | Resolves size effects to HostedCluster annotations. `ResourceRequestsToOverrideAnnotations` encodes workload/container request overrides; KAS Go-memory-limit handling is separate. Writing present effects alone is insufficient to define removal semantics. |
| `hypershift-operator/controllers/hostedcluster/hostedcluster_controller.go` | Mirrors selected annotations and the request-override prefix from HostedCluster (HC) to HostedControlPlane (HCP). Prefix synchronization removes stale mirrored values, but cannot remove stale source annotations the scheduler leaves on HC. Also reconciles CPO/CAPI through the component framework. |
| `support/controlplane-component/defaults.go` | Preserves existing regular-container resources, then applies request overrides after helper injection. `applyRequestsOverrides` matches workload and container and traverses regular/init containers. A generalized final policy pass belongs after existing overrides, with deliberate precedence and removal behavior. |
| `control-plane-operator/controllers/hostedcontrolplane/v2/` | Component builders/templates cover CPO, CAPI manager/provider, ignition server and proxy, etcd StatefulSets, and other components. HO uses shared component implementations too; a CPO-only change is not complete coverage. |
| `hypershift-operator/controllers/hostedcluster/etcd_recovery.go` | Direct etcd-recovery Job construction needs an explicit policy hook; framework-only changes do not cover it automatically. Related manifests are in `hypershift-operator/controllers/manifests/etcdrecovery/manifests.go`. |

Legacy paths described as `preserveResources`, and the current framework's equivalent existing-resource preservation, require special attention. Merely deleting an annotation can leave the previously reconciled requests/limits in the live object. An additive policy pass without an ownership-aware reset is not reversible.

ARO integration context is [HO configuration](../../../hypershiftoperator/), [configuration guidance](../../configuration.md), and [size override operations](../../ops/override-cluster-size.md). These references explain current integration; they do not imply that the generalized policy below already exists in ARO or upstream.

## Proposed Upstream Capability

Design a named, explicitly activated **container resource and runtime policy** capability. This is a design name, not an invented existing `Effects` field or annotation schema. Upstream API review must decide the concrete schema, transport/version identification, validation, and support signaling before implementation. The necessary semantics are:

1. Accept explicit workload/container CPU and memory requests, memory safety limits, CPU-limit removal, Go-memory targets, and optional runtime settings. Preserve unrelated resources and environment entries. Do not overload the existing request-only schema to pretend it supports limits or arbitrary environment policy.
2. Carry the resolved policy and its version/activation identity through scheduler size selection, HC annotations or the agreed API transport, the hostedcluster mirror, and all consuming HO/CPO reconciliation paths. Report unsupported consumers instead of silently claiming successful activation.
3. Apply policy after component defaults, adaptation, helper injection, resource preservation, and existing request overrides so all resulting regular/init/sidecar containers are covered. Add an equivalent hook to direct Job construction. Keep rendering deterministic and reconciliation idempotent.
4. With policy activated, the resolved policy is authoritative for its managed fields, including KAS `GOMEMLIMIT`, even when the legacy KAS Go-memory annotation is present. Without activation, preserve existing legacy behavior. This is deliberate precedence for a shipped setting, not a second competing source of truth.
5. Removing an entry or disabling policy removes its owned effects at every stage. Restore values from current component defaults and still-active legacy overrides, not stale resources copied from the live Pod template. Removing an explicit `GOMAXPROCS` restores underlying behavior; removing policy-managed CPU limits must not remove extended-resource limits. Track sufficient field provenance to distinguish policy effects from unrelated configuration.
6. Ensure policy is effective from first constrained startup. Compare desired output to controller templates and actual Pods, including UID/revision and conditional startup Jobs. A missing/mismatched container blocks a claim of complete policy coverage rather than prompting admission mutation.

The profile resolver lives in the proposed ARO CLI; authoritative rendering belongs in upstream HyperShift. Keep a clean boundary between profile arithmetic and upstream reconciliation. The initial runner may report unsupported policy until custom images provide it; it must not advertise nonexistent limits/runtime fields as available today.

## Verification Required

Upstream implementation must test resolution/validation and render coverage for every owned workload/container role, including injected sidecars, init containers, ignition, HO-created CPO/CAPI, etcd and recovery Jobs. Check no CPU limits, unrelated-resource preservation, Go byte formatting, optional `GOMAXPROCS`, and deterministic reconciliation.

Test activation, edits, per-entry removal, full deactivation, and legacy-KAS precedence with existing live objects whose resources were previously preserved. Verify stale values are removed from HC, HCP, templates, and replacement Pods; test reconciliation order in both directions. Include mixed old/new HO and CPO versions, upgrade and rollback: unsupported combinations must not be scored as though the policy applied. This is essential because HO and CPO images are independently selected and may implement different parts of the pipeline.

Actual ratio/floor calibration, upper-size validation, and production hard-limit safety remain experimental work, not conclusions of this design. The first evidence-producing runner should make those questions answerable without embedding an optimizer.
