import unittest
from fractions import Fraction as F
import json
from pathlib import Path

import cost_allocate as c


class CostAllocationTest(unittest.TestCase):
    def rows(self):
        return [{"identity": "main", "cpuMilli": 100, "memoryMiB": 100, "replicas": 3},
                {"identity": "init", "cpuMilli": 10, "memoryMiB": 64, "replicas": 0}]

    def test_uniform_removes_floors_and_scales_unobserved(self):
        r = c.allocate(self.rows(), 1000, 2000, F(1), F(1, 8), 10, 50)
        self.assertEqual(r["cpuScale"], r["memoryScale"])
        self.assertEqual(r["rows"][0]["allocatedCPUMilli"], 38)
        self.assertEqual(r["rows"][1]["allocatedCPUMilli"], 3)
        self.assertEqual(r["controlledCPUMilli"], 114)
        self.assertGreaterEqual(F(r["unusedCPUMilli"]), 0)
        self.assertTrue(r["withinBudgetWithLimits"])

    def test_ceil_limits_cannot_overspend(self):
        for budget in (F(1, 8), F(1, 4), F(1, 2)):
            r = c.allocate(self.rows(), 100000, 1000, F(1), budget, 10, 50)
            self.assertLessEqual(r["offeringChargedMemoryMiB"], F(r["rawEntitlementMemoryMiB"]))
            self.assertLessEqual(F(r["limitAwareEquivalentUSDPerHour"]), budget)
            self.assertEqual(r["rows"][0]["memoryLimitMiB"], (110 * r["rows"][0]["allocatedMemoryMiB"] + 99) // 100)

    def test_request_only_can_hide_memory_limit_overspend(self):
        r = c.allocate(self.rows(), 100000, 1000, F(1), F(1, 4), 10, 50, False, False)
        self.assertLessEqual(F(r["requestEquivalentUSDPerHour"]), F(1, 4))
        self.assertFalse(r["withinBudgetWithLimits"])

    def test_zero_and_fixed_exhaustion_rejected(self):
        for budget in (F(1, 10000), F(1, 100)):
            with self.assertRaises(ValueError):
                c.allocate(self.rows(), 1000, 1000, F(1), budget, 10, 50)
        with self.assertRaisesRegex(ValueError, "subminimum"):
            c.allocate(self.rows(), 1000, 2000, F(1), F(1, 8), 10, 50, minimum_cpu=10)

    def test_independent_changes_pairs(self):
        r = c.allocate(self.rows(), 1000, 2000, F(1), F(1, 8), 10, 50, True, False)
        self.assertNotEqual(r["cpuScale"], r["memoryScale"])

    def test_restartable_init_scheduling(self):
        pod = {"containers": [{"name": "main"}], "initContainers": [
            {"name": "sidecar", "restartPolicy": "Always"}, {"name": "init"}]}
        values = {"main": [10, 20, 22], "sidecar": [3, 5, 6], "init": [50, 100, 110]}
        self.assertEqual(c.scheduling_charge(pod, lambda row, group: values[row["name"]]), [53, 105, 116])

    def test_exact_price_filter_and_ambiguity(self):
        item = {"armSkuName": "Standard_D16s_v6", "armRegionName": "westus3", "type": "Consumption",
                "currencyCode": "USD", "serviceName": "Virtual Machines", "unitOfMeasure": "1 Hour",
                "isPrimaryMeterRegion": True, "tierMinimumUnits": 0, "productName": "Virtual Machines Dsv6 Series",
                "skuName": "D16s v6", "meterName": "D16s v6", "retailPrice": .806, "unitPrice": .806}
        for label in ("Windows", "Spot", "Low Priority"):
            self.assertEqual(c.select_price([item, {**item, "productName": label}], item["armSkuName"]), item)
        with self.assertRaises(ValueError):
            c.select_price([item, item], item["armSkuName"])
        with self.assertRaises(ValueError):
            c.select_price([{**item, "armRegionName": "westus2"}], item["armSkuName"])

    def test_saved_experiment_if_available(self):
        root = Path(__file__).resolve().parents[3]
        prices = root / "_artifacts/small-cost-v1/prices.json"
        if not prices.exists():
            self.skipTest("private price and measurement snapshot not available")
        result, profiles = c.derive(root, prices)
        self.assertEqual(len(result["models"]), 32)
        expected = {"small-cost-v1": (2440, 6731, 7275, 3, 21),
                    "double-small-cost-v1": (4907, 12483, 13557, 7, 47)}
        for name, (cpu, memory, charged, fallback_cpu, fallback_memory) in expected.items():
            chosen = result["selected"][name]
            self.assertEqual(chosen["sku"], "Standard_D16s_v6")
            self.assertEqual((chosen["offeringCPUMilli"], chosen["offeringMemoryMiB"], chosen["offeringChargedMemoryMiB"]), (cpu, memory, charged))
            self.assertEqual(chosen["fallback"], {"cpuMilli": fallback_cpu, "memoryMiB": fallback_memory})
            self.assertEqual(chosen["runningPodScheduling"]["cpuMilli"], cpu)
            self.assertEqual(chosen["runningPodScheduling"]["chargedMemoryMiB"], charged)
            self.assertEqual(len(chosen["rows"]), 92)
            self.assertEqual(sum(row["replicas"] for row in chosen["rows"]), 97)
            self.assertTrue(chosen["withinBudgetWithLimits"])
            self.assertEqual(json.loads((root / f"tooling/hcp-perf/examples/{name}.json").read_bytes()), profiles[name])


if __name__ == "__main__":
    unittest.main()
