# Native Startup Isolation

Source capability review, September 18, 2026. This describes the native Go
implementation and the isolated HyperShift sizing source, not a live approval.
The original coexistence and identical-catalog constraints below are historical:
the user subsequently approved sequential Small replacement and catalog drift
while retaining exact OpenShift 4.21.32. Small was deleted through the native RP
lifecycle; management nodes remain unchanged. See the current
[Medium progress card](cards/medium150-preparation.md) for the measured placement
blocker. Initial catalog injection and parallel coexistence are future capabilities,
not requirements for the approved sequential experiment.

## Sizing Boundary

HyperShift uses one management-cluster-scoped `ClusterSizingConfiguration/cluster`.
There is **no per-HostedCluster CSC reference**. The experimental CRD requires that
exact name, and HO bootstrap, hosted-cluster sizing, and Azure scheduling all read
`cluster` explicitly. Renaming the budget output does not isolate its effects.

Native `candidate-apply` therefore requires `name: "cluster"`, an empty
`emptyHostedClusterSelector`, and no HostedClusters in the management cluster.
The legacy selector field remains decodable for existing frozen JSON, but any
nonempty selector is rejected before API access: hiding another HC cannot make
a management-wide update safe. `candidate-update` is still restricted to one
explicitly pinned existing HC. Neither action supports coexistence provisioning.

An HC can select a **size within that singleton** using
`hypershift.openshift.io/cluster-size-override`. That is not a CSC reference.
Adding another class requires a valid nonoverlapping partition of `[0,+inf)`,
preserving existing class effects and global settings, plus selection before
bootstrap. Both Small and Medium have zero workers initially and three later;
worker-count criteria alone cannot distinguish them. The RP experimental
`aro-hcp.experimental.cluster.size-override` tag accepts only `Minimal`, mapped
to `e2e_minimal`, not an arbitrary Medium class.
The admin/SPC desired-size path is separate and is not a native pre-create action.
Its handler calls `GetOrCreateServiceProviderCluster` and accepts `Medium`; the
route itself does not require a prior HC. This is a possible pre-create owner-side
selection path, not proof it is safe: parent must verify deployed availability,
authentication, preservation through RP creation, dispatch ordering and readback.
Do not infer those properties from the handler alone or write Cosmos directly.

Consequently the currently supported native sizing path is an **empty, isolated
management destination**, explicitly routed by the lifecycle owner. The existing
CSC apply/observe/create sequence works there without a new sizing API. Separate
node pools on the same management cluster do not isolate the singleton CSC.

## Historical Deployment Pods

Native Go startup observation and performance snapshot admission retain every Pod
in inventory and failure history. A Succeeded/Failed Pod is excluded only from the
current serving profile comparison when its controller ReplicaSet is bound by UID
to the declared Deployment, explicitly scaled to zero, and has a different Pod
template from the Deployment (ignoring only the generated pod-template-hash label).
Labels or an old image alone do not establish obsolete ownership. Current-revision
terminal Pods remain unhealthy even if their resources and images match.

Terminal Pods never count toward serving readiness. Pending, Running, Unknown and
deleting nonterminal Pods retain profile checks; unknown owners remain undeclared.
Controller replicas/convergence, endpoint checks, restart/OOM and probe history are
unchanged. Disabled workloads and explicit Job/CronJob/standalone Pod lifecycle
contracts are not exempted. This is not a UID dismissal protocol or a resource
budget exclusion based on empty requests, and does not authorize deleting evidence.

## Initial Catalogs

HyperShift already supports three HC annotations, mirrored by HO to HCP:

```text
hypershift.openshift.io/certified-operators-catalog-image
hypershift.openshift.io/community-operators-catalog-image
hypershift.openshift.io/redhat-operators-catalog-image
```

All three must be supplied as digests. CPO consumes them before rendering the
catalog Deployments, selects the same bytes for `registry` and `extract-content`,
and removes image-stream triggers. Annotations present on the **initial HC** can
therefore freeze initial catalog selection without changing CPO images.

The current RP create payload does not expose arbitrary HC annotations, and the
native runner has no atomic pre-create metadata injection action. Adding these
as arbitrary ARM tags does not propagate them; unknown experimental tags are
rejected. A post-create poll-and-patch is not an initial-configuration guarantee:
HO/CPO may already have reconciled. Patching a pause after creation has the same
race, and pausing a shared operator would affect Small.

The existing `catalog-pin` action intentionally does not solve this: it requires
an existing exact HC/HCP/namespace, exactly one HC in the management cluster,
and digest equality with already-selected registry/extract images. It freezes
the current selection, not a requested earlier selection or startup state.

The minimum missing capability is **initial HC annotation delivery at its owner**,
scoped to the exact new ARM identity. Prefer a verified CS provisioning property
if that service already exposes one; that service's implementation is not in this
repository. Otherwise the parent must approve an owner-side source change or a
target-scoped admission mechanism installed before creation. Such a mechanism
must fail closed, match the full ARM annotation, preserve all unrelated metadata,
reject conflicting pins, and be proven on CREATE before HO sees the object.
No sibling source, RP API, admission webhook, or live service was changed here.

## Placement Prerequisites

HC `spec.nodeSelector` and `spec.tolerations` are existing HyperShift inputs for
management-side control-plane placement. A request-serving additional selector
is narrower and is not whole-HCP isolation. Azure scheduling does not create
dedicated capacity from the AWS-specific node/placeholder path. Dedicated pools
alone also do not stop unselected Small Pods from landing there.

Before startup, the parent must verify:

- Three eligible zones/hosts for KAS and other HA constraints.
- Three free worker Swift NIC slots for routers, plus rollout needs.
- The Medium scheduler request envelope, daemon/infrastructure demand, init-stage
  demand, storage constraints and explicit rollout/surge headroom per zone.
- Selectors/tolerations on all actual HO/CPO/downstream templates, including
  bootstrap jobs, rather than assuming a common label isolates every workload.
- Stable approved node count/SKU during measurement; reactive autoscaling is not
  a capacity reservation. Pricing D16s_v6 does not establish D4s_v3 performance.

## Unattended Sequence

Once the parent resolves routing and initial metadata delivery, retain the existing
native order: cost, armed retention, guarded candidate, armed startup observation,
one RP create, sustained startup availability plus RP success, credential, three
workers, exact guest identity verification, fresh capture, then separate measured
cases. Reconcile accepted operations rather than retrying PUT/DELETE. Do not use
a second environment ID to bypass the lifecycle claim.

The Medium v2 frozen verification passed the full module race suite and vet;
the old v1 test failure is no longer a gate. New source changes still require a
new tested build/freeze before controller promotion. Neither a green test suite
nor these guards provide the missing initial-metadata delivery capability.
