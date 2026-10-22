import unittest
from decimal import Decimal

import scoped_offline_budgets as s


class ScopedBudgetsTest(unittest.TestCase):
    def test_rounding_and_original_floors(self):
        self.assertEqual(s.b.proposed(Decimal(".125"), .01, "1.2", ".01"), Decimal(".125"))
        self.assertEqual(s.b.proposed(Decimal(".25"), .4806, "1.2", ".01"), Decimal(".58"))
        self.assertEqual(s.b.proposed(64 * s.b.MIB, None, "1.1", str(16 * s.b.MIB)), 64 * s.b.MIB)
        self.assertIsNone(s.b.proposed(None, None, "1.1", str(16 * s.b.MIB)))

    def test_scope_is_not_namespace_or_annotation(self):
        self.assertFalse(set(s.DIRECT) & s.DOWNSTREAM)
        self.assertIn("cluster-network-operator", s.DIRECT)
        self.assertIn("ovnkube-control-plane", s.DOWNSTREAM)
        self.assertNotIn("kube-state-metrics-hcp", s.DIRECT)
        self.assertNotIn("olm-collect-profiles-29816042", s.DIRECT)
        self.assertIn("olm-collect-profiles", s.DIRECT)


if __name__ == "__main__":
    unittest.main()
