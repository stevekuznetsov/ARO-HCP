# Personal HyperShift Sizing

`sizing hypershift` is an offline bridge from a resolved experiment profile to
`scheduling.hypershift.openshift.io/v1alpha1`, kind `ClusterSizingConfiguration`,
name `cluster`. It does not contact Kubernetes, deploy images, or apply policy.
No `config/config.yaml`, config schema, Helm template, or rendered fixture changes
are needed. The experiment owner selects a profile and saves the rendered artifact.

## Render A Candidate

Run from `tooling/hcp-perf`; `$RUN/inputs` must already exist:

```bash
GOWORK=off go run . sizing hypershift --profile examples/small-complete.json \
  > "$RUN/inputs/cluster-sizing.json"
```

Only use the artifact if the command succeeds. Optional `--default-cpu-milli`
and `--default-memory-mib` flags select fallback requests (defaults: 10 and 64).
The library primitive is `sizing.ToHyperShift(resolved, defaultsBudget)`.
`sizing resolve` still emits experiment JSON, not a Kubernetes object.

The wire schema follows the local `hypershift-perf-sizing` worktree's
`api/scheduling/v1alpha1/clustersizingconfiguration_types.go`. Its policy is
`spec.sizes[].effects.containerResourcePolicy`, with explicit `defaultRequests`,
`goMemoryLimitPercent`, either `memoryLimitMultiplier` or `memoryLimitPercent`,
and a `containers` override list. `cpuLimitEqualsRequest` is an optional opt-in.
Overrides contain only `workload`, `container`, `requests`, and optional
`goMemoryLimitPercent`/`goMaxProcs`. They are not legacy `resourceRequests` entries.

The renderer uses one `small` class with `criteria.from: 0` and **no `to`**. Both
the CRD and interval validator require coverage through infinity; a lone
`0..999998` interval is invalid. The profile name is provenance, not the size
class name. Concurrency is 5 with a `0s` window, transition delays are `0s` increase
and `20m` decrease, and placeholders are zero. Capacity is deliberately omitted:
it describes management-node capacity, not summed container requests. This is a
single-candidate node-count policy, not resource-based autoscaling or a node-pool
capacity/scheduling configuration.

Read/mutating inflight limits are initially 150/50 (total 200), matching the
existing small-class starting values. They are provisional, not a measured APF
budget; installing/verifying APF remains separate.

## Candidate Budgets

`small-complete.json` encodes the proposed initial small budgets, not a qualified
minimum. Ratios scale independently with the KAS CPU/memory anchor and floors
prevent shrinking below the listed requests. Quanta are 1m/1Mi.

| Main Container | CPU | Memory |
| --- | --- | --- |
| kube-apiserver | 250m | 1024Mi |
| etcd | 100m | 256Mi |
| kube-controller-manager | 30m | 192Mi |
| openshift-apiserver | 40m | 256Mi |
| openshift-controller-manager | 10m | 128Mi |
| cluster-policy-controller | 10m | 128Mi |
| kube-scheduler | 10m | 64Mi |
| packageserver | 125m | 320Mi |
| control-plane-operator | 550m | 128Mi |
| hosted-cluster-config-operator | 25m | 128Mi |
| ignition-server | 150m | 128Mi |

Workload/container names were checked against HyperShift's component assets under
`control-plane-operator/controllers/hostedcontrolplane/v2/assets`. The ignition
budget is a provisional startup allowance, not a measurement-derived minimum.
The existing `small.json` remains an incomplete coefficient illustration.

"Complete" means fallback policy coverage for every regular/init container
passing through the HyperShift projector, including injected helpers. It does
not mean an exhaustive inventory or evidence of viability. Unlisted containers
receive 10m/64Mi, GOMEMLIMIT at 80% (53687091 bytes, approximately 51.2MiB), and a
192Mi memory safety limit. Listed containers use the same percentage and 3x
safety multiplier. The projector removes CPU limits and preserves unrelated
resource fields. Startup tools, runtime init containers, and helpers may need
128Mi or more; add measured overrides rather than assuming 64Mi is sufficient.
The fallback does not configure management services or workloads outside the
projector's ownership. Check actual templates and Pods, not just the CSC.

