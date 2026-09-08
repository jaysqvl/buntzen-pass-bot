from __future__ import annotations

import re
import time
from dataclasses import dataclass
from typing import Any, Callable


@dataclass(frozen=True)
class VehicleSelectionResult:
    success: bool
    # These fixed reason codes deliberately exclude account vehicle names/plates.
    reason: str


class _Expired(Exception):
    pass


class _Budget:
    def __init__(self, timeout_ms: int, check_cancelled: Callable[[], None]):
        self.deadline = time.monotonic() + max(0, timeout_ms) / 1_000
        self.check_cancelled = check_cancelled

    def timeout(self) -> float:
        self.check_cancelled()
        remaining = (self.deadline - time.monotonic()) * 1_000
        if remaining <= 0:
            raise _Expired
        # No action can inherit the profile's much longer default timeout.
        return min(500, remaining)

    def wait(self, page: Any, predicate: Callable[[], bool]) -> None:
        while True:
            self.timeout()
            if predicate():
                return
            page.wait_for_timeout(min(50, self.timeout()))


def _normalized(value: str) -> str:
    return " ".join(value.split())


def _visible(locator: Any, budget: _Budget) -> list[Any]:
    budget.timeout()
    count = locator.count()
    if count > 100:
        return []
    result = []
    for index in range(count):
        budget.timeout()
        item = locator.nth(index)
        if item.is_visible():
            result.append(item)
    return result


def select_vehicle(
    page: Any,
    container: Any,
    keyword: str,
    *,
    timeout_ms: int = 5_000,
    check_cancelled: Callable[[], None],
) -> VehicleSelectionResult:
    """Select and save one matching vehicle in this pass card's actual widget.

    The September 2026 Yodel widget uses a vehicleSelectTrigger link and its
    vehicleSmartSelect popup, role=radio choices, and an explicit Save button.
    Headings, other cards and the hidden Add Vehicle make picker are excluded.
    Success requires the saved vehicle to appear back on the selected card.
    """
    from playwright.sync_api import Error as PlaywrightError

    budget = _Budget(timeout_ms, check_cancelled)
    reason = "selector_missing"
    keyword = _normalized(keyword).casefold()
    if not keyword:
        return VehicleSelectionResult(False, "vehicle_missing")

    try:
        triggers = _visible(container.locator("a[id^='vehicleSelectTrigger_']"), budget)
        if not triggers:
            return VehicleSelectionResult(False, reason)
        if len(triggers) != 1:
            return VehicleSelectionResult(False, "selector_ambiguous")
        trigger = triggers[0]
        trigger_id = trigger.get_attribute("id", timeout=budget.timeout()) or ""
        # Observed IDs contain only these characters; do not interpolate an
        # arbitrary page-controlled selector or fall back to unrelated dialogs.
        if not re.fullmatch(r"vehicleSelectTrigger_[A-Za-z0-9_-]+", trigger_id):
            return VehicleSelectionResult(False, reason)
        popup_id = trigger_id.replace("vehicleSelectTrigger_", "vehicleSmartSelect_", 1)
        # A modal may be reparented into the app root while opening. Its exact
        # ID is still derived from this card, so it cannot select another card's
        # saved vehicles or the unrelated Add Vehicle make picker.
        popup = page.locator(f"div[id='{popup_id}']")
        if popup.count() != 1:
            return VehicleSelectionResult(False, "popup_unavailable")

        reason = "popup_unavailable"
        trigger.click(timeout=budget.timeout())
        budget.wait(page, popup.is_visible)

        reason = "vehicle_missing"
        choices = _visible(
            popup.locator(
                "[role='radiogroup'][aria-label='Select Vehicle for this Pass'] "
                "[role='radio']"
            ),
            budget,
        )
        matches = []
        for choice in choices:
            label = _normalized(choice.inner_text(timeout=budget.timeout()))
            if keyword in label.casefold():
                matches.append((choice, label))
        if not matches:
            return VehicleSelectionResult(False, reason)
        if len(matches) != 1:
            return VehicleSelectionResult(False, "vehicle_ambiguous")
        choice, label = matches[0]
        reason = "selection_unconfirmed"
        # The option is an accessible list item, but its nested label owns the
        # native radio activation. Clicking whitespace on the row is weaker.
        option_label = choice.locator("label")
        if option_label.count() != 1 or not option_label.is_visible():
            return VehicleSelectionResult(False, reason)
        option_label.click(timeout=budget.timeout())

        def choice_selected() -> bool:
            radios = choice.locator("input[type='radio']")
            return (
                choice.get_attribute("aria-checked", timeout=budget.timeout()) == "true"
                and radios.count() == 1
                and radios.is_checked(timeout=budget.timeout())
            )

        budget.wait(page, choice_selected)
        reason = "save_unavailable"
        save = popup.locator(f"a[id='{popup_id}-save-btn']")

        def save_enabled() -> bool:
            budget.timeout()
            return (
                save.count() == 1
                and save.is_visible()
                and save.is_enabled()
                and save.get_attribute("aria-disabled", timeout=budget.timeout()) != "true"
                and "disabled" not in (
                    save.get_attribute("class", timeout=budget.timeout()) or ""
                ).split()
            )

        budget.wait(page, save_enabled)
        save.click(timeout=budget.timeout())

        reason = "save_unconfirmed"

        def saved_on_card() -> bool:
            budget.timeout()
            if popup.is_visible() or not trigger.is_visible():
                return False
            classes = (trigger.get_attribute("class", timeout=budget.timeout()) or "").split()
            text = _normalized(trigger.inner_text(timeout=budget.timeout()))
            aria = _normalized(trigger.get_attribute("aria-label", timeout=budget.timeout()) or "")
            return (
                "selectedProfileValue" in classes
                and text == label
                and aria.endswith(f", {label} selected")
            )

        budget.wait(page, saved_on_card)
        check_cancelled()
        return VehicleSelectionResult(True, "selected")
    except (_Expired, PlaywrightError):
        # Cancellation and programming errors deliberately propagate. Browser
        # errors are mapped to fixed stages without leaking page text or IDs.
        check_cancelled()
        return VehicleSelectionResult(False, reason)
