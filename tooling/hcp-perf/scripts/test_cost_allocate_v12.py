from fractions import Fraction as F
from pathlib import Path
import unittest
import cost_allocate_v12 as v12

class BootstrapBudgetTest(unittest.TestCase):
    def test_budget(self):
        report,profile=v12.derive(Path(__file__).resolve().parents[3])
        model=report['model']
        self.assertEqual(model['effectiveOfferingCPUMilli'],'2480')
        self.assertEqual(model['offeringChargedMemoryMiB'],10151)
        self.assertEqual(model['offeringRequestMemoryMiB'],9269)
        self.assertLessEqual(F(model['quotaAwareUSDPerHour']),F('.125'))
        rows={(r['identity']['workload'],r['identity']['container']):r for r in model['rows']}
        for key,(cpu,memory) in v12.CHANGES.items():
            self.assertEqual(rows[key]['requests'],dict(cpuMilli=cpu,memoryMiB=memory))
        for row,entry in zip(model['rows'],profile['containers']):
            self.assertEqual(row['requests'],entry['floor'])

if __name__=='__main__':
    unittest.main()