A container may set `"goMemoryLimitPercent": 0` to disable GOMEMLIMIT management
(the projector removes that environment variable), for example for a non-Go
binary. Values 1..100 override the profile percentage. Omission inherits it.
`goMaxProcs` is optional and is never derived from CPU requests; omission preserves
the underlying manifest/runtime behavior. The general resolver accepts up to
MaxInt32, but HyperShift rendering rejects values above 1024. Rendering also
rejects safety ratios not representable as whole percentages, ratios above 16, overflowing memory,
and duplicate workload/container keys even if their kind or init identity differs.
HyperShift cannot distinguish those identities; one entry applies to either
regular or init containers of that name. The renderer emits limit policy settings,
not literal limits or environment strings in the API object; the projector derives them.

## Constrained Limits

The offline resolver and experimental projector now support this explicit profile
opt-in, independently of candidate request budgets:

```json
{
  "cpuLimitEqualsRequest": true,
  "safetyLimit": {"numerator": 11, "denominator": 10}
}
```

Use memory rounding quantum `1` MiB. The renderer emits `memoryLimitPercent: 110`
and `cpuLimitEqualsRequest: true`. Every projected regular, transient init, and
restartable init container receives CPU limit=request and memory
`ceil(request * 110 / 100 / MiB) * MiB`. For example, 64Mi becomes 71Mi and
1024Mi becomes 1127Mi, never 70Mi or 1126Mi. Overflow is rejected before mutation.
Resolver rounding that cannot be represented by the projector is rejected.

Existing saved multiplier policies and historical examples remain unchanged:
integer safety ratios still render `memoryLimitMultiplier`, omission of the CPU
opt-in still removes CPU limits, and absence of the entire policy preserves
legacy behavior. The CRD requires exactly one memory mode. This is necessary for
existing CSCs/HC annotations; neither an implicit default nor a renamed field is
used to reinterpret them. Old projectors do not support the new mode: do not
deploy a percentage policy before verifying both custom operator images.

GOMEMLIMIT stays a percentage of the **request**, not the hard limit. An explicit
per-container zero removes it for non-Go binaries, including init tools. Resource
limits still apply to these containers. No GOMAXPROCS value is derived from CPU:
existing explicit values/overrides are preserved. However Go 1.25+ container-aware
runtime defaults, or an image's automaxprocs library, may react to newly present
CPU quotas when no explicit value exists. Record the actual runtime/image behavior
and throttling; unchanged environment does not prove unchanged parallelism.

Static shared-anchor overrides need no new feature: for non-KAS containers use
zero CPU/memory ratios with denominator 1 and explicit floors, with 1m/1Mi quanta.
The required KAS entry remains unit-ratio with the agreed anchor. No corrected
candidate, examples, or analysis budgets were changed as part of this support.

### Coverage And Blocking Gap

**This does not yet enforce complete-HCP limits. Do not start a whole-HCP
constrained qualification with only this CSC.** The projector is not admission.

| Scope | Enforcement before creation |
| --- | --- |
| HO/CPO component-framework Deployments, StatefulSets, Jobs, CronJobs | Projector runs after adaptation/injection/legacy overrides, before template submission. Includes CPO, ignition, CAPI, router, catalogs and their helpers when rendered through that framework. |
| Initial HO reconciliation | Existing bootstrap publishes the selected zero-node/override policy before workload reconciliation; constrained opt-ins are tested through this path. |
| HO etcd-recovery Job | Explicit projection before creation, tested with constrained limits. Existing immutable jobs with drift report an error, not a patch or automatic restart. |
| Regular, transient init and restartable init in those templates | CPU and memory limits apply equally; non-Go exemption affects only GOMEMLIMIT. |
| Downstream operators' workloads, guest workloads, arbitrary Pods and instrumentation | Not projected merely because they share the namespace or appear in the override list. |

The saved `docs/cards/small-v1-resources.json` active inventory identifies these
unmarked downstream workloads (the snapshot excludes completed/transient init
containers, so it is not a complete startup inventory):

