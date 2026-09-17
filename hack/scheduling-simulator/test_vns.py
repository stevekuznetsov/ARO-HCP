"""VNS regressions without OR-Tools; run python -m unittest test_vns."""

import math
import random
import time
import unittest
from collections import Counter
from dataclasses import replace
from unittest.mock import patch

from demand.model import ClusterDemand, Component
from optimize import engine, exact, vns
from optimize.engine import RunConfig
from skus import SKU


class SyntheticDemand:
    def cluster_demand(self, size, policy, *args, **kwargs):
        return ClusterDemand(size, policy, [
            Component("etcd", "zonal_etcd", True, 3, 100.01234, 100.05678, 1),
            Component("api", "zonal_pair", True, 2, 100.01234, 100.05678, 1),
            Component("other", "overflow", False, 2, 100.01234, 100.05678, 0),
        ])


class VNSTests(unittest.TestCase):
    def setUp(self):
        self.cfg = RunConfig(
            hcps_per_mc=2, reserve_slots=0, concurrent_rolling_hcps=0,
            az_failure_reserve=0, reservation_mode="flat",
            system_reserved_cpu_mc=0, system_reserved_mem_mib=0,
            node_overhead_cpu_mc=0, node_overhead_mem_mib=0,
            node_overhead_pods=0, buffer_mem=0, overflow_az_count=3)
        self.skus = [SKU("small", 1, 1, 3), SKU("large", 2, 2, 3)]
        self.model = SyntheticDemand()

    def pod(self, key, cpu=100, mem=100, nic=0, **kwargs):
        return exact.Pod(0, "1", key, "overflow", cpu, mem, nic, key, **kwargs)

    def node(self, *pods, sku=None):
        node = exact.new_node(self.cfg, sku or self.skus[0])
        for pod in pods:
            node.place(pod)
        return node

    def solve(self, distribution=None, **kwargs):
        return vns.solve_region({"1": 1} if distribution is None else distribution,
                                self.cfg, self.model, self.skus,
                                time_limit=kwargs.pop("time_limit", 30),
                                max_iterations=kwargs.pop("max_iterations", 60), **kwargs)

    def candidate(self, mc, name, seed=1):
        result = vns._candidate(mc, name, self.skus, self.cfg,
                                random.Random(seed), time.perf_counter() + 10)
        self.assertIsNotNone(result)
        return result

    def test_seed_matches_exact_simulate_and_repeatable_iterations(self):
        distribution = {"1": 5, "2": 2}
        baseline = engine.simulate(distribution, replace(self.cfg, mode="exact"), self.model, self.skus)
        seed = self.solve(distribution, max_iterations=0)
        self.assertEqual({k: val for k, val in seed.items() if k != "solver"}, baseline)
        first = self.solve(distribution)
        second = self.solve(distribution)
        self.assertEqual(first["management_clusters"], second["management_clusters"])
        self.assertEqual(first["region"], second["region"])
        for key in ("iterations", "neighborhoods", "objective", "current_objective",
                    "shakes_accepted", "worse_shakes_accepted"):
            self.assertEqual(first["solver"][key], second["solver"][key], key)
        self.assertEqual([r[:1] + r[2:] for r in first["solver"]["best_history"]],
                         [r[:1] + r[2:] for r in second["solver"]["best_history"]])
        self.assertLessEqual(first["solver"]["objective"], seed["solver"]["objective"])
        self.assertEqual(first["solver"]["iterations"], 60)

    def test_node_evacuation_improves_and_does_not_mutate_seed(self):
        pods = [self.pod(str(i), cpu=400) for i in range(3)]
        initial = [[[], [], [], [self.node(p) for p in pods]]]
        groups = [[[], [], [], pods]]
        original = [exact.node_to_json(n) for n in initial[0][3]]
        best, stats = vns.improve_packing(groups, self.skus[:1], self.cfg,
                                         initial=initial, counts=[4], max_iterations=1)
        self.assertEqual(stats["seed_objective"], [0, 12, 12])
        self.assertEqual(stats["objective"], [0, 8, 8])
        self.assertEqual(stats["neighborhoods"]["evacuate"]["improved"], 1)
        self.assertEqual([exact.node_to_json(n) for n in initial[0][3]], original)
        self.assertEqual(Counter(id(p) for n in best[0][3] for p in n.pods), Counter(map(id, pods)))
        self.assertIsNot(best[0][3][0], initial[0][3][0])
        vns._validate(groups, best, self.cfg, self.skus)

    def test_destroy_repack_escapes_evacuation_local_minimum(self):
        # The low-utilization 600m nodes cannot be evacuated into 600m/800m
        # neighbors. Repacking pairs each 600m pod with a 400m pod instead.
        pods = [self.pod(str(i), cpu=cpu) for i, cpu in enumerate((600, 600, 400, 400))]
        mc = [[], [], [], [self.node(pods[0]), self.node(pods[1]), self.node(*pods[2:])]]
        self.assertIsNone(vns._candidate(mc, "evacuate", self.skus[:1], self.cfg,
                                         random.Random(1), time.perf_counter() + 10))
        best, stats = vns.improve_packing([[[], [], [], pods]], self.skus[:1], self.cfg,
                                         initial=[mc], max_iterations=4, shake_probability=0)
        self.assertEqual(stats["objective"], [0, 2, 2])
        self.assertGreater(stats["neighborhoods"]["repack_4"]["improved"], 0)
        vns._validate([[[], [], [], pods]], best, self.cfg, self.skus)

    def test_all_destroy_sizes_and_mixed_only_sku_feasibility(self):
        self.skus = [SKU("cpu", 2, 1, 2), SKU("mem", 1, 4, 2)]
        pods = [self.pod("cpu", cpu=1500, mem=10), self.pod("mem", cpu=10, mem=3000)]
        mc = [[], [], [], [self.node(pods[0]), self.node(pods[1], sku=self.skus[1])]]
        self.assertIsNone(exact.hetero_pack(pods, self.skus, self.cfg))
        for name in ("repack_2", "repack_4", "repack_8"):
            with self.subTest(name=name):
                candidate = self.candidate(mc, name)
                vns._validate([[[], [], [], pods]], [candidate], self.cfg, self.skus)
                self.assertEqual({n.sku for n in candidate[3]}, {"cpu", "mem"})

    def test_repair_prefers_fewer_nodes_at_equal_cores(self):
        pods = [self.pod(str(i), cpu=600) for i in range(2)]
        initial = [[[], [], [], [self.node(p) for p in pods]]]
        groups = [[[], [], [], pods]]
        # Greedy's cores-only tie keeps two 1-core nodes. VNS should choose one
        # 2-core node, despite it being a later base in the sorted catalog.
        self.assertEqual(len(exact.hetero_pack(pods, self.skus, self.cfg)), 2)
        with patch.object(exact, "hetero_pack", side_effect=AssertionError("own repair")):
            best, stats = vns.improve_packing(groups, self.skus, self.cfg, initial=initial,
                                             max_iterations=2, shake_probability=0)
        self.assertEqual(stats["seed_objective"], [0, 2, 2])
        self.assertEqual(stats["objective"], [0, 2, 1])
        self.assertEqual([n.sku for n in best[0][3]], ["large"])
        self.assertEqual(stats["neighborhoods"]["repack_2"]["improved"], 1)
        vns._validate(groups, best, self.cfg, self.skus)

    def test_no_op_reordering_is_feasible_but_never_accepted(self):
        pods = [self.pod(str(i)) for i in range(4)]
        initial = [[[], [], [], [self.node(*pods[:2]), self.node(*pods[2:])]]]

        def reorder(mc, *args):
            result = vns._copy_mc(mc)
            result[3].reverse()
            for node in result[3]:
                node.pods.reverse()
            return result

        with patch.object(vns, "_candidate", side_effect=reorder):
            best, stats = vns.improve_packing([[[], [], [], pods]], self.skus, self.cfg,
                                             initial=initial, max_iterations=6, shake_probability=1)
        self.assertEqual(stats["shakes_accepted"], 0)
        self.assertEqual(stats["worse_shakes_accepted"], 0)
        self.assertEqual(stats["objective"], stats["seed_objective"])
        self.assertEqual(len(stats["best_history"]), 1)
        self.assertEqual(vns._signature(best[0]), vns._signature(initial[0]))
        for counter in stats["neighborhoods"].values():
            self.assertEqual(counter["feasible"], 1)
            self.assertEqual(counter["no_op"], 1)
            self.assertEqual(counter["accepted"], 0)
            self.assertEqual(counter["shakes_accepted"], 0)

    def test_signature_retains_pod_identity_sku_pool_and_padding(self):
        pod = self.pod("same")
        replica = replace(pod)
        other = self.pod("other")
        mc = [[], [], [], [self.node(pod, other), self.node(replica)]]
        signature = vns._signature(mc)
        changed = vns._copy_mc(mc)
        changed[3] = [self.node(replica, other), self.node(pod)]
        self.assertNotEqual(signature, vns._signature(changed))
        changed = vns._copy_mc(mc)
        changed[3][0].sku = "large"
        self.assertNotEqual(signature, vns._signature(changed))
        changed = vns._copy_mc(mc)
        changed[0], changed[3] = changed[3], changed[0]
        self.assertNotEqual(signature, vns._signature(changed))
        changed = vns._copy_mc(mc)
        changed[3].append(self.node())
        self.assertNotEqual(signature, vns._signature(changed))

    def test_rightsize_rebuilds_capacity_keys_and_all_zone_padding(self):
        pod = self.pod("test")
        mc = [[self.node(pod, sku=self.skus[1])],
              [self.node(sku=self.skus[1])], [self.node(sku=self.skus[1])], []]
        candidate = self.candidate(mc, "rightsize")
        self.assertEqual([[n.sku for n in az] for az in candidate[:3]], [["small"]] * 3)
        vns._validate([[[pod], [], [], []]], [candidate], self.cfg, self.skus)
        self.assertEqual(candidate[0][0].keys, {pod.key})
        self.assertEqual(candidate[1][0].keys, set())
        self.assertEqual(candidate[0][0].cap, exact.new_node(self.cfg, self.skus[0]).cap)

    def test_balancing_counts_all_azs_per_sku_removes_obsolete_padding(self):
        a, b, c = [self.pod(k) for k in "abc"]
        mc = [[self.node(a), self.node(b), self.node(sku=self.skus[1])],
              [self.node(c, sku=self.skus[1])], [], [self.node()]]
        normalized = vns._normalize(mc, self.skus, self.cfg)
        expected = Counter(small=2, large=1)
        self.assertEqual([Counter(n.sku for n in az) for az in normalized[:3]], [expected] * 3)
        self.assertEqual(normalized[3], [])
        vns._validate([[[a, b], [c], [], []]], [normalized], self.cfg, self.skus)

    def test_cross_pool_sku_transfer_only_after_last_use(self):
        self.skus.append(SKU("third", 3, 3, 3))
        zpods = [self.pod(str(i)) for i in range(3)]
        opod = self.pod("overflow")
        mc = [[self.node(p)] for p in zpods] + [[self.node(opod, sku=self.skus[2])]]
        candidate = self.candidate(mc, "sku_role", seed=1)
        vns._validate([[[p] for p in zpods] + [[opod]]], [candidate], self.cfg, self.skus)
        self.assertEqual({n.sku for az in candidate[:3] for n in az}, {"large"})
        self.assertEqual([n.sku for n in candidate[3]], ["small"])
        self.assertEqual([s.name for s in vns._allowed(mc, True, self.skus)], ["large", "third"])
        # Only replacing one zonal node does not free small for overflow.
        partial = vns._copy_mc(mc)
        partial[0][0] = self.node(zpods[0], sku=self.skus[1])
        self.assertNotIn("small", [s.name for s in vns._allowed(partial, True, self.skus)])

    def test_shaking_accepts_worse_but_best_history_never_regresses(self):
        pod = self.pod("test")
        groups = [[[], [], [], [pod]]]
        initial = [[[], [], [], [self.node(pod)]]]
        best, stats = vns.improve_packing(groups, self.skus, self.cfg, initial=initial,
                                         max_iterations=60, shake_probability=1)
        self.assertGreater(stats["worse_shakes_accepted"], 0)
        self.assertGreaterEqual(stats["shakes_accepted"], stats["worse_shakes_accepted"])
        self.assertEqual(stats["objective"], [0, 1, 1])
        history = [row[2:] for row in stats["best_history"]]
        self.assertTrue(all(b < a for a, b in zip(history, history[1:])))
        self.assertLessEqual(stats["objective"], stats["current_objective"])
        vns._validate(groups, best, self.cfg, self.skus)
        counters = stats["neighborhoods"].values()
        self.assertEqual(sum(c["attempted"] for c in counters), stats["iterations"])
        for counter in counters:
            self.assertEqual(counter["attempted"], counter["feasible"] + counter["infeasible"]
                             + counter["timed_out"])
            self.assertLessEqual(counter["accepted"] + counter["no_op"], counter["feasible"])

    def test_original_fractional_resources_and_all_reserves_conserved(self):
        self.cfg.hcps_per_mc = 3
        self.cfg.reserve_slots = 1
        self.cfg.concurrent_rolling_hcps = 1
        self.cfg.az_failure_reserve = 0.5
        groups = []
        for shard, _ in exact.group_shapes(exact.distribute_conserving({"1": 5}, self.cfg.usable_slots)):
            zonal, overflow = exact.build_mc(
                self.model, shard, self.cfg.policy, self.cfg.percentile, self.cfg.multiplier,
                self.cfg.unsteered_placement, self.cfg.az_failure_reserve,
                self.cfg.concurrent_rolling_hcps, self.cfg.reserve_slots, self.cfg.reserve_size)
            groups.append([*zonal, overflow])
        best, stats = vns.improve_packing(groups, self.skus, self.cfg, counts=[2, 1], max_iterations=80)
        self.assertTrue(stats["validated"])
        for expected, actual in zip(groups, best):
            for pods, pool in zip(expected, actual):
                self.assertEqual(Counter(map(id, pods)), Counter(id(p) for n in pool for p in n.pods))
            kinds = Counter(p.reserve_kind for pool in actual for n in pool for p in n.pods)
            self.assertEqual(kinds["slot"], 7)
            self.assertEqual(kinds["rollout"], 2)
            self.assertGreater(kinds["azdeath"], 0)
            for pool in actual:
                for node in pool:
                    self.assertAlmostEqual(node.used["cpu"], len(node.pods) * 100.01234)
        result = self.solve({"1": 5})
        self.assertEqual(result["n_mcs"], 3)
        self.assertEqual(sum(mc["count"] * mc["hcps"] for mc in result["management_clusters"]), 5)
        self.assertEqual(result["solver"]["objective"],
                         [result["region"]["zonal_cores"], result["region"]["overflow_cores"],
                          result["region"]["zonal_nodes"] + result["region"]["overflow_nodes"]])

    def test_regional_overlap_reported_not_claimed_global(self):
        a, b = self.pod("a"), self.pod("b")
        groups = [[[a], [], [], []], [[], [], [], [b]]]
        initial = [[[self.node(a)], [self.node()], [self.node()], []],
                   [[], [], [], [self.node(b)]]]
        best, stats = vns.improve_packing(groups, self.skus, self.cfg, initial=initial, max_iterations=0)
        self.assertIsNotNone(best)
        self.assertTrue(stats["validated"])
        self.assertEqual(stats["regional_sku_overlap"], ["small"])
        self.assertFalse(stats["regional_sku_disjoint"])
        self.assertEqual(stats["scope"]["sku_disjointness"], "per_MC_same_as_greedy_not_global")

    def test_validator_rejects_missing_duplicate_and_fixed_az_or_role_changes(self):
        a, b = self.pod("a"), self.pod("b")
        groups = [[[a], [], [], [b]]]
        good = vns._normalize([[self.node(a)], [], [], [self.node(b, sku=self.skus[1])]],
                              self.skus, self.cfg)
        for mode in ("missing", "duplicate", "az", "role", "mc"):
            with self.subTest(mode=mode):
                bad = vns._copy_mc(good)
                if mode == "missing":
                    bad[0][0].pods.clear()
                elif mode == "duplicate":
                    bad[0][0].place(a)
                elif mode == "az":
                    bad[0], bad[1] = bad[1], bad[0]
                elif mode == "role":
                    bad[0][0].pods, bad[3][0].pods = bad[3][0].pods, bad[0][0].pods
                with self.assertRaisesRegex(ValueError, "conservation"):
                    vns._validate(groups, [] if mode == "mc" else [bad], self.cfg, self.skus)

    def test_validator_rejects_all_resource_overloads(self):
        for dim in exact.DIMS:
            with self.subTest(dim=dim):
                self.cfg.max_pods_per_node = 1 if dim == "pods" else 225
                pods = [self.pod(str(i), cpu=600 if dim == "cpu" else 1,
                                 mem=600 if dim == "mem" else 1,
                                 nic=2 if dim == "nic" else 0) for i in range(2)]
                with self.assertRaisesRegex(ValueError, f"{dim} capacity"):
                    vns._validate([[[], [], [], pods]], [[[], [], [], [self.node(*pods)]]],
                                  self.cfg, self.skus)

    def test_validator_rejects_host_spread_balance_quota_and_stale_state(self):
        pod = self.pod("key")
        other = replace(pod)
        with self.assertRaisesRegex(ValueError, "host anti-affinity"):
            vns._validate([[[], [], [], [pod, other]]], [[[], [], [], [self.node(pod, other)]]],
                          self.cfg, self.skus)
        with self.assertRaisesRegex(ValueError, "AZ balance"):
            vns._validate([[[pod], [], [], []]], [[[self.node(pod)], [], [], []]], self.cfg, self.skus)
        with self.assertRaisesRegex(ValueError, "per-MC SKU"):
            vns._validate([[[pod], [], [], [other]]],
                          [[[self.node(pod)], [self.node()], [self.node()], [self.node(other)]]],
                          self.cfg, self.skus)
        for field in ("used", "keys", "cap", "full", "system", "daemonset", "buffer"):
            with self.subTest(field=field):
                node = self.node(pod)
                if field == "keys":
                    node.keys.clear()
                else:
                    getattr(node, field)["cpu"] += 1
                with self.assertRaises(ValueError):
                    vns._validate([[[], [], [], [pod]]], [[[], [], [], [node]]], self.cfg, self.skus)

    def test_validator_rejects_replica_zone_spread_and_nonfinite_pod(self):
        a = replace(self.pod("same"), tier="zonal_pair")
        b = replace(a)
        mc = vns._normalize([[self.node(a), self.node(b)], [], [], []], self.skus, self.cfg)
        with self.assertRaisesRegex(ValueError, "replica AZ spread"):
            vns._validate([[[a, b], [], [], []]], [mc], self.cfg, self.skus)
        for value in (math.nan, math.inf, -1):
            pod = self.pod("bad", cpu=value)
            with self.assertRaisesRegex(ValueError, "invalid pod resource"):
                vns._validate([[[], [], [], [pod]]], [[[], [], [], [self.node(pod)]]], self.cfg, self.skus)

    def test_infeasible_seed_is_error_not_zero(self):
        self.skus = self.skus[:1]
        result = self.solve()
        self.assertEqual(result["solver"]["status"], "SEED_INFEASIBLE")
        self.assertIsNone(result["region"])
        self.assertIsNone(result["solver"]["objective"])
        self.assertFalse(result["solver"]["validated"])
        self.assertEqual(result["management_clusters"], [])
        self.assertIn("error", result)
        pod = self.pod("too-big", cpu=1000.00001)
        best, stats = vns.improve_packing([[[], [], [], [pod]]], self.skus, self.cfg,
                                         initial=[[[], [], [], [self.node(pod)]]])
        self.assertIsNone(best)
        self.assertIn("cpu capacity", stats["error"])

    def test_zero_budget_still_seeds_and_empty_fleet(self):
        with patch.object(vns, "_candidate", side_effect=AssertionError("no search")):
            result = self.solve(time_limit=0)
        stats = result["solver"]
        self.assertTrue(stats["validated"])
        self.assertEqual(stats["iterations"], 0)
        self.assertEqual(stats["objective"], stats["seed_objective"])
        self.assertGreaterEqual(stats["runtime_seconds"], stats["seed_time_seconds"] + stats["search_time_seconds"])
        empty = self.solve({})
        self.assertEqual(empty["solver"]["status"], "EMPTY_FLEET")
        self.assertIsNone(empty["region"])

    def test_budget_checked_in_fits_and_after_greedy_repair(self):
        pod = self.pod("one")
        with self.assertRaises(vns._BudgetExpired):
            vns._repack([pod], self.skus, self.cfg, random.Random(1), time.perf_counter() - 1)
        clock = [0.0]

        def slow_fits(node, pod):
            clock[0] = 2.0
            return True

        with patch.object(exact.Node, "fits", slow_fits), \
                patch.object(vns.time, "perf_counter", side_effect=lambda: clock[0]):
            with self.assertRaises(vns._BudgetExpired):
                vns._repack([pod], self.skus, self.cfg, random.Random(1), 1.0)
        groups = [[[], [], [], [pod]]]
        initial = [[[], [], [], [self.node(pod)]]]
        with patch.object(vns, "_candidate", side_effect=vns._BudgetExpired):
            best, stats = vns.improve_packing(groups, self.skus, self.cfg, initial=initial,
                                             max_iterations=1)
        self.assertIsNotNone(best)
        self.assertEqual(stats["objective"], stats["seed_objective"])
        self.assertEqual(stats["neighborhoods"]["evacuate"]["timed_out"], 1)
        self.assertEqual(stats["stop_reason"], "time_limit")

    def test_seed_time_consumes_budget_before_search(self):
        clock = [0.0]
        original = exact.size_pools_disjoint

        def slow_seed(*args):
            result = original(*args)
            clock[0] += 2
            return result

        with patch.object(vns.time, "perf_counter", side_effect=lambda: clock[0]), \
                patch.object(exact, "size_pools_disjoint", side_effect=slow_seed), \
                patch.object(vns, "_candidate", side_effect=AssertionError("no time left")):
            result = self.solve(time_limit=1)
        self.assertEqual(result["solver"]["seed_time_seconds"], 2)
        self.assertEqual(result["solver"]["iterations"], 0)
        self.assertEqual(result["solver"]["runtime_seconds"], 2)

    def test_corrupt_candidate_rejected_without_losing_best(self):
        pod = self.pod("keep")
        groups = [[[], [], [], [pod]]]
        initial = [[[], [], [], [self.node(pod)]]]
        with patch.object(vns, "_candidate", return_value=[[], [], [], []]):
            best, stats = vns.improve_packing(groups, self.skus, self.cfg, initial=initial,
                                             max_iterations=6, shake_probability=1)
        self.assertEqual(stats["objective"], stats["seed_objective"])
        self.assertEqual(stats["shakes_accepted"], 0)
        self.assertTrue(all(c["infeasible"] == 1 for c in stats["neighborhoods"].values()))
        vns._validate(groups, best, self.cfg, self.skus)

    def test_evacuation_never_uses_other_az_or_pool(self):
        pods = [self.pod(str(i), cpu=100) for i in range(4)]
        mc = [[self.node(p)] for p in pods[:3]] + [[self.node(pods[3], sku=self.skus[1])]]
        for seed in range(8):
            with self.subTest(seed=seed):
                self.assertIsNone(vns._candidate(mc, "evacuate", self.skus, self.cfg,
                                                 random.Random(seed), time.perf_counter() + 10))

    def test_invalid_inputs(self):
        for kwargs in ({"time_limit": -1}, {"time_limit": math.nan}, {"max_iterations": -1},
                       {"max_iterations": True}, {"max_iterations": 1.5}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                self.solve(**kwargs)
        for distribution in ({"1": -1}, {"1": 1.5}, {"1": True}):
            with self.assertRaises(ValueError):
                self.solve(distribution)
        with self.assertRaises(ValueError):
            vns.improve_packing([[], [], [], []], self.skus, self.cfg, counts=[1])
        self.cfg.reserve_slots = self.cfg.hcps_per_mc
        with self.assertRaisesRegex(ValueError, "slots"):
            self.solve()


if __name__ == "__main__":
    unittest.main()
