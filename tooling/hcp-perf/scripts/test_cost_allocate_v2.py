import unittest
from fractions import Fraction as F
from pathlib import Path

import cost_allocate_v2 as q


class QuotaTest(unittest.TestCase):
    def test_subminimum_and_custom_period(self):
        self.assertEqual(q.quota_milli(3), 10)
        self.assertEqual(q.quota_milli(3, 200000), 5)
        self.assertEqual(q.quota_milli(3, 100000, 2000), 20)
        self.assertEqual(q.quota_milli(11, 100001), F(1100000, 100001))
        with self.assertRaises(ValueError):
            q.quota_milli(1, 0)

    def test_parent_caps_not_blind_sum(self):
        result = q.pod_quota([3, 3, 3], [])
        self.assertEqual(result["uncappedStageMaximumMilli"], "30")
        self.assertEqual(result["effectiveQuotaMilli"], "10")
        self.assertEqual(q.pod_quota([20, 3], [])["effectiveQuotaMilli"], "23")
        # Separate parents each retain their own minimum, unlike one shared Pod.
        self.assertEqual(sum(F(q.pod_quota([3], [])["effectiveQuotaMilli"]) for _ in range(3)), 30)

    def test_init_and_restartable_stages(self):
        result = q.pod_quota([3, 3], [(3, True), (20, False)])
        self.assertEqual(result["declaredParentMilli"], 23)
        self.assertEqual(result["effectiveQuotaMilli"], "23")
        self.assertEqual(result["steadyQuotaMilli"], "23")
        result = q.pod_quota([3], [(100, False)])
        self.assertEqual(result["steadyQuotaMilli"], "10")
        self.assertEqual(result["effectiveQuotaMilli"], "100")

    def test_exact_search_does_not_cross_step(self):
        factor = q.search_factor(F(1), lambda x: int(x * 100) < 33)
        self.assertEqual(factor, F(329999999, 10**9))
        self.assertLess(int(factor * 100), 33)

    def test_saved_regression(self):
        root = Path(__file__).resolve().parents[3]
        if not (root / "_artifacts/small-cost-v1/allocation.json").exists():
            self.skipTest("private evidence unavailable")
        report, profiles = q.derive(root, 100000, 1000)
        self.assertEqual(report["v1Regression"]["small-cost-v1"]["effectiveOfferingCPUMilli"], "2611")
        self.assertEqual(report["v1Regression"]["double-small-cost-v1"]["effectiveOfferingCPUMilli"], "4970")
        self.assertEqual(F(report["v1Regression"]["small-cost-v1"]["quotaAwareUSDPerHour"]), F(".131529125"))
        self.assertEqual(F(report["v1Regression"]["double-small-cost-v1"]["quotaAwareUSDPerHour"]), F(".25036375"))
        for model in report["models"].values():
            self.assertLessEqual(F(model["quotaAwareUSDPerHour"]), F(model["budgetUSDPerHour"]))
            self.assertGreaterEqual(F(model["unusedEffectiveCPUMilli"]), 0)
            self.assertGreaterEqual(F(model["unusedChargedMemoryMiB"]), 0)
        self.assertEqual(len(profiles), 4)


if __name__ == "__main__":
    unittest.main()