| Workloads | Canonical owner / investigated path |
| --- | --- |
| `azure-disk-csi-driver-operator`, `azure-file-csi-driver-operator` | Cluster Storage Operator. HyperShift `v2/storage/deployment.go` and its asset configure CSO images, credentials and UID; they do not propagate this policy to operands. |
| `azure-disk-csi-driver-controller`, `azure-file-csi-driver-controller` | Their CSI operators, including driver, attacher/provisioner/resizer/snapshotter, liveness and RBAC proxies. The vendored `operator/v1/ClusterCSIDriver` API has no general per-container resource field. No complete first-Pod resource override contract was verified for the selected release. |
| `csi-snapshot-controller` | Cluster CSI snapshot controller operator. HyperShift `v2/snapshotcontroller/deployment.go` configures image/version/UID, not operand resource limits. |
| `ovnkube-control-plane`, `cloud-network-config-controller`, `multus-admission-controller`, `network-node-identity` | Cluster Network Operator. HyperShift `v2/cno/deployment.go` configures images/credentials/UID, not these limits. |
| `kube-state-metrics-hcp` | Experiment instrumentation, outside the projector; campaign owner must budget/control it separately or explicitly exclude it from product scope. |

[CNO's current OVN bootstrap](https://github.com/openshift/cluster-network-operator/blob/master/pkg/network/ovn_kubernetes.go)
reads back existing Deployment requests for the main, token-minter and socks-proxy
containers. That is request preservation, not a canonical first-creation limit
configuration, and not support for the other downstream workloads. Upstream source
inspection is not proof of the experiment's release-image capability. CSI source
API discovery was unavailable during this pass; do not interpret that as proof
that no release-specific unsupported override exists.

Complete enforcement needs either verified pre-creation owning-operator changes
for every operand and helper, or a separately delivered fail-closed Pod admission
path in the management cluster. Neither is implemented here. Patching reconciled
Deployments after startup, LimitRange defaults, and resource guards cannot provide
the required guarantee. The existing HyperShift `contrib/gomaxprocs-webhook` only
sets GOMAXPROCS and has `failurePolicy: Ignore`; it is not suitable as-is.

A viable admission delivery must be ready before any target Pod can be created,
select the new namespace from creation (including HO bootstrap Pods), use the
agreed policy and exact workload/container mapping with UID-verified owner chains,
reject unavailable/ambiguous policy or owner lookups, and cover both container
lists after sidecar injection. It must not trust a policy-hash annotation as
evidence of actual limits. It also needs working management-cluster TLS/RBAC,
failurePolicy Fail, reinvocation/validation against later mutations, and an
explicit unknown-container policy. AKS cannot assume OpenShift service-ca exists.
These are delivery blockers, not features supplied by the current projector.

### Image Capability Gate

Unit tests and CRD admission prove source semantics, **not image capability or
fresh-start viability**. No images were built and no live configuration changed.
Before a fresh qualification, the campaign owner must retain evidence of:

1. Pinned HO and independently selected CPO image digests/source revisions, live
   CRD retention of both fields, and identical selected policy on CSC, HC and HCP.
2. A pre-creation capability probe under those images showing 64Mi -> 71Mi and
   CPU=request on actual regular/init Pods, with a non-Go exclusion and preserved
   explicit GOMAXPROCS. Schema presence or an operator startup log is insufficient.
3. Downstream enforcement ready before creation, with positive tests for CSI/CNO
   and init containers, plus negative tests demonstrating creation fails when its
   policy/owner lookup or admission service is unavailable. No unconstrained
   bootstrap interval may be hidden by a later rollout.
4. A fresh HCP's complete Pod inventory (including completed startup Jobs and init
   statuses), admitted limits, imageIDs, owner UIDs, runtime behavior, OOM/restart
   history, throttling and readiness. Steady-state measurements under unlimited
   CPU or 3x memory cannot qualify the 110% constrained startup.

## Catalog V2 Candidate

`examples/small-catalog-v2.json` is an exact copy of the `small-complete.json`
parent settings and eleven entries, with a new name/description and six explicit
additions. The original profile and example above remain unchanged. This is a
**known-startup-OOM follow-up, not a qualified minimum or a proven fix**.

