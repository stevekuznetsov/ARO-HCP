# Medium 150% Preparation

September 18, 2026. **Offline allocation and executable verified; no Medium
cluster, load, throughput result or qualified T-shirt size yet.** This is distinct
from historical Medium v1. The hosting-fee denominator is $0.25/HCP-hour, making
the new budget $0.375/HCP-hour.

## Verified Candidate

| Accounting | Retained Small | New Medium |
| --- | ---: | ---: |
| Controlled effective CPU | 3250m | 6970m |
| Offering effective CPU | 3720m | 7440m |
| Controlled charged memory | 13151Mi | 28401Mi |
| Offering charged memory | 15241Mi | 30491Mi |
| Offering scheduler requests | 3507m / 13989Mi | 7216m / 27860Mi |
| Modeled cost per HCP-hour | $0.187442718505859 | $0.374996124267578 |

All 92 container identities join the resolved Small allocation exactly. Weights
are Small-allocation-derived proportions, not newly measured usage. Fixed
downstream 470m/2090Mi is included once. Memory preservation and fixed-memory
overrides are removed; protected floors, replicas, init accounting, overhead and
quota policy remain. Price is the retained D16s_v6 $0.806/hour snapshot, not a
claim about actual deployment cost or D4s_v3 performance.

CPU limits equal requests; memory limits are rounded 110%; Go memory targets are
80%. Existing five GOMAXPROCS=1 overrides remain. CPO has no explicit override;
its future live runtime is unverified.

Private authoritative artifacts, relative to the preserved perf-runner worktree:

- `_artifacts/medium150-20260918-v3/input.json`
- `_artifacts/medium150-20260918-v3/native/ledger.json`
- `_artifacts/medium150-20260918-v3/comparison.json` and `comparison.md`
- `_artifacts/medium150-20260918-v3/HANDOFF.md`, source archive and hash manifests

The final binary SHA256 is
`aaae6afe88ead2ebfba54e2ff1bdfe857835f37376fa57da067ef387641045a2`.
Input SHA256 is
`4f06cc0128dc01fce6c59f8dec67a8437d4850f0500c25ba0d381154b4bc8650`.
Ledger SHA256 is
`c6e6e8cd9ff76416b4d6fb28140eba5d6f94e7e7e2eb032b30391f8f33dbbdb9`.

## Verification

The final combined source passed `GOWORK=off go test -race -count=1 ./...`
(30 packages), `go vet ./...`, repeated isolation and critical-safety tests,
native allocation/check, and an independent ledger/runtime audit. All eight
allocation outputs match the prior verified candidate byte-for-byte.

Timing-sensitive isolation/metrics tests now wait for bounded outcomes instead
of assuming fast persistent storage. Chrome tests pass with a short persistent
temporary path. No tests were skipped to obtain these results. Native startup
now rejects renamed singleton sizing configurations and HC selectors that would
hide retained clusters during a management-wide policy update.

The production critical monitor remains 5s cadence, 4s cycle deadline and 15s
evidence-gap threshold. Offline tests are not live safety qualification. The new
binary has not been promoted to the running controller.

## Cleanup Results

Exactly eight reviewed old metric subtrees were retired after isolated Small
dashboard reproduction, full inventories, live-dependency checks and consistent
SQLite backups. The exact paths and per-file receipts are retained in
`_artifacts/cleanup-execution-20260918-local/final/receipt.json`. Three separately
reviewed regenerable cache/scratch roots were also removed.

- Evidence deletion increased filesystem availability by 1,095,675,904 bytes.
- Cache deletion increased availability by 123,252,736 bytes.
- Completion availability was 16,823,189,504 bytes, about 15.67 GiB, not 40 GiB.
- All 233 Small input hashes and 4,781 retained release copies verified afterward.
- Post-deletion Small export/render passed; decoded JSON equals the maintained
  dataset. Failed-health and partial-capture caveats remain unchanged.
- Taildrive was temporarily unpublished, verified orphan serving processes were
  stopped, and the exact share restored with one serving child.

The 27.803 GiB file-block total was not physical reclaimable storage. Compression
is confirmed; exact compressed physical accounting and complete snapshot
enumeration require privileged read-only inspection. No snapshot deletion or
filesystem balance was performed. Diagnosis is retained in
`_artifacts/storage-diagnosis-20260918T0334Z-tf100c/HANDOFF.md`.

Full historical reanalysis of the eight retired streams is no longer promised.
Only explicitly preserved raw exceptions remain in the release bundle; citation
hashes alone are not backups. Current Small report reproduction is unaffected.

## Revised Scope And Progress

