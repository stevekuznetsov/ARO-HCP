from fractions import Fraction as F
from pathlib import Path
import unittest
import cost_allocate_v13 as v13

class CRDBudgetTest(unittest.TestCase):
    def test_budget(self):
        report,profile=v13.derive(Path(__file__).resolve().parents[3])
        model=report['model']
        self.assertEqual(model['effectiveOfferingCPUMilli'],'2480')
        self.assertLessEqual(model['offeringChargedMemoryMiB'],10163)
        self.assertLessEqual(F(model['quotaAwareUSDPerHour']),F('.125'))
        self.assertEqual(model['offeringChargedMemoryMiB'],model['steadyOfferingMemoryLimitChargeMiB']+model['initStageExtraMemoryMiB'])
        for row,entry in zip(model['rows'],profile['containers']):
            self.assertEqual(row['requests'],entry['floor'])
        self.assertEqual(profile['anchor'],dict(cpuMilli=99,memoryMiB=560))

if __name__=='__main__':
    unittest.main()
