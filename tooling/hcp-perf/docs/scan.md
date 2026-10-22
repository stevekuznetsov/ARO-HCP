# Local Population Planning

## Resource-Locked Decision

The current user-selected experiment is the
[resource-locked scan](../../../docs/resource-locked-scan.md): keep candidate-v2
requests, limits, Go runtime settings, HCP images and three guest nodes fixed;
repeat the three-application validation, then review 12, 30, 60, 90 and 120
applications in order. The experiment is ongoing, not a completed pass. A prior
ACK timeout is not proof that the baseline would pass with changed observation;
candidate viability remains a working hypothesis.

Keep the API workload recipe fixed: one full five-Pod replacement per application
per hour, staggered across the population, with unchanged endpoint mix, payload
bytes and supporting-object recipe. More applications increase aggregate desired
throughput, not per-application churn. Measure useful completions, retries,
rejections and outstanding debt; no completed customer work means no stability
claim, even if the protected canary passes.

The decision record defines soft degradation versus hard stops, separate workload,
performance and capture outcomes, immutable case evidence, profile-drift guards,
admission-first serving-path follow-ups, evidence-backed ancillary resource
adjustments between candidates, cost reporting and the fresh-HCP confirmation gate.
It supersedes older hard-stop interpretations of the 3x baseline expectation for
this scan, not historical case outcomes. The $0.125/HCP-hour target is reported,
not enforced by shrinking the resource profile. No campaign, safety monitor or
autonomous optimizer is implemented by this plan.

## Existing Planner Contract

`scan plan --profile examples/small.json --input examples/scan.json` is a
plan-only CLI command. It runs without credentials or a cluster.

The example holds three guest nodes fixed and plans 0, 15, 60, 150, 300, 450,
and 600 application pods (five per application). Applications must start at zero,
be strictly increasing and unique, and be at most 40,000. The planning ceiling
is `applications * 5 <= guestNodes * 200`. This is not a schedulability claim:
system pods, canaries, rollout surge, and guest resource headroom need runtime
validation, including at the 600-pod endpoint.

Those are the existing example and parser rules, not the reviewed execution order
above. In particular, the repeated three-app validation is a separate case; a
strictly increasing, unique population plan cannot represent that repeat. A
zero-app reference tests platform/canary overhead, not customer-work completion.
This documentation does not change the example, parser, inputs or scripts.

Each result contains the resolved scan inputs, `status: planned`, a SHA-256 of
the JSON-serialized input profile, and full `experiment.Plan` results including
case inputs. Whitespace and object-key order in the source file do not affect
the fingerprint; profile array order does. Every case uses the same profile
and anchor. Footprint and population are separate axes: choose another profile
in a separate scan, not within this population scan. No coefficients, prices,
cost targets, or synthesis weights are inferred. Fee and target inputs remain
in the separate cost specification (`examples/cost.json`).

Case names use the scan name plus a zero-based five-digit index; scan names are
limited to 57 characters so case names fit 63. The seed is held constant across
cases. Population determines the stagger. Optional `turnoverPeriodSeconds`
resolves absent/zero to 3600 and otherwise accepts 1..86400; the resolved value is
recorded in the scan spec and propagated unchanged into every case spec. Each
case is capped at 80,000 scheduled rollouts. Shorter duration alone never
accelerates churn. The historical hourly population recipe above is unchanged;
choosing another period is an explicit activity treatment in a separate plan.
For a fixed-population period sweep keep `maxOutstanding` separately chosen
(initially 8) and constant, along with profile, seed and supporting objects.
Period is not a resource, APF or price change, and plan metadata proves no
achieved throughput. See [activity scope](../scripts/README.md#client-activity-treatment).

`mode` defaults to `validation`: 300 seconds steady, 30-second canary cadence,
30 seconds recovery, and 30 seconds settle. Explicit `soak` plans use 7200,
300, 600, and 300 seconds respectively. Both modes use phase bounds of 3600
seconds startup, 1800 seeding, 600 calibration, and 120 shutdown. These are
planning bounds, not guarantees of convergence. `experiment.Plan` determines
the baseline and whether calibration is included.

The user must explicitly choose `cpuTreatment: quiet` or `contended`. The common
90% CPU-fill target is metadata for a subsequent adversary primitive, inactive
for quiet cases. This planner neither calculates live fill allocations nor
changes control-plane configuration. No environment or cluster is selected.

Optional `harnessEvidence: {"path": "...", "sha256": "..."}` carries an
unverified evidence reference. Only its shape is checked; the referenced file
is not read and cannot authorize execution. There is no execution-readiness
claim, live case, stop-policy evaluator, automatic repeat, or campaign runner.
Output goes only to the supplied writer; input files and live artifacts are
not modified. Evaluate validation evidence first and proceed to long cases
only after the harness is clean. Do not automatically execute or repeat the
planned long cases.
