from fractions import Fraction as F
from pathlib import Path
import unittest
import cost_allocate_v11 as v11

class HelperBudgetTest(unittest.TestCase):
    def test_budget(self):
        root=Path(__file__).resolve().parents[3]
        report,profile=v11.derive(root)
        before,_=v11.v10.derive(root)
        self.assertEqual(report['model']['effectiveOfferingCPUMilli'],'2480')
        self.assertEqual(report['model']['offeringChargedMemoryMiB'],10154)
        self.assertEqual([r['requests']['memoryMiB'] for r in report['model']['rows']],[r['requests']['memoryMiB'] for r in before['model']['rows']])
        self.assertLessEqual(F(report['model']['quotaAwareUSDPerHour']),F('.125'))

if __name__=='__main__':
    unittest.main()
