from __future__ import annotations

import copy
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from buntzen_actions.cart import (
    is_empty_cart, is_single_pass_cart, require_empty_cart, require_single_pass_cart,
)
from buntzen_actions.errors import ActionError


def single_pass():
    return {
        "count": "1", "lists": 1, "rows": 1, "recognized": True, "cards": 1,
        "classifications": 1, "quantities": [{"value": "1", "scoped": True}],
    }


class CartTests(unittest.TestCase):
    def test_empty_cart_requires_known_zero_counter_and_no_mounted_items(self):
        empty = {
            "count": "0", "lists": 0, "rows": 0, "recognized": True,
            "cards": 0, "classifications": 0, "quantities": [],
        }
        self.assertTrue(is_empty_cart(empty))
        empty["lists"] = 1
        self.assertTrue(is_empty_cart(empty), "an empty list may remain mounted")
        for field, value in (("count", "1"), ("cards", 1), ("rows", 1),
                             ("classifications", 1), ("quantities", [{"value": "1"}]),
                             ("lists", 2)):
            snapshot = copy.deepcopy(empty)
            snapshot[field] = value
            with self.subTest(field=field):
                self.assertFalse(is_empty_cart(snapshot))
        for snapshot in (None, {}, {"count": "0"}, single_pass()):
            self.assertFalse(is_empty_cart(snapshot))

    def test_one_pass_requires_consistent_counter_rows_and_quantity(self):
        self.assertTrue(is_single_pass_cart(single_pass()))
        for field, value in (
            ("count", "2"), ("count", "0"), ("count", "one"),
            ("rows", 2), ("rows", 0), ("recognized", False),
            ("cards", 2), ("classifications", 2), ("lists", 0),
            ("quantities", [{"value": "2", "scoped": True}]),
            ("quantities", [{"value": "1", "scoped": False}]),
            ("quantities", [{"value": "1", "scoped": True}] * 2),
            ("quantities", []), ("quantities", [{"value": "1e0", "scoped": True}]),
        ):
            snapshot = copy.deepcopy(single_pass())
            snapshot[field] = value
            with self.subTest(field=field, value=value):
                self.assertFalse(is_single_pass_cart(snapshot))
        for snapshot in (None, [], {}, "1", {"count": "1"}):
            self.assertFalse(is_single_pass_cart(snapshot))

    def test_waits_for_cart_rerender_without_changing_cart(self):
        page = Mock()
        page.evaluate.side_effect = [None, single_pass()]
        control = SimpleNamespace(inbox=SimpleNamespace(check_cancelled=Mock()))
        require_single_pass_cart(page, control)
        page.wait_for_timeout.assert_called_once_with(100)
        self.assertEqual(page.evaluate.call_count, 2)
        self.assertEqual(control.inbox.check_cancelled.call_count, 2)

    def test_extra_cart_items_fail_before_submission(self):
        snapshot = single_pass()
        snapshot["quantities"] = [{"value": "1", "scoped": True}] * 2
        page = Mock()
        page.evaluate.return_value = snapshot
        control = SimpleNamespace(inbox=SimpleNamespace(check_cancelled=Mock()))
        with patch("buntzen_actions.cart.time.monotonic", side_effect=[0, 0, 6]):
            with self.assertRaisesRegex(ActionError, "exactly one pass"):
                require_single_pass_cart(page, control)

    def test_unreadable_cart_fails_closed(self):
        page = Mock()
        page.evaluate.side_effect = RuntimeError("page detached")
        control = SimpleNamespace(inbox=SimpleNamespace(check_cancelled=Mock()))
        with self.assertRaisesRegex(ActionError, "review the cart"):
            require_single_pass_cart(page, control)

    def test_existing_cart_is_not_cleared_or_submitted(self):
        page = Mock()
        page.evaluate.return_value = single_pass()
        control = SimpleNamespace(inbox=SimpleNamespace(check_cancelled=Mock()))
        with patch("buntzen_actions.cart.time.monotonic", side_effect=[0, 0, 6]):
            with self.assertRaisesRegex(ActionError, "inspect and clear the cart"):
                require_empty_cart(page, control)


if __name__ == "__main__":
    unittest.main()
