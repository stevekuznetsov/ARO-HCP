from fractions import Fraction as F
from pathlib import Path
import unittest

import cost_allocate_v4 as v4


class FixedMemoryTest(unittest.TestCase):
    def test_fixed_replicas_and_remaining_weights(self):
        rows = [{"identity": {"workload": "proxy", "container": "haproxy"}, "replicas": 2, "memoryMiB": 96},
                {"identity": {"workload": "other", "container": "main"}, "replicas": 3, "memoryMiB": 128}]
        charge = lambda values: 20 + sum(((110 * v + 99) // 100) * r["replicas"] for v, r in zip(values, rows))
        scale, values = v4.fixed_memory(rows, {("proxy", "haproxy"): 96}, F(400), charge)
        self.assertEqual(values[0], 96)
        self.assertEqual(values[1], int(128 * scale))
        self.assertLessEqual(charge(values), 400)
        self.assertEqual(charge([96, 0]), 232, "both fixed replica limits must be charged")
        with self.assertRaisesRegex(ValueError, "nonlisted"):
            v4.fixed_memory(rows, {("missing", "haproxy"): 96}, F(400), charge)
        with self.assertRaisesRegex(ValueError, "exceed"):
            v4.fixed_memory(rows, {("proxy", "haproxy"): 96}, F(220), charge)

    def test_saved_candidate(self):
        root = Path(__file__).resolve().parents[3]
        if not (root / "_artifacts/small-cost-v3/allocation.json").exists():
            self.skipTest("private evidence unavailable")
        result, profile = v4.derive(root)
        model = result["model"]
        self.assertEqual(model["effectiveOfferingCPUMilli"], "2480")
        self.assertLessEqual(F(model["quotaAwareUSDPerHour"]), F(".125"))
        for delta in result["memoryDeltas"]:
            if delta["identity"]["workload"] == "ignition-server-proxy":
                self.assertEqual((delta["replicas"], delta["newRequestMiB"], delta["newLimitMiB"]), (2, 96, 106))
            else:
                self.assertGreaterEqual(delta["requestDeltaMiB"], -10, "avoid large redistribution step")
                self.assertLessEqual(delta["requestDeltaMiB"], 0)
        self.assertEqual(profile["name"], "small-cost-v4")


if __name__ == "__main__":
    unittest.main()
