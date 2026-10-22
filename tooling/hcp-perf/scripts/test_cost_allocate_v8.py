from copy import deepcopy
from fractions import Fraction as F
import json
from pathlib import Path
import unittest

import cost_allocate_v8 as v8


class StorageMemoryTest(unittest.TestCase):
    def test_storage_uses_existing_init_reservation(self):
        root = Path(__file__).resolve().parents[3]
        if not (root / "_artifacts/small-cost-v7/lifecycle/fresh/first-oom.json").exists():
            self.skipTest("private evidence unavailable")
        old = json.loads((root / "_artifacts/small-cost-v7/allocation.json").read_bytes())
        result, profile = v8.derive(root)
        model = result["model"]
        expected = deepcopy(old["model"]["rows"])
        for row in expected:
            if (row["identity"]["workload"], row["identity"]["container"]) == v8.STORAGE:
                row["requests"]["memoryMiB"], row["memoryLimitMiB"] = 64, 71
        self.assertEqual(model["rows"], expected, "only storage main memory changes across all92 rows")
        self.assertFalse(result["rebalancedOthers"])
        self.assertEqual(model["effectiveOfferingCPUMilli"], "2480")
        self.assertEqual(model["offeringChargedMemoryMiB"], 10163)
        self.assertEqual(model["steadyOfferingMemoryLimitChargeMiB"], 9942)
        self.assertEqual(model["initStageExtraMemoryMiB"], 221)
        self.assertEqual(model["fallback"], {"cpuMilli": 2, "memoryMiB": 31})
        self.assertLessEqual(F(model["quotaAwareUSDPerHour"]), F(".125"))
        self.assertEqual(model["podGroups"], old["model"]["podGroups"])
        self.assertEqual(len(result["quietMemoryAudit"]), 52)
        self.assertGreaterEqual(len(result["mainMemoryRisks"]), 5)
        self.assertEqual(profile["name"], "small-cost-v8")


if __name__ == "__main__":
    unittest.main()
