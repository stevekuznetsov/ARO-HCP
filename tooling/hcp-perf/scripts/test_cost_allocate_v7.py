from fractions import Fraction as F
from pathlib import Path
import unittest

import cost_allocate_v7 as v7


class FreshStartupAllocationTest(unittest.TestCase):
    def test_protections_and_stage_budget(self):
        root = Path(__file__).resolve().parents[3]
        if not (root / "_artifacts/small-cost-v6/lifecycle/result.json").exists():
            self.skipTest("private lifecycle evidence unavailable")
        result, profile = v7.derive(root)
        model = result["model"]
        rows = {(r["identity"]["workload"], r["identity"]["container"]): r for r in model["rows"]}
        for entry in result["protectedCPU"]:
            self.assertEqual(rows[(entry["workload"], entry["container"])]["requests"]["cpuMilli"], entry["cpuMilli"])
        for entry in result["protectedMemory"]:
            self.assertEqual(rows[(entry["workload"], entry["container"])]["requests"]["memoryMiB"], entry["memoryMiB"])
        init = [r for r in model["rows"] if r["identity"].get("initContainer") and r["identity"]["container"] == "availability-prober"]
        self.assertEqual(len(init), 17)
        for r in init:
            self.assertEqual(r["requests"], {"cpuMilli": 2, "memoryMiB": 64})
            self.assertEqual(r["memoryLimitMiB"], 71)
        self.assertGreater(model["initStageExtraMemoryMiB"], 0, "must charge init-dominated parents")
        self.assertGreater(result["fixedOnlyOfferingChargedMemoryMiB"], F(4096000, 403), "redistribution is necessary")
        self.assertEqual(sum(g["chargedMemoryMiB"] for g in model["podGroups"]) + 2090, model["offeringChargedMemoryMiB"])
        self.assertLessEqual(F(model["quotaAwareUSDPerHour"]), F(".125"))
        self.assertGreaterEqual(F(model["unusedEffectiveCPUMilli"]), 0)
        self.assertGreaterEqual(F(model["unusedChargedMemoryMiB"]), 0)
        keys = [(c["identity"]["workload"], c["identity"]["container"]) for c in profile["containers"]]
        self.assertEqual(len(keys), len(set(keys)))
        self.assertEqual(profile["name"], "small-cost-v7")


if __name__ == "__main__":
    unittest.main()