User clarification supersedes the original 40 GiB headroom, retained-Small
coexistence and identical-catalog requirements. Maintain useful disk reserve and
monitor growth rather than chase a fixed free-space number. Exact OpenShift
4.21.32 remains required; catalog changes are allowed and actual digests must be
recorded. Do not call differing catalogs a strictly resource-only comparison.
Management nodes must remain unchanged for now.

Additional reviewed build-cache/scratch cleanup recovered an observed
693,198,848 bytes. Small re-export and all report/release hashes still pass.
Approximately 16.2 GiB remained available during lifecycle work. Details and
trend-based monitoring guidance are in
`_artifacts/cleanup-bounded-20260918-local/HANDOFF.md`. No fixed 40 GiB gate remains.

Small was deleted through the normal native RP lifecycle in run
`small75-delete-20260918T1125Z`. The delete action SUCCEEDED; the overall run is
STOPPED because its keeper was intentionally stopped after verified completion.
RP and CS return 404. Both namespaces, HC/HCP, downstream objects, Maestro bundles,
managed RG and customer-network associations are gone. Customer infrastructure and
the personal underlay remain. Environment claims are released, with no active
iteration workers or generators. Report evidence remains preserved.

Authoritative receipt and post-deletion capacity assessment:
`_artifacts/sequential-live-20260918T1128Z/HANDOFF.md` and `placement-final.json`.

## Remaining Placement Gate

Medium has NOT been submitted. Actual post-Small placement, using current Pod
templates, init semantics, selectors, tolerations and baseline requests, shows:

| Worker memory accounting | MiB |
| --- | ---: |
| Available on the three eligible worker hosts | 27220.17 |
| Medium steady demand restricted to those hosts | 27824 |
| Deficit before bootstrap and rollout headroom | 603.83 |

All fixed downstream demand is management-side and counted once. Two CSI
operators totaling 100 MiB may use the system node and are already excluded from
the worker demand above. Other control-plane workloads cannot use system/infra
spare capacity because of hard taints and missing tolerations. Three worker Swift
slots are now free. CPU is not the aggregate blocker; memory already fails the
necessary fit check before bin packing or HA constraints.

No nodes, pools, taints, tolerations or resource requests were changed to evade
this result. No autoscale event was used as capacity approval. Proceeding requires
an explicit capacity/placement decision, followed by a new fit and startup-headroom
check. Routing to the same management cluster and unchanged CSC preconditions
were verified; the prepared native startup spec remains unsubmitted.

Fresh capture and actual Medium identity/credential binding remain necessary
after successful startup. The offline native workflow is prepared in
`_artifacts/medium-performance-offline-20260918/WORKFLOW.md`; its drafts are
deliberately non-submittable until those live bindings exist. Historical capture
growth was about 0.79 GiB/hour, excluding native case outputs; measure a new pilot
and monitor filesystem reserve before extending a run.

## Future Parallel Sizing

The gated ARM tag `aro-hcp.experimental.cluster.size-override` exists, but this
checkout accepts only `Minimal`, mapping to CS property
`hosted_cluster_size_override=e2e_minimal` and HC annotation
`hypershift.openshift.io/cluster-size-override=e2e_minimal`.

The admin desired-control-plane-size endpoint already accepts `Medium`, `Large`
and `Xlarge`, which map to `medium`, `large` and `xlarge`. This override takes
precedence over the experimental ARM tag. Parallel experiments also need a single
reviewed CSC containing the distinct class policies; the current allocator emits
one class named `small` even when its allocation is Medium. A forced class does
not provision capacity. No future parallel-sizing changes were made here.

See [Native Startup Isolation](../NATIVE-STARTUP-ISOLATION.md) for source capability
details; its original retained-Small/initial-pin experiment constraints are
superseded by the revised scope above, not new launch requirements.

The native delete keeper refreshed the approved RGs at 11:29:25 UTC, preserving
other tags. It is now stopped; the Small managed RG was subsequently deleted by
its normal owner. Auth rotation's inspected service bound is approximately
15:41 UTC. Recheck current identity/token expiry before resuming; never reuse
Small's guest credential for Medium.

## Next Measurement

After all gates pass, follow the original handoff: fresh native startup recording,
sustained availability, three guest workers, quiet validation, then 60 applications
/ 300 Pods at 5 QPS with 2 QPS / burst 2 seeding. Use separate five-minute screens
to bracket request-quality failure, followed by a selected 3600s diagnostic window
and 600s recovery. Measure the matched zero-application-Pod baseline separately.
No capacity, health, contention or hour result can be inferred from this allocation.
