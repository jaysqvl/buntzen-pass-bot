from __future__ import annotations

import unittest
from types import SimpleNamespace

from src.pass_types import build_pass_order


class PassOrderTests(unittest.TestCase):
    def test_all_day_then_afternoon_then_morning(self) -> None:
        config = SimpleNamespace(check_all_day=True, check_afternoon=True, check_morning=True)
        self.assertEqual([item.key for item in build_pass_order(config)], ["all_day", "afternoon", "morning"])

    def test_half_day_only_keeps_afternoon_priority(self) -> None:
        config = SimpleNamespace(check_all_day=False, check_afternoon=True, check_morning=True)
        self.assertEqual([item.key for item in build_pass_order(config)], ["afternoon", "morning"])


if __name__ == "__main__":
    unittest.main()
