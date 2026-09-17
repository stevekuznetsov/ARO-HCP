# CP-SAT Experiments

## Greedy-Seeded Variable Neighborhood Search

`vns.solve_region()` is a separate experiment that starts with the existing
greedy placement and perturbs it. The website's default solver is unchanged.

```sh
venv/bin/python -m optimize.benchmark --solver vns --case medium \
  --time-limit 20 --max-iterations 250 --seed 1 --output .cache/vns-medium.json
venv/bin/python -m optimize.benchmark --solver vns --case user \
  --time-limit 30 --max-iterations 1000 --seed 1 --output .cache/vns-user.json
```

Neighborhoods evacuate a node, repack 2/4/8 nodes with randomized FFD tie order,
rightsize nodes, or move a SKU between zonal/overflow roles within an MC.
Small neighborhoods escalate when they stall. Bounded shaking can accept a
worse working placement to escape a local minimum, while a separate best
incumbent never worsens. Identical MC shape multiplicities weight all objectives.

Every candidate is checked for original pod conservation, all four resource
limits, host anti-affinity, fixed MC/AZ/role, balanced per-SKU zonal inventories,
and per-MC SKU disjointness. Real and reserve pods retain their unrounded demands.
No CP-SAT/OR-Tools is involved in this experiment. Counts distinguish feasible
attempts, actual improvements, accepted shakes, and feasible no-ops.

**Scope:** sharding, pod AZ/role and every reserve copy are fixed to greedy's
`build_mc` output. SKU disjointness is per MC, matching the current greedy pass,
not region-wide. Regional overlap is reported and is not evidence of a deployable
globally-disjoint solution. Legacy reserve routing is deliberately replayed for
the comparison, not certified as correct policy placement.

Initial single-run minimal-policy measurements:

| Fleet | Greedy zonal / overflow cores | VNS zonal / overflow cores | Nodes greedy / VNS | VNS total time |
| --- | ---: | ---: | ---: | ---: |
| 17 HCPs, 250 iterations | 288 / 56 | 192 / 48 | 32 / 21 | 2.11s |
| 477 HCPs, 30s budget | 4020 / 852 | 4020 / 812 | 425 / 414 | 30.08s |

The 477-HCP run retained regional D4 SKU overlap present in greedy. The budget
includes seeding (11.55s in that run) and is cooperative: an individual repair
call can slightly overrun. Seed and fixed iteration counts reproduce search
when time permits. This is heuristic improvement, not a local/global optimality
certificate, and lower cores can still increase memory or dollar cost.

Tests: `venv/bin/python -m unittest test_vns test_benchmark`.

## CP-SAT

The website continues to use greedy packing. CP-SAT is an independent experimental
regional solver, not a prerequisite or replacement for the current flow.

```sh
# Run from hack/scheduling-simulator; ortools is already in requirements.txt.
venv/bin/python -m optimize.benchmark --case medium --time-limit 30 --workers 8 \
  --output .cache/cpsat-medium.json

# Disable CP-SAT's LNS portfolio components for comparison.
venv/bin/python -m optimize.benchmark --case medium --no-lns --time-limit 30 \
  --output .cache/cpsat-medium-no-lns.json

# Keep the existing sharding/AZ/reserve workload fixed, optimize its node packing.
venv/bin/python -m optimize.benchmark --case small --mode pinned \
  --output .cache/cpsat-small-pinned.json

# Deliberately lift the construction guards for the 477-HCP experiment.
venv/bin/python -m optimize.benchmark --case user --time-limit 60 --workers 4 \
  --max-pods 120000 --max-model-size 5000000 --output .cache/cpsat-user-60s.json
```

## Model

`cpsat.solve_region()` defaults to full mode: HCP-to-MC assignment, pod AZ and
node placement, heterogeneous node SKUs, node count, and a region-wide disjoint
zonal/overflow SKU partition are joint decisions. Each HCP remains on one MC.
MC count is fixed by real HCP count and usable slots, and pod role eligibility
follows the demand policy. Growth placeholders remain on every MC; rollout and
AZ-death reserve selection follows the solved placement.

Pods occupy unit intervals at integer node positions. Each candidate node has a
fixed blocker consuming maximum resource capacity minus its selected SKU's usable
capacity. Four cumulative constraints enforce CPU, memory, NIC, and pod limits.
This avoids a full pod-by-node Boolean assignment matrix. Host/AZ anti-affinity
and balanced per-SKU zonal inventories are explicit constraints.

The objective is lexicographic zonal cores, then overflow cores, with a bounded
integer weight. Dollars, total node count, and memory are reported, not optimized.
Demands round conservatively to integer units. Candidate node slots are bounded
and configurable: `OPTIMAL` is a proof only for that bounded, rounded model.
The model is packable headroom, not a complete AZ-failure scenario simulation.

OR-Tools CP-SAT runs its standard portfolio with `use_lns=True`. This enables its
built-in large-neighborhood search, not custom variable-neighborhood search.
There is no greedy hint or seed. Search time is limited separately from model
construction and result validation. Parallel runs are not deterministic despite
the fixed seed; use one worker for reproducibility (with a different search mix).

Every incumbent is independently validated. `UNKNOWN` means no incumbent was found,
not an empty fleet or zero-cost solution. Construction guards protect interactive
use; raising them may require considerable memory. Benchmarks do not measure RSS.

## Initial Results

These are single-run measurements, not statistical performance claims. Both
methods used the bundled profiles/catalog and default reserves. Medium fleet:
10 twelve-worker, 5 thirty-worker, and 2 two-hundred-fifty-worker HCPs.

| Minimal policy, medium fleet | Search / total time | Zonal cores | Overflow cores | Nodes | Provisioned GiB | VM USD/hour |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Greedy | 0.10s total | 288 | 56 | 32 | 1936 | 19.541 |
| Full CP-SAT, LNS enabled | 30s / 30.16s | 192 | 44 | 19 | 1888 | 15.607 |
| Full CP-SAT, LNS disabled | 30s / 30.16s | 396 | 408 | 64 | 4928 | 47.254 |

Both medium minimal CP-SAT runs were `FEASIBLE`, not proven optimal. Across runs,
incumbents vary; enabling LNS does not guarantee a better result in every trial.
Tiny two-HCP cases reached bounded-model optimality within a few seconds. Full
477-HCP cases built in approximately 5-9 seconds, but produced no incumbent within
20- or 60-second search budgets (total approximately 26-71 seconds).

## Comparison Caveats

The benchmark marks noncomparable greedy results explicitly:

- Greedy enforces SKU disjointness per MC, not region-wide; the 477-HCP minimal
  baseline uses D4s_v6 in both roles across MCs.
- The legacy builder routes some rollout reserves by component tier instead of
  actual policy placement. Full CP-SAT uses policy placement. These differences
  are reported as `reserve_role_difference` / `not_same_policy`, not certified
  packing savings. Pinned mode explicitly replays the legacy builder's workload.
- Full mode can change reserves when sharding or AZ assignment changes. Pinned
  mode isolates packing improvements under identical pre-expanded pod demand.

JSON reports include configuration, solver/library version, limits, status,
objective and bound, construction/search/validation timing, resource demand,
inventory, costs and comparability. Output is checkpointed after each completed
approach so an interrupted later run does not erase earlier results.

Tests: `venv/bin/python -m unittest test_cpsat test_benchmark test_pool_accounting test_costing`.
