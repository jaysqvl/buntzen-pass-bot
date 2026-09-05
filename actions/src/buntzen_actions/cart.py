"""Fail closed unless Yodel's rendered cart contains one pass of quantity one."""

from __future__ import annotations

import time
from typing import Any

from .errors import ActionError


# These structures are shared by the public portal's single-item and grouped
# classification cart renderers. Read the mounted cart even when its panel is
# collapsed; return counts only, never pass, account, or vehicle text.
_CART_SNAPSHOT = """() => {
    const carts = document.querySelectorAll('.shoppingCard');
    if (carts.length !== 1) return null;
    const cart = carts[0];
    const counters = cart.querySelectorAll('.cartDigit .counter span.count');
    const lists = cart.querySelectorAll('.shoppingMainList > ul');
    if (counters.length !== 1 || lists.length > 1) return null;
    const rows = lists.length ? Array.from(lists[0].children) : [];
    const quantities = Array.from(cart.querySelectorAll('input.count'));
    return {
        count: counters[0].textContent.trim(),
        lists: lists.length,
        rows: rows.length,
        recognized: rows.every(row =>
            row.matches('li.shoppingList') &&
            (row.classList.contains('singleItemList') !==
             row.classList.contains('multiItemList'))),
        cards: cart.querySelectorAll('.CardListing').length,
        classifications: cart.querySelectorAll('.ClassificationInnerRow').length,
        quantities: quantities.map(input => ({
            value: input.value.trim(),
            scoped: input.closest('.ClassificationInnerRow') !== null
        }))
    };
}"""


def is_single_pass_cart(snapshot: Any) -> bool:
    if not isinstance(snapshot, dict):
        return False
    quantities = snapshot.get("quantities")
    return bool(
        snapshot.get("count") == "1"
        and snapshot.get("lists") == 1
        and snapshot.get("rows") == 1
        and snapshot.get("recognized") is True
        and snapshot.get("cards") == 1
        and snapshot.get("classifications") == 1
        and isinstance(quantities, list)
        and len(quantities) == 1
        and isinstance(quantities[0], dict)
        and quantities[0].get("value") == "1"
        and quantities[0].get("scoped") is True
    )


def is_empty_cart(snapshot: Any) -> bool:
    return bool(
        isinstance(snapshot, dict)
        and snapshot.get("count") == "0"
        and snapshot.get("lists") in (0, 1)
        and snapshot.get("rows") == 0
        and snapshot.get("cards") == 0
        and snapshot.get("classifications") == 0
        and snapshot.get("quantities") == []
    )


def _require_cart(page: Any, control: Any, predicate: Any, message: str) -> None:

    deadline = time.monotonic() + 5.0
    while time.monotonic() < deadline:
        control.inbox.check_cancelled()
        try:
            if predicate(page.evaluate(_CART_SNAPSHOT)):
                return
        except Exception:
            # A detached or changing page is not evidence of a safe cart.
            break
        page.wait_for_timeout(100)
    raise ActionError(message)


def require_empty_cart(page: Any, control: Any) -> None:
    """An existing cart must be reviewed by the user, never silently submitted."""

    _require_cart(
        page, control, is_empty_cart,
        "Yodel's cart could not be verified as empty; inspect and clear the cart "
        "before starting another booking",
    )


def require_single_pass_cart(page: Any, control: Any) -> None:
    """Check after adding the pass and again immediately before final approval."""

    _require_cart(
        page, control, is_single_pass_cart,
        "Yodel's cart could not be verified as exactly one pass of quantity one; "
        "review the cart before booking",
    )
