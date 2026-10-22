# Admission Tuning At Fixed Resources

`workload setup` accepts an optional `admission` object in the existing setup
JSON. No new CLI flags, sizing changes, or automatic tuning are involved. Setup
creates only new run-owned resources; it never patches, adopts, or deletes an
existing priority level, FlowSchema, or system APF object. Use a fresh run ID and
matching seed namespace/identities for each treatment. A changed policy cannot
be applied by rerunning setup against an existing run.

## Policy

Omitting `admission` (or setting it to `null`) preserves the original PLCs exactly:

| Profile | Nominal Shares | Overflow | Queues | Hand Size | Queue Length |
| --- | --- | --- | --- | --- | --- |
| Benchmark | 50 | Reject | None | None | None |
| Canary | 5 | Queue | 8 | 2 | 10 |

Both are always `Limited`, with explicit `lendablePercent: 0` and omitted
`borrowingLimitPercent`. Zero lending does not disable borrowing from other
levels. Neither Exempt mode nor lending/borrowing controls are exposed. Shares
are relative to all APF levels, not fixed seats or a two-profile percentage.

To enable benchmark queuing, add this fragment to a fresh setup specification:

```json
{
  "admission": {
    "benchmark": {
      "nominalConcurrencyShares": 50,
      "queue": { "queueLengthLimit": 10 }
    },
    "canary": {
      "nominalConcurrencyShares": 5,
      "queue": { "queueLengthLimit": 10 }
    }
  }
}
```

This is an example treatment, not a recommended optimum. Omit the benchmark
`queue` (or set it to `null`) for Reject. Canary must have a queue. Only shares
and queue length are tunable; queue count remains 8 and hand size remains 2.
Queues and concurrency enforcement are per API-server replica.

Defaulting applies only to an absent whole policy. Within an explicit policy,
shares are ordinary int32 values: zero is valid and is never replaced with 50 or
5. Each share value must be in `[0,2147483647]`, and the two-profile sum must be
positive. An omitted share has the same zero value, so supply both explicitly
for review clarity. Zero canary shares does not guarantee useful canary service.
Every present queue must specify a length in `[1,2147483647]`; empty queues,
zero/negative lengths, and numeric overflow fail rather than silently defaulting.
These are API-domain bounds, not safe operating recommendations. The CLI rejects
unknown fields, including `type`, `exempt`, `rbacAllow`, and borrowing controls.

Run-derived PLC/FlowSchema names, exact benchmark service-account subjects,
canary identity, RBAC, workload resources, and ownership checks are unchanged.
Credential verification checks the desired local Role and binding, not APF
convergence; a valid admission treatment grants no additional permissions.

## Controlled Comparisons

The initial resource profile is a hypothesis, not an immutable service limit.
Prioritize APF/concurrency tuning for API-serving dependencies. If a background
controller or ancillary service demonstrably needs more resources, use an explicit
per-component override in a new sizing candidate and repeat at the same population.
Record derived Go targets, safety limits, replica-multiplied cost, and which serving
budgets stayed fixed. Do not mix a resource increase with an admission change in
the same causal comparison, or hide starvation by merely loosening health checks.

1. Freeze the parent validation's original inputs, script snapshot, source
   revision plus uncommitted source changes, binary hash, and recorder image
   digest. Do not edit an executing script, replace its binary, or rewrite old
   evidence. Build changed sources to a separate output path. Review source and
   binary differences and validate the new harness separately before future
   scored cases; omission of `admission` preserves APF defaults, not every other
   behavior changed elsewhere in the checkout.
2. Lock CPU/memory policy and actual projected resources. Retain and compare the
   entire CSC `spec.sizes[].effects.containerResourcePolicy`, including fallback
   requests, overrides, memory multiplier, Go memory settings, and GOMAXPROCS.
   Verify actual container requests, limits, and runtime settings remain fixed,
   including init containers and helpers. Keep replica count, placement, node
   capacity, images, and contention treatment fixed as well. This setup feature
   neither changes nor enforces that resource lock.
3. Increase offered workload load in reviewed steps with the original APF and
   inflight settings fixed. Change only one workload dimension per comparison,
   such as request concurrency; retain the same workload mix, measurement
   method, and canary expectations. Record client throttling, achieved rate,
   backlog, errors, latency, and recovery rather than equating offered load with
   executed work.
4. Hold the selected workload constant and change one admission variable per
   fresh run: benchmark shares, canary shares, enabling benchmark queuing, or
   queue length. Do not change workload concurrency and shares together. Keep
   other run-owned PLCs from previous runs in the comparison inventory: even
   unused priority levels can change the share denominator. Any cleanup requires
   separate review and authorization; setup performs none.
5. Treat KAS inflight limits as a separate experiment. They belong in CSC
   `spec.sizes[].effects.maximumRequestsInflight` and
   `maximumMutatingRequestsInflight`, not setup admission. The existing sizing
   renderer starts at 150/50; this change adds no renderer or CLI knobs. Freeze
   the resource-policy fields, not the entire CSC, when deliberately comparing
   inflight settings. Review the CSC diff to allow only the chosen inflight field,
   wait for rollout/convergence, and verify effective flags and unchanged
   resources on every KAS replica before measuring. Do not change shares in the
   same comparison.

This is a staged experiment, not an optimizer or sizing qualification. Follow
the separate [harness validation gates](harness-validation.md) before live use.

## Evidence

`SetupResult.admission` records the effective requested policy, including legacy
defaults. `created` remains the acknowledged resource inventory with API version,
kind, name, namespace, UID, and resourceVersion; it is not a full APF snapshot.
A completed setup does not prove controller convergence or classification.

Retain the setup input and result alongside the existing input/binary hashes.
Before baseline, independently snapshot the guest's PLCs and FlowSchemas with
read-only `kubectl --kubeconfig "$GUEST_ADMIN_KUBECONFIG" get
prioritylevelconfigurations,flowschemas -o json`, plus the CSC, projected resource
configuration, and effective KAS flags from the management cluster. Record UTC
time, cluster identity, command, and hash for each new snapshot. Repeat guest
APF observations through the scored and recovery periods, retaining statuses
and changes, and observe classification, execution/queue limits, queued and
rejected requests, and latency per API-server replica. System APF configuration
must be inventoried even though setup leaves it untouched.

These additional snapshots/periodic APF observations are operator evidence
requirements, not new script automation or a guest watch/log recorder. Preserve
the existing recorder/capture PVC/PV inventory and exports; do not infer APF
coverage from recorder readiness or a successful export. Missing critical
observations invalidate the intended comparison rather than establishing that
an admission policy improved it.
