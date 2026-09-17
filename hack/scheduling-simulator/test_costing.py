import unittest
from types import SimpleNamespace

from costing import cost_summary
from demand.model import ClusterDemand, Component


class Model:
    sizes = ["12", "250", "30"]

    def cluster_demand(self, size, policy, *args, **kwargs):
        return ClusterDemand(size, policy, [
            Component("test", "zonal_pair", True, 1, 1000, 1024, 1),
        ])


def node(size=None, reserve=None, sku="test"):
    pods = [] if size is None else [{"hcp_size": size, "reserve": reserve,
                                     "cpu_mc": 1000, "mem_mib": 1024, "nic": 1}]
    return {"sku": sku, "cap": {"cpu_mc": 4000, "mem_mib": 4096, "nic": 4, "pods": 10},
            "pods": pods}


class CostTests(unittest.TestCase):
    def setUp(self):
        self.cfg = SimpleNamespace(percentile=75, multiplier=1, unsteered_placement="overflow")
        self.result = {"policy": "minimal", "management_clusters": [{
            "count": 3, "hcp_mix": {"12": 1, "250": 1}, "zonal_pool": {}, "overflow_pool": {},
            "packing": {"zonal": [[node("12")], [node("250")], [node("30", "slot")]],
                        "overflow": [node()]},
        }]}
        self.prices = {"hourly": {"test": 1.0}}

    def test_full_bill_amortized_and_placeholders_excluded(self):
        c = cost_summary(self.result, self.cfg, Model(), self.prices)
        self.assertIsNone(c["error"])
        self.assertEqual(c["inventory"], {"test": 12})
        self.assertEqual(c["hourly"], 12)
        self.assertEqual(c["monthly"], 12 * 730)
        self.assertAlmostEqual(sum(r["count"] * (r["hourly"] or 0) for r in c["rows"]), 12)
        for r in c["rows"]:
            self.assertEqual(r["cpu"], 1)
            self.assertEqual(r["mem"], 1)
            if r["size"] == "30":
                self.assertEqual(r["count"], 0)
                self.assertIsNone(r["hourly"])
            else:
                self.assertEqual(r["count"], 3)
                self.assertEqual(r["hourly"], 2)

    def test_missing_price_is_not_free(self):
        c = cost_summary(self.result, self.cfg, Model(), {"hourly": {}})
        self.assertIn("test", c["error"])
        self.assertIsNone(c["hourly"])
        self.assertTrue(all(r["hourly"] is None for r in c["rows"]))

    def test_dominant_resource_weight_and_mixed_prices(self):
        mc = self.result["management_clusters"][0]
        mc["packing"]["zonal"][1][0]["pods"][0]["mem_mib"] = 3072
        mc["packing"]["zonal"][2][0]["sku"] = "larger"
        self.prices["hourly"]["larger"] = 2
        c = cost_summary(self.result, self.cfg, Model(), self.prices)
        rows = {r["size"]: r for r in c["rows"]}
        self.assertAlmostEqual(rows["250"]["zonal_hourly"], 3 * rows["12"]["zonal_hourly"])
        self.assertAlmostEqual(sum(r["count"] * (r["hourly"] or 0) for r in c["rows"]), 15)
        self.assertEqual(c["hourly"], 15)

    def test_infeasible_pool_is_not_free(self):
        self.result["management_clusters"][0]["zonal_pool"] = None
        c = cost_summary(self.result, self.cfg, Model(), self.prices)
        self.assertIn("could not be packed", c["error"])
        self.assertIsNone(c["hourly"])


if __name__ == "__main__":
    unittest.main()
