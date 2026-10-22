from fractions import Fraction as F
from pathlib import Path
import unittest
import cost_allocate_v10 as v10

class ConsolidatedBudgetTest(unittest.TestCase):
    def test_fixed_budget(self):
        result,profile=v10.derive(Path(__file__).resolve().parents[3])
        model=result['model']
        self.assertLessEqual(F(model['effectiveOfferingCPUMilli']),F(1000000,403))
        self.assertLessEqual(model['offeringChargedMemoryMiB'],F(4096000,403))
        self.assertLessEqual(F(model['quotaAwareUSDPerHour']),F('.125'))
        self.assertEqual(model['offeringChargedMemoryMiB'],2090+sum(g['chargedMemoryMiB'] for g in model['podGroups']))
        self.assertEqual(F(model['effectiveOfferingCPUMilli']),470+sum(F(g['effectiveQuotaMilli']) for g in model['podGroups']))
        rows={(r['identity']['workload'],r['identity']['container']):r for r in model['rows']}
        for key in [('catalog-operator','catalog-operator'),('olm-operator','olm-operator')]:
            self.assertEqual(rows[key]['requests']['cpuMilli'],20)
        for key,memory in v10.v9.FIXED.items():
            self.assertEqual(rows[key]['requests']['memoryMiB'],memory)

if __name__=='__main__':
    unittest.main()
