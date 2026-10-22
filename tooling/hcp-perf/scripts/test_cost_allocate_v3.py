import argparse
from fractions import Fraction as F
from pathlib import Path
import unittest

import cost_allocate_v2 as v2
import cost_allocate_v3 as v3


class ObservedQuantumTest(unittest.TestCase):
    def test_observed_samples(self):
        for declared, effective in ((3, 10), (6, 10), (23, 30), (183, 190), (40, 40)):
            self.assertEqual(v2.quota_milli(declared, quantum=1000), effective)
        self.assertEqual(v2.quota_milli(183), 183, "historical v2 default must retain min-only semantics")

    def test_parent_stage_and_custom_quantum(self):
        result = v2.pod_quota([23, 3], [(183, False)], quantum=1000)
        self.assertEqual(result["parentQuotaMilli"], "190")
        self.assertEqual(result["effectiveQuotaMilli"], "190")
        self.assertEqual(result["steadyQuotaMilli"], "40")
        self.assertEqual(v2.pod_quota([23, 3], [], quantum=1000)["effectiveQuotaMilli"], "30")
        self.assertEqual(v2.quota_milli(23, period=200000, quantum=1000), 25)
        with self.assertRaises(ValueError):
            v2.quota_milli(3, quantum=0)

    def test_saved_observed_parents_and_candidates(self):
        root = Path(__file__).resolve().parents[3]
        if not (root / "_artifacts/small-cost-v2/allocation.json").exists():
            self.skipTest("private evidence unavailable")
        result, profiles = v2.derive(root, 100000, 1000, 1000, "v3")
        v3.augment(root, result, profiles, argparse.Namespace(cpu_period_us=100000, min_quota_us=1000, quota_quantum_us=1000))
        self.assertEqual(result["observedQuotaValidation"]["podParents"], 17)
        self.assertEqual(result["observedQuotaValidation"]["pairedContainerRows"], 24)
        for name, model in result["models"].items():
            self.assertLessEqual(F(model["quotaAwareUSDPerHour"]), F(model["budgetUSDPerHour"]))
            if not name.endswith("uniform"):
                self.assertNotEqual(model["cpuScale"], model["memoryScale"])


if __name__ == "__main__":
    unittest.main()
