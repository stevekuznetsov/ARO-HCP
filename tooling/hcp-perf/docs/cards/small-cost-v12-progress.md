# Small Cost V12 Progress

The fresh `perf-small-cost-v10` RP create failed at its one-hour deadline on
2026-09-11 at 21:34:21 UTC. It received V11 and V12 sizing changes during startup.
This is not a fresh or completed V12 qualification.

## Bootstrap Bottleneck

KAS main containers remained Ready with zero restarts while regular `bootstrap`
sidecars repeatedly exceeded the 50-second manifest-apply deadline. Sidecar
backoff made entire Pods unready, removing API service endpoints.

A live bootstrap sample recorded `cpu.max=1000 100000`, 1,111 throttled periods
out of 1,111, 83.62% CPU pressure, and no OOM events. Memory was 22,355,968 bytes
against a 25,165,824-byte limit. CVO also exited on feature-gate changes; not all
its restarts were allocation failures.

## V12 Treatment

| Container | V11 Request | V12 Request |
| --- | --- | --- |
| KAS bootstrap, each of three | 2m / 21Mi | 50m / 64Mi |
| KAS main, each of three | 117m / 511Mi | 79m / 468Mi |
| CPO | 90m / 65Mi | 60m / 65Mi |

The model remains 2,480m effective CPU and charges 10,151Mi memory, costing
$0.12493/HCP-hour under the raw D16s_v6 model. Images and readiness/liveness
checks were unchanged. Lower main-container allocations remain unqualified.

The canonical CSC was applied at 21:27:18 UTC. Its policy hash caused dependency
rollouts, including sequential etcd replacement. KAS had not received V12 when
the RP operation failed. Policy mismatches in that window are old rollout Pods.

All 27 allocation tests and V12 renderer verification passed. The saved report
retains inherited historical delta and steady-memory metadata; updated rows,
Pod charges, and offering totals describe V12.

## Preserved State

All 46 capture PVC UIDs were verified present. Retention was refreshed for five
underlay groups, customer prerequisites, and the current managed group. No guest
credentials or workers were created. No new create or deletion was submitted.
The failed cluster is retained and continues normal controller reconciliation.

Private evidence: `_artifacts/small-cost-current/HANDOFF.md`,
`_artifacts/small-cost-v12/lifecycle/`, and
`_artifacts/small-cost-v10/lifecycle/result.json`.
