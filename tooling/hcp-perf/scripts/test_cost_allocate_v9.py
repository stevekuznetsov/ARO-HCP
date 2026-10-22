from fractions import Fraction as F
from pathlib import Path
import json
import unittest
import cost_allocate_v9 as v9

class BudgetTest(unittest.TestCase):
    def test_exact_budget_and_preserved_cpu(self):
        root=Path(__file__).resolve().parents[3]
        result,profile=v9.derive(root)
        old=json.loads((root/'_artifacts/small-cost-v8/allocation.json').read_text())['model']
        model=result['model']
        self.assertEqual([r['requests']['cpuMilli'] for r in model['rows']],[r['requests']['cpuMilli'] for r in old['rows']])
        self.assertLessEqual(F(model['effectiveOfferingCPUMilli']),F(1000000,403))
        self.assertLessEqual(model['offeringChargedMemoryMiB'],F(4096000,403))
        self.assertLessEqual(F(model['quotaAwareUSDPerHour']),F('.125'))
        self.assertEqual(model['offeringChargedMemoryMiB'],2090+sum(g['chargedMemoryMiB'] for g in model['podGroups']))
        for row in model['rows']:
            self.assertEqual(row['memoryLimitMiB'],(row['requests']['memoryMiB']*110+99)//100)
            key=(row['identity']['workload'],row['identity']['container'])
            if key in v9.FIXED:
                self.assertEqual(row['requests']['memoryMiB'],v9.FIXED[key])
        self.assertEqual(len(profile['containers']),len(model['rows']))

if __name__=='__main__':
    unittest.main()
