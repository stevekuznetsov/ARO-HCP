from copy import deepcopy
from fractions import Fraction as F
import json
from pathlib import Path
import unittest

import cost_allocate_v6 as v6


class TargetedSlackTest(unittest.TestCase):
    def test_exact_two_changes_and_margins(self):
        root = Path(__file__).resolve().parents[3]
        path = root / "_artifacts/small-cost-v5/allocation.json"
        if not path.exists():
            self.skipTest("private evidence unavailable")
        old = json.loads(path.read_bytes())
        result, profile = v6.derive(root)
        model = result["model"]
        expected = deepcopy(old["model"]["rows"])
        changed = 0
        for row in expected:
            key = row["identity"]["workload"], row["identity"]["container"]
            if key == v6.CVO:
                row["requests"]["memoryMiB"], row["memoryLimitMiB"] = 64, 71
                changed += 1
            if key == v6.KCM:
                self.assertEqual(row["replicas"], 2)
                row["requests"]["cpuMilli"] = 20
                changed += 1
        self.assertEqual(changed, 2)
        self.assertEqual(model["rows"], expected, "all 90 other rows exactly unchanged")
        self.assertEqual(model["fallback"], {"cpuMilli": 2, "memoryMiB": 32})
        self.assertEqual(model["effectiveOfferingCPUMilli"], "2480")
        self.assertEqual(model["declaredOfferingCPUMilli"], 2209)
        self.assertEqual(model["offeringChargedMemoryMiB"], 10137)
        self.assertEqual(model["offeringRequestMemoryMiB"], 9343)
        self.assertEqual(F(model["quotaAwareUSDPerHour"]), F(".12493"))
        self.assertEqual(F(model["unusedEffectiveCPUMilli"]), F(560, 403))
        self.assertEqual(F(model["unusedChargedMemoryMiB"]), F(10789, 403))
        for group in model["podGroups"]:
            if group["workload"] == v6.KCM[0]:
                self.assertEqual(group["effectiveQuotaMilli"], "20")
            if group["workload"] == v6.CVO[0]:
                self.assertEqual(group["chargedMemoryMiB"], 71)
        parent = json.loads((root / old["profile"]["path"]).read_bytes())
        expected_profile = deepcopy(parent)
        expected_profile["name"], expected_profile["description"] = profile["name"], profile["description"]
        for entry in expected_profile["containers"]:
            key = entry["identity"]["workload"], entry["identity"]["container"]
            if key == v6.CVO:
                entry["floor"]["memoryMiB"] = 64
            if key == v6.KCM:
                entry["floor"]["cpuMilli"] = 20
        self.assertEqual(profile, expected_profile)


if __name__ == "__main__":
    unittest.main()
