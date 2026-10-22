from fractions import Fraction as F
from pathlib import Path
import json
import unittest

import cost_allocate_v5 as v5


class FixedCPUTest(unittest.TestCase):
    def test_fixed_cpu_and_parent_caps(self):
        rows = [{"identity": {"workload": "oas", "container": "helper"}, "cpuMilli": 20, "replicas": 3},
                {"identity": {"workload": "oas", "container": "main"}, "cpuMilli": 90, "replicas": 3}]
        charge = lambda values: 3 * F(v5.shared.q.pod_quota([max(1, v) for v in values], [], quantum=1000)["effectiveQuotaMilli"])
        factor, values = v5.shared.fixed_memory(rows, {("oas", "helper"): 50}, F(240), charge, field="cpuMilli")
        self.assertEqual(values[0], 50)
        self.assertLessEqual(charge(values), 240)
        self.assertEqual(values[1], int(90 * factor))
        with self.assertRaises(ValueError):
            v5.shared.fixed_memory(rows, {("oas", "helper"): 50}, F(149), charge, field="cpuMilli")
        with self.assertRaises(ValueError):
            v5.shared.fixed_memory(rows, {("other", "helper"): 50}, F(240), charge, field="cpuMilli")

    def test_saved_memory_and_six_helpers(self):
        root = Path(__file__).resolve().parents[3]
        path = root / "_artifacts/small-cost-v4/allocation.json"
        if not path.exists():
            self.skipTest("private evidence unavailable")
        old = json.loads(path.read_bytes())
        result, profile = v5.derive(root)
        model = result["model"]
        helpers = 0
        for before, after in zip(old["model"]["rows"], model["rows"]):
            self.assertEqual(before["requests"]["memoryMiB"], after["requests"]["memoryMiB"])
            self.assertEqual(before["memoryLimitMiB"], after["memoryLimitMiB"])
            self.assertEqual(before["nonGoException"], after["nonGoException"])
            key = after["identity"]["workload"], after["identity"]["container"]
            if key in v5.HELPERS:
                self.assertEqual(after["requests"]["cpuMilli"], 50)
                helpers += after["replicas"]
            else:
                self.assertLessEqual(after["requests"]["cpuMilli"], before["requests"]["cpuMilli"])
        self.assertEqual(helpers, 6)
        self.assertEqual(model["offeringChargedMemoryMiB"], 10112)
        self.assertEqual(model["fallback"]["memoryMiB"], 32)
        self.assertLessEqual(F(model["quotaAwareUSDPerHour"]), F(".125"))
        self.assertEqual(profile["name"], "small-cost-v5")


if __name__ == "__main__":
    unittest.main()