Attempt 4 (`perf-1node-d`, 2026-09-09) recorded
`certified-operators-catalog/registry` terminating `OOMKilled`, exit 137, at
08:08:27Z with a 64Mi request, 192Mi memory limit, and GOMEMLIMIT 53687091 bytes.
The local evidence is in `_artifacts/perf-single-node/attempt-4/`:
`blocker-diagnosis.json`, `blocker-projection.json`, and `catalog-memory.json`.
The 15-second working-set samples can miss transient peaks; the OOM status is
termination evidence, not a measured peak or proof of the minimum needed.

The supplied historical per-container `registry` summary was approximately 177Mi
working set at a 420Mi request, with initial CPU peak 134.9m and p95 40.9m. That
generic container name combines different catalogs and is not a per-catalog
startup qualification. The live OOM shows the old fallback is insufficient for
this catalog's startup, even if sampled working set looks smaller. The provisional
50m CPU request is above that historical p95; CPU remains unlimited for bursts.

Read-only Deployment/Pod metadata from the personal management cluster confirmed
these identities and images in namespace
`ocm-arohcppers-2snc7uedci4k1mq5kt3mbv9g8ls4q6mb-perf-1node-d`.
All three live registry containers execute `/bin/opm serve
/extracted-catalog/catalog --cache-dir=/extracted-catalog/cache`, expose catalog
gRPC, and still have 10m/64Mi requests and 192Mi limits. No Secret content was read.
Image references below are under `arohcpocpdev.azurecr.io/redhat/`:

| Deployment / Container | Observed Image |
| --- | --- |
| community-operators-catalog / registry | `community-operator-index@sha256:99d027ec450ac3769e943a20595b00100a5091fb3e405ec0edeb925f3426b09a` |
| certified-operators-catalog / registry | `certified-operator-index@sha256:d608943f54dab7f34ee292e3ae19d9a1796e7a7081f7c9d75479bcc763ba0731` |
| redhat-operators-catalog / registry | `redhat-operator-index@sha256:b4dccf0e4d7c43f1df26bf2affc343522171238ac1f469fa9f9b7e4b99daa00d` |
| redhat-marketplace-catalog / registry | Absent from this live HCP; explicitly covered if present in the selected release |

The first three names match HyperShift's `v2/assets/<workload>/deployment.yaml`
at base revision `badd40555328303ab0730ce65d28dfce43983f76`. Marketplace has no
current asset: `hostedcontrolplane_controller.go` deletes the deprecated
`redhat-marketplace-catalog` Deployment. Its explicitly requested policy entry
does not create it. Overrides use exact workload/container names, not a generic
`registry` match; `catalog-operator` and catalog init helpers retain their budgets.

All four registry entries request **50m/256Mi**, with **768Mi** safety limits.
They inherit the profile's 80% Go target: **214748364 bytes**, approximately
204.8MiB, computed from the request, not the safety limit. `opm` is a Go catalog
server, so this target applies to its Go runtime, but is soft and does not bound
total container memory or guarantee OOM avoidance.

Live metadata and assets also identify `ignition-server-proxy/haproxy` (bash
launching HAProxy) and `router/router` (`haproxy`) as non-Go runtime applications.
Their explicit `goMemoryLimitPercent: 0` removes the ignored Go setting while
retaining fixed 10m/64Mi requests and 192Mi limits. These are not new memory
allowances. Unknown helpers remain default-controlled; for example catalog
`extract-utilities` runs non-Go `cp`, which ignores GOMEMLIMIT. Default injection
is not evidence that every helper is Go, nor does it manage non-Go memory.

KAS stays 250m/1024Mi; all eleven parent entries, 10m/64Mi fallback, 3x multiplier,
80% default Go ratio, inflight limits, and scheduling settings are unchanged.
For the three live single-replica catalogs, the request delta is +120m/+576Mi
(+160m/+768Mi if all four are present). No other resource budgets increase.

Render from `tooling/hcp-perf`, with the artifact directory already present:

```bash
GOWORK=off go run . sizing hypershift --profile examples/small-catalog-v2.json \
  > ../../_artifacts/perf-single-node/candidate-v2.json
```

