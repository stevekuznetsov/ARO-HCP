"""Small synthetic regressions for exact pool inventory and accounting."""
import unittest
from collections import Counter
from itertools import permutations
from unittest.mock import patch

from optimize import exact
from optimize.engine import RunConfig
from skus import SKU


class PoolAccountingTests(unittest.TestCase):
    def setUp(self):
        self.cfg = RunConfig(
            reservation_mode="flat", system_reserved_cpu_mc=100,
            system_reserved_mem_mib=128, node_overhead_cpu_mc=100,
            node_overhead_mem_mib=128, node_overhead_pods=1,
            buffer_cpu=0.1, buffer_mem=0.1, max_pods_per_node=10,
            overflow_az_count=3,
        )
        self.small = SKU("Standard_small", 2, 4, 3)
        self.large = SKU("Standard_large", 4, 8, 3)
        self.skus = [self.large, self.small]

    def pod(self, key, cpu=1000, mem=512, nic=1):
        return exact.Pod(0, "1", "synthetic", "zonal_pair", cpu, mem, nic, key)

    def assert_reconciles(self, stats, nodes):
        self.assertEqual(stats["total_nodes"], len(nodes))
        self.assertEqual(stats["total_cores"], sum(n.full["cpu"] / 1000 for n in nodes))
        self.assertEqual(stats["total_mem_gib"], sum(n.full["mem"] / 1024 for n in nodes))
        for dim in exact.DIMS:
            if dim == "nic" and stats["role"] == "overflow":
                self.assertIsNone(stats["util_nic"])
                continue
            cap = sum(n.cap[dim] for n in nodes)
            expected = sum(n.used[dim] for n in nodes) / cap if cap else 0
            if dim == "nic" and not cap:
                expected = None
            self.assertEqual(stats[f"util_{dim}"], expected)

    def test_zonal_balances_each_sku_and_reconciles_totals(self):
        pods = [[self.pod("large", 2500, 4500)],
                [self.pod("replica"), self.pod("replica")], []]
        stats, packed, used = exact._pack_zonal(pods, self.skus, self.cfg)
        expected_mix = {self.large.name: 1, self.small.name: 2}
        for original, az in zip(pods, packed):
            self.assertEqual(Counter(n.sku for n in az), expected_mix)
            self.assertCountEqual([id(p) for n in az for p in n.pods],
                                  [id(p) for p in original])
            for node in az:
                if not node.pods:
                    self.assertEqual(node, exact.new_node(
                        self.cfg, next(s for s in self.skus if s.name == node.sku)))
        self.assertEqual(used, set(expected_mix))
        self.assertEqual(stats["skus"], expected_mix)
        self.assertEqual(stats["nodes"], 3)
        self.assertEqual(stats["az_count"], 3)
        self.assertEqual(stats["total_nodes"], 9)
        self.assertEqual(stats["total_cores"], 24)
        self.assert_reconciles(stats, [n for az in packed for n in az])

    def test_overflow_span_does_not_replicate_inventory(self):
        pods = [self.pod("replica"), self.pod("replica")]
        one, nodes_one, used_one = exact._pack_overflow(pods, self.skus, self.cfg, 1)
        three, nodes_three, used_three = exact._pack_overflow(pods, self.skus, self.cfg, 3)
        self.assertEqual(one["az_count"], 1)
        self.assertEqual(three["az_count"], 3)
        self.assertEqual({**one, "az_count": 3}, three)
        self.assertEqual(nodes_one, nodes_three)
        self.assertEqual(used_one, used_three)
        self.assertEqual(three["nodes"], 2)
        self.assertEqual(three["skus"], {self.small.name: 2})
        self.assert_reconciles(three, nodes_three)

    def test_disjoint_pools_reconcile_inventory_cost(self):
        pods = [[self.pod("zonal")], [], []]
        zstats, zonal, ostats, overflow = exact.size_pools_disjoint(
            pods, [self.pod("overflow", nic=0)], self.skus, self.cfg)
        znodes = [n for az in zonal for n in az]
        self.assert_reconciles(zstats, znodes)
        self.assert_reconciles(ostats, overflow)
        self.assertTrue(set(zstats["skus"]).isdisjoint(ostats["skus"]))
        prices = {self.small.name: 0.25, self.large.name: 0.75}
        inventory_cost = sum(prices[n.sku] for n in znodes + overflow)
        stats_cost = (sum(prices[s] * count * zstats["az_count"]
                          for s, count in zstats["skus"].items())
                      + sum(prices[s] * count for s, count in ostats["skus"].items()))
        self.assertEqual(inventory_cost, stats_cost)

    def test_empty_pools(self):
        zstats, zonal, ostats, overflow = exact.size_pools_disjoint(
            [[], [], []], [], [], self.cfg)
        self.assertEqual(zonal, [[], [], []])
        self.assertEqual(overflow, [])
        for stats in (zstats, ostats):
            self.assertEqual(stats["az_count"], 3)
            self.assertEqual(stats["nodes"], 0)
            self.assertEqual(stats["skus"], {})
            self.assert_reconciles(stats, [])

    def test_sku_ties_and_cached_candidates_are_deterministic(self):
        preferred = SKU("Standard_a", 2, 4, 3)
        skus = [SKU("Standard_c", 2, 8, 3), SKU("Standard_b", 2, 4, 3), preferred]
        pod = self.pod("test")
        node = exact.new_node(self.cfg, skus[0])
        node.place(pod)
        expected = sorted(skus, key=lambda s: (s.vcpu, s.memory_gib, s.name))
        baseline = None
        for allowed in permutations(skus):
            with self.subTest(order=[s.name for s in allowed]):
                self.assertEqual(exact._best_fit_sku(node, allowed, self.cfg), preferred)
                self.assertEqual([n.sku for n in exact.hetero_pack([pod], allowed, self.cfg)],
                                 [preferred.name])
                with patch.object(exact, "hetero_pack", wraps=exact.hetero_pack) as pack:
                    result = exact.size_pools_disjoint([[pod], [], []], [pod], allowed, self.cfg)
                for call in pack.call_args_list:
                    candidates = call.args[1]
                    self.assertEqual(candidates, [s for s in expected if s in candidates])
                if baseline is None:
                    baseline = result
                self.assertEqual(result, baseline)


if __name__ == "__main__":
    unittest.main()
