"""Real Chromium checks against sanitized, captured Yodel vehicle widget DOM.

The DOM shape is from the provider. Event handlers below are deliberately a
small test model of its select/Save behavior, not a live provider checkout.
"""
from __future__ import annotations

import os
import time
import unittest
from pathlib import Path

from playwright.sync_api import sync_playwright

from buntzen_actions.errors import Cancelled
from buntzen_actions.vehicle_selection import select_vehicle


FIXTURE = Path(__file__).resolve().parents[1] / "actions/tests/fixtures/yodel_vehicle.html"
BEHAVIOR = """() => {
  window.vehicleFlags = {};
  window.vehicleClicks = {heading: 0, option: 0, save: 0};
  document.querySelectorAll('.cartLabel').forEach(heading =>
    heading.addEventListener('click', () => vehicleClicks.heading++));
  document.querySelectorAll('a[id^="vehicleSelectTrigger_"]').forEach(trigger => {
    const popup = document.getElementById(trigger.id.replace('vehicleSelectTrigger_', 'vehicleSmartSelect_'));
    const save = document.getElementById(popup.id + '-save-btn');
    trigger.addEventListener('click', event => {
      event.preventDefault();
      if (vehicleFlags.reparent) document.body.append(popup);
      popup.classList.add('modal-in');
    });
    popup.querySelectorAll('[role="radio"]').forEach(choice => {
      choice.addEventListener('click', event => {
        event.preventDefault();
        vehicleClicks.option++;
        if (vehicleFlags.noSelect) return;
        popup.querySelectorAll('[role="radio"]').forEach(other => {
          other.setAttribute('aria-checked', String(other === choice));
          other.querySelector('input').checked = other === choice && !vehicleFlags.unchecked;
        });
        if (vehicleFlags.disabledSave) return;
        save.setAttribute('aria-disabled', 'false');
        save.classList.remove('disabled', 'btn-disbled');
      });
    });
    save.addEventListener('click', event => {
      event.preventDefault();
      vehicleClicks.save++;
      if (!vehicleFlags.stayOpen) popup.classList.remove('modal-in');
      if (vehicleFlags.noSave) return;
      const choice = popup.querySelector('[role="radio"][aria-checked="true"]');
      const label = vehicleFlags.wrongSave ? 'Other Vehicle - TEST999' : choice.innerText.trim();
      trigger.textContent = label;
      trigger.classList.add('selectedProfileValue');
      trigger.setAttribute('aria-label', `Select VEHICLE INFO, ${label} selected`);
    });
  });
}"""


class VehicleBrowserTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.playwright = sync_playwright().start()
        launch = {"headless": True}
        if executable := os.environ.get("BUNTZEN_E2E_BROWSER_EXECUTABLE"):
            launch["executable_path"] = executable
        cls.browser = cls.playwright.chromium.launch(**launch)

    @classmethod
    def tearDownClass(cls):
        cls.browser.close()
        cls.playwright.stop()

    def setUp(self):
        self.page = self.browser.new_page()
        self.page.set_default_timeout(15_000)
        self.page.set_content(FIXTURE.read_text())
        self.page.evaluate(BEHAVIOR)
        self.card = self.page.locator(".card:has-text('Afternoon')")
        self.popup = self.card.locator("div[id^='vehicleSmartSelect_']")

    def tearDown(self):
        self.page.close()

    def select(self, **kwargs):
        return select_vehicle(
            self.page, self.card, "Tesla", timeout_ms=kwargs.pop("timeout_ms", 700),
            check_cancelled=kwargs.pop("check_cancelled", lambda: None), **kwargs,
        )

    def flags(self, **values):
        self.page.evaluate("values => Object.assign(vehicleFlags, values)", values)

    def test_selects_and_saves_correct_card_despite_three_hidden_tesla_matches(self):
        self.assertEqual(self.page.locator("label:has-text('Tesla')").count(), 3)
        result = self.select(timeout_ms=2_000)
        self.assertTrue(result.success, result.reason)
        self.assertEqual(result.reason, "selected")
        self.assertEqual(self.page.evaluate("vehicleClicks"), {"heading": 0, "option": 1, "save": 1})
        self.assertEqual(self.card.locator("a[id^='vehicleSelectTrigger_']").inner_text(), "Tesla Example - TEST123")
        self.assertEqual(self.page.locator(".card:has-text('Morning') a[id^='vehicleSelectTrigger_']").inner_text(), "Select...")
        self.assertFalse(self.popup.is_visible())

    def test_missing_vehicle_cannot_match_other_card_or_hidden_make_picker(self):
        self.popup.locator("[role='radio'] label").evaluate("el => {el.firstChild.textContent = 'Other Vehicle - TEST999';}")
        result = self.select()
        self.assertFalse(result.success)
        self.assertEqual(result.reason, "vehicle_missing")
        self.assertEqual(self.page.evaluate("vehicleClicks.option"), 0)

    def test_opened_popup_can_move_outside_card_without_losing_its_identity(self):
        self.flags(reparent=True)
        self.assertTrue(self.select(timeout_ms=2_000).success)
        self.assertEqual(self.card.locator("div[id^='vehicleSmartSelect_']").count(), 0)
        self.assertEqual(self.page.evaluate("vehicleClicks"), {"heading": 0, "option": 1, "save": 1})

    def test_multiple_visible_saved_matches_fail_without_choosing_arbitrarily(self):
        self.popup.locator("[role='radio']").evaluate("el => el.after(el.cloneNode(true))")
        result = self.select()
        self.assertEqual(result.reason, "vehicle_ambiguous")
        self.assertFalse(result.success)
        self.assertEqual(self.page.evaluate("vehicleClicks.option"), 0)

    def test_hidden_matching_choice_is_excluded(self):
        self.popup.locator("[role='radio']").evaluate("el => {const copy = el.cloneNode(true); copy.style.display='none'; el.after(copy);}")
        self.assertTrue(self.select(timeout_ms=2_000).success)

    def test_option_click_does_not_count_as_selection(self):
        self.flags(noSelect=True)
        result = self.select()
        self.assertEqual(result.reason, "selection_unconfirmed")
        self.assertFalse(result.success)
        self.assertEqual(self.page.evaluate("vehicleClicks.save"), 0)

    def test_aria_only_selection_does_not_count_as_checked_radio(self):
        self.flags(unchecked=True)
        self.assertEqual(self.select().reason, "selection_unconfirmed")
        self.assertEqual(self.page.evaluate("vehicleClicks.save"), 0)

    def test_disabled_save_is_not_clicked(self):
        self.flags(disabledSave=True)
        self.assertEqual(self.select().reason, "save_unavailable")
        self.assertEqual(self.page.evaluate("vehicleClicks.save"), 0)

    def test_closing_popup_without_saved_card_readback_is_failure(self):
        self.flags(noSave=True)
        result = self.select()
        self.assertEqual(result.reason, "save_unconfirmed")
        self.assertFalse(result.success)

    def test_different_saved_vehicle_is_failure(self):
        self.flags(wrongSave=True)
        self.assertEqual(self.select().reason, "save_unconfirmed")

    def test_popup_must_close_after_save(self):
        self.flags(stayOpen=True)
        self.assertEqual(self.select().reason, "save_unconfirmed")

    def test_multiple_visible_triggers_are_ambiguous(self):
        self.card.locator("a[id^='vehicleSelectTrigger_']").evaluate("el => el.after(el.cloneNode(true))")
        self.assertEqual(self.select().reason, "selector_ambiguous")
        self.assertEqual(self.page.evaluate("vehicleClicks.option"), 0)

    def test_heading_without_actual_opener_is_not_clicked(self):
        self.card.locator("a[id^='vehicleSelectTrigger_']").evaluate("el => el.remove()")
        self.assertEqual(self.select().reason, "selector_missing")
        self.assertEqual(self.page.evaluate("vehicleClicks.heading"), 0)

    def test_intercepted_click_does_not_inherit_fifteen_second_timeout(self):
        self.page.evaluate("document.body.insertAdjacentHTML('beforeend', '<div style=\"position:fixed;inset:0;z-index:99999\"></div>')")
        started = time.monotonic()
        result = self.select(timeout_ms=600)
        self.assertFalse(result.success)
        self.assertEqual(result.reason, "popup_unavailable")
        self.assertLess(time.monotonic() - started, 2.0)

    def test_cancellation_propagates_during_unconfirmed_selection(self):
        self.flags(noSelect=True)
        calls = 0

        def check_cancelled():
            nonlocal calls
            calls += 1
            if calls >= 18:
                raise Cancelled("test cancellation")

        with self.assertRaises(Cancelled):
            self.select(check_cancelled=check_cancelled)
        self.assertEqual(self.page.evaluate("vehicleClicks.save"), 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
