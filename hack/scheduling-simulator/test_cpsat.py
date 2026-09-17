"""Synthetic CP-SAT regressions; run with python -m unittest test_cpsat."""

import importlib.util
import io
import math
import unittest
from collections import Counter
from contextlib import redirect_stderr
from fractions import Fraction
from unittest.mock import patch

from demand.model import ClusterDemand, Component
from optimize import cpsat, exact
from optimize.engine import RunConfig
from skus import SKU


class SyntheticDemand:
    def cluster_demand(self, size, policy, *args, **kwargs):
        return ClusterDemand(size, policy, [
            Component("etcd", "zonal_etcd", True, 3, 100, 100, 1),
            Component("api", "zonal_pair", True, 2, 100, 100, 1),
            Component("other", "overflow", False, 2, 100, 100, 0),
        ])


@unittest.skipUnless(importlib.util.find_spec("ortools"), "requires ortools")
class CPSATTests(unittest.TestCase):
    def setUp(self):
        self.cfg = RunConfig(
            hcps_per_mc=1, reserve_slots=0, concurrent_rolling_hcps=0,
            az_failure_reserve=0, reservation_mode="flat",
            system_reserved_cpu_mc=0, system_reserved_mem_mib=0,
            node_overhead_cpu_mc=0, node_overhead_mem_mib=0,
            node_overhead_pods=0, buffer_mem=0, overflow_az_count=3)
        self.skus = [SKU("small", 1, 1, 3), SKU("large", 2, 2, 3)]
        self.model = SyntheticDemand()

    def solve(self, distribution=None, **kwargs):
        return cpsat.solve_region({"1": 1} if distribution is None else distribution, self.cfg,
                                  self.model, self.skus, workers=kwargs.pop("workers", 1),
                                  time_limit=kwargs.pop("time_limit", 5), **kwargs)

    def test_two_mcs_regional_disjointness_and_physical_inventory(self):
        with patch.object(exact, "ffd_pack", side_effect=AssertionError("no greedy")):
            result = self.solve({"1": 1, "2": 1})
        self.assertEqual(result["solver"]["status"], "OPTIMAL")
        self.assertTrue(result["solver"]["validated"])
        self.assertEqual(result["n_mcs"], 2)
        self.assertEqual(result["region"]["zonal_cores"], 6)
        self.assertEqual(result["region"]["overflow_cores"], 8)
        self.assertEqual(result["region"]["overflow_nodes"], 4)
        zonal_skus, overflow_skus = set(), set()
        for mc in result["management_clusters"]:
            zonal = mc["packing"]["zonal"]
            self.assertEqual(Counter(n["sku"] for n in zonal[0]),
                             Counter(n["sku"] for n in zonal[1]))
            self.assertEqual(Counter(n["sku"] for n in zonal[1]),
                             Counter(n["sku"] for n in zonal[2]))
            zonal_skus.update(n["sku"] for az in zonal for n in az)
            overflow_skus.update(n["sku"] for n in mc["packing"]["overflow"])
            self.assertEqual(sum(len(n["pods"]) for az in zonal for n in az), 5)
            self.assertEqual(sum(len(n["pods"]) for n in mc["packing"]["overflow"]), 2)
        self.assertFalse(zonal_skus & overflow_skus)
        stats = result["solver"]
        self.assertEqual(stats["objective"], stats["best_objective_bound"])

    def test_no_incumbent_is_not_zero_cost(self):
        result = self.solve(time_limit=0)
        self.assertEqual(result["solver"]["status"], "UNKNOWN")
        self.assertIsNone(result["solver"]["objective"])
        self.assertIsNone(result["region"])
        self.assertEqual(result["management_clusters"], [])

    def test_empty_fleet(self):
        result = self.solve({})
        self.assertEqual(result["solver"]["status"], "EMPTY_FLEET")
        self.assertIsNone(result["region"])

    def test_bounded_infeasibility_and_wider_search(self):
        result = self.solve(slots_per_group=1)
        self.assertEqual(result["solver"]["status"], "INFEASIBLE")
        self.assertTrue(result["solver"]["search_space_bounded"])
        self.assertIsNone(result["region"])
        self.assertTrue(self.solve(slots_per_group=2)["solver"]["validated"])

    def test_single_sku_cannot_serve_both_roles(self):
        self.skus = self.skus[:1]
        self.assertEqual(self.solve()["solver"]["status"], "INFEASIBLE")

    def test_size_guards_before_build(self):
        with patch.object(exact, "build_mc", side_effect=AssertionError("guard first")):
            result = self.solve({"1": 1000000})
        self.assertEqual(result["solver"]["status"], "MODEL_TOO_LARGE")
        self.assertEqual(self.solve(max_model_size=1)["solver"]["status"], "MODEL_TOO_LARGE")

    def test_all_resource_dimensions_and_heterogeneous_nodes(self):
        self.skus = [SKU("cpu", 2, 1, 2), SKU("mem", 1, 4, 2)]
        pods = [exact.Pod(0, "1", "cpu", "overflow", 1500, 10, 0, "cpu"),
                exact.Pod(0, "1", "mem", "overflow", 10, 3000, 0, "mem")]
        with patch.object(exact, "build_mc", return_value=([[], [], []], pods)):
            result = self.solve(mode="pinned")
        self.assertTrue(result["solver"]["validated"])
        self.assertEqual(result["management_clusters"][0]["overflow_pool"]["skus"],
                         {"mem": 1, "cpu": 1})
        for dim in exact.DIMS:
            with self.subTest(dim=dim):
                self.cfg.max_pods_per_node = 1 if dim == "pods" else 225
                pod = exact.Pod(0, "1", "test", "overflow",
                                1100 if dim == "cpu" else 1,
                                600 if dim == "mem" else 1,
                                1 if dim == "nic" else 0, "first")
                other = exact.Pod(1, "1", "test", "overflow", pod.cpu_mc,
                                  pod.mem_mib, pod.nic, "second")
                self.skus = [SKU("only", 2, 1, 2)]
                with patch.object(exact, "build_mc", return_value=([[], [], []], [pod, other])):
                    result = self.solve(slots_per_group=1, mode="pinned")
                    self.assertEqual(result["solver"]["status"], "INFEASIBLE")
                    self.assertTrue(self.solve(slots_per_group=2, mode="pinned")["solver"]["validated"])

    def test_rounding_is_conservative(self):
        self.skus = [SKU("only", 1, 1, 2)]
        pod = exact.Pod(0, "1", "test", "overflow", 1000.00001, 1, 0, "key")
        with patch.object(exact, "build_mc", return_value=([[], [], []], [pod])):
            self.assertEqual(self.solve(mode="pinned")["solver"]["status"], "INFEASIBLE")

    def test_joint_sharding_beats_pinned_with_identical_pods(self):
        self.cfg.hcps_per_mc = 2
        self.skus = [SKU("only", 2, 2, 2)]

        def demand(size, policy, *args, **kwargs):
            cpu, mem = (1500, 100) if size in ("1", "2") else (100, 1500)
            return ClusterDemand(size, policy, [Component("test", "overflow", False, 1, cpu, mem, 0)])

        with patch.object(self.model, "cluster_demand", side_effect=demand):
            pinned = self.solve({"1": 1, "2": 1, "3": 1, "4": 1}, mode="pinned")
            with patch.object(exact, "build_mc", side_effect=AssertionError("no pinned demand")), \
                    patch.object(exact, "distribute_conserving", side_effect=AssertionError("no sharding prerequisite")):
                full = self.solve({"1": 1, "2": 1, "3": 1, "4": 1})
        self.assertEqual(pinned["region"]["overflow_cores"], 8)
        self.assertEqual(full["solver"]["status"], "OPTIMAL")
        self.assertEqual(full["region"]["overflow_cores"], 4)
        self.assertEqual(full["solver"]["scope"]["hcp_mc_sharding"], "joint")
        for mc in full["management_clusters"]:
            self.assertEqual(sum(mc["hcp_mix"].get(s, 0) for s in ("1", "2")), 1)
            self.assertEqual(sum(mc["hcp_mix"].get(s, 0) for s in ("3", "4")), 1)

    def test_joint_az_placement_beats_pinned(self):
        self.skus = [SKU("only", 1, 1, 2)]
        demand = ClusterDemand("1", "minimal", [
            Component(str(i), "zonal_pair", True, 1, cpu, 1, 0)
            for i, cpu in enumerate((700, 100, 100, 700))])
        with patch.object(self.model, "cluster_demand", return_value=demand):
            pinned = self.solve(mode="pinned")
            full = self.solve()
        self.assertEqual(pinned["region"]["zonal_cores"], 6)
        self.assertEqual(full["region"]["zonal_cores"], 3)

    def test_full_resource_limits(self):
        self.skus = [SKU("only", 2, 1, 2)]
        for dim in exact.DIMS:
            with self.subTest(dim=dim):
                self.cfg.max_pods_per_node = 1 if dim == "pods" else 225
                demand = ClusterDemand("1", "minimal", [
                    Component(str(i), "float", False, 1,
                              1100 if dim == "cpu" else 1,
                              600 if dim == "mem" else 1,
                              1 if dim == "nic" else 0) for i in range(2)])
                with patch.object(self.model, "cluster_demand", return_value=demand):
                    self.assertEqual(self.solve(slots_per_group=1)["solver"]["status"], "INFEASIBLE")
                    self.assertTrue(self.solve(slots_per_group=2)["solver"]["validated"])

    def test_full_validator_rejects_missing_or_split_workload(self):
        demands = {"1": self.model.cluster_demand("1", "minimal")}
        sizes, records = cpsat._full_workload({"1": 1}, self.cfg, demands, 1)
        with self.assertRaisesRegex(ValueError, "mandatory/reserve pod conservation"):
            cpsat._validate_full(records, sizes, 1, self.cfg, 1, [0],
                                 [None] * len(records), [[[], [], [], []]], self.skus)
        with self.assertRaisesRegex(ValueError, "HCP split"):
            cpsat._validate_full(records, sizes, 1, self.cfg, 1, [0],
                                 [(1, 0, 0)] * len(records), [[[], [], [], []]], self.skus)

    def test_dynamic_reserves_follow_actual_mc_and_az(self):
        self.cfg.hcps_per_mc = 3
        self.cfg.reserve_slots = 1
        self.cfg.az_failure_reserve = 0.5

        def demand(size, policy, *args, **kwargs):
            return ClusterDemand(size, policy, [
                Component("pair", "zonal_pair", True, 2, 10, 10 * int(size), 0),
                Component("other", "float", False, 2, 10, 10 * int(size), 0)])

        for k in (0, 1, 2):
            with self.subTest(rollouts=k), patch.object(self.model, "cluster_demand", side_effect=demand):
                self.cfg.concurrent_rolling_hcps = k
                result = self.solve({"1": 1, "2": 1, "3": 1, "4": 1})
                self.assertTrue(result["solver"]["validated"])
                assignment = result["hcp_assignments"]
                for mi, mc in enumerate(result["management_clusters"]):
                    occupants = [a for a in assignment if a["mc"] == mi]
                    selected = sorted(occupants, key=lambda a: (-int(a["hcp_size"]), a["hcp"]))[:k]
                    expected = Counter({a["hcp"]: 2 for a in selected})
                    pools = mc["packing"]
                    pods = [p for az in pools["zonal"] for n in az for p in n["pods"]]
                    pods += [p for n in pools["overflow"] for p in n["pods"]]
                    self.assertEqual(Counter(p["hcp"] for p in pods if p["reserve"] == "rollout"), expected)
                    self.assertEqual(sum(p["reserve"] == "slot" for p in pods), 4)
                    for az in pools["zonal"]:
                        pods = [p for n in az for p in n["pods"]]
                        working = sorted((p for p in pods if p["reserve"] is None), key=lambda p: p["hcp"])
                        target = sum(p["mem_mib"] for p in working) * 0.5
                        prefix, expected = 0, Counter()
                        for p in working:
                            if prefix < target:
                                expected[p["hcp"]] += 1
                            prefix += p["mem_mib"]
                        self.assertEqual(Counter(p["hcp"] for p in pods if p["reserve"] == "azdeath"), expected)

    def test_reserves_conserved(self):
        self.cfg.hcps_per_mc = 3
        self.cfg.reserve_slots = 1
        self.cfg.concurrent_rolling_hcps = 1
        self.cfg.az_failure_reserve = 0.5
        result = self.solve()
        self.assertTrue(result["solver"]["validated"])
        mc = result["management_clusters"][0]["packing"]
        pods = [p for az in mc["zonal"] for n in az for p in n["pods"]]
        pods += [p for n in mc["overflow"] for p in n["pods"]]
        self.assertEqual(Counter(p["reserve"] for p in pods),
                          {None: 7, "slot": 7, "rollout": 2, "azdeath": 2})

    def test_fractional_az_reserve_does_not_overflow(self):
        self.cfg.hcps_per_mc = 3
        self.skus = [SKU("only", 1, 1, 2)]
        demand = ClusterDemand("1", "minimal", [
            Component("pair", "zonal_pair", True, 3, 1, 100, 0)])
        for fraction in (1 / 3, math.nextafter(1.0, 0.0), 1.0):
            with self.subTest(fraction=fraction), patch.object(self.model, "cluster_demand", return_value=demand):
                self.cfg.az_failure_reserve = fraction
                result = self.solve({"1": 3})
                self.assertEqual(result["solver"]["status"], "OPTIMAL")
                self.assertTrue(result["solver"]["validated"])
                for az in result["management_clusters"][0]["packing"]["zonal"]:
                    pods = [p for n in az for p in n["pods"]]
                    copies = [p for p in pods if p["reserve"] == "azdeath"]
                    # Rounding 1/3 upward requires a second prefix copy, not one.
                    self.assertEqual(len(copies), 2 if fraction == 1 / 3 else 3)
                    self.assertGreaterEqual(sum(p["mem_mib"] for p in copies), fraction * 300)

    def test_reserve_fraction_rounds_up_with_bounded_denominator(self):
        for value in (0, 1 / 3, 0.5, 0.5000000000000001, math.nextafter(1.0, 0.0), 1):
            with self.subTest(value=value):
                rounded = cpsat._reserve_fraction(value)
                self.assertLessEqual(rounded.denominator, 1000000)
                self.assertGreaterEqual(rounded, Fraction(str(value)))
                self.assertLess(rounded - Fraction(str(value)), Fraction(1, 1000000))

    def test_construction_logging_and_guard(self):
        output = io.StringIO()
        with redirect_stderr(output), patch.object(cpsat, "_add_full_placement",
                                                   side_effect=AssertionError("guard before construction")):
            result = self.solve(max_model_size=100, log_search=True)
        self.assertEqual(result["solver"]["status"], "MODEL_TOO_LARGE")
        self.assertIn("size_estimated", result["solver"]["build_stage_seconds"])
        self.assertNotIn("nodes_started", result["solver"]["build_stage_seconds"])
        self.assertIn("CP-SAT model estimate:", output.getvalue())
        self.assertNotIn("solve_started", output.getvalue())
        result = self.solve(time_limit=0)
        stages = result["solver"]["build_stage_seconds"]
        self.assertEqual(list(stages), ["size_estimated", "nodes_started", "full_placement_started",
                                       "validation_started", "solve_started"])
        self.assertEqual(list(stages.values()), sorted(stages.values()))

    def test_validator_detects_cross_mc_role_conflict(self):
        node = exact.new_node(self.cfg, self.skus[0])
        with self.assertRaisesRegex(ValueError, "regional SKU disjointness"):
            cpsat._validate([[[], [], [], []]] * 2,
                            [[[node], [node], [node], []], [[], [], [], [node]]],
                            self.cfg, self.skus)

    def test_validator_rejects_corruption(self):
        pod = exact.Pod(0, "1", "test", "overflow", 100, 100, 0, "same")
        node = exact.new_node(self.cfg, self.skus[0])
        node.place(pod)
        groups = [[[], [], [], [pod]]]
        packed = [[[], [], [], [node]]]
        cpsat._validate(groups, packed, self.cfg, self.skus)
        with self.assertRaisesRegex(ValueError, "conservation"):
            cpsat._validate(groups, [[[], [], [], []]], self.cfg, self.skus)
        node.used["cpu"] += 1
        with self.assertRaisesRegex(ValueError, "accounting"):
            cpsat._validate(groups, packed, self.cfg, self.skus)
        node.used["cpu"] -= 1
        node.place(pod)
        groups[0][3].append(pod)
        with self.assertRaisesRegex(ValueError, "host anti-affinity"):
            cpsat._validate(groups, packed, self.cfg, self.skus)

    def test_invalid_inputs(self):
        for kwargs in ({"slot_factor": 0}, {"slots_per_group": 0},
                       {"time_limit": -1}, {"workers": 0}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                self.solve(**kwargs)
        with self.assertRaises(ValueError):
            self.solve({"1": -1})


if __name__ == "__main__":
    unittest.main()