No anchor, ratio, or fallback CLI overrides are needed. **Do not apply this to the
existing HCP.** Preserve attempt 4, verify its deletion through the owning APIs,
then consider the artifact only for the next fresh attempt under the apply gates
below. The separate ingress/release incompatibility also needs resolution; this
profile does not fix it. The next guest node pool remains **one replica**, using
the existing `Standard_D8s_v5` (8 vCPU/32GiB) choice for memory headroom, not a
worker-count increase. Catalogs run on management nodes, not that guest worker;
neither VM sizing nor these budgets prove end-to-end capacity. No node pool is
created or resized by rendering, and no successful one-worker result is claimed.

## Explicit Apply Gate

This is **management-cluster-wide**, not namespace-scoped. Use only a dedicated,
authorized personal management cluster whose CSC the experiment owner explicitly
controls. Do not run this against shared environments. The parent runner/operator
must apply the saved artifact only after the custom HO is ready.

1. Deploy a pinned custom HO image containing the new scheduling CRD, scheduler
   propagation, and projector support. Independently select and verify a custom
   CPO image containing the same policy implementation; changing HO alone does
   not guarantee that the release-selected CPO changed. Record both digests and
   source revisions. Use the existing personal image override/deployment
   mechanisms, not global config edits. See the
   [design integration references](../../../docs/design/hcp-perf/README.md).
2. Verify the live CRD exposes `containerResourcePolicy` and its per-container
   runtime fields. An older CRD may prune the new policy; an older controller may
   ignore it. Schema presence alone is not proof of projector support.
3. For a new RP cluster, omit `aro-hcp.experimental.cluster.size-override: Minimal`
   from the payload. That tag maps to the explicit `e2e_minimal` size override,
   which does not exist in this single-class CSC. Ensure any Admin API desired
   size override is also cleared through its owning API. For existing clusters,
   clear source configuration through the RP/Admin API and wait for propagation;
   do not fight Maestro by editing HostedCluster tags/annotations directly.
   The separate Minimal availability-zone scheduling policy is not this size tag.
4. Finish Helm installation/upgrades before taking ownership. The existing
   `limitClusterSizes: true` chart installs fixed `e2e_minimal` requests and can
   overwrite the CSC again on a later pipeline run. No chart toggle is changed
   here. Recheck/reapply only under explicit experiment ownership after upgrades.

After these gates, the following is an **explicit mutation**, not part of render.
It replaces the entire spec, avoiding stale classes/effects retained by a merge.
The resourceVersion test protects against a concurrent CSC writer. Review the
backup and dry-run output before issuing the final command:

```bash
kubectl --kubeconfig "$MGMT_KUBECONFIG" get clustersizingconfiguration cluster -o json \
  > "$RUN/inputs/cluster-sizing-before.json"
jq -n --slurpfile before "$RUN/inputs/cluster-sizing-before.json" \
  --slurpfile desired "$RUN/inputs/cluster-sizing.json" \
  '[{op:"test",path:"/metadata/resourceVersion",value:$before[0].metadata.resourceVersion},
    {op:"replace",path:"/spec",value:$desired[0].spec}]' \
  > "$RUN/inputs/cluster-sizing-patch.json"
kubectl --kubeconfig "$MGMT_KUBECONFIG" patch clustersizingconfiguration cluster \
  --type=json --patch-file "$RUN/inputs/cluster-sizing-patch.json" \
  --dry-run=server -o json > "$RUN/inputs/cluster-sizing-preview.json"
```

Confirm the preview retains the complete desired spec, especially the new policy.
Only then, with explicit approval for this personal cluster:

```bash
kubectl --kubeconfig "$MGMT_KUBECONFIG" patch clustersizingconfiguration cluster \
  --type=json --patch-file "$RUN/inputs/cluster-sizing-patch.json"
```

If the resourceVersion test fails, inspect the concurrent change and generate a
new patch from a fresh read after resolving ownership. Never force past it.
Verify the persisted CSC spec and validation condition, the HostedCluster's
`hypershift.openshift.io/hosted-cluster-size` label (`small`), HC/HCP policy
annotations, template policy hashes, and actual regular/init container requests,
limits, and runtime settings. Wait for rollout convergence and record OOMs,
restarts, readiness, and startup peaks before scoring the candidate. A validation
condition alone can be stale and does not establish rollout completion.

Retain the pre-change spec for an explicitly approved rollback using the same
fresh-read/resourceVersion-checked spec-replacement procedure. There is no
automatic apply, rollback, or cleanup in this primitive.
