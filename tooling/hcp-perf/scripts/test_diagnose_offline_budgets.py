"""Diagnostic comparisons must never silently change the selected candidate."""

import copy
from decimal import Decimal
import unittest

import diagnose_offline_budgets as d


class DiagnosticTest(unittest.TestCase):
    def test_strict_headroom_and_sustained_are_separate(self):
        budget = {"baseline": {"resources": {"requests": {"cpu": "10m", "memory": "64Mi"}}},
                  "coveredQuietEvidence": {"cpuRateCores": {"max": .15},
                       "cpuDistinctIntervalCores": {"max": .201}, "chargeBytes": {"max": 65 * d.b.MIB}},
                  "candidate": {"requests": {"cpu": "250m", "memory": "80Mi"}}}
        instances = [{"windows": [{"metrics": {"cpuRateCores": {"median": .001, "p95": .015, "max": .15}}}]},
                     {"windows": [{"metrics": {"cpuRateCores": {"median": .021, "p95": .045, "max": .11}}}]}]
        before = copy.deepcopy(budget)
        values = d.alternatives(budget, instances)
        self.assertEqual(values["strictObservedCPU1mQuantum"], Decimal(".201"))
        self.assertEqual(values["strictObservedCPU10mQuantum"], Decimal(".21"))
        self.assertEqual(values["oneMinuteMedianCPU10mQuantum"], Decimal(".03"))
        self.assertEqual(values["oneMinuteP95CPU10mQuantum"], Decimal(".05"))
        self.assertEqual(values["strictObservedMemory1MiQuantum"], 65 * d.b.MIB)
        self.assertEqual(values["strictObservedMemory16MiQuantum"], 80 * d.b.MIB)
        self.assertEqual(before, budget)

    def test_unknown_and_existing_floors(self):
        budget = {"baseline": {"resources": {"requests": {}}}, "coveredQuietEvidence": {
            "cpuRateCores": {"max": None}, "cpuDistinctIntervalCores": {"max": None},
            "chargeBytes": {"max": None}}}
        self.assertTrue(all(v is None for v in d.alternatives(budget, []).values()))
        budget["baseline"]["resources"]["requests"] = {"cpu": "550m", "memory": "128Mi"}
        result = d.alternatives(budget, [])
        self.assertEqual(result["strictObservedCPU1mQuantum"], Decimal(".55"))
        self.assertEqual(result["oneMinuteMedianCPU10mQuantum"], Decimal(".55"))
        self.assertEqual(result["strictObservedMemory1MiQuantum"], 128 * d.b.MIB)


if __name__ == "__main__":
    unittest.main()
